package compiler

// 声明式 datasource → SQL。筛选条件的解析、SELECT 列的推导、命名约定、keyset 包装。

import (
	"fmt"
	"strings"

	"google.golang.org/protobuf/reflect/protoreflect"

	"apifactory/factory/spec"
)

// paginationFields 是 list_keyset 模式自己处理的请求字段,不是筛选条件。
var paginationFields = map[string]bool{"page_size": true, "page_token": true}

type resolvedFilter struct {
	Field    protoreflect.FieldDescriptor
	Name     string    // 请求字段名 = 查询参数名
	Column   catColumn // 列
	Op       string    // eq / gt / gte / lt / lte / prefix
	SQLOp    string    // = / > / >= / < / <= / LIKE
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

// baseSQL 生成基础查询(list_keyset 的游标/ORDER BY/LIMIT 之后再追加)。
func baseSQL(s *spec.Spec, cols []string, filters []resolvedFilter) string {
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
	sql := fmt.Sprintf("SELECT %s\nFROM %s", strings.Join(cols, ", "), s.Datasource.Table)
	if len(conds) > 0 {
		sql += "\nWHERE " + strings.Join(conds, "\n  AND ")
	}
	return sql
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

// keysetCast: 游标参数的类型转换(sqlc 在行比较里需要显式类型)。游标列必须 NOT NULL。
var keysetCast = map[string]string{
	"timestamptz": "timestamptz",
	"int8":        "bigint",
	"int4":        "int",
	"text":        "text",
	"bool":        "boolean",
	"float8":      "float8",
}

// keysetSQL: 把基础查询包一层,追加游标条件 / ORDER BY / LIMIT。列的类型来自 sqlc 对基础查询的分析。
func keysetSQL(s *spec.Spec, base string, cols []catColumn) (string, error) {
	p := s.Pattern
	op := "<"
	if p.Direction == "asc" {
		op = ">"
	}
	var keys, args, order []string
	for _, c := range p.OrderBy {
		var col *catColumn
		for i := range cols {
			if cols[i].Name == c {
				col = &cols[i]
			}
		}
		if col == nil {
			return "", fmt.Errorf("%s: order_by column %q is not in the SELECT list", s.Path, c)
		}
		cast, ok := keysetCast[col.Type]
		if !ok || !col.NotNull {
			return "", fmt.Errorf("%s: order_by column %q has type %s (not_null=%v): must be NOT NULL and one of time/int/text/bool/float", s.Path, c, col.Type, col.NotNull)
		}
		keys = append(keys, "base."+c)
		args = append(args, fmt.Sprintf("sqlc.narg(cursor_%s)::%s", c, cast))
		order = append(order, "base."+c+" "+strings.ToUpper(p.Direction))
	}
	return fmt.Sprintf("SELECT * FROM (\n%s\n) AS base\nWHERE (%s IS NULL\n       OR (%s) %s (%s))\nORDER BY %s\nLIMIT sqlc.arg(row_limit)::int",
		indent(base), args[0], strings.Join(keys, ", "), op, strings.Join(args, ", "), strings.Join(order, ", ")), nil
}

func indent(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = "  " + l
	}
	return strings.Join(lines, "\n")
}

// conventionFilter: 命名约定。
//
//	字段名 = 列名          → eq
//	<x>_after             → <x>_at gte
//	<x>_before            → <x>_at lt
//	<x>_prefix            → x prefix
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
		if c, ok := cat.column(table, base); ok && isText(c.Type) {
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

func isText(t string) bool {
	switch t {
	case "text", "varchar", "bpchar", "char":
		return true
	}
	return false
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
