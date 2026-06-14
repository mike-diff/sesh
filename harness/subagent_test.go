package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"sesh/agent"
)

// script replays a fixed sequence of replies, one per Chat call.
type script struct {
	replies []agent.Reply
	i       int
}

func (s *script) Chat(_ context.Context, _ string, _ []agent.Turn, _ []agent.ToolDef, onText, _ func(string)) (agent.Reply, error) {
	if s.i >= len(s.replies) {
		return agent.Reply{}, errors.New("script exhausted")
	}
	r := s.replies[s.i]
	s.i++
	onText(r.Text)
	return r, nil
}

func runTask(t *testing.T, p agent.Provider, prompt string, gate func(agent.ToolCall) error, report func(agent.Usage)) (string, bool) {
	t.Helper()
	tool := taskTool(func() agent.Provider { return p }, func() *Session { return &Session{ID: "task-test"} }, 1, false, gate, report)
	return tool.Run(context.Background(), json.RawMessage(fmt.Sprintf(`{"prompt":%q}`, prompt)))
}

// TestTaskReturnsFinalReport: the child's last assistant text is the tool
// result, and the reported usage reaches the caller.
func TestTaskReturnsFinalReport(t *testing.T) {
	p := &script{replies: []agent.Reply{
		{Text: "CHILD REPORT: found it in repl.go:42", Usage: agent.Usage{Input: 100, Output: 20}},
	}}
	var got agent.Usage
	out, isErr := runTask(t, p, "find the thing", nil, func(u agent.Usage) { got = u })
	if isErr || out != "CHILD REPORT: found it in repl.go:42" {
		t.Fatalf("task result %q err=%v", out, isErr)
	}
	if got.Input != 100 || got.Output != 20 {
		t.Fatalf("usage not reported: %+v", got)
	}
}

// TestTaskChildToolset: children observe and run gated bash, never write/edit;
// nesting stops at maxTaskDepth by absence of the task tool.
func TestTaskChildToolset(t *testing.T) {
	names := func(tools []agent.Tool) string {
		var n []string
		for _, tl := range tools {
			n = append(n, tl.Def.Name)
		}
		return strings.Join(n, ",")
	}
	sess := func() *Session { return &Session{ID: "ct-test"} }
	mid := childTools(nil, sess, 1, false, nil, nil)
	if got := names(mid); got != "read,search,bash,recall,task" {
		t.Fatalf("depth-1 child toolset: %s", got)
	}
	deepest := childTools(nil, sess, tune.TaskDepth, false, nil, nil)
	if got := names(deepest); got != "read,search,bash,recall" {
		t.Fatalf("deepest child must not spawn further tasks: %s", got)
	}
}

// TestTaskChildGate: a child's bash call runs under the parent's gate; a
// decline comes back to the child as a tool error, not a crash, and the child
// can still finish its report.
func TestTaskChildGate(t *testing.T) {
	p := &script{replies: []agent.Reply{
		{Calls: []agent.ToolCall{{ID: "1", Name: "bash", Args: json.RawMessage(`{"command":"rm -rf /"}`)}}},
		{Text: "could not run the command; report based on reads only"},
	}}
	denied := 0
	deny := func(c agent.ToolCall) error {
		if mutating[c.Name] {
			denied++
			return errors.New("declined")
		}
		return nil
	}
	out, isErr := runTask(t, p, "check something", deny, nil)
	if isErr || !strings.Contains(out, "report based on reads") {
		t.Fatalf("child should recover from a declined gate: %q err=%v", out, isErr)
	}
	if denied != 1 {
		t.Fatalf("gate saw %d mutating calls, want 1", denied)
	}
}

// TestTaskErrors: bad input, missing provider, child API failure, and an empty
// report are all model-readable errors, never crashes.
func TestTaskErrors(t *testing.T) {
	if out, isErr := runTask(t, &script{}, "", nil, nil); !isErr || !strings.Contains(out, "non-empty prompt") {
		t.Fatalf("empty prompt: %q err=%v", out, isErr)
	}
	tool := taskTool(func() agent.Provider { return nil }, nil, 1, false, nil, nil)
	if out, isErr := tool.Run(context.Background(), json.RawMessage(`{"prompt":"x"}`)); !isErr || !strings.Contains(out, "no active provider") {
		t.Fatalf("nil provider: %q err=%v", out, isErr)
	}
	if out, isErr := runTask(t, &script{}, "x", nil, nil); !isErr || !strings.Contains(out, "subagent failed") {
		t.Fatalf("provider error: %q err=%v", out, isErr)
	}
	if out, isErr := runTask(t, &script{replies: []agent.Reply{{Text: ""}}}, "x", nil, nil); !isErr || !strings.Contains(out, "without a final report") {
		t.Fatalf("empty report: %q err=%v", out, isErr)
	}
}

// TestTaskCancellation: cancelling the turn context stops the child loop.
func TestTaskCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	blocked := blockingProvider{ctx: ctx}
	tool := taskTool(func() agent.Provider { return blocked }, nil, 1, false, nil, nil)
	out, isErr := tool.Run(ctx, json.RawMessage(`{"prompt":"x"}`))
	if !isErr || !strings.Contains(out, "subagent failed") {
		t.Fatalf("cancelled child should surface as a tool error: %q err=%v", out, isErr)
	}
}

// blockingProvider returns the context's error, standing in for an HTTP call
// that honors cancellation.
type blockingProvider struct{ ctx context.Context }

func (b blockingProvider) Chat(ctx context.Context, _ string, _ []agent.Turn, _ []agent.ToolDef, _, _ func(string)) (agent.Reply, error) {
	<-ctx.Done()
	return agent.Reply{}, ctx.Err()
}

// TestTruncationVerdict: the doctor's clamp diagnosis from probe numbers.
func TestTruncationVerdict(t *testing.T) {
	if v := truncationVerdict(4096, 6000); !strings.Contains(v, "truncated") || !strings.Contains(v, "OLLAMA_CONTEXT_LENGTH") {
		t.Fatalf("clamped endpoint must be diagnosed with the fix named: %q", v)
	}
	if v := truncationVerdict(5900, 6000); v != "" {
		t.Fatalf("full acceptance is healthy: %q", v)
	}
	if v := truncationVerdict(0, 6000); v != "" {
		t.Fatalf("no usage reported is inconclusive, not a failure: %q", v)
	}
}

// TestProbeContext: the probe sends real bulk and reads back what the
// endpoint claims it received.
func TestProbeContext(t *testing.T) {
	got, sent, err := probeContext(context.Background(), usageChat{input: 4096})
	if err != nil || got != 4096 || sent != probePadWords {
		t.Fatalf("probe: got=%d sent=%d err=%v", got, sent, err)
	}
	if !strings.Contains(usageLastPrompt, "alpha bravo") || len(usageLastPrompt) < probePadWords*4 {
		t.Fatalf("probe prompt must carry the padding bulk: %d chars", len(usageLastPrompt))
	}
}

var usageLastPrompt string

// usageChat reports a fixed input-token usage, standing in for a clamping server.
type usageChat struct{ input int }

func (u usageChat) Chat(_ context.Context, _ string, h []agent.Turn, _ []agent.ToolDef, onText, _ func(string)) (agent.Reply, error) {
	usageLastPrompt = h[len(h)-1].Text
	onText("OK")
	return agent.Reply{Text: "OK", Usage: agent.Usage{Input: u.input}}, nil
}
