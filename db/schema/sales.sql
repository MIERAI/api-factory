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
