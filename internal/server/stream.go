package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// ServerEvent is the SSE payload sent to clients.
type ServerEvent struct {
	Type   string `json:"type"`             // text|tool_start|tool_end|done|error|status|waiting_for_input|permission_request
	Text   string `json:"text,omitempty"`   // for type=text
	Tool   string `json:"tool,omitempty"`   // for type=tool_start|tool_end
	Output string `json:"output,omitempty"` // for type=tool_end
	PRUrl  string `json:"pr_url,omitempty"` // for type=done
	Status string `json:"status,omitempty"` // for type=status
	Error  string `json:"error,omitempty"`  // for type=error
	Seq    int64  `json:"seq,omitempty"`    // persisted sequence number, 0 if not persisted (see EventStore)
}

// Timing knobs for ServeSSE. Vars rather than consts purely so tests can
// shorten them; nothing mutates them at runtime.
var (
	// localChannelWaitTimeout is how long ServeSSE waits to find out whether
	// this pod is the one executing the task before committing to the
	// cross-pod relay instead.
	localChannelWaitTimeout = 10 * time.Second
	// localChannelPollInterval is how often that wait re-checks.
	localChannelPollInterval = 100 * time.Millisecond
	// sseHeartbeatInterval is how often an idle stream emits an SSE comment,
	// to keep proxies from closing the connection.
	sseHeartbeatInterval = 15 * time.Second
	// relayStatusRecheckInterval is how often a relay-only stream re-reads
	// the task's durable row, so it can end even if the terminal event never
	// arrives over the relay (lost message, owning pod died).
	relayStatusRecheckInterval = 30 * time.Second
)

// EventStore persists an ordered event log per task so a reconnecting SSE
// client can replay anything it missed via Last-Event-ID. Mirrors the
// EventRelay/ControlRelay decoupling pattern already used in this package —
// Hub doesn't know about internal/db directly.
type EventStore interface {
	AppendRunEvent(ctx context.Context, taskID string, ev ServerEvent) (seq int64, err error)
	GetRunEventsSince(ctx context.Context, taskID string, sinceSeq int64) ([]ServerEvent, error)
}

// Hub manages per-task SSE event channels.
type Hub struct {
	mu       sync.RWMutex
	channels map[string]chan ServerEvent
	// owned holds the taskIDs this pod is actually executing (see Claim).
	// Channel *existence* is not evidence of that: handleCreateTask creates a
	// channel on whichever pod accepted the POST, which in a multi-pod
	// deployment is frequently not the pod that ends up running the task —
	// and on that pod the channel stays empty forever.
	owned map[string]bool
	relay EventRelay
	// relayConfigured records whether a real cross-pod relay was installed,
	// so ServeSSE knows whether "not owned here" can mean "owned elsewhere".
	relayConfigured bool
	// statusCheck, if set, reads the task's current durable state (see
	// SetStatusCheck).
	statusCheck func(taskID string) *ServerEvent
	eventStore  EventStore
	logger      *slog.Logger
}

func NewHub() *Hub {
	return &Hub{
		channels: make(map[string]chan ServerEvent),
		owned:    make(map[string]bool),
		relay:    noopEventRelay{},
	}
}

// SetRelay installs a cross-pod event relay (see EventRelay). Call once at
// startup, before any tasks are dispatched — Hub does not guard relay reads
// with the same mutex as channels, since it is set exactly once during wiring.
func (h *Hub) SetRelay(relay EventRelay) {
	h.relay = relay
	h.relayConfigured = true
}

// SetStatusCheck installs a lookup for a task's current durably-persisted
// state, rendered as the SSE event a client would need to see it. ServeSSE
// uses it only on the relay-only path, as a periodic backstop: the relay's
// channel is never closed, so without this a stream for a task whose terminal
// event never made it across (lost NATS message, owning pod died mid-task)
// would sit there emitting heartbeats forever. Call once at startup, same as
// SetRelay. Unset means no backstop, which is the correct behavior for a
// single-process deployment where the local channel is always authoritative.
func (h *Hub) SetStatusCheck(check func(taskID string) *ServerEvent) {
	h.statusCheck = check
}

// SetEventStore installs durable event persistence, enabling Last-Event-ID
// SSE replay in ServeSSE. Call once at startup, same as SetRelay. Unset
// means no persistence — a disconnected client permanently loses events
// published while it was away, the existing behavior.
func (h *Hub) SetEventStore(store EventStore) {
	h.eventStore = store
}

// SetLogger installs a logger for best-effort persistence failures (see
// Publish). Call once at startup, same as SetRelay/SetStatusCheck. Unset
// means persistence failures are silently swallowed — acceptable for tests,
// not for production wiring.
func (h *Hub) SetLogger(logger *slog.Logger) {
	h.logger = logger
}

