package server

// Tests for Hub.ServeSSE's source selection. In-package (rather than in
// server_test.go) so they can shorten the timing knobs — a 10s local-ownership
// wait and a 30s relay re-check are not things a unit test should sit through.

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// stubRelay stands in for a real cross-pod relay. echo mirrors what NATS
// actually does with a pod's own publishes: they come back on that pod's own
// subscription too, which is exactly why ServeSSE must never read both the
// local channel and the relay.
type stubRelay struct {
	ch   chan ServerEvent
	echo bool
}

func newStubRelay(buf int) *stubRelay { return &stubRelay{ch: make(chan ServerEvent, buf)} }

func (s *stubRelay) Publish(_ string, ev ServerEvent) {
	if !s.echo {
		return
	}
	select {
	case s.ch <- ev:
	default:
	}
}

func (s *stubRelay) Subscribe(string) (<-chan ServerEvent, func()) {
	return s.ch, func() {}
}

// shortenStreamTimings makes ServeSSE's waits test-sized.
func shortenStreamTimings(t *testing.T) {
	t.Helper()
	wait, poll, recheck := localChannelWaitTimeout, localChannelPollInterval, relayStatusRecheckInterval
	localChannelWaitTimeout = 500 * time.Millisecond
	localChannelPollInterval = 20 * time.Millisecond
	relayStatusRecheckInterval = 50 * time.Millisecond
	t.Cleanup(func() {
		localChannelWaitTimeout, localChannelPollInterval, relayStatusRecheckInterval = wait, poll, recheck
	})
}

func serveSSEBody(t *testing.T, h *Hub, taskID string, initial *ServerEvent, budget time.Duration) (string, time.Duration) {
	t.Helper()
	req := httptest.NewRequest("GET", "/v1/tasks/"+taskID+"/stream", nil)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	start := time.Now()
	go func() {
		defer close(done)
		h.ServeSSE(rec, req, taskID, initial)
	}()

	select {
	case <-done:
	case <-time.After(budget):
		t.Fatalf("ServeSSE did not return within %v", budget)
	}
	return rec.Body.String(), time.Since(start)
}

// TestServeSSE_RelayPathStartsImmediately is the core C2 regression test: a
// task running on another pod must be streamed from the relay right away, not
// after the local-ownership wait has expired.
func TestServeSSE_RelayPathStartsImmediately(t *testing.T) {
	relay := newStubRelay(8)
	h := NewHub()
	h.SetRelay(relay)

	// The owning pod publishes before this pod's client even connects.
	relay.ch <- ServerEvent{Type: "permission_request", Tool: "Bash", Text: "terraform apply"}
	relay.ch <- ServerEvent{Type: "done", PRUrl: "https://example.com/pr/7"}

	body, elapsed := serveSSEBody(t, h, "remote-task", nil, 5*time.Second)

	if !strings.Contains(body, "terraform apply") {
		t.Errorf("relayed event missing from stream: %q", body)
	}
	if !strings.Contains(body, `"type":"done"`) {
		t.Errorf("terminal relayed event missing from stream: %q", body)
	}
	if elapsed > 2*time.Second {
		t.Errorf("took %v — the relay must become the active source without waiting out the local-ownership window", elapsed)
	}
}

// TestServeSSE_IgnoresUnownedLocalChannel covers the seam that made the
// original design unsafe: Hub.Create runs on whichever pod accepted the task
// submission, so a local channel can exist on a pod that is not running the
// task — and nothing is ever published to it there. The stream must read the
// relay anyway.
func TestServeSSE_IgnoresUnownedLocalChannel(t *testing.T) {
	relay := newStubRelay(8)
	h := NewHub()
	h.SetRelay(relay)

	// This pod accepted the POST (so it has a channel) but another pod runs
	// the task (so this channel stays empty forever).
	h.Create("submitted-here")

	relay.ch <- ServerEvent{Type: "text", Text: "from the owning pod"}
	relay.ch <- ServerEvent{Type: "done"}

	body, elapsed := serveSSEBody(t, h, "submitted-here", nil, 5*time.Second)

	if !strings.Contains(body, "from the owning pod") {
		t.Errorf("stream read the empty local channel instead of the relay: %q", body)
	}
	if elapsed > 2*time.Second {
		t.Errorf("took %v, want prompt delivery", elapsed)
	}
}

// TestServeSSE_LocalPathWinsWhenClaimed is the mirror image: once this pod
// claims the task, the local channel is authoritative and the relay
// subscription is dropped unread — so each event is delivered exactly once
// even though Hub.Publish wrote it to both.
func TestServeSSE_LocalPathWinsWhenClaimed(t *testing.T) {
	shortenStreamTimings(t)

	relay := newStubRelay(8)
	relay.echo = true // model NATS delivering this pod's own publishes back
	h := NewHub()
	h.SetRelay(relay)

	h.Create("mine") // submitted here
	h.Claim("mine")  // ...and this pod runs it
	h.Publish("mine", ServerEvent{Type: "text", Text: "only-once-marker"})
	h.Publish("mine", ServerEvent{Type: "done"})

	body, _ := serveSSEBody(t, h, "mine", nil, 5*time.Second)

	if n := strings.Count(body, "only-once-marker"); n != 1 {
		t.Errorf("event delivered %d times, want exactly 1 — both sources were read\nbody: %q", n, body)
	}
}

