package tracing_test

// 集成测试(RUNTIME_TEST_DB + RUNTIME_TEST_AMQP,没有设置时跳过): 追踪穿过 outbox / 中继 / RabbitMQ / 消费者,
// 两跳(入口 → A → B)是同一条 trace,父子关系一条链:
//
//	entry ─ txn ─ outbox.write a ─ publish a ─ process qa ─ txn ─ outbox.write b ─ publish b ─ process qb ─ txn(事件 sm.transition)
//
// 每个 txn 下面有 pgx 的 SQL span(INSERT factory_outbox、COMMIT …);中继当选 = 另一条 trace 的 leader 事件。

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/MIERAI/api-factory/runtime/consume"
	"github.com/MIERAI/api-factory/runtime/internal/itest"
	"github.com/MIERAI/api-factory/runtime/message"
	"github.com/MIERAI/api-factory/runtime/outbox"
	"github.com/MIERAI/api-factory/runtime/sm"
	"github.com/MIERAI/api-factory/runtime/topology"
	"github.com/MIERAI/api-factory/runtime/tracing"
	"github.com/MIERAI/api-factory/runtime/txn"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func tracedPool(t *testing.T, dbURL string) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(dbURL)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeExec
	cfg.ConnConfig.Tracer = tracing.PGX{} // 和 server 打开追踪时一样
	p, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

