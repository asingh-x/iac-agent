package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tf-agent/tf-agent/internal/agent"
	"github.com/tf-agent/tf-agent/internal/commands"
	"github.com/tf-agent/tf-agent/internal/config"
	"github.com/tf-agent/tf-agent/internal/db"
	"github.com/tf-agent/tf-agent/internal/hooks"
	"github.com/tf-agent/tf-agent/internal/llm"
	"github.com/tf-agent/tf-agent/internal/permissions"
	"github.com/tf-agent/tf-agent/internal/queue"
	"github.com/tf-agent/tf-agent/internal/sandbox"
	"github.com/tf-agent/tf-agent/internal/session"
	"github.com/tf-agent/tf-agent/internal/skills"
	"github.com/tf-agent/tf-agent/internal/taskctx"
	"github.com/tf-agent/tf-agent/internal/tools"
)

// Runner pulls tasks from the queue and executes them.
type Runner struct {
	hub      *Hub
	store    db.Store
	queue    queue.Queue
	provider llm.Provider
	cfg      *config.Config
	sem      chan struct{} // global LLM concurrency semaphore
	logger   *slog.Logger
	relay    ControlRelay // cross-pod fallback for answer/permission/cancel

	// semaphoreTimeout bounds how long run() will wait for a concurrency
	// slot (global sem, or a per-user one) before failing the task cleanly
	// instead of blocking indefinitely. See ConcurrencyUtilization and the
	// semaphore-acquire section of run() for how it's used.
	semaphoreTimeout time.Duration

	// outcomes is a rolling window of the most recent task terminal
	// outcomes, backing TaskErrorRate (surfaced via /healthz). Its zero
	// value is ready to use.
	outcomes taskOutcomeWindow

	answerMu sync.Mutex
	answers  map[string]chan string // taskID → pending answer channel

	permissionMu sync.Mutex
	permissions  map[string]chan bool // taskID → pending permission-response channel

	cancelMu sync.Mutex
	cancels  map[string]context.CancelFunc // taskID → cancel func

	userSemMu sync.Mutex
	userSems  map[string]chan struct{} // userID → per-user semaphore
	// userSemLastUsed tracks, per userID, when that entry in userSems was
	// last touched by a dispatch (see run()'s per-user semaphore section) —
	// backs sweepIdleUserSemaphores, which reaps entries for users who have
	// gone quiet, so userSems doesn't grow forever across the process
	// lifetime. Always accessed under userSemMu, same as userSems itself.
	userSemLastUsed map[string]time.Time
	// userSemPaused counts, per userID, how many of that user's tasks
	// currently have an open pause — i.e. have handed their per-user slot
	// back to userSems for the duration of an ask_user/permission wait (see
	// pauseGate). sweepIdleUserSemaphores must not reap an entry with a
	// non-zero count: such an entry's channel is legitimately empty even
	// though a task is still conceptually occupying that slot and will
	// reacquire it, and reaping it would let the user's next task build a
	// fresh full-capacity channel and exceed PerUserConcurrency. Only
	// tracked for users who actually have a per-user semaphore (limiting
	// enabled), and entries are deleted as they return to zero so this map
	// doesn't outgrow the one it guards. Always accessed under userSemMu,
	// same as userSems itself.
	userSemPaused map[string]int

	// inFlight tracks running task goroutines, for graceful shutdown.
	// draining guards a real race: sync.WaitGroup forbids a concurrent Add
	// happening around a Wait when the counter could be zero. dispatchMu
	// makes "check draining, then Add" atomic with respect to Shutdown
	// setting draining — Shutdown's Lock() cannot proceed (and therefore
	// Wait cannot be called) until any in-progress Add under RLock has
	// completed, which is what makes this safe.
	dispatchMu sync.RWMutex
	draining   bool
	inFlight   sync.WaitGroup
}

// Shutdown stops accepting the effects of new task work and waits for
// in-flight tasks to finish naturally. If ctx is done before all in-flight
// tasks complete, it force-cancels every still-running task (the same
// mechanism CancelTask uses) so the process can exit rather than hang forever
// on a stuck task.
//
// Callers must stop the queue's Pop loop(s) (e.g. by cancelling the context
// passed to StartQueue) before calling Shutdown, so no new tasks start during
// the drain.
func (r *Runner) Shutdown(ctx context.Context) {
	// Setting draining under the write lock cannot proceed until any
	// in-progress StartQueue dispatch (holding the read lock around its
	// check-then-Add) has fully completed its Add call. That ordering is
	// what makes it safe to call inFlight.Wait() below: sync.WaitGroup
	// forbids a concurrent Add racing a Wait when the counter could be
	// zero, and this guarantees no Add can start after this point.
	r.dispatchMu.Lock()
	r.draining = true
	r.dispatchMu.Unlock()

	done := make(chan struct{})
	go func() {
		r.inFlight.Wait()
		close(done)
	}()

	select {
	case <-done:
		return
	case <-ctx.Done():
		r.logger.Warn("shutdown grace period expired, cancelling in-flight tasks")
		r.cancelMu.Lock()
		for taskID, cancel := range r.cancels {
			r.logger.Warn("force-cancelling task at shutdown", "task_id", taskID)
			cancel()
		}
		r.cancelMu.Unlock()
		<-done // wait for the now-cancelled tasks to actually finish persisting their result
	}
}

// controlRelayTimeout bounds every cross-pod control request (answer /
// permission / cancel). It MUST be bounded: NATS core request-reply only
// fast-fails with ErrNoResponders when *zero* subscriptions exist on the
// subject, and every pod subscribes to "tf.control.*" — so a request for a
// task no pod owns has interest but no responder, and would block forever on
// an unbounded context, hanging the HTTP handler goroutine that called it
// (the server sets WriteTimeout: 0 for SSE, so nothing else rescues it).
//
// A var, not a const, purely so tests can shorten it; nothing mutates it at
// runtime.
var controlRelayTimeout = 5 * time.Second

// relayContext returns the bounded context used for a single cross-pod
// control request. The caller must always call the returned cancel func.
//
// The bound is internal on purpose: plumbing the HTTP request's own context
// through would change the public signatures of CancelTask / SendAnswer /
// SendPermissionResponse, which a large number of call sites and tests depend
// on staying as they are.
func relayContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), controlRelayTimeout)
}

// cancelTaskLocal only checks this pod's own state. It's what
// ControlLocalHandler exposes to NATSControlRelay's inbound handler, kept
// separate from CancelTask so that handler can never recurse back out over
// the relay when this pod turns out not to own the task.
func (r *Runner) cancelTaskLocal(taskID string) error {
	r.cancelMu.Lock()
	cancel, ok := r.cancels[taskID]
	r.cancelMu.Unlock()
	if !ok {
		return errNotOwned
	}
	cancel()
	return nil
}

