package compiler

// R4 的 IR: 组合命令的 发出、消息处理器的 收到 / 每项 / 仅当 / 结果(docs/design/r4-events-messages.md)。
// 表达式按 spec 的规范写法重新解析、加类型(和设计编译器同一套规则,factory/command 的 Resolve)。

import (
	"fmt"

	"google.golang.org/protobuf/reflect/protoreflect"

	"apifactory/factory/command"
	"apifactory/factory/ir"
	"apifactory/factory/kinds"
	"apifactory/factory/spec"
)

// itemScope: 每项 的元素的字段(项.x)的 SQL 类型。
func itemScope(api *ir.API, each string) func(string) (string, error) {
	if each == "" {
		return nil
	}
	return func(name string) (string, error) {
		lf := api.Request.Field(each)
		if lf == nil || lf.Message == nil {
			return "", fmt.Errorf("每项 %s is not a list of messages", each)
		}
		f := lf.Message.Field(name)
		if f == nil {
			return "", fmt.Errorf("项.%s: no field %s in %s", name, name, lf.Message.Name)
		}
		return InputSQLType(f)
	}
}

// refEnumScope: 操作名.列 是枚举的列 → 表。
func refEnumScope(s *spec.Spec, tables map[string]string) func(op, col string) (string, bool) {
	return func(op, col string) (string, bool) {
		t, ok := tables[op]
		if !ok || s.Domain == nil {
			return "", false
		}
		return t, s.Domain.Entities[t].Columns[col].Enum != nil
	}
}

// messagingExprs: 结果的 当 / 发出 的表达式(只解析,不加类型: RETURNING 要的列)。
func messagingExprs(c *spec.Command, api *ir.API) []*ir.CmdExpr {
	var out []*ir.CmdExpr
	add := func(src string, cond bool) {
		if x, err := command.ParseExpr(src, cond); err == nil {
			out = append(out, x)
		}
	}
	emits := append([]spec.Emit{}, c.Emit...)
	for _, r := range c.Results {
		if r.When != "" {
			add(r.When, true)
		}
		emits = append(emits, r.Emit...)
	}
	for _, e := range emits {
		for _, v := range e.Values {
			add(v.Expr, false)
		}
	}
	return out
}

