package design

// 发出 / 结果 / 可重入的检查(R4)。表达式和写入的值同一套(factory/command),这里加类型、规范化写进 spec。

import (
	"fmt"
	"strings"

	"apifactory/factory/command"
	"apifactory/factory/kinds"
	"apifactory/factory/spec"
)

// inputSQL: 输入的值按哪种 SQL 类型写进数据库(枚举 = "enum")。
func (c *ctx) inputSQL(in *Input) (string, error) {
	if c.d.Enum(in.Type) != nil {
		return "enum", nil
	}
	return sqlOfType(in.Type, in.Name)
}

func sqlOfType(typ, name string) (string, error) {
	k, ok := kinds.ByName(typ)
	if !ok {
		return "", fmt.Errorf("%s: 类型 %q 不能用于写操作", name, typ)
	}
	for k.Base != "" && len(k.SQL) == 0 {
		b, ok := kinds.ByName(k.Base)
		if !ok {
			break
		}
		k = b
	}
	if len(k.SQL) == 0 {
		return "", fmt.Errorf("%s: 类型 %q 不能用于写操作", name, typ)
	}
	return k.SQL[0], nil
}

// refEnum: 操作名.列 是枚举的列 → 表(和枚举的值名比较)。
func (c *ctx) refEnum(tables map[string]string) func(op, col string) (string, bool) {
	return func(op, col string) (string, bool) {
		t := tables[op]
		if tt := c.d.Tables[t]; tt != nil && tt.ColEnum[col] != "" {
			return t, true
		}
		return "", false
	}
}

// messaging: 组合命令的 发出、消息处理器的 结果(当 / 发出)、处理器的可重入(E8)。
func (c *ctx) messaging(a *API, inputs map[string]*Input, inputType, itemType func(string) (string, error), tables map[string]string, e *errs) {
	cmd := a.Pattern.Command
	handler := a.Kind == "message"
	ref := func(op, col string) (string, error) {
		rt, ok := tables[op]
		if !ok {
			return "", fmt.Errorf("%s.%s: %s 不是操作", op, col, op)
		}
		cc, ok := c.column(rt, col)
		if !ok {
			return "", fmt.Errorf("%s.%s: 表 %s 没有列 %s", op, col, rt, col)
		}
		return cc.Type, nil
	}
	base := command.Scope{Input: inputType, Ref: ref, RefEnum: c.refEnum(tables), Env: handler,
		Enum: func(table, col, name string) (string, bool) {
			tt := c.d.Tables[table]
			if tt == nil || tt.ColEnum[col] == "" {
				return "", false
			}
			for _, v := range c.d.Enum(tt.ColEnum[col]).Values {
				if v.Name == name {
					return fmt.Sprint(v.DB), true
				}
			}
			return "", false
		},
		EnumOf: func(table, col string) bool { tt := c.d.Tables[table]; return tt != nil && tt.ColEnum[col] != "" }}
	if !handler {
		if len(cmd.Results) > 0 || cmd.Receive != nil || cmd.Each != "" {
			e.add(1, "组合命令: 收到 / 每项 / 结果 只能用于 消息处理")
		}
		c.emits(a, cmd.Emit, base, inputs, "发出", e)
		return
	}
	if len(a.Outputs) > 0 {
		e.add(a.Outputs[0].Line, "消息处理: 没有 输出(处理器不回答请求;结果写在 结果 的 发出 里)")
	}
	// 结果的 当: 操作全部做完之后判定(引用操作的返回值;每项 时写 任一项)
	for i := range cmd.Results {
		r := &cmd.Results[i]
		where := "结果 " + r.Name
		if r.When != "" {
			tree, err := command.ParseExpr(r.When, true)
			if err == nil {
				ws := base
				if r.Any { // 任一项: 每一项判定一次,可以用 项.x(例: 记录的金额 <> 这一项的金额)
					ws.Item = itemType
				}
				err = command.Resolve(tree, ws, "", "", "")
			}
			if err != nil {
				e.add(1, "%s 的 当: %v", where, err)
			} else {
				if cmd.Each != "" && !r.Any && len(command.Refs(tree)) > 0 {
					e.add(1, "%s 的 当: 每项 的操作的值要写 任一项 %s(任一项满足)", where, r.When)
				}
				r.When = command.Format(tree)
			}
		}
		sc := base
		if cmd.Each != "" { // 每项: 发出的值不能引用逐项的操作(哪一项?)
			sc.Ref = func(op, col string) (string, error) {
				return "", fmt.Errorf("%s.%s: 每项 的操作的值不能写进发出的消息(写 消息.列表 原样发出,或者 消息 的字段)", op, col)
			}
		}
		c.emits(a, r.Emit, sc, inputs, where+" 的 发出", e)
	}
	// 可重入(E8): 同样内容、新 id 的消息再来一次,效果不重复
	reentrant := map[string]bool{}
	for _, op := range cmd.Ops {
		ok := false
		why := ""
		switch {
		case op.Only != "":
			r, _, _ := strings.Cut(op.Only, ".")
			ok, why = reentrant[r], "仅当 的操作 "+r+" 不可重入"
		case op.Kind == "insert_idempotent":
			ok = true
		case op.Kind == "update" && op.Cond != "":
			ok, why = selfFalsifying(op), "条件不会被自己的效果变成假(例: 条件 status = HELD,设置 status: COMMITTED)"
		default:
			why = "新建 / 删除 / 没有条件的更新再执行一次会重复生效"
		}
		if !ok {
			e.add(1, "操作 %s: 消息处理器的操作必须可重入(同样内容、新 id 的消息会再来;docs/design/r4-events-messages.md §4.4 E8): %s。"+
				"可以用: 幂等新建 / 条件会被自己变成假的更新 / 仅当 前一个可重入的操作.新建(.已更新)", op.Name, why)
		}
		reentrant[op.Name] = ok
	}
}

