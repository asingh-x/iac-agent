package server

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/tf-agent/tf-agent/internal/config"
	"github.com/tf-agent/tf-agent/internal/db"
	"github.com/tf-agent/tf-agent/internal/llm"
	"github.com/tf-agent/tf-agent/internal/queue"
)

// Server wires all HTTP routes.
type Server struct {
	store  db.Store
	hub    *Hub
	queues map[string]queue.Queue // name → queue; "default" always present
	runner *Runner
	cfg    *config.Config
	fs     embed.FS
}

// NewServer creates a Server. queues maps queue names to their Queue implementations;
// at minimum a "default" entry must be present.
func NewServer(store db.Store, hub *Hub, queues map[string]queue.Queue, runner *Runner, cfg *config.Config, webFS embed.FS) *Server {
	return &Server{store: store, hub: hub, queues: queues, runner: runner, cfg: cfg, fs: webFS}
}

// getQueue returns the named queue, falling back to "default".
func (s *Server) getQueue(name string) queue.Queue {
	if name != "" {
		if q, ok := s.queues[name]; ok {
			return q
		}
	}
	return s.queues["default"]
}

// totalQueueLen returns the sum of pending items across all queues.
func (s *Server) totalQueueLen() int {
	n := 0
	for _, q := range s.queues {
		n += q.Len()
	}
	return n
}

// queueDepths returns the pending item count for every named queue, keyed by
// queue name (e.g. "default"). It also updates the tfagent_queue_depth gauge
// for each queue so /metrics stays in sync with the depths reported here.
func (s *Server) queueDepths() map[string]int {
	depths := make(map[string]int, len(s.queues))
	for name, q := range s.queues {
		n := q.Len()
		depths[name] = n
		metricQueueDepth.WithLabelValues(name).Set(float64(n))
	}
	return depths
}

// Handler returns the root http.Handler with all routes registered.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Health
	mux.HandleFunc("GET /healthz", s.handleHealth)

	// Metrics (unauthenticated — standard Prometheus scrape endpoint)
	mux.Handle("GET /metrics", promhttp.Handler())

	// Static files — serve embedded React app; fall back to index.html for SPA routing.
	// sub may fail when running tests without a built UI (embed.FS is empty); in that
	// case static routes return 404 which is fine for API-only test scenarios.
	if sub, err := fs.Sub(s.fs, "web"); err == nil {
		fileServer := http.FileServer(http.FS(sub))
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			path := strings.TrimPrefix(r.URL.Path, "/")
			if path == "" {
				path = "index.html"
			}
			if f, err := sub.Open(path); err == nil {
				f.Close()
				fileServer.ServeHTTP(w, r)
				return
			}
			// SPA fallback: serve index.html for any unmatched path
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			data, _ := fs.ReadFile(s.fs, "web/index.html")
			w.Write(data)
		})
	}

	// Authenticated routes
	authed := http.NewServeMux()
	authed.HandleFunc("GET /v1/models", s.handleListModels)
	authed.HandleFunc("GET /v1/me", s.handleMe)
	authed.HandleFunc("PATCH /v1/me", s.handleUpdateMe)
	authed.HandleFunc("GET /v1/settings", s.handleGetSettings)
	authed.HandleFunc("PUT /v1/settings", s.handlePutSettings)
	authed.HandleFunc("POST /v1/tasks", s.handleSubmitTask)
	authed.HandleFunc("GET /v1/tasks", s.handleListTasks)
	authed.HandleFunc("GET /v1/tasks/{id}", s.handleGetTask)
	authed.HandleFunc("GET /v1/tasks/{id}/output", s.handleGetTaskOutput)
	authed.HandleFunc("GET /v1/tasks/{id}/stream", s.handleStreamTask)
	authed.HandleFunc("POST /v1/tasks/{id}/answer", s.handleAnswerTask)
	authed.HandleFunc("POST /v1/tasks/{id}/permission", s.handlePermissionResponse)
	authed.HandleFunc("POST /v1/tasks/{id}/cancel", s.handleCancelTask)

	// Admin routes — registered directly on root mux so {id} path values work correctly
	authAdmin := func(h http.HandlerFunc) http.Handler {
		return authMiddleware(s.store, adminMiddleware(http.HandlerFunc(h)))
	}
	mux.Handle("POST /v1/admin/users", authAdmin(s.handleCreateUser))
	mux.Handle("GET /v1/admin/users", authAdmin(s.handleListUsers))
	mux.Handle("PATCH /v1/admin/users/{id}", authAdmin(s.handleUpdateUser))
	mux.Handle("DELETE /v1/admin/users/{id}", authAdmin(s.handleDeleteUser))
	mux.Handle("POST /v1/admin/users/{id}/activate", authAdmin(s.handleSetUserActive))
	mux.Handle("POST /v1/admin/users/{id}/deactivate", authAdmin(s.handleSetUserActive))
	mux.Handle("POST /v1/admin/users/{id}/token", authAdmin(s.handleRegenerateToken))
	mux.Handle("DELETE /v1/admin/users/{id}/token", authAdmin(s.handleRevokeToken))
	mux.Handle("GET /v1/admin/audit-log", authAdmin(s.handleListAuditLog))

	mux.Handle("/v1/", authMiddleware(s.store, authed))

	return requestIDMiddleware(mux)
}

