// Package provider implements the agent.Provider interface for two wire
// protocols, from scratch, with nothing but net/http and encoding/json:
//
//	Anthropic Messages protocol  -> api.anthropic.com and compatibles
//	OpenAI chat-completions      -> api.openai.com, Ollama, vLLM, OpenRouter, ...
//
// Each adapter translates the agent package's neutral types to and from its
// wire format, including hand-parsed SSE streaming. The core never sees a
// protocol; a session recorded against one provider can resume on the other.
package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mike-diff/sesh/agent"
)

// ---------------------------------------------------------------------------
// Shared HTTP + SSE plumbing.
// ---------------------------------------------------------------------------

var httpClient = &http.Client{Timeout: 10 * time.Minute}

// APIError is a typed provider error: inspect Status instead of string-matching.
type APIError struct {
	Status     int
	Message    string
	RetryAfter time.Duration
}

func (e *APIError) Error() string { return fmt.Sprintf("HTTP %d: %s", e.Status, e.Message) }

func (e *APIError) retryable() bool {
	switch e.Status {
	case 429, 500, 502, 503, 529:
		return true
	}
	return false
}

func parseAPIError(status int, hdr http.Header, raw []byte) *APIError {
	msg := strings.TrimSpace(string(raw))
	// Both protocols wrap the message the same way: {"error": {"message": ...}}.
	var shape struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &shape) == nil && shape.Error.Message != "" {
		msg = shape.Error.Message
	}
	e := &APIError{Status: status, Message: msg}
	if s := hdr.Get("Retry-After"); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			e.RetryAfter = time.Duration(n) * time.Second
		}
	}
	return e
}

const maxRetries = 3

// OnRetry, when set, is told about each backoff so the product can render it.
// The core stays silent; this is the provider package's one output hook.
var OnRetry = func(delay time.Duration, err error) {}

// post sends the request with automatic retries: exponential backoff on
// rate limits (429), overload (529), transient 5xx, and network errors,
// honoring Retry-After when the provider sends one.
func post(ctx context.Context, url string, headers map[string]string, body any) (*http.Response, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	var lastErr error
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(b))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		for k, v := range headers {
			req.Header.Set(k, v)
		}

		resp, err := httpClient.Do(req)
		switch {
		case err != nil:
			lastErr = err // network error: retryable
		case resp.StatusCode == http.StatusOK:
			return resp, nil
		default:
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			apiErr := parseAPIError(resp.StatusCode, resp.Header, raw)
			if !apiErr.retryable() {
				return nil, apiErr
			}
			lastErr = apiErr
		}

		if attempt == maxRetries {
			return nil, lastErr
		}
		delay := time.Duration(1<<attempt) * time.Second
		if apiErr, ok := lastErr.(*APIError); ok && apiErr.RetryAfter > 0 {
			delay = apiErr.RetryAfter
		}
		OnRetry(delay, lastErr)
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// sse reads a Server-Sent Events body and hands each data payload to handle.
func sse(body io.Reader, handle func(data []byte) error) error {
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue // event: lines, comments, keep-alives
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		if err := handle([]byte(data)); err != nil {
			return err
		}
	}
	return sc.Err()
}

// callAccum gathers streamed tool-call fragments, shared by both protocols.
// Fragments arrive keyed by content-block index: the id and name first, the
// arguments JSON drip-fed across many events.
type callAccum struct {
	byIdx map[int]*callFrag
}

type callFrag struct {
	id, name string
	args     strings.Builder
}

func newCallAccum() *callAccum { return &callAccum{byIdx: map[int]*callFrag{}} }

// upsert creates the call at index i if needed and folds in whatever the
// fragment carried (OpenAI sends id/name/args in arbitrary fragments).
func (a *callAccum) upsert(i int, id, name, args string) {
	f := a.byIdx[i]
	if f == nil {
		f = &callFrag{}
		a.byIdx[i] = f
	}
	if id != "" {
		f.id = id
	}
	if name != "" {
		f.name = name
	}
	f.args.WriteString(args)
}

// appendArgs adds input only to an already-started call, mirroring the
// Anthropic stream where input_json_delta always follows content_block_start.
func (a *callAccum) appendArgs(i int, s string) {
	if f := a.byIdx[i]; f != nil {
		f.args.WriteString(s)
	}
}

