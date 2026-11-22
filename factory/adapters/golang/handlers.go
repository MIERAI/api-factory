package golang

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"apifactory/factory/core"
	"apifactory/factory/ir"
)

// ---------------------------------------------------------------------------
// handler 代码生成(输入: IR + sqlc 生成的 Go 代码的结构)
// ---------------------------------------------------------------------------

type genCtx struct {
	api     *ir.API
	svc     service
	pbAlias string // jobsv1
	query   queryFunc
	db      *dbPackage
	imports map[string]string // path → alias("" = 默认)
	// 领域层的 enum: 列名 → 映射(只限 datasource.table)
	enums map[string]*enumMapping
	// 请求字段名(norm) → 列名(声明式 datasource 的筛选条件)
	filterCol map[string]string
}

func (g *genCtx) use(path, alias string) { g.imports[path] = alias }

func (g *genCtx) pbType(m *ir.Message) string { return g.pbAlias + "." + m.Name }

// enumFor: 列对应的 enum 映射;proto 是 enum 但领域层没有映射时报错。
func (g *genCtx) enumFor(column string) (*enumMapping, error) {
	if src, ok := g.api.Rename[column]; ok { // 输出改名: 枚举的映射按来源的列
		column = src
	}
	if m, ok := g.enums[column]; ok {
		return m, nil
	}
	return nil, fmt.Errorf("column %q is mapped to a proto enum but the domain layer has no enum mapping for %s.%s", column, g.api.Table, column)
}

// goKind: IR 的字段类型 → Go 的基础类型;Timestamp 返回 "ts"。
func goKind(f *ir.Field) (string, error) {
	if f.List {
		return "", fmt.Errorf("field %s: repeated is not supported by mapping", f.Name)
	}
	switch f.Kind {
	case "int32", "int64", "uint32", "uint64", "string", "bool":
		return f.Kind, nil
	case "double":
		return "float64", nil
	case "float":
		return "float32", nil
	case "timestamp":
		return "ts", nil
	}
	return "", fmt.Errorf("field %s: kind %s is not supported by mapping", f.Name, f.Kind)
}