// CancelTask cancels a running task, asking a different pod via the control
// relay if this pod has no local record of it. Returns an error if no pod
// owns the task.
func (r *Runner) CancelTask(taskID string) error {
	if err := r.cancelTaskLocal(taskID); err != nil {
		if errors.Is(err, errNotOwned) {
			ctx, cancel := relayContext()
			defer cancel()
			return r.relay.RequestCancel(ctx, taskID)
		}
		return err
	}
	return nil
}

// sendAnswerLocal only checks this pod's own state; see cancelTaskLocal's
// doc comment for why this is kept separate from SendAnswer.
func (r *Runner) sendAnswerLocal(taskID, answer string) error {
	r.answerMu.Lock()
	ch, ok := r.answers[taskID]
	r.answerMu.Unlock()
	if !ok {
		return errNotOwned
	}
	select {
	case ch <- answer:
		return nil
	default:
		return fmt.Errorf("task %s answer channel full", taskID)
	}
}

// SendAnswer delivers a user answer to a task that is waiting_for_input,
// asking a different pod via the control relay if this pod has no local
// record of it. Returns an error if no pod is waiting for input on this task.
func (r *Runner) SendAnswer(taskID, answer string) error {
	if err := r.sendAnswerLocal(taskID, answer); err != nil {
		if errors.Is(err, errNotOwned) {
			ctx, cancel := relayContext()
			defer cancel()
			if relayErr := r.relay.RequestAnswer(ctx, taskID, answer); relayErr != nil {
				return fmt.Errorf("task %s is not waiting for input", taskID)
			}
			return nil
		}
		return err
	}
	return nil
}

// sendPermissionResponseLocal only checks this pod's own state; see
// cancelTaskLocal's doc comment for why this is kept separate from
// SendPermissionResponse.
func (r *Runner) sendPermissionResponseLocal(taskID string, allow bool) error {
	r.permissionMu.Lock()
	ch, ok := r.permissions[taskID]
	r.permissionMu.Unlock()
	if !ok {
		return errNotOwned
	}
	select {
	case ch <- allow:
		return nil
	default:
		return fmt.Errorf("task %s permission channel full", taskID)
	}
}

// SendPermissionResponse delivers a user's allow/deny decision to a task
// currently paused on a permission prompt, asking a different pod via the
// control relay if this pod has no local record of it. Returns an error if
// no pod is waiting on a permission decision for this task.
func (r *Runner) SendPermissionResponse(taskID string, allow bool) error {
	if err := r.sendPermissionResponseLocal(taskID, allow); err != nil {
		if errors.Is(err, errNotOwned) {
			ctx, cancel := relayContext()
			defer cancel()
			if relayErr := r.relay.RequestPermission(ctx, taskID, allow); relayErr != nil {
				return fmt.Errorf("task %s is not waiting for a permission decision", taskID)
			}
			return nil
		}
		return err
	}
	return nil
}

// pauseGate depth-counts concurrent "parked waiting for human input" pauses
// on a single task, so the underlying LLM-concurrency semaphore(s) are
// released exactly once when the first such pause begins and reacquired
// exactly once when the last one ends — no matter how many pauses on this
// task are open at once.
//
// This exists because a single LLM turn can batch an ask_user tool call
// together with another tool call that needs permission (e.g. bash), and
// internal/agent/loop.go's executeTools fans multiple tool calls from one
// turn into parallel goroutines. Both the AskUser callback (run from the
// ask_user tool's own fanned-out goroutine) and awaitPermission (run
// synchronously from Runner.run's own event-loop goroutine) can therefore be
// "open" concurrently for the SAME task, even though that task only ever
// acquired one token from sem and one from userSem at the top of run(). A
// naive release/reacquire at each pause site independently would
// double-release a token this task doesn't have a second copy of: either
// silently stealing another task's slot elsewhere on the pod (breaking the
// concurrency-limit invariant), or — if nothing else is available to steal —
// blocking forever on a bare channel receive with no cancellation escape,
// hanging Runner.run's event loop for this task unrecoverably. pauseGate
// makes overlapping pauses on one task share a single release/reacquire
// pair instead, which is safe for any number of concurrent pauses (2, 3, or
// N): the first one in does the real release, the last one out does the
// real reacquire, and everything in between is a no-op depth change.
type pauseGate struct {
	sem     chan struct{}
	userSem chan struct{} // nil if PerUserConcurrency limiting is disabled

	// runner and userID let release/reacquire maintain
	// Runner.userSemPaused, which is what stops sweepIdleUserSemaphores from
	// reaping this user's semaphore entry while it sits empty for the
	// duration of a pause. Both are only meaningful when userSem is non-nil
	// (per-user limiting enabled); a nil runner disables the bookkeeping
	// entirely, for tests that drive a pauseGate in isolation.
	runner *Runner
	userID string

	mu    sync.Mutex
	depth int
}

// release drops this task's concurrency slot(s) back to the shared pool the
// first time it's called while no other pause on this task is open ("first
// one in"); an overlapping call just increments the depth counter.
func (g *pauseGate) release() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.depth++
	if g.depth == 1 {
		// Record the open pause BEFORE handing the per-user token back, so
		// there is never an instant in which the user's channel is empty and
		// no pause is recorded — that instant is exactly what
		// sweepIdleUserSemaphores would misread as "idle, nobody using it".
		g.trackPause(+1)
		<-g.sem
		if g.userSem != nil {
			<-g.userSem
		}
	}
}

// reacquire is release's counterpart: it only takes the slot(s) back once
// the last overlapping pause on this task ends ("last one out").
//
// The sends below are deliberately unbounded, and their safety rests on an
// invariant rather than on a timeout: this task released exactly one token
// from each channel in release(), so each channel has room reserved for
// exactly this send and it cannot block for long. Anything that breaks that
// one-release-per-reacquire pairing (a second release path added outside
// pauseGate, or a channel swapped out mid-pause — see
// sweepIdleUserSemaphores, which must not reap an entry with an open pause
// for precisely this reason) turns these into an unbounded block with no
// cancellation escape. Adding a timeout here is NOT the fix: abandoning the
// send would silently shrink the pool's capacity for the rest of the
// process's life. Preserve the invariant instead.
func (g *pauseGate) reacquire() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.depth--
	if g.depth == 0 {
		g.sem <- struct{}{}
		if g.userSem != nil {
			g.userSem <- struct{}{}
		}
		// Mirror of release's ordering: clear the open pause only after the
		// token is back in the channel, so the two conditions never both
		// read "unused" at once.
		g.trackPause(-1)
	}
}

