# AGENTS.md

Core beliefs and working approach for sesh. This file is loaded as
context by sesh itself, so it speaks to two readers at once: a person
deciding how to extend the code, and an agent working inside this repo.

## What this is

A minimal coding agent: a loop that lets a model read, search, write, edit,
and run shell commands until a task is done. Standard library only, zero
dependencies, one binary, two wire protocols (Anthropic and OpenAI).

It exists to prove one claim: an agent harness is a small, knowable thing. The
irreducible core is a loop calling a model with tools and feeding the results
back. Everything else is a policy you can see, name, and swap.

## The one idea: a policy-free core

The hard-won lesson, taken from pi, is that the core stays small only if it
refuses to own policy. So the code splits along exactly that line:

- **`agent/`** is the core. The neutral conversation types and the loop. It
  never prints, never reads input, and never decides whether an action is
  allowed. It is the part you should almost never need to touch.
- **`provider/`** is the brain seam. Two adapters translate our neutral types
  to and from each wire format, streaming included. A session recorded against
  one provider resumes against the other, because neither protocol leaks past
  this package.
- **`harness/`** is the product: one package, deliberately. Package
  boundaries are API contracts, and freezing policy code behind contracts is
  the pin-yourself trap; one package keeps full refactor freedom while
  `internal/` stops outsiders importing policies (`agent` and `provider`
  remain a clean embeddable library). Files split by concern: `harness.go`
  (Main: flags, wiring, modes), `steering.go` (prompts, identity),
  `tools.go` (built-in tools, confinement), `proc.go` (the background-process
  supervisor: groups, reap, the cleaned-log read primitive), `search.go` (the shaped search),
  `editmatch.go` (the hardened edit), `diff.go` (the applied diff),
  `gates.go` (oversight policies), `render.go` (output hooks), `repl.go`
  (commands and their state), `drive.go` (goal persistence, the judge),
  `continuity.go` (handoff chains, recall), `subagent.go` (task), `tui.go`
  (the console seam), `brains.go` (provider selection), `install.go` and
  `scaffold.go` (the install story), the engines (`skills.go`, `mcp.go`,
  `engines.go`: progressive disclosure for instructions and external tool
  servers), plus sessions, providers, credentials,
  tuning, tool mods, doctor, statusline, history, help, and the editbench
  seams. The binary is `cmd/sesh`, three lines. When in doubt, new behavior belongs in
  `harness/`, not in the core.

If you are about to add a flag, a check, or an `if` to the loop in `agent/`,
stop. That is almost always a policy, and policies live at the edge.

## How the edge plugs into the core

The core exposes one small surface, and the product fills it in:

- **Tools are values.** A `Tool` is a schema plus a pure function. To add or
  replace one, append to `builtinTools()`. The core just runs what it is given.
  The `task` subagent is the proof: a child run of the same loop with a fresh
  history and a reduced toolset, packaged as a plain tool value (`subagent.go`
  in `sesh`).
  Children observe (read, search, gated bash) but never write or edit, so
  parallel contexts cannot make conflicting changes; nesting stops at depth 3
  by the tool's absence from the deepest toolset. A tool also declares whether
  it is parallel-safe (`Tool.Parallel`): which tools qualify is product policy
  (observation yes, mutation never); the core owns only the mechanism, running
  a batch concurrently when every call in it qualifies.
- **Oversight is an injected function.** The gate is a `func(ToolCall) error`
  the product passes in. Declining returns a model-readable error, not a
  crash. The default gate allows everything and consults the user's gate mod;
  `-ask` swaps in one that prompts per mutation. Any future policy is a few
  lines in `harness/` and zero in the core; the gate mod is the proof, a
  user-space executable ruling on every mutating call.
- **Rendering is hooks.** Text, reasoning, and tool I/O reach the terminal
  through callbacks. Interactive mode wires noisy hooks; `-p` print mode wires
  silent ones. A JSON mode would be a third set of hooks. Same loop underneath.

## Beliefs that shaped the code

- **Minimal beats featureful.** Every line is a line someone must understand.
  The whole sesh should stay readable in one sitting. Reach for the standard
  library before a dependency, and for deletion before addition.
- **Provider-agnostic by construction.** No provider type, name, or quirk may
  appear outside `provider/`. The rest of the code speaks only neutral types.
- **Errors are messages to the model, not stack traces.** A tool that fails
  returns text the model can act on ("old text not found; read the file and try
  again"), so the loop can recover instead of dying.
- **Every assumption is a dial, and dials live in files.** A harness
  component encodes an assumption about what the model cannot do on its own,
  and each model generation invalidates some of them. So the assumptions are
  not constants: the numeric ones live in `tuning.json` and the model-facing
  prompts (brief, judge, task, seed, continue) in `prompts/`, both on the
  usual mod chain. Improving with the next model means editing a file and
  re-running the rig, never a refactor or a breaking release. Code carries
  only mechanism; evidence sets the defaults; user space owns the overrides.
- **Steering lives in user space.** The system prompt is resolved from
  `.sesh/SYSTEM.md`, then `~/.sesh/SYSTEM.md`, then a built-in default;
  `APPEND_SYSTEM.md` (same chain) appends without replacing, and `AGENTS.md`
  or `CLAUDE.md` is appended as project context. An `<identity>` block tells
  the model what provider and model actually serve it (refreshed on every
  switch), because models cannot know this themselves. The footer status line
  follows the same chain: an executable `statusline` script gets session
  context as JSON on stdin and its first stdout line is displayed. Change
  behavior by editing a file, not by recompiling. We call these files mods;
  sesh scaffolds a short guide and an example for each into the live mount
  points `~/.sesh/` (global) and the project's `.sesh/` (always gitignored:
  doctor checks).