// --- Health ---

type checkResult struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	checks := map[string]checkResult{}
	healthy := true

	// API key configured
	apiKeyOK := s.cfg.Provider.Anthropic.APIKey != "" || s.cfg.Provider.Name == "bedrock"
	if !apiKeyOK {
		checks["api_key"] = checkResult{OK: false, Error: "ANTHROPIC_API_KEY not set"}
		healthy = false
	} else {
		checks["api_key"] = checkResult{OK: true}
	}

	// Database reachable
	if err := s.store.Ping(ctx); err != nil {
		checks["database"] = checkResult{OK: false, Error: err.Error()}
		healthy = false
	} else {
		checks["database"] = checkResult{OK: true}
	}

	// Encryption key loaded
	if !EncryptionKeyLoaded() {
		checks["encryption_key"] = checkResult{OK: false, Error: "encryption key not loaded"}
		healthy = false
	} else {
		checks["encryption_key"] = checkResult{OK: true}
	}

	status := "ok"
	code := http.StatusOK
	if !healthy {
		status = "degraded"
		code = http.StatusServiceUnavailable
	}

	depths := s.queueDepths()

	// LLM concurrency saturation and recent task error rate: observability
	// only — neither factors into healthy/code above. A saturated queue or
	// a nonzero error rate can just as easily mean "the upstream LLM API is
	// having a bad day," which is a different signal than "this process
	// itself is broken" (api_key/database/encryption_key, checked above),
	// and flipping /healthz unhealthy for it would make an autoscaler or
	// orchestrator kill/recycle pods that are, structurally, fine.
	llmUsed, llmCapacity := 0, 0
	var errRate float64
	var errFailed, errTotal int
	if s.runner != nil {
		llmUsed, llmCapacity = s.runner.ConcurrencyUtilization()
		errRate, errFailed, errTotal = s.runner.TaskErrorRate()
	}
	metricLLMConcurrencyUsed.Set(float64(llmUsed))
	metricLLMConcurrencyCapacity.Set(float64(llmCapacity))

	writeJSON(w, code, map[string]any{
		"status":      status,
		"checks":      checks,
		"queue_len":   s.totalQueueLen(),
		"queue_depth": depths,
		"llm_concurrency": map[string]any{
			"used":     llmUsed,
			"capacity": llmCapacity,
		},
		"task_error_rate": map[string]any{
			"rate":   errRate,
			"failed": errFailed,
			"total":  errTotal,
		},
	})
}

// --- Models ---

