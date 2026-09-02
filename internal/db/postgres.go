package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// PostgresStore implements Store using PostgreSQL.
type PostgresStore struct {
	db *sql.DB
}

// NewPostgres opens a connection pool to PostgreSQL and applies any pending migrations.
func NewPostgres(connStr string) (*PostgresStore, error) {
	db, err := sql.Open("pgx", connStr)
	if err != nil {
		return nil, fmt.Errorf("postgres open: %w", err)
	}
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(5 * time.Minute)

	if err := runMigrations(context.Background(), db); err != nil {
		return nil, fmt.Errorf("postgres migrate: %w", err)
	}
	return &PostgresStore{db: db}, nil
}

func (s *PostgresStore) Close() error { return s.db.Close() }

func (s *PostgresStore) Ping(ctx context.Context) error {
	return s.db.PingContext(ctx)
}

// TruncateForTest deletes all rows from all tables. Only call from tests.
func (s *PostgresStore) TruncateForTest(t interface {
	Helper()
	Fatalf(string, ...any)
}) {
	t.Helper()
	_, err := s.db.Exec(`TRUNCATE TABLE audit_events, tasks, user_settings, users, repo_index RESTART IDENTITY CASCADE`)
	if err != nil {
		t.Fatalf("TruncateForTest: %v", err)
	}
}

// --- Users ---

