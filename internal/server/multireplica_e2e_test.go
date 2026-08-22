//go:build integration

package server

// Two-pod end-to-end tests for the multi-replica story, over a real NATS
// server. Run with:
//
//	NATS_URL=nats://localhost:4222 go test -tags=integration ./internal/server/... -run MultiReplica
//
// Every other test in this package exercises one seam in isolation — the NATS
// transport with fake handlers, or Hub/Runner logic with a fake relay. These
// stand up two genuinely independent Hub+Runner+connection triples against one
// shared NATS server and drive a full round trip through it, which is where the
// interesting bugs live: nothing in a single-seam test can see that "this pod
// has a local channel for the task" does not mean "this pod is running it".
//
// package server (not server_test) so a pod's Runner can be put into the
// "owns this task" state the same way run() does, without needing a real LLM
// provider and a full task lifecycle.

import (
	"context"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/tf-agent/tf-agent/internal/config"
	"github.com/tf-agent/tf-agent/internal/db"
	"github.com/tf-agent/tf-agent/internal/llm"
	"github.com/tf-agent/tf-agent/internal/queue"
)

// e2ePod is one server replica: its own store, Hub, Runner and NATS
// connection, wired exactly the way cmd/server/main.go wires them when
// queue_driver=nats.
type e2ePod struct {
	nc     *nats.Conn
	hub    *Hub
	runner *Runner
	store  db.Store
}

func newE2EPod(t *testing.T) *e2ePod {
	t.Helper()
	nc := natsConnForControlTest(t) // skips the test when NATS_URL is unset
	store := db.NewMemoryStore()
	hub := NewHub()
	runner := NewRunner(hub, store, queue.NewMemoryQueue(10), &llm.MockProvider{}, config.Defaults(), slog.Default())

	hub.SetRelay(NewNATSEventRelay(nc))
	hub.SetStatusCheck(func(taskID string) *ServerEvent {
		task, err := store.GetTask(context.Background(), taskID)
		if err != nil || task == nil {
			return nil
		}
		return InitialSnapshotEvent(task)
	})
	controlRelay, err := NewNATSControlRelay(nc, runner)
	if err != nil {
		t.Fatalf("NewNATSControlRelay: %v", err)
	}
	runner.SetControlRelay(controlRelay)

	return &e2ePod{nc: nc, hub: hub, runner: runner, store: store}
}

// owns puts pod's Runner into the state run() would be in while the task is
// paused and cancellable: an answer channel, a permission channel and a cancel
// func registered locally. Returns them for assertions.
func (p *e2ePod) owns(taskID string) (chan string, chan bool, context.Context) {
	answerCh := make(chan string, 1)
	p.runner.answerMu.Lock()
	p.runner.answers[taskID] = answerCh
	p.runner.answerMu.Unlock()

	permCh := make(chan bool, 1)
	p.runner.permissionMu.Lock()
	p.runner.permissions[taskID] = permCh
	p.runner.permissionMu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	p.runner.cancelMu.Lock()
	p.runner.cancels[taskID] = cancel
	p.runner.cancelMu.Unlock()

	return answerCh, permCh, ctx
}

func e2eTaskID(prefix string) string {
	return fmt.Sprintf("e2e-%s-%d", prefix, time.Now().UnixNano())
}

