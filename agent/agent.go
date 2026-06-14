// Package agent is the policy-free core of the harness: the neutral
// conversation types and the loop that drives a model through tool use.
//
// The core never prints, never reads input, and never decides whether an
// action is allowed. Rendering and oversight are injected by the caller
// through Hooks; tools are pure actions supplied as values. Everything that
// is a policy (gates, output modes, prompts, sessions) lives at the edge.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
)

// ---------------------------------------------------------------------------
// Neutral types: the harness's own vocabulary, owned by no provider.
// ---------------------------------------------------------------------------

type ToolCall struct {
	ID   string
	Name string
	Args json.RawMessage
}

type ToolResult struct {
	ID      string
	Content string
	IsError bool
}

// Turn is one entry of conversation history.
// Role is "user" (text), "assistant" (text and/or tool calls), or "tool" (results).
type Turn struct {
	Role    string       `json:"role"`
	Text    string       `json:"text,omitempty"`
	Calls   []ToolCall   `json:"calls,omitempty"`
	Results []ToolResult `json:"results,omitempty"`
}

type ToolDef struct {
	Name        string
	Description string
	Schema      map[string]any // a plain JSON Schema object
}

// Usage is what one model call cost, when the provider reports it. Input,
// Output, and CacheRead accumulate across a turn's calls; LastInput is the
// final call's full prompt size (input plus cache reads), which is the
// current context size the caller can report or act on.
type Usage struct {
	Input     int
	Output    int
	CacheRead int // input tokens served from the prompt cache at ~10% price
	LastInput int
}

func (u Usage) Add(v Usage) Usage {
	return Usage{u.Input + v.Input, u.Output + v.Output, u.CacheRead + v.CacheRead, u.LastInput}
}

type Reply struct {
	Text  string
	Calls []ToolCall
	Usage Usage
}

// Provider is one model call: streamed via the callbacks, the full reply
// returned at the end. onThink receives reasoning deltas from thinking
// models; they are display-only and never enter history.
type Provider interface {
	Chat(ctx context.Context, system string, history []Turn, tools []ToolDef, onText, onThink func(string)) (Reply, error)
}

// Tool is a definition plus the action that runs it. Tools are pure:
// approval is the Gate's job, rendering is the hooks' job. The context is the
// turn's: a cancelled turn must stop a long-running tool, not wait for it.
//
// Parallel declares the action safe to run concurrently with other Parallel
// tools (it observes; it does not mutate shared state). Which tools qualify is
// the caller's policy; the core only provides the mechanism: when one reply
// carries several calls and every one is Parallel, they run concurrently.
type Tool struct {
	Def      ToolDef
	Run      func(ctx context.Context, args json.RawMessage) (result string, isErr bool)
	Parallel bool
}

// Hooks is the core's entire extension surface. Any field may be nil.
type Hooks struct {
	OnText      func(string)               // assistant text deltas
	OnThink     func(string)               // reasoning deltas (display-only)
	OnToolStart func(ToolCall)             // a tool call is about to run
	OnToolEnd   func(ToolCall, ToolResult) // a tool call finished
	// Gate decides whether a tool call may run. A non-nil error declines it
	// and the error text is returned to the model as the tool result, so
	// word it as something the model can act on. Nil Gate allows everything.
	Gate func(ToolCall) error
}

func (h Hooks) withDefaults() Hooks {
	nop := func(string) {}
	if h.OnText == nil {
		h.OnText = nop
	}
	if h.OnThink == nil {
		h.OnThink = nop
	}
	if h.OnToolStart == nil {
		h.OnToolStart = func(ToolCall) {}
	}
	if h.OnToolEnd == nil {
		h.OnToolEnd = func(ToolCall, ToolResult) {}
	}
	if h.Gate == nil {
		h.Gate = func(ToolCall) error { return nil }
	}
	return h
}

// maxParallelTools bounds how many Parallel tool calls run at once.
const maxParallelTools = 4

// runCalls executes one reply's tool calls. When every call is to a Parallel
// tool, they run concurrently (gates and start hooks fire first, in call
// order; end hooks fire after the batch, in call order, so rendering never
// interleaves). Any other batch keeps the sequential start/run/end cadence,
// which is what an approval gate in the middle of it expects.
func runCalls(ctx context.Context, calls []ToolCall, byName map[string]Tool, h Hooks) []ToolResult {
	parallel := len(calls) > 1
	for _, c := range calls {
		if t, ok := byName[c.Name]; !ok || !t.Parallel {
			parallel = false
			break
		}
	}

	if !parallel {
		var results []ToolResult
		for _, c := range calls {
			h.OnToolStart(c)
			var out string
			var isErr bool
			switch tool, ok := byName[c.Name]; {
			case !ok:
				out, isErr = "unknown tool: "+c.Name, true
			default:
				if gateErr := h.Gate(c); gateErr != nil {
					out, isErr = gateErr.Error(), true
				} else {
					out, isErr = tool.Run(ctx, c.Args)
				}
			}
			r := ToolResult{ID: c.ID, Content: truncate(out), IsError: isErr}
			h.OnToolEnd(c, r)
			results = append(results, r)
		}
		return results
	}

	results := make([]ToolResult, len(calls))
	var wg sync.WaitGroup
	sem := make(chan struct{}, maxParallelTools)
	for i, c := range calls {
		h.OnToolStart(c)
		if gateErr := h.Gate(c); gateErr != nil {
			results[i] = ToolResult{ID: c.ID, Content: gateErr.Error(), IsError: true}
			continue
		}
		wg.Add(1)
		go func(i int, c ToolCall, tool Tool) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			out, isErr := tool.Run(ctx, c.Args)
			results[i] = ToolResult{ID: c.ID, Content: truncate(out), IsError: isErr}
		}(i, c, byName[c.Name])
	}
	wg.Wait()
	for i, c := range calls {
		h.OnToolEnd(c, results[i])
	}
	return results
}

// maxResultChars caps what any one tool result may add to the context window.
const maxResultChars = 30000

func truncate(s string) string {
	if len(s) <= maxResultChars {
		return s
	}
	return s[:maxResultChars] + fmt.Sprintf("\n... [truncated %d bytes]", len(s)-maxResultChars)
}

// Run drives one user turn to completion: call the model, run any tool calls
// it asked for, append the results, and go around again. A reply with no
// tool calls means the turn is done. The returned history includes every
// assistant and tool turn produced, even when an error cut the turn short.
func Run(ctx context.Context, p Provider, system string, history []Turn, tools []Tool, h Hooks) ([]Turn, Usage, error) {
	h = h.withDefaults()
	defs := make([]ToolDef, len(tools))
	byName := make(map[string]Tool, len(tools))
	for i, t := range tools {
		defs[i] = t.Def
		byName[t.Def.Name] = t
	}

	var spent Usage
	for {
		reply, err := p.Chat(ctx, system, history, defs, h.OnText, h.OnThink)
		if err != nil {
			return history, spent, err
		}
		spent = spent.Add(reply.Usage)
		spent.LastInput = reply.Usage.Input + reply.Usage.CacheRead
		history = append(history, Turn{Role: "assistant", Text: reply.Text, Calls: reply.Calls})

		if len(reply.Calls) == 0 {
			return history, spent, nil
		}

		history = append(history, Turn{Role: "tool", Results: runCalls(ctx, reply.Calls, byName, h)})
	}
}
