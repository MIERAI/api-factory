package perf

// Write APIs (新建 / 按键更新 / 按键删除 / 组合命令 / 消息处理 / 状态机): the SQL the generated handlers issue, built
// by the same code — the IR's operations (command.OpsOf; a state machine's transitions: the adapter's
// MachineTransitionOp) through the runtime's txn.Build / txn.RunIn — against the perf database (200k rows per table).
//
//	(a) EXPLAIN ANALYZE of every operation of one request (inside a transaction that is rolled back), the state
//	    machines' scans (due / read / load), and a static rule: deleting (or re-keying) a parent row runs the foreign
//	    key check on the child, which needs an index on the child's columns.
//	(b) concurrency (concurrency.go): N workers on distinct keys; pg_locks / pg_stat_activity sampled.
//	(c) scaling: throughput at N workers ÷ 1 worker ≥ a ratio.
//
// Not covered here: the runtime's own writes (outbox, consumer de-duplication) — runtime/outbox's perf gate.
//
// Request values: an input that is the key of an update / delete (or its version) takes an existing row's value
// (rows read from the table: distinct rows per request, in the state the operation's condition wants, without child
// rows for deletes); an input written by an insert takes the data generator's value for a new row (number n+1, n+2 …:
// new keys, references to existing parents, CHECKs satisfied); other inputs a constant of their type.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/MIERAI/api-factory/runtime/txn"
	"github.com/jackc/pgx/v5"

	golang "apifactory/factory/adapters/golang"
	"apifactory/factory/command"
	"apifactory/factory/core"
	"apifactory/factory/ir"
)

// writeSpec: one kind of request of a write API (a state machine: one per received message type).
type writeSpec struct {
	api     *ir.API
	variant string
	ops     []ir.CommandOp
	queries []namedQuery // state machine: due / read / load (EXPLAIN only)
	pool    map[string][]string // table → extra conditions on the rows used as keys (state = from)
	used    map[string]bool     // rows already taken (table|primary key): the next phase takes others
}

type namedQuery struct {
	name, sql string
	args      func(env *reqEnv) []any
}

func writeSpecs(api *ir.API) ([]writeSpec, error) {
	c := api.Command
	switch c.Op {
	case "insert", "update", "delete", "command", "message":
		ops := slices.Clone(command.OpsOf(c))
		if c.Op == "message" && c.Each != "" { // a message handler's 每项: all its operations per element
			for i := range ops {
				if ops[i].Each == "" {
					ops[i].Each = c.Each
				}
			}
		}
		for _, op := range ops {
			switch op.Kind {
			case "insert", "insert_idempotent", "update", "delete":
			default:
				return nil, fmt.Errorf("写入的检查不认识操作 %s 的种类 %q(factory/perf/write.go)", op.Name, op.Kind)
			}
		}
		return []writeSpec{{api: api, variant: "写入", ops: ops, used: map[string]bool{}}}, nil
	case "machine":
		m := c.Machine
		var out []writeSpec
		for _, r := range m.Receives {
			var tr *ir.MachineTransition
			for i := range m.Transitions {
				if m.Transitions[i].On == r.Type {
					tr = &m.Transitions[i]
				}
			}
			if tr == nil {
				return nil, fmt.Errorf("状态机 %s: 收到 %s 没有转移", m.Name, r.Type)
			}
			op := golang.MachineTransitionOp(m, tr, r)
			from := m.StateByName(tr.From)
			ws := writeSpec{api: api, variant: "收到 " + r.Type, ops: []ir.CommandOp{op}, used: map[string]bool{},
				pool: map[string][]string{m.Table: {fmt.Sprintf("%s = %d", pgx.Identifier{m.State.Name}.Sanitize(), from.DB)}}}
			out = append(out, ws)
		}
		if len(out) > 0 { // the scans of the timeouts and the loads (once)
			key := m.Receives[0].KeyField
			arg := func(env *reqEnv) []any { return []any{env.inputs[key]} }
			out[0].queries = append(out[0].queries,
				namedQuery{"到期扫描(超时)", golang.MachineDueSQL(m), func(*reqEnv) []any { return []any{time.Now(), 100} }},
				namedQuery{"读实体(超时)", golang.MachineReadSQL(m), arg})
			for i := range m.Loads {
				out[0].queries = append(out[0].queries, namedQuery{"载入 " + m.Loads[i].Name, golang.MachineLoadSQL(&m.Loads[i]), arg})
			}
		}
		return out, nil
	}
	return nil, fmt.Errorf("写入的检查不认识命令的种类 %q", c.Op)
}

