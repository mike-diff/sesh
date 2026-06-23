---
name: lean-build
description: Minimal-diff feature development workflow. Use whenever building, modifying, integrating or refactoring code, especially when the work touches shared infrastructure, third-party APIs, auth, audit, deploy, config or data paths.
---

# Lean Build

Use this skill to keep code changes small, reviewable and grounded in the existing codebase.

## Goal

Build the smallest useful version with the fewest files, dependencies and abstractions that can safely solve the problem.

## Before Editing

1. Restate the smallest useful version.
2. List explicit non-goals.
3. Search for existing patterns, utilities and nearby tests.
4. Identify high-risk shared files such as auth, audit, config, deploy, routing and central request handling.
5. Propose the minimum file-touch set.
6. List files intentionally avoided.
7. Challenge each planned new file, dependency, config knob and shared-file edit.

Do not start editing until the plan explains why each touched file must change.

## Default Decisions

Prefer:
- Existing extension points over new frameworks.
- Repo-native auth, audit, logging, config, deploy and test patterns.
- Direct small wrappers over new dependencies when the dependency does not reduce risk.
- Static allowlists for third-party tool surfaces.
- Named code constants for safety limits that should require review to loosen.
- Runtime validation and clear errors over long user-facing tool descriptions.
- Default-off enablement for new third-party or sensitive capabilities.

Avoid:
- avoid versioning (v1, v2), maintain a single codebase
- New services unless the repo cannot safely host the work.
- Dynamic discovery for third-party tools.
- Generic frameworks built for one use case.
- Custom observability paths when native sinks already answer the question.
- Env config for product or safety guardrails unless operators truly need runtime control.
- Silent error swallowing.

## During Implementation

- Make the smallest targeted edits.
- Keep copy and tool descriptions short.
- Put safety policy in validators, errors, comments, docs and tests.
- Bound external calls with timeouts, input caps, output trimming and response redaction where needed.
- Keep central-file changes as tiny hooks, not broad rewrites.
- Clamp harmless model overages when safe; fail clearly for unsafe requests.
- Do not add unused exports, plumbing or future-proofing.

## Required Validation

Run the smallest relevant tests first, then the repo-standard validation before done.

At minimum report:
- targeted tests run
- full lint/type/test command if applicable
- any validation not run and why

## Cleanup Pass

Before final response:
1. Review `git diff --stat`.
2. Review new files and imports.
3. Remove unused code, logs, comments and tests.
4. Collapse abstractions that did not earn their keep.
5. Confirm no secret values, deploys, commits or pushes happened unless requested.

## Final Summary Format

Keep it short:
- changed files or areas
- files avoided if relevant
- non-goals preserved
- validation run
- remaining blockers or decisions