// trackPause adjusts this task's user's open-pause count by delta. A no-op
// when there is no per-user semaphore to protect (limiting disabled) or no
// Runner to record against. Callers hold g.mu; this takes Runner.userSemMu,
// which is only ever acquired for short map updates and never while holding
// g.mu elsewhere, so there is no lock cycle.
func (g *pauseGate) trackPause(delta int) {
	if g.userSem == nil || g.runner == nil {
		return
	}
	r := g.runner
	r.userSemMu.Lock()
	defer r.userSemMu.Unlock()
	n := r.userSemPaused[g.userID] + delta
	if n <= 0 {
		delete(r.userSemPaused, g.userID)
		return
	}
	r.userSemPaused[g.userID] = n
}

// awaitPermission pauses the task on a "confirm this tool call" prompt: it
// publishes a permission_request SSE event and blocks until the user
// responds via SendPermissionResponse, the wait times out (denies, since
// timing out on a destructive-tool confirmation should not silently allow
// it), or the task's context is cancelled (denies).
func (r *Runner) awaitPermission(ctx context.Context, taskID string, req *agent.PermissionRequest, waitTimeoutSeconds int, ch <-chan bool, waitingForInput *int32, release func(), reacquire func()) bool {
	atomic.StoreInt32(waitingForInput, 1)
	defer atomic.StoreInt32(waitingForInput, 0)

	// Release concurrency slots while parked on an approve/deny prompt, for
	// the same reason as the AskUser callback in Runner.run. release/
	// reacquire are Runner.run's pauseGate.release/pauseGate.reacquire
	// methods, which depth-count concurrent pauses on the same task so this
	// can safely overlap with a concurrent AskUser pause (see pauseGate's
	// doc comment) without double-releasing a semaphore slot this task only
	// acquired once.
	release()
	defer reacquire()

	preview := previewToolInput(req.Input)

	if err := r.store.UpdateTaskStatus(ctx, taskID, "waiting_for_input"); err != nil {
		r.logger.Error("failed to update task status", "task_id", taskID, "status", "waiting_for_input", "err", err)
	}
	if err := r.store.UpdateTaskPendingQuestion(ctx, taskID, FormatPermissionPrompt(req.ToolName, preview), db.PendingKindPermission); err != nil {
		r.logger.Error("failed to update task pending question", "task_id", taskID, "err", err)
	}
	r.hub.Publish(taskID, ServerEvent{Type: "permission_request", Tool: req.ToolName, Text: preview})

	defer func() {
		if err := r.store.UpdateTaskPendingQuestion(ctx, taskID, "", ""); err != nil {
			r.logger.Error("failed to clear task pending question", "task_id", taskID, "err", err)
		}
		if err := r.store.UpdateTaskStatus(ctx, taskID, "running"); err != nil {
			r.logger.Error("failed to update task status", "task_id", taskID, "status", "running", "err", err)
		}
		r.hub.Publish(taskID, ServerEvent{Type: "status", Status: "running"})
	}()

	select {
	case allow := <-ch:
		return allow
	case <-time.After(time.Duration(waitTimeoutSeconds) * time.Second):
		r.logger.Warn("permission request timed out, denying", "task_id", taskID, "tool", req.ToolName)
		return false
	case <-ctx.Done():
		return false
	}
}

// FormatPermissionPrompt renders the pending-question text stored for a task
// paused on a tool-permission prompt. ParsePermissionPrompt is its inverse;
// the two must stay in step, which is why they live side by side. The format
// is also what the web client's own "approve tool call" rendering expects.
func FormatPermissionPrompt(tool, preview string) string {
	return fmt.Sprintf("Approve %s: %s", tool, preview)
}

// ParsePermissionPrompt recovers the tool name and input preview from text
// produced by FormatPermissionPrompt. On anything it can't split it returns an
// empty tool and the input unchanged, so a client still sees the raw prompt.
func ParsePermissionPrompt(s string) (tool, preview string) {
	rest, ok := strings.CutPrefix(s, "Approve ")
	if !ok {
		return "", s
	}
	tool, preview, ok = strings.Cut(rest, ": ")
	if !ok {
		return "", s
	}
	return tool, preview
}

// previewToolInput renders a short human-readable preview of a tool call's
// input for a permission prompt: the most relevant field for tools with a
// well-known shape (the command being run, the file being touched, ...),
// falling back to raw (truncated) JSON for anything else.
func previewToolInput(input json.RawMessage) string {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(input, &fields); err == nil {
		for _, key := range []string{"command", "file_path", "path", "pattern"} {
			if raw, ok := fields[key]; ok {
				var s string
				if json.Unmarshal(raw, &s) == nil && s != "" {
					return s
				}
			}
		}
	}
	if len(input) > 200 {
		return string(input[:200]) + "..."
	}
	return string(input)
}

func NewRunner(hub *Hub, store db.Store, q queue.Queue, provider llm.Provider, cfg *config.Config, logger *slog.Logger) *Runner {
	concurrency := cfg.Server.LLMConcurrency
	if concurrency <= 0 {
		concurrency = 10
	}
	semTimeout := time.Duration(cfg.Server.SemaphoreAcquireTimeout) * time.Second
	if semTimeout <= 0 {
		semTimeout = 5 * time.Minute
	}
	return &Runner{
		hub:              hub,
		store:            store,
		queue:            q,
		provider:         provider,
		cfg:              cfg,
		sem:              make(chan struct{}, concurrency),
		logger:           logger,
		relay:            noopControlRelay{},
		semaphoreTimeout: semTimeout,
		answers:          make(map[string]chan string),
		permissions:      make(map[string]chan bool),
		cancels:          make(map[string]context.CancelFunc),
		userSems:         make(map[string]chan struct{}),
		userSemLastUsed:  make(map[string]time.Time),
		userSemPaused:    make(map[string]int),
	}
}

// ConcurrencyUtilization reports how full the global LLM concurrency
// semaphore currently is: used is the number of slots currently held,
// capacity is the total configured slots (cfg.Server.LLMConcurrency).
// Reading len/cap on a channel is safe for concurrent use without locking.
func (r *Runner) ConcurrencyUtilization() (used, capacity int) {
	return len(r.sem), cap(r.sem)
}

