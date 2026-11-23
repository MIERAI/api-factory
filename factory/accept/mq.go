package accept

// 真实 RabbitMQ 的场景(R4 设计稿 §7.2 的第二层): 服务器带 AMQP_URL 运行(拓扑 / outbox 的中继 / 消费循环都是真的),
// 只在设置了 FACTORY_TEST_AMQP(= 服务器的 AMQP_URL)时跑,每个 API 几个场景(语义在进程内的差分里验证):
//   - 发出: 写 → outbox → 中继(publisher confirm 之后才标 published_at)→ 交换机;旁听队列收到同一个 id、同样内容的消息;
//     服务里订阅了这个事件的处理器处理了它(端到端)
//   - 处理器: 发到交换机 → 消费 → 提交 → 确认;发出的消息经中继发出;同一个 id 再发 → 只处理一次
//   - 毒消息 → <queue>.dlq;DLQ 的 requeue 保留 id(还是毒消息 → 回到 DLQ,不丢、不重复)
//   - 重试: 前 1 次失败(x-factory-fail-attempts)→ 副本进 .retry.1(TTL 1s)→ 回到主队列 → 成功,只生效一次
// 共用一个 RabbitMQ 的并行 worker 会互相抢消息: 用独立的 vhost(make accept-mq 串行)。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/MIERAI/api-factory/runtime/broker"
	"github.com/MIERAI/api-factory/runtime/consume"
	"github.com/MIERAI/api-factory/runtime/message"
	"github.com/MIERAI/api-factory/runtime/topology"

	"apifactory/factory/ir"
	"apifactory/factory/spec"
)

// MQEnv: 真实 RabbitMQ 的场景的开关(服务器用同一个 AMQP_URL)。
const MQEnv = "FACTORY_TEST_AMQP"

type mqEnv struct {
	*msgEnv
	url  string
	conn *amqp.Connection
	ch   *amqp.Channel
	tap  <-chan amqp.Delivery // 旁听: factory.events 的全部事件
	seen map[string][]byte    // 旁听收到的: id → body
}

