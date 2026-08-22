package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/tf-agent/tf-agent/internal/server"
)

// outputResponse mirrors handleGetTaskOutput's JSON shape.
type outputResponse struct {
	TaskID      string `json:"task_id"`
	Offset      int    `json:"offset"`
	Limit       int    `json:"limit"`
	TotalLength int    `json:"total_length"`
	Output      string `json:"output"`
}

// completedTaskWithOutput submits a task as token and polls until it reaches
// a terminal state, returning its ID and the store's own record of its full
// output — the mock provider used by newTestEnv always emits the fixed text
// "resource \"aws_s3_bucket\" \"main\" {}" (see server_test.go).
func completedTaskWithOutput(t *testing.T, env *testEnv, token string) (taskID, fullOutput string) {
	t.Helper()
	resp := env.do("POST", "/v1/tasks", token, map[string]any{
		"input":  map[string]string{"type": "prompt", "text": "create S3 bucket"},
		"output": map[string]string{"type": "print"},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("submit task: status = %d, want 201", resp.StatusCode)
	}
	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode submit body: %v", err)
	}
	taskID = body["task_id"]
	if taskID == "" {
		t.Fatal("empty task_id in submit response")
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		r := env.do("GET", "/v1/tasks/"+taskID, token, nil)
		var task map[string]any
		_ = json.NewDecoder(r.Body).Decode(&task)
		r.Body.Close()
		if s, _ := task["status"].(string); s == "done" || s == "failed" {
			out, _ := task["output"].(string)
			return taskID, out
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("task did not reach a terminal state in time")
	return "", ""
}

func TestGetTaskOutput_FullOutput_DefaultParams(t *testing.T) {
	env := newTestEnv(t)
	taskID, fullOutput := completedTaskWithOutput(t, env, env.memberToken)
	if fullOutput == "" {
		t.Fatal("expected a non-empty mock task output to test pagination against")
	}

	resp := env.do("GET", "/v1/tasks/"+taskID+"/output", env.memberToken, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body outputResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Offset != 0 {
		t.Errorf("offset = %d, want 0 (default)", body.Offset)
	}
	if body.TotalLength != len(fullOutput) {
		t.Errorf("total_length = %d, want %d", body.TotalLength, len(fullOutput))
	}
	if body.Output != fullOutput {
		t.Errorf("output = %q, want the full stored output %q (well under the default limit)", body.Output, fullOutput)
	}
	if body.TaskID != taskID {
		t.Errorf("task_id = %q, want %q", body.TaskID, taskID)
	}
}

func TestGetTaskOutput_OffsetAndLimit(t *testing.T) {
	env := newTestEnv(t)
	taskID, fullOutput := completedTaskWithOutput(t, env, env.memberToken)
	if len(fullOutput) < 10 {
		t.Fatalf("mock output %q too short for this test's offset/limit values", fullOutput)
	}

	resp := env.do("GET", "/v1/tasks/"+taskID+"/output?offset=2&limit=5", env.memberToken, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body outputResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := fullOutput[2:7]
	if body.Output != want {
		t.Errorf("output = %q, want %q (bytes [2:7] of %q)", body.Output, want, fullOutput)
	}
	if body.Offset != 2 || body.Limit != 5 {
		t.Errorf("offset/limit = %d/%d, want 2/5", body.Offset, body.Limit)
	}
	if body.TotalLength != len(fullOutput) {
		t.Errorf("total_length = %d, want %d", body.TotalLength, len(fullOutput))
	}
}

func TestGetTaskOutput_OffsetBeyondLength_ReturnsEmptyNotError(t *testing.T) {
	env := newTestEnv(t)
	taskID, fullOutput := completedTaskWithOutput(t, env, env.memberToken)

	resp := env.do("GET", "/v1/tasks/"+taskID+"/output?offset=99999", env.memberToken, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body outputResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Output != "" {
		t.Errorf("output = %q, want empty string for an offset past the end", body.Output)
	}
	if body.TotalLength != len(fullOutput) {
		t.Errorf("total_length = %d, want %d", body.TotalLength, len(fullOutput))
	}
}

func TestGetTaskOutput_HugeLimitDoesNotOverflowIntoPanic(t *testing.T) {
	env := newTestEnv(t)
	taskID, fullOutput := completedTaskWithOutput(t, env, env.memberToken)

	// A limit near math.MaxInt64 used to make start+limit overflow into a
	// large negative number, which slipped past the "end > total" clamp and
	// panicked on the output[start:end] slice.
	resp := env.do("GET", "/v1/tasks/"+taskID+"/output?offset=0&limit=9223372036854775807", env.memberToken, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body outputResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Output != fullOutput {
		t.Errorf("output = %q, want the full output %q", body.Output, fullOutput)
	}

	resp2 := env.do("GET", "/v1/tasks/"+taskID+"/output?offset=9223372036854775807&limit=9223372036854775807", env.memberToken, nil)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp2.StatusCode)
	}
	var body2 outputResponse
	if err := json.NewDecoder(resp2.Body).Decode(&body2); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body2.Output != "" {
		t.Errorf("output = %q, want empty for an offset past the end", body2.Output)
	}
}

func TestGetTaskOutput_InvalidOffsetOrLimit(t *testing.T) {
	env := newTestEnv(t)
	taskID, _ := completedTaskWithOutput(t, env, env.memberToken)

	cases := []string{
		"?offset=-1",
		"?offset=abc",
		"?limit=0",
		"?limit=-5",
		"?limit=notanumber",
	}
	for _, q := range cases {
		resp := env.do("GET", "/v1/tasks/"+taskID+"/output"+q, env.memberToken, nil)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("query %q: status = %d, want 400", q, resp.StatusCode)
		}
	}
}

func TestGetTaskOutput_NotFound(t *testing.T) {
	env := newTestEnv(t)
	resp := env.do("GET", "/v1/tasks/does-not-exist/output", env.memberToken, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestGetTaskOutput_Unauthenticated(t *testing.T) {
	env := newTestEnv(t)
	taskID, _ := completedTaskWithOutput(t, env, env.memberToken)

	resp := env.do("GET", "/v1/tasks/"+taskID+"/output", "", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func TestGetTaskOutput_ForbiddenForOtherUser(t *testing.T) {
	env := newTestEnv(t)
	taskID, _ := completedTaskWithOutput(t, env, env.memberToken)

	ctx := context.Background()
	bobToken := "bob-output-token"
	if _, err := env.store.CreateUser(ctx, "bob-output", server.HashToken(bobToken), "member"); err != nil {
		t.Fatalf("create bob: %v", err)
	}

	resp := env.do("GET", "/v1/tasks/"+taskID+"/output", bobToken, nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
}

func TestGetTaskOutput_AdminCanReadAnyUsersOutput(t *testing.T) {
	env := newTestEnv(t)
	taskID, fullOutput := completedTaskWithOutput(t, env, env.memberToken)

	resp := env.do("GET", "/v1/tasks/"+taskID+"/output", env.adminToken, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin status = %d, want 200", resp.StatusCode)
	}
	var body outputResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Output != fullOutput {
		t.Errorf("admin-read output = %q, want %q", body.Output, fullOutput)
	}
}
