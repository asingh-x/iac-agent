package server_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"embed"

	"github.com/tf-agent/tf-agent/internal/config"
	"github.com/tf-agent/tf-agent/internal/db"
	"github.com/tf-agent/tf-agent/internal/llm"
	"github.com/tf-agent/tf-agent/internal/queue"
	"github.com/tf-agent/tf-agent/internal/server"
)

// testEnv holds the full wired server for a test.
type testEnv struct {
	ts          *httptest.Server
	store       db.Store
	adminToken  string
	memberToken string
	runner      *server.Runner
	cancel      context.CancelFunc
	cfg         *config.Config
	provider    *llm.MockProvider
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()

	store := db.NewMemoryStore()

	// Each test gets its own encryption key so tests are isolated.
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
	_, err := store.CreateUser(ctx, "alice", server.HashToken(memberToken), "member")
	if err != nil {
		t.Fatalf("create member: %v", err)
	}

	// Mock LLM — returns a simple text response then stops.
	mockEvents := []llm.Event{
		{Type: llm.EventText, Delta: "resource \"aws_s3_bucket\" \"main\" {}"},
		{Type: llm.EventStop, StopReason: "end_turn"},
	}
	mockProvider := llm.NewMockProvider("mock", mockEvents)

	cfg := config.Defaults()
	cfg.Server.LLMConcurrency = 5
	cfg.Agent.MaxTurns = 2
	cfg.Agent.MaxTokens = 512
	cfg.Provider.Anthropic.APIKey = "test-key" // satisfy health check; mock provider is used

	hub := server.NewHub()
	q := queue.NewMemoryQueue(100)
	queues := map[string]queue.Queue{"default": q}
	runner := server.NewRunner(hub, store, q, mockProvider, cfg, slog.Default())
	srv := server.NewServer(store, hub, queues, runner, cfg, embed.FS{})

	ts := httptest.NewServer(srv.Handler())

	runCtx, cancel := context.WithCancel(context.Background())
	go runner.Start(runCtx)

	t.Cleanup(func() {
		cancel() // stop the queue pop loop from picking up new work
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 2*time.Second)
		runner.Shutdown(shutCtx) // force-cancel any still in-flight task before closing the store
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
		provider:    mockProvider,
	}
}

func (e *testEnv) do(method, path, token string, body any) *http.Response {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req, _ := http.NewRequest(method, e.ts.URL+path, &buf)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, _ := http.DefaultClient.Do(req)
	return resp
}

// --- Auth ---

