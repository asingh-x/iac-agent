package llm

import (
	"encoding/json"
	"testing"
)

// helper to marshal an anthropicRequest and unmarshal it back into a generic
// map tree, so assertions exercise the actual wire JSON rather than the Go
// struct shape (which could hide a bad json tag).
func marshalToMap(t *testing.T, ar anthropicRequest) map[string]interface{} {
	t.Helper()
	payload, err := json.Marshal(ar)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(payload, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return m
}

func cacheControlOf(t *testing.T, block interface{}) (string, bool) {
	t.Helper()
	bm, ok := block.(map[string]interface{})
	if !ok {
		t.Fatalf("block is not an object: %#v", block)
	}
	cc, ok := bm["cache_control"]
	if !ok {
		return "", false
	}
	ccm, ok := cc.(map[string]interface{})
	if !ok {
		t.Fatalf("cache_control is not an object: %#v", cc)
	}
	typ, _ := ccm["type"].(string)
	return typ, true
}

// TestBuildRequest_SystemPromptGetsCacheBreakpoint verifies that the system
// prompt is sent as a content-block array (not a bare string) with an
// ephemeral cache_control breakpoint attached — this is what actually
// activates prompt caching for the system prompt on the wire.
func TestBuildRequest_SystemPromptGetsCacheBreakpoint(t *testing.T) {
	req := Request{
		Model:     "claude-sonnet-4-6",
		System:    "you are a helpful assistant",
		MaxTokens: 1024,
		Messages: []Message{
			{Role: "user", Content: []ContentBlock{{Type: "text", Text: "hello"}}},
		},
	}

	ar := buildRequest(req, true)
	m := marshalToMap(t, ar)

	sysRaw, ok := m["system"]
	if !ok {
		t.Fatalf("expected top-level \"system\" field in request JSON, got none: %v", m)
	}
	sysBlocks, ok := sysRaw.([]interface{})
	if !ok {
		t.Fatalf("expected \"system\" to serialize as an array of blocks (required for cache_control), got %T: %v", sysRaw, sysRaw)
	}
	if len(sysBlocks) != 1 {
		t.Fatalf("expected exactly 1 system block, got %d", len(sysBlocks))
	}

	block, ok := sysBlocks[0].(map[string]interface{})
	if !ok {
		t.Fatalf("system block is not an object: %#v", sysBlocks[0])
	}
	if block["type"] != "text" {
		t.Errorf("expected system block type \"text\", got %v", block["type"])
	}
	if block["text"] != "you are a helpful assistant" {
		t.Errorf("expected system block text to be preserved, got %v", block["text"])
	}

	typ, hasCC := cacheControlOf(t, sysBlocks[0])
	if !hasCC {
		t.Fatalf("expected system block to carry cache_control, found none: %#v", block)
	}
	if typ != "ephemeral" {
		t.Errorf("expected cache_control.type \"ephemeral\", got %q", typ)
	}
}

// TestBuildRequest_EmptySystemOmitsField ensures we don't emit an empty
// system array (which the API would reject) when there's no system prompt.
func TestBuildRequest_EmptySystemOmitsField(t *testing.T) {
	req := Request{
		Model:     "claude-sonnet-4-6",
		MaxTokens: 1024,
		Messages: []Message{
			{Role: "user", Content: []ContentBlock{{Type: "text", Text: "hello"}}},
		},
	}

	ar := buildRequest(req, true)
	m := marshalToMap(t, ar)

	if _, ok := m["system"]; ok {
		t.Errorf("expected no \"system\" field when System is empty, got: %v", m["system"])
	}
}

// TestBuildRequest_LastMessageBlockGetsCacheBreakpoint verifies the second
// cache breakpoint: the last content block of the last message is marked so
// that, turn over turn (as the agent loop appends assistant replies and tool
// results), each new request can cache-read the entire stable prefix instead
// of re-paying for it.
func TestBuildRequest_LastMessageBlockGetsCacheBreakpoint(t *testing.T) {
	req := Request{
		Model:     "claude-sonnet-4-6",
		System:    "sys",
		MaxTokens: 1024,
		Messages: []Message{
			{Role: "user", Content: []ContentBlock{{Type: "text", Text: "turn 1"}}},
			{Role: "assistant", Content: []ContentBlock{
				{Type: "text", Text: "thinking..."},
				{Type: "tool_use", ID: "t1", Name: "read_file", Input: json.RawMessage(`{"path":"a.tf"}`)},
			}},
			{Role: "user", Content: []ContentBlock{
				{Type: "tool_result", ToolUseID: "t1", Content: "file contents"},
			}},
		},
	}

	ar := buildRequest(req, true)
	m := marshalToMap(t, ar)

	msgs, ok := m["messages"].([]interface{})
	if !ok || len(msgs) != 3 {
		t.Fatalf("expected 3 messages, got %#v", m["messages"])
	}

	// Every block except the very last one in the very last message must be
	// free of cache_control — otherwise we're burning breakpoints (max 4)
	// or, worse, caching a volatile prefix incorrectly.
	for mi, rawMsg := range msgs {
		msg := rawMsg.(map[string]interface{})
		blocks := msg["content"].([]interface{})
		for bi, block := range blocks {
			isLast := mi == len(msgs)-1 && bi == len(blocks)-1
			_, hasCC := cacheControlOf(t, block)
			if isLast && !hasCC {
				t.Errorf("expected cache_control on last block of last message (msg %d, block %d), found none", mi, bi)
			}
			if !isLast && hasCC {
				t.Errorf("unexpected cache_control on non-terminal block (msg %d, block %d): %#v", mi, bi, block)
			}
		}
	}

	// Confirm the marked block is the tool_result block and its content is
	// untouched.
	lastMsg := msgs[2].(map[string]interface{})
	lastBlocks := lastMsg["content"].([]interface{})
	lastBlock := lastBlocks[len(lastBlocks)-1].(map[string]interface{})
	if lastBlock["type"] != "tool_result" {
		t.Fatalf("expected last block to be tool_result, got %v", lastBlock["type"])
	}
	if lastBlock["content"] != "file contents" {
		t.Errorf("expected tool_result content preserved, got %v", lastBlock["content"])
	}
	typ, _ := cacheControlOf(t, lastBlock)
	if typ != "ephemeral" {
		t.Errorf("expected cache_control.type \"ephemeral\" on tool_result block, got %q", typ)
	}
}

// TestBuildRequest_NoMessagesDoesNotPanic guards the edge case of an empty
// message slice (e.g. a malformed request) — applyConversationCacheControl
// must be a no-op, not a panic.
func TestBuildRequest_NoMessagesDoesNotPanic(t *testing.T) {
	req := Request{Model: "claude-sonnet-4-6", MaxTokens: 1024}
	ar := buildRequest(req, false)
	if len(ar.Messages) != 0 {
		t.Fatalf("expected 0 messages, got %d", len(ar.Messages))
	}
}

// TestBuildRequest_StreamFlagPassthrough confirms buildRequest still wires
// the stream flag through untouched by the caching changes.
func TestBuildRequest_StreamFlagPassthrough(t *testing.T) {
	req := Request{Model: "claude-sonnet-4-6", MaxTokens: 1024}

	if ar := buildRequest(req, true); !ar.Stream {
		t.Errorf("expected Stream=true to be preserved")
	}
	if ar := buildRequest(req, false); ar.Stream {
		t.Errorf("expected Stream=false to be preserved")
	}
}

// TestBuildRequest_EmptyToolsOmitted preserves the pre-existing behavior of
// nil-ing out an empty tools slice so "tools" is omitted from the JSON,
// independent of the caching changes.
func TestBuildRequest_EmptyToolsOmitted(t *testing.T) {
	req := Request{Model: "claude-sonnet-4-6", MaxTokens: 1024}
	ar := buildRequest(req, true)
	m := marshalToMap(t, ar)
	if _, ok := m["tools"]; ok {
		t.Errorf("expected no \"tools\" field when Tools is empty, got: %v", m["tools"])
	}
}
