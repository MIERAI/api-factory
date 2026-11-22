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

-- 店別・商品別の売価スナップショット(旧システムから取り込んだ表)。日付は整数 YYYYMMDD(整数编码的日期の例)。
-- 取込のバグで 20260230 のような存在しない日付が混ざることがある(CHECK はかけない: 旧システムの値をそのまま持つ)
CREATE TABLE IF NOT EXISTS price_snapshots (
    store_code   INTEGER NOT NULL CHECK (store_code > 0),
    product_code TEXT    NOT NULL,
    snap_ymd     INTEGER NOT NULL,
    price        NUMERIC NOT NULL CHECK (price >= 0),
    PRIMARY KEY (store_code, product_code, snap_ymd)
);
-- 店舗 + 期間(整数のまま範囲で引く: 日付として比べる条件も整数の比較に直して使う)
CREATE INDEX IF NOT EXISTS price_snapshots_store_ymd_idx ON price_snapshots (store_code, snap_ymd);

-- 店舗の原価率の履歴(区分ごと: STD = 通常 / SALE = 特売)。改定すると行が増える(開始日が違う)。
-- valid_to は最終日(含)。空 = 次の改定まで(按日期的关联の例: 開始日が最大の行を引く)
CREATE TABLE IF NOT EXISTS store_cost_rates (
    store_code INTEGER NOT NULL CHECK (store_code > 0),
    rate_kind  TEXT    NOT NULL,
    valid_from DATE    NOT NULL,
    valid_to   DATE,
    cost_rate  NUMERIC NOT NULL CHECK (cost_rate >= 0),
    PRIMARY KEY (store_code, rate_kind, valid_from)
);
