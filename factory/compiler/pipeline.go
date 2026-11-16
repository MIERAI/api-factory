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
	"int4": core.Int32, "integer": core.Int32, "int8": core.Int64, "bigint": core.Int64,
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
	scan := core.Step{Kind: "scan", Table: table, From: "datasource.table"}
	for _, c := range cat.tables[table] {
		t, err := coreType(table, c)
		if err != nil {
			return nil, err
		}
		scan.Columns = append(scan.Columns, core.NamedType{Name: c.Name, Type: t})
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
		e, err := lib.Filter(cell, f.Column.Name, f.Name)
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
	p.Steps = append([]core.Step{scan}, steps...)
	if _, err := core.Check(p); err != nil {
		return nil, fmt.Errorf("设计规则检查:\n%w", err)
	}
	return p, nil
}
