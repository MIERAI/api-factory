-- Baseline: the schema as it was before versioned migrations (db/schema/{catalog,jobs,sales,shop}.sql
-- on 2026-11-19). IF NOT EXISTS: databases created by the old idempotent schema-apply adopt it as version 1.
-- From here on: every change is a NEW numbered file here AND the same change in db/schema (the factory reads that);
-- CI checks that both produce the same schema (scripts/ci/schema-drift.sh).

-- +goose Up
-- 平台管理的 DB 结构(人工维护,AI 不可修改)
-- 追加型表: 每次变更追加一行,字段在 data(JSONB)里。action_type: 0 = 登记,1 = 删除。后登记的赢(registered_at, id)
CREATE TABLE IF NOT EXISTS product_events (
    id            BIGINT      PRIMARY KEY,
    data          JSONB       NOT NULL,
    action_type   INTEGER     NOT NULL DEFAULT 0,
    registered_by TEXT        NOT NULL DEFAULT 'factory',
    registered_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- 每个商品取最新版本: 按 (商品代码 字节序, 登记顺序 新→旧) 读索引,数据库可以边读边去重,读够条数就停
CREATE INDEX IF NOT EXISTS product_events_code_latest_idx
    ON product_events (((data ->> 'product_code') COLLATE "C"), registered_at DESC, id DESC);

-- 商品ラベル(追加型)。自然キー [product_code, label_code] の label_code は空でもよい
CREATE TABLE IF NOT EXISTS product_label_events (
    id            BIGINT      PRIMARY KEY,
    data          JSONB       NOT NULL,
    action_type   INTEGER     NOT NULL DEFAULT 0,
    registered_by TEXT        NOT NULL DEFAULT 'factory',
    registered_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 平台管理的 DB 结构(人工维护,AI 不可修改)
CREATE TABLE IF NOT EXISTS jobs (
    id         BIGINT      PRIMARY KEY,
    job_type   TEXT        NOT NULL DEFAULT 'default',
    status     INTEGER     NOT NULL CHECK (status IN (0, 1, 2)),  -- 含义见 specs/jobs/_domain.yaml
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now() CHECK (isfinite(created_at)),
    finished_at TIMESTAMPTZ          CHECK (finished_at IS NULL OR isfinite(finished_at))  -- 成功/失败时写入;未完成为 NULL
);

-- ListJobs 的 keyset 分页用
CREATE INDEX IF NOT EXISTS jobs_created_at_id_idx ON jobs (created_at DESC, id DESC);

-- 平台管理的 DB 结构(人工维护,AI 不可修改)
-- 日别・店别の売上(日期 / 十进制 类型的示例)
CREATE TABLE IF NOT EXISTS daily_sales (
    sales_date  DATE    NOT NULL,
    store_code  INTEGER NOT NULL CHECK (store_code > 0),
    amount      NUMERIC NOT NULL CHECK (amount >= 0),
    discount    NUMERIC,
    closed_on   DATE,
    PRIMARY KEY (sales_date, store_code)
);

-- ListTopSales 的 keyset 分页用
CREATE INDEX IF NOT EXISTS daily_sales_amount_idx ON daily_sales (amount DESC, sales_date DESC, store_code DESC);

-- 店別の売価(有効期間つき: valid_to が空 = 期限なし)
CREATE TABLE IF NOT EXISTS store_prices (
    store_code   INTEGER NOT NULL CHECK (store_code > 0),
    product_code TEXT    NOT NULL,
    valid_from   DATE    NOT NULL,
    valid_to     DATE,
    price        NUMERIC NOT NULL CHECK (price >= 0),
    PRIMARY KEY (store_code, product_code, valid_from)
);

-- 売価変更の指示(新売価・旧売価・取引ID。取引ID が空 = 通常の売変)。固定条件(列どうしの比較・定数)の例
CREATE TABLE IF NOT EXISTS price_changes (
    store_code   INTEGER NOT NULL CHECK (store_code > 0),
    product_code TEXT    NOT NULL,
    proc_date    DATE    NOT NULL,
    new_price    INTEGER,
    old_price    INTEGER,
    deal_id      BIGINT,
    PRIMARY KEY (store_code, product_code, proc_date)
);

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

-- +goose Down
DROP TABLE IF EXISTS order_items;
DROP TABLE IF EXISTS orders;
DROP TABLE IF EXISTS product_prices;
DROP TABLE IF EXISTS products;
DROP TABLE IF EXISTS customers;
DROP TABLE IF EXISTS price_changes;
DROP TABLE IF EXISTS store_prices;
DROP TABLE IF EXISTS daily_sales;
DROP TABLE IF EXISTS jobs;
DROP TABLE IF EXISTS product_label_events;
DROP TABLE IF EXISTS product_events;
