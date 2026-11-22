// Package golang 是 Go 运行时模板的适配器: IR → Go 代码。
//
// 输入只有 IR(.factory/plan.json)和编译器写好的 db/queries/*.sql。自己负责 Go 的工具链:
//  1. gen/db: 查询函数(querygen.go;类型来自核心代数 / sqlc 的分析,不用 sqlc 生成 Go 代码)
//  2. buf generate(proto → Go 类型 + Connect 接口)
//  3. handler 骨架 / 模式 handler / enum 转换 / 逻辑样例测试 / 逻辑函数的桩 / wiring
//
// 换成别的语言时,写另一个读同一份 IR 的适配器。
package golang

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

	"apifactory/factory/adapter"
	"apifactory/factory/ir"
	"apifactory/factory/proj"
)

// modulePath: the Go module of the project being generated (its go.mod), set by Run. Generated imports start with it.
// The default is this repository's module, for unit tests that render without running Run.
var modulePath = "apifactory"

// runtimeModule: the public runtime every generated service imports (github.com/MIERAI/api-factory-runtime). The
// pure-function gate of the manifest must forbid it (TestPureGateForbidsTheRuntime): a compute function that imports
// the runtime can reach the database and the network.
const runtimeModule = "github.com/MIERAI/api-factory-runtime"

func init() { adapter.Register("go", Run) }

func Run(plan *ir.Plan) error {
	m, err := proj.Module()
	if err != nil {
		return err
	}
	modulePath = m
	clearGenerated()
	if err := writeQueries(plan); err != nil {
		return err
	}
	dbp, err := loadDBPackage("gen/db")
	if err != nil {
		return err
	}
	if err := run("buf", "generate"); err != nil {
		return err
	}

	services := servicesFrom(plan)
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
	enumsBySvc := map[string]map[string]*enumMapping{}
	for i := range plan.APIs {
		api := &plan.APIs[i]
		if api.Pattern == "custom" { // 自定义: AI 写整个 handler
			continue
		}
		g, svc, err := newGenCtx(plan, api, services, dbp)
		if err != nil {
			return err
		}
		if enumsBySvc[svc.HandlerPath] == nil {
			enumsBySvc[svc.HandlerPath] = map[string]*enumMapping{}
		}
		for _, m := range g.enums {
			enumsBySvc[svc.HandlerPath][m.Helper] = m
		}
		gen, ok := handlerGens[api.Pattern] // 每个出口一个文件(exit_*.go)登记自己的 handler 生成器
		if !ok {
			return fmt.Errorf("%s: Go 适配器没有出口 %s 的 handler 生成器(adapters/golang/exit_*.go)", api.SpecPath, api.Pattern)
		}
		src, err := gen(g)
		if err != nil {
			return fmt.Errorf("%s: %w", api.SpecPath, err)
		}
		if err := writeGo(handlerGenPath(svc, api.Method), []byte(src)); err != nil {
			return err
		}
	}
	for _, svc := range services {
		if err := writeCommandErrors(svc); err != nil {
			return err
		}
		var ms []*enumMapping
		for _, m := range enumsBySvc[svc.HandlerPath] {
			ms = append(ms, m)
		}
		segs := strings.Split(svc.ProtoPackage, ".")
		alias := svc.Domain + segs[len(segs)-1]
		if err := writeEnums(svc, ms, alias, modulePath+"/gen/"+strings.Join(segs, "/")); err != nil {
			return err
		}
	}
	return writeWiring(services)
}

// LogicFile: compute 模式里 AI 实现的纯函数所在的文件(Go 模板的目录约定)。
func LogicFile(api *ir.API) string {
	return filepath.Join("internal/handler", api.Domain, snake(api.Method)+"_logic.go")
}

// newGenCtx 准备一个 API 的代码生成上下文。
func newGenCtx(plan *ir.Plan, api *ir.API, services []service, dbp *dbPackage) (*genCtx, service, error) {
	svc := serviceFor(services, api.ProtoPackage, api.Service)
	q, ok := dbp.funcs[api.Query.Name]
	if !ok && api.Command == nil { // 命令没有查询(写操作作为数据,command.go)
		return nil, svc, fmt.Errorf("%s: gen/db has no query %s", api.SpecPath, api.Query.Name)
	}
	segs := strings.Split(api.ProtoPackage, ".")
	g := &genCtx{
		api: api, svc: svc, query: q, db: dbp,
		pbAlias: api.Domain + segs[len(segs)-1],
		imports: map[string]string{},
	}
	g.use("context", "")
	g.use("connectrpc.com/connect", "")
	g.use(modulePath+"/gen/"+strings.Join(segs, "/"), g.pbAlias)
	g.enums = map[string]*enumMapping{}
	g.filterCol = map[string]string{}
	if api.Table != "" {
		for i := range plan.Enums {
			if e := &plan.Enums[i]; e.Table == api.Table {
				g.enums[e.Column] = mappingFrom(e, g.pbAlias)
			}
		}
		for _, f := range api.Filters {
			g.filterCol[norm(f.Field)] = f.Column
		}
	}
	if api.Command != nil { // 组合命令: 写的每张表的枚举(键 = 表.列,composite.go)
		for _, op := range api.Command.Ops {
			for i := range plan.Enums {
				if e := &plan.Enums[i]; e.Table == op.Table {
					g.enums[e.Table+"."+e.Column] = mappingFrom(e, g.pbAlias)
				}
			}
		}
	}
	return g, svc, nil
}

// clearGenerated 删除 sqlc 的旧输出: sqlc 不会删除已经不存在的查询对应的 .go 文件。
func clearGenerated() {
	old, _ := filepath.Glob("gen/db/*.sql.go")
	for _, f := range old {
		_ = os.Remove(f)
	}
}

