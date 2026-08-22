package llm

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeProvider is a test double for Provider whose Stream behavior is
// controlled by a function field, so tests can script exact
// success/failure sequences and count how many times the wrapped provider
// was actually invoked.
type fakeProvider struct {
	calls int32
	fn    func(ctx context.Context, req Request) (<-chan Event, error)
}

func (f *fakeProvider) Stream(ctx context.Context, req Request) (<-chan Event, error) {
	atomic.AddInt32(&f.calls, 1)
	return f.fn(ctx, req)
}

func (f *fakeProvider) Name() string { return "fake" }

func (f *fakeProvider) callCount() int { return int(atomic.LoadInt32(&f.calls)) }

func succeed(ctx context.Context, req Request) (<-chan Event, error) {
	ch := make(chan Event)
	close(ch)
	return ch, nil
}

func failWith(err error) func(ctx context.Context, req Request) (<-chan Event, error) {
	return func(ctx context.Context, req Request) (<-chan Event, error) {
		return nil, err
	}
}

// asyncSucceed mimics how the real Anthropic/Bedrock providers report a
// normal completion: Stream returns (ch, nil) and the outcome is only known
// once the channel is drained.
func asyncSucceed(ctx context.Context, req Request) (<-chan Event, error) {
	ch := make(chan Event, 2)
	ch <- Event{Type: EventText, Delta: "hello"}
	ch <- Event{Type: EventStop, StopReason: "end_turn"}
	close(ch)
	return ch, nil
}

// asyncFailWith mimics how the real providers report a failure: Stream
// returns (ch, nil) synchronously, and the failure only appears later as an
// EventError on the channel.
func asyncFailWith(err error) func(ctx context.Context, req Request) (<-chan Event, error) {
	return func(ctx context.Context, req Request) (<-chan Event, error) {
		ch := make(chan Event, 2)
		ch <- Event{Type: EventText, Delta: "partial"}
		ch <- Event{Type: EventError, Err: err}
		close(ch)
		return ch, nil
	}
}

// drain fully consumes ch and returns the events seen, blocking until the
// underlying forwarder goroutine has closed it and (for CircuitBreakerProvider)
// finished recording the outcome — tests must drain before asserting breaker
// state for exactly this reason.
func drain(ch <-chan Event) []Event {
	var evs []Event
	for ev := range ch {
		evs = append(evs, ev)
	}
	return evs
}

func TestCircuitBreaker_ClosedPassesThrough(t *testing.T) {
	fp := &fakeProvider{fn: succeed}
	cb := NewCircuitBreakerProvider(fp)

	for i := 0; i < 10; i++ {
		if _, err := cb.Stream(context.Background(), Request{}); err != nil {
			t.Fatalf("call %d: unexpected error: %v", i, err)
		}
	}
	if fp.callCount() != 10 {
		t.Fatalf("expected 10 calls to wrapped provider, got %d", fp.callCount())
	}
}

func TestCircuitBreaker_OpensAfterConsecutiveFailures(t *testing.T) {
	const threshold = 5
	fp := &fakeProvider{fn: failWith(errors.New("boom"))}
	cb := NewCircuitBreakerProviderWithConfig(fp, threshold, time.Hour) // long cooldown, we won't cross it

	for i := 0; i < threshold; i++ {
		if _, err := cb.Stream(context.Background(), Request{}); err == nil {
			t.Fatalf("call %d: expected failure to propagate", i)
		}
	}
	if fp.callCount() != threshold {
		t.Fatalf("expected %d calls to wrapped provider, got %d", threshold, fp.callCount())
	}

	// Circuit should now be open: the next call must fail fast WITHOUT
	// invoking the wrapped provider.
	_, err := cb.Stream(context.Background(), Request{})
	if err == nil {
		t.Fatal("expected circuit-open error, got nil")
	}
	if fp.callCount() != threshold {
		t.Fatalf("wrapped provider must not be called while circuit is open; call count = %d, want %d", fp.callCount(), threshold)
	}
}