// binding: where an input / item field's value comes from.
type binding struct {
	table, col string
	kind       string // key / version / new
	typ        string // SQL type of the column
}

// bindInputs: input X / item X → the column that decides its value (keys first, then versions, then inserts).
func bindInputs(ops []ir.CommandOp) map[string]binding {
	out := map[string]binding{}
	name := func(x *ir.CmdExpr) string {
		if x != nil && (x.Op == "input" || x.Op == "item") {
			return x.Op + ":" + x.Name
		}
		return ""
	}
	for _, op := range ops { // col >= input (an amount) / col = col - input: the smallest amount, so the condition holds
		for _, v := range op.Values {
			if x := v.Expr; x != nil && x.Op == "-" && len(x.Args) == 2 && x.Args[0].Op == "col" {
				if n := name(x.Args[1]); n != "" {
					out[n] = binding{kind: "one", typ: x.Args[1].Type}
				}
			}
		}
		for _, x := range conjuncts(op.Cond) {
			if (x.Op == ">=" || x.Op == ">") && len(x.Args) == 2 && x.Args[0].Op == "col" {
				if n := name(x.Args[1]); n != "" {
					out[n] = binding{kind: "one", typ: x.Args[1].Type}
				}
			}
		}
	}
	for _, op := range ops {
		if op.Kind == "update" || op.Kind == "delete" {
			for _, k := range op.Key {
				if n := name(k.Expr); n != "" {
					if _, ok := out[n]; !ok {
						out[n] = binding{op.Table, k.Column, "key", k.Type}
					}
				}
			}
			for _, x := range conjuncts(op.Cond) { // 守卫 / 条件: col = input → the row's value (the condition holds)
				if x.Op == "=" && len(x.Args) == 2 {
					for _, p := range [][2]*ir.CmdExpr{{x.Args[0], x.Args[1]}, {x.Args[1], x.Args[0]}} {
						if p[0].Op == "col" {
							if n := name(p[1]); n != "" {
								if _, ok := out[n]; !ok {
									out[n] = binding{op.Table, p[0].Name, "key", p[1].Type}
								}
							}
						}
					}
				}
			}
			if op.Lock != nil {
				if n := name(op.Lock.Expr); n != "" {
					if _, ok := out[n]; !ok {
						out[n] = binding{op.Table, op.Lock.Column, "version", op.Lock.Type}
					}
				}
			}
		}
	}
	for _, op := range ops {
		if op.Kind == "insert" || op.Kind == "insert_idempotent" {
			for _, v := range op.Values {
				if n := name(v.Expr); n != "" {
					if _, ok := out[n]; !ok {
						out[n] = binding{op.Table, v.Column, "new", v.Type}
					}
				}
			}
		}
	}
	return out
}

// reqEnv: one request's values.
type reqEnv struct {
	inputs map[string]any
	items  []map[string]any
	env    string
	refs   map[string]map[string]any // op → returning column → value (filled while running)
}

const itemsPerRequest = 3

// usesItems: the operations loop over a list (每项).
func usesItems(ops []ir.CommandOp) bool {
	return slices.ContainsFunc(ops, func(op ir.CommandOp) bool { return op.Each != "" })
}

