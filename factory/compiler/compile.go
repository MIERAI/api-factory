// Package compiler 把式样书"理解透": 式样书 + 领域层 + proto + schema → API 计划(IR)+ 最终的 SQL。
//
// 只做语言无关的事情。SQL 的分析用 sqlc 的 json 输出,从不读任何语言的生成代码。
// 输出:
//   - db/queries/<id>.sql  (最终的 SQL,适配器用它生成各语言的查询代码)
//   - .factory/plan.json   (IR,下游所有环节的输入)
package compiler

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	"apifactory/factory/cells"
	"apifactory/factory/core"
	"apifactory/factory/ir"
	"apifactory/factory/protoload"
	"apifactory/factory/spec"
)

const queriesDir = "db/queries"

func Compile(specs []*spec.Spec) (*ir.Plan, error) {
	files, err := protoload.Load()
	if err != nil {
		return nil, err
	}
	methods := map[*spec.Spec]protoreflect.MethodDescriptor{}
	for _, s := range specs {
		p, _ := s.RPCParts()
		md, err := protoload.FindMethod(files, p.ProtoPackage, p.Service, p.Method)
		if err != nil {
			return nil, fmt.Errorf("%s: api.rpc: %w", s.Path, err)
		}
		methods[s] = md
	}

	// 第 0 遍: schema 目录(表/列/类型)
	a0, err := analyze(nil)
	if err != nil {
		return nil, err
	}
	cat := a0.catalog
	// 存储: JSON版本 的逻辑表: 列来自领域声明(自然键 NOT NULL),之后当普通的表用
	for _, s := range specs {
		if s.Domain == nil {
			continue
		}
		for name, e := range s.Domain.Entities {
			if e.Storage == nil {
				continue
			}
			if _, raw := cat.tables[e.Storage.Table]; !raw {
				return nil, fmt.Errorf("%s: 存储的原始表 %s 不在 db/schema 里", s.Domain.Path, e.Storage.Table)
			}
			var cols []catColumn
			for _, f := range e.Storage.Fields {
				cols = append(cols, catColumn{Name: f.Name, Type: f.Type, NotNull: contains(e.NaturalKey, f.Name)})
			}
			cat.tables[name] = cols
		}
	}

	lib, err := cells.Load()
	if err != nil {
		return nil, err
	}

	// 声明 → 单元展开 → 流水线 → SQL(core 的 SQL 后端);逃生口的 SQL 原样使用
	plan := &ir.Plan{Version: ir.Version}
	apis := make([]ir.API, len(specs))
	final := map[string]string{}
	for i, s := range specs {
		md := methods[s]
		api, err := apiSkeleton(s, md, cat)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", s.Path, err)
		}
		sql := strings.TrimRight(strings.TrimSpace(s.Datasource.SQL), ";")
		if sql == "" {
			if _, ok := cat.tables[s.Datasource.Table]; !ok {
				return nil, fmt.Errorf("%s: datasource.table: %q is not in db/schema/*.sql", s.Path, s.Datasource.Table)
			}
			cols, err := selectColumns(s, md, cat)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", s.Path, err)
			}
			filters, err := resolveFilters(s, md, cat)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", s.Path, err)
			}
			for _, f := range filters {
				api.Filters = append(api.Filters, ir.Filter{Field: f.Name, Column: f.Column.Name, ColumnType: f.Column.Type, Op: f.Op, SQLOp: f.SQLOp, Presence: f.Presence, Columns: f.Columns})
			}
			pl, err := buildPipeline(s, lib, cols, filters, cat)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", s.Path, err)
			}
			q, err := core.ToSQL(pl, core.Options{ParamRef: core.Sqlc})
			if err != nil {
				return nil, fmt.Errorf("%s: %w", s.Path, err)
			}
			api.Pipeline = pl
			sql = q.Text
		}
		api.Query.SQL = sql
		apis[i] = api
		final[s.ID()] = queryFile(s, sql)
	}
	// 逃生口的 SQL + 分页: SQL 对核心代数不透明,仍然用 keysetSQL 包一层(游标列的类型来自 sqlc 对基础查询的分析)
	escBase := map[string]string{}
	for i, s := range specs {
		if apis[i].Pipeline == nil && s.PatternKind() == "list_keyset" {
			escBase[s.ID()] = final[s.ID()]
		}
	}
	if len(escBase) > 0 {
		a1, err := analyze(escBase)
		if err != nil {
			return nil, err
		}
		for i, s := range specs {
			if _, ok := escBase[s.ID()]; !ok {
				continue
			}
			sql, err := keysetSQL(s, apis[i].Query.SQL, a1.queries[s.QueryName()].Columns)
			if err != nil {
				return nil, err
			}
			apis[i].Query.SQL = sql
			final[s.ID()] = queryFile(s, sql)
		}
	}
	a2, err := analyze(final)
	if err != nil {
		return nil, err
	}
	for i, s := range specs {
		q := a2.queries[s.QueryName()]
		for _, c := range q.Columns {
			apis[i].Query.Columns = append(apis[i].Query.Columns, ir.Column{Name: c.Name, Type: c.Type, NotNull: c.NotNull})
		}
		for _, p := range q.Params {
			apis[i].Query.Params = append(apis[i].Query.Params, ir.Param{Name: p.Name, Type: p.Type, NotNull: p.NotNull})
		}
	}

	// 领域层的 enum(和 proto 的一致性在这里检查)
	seen := map[string]bool{}
	for _, s := range specs {
		table := s.Datasource.Table
		if table == "" || s.Domain == nil {
			continue
		}
		e, ok := s.Domain.Entities[table]
		if !ok {
			continue
		}
		if st := e.Storage; st != nil && !seen["storage:"+table] {
			seen["storage:"+table] = true
			plan.Storages = append(plan.Storages, storageIR(table, e.NaturalKey, st))
		}
		cols := make([]string, 0, len(e.Columns))
		for c := range e.Columns {
			cols = append(cols, c)
		}
		sort.Strings(cols)
		for _, col := range cols {
			if e.Columns[col].Enum == nil || seen[table+"."+col] {
				continue
			}
			p, _ := s.RPCParts()
			en, err := resolveEnum(s.Domain, table, col, files, cat, p.ProtoPackage)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", s.Path, err)
			}
			seen[table+"."+col] = true
			plan.Enums = append(plan.Enums, *en)
		}
	}

	plan.APIs = apis
	plan.Services = services(specs, methods)
	return plan, nil
}

