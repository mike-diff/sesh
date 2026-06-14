package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mike-diff/sesh/agent"
)

func TestCallAccum(t *testing.T) {
	// OpenAI-style fragments: id/name first, args drip-fed, out-of-order indexes
	a := newCallAccum()
	a.upsert(1, "", "", `{"b":`)
	a.upsert(1, "call_b", "write", ``)
	a.upsert(1, "", "", `2}`)
	a.upsert(0, "", "read", `{"a":1}`)
	calls := a.collect()
	if len(calls) != 2 {
		t.Fatalf("got %d calls", len(calls))
	}
	if calls[0].Name != "read" || calls[0].ID != "call_0" || string(calls[0].Args) != `{"a":1}` {
		t.Fatalf("call 0: %+v", calls[0])
	}
	if calls[1].ID != "call_b" || string(calls[1].Args) != `{"b":2}` {
		t.Fatalf("call 1: %+v", calls[1])
	}

	// Anthropic-style: appendArgs must ignore blocks that never started,
	// and empty args become {}
	b := newCallAccum()
	b.appendArgs(5, `{"ignored":true}`)
	b.upsert(2, "toolu_1", "bash", "")
	if calls := b.collect(); len(calls) != 1 || string(calls[0].Args) != "{}" {
		t.Fatalf("anthropic accum: %+v", calls)
	}
}

func TestParseAPIError(t *testing.T) {
	e := parseAPIError(429, map[string][]string{"Retry-After": {"7"}}, []byte(`{"error":{"message":"slow down"}}`))
	if e.Status != 429 || e.Message != "slow down" || e.RetryAfter.Seconds() != 7 {
		t.Fatalf("parsed: %+v", e)
	}
	if !e.retryable() {
		t.Fatal("429 should be retryable")
	}
	if parseAPIError(400, nil, []byte("bad")).retryable() {
		t.Fatal("400 should not be retryable")
	}
}

// sseServer returns an httptest server that captures the request and streams
// the given SSE data lines back.
func sseServer(t *testing.T, gotBody *map[string]any, gotHeader *http.Header, events ...string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, gotBody)
		*gotHeader = r.Header.Clone()
		w.Header().Set("Content-Type", "text/event-stream")
		for _, ev := range events {
			fmt.Fprintf(w, "data: %s\n\n", ev)
		}
	}))
}

// TestAnthropicChat pins the wire shape (cache_control on blocks, never
// top-level) and the SSE parse: text deltas stream, tool input accumulates,
// usage is captured.
func TestAnthropicChat(t *testing.T) {
	var body map[string]any
	var hdr http.Header
	srv := sseServer(t, &body, &hdr,
		`{"type":"message_start","message":{"usage":{"input_tokens":42,"cache_read_input_tokens":40}}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"tu1","name":"read"}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"path\":\"x\"}"}}`,
		`{"type":"message_delta","usage":{"output_tokens":7}}`,
	)
	defer srv.Close()

	p := Anthropic{BaseURL: srv.URL, Key: "k", Model: "m"}
	var streamed strings.Builder
	reply, err := p.Chat(context.Background(), "SYS",
		[]agent.Turn{{Role: "user", Text: "hello"}},
		[]agent.ToolDef{{Name: "read", Schema: map[string]any{"type": "object"}}},
		func(s string) { streamed.WriteString(s) }, func(string) {})
	if err != nil {
		t.Fatal(err)
	}

	// request shape: auth header, no top-level cache_control
	if hdr.Get("x-api-key") != "k" {
		t.Fatal("missing x-api-key header")
	}
	if _, ok := body["cache_control"]; ok {
		t.Fatal("cache_control must not be a top-level request field")
	}
	// system is a block array whose block carries the cache breakpoint
	sys, ok := body["system"].([]any)
	if !ok || len(sys) != 1 {
		t.Fatalf("system should be a block array, got %T", body["system"])
	}
	if blk := sys[0].(map[string]any); blk["cache_control"] == nil || blk["text"] != "SYS" {
		t.Fatalf("system block missing cache_control: %v", blk)
	}
	// the last message's final block carries the history breakpoint
	msgs := body["messages"].([]any)
	lastContent := msgs[len(msgs)-1].(map[string]any)["content"].([]any)
	if lastBlk := lastContent[len(lastContent)-1].(map[string]any); lastBlk["cache_control"] == nil {
		t.Fatalf("last message block missing cache_control: %v", lastBlk)
	}

	// reply parse
	if reply.Text != "hi" || streamed.String() != "hi" {
		t.Fatalf("text: %q streamed %q", reply.Text, streamed.String())
	}
	if len(reply.Calls) != 1 || reply.Calls[0].ID != "tu1" || reply.Calls[0].Name != "read" || string(reply.Calls[0].Args) != `{"path":"x"}` {
		t.Fatalf("calls: %+v", reply.Calls)
	}
	if reply.Usage.Input != 42 || reply.Usage.Output != 7 || reply.Usage.CacheRead != 40 {
		t.Fatalf("usage: %+v", reply.Usage)
	}
}

