# dispatch

A GitHub Actions workflow that turns an issue labeled `dispatch` into a
verified pull request. A headless `sesh` run owns the whole loop: research,
plan, implement, review, validate. The repository's own `ci.yml` is the
independent external check on the resulting PR.

It runs the same agent harness against the same codebase you would locally,
unattended, and opens a PR when the local verification gate is green.

## Why the assets live here, not in .sesh/

The repo keeps `.sesh/` fully gitignored: project-local sesh mods and config
are personal, never committed (see `.gitignore`, and `AGENTS.md`'s note that
the project `.sesh/` is always gitignored and doctor-checked).

So the dispatch agent definition lives in tracked `.github/dispatch/`, and the
workflow assembles a runtime `.sesh/` on the runner from these files before
starting sesh. Nothing is committed under `.sesh/`; the repo's convention is
respected, and the agent config is still versioned and reviewable.

Contents:

```
.github/dispatch/
  APPEND_SYSTEM.md     the dispatch agent prompt (always-on)
  providers.json       GLM endpoint + model; key_env -> ZAI_API_KEY (no secret)
  skills/
    lean-build/              on-demand: loads when touching code
    unit-test-quality/       on-demand: loads when touching tests
```

The skills are on-demand and description-matched: the agent loads them only
when the task fits, so they do not fire on every run. They are vendored from
the sesh scaffold skills and are intended to be shared as project skills.

### Frontloading

The two skills also have their operational rules inlined into
`APPEND_SYSTEM.md` as "always active" sections, so the guidance is guaranteed
in the system prompt from turn one. It does not depend on the model reaching
for the on-demand skill tool, and it does not require network access to any
personal skill source. The full skill bodies (and the unit-test-quality
keep-vs-delete examples) stay committed at `.github/dispatch/skills/` for
depth; the prompt points to them by path and the agent reads the relevant one
when a change is non-trivial or a test is hard to classify.

## The three auth gates (safe on a public repo)

1. Label permissions. Only Write/Triage users can add labels on a public repo,
   so the public cannot trip the workflow by labeling an issue.
2. Sender allow-list. The job `if` requires `github.event.sender.login` to be
   in the allow-list. Edit it in `dispatch.yml` before enabling.
3. Environment approval. The job references the `dispatch` environment; set
   its required reviewer to yourself in Settings, Environments. The job pauses
   before the GLM key is injected, so nothing is spent until you approve. This
   is also the accidental-trigger cost valve.

## Secrets and config (Settings, Secrets and variables, Actions)

| Name | Where | Value |
|---|---|---|
| `ZAI_API_KEY` | `dispatch` environment secret | your GLM coding-plan key |
| `DISPATCH_APP_ID` | repo variable | the GitHub App's numeric ID |
| `DISPATCH_APP_PRIVATE_KEY` | repo secret | the App's PEM private key |

Create a GitHub App (Settings, Developer settings, GitHub Apps) with repository
access to this repo and permissions: Contents read+write, Pull requests
read+write, Issues read+write. Store its ID as `DISPATCH_APP_ID` and its
private key as `DISPATCH_APP_PRIVATE_KEY`.

The App name must be globally unique on GitHub and cannot collide with any
existing username (for example `dispatch-bot` is reserved by the `@dispatch-bot`
account). Pick something specific to you, like `mike-diff-dispatch` or
`sesh-dispatch`. Whatever you choose, the workflow derives the commit identity
from the App at runtime, so the name is cosmetic.

The App is required rather than the default `GITHUB_TOKEN` because the default
token cannot trigger downstream workflows: a PR opened with it would never run
`ci.yml`. An App token fires CI normally, is short-lived, and is scoped.

Create the `dispatch` environment (Settings, Environments, New environment),
set yourself as required reviewer, and add `ZAI_API_KEY` as an environment
secret there.

## The pipeline

```
issue labeled dispatch
   |
   v  gate 1: label perms (built into GitHub)
   v  gate 2: job if: sender allow-list
   v  gate 3: dispatch environment requires your approval
   |
   v  checkout (App token, fetch-depth 0) -> fresh branch dispatch/issue-<n>
   v  write issue title+body to TASK.md
   v  assemble runtime .sesh/ from .github/dispatch/
   v  install Go, install sesh (checksum-verified via install.sh)
   v  sesh -p "$(cat TASK.md)" -yes -provider zai -model glm-5.2
   |     research -> plan -> implement <-> review (gate + fresh subagent) -> validate
   |     exit 0 done, 1 error, 3 stuck, 4 iter cap
   v
   commit -> push -> gh pr create (Closes #N)
   v
   remove dispatch label (re-label to re-run)
   v
   PR's own ci.yml runs: the independent external validator
```

## Operating model

1. File an issue describing the change.
2. Add the `dispatch` label.
3. Approve the run when the environment gate pings you.
4. The agent runs for minutes to hours, iterating against the local gate
   (`go build ./... && go vet ./... && go test ./...`), then opens a PR.
5. The PR's own `ci.yml` runs as the final check.
6. Review and merge. The label was auto-removed; re-add it to dispatch a
   follow-up on the same issue.

Exit codes: 0 clean finish; 1 error (read the run log); 3 stuck (the agent
made no progress, usually an ambiguous issue); 4 iteration cap (raise
`-max-iters` or split the issue).

## Limits

- Hosted runners cap a job at 6 hours. For longer tasks, change `runs-on` to a
  self-hosted runner label; everything else stays the same.
- The issue body is prompt input. The agent only mutates this repo and never
  exfiltrates the key (GitHub masks secrets in logs; the prompt steers the
  agent to show tool results, not env dumps). The three review layers (local
  gate, PR CI, your review) convert spent tokens into shipped value.
- sesh ships a rolling release with no version tags (see `release.yml`), so the
  workflow uses `releases/latest/download` via `install.sh`, matching the
  documented install path. There is no tag to pin.
