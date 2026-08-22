package server

// Concurrency/load tests for the pieces task 7 (multi-replica safety) touched:
// Runner's answer/permission/cancel maps and Hub's channel/ownership maps.
// These intentionally do not assert every individual outcome — the point,
// per the resilience-hardening brief, is confidence that the concurrency
// primitives (sync.RWMutex on Hub.channels/owned, the per-task maps on
// Runner) hold up under real contention, not just the light concurrency the
// rest of the unit tests exercise. Run with -race; also run at -count=2 (see
// docs/BENCHMARKS.md) to shake out anything timing-dependent.
//
// In-package (not server_test) so the Runner-side test can seed r.answers/
// r.permissions/r.cancels directly, the same way multireplica_e2e_test.go's
// e2ePod.owns helper does, without needing a full task lifecycle.

import (
	"context"
	"fmt"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fastFakeControlRelay is a ControlRelay double that resolves immediately —
// no blocking on ctx — for use here, where the point is stressing Runner's
// own local maps under concurrent access, not re-proving
// controlRelayTimeout's bounded-wait behavior (see
// TestRunner_RelayFallback_IsBoundedByTimeout in task_runner_test.go for
// that). calls is incremented so the test can confirm the "not locally
// owned" path was actually exercised, not skipped entirely.
type fastFakeControlRelay struct {
	calls int32 // atomic
}

func (f *fastFakeControlRelay) RequestAnswer(context.Context, string, string) error {
	atomic.AddInt32(&f.calls, 1)
	return fmt.Errorf("task not owned by any pod")
}

func (f *fastFakeControlRelay) RequestPermission(context.Context, string, bool) error {
	atomic.AddInt32(&f.calls, 1)
	return fmt.Errorf("task not owned by any pod")
}

func (f *fastFakeControlRelay) RequestCancel(context.Context, string) error {
	atomic.AddInt32(&f.calls, 1)
	return fmt.Errorf("task not owned by any pod")
}

// TestConcurrent_RunnerControlMethods_NoRaceOrPanic hammers
// SendAnswer/SendPermissionResponse/CancelTask concurrently across a mix of
// task IDs this Runner "owns" locally (seeded into answers/permissions/
// cancels, the way run() would) and task IDs it doesn't (falling through to
// the control relay). The assertion that matters is implicit: this test must
// complete without a panic, a deadlock, or (under -race) a data race report.
func TestConcurrent_RunnerControlMethods_NoRaceOrPanic(t *testing.T) {
	relay := &fastFakeControlRelay{}
	r := newRelayTestRunner(relay)

	orig := controlRelayTimeout
	controlRelayTimeout = 200 * time.Millisecond // keep the "not owned" path fast under many concurrent callers
	t.Cleanup(func() { controlRelayTimeout = orig })

	const nOwned = 20
	ownedIDs := make([]string, nOwned)
	for i := range ownedIDs {
		id := fmt.Sprintf("owned-%d", i)
		ownedIDs[i] = id
		r.answers[id] = make(chan string, 8)
		r.permissions[id] = make(chan bool, 8)
		_, cancel := context.WithCancel(context.Background())
		r.cancels[id] = cancel
	}

	const nUnowned = 20
	unownedIDs := make([]string, nUnowned)
	for i := range unownedIDs {
		unownedIDs[i] = fmt.Sprintf("unowned-%d", i)
	}

	allIDs := append(append([]string{}, ownedIDs...), unownedIDs...)

	const nWorkers = 100
	var wg sync.WaitGroup
	for i := 0; i < nWorkers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			taskID := allIDs[i%len(allIDs)]
			switch i % 3 {
			case 0:
				_ = r.SendAnswer(taskID, "answer")
			case 1:
				_ = r.SendPermissionResponse(taskID, i%2 == 0)
			case 2:
				_ = r.CancelTask(taskID)
			}
		}(i)
	}
	wg.Wait()

	if atomic.LoadInt32(&relay.calls) == 0 {
		t.Error("relay was never invoked — the not-locally-owned task IDs should have fallen through to it")
	}
}

