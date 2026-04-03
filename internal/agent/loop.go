package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/tf-agent/tf-agent/internal/commands"
	"github.com/tf-agent/tf-agent/internal/config"
	"github.com/tf-agent/tf-agent/internal/hooks"
	"github.com/tf-agent/tf-agent/internal/llm"
	"github.com/tf-agent/tf-agent/internal/permissions"
	"github.com/tf-agent/tf-agent/internal/session"
	"github.com/tf-agent/tf-agent/internal/skills"
	"github.com/tf-agent/tf-agent/internal/tools"
)

// TurnEventType classifies agent turn events.
type TurnEventType int

const (
	TurnEventText        TurnEventType = iota
	TurnEventToolStart                 // tool call starting
	TurnEventToolEnd                   // tool call completed
	TurnEventPermission                // needs user permission
	TurnEventUsage                     // token usage
	TurnEventError                     // non-fatal error info
	TurnEventDone                      // turn complete
)

// ToolCall describes a pending tool invocation.
type ToolCall struct {
	ID    string
	Name  string
	Input json.RawMessage
}

// ToolResult describes a completed tool invocation.
type ToolResult struct {
	ID     string
	Name   string
	Output string
	Err    error
}

// TurnEvent is emitted by RunTurn for each interesting occurrence.
type TurnEvent struct {
	Type       TurnEventType
	Text       string
	ToolCall   *ToolCall
	ToolResult *ToolResult
	Usage      *llm.UsageEvent
	Err        error
	// PermissionRequest is set when Type == TurnEventPermission.
	// The caller must respond via PermissionCh.
	PermissionRequest *PermissionRequest
}

// PermissionRequest asks the caller if a tool may run.
type PermissionRequest struct {
	ToolName string
	Input    json.RawMessage
	// ResponseCh receives true (allow) or false (deny).
	ResponseCh chan bool
}

// Agent is the core agent that orchestrates the LLM + tools.
type Agent struct {
	provider    llm.Provider
	tools       *tools.Registry
	skills      *skills.Registry
	session     *session.Store
	permissions *permissions.Checker
	hooks       *hooks.Runner
	commands    *commands.Registry
	config      *config.Config
	cwd         string
	model       string
	agentMD     string
}

// NewAgent constructs a fully wired Agent.
func NewAgent(
	provider llm.Provider,
	toolReg *tools.Registry,
	skillReg *skills.Registry,
	sess *session.Store,
	perm *permissions.Checker,
	hookRunner *hooks.Runner,
	cmdRegistry *commands.Registry,
	cfg *config.Config,
	cwd string,
	model string,
	agentMD string,
) *Agent {
	return &Agent{
		provider:    provider,
		tools:       toolReg,
		skills:      skillReg,
		session:     sess,
		permissions: perm,
		hooks:       hookRunner,
		commands:    cmdRegistry,
		config:      cfg,
		cwd:         cwd,
		model:       model,
		agentMD:     agentMD,
	}
}

// RunTurn executes one user turn, emitting events on the returned channel.
// The channel is closed when the turn is complete.
func (a *Agent) RunTurn(ctx context.Context, userInput string) <-chan TurnEvent {
	ch := make(chan TurnEvent, 64)
	go func() {
		defer close(ch)
		a.runTurn(ctx, userInput, ch)
	}()
	return ch
}

