package perf

// Parameter samples through the pipeline itself: the steps before the parameter is used (lookups, nests, logical
// tables, groups … included), with the other request parameters' conditions removed, give the rows the parameter is
// compared with. The sample = the value at quantile q of those rows (range filters: 0.4 / 0.6, others: the median),
// so it exists in the data the filter sees — a lookup's column, a logical table's JSON key, a group's output.

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"apifactory/factory/core"
	"apifactory/factory/ir"
)

// sampleParam: values of cols (one row, at quantile q) for the parameters params: the rows are the pipeline before the
// first of them. where: only rows whose column (text) = the value (the other parameters already chosen), so the
// parameters of one request hit the same rows. Returns the values and their text.
func sampleParam(ctx context.Context, conn *pgx.Conn, api *ir.API, params []string, cols []string, ks []core.Kind, q float64, where map[string]string) ([]any, []string, error) {
	wcols := make([]string, 0, len(where))
	for c := range where {
		wcols = append(wcols, c)
	}
	slices.Sort(wcols)
	param := strings.Join(params, ", ")
	prefixSQL, args, err := prefixRows(api, params, append(slices.Clone(cols), wcols...), perfNow)
	if err != nil {
		return nil, nil, err
	}
	q2 := struct{ Text string }{prefixSQL}
	var sel, order, conds []string
	for _, c := range cols {
		id := "s." + pgx.Identifier{c}.Sanitize()
		sel = append(sel, id+"::text AS "+pgx.Identifier{c}.Sanitize())
		order = append(order, id)
		conds = append(conds, id+" IS NOT NULL")
	}
	for _, c := range wcols {
		args = append(args, where[c])
		conds = append(conds, fmt.Sprintf("s.%s::text = $%d", pgx.Identifier{c}.Sanitize(), len(args)))
	}
	text := fmt.Sprintf(`SELECT %s FROM (
  SELECT %s, row_number() OVER (ORDER BY %s) AS rn_, count(*) OVER () AS cnt_
  FROM (%s) AS s WHERE %s
) AS s WHERE rn_ = greatest(1, ceil(cnt_ * %v)::int8)`,
		strings.Join(quoted(cols, "s."), ", "), strings.Join(sel, ", "), strings.Join(order, ", "), q2.Text, strings.Join(conds, " AND "), q)
	ss := make([]*string, len(cols))
	dst := make([]any, len(cols))
	for i := range ss {
		dst[i] = &ss[i]
	}
	if err := conn.QueryRow(ctx, text, args...).Scan(dst...); err != nil {
		if err == pgx.ErrNoRows {
			return nil, nil, fmt.Errorf("perf: 参数 %s: 生成的数据里 %s 一行都没有(关联没有命中?)\n%s", param, strings.Join(cols, ", "), text)
		}
		return nil, nil, fmt.Errorf("perf: 参数 %s 的取样: %w\n%s", param, err, text)
	}
	out := make([]any, len(cols))
	texts := make([]string, len(cols))
	for i, s := range ss {
		var err error
		if out[i], err = core.FromPGText(s, ks[i]); err != nil {
			return nil, nil, err
		}
		texts[i] = *s
	}
	return out, texts, nil
}

func quoted(cols []string, prefix string) []string {
	var out []string
	for _, c := range cols {
		out = append(out, prefix+pgx.Identifier{c}.Sanitize())
	}
	return out
}

// prefixRows: SQL for the rows a parameter is compared with: the pipeline's steps before the first step using one of
// params, without the other request parameters' conditions, projected to cols (now() = now). Shared by perf's
// sampling and factory loadgen.
func prefixRows(api *ir.API, params []string, cols []string, now any) (string, []any, error) {
	steps := api.Pipeline.Steps
	at := slices.IndexFunc(steps, func(st core.Step) bool {
		return slices.ContainsFunc(params, func(p string) bool { return stepRefs(st, p) })
	})
	param := strings.Join(params, ", ")
	if at < 0 {
		return "", nil, fmt.Errorf("perf: 参数 %s 没有在流水线里用到", param)
	}
	var prefix []core.Step
	for _, st := range steps[:at] {
		if st.Kind == "filter" {
			if st.Pred = dropParams(st.Pred); st.Pred == nil {
				continue
			}
		} else if stepRefs(st, "") {
			return "", nil, fmt.Errorf("perf: 第 %s 步用到了请求参数,取样还不支持(factory/perf/sample.go)", st.Kind)
		}
		if st.Kind == "sort" || st.Kind == "limit" {
			continue
		}
		prefix = append(prefix, st)
	}
	var proj []core.NamedExpr
	for _, c := range cols {
		if slices.ContainsFunc(proj, func(x core.NamedExpr) bool { return x.Name == c }) {
			continue
		}
		proj = append(proj, core.NamedExpr{Name: c, Expr: &core.Expr{Op: "col", Name: c}})
	}
	prefix = append(prefix, core.Step{Kind: "project", Project: proj})
	q, err := core.ToSQL(&core.Pipeline{Steps: prefix, Params: api.Pipeline.Params}, core.Options{})
	if err != nil {
		return "", nil, fmt.Errorf("perf: 参数 %s 的取样流水线(%s 之前的步骤): %w", param, strings.Join(cols, ", "), err)
	}
	var args []any
	for _, d := range q.Params {
		if d.Name != core.NowParam {
			return "", nil, fmt.Errorf("perf: 取样流水线还用到参数 %s", d.Name)
		}
		args = append(args, sqlArg(now))
	}
	return q.Text, args, nil
}

// stepRefs: the step uses request parameter name ("" = any request parameter; now() is not one).
func stepRefs(st core.Step, name string) bool {
	if exprRefs(st.Pred, name) || (st.AsOf != nil && exprRefs(st.AsOf.Ref, name)) || st.Param != "" && (name == "" || st.Param == name) {
		return true
	}
	for _, x := range st.Project {
		if exprRefs(x.Expr, name) {
			return true
		}
	}
	for _, s := range st.Source {
		if stepRefs(s, name) {
			return true
		}
	}
	return false
}

func exprRefs(e *core.Expr, name string) bool {
	if e == nil {
		return false
	}
	switch e.Op {
	case "param", "absent":
		if name == "" || e.Name == name {
			return true
		}
	}
	for _, p := range e.Params {
		if name == "" || p == name {
			return true
		}
	}
	for _, a := range e.Args {
		if exprRefs(a, name) {
			return true
		}
	}
	return false
}

// dropParams: the predicate without the conjuncts that use request parameters (nil = nothing left).
func dropParams(e *core.Expr) *core.Expr {
	if e == nil || !exprRefs(e, "") {
		return e
	}
	if e.Op != "and" {
		return nil
	}
	var keep []*core.Expr
	for _, a := range e.Args {
		if x := dropParams(a); x != nil {
			keep = append(keep, x)
		}
	}
	switch len(keep) {
	case 0:
		return nil
	case 1:
		return keep[0]
	}
	c := *e
	c.Args = keep
	return &c
}
