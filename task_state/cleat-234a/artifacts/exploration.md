# cleat-234a: Verification Exploration

**Date:** 2026-06-06
**Explorer:** cleat-234ap (re-verification; original by cleat-234ae)
**Parent:** cleat-234 (CI Enforcement)

## Summary

All 11 key claims from the parent exploration (cleat-234 STATUS.md, 2026-06-05) were verified against current code by two independent explorer agents (cleat-234ae and cleat-234ap). **Status: fully verified — no drift.**

---

## 1. Closure Test Failures

**Tests run:** `go test ./internal/closure/... -run "TestComputeBasicIdentifiesDurableLeaves|TestComputeBasicCorrectlyTagsPureFunctions"`

**Results:**
- `TestComputeBasicIdentifiesDurableLeaves`: **FAIL** — "expected 8 cleat leaves, got 9" (LongRunning is extra)
- `TestComputeBasicCorrectlyTagsPureFunctions`: **FAIL** — "expected 12 functions, got 13"

**Root cause confirmed:** `LongRunning()` at `testdata/basic/order.go:175` calls `h.DurableCall("noop", "", "")` directly, adding a 9th durable leaf and 13th function. Test expectations in `internal/closure/closure_test.go` were not updated.

**Fix locations (unchanged from parent):**
- `closure_test.go:41` — Add `basicFQ("LongRunning"): true` to `expectedLeaves`
- `closure_test.go:120` — Change `12` to `13`

**Path correction:** TASK.md says `internal/closure/testdata/basic/order.go` but actual path is `testdata/basic/order.go`.

---

## 2. CI Workflow (`.github/workflows/ci.yml`)

### continue-on-error audit — all 9 occurrences verified:

| Line | Job/Step | Verified | Recommendation |
|------|----------|----------|---------------|
| 52 | Lint job | `continue-on-error: true` at start of `lint:` job | REMOVE — go vet is a core correctness check |
| 81 | Ruff (Python linter) | Inside lint job | Keep (informational) |
| 85 | ShellCheck | Inside lint job | Keep (informational) |
| 267 | test-python job | `continue-on-error: true` | REMOVE — Python SDK test failures are real regressions |
| 297 | test-java job | `continue-on-error: true` | REMOVE — Java SDK test failures are real regressions |
| 319 | test-assemblyscript job | `continue-on-error: true` | Keep (AS instability) |
| 369 | test-assemblyscript-wasm job | `continue-on-error: true` | Keep (AS WASM fragility) |
| 490 | build job | `continue-on-error: true` | REMOVE — build failures on main are deploy blockers |
| 558 | coverage job | `continue-on-error: true` | Keep (only on main pushes, informational) |

**Non-ci.yml continue-on-error:**
| File | Line | Job | Verdict |
|------|------|-----|---------|
| ecosystem-ci.yml | 63 | assemblyscript-sdk | Keep |
| release-notes-check.yml | 20 | check-release-notes | Keep |
| ai-pr-review.yml | 33 | AI review job | Keep |

**Total: 12 across 4 files — CONFIRMED.**

### Coverage job trigger (line 556):
```yaml
if: github.ref == 'refs/heads/main' && github.event_name == 'push'
```
**CONFIRMED:** Coverage only runs on push to main. PRs never get coverage results.

### test-tinygo disabled (line 211):
```yaml
if: false  # Temporarily skipped; Go version issues being explored on another branch
```
**CONFIRMED:** Job is disabled. Comment says "Go version issues" — may be resolved with TinyGo v0.41.0+ (Go 1.26 support).

### lint-go commented out (lines 99-112):
**CONFIRMED:** Entire lint-go job is commented out. Comment says "action doesn't support Go 1.26 yet".

### GO_VERSION_STABLE discrepancy:
- ci.yml line 33: `GO_VERSION_STABLE: "1.26"`
- go.mod line 3: `go 1.25.7`
- **CONFIRMED:** Version mismatch.

---

## 3. Ecosystem CI (`.github/workflows/ecosystem-ci.yml`)

### Path filter (lines 5-10):
```yaml
paths:
  - "cleat/**"
  - "internal/**"
  - "cmd/**"
  - "go.mod"
  - "go.sum"
```
**CONFIRMED:** Missing `plugins/**` and `crates/cleat-sdk/**`. Changes to those directories won't trigger ecosystem CI.

### AS SDK best-effort (line 63):
```yaml
continue-on-error: true
```
**CONFIRMED.**

---

## 4. Makefile Coverage Module Prefix Bug

**Line 158:**
```awk
sub(/^github\.com\/rcownie\/cleat\//, "", path);
```

**go.mod line 1:**
```
module github.com/cleat-team/cleat
```

**CONFIRMED:** The module prefix in the Makefile coverage check is `github.com/rcownie/cleat` but go.mod declares `github.com/cleat-team/cleat`. The `sub()` never matches, so package paths are never stripped to their short forms. This means the per-package threshold checks never trigger — the coverage check is effectively a **NO-OP**.

### Additional Makefile issues:
- **No global aggregate threshold** — only per-package thresholds exist (lines 143-151)
- **No ratchet mechanism** — no historical baseline tracking
- Per-package thresholds: `internal/` 50%, `internal/plugin/` 50%, `internal/host/` 15%, `cleat/` 15%, `plugins/` 20%, `cmd/` 0%

---

## 5. What Changed Since Parent Exploration

Nothing material changed. Both independent verifications (cleat-234ae and cleat-234ap) confirm identical code state. One minor path correction: `internal/closure/testdata/basic/order.go` → `testdata/basic/order.go`.

---

## 6. Implementation Readiness

The parent exploration's "Files That Need Changes" table (8 priority levels) is **accurate and current**. No adjustments needed.

### Can proceed independently of cleat-232/cleat-233:
- P0: Closure test fixes (lines 41, 120 of closure_test.go)
- P0: Remove `continue-on-error` from lint job (ci.yml:52)
- P0: Remove `continue-on-error` from build job (ci.yml:490)
- P1: Fix Makefile module prefix (Makefile:158)
- P2: Remove `continue-on-error` from test-python/test-java

### Needs cleat-232/cleat-233 green first:
- P2: Re-enable test-tinygo (needs multi-db green to assess impact)
- P3: Branch protection changes (needs all CI green)
- P3: Add global coverage threshold + ratchet (needs coverage baseline on green CI)

---

## 7. Recommendation

**This task is leaf-ready for implementation.** Verified independently by two explorer agents. No further exploration needed. The implementation can be done in priority order, with P0 and P1 items proceeding immediately and P2/P3 items waiting on cleat-232/cleat-233.
