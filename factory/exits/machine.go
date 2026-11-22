package exits

// R5(docs/design/r5-state-machines.md): 状态机。状态图本身是设计稿里的声明: 工厂生成每个收到的消息类型一个转移处理器、
// 一个超时扫描,并以这个声明为参考模型做基于模型的测试(factory/command/machine.go)。
//
//	种类: 状态机
//	实体: orders                   # 状态存在这张表的行上(主键一列)
//	状态列: status                 # 枚举;状态名 = 枚举的值名
//	进入时间列: updated_at         # 超时的基准(只有转移能写)
//	版本列: version                # 每次转移 / 重发 + 1(超时扫描的乐观锁)
//	重发计数列: state_attempts     # 进入新状态时清零
//	开始: PlaceOrder               # 初始转移: 同一个领域的组合命令(新建实体,状态 = 一个等待状态,发出那个状态的命令)
//	状态:
//	  RESERVING:
//	    命令: { inventory.reserve: { order_id: 实体.id, items: 载入.items } }   # 进入这个状态时发出;超时重发
//	    超时: 30 秒                # 第一次重发
//	    重发间隔: 60 秒            # 之后每隔多久(默认 = 超时)
//	    期限: 15 分钟              # 可选: 超过 → 期限后
//	    期限后: RELEASING          # 或 { 到: X, 发出: [ … ] }
//	  CONFIRMED: { 终态: 是 }
//	载入:                          # 发出的消息要用的子行(和 嵌套 同一种写法;条件 = 实体的键 = 子表的列)
//	  items: { 表: order_items, 条件: [id = order_id], 排序: [line_no 小到大], 上限: 50, 列: [sku, qty] }
//	转移:
//	  - { 从: CHARGING, 收到: payment.succeeded, 守卫: "消息.amount = 实体.total", 到: CONFIRMING }
//	  - { 从: CONFIRMING, 收到: inventory.committed, 到: CONFIRMED, 发出: [ order.confirmed: { order_id: 实体.id } ] }

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"apifactory/factory/ir"
	"apifactory/factory/spec"
)

// MachineResults: 状态机的用例的 结果 的写法(另外: 到的状态名)。
var MachineResults = map[string]string{"忽略": "IGNORED", "拒绝": "REJECTED", "毒消息": "POISON", "重发": "RETRY", "期限": "DEADLINE", "不到期": "NONE"}

func init() {
	Register(&Exit{
		Kind: "machine", Name: "状态机", Cardinality: "one", Command: true,
		DesignKeys: []string{"实体", "状态列", "进入时间列", "版本列", "重发计数列", "创建时间列", "开始", "状态", "载入", "转移"},
		ScalarKeys: []string{"开始"},
		Parse:      parseMachine,
		Check:      checkMachine,
		IR: func(s *spec.Spec, api *ir.API) error {
			api.Command = &ir.Command{Op: "machine"}
			return nil
		},
	})
}

var reDuration = regexp.MustCompile(`^(\d+)\s*(秒|分钟|小时)$`)

// ParseDuration: "30 秒" / "15 分钟" / "1 小时" → 秒。没有天: 长时间的计时是调度器的事(R5 §1)。
func ParseDuration(s string) (int64, error) {
	m := reDuration.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, fmt.Errorf("%q: 写成 N 秒 / N 分钟 / N 小时", s)
	}
	n, _ := strconv.ParseInt(m[1], 10, 64)
	if n <= 0 {
		return 0, fmt.Errorf("%q: 必须是正数", s)
	}
	return n * map[string]int64{"秒": 1, "分钟": 60, "小时": 3600}[m[2]], nil
}

// FormatDuration: 秒 → 设计稿的写法。
func FormatDuration(sec int64) string {
	switch {
	case sec%3600 == 0:
		return fmt.Sprintf("%d 小时", sec/3600)
	case sec%60 == 0:
		return fmt.Sprintf("%d 分钟", sec/60)
	}
	return fmt.Sprintf("%d 秒", sec)
}

