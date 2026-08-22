package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
)

// NewMemoryStore returns a Store backed by in-memory maps.
// Intended for unit tests only — data is not persisted.
func NewMemoryStore() Store {
	return &memStore{
		users:     map[string]*User{},
		tasks:     map[string]*Task{},
		settings:  map[string]*UserSettings{},
		repoIndex: map[string]*RepoIndexEntry{},
	}
}

type memStore struct {
	mu        sync.Mutex
	users     map[string]*User // id → user
	byHash    map[string]*User // tokenHash → user
	tasks     map[string]*Task
	settings  map[string]*UserSettings
	audit     []*AuditEvent
	repoIndex map[string]*RepoIndexEntry // "repoID\x00commitSHA" → entry
}

func repoIndexKey(repoID, commitSHA string) string { return repoID + "\x00" + commitSHA }

func (s *memStore) Close() error                 { return nil }
func (s *memStore) Ping(_ context.Context) error { return nil }

// --- Users ---

func (s *memStore) CreateUser(_ context.Context, username, tokenHash, role string) (*User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, u := range s.users {
		if u.Username == username {
			return nil, fmt.Errorf("create user: duplicate username %q", username)
		}
	}
	u := &User{
		ID:        uuid.New().String(),
		Username:  username,
		TokenHash: tokenHash,
		Role:      role,
		Active:    true,
		CreatedAt: time.Now(),
	}
	s.users[u.ID] = u
	if s.byHash == nil {
		s.byHash = map[string]*User{}
	}
	s.byHash[tokenHash] = u
	return u, nil
}

func (s *memStore) GetUserByTokenHash(_ context.Context, hash string) (*User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := s.byHash[hash]
	if u == nil || !u.Active {
		return nil, sql.ErrNoRows
	}
	return u, nil
}

func (s *memStore) GetUserByID(_ context.Context, id string) (*User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := s.users[id]
	if u == nil {
		return nil, sql.ErrNoRows
	}
	return u, nil
}

func (s *memStore) ListUsers(_ context.Context) ([]*User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*User, 0, len(s.users))
	for _, u := range s.users {
		out = append(out, u)
	}
	return out, nil
}

func (s *memStore) EnsureAdmin(_ context.Context, username, tokenHash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, u := range s.users {
		if u.Username == username {
			return nil
		}
	}
	u := &User{
		ID:        uuid.New().String(),
		Username:  username,
		TokenHash: tokenHash,
		Role:      "admin",
		Active:    true,
		CreatedAt: time.Now(),
	}
	s.users[u.ID] = u
	if s.byHash == nil {
		s.byHash = map[string]*User{}
	}
	s.byHash[tokenHash] = u
	return nil
}

func (s *memStore) UpdateUsername(_ context.Context, userID, username string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if u, ok := s.users[userID]; ok {
		u.Username = username
	}
	return nil
}

func (s *memStore) UpdateUser(_ context.Context, userID, username, role string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if u, ok := s.users[userID]; ok {
		u.Username = username
		u.Role = role
	}
	return nil
}

func (s *memStore) SetUserActive(_ context.Context, userID string, active bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if u, ok := s.users[userID]; ok {
		u.Active = active
	}
	return nil
}

func (s *memStore) RevokeUserToken(_ context.Context, userID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if u, ok := s.users[userID]; ok {
		if s.byHash != nil {
			delete(s.byHash, u.TokenHash)
		}
		u.TokenHash = ""
	}
	return nil
}

func (s *memStore) UpdateUserToken(_ context.Context, userID, tokenHash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if u, ok := s.users[userID]; ok {
		if s.byHash != nil {
			delete(s.byHash, u.TokenHash)
		}
		u.TokenHash = tokenHash
		if s.byHash == nil {
			s.byHash = map[string]*User{}
		}
		s.byHash[tokenHash] = u
	}
	return nil
}

