// sesh: a minimal coding agent with infinite, judged sessions. Stdlib only.
//
// This is the product layer (pi's coding-agent). It owns everything the core
// refuses to: the tools, the oversight gate, rendering, output modes, the
// steering chain, and sessions. The loop itself lives in package agent.
//
//	sesh                               # interactive; set up brains with /provider add
//	sesh -provider local               # start on a named provider
//	sesh -p "list the go files"        # print mode (one shot, read-only; add -yes to allow changes)
//	sesh -continue                     # resume the latest session
//	sesh -fork 20260101-120000         # branch from a session
//	sesh -protocol openai -url http://localhost:8080/v1 -model some-model   # ad hoc, no profile
package harness

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"

	"github.com/mike-diff/sesh/agent"
	"github.com/mike-diff/sesh/provider"
)

const bashTimeout = 60 * time.Second

// Chrome colors for harness-owned output (notes, diffs, tool I/O). Empty when
// color is off, so the same format strings print clean text down a pipe or
// under NO_COLOR. initColor sets them once startup knows the destination.
var (
	dim    string
	yellow string
	red    string
	green  string
	cyan   string
	reset  string
)

func initColor() {
	if useColor() {
		dim, yellow, red, green, reset = "\033[2m", "\033[33m", "\033[31m", "\033[32m", "\033[0m"
		cyan = "\033[36m"
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}

// Main is the whole product: flags, wiring, modes, and the interactive
// loop. cmd/sesh calls it and nothing else.
func Main() {
	protocol := flag.String("protocol", "anthropic", "wire protocol: anthropic or openai")
	url := flag.String("url", "", "base URL (default: the protocol's public API)")
	model := flag.String("model", "", "model id (overrides the provider's default)")
	providerName := flag.String("provider", "", "named provider profile from providers.json")
	resume := flag.String("resume", "", "resume a session by id")
	fork := flag.String("fork", "", "branch a new session from an existing one by id")
	cont := flag.Bool("continue", false, "resume the most recent session")
	list := flag.Bool("list", false, "list saved sessions and exit")
	autoYes := flag.Bool("yes", false, "allow mutation in print mode; interactively, silences -ask")
	ask := flag.Bool("ask", false, "prompt for approval before each write/edit/bash call")
	unsafePaths := flag.Bool("unsafe-paths", false, "allow file tools to touch paths outside the working directory")
	printMode := flag.String("p", "", "print mode: run one prompt, print the final reply, exit (read-only unless -yes)")
	maxTools := flag.Int("max-tools", 0, "cap tool calls per iteration, subagents included (0 = unlimited)")
	maxIters := flag.Int("max-iters", 25, "stop driving a request after this many iterations (1 = single-turn, no persistence)")
	doctor := flag.Bool("doctor", false, "check the configuration: providers, keys, endpoints, statusline")
	install := flag.Bool("install", false, "install this binary to ~/.local/bin and scaffold ~/.sesh")
	update := flag.Bool("update", false, "replace the installed binary with the latest release")
	showVersion := flag.Bool("version", false, "print the build commit and exit (source = built locally)")
	flag.Usage = func() { fmt.Fprint(os.Stderr, usageText()) }
	flag.Parse()
	if *showVersion {
		fmt.Println(commit)
		return
	}
	if *install {
		os.Exit(installCmd())
	}
	if *update {
		os.Exit(updateCmd())
	}
	initColor()         // resolve ANSI use before any colored output
	scaffoldHome()      // first run populates ~/.sesh; later runs fill gaps only
	tune = loadTuning() // dials resolve before anything reads them

	if *list {
		printSessions()
		return
	}
	if *doctor {
		os.Exit(runDoctor())
	}

	explicit := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { explicit[f.Name] = true })

	// Load or start a session.
	var sess *Session
	var err error
	switch {
	case *resume != "":
		if sess, err = loadSession(*resume); err != nil {
			fail(err)
		}
		// A sealed session was handed off; its archived transcript is what
		// descendants' recall reads, so it must never grow again. Resuming
		// one lands on the live end of its chain instead.
		if tip, hops := chainTip(sess); hops > 0 {
			fmt.Fprintf(os.Stderr, "%snote: session %s was sealed by a handoff; resuming the chain tip %s (%d hop(s) later; use -fork %s to branch the archived state)%s\n",
				yellow, sess.ID, tip.ID, hops, sess.ID, reset)
			sess = tip
		}
	case *fork != "":
		if sess, err = forkSession(*fork); err != nil {
			fail(err)
		}
	case *cont:
		cwd, _ := os.Getwd()
		if sess, err = latestSession(cwd); err != nil {
			fail(err)
		}
	default:
		cwd, _ := os.Getwd()
		sess = &Session{ID: newSessionID(), Cwd: cwd, Created: time.Now()}
	}

	// One live instance per session: claim it, or in -continue's case fall
	// back to a fresh session, so running several sesh instances in one directory
	// just works instead of silently clobbering a shared session file.
	if lerr := acquireLock(sess.ID); lerr != nil {
		if !*cont {
			fail(lerr)
		}
		fmt.Fprintf(os.Stderr, "%snote: %v; starting a fresh session instead%s\n", yellow, lerr, reset)
		cwd, _ := os.Getwd()
		sess = &Session{ID: newSessionID(), Cwd: cwd, Created: time.Now()}
		if lerr := acquireLock(sess.ID); lerr != nil {
			fail(lerr)
		}
	}

	// A session resumed outside its home directory still works (the tools
	// confine to the CURRENT directory, and the system prompt is rebuilt for
	// it), but the conversation's file references belong elsewhere; say so
	// instead of letting the mismatch pass silently. Stderr, so print-mode
	// stdout stays clean for pipes.
	if *resume != "" && sess.Cwd != "" {
		if cwd, _ := os.Getwd(); cwd != sess.Cwd {
			fmt.Fprintf(os.Stderr, "%snote: session %s was recorded in %s; its file references belong there%s\n",
				yellow, sess.ID, sess.Cwd, reset)
		}
	}

	// Layer the provider settings: profile, then session, then flags. A fresh
	// session adopts the brain you last used (this directory's latest session
	// first, then the latest anywhere) instead of resetting to the config
	// default; an explicit -provider, or resuming, overrides that.
	pcfg := loadProviders()
	creds := loadCredentials()
	brain := sess
	if len(sess.Turns) == 0 && !explicit["provider"] {
		if prior := lastBrain(sess.Cwd); prior != nil {
			brain = prior
		}
	}
	spec, err := resolveSpec(selection{
		provider: *providerName, protocol: *protocol, url: *url, model: *model,
		explicit: explicit,
	}, brain, pcfg, creds)
	if err != nil {
		fail(err) // a named-but-unknown -provider is fatal; a missing default is not
	}

	// Build the active provider. If it cannot be built yet (no providers
	// configured, or a missing key), interactive mode still starts so the user
	// can run /provider add. Print mode has nothing to do, so it fails below.
	var p agent.Provider
	var buildErr error
	if buildErr = resolveDefaults(spec.protocol, &spec.url, &spec.model); buildErr == nil {
		p, buildErr = buildProvider(spec.protocol, spec.url, spec.model, spec.key, spec.keyEnv)
	}
	if p != nil {
		sess.Provider = spec.name
		sess.Protocol, sess.URL, sess.Model = spec.protocol, spec.url, spec.model
	}

	sweepDeadProcs(sess.ID) // reap processes a previously-crashed sesh left behind
	pm := newProcManager(sess.ID)
	os.Setenv("SESH_SESSION", sess.ID) // tool/gate/statusline mods can find this session's run dir
	go gcBlobs()                       // sweep orphaned image blobs off the hot path; best-effort, never blocks startup
	tools := builtinTools(*unsafePaths, pm)
	// The engines (skill, mcp) join only when their user-space content exists:
	// an empty mount costs zero tokens. They are built-ins, so they claim
	// their names ahead of tool mods like every other built-in.
	engTools, engNotes, engSysNote := engineTools()
	tools = append(tools, engTools...)
	// Tool mods join the toolset before either mode wires task and recall in;
	// taken pre-claims every built-in name so a mod can never shadow one.
	taken := map[string]bool{"task": true, "recall": true}
	for _, t := range tools {
		taken[t.Def.Name] = true
	}
	modTools, modNotes := loadToolMods(taken)
	tools = append(tools, modTools...)
	// A tools-less model (e.g. a local vision model) rejects any tools array, so
	// the no_tools dial drops every tool: the built-ins and engines/mods here, and
	// the task/recall pair each mode wires in below.
	noTools := pcfg.Providers[spec.name].NoTools
	if noTools {
		tools = nil
	}
	for _, n := range append(engNotes, modNotes...) {
		fmt.Fprintf(os.Stderr, "%s%s%s\n", yellow, n, reset)
	}
	// Byte-stable per brain (keeps the prompt cache warm): the identity block
	// only changes when the provider or model does, which busts the cache anyway.
	// The engines note is byte-stable per session like the rest of the prompt.
	system := systemPrompt() + engSysNote + identityBlock(spec.name, spec.model, spec.protocol, false)
	history := sess.Turns

	// Print mode: one turn, final reply to stdout, exit. Silent hooks.
	// Read-only by default: no one is watching, so mutation needs explicit -yes.
	// A run tied to a session gets the same context management as interactive
	// (preflight, pressure handoff): scripted -p -continue loops are exactly
	// the sessions that otherwise grow forever. Management notices go to
	// stderr so piped stdout stays the reply alone.
	if *printMode != "" {
		if p == nil {
			fail(buildErr)
		}
		tied := *resume != "" || *fork != "" || *cont
		activeConsole = &plainConsole{out: os.Stderr}
		r := &repl{
			p: p, protocol: spec.protocol, url: spec.url, model: spec.model,
			key: spec.key, keyEnv: spec.keyEnv, current: spec.name,
			ctxLimit: capWindow(spec.ctxLimit),
			sess:     sess, history: history, system: system, con: activeConsole,
			procs: pm,
		}
		if len(history) > 0 {
			// Usage is unknown until the first call; estimate so preflight
			// can act on a resumed session already near its limit.
			r.ctxTokens = approxTokens(history) + len(system)/4
		}
		if r.ctxLimit == 0 { // same fallback as interactive: never fly blind
			r.ctxLimit, r.assumedCtx = tune.AssumedContext, true
		}
		var mutMu sync.Mutex
		mutations := 0
		raw := printGate(*autoYes)
		counted := func(c agent.ToolCall) error {
			err := raw(c)
			if err == nil && mutates(c) {
				mutMu.Lock()
				mutations++
				mutMu.Unlock()
			}
			return err
		}
		pg := budgetGate(*maxTools, counted) // first turn's budget; drive refreshes per iteration
		sessOf := func() *Session { return r.sess }
		if !noTools {
			tools = append(tools,
				taskTool(func() agent.Provider { return r.p }, sessOf, 1, *unsafePaths, pg, nil),
				recallTool(sessOf))
		}
		if r.preflight(*printMode) {
			os.Exit(1) // the message can never fit; nothing was sent
		}
		mark := len(r.history)
		r.history = append(r.history, agent.Turn{Role: "user", Text: *printMode})
		rehydrateImages(r.history) // a resumed session's prior images carry only a hash; load the bytes back before the wire call
		out, spent, err := agent.Run(context.Background(), r.p, r.system, r.history, tools,
			agent.Hooks{Gate: pg})
		if err != nil {
			if hint := keyHint(err, spec.name); hint != "" {
				fmt.Fprintf(os.Stderr, "%s\n", strings.TrimSpace(hint))
			}
			fail(err)
		}
		r.history = out
		if spent.LastInput > 0 {
			r.ctxTokens = spent.LastInput
		}
		// One-shot runs only persist when the user tied them to a session;
		// otherwise scripted -p calls would litter the session list.
		if tied {
			r.sess.Turns = out
			r.sess.save()
			r.managePressure() // a handoff here moves the lock with it
		}
		// Goal-driven persistence, same lifecycle as interactive: the request
		// is the goal, and a judged not-done keeps the run working. Progress
		// lines go to stderr; stdout stays the final reply alone.
		code := drive(r, driveConfig{
			request: *printMode, maxIters: *maxIters, maxTools: *maxTools, tools: tools,
			hooks:     agent.Hooks{Gate: counted},
			mutations: func() int { mutMu.Lock(); defer mutMu.Unlock(); return mutations },
			say:       func(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...) },
		}, out[mark:])
		if !tied {
			// drive iterations save as they go; untied one-shots must not
			// leave session litter behind.
			os.Remove(r.sess.path())
		}
		if final := lastText(r.history); final != "" {
			fmt.Println(final) // the run's final reply, not replayed history
		}
		pm.reapAll() // a print run that started a server must not leak it
		releaseLock(r.sess.ID)
		switch code {
		case driveStuck, driveMaxIters:
			os.Exit(code)
		}
		return
	}

	// Interactive: the retry notice renders through the product, not the core.
	provider.OnRetry = func(d time.Duration, err error) {
		emit("%s  retrying in %s (%v)%s\n", dim, d, err, reset)
	}

	// The console is the input twin of the rendering hooks: the footer TUI on
	// a real terminal, plain line input on a pipe. It must be restored on exit.
	con := newConsole()
	activeConsole = con // route all transcript output through the footer
	defer con.Close()

	r := &repl{
		p: p, protocol: spec.protocol, url: spec.url, model: spec.model,
		key: spec.key, keyEnv: spec.keyEnv, current: spec.name,
		ctxLimit: spec.ctxLimit, showThink: true,
		ask: *ask && !*autoYes, unsafePaths: *unsafePaths,
		pcfg: pcfg, creds: creds, sess: sess, history: history,
		system: system, con: con, procs: pm,
	}
	pm.onChange = r.refreshProcLine // keep the footer's process row live
	r.refreshSystem()               // attach the identity block to the live brain
	if len(history) > 0 {
		// Estimate the gauge for a resumed session so preflight and the
		// status line work before the first call reports real usage.
		r.ctxTokens = approxTokens(history) + len(r.system)/4
	}
	// The gate counts approved mutations so the drive's no-progress detector
	// can tell working iterations from spinning ones.
	var mutMu sync.Mutex
	mutations := 0
	rawGate := gate(con, *ask && !*autoYes)
	g := func(c agent.ToolCall) error {
		err := rawGate(c)
		if err == nil && mutates(c) {
			mutMu.Lock()
			mutations++
			mutMu.Unlock()
		}
		return err
	}
	mutCount := func() int { mutMu.Lock(); defer mutMu.Unlock(); return mutations }
	r.md = newMarkdown(func(s string) { emit("%s", s) })
	// OnUsage feeds the live tally so the status line climbs each round-trip,
	// not just when the turn ends; account commits the aggregate at turn end.
	hooks := renderHooks(g, &r.showThink, r.md, r.accountLive)
	// The task tool closes over the live repl: a /provider or /model switch
	// applies to later spawns, and child token usage lands in the totals
	// (accountChild shares acctMu because parallel children report concurrently).
	sessOf := func() *Session { return r.sess }
	if !noTools {
		tools = append(tools,
			taskTool(func() agent.Provider { return r.p }, sessOf, 1, *unsafePaths, g, r.accountChild),
			recallTool(sessOf))
	}
	// The TUI completes commands, provider names, and model ids on tab, and
	// reaps owned processes if a signal tears the session down.
	if t, ok := con.(*tuiConsole); ok {
		t.completer = r.completions
		t.atExit = func() { pm.reapAll() }
		t.mention = newMentions(r.md.pal.accent) // #skill / @file completion + highlight
		if tune.InputMaxRows > 0 {
			t.maxInputRows = tune.InputMaxRows
		}
	}
	// Pull the endpoint's model list once, for /model. Best-effort: an endpoint
	// without discovery just leaves the list empty; /model still switches by
	// name. A published context length fills in for a profile that set none
	// (an explicit "context" in providers.json stays the override).
	r.ctxLimit = capWindow(r.ctxLimit)
	r.models, r.modelCtx = discoverModels(p)
	if r.ctxLimit == 0 {
		if c := r.modelCtx[r.model]; c > 0 {
			r.ctxLimit = capWindow(c)
		}
	}
	if r.p != nil && r.ctxLimit == 0 { // assume rather than fly blind; banner keeps asking
		r.ctxLimit, r.assumedCtx = tune.AssumedContext, true
	}

	cwd, _ := os.Getwd()
	r.banner(cwd, *ask && !*autoYes, len(history), buildErr)
	r.showLastResponse() // resuming: show where the conversation left off
	if msg := updatedNotice(os.Getenv("SESH_UPDATED_FROM"), commit); msg != "" {
		emit("%s  %s%s\n\n", dim, msg, reset)
		os.Unsetenv("SESH_UPDATED_FROM") // shown once; do not leak to subprocesses
	}
	r.pushStatus()

	// Opt-in: ask the latest release whether a newer build exists and nudge,
	// off the critical path so a slow or absent network never delays the prompt.
	if tune.UpdateCheck && commit != "source" {
		go func() {
			if updateAvailable() {
				emit("%s  a newer build is available; run /update%s\n\n", dim, reset)
			}
		}()
	}

	// Escape cancels the running turn; Ctrl-C quits (twice within two seconds).
	// The second press is a force quit: restore the terminal first so raw mode
	// never leaks, then signal owned process groups best-effort with no grace
	// window, then release the lock. The OS reaps any group that has not died
	// yet, so a stubborn background process cannot delay the exit. The graceful
	// reapAll stays on the clean single-quit and normal-exit paths.
	intr := newInterrupts(func() {
		con.Close()
		if r.procs != nil {
			r.procs.killAllNow()
		}
		releaseLock(r.sess.ID)
	})
	// Extended-keys terminals deliver Ctrl-C as a keystroke, not an OS signal, so
	// the editor needs a console-level hook to reach the same quit semantics. It
	// is console-level (not per-turn) because Ctrl-C must work at the idle prompt
	// and during a turn alike; os.Exit stays here at the boundary, never inside
	// ctrlC, so the editor goroutine that calls this never holds its own mutex.
	if t, ok := con.(*tuiConsole); ok {
		t.onCtrlC = func() {
			if intr.ctrlC() {
				os.Exit(130)
			}
		}
	}

	say := func(f string, a ...any) {
		emit("%s  "+f+"%s\n", append(append([]any{dim}, a...), reset)...)
	}
	// Live input (type/queue/Escape-cancel while the agent works) needs the
	// footer TUI and a turn that never stops to ask: with -ask the gate reads
	// approvals mid-turn from the same keyboard, so that posture stays
	// synchronous.
	tc, isTUI := con.(*tuiConsole)
	live := isTUI && !(*ask && !*autoYes)

	var pending string // a queued message that becomes the next turn's input
	for {
		line := pending
		pending = ""
		if line == "" {
			l, err := con.ReadLine("-> ")
			if err != nil {
				emit("\n")
				r.goodbye()
				return
			}
			line = l
		}
		if line == "" {
			continue
		}
		if handled, quit := r.command(line); quit {
			r.goodbye()
			return
		} else if handled {
			continue
		}

		if r.p == nil {
			emit("%s  no active provider. run /provider add to set one up.%s\n\n", yellow, reset)
			continue
		}
		if r.preflight(line) {
			continue // the message can never fit; nothing was sent
		}
		cfg := driveConfig{
			request: line, maxIters: *maxIters, tools: tools, hooks: hooks,
			mutations: mutCount, turnCtx: intr.turnContext, drainQueued: intr.drain,
			say: say,
		}
		// Goal-driven persistence: the request is the goal; a judged not-done
		// keeps the session working until done, blocked, stuck, or the cap.
		// Conversation (no tool use) never drives.
		if live {
			// The turn runs in the background while the editor stays live: the
			// user can type a steering message (queued, injected at the next
			// boundary) or press Escape to cancel.
			ctx, done := intr.turnContext()
			stopSpin := r.spin()
			doneCh := make(chan struct{})
			go func() {
				defer close(doneCh)
				turns, ok := r.runTurn(ctx, line, tools, hooks)
				done()
				if ok {
					drive(r, cfg, turns)
				}
			}()
			tc.attendTurn(turnAttend{done: doneCh, cancel: intr.cancelCurrent, queue: intr.enqueue})
			<-doneCh // the worker is finished (attendTurn can also return on EOF); never overlap turns
			stopSpin()
			intr.resetAbort()                  // the turn is over; an Escape now is for the next prompt, not a stale carry-over
			if q := intr.drain(); len(q) > 0 { // typed after the last boundary: run next
				pending = strings.Join(q, "\n")
			}
		} else {
			ctx, done := intr.turnContext()
			stopSpin := r.spin()
			turns, ok := r.runTurn(ctx, line, tools, hooks)
			done()
			if ok {
				drive(r, cfg, turns)
			}
			stopSpin()
		}
	}
}