func parseMachine(d Design, p *spec.Pattern) {
	m := &spec.Machine{Entity: d.Str("实体"), State: d.Str("状态列"), Entered: d.Str("进入时间列"), Version: d.Str("版本列"),
		Attempts: d.Str("重发计数列"), Created: d.Str("创建时间列"), Start: d.Str("开始")}
	p.Command = &spec.Command{Machine: m}
	for k, v := range map[string]string{"实体": m.Entity, "状态列": m.State, "进入时间列": m.Entered, "版本列": m.Version, "重发计数列": m.Attempts} {
		if v == "" {
			d.Err("状态机: 缺少 %s", k)
		}
	}
	if n := d.Node("状态"); n == nil || n.Kind != yaml.MappingNode {
		d.Err("状态机: 状态 写成 { 状态名: { 命令 / 超时 / 重发间隔 / 期限 / 期限后 } 或 { 终态: 是 } }")
	} else {
		for i := 0; i+1 < len(n.Content); i += 2 {
			m.States = append(m.States, parseState(d, n.Content[i].Value, n.Content[i+1]))
		}
	}
	if n := d.Node("载入"); n != nil {
		if n.Kind != yaml.MappingNode {
			d.Err("载入: 写成 { 名字: { 表, 条件, 排序, 上限, 列 } }")
		}
		for i := 0; i+1 < len(n.Content); i += 2 {
			m.Loads = append(m.Loads, parseLoad(d, n.Content[i].Value, n.Content[i+1]))
		}
	}
	n := d.Node("转移")
	if n == nil || n.Kind != yaml.SequenceNode {
		d.Err("状态机: 转移 写成列表: - { 从, 收到, 守卫?, 到, 发出? }")
		return
	}
	for _, t := range n.Content {
		if t.Kind != yaml.MappingNode {
			d.Err("转移 第 %d 行: 写成 { 从, 收到, 守卫?, 到, 发出? }", t.Line)
			continue
		}
		tr := spec.MachineTransition{}
		for i := 0; i+1 < len(t.Content); i += 2 {
			k, v := t.Content[i].Value, t.Content[i+1]
			switch k {
			case "从":
				tr.From = strings.TrimSpace(v.Value)
			case "收到":
				tr.On = strings.TrimSpace(v.Value)
			case "守卫":
				tr.Guard = strings.TrimSpace(v.Value)
			case "到":
				tr.To = strings.TrimSpace(v.Value)
			case "发出":
				tr.Emit = parseEmits(d, v, "转移的 发出")
			default:
				d.Err("转移 第 %d 行: 不认识的键 %s(从 / 收到 / 守卫 / 到 / 发出)", t.Line, k)
			}
		}
		m.Transitions = append(m.Transitions, tr)
	}
}

func parseState(d Design, name string, n *yaml.Node) spec.MachineState {
	st := spec.MachineState{Name: name}
	if n.Kind != yaml.MappingNode {
		d.Err("状态 %s: 写成 { 命令, 超时, … } 或 { 终态: 是 }", name)
		return st
	}
	dur := func(k string, v *yaml.Node) int64 {
		x, err := ParseDuration(v.Value)
		if err != nil {
			d.Err("状态 %s 的 %s: %v", name, k, err)
		}
		return x
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i].Value, n.Content[i+1]
		switch k {
		case "终态":
			if strings.TrimSpace(v.Value) != "是" {
				d.Err("状态 %s: 终态 只能写 是", name)
			}
			st.Final = true
		case "命令":
			if v.Kind != yaml.MappingNode || len(v.Content) != 2 || v.Content[1].Kind != yaml.MappingNode {
				d.Err("状态 %s 的 命令: 写成 { 消息类型: { 字段: 值 } }(一个状态一个命令)", name)
				continue
			}
			em := parseEmits(d, &yaml.Node{Kind: yaml.SequenceNode, Content: []*yaml.Node{v}}, "状态 "+name+" 的 命令")
			if len(em) == 1 {
				st.Command = &em[0]
			}
		case "超时":
			st.Timeout = dur(k, v)
		case "重发间隔":
			st.Every = dur(k, v)
		case "期限":
			st.Deadline = dur(k, v)
		case "期限后":
			switch v.Kind {
			case yaml.ScalarNode:
				st.DeadlineTo = strings.TrimSpace(v.Value)
			case yaml.MappingNode:
				for j := 0; j+1 < len(v.Content); j += 2 {
					switch v.Content[j].Value {
					case "到":
						st.DeadlineTo = strings.TrimSpace(v.Content[j+1].Value)
					case "发出":
						st.DeadlineEmit = parseEmits(d, v.Content[j+1], "状态 "+name+" 的 期限后 的 发出")
					default:
						d.Err("状态 %s 的 期限后: 不认识的键 %s(到 / 发出)", name, v.Content[j].Value)
					}
				}
			default:
				d.Err("状态 %s 的 期限后: 写成 状态名 或 { 到: 状态名, 发出: […] }", name)
			}
		default:
			d.Err("状态 %s: 不认识的键 %s(命令 / 超时 / 重发间隔 / 期限 / 期限后 / 终态)", name, k)
		}
	}
	if st.Timeout > 0 && st.Every == 0 {
		st.Every = st.Timeout
	}
	return st
}

