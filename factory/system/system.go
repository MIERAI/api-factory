// Package system: 一个系统(共用一个消息契约目录的全部项目)的跨项目静态检查和系统级的拓扑清单
// (GAPS G23;boundaries.md §8.1 "跨服务的消息契约对得上 = 工厂的跨项目静态检查")。
//
// 输入 = 每个项目生成的式样书(specs/: 收到 / 发出的消息和当时用的契约字段)+ 设计(design/*/消息.yaml: 谁拥有契约)
// + 共享的契约目录(<contracts>/*.json: 现在的契约)。不需要跑任何服务。
//
// 检查(全部报出来,一个都不放过):
//   - 每条被消费的消息在系统里有发送方;每条发出的消息至少有一个接收方 —— 契约写了 外部: 是 的除外(系统外的发送方 / 接收方)
//   - 一个命令恰好一个接收方(direct 交换机;R4 E5);事件的发送方 = 拥有者,命令的接收方 = 拥有者
//   - 版本一致: 发送方发的版本 = 接收方接受的版本 = 契约目录里的版本
//   - 字段一致: 每个项目生成代码时用的契约字段 = 契约目录里现在的(否则那个项目要重新生成);接收方要的每个字段
//     发送方都发(同名、同类型,接收方必填的发送方也必填)
//
// 拓扑清单(<contracts>/topology.json,api-factory-runtime 的 topology.Manifest): 每个处理器的队列
// (<服务>.<消息类型>,和生成的 wiring 同一个规则)、绑定、重试队列、DLQ,以及每种消息的发送方 / 接收方。
// 平台在部署服务之前声明它(factory-runtime topology apply),订阅方第一次启动之前发出的事件也就有队列接收。
package system

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/MIERAI/api-factory-runtime/message"
	"github.com/MIERAI/api-factory-runtime/topology"

	"apifactory/factory/design"
	"apifactory/factory/spec"
)

// ManifestFile: 契约目录里的系统级拓扑清单(设计编译器读契约时跳过它)。
const ManifestFile = design.TopologyFile

// Project: 系统里的一个项目。
type Project struct {
	Dir     string // 项目根目录
	Service string // proto 的包名前缀 = 服务名(队列名的前缀、envelope.source)
	Owns    map[string]bool
	Uses    []Use
}

// Use: 一个项目的一个 API 收到 / 发出一种消息。
type Use struct {
	API      string // 式样书 ID
	Type     string
	Kind     string // event / command
	Version  int
	Consumes bool // true = 收到;false = 发出
	Fields   []spec.ContractField
}

// System: 一个系统。
type System struct {
	Dir       string
	Contracts string // 共享的契约目录
	Projects  []*Project
	Current   map[string]*spec.Contract // 类型 → 契约目录里最新的版本
	Versions  map[string][]int          // 类型 → 契约目录里有的版本
}

type projectConfig struct {
	Proto struct {
		PackagePrefix string `yaml:"package_prefix"`
	} `yaml:"proto"`
	Contracts string `yaml:"contracts"`
}

// Load: dir 下每个有 factory.yaml 的子目录 = 一个项目;它们的 contracts 必须是同一个目录。
func Load(dir string) (*System, error) {
	cfgs, _ := filepath.Glob(filepath.Join(dir, "*", "factory.yaml"))
	sort.Strings(cfgs)
	if len(cfgs) == 0 {
		return nil, fmt.Errorf("%s: no project (no */factory.yaml)", dir)
	}
	s := &System{Dir: dir, Current: map[string]*spec.Contract{}, Versions: map[string][]int{}}
	for _, cfgPath := range cfgs {
		pdir := filepath.Dir(cfgPath)
		b, err := os.ReadFile(cfgPath)
		if err != nil {
			return nil, err
		}
		var c projectConfig
		if err := yaml.Unmarshal(b, &c); err != nil {
			return nil, fmt.Errorf("%s: %w", cfgPath, err)
		}
		if c.Proto.PackagePrefix == "" {
			return nil, fmt.Errorf("%s: proto.package_prefix is required (the service's name)", cfgPath)
		}
		if c.Contracts == "" {
			c.Contracts = "contracts"
		}
		cdir := filepath.Clean(filepath.Join(pdir, c.Contracts))
		switch {
		case s.Contracts == "":
			s.Contracts = cdir
		case s.Contracts != cdir:
			return nil, fmt.Errorf("%s: contracts %s, but the other projects use %s (a system shares one contracts directory)", cfgPath, cdir, s.Contracts)
		}
		p := &Project{Dir: pdir, Service: c.Proto.PackagePrefix, Owns: map[string]bool{}}
		if err := p.load(); err != nil {
			return nil, err
		}
		s.Projects = append(s.Projects, p)
	}
	return s, s.loadContracts()
}

