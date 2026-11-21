package provider

// ---------------------------------------------------------------------------
// OpenAI Responses protocol, stateless.
// POST {base}/responses · Bearer auth · typed SSE events · store:false
//
// Reasoning models need this protocol for tool calls with reasoning on. Every
// request carries the whole history (store:false, no previous_response_id),
// with each reasoning item's encrypted_content replayed opaquely: the server
// keeps nothing, the session file stays the only state, and the many
// compatible servers that serve this route statelessly work the same way.
// ---------------------------------------------------------------------------

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/mike-diff/sesh/agent"
)

// Responses speaks the stateless Responses protocol. A reasoning item maps
// onto a ThinkingBlock as: summary text in Text, the item id in Signature, the
// encrypted_content in RedactedData.
type Responses struct {
	BaseURL, Key, Model string
	// MaxTokens caps one reply's output tokens (max_output_tokens). 0 sends
	// no cap.
	MaxTokens int
	// Effort is reasoning.effort ("low" through "max"). Empty sends nothing.
	Effort string
}

func (p Responses) Chat(ctx context.Context, system string, history []agent.Turn, tools []agent.ToolDef, onText, onThink func(string)) (agent.Reply, error) {
	var input []map[string]any
	for _, t := range history {
		switch t.Role {
		case "user":
			parts := []map[string]any{}
			if t.Text != "" {
				parts = append(parts, map[string]any{"type": "input_text", "text": t.Text})
			}
			for _, im := range t.Images {
				url := "data:" + im.MediaType + ";base64," + base64.StdEncoding.EncodeToString(im.Data)
				parts = append(parts, map[string]any{"type": "input_image", "image_url": url})
			}
			input = append(input, map[string]any{"role": "user", "content": parts})
		case "assistant":
			// Another model's reasoning cannot be decrypted by this one, so only
			// this model's own items go back.
			for _, tb := range t.Thinking {
				if tb.Model != p.Model || tb.RedactedData == "" {
					continue
				}
				item := map[string]any{"type": "reasoning", "encrypted_content": tb.RedactedData, "summary": []map[string]any{}}
				if tb.Signature != "" {
					item["id"] = tb.Signature
				}
				if tb.Text != "" {
					item["summary"] = []map[string]any{{"type": "summary_text", "text": tb.Text}}
				}
				input = append(input, item)
			}
			if t.Text != "" {
				input = append(input, map[string]any{"type": "message", "role": "assistant",
					"content": []map[string]any{{"type": "output_text", "text": t.Text}}})
			}
			for _, c := range t.Calls {
				input = append(input, map[string]any{"type": "function_call", "call_id": c.ID, "name": c.Name, "arguments": string(wireArgs(c.Args))})
			}
		case "tool":
			// This protocol has no is_error flag; say it in the output.
			for _, r := range t.Results {
				out := r.Content
				if r.IsError {
					out = "ERROR: " + out
				}
				input = append(input, map[string]any{"type": "function_call_output", "call_id": r.ID, "output": out})
			}
		}
	}

	var toolsParam []map[string]any
	for _, td := range tools {
		// strict stays off: it demands schemas closed with additionalProperties
		// false and every property required, which ordinary tool schemas are not.
		toolsParam = append(toolsParam, map[string]any{
			"type": "function", "name": td.Name, "description": td.Description, "parameters": td.Schema, "strict": false,
		})
	}

	body := map[string]any{
		"model": p.Model, "instructions": system, "input": input,
		"stream": true, "store": false, "include": []string{"reasoning.encrypted_content"},
	}
	if p.Effort != "" {
		body["reasoning"] = map[string]any{"effort": p.Effort}
	}
	if p.MaxTokens > 0 {
		body["max_output_tokens"] = p.MaxTokens
	}
	if len(toolsParam) > 0 {
		body["tools"] = toolsParam
	}
	headers := map[string]string{}
	if p.Key != "" {
		headers["Authorization"] = "Bearer " + p.Key
	}
	url := strings.TrimRight(p.BaseURL, "/") + "/responses"
	return retryUnseen(ctx, onText, onThink, func(onText, onThink func(string)) (agent.Reply, error) {
		return p.stream(ctx, url, headers, body, onText, onThink)
	})
}

