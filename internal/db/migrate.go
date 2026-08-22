package db

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"path"
	"sort"
	"strings"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// migration is one embedded .sql file, identified by its numeric filename prefix
// (e.g. "0001_init.sql" -> version "0001").
type migration struct {
	version string
	name    string
	sql     string
}

// loadMigrations reads all embedded .sql files sorted by filename so they apply
// in the order their version prefixes imply.
func loadMigrations() ([]migration, error) {
	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		return nil, fmt.Errorf("read migrations dir: %w", err)
	}
	var out []migration
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		data, err := migrationFS.ReadFile(path.Join("migrations", e.Name()))
		if err != nil {
			return nil, fmt.Errorf("read migration %s: %w", e.Name(), err)
		}
		version := strings.SplitN(e.Name(), "_", 2)[0]
		out = append(out, migration{version: version, name: e.Name(), sql: string(data)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out, nil
}

// runMigrations applies any embedded migration not yet recorded in
// schema_migrations, in filename order. Safe to call on every process start,
// including concurrently from multiple instances — already-applied migrations
// are skipped, and applyMigration serializes concurrent attempts on the same
// migration via a Postgres advisory lock.
func runMigrations(ctx context.Context, sqlDB *sql.DB) error {
	if _, err := sqlDB.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
		    version     TEXT PRIMARY KEY,
		    applied_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	migrations, err := loadMigrations()
	if err != nil {
		return err
	}

	for _, m := range migrations {
		if err := applyMigration(ctx, sqlDB, m); err != nil {
			return err
		}
	}
	return nil
}

// applyMigration applies a single migration inside a transaction guarded by a
// session-scoped-to-the-transaction Postgres advisory lock (pg_advisory_xact_lock),
// which serializes concurrent instances trying to apply the same migration. The
// "already applied?" check happens AFTER the lock is acquired, so an instance that
// loses the race sees the migration recorded by the winner and skips it instead of
// re-running (possibly non-idempotent) SQL a second time.
func applyMigration(ctx context.Context, sqlDB *sql.DB, m migration) error {
	tx, err := sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx for %s: %w", m.name, err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext('tf_agent_migrations'))`); err != nil {
		return fmt.Errorf("lock migrations for %s: %w", m.name, err)
	}

	var applied bool
	if err := tx.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version = $1)`, m.version,
	).Scan(&applied); err != nil {
		return fmt.Errorf("check migration %s: %w", m.name, err)
	}
	if applied {
		return nil
	}

	if _, err := tx.ExecContext(ctx, m.sql); err != nil {
		return fmt.Errorf("apply migration %s: %w", m.name, err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations (version) VALUES ($1) ON CONFLICT (version) DO NOTHING`, m.version,
	); err != nil {
		return fmt.Errorf("record migration %s: %w", m.name, err)
	}
	return tx.Commit()
}
