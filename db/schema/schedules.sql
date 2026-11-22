-- 平台管理的 DB 结构(人工维护,AI 不可修改)
-- 任务类型的定时设置(写接口的样例: 命令工厂 R3-1 的 UpdateJobSchedule,乐观锁)
CREATE TABLE IF NOT EXISTS job_schedules (
    job_type   TEXT        PRIMARY KEY CHECK (length(job_type) BETWEEN 1 AND 64),
    cron       TEXT        NOT NULL CHECK (length(cron) BETWEEN 1 AND 100),
    enabled    BOOLEAN     NOT NULL DEFAULT true,
    version    INTEGER     NOT NULL DEFAULT 1 CHECK (version > 0),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
