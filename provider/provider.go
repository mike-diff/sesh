// Package provider implements the agent.Provider interface for three wire
// protocols, from scratch, with nothing but net/http and encoding/json:
//
//	Anthropic Messages protocol  -> api.anthropic.com and compatibles
//	OpenAI chat-completions      -> api.openai.com, Ollama, vLLM, OpenRouter, ...
//	OpenAI Responses (stateless) -> api.openai.com reasoning models and compatibles
//
// Each adapter translates the agent package's neutral types to and from its
// wire format, including hand-parsed SSE streaming. The core never sees a
// protocol; a session recorded against one provider can resume on the other.
package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mike-diff/sesh/agent"
)

// ---------------------------------------------------------------------------
// Shared HTTP + SSE plumbing.
// ---------------------------------------------------------------------------

// httpClient carries no overall timeout: a long reasoning reply can stream
// for many minutes while making progress. Silence is what gets bounded, per
// read, by idleBody.
var httpClient = &http.Client{}

// streamIdle is the longest a streaming reply may go without a single byte
// before the call fails. Reasoning models can think silently for minutes
// before the first token, so it is generous; a variable so tests can shrink it.
var streamIdle = 10 * time.Minute

var errStreamIdle = errors.New("the provider stream went silent and was abandoned")

// idleBody fails a response body that stops delivering bytes for streamIdle,
// by cancelling the request it belongs to. Each read that makes progress
// rearms the timer.
type idleBody struct {
	io.ReadCloser
	timer  *time.Timer
	cancel context.CancelFunc
	fired  atomic.Bool
}

func newIdleBody(rc io.ReadCloser, cancel context.CancelFunc) *idleBody {
	b := &idleBody{ReadCloser: rc, cancel: cancel}
	b.timer = time.AfterFunc(streamIdle, func() { b.fired.Store(true); cancel() })
	return b
}

func (b *idleBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil && b.fired.Load() {
		return n, errStreamIdle // not a user cancel, though it surfaces as one
	}
	b.timer.Reset(streamIdle)
	return n, err
}

func (b *idleBody) Close() error {
	b.timer.Stop()
	b.cancel()
	return b.ReadCloser.Close()
}

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
		rctx, cancel := context.WithCancel(ctx)
		req, err := http.NewRequestWithContext(rctx, "POST", url, bytes.NewReader(b))
		if err != nil {
			cancel()
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
			resp.Body = newIdleBody(resp.Body, cancel)
			return resp, nil
		default:
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			apiErr := parseAPIError(resp.StatusCode, resp.Header, raw)
			if !apiErr.retryable() {
				cancel()
				return nil, apiErr
			}
			lastErr = apiErr
		}
		cancel()

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
func sse(ctx context.Context, body io.Reader, handle func(data []byte) error) error {
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		// Stop the instant the turn is cancelled (Ctrl-C). The scanner can hold
		// megabytes of already-received events; without this check they keep
		// rendering after the request itself is aborted, so the turn looks
		// uncancellable.
		if err := ctx.Err(); err != nil {
			return err
		}
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
	if err := sc.Err(); err != nil {
		return err
	}
	return ctx.Err()
}

// retryUnseen runs one request-and-stream attempt and repeats it once when the
// stream failed before the user saw any of it. post already retries what fails
// before the response; this covers what fails after a 200, such as an
// overloaded error event or a stream gone silent. A stream that already
// rendered text is not replayed: the user would read it twice.
func retryUnseen(ctx context.Context, onText, onThink func(string), attempt func(onText, onThink func(string)) (agent.Reply, error)) (agent.Reply, error) {
	for try := 0; ; try++ {
		seen := false
		watch := func(f func(string)) func(string) {
			return func(s string) {
				if s != "" {
					seen = true
				}
				f(s)
			}
		}
		reply, err := attempt(watch(onText), watch(onThink))
		var apiErr *APIError
		if err == nil || seen || try > 0 || ctx.Err() != nil || errors.As(err, &apiErr) {
			return reply, err
		}
		delay := time.Second
		OnRetry(delay, err)
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return agent.Reply{}, ctx.Err()
		}
	}
}

