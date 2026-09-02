//go:build integration

package queue

// Run with: DB_URL=... go test -tags=integration ./internal/queue/... -run TestPostgresChaos -v
//
// This is the PostgresQueue analogue of chaos_test.go's NATS coverage: the
// same two "pod crash" and "pod graceful shutdown" scenarios, proven against
// the lease/fencing mechanism (SKIP LOCKED + lease_token) instead of NATS's
// AckWait/durable-consumer machinery.

import (
	"context"
	"testing"
	"time"
)

// chaosLeaseTTL is short enough to keep this test fast, but well clear of
// normal Postgres round-trip latency so the reclaim isn't a timing
// coincidence — mirroring chaos_test.go's chaosAckWait convention.
const chaosLeaseTTL = 2 * time.Second

// TestPostgresChaos_AbandonedLeaseRedeliversToADifferentConsumer proves the
// actual "pod crash mid-task" recovery story for queue_driver=postgres: a
// real killed process never gets to call Ack, Nak, or Extend — it just stops
// existing. That has to be indistinguishable, from the queue's point of
// view, from a delivery nobody ever touches again. This test abandons a
// popped delivery outright and confirms a second, wholly independent
// PostgresQueue instance ("pod B") can reclaim the same row once its lease
// expires — the mechanism task 7A's main.go wiring relies on to make
// skipping the stale-task DB cleanup safe under queue_driver=postgres too.
func TestPostgresChaos_AbandonedLeaseRedeliversToADifferentConsumer(t *testing.T) {
	url := requireDBURL(t)
	const queueName = "pgchaos-abandon"

	qA, err := NewPostgresQueue(url, queueName, chaosLeaseTTL, 5)
	if err != nil {
		t.Fatalf("NewPostgresQueue a: %v", err)
	}
	defer qA.Close()
	qB, err := NewPostgresQueue(url, queueName, chaosLeaseTTL, 5)
	if err != nil {
		t.Fatalf("NewPostgresQueue b: %v", err)
	}
	defer qB.Close()

	// Clean up any previous test data so this test is re-runnable (the
	// "chaos-1" id is a primary key and a prior leased/undone row would
	// otherwise collide with this run's Push).
	_, _ = qA.db.ExecContext(context.Background(), `DELETE FROM task_queue WHERE queue_name = $1`, queueName)

	if err := qA.Push(context.Background(), Item{TaskID: "chaos-1"}); err != nil {
		t.Fatalf("Push: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	_, delivery, err := qA.Pop(ctx) // qA claims it, then abandons — never Ack/Nak/Extend
	cancel()
	if err != nil {
		t.Fatalf("Pop on qA: %v", err)
	}
	// Simulate pod A crashing here: no Ack, no Nak, nothing — a killed
	// process never gets the chance to call either. delivery is captured
	// only so this intent (and the absence of any call on it) is explicit in
	// the test, not because anything below uses it.
	_ = delivery

	// Before the lease expires, qB must not be able to claim it.
	ctxEarly, cancelEarly := context.WithTimeout(context.Background(), 1*time.Second)
	_, _, err = qB.Pop(ctxEarly)
	cancelEarly()
	if err == nil {
		t.Fatal("qB claimed the item before the lease expired — reclaim happened too early")
	}

	// After the lease TTL elapses, qB must be able to claim it.
	ctxLate, cancelLate := context.WithTimeout(context.Background(), chaosLeaseTTL+2*time.Second)
	defer cancelLate()
	item, redelivery, err := qB.Pop(ctxLate)
	if err != nil {
		t.Fatalf("qB failed to reclaim the abandoned item after lease expiry: %v", err)
	}
	if item.TaskID != "chaos-1" {
		t.Fatalf("reclaimed TaskID = %q, want %q", item.TaskID, "chaos-1")
	}
	if err := redelivery.Ack(); err != nil {
		t.Fatalf("Ack after reclaim: %v", err)
	}
}

// TestPostgresChaos_CloseDoesNotDisruptAnotherInstancesLease pins the
// Postgres analogue of chaos_test.go's
// TestChaos_GracefulCloseByConsumerCreatorDoesNotDeleteSharedConsumer: closing
// one *PostgresQueue instance (the Postgres equivalent of a graceful pod
// shutdown — closing its own *sql.DB connection pool) must not disrupt
// another instance's in-flight lease on the same queue name. Unlike NATS,
// where every pod bound to a queue name shares one durable JetStream
// consumer's server-side state (so the wrong pod's Close() could delete it
// out from under everyone else), Postgres leases live entirely in the
// task_queue row itself — Close() only tears down the calling instance's own
// connection pool, so there is no shared session/lock state to release and
// no equivalent failure mode to reproduce here. This test proves that
// directly: qA.Close() must not make its still-leased row claimable by
// another instance before the lease actually expires.
func TestPostgresChaos_CloseDoesNotDisruptAnotherInstancesLease(t *testing.T) {
	url := requireDBURL(t)
	const queueName = "pgchaos-close"

	qA, err := NewPostgresQueue(url, queueName, 5*time.Minute, 5)
	if err != nil {
		t.Fatalf("NewPostgresQueue a: %v", err)
	}
	qC, err := NewPostgresQueue(url, queueName, 5*time.Minute, 5)
	if err != nil {
		t.Fatalf("NewPostgresQueue c: %v", err)
	}
	defer qC.Close()

	// Clean up any previous test data so this test is re-runnable.
	_, _ = qC.db.ExecContext(context.Background(), `DELETE FROM task_queue WHERE queue_name = $1`, queueName)

	if err := qA.Push(context.Background(), Item{TaskID: "chaos-2"}); err != nil {
		t.Fatalf("Push: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	_, delivery, err := qA.Pop(ctx) // qA claims it (simulating one pod)
	cancel()
	if err != nil {
		t.Fatalf("Pop on qA: %v", err)
	}

	// qA "gracefully shuts down" — closing its own DB connection pool must
	// not affect qA's already-claimed row (no shared lock/session state to
	// release, unlike NATS's durable-consumer-deletion bug this mirrors).
	if err := qA.Close(); err != nil {
		t.Fatalf("qA.Close: %v", err)
	}

	// A third instance polling the same queue name must NOT see chaos-2 as
	// claimable immediately (the lease qA held is still valid — Close()
	// doesn't release it), proving graceful shutdown doesn't corrupt
	// another connection's in-flight lease the way NATS's Unsubscribe() bug
	// used to (see docs/architecture.md's NATSQueue.Close() history).
	ctxCheck, cancelCheck := context.WithTimeout(context.Background(), 1*time.Second)
	_, _, err = qC.Pop(ctxCheck)
	cancelCheck()
	if err == nil {
		t.Fatal("qC claimed chaos-2 immediately after qA.Close() — the lease should still be held")
	}

	// The delivery obtained before Close() carries a reference back to qA
	// (postgresDelivery.Ack does d.q.db.Exec — d.q is the *PostgresQueue
	// that produced it), and qA.db is exactly the connection pool
	// qA.Close() just shut down. Unlike NATS, where Ack talks to the
	// server over a still-open connection regardless of which pod created
	// the subscription, here the delivery has no life independent of the
	// instance that popped it. So this Ack is expected to fail: that's not
	// a bug in the queue, it's a real shutdown-ordering constraint this
	// test documents — a real caller must Ack/Nak/Extend a delivery before
	// closing the instance that produced it, never after. We assert the
	// failure explicitly (rather than silently discarding the delivery)
	// so a future change that makes this start succeeding — or a change
	// that makes it hang or panic instead of erroring cleanly — is caught
	// here rather than discovered in production during a rolling restart.
	if err := delivery.Ack(); err == nil {
		t.Error("delivery.Ack() succeeded after qA.Close() — if this is now expected, update this test's comment and the shutdown-ordering guidance it documents")
	} else {
		t.Logf("delivery.Ack() after qA.Close() failed as expected (closed connection pool): %v", err)
	}
}