func (a *Agent) runTurn(ctx context.Context, userInput string, ch chan<- TurnEvent) {
	// Save user message to session.
	_ = a.session.Append(session.Record{Type: "user", Content: userInput})

	// Build message history from session records.
	messages := a.buildMessages()

	// Add the new user message.
	messages = append(messages, llm.Message{
		Role: "user",
		Content: []llm.ContentBlock{
			{Type: "text", Text: userInput},
		},
	})

	// Combine tool schemas from tools + skills.
	schemas := a.allSchemas()

	systemPrompt := BuildSystemPrompt(a.cwd, a.tools, a.skills, a.agentMD)
	maxTurns := a.config.Agent.MaxTurns
	if maxTurns <= 0 {
		maxTurns = 10
	}

	for turn := 0; turn < maxTurns; turn++ {
		req := llm.Request{
			Model:     a.model,
			System:    systemPrompt,
			Messages:  messages,
			Tools:     schemas,
			MaxTokens: a.config.Agent.MaxTokens,
		}

		eventCh, err := streamWithRetry(ctx, a.provider, req)
		if err != nil {
			ch <- TurnEvent{Type: TurnEventError, Err: err}
			return
		}

		// Accumulate the assistant message as we stream.
		var assistantText string
		var toolCalls []llm.ToolUseEvent
		var stopReason string
		var totalUsage llm.UsageEvent

		for ev := range eventCh {
			select {
			case <-ctx.Done():
				return
			default:
			}

			switch ev.Type {
			case llm.EventText:
				assistantText += ev.Delta
				ch <- TurnEvent{Type: TurnEventText, Text: ev.Delta}

			case llm.EventToolUse:
				if ev.ToolUse != nil {
					toolCalls = append(toolCalls, *ev.ToolUse)
					ch <- TurnEvent{Type: TurnEventToolStart, ToolCall: &ToolCall{
						ID:    ev.ToolUse.ID,
						Name:  ev.ToolUse.Name,
						Input: ev.ToolUse.Input,
					}}
				}

			case llm.EventUsage:
				if ev.Usage != nil {
					totalUsage.InputTokens += ev.Usage.InputTokens
					totalUsage.OutputTokens += ev.Usage.OutputTokens
					totalUsage.CacheRead += ev.Usage.CacheRead
					totalUsage.CacheCreated += ev.Usage.CacheCreated
					ch <- TurnEvent{Type: TurnEventUsage, Usage: ev.Usage}
				}

			case llm.EventStop:
				stopReason = ev.StopReason

			case llm.EventError:
				ch <- TurnEvent{Type: TurnEventError, Err: ev.Err}
				return
			}
		}

		// Build the assistant message to add to history.
		assistantMsg := a.buildAssistantMessage(assistantText, toolCalls)
		messages = append(messages, assistantMsg)

		// Save to session.
		_ = a.session.Append(session.Record{
			Type:    "assistant",
			Content: assistantText,
			Model:   a.model,
			Usage: &session.UsageRecord{
				Input:  totalUsage.InputTokens,
				Output: totalUsage.OutputTokens,
			},
		})

		if len(toolCalls) == 0 || stopReason == "end_turn" {
			break
		}

		// Execute tool calls and collect results.
		toolResults := a.executeTools(ctx, toolCalls, ch)
		if toolResults == nil {
			return // context cancelled
		}

		// Append tool results as a user message.
		toolResultMsg := a.buildToolResultMessage(toolResults)
		messages = append(messages, toolResultMsg)

		if stopReason != "tool_use" {
			break
		}
	}

	ch <- TurnEvent{Type: TurnEventDone}
}

// executeTools runs each tool call, respecting permissions and hooks.
// When multiple calls are present they are fanned out in parallel goroutines;
// results are returned in the same order as the input calls slice.
func (a *Agent) executeTools(ctx context.Context, calls []llm.ToolUseEvent, ch chan<- TurnEvent) []toolResultEntry {
	if len(calls) == 0 {
		return nil
	}

	if len(calls) == 1 {
		entry := a.executeSingleTool(ctx, calls[0], ch)
		if entry == nil {
			return nil // context cancelled
		}
		return []toolResultEntry{*entry}
	}

	// Fan out: run all calls in parallel, preserving order.
	results := make([]toolResultEntry, len(calls))
	cancelled := make([]bool, len(calls))

	var wg sync.WaitGroup
	wg.Add(len(calls))

	for i, call := range calls {
		i, call := i, call // capture loop variables
		go func() {
			defer wg.Done()
			entry := a.executeSingleTool(ctx, call, ch)
			if entry == nil {
				cancelled[i] = true
				return
			}
			results[i] = *entry
		}()
	}

	wg.Wait()

	// If any goroutine saw a cancelled context, propagate nil.
	for _, c := range cancelled {
		if c {
			return nil
		}
	}

	return results
}

