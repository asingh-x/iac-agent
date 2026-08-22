package server

import (
	"encoding/json"

	"github.com/nats-io/nats.go"
)

// EventRelay lets Hub broadcast SSE events across pods, and lets ServeSSE
// pick up events for a task that a different pod is actually running.
type EventRelay interface {
	// Publish broadcasts ev for taskID to every pod (including this one's
	// own future Subscribe calls, but Hub only uses this for cross-pod
	// fan-out — its own local channel already has the event).
	Publish(taskID string, ev ServerEvent)
	// Subscribe returns a channel of events published for taskID by any pod
	// and a cancel func to release the subscription. The channel is never
	// closed on a live subscription — call cancel when done with it.
	Subscribe(taskID string) (<-chan ServerEvent, func())
}

// noopEventRelay is the default: no cross-pod transport configured (i.e. a
// single-process / memory-queue deployment). Subscribe returns an
// already-closed channel so a Hub falling back to it reproduces exactly the
// pre-existing "task not found" behavior.
type noopEventRelay struct{}

func (noopEventRelay) Publish(string, ServerEvent) {}

func (noopEventRelay) Subscribe(string) (<-chan ServerEvent, func()) {
	ch := make(chan ServerEvent)
	close(ch)
	return ch, func() {}
}

func eventSubject(taskID string) string { return "tf.events." + taskID }

// NATSEventRelay fans SSE events out across pods over NATS core pub/sub.
type NATSEventRelay struct {
	nc *nats.Conn
}

// NewNATSEventRelay wraps an already-connected core NATS connection.
func NewNATSEventRelay(nc *nats.Conn) *NATSEventRelay {
	return &NATSEventRelay{nc: nc}
}

func (r *NATSEventRelay) Publish(taskID string, ev ServerEvent) {
	data, err := json.Marshal(ev)
	if err != nil {
		return
	}
	_ = r.nc.Publish(eventSubject(taskID), data)
}

func (r *NATSEventRelay) Subscribe(taskID string) (<-chan ServerEvent, func()) {
	out := make(chan ServerEvent, 256)
	sub, err := r.nc.Subscribe(eventSubject(taskID), func(msg *nats.Msg) {
		var ev ServerEvent
		if json.Unmarshal(msg.Data, &ev) != nil {
			return
		}
		select {
		case out <- ev:
		default:
			// Slow consumer: drop rather than block the NATS dispatch
			// goroutine, matching Hub.Publish's own drop-if-full behavior.
		}
	})
	if err != nil {
		close(out)
		return out, func() {}
	}
	return out, func() { _ = sub.Unsubscribe() }
}