// TestOpenAIChat pins the chat-completions request shape and the fragment
// accumulation of streamed tool calls.
func TestOpenAIChat(t *testing.T) {
	var body map[string]any
	var hdr http.Header
	srv := sseServer(t, &body, &hdr,
		`{"choices":[{"delta":{"content":"he"}}]}`,
		`{"choices":[{"delta":{"content":"y"}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"read","arguments":"{\"pa"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"th\":\"x\"}"}}]}}]}`,
		`{"usage":{"prompt_tokens":10,"completion_tokens":3}}`,
		`[DONE]`,
	)
	defer srv.Close()

	p := OpenAI{BaseURL: srv.URL, Key: "k", Model: "m"}
	reply, err := p.Chat(context.Background(), "SYS",
		[]agent.Turn{{Role: "user", Text: "hello"}},
		[]agent.ToolDef{{Name: "read", Schema: map[string]any{"type": "object"}}},
		func(string) {}, func(string) {})
	if err != nil {
		t.Fatal(err)
	}

	if hdr.Get("Authorization") != "Bearer k" {
		t.Fatal("missing bearer auth")
	}
	msgs := body["messages"].([]any)
	if first := msgs[0].(map[string]any); first["role"] != "system" || first["content"] != "SYS" {
		t.Fatalf("system message: %v", first)
	}
	if reply.Text != "hey" {
		t.Fatalf("text: %q", reply.Text)
	}
	if len(reply.Calls) != 1 || reply.Calls[0].ID != "c1" || string(reply.Calls[0].Args) != `{"path":"x"}` {
		t.Fatalf("calls: %+v", reply.Calls)
	}
	if reply.Usage.Input != 10 || reply.Usage.Output != 3 {
		t.Fatalf("usage: %+v", reply.Usage)
	}
}

// TestOpenAICachedTokens: the adapter reads prompt_tokens_details.cached_tokens
// and keeps the neutral semantics (Input excludes the cached subset, so
// Input+CacheRead is the full prompt, exactly as the Anthropic adapter reports
// it). Breaker: drop the details decode and CacheRead comes back 0 with Input
// at the full 1000.
func TestOpenAICachedTokens(t *testing.T) {
	var body map[string]any
	var hdr http.Header
	srv := sseServer(t, &body, &hdr,
		`{"choices":[{"delta":{"content":"ok"}}]}`,
		`{"usage":{"prompt_tokens":1000,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":900}}}`,
		`[DONE]`,
	)
	defer srv.Close()

	p := OpenAI{BaseURL: srv.URL, Key: "k", Model: "m"}
	reply, err := p.Chat(context.Background(), "SYS",
		[]agent.Turn{{Role: "user", Text: "hello"}}, nil,
		func(string) {}, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	if reply.Usage.Input != 100 || reply.Usage.CacheRead != 900 {
		t.Fatalf("usage %+v, want Input 100 (uncached only) and CacheRead 900", reply.Usage)
	}
}

// TestListModelInfos: discovery parses the context-length field names the
// wild uses, and models without one report 0.
func TestListModelInfos(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"data":[
			{"id":"plain"},
			{"id":"router","context_length":202752},
			{"id":"vllm","max_model_len":131072},
			{"id":"gateway","context_window":65536}
		]}`)
	}))
	defer srv.Close()

	infos, err := OpenAI{BaseURL: srv.URL, Model: "x"}.ListModelInfos(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{"plain": 0, "router": 202752, "vllm": 131072, "gateway": 65536}
	if len(infos) != len(want) {
		t.Fatalf("got %d models", len(infos))
	}
	for _, m := range infos {
		if want[m.ID] != m.Context {
			t.Fatalf("%s: context %d, want %d", m.ID, m.Context, want[m.ID])
		}
	}
	models, err := OpenAI{BaseURL: srv.URL, Model: "x"}.ListModels(context.Background())
	if err != nil || len(models) != 4 || models[0] != "gateway" {
		t.Fatalf("ListModels must keep returning sorted ids: %v err=%v", models, err)
	}
}

// TestSSECancel: a cancelled context stops the stream at once instead of
// draining buffered events. Breaker: drop the ctx check in sse and every
// buffered "data:" line is still delivered after the turn is cancelled.
func TestSSECancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the turn is already cancelled when sse starts reading
	body := strings.NewReader("data: a\n\ndata: b\n\ndata: c\n\n")
	delivered := 0
	err := sse(ctx, body, func([]byte) error { delivered++; return nil })
	if delivered != 0 {
		t.Fatalf("a cancelled stream must deliver no buffered events, delivered %d", delivered)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("sse must report the cancellation, got %v", err)
	}
}
