// The interactive REPL: the slash-command dispatch and the state it mutates.
// Extracted from main so commands are testable without a terminal: main owns
// only the read loop and the chat turn; everything a command can touch lives
// here, on the repl struct.
package harness

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mike-diff/sesh/agent"
	"github.com/mike-diff/sesh/provider"
)

// repl is the live state of an interactive session: the active brain and its
// settings, the discovered models, the loaded config, and the conversation.
type repl struct {
	p        agent.Provider
	protocol string
	url      string
	model    string
	key      string
	keyEnv   string
	current  string // active profile name; "" when ad hoc flags built the provider
	models   []string
	modelCtx map[string]int // discovered context lengths by model id (often empty)
	pcfg     ProvidersConfig
	creds    map[string]string
	sess     *Session
	procs    *procManager // background processes this session owns
	history  []agent.Turn
	system   string
	con      console
	// rendering and context policy
	showThink   bool // render reasoning deltas (they are display-only either way)
	ctxLimit    int  // model context window in tokens; 0 = unknown
	assumedCtx  bool // ctxLimit is the fallback guess, not declared or discovered
	switched    bool // brain changed mid-conversation: noted in the identity block
	ask         bool // -ask is active: carried across a /update reload
	unsafePaths bool // -unsafe-paths is active: carried across a /update reload
	// running totals for the status line
	turns                   int
	totIn, totOut, totCache int
	ctxTokens               int // the last call's full prompt size = current context
	// spinBase is the status line the working-time spinner decorates. It is
	// refreshed whenever the totals change (including between drive iterations)
	// so a long worked turn shows the tokens climbing, not a frozen base.
	spinMu   sync.Mutex
	spinBase string
}

// account folds one model call's usage into the running totals and the
// current-context gauge, then refreshes the spinner base so the change is
// visible mid-turn. Shared by afterTurn and every drive iteration: a driven
// turn's later iterations are real spend and must reach the status line, not
// just the first sub-turn (the bug that made the count look frozen on long
// worked turns).
func (r *repl) account(spent agent.Usage) {
	r.totIn += spent.Input
	r.totOut += spent.Output
	r.totCache += spent.CacheRead
	if spent.LastInput > 0 {
		r.ctxTokens = spent.LastInput
	}
	r.setSpinBase()
}

// accountAux folds an auxiliary call's spend (the judge, mainly) into the
// totals without moving the context gauge: that call is real tokens, but its
// prompt is a side conversation, not the session's current context.
func (r *repl) accountAux(spent agent.Usage) {
	r.totIn += spent.Input
	r.totOut += spent.Output
	r.totCache += spent.CacheRead
	r.setSpinBase()
}

// setSpinBase recomputes the spinner's base status. Called on each accounting
// update rather than every spinner tick, so a user statusline script runs per
// iteration, not once a second.
func (r *repl) setSpinBase() {
	s := r.statusText()
	r.spinMu.Lock()
	r.spinBase = s
	r.spinMu.Unlock()
}

// statusText renders the current status line content; pushStatus hands it to
// the console. Split so the turn spinner can decorate a cached base instead of
// re-running a user statusline script every tick.
func (r *repl) statusText() string {
	cwd, _ := os.Getwd()
	return renderStatus(statusInfo{
		Provider: r.current, Model: r.model, Protocol: r.protocol,
		Session: r.sess.ID, Turns: r.turns,
		InputTokens: r.totIn, OutputTokens: r.totOut, CacheRead: r.totCache,
		ContextTokens: r.ctxTokens, ContextLimit: r.ctxLimit,
		Cwd: cwd,
	})
}

func (r *repl) pushStatus() {
	r.con.SetStatus(r.statusText())
	r.refreshProcLine()
}

// refreshProcLine repaints the footer's process row from the live registry.
// A no-op off a terminal (the plain console has no footer).
func (r *repl) refreshProcLine() {
	t, ok := r.con.(*tuiConsole)
	if !ok || r.procs == nil {
		return
	}
	t.SetProcLine(r.procs.manifestLine(t.width()))
}

// refreshSystem rebuilds the system prompt with the live identity block.
// Called at startup and on every provider or model switch; a switch already
// invalidates the provider's prompt cache, so the rebuild is cache-neutral.
func (r *repl) refreshSystem() {
	r.system = systemPrompt() + identityBlock(r.current, r.model, r.protocol, r.switched)
}

// goodbye releases the live-instance lock and prints how to pick the
// conversation back up. The resume hint only once there is a conversation: an
// empty session was never saved, so there is nothing to resume.
func (r *repl) goodbye() {
	if r.procs != nil {
		r.procs.reapAll() // owned processes never outlive their session
	}
	releaseLock(r.sess.ID)
	if len(r.history) == 0 {
		return
	}
	emit("%ssesh -resume %s%s\n", dim, r.sess.ID, reset)
}

// spin shows a working-time counter in the (always-visible) status line while
// a turn runs, plus the terminal title for tmux pane titles. The base status
// is cached so a user statusline script is not re-executed every second.
func (r *repl) spin() (stop func()) {
	r.setSpinBase()
	done := make(chan struct{})
	go func() {
		start := time.Now()
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case <-tick.C:
				secs := int(time.Since(start).Seconds())
				r.spinMu.Lock()
				base := r.spinBase
				r.spinMu.Unlock()
				r.con.SetStatus(fmt.Sprintf("%s · working %ds (ctrl-c cancels)", base, secs))
				r.con.SetTitle(fmt.Sprintf("sesh · working %ds", secs))
			}
		}
	}()
	return func() {
		close(done)
		r.con.SetTitle("sesh")
		r.pushStatus()
	}
}

