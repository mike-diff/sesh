package harness

import (
	"bufio"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/mike-diff/sesh/agent"
	"github.com/mike-diff/sesh/provider"
)

// fakeChat is a Provider that returns one fixed reply (or error), for testing
// product-layer logic that drives the loop, like /compact.
type fakeChat struct {
	text string
	err  error
}

func (f fakeChat) Chat(_ context.Context, _ string, _ []agent.Turn, _ []agent.ToolDef, onText, _ func(string)) (agent.Reply, error) {
	if f.err != nil {
		return agent.Reply{}, f.err
	}
	onText(f.text)
	return agent.Reply{Text: f.text}, nil
}

// fakeLister is a Provider that also answers model discovery, for testing
// /reload.
type fakeLister struct {
	fakeChat
	models []string
}

func (f fakeLister) ListModelInfos(_ context.Context) ([]provider.ModelInfo, error) {
	infos := make([]provider.ModelInfo, len(f.models))
	for i, m := range f.models {
		infos[i] = provider.ModelInfo{ID: m}
	}
	return infos, nil
}

// newTestRepl builds a repl wired to an isolated HOME and an endpoint that
// fails fast (nothing listens on the URL), so commands exercise their state
// transitions without a network or a terminal.
func newTestRepl(t *testing.T) *repl {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	return &repl{
		protocol: "openai", url: "http://127.0.0.1:0", model: "m0",
		pcfg:  ProvidersConfig{Providers: map[string]Profile{}},
		creds: map[string]string{},
		sess:  &Session{ID: "test"},
		con:   &plainConsole{in: bufio.NewReader(strings.NewReader(""))},
	}
}

// TestReloadCmd: /reload re-runs discovery against the live
// provider and replaces the cached model list. Breaker: drop the discoverModels
// reassignment in reloadCmd and the stale list survives the call.
func TestReloadCmd(t *testing.T) {
	r := newTestRepl(t)
	r.models = []string{"stale"}
	r.p = fakeLister{models: []string{"alpha", "beta", "gamma"}}
	if h, _ := r.command("/reload"); !h {
		t.Fatal("/reload must be handled as a command")
	}
	if got := strings.Join(r.models, ","); got != "alpha,beta,gamma" {
		t.Fatalf("model list not refreshed from the provider: %q", got)
	}
}

// TestSlashCommandCompletionCoversDispatch: every dispatched command is also
// tab-completable, because both derive from slashCommands. Breaker: hardcode the
// completion list and let it drift, and a command goes missing from completion.
func TestSlashCommandCompletionCoversDispatch(t *testing.T) {
	r := newTestRepl(t)
	completed := map[string]bool{}
	for _, c := range r.completions("/") {
		completed[strings.TrimSpace(c)] = true
	}
	for _, c := range slashCommands {
		if !completed[c.name] {
			t.Fatalf("%s is dispatched but not tab-completed", c.name)
		}
	}
	if got := r.completions("/up"); len(got) != 1 || strings.TrimSpace(got[0]) != "/update" {
		t.Fatalf("/up should complete to /update, got %v", got)
	}
}

// TestCustomModelPersists: a user-typed model is saved per provider, stays in
// the /model choices, and only one is kept. Breaker: stop writing
// Profile.CustomModel and the choice vanishes on the next session.
func TestCustomModelPersists(t *testing.T) {
	r := newTestRepl(t)
	saveGlobalProviders(ProvidersConfig{Default: "p", Providers: map[string]Profile{
		"p": {Protocol: "openai", URL: "http://127.0.0.1:0", Model: "m1"},
	}})
	r.pcfg = loadProviders()
	r.current, r.protocol, r.url = "p", "openai", "http://127.0.0.1:0"

	r.setCustomModel("my-custom-model")
	if r.model != "my-custom-model" {
		t.Fatalf("a custom model must become active: %q", r.model)
	}
	if got := loadGlobalProviders().Providers["p"].CustomModel; got != "my-custom-model" {
		t.Fatalf("a custom model must persist to the profile: %q", got)
	}
	if !slices.Contains(r.modelChoices(), "my-custom-model") {
		t.Fatalf("a custom model must stay in /model choices: %v", r.modelChoices())
	}

	r.setCustomModel("second") // only one per provider: replaces the first
	if got := loadGlobalProviders().Providers["p"].CustomModel; got != "second" {
		t.Fatalf("only one custom model per provider, want \"second\": %q", got)
	}
	if slices.Contains(r.modelChoices(), "my-custom-model") {
		t.Fatalf("the replaced custom model should be gone: %v", r.modelChoices())
	}
}

