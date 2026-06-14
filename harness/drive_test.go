package harness

import (
	"context"
	"strings"
	"testing"

	"github.com/mike-diff/sesh/agent"
)

// seqChat runs each call through the next scripted function, so a test can
// interleave worker replies, judge verdicts, and injected errors.
type seqChat struct {
	fns []func(ctx context.Context) (agent.Reply, error)
	i   int
}

func (s *seqChat) Chat(ctx context.Context, _ string, _ []agent.Turn, _ []agent.ToolDef, onText, _ func(string)) (agent.Reply, error) {
	if s.i >= len(s.fns) {
		return agent.Reply{}, context.DeadlineExceeded
	}
	fn := s.fns[s.i]
	s.i++
	r, err := fn(ctx)
	if err == nil {
		onText(r.Text)
	}
	return r, err
}

func reply(text string) func(context.Context) (agent.Reply, error) {
	return func(context.Context) (agent.Reply, error) {
		return agent.Reply{Text: text}, nil
	}
}

// replyU is reply with reported usage, for accounting tests.
func replyU(text string, in, out int) func(context.Context) (agent.Reply, error) {
	return func(context.Context) (agent.Reply, error) {
		return agent.Reply{Text: text, Usage: agent.Usage{Input: in, Output: out}}, nil
	}
}

// workTurns is a first turn that did real work (contains tool activity).
func workTurns() []agent.Turn {
	return []agent.Turn{
		{Role: "user", Text: "fix the bug"},
		{Role: "assistant", Calls: []agent.ToolCall{{ID: "1", Name: "edit"}}},
		{Role: "tool", Results: []agent.ToolResult{{ID: "1", Content: "applied"}}},
		{Role: "assistant", Text: "patched it"},
	}
}

func chatTurns() []agent.Turn {
	return turnsOf("what does this do?", "it parses configs")
}

func driveRepl(t *testing.T, p agent.Provider, first []agent.Turn) *repl {
	t.Helper()
	r := newTestRepl(t)
	r.p = p
	r.history = first
	r.sess.Turns = first
	return r
}

func counting() (bump func(), count func() int) {
	n := 0
	return func() { n++ }, func() int { return n }
}

func TestWorkedOn(t *testing.T) {
	if !workedOn(workTurns()) {
		t.Fatal("tool activity is work")
	}
	if workedOn(chatTurns()) {
		t.Fatal("plain conversation is not work")
	}
}

// TestDriveJudgeCancel: Ctrl-C during the judge phase pauses the drive instead
// of reading as a dead judge. Breaker: drop the isCanceled check in the judge
// error path and a cancel returns driveBlocked, not driveInterrupted.
func TestDriveJudgeCancel(t *testing.T) {
	p := &seqChat{fns: []func(context.Context) (agent.Reply, error){
		func(context.Context) (agent.Reply, error) { return agent.Reply{}, context.Canceled }, // the judge call
	}}
	r := driveRepl(t, p, workTurns())
	got := drive(r, driveConfig{request: "fix the bug", maxIters: 25, say: func(string, ...any) {}}, workTurns())
	if got != driveInterrupted {
		t.Fatalf("a cancelled judge must pause the drive (driveInterrupted=%d), got %d", driveInterrupted, got)
	}
}

func TestParseVerdict(t *testing.T) {
	v, err := parseVerdict(`{"done": true, "blocked": false, "reason": "tests pass"}`)
	if err != nil || !v.Done || v.Reason != "tests pass" {
		t.Fatalf("clean JSON: %+v err=%v", v, err)
	}
	v, err = parseVerdict("verdict:\n```json\n{\"done\": false, \"blocked\": true, \"reason\": \"needs a decision\"}\n```")
	if err != nil || v.Done || !v.Blocked {
		t.Fatalf("fenced JSON: %+v err=%v", v, err)
	}
	if _, err = parseVerdict("looks done to me"); err == nil {
		t.Fatal("prose without JSON must error")
	}
}

