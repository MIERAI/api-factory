package golang

// 状态机(R5)的 Go 代码: 每个收到的消息类型一个转移处理器(consume.Handler: 一条条件更新 UPDATE … WHERE 状态 = 从 [AND 守卫],
// 不满足时按状态图分类成 忽略 / 拒绝)+ 超时扫描的三个函数(sm.Scanner 的 Due / Fire / Stats)+ 载入。
// 写操作和发出的消息复用组合命令 / 消息处理器的生成器(cmdGen: 一个叫 实体 的更新操作,发出里 实体.x = 它的返回值)。

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"apifactory/factory/ir"
)

func init() { handlerGens["machine"] = genMachine }

const entityOp = "实体"

type loadGen struct {
	fn, row, varName string
	load             *ir.MachineLoad
}

// genMachine: 一个状态机 → <machine>_gen.go。
func genMachine(g *genCtx) (string, error) {
	m := g.api.Command.Machine
	if m == nil {
		return "", fmt.Errorf("machine: no machine in the IR")
	}
	base := g.api
	defer func() { g.api = base }()
	delete(g.imports, "connectrpc.com/connect")
	g.use(runtimeModule+"/txn", "")
	g.use(runtimeModule+"/consume", "")
	g.use(runtimeModule+"/message", "")
	g.use(runtimeModule+"/sm", "")
	g.use("github.com/jackc/pgx/v5", "")
	lower := strings.ToLower(m.Name[:1]) + m.Name[1:]
	loads := map[string]*loadGen{}
	var b strings.Builder
	fmt.Fprintf(&b, "// 状态机 %s(实体 %s,状态列 %s):\n", m.Name, m.Table, m.State.Name)
	for _, st := range m.States {
		switch {
		case st.Final:
			fmt.Fprintf(&b, "//   %-12s %d  终态\n", st.Name, st.DB)
		default:
			dl := ""
			if st.Deadline > 0 {
				dl = fmt.Sprintf(",期限 %d 秒 → %s", st.Deadline, st.DeadlineTo)
			}
			fmt.Fprintf(&b, "//   %-12s %d  命令 %s,%d 秒之后重发、之后每 %d 秒%s\n", st.Name, st.DB, st.Command.Type, st.Timeout, st.Every, dl)
		}
	}
	b.WriteString("\n")
	for i := range m.Loads {
		ld := &m.Loads[i]
		lg := &loadGen{fn: lower + "Load" + goCamelCase(ld.Name), row: lower + goCamelCase(ld.Name) + "Row", varName: "load" + goCamelCase(ld.Name), load: ld}
		loads[ld.Name] = lg
		src, err := genLoad(g, m, lg)
		if err != nil {
			return "", err
		}
		b.WriteString(src)
	}
	for _, r := range m.Receives {
		src, err := genTransition(g, base, m, r, loads)
		if err != nil {
			return "", fmt.Errorf("machine %s: %s: %w", m.Name, r.Type, err)
		}
		b.WriteString(src)
	}
	g.api = base
	src, err := genTimeouts(g, base, m, loads)
	if err != nil {
		return "", fmt.Errorf("machine %s: timeouts: %w", m.Name, err)
	}
	b.WriteString(src)
	g.api = base
	return g.header(b.String()), nil
}

// entityAPI: 生成写操作 / 发出用的 API: 请求 = req(收到的消息的 data,超时 = 空),操作 = 一个叫 实体 的更新。
func entityAPI(base *ir.API, m *ir.Machine, req *ir.Message, op ir.CommandOp) *ir.API {
	a := *base
	a.Request = *req
	a.Command = &ir.Command{Op: "machine", Ops: []ir.CommandOp{op}, Contracts: m.Contracts, Machine: m}
	return &a
}

// stateValues: 转移到 to: 状态 = to,进入时间 = now,重发计数 = 0,版本 + 1。
func stateValues(m *ir.Machine, to *ir.MachineState) []ir.CommandVal {
	return []ir.CommandVal{
		{Column: m.State.Name, Type: m.State.Type, Expr: &ir.CmdExpr{Op: "const", Value: strconv.FormatInt(to.DB, 10), Type: m.State.Type}},
		{Column: m.Entered, Type: "timestamptz", Expr: &ir.CmdExpr{Op: "now", Type: "timestamptz"}},
		{Column: m.Attempts, Type: "int4", Expr: &ir.CmdExpr{Op: "const", Value: "0", Type: colType(m, m.Attempts)}},
	}
}

