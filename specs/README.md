# TLA+ specifications — status

Five specifications of cleat's concurrency-sensitive protocols.

**`CleatClaim.tla` (cleat#1996) and `CleatRunLifecycle.tla` (cleat#1997) have been
model-checked.** The other three — `CleatConcurrencyKeys.tla`, `CleatSignals.tla`,
`CleatStateMachine.tla` — are **not maintained**. Per cleat#1996 they are not being brought
up to SANY/TLC parity themselves; they are superseded by `CleatRunLifecycle.tla`, by
cleat#1998 (parents awaiting children, parent-close cascades) and cleat#2000 (queue
admission, rate tokens, claim fairness) — read those issues for the actual scope rather than
assuming a 1:1 file replacement, since none of them commits to reusing a legacy file's name
or structure. cleat#1999 (durable-call intents, re-replay, retention) is a fourth new model
with no legacy predecessor in this directory at all. Do not cite any of the unmaintained
three as proof a protocol is correct, and re-derive the table below rather than trusting
it — see CLAUDE.md's own rule on why a count like this rots.

**cleat#2034, filed 2026-09-23: `CleatClaim.tla`'s own `ClaimProgress` may be silently
vacuous, not actually verified by the "No error has been found" this file used to cite
unqualified.** Found while building `CleatRunLifecycle.tla`'s known-positive checks: deleting
`WF_vars(Claim(w))` from `CleatClaim.tla`'s own `Fairness` — which should trivially make
"eventually something is claimed" fail, since nothing then forces a claim — still reports "No
error has been found," identical state count to the unmutated run. Likely the same
CONSTRAINT-boundary hazard `CleatRunLifecycle.tla` hit three times and fixed (see that file's
own `SettledIsFinal`/`Fairness` comments); not yet fixed in `CleatClaim.tla` itself, and not
yet checked against its other three properties. Treat `ClaimProgress` (and, unverified,
`ReapProgress`/`TerminalStableLiveness`/`NoStarvation`) as **checked-but-not-proven-capable-
of-failing** until cleat#2034 closes.

## What is and is not true, per spec

| | CleatClaim | CleatRunLifecycle | ConcurrencyKeys | Signals | StateMachine |
|---|---|---|---|---|---|
| Parsed by SANY | **Yes** | **Yes** | No | No | No |
| Checked by TLC | **Yes**, but see cleat#2034 | **Yes** | No | No | No |
| `.cfg` exists | **Yes** | **Yes** | No | No | No |
| Run in CI | **Yes**, on touching PRs | **Yes**, on touching PRs | No | No | No |
| Maintained | **Yes** | **Yes** | **No — superseded, see above** | **No — superseded, see above** | **No — superseded, see above** |

## `CleatRunLifecycle.tla` (cleat#1997)

Models every writer of `workflow_instances.status` — the claim/heartbeat/reap protocol
`CleatClaim.tla` already covers, extended with the two-phase defer transition, five operator
verbs (Terminate/Cancel/AdminForceComplete/AdminForceFail), a retry/re-replay pair, and a
fixed two-instance parent-close cascade. `CleatRunLifecycle.cfg`: `Workers = {w1}`,
`NumInstances = 2`, `HeartbeatTimeout = DeferPhaseTimeout = 2`, `MaxClaimBatch = 1`, clock
self-clamping at `ClockCeiling == 5` (no `.cfg` `CONSTRAINT` — see the file's own
`ClockCeiling`/`NextClock`/`FutureClock` comments for why a hard-wall `CONSTRAINT` was tried
first and found to silently defeat liveness checking, the same day, on this same model).
Measured 2026-09-23: 444,729 distinct states, 4,330,355 generated, search depth 14, ~9s.
`TypeOK`, `Safety`, and `SettledIsFinal` (an action invariant) all hold at this bound —
re-derive with `make tla`, not by trusting this paragraph.