// reqExpr: 请求字段 → 查询参数类型 的转换表达式。
func (g *genCtx) reqExpr(f *ir.Field, paramType string) (string, error) {
	goName := goCamelCase(f.Name)
	if f.List { // 列表参数: []string / []int32 / []int64 / 日期的列表(枚举的列表还不支持)
		if k, err := goKind(&ir.Field{Name: f.Name, Kind: f.Kind}); err == nil && paramType == "[]"+k {
			return fmt.Sprintf("m.Get%s()", goName), nil
		}
		if f.Kind == "enum" { // 枚举的列表: 每个值 proto → DB
			column := f.Name
			if c, ok := g.filterCol[norm(column)]; ok {
				column = c
			}
			m, err := g.enumFor(column)
			if err != nil {
				return "", err
			}
			if paramType != "[]"+m.DBGoType {
				return "", fmt.Errorf("enum list request field %s cannot be mapped to query param type %s", f.Name, paramType)
			}
			return fmt.Sprintf("func() []%[1]s {\n\t\tout := make([]%[1]s, 0, len(m.Get%[2]s()))\n\t\tfor _, v := range m.Get%[2]s() {\n\t\t\tout = append(out, %[3]sToDB(v))\n\t\t}\n\t\treturn out\n\t}()", m.DBGoType, goName, m.Helper), nil
		}
		if f.Kind == "string" && paramType == "[]time.Time" { // 日期 "YYYY-MM-DD" 的列表
			g.use(runtimeModule+"/conv", "")
			return fmt.Sprintf("conv.Dates(m.Get%s())", goName), nil
		}
		return "", fmt.Errorf("list request field %s (%s) cannot be mapped to query param type %s", f.Name, f.Kind, paramType)
	}
	if f.Kind == "string" && (paramType == "time.Time" || paramType == "*time.Time") { // 日期 "YYYY-MM-DD"
		g.use(runtimeModule+"/conv", "")
		if paramType == "*time.Time" {
			return fmt.Sprintf("conv.DatePtr(m.%s)", goName), nil
		}
		return fmt.Sprintf("conv.Date(m.Get%s())", goName), nil
	}
	if f.Kind == "enum" && !f.List {
		column := f.Name
		if c, ok := g.filterCol[norm(column)]; ok {
			column = c
		}
		m, err := g.enumFor(column)
		if err != nil {
			return "", err
		}
		switch {
		case f.Optional && paramType == "*"+m.DBGoType:
			return fmt.Sprintf("%sPtrToDB(m.%s)", m.Helper, goName), nil
		case !f.Optional && paramType == m.DBGoType:
			return fmt.Sprintf("%sToDB(m.Get%s())", m.Helper, goName), nil
		}
		return "", fmt.Errorf("enum request field %s (optional=%v) cannot be mapped to query param type %s", f.Name, f.Optional, paramType)
	}
	kind, err := goKind(f)
	if err != nil {
		return "", err
	}
	switch {
	case kind == "ts" && paramType == "*time.Time":
		g.use(runtimeModule+"/conv", "")
		return fmt.Sprintf("conv.TimePtr(m.%s)", goName), nil
	case kind == "ts" && paramType == "time.Time":
		return fmt.Sprintf("m.Get%s().AsTime()", goName), nil
	case f.Optional && paramType == "*"+kind:
		return "m." + goName, nil // nil = 未指定 = 不筛选;*0 也是有效值
	case paramType == kind:
		return fmt.Sprintf("m.Get%s()", goName), nil
	}
	return "", fmt.Errorf("request field %s (%s, optional=%v) cannot be mapped to query param type %s", f.Name, kind, f.Optional, paramType)
}

// resExpr: 查询结果字段 → 响应字段 的转换表达式。
func (g *genCtx) resExpr(src, srcType string, f *ir.Field) (string, error) {
	if f.Kind == "enum" && !f.List {
		m, err := g.enumFor(f.Name)
		if err != nil {
			return "", err
		}
		if srcType != m.DBGoType {
			return "", fmt.Errorf("query column %s (%s) cannot be mapped to enum field %s (DB type %s)", src, srcType, f.Name, m.DBGoType)
		}
		return fmt.Sprintf("%sToProto(%s)", m.Helper, src), nil
	}
	kind, err := goKind(f)
	if err != nil {
		return "", err
	}
	switch {
	case kind == "string" && srcType == "time.Time" && f.Optional && !f.List: // 列不为空,响应字段可选
		g.use(runtimeModule+"/conv", "")
		return fmt.Sprintf("conv.Ptr(conv.DateString(%s))", src), nil
	case kind == "string" && srcType == "time.Time": // date 列 → "YYYY-MM-DD"
		g.use(runtimeModule+"/conv", "")
		return fmt.Sprintf("conv.DateString(%s)", src), nil
	case kind == "string" && srcType == "*time.Time" && f.Optional:
		g.use(runtimeModule+"/conv", "")
		return fmt.Sprintf("conv.DateStringPtr(%s)", src), nil
	case kind == "ts" && srcType == "time.Time":
		g.use(runtimeModule+"/conv", "")
		return fmt.Sprintf("conv.TS(%s)", src), nil
	case kind == "ts" && srcType == "*time.Time":
		g.use(runtimeModule+"/conv", "")
		return fmt.Sprintf("conv.TSPtr(%s)", src), nil
	case srcType == kind && f.Optional && !f.List: // 列不为空(类型收窄),响应字段是可选的(可空性来自表定义)
		g.use(runtimeModule+"/conv", "")
		return fmt.Sprintf("conv.Ptr(%s)", src), nil
	case srcType == kind:
		return src, nil
	case f.Optional && srcType == "*"+kind:
		return src, nil
	}
	return "", fmt.Errorf("query column %s (%s) cannot be mapped to response field %s (%s)", src, srcType, f.Name, kind)
}

