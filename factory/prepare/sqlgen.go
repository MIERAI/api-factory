package prepare

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"google.golang.org/protobuf/reflect/protoreflect"

	"apifactory/factory/spec"
)

// ---------------------------------------------------------------------------
// schema 目录: 由 sqlc 解析 db/schema/*.sql 后输出(gen.json),不自己解析 DDL
// ---------------------------------------------------------------------------

const catalogPath = ".factory/sqlc/codegen.json"

type catColumn struct {
	Name    string
	Type    string // int4 / int8 / text / timestamptz ...
	NotNull bool
}

type catalog struct {
	tables map[string][]catColumn
}

func loadCatalog() (*catalog, error) {
	b, err := os.ReadFile(catalogPath)
	if err != nil {
		return nil, fmt.Errorf("sqlc catalog: %w", err)
	}
	var raw struct {
		Catalog struct {
			DefaultSchema string `json:"default_schema"`
			Schemas       []struct {
				Name   string `json:"name"`
				Tables []struct {
					Rel struct {
						Name string `json:"name"`
					} `json:"rel"`
					Columns []struct {
						Name    string `json:"name"`
						NotNull bool   `json:"not_null"`
						Type    struct {
							Name string `json:"name"`
						} `json:"type"`
					} `json:"columns"`
				} `json:"tables"`
			} `json:"schemas"`
		} `json:"catalog"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, err
	}
	cat := &catalog{tables: map[string][]catColumn{}}
	for _, sc := range raw.Catalog.Schemas {
		if sc.Name != raw.Catalog.DefaultSchema {
			continue
		}
		for _, t := range sc.Tables {
			var cols []catColumn
			for _, c := range t.Columns {
				cols = append(cols, catColumn{Name: c.Name, Type: c.Type.Name, NotNull: c.NotNull})
			}
			cat.tables[t.Rel.Name] = cols
		}
	}
	return cat, nil
}

func (c *catalog) column(table, name string) (catColumn, bool) {
	for _, col := range c.tables[table] {
		if col.Name == name {
			return col, true
		}
	}
	return catColumn{}, false
}

// ---------------------------------------------------------------------------
// 声明式 datasource → SQL(sqlc 输入)
// ---------------------------------------------------------------------------

// paginationFields 是 list_keyset 模式自己处理的请求字段,不是筛选条件。
var paginationFields = map[string]bool{"page_size": true, "page_token": true}

// compileSQL 生成基础查询(list_keyset 的游标/ORDER BY/LIMIT 之后再追加)。
func compileSQL(s *spec.Spec, md protoreflect.MethodDescriptor, cat *catalog) (string, error) {
	ds := s.Datasource
	if ds.SQL != "" {
		return strings.TrimRight(strings.TrimSpace(ds.SQL), ";"), nil
	}
	table := ds.Table
	if _, ok := cat.tables[table]; !ok {
		return "", fmt.Errorf("datasource.table: %q is not in db/schema/*.sql", table)
	}

	cols, err := selectColumns(s, md, cat)
	if err != nil {
		return "", err
	}

	filters, err := resolveFilters(s, md, cat)
	if err != nil {
		return "", err
	}
	var conds []string
	for _, f := range filters {
		if f.Op == "prefix" {
			conds = append(conds, prefixCond(f))
			continue
		}
		if f.Presence {
			// optional / Timestamp: 未指定(NULL)时不筛选
			conds = append(conds, fmt.Sprintf("(sqlc.narg(%s)::%s IS NULL OR %s %s sqlc.narg(%s)::%s)",
				f.Name, f.Column.Type, f.Column.Name, f.SQLOp, f.Name, f.Column.Type))
		} else {
			conds = append(conds, fmt.Sprintf("%s %s sqlc.arg(%s)::%s", f.Column.Name, f.SQLOp, f.Name, f.Column.Type))
		}
	}

	sql := fmt.Sprintf("SELECT %s\nFROM %s", strings.Join(cols, ", "), table)
	if len(conds) > 0 {
		sql += "\nWHERE " + strings.Join(conds, "\n  AND ")
	}
	return sql, nil
}

// prefixCond: 前缀匹配。参数里的 \ % _ 先转义,按字面匹配(手写 LIKE x || '%' 会把用户输入当通配符: 第一波 R4)。
// 平台每次用实际参数做执行计划,所以有 text_pattern_ops 索引时可以用上索引(starts_with 用不上)。
func prefixCond(f resolvedFilter) string {
	arg := fmt.Sprintf("sqlc.arg(%s)::text", f.Name)
	if f.Presence {
		arg = fmt.Sprintf("sqlc.narg(%s)::text", f.Name)
	}
	like := fmt.Sprintf(`%s LIKE replace(replace(replace(%s, '\', '\\'), '%%', '\%%'), '_', '\_') || '%%'`, f.Column.Name, arg)
	if f.Presence {
		return fmt.Sprintf("(%s IS NULL OR %s)", arg, like)
	}
	return like
}

// resolvedFilter: 一个请求字段对应的筛选条件(SQL 生成和验收用例生成共用)。
type resolvedFilter struct {
	Field    protoreflect.FieldDescriptor
	Name     string    // 请求字段名 = 查询参数名
	Column   catColumn // 列
	Op       string    // eq / gt / gte / lt / lte
	SQLOp    string    // = / > / >= / < / <=
	Presence bool      // true: 未指定时不筛选
}

func resolveFilters(s *spec.Spec, md protoreflect.MethodDescriptor, cat *catalog) ([]resolvedFilter, error) {
	ds := s.Datasource
	table := ds.Table
	in := md.Input()
	used := map[string]bool{}
	var out []resolvedFilter
	for i := 0; i < in.Fields().Len(); i++ {
		fd := in.Fields().Get(i)
		name := string(fd.Name())
		if s.PatternKind() == "list_keyset" && paginationFields[name] {
			continue
		}
		f, ok := ds.Filters[name]
		if !ok {
			f, ok = conventionFilter(name, table, cat)
		}
		if !ok {
			return nil, fmt.Errorf("request field %q has no matching column in %s (declare datasource.filters.%s)", name, table, name)
		}
		used[name] = true
		col, ok := cat.column(table, f.Column)
		if !ok {
			return nil, fmt.Errorf("datasource.filters.%s: column %q is not in table %s", name, f.Column, table)
		}
		out = append(out, resolvedFilter{Field: fd, Name: name, Column: col, Op: f.Op, SQLOp: spec.FilterOps[f.Op], Presence: fd.HasPresence()})
	}
	for name := range ds.Filters {
		if !used[name] {
			return nil, fmt.Errorf("datasource.filters.%s: no such request field in %s", name, in.Name())
		}
	}
	return out, nil
}

// selectColumns: 显式的 datasource.columns,否则从响应字段推导。
func selectColumns(s *spec.Spec, md protoreflect.MethodDescriptor, cat *catalog) ([]string, error) {
	table := s.Datasource.Table
	check := func(names []string, origin string) error {
		for _, n := range names {
			if _, ok := cat.column(table, n); !ok {
				return fmt.Errorf("%s: column %q is not in table %s (declare datasource.columns or use datasource.sql)", origin, n, table)
			}
		}
		return nil
	}
	if cols := s.Datasource.Columns; len(cols) > 0 {
		return cols, check(cols, "datasource.columns")
	}
	var msg protoreflect.MessageDescriptor
	switch s.PatternKind() {
	case "get_by_id":
		msg = md.Output()
	case "list_keyset":
		out := md.Output()
		for i := 0; i < out.Fields().Len(); i++ {
			if fd := out.Fields().Get(i); fd.IsList() && fd.Kind() == protoreflect.MessageKind {
				msg = fd.Message()
			}
		}
		if msg == nil {
			return nil, fmt.Errorf("pattern list_keyset: %s has no repeated message field", out.Name())
		}
	default:
		return nil, fmt.Errorf("datasource.columns is required for pattern %s", s.PatternKind())
	}
	var cols []string
	for i := 0; i < msg.Fields().Len(); i++ {
		cols = append(cols, string(msg.Fields().Get(i).Name()))
	}
	if err := check(cols, "response "+string(msg.Name())); err != nil {
		return nil, err
	}
	// keyset 的游标列必须在 SELECT 里
	for _, c := range s.Pattern.OrderBy {
		if !contains(cols, c) {
			cols = append(cols, c)
		}
	}
	return cols, check(cols, "pattern.order_by")
}

// conventionFilter: 命名约定。
//
//	字段名 = 列名          → eq
//	<x>_after             → <x>_at gte
//	<x>_before            → <x>_at lt
//	<单数表名>_id          → id eq   (jobs: job_id → id)
func conventionFilter(field, table string, cat *catalog) (spec.Filter, bool) {
	if _, ok := cat.column(table, field); ok {
		return spec.Filter{Column: field, Op: "eq"}, true
	}
	for suffix, op := range map[string]string{"_after": "gte", "_before": "lt"} {
		if base, ok := strings.CutSuffix(field, suffix); ok {
			if _, ok := cat.column(table, base+"_at"); ok {
				return spec.Filter{Column: base + "_at", Op: op}, true
			}
		}
	}
	if base, ok := strings.CutSuffix(field, "_prefix"); ok {
		if c, ok := cat.column(table, base); ok && kindOf(c.Type) == "text" {
			return spec.Filter{Column: base, Op: "prefix"}, true
		}
	}
	if field == singular(table)+"_id" {
		if _, ok := cat.column(table, "id"); ok {
			return spec.Filter{Column: "id", Op: "eq"}, true
		}
	}
	return spec.Filter{}, false
}

func singular(s string) string {
	switch {
	case strings.HasSuffix(s, "ies"):
		return strings.TrimSuffix(s, "ies") + "y"
	case strings.HasSuffix(s, "ses"):
		return strings.TrimSuffix(s, "es")
	case strings.HasSuffix(s, "s"):
		return strings.TrimSuffix(s, "s")
	}
	return s
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
