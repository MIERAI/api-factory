# storefront v2: end-to-end skeleton

The generated services, built from their generated Dockerfiles, run as separate processes and talk to each
other through a real RabbitMQ. This is the hand-written layer (`docs/design/boundaries.md` §8). The factory
proves each service and the contracts. Whether the whole system works is a system test, and this directory
holds it.

> **This directory moves to the storefront repository when v2 starts there.** It lives in the factory's
> examples only because the v2 repository does not exist yet. Nothing in the factory depends on it.
> `scripts/projects.sh` finds projects by their `factory.yaml`, so it skips this directory.

## Run

```sh
make e2e          # from the repository root (or here): clean stack → driver → down -v; prints the wall time
```

| Here | |
|---|---|
| `make up` / `make driver` / `make logs` / `make down` | keep the stack up and re-run the driver; SKUs and order ids are unique per run |
| `make clean` | `down -v` and also removes the three images built here. The shared `rabbitmq` / `postgres` images stay |

Needs docker compose and Go. The images used are `rabbitmq:3.13.7-management`, `postgres:16-alpine`,
`golang:1.27.4-alpine` and `gcr.io/distroless/static-debian12:nonroot`. Compose project `sf2-e2e`. Host ports
are chosen so nothing collides with the factory's own containers:

| | host port |
|---|---|
| RabbitMQ AMQP / management UI (user `e2e`, vhost `e2e`) | 56672 / 56673 |
| Postgres (databases `inventory`, `payment`, `notification`, `order`) | 56432 |
| inventory / payment / notification / order API | 58081 / 58082 / 58083 / 58084 |
| … their `METRICS_PORT` | 59091 / 59092 / 59093 / 59094 |

Wall time on an M4 Pro, build cache warm: `make e2e` ≈ 32 s with order in the stack (stack up 11 s, driver
15 s). Most of the driver's time is the 2 s quiet periods that show "no second answer". Each image is about
20 MB.

## What is in the stack

- **RabbitMQ**, one vhost, data on tmpfs.
- **One Postgres with one database per service.** Services never share tables. Data is on tmpfs.
- **A one-shot schema loader per database** applies the project's `db/schema/*.sql` with psql, including the
  platform tables in `zz_factory_platform.sql`. The generated projects have no migration image (GAPS G24).
- **inventory, payment, notification, order**, built from their generated `Dockerfile`s, unchanged. Each one
  gets only `DATABASE_URL`, `AMQP_URL`, `AUTH_MODE=none` and `PAGE_TOKEN_KEY`. Each one declares its own
  topology, runs its outbox relay and runs its consumers; order also runs the saga's timeout scanner.
- **The driver** (`driver/`, Go) no longer plays the order service (R5, 2026-11-23): it places orders through
  order's `PlaceOrder` (W5) and the generated saga (`OrderSaga`: M4–M9, J2) drives inventory and payment. The
  driver listens to every event and to the saga's commands, injects the odd message by hand (a late answer, a
  redelivery, poison, inventory's tombstone) and checks state through the services' query APIs. It uses the
  runtime's own `message` / `topology` / `consume` packages, so envelopes, exchanges, routing keys and headers
  are exactly the ones a generated service uses.

## What it proves

Every step goes through RabbitMQ across processes (the saga and the driver publish to the exchanges, and the
services consume, write, emit through the outbox, and the relay publishes). Each step checks state through the
services' query APIs.

| Step | Checks |
|---|---|
| seed | stock rows created with SQL (no API creates them: G18), then `AdjustStock` (W4) over HTTP |
| saga, happy path (W5, M1, M4, M10, M6, M2, M8, M11) | `PlaceOrder` (3 × 1000.10 + 2 × 250) → the saga sends `inventory.reserve` (source order, the lines' SKU / qty) → `inventory.reserved` → `payment.charge` (amount = the order total 3500.30) → `payment.succeeded` → `inventory.commit` → `inventory.committed` → `order.confirmed` (customer, total); `GetOrder` = CONFIRMED, stock committed, one payment, one notification |
| saga, not enough stock (M1 failure, M5, M12) | → `reservation_failed{INSUFFICIENT_STOCK}` → CANCELLED with that reason; nothing charged; all or nothing across SKUs; notification records the cancellation |
| saga, payment refused (M10 failure, M7, M3, M9) | 100000.01 → `payment.failed{LIMIT_EXCEEDED}` → `inventory.release` → `inventory.released` → CANCELLED `{COMPENSATED}`; the stock comes back, nothing committed |
| M3 tombstone (by hand) | release **before** a reserve → tombstone, so the late reserve gets `reservation_failed{RELEASED}`. Both answers also reach the saga for an order it does not know → order's DLQ (rejected: not found) |
| redelivery, same id | payment / the saga / notification each count it as `duplicate` (`factory_messages_total`): no second reply or command, state unchanged |
| late answer, new id | `inventory.reserved` again for a confirmed order → the saga ignores it (`outcome="business"`), no second charge. `payment.charge` re-sent with a different amount → payment answers the **first** result, the saga ignores that `payment.succeeded`, still one payment and one commit |
| poison | each of these ends in `<queue>.dlq` with `x-factory-outcome: poison` and no reply: a body that is not an envelope, a contract violation (qty 0), a missing stock row, an unknown contract version, an impossible answer for the saga (`inventory.committed` for a cancelled order). Checked with the runtime's DLQ tooling (`consume.DLQList`) |
| I1 / I6 | every outbox is drained at the end, and every DLQ is empty once the injected poison is taken off |

No generated file was hand-patched. The contracts and the topology agree across the three services. Every
queue a handler consumes is bound by its owner, and every reply a handler emits matches its contract. The
friction found while building this is in `../GAPS.md` (G18–G25).

## What it does NOT prove yet

- **The saga's timeouts and outages.** The driver never waits 30 s, so no resend, no RESERVING deadline and no
  "inventory / payment down for 10 minutes, then everything finishes" (R5 §5.3, S-I1 / S-I2) run here yet. The
  saga's timeouts are proven by the factory's differential tests with the test clock (GAPS G26).
- **Crashes and broker outages across processes.** Kill -9 between commit and ack, a relay crash, and
  `docker pause rabbitmq` for 30 s are covered by the runtime's integration tests and `make accept-mq`, not
  here (R4 §7.3).
- **The retry path** (transient failure → `.retry.N` → success / exhausted). It needs `FACTORY_TEST_FAULTS=1`
  in the services, and this stack runs them as in production.
- **catalog as a container.** catalog has no messages yet (G18).
- **Concurrency and load**, for example a hot SKU with many reserves at once. That is the factory's
  differential tests (R4 §7.4) and the platform's load tests.
- **The platform.** k8s manifests, per-service database roles, TLS to RabbitMQ, the vhost per environment, and
  SLO / alerts on `factory_dlq_messages` and outbox age are all out of scope.
