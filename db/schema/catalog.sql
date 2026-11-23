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
-- 自然キーの字节序で最新版を順に読む / ラベルコードだけの条件(make perf: 00017)
CREATE INDEX IF NOT EXISTS product_label_events_key_c_idx
    ON product_label_events (((data ->> 'product_code') COLLATE "C"), ((data ->> 'label_code') COLLATE "C"), registered_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS product_label_events_label_idx
    ON product_label_events ((data ->> 'label_code'));
-- 关联 / 嵌套の対象が逻辑表のとき(R2.5 T1)、関連条件(キー = 外側の列)は既定の照合順序で比較される:
-- 既定の照合順序の式索引で、キーごとの最新の版を直接引く
CREATE INDEX IF NOT EXISTS product_events_code_idx
    ON product_events ((data ->> 'product_code'), registered_at DESC, id DESC);

-- カテゴリマスタ(追加型)。商品マスタの category から引く(逻辑表 → 逻辑表の关联)
CREATE TABLE IF NOT EXISTS category_events (
    id            BIGINT      PRIMARY KEY,
    data          JSONB       NOT NULL,
    action_type   INTEGER     NOT NULL DEFAULT 0,
    registered_by TEXT        NOT NULL DEFAULT 'factory',
    registered_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS category_events_key_idx
    ON category_events ((data ->> 'category'), registered_at DESC, id DESC);
-- 一覧はカテゴリの字节序(COLLATE "C")で最新版を順に読む(make perf: 00017)
CREATE INDEX IF NOT EXISTS category_events_key_c_idx
    ON category_events (((data ->> 'category') COLLATE "C"), registered_at DESC, id DESC);

-- 商品の原価の履歴(追加型。自然キー [product_code, cost_kind, valid_from])。日付は旧システムの整数 YYYYMMDD
-- (逻辑表への按日期的关联 + 整数编码的日期の例: R2.7)。商品ごとに全区分の版を引くので product_code の式索引
CREATE TABLE IF NOT EXISTS product_cost_events (
    id            BIGINT      PRIMARY KEY,
    data          JSONB       NOT NULL,
    action_type   INTEGER     NOT NULL DEFAULT 0,
    registered_by TEXT        NOT NULL DEFAULT 'factory',
    registered_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS product_cost_events_code_idx
    ON product_cost_events ((data ->> 'product_code'), registered_at DESC, id DESC);