`L1_EventualSettlement`, `L2_ClaimProgress` and `L3_CascadeProgress` are **defined but
deliberately NOT gated** in `CleatRunLifecycle.cfg`. Three distinct, real, no-mutation-needed
counter-examples were found for this family of `[](P => <>Q)` property on the clean spec in
one session — an unfair, single-step operator/redrive action (Terminate, Cancel,
AdminReReplay) can establish and then revoke a leads-to antecedent before any
fairness-dependent mechanism gets a qualifying window to react. Two were fixed (L2: WF→SF on
worker-gated actions, plus an explicit `FleetEventuallyStable` assumption; L3: an honest "or
the antecedent itself later became false" escape clause matching L2's). The pattern did not
converge after three fixes — each one closed a specific trace, not the general hazard — so
gating stopped there rather than continuing indefinitely; see the file's own header comment
above `L1_EventualSettlement` for the full account. L1 has not been separately confirmed to
have or lack the same class of issue.

`CleatClaim.tla` used a full-width `======` rule as a decorative section separator, and
several bare-word section headers (`VARIABLES`, `ACTIONS`, ...) between `\* ===` comment
lines with the `\*` left off the header line itself. In TLA+, `======` is the module
*terminator* and a bare `VARIABLES`/`CONSTANT` is a second declaration keyword — both are
real syntax, so SANY does not treat either as decoration. The first `======` sat at line 53
of 495, so roughly 89% of the file was outside the module and would have been silently
ignored by any tool that got that far; it never did, because the bare `VARIABLES` header
aborts parsing first. That the file survived in this state is itself evidence it was never
run through SANY before now.

TLC then found what SANY cannot: `NULL == CHOOSE x : x \notin Workers` is an unbounded
CHOOSE (there is no finite bounding set), which TLC refuses to evaluate. `NULL` is now a
`CONSTANT`, bound to a model value in `CleatClaim.cfg`, with `ASSUME NULL \notin Workers`
in the module replacing the derivation. And `|S|` (cardinality) is not TLA+ syntax; it
parses as two separate `|` (logical-or-like) tokens around a bare `S`, which is what SANY's
second parse error actually was. Fixed to `Cardinality(S)`, from `FiniteSets`, already
extended by the module.

## Bounds and state count

`CleatClaim.cfg`: `Workers = {w1, w2}`, `NumInstances = 2`, `HeartbeatInterval = 1`,
`HeartbeatTimeout = 2`, `MaxClaimBatch = 1`, clock bounded by `ClockBound == clock < 8`.
Measured 2026-09-23: 17,436 distinct states, 130,501 states generated, search depth 8,
finished in ~3.6s. `TypeOK`, `Safety`, `ClaimProgress`, `ReapProgress`,
`TerminalStableLiveness` and `NoStarvation` all hold at this bound — re-derive with `make
tla`, not by trusting this paragraph, since (per CLAUDE.md) a number like this is a snapshot,
not a promise.

The originally-suggested bounds (3 workers, 5 instances, `clock < 30`) are not a smaller
version of the same check — measured before landing on the above, that configuration passed
1.9M distinct states and was still growing past two minutes. A `CONSTRAINT` on `clock` alone
does not bound `heartbeatAt`/`nextWakeAt`, which range freely beneath it; the state space is
governed by those, not by the clock bound. Keep the bounds small and prefer widening a
specific dimension deliberately over assuming a bound "usually sufficient" without measuring.

## Known drift: the spec models the claim protocol as of 2026-08-02, not as of today

`CleatClaim.tla`'s `Claim`/`Heartbeat`/`Fail`/`Release` actions predate cleat#1965 (holder
validity now follows run state, not a bare `assigned_to` match) and cleat#1917
(`worker_concurrency`, a per-worker cap this model does not represent at all). TLC returning
"no error" above is a true statement about the model as written, not a verification of the
current `HeartbeatBatchFenced`/claim path — see cleat#2008 (cleat/PR#2015) for a recent,
real bug in that exact area that a stale model would not have been positioned to catch
either way, since it never modelled per-execution fencing at all. cleat#2000 (queue
admission, rate tokens and claim fairness, explicitly scoped "before #1917 is built") is the
most likely home for the `worker_concurrency` half of this drift; the #1965 holder-validity
half has no issue of its own yet and is recorded here so it is not lost. Neither is folded
into this PR silently — this paragraph is the disclosure.

## Implementation references are stale in the three unmaintained specs

`CleatClaim.tla`'s header now names its current implementing files (no line numbers) and is
current as of this PR. The three unmaintained specs still cite `internal/host/db.go` and
similar paths from before commit `3eeb74e` (2026-06-01, the `internal/host` → `engine`
move and the `durable → cleat` rename) — do not trust them, and do not spend time fixing
them, since they are superseded rather than being brought current.

## Invariant-to-test mapping

The policy going forward, for `CleatClaim.tla` and for the new models in cleat#1997-#2000:
when TLC reports a counterexample, the fix is not to land directly. First write a
dialect-parameterised Go test that reproduces the counterexample's trace against the real
store (see `engine/heartbeat_batch_fenced_test.go` for the shape — a store-level test with a
`postgres`/`mysql`/`mssql` subtest each), confirm it fails the way the model predicted, then
land the fix with that test alongside it. A TLC counterexample without a corresponding Go
test is a claim about the model, not yet a verified claim about the implementation; CLAUDE.md's
"could this check have disagreed?" question applies here exactly as it does to any other guard.
No counterexample has been found yet on `CleatClaim.tla`'s current bounds, so there is nothing
to map today — this section exists so the next one that surfaces has a documented process to
follow rather than an ad hoc call.

## Adding a model (cleat#1997-#2000)

Per cleat#1996, the treatment is the same as `CleatClaim.tla` got, whether the result is a
new file or eventually retires one of the three unmaintained ones: get it parsing and
checking clean, write a `.cfg` with bounds small enough for `make tla` to stay well under a
minute for that spec, record the bounds and state count here, and extend the CI job's path
filter (and `make tla`'s spec discovery, which is "every `.tla` with a matching `.cfg`") to
pick it up automatically — no per-spec CI wiring should be needed beyond adding the `.cfg`.
Each new model's header must list its actors "from the code, not from memory" (cleat#1996's
own rule) with the grep that found them, re-run whenever the spec is touched.

```sh
# tla2tools.jar is not vendored here; fetched by checksum in CI. Locally:
# https://github.com/tlaplus/tlaplus/releases
java -cp tla2tools.jar tlc2.TLC -config specs/CleatClaim.cfg specs/CleatClaim.tla
```

Tracked in `IMPROVEMENT-PLAN.md` Phase 4, and in cleat#1997-#2000.