func (s *Server) handleListModels(w http.ResponseWriter, r *http.Request) {
	activeModel := llm.ModelName(s.cfg)
	models := llm.ModelsForProvider(s.cfg.Provider.Name, activeModel)
	writeJSON(w, http.StatusOK, map[string]any{
		"provider": s.cfg.Provider.Name,
		"models":   models,
	})
}

// --- Tasks ---

type submitTaskRequest struct {
	Input     taskInput  `json:"input"`
	Output    taskOutput `json:"output"`
	QueueName string     `json:"queue_name"` // optional; defaults to "default"
}

type taskInput struct {
	Type            string `json:"type"`             // prompt | jira
	Text            string `json:"text"`             // for type=prompt
	Ticket          string `json:"ticket"`           // for type=jira
	AtlassianToken  string `json:"atlassian_token"`  // jira auth
	AtlassianDomain string `json:"atlassian_domain"` // e.g. mycompany.atlassian.net
	AtlassianEmail  string `json:"atlassian_email"`
}

type taskOutput struct {
	Type        string `json:"type"`         // pr | files | print
	GitHubToken string `json:"github_token"` // for type=pr
	RepoURL     string `json:"repo_url"`     // for type=pr
	OutputDir   string `json:"output_dir"`   // for type=files
}

func (s *Server) handleSubmitTask(w http.ResponseWriter, r *http.Request) {
	user := userFromContext(r.Context())

	var req submitTaskRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	// Validate input
	inputType := req.Input.Type
	if inputType != "prompt" && inputType != "jira" {
		writeError(w, http.StatusBadRequest, "input.type must be prompt or jira")
		return
	}
	inputText := req.Input.Text
	if inputType == "jira" {
		inputText = req.Input.Ticket
	}
	if strings.TrimSpace(inputText) == "" {
		writeError(w, http.StatusBadRequest, "input text or jira ticket is required")
		return
	}

	// Validate output
	outputType := req.Output.Type
	if outputType == "" {
		outputType = "print"
	}
	if outputType != "pr" && outputType != "files" && outputType != "print" {
		writeError(w, http.StatusBadRequest, "output.type must be pr, files, or print")
		return
	}
	if outputType == "pr" && req.Output.RepoURL == "" {
		writeError(w, http.StatusBadRequest, "output.repo_url is required for type=pr")
		return
	}

	// Pull tokens from user settings when not supplied in the request.
	if us, err := s.store.GetUserSettings(r.Context(), user.ID); err == nil {
		if outputType == "pr" && req.Output.GitHubToken == "" && us.GitHubToken != "" {
			if dec, decErr := Decrypt(us.GitHubToken); decErr == nil {
				req.Output.GitHubToken = dec
			} else {
				log.Printf("failed to decrypt stored github_token for user %s: %v", user.ID, decErr)
			}
		}
		if req.Input.AtlassianToken == "" && us.AtlassianToken != "" {
			if dec, decErr := Decrypt(us.AtlassianToken); decErr == nil {
				req.Input.AtlassianToken = dec
			} else {
				log.Printf("failed to decrypt stored atlassian_token for user %s: %v", user.ID, decErr)
			}
		}
		if req.Input.AtlassianDomain == "" {
			req.Input.AtlassianDomain = us.AtlassianDomain
		}
		if req.Input.AtlassianEmail == "" {
			req.Input.AtlassianEmail = us.AtlassianEmail
		}
	}

	if outputType == "pr" && req.Output.GitHubToken == "" {
		writeError(w, http.StatusBadRequest, "no GitHub token configured — add one in Settings")
		return
	}

	// Fail fast if the global LLM concurrency semaphore is already fully
	// saturated, instead of accepting the task and letting it sit queued
	// only to fail later once run() gives up waiting for a slot (see
	// Runner.run's bounded semaphore wait). This is a best-effort admission
	// check, not a reservation — capacity can change between this check and
	// the task actually being dequeued — but it sheds load at the cheapest
	// possible point for a client that can retry, the same way a full queue
	// already does below.
	if s.runner != nil {
		if used, capacity := s.runner.ConcurrencyUtilization(); capacity > 0 && used >= capacity {
			writeError(w, http.StatusServiceUnavailable, "server at capacity, try again later")
			return
		}
	}

	// Persist task
	task, err := s.store.CreateTask(r.Context(), user.ID, inputType, inputText, outputType)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create task")
		return
	}

	// Create event channel before pushing to queue (avoid race with SSE subscriber)
	s.hub.Create(task.ID)

	// Enqueue — route to the requested named queue (fallback: "default").
	err = s.getQueue(req.QueueName).Push(r.Context(), queue.Item{
		TaskID:          task.ID,
		UserID:          user.ID,
		QueueName:       req.QueueName,
		InputType:       inputType,
		InputText:       inputText,
		OutputType:      outputType,
		OutputDir:       req.Output.OutputDir,
		GitHubToken:     req.Output.GitHubToken,
		RepoURL:         req.Output.RepoURL,
		AtlassianToken:  req.Input.AtlassianToken,
		AtlassianDomain: req.Input.AtlassianDomain,
		AtlassianEmail:  req.Input.AtlassianEmail,
	})
	if err != nil {
		// Compensate: mark the task as failed so it doesn't linger in queued state.
		s.hub.Close(task.ID)
		_ = s.store.UpdateTaskResult(r.Context(), task.ID, "failed", "", "queue full at submission", "", 0, 0)
		writeError(w, http.StatusServiceUnavailable, "queue full, try again later")
		return
	}

	metricTasksSubmitted.Inc()

	writeJSON(w, http.StatusCreated, map[string]string{
		"task_id":    task.ID,
		"status":     "queued",
		"stream_url": "/v1/tasks/" + task.ID + "/stream",
	})
}