// genRequests: count requests for ws (every call takes other rows / new row numbers: the phases of a check never share
// keys). pick(n, i): which of n candidate rows request i uses — nil = a different row per request (distinct keys).
func genRequests(ctx context.Context, conn *pgx.Conn, ws writeSpec, tables map[string]*table, count int, seq map[string]int, pick func(n, i int) int) ([]*reqEnv, error) {
	bind := bindInputs(ws.ops)
	items := 0
	if usesItems(ws.ops) {
		items = itemsPerRequest
	}
	envs := make([]*reqEnv, count)
	for i := range envs {
		envs[i] = &reqEnv{inputs: map[string]any{}, refs: map[string]map[string]any{}, env: fmt.Sprintf("perf-%d-%d", time.Now().UnixNano(), i)}
	}
	// group the bindings by table and kind (a version is read from the key's row)
	type group struct {
		table, kind string
		names       []string // input:X / item:X
	}
	groups := map[string]*group{}
	for n, b := range bind {
		kind := b.kind
		if kind == "one" { // the operation's constant 1 (runner.value)
			continue
		}
		if kind == "version" {
			kind = "key"
		}
		k := b.table + "|" + kind
		if groups[k] == nil {
			groups[k] = &group{table: b.table, kind: kind}
		}
		groups[k].names = append(groups[k].names, n)
	}
	gkeys := make([]string, 0, len(groups))
	for k := range groups {
		gkeys = append(gkeys, k)
	}
	// rows with both request-level and per-item columns first: they decide how many items a request has
	mixed := func(g *group) bool {
		in, it := false, false
		for _, n := range g.names {
			in, it = in || strings.HasPrefix(n, "input:"), it || strings.HasPrefix(n, "item:")
		}
		return in && it && g.kind == "key"
	}
	sort.Slice(gkeys, func(i, j int) bool {
		mi, mj := mixed(groups[gkeys[i]]), mixed(groups[gkeys[j]])
		if mi != mj {
			return mi
		}
		return gkeys[i] < gkeys[j]
	})
	itemsSet := false
	setItems := func(k int) {
		for _, e := range envs {
			for len(e.items) < k {
				e.items = append(e.items, map[string]any{})
			}
		}
		items, itemsSet = k, true
	}
	assign := func(e *reqEnv, j int, n string, s *string) error {
		v, err := typed(s, bind[n].typ)
		if err != nil {
			return fmt.Errorf("%s: %w", n, err)
		}
		if strings.HasPrefix(n, "item:") {
			e.items[j][strings.TrimPrefix(n, "item:")] = v
		} else {
			e.inputs[strings.TrimPrefix(n, "input:")] = v
		}
		return nil
	}
	for _, k := range gkeys {
		g := groups[k]
		sort.Strings(g.names)
		var cols []string
		for _, n := range g.names {
			cols = append(cols, bind[n].col)
		}
		t := tables[g.table]
		if t == nil {
			return nil, fmt.Errorf("表 %s 不在 schema 里", g.table)
		}
		label := map[string]string{"key": "已有行", "new": "新行"}[g.kind]
		if mixed(g) { // (transfer_id, account_id) with account_id per item: the items' rows share the request's values
			var inCols []int
			for c, n := range g.names {
				if strings.HasPrefix(n, "input:") {
					inCols = append(inCols, c)
				}
			}
			rows, err := existingRows(ctx, conn, ws, tables, bind, t, g.names, cols, 0, nil)
			if err != nil {
				return nil, fmt.Errorf("%s 的%s: %w", g.table, label, err)
			}
			byReq := map[string][][]*string{}
			var order []string
			for _, r := range rows {
				var kk []string
				for _, c := range inCols {
					kk = append(kk, deref(r[c]))
				}
				key := strings.Join(kk, "\x00")
				if _, ok := byReq[key]; !ok {
					order = append(order, key)
				}
				byReq[key] = append(byReq[key], r)
			}
			per := 0
			for p := itemsPerRequest; p >= 1 && per == 0; p-- { // as many items as enough requests can have
				n := 0
				for _, key := range order {
					if len(byReq[key]) >= p && !ws.used["req|"+g.table+"|"+key] {
						n++
					}
				}
				if n >= count {
					per = p
				}
			}
			if per == 0 {
				return nil, fmt.Errorf("%s 的%s: 没有 %d 组(请求的值相同的行)能用", g.table, label, count)
			}
			setItems(per)
			i := 0
			for _, key := range order {
				if i == count {
					break
				}
				if len(byReq[key]) < per || ws.used["req|"+g.table+"|"+key] {
					continue
				}
				ws.used["req|"+g.table+"|"+key] = true
				for j := range per {
					for c, n := range g.names {
						if err := assign(envs[i], j, n, byReq[key][j][c]); err != nil {
							return nil, err
						}
					}
				}
				i++
			}
			continue
		}
		if !itemsSet {
			setItems(items)
		}
		per := 1
		if slices.ContainsFunc(g.names, func(n string) bool { return strings.HasPrefix(n, "item:") }) {
			per = max(1, items)
		}
		need := count * per
		var rows [][]*string
		var err error
		if g.kind == "key" {
			rows, err = existingRows(ctx, conn, ws, tables, bind, t, g.names, cols, need, pick)
		} else {
			rows, err = newRows(ctx, conn, t, cols, need, seq)
		}
		if err != nil {
			return nil, fmt.Errorf("%s 的%s: %w", g.table, label, err)
		}
		for i, e := range envs {
			for j := range per {
				for c, n := range g.names {
					jj := j
					if !strings.HasPrefix(n, "item:") {
						jj = 0
					}
					if err := assign(e, jj, n, rows[i*per+j][c]); err != nil {
						return nil, err
					}
				}
			}
		}
	}
	if !itemsSet {
		setItems(items)
	}
	return envs, nil
}