// userSemIdleThreshold is how long a per-user semaphore entry can sit with
// no dispatch activity before sweepIdleUserSemaphores reaps it. An hour is
// comfortably longer than any realistic gap between a single user's tasks,
// while still bounding how long Runner.userSems holds an entry for a user
// who submitted exactly once and never came back — the map would otherwise
// grow for as long as the process runs.
const userSemIdleThreshold = time.Hour

// userSemSweepInterval controls how often StartUserSemaphoreSweeper checks
// for idle per-user semaphore entries. Independent of userSemIdleThreshold
// (the age at which an entry qualifies) — this just bounds how promptly a
// newly-idle entry gets noticed.
var userSemSweepInterval = 10 * time.Minute

// sweepIdleUserSemaphores removes userSems/userSemLastUsed entries for
// users with no dispatch activity in the last userSemIdleThreshold relative
// to now, and returns how many it removed.
//
// It only ever removes an entry that no task is using: the channel must be
// empty (len == 0) AND the user must have no open pause (see
// Runner.userSemPaused). Both checks are load-bearing, for different
// reasons:
//
//   - len(ch) > 0 covers a task actively holding a slot right now. Mostly
//     defense in depth, since the timestamp is refreshed at the start of
//     every dispatch attempt for that user, in the same locked section as
//     the lookup/create, so an in-use entry's timestamp is normally recent
//     (see run()'s per-user semaphore section).
//   - userSemPaused > 0 covers a task that is paused waiting for human
//     input and has therefore handed its slot back (see pauseGate). Here
//     the timestamp argument above genuinely does not hold: a pause can
//     last up to WaitForInputTimeout (7 days by default), far longer than
//     userSemIdleThreshold, so a paused task's entry really can look both
//     idle and unused. Reaping it would leave the paused task holding
//     nothing and its eventual reacquire pushing into an abandoned
//     channel, while the user's next task built a fresh, full-capacity one
//     — quietly letting that user exceed PerUserConcurrency by a slot.
//
// Safe against a task starting for this user concurrently with a sweep:
// the lookup-or-create-and-stamp sequence in run() and this function's
// read-and-delete both hold r.userSemMu, so they can never interleave.
// Deleting a map entry a goroutine still holds a Go channel reference to is
// safe — channels are safe to abandon, so that goroutine's own acquire/
// release against its captured channel keeps working unaffected. A new
// task for that user simply creates a fresh entry on its next dispatch.
func (r *Runner) sweepIdleUserSemaphores(now time.Time) (removed int) {
	r.userSemMu.Lock()
	defer r.userSemMu.Unlock()
	for userID, lastUsed := range r.userSemLastUsed {
		if now.Sub(lastUsed) < userSemIdleThreshold {
			continue
		}
		if ch, ok := r.userSems[userID]; ok && len(ch) > 0 {
			continue // a task currently holds a slot; leave this entry alone
		}
		if r.userSemPaused[userID] > 0 {
			// A task is paused mid-run with its slot handed back, and will
			// reacquire it against this exact channel.
			continue
		}
		delete(r.userSems, userID)
		delete(r.userSemLastUsed, userID)
		removed++
	}
	return removed
}

// StartUserSemaphoreSweeper periodically removes per-user semaphore entries
// for users idle longer than userSemIdleThreshold, so Runner.userSems does
// not grow without bound across the lifetime of a long-running process.
// Runs until ctx is cancelled — call in a goroutine, tied to the same
// top-level shutdown ctx as StartQueue's pop loop(s) (see
// cmd/server/main.go), the same pattern used there for
// runStaleTaskReconciler.
func (r *Runner) StartUserSemaphoreSweeper(ctx context.Context) {
	ticker := time.NewTicker(userSemSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if n := r.sweepIdleUserSemaphores(time.Now()); n > 0 {
				r.logger.Info("swept idle per-user semaphore entries", "count", n)
			}
		}
	}
}

// SetControlRelay installs a cross-pod control relay (see ControlRelay).
// Call once at startup, before any tasks are dispatched.
func (r *Runner) SetControlRelay(relay ControlRelay) {
	r.relay = relay
}

// Compile-time check that Runner still satisfies ControlLocalHandler, so a
// future change to sendAnswerLocal/sendPermissionResponseLocal/cancelTaskLocal
// that breaks the contract NATSControlRelay depends on fails the build
// instead of failing silently at runtime.
var _ ControlLocalHandler = (*Runner)(nil)

// Start blocks, continuously pulling from the Runner's default queue.
// Run in a goroutine.
func (r *Runner) Start(ctx context.Context) {
	r.StartQueue(ctx, r.queue)
}

// StartQueue blocks, continuously pulling from q.
// Call in a goroutine for each named queue to give each its own worker loop.
//
// ctx only governs the pop loop itself: cancelling it stops picking up new
// work, but does NOT cancel tasks already dispatched to run() — those get an
// independent lifetime so a shutdown signal drains in-flight work instead of
// killing it outright. See Shutdown.
func (r *Runner) StartQueue(ctx context.Context, q queue.Queue) {
	for {
		item, delivery, err := q.Pop(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			continue
		}
		// Check-then-Add under the read lock: see the comment on Shutdown for
		// why this makes Add-vs-Wait race-free rather than just "unlikely".
		r.dispatchMu.RLock()
		if r.draining {
			r.dispatchMu.RUnlock()
			// Shutdown has begun: don't start new work. Nak so a durable
			// queue (NATS) redelivers this to another worker/restart rather
			// than losing it; a no-op for the in-memory queue, which is
			// already documented as lossy on restart.
			if err := delivery.Nak(); err != nil {
				r.logger.Error("failed to nak task during shutdown", "task_id", item.TaskID, "err", err)
			}
			continue
		}
		r.inFlight.Add(1)
		r.dispatchMu.RUnlock()
		go func() {
			defer r.inFlight.Done()
			r.run(context.Background(), item, delivery)
		}()
	}
}

// heartbeatInterval controls how often Delivery.Extend is called while a task
// is running, so a long-running task isn't redelivered out from under an
// active worker (NATS's AckWait is 5 minutes; this must stay well under that).
const heartbeatInterval = 2 * time.Minute