// completions returns full-line completions for the TUI's tab key: command
// names, then provider names or discovered models for their subcommands.
func (r *repl) completions(line string) []string {
	var out []string
	switch {
	case strings.HasPrefix(line, "/provider "):
		arg := strings.TrimPrefix(line, "/provider ")
		for _, c := range append([]string{"add", "remove "}, r.pcfg.names()...) {
			if strings.HasPrefix(c, arg) {
				out = append(out, "/provider "+c)
			}
		}
	case strings.HasPrefix(line, "/model "):
		arg := strings.TrimPrefix(line, "/model ")
		for _, m := range r.models {
			if strings.HasPrefix(m, arg) {
				out = append(out, "/model "+m)
			}
		}
	case strings.HasPrefix(line, "/"):
		for _, c := range slashCommands {
			name := c.name
			if c.arg {
				name += " " // a space invites the argument
			}
			if strings.HasPrefix(name, line) {
				out = append(out, name)
			}
		}
	}
	return out
}

// slashCommand is one in-session command. The table below is the single source
// of truth: command() dispatches from it and completions() lists from it, so a
// new command is tab-completable the moment it is added here, no second list to
// keep in sync.
type slashCommand struct {
	name string // e.g. "/model"
	arg  bool   // accepts a "/name <arg>" form (and tab-completes with a space)
	quit bool   // ends the session
	run  func(r *repl, arg string)
}

var slashCommands = []slashCommand{
	{name: "/provider", arg: true, run: func(r *repl, a string) { r.providerCmd(a) }},
	{name: "/model", arg: true, run: func(r *repl, a string) { r.modelCmd(a) }},
	{name: "/reload", run: func(r *repl, _ string) { r.reloadCmd() }},
	{name: "/update", run: func(r *repl, _ string) { r.updateCmd() }},
	{name: "/context", arg: true, run: func(r *repl, a string) { r.contextCmd(a) }},
	{name: "/handoff", run: func(r *repl, _ string) { r.handoff() }},
	{name: "/chain", run: func(r *repl, _ string) { r.chainCmd() }},
	{name: "/compact", run: func(r *repl, _ string) { r.compactCmd() }},
	{name: "/settings", run: func(r *repl, _ string) { r.settingsCmd() }},
	{name: "/help", run: func(r *repl, _ string) { r.helpCmd() }},
	{name: "/exit", quit: true},
	{name: "/quit", quit: true},
}

// command dispatches one input line. handled=false means the line is a chat
// message for the model; quit=true means the session should end.
func (r *repl) command(line string) (handled, quit bool) {
	if line == "exit" || line == "quit" {
		return true, true // bare words, no slash
	}
	for _, c := range slashCommands {
		if line == c.name || (c.arg && strings.HasPrefix(line, c.name+" ")) {
			if c.quit {
				return true, true
			}
			c.run(r, strings.TrimSpace(strings.TrimPrefix(line, c.name)))
			return true, false
		}
	}
	return false, false
}

// contextCmd shows or sets the context window, the one number the whole
// continuity system hangs off. Setting it persists to the active profile and
// enables automatic handoff: a user who tells us the window wants it managed.
func (r *repl) contextCmd(arg string) {
	if arg == "" {
		switch {
		case r.ctxLimit == 0:
			emit("%s  context window unknown; /context <tokens> enables pressure tracking and automatic handoff%s\n\n", dim, reset)
		default:
			state := "automatic handoff at 80%"
			if r.assumedCtx {
				state += "; assumed, not confirmed: /context <tokens> to set exactly"
			}
			emit("%s  context window %s tokens (%s)%s\n\n", dim, kTokens(r.ctxLimit), state, reset)
		}
		return
	}
	n, err := strconv.Atoi(strings.TrimSpace(arg))
	if err != nil || n <= 0 {
		emit("%s  usage: /context <tokens>   e.g. /context 131072%s\n\n", red, reset)
		return
	}
	r.ctxLimit = capWindow(n)
	r.assumedCtx = false
	if r.current != "" { // persist on the active profile so it survives restarts
		g := loadGlobalProviders()
		if prof, ok := g.Providers[r.current]; ok {
			prof.Context = n
			g.Providers[r.current] = prof
			saveGlobalProviders(g)
			r.pcfg = loadProviders()
		}
	}
	r.pushStatus()
	note := ""
	if r.ctxLimit != n {
		note = fmt.Sprintf(" (capped to %s; larger buys rot, not memory)", kTokens(r.ctxLimit))
	}
	emit("%s  context window set to %s tokens%s; automatic handoff at 80%%%s\n\n", dim, kTokens(n), note, reset)
}

// settingsCmd opens the session-settings picker: arrows pick a setting, enter
// toggles it, and the menu reopens with the new value until cancelled. These
// are policies for THIS session; durable knobs live in files (providers.json,
// tuning.json), never behind a menu.
func (r *repl) settingsCmd() {
	onoff := func(b bool) string {
		if b {
			return "on"
		}
		return "off"
	}
	cur := 0
	for {
		items := []string{
			"show thinking: " + onoff(r.showThink),
		}
		idx, err := r.con.Select("settings (enter toggles, cancel closes)", items, cur)
		if err != nil || idx < 0 {
			emit("\n")
			return
		}
		cur = idx
		switch idx {
		case 0:
			r.showThink = !r.showThink
			state := "hidden"
			if r.showThink {
				state = "shown"
			}
			emit("%s  model reasoning is now %s (it never enters history either way)%s\n", dim, state, reset)
		}
	}
}