// CreateUser's INSERT is safe to retry: id is a UUID generated here, before
// the call, so a retry after an already-successful-but-unacknowledged first
// attempt hits a primary-key violation (a real, surfaced error) rather than
// silently creating a second row for the same logical user.
func (s *PostgresStore) CreateUser(ctx context.Context, username, tokenHash, role string) (*User, error) {
	u := &User{
		ID:        uuid.NewString(),
		Username:  username,
		TokenHash: tokenHash,
		Role:      role,
		Active:    true,
		CreatedAt: time.Now().UTC(),
	}
	err := s.withRetry(ctx, func() error {
		_, err := s.db.ExecContext(ctx,
			`INSERT INTO users (id, username, token_hash, role, active, created_at)
			 VALUES ($1, $2, $3, $4, TRUE, $5)`,
			u.ID, u.Username, u.TokenHash, u.Role, u.CreatedAt,
		)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("create user: %w", err)
	}
	return u, nil
}

func (s *PostgresStore) GetUserByTokenHash(ctx context.Context, hash string) (*User, error) {
	var u *User
	err := s.withRetry(ctx, func() error {
		row := s.db.QueryRowContext(ctx,
			`SELECT id, username, token_hash, role, active, created_at
			 FROM users WHERE token_hash = $1 AND active = TRUE`, hash)
		var scanErr error
		u, scanErr = scanPGUser(row)
		return scanErr
	})
	return u, err
}

// EnsureAdmin's INSERT is an UPSERT (ON CONFLICT DO UPDATE) — idempotent
// regardless of whether a retried attempt follows an already-successful one.
func (s *PostgresStore) EnsureAdmin(ctx context.Context, username, tokenHash string) error {
	return s.withRetry(ctx, func() error {
		_, err := s.db.ExecContext(ctx,
			`INSERT INTO users (id, username, token_hash, role, active, created_at)
			 VALUES ($1, $2, $3, 'admin', TRUE, NOW())
			 ON CONFLICT(username) DO UPDATE SET token_hash = EXCLUDED.token_hash, active = TRUE`,
			uuid.NewString(), username, tokenHash,
		)
		return err
	})
}

func (s *PostgresStore) UpdateUsername(ctx context.Context, userID, username string) error {
	return s.withRetry(ctx, func() error {
		_, err := s.db.ExecContext(ctx, `UPDATE users SET username = $1 WHERE id = $2`, username, userID)
		return err
	})
}

func (s *PostgresStore) UpdateUser(ctx context.Context, userID, username, role string) error {
	return s.withRetry(ctx, func() error {
		_, err := s.db.ExecContext(ctx, `UPDATE users SET username = $1, role = $2 WHERE id = $3`, username, role, userID)
		return err
	})
}

func (s *PostgresStore) SetUserActive(ctx context.Context, userID string, active bool) error {
	return s.withRetry(ctx, func() error {
		_, err := s.db.ExecContext(ctx, `UPDATE users SET active = $1 WHERE id = $2`, active, userID)
		return err
	})
}

func (s *PostgresStore) RevokeUserToken(ctx context.Context, userID string) error {
	return s.withRetry(ctx, func() error {
		_, err := s.db.ExecContext(ctx, `UPDATE users SET token_hash = '', active = FALSE WHERE id = $1`, userID)
		return err
	})
}

func (s *PostgresStore) UpdateUserToken(ctx context.Context, userID, tokenHash string) error {
	return s.withRetry(ctx, func() error {
		_, err := s.db.ExecContext(ctx, `UPDATE users SET token_hash = $1, active = TRUE WHERE id = $2`, tokenHash, userID)
		return err
	})
}

// DeleteUser is a DELETE keyed by primary key — idempotent (deleting an
// already-deleted row is a no-op affecting zero rows, not an error).
func (s *PostgresStore) DeleteUser(ctx context.Context, userID string) error {
	return s.withRetry(ctx, func() error {
		_, err := s.db.ExecContext(ctx, `DELETE FROM users WHERE id = $1`, userID)
		return err
	})
}

func (s *PostgresStore) GetUserByID(ctx context.Context, userID string) (*User, error) {
	var u *User
	err := s.withRetry(ctx, func() error {
		row := s.db.QueryRowContext(ctx,
			`SELECT id, username, token_hash, role, active, created_at FROM users WHERE id = $1`, userID)
		var scanErr error
		u, scanErr = scanPGUser(row)
		return scanErr
	})
	return u, err
}

func (s *PostgresStore) ListUsers(ctx context.Context) ([]*User, error) {
	var users []*User
	err := s.withRetry(ctx, func() error {
		users = nil // reset in case a prior attempt partially iterated before failing
		rows, err := s.db.QueryContext(ctx,
			`SELECT id, username, token_hash, role, active, created_at FROM users ORDER BY created_at`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			u, err := scanPGUser(rows)
			if err != nil {
				return err
			}
			users = append(users, u)
		}
		return rows.Err()
	})
	return users, err
}

// --- Tasks ---

// CreateTask's INSERT is safe to retry for the same reason as CreateUser's:
// id is a client-generated UUID, so a retry after an already-successful
// first attempt surfaces as a primary-key violation, not a duplicate task.
func (s *PostgresStore) CreateTask(ctx context.Context, userID, inputType, inputText, outputType string) (*Task, error) {
	t := &Task{
		ID:         uuid.NewString(),
		UserID:     userID,
		Status:     "queued",
		InputType:  inputType,
		InputText:  inputText,
		OutputType: outputType,
		CreatedAt:  time.Now().UTC(),
	}
	err := s.withRetry(ctx, func() error {
		_, err := s.db.ExecContext(ctx,
			`INSERT INTO tasks (id, user_id, status, input_type, input_text, output_type, created_at)
			 VALUES ($1, $2, 'queued', $3, $4, $5, $6)`,
			t.ID, t.UserID, t.InputType, t.InputText, t.OutputType, t.CreatedAt,
		)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("create task: %w", err)
	}
	return t, nil
}

func (s *PostgresStore) UpdateTaskStatus(ctx context.Context, id, status string) error {
	now := time.Now().UTC()
	return s.withRetry(ctx, func() error {
		switch status {
		case "running":
			_, err := s.db.ExecContext(ctx,
				`UPDATE tasks SET status=$1, started_at=$2 WHERE id=$3`, status, now, id)
			return err
		default:
			_, err := s.db.ExecContext(ctx,
				`UPDATE tasks SET status=$1 WHERE id=$2`, status, id)
			return err
		}
	})
}

func (s *PostgresStore) UpdateTaskPendingQuestion(ctx context.Context, id, question, kind string) error {
	return s.withRetry(ctx, func() error {
		_, err := s.db.ExecContext(ctx,
			`UPDATE tasks SET pending_question=$1, pending_kind=$2 WHERE id=$3`,
			nullStr(question), nullStr(kind), id)
		return err
	})
}

func (s *PostgresStore) UpdateTaskResult(ctx context.Context, id, status, prURL, errorMsg, output string, inputTokens, outputTokens int) error {
	now := time.Now().UTC()
	return s.withRetry(ctx, func() error {
		_, err := s.db.ExecContext(ctx,
			`UPDATE tasks SET status=$1, pr_url=$2, error_msg=$3, output=$4,
			        input_tokens=$5, output_tokens=$6, completed_at=$7
			 WHERE id=$8`,
			status, nullStr(prURL), nullStr(errorMsg), nullStr(output),
			inputTokens, outputTokens, now, id,
		)
		return err
	})
}

func (s *PostgresStore) GetTask(ctx context.Context, id string) (*Task, error) {
	var t *Task
	err := s.withRetry(ctx, func() error {
		row := s.db.QueryRowContext(ctx,
			`SELECT id, user_id, status, input_type, input_text, output_type,
			        COALESCE(pr_url,''), COALESCE(error_msg,''), COALESCE(output,''),
			        input_tokens, output_tokens, created_at, started_at, completed_at,
			        COALESCE(pending_question,''), COALESCE(pending_kind,'')
			 FROM tasks WHERE id=$1`, id)
		var scanErr error
		t, scanErr = scanTask(row)
		return scanErr
	})
	return t, err
}

// MarkStaleTasksFailed and FailTasksOlderThan below are batch UPDATEs, not
// keyed by primary key — safe to wrap in withRetry for a different reason
// than this file's single-row methods: each query's own WHERE clause
// self-excludes rows it already updated (once status='failed' or
// completed_at is no longer NULL, a retry's predicate no longer matches
// them), so a retry converges on the remaining rows instead of re-applying
// the effect to rows already handled.
func (s *PostgresStore) MarkStaleTasksFailed(ctx context.Context) error {
	return s.withRetry(ctx, func() error {
		_, err := s.db.ExecContext(ctx,
			`UPDATE tasks SET status='failed', error_msg='Server restarted while task was in progress'
			 WHERE status IN ('running', 'queued', 'waiting_for_input') AND completed_at IS NULL`)
		return err
	})
}

// FailTasksOlderThan's retry-safety reasoning is the same predicate-based
// idempotency as MarkStaleTasksFailed above — see that doc comment. One
// side effect worth knowing: on the rare ack-lost-after-success case (the
// UPDATE truly succeeded but withRetry couldn't observe that and retries),
// the returned row count can undercount, since the retry's predicate no
// longer matches the rows the first attempt already updated. That count is
// only used for an informational log line in cmd/server/main.go — no
// correctness impact.
func (s *PostgresStore) FailTasksOlderThan(ctx context.Context, maxAge time.Duration, errMsg string) (int, error) {
	cutoff := time.Now().UTC().Add(-maxAge)
	var n int64
	err := s.withRetry(ctx, func() error {
		res, err := s.db.ExecContext(ctx,
			`UPDATE tasks SET status='failed', error_msg=$1, completed_at=$2
			 WHERE status IN ('running', 'queued', 'waiting_for_input') AND completed_at IS NULL AND created_at < $3`,
			errMsg, time.Now().UTC(), cutoff)
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		return err
	})
	return int(n), err
}

func (s *PostgresStore) ListUserTasks(ctx context.Context, userID string, limit int) ([]*Task, error) {
	var tasks []*Task
	err := s.withRetry(ctx, func() error {
		tasks = nil // reset in case a prior attempt partially iterated before failing
		rows, err := s.db.QueryContext(ctx,
			`SELECT id, user_id, status, input_type, input_text, output_type,
			        COALESCE(pr_url,''), COALESCE(error_msg,''), COALESCE(output,''),
			        input_tokens, output_tokens, created_at, started_at, completed_at,
			        COALESCE(pending_question,''), COALESCE(pending_kind,'')
			 FROM tasks WHERE user_id=$1 ORDER BY created_at DESC LIMIT $2`,
			userID, limit,
		)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			t, err := scanTask(rows)
			if err != nil {
				return err
			}
			tasks = append(tasks, t)
		}
		return rows.Err()
	})
	return tasks, err
}

// --- User settings ---

func (s *PostgresStore) GetUserSettings(ctx context.Context, userID string) (*UserSettings, error) {
	var result *UserSettings
	err := s.withRetry(ctx, func() error {
		row := s.db.QueryRowContext(ctx,
			`SELECT user_id,
			        COALESCE(github_token,''), COALESCE(atlassian_token,''),
			        COALESCE(atlassian_domain,''), COALESCE(atlassian_email,''),
			        updated_at
			 FROM user_settings WHERE user_id = $1`, userID)
		us := &UserSettings{}
		err := row.Scan(&us.UserID, &us.GitHubToken, &us.AtlassianToken,
			&us.AtlassianDomain, &us.AtlassianEmail, &us.UpdatedAt)
		if err == sql.ErrNoRows {
			result = &UserSettings{UserID: userID}
			return nil
		}
		if err != nil {
			return err
		}
		result = us
		return nil
	})
	return result, err
}

// UpsertUserSettings's INSERT is an UPSERT (ON CONFLICT DO UPDATE) —
// idempotent regardless of whether a retried attempt follows an
// already-successful one.
func (s *PostgresStore) UpsertUserSettings(ctx context.Context, us *UserSettings) error {
	return s.withRetry(ctx, func() error {
		_, err := s.db.ExecContext(ctx,
			`INSERT INTO user_settings (user_id, github_token, atlassian_token, atlassian_domain, atlassian_email, updated_at)
			 VALUES ($1, $2, $3, $4, $5, NOW())
			 ON CONFLICT(user_id) DO UPDATE SET
			   github_token     = EXCLUDED.github_token,
			   atlassian_token  = EXCLUDED.atlassian_token,
			   atlassian_domain = EXCLUDED.atlassian_domain,
			   atlassian_email  = EXCLUDED.atlassian_email,
			   updated_at       = NOW()`,
			us.UserID,
			nullStr(us.GitHubToken), nullStr(us.AtlassianToken),
			nullStr(us.AtlassianDomain), nullStr(us.AtlassianEmail),
		)
		return err
	})
}

// --- Audit ---

// RecordAuditEvent's INSERT is safe to retry for the same reason as
// CreateUser's: id is a client-generated UUID, so a retry after an
// already-successful first attempt surfaces as a primary-key violation, not
// a duplicate audit entry.
func (s *PostgresStore) RecordAuditEvent(ctx context.Context, actorID, actorUsername, action, targetID, detail string) error {
	return s.withRetry(ctx, func() error {
		_, err := s.db.ExecContext(ctx,
			`INSERT INTO audit_events (id, actor_id, actor_username, action, target_id, detail, created_at)
			 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			uuid.NewString(), actorID, actorUsername, action, nullStr(targetID), nullStr(detail), time.Now().UTC(),
		)
		return err
	})
}

func (s *PostgresStore) GetRepoIndex(ctx context.Context, repoID, commitSHA string) (*RepoIndexEntry, error) {
	var entry *RepoIndexEntry
	err := s.withRetry(ctx, func() error {
		row := s.db.QueryRowContext(ctx,
			`SELECT repo_id, commit_sha, summary, indexed_at FROM repo_index WHERE repo_id = $1 AND commit_sha = $2`,
			repoID, commitSHA)
		var e RepoIndexEntry
		var raw string
		err := row.Scan(&e.RepoID, &e.CommitSHA, &raw, &e.IndexedAt)
		if err == sql.ErrNoRows {
			entry = nil
			return nil
		}
		if err != nil {
			return err
		}
		e.Summary = json.RawMessage(raw)
		entry = &e
		return nil
	})
	return entry, err
}

// SaveRepoIndex's INSERT is an UPSERT (ON CONFLICT DO UPDATE) — idempotent
// regardless of whether a retried attempt follows an already-successful one.
func (s *PostgresStore) SaveRepoIndex(ctx context.Context, repoID, commitSHA string, summary json.RawMessage) error {
	return s.withRetry(ctx, func() error {
		_, err := s.db.ExecContext(ctx,
			`INSERT INTO repo_index (id, repo_id, commit_sha, summary, indexed_at)
			 VALUES ($1, $2, $3, $4, $5)
			 ON CONFLICT (repo_id, commit_sha) DO UPDATE SET summary = EXCLUDED.summary, indexed_at = EXCLUDED.indexed_at`,
			uuid.NewString(), repoID, commitSHA, string(summary), time.Now().UTC(),
		)
		return err
	})
}

func (s *PostgresStore) ListAuditEvents(ctx context.Context, limit int) ([]*AuditEvent, error) {
	var events []*AuditEvent
	err := s.withRetry(ctx, func() error {
		events = nil // reset in case a prior attempt partially iterated before failing
		rows, err := s.db.QueryContext(ctx,
			`SELECT id, actor_id, COALESCE(actor_username,''), action, COALESCE(target_id,''), COALESCE(detail,''), created_at
			 FROM audit_events ORDER BY created_at DESC LIMIT $1`, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			e := &AuditEvent{}
			if err := rows.Scan(&e.ID, &e.ActorID, &e.ActorUsername, &e.Action, &e.TargetID, &e.Detail, &e.CreatedAt); err != nil {
				return err
			}
			events = append(events, e)
		}
		return rows.Err()
	})
	return events, err
}

// --- Run events ---

func (s *PostgresStore) AppendRunEvent(ctx context.Context, taskID, eventType string, payload json.RawMessage) (int64, error) {
	var seq int64
	err := s.withRetry(ctx, func() error {
		return s.db.QueryRowContext(ctx,
			`INSERT INTO run_events (task_id, event_type, payload) VALUES ($1, $2, $3) RETURNING id`,
			taskID, eventType, string(payload),
		).Scan(&seq)
	})
	return seq, err
}

func (s *PostgresStore) GetRunEventsSince(ctx context.Context, taskID string, sinceSeq int64) ([]RunEvent, error) {
	var events []RunEvent
	err := s.withRetry(ctx, func() error {
		events = nil // reset in case a prior attempt partially iterated before failing
		rows, err := s.db.QueryContext(ctx,
			`SELECT id, event_type, payload, created_at FROM run_events WHERE task_id = $1 AND id > $2 ORDER BY id`,
			taskID, sinceSeq,
		)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var ev RunEvent
			var rawPayload string
			if err := rows.Scan(&ev.Seq, &ev.Type, &rawPayload, &ev.CreatedAt); err != nil {
				return err
			}
			ev.Payload = json.RawMessage(rawPayload)
			events = append(events, ev)
		}
		return rows.Err()
	})
	return events, err
}

// --- helpers ---

// scanner is satisfied by *sql.Row and *sql.Rows.
type scanner interface {
	Scan(dest ...any) error
}

// nullStr converts an empty string to nil so the DB stores NULL.
func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func scanPGUser(s scanner) (*User, error) {
	u := &User{}
	err := s.Scan(&u.ID, &u.Username, &u.TokenHash, &u.Role, &u.Active, &u.CreatedAt)
	if err != nil {
		return nil, err
	}
	return u, nil
}

func scanTask(s scanner) (*Task, error) {
	t := &Task{}
	err := s.Scan(
		&t.ID, &t.UserID, &t.Status, &t.InputType, &t.InputText, &t.OutputType,
		&t.PRUrl, &t.ErrorMsg, &t.Output,
		&t.InputTokens, &t.OutputTokens,
		&t.CreatedAt, &t.StartedAt, &t.CompletedAt,
		&t.PendingQuestion, &t.PendingKind,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return t, err
}
