package agent

import (
	"fmt"

	"github.com/tf-agent/tf-agent/internal/session"
)

// contextWindowTokens approximates the input context window (in tokens) for
// the Claude model family this agent talks to (Sonnet/Haiku, ~200K tokens).
//
// Note: cfg.Agent.MaxTokens (internal/config/settings.go) caps the *output*
// length of a single response — it is passed straight through as
// llm.Request.MaxTokens in runTurn — and says nothing about how much
// conversation history fits in the input context. Using it as an
// auto-compaction trigger would compact on nearly every turn (its default is
// 8192, far smaller than the actual context window), so we track the
// model's real context window here instead.
const contextWindowTokens = 200_000

// autoCompactThresholdRatio is the fraction of the context window at which
// automatic compaction kicks in. 80% leaves headroom for the next turn's
// tool results and response while still compacting well before a request
// would be rejected for exceeding the context window. Neither the manual
// "/compact" command nor the old CompactIfNeeded (record-count based, never
// wired up) established a token-based threshold, so this value is a
// deliberate, documented choice rather than a reuse of an existing one.
const autoCompactThresholdRatio = 0.8

// autoCompactTokenThreshold is the absolute input-token count above which
// auto-compaction triggers.
const autoCompactTokenThreshold = int(contextWindowTokens * autoCompactThresholdRatio)

// autoCompactKeepTurns matches the number of recent user turns preserved by
// the manual "/compact" command (internal/commands/handlers.go calls
// session.Compact(records, 5)), so automatic and manual compaction produce
// the same shape of history.
const autoCompactKeepTurns = 5

// lastKnownInputTokens returns the input-token count reported by the most
// recent assistant turn in the session, or 0 if no usage has been recorded
// yet (e.g. the very first turn). Providers report input token usage as the
// size of the full request (system prompt + history) at the time it was
// sent, so this is the best available signal for "how large is the
// conversation we are about to grow further" without re-tokenizing history
// ourselves.
func lastKnownInputTokens(sess *session.Store) int {
	records := sess.Records()
	for i := len(records) - 1; i >= 0; i-- {
		if r := records[i]; r.Type == "assistant" && r.Usage != nil {
			return r.Usage.Input
		}
	}
	return 0
}

// maybeAutoCompact runs the same compaction logic as the manual "/compact"
// command (session.Compact) when lastInputTokens is approaching the model's
// context window. It reports whether compaction actually changed the
// session (session.Compact is a no-op when there aren't more than
// autoCompactKeepTurns user turns yet, in which case there is nothing
// automatic compaction needs to do either).
//
// Unlike the manual command — whose invocation and chat reply are
// themselves visible to the user — automatic compaction has no user-facing
// reply, so it appends a "system" record documenting that it ran. Record
// types other than "user"/"assistant"/"tool_use"/"tool_result" are ignored
// by buildMessages (see the `default: i++` case), so this note is stored
// for history/debugging visibility without being sent to the LLM.
func (a *Agent) maybeAutoCompact(lastInputTokens int) bool {
	if lastInputTokens < autoCompactTokenThreshold {
		return false
	}

	records := a.session.Records()
	compacted := session.Compact(records, autoCompactKeepTurns)
	if len(compacted) >= len(records) {
		// Nothing to compact yet (not enough turns beyond autoCompactKeepTurns).
		return false
	}

	a.session.Clear()
	for _, r := range compacted {
		_ = a.session.Append(r)
	}

	_ = a.session.Append(session.Record{
		Type: "system",
		Content: fmt.Sprintf(
			"Auto-compacted context: %d records -> %d records (last input usage %d tokens >= %d token threshold)",
			len(records), len(compacted), lastInputTokens, autoCompactTokenThreshold,
		),
	})

	return true
}
