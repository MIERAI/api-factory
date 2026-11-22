package accept

// 消息(R4,docs/design/r4-events-messages.md §7)的验收。消息处理器经进程内投递(运行时的 /factory.test/deliver:
// consume.Consumer.Process = 契约校验 + 去重 + 处理器 + 提交,不经 RabbitMQ)执行:
//   - 人写的用例: 准备数据 → 投递 → 结果(成功 / 业务结果 / 死信 / 毒消息)+ outbox 里新发出的消息 + 之后的表 + 去重表;
//     同一条消息再投一次 → duplicate,什么都不变(消费端的幂等)
//   - 自动生成的校验用例: 违反契约的 data → 毒消息(解码失败),合法的 → 不是解码失败
//   - 状态差分: 随机的初始状态 + 随机的消息序列(新的消息按"目标"抽样;同一个 id 重投;同样内容、新 id 的重发);
//     每一步比较 结果 + 全部相关的表 + outbox 新增的消息(类型 / 主体 / data)+ 去重表,模型(command.RunHandler)vs 数据库。
//     覆盖条件: 每种结果(成功 / 每个业务结果 / 死信 / 毒消息 / duplicate / 重发)、每项 的第 k 项不满足(k = 1, 2)、跳过
//   - 故障注入: 第 k 个写操作(含 outbox 的行)之后失败 → 结果 = 重试,表 / outbox / 去重表都不变;同一条消息再投 → 按模型处理一次
//   - 并发: 同一条消息同时投 8 次 → 只处理一次;不同的消息同时争同一批行 → 最终状态 = 按处理结果的串行执行
// 组合命令的 发出: 人写的用例比较 outbox,状态差分比较每个请求新增的消息,故障注入检查回滚之后 outbox 也没有(command_prog.go)。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/MIERAI/api-factory-runtime/consume"
	"github.com/MIERAI/api-factory-runtime/message"

	"apifactory/factory/command"
	"apifactory/factory/compiler"
	"apifactory/factory/ir"
	"apifactory/factory/spec"
)

const (
	envPlaceholder = "$信封.id" // 人写的用例的 之后: 这个值 = 投递的消息的 id
	handlerSteps   = 56       // 状态差分: 消息序列的长度(另加覆盖目标)
	concurrentMsgs = 8
)

// delivered: 进程内投递的回答。
type delivered struct {
	Outcome  string `json:"outcome"`
	Business string `json:"business"`
	Attempt  int    `json:"attempt"`
	Error    string `json:"error"`
}

// outRow: outbox 的一行。
type outRow struct {
	Seq     int64
	ID      string
	Type    string
	Subject string
	Source  string
	Data    map[string]any
}

// msgEnv: 一个 API 的消息相关的环境(消息处理器 / 有 发出 的组合命令)。
type msgEnv struct {
	ctx     context.Context
	d       *dbs
	api     *ir.API
	base    string // 服务器的根 URL
	queue   string // 处理器的名字(= 队列)
	source  string
	lastSeq int64
}

func newMsgEnv(ctx context.Context, d *dbs, url string, api *ir.API) *msgEnv {
	base := url
	if i := strings.Index(url, "/"+api.ProtoPackage+"."); i >= 0 {
		base = url[:i]
	}
	src := strings.Split(api.ProtoPackage, ".")[0]
	e := &msgEnv{ctx: ctx, d: d, api: api, base: base, source: src}
	if r := api.Command.Receive; r != nil {
		e.queue = src + "." + r.Type
	}
	return e
}

// reset: 清空 outbox 和去重表(每个用例 / 每组测试从空开始)。
func (e *msgEnv) reset() error {
	_, err := e.d.main.Exec(e.ctx, "TRUNCATE factory_outbox, factory_processed_messages RESTART IDENTITY")
	e.lastSeq = 0
	return err
}

// envelope: data(请求的写法)→ 消息的 body。
func (e *msgEnv) envelope(id string, data map[string]any) []byte {
	r := e.api.Command.Receive
	body, _ := json.Marshal(map[string]any{
		"id": id, "type": r.Type, "version": r.Version, "occurred_at": time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339),
		"source": "factory-accept", "subject": fmt.Sprint(data[r.Subject]), "data": withoutNow(data),
	})
	return body
}