func parseLoad(d Design, name string, n *yaml.Node) spec.Nest {
	ld := spec.Nest{Name: name}
	if n.Kind != yaml.MappingNode {
		d.Err("载入 %s: 写成 { 表, 条件, 排序, 上限, 列 }", name)
		return ld
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i].Value, n.Content[i+1]
		var items []string
		for _, x := range v.Content {
			items = append(items, strings.TrimSpace(x.Value))
		}
		switch k {
		case "表":
			ld.Table = strings.TrimSpace(v.Value)
		case "条件":
			for _, c := range items {
				if !strings.Contains(c, "=") {
					d.Err("载入 %s 的 条件: 写成 实体的键 = 子表的列", name)
					continue
				}
				ld.On = append(ld.On, c)
			}
		case "排序":
			cols, desc, err := d.OrderList(items)
			if err != nil {
				d.Err("载入 %s: %v", name, err)
			}
			for j, c := range cols {
				ld.OrderBy = append(ld.OrderBy, c+" "+map[bool]string{false: "asc", true: "desc"}[desc[j]])
			}
		case "上限":
			if x, err := strconv.Atoi(strings.TrimSpace(v.Value)); err != nil || x <= 0 {
				d.Err("载入 %s: 上限 必须是正整数(多于上限 = 数据的问题,报错不截断)", name)
			} else {
				ld.Max = x
			}
		case "列":
			for _, c := range items {
				ld.Columns = append(ld.Columns, spec.TakeCol{Name: c, From: c})
			}
		default:
			d.Err("载入 %s: 不认识的键 %s(表 / 条件 / 排序 / 上限 / 列)", name, k)
		}
	}
	return ld
}

// checkMachine: 规格检查(spec 层;状态图的 DRC 在设计编译器里,factory/design/machine.go)。
func checkMachine(s *spec.Spec) []string {
	var out []string
	add := func(f string, a ...any) { out = append(out, fmt.Sprintf("pattern machine: "+f, a...)) }
	c := s.Pattern.Command
	if c == nil || c.Machine == nil {
		return []string{"pattern machine: machine is required"}
	}
	m := c.Machine
	if len(c.Ops) > 0 || len(c.Emit) > 0 || len(c.Results) > 0 || c.Receive != nil {
		add("a machine has no ops / emit / results / receive of its own (transitions are generated)")
	}
	names := map[string]bool{}
	for _, st := range m.States {
		if names[st.Name] {
			add("state %s twice", st.Name)
		}
		names[st.Name] = true
		if st.Final && (st.Command != nil || st.Timeout > 0 || st.Deadline > 0) {
			add("state %s: a final state has no command / timeout / deadline", st.Name)
		}
		if !st.Final && (st.Command == nil || st.Timeout <= 0) {
			add("state %s: a waiting state needs a command and a timeout", st.Name)
		}
		if (st.Deadline > 0) != (st.DeadlineTo != "") {
			add("state %s: deadline and deadline_to go together", st.Name)
		}
	}
	for _, t := range m.Transitions {
		if !names[t.From] || !names[t.To] {
			add("transition %s --%s--> %s: unknown state", t.From, t.On, t.To)
		}
	}
	return out
}

// TakeColNames: 载入的列名。
func TakeColNames(n spec.Nest) []string {
	var out []string
	for _, c := range n.Columns {
		out = append(out, c.From)
	}
	return out
}
