-- ============================================================================
-- logistics 领域(多表: 关联 / 多对一查找的例子)。含义见 design/logistics/领域.yaml
-- 出荷は倉庫・区画・配送業者のコードを持つ。マスタは別システムから日次で取り込むため外部キーは張らない
-- (マスタにまだ無いコード・廃止されたコードが出荷に残る → 関連先が見つからない行がありうる)
-- ============================================================================
CREATE TABLE IF NOT EXISTS regions (
    region_code TEXT PRIMARY KEY,
    name        TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS warehouses (
    warehouse_code TEXT PRIMARY KEY,
    name           TEXT NOT NULL,
    region_code    TEXT              -- 地域未設定 = 空
);

-- 倉庫の区画: 区画番号は倉庫ごと(複合キー)
CREATE TABLE IF NOT EXISTS zones (
    warehouse_code TEXT    NOT NULL,
    zone_no        INTEGER NOT NULL,
    label          TEXT    NOT NULL,
    PRIMARY KEY (warehouse_code, zone_no)
);

-- 配送業者: 取引先コードは一意(自社便は取引先コードなし = 空)
CREATE TABLE IF NOT EXISTS carriers (
    carrier_id   BIGINT PRIMARY KEY,
    name         TEXT   NOT NULL,
    partner_code TEXT   UNIQUE
);

CREATE TABLE IF NOT EXISTS shipments (
    id             BIGINT  PRIMARY KEY,
    warehouse_code TEXT    NOT NULL,
    zone_no        INTEGER,          -- 区画未割当 = 空
    partner_code   TEXT,             -- 配送業者の取引先コード(EDI で届いたまま。自社便・未確定は空)
    shipped_on     DATE    NOT NULL,
    weight         NUMERIC NOT NULL CHECK (weight > 0)
);
CREATE INDEX IF NOT EXISTS shipments_shipped_on_idx ON shipments (shipped_on, id);
CREATE INDEX IF NOT EXISTS shipments_warehouse_idx ON shipments (warehouse_code, id);
