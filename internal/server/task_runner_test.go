package server

import (
	"context"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/tf-agent/tf-agent/internal/db"
	"github.com/tf-agent/tf-agent/internal/queue"
)

// fakeDelivery records Ack/Nak/Extend calls for assertions.
type fakeDelivery struct {
	acked, naked, extended int
}

func (f *fakeDelivery) Ack() error    { f.acked++; return nil }
func (f *fakeDelivery) Nak() error    { f.naked++; return nil }
func (f *fakeDelivery) Extend() error { f.extended++; return nil }

// failingStore wraps a real Store but forces UpdateTaskResult to fail, to
// exercise the "ack only after persistence" nak path.
type failingStore struct {
	db.Store
	failUpdateResult bool
}

func (f *failingStore) UpdateTaskResult(ctx context.Context, id, status, prURL, errorMsg, output string, inputTokens, outputTokens int) error {
	if f.failUpdateResult {
		return fmt.Errorf("simulated persistence failure")
	}
	return f.Store.UpdateTaskResult(ctx, id, status, prURL, errorMsg, output, inputTokens, outputTokens)
}

func TestIsTerminalStatus(t *testing.T) {
	terminal := []string{"done", "failed", "cancelled"}
	nonTerminal := []string{"queued", "running", "waiting_for_input", "", "bogus"}

	for _, s := range terminal {
		if !isTerminalStatus(s) {
			t.Errorf("isTerminalStatus(%q) = false, want true", s)
		}
	}
	for _, s := range nonTerminal {
		if isTerminalStatus(s) {
			t.Errorf("isTerminalStatus(%q) = true, want false", s)
		}
	}
}

