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
| Postgres (databases `inventory`, `payment`, `notification`) | 56432 |
| inventory / payment / notification API | 58081 / 58082 / 58083 |
| … their `METRICS_PORT` | 59091 / 59092 / 59093 |

Wall time on an M4 Pro, build cache warm: `make e2e` ≈ 20–23 s (stack up 8–10 s, driver 10.9 s). Most of
the driver's time is the 2 s quiet periods that show "no second answer". `docker compose build --no-cache`
of the three images takes 17 s, and each image is about 20 MB.

## What is in the stack

- **RabbitMQ**, one vhost, data on tmpfs.
- **One Postgres with one database per service.** Services never share tables. Data is on tmpfs.
- **A one-shot schema loader per database** applies the project's `db/schema/*.sql` with psql, including the
  platform tables in `zz_factory_platform.sql`. The generated projects have no migration image (GAPS G24).
- **inventory, payment, notification**, built from their generated `Dockerfile`s, unchanged. Each one gets
  only `DATABASE_URL`, `AMQP_URL`, `AUTH_MODE=none` and `PAGE_TOKEN_KEY`. Each one declares its own topology,
  runs its outbox relay and runs its consumers.
- **The driver** (`driver/`, Go) plays the order service. Order's saga is R5, so no generated service drives
  the flow. The driver uses the runtime's own `message` / `topology` / `consume` packages, so envelopes,
  exchanges, routing keys and headers are exactly the ones a generated service uses.

## What it proves

All 20 steps go through RabbitMQ across processes (the driver publishes to the exchanges, and the services
consume, write, emit through the outbox, and the relay publishes). Each step checks state through the
services' query APIs.

| Step | Checks |
|---|---|
| seed | stock rows created with SQL (no API creates them: G18), then `AdjustStock` (W4) over HTTP |
| M1 `inventory.reserve` | enough stock → `inventory.reserved` (items echoed; source / subject right) with available / reserved moved. Exceeding → `inventory.reservation_failed{INSUFFICIENT_STOCK}`, with all or nothing across SKUs |
| M2 `inventory.commit` | → `inventory.committed`, and the reserved quantity leaves |
| M3 `inventory.release` | after a reserve → `inventory.released`, and the stock comes back. **Before** a reserve → tombstone, so the late reserve gets `reservation_failed{RELEASED}` |
| M10 `payment.charge` | 4500.50 → `payment.succeeded`. 100000.01 → `payment.failed{LIMIT_EXCEEDED}`. Both are checked through `GetPaymentByOrder` |
| M11 / M12 `order.confirmed` / `order.cancelled` | notification records one row per event (event id = envelope id), seen through `ListNotificationsByOrder` |
| redelivery, same id | payment / inventory / notification each count it as `duplicate` (`factory_messages_total`): no second reply and state unchanged |
| re-send, new id | `payment.charge` for the same order with a different amount answers the **first** result, and there is still one payment |
| poison | each of these ends in `<queue>.dlq` with `x-factory-outcome: poison` and no reply: a body that is not an envelope, a contract violation (qty 0), a missing stock row, an unknown contract version. Checked with the runtime's DLQ tooling (`consume.DLQList`) |
| I1 / I6 | every outbox is drained at the end, and every DLQ is empty once the injected poison is taken off |

No generated file was hand-patched. The contracts and the topology agree across the three services. Every
queue a handler consumes is bound by its owner, and every reply a handler emits matches its contract. The
friction found while building this is in `../GAPS.md` (G18–G25).

## What it does NOT prove yet

- **The order saga (R5).** Nothing here decides "reserved + paid → commit → confirmed" or compensation. The
  driver sends each command by hand and checks each answer separately. W5 and M4–M9 are not exercised, so
  timeouts, stuck orders and the saga's 60-second budget are not either.
- **Crashes and broker outages across processes.** Kill -9 between commit and ack, a relay crash, and
  `docker pause rabbitmq` for 30 s are covered by the runtime's integration tests and `make accept-mq`, not
  here (R4 §7.3).
- **The retry path** (transient failure → `.retry.N` → success / exhausted). It needs `FACTORY_TEST_FAULTS=1`
  in the services, and this stack runs them as in production.
- **catalog and order as containers.** catalog has no messages yet (G18). Only order's queries exist.
- **Concurrency and load**, for example a hot SKU with many reserves at once. That is the factory's
  differential tests (R4 §7.4) and the platform's load tests.
- **The platform.** k8s manifests, per-service database roles, TLS to RabbitMQ, the vhost per environment, and
  SLO / alerts on `factory_dlq_messages` and outbox age are all out of scope.
