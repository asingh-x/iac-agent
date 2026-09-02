package server_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"embed"

	"github.com/tf-agent/tf-agent/internal/config"
	"github.com/tf-agent/tf-agent/internal/db"
	"github.com/tf-agent/tf-agent/internal/llm"
	"github.com/tf-agent/tf-agent/internal/queue"
	"github.com/tf-agent/tf-agent/internal/server"
)

// sequencedProvider returns a different canned response on each successive
// Stream call, so a test can script "tool call, then (after the tool runs)
// a final text response" — exactly what's needed to drive a real permission
// pause through a full task run.
type sequencedProvider struct {
	mu    sync.Mutex
	calls [][]llm.Event
	idx   int
}

func (p *sequencedProvider) Stream(_ context.Context, _ llm.Request) (<-chan llm.Event, error) {
	p.mu.Lock()
	i := p.idx
	if i >= len(p.calls) {
		i = len(p.calls) - 1
	}
	p.idx++
	events := p.calls[i]
	p.mu.Unlock()

	ch := make(chan llm.Event, len(events))
	for _, e := range events {
		ch <- e
	}
	close(ch)
	return ch, nil
}

func (p *sequencedProvider) Name() string { return "sequenced-mock" }

// permTestEnv is a trimmed newTestEnv that takes an explicit llm.Provider,
// so tests can script tool-call sequences that require a real permission
// decision (something the fixed-response llm.MockProvider used by
// newTestEnv can't drive end to end). Optional opts are applied to cfg
// before the Runner is constructed, so a test can tune things Runner reads
// at construction time (e.g. cfg.Server.LLMConcurrency, which sizes r.sem).
func permTestEnv(t *testing.T, provider llm.Provider, opts ...func(*config.Config)) *testEnv {
	t.Helper()

	store := db.NewMemoryStore()
	t.Setenv("TF_AGENT_ENCRYPTION_KEY", "0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20")
	if err := server.LoadEncryptionKey(); err != nil {
		t.Fatalf("LoadEncryptionKey: %v", err)
	}

	ctx := context.Background()
	adminToken := "admin-secret-token"
	memberToken := "member-secret-token"
	if _, err := store.CreateUser(ctx, "admin", server.HashToken(adminToken), "admin"); err != nil {
		t.Fatalf("create admin: %v", err)
	}
	if _, err := store.CreateUser(ctx, "alice", server.HashToken(memberToken), "member"); err != nil {
		t.Fatalf("create member: %v", err)
	}

	cfg := config.Defaults()
	cfg.Server.LLMConcurrency = 5
	cfg.Agent.MaxTurns = 3
	cfg.Agent.MaxTokens = 512
	cfg.Provider.Anthropic.APIKey = "test-key"
	// Bash is "ask" by default (config.Defaults()) — that's the exact
	// behavior under test, kept explicit here so the test's intent survives
	// even if the default ever changes.
	cfg.Permissions.Bash = "ask"
	for _, opt := range opts {
		opt(cfg)
	}

	hub := server.NewHub()
	q := queue.NewMemoryQueue(100)
	queues := map[string]queue.Queue{"default": q}
	runner := server.NewRunner(hub, store, q, provider, cfg, slog.Default())
	srv := server.NewServer(store, hub, queues, runner, cfg, embed.FS{})
	ts := httptest.NewServer(srv.Handler())

	runCtx, cancel := context.WithCancel(context.Background())
	go runner.Start(runCtx)

	t.Cleanup(func() {
		cancel()
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 2*time.Second)
		runner.Shutdown(shutCtx)
		shutCancel()
		ts.Close()
		store.Close()
	})

	return &testEnv{
		ts:          ts,
		store:       store,
		adminToken:  adminToken,
		memberToken: memberToken,
		runner:      runner,
		cancel:      cancel,
		cfg:         cfg,
	}
}

