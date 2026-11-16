-- 平台管理的 DB 结构(人工维护,AI 不可修改)
-- 追加型表: 每次变更追加一行,字段在 data(JSONB)里。action_type: 0 = 登记,1 = 删除。后登记的赢(registered_at, id)
CREATE TABLE IF NOT EXISTS product_events (
    id            BIGINT      PRIMARY KEY,
    data          JSONB       NOT NULL,
    action_type   INTEGER     NOT NULL DEFAULT 0,
    registered_by TEXT        NOT NULL DEFAULT 'factory',
    registered_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
