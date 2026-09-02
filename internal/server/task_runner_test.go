package server

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tf-agent/tf-agent/internal/config"
	"github.com/tf-agent/tf-agent/internal/db"
	"github.com/tf-agent/tf-agent/internal/llm"
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

// --- C1: the cross-pod control relay must never be called with an unbounded
// context ---

// blockingControlRelay models NATSControlRelay.request's real behavior for a
// task no pod owns: NATS has interest on "tf.control.*" from every pod, so
// ErrNoResponders never fires and the request blocks until its context is
// done. It also records whether each call site supplied a deadline at all.
type blockingControlRelay struct {
	mu           sync.Mutex
	deadlineSeen map[string]bool
}

func newBlockingControlRelay() *blockingControlRelay {
	return &blockingControlRelay{deadlineSeen: map[string]bool{}}
}

func (b *blockingControlRelay) block(kind string, ctx context.Context) error {
	_, hasDeadline := ctx.Deadline()
	b.mu.Lock()
	b.deadlineSeen[kind] = hasDeadline
	b.mu.Unlock()
	<-ctx.Done() // an unbounded context parks here forever
	return fmt.Errorf("relay request timed out: %w", ctx.Err())
}

func (b *blockingControlRelay) hadDeadline(kind string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.deadlineSeen[kind]
}

func (b *blockingControlRelay) RequestAnswer(ctx context.Context, _, _ string) error {
	return b.block("answer", ctx)
}

func (b *blockingControlRelay) RequestPermission(ctx context.Context, _ string, _ bool) error {
	return b.block("permission", ctx)
}

func (b *blockingControlRelay) RequestCancel(ctx context.Context, _ string) error {
	return b.block("cancel", ctx)
}

// newRelayTestRunner builds the minimal Runner needed to exercise the three
// public control methods' relay-fallback paths.
func newRelayTestRunner(relay ControlRelay) *Runner {
	return &Runner{
		logger:      slog.Default(),
		relay:       relay,
		answers:     map[string]chan string{},
		permissions: map[string]chan bool{},
		cancels:     map[string]context.CancelFunc{},
	}
}

func TestRunner_RelayFallback_IsBoundedByTimeout(t *testing.T) {
	orig := controlRelayTimeout
	controlRelayTimeout = 150 * time.Millisecond
	t.Cleanup(func() { controlRelayTimeout = orig })

	cases := []struct {
		kind string
		call func(*Runner) error
	}{
		{"answer", func(r *Runner) error { return r.SendAnswer("nobody-owns", "hi") }},
		{"permission", func(r *Runner) error { return r.SendPermissionResponse("nobody-owns", true) }},
		{"cancel", func(r *Runner) error { return r.CancelTask("nobody-owns") }},
	}

	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			relay := newBlockingControlRelay()
			r := newRelayTestRunner(relay)

			done := make(chan error, 1)
			start := time.Now()
			go func() { done <- tc.call(r) }()

			select {
			case err := <-done:
				if err == nil {
					t.Fatalf("expected an error when no pod owns the task")
				}
				if elapsed := time.Since(start); elapsed > time.Second {
					t.Errorf("returned after %v, want ~%v — the relay call is not bounded", elapsed, controlRelayTimeout)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("relay fallback never returned — the relay call is unbounded (C1 regression)")
			}

			if !relay.hadDeadline(tc.kind) {
				t.Errorf("relay.Request%s was called with a context that has no deadline", tc.kind)
			}
		})
	}
}

