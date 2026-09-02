package db

import (
	"context"
	"encoding/json"
	"time"
)

// User represents a registered server user.
type User struct {
	ID        string
	Username  string
	TokenHash string
	Role      string // admin | member
	Active    bool
	CreatedAt time.Time
}

// Pause kinds recorded in Task.PendingKind. A task with status
// "waiting_for_input" is paused on one of two very different prompts, and a
// client reconnecting mid-pause has to know which: a free-text question needs
// a text box, a tool-permission prompt needs approve/deny buttons.
const (
	PendingKindQuestion   = "question"   // ask_user: free-text answer expected
	PendingKindPermission = "permission" // tool-permission prompt: allow/deny expected
)

// Task represents a submitted agent task.
type Task struct {
	ID              string     `json:"id"`
	UserID          string     `json:"user_id"`
	Status          string     `json:"status"`
	InputType       string     `json:"input_type"`
	InputText       string     `json:"input_text"`
	OutputType      string     `json:"output_type"`
	PRUrl           string     `json:"pr_url,omitempty"`
	InputTokens     int        `json:"input_tokens"`
	OutputTokens    int        `json:"output_tokens"`
	ErrorMsg        string     `json:"error_msg,omitempty"`
	Output          string     `json:"output,omitempty"`
	PendingQuestion string     `json:"pending_question,omitempty"`
	PendingKind     string     `json:"pending_kind,omitempty"` // "" | question | permission
	CreatedAt       time.Time  `json:"created_at"`
	StartedAt       *time.Time `json:"started_at,omitempty"`
	CompletedAt     *time.Time `json:"completed_at,omitempty"`
}

// UserSettings holds per-user configurable tokens. Sensitive fields are stored
// encrypted at rest; the server layer encrypts/decrypts before calling the store.
type UserSettings struct {
	UserID          string
	GitHubToken     string // encrypted at rest
	AtlassianToken  string // encrypted at rest
	AtlassianDomain string
	AtlassianEmail  string
	UpdatedAt       time.Time
}

// AuditEvent records a single admin action for accountability. Written on every
// user-management mutation (create, update, delete, activate/deactivate,
// token regenerate/revoke) so "who did this and when" is answerable after the fact.
type AuditEvent struct {
	ID            string    `json:"id"`
	ActorID       string    `json:"actor_id"`
	ActorUsername string    `json:"actor_username,omitempty"`
	Action        string    `json:"action"`
	TargetID      string    `json:"target_id,omitempty"`
	Detail        string    `json:"detail,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
}

// RepoIndexEntry is a cached structural summary of a repository at a specific
// commit, so a task doesn't need to re-scan/re-parse the same repo content
// every time it operates on the same commit.
type RepoIndexEntry struct {
	RepoID    string          `json:"repo_id"`
	CommitSHA string          `json:"commit_sha"`
	Summary   json.RawMessage `json:"summary"`
	IndexedAt time.Time       `json:"indexed_at"`
}

// RunEvent is a single event in the ordered event log for a task.
type RunEvent struct {
	Seq       int64
	Type      string
	Payload   json.RawMessage
	CreatedAt time.Time
}

// Store is the persistence interface. Swap implementations without touching callers.
type Store interface {
	// Users
	CreateUser(ctx context.Context, username, tokenHash, role string) (*User, error)
	GetUserByTokenHash(ctx context.Context, hash string) (*User, error)
	ListUsers(ctx context.Context) ([]*User, error)
	EnsureAdmin(ctx context.Context, username, tokenHash string) error
	UpdateUsername(ctx context.Context, userID, username string) error
	UpdateUser(ctx context.Context, userID, username, role string) error
	SetUserActive(ctx context.Context, userID string, active bool) error
	RevokeUserToken(ctx context.Context, userID string) error
	UpdateUserToken(ctx context.Context, userID, tokenHash string) error
	DeleteUser(ctx context.Context, userID string) error
	GetUserByID(ctx context.Context, userID string) (*User, error)

	// Tasks
	CreateTask(ctx context.Context, userID, inputType, inputText, outputType string) (*Task, error)
	UpdateTaskStatus(ctx context.Context, id, status string) error
	// UpdateTaskPendingQuestion records (or clears, with question == "") the
	// prompt a task is paused on. kind is one of the PendingKind* constants
	// when pausing, and "" when clearing.
	UpdateTaskPendingQuestion(ctx context.Context, id, question, kind string) error
	UpdateTaskResult(ctx context.Context, id, status, prURL, errorMsg, output string, inputTokens, outputTokens int) error
	GetTask(ctx context.Context, id string) (*Task, error)
	ListUserTasks(ctx context.Context, userID string, limit int) ([]*Task, error)
	MarkStaleTasksFailed(ctx context.Context) error
	// FailTasksOlderThan marks any row with status running/queued/
	// waiting_for_input AND created_at older than maxAge as failed with
	// errMsg. Unlike MarkStaleTasksFailed (which sweeps every non-terminal
	// row unconditionally and is unsafe under queue_driver=nats — see its
	// call site in cmd/server/main.go), this only ever touches rows that have
	// been non-terminal for an unusually long time, so it's safe to run
	// unconditionally under any queue driver: it never fails another pod's
	// task that is simply, currently, legitimately still running. It exists
	// to reconcile the narrow case a task's queue delivery is abandoned and
	// redelivered until it exhausts the queue's max-delivery-attempts and has
	// no other automatic path back to a terminal status. Returns the number
	// of rows reconciled, for logging.
	FailTasksOlderThan(ctx context.Context, maxAge time.Duration, errMsg string) (int, error)

	// User settings
	GetUserSettings(ctx context.Context, userID string) (*UserSettings, error)
	UpsertUserSettings(ctx context.Context, s *UserSettings) error

	// Audit
	RecordAuditEvent(ctx context.Context, actorID, actorUsername, action, targetID, detail string) error
	ListAuditEvents(ctx context.Context, limit int) ([]*AuditEvent, error)

	// Repo index — cached structural summaries of a repo at a given commit.
	// GetRepoIndex returns (nil, nil) on a cache miss — a missing entry is
	// not an error condition, it just means the caller should parse fresh
	// and call SaveRepoIndex.
	GetRepoIndex(ctx context.Context, repoID, commitSHA string) (*RepoIndexEntry, error)
	SaveRepoIndex(ctx context.Context, repoID, commitSHA string, summary json.RawMessage) error

	// Run events — ordered, persistent event log per task for SSE replay.
	AppendRunEvent(ctx context.Context, taskID, eventType string, payload json.RawMessage) (seq int64, err error)
	GetRunEventsSince(ctx context.Context, taskID string, sinceSeq int64) ([]RunEvent, error)

	// Ping verifies the database connection is alive.
	Ping(ctx context.Context) error

	Close() error
}
