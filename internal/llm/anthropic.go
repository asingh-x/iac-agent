package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const anthropicAPIURL = "https://api.anthropic.com/v1/messages"

// AnthropicProvider implements Provider using the Anthropic HTTP API directly
// (raw HTTP + SSE — no SDK dependency).
type AnthropicProvider struct {
	apiKey string
	debug  bool
	client *http.Client
}

// NewAnthropicProvider creates a provider backed by the Anthropic API.
// When debug is true, raw requests and SSE events are logged to stderr.
func NewAnthropicProvider(apiKey string, debug bool) *AnthropicProvider {
	return &AnthropicProvider{
		apiKey: apiKey,
		debug:  debug,
		client: &http.Client{Timeout: 0},
	}
}

func (p *AnthropicProvider) Name() string { return "anthropic" }

// --- internal wire types ---

type anthropicRequest struct {
	Model     string             `json:"model"`
	MaxTokens int                `json:"max_tokens"`
	Stream    bool               `json:"stream"`
	System    []wireSystemBlock  `json:"system,omitempty"`
	Messages  []anthropicMessage `json:"messages"`
	Tools     []ToolSchema       `json:"tools,omitempty"`
}

type anthropicMessage struct {
	Role    string        `json:"role"`
	Content []interface{} `json:"content"`
}

// wireCacheControl marks a content block as a prompt-caching breakpoint.
// Anthropic caches everything up to and including the block it's attached to.
type wireCacheControl struct {
	Type string `json:"type"`
}

// ephemeralCacheControl is the standard (5-minute TTL) cache breakpoint marker.
func ephemeralCacheControl() *wireCacheControl {
	return &wireCacheControl{Type: "ephemeral"}
}

// wireSystemBlock is a system-prompt content block. The Anthropic API accepts
// System either as a bare string or as an array of blocks like this one — the
// array form is required to attach a cache_control breakpoint.
type wireSystemBlock struct {
	Type         string            `json:"type"`
	Text         string            `json:"text"`
	CacheControl *wireCacheControl `json:"cache_control,omitempty"`
}

type wireTextBlock struct {
	Type         string            `json:"type"`
	Text         string            `json:"text"`
	CacheControl *wireCacheControl `json:"cache_control,omitempty"`
}

type wireToolUseBlock struct {
	Type         string            `json:"type"`
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	Input        json.RawMessage   `json:"input"`
	CacheControl *wireCacheControl `json:"cache_control,omitempty"`
}

type wireToolResultBlock struct {
	Type         string            `json:"type"`
	ToolUseID    string            `json:"tool_use_id"`
	Content      string            `json:"content"`
	CacheControl *wireCacheControl `json:"cache_control,omitempty"`
}

func convertMessages(msgs []Message) []anthropicMessage {
	out := make([]anthropicMessage, 0, len(msgs))
	for _, m := range msgs {
		blocks := make([]interface{}, 0, len(m.Content))
		for _, cb := range m.Content {
			switch cb.Type {
			case "text":
				blocks = append(blocks, wireTextBlock{Type: "text", Text: cb.Text})
			case "tool_use":
				inp := cb.Input
				if len(inp) == 0 {
					inp = json.RawMessage("{}")
				}
				blocks = append(blocks, wireToolUseBlock{
					Type: "tool_use", ID: cb.ID, Name: cb.Name, Input: inp,
				})
			case "tool_result":
				blocks = append(blocks, wireToolResultBlock{
					Type: "tool_result", ToolUseID: cb.ToolUseID, Content: cb.Content,
				})
			}
		}
		out = append(out, anthropicMessage{Role: m.Role, Content: blocks})
	}
	return out
}

// buildSystemBlocks wraps the flat system prompt string into the array form
// required to attach a cache_control breakpoint. The system prompt is built
// once per agent turn-loop and resent unchanged on every request within that
// loop (see internal/agent/loop.go RunTurn + BuildSystemPrompt), so it is the
// highest-value, lowest-risk cache breakpoint: caching it means every
// tool-use round trip within a turn re-reads the (large, static) system
// prompt from cache instead of paying full input-token price for it again.
func buildSystemBlocks(system string) []wireSystemBlock {
	if system == "" {
		return nil
	}
	return []wireSystemBlock{
		{
			Type:         "text",
			Text:         system,
			CacheControl: ephemeralCacheControl(),
		},
	}
}