// waitForStatus polls GET /v1/tasks/{id} until status is one of want, or
// fails the test after a few seconds.
func waitForStatus(t *testing.T, env *testEnv, taskID string, want ...string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last map[string]any
	for time.Now().Before(deadline) {
		r := env.do("GET", "/v1/tasks/"+taskID, env.memberToken, nil)
		var got map[string]any
		_ = json.NewDecoder(r.Body).Decode(&got)
		r.Body.Close()
		last = got
		status, _ := got["status"].(string)
		for _, w := range want {
			if status == w {
				return got
			}
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatalf("task %s never reached status in %v, last seen: %+v", taskID, want, last)
	return nil
}

func TestPermission_ApproveResumesTaskAndRunsTool(t *testing.T) {
	toolInput := json.RawMessage(`{"command":"echo approved"}`)
	provider := &sequencedProvider{calls: [][]llm.Event{
		{
			{Type: llm.EventToolUse, ToolUse: &llm.ToolUseEvent{ID: "call_1", Name: "bash", Input: toolInput}},
			{Type: llm.EventStop, StopReason: "tool_use"},
		},
		{
			{Type: llm.EventText, Delta: "done"},
			{Type: llm.EventStop, StopReason: "end_turn"},
		},
	}}
	env := permTestEnv(t, provider)

	submit := env.do("POST", "/v1/tasks", env.memberToken, map[string]any{
		"input":  map[string]string{"type": "prompt", "text": "run a command"},
		"output": map[string]string{"type": "print"},
	})
	if submit.StatusCode != http.StatusCreated {
		t.Fatalf("submit status = %d, want 201", submit.StatusCode)
	}
	var body map[string]string
	_ = json.NewDecoder(submit.Body).Decode(&body)
	taskID := body["task_id"]

	// The task must actually pause waiting for a permission decision — this
	// is exactly the behavior that was previously auto-approved with no real
	// pause at all.
	got := waitForStatus(t, env, taskID, "waiting_for_input", "done", "failed")
	if got["status"] != "waiting_for_input" {
		t.Fatalf("expected task to pause at waiting_for_input for the permission prompt, got status=%v", got["status"])
	}
	pendingQ, _ := got["pending_question"].(string)
	if !strings.Contains(pendingQ, "bash") || !strings.Contains(pendingQ, "echo approved") {
		t.Errorf("pending_question = %q, want it to describe the pending bash command", pendingQ)
	}

	// Approve.
	resp := env.do("POST", "/v1/tasks/"+taskID+"/permission", env.memberToken, map[string]any{"allow": true})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("permission approve status = %d, want 200", resp.StatusCode)
	}

	final := waitForStatus(t, env, taskID, "done", "failed")
	if final["status"] != "done" {
		t.Fatalf("expected task to complete successfully after approval, got status=%v", final)
	}
}

func TestPermission_DenyBlocksToolButTaskContinues(t *testing.T) {
	toolInput := json.RawMessage(`{"command":"echo should_not_run"}`)
	provider := &sequencedProvider{calls: [][]llm.Event{
		{
			{Type: llm.EventToolUse, ToolUse: &llm.ToolUseEvent{ID: "call_1", Name: "bash", Input: toolInput}},
			{Type: llm.EventStop, StopReason: "tool_use"},
		},
		{
			{Type: llm.EventText, Delta: "the command was denied, so I stopped"},
			{Type: llm.EventStop, StopReason: "end_turn"},
		},
	}}
	env := permTestEnv(t, provider)

	submit := env.do("POST", "/v1/tasks", env.memberToken, map[string]any{
		"input":  map[string]string{"type": "prompt", "text": "run a command"},
		"output": map[string]string{"type": "print"},
	})
	var body map[string]string
	_ = json.NewDecoder(submit.Body).Decode(&body)
	taskID := body["task_id"]

	waitForStatus(t, env, taskID, "waiting_for_input", "done", "failed")

	resp := env.do("POST", "/v1/tasks/"+taskID+"/permission", env.memberToken, map[string]any{"allow": false})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("permission deny status = %d, want 200", resp.StatusCode)
	}

	final := waitForStatus(t, env, taskID, "done", "failed")
	if final["status"] != "done" {
		t.Fatalf("expected task to complete (the agent handles a denial, it doesn't fail the task), got status=%v", final)
	}
	// The model's own text response is what lands in the task's stored
	// output (tool outputs, including the denial message, are streamed as
	// tool_end SSE events, not accumulated into the task's output field) —
	// confirming the agent saw the denial and reacted to it is enough to
	// prove the deny path actually ran, not just that the tool got skipped.
	output, _ := final["output"].(string)
	if !strings.Contains(strings.ToLower(output), "denied") {
		t.Errorf("expected the task output to reflect the tool was denied, got: %q", output)
	}
}