// wireArgs is a call's arguments as both protocols require them on replay: a
// JSON object. A call cut off mid-arguments is kept in history (it happened,
// and the core answered it with an error) but goes back as an empty object.
func wireArgs(raw json.RawMessage) json.RawMessage {
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil || obj == nil {
		return json.RawMessage("{}")
	}
	return raw
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
// where the provider sent none, defaulting empty arguments to {}, and keeping
// incomplete arguments as a JSON string so history always marshals.
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
		args := json.RawMessage(f.args.String())
		switch {
		case len(args) == 0:
			args = json.RawMessage("{}")
		case !json.Valid(args):
			// Arguments cut off at the output limit. Raw, they would make every
			// later marshal of this history fail; as a JSON string they stay
			// readable in the transcript and the core refuses to run them.
			args, _ = json.Marshal(string(args))
		}
		calls = append(calls, agent.ToolCall{ID: f.id, Name: f.name, Args: args})
	}
	return calls
}

// ---------------------------------------------------------------------------
// Anthropic Messages protocol.
// POST {base}/v1/messages · x-api-key · content blocks · stop_reason
// ---------------------------------------------------------------------------

type Anthropic struct {
	BaseURL, Key, Model string
	// MaxTokens caps one reply's output tokens. 0 means the 16000 default;
	// a 400 that names the model's real cap self-heals to that cap (see
	// overflowCap), so legacy models with smaller limits keep working.
	MaxTokens int
	// ThinkingBudget enables extended thinking with this per-reply budget
	// (thinking tokens bill as output). 0, the default, sends no thinking
	// parameter; when set, max_tokens is raised to clear the budget. Models
	// that think adaptively reject a budget: use Effort instead.
	ThinkingBudget int
	// Effort ("low" through "max") sets how much the model spends on a reply,
	// thinking included. Empty sends nothing, leaving the model's default.
	Effort string
	// CacheTTL sets the prompt cache lifetime ("5m", "1h"). Empty keeps the
	// API default. A longer lifetime costs more to write and survives longer
	// pauses between turns.
	CacheTTL string
}

// defaultMaxTokens is the output budget when none is configured. It must
// stay at or below the smallest cap among models users actually run, which
// the self-heal path backstops when it does not.
const defaultMaxTokens = 16000

// maxTokensOverflowRe matches the API's stated overflow ("max_tokens: 16000
// > 8192, which is the maximum allowed number of output tokens for ...").
var maxTokensOverflowRe = regexp.MustCompile(`max_tokens: (\d+) > (\d+)`)

// Server-stated output caps, remembered per model for the process lifetime
// so one 400 teaches every later call (adapter values are copied around, so
// the map is package-level). Mutex-guarded: parallel task subagents call
// concurrently.
var (
	outputCapsMu sync.Mutex
	outputCaps   = map[string]int{}
)

func rememberedOutputCap(model string) int {
	outputCapsMu.Lock()
	defer outputCapsMu.Unlock()
	return outputCaps[model]
}

func rememberOutputCap(model string, cap int) {
	outputCapsMu.Lock()
	outputCaps[model] = cap
	outputCapsMu.Unlock()
}

