package server

import (
	"encoding/json"
	"fmt"
	"net/http"
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
}

// Hub manages per-task SSE event channels.
type Hub struct {
	mu       sync.RWMutex
	channels map[string]chan ServerEvent
}

func NewHub() *Hub {
	return &Hub{channels: make(map[string]chan ServerEvent)}
}

// Create registers a new buffered channel for taskID.
func (h *Hub) Create(taskID string) chan ServerEvent {
	ch := make(chan ServerEvent, 256)
	h.mu.Lock()
	h.channels[taskID] = ch
	h.mu.Unlock()
	return ch
}

// Publish sends an event to the task's channel (non-blocking; drops if full).
func (h *Hub) Publish(taskID string, ev ServerEvent) {
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

// Close closes and removes the task's channel.
func (h *Hub) Close(taskID string) {
	h.mu.Lock()
	if ch, ok := h.channels[taskID]; ok {
		close(ch)
		delete(h.channels, taskID)
	}
	h.mu.Unlock()
}

// ServeSSE streams events for taskID as server-sent events.
// Blocks until the channel is closed or the client disconnects.
func (h *Hub) ServeSSE(w http.ResponseWriter, r *http.Request, taskID string) {
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

	// Wait for the channel to exist (task may not have started yet).
	deadline := time.Now().Add(10 * time.Second)
	var ch chan ServerEvent
	for time.Now().Before(deadline) {
		h.mu.RLock()
		ch = h.channels[taskID]
		h.mu.RUnlock()
		if ch != nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if ch == nil {
		writeSSEEvent(w, flusher, ServerEvent{Type: "error", Error: "task not found or already completed"})
		return
	}

	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case ev, open := <-ch:
			if !open {
				return
			}
			writeSSEEvent(w, flusher, ev)
			if ev.Type == "done" || ev.Type == "error" {
				return
			}
		case <-ticker.C:
			// Heartbeat keeps the connection alive through proxies.
			fmt.Fprintf(w, ": heartbeat\n\n")
			flusher.Flush()
		}
	}
}

func writeSSEEvent(w http.ResponseWriter, f http.Flusher, ev ServerEvent) {
	data, _ := json.Marshal(ev)
	fmt.Fprintf(w, "data: %s\n\n", data)
	f.Flush()
}
