-- GetProductLabel looked up one (product_code, label_code) with a sequential scan: ~100–175 ms on prod's
-- 1,000,000 events per database (dev has a handful, so only prod showed it — found by the rpc-latency SLO and the
-- quiet-traffic negative test, 2026-11-20). The index must use the default collation: the query's WHERE compares
-- with it, so an index on (… COLLATE "C") like product_events_code_latest_idx is not used (measured: 44 ms vs 0.1 ms).
-- CONCURRENTLY: prod's tables are large; don't block writes while it builds (needs no transaction).

-- +goose NO TRANSACTION
-- +goose Up
CREATE INDEX CONCURRENTLY IF NOT EXISTS product_label_events_key_idx
    ON product_label_events ((data ->> 'product_code'), (data ->> 'label_code'), registered_at DESC, id DESC);

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS product_label_events_key_idx;
