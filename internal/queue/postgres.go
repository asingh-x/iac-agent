package queue

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// PostgresQueue is a Queue implementation backed by a Postgres table
// (task_queue), using SELECT ... FOR UPDATE SKIP LOCKED for atomic claim
// semantics and a lease_token column for fencing — see this package's
// PostgresQueue-related tests and docs/superpowers/plans for the full
// design rationale (or docs/roadmap.md once shipped).
type PostgresQueue struct {
	db          *sql.DB
	name        string
	leaseTTL    time.Duration
	maxAttempts int
}

func NewPostgresQueue(dsn, name string, leaseTTL time.Duration, maxAttempts int) (*PostgresQueue, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(5)
	db.SetMaxIdleConns(2)
	if err := db.PingContext(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return &PostgresQueue{db: db, name: name, leaseTTL: leaseTTL, maxAttempts: maxAttempts}, nil
}

func (q *PostgresQueue) Close() error {
	return q.db.Close()
}

func (q *PostgresQueue) Push(ctx context.Context, item Item) error {
	payload, err := json.Marshal(item)
	if err != nil {
		return err
	}
	_, err = q.db.ExecContext(ctx,
		`INSERT INTO task_queue (id, queue_name, payload, max_attempts) VALUES ($1, $2, $3, $4)`,
		item.TaskID, q.name, payload, q.maxAttempts,
	)
	return err
}

// Len returns the number of not-yet-completed items on this queue: queued,
// or leased but eligible for reclaim (a dead worker's abandoned lease is
// still "pending" from an operator's point of view).
func (q *PostgresQueue) Len() int {
	var n int
	err := q.db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM task_queue WHERE queue_name = $1 AND status IN ('queued', 'leased')`,
		q.name,
	).Scan(&n)
	if err != nil {
		return 0
	}
	return n
}