// Write: 最终的 SQL → db/queries,IR → path。
func Write(plan *ir.Plan, path string) error {
	old, _ := filepath.Glob(filepath.Join(queriesDir, "*.sql"))
	for _, f := range old {
		_ = os.Remove(f) // 全量重新生成,删掉已不存在的式样书留下的查询
	}
	if err := os.MkdirAll(queriesDir, 0o755); err != nil {
		return err
	}
	for _, a := range plan.APIs {
		body := fmt.Sprintf("-- Code generated by factory from %s. DO NOT EDIT.\n\n-- name: %s :%s\n%s;\n", a.SpecPath, a.Query.Name, a.Query.Cardinality, a.Query.SQL)
		if err := os.WriteFile(filepath.Join(queriesDir, a.ID+".sql"), []byte(body), 0o644); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return ir.Save(plan, path)
}

func queryFile(s *spec.Spec, sql string) string {
	return fmt.Sprintf("-- name: %s :%s\n%s;\n", s.QueryName(), s.Cardinality(), sql)
}

// apiSkeleton: 和 SQL 无关的部分(消息、模式参数、样例、基础请求)。
func apiSkeleton(s *spec.Spec, md protoreflect.MethodDescriptor, cat *catalog) (ir.API, error) {
	p, _ := s.RPCParts()
	api := ir.API{
		ID: s.ID(), SpecPath: s.Path, Domain: p.Domain(),
		ProtoPackage: p.ProtoPackage, Service: p.Service, Method: p.Method,
		Summary: summary(md), Pattern: s.PatternKind(), Table: s.Datasource.Table,
		Request:  convMessage(md.Input(), 0),
		Response: convMessage(md.Output(), 0),
		Query: ir.Query{
			Name: s.QueryName(), Cardinality: s.Cardinality(), Escape: s.Datasource.SQL != "",
		},
	}
	switch s.PatternKind() {
	case "get_by_id":
		api.GetByID = &ir.GetByID{NotFoundMessage: s.Pattern.NotFoundMessage}
	case "list_all":
		la := &ir.ListAll{OrderBy: s.Pattern.OrderBy, Desc: s.Pattern.Direction == "desc", MaxRows: s.Pattern.MaxRows}
		for _, f := range api.Response.Fields {
			if f.List && f.Kind == "message" {
				la.ItemsField = f.Name
			}
		}
		if la.ItemsField == "" {
			return api, fmt.Errorf("pattern list_all: %s has no repeated message field", api.Response.Name)
		}
		api.ListAll = la
	case "list_keyset":
		k := &ir.Keyset{OrderBy: s.Pattern.OrderBy, Desc: s.Pattern.Direction == "desc", DefaultSize: s.Pattern.PageSize.Default, MaxSize: s.Pattern.PageSize.Max}
		for _, f := range api.Response.Fields {
			if f.List && f.Kind == "message" {
				if k.ItemsField != "" {
					return api, fmt.Errorf("pattern list_keyset: %s must have exactly one repeated message field", api.Response.Name)
				}
				k.ItemsField = f.Name
			}
		}
		for _, name := range []string{"page_size", "page_token"} {
			if api.Request.Field(name) == nil {
				return api, fmt.Errorf("pattern list_keyset: %s must have field %s", api.Request.Name, name)
			}
		}
		if api.Response.Field("next_page_token") == nil {
			return api, fmt.Errorf("pattern list_keyset: %s must have field next_page_token", api.Response.Name)
		}
		if k.ItemsField == "" {
			return api, fmt.Errorf("pattern list_keyset: %s has no repeated message field", api.Response.Name)
		}
		api.Keyset = k
	case "compute":
		c := &ir.Compute{LogicFunction: lowerFirst(p.Method) + "Logic"}
		for _, e := range s.LogicExamples {
			c.Examples = append(c.Examples, ir.Example{Name: e.Name, Request: e.Request, Rows: e.Rows, Expect: e.Expect})
		}
		api.Compute = c
	}
	api.BaseRequest = baseRequest(s)
	return api, nil
}

// baseRequest: 第一个 code: OK 的人写用例的请求。
func baseRequest(s *spec.Spec) map[string]any {
	for _, c := range s.Acceptance.Cases {
		st := c.AllSteps()[0]
		if strings.EqualFold(st.Expect.Code, "OK") {
			out := map[string]any{}
			for k, v := range st.Request {
				if str, ok := v.(string); ok && strings.HasPrefix(str, "$") {
					continue
				}
				out[k] = v
			}
			return out
		}
	}
	return nil
}

func services(specs []*spec.Spec, methods map[*spec.Spec]protoreflect.MethodDescriptor) []ir.Service {
	byKey := map[string]ir.Service{}
	for _, s := range specs {
		p, _ := s.RPCParts()
		byKey[p.ProtoPackage+"."+p.Service] = ir.Service{ProtoPackage: p.ProtoPackage, Name: p.Service, Domain: p.Domain(), ProtoFile: methods[s].ParentFile().Path()}
	}
	out := make([]ir.Service, 0, len(byKey))
	for _, v := range byKey {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ProtoPackage+out[i].Name < out[j].ProtoPackage+out[j].Name })
	return out
}