// TestServeSSE_NoDuplicateWhenClaimArrivesLate exercises the race the source
// commit exists to make safe: the relay subscription is already open (and has
// unread events on it) when this pod claims the task and starts publishing
// locally.
func TestServeSSE_NoDuplicateWhenClaimArrivesLate(t *testing.T) {
	shortenStreamTimings(t)

	relay := newStubRelay(8)
	relay.echo = true
	h := NewHub()
	h.SetRelay(relay)

	req := httptest.NewRequest("GET", "/v1/tasks/late/stream", nil)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ServeSSE(rec, req, "late", nil)
	}()

	// Let the relay subscription and the ownership wait get going first.
	time.Sleep(100 * time.Millisecond)

	h.Claim("late")
	h.Publish("late", ServerEvent{Type: "text", Text: "only-once-marker"})
	h.Publish("late", ServerEvent{Type: "done"})

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("ServeSSE did not return after the task finished")
	}

	body := rec.Body.String()
	if n := strings.Count(body, "only-once-marker"); n != 1 {
		t.Errorf("event delivered %d times, want exactly 1\nbody: %q", n, body)
	}
	if !strings.Contains(body, `"type":"done"`) {
		t.Errorf("terminal event missing: %q", body)
	}
}

// TestServeSSE_IdleRecheckEndsStream covers the permanent-hang half of C2: the
// relay's channel is never closed, so a terminal event that never arrives
// (lost message, owning pod died) used to leave the client heartbeating
// forever.
func TestServeSSE_IdleRecheckEndsStream(t *testing.T) {
	shortenStreamTimings(t)

	relay := newStubRelay(4)
	h := NewHub()
	h.SetRelay(relay)
	h.SetStatusCheck(func(string) *ServerEvent {
		return &ServerEvent{Type: "done", PRUrl: "https://example.com/pr/9"}
	})

	// One event arrives, then the owning pod goes silent forever.
	relay.ch <- ServerEvent{Type: "text", Text: "still working"}

	body, elapsed := serveSSEBody(t, h, "abandoned-task", nil, 5*time.Second)

	if !strings.Contains(body, "https://example.com/pr/9") {
		t.Errorf("stream did not end from the durable-state re-check: %q", body)
	}
	if elapsed > 3*time.Second {
		t.Errorf("took %v — the re-check should have ended the stream promptly", elapsed)
	}
}

// TestServeSSE_IdleRecheckKeepsLongRunningStreamOpen guards the other side of
// that backstop: a task that is merely quiet must not be cut off.
func TestServeSSE_IdleRecheckKeepsLongRunningStreamOpen(t *testing.T) {
	shortenStreamTimings(t)

	relay := newStubRelay(4)
	h := NewHub()
	h.SetRelay(relay)
	checks := 0
	h.SetStatusCheck(func(string) *ServerEvent {
		checks++
		return &ServerEvent{Type: "status", Status: "running"}
	})
	relay.ch <- ServerEvent{Type: "text", Text: "still working"}

	req := httptest.NewRequest("GET", "/v1/tasks/slow/stream", nil)
	ctx, cancel := context.WithCancel(req.Context())
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ServeSSE(rec, req, "slow", nil)
	}()

	select {
	case <-done:
		t.Fatal("ServeSSE ended a still-running task's stream")
	case <-time.After(300 * time.Millisecond):
	}

	cancel() // client disconnects
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ServeSSE did not return after the client disconnected")
	}
	if checks == 0 {
		t.Error("expected the durable-state re-check to have run at least once")
	}
}

// TestServeSSE_NoRelayConfigured_UnknownTaskReportsNotFound pins the
// single-process behavior: with no relay there is nothing to fall back to, and
// an unknown task must still end with the original error rather than hanging.
func TestServeSSE_NoRelayConfigured_UnknownTaskReportsNotFound(t *testing.T) {
	shortenStreamTimings(t)

	h := NewHub()
	body, _ := serveSSEBody(t, h, "no-such-task", nil, 5*time.Second)
	if !strings.Contains(body, "task not found or already completed") {
		t.Errorf("body = %q, want the unchanged not-found error", body)
	}
}

// TestServeSSE_NoRelayConfigured_UsesLocalChannelImmediately pins the other
// half of that behavior: without a relay, a channel created at submission time
// is usable straight away, before any Claim — a queued task's client must not
// be made to wait.
func TestServeSSE_NoRelayConfigured_UsesLocalChannelImmediately(t *testing.T) {
	shortenStreamTimings(t)

	h := NewHub()
	ch := h.Create("queued-task")
	ch <- ServerEvent{Type: "status", Status: "queued"}
	ch <- ServerEvent{Type: "done"}

	body, elapsed := serveSSEBody(t, h, "queued-task", nil, 2*time.Second)
	if !strings.Contains(body, `"status":"queued"`) {
		t.Errorf("body = %q, want the buffered local events", body)
	}
	if elapsed > time.Second {
		t.Errorf("took %v, want immediate use of the local channel", elapsed)
	}
}
