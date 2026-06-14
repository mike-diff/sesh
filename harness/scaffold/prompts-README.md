# prompts/: override the model-facing templates

Drop a file named after a template to replace the built-in:

    brief.md      writes the handoff brief from the sealed transcript
    judge.md      rules done/blocked/continue after a tool-using turn
    task.md       frames a subagent's assignment
    seed.md       opens each new chain link
    continue.md   feeds the judge's reason into the next iteration

Placeholders like `{{cwd}}` are substituted; unknown ones stay visible so a
typo is seen, never swallowed. A project's `.sesh/prompts/<name>.md` wins
over this directory.

The placeholders each template accepts:

    brief.md      (none; the sealed transcript is appended)
    judge.md      (none; the request and transcript are appended)
    task.md       {{cwd}}
    seed.md       {{parent}} {{ledger}} {{brief}} {{repo}}
    continue.md   {{iteration}} {{verdict}} {{request}}
