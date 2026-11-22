package accept

// 状态机(R5,docs/design/r5-state-machines.md §5)的验收。参考模型 = 状态图的解释器(command.State.Deliver / Timeouts),
// 被测 = 生成的转移处理器(进程内投递 /factory.test/deliver)+ 超时扫描(/factory.test/timeouts,测试时钟 = 请求里的 now)。
// 每一步比较: 结果(转移 / 忽略 / 拒绝)+ 实体表和载入的表 + outbox 新增的消息 + 去重表。
//   - 人写的用例: 准备数据 → 一条消息 / 一轮超时 → 结果 + 发出 + 之后(+ 同一条消息再投 = duplicate,什么都不变)
//   - 转移全覆盖: 每个(状态 × 收到的消息类型)的格子 × 4 轮(每轮一份新的随机状态);每个守卫的真 / 假;实体不存在;
//     每次投递之后同一条消息再投一次(重投)
//   - 超时: 每个等待状态: 第 1 / 2 / 3 次重发的"刚好未到 / 刚好到"、期限的前后;一轮的上限(积压分多轮)
//   - 随机游走: 投递期待的回答 / 重投(同一个 id)/ 重发引起的回答(新 id)/ 迟到的旧回答 / 推进时钟跑一轮超时
//   - 故障注入: 转移 / 超时的第 k 个写之后失败 → 状态、outbox、去重表都不变;之后同一条消息 / 下一轮超时照常处理
//   - 并发: 同一条消息同时投 8 次;同一个回答的两份(不同 id)同时投;回答和超时同时;两个超时扫描同时 → 结果 = 某个串行顺序

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"math/big"
	"math/rand"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/MIERAI/api-factory-runtime/message"
	"github.com/MIERAI/api-factory-runtime/topology"

	"apifactory/factory/command"
	"apifactory/factory/constraints"
	"apifactory/factory/ir"
	"apifactory/factory/spec"
)

const (
	machRounds     = 4  // 转移全覆盖 / 超时 / 故障 / 并发: 每个格子几次
	machWalkSteps  = 60 // 随机游走的步数
	machWalkStarts = 6  // 随机游走: 初始状态里的实体数
	machBatch      = 100
)

type machEnv struct {
	ctx    context.Context
	d      *dbs
	conn   *pgx.Conn
	plan   *ir.Plan
	api    *ir.API
	m      *ir.Machine
	msg    *msgEnv
	rnd    *rand.Rand
	tables []string                  // 实体 + 载入的表(被引用的在前)
	cat    map[string]*command.Table //
	skip   map[string]bool           // 表.列: 数据库生成(identity / generated),不插入
	pools  map[string][]any          // 表.列 → 合法的值(数据库的写法)
	fields map[string]*ir.Field      // 表.列 → 发出的契约的字段(值域)
	state  *command.State
	proc   map[string]bool // 模型的去重表: 队列 + "/" + id
	sent   []machSent      // 投递过的消息(重投 / 迟到用)
	t0     time.Time
	key    int64 // 新的实体的键(不是数据库生成的时候)
}

type machSent struct {
	typ, id string
	data    map[string]any
}

func runMachine(ctx context.Context, d *dbs, url string, plan *ir.Plan, api *ir.API, s *spec.Spec) ([]Result, error) {
	m := api.Command.Machine
	h := fnv.New64a()
	h.Write([]byte(api.ID + "/machine" + os.Getenv("FACTORY_DIFF_SEED")))
	e := &machEnv{ctx: ctx, d: d, conn: d.main, plan: plan, api: api, m: m, msg: newMsgEnv(ctx, d, url, api),
		rnd: rand.New(rand.NewSource(int64(h.Sum64()))), cat: map[string]*command.Table{}, skip: map[string]bool{},
		pools: map[string][]any{}, fields: map[string]*ir.Field{}, t0: time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC), key: 1000}
	if _, err := e.conn.Exec(ctx, "SET TimeZone = 'UTC'"); err != nil {
		return nil, err
	}
	var results []Result
	tables, err := commandTables(ctx, d.main, api, s.Acceptance.Fixtures, s.Acceptance.Cases)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", s.ID(), err)
	}
	for i, c := range s.Acceptance.Cases {
		ok, detail := e.humanCase(tables, s.Acceptance.Fixtures, c, i)
		results = append(results, Result{Spec: s.ID(), Case: c.Name, Passed: ok, Detail: detail})
	}
	if err := e.setup(); err != nil {
		return append(results, Result{Spec: api.ID, Suite: "diff", Case: "R5 准备", Passed: false, Detail: err.Error()}), nil
	}
	for _, suite := range []func() []Result{e.coverage, e.timeouts, e.walk, e.faults, e.concurrency} {
		results = append(results, suite()...)
	}
	return append(results, runMachineMQ(ctx, d, url, plan, api, s)...), nil // 真实 RabbitMQ(FACTORY_TEST_AMQP)
}

// ---------------------------------------------------------------------------
// 投递 / 超时 / 状态
// ---------------------------------------------------------------------------

func (e *machEnv) queue(typ string) string { return e.msg.source + "." + typ }

func (e *machEnv) body(typ, id string, data map[string]any) []byte {
	ct := e.m.Contract(typ)
	b, _ := json.Marshal(map[string]any{
		"id": id, "type": typ, "version": ct.Version, "occurred_at": e.t0.Format(time.RFC3339),
		"source": "factory-accept", "subject": fmt.Sprint(data[ct.Subject]), "data": data,
	})
	return b
}

func (e *machEnv) deliver(typ, id string, data map[string]any, now time.Time, fault int) (delivered, error) {
	e.msg.queue = e.queue(typ)
	return e.msg.deliver(e.body(typ, id, data), now.UTC().Format(time.RFC3339Nano), fault)
}

type roundResult struct {
	Due       int      `json:"due"`
	Retried   int      `json:"retried"`
	Deadlines int      `json:"deadlines"`
	Skipped   int      `json:"skipped"`
	Conflicts int      `json:"conflicts"`
	Backlog   int      `json:"backlog"`
	Errors    []string `json:"errors"`
}