func colType(m *ir.Machine, name string) string {
	for _, c := range m.Columns {
		if c.Name == name {
			return c.Type
		}
	}
	return "int4"
}

func incr(m *ir.Machine, col string) ir.CommandVal {
	t := colType(m, col)
	return ir.CommandVal{Column: col, Type: t, Expr: &ir.CmdExpr{Op: "+", Type: t, Args: []*ir.CmdExpr{
		{Op: "col", Name: col, Type: t}, {Op: "const", Value: "1", Type: t}}}}
}

// guardSQL: 守卫里的 实体.x → 这一行的列(UPDATE 的 WHERE 里求值)。
func guardSQL(x *ir.CmdExpr) *ir.CmdExpr {
	if x == nil {
		return nil
	}
	cp := *x
	if cp.Op == "ref" && cp.Ref == entityOp {
		return &ir.CmdExpr{Op: "col", Name: cp.Name, Type: cp.Type}
	}
	cp.Args = nil
	for _, a := range x.Args {
		cp.Args = append(cp.Args, guardSQL(a))
	}
	return &cp
}

// msgExpr: 表达式的设计稿写法(消息的字段写 消息.x)。
func msgExpr(x *ir.CmdExpr) string { return strings.ReplaceAll(formatExpr(x), "输入.", "消息.") }

func stateNames(m *ir.Machine, names []string) string {
	return strings.Join(names, " / ")
}

// transitionEmits: 转移到 to 时发出的消息: to 是等待状态 → 先发它的命令,再发转移的 发出。
func transitionEmits(to *ir.MachineState, extra []ir.CommandEmit) []ir.CommandEmit {
	var out []ir.CommandEmit
	if to.Command != nil {
		out = append(out, *to.Command)
	}
	return append(out, extra...)
}

