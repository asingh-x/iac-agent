CREATE TABLE IF NOT EXISTS users (
    id          TEXT PRIMARY KEY,
    username    TEXT UNIQUE NOT NULL,
    token_hash  TEXT NOT NULL,
    role        TEXT NOT NULL DEFAULT 'member',
    active      BOOLEAN NOT NULL DEFAULT TRUE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS tasks (
    id            TEXT PRIMARY KEY,
    user_id       TEXT NOT NULL REFERENCES users(id),
    status        TEXT NOT NULL DEFAULT 'queued',
    input_type    TEXT NOT NULL,
    input_text    TEXT NOT NULL,
    output_type   TEXT NOT NULL,
    pr_url        TEXT,
    input_tokens  INTEGER NOT NULL DEFAULT 0,
    output_tokens INTEGER NOT NULL DEFAULT 0,
    error_msg     TEXT,
    output        TEXT,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    started_at       TIMESTAMPTZ,
    completed_at     TIMESTAMPTZ,
    pending_question TEXT
);

CREATE INDEX IF NOT EXISTS idx_tasks_user_id ON tasks(user_id);
CREATE INDEX IF NOT EXISTS idx_tasks_status  ON tasks(status);

CREATE TABLE IF NOT EXISTS user_settings (
    user_id          TEXT PRIMARY KEY REFERENCES users(id),
    github_token     TEXT,
    atlassian_token  TEXT,
    atlassian_domain TEXT,
    atlassian_email  TEXT,
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
