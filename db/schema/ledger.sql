-- 平台管理的 DB 结构(人工维护,AI 不可修改)
-- 账户和扣款(消息的样例: 事件 / 消息处理器 R4 —— 发出、幂等消费、每项、仅当、墓碑)

-- 账户(余额不会小于 0;held = 已扣下、还没结算的金额)
CREATE TABLE IF NOT EXISTS accounts (
    account_id TEXT        PRIMARY KEY CHECK (account_id ~ '^[A-Z0-9-]{3,32}$'),
    owner      TEXT        NOT NULL CHECK (length(owner) BETWEEN 1 AND 64),
    balance    BIGINT      NOT NULL CHECK (balance >= 0),
    held       BIGINT      NOT NULL DEFAULT 0 CHECK (held >= 0),
    opened_at  TIMESTAMPTZ NOT NULL
);

-- 一笔转账在一个账户上的扣款(一笔转账、一个账户一行;撤销先于扣款到达时留下 REVERSED 的墓碑)
CREATE TABLE IF NOT EXISTS debits (
    transfer_id BIGINT      NOT NULL,
    account_id  TEXT        NOT NULL,
    amount      BIGINT      NOT NULL CHECK (amount > 0),
    status      INTEGER     NOT NULL CHECK (status IN (1, 2, 3)),   -- 1 已扣下 2 已结算 3 已撤销
    debited_at  TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (transfer_id, account_id)
);

-- 账户的动态(以事件 id 为主键: 同一个事件只记一次)
CREATE TABLE IF NOT EXISTS account_activity (
    event_id    TEXT        PRIMARY KEY,
    account_id  TEXT        NOT NULL,
    kind        INTEGER     NOT NULL CHECK (kind IN (1, 2)),       -- 1 开户 2 大额开户
    balance     BIGINT      NOT NULL,
    recorded_at TIMESTAMPTZ NOT NULL
);
