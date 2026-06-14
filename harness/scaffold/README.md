# ~/.sesh: sesh's home

Everything sesh remembers or lets you change lives here. This directory IS
the global mod mount point; a project's `.sesh/` overrides it per file.

    providers.json     provider profiles (/provider add writes this)
    credentials.json   API keys, AES-256-GCM encrypted; master key alongside
    SYSTEM.md          replace the built-in system prompt
    APPEND_SYSTEM.md   append to the system prompt instead of replacing it
    tuning.json        behavioral dials; state only what you change
    prompts/           override the model-facing templates (see its README)
    tools/             executables that become agent tools (see its README)
    statusline         executable: owns the footer status line
    gate               executable: rules on every mutating tool call
    sessions/ chains/  transcripts and handoff ledgers (plain JSON/JSONL)

The `.example` files are inert documentation: activate one by renaming it,
for example `mv gate.example gate && chmod +x gate`.

Scaffold files return if deleted but are never overwritten once present;
truncate one to empty to silence it for good.

sesh's own read/search tools refuse this directory (credentials live here);
bash can reach it, which is the documented trust boundary. Each mount here
has its own short README or .example; the project lives at
https://github.com/mike-diff/sesh
