//go:build integration

package queue_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tf-agent/tf-agent/internal/queue"
)

// Run with: NATS_URL=nats://localhost:4222 go test -tags=integration ./internal/queue/...

func newNATSQueue(t *testing.T) *queue.NATSQueue {
	t.Helper()
	return newNATSQueueWithMaxMsgs(t, 0) // 0 -> queue.DefaultNATSMaxMsgs
}

// newNATSQueueWithMaxMsgs is like newNATSQueue but lets the caller override
// the stream's MaxMsgs backpressure limit (e.g. a small value so a
// backpressure test doesn't need to publish thousands of messages).
func newNATSQueueWithMaxMsgs(t *testing.T, maxMsgs int) *queue.NATSQueue {
	t.Helper()
	url := os.Getenv("NATS_URL")
	if url == "" {
		t.Skip("NATS_URL not set — skipping NATS integration tests")
	}
	// Use a sanitized test name as the queue name for isolation between test cases.
	name := "test-" + strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' {
			return r
		}
		return '-'
	}, t.Name())
	q, err := queue.NewNATSQueue(url, name, maxMsgs)
	if err != nil {
		t.Fatalf("NewNATSQueue: %v", err)
	}
	t.Cleanup(func() { _ = q.Close() })
	return q
}

func TestNATSQueue_PushPop(t *testing.T) {
	q := newNATSQueue(t)
	ctx := context.Background()

	item := queue.Item{
		TaskID:     "test-task-1",
		UserID:     "user-1",
		InputType:  "prompt",
		InputText:  "create an S3 bucket",
		OutputType: "print",
	}

	if err := q.Push(ctx, item); err != nil {
		t.Fatalf("Push: %v", err)
	}

	ctx2, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	got, _, err := q.Pop(ctx2)
	if err != nil {
		t.Fatalf("Pop: %v", err)
	}
	if got.TaskID != item.TaskID {
		t.Errorf("got TaskID %q, want %q", got.TaskID, item.TaskID)
	}
	if got.InputText != item.InputText {
		t.Errorf("got InputText %q, want %q", got.InputText, item.InputText)
	}
}

func TestNATSQueue_PopCancelledContext(t *testing.T) {
	q := newNATSQueue(t)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	_, _, err := q.Pop(ctx)
	if err == nil {
		t.Error("expected context error, got nil")
	}
}

func TestNATSQueue_MultipleItems(t *testing.T) {
	q := newNATSQueue(t)
	ctx := context.Background()

	items := []queue.Item{
		{TaskID: "multi-1", UserID: "u1", InputType: "prompt", InputText: "task 1", OutputType: "print"},
		{TaskID: "multi-2", UserID: "u1", InputType: "prompt", InputText: "task 2", OutputType: "print"},
		{TaskID: "multi-3", UserID: "u2", InputType: "prompt", InputText: "task 3", OutputType: "pr"},
	}

	for _, item := range items {
		if err := q.Push(ctx, item); err != nil {
			t.Fatalf("Push %s: %v", item.TaskID, err)
		}
	}

	seen := map[string]bool{}
	for range items {
		ctx2, cancel := context.WithTimeout(ctx, 5*time.Second)
		got, _, err := q.Pop(ctx2)
		cancel()
		if err != nil {
			t.Fatalf("Pop: %v", err)
		}
		seen[got.TaskID] = true
	}

	for _, item := range items {
		if !seen[item.TaskID] {
			t.Errorf("never received task %s", item.TaskID)
		}
	}
}