// interrupts owns turn control for the session: the cancel function for the
// turn in flight (the live editor's Escape calls it) and the queue of messages
// the user types while a turn runs (drained and injected as a steer at the next
// boundary). Ctrl-C is handled here too, but only to quit (a stray press warns,
// a second within the window quits), since cancelling is Escape's job now.
type interrupts struct {
	mu      sync.Mutex
	cancel  context.CancelFunc // non-nil while a turn is running
	aborted bool               // an Escape landed between iterations; the next context starts cancelled
	last    time.Time
	cleanup func()
	queued  []string // messages typed during a turn, drained at the next boundary
}

const doublePressWindow = 2 * time.Second

// newInterrupts wires Ctrl-C to quit (a stray press warns first, a second within
// the window quits and restores the terminal). Cancelling a turn is Escape's
// job, handled by the live editor, so Ctrl-C is left purely for quitting.
func newInterrupts(cleanup func()) *interrupts {
	in := &interrupts{cleanup: cleanup}
	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, os.Interrupt)
	go func() {
		// Some terminals deliver Ctrl-C as an OS signal; others (extended-keys
		// mode) deliver it as a keystroke that the editor routes to ctrlC. Both
		// triggers share this one body, and os.Exit lives at the call boundary so
		// ctrlC itself stays unit-testable around the spy-observable cleanup.
		for range sigc {
			if in.ctrlC() {
				os.Exit(130)
			}
		}
	}()
	return in
}