// applyConversationCacheControl marks the last content block of the last
// message as a cache breakpoint. Because the agent loop only ever appends to
// the message history within a turn-loop (assistant reply, then tool
// results, then the next request), the prefix up to and including the
// previous "last block" is byte-identical across requests — so marking the
// new last block on every request lets each subsequent request cache-read
// everything before it instead of re-paying for the full growing history.
func applyConversationCacheControl(messages []anthropicMessage) {
	if len(messages) == 0 {
		return
	}
	last := &messages[len(messages)-1]
	if len(last.Content) == 0 {
		return
	}
	idx := len(last.Content) - 1
	switch b := last.Content[idx].(type) {
	case wireTextBlock:
		b.CacheControl = ephemeralCacheControl()
		last.Content[idx] = b
	case wireToolUseBlock:
		b.CacheControl = ephemeralCacheControl()
		last.Content[idx] = b
	case wireToolResultBlock:
		b.CacheControl = ephemeralCacheControl()
		last.Content[idx] = b
	}
}

// buildRequest constructs the outgoing Anthropic Messages API request body,
// attaching prompt-caching breakpoints: one on the system prompt block, and
// one on the last content block of the conversation so far. This is the
// single seam used by both doStream and the unit tests below — it does no
// I/O, so it can be tested without a live API key or network call.
func buildRequest(req Request, stream bool) anthropicRequest {
	ar := anthropicRequest{
		Model:     req.Model,
		MaxTokens: req.MaxTokens,
		Stream:    stream,
		System:    buildSystemBlocks(req.System),
		Messages:  convertMessages(req.Messages),
		Tools:     req.Tools,
	}
	if len(ar.Tools) == 0 {
		ar.Tools = nil
	}
	applyConversationCacheControl(ar.Messages)
	return ar
}

// Stream opens a streaming connection and returns a channel of events.
// Retries up to 3 times on 429/503/529.
func (p *AnthropicProvider) Stream(ctx context.Context, req Request) (<-chan Event, error) {
	ch := make(chan Event, 64)
	go func() {
		defer close(ch)
		if err := p.doStream(ctx, req, ch); err != nil {
			select {
			case ch <- Event{Type: EventError, Err: err}:
			default:
			}
		}
	}()
	return ch, nil
}

func (p *AnthropicProvider) doStream(ctx context.Context, req Request, ch chan<- Event) error {
	ar := buildRequest(req, true)

	payload, err := json.Marshal(ar)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	if p.debug {
		log.Printf("[DEBUG] anthropic request: %s", payload)
	}

	const maxRetries = 6
	var lastErr error
	backoff := 2 * time.Second
	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
		}

		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, anthropicAPIURL, bytes.NewReader(payload))
		if err != nil {
			return fmt.Errorf("new request: %w", err)
		}
		httpReq.Header.Set("x-api-key", p.apiKey)
		httpReq.Header.Set("anthropic-version", "2023-06-01")
		httpReq.Header.Set("content-type", "application/json")
		httpReq.Header.Set("anthropic-beta", "prompt-caching-2024-07-31")

		resp, err := p.client.Do(httpReq)
		if err != nil {
			lastErr = err
			backoff *= 2
			if backoff > 60*time.Second {
				backoff = 60 * time.Second
			}
			continue
		}

		if resp.StatusCode == 429 || resp.StatusCode == 503 || resp.StatusCode == 529 {
			// Honour Retry-After header if present, otherwise exponential backoff.
			wait := backoff
			if ra := resp.Header.Get("Retry-After"); ra != "" {
				if secs, err := strconv.Atoi(ra); err == nil {
					wait = time.Duration(secs) * time.Second
				}
			}
			_ = resp.Body.Close()
			lastErr = fmt.Errorf("API rate limited (HTTP %d), waiting %s before retry %d/%d", resp.StatusCode, wait.Round(time.Second), attempt+1, maxRetries)
			log.Printf("[WARN] %s", lastErr)
			backoff *= 2
			if backoff > 60*time.Second {
				backoff = 60 * time.Second
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
			continue
		}

		if resp.StatusCode != 200 {
			b, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			return fmt.Errorf("API error %d: %s", resp.StatusCode, b)
		}

		parseErr := p.parseSSE(ctx, resp.Body, ch)
		_ = resp.Body.Close()
		return parseErr
	}
	return fmt.Errorf("rate limit: all %d retries exhausted — try again in a moment", maxRetries)
}

