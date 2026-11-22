// Command driver plays the order service (its saga is R5, not built yet) against the running e2e stack
// (compose.yaml): it publishes the commands / events order would publish, through the real RabbitMQ, and checks
// what inventory / payment / notification answer (events on factory.events) and store (their query APIs).
//
// Envelopes, exchanges, routing keys and headers are the runtime's (api-factory-runtime message / topology): the
// driver uses the same packages a generated service uses, so a mismatch shows up here as a failed step.
//
//	go run .            (the stack must be up: make up)       env: see config() for the endpoints
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/MIERAI/api-factory-runtime/broker"
	"github.com/MIERAI/api-factory-runtime/consume"
	"github.com/MIERAI/api-factory-runtime/message"
	"github.com/MIERAI/api-factory-runtime/topology"
)

type cfg struct {
	amqp                                      string
	inventory, payment, notification          string // API base URLs
	inventoryMet, paymentMet, notificationMet string // metrics base URLs (METRICS_PORT)
	inventoryDB, paymentDB, notificationDB    string
	wait                                      time.Duration // how long to wait for an answer
	quiet                                     time.Duration // how long "no answer" must hold
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func config() cfg {
	db := env("E2E_PG", "postgres://e2e:e2e@127.0.0.1:56432")
	return cfg{
		amqp:            env("E2E_AMQP_URL", "amqp://e2e:e2e@127.0.0.1:56672/e2e"),
		inventory:       env("E2E_INVENTORY_URL", "http://127.0.0.1:58081"),
		payment:         env("E2E_PAYMENT_URL", "http://127.0.0.1:58082"),
		notification:    env("E2E_NOTIFICATION_URL", "http://127.0.0.1:58083"),
		inventoryMet:    env("E2E_INVENTORY_METRICS", "http://127.0.0.1:59091"),
		paymentMet:      env("E2E_PAYMENT_METRICS", "http://127.0.0.1:59092"),
		notificationMet: env("E2E_NOTIFICATION_METRICS", "http://127.0.0.1:59093"),
		inventoryDB:     db + "/inventory?sslmode=disable",
		paymentDB:       db + "/payment?sslmode=disable",
		notificationDB:  db + "/notification?sslmode=disable",
		wait:            15 * time.Second,
		quiet:           2 * time.Second,
	}
}

// The queues the generated wiring declares (cmd/server/wiring_gen.go of each project): <service>.<message type>.
var consumedQueues = []string{
	"inventory.inventory.reserve", "inventory.inventory.commit", "inventory.inventory.release",
	"payment.payment.charge",
	"notification.order.confirmed", "notification.order.cancelled",
}

func main() {
	start := time.Now()
	c := config()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	d, err := newDriver(ctx, c)
	if err != nil {
		fmt.Fprintln(os.Stderr, "✗ setup:", err)
		os.Exit(1)
	}
	defer d.close()
	d.run(ctx)
	fmt.Printf("\n%d passed, %d failed (%.1fs)\n", d.passed, len(d.failed), time.Since(start).Seconds())
	if len(d.failed) > 0 {
		for _, f := range d.failed {
			fmt.Println("  ✗", f)
		}
		os.Exit(1)
	}
}

// ─── the driver ────────────────────────────────────────────────────────────────────────────────────────────

type driver struct {
	c      cfg
	conn   *amqp.Connection
	pub    *amqp.Channel
	ret    chan amqp.Return
	pubMu  sync.Mutex
	broker *broker.Conn // the runtime's connection: DLQ inspection with the operator tooling (consume.DLQList)
	invDB  *pgx.Conn

	mu     sync.Mutex
	events []message.Envelope // everything published on factory.events (a "#" listener)

	run_   string // this run's id: SKUs and order ids are unique per run (the stack can be reused)
	order  int64
	passed int
	failed []string
}

func newDriver(ctx context.Context, c cfg) (*driver, error) {
	d := &driver{c: c, broker: broker.New(c.amqp, "e2e-driver")}
	now := time.Now()
	d.run_ = strings.ToUpper(strconv.FormatInt(now.Unix(), 36))
	d.order = now.Unix() * 1000

	if err := d.waitReady(ctx); err != nil {
		return nil, err
	}
	var err error
	if d.conn, err = amqp.Dial(c.amqp); err != nil {
		return nil, fmt.Errorf("amqp: %w", err)
	}
	// listener: a private queue bound to every event (the order service is not running, so nobody else consumes
	// inventory.reserved etc.; events without a subscriber are dropped — bind before publishing anything)
	lch, err := d.conn.Channel()
	if err != nil {
		return nil, err
	}
	if err := topology.DeclareExchanges(lch, topology.DefaultExchanges); err != nil {
		return nil, err
	}
	q, err := lch.QueueDeclare("", false, true, true, false, nil)
	if err != nil {
		return nil, err
	}
	if err := lch.QueueBind(q.Name, "#", topology.DefaultExchanges.Events, false, nil); err != nil {
		return nil, err
	}
	deliveries, err := lch.Consume(q.Name, "e2e-listener", true, true, false, false, nil)
	if err != nil {
		return nil, err
	}
	go func() {
		for dl := range deliveries {
			env, err := message.Decode(dl.Body)
			if err != nil {
				fmt.Printf("  ! undecodable event on factory.events: %v\n", err)
				continue
			}
			if dl.MessageId != env.ID || dl.Type != env.Type {
				fmt.Printf("  ! AMQP properties disagree with the envelope: message_id=%q type=%q vs %s %s\n", dl.MessageId, dl.Type, env.ID, env.Type)
			}
			d.mu.Lock()
			d.events = append(d.events, env)
			d.mu.Unlock()
		}
	}()
	if d.pub, err = d.conn.Channel(); err != nil {
		return nil, err
	}
	if err := d.pub.Confirm(false); err != nil {
		return nil, err
	}
	d.ret = d.pub.NotifyReturn(make(chan amqp.Return, 4))
	if d.invDB, err = pgx.Connect(ctx, c.inventoryDB); err != nil {
		return nil, fmt.Errorf("inventory db: %w", err)
	}
	return d, nil
}

func (d *driver) close() {
	if d.invDB != nil {
		_ = d.invDB.Close(context.Background())
	}
	if d.conn != nil {
		_ = d.conn.Close()
	}
	_ = d.broker.Close()
}

// waitReady: every service answers /readyz and every handler queue has a consumer. /readyz alone is not enough:
// it is 200 as soon as HTTP listens, before the topology is declared and the consumers subscribed (GAPS G19).
func (d *driver) waitReady(ctx context.Context) error {
	deadline := time.Now().Add(90 * time.Second)
	for _, u := range []string{d.c.inventory, d.c.payment, d.c.notification} {
		for {
			resp, err := http.Get(u + "/readyz")
			if err == nil {
				resp.Body.Close()
				if resp.StatusCode == 200 {
					break
				}
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("%s/readyz not ready: %v", u, err)
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	for _, q := range consumedQueues {
		for {
			n, err := d.consumers(ctx, q)
			if err == nil && n > 0 {
				break
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("queue %s has no consumer (err %v)", q, err)
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	return nil
}

func (d *driver) consumers(ctx context.Context, queue string) (int, error) {
	ch, err := d.broker.Channel(ctx)
	if err != nil {
		return 0, err
	}
	defer ch.Close()
	q, err := ch.QueueDeclarePassive(queue, true, false, false, false, nil)
	if err != nil {
		return 0, err
	}
	return q.Consumers, nil
}

// ─── steps ─────────────────────────────────────────────────────────────────────────────────────────────────

func (d *driver) step(name string, fn func() error) {
	t := time.Now()
	if err := fn(); err != nil {
		d.failed = append(d.failed, name+": "+err.Error())
		fmt.Printf("✗ %-62s %v\n", name, err)
		return
	}
	d.passed++
	fmt.Printf("✓ %-62s %4dms\n", name, time.Since(t).Milliseconds())
}

func (d *driver) nextOrder() int64 { d.order++; return d.order }

func (d *driver) sku(s string) string { return "E2E-" + d.run_ + "-" + s }

type item struct {
	Sku string `json:"sku"`
	Qty int32  `json:"qty"`
}

func (d *driver) run(ctx context.Context) {
	a, b := d.sku("A"), d.sku("B")
	itemsJSON := func(items ...item) []item { return items }

	// ── seed: the stock rows (no API creates them: GAPS G18), then AdjustStock (W4) through the API ──
	d.step("seed: stock rows (SQL) + AdjustStock +10 / +5 (API)", func() error {
		for _, s := range []string{a, b} {
			if _, err := d.invDB.Exec(ctx, `INSERT INTO stock (sku, available) VALUES ($1, 0)`, s); err != nil {
				return err
			}
		}
		for _, x := range []item{{a, 10}, {b, 5}} {
			var out struct{ Available int32 }
			if err := d.call(d.c.inventory, "inventory.inventory.v1.InventoryService/AdjustStock",
				map[string]any{"sku": x.Sku, "delta": x.Qty, "reason": "ADJUSTMENT_REASON_RECEIVED"}, &out); err != nil {
				return err
			}
			if out.Available != x.Qty {
				return fmt.Errorf("AdjustStock %s: available = %d, want %d", x.Sku, out.Available, x.Qty)
			}
		}
		return d.expectStock(a, 10, 0)
	})

	// ── M1 inventory.reserve ──
	o1 := d.nextOrder()
	var reserve1 message.Envelope
	d.step("M1 reserve (enough stock) → inventory.reserved", func() error {
		reserve1 = d.envelope("inventory.reserve", o1, map[string]any{"order_id": str(o1), "items": itemsJSON(item{a, 3}, item{b, 2})})
		if err := d.publish(message.Command, reserve1, nil); err != nil {
			return err
		}
		ev, err := d.await("inventory.reserved", o1)
		if err != nil {
			return err
		}
		var data struct{ Items []item }
		if err := json.Unmarshal(ev.Data, &data); err != nil {
			return err
		}
		if len(data.Items) != 2 || data.Items[0] != (item{a, 3}) || data.Items[1] != (item{b, 2}) {
			return fmt.Errorf("inventory.reserved items = %+v", data.Items)
		}
		if ev.Source != "inventory" || ev.Subject != str(o1) {
			return fmt.Errorf("envelope source %q subject %q", ev.Source, ev.Subject)
		}
		if err := d.expectStock(a, 7, 3); err != nil {
			return err
		}
		return d.expectStock(b, 3, 2)
	})

	o2 := d.nextOrder()
	d.step("M1 reserve (exceeds stock) → reservation_failed INSUFFICIENT_STOCK", func() error {
		if err := d.publish(message.Command, d.envelope("inventory.reserve", o2,
			map[string]any{"order_id": str(o2), "items": itemsJSON(item{a, 1}, item{b, 100})}), nil); err != nil {
			return err
		}
		ev, err := d.await("inventory.reservation_failed", o2)
		if err != nil {
			return err
		}
		if r := field(ev, "reason"); r != "INSUFFICIENT_STOCK" {
			return fmt.Errorf("reason = %q", r)
		}
		return d.expectStock(a, 7, 3) // all or nothing: a's 1 rolled back with b
	})

	// ── M2 inventory.commit ──
	d.step("M2 commit → inventory.committed, reserved stock leaves", func() error {
		if err := d.publish(message.Command, d.envelope("inventory.commit", o1,
			map[string]any{"order_id": str(o1), "items": itemsJSON(item{a, 3}, item{b, 2})}), nil); err != nil {
			return err
		}
		if _, err := d.await("inventory.committed", o1); err != nil {
			return err
		}
		if err := d.expectStock(a, 7, 0); err != nil {
			return err
		}
		return d.expectStock(b, 3, 0)
	})

	// ── M3 inventory.release ──
	o3 := d.nextOrder()
	d.step("M3 reserve then release → inventory.released, stock back", func() error {
		if err := d.publish(message.Command, d.envelope("inventory.reserve", o3,
			map[string]any{"order_id": str(o3), "items": itemsJSON(item{a, 2})}), nil); err != nil {
			return err
		}
		if _, err := d.await("inventory.reserved", o3); err != nil {
			return err
		}
		if err := d.expectStock(a, 5, 2); err != nil {
			return err
		}
		if err := d.publish(message.Command, d.envelope("inventory.release", o3,
			map[string]any{"order_id": str(o3), "items": itemsJSON(item{a, 2})}), nil); err != nil {
			return err
		}
		if _, err := d.await("inventory.released", o3); err != nil {
			return err
		}
		return d.expectStock(a, 7, 0)
	})

	o4 := d.nextOrder()
	d.step("M3 release before reserve (tombstone) → reserve fails RELEASED", func() error {
		if err := d.publish(message.Command, d.envelope("inventory.release", o4,
			map[string]any{"order_id": str(o4), "items": itemsJSON(item{a, 1})}), nil); err != nil {
			return err
		}
		if _, err := d.await("inventory.released", o4); err != nil {
			return err
		}
		if err := d.publish(message.Command, d.envelope("inventory.reserve", o4,
			map[string]any{"order_id": str(o4), "items": itemsJSON(item{a, 1})}), nil); err != nil {
			return err
		}
		ev, err := d.await("inventory.reservation_failed", o4)
		if err != nil {
			return err
		}
		if r := field(ev, "reason"); r != "RELEASED" {
			return fmt.Errorf("reason = %q", r)
		}
		return d.expectStock(a, 7, 0)
	})

	// ── M10 payment.charge ──
	o5, o6 := d.nextOrder(), d.nextOrder()
	var charge5 message.Envelope
	d.step("M10 charge 4500.50 → payment.succeeded", func() error {
		charge5 = d.envelope("payment.charge", o5, map[string]any{"order_id": str(o5), "amount": "4500.50"})
		if err := d.publish(message.Command, charge5, nil); err != nil {
			return err
		}
		ev, err := d.await("payment.succeeded", o5)
		if err != nil {
			return err
		}
		if amt := field(ev, "amount"); amt != "4500.50" {
			return fmt.Errorf("amount = %q", amt)
		}
		return d.expectPayment(o5, "PAYMENT_STATUS_SUCCEEDED", "")
	})
	d.step("M10 charge 100000.01 (above the limit) → payment.failed", func() error {
		if err := d.publish(message.Command, d.envelope("payment.charge", o6,
			map[string]any{"order_id": str(o6), "amount": "100000.01"}), nil); err != nil {
			return err
		}
		ev, err := d.await("payment.failed", o6)
		if err != nil {
			return err
		}
		if r := field(ev, "reason"); r != "LIMIT_EXCEEDED" {
			return fmt.Errorf("reason = %q", r)
		}
		return d.expectPayment(o6, "PAYMENT_STATUS_FAILED", "LIMIT_EXCEEDED")
	})

	// ── M11 / M12 notification ──
	var confirmed1 message.Envelope
	d.step("M11 order.confirmed → notification recorded", func() error {
		confirmed1 = d.envelope("order.confirmed", o1, map[string]any{"order_id": str(o1), "customer_id": "42", "total": "4500.50"})
		if err := d.publish(message.Event, confirmed1, nil); err != nil {
			return err
		}
		return d.expectNotifications(o1, []string{confirmed1.ID}, "NOTIFICATION_KIND_ORDER_CONFIRMED")
	})
	d.step("M12 order.cancelled → notification recorded", func() error {
		ev := d.envelope("order.cancelled", o2, map[string]any{"order_id": str(o2), "customer_id": "42"})
		if err := d.publish(message.Event, ev, nil); err != nil {
			return err
		}
		return d.expectNotifications(o2, []string{ev.ID}, "NOTIFICATION_KIND_ORDER_CANCELLED")
	})

	// ── redelivery: the same message id is processed once ──
	d.step("redeliver payment.charge (same id) → duplicate, no second reply", func() error {
		return d.redeliver(charge5, message.Command, d.c.paymentMet, "payment.payment.charge", "payment.succeeded", o5, func() error {
			return d.expectPayment(o5, "PAYMENT_STATUS_SUCCEEDED", "")
		})
	})
	d.step("redeliver inventory.reserve (same id) → duplicate, stock unchanged", func() error {
		return d.redeliver(reserve1, message.Command, d.c.inventoryMet, "inventory.inventory.reserve", "inventory.reserved", o1, func() error {
			if err := d.expectStock(a, 7, 0); err != nil {
				return err
			}
			return d.expectStock(b, 3, 0)
		})
	})
	d.step("redeliver order.confirmed (same id) → still one notification", func() error {
		return d.redeliver(confirmed1, message.Event, d.c.notificationMet, "notification.order.confirmed", "", o1, func() error {
			return d.expectNotifications(o1, []string{confirmed1.ID}, "NOTIFICATION_KIND_ORDER_CONFIRMED")
		})
	})
	d.step("re-send payment.charge (new id, same order) → first result, one payment", func() error {
		before := d.count("payment.succeeded", o5)
		if err := d.publish(message.Command, d.envelope("payment.charge", o5,
			map[string]any{"order_id": str(o5), "amount": "999999"}), nil); err != nil { // different amount: still the first answer
			return err
		}
		if err := d.awaitCount("payment.succeeded", o5, before+1); err != nil {
			return err
		}
		ev := d.last("payment.succeeded", o5)
		if amt := field(ev, "amount"); amt != "4500.50" {
			return fmt.Errorf("the re-sent charge answered amount %q, want the first charge's 4500.50", amt)
		}
		return d.expectPayment(o5, "PAYMENT_STATUS_SUCCEEDED", "")
	})

	// ── poison messages end in the DLQ ──
	d.step("poison: not a JSON envelope → inventory.reserve DLQ", func() error {
		id := message.NewID()
		if err := d.publishRaw(topology.DefaultExchanges.Commands, "inventory.reserve", id, "inventory.reserve", []byte("not json"), nil); err != nil {
			return err
		}
		return d.expectDLQ("inventory.inventory.reserve", id, "poison")
	})
	d.step("poison: contract violation (qty 0) → DLQ, no reply", func() error {
		o := d.nextOrder()
		ev := d.envelope("inventory.reserve", o, map[string]any{"order_id": str(o), "items": itemsJSON(item{a, 0})})
		if err := d.publish(message.Command, ev, nil); err != nil {
			return err
		}
		if err := d.expectDLQ("inventory.inventory.reserve", ev.ID, "poison"); err != nil {
			return err
		}
		return d.expectNone("inventory.", o)
	})
	d.step("poison: unknown SKU (stock not found) → DLQ, no reply", func() error {
		o := d.nextOrder()
		ev := d.envelope("inventory.reserve", o, map[string]any{"order_id": str(o), "items": itemsJSON(item{d.sku("NONE"), 1})})
		if err := d.publish(message.Command, ev, nil); err != nil {
			return err
		}
		if err := d.expectDLQ("inventory.inventory.reserve", ev.ID, "poison"); err != nil {
			return err
		}
		if err := d.expectNone("inventory.", o); err != nil {
			return err
		}
		return d.expectStock(a, 7, 0)
	})
	d.step("poison: unknown contract version → payment.charge DLQ", func() error {
		o := d.nextOrder()
		ev := d.envelope("payment.charge", o, map[string]any{"order_id": str(o), "amount": "1"})
		ev.Version = 2
		if err := d.publish(message.Command, ev, nil); err != nil {
			return err
		}
		if err := d.expectDLQ("payment.payment.charge", ev.ID, "poison"); err != nil {
			return err
		}
		return d.expectNone("payment.", o)
	})

	// ── invariants at the end ──
	d.step("I1 outboxes drained (every committed message published)", func() error {
		for _, u := range []string{d.c.inventoryDB, d.c.paymentDB, d.c.notificationDB} {
			n, err := pendingOutbox(ctx, u)
			if err != nil {
				return err
			}
			if n != 0 {
				return fmt.Errorf("%s: %d outbox rows not published", u, n)
			}
		}
		return nil
	})
	d.step("I6 every DLQ is empty (only the injected poison was there)", func() error {
		for _, q := range consumedQueues {
			n, err := consume.QueueDepth(ctx, d.broker, topology.DLQ(q))
			if err != nil {
				return err
			}
			if n != 0 {
				return fmt.Errorf("%s: %d messages", topology.DLQ(q), n)
			}
		}
		return nil
	})
}

// ─── messages ──────────────────────────────────────────────────────────────────────────────────────────────

func str(n int64) string { return strconv.FormatInt(n, 10) } // 整数64 in data is a JSON string (the contracts' pattern)

func (d *driver) envelope(typ string, subject int64, data map[string]any) message.Envelope {
	raw, err := json.Marshal(data)
	if err != nil {
		panic(err)
	}
	return message.Envelope{ID: message.NewID(), Type: typ, Version: 1, OccurredAt: time.Now(), Source: "e2e-driver",
		Subject: str(subject), Data: raw}
}

// publish: the way the outbox relay publishes (outbox/relay.go): exchange by kind, routing key = type, persistent,
// message_id = envelope id, type / x-factory-version in the properties; commands mandatory (unroutable = error).
func (d *driver) publish(kind message.Kind, env message.Envelope, headers amqp.Table) error {
	body, err := env.Encode()
	if err != nil {
		return err
	}
	ex, err := topology.DefaultExchanges.For(kind)
	if err != nil {
		return err
	}
	h := amqp.Table{message.HeaderVersion: int32(env.Version)}
	for k, v := range headers {
		h[k] = v
	}
	return d.publishRaw(ex, env.Type, env.ID, env.Type, body, h, kind == message.Command)
}

func (d *driver) publishRaw(exchange, key, id, typ string, body []byte, h amqp.Table, mandatory ...bool) error {
	d.pubMu.Lock()
	defer d.pubMu.Unlock()
	mand := len(mandatory) == 0 || mandatory[0]
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conf, err := d.pub.PublishWithDeferredConfirmWithContext(ctx, exchange, key, mand, false, amqp.Publishing{
		ContentType: message.ContentType, DeliveryMode: amqp.Persistent, MessageId: id, Type: typ,
		Timestamp: time.Now(), AppId: "e2e-driver", Headers: h, Body: body,
	})
	if err != nil {
		return err
	}
	ok, err := conf.WaitContext(ctx)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("broker nacked the publish")
	}
	select {
	case r := <-d.ret:
		return fmt.Errorf("unroutable: %s/%s (%d %s)", r.Exchange, r.RoutingKey, r.ReplyCode, r.ReplyText)
	default:
		return nil
	}
}

func (d *driver) matching(typ string, subject int64) []message.Envelope {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []message.Envelope
	for _, e := range d.events {
		if (e.Type == typ || strings.HasSuffix(typ, ".") && strings.HasPrefix(e.Type, typ)) && e.Subject == str(subject) {
			out = append(out, e)
		}
	}
	return out
}

func (d *driver) count(typ string, subject int64) int { return len(d.matching(typ, subject)) }

func (d *driver) last(typ string, subject int64) message.Envelope {
	m := d.matching(typ, subject)
	if len(m) == 0 {
		return message.Envelope{}
	}
	return m[len(m)-1]
}

// await: the first event of typ for subject (answers to an order are unique per type in these scenarios).
func (d *driver) await(typ string, subject int64) (message.Envelope, error) {
	if err := d.awaitCount(typ, subject, 1); err != nil {
		return message.Envelope{}, err
	}
	return d.matching(typ, subject)[0], nil
}

func (d *driver) awaitCount(typ string, subject int64, n int) error {
	deadline := time.Now().Add(d.c.wait)
	for time.Now().Before(deadline) {
		if d.count(typ, subject) >= n {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	d.mu.Lock()
	seen := make([]string, 0, len(d.events))
	for _, e := range d.events {
		if e.Subject == str(subject) {
			seen = append(seen, e.Type)
		}
	}
	d.mu.Unlock()
	return fmt.Errorf("no %d× %s for order %d within %s (seen for this order: %v)", n, typ, subject, d.c.wait, seen)
}

// expectNone: no event whose type starts with prefix for subject, during the quiet period.
func (d *driver) expectNone(prefix string, subject int64) error {
	time.Sleep(d.c.quiet)
	if m := d.matching(prefix, subject); len(m) > 0 {
		return fmt.Errorf("unexpected %s for order %d", m[0].Type, subject)
	}
	return nil
}

func field(e message.Envelope, k string) string {
	var m map[string]any
	_ = json.Unmarshal(e.Data, &m)
	if v, ok := m[k]; ok && v != nil {
		return fmt.Sprint(v)
	}
	return ""
}

// redeliver: publish the same envelope (same id) again; wait until the handler counted it as a duplicate
// (factory_messages_total{outcome="duplicate"}), then: no new reply, and the state check still holds.
func (d *driver) redeliver(env message.Envelope, kind message.Kind, metricsURL, handler, reply string, subject int64, state func() error) error {
	before, err := metric(metricsURL, "factory_messages_total", map[string]string{"handler": handler, "outcome": "duplicate"})
	if err != nil {
		return err
	}
	replies := 0
	if reply != "" {
		replies = d.count(reply, subject)
	}
	if err := d.publish(kind, env, amqp.Table{}); err != nil {
		return err
	}
	deadline := time.Now().Add(d.c.wait)
	for {
		n, err := metric(metricsURL, "factory_messages_total", map[string]string{"handler": handler, "outcome": "duplicate"})
		if err != nil {
			return err
		}
		if n >= before+1 {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s: duplicate counter stayed at %v", handler, before)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if reply != "" {
		time.Sleep(d.c.quiet) // a (wrong) second reply would go through the outbox relay: give it time to show up
		if n := d.count(reply, subject); n != replies {
			return fmt.Errorf("%s answered again: %d → %d", reply, replies, n)
		}
	}
	return state()
}

// expectDLQ: the message with this id is in <queue>.dlq with the outcome header; it is removed (acked) there.
func (d *driver) expectDLQ(queue, id, outcome string) error {
	deadline := time.Now().Add(d.c.wait)
	for time.Now().Before(deadline) {
		found, err := d.takeDLQ(queue, id)
		if err != nil {
			return err
		}
		if found != nil {
			if found.Outcome != outcome {
				return fmt.Errorf("DLQ outcome %q (error %q), want %q", found.Outcome, found.Error, outcome)
			}
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("message %s not in %s within %s", id, topology.DLQ(queue), d.c.wait)
}

// takeDLQ: consume.DLQList shows the dead letters (the operator tooling); the matching one is then taken off with
// a basic.get so the end-of-run "every DLQ is empty" check holds.
func (d *driver) takeDLQ(queue, id string) (*consume.DeadLetter, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	list, err := consume.DLQList(ctx, d.broker, queue, 100)
	if err != nil {
		return nil, err
	}
	var hit *consume.DeadLetter
	for i := range list {
		if list[i].ID == id {
			hit = &list[i]
		}
	}
	if hit == nil {
		return nil, nil
	}
	ch, err := d.conn.Channel()
	if err != nil {
		return nil, err
	}
	defer ch.Close()
	for range 100 {
		m, ok, err := ch.Get(topology.DLQ(queue), false)
		if err != nil || !ok {
			return hit, err
		}
		if m.MessageId == id {
			return hit, m.Ack(false)
		}
		_ = m.Nack(false, true)
	}
	return hit, nil
}

// ─── the services' APIs (Connect, JSON) ───────────────────────────────────────────────────────────────────

func (d *driver) call(base, procedure string, req, out any) error {
	body, _ := json.Marshal(req)
	resp, err := http.Post(base+"/"+procedure, "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return fmt.Errorf("%s: HTTP %d %s", procedure, resp.StatusCode, b)
	}
	return json.Unmarshal(b, out)
}

func (d *driver) expectStock(sku string, available, reserved int32) error {
	var out struct {
		Available int32
		Reserved  int32
		Found     bool
	}
	if err := d.call(d.c.inventory, "inventory.inventory.v1.InventoryService/GetStock", map[string]any{"sku": sku}, &out); err != nil {
		return err
	}
	if !out.Found || out.Available != available || out.Reserved != reserved {
		return fmt.Errorf("GetStock %s = available %d reserved %d (found %v), want %d / %d", sku, out.Available, out.Reserved, out.Found, available, reserved)
	}
	return nil
}

func (d *driver) expectPayment(order int64, status, reason string) error {
	var out struct {
		Status string
		Reason string
		Found  bool
	}
	if err := d.call(d.c.payment, "payment.payment.v1.PaymentService/GetPaymentByOrder", map[string]any{"order_id": str(order)}, &out); err != nil {
		return err
	}
	if !out.Found || out.Status != status || out.Reason != reason {
		return fmt.Errorf("GetPaymentByOrder %d = %+v, want %s %q", order, out, status, reason)
	}
	return nil
}

// expectNotifications: the order's notifications are exactly these event ids (waits for the consumer).
func (d *driver) expectNotifications(order int64, ids []string, kind string) error {
	var out struct {
		Notifications []struct {
			EventID string `json:"eventId"`
			Kind    string
		}
	}
	deadline := time.Now().Add(d.c.wait)
	for {
		if err := d.call(d.c.notification, "notification.notification.v1.NotificationService/ListNotificationsByOrder",
			map[string]any{"order_id": str(order)}, &out); err != nil {
			return err
		}
		if len(out.Notifications) >= len(ids) || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(out.Notifications) != len(ids) {
		return fmt.Errorf("order %d has %d notifications, want %d", order, len(out.Notifications), len(ids))
	}
	for i, n := range out.Notifications {
		if n.EventID != ids[i] || n.Kind != kind {
			return fmt.Errorf("notification %d = %s %s, want %s %s", i, n.EventID, n.Kind, ids[i], kind)
		}
	}
	return nil
}

// ─── metrics / databases ──────────────────────────────────────────────────────────────────────────────────

var sample = regexp.MustCompile(`^([a-zA-Z_:][a-zA-Z0-9_:]*)(\{[^}]*\})?\s+(\S+)$`)

// metric: the value of one sample on a Prometheus text page (0 when absent: counters start missing).
func metric(base, name string, labels map[string]string) (float64, error) {
	resp, err := http.Get(base + "/metrics")
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		m := sample.FindStringSubmatch(sc.Text())
		if m == nil || m[1] != name {
			continue
		}
		ok := true
		for k, v := range labels {
			if !strings.Contains(m[2], k+`="`+v+`"`) {
				ok = false
			}
		}
		if ok {
			return strconv.ParseFloat(m[3], 64)
		}
	}
	return 0, sc.Err()
}

func pendingOutbox(ctx context.Context, url string) (int, error) {
	deadline := time.Now().Add(10 * time.Second) // I1: bounded time once RabbitMQ is up (R4 §7.4: 10s)
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		return 0, err
	}
	defer conn.Close(ctx)
	for {
		var n int
		if err := conn.QueryRow(ctx, `SELECT count(*) FROM factory_outbox WHERE published_at IS NULL`).Scan(&n); err != nil {
			return 0, err
		}
		if n == 0 || time.Now().After(deadline) {
			return n, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
}
