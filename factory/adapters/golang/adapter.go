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

// runtimeModule: the public runtime every generated service imports (github.com/MIERAI/api-factory/runtime). The
// pure-function gate of the manifest must forbid it (TestPureGateForbidsTheRuntime): a compute function that imports
// the runtime can reach the database and the network.
const runtimeModule = "github.com/MIERAI/api-factory/runtime"

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
		if err := writeMessaging(svc); err != nil {
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
	// R4: 有消息(发出 / 消息处理器)= Handler 带 outbox 的 Writer,messages_gen.go;Handlers = 这个领域的消息处理器;
	// HandlerOnly = 领域里只有消息处理器(没有 HTTP 的服务)
	Messages    bool
	Handlers    []handlerInfo
	HandlerOnly bool
	Machines    []string // R5: 这个领域的状态机(超时扫描: h.<机器>Due / Fire / Stats)
}

// handlerInfo: 一个消息处理器(wiring 的消费循环 + 拓扑)。
type handlerInfo struct {
	Method, Queue, Type, Kind string
	Version                   int
}

func serviceFor(services []service, pkg, name string) service {
	for _, s := range services {
		if s.ProtoPackage == pkg && s.Service == name {
			return s
		}
	}
	for _, s := range services { // 消息处理器(MessageHandlerService 不挂载): 同一个领域的 Handler
		if s.ProtoPackage == pkg && len(s.Handlers) > 0 {
			return s
		}
	}
	return service{}
}

