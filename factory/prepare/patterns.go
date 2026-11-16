package prepare

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"google.golang.org/protobuf/reflect/protoreflect"

	"apifactory/factory/spec"
)

// ---------------------------------------------------------------------------
// list_keyset: 把式样书的基础查询包一层,追加游标条件 / ORDER BY / LIMIT
// ---------------------------------------------------------------------------

func keysetSQL(s *spec.Spec, row []goField) (string, error) {
	p := s.Pattern
	op := "<"
	if p.Direction == "asc" {
		op = ">"
	}
	var cols, args, order []string
	for _, c := range p.OrderBy {
		f, ok := fieldByNorm(row, c)
		if !ok {
			return "", fmt.Errorf("%s: order_by column %q is not in the SELECT list", s.Path, c)
		}
		cast, ok := goToSQLCast[f.Type]
		if !ok {
			return "", fmt.Errorf("%s: order_by column %q has type %s (must be NOT NULL and one of time/int/text/bool/float)", s.Path, c, f.Type)
		}
		cols = append(cols, "base."+c)
		args = append(args, fmt.Sprintf("sqlc.narg(cursor_%s)::%s", c, cast))
		order = append(order, "base."+c+" "+strings.ToUpper(p.Direction))
	}
	base := strings.TrimRight(strings.TrimSpace(s.Datasource.Query.SQL), ";")
	first := args[0]
	return fmt.Sprintf("SELECT * FROM (\n%s\n) AS base\nWHERE (%s IS NULL\n       OR (%s) %s (%s))\nORDER BY %s\nLIMIT sqlc.arg(row_limit)::int",
		indent(base), first, strings.Join(cols, ", "), op, strings.Join(args, ", "), strings.Join(order, ", ")), nil
}

func indent(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = "  " + l
	}
	return strings.Join(lines, "\n")
}

func fieldByNorm(fields []goField, name string) (goField, bool) {
	for _, f := range fields {
		if norm(f.Name) == norm(name) {
			return f, true
		}
	}
	return goField{}, false
}

// ---------------------------------------------------------------------------
// handler 代码生成
// ---------------------------------------------------------------------------

type genCtx struct {
	spec    *spec.Spec
	parts   spec.RPCParts
	svc     service
	pbAlias string // jobsv1
	method  protoreflect.MethodDescriptor
	query   queryFunc
	db      *dbPackage
	imports map[string]string // path → alias("" = 默认)
}

func (g *genCtx) use(path, alias string) { g.imports[path] = alias }

func (g *genCtx) pbType(m protoreflect.MessageDescriptor) string {
	return g.pbAlias + "." + string(m.Name())
}

// reqExpr: 请求字段 → 查询参数类型 的转换表达式。
func (g *genCtx) reqExpr(fd protoreflect.FieldDescriptor, paramType string) (string, error) {
	kind, err := protoGoKind(fd)
	if err != nil {
		return "", err
	}
	goName := goCamelCase(string(fd.Name()))
	switch {
	case kind == "ts" && paramType == "*time.Time":
		g.use("apifactory/platform/conv", "")
		return fmt.Sprintf("conv.TimePtr(m.%s)", goName), nil
	case kind == "ts" && paramType == "time.Time":
		return fmt.Sprintf("m.Get%s().AsTime()", goName), nil
	case fd.HasOptionalKeyword() && paramType == "*"+kind:
		return "m." + goName, nil // nil = 未指定 = 不筛选;*0 也是有效值
	case paramType == kind:
		return fmt.Sprintf("m.Get%s()", goName), nil
	}
	return "", fmt.Errorf("request field %s (%s, optional=%v) cannot be mapped to query param type %s",
		fd.Name(), kind, fd.HasOptionalKeyword(), paramType)
}

// resExpr: 查询结果字段 → 响应字段 的转换表达式。
func (g *genCtx) resExpr(src, srcType string, fd protoreflect.FieldDescriptor) (string, error) {
	kind, err := protoGoKind(fd)
	if err != nil {
		return "", err
	}
	switch {
	case kind == "ts" && srcType == "time.Time":
		g.use("apifactory/platform/conv", "")
		return fmt.Sprintf("conv.TS(%s)", src), nil
	case kind == "ts" && srcType == "*time.Time":
		g.use("apifactory/platform/conv", "")
		return fmt.Sprintf("conv.TSPtr(%s)", src), nil
	case srcType == kind:
		return src, nil
	case fd.HasOptionalKeyword() && srcType == "*"+kind:
		return src, nil
	}
	return "", fmt.Errorf("query column %s (%s) cannot be mapped to response field %s (%s)", src, srcType, fd.Name(), kind)
}