// collect returns the finished calls in block-index order, synthesizing an id
// where the provider sent none and defaulting empty arguments to {}.
func (a *callAccum) collect() []agent.ToolCall {
	idxs := make([]int, 0, len(a.byIdx))
	for i := range a.byIdx {
		idxs = append(idxs, i)
	}
	sort.Ints(idxs)
	var calls []agent.ToolCall
	for _, i := range idxs {
		f := a.byIdx[i]
		if f.id == "" {
			f.id = fmt.Sprintf("call_%d", i)
		}
		args := f.args.String()
		if args == "" {
			args = "{}"
		}
		calls = append(calls, agent.ToolCall{ID: f.id, Name: f.name, Args: json.RawMessage(args)})
	}
	return calls
}

// ---------------------------------------------------------------------------
// Anthropic Messages protocol.
// POST {base}/v1/messages · x-api-key · content blocks · stop_reason
// ---------------------------------------------------------------------------

type Anthropic struct {
	BaseURL, Key, Model string
}

// markLastMessage sets a cache breakpoint on the final content block of the
// last message, converting string content to a block first. Everything up to
// the breakpoint is cached, so the next call re-reads history from cache.
func markLastMessage(msgs []map[string]any, ephemeral map[string]any) {
	if len(msgs) == 0 {
		return
	}
	last := msgs[len(msgs)-1]
	switch content := last["content"].(type) {
	case string:
		if content == "" {
			return // the API rejects empty text blocks; leave it as a string
		}
		last["content"] = []map[string]any{{"type": "text", "text": content, "cache_control": ephemeral}}
	case []map[string]any:
		if len(content) > 0 {
			content[len(content)-1]["cache_control"] = ephemeral
		}
	}
}

