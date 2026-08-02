# TLA+ specifications — status

Four specifications of cleat's concurrency-sensitive protocols.

**These have not been model-checked.** Read them as precise design documentation, not as
verification evidence, and do not cite them as proof that the protocols are correct.

## What is and is not true

| | Status |
|---|---|
| Written | Yes — four specs, ~1,400 lines |
| Syntactically whole modules | Yes, as of 2026-08-02 (see below) |
| Parsed by SANY | **No** |
| Checked by TLC | **No** |
| `.cfg` configuration files | **Do not exist** |
| Run in CI | **No** |

`CleatClaim.tla` used a full-width `======` rule as a decorative section separator. In TLA+
that is the module *terminator*, and the first one sat at line 53 of 495 — so roughly 89% of
the specification was outside the module and would have been ignored by any TLA+ tool. Those
separators are now commented (`\* ====`). That the file survived in this state is itself
evidence it was never run through SANY or TLC.

## Drift

The implementation references in the spec comments are stale. `CleatClaim.tla` cites
`internal/host/db.go` and `cmd/durable-worker/`; both moved in commit `3eeb74e` (2026-06-01)
and the `durable→cleat` rename. Line numbers should be assumed wrong.

## To actually verify these

Each spec needs a `.cfg` naming its `CONSTANTS`, `INIT`, `NEXT` and the invariants to check.
`CleatClaim.tla` carries a suggested configuration in a comment at the end of the file.

```sh
# tla2tools.jar is not vendored here; fetch it from
# https://github.com/tlaplus/tlaplus/releases
java -cp tla2tools.jar tlc2.TLC specs/CleatClaim.tla -config specs/CleatClaim.cfg
```

Until that runs green in CI, this directory documents intent rather than establishing
correctness. Tracked in `IMPROVEMENT-PLAN.md` Phase 4.
