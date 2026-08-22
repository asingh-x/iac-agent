//go:build integration

package queue_test

// Run with: NATS_URL=nats://localhost:4222 go test -tags=integration ./internal/queue/... -run Chaos
//
// This proves the actual "pod crash mid-task" recovery story for
// queue_driver=nats: a real killed process never gets to call Ack or Nak —
// it just stops existing. That has to be indistinguishable, from the
// queue's point of view, from a delivery nobody ever touches again. This
// test abandons a popped delivery outright (no Ack, no Nak) and confirms
// NATS's own AckWait-based redelivery hands the same item to a second,
// wholly independent consumer ("pod B") once the wait elapses — the
// mechanism task 7A's fix (main.go's shouldMarkStaleTasksFailed) relies on
// to make skipping the stale-task DB cleanup under queue_driver=nats safe.

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tf-agent/tf-agent/internal/queue"
)

// chaosAckWait is short enough to keep this test fast, but well clear of
// normal NATS round-trip latency so the redelivery isn't a timing coincidence.
const chaosAckWait = 2 * time.Second

// chaosQueueName includes a nanosecond timestamp, not just the sanitized
// test name, because this test intentionally leaves its durable JetStream
// consumer's lifecycle to chance (see podA's comment below): a name reused
// across runs would let one run's leftover server-side consumer/stream state
// (e.g. whether it happened to get deleted by a prior run's cleanup) leak
// into the next run's timing, defeating the point of proving AckWait-based
// redelivery deterministically.
func chaosQueueName(t *testing.T) string {
	t.Helper()
	safe := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' {
			return r
		}
		return '-'
	}, t.Name())
	return fmt.Sprintf("chaos-%s-%d", safe, time.Now().UnixNano())
}

// newChaosPod returns an independent NATSQueue "pod" — its own NATS
// connection — bound to the shared durable pull consumer for name. Every pod
// sharing a queue name binds to the same durable JetStream consumer for it,
// exactly as multiple real server replicas do in production
// (cmd/server/main.go calls queue.NewNATSQueue with the same name from every
// pod), so two separate calls with the same name here really do model two
// separate pods pulling from one shared queue.
func newChaosPod(t *testing.T, url, name string) *queue.NATSQueue {
	t.Helper()
	q, err := queue.NewNATSQueueWithAckWait(url, name, 0, chaosAckWait)
	if err != nil {
		t.Fatalf("NewNATSQueueWithAckWait: %v", err)
	}
	return q
}

func TestChaos_AbandonedDeliveryRedeliversToADifferentConsumer(t *testing.T) {
	url := os.Getenv("NATS_URL")
	if url == "" {
		t.Skip("NATS_URL not set — skipping NATS integration tests")
	}
	name := chaosQueueName(t)

	podA := newChaosPod(t, url, name)
	// podA is deliberately NOT closed until the very end (t.Cleanup, after
	// every assertion below): its subscription created the durable consumer
	// (NATS jetstream semantics: whichever caller creates a durable consumer
	// deletes it on Unsubscribe/Close), so closing it mid-test could tear
	// down the very consumer pod B needs to redeliver from. A real crash
	// wouldn't get a clean Close call anyway.
	t.Cleanup(func() { _ = podA.Close() })

	item := queue.Item{
		TaskID:     "chaos-task-1",
		UserID:     "user-1",
		InputType:  "prompt",
		InputText:  "create an S3 bucket",
		OutputType: "print",
	}
	if err := podA.Push(context.Background(), item); err != nil {
		t.Fatalf("Push: %v", err)
	}

	popCtx, popCancel := context.WithTimeout(context.Background(), 5*time.Second)
	got, delivery, err := podA.Pop(popCtx)
	popCancel()
	if err != nil {
		t.Fatalf("Pop (pod A): %v", err)
	}
	if got.TaskID != item.TaskID {
		t.Fatalf("pod A got TaskID %q, want %q", got.TaskID, item.TaskID)
	}

	// Simulate pod A crashing here: no Ack, no Nak, nothing — a killed
	// process never gets the chance to call either. delivery is captured
	// only so this intent (and the absence of any call on it) is explicit in
	// the test, not because anything below uses it.
	_ = delivery

	// Once AckWait elapses, JetStream must consider the delivery abandoned
	// and hand it to the next puller — a second, independent pod, on its own
	// connection, which never saw the first delivery at all.
	podB := newChaosPod(t, url, name)
	t.Cleanup(func() { _ = podB.Close() })

	redeliverCtx, redeliverCancel := context.WithTimeout(context.Background(), chaosAckWait+15*time.Second)
	defer redeliverCancel()
	redelivered, redelivery, err := podB.Pop(redeliverCtx)
	if err != nil {
		t.Fatalf("Pop (pod B, waiting for redelivery after AckWait elapses): %v", err)
	}
	if redelivered.TaskID != item.TaskID {
		t.Fatalf("pod B got TaskID %q, want %q (the redelivered item pod A abandoned)", redelivered.TaskID, item.TaskID)
	}
	if redelivered.InputText != item.InputText {
		t.Errorf("pod B got InputText %q, want %q", redelivered.InputText, item.InputText)
	}

	// Pod B completing the item (Ack) is the other half of the recovery
	// story: the durable queue mechanism works end to end, not just "the
	// message reappeared".
	if err := redelivery.Ack(); err != nil {
		t.Fatalf("Ack (pod B): %v", err)
	}
}