func (r *repl) helpCmd() {
	emit(`%scommands:
  /provider              pick a provider (arrows + enter)
  /provider add          wizard: add or re-key a provider
  /provider remove <n>   delete a provider and its stored key
  /provider <name>       switch provider, same conversation
  /model                 pick a model (arrows + enter)
  /model <id|#|substr>   switch model or add a custom one; sticks to the provider
  /reload                re-fetch the model list from the active provider
  /update                self-update, then reload into this same session
  /context [tokens]      show or set the model's context window; setting it
                         persists to the profile and enables automatic handoff
  /handoff               continue in a fresh session: brief + ledger + recent
                         turns carry over; recall searches the archived chain
  /chain                 show this conversation's handoff chain and its ledger
  /compact               summarize history in place (lossier than /handoff)
  /settings              session settings picker (show thinking)
  /help                  this help
  exit, /exit            quit (ctrl-d works too); prints how to resume
keys: ctrl-c cancels the running turn (twice quits) · shift+enter or ctrl-j
      inserts a newline · up/down history · tab completes · pastes over 3
      lines collapse to a [snippet] sent in full
config: ~/.sesh/ holds providers.json, credentials, SYSTEM.md, statusline
%s
`, dim, reset)
}

// banner prints the session header: active brain, autonomy posture, and the
// available commands.
func (r *repl) banner(cwd string, ask bool, resumed int, buildErr error) {
	if r.p == nil {
		emit("sesh · no provider configured · %s\nsession %s", cwd, r.sess.ID)
	} else {
		emit("sesh · %s via %s protocol · %s\nsession %s", r.model, r.protocol, cwd, r.sess.ID)
	}
	if resumed > 0 {
		emit(" (resumed, %d turns)", resumed)
	}
	emit("\n")
	switch {
	case ask:
		emit("-ask: write/edit/bash prompt for approval.\n")
	case findGateMod() != "":
		emit("tools run freely; gate mod %s rules on write/edit/bash. ctrl-c interrupts.\n", findGateMod())
	default:
		emit("tools run freely; ctrl-c interrupts (-ask to approve each mutation).\n")
	}
	if len(r.models) > 0 {
		emit("%s%d models on this endpoint; /model to list%s\n", dim, len(r.models), reset)
	}
	if r.p != nil && r.assumedCtx {
		emit("%scontext window unknown; assuming %s tokens (handoff at 80%%). run /context <tokens> to set it exactly%s\n",
			yellow, kTokens(r.ctxLimit), reset)
	}
	if r.p == nil {
		if len(r.pcfg.Providers) == 0 {
			emit("%sno providers yet. run /provider add to set one up.%s\n", yellow, reset)
		} else {
			emit("%sno active provider (%v). run /provider add, or /provider <name>.%s\n", yellow, buildErr, reset)
		}
	}
	emit("%scommands: /provider · /model · /compact · /settings · /help · exit — ctrl-c cancels a turn%s\n\n", dim, reset)
}

// applyProvider rebuilds the live provider from a profile, re-discovers
// models, and records the switch in the session. Shared by /provider <name>
// and /provider add.
func (r *repl) applyProvider(name string, prof Profile, key string) bool {
	proto, nurl, nmodel := prof.Protocol, prof.URL, prof.Model
	if err := resolveDefaults(proto, &nurl, &nmodel); err != nil {
		emit("%s  %v%s\n\n", red, err, reset)
		return false
	}
	np, err := buildProvider(proto, nurl, nmodel, key, prof.KeyEnv)
	if err != nil {
		emit("%s  %v%s\n\n", red, err, reset)
		return false
	}
	if len(r.history) > 0 {
		r.switched = true // the new brain inherits another model's turns
	}
	r.protocol, r.url, r.model = proto, nurl, nmodel
	r.key, r.keyEnv, r.current, r.p = key, prof.KeyEnv, name, np
	r.ctxLimit, r.assumedCtx = capWindow(prof.Context), false
	r.models, r.modelCtx = discoverModels(np)
	if r.ctxLimit == 0 {
		if c := r.modelCtx[nmodel]; c > 0 {
			r.ctxLimit = capWindow(c)
		}
	}
	if r.ctxLimit == 0 { // nothing declared, nothing discovered: assume, never fly blind
		r.ctxLimit, r.assumedCtx = tune.AssumedContext, true
	}
	r.refreshSystem()
	r.sess.Provider = name
	r.sess.Protocol, r.sess.URL, r.sess.Model = proto, nurl, nmodel
	r.sess.save()
	r.pushStatus()
	return true
}

// reloadCmd re-runs model discovery against the active provider, so the
// /model list picks up models added to or removed from the endpoint since
// startup. Discovery is otherwise refreshed only when the provider changes.
func (r *repl) reloadCmd() {
	if r.p == nil {
		emit("%s  no active provider; run /provider add%s\n\n", dim, reset)
		return
	}
	r.models, r.modelCtx = discoverModels(r.p)
	if len(r.models) == 0 {
		emit("%s  no models listed (this endpoint has no models route, or needs a key: /provider add to re-key)%s\n\n", dim, reset)
		return
	}
	emit("%s  reloaded %d models; /model to list%s\n\n", dim, len(r.models), reset)
}

// updateExecArgs builds the argv for the post-update re-exec: resume this exact
// session, and carry the safety flags forward so the reload cannot quietly
// change the posture. The brain, history, and cwd are restored from the
// resumed session itself, so they need not be re-passed.
func (r *repl) updateExecArgs(self string) []string {
	args := []string{self, "-resume", r.sess.ID}
	if r.ask {
		args = append(args, "-ask")
	}
	if r.unsafePaths {
		args = append(args, "-unsafe-paths")
	}
	return args
}

