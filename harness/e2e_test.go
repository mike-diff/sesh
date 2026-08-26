// The end-to-end rig: the real binary driven headless against a scripted
// in-process endpoint. Unit tests pin pieces; this pins the assembled
// product: flags, gates, tools, the drive loop, session persistence, and the
// wire protocols exactly as a user's `-p` run exercises them.
//
// Opt-in, like the retention rig (a build step makes it slower than unit
// tests):
//
//	SESH_E2E=1 go test ./harness/ -run TestE2E -v
//
// Each scenario gets its own HOME, working directory, and mock endpoint, so
// runs cannot see each other's sessions or providers. The regressions here
// are the ones the verification rig caught in the first place: mode-clobbering
// writes, history-losing failures, judge-stopping prose, silent search skips,
// protocol-shape bugs on the anthropic wire.
package harness

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// e2eStep is one scripted model reply. kind: text | tool | error |
// mtoverflow.
type e2eStep struct {
	Kind   string
	Text   string
	Name   string
	Args   string
	Status int
	Msg    string
	Cap    int // mtoverflow: the server-stated output cap
}

func eText(s string) e2eStep { return e2eStep{Kind: "text", Text: s} }
func eTool(name, args string) e2eStep {
	return e2eStep{Kind: "tool", Name: name, Args: args}
}

// verdictJSON is a judge reply that rules done.
func verdictJSON(reason string) e2eStep {
	return eText(`{"done": true, "blocked": false, "reason": "` + reason + `"}`)
}

// capturedReq is one request the mock saw, with its protocol class.
type capturedReq struct {
	Class string // "worker" | "judge" | "anthropic"
	Body  map[string]any
}

type e2eMock struct {
	t       *testing.T
	srv     *httptest.Server
	mu      sync.Mutex
	worker  []e2eStep
	judge   []e2eStep
	wi, ji  int
	reqs    []capturedReq
	homeDir string
}

