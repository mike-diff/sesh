# ~/.sesh: sesh's home

Everything sesh remembers or lets you change lives here. This directory IS
the global mod mount point; a project's `.sesh/` overrides it per file.

    providers.json     provider profiles (/provider add writes this)
    credentials.json   API keys, AES-256-GCM encrypted; master key alongside
    SYSTEM.md          replace the built-in system prompt
    APPEND_SYSTEM.md   append to the system prompt instead of replacing it
    tuning.json        behavioral dials; state only what you change
                       (takes // comments; see tuning.json.example)
    theme.json         colors for rendered markdown output (see Theme below)
    prompts/           override the model-facing templates (see its README)
    tools/             executables that become agent tools (see its README)
    statusline         executable: owns the footer status line
    gate               executable: rules on every mutating tool call
    sessions/ chains/  transcripts and handoff ledgers (plain JSON/JSONL)
    run/               background-process logs, cleared when a session exits

The `.example` files are inert documentation: activate one by renaming it,
for example `mv gate.example gate && chmod +x gate`.

Scaffold files return if deleted but are never overwritten once present;
truncate one to empty to silence it for good.

sesh's own read/search tools refuse this directory (credentials live here);
bash can reach it, which is the documented trust boundary. Each mount here
has its own short README or .example; the project lives at
https://github.com/mike-diff/sesh

## providers.json per-profile dials

Beyond the wizard's basics, a profile accepts fields the adapters consume
(state only what you change):

    "context": 200000        // the model's context window; enables pressure
                             // tracking and automatic handoff near the limit
    "max_tokens": 16000      // output cap per reply. anthropic: replaces the
                             // max_output_tokens tuning default; a 400 naming
                             // the model's real cap self-heals to it. openai:
                             // sesh sends a cap only when this is set
    "thinking_budget": 10000 // anthropic extended thinking, per reply;
                             // thinking bills as output and max_tokens is
                             // raised to clear the budget. Default off
    "reasoning_effort": "high" // openai-protocol reasoning hint, passed
                             // through where the server supports one
    "vision": true           // force image support on/off (default: name
                             // heuristic; unknown models are text-only)

## Theme

`theme.json` recolors the markdown sesh renders as it streams the model's
replies. State only the roles you want to change; the rest keep their built-in
colors:

    {
      "heading": "#7aa2f7",
      "code":    "#9ece6a",
      "muted":   "#565f89",
      "accent":  "#bb9af7"
    }

The roles: `heading` (rendered bold as well), `code` (inline spans and fenced
blocks, both uniform), `muted` (blockquotes and horizontal rules), `accent`
(list bullets). Values are `#rrggbb`; 24-bit color degrades to the 256-color
palette on terminals that lack truecolor. A project `.sesh/theme.json` overrides
the global one role by role. Color is suppressed entirely when output is piped,
`NO_COLOR` is set to a non-empty value, or `TERM` is `dumb`. The theme is read
once at startup, so restart sesh to see a change.
