package main

import (
	"context"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tf-agent/tf-agent/internal/config"
	"github.com/tf-agent/tf-agent/internal/db"
)

// TestShouldMarkStaleTasksFailed pins the exact predicate main() consults
// before calling store.MarkStaleTasksFailed at startup. Getting this
// backwards would silently reintroduce the bug it exists to prevent: under
// queue_driver=nats, a rolling restart or HPA scale-up of one pod would mark
// OTHER pods' genuinely in-flight tasks as failed (see the doc comment on
// shouldMarkStaleTasksFailed in main.go for the full reasoning).
func TestShouldMarkStaleTasksFailed(t *testing.T) {
	cases := []struct {
		queueDriver string
		want        bool
	}{
		{"nats", false},  // durable queue: NATS redelivery + isTerminalStatus handle recovery
		{"memory", true}, // in-memory queue: a restart genuinely loses in-flight work
		{"", true},       // empty resolves to memory downstream (the "--- Queue ---" switch's default case)
		{"bogus", true},  // any other unrecognized value also resolves to memory downstream
	}

	for _, tc := range cases {
		if got := shouldMarkStaleTasksFailed(tc.queueDriver); got != tc.want {
			t.Errorf("shouldMarkStaleTasksFailed(%q) = %v, want %v", tc.queueDriver, got, tc.want)
		}
	}
}

// TestShouldMarkStaleTasksFailed_PinsThe7AFixEndToEnd is the store-level
// proof that actually pins 7A's correctness (task7-resilience-brief.md, 7B
// step 5): a task row genuinely still "running" — simulating a pod that is
// still alive and mid-task while THIS pod is the one restarting — must
// survive untouched when queue_driver=nats, and must be reconciled to
// "failed" when it doesn't (memory, and the resolved default). No NATS or
// Postgres needed: this is exercising the exact gate main() puts in front of
// store.MarkStaleTasksFailed, not the queue mechanics themselves (see
// internal/queue/chaos_test.go for the NATS redelivery half of this story).
func TestShouldMarkStaleTasksFailed_PinsThe7AFixEndToEnd(t *testing.T) {
	ctx := context.Background()

	t.Run("nats: cleanup skipped, running row survives untouched", func(t *testing.T) {
		store := db.NewMemoryStore()
		task, err := store.CreateTask(ctx, "user-1", "prompt", "provision a vpc", "print")
		if err != nil {
			t.Fatalf("CreateTask: %v", err)
		}
		if err := store.UpdateTaskStatus(ctx, task.ID, "running"); err != nil {
			t.Fatalf("UpdateTaskStatus: %v", err)
		}

		if shouldMarkStaleTasksFailed("nats") {
			t.Fatal("shouldMarkStaleTasksFailed(\"nats\") = true, want false")
		}
		// main() only calls store.MarkStaleTasksFailed when the helper above
		// returns true, so under nats that call simply never happens — which
		// is exactly what this asserts by never calling it here either.

		got, err := store.GetTask(ctx, task.ID)
		if err != nil {
			t.Fatalf("GetTask: %v", err)
		}
		if got.Status != "running" {
			t.Errorf("task status = %q, want unchanged %q — a task genuinely still running on another pod must not be touched", got.Status, "running")
		}
	})

	for _, driver := range []string{"memory", ""} {
		driver := driver
		t.Run("driver="+driver+": cleanup runs, running row is reconciled to failed", func(t *testing.T) {
			store := db.NewMemoryStore()
			task, err := store.CreateTask(ctx, "user-1", "prompt", "provision a vpc", "print")
			if err != nil {
				t.Fatalf("CreateTask: %v", err)
			}
			if err := store.UpdateTaskStatus(ctx, task.ID, "running"); err != nil {
				t.Fatalf("UpdateTaskStatus: %v", err)
			}

			if !shouldMarkStaleTasksFailed(driver) {
				t.Fatalf("shouldMarkStaleTasksFailed(%q) = false, want true", driver)
			}
			if err := store.MarkStaleTasksFailed(ctx); err != nil {
				t.Fatalf("MarkStaleTasksFailed: %v", err)
			}

			got, err := store.GetTask(ctx, task.ID)
			if err != nil {
				t.Fatalf("GetTask: %v", err)
			}
			if got.Status != "failed" {
				t.Errorf("driver=%q: task status = %q, want failed — a single-process restart really does lose this task", driver, got.Status)
			}
		})
	}
}