// loadCalls: 发出里用到的载入 → 读子行的语句(返回值 r1 的键)。
func loadCalls(m *ir.Machine, emits []ir.CommandEmit, loads map[string]*loadGen, ind, ret string) string {
	used := map[string]bool{}
	for _, em := range emits {
		for _, v := range em.Values {
			if v.List && v.Expr.Op == "ref" && v.Expr.Ref == "载入" {
				used[v.Expr.Name] = true
			}
		}
	}
	var names []string
	for n := range used {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, n := range names {
		lg := loads[n]
		fmt.Fprintf(&b, "%s%s, err := %s(ctx, tx, r1.%s)\n%sif err != nil {\n%s\treturn %serr\n%s}\n", ind, lg.varName, lg.fn, goCamelCase(m.Key.Name), ind, ind, ret, ind)
	}
	return b.String()
}

func genTransition(g *genCtx, base *ir.API, m *ir.Machine, r ir.MachineReceive, loads map[string]*loadGen) (string, error) {
	ct := m.Contract(r.Type)
	if ct == nil {
		return "", fmt.Errorf("no contract")
	}
	var tr *ir.MachineTransition
	for i := range m.Transitions {
		if m.Transitions[i].On == r.Type {
			tr = &m.Transitions[i]
		}
	}
	from, to := m.StateByName(tr.From), m.StateByName(tr.To)
	op := MachineTransitionOp(m, tr, r)
	g.api = entityAPI(base, m, &ct.Message, op)
	cg := newCmdGen(g)
	cg.loads = loads
	lit, vars, _, err := cg.op(op, 1)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	late := "没有"
	if len(r.Late) > 0 {
		late = stateNames(m, r.Late)
	}
	guard := ""
	if tr.Guard != nil {
		guard = fmt.Sprintf(",守卫 %s", msgExpr(tr.Guard))
	}
	fmt.Fprintf(&b, "// %s 状态机 %s 收到 %s v%d(入口 = RabbitMQ 的队列,不是 HTTP)。\n// 转移: %s → %s%s。不能转移时按当前状态分类: %s → 忽略(迟到 / 重复);其他 → 拒绝(DLQ)\n",
		r.Method, m.Name, r.Type, r.Version, tr.From, tr.To, guard, late)
	fmt.Fprintf(&b, "func (h *Handler) %s(ctx context.Context, tx pgx.Tx, env message.Envelope) error {\n", r.Method)
	fmt.Fprintf(&b, "\tm := &%s{}\n\tif err := message.DecodeData(env, m); err != nil { // 解不开 / 违反契约 = 毒消息\n\t\treturn consume.Poison(err)\n\t}\n", g.pbType(&ct.Message))
	for _, v := range vars {
		fmt.Fprintf(&b, "\t%s\n", v)
	}
	fmt.Fprintf(&b, "\terr := txn.RunIn(ctx, tx, %s\n\t)\n", lit)
	b.WriteString("\tswitch txn.ReasonOf(err) {\n\tcase txn.None:\n\t\tif err != nil {\n\t\t\treturn err\n\t\t}\n")
	fmt.Fprintf(&b, "\tcase txn.Unmet: // 没有转移(当前状态 = r1.%s)\n\t\tswitch r1.%s {\n", goCamelCase(m.State.Name), goCamelCase(m.State.Name))
	if len(r.Late) > 0 {
		var dbs []string
		for _, n := range r.Late {
			dbs = append(dbs, strconv.FormatInt(m.StateByName(n).DB, 10))
		}
		fmt.Fprintf(&b, "\t\tcase %s: // 迟到 / 重复(%s): 忽略\n\t\t\tsm.Ignored(%q, \"late\")\n\t\t\tconsume.Business(ctx, \"IGNORED\")\n\t\t\treturn nil\n",
			strings.Join(dbs, ", "), late, m.Name)
	}
	g.use("errors", "")
	if tr.Guard != nil {
		fmt.Fprintf(&b, "\t\tcase %d: // %s: 守卫为假 → 拒绝(数据不一致,不自动吞掉)\n\t\t\tsm.Rejected(%q, \"guard\")\n\t\t\treturn consume.Poison(errors.New(%q))\n",
			from.DB, from.Name, m.Name, fmt.Sprintf("%s: %s in %s: guard %s is false", m.Name, r.Type, from.Name, msgExpr(tr.Guard)))
	}
	g.use("fmt", "")
	fmt.Fprintf(&b, "\t\t}\n\t\tsm.Rejected(%q, \"impossible\") // 不可能的消息(某处有 bug)\n\t\treturn consume.Poison(fmt.Errorf(%q, r1.%s))\n",
		m.Name, m.Name+": "+r.Type+" cannot happen in state %d", goCamelCase(m.State.Name))
	fmt.Fprintf(&b, "\tcase txn.NotFound: // 实体不存在 → 拒绝\n\t\tsm.Rejected(%q, \"not_found\")\n\t\treturn consume.Poison(err)\n\tdefault:\n\t\t_, err := handlerResult(err)\n\t\treturn err\n\t}\n", m.Name)
	fmt.Fprintf(&b, "\tsm.Transitioned(ctx, %q, %q, %q, %q)\n", m.Name, tr.From, tr.To, r.Type) // ctx: 追踪的事件 sm.transition
	if to.Final && m.Created != "" {
		g.use(runtimeModule+"/clock", "")
		fmt.Fprintf(&b, "\tsm.Completed(%q, %q, r1.%s, clock.Now(ctx))\n", m.Name, to.Name, goCamelCase(m.Created))
	}
	emits := transitionEmits(to, tr.Emits)
	b.WriteString(loadCalls(m, emits, loads, "\t", ""))
	e, err := cg.emits(emits, "\t")
	if err != nil {
		return "", err
	}
	b.WriteString(e)
	b.WriteString("}\n\n")
	return b.String(), nil
}

// MachineTransitionOp: 转移 tr(收到 r)的写操作: UPDATE 实体 SET 状态 = 到, 进入时间 = now, 重发计数 = 0, 版本 + 1
// WHERE 键 AND 状态 = 从 [AND 守卫]。生成的处理器和 factory perf(写入的检查)用同一个。
func MachineTransitionOp(m *ir.Machine, tr *ir.MachineTransition, r ir.MachineReceive) ir.CommandOp {
	from, to := m.StateByName(tr.From), m.StateByName(tr.To)
	cond := &ir.CmdExpr{Op: "=", Args: []*ir.CmdExpr{
		{Op: "col", Name: m.State.Name, Type: m.State.Type},
		{Op: "const", Value: strconv.FormatInt(from.DB, 10), Type: m.State.Type}}}
	if tr.Guard != nil {
		cond = &ir.CmdExpr{Op: "and", Args: []*ir.CmdExpr{cond, guardSQL(tr.Guard)}}
	}
	return ir.CommandOp{Name: entityOp, Kind: "update", Table: m.Table,
		Key:       []ir.CommandVal{{Column: m.Key.Name, Type: m.Key.Type, Expr: &ir.CmdExpr{Op: "input", Name: r.KeyField, Type: m.Key.Type}}},
		Values:    append(stateValues(m, to), incr(m, m.Version)),
		Cond:      cond,
		Unmet:     "STATE",
		Returning: m.Columns,
		NotFound:  fmt.Sprintf("%s not found: {%s}", m.Table, r.KeyField),
	}
}

// MachineDueSQL: 到期的实体($1 = now,$2 = 最多几个)。MachineReadSQL: 超时处理读一个实体($1 = 键)。
// MachineLoadSQL: 载入的子行($1 = 实体的键)。生成的代码和 factory perf 用同一份 SQL。
func MachineDueSQL(m *ir.Machine) string {
	q := func(s string) string { return `"` + s + `"` }
	var conds []string
	for _, st := range m.States {
		if st.Final {
			continue
		}
		c := fmt.Sprintf("%s + interval '1 second' * (%d + %d * %s) <= $1", q(m.Entered), st.Timeout, st.Every, q(m.Attempts))
		if st.Deadline > 0 {
			c = fmt.Sprintf("(%s OR %s + interval '%d seconds' <= $1)", c, q(m.Entered), st.Deadline)
		}
		conds = append(conds, fmt.Sprintf("(%s = %d AND %s)", q(m.State.Name), st.DB, c))
	}
	return fmt.Sprintf("SELECT %s FROM %s WHERE %s ORDER BY %s, %s LIMIT $2", q(m.Key.Name), q(m.Table), strings.Join(conds, " OR "), q(m.Entered), q(m.Key.Name))
}

func MachineReadSQL(m *ir.Machine) string {
	q := func(s string) string { return `"` + s + `"` }
	return fmt.Sprintf("SELECT %s, %s, %s, %s FROM %s WHERE %s = $1", q(m.State.Name), q(m.Version), q(m.Attempts), q(m.Entered), q(m.Table), q(m.Key.Name))
}

func MachineLoadSQL(ld *ir.MachineLoad) string {
	q := func(s string) string { return `"` + s + `"` }
	var cols, order []string
	for _, c := range ld.Columns {
		cols = append(cols, q(c.Name))
	}
	for i, c := range ld.OrderBy {
		dir := "ASC"
		if ld.Desc[i] {
			dir = "DESC"
		}
		order = append(order, q(c)+" "+dir)
	}
	return fmt.Sprintf("SELECT %s FROM %s WHERE %s = $1 ORDER BY %s LIMIT %d", strings.Join(cols, ", "), q(ld.Table), q(ld.Child), strings.Join(order, ", "), ld.Max+1)
}

// genTimeouts: Due(到期的实体)/ Fire(一个实体: 读行 → 期限 / 重发)/ Stats(每个等待状态的实体数、最老的进入时间)。
func genTimeouts(g *genCtx, base *ir.API, m *ir.Machine, loads map[string]*loadGen) (string, error) {
	g.use("time", "")
	var b strings.Builder
	q := func(s string) string { return `"` + s + `"` }
	var waiting []string
	for _, st := range m.States {
		if !st.Final {
			waiting = append(waiting, strconv.FormatInt(st.DB, 10))
		}
	}
	keyGo, _ := goOfSQL(m.Key.Type)
	due := MachineDueSQL(m)
	fmt.Fprintf(&b, "// %sDue: 到期的实体(sm.Scanner.Due): 每个等待状态,进入时间 + 超时 + 重发间隔 × 重发计数 <= now,或者 进入时间 + 期限 <= now。\n", m.Name)
	fmt.Fprintf(&b, "func (h *Handler) %sDue(ctx context.Context, q sm.Querier, now time.Time, limit int) ([]any, error) {\n", m.Name)
	fmt.Fprintf(&b, "\trows, err := q.Query(ctx, %s, now, limit)\n\tif err != nil {\n\t\treturn nil, err\n\t}\n\tkeys, err := pgx.CollectRows(rows, pgx.RowTo[%s])\n\tout := make([]any, len(keys))\n\tfor i, k := range keys {\n\t\tout[i] = k\n\t}\n\treturn out, err\n}\n\n",
		strconv.Quote(due), keyGo)
	// Fire
	stGo, _ := goOfSQL(m.State.Type)
	verGo, _ := goOfSQL(colType(m, m.Version))
	attGo, _ := goOfSQL(colType(m, m.Attempts))
	read := MachineReadSQL(m)
	g.use("errors", "")
	fmt.Fprintf(&b, "// %sFire: 一个实体的超时(sm.Scanner.Fire,在它自己的事务里): 读行 → 期限 / 重发 → UPDATE … WHERE 版本 = 读到的 → 发出。\n// 版本变了(回答 / 别的扫描先改了这一行)= sm.Conflict: 扫描让整个事务重来(重新读、重新判定)。\n", m.Name)
	fmt.Fprintf(&b, "func (h *Handler) %sFire(ctx context.Context, tx pgx.Tx, key any, now time.Time) (sm.Fired, error) {\n", m.Name)
	fmt.Fprintf(&b, "\tvar cur struct {\n\t\tState    %s\n\t\tVersion  %s\n\t\tAttempts %s\n\t\tEntered  time.Time\n\t}\n", stGo, verGo, attGo)
	fmt.Fprintf(&b, "\tif err := tx.QueryRow(ctx, %s, key).Scan(&cur.State, &cur.Version, &cur.Attempts, &cur.Entered); err != nil {\n\t\tif errors.Is(err, pgx.ErrNoRows) {\n\t\t\treturn sm.Fired{}, nil\n\t\t}\n\t\treturn sm.Fired{}, err\n\t}\n\tswitch cur.State {\n", strconv.Quote(read))
	empty := &ir.Message{Name: base.Request.Name}
	keyX := &ir.CmdExpr{Op: "go", Value: "key"}
	lock := &ir.CommandVal{Column: m.Version, Type: colType(m, m.Version), Expr: &ir.CmdExpr{Op: "go", Value: "cur.Version"}}
	fire := func(op ir.CommandOp, emits []ir.CommandEmit, fired, ind string) (string, error) {
		g.api = entityAPI(base, m, empty, op)
		cg := newCmdGen(g)
		cg.loads = loads
		cg.ret = fired + ", "
		lit, vars, _, err := cg.op(op, 1)
		if err != nil {
			return "", err
		}
		var s strings.Builder
		for _, v := range vars {
			fmt.Fprintf(&s, "%s%s\n", ind, v)
		}
		fmt.Fprintf(&s, "%sif err := txn.RunIn(ctx, tx, %s\n%s); err != nil {\n%s\tswitch txn.ReasonOf(err) {\n%s\tcase txn.Conflict: // 版本变了: 重来\n%s\t\treturn sm.Fired{Kind: sm.Conflict}, nil\n%s\tcase txn.NotFound: // 删掉了\n%s\t\treturn sm.Fired{}, nil\n%s\t}\n%s\treturn sm.Fired{}, err\n%s}\n",
			ind, lit, ind, ind, ind, ind, ind, ind, ind, ind, ind)
		s.WriteString(loadCalls(m, emits, loads, ind, "sm.Fired{}, "))
		e, err := cg.emits(emits, ind)
		if err != nil {
			return "", err
		}
		s.WriteString(e)
		return s.String(), nil
	}
	for _, st := range m.States {
		if st.Final {
			continue
		}
		fmt.Fprintf(&b, "\tcase %d: // %s\n", st.DB, st.Name)
		if st.Deadline > 0 {
			to := m.StateByName(st.DeadlineTo)
			op := ir.CommandOp{Name: entityOp, Kind: "update", Table: m.Table, Key: []ir.CommandVal{{Column: m.Key.Name, Type: m.Key.Type, Expr: keyX}},
				Values: stateValues(m, to), Lock: lock, Returning: m.Columns}
			fired := fmt.Sprintf("sm.Fired{Kind: sm.Deadline, From: %q, To: %q}", st.Name, to.Name)
			body, err := fire(op, transitionEmits(to, st.DeadlineEmits), fired, "\t\t\t")
			if err != nil {
				return "", err
			}
			fmt.Fprintf(&b, "\t\tif !cur.Entered.Add(%d * time.Second).After(now) { // 期限 %d 秒 → %s\n", st.Deadline, st.Deadline, st.DeadlineTo)
			if to.Final && m.Created != "" {
				g.use(runtimeModule+"/clock", "")
				body = strings.Replace(body, "\t\t\treturn "+fired, fmt.Sprintf("\t\t\tsm.Completed(%q, %q, r1.%s, now)\n\t\t\treturn %s", m.Name, to.Name, goCamelCase(m.Created), fired), 1)
			}
			b.WriteString(body)
			b.WriteString("\t\t}\n")
		}
		op := ir.CommandOp{Name: entityOp, Kind: "update", Table: m.Table, Key: []ir.CommandVal{{Column: m.Key.Name, Type: m.Key.Type, Expr: keyX}},
			Values: []ir.CommandVal{incr(m, m.Attempts)}, Lock: lock, Returning: m.Columns}
		fired := fmt.Sprintf("sm.Fired{Kind: sm.Retry, From: %q, To: %q}", st.Name, st.Name)
		body, err := fire(op, []ir.CommandEmit{*st.Command}, fired, "\t\t\t")
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "\t\tif !cur.Entered.Add(time.Duration(%d+%d*int64(cur.Attempts)) * time.Second).After(now) { // 重发: 进入之后 %d 秒,之后每 %d 秒\n", st.Timeout, st.Every, st.Timeout, st.Every)
		b.WriteString(body)
		b.WriteString("\t\t}\n")
	}
	b.WriteString("\t}\n\treturn sm.Fired{}, nil\n}\n\n")
	// Stats
	stats := fmt.Sprintf("SELECT %s, count(*), min(%s) FROM %s WHERE %s IN (%s) GROUP BY %s", q(m.State.Name), q(m.Entered), q(m.Table), q(m.State.Name), strings.Join(waiting, ", "), q(m.State.Name))
	var names []string
	for _, st := range m.States {
		if !st.Final {
			names = append(names, fmt.Sprintf("%d: %q", st.DB, st.Name))
		}
	}
	fmt.Fprintf(&b, "// %sStats: 每个等待状态的实体数和最老的进入时间(指标: factory_sm_instances / factory_sm_oldest_in_state_seconds)。\n", m.Name)
	fmt.Fprintf(&b, "func (h *Handler) %sStats(ctx context.Context, q sm.Querier) ([]sm.StateStat, error) {\n", m.Name)
	fmt.Fprintf(&b, "\tnames := map[%s]string{%s}\n\tby := map[string]sm.StateStat{}\n\trows, err := q.Query(ctx, %s)\n\tif err != nil {\n\t\treturn nil, err\n\t}\n\tdefer rows.Close()\n", stGo, strings.Join(names, ", "), strconv.Quote(stats))
	fmt.Fprintf(&b, "\tfor rows.Next() {\n\t\tvar s %s\n\t\tvar x sm.StateStat\n\t\tif err := rows.Scan(&s, &x.Count, &x.Oldest); err != nil {\n\t\t\treturn nil, err\n\t\t}\n\t\tx.State = names[s]\n\t\tby[x.State] = x\n\t}\n\tif err := rows.Err(); err != nil {\n\t\treturn nil, err\n\t}\n", stGo)
	var order []string
	for _, st := range m.States {
		if !st.Final {
			order = append(order, strconv.Quote(st.Name))
		}
	}
	fmt.Fprintf(&b, "\tvar out []sm.StateStat\n\tfor _, n := range []string{%s} {\n\t\tx := by[n]\n\t\tx.State = n\n\t\tout = append(out, x)\n\t}\n\treturn out, nil\n}\n", strings.Join(order, ", "))
	return b.String(), nil
}

