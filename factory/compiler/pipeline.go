package compiler

// 声明式 datasource → 核心代数的流水线(单元展开)→ SQL(core 的 SQL 后端)。

import (
	"fmt"
	"sort"
	"strings"

	"google.golang.org/protobuf/reflect/protoreflect"

	"apifactory/factory/cells"
	"apifactory/factory/core"
	"apifactory/factory/exits"
	"apifactory/factory/ir"
	"apifactory/factory/kinds"
	"apifactory/factory/sources"
	"apifactory/factory/spec"
	"apifactory/factory/where"
)

func coreType(table string, c catColumn) (core.Type, error) {
	k, ok := sqlKind(c.Type)
	if !ok {
		return core.Type{}, fmt.Errorf("%s.%s: 列类型 %s 核心代数还不支持", table, c.Name, c.Type)
	}
	return core.Type{Kind: k, Nullable: !c.NotNull}, nil
}

// WhereTypes: 固定条件(factory/where)用的列的类型(表的逻辑列;设计编译器也用它检查)。
func WhereTypes(cols []Column) func(string) (core.Type, bool) {
	return func(name string) (core.Type, bool) {
		for _, c := range cols {
			if c.Name == name {
				k, ok := sqlKind(c.Type)
				return core.Type{Kind: k, Nullable: !c.NotNull}, ok && k != core.JSON
			}
		}
		return core.Type{}, false
	}
}

// cat: schema 的目录(来源的扫描用);view: 加了关联列的目录(筛选 / 输出 / 固定条件用);lookups: 关联的步骤(接在来源之后)。
// inputs: 请求字段名 → 有没有 presence(派生参数的输入: 没有 presence = 必填)。
func buildPipeline(s *spec.Spec, lib *cells.Library, cols []string, filters []resolvedFilter, cat, view *catalog, lookups []core.Step, inputs map[string]bool) (*core.Pipeline, error) {
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
		cell := f.Op // 单元的内部名(eq / in / valid_at …);单元库按内部名或名字都能找到
		cols := f.Columns
		if len(cols) == 0 {
			cols = []string{f.Column.Name}
		}
		var members []string
		if f.Op == "in_tuples" { // 组的列表: 每个字段一个列表参数(类型 = 列的类型)
			for _, c := range cols {
				cc, _ := view.column(table, c)
				ct, err := coreType(table, cc)
				if err != nil {
					return nil, err
				}
				m := ir.MemberParam(f.Name, c)
				members = append(members, m)
				p.Params = append(p.Params, core.ParamDecl{Name: m, Type: core.Type{Kind: ct.Kind}, Required: !f.Presence, List: true})
			}
		} else {
			k := t.Kind
			if k == core.Int64 && f.Field != nil && f.Field.Kind() == protoreflect.Int32Kind { // 整数32 的输入和整数64 的列(名次 / 件数)比较: 参数按输入的类型
				k = core.Int32
			}
			if ymdInput(s, table, f) { // 日期的输入筛选整数编码的日期的列: 参数是日期
				k = core.Date
			}
			p.Params = append(p.Params, core.ParamDecl{Name: f.Name, Type: core.Type{Kind: k}, Required: !f.Presence, List: f.List})
		}
		e, err := lib.Filter(cell, cols, f.Name, members...)
		if err != nil {
			return nil, fmt.Errorf("筛选 %s: %w", f.Name, err)
		}
		if ymdInput(s, table, f) {
			e = ymdColumn(e, f.Column.Name)
		}
		if f.Grouped { // 分组之后的筛选: 接在 rank 之后(模式的 $分组之后)
			parts.Having = append(parts.Having, e)
			continue
		}
		parts.Filters = append(parts.Filters, e)
	}
	derived, err := derivedFilters(s, lib, view, p, inputs) // view: 派生参数也可以和关联加的列比较
	if err != nil {
		return nil, err
	}
	parts.Filters = append(parts.Filters, derived...)
	for _, w := range s.Datasource.Where { // 固定条件: 和筛选一样作用在来源的列上
		e, err := where.ParseOpt(w, WhereTypes(view.tables[table]), whereOpt(s, table))
		if err != nil {
			return nil, err
		}
		parts.Filters = append(parts.Filters, e)
	}
	for _, c := range cols {
		src := c
		if r, ok := s.Datasource.Rename[c]; ok { // 输出改名: 响应字段 ← 来源的列
			src = r
		}
		parts.Output = append(parts.Output, core.NamedExpr{Name: c, Expr: core.Col(src)})
	}
	ex, ok := exits.ByKind(s.PatternKind()) // 出口的注册表(factory/exits)
	if !ok || ex.Cell == "" {
		return nil, fmt.Errorf("模式 %s 没有对应的套装", s.PatternKind())
	}
	grouped, err := GroupedColumns(table, view.tables[table], s.Pattern.Group, s.Pattern.Rank) // 分组键可以是关联加的列
	if err != nil {
		return nil, err
	}
	if ex.Parts != nil {
		if err := ex.Parts(s, partsCtx{table, view, grouped}, &parts, p); err != nil { // view: 加了关联 / 子聚合的列
			return nil, err
		}
	}
	name := ex.Cell
	steps, err := lib.Pattern(name, parts)
	if err != nil {
		return nil, err
	}
	p.Steps = append(append(source, lookups...), steps...)
	if _, err := core.Check(p); err != nil {
		return nil, fmt.Errorf("设计规则检查:\n%w", err)
	}
	if err := checkPickUnique(p); err != nil {
		return nil, fmt.Errorf("设计规则检查:\n%w", err)
	}
	return p, nil
}