func TestTraceCrossesOutboxAndBroker(t *testing.T) {
	dbURL := itest.NewDB(t)
	b := itest.Broker(t)
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	restore := tracing.Use(tp)
	defer restore()
	pool := tracedPool(t, dbURL)

	sfx := itest.Suffix()
	typA, typB := "evt.a."+sfx, "evt.b."+sfx
	qa, qb := topology.QueueName("rt-trace", typA), topology.QueueName("rt-trace", typB)
	spec := topology.Spec{Retry: topology.DefaultRetry, Queues: []topology.Queue{
		{Name: qa, Subscriptions: []topology.Subscription{{Kind: message.Event, Type: typA}}},
		{Name: qb, Subscriptions: []topology.Subscription{{Kind: message.Event, Type: typB}}},
	}}
	ch := itest.Channel(t, b)
	if err := topology.Declare(ch, spec); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if ch, err := b.Channel(context.Background()); err == nil {
			_ = topology.Delete(ch, spec)
			_ = ch.Close()
		}
	})

	exec := txn.New(pool)
	w := outbox.NewWriter("rt-trace")
	var doneB atomic.Bool
	ca := &consume.Consumer{Queue: qa, Exec: exec, Broker: b, Log: quiet, DLQCheckEvery: -1,
		Handler: func(ctx context.Context, tx pgx.Tx, env message.Envelope) error { // A: 收到 a → 发出 b
			_, err := w.Write(ctx, tx, outbox.Message{Kind: message.Event, Type: typB, Version: 1, Subject: env.Subject})
			return err
		}}
	cb := &consume.Consumer{Queue: qb, Exec: exec, Broker: b, Log: quiet, DLQCheckEvery: -1,
		Handler: func(ctx context.Context, tx pgx.Tx, env message.Envelope) error { // B: 状态机的转移
			sm.Transitioned(ctx, "Saga", "WAITING", "DONE", env.Type)
			doneB.Store(true)
			return nil
		}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	relayDone := make(chan error, 1)
	go func() {
		relayDone <- (&outbox.Relay{DB: pool, Broker: b, Log: quiet, Poll: 200 * time.Millisecond}).Run(ctx)
	}()
	go func() { _ = ca.Run(ctx) }()
	go func() { _ = cb.Run(ctx) }()

	// 入口(相当于 server 的 span): 一个写事务发出 a
	ectx, entry := tracing.Start(context.Background(), "entry", trace.SpanKindServer)
	if err := exec.InTx(ectx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := w.Write(ctx, tx, outbox.Message{Kind: message.Event, Type: typA, Version: 1, Subject: "o-1"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	entry.End()
	traceID := entry.SpanContext().TraceID()

	itest.Eventually(t, 20*time.Second, "B handled", doneB.Load)
	byName := map[string][]sdktrace.ReadOnlySpan{}
	var inTrace []sdktrace.ReadOnlySpan
	itest.Eventually(t, 10*time.Second, "process "+qb+" ended", func() bool {
		byName, inTrace = map[string][]sdktrace.ReadOnlySpan{}, nil
		for _, s := range rec.Ended() {
			if s.SpanContext().TraceID() == traceID {
				inTrace = append(inTrace, s)
				byName[s.Name()] = append(byName[s.Name()], s)
			}
		}
		return len(byName["process "+qb]) == 1
	})
	cancel()
	if err := <-relayDone; err != nil {
		t.Fatal(err)
	}

	one := func(name string) sdktrace.ReadOnlySpan {
		t.Helper()
		if len(byName[name]) != 1 {
			var names []string
			for _, s := range inTrace {
				names = append(names, s.Name())
			}
			t.Fatalf("want exactly one span %q in the trace, got %d; spans: %s", name, len(byName[name]), strings.Join(names, ", "))
		}
		return byName[name][0]
	}
	parentOf := func(child, parent sdktrace.ReadOnlySpan) {
		t.Helper()
		if child.Parent().SpanID() != parent.SpanContext().SpanID() {
			t.Errorf("%s: parent %s, want %s (%s)", child.Name(), child.Parent().SpanID(), parent.SpanContext().SpanID(), parent.Name())
		}
	}
	entrySpan := one("entry")
	wa, pa, pra := one("outbox.write "+typA), one("publish "+typA), one("process "+qa)
	wb, pb, prb := one("outbox.write "+typB), one("publish "+typB), one("process "+qb)
	txns := byName["txn"]
	if len(txns) != 3 {
		t.Fatalf("want 3 txn spans (entry, A, B), got %d", len(txns))
	}
	txnUnder := func(parent sdktrace.ReadOnlySpan) sdktrace.ReadOnlySpan {
		for _, s := range txns {
			if s.Parent().SpanID() == parent.SpanContext().SpanID() {
				return s
			}
		}
		t.Fatalf("no txn span under %s", parent.Name())
		return nil
	}
	t0 := txnUnder(entrySpan)
	parentOf(wa, t0)
	parentOf(pa, wa)
	parentOf(pra, pa)
	ta := txnUnder(pra)
	parentOf(wb, ta)
	parentOf(pb, wb)
	parentOf(prb, pb)
	tb := txnUnder(prb)

	if k := pa.SpanKind(); k != trace.SpanKindProducer {
		t.Errorf("publish kind %v", k)
	}
	if k := prb.SpanKind(); k != trace.SpanKindConsumer {
		t.Errorf("process kind %v", k)
	}
	// 状态机的转移 = B 的事务 span 上的事件
	var transition bool
	for _, ev := range tb.Events() {
		if ev.Name == "sm.transition" {
			transition = true
		}
	}
	if !transition {
		t.Errorf("no sm.transition event on B's txn span")
	}
	// SQL 的 span: 每个事务里有 INSERT factory_outbox(A 之前的事务)和 COMMIT
	var inserts, commits int
	for _, s := range inTrace {
		switch s.Name() {
		case "INSERT factory_outbox":
			inserts++
		case "COMMIT":
			commits++
		}
	}
	if inserts != 2 || commits != 3 {
		t.Errorf("SQL spans: %d INSERT factory_outbox (want 2), %d COMMIT (want 3)", inserts, commits)
	}
	// 后台的轮询没有父 span: 不产生 SQL 的 trace(只有 leader 的事件是自己的 trace)
	for _, s := range rec.Ended() {
		if !s.Parent().IsValid() && s.SpanContext().TraceID() != traceID && !strings.HasPrefix(s.Name(), "leader ") {
			t.Errorf("unexpected root span %q (background queries must not start traces)", s.Name())
		}
	}
	var acquired, released bool
	for _, s := range rec.Ended() {
		if s.Name() != "leader "+outbox.LockName {
			continue
		}
		for _, ev := range s.Events() {
			acquired = acquired || ev.Name == "leader.acquired"
			released = released || ev.Name == "leader.released"
		}
	}
	if !acquired || !released {
		t.Errorf("leader events: acquired %v released %v", acquired, released)
	}
	t.Logf("one trace, %d spans", len(inTrace))
}