// genLoad: 载入的子行(在转移 / 超时的事务里读;多于上限 = 数据的问题,报错不截断)。
func genLoad(g *genCtx, m *ir.Machine, lg *loadGen) (string, error) {
	ld := lg.load
	var fields []string
	for _, c := range ld.Columns {
		goType, ok := goOfSQL(c.Type)
		if !ok {
			return "", fmt.Errorf("载入 %s: column %s: type %s", ld.Name, c.Name, c.Type)
		}
		fields = append(fields, fmt.Sprintf("\t%s %s", goCamelCase(c.Name), ptr(goType, !c.NotNull)))
	}
	keyGo, _ := goOfSQL(m.Key.Type)
	sql := MachineLoadSQL(ld)
	g.use("fmt", "")
	var b strings.Builder
	fmt.Fprintf(&b, "// %s: 载入 %s 的一行。\ntype %s struct {\n%s\n}\n\n", lg.row, ld.Name, lg.row, strings.Join(fields, "\n"))
	fmt.Fprintf(&b, "// %s: 载入 %s: %s 里 %s = 实体的键 的行(最多 %d 行;多于 %d 行 = 数据的问题,报错不截断)。\n", lg.fn, ld.Name, ld.Table, ld.Child, ld.Max, ld.Max)
	fmt.Fprintf(&b, "func %s(ctx context.Context, tx pgx.Tx, key %s) ([]%s, error) {\n\trows, err := tx.Query(ctx, %s, key)\n\tif err != nil {\n\t\treturn nil, err\n\t}\n\tout, err := pgx.CollectRows(rows, pgx.RowToStructByPos[%s])\n\tif err != nil {\n\t\treturn nil, err\n\t}\n\tif len(out) > %d {\n\t\treturn nil, consume.Poison(fmt.Errorf(%q, key))\n\t}\n\treturn out, nil\n}\n\n",
		lg.fn, keyGo, lg.row, strconv.Quote(sql), lg.row, ld.Max, m.Name+": 载入 "+ld.Name+": more than "+strconv.Itoa(ld.Max)+" rows for %v")
	return b.String(), nil
}

