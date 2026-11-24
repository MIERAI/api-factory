package golang

// 组合命令(命令工厂 R3-2)的 Go handler: 按顺序的写操作(txn.Op 的列表)交给运行时的事务执行器,一个事务。
// 条件更新 = Cond + Unmet(业务原因码);幂等新建 = Conflict + Created;后面的操作引用前面的返回值 = txn.Later
// (执行到那个操作时才取值)。全部确定性生成,AI 不写。

import (
	"fmt"
	"strconv"
	"strings"

	"apifactory/factory/ir"
)

func init() { handlerGens["command"] = genComposite }

var compositeKinds = map[string]string{"insert": "txn.Insert", "insert_idempotent": "txn.Insert", "update": "txn.Update", "delete": "txn.Delete"}

// cmdGen: 组合命令 / 消息处理器的操作 → txn.Op 的 Go 代码(两种出口共用)。
type cmdGen struct {
	g    *genCtx
	idx  map[string]int // 操作名 → 第几个(变量 r1, r2 …)
	item *ir.Message    // 每项: 元素的 message(变量 it);nil = 没有 每项
	// R5(状态机): 发出里的 载入.名字(读好的子行的变量);ret = 发出的 return 前面另外的返回值(超时的 Fire: "sm.Fired{…}, ")
	loads map[string]*loadGen
	ret   string
}

func newCmdGen(g *genCtx) *cmdGen {
	cg := &cmdGen{g: g, idx: map[string]int{}}
	for i, op := range g.api.Command.Ops {
		cg.idx[op.Name] = i + 1
	}
	return cg
}

// value: 叶子的表达式 → Go 的值(参数)
func (cg *cmdGen) value(x *ir.CmdExpr) (string, error) {
	g, in := cg.g, &cg.g.api.Request
	switch x.Op {
	case "input":
		f := in.Field(x.Name)
		if f == nil {
			return "", fmt.Errorf("command: 输入.%s is not a request field", x.Name)
		}
		if f.Kind == "enum" {
			m, err := g.compositeEnum(x.Table, x.Column)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("%sToDB(m.Get%s())", m.Helper, goCamelCase(f.Name)), nil
		}
		goType, ok := goOfSQL(x.Type)
		if !ok {
			return "", fmt.Errorf("command: 输入.%s: type %s is not supported", x.Name, x.Type)
		}
		return g.reqExpr(f, goType)
	case "ref":
		i, ok := cg.idx[x.Ref]
		if !ok {
			return "", fmt.Errorf("command: %s is not an op", x.Ref)
		}
		return fmt.Sprintf("txn.Later(func() any { return r%d.%s })", i, goCamelCase(x.Name)), nil
	case "const":
		return constLit(x)
	case "now":
		g.use(runtimeModule+"/clock", "")
		return "clock.Now(ctx)", nil
	case "item": // 每项的当前元素(变量 it)
		return cg.itemValue(x)
	case "env": // 消息的 id
		return "env.ID", nil
	case "null":
		return "nil", nil
	case "go": // 状态机的生成器自己组装的值(Go 的表达式;不出现在 IR 里)
		return x.Value, nil
	case "if": // 如果 条件 则 a 否则 b(条件只看 输入 / 项 / 常量,在 Go 里算)
		c, err := cg.cond(x.Args[0])
		if err != nil {
			return "", err
		}
		a, err := cg.value(x.Args[1])
		if err != nil {
			return "", err
		}
		b, err := cg.value(x.Args[2])
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("func() any {\n\tif %s {\n\t\treturn %s\n\t}\n\treturn %s\n}()", c, a, b), nil
	}
	return "", fmt.Errorf("command: %s is not a value", x.Op)
}

// expr: 表达式 → txn.Expr 的构造
func (cg *cmdGen) expr(x *ir.CmdExpr) (string, error) {
	switch x.Op {
	case "col":
		return fmt.Sprintf("txn.C(%q)", x.Name), nil
	case "+", "-":
		a, err := cg.expr(x.Args[0])
		if err != nil {
			return "", err
		}
		b, err := cg.expr(x.Args[1])
		if err != nil {
			return "", err
		}
		fn := map[string]string{"+": "Add", "-": "Sub"}[x.Op]
		return fmt.Sprintf("txn.%s(%s, %s)", fn, a, b), nil
	case "*": // 乘(R5): 两边常常都是参数,写明参数的类型(Postgres 推不出 $1 * $2 的类型)
		var parts [2]string
		for i, a := range x.Args {
			s, err := cg.expr(a)
			if err != nil {
				return "", err
			}
			if a.Op != "col" {
				s = fmt.Sprintf("txn.T(%s, %q)", s, a.Type)
			}
			parts[i] = s
		}
		return fmt.Sprintf("txn.Mul(%s, %s)", parts[0], parts[1]), nil
	case "=", "<>", "<", "<=", ">", ">=":
		a, err := cg.expr(x.Args[0])
		if err != nil {
			return "", err
		}
		b, err := cg.expr(x.Args[1])
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("txn.Cmp(%q, %s, %s)", x.Op, a, b), nil
	case "and":
		var parts []string
		for _, a := range x.Args {
			p, err := cg.expr(a)
			if err != nil {
				return "", err
			}
			parts = append(parts, p)
		}
		return "txn.And(" + strings.Join(parts, ", ") + ")", nil
	}
	v, err := cg.value(x)
	if err != nil {
		return "", err
	}
	return "txn.V(" + v + ")", nil
}