// executeSingleTool executes one tool call and emits events on ch.
// Returns nil when the context is cancelled before execution.
func (a *Agent) executeSingleTool(ctx context.Context, call llm.ToolUseEvent, ch chan<- TurnEvent) *toolResultEntry {
	select {
	case <-ctx.Done():
		return nil
	default:
	}

	// Check permission.
	level := a.permissions.Check(call.Name, call.Input)
	if level == permissions.LevelDeny {
		output := fmt.Sprintf("Permission denied for tool: %s", call.Name)
		ch <- TurnEvent{Type: TurnEventToolEnd, ToolResult: &ToolResult{
			ID: call.ID, Name: call.Name, Output: output,
			Err: fmt.Errorf("permission denied"),
		}}
		_ = a.session.Append(session.Record{
			Type: "tool_result", ID: call.ID, Name: call.Name, Content: output,
		})
		return &toolResultEntry{id: call.ID, name: call.Name, output: output}
	}

	if level == permissions.LevelAsk {
		respCh := make(chan bool, 1)
		ch <- TurnEvent{
			Type: TurnEventPermission,
			PermissionRequest: &PermissionRequest{
				ToolName:   call.Name,
				Input:      call.Input,
				ResponseCh: respCh,
			},
		}
		allowed, ok := <-respCh
		if !ok || !allowed {
			output := fmt.Sprintf("User denied execution of tool: %s", call.Name)
			ch <- TurnEvent{Type: TurnEventToolEnd, ToolResult: &ToolResult{
				ID: call.ID, Name: call.Name, Output: output,
			}}
			_ = a.session.Append(session.Record{
				Type: "tool_result", ID: call.ID, Name: call.Name, Content: output,
			})
			return &toolResultEntry{id: call.ID, name: call.Name, output: output}
		}
	}

	// Log tool call to session.
	_ = a.session.Append(session.Record{
		Type:  "tool_use",
		ID:    call.ID,
		Name:  call.Name,
		Input: call.Input,
	})

	// Pre-hooks.
	a.hooks.RunPre(ctx, call.Name, call.Input)

	// Execute the tool (check skills first, then tools).
	var output string
	var execErr error

	if skill, ok := a.skills.Get(call.Name); ok {
		output, execErr = skill.Execute(ctx, call.Input)
	} else if tool, ok := a.tools.Get(call.Name); ok {
		output, execErr = tool.Execute(ctx, call.Input)
	} else {
		execErr = fmt.Errorf("unknown tool or skill: %s", call.Name)
	}

	// Post-hooks.
	a.hooks.RunPost(ctx, call.Name, call.Input)

	// If the tool itself returned a context cancellation, propagate it as
	// a clean cancellation rather than a regular tool error.
	if errors.Is(execErr, context.Canceled) {
		return nil
	}

	if execErr != nil {
		output = fmt.Sprintf("Error: %s", execErr.Error())
	}

	ch <- TurnEvent{Type: TurnEventToolEnd, ToolResult: &ToolResult{
		ID: call.ID, Name: call.Name, Output: output, Err: execErr,
	}}

	_ = a.session.Append(session.Record{
		Type:      "tool_result",
		ToolUseID: call.ID,
		Name:      call.Name,
		Content:   output,
	})

	return &toolResultEntry{id: call.ID, name: call.Name, output: output}
}