// recordingStore wraps a real Store and records every FailTasksOlderThan
// call (the maxAge it was called with) onto a channel, so a test can observe
// runStaleTaskReconciler actually invoking it without needing to race real
// wall-clock task aging.
type recordingStore struct {
	db.Store
	calls   int32
	maxAges chan time.Duration
}

func (r *recordingStore) FailTasksOlderThan(ctx context.Context, maxAge time.Duration, errMsg string) (int, error) {
	atomic.AddInt32(&r.calls, 1)
	select {
	case r.maxAges <- maxAge:
	default:
	}
	return r.Store.FailTasksOlderThan(ctx, maxAge, errMsg)
}

// TestRunStaleTaskReconciler_RunsPeriodicallyAndStopsOnCancel pins the two
// load-bearing properties of the QW4 reconciler goroutine: it actually calls
// store.FailTasksOlderThan on the configured interval with the configured
// max age (not just once at startup, and not with the wrong value), and it
// stops cleanly when its context is cancelled — the same shutdown contract
// the queue pop loops in main() rely on, so the process can exit rather than
// leak a goroutine forever.
func TestRunStaleTaskReconciler_RunsPeriodicallyAndStopsOnCancel(t *testing.T) {
	origInterval := staleTaskReconcileInterval
	staleTaskReconcileInterval = 10 * time.Millisecond
	t.Cleanup(func() { staleTaskReconcileInterval = origInterval })

	store := &recordingStore{Store: db.NewMemoryStore(), maxAges: make(chan time.Duration, 8)}
	cfg := config.Defaults()
	cfg.Server.StaleTaskMaxAge = 3600 // 1 hour; arbitrary, just needs to round-trip through

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runStaleTaskReconciler(ctx, store, cfg, slog.Default())
		close(done)
	}()

	select {
	case got := <-store.maxAges:
		want := time.Duration(cfg.Server.StaleTaskMaxAge) * time.Second
		if got != want {
			t.Errorf("FailTasksOlderThan called with maxAge = %v, want %v", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runStaleTaskReconciler did not call FailTasksOlderThan within 2s of starting")
	}

	// A second tick should follow — this is periodic, not one-shot.
	select {
	case <-store.maxAges:
	case <-time.After(2 * time.Second):
		t.Fatal("runStaleTaskReconciler did not call FailTasksOlderThan a second time — expected periodic, not one-shot")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runStaleTaskReconciler did not return after its context was cancelled — goroutine leak on shutdown")
	}
}

// TestRunStaleTaskReconciler_NonPositiveConfigFallsBackToDefault pins the
// zero/negative-config fallback: a misconfigured (or simply unset)
// stale_task_max_age must not turn into a maxAge of zero, which would make
// every non-terminal task fair game on the very next tick — reintroducing
// exactly the cross-pod false-failure hazard this age-based approach exists
// to avoid.
func TestRunStaleTaskReconciler_NonPositiveConfigFallsBackToDefault(t *testing.T) {
	origInterval := staleTaskReconcileInterval
	staleTaskReconcileInterval = 10 * time.Millisecond
	t.Cleanup(func() { staleTaskReconcileInterval = origInterval })

	store := &recordingStore{Store: db.NewMemoryStore(), maxAges: make(chan time.Duration, 8)}
	cfg := config.Defaults()
	cfg.Server.StaleTaskMaxAge = 0

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runStaleTaskReconciler(ctx, store, cfg, slog.Default())
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	select {
	case got := <-store.maxAges:
		if got != 2*time.Hour {
			t.Errorf("FailTasksOlderThan called with maxAge = %v, want the 2h fallback default", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runStaleTaskReconciler did not call FailTasksOlderThan within 2s of starting")
	}
}
