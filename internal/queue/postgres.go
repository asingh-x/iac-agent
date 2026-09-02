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
// PostgresQueue.Pop.
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

// Extend pushes this lease's leased_until further into the future, guarded
// by lease_token: if this delivery's token no longer matches the row's
// current lease_token (the lease already expired and was reclaimed by
// another poller), the UPDATE matches zero rows and this is a silent no-op
// rather than corrupting the reclaiming poller's lease.
func (d *postgresDelivery) Extend() error {
	leasedUntil := time.Now().Add(d.q.leaseTTL)
	_, err := d.q.db.Exec(
		`UPDATE task_queue SET leased_until = $3, updated_at = NOW() WHERE id = $1 AND lease_token = $2`,
		d.taskID, d.leaseToken, leasedUntil,
	)
	return err
}

// Ack marks this item done, guarded by lease_token in the same way as
// Extend.
//
// A stale lease_token (this delivery's lease already expired and was
// reclaimed by another poller) means zero rows match — that's success
// from this zombie caller's point of view: whoever holds the current
// lease is responsible for the row now, not an error to surface.
func (d *postgresDelivery) Ack() error {
	_, err := d.q.db.Exec(
		`UPDATE task_queue SET status = 'done', updated_at = NOW() WHERE id = $1 AND lease_token = $2`,
		d.taskID, d.leaseToken,
	)
	return err
}

// backoff returns an exponential delay capped at 5 minutes, keyed by
// attempt number (1-indexed, matching attempt_count after Pop's increment).
func backoff(attempt int) time.Duration {
	d := time.Duration(1<<uint(attempt)) * time.Second // 2s, 4s, 8s, 16s, ...
	const cap = 5 * time.Minute
	if d > cap {
		return cap
	}
	return d
}

// Nak signals that processing failed. If this delivery's lease_token is
// still current and attempt_count hasn't hit max_attempts yet, the item is
// put back on the queue with an exponential backoff delay before its next
// attempt. Otherwise it is transitioned to dead_letter — unless the
// lease_token is stale (a zombie's Nak, whose row now belongs to a
// different lease holder), in which case both UPDATEs affect zero rows and
// this is a harmless no-op, matching Ack's fencing behavior.
func (d *postgresDelivery) Nak() error {
	// Retry path: only succeeds (affects a row) if this delivery's
	// lease_token is still current AND attempt_count hasn't hit the cap yet.
	next := time.Now().Add(backoff(d.attempt))
	res, err := d.q.db.Exec(`
		UPDATE task_queue
		SET status = 'queued', next_attempt_at = $3, updated_at = NOW()
		WHERE id = $1 AND lease_token = $2 AND attempt_count < max_attempts
	`, d.taskID, d.leaseToken, next)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return nil
	}

	// Either this was the final allowed attempt (dead-letter it), or the
	// lease_token is stale (a zombie's Nak — no-op, matching Ack's fencing
	// behavior). Try the dead-letter transition; if that also affects zero
	// rows, it was the stale-fencing case, which is a harmless no-op.
	_, err = d.q.db.Exec(`
		UPDATE task_queue
		SET status = 'dead_letter',
		    dead_letter_reason = 'exceeded max_attempts (' || max_attempts || ')',
		    updated_at = NOW()
		WHERE id = $1 AND lease_token = $2 AND attempt_count >= max_attempts
	`, d.taskID, d.leaseToken)
	return err
}