// memberExpr: 组的列表(in_tuples)的一个字段 → 并行的列表参数(第 i 个 = 第 i 个组的这个字段)。
func (g *genCtx) memberExpr(param, paramType string) (string, bool, error) {
	for _, flt := range g.api.Filters {
		if flt.Op != "in_tuples" {
			continue
		}
		for _, col := range flt.Columns {
			if norm(ir.MemberParam(flt.Field, col)) != norm(param) {
				continue
			}
			rf, err := g.requestField(flt.Field)
			if err != nil {
				return "", true, err
			}
			if !rf.List || rf.Message == nil {
				return "", true, fmt.Errorf("request field %s: in_tuples needs a repeated message", flt.Field)
			}
			var mf *ir.Field
			for i := range rf.Message.Fields {
				if rf.Message.Fields[i].Name == col {
					mf = &rf.Message.Fields[i]
				}
			}
			if mf == nil {
				return "", true, fmt.Errorf("request field %s has no member %s", flt.Field, col)
			}
			k, err := goKind(mf)
			if err != nil {
				return "", true, err
			}
			get := fmt.Sprintf("x.Get%s()", goCamelCase(col))
			elem := strings.TrimPrefix(paramType, "[]")
			switch {
			case k == elem:
			case k == "string" && elem == "time.Time": // 日期 "YYYY-MM-DD"
				g.use(runtimeModule+"/conv", "")
				get = fmt.Sprintf("conv.Date(%s)", get)
			case k == "ts" && elem == "time.Time":
				get += ".AsTime()"
			default:
				return "", true, fmt.Errorf("member %s.%s (%s) cannot be mapped to query param type %s", flt.Field, col, k, paramType)
			}
			return fmt.Sprintf("func() %[1]s {\n\t\tout := make(%[1]s, 0, len(m.Get%[2]s()))\n\t\tfor _, x := range m.Get%[2]s() {\n\t\t\tout = append(out, %[3]s)\n\t\t}\n\t\treturn out\n\t}()", paramType, goCamelCase(flt.Field), get), true, nil
		}
	}
	return "", false, nil
}

// requestField 按名字找到查询参数对应的请求字段。
func (g *genCtx) requestField(param string) (*ir.Field, error) {
	for i := range g.api.Request.Fields {
		if f := &g.api.Request.Fields[i]; norm(f.Name) == norm(param) {
			return f, nil
		}
	}
	return nil, fmt.Errorf("query param %q has no matching field in %s (with datasource.sql, name params after request fields)", param, g.api.Request.Name)
}

// paramAssignments 生成查询参数结构体的各字段赋值。skip 里的参数由模式本身处理。
func (g *genCtx) paramAssignments(skip func(string) bool) ([]string, error) {
	var lines []string
	for _, f := range g.db.structs[g.query.ParamType] {
		if skip(f.Name) {
			continue
		}
		if norm(f.Name) == norm(core.NowParam) { // now(): 请求开始的时刻(运行时的时钟;测试时由请求头固定),不来自请求
			if f.Type != "time.Time" {
				return nil, fmt.Errorf("query param %s: type %s, want time.Time", f.Name, f.Type)
			}
			g.use(runtimeModule+"/clock", "")
			lines = append(lines, fmt.Sprintf("%s: clock.Now(ctx),", f.Name))
			continue
		}
		if e, ok, err := g.memberExpr(f.Name, f.Type); ok || err != nil { // 组的列表的一个字段
			if err != nil {
				return nil, err
			}
			lines = append(lines, fmt.Sprintf("%s: %s,", f.Name, e))
			continue
		}
		rf, err := g.requestField(f.Name)
		if err != nil {
			return nil, err
		}
		e, err := g.reqExpr(rf, f.Type)
		if err != nil {
			return nil, err
		}
		lines = append(lines, fmt.Sprintf("%s: %s,", f.Name, e))
	}
	return lines, nil
}