// TestUpdateExecArgs: the post-update re-exec resumes this exact session and
// carries the safety flags. Breaker: stop appending -ask/-unsafe-paths and a
// reload silently drops the session's safety posture.
func TestUpdateExecArgs(t *testing.T) {
	r := newTestRepl(t) // newTestRepl sets sess.ID == "test"
	if got := strings.Join(r.updateExecArgs("/usr/bin/sesh"), " "); got != "/usr/bin/sesh -resume test" {
		t.Fatalf("plain reload args: %q", got)
	}
	r.ask, r.unsafePaths = true, true
	if got := strings.Join(r.updateExecArgs("/usr/bin/sesh"), " "); got != "/usr/bin/sesh -resume test -ask -unsafe-paths" {
		t.Fatalf("safety flags must carry across reload: %q", got)
	}
}

// TestUpdatedNotice: a post-update reload announces the new build and the one it
// replaced, and stays silent otherwise. Breaker: always returning a line makes
// every normal startup falsely claim an update.
func TestUpdatedNotice(t *testing.T) {
	if got := updatedNotice("", "newsha"); got != "" {
		t.Fatalf("a normal start must not claim an update: %q", got)
	}
	if got := updatedNotice("same", "same"); got != "" {
		t.Fatalf("no change must be silent: %q", got)
	}
	if got := updatedNotice("oldsha", "newsha"); got != "updated to newsha (was oldsha)" {
		t.Fatalf("update notice wrong: %q", got)
	}
}

// TestSettingsCmd: /settings is a looping picker: a selection toggles the
// setting and the menu reopens until cancelled. Breakers: drop the toggle and
// showThink never flips; drop the loop and the second pick in one invocation
// never lands, leaving showThink on instead of back off.
func TestSettingsCmd(t *testing.T) {
	r := newTestRepl(t)
	r.con = &plainConsole{in: bufio.NewReader(strings.NewReader("1\n1\n\n"))}
	if h, _ := r.command("/settings"); !h {
		t.Fatal("/settings must be a command")
	}
	if r.showThink {
		t.Fatal("two picks of the thinking entry must toggle it on and back off")
	}
	r.con = &plainConsole{in: bufio.NewReader(strings.NewReader("1\n\n"))}
	r.command("/settings")
	if !r.showThink {
		t.Fatal("one pick must toggle thinking on")
	}
}

func TestReplDispatch(t *testing.T) {
	r := newTestRepl(t)
	if h, q := r.command("exit"); !h || !q {
		t.Fatal("exit should quit")
	}
	if h, q := r.command("quit"); !h || !q {
		t.Fatal("quit should quit")
	}
	if h, q := r.command("/exit"); !h || !q {
		t.Fatal("/exit should quit")
	}
	if h, q := r.command("/quit"); !h || !q {
		t.Fatal("/quit should quit")
	}
	if h, _ := r.command("fix the tests"); h {
		t.Fatal("chat lines are not commands")
	}
	if h, q := r.command("/model"); !h || q {
		t.Fatal("/model is a command and does not quit")
	}
	if h, _ := r.command("/provider"); !h {
		t.Fatal("/provider is a command")
	}
}

func TestReplModelSwitch(t *testing.T) {
	r := newTestRepl(t)
	// no discovery list: switch by name is allowed
	if h, _ := r.command("/model m2"); !h {
		t.Fatal("not handled")
	}
	if r.model != "m2" || r.sess.Model != "m2" {
		t.Fatalf("model switch: repl=%q session=%q", r.model, r.sess.Model)
	}
	if r.p == nil {
		t.Fatal("provider should be rebuilt for the new model")
	}
}

