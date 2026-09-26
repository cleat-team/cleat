<!-- tla-index: issue=1996 -->
# CleatClaim.tla — status

**cleat#2041, resolved 2026-09-25.** The `NoStarvation` gap this file below describes as needing
"per-instance fairness" was fixed — and **not the way that paragraph predicted**. The claimant does
not choose which instance to take: `engine/store_lifecycle.go` is `ORDER BY w.priority ASC,
w.created_at LIMIT $2 FOR UPDATE SKIP LOCKED`, a total order whose head is taken, so there was never
a per-instance disjunct for fairness to be about. `Claim` now takes the head (`FrontBatch`) instead
of an arbitrary `SUBSET`, `priority` is modelled (`HighPriorityInstances`), and work enters the queue
through a new **`Arrive`** action — without which `priority` is inert, because a fixed instance set
is ordered exactly once and no priority value can starve anything.

**What is gated now is a SAFETY property, not `NoStarvation`.** `ClaimRespectsOrder` is an action
invariant: a claim takes a PREFIX of `Precedes`, so nothing is ever overtaken. It is checked during
ordinary reachability rather than by the liveness engine, it depends on nothing outside the model,
and it goes red when `Claim` stops taking a prefix — verified by mutating the batch to claim only the
newest ready instance, which fails it at 24 distinct states. That is the property the defect was
actually about, and unlike the liveness one it fails when the thing it names is broken.

**`NoStarvation` remains defined and ungated, but for a NEW reason.** It passes now — through the
**unclamped `FutureClock`**, not through the claim protocol: with the clamp restored and everything
else unchanged it FAILS at 3,349 distinct states. And the starvation the engine actually has — a run
passed over indefinitely by *sustained* higher-priority arrivals, measured against a live store in
`engine/the_claim_order_is_priority_then_age_test.go` — is not representable at any finite
`NumInstances`: arrivals stop, the order settles, the queue drains. Unreachable here, which is not
the same as disproved. Full reasoning is on the definition in `CleatClaim.tla` and in the `.cfg`.

---

**cleat#2034, filed 2026-09-23, FIXED the same day.** `CleatClaim.tla`'s own `ClaimProgress`
was silently vacuous — deleting `WF_vars(Claim(w))` from `Fairness` left "No error has been
found" at an identical state count to the unmutated run, the same CONSTRAINT-boundary hazard
`CleatRunLifecycle.tla` and `CleatQueueAdmission.tla` both hit. Fixed with the self-clamping
`ClockCeiling`/`NextClock`/`FutureClock` pattern `CleatRunLifecycle.tla` established (not
`CleatQueueAdmission.tla`'s clock-elimination approach — this file's clock feeds two
deadline-shaped quantities read by comparison, not one independent per-run countdown), plus
the same `WF`→`SF` fairness upgrade and `FleetEventuallyStable` assumption
`CleatRunLifecycle.tla` needed for its own `L2`. Fixing the vacuity then surfaced two further
*genuine*, non-vacuous counter-examples on the clean spec, both the same "revocable antecedent"
and "disjunctive fairness isn't per-entity fairness" shapes `CleatRunLifecycle.tla`'s own
`L2`/`L3` document: `ReapProgress` needed the identical "or the antecedent itself later became
false" escape (fixed, now gated and passing); `NoStarvation` could not be repaired the same
way — its antecedent is never revoked by anything in the trace that stalls it — and is now
**defined but deliberately NOT gated**, matching `CleatRunLifecycle.tla`'s own precedent for
`L1`/`L2`/`L3`. See "Bounds and state count" below for the measurements and known-positive
table, and `CleatClaim.tla`'s own comments above `Fairness`, `ReapProgress` and `NoStarvation`
for the full account.

## Bounds and state count

`CleatClaim.cfg`: `Workers = {w1, w2}`, `NumInstances = 2`, `HeartbeatInterval = 1`,
`HeartbeatTimeout = 2`, `MaxClaimBatch = 1`, `HighPriorityInstances = {}` (all equal priority),
clock self-clamping at `ClockCeiling == 5` (no `.cfg` `CONSTRAINT` — see below and the file's own
`ClockCeiling`/`NextClock`/`FutureClock` comments). **Re-measured 2026-09-25 after cleat#2041:
2,483 distinct states, 15,233 states generated**, ~2s. `TypeOK`, `Safety`, `ClaimProgress`,
`ClaimRespectsOrder`, `ReapProgress` and `TerminalStableLiveness` all hold at this bound;
`NoStarvation` is defined but deliberately not gated (see below) — re-derive with `make tla`, not by
trusting this paragraph, since (per CLAUDE.md) a number like this is a snapshot, not a promise.