func runMQ(ctx context.Context, d *dbs, url string, plan *ir.Plan, api *ir.API, s *spec.Spec) []Result {
	amqpURL := os.Getenv(MQEnv)
	if amqpURL == "" || api.Command == nil || (!api.Command.IsHandler() && len(api.Command.Emits) == 0) {
		return nil
	}
	fail := func(c, f string, a ...any) []Result {
		return []Result{{Spec: api.ID, Suite: "diff", Case: "RabbitMQ: " + c, Passed: false, Detail: fmt.Sprintf(f, a...)}}
	}
	var base *spec.Case
	for i := range s.Acceptance.Cases {
		if s.Acceptance.Cases[i].Expect.Code == "OK" {
			base = &s.Acceptance.Cases[i]
			break
		}
	}
	if base == nil {
		return fail("准备", "没有成功的人写用例(场景用它的准备数据和请求)")
	}
	conn, err := amqp.Dial(amqpURL)
	if err != nil {
		return fail("准备", "%v", err)
	}
	defer conn.Close()
	ch, err := conn.Channel()
	if err != nil {
		return fail("准备", "%v", err)
	}
	if err := topology.DeclareExchanges(ch, topology.DefaultExchanges); err != nil {
		return fail("准备", "%v", err)
	}
	q, err := ch.QueueDeclare("", false, true, true, false, nil)
	if err == nil {
		err = ch.QueueBind(q.Name, "#", topology.DefaultExchanges.Events, false, nil)
	}
	if err == nil {
		err = bindCommands(ch, q.Name, api.Command.Contracts)
	}
	if err != nil {
		return fail("准备", "旁听队列: %v", err)
	}
	tap, err := ch.Consume(q.Name, "", true, true, false, false, nil)
	if err != nil {
		return fail("准备", "%v", err)
	}
	e := &mqEnv{msgEnv: newMsgEnv(ctx, d, url, api), url: amqpURL, conn: conn, ch: ch, tap: tap, seen: map[string][]byte{}}
	reset := func() error {
		if err := loadFixtures(ctx, d, plan, s.Acceptance.Fixtures); err != nil {
			return err
		}
		return e.reset()
	}
	var out []Result
	add := func(name string, problems []string, ok string) {
		out = append(out, Result{Spec: api.ID, Suite: "diff", Case: "RabbitMQ: " + name, Passed: len(problems) == 0, Detail: strings.Join(append([]string{ok}, problems...), " | ")})
	}
	if !api.Command.IsHandler() { // 组合命令的 发出
		if err := reset(); err != nil {
			return fail("准备", "%v", err)
		}
		r := runStepResult(url, base, &api.Response)
		var problems []string
		if r != "" {
			problems = append(problems, r)
		}
		problems = append(problems, e.waitRelayed(10*time.Second)...)
		add("发出 → outbox → 中继 → 交换机", problems, "published_at 有值、旁听收到同一个 id 的同样内容 ✓")
		return out
	}
	// 处理器: 发到交换机 → 消费 → 提交 → 确认
	if err := reset(); err != nil {
		return fail("准备", "%v", err)
	}
	id := message.NewID()
	body := e.envelope(id, base.Request)
	problems := e.publish(body, id, nil)
	problems = append(problems, e.waitProcessed(id, 10*time.Second)...)
	problems = append(problems, e.waitRelayed(10*time.Second)...)
	n0, _ := e.outboxCount()
	add("投递 → 处理 → 提交 → 确认;发出的消息经中继发出", problems, "✓")
	// 同一个 id 再发
	problems = e.publish(body, id, nil)
	time.Sleep(1500 * time.Millisecond)
	if n, _ := e.outboxCount(); n != n0 {
		problems = append(problems, fmt.Sprintf("重投之后 outbox %d → %d 行(应该不变)", n0, n))
	}
	if p, _ := e.processed(); len(p) != 1 {
		problems = append(problems, fmt.Sprintf("去重表 %d 行(应该 1)", len(p)))
	}
	add("同一个 id 再发 → 只处理一次", problems, "duplicate ✓")
	// 毒消息 → DLQ;requeue 保留 id → 还是毒消息 → 回到 DLQ
	b := broker.New(amqpURL, "factory-accept")
	defer b.Close()
	dlq := topology.DLQ(e.queue)
	d0, _ := consume.QueueDepth(ctx, b, dlq)
	pid := message.NewID()
	problems = e.publish(e.envelope(pid, map[string]any{}), pid, nil)
	problems = append(problems, waitDepth(ctx, b, dlq, d0+1, 10*time.Second)...)
	if n, err := consume.DLQRequeue(ctx, b, e.queue, 100); err != nil || n < 1 {
		problems = append(problems, fmt.Sprintf("dlq requeue: %d %v", n, err))
	}
	problems = append(problems, waitDepth(ctx, b, dlq, d0+1, 10*time.Second)...)
	if letters, err := consume.DLQList(ctx, b, e.queue, 100); err == nil {
		k := 0
		for _, l := range letters {
			if l.ID == pid {
				k++
			}
		}
		if k != 1 {
			problems = append(problems, fmt.Sprintf("DLQ 里这条毒消息 %d 份(应该 1)", k))
		}
	}
	_, _ = consume.DLQDrop(ctx, b, e.queue, 1000)
	add("毒消息 → DLQ;requeue 保留 id → 回到 DLQ", problems, "✓")
	// 重试: 前 1 次失败 → .retry.1(1s)→ 成功
	if err := reset(); err != nil {
		return append(out, fail("准备", "%v", err)...)
	}
	rid := message.NewID()
	problems = e.publish(e.envelope(rid, base.Request), rid, amqp.Table{message.HeaderFailAttempts: "1"})
	start := time.Now()
	problems = append(problems, e.waitProcessed(rid, 15*time.Second)...)
	if el := time.Since(start); len(problems) == 0 && el < 900*time.Millisecond {
		problems = append(problems, fmt.Sprintf("%v 就处理完了: 没有经过 1s 的重试队列", el))
	}
	if n, _ := consume.QueueDepth(ctx, b, topology.RetryQueue(e.queue, 1)); n != 0 {
		problems = append(problems, fmt.Sprintf("重试队列还有 %d 条", n))
	}
	if p, _ := e.processed(); len(p) != 1 {
		problems = append(problems, fmt.Sprintf("去重表 %d 行(应该 1: 只生效一次)", len(p)))
	}
	add("暂时性错误 → 重试队列 → 成功(只生效一次)", problems, "✓")
	return out
}

