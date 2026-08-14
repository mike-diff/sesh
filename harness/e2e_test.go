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
		m.capture(r, "anthropic")
		w.Header().Set("Content-Type", "text/event-stream")
		step, ok := m.popWorker()
		if !ok {
			w.WriteHeader(500)
			fmt.Fprintf(w, `{"error":{"message":"mock worker queue empty"}}`)
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
	case <-done:
	case <-time.After(60 * time.Second):
		cmd.Process.Kill()
		t.Fatal("sesh run timed out")
	}
	return out.String(), errb.String()
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
}