// stream sends one Responses request and parses its typed event stream.
// Function calls are keyed by output_index: the call opens in
// output_item.added and its arguments drip in as function_call_arguments
// deltas.
func (p Responses) stream(ctx context.Context, url string, headers map[string]string, body map[string]any, onText, onThink func(string)) (agent.Reply, error) {
	resp, err := post(ctx, url, headers, body)
	if err != nil {
		return agent.Reply{}, err
	}
	defer resp.Body.Close()

	calls := newCallAccum()
	var text strings.Builder
	var reply agent.Reply
	err = sse(ctx, resp.Body, func(data []byte) error {
		var ev struct {
			Type        string `json:"type"`
			OutputIndex int    `json:"output_index"`
			Delta       string `json:"delta"`
			Message     string `json:"message"` // top-level error events
			Item        struct {
				Type             string `json:"type"`
				ID               string `json:"id"`
				CallID           string `json:"call_id"`
				Name             string `json:"name"`
				Arguments        string `json:"arguments"`
				EncryptedContent string `json:"encrypted_content"`
				Summary          []struct {
					Text string `json:"text"`
				} `json:"summary"`
			} `json:"item"`
			Response struct {
				Error *struct {
					Message string `json:"message"`
				} `json:"error"`
				Usage struct {
					InputTokens        int `json:"input_tokens"`
					OutputTokens       int `json:"output_tokens"`
					InputTokensDetails struct {
						CachedTokens int `json:"cached_tokens"`
					} `json:"input_tokens_details"`
				} `json:"usage"`
			} `json:"response"`
		}
		if json.Unmarshal(data, &ev) != nil {
			return nil
		}
		switch ev.Type {
		case "response.output_text.delta":
			text.WriteString(ev.Delta)
			onText(ev.Delta)
		case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
			onThink(ev.Delta)
		case "response.output_item.added":
			if ev.Item.Type == "function_call" {
				calls.upsert(ev.OutputIndex, ev.Item.CallID, ev.Item.Name, "")
			}
		case "response.function_call_arguments.delta":
			calls.appendArgs(ev.OutputIndex, ev.Delta)
		case "response.output_item.done":
			switch ev.Item.Type {
			case "function_call":
				// Some servers send the arguments only on the finished item.
				if f := calls.byIdx[ev.OutputIndex]; f == nil || f.args.Len() == 0 {
					calls.upsert(ev.OutputIndex, ev.Item.CallID, ev.Item.Name, ev.Item.Arguments)
				}
			case "reasoning":
				var summary []string
				for _, s := range ev.Item.Summary {
					summary = append(summary, s.Text)
				}
				reply.Thinking = append(reply.Thinking, agent.ThinkingBlock{
					Text: strings.Join(summary, "\n"), Signature: ev.Item.ID,
					RedactedData: ev.Item.EncryptedContent, Model: p.Model,
				})
			}
		case "response.completed", "response.incomplete":
			u := ev.Response.Usage
			cached := u.InputTokensDetails.CachedTokens
			// input_tokens includes the cached subset; neutral Usage keeps Input
			// uncached-only, matching the other adapters.
			reply.Usage = agent.Usage{Input: u.InputTokens - cached, Output: u.OutputTokens, CacheRead: cached}
		case "response.failed":
			if e := ev.Response.Error; e != nil {
				return fmt.Errorf("api error: %s", e.Message)
			}
			return fmt.Errorf("api error: the response failed")
		case "error":
			return fmt.Errorf("api error: %s", ev.Message)
		}
		return nil
	})
	if err != nil {
		return agent.Reply{}, err
	}
	reply.Text = text.String()
	reply.Calls = calls.collect()
	return reply, nil
}

// ListModelInfos returns the models the endpoint serves; the route is the
// same one the chat-completions protocol uses.
func (p Responses) ListModelInfos(ctx context.Context) ([]ModelInfo, error) {
	return OpenAI{BaseURL: p.BaseURL, Key: p.Key}.ListModelInfos(ctx)
}

// ListModels returns the model ids the endpoint serves.
func (p Responses) ListModels(ctx context.Context) ([]string, error) {
	return OpenAI{BaseURL: p.BaseURL, Key: p.Key}.ListModels(ctx)
}