// ctrlC applies one Ctrl-C press: a stray press warns, a second within the
// window force-quits via cleanup. It reports whether this press force-quit so
// the caller can os.Exit(130) at the boundary, keeping os.Exit out of the unit
// under test. Both the signal goroutine and the live editor call this.
func (in *interrupts) ctrlC() bool {
	in.mu.Lock()
	double := time.Since(in.last) < doublePressWindow
	in.last = time.Now()
	in.mu.Unlock()
	if double {
		in.cleanup()
		return true
	}
	emit("\n%s  (ctrl-c again to quit; esc cancels the turn)%s\n", yellow, reset)
	return false
}

// cancelCurrent aborts the turn in flight (the live editor's Escape). It also
// records the abort even when no context is live: between iterations the worker
// detaches one context before the next is minted, and an Escape in that window
// would otherwise vanish. The sticky flag makes the next turnContext start
// already cancelled, so the abort reaches the iteration it was meant for.
func (in *interrupts) cancelCurrent() {
	in.mu.Lock()
	c := in.cancel
	in.aborted = true
	in.mu.Unlock()
	if c != nil {
		c()
	}
}

// enqueue stashes a message typed while a turn runs; drain returns and clears
// the queue at a safe boundary, where it can be injected as a user turn.
func (in *interrupts) enqueue(s string) {
	in.mu.Lock()
	in.queued = append(in.queued, s)
	in.mu.Unlock()
}

func (in *interrupts) drain() []string {
	in.mu.Lock()
	q := in.queued
	in.queued = nil
	in.mu.Unlock()
	return q
}

// turnContext hands out a cancellable context for one turn and the cleanup
// that detaches it from the watcher. An Escape that arrived between iterations
// (the sticky aborted flag) cancels the fresh context at once, so the abort is
// honored by the iteration it was aimed at instead of being dropped in the gap.
func (in *interrupts) turnContext() (context.Context, func()) {
	ctx, cancel := context.WithCancel(context.Background())
	in.mu.Lock()
	in.cancel = cancel
	wasAborted := in.aborted
	in.aborted = false
	in.mu.Unlock()
	if wasAborted {
		cancel()
	}
	return ctx, func() {
		in.mu.Lock()
		in.cancel = nil
		in.mu.Unlock()
		cancel()
	}
}

// resetAbort clears a stale abort once a turn has fully ended, so an Escape that
// landed as the drive was already stopping cannot cancel the next request the
// user types. Called at the top-level boundary, not inside a drive cycle.
func (in *interrupts) resetAbort() {
	in.mu.Lock()
	in.aborted = false
	in.mu.Unlock()
}
