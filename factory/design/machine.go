package design

// 状态机(R5,docs/design/r5-state-machines.md)在设计编译器里的部分: 实体表的要求、状态 = 状态列的枚举的值名、
// 契约(收到的 / 发出的)、表达式(实体.x / 消息.x / 载入.名字)、状态图的 DRC(§4.1)、开始(初始转移)的检查、
// 别的写接口不能写状态 / 进入时间 / 重发计数(§3.1)、例子、machines.lock.yaml(§8 风险 2)。

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"apifactory/factory/command"
	"apifactory/factory/exits"
	"apifactory/factory/kinds"
	"apifactory/factory/spec"
)

// EntityRef / LoadPrefix: 状态机的表达式里实体的列(实体.x)、载入的子行(载入.名字,只能原样写进列表字段)。
const (
	EntityRef  = "实体"
	LoadPrefix = "载入."
)

func (c *ctx) compileMachine(a *API) (*spec.Spec, error) {
	e := &errs{file: a.Path}
	cmd := a.Pattern.Command
	m := cmd.Machine
	if len(a.Inputs) > 0 || len(a.Outputs) > 0 || a.Source != "" {
		e.add(1, "状态机: 不写 输入 / 输出 / 数据来源(实体、状态、转移都在状态机的键里)")
	}
	t := c.d.Tables[m.Entity]
	if _, ok := c.cat[m.Entity]; !ok || t == nil {
		e.add(1, "实体 %s 不是这个领域的表(领域.yaml 的 表 + db/schema)", m.Entity)
		return nil, e.err()
	}
	if len(t.PrimaryKey) != 1 {
		e.add(1, "实体 %s: 主键必须是一列(消息的主体 = 实体的键)", m.Entity)
	}
	colType := func(col string) string {
		cc, _ := c.column(m.Entity, col)
		return cc.Type
	}
	need := func(what, col string, ok func(string) bool, want string) {
		if col == "" {
			return
		}
		cc, found := c.column(m.Entity, col)
		switch {
		case !found:
			e.add(1, "%s %s 不是表 %s 的列", what, col, m.Entity)
		case !ok(cc.Type) || !cc.NotNull:
			e.add(1, "%s %s 必须是 NOT NULL 的%s列(现在是 %s)", what, col, want, cc.Type)
		}
	}
	isInt := func(t string) bool { return t == "int4" || t == "int8" || t == "int2" }
	isTS := func(t string) bool { return t == "timestamptz" }
	need("状态列", m.State, isInt, "整数")
	need("进入时间列", m.Entered, isTS, " timestamptz ")
	need("版本列", m.Version, isInt, "整数")
	need("重发计数列", m.Attempts, isInt, "整数")
	need("创建时间列", m.Created, isTS, " timestamptz ")
	en := c.d.Enum(t.ColEnum[m.State])
	if en == nil {
		e.add(1, "状态列 %s 要在领域.yaml 里声明 枚举(状态名 = 枚举的值名)", m.State)
		return nil, e.err()
	}
	enumDB := map[string]int64{}
	for _, v := range en.Values {
		enumDB[v.Name] = v.DB
	}
	states := map[string]*spec.MachineState{}
	for i := range m.States {
		st := &m.States[i]
		if _, ok := enumDB[st.Name]; !ok {
			e.add(1, "状态 %s 不是枚举 %s 的值名", st.Name, en.Name)
		}
		states[st.Name] = st
		switch {
		case st.Final && (st.Command != nil || st.Timeout > 0 || st.Deadline > 0 || st.DeadlineTo != ""):
			e.add(1, "状态 %s: 终态不写 命令 / 超时 / 期限", st.Name)
		case !st.Final && st.Command == nil:
			e.add(1, "状态 %s: 等待状态要写 命令(进入时发出、超时重发)", st.Name)
		case !st.Final && st.Timeout <= 0: // §4.1: 没有超时的等待状态在依赖丢消息时会永远卡住
			e.add(1, "状态 %s: 等待状态要写 超时(依赖丢了消息也能继续)", st.Name)
		case (st.Deadline > 0) != (st.DeadlineTo != ""):
			e.add(1, "状态 %s: 期限 和 期限后 一起写", st.Name)
		case st.Deadline > 0 && st.Deadline <= st.Timeout:
			e.add(1, "状态 %s: 期限(%s)要比 超时(%s)长", st.Name, exits.FormatDuration(st.Deadline), exits.FormatDuration(st.Timeout))
		}
	}
	if err := e.err(); err != nil {
		return nil, err
	}
	for _, st := range m.States {
		if st.DeadlineTo != "" && states[st.DeadlineTo] == nil {
			e.add(1, "状态 %s 的 期限后 %s 不是状态", st.Name, st.DeadlineTo)
		}
	}

	// ---- 载入(和 嵌套 同一种写法;条件 = 实体的键 = 子表的列)
	loads := map[string]*spec.Nest{}
	for i := range m.Loads {
		ld := &m.Loads[i]
		where := "载入 " + ld.Name
		loads[ld.Name] = ld
		if _, ok := c.cat[ld.Table]; !ok || c.d.Tables[ld.Table] == nil {
			e.add(1, "%s: 表 %s 不是这个领域的表", where, ld.Table)
			continue
		}
		if len(ld.On) != 1 {
			e.add(1, "%s: 条件 写一项: 实体的键 = 子表的列", where)
			continue
		}
		l, r, _ := strings.Cut(ld.On[0], "=")
		l, r = strings.TrimSpace(l), strings.TrimSpace(r)
		if len(t.PrimaryKey) == 1 && l != t.PrimaryKey[0] {
			e.add(1, "%s: 条件的左边要是实体的键 %s", where, t.PrimaryKey[0])
		}
		if _, ok := c.column(ld.Table, r); !ok {
			e.add(1, "%s: 表 %s 没有列 %s", where, ld.Table, r)
		}
		ld.On[0] = l + " = " + r
		if ld.Max <= 0 || len(ld.Columns) == 0 || len(ld.OrderBy) == 0 {
			e.add(1, "%s: 要写 排序、上限、列", where)
		}
		for _, cl := range ld.Columns {
			if _, ok := c.column(ld.Table, cl.From); !ok {
				e.add(1, "%s: 表 %s 没有列 %s", where, ld.Table, cl.From)
			}
		}
		ordered := map[string]bool{r: true}
		for _, o := range ld.OrderBy {
			col, _, _ := strings.Cut(o, " ")
			if _, ok := c.column(ld.Table, col); !ok {
				e.add(1, "%s: 排序的列 %s 不是表 %s 的列", where, col, ld.Table)
			}
			ordered[col] = true
		}
		total := true // 子行的顺序是全序: 条件的列 + 排序的列 覆盖子表的主键
		for _, k := range c.d.Tables[ld.Table].PrimaryKey {
			total = total && ordered[k]
		}
		if !total {
			e.add(1, "%s: 条件和排序的列要覆盖表 %s 的主键 %v(子行的顺序必须确定)", where, ld.Table, c.d.Tables[ld.Table].PrimaryKey)
		}
	}
	c.loadList = func(name string, elem []spec.ContractField) error {
		ld := loads[name]
		if ld == nil {
			return fmt.Errorf("载入.%s: 没有这个载入", name)
		}
		for _, f := range elem {
			found := false
			for _, cl := range ld.Columns {
				if cl.From != f.Name {
					continue
				}
				found = true
				cc, _ := c.column(ld.Table, cl.From)
				want, err := sqlOfType(f.Type, f.Name)
				if err != nil {
					return err
				}
				if cc.Type != want && !(want == "int8" && isInt(cc.Type)) {
					return fmt.Errorf("载入.%s 的列 %s 是 %s,契约的元素是 %s(%s)", name, cl.From, cc.Type, f.Type, want)
				}
				if !cc.NotNull && f.Required {
					return fmt.Errorf("载入.%s 的列 %s 可能为空,契约的元素 %s 是必填", name, cl.From, f.Name)
				}
			}
			if !found {
				return fmt.Errorf("载入.%s 没有列 %s(契约的列表元素的字段)", name, f.Name)
			}
		}
		return nil
	}
	defer func() { c.loadList = nil }()

	// ---- 表达式的作用域: 实体.x(实体的列)、消息.x(收到的契约的字段)
	entityScope := func(inputs map[string]*Input) command.Scope {
		return command.Scope{
			Ref: func(op, col string) (string, error) {
				if op != EntityRef {
					return "", fmt.Errorf("%s.%s: 状态机的表达式里只有 实体.列 / 消息.字段 / 载入.名字", op, col)
				}
				cc, ok := c.column(m.Entity, col)
				if !ok {
					return "", fmt.Errorf("实体.%s: 表 %s 没有列 %s", col, m.Entity, col)
				}
				return cc.Type, nil
			},
			Input: func(name string) (string, error) {
				in := inputs[name]
				if in == nil {
					if len(inputs) == 0 {
						return "", fmt.Errorf("消息.%s: 这里没有收到的消息(状态的命令 / 期限后的发出 只能用 实体.列 / 载入)", name)
					}
					return "", fmt.Errorf("消息.%s: 收到的消息没有字段 %s", name, name)
				}
				if in.Type == GroupListType {
					return "", fmt.Errorf("消息.%s 是列表: 状态机不能逐项处理", name)
				}
				return c.inputSQL(in)
			},
			RefEnum: func(op, col string) (string, bool) { return m.Entity, op == EntityRef && t.ColEnum[col] != "" },
			Enum: func(table, col, name string) (string, bool) {
				tt := c.d.Tables[table]
				if tt == nil || tt.ColEnum[col] == "" {
					return "", false
				}
				for _, v := range c.d.Enum(tt.ColEnum[col]).Values {
					if v.Name == name {
						return strconv.FormatInt(v.DB, 10), true
					}
				}
				return "", false
			},
			EnumOf: func(table, col string) bool { tt := c.d.Tables[table]; return tt != nil && tt.ColEnum[col] != "" },
		}
	}
	none := map[string]*Input{}

	// ---- 收到的消息: 契约存在,主体 = 实体的键
	received := map[string]map[string]*Input{}
	for _, tr := range m.Transitions {
		if _, done := received[tr.On]; done || tr.On == "" {
			continue
		}
		ct := c.emitContract(a, tr.On, e) // data 的 proto 消息(和发出的同一个),契约进 spec
		if ct == nil {
			continue
		}
		ins := map[string]*Input{}
		for _, in := range inputsOf(ct.Fields) {
			ins[in.Name] = in
		}
		received[tr.On] = ins
		subj := ins[ct.Subject]
		if subj == nil || len(t.PrimaryKey) != 1 {
			continue
		}
		st, err := c.inputSQL(subj)
		kt := colType(t.PrimaryKey[0])
		if err != nil || (st != kt && !(isInt(st) && isInt(kt))) {
			e.add(1, "收到 %s: 契约的主体 %s(%s)要和实体的键 %s(%s)同类型(主体 = 哪个实体)", tr.On, ct.Subject, subj.Type, t.PrimaryKey[0], kt)
		}
	}

	// ---- 状态的命令 / 期限后的发出 / 转移的发出 和 守卫
	for i := range m.States {
		st := &m.States[i]
		if st.Command != nil {
			list := []spec.Emit{*st.Command}
			c.emits(a, list, entityScope(none), none, "状态 "+st.Name+" 的 命令", e)
			st.Command = &list[0]
			if ct := c.contracts.byTyp[st.Command.Type]; ct != nil && ct.Kind != "command" {
				e.add(1, "状态 %s 的 命令 %s 是事件: 等待状态发出的是命令(要对方回答;事件写在转移的 发出 里)", st.Name, st.Command.Type)
			}
		}
		c.emits(a, st.DeadlineEmit, entityScope(none), none, "状态 "+st.Name+" 的 期限后 的 发出", e)
		if st.DeadlineTo != "" {
			if to := states[st.DeadlineTo]; to != nil && to.Command != nil {
				for _, em := range st.DeadlineEmit {
					if em.Type == to.Command.Type {
						e.add(1, "状态 %s 的 期限后: %s 是 %s 的命令,进入 %s 时自动发出,不要再写", st.Name, em.Type, st.DeadlineTo, st.DeadlineTo)
					}
				}
			}
		}
	}
	seen := map[[2]string]bool{}
	fromOf := map[string]string{}
	for i := range m.Transitions {
		tr := &m.Transitions[i]
		where := fmt.Sprintf("转移 %s --%s--> %s", tr.From, tr.On, tr.To)
		from, to := states[tr.From], states[tr.To]
		switch {
		case from == nil || to == nil:
			e.add(1, "%s: 从 / 到 要是 状态 里的状态", where)
			continue
		case tr.On == "":
			e.add(1, "%s: 缺少 收到", where)
			continue
		case from.Final:
			e.add(1, "%s: 终态没有出边(终态不再变化)", where)
		case seen[[2]string{tr.From, tr.On}]:
			e.add(1, "%s: 同一个 (从, 收到) 只能有一条转移(守卫 + 否则 的多分支还不支持)", where)
		case fromOf[tr.On] != "" && fromOf[tr.On] != tr.From:
			e.add(1, "%s: 消息 %s 已经在 %s 上有转移: 一个消息类型只能从一个状态转移(还不支持)", where, tr.On, fromOf[tr.On])
		case tr.From == tr.To:
			e.add(1, "%s: 不能转移到自己(重发由 超时 负责)", where)
		}
		seen[[2]string{tr.From, tr.On}] = true
		fromOf[tr.On] = tr.From
		ins := received[tr.On]
		if ins == nil {
			continue
		}
		if tr.Guard != "" {
			tree, err := command.ParseExpr(tr.Guard, true)
			if err == nil {
				err = command.Resolve(tree, entityScope(ins), "", "", "")
			}
			if err == nil && len(command.Refs(tree)) == 0 {
				err = fmt.Errorf("守卫要比较 实体.列(只看消息的条件应该由发送方保证)")
			}
			if err != nil {
				e.add(1, "%s 的 守卫: %v", where, err)
			} else {
				tr.Guard = command.Format(tree)
			}
		}
		c.emits(a, tr.Emit, entityScope(ins), ins, where+" 的 发出", e)
		if to.Command != nil {
			for _, em := range tr.Emit {
				if em.Type == to.Command.Type {
					e.add(1, "%s 的 发出: %s 是 %s 的命令,进入 %s 时自动发出,不要再写", where, em.Type, tr.To, tr.To)
				}
			}
		}
	}
	if err := e.err(); err != nil {
		return nil, err
	}

	// ---- 开始: 同一个领域的组合命令,新建实体,状态 = 一个等待状态,发出那个状态的命令
	initial := c.machineStart(a, m, en, enumDB, states, e)
	// ---- 状态图: 从初始状态可达、能到达终态
	if initial != "" {
		next := map[string][]string{}
		prev := map[string][]string{}
		edge := func(a, b string) { next[a] = append(next[a], b); prev[b] = append(prev[b], a) }
		for _, tr := range m.Transitions {
			edge(tr.From, tr.To)
		}
		for _, st := range m.States {
			if st.DeadlineTo != "" {
				edge(st.Name, st.DeadlineTo)
			}
		}
		reach := closure([]string{initial}, next)
		var finals []string
		for _, st := range m.States {
			if st.Final {
				finals = append(finals, st.Name)
			}
		}
		toFinal := closure(finals, prev)
		for _, st := range m.States {
			if !reach[st.Name] {
				e.add(1, "状态 %s: 从初始状态 %s 到不了(没用的状态)", st.Name, initial)
			}
			if !st.Final && !toFinal[st.Name] {
				e.add(1, "状态 %s: 到不了任何终态(会永远停在这里)", st.Name)
			}
		}
		if len(finals) == 0 {
			e.add(1, "状态机: 至少要有一个终态")
		}
	}
	c.machineWriters(a, m, e)
	if err := e.err(); err != nil {
		return nil, err
	}
	if err := c.machineLock(a, m, enumDB); err != nil {
		e.add(1, "%v", err)
		return nil, e.err()
	}

	// ---- 机器用的式样书
	s := &spec.Spec{SpecVersion: spec.Version}
	s.API = spec.API{RPC: c.pkg + "." + c.handlerService() + "/" + a.Name, Owner: a.Owner, RiskLevel: a.Risk, Description: a.Background, Hot: a.Hot, Load: a.Load}
	s.Pattern = a.Pattern
	s.Pattern.Kind = a.Kind
	for _, r := range a.Rules {
		s.Behavior = append(s.Behavior, spec.Behavior{When: r.When, Then: r.Then})
	}
	for _, qa := range a.Reviewed {
		s.ReviewDecisions = append(s.ReviewDecisions, spec.ReviewDecision{Question: qa.Question, Decision: qa.Answer})
	}
	if err := c.machineExamples(a, s, received); err != nil {
		return nil, err
	}
	req := &message{Name: a.Name + "Request", Desc: "状态机 " + a.Name + " 没有请求(只是类型的说明: 转移的入口是收到的消息,超时由扫描任务驱动)"}
	resp := &message{Name: a.Name + "Response", Desc: "状态机 " + a.Name + " 没有响应"}
	c.addMessage(req)
	c.addMessage(resp)
	var recv []string
	for typ := range received {
		recv = append(recv, typ)
	}
	sort.Strings(recv)
	c.handlerRPCs = append(c.handlerRPCs, fmt.Sprintf("  // %s(状态机: 收到 %s;超时由扫描任务驱动)\n  rpc %s(%s) returns (%s);",
		oneLine(a.Desc), strings.Join(recv, " / "), a.Name, req.Name, resp.Name))
	return s, nil
}

