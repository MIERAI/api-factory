-- logistics 领域: 出荷の梱包(一对多: 嵌套 / 子聚合の例子)。db/schema/logistics.sql と同じ内容

-- +goose Up
CREATE TABLE IF NOT EXISTS packages (
    shipment_id BIGINT      NOT NULL,
    package_no  INTEGER     NOT NULL CHECK (package_no > 0),
    label       TEXT        NOT NULL CHECK (label ~ '^[A-Z]{2}[0-9]{6}$'),
    weight      NUMERIC     NOT NULL CHECK (weight > 0),
    sealed_at   TIMESTAMPTZ,          -- 封緘時刻(未封緘 = 空)
    PRIMARY KEY (shipment_id, package_no)
);

-- +goose Down
DROP TABLE IF EXISTS packages;
