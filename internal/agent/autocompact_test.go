package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/tf-agent/tf-agent/internal/llm"
)

// TestAgent_AutoCompact_TriggersWhenApproachingContextWindow drives many
// simulated user turns through RunTurn, each reporting input token usage
// above the auto-compaction threshold, and verifies that compaction fires
// on its own — without any "/compact" command being invoked — once enough
// turns have accumulated to actually have something to compact.
func TestAgent_AutoCompact_TriggersWhenApproachingContextWindow(t *testing.T) {
	// Every simulated turn reports usage comfortably above
	// autoCompactTokenThreshold (80% of contextWindowTokens).
	events := []llm.Event{
		{Type: llm.EventText, Delta: "ok"},
		{Type: llm.EventUsage, Usage: &llm.UsageEvent{InputTokens: autoCompactTokenThreshold + 1_000}},
		{Type: llm.EventStop, StopReason: "end_turn"},
	}
	provider := llm.NewMockProvider("mock", events)
	agent := buildTestAgent(t, provider, nil)

	// Drive more turns than autoCompactKeepTurns so there is something for
	// session.Compact to actually collapse. maybeAutoCompact checks usage
	// from the *previous* turn, so compaction can only fire starting on the
	// turn after autoCompactKeepTurns have already been recorded.
	const totalTurns = autoCompactKeepTurns + 3
	for i := 0; i < totalTurns; i++ {
		collectEvents(agent.RunTurn(context.Background(), "message"))
	}

	records := agent.session.Records()

	var sawAutoCompactNote bool
	var userTurns int
	for _, r := range records {
		if r.Type == "system" && strings.Contains(r.Content, "Auto-compacted context") {
			sawAutoCompactNote = true
		}
		if r.Type == "user" {
			userTurns++
		}
	}

	if !sawAutoCompactNote {
		t.Error("expected a 'system' record documenting automatic compaction, none found")
	}

	// Without compaction there would be totalTurns user records. Compaction
	// collapses everything before the last autoCompactKeepTurns turns into a
	// single summary "user" record, so the count must be well below totalTurns.
	if userTurns >= totalTurns {
		t.Errorf("expected fewer user records after auto-compaction: got %d, totalTurns=%d", userTurns, totalTurns)
	}
}

// TestAgent_AutoCompact_NoOpBelowThreshold verifies that a conversation
// whose reported usage never approaches the context window is left alone —
// automatic compaction must not fire for ordinary, small conversations.
func TestAgent_AutoCompact_NoOpBelowThreshold(t *testing.T) {
	events := []llm.Event{
		{Type: llm.EventText, Delta: "ok"},
		{Type: llm.EventUsage, Usage: &llm.UsageEvent{InputTokens: 500}},
		{Type: llm.EventStop, StopReason: "end_turn"},
	}
	provider := llm.NewMockProvider("mock", events)
	agent := buildTestAgent(t, provider, nil)

	const totalTurns = autoCompactKeepTurns + 3
	for i := 0; i < totalTurns; i++ {
		collectEvents(agent.RunTurn(context.Background(), "message"))
	}

	records := agent.session.Records()
	for _, r := range records {
		if r.Type == "system" && strings.Contains(r.Content, "Auto-compacted context") {
			t.Error("did not expect automatic compaction to fire when usage stays below threshold")
		}
	}

	var userTurns int
	for _, r := range records {
		if r.Type == "user" {
			userTurns++
		}
	}
	if userTurns != totalTurns {
		t.Errorf("expected %d user records (no compaction), got %d", totalTurns, userTurns)
	}
}