func closure(start []string, next map[string][]string) map[string]bool {
	seen := map[string]bool{}
	stack := append([]string{}, start...)
	for len(stack) > 0 {
		x := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[x] {
			continue
		}
		seen[x] = true
		stack = append(stack, next[x]...)
	}
	return seen
}

// machineStart: 开始的 API 新建实体(状态 = 等待状态的值名或数据库值)并发出那个状态的命令。返回初始状态。
func (c *ctx) machineStart(a *API, m *spec.Machine, en *Enum, enumDB map[string]int64, states map[string]*spec.MachineState, e *errs) string {
	if m.Start == "" {
		e.add(1, "状态机: 要写 开始(新建实体的组合命令: 初始转移)")
		return ""
	}
	var start *API
	for _, x := range c.apis {
		if x.Name == m.Start {
			start = x
		}
	}
	if start == nil || start.Kind != "command" || start.Pattern.Command == nil {
		e.add(1, "开始 %s: 不是这个领域的组合命令", m.Start)
		return ""
	}
	initial := ""
	for _, op := range start.Pattern.Command.Ops {
		if op.Kind != "insert" || op.Table != m.Entity {
			continue
		}
		for _, v := range op.Values {
			if v.Column != m.State {
				continue
			}
			x := strings.TrimSpace(v.Expr)
			for name, db := range enumDB {
				if x == name || x == strconv.FormatInt(db, 10) {
					initial = name
				}
			}
		}
	}
	st := states[initial]
	switch {
	case initial == "":
		e.add(1, "开始 %s: 要 新建 %s,状态 %s 写成一个状态的值名(初始状态)", m.Start, m.Entity, m.State)
		return ""
	case st == nil || st.Final:
		e.add(1, "开始 %s: 初始状态 %s 要是状态机的等待状态", m.Start, initial)
		return ""
	}
	emitted := false
	for _, em := range start.Pattern.Command.Emit {
		emitted = emitted || (st.Command != nil && em.Type == st.Command.Type)
	}
	if !emitted {
		e.add(1, "开始 %s: 要发出初始状态 %s 的命令 %s(和新建同一个事务)", m.Start, initial, st.Command.Type)
	}
	return initial
}

