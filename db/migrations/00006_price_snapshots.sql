-- 売価スナップショット(整数编码的日期 YYYYMMDD の例: R2.5 T3)。db/schema/sales.sql と同じ定義。

-- +goose Up
CREATE TABLE IF NOT EXISTS price_snapshots (
    store_code   INTEGER NOT NULL CHECK (store_code > 0),
    product_code TEXT    NOT NULL,
    snap_ymd     INTEGER NOT NULL,
    price        NUMERIC NOT NULL CHECK (price >= 0),
    PRIMARY KEY (store_code, product_code, snap_ymd)
);
CREATE INDEX IF NOT EXISTS price_snapshots_store_ymd_idx ON price_snapshots (store_code, snap_ymd);

-- +goose Down
DROP TABLE IF EXISTS price_snapshots;
