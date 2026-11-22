package compiler

// 声明式 datasource → SQL。筛选条件的解析、SELECT 列的推导、命名约定、keyset 包装。

import (
	"fmt"
	"strings"

	"google.golang.org/protobuf/reflect/protoreflect"

	"apifactory/factory/exits"
	"apifactory/factory/kinds"
	"apifactory/factory/spec"
)

type resolvedFilter struct {
	Field    protoreflect.FieldDescriptor
	Name     string    // 请求字段名 = 查询参数名
	Column   catColumn // 列
	Op       string    // eq / gt / gte / lt / lte / prefix
	SQLOp    string    // = / > / >= / < / <= / LIKE
	Presence bool      // true: 未指定时不筛选
	List     bool      // 列表参数(repeated)
	Columns  []string  // 多列的筛选单元(第一列 = Column)
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
		if ex, ok := exits.ByKind(s.PatternKind()); ok && ex.RequestNames(s.Pattern)[name] { // 出口加的字段(page_size …)不是筛选条件
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
		rf := resolvedFilter{Field: fd, Name: name, Column: col, Op: f.Op, SQLOp: spec.FilterOps[f.Op], Presence: fd.HasPresence()}
		for _, c := range f.Columns {
			if _, ok := cat.column(table, c); !ok {
				return nil, fmt.Errorf("datasource.filters.%s: column %q is not in table %s", name, c, table)
			}
		}
		rf.Columns = f.Columns
		if f.Op == "in_tuples" { // 组的列表: 组的字段 = 筛选的列(顺序一致)
			if !fd.IsList() || fd.Kind() != protoreflect.MessageKind {
				return nil, fmt.Errorf("datasource.filters.%s: in_tuples needs a repeated message field", name)
			}
			mf := fd.Message().Fields()
			if mf.Len() != len(f.Columns) {
				return nil, fmt.Errorf("datasource.filters.%s: %s has %d fields, filter has %d columns", name, fd.Message().Name(), mf.Len(), len(f.Columns))
			}
			for i, c := range f.Columns {
				if string(mf.Get(i).Name()) != c {
					return nil, fmt.Errorf("datasource.filters.%s: field #%d of %s is %s, column is %s", name, i+1, fd.Message().Name(), mf.Get(i).Name(), c)
				}
			}
		}
		if fd.IsList() { // 列表参数: 至少 1 个(min_items ≥ 1)时必填
			rf.List = true
			r := convRules(fd)
			rf.Presence = !(r != nil && r.MinItems != nil && *r.MinItems > 0)
		}
		out = append(out, rf)
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
	grouped, err := GroupedColumns(table, cat.tables[table], s.Pattern.Group, s.Pattern.Rank)
	if err != nil {
		return nil, err
	}
	check := func(names []string, origin string) error {
		for _, n := range names {
			if src, ok := s.Datasource.Rename[n]; ok { // 改名的响应字段: 查来源的列
				n = src
			}
			if grouped != nil { // 分组之后: 只有分组键 / 聚合 / 名次
				found := false
				for _, c := range grouped {
					found = found || c.Name == n
				}
				if !found {
					return fmt.Errorf("%s: %q 不是分组之后的列(分组键 / 计算 / 排名的输出)", origin, n)
				}
				continue
			}
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
	ex, _ := exits.ByKind(s.PatternKind())
	switch {
	case ex != nil && ex.FromTable:
		msg = md.Output()
	case ex != nil && ex.Rows:
		out := md.Output()
		for i := 0; i < out.Fields().Len(); i++ {
			if fd := out.Fields().Get(i); fd.IsList() && fd.Kind() == protoreflect.MessageKind {
				msg = fd.Message()
			}
		}
		if msg == nil {
			return nil, fmt.Errorf("pattern %s: %s has no repeated message field", s.PatternKind(), out.Name())
		}
	default:
		return nil, fmt.Errorf("datasource.columns is required for pattern %s", s.PatternKind())
	}
	extra := map[string]bool{} // 出口加的响应字段(found 等)不是列
	if ex != nil && ex.FromTable && ex.ExtraFor != nil {
		for _, f := range ex.ExtraFor(s.Pattern) {
			extra[f.Name] = true
		}
	}
	var cols []string
	for i := 0; i < msg.Fields().Len(); i++ {
		if n := string(msg.Fields().Get(i).Name()); !extra[n] {
			cols = append(cols, n)
		}
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
		t, ok := kinds.BySQL(col.Type) // 游标参数的类型转换(sqlc 在行比较里需要显式类型)
		cast := ""
		if ok {
			cast = t.CanonicalSQL()
		}
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
