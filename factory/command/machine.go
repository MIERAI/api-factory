package command

// 状态机(R5,docs/design/r5-state-machines.md §5.1)的参考模型: 直接解释 IR 里的状态图(状态、转移、守卫、超时、期限、载入),
// 在模型状态(R3)上执行。和生成的代码(每个消息类型一个条件更新 + 按生成好的迟到集合分类;超时 = SQL 找到期 + Fire)
// 各自独立实现 —— 迟到集合、到期的判定、发出的消息在这里从状态图重新算,不用编译器算好的 Receives.Late。
//
//   收到 T: 找到实体(契约的主体 = 键)→ 不存在 = 拒绝(not_found)
//     当前状态 = T 的 从,守卫成立(或没有)→ 转移: 状态 = 到,进入时间 = now,重发计数 = 0,版本 + 1;
//       发出 = 到 的命令(等待状态)+ 转移的 发出(实体.x = 更新之后的行)
//     当前状态 = 从,守卫不成立 → 拒绝(guard);当前状态能从某个有 T 的转移的 从 到达 → 忽略(late);其他 → 拒绝(impossible)
//   超时(now): 每个等待状态的实体: 进入时间 + 期限 <= now → 期限后;否则 进入时间 + 超时 + 间隔 × 重发计数 <= now → 重发
//     (按 进入时间、键 的顺序,最多 limit 个)

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"apifactory/factory/ir"
)

// 状态机的结果。
const (
	MachineTransition = "transition"
	MachineIgnored    = "ignored"
	MachineRejected   = "rejected"
	MachineRetry      = "retry"
	MachineDeadline   = "deadline"
)

// MachineResult: 一条消息 / 一个实体的超时 在模型上的结果。
type MachineResult struct {
	Outcome  string
	Reason   string // 拒绝: guard / impossible / not_found;载入失败: load
	From, To string
	Key      any
	Emits    []Msg
	Writes   int // 写操作 + 发出的消息(故障注入: 第 k 个写之后失败)
}

// machineEntity: 实体的行(没有 = -1)。
func (s *State) machineEntity(m *ir.Machine, key any) (*TableState, int) {
	ts := s.Tables[m.Table]
	if ts == nil {
		return nil, -1
	}
	k, err := Canon(m.Key.Type, key)
	if err != nil {
		return ts, -1
	}
	for i, r := range ts.Rows {
		if Equal(r[m.Key.Name], k) {
			return ts, i
		}
	}
	return ts, -1
}

// machineEdges: 状态图的边(转移 + 期限)。
func machineEdges(m *ir.Machine) map[string][]string {
	next := map[string][]string{}
	for _, t := range m.Transitions {
		next[t.From] = append(next[t.From], t.To)
	}
	for _, st := range m.States {
		if st.DeadlineTo != "" {
			next[st.Name] = append(next[st.Name], st.DeadlineTo)
		}
	}
	return next
}

// MachineLate: 收到 typ 时当前状态 cur 是不是迟到 / 重复(有 typ 的转移的 从 经过至少一条边能到 cur)。
func MachineLate(m *ir.Machine, typ, cur string) bool {
	next := machineEdges(m)
	for _, t := range m.Transitions {
		if t.On != typ {
			continue
		}
		seen := map[string]bool{}
		stack := append([]string{}, next[t.From]...)
		for len(stack) > 0 {
			x := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if seen[x] {
				continue
			}
			seen[x] = true
			if x == cur {
				return true
			}
			stack = append(stack, next[x]...)
		}
	}
	return false
}

