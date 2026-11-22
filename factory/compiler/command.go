package compiler

// 命令(命令工厂,R3-1): 没有 SQL 文件、没有流水线。IR 里记下写哪张表的哪些列(带列的类型),
// 适配器生成"写操作作为数据"的 handler,运行时的事务执行器组装 SQL(api-factory-runtime/txn)。

import (
	"encoding/json"
	"fmt"
	"strings"

	"google.golang.org/protobuf/reflect/protoreflect"

	"apifactory/factory/command"
	"apifactory/factory/ir"
	"apifactory/factory/kinds"
	"apifactory/factory/spec"
)

// commandIR: spec 的 command → IR(列的类型来自 schema;响应的字段 = 写之后的行的列)。
func commandIR(s *spec.Spec, api *ir.API, cat *catalog, md protoreflect.MethodDescriptor) error {
	if api.Command.Op == "command" || api.Command.Op == "message" { // 组合命令(R3-2)/ 消息处理器(R4)
		if err := compositeIR(s, api, cat); err != nil {
			return err
		}
		return messagingIR(s, api, cat, md)
	}
	cmd, c := api.Command, s.Pattern.Command
	table := s.Datasource.Table
	if _, ok := cat.tables[table]; !ok {
		return fmt.Errorf("datasource.table: %q is not in db/schema/*.sql", table)
	}
	if len(s.Datasource.Rename) > 0 {
		return fmt.Errorf("pattern %s: 输出不能改名(响应的字段 = 写之后的行的列)", api.Pattern)
	}
	bind := func(b spec.Binding) (ir.CommandCol, error) {
		col, ok := cat.column(table, b.Column)
		if !ok {
			return ir.CommandCol{}, fmt.Errorf("%s: column %s is not in table %s", b.Field, b.Column, table)
		}
		if api.Request.Field(b.Field) == nil {
			return ir.CommandCol{}, fmt.Errorf("%s is not a request field", b.Field)
		}
		return ir.CommandCol{Field: b.Field, Column: b.Column, Type: col.Type, NotNull: col.NotNull}, nil
	}
	for _, b := range c.Key {
		x, err := bind(b)
		if err != nil {
			return fmt.Errorf("command.key: %w", err)
		}
		cmd.Key = append(cmd.Key, x)
	}
	for _, b := range c.Write {
		x, err := bind(b)
		if err != nil {
			return fmt.Errorf("command.write: %w", err)
		}
		cmd.Write = append(cmd.Write, x)
	}
	if c.Lock != nil {
		x, err := bind(*c.Lock)
		if err != nil {
			return fmt.Errorf("command.lock: %w", err)
		}
		if x.Type != "int4" && x.Type != "int8" {
			return fmt.Errorf("command.lock: column %s is %s, want an integer", x.Column, x.Type)
		}
		cmd.Lock = &x
	}
	for _, x := range c.Set {
		col, ok := cat.column(table, x.Column)
		if !ok {
			return fmt.Errorf("command.set: column %s is not in table %s", x.Column, table)
		}
		set := ir.CommandSet{Column: x.Column, Type: col.Type}
		if x.Value == spec.NowValue {
			if col.Type != "timestamptz" {
				return fmt.Errorf("command.set.%s: now() needs a timestamptz column (got %s)", x.Column, col.Type)
			}
			set.Now = true
		} else {
			set.Value = x.Value
		}
		cmd.Set = append(cmd.Set, set)
	}
	for _, f := range api.Response.Fields { // 响应的字段 = 写之后的行的列(同名)
		col, ok := cat.column(table, f.Name)
		if !ok {
			return fmt.Errorf("response field %s: no column %s in table %s (a command returns the written row)", f.Name, f.Name, table)
		}
		cmd.Returning = append(cmd.Returning, ir.Column{Name: col.Name, Type: col.Type, NotNull: col.NotNull})
	}
	return nil
}