func (r *Runner) run(ctx context.Context, item queue.Item, delivery queue.Delivery) {
	defer func() {
		if rec := recover(); rec != nil {
			r.logger.Error("panic in task runner", "task_id", item.TaskID, "panic", rec)
			r.fail(ctx, item.TaskID, fmt.Sprintf("internal error: %v", rec), delivery)
		}
	}()

	// A redelivered message for a task that already reached a terminal state
	// means the original worker's Ack was lost (e.g. a network blip) even
	// though the task actually finished — not that the task needs re-running.
	// Skip re-execution and just ack so the message stops being redelivered.
	if existing, err := r.store.GetTask(ctx, item.TaskID); err == nil && existing != nil && isTerminalStatus(existing.Status) {
		r.logger.Info("skipping redelivered task already in terminal state", "task_id", item.TaskID, "status", existing.Status)
		if err := delivery.Ack(); err != nil {
			r.logger.Error("failed to ack already-terminal redelivered task", "task_id", item.TaskID, "err", err)
		}
		return
	}

	// Keep the queue delivery alive for the duration of a long-running task.
	heartbeatCtx, stopHeartbeat := context.WithCancel(ctx)
	defer stopHeartbeat()
	go func() {
		ticker := time.NewTicker(heartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-heartbeatCtx.Done():
				return
			case <-ticker.C:
				if err := delivery.Extend(); err != nil {
					r.logger.Warn("failed to extend queue delivery", "task_id", item.TaskID, "err", err)
				}
			}
		}
	}()

	// Enforce per-user concurrency limit. Bounded by r.semaphoreTimeout: with
	// no timeout branch here, a user whose per-user slots are all busy would
	// block this goroutine (and its NATS delivery heartbeat) forever — and
	// since the per-task context.CancelFunc isn't registered in r.cancels
	// until after this section, Shutdown's grace-period force-cancel can't
	// even reach a task stuck here to unblock it.
	var userSem chan struct{} // nil if PerUserConcurrency limiting is disabled
	if limit := r.cfg.Server.PerUserConcurrency; limit > 0 {
		r.userSemMu.Lock()
		if _, ok := r.userSems[item.UserID]; !ok {
			r.userSems[item.UserID] = make(chan struct{}, limit)
		}
		userSem = r.userSems[item.UserID]
		// Stamp "last used" in the same critical section as the lookup/
		// create above — not after the select below acquires a slot — so
		// sweepIdleUserSemaphores (also gated on r.userSemMu) can never see
		// a stale timestamp for an entry a dispatch is actively in the
		// middle of touching. Both the read (sweep) and this write hold the
		// same lock, and this write always happens-before the corresponding
		// channel is handed to a real task below, so a sweep can only ever
		// reap an entry with no dispatch activity at all in the idle
		// window — never one merely between "looked up" and "acquired".
		r.userSemLastUsed[item.UserID] = time.Now()
		r.userSemMu.Unlock()
		select {
		case userSem <- struct{}{}:
		case <-ctx.Done():
			return
		case <-time.After(r.semaphoreTimeout):
			r.logger.Warn("timed out waiting for per-user LLM concurrency slot", "task_id", item.TaskID, "user_id", item.UserID, "timeout", r.semaphoreTimeout)
			r.fail(ctx, item.TaskID, "server at capacity: timed out waiting for an available per-user execution slot, try again later", delivery)
			return
		}
		defer func() { <-userSem }()
	}

	// Acquire global semaphore. Bounded for the same reason as the per-user
	// wait above.
	select {
	case r.sem <- struct{}{}:
	case <-ctx.Done():
		return
	case <-time.After(r.semaphoreTimeout):
		r.logger.Warn("timed out waiting for global LLM concurrency slot", "task_id", item.TaskID, "timeout", r.semaphoreTimeout)
		r.fail(ctx, item.TaskID, "server at capacity: timed out waiting for an available LLM concurrency slot, try again later", delivery)
		return
	}
	defer func() { <-r.sem }()

	// pg depth-counts concurrent pauses on this task so overlapping pauses
	// (e.g. an ask_user tool call batched with another tool call needing
	// permission in the same LLM turn — internal/agent/loop.go's
	// executeTools fans these out into parallel goroutines) share one
	// release/reacquire of the underlying semaphore instead of each
	// independently releasing/reacquiring, which would double-release a
	// token this task only acquired once. See pauseGate's doc comment.
	//
	// runner/userID are what let it also record the pause in
	// r.userSemPaused, so sweepIdleUserSemaphores doesn't mistake this
	// user's momentarily-empty semaphore for an idle one and reap it
	// out from under a pause that can legitimately outlast
	// userSemIdleThreshold.
	pg := &pauseGate{sem: r.sem, userSem: userSem, runner: r, userID: item.UserID}

	// Create a per-task cancellable context so individual tasks can be stopped.
	taskCtxCancel, cancel := context.WithCancel(ctx)
	defer cancel()
	r.cancelMu.Lock()
	r.cancels[item.TaskID] = cancel
	r.cancelMu.Unlock()
	defer func() {
		r.cancelMu.Lock()
		delete(r.cancels, item.TaskID)
		r.cancelMu.Unlock()
	}()
	ctx = taskCtxCancel

	// Enforce a maximum active-execution duration so a runaway task can't run
	// forever. Time spent paused in waiting_for_input does not count against
	// this budget — that wait has its own, much longer, timeout (see the
	// AskUser callback below) — so waitingForInput pauses the watchdog's clock.
	maxTaskDuration := time.Duration(r.cfg.Agent.MaxTaskDuration) * time.Second
	if maxTaskDuration <= 0 {
		maxTaskDuration = 30 * time.Minute
	}
	var waitingForInput int32 // atomic bool
	var timedOut int32        // atomic bool: set by the watchdog when it fires
	watchdogDone := make(chan struct{})
	defer close(watchdogDone)
	go func() {
		var active time.Duration
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-watchdogDone:
				return
			case <-ticker.C:
				if atomic.LoadInt32(&waitingForInput) == 1 {
					continue
				}
				active += time.Second
				if active >= maxTaskDuration {
					atomic.StoreInt32(&timedOut, 1)
					r.logger.Warn("task exceeded max execution duration, cancelling", "task_id", item.TaskID, "max_duration", maxTaskDuration)
					cancel()
					return
				}
			}
		}
	}()

	startedAt := time.Now()

	// Claim the task's event stream for this pod before publishing anything
	// for it. This is what tells Hub.ServeSSE that this pod's local channel
	// is the authoritative source for the task — on any other pod (including
	// the one that merely accepted the submission and created a channel for
	// it) the stream must read from the cross-pod relay instead. Every path
	// out of run() from here on ends in hub.Close, which releases the claim.
	r.hub.Claim(item.TaskID)

	// Mark running.
	if err := r.store.UpdateTaskStatus(ctx, item.TaskID, "running"); err != nil {
		r.logger.Error("failed to update task status", "task_id", item.TaskID, "status", "running", "err", err)
	}
	r.hub.Publish(item.TaskID, ServerEvent{Type: "status", Status: "running"})

	// Merge stored user settings into credentials.
	// Request-supplied tokens take precedence; fall back to user's saved settings.
	githubToken := item.GitHubToken
	atlassianToken := item.AtlassianToken
	atlassianDomain := item.AtlassianDomain
	atlassianEmail := item.AtlassianEmail

	if us, err := r.store.GetUserSettings(ctx, item.UserID); err == nil {
		if githubToken == "" && us.GitHubToken != "" {
			if dec, err := Decrypt(us.GitHubToken); err == nil {
				githubToken = dec
			}
		}
		if atlassianToken == "" && us.AtlassianToken != "" {
			if dec, err := Decrypt(us.AtlassianToken); err == nil {
				atlassianToken = dec
			}
		}
		if atlassianDomain == "" {
			atlassianDomain = us.AtlassianDomain
		}
		if atlassianEmail == "" {
			atlassianEmail = us.AtlassianEmail
		}
	}

	creds := taskctx.Credentials{
		OutputType:      item.OutputType,
		OutputDir:       item.OutputDir,
		RepoURL:         item.RepoURL,
		GitHubToken:     githubToken,
		AtlassianToken:  atlassianToken,
		AtlassianDomain: atlassianDomain,
		AtlassianEmail:  atlassianEmail,
		TaskID:          item.TaskID,
	}
	taskCtx := taskctx.WithCredentials(ctx, creds)

	// Wire mid-session pause: create per-task answer channel, inject ask_user callback.
	answerCh := make(chan string, 1)
	r.answerMu.Lock()
	r.answers[item.TaskID] = answerCh
	r.answerMu.Unlock()
	defer func() {
		r.answerMu.Lock()
		delete(r.answers, item.TaskID)
		r.answerMu.Unlock()
	}()

	waitTimeout := r.cfg.Agent.WaitForInputTimeout
	if waitTimeout <= 0 {
		waitTimeout = 7 * 24 * 3600 // 7 days default
	}

	// Per-task permission-response channel, for the confirm-before-destructive-
	// tool flow (see the TurnEventPermission case in the event loop below).
	permissionCh := make(chan bool, 1)
	r.permissionMu.Lock()
	r.permissions[item.TaskID] = permissionCh
	r.permissionMu.Unlock()
	defer func() {
		r.permissionMu.Lock()
		delete(r.permissions, item.TaskID)
		r.permissionMu.Unlock()
	}()

	taskCtx = taskctx.WithAskUser(taskCtx, func(askCtx context.Context, question string) (string, error) {
		atomic.StoreInt32(&waitingForInput, 1)
		defer atomic.StoreInt32(&waitingForInput, 0)

		// Release concurrency slots while parked on a human answer — an
		// unanswered question (up to WaitForInputTimeout, 7 days by default)
		// must not hold capacity that could serve other tasks. Goes through
		// pg (a *pauseGate), not a direct channel release, because this
		// callback can run concurrently with awaitPermission for the SAME
		// task: a single LLM turn can batch an ask_user call together with
		// another tool call that needs permission, and executeTools fans
		// those out into parallel goroutines (internal/agent/loop.go). See
		// pauseGate's doc comment.
		pg.release()
		defer pg.reacquire()

		if err := r.store.UpdateTaskStatus(ctx, item.TaskID, "waiting_for_input"); err != nil {
			r.logger.Error("failed to update task status", "task_id", item.TaskID, "status", "waiting_for_input", "err", err)
		}
		if err := r.store.UpdateTaskPendingQuestion(ctx, item.TaskID, question, db.PendingKindQuestion); err != nil {
			r.logger.Error("failed to update task pending question", "task_id", item.TaskID, "err", err)
		}
		r.hub.Publish(item.TaskID, ServerEvent{Type: "waiting_for_input", Text: question})

		select {
		case answer := <-answerCh:
			if err := r.store.UpdateTaskPendingQuestion(ctx, item.TaskID, "", ""); err != nil {
				r.logger.Error("failed to clear task pending question", "task_id", item.TaskID, "err", err)
			}
			if err := r.store.UpdateTaskStatus(ctx, item.TaskID, "running"); err != nil {
				r.logger.Error("failed to update task status", "task_id", item.TaskID, "status", "running", "err", err)
			}
			r.hub.Publish(item.TaskID, ServerEvent{Type: "status", Status: "running"})
			return answer, nil
		case <-time.After(time.Duration(waitTimeout) * time.Second):
			return "", fmt.Errorf("timed out waiting for user input")
		case <-askCtx.Done():
			return "", askCtx.Err()
		}
	})

	ag, err := r.wireAgent(taskCtx, item)
	if err != nil {
		r.fail(ctx, item.TaskID, fmt.Sprintf("agent setup: %v", err), delivery)
		return
	}

	prompt := r.buildPrompt(item)
	eventCh := ag.RunTurn(taskCtx, prompt)

	var totalIn, totalOut int
	var totalCacheRead, totalCacheCreated int
	var prURL string
	var outputBuf strings.Builder
	var taskErr error

	for ev := range eventCh {
		switch ev.Type {
		case agent.TurnEventText:
			outputBuf.WriteString(ev.Text)
			r.hub.Publish(item.TaskID, ServerEvent{Type: "text", Text: ev.Text})

		case agent.TurnEventToolStart:
			if ev.ToolCall != nil {
				r.hub.Publish(item.TaskID, ServerEvent{Type: "tool_start", Tool: ev.ToolCall.Name})
			}

		case agent.TurnEventToolEnd:
			if ev.ToolResult != nil {
				out := ev.ToolResult.Output
				if len(out) > 500 {
					out = out[:500] + "..."
				}
				r.hub.Publish(item.TaskID, ServerEvent{Type: "tool_end", Tool: ev.ToolResult.Name, Output: out})
				// capture PR URL if CreatePR ran
				if ev.ToolResult.Name == "CreatePR" && ev.ToolResult.Err == nil {
					out := ev.ToolResult.Output
					if i := strings.Index(out, "https://"); i >= 0 {
						prURL = out[i:]
					} else {
						prURL = out
					}
				}
			}

		case agent.TurnEventUsage:
			if ev.Usage != nil {
				totalIn += ev.Usage.InputTokens
				totalOut += ev.Usage.OutputTokens
				totalCacheRead += ev.Usage.CacheRead
				totalCacheCreated += ev.Usage.CacheCreated
			}

		case agent.TurnEventPermission:
			if ev.PermissionRequest != nil {
				ev.PermissionRequest.ResponseCh <- r.awaitPermission(ctx, item.TaskID, ev.PermissionRequest, waitTimeout, permissionCh, &waitingForInput, pg.release, pg.reacquire)
			}

		case agent.TurnEventError:
			if ev.Err != nil {
				taskErr = ev.Err
			}
		}
	}

	metricTaskDuration.Observe(time.Since(startedAt).Seconds())
	metricLLMInputTokens.Add(float64(totalIn))
	metricLLMOutputTokens.Add(float64(totalOut))
	metricLLMCacheReadTokens.Add(float64(totalCacheRead))
	metricLLMCacheCreatedTokens.Add(float64(totalCacheCreated))

	// If the event loop exited without a taskErr but the context was cancelled,
	// treat it as cancellation (happens when executeSingleTool returns nil early).
	if taskErr == nil && errors.Is(ctx.Err(), context.Canceled) {
		taskErr = context.Canceled
	}

	// Use a fresh context for all post-task DB writes and hub events — the task
	// context (ctx) may already be cancelled, which would silently drop these calls.
	saveCtx := context.Background()

	if taskErr != nil {
		if errors.Is(taskErr, context.Canceled) && atomic.LoadInt32(&timedOut) == 1 {
			const msg = "Task exceeded maximum execution time"
			r.persistFinalResult(saveCtx, delivery, item.TaskID, "failed", "", msg, outputBuf.String(), totalIn, totalOut)
			r.hub.Publish(item.TaskID, ServerEvent{Type: "error", Error: msg})
		} else if errors.Is(taskErr, context.Canceled) {
			r.persistFinalResult(saveCtx, delivery, item.TaskID, "cancelled", "", "Task cancelled by user", outputBuf.String(), totalIn, totalOut)
			r.hub.Publish(item.TaskID, ServerEvent{Type: "error", Error: "Task cancelled by user"})
		} else {
			r.persistFinalResult(saveCtx, delivery, item.TaskID, "failed", "", taskErr.Error(), outputBuf.String(), totalIn, totalOut)
			r.hub.Publish(item.TaskID, ServerEvent{Type: "error", Error: taskErr.Error()})
		}
		r.hub.Close(item.TaskID)
		return
	}

	r.persistFinalResult(saveCtx, delivery, item.TaskID, "done", prURL, "", outputBuf.String(), totalIn, totalOut)
	r.hub.Publish(item.TaskID, ServerEvent{Type: "done", PRUrl: prURL})
	r.hub.Close(item.TaskID)
}

