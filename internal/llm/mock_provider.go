package llm

import (
	"context"
	"time"
)

// MockProvider is a test double for Provider.
// Set Events to the sequence of events to emit.
type MockProvider struct {
	Events   []Event
	Requests []Request
	name     string

	// Delay, if set, makes Stream block for this long (or until ctx is
	// cancelled, whichever comes first) before returning — used to simulate
	// a slow LLM call in tests that exercise timeout/cancellation behavior.
	Delay time.Duration
}

// NewMockProvider creates a MockProvider that will emit the given events on each Stream call.
func NewMockProvider(name string, events []Event) *MockProvider {
	return &MockProvider{name: name, Events: events}
}

// Stream records the request and returns the pre-configured events on a buffered channel.
func (m *MockProvider) Stream(ctx context.Context, req Request) (<-chan Event, error) {
	m.Requests = append(m.Requests, req)

	if m.Delay > 0 {
		select {
		case <-time.After(m.Delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	ch := make(chan Event, len(m.Events))
	for _, e := range m.Events {
		ch <- e
	}
	close(ch)
	return ch, nil
}

// Name returns the provider name.
func (m *MockProvider) Name() string { return m.name }
