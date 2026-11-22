-- storefront v2 · inventory (库存). This service's own database.
CREATE TABLE IF NOT EXISTS stock (
    sku         TEXT        NOT NULL PRIMARY KEY CHECK (sku ~ '^[A-Z0-9-]{3,32}$'),
    available   INTEGER     NOT NULL CHECK (available >= 0),   -- never negative: no overselling
    reserved    INTEGER     NOT NULL DEFAULT 0 CHECK (reserved >= 0),
    version     INTEGER     NOT NULL DEFAULT 1 CHECK (version > 0),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- A reservation of an order (the saga's step 1); committed or released later (R3–R5 write it).
CREATE TABLE IF NOT EXISTS reservations (
    order_id    BIGINT      NOT NULL,
    sku         TEXT        NOT NULL,
    qty         INTEGER     NOT NULL CHECK (qty > 0),
    status      INTEGER     NOT NULL DEFAULT 1 CHECK (status IN (1, 2, 3)),   -- 1 held, 2 committed, 3 released
    expires_at  TIMESTAMPTZ NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (order_id, sku)
);
CREATE INDEX IF NOT EXISTS reservations_expiry_idx ON reservations (expires_at) WHERE status = 1;