func (a *Agent) buildMessages() []llm.Message {
	records := a.session.Records()
	var msgs []llm.Message

	for i := 0; i < len(records); {
		r := records[i]
		switch r.Type {
		case "user":
			// Collect consecutive user + tool_result records into one user message.
			var blocks []llm.ContentBlock
			blocks = append(blocks, llm.ContentBlock{Type: "text", Text: r.Content})
			i++
			// Collect any tool_result records that follow.
			for i < len(records) && records[i].Type == "tool_result" {
				tr := records[i]
				blocks = append(blocks, llm.ContentBlock{
					Type:      "tool_result",
					ToolUseID: tr.ToolUseID,
					Content:   tr.Content,
				})
				i++
			}
			msgs = append(msgs, llm.Message{Role: "user", Content: blocks})

		case "assistant":
			var blocks []llm.ContentBlock
			if r.Content != "" {
				blocks = append(blocks, llm.ContentBlock{Type: "text", Text: r.Content})
			}
			i++
			// Collect any tool_use records that follow.
			for i < len(records) && records[i].Type == "tool_use" {
				tu := records[i]
				blocks = append(blocks, llm.ContentBlock{
					Type:  "tool_use",
					ID:    tu.ID,
					Name:  tu.Name,
					Input: tu.Input,
				})
				i++
			}
			msgs = append(msgs, llm.Message{Role: "assistant", Content: blocks})
			// Collect any tool_result records that follow the tool_uses into a user message.
			if i < len(records) && records[i].Type == "tool_result" {
				var resultBlocks []llm.ContentBlock
				for i < len(records) && records[i].Type == "tool_result" {
					tr := records[i]
					resultBlocks = append(resultBlocks, llm.ContentBlock{
						Type:      "tool_result",
						ToolUseID: tr.ToolUseID,
						Content:   tr.Content,
					})
					i++
				}
				msgs = append(msgs, llm.Message{Role: "user", Content: resultBlocks})
			}

		default:
			i++
		}
	}

	return msgs
}

func (a *Agent) buildAssistantMessage(text string, toolCalls []llm.ToolUseEvent) llm.Message {
	var blocks []llm.ContentBlock
	if text != "" {
		blocks = append(blocks, llm.ContentBlock{Type: "text", Text: text})
	}
	for _, tc := range toolCalls {
		blocks = append(blocks, llm.ContentBlock{
			Type:  "tool_use",
			ID:    tc.ID,
			Name:  tc.Name,
			Input: tc.Input,
		})
	}
	return llm.Message{Role: "assistant", Content: blocks}
}

type toolResultEntry struct {
	id     string
	name   string
	output string
}

func (a *Agent) buildToolResultMessage(results []toolResultEntry) llm.Message {
	blocks := make([]llm.ContentBlock, 0, len(results))
	for _, r := range results {
		blocks = append(blocks, llm.ContentBlock{
			Type:      "tool_result",
			ToolUseID: r.id,
			Content:   r.output,
		})
	}
	return llm.Message{Role: "user", Content: blocks}
}

func (a *Agent) allSchemas() []llm.ToolSchema {
	schemas := a.tools.Schemas()
	schemas = append(schemas, a.skills.Schemas()...)
	return schemas
}

// streamWithRetry calls provider.Stream with exponential backoff on transient errors.
// Retries up to 3 times on timeout / rate-limit / overload responses.
func streamWithRetry(ctx context.Context, p llm.Provider, req llm.Request) (<-chan llm.Event, error) {
	const maxRetries = 3
	backoff := time.Second

	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff):
			}
			backoff *= 2
		}

		ch, err := p.Stream(ctx, req)
		if err == nil {
			return ch, nil
		}
		lastErr = err

		if !isRetryable(err) {
			return nil, err
		}
	}
	return nil, fmt.Errorf("LLM request failed after %d retries: %w", maxRetries, lastErr)
}

// isRetryable returns true for transient errors worth retrying.
func isRetryable(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, keyword := range []string{"timeout", "429", "529", "503", "overloaded", "rate limit", "too many requests"} {
		if strings.Contains(msg, keyword) {
			return true
		}
	}
	return false
}