// overflowCap extracts the server-stated cap from an error, if that error is
// the documented max_tokens overflow rejection.
func overflowCap(err error) (int, bool) {
	apiErr, ok := err.(*APIError)
	if !ok || apiErr.Status != 400 {
		return 0, false
	}
	m := maxTokensOverflowRe.FindStringSubmatch(apiErr.Message)
	if m == nil {
		return 0, false
	}
	n, perr := strconv.Atoi(m[2])
	if perr != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// maxTokensFor resolves the output cap for a call: a remembered server-stated
// cap wins (it is ground truth), then the configured value, then the default.
func (p Anthropic) maxTokensFor() int {
	if c := rememberedOutputCap(p.Model); c > 0 {
		return c
	}
	if p.MaxTokens > 0 {
		return p.MaxTokens
	}
	return defaultMaxTokens
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
	lastAssistant, lastThinking := -1, []agent.ThinkingBlock(nil)
	for _, t := range history {
		switch t.Role {
		case "user":
			if len(t.Images) > 0 {
				var blocks []map[string]any
				if t.Text != "" {
					blocks = append(blocks, map[string]any{"type": "text", "text": t.Text})
				}
				for _, im := range t.Images {
					blocks = append(blocks, map[string]any{
						"type": "image",
						"source": map[string]any{
							"type":       "base64",
							"media_type": im.MediaType,
							"data":       base64.StdEncoding.EncodeToString(im.Data),
						},
					})
				}
				msgs = append(msgs, map[string]any{"role": "user", "content": blocks})
				break
			}
			msgs = append(msgs, map[string]any{"role": "user", "content": t.Text})
		case "assistant":
			var blocks []map[string]any
			if t.Text != "" {
				blocks = append(blocks, map[string]any{"type": "text", "text": t.Text})
			}
			for _, c := range t.Calls {
				blocks = append(blocks, map[string]any{"type": "tool_use", "id": c.ID, "name": c.Name, "input": wireArgs(c.Args)})
			}
			if len(blocks) == 0 {
				// A saved empty reply (or one kept after a mid-batch failure)
				// serializes to content:null otherwise, which the Messages API
				// rejects with a 400 for every later call in the session.
				blocks = []map[string]any{{"type": "text", "text": "(empty reply)"}}
			}
			msgs = append(msgs, map[string]any{"role": "assistant", "content": blocks})
			lastAssistant, lastThinking = len(msgs)-1, t.Thinking
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
	// The API requires the final assistant message to lead with its thinking
	// blocks (signatures intact) when a thinking conversation continues, and a
	// model may think without being asked (adaptive thinking), so whatever
	// blocks that turn carries go back, budget or not. Older turns' blocks are
	// dead weight and stay off, and so do another model's: its signatures are
	// not valid for this one.
	maxTokens := p.maxTokensFor()
	if lastAssistant >= 0 {
		var blocks []map[string]any
		for _, tb := range lastThinking {
			// Unlabeled blocks predate the label and were only ever kept under
			// a budget, so they replay on the same condition as before.
			if mine := tb.Model == p.Model || (tb.Model == "" && p.ThinkingBudget > 0); !mine {
				continue
			}
			if tb.RedactedData != "" {
				blocks = append(blocks, map[string]any{"type": "redacted_thinking", "data": tb.RedactedData})
			} else {
				blocks = append(blocks, map[string]any{"type": "thinking", "thinking": tb.Text, "signature": tb.Signature})
			}
		}
		if len(blocks) > 0 {
			content := msgs[lastAssistant]["content"].([]map[string]any)
			msgs[lastAssistant]["content"] = append(blocks, content...)
		}
	}
	if margin := p.ThinkingBudget + 8000; p.ThinkingBudget > 0 && maxTokens < margin {
		maxTokens = margin // max_tokens must clear the thinking budget
	}
	ephemeral := map[string]any{"type": "ephemeral"}
	if p.CacheTTL != "" {
		ephemeral["ttl"] = p.CacheTTL
	}
	markLastMessage(msgs, ephemeral)
	body := map[string]any{
		"model": p.Model, "max_tokens": maxTokens,
		"system":   []map[string]any{{"type": "text", "text": system, "cache_control": ephemeral}},
		"messages": msgs, "stream": true,
	}
	if p.ThinkingBudget > 0 {
		body["thinking"] = map[string]any{"type": "enabled", "budget_tokens": p.ThinkingBudget}
	}
	if p.Effort != "" {
		body["output_config"] = map[string]any{"effort": p.Effort}
	}
	if len(toolsParam) > 0 {
		body["tools"] = toolsParam
	}

	url := strings.TrimRight(p.BaseURL, "/") + "/v1/messages"
	headers := map[string]string{"x-api-key": p.Key, "anthropic-version": "2023-06-01"}
	return retryUnseen(ctx, onText, onThink, func(onText, onThink func(string)) (agent.Reply, error) {
		return p.stream(ctx, url, headers, body, onText, onThink)
	})
}

// stream sends one Messages request and parses its event stream.
func (p Anthropic) stream(ctx context.Context, url string, headers map[string]string, body map[string]any, onText, onThink func(string)) (agent.Reply, error) {
	resp, err := post(ctx, url, headers, body)
	if cap, ok := overflowCap(err); ok {
		// The server says this model caps below what we asked: clamp to the
		// stated cap, remember it, and retry once. Without this, any model
		// whose cap sits under the configured default fails every call.
		rememberOutputCap(p.Model, cap)
		body["max_tokens"] = cap
		resp, err = post(ctx, url, headers, body)
	}
	if err != nil {
		return agent.Reply{}, err
	}
	defer resp.Body.Close()

	// Streamed content blocks arrive by index: tool_use blocks open in
	// content_block_start, then their input drips in as input_json_delta.
	calls := newCallAccum()
	var text strings.Builder
	var reply agent.Reply
	// Thinking blocks accumulate by content-block index until they close, so
	// the finished blocks (text plus signature) can ride back on the next
	// call, which the API requires for tool use under thinking.
	thinkIdx, thinkCur := -1, agent.ThinkingBlock{}

	err = sse(ctx, resp.Body, func(data []byte) error {
		var ev struct {
			Type         string `json:"type"`
			Index        int    `json:"index"`
			ContentBlock struct {
				Type string `json:"type"`
				ID   string `json:"id"`
				Name string `json:"name"`
				Data string `json:"data"` // redacted_thinking carries its blob here
			} `json:"content_block"`
			Delta struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				Thinking    string `json:"thinking"`
				Signature   string `json:"signature"`
				PartialJSON string `json:"partial_json"`
			} `json:"delta"`
			Message struct {
				Usage struct {
					InputTokens              int `json:"input_tokens"`
					CacheReadInputTokens     int `json:"cache_read_input_tokens"`
					CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
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
			reply.Usage.CacheWrite = ev.Message.Usage.CacheCreationInputTokens
		case "message_delta":
			reply.Usage.Output = ev.Usage.OutputTokens
		case "content_block_start":
			switch ev.ContentBlock.Type {
			case "tool_use":
				calls.upsert(ev.Index, ev.ContentBlock.ID, ev.ContentBlock.Name, "")
			case "thinking":
				thinkIdx, thinkCur = ev.Index, agent.ThinkingBlock{Model: p.Model}
			case "redacted_thinking":
				reply.Thinking = append(reply.Thinking, agent.ThinkingBlock{RedactedData: ev.ContentBlock.Data, Model: p.Model})
			}
		case "content_block_stop":
			if ev.Index == thinkIdx {
				reply.Thinking = append(reply.Thinking, thinkCur)
				thinkIdx = -1
			}
		case "content_block_delta":
			switch ev.Delta.Type {
			case "text_delta":
				text.WriteString(ev.Delta.Text)
				onText(ev.Delta.Text)
			case "thinking_delta":
				thinkCur.Text += ev.Delta.Thinking
				onThink(ev.Delta.Thinking)
			case "signature_delta":
				thinkCur.Signature += ev.Delta.Signature
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
	// MaxTokens caps one reply's output tokens. 0 (the default) sends no cap:
	// OpenAI and most compatible servers pick their own, and capping by
	// accident would bite models that budget large completions.
	MaxTokens int
	// ReasoningEffort passes a reasoning-effort hint ("low", "high", ...)
	// through to servers that support one. Empty sends nothing. When set,
	// MaxTokens (if any) travels as max_completion_tokens: reasoning models
	// reject the classic max_tokens parameter.
	ReasoningEffort string
}

func (p OpenAI) Chat(ctx context.Context, system string, history []agent.Turn, tools []agent.ToolDef, onText, onThink func(string)) (agent.Reply, error) {
	msgs := []map[string]any{{"role": "system", "content": system}}
	for _, t := range history {
		switch t.Role {
		case "user":
			if len(t.Images) > 0 {
				var parts []map[string]any
				if t.Text != "" {
					parts = append(parts, map[string]any{"type": "text", "text": t.Text})
				}
				for _, im := range t.Images {
					url := "data:" + im.MediaType + ";base64," + base64.StdEncoding.EncodeToString(im.Data)
					parts = append(parts, map[string]any{
						"type":      "image_url",
						"image_url": map[string]any{"url": url},
					})
				}
				msgs = append(msgs, map[string]any{"role": "user", "content": parts})
				break
			}
			msgs = append(msgs, map[string]any{"role": "user", "content": t.Text})
		case "assistant":
			m := map[string]any{"role": "assistant", "content": t.Text}
			if r := ownReasoning(t.Thinking, p.Model); r != "" {
				m["reasoning_content"] = r
			}
			if len(t.Calls) > 0 {
				var tcs []map[string]any
				for _, c := range t.Calls {
					tcs = append(tcs, map[string]any{
						"id": c.ID, "type": "function",
						"function": map[string]any{"name": c.Name, "arguments": string(wireArgs(c.Args))},
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
	switch {
	case p.ReasoningEffort != "":
		body["reasoning_effort"] = p.ReasoningEffort
		if p.MaxTokens > 0 {
			body["max_completion_tokens"] = p.MaxTokens // reasoning models reject max_tokens
		}
	case p.MaxTokens > 0:
		body["max_tokens"] = p.MaxTokens
	}
	if len(toolsParam) > 0 {
		body["tools"] = toolsParam
	}
	headers := map[string]string{}
	if p.Key != "" {
		headers["Authorization"] = "Bearer " + p.Key
	}

	url := strings.TrimRight(p.BaseURL, "/") + "/chat/completions"
	return retryUnseen(ctx, onText, onThink, func(onText, onThink func(string)) (agent.Reply, error) {
		return p.stream(ctx, url, headers, body, onText, onThink)
	})
}

// stream sends one chat-completions request and parses its event stream.
func (p OpenAI) stream(ctx context.Context, url string, headers map[string]string, body map[string]any, onText, onThink func(string)) (agent.Reply, error) {
	resp, err := post(ctx, url, headers, body)
	if err != nil {
		return agent.Reply{}, err
	}
	defer resp.Body.Close()

	// Streamed tool calls arrive as fragments keyed by index: the id and name
	// in the first fragment, the arguments JSON drip-fed across the rest.
	calls := newCallAccum()
	var text, reasoning strings.Builder
	var reply agent.Reply

	err = sse(ctx, resp.Body, func(data []byte) error {
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
			reasoning.WriteString(think)
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
	if reasoning.Len() > 0 {
		reply.Thinking = []agent.ThinkingBlock{{Text: reasoning.String(), Model: p.Model}}
	}
	return reply, nil
}

// ownReasoning is the reasoning text this model wrote for one assistant turn,
// for replay as reasoning_content. Thinking servers on this protocol expect
// it back: some reject a tool conversation without it, others silently lose
// quality and cache hits. Another model's reasoning is dropped, never
// converted: a model reads replayed reasoning as its own.
func ownReasoning(blocks []agent.ThinkingBlock, model string) string {
	var b strings.Builder
	for _, tb := range blocks {
		if tb.Model == model && tb.Signature == "" && tb.RedactedData == "" {
			b.WriteString(tb.Text)
		}
	}
	return b.String()
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
