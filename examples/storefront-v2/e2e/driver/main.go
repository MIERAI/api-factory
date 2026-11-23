// Command driver runs the storefront v2 system end to end against the running e2e stack (compose.yaml): products are
// created through catalog (W1, which emits catalog.product_created; inventory's InitStock creates the stock row: M13),
// orders are placed through the generated order service (W5 PlaceOrder) and the generated order saga (R5: OrderSaga)
// drives inventory and payment through the real RabbitMQ. The driver only creates products, places orders, injects
// the odd message (a late answer, a redelivery, poison) and checks what the services sent (read from each service's
// outbox: published_at set = the relay got the broker's confirm) and store (their query APIs).
//
// Envelopes, exchanges, routing keys and headers are the runtime's (runtime/message, runtime/topology): the
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

	"github.com/MIERAI/api-factory/runtime/broker"
	"github.com/MIERAI/api-factory/runtime/consume"
	"github.com/MIERAI/api-factory/runtime/message"
	"github.com/MIERAI/api-factory/runtime/topology"
)

type cfg struct {
	amqp                                                            string
	catalog, inventory, payment, notification, order                string // API base URLs
	catalogMet, inventoryMet, paymentMet, notificationMet, orderMet string // metrics base URLs (METRICS_PORT)
	catalogDB, inventoryDB, paymentDB, notificationDB, orderDB      string
	wait                                                            time.Duration // how long to wait for an answer
	quiet                                                           time.Duration // how long "no answer" must hold
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
		catalog:         env("E2E_CATALOG_URL", "http://127.0.0.1:58085"),
		catalogMet:      env("E2E_CATALOG_METRICS", "http://127.0.0.1:59095"),
		catalogDB:       db + "/catalog?sslmode=disable",
		inventory:       env("E2E_INVENTORY_URL", "http://127.0.0.1:58081"),
		payment:         env("E2E_PAYMENT_URL", "http://127.0.0.1:58082"),
		notification:    env("E2E_NOTIFICATION_URL", "http://127.0.0.1:58083"),
		order:           env("E2E_ORDER_URL", "http://127.0.0.1:58084"),
		inventoryMet:    env("E2E_INVENTORY_METRICS", "http://127.0.0.1:59091"),
		paymentMet:      env("E2E_PAYMENT_METRICS", "http://127.0.0.1:59092"),
		notificationMet: env("E2E_NOTIFICATION_METRICS", "http://127.0.0.1:59093"),
		orderMet:        env("E2E_ORDER_METRICS", "http://127.0.0.1:59094"),
		inventoryDB:     db + "/inventory?sslmode=disable",
		paymentDB:       db + "/payment?sslmode=disable",
		notificationDB:  db + "/notification?sslmode=disable",
		orderDB:         db + "/order?sslmode=disable",
		wait:            15 * time.Second,
		quiet:           2 * time.Second,
	}
}

// The queues the generated wiring declares (cmd/server/wiring_gen.go of each project; the system's topology.json, applied
// before the services start, has the same): <service>.<message type>.
var consumedQueues = []string{
	"inventory.catalog.product_created",
	"inventory.inventory.reserve", "inventory.inventory.commit", "inventory.inventory.release",
	"payment.payment.charge",
	"notification.order.confirmed", "notification.order.cancelled",
	"order.inventory.reserved", "order.inventory.reservation_failed", "order.inventory.committed", "order.inventory.released",
	"order.payment.succeeded", "order.payment.failed",
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

	outboxes []*pgx.Conn // one per service database (watchOutboxes)

	mu     sync.Mutex
	events []message.Envelope // every message the services sent (their outboxes, once published) + the driver's own

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
	// what the services sent: each service's outbox (published rows, in seq order). No private "#" queue on the
	// exchanges any more (GAPS G23: the topology job declares every queue before the services start, so nothing the
	// services send is lost, and the driver does not have to be bound first to see it)
	for _, u := range []string{c.catalogDB, c.inventoryDB, c.paymentDB, c.notificationDB, c.orderDB} {
		conn, err := pgx.Connect(ctx, u)
		if err != nil {
			return nil, fmt.Errorf("outbox %s: %w", u, err)
		}
		d.outboxes = append(d.outboxes, conn)
	}
	go d.watchOutboxes(ctx)
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
	d.mu.Lock() // the watcher holds the lock while it reads
	for _, c := range d.outboxes {
		_ = c.Close(context.Background())
	}
	d.outboxes = nil
	d.mu.Unlock()
	if d.conn != nil {
		_ = d.conn.Close()
	}
	_ = d.broker.Close()
}