- **The conversation is the state.** History is the only state that matters.
  Sessions are that history saved as JSON, which is why they are portable,
  forkable, and compactable. There is no hidden state to lose.
- **Reset, never re-summarize.** A summary of a summary loses most of
  everything within two or three generations, so when a context window fills
  sesh hands off (`/handoff`, or automatically near the limit) instead
  of compacting in place: a fresh session in the same chain, seeded with an
  append-only ledger carried verbatim, a brief written by a fresh-context
  model call, and the latest turns verbatim (`continuity.go`). The boundary is
  silent because it is recoverable, not because the summary is trusted: prior
  links are archived in full and the `recall` tool searches the whole chain.
  The retention rig (`rig_test.go`, opt-in via `SESH_RIG=1`) measures what
  each boundary mechanism actually preserves; changes here must keep it green.
  Pressure management is not optional: when a window is known, handoff is the
  lifecycle. There is no off switch, because a session past its window is
  silently truncating on some servers while the user reads a warning; the
  tuning.json dials move the thresholds for anyone who wants them elsewhere.
- **Autonomy by default; the dial is explicit.** Interactive tools run
  freely, mutation included: the human watching the transcript is the
  oversight, and ctrl-c is the gate. Prompting before every write was the
  harness pre-judging the model, the same mistake as iteration caps. The dial
  still moves deliberately, never by accident: `-ask` restores per-call
  approval, the gate mod (an executable at `.sesh/gate`, then `~/.sesh/gate`)
  rules on every mutating call in code and fails closed, and print mode
  (`-p`) stays read-only without `-yes` because unattended runs have no one
  watching to interrupt. The observe/mutate line survives as the line where
  the gate mod and `-ask` apply.
- **Boundaries live in code, not prose.** The file tools refuse paths outside
  the working directory, with symlinks resolved so a link cannot smuggle an
  operation out of the tree (`-unsafe-paths` is the deliberate opt-out), and
  they always refuse `~/.sesh`. A constraint that exists only in the system
  prompt is a request, not a boundary. `bash` is the one documented exception;
  containerize when that matters.

## Providers

Brains are named profiles in `providers.json`, but a new user never edits it by
hand. Everything is driven from the prompt:

- `/provider add` walks a short wizard (name, protocol, url, key), discovers the
  endpoint's models, lets you pick a default, and switches to the new provider
- `/provider remove <name>` deletes the provider and its key together
- `/provider` lists providers and the current one; `/provider <name>` switches
- `/model` lists the endpoint's discovered models; `/model <id>` switches model

A brand-new run with nothing configured still starts (no usable provider is not
fatal in interactive mode); it just prints "run /provider add". The config is
resolved with the same file chain as the steering prompt: `~/.sesh/providers.json`
(global, where add/remove write), overlaid by `.sesh/providers.json` (project).
Flags (`-provider`, `-protocol`, `-url`, `-model`) override per field, and a
resumed session keeps its own brain.

Keys are never exported into the shell, and never stored in plaintext. They live
in `~/.sesh/credentials.json` as AES-256-GCM ciphertext under a random master
key in `~/.sesh/key` (both 0600). A key that leaks into a transcript or log is
ciphertext, useless off this machine. That protects against incidental leakage,
not against an agent that reads both files locally, so the built-in `read` and
`search` tools also refuse the `~/.sesh` directory (see `underSeshDir`).
`bash` remains a hole by nature; the encryption is the backstop if a key escapes.
Resolution order: a profile's inline `key`, then `credentials.json` by provider
name, then `key_env`, then the conventional env var.

Profiles do not list models; sesh discovers them. On start, on every
`/provider` switch, and inside `/provider add`, it calls the endpoint's `/models`
route (see `provider.ListModels`). Discovery is best-effort: an endpoint without
a models route (or a missing key) just leaves the list empty, and `/model <id>`
still works by name.

Switching is live and cheap because the types are neutral: the same conversation
continues against a different brain, which is the whole point of keeping
protocols sealed inside `provider/`.

## What we deliberately leave out

A plugin runtime (our answer is the hooks struct plus owning the source), a
full TUI, session-tree navigation, and a package manager. These are product
surface, not core. "Minimal" means trusting that line and saying no.

Bare-word subcommands too: the external surface is flags only (`-install`,
`-update`, `-doctor`), one stdlib parser, one self-generating help block.
Inside a session the grammar is slash commands; a third grammar for a few
maintenance verbs is not worth it, and bare arguments stay reserved for
better uses than spelling `-update` without the dash.

And undo/checkpoints: keep your project in git and commit before letting any
agent loose; sesh will not maintain a shadow history of your tree. What it
does instead is make every mutation visible the moment it happens: edit and
write results carry the diff they applied.

## Working in this repo

- Verify before claiming done: `go build ./...`, `go vet ./...`, and
  `go test ./...` must all pass. After changing a tool, exercise it with `bash`.
- **Tests earn their keep by failing.** Passing is not the bar; agents
  reliably write tests that pass. Before keeping any test, name the one-line
  code change that would make it fail. Cannot name one: it proves nothing,
  delete it. Assert behavior, never implementation echo (`parse(bad) returns
  a recoverable error`, not `name == "editor"`). Kill tautological,
  green-but-empty, and flaky tests on sight, including in review. The layers:
  unit tests prove the pieces first; the retention rig and the live benches
  then exercise sesh the way a user does; a change is done when both
  layers agree, never on unit green alone.
- Match the surrounding style. Prefer `edit` over `write` for existing files.
  Keep functions small and names precise.
- Never use em dashes in prose, comments, or commit messages. Use colons,
  commas, parentheses, or periods.
- Keep the core pure. If a change makes `agent/` import a provider, read input,
  or print, it is in the wrong package.
