package queue

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
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
	// Bounded so a hung Postgres connection can't hang /healthz (which calls
	// this) indefinitely — still returns 0 on any error or timeout, same as
	// before.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var n int
	err := q.db.QueryRowContext(ctx,
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
//
// A transient error from tryClaim (e.g. the connection is refused, the DSN
// is wrong, or Postgres is otherwise unhealthy) is logged and absorbed by
// the same poll-interval sleep as the ordinary "nothing eligible" case,
// rather than returned to the caller: task_runner.go's StartQueue loop has
// no backoff of its own on a Pop error, so returning here would hot-spin
// this goroutine at full CPU against an already-unhealthy database. This
// mirrors NATSQueue.Pop, which absorbs its own connection errors the same
// way — Pop only ever returns on ctx.Done().
func (q *PostgresQueue) Pop(ctx context.Context) (Item, Delivery, error) {
	for {
		item, delivery, err := q.tryClaim(ctx)
		if err == nil {
			return item, delivery, nil
		}
		if err != sql.ErrNoRows {
			slog.Error("postgres queue: tryClaim failed, retrying after poll interval", "queue", q.name, "err", err)
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
//
// Known limitation: this WHERE clause has no attempt_count cap of its own.
// The only path to 'dead_letter' is the explicit Nak() dead-letter branch —
// so a task whose worker crashes on every attempt (and therefore never
// calls Nak) is re-leased indefinitely here past max_attempts and never
// reaches dead_letter via this mechanism. A proper fix needs a companion
// sweep mechanism (out of scope for this change — see docs/roadmap.md's
// Known issues row). In practice this gap is bounded by the existing
// stale-task reconciler (cmd/server/main.go's runStaleTaskReconciler),
// which eventually marks such a task failed on its own, much longer,
// age-based schedule.
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

// Ack confirms this item was fully processed and deletes its row, guarded
// by lease_token (and status = 'leased') in the same way as Extend.
//
// This deletes rather than marking status = 'done', mirroring NATS's
// delete-on-ack behavior (JetStream's WorkQueuePolicy removes a message
// once acked). Item carries decrypted GitHubToken/AtlassianToken fields
// that Push persists into the payload column in cleartext — per Item's own
// doc comment these credentials are "never persisted" beyond the task's
// lifetime, so a row that will never be read again must not stick around
// forever holding them. Deleting on Ack also bounds task_queue's growth,
// which an UPDATE-to-'done' never did (nothing ever purged those rows).
//
// A stale lease_token (this delivery's lease already expired and was
// reclaimed by another poller) means zero rows match — that's success
// from this zombie caller's point of view: whoever holds the current
// lease is responsible for the row now, not an error to surface.
func (d *postgresDelivery) Ack() error {
	_, err := d.q.db.Exec(
		`DELETE FROM task_queue WHERE id = $1 AND lease_token = $2 AND status = 'leased'`,
		d.taskID, d.leaseToken,
	)
	return err
}

// backoff returns an exponential delay capped at 5 minutes, keyed by
// attempt number (1-indexed, matching attempt_count after Pop's increment).
func backoff(attempt int) time.Duration {
	// Clamp the shift amount before computing 1<<uint(attempt): in Go,
	// shifting a fixed-width integer by a count >= its bit width yields 0
	// (not a panic), so a pathologically large max_attempts config value
	// would otherwise degenerate this to a 0s "backoff" — an immediate
	// retry loop — instead of the intended 5-minute cap. Unreachable at the
	// documented default of 5, but cheap to guard. 30 is already far past
	// the point 1<<30 seconds blows through the 5-minute cap below, so the
	// clamp itself never changes behavior for any sane config.
	const maxShift = 30
	shift := attempt
	if shift < 0 {
		shift = 0
	} else if shift > maxShift {
		shift = maxShift
	}
	d := time.Duration(1<<uint(shift)) * time.Second // 2s, 4s, 8s, 16s, ...
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
//
// Both branches also require status = 'leased', in addition to the
// id/lease_token fencing check, as defense in depth against a redundant or
// late Nak resurrecting a row that is no longer leased — e.g. already
// 'done' via a prior Ack, or (per Ack's delete-on-ack behavior) already
// gone entirely, in which case this simply affects zero rows like any
// other fencing mismatch.
func (d *postgresDelivery) Nak() error {
	// Retry path: only succeeds (affects a row) if this delivery's
	// lease_token is still current AND attempt_count hasn't hit the cap yet.
	next := time.Now().Add(backoff(d.attempt))
	res, err := d.q.db.Exec(`
		UPDATE task_queue
		SET status = 'queued', next_attempt_at = $3, updated_at = NOW()
		WHERE id = $1 AND lease_token = $2 AND status = 'leased' AND attempt_count < max_attempts
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
	//
	// dead_letter rows are retained by design (unlike Ack's delete-on-ack)
	// since an operator inspects them — so, unlike the retry path above,
	// this also strips the credential fields from the payload (Postgres
	// JSONB minus-key operator) rather than leaving decrypted
	// GitHubToken/AtlassianToken sitting in a row meant to be kept around.
	_, err = d.q.db.Exec(`
		UPDATE task_queue
		SET status = 'dead_letter',
		    dead_letter_reason = 'exceeded max_attempts (' || max_attempts || ')',
		    payload = payload - 'GitHubToken' - 'AtlassianToken',
		    updated_at = NOW()
		WHERE id = $1 AND lease_token = $2 AND status = 'leased' AND attempt_count >= max_attempts
	`, d.taskID, d.leaseToken)
	return err
}