// TestRunner_LocalChannelFullDoesNotHitRelay proves a non-errNotOwned local
// failure (the answer channel being full on the pod that genuinely owns the
// task) passes straight through unchanged, instead of being misread as "some
// other pod must own this" and forwarded over the relay.
func TestRunner_LocalChannelFullDoesNotHitRelay(t *testing.T) {
	relay := newBlockingControlRelay() // would hang the test if ever reached
	r := newRelayTestRunner(relay)

	full := make(chan string, 1)
	full <- "already queued"
	r.answers["owned-task"] = full

	err := r.SendAnswer("owned-task", "second answer")
	if err == nil {
		t.Fatal("expected an error when the local answer channel is full")
	}
	if !strings.Contains(err.Error(), "answer channel full") {
		t.Errorf("err = %v, want the original 'answer channel full' error passed through unchanged", err)
	}
	if relay.hadDeadline("answer") {
		t.Error("a full local channel must not trigger the cross-pod relay fallback")
	}

	fullPerm := make(chan bool, 1)
	fullPerm <- true
	r.permissions["owned-task"] = fullPerm

	err = r.SendPermissionResponse("owned-task", false)
	if err == nil {
		t.Fatal("expected an error when the local permission channel is full")
	}
	if !strings.Contains(err.Error(), "permission channel full") {
		t.Errorf("err = %v, want the original 'permission channel full' error passed through unchanged", err)
	}
	if relay.hadDeadline("permission") {
		t.Error("a full local permission channel must not trigger the cross-pod relay fallback")
	}
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

// --- A2: bounded semaphore wait — a task must fail cleanly instead of
// blocking the queue worker goroutine forever when concurrency is saturated.

// newSemaphoreTestRunner builds the minimal Runner needed to exercise run()
// end to end without a real LLM provider: a mock provider that emits one
// text event then stops.
func newSemaphoreTestRunner(store db.Store, cfg *config.Config, semTimeout time.Duration) *Runner {
	mockProvider := llm.NewMockProvider("mock", []llm.Event{
		{Type: llm.EventText, Delta: "ok"},
		{Type: llm.EventStop, StopReason: "end_turn"},
	})
	return &Runner{
		store:            store,
		logger:           slog.Default(),
		hub:              NewHub(),
		provider:         mockProvider,
		cfg:              cfg,
		sem:              make(chan struct{}, 5),
		userSems:         map[string]chan struct{}{},
		userSemLastUsed:  map[string]time.Time{},
		userSemPaused:    map[string]int{},
		cancels:          map[string]context.CancelFunc{},
		answers:          map[string]chan string{},
		permissions:      map[string]chan bool{},
		semaphoreTimeout: semTimeout,
	}
}

func TestRun_GlobalSemaphoreTimeout_FailsCleanly(t *testing.T) {
	store := db.NewMemoryStore()
	task, err := store.CreateTask(context.Background(), "user-1", "prompt", "x", "print")
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	cfg := config.Defaults()
	cfg.Server.PerUserConcurrency = 0 // isolate this test to the global semaphore only
	r := newSemaphoreTestRunner(store, cfg, 50*time.Millisecond)
	r.sem = make(chan struct{}, 1)
	r.sem <- struct{}{} // saturate the only slot

	delivery := &fakeDelivery{}
	start := time.Now()
	r.run(context.Background(), queue.Item{TaskID: task.ID, UserID: "user-1"}, delivery)
	elapsed := time.Since(start)

	if elapsed > 2*time.Second {
		t.Errorf("run() took %v, want it to give up around the 50ms semaphore timeout instead of blocking indefinitely", elapsed)
	}
	if delivery.acked != 1 {
		t.Errorf("acked = %d, want 1 — a timed-out task must still be persisted+acked, not left stuck", delivery.acked)
	}
	if delivery.naked != 0 {
		t.Errorf("naked = %d, want 0", delivery.naked)
	}
	got, err := store.GetTask(context.Background(), task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Status != "failed" {
		t.Errorf("task status = %q, want failed", got.Status)
	}
	if !strings.Contains(got.ErrorMsg, "capacity") {
		t.Errorf("error_msg = %q, want it to mention capacity/timeout", got.ErrorMsg)
	}
}

func TestRun_PerUserSemaphoreTimeout_FailsCleanly(t *testing.T) {
	store := db.NewMemoryStore()
	task, err := store.CreateTask(context.Background(), "user-1", "prompt", "x", "print")
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	cfg := config.Defaults()
	cfg.Server.PerUserConcurrency = 1
	r := newSemaphoreTestRunner(store, cfg, 50*time.Millisecond)
	r.userSems["user-1"] = make(chan struct{}, 1)
	r.userSems["user-1"] <- struct{}{} // saturate this user's only slot; global sem stays wide open

	delivery := &fakeDelivery{}
	start := time.Now()
	r.run(context.Background(), queue.Item{TaskID: task.ID, UserID: "user-1"}, delivery)
	elapsed := time.Since(start)

	if elapsed > 2*time.Second {
		t.Errorf("run() took %v, want it to give up around the 50ms semaphore timeout instead of blocking indefinitely", elapsed)
	}
	if delivery.acked != 1 {
		t.Errorf("acked = %d, want 1 — a timed-out task must still be persisted+acked, not left stuck", delivery.acked)
	}
	got, err := store.GetTask(context.Background(), task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Status != "failed" {
		t.Errorf("task status = %q, want failed", got.Status)
	}
	if !strings.Contains(got.ErrorMsg, "capacity") {
		t.Errorf("error_msg = %q, want it to mention capacity/timeout", got.ErrorMsg)
	}
	// The global semaphore must have been released again (run() returned
	// before ever reaching it, since the per-user wait times out first).
	if used := len(r.sem); used != 0 {
		t.Errorf("global semaphore used = %d, want 0 — run() should never have reached it", used)
	}
}

// TestRun_SemaphoreTimeout_AcquiresFreedSlotInstead proves the timeout branch
// doesn't fire spuriously: when a slot frees up well within the timeout
// window, run() proceeds normally (through to a real, if mocked, task
// execution) instead of waiting out the full timeout.
func TestRun_SemaphoreTimeout_AcquiresFreedSlotInstead(t *testing.T) {
	store := db.NewMemoryStore()
	task, err := store.CreateTask(context.Background(), "user-1", "prompt", "x", "print")
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	cfg := config.Defaults()
	cfg.Server.PerUserConcurrency = 0
	cfg.Agent.MaxTurns = 2
	r := newSemaphoreTestRunner(store, cfg, 5*time.Second) // generous timeout the test must not wait out
	r.sem = make(chan struct{}, 1)
	r.sem <- struct{}{} // saturate briefly

	go func() {
		time.Sleep(50 * time.Millisecond)
		<-r.sem // free the slot well before the 5s timeout
	}()

	delivery := &fakeDelivery{}
	start := time.Now()
	r.run(context.Background(), queue.Item{TaskID: task.ID, UserID: "user-1"}, delivery)
	elapsed := time.Since(start)

	if elapsed > 3*time.Second {
		t.Errorf("run() took %v after the slot freed at ~50ms — looks like it waited out the full semaphore timeout instead of acquiring the freed slot", elapsed)
	}
	got, err := store.GetTask(context.Background(), task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if strings.Contains(got.ErrorMsg, "capacity") {
		t.Errorf("task failed with a capacity-timeout error even though a slot freed up before the timeout: %q", got.ErrorMsg)
	}
	if got.Status != "done" {
		t.Errorf("task status = %q, want done (mock provider completes a single turn cleanly)", got.Status)
	}
}

// --- A3: per-user semaphore map cleanup ---

func TestSweepIdleUserSemaphores_RemovesOnlyIdleAndEmptyEntries(t *testing.T) {
	r := &Runner{
		logger:          slog.Default(),
		userSems:        map[string]chan struct{}{},
		userSemLastUsed: map[string]time.Time{},
	}
	now := time.Now()

	// Idle and empty: eligible for removal.
	r.userSems["idle-empty"] = make(chan struct{}, 1)
	r.userSemLastUsed["idle-empty"] = now.Add(-2 * userSemIdleThreshold)

	// Idle by timestamp, but a task currently holds the slot: must survive.
	idleInUse := make(chan struct{}, 1)
	idleInUse <- struct{}{}
	r.userSems["idle-in-use"] = idleInUse
	r.userSemLastUsed["idle-in-use"] = now.Add(-2 * userSemIdleThreshold)

	// Recently used: must survive.
	r.userSems["recent"] = make(chan struct{}, 1)
	r.userSemLastUsed["recent"] = now

	removed := r.sweepIdleUserSemaphores(now)
	if removed != 1 {
		t.Errorf("removed = %d, want 1", removed)
	}
	if _, ok := r.userSems["idle-empty"]; ok {
		t.Error("idle-empty entry should have been removed")
	}
	if _, ok := r.userSemLastUsed["idle-empty"]; ok {
		t.Error("idle-empty timestamp should have been removed alongside its channel")
	}
	if _, ok := r.userSems["idle-in-use"]; !ok {
		t.Error("idle-in-use entry (slot currently held) must not be removed")
	}
	if _, ok := r.userSems["recent"]; !ok {
		t.Error("recently-used entry must not be removed")
	}
}

// TestSweepIdleUserSemaphores_KeepsEntryWithOpenPause covers the invariant
// that releasing concurrency slots during an ask_user/permission pause broke.
// The sweeper's original "an in-use entry always has a recent timestamp"
// justification held only while a running task kept a token in its channel
// for its whole lifetime. A paused task now hands that token back, so its
// user's entry reads len(ch) == 0 while the task is still very much using
// the slot conceptually — and a pause can run for WaitForInputTimeout (7
// days by default), many times userSemIdleThreshold. Reaping the entry there
// would let the user's next task create a fresh, full-capacity channel and
// run one task more than PerUserConcurrency permits.
func TestSweepIdleUserSemaphores_KeepsEntryWithOpenPause(t *testing.T) {
	r := &Runner{
		logger:          slog.Default(),
		userSems:        map[string]chan struct{}{},
		userSemLastUsed: map[string]time.Time{},
		userSemPaused:   map[string]int{},
	}
	now := time.Now()

	// A task for this user is paused: it released its token (so the channel
	// is empty, indistinguishable from unused by len alone) and its last
	// dispatch was long enough ago to look idle.
	paused := make(chan struct{}, 1)
	r.userSems["paused"] = paused
	r.userSemLastUsed["paused"] = now.Add(-2 * userSemIdleThreshold)
	r.userSemPaused["paused"] = 1

	// Control: same shape, no pause open — must still be reaped, so this
	// test can't pass by simply never reaping anything.
	r.userSems["idle-empty"] = make(chan struct{}, 1)
	r.userSemLastUsed["idle-empty"] = now.Add(-2 * userSemIdleThreshold)

	if removed := r.sweepIdleUserSemaphores(now); removed != 1 {
		t.Errorf("removed = %d, want 1 (only the entry with no open pause)", removed)
	}
	if got, ok := r.userSems["paused"]; !ok {
		t.Fatal("a user with an open pause had their semaphore entry reaped — their next task would get a fresh full-capacity channel and exceed PerUserConcurrency")
	} else if got != paused {
		t.Error("the paused user's channel was replaced; the paused task will reacquire against the original one")
	}
	if _, ok := r.userSemLastUsed["paused"]; !ok {
		t.Error("the paused user's timestamp was reaped even though their channel survived")
	}
	if _, ok := r.userSems["idle-empty"]; ok {
		t.Error("the genuinely idle entry should still have been removed")
	}

	// Once the pause ends the entry becomes reapable again, so the fix
	// doesn't just pin entries forever.
	delete(r.userSemPaused, "paused")
	if removed := r.sweepIdleUserSemaphores(now); removed != 1 {
		t.Errorf("removed = %d after the pause ended, want 1", removed)
	}
	if _, ok := r.userSems["paused"]; ok {
		t.Error("entry should be reapable again once no pause is open")
	}
}

// TestPauseGate_OpenPauseProtectsUserSemaphoreFromSweep is the same guarantee
// driven through pauseGate's real release/reacquire path (the code Runner.run
// wires into both pause sites) rather than by poking userSemPaused directly:
// the act of pausing must be what protects the entry, and the act of resuming
// must be what un-protects it.
func TestPauseGate_OpenPauseProtectsUserSemaphoreFromSweep(t *testing.T) {
	const userID = "user-1"

	r := &Runner{
		logger:          slog.Default(),
		sem:             make(chan struct{}, 1),
		userSems:        map[string]chan struct{}{},
		userSemLastUsed: map[string]time.Time{},
		userSemPaused:   map[string]int{},
	}

	// Stand in for run()'s per-user semaphore section: create the entry,
	// stamp it, and take this task's one slot.
	userSem := make(chan struct{}, 1)
	r.userSems[userID] = userSem
	r.userSemLastUsed[userID] = time.Now()
	userSem <- struct{}{}
	r.sem <- struct{}{}

	pg := &pauseGate{sem: r.sem, userSem: userSem, runner: r, userID: userID}

	// A "now" well past the idle threshold makes the entry maximally
	// eligible for reaping — the same trick TestUserSemaphoreSweep_
	// ConcurrentWithDispatch uses, and cheaper than making the production
	// threshold a test-tunable var.
	sweepNow := time.Now().Add(2 * userSemIdleThreshold)

	// Sanity check the premise: with no pause open, this entry IS reapable,
	// so surviving below is a real result and not a quirk of the setup.
	// (The token is held here, hence len(ch) > 0 saves it; drain it first to
	// reproduce exactly what a pause leaves behind.)
	<-userSem
	if removed := r.sweepIdleUserSemaphores(sweepNow); removed != 1 {
		t.Fatalf("premise check: removed = %d, want 1 — an idle, empty entry with no pause must be reapable", removed)
	}
	// Put it back the way run() would have left it, pre-pause.
	r.userSems[userID] = userSem
	r.userSemLastUsed[userID] = time.Now()
	userSem <- struct{}{}

	pg.release() // the task parks on an ask_user / permission prompt

	if got := len(userSem); got != 0 {
		t.Fatalf("userSem length after release = %d, want 0 — the pause must actually hand the slot back", got)
	}
	if removed := r.sweepIdleUserSemaphores(sweepNow); removed != 0 {
		t.Errorf("removed = %d during an open pause, want 0", removed)
	}
	if got, ok := r.userSems[userID]; !ok || got != userSem {
		t.Fatal("the paused task's user-semaphore entry was reaped mid-pause")
	}

	pg.reacquire() // the user answered

	if got := len(userSem); got != 1 {
		t.Errorf("userSem length after reacquire = %d, want 1", got)
	}
	r.userSemMu.Lock()
	remaining := r.userSemPaused[userID]
	r.userSemMu.Unlock()
	if remaining != 0 {
		t.Errorf("userSemPaused[%q] = %d after the pause ended, want 0 — a leaked count would pin the entry forever", userID, remaining)
	}
}

func TestSweepIdleUserSemaphores_NoEntries(t *testing.T) {
	r := &Runner{
		logger:          slog.Default(),
		userSems:        map[string]chan struct{}{},
		userSemLastUsed: map[string]time.Time{},
	}
	if removed := r.sweepIdleUserSemaphores(time.Now()); removed != 0 {
		t.Errorf("removed = %d, want 0 for an empty map", removed)
	}
}

// TestRun_StampsUserSemaphoreLastUsed proves run() actually feeds the
// timestamp map sweepIdleUserSemaphores depends on, not just that the sweep
// logic works in isolation.
func TestRun_StampsUserSemaphoreLastUsed(t *testing.T) {
	store := db.NewMemoryStore()
	task, err := store.CreateTask(context.Background(), "user-1", "prompt", "x", "print")
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	cfg := config.Defaults()
	cfg.Server.PerUserConcurrency = 1
	r := newSemaphoreTestRunner(store, cfg, 5*time.Second)
	r.userSemLastUsed = map[string]time.Time{}

	before := time.Now()
	r.run(context.Background(), queue.Item{TaskID: task.ID, UserID: "user-1"}, &fakeDelivery{})

	r.userSemMu.Lock()
	stamped, ok := r.userSemLastUsed["user-1"]
	r.userSemMu.Unlock()
	if !ok {
		t.Fatal("expected run() to record a last-used timestamp for user-1")
	}
	if stamped.Before(before) {
		t.Errorf("stamped timestamp %v is before the dispatch even started (%v)", stamped, before)
	}
}

// TestUserSemaphoreSweep_ConcurrentWithDispatch is a -race regression test:
// it hammers the lookup-or-create-and-stamp sequence run() uses (directly,
// without pulling in the rest of run()'s machinery) concurrently with an
// aggressive sweeper that treats every entry as maximally idle on every
// pass — the adversarial case for the race the brief calls out between a
// task starting for a user right as their entry could be swept. It asserts
// no panic/race/deadlock and that every dispatch can still acquire its slot
// promptly even while entries are being reaped out from under it.
func TestUserSemaphoreSweep_ConcurrentWithDispatch(t *testing.T) {
	r := &Runner{
		userSems:        map[string]chan struct{}{},
		userSemLastUsed: map[string]time.Time{},
	}

	stop := make(chan struct{})
	sweeperDone := make(chan struct{})
	go func() {
		defer close(sweeperDone)
		for {
			select {
			case <-stop:
				return
			default:
				// A "now" far in the future makes every entry's timestamp
				// look maximally idle, maximizing how often the sweep
				// actually attempts a delete — the adversarial case.
				r.sweepIdleUserSemaphores(time.Now().Add(24 * time.Hour))
			}
		}
	}()

	dispatch := func(t *testing.T, userID string) {
		r.userSemMu.Lock()
		if _, ok := r.userSems[userID]; !ok {
			r.userSems[userID] = make(chan struct{}, 2)
		}
		ch := r.userSems[userID]
		r.userSemLastUsed[userID] = time.Now()
		r.userSemMu.Unlock()

		select {
		case ch <- struct{}{}:
		case <-time.After(2 * time.Second):
			t.Errorf("dispatch for %s could not acquire its slot within 2s", userID)
			return
		}
		<-ch
	}

	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		userID := fmt.Sprintf("user-%d", i%5)
		go func(uid string) {
			defer wg.Done()
			dispatch(t, uid)
		}(userID)
	}
	wg.Wait()

	close(stop)
	<-sweeperDone
}

// --- A4: ConcurrencyUtilization + TaskErrorRate ---

func TestConcurrencyUtilization(t *testing.T) {
	r := &Runner{sem: make(chan struct{}, 5)}
	used, capacity := r.ConcurrencyUtilization()
	if used != 0 || capacity != 5 {
		t.Fatalf("got used=%d capacity=%d, want 0/5", used, capacity)
	}

	r.sem <- struct{}{}
	r.sem <- struct{}{}
	used, capacity = r.ConcurrencyUtilization()
	if used != 2 || capacity != 5 {
		t.Errorf("got used=%d capacity=%d, want 2/5", used, capacity)
	}
}

func TestTaskErrorRate_NoTasksYet(t *testing.T) {
	r := &Runner{}
	rate, failed, total := r.TaskErrorRate()
	if rate != 0 || failed != 0 || total != 0 {
		t.Errorf("got rate=%v failed=%d total=%d, want all zero with no completed tasks", rate, failed, total)
	}
}

func TestTaskErrorRate_ComputesFraction(t *testing.T) {
	r := &Runner{}
	r.outcomes.record(false) // done
	r.outcomes.record(true)  // failed
	r.outcomes.record(false) // done
	r.outcomes.record(false) // done (also stands in for a "cancelled" outcome — see below)

	rate, failed, total := r.TaskErrorRate()
	if failed != 1 || total != 4 {
		t.Fatalf("failed=%d total=%d, want 1/4", failed, total)
	}
	if rate != 0.25 {
		t.Errorf("rate = %v, want 0.25", rate)
	}
}

func TestTaskOutcomeWindow_CapsAtFixedSize(t *testing.T) {
	r := &Runner{}
	for i := 0; i < taskOutcomeWindowSize+10; i++ {
		r.outcomes.record(false) // all successes, well past the window size
	}
	r.outcomes.record(true) // one failure, most recent

	_, failed, total := r.TaskErrorRate()
	if total != taskOutcomeWindowSize {
		t.Errorf("total = %d, want capped at %d", total, taskOutcomeWindowSize)
	}
	if failed != 1 {
		t.Errorf("failed = %d, want 1 (the single most recent failure)", failed)
	}
}

// TestPersistFinalResult_RecordsTaskOutcome proves persistFinalResult
// actually feeds TaskErrorRate's window, not just that the window's own
// logic works in isolation.
func TestPersistFinalResult_RecordsTaskOutcome(t *testing.T) {
	store := db.NewMemoryStore()
	task, err := store.CreateTask(context.Background(), "user-1", "prompt", "x", "print")
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	r := &Runner{store: store, logger: slog.Default()}

	r.persistFinalResult(context.Background(), &fakeDelivery{}, task.ID, "failed", "", "boom", "", 0, 0)

	rate, failed, total := r.TaskErrorRate()
	if total != 1 || failed != 1 || rate != 1 {
		t.Errorf("got rate=%v failed=%d total=%d, want 1/1/1.0 after a single failed task", rate, failed, total)
	}
}