// queryCall: 调用 sqlc 生成的查询函数(无参数 / 参数结构体 / 单个参数)。
func (g *genCtx) queryCall() (string, error) {
	call, err := g.queryCall0()
	if err != nil {
		return "", err
	}
	ref, err := g.queries()
	if err != nil {
		return "", err
	}
	return strings.Replace(call, "h.q.", ref+".", 1), nil
}

// queries: 查询用的连接。分库的表 = 按分库键选(分库键由必填的「等于」固定,编译时检查过);否则主库。
// 所有出口的 handler 都要经过这里(分页曾经直接写 h.q,分库 + 分页不选库: 矩阵发现)。
func (g *genCtx) queries() (string, error) {
	if g.api.Shard == nil {
		return "h.q", nil
	}
	rf, err := g.requestField(g.api.Shard.Field)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("h.at(int64(m.Get%s()))", goCamelCase(rf.Name)), nil
}

func (g *genCtx) queryCall0() (string, error) {
	name := g.api.Query.Name
	switch {
	case g.query.ParamType == "":
		return fmt.Sprintf("h.q.%s(ctx)", name), nil
	case g.db.structs[g.query.ParamType] != nil:
		g.use(modulePath+"/gen/db", "")
		lines, err := g.paramAssignments(func(string) bool { return false })
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("h.q.%s(ctx, db.%s{\n%s\n})", name, g.query.ParamType, strings.Join(lines, "\n")), nil
	case norm(g.query.ParamName) == norm(core.NowParam): // 唯一的参数是 now()
		g.use(runtimeModule+"/clock", "")
		return fmt.Sprintf("h.q.%s(ctx, clock.Now(ctx))", name), nil
	default:
		rf, err := g.requestField(g.query.ParamName)
		if err != nil {
			return "", err
		}
		e, err := g.reqExpr(rf, g.query.ParamType)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("h.q.%s(ctx, %s)", name, e), nil
	}
}

func (g *genCtx) header(body string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "// Code generated by factory from %s. DO NOT EDIT.\n// pattern: %s\n\npackage %s\n\nimport (\n", g.api.SpecPath, g.api.Pattern, g.svc.Domain)
	paths := make([]string, 0, len(g.imports))
	for p := range g.imports {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	// 标准库 / 第三方 / 本模块 三组,与 goimports 一致
	groups := [3][]string{}
	for _, p := range paths {
		i := 1
		if !strings.Contains(strings.Split(p, "/")[0], ".") {
			i = 0
			if strings.HasPrefix(p, modulePath+"/") {
				i = 2
			}
		}
		groups[i] = append(groups[i], p)
	}
	for gi, grp := range groups {
		if len(grp) == 0 {
			continue
		}
		if gi > 0 && b.Len() > 0 && !strings.HasSuffix(b.String(), "(\n") {
			b.WriteString("\n")
		}
		for _, p := range grp {
			if a := g.imports[p]; a != "" {
				fmt.Fprintf(&b, "\t%s %q\n", a, p)
			} else {
				fmt.Fprintf(&b, "\t%q\n", p)
			}
		}
	}
	b.WriteString(")\n\n")
	b.WriteString(body)
	return b.String()
}

var placeholderRe = regexp.MustCompile(`\{([a-z0-9_]+)\}`)