// TestReplModelGuard: with a discovered list, a model the endpoint does not
// serve is refused instead of 404ing the next turn, and a model that is
// another provider's default hops to that provider.
func TestReplModelGuard(t *testing.T) {
	r := newTestRepl(t)
	r.current = "local"
	r.models = []string{"m0", "m2"}
	r.pcfg = ProvidersConfig{Providers: map[string]Profile{
		"local":  {Protocol: "openai", URL: "http://127.0.0.1:0", Model: "m0"},
		"remote": {Protocol: "openai", URL: "http://127.0.0.1:0", Model: "beta-x"},
	}}

	// unknown everywhere: refused, state untouched
	r.command("/model nope")
	if r.model != "m0" || r.current != "local" {
		t.Fatalf("unknown model must not switch anything: model=%q current=%q", r.model, r.current)
	}

	// another provider's default: hop providers
	r.command("/model beta-x")
	if r.current != "remote" || r.model != "beta-x" {
		t.Fatalf("should have hopped to remote: current=%q model=%q", r.current, r.model)
	}

	// on the list: plain switch, no provider change
	r.models = []string{"beta-x", "beta-y"}
	r.command("/model beta-y")
	if r.current != "remote" || r.model != "beta-y" {
		t.Fatalf("in-list switch: current=%q model=%q", r.current, r.model)
	}
}

func TestReplProviderSwitchAndRemove(t *testing.T) {
	r := newTestRepl(t)
	g := ProvidersConfig{Default: "a", Providers: map[string]Profile{
		"a": {Protocol: "openai", URL: "http://127.0.0.1:0", Model: "ma"},
		"b": {Protocol: "openai", URL: "http://127.0.0.1:0", Model: "mb"},
	}}
	if err := saveGlobalProviders(g); err != nil {
		t.Fatal(err)
	}
	r.pcfg = loadProviders()

	r.command("/provider b")
	if r.current != "b" || r.model != "mb" {
		t.Fatalf("switch: current=%q model=%q", r.current, r.model)
	}
	if r.sess.Model != "mb" {
		t.Fatalf("switch not recorded in session: %q", r.sess.Model)
	}

	r.command("/provider remove b")
	got := loadGlobalProviders()
	if _, ok := got.Providers["b"]; ok {
		t.Fatal("b should be removed from the global config")
	}
	if got.Default != "a" {
		t.Fatalf("default should fall back to a survivor: %q", got.Default)
	}
	if _, ok := r.pcfg.Providers["b"]; ok {
		t.Fatal("removal should refresh the loaded config")
	}
}

// TestReplProviderAddWizard drives the add wizard with scripted input: name,
// protocol (default), url, key (blank), default model, and (since discovery
// fails against a dead endpoint) the manual context window.
func TestReplProviderAddWizard(t *testing.T) {
	r := newTestRepl(t)
	r.con = &plainConsole{in: bufio.NewReader(strings.NewReader("loc\n\nhttp://127.0.0.1:0\n\nmx\n128000\n"))}
	if h, _ := r.command("/provider add"); !h {
		t.Fatal("not handled")
	}
	g := loadGlobalProviders()
	p, ok := g.Providers["loc"]
	if !ok || p.Model != "mx" || p.URL != "http://127.0.0.1:0" || p.Protocol != "openai" {
		t.Fatalf("wizard result: %+v ok=%v", p, ok)
	}
	if p.Context != 128000 {
		t.Fatalf("a known window should persist: %+v", p)
	}
	if g.Default != "loc" {
		t.Fatalf("first provider should become the default: %q", g.Default)
	}
	if r.current != "loc" || r.model != "mx" {
		t.Fatalf("wizard should switch to the new provider: %q %q", r.current, r.model)
	}
}

// TestModelSwitchAdoptsDiscoveredContext: switching models retunes the window
// to what the endpoint published, so handoff timing follows the model.
func TestModelSwitchAdoptsDiscoveredContext(t *testing.T) {
	r := newTestRepl(t)
	r.models = []string{"m1", "m2"}
	r.modelCtx = map[string]int{"m2": 200000}
	r.ctxLimit = 32768
	r.command("/model m2")
	if r.model != "m2" || r.ctxLimit != 200000 {
		t.Fatalf("discovered context should follow the switch: model=%s ctx=%d", r.model, r.ctxLimit)
	}
	// switching to a model with no published window keeps the old limit
	r.command("/model m1")
	if r.ctxLimit != 200000 {
		t.Fatalf("unknown window must not zero the limit: %d", r.ctxLimit)
	}
}