// newRig builds the mock endpoint plus an isolated HOME whose providers.json
// points at it, and a scenario working directory. The returned home has two
// profiles: mock (openai) and amock (anthropic, max_tokens 4096).
func newRig(t *testing.T, worker, judge []e2eStep) (*e2eMock, string) {
	t.Helper()
	m := &e2eMock{t: t, worker: worker, judge: judge, homeDir: t.TempDir()}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[{"id":"mock-model","context_length":100000}]}`)
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		body := m.capture(r, "worker")
		w.Header().Set("Content-Type", "text/event-stream")
		step, class, ok := func() (e2eStep, string, bool) {
			if isJudgeBody(body) {
				s, ok := m.popJudge()
				return s, "judge", ok
			}
			s, ok := m.popWorker()
			return s, "worker", ok
		}()
		if !ok {
			w.WriteHeader(500)
			fmt.Fprintf(w, `{"error":{"message":"mock %s queue empty"}}`, class)
			return
		}
		m.openaiReply(w, step)
	})
	mux.HandleFunc("/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		body := m.capture(r, "anthropic")
		w.Header().Set("Content-Type", "text/event-stream")
		step, ok := m.popWorker()
		if isJudgeBody(body) {
			step, ok = m.popJudge()
		}
		if !ok {
			w.WriteHeader(500)
			fmt.Fprintf(w, `{"error":{"message":"mock queue empty"}}`)
			return
		}
		m.anthropicReply(w, step)
	})
	m.srv = httptest.NewServer(mux)
	t.Cleanup(m.srv.Close)

	providers := map[string]any{
		"default": "mock",
		"providers": map[string]any{
			"mock":  map[string]any{"protocol": "openai", "url": m.srv.URL + "/v1", "model": "mock-model", "context": 100000},
			"amock": map[string]any{"protocol": "anthropic", "url": m.srv.URL, "model": "claude-3-5-sonnet", "context": 100000, "max_tokens": 4096},
			"think": map[string]any{"protocol": "anthropic", "url": m.srv.URL, "model": "claude-sonnet-4", "context": 100000, "thinking_budget": 5000},
		},
	}
	b, _ := json.MarshalIndent(providers, "", "  ")
	if err := os.MkdirAll(filepath.Join(m.homeDir, ".sesh"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(m.homeDir, ".sesh", "providers.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	return m, t.TempDir()
}

func isJudgeBody(body map[string]any) bool {
	// openai: a system message carries the judge prompt; anthropic: the
	// system field is an array of text blocks.
	if blocks, ok := body["system"].([]any); ok {
		for _, b := range blocks {
			if bm, ok := b.(map[string]any); ok {
				if c, ok := bm["text"].(string); ok && strings.Contains(c, "judge request completion") {
					return true
				}
			}
		}
	}
	msgs, _ := body["messages"].([]any)
	for _, mm := range msgs {
		m, ok := mm.(map[string]any)
		if !ok || m["role"] != "system" {
			continue
		}
		if c, ok := m["content"].(string); ok && strings.Contains(c, "judge request completion") {
			return true
		}
	}
	return false
}

func (m *e2eMock) capture(r *http.Request, class string) map[string]any {
	b, _ := io.ReadAll(r.Body)
	var body map[string]any
	json.Unmarshal(b, &body)
	if class == "worker" && isJudgeBody(body) {
		class = "judge"
	}
	m.mu.Lock()
	m.reqs = append(m.reqs, capturedReq{Class: class, Body: body})
	m.mu.Unlock()
	return body
}

func (m *e2eMock) popWorker() (e2eStep, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.wi >= len(m.worker) {
		return e2eStep{}, false
	}
	s := m.worker[m.wi]
	m.wi++
	return s, true
}

func (m *e2eMock) popJudge() (e2eStep, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ji >= len(m.judge) {
		return e2eStep{}, false
	}
	s := m.judge[m.ji]
	m.ji++
	return s, true
}

func (m *e2eMock) sse(w http.ResponseWriter, payload any) {
	b, _ := json.Marshal(payload)
	fmt.Fprintf(w, "data: %s\n\n", b)
}

func (m *e2eMock) openaiReply(w http.ResponseWriter, s e2eStep) {
	switch s.Kind {
	case "error":
		w.WriteHeader(s.Status)
		fmt.Fprintf(w, `{"error":{"message":%q}}`, s.Msg)
	case "tool":
		args := s.Args
		if args == "" {
			args = "{}"
		}
		m.sse(w, map[string]any{"choices": []any{map[string]any{
			"delta": map[string]any{"tool_calls": []any{map[string]any{
				"index": 0, "id": "call_1",
				"function": map[string]any{"name": s.Name, "arguments": args},
			}}}}}})
		m.sse(w, map[string]any{"choices": []any{}, "usage": map[string]any{"prompt_tokens": 50, "completion_tokens": 5}})
		fmt.Fprint(w, "data: [DONE]\n\n")
	default:
		m.sse(w, map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": s.Text}}}})
		m.sse(w, map[string]any{"choices": []any{}, "usage": map[string]any{"prompt_tokens": 50, "completion_tokens": 5}})
		fmt.Fprint(w, "data: [DONE]\n\n")
	}
}

func (m *e2eMock) anthropicReply(w http.ResponseWriter, s e2eStep) {
	switch s.Kind {
	case "error":
		w.WriteHeader(s.Status)
		fmt.Fprintf(w, `{"type":"error","error":{"type":"invalid_request_error","message":%q}}`, s.Msg)
	case "mtoverflow":
		w.WriteHeader(400)
		fmt.Fprintf(w, `{"type":"error","error":{"type":"invalid_request_error","message":"max_tokens: 16000 > %d, which is the maximum allowed number of output tokens for claude-3-5-sonnet"}}`, s.Cap)
	case "thinktool":
		args := s.Args
		if args == "" {
			args = "{}"
		}
		m.sse(w, map[string]any{"type": "message_start", "message": map[string]any{"usage": map[string]any{"input_tokens": 40}}})
		m.sse(w, map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "thinking"}})
		m.sse(w, map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "thinking_delta", "thinking": "reasoning about the tool"}})
		m.sse(w, map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "signature_delta", "signature": "sig-roundtrip"}})
		m.sse(w, map[string]any{"type": "content_block_stop", "index": 0})
		m.sse(w, map[string]any{"type": "content_block_start", "index": 1, "content_block": map[string]any{"type": "tool_use", "id": "toolu_1", "name": s.Name}})
		m.sse(w, map[string]any{"type": "content_block_delta", "index": 1, "delta": map[string]any{"type": "input_json_delta", "partial_json": args}})
		m.sse(w, map[string]any{"type": "content_block_stop", "index": 1})
		m.sse(w, map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "tool_use"}, "usage": map[string]any{"output_tokens": 5}})
		m.sse(w, map[string]any{"type": "message_stop"})
	case "tool":
		args := s.Args
		if args == "" {
			args = "{}"
		}
		m.sse(w, map[string]any{"type": "message_start", "message": map[string]any{"usage": map[string]any{"input_tokens": 40}}})
		m.sse(w, map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "tool_use", "id": "toolu_1", "name": s.Name}})
		m.sse(w, map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "input_json_delta", "partial_json": args}})
		m.sse(w, map[string]any{"type": "content_block_stop", "index": 0})
		m.sse(w, map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "tool_use"}, "usage": map[string]any{"output_tokens": 5}})
		m.sse(w, map[string]any{"type": "message_stop"})
	default:
		m.sse(w, map[string]any{"type": "message_start", "message": map[string]any{"usage": map[string]any{"input_tokens": 40}}})
		m.sse(w, map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text"}})
		m.sse(w, map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": s.Text}})
		m.sse(w, map[string]any{"type": "content_block_stop", "index": 0})
		m.sse(w, map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn"}, "usage": map[string]any{"output_tokens": 5}})
		m.sse(w, map[string]any{"type": "message_stop"})
	}
}

// reset swaps both queues for a follow-up run on the same mock (counters
// included): scenarios that run sesh twice keep one endpoint.
func (m *e2eMock) reset(worker, judge []e2eStep) {
	m.mu.Lock()
	m.worker, m.judge = worker, judge
	m.wi, m.ji = 0, 0
	m.mu.Unlock()
}

// run executes one headless sesh invocation in the scenario directory.
func (m *e2eMock) run(t *testing.T, dir, prompt string, args ...string) (string, string) {
	t.Helper()
	_, out, errb := m.runCode(t, dir, prompt, args...)
	return out, errb
}

// runCode is run plus the process exit code, so scenarios can pin the
// print-mode exit-status contract.
func (m *e2eMock) runCode(t *testing.T, dir, prompt string, args ...string) (int, string, string) {
	t.Helper()
	cmd := exec.Command(seshBin, append([]string{"-p", prompt, "-yes"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "HOME="+m.homeDir)
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		return code, out.String(), errb.String()
	case <-time.After(60 * time.Second):
		cmd.Process.Kill()
		t.Fatal("sesh run timed out")
		return 0, "", "" // unreachable
	}
}

// lastToolResult digs the newest tool result out of the n-th captured worker
// request (1-based), so scenarios assert exactly what the model received.
func (m *e2eMock) lastToolResult(t *testing.T, n int) string {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	var workerReqs []capturedReq
	for _, r := range m.reqs {
		if r.Class == "worker" {
			workerReqs = append(workerReqs, r)
		}
	}
	if n < 1 || n > len(workerReqs) {
		t.Fatalf("worker request %d not captured (%d total)", n, len(workerReqs))
	}
	body := workerReqs[n-1].Body
	msgs, _ := body["messages"].([]any)
	for i := len(msgs) - 1; i >= 0; i-- {
		mm, ok := msgs[i].(map[string]any)
		if !ok || mm["role"] != "tool" {
			continue
		}
		c, _ := mm["content"].(string)
		return c
	}
	t.Fatalf("request %d carries no tool result", n)
	return ""
}

// anthropicReqs returns the captured anthropic-protocol bodies in order.
func (m *e2eMock) anthropicReqs(t *testing.T) []map[string]any {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []map[string]any
	for _, r := range m.reqs {
		if r.Class == "anthropic" {
			out = append(out, r.Body)
		}
	}
	return out
}

// seedSession writes a resumable session file into the rig's HOME so a -p
// -continue run is tied to persisted history.
func (m *e2eMock) seedSession(t *testing.T, dir string) {
	t.Helper()
	s := map[string]any{
		"id": "20260101-000000-00000001", "title": "seed", "cwd": dir,
		"provider": "mock", "protocol": "openai", "url": m.srv.URL + "/v1", "model": "mock-model",
		"created": "2026-01-01T00:00:00Z", "updated": "2026-01-01T00:00:00Z",
		"turns": []any{
			map[string]any{"role": "user", "text": "hi"},
			map[string]any{"role": "assistant", "text": "hello"},
		},
	}
	b, _ := json.MarshalIndent(s, "", "  ")
	sdir := filepath.Join(m.homeDir, ".sesh", "sessions")
	os.MkdirAll(sdir, 0o755)
	if err := os.WriteFile(filepath.Join(sdir, "20260101-000000-00000001.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func savedSessionText(t *testing.T, home string) string {
	t.Helper()
	sdir := filepath.Join(home, ".sesh", "sessions")
	entries, err := os.ReadDir(sdir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") || strings.Contains(e.Name(), "precompact") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(sdir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	t.Fatal("no saved session")
	return ""
}

// ---------------------------------------------------------------------------
// The binary: built once per test run.
// ---------------------------------------------------------------------------

var (
	seshBinOnce sync.Once
	seshBin     string
	seshBinErr  error
)

func buildSesh(t *testing.T) string {
	t.Helper()
	seshBinOnce.Do(func() {
		_, thisFile, _, _ := runtime.Caller(0)
		root := filepath.Dir(filepath.Dir(thisFile))
		out := filepath.Join(t.TempDir(), "sesh")
		cmd := exec.Command("go", "build", "-o", out, "./cmd/sesh")
		cmd.Dir = root
		if b, err := cmd.CombinedOutput(); err != nil {
			seshBinErr = fmt.Errorf("go build: %v: %s", err, b)
			return
		}
		seshBin = out
	})
	if seshBinErr != nil {
		t.Skipf("cannot build binary: %v", seshBinErr)
	}
	return seshBin
}

// ---------------------------------------------------------------------------
// Scenarios.
// ---------------------------------------------------------------------------

func TestE2E(t *testing.T) {
	if os.Getenv("SESH_E2E") == "" {
		t.Skip("end-to-end rig; set SESH_E2E=1 (builds the binary, runs it headless against a scripted endpoint)")
	}
	buildSesh(t)

	t.Run("WriteKeepsMode", func(t *testing.T) {
		m, dir := newRig(t,
			[]e2eStep{
				eTool("write", `{"path":"run.sh","content":"#!/bin/sh\necho new\n"}`),
				eTool("write", `{"path":"secret.conf","content":"new"}`),
				eText("wrote both"),
			},
			[]e2eStep{verdictJSON("written")})
		os.WriteFile(filepath.Join(dir, "run.sh"), []byte("#!/bin/sh\necho old\n"), 0o755)
		os.Chmod(filepath.Join(dir, "run.sh"), 0o755)
		os.WriteFile(filepath.Join(dir, "secret.conf"), []byte("old"), 0o600)
		os.Chmod(filepath.Join(dir, "secret.conf"), 0o600)

		out, _ := m.run(t, dir, "overwrite both files")
		if !strings.Contains(out, "wrote both") {
			t.Fatalf("run did not complete: %q", out)
		}
		for _, c := range []struct {
			path string
			want os.FileMode
		}{{"run.sh", 0o755}, {"secret.conf", 0o600}} {
			fi, err := os.Stat(filepath.Join(dir, c.path))
			if err != nil {
				t.Fatal(err)
			}
			if fi.Mode().Perm() != c.want {
				t.Fatalf("%s: mode %o, want %o (write must preserve permissions)", c.path, fi.Mode().Perm(), c.want)
			}
		}
	})

	t.Run("FailedIterationKeepsWork", func(t *testing.T) {
		m, dir := newRig(t,
			[]e2eStep{
				eTool("write", `{"path":"out1.txt","content":"one"}`),
				eText("wrote out1"),
				eTool("write", `{"path":"out2.txt","content":"two"}`),
				{Kind: "error", Status: 400, Msg: "mock injected failure"},
			},
			[]e2eStep{
				eText(`{"done": false, "blocked": false, "reason": "insufficient evidence: verify"}`),
				verdictJSON("verified after retry"),
			})
		m.seedSession(t, dir)

		out, stderr := m.run(t, dir, "write out1.txt then keep going until verified", "-continue")
		if !strings.Contains(out, "wrote out1") || !strings.Contains(stderr, "done after 2 iterations") {
			t.Fatalf("drive must recover from the injected failure and finish: out=%q stderr=%s", out, stderr)
		}
		if _, err := os.Stat(filepath.Join(dir, "out2.txt")); err != nil {
			t.Fatal("out2.txt must exist on disk")
		}
		if saved := savedSessionText(t, m.homeDir); !strings.Contains(saved, "out2") {
			t.Fatal("a completed write must survive a later failure inside the saved transcript (its side effect is on disk)")
		}
	})

	t.Run("JudgeProseFallsBack", func(t *testing.T) {
		m, dir := newRig(t,
			[]e2eStep{
				eTool("write", `{"path":"a.txt","content":"x"}`),
				eText("wrote a.txt"),
				eText("ran the build and tests; all green"),
			},
			[]e2eStep{
				eText("The work appears complete to me."),
				eText("Still looks complete."),
				verdictJSON("verified"),
			})
		out, stderr := m.run(t, dir, "write a.txt containing x")
		if !strings.Contains(out, "all green") {
			t.Fatalf("final reply missing: %q", out)
		}
		if !strings.Contains(stderr, "unparseable twice") {
			t.Fatalf("two prose verdicts must trigger the fallback, stderr:\n%s", stderr)
		}
	})

	t.Run("JudgeProseRetrySucceeds", func(t *testing.T) {
		m, dir := newRig(t,
			[]e2eStep{eTool("write", `{"path":"b.txt","content":"y"}`), eText("wrote b.txt")},
			[]e2eStep{eText("looks done to me"), verdictJSON("ok")})
		out, stderr := m.run(t, dir, "write b.txt containing y")
		if !strings.Contains(out, "wrote b.txt") {
			t.Fatalf("final reply missing: %q", out)
		}
		if !strings.Contains(stderr, "asking once more") {
			t.Fatalf("one prose verdict must be retried as JSON-only, stderr:\n%s", stderr)
		}
	})

	// Test the print-mode wire rather than the helper: this reaches the repl
	// literal that must retain provider configuration. Breaker: make drive use
	// r.p for the judge and its request carries mock-model instead of cheap-rig.
	t.Run("PrintJudgeModelDial", func(t *testing.T) {
		m, dir := newRig(t,
			[]e2eStep{eTool("write", `{"path":"judge.txt","content":"ok"}`), eText("wrote judge.txt")},
			[]e2eStep{verdictJSON("judge.txt exists")})
		if err := os.MkdirAll(filepath.Join(dir, ".sesh"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".sesh", "tuning.json"), []byte(`{"judge_model":"cheap-rig"}`), 0o644); err != nil {
			t.Fatal(err)
		}

		m.run(t, dir, "write judge.txt")
		m.mu.Lock()
		defer m.mu.Unlock()
		var workerModels, judgeModels []string
		for _, req := range m.reqs {
			model, _ := req.Body["model"].(string)
			switch req.Class {
			case "worker":
				workerModels = append(workerModels, model)
			case "judge":
				judgeModels = append(judgeModels, model)
			}
		}
		if len(workerModels) == 0 || len(judgeModels) == 0 {
			t.Fatalf("expected worker and judge requests, got worker=%v judge=%v", workerModels, judgeModels)
		}
		for _, model := range workerModels {
			if model != "mock-model" {
				t.Fatalf("worker model changed with judge dial: %q", model)
			}
		}
		for _, model := range judgeModels {
			if model != "cheap-rig" {
				t.Fatalf("judge_model did not reach the print-mode wire: %q", model)
			}
		}
	})

	t.Run("ReadPagesToEnd", func(t *testing.T) {
		m, dir := newRig(t,
			[]e2eStep{
				eTool("read", `{"path":"big.txt"}`),
				eTool("read", `{"path":"big.txt","offset":2195,"limit":10}`),
				eText("the last line is MARKER-END"),
			},
			[]e2eStep{verdictJSON("read")})
		var lines []string
		for i := 1; i <= 2200; i++ {
			lines = append(lines, "line "+strconv.Itoa(i)+" "+strings.Repeat("pad ", 12))
		}
		lines[0] = "MARKER-START " + strings.Repeat("pad ", 12)
		lines[2199] = "MARKER-END"
		os.WriteFile(filepath.Join(dir, "big.txt"), []byte(strings.Join(lines, "\n")+"\n"), 0o644)

		out, _ := m.run(t, dir, "read big.txt and tell me the last line")
		if !strings.Contains(out, "MARKER-END") {
			t.Fatalf("model could not reach the end: %q", out)
		}
		if res := m.lastToolResult(t, 2); !strings.Contains(res, "MARKER-START") || !strings.Contains(res, "re-read with offset") {
			t.Fatalf("unpaged read must end with paging guidance, got tail:\n%s", res[len(res)-200:])
		}
		if res := m.lastToolResult(t, 3); !strings.Contains(res, "MARKER-END") || !strings.Contains(res, "(lines 2195-2200 of 2200)") {
			t.Fatalf("paged read must carry the exact window, got:\n%s", res)
		}
	})

	t.Run("SearchDisclosesOversize", func(t *testing.T) {
		m, dir := newRig(t,
			[]e2eStep{eTool("search", `{"pattern":"zzz_needle"}`), eText("searched")},
			[]e2eStep{verdictJSON("searched")})
		os.WriteFile(filepath.Join(dir, "small.txt"), []byte("zzz_needle here\n"), 0o644)
		os.WriteFile(filepath.Join(dir, "big.bin"), []byte("zzz_needle head\n"+strings.Repeat("filler\n", 400000)), 0o644)

		m.run(t, dir, "search for zzz_needle everywhere")
		if res := m.lastToolResult(t, 2); !strings.Contains(res, "small.txt") || !strings.Contains(res, "big.bin") || !strings.Contains(res, "not searched") {
			t.Fatalf("oversize skips must be disclosed by name, got:\n%s", res)
		}
	})

	t.Run("EmptyReplySurvivesAnthropicResume", func(t *testing.T) {
		m, dir := newRig(t, []e2eStep{eText("")}, []e2eStep{verdictJSON("x")})
		m.seedSession(t, dir)
		if _, stderr := m.run(t, dir, "first prompt", "-continue"); strings.Contains(stderr, "error") {
			t.Fatalf("first run errored: %s", stderr)
		}
		m.reset([]e2eStep{eText("second reply")}, nil)
		out, _ := m.run(t, dir, "second prompt", "-continue", "-provider", "amock")
		if !strings.Contains(out, "second reply") {
			t.Fatalf("anthropic resume failed: %q", out)
		}
		for _, body := range m.anthropicReqs(t) {
			msgs, _ := body["messages"].([]any)
			for _, mm := range msgs {
				am, ok := mm.(map[string]any)
				if !ok || am["role"] != "assistant" {
					continue
				}
				if am["content"] == nil {
					t.Fatalf("assistant turn serialized as content:null, which the Messages API rejects: %v", msgs)
				}
			}
		}
	})

	t.Run("AnthropicMaxTokensProfileAndSelfHeal", func(t *testing.T) {
		// Profile cap goes on the wire.
		m1, dir1 := newRig(t, []e2eStep{eText("ok")}, nil)
		if _, stderr := m1.run(t, dir1, "say ok", "-provider", "amock"); strings.Contains(stderr, "error") {
			t.Fatalf("profile run errored: %s", stderr)
		}
		reqs := m1.anthropicReqs(t)
		if len(reqs) == 0 || reqs[0]["max_tokens"].(float64) != 4096 {
			t.Fatalf("profile max_tokens must reach the wire, got %v", reqs[0]["max_tokens"])
		}

		// A 400 naming the real cap self-heals: retry once at the stated cap.
		m2, dir2 := newRig(t,
			[]e2eStep{{Kind: "mtoverflow", Cap: 8192}, eText("healed")},
			nil)
		out, stderr := m2.run(t, dir2, "say healed", "-provider", "amock")
		if !strings.Contains(out, "healed") {
			t.Fatalf("self-heal retry must succeed, out=%q stderr=%s", out, stderr)
		}
		reqs = m2.anthropicReqs(t)
		if len(reqs) != 2 {
			t.Fatalf("overflow must produce exactly two requests, got %d", len(reqs))
		}
		if reqs[1]["max_tokens"].(float64) != 8192 {
			t.Fatalf("retry must use the server-stated cap, got %v", reqs[1]["max_tokens"])
		}
	})

	t.Run("ThinkingRoundTrip", func(t *testing.T) {
		m, dir := newRig(t,
			[]e2eStep{
				{Kind: "thinktool", Name: "write", Args: `{"path":"t.txt","content":"hi"}`},
				eText("wrote t.txt with thinking"),
			},
			[]e2eStep{verdictJSON("written")})
		os.WriteFile(filepath.Join(dir, "seed.txt"), []byte("seed\n"), 0o644)
		out, stderr := m.run(t, dir, "write t.txt", "-provider", "think")
		if !strings.Contains(out, "wrote t.txt with thinking") {
			t.Fatalf("thinking run did not finish: out=%q stderr=%s", out, stderr)
		}
		reqs := m.anthropicReqs(t)
		if len(reqs) < 2 {
			t.Fatalf("expected the tool call plus its follow-up, got %d requests", len(reqs))
		}
		first := reqs[0]
		th, ok := first["thinking"].(map[string]any)
		if !ok || th["budget_tokens"].(float64) != 5000 {
			t.Fatalf("thinking budget must reach the wire: %v", first["thinking"])
		}
		if first["max_tokens"].(float64) <= 5000 {
			t.Fatalf("max_tokens must clear the budget: %v", first["max_tokens"])
		}
		// The follow-up must lead the final assistant message with its
		// thinking block: the API rejects tool-use continuations otherwise.
		msgs, _ := reqs[1]["messages"].([]any)
		var lastAsst map[string]any
		for _, mm := range msgs {
			if x, ok := mm.(map[string]any); ok && x["role"] == "assistant" {
				lastAsst = x
			}
		}
		blocks, _ := lastAsst["content"].([]any)
		if len(blocks) == 0 {
			t.Fatal("final assistant message has no content blocks")
		}
		b0, _ := blocks[0].(map[string]any)
		if b0["type"] != "thinking" || b0["signature"] != "sig-roundtrip" {
			t.Fatalf("final assistant message must lead with its thinking block, got %v", b0)
		}
	})

	t.Run("GateModDeniesMutation", func(t *testing.T) {
		m, dir := newRig(t,
			[]e2eStep{eTool("write", `{"path":"x.txt","content":"no"}`), eText("write was denied")},
			[]e2eStep{verdictJSON("denied")})
		os.MkdirAll(filepath.Join(dir, ".sesh"), 0o755)
		os.WriteFile(filepath.Join(dir, ".sesh", "gate"), []byte("#!/bin/sh\necho 'no writes here'\nexit 1\n"), 0o755)

		m.run(t, dir, "write x.txt")
		if res := m.lastToolResult(t, 2); !strings.Contains(res, "gate mod denied") {
			t.Fatalf("the gate mod must rule the mutating call, got:\n%s", res)
		}
		if _, err := os.Stat(filepath.Join(dir, "x.txt")); err == nil {
			t.Fatal("a denied write must not touch disk")
		}
	})

	// The print-mode exit-status contract, through the real binary. A
	// scriptable caller must tell done, blocked-on-user, and operational
	// failure apart. Breaker: drop the driveBlocked case from the print-mode
	// exit switch and PrintExitBlocked sees 0, not 2; return driveBlocked on
	// judge transport failure (the old conflation) and PrintExitJudgeFail
	// sees 2, not 1.
	t.Run("PrintExitDone", func(t *testing.T) {
		m, dir := newRig(t,
			[]e2eStep{eTool("write", `{"path":"x.txt","content":"hi"}`), eText("wrote x.txt")},
			[]e2eStep{verdictJSON("x.txt exists with the asked content")})
		code, out, _ := m.runCode(t, dir, "write x.txt")
		if code != 0 {
			t.Fatalf("done must exit 0, got %d", code)
		}
		if !strings.Contains(out, "wrote x.txt") {
			t.Fatalf("final reply missing: %q", out)
		}
	})
	t.Run("PrintExitBlocked", func(t *testing.T) {
		m, dir := newRig(t,
			[]e2eStep{eTool("write", `{"path":"x.txt","content":"hi"}`), eText("waiting on you")},
			[]e2eStep{eText(`{"done": false, "blocked": true, "reason": "two valid designs; the user must pick"}`)})
		code, _, errb := m.runCode(t, dir, "write x.txt")
		if code != 2 {
			t.Fatalf("blocked must exit 2, got %d; stderr:\n%s", code, errb)
		}
		if !strings.Contains(errb, "needs you") {
			t.Fatalf("blocked notice missing from stderr:\n%s", errb)
		}
	})
	t.Run("PrintExitMaxIters", func(t *testing.T) {
		notDone := eText(`{"done": false, "blocked": false, "reason": "insufficient evidence: tests not run"}`)
		m, dir := newRig(t,
			[]e2eStep{eTool("write", `{"path":"a.txt","content":"1"}`), eTool("write", `{"path":"b.txt","content":"2"}`), eText("out of iterations")},
			[]e2eStep{notDone, notDone})
		code, _, errb := m.runCode(t, dir, "write files", "-max-iters", "2")
		if code != 4 {
			t.Fatalf("max-iters must exit 4, got %d; stderr:\n%s", code, errb)
		}
	})
	t.Run("PrintExitJudgeFail", func(t *testing.T) {
		m, dir := newRig(t,
			[]e2eStep{eTool("write", `{"path":"x.txt","content":"hi"}`), eText("no verdict came")},
			[]e2eStep{{Kind: "error", Status: 500, Msg: "judge exploded"}})
		code, _, errb := m.runCode(t, dir, "write x.txt")
		if code != 1 {
			t.Fatalf("judge failure must exit 1, got %d; stderr:\n%s", code, errb)
		}
		if !strings.Contains(errb, "judge unavailable") {
			t.Fatalf("judge-unavailable notice missing from stderr:\n%s", errb)
		}
	})
	// The defect this feature exists for, end to end through the real binary: a
	// failing test run whose output exceeds the window budget must still reach
	// the model with its verdict, and the elided middle must be recoverable with
	// the read tool at the offset the pointer names.
	t.Run("OversizedOutputKeepsVerdictAndSpills", func(t *testing.T) {
		m, dir := newRig(t,
			[]e2eStep{
				eTool("bash", `{"command":"sh gen.sh"}`),
				eText("the suite failed"),
			},
			[]e2eStep{verdictJSON("reported the failure")})
		// A generator whose output brackets the budget: PASS noise around a
		// diagnostic in the middle, and the verdict on the final line.
		gen := "#!/bin/sh\n" +
			"i=0; while [ $i -lt 900 ]; do echo \"=== RUN   TestAlpha$i\"; echo \"--- PASS: TestAlpha$i (0.00s)\"; i=$((i+1)); done\n" +
			"echo '    a_test.go:403: boom: nil map write at pkg/store.go:88'\n" +
			"i=0; while [ $i -lt 900 ]; do echo \"=== RUN   TestBeta$i\"; echo \"--- PASS: TestBeta$i (0.00s)\"; i=$((i+1)); done\n" +
			"echo 'FAIL\tsigprobe/pkg\t0.013s'\n"
		os.WriteFile(filepath.Join(dir, "gen.sh"), []byte(gen), 0o755)

		m.run(t, dir, "run the suite and tell me whether it passed")

		res := m.lastToolResult(t, 2)
		// The judge rules done on the transcript, so the verdict must survive the
		// per-result elision too: a head-only cut fed it a wall of PASS lines
		// from a run that failed.
		judged := false
		m.mu.Lock()
		for _, r := range m.reqs {
			if r.Class != "judge" {
				continue
			}
			msgs, _ := r.Body["messages"].([]any)
			for _, mm := range msgs {
				x, ok := mm.(map[string]any)
				if !ok {
					continue
				}
				if c, _ := x["content"].(string); strings.Contains(c, "FAIL\tsigprobe/pkg") {
					judged = true
				}
			}
		}
		m.mu.Unlock()
		if !judged {
			t.Error("the judge must see the failing verdict in the transcript")
		}
		if len(res) > tune.ResultMaxChars {
			t.Fatalf("result reached the model unbounded: %d bytes", len(res))
		}
		if !strings.Contains(res, "FAIL\tsigprobe/pkg") {
			t.Fatalf("the verdict on the last line must survive:\n%s", tailOf(res, 400))
		}
		if !strings.Contains(res, "TestAlpha0") {
			t.Fatalf("the head must survive:\n%s", headOf(res, 400))
		}

		// The pointer must name a real file, and it must hold what was elided.
		i := strings.Index(res, "full output: ")
		if i < 0 {
			t.Fatalf("no spill pointer in the shaped result:\n%s", res[max(0, len(res)-400):])
		}
		path := res[i+len("full output: "):]
		path = path[:strings.Index(path, " ")]
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("spilled output unreadable at the path the model was given: %v", err)
		}
		if !strings.Contains(string(body), "boom: nil map write") {
			t.Fatal("the spilled file must hold the elided diagnostic")
		}
		if !strings.Contains(string(body), "TestBeta450") {
			t.Fatal("the spilled file must hold the full output, not just the shaped ends")
		}
	})
}

func headOf(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func tailOf(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