// TestMultiReplica_StreamingPodReceivesOwningPodsEvents is the cross-pod SSE
// round trip: pod A runs the task, a client's stream landed on pod B, and pod
// B has to deliver pod A's events — promptly, once each, and ending when the
// task ends.
//
// The submittedOnStreamingPod case is the one that matters most: pod B holds a
// local event channel for the task (it accepted the POST) that pod A's events
// will never reach, so a design that treats a local channel as proof of
// ownership streams silence forever.
func TestMultiReplica_StreamingPodReceivesOwningPodsEvents(t *testing.T) {
	for _, submittedOnStreamingPod := range []bool{false, true} {
		name := "submitted-elsewhere"
		if submittedOnStreamingPod {
			name = "submitted-on-streaming-pod"
		}
		t.Run(name, func(t *testing.T) {
			podA := newE2EPod(t) // runs the task
			podB := newE2EPod(t) // the client's SSE connection landed here
			taskID := e2eTaskID("events")

			if submittedOnStreamingPod {
				podB.hub.Create(taskID)
			}
			podA.hub.Claim(taskID)

			req := httptest.NewRequest("GET", "/v1/tasks/"+taskID+"/stream", nil)
			rec := httptest.NewRecorder()
			done := make(chan struct{})
			go func() {
				defer close(done)
				podB.hub.ServeSSE(rec, req, taskID, &ServerEvent{Type: "status", Status: "running"})
			}()

			// Let pod B's NATS subscription register, then publish from pod A
			// well inside the window the old design spent waiting for a local
			// channel — every one of these events used to be lost.
			time.Sleep(300 * time.Millisecond)
			publishedAt := time.Now()
			podA.hub.Publish(taskID, ServerEvent{Type: "permission_request", Tool: "Bash", Text: "terraform apply"})
			podA.hub.Publish(taskID, ServerEvent{Type: "text", Text: "cross-pod-marker"})
			podA.hub.Publish(taskID, ServerEvent{Type: "done", PRUrl: "https://example.com/pr/1"})

			select {
			case <-done:
			case <-time.After(15 * time.Second):
				t.Fatalf("pod B's stream never ended — pod A's events did not reach it (body so far: %q)", rec.Body.String())
			}

			elapsed := time.Since(publishedAt)
			body := rec.Body.String()

			if !strings.Contains(body, "terraform apply") {
				t.Errorf("permission_request published by pod A missing from pod B's stream: %q", body)
			}
			if n := strings.Count(body, "cross-pod-marker"); n != 1 {
				t.Errorf("text event delivered %d times, want exactly 1: %q", n, body)
			}
			if !strings.Contains(body, "https://example.com/pr/1") {
				t.Errorf("terminal event missing from pod B's stream: %q", body)
			}
			if elapsed > 5*time.Second {
				t.Errorf("pod B took %v after pod A published — events must stream as they arrive, not after the local-ownership window", elapsed)
			}
		})
	}
}

// TestMultiReplica_OwningPodStreamsItsOwnEventsOnce is the other side of the
// same coin: on the pod that runs the task, every event exists both locally and
// on the relay (NATS delivers a pod its own publishes), so the stream must read
// one source, not both.
func TestMultiReplica_OwningPodStreamsItsOwnEventsOnce(t *testing.T) {
	pod := newE2EPod(t)
	taskID := e2eTaskID("self")

	pod.hub.Create(taskID)
	pod.hub.Claim(taskID)

	req := httptest.NewRequest("GET", "/v1/tasks/"+taskID+"/stream", nil)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		pod.hub.ServeSSE(rec, req, taskID, nil)
	}()

	time.Sleep(300 * time.Millisecond)
	pod.hub.Publish(taskID, ServerEvent{Type: "text", Text: "same-pod-marker"})
	pod.hub.Publish(taskID, ServerEvent{Type: "done"})

	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatalf("stream never ended (body so far: %q)", rec.Body.String())
	}

	body := rec.Body.String()
	if n := strings.Count(body, "same-pod-marker"); n != 1 {
		t.Errorf("event delivered %d times, want exactly 1 — the local channel and the relay were both read: %q", n, body)
	}
}

