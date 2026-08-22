//go:build integration

package server

// Run with: NATS_URL=nats://localhost:4222 go test -tags=integration ./internal/server/... -run ControlRelay
//
// package server (not server_test): this test exercises NATSControlRelay
// against a fake ControlLocalHandler directly, including the unexported
// interface, without needing a full Runner/task lifecycle.

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

type fakeLocalHandler struct {
	ownsTask  string // taskID this fake "owns"; requests for any other taskID are refused
	gotAnswer string
	gotAllow  bool
	cancelled bool
}

func (f *fakeLocalHandler) sendAnswerLocal(taskID, answer string) error {
	if taskID != f.ownsTask {
		return errNotOwned
	}
	f.gotAnswer = answer
	return nil
}

func (f *fakeLocalHandler) sendPermissionResponseLocal(taskID string, allow bool) error {
	if taskID != f.ownsTask {
		return errNotOwned
	}
	f.gotAllow = allow
	return nil
}

func (f *fakeLocalHandler) cancelTaskLocal(taskID string) error {
	if taskID != f.ownsTask {
		return errNotOwned
	}
	f.cancelled = true
	return nil
}

func natsConnForControlTest(t *testing.T) *nats.Conn {
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

func TestNATSControlRelay_RequestReachesOwningPod(t *testing.T) {
	ncOwner := natsConnForControlTest(t)
	ncRequester := natsConnForControlTest(t)

	owner := &fakeLocalHandler{ownsTask: "task-1"}
	if _, err := NewNATSControlRelay(ncOwner, owner); err != nil {
		t.Fatalf("NewNATSControlRelay (owner): %v", err)
	}

	requesterRelay, err := NewNATSControlRelay(ncRequester, &fakeLocalHandler{ownsTask: "none-of-these"})
	if err != nil {
		t.Fatalf("NewNATSControlRelay (requester): %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := requesterRelay.RequestAnswer(ctx, "task-1", "42"); err != nil {
		t.Fatalf("RequestAnswer: %v", err)
	}
	if owner.gotAnswer != "42" {
		t.Errorf("owner.gotAnswer = %q, want 42", owner.gotAnswer)
	}
}

func TestNATSControlRelay_NoOwnerTimesOut(t *testing.T) {
	nc := natsConnForControlTest(t)
	relay, err := NewNATSControlRelay(nc, &fakeLocalHandler{ownsTask: "some-other-task"})
	if err != nil {
		t.Fatalf("NewNATSControlRelay: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	if err := relay.RequestCancel(ctx, "task-nobody-owns"); err == nil {
		t.Error("expected an error when no pod owns the task, got nil")
	}
}