func deref(s *string) string {
	if s == nil {
		return "\x01null"
	}
	return *s
}


// existingRows: rows of t (columns cols, as text) for the names bound to them. need = 0: all candidates (the caller
// groups them). The candidates: rows in the state the operations' conditions want (col = constant), whose values
// also exist where the same input is another table's key, without child rows when an operation deletes them; rows an
// earlier phase took are skipped (a hash order + an offset kept in seq).
func existingRows(ctx context.Context, conn *pgx.Conn, ws writeSpec, tables map[string]*table, bind map[string]binding, t *table, names, cols []string, need int, pick func(n, i int) int) ([][]*string, error) {
	conds := append([]string{"true"}, ws.pool[t.name]...)
	for _, op := range ws.ops {
		for _, k := range op.Key { // the same input is the key of another table too: the value must exist there
			n := ""
			if k.Expr != nil && (k.Expr.Op == "input" || k.Expr.Op == "item") {
				n = k.Expr.Op + ":" + k.Expr.Name
			}
			if c := slices.Index(names, n); c >= 0 && op.Table != t.name {
				conds = append(conds, fmt.Sprintf("EXISTS (SELECT 1 FROM %s AS o WHERE o.%s = p.%s)",
					pgx.Identifier{op.Table}.Sanitize(), pgx.Identifier{k.Column}.Sanitize(), pgx.Identifier{cols[c]}.Sanitize()))
			}
		}
		for _, v := range op.Values { // col = col - (another operation's value): small values there, large values here
			x := v.Expr
			if op.Kind != "update" || x == nil || x.Op != "-" || len(x.Args) != 2 || x.Args[0].Op != "col" {
				continue
			}
			if op.Table == t.name {
				conds = append(conds, fmt.Sprintf("p.%s >= 100", pgx.Identifier{x.Args[0].Name}.Sanitize()))
			}
			if a := x.Args[1]; a.Op == "ref" {
				for _, src := range ws.ops {
					if src.Name == a.Ref && src.Table == t.name {
						conds = append(conds, fmt.Sprintf("p.%s <= 100", pgx.Identifier{a.Name}.Sanitize()))
					}
				}
			}
		}
		if op.Table != t.name {
			continue
		}
		if op.Kind == "delete" {
			for _, child := range sortedTables(tables) {
				groups := map[string][][2]string{}
				for col, r := range child.refs {
					if r.hard && r.parent == t {
						groups[r.group] = append(groups[r.group], [2]string{col, r.slot})
					}
				}
				for _, pairs := range groups {
					var on []string
					for _, p := range pairs {
						on = append(on, fmt.Sprintf("c.%s = p.%s", pgx.Identifier{p[0]}.Sanitize(), pgx.Identifier{p[1]}.Sanitize()))
					}
					conds = append(conds, fmt.Sprintf("NOT EXISTS (SELECT 1 FROM %s AS c WHERE %s)", pgx.Identifier{child.name}.Sanitize(), strings.Join(on, " AND ")))
				}
			}
		}
		if op.Kind == "update" && op.Cond != nil {
			for _, x := range conjuncts(op.Cond) {
				if len(x.Args) != 2 || x.Args[0].Op != "col" {
					continue
				}
				col := pgx.Identifier{x.Args[0].Name}.Sanitize()
				switch {
				case x.Op == "=" && x.Args[1].Op == "const":
					conds = append(conds, fmt.Sprintf("p.%s::text = %s", col, quote(x.Args[1].Value)))
				case x.Op == ">=" || x.Op == ">": // col >= an amount: not at its minimum
					conds = append(conds, fmt.Sprintf("p.%s >= 10", col))
				}
			}
		}
	}
	var sel []string
	for _, c := range cols {
		sel = append(sel, "p."+pgx.Identifier{c}.Sanitize()+"::text")
	}
	var pk []string // stable order and identity of a row (its primary key; updates change other columns)
	for _, c := range t.keyCols() {
		pk = append(pk, "p."+pgx.Identifier{c}.Sanitize()+"::text")
	}
	if len(pk) == 0 {
		pk = []string{"p.ctid::text"}
	}
	q := fmt.Sprintf("SELECT concat_ws('|', %s), %s FROM %s AS p WHERE %s ORDER BY hashtext(concat_ws('|', %s)) LIMIT 400000",
		strings.Join(pk, ", "), strings.Join(sel, ", "), pgx.Identifier{t.name}.Sanitize(), strings.Join(conds, " AND "), strings.Join(pk, ", "))
	rows, err := conn.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("%w\n%s", err, q)
	}
	var out [][]*string
	var ids []string
	for rows.Next() {
		var id string
		r := make([]*string, len(cols))
		dst := []any{&id}
		for i := range r {
			dst = append(dst, &r[i])
		}
		if err := rows.Scan(dst...); err != nil {
			rows.Close()
			return nil, err
		}
		if ws.used[t.name+"|"+id] {
			continue
		}
		out, ids = append(out, r), append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if need == 0 {
		return out, nil
	}
	if pick != nil {
		if len(out) == 0 {
			return nil, fmt.Errorf("没有能用的行\n%s", q)
		}
		picked := make([][]*string, need)
		for i := range picked {
			picked[i] = out[pick(len(out), i)%len(out)]
		}
		return picked, nil
	}
	if len(out) < need {
		return nil, fmt.Errorf("只有 %d 行能用,需要 %d 行(不同的键)\n%s", len(out), need, q)
	}
	for _, id := range ids[:need] {
		ws.used[t.name+"|"+id] = true
	}
	return out[:need], nil
}