// TestDriveSkipsConversation: a turn with no tool use is never judged: the
// provider would error if called at all.
func TestDriveSkipsConversation(t *testing.T) {
	r := driveRepl(t, &seqChat{}, chatTurns()) // any call would fail
	_, count := counting()
	if code := drive(r, driveConfig{request: "what does this do?", maxIters: 25, mutations: count}, chatTurns()); code != driveDone {
		t.Fatalf("conversation must not drive: %d", code)
	}
}

// TestDriveRespectsSingleTurnDial: -max-iters 1 restores the one-shot
// contract: no judge call, no iterations.
func TestDriveRespectsSingleTurnDial(t *testing.T) {
	r := driveRepl(t, &seqChat{}, workTurns())
	_, count := counting()
	if code := drive(r, driveConfig{request: "fix", maxIters: 1, mutations: count}, workTurns()); code != driveDone {
		t.Fatalf("maxIters 1 must not drive: %d", code)
	}
}

// TestDriveDoneFirstVerdict: the judge ruling done on the first turn adds
// nothing to history.
func TestDriveDoneFirstVerdict(t *testing.T) {
	p := &seqChat{fns: []func(context.Context) (agent.Reply, error){
		reply(`{"done": true, "blocked": false, "reason": "transcript shows the edit applied and verified"}`),
	}}
	r := driveRepl(t, p, workTurns())
	_, count := counting()
	if code := drive(r, driveConfig{request: "fix the bug", maxIters: 25, mutations: count}, workTurns()); code != driveDone {
		t.Fatalf("code %d", code)
	}
	if len(r.history) != len(workTurns()) {
		t.Fatal("a done verdict must not grow history")
	}
}

// TestDriveContinueThreadsReason: not-done runs another iteration whose
// opening carries the judge's reason and the original request; the second
// verdict ends it.
func TestDriveContinueThreadsReason(t *testing.T) {
	p := &seqChat{fns: []func(context.Context) (agent.Reply, error){
		reply(`{"done": false, "blocked": false, "reason": "insufficient evidence: no test run; run go test"}`),
		reply("ran the tests, all green"),
		reply(`{"done": true, "blocked": false, "reason": "tests shown passing"}`),
	}}
	r := driveRepl(t, p, workTurns())
	bump, count := counting()
	cfg := driveConfig{request: "fix the bug", maxIters: 5,
		mutations: func() int { bump(); return count() }}
	if code := drive(r, cfg, workTurns()); code != driveDone {
		t.Fatalf("code %d", code)
	}
	opening := r.history[len(workTurns())].Text
	if !strings.Contains(opening, "run go test") || !strings.Contains(opening, "fix the bug") {
		t.Fatalf("continuation must carry the verdict and the request: %.200s", opening)
	}
}

// TestDriveAccumulatesTokens: every drive iteration's usage must reach the
// status-line totals, not just the first sub-turn. Breaker: drop the
// account() call in drive's loop and the worker iteration's tokens vanish
// from totIn/totOut, exactly the bug that froze the count on long worked
// turns. Pre-seeded totals prove accumulation, not assignment.
func TestDriveAccumulatesTokens(t *testing.T) {
	p := &seqChat{fns: []func(context.Context) (agent.Reply, error){
		reply(`{"done": false, "blocked": false, "reason": "no test run yet"}`),    // judge: iter 1
		replyU("ran the tests, green", 500, 80),                                    // worker: iter 1
		reply(`{"done": true, "blocked": false, "reason": "tests shown passing"}`), // judge: iter 2
	}}
	r := driveRepl(t, p, workTurns())
	r.totIn, r.totOut = 40, 7 // pretend the first sub-turn already counted
	bump, count := counting()
	cfg := driveConfig{request: "fix the bug", maxIters: 5,
		mutations: func() int { bump(); return count() }}
	if code := drive(r, cfg, workTurns()); code != driveDone {
		t.Fatalf("code %d", code)
	}
	if r.totIn != 540 || r.totOut != 87 {
		t.Fatalf("drive must fold each iteration's usage into the totals: in=%d (want 540) out=%d (want 87)", r.totIn, r.totOut)
	}
	if r.ctxTokens != 500 {
		t.Fatalf("ctx gauge must track the last iteration's prompt size: %d (want 500)", r.ctxTokens)
	}
}