// machineWriters: §3.1 的 DRC: 状态列、进入时间列、重发计数列只有状态机的转移能写(开始的新建除外)。
func (c *ctx) machineWriters(a *API, m *spec.Machine, e *errs) {
	owned := map[string]bool{m.State: true, m.Entered: true, m.Attempts: true}
	for _, x := range c.apis {
		if x == a || x.Pattern.Command == nil {
			continue
		}
		for _, op := range x.Pattern.Command.Ops {
			if op.Table != m.Entity || (x.Name == m.Start && op.Kind == "insert") {
				continue
			}
			for _, v := range op.Values {
				if owned[v.Column] {
					e.add(1, "%s 的操作 %s 写了 %s.%s: 状态 / 进入时间 / 重发计数 只有状态机 %s 的转移能写(超时的基准会被打乱)",
						x.Name, op.Name, m.Entity, v.Column, a.Name)
				}
			}
		}
		for _, in := range x.Inputs {
			if (in.Write != "" && owned[in.Write]) && x.Source == m.Entity {
				e.add(1, "%s 的输入 %s 写了 %s.%s: 只有状态机 %s 的转移能写", x.Name, in.Name, m.Entity, in.Write, a.Name)
			}
		}
	}
}

// machineLockFile: 状态机的锁文件(§8 风险 2,⚖️ S7): 部署过的状态只能新增;删除 / 改名要先确认在途的实例数 = 0。
type machineLockFile map[string]machineLock

