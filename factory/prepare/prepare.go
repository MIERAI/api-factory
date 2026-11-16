// Package prepare 是 Go 运行时模板的适配器: 从式样书确定性地生成所有"不需要 AI"的东西。
//
//  1. 式样书的 query → db/queries/*.sql(list_keyset 会追加游标/ORDER BY/LIMIT,需要先跑一遍 sqlc 拿列类型)
//  2. buf generate(proto → Go 类型 + Connect 接口)
//  3. sqlc generate(SQL → 类型安全的查询函数)
//  4. handler 骨架(struct + 构造函数,未实现的方法默认返回 UNIMPLEMENTED)
//  5. 模式 handler(get_by_id / list_keyset: 映射 + 模式逻辑,完全不需要 AI)
//  6. wiring(把 handler 挂到服务器上)
//
// 换成别的语言时,只需要替换这个包。
package prepare

import (
	"bytes"
	"fmt"
	"go/format"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"text/template"

	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	"apifactory/factory/spec"
)

const modulePath = "apifactory"

func Run(specs []*spec.Spec) error {
	files, err := loadProto()
	if err != nil {
		return err
	}
	methods := map[*spec.Spec]protoreflect.MethodDescriptor{}
	for _, s := range specs {
		p, _ := s.RPCParts()
		md, err := findMethod(files, p.ProtoPackage, p.Service, p.Method)
		if err != nil {
			return fmt.Errorf("%s: api.rpc: %w", s.Path, err)
		}
		methods[s] = md
	}

	// 第 0 遍: 只用探测查询跑 sqlc,拿到 schema 目录(表/列/类型)
	if err := writeProbe(); err != nil {
		return err
	}
	if err := run("sqlc", "generate"); err != nil {
		return err
	}
	cat, err := loadCatalog()
	if err != nil {
		return err
	}
	// 第 1 遍: 声明 → 基础查询
	base := map[*spec.Spec]string{}
	for _, s := range specs {
		sql, err := compileSQL(s, methods[s], cat)
		if err != nil {
			return fmt.Errorf("%s: %w", s.Path, err)
		}
		base[s] = sql
	}
	if err := writeQueries(specs, base, nil); err != nil {
		return err
	}
	if err := run("sqlc", "generate"); err != nil {
		return err
	}
	dbp, err := loadDBPackage("gen/db")
	if err != nil {
		return err
	}
	// 第 2 遍: list_keyset 追加游标条件 / ORDER BY / LIMIT(游标参数的类型来自第 1 遍的结果列)
	if err := writeQueries(specs, base, dbp); err != nil {
		return err
	}
	if err := run("sqlc", "generate"); err != nil {
		return err
	}
	if dbp, err = loadDBPackage("gen/db"); err != nil {
		return err
	}
	if err := run("buf", "generate"); err != nil {
		return err
	}

	services, err := collectServices(specs)
	if err != nil {
		return err
	}
	// 生成的 handler 文件全量重建
	old, _ := filepath.Glob("internal/handler/*/*_gen.go")
	for _, f := range old {
		_ = os.Remove(f)
	}
	for _, svc := range services {
		if err := writeHandlerScaffold(svc); err != nil {
			return err
		}
	}
	for _, s := range specs {
		if s.PatternKind() == "custom" {
			continue
		}
		g, svc, err := newGenCtx(s, services, files, dbp)
		if err != nil {
			return err
		}
		parts := g.parts
		var src string
		switch s.PatternKind() {
		case "get_by_id":
			src, err = genGetByID(g)
		case "list_keyset":
			src, err = genListKeyset(g)
		case "compute":
			src, err = genCompute(g)
			if err == nil {
				stub := s.AIMayEdit()[0]
				if _, statErr := os.Stat(stub); os.IsNotExist(statErr) {
					if err := writeGo(stub, []byte(genLogicStub(g))); err != nil {
						return err
					}
				}
			}
		}
		if err != nil {
			return fmt.Errorf("%s: %w", s.Path, err)
		}
		if err := writeGo(handlerGenPath(svc, parts.Method), []byte(src)); err != nil {
			return err
		}
	}
	return writeWiring(services)
}

// newGenCtx 准备一个 API 的代码生成上下文(proto 描述符 + sqlc 函数签名)。
func newGenCtx(s *spec.Spec, services []service, files *protoregistry.Files, dbp *dbPackage) (*genCtx, service, error) {
	parts, _ := s.RPCParts()
	svc := serviceFor(services, parts)
	md, err := findMethod(files, parts.ProtoPackage, parts.Service, parts.Method)
	if err != nil {
		return nil, svc, fmt.Errorf("%s: %w", s.Path, err)
	}
	q, ok := dbp.funcs[s.QueryName()]
	if !ok {
		return nil, svc, fmt.Errorf("%s: sqlc did not generate %s", s.Path, s.QueryName())
	}
	segs := strings.Split(parts.ProtoPackage, ".")
	g := &genCtx{
		spec: s, parts: parts, svc: svc, method: md, query: q, db: dbp,
		pbAlias: parts.Domain() + segs[len(segs)-1],
		imports: map[string]string{},
	}
	g.use("context", "")
	g.use("connectrpc.com/connect", "")
	g.use(modulePath+"/gen/"+strings.Join(segs, "/"), g.pbAlias)
	return g, svc, nil
}

// clearGenerated 删除 sqlc 的旧输出: sqlc 不会删除已经不存在的查询对应的 .go 文件。
func clearGenerated() {
	old, _ := filepath.Glob("gen/db/*.sql.go")
	for _, f := range old {
		_ = os.Remove(f)
	}
}

