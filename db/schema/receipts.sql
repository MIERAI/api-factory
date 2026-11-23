-- 平台管理的 DB 结构(人工维护,AI 不可修改)
-- 売上明細(1 行 = レシートの 1 明細)。分组 / 排名(group / rank)の例: 店別・部門別の合計、件数の順位
CREATE TABLE IF NOT EXISTS sales_lines (
    line_id     BIGINT  PRIMARY KEY,
    sales_date  DATE    NOT NULL,
    store_code  INTEGER NOT NULL CHECK (store_code > 0),
    category    TEXT    CHECK (category <> ''),             -- 部門(未分類は空。空文字は入れない)
    quantity    INTEGER NOT NULL CHECK (quantity > 0),
    amount      NUMERIC NOT NULL CHECK (amount >= 0),
    discount    NUMERIC CHECK (discount > 0)                 -- 値引き額(値引きなしは空。0 は入れない)
);
CREATE INDEX IF NOT EXISTS sales_lines_date_store_idx ON sales_lines (sales_date, store_code);
CREATE INDEX IF NOT EXISTS sales_lines_store_idx ON sales_lines (store_code);  -- 店舗の一覧での絞り込み(make perf: 00017)