func TestCircuitBreaker_HalfOpenAfterCooldown(t *testing.T) {
	const threshold = 3
	const cooldown = 20 * time.Millisecond
	fp := &fakeProvider{fn: failWith(errors.New("boom"))}
	cb := NewCircuitBreakerProviderWithConfig(fp, threshold, cooldown)

	for i := 0; i < threshold; i++ {
		_, _ = cb.Stream(context.Background(), Request{})
	}
	// Confirm it's open.
	if _, err := cb.Stream(context.Background(), Request{}); err == nil {
		t.Fatal("expected circuit-open error")
	}
	callsWhileOpen := fp.callCount()

	// Cross the cooldown boundary.
	time.Sleep(50 * time.Millisecond)

	fp.fn = succeed
	if _, err := cb.Stream(context.Background(), Request{}); err != nil {
		t.Fatalf("expected half-open trial call to be let through and succeed, got: %v", err)
	}
	if fp.callCount() != callsWhileOpen+1 {
		t.Fatalf("expected exactly one additional (trial) call, got %d more", fp.callCount()-callsWhileOpen)
	}
}

func TestCircuitBreaker_SuccessfulHalfOpenTrialClosesCircuit(t *testing.T) {
	const threshold = 2
	const cooldown = 20 * time.Millisecond
	fp := &fakeProvider{fn: failWith(errors.New("boom"))}
	cb := NewCircuitBreakerProviderWithConfig(fp, threshold, cooldown)

	for i := 0; i < threshold; i++ {
		_, _ = cb.Stream(context.Background(), Request{})
	}
	time.Sleep(50 * time.Millisecond)

	fp.fn = succeed
	ch, err := cb.Stream(context.Background(), Request{})
	if err != nil {
		t.Fatalf("half-open trial: unexpected error: %v", err)
	}
	// A synchronous (nil, nil) success like this still goes through the async
	// channel-wrapping path (see Stream's doc comment) — the breaker only
	// records the trial's outcome once the returned channel is drained, so
	// callers (like this test, and the real internal/agent/loop.go turn
	// loop) MUST drain before relying on the state transition having
	// happened.
	drain(ch)

	// Circuit should now be fully closed again: subsequent calls succeed
	// normally, and the failure counter has reset (verified by driving it
	// through threshold-1 failures without opening, then one more success).
	for i := 0; i < threshold-1; i++ {
		fp.fn = failWith(errors.New("boom"))
		if _, err := cb.Stream(context.Background(), Request{}); err == nil {
			t.Fatal("expected the underlying failure to propagate")
		}
	}
	fp.fn = succeed
	if _, err := cb.Stream(context.Background(), Request{}); err != nil {
		t.Fatalf("expected circuit to still be closed (not tripped): %v", err)
	}
}

func TestCircuitBreaker_FailedHalfOpenTrialReopens(t *testing.T) {
	const threshold = 2
	const cooldown = 20 * time.Millisecond
	fp := &fakeProvider{fn: failWith(errors.New("boom"))}
	cb := NewCircuitBreakerProviderWithConfig(fp, threshold, cooldown)

	for i := 0; i < threshold; i++ {
		_, _ = cb.Stream(context.Background(), Request{})
	}
	time.Sleep(50 * time.Millisecond)

	// Half-open trial call also fails.
	callsBeforeTrial := fp.callCount()
	if _, err := cb.Stream(context.Background(), Request{}); err == nil {
		t.Fatal("expected trial failure to propagate")
	}
	if fp.callCount() != callsBeforeTrial+1 {
		t.Fatalf("expected exactly one trial call, got %d more", fp.callCount()-callsBeforeTrial)
	}

	// Circuit should be open again immediately (no second trial without a
	// fresh cooldown).
	callsAfterTrial := fp.callCount()
	if _, err := cb.Stream(context.Background(), Request{}); err == nil {
		t.Fatal("expected fast-fail immediately after a failed half-open trial")
	}
	if fp.callCount() != callsAfterTrial {
		t.Fatalf("wrapped provider must not be called right after a failed trial; call count = %d, want %d", fp.callCount(), callsAfterTrial)
	}

	// And after another cooldown, a new trial is allowed.
	time.Sleep(50 * time.Millisecond)
	fp.fn = succeed
	if _, err := cb.Stream(context.Background(), Request{}); err != nil {
		t.Fatalf("expected a fresh half-open trial after the second cooldown: %v", err)
	}
}