// Create registers a new buffered channel for taskID.
func (h *Hub) Create(taskID string) chan ServerEvent {
	ch := make(chan ServerEvent, 256)
	h.mu.Lock()
	h.channels[taskID] = ch
	h.mu.Unlock()
	return ch
}

// Claim marks this pod as the one executing taskID and returns the channel its
// events go to, creating it if the task was submitted on a different pod.
// Runner.run calls this before publishing anything for the task, which is what
// makes the marker safe for ServeSSE to use as the "read the local channel"
// signal: by the time any event exists, ownership is already recorded. An
// existing channel is reused rather than replaced, so the events buffered
// since submission are preserved. Cleared by Close.
func (h *Hub) Claim(taskID string) chan ServerEvent {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.owned[taskID] = true
	if ch, ok := h.channels[taskID]; ok {
		return ch
	}
	ch := make(chan ServerEvent, 256)
	h.channels[taskID] = ch
	return ch
}

// Publish sends an event to the task's local channel (non-blocking; drops if
// full) and broadcasts it to the cross-pod relay, so a client whose SSE
// connection landed on a different pod still sees it.
func (h *Hub) Publish(taskID string, ev ServerEvent) {
	if h.eventStore != nil {
		// Best-effort: a persistence failure must never block live delivery.
		// context.Background() matches this codebase's existing convention
		// (see Runner.persistFinalResult) of using a fresh context for a
		// durable write that must complete independent of the caller's own
		// (possibly-cancelled) task context.
		if seq, err := h.eventStore.AppendRunEvent(context.Background(), taskID, ev); err == nil {
			ev.Seq = seq
		} else if h.logger != nil {
			h.logger.Error("failed to persist run event", "task_id", taskID, "event_type", ev.Type, "err", err)
		}
	}

	h.relay.Publish(taskID, ev)

	h.mu.RLock()
	ch, ok := h.channels[taskID]
	h.mu.RUnlock()
	if !ok {
		return
	}
	select {
	case ch <- ev:
	default:
	}
}

// Close closes and removes the task's channel, and drops this pod's claim on
// the task.
func (h *Hub) Close(taskID string) {
	h.mu.Lock()
	if ch, ok := h.channels[taskID]; ok {
		close(ch)
		delete(h.channels, taskID)
	}
	delete(h.owned, taskID)
	h.mu.Unlock()
}

