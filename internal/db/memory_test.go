package db

import (
	"context"
	"encoding/json"
	"testing"
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
