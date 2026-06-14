# sesh (aka session)

A small and minimal coding agent written in Go with zero dependencies. Point it at any AI protocol (Claude, GPT, Ollama, vLLM, OpenRouter and friends) and it works through a small set of tools until your coding task is done.

sesh was created with a few things in mind:
- An agent harness is a small, knowable thing
- The irreducible core is a loop that calls a model with tools and feeds the results back
- Everything else is a policy you can see, name and swap.

## Core beliefs

- **A policy-free core.** The loop lives in `agent/` and owns no policy: it never prints, never reads input, never decides whether an action is allowed. Everything opinionated lives at the edge, where you can change it, and should, to make it your own.
- **Minimal beats featureful.** Every line is a line someone has to understand, so we reach for the standard library before a dependency and for deletion before addition.
- **Stop when you're done, not when the model context bloats.** A fresh-context judge reads your request and the transcript and decides whether the task is actually finished (see [the drive loop](#the-drive-loop)).
- **Autonomy by default, with explicit dials.** Tools run without per-call prompts; you watch the transcript and Ctrl-C is the brake. The safety dials are real and live in code, not in the prompt (see [Autonomy by default](#autonomy-by-default)).

The reasoning behind each belief is in [AGENTS.md](AGENTS.md). sesh automatically appends a project's `AGENTS.md` or `CLAUDE.md` (from the directory you launch it in) to the system prompt, so this file is also how sesh steers itself in its own repo.

## Install

macOS and Linux (amd64 and arm64):

```sh
curl -fsSL https://raw.githubusercontent.com/mike-diff/sesh/main/install.sh | sh
```

Or with Go 1.22+:

```sh
go install github.com/mike-diff/sesh/cmd/sesh@latest
```

Or from a branch:

```sh
go build -o bin/sesh ./cmd/sesh && ./bin/sesh -install
```

`-install` copies the binary to `~/.local/bin` and scaffolds `~/.sesh` with documented mount points and inert examples (first run scaffolds too, so a plain `go build` and `./bin/sesh` also just works). sesh is a single rolling codebase with no version numbers: `sesh -update` swaps in the latest published build when its checksum differs, and `sesh -version` prints the commit a binary was built from.

## Quickstart

```sh
go build -o bin/sesh ./cmd/sesh
./bin/sesh
```

On first run no provider is configured. Set one up from inside the session; the wizard discovers the endpoint's models for you:

```
-> /provider add
  name: local
  protocol (openai/anthropic) [openai]:
  base url (blank for the protocol default): http://localhost:8080/v1
  api key (blank for none):
  discovered 12 models
  default model [your-model]:
  added "local" and switched to it
```

Then just talk to it:

```
-> Hey sesh, tell me about yourself, how you work, what you can do and what are mods.
```

Submit `/help` for the key reference. When stdin or stdout is a pipe it falls back to plain line input. Type `exit` to quit; sessions autosave to `~/.sesh/sessions/`.

## The tools

sesh gives the model eight built-in tools, plus two more that appear only when you add content for them:

- **Observe** (read-only, run in parallel): `read`, `search`, `loc`, `recall`
- **Change** (gated, run one at a time): `write`, `edit`, `bash`
- **Delegate**: `task` spawns a read-only subagent with its own fresh context window, so a noisy investigation never clutters the main conversation
- **On demand** (zero tokens until you add content): `skill` and `mcp`, the two engines described under [Extending with mods](#extending-with-mods)

Tools are plain values: a schema plus a function. Adding or replacing one is a few lines in the product layer and zero in the core (see [Architecture](#architecture)).

## Infinite sessions

Long sessions degrade. sesh never re-summarizes a summary. When the context window nears its limit, or on `/handoff`, the session seals and a fresh one continues it in the same chain, seeded with:

- the **chain ledger**: one short entry per handoff, carried forward verbatim
- a **handoff brief** written by a fresh-context model call reading the sealed transcript: task, decisions, dead ends, files, environment, next step
- **repo state** from git, not summarized by the model
- the **most recent exchanges, verbatim**

The old session stays on disk in full and the `recall` tool searches the whole chain, so anything the brief dropped is one tool call away: the boundary is recoverable, not just summarized. `-continue` picks up the newest link. A known context window always hands off near its limit; the thresholds are dials in `tuning.json`.

## The drive loop

sesh runs the agent in a loop and keeps it going until the work is done, not just until the model stops. There is nothing to arm: the goal is your request.

What decides when to break the loop is a fresh-context judge. After any turn that used tools, it reads your request and the transcript and rules on evidence alone: `done` (builds run, tests pass), `blocked` (it genuinely needs your decision, so the prompt comes back to you), or not done (its reason feeds the next iteration and the loop continues). Plain conversation is never judged and never loops: chat stays chat. Hard limits bound the loop (`-max-iters`, a no-progress detector, `-max-tools`, and Ctrl-C, which pauses so your next message can steer). Print mode follows the same lifecycle: `sesh -p "implement X and verify it" -yes` runs to judged completion.

```
                 your request
                      |
                      v
       +-->  +-----------------------+
       |     |  agent turn:          |
       |     |  model calls tools,   |---- no tools ---->  reply, done
       |     |  results fed back     |                     (never judged)
       |     +-----------+-----------+
       |                 | used tools
       |                 v
       |     +-----------------------+
       |     |   fresh-context judge |
       |     +---+---------+-----+---+
       |         |         |     |
       |      not done    done  blocked
       |         |         |     |
       +---------+         v     v
      (reason feeds     stop &  prompt
       next iteration)  reply   back to you

  bounds: -max-iters · no-progress (stuck) · -max-tools · Ctrl-C
```

## Autonomy by default

sesh does not ask permission per tool call. It assumes you trust the model you chose with the project you pointed it at: every call renders live, every edit and overwrite shows its diff, and Ctrl-C interrupts. Oversight is interruption-during, not approval-before. There is no undo, so keep your project in git and commit before letting any agent run wild.

What holds in code regardless:

- File tools can only read or change files inside the directory you launched sesh in, and they always refuse the `~/.sesh` key store. A symlink inside the project cannot trick them into reaching outside it. Pass `-unsafe-paths` to lift the directory limit when you genuinely need to.
- `bash` is the documented exception: it can do anything your shell can. Stored API keys are encrypted at rest (AES-256-GCM), which helps protect keys leaked off the machine, not from your own shell. Point sesh at models you trust and containerize when that is not enough.

To tighten the dial: `-ask` restores a per-call approval prompt and a **gate mod** (an executable at `.sesh/gate` or `~/.sesh/gate`) rules on every mutating call in code and fails closed. Print mode (`-p`) is read-only unless you pass `-yes`.

If you run sesh unattended, setting up a gate mod first is HIGHLY recommended: it puts your safety policy in code in front of every mutating call (sesh scaffolds a working `gate.example` and a short guide into `~/.sesh`).

**No telemetry.** sesh never phones home. The only outbound network is the provider you configure, any MCP servers you add, and `sesh -update` when you run it (which verifies a SHA-256 checksum). There is no usage tracking and no automatic update check.

## Extending with mods

Every customization is a file you drop into a mount point. No recompile, no plugin runtime, no registration: sesh looks for well-known filenames and uses what it finds. Global mods live in `~/.sesh/`; a project's `.sesh/` (gitignore it) overlays them for that project only.

| Mod | What it does |
|---|---|
| `SYSTEM.md` / `APPEND_SYSTEM.md` | replace or extend the system prompt |
| `AGENTS.md` / `CLAUDE.md` | project context, auto-appended to the system prompt (read from the launch directory) |
| `statusline` | an executable that owns the footer status line |
| `gate` | an executable that rules on every mutating call (fails closed) |
| `providers.json` | named provider profiles (managed by `/provider`) |
| `tuning.json` | the behavioral dials (handoff thresholds, caps, and more) |
| `prompts/` | override the model-facing templates (brief, judge, task, ...) |
| `tools/` | executables that join the model's toolset (global only) |
| `skills/` | Agent Skills the model loads on demand |
| `mcp.json` | external MCP tool servers (global only) |

The last two are considered **engines** and they bring progressive disclosure: a manifest the model always sees, payloads loaded only when a task calls for them and zero tokens until you add content.

- **Skills.** Drop an [Agent Skill](https://agentskills.io) folder (`SKILL.md` plus optional `scripts/`, `references/`, `assets/`) into `~/.sesh/skills/<name>/`. The `skill` tool then carries one manifest line per skill and loads a body only when the task matches.
- **MCP servers.** Define servers in `~/.sesh/mcp.json`, global mount only since a server is code running with your permissions (the standard `mcpServers` shape: stdio via `command`, remote via `url`). The `mcp` tool lists every `server:tool` from a cached manifest; servers spawn lazily and every call is gated, so a gate mod can write per-server policy. Toggle a server with `"disabled": true`, or pick the active set per project with a `.sesh/mcp.json` like `{"servers": ["github"]}`.

On first run sesh scaffolds `~/.sesh` with a short guide and an example for each mod type.

## Architecture

```
agent/      the core: neutral types + the loop. Never prints, never reads
            input, never decides whether an action is allowed.
provider/   the wire seam: Anthropic Messages and OpenAI chat-completions
            adapters, hand-rolled SSE, retries, model discovery.
harness/    the product: one package (full refactor freedom), where every
            policy lives. Prompts, tools, search, edit, oversight, rendering,
            the drive loop, handoff chains, the engines.
cmd/sesh/   the binary: three lines.
```

The core exposes one extension surface (a `Hooks` struct plus tools-as-values) and refuses to own policy. Rendering, oversight and output modes are all injected by the product, which is why one loop serves both the interactive REPL and print mode. The full per-file map and the boundary rules are in [AGENTS.md](AGENTS.md).

## Reference

- **Commands and flags:** `sesh -help`, the complete self-generating list.
- **Config and every mod type:** the guides sesh scaffolds into `~/.sesh` (and `sesh -doctor`).
- **Design beliefs and architecture:** [AGENTS.md](AGENTS.md).

## Development

```sh
gofmt -l . && go vet ./... && go test ./...
```

CI enforces all three. Keep the core pure: if a change makes `agent/` import a provider, read input, or print, it is in the wrong package.

## License

[MIT](LICENSE).