func (p *Project) load() error {
	owned, _ := filepath.Glob(filepath.Join(p.Dir, "design", "*", "消息.yaml"))
	for _, f := range owned {
		b, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		var doc struct {
			Messages map[string]any `yaml:"消息"`
		}
		if err := yaml.Unmarshal(b, &doc); err != nil {
			return fmt.Errorf("%s: %w", f, err)
		}
		for t := range doc.Messages {
			p.Owns[t] = true
		}
	}
	specs, err := spec.LoadAll(filepath.Join(p.Dir, "specs"))
	if err != nil {
		return err
	}
	for _, sp := range specs {
		c := sp.Pattern.Command
		if c == nil {
			continue
		}
		contract := map[string]spec.Contract{}
		for _, ct := range c.Contracts {
			contract[ct.Type] = ct
		}
		seen := map[string]bool{}
		add := func(typ string, consumes bool) {
			key := fmt.Sprint(typ, consumes)
			if typ == "" || seen[key] {
				return
			}
			seen[key] = true
			ct := contract[typ]
			u := Use{API: sp.ID(), Type: typ, Kind: ct.Kind, Version: ct.Version, Consumes: consumes, Fields: ct.Fields}
			if consumes && c.Receive != nil && c.Receive.Type == typ {
				u.Version = c.Receive.Version
			}
			p.Uses = append(p.Uses, u)
		}
		emits := func(es []spec.Emit) {
			for _, e := range es {
				add(e.Type, false)
			}
		}
		if c.Receive != nil {
			add(c.Receive.Type, true)
		}
		emits(c.Emit)
		for _, r := range c.Results {
			emits(r.Emit)
		}
		if m := c.Machine; m != nil {
			for _, t := range m.Transitions {
				add(t.On, true)
				emits(t.Emit)
			}
			for _, st := range m.States {
				if st.Command != nil {
					add(st.Command.Type, false)
				}
				emits(st.DeadlineEmit)
			}
		}
	}
	sort.SliceStable(p.Uses, func(i, j int) bool {
		a, b := p.Uses[i], p.Uses[j]
		return a.Type+a.API < b.Type+b.API
	})
	return nil
}

