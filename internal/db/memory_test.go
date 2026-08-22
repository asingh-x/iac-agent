package db

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestRepoIndex_MissReturnsNilNoError(t *testing.T) {
	store := NewMemoryStore()
	got, err := store.GetRepoIndex(context.Background(), "repo-1", "sha-1")
	if err != nil {
		t.Fatalf("GetRepoIndex: unexpected error %v", err)
	}
	if got != nil {
		t.Errorf("expected nil entry on cache miss, got %+v", got)
	}
}

func TestRepoIndex_SaveThenGet(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()
	summary := json.RawMessage(`{"resources":[{"type":"aws_s3_bucket","name":"main"}]}`)

	if err := store.SaveRepoIndex(ctx, "repo-1", "sha-1", summary); err != nil {
		t.Fatalf("SaveRepoIndex: %v", err)
	}

	got, err := store.GetRepoIndex(ctx, "repo-1", "sha-1")
	if err != nil {
		t.Fatalf("GetRepoIndex: %v", err)
	}
	if got == nil {
		t.Fatal("expected a cache hit, got nil")
	}
	if string(got.Summary) != string(summary) {
		t.Errorf("Summary = %s, want %s", got.Summary, summary)
	}

	// A different commit sha for the same repo must be a separate cache entry.
	miss, err := store.GetRepoIndex(ctx, "repo-1", "sha-2")
	if err != nil {
		t.Fatalf("GetRepoIndex (different sha): %v", err)
	}
	if miss != nil {
		t.Errorf("expected cache miss for a different commit sha, got %+v", miss)
	}
}

func TestRepoIndex_SaveOverwritesSameKey(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()

	_ = store.SaveRepoIndex(ctx, "repo-1", "sha-1", json.RawMessage(`{"v":1}`))
	_ = store.SaveRepoIndex(ctx, "repo-1", "sha-1", json.RawMessage(`{"v":2}`))

	got, err := store.GetRepoIndex(ctx, "repo-1", "sha-1")
	if err != nil {
		t.Fatalf("GetRepoIndex: %v", err)
	}
	if string(got.Summary) != `{"v":2}` {
		t.Errorf("Summary = %s, want the latest saved value", got.Summary)
	}
}

func TestMarkStaleTasksFailed_SkipsAlreadyCompletedTasks(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()

	stale, err := store.CreateTask(ctx, "user-1", "prompt", "stale task", "print")
	if err != nil {
		t.Fatalf("CreateTask (stale): %v", err)
	}
	if err := store.UpdateTaskStatus(ctx, stale.ID, "running"); err != nil {
		t.Fatalf("UpdateTaskStatus: %v", err)
	}

	completed, err := store.CreateTask(ctx, "user-1", "prompt", "completed task", "print")
	if err != nil {
		t.Fatalf("CreateTask (completed): %v", err)
	}
	// Simulate a task that finished successfully — status is terminal and
	// completed_at is set. In a correctly-implemented store this can't
	// simultaneously be "running", but the completed_at guard is defense in
	// depth against exactly this kind of inconsistent state.
	if err := store.UpdateTaskResult(ctx, completed.ID, "running", "", "", "output", 1, 1); err != nil {
		t.Fatalf("UpdateTaskResult: %v", err)
	}

	if err := store.MarkStaleTasksFailed(ctx); err != nil {
		t.Fatalf("MarkStaleTasksFailed: %v", err)
	}

	gotStale, err := store.GetTask(ctx, stale.ID)
	if err != nil {
		t.Fatalf("GetTask (stale): %v", err)
	}
	if gotStale.Status != "failed" {
		t.Errorf("stale task status = %q, want failed", gotStale.Status)
	}

	gotCompleted, err := store.GetTask(ctx, completed.ID)
	if err != nil {
		t.Fatalf("GetTask (completed): %v", err)
	}
	if gotCompleted.Status != "running" {
		t.Errorf("already-completed task status = %q, want unchanged (running) — must not retroactively fail a completed task", gotCompleted.Status)
	}
}

// backdateTaskForTest directly rewrites a task's created_at in the in-memory
// store, bypassing the Store interface (which has no setter for it, on
// purpose — created_at is set once at creation in real usage). This is the
// most direct way to simulate "genuinely old" for FailTasksOlderThan's age
// predicate: the test is in-package (package db) specifically so it can reach
// into memStore's internals like this.
func backdateTaskForTest(t *testing.T, store Store, taskID string, age time.Duration) {
	t.Helper()
	ms, ok := store.(*memStore)
	if !ok {
		t.Fatalf("backdateTaskForTest: store is %T, not *memStore", store)
	}
	ms.mu.Lock()
	defer ms.mu.Unlock()
	task, ok := ms.tasks[taskID]
	if !ok {
		t.Fatalf("backdateTaskForTest: no such task %q", taskID)
	}
	task.CreatedAt = time.Now().Add(-age)
}

