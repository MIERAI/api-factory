package perf

// EXPLAIN ANALYZE of one request (pipeline APIs: per variant of the parameters; escape-hatch SQL: the spec's first OK
// request) and the plan rules. Hard rules (✗):
//   - an external sort (spilled to disk)
//   - a sequential scan of a big table that throws away ≥ 90% of its rows while a required filter is given
//     (= an index is missing), or one repeated per outer row (a nested loop over a full scan)
//   - execution time > the threshold (the threshold scales with the machine: calibrate.go)
// Known boundaries (⚠, listed with their reason): the 包含 filter (btree cannot help), non-key filters after 取最新版本.
// API kinds perf has no check for are ✗, unless listed in boundaryKinds with the reason. Write APIs: write.go.

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"apifactory/factory/core"
	"apifactory/factory/ir"
)

// perfNow: now() in perf requests (inside the generated data's time range).
var perfNow = time.Date(2024, 4, 1, 0, 0, 0, 0, time.UTC)

// boundaryKinds: API kinds (ir.API.Pattern) without a check, and why (none today: reads here, writes in write.go).
// Anything else without a check fails.
var boundaryKinds = map[string]string{}

func unknownKind(api *ir.API) Report {
	r := Report{API: api.ID, Variant: api.Pattern}
	if why, ok := boundaryKinds[api.Pattern]; ok {
		r.Boundary = why
		return r
	}
	r.Fail = append(r.Fail, fmt.Sprintf("perf 没有这种 API 的检查(pattern %q,没有流水线 / 逃生口 SQL);要么在 factory/perf 里加检查,要么写进 boundaryKinds 并说明原因", api.Pattern))
	return r
}