func genGetByID(g *genCtx) (string, error) {
	in, out := &g.api.Request, &g.api.Response
	g.use(runtimeModule+"/apierr", "")

	call, err := g.queryCall()
	if err != nil {
		return "", err
	}

	// NOT_FOUND 消息: {field} → %v
	var nfArgs []string
	var nfErr error
	nfFormat := placeholderRe.ReplaceAllStringFunc(g.api.GetByID.NotFoundMessage, func(m string) string {
		name := m[1 : len(m)-1]
		if in.Field(name) == nil {
			nfErr = fmt.Errorf("not_found_message: {%s} is not a request field", name)
		}
		nfArgs = append(nfArgs, fmt.Sprintf("m.Get%s()", goCamelCase(name)))
		return "%v"
	})
	if nfErr != nil {
		return "", nfErr
	}
	nf := fmt.Sprintf("%q", nfFormat)
	if len(nfArgs) > 0 {
		nf += ", " + strings.Join(nfArgs, ", ")
	}
	notFound := fmt.Sprintf("return nil, apierr.NotFound(%s)", nf)
	if g.api.GetByID.NotFoundEmpty { // 找不到时: 返回空(found = false,其他字段不设置)
		notFound = fmt.Sprintf("return connect.NewResponse(&%s{}), nil", g.pbType(out))
	}

	// 响应
	row, _ := g.db.rowFields(g.query)
	var resLines []string
	if row == nil { // 标量结果 → 响应必须只有一个字段
		if len(out.Fields) != 1 {
			return "", fmt.Errorf("query returns a single column but %s has %d fields", out.Name, len(out.Fields))
		}
		f := &out.Fields[0]
		e, err := g.resExpr("row", g.query.ReturnType, f)
		if err != nil {
			return "", err
		}
		resLines = append(resLines, fmt.Sprintf("%s: %s,", goCamelCase(f.Name), e))
	} else {
		for i := range out.Fields {
			f := &out.Fields[i]
			if g.api.GetByID.NotFoundEmpty && f.Name == "found" { // 找不到时: 返回空
				resLines = append(resLines, "Found: true,")
				continue
			}
			col, ok := fieldByNorm(row, f.Name)
			if !ok {
				return "", fmt.Errorf("response field %s has no matching query column", f.Name)
			}
			e, err := g.resExpr("row."+col.Name, col.Type, f)
			if err != nil {
				return "", err
			}
			resLines = append(resLines, fmt.Sprintf("%s: %s,", goCamelCase(f.Name), e))
		}
	}

	body := fmt.Sprintf(`// %s %s
func (h *Handler) %s(ctx context.Context, req *connect.Request[%s]) (*connect.Response[%s], error) {
	m := req.Msg
	row, err := %s
	if err != nil {
		if apierr.IsNoRows(err) {
			%s
		}
		return nil, err
	}
	return connect.NewResponse(&%s{
		%s
	}), nil
}
`, g.api.Method, g.api.Summary, g.api.Method, g.pbType(in), g.pbType(out), call, notFound, g.pbType(out), strings.Join(resLines, "\n"))
	return g.header(body), nil
}

