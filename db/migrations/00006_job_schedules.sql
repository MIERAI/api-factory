-- job_schedules: the schedule of a job type (sample of the command factory R3-1: UpdateJobSchedule with optimistic locking).
-- Same definition as db/schema/schedules.sql.

-- +goose Up
CREATE TABLE IF NOT EXISTS job_schedules (
    job_type   TEXT        PRIMARY KEY CHECK (length(job_type) BETWEEN 1 AND 64),
    cron       TEXT        NOT NULL CHECK (length(cron) BETWEEN 1 AND 100),
    enabled    BOOLEAN     NOT NULL DEFAULT true,
    version    INTEGER     NOT NULL DEFAULT 1 CHECK (version > 0),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE IF EXISTS job_schedules;