func TestCircuitBreaker_ContextCancellationNotCountedAsFailure(t *testing.T) {
	const threshold = 3
	fp := &fakeProvider{fn: failWith(context.Canceled)}
	cb := NewCircuitBreakerProviderWithConfig(fp, threshold, time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // caller already gave up

	// Drive many more calls than the failure threshold — none of them
	// should count, so the circuit must stay closed the whole time.
	for i := 0; i < threshold*5; i++ {
		if _, err := cb.Stream(ctx, Request{}); !errors.Is(err, context.Canceled) {
			t.Fatalf("call %d: expected context.Canceled to propagate, got: %v", i, err)
		}
	}

	// Circuit must still be closed: a normal successful call goes through
	// to the wrapped provider (not fast-failed).
	fp.fn = succeed
	callsBefore := fp.callCount()
	if _, err := cb.Stream(context.Background(), Request{}); err != nil {
		t.Fatalf("expected circuit to still be closed after only cancellations, got: %v", err)
	}
	if fp.callCount() != callsBefore+1 {
		t.Fatal("expected the wrapped provider to actually be invoked (circuit closed)")
	}
}

func TestCircuitBreaker_DeadlineExceededNotCountedAsFailure(t *testing.T) {
	const threshold = 2
	fp := &fakeProvider{fn: failWith(context.DeadlineExceeded)}
	cb := NewCircuitBreakerProviderWithConfig(fp, threshold, time.Hour)

	ctx, cancel := context.WithTimeout(context.Background(), 0)
	defer cancel()
	time.Sleep(time.Millisecond)

	for i := 0; i < threshold*3; i++ {
		_, _ = cb.Stream(ctx, Request{})
	}

	fp.fn = succeed
	callsBefore := fp.callCount()
	if _, err := cb.Stream(context.Background(), Request{}); err != nil {
		t.Fatalf("expected circuit to still be closed: %v", err)
	}
	if fp.callCount() != callsBefore+1 {
		t.Fatal("expected the wrapped provider to actually be invoked (circuit closed)")
	}
}

func TestCircuitBreaker_ConcurrentCallsNoRace(t *testing.T) {
	const threshold = 5
	const cooldown = 10 * time.Millisecond

	var toggle int32
	fp := &fakeProvider{
		fn: func(ctx context.Context, req Request) (<-chan Event, error) {
			// Alternate success/failure to exercise every state
			// transition path under concurrent access.
			n := atomic.AddInt32(&toggle, 1)
			if n%3 == 0 {
				return nil, fmt.Errorf("boom %d", n)
			}
			return succeed(ctx, req)
		},
	}
	cb := NewCircuitBreakerProviderWithConfig(fp, threshold, cooldown)

	var wg sync.WaitGroup
	for g := 0; g < 20; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				_, _ = cb.Stream(context.Background(), Request{})
			}
		}()
	}
	wg.Wait()
}

// --- Async (channel-based) failure detection ---
//
// The real Anthropic/Bedrock providers always return (ch, nil) from Stream
// itself; a failure only shows up later as an EventError on the channel.
// These tests prove the breaker actually detects that shape of failure, not
// just a synchronous error return — which is the gap an initial
// implementation had (it only checked Stream's return error).

