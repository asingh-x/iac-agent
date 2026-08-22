package queue

import (
	"context"
	"testing"
	"time"
)

func TestMemoryQueue_PushPop(t *testing.T) {
	q := NewMemoryQueue(10)
	ctx := context.Background()

	item := Item{TaskID: "task-1", InputType: "prompt", InputText: "hello", OutputType: "print"}
	if err := q.Push(ctx, item); err != nil {
		t.Fatalf("Push: %v", err)
	}
	if q.Len() != 1 {
		t.Errorf("Len = %d, want 1", q.Len())
	}

	got, _, err := q.Pop(ctx)
	if err != nil {
		t.Fatalf("Pop: %v", err)
	}
	if got.TaskID != item.TaskID {
		t.Errorf("TaskID = %q, want %q", got.TaskID, item.TaskID)
	}
	if q.Len() != 0 {
		t.Errorf("Len after pop = %d, want 0", q.Len())
	}
}

func TestMemoryQueue_FIFO(t *testing.T) {
	q := NewMemoryQueue(10)
	ctx := context.Background()

	for i, id := range []string{"first", "second", "third"} {
		_ = q.Push(ctx, Item{TaskID: id, InputText: string(rune('A' + i))})
	}

	for _, want := range []string{"first", "second", "third"} {
		got, _, err := q.Pop(ctx)
		if err != nil {
			t.Fatalf("Pop: %v", err)
		}
		if got.TaskID != want {
			t.Errorf("FIFO order: got %q, want %q", got.TaskID, want)
		}
	}
}

func TestMemoryQueue_Full(t *testing.T) {
	q := NewMemoryQueue(2)
	ctx := context.Background()

	_ = q.Push(ctx, Item{TaskID: "a"})
	_ = q.Push(ctx, Item{TaskID: "b"})

	err := q.Push(ctx, Item{TaskID: "c"})
	if err == nil {
		t.Error("expected error when pushing to a full queue")
	}
}

func TestMemoryQueue_PopCancelledContext(t *testing.T) {
	q := NewMemoryQueue(10)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, _, err := q.Pop(ctx)
	if err == nil {
		t.Error("expected context cancellation error on empty queue pop")
	}
}

func TestMemoryQueue_MultipleItems(t *testing.T) {
	q := NewMemoryQueue(100)
	ctx := context.Background()

	for i := 0; i < 50; i++ {
		_ = q.Push(ctx, Item{TaskID: string(rune('a' + i))})
	}
	if q.Len() != 50 {
		t.Errorf("Len = %d, want 50", q.Len())
	}
}