// requestField 按名字(或 pattern.mapping)找到查询参数对应的请求字段。
func (g *genCtx) requestField(param string) (protoreflect.FieldDescriptor, error) {
	in := g.method.Input()
	if name, ok := g.spec.Pattern.Mapping[param]; ok {
		if fd := in.Fields().ByName(protoreflect.Name(name)); fd != nil {
			return fd, nil
		}
		return nil, fmt.Errorf("pattern.mapping: request field %q not found", name)
	}
	for i := 0; i < in.Fields().Len(); i++ {
		fd := in.Fields().Get(i)
		if norm(string(fd.Name())) == norm(param) {
			return fd, nil
		}
	}
	return nil, fmt.Errorf("query param %q has no matching field in %s (add pattern.mapping)", param, in.Name())
}

// paramAssignments 生成查询参数结构体的各字段赋值。skip 里的参数由模式本身处理。
func (g *genCtx) paramAssignments(skip func(string) bool) ([]string, error) {
	fields := g.db.structs[g.query.ParamType]
	var lines []string
	for _, f := range fields {
		if skip(f.Name) {
			continue
		}
		fd, err := g.requestField(f.Name)
		if err != nil {
			return nil, err
		}
		e, err := g.reqExpr(fd, f.Type)
		if err != nil {
			return nil, err
		}
		lines = append(lines, fmt.Sprintf("%s: %s,", f.Name, e))
	}
	return lines, nil
}