// loadValue: 列表字段 ← 载入的子行(按名字取契约元素的字段)。
func (cg *cmdGen) loadValue(name string, cf *ir.Field) (string, error) {
	lg := cg.loads[name]
	if lg == nil || cf.Message == nil {
		return "", fmt.Errorf("载入.%s: no such load", name)
	}
	var cp []string
	for _, f := range cf.Message.Fields {
		var col *ir.Column
		for i := range lg.load.Columns {
			if lg.load.Columns[i].Name == f.Name {
				col = &lg.load.Columns[i]
			}
		}
		if col == nil {
			return "", fmt.Errorf("载入.%s: no column %s", name, f.Name)
		}
		goType, _ := goOfSQL(col.Type)
		e, err := cg.g.resExpr("x."+goCamelCase(col.Name), ptr(goType, !col.NotNull), &f)
		if err != nil {
			return "", err
		}
		cp = append(cp, fmt.Sprintf("%s: %s,", goCamelCase(f.Name), e))
	}
	el := cg.g.pbType(cf.Message)
	return fmt.Sprintf("func() []*%[1]s {\n\tout := make([]*%[1]s, 0, len(%[2]s))\n\tfor _, x := range %[2]s {\n\t\tout = append(out, &%[1]s{%[3]s})\n\t}\n\treturn out\n}()",
		el, lg.varName, strings.Join(cp, " ")), nil
}
