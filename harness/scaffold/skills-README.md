# ~/.sesh/skills

Each directory here is one Agent Skill (agentskills.io format): focused
instructions the model loads on demand. The skill tool appears in the
toolset only when at least one valid skill exists; its description carries
a manifest line per skill, and the model loads a body only when a task
matches.

The shape:

    my-skill/
    ├── SKILL.md          required: frontmatter + instructions
    ├── scripts/          optional: executable helpers
    ├── references/       optional: long docs, loaded on demand
    └── assets/           optional: templates, resources

SKILL.md opens with YAML frontmatter. The rules are enforced; a skill that
breaks them does not load, and `sesh -doctor` names the violation:

    ---
    name: my-skill        # must equal the directory name;
                          # max 64 chars of [a-z0-9-], no edge hyphens
    description: >        # max 1024 chars; the ONLY text the model sees
      What this does and when to use it. Write "Use when the user..."
      covering intent variants; name what to skip it for.
    ---
    # Instructions follow, ideally under 5000 tokens.
    # Long material goes in references/ with a line saying when to load it.

A project can carry its own skills in `.sesh/skills/` (gitignored, same
trust class as AGENTS.md); they shadow same-named global ones and show as
[project] in the manifest.

These rules are enforced and are the whole contract; `sesh -doctor` names
any violation. Project: https://github.com/mike-diff/sesh