func genListKeyset(g *genCtx) (string, error) {
	in, out := &g.api.Request, &g.api.Response
	k := g.api.Keyset
	g.use(runtimeModule+"/apierr", "")
	g.use(runtimeModule+"/paging", "")
	g.use(modulePath+"/gen/db", "")

	items := out.Field(k.ItemsField)
	if items == nil || items.Message == nil {
		return "", fmt.Errorf("pattern list_keyset: %s has no repeated message field %s", out.Name, k.ItemsField)
	}
	if g.db.structs[g.query.ParamType] == nil {
		return "", fmt.Errorf("pattern list_keyset: query %s must take a params struct", g.api.Query.Name)
	}
	row, rowType := g.db.rowFields(g.query)
	if row == nil {
		return "", fmt.Errorf("pattern list_keyset: query must return rows, got %s", rowType)
	}

	// 游标
	cursorType := lowerFirst(g.api.Method) + "Cursor"
	var cursorFields, cursorSet, cursorBuild []string
	cursorParams := map[string]bool{"RowLimit": true}
	for _, c := range k.OrderBy {
		f, _ := fieldByNorm(row, c)
		if f.Type == "time.Time" {
			g.use("time", "")
		}
		cursorFields = append(cursorFields, fmt.Sprintf("%s %s `json:%q`", f.Name, f.Type, c))
		pf, ok := fieldByNorm(g.db.structs[g.query.ParamType], "cursor_"+c)
		if !ok {
			return "", fmt.Errorf("pattern list_keyset: generated query has no cursor_%s param", c)
		}
		cursorParams[pf.Name] = true
		cursorSet = append(cursorSet, fmt.Sprintf("params.%s = &c.%s", pf.Name, f.Name))
		cursorBuild = append(cursorBuild, fmt.Sprintf("%s: last.%s,", f.Name, f.Name))
	}

	assigns, err := g.paramAssignments(func(name string) bool { return cursorParams[name] })
	if err != nil {
		return "", err
	}

	// 行 → 响应元素
	elem := items.Message
	var elemLines []string
	for i := range elem.Fields {
		f := &elem.Fields[i]
		col, ok := fieldByNorm(row, f.Name)
		if !ok {
			return "", fmt.Errorf("response field %s.%s has no matching query column", elem.Name, f.Name)
		}
		e, err := g.resExpr("r."+col.Name, col.Type, f)
		if err != nil {
			return "", err
		}
		elemLines = append(elemLines, fmt.Sprintf("%s: %s,", goCamelCase(f.Name), e))
	}
	itemsGo := goCamelCase(items.Name)

	qref, err := g.queries()
	if err != nil {
		return "", err
	}
	body := fmt.Sprintf(`// %s 是 page_token 里携带的游标(本页最后一行的 order_by 列)。
type %s struct {
	%s
}

// %s %s
func (h *Handler) %s(ctx context.Context, req *connect.Request[%s]) (*connect.Response[%s], error) {
	m := req.Msg
	size := paging.Size(m.GetPageSize(), %d, %d)
	fp := paging.Fingerprint(m, "page_token", "page_size")
	params := db.%s{
		%s
		RowLimit: size + 1,
	}
	if tok := m.GetPageToken(); tok != "" {
		var c %s
		if err := h.pager.Decode(tok, fp, &c); err != nil {
			return nil, apierr.InvalidArgument("invalid page_token")
		}
		%s
	}
	rows, err := %s(ctx, params)
	if err != nil {
		return nil, err
	}
	rows, hasNext := paging.Trim(rows, size)
	res := &%s{%s: make([]*%s, 0, len(rows))}
	for _, r := range rows {
		res.%s = append(res.%s, &%s{
			%s
		})
	}
	if hasNext {
		last := rows[len(rows)-1]
		tok, err := h.pager.Encode(fp, %s{
			%s
		})
		if err != nil {
			return nil, err
		}
		res.NextPageToken = tok
	}
	return connect.NewResponse(res), nil
}
`, cursorType, cursorType, strings.Join(cursorFields, "\n"),
		g.api.Method, g.api.Summary, g.api.Method, g.pbType(in), g.pbType(out),
		k.DefaultSize, k.MaxSize,
		g.query.ParamType, strings.Join(assigns, "\n"),
		cursorType, strings.Join(cursorSet, "\n"),
		qref+"."+g.api.Query.Name,
		g.pbType(out), itemsGo, g.pbType(elem),
		itemsGo, itemsGo, g.pbType(elem), strings.Join(elemLines, "\n"),
		cursorType, strings.Join(cursorBuild, "\n"))
	return g.header(body), nil
}

