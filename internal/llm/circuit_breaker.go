package llm

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Circuit breaker tuning. These are deliberately plain constants rather than
// a new config.Config section: they're an internal resilience knob (how
// aggressively to fail fast), not something an operator needs to tune per
// deployment. If that ever changes, promote them into
// config.ProviderConfig and thread them through NewProvider.
const (
	// defaultFailureThreshold is how many consecutive Stream failures, while
	// the circuit is closed, it takes to open it.
	defaultFailureThreshold = 5
	// defaultCooldownPeriod is how long the circuit stays open before a
	// single half-open trial call is let through to test recovery.
	defaultCooldownPeriod = 30 * time.Second
)

// breakerState is the circuit breaker's state machine.
type breakerState int

const (
	stateClosed   breakerState = iota // normal operation, calls pass through
	stateOpen                         // failing fast, wrapped provider is not called
	stateHalfOpen                     // cooldown elapsed, one trial call in flight
)

// CircuitBreakerProvider wraps any llm.Provider and fails fast once the
// wrapped provider has failed repeatedly in a short window, instead of every
// caller paying the full retry+backoff latency before finding out the
// provider is down. Compare with internal/agent/loop.go's streamWithRetry,
// which only retries *within* a single call — it carries no memory across
// calls, so if the provider is degraded, every single task still pays the
// full retry+backoff cost before failing.
//
// State machine (standard closed/open/half-open):
//   - closed:    normal operation; calls pass through to the wrapped provider.
//     A run of defaultFailureThreshold consecutive failures opens the circuit.
//   - open:      calls fail immediately, without invoking the wrapped
//     provider's Stream at all, until defaultCooldownPeriod has elapsed.
//   - half-open: once the cooldown elapses, exactly one call is let through
//     as a trial. Success closes the circuit and resets the failure count;
//     failure re-opens it for another full cooldown period.
//
// A "failure" is either Stream returning a non-nil error synchronously, or
// (for providers that stream success/failure asynchronously, like Anthropic
// and Bedrock) an EventError appearing anywhere on the returned channel
// before it closes. A context-cancellation error (the caller's own ctx being
// cancelled or timing out) is explicitly excluded either way — that reflects
// the caller giving up, not the provider being degraded — mirroring the
// errors.Is(err, context.Canceled) check in internal/agent/loop.go's
// tool-execution path.
//
// Thread-safe: Stream is called concurrently (bounded by
// config.Server.LLMConcurrency), so all state transitions are guarded by a
// mutex. The wrapped provider's Stream call itself happens outside the lock
// so a slow/blocking provider call cannot stall unrelated goroutines from
// observing or transitioning circuit state.
type CircuitBreakerProvider struct {
	wrapped Provider

	failureThreshold int
	cooldownPeriod   time.Duration

	mu              sync.Mutex
	state           breakerState
	consecutiveFail int
	openedAt        time.Time
}

// NewCircuitBreakerProvider wraps p with a circuit breaker using the default
// thresholds: opens after 5 consecutive failures, stays open for 30 seconds
// before allowing a half-open trial call.
func NewCircuitBreakerProvider(p Provider) *CircuitBreakerProvider {
	return NewCircuitBreakerProviderWithConfig(p, defaultFailureThreshold, defaultCooldownPeriod)
}

// NewCircuitBreakerProviderWithConfig wraps p with an explicit failure
// threshold and cooldown period. Exported primarily so tests can shrink the
// cooldown to a few milliseconds instead of sleeping for the real 30s
// default.
func NewCircuitBreakerProviderWithConfig(p Provider, failureThreshold int, cooldownPeriod time.Duration) *CircuitBreakerProvider {
	return &CircuitBreakerProvider{
		wrapped:          p,
		failureThreshold: failureThreshold,
		cooldownPeriod:   cooldownPeriod,
	}
}

// Name returns the wrapped provider's name, so the breaker is transparent to
// anything that logs or reports on which provider is in use.
func (cb *CircuitBreakerProvider) Name() string { return cb.wrapped.Name() }

