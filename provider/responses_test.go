package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/mike-diff/sesh/agent"
)

// TestResponsesStream: text deltas stream, a function call assembles from
// output_item.added plus argument deltas, a call whose arguments arrive only
// on the finished item still gets them, the reasoning item is kept for
// replay, and usage splits cached input out. Breakers: drop any event case
// and its assertion fails.
func TestResponsesStream(t *testing.T) {
	var body map[string]any
	srv := sseServer(t, &body, &http.Header{},
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs_1","encrypted_content":"ENC","summary":[{"text":"thought"}]}}`,
		`{"type":"response.output_text.delta","delta":"hi"}`,
		`{"type":"response.output_item.added","output_index":2,"item":{"type":"function_call","call_id":"c1","name":"read"}}`,
		`{"type":"response.function_call_arguments.delta","output_index":2,"delta":"{\"path\":"}`,
		`{"type":"response.function_call_arguments.delta","output_index":2,"delta":"\"a\"}"}`,
		`{"type":"response.output_item.done","output_index":3,"item":{"type":"function_call","call_id":"c2","name":"loc","arguments":"{}"}}`,
		`{"type":"response.completed","response":{"usage":{"input_tokens":100,"output_tokens":7,"input_tokens_details":{"cached_tokens":60}}}}`)
	defer srv.Close()
	reply, err := Responses{BaseURL: srv.URL, Model: "m"}.Chat(context.Background(), "sys",
		[]agent.Turn{{Role: "user", Text: "x"}}, nil, func(string) {}, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	if reply.Text != "hi" {
		t.Fatalf("text: %q", reply.Text)
	}
	if len(reply.Calls) != 2 || reply.Calls[0].ID != "c1" || string(reply.Calls[0].Args) != `{"path":"a"}` ||
		reply.Calls[1].ID != "c2" || string(reply.Calls[1].Args) != "{}" {
		t.Fatalf("calls: %+v", reply.Calls)
	}
	want := agent.ThinkingBlock{Text: "thought", Signature: "rs_1", RedactedData: "ENC", Model: "m"}
	if len(reply.Thinking) != 1 || reply.Thinking[0] != want {
		t.Fatalf("reasoning item: %+v", reply.Thinking)
	}
	if reply.Usage != (agent.Usage{Input: 40, Output: 7, CacheRead: 60}) {
		t.Fatalf("usage: %+v", reply.Usage)
	}
}

// TestResponsesWireStateless: the request is stateless (store false, the
// encrypted reasoning requested, no previous_response_id), the system prompt
// rides as instructions, history maps onto typed items in order, and only
// this model's reasoning goes back. Breakers: flip store, drop the include,
// skip reasoning replay, or replay another model's item, and an assertion
// fails.
func TestResponsesWireStateless(t *testing.T) {
	var body map[string]any
	srv := sseServer(t, &body, &http.Header{}, `{"type":"response.completed","response":{}}`)
	defer srv.Close()
	hist := []agent.Turn{
		{Role: "user", Text: "q"},
		{Role: "assistant", Text: "looking",
			Thinking: []agent.ThinkingBlock{
				{Signature: "rs_mine", RedactedData: "E1", Model: "m"},
				{Signature: "rs_other", RedactedData: "E2", Model: "other"},
			},
			Calls: []agent.ToolCall{{ID: "c1", Name: "read", Args: json.RawMessage(`{"path":"a"}`)}}},
		{Role: "tool", Results: []agent.ToolResult{{ID: "c1", Content: "nope", IsError: true}}},
	}
	tools := []agent.ToolDef{{Name: "read", Description: "d", Schema: map[string]any{"type": "object"}}}
	if _, err := (Responses{BaseURL: srv.URL, Model: "m", Effort: "high"}).Chat(context.Background(), "sys", hist, tools, func(string) {}, func(string) {}); err != nil {
		t.Fatal(err)
	}
	if body["store"] != false || body["instructions"] != "sys" || body["previous_response_id"] != nil {
		t.Fatalf("request must be stateless with instructions: store=%v instructions=%v", body["store"], body["instructions"])
	}
	if inc, _ := body["include"].([]any); len(inc) != 1 || inc[0] != "reasoning.encrypted_content" {
		t.Fatalf("encrypted reasoning must be requested: %v", body["include"])
	}
	if r, _ := body["reasoning"].(map[string]any); r["effort"] != "high" {
		t.Fatalf("effort: %v", body["reasoning"])
	}
	var types []string
	for _, it := range body["input"].([]any) {
		m := it.(map[string]any)
		typ, _ := m["type"].(string)
		if typ == "" {
			typ = m["role"].(string)
		}
		types = append(types, typ)
		if typ == "reasoning" && m["id"] != "rs_mine" {
			t.Fatalf("only this model's reasoning may be replayed, got %v", m["id"])
		}
		if typ == "function_call_output" && m["output"] != "ERROR: nope" {
			t.Fatalf("errors must be marked in the output: %v", m["output"])
		}
	}
	want := []string{"user", "reasoning", "message", "function_call", "function_call_output"}
	if len(types) != len(want) {
		t.Fatalf("input items %v, want %v", types, want)
	}
	for i := range want {
		if types[i] != want[i] {
			t.Fatalf("input items %v, want %v", types, want)
		}
	}
}
