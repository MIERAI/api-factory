-- 平台管理的 DB 结构(人工维护,AI 不可修改)
-- 追加型表: 每次变更追加一行,字段在 data(JSONB)里。action_type: 0 = 登记,1 = 删除。后登记的赢(registered_at, id)
CREATE TABLE IF NOT EXISTS product_events (
    id            BIGINT      PRIMARY KEY,
    data          JSONB       NOT NULL,
    action_type   INTEGER     NOT NULL DEFAULT 0,
    registered_by TEXT        NOT NULL DEFAULT 'factory',
    registered_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- 每个商品取最新版本: 按 (商品代码 字节序, 登记顺序 新→旧) 读索引,数据库可以边读边去重,读够条数就停
CREATE INDEX IF NOT EXISTS product_events_code_latest_idx
    ON product_events (((data ->> 'product_code') COLLATE "C"), registered_at DESC, id DESC);

-- 商品ラベル(追加型)。自然キー [product_code, label_code] の label_code は空でもよい
CREATE TABLE IF NOT EXISTS product_label_events (
    id            BIGINT      PRIMARY KEY,
    data          JSONB       NOT NULL,
    action_type   INTEGER     NOT NULL DEFAULT 0,
    registered_by TEXT        NOT NULL DEFAULT 'factory',
    registered_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- 按自然键查最新版本(db/migrations/00002): 默认排序规则 — WHERE 用它比较,COLLATE "C" 的索引用不上
CREATE INDEX IF NOT EXISTS product_label_events_key_idx
    ON product_label_events ((data ->> 'product_code'), (data ->> 'label_code'), registered_at DESC, id DESC);
