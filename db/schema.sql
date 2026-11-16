-- 平台管理的 DB 结构(人工维护,AI 不可修改)
CREATE TABLE IF NOT EXISTS jobs (
    id         BIGINT      PRIMARY KEY,
    status     INTEGER     NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