// genCompute: handler = 映射 + 查询 + 调用 AI 写的纯函数。
func genCompute(g *genCtx) (string, error) {
	in, out := &g.api.Request, &g.api.Response
	call, err := g.queryCall()
	if err != nil {
		return "", err
	}
	// 逻辑函数的输入行: enum 列按领域层转换成 proto enum(未知值 → UNSPECIFIED + 指标),
	// 逻辑代码不依赖 DB 的编码,也不 import gen/db。
	rowDecl, convert, err := g.logicRows()
	if err != nil {
		return "", err
	}
	fn := g.api.Compute.LogicFunction
	body := fmt.Sprintf(`%s
// %s %s
// 业务逻辑在 %s(纯函数,由 AI 实现)。
func (h *Handler) %s(ctx context.Context, req *connect.Request[%s]) (*connect.Response[%s], error) {
	m := req.Msg
	rows, err := %s
	if err != nil {
		return nil, err
	}
	%s
	res, err := %s(m, in)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(res), nil
}
`, rowDecl, g.api.Method, g.api.Summary, fn, g.api.Method, g.pbType(in), g.pbType(out), call, convert, fn)
	return g.header(body), nil
}

func (g *genCtx) logicRowType() string { return lowerFirst(g.api.Method) + "Row" }

// logicRows: 逻辑输入行的类型声明 + 从 sqlc 行转换的代码。
func (g *genCtx) logicRows() (decl, convert string, err error) {
	row, _ := g.db.rowFields(g.query)
	if row == nil || !strings.HasPrefix(g.query.ReturnType, "[]") {
		return "", "", fmt.Errorf("pattern compute: query must return rows (got %s)", g.query.ReturnType)
	}
	var fields, assigns []string
	for _, f := range row {
		typ, expr := f.Type, "r."+f.Name
		col := snake(f.Name)
		for c := range g.enums {
			if norm(c) == norm(f.Name) {
				col = c
			}
		}
		if m, ok := g.enums[col]; ok {
			if f.Type != m.DBGoType {
				return "", "", fmt.Errorf("enum column %s: nullable enum columns are not supported yet (type %s)", col, f.Type)
			}
			typ, expr = m.GoType, fmt.Sprintf("%sToProto(r.%s)", m.Helper, f.Name)
		}
		if strings.Contains(typ, "time.") {
			g.use("time", "")
		}
		fields = append(fields, fmt.Sprintf("%s %s", f.Name, typ))
		assigns = append(assigns, fmt.Sprintf("%s: %s,", f.Name, expr))
	}
	t := g.logicRowType()
	fn := g.api.Compute.LogicFunction
	decl = fmt.Sprintf("// %s: %s 的输入行(enum 列已按领域层转换成 proto enum)。\ntype %s struct {\n%s\n}\n", t, fn, t, strings.Join(fields, "\n"))
	convert = fmt.Sprintf("in := make([]%s, 0, len(rows))\n\tfor _, r := range rows {\n\t\tin = append(in, %s{\n%s\n})\n\t}", t, t, strings.Join(assigns, "\n"))
	return decl, convert, nil
}

// LogicSignature 返回 compute 模式里 AI 要实现的函数签名(prompt 和桩共用)。
func LogicSignature(g *genCtx) string {
	return fmt.Sprintf("func %s(req *%s, rows %s) (*%s, error)",
		g.api.Compute.LogicFunction, g.pbType(&g.api.Request), "[]"+g.logicRowType(), g.pbType(&g.api.Response))
}

// genLogicStub: 逻辑文件不存在时生成桩(返回 UNIMPLEMENTED),AI 在此基础上实现。
func genLogicStub(g *genCtx) string {
	g2 := &genCtx{api: g.api, svc: g.svc, query: g.query, db: g.db, pbAlias: g.pbAlias, imports: map[string]string{}}
	g2.use("errors", "")
	g2.use("connectrpc.com/connect", "")
	g2.use(modulePath+"/gen/"+strings.ReplaceAll(g.api.ProtoPackage, ".", "/"), g.pbAlias)
	fn := g.api.Compute.LogicFunction
	body := fmt.Sprintf(`// %s 实现 %s 的业务逻辑(式样书 behavior 节)。
// 纯函数: 不访问 DB/网络/文件,不用 context,不调用 time.Now,不用全局可变状态。
%s {
	return nil, connect.NewError(connect.CodeUnimplemented, errors.New("%s: not implemented"))
}
`, fn, g.api.Method, LogicSignature(g), fn)
	src := g2.header(body)
	// 桩不是生成物(AI 会改写),去掉 DO NOT EDIT 标记
	return strings.Replace(src, "// Code generated by factory from "+g.api.SpecPath+". DO NOT EDIT.\n// pattern: compute\n", "// Logic for "+g.api.SpecPath+" — implemented by AI.\n", 1)
}