func TestPersistFinalResult_AcksOnSuccess(t *testing.T) {
	store := db.NewMemoryStore()
	task, err := store.CreateTask(context.Background(), "user-1", "prompt", "do a thing", "print")
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	r := &Runner{store: store, logger: slog.Default()}
	delivery := &fakeDelivery{}

	r.persistFinalResult(context.Background(), delivery, task.ID, "done", "", "", "output", 10, 20)

	if delivery.acked != 1 {
		t.Errorf("acked = %d, want 1", delivery.acked)
	}
	if delivery.naked != 0 {
		t.Errorf("naked = %d, want 0", delivery.naked)
	}

	got, err := store.GetTask(context.Background(), task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Status != "done" {
		t.Errorf("task status = %q, want done", got.Status)
	}
}

func TestPersistFinalResult_NaksOnStoreFailure(t *testing.T) {
	real := db.NewMemoryStore()
	task, err := real.CreateTask(context.Background(), "user-1", "prompt", "do a thing", "print")
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	store := &failingStore{Store: real, failUpdateResult: true}

	r := &Runner{store: store, logger: slog.Default()}
	delivery := &fakeDelivery{}

	r.persistFinalResult(context.Background(), delivery, task.ID, "done", "", "", "output", 10, 20)

	if delivery.naked != 1 {
		t.Errorf("naked = %d, want 1 — a failed DB write must not ack the queue message", delivery.naked)
	}
	if delivery.acked != 0 {
		t.Errorf("acked = %d, want 0 when persistence failed", delivery.acked)
	}
}

func TestRun_SkipsRedeliveredTerminalTask(t *testing.T) {
	store := db.NewMemoryStore()
	task, err := store.CreateTask(context.Background(), "user-1", "prompt", "do a thing", "print")
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	// Simulate the task already having completed successfully before this
	// (re)delivery arrived — status is terminal.
	if err := store.UpdateTaskResult(context.Background(), task.ID, "done", "", "", "already done", 5, 5); err != nil {
		t.Fatalf("UpdateTaskResult: %v", err)
	}

	r := &Runner{store: store, logger: slog.Default()}
	delivery := &fakeDelivery{}

	// r.run must return immediately (no wireAgent/provider needed) and ack
	// without re-executing or mutating the already-final task result.
	r.run(context.Background(), queue.Item{TaskID: task.ID, UserID: "user-1"}, delivery)

	if delivery.acked != 1 {
		t.Errorf("acked = %d, want 1 for an already-terminal redelivered task", delivery.acked)
	}

	got, err := store.GetTask(context.Background(), task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Output != "already done" {
		t.Errorf("task output = %q, want unchanged %q — redelivery must not re-run the task", got.Output, "already done")
	}
}

// TestStartQueue_ShutdownRace is a regression test for a real data race
// caught by `go test -race`: sync.WaitGroup forbids a concurrent Add racing a
// Wait when the counter could be zero, and StartQueue's per-item
// inFlight.Add(1) could race with Shutdown's inFlight.Wait() if Shutdown was
// invoked immediately after cancelling StartQueue's context (a real
// production sequence: cancel, then shut down). Run with -race.
func TestStartQueue_ShutdownRace(t *testing.T) {
	store := db.NewMemoryStore()
	q := queue.NewMemoryQueue(1000)
	ctx := context.Background()

	const n = 500
	for i := 0; i < n; i++ {
		task, err := store.CreateTask(ctx, "user-1", "prompt", "x", "print")
		if err != nil {
			t.Fatalf("CreateTask: %v", err)
		}
		// Mark it terminal immediately so run() takes the fast skip-path
		// with no real agent/provider needed — this test is purely about
		// the dispatch/shutdown race, not task execution.
		if err := store.UpdateTaskResult(ctx, task.ID, "done", "", "", "", 0, 0); err != nil {
			t.Fatalf("UpdateTaskResult: %v", err)
		}
		if err := q.Push(ctx, queue.Item{TaskID: task.ID, UserID: "user-1"}); err != nil {
			t.Fatalf("Push: %v", err)
		}
	}

	r := &Runner{store: store, logger: slog.Default(), cancels: map[string]context.CancelFunc{}}

	loopCtx, cancel := context.WithCancel(ctx)
	go r.StartQueue(loopCtx, q)

	// Cancel almost immediately so dispatch is still actively racing with
	// the cancellation — the tight window this test exists to cover.
	time.Sleep(time.Millisecond)
	cancel()

	shutCtx, shutCancel := context.WithTimeout(ctx, 5*time.Second)
	defer shutCancel()
	r.Shutdown(shutCtx)
}

func TestRunner_Shutdown_ReturnsImmediatelyWhenIdle(t *testing.T) {
	r := &Runner{logger: slog.Default(), cancels: map[string]context.CancelFunc{}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	start := time.Now()
	r.Shutdown(ctx)
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Errorf("Shutdown took %v with no in-flight work, want near-instant", elapsed)
	}
}

func TestRunner_Shutdown_WaitsForInFlightTaskToFinish(t *testing.T) {
	r := &Runner{logger: slog.Default(), cancels: map[string]context.CancelFunc{}}
	r.inFlight.Add(1)
	go func() {
		time.Sleep(100 * time.Millisecond)
		r.inFlight.Done()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	start := time.Now()
	r.Shutdown(ctx)
	elapsed := time.Since(start)
	if elapsed < 100*time.Millisecond {
		t.Errorf("Shutdown returned after %v — should have waited for the in-flight task to finish", elapsed)
	}
	if elapsed > time.Second {
		t.Errorf("Shutdown took %v — the in-flight task finished well within the grace period", elapsed)
	}
}

func TestRunner_Shutdown_ForceCancelsAfterGracePeriodExpires(t *testing.T) {
	r := &Runner{logger: slog.Default(), cancels: map[string]context.CancelFunc{}}

	taskCtx, taskCancel := context.WithCancel(context.Background())
	r.cancelMu.Lock()
	r.cancels["task-1"] = taskCancel
	r.cancelMu.Unlock()

	cancelled := make(chan struct{})
	r.inFlight.Add(1)
	go func() {
		defer r.inFlight.Done()
		<-taskCtx.Done()
		close(cancelled)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	r.Shutdown(ctx)

	select {
	case <-cancelled:
	default:
		t.Error("expected the in-flight task's context to be cancelled once the grace period expired")
	}
}