func servicesFrom(plan *ir.Plan) []service {
	commands, unmets := map[string]bool{}, map[string]bool{}
	messages, handlerSvc := map[string]bool{}, map[string]bool{} // 按 proto 包: 有消息;处理器的服务(不挂载)
	handlers := map[string][]handlerInfo{}
	machines := map[string][]string{}
	for _, a := range plan.APIs {
		if a.Command.IsMachine() && a.Command.Machine != nil {
			m := a.Command.Machine
			messages[a.ProtoPackage] = true
			handlerSvc[a.ProtoPackage+"."+a.Service] = true
			src := strings.Split(a.ProtoPackage, ".")[0]
			for _, r := range m.Receives {
				kind := "message.Event"
				if r.Kind == "command" {
					kind = "message.Command"
				}
				handlers[a.ProtoPackage] = append(handlers[a.ProtoPackage], handlerInfo{Method: r.Method, Queue: src + "." + r.Type, Type: r.Type, Kind: kind, Version: r.Version})
			}
			machines[a.ProtoPackage] = append(machines[a.ProtoPackage], m.Name)
			continue
		}
		if a.Command != nil {
			commands[a.ProtoPackage+"."+a.Service] = true
			if len(a.Command.Unmets()) > 0 {
				unmets[a.ProtoPackage+"."+a.Service] = true
			}
			if len(a.Command.Emits) > 0 || a.Command.IsHandler() {
				messages[a.ProtoPackage] = true
			}
			if a.Command.IsHandler() {
				handlerSvc[a.ProtoPackage+"."+a.Service] = true
				r := a.Command.Receive
				kind := "message.Event"
				if r.Kind == "command" {
					kind = "message.Command"
				}
				src := strings.Split(a.ProtoPackage, ".")[0]
				handlers[a.ProtoPackage] = append(handlers[a.ProtoPackage], handlerInfo{Method: a.Method, Queue: src + "." + r.Type, Type: r.Type, Kind: kind, Version: r.Version})
				commands[a.ProtoPackage+"."+a.Service] = false
			}
		}
	}
	var out []service
	hasMain := map[string]bool{}
	for _, s := range plan.Services {
		if !handlerSvc[s.ProtoPackage+"."+s.Name] {
			hasMain[s.ProtoPackage] = true
		}
	}
	for _, s := range plan.Services {
		if handlerSvc[s.ProtoPackage+"."+s.Name] { // 消息处理器的服务: 不挂载;处理器挂在同一个领域的 Handler 上
			if hasMain[s.ProtoPackage] {
				continue
			}
		}
		segs := strings.Split(s.ProtoPackage, ".")
		connectPkg := s.Domain + segs[len(segs)-1] + "connect"
		out = append(out, service{
			Domain:       s.Domain,
			Service:      s.Name,
			ProtoPackage: s.ProtoPackage,
			ConnectPkg:   connectPkg,
			ConnectPath:  modulePath + "/gen/" + strings.Join(segs, "/") + "/" + connectPkg,
			HandlerPath:  modulePath + "/internal/handler/" + s.Domain,
			Commands:     commands[s.ProtoPackage+"."+s.Name] || messages[s.ProtoPackage],
			Unmets:       unmets[s.ProtoPackage+"."+s.Name],
			Messages:     messages[s.ProtoPackage],
			Handlers:     handlers[s.ProtoPackage],
			HandlerOnly:  handlerSvc[s.ProtoPackage+"."+s.Name],
			Machines:     machines[s.ProtoPackage],
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
	"github.com/MIERAI/api-factory/runtime/paging"
	"github.com/MIERAI/api-factory/runtime/shard"
{{- if .Messages}}
	"github.com/MIERAI/api-factory/runtime/outbox"
{{- end}}
{{- if .Commands}}
	"github.com/MIERAI/api-factory/runtime/txn"
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
{{- if .Messages}}
	out    *outbox.Writer // 发出的消息(R4: 写进 outbox,和写操作同一个事务)
{{- end}}
}

func New(q *db.Queries, pager *paging.Pager, shards *shard.Router{{if .Commands}}, tx *txn.Executor{{end}}{{if .Messages}}, out *outbox.Writer{{end}}) *Handler {
	return &Handler{q: q, pager: pager, shards: shards{{if .Commands}}, tx: tx{{end}}{{if .Messages}}, out: out{{end}}}
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
{{- if .Messages}}
	"context"
	"log/slog"
{{ end}}
	"connectrpc.com/connect"

	"{{module}}/gen/db"
{{- if .Messages}}
	"github.com/MIERAI/api-factory/runtime/consume"
{{- if .HasHandlers}}
	"github.com/MIERAI/api-factory/runtime/message"
{{- end}}
	"github.com/MIERAI/api-factory/runtime/outbox"
{{- end}}
	"github.com/MIERAI/api-factory/runtime/server"
{{- if .HasMachines}}
	"github.com/MIERAI/api-factory/runtime/sm"
{{- end}}
{{- if .Messages}}
	"github.com/MIERAI/api-factory/runtime/topology"
{{- end}}
{{- if .Commands}}
	"github.com/MIERAI/api-factory/runtime/txn"
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
{{- if .Messages}}
	ob := outbox.NewWriter({{printf "%q" .Source}}) // 发出的消息(envelope.source = 服务名)
	var consumers []*consume.Consumer             // 消息处理器的消费循环
{{- end}}
{{- if .HasMachines}}
	var scanners []*sm.Scanner // 状态机的超时扫描
{{- end}}
{{- range .Services}}
	{
{{- if .Messages}}
		h := {{.Domain}}handler.New(q, deps.Pager, deps.Shards{{if .Commands}}, tx{{end}}, ob)
{{- if not .HandlerOnly}}
		path, hh := {{.ConnectPkg}}.New{{.Service}}Handler(h, opts...)
		out = append(out, server.Service{Path: path, Handler: hh})
{{- end}}
{{- range .Handlers}}
		consumers = append(consumers, &consume.Consumer{Queue: {{printf "%q" .Queue}}, Handler: h.{{.Method}}, Exec: tx, Broker: deps.AMQP,
			Accept: []consume.Accept{ {Type: {{printf "%q" .Type}}, Versions: []int{ {{.Version}} } } }})
{{- end}}
{{- range .Machines}}
		scanners = append(scanners, &sm.Scanner{Machine: {{printf "%q" .}}, Exec: tx, Due: h.{{.}}Due, Fire: h.{{.}}Fire, Stats: h.{{.}}Stats})
{{- end}}
{{- else}}
		path, h := {{.ConnectPkg}}.New{{.Service}}Handler({{.Domain}}handler.New(q, deps.Pager, deps.Shards{{if .Commands}}, tx{{end}}), opts...)
		out = append(out, server.Service{Path: path, Handler: h})
{{- end}}
	}
{{- end}}
{{- if .HasMachines}}
	return append(out, messaging(deps, consumers, scanners)...)
{{- else if .Messages}}
	return append(out, messaging(deps, consumers)...)
{{- else}}
	return out
{{- end}}
}
{{- if .Messages}}

{{- if .HasMachines}}
// messaging: 消息(R4)和状态机(R5)。进程内投递 / 跑一轮超时的测试入口(只在 FACTORY_TEST_FAULTS=1 时响应)+ 有 AMQP_URL 时:
// 拓扑(交换机 / 队列 / 重试 / DLQ,参数不一致 = 启动失败)、outbox 的中继(每个数据库一个在工作)、每个处理器的消费循环、
// 每个状态机的超时扫描(每个数据库一个在工作;FACTORY_TEST_CLOCK=1 时不自己跑,由测试驱动)。
func messaging(deps server.Deps, consumers []*consume.Consumer, scanners []*sm.Scanner) []server.Service {
	test := []server.Service{
		{Path: consume.TestPath, Handler: consume.TestHandler(consumers...)},
		{Path: sm.TestPath, Handler: sm.TestHandler(deps.Pool, scanners...)},
	}
	if deps.AMQP == nil { // 没有 AMQP_URL: outbox 只写不发(本地开发 / 进程内的验收)
		// 部署时忘了 AMQP_URL 不会报错(服务照常回答 API),所以至少在启动日志里说出来
		slog.Warn("AMQP_URL not set: messages are written to the outbox but never sent, no message handler consumes and no state machine times out")
		return test
	}
{{- else}}
// messaging: 消息(R4)。进程内投递的测试入口(只在 FACTORY_TEST_FAULTS=1 时响应)+ 有 AMQP_URL 时:
// 拓扑(交换机 / 队列 / 重试 / DLQ,参数不一致 = 启动失败)、outbox 的中继(每个数据库一个在工作)、每个处理器的消费循环。
func messaging(deps server.Deps, consumers []*consume.Consumer) []server.Service {
	test := server.Service{Path: consume.TestPath, Handler: consume.TestHandler(consumers...)}
	if deps.AMQP == nil { // 没有 AMQP_URL: outbox 只写不发(本地开发 / 进程内的验收)
		// 部署时忘了 AMQP_URL 不会报错(服务照常回答 API),所以至少在启动日志里说出来
		slog.Warn("AMQP_URL not set: messages are written to the outbox but never sent, and no message handler consumes")
		return []server.Service{test}
	}
{{- end}}
	spec := topology.Spec{Retry: topology.DefaultRetry, Queues: []topology.Queue{
{{- range .Services}}{{range .Handlers}}
		{Name: {{printf "%q" .Queue}}, Subscriptions: []topology.Subscription{ {Kind: {{.Kind}}, Type: {{printf "%q" .Type}}} }},
{{- end}}{{end}}
	}}
	// 准备好了(/readyz 在那之前 503): 拓扑声明好了 + 每个消费循环订阅了。消费循环先等拓扑再订阅
	topo := deps.Workers.Ready("topology")
	deps.Workers.Go("topology", func(ctx context.Context) error {
		if err := topology.Ensure(ctx, deps.AMQP, spec); err != nil {
			return err
		}
		topo.Done()
		<-ctx.Done()
		return nil
	})
	relay := &outbox.Relay{DB: deps.Pool, Broker: deps.AMQP}
	deps.Workers.Go("outbox relay", relay.Run)
	for _, c := range consumers {
		c.WaitFor, c.Subscribed = topo.C(), deps.Workers.Ready("consume "+c.Queue).Done
		deps.Workers.Go("consume "+c.Queue, c.Run)
	}
{{- if .HasMachines}}
	for _, s := range scanners {
		deps.Workers.Go("sm timeout "+s.Machine, func(ctx context.Context) error { return s.Run(ctx, deps.Pool) })
	}
	return test
{{- else}}
	return []server.Service{test}
{{- end}}
}
{{- end}}
`))

func writeWiring(services []service) error {
	data := struct {
		Services    []service
		Commands    bool
		Messages    bool
		HasHandlers bool
		HasMachines bool
		Source      string // 服务名(proto 包名的前缀): envelope.source、队列名的前缀
	}{Services: services}
	for _, s := range services {
		data.HasMachines = data.HasMachines || len(s.Machines) > 0
		data.Commands = data.Commands || s.Commands
		data.Messages = data.Messages || s.Messages
		data.HasHandlers = data.HasHandlers || len(s.Handlers) > 0
		data.Source = strings.Split(s.ProtoPackage, ".")[0]
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
