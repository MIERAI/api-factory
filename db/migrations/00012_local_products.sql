-- 店舗の DB ごとの商品マスタ(分库: 随父 の例: R2.6 G5)。db/schema/sales.sql と同じ定義。

-- +goose Up
CREATE TABLE IF NOT EXISTS local_products (
    product_code TEXT PRIMARY KEY,
    name         TEXT NOT NULL
);

-- +goose Down
DROP TABLE IF EXISTS local_products;