// Deliver: 在模型上处理一条消息(in = data,数据库的写法;now = 规范的时刻)。转移 → 状态改变;其他 → 不变。
func (s *State) Deliver(m *ir.Machine, typ string, in Inputs, now any) MachineResult {
	var tr *ir.MachineTransition
	for i := range m.Transitions {
		if m.Transitions[i].On == typ {
			tr = &m.Transitions[i]
		}
	}
	ct := m.Contract(typ)
	if tr == nil || ct == nil {
		return MachineResult{Outcome: MachineRejected, Reason: "impossible"}
	}
	ts, i := s.machineEntity(m, in[ct.Subject])
	if i < 0 {
		return MachineResult{Outcome: MachineRejected, Reason: "not_found", Key: in[ct.Subject]}
	}
	row := ts.Rows[i]
	cur := s.machineState(m, row)
	res := MachineResult{From: cur, Key: row[m.Key.Name]}
	if cur == tr.From {
		ok := true
		if tr.Guard != nil {
			ec := &evalCtx{in: in, now: now, prev: []OpResult{{Ran: true, Row: row}}, refs: map[string]int{"实体": 0}}
			ok, _ = ec.cond(tr.Guard)
		}
		if !ok {
			res.Outcome, res.Reason = MachineRejected, "guard"
			return res
		}
		to := m.StateByName(tr.To)
		return s.machineEnter(m, ts, i, to, transitionMsgs(to, tr.Emits), in, now, MachineTransition)
	}
	if MachineLate(m, typ, cur) {
		res.Outcome, res.Reason = MachineIgnored, "late"
		return res
	}
	res.Outcome, res.Reason = MachineRejected, "impossible"
	return res
}

func transitionMsgs(to *ir.MachineState, extra []ir.CommandEmit) []ir.CommandEmit {
	var out []ir.CommandEmit
	if to.Command != nil {
		out = append(out, *to.Command)
	}
	return append(out, extra...)
}

func (s *State) machineState(m *ir.Machine, row Row) string {
	n, _ := strconv.ParseInt(fmt.Sprint(row[m.State.Name]), 10, 64)
	if st := m.StateByDB(n); st != nil {
		return st.Name
	}
	return fmt.Sprintf("<%v>", row[m.State.Name])
}

func incrCanon(v any) any {
	n, _ := strconv.ParseInt(fmt.Sprint(v), 10, 64)
	return strconv.FormatInt(n+1, 10)
}

// machineEnter: 进入 to(转移 / 期限): 状态、进入时间、重发计数清零、版本 + 1,发出。载入失败 → 不变,Outcome = rejected(load)。
func (s *State) machineEnter(m *ir.Machine, ts *TableState, i int, to *ir.MachineState, emits []ir.CommandEmit, in Inputs, now any, outcome string) MachineResult {
	old := ts.Rows[i]
	row := old.clone()
	row[m.State.Name] = strconv.FormatInt(to.DB, 10)
	row[m.Entered] = now
	row[m.Attempts] = "0"
	row[m.Version] = incrCanon(row[m.Version])
	res := MachineResult{Outcome: outcome, From: s.machineState(m, old), To: to.Name, Key: row[m.Key.Name]}
	msgs, err := s.machineMsgs(m, emits, row, in, now)
	if err != nil {
		return MachineResult{Outcome: MachineRejected, Reason: "load", From: res.From, Key: res.Key}
	}
	ts.Rows[i] = row
	res.Emits = msgs
	res.Writes = 1 + len(msgs)
	return res
}

// machineMsgs: 发出的消息(实体.x = 写之后的行;载入.名字 = 子表的行)。
func (s *State) machineMsgs(m *ir.Machine, emits []ir.CommandEmit, row Row, in Inputs, now any) ([]Msg, error) {
	loads := map[string][]Inputs{}
	for _, em := range emits {
		for _, v := range em.Values {
			if v.List && v.Expr.Op == "ref" {
				rows, err := s.machineLoad(m, v.Expr.Name, row[m.Key.Name])
				if err != nil {
					return nil, err
				}
				loads[v.Expr.Name] = rows
			}
		}
	}
	ec := &evalCtx{in: in, now: now, prev: []OpResult{{Ran: true, Row: row}}, refs: map[string]int{"实体": 0}, loads: loads}
	return evalEmits(emits, ec, nil), nil
}

// machineLoad: 载入: 子表里 Child = 键 的行,按 OrderBy 排序;多于 Max 行 = 错误。
func (s *State) machineLoad(m *ir.Machine, name string, key any) ([]Inputs, error) {
	var ld *ir.MachineLoad
	for i := range m.Loads {
		if m.Loads[i].Name == name {
			ld = &m.Loads[i]
		}
	}
	if ld == nil {
		return nil, fmt.Errorf("no load %s", name)
	}
	ts := s.Tables[ld.Table]
	if ts == nil {
		return nil, fmt.Errorf("load %s: no table %s", name, ld.Table)
	}
	var rows []Row
	for _, r := range ts.Rows {
		if Equal(r[ld.Child], key) {
			rows = append(rows, r)
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		for k, c := range ld.OrderBy {
			if x := canonCompare(rows[i][c], rows[j][c]); x != 0 {
				return (x < 0) != ld.Desc[k]
			}
		}
		return false
	})
	if len(rows) > ld.Max {
		return nil, fmt.Errorf("load %s: %d rows > %d", name, len(rows), ld.Max)
	}
	var out []Inputs
	for _, r := range rows {
		e := Inputs{}
		for _, c := range ld.Columns {
			e[c.Name] = r[c.Name]
		}
		out = append(out, e)
	}
	return out, nil
}