// explain: variant = "" 只有必填参数 / "*" 全部参数 / 参数名 必填参数 + 这一个可选参数。
func explain(ctx context.Context, conn *pgx.Conn, plan *ir.Plan, api *ir.API, variant string, opt Options, tables map[string]*table) (Report, error) {
	all := variant == "*"
	r := Report{API: api.ID, Variant: "必填参数"}
	switch variant {
	case "":
	case "*":
		r.Variant = "全部参数"
	default:
		r.Variant = "+" + variant
	}
	q, err := core.ToSQL(api.Pipeline, core.Options{})
	if err != nil {
		return r, err
	}
	filters := map[string]ir.Filter{}
	tuple := map[string]ir.Filter{} // 组属于的字段的参数 → 这个筛选
	for _, f := range api.Filters {
		if f.Op == "in_tuples" {
			for _, c := range f.Columns {
				m := ir.MemberParam(f.Field, c)
				filters[m] = ir.Filter{Field: m, Column: c, Op: "in"}
				tuple[m] = f
			}
			continue
		}
		filters[f.Field] = f
	}
	type given struct {
		at int // index in q.Params
		d  core.ParamDecl
		f  ir.Filter
	}
	args := make([]any, len(q.Params))
	var gs []given
	var requiredCols, givenCols, containsCols []string
	for i, d := range q.Params {
		switch {
		case d.Name == core.NowParam:
			args[i] = sqlArg(perfNow)
		case d.Name == "row_limit" && api.Keyset != nil:
			args[i] = int64(api.Keyset.DefaultSize + 1)
		case strings.HasPrefix(d.Name, "cursor_"): // 第一页
		default:
			f, ok := filters[d.Name]
			req := required(api, d)
			if !ok { // not a filter (例: 相对时间的天数): the spec's first OK request
				if req {
					x, err := baseValue(api, d.Name, d.Type.Kind)
					if err != nil {
						return r, fmt.Errorf("参数 %s: %w", d.Name, err)
					}
					args[i] = sqlArg(x)
				}
				continue
			}
			if !req && !all && d.Name != variant && tuple[d.Name].Field != variant {
				continue
			}
			if f.Op == "contains" && !slices.Contains(containsCols, f.Column) { // 包含: btree 用不上
				containsCols = append(containsCols, f.Column)
			}
			if req && !slices.Contains(requiredCols, f.Column) {
				requiredCols = append(requiredCols, f.Column)
			}
			givenCols = append(givenCols, f.Column)
			gs = append(gs, given{i, d, f})
		}
	}
	// samples: the point filters (=, in, prefix, contains, valid_at) of one request from one row; then each range /
	// ne filter at its quantile among the rows matching the point values; tuples (组属于) one row per group
	values := map[int]any{}
	where := map[string]string{}
	var point []given
	for _, g := range gs {
		if _, isTuple := tuple[g.d.Name]; !isTuple && isPoint(g.f.Op) {
			point = append(point, g)
		}
	}
	if len(point) > 0 {
		var ps, cs []string
		var ks []core.Kind
		for _, g := range point {
			if !slices.Contains(cs, g.f.Column) {
				ps, cs, ks = append(ps, g.d.Name), append(cs, g.f.Column), append(ks, g.d.Type.Kind)
			}
		}
		xs, texts, err := sampleParam(ctx, conn, api, ps, cs, ks, 0.5, nil)
		if err == nil {
			for _, g := range point {
				j := slices.Index(cs, g.f.Column)
				values[g.at], where[g.f.Column] = xs[j], texts[j]
			}
		}
	}
	tupleRow := map[string][]any{}
	for _, g := range gs {
		if _, done := values[g.at]; done {
			continue
		}
		if tf, ok := tuple[g.d.Name]; ok {
			row, ok := tupleRow[tf.Field]
			if !ok {
				var ks []core.Kind
				var ps []string
				for _, c := range tf.Columns {
					ps = append(ps, ir.MemberParam(tf.Field, c))
					ks = append(ks, api.Pipeline.Param(ir.MemberParam(tf.Field, c)).Type.Kind)
				}
				var err error
				if row, _, err = sampleParam(ctx, conn, api, ps, tf.Columns, ks, 0.5, nil); err != nil {
					return r, fmt.Errorf("参数 %s: %w", tf.Field, err)
				}
				tupleRow[tf.Field] = row
			}
			values[g.at] = row[slices.Index(tf.Columns, g.f.Column)]
			continue
		}
		w := map[string]string{}
		for c, v := range where {
			if c != g.f.Column {
				w[c] = v
			}
		}
		one := func(w map[string]string) (any, error) {
			xs, _, err := sampleParam(ctx, conn, api, []string{g.d.Name}, []string{g.f.Column}, []core.Kind{g.d.Type.Kind}, quantile(g.f.Op), w)
			if err != nil {
				return nil, err
			}
			return xs[0], nil
		}
		x, err := one(w)
		if err != nil && len(w) > 0 { // no row with the other values (the column comes later in the pipeline …): alone
			x, err = one(nil)
		}
		if err != nil {
			return r, fmt.Errorf("参数 %s: %w", g.d.Name, err)
		}
		values[g.at] = x
	}
	for _, g := range gs {
		x := values[g.at]
		if s, isText := x.(string); isText && (g.f.Op == "prefix" || g.f.Op == "contains") {
			x = string([]rune(s)[:min(2, len([]rune(s)))])
		}
		if g.d.List {
			x = []any{x}
		}
		args[g.at] = sqlArg(x)
	}
	if err := analyze(ctx, conn, q.Text, args, opt, tables, &r, requiredCols); err != nil {
		return r, err
	}
	if variant == "" && r.Rows == 0 {
		r.Warn = append(r.Warn, "只有必填参数时 0 行: 样本参数没有命中数据,这个计划可能不代表真实负载")
	}
	// known boundaries replace the failures they explain: the time of a version scan (取最新版本 before a non-key
	// condition, or a join to a logical table on a non-key column); the sequential scan (and its time) of 包含
	switch {
	case r.slow != "" && versionScan(api, givenCols) != "":
		r.Boundary = r.slow + ": " + versionScan(api, givenCols)
	case r.slow != "" && joinVersionScan(api) != "":
		r.Boundary = r.slow + ": " + joinVersionScan(api)
	case len(containsCols) > 0 && r.seqScanOnly:
		r.Boundary = fmt.Sprintf("%s: 已知边界: 包含(部分一致)的筛选 %s 用不上 btree 索引(需要的话建 pg_trgm 的 GIN 索引,或改用 前缀匹配)",
			strings.Join(append(r.Fail, r.slow), "; "), strings.Join(containsCols, ", "))
		r.Fail = nil
	case r.slow != "":
		r.Fail = append(r.Fail, r.slow)
	}
	return r, nil
}

