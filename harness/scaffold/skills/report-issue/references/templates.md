# sesh issue templates

Pick the template that matches the request, then fill every section. Delete a
section only when it genuinely does not apply. The title prefix keeps issues
sortable without depending on repo labels (which may not exist).

Before filing, re-check the body against the skill's privacy constraints: no
keys, URLs, paths, machine specs, or unapproved project code.

## Bug report

Title: `[bug] <one-line summary>`

Body:

```
### What happened
<the wrong behavior in a sentence or two>

### Steps to reproduce
1. ...
2. ...

### Expected
<what should have happened>

### Actual
<what happened instead, with the exact error text if there was one>

### Tool involved (if any)
- tool: <read | search | loc | write | edit | bash | task | recall | skill | mcp>
- the call and the result or error it returned, verbatim (no project code)

### sesh context
- build: <from collect-context.sh>
- provider protocol / model / context window: <from your identity block>
- active mods: <from collect-context.sh>
- tuning overrides: <from collect-context.sh, or "none">
- invocation: <interactive | -p>; flags: <-ask | -yes | -max-iters N | -unsafe-paths | none>
```

## Feature request

Title: `[feature] <one-line summary>`

Body:

```
### Problem / motivation
<what you cannot do today, and why it matters>

### Proposed solution
<what you want sesh to do>

### Which layer does this touch?
<core (the agent loop) | provider (the wire) | harness (the product) | a mod>, and why

### Could it be a mod instead of core?
<sesh keeps policy at the edge. Explain why this must be built in, or how it could
ship as a mod: a skill, a gate, a tool, a prompt or tuning override.>

### Alternatives considered
<other approaches or workarounds you tried>

### sesh context (only if relevant)
- build / model: <include only when the request is version- or model-specific>
```

## Other (question or docs gap)

Title: `[question] <one-line summary>`

Body:

```
### Question
<what you want to understand>

### What you already tried
<docs read (AGENTS.md, the ~/.sesh guides, `sesh -help`) and things attempted>

### Context
<the relevant sesh behavior, redacted>
```