// Stream implements Provider. When the circuit is open it returns an error
// immediately without calling the wrapped provider at all; otherwise it
// delegates to the wrapped provider and records the outcome.
//
// The real providers (Anthropic, Bedrock) always return (ch, nil) from Stream
// itself — an API/network failure surfaces later as an EventError on the
// channel (consumed by internal/agent/loop.go's `for ev := range eventCh`
// loop), not as Stream's return error. So a synchronous error from the
// wrapped call is recorded immediately, but when Stream succeeds
// synchronously the outcome is only known once the stream actually finishes:
// the returned channel is wrapped so failures/successes are still detected
// and fed back into the breaker, while every event is transparently forwarded
// to the real caller unchanged.
func (cb *CircuitBreakerProvider) Stream(ctx context.Context, req Request) (<-chan Event, error) {
	if !cb.allow() {
		return nil, fmt.Errorf("circuit breaker open: LLM provider %q appears degraded, failing fast", cb.wrapped.Name())
	}

	ch, err := cb.wrapped.Stream(ctx, req)
	if err != nil {
		cb.recordResult(ctx, err)
		return ch, err
	}

	// Buffered the same as AnthropicProvider's internal channel (see
	// anthropic.go) so wrapping doesn't introduce a new blocking risk beyond
	// what already exists if a consumer stops reading early.
	out := make(chan Event, 64)
	go func() {
		defer close(out)
		var streamErr error
		for ev := range ch {
			if ev.Type == EventError && ev.Err != nil {
				streamErr = ev.Err
			}
			out <- ev
		}
		cb.recordResult(ctx, streamErr)
	}()
	return out, nil
}

// allow decides, under lock, whether this call may proceed to the wrapped
// provider. It performs the open -> half-open transition once the cooldown
// has elapsed, and ensures at most one trial call is in flight while
// half-open (concurrent callers during that window are failed fast, same as
// while fully open).
func (cb *CircuitBreakerProvider) allow() bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	switch cb.state {
	case stateOpen:
		if time.Since(cb.openedAt) < cb.cooldownPeriod {
			return false
		}
		// Cooldown elapsed: transition to half-open and let this call
		// through as the single trial.
		cb.state = stateHalfOpen
		return true

	case stateHalfOpen:
		// A trial call is already in flight; don't let a second one
		// through concurrently.
		return false

	default: // stateClosed
		return true
	}
}

// recordResult updates the state machine after the wrapped provider's
// Stream call returns.
func (cb *CircuitBreakerProvider) recordResult(ctx context.Context, err error) {
	cancelled := isCallerCancellation(ctx, err)

	cb.mu.Lock()
	defer cb.mu.Unlock()

	if cancelled {
		if cb.state == stateHalfOpen {
			// The trial call was inconclusive — the caller gave up, which
			// says nothing about the provider's health. Re-open (with a
			// fresh cooldown) rather than leaving the breaker stuck in
			// half-open forever with no further trial ever able to run.
			cb.state = stateOpen
			cb.openedAt = time.Now()
		}
		return
	}

	if err != nil {
		cb.onFailureLocked()
	} else {
		cb.onSuccessLocked()
	}
}

// onFailureLocked must be called with cb.mu held.
func (cb *CircuitBreakerProvider) onFailureLocked() {
	if cb.state == stateHalfOpen {
		// The trial call failed: recovery not confirmed, re-open for
		// another full cooldown period.
		cb.state = stateOpen
		cb.openedAt = time.Now()
		return
	}

	cb.consecutiveFail++
	if cb.consecutiveFail >= cb.failureThreshold {
		cb.state = stateOpen
		cb.openedAt = time.Now()
	}
}

// onSuccessLocked must be called with cb.mu held.
func (cb *CircuitBreakerProvider) onSuccessLocked() {
	if cb.state == stateHalfOpen {
		// The trial call succeeded: recovery confirmed, close the circuit.
		cb.state = stateClosed
	}
	cb.consecutiveFail = 0
}

// isCallerCancellation reports whether err represents the caller's own
// context being cancelled or timing out, as opposed to the provider failing
// on its own. Mirrors the pattern already used in
// internal/agent/loop.go (errors.Is(execErr, context.Canceled)).
func isCallerCancellation(ctx context.Context, err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	return ctx.Err() != nil
}
