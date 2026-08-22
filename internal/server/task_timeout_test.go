package server_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestServer_TaskExceedsMaxDuration_MarkedFailed verifies the task-level
// execution watchdog: a task whose active execution time exceeds
// cfg.Agent.MaxTaskDuration is cancelled and marked failed with a clear
// reason, rather than being allowed to run forever.
func TestServer_TaskExceedsMaxDuration_MarkedFailed(t *testing.T) {
	env := newTestEnv(t)
	env.cfg.Agent.MaxTaskDuration = 1    // seconds — watchdog should fire ~1s in
	env.provider.Delay = 5 * time.Second // the "LLM call" blocks far longer than that

	submit := env.do("POST", "/v1/tasks", env.memberToken, map[string]any{
		"input":  map[string]string{"type": "prompt", "text": "create S3 bucket"},
		"output": map[string]string{"type": "print"},
	})
	if submit.StatusCode != http.StatusCreated {
		t.Fatalf("submit status = %d, want 201", submit.StatusCode)
	}
	var body map[string]string
	_ = json.NewDecoder(submit.Body).Decode(&body)
	taskID := body["task_id"]
	if taskID == "" {
		t.Fatal("expected task_id in submit response")
	}

	deadline := time.Now().Add(4 * time.Second)
	var status, errMsg string
	for time.Now().Before(deadline) {
		r := env.do("GET", "/v1/tasks/"+taskID, env.memberToken, nil)
		var got map[string]any
		_ = json.NewDecoder(r.Body).Decode(&got)
		r.Body.Close()
		status, _ = got["status"].(string)
		errMsg, _ = got["error_msg"].(string)
		if status == "done" || status == "failed" || status == "cancelled" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if status != "failed" {
		t.Fatalf("status = %q, want failed", status)
	}
	if !strings.Contains(errMsg, "exceeded maximum execution time") {
		t.Errorf("error_msg = %q, want it to mention exceeding the maximum execution time", errMsg)
	}
}
