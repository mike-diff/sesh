---
name: unit-test-quality
description: The bar for writing or reviewing unit tests. Use whenever creating, modifying or reviewing test files, adding test coverage, or before declaring code done. Enforces behavior-asserting tests over tautological, green-but-empty or flaky ones.
metadata:
  author: mike-salvati
  version: "1.0"
  source: Shopify gitstream test-quality bar (system/gitstream/intent/TESTING.md)
---

# Unit Test Quality

AI makes tests cheap to produce and easy to fake. Passing is no longer the bar.

## The one question

Before writing or keeping any test, answer it:

> What single one-line change to production code would make this test fail?

If you cannot name that change, the test proves nothing. Do not write it. If it already exists, delete it.

## Write the test first

1. Write the failing test that pins the behavior (red).
2. Make it pass with the smallest change (green).
3. Refactor with the test holding the line (clean).

Never write the test after the code to "lock in" whatever the code currently does. That produces tests that assert the implementation, not the behavior.

## Assert behavior, not implementation

Keep tests that exercise a real input/output contract or a real failure path:

- `expect(parse(bad)).toThrow(ValidationError)`
- `expect(total([2, 3])).toBe(5)`
- A real error path the code can actually raise (assert the error type the code truly throws, not a plausible-looking one).

## Delete the fakes

These pass but mean nothing. Do not write them; remove them on sight:

- **Tautology**: `expect(true).toBe(true)`, `expect(NAME).toBe('editor')` (constant vs its own literal), identity transforms, self-comparison, re-deriving the expected value with the same code under test.
- **Green but empty**: behavior-named test that only asserts "does not throw", or asserts nothing meaningful. Worse than no test, because it implies coverage that is not real.
- **Mock echo**: the test verifies the mock harness, not the code. If you are re-implementing the dependency's behavior in a fake and then asserting the fake, delete it.
- **Compiler-guaranteed**: asserting something the type system already enforces.
- **Flaky / race-prone**: wait for the condition, not the clock (signal > poll > fake clock > bounded sleep). Latency injected into a fake is fine. A flaky test is worse than no test; fix it or delete it, never quarantine-and-forget.

## The one legitimate exception: contract pins

Asserting a named constant equals its wire/SQL/protocol literal is NOT a tautology when that literal is a real external contract (an API field name, a DB column, an enum sent over the wire). Pin it deliberately and say why in the test name. A constant compared against itself with no external contract is still a tautology.

## Over-production

Agents love adding tests, negative assertions and large test diffs. More tests is not more safety. Prefer fewer tests that each pin distinct, real behavior over many that overlap or assert nothing. If a test does not earn its place by the one-question rule, cut it.

## Before declaring tests done

Run this self-review on every test you wrote or touched:

1. For each test, name the one-line production change that breaks it. If you cannot, delete the test.
2. No tautology, green-but-empty, mock-echo or compiler-guaranteed tests remain.
3. Failure-path tests assert the error the code actually raises.
4. No hard sleeps; timing waits on a condition.
5. The suite ran and passed locally.

## Calibrate against real examples

See `references/eval-cases.md` for labeled real tests (keep vs delete) drawn from
gitstream's test-quality bar. When unsure how to classify a test, match it to the
nearest case. To validate this skill, classify every case and name each breaker;
you should catch every `delete` and never misflag a `keep` (watch the contract
pins, which look like tautologies but are not).

Heuristic and patterns from Shopify gitstream's test-quality bar (`system/gitstream/intent/TESTING.md`, `test-reviewer` agent).
