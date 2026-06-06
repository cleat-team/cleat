# cleat-233ce Re-Verification Report

**Explorer:** cleat-233ce
**Date:** 2026-06-06
**Task:** cleat-233c — Rust SDK WASM Integration Test (re-verification)

## Verdict: STILL PASSING

Re-verified all claims from the 2026-06-05 STATUS.md and review (cleat-233cr). Everything is green.

## Verification Results (2026-06-06)

### Rust Crate Tests — 102 PASS
| Crate | Tests | Result |
|-------|-------|--------|
| cleat-sdk | 32 | PASS |
| cleat-macro | 8 integration + 5 UI | PASS |
| cleat-test | 57 | PASS |

### Go Integration Tests — 4 PASS
| Test | Duration | Result |
|------|----------|--------|
| TestRustWorkflowExecute | 0.14s | 4 service calls verified (inventory→payments→shipping→notifications) |
| TestRustWorkflowReplay | 0.20s | Deterministic, 0 real calls on replay |
| TestRustWorkflowCancelOrder | 0.12s | Cancellation entry point works |
| TestRustWorkflowCompensation | 0.13s | Saga refund+release: 5 steps (3 forward + 2 compensation) |

`-short` mode correctly skips all 4 tests (0.010s total).

### WASM Compilation
- `wasm32-wasip1` (test target): PASS, 122K, 1 unused-import warning
- `wasm32-unknown-unknown` (CLI target): PASS, 103K, same warning

### Issues Status

| Issue | Previous Status | Current Status |
|-------|----------------|----------------|
| CI gap (`./internal/host/...`) | FIXED (cleat-233cr) | FIXED — `./engine/...` at line 101 |
| `cleat_json_stringify` cfg gating | FIXED (cleat-233cr) | FIXED — both parse+stringify gated at L297, L307 |
| `findProjectRoot` go.mod walk-up | CORRECT | CORRECT — matches `findRepoRoot` pattern |
| Build target mismatch | OPEN (MEDIUM) | OPEN — CLI uses unknown-unknown, tests use wasip1 |
| Unused import warning | OPEN (LOW) | OPEN — `lib.rs:6` `use cleat_sdk::HostCalls` |

### Warnings (harmless)
- `use cleat_sdk::HostCalls` triggers unused-import warning because `#[cleat_entry]` references it internally, invisible to rustc.
- Build target mismatch between CLI (`wasm32-unknown-unknown`, avoids non-deterministic WASI imports) and tests (`wasm32-wasip1`). Documented rationale exists in `cmd/cleat/build_rust.go:14`.

## Delta from Original Report

No new issues found. Both items flagged as fixed by cleat-233cr (CI gap, cfg gating) remain fixed. The two remaining cosmetic items (build target, unused import) persist unchanged.
