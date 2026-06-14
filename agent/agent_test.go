package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// scripted is a Provider that replays a fixed list of replies, recording the
// tool defs it was handed so a test can assert the core passes them through.
type scripted struct {
	replies  []Reply
	i        int
	gotTools []ToolDef
}

func (s *scripted) Chat(_ context.Context, _ string, _ []Turn, tools []ToolDef, onText, onThink func(string)) (Reply, error) {
	s.gotTools = tools
	r := s.replies[s.i]
	s.i++
	onText(r.Text)
	return r, nil
}

func tool(name string, run func(json.RawMessage) (string, bool)) Tool {
	return Tool{Def: ToolDef{Name: name}, Run: func(_ context.Context, raw json.RawMessage) (string, bool) { return run(raw) }}
}

// TestRunLoop: a tool call is executed, its result fed back, and the second
// reply (no calls) ends the turn. History and usage must reflect every step.
func TestRunLoop(t *testing.T) {
	ran := false
	tools := []Tool{tool("touch", func(json.RawMessage) (string, bool) { ran = true; return "ok", false })}
	p := &scripted{replies: []Reply{
		{Calls: []ToolCall{{ID: "1", Name: "touch", Args: json.RawMessage(`{}`)}}, Usage: Usage{Input: 10, Output: 5}},
		{Text: "done", Usage: Usage{Input: 8, Output: 2}},
	}}

	var started, ended int
	h := Hooks{
		OnToolStart: func(ToolCall) { started++ },
		OnToolEnd:   func(ToolCall, ToolResult) { ended++ },
	}
	hist, used, err := Run(context.Background(), p, "sys", []Turn{{Role: "user", Text: "go"}}, tools, h)
	if err != nil {
		t.Fatal(err)
	}
	if !ran {
		t.Fatal("tool never ran")
	}
	if started != 1 || ended != 1 {
		t.Fatalf("hooks: started=%d ended=%d", started, ended)
	}
	// user, assistant(call), tool(result), assistant(final)
	if len(hist) != 4 || hist[3].Text != "done" {
		t.Fatalf("history shape: %+v", hist)
	}
	if hist[2].Role != "tool" || hist[2].Results[0].Content != "ok" {
		t.Fatalf("tool turn: %+v", hist[2])
	}
	if used.Input != 18 || used.Output != 7 {
		t.Fatalf("usage not summed: %+v", used)
	}
	if used.LastInput != 8 {
		t.Fatalf("LastInput should be the final call's prompt size: %+v", used)
	}
	if len(p.gotTools) != 1 || p.gotTools[0].Name != "touch" {
		t.Fatalf("tool defs not passed through: %+v", p.gotTools)
	}
}

// TestGateDenies: a non-nil Gate blocks the tool, its error reaches the model
// as the result, and the tool body never runs.
func TestGateDenies(t *testing.T) {
	ran := false
	tools := []Tool{tool("danger", func(json.RawMessage) (string, bool) { ran = true; return "did it", false })}
	p := &scripted{replies: []Reply{
		{Calls: []ToolCall{{ID: "1", Name: "danger", Args: json.RawMessage(`{}`)}}},
		{Text: "ok"},
	}}
	h := Hooks{Gate: func(ToolCall) error { return errors.New("nope, declined") }}
	hist, _, err := Run(context.Background(), p, "", []Turn{{Role: "user", Text: "x"}}, tools, h)
	if err != nil {
		t.Fatal(err)
	}
	if ran {
		t.Fatal("denied tool should not have run")
	}
	r := hist[2].Results[0]
	if !r.IsError || !strings.Contains(r.Content, "declined") {
		t.Fatalf("gate error not surfaced: %+v", r)
	}
}