func (g *genCtx) header(body string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "// Code generated by factory from %s. DO NOT EDIT.\n// pattern: %s\n\npackage %s\n\nimport (\n", g.spec.Path, g.spec.PatternKind(), g.svc.Domain)
	paths := make([]string, 0, len(g.imports))
	for p := range g.imports {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	// 标准库 / 第三方 / 本模块 三组,与 goimports 一致
	groups := [3][]string{}
	for _, p := range paths {
		i := 1
		switch {
		case !strings.Contains(strings.Split(p, "/")[0], "."):
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
	in, out := g.method.Input(), g.method.Output()
	g.use("apifactory/platform/apierr", "")

	// 查询参数
	var call string
	switch {
	case g.query.ParamType == "":
		call = fmt.Sprintf("h.q.%s(ctx)", g.spec.Datasource.Query.Name)
	case g.db.structs[g.query.ParamType] != nil:
		g.use("apifactory/gen/db", "")
		lines, err := g.paramAssignments(func(string) bool { return false })
		if err != nil {
			return "", err
		}
		call = fmt.Sprintf("h.q.%s(ctx, db.%s{\n%s\n})", g.spec.Datasource.Query.Name, g.query.ParamType, strings.Join(lines, "\n"))
	default:
		fd, err := g.requestField(g.query.ParamName)
		if err != nil {
			return "", err
		}
		e, err := g.reqExpr(fd, g.query.ParamType)
		if err != nil {
			return "", err
		}
		call = fmt.Sprintf("h.q.%s(ctx, %s)", g.spec.Datasource.Query.Name, e)
	}

	// NOT_FOUND 消息: {field} → %v
	var nfArgs []string
	var nfErr error
	nfFormat := placeholderRe.ReplaceAllStringFunc(g.spec.Pattern.NotFoundMessage, func(m string) string {
		name := m[1 : len(m)-1]
		if in.Fields().ByName(protoreflect.Name(name)) == nil {
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

	// 响应
	row, _ := g.db.rowFields(g.query)
	var resLines []string
	if row == nil { // 标量结果 → 响应必须只有一个字段
		if out.Fields().Len() != 1 {
			return "", fmt.Errorf("query returns a single column but %s has %d fields", out.Name(), out.Fields().Len())
		}
		fd := out.Fields().Get(0)
		e, err := g.resExpr("row", g.query.ReturnType, fd)
		if err != nil {
			return "", err
		}
		resLines = append(resLines, fmt.Sprintf("%s: %s,", goCamelCase(string(fd.Name())), e))
	} else {
		for i := 0; i < out.Fields().Len(); i++ {
			fd := out.Fields().Get(i)
			f, ok := fieldByNorm(row, string(fd.Name()))
			if !ok {
				return "", fmt.Errorf("response field %s has no matching query column", fd.Name())
			}
			e, err := g.resExpr("row."+f.Name, f.Type, fd)
			if err != nil {
				return "", err
			}
			resLines = append(resLines, fmt.Sprintf("%s: %s,", goCamelCase(string(fd.Name())), e))
		}
	}

	body := fmt.Sprintf(`// %s %s
func (h *Handler) %s(ctx context.Context, req *connect.Request[%s]) (*connect.Response[%s], error) {
	m := req.Msg
	row, err := %s
	if err != nil {
		if apierr.IsNoRows(err) {
			return nil, apierr.NotFound(%s)
		}
		return nil, err
	}
	return connect.NewResponse(&%s{
		%s
	}), nil
}
`, g.parts.Method, oneLine(g.spec.API.Summary), g.parts.Method, g.pbType(in), g.pbType(out), call, nf, g.pbType(out), strings.Join(resLines, "\n"))
	return g.header(body), nil
}

func genListKeyset(g *genCtx) (string, error) {
	in, out := g.method.Input(), g.method.Output()
	p := g.spec.Pattern
	g.use("apifactory/platform/apierr", "")
	g.use("apifactory/platform/paging", "")
	g.use("apifactory/gen/db", "")

	// AIP-158 约定的分页字段
	for _, name := range []string{"page_size", "page_token"} {
		if in.Fields().ByName(protoreflect.Name(name)) == nil {
			return "", fmt.Errorf("pattern list_keyset: %s must have field %s", in.Name(), name)
		}
	}
	if out.Fields().ByName("next_page_token") == nil {
		return "", fmt.Errorf("pattern list_keyset: %s must have field next_page_token", out.Name())
	}
	var items protoreflect.FieldDescriptor
	for i := 0; i < out.Fields().Len(); i++ {
		fd := out.Fields().Get(i)
		if fd.IsList() && fd.Kind() == protoreflect.MessageKind {
			if items != nil {
				return "", fmt.Errorf("pattern list_keyset: %s must have exactly one repeated message field", out.Name())
			}
			items = fd
		}
	}
	if items == nil {
		return "", fmt.Errorf("pattern list_keyset: %s has no repeated message field", out.Name())
	}
	if g.db.structs[g.query.ParamType] == nil {
		return "", fmt.Errorf("pattern list_keyset: query %s must take a params struct", g.spec.Datasource.Query.Name)
	}
	row, rowType := g.db.rowFields(g.query)
	if row == nil {
		return "", fmt.Errorf("pattern list_keyset: query must return rows, got %s", rowType)
	}

	// 游标
	cursorType := lowerFirst(g.parts.Method) + "Cursor"
	var cursorFields, cursorSet, cursorBuild []string
	cursorParams := map[string]bool{"RowLimit": true}
	for _, c := range p.OrderBy {
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
	elem := items.Message()
	var elemLines []string
	for i := 0; i < elem.Fields().Len(); i++ {
		fd := elem.Fields().Get(i)
		f, ok := fieldByNorm(row, string(fd.Name()))
		if !ok {
			return "", fmt.Errorf("response field %s.%s has no matching query column", elem.Name(), fd.Name())
		}
		e, err := g.resExpr("r."+f.Name, f.Type, fd)
		if err != nil {
			return "", err
		}
		elemLines = append(elemLines, fmt.Sprintf("%s: %s,", goCamelCase(string(fd.Name())), e))
	}
	itemsGo := goCamelCase(string(items.Name()))

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
	rows, err := h.q.%s(ctx, params)
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
		g.parts.Method, oneLine(g.spec.API.Summary), g.parts.Method, g.pbType(in), g.pbType(out),
		p.PageSize.Default, p.PageSize.Max,
		g.query.ParamType, strings.Join(assigns, "\n"),
		cursorType, strings.Join(cursorSet, "\n"),
		g.spec.Datasource.Query.Name,
		g.pbType(out), itemsGo, g.pbType(elem),
		itemsGo, itemsGo, g.pbType(elem), strings.Join(elemLines, "\n"),
		cursorType, strings.Join(cursorBuild, "\n"))
	return g.header(body), nil
}

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
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