// taskWithCost wraps a Task with a computed cost_usd field.
type taskWithCost struct {
	*db.Task
	CostUSD float64 `json:"cost_usd"`
}

func (s *Server) withCost(t *db.Task) taskWithCost {
	model := llm.ModelName(s.cfg)
	return taskWithCost{Task: t, CostUSD: llm.CalculateCostUSD(model, t.InputTokens, t.OutputTokens)}
}

func (s *Server) handleGetTask(w http.ResponseWriter, r *http.Request) {
	user := userFromContext(r.Context())
	taskID := r.PathValue("id")

	task, err := s.store.GetTask(r.Context(), taskID)
	if err != nil || task == nil {
		writeError(w, http.StatusNotFound, "task not found")
		return
	}
	if task.UserID != user.ID && user.Role != "admin" {
		writeError(w, http.StatusForbidden, "forbidden")
		return
	}
	writeJSON(w, http.StatusOK, s.withCost(task))
}

// defaultTaskOutputLimit bounds how many bytes of a task's output
// handleGetTaskOutput returns when the caller doesn't specify limit — large
// enough to cover the overwhelming majority of task outputs in one call,
// small enough that a client can't accidentally pull an unbounded blob (the
// exact problem this endpoint exists to let a client avoid on the plain
// GET /v1/tasks/{id} response).
const defaultTaskOutputLimit = 100_000