// TestMultiReplica_ControlRequestsReachOwningPod covers the control plane:
// answer, permission and cancel requests submitted on the pod that does NOT
// own the task must be delivered to the pod that does.
func TestMultiReplica_ControlRequestsReachOwningPod(t *testing.T) {
	podA := newE2EPod(t) // owns the task
	podB := newE2EPod(t) // the HTTP request landed here
	taskID := e2eTaskID("control")

	answerCh, permCh, taskCtx := podA.owns(taskID)

	// Let both pods' tf.control.* subscriptions register.
	time.Sleep(300 * time.Millisecond)

	if err := podB.runner.SendAnswer(taskID, "eu-west-1"); err != nil {
		t.Fatalf("SendAnswer on the non-owning pod: %v", err)
	}
	select {
	case got := <-answerCh:
		if got != "eu-west-1" {
			t.Errorf("owning pod received answer %q, want eu-west-1", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("answer never reached the owning pod")
	}

	// A deny is the security-relevant case, and coincides with Go's zero
	// value — it has to cross the wire as an explicit false.
	if err := podB.runner.SendPermissionResponse(taskID, false); err != nil {
		t.Fatalf("SendPermissionResponse on the non-owning pod: %v", err)
	}
	select {
	case allow := <-permCh:
		if allow {
			t.Error("owning pod received allow=true for a denied permission request")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("permission decision never reached the owning pod")
	}

	if err := podB.runner.CancelTask(taskID); err != nil {
		t.Fatalf("CancelTask on the non-owning pod: %v", err)
	}
	select {
	case <-taskCtx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation never reached the owning pod")
	}
}

// TestMultiReplica_UnownedControlRequestFailsPromptly is the live proof of the
// C1 fix. Every pod subscribes to tf.control.*, so NATS always sees interest
// and never fast-fails with ErrNoResponders; a request for a task no pod owns
// blocks for exactly as long as its context allows. With an unbounded context
// that is forever, in an HTTP handler goroutine.
func TestMultiReplica_UnownedControlRequestFailsPromptly(t *testing.T) {
	orig := controlRelayTimeout
	controlRelayTimeout = time.Second
	t.Cleanup(func() { controlRelayTimeout = orig })

	// Two pods, both subscribed, neither owning the task.
	newE2EPod(t)
	pod := newE2EPod(t)
	time.Sleep(300 * time.Millisecond)

	cases := []struct {
		name string
		call func(string) error
	}{
		{"answer", func(id string) error { return pod.runner.SendAnswer(id, "x") }},
		{"permission", func(id string) error { return pod.runner.SendPermissionResponse(id, true) }},
		{"cancel", func(id string) error { return pod.runner.CancelTask(id) }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			taskID := e2eTaskID("orphan")
			result := make(chan error, 1)
			start := time.Now()
			go func() { result <- tc.call(taskID) }()

			select {
			case err := <-result:
				if err == nil {
					t.Error("expected an error for a task no pod owns")
				}
				if elapsed := time.Since(start); elapsed > 4*time.Second {
					t.Errorf("returned after %v, want ~%v", elapsed, controlRelayTimeout)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("request never returned — the relay call is unbounded against a live NATS server (C1)")
			}
		})
	}
}

// TestMultiReplica_StreamEndsWhenOwningPodDies covers the permanent-hang half
// of C2 against real NATS: the owning pod's terminal event never crosses (it
// died), and the streaming pod has to notice from durable state instead of
// heartbeating at the client forever.
func TestMultiReplica_StreamEndsWhenOwningPodDies(t *testing.T) {
	origWait, origRecheck := localChannelWaitTimeout, relayStatusRecheckInterval
	localChannelWaitTimeout = time.Second
	relayStatusRecheckInterval = 300 * time.Millisecond
	t.Cleanup(func() {
		localChannelWaitTimeout, relayStatusRecheckInterval = origWait, origRecheck
	})

	podA := newE2EPod(t)
	podB := newE2EPod(t)

	// The task exists durably and pod B can read it — the row is what a
	// crashed pod's task is left in once MarkStaleTasksFailed runs.
	task, err := podB.store.CreateTask(context.Background(), "user-1", "prompt", "do a thing", "print")
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	taskID := task.ID

	podA.hub.Claim(taskID)

	req := httptest.NewRequest("GET", "/v1/tasks/"+taskID+"/stream", nil)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		podB.hub.ServeSSE(rec, req, taskID, nil)
	}()

	time.Sleep(300 * time.Millisecond)
	podA.hub.Publish(taskID, ServerEvent{Type: "text", Text: "working on it"})

	// Pod A dies here: no terminal event ever crosses the relay. Its task row
	// is reconciled to failed, the way MarkStaleTasksFailed does at restart.
	if err := podB.store.UpdateTaskResult(context.Background(), taskID, "failed",
		"", "Server restarted while task was in progress", "", 0, 0); err != nil {
		t.Fatalf("UpdateTaskResult: %v", err)
	}

	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatalf("stream never ended after the owning pod stopped publishing (body: %q)", rec.Body.String())
	}

	body := rec.Body.String()
	if !strings.Contains(body, "working on it") {
		t.Errorf("live relayed event missing: %q", body)
	}
	if !strings.Contains(body, "Server restarted while task was in progress") {
		t.Errorf("stream did not end with the task's durable terminal state: %q", body)
	}
}
