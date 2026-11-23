-- 商品の原価の履歴(追加型の JSON 版本表。逻辑表への按日期的关联の例: R2.7)。db/schema/catalog.sql と同じ定義。

-- +goose Up
CREATE TABLE IF NOT EXISTS product_cost_events (
    id            BIGINT      PRIMARY KEY,
    data          JSONB       NOT NULL,
    action_type   INTEGER     NOT NULL DEFAULT 0,
    registered_by TEXT        NOT NULL DEFAULT 'factory',
    registered_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS product_cost_events_code_idx
    ON product_cost_events ((data ->> 'product_code'), registered_at DESC, id DESC);

-- +goose Down
DROP TABLE IF EXISTS product_cost_events;
