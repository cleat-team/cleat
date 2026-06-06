# cleat-234e Status

**Phase:** explored
**Last updated:** 2026-06-06
**Explored by:** cleat-234ep (explorer agent)
**Parent task:** cleat-234

## Verification Summary

All claims from prior cleat-234 explorations verified against current code. Both closure tests fail exactly as described. All 12 continue-on-error instances confirmed. Makefile prefix bug and coverage gaps confirmed.

### Confirmed by independent test run:

- `TestComputeBasicIdentifiesDurableLeaves` — FAIL: unexpected leaf `LongRunning`, expected 8 got 9
- `TestComputeBasicCorrectlyTagsPureFunctions` — FAIL: expected 12 functions, got 13
- Root cause: `LongRunning()` at `testdata/basic/order.go:175` calls `h.DurableCall("noop", "", "")`
- 12 `continue-on-error: true` across 4 workflow files (5 safe to remove)
- Makefile line 158: `github.com/rcownie/cleat` vs go.mod `github.com/cleat-team/cleat`
- Coverage job only fires on push to main (ci.yml:556)
- test-tinygo disabled with `if: false` (ci.yml:211)
- lint-go entirely commented out (ci.yml:99-112)
- go.mod `go 1.25.7` vs CI `GO_VERSION_STABLE: "1.26"` mismatch

### Required fixes (unchanged from prior explorations)

| Priority | File | Line(s) | Change |
|----------|------|---------|--------|
| P0 | `internal/closure/closure_test.go` | 40 | Add `basicFQ("LongRunning"): true,` to expectedLeaves |
| P0 | `internal/closure/closure_test.go` | 120 | Change `12` to `13` |
| P0 | `.github/workflows/ci.yml` | 52 | Remove `continue-on-error` from lint job |
| P0 | `.github/workflows/ci.yml` | 490 | Remove `continue-on-error` from build job |
| P1 | `Makefile` | 158 | Fix module prefix to `github.com/cleat-team/cleat` |
| P1 | `.github/workflows/ci.yml` | 556 | Extend coverage to PRs |
| P2 | `.github/workflows/ci.yml` | 267, 297 | Remove continue-on-error from test-python, test-java |
| P2 | `.github/workflows/ci.yml` | 211 | Re-enable test-tinygo (TinyGo v0.41.1 supports Go 1.26) |

## Recommendation

Task is leaf-ready. All P0 changes are independent of cleat-232/cleat-233 dependencies and can proceed immediately. The closure test fixes are a 2-line change. The CI continue-on-error removals are 2-line deletions.

## Next Phase

Ready for `planning` — a planner agent should create the implementation plan. The implementation is well-understood and scoped: 4 lines of code changes + 2 CI config deletions.
