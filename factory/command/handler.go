package command

// 消息处理器(R4,docs/design/r4-events-messages.md §4.2 / §5)和 发出 在模型状态上的执行 —— 和生成的代码、运行时
// (consume / outbox)各自独立实现,状态差分测试比较两边:
//   - 每项: 元素按键排序,逐项执行操作;仅当(前一个幂等新建真的插入了 / 前一个更新真的更新了);不满足: 跳过
//   - 某个操作的条件不满足(声明的原因码)→ 全部回滚(保存点),结果 = 那个原因码
//   - 找不到 / 违反约束 → 毒消息(什么都不提交,也不记"处理过了")
//   - 操作全部做完: 结果的 当 按顺序判定(任一项 = 某一项成立);成立的非成功结果 → 回滚;处理: 死信 → 什么都不提交
//   - 成功: 状态改变,按 成功 的分支发出消息
//   去重(同一个 id)由调用方的"已处理集合"负责(运行时的 factory_processed_messages)。

import (
	"fmt"
	"sort"
	"strings"

	"apifactory/factory/ir"
)

// 消息处理器的结果。
const (
	HandlerOK       = "ok"       // 成功(状态改变,发出 成功 的消息)
	HandlerBusiness = "business" // 声明的业务结果(状态不变,记下处理过了,发出那个结果的消息)
	HandlerDLQ      = "dlq"      // 处理: 死信(什么都不提交;运行时的 outcome = poison)
	HandlerPoison   = "poison"   // 找不到 / 违反约束 / 契约不合法(什么都不提交)
)

// Msg: 发出的一条消息(data 的值是规范值;列表 = []map[string]any)。
type Msg struct {
	Type    string
	Subject string
	Data    map[string]any
}

// HandlerResult: 一条消息的处理结果。Failed = 条件不满足的那一项(从 0 开始,-1 = 没有)。
type HandlerResult struct {
	Outcome  string
	Business string
	Emits    []Msg
	Detail   string
	Failed   int
	Skipped  int // 跳过的更新的次数(覆盖条件)
	Writes   int // 执行了的写操作 + 发出的消息(故障注入: 第 k 个写之后失败)
}

// Processed: 这个结果要不要记下"处理过了"(提交了)。
func (r HandlerResult) Processed() bool {
	return r.Outcome == HandlerOK || r.Outcome == HandlerBusiness
}

// RunHandler: 在模型上处理一条消息。in = data 的字段(数据库的写法),items = 每项 的列表(原来的顺序),env = 消息的 id。
func (s *State) RunHandler(c *ir.Command, in Inputs, items []Inputs, env string, now any) HandlerResult {
	work := s.Clone()
	res := HandlerResult{Failed: -1}
	refs := map[string]int{}
	for i, op := range c.Ops {
		refs[op.Name] = i
	}
	order := items
	if c.Each != "" {
		order = append([]Inputs{}, items...)
		sort.SliceStable(order, func(i, j int) bool { return lessKey(order[i], order[j], c.EachKey) })
	} else {
		order = []Inputs{nil}
	}
	anyHit := map[int]bool{}
	var last []OpResult
	for k, item := range order {
		ops := make([]OpResult, len(c.Ops))
		ec := &evalCtx{in: in, item: item, env: env, now: now, refs: refs, lenient: true}
		for i, op := range c.Ops {
			ec.prev = ops[:i]
			if op.Only != "" {
				j := refs[op.Only]
				flag := ops[j].Created
				if op.OnlyFlag == "已更新" { // 跳过的更新: Ran = false
					flag = ops[j].Ran
				}
				if !flag {
					continue // 仅当 不成立: 不执行(Ran = false,Row = 空)
				}
			}
			ts := work.Tables[op.Table]
			if ts == nil {
				res.Outcome, res.Detail = HandlerPoison, "no table "+op.Table
				return res
			}
			ec.row = nil
			r, outcome, detail := work.exec(ts, op, ec)
			switch {
			case outcome == OK:
				ops[i] = r
				res.Writes++
			case outcome == Unmet && op.Skip:
				ops[i] = OpResult{Row: r.Row}
				res.Skipped++
			case outcome == Unmet:
				res.Outcome, res.Business, res.Failed = HandlerBusiness, op.Unmet, k
				res.Detail = fmt.Sprintf("第 %d 项 %s: %s", k+1, op.Name, detail)
				res.Emits = s.emitsOf(c, op.Unmet, in, items, env, now, nil)
				return res
			default:
				res.Outcome, res.Detail = HandlerPoison, fmt.Sprintf("第 %d 项 %s: %s %s", k+1, op.Name, outcome, detail)
				return res
			}
		}
		ec.prev = ops
		for ri, r := range c.Results {
			if r.When != nil && r.Any {
				if ok, _ := ec.cond(r.When); ok {
					anyHit[ri] = true
				}
			}
		}
		last = ops
	}
	ec := &evalCtx{in: in, env: env, now: now, refs: refs, prev: last, lenient: true}
	for ri, r := range c.Results {
		if r.Name == "" || r.When == nil {
			continue
		}
		hit := anyHit[ri]
		if !r.Any {
			hit, _ = ec.cond(r.When)
		}
		if !hit {
			continue
		}
		if r.DLQ {
			res.Outcome, res.Business, res.Detail = HandlerDLQ, r.Name, Format(r.When)
			return res
		}
		res.Outcome, res.Business, res.Detail = HandlerBusiness, r.Name, Format(r.When)
		res.Emits = s.emitsOf(c, r.Name, in, items, env, now, nil)
		return res
	}
	res.Outcome = HandlerOK
	for _, r := range c.Results {
		if r.Name != "" {
			continue
		}
		if r.When != nil {
			if ok, _ := ec.cond(r.When); !ok {
				continue
			}
		}
		res.Emits = evalEmits(r.Emits, ec, items)
		break
	}
	res.Writes += len(res.Emits)
	s.Tables = work.Tables
	return res
}