// selfFalsifying: 条件里有 列 = 常量,而 设置 把这一列改成别的常量(执行一次之后条件就不成立)。
func selfFalsifying(op spec.CommandOp) bool {
	tree, err := command.ParseExpr(op.Cond, true)
	if err != nil {
		return false
	}
	var cmps []*struct{ col, val string }
	parts := []string{command.Format(tree)}
	if tree.Op == "and" {
		parts = nil
		for _, a := range tree.Args {
			parts = append(parts, command.Format(a))
		}
	}
	for _, p := range parts {
		col, val, ok := strings.Cut(p, " = ")
		if ok && !strings.ContainsAny(col, ". +-") && !strings.ContainsAny(val, " .") {
			cmps = append(cmps, &struct{ col, val string }{col, val})
		}
	}
	for _, cv := range cmps {
		for _, v := range op.Values {
			if v.Column == cv.col && !strings.ContainsAny(v.Expr, " .") && v.Expr != cv.val {
				return true
			}
		}
	}
	return false
}

// emits: 发出的消息: data 的字段 ← 表达式(类型 = 契约的字段;列表 = 原样传 消息 / 输入 的同样形状的列表)。
func (c *ctx) emits(a *API, list []spec.Emit, sc command.Scope, inputs map[string]*Input, where string, e *errs) {
	for i := range list {
		em := &list[i]
		ct := c.emitContract(a, em.Type, e)
		if ct == nil {
			continue
		}
		fields := map[string]spec.ContractField{}
		for _, f := range ct.Fields {
			fields[f.Name] = f
		}
		seen := map[string]bool{}
		for j := range em.Values {
			v := &em.Values[j]
			f, ok := fields[v.Column]
			if !ok {
				e.add(1, "%s %s: 契约没有字段 %s", where, em.Type, v.Column)
				continue
			}
			seen[v.Column] = true
			if name, ok := strings.CutPrefix(v.Expr, LoadPrefix); ok && f.Type == ListType && c.loadList != nil { // 状态机: 载入的子行(R5)
				if err := c.loadList(name, f.Elem); err != nil {
					e.add(1, "%s %s.%s: %v", where, em.Type, v.Column, err)
				}
				continue
			}
			if f.Type == ListType {
				name := strings.TrimPrefix(strings.TrimPrefix(v.Expr, command.InputPrefix), command.MessagePrefix)
				in := inputs[name]
				if in == nil || in.Type != GroupListType || !sameElems(in.Group, f.Elem) {
					e.add(1, "%s %s.%s: 列表字段要原样传一个列表(消息.<列表> / 输入.<列表>,有契约的元素的每个字段: 名字和类型相同)", where, em.Type, v.Column)
					continue
				}
				if len(f.Unique) > 0 && !sameSet(f.Unique, in.Unique) && a.Kind != "message" {
					e.add(1, "%s %s.%s: 契约的列表要求 唯一 %v: 输入 %s 也要写 唯一: %v(不然重复的元素会让对方当成毒消息)", where, em.Type, v.Column, f.Unique, name, f.Unique)
				}
				v.Expr = command.InputPrefix + name
				continue
			}
			want, err := sqlOfType(f.Type, v.Column)
			if err != nil {
				e.add(1, "%s %s: %v", where, em.Type, err)
				continue
			}
			tree, err := command.ParseExpr(v.Expr, false)
			if err == nil {
				err = command.Resolve(tree, sc, want, "", "")
			}
			if err != nil {
				e.add(1, "%s %s.%s = %s: %v", where, em.Type, v.Column, v.Expr, err)
				continue
			}
			if tree.Op == "input" { // 可以不传的输入写进必填的字段: 没传 = 零值,对方会当成"有值"
				if in := inputs[tree.Name]; in != nil && !in.Required && f.Required {
					e.add(1, "%s %s.%s: 输入 %s 可以不传,不能写进必填的字段", where, em.Type, v.Column, tree.Name)
				}
			}
			v.Expr = command.Format(tree)
		}
		for _, f := range ct.Fields {
			if f.Required && !seen[f.Name] {
				e.add(1, "%s %s: 缺少必填的字段 %s", where, em.Type, f.Name)
			}
		}
	}
}

// sameElems: 契约的元素的每个字段,输入的组里都有(名字和类型相同;组可以多几个字段,发出时只写契约的字段)。
func sameElems(in []*Input, fs []spec.ContractField) bool {
	for _, f := range fs {
		found := false
		for _, g := range in {
			found = found || (g.Name == f.Name && g.Type == f.Type)
		}
		if !found {
			return false
		}
	}
	return len(fs) > 0
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for _, x := range a {
		if !contains(b, x) {
			return false
		}
	}
	return true
}