// handleGetTaskOutput returns a byte-offset slice of a task's stored output,
// so a client can page through a large output instead of always receiving
// the whole thing embedded in GET /v1/tasks/{id}. Same owner-or-admin
// ownership check as the sibling task endpoints.
//
// offset/limit slice task.Output by byte offset, not rune/codepoint offset —
// a slice boundary landing mid-UTF-8-rune is possible or the caller passes a
// value from earlier that no longer means the same page (output only ever
// grows for a still-running task) — the plain-string byte slicing this
// implements is exactly what's asked for here, but a client display layer
// should tolerate a stray partial rune at either edge.
func (s *Server) handleGetTaskOutput(w http.ResponseWriter, r *http.Request) {
	user := userFromContext(r.Context())
	taskID := r.PathValue("id")

	task, err := s.store.GetTask(r.Context(), taskID)
	if err != nil || task == nil {
		writeError(w, http.StatusNotFound, "task not found")
		return
	}
	if task.UserID != user.ID && user.Role != "admin" {
		writeError(w, http.StatusForbidden, "forbidden")
		return
	}

	offset := 0
	if v := r.URL.Query().Get("offset"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "offset must be a non-negative integer")
			return
		}
		offset = n
	}

	limit := defaultTaskOutputLimit
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			writeError(w, http.StatusBadRequest, "limit must be a positive integer")
			return
		}
		limit = n
	}

	total := len(task.Output)
	start := offset
	if start > total {
		start = total
	}
	// Computed as "end = total unless limit is small enough that start+limit
	// can't overflow" rather than the more obvious `end := start + limit; if
	// end > total { end = total }` — with limit near math.MaxInt64 (an
	// attacker-controlled query param), that addition overflows to a large
	// negative number, which is never ">total" and slips past the clamp,
	// causing a slice-bounds panic on task.Output[start:end] below.
	end := total
	if limit < total-start {
		end = start + limit
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"task_id":      taskID,
		"offset":       offset,
		"limit":        limit,
		"total_length": total,
		"output":       task.Output[start:end],
	})
}

