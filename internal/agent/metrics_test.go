package agent

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/tf-agent/tf-agent/internal/llm"
	"github.com/tf-agent/tf-agent/internal/tools"
)

// TestExecuteSingleTool_RecordsSuccessMetrics proves a successful tool.Execute
// call at the loop.go:368 call site increments both the call counter (labelled
// success) and the duration histogram's sample count for that tool name.
func TestExecuteSingleTool_RecordsSuccessMetrics(t *testing.T) {
	before := testutil.ToFloat64(metricToolCalls.WithLabelValues("bash", "success"))

	toolInput := json.RawMessage(`{"command":"echo tool_executed"}`)
	firstCall := []llm.Event{
		{Type: llm.EventToolUse, ToolUse: &llm.ToolUseEvent{ID: "call_1", Name: "bash", Input: toolInput}},
		{Type: llm.EventStop, StopReason: "tool_use"},
	}
	secondCall := []llm.Event{
		{Type: llm.EventText, Delta: "Done."},
		{Type: llm.EventStop, StopReason: "end_turn"},
	}
	callCount := 0
	provider := &multiCallMock{calls: [][]llm.Event{firstCall, secondCall}, current: &callCount}

	reg := tools.NewRegistry()
	reg.Register(&tools.BashTool{})
	agent := buildTestAgent(t, provider, reg)

	collectEvents(agent.RunTurn(context.Background(), "run echo"))

	after := testutil.ToFloat64(metricToolCalls.WithLabelValues("bash", "success"))
	if after != before+1 {
		t.Errorf("tfagent_tool_calls_total{tool=bash,result=success} = %v, want %v", after, before+1)
	}

	durationSamples := testutil.CollectAndCount(metricToolDuration)
	if durationSamples == 0 {
		t.Error("expected at least one tfagent_tool_duration_seconds sample to have been recorded, got none")
	}
}

// TestExecuteSingleTool_RecordsErrorMetrics proves a tool.Execute call that
// returns an error is labelled "error", not "success" — this is what lets an
// operator distinguish a genuinely broken tool from normal traffic on
// /metrics.
func TestExecuteSingleTool_RecordsErrorMetrics(t *testing.T) {
	before := testutil.ToFloat64(metricToolCalls.WithLabelValues("bash", "error"))

	// An empty command makes BashTool.Execute return an error synchronously,
	// without needing to actually run (or fail to run) a real shell command.
	toolInput := json.RawMessage(`{"command":""}`)
	firstCall := []llm.Event{
		{Type: llm.EventToolUse, ToolUse: &llm.ToolUseEvent{ID: "call_1", Name: "bash", Input: toolInput}},
		{Type: llm.EventStop, StopReason: "tool_use"},
	}
	secondCall := []llm.Event{
		{Type: llm.EventText, Delta: "Done."},
		{Type: llm.EventStop, StopReason: "end_turn"},
	}
	callCount := 0
	provider := &multiCallMock{calls: [][]llm.Event{firstCall, secondCall}, current: &callCount}

	reg := tools.NewRegistry()
	reg.Register(&tools.BashTool{})
	agent := buildTestAgent(t, provider, reg)

	collectEvents(agent.RunTurn(context.Background(), "run empty command"))

	after := testutil.ToFloat64(metricToolCalls.WithLabelValues("bash", "error"))
	if after != before+1 {
		t.Errorf("tfagent_tool_calls_total{tool=bash,result=error} = %v, want %v", after, before+1)
	}
}
