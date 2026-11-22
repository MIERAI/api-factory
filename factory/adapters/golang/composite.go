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

func genComposite(g *genCtx) (string, error) {
	cmd := g.api.Command
	in, out := &g.api.Request, &g.api.Response
	g.use(runtimeModule+"/txn", "")
	idx := map[string]int{} // 操作名 → 第几个(变量 r1, r2 …)
	for i, op := range cmd.Ops {
		idx[op.Name] = i + 1
	}

	// value: 叶子的表达式 → Go 的值(参数)
	var value func(x *ir.CmdExpr) (string, error)
	value = func(x *ir.CmdExpr) (string, error) {
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
			i, ok := idx[x.Ref]
			if !ok {
				return "", fmt.Errorf("command: %s is not an op", x.Ref)
			}
			return fmt.Sprintf("txn.Later(func() any { return r%d.%s })", i, goCamelCase(x.Name)), nil
		case "const":
			return constLit(x)
		case "now":
			g.use(runtimeModule+"/clock", "")
			return "clock.Now(ctx)", nil
		}
		return "", fmt.Errorf("command: %s is not a value", x.Op)
	}
	// expr: 表达式 → txn.Expr 的构造
	var expr func(x *ir.CmdExpr) (string, error)
	expr = func(x *ir.CmdExpr) (string, error) {
		switch x.Op {
		case "col":
			return fmt.Sprintf("txn.C(%q)", x.Name), nil
		case "+", "-":
			a, err := expr(x.Args[0])
			if err != nil {
				return "", err
			}
			b, err := expr(x.Args[1])
			if err != nil {
				return "", err
			}
			fn := map[string]string{"+": "Add", "-": "Sub"}[x.Op]
			return fmt.Sprintf("txn.%s(%s, %s)", fn, a, b), nil
		case "=", "<>", "<", "<=", ">", ">=":
			a, err := expr(x.Args[0])
			if err != nil {
				return "", err
			}
			b, err := expr(x.Args[1])
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("txn.Cmp(%q, %s, %s)", x.Op, a, b), nil
		case "and":
			var parts []string
			for _, a := range x.Args {
				p, err := expr(a)
				if err != nil {
					return "", err
				}
				parts = append(parts, p)
			}
			return "txn.And(" + strings.Join(parts, ", ") + ")", nil
		}
		v, err := value(x)
		if err != nil {
			return "", err
		}
		return "txn.V(" + v + ")", nil
	}
	col := func(v ir.CommandVal) (string, error) {
		switch v.Expr.Op {
		case "input", "ref", "const", "now":
			x, err := value(v.Expr)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("{Name: %q, Value: %s},", v.Column, x), nil
		}
		x, err := expr(v.Expr)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("{Name: %q, Expr: %s},", v.Column, x), nil
	}

	var vars, pre, ops []string
	for i, op := range cmd.Ops {
		n := i + 1
		var lines []string
		lines = append(lines, "Kind: "+compositeKinds[op.Kind]+",", fmt.Sprintf("Table: %q,", op.Table))
		if len(op.Values) > 0 || op.Lock != nil {
			var cs, optional []string
			for _, v := range op.Values {
				if f := in.Field(v.Expr.Name); v.Expr.Op == "input" && f != nil && f.Optional && f.Kind != "enum" { // 不传 = 不写这一列
					goType, ok := goOfSQL(v.Expr.Type)
					if !ok {
						return "", fmt.Errorf("op %s: 输入.%s: type %s is not supported", op.Name, f.Name, v.Expr.Type)
					}
					e, err := g.reqExpr(f, "*"+goType)
					if err != nil {
						return "", err
					}
					optional = append(optional, fmt.Sprintf("if v := %s; v != nil { // 不传 = 不写这一列(默认值)\n\t\tvals%d = append(vals%d, txn.Col{Name: %q, Value: *v})\n\t}", e, n, n, v.Column))
					continue
				}
				c, err := col(v)
				if err != nil {
					return "", fmt.Errorf("op %s: %w", op.Name, err)
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
				c, err := col(v)
				if err != nil {
					return "", fmt.Errorf("op %s: key: %w", op.Name, err)
				}
				cs = append(cs, c)
			}
			lines = append(lines, "Key: []txn.Col{\n"+strings.Join(cs, "\n")+"\n},")
		}
		if op.Lock != nil {
			x, err := value(op.Lock.Expr)
			if err != nil {
				return "", err
			}
			lines = append(lines, fmt.Sprintf("Version: &txn.Col{Name: %q, Value: %s},", op.Lock.Column, x))
		}
		if op.Cond != nil {
			x, err := expr(op.Cond)
			if err != nil {
				return "", fmt.Errorf("op %s: cond: %w", op.Name, err)
			}
			lines = append(lines, "Cond: "+x+",", fmt.Sprintf("Unmet: %q,", op.Unmet))
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
					return "", fmt.Errorf("op %s: returning column %s: type %s is not supported", op.Name, c.Name, c.Type)
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
				return "", err
			}
			lines = append(lines, "NotFound: "+msg+",")
		}
		ops = append(ops, fmt.Sprintf("txn.Op{ // %s\n%s\n},", op.Name, strings.Join(lines, "\n")))
	}

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
	fmt.Fprintf(&b, "\tif err := h.tx.Run(ctx,\n%s\n\t); err != nil {\n\t\treturn nil, writeError(err)\n\t}\n", strings.Join(ops, "\n"))
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
