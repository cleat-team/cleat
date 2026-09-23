# TLA+ specifications — status

Six specifications of cleat's concurrency-sensitive protocols.

**`CleatClaim.tla` (cleat#1996), `CleatRunLifecycle.tla` (cleat#1997),
`CleatQueueAdmission.tla` (cleat#2000) and `CleatDurableCallIntent.tla` (cleat#1999) have been
model-checked.** The other two — `CleatSignals.tla`, `CleatStateMachine.tla` — are **not
maintained**. Per cleat#1996 they are not being brought up to SANY/TLC parity themselves; they
are superseded by `CleatRunLifecycle.tla` and by cleat#1998 (parents awaiting children,
parent-close cascades) — read that issue for the actual scope rather than assuming a 1:1 file
replacement, since it does not commit to reusing a legacy file's name or structure.
`CleatDurableCallIntent.tla` (durable-call intents, ambiguity, re-replay, retention) is a
fourth model-checked spec with no legacy predecessor in this directory at all. Do not cite
either unmaintained file as proof a protocol is correct, and re-derive the table below rather
than trusting it — see CLAUDE.md's own rule on why a count like this rots.

**`CleatConcurrencyKeys.tla` is gone, not merely superseded.** cleat#2000's own scope was to
replace it, and unlike Signals/StateMachine (which have no committed replacement file yet)
`CleatQueueAdmission.tla` is a direct, landed replacement — see `specs/CleatQueueAdmission.md`'s
"What `CleatQueueAdmission.tla` covers, and what it deliberately doesn't" for the boundary
between what it took over and what it deliberately left out (the bare-key mutex case
`CleatConcurrencyKeys.tla` modelled is argued, not re-modelled). Keeping a confirmed-superseded,
never-checked file around past its replacement landing is a maintenance burden with no
offsetting benefit, so it was deleted rather than left to keep saying "not maintained" beside
its own replacement.

Each model-checked spec has its own status file, `specs/<Spec>.md`, holding what it checks, its
`.cfg` bounds, the measured state count, and its known-positive table — that is where a TLC
result changes when the spec is touched, not here. This file covers only what does not change
per spec: the method (how a counterexample gets promoted to a Go test, and how a new model gets
added) and the index below.

## Index

<!-- BEGIN GENERATED: spec-index -->
<!-- Regenerate with: python3 scripts/check-tla-ci-paths.py --write -->
| spec | issue | checked by TLC | maintained | status file |
|---|---|---|---|---|
| `CleatClaim.tla` | cleat#1996 | Yes | Yes | [`CleatClaim.md`](CleatClaim.md) |
| `CleatDurableCallIntent.tla` | cleat#1999 | Yes | Yes | [`CleatDurableCallIntent.md`](CleatDurableCallIntent.md) |
| `CleatKeyRotation.tla` | cleat#1991 | Yes | Yes | [`CleatKeyRotation.md`](CleatKeyRotation.md) |
| `CleatQueueAdmission.tla` | cleat#2000 | Yes | Yes | [`CleatQueueAdmission.md`](CleatQueueAdmission.md) |
| `CleatRunLifecycle.tla` | cleat#1997 | Yes | Yes | [`CleatRunLifecycle.md`](CleatRunLifecycle.md) |
| `CleatSignals.tla` | — | No | **No — superseded, see above** | — |
| `CleatStateMachine.tla` | — | No | **No — superseded, see above** | — |
<!-- END GENERATED: spec-index -->

A spec is "checked by TLC" here iff `specs/<Spec>.tla` has a matching `specs/<Spec>.cfg`; it has
a status file iff `specs/<Spec>.md` exists. Re-derive rather than trusting this table — see
CLAUDE.md's own rule on why a count like this rots; `scripts/check-tla-ci-paths.py` (no flag)
fails CI if this block is stale.

## Implementation references are stale in the two unmaintained specs

`CleatClaim.tla`'s and `CleatQueueAdmission.tla`'s headers name their current implementing
files (no line numbers) and are current as of the PR that landed each. The two unmaintained
specs still cite `internal/host/db.go` and similar paths from before commit `3eeb74e`
(2026-06-01, the `internal/host` → `engine` move and the `durable → cleat` rename) — do not
trust them, and do not spend time fixing them, since they are superseded rather than being
brought current.

## Invariant-to-test mapping

The policy going forward, for `CleatClaim.tla`, `CleatRunLifecycle.tla`,
`CleatQueueAdmission.tla`, and for the new models in cleat#1998-#1999: when TLC reports a
counterexample on a GATED invariant or property, the fix is not to land directly. First write
a dialect-parameterised Go test that reproduces the counterexample's trace against the real
store (see `engine/heartbeat_batch_fenced_test.go` for the shape — a store-level test with a
`postgres`/`mysql`/`mssql` subtest each), confirm it fails the way the model predicted, then
land the fix with that test alongside it. A TLC counterexample without a corresponding Go test
is a claim about the model, not yet a verified claim about the implementation; CLAUDE.md's
"could this check have disagreed?" question applies here exactly as it does to any other
guard.

`CleatDurableCallIntent.tla` is the first spec to actually exercise this policy: S1 and L1
both found real counterexamples (see `specs/CleatDurableCallIntent.md`), and per the policy
the fix is not landed in this PR. The counterexample traces are filed as a tracked issue (the
retention-sweep-clears-a-pending-intent gap, and its two Go-level symptoms — the guard in
`engine/admin_ops.go`'s `ReReplay`, and the stale "can never match" comments in `engine/db.go`
/ `engine/retention_predicates.go`), with the Go regression test to be written alongside that
fix, not this one — see cleat#2038, which shipped that guard and test. `RetryWorkflow`'s
complete absence of an equivalent guard (see above) is filed as a second, separate issue — a
missing guard and a guard defeated by retention are different defects.

No counterexample has been found yet on any of the other three specs' shipped, GATED
invariants/properties, so there is nothing to map today for them — this section exists so the
next one that surfaces has a documented process to follow rather than an ad hoc call.
`CleatRunLifecycle.tla`'s `L1_EventualSettlement`/`L2_ClaimProgress`/`L3_CascadeProgress` are a
distinct, already-documented case (see `specs/CleatRunLifecycle.md`): three genuine
counterexamples were found for them during development, which is exactly why they are defined
but deliberately left ungated rather than shipped as a false green — not a gated property
failing, so this policy does not (yet) apply to them. The counterexamples TLC has produced
against a GATED property so far are `CleatQueueAdmission.tla`'s three deliberate mutations
recorded in `specs/CleatQueueAdmission.md`'s "Bounds and state count" (`S1`'s
`ConcurrencyLimit` conjunct removed, and `WF_vars(Claim(w))` / `WF_vars(Reap)` each removed
from `Fairness`, one at a time, to prove `S1`/`L1`/`L2`/`S2` can actually fail); those are
known-positive controls on the checker, not defect reports, and need no Go test for the same
reason a passing negative control never does.