// TestChaos_GracefulCloseByConsumerCreatorDoesNotDeleteSharedConsumer pins the
// fix for a real bug found while writing the test above: nats.go's
// *Subscription.Unsubscribe() auto-deletes a durable JetStream consumer if
// this subscription is the one that created it (see its own doc comment in
// nats.go@v1.50.0's nats.go:5050-5053). Every pod that binds to the same
// queue name shares one durable consumer — that's how work fans out across
// replicas — so whichever pod happened to be first to start against a given
// name "owns" it in the library's eyes. If that pod's Close() called
// Unsubscribe() (as it used to), its later *graceful* shutdown would delete
// the consumer out from under every other pod still using it, and a freshly
// recreated consumer forgets in-flight delivery state — handing out an
// already-checked-out message to a second pod immediately, instead of
// waiting out AckWait. NATSQueue.Close() no longer calls Unsubscribe() for
// exactly this reason; this test proves an in-flight (popped, not yet
// acked) item survives the creator's graceful Close() untouched.
func TestChaos_GracefulCloseByConsumerCreatorDoesNotDeleteSharedConsumer(t *testing.T) {
	url := os.Getenv("NATS_URL")
	if url == "" {
		t.Skip("NATS_URL not set — skipping NATS integration tests")
	}
	name := chaosQueueName(t)

	// Long enough to give a clear, unambiguous window: if the item comes
	// back well before this elapses, the consumer's delivery tracking was
	// reset (the bug); if it never comes back until this elapses, the fix
	// held.
	const graceAckWait = 8 * time.Second

	podA, err := queue.NewNATSQueueWithAckWait(url, name, 0, graceAckWait)
	if err != nil {
		t.Fatalf("NewNATSQueueWithAckWait (pod A): %v", err)
	}
	// podA creates the durable consumer (first PullSubscribe against a
	// brand-new name) — it is deliberately closed mid-test, not via
	// t.Cleanup, since the Close() call under test is the point.

	item := queue.Item{
		TaskID:     "close-safety-task-1",
		UserID:     "user-1",
		InputType:  "prompt",
		InputText:  "create an S3 bucket",
		OutputType: "print",
	}
	if err := podA.Push(context.Background(), item); err != nil {
		t.Fatalf("Push: %v", err)
	}

	popCtx, popCancel := context.WithTimeout(context.Background(), 5*time.Second)
	_, _, err = podA.Pop(popCtx) // leave it checked out, unacked: genuinely "in flight"
	popCancel()
	if err != nil {
		t.Fatalf("Pop (pod A): %v", err)
	}

	// Simulate a graceful shutdown of the consumer-creating pod while its own
	// delivery is still outstanding.
	if err := podA.Close(); err != nil {
		t.Fatalf("Close (pod A): %v", err)
	}

	// A third pod, bound to the same durable name, must NOT receive the
	// still-in-flight item within a window well short of AckWait.
	podC, err := queue.NewNATSQueueWithAckWait(url, name, 0, graceAckWait)
	if err != nil {
		t.Fatalf("NewNATSQueueWithAckWait (pod C): %v", err)
	}
	t.Cleanup(func() { _ = podC.Close() })

	earlyCtx, earlyCancel := context.WithTimeout(context.Background(), 2*time.Second)
	_, _, err = podC.Pop(earlyCtx)
	earlyCancel()
	if err == nil {
		t.Fatalf("pod C received the item within 2s of pod A's graceful Close(), well before the %s AckWait — pod A's Close() deleted the shared durable consumer and reset its delivery tracking (the bug this fix addresses)", graceAckWait)
	}

	// Once AckWait genuinely elapses, the item must still become available —
	// proving the consumer (and its delivery tracking) survived intact, not
	// that nothing is ever redelivered at all.
	lateCtx, lateCancel := context.WithTimeout(context.Background(), graceAckWait+10*time.Second)
	defer lateCancel()
	redelivered, redelivery, err := podC.Pop(lateCtx)
	if err != nil {
		t.Fatalf("Pop (pod C, after AckWait): %v", err)
	}
	if redelivered.TaskID != item.TaskID {
		t.Fatalf("pod C got TaskID %q, want %q", redelivered.TaskID, item.TaskID)
	}
	if err := redelivery.Ack(); err != nil {
		t.Fatalf("Ack (pod C): %v", err)
	}
}