// derivedFilters: 派生参数(datasource.derived)→ 筛选。值 = 核心代数的时间运算(now / add / trunc),
// 名字 = 请求参数(派生参数的输入,加进流水线的参数;必填 = proto 里没有 presence)。按名字排序(SQL 确定)。
func derivedFilters(s *spec.Spec, lib *cells.Library, cat *catalog, p *core.Pipeline, presence map[string]bool) ([]*core.Expr, error) {
	ds := s.Datasource
	names := make([]string, 0, len(ds.Derived))
	for n := range ds.Derived {
		names = append(names, n)
	}
	sort.Strings(names)
	var out []*core.Expr
	for _, name := range names {
		d := ds.Derived[name]
		var optional []string
		for id := range d.Inputs {
			if _, ok := presence[id]; !ok {
				return nil, fmt.Errorf("派生参数 %s: 输入 %s 不是请求字段", name, id)
			}
		}
		v, err := core.ParseExpr(d.Expr, func(id string) (*core.Expr, bool) {
			k, ok := d.Inputs[id]
			if !ok {
				return nil, false
			}
			if p.Param(id) == nil {
				p.Params = append(p.Params, core.ParamDecl{Name: id, Type: core.Type{Kind: core.Kind(k)}, Required: !presence[id]})
			}
			if !p.Param(id).Required && !contains(optional, id) {
				optional = append(optional, id)
			}
			return core.Param(id), true
		})
		if err != nil {
			return nil, fmt.Errorf("派生参数 %s: %w", name, err)
		}
		core.SetZone(v, d.TimeZone)
		if _, ok := cat.column(ds.Table, d.Column); !ok {
			return nil, fmt.Errorf("派生参数 %s: 列 %s 不在 %s 里", name, d.Column, ds.Table)
		}
		e, err := lib.FilterValue(d.Op, []string{d.Column}, v, optional)
		if err != nil {
			return nil, fmt.Errorf("派生参数 %s: %w", name, err)
		}
		if encoding(s, ds.Table, d.Column) == ymdEncoding { // 整数编码的日期的列: 按 ymd 读成日期再比较(SQL 按整数比较)
			e = ymdColumn(e, d.Column)
		}
		out = append(out, e)
	}
	return out, nil
}

// whereOpt: 固定条件里的 今天 / 现在(今天 = 领域的时区的日期;整数编码的日期的列和 今天 比较时按日期读)。
func whereOpt(s *spec.Spec, table string) where.Options {
	return where.Options{Time: true, Zone: s.Datasource.TimeZone, YMD: func(col string) bool {
		return s.Domain != nil && encoding(s, table, col) == ymdEncoding
	}}
}

// checkPickUnique: 每组取一条(API 部分)的 分组键 + 组内的排序 必须覆盖一个唯一键,否则组内可能并列,
// 取哪一行不确定(参考求值器在运行时报 ambiguous order;这里在设计稿编译时就报)。
// 唯一键: 多版本的来源取最新之后 = 自然键。普通的表的主键编译器还不知道 → 不检查(运行时由参考求值器检查)。
func checkPickUnique(p *core.Pipeline) error {
	var unique []string
	for _, st := range p.Steps {
		if st.Kind != "pick_per_key" {
			continue
		}
		if strings.HasPrefix(st.From, "来源 ") {
			unique = st.Keys
			continue
		}
		if unique == nil {
			continue
		}
		have := map[string]bool{}
		for _, k := range st.Keys {
			have[k] = true
		}
		for _, o := range st.Order {
			have[o.Col] = true
		}
		var miss []string
		for _, u := range unique {
			if !have[u] {
				miss = append(miss, u)
			}
		}
		if len(miss) > 0 {
			return fmt.Errorf("%s: 每组取一条 按 %v 分组,组内的排序 %v 不能确定唯一的一行(同一组里可能有多行排序值相同)。"+
				"排序里加上 %v(数据来源的主键)", st.From, st.Keys, orderCols(st.Order), miss)
		}
	}
	return nil
}