func TestLastText(t *testing.T) {
	if got := lastText(nil); got != "" {
		t.Fatalf("empty history: %q", got)
	}
	turns := []agent.Turn{
		{Role: "user", Text: "q"},
		{Role: "assistant", Text: "first"},
		{Role: "assistant", Calls: []agent.ToolCall{{ID: "1"}}}, // no text
		{Role: "tool"},
		{Role: "assistant", Text: "final"},
		{Role: "user", Text: "trailing user text must not win"},
	}
	if got := lastText(turns); got != "final" {
		t.Fatalf("lastText: %q", got)
	}
}

func TestCompactHistory(t *testing.T) {
	four := []agent.Turn{
		{Role: "user", Text: "a"}, {Role: "assistant", Text: "b"},
		{Role: "user", Text: "c"}, {Role: "assistant", Text: "d"},
	}

	// too short to compact: unchanged
	short := four[:2]
	if got := compactHistory(fakeChat{text: "S"}, "sys", short); len(got) != 2 {
		t.Fatalf("short history should be unchanged: %d turns", len(got))
	}

	// success: two turns, the summary in the assistant turn
	got := compactHistory(fakeChat{text: "THE SUMMARY"}, "sys", four)
	if len(got) != 2 || got[0].Role != "user" || got[1].Role != "assistant" || got[1].Text != "THE SUMMARY" {
		t.Fatalf("compacted shape: %+v", got)
	}

	// provider error: unchanged
	if got := compactHistory(fakeChat{err: errors.New("boom")}, "sys", four); len(got) != 4 {
		t.Fatalf("error should leave history unchanged: %d turns", len(got))
	}

	// empty summary: unchanged
	if got := compactHistory(fakeChat{text: ""}, "sys", four); len(got) != 4 {
		t.Fatalf("empty summary should leave history unchanged: %d turns", len(got))
	}
}

// TestStickyModel: /model writes the choice back to the global profile, so
// the provider remembers its last model across hops and sessions.
func TestStickyModel(t *testing.T) {
	r := newTestRepl(t)
	g := ProvidersConfig{Default: "a", Providers: map[string]Profile{
		"a": {Protocol: "openai", URL: "http://127.0.0.1:0", Model: "m0"},
	}}
	if err := saveGlobalProviders(g); err != nil {
		t.Fatal(err)
	}
	r.pcfg = loadProviders()
	r.current = "a"

	r.command("/model m9")
	if got := loadGlobalProviders().Providers["a"].Model; got != "m9" {
		t.Fatalf("model did not stick to the profile: %q", got)
	}
	if r.pcfg.Providers["a"].Model != "m9" {
		t.Fatal("loaded config not refreshed after sticky write")
	}

	// ad hoc (no profile name): nothing written
	r.current = ""
	r.command("/model m10")
	if got := loadGlobalProviders().Providers["a"].Model; got != "m9" {
		t.Fatalf("ad hoc switch must not touch profiles: %q", got)
	}
}

func TestResolveModelArg(t *testing.T) {
	models := []string{"alpha:9b", "alpha:4b", "beta-1"}
	if m, _ := resolveModelArg("2", models); m != "alpha:4b" {
		t.Fatalf("index: %q", m)
	}
	if m, _ := resolveModelArg("beta-1", models); m != "beta-1" {
		t.Fatalf("exact: %q", m)
	}
	if m, _ := resolveModelArg("4b", models); m != "alpha:4b" {
		t.Fatalf("unique substring: %q", m)
	}
	if _, amb := resolveModelArg("alpha", models); len(amb) != 2 {
		t.Fatalf("ambiguous should report matches: %v", amb)
	}
	if m, _ := resolveModelArg("nope", models); m != "nope" {
		t.Fatalf("no match passes through: %q", m)
	}
	if m, _ := resolveModelArg("7", models); m != "7" {
		t.Fatalf("out-of-range index passes through: %q", m)
	}
}