// fakeEventRelay is a minimal thread-safe in-process EventRelay double for
// stressing Hub's own concurrency primitives without needing a real NATS
// server. Unlike stream_internal_test.go's stubRelay (one shared channel
// regardless of taskID, fine for its single-task tests), this keys
// subscriptions per taskID, since this test hammers many task IDs at once.
type fakeEventRelay struct {
	mu   sync.Mutex
	subs map[string][]chan ServerEvent
}

func newFakeEventRelay() *fakeEventRelay {
	return &fakeEventRelay{subs: make(map[string][]chan ServerEvent)}
}

func (f *fakeEventRelay) Publish(taskID string, ev ServerEvent) {
	f.mu.Lock()
	chans := append([]chan ServerEvent{}, f.subs[taskID]...)
	f.mu.Unlock()
	for _, ch := range chans {
		select {
		case ch <- ev:
		default:
		}
	}
}

func (f *fakeEventRelay) Subscribe(taskID string) (<-chan ServerEvent, func()) {
	ch := make(chan ServerEvent, 64)
	f.mu.Lock()
	f.subs[taskID] = append(f.subs[taskID], ch)
	f.mu.Unlock()
	cancel := func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		list := f.subs[taskID]
		for i, c := range list {
			if c == ch {
				f.subs[taskID] = append(list[:i:i], list[i+1:]...)
				break
			}
		}
	}
	return ch, cancel
}

// TestConcurrent_HubServeSSE_NoRaceOrPanic opens and closes many concurrent
// SSE connections (via httptest) for a mix of claimed, unclaimed and
// nonexistent task IDs, while other goroutines concurrently Publish/Claim/
// Close the same task IDs. As with the Runner test above, the point is that
// Hub's sync.RWMutex-protected channels/owned maps hold up under real
// contention — run with -race.
func TestConcurrent_HubServeSSE_NoRaceOrPanic(t *testing.T) {
	origWait, origPoll, origRecheck := localChannelWaitTimeout, localChannelPollInterval, relayStatusRecheckInterval
	localChannelWaitTimeout = 150 * time.Millisecond
	localChannelPollInterval = 10 * time.Millisecond
	relayStatusRecheckInterval = 50 * time.Millisecond
	t.Cleanup(func() {
		localChannelWaitTimeout, localChannelPollInterval, relayStatusRecheckInterval = origWait, origPoll, origRecheck
	})

	hub := NewHub()
	hub.SetRelay(newFakeEventRelay())
	hub.SetStatusCheck(func(string) *ServerEvent { return nil }) // no durable backstop needed for this test

	const nClaimed, nUnclaimed, nNonexistent = 10, 10, 10

	var allIDs []string
	for i := 0; i < nClaimed; i++ {
		id := fmt.Sprintf("claimed-%d", i)
		hub.Claim(id) // "running" here — the local channel is authoritative
		allIDs = append(allIDs, id)
	}
	for i := 0; i < nUnclaimed; i++ {
		id := fmt.Sprintf("unclaimed-%d", i)
		hub.Create(id) // submitted here, but (as far as this pod knows) running elsewhere
		allIDs = append(allIDs, id)
	}
	for i := 0; i < nNonexistent; i++ {
		allIDs = append(allIDs, fmt.Sprintf("nonexistent-%d", i))
	}

	var wg sync.WaitGroup

	const nStreamers = 60
	for i := 0; i < nStreamers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			taskID := allIDs[i%len(allIDs)]
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()
			req := httptest.NewRequest("GET", "/v1/tasks/"+taskID+"/stream", nil).WithContext(ctx)
			rec := httptest.NewRecorder()
			hub.ServeSSE(rec, req, taskID, nil)
		}(i)
	}

	const nMutators = 60
	for i := 0; i < nMutators; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			taskID := allIDs[i%len(allIDs)]
			switch i % 3 {
			case 0:
				hub.Publish(taskID, ServerEvent{Type: "text", Text: "hammer"})
			case 1:
				hub.Claim(taskID)
			case 2:
				hub.Close(taskID)
			}
		}(i)
	}

	wg.Wait()
}
