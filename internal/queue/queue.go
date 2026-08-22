package queue

import "context"

// Item is a task queued for execution.
type Item struct {
	TaskID     string
	UserID     string
	QueueName  string // named queue to route this task; empty means "default"
	InputType  string // prompt | jira
	InputText  string
	OutputType string // pr | files | print
	OutputDir  string

	// Per-request credentials — never persisted.
	GitHubToken     string
	RepoURL         string
	AtlassianToken  string
	AtlassianDomain string
	AtlassianEmail  string
}

// Delivery is the acknowledgment handle for one popped Item. The item is not
// durably removed from the queue until Ack is called, so callers must only
// Ack after the item's work is fully persisted — Ack before that point risks
// losing the task if the process crashes in between (see NATSQueue.Pop).
type Delivery interface {
	// Ack confirms the item was fully processed and persisted; it will not
	// be redelivered.
	Ack() error
	// Nak signals that processing did not complete (e.g. persistence failed)
	// and the item should be redelivered.
	Nak() error
	// Extend resets the queue's redelivery timer. Call periodically during
	// long-running processing so an active worker doesn't lose the item to
	// redelivery out from under it. No-op for queues with no redelivery timeout.
	Extend() error
}

// Queue is the task submission interface.
// Phase 1: in-memory channel.
// Phase 2+: swap to Redis without changing callers.
type Queue interface {
	Push(ctx context.Context, item Item) error
	Pop(ctx context.Context) (Item, Delivery, error)
	Len() int
}
