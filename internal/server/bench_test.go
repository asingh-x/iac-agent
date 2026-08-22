//go:build integration

package server

// Run with: NATS_URL=nats://localhost:4222 go test -tags=integration -bench=. -run=^$ ./internal/server/...
// (-run=^$ skips every non-benchmark Test function so only the Benchmarks run.)
//
// package server (not server_test): the control-relay benchmark reuses
// fakeLocalHandler and natsConnForControlTest from control_relay_test.go,
// which are unexported and live in this package.
//
// See docs/BENCHMARKS.md for how to run these and what the numbers mean.

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

// BenchmarkNATSEventRelay_PublishSubscribeRoundTrip measures the latency of
// Hub's cross-pod SSE fan-out mechanism end to end: one connection publishes,
// a second (standing in for a different pod's ServeSSE) receives.
func BenchmarkNATSEventRelay_PublishSubscribeRoundTrip(b *testing.B) {
	url := os.Getenv("NATS_URL")
	if url == "" {
		b.Skip("NATS_URL not set — skipping NATS integration benchmark")
	}

	ncPub, err := nats.Connect(url)
	if err != nil {
		b.Fatalf("nats.Connect (publisher): %v", err)
	}
	defer ncPub.Close()
	ncSub, err := nats.Connect(url)
	if err != nil {
		b.Fatalf("nats.Connect (subscriber): %v", err)
	}
	defer ncSub.Close()

	relayPub := NewNATSEventRelay(ncPub)
	relaySub := NewNATSEventRelay(ncSub)

	taskID := fmt.Sprintf("bench-event-relay-%d", time.Now().UnixNano())
	ch, cancel := relaySub.Subscribe(taskID)
	defer cancel()
	time.Sleep(200 * time.Millisecond) // let the subscription register before the timed loop

	ev := ServerEvent{Type: "text", Text: "bench"}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		relayPub.Publish(taskID, ev)
		select {
		case <-ch:
		case <-time.After(5 * time.Second):
			b.Fatalf("timed out waiting for published event on iteration %d", i)
		}
	}
}

// BenchmarkNATSControlRelay_RequestRoundTrip measures the latency of a
// cross-pod answer/permission/cancel request: one connection requests, a
// second (the "owning pod") satisfies it via its local handler and replies.
func BenchmarkNATSControlRelay_RequestRoundTrip(b *testing.B) {
	url := os.Getenv("NATS_URL")
	if url == "" {
		b.Skip("NATS_URL not set — skipping NATS integration benchmark")
	}

	ncOwner, err := nats.Connect(url)
	if err != nil {
		b.Fatalf("nats.Connect (owner): %v", err)
	}
	defer ncOwner.Close()
	ncRequester, err := nats.Connect(url)
	if err != nil {
		b.Fatalf("nats.Connect (requester): %v", err)
	}
	defer ncRequester.Close()

	taskID := fmt.Sprintf("bench-control-relay-%d", time.Now().UnixNano())
	owner := &fakeLocalHandler{ownsTask: taskID}
	if _, err := NewNATSControlRelay(ncOwner, owner); err != nil {
		b.Fatalf("NewNATSControlRelay (owner): %v", err)
	}
	requesterRelay, err := NewNATSControlRelay(ncRequester, &fakeLocalHandler{ownsTask: "none-of-these"})
	if err != nil {
		b.Fatalf("NewNATSControlRelay (requester): %v", err)
	}
	time.Sleep(200 * time.Millisecond) // let both tf.control.* subscriptions register

	ctx := context.Background()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		reqCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		if err := requesterRelay.RequestAnswer(reqCtx, taskID, "bench-answer"); err != nil {
			cancel()
			b.Fatalf("RequestAnswer failed on iteration %d: %v", i, err)
		}
		cancel()
	}
}
