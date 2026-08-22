package db

import (
	"context"
	"database/sql/driver"
	"errors"
	"io"
	"net"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// dbMaxAttempts is the total number of times withRetry will call fn — the
// first attempt plus up to dbMaxAttempts-1 retries. 3 total attempts with a
// short exponential backoff absorbs a brief connection blip (a Postgres
// failover, a network hiccup) without turning a single transient error into
// a task-visible failure, while staying small enough that a genuinely down
// database still fails fast.
const dbMaxAttempts = 3

// dbRetryBaseDelay is the base of the exponential backoff between attempts:
// 100ms, then 200ms (attempt indices 0 and 1; there's no wait after the
// final attempt). Hardcoded rather than made configurable — this is an
// internal resiliency detail, not something an operator needs to tune per
// environment, and every call site shares the same tolerance for added
// latency on the (rare) retry path.
const dbRetryBaseDelay = 100 * time.Millisecond

// withRetry calls fn, retrying up to dbMaxAttempts total times with
// exponential backoff if fn's error is a genuinely transient, connection-
// level failure (see isTransientConnError) — never for a context
// cancellation/deadline the caller itself set, and never for an error the
// database actually processed and reported back (constraint violation,
// syntax error, ...), since retrying those would just reproduce the same
// outcome instead of recovering.
//
// Every call site wraps a query that is safe to retry even in the case
// where fn's first attempt actually succeeded on the server but the
// response was lost before the caller saw it (the classic "did my write
// land" ambiguity a network blip creates): every mutating query in this
// file is either an idempotent UPSERT (ON CONFLICT DO UPDATE), an UPDATE/
// DELETE that sets absolute values keyed by primary key (never a relative
// increment, so re-applying it converges rather than compounds), or an
// INSERT whose primary key is generated client-side before the call — a
// retried INSERT after an already-successful one surfaces as a duplicate-
// key error rather than silently creating a second row. See the doc
// comments on CreateUser/CreateTask/RecordAuditEvent for the individual
// cases.
func (s *PostgresStore) withRetry(ctx context.Context, fn func() error) error {
	var err error
	for attempt := 0; attempt < dbMaxAttempts; attempt++ {
		err = fn()
		if err == nil || !isTransientConnError(err) {
			return err
		}
		if attempt == dbMaxAttempts-1 {
			break
		}
		delay := dbRetryBaseDelay * time.Duration(1<<uint(attempt))
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			// The caller's context ended while we were backing off — return
			// the real (transient) error we were about to retry, not
			// ctx.Err(), so the caller sees why the operation actually
			// failed rather than a generic cancellation.
			return err
		}
	}
	return err
}

// isTransientConnError reports whether err represents a connection-level
// Postgres/network failure that is safe to retry — the connection was
// refused, reset, or dropped before or during the round trip — as opposed
// to an error the server actually processed and returned (a constraint
// violation, a syntax error, a permission error, ...) or a context
// cancellation/deadline the caller itself is responsible for.
//
// Deliberately excluded:
//   - context.Canceled / context.DeadlineExceeded: retrying a context the
//     caller intentionally cancelled or timed out would override their
//     intent, and since withRetry re-uses the same ctx for every attempt, a
//     retry against an already-done context would just fail identically
//     anyway (there is no way to distinguish "the dial timed out" from "the
//     caller's own query deadline expired" from outside the driver, so the
//     conservative choice — never retry either — is the only safe one).
//   - *pgconn.PgError values outside SQLSTATE class 08 (connection
//     exception): these are real server-side outcomes (23505 unique
//     violation, 42601 syntax error, ...) that a retry would just reproduce.
func isTransientConnError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, driver.ErrBadConn) {
		return true
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) {
		return true
	}
	// pgconn.ConnectError wraps a failed connection attempt (dial refused,
	// DNS failure, TLS handshake failure, ...) — always safe to retry since
	// by definition nothing was ever sent to a server.
	var connectErr *pgconn.ConnectError
	if errors.As(err, &connectErr) {
		return true
	}
	// A *pgconn.PgError is the server's own response to a query it actually
	// received and processed — safe to retry only for its connection-
	// exception class (SQLSTATE class 08), never for a data/constraint/
	// syntax error the server is correctly reporting.
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return strings.HasPrefix(pgErr.Code, "08")
	}
	// Any other net.Error at this point (dial/read/write failure, i/o
	// timeout on the socket) that wasn't already classified above as a
	// caller-set context deadline is a genuine transport-level failure.
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	return false
}