func (s *Server) handleListTasks(w http.ResponseWriter, r *http.Request) {
	user := userFromContext(r.Context())
	tasks, err := s.store.ListUserTasks(r.Context(), user.ID, 50)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list tasks")
		return
	}
	out := make([]taskWithCost, len(tasks))
	for i, t := range tasks {
		out[i] = s.withCost(t)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleAnswerTask(w http.ResponseWriter, r *http.Request) {
	user := userFromContext(r.Context())
	taskID := r.PathValue("id")

	task, err := s.store.GetTask(r.Context(), taskID)
	if err != nil || task == nil {
		writeError(w, http.StatusNotFound, "task not found")
		return
	}
	if task.UserID != user.ID && user.Role != "admin" {
		writeError(w, http.StatusForbidden, "forbidden")
		return
	}

	var req struct {
		Answer string `json:"answer"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Answer) == "" {
		writeError(w, http.StatusBadRequest, "answer is required")
		return
	}

	if err := s.runner.SendAnswer(taskID, req.Answer); err != nil {
		writeError(w, http.StatusConflict, "task is not waiting for input")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handlePermissionResponse(w http.ResponseWriter, r *http.Request) {
	user := userFromContext(r.Context())
	taskID := r.PathValue("id")

	task, err := s.store.GetTask(r.Context(), taskID)
	if err != nil || task == nil {
		writeError(w, http.StatusNotFound, "task not found")
		return
	}
	if task.UserID != user.ID && user.Role != "admin" {
		writeError(w, http.StatusForbidden, "forbidden")
		return
	}

	var req struct {
		Allow bool `json:"allow"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "allow (boolean) is required")
		return
	}

	if err := s.runner.SendPermissionResponse(taskID, req.Allow); err != nil {
		writeError(w, http.StatusConflict, "task is not waiting for a permission decision")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleCancelTask(w http.ResponseWriter, r *http.Request) {
	user := userFromContext(r.Context())
	taskID := r.PathValue("id")

	task, err := s.store.GetTask(r.Context(), taskID)
	if err != nil || task == nil {
		writeError(w, http.StatusNotFound, "task not found")
		return
	}
	if task.UserID != user.ID && user.Role != "admin" {
		writeError(w, http.StatusForbidden, "forbidden")
		return
	}

	if err := s.runner.CancelTask(taskID); err != nil {
		writeError(w, http.StatusConflict, "task is not running")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "cancelled"})
}

// InitialSnapshotEvent turns a task's current Postgres row into the SSE
// event ServeSSE should send before anything else. This is what makes a pod
// that isn't executing the task still "have the info" the instant a client
// connects to it — regardless of whether the cross-pod event relay is even
// configured — by reflecting whatever state is already durably persisted.
//
// Exported because it is also the body of Hub's relay-path status backstop,
// wired from package main (see Hub.SetStatusCheck).
//
// Known, accepted race: the row is read at connect time and can already be
// stale by the time the event reaches the client (e.g. the user's answer
// landed a moment earlier, or the task just finished). The consequence is a
// UX rough edge, not corruption — a client acting on a stale "waiting for
// input" snapshot gets a normal 409 from the answer/permission endpoint, and
// the live stream that follows immediately corrects the state.
func InitialSnapshotEvent(task *db.Task) *ServerEvent {
	switch task.Status {
	case "done":
		return &ServerEvent{Type: "done", PRUrl: task.PRUrl}
	case "failed", "cancelled":
		return &ServerEvent{Type: "error", Error: task.ErrorMsg}
	case "waiting_for_input":
		// The agent supports two kinds of pause, both persisted as status
		// waiting_for_input: a free-text ask_user question, and a
		// tool-permission prompt. PendingKind records which, so a client
		// reconnecting (or landing on a different pod) mid-prompt learns
		// whether to render a text box or approve/deny buttons.
		if task.PendingKind == db.PendingKindPermission {
			tool, preview := ParsePermissionPrompt(task.PendingQuestion)
			return &ServerEvent{Type: "permission_request", Tool: tool, Text: preview}
		}
		return &ServerEvent{Type: "waiting_for_input", Text: task.PendingQuestion}
	default: // "queued", "running"
		return &ServerEvent{Type: "status", Status: task.Status}
	}
}

func (s *Server) handleStreamTask(w http.ResponseWriter, r *http.Request) {
	user := userFromContext(r.Context())
	taskID := r.PathValue("id")

	task, err := s.store.GetTask(r.Context(), taskID)
	if err != nil || task == nil {
		writeError(w, http.StatusNotFound, "task not found")
		return
	}
	if task.UserID != user.ID && user.Role != "admin" {
		writeError(w, http.StatusForbidden, "forbidden")
		return
	}

	s.hub.ServeSSE(w, r, taskID, InitialSnapshotEvent(task))
}

// --- Admin ---

type createUserRequest struct {
	Username string `json:"username"`
	Token    string `json:"token"` // raw token, admin generates this
	Role     string `json:"role"`  // admin | member (default: member)
}

func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	var req createUserRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Username == "" || req.Token == "" {
		writeError(w, http.StatusBadRequest, "username and token are required")
		return
	}
	role := req.Role
	if role == "" {
		role = "member"
	}

	user, err := s.store.CreateUser(r.Context(), req.Username, HashToken(req.Token), role)
	if err != nil {
		writeError(w, http.StatusConflict, "username already exists")
		return
	}
	actor := userFromContext(r.Context())
	if err := s.store.RecordAuditEvent(r.Context(), actor.ID, actor.Username, "user.create", user.ID,
		fmt.Sprintf("username=%s role=%s", user.Username, user.Role)); err != nil {
		log.Printf("audit: failed to record user.create for %s: %v", user.ID, err)
	}
	writeJSON(w, http.StatusCreated, map[string]string{
		"id":       user.ID,
		"username": user.Username,
		"role":     user.Role,
	})
}