func orderCols(o []core.SortKey) []string {
	var out []string
	for _, k := range o {
		out = append(out, k.Col)
	}
	return out
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
	table   string
	cat     *catalog
	grouped []catColumn // 分组之后的列(没有分组 → nil): 出口看到的列是这些
}

func (x partsCtx) ColumnKind(col string) (core.Kind, error) {
	if x.grouped != nil {
		for _, c := range x.grouped {
			if c.Name == col {
				t, err := coreType(x.table, c)
				return t.Kind, err
			}
		}
		return "", fmt.Errorf("列 %s 不在分组之后的列里(分组键 / 聚合 / 名次)", col)
	}
	c, ok := x.cat.column(x.table, col)
	if !ok {
		return "", fmt.Errorf("列 %s 不在 %s 里", col, x.table)
	}
	t, err := coreType(x.table, c)
	return t.Kind, err
}

// GroupedColumns: 分组(+ 名次)之后的列 = 分组键(表的列)+ 聚合(类型由核心决定: core.AggType)+ 名次(int8)。
// 没有分组 → nil。设计编译器(响应字段的类型)和编译器(SELECT 的列 / 游标的类型)共用。
func GroupedColumns(table string, cols []Column, g *spec.GroupBy, r *spec.RankBy) ([]Column, error) {
	if g == nil {
		return nil, nil
	}
	find := func(name string) (Column, bool) {
		for _, c := range cols {
			if c.Name == name {
				return c, true
			}
		}
		return Column{}, false
	}
	var out []Column
	var errs []string
	for _, k := range g.Keys {
		c, ok := find(k)
		if !ok {
			errs = append(errs, fmt.Sprintf("分组键 %s 不是 %s 的列", k, table))
			continue
		}
		out = append(out, c)
	}
	for _, a := range g.Aggs {
		var col *core.Type
		if a.Column != "" {
			c, ok := find(a.Column)
			if !ok {
				errs = append(errs, fmt.Sprintf("%s: 列 %s 不是 %s 的列", a.Name, a.Column, table))
				continue
			}
			t, err := coreType(table, c)
			if err != nil {
				errs = append(errs, fmt.Sprintf("%s: %v", a.Name, err))
				continue
			}
			col = &t
		}
		t, err := core.AggType(a.Func, col, false)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", a.Name, err))
			continue
		}
		typ := t.Kind.SQLType()
		if a.Func != "sum" && a.Column != "" { // min / max: 原列的类型(派生的类型、varchar 等保持原样)
			c, _ := find(a.Column)
			typ = c.Type
		}
		out = append(out, Column{Name: a.Name, Type: typ, NotNull: !t.Nullable})
	}
	if r != nil {
		out = append(out, Column{Name: r.As, Type: "int8", NotNull: true})
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("分组: %s", strings.Join(errs, "; "))
	}
	return out, nil
}

// ymdEncoding: 整数编码的日期(领域的列: { 编码: YYYYMMDD })。
const ymdEncoding = "YYYYMMDD"

// encoding: 领域里列的编码(没有 → "")。
func encoding(s *spec.Spec, table, col string) string {
	if m := s.Domain.Column(table, col); m != nil {
		return m.Encoding
	}
	return ""
}

// ymdInput: 日期的输入(proto 里是文本)筛选整数编码的日期的列。整数的输入照常按整数比较(同一种编码)。
func ymdInput(s *spec.Spec, table string, f resolvedFilter) bool {
	return !f.Grouped && len(f.Columns) == 0 && f.Field != nil && f.Field.Kind() == protoreflect.StringKind && !f.List &&
		encoding(s, table, f.Column.Name) == ymdEncoding
}

// ymdColumn: 条件里的列 col → ymd(col)。
func ymdColumn(e *core.Expr, col string) *core.Expr {
	if e == nil {
		return nil
	}
	if e.Op == "col" && e.Name == col {
		return core.YMD(e)
	}
	c := *e
	c.Args = nil
	for _, a := range e.Args {
		c.Args = append(c.Args, ymdColumn(a, col))
	}
	return &c
}
