-- storefront v2 · order (订单). This service's own database: orders and their lines. Meaning: design/order/领域.yaml
-- The order saga's state lives on the order row: PENDING → RESERVING (stock) → CHARGING (payment) → CONFIRMING
-- (commit the stock) → CONFIRMED; a failure goes RELEASING (give the stock back) → CANCELLED. Every transition
-- updates updated_at (R3–R5 write it).
CREATE TABLE IF NOT EXISTS orders (
    id          BIGINT      NOT NULL PRIMARY KEY CHECK (id > 0),
    customer_id BIGINT      NOT NULL CHECK (customer_id > 0),
    status      INTEGER     NOT NULL DEFAULT 1 CHECK (status IN (1, 2, 3, 4, 5, 6, 7)),
    total       NUMERIC     NOT NULL DEFAULT 0 CHECK (total >= 0),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now() CHECK (isfinite(created_at)),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now() CHECK (isfinite(updated_at))
);
-- a customer's order history (newest first: the paging order)
CREATE INDEX IF NOT EXISTS orders_customer_history_idx ON orders (customer_id, created_at DESC, id DESC);
-- stuck orders (Q7): orders waiting in a saga step, oldest transition first
CREATE INDEX IF NOT EXISTS orders_waiting_idx ON orders (updated_at, id) WHERE status IN (2, 3, 4, 5);

-- order lines: line numbers start at 1 within an order. amount = qty × price (computed by the database)
CREATE TABLE IF NOT EXISTS order_items (
    order_id BIGINT  NOT NULL REFERENCES orders (id),
    line_no  INTEGER NOT NULL CHECK (line_no > 0),
    sku      TEXT    NOT NULL CHECK (sku ~ '^[A-Z0-9-]{3,32}$'),
    qty      INTEGER NOT NULL CHECK (qty > 0),
    price    NUMERIC NOT NULL CHECK (price >= 0),
    amount   NUMERIC NOT NULL GENERATED ALWAYS AS (qty * price) STORED,
    PRIMARY KEY (order_id, line_no)
);