// TestFailTasksOlderThan_OnlyReconcilesGenuinelyOldRows is the age-based
// reconciliation this closes a gap for (see the doc comment on
// FailTasksOlderThan in store.go): a task whose queue delivery is abandoned
// and redelivered until it exhausts the queue's max-delivery-attempts has no
// other automatic path back to a terminal DB status. The key property this
// pins is the one the old, now-removed, unconditional MarkStaleTasksFailed
// sweep got wrong: a row that is merely "currently non-terminal" must be left
// alone — only a row that has been non-terminal for an unusually long time
// gets reconciled.
func TestFailTasksOlderThan_OnlyReconcilesGenuinelyOldRows(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()
	const maxAge = 2 * time.Hour
	const errMsg = "reconciled: exceeded stale-task age threshold"

	// Genuinely old and stuck: created 3 hours ago (older than the 2h
	// threshold), still running. This is exactly the MaxDeliver-exhausted
	// scenario the reconciler exists for.
	old, err := store.CreateTask(ctx, "user-1", "prompt", "old stuck task", "print")
	if err != nil {
		t.Fatalf("CreateTask (old): %v", err)
	}
	if err := store.UpdateTaskStatus(ctx, old.ID, "running"); err != nil {
		t.Fatalf("UpdateTaskStatus (old): %v", err)
	}
	backdateTaskForTest(t, store, old.ID, 3*time.Hour)

	// Freshly created and running: must NOT be touched, no matter its status,
	// because it hasn't been non-terminal for anywhere near maxAge yet. This
	// is the exact case the old unconditional sweep got wrong (failing other
	// pods' genuinely in-flight tasks) and the age predicate exists to avoid.
	fresh, err := store.CreateTask(ctx, "user-1", "prompt", "fresh running task", "print")
	if err != nil {
		t.Fatalf("CreateTask (fresh): %v", err)
	}
	if err := store.UpdateTaskStatus(ctx, fresh.ID, "running"); err != nil {
		t.Fatalf("UpdateTaskStatus (fresh): %v", err)
	}

	// Old AND queued (not just running) — the predicate must cover all three
	// non-terminal statuses, not just "running".
	oldQueued, err := store.CreateTask(ctx, "user-1", "prompt", "old queued task", "print")
	if err != nil {
		t.Fatalf("CreateTask (old queued): %v", err)
	}
	backdateTaskForTest(t, store, oldQueued.ID, 5*time.Hour)

	// Old but already completed: must never be retroactively reconciled, same
	// defense-in-depth guard MarkStaleTasksFailed has via completed_at.
	oldCompleted, err := store.CreateTask(ctx, "user-1", "prompt", "old completed task", "print")
	if err != nil {
		t.Fatalf("CreateTask (old completed): %v", err)
	}
	if err := store.UpdateTaskResult(ctx, oldCompleted.ID, "done", "", "", "output", 1, 1); err != nil {
		t.Fatalf("UpdateTaskResult (old completed): %v", err)
	}
	backdateTaskForTest(t, store, oldCompleted.ID, 5*time.Hour)

	n, err := store.FailTasksOlderThan(ctx, maxAge, errMsg)
	if err != nil {
		t.Fatalf("FailTasksOlderThan: %v", err)
	}
	if n != 2 {
		t.Errorf("FailTasksOlderThan reconciled %d rows, want 2 (old running + old queued)", n)
	}

	gotOld, err := store.GetTask(ctx, old.ID)
	if err != nil {
		t.Fatalf("GetTask (old): %v", err)
	}
	if gotOld.Status != "failed" {
		t.Errorf("old task status = %q, want failed", gotOld.Status)
	}
	if gotOld.ErrorMsg != errMsg {
		t.Errorf("old task error_msg = %q, want %q", gotOld.ErrorMsg, errMsg)
	}

	gotFresh, err := store.GetTask(ctx, fresh.ID)
	if err != nil {
		t.Fatalf("GetTask (fresh): %v", err)
	}
	if gotFresh.Status != "running" {
		t.Errorf("fresh task status = %q, want unchanged (running) — a task that is simply currently non-terminal must never be touched by an age-based sweep", gotFresh.Status)
	}

	gotOldQueued, err := store.GetTask(ctx, oldQueued.ID)
	if err != nil {
		t.Fatalf("GetTask (old queued): %v", err)
	}
	if gotOldQueued.Status != "failed" {
		t.Errorf("old queued task status = %q, want failed", gotOldQueued.Status)
	}

	gotOldCompleted, err := store.GetTask(ctx, oldCompleted.ID)
	if err != nil {
		t.Fatalf("GetTask (old completed): %v", err)
	}
	if gotOldCompleted.Status != "done" {
		t.Errorf("old completed task status = %q, want unchanged (done) — must not retroactively fail an already-completed task", gotOldCompleted.Status)
	}
}