// canonCompare: 两个规范值的顺序(整数按数值,其他按文本的字节序;空在前)。
func canonCompare(a, b any) int {
	if a == nil || b == nil {
		switch {
		case a == nil && b == nil:
			return 0
		case a == nil:
			return -1
		}
		return 1
	}
	x, e1 := strconv.ParseInt(fmt.Sprint(a), 10, 64)
	y, e2 := strconv.ParseInt(fmt.Sprint(b), 10, 64)
	if e1 == nil && e2 == nil {
		switch {
		case x < y:
			return -1
		case x > y:
			return 1
		}
		return 0
	}
	return strings.Compare(fmt.Sprint(a), fmt.Sprint(b))
}

// MachineDue: 一个实体在 now 到期了吗(期限优先)。返回 "" / retry / deadline。
func MachineDue(m *ir.Machine, st *ir.MachineState, entered time.Time, attempts int64, now time.Time) string {
	if st == nil || st.Final {
		return ""
	}
	if st.Deadline > 0 && !entered.Add(time.Duration(st.Deadline)*time.Second).After(now) {
		return MachineDeadline
	}
	if !entered.Add(time.Duration(st.Timeout+st.Every*attempts) * time.Second).After(now) {
		return MachineRetry
	}
	return ""
}

// Timeouts: 在模型上跑一轮超时(now;最多 limit 个,按 进入时间、键)。返回每个处理了的实体的结果(状态改变)。
func (s *State) Timeouts(m *ir.Machine, now time.Time, limit int) []MachineResult {
	ts := s.Tables[m.Table]
	if ts == nil {
		return nil
	}
	type cand struct {
		key     any
		entered time.Time
		kind    string
	}
	var due []cand
	for _, r := range ts.Rows {
		n, _ := strconv.ParseInt(fmt.Sprint(r[m.State.Name]), 10, 64)
		st := m.StateByDB(n)
		e, err := time.Parse(time.RFC3339Nano, fmt.Sprint(r[m.Entered]))
		if err != nil {
			continue
		}
		att, _ := strconv.ParseInt(fmt.Sprint(r[m.Attempts]), 10, 64)
		if k := MachineDue(m, st, e, att, now); k != "" {
			due = append(due, cand{r[m.Key.Name], e, k})
		}
	}
	sort.SliceStable(due, func(i, j int) bool {
		if !due[i].entered.Equal(due[j].entered) {
			return due[i].entered.Before(due[j].entered)
		}
		return canonCompare(due[i].key, due[j].key) < 0
	})
	if len(due) > limit {
		due = due[:limit]
	}
	nowC, _ := Canon("timestamptz", now)
	var out []MachineResult
	for _, d := range due {
		ts, i := s.machineEntity(m, d.key)
		if i < 0 {
			continue
		}
		cur := m.StateByName(s.machineState(m, ts.Rows[i]))
		switch d.kind {
		case MachineDeadline:
			to := m.StateByName(cur.DeadlineTo)
			out = append(out, s.machineEnter(m, ts, i, to, transitionMsgs(to, cur.DeadlineEmits), nil, nowC, MachineDeadline))
		default:
			old := ts.Rows[i]
			row := old.clone()
			row[m.Attempts] = incrCanon(row[m.Attempts])
			row[m.Version] = incrCanon(row[m.Version])
			msgs, err := s.machineMsgs(m, []ir.CommandEmit{*cur.Command}, row, nil, nowC)
			if err != nil {
				out = append(out, MachineResult{Outcome: MachineRejected, Reason: "load", From: cur.Name, Key: d.key})
				continue
			}
			ts.Rows[i] = row
			out = append(out, MachineResult{Outcome: MachineRetry, From: cur.Name, To: cur.Name, Key: d.key, Emits: msgs, Writes: 1 + len(msgs)})
		}
	}
	return out
}