// isPoint: filters whose sample is one row's value (the request's point filters are sampled together).
func isPoint(op string) bool {
	switch op {
	case "eq", "in", "prefix", "contains", "valid_at":
		return true
	}
	return false
}

var sqlcArgRe = regexp.MustCompile(`sqlc\.n?arg\(\s*'?(\w+)'?\s*\)`)

// explainEscape: escape-hatch SQL (datasource.sql) with the spec's first OK request's values.
func explainEscape(ctx context.Context, conn *pgx.Conn, api *ir.API, opt Options, tables map[string]*table) (Report, error) {
	r := Report{API: api.ID, Variant: "逃生口 SQL(第一个 OK 用例的参数)"}
	index := map[string]int{}
	var args []any
	var required []string
	types := map[string]ir.Param{}
	for _, p := range api.Query.Params {
		types[p.Name] = p
	}
	var ferr error
	text := sqlcArgRe.ReplaceAllStringFunc(api.Query.SQL, func(m string) string {
		name := sqlcArgRe.FindStringSubmatch(m)[1]
		if i, ok := index[name]; ok {
			return fmt.Sprintf("$%d", i)
		}
		var v any
		p := types[name]
		k, _ := kindOfSQL(p.Type)
		switch {
		case name == "row_limit":
			v = int64(51)
			if api.Keyset != nil {
				v = int64(api.Keyset.DefaultSize + 1)
			}
		case strings.HasPrefix(name, "cursor_"):
		default:
			if _, has := api.BaseRequest[name]; !has {
				if p.NotNull {
					ferr = fmt.Errorf("参数 %s: 第一个 OK 用例的请求里没有", name)
				}
				break
			}
			x, err := baseValue(api, name, k)
			if err != nil {
				ferr = err
			}
			v = sqlArg(x)
			required = append(required, name)
		}
		args = append(args, v)
		index[name] = len(args)
		return fmt.Sprintf("$%d", len(args))
	})
	if ferr != nil {
		return r, ferr
	}
	if err := analyze(ctx, conn, text, args, opt, tables, &r, required); err != nil {
		return r, err
	}
	if r.slow != "" {
		r.Fail = append(r.Fail, r.slow)
	}
	return r, nil
}

// baseValue: a parameter's value from the spec's first OK request (protojson) as the parameter's kind.
func baseValue(api *ir.API, name string, k core.Kind) (any, error) {
	raw, ok := api.BaseRequest[name]
	if !ok {
		return nil, fmt.Errorf("必填参数 %s 不是筛选,第一个 OK 用例的请求里也没有(perf 不知道用什么值)", name)
	}
	var s string
	switch x := raw.(type) {
	case float64:
		s = strconv.FormatFloat(x, 'f', -1, 64)
	case string:
		s = x
	case bool:
		s = strconv.FormatBool(x)
	default:
		return nil, fmt.Errorf("参数 %s: 第一个 OK 用例的值 %v 不是标量", name, raw)
	}
	if k == core.Timestamp {
		t, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			return nil, fmt.Errorf("参数 %s: %w", name, err)
		}
		return t, nil
	}
	if k == "" {
		return s, nil
	}
	return core.FromPGText(&s, k)
}