// resolveEnum: 检查领域层的映射和 proto enum 一致。
func resolveEnum(d *spec.Domain, table, column string, files *protoregistry.Files, cat *catalog, protoPkg string) (*ir.Enum, error) {
	meta := d.Column(table, column)
	desc, err := files.FindDescriptorByName(protoreflect.FullName(meta.Enum.Proto))
	if err != nil {
		return nil, fmt.Errorf("domain enum %s.%s: proto %s: %w", table, column, meta.Enum.Proto, err)
	}
	ed, ok := desc.(protoreflect.EnumDescriptor)
	if !ok {
		return nil, fmt.Errorf("domain enum %s.%s: %s is not an enum", table, column, meta.Enum.Proto)
	}
	if string(ed.ParentFile().Package()) != protoPkg {
		return nil, fmt.Errorf("domain enum %s.%s: %s must be in package %s", table, column, meta.Enum.Proto, protoPkg)
	}
	col, ok := cat.column(table, column)
	if !ok {
		return nil, fmt.Errorf("domain enum %s.%s: column not in schema", table, column)
	}
	switch col.Type {
	case "int2", "int4", "int8":
	default:
		return nil, fmt.Errorf("domain enum %s.%s: column type %s is not an integer type", table, column, col.Type)
	}
	e := &ir.Enum{Table: table, Column: column, ProtoEnum: meta.Enum.Proto, Name: string(ed.Name()), DBType: col.Type, OnUnknown: meta.OnUnknown}
	if p, ok := ed.Parent().(protoreflect.MessageDescriptor); ok {
		e.Parent = string(p.Name())
	}
	covered := map[string]bool{}
	for _, dbv := range meta.Enum.SortedDBValues() {
		name := meta.Enum.Values[dbv]
		v := ed.Values().ByName(protoreflect.Name(name))
		if v == nil {
			return nil, fmt.Errorf("domain enum %s.%s: %s is not a value of %s", table, column, name, meta.Enum.Proto)
		}
		if v.Number() == 0 {
			return nil, fmt.Errorf("domain enum %s.%s: %s is the zero value (reserved for UNSPECIFIED)", table, column, name)
		}
		if covered[name] {
			return nil, fmt.Errorf("domain enum %s.%s: %s is mapped from more than one DB value", table, column, name)
		}
		covered[name] = true
		e.Values = append(e.Values, ir.EnumValue{DB: dbv, Name: name, Number: int32(v.Number())})
	}
	for i := 0; i < ed.Values().Len(); i++ {
		v := ed.Values().Get(i)
		if v.Number() == 0 {
			e.Unspecified = string(v.Name())
			continue
		}
		if !covered[string(v.Name())] {
			return nil, fmt.Errorf("domain enum %s.%s: proto value %s has no DB value in the domain layer", table, column, v.Name())
		}
	}
	if e.Unspecified == "" {
		return nil, fmt.Errorf("proto enum %s has no zero value", meta.Enum.Proto)
	}
	return e, nil
}

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}

func storageIR(view string, nk []string, st *spec.Storage) ir.Storage {
	out := ir.Storage{View: view, Table: st.Table, NaturalKey: nk, DataColumn: st.DataColumn, ActionColumn: st.ActionColumn,
		DeleteValue: st.DeleteValue, OrderBy: st.OrderBy}
	if ld := st.LogicalDelete; ld != nil {
		out.LogicalDeleteKey, out.LogicalDeleteValue = ld.Key, ld.Value
	}
	for _, f := range st.Fields {
		out.Fields = append(out.Fields, ir.StorageField{Name: f.Name, Key: f.JSONKey(), Type: f.Type})
	}
	return out
}
