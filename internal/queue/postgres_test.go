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