func (cg *cmdGen) col(v ir.CommandVal) (string, error) {
	switch v.Expr.Op {
	case "input", "ref", "const", "now", "item", "env", "null", "if", "go":
		x, err := cg.value(v.Expr)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("{Name: %q, Value: %s},", v.Column, x), nil
	}
	x, err := cg.expr(v.Expr)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("{Name: %q, Expr: %s},", v.Column, x), nil
}

// op: 第 n 个操作 → txn.Op 的字面值;vars = 返回值 / 新建标志的变量,pre = 组装值的语句(可以不传的输入)。
func (cg *cmdGen) op(op ir.CommandOp, n int) (lit string, vars, pre []string, err error) {
	g, in := cg.g, &cg.g.api.Request
	var lines []string
	lines = append(lines, "Kind: "+compositeKinds[op.Kind]+",", fmt.Sprintf("Table: %q,", op.Target()))
	if len(op.Values) > 0 || op.Lock != nil {
		var cs, optional []string
		for _, v := range op.Values {
			if f := in.Field(v.Expr.Name); v.Expr.Op == "input" && f != nil && f.Optional && f.Kind != "enum" { // 不传 = 不写这一列
				goType, ok := goOfSQL(v.Expr.Type)
				if !ok {
					return "", nil, nil, fmt.Errorf("op %s: 输入.%s: type %s is not supported", op.Name, f.Name, v.Expr.Type)
				}
				e, err := g.reqExpr(f, "*"+goType)
				if err != nil {
					return "", nil, nil, err
				}
				optional = append(optional, fmt.Sprintf("if v := %s; v != nil { // 不传 = 不写这一列(默认值)\n\t\tvals%d = append(vals%d, txn.Col{Name: %q, Value: *v})\n\t}", e, n, n, v.Column))
				continue
			}
			c, err := cg.col(v)
			if err != nil {
				return "", nil, nil, fmt.Errorf("op %s: %w", op.Name, err)
			}
			cs = append(cs, c)
		}
		if op.Kind == "update" && op.Lock != nil { // 乐观锁: 版本 + 1
			cs = append(cs, fmt.Sprintf("txn.Incr1(%q),", op.Lock.Column))
		}
		switch {
		case len(optional) > 0:
			pre = append(pre, fmt.Sprintf("vals%d := []txn.Col{ // %s 的值\n%s\n}", n, op.Name, strings.Join(cs, "\n")))
			pre = append(pre, optional...)
			lines = append(lines, fmt.Sprintf("Values: vals%d,", n))
		case len(cs) > 0:
			lines = append(lines, "Values: []txn.Col{\n"+strings.Join(cs, "\n")+"\n},")
		}
	}
	if len(op.Key) > 0 {
		var cs []string
		for _, v := range op.Key {
			c, err := cg.col(v)
			if err != nil {
				return "", nil, nil, fmt.Errorf("op %s: key: %w", op.Name, err)
			}
			cs = append(cs, c)
		}
		lines = append(lines, "Key: []txn.Col{\n"+strings.Join(cs, "\n")+"\n},")
	}
	if op.Lock != nil {
		x, err := cg.value(op.Lock.Expr)
		if err != nil {
			return "", nil, nil, err
		}
		lines = append(lines, fmt.Sprintf("Version: &txn.Col{Name: %q, Value: %s},", op.Lock.Column, x))
	}
	if op.Cond != nil {
		x, err := cg.expr(op.Cond)
		if err != nil {
			return "", nil, nil, fmt.Errorf("op %s: cond: %w", op.Name, err)
		}
		unmet := op.Unmet
		if op.Skip { // 不满足: 跳过(R4): 运行时要一个原因码,生成的代码按 Unmet 跳过
			unmet = "SKIP"
		}
		lines = append(lines, "Cond: "+x+",", fmt.Sprintf("Unmet: %q,", unmet))
	}
	if len(op.Conflict) > 0 {
		var qs []string
		for _, c := range op.Conflict {
			qs = append(qs, strconv.Quote(c))
		}
		lines = append(lines, "Conflict: []string{"+strings.Join(qs, ", ")+"},")
	}
	if op.Created {
		vars = append(vars, fmt.Sprintf("var created%d bool // %s: 是否真的插入了(幂等新建)", n, op.Name))
		lines = append(lines, fmt.Sprintf("Created: &created%d,", n))
	}
	if len(op.Returning) > 0 {
		var fields, names, into []string
		for _, c := range op.Returning {
			goType, ok := goOfSQL(c.Type)
			if !ok {
				return "", nil, nil, fmt.Errorf("op %s: returning column %s: type %s is not supported", op.Name, c.Name, c.Type)
			}
			goType = ptr(goType, !c.NotNull)
			if strings.Contains(goType, "time.") {
				g.use("time", "")
			}
			fields = append(fields, fmt.Sprintf("%s %s", goCamelCase(c.Name), goType))
			names = append(names, strconv.Quote(c.Name))
			into = append(into, fmt.Sprintf("&r%d.%s", n, goCamelCase(c.Name)))
		}
		vars = append(vars, fmt.Sprintf("var r%d struct { // %s(%s)的返回值\n%s\n}", n, op.Name, op.Table, strings.Join(fields, "\n")))
		lines = append(lines, "Returning: []string{"+strings.Join(names, ", ")+"},", "Into: []any{"+strings.Join(into, ", ")+"},")
	}
	if op.Kind == "update" || op.Kind == "delete" {
		msg, err := g.message(op.NotFound)
		if err != nil {
			return "", nil, nil, err
		}
		lines = append(lines, "NotFound: "+msg+",")
	}
	return fmt.Sprintf("txn.Op{ // %s\n%s\n},", op.Name, strings.Join(lines, "\n")), vars, pre, nil
}

