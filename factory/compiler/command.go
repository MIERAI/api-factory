package compiler

// 命令(命令工厂,R3-1): 没有 SQL 文件、没有流水线。IR 里记下写哪张表的哪些列(带列的类型),
// 适配器生成"写操作作为数据"的 handler,运行时的事务执行器组装 SQL(api-factory-runtime/txn)。

import (
	"fmt"

	"apifactory/factory/ir"
	"apifactory/factory/spec"
)

// commandIR: spec 的 command → IR(列的类型来自 schema;响应的字段 = 写之后的行的列)。
func commandIR(s *spec.Spec, api *ir.API, cat *catalog) error {
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
