//go:build integration

package queue

import (
	"context"
	"fmt"
	"os"
	"sync"
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

// TestPostgresQueue_Pop_NoDoubleClaimUnderConcurrentPollers exercises the
// actual FOR UPDATE SKIP LOCKED contention path directly: unlike
// TestPostgresQueue_Pop_ClaimsExactlyOnceAcrossTwoPollers (where qA's claim
// fully commits before qB ever polls), here every worker is released through
// a shared start barrier so they genuinely race to claim the same batch of
// still-'queued' rows at the same instant. This is the property the whole
// task exists to guarantee: two concurrent pollers must never claim the same
// row.
func TestPostgresQueue_Pop_NoDoubleClaimUnderConcurrentPollers(t *testing.T) {
	url := requireDBURL(t)
	const queueName = "pgqtest-pop-concurrent"
	const nItems = 8
	const nWorkers = 20

	setup, err := NewPostgresQueue(url, queueName, 5*time.Minute, 5)
	if err != nil {
		t.Fatalf("NewPostgresQueue setup: %v", err)
	}
	defer setup.Close()

	// Clean up any previous test data so this test is re-runnable.
	_, _ = setup.db.ExecContext(context.Background(), `DELETE FROM task_queue WHERE queue_name = $1`, queueName)

	for i := 0; i < nItems; i++ {
		if err := setup.Push(context.Background(), Item{TaskID: fmt.Sprintf("pop-race-%d", i)}); err != nil {
			t.Fatalf("Push %d: %v", i, err)
		}
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	claimCount := map[string]int{} // TaskID -> number of times any worker claimed it

	for w := 0; w < nWorkers; w++ {
		q, err := NewPostgresQueue(url, queueName, 5*time.Minute, 5)
		if err != nil {
			t.Fatalf("NewPostgresQueue worker %d: %v", w, err)
		}
		defer q.Close()

		wg.Add(1)
		go func(q *PostgresQueue) {
			defer wg.Done()
			<-start // block until every worker is released together

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			item, _, err := q.Pop(ctx)
			if err != nil {
				return // no eligible row left for this worker — expected once nItems are claimed
			}
			mu.Lock()
			claimCount[item.TaskID]++
			mu.Unlock()
		}(q)
	}

	close(start) // release all goroutines at once so they race for the same rows
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	total := 0
	for id, count := range claimCount {
		total += count
		if count != 1 {
			t.Fatalf("task %q claimed %d times, want exactly 1 — DOUBLE CLAIM DETECTED", id, count)
		}
	}
	if len(claimCount) != nItems || total != nItems {
		t.Fatalf("claimed %d distinct items (%d total claims), want %d items claimed exactly once each; claimCount=%v", len(claimCount), total, nItems, claimCount)
	}
}
