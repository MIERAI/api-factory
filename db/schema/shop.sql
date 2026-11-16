-- ============================================================================
-- shop 领域(批量验证 第一波)。含义见 specs/shop/_domain.yaml
-- 金额一律用最小货币单位的整数(円)
-- ============================================================================
CREATE TABLE IF NOT EXISTS customers (
    id         BIGINT      PRIMARY KEY,
    name       TEXT        NOT NULL,
    email      TEXT        NOT NULL,
    tier       INTEGER     NOT NULL CHECK (tier IN (0, 1, 2)),
    created_at TIMESTAMPTZ NOT NULL CHECK (isfinite(created_at))
);

CREATE TABLE IF NOT EXISTS products (
    id         BIGINT      PRIMARY KEY,
    sku        TEXT        NOT NULL UNIQUE CHECK (char_length(sku) <= 32),
    name       TEXT        NOT NULL,
    category   TEXT        NOT NULL,
    status     INTEGER     NOT NULL CHECK (status IN (0, 1, 2)),
    created_at TIMESTAMPTZ NOT NULL CHECK (isfinite(created_at))
);
CREATE INDEX IF NOT EXISTS products_created_at_id_idx ON products (created_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS products_name_prefix_idx ON products (name text_pattern_ops);  -- 前缀匹配用

-- 价格历史: 只追加。某时点的价格 = valid_from <= 该时点 的最新一行
CREATE TABLE IF NOT EXISTS product_prices (
    product_id BIGINT      NOT NULL,
    valid_from TIMESTAMPTZ NOT NULL CHECK (isfinite(valid_from)),
    price      BIGINT      NOT NULL CHECK (price >= 0),
    currency   TEXT        NOT NULL CHECK (currency = 'JPY'),  -- 本店只用日元;多币种时再放开
    PRIMARY KEY (product_id, valid_from)
);

CREATE TABLE IF NOT EXISTS orders (
    id           BIGINT      PRIMARY KEY,
    customer_id  BIGINT      NOT NULL,
    status       INTEGER     NOT NULL CHECK (status IN (0, 1, 2, 3)),
    ordered_at   TIMESTAMPTZ NOT NULL CHECK (isfinite(ordered_at)),
    shipped_at   TIMESTAMPTZ          CHECK (shipped_at IS NULL OR isfinite(shipped_at)),
    total_amount BIGINT      NOT NULL CHECK (total_amount >= 0)
);
CREATE INDEX IF NOT EXISTS orders_ordered_at_id_idx ON orders (ordered_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS orders_customer_id_idx ON orders (customer_id, ordered_at);

CREATE TABLE IF NOT EXISTS order_items (
    order_id   BIGINT  NOT NULL,
    line_no    INTEGER NOT NULL CHECK (line_no > 0),
    product_id BIGINT  NOT NULL,
    quantity   INTEGER NOT NULL CHECK (quantity > 0),
    unit_price BIGINT  NOT NULL CHECK (unit_price >= 0),
    PRIMARY KEY (order_id, line_no)
);
