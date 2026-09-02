-- 0007_task_queue.sql
CREATE TABLE IF NOT EXISTS task_queue (
    id                 TEXT PRIMARY KEY,
    queue_name         TEXT NOT NULL,
    payload            JSONB NOT NULL,
    status             TEXT NOT NULL DEFAULT 'queued', -- queued | leased | done | dead_letter
    lease_token        BIGINT NOT NULL DEFAULT 0,
    leased_until       TIMESTAMPTZ,
    attempt_count      INTEGER NOT NULL DEFAULT 0,
    max_attempts       INTEGER NOT NULL DEFAULT 5,
    next_attempt_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    dead_letter_reason TEXT,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_task_queue_poll ON task_queue(queue_name, status, next_attempt_at);