func (p Anthropic) Chat(ctx context.Context, system string, history []agent.Turn, tools []agent.ToolDef, onText, onThink func(string)) (agent.Reply, error) {
	var msgs []map[string]any
	for _, t := range history {
		switch t.Role {
		case "user":
			msgs = append(msgs, map[string]any{"role": "user", "content": t.Text})
		case "assistant":
			var blocks []map[string]any
			if t.Text != "" {
				blocks = append(blocks, map[string]any{"type": "text", "text": t.Text})
			}
			for _, c := range t.Calls {
				blocks = append(blocks, map[string]any{"type": "tool_use", "id": c.ID, "name": c.Name, "input": c.Args})
			}
			msgs = append(msgs, map[string]any{"role": "assistant", "content": blocks})
		case "tool":
			var blocks []map[string]any
			for _, r := range t.Results {
				blocks = append(blocks, map[string]any{
					"type": "tool_result", "tool_use_id": r.ID, "content": r.Content, "is_error": r.IsError,
				})
			}
			msgs = append(msgs, map[string]any{"role": "user", "content": blocks})
		}
	}

	var toolsParam []map[string]any
	for _, td := range tools {
		toolsParam = append(toolsParam, map[string]any{
			"name": td.Name, "description": td.Description, "input_schema": td.Schema,
		})
	}

	// Prompt caching: cache_control is a content-block property, not a request
	// field. Two breakpoints: the system block caches the stable prefix (tools +
	// system), and the last message caches the growing history, so each call
	// re-reads the prior turns at the cached price instead of full price.
	ephemeral := map[string]any{"type": "ephemeral"}
	markLastMessage(msgs, ephemeral)
	body := map[string]any{
		"model": p.Model, "max_tokens": 16000,
		"system":   []map[string]any{{"type": "text", "text": system, "cache_control": ephemeral}},
		"messages": msgs, "stream": true,
	}
	if len(toolsParam) > 0 {
		body["tools"] = toolsParam
	}

	resp, err := post(ctx, strings.TrimRight(p.BaseURL, "/")+"/v1/messages",
		map[string]string{"x-api-key": p.Key, "anthropic-version": "2023-06-01"}, body)
	if err != nil {
		return agent.Reply{}, err
	}
	defer resp.Body.Close()

	// Streamed content blocks arrive by index: tool_use blocks open in
	// content_block_start, then their input drips in as input_json_delta.
	calls := newCallAccum()
	var text strings.Builder
	var reply agent.Reply

	err = sse(resp.Body, func(data []byte) error {
		var ev struct {
			Type         string `json:"type"`
			Index        int    `json:"index"`
			ContentBlock struct {
				Type string `json:"type"`
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"content_block"`
			Delta struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				Thinking    string `json:"thinking"`
				PartialJSON string `json:"partial_json"`
			} `json:"delta"`
			Message struct {
				Usage struct {
					InputTokens          int `json:"input_tokens"`
					CacheReadInputTokens int `json:"cache_read_input_tokens"`
				} `json:"usage"`
			} `json:"message"`
			Usage struct {
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(data, &ev) != nil {
			return nil
		}
		switch ev.Type {
		case "message_start":
			reply.Usage.Input = ev.Message.Usage.InputTokens
			reply.Usage.CacheRead = ev.Message.Usage.CacheReadInputTokens
		case "message_delta":
			reply.Usage.Output = ev.Usage.OutputTokens
		case "content_block_start":
			if ev.ContentBlock.Type == "tool_use" {
				calls.upsert(ev.Index, ev.ContentBlock.ID, ev.ContentBlock.Name, "")
			}
		case "content_block_delta":
			switch ev.Delta.Type {
			case "text_delta":
				text.WriteString(ev.Delta.Text)
				onText(ev.Delta.Text)
			case "thinking_delta":
				onThink(ev.Delta.Thinking)
			case "input_json_delta":
				calls.appendArgs(ev.Index, ev.Delta.PartialJSON)
			}
		case "error":
			return fmt.Errorf("api error: %s", ev.Error.Message)
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

// ---------------------------------------------------------------------------
// OpenAI chat-completions protocol.
// POST {base}/chat/completions · Bearer auth · tool_calls · finish_reason
// Speaks to OpenAI, Ollama, vLLM, OpenRouter, and most local servers.
// ---------------------------------------------------------------------------

type OpenAI struct {
	BaseURL, Key, Model string
}

func (p OpenAI) Chat(ctx context.Context, system string, history []agent.Turn, tools []agent.ToolDef, onText, onThink func(string)) (agent.Reply, error) {
	msgs := []map[string]any{{"role": "system", "content": system}}
	for _, t := range history {
		switch t.Role {
		case "user":
			msgs = append(msgs, map[string]any{"role": "user", "content": t.Text})
		case "assistant":
			m := map[string]any{"role": "assistant", "content": t.Text}
			if len(t.Calls) > 0 {
				var tcs []map[string]any
				for _, c := range t.Calls {
					tcs = append(tcs, map[string]any{
						"id": c.ID, "type": "function",
						"function": map[string]any{"name": c.Name, "arguments": string(c.Args)},
					})
				}
				m["tool_calls"] = tcs
			}
			msgs = append(msgs, m)
		case "tool":
			// This protocol has no is_error flag; say it in the content.
			for _, r := range t.Results {
				content := r.Content
				if r.IsError {
					content = "ERROR: " + content
				}
				msgs = append(msgs, map[string]any{"role": "tool", "tool_call_id": r.ID, "content": content})
			}
		}
	}

	var toolsParam []map[string]any
	for _, td := range tools {
		toolsParam = append(toolsParam, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name": td.Name, "description": td.Description, "parameters": td.Schema,
			},
		})
	}

	body := map[string]any{
		"model": p.Model, "messages": msgs, "stream": true,
		"stream_options": map[string]any{"include_usage": true},
	}
	if len(toolsParam) > 0 {
		body["tools"] = toolsParam
	}
	headers := map[string]string{}
	if p.Key != "" {
		headers["Authorization"] = "Bearer " + p.Key
	}

	resp, err := post(ctx, strings.TrimRight(p.BaseURL, "/")+"/chat/completions", headers, body)
	if err != nil {
		return agent.Reply{}, err
	}
	defer resp.Body.Close()

	// Streamed tool calls arrive as fragments keyed by index: the id and name
	// in the first fragment, the arguments JSON drip-fed across the rest.
	calls := newCallAccum()
	var text strings.Builder
	var reply agent.Reply

	err = sse(resp.Body, func(data []byte) error {
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
					// Thinking models stream reasoning in one of these,
					// depending on the server (Ollama, DeepSeek/vLLM).
					Reasoning        string `json:"reasoning"`
					ReasoningContent string `json:"reasoning_content"`
					ToolCalls        []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens        int `json:"prompt_tokens"`
				CompletionTokens    int `json:"completion_tokens"`
				PromptTokensDetails *struct {
					CachedTokens int `json:"cached_tokens"`
				} `json:"prompt_tokens_details"`
			} `json:"usage"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(data, &chunk) != nil {
			return nil
		}
		if chunk.Error != nil {
			return fmt.Errorf("api error: %s", chunk.Error.Message)
		}
		if chunk.Usage != nil {
			reply.Usage.Input = chunk.Usage.PromptTokens
			reply.Usage.Output = chunk.Usage.CompletionTokens
			// prompt_tokens includes the cached subset; neutral Usage keeps
			// Input as uncached-only so Input+CacheRead is the full prompt,
			// matching what the Anthropic adapter reports.
			if d := chunk.Usage.PromptTokensDetails; d != nil && d.CachedTokens > 0 {
				reply.Usage.CacheRead = d.CachedTokens
				reply.Usage.Input = chunk.Usage.PromptTokens - d.CachedTokens
			}
		}
		if len(chunk.Choices) == 0 {
			return nil
		}
		d := chunk.Choices[0].Delta
		if think := d.Reasoning + d.ReasoningContent; think != "" {
			onThink(think)
		}
		if d.Content != "" {
			text.WriteString(d.Content)
			onText(d.Content)
		}
		for _, tc := range d.ToolCalls {
			calls.upsert(tc.Index, tc.ID, tc.Function.Name, tc.Function.Arguments)
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

// ---------------------------------------------------------------------------
// Model discovery. Both protocols expose the same list shape at a /models
// endpoint, so the harness can pull what an endpoint serves on startup and let
// the user switch between those models. Best-effort: callers fall back to the
// configured model when an endpoint does not answer.
// ---------------------------------------------------------------------------

// ModelInfo is what discovery learns about one model. Context is 0 when the
// endpoint does not publish a context length; the field names below cover the
// endpoints that do (OpenRouter and gateways, vLLM, assorted proxies). The
// OpenAI API itself and ollama publish nothing, so 0 is the common case.
type ModelInfo struct {
	ID      string
	Context int
}

// modelList is the shared response shape: {"data": [{"id": "..."}, ...]},
// with the optional context-size fields the wild uses.
type modelList struct {
	Data []struct {
		ID               string `json:"id"`
		ContextLength    int    `json:"context_length"`     // OpenRouter and friends
		ContextWindow    int    `json:"context_window"`     // assorted gateways
		MaxModelLen      int    `json:"max_model_len"`      // vLLM
		MaxContextLength int    `json:"max_context_length"` // assorted proxies
	} `json:"data"`
}

func (m modelList) infos() []ModelInfo {
	out := make([]ModelInfo, 0, len(m.Data))
	for _, d := range m.Data {
		if d.ID == "" {
			continue
		}
		ctxLen := d.ContextLength
		for _, alt := range []int{d.ContextWindow, d.MaxModelLen, d.MaxContextLength} {
			if ctxLen == 0 {
				ctxLen = alt
			}
		}
		out = append(out, ModelInfo{ID: d.ID, Context: ctxLen})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func ids(infos []ModelInfo) []string {
	out := make([]string, len(infos))
	for i, m := range infos {
		out[i] = m.ID
	}
	return out
}

// getJSON performs a plain GET and decodes the JSON body. No retries: discovery
// is best-effort and must not delay startup on a flaky endpoint.
func getJSON(ctx context.Context, url string, headers map[string]string, out any) error {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return parseAPIError(resp.StatusCode, resp.Header, raw)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// ListModelInfos returns the models the OpenAI-protocol endpoint serves, with
// context lengths where the endpoint publishes them.
func (p OpenAI) ListModelInfos(ctx context.Context) ([]ModelInfo, error) {
	headers := map[string]string{}
	if p.Key != "" {
		headers["Authorization"] = "Bearer " + p.Key
	}
	var ml modelList
	if err := getJSON(ctx, strings.TrimRight(p.BaseURL, "/")+"/models", headers, &ml); err != nil {
		return nil, err
	}
	return ml.infos(), nil
}

// ListModels returns the model ids the OpenAI-protocol endpoint serves.
func (p OpenAI) ListModels(ctx context.Context) ([]string, error) {
	infos, err := p.ListModelInfos(ctx)
	if err != nil {
		return nil, err
	}
	return ids(infos), nil
}

// ListModelInfos returns the models the Anthropic-protocol endpoint serves.
// The Anthropic API does not publish context lengths, so Context stays 0.
func (p Anthropic) ListModelInfos(ctx context.Context) ([]ModelInfo, error) {
	headers := map[string]string{"x-api-key": p.Key, "anthropic-version": "2023-06-01"}
	var ml modelList
	if err := getJSON(ctx, strings.TrimRight(p.BaseURL, "/")+"/v1/models", headers, &ml); err != nil {
		return nil, err
	}
	return ml.infos(), nil
}

// ListModels returns the model ids the Anthropic-protocol endpoint serves.
func (p Anthropic) ListModels(ctx context.Context) ([]string, error) {
	infos, err := p.ListModelInfos(ctx)
	if err != nil {
		return nil, err
	}
	return ids(infos), nil
}