// taskOutcomeWindowSize bounds how many of the most recent task outcomes
// TaskErrorRate considers. A rolling window rather than a lifetime ratio, so
// /healthz reflects "is this happening right now" — a lifetime ratio would
// stay permanently elevated after a single bad hour, long after recovery.
const taskOutcomeWindowSize = 100

// taskOutcomeWindow is a small fixed-capacity ring buffer of the most
// recent task terminal outcomes (true = failed, false = done/cancelled),
// guarded by its own mutex. Its zero value is ready to use. Backs
// Runner.TaskErrorRate.
type taskOutcomeWindow struct {
	mu      sync.Mutex
	entries [taskOutcomeWindowSize]bool
	count   int // number of entries written so far, caps at len(entries)
	next    int // ring cursor: index the next record() writes to
}

func (w *taskOutcomeWindow) record(failed bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.entries[w.next] = failed
	w.next = (w.next + 1) % len(w.entries)
	if w.count < len(w.entries) {
		w.count++
	}
}

func (w *taskOutcomeWindow) snapshot() (failed, total int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	total = w.count
	for i := 0; i < w.count; i++ {
		if w.entries[i] {
			failed++
		}
	}
	return failed, total
}

// TaskErrorRate returns the fraction of "failed" outcomes among the most
// recent taskOutcomeWindowSize completed tasks, along with the raw
// failed/total counts backing that fraction. A user-initiated "cancelled"
// outcome counts toward total but not failed — it isn't a signal of server
// health the way an actual task failure is. total is 0 (and rate 0) until
// at least one task has completed since process start.
func (r *Runner) TaskErrorRate() (rate float64, failed, total int) {
	failed, total = r.outcomes.snapshot()
	if total == 0 {
		return 0, 0, 0
	}
	return float64(failed) / float64(total), failed, total
}

