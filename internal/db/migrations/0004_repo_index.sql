CREATE TABLE IF NOT EXISTS repo_index (
    id           TEXT PRIMARY KEY,
    repo_id      TEXT NOT NULL,
    commit_sha   TEXT NOT NULL,
    summary      JSONB NOT NULL,
    indexed_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (repo_id, commit_sha)
);

CREATE INDEX IF NOT EXISTS idx_repo_index_repo_id ON repo_index(repo_id);
