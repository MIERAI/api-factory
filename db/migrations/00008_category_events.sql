-- カテゴリマスタ(追加型の JSON 版本表)と、逻辑表どうしの关联 / 嵌套用の索引(R2.5 T1)。db/schema/catalog.sql と同じ定義。
-- 关联の条件(キー = 外側の列)は既定の照合順序で比較されるので、既定の照合順序の式索引(00002 と同じ理由)。
-- CONCURRENTLY: product_events は大きい(書き込みを止めない。トランザクションの外で実行)。

-- +goose NO TRANSACTION
-- +goose Up
CREATE TABLE IF NOT EXISTS category_events (
    id            BIGINT      PRIMARY KEY,
    data          JSONB       NOT NULL,
    action_type   INTEGER     NOT NULL DEFAULT 0,
    registered_by TEXT        NOT NULL DEFAULT 'factory',
    registered_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX CONCURRENTLY IF NOT EXISTS category_events_key_idx
    ON category_events ((data ->> 'category'), registered_at DESC, id DESC);
CREATE INDEX CONCURRENTLY IF NOT EXISTS product_events_code_idx
    ON product_events ((data ->> 'product_code'), registered_at DESC, id DESC);

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS product_events_code_idx;
DROP TABLE IF EXISTS category_events;
