package exits

// R4(docs/design/r4-events-messages.md): 消息。
//
// 组合命令的 发出: 操作全部成功时,同一个事务里写进 outbox(平台的中继发到 RabbitMQ)。
//
//	发出:
//	  - task.created: { task_id: 新建.id, title: 输入.title }
//
// 消息处理(种类 消息处理): 入口换成 RabbitMQ 的组合命令。去重、确认、重试、死信由运行时做(api-factory-runtime/consume)。
//
//	种类: 消息处理
//	收到: inventory.reserve                 # 契约(消息.yaml / 共享的 contracts/)
//	每项: 消息.items                        # 对列表的每个元素执行 操作(项.xxx;按元素的唯一键排序 = 加行锁的顺序)
//	操作:
//	  - 预留: { 幂等新建: reservations, 键: [order_id, sku], 值: { … } }
//	  - 扣减: { 更新: stock, 键: { sku: 项.sku }, 仅当: 预留.新建, 设置: { … }, 条件: "available >= 项.qty", 不满足: INSUFFICIENT_STOCK }
//	结果:                                   # 按顺序判定;成功 最后
//	  RELEASED:           { 当: "任一项 预留.status = RELEASED", 发出: [ … ] }
//	  INSUFFICIENT_STOCK: { 发出: [ … ] }   # = 某个操作的 不满足
//	  ALREADY_RELEASED:   { 当: "…", 处理: 死信 }
//	  成功: { 发出: [ … ] }                 # 或者分支: [ { 当: "…", 发出: [ … ] }, { 否则: 是, 发出: [ … ] } ]

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"

	"apifactory/factory/ir"
	"apifactory/factory/spec"
)

// AnyWord: 结果的 当 的前缀: 每项 的任一项满足。DLQWord: 处理: 死信。
const (
	AnyWord = "任一项"
	DLQWord = "死信"
)

// HandlerOutcomes: 消息处理器的用例的 结果: 的写法(成功 / 业务结果的名字 / 死信 / 毒消息)。
var HandlerOutcomes = map[string]string{spec.SuccessResult: "OK", DLQWord: "DLQ", "毒消息": "POISON"}

func init() {
	Register(&Exit{
		Kind: "message", Name: "消息处理", Cardinality: "one", Command: true,
		DesignKeys: []string{"收到", "每项", "操作", "结果"},
		Parse:      parseHandler,
		Check:      checkHandler,
		IR: func(s *spec.Spec, api *ir.API) error {
			api.Command = &ir.Command{Op: "message"}
			return nil
		},
	})
}

func parseHandler(d Design, p *spec.Pattern) {
	parseOps(d, p)
	c := p.Command
	if t := d.Str("收到"); t != "" {
		c.Receive = &spec.MessageRef{Type: t}
	} else {
		d.Err("消息处理: 缺少 收到(消息类型,例: inventory.reserve)")
	}
	if e := d.Str("每项"); e != "" {
		name, ok := strings.CutPrefix(e, "消息.")
		if !ok || name == "" {
			d.Err("每项: 写成 消息.列表字段(例: 消息.items)")
		}
		c.Each = name
	}
	n := d.Node("结果")
	if n == nil {
		return
	}
	if n.Kind != yaml.MappingNode {
		d.Err("结果: 写成 { 成功: { 发出: […] }, 原因码: { 发出: […] }, … }")
		return
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		name, body := n.Content[i].Value, n.Content[i+1]
		if body.Kind == yaml.SequenceNode { // 分支: 按顺序判定 当,最后一个可以是 否则
			if name != spec.SuccessResult {
				d.Err("结果 %s: 只有 成功 可以写分支的列表", name)
				continue
			}
			for _, b := range body.Content {
				r := parseResult(d, name, b)
				if r.When == "" && !isOtherwise(b) {
					d.Err("结果 成功 的分支: 每个分支写 当: \"…\"(最后一个可以写 否则: 是)")
				}
				c.Results = append(c.Results, r)
			}
			continue
		}
		c.Results = append(c.Results, parseResult(d, name, body))
	}
}

func isOtherwise(n *yaml.Node) bool {
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == "否则" {
			return true
		}
	}
	return false
}

func parseResult(d Design, name string, n *yaml.Node) spec.Result {
	r := spec.Result{Name: name}
	if n.Kind == yaml.ScalarNode && strings.TrimSpace(n.Value) == "" {
		return r
	}
	if n.Kind != yaml.MappingNode {
		d.Err("结果 %s: 写成 { 当?: \"…\", 发出?: […], 处理?: 死信 }", name)
		return r
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i].Value, n.Content[i+1]
		switch k {
		case "当":
			w := strings.TrimSpace(v.Value)
			if rest, ok := strings.CutPrefix(w, AnyWord); ok {
				r.Any, w = true, strings.TrimSpace(rest)
			}
			r.When = w
		case "否则":
		case "发出":
			r.Emit = parseEmits(d, v, "结果 "+name+" 的 发出")
		case "处理":
			if strings.TrimSpace(v.Value) != DLQWord {
				d.Err("结果 %s: 处理 只能写 死信(不提交,消息进 DLQ,需要人处理)", name)
			}
			r.DLQ = true
		default:
			d.Err("结果 %s: 不认识的键 %s(当 / 否则 / 发出 / 处理)", name, k)
		}
	}
	return r
}

