package server

// Tests for Hub.ServeSSE's source selection. In-package (rather than in
// server_test.go) so they can shorten the timing knobs — a 10s local-ownership
// wait and a 30s relay re-check are not things a unit test should sit through.

import (
	"context"
	"net/http/httptest"
	"strings"
	"sync"
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

// fakeEventStore is a minimal in-memory EventStore test double for exercising
// Last-Event-ID replay without a real Postgres-backed store.
type fakeEventStore struct {
	mu     sync.Mutex
	events map[string][]ServerEvent
	seq    int64
}

func newFakeEventStore() *fakeEventStore {
	return &fakeEventStore{events: make(map[string][]ServerEvent)}
}

func (f *fakeEventStore) AppendRunEvent(_ context.Context, taskID string, ev ServerEvent) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	ev.Seq = f.seq
	f.events[taskID] = append(f.events[taskID], ev)
	return f.seq, nil
}

func (f *fakeEventStore) GetRunEventsSince(_ context.Context, taskID string, sinceSeq int64) ([]ServerEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []ServerEvent
	for _, ev := range f.events[taskID] {
		if ev.Seq > sinceSeq {
			out = append(out, ev)
		}
	}
	return out, nil
}

func TestHub_ServeSSE_LastEventID_ReplaysMissedEvents(t *testing.T) {
	hub := NewHub()
	store := newFakeEventStore()
	hub.SetEventStore(store)

	taskID := "task-replay"
	hub.Claim(taskID)
	hub.Publish(taskID, ServerEvent{Type: "text", Text: "first"})
	hub.Publish(taskID, ServerEvent{Type: "text", Text: "second"})

	// Simulate a client that already saw "first" (Last-Event-ID: 1) and
	// reconnects expecting to receive "second" first, then live events.
	req := httptest.NewRequest("GET", "/v1/tasks/"+taskID+"/stream", nil)
	req.Header.Set("Last-Event-ID", "1")
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		hub.ServeSSE(rec, req, taskID, nil)
		close(done)
	}()

	// Give ServeSSE a moment to write the replayed backlog, then publish one
	// more live event and close the channel to end the stream.
	time.Sleep(50 * time.Millisecond)
	hub.Publish(taskID, ServerEvent{Type: "done"})

	// Bounded, deliberately: an unbounded `<-done` here would hang the whole
	// package's test binary until the 10-minute go-test panic if ServeSSE ever
	// stops returning — which is exactly the failure mode
	// TestServeSSE_ReplayCoveringTerminalEventReturns below covers, and this
	// test is one event-ordering shift away from tripping over it too (if the
	// terminal `done` above ever landed inside the replay window rather than
	// after it). Fail fast with a useful message instead.
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("ServeSSE did not return after the task's terminal event was published")
	}

	body := rec.Body.String()
	if !strings.Contains(body, "id: 2") || !strings.Contains(body, `"text":"second"`) {
		t.Errorf("expected replayed event 'second' (seq 2) in output, got: %q", body)
	}
	if strings.Contains(body, `"text":"first"`) {
		t.Errorf("must not replay 'first' (seq 1) — client already saw it, got: %q", body)
	}
	// "second" must appear exactly once: the replay from the store sends it,
	// and the live loop reading the leftover local-channel buffer (which
	// still has it queued, since nothing ever consumed it before this
	// connection) must not send it again. A plain strings.Contains check
	// above cannot tell 1 occurrence from N, which is exactly how an earlier
	// version of this fix passed while still double-delivering "second".
	if n := strings.Count(body, `"text":"second"`); n != 1 {
		t.Errorf("event 'second' (seq 2) delivered %d times, want exactly 1 — replay and the live loop both sent it, got: %q", n, body)
	}
}

