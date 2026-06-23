# Eval cases: keep or delete

Labeled real tests for the one-question bar. Each case is `keep` (proves real
behavior) or `delete` (passes but proves nothing). Labels and sources are from
Shopify gitstream's test-quality bar (`system/gitstream/intent/TESTING.md`,
`.claude/agents/test-reviewer.md`), not invented here. A few clearly-marked
synthetic cases cover the simplest shapes.

How to use as an eval: for each case, classify keep/delete and name the one-line
production change that would break it. Score precision/recall on the `delete`
class (catching slop). Target: catch every `delete`, never misflag a `keep`
(especially contract pins).

---

## DELETE — tautological / no-value

### D1 — self-comparison (re-derived expectation)
- Source: `system/gitstream/internal/synthesisoutbox/session_test.go` (deleted `TestSession_LockKeyMatchesReviewProject`)
- Defect: computes `want` and `got` by calling the same `reviewproject.PRLockKey(...)` with the same args, then asserts equal. Neither side runs the production path it claims to fence.
- Breaker: none. `AcquireSession` could change its key derivation and this stays green.
- Fix: assert the production accessor matches the helper (`TestSession_AccessorsReturnConstructorValues`), or observe the SQL the production path dispatches.

### D2 — identity transform
- `expect(Status("nope").String()).toBe("nope")` where `String()` is `return string(x)`.
- Defect: the asserted value was built from the same literal; tests the language, not our code.
- Breaker: none.

### D3 — boolean constant (synthetic)
- `expect(true).toBe(true)`
- Breaker: none. Pure tautology.

### D4 — green-but-empty (synthetic)
- A test named `it("rejects invalid input")` whose only assertion is `expect(() => run(x)).not.toThrow()`.
- Defect: behavior-named, asserts nothing about the rejection. Worse than no test: implies coverage that is not real.
- Breaker: none meaningful.

### D5 — mock echo
- Configure a fake to return `X`, call through a pass-through, assert the result is `X`.
- Defect: verifies the mock harness, not the code under test.
- Breaker: changing production logic does not fail it; only changing the fake does.

### D6 — JSON round-trip with no custom codec (synthetic)
- `expect(JSON.parse(JSON.stringify(x))).toEqual(x)`
- Defect: tests the serializer, not our code.
- Breaker: none.

### D7 — race-prone timing
- Source pattern called out in `TESTING.md` (Seq assertion / reliability sections).
- `time.Sleep(200ms); assertBundleLanded()` — gambles a hard sleep against an async condition.
- Defect: green on a quiet laptop, flaky under CI load.
- Fix: wait on the signal the code emits, or poll the observable effect with a deadline, or a fake clock. Injecting latency *into a fake* is fine.

---

## KEEP — proves real behavior

### K1 — contract pin (NOT a tautology)
- Source: `synthesisdrain.TestOutcomeValues` pinning `OutcomeSucceeded == "succeeded"`.
- Why keep: the left side is a production constant; the literal is an external wire/SQL contract a refactor could silently break.
- Breaker: renaming the constant's wire value to `"success"` fails it. That is the point.

### K2 — behavioral fence (the replacement for D1)
- Source: `TestSession_AccessorsReturnConstructorValues` — asserts `sess.LockKey()` equals `reviewproject.PRLockKey(repositoryID, prNumber)` for a Session built mirroring that call.
- Breaker: `Session.LockKey` decoupling from `PRLockKey` fails it.

### K3 — differential (optimized vs reference)
- Source: `internal/pushpolicy/secret_diff_equiv_test.go`.
- Why keep: asserts an optimized path agrees with a simple reference implementation across inputs.
- Breaker: any divergence in the optimized path fails it.

### K4 — parity (two production flavours must agree)
- Source: `TestFormatAuthDenied_StringAndWriterParity`.
- Breaker: the String and Writer renderings drifting apart fails it.

### K5 — fail-injection
- Force the error condition, assert the recovery / sentinel.
- Source: zero-value safety contract in `session_test.go` (`TestSession_ZeroValueMethodsAreSafe`): after `Release`, every row method returns `ErrSessionReleased` rather than panicking.
- Breaker: a method panicking or returning a different error on a released session fails it.

### K6 — value behavior (synthetic)
- `expect(total([2, 3])).toBe(5)`
- Breaker: an off-by-one or wrong reducer in `total` fails it.

### K7 — real failure path (synthetic)
- `expect(() => parse(bad)).toThrow(ValidationError)`
- Breaker: `parse` swallowing the error or throwing a different type fails it.
