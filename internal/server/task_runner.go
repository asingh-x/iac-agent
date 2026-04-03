package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

	cancelMu sync.Mutex
	cancels  map[string]context.CancelFunc // taskID → cancel func

	userSemMu sync.Mutex
	userSems  map[string]chan struct{} // userID → per-user semaphore
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

func NewRunner(hub *Hub, store db.Store, q queue.Queue, provider llm.Provider, cfg *config.Config, logger *slog.Logger) *Runner {
	concurrency := cfg.Server.LLMConcurrency
	if concurrency <= 0 {
		concurrency = 10
	}
	return &Runner{
		hub:      hub,
		store:    store,
		queue:    q,
		provider: provider,
		cfg:      cfg,
		sem:      make(chan struct{}, concurrency),
		logger:   logger,
		answers:  make(map[string]chan string),
		cancels:  make(map[string]context.CancelFunc),
		userSems: make(map[string]chan struct{}),
	}
}

// Start blocks, continuously pulling from the Runner's default queue.
// Run in a goroutine.
func (r *Runner) Start(ctx context.Context) {
	r.StartQueue(ctx, r.queue)
}

// StartQueue blocks, continuously pulling from q.
// Call in a goroutine for each named queue to give each its own worker loop.
func (r *Runner) StartQueue(ctx context.Context, q queue.Queue) {
	for {
		item, err := q.Pop(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			continue
		}
		go r.run(ctx, item)
	}
}

func (r *Runner) run(ctx context.Context, item queue.Item) {
	defer func() {
		if rec := recover(); rec != nil {
			r.logger.Error("panic in task runner", "task_id", item.TaskID, "panic", rec)
			r.fail(ctx, item.TaskID, fmt.Sprintf("internal error: %v", rec))
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

	taskCtx = taskctx.WithAskUser(taskCtx, func(askCtx context.Context, question string) (string, error) {
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
		r.fail(ctx, item.TaskID, fmt.Sprintf("agent setup: %v", err))
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
				ev.PermissionRequest.ResponseCh <- true
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
		if errors.Is(taskErr, context.Canceled) {
			metricTasksCompleted.WithLabelValues("cancelled").Inc()
			if err := r.store.UpdateTaskResult(saveCtx, item.TaskID, "cancelled", "", "Task cancelled by user", outputBuf.String(), totalIn, totalOut); err != nil {
				r.logger.Error("failed to update task result", "task_id", item.TaskID, "status", "cancelled", "err", err)
			}
			r.hub.Publish(item.TaskID, ServerEvent{Type: "error", Error: "Task cancelled by user"})
		} else {
			metricTasksCompleted.WithLabelValues("failed").Inc()
			if err := r.store.UpdateTaskResult(saveCtx, item.TaskID, "failed", "", taskErr.Error(), outputBuf.String(), totalIn, totalOut); err != nil {
				r.logger.Error("failed to update task result", "task_id", item.TaskID, "status", "failed", "err", err)
			}
			r.hub.Publish(item.TaskID, ServerEvent{Type: "error", Error: taskErr.Error()})
		}
		r.hub.Close(item.TaskID)
		return
	}

	metricTasksCompleted.WithLabelValues("done").Inc()
	if err := r.store.UpdateTaskResult(saveCtx, item.TaskID, "done", prURL, "", outputBuf.String(), totalIn, totalOut); err != nil {
		r.logger.Error("failed to update task result", "task_id", item.TaskID, "status", "done", "err", err)
	}
	r.hub.Publish(item.TaskID, ServerEvent{Type: "done", PRUrl: prURL})
	r.hub.Close(item.TaskID)
}

func (r *Runner) fail(_ context.Context, taskID, msg string) {
	metricTasksCompleted.WithLabelValues("failed").Inc()
	if err := r.store.UpdateTaskResult(context.Background(), taskID, "failed", "", msg, "", 0, 0); err != nil {
		r.logger.Error("failed to persist task failure", "task_id", taskID, "err", err)
	}
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
	skillReg.Register(&skills.RepoScanSkill{})
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
