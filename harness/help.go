package harness

import (
	"flag"
	"fmt"
	"strings"
)

// usageText is the complete feature reference, written for both readers a
// CLI has: a person, and an agent driving the harness through a shell that
// reads -help to discover what it can do. Every mode, file, command, tool,
// and exit code is here; nothing requires reading the source to find.
func usageText() string {
	return `sesh: a minimal coding agent with infinite, judged sessions. Any Anthropic- or OpenAI-protocol model,
zero dependencies, one binary.

MODES
  sesh                      interactive REPL (footer TUI on a terminal, plain on pipes)
  sesh -p "request"         print mode: work to completion, final reply on stdout,
                               progress on stderr. Read-only unless -yes.
  sesh -doctor              check providers, keys, endpoints, context truncation,
                               statusline, sessions; exit nonzero on failure
  sesh -list                list saved sessions ([sealed -> id] marks handed-off links)
  sesh -install             copy this binary to ~/.local/bin and scaffold ~/.sesh
  sesh -update              replace the installed binary with the latest build
  sesh -version             print the build commit (source = built locally)

GOAL-DRIVEN PERSISTENCE (default in every mode)
  The goal is the request; nothing arms it. After any turn that used tools, a
  fresh-context judge rules from transcript evidence: done (stop), blocked
  (return to the user), or continue (the reason feeds the next iteration and
  work resumes). Plain conversation is never judged and never loops.
  Stop layers: -max-iters, a no-progress detector, -max-tools, Esc (cancels a
  turn; type while it works to steer at the next step). Ctrl-C quits.

CONTINUITY (infinite sessions)
  Context pressure is managed by handoff, never lossy in-place compaction: at
  80% of the window (at a clean boundary; forced at 90%) the session seals and
  a chained successor continues it, seeded with a fresh-context brief, the
  chain ledger, repo state from git, and the latest turns verbatim. Archived
  links stay searchable via the recall tool; /chain shows the whole chain.
  -continue resumes the newest unsealed session for this directory; -resume on
  a sealed session lands on its chain tip. One live instance per session
  (pid locks); concurrent -continue falls back to a fresh session.

FLAGS
` + flagDefaults() + `
INPUT KEYS (interactive footer TUI)
  Ctrl-V (Alt-V fallback)      paste a clipboard image, shown inline as [image-N]
                               and sent to a vision-capable model (Alt-V for
                               terminals that swallow Ctrl-V, like Windows Terminal)
  Shift-Enter, Ctrl-J, \+Enter newline; Enter submits
  Esc                          cancel the running turn (type to steer at the next step)

SESSION COMMANDS (interactive; tab completes)
  /provider [add|remove|name]  pick, add (wizard), remove, or switch providers
  /model [id|#|substring]      pick/switch models, or add a custom one; window retunes
  /reload                      re-fetch the model list from the active provider
  /update                      self-update, then reload into this same session
  /context [tokens]            show or set the context window (persists, enables handoff)
  /handoff                     hand off to a fresh chained session now
  /chain                       show this conversation's handoff chain and ledger
  /compact                     summarize in place (lossier than /handoff)
  /settings                    session settings picker: show thinking
  /copy                        copy the last response to the clipboard (clean source)
  /help                        command and key reference
  exit, /exit, ctrl-d          quit (prints sesh -resume <id>)

MODEL TOOLS (what the agent inside can do)
  read, search                 observe the working directory (plus archived
                               sessions and chain ledgers under ~/.sesh)
  loc                          count source lines by extension (read-only)
  write, edit, bash            mutate; run freely by default (-ask prompts,
                               the gate mod rules in code, -p needs -yes)
  task                         spawn a read-only subagent with a fresh context
                               window (depth-capped); parallel when batched
  recall                       search the archived transcripts of this
                               conversation's whole session chain
  proc                         start/list/logs/stop long-lived background
                               processes (dev servers); a bash command that
                               does not return is auto-promoted here. Reuses a
                               command already running, reports a port's holder
                               without killing it, survives handoffs, and is
                               reaped when the session exits.

FILES AND MODS (project .sesh/ overrides global ~/.sesh/)
  providers.json               named provider profiles (managed by /provider);
                               "vision": true|false overrides image support (default
                               by name); "no_tools": true sends no tools (tools-less
                               models, e.g. local vision models)
  credentials.json, key        AES-256-GCM encrypted API keys (0600)
  SYSTEM.md / APPEND_SYSTEM.md replace / extend the system prompt
  prompts/<name>.md            override model-facing templates: brief, judge,
                               task, seed, continue (placeholders in prompts/README.md)
  tuning.json                  the behavioral dials: handoff_pct, hard_pct,
                               max_useful_context, assumed_context,
                               seed_ledger_entries, task_depth, stuck_after,
                               recall_links, diff_lines, proc_promote_secs,
                               max_procs, proc_log_tail, update_check,
                               input_max_rows, brief_provider, brief_model
                               (state only what you change)
  tools/<name>                 executables that become agent tools (global
                               mount only): --schema describes, args JSON on
                               stdin, stdout is the result; mutating ones
                               follow the gate policy like write/edit/bash
  gate                         executable; rules on every mutating call: JSON
                               on stdin, exit nonzero denies (first stdout
                               line is the reason); broken mod fails closed
  statusline                   executable; JSON on stdin, first line shown
  sessions/, chains/           transcripts and chain ledgers (plain JSON/JSONL)
  run/                         background-process logs and crash records,
                               cleared when a session exits

EXIT CODES (print mode)
  0 done or blocked-on-user · 1 error · 3 stuck (no progress) · 4 max iterations

EXAMPLES
  sesh -provider local
  sesh -p "add a /health endpoint with a test and verify it" -yes
  sesh -continue -p "now do the same for /version" -yes
  sesh -resume 20260611-110555-3bd6e39c
`
}

// flagDefaults renders the flag block indented to match the rest of usage.
func flagDefaults() string {
	var b strings.Builder
	flag.VisitAll(func(f *flag.Flag) {
		def := ""
		if f.DefValue != "" && f.DefValue != "false" {
			def = " (default " + f.DefValue + ")"
		}
		fmt.Fprintf(&b, "  -%-14s %s%s\n", f.Name, f.Usage, def)
	})
	return b.String()
}