// persistFinalResult writes a task's terminal outcome to the store and only
// then acks the queue delivery — a failed write naks instead, so the queue
// redelivers and the whole task is retried rather than silently lost. This is
// the "ack only after persistence" invariant for the durable queue.
func (r *Runner) persistFinalResult(ctx context.Context, delivery queue.Delivery, taskID, status, prURL, errMsg, output string, inputTokens, outputTokens int) {
	metricTasksCompleted.WithLabelValues(status).Inc()
	r.outcomes.record(status == "failed")
	if err := r.store.UpdateTaskResult(ctx, taskID, status, prURL, errMsg, output, inputTokens, outputTokens); err != nil {
		r.logger.Error("failed to update task result", "task_id", taskID, "status", status, "err", err)
		if nakErr := delivery.Nak(); nakErr != nil {
			r.logger.Error("failed to nak task after persistence failure", "task_id", taskID, "err", nakErr)
		}
		return
	}
	if err := delivery.Ack(); err != nil {
		r.logger.Error("failed to ack completed task", "task_id", taskID, "err", err)
	}
}

// isTerminalStatus reports whether a task status is a finished state that
// should never be re-executed.
func isTerminalStatus(status string) bool {
	switch status {
	case "done", "failed", "cancelled":
		return true
	default:
		return false
	}
}

func (r *Runner) fail(_ context.Context, taskID, msg string, delivery queue.Delivery) {
	r.persistFinalResult(context.Background(), delivery, taskID, "failed", "", msg, "", 0, 0)
	r.hub.Publish(taskID, ServerEvent{Type: "error", Error: msg})
	r.hub.Close(taskID)
}

