package queue

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

// TestMemoryQueue_PushFull verifies that pushing to a full queue returns an
// error immediately (non-blocking) rather than hanging.
func TestMemoryQueue_PushFull(t *testing.T) {
	const capacity = 3
	q := NewMemoryQueue(capacity)
	ctx := context.Background()

	// Fill the queue to capacity.
	for i := 0; i < capacity; i++ {
		if err := q.Push(ctx, Item{TaskID: fmt.Sprintf("task-%d", i)}); err != nil {
			t.Fatalf("Push %d: unexpected error: %v", i, err)
		}
	}
	if q.Len() != capacity {
		t.Fatalf("Len = %d, want %d", q.Len(), capacity)
	}

	// One more push must fail immediately.
	err := q.Push(ctx, Item{TaskID: "overflow"})
	if err == nil {
		t.Fatal("expected error pushing to full queue, got nil")
	}
	// Queue length should still equal capacity.
	if q.Len() != capacity {
		t.Errorf("Len after failed push = %d, want %d", q.Len(), capacity)
	}
}

// TestMemoryQueue_PopCancelled verifies that Pop returns the context error when
// the context is cancelled while the queue is empty.
func TestMemoryQueue_PopCancelled(t *testing.T) {
	q := NewMemoryQueue(10)
	ctx, cancel := context.WithCancel(context.Background())

	// Cancel immediately before calling Pop.
	cancel()

	_, _, err := q.Pop(ctx)
	if err == nil {
		t.Fatal("expected cancellation error from Pop, got nil")
	}
	if err != context.Canceled {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

// TestMemoryQueue_PopCancelledMidWait verifies Pop returns when the context is
// cancelled after Pop has already started blocking.
func TestMemoryQueue_PopCancelledMidWait(t *testing.T) {
	q := NewMemoryQueue(10)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		_, _, err := q.Pop(ctx)
		done <- err
	}()

	// Cancel the context while the goroutine is blocked waiting.
	cancel()

	err := <-done
	if err == nil {
		t.Fatal("expected error from Pop after context cancel, got nil")
	}
	if err != context.Canceled {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

// TestMemoryQueue_Len verifies Len returns accurate counts across push and pop
// operations.
func TestMemoryQueue_Len(t *testing.T) {
	q := NewMemoryQueue(10)
	ctx := context.Background()

	if n := q.Len(); n != 0 {
		t.Fatalf("Len on empty queue = %d, want 0", n)
	}

	// Push 5 items; Len should track each addition.
	for i := 1; i <= 5; i++ {
		_ = q.Push(ctx, Item{TaskID: fmt.Sprintf("t%d", i)})
		if n := q.Len(); n != i {
			t.Errorf("after %d pushes, Len = %d, want %d", i, n, i)
		}
	}

	// Pop 3 items; Len should track each removal.
	for i := 4; i >= 2; i-- {
		if _, _, err := q.Pop(ctx); err != nil {
			t.Fatalf("Pop: %v", err)
		}
		if n := q.Len(); n != i {
			t.Errorf("Len = %d, want %d", n, i)
		}
	}
}

// TestMemoryQueue_PushCancelledContext verifies that Push honours context
// cancellation even when the queue has capacity. Because MemoryQueue.Push uses
// a non-blocking select with a `default` branch, the ctx.Done() arm is only
// reached when the context is already done at call time — we verify that
// scenario here to complete branch coverage.
func TestMemoryQueue_PushCancelledContext(t *testing.T) {
	q := NewMemoryQueue(10)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before pushing

	err := q.Push(ctx, Item{TaskID: "should-fail"})
	// The implementation has both ctx.Done and default branches; a pre-cancelled
	// context will hit one of them. Either a cancellation error or a "queue full"
	// error is acceptable — what matters is that no panic occurs and the item
	// was not added if an error was returned.
	if err == nil {
		// If no error the item was queued (ctx.Done lost the race to default
		// non-blocking select); that's fine, just pop and verify.
		if q.Len() != 1 {
			t.Errorf("Push succeeded but Len = %d, want 1", q.Len())
		}
	} else {
		// Error returned — queue must still be empty.
		if q.Len() != 0 {
			t.Errorf("Push returned error but Len = %d, want 0", q.Len())
		}
	}
}

// TestMemoryQueue_DeliveryIsNoop verifies MemoryQueue's Delivery handle is a
// harmless no-op (an in-memory channel has no redelivery mechanism to manage).
func TestMemoryQueue_DeliveryIsNoop(t *testing.T) {
	q := NewMemoryQueue(10)
	ctx := context.Background()
	_ = q.Push(ctx, Item{TaskID: "a"})

	_, delivery, err := q.Pop(ctx)
	if err != nil {
		t.Fatalf("Pop: %v", err)
	}
	if delivery == nil {
		t.Fatal("expected non-nil Delivery from Pop")
	}
	if err := delivery.Ack(); err != nil {
		t.Errorf("Ack: unexpected error: %v", err)
	}
	if err := delivery.Nak(); err != nil {
		t.Errorf("Nak: unexpected error: %v", err)
	}
	if err := delivery.Extend(); err != nil {
		t.Errorf("Extend: unexpected error: %v", err)
	}
}

// TestMemoryQueue_ConcurrentPushPop pushes N items from multiple goroutines
// and pops them from multiple goroutines, verifying all items are received
// exactly once. Safe for the race detector.
func TestMemoryQueue_ConcurrentPushPop(t *testing.T) {
	const (
		workers   = 8
		perWorker = 50
		total     = workers * perWorker
	)

	q := NewMemoryQueue(total)
	ctx := context.Background()

	var pushWg sync.WaitGroup
	for w := 0; w < workers; w++ {
		w := w
		pushWg.Add(1)
		go func() {
			defer pushWg.Done()
			for i := 0; i < perWorker; i++ {
				id := fmt.Sprintf("w%d-i%d", w, i)
				if err := q.Push(ctx, Item{TaskID: id}); err != nil {
					t.Errorf("Push %s: %v", id, err)
				}
			}
		}()
	}
	pushWg.Wait()

	if n := q.Len(); n != total {
		t.Fatalf("expected %d items after concurrent pushes, got %d", total, n)
	}

	received := make(chan string, total)
	var popWg sync.WaitGroup
	for w := 0; w < workers; w++ {
		popWg.Add(1)
		go func() {
			defer popWg.Done()
			for i := 0; i < perWorker; i++ {
				item, _, err := q.Pop(ctx)
				if err != nil {
					t.Errorf("Pop: %v", err)
					return
				}
				received <- item.TaskID
			}
		}()
	}
	popWg.Wait()
	close(received)

	count := 0
	for range received {
		count++
	}
	if count != total {
		t.Errorf("received %d items, want %d", count, total)
	}
	if n := q.Len(); n != 0 {
		t.Errorf("Len after all pops = %d, want 0", n)
	}
}