type service struct {
	Domain       string // jobs
	Service      string // JobService
	ProtoPackage string // factory.jobs.v1
	ConnectPkg   string // jobsv1connect
	ConnectPath  string // apifactory/gen/factory/jobs/v1/jobsv1connect
	HandlerPath  string // apifactory/internal/handler/jobs
	Commands     bool   // 有命令(写入)的 API: Handler 带事务执行器,commands_gen.go
	Unmets       bool   // 有条件更新(组合命令的 条件): writeError 多一种原因(业务原因码)
}

func serviceFor(services []service, pkg, name string) service {
	for _, s := range services {
		if s.ProtoPackage == pkg && s.Service == name {
			return s
		}
	}
	return service{}
}

func servicesFrom(plan *ir.Plan) []service {
	commands, unmets := map[string]bool{}, map[string]bool{}
	for _, a := range plan.APIs {
		if a.Command != nil {
			commands[a.ProtoPackage+"."+a.Service] = true
			if len(a.Command.Unmets()) > 0 {
				unmets[a.ProtoPackage+"."+a.Service] = true
			}
		}
	}
	var out []service
	for _, s := range plan.Services {
		segs := strings.Split(s.ProtoPackage, ".")
		connectPkg := s.Domain + segs[len(segs)-1] + "connect"
		out = append(out, service{
			Domain:       s.Domain,
			Service:      s.Name,
			ProtoPackage: s.ProtoPackage,
			ConnectPkg:   connectPkg,
			ConnectPath:  modulePath + "/gen/" + strings.Join(segs, "/") + "/" + connectPkg,
			HandlerPath:  modulePath + "/internal/handler/" + s.Domain,
			Commands:     commands[s.ProtoPackage+"."+s.Name],
			Unmets:       unmets[s.ProtoPackage+"."+s.Name],
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ConnectPath < out[j].ConnectPath })
	return out
}

var scaffoldTmpl = template.Must(template.New("").Funcs(tmplFuncs).Parse(`// Code generated by factory. DO NOT EDIT.

// Package {{.Domain}} 实现 {{.ProtoPackage}}.{{.Service}}。
// 模式 API 的实现在 *_gen.go(工厂生成);custom API 的实现由 AI 写在各自的文件里。
package {{.Domain}}

import (
	"{{.ConnectPath}}"
	"{{module}}/gen/db"
	"github.com/MIERAI/api-factory-runtime/paging"
	"github.com/MIERAI/api-factory-runtime/shard"
{{- if .Commands}}
	"github.com/MIERAI/api-factory-runtime/txn"
{{- end}}
)

// Handler 实现 {{.ConnectPkg}}.{{.Service}}Handler。
// 尚未实现的 RPC 由内嵌的 Unimplemented 返回 UNIMPLEMENTED。
type Handler struct {
	{{.ConnectPkg}}.Unimplemented{{.Service}}Handler
	q      *db.Queries
	pager  *paging.Pager
	shards *shard.Router
{{- if .Commands}}
	tx     *txn.Executor // 命令(写入)的事务执行器
{{- end}}
}

func New(q *db.Queries, pager *paging.Pager, shards *shard.Router{{if .Commands}}, tx *txn.Executor{{end}}) *Handler {
	return &Handler{q: q, pager: pager, shards: shards{{if .Commands}}, tx: tx{{end}}}
}

// at: 分库的表 —— 按分库键(店铺代码等)选连接。没有配置分库时就是主库
func (h *Handler) at(key int64) *db.Queries { return db.New(h.shards.For(key)) }
`))

// tmplFuncs: {{module}} = the project's module (evaluated when a template runs, after Run has set it).
var tmplFuncs = template.FuncMap{"module": func() string { return modulePath }}

func writeHandlerScaffold(svc service) error {
	dir := filepath.Join("internal/handler", svc.Domain)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return render(scaffoldTmpl, svc, filepath.Join(dir, "handler_gen.go"))
}

var wiringTmpl = template.Must(template.New("").Funcs(tmplFuncs).Parse(`// Code generated by factory. DO NOT EDIT.

package main

import (
	"connectrpc.com/connect"

	"{{module}}/gen/db"
	"github.com/MIERAI/api-factory-runtime/server"
{{- if .Commands}}
	"github.com/MIERAI/api-factory-runtime/txn"
{{- end}}
{{- range .Services}}
	"{{.ConnectPath}}"
	{{.Domain}}handler "{{.HandlerPath}}"
{{- end}}
)

func services(deps server.Deps, opts ...connect.HandlerOption) []server.Service {
	q := db.New(deps.Pool)
	var out []server.Service
{{- if .Commands}}
	tx := txn.New(deps.Pool) // 命令(写入)的事务执行器
{{- end}}
{{- range .Services}}
	{
		path, h := {{.ConnectPkg}}.New{{.Service}}Handler({{.Domain}}handler.New(q, deps.Pager, deps.Shards{{if .Commands}}, tx{{end}}), opts...)
		out = append(out, server.Service{Path: path, Handler: h})
	}
{{- end}}
	return out
}
`))

func writeWiring(services []service) error {
	data := struct {
		Services []service
		Commands bool
	}{Services: services}
	for _, s := range services {
		data.Commands = data.Commands || s.Commands
	}
	return render(wiringTmpl, data, "cmd/server/wiring_gen.go")
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

// handlerGens: 出口 → handler 生成器(每个出口一个 exit_*.go 文件在 init 里登记)。
var handlerGens = map[string]func(*genCtx) (string, error){}
