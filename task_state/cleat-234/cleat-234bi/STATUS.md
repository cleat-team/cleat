# cleat-234bi Status

**Task:** Independent investigation of cleat-234 (CI Enforcement)
**Status:** done — independent verification complete
**Date:** 2026-06-06

## Summary

All major claims from the cleat-234 exploration verified against current code. All findings confirmed. One critical new finding: the Makefile coverage check bug is more severe than previously reported — it's a complete no-op, not a cosmetic issue.

## Key Findings

### Confirmed (matching prior explorations)
- Both closure tests fail exactly as described (LongRunning adds 9th leaf, 13th function)
- 12 continue-on-error across 4 workflow files (5 safe to remove)
- Coverage job only fires on push to main (ci.yml:556)
- test-tinygo disabled with `if: false` (ci.yml:211)
- lint-go entirely commented out (ci.yml:99-112)
- Ecosystem CI path filter missing `plugins/**` and `crates/cleat-sdk/**`
- Go version discrepancy: go.mod 1.25.7 vs CI GO_VERSION_STABLE 1.26

### New Finding: Makefile Coverage Check Is Completely Non-Functional

Previous explorations described this as "cosmetic" or "likely still works." The awk threshold matching logic (`index(p, prefix "/") == 1`) NEVER matches because the module prefix (`github.com/rcownie/cleat` vs actual `github.com/cleat-team/cleat`) is never stripped. Every package gets threshold -1 and is skipped. The coverage check always passes vacuously.

**This is high-severity** because:
1. Coverage has never actually been enforced
2. Fixing the prefix bug will suddenly surface real coverage gaps
3. The threshold matching logic needs correction too (exact prefix matches fail due to trailing "/" requirement)

See `artifacts/investigation.md` section 9 for detailed awk trace.

## Artifact

`artifacts/investigation.md` — full independent verification with awk trace analysis

## Verdict

**PASS with one elevated finding.** The cleat-234 exploration is accurate. The Makefile coverage check severity should be upgraded from "cosmetic" to "non-functional blocker" in the implementation plan. The P1 "Fix Makefile module prefix" item should be elevated to P0 because fixing it will reveal actual coverage gaps that need to be addressed.
