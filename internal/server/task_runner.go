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

	answerMu sync.Mutex
	answers  map[string]chan string // taskID → pending answer channel

	permissionMu sync.Mutex
	permissions  map[string]chan bool // taskID → pending permission-response channel

	cancelMu sync.Mutex
	cancels  map[string]context.CancelFunc // taskID → cancel func

	userSemMu sync.Mutex
	userSems  map[string]chan struct{} // userID → per-user semaphore

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

// CancelTask cancels a running task. Returns an error if the task is not running.
func (r *Runner) CancelTask(taskID string) error {
	r.cancelMu.Lock()
	cancel, ok := r.cancels[taskID]
	r.cancelMu.Unlock()
	if !ok {
		return fmt.Errorf("task %s is not running", taskID)
	}
	cancel()
	return nil
}

// SendAnswer delivers a user answer to a task that is waiting_for_input.
// Returns an error if the task is not currently waiting.
func (r *Runner) SendAnswer(taskID, answer string) error {
	r.answerMu.Lock()
	ch, ok := r.answers[taskID]
	r.answerMu.Unlock()
	if !ok {
		return fmt.Errorf("task %s is not waiting for input", taskID)
	}
	select {
	case ch <- answer:
		return nil
	default:
		return fmt.Errorf("task %s answer channel full", taskID)
	}
}

// SendPermissionResponse delivers a user's allow/deny decision to a task
// that is currently paused asking whether a tool may run. Returns an error
// if the task is not currently waiting on a permission decision.
func (r *Runner) SendPermissionResponse(taskID string, allow bool) error {
	r.permissionMu.Lock()
	ch, ok := r.permissions[taskID]
	r.permissionMu.Unlock()
	if !ok {
		return fmt.Errorf("task %s is not waiting for a permission decision", taskID)
	}
	select {
	case ch <- allow:
		return nil
	default:
		return fmt.Errorf("task %s permission channel full", taskID)
	}
}

// awaitPermission pauses the task on a "confirm this tool call" prompt: it
// publishes a permission_request SSE event and blocks until the user
// responds via SendPermissionResponse, the wait times out (denies, since
// timing out on a destructive-tool confirmation should not silently allow
// it), or the task's context is cancelled (denies).
func (r *Runner) awaitPermission(ctx context.Context, taskID string, req *agent.PermissionRequest, waitTimeoutSeconds int, ch <-chan bool, waitingForInput *int32) bool {
	atomic.StoreInt32(waitingForInput, 1)
	defer atomic.StoreInt32(waitingForInput, 0)

	preview := previewToolInput(req.Input)

	if err := r.store.UpdateTaskStatus(ctx, taskID, "waiting_for_input"); err != nil {
		r.logger.Error("failed to update task status", "task_id", taskID, "status", "waiting_for_input", "err", err)
	}
	if err := r.store.UpdateTaskPendingQuestion(ctx, taskID, fmt.Sprintf("Approve %s: %s", req.ToolName, preview)); err != nil {
		r.logger.Error("failed to update task pending question", "task_id", taskID, "err", err)
	}
	r.hub.Publish(taskID, ServerEvent{Type: "permission_request", Tool: req.ToolName, Text: preview})

	defer func() {
		if err := r.store.UpdateTaskPendingQuestion(ctx, taskID, ""); err != nil {
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
	return &Runner{
		hub:         hub,
		store:       store,
		queue:       q,
		provider:    provider,
		cfg:         cfg,
		sem:         make(chan struct{}, concurrency),
		logger:      logger,
		answers:     make(map[string]chan string),
		permissions: make(map[string]chan bool),
		cancels:     make(map[string]context.CancelFunc),
		userSems:    make(map[string]chan struct{}),
	}
}

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

	// Enforce per-user concurrency limit.
	if limit := r.cfg.Server.PerUserConcurrency; limit > 0 {
		r.userSemMu.Lock()
		if _, ok := r.userSems[item.UserID]; !ok {
			r.userSems[item.UserID] = make(chan struct{}, limit)
		}
		userSem := r.userSems[item.UserID]
		r.userSemMu.Unlock()
		select {
		case userSem <- struct{}{}:
		case <-ctx.Done():
			return
		}
		defer func() { <-userSem }()
	}

	// Acquire global semaphore.
	select {
	case r.sem <- struct{}{}:
	case <-ctx.Done():
		return
	}
	defer func() { <-r.sem }()

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

		if err := r.store.UpdateTaskStatus(ctx, item.TaskID, "waiting_for_input"); err != nil {
			r.logger.Error("failed to update task status", "task_id", item.TaskID, "status", "waiting_for_input", "err", err)
		}
		if err := r.store.UpdateTaskPendingQuestion(ctx, item.TaskID, question); err != nil {
			r.logger.Error("failed to update task pending question", "task_id", item.TaskID, "err", err)
		}
		r.hub.Publish(item.TaskID, ServerEvent{Type: "waiting_for_input", Text: question})

		select {
		case answer := <-answerCh:
			if err := r.store.UpdateTaskPendingQuestion(ctx, item.TaskID, ""); err != nil {
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
			}

		case agent.TurnEventPermission:
			if ev.PermissionRequest != nil {
				ev.PermissionRequest.ResponseCh <- r.awaitPermission(ctx, item.TaskID, ev.PermissionRequest, waitTimeout, permissionCh, &waitingForInput)
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

// persistFinalResult writes a task's terminal outcome to the store and only
// then acks the queue delivery — a failed write naks instead, so the queue
// redelivers and the whole task is retried rather than silently lost. This is
// the "ack only after persistence" invariant for the durable queue.
func (r *Runner) persistFinalResult(ctx context.Context, delivery queue.Delivery, taskID, status, prURL, errMsg, output string, inputTokens, outputTokens int) {
	metricTasksCompleted.WithLabelValues(status).Inc()
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

	skillReg := skills.NewRegistry()
	skillReg.Register(skills.NewRepoScanSkill(r.store))
	skillReg.Register(skills.NewClarifierSkill(r.provider, model))
	skillReg.Register(skills.NewGenerateSkill(cwd))
	skillReg.Register(&skills.ValidateSkill{})
	skillReg.Register(&skills.CreatePRSkill{})
	skillReg.Register(&skills.SecurityScanSkill{})
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