// deliver: 进程内投递。now = 处理开始的时刻(RFC 3339),fault > 0 = 第 fault 个写操作之后失败。
func (e *msgEnv) deliver(body []byte, now string, fault int) (delivered, error) {
	h := map[string]any{}
	if now != "" {
		h[message.HeaderNow] = now
	}
	if fault > 0 {
		h[message.HeaderFailAfter] = strconv.Itoa(fault)
	}
	b, _ := json.Marshal(consume.TestRequest{Handler: e.queue, Body: string(body), Headers: h})
	resp, err := http.Post(e.base+consume.TestPath, "application/json", bytes.NewReader(b))
	if err != nil {
		return delivered{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return delivered{}, fmt.Errorf("deliver: HTTP %d %s(服务器要 FACTORY_TEST_FAULTS=1)", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out delivered
	if err := json.Unmarshal(raw, &out); err != nil {
		return delivered{}, fmt.Errorf("deliver: %s", raw)
	}
	return out, nil
}

// newRows: outbox 里 lastSeq 之后的行(按 seq)。
func (e *msgEnv) newRows() ([]outRow, error) {
	rows, err := e.d.main.Query(e.ctx, `SELECT seq, id::text, type, subject, envelope->>'source', envelope->'data' FROM factory_outbox WHERE seq > $1 ORDER BY seq`, e.lastSeq)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []outRow
	for rows.Next() {
		var r outRow
		var data []byte
		if err := rows.Scan(&r.Seq, &r.ID, &r.Type, &r.Subject, &r.Source, &data); err != nil {
			return nil, err
		}
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.UseNumber()
		if err := dec.Decode(&r.Data); err != nil {
			return nil, err
		}
		out = append(out, r)
		e.lastSeq = r.Seq
	}
	return out, rows.Err()
}

// processed: 去重表里这个处理器的消息 id。
func (e *msgEnv) processed() (map[string]bool, error) {
	rows, err := e.d.main.Query(e.ctx, `SELECT message_id::text FROM factory_processed_messages WHERE handler = $1`, e.queue)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// contract: 发出的消息类型的契约。
func (e *msgEnv) contract(typ string) *ir.Contract {
	for i := range e.api.Command.Contracts {
		if e.api.Command.Contracts[i].Type == typ {
			return &e.api.Command.Contracts[i]
		}
	}
	return nil
}

// checkRows: outbox 新增的行 vs 模型发出的消息(顺序、类型、主体、data 的每个字段;id 是 UUIDv7、source = 服务名)。
func (e *msgEnv) checkRows(want []command.Msg, got []outRow) []string {
	var out []string
	if len(want) != len(got) {
		var gt, wt []string
		for _, g := range got {
			gt = append(gt, g.Type)
		}
		for _, w := range want {
			wt = append(wt, w.Type)
		}
		return []string{fmt.Sprintf("发出的消息: 模型 %d 条 %v,outbox %d 条 %v", len(want), wt, len(got), gt)}
	}
	for i, w := range want {
		g := got[i]
		if g.Type != w.Type {
			out = append(out, fmt.Sprintf("第 %d 条消息: 模型 %s,outbox %s", i+1, w.Type, g.Type))
			continue
		}
		if !message.ValidID(g.ID) || g.Source != e.source {
			out = append(out, fmt.Sprintf("第 %d 条消息 %s: id %q(要 UUIDv7)/ source %q(要 %s)", i+1, g.Type, g.ID, g.Source, e.source))
		}
		ct := e.contract(w.Type)
		if ct == nil {
			out = append(out, "没有契约 "+w.Type)
			continue
		}
		if ws := command.ShowRaw(canonField(ct.Message.Field(ct.Subject), w.Data[ct.Subject])); ws != g.Subject {
			out = append(out, fmt.Sprintf("第 %d 条消息 %s 的主体: 模型 %s,outbox %s", i+1, w.Type, ws, g.Subject))
		}
		for _, f := range ct.Message.Fields {
			wv, gv := canonField(&f, w.Data[f.Name]), canonField(&f, g.Data[f.Name])
			if !sameCanon(wv, gv) {
				out = append(out, fmt.Sprintf("第 %d 条消息 %s.%s: 模型 %v,outbox %v", i+1, w.Type, f.Name, showAny(wv), showAny(gv)))
			}
		}
	}
	return out
}

// canonField: data 的一个值 → 规范值(按 proto 字段的种类;列表 = 每个元素的每个字段)。没有 = 零值(protojson 写出全部字段)。
func canonField(f *ir.Field, v any) any {
	if f == nil {
		return v
	}
	if f.List && f.Message != nil {
		var out []map[string]any
		var items []any
		switch x := v.(type) {
		case []any:
			items = x
		case []map[string]any:
			for _, m := range x {
				items = append(items, m)
			}
		}
		for _, it := range items {
			m, _ := it.(map[string]any)
			e := map[string]any{}
			for _, sf := range f.Message.Fields {
				e[sf.Name] = canonField(&sf, m[sf.Name])
			}
			out = append(out, e)
		}
		return out
	}
	typ, err := compiler.InputSQLType(f)
	if err != nil {
		return v
	}
	if v == nil {
		switch typ {
		case "int2", "int4", "int8":
			v = "0"
		case "text":
			v = ""
		case "bool":
			v = false
		}
	}
	if n, ok := v.(json.Number); ok {
		v = n.String()
	}
	c, err := command.Canon(typ, v)
	if err != nil {
		return v
	}
	return c
}

func sameCanon(a, b any) bool {
	am, aok := a.([]map[string]any)
	bm, bok := b.([]map[string]any)
	if aok || bok {
		if len(am) != len(bm) {
			return false
		}
		for i := range am {
			for k, v := range am[i] {
				if !command.Equal(v, bm[i][k]) {
					return false
				}
			}
		}
		return true
	}
	return command.Equal(a, b)
}

func showAny(v any) string {
	if m, ok := v.([]map[string]any); ok {
		var parts []string
		for _, x := range m {
			keys := make([]string, 0, len(x))
			for k := range x {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			var kv []string
			for _, k := range keys {
				kv = append(kv, k+"="+command.Show(x[k]))
			}
			parts = append(parts, "{"+strings.Join(kv, " ")+"}")
		}
		return "[" + strings.Join(parts, " ") + "]"
	}
	return command.Show(v)
}

// ---------------------------------------------------------------------------
// 人写的用例 / 自动生成的校验用例
// ---------------------------------------------------------------------------

// runHandler: 一个消息处理器的全部验收。
func runHandler(ctx context.Context, d *dbs, url string, plan *ir.Plan, api *ir.API, s *spec.Spec) ([]Result, error) {
	e := newMsgEnv(ctx, d, url, api)
	tables, err := commandTables(ctx, d.main, api, s.Acceptance.Fixtures, s.Acceptance.Cases)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", s.ID(), err)
	}
	var results []Result
	for i, c := range s.Acceptance.Cases {
		ok, detail := e.handlerCase(plan, tables, s.Acceptance.Fixtures, c, i)
		results = append(results, Result{Spec: s.ID(), Case: c.Name, Passed: ok, Detail: detail})
	}
	auto, err := spec.LoadAutoAcceptance(s.ID())
	if err != nil {
		return nil, err
	}
	if auto != nil {
		for _, suite := range auto.Suites {
			fx := s.Acceptance.Fixtures
			if suite.Fixtures != nil {
				fx = suite.Fixtures
			}
			for _, c := range suite.Cases {
				if err := loadFixtures(ctx, d, plan, fx); err != nil {
					return nil, err
				}
				ok, detail := e.validationCase(c)
				results = append(results, Result{Spec: s.ID(), Suite: "auto:" + suite.Name, Case: c.Name, Passed: ok, Detail: detail})
			}
		}
	}
	return append(results, runHandlerDiff(ctx, d, url, plan, api)...), nil
}

func (e *msgEnv) handlerCase(plan *ir.Plan, tables map[string]*command.Table, fixtures map[string][]map[string]any, c spec.Case, n int) (bool, string) {
	if err := loadFixtures(e.ctx, e.d, plan, fixtures); err != nil {
		return false, "fixtures: " + err.Error()
	}
	if err := e.reset(); err != nil {
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
	id := message.NewID()
	body := e.envelope(id, c.Request)
	got, err := e.deliver(body, c.Now, 0)
	if err != nil {
		return false, err.Error()
	}
	var problems []string
	want := c.Expect.Code
	switch {
	case want == "OK":
		if got.Outcome != "ok" || got.Business != "" {
			problems = append(problems, fmt.Sprintf("结果: 期待 成功,服务器 %s %s %s", got.Outcome, got.Business, got.Error))
		}
	case want == "POISON":
		if got.Outcome != "poison" {
			problems = append(problems, fmt.Sprintf("结果: 期待 毒消息,服务器 %s %s", got.Outcome, got.Error))
		}
	case strings.HasPrefix(want, "DLQ:"):
		name := strings.TrimPrefix(want, "DLQ:")
		if got.Outcome != "poison" || !strings.Contains(got.Error, name+":") {
			problems = append(problems, fmt.Sprintf("结果: 期待 %s(死信),服务器 %s %s", name, got.Outcome, got.Error))
		}
	default:
		if got.Outcome != "ok" || got.Business != want {
			problems = append(problems, fmt.Sprintf("结果: 期待 %s,服务器 %s %q %s", want, got.Outcome, got.Business, got.Error))
		}
	}
	rows, err := e.newRows()
	if err != nil {
		return false, err.Error()
	}
	if c.Emitted != nil {
		problems = append(problems, emittedDiff(*c.Emitted, rows)...)
	}
	proc, err := e.processed()
	if err != nil {
		return false, err.Error()
	}
	committed := want == "OK" || (want != "POISON" && !strings.HasPrefix(want, "DLQ:"))
	if committed != proc[id] {
		problems = append(problems, fmt.Sprintf("去重表: 期待 处理过了=%v,数据库 %v", committed, proc[id]))
	}
	problems = append(problems, afterProblems(plan, tables, before, c.After, id, e)...)
	// 同一条消息再投一次: 处理过了 → duplicate,什么都不变;没提交的(毒消息 / 死信)→ 同样的结果
	again, err := e.deliver(body, c.Now, 0)
	if err != nil {
		return false, err.Error()
	}
	switch {
	case committed && again.Outcome != "duplicate":
		problems = append(problems, fmt.Sprintf("重投同一条消息: 期待 duplicate,服务器 %s %s", again.Outcome, again.Business))
	case !committed && again.Outcome != got.Outcome:
		problems = append(problems, fmt.Sprintf("重投同一条消息: 期待 %s,服务器 %s", got.Outcome, again.Outcome))
	}
	if extra, _ := e.newRows(); len(extra) > 0 && committed {
		problems = append(problems, fmt.Sprintf("重投同一条消息: 又发出了 %d 条消息", len(extra)))
	}
	detail := fmt.Sprintf("结果 %s %s", got.Outcome, got.Business)
	if len(problems) > 0 {
		return false, detail + " | " + strings.Join(problems, "; ")
	}
	return true, detail + " | 发出 / 之后 / 去重 / 重投 ✓"
}

// afterProblems: 之后 的状态(没写出的表 = 不变);$信封.id = 投递的消息的 id。
func afterProblems(plan *ir.Plan, tables map[string]*command.Table, before map[string][]command.Row, after *spec.After, id string, e *msgEnv) []string {
	if after == nil {
		return []string{"用例没有 之后"}
	}
	names := make([]string, 0, len(tables))
	for n := range tables {
		names = append(names, n)
	}
	sort.Strings(names)
	var out []string
	for _, n := range names {
		t := tables[n]
		got, err := command.ReadRows(e.ctx, e.d.main, t)
		if err != nil {
			return []string{err.Error()}
		}
		want, listed := after.Tables[n]
		if after.Unchanged || !listed {
			if msg := command.Diff(t, before[n], got, false); msg != "" {
				out = append(out, "应该不变: "+msg)
			}
			continue
		}
		var subst []map[string]any
		for _, r := range want {
			m := map[string]any{}
			for k, v := range r {
				if v == envPlaceholder {
					v = id
				}
				m[k] = v
			}
			subst = append(subst, m)
		}
		if msg := afterDiff(plan, t, subst, got); msg != "" {
			out = append(out, msg)
		}
	}
	return out
}

// emittedDiff: 用例的 发出(写出来的字段)vs outbox 新增的行。
func emittedDiff(want []spec.EmittedMsg, got []outRow) []string {
	if len(want) != len(got) {
		var gt []string
		for _, g := range got {
			gt = append(gt, g.Type)
		}
		return []string{fmt.Sprintf("发出: 期待 %d 条,outbox %d 条 %v", len(want), len(got), gt)}
	}
	var out []string
	for i, w := range want {
		if w.Type != got[i].Type {
			out = append(out, fmt.Sprintf("发出 第 %d 条: 期待 %s,outbox %s", i+1, w.Type, got[i].Type))
			continue
		}
		for k, v := range w.Data {
			if !looseEqual(v, got[i].Data[k]) {
				out = append(out, fmt.Sprintf("发出 第 %d 条 %s.%s: 期待 %v,outbox %v", i+1, w.Type, k, v, got[i].Data[k]))
			}
		}
	}
	return out
}

// looseEqual: 用例里写的值(YAML)vs data 的 JSON(64 位整数是字符串;列表逐个元素比较写出来的字段)。
func looseEqual(want, got any) bool {
	switch w := want.(type) {
	case []any:
		g, ok := got.([]any)
		if !ok || len(g) != len(w) {
			return false
		}
		for i := range w {
			if !looseEqual(w[i], g[i]) {
				return false
			}
		}
		return true
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			return false
		}
		for k, v := range w {
			if !looseEqual(v, g[k]) {
				return false
			}
		}
		return true
	case time.Time:
		return w.UTC().Format(time.RFC3339Nano) == fmt.Sprint(got)
	}
	return fmt.Sprint(want) == fmt.Sprint(got)
}

// validationCase: 自动生成的校验用例(INVALID_ARGUMENT = 违反契约 → 毒消息,解码失败)。
func (e *msgEnv) validationCase(c spec.Case) (bool, string) {
	if err := e.reset(); err != nil {
		return false, err.Error()
	}
	got, err := e.deliver(e.envelope(message.NewID(), c.Request), c.Now, 0)
	if err != nil {
		return false, err.Error()
	}
	decodeFail := got.Outcome == "poison" && strings.Contains(got.Error, ": data: ")
	want := c.Expect.Code
	switch {
	case want == "INVALID_ARGUMENT" && !decodeFail:
		return false, fmt.Sprintf("期待 毒消息(违反契约),服务器 %s %s", got.Outcome, got.Error)
	case want == "!INVALID_ARGUMENT" && decodeFail:
		return false, fmt.Sprintf("合法的消息被当成违反契约: %s", got.Error)
	}
	return true, fmt.Sprintf("%s %s", got.Outcome, got.Business)
}

// ---------------------------------------------------------------------------
// 状态差分 / 故障注入 / 并发
// ---------------------------------------------------------------------------

type handlerEnv struct {
	*progEnv
	m     *msgEnv
	proc  map[string]bool // 模型的已处理集合
	sent  []sentMsg       // 投递过的消息(重投 / 重发用)
	home  map[string][2]string
	ihome map[string][2]string   // 每项: 元素的字段 → 表.列
	binds map[string][][2]string // 字段(顶层 / 元素)→ 写进 / 按它找的全部 表.列
	item  *ir.Message
}

type sentMsg struct {
	id   string
	data map[string]any
}

func runHandlerDiff(ctx context.Context, d *dbs, url string, plan *ir.Plan, api *ir.API) []Result {
	pe, fail := newProgEnv(ctx, d, url, plan, api)
	if fail != nil {
		return fail
	}
	h := &handlerEnv{progEnv: pe, m: newMsgEnv(ctx, d, url, api), home: map[string][2]string{}, ihome: map[string][2]string{}, binds: map[string][][2]string{}}
	cmd := api.Command
	if cmd.Each != "" {
		h.item = api.Request.Field(cmd.Each).Message
	}
	for _, op := range cmd.Ops {
		for _, v := range append(append([]ir.CommandVal{}, op.Key...), op.Values...) {
			if v.Expr.Op == "input" || v.Expr.Op == "item" {
				b := [2]string{op.Table, v.Column}
				if !containsBind(h.binds[v.Expr.Op+":"+v.Expr.Name], b) {
					h.binds[v.Expr.Op+":"+v.Expr.Name] = append(h.binds[v.Expr.Op+":"+v.Expr.Name], b)
				}
			}
			switch v.Expr.Op {
			case "input":
				if _, ok := h.home[v.Expr.Name]; !ok {
					h.home[v.Expr.Name] = [2]string{op.Table, v.Column}
				}
			case "item":
				if _, ok := h.ihome[v.Expr.Name]; !ok || indexOf(h.cat[op.Table].PK, v.Column) >= 0 && op.Kind != "insert_idempotent" {
					h.ihome[v.Expr.Name] = [2]string{op.Table, v.Column}
				}
			}
		}
	}
	var results []Result
	for _, suite := range []func() []Result{h.diff, h.faults, h.concurrency} {
		if err := h.reload(); err != nil {
			return append(results, Result{Spec: api.ID, Suite: "diff", Case: "R4 准备", Passed: false, Detail: err.Error()})
		}
		results = append(results, suite()...)
	}
	return results
}

func containsBind(xs [][2]string, b [2]string) bool {
	for _, x := range xs {
		if x == b {
			return true
		}
	}
	return false
}

func (h *handlerEnv) reload() error {
	if err := h.load(); err != nil {
		return err
	}
	if err := h.align(); err != nil {
		return err
	}
	h.proc = map[string]bool{} // 投递过的消息(h.sent)留着: 新的状态上重发 / 重投(去重表也清空了)
	return h.m.reset()
}

// align: 同一个字段写进 / 按它找好几张表(例: 项.account_id → debits.account_id 和 accounts 的主键)时,随机的初始状态里
// 这些列取同一批值(不然扣款记录和账户永远对不上,命中 / 跳过 / 墓碑的路径造不出来)。没有外键的表之间才需要。
func (h *handlerEnv) align() error {
	changed := false
	for _, bs := range h.binds {
		var src *[2]string
		for i, b := range bs {
			if pk := h.cat[b[0]].PK; len(pk) == 1 && pk[0] == b[1] {
				src = &bs[i]
			}
		}
		if src == nil || h.state.Tables[src[0]] == nil || len(h.state.Tables[src[0]].Rows) == 0 {
			continue
		}
		vals := h.state.Tables[src[0]].Rows
		for _, b := range bs {
			if b == *src || h.state.Tables[b[0]] == nil {
				continue
			}
			t := h.cat[b[0]]
			for _, r := range h.state.Tables[b[0]].Rows {
				if h.rnd.Intn(100) < 15 { // 留一些对不上的(找不到 / 毒消息)
					continue
				}
				nv := rawOf(vals[h.rnd.Intn(len(vals))][src[1]])
				var where []string
				args := []any{nv}
				for _, k := range t.PK {
					args = append(args, rawOf(r[k]))
					where = append(where, fmt.Sprintf("%s = $%d", pgxIdent(k), len(args)))
				}
				q := fmt.Sprintf("UPDATE %s SET %s = $1 WHERE %s", pgxIdent(b[0]), pgxIdent(b[1]), strings.Join(where, " AND "))
				if _, err := h.conn.Exec(h.ctx, q, args...); err == nil {
					changed = true
				}
			}
		}
	}
	if !changed {
		return nil
	}
	db, err := h.readState()
	if err != nil {
		return err
	}
	h.syncState(db)
	return nil
}

// sampleMsg: 一条随机的消息(data 的请求写法)。键的字段取已有的行(命中 / 墓碑 / 跳过)或者新的;条件里的数量取边界。
func (h *handlerEnv) sampleMsg() map[string]any {
	cmd := h.api.Command
	req := map[string]any{}
	anchor := map[string]any{} // 列 → 值: 顶层字段取了某一行的值时,元素优先取同一批的行
	var anchorTable string
	for _, f := range h.api.Request.Fields {
		if f.List {
			continue
		}
		hm, ok := h.home[f.Name]
		if !ok {
			if v, ok := h.base[f.Name]; ok {
				req[f.Name] = v
			}
			continue
		}
		ts := h.state.Tables[hm[0]]
		switch {
		case ts != nil && len(ts.Rows) > 0 && h.rnd.Intn(100) < 55:
			r := ts.Rows[h.rnd.Intn(len(ts.Rows))]
			req[f.Name] = h.jsonOf(&f, rawOf(r[hm[1]]))
			anchor[hm[1]], anchorTable = r[hm[1]], hm[0]
		default:
			if p := h.helper[hm[0]].pools[hm[1]]; len(p) > 0 {
				req[f.Name] = h.jsonOf(&f, p[h.rnd.Intn(len(p))])
			}
			if k := h.missingKey(hm[0]); k != nil && indexOf(h.cat[hm[0]].PK, hm[1]) >= 0 {
				req[f.Name] = h.jsonOf(&f, k[indexOf(h.cat[hm[0]].PK, hm[1])])
			}
		}
	}
	if cmd.Each == "" || h.item == nil {
		return req
	}
	n := 1 + h.rnd.Intn(3)
	used := map[string]bool{}
	var items []any
	key := cmd.EachKey[0]
	kh := h.ihome[key]
	for len(items) < n {
		el := map[string]any{}
		var target command.Row // 键对应的行(条件的边界)
		// 键: 同一批的行(anchor)/ 键所在表的已有的行 / 新的键
		var kv any
		r := h.rnd.Intn(100)
		if ts := h.state.Tables[anchorTable]; anchorTable != "" && r < 70 && ts != nil {
			var cands []command.Row
			for _, row := range ts.Rows {
				same := true
				for c, v := range anchor {
					same = same && command.Equal(row[c], v)
				}
				if same {
					cands = append(cands, row)
				}
			}
			if len(cands) > 0 {
				row := cands[h.rnd.Intn(len(cands))]
				for f, hm := range h.ihome {
					if hm[0] == anchorTable {
						if _, set := el[f]; !set {
							el[f] = rawOf(row[hm[1]])
						}
					}
				}
				kv = el[key]
			}
		}
		if kv == nil {
			ts := h.state.Tables[kh[0]]
			if ts != nil && len(ts.Rows) > 0 && h.rnd.Intn(100) < 92 {
				kv = rawOf(ts.Rows[h.rnd.Intn(len(ts.Rows))][kh[1]])
			} else if k := h.missingKey(kh[0]); k != nil {
				kv = k[indexOf(h.cat[kh[0]].PK, kh[1])]
			}
		}
		if kv == nil || used[fmt.Sprint(kv)] {
			if len(used) >= len(h.state.Tables[kh[0]].Rows) {
				break
			}
			continue
		}
		used[fmt.Sprint(kv)] = true
		el[key] = kv
		for _, op := range cmd.Ops { // 条件里的元素字段: 对象的行(按键)的列 ± 1
			if op.Kind != "update" || op.Cond == nil {
				continue
			}
			ts := h.state.Tables[op.Table]
			k := command.Row{}
			for _, kc := range op.Key {
				if kc.Expr.Op == "item" && kc.Expr.Name == key {
					k[kc.Column], _ = command.Canon(kc.Type, kv)
				}
			}
			if len(k) > 0 && ts != nil {
				target = findRow(ts, k)
			}
		}
		for _, f := range h.item.Fields {
			if _, set := el[f.Name]; set && f.Name != key && h.rnd.Intn(2) == 0 {
				continue
			}
			if f.Name == key {
				continue
			}
			var cands []any
			if hm, ok := h.ihome[f.Name]; ok {
				cands = append(cands, h.helper[hm[0]].pools[hm[1]]...)
			}
			if target != nil {
				for _, c := range target {
					if n, err := strconv.ParseInt(fmt.Sprint(c), 10, 64); err == nil && n > 0 {
						cands = append(cands, n, n+1, n-1, n, n/2+1)
					}
				}
			}
			cands = append(cands, int64(1), int64(2))
			v := cands[h.rnd.Intn(len(cands))]
			if s, ok := v.(int64); ok && s < 1 {
				v = int64(1)
			}
			el[f.Name] = v
		}
		j := map[string]any{}
		for _, f := range h.item.Fields {
			j[f.Name] = h.jsonOf(&f, el[f.Name])
		}
		items = append(items, j)
	}
	req[cmd.Each] = items
	return req
}

// modelInputs: data(请求写法)→ 模型的输入 + 每项 的元素。
func (h *handlerEnv) modelInputs(req map[string]any) (command.Inputs, []command.Inputs) {
	in := h.inputs(req)
	cmd := h.api.Command
	var items []command.Inputs
	if cmd.Each != "" && h.item != nil {
		delete(in, cmd.Each)
		list, _ := req[cmd.Each].([]any)
		for _, x := range list {
			m, _ := x.(map[string]any)
			it := command.Inputs{}
			for _, f := range h.item.Fields {
				if v, ok := m[f.Name]; ok {
					typ, _ := compiler.InputSQLType(&f)
					c, err := command.Canon(typ, v)
					if err != nil {
						c = v
					}
					it[f.Name] = c
				}
			}
			items = append(items, it)
		}
	}
	return in, items
}

// predict: 模型上处理一条消息(不改 h.state)。
func (h *handlerEnv) predict(id string, req map[string]any) (*command.State, command.HandlerResult, bool) {
	model := h.state.Clone()
	if h.proc[id] {
		return model, command.HandlerResult{}, true
	}
	if bad, err := h.val.invalid(req); err == nil && bad {
		return model, command.HandlerResult{Outcome: command.HandlerPoison, Detail: "违反契约", Failed: -1}, false
	}
	in, items := h.modelInputs(req)
	return model, model.RunHandler(h.api.Command, in, items, id, h.now()), false
}

// step: 投递一条消息: 模型 → 服务器 → 结果 / 表 / outbox / 去重表。不一致 → 模型同步成数据库。
func (h *handlerEnv) step(name, id string, req map[string]any) (Result, command.HandlerResult, bool) {
	h.seq++
	model, want, dup := h.predict(id, req)
	got, err := h.m.deliver(h.m.envelope(id, req), h.nowText(), 0)
	if err != nil {
		return Result{Spec: h.api.ID, Suite: "diff", Case: name, Passed: false, Detail: err.Error()}, want, dup
	}
	var problems []string
	switch {
	case dup:
		if got.Outcome != "duplicate" {
			problems = append(problems, fmt.Sprintf("结果: 模型 duplicate(处理过了),服务器 %s %s", got.Outcome, got.Business))
		}
	default:
		problems = append(problems, outcomeProblems(want, got)...)
	}
	db, err := h.readState()
	if err != nil {
		return Result{Spec: h.api.ID, Suite: "diff", Case: name, Passed: false, Detail: err.Error()}, want, dup
	}
	for _, p := range h.diffState(model, db, true) {
		problems = append(problems, "状态: "+p)
	}
	rows, err := h.m.newRows()
	if err != nil {
		return Result{Spec: h.api.ID, Suite: "diff", Case: name, Passed: false, Detail: err.Error()}, want, dup
	}
	problems = append(problems, h.m.checkRows(want.Emits, rows)...)
	proc, err := h.m.processed()
	if err != nil {
		return Result{Spec: h.api.ID, Suite: "diff", Case: name, Passed: false, Detail: err.Error()}, want, dup
	}
	if !dup && want.Processed() {
		h.proc[id] = true
	}
	if len(proc) != len(h.proc) || !proc[id] && h.proc[id] {
		problems = append(problems, fmt.Sprintf("去重表: 模型 %d 条(这条 %v),数据库 %d 条(这条 %v)", len(h.proc), h.proc[id], len(proc), proc[id]))
		h.proc = proc
	}
	desc := fmt.Sprintf("%s → %s", describeReq(req), handlerOutcomeText(want, dup))
	if len(problems) > 0 {
		h.syncState(db)
		return Result{Spec: h.api.ID, Suite: "diff", Case: name, Passed: false, Detail: desc + " | " + strings.Join(problems, " | ")}, want, dup
	}
	h.state = model
	return Result{Spec: h.api.ID, Suite: "diff", Case: name, Passed: true, Detail: desc}, want, dup
}

func outcomeProblems(want command.HandlerResult, got delivered) []string {
	switch want.Outcome {
	case command.HandlerOK:
		if got.Outcome != "ok" || got.Business != "" {
			return []string{fmt.Sprintf("结果: 模型 成功,服务器 %s %q %s", got.Outcome, got.Business, got.Error)}
		}
	case command.HandlerBusiness:
		if got.Outcome != "ok" || got.Business != want.Business {
			return []string{fmt.Sprintf("结果: 模型 %s(%s),服务器 %s %q %s", want.Business, want.Detail, got.Outcome, got.Business, got.Error)}
		}
	case command.HandlerDLQ:
		if got.Outcome != "poison" || !strings.Contains(got.Error, want.Business+":") {
			return []string{fmt.Sprintf("结果: 模型 %s(死信),服务器 %s %s", want.Business, got.Outcome, got.Error)}
		}
	case command.HandlerPoison:
		if got.Outcome != "poison" {
			return []string{fmt.Sprintf("结果: 模型 毒消息(%s),服务器 %s %q %s", want.Detail, got.Outcome, got.Business, got.Error)}
		}
	}
	return nil
}

func handlerOutcomeText(r command.HandlerResult, dup bool) string {
	if dup {
		return "duplicate"
	}
	t := r.Outcome
	if r.Business != "" {
		t += "(" + r.Business + ")"
	}
	if r.Failed >= 0 {
		t += fmt.Sprintf(" 第 %d 项", r.Failed+1)
	}
	if r.Skipped > 0 {
		t += fmt.Sprintf(" 跳过 %d", r.Skipped)
	}
	return t
}

// aimOf: 结果 → 覆盖目标的名字。
func aimOf(r command.HandlerResult) string {
	switch r.Outcome {
	case command.HandlerBusiness:
		return "business:" + r.Business
	case command.HandlerDLQ:
		return "dlq:" + r.Business
	}
	return r.Outcome
}

// needs: 覆盖目标(每种结果;每项 的第 1 / 2 项不满足;跳过)。
func (h *handlerEnv) needs() []string {
	cmd := h.api.Command
	out := []string{command.HandlerOK}
	hasUnmet := false
	for _, op := range cmd.Ops {
		if op.Unmet != "" {
			hasUnmet = true
		}
		if op.Skip {
			out = append(out, "skip")
		}
		if (op.Kind == "update" || op.Kind == "delete") && op.NotFound != "" && len(op.Key) > 0 && !h.guaranteed(op) {
			out = append(out, command.HandlerPoison)
		}
	}
	for _, r := range cmd.Results {
		switch {
		case r.Name == "":
		case r.DLQ:
			out = append(out, "dlq:"+r.Name)
		default:
			out = append(out, "business:"+r.Name)
		}
	}
	if hasUnmet && cmd.Each != "" {
		out = append(out, "item:1", "item:2")
	}
	seen := map[string]bool{}
	var uniq []string
	for _, x := range out {
		if !seen[x] {
			seen[x] = true
			uniq = append(uniq, x)
		}
	}
	return uniq
}

// guaranteed: 前面的幂等新建按同一个键插入了这一行(更新的对象一定存在: 找不到 造不出来)。
func (h *handlerEnv) guaranteed(op ir.CommandOp) bool {
	for _, prev := range h.api.Command.Ops {
		if prev.Name == op.Name {
			return false
		}
		if prev.Kind != "insert_idempotent" || prev.Table != op.Table {
			continue
		}
		same := len(prev.Conflict) == len(op.Key)
		for _, k := range op.Key {
			same = same && indexOf(prev.Conflict, k.Column) >= 0
		}
		if same {
			return true
		}
	}
	return false
}

func hitsAim(r command.HandlerResult, aim string) bool {
	switch {
	case aim == "skip":
		return r.Skipped > 0 && r.Outcome == command.HandlerOK
	case strings.HasPrefix(aim, "item:"):
		k, _ := strconv.Atoi(strings.TrimPrefix(aim, "item:"))
		return r.Failed == k-1
	}
	return aimOf(r) == aim
}

// findFresh: find,造不出来就换一份新的随机初始状态再试(最多 3 次)。
func (h *handlerEnv) findFresh(aim string) map[string]any {
	for i := 0; i < 4; i++ {
		if req := h.find(aim); req != nil {
			return req
		}
		if err := h.reload(); err != nil {
			return nil
		}
	}
	return nil
}

// find: 抽样到命中目标的消息。
func (h *handlerEnv) find(aim string) map[string]any {
	for i := 0; i < progSamples*2; i++ {
		req := h.sampleMsg()
		if bad, err := h.val.invalid(req); err != nil || bad {
			continue
		}
		in, items := h.modelInputs(req)
		if hitsAim(h.state.Clone().RunHandler(h.api.Command, in, items, "00000000-0000-7000-8000-000000000000", h.now()), aim) {
			return req
		}
	}
	return nil
}

func (h *handlerEnv) diff() []Result {
	needs := h.needs()
	var plan []string
	for _, a := range needs {
		for i := 0; i <= minOutcome+1; i++ {
			plan = append(plan, a)
		}
	}
	for len(plan) < handlerSteps {
		plan = append(plan, needs[h.rnd.Intn(len(needs))])
	}
	for i := 0; i < minOutcome+4; i++ {
		plan = append(plan, "redeliver", "resend")
	}
	h.rnd.Shuffle(len(plan), func(i, j int) { plan[i], plan[j] = plan[j], plan[i] })
	counts := map[string]int{}
	var results []Result
	for i, aim := range plan {
		name := fmt.Sprintf("消息 #%02d 目标 %s", i+1, aim)
		var id string
		var req map[string]any
		switch {
		case (aim == "redeliver" || aim == "resend") && len(h.sent) > 0:
			prev := h.sent[h.rnd.Intn(len(h.sent))]
			if aim == "redeliver" { // 重投: 优先取这份状态里处理过的(→ duplicate)
				var done []sentMsg
				for _, m := range h.sent {
					if h.proc[m.id] {
						done = append(done, m)
					}
				}
				if len(done) > 0 {
					prev = done[h.rnd.Intn(len(done))]
				} else if r := h.find(command.HandlerOK); r != nil { // 这份状态里还没有处理过的: 先处理一条,再重投它
					pid := message.NewID()
					res, _, _ := h.step(name+" (先处理)", pid, r)
					results = append(results, res)
					h.sent = append(h.sent, sentMsg{id: pid, data: r})
					prev = sentMsg{id: pid, data: r}
				}
			}
			id, req = prev.id, prev.data
			if aim == "resend" { // 同样内容、新 id: 语义层的幂等(可重入)
				id = message.NewID()
			}
		default:
			if aim == "redeliver" || aim == "resend" {
				aim = command.HandlerOK
			}
			req = h.find(aim)
			if req == nil {
				if err := h.reload(); err != nil {
					results = append(results, Result{Spec: h.api.ID, Suite: "diff", Case: name, Passed: false, Detail: err.Error()})
					continue
				}
				req = h.find(aim)
			}
			if req == nil {
				results = append(results, Result{Spec: h.api.ID, Suite: "diff", Case: name, Passed: true, Detail: "跳过: 造不出命中目标的消息"})
				continue
			}
			id = message.NewID()
		}
		res, want, dup := h.step(name, id, req)
		results = append(results, res)
		h.sent = append(h.sent, sentMsg{id: id, data: req})
		switch {
		case dup:
			counts["redeliver"]++
		default:
			if aim == "resend" {
				counts["resend"]++
			}
			for _, a := range needs {
				if hitsAim(want, a) {
					counts[a]++
				}
			}
		}
	}
	var parts, weak []string
	for _, a := range append(needs, "redeliver", "resend") {
		parts = append(parts, fmt.Sprintf("%s %d", a, counts[a]))
		if counts[a] < minOutcome {
			weak = append(weak, a)
		}
	}
	text := fmt.Sprintf("结果: %s(%d 条消息)", strings.Join(parts, " / "), len(plan))
	if len(weak) > 0 {
		text += fmt.Sprintf(" ✗ 少于 %d 次: %v", minOutcome, weak)
	}
	return append(results, Result{Spec: h.api.ID, Suite: "diff", Case: "覆盖条件(消息处理器)", Passed: len(weak) == 0, Detail: text})
}

// faults: 第 k 个写操作之后失败 → 重试,什么都不变;同一条消息再投 → 处理一次。
func (h *handlerEnv) faults() []Result {
	var results []Result
	for k := 1; k <= 6; k++ {
		for rep := 1; rep <= 2; rep++ {
			name := fmt.Sprintf("故障注入: 第 %d 个写操作之后失败 #%d", k, rep)
			var req map[string]any
			for i := 0; i < 6 && req == nil; i++ {
				r := h.findFresh(command.HandlerOK)
				if r == nil {
					continue
				}
				in, items := h.modelInputs(r)
				if h.state.Clone().RunHandler(h.api.Command, in, items, "x", h.now()).Writes >= k {
					req = r
				}
			}
			if req == nil {
				if k == 1 {
					results = append(results, Result{Spec: h.api.ID, Suite: "diff", Case: name, Passed: false, Detail: "造不出成功的消息"})
				}
				continue
			}
			id := message.NewID()
			h.seq++
			got, err := h.m.deliver(h.m.envelope(id, req), h.nowText(), k)
			if err != nil {
				results = append(results, Result{Spec: h.api.ID, Suite: "diff", Case: name, Passed: false, Detail: err.Error()})
				continue
			}
			var problems []string
			if got.Outcome != "retry" {
				problems = append(problems, fmt.Sprintf("结果 %s %s %s,期待 retry(暂时性错误)", got.Outcome, got.Business, got.Error))
			}
			db, err := h.readState()
			if err != nil {
				results = append(results, Result{Spec: h.api.ID, Suite: "diff", Case: name, Passed: false, Detail: err.Error()})
				continue
			}
			if d := h.diffState(h.state, db, false); len(d) > 0 {
				problems = append(problems, "状态应该不变(回滚): "+strings.Join(d, "; "))
				h.syncState(db)
			}
			if rows, _ := h.m.newRows(); len(rows) > 0 {
				problems = append(problems, fmt.Sprintf("回滚了却有 %d 条消息进了 outbox", len(rows)))
			}
			if proc, _ := h.m.processed(); proc[id] {
				problems = append(problems, "回滚了却记下了 处理过了(重试时会被当成 duplicate 跳过)")
			}
			// 重试: 同一条消息再投(没有故障)→ 按模型处理一次
			res, _, _ := h.step(name+" → 重试", id, req)
			if !res.Passed {
				problems = append(problems, "重试: "+res.Detail)
			}
			results = append(results, Result{Spec: h.api.ID, Suite: "diff", Case: name, Passed: len(problems) == 0,
				Detail: describeReq(req) + " | " + strings.Join(append([]string{"回滚 + 重试 ✓"}, problems...), " | ")})
		}
	}
	return results
}

// concurrency: 同一条消息同时投 8 次 → 只处理一次;不同的消息同时争同一批行 → 最终状态 = 按结果的串行执行。
func (h *handlerEnv) concurrency() []Result {
	var results []Result
	for rep := 1; rep <= 2; rep++ {
		name := fmt.Sprintf("并发: 同一条消息同时投 %d 次 #%d", concurrentMsgs, rep)
		req := h.findFresh(command.HandlerOK)
		if req == nil {
			results = append(results, Result{Spec: h.api.ID, Suite: "diff", Case: name, Passed: false, Detail: "造不出成功的消息"})
			continue
		}
		id := message.NewID()
		h.seq++
		model, want, _ := h.predict(id, req)
		gots := h.burst([][]byte{h.m.envelope(id, req)}, concurrentMsgs)
		var problems []string
		n := map[string]int{}
		for _, g := range gots {
			n[g.Outcome]++
		}
		if n["ok"] != 1 || n["duplicate"] != concurrentMsgs-1 {
			problems = append(problems, fmt.Sprintf("结果 %v,期待 ok 1 次、duplicate %d 次", n, concurrentMsgs-1))
		}
		db, _ := h.readState()
		if d := h.diffState(model, db, true); len(d) > 0 {
			problems = append(problems, "状态 ≠ 处理一次: "+strings.Join(d, "; "))
		}
		rows, _ := h.m.newRows()
		problems = append(problems, h.m.checkRows(want.Emits, rows)...)
		h.syncState(db)
		h.proc[id] = true
		results = append(results, Result{Spec: h.api.ID, Suite: "diff", Case: name, Passed: len(problems) == 0, Detail: strings.Join(append([]string{"只处理一次 ✓"}, problems...), " | ")})
	}
	for rep := 1; rep <= 2; rep++ {
		name := fmt.Sprintf("并发: 不同的消息同时 %d 条 #%d", concurrentMsgs, rep)
		var reqs []map[string]any
		var bodies [][]byte
		var ids []string
		keys := map[string]bool{} // 幂等新建的键不重叠(先提交的赢: 顺序未知,值不同的新建没有确定的串行结果)
		for tries := 0; len(reqs) < concurrentMsgs && tries < concurrentMsgs*4; tries++ {
			r := h.find(command.HandlerOK)
			if r == nil {
				break
			}
			ks := h.idemKeys(r)
			clash := false
			for _, k := range ks {
				clash = clash || keys[k]
			}
			if clash {
				continue
			}
			for _, k := range ks {
				keys[k] = true
			}
			id := message.NewID()
			reqs, ids, bodies = append(reqs, r), append(ids, id), append(bodies, h.m.envelope(id, r))
		}
		if len(reqs) < 2 {
			continue
		}
		h.seq++
		gots := h.burst(bodies, 1)
		db, _ := h.readState()
		// 串行: 先按 成功 的(数据库里的处理顺序未知: 成功的效果可交换),再按 业务结果 的
		model := h.state.Clone()
		var problems []string
		for _, pass := range []bool{true, false} {
			for i, g := range gots {
				if (g.Outcome == "ok" && g.Business == "") != pass {
					continue
				}
				in, items := h.modelInputs(reqs[i])
				r := model.RunHandler(h.api.Command, in, items, ids[i], h.now())
				if (pass && r.Outcome != command.HandlerOK) || (!pass && outcomeProblems(r, g) != nil) {
					problems = append(problems, fmt.Sprintf("第 %d 条: 服务器 %s %s,串行的模型 %s", i+1, g.Outcome, g.Business, handlerOutcomeText(r, false)))
				}
				if r.Processed() {
					h.proc[ids[i]] = true
				}
			}
		}
		if d := h.diffState(model, db, true); len(d) > 0 {
			problems = append(problems, "状态 ≠ 串行执行: "+strings.Join(d, "; "))
		}
		_, _ = h.m.newRows()
		h.syncState(db)
		results = append(results, Result{Spec: h.api.ID, Suite: "diff", Case: name, Passed: len(problems) == 0, Detail: strings.Join(append([]string{fmt.Sprintf("%d 条 ✓", len(reqs))}, problems...), " | ")})
	}
	return results
}

// idemKeys: 消息会新建的幂等键(表 + 键的值)。
func (h *handlerEnv) idemKeys(req map[string]any) []string {
	in, items := h.modelInputs(req)
	if len(items) == 0 {
		items = []command.Inputs{nil}
	}
	var out []string
	for _, op := range h.api.Command.Ops {
		if op.Kind != "insert_idempotent" {
			continue
		}
		for _, it := range items {
			k := op.Table
			for _, v := range op.Values {
				if indexOf(op.Conflict, v.Column) < 0 {
					continue
				}
				switch v.Expr.Op {
				case "input":
					k += "|" + fmt.Sprint(in[v.Expr.Name])
				case "item":
					k += "|" + fmt.Sprint(it[v.Expr.Name])
				default:
					k += "|" + command.Format(v.Expr)
				}
			}
			out = append(out, k)
		}
	}
	return out
}

// burst: 每个 body 同时投 times 次。
func (h *handlerEnv) burst(bodies [][]byte, times int) []delivered {
	out := make([]delivered, len(bodies)*times)
	var wg sync.WaitGroup
	now := h.nowText()
	for i, b := range bodies {
		for t := 0; t < times; t++ {
			wg.Add(1)
			go func(k int, b []byte) {
				defer wg.Done()
				out[k], _ = h.m.deliver(b, now, 0)
			}(i*times+t, b)
		}
	}
	wg.Wait()
	return out
}

func pgxIdent(s string) string { return pgx.Identifier{s}.Sanitize() }
