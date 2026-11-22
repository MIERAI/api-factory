-- storefront v2 · order (订单). This service's own database.
-- The order saga's state lives on the order row: PENDING → RESERVING (stock) → CHARGING (payment) → CONFIRMING
-- (commit the stock) → CONFIRMED; a failure goes RELEASING (give the stock back) → CANCELLED. Every transition
-- updates updated_at (R3–R5 write it).
CREATE TABLE IF NOT EXISTS orders (
    id          BIGINT      PRIMARY KEY,
    customer_id BIGINT      NOT NULL,
    status      INTEGER     NOT NULL CHECK (status BETWEEN 1 AND 7),
    total       NUMERIC     NOT NULL CHECK (total >= 0),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now() CHECK (isfinite(created_at)),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now() CHECK (isfinite(updated_at))
);
-- Stuck orders (Q7): orders waiting in a saga step, oldest transition first
CREATE INDEX IF NOT EXISTS orders_waiting_idx ON orders (updated_at, id) WHERE status IN (2, 3, 4, 5);
CREATE INDEX IF NOT EXISTS orders_customer_idx ON orders (customer_id, created_at DESC, id DESC);
