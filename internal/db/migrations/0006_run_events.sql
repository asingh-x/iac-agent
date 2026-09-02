CREATE TABLE IF NOT EXISTS run_events (
    id         BIGSERIAL PRIMARY KEY,
    task_id    TEXT NOT NULL REFERENCES tasks(id),
    event_type TEXT NOT NULL,
    payload    JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_run_events_task_id_id ON run_events(task_id, id);