// compositeIR: 组合命令 → IR。表达式按 spec 的规范写法重新解析、加类型(factory/command 的 Resolve:
// 和设计编译器同一套规则);每个操作 RETURNING 后面的操作 / 输出要用的列。
func compositeIR(s *spec.Spec, api *ir.API, cat *catalog) error {
	c := s.Pattern.Command
	cmd := api.Command
	tables := map[string]string{}
	enumOf := func(table, col string) bool {
		if s.Domain == nil {
			return false
		}
		return s.Domain.Entities[table].Columns[col].Enum != nil
	}
	input := func(name string) (string, error) {
		f := api.Request.Field(name)
		if f == nil {
			return "", fmt.Errorf("输入.%s is not a request field", name)
		}
		return InputSQLType(f)
	}
	for _, op := range c.Ops {
		if _, ok := cat.tables[op.Table]; !ok {
			return fmt.Errorf("op %s: table %q is not in db/schema/*.sql", op.Name, op.Table)
		}
		table := op.Table
		colType := func(col string) (string, bool) {
			x, ok := cat.column(table, col)
			return x.Type, ok
		}
		sc := command.Scope{
			Table: table,
			Ref: func(ref, col string) (string, error) {
				rt, ok := tables[ref]
				if !ok {
					return "", fmt.Errorf("%s is not an earlier op", ref)
				}
				x, ok := cat.column(rt, col)
				if !ok {
					return "", fmt.Errorf("%s.%s: no column %s in %s", ref, col, col, rt)
				}
				return x.Type, nil
			},
			Input:   input,
			EnumOf:  enumOf,
			Item:    itemScope(api, c.Each),
			RefEnum: refEnumScope(s, tables),
			Env:     api.Command.Op == "message",
		}
		val := func(x spec.ColExpr, rowCols bool) (ir.CommandVal, error) {
			col, ok := cat.column(table, x.Column)
			if !ok {
				return ir.CommandVal{}, fmt.Errorf("op %s: column %s is not in table %s", op.Name, x.Column, table)
			}
			tree, err := command.ParseExpr(x.Expr, false)
			if err != nil {
				return ir.CommandVal{}, fmt.Errorf("op %s: %s: %w", op.Name, x.Column, err)
			}
			want, wt, wc := col.Type, "", ""
			if enumOf(table, x.Column) {
				want, wt, wc = "enum:"+table+"."+x.Column, table, x.Column
			}
			s2 := sc
			if rowCols {
				s2.Column = colType
			}
			if err := command.Resolve(tree, s2, want, wt, wc); err != nil {
				return ir.CommandVal{}, fmt.Errorf("op %s: %s = %s: %w", op.Name, x.Column, x.Expr, err)
			}
			return ir.CommandVal{Column: x.Column, Type: col.Type, NotNull: col.NotNull, Expr: tree}, nil
		}
		o := ir.CommandOp{Name: op.Name, Kind: op.Kind, Table: table, Conflict: op.Conflict, Unmet: op.Unmet, NotFound: op.NotFound, Skip: op.Skip}
		if op.Only != "" {
			o.Only, o.OnlyFlag, _ = strings.Cut(op.Only, ".")
		}
		for _, k := range op.Key {
			v, err := val(k, false)
			if err != nil {
				return err
			}
			o.Key = append(o.Key, v)
		}
		for _, x := range op.Values {
			v, err := val(x, op.Kind == "update")
			if err != nil {
				return err
			}
			o.Values = append(o.Values, v)
		}
		if op.Lock != nil {
			v, err := val(*op.Lock, false)
			if err != nil {
				return err
			}
			if v.Expr.Op != "input" {
				return fmt.Errorf("op %s: lock: want an input", op.Name)
			}
			o.Lock = &v
		}
		if op.Cond != "" {
			tree, err := command.ParseExpr(op.Cond, true)
			if err != nil {
				return fmt.Errorf("op %s: cond: %w", op.Name, err)
			}
			s2 := sc
			s2.Column = colType
			if err := command.Resolve(tree, s2, "", "", ""); err != nil {
				return fmt.Errorf("op %s: cond %s: %w", op.Name, op.Cond, err)
			}
			o.Cond = tree
		}
		tables[op.Name] = table
		cmd.Ops = append(cmd.Ops, o)
	}
	// RETURNING: 后面的操作引用的列 + 输出的列(表的列的顺序)
	need := map[string]map[string]bool{}
	mark := func(op, col string) {
		if need[op] == nil {
			need[op] = map[string]bool{}
		}
		need[op][col] = true
	}
	for _, o := range cmd.Ops {
		var exprs []*ir.CmdExpr
		for _, v := range append(append([]ir.CommandVal{}, o.Key...), o.Values...) {
			exprs = append(exprs, v.Expr)
		}
		exprs = append(exprs, o.Cond)
		for _, x := range exprs {
			for _, r := range command.Refs(x) {
				mark(r[0], r[1])
			}
		}
	}
	for _, x := range append(messagingExprs(c, api), nil) { // 结果的 当 / 发出 引用的列(messagingIR 再解析一遍,这里只要列名)
		if x == nil {
			continue
		}
		for _, r := range command.Refs(x) {
			mark(r[0], r[1])
		}
	}
	if len(c.Output) != len(api.Response.Fields) {
		return fmt.Errorf("composite command: %d outputs but %d response fields", len(c.Output), len(api.Response.Fields))
	}
	for i, out := range c.Output {
		if api.Response.Fields[i].Name != out.Field {
			return fmt.Errorf("composite command: output %d is %s, response field is %s", i, out.Field, api.Response.Fields[i].Name)
		}
		t, ok := tables[out.Op]
		if !ok {
			return fmt.Errorf("output %s: op %s does not exist", out.Field, out.Op)
		}
		co := ir.CommandOut{Field: out.Field, Op: out.Op, Table: t, Column: out.Column, Created: out.Created}
		if out.Created {
			for j := range cmd.Ops {
				if cmd.Ops[j].Name == out.Op {
					cmd.Ops[j].Created = true
				}
			}
		} else {
			col, ok := cat.column(t, out.Column)
			if !ok {
				return fmt.Errorf("output %s: no column %s in %s", out.Field, out.Column, t)
			}
			co.Type, co.NotNull = col.Type, col.NotNull
			mark(out.Op, out.Column)
		}
		cmd.Outputs = append(cmd.Outputs, co)
	}
	for j := range cmd.Ops {
		o := &cmd.Ops[j]
		for _, col := range cat.tables[o.Table] {
			if need[o.Name][col.Name] {
				o.Returning = append(o.Returning, ir.Column{Name: col.Name, Type: col.Type, NotNull: col.NotNull})
			}
		}
	}
	return nil
}