func genComposite(g *genCtx) (string, error) {
	cmd := g.api.Command
	in, out := &g.api.Request, &g.api.Response
	g.use(runtimeModule+"/txn", "")
	cg := newCmdGen(g)
	idx := cg.idx

	var vars, pre, ops []string
	blockVars := map[int][]string{} // 每项 的操作(R5): 循环里的变量
	hasEach := false
	for i, op := range cmd.Ops {
		cg.item = nil
		if op.Each != "" {
			hasEach = true
			lf := in.Field(op.Each)
			if lf == nil || lf.Message == nil {
				return "", fmt.Errorf("op %s: 每项 %s is not a list of messages", op.Name, op.Each)
			}
			cg.item = lf.Message
		}
		lit, v, p, err := cg.op(op, i+1)
		if err != nil {
			return "", err
		}
		if op.Each != "" {
			if len(p) > 0 {
				return "", fmt.Errorf("op %s: optional inputs in a 每项 op are not supported", op.Name)
			}
			blockVars[i] = v
			ops = append(ops, lit)
			continue
		}
		vars, pre, ops = append(vars, v...), append(pre, p...), append(ops, lit)
	}
	cg.item = nil

	var res []string
	for i, o := range cmd.Outputs {
		f := &out.Fields[i]
		n := idx[o.Op]
		if o.Created {
			res = append(res, fmt.Sprintf("%s: created%d,", goCamelCase(f.Name), n))
			continue
		}
		goType, _ := goOfSQL(o.Type)
		goType = ptr(goType, !o.NotNull)
		if f.Kind == "enum" { // 枚举的映射按来源的表.列
			m, err := g.compositeEnum(o.Table, o.Column)
			if err != nil {
				return "", err
			}
			g.enums[f.Name] = m
		}
		e, err := g.resExpr(fmt.Sprintf("r%d.%s", n, goCamelCase(o.Column)), goType, f)
		if err != nil {
			return "", fmt.Errorf("output %s: %w", f.Name, err)
		}
		res = append(res, fmt.Sprintf("%s: %s,", goCamelCase(f.Name), e))
	}

	var b strings.Builder
	fmt.Fprintf(&b, "// %s %s\nfunc (h *Handler) %s(ctx context.Context, req *connect.Request[%s]) (*connect.Response[%s], error) {\n\tm := req.Msg\n",
		g.api.Method, g.api.Summary, g.api.Method, g.pbType(in), g.pbType(out))
	for _, v := range vars {
		fmt.Fprintf(&b, "\t%s\n", v)
	}
	for _, p := range pre {
		fmt.Fprintf(&b, "\t%s\n", p)
	}
	if hasEach { // 每项(R5): 一个事务里按顺序执行,连续的同一个 每项 的操作 = 一个循环
		g.use("github.com/jackc/pgx/v5", "")
		var body strings.Builder
		for i := 0; i < len(cmd.Ops); {
			op := cmd.Ops[i]
			if op.Each == "" {
				fmt.Fprintf(&body, "\t\tif err := txn.RunIn(ctx, tx,\n%s\n\t\t); err != nil {\n\t\t\treturn err\n\t\t}\n", ops[i])
				i++
				continue
			}
			j := i
			for j < len(cmd.Ops) && cmd.Ops[j].Each == op.Each {
				j++
			}
			lf := in.Field(op.Each)
			items := fmt.Sprintf("items%d", i+1)
			if len(op.EachKey) > 0 {
				g.use("slices", "")
				g.use("cmp", "")
				var keys []string
				for _, k := range op.EachKey {
					keys = append(keys, fmt.Sprintf("cmp.Compare(a.Get%s(), b.Get%s())", goCamelCase(k), goCamelCase(k)))
				}
				cmpExpr := keys[0]
				if len(keys) > 1 {
					cmpExpr = "cmp.Or(" + strings.Join(keys, ", ") + ")"
				}
				fmt.Fprintf(&body, "\t\t%s := slices.Clone(m.Get%s())\n\t\tslices.SortStableFunc(%s, func(a, b *%s) int { return %s }) // 按元素的唯一键的顺序执行\n",
					items, goCamelCase(op.Each), items, g.pbType(lf.Message), cmpExpr)
			} else {
				fmt.Fprintf(&body, "\t\t%s := m.Get%s()\n", items, goCamelCase(op.Each))
			}
			fmt.Fprintf(&body, "\t\t// 每项 输入.%s\n\t\tfor _, it := range %s {\n", op.Each, items)
			for k := i; k < j; k++ {
				for _, v := range blockVars[k] {
					fmt.Fprintf(&body, "\t\t\t%s\n", v)
				}
				fmt.Fprintf(&body, "\t\t\tif err := txn.RunIn(ctx, tx,\n%s\n\t\t\t); err != nil {\n\t\t\t\treturn err\n\t\t\t}\n", ops[k])
			}
			body.WriteString("\t\t}\n")
			i = j
		}
		emit, err := cg.emits(cmd.Emits, "\t\t")
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "\tif err := h.tx.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {\n%s%s\t}); err != nil {\n\t\treturn nil, writeError(err)\n\t}\n", body.String(), emit)
	} else if len(cmd.Emits) > 0 { // 发出(R4): 写操作和 outbox 同一个事务(message.go)
		emit, err := cg.emits(cmd.Emits, "\t\t")
		if err != nil {
			return "", err
		}
		g.use("github.com/jackc/pgx/v5", "")
		fmt.Fprintf(&b, "\tif err := h.tx.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {\n\t\tif err := txn.RunIn(ctx, tx,\n%s\n\t\t); err != nil {\n\t\t\treturn err\n\t\t}\n%s\t}); err != nil {\n\t\treturn nil, writeError(err)\n\t}\n",
			strings.Join(ops, "\n"), emit)
	} else {
		fmt.Fprintf(&b, "\tif err := h.tx.Run(ctx,\n%s\n\t); err != nil {\n\t\treturn nil, writeError(err)\n\t}\n", strings.Join(ops, "\n"))
	}
	fmt.Fprintf(&b, "\treturn connect.NewResponse(&%s{\n%s\n\t}), nil\n}\n", g.pbType(out), strings.Join(res, "\n"))
	return g.header(b.String()), nil
}

// compositeEnum: 表.列 的枚举映射(组合命令写多张表: 不按 API 的表查;newGenCtx 按 表.列 登记)。
func (g *genCtx) compositeEnum(table, column string) (*enumMapping, error) {
	if m, ok := g.enums[table+"."+column]; ok {
		return m, nil
	}
	return nil, fmt.Errorf("command: no enum mapping for %s.%s", table, column)
}

// constLit: 常量 → Go 的字面值(按值的 SQL 类型)。
func constLit(x *ir.CmdExpr) (string, error) {
	switch x.Type {
	case "int2", "int4":
		return "int32(" + x.Value + ")", nil
	case "int8":
		return "int64(" + x.Value + ")", nil
	case "numeric", "text":
		return strconv.Quote(x.Value), nil
	case "bool":
		return x.Value, nil
	}
	return "", fmt.Errorf("command: a constant of type %s is not supported", x.Type)
}
