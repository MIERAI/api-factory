-- 店舗の原価率の履歴(按日期的关联の例: R2.6 G4)。db/schema/sales.sql と同じ定義。

-- +goose Up
CREATE TABLE IF NOT EXISTS store_cost_rates (
    store_code INTEGER NOT NULL CHECK (store_code > 0),
    rate_kind  TEXT    NOT NULL,
    valid_from DATE    NOT NULL,
    valid_to   DATE,
    cost_rate  NUMERIC NOT NULL CHECK (cost_rate >= 0),
    PRIMARY KEY (store_code, rate_kind, valid_from)
);

-- +goose Down
DROP TABLE IF EXISTS store_cost_rates;