func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request) {
	users, err := s.store.ListUsers(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list users")
		return
	}
	// strip token hashes from response
	type safeUser struct {
		ID        string `json:"id"`
		Username  string `json:"username"`
		Role      string `json:"role"`
		Active    bool   `json:"active"`
		CreatedAt string `json:"created_at"`
	}
	out := make([]safeUser, len(users))
	for i, u := range users {
		out[i] = safeUser{u.ID, u.Username, u.Role, u.Active, u.CreatedAt.Format("2006-01-02T15:04:05Z")}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleUpdateUser(w http.ResponseWriter, r *http.Request) {
	userID := r.PathValue("id")
	var req struct {
		Username string `json:"username"`
		Role     string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	req.Username = strings.TrimSpace(req.Username)
	if req.Username == "" {
		writeError(w, http.StatusBadRequest, "username cannot be empty")
		return
	}
	if req.Role != "admin" && req.Role != "member" {
		writeError(w, http.StatusBadRequest, "role must be admin or member")
		return
	}
	if err := s.store.UpdateUser(r.Context(), userID, req.Username, req.Role); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update user")
		return
	}
	actor := userFromContext(r.Context())
	if err := s.store.RecordAuditEvent(r.Context(), actor.ID, actor.Username, "user.update", userID,
		fmt.Sprintf("username=%s role=%s", req.Username, req.Role)); err != nil {
		log.Printf("audit: failed to record user.update for %s: %v", userID, err)
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": userID, "username": req.Username, "role": req.Role})
}

func (s *Server) handleRegenerateToken(w http.ResponseWriter, r *http.Request) {
	userID := r.PathValue("id")
	rawToken, err := generateToken()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to generate token")
		return
	}
	if err := s.store.UpdateUserToken(r.Context(), userID, HashToken(rawToken)); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update token")
		return
	}
	actor := userFromContext(r.Context())
	if err := s.store.RecordAuditEvent(r.Context(), actor.ID, actor.Username, "user.token_regenerate", userID, ""); err != nil {
		log.Printf("audit: failed to record user.token_regenerate for %s: %v", userID, err)
	}
	writeJSON(w, http.StatusOK, map[string]string{"token": rawToken})
}

func (s *Server) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	userID := r.PathValue("id")
	self := userFromContext(r.Context())
	if self.ID == userID {
		writeError(w, http.StatusBadRequest, "cannot delete your own account")
		return
	}
	if err := s.store.DeleteUser(r.Context(), userID); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to delete user")
		return
	}
	if err := s.store.RecordAuditEvent(r.Context(), self.ID, self.Username, "user.delete", userID, ""); err != nil {
		log.Printf("audit: failed to record user.delete for %s: %v", userID, err)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleSetUserActive(w http.ResponseWriter, r *http.Request) {
	userID := r.PathValue("id")
	active := strings.HasSuffix(r.URL.Path, "/activate")
	self := userFromContext(r.Context())
	if self.ID == userID && !active {
		writeError(w, http.StatusBadRequest, "cannot deactivate your own account")
		return
	}
	if err := s.store.SetUserActive(r.Context(), userID, active); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update user status")
		return
	}
	action := "user.deactivate"
	if active {
		action = "user.activate"
	}
	if err := s.store.RecordAuditEvent(r.Context(), self.ID, self.Username, action, userID, ""); err != nil {
		log.Printf("audit: failed to record %s for %s: %v", action, userID, err)
	}
	writeJSON(w, http.StatusOK, map[string]any{"active": active})
}

func (s *Server) handleRevokeToken(w http.ResponseWriter, r *http.Request) {
	userID := r.PathValue("id")
	self := userFromContext(r.Context())
	if self.ID == userID {
		writeError(w, http.StatusBadRequest, "cannot revoke your own token")
		return
	}
	if err := s.store.RevokeUserToken(r.Context(), userID); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to revoke token")
		return
	}
	if err := s.store.RecordAuditEvent(r.Context(), self.ID, self.Username, "user.token_revoke", userID, ""); err != nil {
		log.Printf("audit: failed to record user.token_revoke for %s: %v", userID, err)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListAuditLog(w http.ResponseWriter, r *http.Request) {
	events, err := s.store.ListAuditEvents(r.Context(), 200)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list audit log")
		return
	}
	writeJSON(w, http.StatusOK, events)
}

