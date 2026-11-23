package perf

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"apifactory/factory/ir"
)

// checkWrites: (a) EXPLAIN of every write API's operations and state machine scans, the foreign key index rule,
// (b) + (c) the concurrency check. Runs after the read checks (it changes the data).
func checkWrites(ctx context.Context, conn *pgx.Conn, plan *ir.Plan, tables map[string]*table, opt Options) ([]Report, error) {
	var out []Report
	seq := map[string]int{}
	written := writtenParents(plan)
	for i := range plan.APIs {
		api := &plan.APIs[i]
		if api.Command == nil {
			continue
		}
		specs, err := writeSpecs(api)
		if err != nil {
			out = append(out, Report{API: api.ID, Variant: api.Pattern, Fail: []string{err.Error()}})
			continue
		}
		out = append(out, fkIndexes(ctx, conn, api, tables, written)...)
		for _, ws := range specs {
			rs, err := explainWrite(ctx, conn, ws, tables, opt, seq)
			if err != nil {
				out = append(out, Report{API: api.ID, Variant: ws.variant, Fail: []string{"写入的 EXPLAIN: " + err.Error()}})
				continue
			}
			out = append(out, rs...)
			if opt.Workers > 0 {
				out = append(out, concurrency(ctx, conn, ws, tables, opt, seq))
			}
		}
	}
	return out, nil
}

// explainWrite: one request; each operation EXPLAIN ANALYZE'd inside the request's transaction (rolled back).
func explainWrite(ctx context.Context, conn *pgx.Conn, ws writeSpec, tables map[string]*table, opt Options, seq map[string]int) ([]Report, error) {
	envs, err := genRequests(ctx, conn, ws, tables, 1, seq, nil)
	if err != nil {
		return nil, err
	}
	env := envs[0]
	tx, err := conn.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var out []Report
	n := 0
	explain := func(name, sql string, args []any) error {
		n++
		r := Report{API: ws.api.ID, Variant: fmt.Sprintf("%s: %s", ws.variant, name)}
		if _, err := tx.Exec(ctx, "SAVEPOINT perf_explain"); err != nil {
			return err
		}
		var raw string
		err := tx.QueryRow(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+sql, args...).Scan(&raw)
		if _, rerr := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT perf_explain"); rerr != nil {
			return rerr
		}
		if err != nil { // the statement itself fails here (a constraint …): the run below reports it the way the API would
			r.Warn = append(r.Warn, "EXPLAIN 时语句失败(样本值): "+err.Error())
			out = append(out, r)
			return nil
		}
		writeRules(raw, tables, opt, &r)
		out = append(out, r)
		return nil
	}
	if err := runRequest(ctx, tx, ws, env, explain); err != nil && !business(err) {
		return nil, fmt.Errorf("样本请求失败: %w", err)
	} else if err != nil && len(out) > 0 {
		out[len(out)-1].Warn = append(out[len(out)-1].Warn, "样本请求的结果: "+err.Error()+"(之后的操作没有检查)")
	}
	for _, q := range ws.queries {
		r := Report{API: ws.api.ID, Variant: q.name}
		args := q.args(env)
		if err := analyze(ctx, conn, q.sql, args, opt, tables, &r, []string{"(定时 / 每条消息都跑)"}); err != nil {
			return nil, err
		}
		if r.slow != "" {
			r.Fail = append(r.Fail, r.slow)
		}
		out = append(out, r)
	}
	return out, nil
}

