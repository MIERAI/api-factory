-- 店舗の DB の逻辑表(追加型の JSON 版本表: 分库: 随父 の商品名 / 店舗コードで分库のお知らせ。R2.7)。db/schema/sales.sql と同じ定義。

-- +goose Up
CREATE TABLE IF NOT EXISTS store_product_name_events (
    id            BIGINT      PRIMARY KEY,
    data          JSONB       NOT NULL,
    action_type   INTEGER     NOT NULL DEFAULT 0,
    registered_by TEXT        NOT NULL DEFAULT 'factory',
    registered_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS store_product_name_events_code_idx
    ON store_product_name_events ((data ->> 'product_code'), registered_at DESC, id DESC);
CREATE TABLE IF NOT EXISTS store_notice_events (
    id            BIGINT      PRIMARY KEY,
    data          JSONB       NOT NULL,
    action_type   INTEGER     NOT NULL DEFAULT 0,
    registered_by TEXT        NOT NULL DEFAULT 'factory',
    registered_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS store_notice_events_store_idx
    ON store_notice_events ((data ->> 'store_code'), registered_at DESC, id DESC);

-- +goose Down
DROP TABLE IF EXISTS store_notice_events;
DROP TABLE IF EXISTS store_product_name_events;