type machineLock struct {
	States  map[string]int64 `yaml:"状态"`
	Retired []string         `yaml:"已下线,omitempty"`
}

func (c *ctx) machineLock(a *API, m *spec.Machine, enumDB map[string]int64) error {
	path := filepath.Join(c.d.Dir, "machines.lock.yaml")
	lf := machineLockFile{}
	if b, err := os.ReadFile(path); err == nil {
		if err := yaml.Unmarshal(b, &lf); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	}
	cur := lf[a.Name]
	have := map[string]bool{}
	for _, st := range m.States {
		have[st.Name] = true
	}
	var problems []string
	for name, db := range cur.States {
		retired := contains(cur.Retired, name)
		switch {
		case !have[name] && !retired:
			problems = append(problems, fmt.Sprintf("状态 %s(数据库值 %d)部署过,现在没有了: 部署前确认在途的实例数 = 0"+
				"(SELECT count(*) FROM %s WHERE %s = %d),然后在 %s 的 %s.已下线 里写上 %s", name, db, m.Entity, m.State, db, path, a.Name, name))
		case have[name] && enumDB[name] != db:
			problems = append(problems, fmt.Sprintf("状态 %s 的数据库值从 %d 变成了 %d: 在途的实例会变成别的状态", name, db, enumDB[name]))
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("%s", strings.Join(problems, "\n"))
	}
	next := machineLock{States: map[string]int64{}, Retired: cur.Retired}
	for name, db := range cur.States {
		if contains(cur.Retired, name) {
			next.States[name] = db
		}
	}
	for _, st := range m.States {
		next.States[st.Name] = enumDB[st.Name]
	}
	lf[a.Name] = next
	b, err := yaml.Marshal(lf)
	if err != nil {
		return err
	}
	head := "# 由设计编译器维护: 部署过的状态机的状态(只能新增;删除 / 改名的状态要先确认在途的实例数 = 0,再写进 已下线)\n"
	return os.WriteFile(path, append([]byte(head), b...), 0o644)
}

// machineExamples: 例子 → acceptance(准备数据 + 一个事件: 消息 或 超时 + 结果 + 发出 + 之后)。
func (c *ctx) machineExamples(a *API, s *spec.Spec, received map[string]map[string]*Input) error {
	wrap := func(err error) error { return fmt.Errorf("%s: 例子: %w", a.Path, err) }
	m := a.Pattern.Command.Machine
	s.Acceptance.Fixtures = map[string][]map[string]any{}
	if a.Fixtures != nil {
		for _, p := range pairs(a.Fixtures) {
			rows, err := c.fixtureRows(p.Key, p.Val.Content)
			if err != nil {
				return wrap(fmt.Errorf("准备数据 %s: %w", p.Key, err))
			}
			s.Acceptance.Fixtures[p.Key] = rows
		}
	}
	for _, t := range append([]string{m.Entity}, loadTables(m)...) { // 准备数据里没写 = 空表
		if _, ok := s.Acceptance.Fixtures[t]; !ok {
			s.Acceptance.Fixtures[t] = []map[string]any{}
		}
	}
	for _, n := range a.Cases {
		cs := spec.Case{Name: str(n, "说明")}
		msg, to := get(n, "消息"), get(n, "超时")
		switch {
		case (msg == nil) == (to == nil):
			return wrap(fmt.Errorf("第 %d 行: 状态机的用例写 消息: { 类型: { 字段: 值 } } 或者 超时: 是(跑一轮超时)", n.Line))
		case msg != nil:
			if msg.Kind != yaml.MappingNode || len(msg.Content) != 2 {
				return wrap(fmt.Errorf("第 %d 行: 消息 写成 { 消息类型: { 字段: 值 } }", msg.Line))
			}
			typ := strings.TrimSpace(msg.Content[0].Value)
			if received[typ] == nil {
				return wrap(fmt.Errorf("第 %d 行: 消息 %s: 状态机不收这个消息", msg.Line, typ))
			}
			data, err := c.request(c.msgByNm[protoName(c.contracts.byTyp[typ])], msg.Content[1])
			if err != nil {
				return wrap(err)
			}
			cs.Message = &spec.EmittedMsg{Type: typ, Data: data}
		default:
			if strings.TrimSpace(to.Value) != "是" {
				return wrap(fmt.Errorf("第 %d 行: 超时 只能写 是(在用例的 现在 跑一轮超时)", to.Line))
			}
			cs.Timeout = true
		}
		res := strings.TrimSpace(str(n, "结果"))
		code, ok := exits.MachineResults[res]
		if !ok {
			for _, st := range m.States {
				if st.Name == res {
					code, ok = "STATE:"+res, true
				}
			}
		}
		if !ok {
			return wrap(fmt.Errorf("第 %d 行: 结果 %q: 写 到的状态名 / 忽略 / 拒绝 / 毒消息(消息);重发 / 期限 / 不到期(超时)", n.Line, res))
		}
		if cs.Timeout != (code == "RETRY" || code == "DEADLINE" || code == "NONE") && !strings.HasPrefix(code, "STATE:") {
			return wrap(fmt.Errorf("第 %d 行: 结果 %s 和 %s 不配", n.Line, res, map[bool]string{true: "超时", false: "消息"}[cs.Timeout]))
		}
		cs.Expect.Code = code
		var err error
		if _, cs.After, err = c.commandCase(a, &message{}, withKey(n, "结果", "{}")); err != nil {
			return wrap(err)
		}
		if cs.Emitted, err = c.emittedCase(a, n); err != nil {
			return wrap(err)
		}
		now := str(n, "现在")
		if now == "" {
			now = a.Now
		}
		if now == "" {
			return wrap(fmt.Errorf("用例 %q: 要写 现在(转移写进入时间;超时按现在判定)", cs.Name))
		}
		tt, err := c.parseTime(now)
		if err != nil {
			return wrap(fmt.Errorf("用例 %q 的 现在: %v", cs.Name, err))
		}
		cs.Now = utc(tt)
		s.Acceptance.Cases = append(s.Acceptance.Cases, cs)
	}
	return nil
}

func loadTables(m *spec.Machine) []string {
	var out []string
	for _, l := range m.Loads {
		out = append(out, l.Table)
	}
	return out
}

var _ = kinds.Names