// bindCommands: 发出的命令(别的服务执行的,例: review.request)也绑到旁听队列: 中继按 mandatory 发命令,
// 没有队列收的命令留在 outbox 里(没有执行方的服务在这里扮演执行方)。
func bindCommands(ch *amqp.Channel, q string, cts []ir.Contract) error {
	for _, ct := range cts {
		if ct.Kind == "command" {
			if err := ch.QueueBind(q, ct.Type, topology.DefaultExchanges.Commands, false, nil); err != nil {
				return err
			}
		}
	}
	return nil
}

func (e *mqEnv) publish(body []byte, id string, h amqp.Table) []string {
	r := e.api.Command.Receive
	ex := topology.DefaultExchanges.Events
	if r.Kind == "command" {
		ex = topology.DefaultExchanges.Commands
	}
	err := e.ch.PublishWithContext(e.ctx, ex, r.Type, false, false, amqp.Publishing{
		ContentType: message.ContentType, DeliveryMode: amqp.Persistent, MessageId: id, Type: r.Type, Body: body, Headers: h})
	if err != nil {
		return []string{"publish: " + err.Error()}
	}
	return nil
}

func (e *mqEnv) waitProcessed(id string, d time.Duration) []string {
	for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
		if p, err := e.processed(); err == nil && p[id] {
			return nil
		}
	}
	return []string{fmt.Sprintf("%v 之内没有处理(去重表里没有 %s)", d, id)}
}

// waitRelayed: outbox 的全部行都发出了(published_at),旁听队列收到了每个事件(同一个 id、同样的 data)。
func (e *mqEnv) waitRelayed(d time.Duration) []string {
	end := time.Now().Add(d)
	for ; time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
		var pending int
		if err := e.d.main.QueryRow(e.ctx, `SELECT count(*) FROM factory_outbox WHERE published_at IS NULL`).Scan(&pending); err == nil && pending == 0 {
			break
		}
	}
	rows, err := e.d.main.Query(e.ctx, `SELECT id::text, kind, envelope::text, published_at IS NOT NULL FROM factory_outbox ORDER BY seq`)
	if err != nil {
		return []string{err.Error()}
	}
	type row struct {
		id, env string
		kind    int
		pub     bool
	}
	var all []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.kind, &r.env, &r.pub); err != nil {
			rows.Close()
			return []string{err.Error()}
		}
		all = append(all, r)
	}
	rows.Close()
	var out []string
	for _, r := range all {
		if !r.pub {
			out = append(out, fmt.Sprintf("outbox %s: %v 之内没有发出(published_at 为空)", r.id, d))
			continue
		}
		if r.kind != int(message.Event) {
			continue
		}
		got, ok := e.tapWait(r.id, time.Until(end)+time.Second)
		if !ok {
			out = append(out, fmt.Sprintf("旁听队列没有收到 %s", r.id))
			continue
		}
		var a, b any
		_ = json.Unmarshal([]byte(r.env), &a)
		_ = json.Unmarshal(got, &b)
		if fmt.Sprint(a) != fmt.Sprint(b) {
			out = append(out, fmt.Sprintf("%s: 发出的内容和 outbox 不同", r.id))
		}
	}
	return out
}

func (e *mqEnv) tapWait(id string, d time.Duration) ([]byte, bool) {
	if b, ok := e.seen[id]; ok {
		return b, true
	}
	t := time.After(d)
	for {
		select {
		case m := <-e.tap:
			e.seen[m.MessageId] = m.Body
			if m.MessageId == id {
				return m.Body, true
			}
		case <-t:
			return nil, false
		}
	}
}

func (e *mqEnv) outboxCount() (int, error) {
	var n int
	err := e.d.main.QueryRow(e.ctx, `SELECT count(*) FROM factory_outbox`).Scan(&n)
	return n, err
}

func waitDepth(ctx context.Context, b *broker.Conn, queue string, want int, d time.Duration) []string {
	n := -1
	for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(150 * time.Millisecond) {
		var err error
		if n, err = consume.QueueDepth(ctx, b, queue); err == nil && n == want {
			return nil
		}
	}
	return []string{fmt.Sprintf("%s: %d 条(期待 %d)", queue, n, want)}
}

// runStepResult: 发一个人写的请求(成功的用例),"" = 响应符合用例。
func runStepResult(url string, c *spec.Case, out *ir.Message) string {
	ok, detail := runCase(url, *c, out)
	if ok {
		return ""
	}
	return "请求: " + detail
}