// TestRunTurnRollback: any abnormal turn end (error or cancel) rolls the
// unconsumed user turn back out of history and the session file.
func TestRunTurnRollback(t *testing.T) {
	r := newTestRepl(t)
	r.history = []agent.Turn{{Role: "user", Text: "a"}, {Role: "assistant", Text: "b"}}

	r.p = fakeChat{err: errors.New("HTTP 500: boom")}
	r.runTurn(context.Background(), "next", nil, agent.Hooks{})
	if len(r.history) != 2 {
		t.Fatalf("error: history should be rolled back, got %d turns", len(r.history))
	}
	if len(r.sess.Turns) != 2 {
		t.Fatalf("error: session should hold the rolled-back history, got %d", len(r.sess.Turns))
	}

	r.p = fakeChat{err: context.Canceled}
	r.runTurn(context.Background(), "next", nil, agent.Hooks{})
	if len(r.history) != 2 {
		t.Fatalf("cancel: history should be rolled back, got %d turns", len(r.history))
	}

	r.p = fakeChat{text: "fine"}
	r.runTurn(context.Background(), "next", nil, agent.Hooks{})
	if len(r.history) != 4 {
		t.Fatalf("success: history should grow by 2, got %d turns", len(r.history))
	}
}

// TestRunTurnSurvivesMidTurnHandoff: when afterTurn's pressure check hands off
// (or falls back to compaction), r.history is replaced with a far shorter one,
// and the turn's produced-slice mark indexes the OLD history. Breaker: slice
// r.history[mark:] after afterTurn instead of before and this panics with
// slice bounds out of range, the crash the live bench caught.
func TestRunTurnSurvivesMidTurnHandoff(t *testing.T) {
	r := newTestRepl(t)
	for i := 0; i < 3; i++ {
		r.history = append(r.history,
			agent.Turn{Role: "user", Text: "q"}, agent.Turn{Role: "assistant", Text: "a"})
	}
	r.sess.Turns = r.history
	r.sess.save()
	r.p = fakeChat{text: "answer"}
	r.ctxLimit = 1000
	r.ctxTokens = 990 // hard zone: the next afterTurn must break the session

	turns, ok := r.runTurn(context.Background(), "final question", nil, agent.Hooks{})
	if !ok {
		t.Fatal("the turn itself succeeded and must report ok")
	}
	if len(turns) != 2 || turns[0].Text != "final question" || turns[1].Text != "answer" {
		t.Fatalf("produced turns must be the exchange that just ran, got %+v", turns)
	}
	if r.sess.ID == "test" {
		t.Fatal("no handoff happened; this test then proves nothing")
	}
}

// TestBriefWriterDial: the brief_provider/brief_model dials choose the brief
// writer, and anything unresolvable falls back to the worker. Breakers: ignore
// the dial in briefWriter and the dialed case returns the worker; fail instead
// of falling back and the bad-profile case returns nil.
func TestBriefWriterDial(t *testing.T) {
	r := newTestRepl(t)
	r.p = fakeChat{text: "x"}
	old := tune
	defer func() { tune = old }()

	tune.BriefProvider, tune.BriefModel = "", ""
	if bp, label := r.briefWriter(); bp != r.p || label != "" {
		t.Fatalf("no dial set: the worker must write its own brief, got label %q", label)
	}

	tune.BriefModel = "cheap-model"
	bp, label := r.briefWriter()
	if label != "cheap-model" {
		t.Fatalf("brief_model dial ignored: label %q", label)
	}
	if bp == nil || bp == agent.Provider(r.p) {
		t.Fatal("dialed writer must be a distinct provider on the worker's endpoint")
	}

	tune.BriefProvider, tune.BriefModel = "no-such-profile", ""
	if bp, _ := r.briefWriter(); bp != r.p {
		t.Fatal("unresolvable dial must fall back to the worker, never fail the handoff")
	}
}

func TestKeyHint(t *testing.T) {
	if h := keyHint(&provider.APIError{Status: 401, Message: "bad"}, "remote"); !strings.Contains(h, "/provider add remote") {
		t.Fatalf("401 hint: %q", h)
	}
	if h := keyHint(&provider.APIError{Status: 500, Message: "oops"}, "remote"); h != "" {
		t.Fatalf("500 must not hint: %q", h)
	}
	if h := keyHint(errors.New("plain"), "remote"); h != "" {
		t.Fatalf("non-API error must not hint: %q", h)
	}
}

