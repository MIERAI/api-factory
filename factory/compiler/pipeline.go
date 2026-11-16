package compiler

// 声明式 datasource → 核心代数的流水线(单元展开)→ SQL(core 的 SQL 后端)。

import (
	"fmt"

	"apifactory/factory/cells"
	"apifactory/factory/core"
	"apifactory/factory/spec"
)

// sqlKinds: schema 的列类型 → 核心类型
var sqlKinds = map[string]core.Kind{
	"int4": core.Int32, "integer": core.Int32, "int8": core.Int64, "bigint": core.Int64, "bigserial": core.Int64, "serial": core.Int32,
	"text": core.Text, "varchar": core.Text, "bpchar": core.Text,
	"bool": core.Bool, "boolean": core.Bool, "numeric": core.Decimal, "date": core.Date,
	"timestamptz": core.Timestamp, "jsonb": core.JSON,
}

func coreType(table string, c catColumn) (core.Type, error) {
	k, ok := sqlKinds[c.Type]
	if !ok {
		return core.Type{}, fmt.Errorf("%s.%s: 列类型 %s 核心代数还不支持", table, c.Name, c.Type)
	}
	return core.Type{Kind: k, Nullable: !c.NotNull}, nil
}

// patternCells: 式样书的模式 → 单元库里的模式(套装)
var patternCells = map[string]string{"get_by_id": "单条", "list_keyset": "分页", "compute": "计算", "list_all": "多条"}

// filterCells: 式样书的筛选运算 → 单元库里的筛选单元
var filterCells = map[string]string{"eq": "等于", "gt": "大于", "gte": "大于等于", "lt": "小于", "lte": "小于等于", "prefix": "前缀匹配"}

func buildPipeline(s *spec.Spec, lib *cells.Library, cols []string, filters []resolvedFilter, cat *catalog) (*core.Pipeline, error) {
	table := s.Datasource.Table
	p := &core.Pipeline{}
	source, err := sourceSteps(s, lib, cat)
	if err != nil {
		return nil, err
	}
	var parts cells.Parts
	for _, f := range filters {
		t, err := coreType(table, f.Column)
		if err != nil {
			return nil, err
		}
		p.Params = append(p.Params, core.ParamDecl{Name: f.Name, Type: core.Type{Kind: t.Kind}, Required: !f.Presence, List: f.List})
		cell, ok := filterCells[f.Op]
		if !ok {
			cell = f.Op // 直接写单元名(属于 / 有效期包含 …)
		}
		cols := f.Columns
		if len(cols) == 0 {
			cols = []string{f.Column.Name}
		}
		e, err := lib.Filter(cell, cols, f.Name)
		if err != nil {
			return nil, fmt.Errorf("筛选 %s: %w", f.Name, err)
		}
		parts.Filters = append(parts.Filters, e)
	}
	for _, c := range cols {
		parts.Output = append(parts.Output, core.NamedExpr{Name: c, Expr: core.Col(c)})
	}
	if s.PatternKind() == "list_keyset" {
		var params []string
		for _, c := range s.Pattern.OrderBy {
			col, ok := cat.column(table, c)
			if !ok {
				return nil, fmt.Errorf("order_by: 列 %s 不在 %s 里", c, table)
			}
			t, err := coreType(table, col)
			if err != nil {
				return nil, err
			}
			p.Params = append(p.Params, core.ParamDecl{Name: "cursor_" + c, Type: core.Type{Kind: t.Kind}})
			params = append(params, "cursor_"+c)
			parts.Order = append(parts.Order, core.SortKey{Col: c, Desc: s.Pattern.Direction != "asc"})
		}
		parts.Cursor = core.TupleAfter(s.Pattern.OrderBy, params, s.Pattern.Direction != "asc")
		parts.Rows = "row_limit"
		p.Params = append(p.Params, core.ParamDecl{Name: "row_limit", Type: core.Type{Kind: core.Int32}, Required: true})
	}
	if s.PatternKind() == "list_all" {
		for _, c := range s.Pattern.OrderBy {
			parts.Order = append(parts.Order, core.SortKey{Col: c, Desc: s.Pattern.Direction == "desc"})
		}
		parts.Limit = int(s.Pattern.MaxRows) + 1
	}
	name, ok := patternCells[s.PatternKind()]
	if !ok {
		return nil, fmt.Errorf("模式 %s 没有对应的套装", s.PatternKind())
	}
	steps, err := lib.Pattern(name, parts)
	if err != nil {
		return nil, err
	}
	p.Steps = append(source, steps...)
	if _, err := core.Check(p); err != nil {
		return nil, fmt.Errorf("设计规则检查:\n%w", err)
	}
	return p, nil
}

func scanStep(table string, cat *catalog) (core.Step, error) {
	scan := core.Step{Kind: "scan", Table: table, From: "datasource.table"}
	for _, c := range cat.tables[table] {
		t, err := coreType(table, c)
		if err != nil {
			return scan, err
		}
		scan.Columns = append(scan.Columns, core.NamedType{Name: c.Name, Type: t})
	}
	return scan, nil
}

// sourceSteps: 流水线的开头。普通的表 = scan;存储: JSON版本 的逻辑表 = 来源单元 JSON版本 展开。
func sourceSteps(s *spec.Spec, lib *cells.Library, cat *catalog) ([]core.Step, error) {
	table := s.Datasource.Table
	var e spec.Entity
	if s.Domain != nil {
		e = s.Domain.Entities[table]
	}
	st := e.Storage
	if st == nil {
		scan, err := scanStep(table, cat)
		return []core.Step{scan}, err
	}
	scan, err := scanStep(st.Table, cat)
	if err != nil {
		return nil, err
	}
	data := core.Col(st.DataColumn)
	var parts cells.SourceParts
	parts.Scan, parts.Keys = scan, e.NaturalKey
	for _, f := range st.Fields {
		k, ok := sqlKinds[f.Type]
		if !ok {
			return nil, fmt.Errorf("存储字段 %s: 类型 %s 核心代数还不支持", f.Name, f.Type)
		}
		parts.Expand = append(parts.Expand, core.NamedExpr{Name: f.Name, Expr: core.JSONField(data, f.JSONKey(), core.Type{Kind: k})})
		parts.Logical = append(parts.Logical, core.NamedExpr{Name: f.Name, Expr: core.Col(f.Name)})
	}
	parts.Expand = append(parts.Expand, core.NamedExpr{Name: "_action", Expr: core.Col(st.ActionColumn)})
	for _, c := range st.OrderBy {
		parts.Expand = append(parts.Expand, core.NamedExpr{Name: "_o_" + c, Expr: core.Col(c)})
		parts.Order = append(parts.Order, core.SortKey{Col: "_o_" + c, Desc: true})
	}
	parts.Names = map[string]*core.Expr{
		"操作":    core.Col("_action"),
		"删除值":   core.Const(st.DeleteValue, core.Type{Kind: core.Int32}),
		"逻辑删除":  core.Const(nil, core.Type{Kind: core.Text}),
		"逻辑删除值": core.Const("", core.Type{Kind: core.Text}),
	}
	if ld := st.LogicalDelete; ld != nil {
		parts.Expand = append(parts.Expand, core.NamedExpr{Name: "_logical_delete", Expr: core.JSONField(data, ld.Key, core.Type{Kind: core.Text})})
		parts.Names["逻辑删除"] = core.Col("_logical_delete")
		parts.Names["逻辑删除值"] = core.Const(ld.Value, core.Type{Kind: core.Text})
	}
	return lib.Source("JSON版本", parts)
}