// messagingIR: 发出 / 收到 / 每项 / 仅当 / 结果 → IR。
func messagingIR(s *spec.Spec, api *ir.API, cat *catalog, md protoreflect.MethodDescriptor) error {
	c, cmd := s.Pattern.Command, api.Command
	tables := map[string]string{}
	opIdx := map[string]int{}
	for i, op := range cmd.Ops {
		tables[op.Name] = op.Table
		opIdx[op.Name] = i
	}
	contracts := map[string]spec.Contract{}
	for _, ct := range c.Contracts {
		contracts[ct.Type] = ct
		m := md.ParentFile().Messages().ByName(protoreflect.Name(ct.Proto))
		if m == nil {
			return fmt.Errorf("contract %s: proto message %s not found", ct.Type, ct.Proto)
		}
		ic := ir.Contract{Type: ct.Type, Kind: ct.Kind, Version: ct.Version, Subject: ct.Subject, Message: convMessage(m, 0)}
		if c.Receive != nil && ct.Type == c.Receive.Type && ct.Proto == string(md.Input().Name()) {
			cp := ic
			cmd.Receive = &cp
			continue
		}
		cmd.Contracts = append(cmd.Contracts, ic)
	}
	if api.Command.Op == "message" {
		if cmd.Receive == nil {
			return fmt.Errorf("message handler: the received contract %v is missing", c.Receive)
		}
		cmd.Each = c.Each
		if c.Each != "" {
			for _, f := range contracts[c.Receive.Type].Fields {
				if f.Name == c.Each {
					cmd.EachKey = f.Unique
				}
			}
			if len(cmd.EachKey) == 0 {
				return fmt.Errorf("每项 %s: the list has no unique key", c.Each)
			}
		}
	}
	for i := range cmd.Ops { // 仅当: 前面的操作要记下 新建 / 已更新
		o := &cmd.Ops[i]
		if o.Only == "" {
			continue
		}
		j, ok := opIdx[o.Only]
		if !ok || j >= i {
			return fmt.Errorf("op %s: only %s.%s: not an earlier op", o.Name, o.Only, o.OnlyFlag)
		}
		switch o.OnlyFlag {
		case "新建":
			cmd.Ops[j].Created = true
		case "已更新":
			cmd.Ops[j].Updated = true
		default:
			return fmt.Errorf("op %s: only flag %q", o.Name, o.OnlyFlag)
		}
	}
	enumOf := func(table, col string) bool {
		return s.Domain != nil && s.Domain.Entities[table].Columns[col].Enum != nil
	}
	sc := command.Scope{
		Ref: func(ref, col string) (string, error) {
			rt, ok := tables[ref]
			if !ok {
				return "", fmt.Errorf("%s is not an op", ref)
			}
			x, ok := cat.column(rt, col)
			if !ok {
				return "", fmt.Errorf("%s.%s: no column %s in %s", ref, col, col, rt)
			}
			return x.Type, nil
		},
		Input: func(name string) (string, error) {
			f := api.Request.Field(name)
			if f == nil {
				return "", fmt.Errorf("输入.%s is not a request field", name)
			}
			return InputSQLType(f)
		},
		EnumOf:  enumOf,
		RefEnum: refEnumScope(s, tables),
		Env:     api.Command.Op == "message",
	}
	emits := func(list []spec.Emit) ([]ir.CommandEmit, error) {
		var out []ir.CommandEmit
		for _, e := range list {
			ct, ok := contracts[e.Type]
			if !ok {
				return nil, fmt.Errorf("emit %s: no contract", e.Type)
			}
			em := ir.CommandEmit{Type: e.Type}
			for _, v := range e.Values {
				var cf *spec.ContractField
				for i := range ct.Fields {
					if ct.Fields[i].Name == v.Column {
						cf = &ct.Fields[i]
					}
				}
				if cf == nil {
					return nil, fmt.Errorf("emit %s: no field %s", e.Type, v.Column)
				}
				tree, err := command.ParseExpr(v.Expr, false)
				if err != nil {
					return nil, fmt.Errorf("emit %s.%s: %w", e.Type, v.Column, err)
				}
				if cf.Elem != nil { // 列表: 原样传请求的列表
					if tree.Op != "input" || api.Request.Field(tree.Name) == nil || !api.Request.Field(tree.Name).List {
						return nil, fmt.Errorf("emit %s.%s: a list field takes a request list as is", e.Type, v.Column)
					}
					em.Values = append(em.Values, ir.EmitVal{Field: v.Column, List: true, Expr: tree})
					continue
				}
				want, err := sqlOf(cf.Type)
				if err != nil {
					return nil, fmt.Errorf("emit %s.%s: %w", e.Type, v.Column, err)
				}
				if err := command.Resolve(tree, sc, want, "", ""); err != nil {
					return nil, fmt.Errorf("emit %s.%s = %s: %w", e.Type, v.Column, v.Expr, err)
				}
				em.Values = append(em.Values, ir.EmitVal{Field: v.Column, Type: want, Expr: tree})
			}
			out = append(out, em)
		}
		return out, nil
	}
	var err error
	if cmd.Emits, err = emits(c.Emit); err != nil {
		return err
	}
	for _, r := range c.Results {
		res := ir.CommandResult{Name: r.Name, Any: r.Any, DLQ: r.DLQ}
		if r.Name == spec.SuccessResult {
			res.Name = ""
		}
		if r.When != "" {
			tree, err := command.ParseExpr(r.When, true)
			if err != nil {
				return fmt.Errorf("result %s: when: %w", r.Name, err)
			}
			if err := command.Resolve(tree, sc, "", "", ""); err != nil {
				return fmt.Errorf("result %s: when %s: %w", r.Name, r.When, err)
			}
			res.When = tree
		}
		if res.Emits, err = emits(r.Emit); err != nil {
			return err
		}
		cmd.Results = append(cmd.Results, res)
	}
	return nil
}

// sqlOf: 设计语言的类型名 → SQL 类型。
func sqlOf(typ string) (string, error) {
	k, ok := kinds.ByName(typ)
	for ok && k.Base != "" && len(k.SQL) == 0 {
		k, ok = kinds.ByName(k.Base)
	}
	if !ok || len(k.SQL) == 0 {
		return "", fmt.Errorf("type %q", typ)
	}
	return k.SQL[0], nil
}
