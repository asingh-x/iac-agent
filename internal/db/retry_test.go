package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"net"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// --- isTransientConnError classification ---

func TestIsTransientConnError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"plain error", errors.New("boom"), false},
		{"context canceled", context.Canceled, false},
		{"context deadline exceeded", context.DeadlineExceeded, false},
		{"wrapped context canceled", errWrap{context.Canceled}, false},
		{"driver.ErrBadConn", driver.ErrBadConn, true},
		{"io.EOF", io.EOF, true},
		{"io.ErrUnexpectedEOF", io.ErrUnexpectedEOF, true},
		{"ECONNREFUSED", syscall.ECONNREFUSED, true},
		{"ECONNRESET", syscall.ECONNRESET, true},
		{"EPIPE", syscall.EPIPE, true},
		{"net.OpError wrapping ECONNREFUSED", &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}, true},
		{"pgError connection_exception (08006)", &pgconn.PgError{Code: "08006"}, true},
		{"pgError cannot_connect_now (08004)", &pgconn.PgError{Code: "08004"}, true},
		{"pgError unique_violation (23505)", &pgconn.PgError{Code: "23505"}, false},
		{"pgError syntax_error (42601)", &pgconn.PgError{Code: "42601"}, false},
		{"sql.ErrNoRows", sql.ErrNoRows, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isTransientConnError(tc.err); got != tc.want {
				t.Errorf("isTransientConnError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// errWrap wraps another error, satisfying errors.Is/As via Unwrap — used to
// prove the classifier looks through wrapping, not just literal equality.
type errWrap struct{ err error }

func (e errWrap) Error() string { return "wrapped: " + e.err.Error() }
func (e errWrap) Unwrap() error { return e.err }

// --- withRetry behavior ---

func TestWithRetry_SucceedsWithoutRetryOnFirstTry(t *testing.T) {
	s := &PostgresStore{}
	calls := 0
	err := s.withRetry(context.Background(), func() error {
		calls++
		return nil
	})
	if err != nil {
		t.Fatalf("withRetry: %v", err)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1", calls)
	}
}

func TestWithRetry_RecoversAfterTransientFailures(t *testing.T) {
	s := &PostgresStore{}
	calls := 0
	err := s.withRetry(context.Background(), func() error {
		calls++
		if calls < dbMaxAttempts {
			return driver.ErrBadConn
		}
		return nil
	})
	if err != nil {
		t.Fatalf("withRetry: %v", err)
	}
	if calls != dbMaxAttempts {
		t.Errorf("calls = %d, want %d", calls, dbMaxAttempts)
	}
}

func TestWithRetry_GivesUpAfterMaxAttempts(t *testing.T) {
	s := &PostgresStore{}
	calls := 0
	err := s.withRetry(context.Background(), func() error {
		calls++
		return driver.ErrBadConn
	})
	if err == nil {
		t.Fatal("expected an error when every attempt fails transiently")
	}
	if calls != dbMaxAttempts {
		t.Errorf("calls = %d, want %d (gave up after max attempts)", calls, dbMaxAttempts)
	}
}

func TestWithRetry_DoesNotRetryNonTransientError(t *testing.T) {
	s := &PostgresStore{}
	calls := 0
	wantErr := errors.New("unique_violation")
	err := s.withRetry(context.Background(), func() error {
		calls++
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1 — a non-transient error must not be retried", calls)
	}
}

func TestWithRetry_DoesNotRetryContextCanceled(t *testing.T) {
	s := &PostgresStore{}
	calls := 0
	err := s.withRetry(context.Background(), func() error {
		calls++
		return context.Canceled
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1 — a caller cancellation must not be retried", calls)
	}
}

func TestWithRetry_StopsWaitingWhenContextDoneDuringBackoff(t *testing.T) {
	s := &PostgresStore{}
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0

	done := make(chan error, 1)
	go func() {
		done <- s.withRetry(ctx, func() error {
			calls++
			return driver.ErrBadConn // always transient, would otherwise retry dbMaxAttempts times
		})
	}()

	// Cancel well before the first backoff (100ms) would naturally elapse.
	time.Sleep(10 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, driver.ErrBadConn) {
			t.Errorf("err = %v, want the underlying transient error, not ctx.Err()", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("withRetry did not return promptly after ctx was cancelled during backoff")
	}
	if calls >= dbMaxAttempts {
		t.Errorf("calls = %d, want fewer than %d — cancellation during backoff should cut retries short", calls, dbMaxAttempts)
	}
}

// --- End-to-end: a real *sql.DB backed by a fake driver.Connector that fails
// transiently N times before succeeding, proving PostgresStore methods
// actually recover via withRetry rather than just the helper in isolation.

// flakyDriver models a Postgres connection whose queries fail with a
// transient, non-ErrBadConn network error (so database/sql's own internal
// bad-connection retry — which specifically special-cases driver.ErrBadConn
// — never kicks in and masks whether *our* withRetry is doing the work) for
// the first failCount calls, then succeeds.
type flakyDriver struct {
	failCount int
	calls     int
}

func (d *flakyDriver) Open(name string) (driver.Conn, error) {
	return nil, errors.New("flakyDriver: use flakyConnector, not sql.Register")
}

func (d *flakyDriver) maybeFail() error {
	d.calls++
	if d.calls <= d.failCount {
		return &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}
	}
	return nil
}

type flakyConnector struct{ d *flakyDriver }

func (c *flakyConnector) Connect(ctx context.Context) (driver.Conn, error) {
	return &flakyConn{d: c.d}, nil // connection establishment always succeeds; failures happen at query time
}
func (c *flakyConnector) Driver() driver.Driver { return c.d }

// flakyConn implements just enough of driver.Conn (+ QueryerContext) for
// database/sql to route QueryRowContext through QueryContext directly,
// without ever calling Prepare/Begin.
type flakyConn struct{ d *flakyDriver }

func (c *flakyConn) Prepare(query string) (driver.Stmt, error) {
	return nil, errors.New("not implemented")
}
func (c *flakyConn) Close() error              { return nil }
func (c *flakyConn) Begin() (driver.Tx, error) { return nil, errors.New("not implemented") }

func (c *flakyConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if err := c.d.maybeFail(); err != nil {
		return nil, err
	}
	return emptyRows{}, nil
}

// emptyRows models a zero-row result set — GetUserByID's Scan then reports
// sql.ErrNoRows, which is exactly the "real, non-transient" outcome this
// test wants to see once the fake connection stops failing.
type emptyRows struct{}

func (emptyRows) Columns() []string {
	return []string{"id", "username", "token_hash", "role", "active", "created_at"}
}
func (emptyRows) Close() error                   { return nil }
func (emptyRows) Next(dest []driver.Value) error { return io.EOF }

func TestPostgresStore_GetUserByID_RecoversFromTransientConnFailures(t *testing.T) {
	fd := &flakyDriver{failCount: dbMaxAttempts - 1} // fails one fewer time than we'll retry
	store := &PostgresStore{db: sql.OpenDB(&flakyConnector{d: fd})}
	t.Cleanup(func() { store.db.Close() })

	_, err := store.GetUserByID(context.Background(), "nonexistent")
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetUserByID err = %v, want sql.ErrNoRows (the query should have eventually gone through)", err)
	}
	if fd.calls != dbMaxAttempts {
		t.Errorf("underlying QueryContext called %d times, want %d (%d failures + 1 success)", fd.calls, dbMaxAttempts, dbMaxAttempts-1)
	}
}

func TestPostgresStore_GetUserByID_GivesUpAfterMaxAttempts(t *testing.T) {
	fd := &flakyDriver{failCount: 999} // always fails
	store := &PostgresStore{db: sql.OpenDB(&flakyConnector{d: fd})}
	t.Cleanup(func() { store.db.Close() })

	_, err := store.GetUserByID(context.Background(), "nonexistent")
	if err == nil {
		t.Fatal("expected an error when every underlying attempt fails transiently")
	}
	if errors.Is(err, sql.ErrNoRows) {
		t.Error("got sql.ErrNoRows, want the underlying transient connection error to surface")
	}
	if fd.calls != dbMaxAttempts {
		t.Errorf("underlying QueryContext called %d times, want %d (gave up after max attempts)", fd.calls, dbMaxAttempts)
	}
}