## Adding a model (cleat#1998-#1999)

Per cleat#1996, the treatment is the same `CleatClaim.tla`, `CleatRunLifecycle.tla` and
`CleatQueueAdmission.tla` all got, whether the result is a new file or eventually retires one
of the two remaining unmaintained ones: get it parsing and checking clean, write a `.cfg` with
bounds small enough for `make tla` to stay well under a minute for that spec, and write
`specs/<Spec>.md` recording the bounds and state count (see any existing one for the shape:
what's checked, the `.cfg` bounds, the measured state count, and a known-positive table). Start
its first line with `<!-- tla-index: issue=NNNN -->` naming the tracking issue — the generator
below reads that to build the index and reads `Implemented by` in the `.tla` header (see any
existing spec) to build both `.github/workflows/tla.yml`'s CI trigger and the invocation list
below. No per-spec CI wiring or README edit should be needed beyond those two files: run

    python3 scripts/check-tla-ci-paths.py --write

which regenerates the index above, the invocation list below, and `tla.yml`'s `paths:` list
from what's on disk, then run `scripts/check-tla-ci-paths.py` (no flag) to confirm nothing else
drifted, and `make tla` to confirm it stays fast. Each new model's header must list its actors
"from the code, not from memory" (cleat#1996's own rule) with the grep that found them, re-run
whenever the spec is touched.

**"Well under a minute" is a target, not a rule that overrides soundness.**
`CleatQueueAdmission.cfg` measures ~53s, not comfortably under a minute — see
`specs/CleatQueueAdmission.md`'s "Only one worker in the shipped bound" for why: the dimension
that would have shrunk it (a second worker) is exactly the one whose true cost the earlier,
unsound `ClockBound` was hiding, and the dimension that looks like an easy cut
(`RunsPerTenant = 1`) makes `L2` vacuous instead. Prefer a slow-but-sound bound to a
fast-but-vacuous one; if a new model faces the same tradeoff, name it in that spec's own status
file rather than silently shipping the fast bound.

```sh
# tla2tools.jar is not vendored here; fetched by checksum in CI. Locally:
# https://github.com/tlaplus/tlaplus/releases
# BEGIN GENERATED: tlc-invocations (python3 scripts/check-tla-ci-paths.py --write)
java -cp tla2tools.jar tlc2.TLC -config specs/CleatClaim.cfg specs/CleatClaim.tla
java -cp tla2tools.jar tlc2.TLC -config specs/CleatDurableCallIntent.cfg specs/CleatDurableCallIntent.tla
java -cp tla2tools.jar tlc2.TLC -config specs/CleatKeyRotation.cfg specs/CleatKeyRotation.tla
java -cp tla2tools.jar tlc2.TLC -config specs/CleatQueueAdmission.cfg specs/CleatQueueAdmission.tla
java -cp tla2tools.jar tlc2.TLC -config specs/CleatRunLifecycle.cfg specs/CleatRunLifecycle.tla
# END GENERATED: tlc-invocations
```

Tracked in `IMPROVEMENT-PLAN.md` Phase 4, and in cleat#1998-#1999.
