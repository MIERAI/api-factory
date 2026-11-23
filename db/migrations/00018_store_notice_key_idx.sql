-- GetLatestStoreNotice: 店舗コード(JSON の整数の键)の条件に使える索引。
-- 查询里的整数的键是带校验的转换,(data ->> 'store_code') の索引は使えなかった(make perf: 全表扫描)。
-- 式は工厂が生成する(core.JSONKeyIndexSQL: 查询の json_key と逐字相同;不正な値は NULL で、写入は失败しない)。
-- make perf が schema の索引と生成の式を比较する(factory/perf/keyindex.go)。

-- +goose Up
CREATE INDEX IF NOT EXISTS store_notice_events_store_key_idx
    ON store_notice_events ((CASE WHEN (data ->> 'store_code') ~ '^[ \t\n\r\f\v]*[+-]?[0-9]+[ \t\n\r\f\v]*$' THEN (CASE WHEN ((data ->> 'store_code')::numeric BETWEEN -2147483648 AND 2147483647) THEN ((data ->> 'store_code')::int4) END) END), registered_at DESC, id DESC);
DROP INDEX IF EXISTS store_notice_events_store_idx;

-- +goose Down
CREATE INDEX IF NOT EXISTS store_notice_events_store_idx
    ON store_notice_events ((data ->> 'store_code'), registered_at DESC, id DESC);
DROP INDEX IF EXISTS store_notice_events_store_key_idx;
