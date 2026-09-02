package server

// Regression coverage for pauseGate (see its doc comment in task_runner.go):
// a Critical bug found in review of the semaphore-release-on-pause change
// showed that a single LLM turn can batch an ask_user tool call together
// with another tool call needing permission (e.g. bash), and
// internal/agent/loop.go's executeTools fans multiple tool calls from one
// turn into parallel goroutines. Before pauseGate existed, the AskUser
// callback and awaitPermission each independently released r.sem/userSem
// with a bare, un-selected channel receive — so two concurrent pauses on
// the SAME task (which only ever acquired one token of each at the top of
// Runner.run) would race to drain a channel that only had one token to
// give. Whichever lost the race blocked forever, with no ctx.Done escape,
// hanging that task's goroutine unrecoverably (or, with capacity to spare
// elsewhere on the pod, silently stole another task's slot instead).
//
// This test drives pauseGate directly, using an explicit barrier (not
// sleep-based timing) to force N concurrent release() calls to genuinely
// overlap, so the depth-counting behavior is exercised deterministically
// on every run rather than only when goroutine scheduling happens to race
// a particular way. See TestConcurrentAskUserAndPermissionPauses in
// permission_test.go for the corresponding end-to-end reproduction that
// drives the real fan-out scenario through an actual task run.

import (
	"sync"
	"testing"
	"time"
)

// TestPauseGate_ConcurrentPausesShareOneReleaseAndReacquire proves that N
// overlapping pauses on the same task release the underlying semaphore(s)
// exactly once (first pause in) and reacquire them exactly once (last pause
// out), regardless of how many pauses overlap — not just the N=2 case from
// the bug report.
func TestPauseGate_ConcurrentPausesShareOneReleaseAndReacquire(t *testing.T) {
	sem := make(chan struct{}, 1)
	sem <- struct{}{} // this task's one held global slot
	userSem := make(chan struct{}, 1)
	userSem <- struct{}{} // this task's one held per-user slot

	g := &pauseGate{sem: sem, userSem: userSem}

	const n = 3 // more than 2, to prove this isn't a special-cased pair
	runConcurrently(t, n, g.release)

	if got := len(sem); got != 0 {
		t.Errorf("sem length after %d overlapping release() calls = %d, want 0 (drained exactly once)", n, got)
	}
	if got := len(userSem); got != 0 {
		t.Errorf("userSem length after %d overlapping release() calls = %d, want 0 (drained exactly once)", n, got)
	}
	if g.depth != n {
		t.Errorf("depth after %d overlapping release() calls = %d, want %d", n, g.depth, n)
	}

	runConcurrently(t, n, g.reacquire)

	if got := len(sem); got != 1 {
		t.Errorf("sem length after all %d pauses ended = %d, want 1 (slot restored exactly once)", n, got)
	}
	if got := len(userSem); got != 1 {
		t.Errorf("userSem length after all %d pauses ended = %d, want 1 (slot restored exactly once)", n, got)
	}
	if g.depth != 0 {
		t.Errorf("depth after all pauses ended = %d, want 0", g.depth)
	}
}

// TestPauseGate_NilUserSemIsNoop confirms release/reacquire tolerate a nil
// userSem (the PerUserConcurrency-disabled case), which run() relies on:
// pauseGate must not panic or block trying to touch a nil channel.
func TestPauseGate_NilUserSemIsNoop(t *testing.T) {
	sem := make(chan struct{}, 1)
	sem <- struct{}{}

	g := &pauseGate{sem: sem, userSem: nil}

	runConcurrently(t, 2, g.release)
	if got := len(sem); got != 0 {
		t.Errorf("sem length = %d, want 0", got)
	}

	runConcurrently(t, 2, g.reacquire)
	if got := len(sem); got != 1 {
		t.Errorf("sem length = %d, want 1", got)
	}
}

// runConcurrently calls fn from n goroutines, using an explicit barrier
// (every goroutine must reach the barrier before any of them proceeds) so
// the calls genuinely overlap rather than merely being started close
// together and hoping the scheduler races them. It fails the test — rather
// than hanging the suite — if fn doesn't return within a bounded deadline,
// which is exactly the observable symptom of the pre-pauseGate bug: a
// second concurrent release with no token left to take blocks forever on a
// bare channel receive.
func runConcurrently(t *testing.T, n int, fn func()) {
	t.Helper()

	var wg sync.WaitGroup
	wg.Add(n)
	ready := make(chan struct{}, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			ready <- struct{}{}
			<-start // barrier: don't call fn until every goroutine is lined up
			fn()
		}()
	}
	for i := 0; i < n; i++ {
		<-ready
	}
	close(start) // release all n goroutines at once, forcing genuine overlap

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("concurrent calls deadlocked — a bare channel receive with no depth counting would block forever here once the one available token is taken")
	}
}