func (r *Runner) wireAgent(ctx context.Context, item queue.Item) (*agent.Agent, error) {
	cwd := item.OutputDir
	if cwd == "" {
		var err error
		cwd, err = os.MkdirTemp("", "tf-agent-*")
		if err != nil {
			return nil, err
		}
	} else {
		_ = os.MkdirAll(cwd, 0755)
	}

	model := llm.ModelName(r.cfg)

	toolReg := tools.NewRegistry()
	toolReg.Register(tools.NewReadTool(cwd))
	toolReg.Register(tools.NewWriteTool(cwd))
	toolReg.Register(tools.NewEditTool(cwd))
	toolReg.Register(tools.NewGlobTool(cwd))
	toolReg.Register(tools.NewGrepTool(cwd))
	toolReg.Register(tools.NewLsTool(cwd))
	toolReg.Register(&tools.BashTool{})
	toolReg.Register(&tools.TaskTool{})
	toolReg.Register(&tools.AskUserTool{})
	toolReg.Register(tools.NewAgentTool(r.buildSubAgentRunner(cwd)))

	// sandboxExecutor is always constructed: ValidateSkill/SecurityScanSkill
	// require a real sandbox.Executor and no longer fall back to running
	// terraform/tflint/checkov directly on the host — see internal/sandbox
	// and docs/sandbox.md. sandbox_backend selects which Executor
	// implementation backs it; anything other than "kubernetes" (including
	// empty, the default) uses the Docker backend.
	var sandboxExecutor sandbox.Executor
	switch r.cfg.Server.SandboxBackend {
	case "kubernetes":
		k8sExecutor, err := sandbox.NewK8sJobExecutor(
			r.cfg.Server.SandboxKubeconfigPath,
			r.cfg.Server.SandboxKubeNamespace,
			r.cfg.Server.SandboxImage,
			r.cfg.Server.SandboxKubeMemory,
			r.cfg.Server.SandboxKubeCPUs,
		)
		if err != nil {
			return nil, fmt.Errorf("sandbox: build kubernetes executor: %w", err)
		}
		sandboxExecutor = k8sExecutor
	default:
		sandboxExecutor = sandbox.NewDockerExecutor(
			r.cfg.Server.SandboxImage,
			r.cfg.Server.SandboxMemory,
			r.cfg.Server.SandboxCPUs,
		)
	}

	skillReg := skills.NewRegistry()
	skillReg.Register(skills.NewRepoScanSkill(r.store))
	skillReg.Register(skills.NewClarifierSkill(r.provider, model))
	skillReg.Register(skills.NewGenerateSkill(cwd))
	skillReg.Register(skills.NewValidateSkill(sandboxExecutor))
	skillReg.Register(&skills.CreatePRSkill{})
	skillReg.Register(skills.NewSecurityScanSkill(sandboxExecutor))
	skillReg.Register(&skills.JiraFetchSkill{})
	skillReg.Register(&skills.DriftDetectSkill{})

	sessDir := filepath.Join(os.Getenv("HOME"), ".tf-agent", "sessions")
	_ = os.MkdirAll(sessDir, 0755)
	sess, err := session.New(sessDir, "")
	if err != nil {
		return nil, err
	}

	perm := permissions.NewChecker(&r.cfg.Permissions)
	hookRunner := hooks.NewRunner(&r.cfg.Hooks)

	var totalIn, totalOut int
	cmdRegistry := commands.NewRegistry()
	commands.RegisterAll(cmdRegistry, sess, &model, &totalIn, &totalOut, func() { sess.Clear() }, r.cfg, skillReg)

	agentMD := session.LoadAgentMD(cwd)

	ag := agent.NewAgent(
		r.provider, toolReg, skillReg, sess,
		perm, hookRunner, cmdRegistry,
		r.cfg, cwd, model, agentMD,
	)
	return ag, nil
}

// buildSubAgentRunner returns a SubAgentRunner closure that wires and runs an
// isolated sub-agent for the given cwd and role.  The closure is injected into
// AgentTool so that internal/tools does not need to import internal/agent.
func (r *Runner) buildSubAgentRunner(cwd string) tools.SubAgentRunner {
	return func(ctx context.Context, prompt, role string, timeoutSecs int) (string, error) {
		if timeoutSecs <= 0 {
			timeoutSecs = 120
		}
		ctx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSecs)*time.Second)
		defer cancel()
		model := llm.ModelName(r.cfg)

		toolReg := tools.NewRegistry()
		switch role {
		case "reviewer":
			toolReg.Register(tools.NewReadTool(cwd))
			toolReg.Register(tools.NewGlobTool(cwd))
			toolReg.Register(tools.NewGrepTool(cwd))
			toolReg.Register(tools.NewLsTool(cwd))
		case "coder":
			toolReg.Register(tools.NewReadTool(cwd))
			toolReg.Register(tools.NewWriteTool(cwd))
			toolReg.Register(tools.NewEditTool(cwd))
			toolReg.Register(tools.NewGlobTool(cwd))
			toolReg.Register(tools.NewGrepTool(cwd))
			toolReg.Register(tools.NewLsTool(cwd))
			toolReg.Register(&tools.BashTool{})
		case "tester":
			toolReg.Register(tools.NewReadTool(cwd))
			toolReg.Register(&tools.BashTool{})
			toolReg.Register(tools.NewGlobTool(cwd))
			toolReg.Register(tools.NewGrepTool(cwd))
		case "security-auditor":
			toolReg.Register(tools.NewReadTool(cwd))
			toolReg.Register(&tools.BashTool{})
			toolReg.Register(tools.NewGlobTool(cwd))
			toolReg.Register(tools.NewGrepTool(cwd))
		default:
			toolReg.Register(tools.NewReadTool(cwd))
			toolReg.Register(tools.NewGlobTool(cwd))
			toolReg.Register(tools.NewGrepTool(cwd))
			toolReg.Register(tools.NewLsTool(cwd))
			toolReg.Register(&tools.BashTool{})
		}

		skillReg := skills.NewRegistry()

		sess, err := session.New(os.TempDir(), "")
		if err != nil {
			return "", fmt.Errorf("sub-agent session: %w", err)
		}

		perm := permissions.NewChecker(&config.PermissionsConfig{Default: "auto"})
		hookRunner := hooks.NewRunner(&config.HooksConfig{})
		cmdRegistry := commands.NewRegistry()

		ag := agent.NewAgent(
			r.provider, toolReg, skillReg, sess,
			perm, hookRunner, cmdRegistry,
			r.cfg, cwd, model, "",
		)

		var sb strings.Builder
		for ev := range ag.RunTurn(ctx, prompt) {
			if ev.Type == agent.TurnEventText {
				sb.WriteString(ev.Text)
			}
		}
		return sb.String(), nil
	}
}

func (r *Runner) buildPrompt(item queue.Item) string {
	repoLine := ""
	if item.RepoURL != "" {
		repoLine = fmt.Sprintf("\nTarget repo for PR: %s", item.RepoURL)
	}
	switch item.InputType {
	case "jira":
		return fmt.Sprintf(
			"Fetch Jira ticket %s and implement the infrastructure described in it. Output type: %s.%s",
			item.InputText, item.OutputType, repoLine,
		)
	default:
		return fmt.Sprintf("%s\n\nOutput type: %s.%s", item.InputText, item.OutputType, repoLine)
	}
}