// updateCmd self-updates and, if the binary changed, re-execs the new one
// resumed on this session. The conversation is already on disk (autosaved), so
// the reload is seamless. A source build or a failed update leaves the session
// running untouched.
func (r *repl) updateCmd() {
	emit("%s  checking for an update...%s\n", dim, reset)
	changed, err := runUpdate()
	if err != nil {
		emit("%s  %v%s\n\n", red, err, reset)
		return
	}
	if !changed {
		emit("%s  already up to date%s\n\n", dim, reset)
		return
	}
	self, err := selfPath()
	if err != nil {
		emit("%s  updated, but cannot locate the binary to reload (%v); restart sesh to use it%s\n\n", red, err, reset)
		return
	}
	r.sess.save() // autosaved already; flush once more before handing off
	args := r.updateExecArgs(self)
	emit("%s  updated; reloading into this session...%s\n", dim, reset)
	releaseLock(r.sess.ID) // execve keeps our PID, so the successor must be free to re-lock
	r.con.Close()          // restore the terminal before the new image takes it
	// Hand the old commit to the new image so it can confirm what changed.
	env := append(os.Environ(), "SESH_UPDATED_FROM="+commit)
	if err := syscall.Exec(self, args, env); err != nil {
		// Exec returns only on failure; reclaim the lock so the session is not orphaned.
		acquireLock(r.sess.ID)
		fmt.Fprintf(os.Stderr, "reload failed (%v); the update is installed, restart sesh to use it\n", err)
	}
}

// updatedNotice is the one-line confirmation a post-/update reload prints on
// startup: the new build and the one it replaced. Empty when this start was not
// a self-update reload (no SESH_UPDATED_FROM) or nothing actually changed.
func updatedNotice(from, now string) string {
	if from == "" || from == now {
		return ""
	}
	return fmt.Sprintf("updated to %s (was %s)", now, from)
}

// providerCmd handles /provider and its subcommands: list, add, remove, switch.
func (r *repl) providerCmd(arg string) {
	switch {
	case arg == "":
		r.providerList()
	case arg == "add":
		r.providerAdd()
	case arg == "remove" || strings.HasPrefix(arg, "remove "):
		r.providerRemove(strings.TrimSpace(strings.TrimPrefix(arg, "remove")))
	default:
		r.providerSwitch(arg)
	}
}

// providerList opens the picker: arrow through the configured providers and
// selecting one switches to it.
func (r *repl) providerList() {
	names := r.pcfg.names()
	if len(names) == 0 {
		emit("%s  no providers yet. run /provider add%s\n\n", dim, reset)
		return
	}
	items := make([]string, len(names))
	for i, n := range names {
		marker := "  "
		if n == r.current {
			marker = "* "
		}
		items[i] = fmt.Sprintf("%s%-14s %s", marker, n, r.pcfg.Providers[n].Model)
	}
	idx, err := r.con.Select("provider (/provider add · /provider remove <name>)", items, slices.Index(names, r.current))
	if err != nil || idx < 0 {
		emit("%s  (no change)%s\n\n", dim, reset)
		return
	}
	if names[idx] == r.current {
		emit("%s  already on %s%s\n\n", dim, names[idx], reset)
		return
	}
	r.providerSwitch(names[idx])
}

// providerAdd is the setup wizard: name, endpoint, key, then a default model
// picked from live discovery. It persists the profile (global) and the
// encrypted key, then switches to the new provider.
func (r *repl) providerAdd() {
	name := ask(r.con, "  name", "")
	if name == "" {
		emit("%s  cancelled%s\n\n", dim, reset)
		return
	}
	proto := ask(r.con, "  protocol (openai/anthropic)", "openai")
	if proto != "openai" && proto != "anthropic" {
		emit("%s  protocol must be openai or anthropic%s\n\n", red, reset)
		return
	}
	nurl := ask(r.con, "  base url (blank for the protocol default)", "")
	secret, _ := r.con.ReadSecret("  api key (blank for none): ")

	// Discover models up front so the user can pick a default.
	durl, dmodel := nurl, "discovery"
	resolveDefaults(proto, &durl, &dmodel)
	var found []string
	var foundCtx map[string]int
	if tmpP, err := buildProvider(proto, durl, "discovery", secret, ""); err == nil {
		found, foundCtx = discoverModels(tmpP)
	}
	var nmodel string
	if len(found) > 0 {
		emit("%s  discovered %d models%s\n", dim, len(found), reset)
		nmodel = ask(r.con, "  default model", found[0])
	} else {
		nmodel = ask(r.con, "  default model", "")
	}
	if nmodel == "" {
		emit("%s  a default model is required; cancelled%s\n\n", dim, reset)
		return
	}

	// The context window drives pressure tracking and automatic handoff. Take
	// it from discovery when the endpoint publishes it; ask otherwise.
	nctx := foundCtx[nmodel]
	if nctx > 0 {
		emit("%s  context window %s (from the endpoint)%s\n", dim, kTokens(nctx), reset)
	} else if s := ask(r.con, "  context window in tokens (blank if unknown)", ""); s != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil && n > 0 {
			nctx = n
		} else {
			emit("%s  not a number; leaving the context window unset%s\n", dim, reset)
		}
	}
	if nctx > 0 {
		emit("%s  automatic handoff will engage at 80%% of %s tokens%s\n", dim, kTokens(nctx), reset)
	}

	// Persist the provider definition (global) and the encrypted key.
	g := loadGlobalProviders()
	if _, exists := g.Providers[name]; exists {
		emit("%s  updating existing provider %q%s\n", dim, name, reset)
	}
	g.Providers[name] = Profile{Protocol: proto, URL: nurl, Model: nmodel, Context: nctx}
	if g.Default == "" {
		g.Default = name
	}
	if err := saveGlobalProviders(g); err != nil {
		emit("%s  %v%s\n\n", red, err, reset)
		return
	}
	if secret != "" {
		if err := saveCredential(name, secret); err != nil {
			emit("%s  %v%s\n\n", red, err, reset)
			return
		}
		r.creds[name] = secret
	}
	r.pcfg = loadProviders()
	if r.applyProvider(name, Profile{Protocol: proto, URL: nurl, Model: nmodel, Context: nctx}, secret) {
		emit("%s  added %q and switched to it (%s via %s)%s\n\n", dim, name, r.model, r.protocol, reset)
	}
}