func (s *System) loadContracts() error {
	files, _ := filepath.Glob(filepath.Join(s.Contracts, "*.json"))
	sort.Strings(files)
	for _, f := range files {
		if filepath.Base(f) == ManifestFile {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		var doc struct {
			X *spec.Contract `json:"x-factory"`
		}
		if err := json.Unmarshal(b, &doc); err != nil {
			return fmt.Errorf("%s: %w", f, err)
		}
		if doc.X == nil {
			return fmt.Errorf("%s: no x-factory (not a contract generated by the factory)", f)
		}
		s.Versions[doc.X.Type] = append(s.Versions[doc.X.Type], doc.X.Version)
		if cur := s.Current[doc.X.Type]; cur == nil || cur.Version < doc.X.Version {
			s.Current[doc.X.Type] = doc.X
		}
	}
	return nil
}

// endpoint: 一个项目的一个 API(报错用)。
type endpoint struct {
	project *Project
	use     Use
}

func (e endpoint) String() string { return e.project.Service + "(" + e.use.API + ")" }

type flow struct {
	producers, consumers []endpoint
}

func (s *System) flows() (map[string]*flow, []string) {
	m := map[string]*flow{}
	for _, p := range s.Projects {
		for _, u := range p.Uses {
			f := m[u.Type]
			if f == nil {
				f = &flow{}
				m[u.Type] = f
			}
			if u.Consumes {
				f.consumers = append(f.consumers, endpoint{p, u})
			} else {
				f.producers = append(f.producers, endpoint{p, u})
			}
		}
	}
	for t := range s.Current { // 契约目录里有、没人用的契约(拥有者声明了却没人发 / 没人收)
		if m[t] == nil {
			m[t] = &flow{}
		}
	}
	types := make([]string, 0, len(m))
	for t := range m {
		types = append(types, t)
	}
	sort.Strings(types)
	return m, types
}

// Check: 跨项目的检查。返回全部问题(空 = 通过)。
func (s *System) Check() []string {
	var out []string
	bad := func(format string, args ...any) { out = append(out, fmt.Sprintf(format, args...)) }
	flows, types := s.flows()
	for _, t := range types {
		f := flows[t]
		cur := s.Current[t]
		if cur == nil {
			for _, e := range append(append([]endpoint{}, f.producers...), f.consumers...) {
				bad("%s: %s uses %s, which has no contract in %s", t, e, t, s.Contracts)
			}
			continue
		}
		owner := ""
		for _, p := range s.Projects {
			if p.Owns[t] {
				if owner != "" {
					bad("%s: owned by both %s and %s (one 消息.yaml declares a contract)", t, owner, p.Service)
				}
				owner = p.Service
			}
		}
		if owner == "" {
			bad("%s: no project of the system declares it in design/*/消息.yaml", t)
		}
		if len(f.consumers) == 0 && !cur.External {
			bad("%s (%s): sent by %s but nobody in the system handles it — add a handler, or mark the contract 外部: 是 (consumed outside the system)", t, cur.Kind, list(f.producers))
		}
		if len(f.producers) == 0 && !cur.External {
			bad("%s (%s): handled by %s but nobody in the system sends it — mark the contract 外部: 是 if it comes from outside the system", t, cur.Kind, list(f.consumers))
		}
		if cur.Kind == "command" {
			if services := distinct(f.consumers); len(services) > 1 {
				bad("%s (command): handled by %s; a command has exactly one handler (events can have many subscribers)", t, strings.Join(services, ", "))
			}
			for _, c := range f.consumers {
				if owner != "" && c.project.Service != owner {
					bad("%s (command): handled by %s, but %s owns it (a command's contract belongs to the service that executes it)", t, c, owner)
				}
			}
		} else {
			for _, p := range f.producers {
				if owner != "" && p.project.Service != owner {
					bad("%s (event): sent by %s, but %s owns it (an event's contract belongs to the service that sends it)", t, p, owner)
				}
			}
		}
		for _, e := range append(append([]endpoint{}, f.producers...), f.consumers...) {
			role := "sends"
			if e.use.Consumes {
				role = "accepts"
			}
			if e.use.Kind != cur.Kind {
				bad("%s: %s was generated with kind %s, the contract is a %s (regenerate %s)", t, e, e.use.Kind, cur.Kind, e.project.Dir)
			}
			if e.use.Version != cur.Version {
				bad("%s: %s %s v%d, the contract is v%d (versions %v in %s; regenerate %s or keep v%d)", t, e, role, e.use.Version, cur.Version, s.Versions[t], s.Contracts, e.project.Dir, e.use.Version)
				continue
			}
			if msg := sameFields(e.use.Fields, cur.Fields); msg != "" {
				bad("%s v%d: %s was generated with other fields than the contract (%s): regenerate %s", t, cur.Version, e, msg, e.project.Dir)
			}
		}
		for _, c := range f.consumers {
			for _, p := range f.producers {
				if c.use.Version != p.use.Version {
					bad("%s: %s sends v%d, %s accepts v%d", t, p, p.use.Version, c, c.use.Version)
				} else if msg := covers(p.use.Fields, c.use.Fields); msg != "" {
					bad("%s v%d: %s needs what %s does not send: %s", t, c.use.Version, c, p, msg)
				}
			}
		}
	}
	return out
}

func list(es []endpoint) string {
	var s []string
	for _, e := range es {
		s = append(s, e.String())
	}
	return strings.Join(s, ", ")
}

func distinct(es []endpoint) []string {
	seen := map[string]bool{}
	var out []string
	for _, e := range es {
		if !seen[e.project.Service] {
			seen[e.project.Service] = true
			out = append(out, e.project.Service)
		}
	}
	sort.Strings(out)
	return out
}

// sameFields: 两组字段的名字 / 类型 / 必填 / 元素一致("" = 一致)。约束和说明不比(它们不影响线上的表示)。
func sameFields(a, b []spec.ContractField) string {
	if len(a) != len(b) {
		return fmt.Sprintf("%d fields vs %d", len(a), len(b))
	}
	for i := range a {
		x, y := a[i], b[i]
		switch {
		case x.Name != y.Name:
			return "field " + x.Name + " vs " + y.Name
		case x.Type != y.Type:
			return fmt.Sprintf("field %s: %s vs %s", x.Name, x.Type, y.Type)
		case x.Required != y.Required:
			return "field " + x.Name + ": required differs"
		}
		if msg := sameFields(x.Elem, y.Elem); msg != "" {
			return "field " + x.Name + ": " + msg
		}
	}
	return ""
}

// covers: 发送方的字段 sent 覆盖接收方要的 want: 每个接收方的字段发送方都有(同类型),接收方必填的发送方也必填。
func covers(sent, want []spec.ContractField) string {
	by := map[string]spec.ContractField{}
	for _, f := range sent {
		by[f.Name] = f
	}
	for _, w := range want {
		f, ok := by[w.Name]
		switch {
		case !ok && w.Required:
			return "required field " + w.Name
		case !ok:
			continue
		case f.Type != w.Type:
			return fmt.Sprintf("field %s: %s sent, %s expected", w.Name, f.Type, w.Type)
		case w.Required && !f.Required:
			return "field " + w.Name + " is optional for the sender, required for the handler"
		}
		if msg := covers(f.Elem, w.Elem); msg != "" {
			return "field " + w.Name + ": " + msg
		}
	}
	return ""
}

// Manifest: 系统级的拓扑清单。队列名、重试策略和生成的 wiring 一样(topology.QueueName(服务, 类型)、DefaultRetry)。
func (s *System) Manifest() (topology.Manifest, error) {
	m := topology.Manifest{
		Version:   topology.ManifestVersion,
		Comment:   "Code generated by factory system from the projects' specs and the contracts. DO NOT EDIT. Applied by the platform before the services start: factory-runtime topology apply --file " + ManifestFile,
		Exchanges: topology.DefaultExchanges,
		Retry:     topology.RetryOf(topology.DefaultRetry),
		Queues:    []topology.ManifestQueue{},
	}
	spec := topology.Spec{Retry: topology.DefaultRetry}
	flows, types := s.flows()
	for _, t := range types {
		f := flows[t]
		cur := s.Current[t]
		mm := topology.ManifestMessage{Type: t, Producers: distinct(f.producers), Consumers: distinct(f.consumers)}
		if mm.Producers == nil {
			mm.Producers = []string{}
		}
		if mm.Consumers == nil {
			mm.Consumers = []string{}
		}
		if cur != nil {
			mm.Kind, mm.Version, mm.External = cur.Kind, cur.Version, cur.External
		}
		for _, p := range s.Projects {
			if p.Owns[t] {
				mm.Owner = p.Service
			}
		}
		m.Messages = append(m.Messages, mm)
		for _, svc := range mm.Consumers {
			kind := message.Event
			if mm.Kind == "command" {
				kind = message.Command
			}
			q := topology.Queue{Name: topology.QueueName(svc, t), Subscriptions: []topology.Subscription{{Kind: kind, Type: t}}}
			mq, err := topology.QueueOf(svc, q, spec)
			if err != nil {
				return m, err
			}
			m.Queues = append(m.Queues, mq)
		}
	}
	sort.Slice(m.Queues, func(i, j int) bool { return m.Queues[i].Name < m.Queues[j].Name })
	return m, nil
}

// ManifestJSON: 清单的字节(确定的: 同样的输入永远同样的字节)。
func (s *System) ManifestJSON() ([]byte, error) {
	m, err := s.Manifest()
	if err != nil {
		return nil, err
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}
