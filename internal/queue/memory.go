package queue

import (
	"context"
	"fmt"
)

// MemoryQueue is an in-memory, channel-backed Queue.
// Safe for Phase 1 (single process). Tasks are lost on restart.
// Phase 2: replace with RedisQueue implementing the same interface.
type MemoryQueue struct {
	ch chan Item
}

// NewMemoryQueue creates a queue with the given buffer size.
func NewMemoryQueue(bufferSize int) *MemoryQueue {
	return &MemoryQueue{ch: make(chan Item, bufferSize)}
}

func (q *MemoryQueue) Push(ctx context.Context, item Item) error {
	select {
	case q.ch <- item:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	default:
		return fmt.Errorf("queue full (capacity %d)", cap(q.ch))
	}
}

// noopDelivery is used by MemoryQueue: an in-memory channel has no
// redelivery mechanism, so acknowledgment is a no-op.
type noopDelivery struct{}

func (noopDelivery) Ack() error    { return nil }
func (noopDelivery) Nak() error    { return nil }
func (noopDelivery) Extend() error { return nil }

func (q *MemoryQueue) Pop(ctx context.Context) (Item, Delivery, error) {
	select {
	case item := <-q.ch:
		return item, noopDelivery{}, nil
	case <-ctx.Done():
		return Item{}, nil, ctx.Err()
	}
}

func (q *MemoryQueue) Len() int { return len(q.ch) }