func (r *repl) providerRemove(name string) {
	if name == "" {
		emit("%s  usage: /provider remove <name>%s\n\n", red, reset)
		return
	}
	g := loadGlobalProviders()
	if _, ok := g.Providers[name]; !ok {
		emit("%s  no provider named %q%s\n\n", red, name, reset)
		return
	}
	delete(g.Providers, name)
	if g.Default == name { // hand the default to any survivor
		g.Default = ""
		for n := range g.Providers {
			g.Default = n
			break
		}
	}
	if err := saveGlobalProviders(g); err != nil {
		emit("%s  %v%s\n\n", red, err, reset)
		return
	}
	deleteCredential(name)
	delete(r.creds, name)
	r.pcfg = loadProviders()
	note := ""
	if name == r.current {
		note = " (was active; /provider <name> to switch)"
	}
	emit("%s  removed %q%s%s\n\n", dim, name, note, reset)
}

func (r *repl) providerSwitch(name string) {
	_, prof, err := r.pcfg.resolve(name)
	if err != nil {
		emit("%s  %v%s\n\n", red, err, reset)
		return
	}
	if prof.Protocol == "" {
		prof.Protocol = r.protocol
	}
	nkey := prof.Key // inline key wins; otherwise the saved credential
	if nkey == "" {
		nkey = r.creds[name]
	}
	if r.applyProvider(name, prof, nkey) {
		emit("%s  switched to %s (%s via %s protocol)%s\n", dim, name, r.model, r.protocol, reset)
		if len(r.models) == 0 {
			emit("%s  (no models listed; if this endpoint needs a key, run /provider add to re-key it)%s\n", dim, reset)
		}
		emit("\n")
	}
}