// serveSSEReconnect runs ServeSSE with a Last-Event-ID header set, i.e. as a
// reconnecting client, and returns the response body. Like serveSSEBody it
// waits only a bounded budget and fails the test if ServeSSE has not returned
// by then — never an unbounded receive, since "ServeSSE never returns" is
// precisely the bug the reconnect tests below exist to catch, and an unbounded
// wait would turn that into a 10-minute hang of the whole test binary rather
// than a failed test.
//
// The request gets a cancellable context that is cancelled on test cleanup, so
// a ServeSSE that did fail to return doesn't linger for the rest of the run.
func serveSSEReconnect(t *testing.T, h *Hub, taskID, lastEventID string, budget time.Duration) (string, time.Duration) {
	t.Helper()

	req := httptest.NewRequest("GET", "/v1/tasks/"+taskID+"/stream", nil)
	ctx, cancel := context.WithCancel(req.Context())
	t.Cleanup(cancel)
	req = req.WithContext(ctx)
	req.Header.Set("Last-Event-ID", lastEventID)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	start := time.Now()
	go func() {
		defer close(done)
		h.ServeSSE(rec, req, taskID, nil)
	}()

	select {
	case <-done:
	case <-time.After(budget):
		t.Fatalf("ServeSSE did not return within %v — the replayed terminal event did not end the stream", budget)
	}
	return rec.Body.String(), time.Since(start)
}

// TestServeSSE_ReplayCoveringTerminalEventReturns is the regression test for a
// Critical bug: a Last-Event-ID replay whose backlog *includes* the task's
// terminal event left ServeSSE running forever.
//
// How it happens: the client's GET /v1/tasks/{id} reports a non-terminal
// status (so `initial` is non-terminal and the `initial` block's own terminal
// check doesn't fire), the task then finishes in the narrow window before
// ServeSSE's replay query runs, and so the durable backlog handed back by
// GetRunEventsSince ends with the `done`/`error`. The replay loop wrote it out
// but did not stop; and because the dedup filter suppresses every event at or
// below replayThreshold, the very same terminal event still queued in the
// local channel / relay buffer was dropped as a duplicate — so the live loop's
// own `if ev.Type == "done" || ev.Type == "error" { return }` was never
// reached. The stream then sat in the heartbeat loop, leaking a goroutine and
// a connection for every such reconnect.
//
// Both subtests fail (by exhausting serveSSEReconnect's budget) against the
// code before the fix, and pass after it.
func TestServeSSE_ReplayCoveringTerminalEventReturns(t *testing.T) {
	// The cross-pod case, and the permanent one: the task ran and finished on
	// another pod, so this pod has no local channel for it at all, and the
	// relay's channel is never closed. With no durable-state backstop
	// installed there is nothing left that could ever end this stream.
	t.Run("relay path with no durable-state backstop", func(t *testing.T) {
		shortenStreamTimings(t)

		taskID := "finished-on-another-pod"
		h := NewHub()
		h.SetRelay(newStubRelay(4)) // nothing on it: the task is already over
		store := newFakeEventStore()
		h.SetEventStore(store)

		// Seed the durable log directly (not via Publish), since the pod that
		// produced these events is not this one.
		for _, ev := range []ServerEvent{
			{Type: "status", Status: "running"},
			{Type: "text", Text: "applying"},
			{Type: "done", PRUrl: "https://example.com/pr/42"},
		} {
			if _, err := store.AppendRunEvent(context.Background(), taskID, ev); err != nil {
				t.Fatalf("seeding the event log: %v", err)
			}
		}

		// The client saw seq 1 before dropping; the replay owes it seq 2 and
		// seq 3 — and seq 3 is the terminal event.
		body, _ := serveSSEReconnect(t, h, taskID, "1", 5*time.Second)

		if !strings.Contains(body, `"type":"done"`) {
			t.Errorf("terminal event missing from the replay: %q", body)
		}
		if !strings.Contains(body, "https://example.com/pr/42") {
			t.Errorf("replayed terminal event lost its payload: %q", body)
		}
		if strings.Contains(body, "task not found") {
			t.Errorf("stream ended with the not-found error instead of the replayed terminal event: %q", body)
		}
	})

	// The same-pod case: this pod owns the task and its local channel still
	// holds the buffered events (nothing drained them — the prior connection
	// dropped), including the terminal one, which the dedup filter now
	// suppresses because the replay just sent it.
	t.Run("local channel whose buffer the replay already covered", func(t *testing.T) {
		shortenStreamTimings(t)

		taskID := "finished-here"
		h := NewHub()
		store := newFakeEventStore()
		h.SetEventStore(store)

		h.Claim(taskID)
		h.Publish(taskID, ServerEvent{Type: "text", Text: "applying"})
		h.Publish(taskID, ServerEvent{Type: "done", PRUrl: "https://example.com/pr/43"})
		// Deliberately no h.Close: that models both the window between run()'s
		// terminal Publish and its hub.Close, and the general case of a local
		// channel whose buffered events no connection ever consumed. The
		// stream must end on the replayed terminal event's own merits, not
		// because the channel happened to be closed underneath it.

		body, _ := serveSSEReconnect(t, h, taskID, "1", 5*time.Second)

		if n := strings.Count(body, `"type":"done"`); n != 1 {
			t.Errorf("terminal event delivered %d times, want exactly 1: %q", n, body)
		}
		if strings.Contains(body, `"text":"applying"`) {
			t.Errorf("must not replay seq 1, which the client already had: %q", body)
		}
	})
}