func TestCircuitBreaker_DetectsAsyncEventError(t *testing.T) {
	const threshold = 5
	fp := &fakeProvider{fn: asyncFailWith(errors.New("529 overloaded"))}
	cb := NewCircuitBreakerProviderWithConfig(fp, threshold, time.Hour)

	for i := 0; i < threshold; i++ {
		ch, err := cb.Stream(context.Background(), Request{})
		if err != nil {
			t.Fatalf("call %d: Stream should succeed synchronously (async failure), got %v", i, err)
		}
		drain(ch) // must fully drain before the breaker records the outcome
	}

	// The circuit must now be open purely from async EventErrors — the next
	// call must fail fast WITHOUT invoking the wrapped provider.
	if _, err := cb.Stream(context.Background(), Request{}); err == nil {
		t.Fatal("expected circuit-open error after threshold-many async EventErrors")
	}
	if fp.callCount() != threshold {
		t.Fatalf("wrapped provider must not be called while circuit is open; call count = %d, want %d", fp.callCount(), threshold)
	}
}

func TestCircuitBreaker_AsyncEventError_StillForwardsEventsToCaller(t *testing.T) {
	fp := &fakeProvider{fn: asyncFailWith(errors.New("boom"))}
	cb := NewCircuitBreakerProviderWithConfig(fp, 100, time.Hour)

	ch, err := cb.Stream(context.Background(), Request{})
	if err != nil {
		t.Fatalf("unexpected synchronous error: %v", err)
	}
	evs := drain(ch)
	if len(evs) != 2 {
		t.Fatalf("expected both events forwarded to the caller, got %d", len(evs))
	}
	if evs[0].Type != EventText || evs[0].Delta != "partial" {
		t.Errorf("first event not forwarded correctly: %+v", evs[0])
	}
	if evs[1].Type != EventError {
		t.Errorf("second event not forwarded correctly: %+v", evs[1])
	}
}

func TestCircuitBreaker_AsyncSuccess_DoesNotOpenCircuit(t *testing.T) {
	const threshold = 3
	fp := &fakeProvider{fn: asyncSucceed}
	cb := NewCircuitBreakerProviderWithConfig(fp, threshold, time.Hour)

	// Far more calls than the failure threshold — none of them are failures,
	// so the circuit must never open.
	for i := 0; i < threshold*3; i++ {
		ch, err := cb.Stream(context.Background(), Request{})
		if err != nil {
			t.Fatalf("call %d: unexpected error: %v", i, err)
		}
		evs := drain(ch)
		if len(evs) != 2 {
			t.Fatalf("call %d: expected 2 forwarded events, got %d", i, len(evs))
		}
	}
	if fp.callCount() != threshold*3 {
		t.Fatalf("expected every call to reach the wrapped provider (circuit never opened), got %d calls", fp.callCount())
	}
}

func TestCircuitBreaker_AsyncEventError_HalfOpenRecovery(t *testing.T) {
	const threshold = 2
	const cooldown = 20 * time.Millisecond
	fp := &fakeProvider{fn: asyncFailWith(errors.New("boom"))}
	cb := NewCircuitBreakerProviderWithConfig(fp, threshold, cooldown)

	for i := 0; i < threshold; i++ {
		ch, _ := cb.Stream(context.Background(), Request{})
		drain(ch)
	}
	if _, err := cb.Stream(context.Background(), Request{}); err == nil {
		t.Fatal("expected circuit-open error")
	}

	time.Sleep(50 * time.Millisecond) // cross the cooldown boundary

	fp.fn = asyncSucceed
	ch, err := cb.Stream(context.Background(), Request{})
	if err != nil {
		t.Fatalf("half-open trial call should be let through, got error: %v", err)
	}
	drain(ch)

	// Trial succeeded (async, via EventStop with no EventError) — circuit
	// must now be closed again.
	ch2, err := cb.Stream(context.Background(), Request{})
	if err != nil {
		t.Fatalf("expected circuit closed after successful trial, got error: %v", err)
	}
	drain(ch2)
}
