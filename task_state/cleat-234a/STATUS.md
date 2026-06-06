# cleat-234a Status

**Phase:** planning
**Last updated:** 2026-06-06
**Explored by:** cleat-234ap (re-verification, independent of cleat-234ae)
**Dispatched by:** manual

## Re-Verification Summary (cleat-234ap)

Fresh independent verification of all 6 TASK.md action items plus the 11 parent claims. **No drift — parent exploration remains accurate.**

### Verified claims:
1. **Closure tests fail** — `TestComputeBasicIdentifiesDurableLeaves`: FAIL ("expected 8 cleat leaves, got 9"), `TestComputeBasicCorrectlyTagsPureFunctions`: FAIL ("expected 12 functions, got 13"). Root cause: `LongRunning()` at `testdata/basic/order.go:175` calls `h.DurableCall("noop", "", "")`.
2. **ci.yml continue-on-error: 9 occurrences** at lines 52, 81, 85, 267, 297, 319, 369, 490, 558. Total across 4 workflow files: 12 (confirmed).
3. **Coverage job trigger** at line 556: `if: github.ref == 'refs/heads/main' && github.event_name == 'push'` — confirmed.
4. **test-tinygo disabled** at line 211: `if: false` — confirmed.
5. **lint-go commented out** lines 99-112 — confirmed.
6. **Makefile module prefix bug** at line 158: `sub(/^github\.com\/rcownie\/cleat\//, "", path)` vs go.mod `github.com/cleat-team/cleat` — confirmed.
7. **go.mod `go 1.25.7`** vs CI `GO_VERSION_STABLE: "1.26"` — confirmed mismatch.
8. **Ecosystem CI path filter** missing `plugins/**` and `crates/cleat-sdk/**` — confirmed.
9. **LongRunning function** exists at `testdata/basic/order.go:175` (TASK.md says `internal/closure/testdata/basic/order.go` — minor path correction).

### One path correction from TASK.md
TASK.md line 24 says `internal/closure/testdata/basic/order.go`. Actual path is `testdata/basic/order.go`. The `internal/closure/` prefix is incorrect.

### Recommendation
Task is leaf-ready. P0/P1 items can proceed immediately. P2/P3 items should wait for cleat-232/cleat-233 green lights.
