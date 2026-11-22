-- logistics 领域: 配送業者と倉庫の契約(嵌套の固定条件で 今天 を使う例子)。db/schema/logistics.sql と同じ内容

-- +goose Up
CREATE TABLE IF NOT EXISTS carrier_contracts (
    carrier_id     BIGINT NOT NULL,
    warehouse_code TEXT   NOT NULL,
    valid_from     DATE   NOT NULL,
    valid_to       DATE,
    PRIMARY KEY (carrier_id, warehouse_code, valid_from)
);

-- +goose Down
DROP TABLE IF EXISTS carrier_contracts;