func (s *memStore) DeleteUser(_ context.Context, userID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if u, ok := s.users[userID]; ok {
		if s.byHash != nil {
			delete(s.byHash, u.TokenHash)
		}
		delete(s.users, userID)
	}
	return nil
}

// --- Tasks ---

func (s *memStore) CreateTask(_ context.Context, userID, inputType, inputText, outputType string) (*Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := &Task{
		ID:         uuid.New().String(),
		UserID:     userID,
		Status:     "queued",
		InputType:  inputType,
		InputText:  inputText,
		OutputType: outputType,
		CreatedAt:  time.Now(),
	}
	s.tasks[t.ID] = t
	return t, nil
}

func (s *memStore) UpdateTaskStatus(_ context.Context, id, status string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := s.tasks[id]; ok {
		t.Status = status
		if status == "running" {
			now := time.Now()
			t.StartedAt = &now
		}
	}
	return nil
}

func (s *memStore) UpdateTaskPendingQuestion(_ context.Context, id, question string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := s.tasks[id]; ok {
		t.PendingQuestion = question
	}
	return nil
}

func (s *memStore) UpdateTaskResult(_ context.Context, id, status, prURL, errorMsg, output string, inputTokens, outputTokens int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := s.tasks[id]; ok {
		t.Status = status
		t.PRUrl = prURL
		t.ErrorMsg = errorMsg
		t.Output = output
		t.InputTokens = inputTokens
		t.OutputTokens = outputTokens
		now := time.Now()
		t.CompletedAt = &now
	}
	return nil
}

func (s *memStore) GetTask(_ context.Context, id string) (*Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.tasks[id]
	if t == nil {
		return nil, nil
	}
	cp := *t
	return &cp, nil
}

func (s *memStore) ListUserTasks(_ context.Context, userID string, limit int) ([]*Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*Task
	for _, t := range s.tasks {
		if t.UserID == userID {
			cp := *t
			out = append(out, &cp)
		}
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (s *memStore) MarkStaleTasksFailed(_ context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.tasks {
		if (t.Status == "running" || t.Status == "queued" || t.Status == "waiting_for_input") && t.CompletedAt == nil {
			t.Status = "failed"
			t.ErrorMsg = "Server restarted while task was in progress"
		}
	}
	return nil
}

// --- User settings ---

func (s *memStore) GetUserSettings(_ context.Context, userID string) (*UserSettings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if us, ok := s.settings[userID]; ok {
		cp := *us
		return &cp, nil
	}
	return &UserSettings{UserID: userID}, nil
}

func (s *memStore) UpsertUserSettings(_ context.Context, us *UserSettings) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *us
	cp.UpdatedAt = time.Now()
	s.settings[us.UserID] = &cp
	return nil
}

// --- Audit ---

func (s *memStore) RecordAuditEvent(_ context.Context, actorID, actorUsername, action, targetID, detail string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.audit = append(s.audit, &AuditEvent{
		ID:            uuid.New().String(),
		ActorID:       actorID,
		ActorUsername: actorUsername,
		Action:        action,
		TargetID:      targetID,
		Detail:        detail,
		CreatedAt:     time.Now(),
	})
	return nil
}

func (s *memStore) ListAuditEvents(_ context.Context, limit int) ([]*AuditEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*AuditEvent, 0, len(s.audit))
	for i := len(s.audit) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, s.audit[i])
	}
	return out, nil
}

// --- Repo index ---

func (s *memStore) GetRepoIndex(_ context.Context, repoID, commitSHA string) (*RepoIndexEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.repoIndex[repoIndexKey(repoID, commitSHA)]
	if !ok {
		return nil, nil
	}
	cp := *e
	return &cp, nil
}

func (s *memStore) SaveRepoIndex(_ context.Context, repoID, commitSHA string, summary json.RawMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.repoIndex[repoIndexKey(repoID, commitSHA)] = &RepoIndexEntry{
		RepoID:    repoID,
		CommitSHA: commitSHA,
		Summary:   summary,
		IndexedAt: time.Now(),
	}
	return nil
}
