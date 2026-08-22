package server

// Internal (package server, not server_test) so these tests can reach
// Runner's unexported sem/outcomes fields directly to simulate saturation
// and past task outcomes without needing a real LLM provider or a full
// queue/runner integration loop.

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tf-agent/tf-agent/internal/config"
	"github.com/tf-agent/tf-agent/internal/db"
	"github.com/tf-agent/tf-agent/internal/llm"
	"github.com/tf-agent/tf-agent/internal/queue"
)

// newCapacityTestServer wires a full Server + Handler for HTTP-level tests
// without starting the Runner's queue-pop loop, so a test can saturate the
// semaphore directly and know no worker goroutine will race it draining the
// queue.
func newCapacityTestServer(t *testing.T) (ts *httptest.Server, runner *Runner, token string) {
	t.Helper()
	store := db.NewMemoryStore()
	t.Cleanup(func() { store.Close() })

	t.Setenv("TF_AGENT_ENCRYPTION_KEY", "0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20")
	if err := LoadEncryptionKey(); err != nil {
		t.Fatalf("LoadEncryptionKey: %v", err)
	}

	token = "capacity-test-token"
	if _, err := store.CreateUser(context.Background(), "capacity-user", HashToken(token), "member"); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	cfg := config.Defaults()
	cfg.Server.LLMConcurrency = 1
	cfg.Provider.Anthropic.APIKey = "test-key"

	hub := NewHub()
	q := queue.NewMemoryQueue(100)
	runner = NewRunner(hub, store, q, &llm.MockProvider{}, cfg, slog.Default())

	srv := NewServer(store, hub, map[string]queue.Queue{"default": q}, runner, cfg, embed.FS{})
	ts = httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	return ts, runner, token
}

func postJSON(t *testing.T, ts *httptest.Server, path, token string, body any) *http.Response {
	t.Helper()
	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req, err := http.NewRequest("POST", ts.URL+path, bytes.NewReader(buf))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	return resp
}

// TestHandleSubmitTask_ReturnsServiceUnavailable_WhenGlobalSemaphoreFull
// exercises A2's HTTP-submission-time fast-fail path: a saturated global LLM
// semaphore should make the server reject a new task immediately with 503,
// rather than accepting it into the queue only to have it fail later after
// timing out on the semaphore wait inside run().
func TestHandleSubmitTask_ReturnsServiceUnavailable_WhenGlobalSemaphoreFull(t *testing.T) {
	ts, runner, token := newCapacityTestServer(t)

	// Saturate the global semaphore directly — this test is purely about the
	// submission-time admission check, not actual task execution.
	runner.sem <- struct{}{}

	resp := postJSON(t, ts, "/v1/tasks", token, map[string]any{
		"input":  map[string]string{"type": "prompt", "text": "create an S3 bucket"},
		"output": map[string]string{"type": "print"},
	})
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 when the global LLM semaphore is saturated", resp.StatusCode)
	}
}

// TestHandleSubmitTask_AcceptsTask_WhenSemaphoreHasCapacity is the negative
// case: with an open slot, submission must succeed as normal — the new
// admission check must not reject perfectly healthy submissions.
func TestHandleSubmitTask_AcceptsTask_WhenSemaphoreHasCapacity(t *testing.T) {
	ts, _, token := newCapacityTestServer(t)

	resp := postJSON(t, ts, "/v1/tasks", token, map[string]any{
		"input":  map[string]string{"type": "prompt", "text": "create an S3 bucket"},
		"output": map[string]string{"type": "print"},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("status = %d, want 201 when the semaphore has an open slot", resp.StatusCode)
	}
}

// TestHandleSubmitTask_CapacityCheckToleratesNilRunner guards the nil-runner
// defensive check added alongside the capacity admission logic — some tests
// (and, in principle, a server wired without a runner) construct a Server
// with a nil *Runner, and submission must not panic in that case (existing
// unrelated failures — e.g. no queue routing — are fine).
func TestHandleSubmitTask_CapacityCheckToleratesNilRunner(t *testing.T) {
	store := db.NewMemoryStore()
	t.Cleanup(func() { store.Close() })
	t.Setenv("TF_AGENT_ENCRYPTION_KEY", "0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20")
	if err := LoadEncryptionKey(); err != nil {
		t.Fatalf("LoadEncryptionKey: %v", err)
	}
	token := "nil-runner-token"
	if _, err := store.CreateUser(context.Background(), "nil-runner-user", HashToken(token), "member"); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	cfg := config.Defaults()
	cfg.Provider.Anthropic.APIKey = "test-key"
	q := queue.NewMemoryQueue(100)
	srv := NewServer(store, NewHub(), map[string]queue.Queue{"default": q}, nil, cfg, embed.FS{})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	resp := postJSON(t, ts, "/v1/tasks", token, map[string]any{
		"input":  map[string]string{"type": "prompt", "text": "x"},
		"output": map[string]string{"type": "print"},
	})
	// The point of this test is "did not panic"; a nil runner still can't
	// actually run anything, so 201 is the only meaningful success signal
	// (the queue push itself doesn't need a runner).
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("status = %d, want 201 (nil runner must not break the capacity check)", resp.StatusCode)
	}
}

// --- A4: /healthz concurrency + error-rate reporting ---

func TestHandleHealth_ReportsConcurrencyAndErrorRate(t *testing.T) {
	ts, runner, _ := newCapacityTestServer(t)

	runner.sem <- struct{}{} // one slot in use, out of capacity 1
	runner.outcomes.record(true)
	runner.outcomes.record(false)

	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	var body struct {
		Status         string `json:"status"`
		LLMConcurrency struct {
			Used     int `json:"used"`
			Capacity int `json:"capacity"`
		} `json:"llm_concurrency"`
		TaskErrorRate struct {
			Rate   float64 `json:"rate"`
			Failed int     `json:"failed"`
			Total  int     `json:"total"`
		} `json:"task_error_rate"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode healthz body: %v", err)
	}

	if body.LLMConcurrency.Used != 1 || body.LLMConcurrency.Capacity != 1 {
		t.Errorf("llm_concurrency = %+v, want used=1 capacity=1", body.LLMConcurrency)
	}
	if body.TaskErrorRate.Failed != 1 || body.TaskErrorRate.Total != 2 || body.TaskErrorRate.Rate != 0.5 {
		t.Errorf("task_error_rate = %+v, want failed=1 total=2 rate=0.5", body.TaskErrorRate)
	}
}

// TestHandleHealth_SaturationAndErrorsDoNotFlipUnhealthy is the A4
// self-review requirement: a fully saturated semaphore plus a 100% recent
// task error rate must not, by themselves, turn /healthz unhealthy or flip
// its status code to 503 — that's exactly the "LLM API is down but this
// process is structurally fine" case the brief calls out, which the
// existing api_key/database/encryption_key checks already own.
func TestHandleHealth_SaturationAndErrorsDoNotFlipUnhealthy(t *testing.T) {
	ts, runner, _ := newCapacityTestServer(t)

	runner.sem <- struct{}{} // saturate the one slot
	for i := 0; i < 5; i++ {
		runner.outcomes.record(true) // 100% recent failure rate
	}

	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status code = %d, want 200 even at full saturation + 100%% error rate", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode healthz body: %v", err)
	}
	if body["status"] != "ok" {
		t.Errorf("status field = %v, want ok — saturation/error-rate must not be conflated with structural health", body["status"])
	}
}