// --- Me ---

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	user := userFromContext(r.Context())
	writeJSON(w, http.StatusOK, map[string]string{
		"id":       user.ID,
		"username": user.Username,
		"role":     user.Role,
	})
}

func (s *Server) handleUpdateMe(w http.ResponseWriter, r *http.Request) {
	user := userFromContext(r.Context())
	var req struct {
		Username string `json:"username"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	req.Username = strings.TrimSpace(req.Username)
	if req.Username == "" {
		writeError(w, http.StatusBadRequest, "username cannot be empty")
		return
	}
	if err := s.store.UpdateUsername(r.Context(), user.ID, req.Username); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update username")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"id":       user.ID,
		"username": req.Username,
		"role":     user.Role,
	})
}

// --- User settings ---

type settingsResponse struct {
	GitHubTokenSet    bool   `json:"github_token_set"`
	AtlassianTokenSet bool   `json:"atlassian_token_set"`
	AtlassianDomain   string `json:"atlassian_domain"`
	AtlassianEmail    string `json:"atlassian_email"`
}

func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	user := userFromContext(r.Context())
	us, err := s.store.GetUserSettings(r.Context(), user.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to get settings")
		return
	}
	// Decrypt to check whether tokens are set; never send plaintext tokens to client.
	ghSet := false
	if us.GitHubToken != "" {
		if dec, err := Decrypt(us.GitHubToken); err == nil && dec != "" {
			ghSet = true
		} else if err != nil {
			log.Printf("failed to decrypt stored github_token for user %s: %v", user.ID, err)
		}
	}
	atSet := false
	if us.AtlassianToken != "" {
		if dec, err := Decrypt(us.AtlassianToken); err == nil && dec != "" {
			atSet = true
		} else if err != nil {
			log.Printf("failed to decrypt stored atlassian_token for user %s: %v", user.ID, err)
		}
	}
	writeJSON(w, http.StatusOK, settingsResponse{
		GitHubTokenSet:    ghSet,
		AtlassianTokenSet: atSet,
		AtlassianDomain:   us.AtlassianDomain,
		AtlassianEmail:    us.AtlassianEmail,
	})
}

type updateSettingsRequest struct {
	GitHubToken     string `json:"github_token"`
	AtlassianToken  string `json:"atlassian_token"`
	AtlassianDomain string `json:"atlassian_domain"`
	AtlassianEmail  string `json:"atlassian_email"`
}

func (s *Server) handlePutSettings(w http.ResponseWriter, r *http.Request) {
	user := userFromContext(r.Context())
	var req updateSettingsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	// Load existing settings so we can keep tokens that weren't updated.
	existing, err := s.store.GetUserSettings(r.Context(), user.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load settings")
		return
	}

	updated := &db.UserSettings{
		UserID:          user.ID,
		GitHubToken:     existing.GitHubToken,
		AtlassianToken:  existing.AtlassianToken,
		AtlassianDomain: existing.AtlassianDomain,
		AtlassianEmail:  existing.AtlassianEmail,
	}
	// Only overwrite domain/email when explicitly provided in the request.
	if strings.TrimSpace(req.AtlassianDomain) != "" {
		updated.AtlassianDomain = strings.TrimSpace(req.AtlassianDomain)
	}
	if strings.TrimSpace(req.AtlassianEmail) != "" {
		updated.AtlassianEmail = strings.TrimSpace(req.AtlassianEmail)
	}

	if strings.TrimSpace(req.GitHubToken) != "" {
		enc, err := Encrypt(strings.TrimSpace(req.GitHubToken))
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to encrypt github token")
			return
		}
		updated.GitHubToken = enc
	}
	if strings.TrimSpace(req.AtlassianToken) != "" {
		enc, err := Encrypt(strings.TrimSpace(req.AtlassianToken))
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to encrypt atlassian token")
			return
		}
		updated.AtlassianToken = enc
	}

	if err := s.store.UpsertUserSettings(r.Context(), updated); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to save settings")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