// parseEmits: 发出的列表: - 类型: { data 的字段: 表达式 }。
func parseEmits(d Design, n *yaml.Node, where string) []spec.Emit {
	if n == nil {
		return nil
	}
	if n.Kind != yaml.SequenceNode {
		d.Err("%s: 写成列表: - 消息类型: { 字段: 值 }", where)
		return nil
	}
	var out []spec.Emit
	for _, item := range n.Content {
		if item.Kind != yaml.MappingNode || len(item.Content) != 2 || item.Content[1].Kind != yaml.MappingNode {
			d.Err("%s 第 %d 行: 每条写成 - 消息类型: { 字段: 值 }", where, item.Line)
			continue
		}
		em := spec.Emit{Type: strings.TrimSpace(item.Content[0].Value)}
		body := item.Content[1]
		for i := 0; i+1 < len(body.Content); i += 2 {
			em.Values = append(em.Values, spec.ColExpr{Column: body.Content[i].Value, Expr: strings.TrimSpace(body.Content[i+1].Value)})
		}
		out = append(out, em)
	}
	return out
}

// checkMessaging: 发出 / 仅当 / 结果 的规格检查(组合命令和消息处理共用)。
func checkMessaging(c *spec.Command, prefix string) []string {
	var out []string
	add := func(f string, a ...any) { out = append(out, fmt.Sprintf(prefix+f, a...)) }
	kinds := map[string]string{}
	for _, op := range c.Ops {
		if op.Only != "" {
			ref, flag, ok := strings.Cut(op.Only, ".")
			switch {
			case !ok:
				add("%s: only %q: write <op>.%s or <op>.%s", op.Name, op.Only, CreatedWord, UpdatedWord)
			case kinds[ref] == "":
				add("%s: only %q: %s is not an earlier op", op.Name, op.Only, ref)
			case flag == CreatedWord && kinds[ref] != "insert_idempotent":
				add("%s: only %q: %s is for insert_idempotent", op.Name, op.Only, CreatedWord)
			case flag == UpdatedWord && kinds[ref] != "update":
				add("%s: only %q: %s is for update", op.Name, op.Only, UpdatedWord)
			case flag != CreatedWord && flag != UpdatedWord:
				add("%s: only %q: write <op>.%s or <op>.%s", op.Name, op.Only, CreatedWord, UpdatedWord)
			}
		}
		if op.Skip && op.Kind != "update" {
			add("%s: skip (不满足: 跳过) is for update", op.Name)
		}
		kinds[op.Name] = op.Kind
	}
	emits := append([]spec.Emit{}, c.Emit...)
	for _, r := range c.Results {
		emits = append(emits, r.Emit...)
	}
	for _, e := range emits {
		if e.Type == "" || strings.ContainsAny(e.Type, " \t*#") {
			add("emit: message type %q", e.Type)
		}
	}
	return out
}

func checkHandler(s *spec.Spec) []string {
	c := s.Pattern.Command
	if c == nil || c.Receive == nil {
		return []string{"pattern message: receive is required"}
	}
	cp := *s
	pat := s.Pattern
	pc := *c
	pc.Receive, pc.Each, pc.Results, pc.Contracts = nil, "", nil, nil
	pat.Command = &pc
	cp.Pattern = pat
	var out []string
	for _, m := range checkComposite(&cp) {
		out = append(out, strings.Replace(m, "pattern command: ", "pattern message: ", 1))
	}
	add := func(f string, a ...any) { out = append(out, fmt.Sprintf("pattern message: "+f, a...)) }
	if len(c.Emit) > 0 {
		add("emit: a handler emits in its results (结果.<名字>.发出)")
	}
	if len(c.Output) > 0 {
		add("output: a handler has no response")
	}
	unmets := map[string]bool{}
	for _, op := range c.Ops {
		if op.Unmet != "" {
			unmets[op.Unmet] = true
		}
	}
	names := map[string]bool{}
	success := 0
	for i, r := range c.Results {
		switch {
		case r.Name == spec.SuccessResult:
			success++
			if r.DLQ {
				add("results: 成功 cannot go to the DLQ")
			}
			if r.When == "" && i != len(c.Results)-1 {
				add("results: the 成功 branch without 当 must be the last")
			}
		case names[r.Name]:
			add("results: %s twice", r.Name)
		case !reasonCode(r.Name):
			add("results: %q must be 成功 or an UPPER_SNAKE business outcome", r.Name)
		case r.When == "" && !unmets[r.Name]:
			add("results: %s is neither an op's unmet code nor has 当", r.Name)
		case r.When != "" && unmets[r.Name]:
			add("results: %s is an op's unmet code: it cannot also have 当", r.Name)
		}
		if r.Name != spec.SuccessResult && success > 0 {
			add("results: 成功 must be the last result")
		}
		names[r.Name] = true
		if r.Any && c.Each == "" {
			add("results: %s: %s needs 每项", r.Name, AnyWord)
		}
		if r.DLQ && len(r.Emit) > 0 {
			add("results: %s goes to the DLQ: it cannot emit", r.Name)
		}
	}
	for u := range unmets {
		if !names[u] {
			add("results: the unmet code %s has no result (what does the handler answer?)", u)
		}
	}
	if success == 0 {
		add("results: 成功 is required")
	}
	return out
}