// round: 服务器跑一轮超时(测试的入口;now = 测试时钟)。
func (e *machEnv) round(now time.Time, fault int) (roundResult, error) {
	req := map[string]any{"machine": e.m.Name, "now": now.UTC().Format(time.RFC3339Nano)}
	if fault > 0 {
		req["headers"] = map[string]string{"x-factory-fail-after": strconv.Itoa(fault)}
	}
	b, _ := json.Marshal(req)
	resp, err := http.Post(e.msg.base+"/factory.test/timeouts", "application/json", bytes.NewReader(b))
	if err != nil {
		return roundResult{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return roundResult{}, fmt.Errorf("timeouts: HTTP %d %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out roundResult
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, fmt.Errorf("timeouts: %s", raw)
	}
	return out, nil
}

func (e *machEnv) readDB() (map[string][]command.Row, error) {
	out := map[string][]command.Row{}
	for _, t := range e.tables {
		rows, err := command.ReadRows(e.ctx, e.conn, e.cat[t])
		if err != nil {
			return nil, err
		}
		out[t] = rows
	}
	return out, nil
}

// compare: 模型 vs 数据库(表 + 去重表)。不一致 → 模型同步成数据库(一个 bug 不连带后面)。
func (e *machEnv) compare() []string {
	db, err := e.readDB()
	if err != nil {
		return []string{err.Error()}
	}
	var out []string
	for _, t := range e.tables {
		if msg := command.Diff(e.cat[t], e.state.Tables[t].Rows, db[t], false); msg != "" {
			out = append(out, t+": "+msg)
		}
	}
	got := map[string]bool{}
	rows, err := e.conn.Query(e.ctx, `SELECT handler || '/' || message_id::text FROM factory_processed_messages`)
	if err != nil {
		return append(out, err.Error())
	}
	for rows.Next() {
		var k string
		_ = rows.Scan(&k)
		got[k] = true
	}
	rows.Close()
	for k := range e.proc {
		if !got[k] {
			out = append(out, "去重表: 模型处理过了 "+k+",数据库没有")
		}
	}
	for k := range got {
		if !e.proc[k] {
			out = append(out, "去重表: 数据库有 "+k+",模型没有处理过")
		}
	}
	if len(out) > 0 {
		e.sync(db, got)
	}
	return out
}

func (e *machEnv) sync(db map[string][]command.Row, proc map[string]bool) {
	for _, t := range e.tables {
		e.state.Tables[t].Rows = db[t]
	}
	if proc != nil {
		e.proc = proc
	}
}

// predictMsg: 模型上处理一条消息(重投的 id = 处理过了 → duplicate)。返回 结果、是否重复。
func (e *machEnv) predictMsg(typ, id string, data map[string]any, now time.Time) (command.MachineResult, bool) {
	if e.proc[e.queue(typ)+"/"+id] {
		return command.MachineResult{}, true
	}
	nowC, _ := command.Canon("timestamptz", now)
	return e.state.Deliver(e.m, typ, command.Inputs(data), nowC), false
}

// step: 投递一条消息: 模型 → 服务器 → 比较。
func (e *machEnv) step(name, typ, id string, data map[string]any, now time.Time) (Result, command.MachineResult) {
	want, dup := e.predictMsg(typ, id, data, now)
	got, err := e.deliver(typ, id, data, now, 0)
	desc := fmt.Sprintf("%s %s → 模型 %s", typ, showData(data), machOutcome(want, dup))
	if err != nil {
		return Result{Spec: e.api.ID, Suite: "diff", Case: name, Passed: false, Detail: desc + " | " + err.Error()}, want
	}
	var problems []string
	problems = append(problems, outcomeVs(want, dup, got)...)
	if !dup && (want.Outcome == command.MachineTransition || want.Outcome == command.MachineIgnored) {
		e.proc[e.queue(typ)+"/"+id] = true
	}
	rows, err := e.msg.newRows()
	if err != nil {
		problems = append(problems, err.Error())
	}
	problems = append(problems, e.msg.checkRows(want.Emits, rows)...)
	problems = append(problems, e.compare()...)
	e.sent = append(e.sent, machSent{typ, id, data})
	if len(problems) > 0 {
		return Result{Spec: e.api.ID, Suite: "diff", Case: name, Passed: false, Detail: desc + " | 服务器 " + got.Outcome + " " + got.Business + " " + got.Error + " | " + strings.Join(problems, " | ")}, want
	}
	return Result{Spec: e.api.ID, Suite: "diff", Case: name, Passed: true, Detail: desc}, want
}

func machOutcome(r command.MachineResult, dup bool) string {
	switch {
	case dup:
		return "duplicate"
	case r.Outcome == command.MachineTransition:
		return r.From + " → " + r.To
	case r.Reason != "":
		return r.Outcome + "(" + r.Reason + ")"
	}
	return r.Outcome
}

// outcomeVs: 模型的结果 vs 服务器的回答(转移 = ok;忽略 = ok + IGNORED;拒绝 = poison;重复 = duplicate)。
func outcomeVs(want command.MachineResult, dup bool, got delivered) []string {
	ok := false
	switch {
	case dup:
		ok = got.Outcome == "duplicate"
	case want.Outcome == command.MachineTransition:
		ok = got.Outcome == "ok" && got.Business == ""
	case want.Outcome == command.MachineIgnored:
		ok = got.Outcome == "ok" && got.Business == "IGNORED"
	default:
		ok = got.Outcome == "poison"
	}
	if !ok {
		return []string{fmt.Sprintf("结果: 模型 %s,服务器 %s %s %s", machOutcome(want, dup), got.Outcome, got.Business, got.Error)}
	}
	return nil
}

// tick: 跑一轮超时: 模型 → 服务器 → 比较。
func (e *machEnv) tick(name string, now time.Time) (Result, []command.MachineResult) {
	want := e.state.Timeouts(e.m, now, machBatch)
	got, err := e.round(now, 0)
	var retried, deadlines int
	var emits []command.Msg
	for _, w := range want {
		switch w.Outcome {
		case command.MachineRetry:
			retried++
		case command.MachineDeadline:
			deadlines++
		}
		emits = append(emits, w.Emits...)
	}
	desc := fmt.Sprintf("超时 @%s → 模型 重发 %d / 期限 %d", now.Format("15:04:05.000000"), retried, deadlines)
	if err != nil {
		return Result{Spec: e.api.ID, Suite: "diff", Case: name, Passed: false, Detail: desc + " | " + err.Error()}, want
	}
	var problems []string
	if got.Retried != retried || got.Deadlines != deadlines || len(got.Errors) > 0 {
		problems = append(problems, fmt.Sprintf("服务器 重发 %d / 期限 %d / 错误 %v", got.Retried, got.Deadlines, got.Errors))
	}
	rows, err := e.msg.newRows()
	if err != nil {
		problems = append(problems, err.Error())
	}
	problems = append(problems, e.msg.checkRows(emits, rows)...)
	problems = append(problems, e.compare()...)
	if len(problems) > 0 {
		return Result{Spec: e.api.ID, Suite: "diff", Case: name, Passed: false, Detail: desc + " | " + strings.Join(problems, " | ")}, want
	}
	return Result{Spec: e.api.ID, Suite: "diff", Case: name, Passed: true, Detail: desc}, want
}

func showData(d map[string]any) string {
	b, _ := json.Marshal(d)
	s := string(b)
	if len(s) > 120 {
		s = s[:117] + "..."
	}
	return s
}

// ---------------------------------------------------------------------------
// 人写的用例
// ---------------------------------------------------------------------------

func (e *machEnv) humanCase(tables map[string]*command.Table, fixtures map[string][]map[string]any, c spec.Case, n int) (bool, string) {
	if err := loadFixtures(e.ctx, e.d, e.plan, fixtures); err != nil {
		return false, "fixtures: " + err.Error()
	}
	if err := e.msg.reset(); err != nil {
		return false, err.Error()
	}
	before := map[string][]command.Row{}
	for name, t := range tables {
		rows, err := command.ReadRows(e.ctx, e.d.main, t)
		if err != nil {
			return false, err.Error()
		}
		before[name] = rows
	}
	now, _ := time.Parse(time.RFC3339Nano, c.Now)
	var problems []string
	detail := ""
	id := message.NewID()
	want := c.Expect.Code
	if c.Message != nil {
		got, err := e.deliver(c.Message.Type, id, c.Message.Data, now, 0)
		if err != nil {
			return false, err.Error()
		}
		detail = fmt.Sprintf("结果 %s %s", got.Outcome, got.Business)
		ok := false
		switch {
		case strings.HasPrefix(want, "STATE:"):
			ok = got.Outcome == "ok" && got.Business == ""
		case want == "IGNORED":
			ok = got.Outcome == "ok" && got.Business == "IGNORED"
		case want == "REJECTED" || want == "POISON":
			ok = got.Outcome == "poison"
		}
		if !ok {
			problems = append(problems, fmt.Sprintf("结果: 期待 %s,服务器 %s %s %s", want, got.Outcome, got.Business, got.Error))
		}
		if st := strings.TrimPrefix(want, "STATE:"); st != want {
			ct := e.m.Contract(c.Message.Type)
			var cur int64
			_ = e.conn.QueryRow(e.ctx, fmt.Sprintf("SELECT %s FROM %s WHERE %s = $1", pgxIdent(e.m.State.Name), pgxIdent(e.m.Table), pgxIdent(e.m.Key.Name)),
				c.Message.Data[ct.Subject]).Scan(&cur)
			if s := e.m.StateByDB(cur); s == nil || s.Name != st {
				problems = append(problems, fmt.Sprintf("状态: 期待 %s,数据库 %d", st, cur))
			}
		}
	} else {
		got, err := e.round(now, 0)
		if err != nil {
			return false, err.Error()
		}
		detail = fmt.Sprintf("重发 %d / 期限 %d", got.Retried, got.Deadlines)
		ok := false
		switch want {
		case "RETRY":
			ok = got.Retried > 0 && got.Deadlines == 0
		case "DEADLINE":
			ok = got.Deadlines > 0
		case "NONE":
			ok = got.Retried == 0 && got.Deadlines == 0
		}
		if !ok || len(got.Errors) > 0 {
			problems = append(problems, fmt.Sprintf("结果: 期待 %s,服务器 重发 %d / 期限 %d %v", want, got.Retried, got.Deadlines, got.Errors))
		}
	}
	rows, err := e.msg.newRows()
	if err != nil {
		return false, err.Error()
	}
	if c.Emitted != nil {
		problems = append(problems, emittedDiff(*c.Emitted, rows)...)
	}
	problems = append(problems, afterProblems(e.plan, tables, before, c.After, id, e.msg)...)
	if c.Message != nil { // 同一条消息再投: 提交了的 → duplicate;拒绝的 → 同样拒绝;什么都不变
		committed := strings.HasPrefix(want, "STATE:") || want == "IGNORED"
		proc, err := e.msg.processed()
		if err != nil {
			return false, err.Error()
		}
		if proc[id] != committed {
			problems = append(problems, fmt.Sprintf("去重表: 期待 处理过了=%v,数据库 %v", committed, proc[id]))
		}
		again, err := e.deliver(c.Message.Type, id, c.Message.Data, now, 0)
		if err != nil {
			return false, err.Error()
		}
		if committed && again.Outcome != "duplicate" || !committed && again.Outcome != "poison" {
			problems = append(problems, "重投同一条消息: 服务器 "+again.Outcome)
		}
		if extra, _ := e.msg.newRows(); len(extra) > 0 {
			problems = append(problems, fmt.Sprintf("重投同一条消息: 又发出了 %d 条消息", len(extra)))
		}
	}
	if len(problems) > 0 {
		return false, detail + " | " + strings.Join(problems, "; ")
	}
	return true, detail + " | 发出 / 之后 / 去重 / 重投 ✓"
}

// ---------------------------------------------------------------------------
// 随机的状态
// ---------------------------------------------------------------------------

// setup: 表的目录、值域(发出的契约的字段 → 约束的值域)。
func (e *machEnv) setup() error {
	refs, err := foreignRefs(e.ctx, e.conn)
	if err != nil {
		return err
	}
	list := []string{e.m.Table}
	for _, l := range e.m.Loads {
		if !contains(list, l.Table) {
			list = append(list, l.Table)
		}
	}
	e.tables = sortedTables(refs, list)
	for _, t := range e.tables {
		ct, err := command.LoadTable(e.ctx, e.conn, t)
		if err != nil {
			return err
		}
		e.cat[t] = ct
		rows, err := e.conn.Query(e.ctx, `SELECT attname FROM pg_attribute WHERE attrelid = $1::regclass AND attnum > 0
			AND (attidentity <> '' OR attgenerated <> '')`, pgx.Identifier{t}.Sanitize())
		if err != nil {
			return err
		}
		for rows.Next() {
			var c string
			_ = rows.Scan(&c)
			e.skip[t+"."+c] = true
		}
		rows.Close()
	}
	// 发出的消息的字段 → 列(实体.x / 载入的列): 值域取契约的约束(格式 / 长度 / 范围)
	walkEmits := func(list []ir.CommandEmit) {
		for _, em := range list {
			ct := e.m.Contract(em.Type)
			for _, v := range em.Values {
				f := ct.Message.Field(v.Field)
				switch {
				case v.List && f != nil && f.Message != nil:
					for _, l := range e.m.Loads {
						if l.Name == v.Expr.Name {
							for i := range f.Message.Fields {
								e.fields[l.Table+"."+f.Message.Fields[i].Name] = &f.Message.Fields[i]
							}
						}
					}
				case v.Expr.Op == "ref" && f != nil:
					e.fields[e.m.Table+"."+v.Expr.Name] = f
				}
			}
		}
	}
	for _, st := range e.m.States {
		if st.Command != nil {
			walkEmits([]ir.CommandEmit{*st.Command})
		}
		walkEmits(st.DeadlineEmits)
	}
	for _, t := range e.m.Transitions {
		walkEmits(t.Emits)
	}
	for _, t := range e.tables {
		for _, c := range e.cat[t].Cols {
			e.pools[t+"."+c.Name] = e.poolOf(t, c)
		}
	}
	return nil
}

// poolOf: 一列的值域(数据库的写法): 枚举 → 数据库值;发出的契约的字段 → 约束的值域;其他 → 按类型的小值域。
func (e *machEnv) poolOf(t string, c command.Column) []any {
	if en := e.plan.Enum(t, c.Name); en != nil {
		var vals []any
		for _, v := range en.Values {
			vals = append(vals, v.DB)
		}
		return vals
	}
	var vals []any
	if f := e.fields[t+"."+c.Name]; f != nil {
		if d := constraints.DomainOf(f); d != nil {
			vals = append(vals, d.Values...)
		} else if k := patternKind(f); k != nil {
			vals = append(vals, k.Domain...)
		}
	}
	if len(vals) == 0 {
		vals = genericPool(c)
	}
	for i, v := range vals {
		vals[i] = dbForm(c.Type, v)
	}
	return vals
}

// reset: 清空实体和载入的表、outbox、去重表;模型从空开始。
func (e *machEnv) reset() error {
	var idents []string
	for _, t := range e.tables {
		idents = append(idents, pgx.Identifier{t}.Sanitize())
	}
	if _, err := e.conn.Exec(e.ctx, "TRUNCATE "+strings.Join(idents, ", ")+" CASCADE"); err != nil {
		return err
	}
	if err := e.msg.reset(); err != nil {
		return err
	}
	e.state = &command.State{Tables: map[string]*command.TableState{}}
	for _, t := range e.tables {
		e.state.Tables[t] = &command.TableState{T: e.cat[t]}
	}
	e.proc, e.sent = map[string]bool{}, nil
	return nil
}

// entity: 新建一个实体(状态 st,进入时间 entered,重发计数 attempts)+ 载入的子行。返回键(规范值)。
func (e *machEnv) entity(st string, entered time.Time, attempts int64) (any, error) {
	s := e.m.StateByName(st)
	t := e.cat[e.m.Table]
	for try := 0; try < 20; try++ {
		row := map[string]any{}
		for _, c := range t.Cols {
			if e.skip[t.Name+"."+c.Name] {
				continue
			}
			p := e.pools[t.Name+"."+c.Name]
			if len(p) > 0 && (c.NotNull || e.rnd.Intn(3) > 0) {
				row[c.Name] = p[e.rnd.Intn(len(p))]
			}
		}
		if !e.skip[t.Name+"."+e.m.Key.Name] {
			e.key += 1 + int64(e.rnd.Intn(3))
			row[e.m.Key.Name] = e.key
		}
		row[e.m.State.Name] = s.DB
		row[e.m.Entered] = entered
		row[e.m.Attempts] = attempts
		row[e.m.Version] = int64(1 + e.rnd.Intn(4))
		if e.m.Created != "" {
			row[e.m.Created] = entered.Add(-time.Duration(e.rnd.Intn(600)) * time.Second)
		}
		key, err := e.insert(t, row, e.m.Key.Name)
		if err != nil {
			continue
		}
		for _, l := range e.m.Loads {
			if err := e.children(l, key); err != nil {
				return nil, err
			}
		}
		return key, e.refresh()
	}
	return nil, fmt.Errorf("实体 %s: 造不出满足约束的行(值域: %v)", e.m.Table, e.pools)
}

// children: 载入的子行(1 ~ 3 行;主键的列取不同的值)。
func (e *machEnv) children(l ir.MachineLoad, key any) error {
	t := e.cat[l.Table]
	n := 1 + e.rnd.Intn(min(3, l.Max))
	used := map[string]bool{}
	made := 0
	for try := 0; made < n && try < 30; try++ {
		row := map[string]any{l.Child: rawOf(key)}
		for _, c := range t.Cols {
			if c.Name == l.Child || e.skip[t.Name+"."+c.Name] {
				continue
			}
			p := e.pools[t.Name+"."+c.Name]
			if len(p) > 0 && (c.NotNull || e.rnd.Intn(3) > 0) {
				row[c.Name] = p[e.rnd.Intn(len(p))]
			}
		}
		k := fmt.Sprint(row)
		for _, pk := range t.PK {
			k += "|" + fmt.Sprint(row[pk])
		}
		if used[k] {
			continue
		}
		if _, err := e.insert(t, row, ""); err == nil {
			used[k] = true
			made++
		}
	}
	return nil
}

func (e *machEnv) insert(t *command.Table, row map[string]any, ret string) (any, error) {
	cols := make([]string, 0, len(row))
	for c := range row {
		cols = append(cols, c)
	}
	sort.Strings(cols)
	names, holders, args := make([]string, len(cols)), make([]string, len(cols)), make([]any, len(cols))
	for i, c := range cols {
		names[i], holders[i], args[i] = pgx.Identifier{c}.Sanitize(), "$"+strconv.Itoa(i+1), row[c]
	}
	q := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)", pgx.Identifier{t.Name}.Sanitize(), strings.Join(names, ","), strings.Join(holders, ","))
	if ret == "" {
		_, err := e.conn.Exec(e.ctx, q, args...)
		return nil, err
	}
	var k any
	err := e.conn.QueryRow(e.ctx, q+" RETURNING "+pgx.Identifier{ret}.Sanitize()+"::text", args...).Scan(&k)
	if err != nil {
		return nil, err
	}
	return command.Canon(e.m.Key.Type, k)
}

// refresh: 模型 = 数据库现在的行(新建实体之后)。
func (e *machEnv) refresh() error {
	db, err := e.readDB()
	if err != nil {
		return err
	}
	e.sync(db, nil)
	return nil
}

// row: 模型里键 = k 的实体。
func (e *machEnv) row(k any) command.Row {
	for _, r := range e.state.Tables[e.m.Table].Rows {
		if command.Equal(r[e.m.Key.Name], k) {
			return r
		}
	}
	return nil
}

func (e *machEnv) stateOf(k any) string {
	r := e.row(k)
	if r == nil {
		return ""
	}
	n, _ := strconv.ParseInt(fmt.Sprint(r[e.m.State.Name]), 10, 64)
	if s := e.m.StateByDB(n); s != nil {
		return s.Name
	}
	return ""
}

// data: 收到 typ 的一条消息的 data(请求的写法),主体 = key。guard: 守卫里和 实体.x 比较的字段取那个值(true)或者别的值(false)。
func (e *machEnv) data(typ string, key any, guard bool) map[string]any {
	ct := e.m.Contract(typ)
	out := map[string]any{}
	for i := range ct.Message.Fields {
		f := &ct.Message.Fields[i]
		if f.Name == ct.Subject {
			out[f.Name] = rawOf(key)
			continue
		}
		if f.List && f.Message != nil {
			n := 1 + e.rnd.Intn(2)
			var list []any
			for j := 0; j < n; j++ {
				el := map[string]any{}
				for k := range f.Message.Fields {
					sf := &f.Message.Fields[k]
					vals := fieldVals(sf)
					el[sf.Name] = vals[(j+e.rnd.Intn(2)*n)%len(vals)]
				}
				list = append(list, el)
			}
			out[f.Name] = list
			continue
		}
		vals := fieldVals(f)
		out[f.Name] = vals[e.rnd.Intn(len(vals))]
	}
	var tr *ir.MachineTransition
	for i := range e.m.Transitions {
		if e.m.Transitions[i].On == typ {
			tr = &e.m.Transitions[i]
		}
	}
	if tr != nil && tr.Guard != nil {
		r := e.row(key)
		walkExpr(tr.Guard, func(x *ir.CmdExpr) {
			if x.Op != "=" || len(x.Args) != 2 {
				return
			}
			in, ref := x.Args[0], x.Args[1]
			if in.Op != "input" {
				in, ref = ref, in
			}
			if in.Op != "input" || ref.Op != "ref" || r == nil {
				return
			}
			f := ct.Message.Field(in.Name)
			v := r[ref.Name]
			if guard {
				out[in.Name] = jsonForm(f, v)
				return
			}
			for _, alt := range fieldVals(f) {
				if c, err := command.Canon(ref.Type, alt); err == nil && !command.Equal(c, v) {
					out[in.Name] = alt
					return
				}
			}
		})
	}
	return out
}

// fieldVals: 契约字段的合法值(请求的写法)。
func fieldVals(f *ir.Field) []any {
	var vals []any
	if d := constraints.DomainOf(f); d != nil {
		vals = append(vals, d.Values...)
	} else if t := patternKind(f); t != nil && len(t.Domain) > 0 {
		vals = append(vals, t.Domain...)
	}
	if len(vals) == 0 {
		switch f.Kind {
		case "int32", "int64":
			vals = []any{int64(1), int64(2), int64(3)}
		case "bool":
			vals = []any{true, false}
		default:
			vals = []any{"v-a", "v-b"}
		}
	}
	for i, v := range vals {
		vals[i] = jsonForm(f, dbForm("", v))
	}
	return vals
}

// jsonForm: 规范值 / 数据库的写法 → data 里的写法(整数 → 数字;其他 → 文本)。
func jsonForm(f *ir.Field, v any) any {
	if f != nil && (f.Kind == "int32" || f.Kind == "int64") {
		n, err := strconv.ParseInt(fmt.Sprint(v), 10, 64)
		if err == nil {
			return n
		}
	}
	if f != nil && f.Kind == "bool" {
		return fmt.Sprint(v) == "true"
	}
	if s, ok := v.(string); ok {
		if r, ok := new(big.Rat).SetString(s); ok && strings.Contains(s, "/") { // 十进制的规范值(1300.5 = "2601/2")→ 十进制的文本
			return ratText(r)
		}
		return s
	}
	return fmt.Sprint(v)
}

// missingKey: 不存在的实体的键。
func (e *machEnv) missingKey() any {
	k, _ := command.Canon(e.m.Key.Type, e.key+1000+int64(e.rnd.Intn(100)))
	return k
}

func (e *machEnv) waiting() []ir.MachineState {
	var out []ir.MachineState
	for _, s := range e.m.States {
		if !s.Final {
			out = append(out, s)
		}
	}
	return out
}

func (e *machEnv) transitionOf(typ string) *ir.MachineTransition {
	for i := range e.m.Transitions {
		if e.m.Transitions[i].On == typ {
			return &e.m.Transitions[i]
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// 转移全覆盖
// ---------------------------------------------------------------------------

func (e *machEnv) coverage() []Result {
	var results []Result
	cells := map[string]int{}
	guards := map[string]int{}
	for round := 0; round < machRounds; round++ {
		if err := e.reset(); err != nil {
			return append(results, Result{Spec: e.api.ID, Suite: "diff", Case: "R5 覆盖 准备", Passed: false, Detail: err.Error()})
		}
		now := e.t0.Add(time.Duration(round) * time.Hour)
		type target struct {
			st, typ string
			key     any
			guard   bool
		}
		var targets []target
		for _, st := range e.m.States {
			for _, r := range e.m.Receives {
				for _, g := range []bool{true, false} {
					tr := e.transitionOf(r.Type)
					if !g && (tr.Guard == nil || tr.From != st.Name) {
						continue
					}
					k, err := e.entity(st.Name, now.Add(-time.Duration(e.rnd.Intn(20))*time.Second), int64(e.rnd.Intn(3)))
					if err != nil {
						return append(results, Result{Spec: e.api.ID, Suite: "diff", Case: "R5 覆盖 准备", Passed: false, Detail: err.Error()})
					}
					targets = append(targets, target{st.Name, r.Type, k, g})
				}
			}
		}
		e.rnd.Shuffle(len(targets), func(i, j int) { targets[i], targets[j] = targets[j], targets[i] })
		for _, tg := range targets {
			data := e.data(tg.typ, tg.key, tg.guard)
			id := message.NewID()
			name := fmt.Sprintf("R5 覆盖: %s × %s", tg.st, tg.typ)
			if tr := e.transitionOf(tg.typ); tr.Guard != nil && tr.From == tg.st {
				name += fmt.Sprintf("(守卫 %v)", tg.guard)
			}
			res, want := e.step(name, tg.typ, id, data, now)
			results = append(results, res)
			cells[tg.st+" × "+tg.typ]++
			if tr := e.transitionOf(tg.typ); tr.Guard != nil && tr.From == tg.st {
				guards[fmt.Sprintf("%s %v", tg.typ, want.Outcome == command.MachineTransition)]++
			}
			// 重投同一条消息(同一个 id): 处理过了 → duplicate;拒绝的 → 同样拒绝;什么都不变
			res2, _ := e.step(name+" 重投", tg.typ, id, data, now)
			results = append(results, res2)
		}
		for _, r := range e.m.Receives { // 实体不存在
			res, _ := e.step("R5 覆盖: 实体不存在 × "+r.Type, r.Type, message.NewID(), e.data(r.Type, e.missingKey(), true), now)
			results = append(results, res)
		}
	}
	var weak []string
	for _, st := range e.m.States {
		for _, r := range e.m.Receives {
			if cells[st.Name+" × "+r.Type] < machRounds {
				weak = append(weak, st.Name+" × "+r.Type)
			}
		}
	}
	for _, t := range e.m.Transitions {
		if t.Guard == nil {
			continue
		}
		for _, g := range []bool{true, false} {
			if n := guards[fmt.Sprintf("%s %v", t.On, g)]; n < machRounds {
				weak = append(weak, fmt.Sprintf("守卫 %s = %v: %d 次", t.On, g, n))
			}
		}
	}
	text := fmt.Sprintf("%d 个格子(状态 × 消息)各 %d 次;守卫的真 / 假各 %d 次", len(e.m.States)*len(e.m.Receives), machRounds, machRounds)
	if len(weak) > 0 {
		text += " | 不足: " + strings.Join(weak, ", ")
	}
	return append(results, Result{Spec: e.api.ID, Suite: "diff", Case: "R5 覆盖条件", Passed: len(weak) == 0, Detail: text})
}

// ---------------------------------------------------------------------------
// 超时
// ---------------------------------------------------------------------------

func (e *machEnv) timeouts() []Result {
	var results []Result
	fail := func(err error) []Result {
		return append(results, Result{Spec: e.api.ID, Suite: "diff", Case: "R5 超时 准备", Passed: false, Detail: err.Error()})
	}
	us := time.Microsecond
	for _, st := range e.waiting() {
		for round := 0; round < machRounds; round++ {
			// 第 n 次重发(n = 1, 2, 3)的时刻: 进入 + 超时 + 间隔 × (n − 1);刚好未到(− 1µs)→ 不重发,刚好到 → 重发
			for n := int64(1); n <= 3; n++ {
				due := time.Duration(st.Timeout+st.Every*(n-1)) * time.Second
				if st.Deadline > 0 && due >= time.Duration(st.Deadline)*time.Second {
					continue
				}
				if err := e.reset(); err != nil {
					return fail(err)
				}
				entered := e.t0.Add(time.Duration(round*7) * time.Minute)
				if _, err := e.entity(st.Name, entered, n-1); err != nil {
					return fail(err)
				}
				r1, _ := e.tick(fmt.Sprintf("R5 超时: %s 第 %d 次重发 刚好未到", st.Name, n), entered.Add(due-us))
				r2, _ := e.tick(fmt.Sprintf("R5 超时: %s 第 %d 次重发 刚好到", st.Name, n), entered.Add(due))
				r3, _ := e.tick(fmt.Sprintf("R5 超时: %s 第 %d 次重发之后同一时刻再扫一轮", st.Name, n), entered.Add(due))
				results = append(results, r1, r2, r3)
			}
			if st.Deadline == 0 {
				continue
			}
			// 期限: 刚好未到(重发)/ 刚好到(期限后)
			if err := e.reset(); err != nil {
				return fail(err)
			}
			entered := e.t0.Add(time.Duration(round*11) * time.Minute)
			n := (st.Deadline - st.Timeout) / st.Every // 期限之前最后一次重发之后的重发计数
			if _, err := e.entity(st.Name, entered, n+1); err != nil {
				return fail(err)
			}
			dl := time.Duration(st.Deadline) * time.Second
			r1, _ := e.tick(fmt.Sprintf("R5 超时: %s 期限 刚好未到", st.Name), entered.Add(dl-us))
			r2, _ := e.tick(fmt.Sprintf("R5 超时: %s 期限 刚好到", st.Name), entered.Add(dl))
			results = append(results, r1, r2)
		}
	}
	// 一轮的上限: 到期的实体多于上限 → 分多轮(按进入时间)
	if err := e.reset(); err != nil {
		return fail(err)
	}
	st := e.waiting()[0]
	for i := 0; i < machBatch+5; i++ {
		if _, err := e.entity(st.Name, e.t0.Add(time.Duration(i)*time.Second), 0); err != nil {
			return fail(err)
		}
	}
	at := e.t0.Add(time.Duration(machBatch+5)*time.Second + time.Duration(st.Timeout)*time.Second)
	r1, _ := e.tick("R5 超时: 积压(第 1 轮 = 上限)", at)
	r2, _ := e.tick("R5 超时: 积压(第 2 轮 = 剩下的)", at)
	return append(results, r1, r2)
}

// ---------------------------------------------------------------------------
// 随机游走
// ---------------------------------------------------------------------------

func (e *machEnv) walk() []Result {
	var results []Result
	if err := e.reset(); err != nil {
		return []Result{{Spec: e.api.ID, Suite: "diff", Case: "R5 随机游走 准备", Passed: false, Detail: err.Error()}}
	}
	now := e.t0
	initial := e.initial()
	var keys []any
	for i := 0; i < machWalkStarts; i++ {
		k, err := e.entity(initial, now, 0)
		if err != nil {
			return []Result{{Spec: e.api.ID, Suite: "diff", Case: "R5 随机游走 准备", Passed: false, Detail: err.Error()}}
		}
		keys = append(keys, k)
	}
	counts := map[string]int{}
	for i := 0; i < machWalkSteps; i++ {
		now = now.Add(time.Duration(1+e.rnd.Intn(3)) * time.Second)
		var live []any // 还没到终态的实体(多数步骤在它们上面走)
		for _, k := range keys {
			if st := e.m.StateByName(e.stateOf(k)); st != nil && !st.Final {
				live = append(live, k)
			}
		}
		if len(live) == 0 { // 全部到了终态: 再开始一个(开始 = 新建在初始状态)
			k, err := e.entity(initial, now, 0)
			if err != nil {
				return append(results, Result{Spec: e.api.ID, Suite: "diff", Case: "R5 随机游走", Passed: false, Detail: err.Error()})
			}
			keys = append(keys, k)
			live = append(live, k)
		}
		k := live[e.rnd.Intn(len(live))]
		if e.rnd.Intn(5) == 0 {
			k = keys[e.rnd.Intn(len(keys))]
		}
		cur := e.stateOf(k)
		var answers []string // 当前状态能转移的消息
		for _, t := range e.m.Transitions {
			if t.From == cur {
				answers = append(answers, t.On)
			}
		}
		choice := e.rnd.Intn(100)
		switch {
		case choice < 50 && len(answers) > 0: // 期待的回答
			typ := answers[e.rnd.Intn(len(answers))]
			res, _ := e.step(fmt.Sprintf("R5 随机游走 #%d: 回答 %s", i+1, typ), typ, message.NewID(), e.data(typ, k, e.rnd.Intn(10) > 0), now)
			results = append(results, res)
			counts["回答"]++
		case choice < 62 && len(e.sent) > 0: // 重投(同一个 id)
			s := e.sent[e.rnd.Intn(len(e.sent))]
			res, _ := e.step(fmt.Sprintf("R5 随机游走 #%d: 重投 %s", i+1, s.typ), s.typ, s.id, s.data, now)
			results = append(results, res)
			counts["重投"]++
		case choice < 76 && len(e.sent) > 0: // 重发引起的回答 / 迟到的旧回答(同样的内容,新 id)
			s := e.sent[e.rnd.Intn(len(e.sent))]
			res, _ := e.step(fmt.Sprintf("R5 随机游走 #%d: 同样内容的新消息 %s", i+1, s.typ), s.typ, message.NewID(), s.data, now)
			results = append(results, res)
			counts["迟到 / 重发的回答"]++
		default: // 推进时钟,跑一轮超时
			var step time.Duration
			if st := e.m.StateByName(cur); st != nil && !st.Final {
				step = time.Duration(st.Timeout+st.Every*int64(e.rnd.Intn(3))) * time.Second
			} else {
				step = time.Duration(30+e.rnd.Intn(600)) * time.Second
			}
			if e.rnd.Intn(5) == 0 {
				step = 20 * time.Minute // 越过期限
			}
			now = now.Add(step)
			res, _ := e.tick(fmt.Sprintf("R5 随机游走 #%d: 推进 %s 跑一轮超时", i+1, step), now)
			results = append(results, res)
			counts["超时"]++
		}
	}
	var parts []string
	for _, k := range []string{"回答", "重投", "迟到 / 重发的回答", "超时"} {
		parts = append(parts, fmt.Sprintf("%s %d", k, counts[k]))
	}
	return append(results, Result{Spec: e.api.ID, Suite: "diff", Case: "R5 随机游走(步骤)", Passed: true, Detail: strings.Join(parts, " / ")})
}

// initial: 初始状态(开始的组合命令新建实体时的状态 = 第一个转移的 从 之外没有入边的等待状态;没有 → 第一个等待状态)。
func (e *machEnv) initial() string {
	in := map[string]bool{}
	for _, t := range e.m.Transitions {
		in[t.To] = true
	}
	for _, s := range e.m.States {
		if s.DeadlineTo != "" {
			in[s.DeadlineTo] = true
		}
	}
	for _, s := range e.waiting() {
		if !in[s.Name] {
			return s.Name
		}
	}
	return e.waiting()[0].Name
}

// ---------------------------------------------------------------------------
// 故障注入
// ---------------------------------------------------------------------------

func (e *machEnv) faults() []Result {
	var results []Result
	fail := func(err error) []Result {
		return append(results, Result{Spec: e.api.ID, Suite: "diff", Case: "R5 故障 准备", Passed: false, Detail: err.Error()})
	}
	check := func(name string, extra []string) {
		problems := append([]string{}, extra...)
		if rows, err := e.msg.newRows(); err != nil || len(rows) > 0 {
			problems = append(problems, fmt.Sprintf("回滚之后 outbox 里有 %d 条消息 %v", len(rows), err))
		}
		problems = append(problems, e.compare()...)
		results = append(results, Result{Spec: e.api.ID, Suite: "diff", Case: name, Passed: len(problems) == 0, Detail: strings.Join(append([]string{"回滚 ✓"}, problems...), " | ")})
	}
	for _, t := range e.m.Transitions {
		for round := 0; round < machRounds; round++ {
			if err := e.reset(); err != nil {
				return fail(err)
			}
			now := e.t0.Add(time.Duration(round) * time.Minute)
			k, err := e.entity(t.From, now.Add(-time.Second), 0)
			if err != nil {
				return fail(err)
			}
			data := e.data(t.On, k, true)
			want, _ := e.predictMsg(t.On, "x", data, now)
			e.refresh() // predictMsg 改了模型: 恢复
			for f := 1; f <= max(want.Writes, 1); f++ {
				id := message.NewID()
				got, err := e.deliver(t.On, id, data, now, f)
				var extra []string
				if err != nil {
					extra = append(extra, err.Error())
				} else if got.Outcome != "retry" {
					extra = append(extra, fmt.Sprintf("第 %d 个写之后失败: 期待 retry,服务器 %s %s", f, got.Outcome, got.Error))
				}
				check(fmt.Sprintf("R5 故障: %s 第 %d 个写之后失败(%d 轮)", t.On, f, round+1), extra)
				if f == want.Writes { // 同一条消息再投(没有故障)→ 照常转移一次
					res, _ := e.step(fmt.Sprintf("R5 故障: %s 失败之后再投", t.On), t.On, id, data, now)
					results = append(results, res)
				}
			}
		}
	}
	for _, st := range e.waiting() {
		for round := 0; round < machRounds; round++ {
			if err := e.reset(); err != nil {
				return fail(err)
			}
			entered := e.t0.Add(time.Duration(round) * time.Minute)
			if _, err := e.entity(st.Name, entered, 0); err != nil {
				return fail(err)
			}
			at := entered.Add(time.Duration(st.Timeout) * time.Second)
			if st.Deadline > 0 && round%2 == 1 {
				at = entered.Add(time.Duration(st.Deadline) * time.Second)
			}
			probe := e.state.Clone()
			want := probe.Timeouts(e.m, at, machBatch)
			writes := 1
			if len(want) > 0 {
				writes = want[0].Writes
			}
			for f := 1; f <= writes; f++ {
				got, err := e.round(at, f)
				var extra []string
				if err != nil {
					extra = append(extra, err.Error())
				} else if len(got.Errors) != 1 || got.Retried+got.Deadlines != 0 {
					extra = append(extra, fmt.Sprintf("第 %d 个写之后失败: 期待一个错误,服务器 %+v", f, got))
				}
				check(fmt.Sprintf("R5 故障: %s 的超时 第 %d 个写之后失败(%d 轮)", st.Name, f, round+1), extra)
			}
			res, _ := e.tick(fmt.Sprintf("R5 故障: %s 的超时 失败之后下一轮", st.Name), at)
			results = append(results, res)
		}
	}
	return results
}

// ---------------------------------------------------------------------------
// 并发
// ---------------------------------------------------------------------------

func (e *machEnv) concurrency() []Result {
	var results []Result
	fail := func(err error) []Result {
		return append(results, Result{Spec: e.api.ID, Suite: "diff", Case: "R5 并发 准备", Passed: false, Detail: err.Error()})
	}
	finish := func(name string, wantEmits []command.Msg, extra []string) {
		problems := append([]string{}, extra...)
		rows, err := e.msg.newRows()
		if err != nil {
			problems = append(problems, err.Error())
		}
		problems = append(problems, e.msg.checkRows(wantEmits, rows)...)
		problems = append(problems, e.compare()...)
		results = append(results, Result{Spec: e.api.ID, Suite: "diff", Case: name, Passed: len(problems) == 0, Detail: strings.Join(append([]string{"串行等价 ✓"}, problems...), " | ")})
	}
	for _, t := range e.m.Transitions {
		for round := 0; round < machRounds; round++ {
			if err := e.reset(); err != nil {
				return fail(err)
			}
			now := e.t0.Add(time.Duration(round) * time.Minute)
			k, err := e.entity(t.From, now.Add(-time.Second), 0)
			if err != nil {
				return fail(err)
			}
			data := e.data(t.On, k, true)
			// (a) 同一条消息(同一个 id)同时投 8 次 → 处理一次,其余 duplicate
			id := message.NewID()
			got := e.burst(t.On, []string{id}, data, now, concurrentMsgs)
			want, _ := e.predictMsg(t.On, id, data, now)
			e.proc[e.queue(t.On)+"/"+id] = true
			var extra []string
			if n := countOutcome(got, "ok"); n != 1 || countOutcome(got, "duplicate") != concurrentMsgs-1 {
				extra = append(extra, fmt.Sprintf("同一条消息同时 %d 次: ok %d / duplicate %d", concurrentMsgs, n, countOutcome(got, "duplicate")))
			}
			finish(fmt.Sprintf("R5 并发: %s 同一条消息同时 %d 次", t.On, concurrentMsgs), want.Emits, extra)
			// (b) 同一个回答的两份(不同 id)同时 → 一次转移 + 其余忽略(迟到)
			if err := e.reset(); err != nil {
				return fail(err)
			}
			k, err = e.entity(t.From, now.Add(-time.Second), 0)
			if err != nil {
				return fail(err)
			}
			data = e.data(t.On, k, true)
			ids := []string{message.NewID(), message.NewID(), message.NewID()}
			got = e.burst(t.On, ids, data, now, 1)
			want, _ = e.predictMsg(t.On, ids[0], data, now)
			extra = nil
			okN, ignored := 0, 0
			for _, g := range got {
				switch {
				case g.Outcome == "ok" && g.Business == "":
					okN++
				case g.Outcome == "ok" && g.Business == "IGNORED":
					ignored++
				}
			}
			for _, id := range ids {
				e.proc[e.queue(t.On)+"/"+id] = true
			}
			if okN != 1 || ignored != len(ids)-1 {
				extra = append(extra, fmt.Sprintf("同一个回答 %d 份同时: 转移 %d / 忽略 %d(期待 1 / %d)", len(ids), okN, ignored, len(ids)-1))
			}
			finish(fmt.Sprintf("R5 并发: %s 的 %d 份(不同 id)同时", t.On, len(ids)), want.Emits, extra)
		}
	}
	// (c) 回答和超时扫描同时: 结果 = "先重发再转移" 或 "先转移、超时不再重发" 之一
	for _, t := range e.m.Transitions {
		from := e.m.StateByName(t.From)
		for round := 0; round < machRounds; round++ {
			if err := e.reset(); err != nil {
				return fail(err)
			}
			entered := e.t0.Add(time.Duration(round) * time.Minute)
			k, err := e.entity(t.From, entered, 0)
			if err != nil {
				return fail(err)
			}
			at := entered.Add(time.Duration(from.Timeout) * time.Second)
			data := e.data(t.On, k, true)
			id := message.NewID()
			var wg sync.WaitGroup
			var got delivered
			var rr roundResult
			var err1, err2 error
			wg.Add(2)
			go func() { defer wg.Done(); got, err1 = e.deliver(t.On, id, data, at, 0) }()
			go func() { defer wg.Done(); rr, err2 = e.round(at, 0) }()
			wg.Wait()
			// 两个串行顺序的模型
			base := e.state.Clone()
			orderA := base.Clone() // 先超时,再回答
			retA := orderA.Timeouts(e.m, at, machBatch)
			nowC, _ := command.Canon("timestamptz", at)
			ansA := orderA.Deliver(e.m, t.On, command.Inputs(data), nowC)
			orderB := base.Clone() // 先回答,再超时
			ansB := orderB.Deliver(e.m, t.On, command.Inputs(data), nowC)
			retB := orderB.Timeouts(e.m, at, machBatch)
			var extra []string
			if err1 != nil || err2 != nil {
				extra = append(extra, fmt.Sprint(err1, err2))
			}
			db, _ := e.readDB()
			rows, _ := e.msg.newRows()
			matches := func(st *command.State, emits []command.Msg) bool {
				for _, tn := range e.tables {
					if command.Diff(e.cat[tn], st.Tables[tn].Rows, db[tn], false) != "" {
						return false
					}
				}
				return len(e.msg.checkRowsSet(emits, rows)) == 0
			}
			emitsA := append(append([]command.Msg{}, msgsOf(retA)...), ansA.Emits...)
			emitsB := append(append([]command.Msg{}, ansB.Emits...), msgsOf(retB)...)
			switch {
			case matches(orderA, emitsA):
				e.state = orderA
			case matches(orderB, emitsB):
				e.state = orderB
			default:
				extra = append(extra, fmt.Sprintf("和两个串行顺序都不同(服务器: 回答 %s %s,超时 重发 %d / 期限 %d %v)", got.Outcome, got.Business, rr.Retried, rr.Deadlines, rr.Errors))
				e.sync(db, nil)
			}
			e.proc[e.queue(t.On)+"/"+id] = true
			problems := append(extra, e.compare()...)
			results = append(results, Result{Spec: e.api.ID, Suite: "diff", Case: fmt.Sprintf("R5 并发: %s 和超时扫描同时", t.On), Passed: len(problems) == 0,
				Detail: strings.Join(append([]string{"串行等价 ✓"}, problems...), " | ")})
		}
	}
	// (d) 两个超时扫描同时 → 每个到期的实体只重发一次
	for _, st := range e.waiting() {
		for round := 0; round < machRounds; round++ {
			if err := e.reset(); err != nil {
				return fail(err)
			}
			entered := e.t0.Add(time.Duration(round) * time.Minute)
			a0 := int64(e.rnd.Intn(2))
			for i := 0; i < 3; i++ {
				if _, err := e.entity(st.Name, entered.Add(time.Duration(i)*time.Second), a0); err != nil {
					return fail(err)
				}
			}
			at := entered.Add(time.Duration(st.Timeout+st.Every*a0)*time.Second + 2*time.Second) // 三个都到期,重发一次之后都不到期
			var wg sync.WaitGroup
			rs := make([]roundResult, 2)
			for i := range rs {
				wg.Add(1)
				go func() { defer wg.Done(); rs[i], _ = e.round(at, 0) }()
			}
			wg.Wait()
			want := e.state.Timeouts(e.m, at, machBatch)                 // 串行: 第一轮
			want = append(want, e.state.Timeouts(e.m, at, machBatch)...) // 第二轮(同一时刻)
			var extra []string
			if n := rs[0].Retried + rs[0].Deadlines + rs[1].Retried + rs[1].Deadlines; n != len(want) {
				extra = append(extra, fmt.Sprintf("两个扫描同时: 一共处理了 %d 个,期待 %d 个(%+v %+v)", n, len(want), rs[0], rs[1]))
			}
			rows, _ := e.msg.newRows()
			extra = append(extra, e.msg.checkRowsSet(msgsOf(want), rows)...)
			problems := append(extra, e.compare()...)
			results = append(results, Result{Spec: e.api.ID, Suite: "diff", Case: fmt.Sprintf("R5 并发: %s 两个超时扫描同时", st.Name), Passed: len(problems) == 0,
				Detail: strings.Join(append([]string{"每个实体一次 ✓"}, problems...), " | ")})
		}
	}
	return results
}

func msgsOf(rs []command.MachineResult) []command.Msg {
	var out []command.Msg
	for _, r := range rs {
		out = append(out, r.Emits...)
	}
	return out
}

func countOutcome(ds []delivered, o string) int {
	n := 0
	for _, d := range ds {
		if d.Outcome == o {
			n++
		}
	}
	return n
}

// burst: 每个 id 同时投 times 次。
func (e *machEnv) burst(typ string, ids []string, data map[string]any, now time.Time, times int) []delivered {
	var mu sync.Mutex
	var out []delivered
	var wg sync.WaitGroup
	for _, id := range ids {
		for i := 0; i < times; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				d, err := e.deliver(typ, id, data, now, 0)
				if err != nil {
					d = delivered{Outcome: "error", Error: err.Error()}
				}
				mu.Lock()
				out = append(out, d)
				mu.Unlock()
			}()
		}
	}
	wg.Wait()
	return out
}

// checkRowsSet: 同 checkRows,但不看顺序(并发时消息的先后不确定): 按 类型 + 主体 + data 配对。
func (e *msgEnv) checkRowsSet(want []command.Msg, got []outRow) []string {
	key := func(typ string, data map[string]any) string {
		ct := e.contract(typ)
		if ct == nil {
			return typ
		}
		var parts []string
		for _, f := range ct.Message.Fields {
			parts = append(parts, f.Name+"="+showAny(canonField(&f, data[f.Name])))
		}
		return typ + "{" + strings.Join(parts, ",") + "}"
	}
	var w, g []string
	for _, m := range want {
		w = append(w, key(m.Type, m.Data))
	}
	for _, r := range got {
		g = append(g, key(r.Type, r.Data))
	}
	sort.Strings(w)
	sort.Strings(g)
	if strings.Join(w, "\n") != strings.Join(g, "\n") {
		return []string{fmt.Sprintf("发出的消息(不看顺序): 模型 %v,outbox %v", w, g)}
	}
	return nil
}

// ---------------------------------------------------------------------------
// 真实 RabbitMQ(FACTORY_TEST_AMQP,make accept-mq)
// ---------------------------------------------------------------------------

// runMachineMQ: 状态机经过真实的 RabbitMQ: 人写的转移用例的消息发到交换机 → 消费循环 → 转移 → 提交 → 确认;
// 转移发出的命令 / 事件经 outbox 的中继发出(旁听队列收到事件);同一个 id 再发 → 只处理一次。语义在进程内的差分里验证。
func runMachineMQ(ctx context.Context, d *dbs, url string, plan *ir.Plan, api *ir.API, s *spec.Spec) []Result {
	amqpURL := os.Getenv(MQEnv)
	if amqpURL == "" {
		return nil
	}
	fail := func(f string, a ...any) []Result {
		return []Result{{Spec: api.ID, Suite: "diff", Case: "RabbitMQ: 准备", Passed: false, Detail: fmt.Sprintf(f, a...)}}
	}
	conn, err := amqp.Dial(amqpURL)
	if err != nil {
		return fail("%v", err)
	}
	defer conn.Close()
	ch, err := conn.Channel()
	if err != nil {
		return fail("%v", err)
	}
	if err := topology.DeclareExchanges(ch, topology.DefaultExchanges); err != nil {
		return fail("%v", err)
	}
	q, err := ch.QueueDeclare("", false, true, true, false, nil)
	if err == nil {
		err = ch.QueueBind(q.Name, "#", topology.DefaultExchanges.Events, false, nil)
	}
	if err == nil {
		err = bindCommands(ch, q.Name, api.Command.Contracts)
	}
	if err != nil {
		return fail("旁听队列: %v", err)
	}
	tap, err := ch.Consume(q.Name, "", true, true, false, false, nil)
	if err != nil {
		return fail("%v", err)
	}
	e := &mqEnv{msgEnv: newMsgEnv(ctx, d, url, api), url: amqpURL, conn: conn, ch: ch, tap: tap, seen: map[string][]byte{}}
	m := api.Command.Machine
	var out []Result
	n := 0
	for _, c := range s.Acceptance.Cases {
		st, ok := strings.CutPrefix(c.Expect.Code, "STATE:")
		if c.Message == nil || !ok || n >= 2 {
			continue
		}
		n++
		if err := loadFixtures(ctx, d, plan, s.Acceptance.Fixtures); err != nil {
			return append(out, fail("%v", err)...)
		}
		if err := e.reset(); err != nil {
			return append(out, fail("%v", err)...)
		}
		ct := m.Contract(c.Message.Type)
		e.queue = e.source + "." + c.Message.Type
		id := message.NewID()
		body, _ := json.Marshal(map[string]any{"id": id, "type": ct.Type, "version": ct.Version, "occurred_at": time.Now().UTC().Format(time.RFC3339),
			"source": "factory-accept", "subject": fmt.Sprint(c.Message.Data[ct.Subject]), "data": c.Message.Data})
		ex := topology.DefaultExchanges.Events
		if ct.Kind == "command" {
			ex = topology.DefaultExchanges.Commands
		}
		publish := func() []string {
			err := ch.PublishWithContext(ctx, ex, ct.Type, false, false, amqp.Publishing{ContentType: message.ContentType,
				DeliveryMode: amqp.Persistent, MessageId: id, Type: ct.Type, Body: body})
			if err != nil {
				return []string{"publish: " + err.Error()}
			}
			return nil
		}
		problems := publish()
		problems = append(problems, e.waitProcessed(id, 10*time.Second)...)
		problems = append(problems, e.waitRelayed(10*time.Second)...)
		var cur int64
		_ = d.main.QueryRow(ctx, fmt.Sprintf("SELECT %s FROM %s WHERE %s = $1", pgxIdent(m.State.Name), pgxIdent(m.Table), pgxIdent(m.Key.Name)),
			c.Message.Data[ct.Subject]).Scan(&cur)
		if x := m.StateByDB(cur); x == nil || x.Name != st {
			problems = append(problems, fmt.Sprintf("状态: 期待 %s,数据库 %d", st, cur))
		}
		n0, _ := e.outboxCount()
		out = append(out, Result{Spec: api.ID, Suite: "diff", Case: fmt.Sprintf("RabbitMQ: %s → 转移到 %s → 发出的消息经中继发出", c.Message.Type, st),
			Passed: len(problems) == 0, Detail: strings.Join(append([]string{"✓"}, problems...), " | ")})
		problems = publish() // 同一个 id 再发
		time.Sleep(1500 * time.Millisecond)
		if k, _ := e.outboxCount(); k != n0 {
			problems = append(problems, fmt.Sprintf("重投之后 outbox %d → %d 行(应该不变)", n0, k))
		}
		if p, _ := e.processed(); len(p) != 1 {
			problems = append(problems, fmt.Sprintf("去重表 %d 行(应该 1)", len(p)))
		}
		out = append(out, Result{Spec: api.ID, Suite: "diff", Case: fmt.Sprintf("RabbitMQ: %s 同一个 id 再发 → 只处理一次", c.Message.Type),
			Passed: len(problems) == 0, Detail: strings.Join(append([]string{"duplicate ✓"}, problems...), " | ")})
	}
	return out
}