// pendingTool accumulates streamed tool-use input fragments.
type pendingTool struct {
	id        string
	name      string
	inputJSON strings.Builder
}

func (p *AnthropicProvider) parseSSE(ctx context.Context, body io.Reader, ch chan<- Event) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)

	var evName, evData string
	var tool *pendingTool

	dispatch := func(name, data string) {
		var raw map[string]json.RawMessage
		if err := json.Unmarshal([]byte(data), &raw); err != nil {
			return
		}

		switch name {
		case "message_start":
			if msgRaw, ok := raw["message"]; ok {
				var msg struct {
					Usage struct {
						InputTokens  int `json:"input_tokens"`
						CacheRead    int `json:"cache_read_input_tokens"`
						CacheCreated int `json:"cache_creation_input_tokens"`
					} `json:"usage"`
				}
				if err := json.Unmarshal(msgRaw, &msg); err == nil {
					ch <- Event{Type: EventUsage, Usage: &UsageEvent{
						InputTokens:  msg.Usage.InputTokens,
						CacheRead:    msg.Usage.CacheRead,
						CacheCreated: msg.Usage.CacheCreated,
					}}
				}
			}

		case "content_block_start":
			var cbs struct {
				ContentBlock struct {
					Type string `json:"type"`
					ID   string `json:"id"`
					Name string `json:"name"`
				} `json:"content_block"`
			}
			if err := json.Unmarshal([]byte(data), &cbs); err == nil {
				if cbs.ContentBlock.Type == "tool_use" {
					tool = &pendingTool{id: cbs.ContentBlock.ID, name: cbs.ContentBlock.Name}
				}
			}

		case "content_block_delta":
			var cbd struct {
				Delta struct {
					Type        string `json:"type"`
					Text        string `json:"text"`
					PartialJSON string `json:"partial_json"`
				} `json:"delta"`
			}
			if err := json.Unmarshal([]byte(data), &cbd); err == nil {
				switch cbd.Delta.Type {
				case "text_delta":
					ch <- Event{Type: EventText, Delta: cbd.Delta.Text}
				case "input_json_delta":
					if tool != nil {
						tool.inputJSON.WriteString(cbd.Delta.PartialJSON)
					}
				}
			}

		case "content_block_stop":
			if tool != nil {
				s := tool.inputJSON.String()
				if s == "" {
					s = "{}"
				}
				ch <- Event{
					Type: EventToolUse,
					ToolUse: &ToolUseEvent{
						ID:    tool.id,
						Name:  tool.name,
						Input: json.RawMessage(s),
					},
				}
				tool = nil
			}

		case "message_delta":
			var md struct {
				Delta struct {
					StopReason string `json:"stop_reason"`
				} `json:"delta"`
				Usage struct {
					OutputTokens int `json:"output_tokens"`
				} `json:"usage"`
			}
			if err := json.Unmarshal([]byte(data), &md); err == nil {
				ch <- Event{Type: EventUsage, Usage: &UsageEvent{OutputTokens: md.Usage.OutputTokens}}
				if md.Delta.StopReason != "" {
					ch <- Event{Type: EventStop, StopReason: md.Delta.StopReason}
				}
			}

		case "message_stop":
			// message_stop is only the stream EOF sentinel — the semantic stop reason
			// was already emitted from message_delta. Do not overwrite it.
		}
	}

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		line := scanner.Text()
		if line == "" {
			if evName != "" && evData != "" {
				if p.debug {
					log.Printf("[DEBUG] anthropic SSE event=%s data=%s", evName, evData)
				}
				dispatch(evName, evData)
			}
			evName, evData = "", ""
			continue
		}
		if after, ok := strings.CutPrefix(line, "event:"); ok {
			evName = strings.TrimSpace(after)
		} else if after, ok := strings.CutPrefix(line, "data:"); ok {
			evData = strings.TrimSpace(after)
		}
	}
	return scanner.Err()
}