// writeRules: a write statement's plan. ✗: an update / delete that scans the table instead of using the key; a
// foreign key check (trigger) or the statement slower than the threshold.
func writeRules(raw string, tables map[string]*table, opt Options, r *Report) {
	var doc []struct {
		Plan          map[string]any `json:"Plan"`
		ExecutionTime float64        `json:"Execution Time"`
		Triggers      []struct {
			Name     string  `json:"Trigger Name"`
			Relation string  `json:"Relation"`
			Time     float64 `json:"Time"`
			Calls    float64 `json:"Calls"`
		} `json:"Triggers"`
	}
	if err := json.Unmarshal([]byte(raw), &doc); err != nil || len(doc) == 0 {
		r.Fail = append(r.Fail, fmt.Sprintf("explain json: %v", err))
		return
	}
	r.Ms = doc[0].ExecutionTime
	limit := opt.MaxMs * opt.Scale
	var nodes []string
	var walk func(n map[string]any)
	walk = func(n map[string]any) {
		typ, _ := n["Node Type"].(string)
		rel, _ := n["Relation Name"].(string)
		label := typ
		if op, _ := n["Operation"].(string); op != "" {
			label = op
		}
		if rel != "" {
			label += "(" + rel + ")"
		}
		if idx, _ := n["Index Name"].(string); idx != "" {
			label += "[" + idx + "]"
		}
		nodes = append(nodes, label)
		if typ == "Seq Scan" {
			if t, ok := tables[rel]; ok && t.n >= 10000 {
				r.Fail = append(r.Fail, fmt.Sprintf("按键的写入却全表扫描 %s(%d 行);键需要索引(主键 / 唯一约束)", rel, t.n))
			}
		}
		if kids, ok := n["Plans"].([]any); ok {
			for _, k := range kids {
				if m, ok := k.(map[string]any); ok {
					walk(m)
				}
			}
		}
	}
	walk(doc[0].Plan)
	for _, tr := range doc[0].Triggers {
		nodes = append(nodes, fmt.Sprintf("触发器 %s(%s) %.2fms", tr.Name, tr.Relation, tr.Time))
		if tr.Time > limit {
			r.Fail = append(r.Fail, fmt.Sprintf("触发器 %s(%s,外键检查等)%.1fms > %.0fms", tr.Name, tr.Relation, tr.Time, limit))
		}
	}
	r.Plan = strings.Join(nodes, " → ")
	if r.Ms > limit {
		r.Fail = append(r.Fail, fmt.Sprintf("执行时间 %.1fms > %.0fms", r.Ms, limit))
	}
}

// writtenParents: tables some write API deletes rows of (the foreign key check runs on their children).
func writtenParents(plan *ir.Plan) map[string][]string {
	out := map[string][]string{}
	for i := range plan.APIs {
		api := &plan.APIs[i]
		if api.Command == nil || api.Command.Op == "machine" {
			continue
		}
		for _, op := range commandOps(api.Command) {
			if op.Kind == "delete" && !slices.Contains(out[op.Table], api.ID) {
				out[op.Table] = append(out[op.Table], api.ID)
			}
		}
	}
	return out
}

// fkIndexes: for each table this API deletes from, every foreign key referencing it needs an index on the child's
// columns (else each delete scans the child table, under a lock).
func fkIndexes(ctx context.Context, conn *pgx.Conn, api *ir.API, tables map[string]*table, written map[string][]string) []Report {
	var out []Report
	for parent, apis := range written {
		if !slices.Contains(apis, api.ID) {
			continue
		}
		p := tables[parent]
		if p == nil {
			continue
		}
		for _, child := range sortedTables(tables) {
			groups := map[string][]string{}
			for col, r := range child.refs {
				if r.hard && r.parent == p {
					groups[r.group] = append(groups[r.group], col)
				}
			}
			var names []string
			for g := range groups {
				names = append(names, g)
			}
			sort.Strings(names)
			for _, g := range names {
				cols := groups[g]
				sort.Strings(cols)
				r := Report{API: api.ID, Variant: fmt.Sprintf("外键 %s(%s) → %s", child.name, strings.Join(cols, ", "), parent)}
				ok, err := leadingIndex(ctx, conn, child.name, cols)
				switch {
				case err != nil:
					r.Fail = append(r.Fail, err.Error())
				case !ok:
					r.Fail = append(r.Fail, fmt.Sprintf("删除 %s 的行时外键检查要扫 %s 全表: 给 %s(%s) 建索引", parent, child.name, child.name, strings.Join(cols, ", ")))
				default:
					r.Plan = "子表的外键列有索引"
				}
				out = append(out, r)
			}
		}
	}
	return out
}

// leadingIndex: an index of table whose leading columns are exactly cols (any order).
func leadingIndex(ctx context.Context, conn *pgx.Conn, table string, cols []string) (bool, error) {
	rows, err := conn.Query(ctx, `
		SELECT array(SELECT a.attname::text FROM unnest(i.indkey) WITH ORDINALITY k(n, o)
		             JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = k.n ORDER BY k.o)
		FROM pg_index i WHERE i.indrelid = to_regclass($1) AND i.indpred IS NULL`, pgx.Identifier{table}.Sanitize())
	if err != nil {
		return false, err
	}
	idx, err := pgx.CollectRows(rows, pgx.RowTo[[]string])
	if err != nil {
		return false, err
	}
	for _, ix := range idx {
		if len(ix) < len(cols) {
			continue
		}
		lead := slices.Clone(ix[:len(cols)])
		sort.Strings(lead)
		if slices.Equal(lead, cols) {
			return true, nil
		}
	}
	return false, nil
}

func commandOps(c *ir.Command) []ir.CommandOp {
	if c.Op == "machine" {
		return nil
	}
	if c.Op == "command" || c.Op == "message" {
		return c.Ops
	}
	return []ir.CommandOp{{Kind: c.Op, Table: c.Table}}
}
