//go:build integration

package server_test

// Run with: NATS_URL=nats://localhost:4222 go test -tags=integration ./internal/server/... -run EventRelay

import (
	"os"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/tf-agent/tf-agent/internal/server"
)

func natsConnForTest(t *testing.T) *nats.Conn {
	t.Helper()
	url := os.Getenv("NATS_URL")
	if url == "" {
		t.Skip("NATS_URL not set — skipping NATS integration tests")
	}
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("nats.Connect: %v", err)
	}
	t.Cleanup(nc.Close)
	return nc
}

func TestNATSEventRelay_PublishSubscribeAcrossConnections(t *testing.T) {
	// Two separate connections stand in for two separate pods.
	ncA := natsConnForTest(t)
	ncB := natsConnForTest(t)
	relayA := server.NewNATSEventRelay(ncA)
	relayB := server.NewNATSEventRelay(ncB)

	ch, cancel := relayB.Subscribe("task-xyz")
	defer cancel()

	// Give the subscription time to actually register before publishing.
	time.Sleep(100 * time.Millisecond)
	relayA.Publish("task-xyz", server.ServerEvent{Type: "text", Text: "hello from pod A"})

	select {
	case ev := <-ch:
		if ev.Type != "text" || ev.Text != "hello from pod A" {
			t.Errorf("got %+v, want Type=text Text=%q", ev, "hello from pod A")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for cross-connection event")
	}
}

func TestNATSEventRelay_SubscribeIsIsolatedPerTask(t *testing.T) {
	nc := natsConnForTest(t)
	relay := server.NewNATSEventRelay(nc)

	chA, cancelA := relay.Subscribe("task-a")
	defer cancelA()
	chB, cancelB := relay.Subscribe("task-b")
	defer cancelB()

	time.Sleep(100 * time.Millisecond)
	relay.Publish("task-a", server.ServerEvent{Type: "text", Text: "for a"})

	select {
	case ev := <-chA:
		if ev.Text != "for a" {
			t.Errorf("chA got %+v", ev)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting on chA")
	}

	select {
	case ev := <-chB:
		t.Fatalf("chB should not have received an event for task-a, got %+v", ev)
	case <-time.After(300 * time.Millisecond):
		// expected: nothing arrives on the wrong task's channel
	}
}
