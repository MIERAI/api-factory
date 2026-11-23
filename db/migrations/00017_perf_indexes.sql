-- make perf(每表 20 万行 + EXPLAIN)找到的缺少的索引。db/schema/*.sql 里同样的定义。
-- (2026-11-23: perf 自 R2 起没有跑,R2〜R2.7 的 API 第一次在数据量下检查)

-- +goose Up
-- ListCategoryShelfLabels: カテゴリの最新版を字节序(COLLATE "C")で順に読む(全件ソートが溢出していた)
CREATE INDEX IF NOT EXISTS category_events_key_c_idx
    ON category_events (((data ->> 'category') COLLATE "C"), registered_at DESC, id DESC);
-- ListLabelsWithProduct: ラベルの最新版を自然キーの字节序で順に読む
CREATE INDEX IF NOT EXISTS product_label_events_key_c_idx
    ON product_label_events (((data ->> 'product_code') COLLATE "C"), ((data ->> 'label_code') COLLATE "C"), registered_at DESC, id DESC);
-- ListLabelsByLabelCodes: 自然キーの 2 番目の列だけの条件
CREATE INDEX IF NOT EXISTS product_label_events_label_idx
    ON product_label_events ((data ->> 'label_code'));
-- ListLatestJobPerType: 種類ごとの最新(DISTINCT ON の順で読む)
CREATE INDEX IF NOT EXISTS jobs_type_latest_idx ON jobs ((job_type COLLATE "C"), created_at DESC, id DESC);
-- ListLatestFinishedJobs: 種類 + 状態
CREATE INDEX IF NOT EXISTS jobs_type_status_idx ON jobs (job_type, status);
-- ListCategorySales: 店舗の一覧での絞り込み(sales_lines_date_store_idx は日付が先頭)
CREATE INDEX IF NOT EXISTS sales_lines_store_idx ON sales_lines (store_code);
-- ListPriceChangeHistory: 商品 + 処理日(店舗の一覧なし)
CREATE INDEX IF NOT EXISTS price_changes_product_date_idx ON price_changes (product_code, proc_date);
-- ListShippingDelays: 未出荷の受注だけの部分索引
CREATE INDEX IF NOT EXISTS orders_delayed_idx ON orders (ordered_at, id) WHERE status = 0 AND shipped_at IS NULL;

-- +goose Down
DROP INDEX IF EXISTS orders_delayed_idx;
DROP INDEX IF EXISTS price_changes_product_date_idx;
DROP INDEX IF EXISTS sales_lines_store_idx;
DROP INDEX IF EXISTS jobs_type_status_idx;
DROP INDEX IF EXISTS jobs_type_latest_idx;
DROP INDEX IF EXISTS product_label_events_label_idx;
DROP INDEX IF EXISTS product_label_events_key_c_idx;
DROP INDEX IF EXISTS category_events_key_c_idx;
