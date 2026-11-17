package compiler

// 声明式 datasource → 核心代数的流水线(单元展开)→ SQL(core 的 SQL 后端)。

import (
	"fmt"

	"apifactory/factory/cells"
	"apifactory/factory/core"
	"apifactory/factory/exits"
	"apifactory/factory/kinds"
	"apifactory/factory/sources"
	"apifactory/factory/spec"
)

func coreType(table string, c catColumn) (core.Type, error) {
	k, ok := sqlKind(c.Type)
	if !ok {
		return core.Type{}, fmt.Errorf("%s.%s: 列类型 %s 核心代数还不支持", table, c.Name, c.Type)
	}
	return core.Type{Kind: k, Nullable: !c.NotNull}, nil
}

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
	ex, ok := exits.ByKind(s.PatternKind()) // 出口的注册表(factory/exits)
	if !ok || ex.Cell == "" {
		return nil, fmt.Errorf("模式 %s 没有对应的套装", s.PatternKind())
	}
	if ex.Parts != nil {
		if err := ex.Parts(s, partsCtx{table, cat}, &parts, p); err != nil {
			return nil, err
		}
	}
	name := ex.Cell
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

// sourceSteps: 流水线的开头,由来源决定(factory/sources: 普通的表 = scan;JSON版本 = 来源单元展开)。
func sourceSteps(s *spec.Spec, lib *cells.Library, cat *catalog) ([]core.Step, error) {
	table := s.Datasource.Table
	var e spec.Entity
	if s.Domain != nil {
		e = s.Domain.Entities[table]
	}
	kind, physical := "table", table
	if e.Storage != nil {
		kind, physical = e.Storage.Kind, e.Storage.Table
	}
	src, ok := sources.ByKind(kind)
	if !ok {
		return nil, fmt.Errorf("表 %s: 存储 %s 不存在", table, kind)
	}
	scan, err := scanStep(physical, cat)
	if err != nil {
		return nil, err
	}
	return src.Steps(e.Storage, e.NaturalKey, scan, lib)
}

// sqlKind: schema 的列类型 → 核心类型(类型的注册表;jsonb 只能作为 json_field 的输入)
func sqlKind(sqlType string) (core.Kind, bool) {
	if sqlType == "jsonb" || sqlType == "json" {
		return core.JSON, true
	}
	t, ok := kinds.BySQL(sqlType)
	if !ok || t.Core == "" {
		return "", false
	}
	return t.Core, true
}

// partsCtx: 出口生成流水线部件时用(exits.PartsCtx)。
type partsCtx struct {
	table string
	cat   *catalog
}

func (x partsCtx) ColumnKind(col string) (core.Kind, error) {
	c, ok := x.cat.column(x.table, col)
	if !ok {
		return "", fmt.Errorf("列 %s 不在 %s 里", col, x.table)
	}
	t, err := coreType(x.table, c)
	return t.Kind, err
}