// TestUnknownTool: a call for a tool that was never registered is reported
// back to the model rather than crashing the loop.
func TestUnknownTool(t *testing.T) {
	p := &scripted{replies: []Reply{
		{Calls: []ToolCall{{ID: "1", Name: "ghost", Args: json.RawMessage(`{}`)}}},
		{Text: "ok"},
	}}
	hist, _, err := Run(context.Background(), p, "", []Turn{{Role: "user", Text: "x"}}, nil, Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	if r := hist[2].Results[0]; !r.IsError || !strings.Contains(r.Content, "unknown tool") {
		t.Fatalf("unknown tool not reported: %+v", r)
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate(strings.Repeat("z", maxResultChars+10)); len(got) > maxResultChars+50 || !strings.Contains(got, "truncated") {
		t.Fatal("truncate did not cap")
	}
	if got := truncate("short"); got != "short" {
		t.Fatalf("truncate altered a small string: %q", got)
	}
}

// ptool builds a Parallel tool for the concurrency tests.
func ptool(name string, run func(json.RawMessage) (string, bool)) Tool {
	t := tool(name, run)
	t.Parallel = true
	return t
}

// TestParallelCalls: an all-Parallel batch truly runs concurrently (the two
// tools rendezvous, which deadlocks under sequential execution), results stay
// in call order, and end hooks fire in call order after the batch.
func TestParallelCalls(t *testing.T) {
	meetA, meetB := make(chan struct{}), make(chan struct{})
	rendezvous := func(mine, theirs chan struct{}) (string, bool) {
		close(mine)
		select {
		case <-theirs:
			return "met", false
		case <-time.After(5 * time.Second):
			return "never met: calls ran sequentially", true
		}
	}
	tools := []Tool{
		ptool("a", func(json.RawMessage) (string, bool) { return rendezvous(meetA, meetB) }),
		ptool("b", func(json.RawMessage) (string, bool) { return rendezvous(meetB, meetA) }),
	}
	p := &scripted{replies: []Reply{
		{Calls: []ToolCall{
			{ID: "1", Name: "a", Args: json.RawMessage(`{}`)},
			{ID: "2", Name: "b", Args: json.RawMessage(`{}`)},
		}},
		{Text: "done"},
	}}
	var ends []string
	h := Hooks{OnToolEnd: func(c ToolCall, _ ToolResult) { ends = append(ends, c.Name) }}
	hist, _, err := Run(context.Background(), p, "sys", []Turn{{Role: "user", Text: "go"}}, tools, h)
	if err != nil {
		t.Fatal(err)
	}
	results := hist[2].Results
	if results[0].ID != "1" || results[1].ID != "2" {
		t.Fatalf("results out of call order: %+v", results)
	}
	for _, r := range results {
		if r.IsError || r.Content != "met" {
			t.Fatalf("parallel batch did not run concurrently: %+v", r)
		}
	}
	if len(ends) != 2 || ends[0] != "a" || ends[1] != "b" {
		t.Fatalf("end hooks must fire in call order: %v", ends)
	}
}

// TestMixedBatchSequential: one non-Parallel call in the batch keeps the whole
// batch sequential, never two tools in flight at once.
func TestMixedBatchSequential(t *testing.T) {
	var inFlight, maxFlight int32
	track := func(json.RawMessage) (string, bool) {
		n := atomic.AddInt32(&inFlight, 1)
		for {
			m := atomic.LoadInt32(&maxFlight)
			if n <= m || atomic.CompareAndSwapInt32(&maxFlight, m, n) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		atomic.AddInt32(&inFlight, -1)
		return "ok", false
	}
	tools := []Tool{ptool("obs", track), tool("mut", track)} // mut is not Parallel
	p := &scripted{replies: []Reply{
		{Calls: []ToolCall{
			{ID: "1", Name: "obs", Args: json.RawMessage(`{}`)},
			{ID: "2", Name: "mut", Args: json.RawMessage(`{}`)},
			{ID: "3", Name: "obs", Args: json.RawMessage(`{}`)},
		}},
		{Text: "done"},
	}}
	if _, _, err := Run(context.Background(), p, "sys", []Turn{{Role: "user", Text: "go"}}, tools, Hooks{}); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&maxFlight); got != 1 {
		t.Fatalf("mixed batch overlapped: max %d tools in flight", got)
	}
}

// TestParallelGateStillApplies: a gate decline inside an all-Parallel batch
// turns into an ordered error result without running that tool.
func TestParallelGateStillApplies(t *testing.T) {
	ran := map[string]bool{}
	var mu sync.Mutex
	mark := func(name string) func(json.RawMessage) (string, bool) {
		return func(json.RawMessage) (string, bool) {
			mu.Lock()
			ran[name] = true
			mu.Unlock()
			return "ok", false
		}
	}
	tools := []Tool{ptool("a", mark("a")), ptool("b", mark("b"))}
	p := &scripted{replies: []Reply{
		{Calls: []ToolCall{
			{ID: "1", Name: "a", Args: json.RawMessage(`{}`)},
			{ID: "2", Name: "b", Args: json.RawMessage(`{}`)},
		}},
		{Text: "done"},
	}}
	h := Hooks{Gate: func(c ToolCall) error {
		if c.Name == "b" {
			return errors.New("declined")
		}
		return nil
	}}
	hist, _, err := Run(context.Background(), p, "sys", []Turn{{Role: "user", Text: "go"}}, tools, h)
	if err != nil {
		t.Fatal(err)
	}
	results := hist[2].Results
	if !ran["a"] || ran["b"] {
		t.Fatalf("gate must skip only the declined call: ran=%v", ran)
	}
	if !results[1].IsError || !strings.Contains(results[1].Content, "declined") {
		t.Fatalf("declined call must yield an ordered error result: %+v", results[1])
	}
}