// ServeSSE streams events for taskID as server-sent events. initial, if
// non-nil, is written before anything else — callers use it to give a client
// an immediate snapshot of the task's current state (e.g. from Postgres) even
// when this pod is not the one running the task.
//
// The stream reads from exactly one of two sources: this pod's local channel
// when this pod is executing the task, or the cross-pod relay otherwise. The
// relay subscription is opened up front, before ownership is known, so that
// nothing published in that window is lost; whichever source loses the race
// is dropped unread, since Hub.Publish writes to both and reading both would
// deliver every event twice.
//
// Blocks until the task reaches a terminal state, the client disconnects, or
// (on the relay path with no relay configured) the "no owning pod" error is
// reported.
func (h *Hub) ServeSSE(w http.ResponseWriter, r *http.Request, taskID string, initial *ServerEvent) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	metricActiveSSEConns.Inc()
	defer metricActiveSSEConns.Dec()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	if initial != nil {
		writeSSEEvent(w, flusher, *initial)
		if initial.Type == "done" || initial.Type == "error" {
			return
		}
	}

	// lastEventID is the sequence number, if any, the client says it already
	// has (from Last-Event-ID). It gates two things below: which backlog gets
	// replayed from the durable store, and which already-seen events get
	// filtered back out of the local channel/relay once live reading resumes
	// (see the filter in the main loop) — a reconnect can land on the same
	// buffered local channel the previous connection never fully drained
	// (e.g. it dropped between reading two buffered Publishes), so without
	// that filter the replay and the leftover buffer would both redeliver the
	// same events. Zero means "no header" and disables both.
	var lastEventID int64
	if lastIDStr := r.Header.Get("Last-Event-ID"); lastIDStr != "" && h.eventStore != nil {
		if lastID, err := strconv.ParseInt(lastIDStr, 10, 64); err == nil {
			lastEventID = lastID
			missed, err := h.eventStore.GetRunEventsSince(r.Context(), taskID, lastID)
			if err == nil {
				for _, ev := range missed {
					writeSSEEvent(w, flusher, ev)
				}
			}
		}
	}

	// Subscribe to the cross-pod relay first, before waiting to find out
	// whether this pod owns the task. NATS core pub/sub has no replay, so
	// anything the owning pod publishes while we are still deciding would be
	// lost forever if the subscription started later. With the default no-op
	// relay this is an already-closed channel, which reproduces the original
	// "task not found" behavior exactly.
	relayCh, relayCancel := h.relay.Subscribe(taskID)
	relayLive := true
	defer func() {
		if relayLive {
			relayCancel()
		}
	}()

	local := h.awaitLocalChannel(r.Context(), taskID, relayCh)

	// Commit to exactly one source. Hub.Publish writes every event to both
	// the local channel and the relay, so reading both would deliver each
	// event twice.
	var source <-chan ServerEvent
	statusCheck := h.statusCheck
	if local != nil {
		// This pod is executing the task (or there is no relay at all, so a
		// local channel is the only possible source). The local channel is
		// the complete record: it exists before the first Publish for this
		// task and buffers everything since. Drop the relay subscription
		// unread.
		relayCancel()
		relayLive = false
		source = local
		// The local channel delivers the task's own terminal event and is
		// closed when it ends, so the durable-state backstop is unnecessary.
		statusCheck = nil
	} else {
		source = relayCh
	}

	heartbeat := time.NewTicker(sseHeartbeatInterval)
	defer heartbeat.Stop()

	// Relay-only path: the relay's channel is never closed, so poll the
	// task's durable state periodically as a backstop against a terminal
	// event that never arrives (lost message, owning pod died). A nil channel
	// blocks forever in select, which disables the case entirely.
	var recheck <-chan time.Time
	if statusCheck != nil {
		recheckTicker := time.NewTicker(relayStatusRecheckInterval)
		defer recheckTicker.Stop()
		recheck = recheckTicker.C
	}

	for {
		select {
		case <-r.Context().Done():
			return
		case ev, open := <-source:
			if !open {
				writeSSEEvent(w, flusher, ServerEvent{Type: "error", Error: "task not found or already completed"})
				return
			}
			// Skip anything the replay above already covered (or the client
			// already had). This is what makes replay safe to combine with a
			// local channel that was never drained by a prior connection: the
			// same already-seen events can otherwise still be sitting in its
			// buffer.
			if ev.Seq != 0 && ev.Seq <= lastEventID {
				continue
			}
			writeSSEEvent(w, flusher, ev)
			if ev.Type == "done" || ev.Type == "error" {
				return
			}
		case <-recheck:
			if ev := statusCheck(taskID); ev != nil && (ev.Type == "done" || ev.Type == "error") {
				writeSSEEvent(w, flusher, *ev)
				return
			}
		case <-heartbeat.C:
			fmt.Fprintf(w, ": heartbeat\n\n")
			flusher.Flush()
		}
	}
}

// awaitLocalChannel waits briefly to find out whether this pod is the one
// executing taskID, in which case its local channel is the authoritative
// source for the stream. It returns nil when the stream should read from the
// cross-pod relay instead.
//
// "A local channel exists" is deliberately not sufficient on its own: the
// channel is created on whichever pod accepted the task submission, which in
// a multi-pod deployment is often not the pod that runs the task — reading it
// there would stream nothing at all, forever. So the local channel is used
// only when this pod claimed the task (Hub.Claim, called by Runner.run before
// its first Publish), or when no relay is configured, in which case a local
// channel is by definition the only source there can be.
//
// relayCh is only inspected with len(), which peeks without consuming.
func (h *Hub) awaitLocalChannel(ctx context.Context, taskID string, relayCh <-chan ServerEvent) chan ServerEvent {
	deadline := time.Now().Add(localChannelWaitTimeout)
	for {
		h.mu.RLock()
		ch, exists := h.channels[taskID]
		owned := h.owned[taskID]
		h.mu.RUnlock()
		if exists && (owned || !h.relayConfigured) {
			return ch
		}

		// Some pod is already publishing for this task, so commit to the
		// relay now instead of stalling the client for the rest of the wait.
		// This is safe regardless of what the ownership check above just
		// read: Publish always writes to the relay before it touches the
		// local channel, so the relay (subscribed before this wait started)
		// is a superset of the local channel from that point on. Committing
		// to it can never lose an event, whether or not this pod ever claims
		// the task.
		if h.relayConfigured && len(relayCh) > 0 {
			return nil
		}

		if !time.Now().Before(deadline) {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(localChannelPollInterval):
		}
	}
}

func writeSSEEvent(w http.ResponseWriter, f http.Flusher, ev ServerEvent) {
	data, _ := json.Marshal(ev)
	if ev.Seq != 0 {
		fmt.Fprintf(w, "id: %d\n", ev.Seq)
	}
	fmt.Fprintf(w, "data: %s\n\n", data)
	f.Flush()
}
