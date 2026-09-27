<!-- tla-index: issue=1997 -->
# CleatRunLifecycle.tla — status

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
