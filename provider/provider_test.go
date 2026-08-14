package provider

import (
	"context"
	"encoding/base64"
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

// TestAnthropicImageContent: a user Turn carrying an Image serializes its
// content as a block array (a text block when there is text, then a bare-base64
// image block), while a text-only turn keeps the plain-string content exactly as
// before. Breaker: remove the len(t.Images)>0 branch and the image turn falls
// through to a string content with no image block, failing the block assertions.
func TestAnthropicImageContent(t *testing.T) {
	var body map[string]any
	var hdr http.Header
	srv := sseServer(t, &body, &hdr,
		`{"type":"message_start","message":{"usage":{"input_tokens":1}}}`,
	)
	defer srv.Close()

	p := Anthropic{BaseURL: srv.URL, Key: "k", Model: "m"}
	_, err := p.Chat(context.Background(), "SYS",
		[]agent.Turn{
			{Role: "user", Text: "plain"},
			{Role: "user", Text: "look", Images: []agent.Image{
				{MediaType: "image/png", Data: []byte("PNGBYTES")},
			}},
		}, nil, func(string) {}, func(string) {})
	if err != nil {
		t.Fatal(err)
	}

	msgs := body["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("got %d messages", len(msgs))
	}

	// text-only turn: content stays a bare string (no regression)
	if c := msgs[0].(map[string]any)["content"]; c != "plain" {
		t.Fatalf("text-only content must stay the bare string, got %T %v", c, c)
	}

	// image turn: content is a block array, text block first, then image block
	blocks := msgs[1].(map[string]any)["content"].([]any)
	if len(blocks) != 2 {
		t.Fatalf("image turn should have a text block and an image block, got %v", blocks)
	}
	if tb := blocks[0].(map[string]any); tb["type"] != "text" || tb["text"] != "look" {
		t.Fatalf("first block must carry the text: %v", tb)
	}
	im := blocks[1].(map[string]any)
	if im["type"] != "image" {
		t.Fatalf("second block must be an image: %v", im)
	}
	src := im["source"].(map[string]any)
	if src["type"] != "base64" || src["media_type"] != "image/png" {
		t.Fatalf("image source shape: %v", src)
	}
	// data is BARE base64, no data: URI prefix
	want := base64.StdEncoding.EncodeToString([]byte("PNGBYTES"))
	if src["data"] != want {
		t.Fatalf("image data must be bare base64 %q, got %q", want, src["data"])
	}
}

// TestOpenAIImageContent: the OpenAI adapter emits a parts array with a data-URI
// image_url part for an image turn, and keeps plain-string content for a
// text-only turn. Breaker: remove the len(t.Images)>0 branch and the image turn
// serializes as a bare string with no image_url part.
func TestOpenAIImageContent(t *testing.T) {
	var body map[string]any
	var hdr http.Header
	srv := sseServer(t, &body, &hdr,
		`{"choices":[{"delta":{"content":"ok"}}]}`,
		`[DONE]`,
	)
	defer srv.Close()

	p := OpenAI{BaseURL: srv.URL, Key: "k", Model: "m"}
	_, err := p.Chat(context.Background(), "SYS",
		[]agent.Turn{
			{Role: "user", Text: "plain"},
			{Role: "user", Text: "look", Images: []agent.Image{
				{MediaType: "image/jpeg", Data: []byte("JPGBYTES")},
			}},
		}, nil, func(string) {}, func(string) {})
	if err != nil {
		t.Fatal(err)
	}

	msgs := body["messages"].([]any)
	// msgs[0] is the system message; the two user turns follow
	if len(msgs) != 3 {
		t.Fatalf("got %d messages", len(msgs))
	}
	if c := msgs[1].(map[string]any)["content"]; c != "plain" {
		t.Fatalf("text-only content must stay the bare string, got %T %v", c, c)
	}

	parts := msgs[2].(map[string]any)["content"].([]any)
	if len(parts) != 2 {
		t.Fatalf("image turn should have a text part and an image_url part, got %v", parts)
	}
	if tp := parts[0].(map[string]any); tp["type"] != "text" || tp["text"] != "look" {
		t.Fatalf("first part must carry the text: %v", tp)
	}
	ip := parts[1].(map[string]any)
	if ip["type"] != "image_url" {
		t.Fatalf("second part must be image_url: %v", ip)
	}
	wantURL := "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString([]byte("JPGBYTES"))
	if got := ip["image_url"].(map[string]any)["url"]; got != wantURL {
		t.Fatalf("image_url must be a data URI %q, got %q", wantURL, got)
	}
}

// TestOpenAIOmitsToolsWhenNone: with no tools the request body carries no "tools"
// field, so a tools-less model (a local vision model via the no_tools profile
// dial) is not rejected. Breaker: drop the len(toolsParam)>0 guard and an empty
// "tools" appears in the body.
func TestOpenAIOmitsToolsWhenNone(t *testing.T) {
	var body map[string]any
	var hdr http.Header
	srv := sseServer(t, &body, &hdr, `{"choices":[{"delta":{"content":"ok"}}]}`, `[DONE]`)
	defer srv.Close()

	p := OpenAI{BaseURL: srv.URL, Key: "k", Model: "m"}
	if _, err := p.Chat(context.Background(), "SYS",
		[]agent.Turn{{Role: "user", Text: "hi"}}, nil, func(string) {}, func(string) {}); err != nil {
		t.Fatal(err)
	}
	if _, ok := body["tools"]; ok {
		t.Fatalf("with no tools the body must omit the tools field, got %v", body["tools"])
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

// TestAnthropicEmptyAssistantContent: an assistant turn with neither text nor
// tool calls must serialize as a text block, never content:null, which the
// Messages API rejects with a 400 for every later call in the session.
// Breaker: remove the empty-blocks guard in the Anthropic serializer and the
// captured body carries null again.
func TestAnthropicEmptyAssistantContent(t *testing.T) {
	var body map[string]any
	srv := sseServer(t, &body, &http.Header{},
		`{"type":"message_start","message":{"usage":{"input_tokens":3}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`,
		`{"type":"message_stop"}`)
	defer srv.Close()
	p := Anthropic{BaseURL: srv.URL, Model: "m-empty-content"}
	_, err := p.Chat(context.Background(), "sys", []agent.Turn{
		{Role: "user", Text: "hi"},
		{Role: "assistant"}, // a cancelled or empty reply, saved and resumed
		{Role: "user", Text: "again"},
	}, nil, func(string) {}, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range body["messages"].([]any) {
		if m.(map[string]any)["role"] != "assistant" {
			continue
		}
		content := m.(map[string]any)["content"]
		if content == nil {
			t.Fatalf("assistant turn serialized as content:null: %v", body["messages"])
		}
	}
}

// TestAnthropicMaxTokensField: the adapter sends the configured MaxTokens and
// falls back to the 16000 default when unset.
// Breaker: hardcode 16000 again and the 4096 assertion fails; drop the
// default and the unset half fails.
func TestAnthropicMaxTokensField(t *testing.T) {
	send := func(mt int) map[string]any {
		var body map[string]any
		srv := sseServer(t, &body, &http.Header{},
			`{"type":"message_start","message":{"usage":{"input_tokens":3}}}`,
			`{"type":"message_stop"}`)
		defer srv.Close()
		p := Anthropic{BaseURL: srv.URL, Model: "m-mt", MaxTokens: mt}
		if _, err := p.Chat(context.Background(), "s", []agent.Turn{{Role: "user", Text: "x"}}, nil, func(string) {}, func(string) {}); err != nil {
			t.Fatal(err)
		}
		return body
	}
	if got := send(4096)["max_tokens"].(float64); got != 4096 {
		t.Fatalf("configured max_tokens not on the wire: %v", got)
	}
	if got := send(0)["max_tokens"].(float64); got != 16000 {
		t.Fatalf("default max_tokens changed: %v", got)
	}
}

// TestAnthropicMaxTokensSelfHeal: a 400 whose message states the model's real
// output cap ("max_tokens: N > M, which is the maximum allowed...") makes the
// adapter clamp to M and retry once, and remember M for the model so later
// calls start there.
// Breaker: remove the overflow check and the first response errors out; drop
// the remembered cap and the third request still sends 16000.
func TestAnthropicMaxTokensSelfHeal(t *testing.T) {
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		json.Unmarshal(b, &m)
		bodies = append(bodies, m)
		if m["max_tokens"].(float64) > 8192 {
			w.WriteHeader(400)
			fmt.Fprintf(w, `{"type":"error","error":{"type":"invalid_request_error","message":"max_tokens: %.0f > 8192, which is the maximum allowed number of output tokens for m-legacy"}}`, m["max_tokens"].(float64))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":3}}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"message_stop\"}\n\n")
	}))
	defer srv.Close()
	p := Anthropic{BaseURL: srv.URL, Model: "m-legacy"}
	for i := 0; i < 2; i++ {
		if _, err := p.Chat(context.Background(), "s", []agent.Turn{{Role: "user", Text: "x"}}, nil, func(string) {}, func(string) {}); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if len(bodies) != 3 {
		t.Fatalf("want 3 requests (overflow, retry, remembered), got %d", len(bodies))
	}
	if got := bodies[0]["max_tokens"].(float64); got != 16000 {
		t.Fatalf("first request max_tokens = %v, want the 16000 default", got)
	}
	for i, b := range bodies[1:] {
		if b["max_tokens"].(float64) != 8192 {
			t.Fatalf("request %d max_tokens = %v, want the server-stated cap 8192", i+1, b["max_tokens"])
		}
	}
}

// TestOpenAIMaxTokensOnlyWhenSet: the OpenAI adapter sends max_tokens only
// when explicitly configured, so the default wire shape is unchanged.
// Breaker: always send it and the "absent" half fails.
func TestOpenAIMaxTokensOnlyWhenSet(t *testing.T) {
	send := func(mt int) map[string]any {
		var body map[string]any
		srv := sseServer(t, &body, &http.Header{}, `data-unused`)
		defer srv.Close()
		p := OpenAI{BaseURL: srv.URL, Model: "m-oai", MaxTokens: mt}
		p.Chat(context.Background(), "s", []agent.Turn{{Role: "user", Text: "x"}}, nil, func(string) {}, func(string) {})
		return body
	}
	if _, ok := send(0)["max_tokens"]; ok {
		t.Fatal("unset max_tokens must not be sent")
	}
	if got := send(4096)["max_tokens"].(float64); got != 4096 {
		t.Fatalf("configured max_tokens not sent: %v", got)
	}
}

// thinkingStreamEvents is a full thinking reply: a thinking block with text
// and signature, then the text answer.
func thinkingStreamEvents() []string {
	return []string{
		`{"type":"message_start","message":{"usage":{"input_tokens":3}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"thinking"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"let me think"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig-abc"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"text"}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"the answer"}}`,
		`{"type":"content_block_stop","index":1}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":9}}`,
		`{"type":"message_stop"}`,
	}
}

// TestAnthropicThinkingCaptured: with a thinking budget set, the request
// carries the thinking parameter and a max_tokens that clears the budget, and
// the streamed thinking block is captured with its signature so the next
// request can pass it back (required for tool use under thinking).
// Breakers: drop the thinking body key and the first half fails; send the
// budget as max_tokens without margin and the margin assertion fails; stop
// capturing thinking blocks and the second half fails.
func TestAnthropicThinkingCaptured(t *testing.T) {
	var body map[string]any
	srv := sseServer(t, &body, &http.Header{}, thinkingStreamEvents()...)
	defer srv.Close()
	p := Anthropic{BaseURL: srv.URL, Model: "m-think", MaxTokens: 16000, ThinkingBudget: 10000}
	var sawThink string
	reply, err := p.Chat(context.Background(), "s", []agent.Turn{{Role: "user", Text: "x"}}, nil,
		func(string) {}, func(s string) { sawThink += s })
	if err != nil {
		t.Fatal(err)
	}
	th, ok := body["thinking"].(map[string]any)
	if !ok || th["type"] != "enabled" || th["budget_tokens"].(float64) != 10000 {
		t.Fatalf("thinking budget must reach the wire: %v", body["thinking"])
	}
	if body["max_tokens"].(float64) <= 10000 {
		t.Fatalf("max_tokens must clear the thinking budget: %v", body["max_tokens"])
	}
	if sawThink != "let me think" {
		t.Fatalf("thinking deltas must stream for display, got %q", sawThink)
	}
	if len(reply.Thinking) != 1 ||
		reply.Thinking[0].Text != "let me think" || reply.Thinking[0].Signature != "sig-abc" {
		t.Fatalf("thinking block must be captured for the round-trip: %+v", reply.Thinking)
	}
}

// TestAnthropicThinkingRoundTrip: the final assistant message must lead with
// its thinking block when thinking is enabled (the API rejects tool-use
// continuations otherwise).
// Breaker: skip re-serializing thinking and the block disappears from the
// wire body.
func TestAnthropicThinkingRoundTrip(t *testing.T) {
	var body map[string]any
	srv := sseServer(t, &body, &http.Header{},
		`{"type":"message_start","message":{"usage":{"input_tokens":3}}}`,
		`{"type":"message_stop"}`)
	defer srv.Close()
	p := Anthropic{BaseURL: srv.URL, Model: "m-think", ThinkingBudget: 10000}
	hist := []agent.Turn{
		{Role: "user", Text: "q"},
		{Role: "assistant", Thinking: []agent.ThinkingBlock{{Text: "ponder", Signature: "sig1"}},
			Calls: []agent.ToolCall{{ID: "t1", Name: "read", Args: []byte(`{}`)}}},
		{Role: "tool", Results: []agent.ToolResult{{ID: "t1", Content: "data"}}},
	}
	if _, err := p.Chat(context.Background(), "s", hist, nil, func(string) {}, func(string) {}); err != nil {
		t.Fatal(err)
	}
	msgs, _ := body["messages"].([]any)
	last := msgs[len(msgs)-2].(map[string]any) // the tool result is the final message
	if last["role"] != "assistant" {
		t.Fatalf("expected the assistant turn, got %v", last["role"])
	}
	blocks := last["content"].([]any)
	first := blocks[0].(map[string]any)
	if first["type"] != "thinking" || first["signature"] != "sig1" || first["thinking"] != "ponder" {
		t.Fatalf("assistant turn must lead with its thinking block, got %v", first)
	}
}

// TestAnthropicNoThinkingWhenOff: without a budget the wire carries no
// thinking key and history thinking blocks stay off the wire.
// Breaker: always send thinking and the first assertion fires.
func TestAnthropicNoThinkingWhenOff(t *testing.T) {
	var body map[string]any
	srv := sseServer(t, &body, &http.Header{},
		`{"type":"message_start","message":{"usage":{"input_tokens":3}}}`,
		`{"type":"message_stop"}`)
	defer srv.Close()
	p := Anthropic{BaseURL: srv.URL, Model: "m-plain"}
	hist := []agent.Turn{
		{Role: "user", Text: "q"},
		{Role: "assistant", Thinking: []agent.ThinkingBlock{{Text: "old", Signature: "s0"}}, Text: "a"},
		{Role: "user", Text: "again"},
	}
	if _, err := p.Chat(context.Background(), "s", hist, nil, func(string) {}, func(string) {}); err != nil {
		t.Fatal(err)
	}
	if _, ok := body["thinking"]; ok {
		t.Fatal("no budget configured: the wire must carry no thinking key")
	}
	if s := fmt.Sprint(body["messages"]); strings.Contains(s, "thinking") {
		t.Fatalf("history thinking blocks must not be sent when thinking is off: %s", s)
	}
}

// TestOpenAIReasoningEffort: the profile's reasoning_effort passes through
// when set, and is absent otherwise.
// Breaker: always send it (or never) and one half fails.
func TestOpenAIReasoningEffort(t *testing.T) {
	send := func(effort string) map[string]any {
		var body map[string]any
		srv := sseServer(t, &body, &http.Header{}, `x`)
		defer srv.Close()
		p := OpenAI{BaseURL: srv.URL, Model: "m-oai", ReasoningEffort: effort}
		p.Chat(context.Background(), "s", []agent.Turn{{Role: "user", Text: "x"}}, nil, func(string) {}, func(string) {})
		return body
	}
	if _, ok := send("")["reasoning_effort"]; ok {
		t.Fatal("unset effort must not be sent")
	}
	if got := send("high")["reasoning_effort"]; got != "high" {
		t.Fatalf("effort must pass through, got %v", got)
	}
}
