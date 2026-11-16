-- 平台管理的 DB 结构(人工维护,AI 不可修改)
CREATE TABLE IF NOT EXISTS jobs (
    id         BIGINT      PRIMARY KEY,
    job_type   TEXT        NOT NULL DEFAULT 'default',
    status     INTEGER     NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at TIMESTAMPTZ          -- 成功/失败时写入;未完成为 NULL
);

-- ListJobs 的 keyset 分页用
CREATE INDEX IF NOT EXISTS jobs_created_at_id_idx ON jobs (created_at DESC, id DESC);