// stalledEventStore models a wedged database: AppendRunEvent never completes
// on its own, it only unblocks when the context it was given is done.
type stalledEventStore struct {
	entered chan struct{} // one send per AppendRunEvent call
}

func (s *stalledEventStore) AppendRunEvent(ctx context.Context, _ string, _ ServerEvent) (int64, error) {
	select {
	case s.entered <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return 0, ctx.Err()
}

func (s *stalledEventStore) GetRunEventsSince(context.Context, string, int64) ([]ServerEvent, error) {
	return nil, nil
}

// TestHub_Publish_PersistenceIsBounded proves a stalled event store cannot
// wedge Hub.Publish. That write is synchronous, happens for every streamed
// text delta, and runs on the task's own event-loop goroutine — with the
// original bare context.Background() a database that stopped responding
// blocked the task's event loop indefinitely (which also meant the task could
// never reach a pause and give its concurrency slot back). The write is now
// bounded by publishPersistTimeout, after which it is abandoned and logged and
// the event is still delivered live, un-replayable but not lost.
func TestHub_Publish_PersistenceIsBounded(t *testing.T) {
	restore := publishPersistTimeout
	publishPersistTimeout = 100 * time.Millisecond
	t.Cleanup(func() { publishPersistTimeout = restore })

	store := &stalledEventStore{entered: make(chan struct{}, 1)}
	h := NewHub()
	h.SetEventStore(store)
	ch := h.Claim("wedged-db")

	published := make(chan struct{})
	go func() {
		defer close(published)
		h.Publish("wedged-db", ServerEvent{Type: "text", Text: "delta"})
	}()

	select {
	case <-store.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("Publish never reached the event store")
	}

	select {
	case <-published:
	case <-time.After(2 * time.Second):
		t.Fatal("Publish did not return after the persistence deadline — a stalled store still wedges the task's event loop")
	}

	// Live delivery must survive the abandoned write, with no Seq (so the
	// replay dedup filter, which ignores Seq 0, can't suppress it either).
	select {
	case ev := <-ch:
		if ev.Text != "delta" {
			t.Errorf("delivered event = %+v, want the published text delta", ev)
		}
		if ev.Seq != 0 {
			t.Errorf("Seq = %d, want 0 for an event whose persistence failed", ev.Seq)
		}
	default:
		t.Error("event was not delivered to the local channel after the persistence timeout")
	}
}
