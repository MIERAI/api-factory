-- order 服务独占的数据库: 订单和订单明细。含义见 design/order/领域.yaml
CREATE TABLE IF NOT EXISTS orders (
    id          BIGINT      NOT NULL PRIMARY KEY CHECK (id > 0),
    customer_id BIGINT      NOT NULL CHECK (customer_id > 0),
    status      INTEGER     NOT NULL DEFAULT 1 CHECK (status IN (1, 2, 3, 4, 5, 6, 7)),
    total       NUMERIC     NOT NULL DEFAULT 0 CHECK (total >= 0),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- 顾客的订单历史(新到旧,分页的顺序)
CREATE INDEX IF NOT EXISTS orders_customer_history_idx ON orders (customer_id, created_at DESC, id DESC);

-- 订单明细: 行号在订单内从 1 开始。金额 = 数量 × 单价(数据库计算,写入方不能写错)
CREATE TABLE IF NOT EXISTS order_items (
    order_id BIGINT  NOT NULL REFERENCES orders (id),
    line_no  INTEGER NOT NULL CHECK (line_no > 0),
    sku      TEXT    NOT NULL CHECK (sku ~ '^[A-Z0-9-]{3,32}$'),
    qty      INTEGER NOT NULL CHECK (qty > 0),
    price    NUMERIC NOT NULL CHECK (price >= 0),
    amount   NUMERIC NOT NULL GENERATED ALWAYS AS (qty * price) STORED,
    PRIMARY KEY (order_id, line_no)
);
