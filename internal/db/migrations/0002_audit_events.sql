CREATE TABLE IF NOT EXISTS audit_events (
    id          TEXT PRIMARY KEY,
    actor_id    TEXT NOT NULL REFERENCES users(id),
    action      TEXT NOT NULL,
    target_id   TEXT,
    detail      TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_audit_events_created_at ON audit_events(created_at DESC);