// InputSQLType: 请求字段的值按哪种 SQL 类型写进数据库(带格式的文本: 十进制 → numeric、日期 → date;枚举 = "enum")。
func InputSQLType(f *ir.Field) (string, error) {
	if f.List {
		return "", fmt.Errorf("%s: a list input cannot be written", f.Name)
	}
	switch f.Kind {
	case "int32":
		return "int4", nil
	case "int64":
		return "int8", nil
	case "bool":
		return "bool", nil
	case "timestamp":
		return "timestamptz", nil
	case "enum":
		return "enum", nil
	case "string":
		if f.Rules != nil {
			var r struct {
				String struct {
					Pattern string `json:"pattern"`
				} `json:"string"`
			}
			if json.Unmarshal(f.Rules.Raw, &r) == nil && r.String.Pattern != "" {
				if k, ok := kinds.ByPattern(r.String.Pattern); ok {
					for k.Base != "" && len(k.SQL) == 0 {
						b, ok := kinds.ByName(k.Base)
						if !ok {
							break
						}
						k = b
					}
					if len(k.SQL) > 0 {
						return k.SQL[0], nil
					}
				}
			}
		}
		return "text", nil
	}
	return "", fmt.Errorf("%s: kind %s cannot be written", f.Name, f.Kind)
}
