---
name: report-issue
description: Use when the user wants to file a GitHub issue about sesh itself: a bug report, a feature request, or a question about how sesh works. Triggers on phrasing like "report a bug", "file an issue", "open an issue", "this is broken", "feature request", "sesh should...", or clear frustration with sesh's own behavior (the harness, its tools, the drive loop, handoff, mods, or the skill/mcp engines). It gathers the sesh-specific context a maintainer needs, fills a bug, feature, or question template, shows you the result, and files it only after you approve. Skip for bugs in the user's OWN project code (that is ordinary coding work, not a sesh issue) and for issues about other tools or services.
---

# Report a sesh issue

Turn a bug report, feature request, or question about sesh into a complete
GitHub issue. You collect the sesh-specific context a maintainer needs, fill the
matching template, show the user the result, and file it only after they approve.
The issue goes to the sesh project repo, never to the user's own repositories.

<constraints>
Privacy first: the issue is public. These are hard rules.

- NEVER include API keys, credentials, provider base URLs, environment variable
  values, file paths (absolute or home), or machine details (OS, hardware).
- NEVER paste the user's project code, file contents, or transcript text unless
  the user explicitly approves a specific snippet. Ask first, every time.
- Mod file CONTENTS are off by default. Report that a mod is present, not what is
  inside it, unless the user opts in.
- Keep it about sesh. If the fault is in the user's own code, this is not the
  right tool: say so and stop.
- Do not file anything until the user approves the previewed body (see workflow).
</constraints>

<workflow>
STOP. Finish each step before the next.

1. Classify the request as bug, feature, or other (a question or docs gap). If it
   is unclear, ask one short question to decide. Then load the templates with
   `skill {"name": "report-issue", "file": "references/templates.md"}` and use the
   matching one.

2. Collect machine-safe context. The skill ships `scripts/collect-context.sh`.
   The bash tool runs from the project root, not this skill's directory, so find
   the script under whichever mount the skill is installed in and run it:

   ```
   for p in .sesh/skills/report-issue/scripts/collect-context.sh "$HOME/.sesh/skills/report-issue/scripts/collect-context.sh"; do [ -f "$p" ] && sh "$p" && break; done
   ```

   It prints only sesh facts (build commit, active mod names, tuning overrides,
   engine counts), never keys, URLs, paths, or machine specs. If the script is
   not found or exits nonzero, read its contents with
   `skill {"name": "report-issue", "file": "scripts/collect-context.sh"}` and
   gather the same facts by hand. Never invent values.

3. Add runtime facts you already hold: from your <identity> block, the current
   provider's protocol and model, and the context window if known. sesh's
   behavior is model-dependent, so a bug needs these. Do NOT read providers.json
   (it contains endpoint URLs); use the identity block.

4. Fill the template from the user's words. Bug: steps to reproduce, expected,
   actual, and the exact tool call plus the error text if a tool is involved.
   Feature: the problem, the proposed solution, which layer it touches (core,
   provider, harness, or a mod), and whether it could ship as a mod instead of
   core. Other: the question and what they already tried.

5. Re-read the filled body against the constraints and strip anything that
   slipped through: a path, a URL, a key, or pasted code the user did not approve.

6. Preview: show the user the exact title and body that will be filed. STOP and
   wait for explicit approval. If you cannot get approval in this run (for
   example a one-shot, non-interactive `-p` invocation), do NOT file: print the
   body and the URL `https://github.com/mike-diff/sesh/issues/new` and stop.

7. File it after approval. Write the body to a temp file, then run with bash:
   `gh issue create --repo "${SESH_ISSUE_REPO:-mike-diff/sesh}" --title "<prefixed title>" --body-file <tmpfile>`.
   Report the issue URL gh prints. If gh is missing or unauthenticated, fall back
   to printing the body and the issues/new URL above for manual submission.
</workflow>

<scope>
File one issue per request, to the sesh project repo only (a fork can override
the target with the `SESH_ISSUE_REPO` environment variable). Never open an issue
on the user's own repository, and never file without the preview-and-approve
step. Routine coding in the user's project is out of scope for this skill.
</scope>