**Note the direction the count moved: 8,738 → 2,483, DOWN.** cleat#2041 replaced `Claim`'s
arbitrary-subset choice with a deterministic prefix, which removes branching rather than adding it;
`Arrive` adds some back. A smaller number here is not a weaker check — it is a less nondeterministic
model of the same system, and the property that matters is now an invariant over it.

Unlike `CleatQueueAdmission.tla`'s clock-elimination fix, this bound did **not** need to be
re-derived by deliberate incremental growth after removing the CONSTRAINT: a self-clamping
clock is finite by construction (`0..ClockCeiling`), so the reachable graph was never subject
to the unsound truncation a `.cfg` `CONSTRAINT` performs, and the state count above is
essentially unchanged in shape from before the fix — what changed is that the count is now
known to be the TRUE graph rather than a possibly-truncated one, and every liveness verdict
computed over it is now sound.

**SUPERSEDED 2026-09-23 — the clock-bounded version below was silently unsound for all four
liveness properties, and the paragraph is kept to show what changed and why.** `CleatClaim.cfg`
previously read `Workers = {w1, w2}`, `NumInstances = 2`, ..., clock bounded by `ClockBound ==
clock < 8`, measuring 17,436 distinct states, 130,501 states generated, search depth 8, ~3.6s,
with all of `TypeOK`, `Safety`, `ClaimProgress`, `ReapProgress`, `TerminalStableLiveness` and
`NoStarvation` reported as holding. cleat#2034 found that "holding" was not a fact about the
model: the state CONSTRAINT TLC uses to bound `clock` excludes every real successor state at
its boundary, leaving only the always-enabled stuttering step, under which every `WF_vars`
condition is vacuously satisfied regardless of what it names (TLC's own startup warning points
at *Specifying Systems* section 14.3.5 for exactly this) — the identical mechanism found first
in `CleatQueueAdmission.tla`'s pre-merge version and fixed second here, third (chronologically
first, in wall-clock terms) in `CleatRunLifecycle.tla`. See `CleatClaim.tla`'s own comment
above `ClockCeiling` for the fix and why self-clamping (`CleatRunLifecycle.tla`'s approach)
fits this file better than `CleatQueueAdmission.tla`'s clock-elimination.

The originally-suggested bounds (3 workers, 5 instances, `clock < 30`) were never viable either
way — measured against the pre-fix CONSTRAINT-bounded spec, that configuration passed 1.9M
distinct states and was still growing past two minutes. A `CONSTRAINT` on `clock` alone does
not bound `heartbeatAt`/`nextWakeAt`, which range freely beneath it; the state space is
governed by those, not by the clock bound. Keep the bounds small and prefer widening a
specific dimension deliberately over assuming a bound "usually sufficient" without measuring.

**Fixing the vacuity surfaced two further genuine (non-vacuous) counter-examples on the clean
spec, and they did not converge the same way — one was fixed, one was not.** `ReapProgress`'s
original consequent (`<>(status[i] = "ready")`) was too strong — a worker can crash, restart,
and directly resolve its own claimed instance without `Reap` ever firing, which is legitimate
behaviour. Broadened once to `<>(status[i] /= "running")`, which still failed: a worker can
crash for exactly one step and restart before either `Reap` or its own management actions get
a continuously/infinitely-often-enabled window to react, then spend forever managing a
*different* instance, satisfying its own disjunctive `SF_vars(...)` clause while the first
instance it briefly orphaned sits untouched. This is the identical "revocable antecedent" shape
`CleatRunLifecycle.tla`'s own `L2_ClaimProgress`/`L3_CascadeProgress` document, fixed the same
way: an honest `\/ <>(~antecedent)` escape — the worker coming back alive before anything acts
is the antecedent ceasing to hold, not starvation. Re-run clean afterward (see the
known-positive table below).

**SUPERSEDED 2026-09-25 by cleat#2041 — kept to show what the finding was and how it resolved.**
The paragraph below describes its counter-example correctly, and "needs per-instance fairness, which
is a real model change" was the right call at the time. What it could not know is that the fairness
was never the fix: the claimant does not choose, so there is no per-instance disjunct to be fair
about, and the property that replaced this one — `ClaimRespectsOrder` — is a safety invariant rather
than a liveness property. See the top of this file.

`NoStarvation` hit the same "disjunctive fairness is not per-entity fairness" root cause but
could **not** be repaired the same way: a sole surviving worker under `MaxClaimBatch = 1` can
nondeterministically always choose the same ready instance to reclaim (Claim it, Release it,
Claim it again, forever), which alone satisfies `SF_vars(Claim(w))` while a sibling ready
instance — whose antecedent is never revoked, since the worker never dies and the instance
never stops being ready past its wake time — starves for the entire infinite trace. No escape
clause repairs this, because there is no point where the antecedent becomes false to escape
through; genuinely fixing it needs per-instance fairness (a real model change), which is out of
cleat#2034's scope — tracked separately as **cleat#2041**. `NoStarvation` is left **defined but
deliberately not gated** in `CleatClaim.cfg`, matching `CleatRunLifecycle.tla`'s own precedent
for `L1`/`L2`/`L3` — shipping it gated would have been a false claim. See `CleatClaim.tla`'s own
comment above `NoStarvation` for the full trace.

**Known-positive, re-run against the fixed spec — three properties, three mutations, and the
state-count check that matters: does removing fairness change the VERDICT without changing the
GRAPH.**

| mutation | verdict | distinct states / generated | contrast with the pre-fix run |
|---|---|---|---|
| `SF_vars(Claim(w))` deleted from `Fairness` | genuine counterexample — `ClaimProgress` fails | **15,233 / 2,483 (identical to the current baseline)** — re-run 2026-09-25 after cleat#2041, which changed the baseline from 8,738 / 59,229 | pre-fix, the identical mutation (then `WF_vars(Claim(w))`) reported "No error found" at the *same* state count as the fair run (87,180 / 130,501, on the then-current bound) |
| `WF_vars(Reap)` deleted from `Fairness` | genuine counterexample — `ReapProgress` fails | **15,233 / 2,483 (identical to the current baseline)** — re-run 2026-09-25, same note | not separately measured pre-fix; the mechanism is the same as the row above |
| Terminal-refusal guard deleted from `Fail(w)` (can now flip any instance, including an already-terminal one) | genuine counterexample — `TerminalStableLiveness` (an action invariant) fails immediately, `Error: Action property TerminalStableLiveness is violated.` | 85 / 143 (early exit — an action invariant halts the search at first violation, unlike a liveness property, which must explore the complete graph before it can report a leads-to failure) | this property is checked during ordinary reachability, not exposed to the liveness engine at all, so it was never subject to the CONSTRAINT-clamping vacuity in the first place — the mutation exists only to confirm the guard being tested is real, not to compare against a pre-fix number |

| `Claim`'s batch mutated from `FrontBatch` to a non-prefix — `LET S == {i \in ReadyInstances : i = NumInstances}`, i.e. claim only the NEWEST ready instance | genuine counterexample — `ClaimRespectsOrder` (an action invariant) fails at once: `Error: Action property ClaimRespectsOrder is violated.` | 24 / 57 (early exit, same reason as the `TerminalStableLiveness` row) | none needed — this property is new in cleat#2041, so there is no pre-fix number to compare against. What it demonstrates is that the gate is not free: it goes red when `Claim` stops taking a prefix |

The first two rows are the direct analogue of `CleatQueueAdmission.tla`'s own known-positive
table below: identical state count, flipped verdict, is the actual evidence a fairness clause
is doing real work rather than being checked against an already-truncated graph. The fourth row is
a different use of the same discipline — not a fairness clause proving itself, but a new GATE
proving it can fail, which is the only thing that makes gating it worth anything.

## Known drift: `CleatClaim.tla` models the claim protocol as of 2026-08-02, not as of today

`CleatClaim.tla`'s `Claim`/`Heartbeat`/`Fail`/`Release` actions predate cleat#1965 (holder
validity now follows run state, not a bare `assigned_to` match) and cleat#1917
(`worker_concurrency`, a per-worker cap this model does not represent at all). TLC returning
"no error" above is a true statement about the model as written, not a verification of the
current `HeartbeatBatchFenced`/claim path — see cleat#2008 (cleat/PR#2015) for a recent,
real bug in that exact area that a stale model would not have been positioned to catch
either way, since it never modelled per-execution fencing at all.

`CleatQueueAdmission.tla` (cleat#2000) is now where the `worker_concurrency` (#1917) and
run-state holder-validity (#1965) halves of this drift are modelled — see its own header and
"What `CleatQueueAdmission.tla` covers" below. This closes the drift for the QUEUE path.
**It does not touch `CleatClaim.tla` itself**, whose own `Claim`/`Heartbeat`/`Fail`/`Release`
actions model the bare-key/no-queue claim and still predate both #1965 and #1917 exactly as
this paragraph originally recorded — that half of the drift is not folded in here, and remains
open.