// resolveModelArg expands /model shorthand against the discovered list: a
// 1-based index, an exact id, or a unique substring. Ambiguous substrings are
// returned for the caller to report; anything else passes through untouched.
func resolveModelArg(arg string, models []string) (string, []string) {
	if n, err := strconv.Atoi(arg); err == nil && n >= 1 && n <= len(models) {
		return models[n-1], nil
	}
	if slices.Contains(models, arg) {
		return arg, nil
	}
	var matches []string
	for _, m := range models {
		if strings.Contains(m, arg) {
			matches = append(matches, m)
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	return arg, matches
}

// stickyModel records a model change on the active profile, so the choice
// survives provider hops and new sessions. Profiles that exist only in a
// project overlay are left alone; ad hoc sessions keep it in the session.
func stickyModel(name, model string) {
	if name == "" {
		return
	}
	g := loadGlobalProviders()
	prof, ok := g.Providers[name]
	if !ok || prof.Model == model {
		return
	}
	prof.Model = model
	g.Providers[name] = prof
	saveGlobalProviders(g)
}

// modelChoices is the model list the user picks from: what discovery found,
// plus this provider's one persisted custom model (the endpoint did not list
// it, but the user added it, so it stays selectable across restarts).
func (r *repl) modelChoices() []string {
	choices := r.models
	if r.current != "" {
		if cm := r.pcfg.Providers[r.current].CustomModel; cm != "" && !slices.Contains(choices, cm) {
			choices = append(append([]string{}, choices...), cm)
		}
	}
	return choices
}

func (r *repl) modelCmd(m string) {
	choices := r.modelChoices()
	if m == "" {
		const customEntry = "+ enter a custom model"
		idx, err := r.con.Select("model", append(slices.Clone(choices), customEntry), slices.Index(choices, r.model))
		if err != nil || idx < 0 {
			emit("%s  (no change)%s\n\n", dim, reset)
			return
		}
		if idx == len(choices) { // the custom-model entry
			name, rerr := r.con.ReadLine("custom model> ")
			if name = strings.TrimSpace(name); rerr != nil || name == "" {
				emit("%s  (no change)%s\n\n", dim, reset)
				return
			}
			r.setCustomModel(name)
			return
		}
		if choices[idx] == r.model {
			emit("%s  already on %s%s\n\n", dim, r.model, reset)
			return
		}
		m = choices[idx]
	}
	var ambiguous []string
	m, ambiguous = resolveModelArg(m, choices)
	if len(ambiguous) > 1 {
		emit("%s  %q matches several models: %s%s\n\n", red, m, strings.Join(ambiguous, ", "), reset)
		return
	}
	// When we know this endpoint's models, a name that is not among them (nor
	// the custom one) would 404 on the next turn. Catch it now, and if it is
	// another provider's default, the user clearly means that provider: hop.
	if len(choices) > 0 && !slices.Contains(choices, m) {
		var serves []string
		for name, prof := range r.pcfg.Providers {
			if prof.Model == m {
				serves = append(serves, name)
			}
		}
		sort.Strings(serves)
		switch {
		case len(serves) == 1:
			emit("%s  %s is not on this endpoint; switching to provider %s%s\n", dim, m, serves[0], reset)
			r.providerSwitch(serves[0])
		case len(serves) > 1:
			emit("%s  %s is not on this endpoint; it is the default of providers %s. use /provider <name>%s\n\n",
				red, m, strings.Join(serves, ", "), reset)
		default:
			emit("%s  %s is not on this endpoint (%s). /model lists what is, or pick \"+ enter a custom model\"; /provider <name> changes endpoints%s\n\n",
				red, m, r.current, reset)
		}
		return
	}
	r.switchModel(m)
}

// setCustomModel records a user-typed model as this provider's one custom model
// (persisted, so it survives restarts and stays in /model), then switches to it.
func (r *repl) setCustomModel(model string) {
	if r.current != "" {
		g := loadGlobalProviders()
		if prof, ok := g.Providers[r.current]; ok {
			prof.CustomModel = model
			g.Providers[r.current] = prof
			saveGlobalProviders(g)
			r.pcfg = loadProviders()
		}
	}
	r.switchModel(model)
}

// switchModel makes m the active model: rebuilds the brain, records the choice
// on the session and the provider (sticky), and retunes the window when the
// endpoint published one for m.
func (r *repl) switchModel(m string) {
	np, err := buildProvider(r.protocol, r.url, m, r.key, r.keyEnv)
	if err != nil {
		emit("%s  %v%s\n\n", red, err, reset)
		return
	}
	if len(r.history) > 0 {
		r.switched = true
	}
	r.model, r.p = m, np
	r.refreshSystem()
	r.sess.Model = m
	r.sess.save()
	stickyModel(r.current, m) // the provider remembers its last model
	r.pcfg = loadProviders()  // so a later /provider hop uses the new default
	note := ""
	// A model switch changes the window too, when the endpoint says so; a
	// stale limit from the previous model would mistime the handoff.
	if c := capWindow(r.modelCtx[m]); c > 0 && c != r.ctxLimit {
		r.ctxLimit = c
		note = fmt.Sprintf(" (context %s, from the endpoint)", kTokens(c))
	}
	r.pushStatus()
	emit("%s  switched model to %s%s%s\n\n", dim, m, note, reset)
}

// capWindow clamps a declared or discovered context length to what is
// actually useful (tune.MaxUsefulContext: past it, measured reasoning quality
// falls regardless of the advertised window). Zero (unknown) stays zero.
func capWindow(n int) int {
	if n > tune.MaxUsefulContext {
		return tune.MaxUsefulContext
	}
	return n
}

// preflight inspects an outgoing message against the window BEFORE it is
// sent, because the pressure check in afterTurn only runs after a turn
// completes. A message that can never fit is refused with guidance instead of
// a burned API call; one that would land deep in the reserve triggers the
// handoff first, so the fresh window absorbs it.
func (r *repl) preflight(line string) (refused bool) {
	if r.ctxLimit == 0 || r.p == nil {
		return false
	}
	est := len(line) / 4
	if est > r.ctxLimit*8/10 {
		emit("%s  this message is ~%s tokens against a %s-token window; it can never fit. Trim it, split it, or point the model at a file to read instead.%s\n\n",
			red, kTokens(est), kTokens(r.ctxLimit), reset)
		return true
	}
	if len(r.history) >= 4 && r.ctxTokens+est > r.ctxLimit*tune.HardPct/100 {
		emit("%s  this message would land at ~%d%% of the window; handing off first%s\n",
			yellow, (r.ctxTokens+est)*100/r.ctxLimit, reset)
		r.handoff()
	}
	return false
}

// handoff seals this session and continues the conversation in a fresh one:
// briefWriter returns the provider that writes handoff briefs and the model
// label to show when it is not the worker: the brief_provider/brief_model
// dials choose a cheaper brain (the cheap-brief experiment). Built fresh
// per handoff (construction is cheap and handoffs are rare); any resolution
// failure falls back to the worker with a visible note, because a handoff
// must never fail over a tuning preference.
func (r *repl) briefWriter() (agent.Provider, string) {
	if tune.BriefProvider == "" && tune.BriefModel == "" {
		return r.p, ""
	}
	proto, url, key, keyEnv := r.protocol, r.url, r.key, r.keyEnv
	model := tune.BriefModel
	if tune.BriefProvider != "" {
		pname, prof, err := r.pcfg.resolve(tune.BriefProvider)
		if err != nil {
			emit("%s  brief_provider %q not found; the worker writes its own brief%s\n", dim, tune.BriefProvider, reset)
			return r.p, ""
		}
		proto, url, key, keyEnv = prof.Protocol, prof.URL, prof.Key, prof.KeyEnv
		if key == "" {
			key = r.creds[pname]
		}
		if model == "" {
			model = prof.Model
		}
	}
	if err := resolveDefaults(proto, &url, &model); err != nil {
		emit("%s  brief writer unavailable (%v); the worker writes its own brief%s\n", dim, err, reset)
		return r.p, ""
	}
	bp, err := buildProvider(proto, url, model, key, keyEnv)
	if err != nil {
		emit("%s  brief writer unavailable (%v); the worker writes its own brief%s\n", dim, err, reset)
		return r.p, ""
	}
	return bp, model
}

// brief written by a fresh-context call, ledger carried forward, recent turns
// verbatim, everything older recoverable via recall. Returns false (with the
// history untouched) when any step fails, so the caller can fall back.
func (r *repl) handoff() bool {
	if r.p == nil {
		emit("%s  no active provider; nothing to hand off against%s\n", yellow, reset)
		return false
	}
	if len(r.history) < 4 {
		emit("%s  nothing to hand off yet%s\n", dim, reset)
		return false
	}
	bp, bmodel := r.briefWriter()
	if bmodel != "" {
		emit("%s  writing handoff brief (%s)...%s\n", dim, bmodel, reset)
	} else {
		emit("%s  writing handoff brief...%s\n", dim, reset)
	}
	brief, entry, used, err := writeBrief(context.Background(), bp, renderTranscript(r.history, 300))
	if err != nil {
		emit("%s  handoff brief failed: %v%s\n", red, err, reset)
		return false
	}
	r.totIn += used.Input
	r.totOut += used.Output
	r.totCache += used.CacheRead
	budget := 6000
	if r.ctxLimit > 0 {
		budget = r.ctxLimit / 8
	}
	mech := mechanicalFacts()
	if r.procs != nil {
		if note := r.procs.seedNote(); note != "" {
			mech += "\n" + note // the successor inherits running processes, not duplicates them
		}
	}
	next := seedChain(r.sess, brief, entry, mech, verbatimTail(r.history, budget))
	if err := next.save(); err != nil {
		emit("%s  could not save the new session: %v%s\n", red, err, reset)
		return false
	}
	// The chain record lands before the seal: the ledger file is the source
	// of truth a recovery can replay if the process dies mid-handoff.
	appendChainRecord(next.Root, chainRecord{
		Time: time.Now(), From: r.sess.ID, To: next.ID, Entry: entry,
		CtxTokens: r.ctxTokens, BriefIn: used.Input, BriefOut: used.Output,
		CachedIn: used.CacheRead,
	})
	// Seal the dying session: full transcript plus the forward pointer. A
	// sealed transcript is what descendants' recall reads, so it must never
	// grow again; -resume on it follows continued_by to the chain tip.
	r.sess.Turns = r.history
	r.sess.Child = next.ID
	r.sess.save()
	prior := r.sess.ID
	r.sess = next
	r.history = next.Turns
	r.ctxTokens = 0
	// Owned processes follow the live end of the chain: a dev server must not
	// die just because the context filled up.
	if r.procs != nil {
		r.procs.rekey(next.ID)
	}
	// The live-instance lock follows the live end of the chain.
	releaseLock(prior)
	acquireLock(next.ID)
	r.pushStatus()
	emit("%s  handed off: %s continues as %s (handoff %d; brief cost %d in / %d out tokens; /chain shows the ledger)%s\n",
		dim, prior, next.ID, next.Hops, used.Input, used.Output, reset)
	return true
}

// chainCmd shows this conversation's chain: every handoff, what each link
// accomplished, and what the boundary cost. The transparency that makes a
// silent mechanism trustworthy.
func (r *repl) chainCmd() {
	root := r.sess.Root
	if root == "" {
		root = r.sess.ID
	}
	recs := readChain(root)
	if len(recs) == 0 {
		emit("%s  no handoffs yet; this session is its own chain%s\n\n", dim, reset)
		return
	}
	emit("%schain %s · %d handoffs · ledger file %s%s\n", dim, root, len(recs), chainPath(root), reset)
	for i, rec := range recs {
		emit("%s  %3d. %s -> %s · at %s tokens · brief %d in/%d out%s\n", dim,
			i+1, rec.From, rec.To, kTokens(rec.CtxTokens), rec.BriefIn, rec.BriefOut, reset)
		emit("%s       %s%s\n", dim, compact(rec.Entry), reset)
	}
	emit("%s  live end: %s (this session)%s\n\n", dim, r.sess.ID, reset)
}

func (r *repl) compactCmd() {
	if r.p == nil {
		emit("%s  no active provider; nothing to compact against%s\n\n", yellow, reset)
		return
	}
	compacted := compactHistory(r.p, r.system, r.history)
	if len(compacted) != len(r.history) {
		// Preserve the full transcript as its own resumable session before the
		// live one is overwritten with the summary.
		backup := *r.sess
		backup.ID = r.sess.ID + "-precompact"
		backup.Title = "pre-compact transcript of " + r.sess.ID
		if err := backup.save(); err == nil {
			emit("%s  full transcript kept as session %s%s\n", dim, backup.ID, reset)
		}
	}
	r.history = compacted
	r.sess.Turns = r.history
	r.sess.save()
	emit("\n")
}

// ---------------------------------------------------------------------------
// REPL helpers: prompting, secret entry, model listing, compaction.
// ---------------------------------------------------------------------------

// setEcho toggles terminal echo via stty. Best-effort: if stty is missing or
// stdin is not a terminal (piped input), it does nothing.
func setEcho(on bool) {
	arg := "-echo"
	if on {
		arg = "echo"
	}
	cmd := exec.Command("stty", arg)
	cmd.Stdin = os.Stdin
	cmd.Run()
}

// ask prompts for a line of input, returning def when the user just hits enter.
func ask(c console, prompt, def string) string {
	p := prompt + ": "
	if def != "" {
		p = fmt.Sprintf("%s [%s]: ", prompt, def)
	}
	line, _ := c.ReadLine(p)
	if line != "" {
		return line
	}
	return def
}

// ---------------------------------------------------------------------------
// Turn lifecycle: run one chat exchange and clean up after it, normally or not.
// ---------------------------------------------------------------------------

// runTurn executes one chat turn and returns the turns it produced (nil when
// the turn failed). On any abnormal end (API error or a Ctrl-C cancellation)
// the unconsumed user turn is rolled back, so the session can never
// accumulate consecutive user messages, which the Anthropic protocol rejects
// on the next call.
func (r *repl) runTurn(ctx context.Context, line string, tools []agent.Tool, hooks agent.Hooks) ([]agent.Turn, bool) {
	mark := len(r.history)
	r.history = append(r.history, agent.Turn{Role: "user", Text: line})
	out, spent, err := agent.Run(ctx, r.p, r.system, r.history, tools, hooks)
	r.history = out
	emit("\n")
	if err != nil {
		r.history = r.history[:mark]
		if isCanceled(err) {
			emit("%s  turn cancelled; your message was not consumed%s\n", yellow, reset)
		} else {
			emit("%serror: %v%s\n", red, err, reset)
			if hint := keyHint(err, r.current); hint != "" {
				emit("%s%s%s\n", yellow, hint, reset)
			}
		}
		r.sess.Turns = r.history
		r.sess.save()
		emit("\n")
		return nil, false
	}
	// Capture the produced turns before afterTurn: a handoff (or compaction
	// fallback) there replaces r.history, and mark indexes the history that
	// produced this exchange, not whatever a boundary seeded after it.
	produced := r.history[mark:]
	r.afterTurn(spent)
	return produced, true
}

// afterTurn reports the turn's cost, updates the status line, watches context
// pressure, and saves the session.
func (r *repl) afterTurn(spent agent.Usage) {
	if spent.Input+spent.Output > 0 {
		emit("%s  tokens: in %d", dim, spent.Input)
		if spent.CacheRead > 0 {
			emit(" (+%d cached)", spent.CacheRead)
		}
		emit(" · out %d%s\n", spent.Output, reset)
	}
	r.turns++
	r.account(spent)
	r.pushStatus()

	r.managePressure()

	r.sess.Turns = r.history
	if err := r.sess.save(); err != nil {
		emit("%ssession save failed: %v%s\n", red, err, reset)
	}
	emit("\n")
}

// settled reports whether the conversation sits at a clean boundary: the last
// exchange produced no tool activity, so the model answered rather than being
// mid-investigation. Cutting mid-task is the classic auto-compaction failure;
// a text-only reply is the best cheap signal that a thread just closed.
func (r *repl) settled() bool {
	for i := len(r.history) - 1; i >= 0; i-- {
		switch r.history[i].Role {
		case "user":
			return true // no tool activity since the user last spoke
		case "tool":
			return false
		}
	}
	return true
}

// managePressure watches the context gauge after a turn. Soft zone (80-90%):
// hand off only at a settled boundary, deferring mid-investigation cuts. Hard
// zone (90%+): hand off regardless; the reserve exists to absorb exactly one
// more turn, not two. Shared by interactive afterTurn and tied print mode.
func (r *repl) managePressure() {
	if r.ctxLimit == 0 || r.ctxTokens < r.ctxLimit*tune.HandoffPct/100 {
		return
	}
	pct := r.ctxTokens * 100 / r.ctxLimit
	// Hand off only when there is enough history for a brief to help; a
	// near-full context on a tiny history is a prompt problem.
	if len(r.history) >= 4 {
		if pct < tune.HardPct && !r.settled() {
			emit("%s  context at %d%%; deferring handoff to a clean boundary (forced at %d%%)%s\n", dim, pct, tune.HardPct, reset)
			return
		}
		emit("%s  context at %d%% of %s tokens; handing off to a fresh session%s\n", yellow, pct, kTokens(r.ctxLimit), reset)
		if !r.handoff() {
			emit("%s  handoff failed; falling back to in-place compaction%s\n", yellow, reset)
			r.compactCmd()
		}
		r.ctxTokens = 0 // unknown until the next call reports it
		r.pushStatus()
	} else {
		emit("%s  context at %d%% of %s tokens; consider /handoff (or /compact)%s\n", yellow, pct, kTokens(r.ctxLimit), reset)
	}
}

// isCanceled reports whether the turn ended because its context was cancelled
// (Ctrl-C), however the HTTP stack wrapped it.
func isCanceled(err error) bool {
	return errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "context canceled")
}

// keyHint turns an authentication failure into the next step.
func keyHint(err error, providerName string) string {
	var apiErr *provider.APIError
	if !errors.As(err, &apiErr) || (apiErr.Status != 401 && apiErr.Status != 403) {
		return ""
	}
	name := providerName
	if name == "" {
		name = "<name>"
	}
	return fmt.Sprintf("  (authentication failed; re-key with: /provider add %s)", name)
}

// compactHistory asks the model to summarize the conversation so far and
// returns a fresh two-turn history holding that summary; on failure it returns
// the history unchanged. Preserving the full transcript is the caller's job
// (compactCmd snapshots it as a separate session).
func compactHistory(p agent.Provider, system string, history []agent.Turn) []agent.Turn {
	if len(history) < 4 {
		emit("%s  nothing to compact yet%s\n", dim, reset)
		return history
	}
	emit("%s  compacting...%s\n", dim, reset)
	ask := append(history, agent.Turn{Role: "user",
		Text: "Summarize everything above: the task, key decisions, files changed, and what remains. Be thorough but concise. This summary will replace the conversation history, so preserve anything you would need to continue."})
	out, _, err := agent.Run(context.Background(), p, system, ask, nil, agent.Hooks{})
	// The summary must come from the run's final turn specifically: scanning
	// the whole history would fall back to an older assistant message when the
	// model returns nothing, silently "compacting" to a stale reply.
	var summary string
	if len(out) > 0 && out[len(out)-1].Role == "assistant" {
		summary = strings.TrimSpace(out[len(out)-1].Text)
	}
	if err != nil || summary == "" {
		emit("%s  compact failed; history unchanged%s\n", dim, reset)
		return history
	}
	emit("%s  compacted %d turns into a summary%s\n", dim, len(history), reset)
	return []agent.Turn{
		{Role: "user", Text: "Summary of the conversation so far:"},
		{Role: "assistant", Text: summary},
	}
}

func lastText(turns []agent.Turn) string {
	for i := len(turns) - 1; i >= 0; i-- {
		if turns[i].Role == "assistant" && turns[i].Text != "" {
			return turns[i].Text
		}
	}
	return ""
}
