-- 売上明細(分组 / 排名 の例: R2-4 group / rank)。db/schema/sales.sql と同じ定義。

-- +goose Up
CREATE TABLE IF NOT EXISTS sales_lines (
    line_id     BIGINT  PRIMARY KEY,
    sales_date  DATE    NOT NULL,
    store_code  INTEGER NOT NULL CHECK (store_code > 0),
    category    TEXT,                       -- 部門(未分類は空)
    quantity    INTEGER NOT NULL CHECK (quantity > 0),
    amount      NUMERIC NOT NULL CHECK (amount >= 0),
    discount    NUMERIC                     -- 値引き額(値引きなしは空)
);
CREATE INDEX IF NOT EXISTS sales_lines_date_store_idx ON sales_lines (sales_date, store_code);

-- +goose Down
DROP TABLE IF EXISTS sales_lines;