func fieldByNorm(fields []goField, name string) (goField, bool) {
	for _, f := range fields {
		if norm(f.Name) == norm(name) {
			return f, true
		}
	}
	return goField{}, false
}

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}

func snake(s string) string {
	var b strings.Builder
	for i, r := range s {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				b.WriteByte('_')
			}
			r += 'a' - 'A'
		}
		b.WriteRune(r)
	}
	return b.String()
}

func handlerGenPath(svc service, method string) string {
	return filepath.Join("internal/handler", svc.Domain, snake(method)+"_gen.go")
}

// genListAll: 多条(不分页)。查询取 上限+1 行,超过上限时报错(不悄悄截断: 截断的结果会被当成"全部")。
func genListAll(g *genCtx) (string, error) {
	return genRowsList(g, g.api.ListAll.ItemsField, g.api.ListAll.MaxRows)
}

// genRowsList: 响应 = 行的列表(多条 / 前N条 …)。maxRows > 0: 超过时报错(查询多取了一行)。
func genRowsList(g *genCtx, itemsField string, maxRows int32) (string, error) {
	in, out := &g.api.Request, &g.api.Response
	items := out.Field(itemsField)
	if items == nil || items.Message == nil {
		return "", fmt.Errorf("pattern %s: %s has no repeated message field %s", g.api.Pattern, out.Name, itemsField)
	}
	call, err := g.queryCall()
	if err != nil {
		return "", err
	}
	row, rowType := g.db.rowFields(g.query)
	if row == nil {
		return "", fmt.Errorf("pattern %s: query must return rows, got %s", g.api.Pattern, rowType)
	}
	elem := items.Message
	var elemLines []string
	for i := range elem.Fields {
		f := &elem.Fields[i]
		col, ok := fieldByNorm(row, f.Name)
		if !ok {
			return "", fmt.Errorf("response field %s.%s has no matching query column", elem.Name, f.Name)
		}
		e, err := g.resExpr("r."+col.Name, col.Type, f)
		if err != nil {
			return "", err
		}
		elemLines = append(elemLines, fmt.Sprintf("%s: %s,", goCamelCase(f.Name), e))
	}
	itemsGo := goCamelCase(items.Name)
	guard := ""
	if maxRows > 0 {
		g.use(runtimeModule+"/apierr", "")
		guard = fmt.Sprintf("\tif len(rows) > %d {\n\t\treturn nil, apierr.FailedPrecondition(\"too many results (more than %d); narrow the conditions\")\n\t}\n", maxRows, maxRows)
	}
	msgLine := "m := req.Msg"
	if !strings.Contains(call, "m.") { // 没有输入的 API
		msgLine = "_ = req"
	}
	body := fmt.Sprintf(`// %s %s
func (h *Handler) %s(ctx context.Context, req *connect.Request[%s]) (*connect.Response[%s], error) {
	`+msgLine+`
	rows, err := %s
	if err != nil {
		return nil, err
	}
%s	res := &%s{%s: make([]*%s, 0, len(rows))}
	for _, r := range rows {
		res.%s = append(res.%s, &%s{
			%s
		})
	}
	return connect.NewResponse(res), nil
}
`, g.api.Method, g.api.Summary, g.api.Method, g.pbType(in), g.pbType(out), call, guard,
		g.pbType(out), itemsGo, g.pbType(elem), itemsGo, itemsGo, g.pbType(elem), strings.Join(elemLines, "\n"))
	return g.header(body), nil
}