// waitReady: every service's /readyz is 200. Since R4.1 that means the topology is declared and every consumer is
// subscribed (GAPS G19; `docker compose up --wait` already waited for it through the image's HEALTHCHECK — this is for
// `make driver` against a stack that is still starting).
func (d *driver) waitReady(ctx context.Context) error {
	deadline := time.Now().Add(90 * time.Second)
	for _, u := range []string{d.c.catalog, d.c.inventory, d.c.payment, d.c.notification, d.c.order} {
		for {
			resp, err := http.Get(u + "/readyz")
			status := 0
			if err == nil {
				status = resp.StatusCode
				resp.Body.Close()
				if status == 200 {
					break
				}
			}
			if time.Now().After(deadline) || ctx.Err() != nil {
				return fmt.Errorf("%s/readyz not ready: %d %v", u, status, err)
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	return nil
}

// watchOutboxes: every 50 ms, the newly published rows of every service's outbox → d.events (in the order the relay
// marked them: published_at, then seq; a row published late — a retried command — is still seen).
func (d *driver) watchOutboxes(ctx context.Context) {
	since := make([]time.Time, len(d.outboxes))
	seen := make([]map[int64]bool, len(d.outboxes))
	for i := range seen {
		seen[i] = map[int64]bool{}
	}
	for ctx.Err() == nil {
		d.mu.Lock()
		if d.outboxes == nil {
			d.mu.Unlock()
			return
		}
		for i, conn := range d.outboxes {
			rows, err := conn.Query(ctx, `SELECT seq, published_at, envelope::text FROM factory_outbox
WHERE published_at >= $1 ORDER BY published_at, seq`, since[i])
			if err != nil {
				continue
			}
			for rows.Next() {
				var seq int64
				var at time.Time
				var body string
				if rows.Scan(&seq, &at, &body) != nil || seen[i][seq] {
					continue
				}
				seen[i][seq], since[i] = true, at
				env, err := message.Decode([]byte(body))
				if err != nil {
					fmt.Printf("  ! undecodable outbox row %d: %v\n", seq, err)
					continue
				}
				d.events = append(d.events, env)
			}
			rows.Close()
		}
		d.mu.Unlock()
		time.Sleep(50 * time.Millisecond)
	}
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

	// ── seed: CreateProduct (W1) → catalog.product_created → InitStock (M13) creates the stock row → AdjustStock (W4) ──
	d.step("seed: CreateProduct → product_created → InitStock → AdjustStock +10 / +5", func() error {
		for _, s := range []string{a, b} {
			var out struct{ Sku, Status string }
			if err := d.call(d.c.catalog, "catalog.catalog.v1.CatalogService/CreateProduct",
				map[string]any{"sku": s, "name": "e2e " + s, "category": "e2e", "price": "1"}, &out); err != nil {
				return err
			}
			if out.Sku != s || out.Status != "PRODUCT_STATUS_ON_SALE" {
				return fmt.Errorf("CreateProduct %s → %+v", s, out)
			}
			ev, err := d.awaitSubject("catalog.product_created", s)
			if err != nil {
				return err
			}
			if ev.Source != "catalog" || field(ev, "sku") != s {
				return fmt.Errorf("catalog.product_created from %q sku %q", ev.Source, field(ev, "sku"))
			}
			if err := d.eventually(func() error { return d.expectStock(s, 0, 0) }); err != nil {
				return fmt.Errorf("InitStock: %w", err)
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
	d.step("M13 product_created again (new id) → stock row unchanged", func() error {
		h := map[string]string{"handler": "inventory.catalog.product_created", "outcome": "ok"}
		before, err := metric(d.c.inventoryMet, "factory_messages_total", h)
		if err != nil {
			return err
		}
		raw, _ := json.Marshal(map[string]any{"sku": a})
		ev := message.Envelope{ID: message.NewID(), Type: "catalog.product_created", Version: 1, OccurredAt: time.Now(),
			Source: "e2e-driver", Subject: a, Data: raw}
		if err := d.publish(message.Event, ev, nil); err != nil {
			return err
		}
		if err := d.awaitMetric(d.c.inventoryMet, "factory_messages_total", h, before+1); err != nil {
			return err
		}
		return d.expectStock(a, 10, 0)
	})
	d.step("CreateProduct with an existing SKU → ALREADY_EXISTS, no event", func() error {
		var out struct{}
		err := d.call(d.c.catalog, "catalog.catalog.v1.CatalogService/CreateProduct",
			map[string]any{"sku": a, "name": "again", "category": "e2e", "price": "2"}, &out)
		if err == nil || !strings.Contains(err.Error(), "already_exists") {
			return fmt.Errorf("want already_exists, got %v", err)
		}
		time.Sleep(d.c.quiet)
		n := 0
		for _, e := range d.matchingSubject("catalog.product_created", a) {
			if e.Source == "catalog" {
				n++
			}
		}
		if n != 1 {
			return fmt.Errorf("catalog sent %d catalog.product_created for %s, want 1", n, a)
		}
		return nil
	})

	// ── the saga, happy path: PlaceOrder → reserve → charge → commit → CONFIRMED (M1, M10, M2, M4, M6, M8, M11) ──
	var o1 int64
	var confirmed1, charge1, reserved1 message.Envelope
	d.step("W5 + saga: enough stock, paid → CONFIRMED", func() error {
		var err error
		if o1, err = d.placeOrder(line{1, a, 3, "1000.10"}, line{2, b, 2, "250"}); err != nil {
			return err
		}
		if err := d.expectOrder(o1, "ORDER_STATUS_CONFIRMED"); err != nil {
			return err
		}
		reserve, err := d.await("inventory.reserve", o1)
		if err != nil {
			return err
		}
		var data struct{ Items []item }
		if err := json.Unmarshal(reserve.Data, &data); err != nil {
			return err
		}
		if reserve.Source != "order" || len(data.Items) != 2 || data.Items[0] != (item{a, 3}) || data.Items[1] != (item{b, 2}) {
			return fmt.Errorf("inventory.reserve from %q items %+v", reserve.Source, data.Items)
		}
		if reserved1, err = d.await("inventory.reserved", o1); err != nil {
			return err
		}
		if charge1, err = d.await("payment.charge", o1); err != nil {
			return err
		}
		if amt := field(charge1, "amount"); amt != "3500.3" && amt != "3500.30" {
			return fmt.Errorf("payment.charge amount %q, want the order total 3 × 1000.10 + 2 × 250 = 3500.30", amt)
		}
		for _, t := range []string{"payment.succeeded", "inventory.commit", "inventory.committed"} {
			if _, err := d.await(t, o1); err != nil {
				return err
			}
		}
		if confirmed1, err = d.await("order.confirmed", o1); err != nil {
			return err
		}
		if c := field(confirmed1, "customer_id"); c != "42" {
			return fmt.Errorf("order.confirmed customer_id %q", c)
		}
		if err := d.expectStock(a, 7, 0); err != nil {
			return err
		}
		if err := d.expectStock(b, 3, 0); err != nil {
			return err
		}
		if err := d.expectPayment(o1, "PAYMENT_STATUS_SUCCEEDED", ""); err != nil {
			return err
		}
		return d.expectNotifications(o1, []string{confirmed1.ID}, "NOTIFICATION_KIND_ORDER_CONFIRMED")
	})

	// ── not enough stock: reserve fails → CANCELLED (M1 failure, M5, M12) ──
	d.step("W5 + saga: not enough stock → reservation_failed → CANCELLED", func() error {
		o, err := d.placeOrder(line{1, a, 1, "10"}, line{2, b, 100, "10"})
		if err != nil {
			return err
		}
		if err := d.expectOrder(o, "ORDER_STATUS_CANCELLED"); err != nil {
			return err
		}
		ev, err := d.await("order.cancelled", o)
		if err != nil {
			return err
		}
		if r := field(ev, "reason"); r != "INSUFFICIENT_STOCK" {
			return fmt.Errorf("order.cancelled reason %q", r)
		}
		if n := d.count("payment.charge", o); n != 0 {
			return fmt.Errorf("charged a cancelled order (%d payment.charge)", n)
		}
		if err := d.expectStock(a, 7, 0); err != nil { // all or nothing: a's 1 rolled back with b
			return err
		}
		return d.expectNotifications(o, []string{ev.ID}, "NOTIFICATION_KIND_ORDER_CANCELLED")
	})

	// ── payment refused: charge fails → release the stock → CANCELLED (M10 failure, M7, M3, M9) ──
	d.step("W5 + saga: above the payment limit → payment.failed → release → CANCELLED", func() error {
		o, err := d.placeOrder(line{1, a, 1, "100000.01"})
		if err != nil {
			return err
		}
		if err := d.expectOrder(o, "ORDER_STATUS_CANCELLED"); err != nil {
			return err
		}
		for _, t := range []string{"inventory.reserved", "payment.failed", "inventory.release", "inventory.released"} {
			if _, err := d.await(t, o); err != nil {
				return err
			}
		}
		ev, err := d.await("order.cancelled", o)
		if err != nil {
			return err
		}
		if r := field(ev, "reason"); r != "COMPENSATED" {
			return fmt.Errorf("order.cancelled reason %q", r)
		}
		if n := d.count("inventory.commit", o); n != 0 {
			return fmt.Errorf("committed the stock of a cancelled order")
		}
		if err := d.expectPayment(o, "PAYMENT_STATUS_FAILED", "LIMIT_EXCEEDED"); err != nil {
			return err
		}
		return d.expectStock(a, 7, 0) // released: back where it was
	})

	// ── inventory's tombstone, driven by hand (the saga reaches it only after RESERVING's 15-minute deadline):
	// the answers reach the order saga too, for an order it does not know → its DLQ (rejected: not found) ──
	o4 := d.nextOrder()
	d.step("M3 release before reserve (tombstone) → reserve fails RELEASED", func() error {
		rel := d.envelope("inventory.release", o4, map[string]any{"order_id": str(o4), "items": itemsJSON(item{a, 1})})
		if err := d.publish(message.Command, rel, nil); err != nil {
			return err
		}
		released, err := d.await("inventory.released", o4)
		if err != nil {
			return err
		}
		if err := d.publish(message.Command, d.envelope("inventory.reserve", o4,
			map[string]any{"order_id": str(o4), "items": itemsJSON(item{a, 1})}), nil); err != nil {
			return err
		}
		failed, err := d.await("inventory.reservation_failed", o4)
		if err != nil {
			return err
		}
		if r := field(failed, "reason"); r != "RELEASED" {
			return fmt.Errorf("reason = %q", r)
		}
		if err := d.expectStock(a, 7, 0); err != nil {
			return err
		}
		if err := d.expectDLQ("order.inventory.released", released.ID, "poison"); err != nil {
			return fmt.Errorf("the saga on an unknown order: %w", err)
		}
		return d.expectDLQ("order.inventory.reservation_failed", failed.ID, "poison")
	})

	// ── redelivery: the same message id is processed once ──
	d.step("redeliver payment.charge (same id) → duplicate, no second reply", func() error {
		return d.redeliver(charge1, message.Command, d.c.paymentMet, "payment.payment.charge", "payment.succeeded", o1, func() error {
			return d.expectPayment(o1, "PAYMENT_STATUS_SUCCEEDED", "")
		})
	})
	d.step("redeliver inventory.reserved (same id) → saga: duplicate, no second command", func() error {
		return d.redeliver(reserved1, message.Event, d.c.orderMet, "order.inventory.reserved", "payment.charge", o1, func() error {
			return d.expectOrder(o1, "ORDER_STATUS_CONFIRMED")
		})
	})
	d.step("redeliver order.confirmed (same id) → still one notification", func() error {
		return d.redeliver(confirmed1, message.Event, d.c.notificationMet, "notification.order.confirmed", "", o1, func() error {
			return d.expectNotifications(o1, []string{confirmed1.ID}, "NOTIFICATION_KIND_ORDER_CONFIRMED")
		})
	})
	// ── late answers: the same content with a new id (a resend's second answer) is ignored by the saga ──
	d.step("late inventory.reserved (new id) for a confirmed order → ignored", func() error {
		before, err := metric(d.c.orderMet, "factory_messages_total", map[string]string{"handler": "order.inventory.reserved", "outcome": "business"})
		if err != nil {
			return err
		}
		charges := d.count("payment.charge", o1)
		late := d.envelope("inventory.reserved", o1, map[string]any{"order_id": str(o1), "items": itemsJSON(item{a, 3}, item{b, 2})})
		if err := d.publish(message.Event, late, nil); err != nil {
			return err
		}
		if err := d.awaitMetric(d.c.orderMet, "factory_messages_total", map[string]string{"handler": "order.inventory.reserved", "outcome": "business"}, before+1); err != nil {
			return err
		}
		time.Sleep(d.c.quiet)
		if n := d.count("payment.charge", o1); n != charges {
			return fmt.Errorf("the late answer charged again: %d → %d", charges, n)
		}
		return d.expectOrder(o1, "ORDER_STATUS_CONFIRMED")
	})
	d.step("re-send payment.charge (new id, same order) → first result, saga ignores it", func() error {
		before := d.count("payment.succeeded", o1)
		if err := d.publish(message.Command, d.envelope("payment.charge", o1,
			map[string]any{"order_id": str(o1), "amount": "999999"}), nil); err != nil { // different amount: still the first answer
			return err
		}
		if err := d.awaitCount("payment.succeeded", o1, before+1); err != nil {
			return err
		}
		ev := d.last("payment.succeeded", o1)
		if amt := field(ev, "amount"); amt != field(charge1, "amount") {
			return fmt.Errorf("the re-sent charge answered amount %q, want the first charge's %s", amt, field(charge1, "amount"))
		}
		if err := d.expectPayment(o1, "PAYMENT_STATUS_SUCCEEDED", ""); err != nil {
			return err
		}
		time.Sleep(d.c.quiet)
		if n := d.count("inventory.commit", o1); n != 1 {
			return fmt.Errorf("the late payment.succeeded made the saga commit again (%d inventory.commit)", n)
		}
		return d.expectOrder(o1, "ORDER_STATUS_CONFIRMED")
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
	d.step("saga: an impossible answer (inventory.committed for a cancelled order) → DLQ", func() error {
		// inventory.committed can only follow CONFIRMING; a cancelled order never was there → rejected (impossible)
		o, err := d.placeOrder(line{1, a, 1, "10"}, line{2, b, 100, "10"})
		if err != nil {
			return err
		}
		if err := d.expectOrder(o, "ORDER_STATUS_CANCELLED"); err != nil {
			return err
		}
		ev := d.envelope("inventory.committed", o, map[string]any{"order_id": str(o)})
		if err := d.publish(message.Event, ev, nil); err != nil {
			return err
		}
		if err := d.expectDLQ("order.inventory.committed", ev.ID, "poison"); err != nil {
			return err
		}
		return d.expectOrder(o, "ORDER_STATUS_CANCELLED")
	})

	// ── invariants at the end ──
	d.step("I1 outboxes drained (every committed message published)", func() error {
		for _, u := range []string{d.c.catalogDB, d.c.inventoryDB, d.c.paymentDB, d.c.notificationDB, d.c.orderDB} {
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
	d.step("G23 nothing unroutable (factory_outbox_unroutable_total absent on every service)", func() error {
		for _, u := range []string{d.c.catalogMet, d.c.inventoryMet, d.c.paymentMet, d.c.notificationMet, d.c.orderMet} {
			n, err := metric(u, "factory_outbox_unroutable_total", nil)
			if err != nil {
				return err
			}
			if n != 0 {
				return fmt.Errorf("%s: a message had no queue (%v)", u, n)
			}
		}
		return nil
	})
	d.step("S-I3 at most one payment per order; CONFIRMED ⇔ paid", func() error {
		return d.expectPayment(o1, "PAYMENT_STATUS_SUCCEEDED", "")
	})
}

// ─── the order service (W5 + the saga) ─────────────────────────────────────────────────────────────────────

type line struct {
	no    int32
	sku   string
	qty   int32
	price string
}

// placeOrder: W5 PlaceOrder (customer 42). Returns the order id the service generated.
func (d *driver) placeOrder(lines ...line) (int64, error) {
	var items []map[string]any
	for _, l := range lines {
		items = append(items, map[string]any{"line_no": l.no, "sku": l.sku, "qty": l.qty, "price": l.price})
	}
	var out struct {
		OrderID string `json:"orderId"`
		Status  string
	}
	if err := d.call(d.c.order, "order.order.v1.OrderService/PlaceOrder", map[string]any{"customer_id": "42", "items": items}, &out); err != nil {
		return 0, err
	}
	if out.Status != "ORDER_STATUS_RESERVING" {
		return 0, fmt.Errorf("PlaceOrder status %q, want ORDER_STATUS_RESERVING", out.Status)
	}
	return strconv.ParseInt(out.OrderID, 10, 64)
}

// expectOrder: GetOrder reaches this status within the wait (the saga is asynchronous).
func (d *driver) expectOrder(order int64, status string) error {
	var out struct{ Status string }
	deadline := time.Now().Add(d.c.wait)
	for {
		if err := d.call(d.c.order, "order.order.v1.OrderService/GetOrder", map[string]any{"id": str(order)}, &out); err != nil {
			return err
		}
		if out.Status == status {
			return nil
		}
		if time.Now().After(deadline) {
			d.mu.Lock()
			var seen []string
			for _, e := range d.events {
				if e.Subject == str(order) {
					seen = append(seen, e.Type)
				}
			}
			d.mu.Unlock()
			return fmt.Errorf("order %d is %s, want %s within %s (messages seen: %v)", order, out.Status, status, d.c.wait, seen)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (d *driver) awaitMetric(base, name string, labels map[string]string, want float64) error {
	deadline := time.Now().Add(d.c.wait)
	for {
		n, err := metric(base, name, labels)
		if err != nil {
			return err
		}
		if n >= want {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s%v stayed at %v (want %v)", name, labels, n, want)
		}
		time.Sleep(50 * time.Millisecond)
	}
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
// message_id = envelope id, type / x-factory-version in the properties, mandatory (unroutable = error).
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
	// mandatory for both kinds, like the relay since R4.1: every queue is declared by the topology job
	if err := d.publishRaw(ex, env.Type, env.ID, env.Type, body, h, true); err != nil {
		return err
	}
	d.mu.Lock()
	d.events = append(d.events, env) // the driver's own messages count as seen (expectNone skips source e2e-driver)
	d.mu.Unlock()
	return nil
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
	return d.matchingSubject(typ, str(subject))
}

func (d *driver) matchingSubject(typ, subject string) []message.Envelope {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []message.Envelope
	for _, e := range d.events {
		if (e.Type == typ || strings.HasSuffix(typ, ".") && strings.HasPrefix(e.Type, typ)) && e.Subject == subject {
			out = append(out, e)
		}
	}
	return out
}

// awaitSubject: the first message of typ for a non-numeric subject (catalog: the SKU).
func (d *driver) awaitSubject(typ, subject string) (message.Envelope, error) {
	deadline := time.Now().Add(d.c.wait)
	for time.Now().Before(deadline) {
		if m := d.matchingSubject(typ, subject); len(m) > 0 {
			return m[0], nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return message.Envelope{}, fmt.Errorf("no %s for %s within %s", typ, subject, d.c.wait)
}

// eventually: check until it passes or the wait is over (asynchronous handlers).
func (d *driver) eventually(check func() error) error {
	deadline := time.Now().Add(d.c.wait)
	for {
		err := check()
		if err == nil || time.Now().After(deadline) {
			return err
		}
		time.Sleep(50 * time.Millisecond)
	}
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

// expectNone: no message (from a service: the driver's own commands are heard too) whose type starts with prefix
// for subject, during the quiet period.
func (d *driver) expectNone(prefix string, subject int64) error {
	time.Sleep(d.c.quiet)
	for _, m := range d.matching(prefix, subject) {
		if m.Source != "e2e-driver" {
			return fmt.Errorf("unexpected %s from %s for order %d", m.Type, m.Source, subject)
		}
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