// TestDriveBlocked: a blocked verdict hands back to the user immediately.
func TestDriveBlocked(t *testing.T) {
	p := &seqChat{fns: []func(context.Context) (agent.Reply, error){
		reply(`{"done": false, "blocked": true, "reason": "two valid schema designs; the user must pick"}`),
	}}
	r := driveRepl(t, p, workTurns())
	_, count := counting()
	if code := drive(r, driveConfig{request: "design the schema", maxIters: 25, mutations: count}, workTurns()); code != driveBlocked {
		t.Fatalf("code %d", code)
	}
}

// TestDriveStuck: iterations that mutate nothing stop as stuck before the
// iteration cap.
func TestDriveStuck(t *testing.T) {
	notDone := reply(`{"done": false, "blocked": false, "reason": "still failing"}`)
	p := &seqChat{fns: []func(context.Context) (agent.Reply, error){
		notDone, reply("try"), notDone, reply("try"), notDone, reply("try"),
	}}
	r := driveRepl(t, p, workTurns())
	_, count := counting() // never bumped: zero progress
	if code := drive(r, driveConfig{request: "fix", maxIters: 10, mutations: count}, workTurns()); code != driveStuck {
		t.Fatalf("code %d", code)
	}
}

// TestDriveMaxIters: visible progress without completion runs out the cap.
func TestDriveMaxIters(t *testing.T) {
	notDone := reply(`{"done": false, "blocked": false, "reason": "more to do"}`)
	p := &seqChat{fns: []func(context.Context) (agent.Reply, error){
		notDone, reply("work"), notDone,
	}}
	r := driveRepl(t, p, workTurns())
	bump, count := counting()
	cfg := driveConfig{request: "fix", maxIters: 2,
		mutations: func() int { bump(); return count() }}
	if code := drive(r, cfg, workTurns()); code != driveMaxIters {
		t.Fatalf("code %d", code)
	}
}

// TestDriveInterrupted: Ctrl-C during an iteration pauses cleanly: the
// unconsumed opening is rolled back so the next message cannot produce
// consecutive user turns.
func TestDriveInterrupted(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	p := &seqChat{fns: []func(context.Context) (agent.Reply, error){
		reply(`{"done": false, "blocked": false, "reason": "keep going"}`),
		func(ctx context.Context) (agent.Reply, error) { return agent.Reply{}, ctx.Err() },
	}}
	r := driveRepl(t, p, workTurns())
	_, count := counting()
	cfg := driveConfig{request: "fix", maxIters: 5, mutations: count,
		turnCtx: func() (context.Context, func()) { return cancelled, func() {} }}
	if code := drive(r, cfg, workTurns()); code != driveInterrupted {
		t.Fatalf("code %d", code)
	}
	if len(r.history) != len(workTurns()) {
		t.Fatal("the interrupted opening must be rolled back")
	}
}

// TestDriveJudgeUnavailable: no verdict means no mandate to keep spending.
func TestDriveJudgeUnavailable(t *testing.T) {
	p := &seqChat{} // judge call errors immediately
	r := driveRepl(t, p, workTurns())
	_, count := counting()
	if code := drive(r, driveConfig{request: "fix", maxIters: 5, mutations: count}, workTurns()); code != driveBlocked {
		t.Fatalf("code %d", code)
	}
}

// promptSpy records the prompt the judge actually received.
type promptSpy struct {
	saw   *string
	reply string
}

func (p promptSpy) Chat(_ context.Context, _ string, h []agent.Turn, _ []agent.ToolDef, onText, _ func(string)) (agent.Reply, error) {
	*p.saw = h[len(h)-1].Text
	onText(p.reply)
	return agent.Reply{Text: p.reply}, nil
}