// TestReplCompactWithoutProvider: /compact with no active brain must refuse
// gracefully instead of panicking on a nil provider.
func TestReplCompactWithoutProvider(t *testing.T) {
	r := newTestRepl(t)
	r.p = nil
	r.history = []agent.Turn{
		{Role: "user", Text: "a"}, {Role: "assistant", Text: "b"},
		{Role: "user", Text: "c"}, {Role: "assistant", Text: "d"},
	}
	if h, _ := r.command("/compact"); !h {
		t.Fatal("not handled")
	}
	if len(r.history) != 4 {
		t.Fatal("history must be unchanged when there is no provider")
	}
}

// TestContextCmd: /context sets the window, caps it, persists to the active
// profile, and enables automatic handoff.
func TestContextCmd(t *testing.T) {
	r := newTestRepl(t)
	r.current = "loc"
	g := ProvidersConfig{Providers: map[string]Profile{"loc": {Protocol: "openai", Model: "m"}}}
	saveGlobalProviders(g)

	r.command("/context 131072")
	if r.ctxLimit != 131072 {
		t.Fatalf("set failed: limit=%d", r.ctxLimit)
	}
	saved := loadGlobalProviders().Providers["loc"]
	if saved.Context != 131072 {
		t.Fatalf("not persisted: %+v", saved)
	}

	r.command("/context 1000000")
	if r.ctxLimit != tune.MaxUsefulContext {
		t.Fatalf("oversized window must cap: %d", r.ctxLimit)
	}
	if h, _ := r.command("/context nonsense"); !h {
		t.Fatal("bad input must still be handled")
	}
}

// TestBudgetGate: the unattended tool budget refuses past the cap with a
// model-readable error and is off by default.
func TestBudgetGate(t *testing.T) {
	inner := func(agent.ToolCall) error { return nil }
	g := budgetGate(2, inner)
	if g(agent.ToolCall{Name: "read"}) != nil || g(agent.ToolCall{Name: "read"}) != nil {
		t.Fatal("calls within budget must pass")
	}
	if err := g(agent.ToolCall{Name: "read"}); err == nil || !strings.Contains(err.Error(), "budget") {
		t.Fatalf("over budget must refuse readably: %v", err)
	}
	if budgetGate(0, inner)(agent.ToolCall{}) != nil {
		t.Fatal("zero budget means unlimited")
	}
}

// TestLastResponseBlock: the resume banner shows the prior assistant reply, and
// a long one is tail-trimmed (the conclusion survives, the head is dropped).
// Breaker: drop the truncation and the head leaks; return the wrong turn and
// the assistant text is missing.
func TestLastResponseBlock(t *testing.T) {
	if lastResponseBlock(nil) != "" {
		t.Fatal("no history must yield no block")
	}
	short := []agent.Turn{{Role: "user", Text: "do it"}, {Role: "assistant", Text: "done: added the endpoint"}}
	if blk := lastResponseBlock(short); !strings.Contains(blk, "last response") || !strings.Contains(blk, "added the endpoint") {
		t.Fatalf("short reply must show in full: %q", blk)
	}
	long := "HEAD_MARKER " + strings.Repeat("x", 3000) + " TAIL_MARKER"
	blk := lastResponseBlock([]agent.Turn{{Role: "assistant", Text: long}})
	if !strings.Contains(blk, "TAIL_MARKER") {
		t.Fatalf("a long reply must keep its conclusion: %q", blk[:80])
	}
	if strings.Contains(blk, "HEAD_MARKER") {
		t.Fatal("a long reply must drop its head, not flood the banner")
	}
	if !strings.Contains(blk, "showing the end") {
		t.Fatal("truncation must be disclosed")
	}
}

// TestStatusTextNoProvider: with no live provider, the status line reports it
// rather than the resolved-default model. Breaker: drop the r.p==nil branch and
// statusText renders the resolved-default model as if it were active.
func TestStatusTextNoProvider(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	chtmp(t)
	r := &repl{sess: &Session{ID: "s1"}} // p is nil: no provider configured
	if got := r.statusText(); !strings.Contains(got, "no provider") {
		t.Fatalf("status with no provider must say so: %q", got)
	}
}