// TestNATSQueue_PushBackpressure verifies that once the shared stream's
// MaxMsgs limit is reached, Push starts returning an error instead of
// accepting publishes indefinitely (the backpressure mechanism added to
// NewNATSQueue via StreamConfig.MaxMsgs + Discard: nats.DiscardNew).
//
// Note: MaxMsgs bounds the TOTAL backlog on the shared TF_AGENT stream, not
// just this test's queue name/subject (see the doc comment on NewNATSQueue).
// Other tests in this file Pop messages without Acking them, which can leave
// a few unacked messages sitting in the shared stream and eating into this
// test's budget. So rather than asserting exactly maxMsgs pushes succeed
// before failure, this pushes in a loop (bounded generously above maxMsgs)
// until Push fails, then confirms the failure isn't a one-off blip.
func TestNATSQueue_PushBackpressure(t *testing.T) {
	const maxMsgs = 5
	q := newNATSQueueWithMaxMsgs(t, maxMsgs)
	ctx := context.Background()

	pushed := 0
	var pushErr error
	for i := 0; i < maxMsgs*4; i++ {
		item := queue.Item{
			TaskID:     fmt.Sprintf("backpressure-%d", i),
			UserID:     "u1",
			InputType:  "prompt",
			InputText:  "x",
			OutputType: "print",
		}
		if err := q.Push(ctx, item); err != nil {
			pushErr = err
			break
		}
		pushed++
	}
	if pushErr == nil {
		t.Fatalf("expected Push to eventually fail with backpressure (maxMsgs=%d) after %d successful pushes, got no error", maxMsgs, pushed)
	}
	t.Logf("backpressure triggered after %d successful pushes (maxMsgs=%d): %v", pushed, maxMsgs, pushErr)

	// Confirm the rejection persists — a real limit, not a transient blip.
	again := queue.Item{TaskID: "backpressure-again", InputType: "prompt", InputText: "x", OutputType: "print"}
	if err := q.Push(ctx, again); err == nil {
		t.Error("expected Push to still fail immediately after backpressure was triggered, got nil error")
	}
}

func TestNATSQueue_CredentialsNotPersisted(t *testing.T) {
	// GitHub/Atlassian tokens should survive the round-trip (they're in-process, not sensitive over wire in test)
	q := newNATSQueue(t)
	ctx := context.Background()

	item := queue.Item{
		TaskID:      "creds-test",
		GitHubToken: "gh-secret-token",
		RepoURL:     "https://github.com/org/repo",
	}
	if err := q.Push(ctx, item); err != nil {
		t.Fatalf("Push: %v", err)
	}
	ctx2, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	got, _, err := q.Pop(ctx2)
	if err != nil {
		t.Fatalf("Pop: %v", err)
	}
	if got.GitHubToken != item.GitHubToken {
		t.Errorf("got GitHubToken %q, want %q", got.GitHubToken, item.GitHubToken)
	}
}

// TestNATSQueue_Len_IsPerQueueNotWholeStream proves Len() reports this
// queue's own pending count, not the whole shared TF_AGENT stream's total
// — the bug this test guards against: two named queues used to report the
// identical (wrong, too-high) combined count from either one.
func TestNATSQueue_Len_IsPerQueueNotWholeStream(t *testing.T) {
	url := os.Getenv("NATS_URL")
	if url == "" {
		t.Skip("NATS_URL not set — skipping NATS integration tests")
	}

	// Use test name as base to ensure fresh consumers on each test run.
	baseName := "test-" + strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' {
			return r
		}
		return '-'
	}, t.Name())

	qA, err := queue.NewNATSQueue(url, baseName+"-a", queue.DefaultNATSMaxMsgs)
	if err != nil {
		t.Fatalf("NewNATSQueue a: %v", err)
	}
	defer qA.Close()
	qB, err := queue.NewNATSQueue(url, baseName+"-b", queue.DefaultNATSMaxMsgs)
	if err != nil {
		t.Fatalf("NewNATSQueue b: %v", err)
	}
	defer qB.Close()

	ctx := context.Background()
	if err := qA.Push(ctx, queue.Item{TaskID: "a1"}); err != nil {
		t.Fatalf("push a1: %v", err)
	}
	if err := qA.Push(ctx, queue.Item{TaskID: "a2"}); err != nil {
		t.Fatalf("push a2: %v", err)
	}
	if err := qB.Push(ctx, queue.Item{TaskID: "b1"}); err != nil {
		t.Fatalf("push b1: %v", err)
	}

	if got := qA.Len(); got != 2 {
		t.Errorf("qA.Len() = %d, want 2 (its own 2 pushed items, not the shared stream's 3)", got)
	}
	if got := qB.Len(); got != 1 {
		t.Errorf("qB.Len() = %d, want 1 (its own 1 pushed item, not the shared stream's 3)", got)
	}
}