// emitsOf: 业务结果 name 的消息(回滚之后: 只能用 消息 的字段 / 常量 / 现在 / 消息的 id)。
func (s *State) emitsOf(c *ir.Command, name string, in Inputs, items []Inputs, env string, now any, prev []OpResult) []Msg {
	for _, r := range c.Results {
		if r.Name == name {
			return evalEmits(r.Emits, &evalCtx{in: in, env: env, now: now, prev: prev, refs: map[string]int{}, lenient: true}, items)
		}
	}
	return nil
}

// RunEmits: 组合命令的 发出(操作全部成功之后;ops = 每个操作的结果)。
func RunEmits(c *ir.Command, in Inputs, now any, ops []OpResult) []Msg {
	refs := map[string]int{}
	for i, op := range c.Ops {
		refs[op.Name] = i
	}
	return evalEmits(c.Emits, &evalCtx{in: in, now: now, refs: refs, prev: ops}, nil)
}

func evalEmits(list []ir.CommandEmit, ec *evalCtx, items []Inputs) []Msg {
	var out []Msg
	for _, em := range list {
		m := Msg{Type: em.Type, Data: map[string]any{}}
		for _, v := range em.Values {
			if v.List {
				var l []map[string]any
				for _, it := range items {
					e := map[string]any{}
					for k, x := range it {
						e[k] = x
					}
					l = append(l, e)
				}
				m.Data[v.Field] = l
				continue
			}
			x, err := ec.eval(v.Expr)
			if err != nil {
				x = "<" + err.Error() + ">"
			}
			m.Data[v.Field] = x
		}
		out = append(out, m)
	}
	return out
}

// SubjectOf: 消息的主体(契约的主体字段的值,文本)。
func SubjectOf(m Msg, field string) string { return ShowRaw(m.Data[field]) }

// ShowRaw: 规范值的文本(主体 / 比较用)。
func ShowRaw(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}

// lessKey: 元素按键排序(整数按数值,文本按字节;和生成的代码的 cmp.Compare 一致)。
func lessKey(a, b Inputs, keys []string) bool {
	for _, k := range keys {
		x, y := a[k], b[k]
		if c := compareRaw(x, y); c != 0 {
			return c < 0
		}
	}
	return false
}

func compareRaw(x, y any) int {
	xi, xok := toInt(x)
	yi, yok := toInt(y)
	if xok && yok {
		switch {
		case xi < yi:
			return -1
		case xi > yi:
			return 1
		}
		return 0
	}
	return strings.Compare(fmt.Sprint(x), fmt.Sprint(y))
}

func toInt(v any) (int64, bool) {
	switch x := v.(type) {
	case int64:
		return x, true
	case int:
		return int64(x), true
	case int32:
		return int64(x), true
	}
	return 0, false
}