// analyze: EXPLAIN ANALYZE (the fastest of 3 runs: the first one warms the cache) + the plan rules.
func analyze(ctx context.Context, conn *pgx.Conn, text string, args []any, opt Options, tables map[string]*table, r *Report, requiredCols []string) error {
	var best map[string]any
	r.Ms = -1
	for range 3 {
		var raw string
		if err := conn.QueryRow(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+text, args...).Scan(&raw); err != nil {
			return fmt.Errorf("explain: %w\n%s", err, text)
		}
		var doc []struct {
			Plan          map[string]any `json:"Plan"`
			ExecutionTime float64        `json:"Execution Time"`
		}
		if err := json.Unmarshal([]byte(raw), &doc); err != nil || len(doc) == 0 {
			return fmt.Errorf("explain json: %v", err)
		}
		if r.Ms < 0 || doc[0].ExecutionTime < r.Ms {
			r.Ms, best = doc[0].ExecutionTime, doc[0].Plan
		}
	}
	if v, ok := best["Actual Rows"].(float64); ok {
		r.Rows = int(v)
	}
	var nodes []string
	var walk func(n map[string]any, parallel bool)
	walk = func(n map[string]any, parallel bool) {
		typ, _ := n["Node Type"].(string)
		rel, _ := n["Relation Name"].(string)
		label := typ
		if rel != "" {
			label += "(" + rel + ")"
		}
		if idx, _ := n["Index Name"].(string); idx != "" {
			label += "[" + idx + "]"
		}
		nodes = append(nodes, label)
		if m, _ := n["Sort Method"].(string); strings.Contains(m, "external") {
			var keys []string
			if ks, ok := n["Sort Key"].([]any); ok {
				for _, k := range ks {
					keys = append(keys, fmt.Sprint(k))
				}
			}
			r.Fail = append(r.Fail, fmt.Sprintf("排序溢出到磁盘(%s);考虑按排序键建索引: %s", m, strings.Join(keys, ", ")))
		}
		if typ == "Seq Scan" {
			if t, ok := tables[rel]; ok && t.n >= 10000 {
				rows, _ := n["Actual Rows"].(float64)
				removed, _ := n["Rows Removed by Filter"].(float64)
				loops, _ := n["Actual Loops"].(float64)
				switch {
				case loops > 1 && !parallel: // under Gather the loops are the parallel workers
					r.Fail = append(r.Fail, fmt.Sprintf("全表扫描 %s(%d 行)重复了 %.0f 次(嵌套循环的内侧);对方的键需要索引", rel, t.n, loops))
				case removed >= 0.9*(rows+removed) && removed > 0 && len(requiredCols) > 0:
					r.Fail = append(r.Fail, fmt.Sprintf("有必填的筛选条件却全表扫描 %s(%d 行,扔掉 %.0f%%);考虑索引 (%s)", rel, t.n, 100*removed/(rows+removed), strings.Join(requiredCols, ", ")))
					r.seqScanOnly = true
				case removed >= 0.9*(rows+removed) && removed > 0:
					r.Warn = append(r.Warn, fmt.Sprintf("全表扫描 %s(%d 行,扔掉 %.0f%%);只传这些可选条件时没有索引可用", rel, t.n, 100*removed/(rows+removed)))
				}
			}
		}
		if kids, ok := n["Plans"].([]any); ok {
			for _, k := range kids {
				if m, ok := k.(map[string]any); ok {
					walk(m, parallel || strings.HasPrefix(typ, "Gather"))
				}
			}
		}
	}
	walk(best, false)
	r.Plan = strings.Join(nodes, " → ")
	if limit := opt.MaxMs * opt.Scale; r.Ms > limit {
		r.slow = fmt.Sprintf("执行时间 %.1fms > %.0fms", r.Ms, limit)
	}
	return nil
}
