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

// postgresDelivery is the acknowledgment handle for one item claimed via
// PostgresQueue.Pop. Ack/Nak are implemented in Tasks 4-5; Extend alongside
// them — stubbed here so postgresDelivery satisfies the Delivery interface
// and this package compiles.
type postgresDelivery struct {
	q          *PostgresQueue
	taskID     string
	leaseToken int64
	attempt    int
}

// pgPollInterval is how often Pop retries when no row is currently eligible
// for claim, mirroring NATSQueue.Pop's own poll-loop shape (500ms, matching
// this package's existing convention).
const pgPollInterval = 500 * time.Millisecond

// Pop polls every pgPollInterval until it claims a row or ctx is done.
func (q *PostgresQueue) Pop(ctx context.Context) (Item, Delivery, error) {
	for {
		item, delivery, err := q.tryClaim(ctx)
		if err == nil {
			return item, delivery, nil
		}
		if err != sql.ErrNoRows {
			return Item{}, nil, err
		}
		select {
		case <-ctx.Done():
			return Item{}, nil, ctx.Err()
		case <-time.After(pgPollInterval):
		}
	}
}

// tryClaim attempts a single atomic claim of one eligible row for this
// queue's name via SELECT ... FOR UPDATE SKIP LOCKED, so two concurrent
// pollers racing on this query never claim the same row: the inner SELECT
// locks its chosen row, SKIP LOCKED makes the other poller's SELECT skip
// past any row already locked by the first, and the WHERE clause's status/
// leased_until check ensures a row that is currently 'leased' with a
// leased_until still in the future is not eligible at all (regardless of
// locking) — only 'queued' rows or rows whose lease has expired qualify.
func (q *PostgresQueue) tryClaim(ctx context.Context) (Item, Delivery, error) {
	leasedUntil := time.Now().Add(q.leaseTTL)
	var (
		id         string
		payload    []byte
		leaseToken int64
		attempt    int
	)
	err := q.db.QueryRowContext(ctx, `
		UPDATE task_queue
		SET status = 'leased',
		    lease_token = lease_token + 1,
		    leased_until = $2,
		    attempt_count = attempt_count + 1,
		    updated_at = NOW()
		WHERE id = (
			SELECT id FROM task_queue
			WHERE queue_name = $1
			  AND next_attempt_at <= NOW()
			  AND (status = 'queued' OR (status = 'leased' AND leased_until < NOW()))
			ORDER BY next_attempt_at ASC
			FOR UPDATE SKIP LOCKED
			LIMIT 1
		)
		RETURNING id, payload, lease_token, attempt_count
	`, q.name, leasedUntil).Scan(&id, &payload, &leaseToken, &attempt)
	if err != nil {
		return Item{}, nil, err // sql.ErrNoRows when nothing eligible
	}

	var item Item
	if err := json.Unmarshal(payload, &item); err != nil {
		return Item{}, nil, err
	}
	return item, &postgresDelivery{q: q, taskID: id, leaseToken: leaseToken, attempt: attempt}, nil
}

// Ack/Nak/Extend are implemented in the next two tasks — stub them here so
// postgresDelivery satisfies the Delivery interface and this compiles.
func (d *postgresDelivery) Ack() error    { return nil }
func (d *postgresDelivery) Nak() error    { return nil }
func (d *postgresDelivery) Extend() error { return nil }
