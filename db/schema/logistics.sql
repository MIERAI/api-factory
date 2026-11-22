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

-- 出荷の梱包(1 出荷に 0 個以上)。梱包番号は出荷ごと。ラベルは配送業者の追跡番号(英大文字 2 桁 + 数字 6 桁)
-- 梱包は出荷の明細なので外部キーを張らない方針は同じ(取り込み順で出荷より先に届くことがある)
CREATE TABLE IF NOT EXISTS packages (
    shipment_id BIGINT      NOT NULL,
    package_no  INTEGER     NOT NULL CHECK (package_no > 0),
    label       TEXT        NOT NULL CHECK (label ~ '^[A-Z]{2}[0-9]{6}$'),
    weight      NUMERIC     NOT NULL CHECK (weight > 0),
    sealed_at   TIMESTAMPTZ,          -- 封緘時刻(未封緘 = 空)
    PRIMARY KEY (shipment_id, package_no)
);