func TestServer_NoToken_Unauthorized(t *testing.T) {
	env := newTestEnv(t)
	resp := env.do("GET", "/v1/tasks", "", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func TestServer_InvalidToken_Unauthorized(t *testing.T) {
	env := newTestEnv(t)
	resp := env.do("GET", "/v1/tasks", "wrong-token", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func TestServer_AdminRoute_ForbiddenForMember(t *testing.T) {
	env := newTestEnv(t)
	resp := env.do("GET", "/v1/admin/users", env.memberToken, nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
}

// --- Health ---

func TestServer_Health(t *testing.T) {
	env := newTestEnv(t)
	resp := env.do("GET", "/healthz", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body["status"] != "ok" {
		t.Errorf("health status = %v, want ok", body["status"])
	}
	if _, ok := body["queue_len"]; !ok {
		t.Errorf("health response missing queue_len: %v", body)
	}
	depth, ok := body["queue_depth"].(map[string]any)
	if !ok {
		t.Fatalf("health response missing queue_depth map: %v", body)
	}
	if _, ok := depth["default"]; !ok {
		t.Errorf("queue_depth missing \"default\" entry: %v", depth)
	}
}

// TestServer_Health_QueueDepth verifies /healthz reports the actual pending
// item count per named queue, and that the tfagent_queue_depth Prometheus
// gauge is registered and reflects the same value at /metrics.
func TestServer_Health_QueueDepth(t *testing.T) {
	store := db.NewMemoryStore()
	t.Cleanup(func() { store.Close() })

	t.Setenv("TF_AGENT_ENCRYPTION_KEY", "0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20")
	if err := server.LoadEncryptionKey(); err != nil {
		t.Fatalf("LoadEncryptionKey: %v", err)
	}

	cfg := config.Defaults()
	cfg.Provider.Anthropic.APIKey = "test-key"

	// Push items onto two named queues without starting a runner, so the
	// items stay put and depth reporting can be asserted deterministically.
	defaultQ := queue.NewMemoryQueue(100)
	highQ := queue.NewMemoryQueue(100)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		item := queue.Item{TaskID: fmt.Sprintf("t%d", i), InputType: "prompt", InputText: "x", OutputType: "print"}
		if err := defaultQ.Push(ctx, item); err != nil {
			t.Fatalf("push default: %v", err)
		}
	}
	if err := highQ.Push(ctx, queue.Item{TaskID: "h0", InputType: "prompt", InputText: "x", OutputType: "print"}); err != nil {
		t.Fatalf("push high: %v", err)
	}

	queues := map[string]queue.Queue{"default": defaultQ, "high": highQ}
	srv := server.NewServer(store, server.NewHub(), queues, nil, cfg, embed.FS{})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	var body struct {
		QueueLen   int            `json:"queue_len"`
		QueueDepth map[string]int `json:"queue_depth"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode healthz body: %v", err)
	}
	if body.QueueLen != 4 {
		t.Errorf("queue_len = %d, want 4", body.QueueLen)
	}
	if body.QueueDepth["default"] != 3 {
		t.Errorf("queue_depth[default] = %d, want 3", body.QueueDepth["default"])
	}
	if body.QueueDepth["high"] != 1 {
		t.Errorf("queue_depth[high] = %d, want 1", body.QueueDepth["high"])
	}

	// The healthz call above should have updated the Prometheus gauge; confirm
	// it is registered and scraped with the matching per-queue values.
	mresp, err := http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	data, err := io.ReadAll(mresp.Body)
	if err != nil {
		t.Fatalf("read /metrics body: %v", err)
	}
	metrics := string(data)
	if !strings.Contains(metrics, `tfagent_queue_depth{queue="default"} 3`) {
		t.Errorf("/metrics missing tfagent_queue_depth for default queue at 3:\n%s", metrics)
	}
	if !strings.Contains(metrics, `tfagent_queue_depth{queue="high"} 1`) {
		t.Errorf("/metrics missing tfagent_queue_depth for high queue at 1:\n%s", metrics)
	}
}

// --- Admin: create user ---

func TestServer_AdminCreateUser(t *testing.T) {
	env := newTestEnv(t)

	resp := env.do("POST", "/v1/admin/users", env.adminToken, map[string]string{
		"username": "bob",
		"token":    "bob-secret-token",
		"role":     "member",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("status = %d, want 201", resp.StatusCode)
	}

	var body map[string]string
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body["username"] != "bob" {
		t.Errorf("username = %q, want bob", body["username"])
	}
}

func TestServer_AdminCreateUser_MissingFields(t *testing.T) {
	env := newTestEnv(t)
	resp := env.do("POST", "/v1/admin/users", env.adminToken, map[string]string{
		"username": "incomplete",
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestServer_AdminListUsers(t *testing.T) {
	env := newTestEnv(t)
	resp := env.do("GET", "/v1/admin/users", env.adminToken, nil)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	var users []map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&users)
	if len(users) < 2 {
		t.Errorf("expected at least 2 users, got %d", len(users))
	}
	// Token hashes must not be in the response.
	for _, u := range users {
		if _, ok := u["token_hash"]; ok {
			t.Error("token_hash must not be returned in user list")
		}
	}
}

// --- Task submission ---

func TestServer_SubmitTask_Prompt_Print(t *testing.T) {
	env := newTestEnv(t)

	resp := env.do("POST", "/v1/tasks", env.memberToken, map[string]any{
		"input":  map[string]string{"type": "prompt", "text": "create S3 bucket"},
		"output": map[string]string{"type": "print"},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("status = %d, want 201", resp.StatusCode)
	}

	var body map[string]string
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body["task_id"] == "" {
		t.Error("expected task_id in response")
	}
	if body["stream_url"] == "" {
		t.Error("expected stream_url in response")
	}
}

func TestServer_SubmitTask_InvalidInputType(t *testing.T) {
	env := newTestEnv(t)
	resp := env.do("POST", "/v1/tasks", env.memberToken, map[string]any{
		"input":  map[string]string{"type": "unknown"},
		"output": map[string]string{"type": "print"},
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestServer_SubmitTask_PR_MissingToken(t *testing.T) {
	env := newTestEnv(t)
	resp := env.do("POST", "/v1/tasks", env.memberToken, map[string]any{
		"input":  map[string]string{"type": "prompt", "text": "create EKS"},
		"output": map[string]string{"type": "pr"}, // missing github_token and repo_url
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestServer_SubmitTask_EmptyPrompt(t *testing.T) {
	env := newTestEnv(t)
	resp := env.do("POST", "/v1/tasks", env.memberToken, map[string]any{
		"input":  map[string]string{"type": "prompt", "text": ""},
		"output": map[string]string{"type": "print"},
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

// --- Get task ---

func TestServer_GetTask(t *testing.T) {
	env := newTestEnv(t)

	submit := env.do("POST", "/v1/tasks", env.memberToken, map[string]any{
		"input":  map[string]string{"type": "prompt", "text": "create RDS"},
		"output": map[string]string{"type": "print"},
	})
	var submitBody map[string]string
	_ = json.NewDecoder(submit.Body).Decode(&submitBody)
	taskID := submitBody["task_id"]

	resp := env.do("GET", "/v1/tasks/"+taskID, env.memberToken, nil)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}

	var task map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&task)
	if task["id"] != taskID {
		t.Errorf("task id = %v, want %s", task["id"], taskID)
	}
}

func TestServer_GetTask_NotFound(t *testing.T) {
	env := newTestEnv(t)
	resp := env.do("GET", "/v1/tasks/nonexistent-task-id", env.memberToken, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestServer_GetTask_OtherUserForbidden(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	// Create a second member user.
	secondToken := "second-user-token"
	_, _ = env.store.CreateUser(ctx, "bob2", server.HashToken(secondToken), "member")

	// Alice submits a task.
	submit := env.do("POST", "/v1/tasks", env.memberToken, map[string]any{
		"input":  map[string]string{"type": "prompt", "text": "alice task"},
		"output": map[string]string{"type": "print"},
	})
	var submitBody map[string]string
	_ = json.NewDecoder(submit.Body).Decode(&submitBody)
	taskID := submitBody["task_id"]

	// Bob tries to access Alice's task.
	resp := env.do("GET", "/v1/tasks/"+taskID, secondToken, nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
}

// --- List tasks ---

func TestServer_ListTasks(t *testing.T) {
	env := newTestEnv(t)

	// Submit 2 tasks.
	for _, text := range []string{"task one", "task two"} {
		env.do("POST", "/v1/tasks", env.memberToken, map[string]any{
			"input":  map[string]string{"type": "prompt", "text": text},
			"output": map[string]string{"type": "print"},
		})
	}

	resp := env.do("GET", "/v1/tasks", env.memberToken, nil)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	var tasks []map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&tasks)
	if len(tasks) < 2 {
		t.Errorf("expected at least 2 tasks, got %d", len(tasks))
	}
}

// --- SSE streaming ---

func TestServer_StreamTask_ReceivesDoneEvent(t *testing.T) {
	env := newTestEnv(t)

	submit := env.do("POST", "/v1/tasks", env.memberToken, map[string]any{
		"input":  map[string]string{"type": "prompt", "text": "create VPC"},
		"output": map[string]string{"type": "print"},
	})
	var submitBody map[string]string
	_ = json.NewDecoder(submit.Body).Decode(&submitBody)
	taskID := submitBody["task_id"]

	// Open SSE stream with a timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, "GET", env.ts.URL+"/v1/tasks/"+taskID+"/stream", nil)
	req.Header.Set("Authorization", "Bearer "+env.memberToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("SSE request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("SSE status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}

	// Read events until done or timeout.
	var gotDone bool
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		var ev map[string]any
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			continue
		}
		if ev["type"] == "done" || ev["type"] == "error" {
			gotDone = true
			break
		}
	}

	if !gotDone {
		t.Error("expected to receive a done or error event from the SSE stream")
	}
}

func TestServer_StreamTask_NotFound(t *testing.T) {
	env := newTestEnv(t)
	resp := env.do("GET", "/v1/tasks/nonexistent/stream", env.memberToken, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// --- Hub unit tests ---

func TestHub_CreatePublishClose(t *testing.T) {
	hub := server.NewHub()
	ch := hub.Create("task-1")

	hub.Publish("task-1", server.ServerEvent{Type: "text", Text: "hello"})
	hub.Publish("task-1", server.ServerEvent{Type: "done"})

	ev1 := <-ch
	if ev1.Type != "text" {
		t.Errorf("ev1.Type = %q, want text", ev1.Type)
	}
	ev2 := <-ch
	if ev2.Type != "done" {
		t.Errorf("ev2.Type = %q, want done", ev2.Type)
	}

	hub.Close("task-1")
	_, open := <-ch
	if open {
		t.Error("channel should be closed after hub.Close")
	}
}

func TestHub_PublishToUnknownTask(t *testing.T) {
	hub := server.NewHub()
	// Should not panic.
	hub.Publish("does-not-exist", server.ServerEvent{Type: "text"})
}

// --- Auth helpers ---

func TestHashToken_Deterministic(t *testing.T) {
	h1 := server.HashToken("my-secret-token")
	h2 := server.HashToken("my-secret-token")
	if h1 != h2 {
		t.Error("HashToken should be deterministic")
	}
}

func TestHashToken_Different(t *testing.T) {
	h1 := server.HashToken("token-a")
	h2 := server.HashToken("token-b")
	if h1 == h2 {
		t.Error("different tokens should produce different hashes")
	}
}

// --- /v1/me ---

func TestServer_GetMe(t *testing.T) {
	env := newTestEnv(t)
	resp := env.do("GET", "/v1/me", env.memberToken, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/me status = %d, want 200", resp.StatusCode)
	}
	var body map[string]string
	decodeBody(resp, &body)
	if body["username"] != "alice" {
		t.Errorf("username = %q, want alice", body["username"])
	}
	if body["role"] != "member" {
		t.Errorf("role = %q, want member", body["role"])
	}
	if body["id"] == "" {
		t.Error("id must not be empty")
	}
}

func TestServer_GetMe_Unauthorized(t *testing.T) {
	env := newTestEnv(t)
	resp := env.do("GET", "/v1/me", "", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func TestServer_UpdateMe(t *testing.T) {
	env := newTestEnv(t)
	resp := env.do("PATCH", "/v1/me", env.memberToken, map[string]string{"username": "alice-renamed"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH /v1/me status = %d, want 200", resp.StatusCode)
	}
	var body map[string]string
	decodeBody(resp, &body)
	if body["username"] != "alice-renamed" {
		t.Errorf("username = %q, want alice-renamed", body["username"])
	}

	// Confirm the change persisted via GET /v1/me.
	resp2 := env.do("GET", "/v1/me", env.memberToken, nil)
	var body2 map[string]string
	decodeBody(resp2, &body2)
	if body2["username"] != "alice-renamed" {
		t.Errorf("persisted username = %q, want alice-renamed", body2["username"])
	}
}

func TestServer_UpdateMe_EmptyUsername(t *testing.T) {
	env := newTestEnv(t)
	resp := env.do("PATCH", "/v1/me", env.memberToken, map[string]string{"username": "  "})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

// --- /v1/settings ---

func TestServer_GetSettings_Empty(t *testing.T) {
	env := newTestEnv(t)
	resp := env.do("GET", "/v1/settings", env.memberToken, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/settings status = %d, want 200", resp.StatusCode)
	}
	var body map[string]any
	decodeBody(resp, &body)
	if body["github_token_set"] != false {
		t.Errorf("github_token_set = %v, want false", body["github_token_set"])
	}
	if body["atlassian_token_set"] != false {
		t.Errorf("atlassian_token_set = %v, want false", body["atlassian_token_set"])
	}
}

func TestServer_SaveAndGetGitHubToken(t *testing.T) {
	env := newTestEnv(t)

	// Save a GitHub token.
	resp := env.do("PUT", "/v1/settings", env.memberToken, map[string]string{
		"github_token": "ghp_testtoken123",
	})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("PUT /v1/settings status = %d, want 204", resp.StatusCode)
	}

	// GET settings must report token as configured.
	resp2 := env.do("GET", "/v1/settings", env.memberToken, nil)
	var body map[string]any
	decodeBody(resp2, &body)
	if body["github_token_set"] != true {
		t.Errorf("github_token_set = %v, want true", body["github_token_set"])
	}
	// Plaintext token must never be returned.
	if _, ok := body["github_token"]; ok {
		t.Error("github_token plaintext must not be returned")
	}
}

func TestServer_SaveAndGetAtlassianSettings(t *testing.T) {
	env := newTestEnv(t)

	resp := env.do("PUT", "/v1/settings", env.memberToken, map[string]any{
		"atlassian_token":  "ATATT3xtest",
		"atlassian_domain": "myco.atlassian.net",
		"atlassian_email":  "user@myco.com",
	})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("PUT /v1/settings status = %d, want 204", resp.StatusCode)
	}

	resp2 := env.do("GET", "/v1/settings", env.memberToken, nil)
	var body map[string]any
	decodeBody(resp2, &body)
	if body["atlassian_token_set"] != true {
		t.Errorf("atlassian_token_set = %v, want true", body["atlassian_token_set"])
	}
	if body["atlassian_domain"] != "myco.atlassian.net" {
		t.Errorf("atlassian_domain = %v, want myco.atlassian.net", body["atlassian_domain"])
	}
	if body["atlassian_email"] != "user@myco.com" {
		t.Errorf("atlassian_email = %v, want user@myco.com", body["atlassian_email"])
	}
}

func TestServer_Settings_UpdateTokenOnly(t *testing.T) {
	env := newTestEnv(t)

	// Save initial settings.
	env.do("PUT", "/v1/settings", env.memberToken, map[string]any{
		"github_token":     "ghp_initial",
		"atlassian_domain": "myco.atlassian.net",
	})

	// Update only the domain; token should be preserved.
	env.do("PUT", "/v1/settings", env.memberToken, map[string]any{
		"atlassian_domain": "newco.atlassian.net",
	})

	resp := env.do("GET", "/v1/settings", env.memberToken, nil)
	var body map[string]any
	decodeBody(resp, &body)
	if body["github_token_set"] != true {
		t.Error("github_token must be preserved after a partial update")
	}
	if body["atlassian_domain"] != "newco.atlassian.net" {
		t.Errorf("atlassian_domain = %v, want newco.atlassian.net", body["atlassian_domain"])
	}
}

func TestServer_Settings_IsolatedPerUser(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	secondToken := "second-user-token-settings"
	_, _ = env.store.CreateUser(ctx, "bob-settings", server.HashToken(secondToken), "member")

	// Alice saves a GitHub token.
	env.do("PUT", "/v1/settings", env.memberToken, map[string]string{"github_token": "ghp_alice"})

	// Bob must not see Alice's token.
	resp := env.do("GET", "/v1/settings", secondToken, nil)
	var body map[string]any
	decodeBody(resp, &body)
	if body["github_token_set"] != false {
		t.Error("bob must not see alice's github token")
	}
}

func TestServer_SubmitTask_PR_UsesSettingsToken(t *testing.T) {
	env := newTestEnv(t)

	// Save a GitHub token via settings.
	env.do("PUT", "/v1/settings", env.memberToken, map[string]string{"github_token": "ghp_from_settings"})

	// Submit a PR task without passing github_token in the body.
	resp := env.do("POST", "/v1/tasks", env.memberToken, map[string]any{
		"input":  map[string]string{"type": "prompt", "text": "create S3 bucket"},
		"output": map[string]any{"type": "pr", "repo_url": "https://github.com/example/repo"},
	})
	if resp.StatusCode != http.StatusCreated {
		var errBody map[string]string
		_ = json.NewDecoder(resp.Body).Decode(&errBody)
		t.Fatalf("status = %d, want 201; error = %v", resp.StatusCode, errBody["error"])
	}
}

func TestServer_SubmitTask_PR_NoTokenAnywhere(t *testing.T) {
	env := newTestEnv(t)

	// No settings token set, no token in body → should fail.
	resp := env.do("POST", "/v1/tasks", env.memberToken, map[string]any{
		"input":  map[string]string{"type": "prompt", "text": "create EKS"},
		"output": map[string]any{"type": "pr", "repo_url": "https://github.com/example/repo"},
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 when no GitHub token configured", resp.StatusCode)
	}
}

// helper: read JSON body
func decodeBody(resp *http.Response, v any) {
	defer resp.Body.Close()
	_ = json.NewDecoder(resp.Body).Decode(v)
}

// --- List models ---

func TestServer_ListModels(t *testing.T) {
	env := newTestEnv(t)

	// Authenticated member gets 200 with provider and models list.
	resp := env.do("GET", "/v1/models", env.memberToken, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body map[string]any
	decodeBody(resp, &body)
	if _, ok := body["provider"]; !ok {
		t.Error("response must contain 'provider' field")
	}
	models, ok := body["models"].([]any)
	if !ok || len(models) == 0 {
		t.Fatalf("expected non-empty 'models' array, got %v", body["models"])
	}
	first, ok := models[0].(map[string]any)
	if !ok {
		t.Fatalf("first model entry is not an object: %v", models[0])
	}
	if first["id"] == nil || first["id"] == "" {
		t.Error("first model must have 'id'")
	}
	if first["name"] == nil || first["name"] == "" {
		t.Error("first model must have 'name'")
	}

	// No token → 401.
	resp2 := env.do("GET", "/v1/models", "", nil)
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Errorf("no-token status = %d, want 401", resp2.StatusCode)
	}
}

// --- Answer task ---

func TestServer_AnswerTask(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	// Submit a task and wait for it to complete so the runner removes it from
	// the waiting map — only then will SendAnswer return "not waiting" (409).
	submit := env.do("POST", "/v1/tasks", env.memberToken, map[string]any{
		"input":  map[string]string{"type": "prompt", "text": "create S3 bucket"},
		"output": map[string]string{"type": "print"},
	})
	if submit.StatusCode != http.StatusCreated {
		t.Fatalf("submit status = %d, want 201", submit.StatusCode)
	}
	var submitBody map[string]string
	decodeBody(submit, &submitBody)
	taskID := submitBody["task_id"]

	// Wait until the task reaches a terminal state.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		r := env.do("GET", "/v1/tasks/"+taskID, env.memberToken, nil)
		var t2 map[string]any
		_ = json.NewDecoder(r.Body).Decode(&t2)
		r.Body.Close()
		if s, _ := t2["status"].(string); s == "done" || s == "failed" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Task is not waiting for input (already done) → 409.
	resp := env.do("POST", "/v1/tasks/"+taskID+"/answer", env.memberToken, map[string]string{"answer": "yes"})
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("answer not-waiting status = %d, want 409", resp.StatusCode)
	}

	// Empty answer → 400.
	resp2 := env.do("POST", "/v1/tasks/"+taskID+"/answer", env.memberToken, map[string]string{"answer": ""})
	if resp2.StatusCode != http.StatusBadRequest {
		t.Errorf("empty-answer status = %d, want 400", resp2.StatusCode)
	}

	// Nonexistent task → 404.
	resp3 := env.do("POST", "/v1/tasks/does-not-exist/answer", env.memberToken, map[string]string{"answer": "yes"})
	if resp3.StatusCode != http.StatusNotFound {
		t.Errorf("nonexistent-task status = %d, want 404", resp3.StatusCode)
	}

	// Another user's task: create bob as a second member.
	bobToken := "bob-answer-token"
	_, _ = env.store.CreateUser(ctx, "bob-answer", server.HashToken(bobToken), "member")

	// Bob tries to answer Alice's task → 403.
	resp4 := env.do("POST", "/v1/tasks/"+taskID+"/answer", bobToken, map[string]string{"answer": "yes"})
	if resp4.StatusCode != http.StatusForbidden {
		t.Errorf("other-user-answer status = %d, want 403", resp4.StatusCode)
	}
}

// --- Admin: delete user ---

func TestServer_AdminDeleteUser(t *testing.T) {
	env := newTestEnv(t)

	// Create a user to delete.
	createResp := env.do("POST", "/v1/admin/users", env.adminToken, map[string]string{
		"username": "delete-me",
		"token":    "delete-me-token",
		"role":     "member",
	})
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("create user status = %d, want 201", createResp.StatusCode)
	}
	var created map[string]string
	decodeBody(createResp, &created)
	userID := created["id"]

	// Delete the user.
	delResp := env.do("DELETE", "/v1/admin/users/"+userID, env.adminToken, nil)
	if delResp.StatusCode != http.StatusNoContent && delResp.StatusCode != http.StatusOK {
		t.Errorf("delete status = %d, want 204 or 200", delResp.StatusCode)
	}

	// Verify user no longer appears in list.
	listResp := env.do("GET", "/v1/admin/users", env.adminToken, nil)
	var users []map[string]any
	decodeBody(listResp, &users)
	for _, u := range users {
		if u["id"] == userID {
			t.Errorf("deleted user %q still appears in user list", userID)
		}
	}

	// Non-admin → 403.
	resp403 := env.do("DELETE", "/v1/admin/users/"+userID, env.memberToken, nil)
	if resp403.StatusCode != http.StatusForbidden {
		t.Errorf("non-admin delete status = %d, want 403", resp403.StatusCode)
	}
}

// --- Admin: set user active / inactive ---

func TestServer_AdminSetUserActive(t *testing.T) {
	env := newTestEnv(t)

	// Create a user to toggle.
	createResp := env.do("POST", "/v1/admin/users", env.adminToken, map[string]string{
		"username": "toggle-user",
		"token":    "toggle-user-token",
		"role":     "member",
	})
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("create user status = %d, want 201", createResp.StatusCode)
	}
	var created map[string]string
	decodeBody(createResp, &created)
	userID := created["id"]

	// Deactivate.
	deactResp := env.do("POST", "/v1/admin/users/"+userID+"/deactivate", env.adminToken, nil)
	if deactResp.StatusCode != http.StatusOK {
		t.Errorf("deactivate status = %d, want 200", deactResp.StatusCode)
	}

	// Activate.
	actResp := env.do("POST", "/v1/admin/users/"+userID+"/activate", env.adminToken, nil)
	if actResp.StatusCode != http.StatusOK {
		t.Errorf("activate status = %d, want 200", actResp.StatusCode)
	}

	// Non-admin → 403.
	resp403 := env.do("POST", "/v1/admin/users/"+userID+"/deactivate", env.memberToken, nil)
	if resp403.StatusCode != http.StatusForbidden {
		t.Errorf("non-admin deactivate status = %d, want 403", resp403.StatusCode)
	}
}

// --- Admin: revoke token ---

func TestServer_AdminRevokeToken(t *testing.T) {
	env := newTestEnv(t)

	// Create a user whose token we will revoke.
	revokeRawToken := "revoke-me-token"
	createResp := env.do("POST", "/v1/admin/users", env.adminToken, map[string]string{
		"username": "revoke-user",
		"token":    revokeRawToken,
		"role":     "member",
	})
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("create user status = %d, want 201", createResp.StatusCode)
	}
	var created map[string]string
	decodeBody(createResp, &created)
	userID := created["id"]

	// Verify the token works before revoke.
	preResp := env.do("GET", "/v1/me", revokeRawToken, nil)
	if preResp.StatusCode != http.StatusOK {
		t.Fatalf("pre-revoke GET /v1/me status = %d, want 200", preResp.StatusCode)
	}

	// Revoke.
	revokeResp := env.do("DELETE", "/v1/admin/users/"+userID+"/token", env.adminToken, nil)
	if revokeResp.StatusCode != http.StatusNoContent && revokeResp.StatusCode != http.StatusOK {
		t.Errorf("revoke status = %d, want 204 or 200", revokeResp.StatusCode)
	}

	// Old token no longer works.
	postResp := env.do("GET", "/v1/me", revokeRawToken, nil)
	if postResp.StatusCode != http.StatusUnauthorized {
		t.Errorf("post-revoke GET /v1/me status = %d, want 401", postResp.StatusCode)
	}

	// Non-admin → 403.
	resp403 := env.do("DELETE", "/v1/admin/users/"+userID+"/token", env.memberToken, nil)
	if resp403.StatusCode != http.StatusForbidden {
		t.Errorf("non-admin revoke status = %d, want 403", resp403.StatusCode)
	}
}

// --- Admin: regenerate token ---

func TestServer_RegenerateToken(t *testing.T) {
	env := newTestEnv(t)

	// Create a member user with a known raw token.
	oldRawToken := "regen-old-token"
	createResp := env.do("POST", "/v1/admin/users", env.adminToken, map[string]string{
		"username": "regen-user",
		"token":    oldRawToken,
		"role":     "member",
	})
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("create user status = %d, want 201", createResp.StatusCode)
	}
	var created map[string]string
	decodeBody(createResp, &created)
	userID := created["id"]

	// Regenerate token.
	regenResp := env.do("POST", "/v1/admin/users/"+userID+"/token", env.adminToken, nil)
	if regenResp.StatusCode != http.StatusOK {
		t.Fatalf("regen status = %d, want 200", regenResp.StatusCode)
	}
	var regenBody map[string]string
	decodeBody(regenResp, &regenBody)
	newToken := regenBody["token"]
	if newToken == "" {
		t.Fatal("expected new token in response, got empty string")
	}
	if newToken == oldRawToken {
		t.Error("new token must differ from the old token")
	}

	// Old token no longer works.
	oldResp := env.do("GET", "/v1/me", oldRawToken, nil)
	if oldResp.StatusCode != http.StatusUnauthorized {
		t.Errorf("old token GET /v1/me status = %d, want 401", oldResp.StatusCode)
	}

	// New token works.
	newResp := env.do("GET", "/v1/me", newToken, nil)
	if newResp.StatusCode != http.StatusOK {
		t.Errorf("new token GET /v1/me status = %d, want 200", newResp.StatusCode)
	}
}

// suppress unused import
var _ = fmt.Sprintf

func TestServer_AdminActions_AreAudited(t *testing.T) {
	env := newTestEnv(t)

	resp := env.do("POST", "/v1/admin/users", env.adminToken, map[string]string{
		"username": "carol",
		"token":    "carol-secret-token",
		"role":     "member",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create user status = %d, want 201", resp.StatusCode)
	}
	var created map[string]string
	_ = json.NewDecoder(resp.Body).Decode(&created)

	resp = env.do("POST", fmt.Sprintf("/v1/admin/users/%s/deactivate", created["id"]), env.adminToken, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("deactivate status = %d, want 200", resp.StatusCode)
	}

	events, err := env.store.ListAuditEvents(context.Background(), 10)
	if err != nil {
		t.Fatalf("ListAuditEvents: %v", err)
	}
	if len(events) < 2 {
		t.Fatalf("expected at least 2 audit events, got %d", len(events))
	}

	var sawCreate, sawDeactivate bool
	for _, e := range events {
		if e.Action == "user.create" && e.TargetID == created["id"] {
			sawCreate = true
		}
		if e.Action == "user.deactivate" && e.TargetID == created["id"] {
			sawDeactivate = true
		}
	}
	if !sawCreate {
		t.Error("expected a user.create audit event for the new user")
	}
	if !sawDeactivate {
		t.Error("expected a user.deactivate audit event for the new user")
	}
}

func TestServer_AuditLog_ForbiddenForMember(t *testing.T) {
	env := newTestEnv(t)
	resp := env.do("GET", "/v1/admin/audit-log", env.memberToken, nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
}
