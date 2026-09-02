//go:build integration

package queue

import (
	"context"
	"os"
	"testing"
	"time"
)

func requireDBURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("DB_URL")
	if url == "" {
		t.Skip("DB_URL not set — skipping postgres queue integration tests")
	}
	return url
}

func TestPostgresQueue_PushThenLen(t *testing.T) {
	url := requireDBURL(t)
	q, err := NewPostgresQueue(url, "pgqtest-push", 5*time.Minute, 5)
	if err != nil {
		t.Fatalf("NewPostgresQueue: %v", err)
	}
	defer q.Close()

	// Clean up any previous test data
	_, _ = q.db.ExecContext(context.Background(), `DELETE FROM task_queue WHERE queue_name = $1`, "pgqtest-push")

	if got := q.Len(); got != 0 {
		t.Fatalf("Len() before any push = %d, want 0", got)
	}
	if err := q.Push(context.Background(), Item{TaskID: "push-1"}); err != nil {
		t.Fatalf("Push: %v", err)
	}
	if got := q.Len(); got != 1 {
		t.Fatalf("Len() after one push = %d, want 1", got)
	}
}

func TestPostgresQueue_Pop_ClaimsExactlyOnceAcrossTwoPollers(t *testing.T) {
	url := requireDBURL(t)
	qA, err := NewPostgresQueue(url, "pgqtest-pop", 5*time.Minute, 5)
	if err != nil {
		t.Fatalf("NewPostgresQueue a: %v", err)
	}
	defer qA.Close()
	qB, err := NewPostgresQueue(url, "pgqtest-pop", 5*time.Minute, 5)
	if err != nil {
		t.Fatalf("NewPostgresQueue b: %v", err)
	}
	defer qB.Close()

	// Clean up any previous test data so this test is re-runnable (the
	// "pop-1" id is a primary key and a prior leased/undone row would
	// otherwise collide with this run's Push).
	_, _ = qA.db.ExecContext(context.Background(), `DELETE FROM task_queue WHERE queue_name = $1`, "pgqtest-pop")

	if err := qA.Push(context.Background(), Item{TaskID: "pop-1"}); err != nil {
		t.Fatalf("Push: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	item, _, err := qA.Pop(ctx)
	if err != nil {
		t.Fatalf("Pop on qA: %v", err)
	}
	if item.TaskID != "pop-1" {
		t.Fatalf("got TaskID %q, want %q", item.TaskID, "pop-1")
	}

	// A second poller must NOT see the same item again while it's leased.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel2()
	_, _, err = qB.Pop(ctx2)
	if err == nil {
		t.Fatal("expected qB.Pop to time out (item already leased by qA), got a result instead")
	}
}
