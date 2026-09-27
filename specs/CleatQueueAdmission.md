<!-- tla-index: issue=2000 -->
# CleatQueueAdmission.tla — status

## Bounds and state count

**SUPERSEDED 2026-09-23 — the clock-bounded version below was silently unsound for all three
liveness properties, and the paragraph is kept to show what changed and why.** A cross-session
report (cleat#2034) found the sibling `CleatClaim.tla`'s `ClaimProgress` passing "No error
found" at an identical state count whether or not its `WF(Claim(w))` fairness clause was even
present — the state CONSTRAINT TLC uses to bound the clock excludes every real successor state
at its boundary, leaving only the always-enabled stuttering step, under which every `WF_vars`
condition is vacuously satisfied regardless of what it names (TLC's own startup warning points
at *Specifying Systems* section 14.3.5 for exactly this). Re-running the same known-positive
against `CleatQueueAdmission.tla`'s clock-bounded first version, before it ever merged,
reproduced the identical failure on all three of `S2`, `L1` and `L2` — dropping each one's
fairness clause still reported "No error found" at the same state count as the real run. See
`CleatQueueAdmission.tla`'s own "Why there is no clock variable" comment for the mechanism.

The fix replaces the monotonic `clock` and absolute `rateTokenExpiresAt : [Runs -> Nat]` with a
countdown, `rateTokenRemaining : [Runs -> 0..RatePeriod]`, decremented once per step and reset
to `RatePeriod` on a fresh admission. That domain is finite by construction from the CONSTANTS
alone, so no external CONSTRAINT is needed and liveness checking is sound without one.

`CleatQueueAdmission.cfg`: `NumTenants = 2`, `RunsPerTenant = 2`, `Workers = {w1}`,
`ConcurrencyLimit = 2`, `WorkerCap = 1`, `RateLimit = 1`, `RatePeriod = 1`, no `CONSTRAINT`.
Measured 2026-09-23: 60,427 distinct states, 609,060 states generated, search depth 10,
finished in ~53s. `TypeOK`, `RunningRunsHoldSlots`, `S1`, `S3`, `S2`, `L1` and `L2` all hold at
this bound — re-derive with `make tla`, not by trusting this paragraph.

**Only one worker in the shipped bound, and that is a deliberate, disclosed tradeoff, not an
oversight.** The same CONSTANTS with `Workers = {w1, w2}` were tried first, matching the
original (unsound) config; with the CONSTRAINT gone, that is the TRUE reachable space rather
than the truncated one the clock bound was silently hiding, and it exceeded 300,000 distinct
states and was still climbing after four minutes — the same "still growing" failure mode
`CleatClaim.cfg`'s own history records above, and the killed run was itself instructive: it is
a direct measurement of how much of the real state space the unsound `ClockBound` had been
excluding from every prior run of this spec. `RunsPerTenant` could not be dropped to compensate
(§ below explains why: at `RunsPerTenant = 1`, `L2`'s "a ready run stays blocked" antecedent is
never reached at all, which is the same vacuity this whole rewrite exists to eliminate, reached
by a different door), so the two-worker dimension was dropped instead. `WorkerCap = 1 <
ConcurrencyLimit = 2` still makes the per-worker cap the binding constraint on this bound, so
the gate is genuinely exercised; cross-worker admission distribution is exercised instead by
the `S1` known-positive below, which uses two workers deliberately because invariant violations
are found early rather than by exhausting the graph.

Bounds were found by growing from a much smaller, fast-measured config (`NumTenants = 2,
RunsPerTenant = 1, Workers = {w1}`, everything else at 1: 2,128 distinct states, ~1s) one
dimension at a time, confirming each addition's cost before combining it with the next, rather
than editing the two-worker killed config down.

**`RunsPerTenant = 1` is not merely a smaller bound — it makes `L2` vacuous, and that is why the
shipped config keeps `RunsPerTenant = 2` even at the cost above.** With one run per tenant, that
run can never be blocked by `CanAdmit`: nothing else exists to hold the slot ahead of it, so it
is admitted the moment the rotation visits its tenant. `L2`'s "ready run eventually leaves
ready" would then hold for every reachable state without `CanAdmit`'s gate ever having refused
anything — the same shape of vacuity as the clock-bound defect, reached through an empty
antecedent rather than a false CONSTRAINT. `RunsPerTenant = 2` with `WorkerCap = 1` guarantees a
genuine blocked-then-admitted trace: the second run of a tenant is held out by the first's live
holder until it settles.

**Known-positive, not just a clean run — now covering all three liveness properties, not only
the one safety invariant this section originally reported.** Three separate mutations, each
against the fixed spec and the bound above:

| mutation | verdict | distinct states | contrast with the pre-fix run |
|---|---|---|---|
| `ConcurrencyLimit` conjunct deleted from `CanAdmit` (config widened to `RunsPerTenant = 3, ConcurrencyLimit = 2, WorkerCap = 2`, so the per-worker cap alone cannot coincidentally re-impose the same bound: 2 workers × cap 2 == 4 > limit 2) | genuine counterexample, depth 6 — three live holders on one tenant's queue against a declared limit of two | 9,549 | (unaffected by the clock-vs-countdown change; `S1` is a safety invariant, not a liveness property) |
| `WF_vars(Claim(w))` deleted from `Fairness` | genuine counterexample — a **one-state stuttering trace from Init**: no worker ever claims anything, violating `L1` and `L2` immediately | 13,755 (early exit; the baseline's 60,427 is not reached because TLC stops at the first violation) | pre-fix, the identical mutation reported "No error found" at the *same* state count as the fair run |
| `WF_vars(Reap)` deleted from `Fairness` | genuine counterexample — `SettleWithoutRelease` strands a holder at step 4, then the trace loops through further admissions and settles that never call `Reap` again, violating `S2` | 60,427 (identical to the fair run — expected: removing fairness changes which infinite paths are ACCEPTED, not which states are REACHABLE) | pre-fix, the identical mutation reported "No error found" at the *same* state count as the fair run |

The middle and bottom rows are the ones this rewrite exists for: before the fix, dropping
either fairness clause left the verdict AND the state count unchanged from the fair run — the
textbook vacuity signature this file's own CLAUDE.md warns about under "could this check have
disagreed?". After the fix, both now report a genuine violation, with a real counterexample
trace TLC can print. CLAUDE.md's own rule: a check that has never been shown capable of failing
is a claim, not a verification — and for a liveness property specifically, "capable of failing"
means capable of failing *when its own fairness is withdrawn*, not just when a safety invariant
is weakened.

## What `CleatQueueAdmission.tla` covers, and what it deliberately doesn't

Read the spec's own header first — it is longer and more precise than this section, and this
section will rot faster (CLAUDE.md's own rule). In short: it models a REGISTERED queue's
admission gate (concurrency limit, cleat#1917's per-worker cap, cleat#1918's rate limiter,
all three composed under one queues-row lock) and the per-worker rotation that spreads claims
across tenants. It does NOT model the bare-key mutex (`concurrency_keys`, no registered
queue) — the now-deleted `CleatConcurrencyKeys.tla` already argued that case is structurally
trivial (a primary key gives mutual exclusion) and nothing built since changes that argument,
which is why this file supersedes and retires it rather than extending it. It also does not
model a holder MOVING to a different worker when a parked run wakes and is claimed elsewhere
(cleat#1917 decision 4), the `claimedKeyTTL` time-based reap backstop (unreachable by any
state this model can produce — see the header), or the retention-driven cascade delete of a
settled run's row (subsumed by the run-state reap disjunct, which already fires before any
retention sweep could run). None of these omissions are silent: each is named and argued in
the spec's own header comment, not just here.