func clearQueries() (string, error) {
	clearGenerated()
	dir := "db/queries"
	old, _ := filepath.Glob(filepath.Join(dir, "*.sql"))
	for _, f := range old {
		_ = os.Remove(f) // 全量重新生成,删掉已不存在的式样书留下的查询
	}
	return dir, os.MkdirAll(dir, 0o755)
}

func writeProbe() error {
	dir, err := clearQueries()
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "_factory_probe.sql"), []byte("-- name: FactoryProbe :exec\nSELECT 1;\n"), 0o644)
}

func writeQueries(specs []*spec.Spec, base map[*spec.Spec]string, dbp *dbPackage) error {
	dir, err := clearQueries()
	if err != nil {
		return err
	}
	for _, s := range specs {
		sql := base[s]
		if s.PatternKind() == "list_keyset" && dbp != nil {
			row, _ := dbp.rowFields(dbp.funcs[s.QueryName()])
			if sql, err = keysetSQL(s, sql, row); err != nil {
				return err
			}
		}
		body := fmt.Sprintf("-- Code generated by factory from %s. DO NOT EDIT.\n\n-- name: %s :%s\n%s;\n",
			s.Path, s.QueryName(), s.Cardinality(), sql)
		if err := os.WriteFile(filepath.Join(dir, s.ID()+".sql"), []byte(body), 0o644); err != nil {
			return err
		}
	}
	return nil
}

type service struct {
	Domain       string // jobs
	Service      string // JobService
	ProtoPackage string // factory.jobs.v1
	ConnectPkg   string // jobsv1connect
	ConnectPath  string // apifactory/gen/factory/jobs/v1/jobsv1connect
	HandlerPath  string // apifactory/internal/handler/jobs
}

func serviceFor(services []service, p spec.RPCParts) service {
	for _, s := range services {
		if s.ProtoPackage == p.ProtoPackage && s.Service == p.Service {
			return s
		}
	}
	return service{}
}

func collectServices(specs []*spec.Spec) ([]service, error) {
	byKey := map[string]service{}
	for _, s := range specs {
		p, err := s.RPCParts()
		if err != nil {
			return nil, err
		}
		segs := strings.Split(p.ProtoPackage, ".")
		connectPkg := p.Domain() + segs[len(segs)-1] + "connect"
		svc := service{
			Domain:       p.Domain(),
			Service:      p.Service,
			ProtoPackage: p.ProtoPackage,
			ConnectPkg:   connectPkg,
			ConnectPath:  modulePath + "/gen/" + strings.Join(segs, "/") + "/" + connectPkg,
			HandlerPath:  modulePath + "/internal/handler/" + p.Domain(),
		}
		key := p.ProtoPackage + "." + p.Service
		if prev, ok := byKey[key]; ok && prev != svc {
			return nil, fmt.Errorf("conflicting service definitions for %s", key)
		}
		byKey[key] = svc
	}
	out := make([]service, 0, len(byKey))
	for _, v := range byKey {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ConnectPath < out[j].ConnectPath })
	return out, nil
}

var scaffoldTmpl = template.Must(template.New("").Parse(`// Code generated by factory. DO NOT EDIT.

// Package {{.Domain}} 实现 {{.ProtoPackage}}.{{.Service}}。
// 模式 API 的实现在 *_gen.go(工厂生成);custom API 的实现由 AI 写在各自的文件里。
package {{.Domain}}

import (
	"{{.ConnectPath}}"
	"apifactory/gen/db"
	"apifactory/platform/paging"
)

// Handler 实现 {{.ConnectPkg}}.{{.Service}}Handler。
// 尚未实现的 RPC 由内嵌的 Unimplemented 返回 UNIMPLEMENTED。
type Handler struct {
	{{.ConnectPkg}}.Unimplemented{{.Service}}Handler
	q     *db.Queries
	pager *paging.Pager
}

func New(q *db.Queries, pager *paging.Pager) *Handler {
	return &Handler{q: q, pager: pager}
}
`))

func writeHandlerScaffold(svc service) error {
	dir := filepath.Join("internal/handler", svc.Domain)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return render(scaffoldTmpl, svc, filepath.Join(dir, "handler_gen.go"))
}

var wiringTmpl = template.Must(template.New("").Parse(`// Code generated by factory. DO NOT EDIT.

package main

import (
	"connectrpc.com/connect"

	"apifactory/gen/db"
	"apifactory/platform/server"
{{- range .}}
	"{{.ConnectPath}}"
	{{.Domain}}handler "{{.HandlerPath}}"
{{- end}}
)

func services(deps server.Deps, opts ...connect.HandlerOption) []server.Service {
	q := db.New(deps.Pool)
	var out []server.Service
{{- range .}}
	{
		path, h := {{.ConnectPkg}}.New{{.Service}}Handler({{.Domain}}handler.New(q, deps.Pager), opts...)
		out = append(out, server.Service{Path: path, Handler: h})
	}
{{- end}}
	return out
}
`))

func writeWiring(services []service) error {
	return render(wiringTmpl, services, "cmd/server/wiring_gen.go")
}

func render(t *template.Template, data any, path string) error {
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return err
	}
	return writeGo(path, buf.Bytes())
}

func writeGo(path string, src []byte) error {
	out, err := format.Source(src)
	if err != nil {
		return fmt.Errorf("gofmt %s: %w\n%s", path, err, src)
	}
	return os.WriteFile(path, out, 0o644)
}

func run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return nil
}