func conjuncts(x *ir.CmdExpr) []*ir.CmdExpr {
	if x == nil {
		return nil
	}
	if x.Op == "and" {
		var out []*ir.CmdExpr
		for _, a := range x.Args {
			out = append(out, conjuncts(a)...)
		}
		return out
	}
	return []*ir.CmdExpr{x}
}

// newRows: the data generator's values of cols for new rows (row numbers after the data: new keys).
func newRows(ctx context.Context, conn *pgx.Conn, t *table, cols []string, need int, seq map[string]int) ([][]*string, error) {
	var exprs []string
	for _, c := range cols {
		k, ok := t.kindOf(c)
		if !ok {
			return nil, fmt.Errorf("%s.%s: 列不在 schema 里(或类型数据生成器不认识)", t.name, c)
		}
		e, err := t.colExpr(core.NamedType{Name: c, Type: core.Type{Kind: k}}, "i", 0, false)
		if err != nil {
			return nil, fmt.Errorf("%s.%s: %w", t.name, c, err)
		}
		exprs = append(exprs, "("+e+")::text")
	}
	start := t.n + 1 + seq[t.name]
	seq[t.name] += need
	q := fmt.Sprintf("SELECT %s FROM generate_series(%d, %d) AS i", strings.Join(exprs, ", "), start, start+need-1)
	rows, err := conn.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("%w\n%s", err, q)
	}
	defer rows.Close()
	var out [][]*string
	for rows.Next() {
		r := make([]*string, len(cols))
		dst := make([]any, len(cols))
		for i := range r {
			dst[i] = &r[i]
		}
		if err := rows.Scan(dst...); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// typed: a value as text → the Go value for a parameter of SQL type typ.
func typed(s *string, typ string) (any, error) {
	if s == nil {
		return nil, nil
	}
	k, ok := kindOfSQL(typ)
	if !ok {
		return *s, nil // enum mapping / jsonb …: the text
	}
	v, err := core.FromPGText(s, k)
	if err != nil {
		return nil, err
	}
	return sqlArg(v), nil
}

// constOf: a value of SQL type typ for inputs no column decides (amounts in conditions …).
func constOf(typ string) any {
	switch k, _ := kindOfSQL(typ); k {
	case core.Int32, core.Int64:
		return int64(1)
	case core.Decimal:
		return "1"
	case core.Bool:
		return true
	case core.Date:
		return perfNow.Format("2006-01-02")
	case core.Timestamp:
		return time.Now()
	}
	return "perf"
}

// runner: the operations of one request → txn.Ops (the adapter's mapping, composite.go: value / expr / op).
type runner struct {
	env     *reqEnv
	item    map[string]any
	pending []func() // after the operation ran: its RETURNING values → env.refs
}

func (r *runner) value(x *ir.CmdExpr) (any, error) {
	switch x.Op {
	case "input":
		if v, ok := r.env.inputs[x.Name]; ok {
			return v, nil
		}
		return constOf(x.Type), nil
	case "item":
		if v, ok := r.item[x.Name]; ok {
			return v, nil
		}
		return constOf(x.Type), nil
	case "ref":
		env, op, col := r.env, x.Ref, x.Name
		return txn.Later(func() any { return env.refs[op][col] }), nil
	case "const":
		if x.Value == "" && x.Type != "text" {
			return nil, nil
		}
		return typed(&x.Value, x.Type)
	case "now":
		return time.Now(), nil
	case "env":
		return r.env.env, nil
	case "null":
		return nil, nil
	case "if":
		c, err := r.eval(x.Args[0])
		if err != nil {
			return nil, err
		}
		if c {
			return r.value(x.Args[1])
		}
		return r.value(x.Args[2])
	}
	return nil, fmt.Errorf("写入的检查: %s 不是值", x.Op)
}

// eval: a condition on inputs / items / constants (the generated code evaluates it in Go).
func (r *runner) eval(x *ir.CmdExpr) (bool, error) {
	switch x.Op {
	case "and":
		for _, a := range x.Args {
			ok, err := r.eval(a)
			if err != nil || !ok {
				return false, err
			}
		}
		return true, nil
	case "=", "<>", "<", "<=", ">", ">=":
		a, err := r.value(x.Args[0])
		if err != nil {
			return false, err
		}
		b, err := r.value(x.Args[1])
		if err != nil {
			return false, err
		}
		if a == nil || b == nil {
			return false, nil
		}
		c := compareAny(a, b)
		return map[string]bool{"=": c == 0, "<>": c != 0, "<": c < 0, "<=": c <= 0, ">": c > 0, ">=": c >= 0}[x.Op], nil
	}
	return false, fmt.Errorf("写入的检查: %s 不是条件", x.Op)
}

func compareAny(a, b any) int {
	fa, ea := strconv.ParseFloat(fmt.Sprint(a), 64)
	fb, eb := strconv.ParseFloat(fmt.Sprint(b), 64)
	if ea == nil && eb == nil {
		switch {
		case fa < fb:
			return -1
		case fa > fb:
			return 1
		}
		return 0
	}
	return strings.Compare(fmt.Sprint(a), fmt.Sprint(b))
}

func (r *runner) expr(x *ir.CmdExpr) (*txn.Expr, error) {
	switch x.Op {
	case "col":
		return txn.C(x.Name), nil
	case "+", "-", "*", "=", "<>", "<", "<=", ">", ">=":
		a, err := r.expr(x.Args[0])
		if err != nil {
			return nil, err
		}
		b, err := r.expr(x.Args[1])
		if err != nil {
			return nil, err
		}
		switch x.Op {
		case "+":
			return txn.Add(a, b), nil
		case "-":
			return txn.Sub(a, b), nil
		case "*":
			if x.Args[0].Op != "col" {
				a = txn.T(a, x.Args[0].Type)
			}
			if x.Args[1].Op != "col" {
				b = txn.T(b, x.Args[1].Type)
			}
			return txn.Mul(a, b), nil
		}
		return txn.Cmp(x.Op, a, b), nil
	case "and":
		var parts []*txn.Expr
		for _, a := range x.Args {
			p, err := r.expr(a)
			if err != nil {
				return nil, err
			}
			parts = append(parts, p)
		}
		return txn.And(parts...), nil
	}
	v, err := r.value(x)
	if err != nil {
		return nil, err
	}
	return txn.V(v), nil
}

func (r *runner) col(v ir.CommandVal) (txn.Col, error) {
	switch v.Expr.Op {
	case "input", "ref", "const", "now", "item", "env", "null", "if":
		x, err := r.value(v.Expr)
		return txn.Col{Name: v.Column, Value: x}, err
	}
	x, err := r.expr(v.Expr)
	return txn.Col{Name: v.Column, Expr: x}, err
}

var opKinds = map[string]txn.Kind{"insert": txn.Insert, "insert_idempotent": txn.Insert, "update": txn.Update, "delete": txn.Delete}

func (r *runner) op(op ir.CommandOp) (txn.Op, error) {
	o := txn.Op{Kind: opKinds[op.Kind], Table: op.Target(), Conflict: op.Conflict, NotFound: op.NotFound}
	for _, v := range op.Values {
		c, err := r.col(v)
		if err != nil {
			return o, fmt.Errorf("op %s: %w", op.Name, err)
		}
		o.Values = append(o.Values, c)
	}
	if op.Kind == "update" && op.Lock != nil {
		o.Values = append(o.Values, txn.Incr1(op.Lock.Column))
	}
	for _, v := range op.Key {
		c, err := r.col(v)
		if err != nil {
			return o, fmt.Errorf("op %s: key: %w", op.Name, err)
		}
		o.Key = append(o.Key, c)
	}
	if op.Lock != nil {
		x, err := r.value(op.Lock.Expr)
		if err != nil {
			return o, err
		}
		o.Version = &txn.Col{Name: op.Lock.Column, Value: x}
	}
	if op.Cond != nil {
		x, err := r.expr(op.Cond)
		if err != nil {
			return o, fmt.Errorf("op %s: cond: %w", op.Name, err)
		}
		o.Cond, o.Unmet = x, op.Unmet
		if op.Skip || o.Unmet == "" {
			o.Unmet = "SKIP"
		}
	}
	if op.Created {
		o.Created = new(bool)
	}
	if len(op.Returning) > 0 {
		refs := map[string]any{}
		r.env.refs[op.Name] = refs
		vals := make([]any, len(op.Returning))
		for i, c := range op.Returning {
			o.Returning = append(o.Returning, c.Name)
			o.Into = append(o.Into, &vals[i])
		}
		r.pending = append(r.pending, func() {
			for i, c := range op.Returning {
				refs[c.Name] = vals[i]
			}
		})
	}
	return o, nil
}

// steps: the operations of a request in order (每项: the consecutive operations of one list run per item).
func steps(ops []ir.CommandOp, env *reqEnv) [][2]int /* (op index, item index or -1) */ {
	var out [][2]int
	for i := 0; i < len(ops); {
		if ops[i].Each == "" {
			out = append(out, [2]int{i, -1})
			i++
			continue
		}
		j := i
		for j < len(ops) && ops[j].Each == ops[i].Each {
			j++
		}
		for it := range env.items {
			for k := i; k < j; k++ {
				out = append(out, [2]int{k, it})
			}
		}
		i = j
	}
	return out
}

// business: an outcome the API answers (not found, version conflict, condition, duplicate, missing parent).
func business(err error) bool {
	var te *txn.Error
	if !errors.As(err, &te) {
		return false
	}
	switch te.Reason {
	case txn.NotFound, txn.Conflict, txn.Unmet, txn.Unique, txn.ForeignKey, txn.Check:
		return true
	}
	return false
}

// runRequest: one request in tx; explain: before each operation, EXPLAIN ANALYZE it (rolled back to a savepoint).
func runRequest(ctx context.Context, tx pgx.Tx, ws writeSpec, env *reqEnv, explain func(name, sql string, args []any) error) error {
	flags := map[string]bool{} // op#item → created (幂等新建) / updated (更新): 仅当 …
	for _, st := range steps(ws.ops, env) {
		op := ws.ops[st[0]]
		r := &runner{env: env}
		if st[1] >= 0 {
			r.item = env.items[st[1]]
		}
		at := func(name string) string { return fmt.Sprintf("%s#%d", name, st[1]) }
		if op.Only != "" && !flags[at(op.Only)] { // 仅当 前面的操作 新建 / 已更新
			continue
		}
		o, err := r.op(op)
		if err != nil {
			return err
		}
		if explain != nil {
			sql, args, err := txn.Build(o)
			if err != nil {
				return err
			}
			if err := explain(op.Name, sql, args); err != nil {
				return err
			}
		}
		err = txn.RunIn(ctx, tx, o)
		switch {
		case err == nil:
			flags[at(op.Name)] = o.Created == nil || *o.Created
		case op.Skip && txn.ReasonOf(err) == txn.Unmet: // 不满足 = 跳过(the generated code does the same)
			flags[at(op.Name)] = false
		default:
			return err
		}
		for _, f := range r.pending {
			f()
		}
	}
	return nil
}

func sortedTables(tables map[string]*table) []*table {
	var out []*table
	for _, t := range tables {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}