func TestPermission_ResponseWithoutPendingRequest_Returns409(t *testing.T) {
	env := permTestEnv(t, &sequencedProvider{calls: [][]llm.Event{
		{{Type: llm.EventText, Delta: "hi"}, {Type: llm.EventStop, StopReason: "end_turn"}},
	}})

	submit := env.do("POST", "/v1/tasks", env.memberToken, map[string]any{
		"input":  map[string]string{"type": "prompt", "text": "say hi"},
		"output": map[string]string{"type": "print"},
	})
	var body map[string]string
	_ = json.NewDecoder(submit.Body).Decode(&body)
	taskID := body["task_id"]

	waitForStatus(t, env, taskID, "done", "failed")

	resp := env.do("POST", "/v1/tasks/"+taskID+"/permission", env.memberToken, map[string]any{"allow": true})
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("status = %d, want 409 for a task with no pending permission request", resp.StatusCode)
	}
}

// TestConcurrentAskUserAndPermissionPauses_ShareOneSemaphoreRelease is an
// end-to-end coverage test for the real fan-out scenario a Critical
// concurrency bug was found in during review of the semaphore-release-on-
// pause change: a single LLM turn can batch an ask_user tool call together
// with another tool call that needs permission (e.g. bash) —
// internal/agent/loop.go's executeTools fans multiple tool calls from one
// turn out into parallel goroutines. Both the AskUser callback (run from
// the ask_user tool's own fanned-out goroutine) and awaitPermission (run
// from Runner.run's own event-loop goroutine) then pause CONCURRENTLY for
// the SAME task, even though that task only ever acquired one token from
// r.sem at the top of run(). This test proves that scenario completes
// end to end with pauseGate wired into both pause sites in run().
//
// NOTE: unlike pause_gate_test.go's TestPauseGate_* tests (the authoritative
// regression guard for the double-release bug itself, verified there to
// fail against the pre-fix pattern and pass against the fix), this
// particular end-to-end test does NOT reliably fail against the pre-fix
// code in this single-task setup: with LLMConcurrency=1 and both responses
// pre-buffered, the pre-fix bug's two concurrent bare receives on the one-
// token r.sem happen to serialize "by accident" through that same token
// (whichever pause loses the initial race blocks until the winner's
// deferred reacquire hands the token back), and since both pauses resolve
// almost instantly against pre-buffered answers, the accounting silently
// balances out. Verified empirically by running this exact test against
// the actual pre-fix commit and against a reverted-to-naive pauseGate — it
// passed both times. It's kept for its independent value (proving pauseGate
// is correctly wired into run(), not just correct in isolation, and that
// this batching scenario actually completes), not as a substitute for
// pause_gate_test.go's guard against the specific race.
//
// Overlap between the two pauses is not left to scheduling luck: the
// ask_user answer and the permission decision are both delivered here via
// Runner.SendAnswer/SendPermissionResponse, which just buffer into
// per-task channels (capacity 1) that Runner.run registers well before the
// LLM stream even starts — see the "Wire mid-session pause" section of
// run(). So this test starts delivering both responses immediately after
// the task exists, before either pause has necessarily even begun; each
// pause's release()/reacquire() must still run to completion — regardless
// of whether the answer/decision is already sitting in its channel — for
// the task to ever finish. waitForStatus's bounded poll is the only "wait"
// in this test, and it is polling for an HTTP-observable status transition
// (the same pattern every other test in this file uses), not a sleep
// guessing when a race will reproduce.
func TestConcurrentAskUserAndPermissionPauses_ShareOneSemaphoreRelease(t *testing.T) {
	askInput := json.RawMessage(`{"question":"which region?"}`)
	bashInput := json.RawMessage(`{"command":"echo hi"}`)
	provider := &sequencedProvider{calls: [][]llm.Event{
		{
			// Both tool calls in the SAME batch: internal/agent/loop.go's
			// executeTools only fans calls out into parallel goroutines
			// when there's more than one in a batch — this is what makes
			// the ask_user pause and the permission pause run concurrently
			// for this task.
			{Type: llm.EventToolUse, ToolUse: &llm.ToolUseEvent{ID: "call_ask", Name: "ask_user", Input: askInput}},
			{Type: llm.EventToolUse, ToolUse: &llm.ToolUseEvent{ID: "call_bash", Name: "bash", Input: bashInput}},
			{Type: llm.EventStop, StopReason: "tool_use"},
		},
		{
			{Type: llm.EventText, Delta: "done"},
			{Type: llm.EventStop, StopReason: "end_turn"},
		},
	}}

	// Pin LLMConcurrency to 1: this task holds the ONLY global slot, so a
	// double-release has nothing else on the pod to silently steal — it can
	// only manifest as the deadlock described above, making the bug's
	// symptom unambiguous (the task simply never finishes) instead of a
	// quieter capacity-accounting drift that would need a second task and
	// more machinery to observe.
	env := permTestEnv(t, provider, func(cfg *config.Config) {
		cfg.Server.LLMConcurrency = 1
	})

	submit := env.do("POST", "/v1/tasks", env.memberToken, map[string]any{
		"input":  map[string]string{"type": "prompt", "text": "look up the region and run a command"},
		"output": map[string]string{"type": "print"},
	})
	if submit.StatusCode != http.StatusCreated {
		t.Fatalf("submit status = %d, want 201", submit.StatusCode)
	}
	var body map[string]string
	_ = json.NewDecoder(submit.Body).Decode(&body)
	taskID := body["task_id"]

	// Deliver both responses as soon as possible after the task exists.
	// SendAnswer/SendPermissionResponse just push into per-task channels of
	// capacity 1 that are registered well before either pause begins (see
	// the doc comment above), so it's fine if these calls land before the
	// corresponding pause has started — retry briefly only because the task
	// may not have been dequeued and reached that registration point yet.
	pollUntilOK(t, func() int {
		resp := env.do("POST", "/v1/tasks/"+taskID+"/answer", env.memberToken, map[string]any{"answer": "us-east-1"})
		return resp.StatusCode
	})
	pollUntilOK(t, func() int {
		resp := env.do("POST", "/v1/tasks/"+taskID+"/permission", env.memberToken, map[string]any{"allow": true})
		return resp.StatusCode
	})

	// The assertion: both the ask_user pause and the permission pause must
	// resolve and let the task finish, proving pauseGate is wired correctly
	// into both sites (see the function doc comment for why this doesn't
	// double as a reliable pre-fix/post-fix regression signal on its own).
	final := waitForStatus(t, env, taskID, "done", "failed")
	if final["status"] != "done" {
		t.Fatalf("expected task to complete after both the ask_user answer and the permission decision were delivered, got status=%v", final)
	}
}

// pollUntilOK retries fn (expected to be a side-effecting HTTP call
// returning a status code) until it returns 200, or fails the test after a
// few seconds. Used where a call can legitimately 409 for a moment before
// Runner.run has registered the per-task state it targets.
func pollUntilOK(t *testing.T, fn func() int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last int
	for time.Now().Before(deadline) {
		last = fn()
		if last == http.StatusOK {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("call never returned 200 within deadline, last status: %d", last)
}
