# Design: crash recovery for external calls

Status: **implemented, all six phases.** Superseded the approach that was sketched in
`engine/flush.go` (`flushCallIntent` / `completeCallEvent`), which was **deleted in Phase A**
rather than wired in — see §2 for why.

> **Corrected 2026-09-18 (cleat#1865).** This line read "design, not implemented" for weeks
> after Phase F landed (2026-09-02), and §9's "Open questions" still asked one Phase D–F had
> already answered — found stale when it was cited as current evidence on cleat#1778 and was
> wrong. IMPROVEMENT-PLAN §1.4 (archived to `IMPROVEMENT-PLAN-CLOSED.md`) is the authoritative
> phase-by-phase record; §8 and §9 below are corrected to match it, not merely re-marked.

Idempotency keys (Tier 1), write-ahead intent (Tier 2) and resolution — automatic, then admin
(Tier 3) — are all live and reachable from the shipped worker. The detector and the
`pendingSentinel` constant that Phase A kept are the read half of this design.

Companion to [`durable-calls.md`](durable-calls.md), which describes the contract as it
stands today. IMPROVEMENT-PLAN item 1.4.

---

## 1. The gap

`freshCall` dispatches the external call, then persists the event:

```
caller.Call(...)          ← the side effect happens
recordEvent(rec)          ← the outcome becomes durable
```

A crash between those two lines loses the outcome. On replay the step has no event, so the
call is made **again**. For a non-idempotent operation — charge a card, send an email, post a
ledger entry — that is a duplicated real-world effect, produced silently.

This is documented and it is a defensible default: exactly-once execution across an
unreliable boundary is not achievable in general. What is achievable, and what is missing, is
**making the duplicate visible or unnecessary**.

## 2. Why the existing code could not simply be wired in

> Line references in this section are to the tree **as of `dfccee4`**, before Phase A removed
> the two functions. They are kept because they are the evidence for the design decisions
> below, not because the code is still there.

`flushCallIntent` and `completeCallEvent` implemented write-ahead intent: insert a row marked
pending, dispatch, then update it with the outcome. The read side is live and correct — a
pending row is detected on replay at `engine/durablecalls.go:150` and reported as
`[AMBIGUOUS]`. The write side has never had a caller.

IMPROVEMENT-PLAN 1.4 says the fix is to call it from `freshCall` and that "the detector needs
no changes". Doing that would break every workflow that makes a durable call. Three defects,
none of which can appear until the code has a caller — which is why 48 test references are
all green:

**2.1 — Every completion path refuses to overwrite an intent row.** `insertEventSQL`
(`engine/flush.go:36`) and both adaptive-flush batch inserts carry:

```sql
ON CONFLICT (workflow_id, step) DO UPDATE SET response = EXCLUDED.response, error = EXCLUDED.error
  WHERE event_history.response = '' AND event_history.error IS NULL
```

`flushCallIntent` writes `error = pendingSentinel`, which is not NULL. A completion arriving
through `recordEvent` is therefore a **silent no-op**: no error, no update. The sentinel
persists, and every replay of that workflow reports `[AMBIGUOUS]` forever.

**2.2 — The intent row's checksum does not describe the intent row.** `engine/flush.go:201`
computes the checksum from `rec` (whose `Err` is empty); line 207 then stores `pendingSentinel`
in the error column. Replay verifies checksums *before* the guest runs
(`engine/executor.go:277`), so in the exact crash window this feature exists to handle, the
workflow fails checksum verification instead of reporting the ambiguity.

**2.3 — The checksum chain is read from the database.** Both functions `SELECT` step−1's
checksum rather than using `s.lastChecksum`. Under the adaptive flusher step−1 may not be
persisted yet, so the previous checksum reads as empty and the chain diverges from what
`recordEvent` computes.

Beyond the defects, the approach has three design problems:

- **It doubles writes on the hottest path.** Two transactions per call instead of one. The
  adaptive flusher exists because per-step flushing was a throughput problem; an unconditional
  intent write pushes directly against that.
- **The sentinel overloads the `error` column** with a control value. That overloading is the
  direct cause of 2.1.
- **It has no resolution path.** Reporting `[AMBIGUOUS]` converts a rare silent duplicate into
  a rare permanent failure. For some workloads that is worse. A workflow that learns its
  outcome is unknown, and has no way to find out, is stuck.

The 350 lines are not a head start. They are three latent bugs and a design that fights the
storage layer.

## 3. Principle

**The right guarantee depends on the operation, and only the workflow author knows which one
applies.** A GET is safe to repeat. A card charge is not. A card charge *with an idempotency
key* is. One global policy cannot be correct for all three, and the cost profiles differ by an
order of magnitude.

So: per-operation policy, with a default that matches today's documented behaviour.

```go
type CallSemantics int

const (
    AtLeastOnce   CallSemantics = iota // default, today's behaviour, no extra cost
    IdempotentKey                      // engine supplies a stable key; service dedupes
    WriteAheadIntent                   // durable intent; ambiguity surfaced and resolvable
)
```

Declared per call site, or per (service, operation) at registration. The default is what ships
today, so nothing changes for existing workflows and nobody pays for a guarantee they did not
ask for.

## 4. Tier 1 — idempotency keys (primary mechanism)

For any service that can deduplicate, this solves the problem outright and costs **no extra
database write**.

The engine derives a key that is stable across replays:

```
key = base32(sha256(workflowID || "\x00" || runID || "\x00" || step))
```

`step` is deterministic on replay, so the key is identical on every attempt of the same
logical call. `runID` is included so that `ContinueAsNew` — genuinely new work — gets fresh
keys rather than colliding with the previous run.

The key is passed to the callee (`Idempotency-Key` header for HTTP, an explicit parameter for
plugins). After a crash, replay re-issues the call with the same key and the service returns
the original outcome. No ambiguity, no extra round trip, no schema change.

**Cost:** the `Caller` interface gains a key parameter, which is a breaking change for
external callers and plugin authors. That is the main expense of this tier and the reason it
is worth doing on its own rather than bundled.

**Limit:** it only works where the service honours keys. Everything else needs Tier 2.

## 5. Tier 2 — write-ahead intent, done properly

For non-idempotent operations against services that cannot dedupe.

### Schema

Add a dedicated column rather than overloading `error`:

```sql
ALTER TABLE event_history ADD COLUMN intent_at TIMESTAMPTZ NULL;
```

An event is *pending* iff `intent_at IS NOT NULL AND checksum IS NULL`. `error` goes back to
meaning only "the call failed", which removes the cause of 2.1.

`pendingSentinel` is deleted, not migrated. **Nothing has ever written it**, so no row in any
deployment carries it — a rare case where a format change has zero migration burden. Keep the
detector, retargeted at `intent_at`.

### Flow

1. INSERT the event with `intent_at = now()`, `checksum = NULL`, response/error unset.
   Synchronous, own transaction, committed before dispatch. Not batchable — durability before
   the side effect is the entire point.
2. Dispatch the call.
3. UPDATE the row: set response/error, `intent_at = NULL`, and the checksum computed over the
   final record chained from `s.lastChecksum`. Guard on `WHERE intent_at IS NOT NULL AND
   checksum IS NULL` and assert exactly one row changed — `completeCallEvent` already does
   this, and it is the one part worth keeping.

Checksums are `NULL` for pending rows and verification skips them, since a pending row is by
definition incomplete. That removes 2.2 by construction. Chaining from `s.lastChecksum` rather
than a DB read removes 2.3.

### Interaction with the flush machinery

- Intent-mode steps must not also flow through `recordEvent`'s flush. Branch in `freshCall`,
  and assert it: a step that reaches both paths is a bug that would otherwise be silent.
- ~~`--no-per-step-flush` defeats this entirely — it defers persistence to batch finalization, so
  the intent is not durable before dispatch. Using it with any `WriteAheadIntent` operation must
  be **rejected at startup**, not warned about.~~ **Wrong, and checked rather than assumed
  (2026-08-05).** That is true of an implementation that routes the intent through `flushEvent`,
  which is what this paragraph assumed. The implementation writes through the store's own
  `WriteCallIntent`, which never consults `e.noPerStepFlush`, so the two settings are
  orthogonal. `TestDurableCall_IntentSurvivesNoPerStepFlush` asserts it on all three dialects,
  and if it ever fails, the startup rejection described here is the fix. No startup check was
  added: forbidding a combination that works is a cost with no benefit.
  (`--synchronous-commit-off` is not a threat: it applies only to finalize transactions.)
- The adaptive flusher may batch completions but never intents. Simplest v1: keep completions
  synchronous too, measure, and only add batching if the numbers demand it.

## 6. Tier 3 — making ambiguity resolvable

Detection alone leaves the workflow stuck. Three exits, in order of preference:

1. **Automatic resolution.** An optional per-operation hook: given the idempotency key, ask the
   service whether that operation completed and with what result. If it answers, the engine
   completes the event and replay proceeds normally. Most ambiguities become non-events.
2. **Typed error to the guest.** `ErrAmbiguous` already exists as error code 5. Today the
   detail arrives as a formatted string *inside the workflow result*, so tooling has to parse
   prose. It should be a structured value carrying step, service, operation and key.
3. **Admin force-resolve.** Supply an outcome for a pending step by hand. **Done, phase F,
   2026-09-02**: `engine.ResolveStep` (`engine/admin_intent.go`), reachable at
   `POST /api/admin/instances/{id}/steps/{step}/resolve`. It builds on phase E's
   `ResolveCallIntent`, needed no new SQL, and the outcome is written as though the call had
   returned it — `EventRecord.ResolvedBy` records separately that it was asserted rather than
   observed. Resolving an already-resolved step is a conflict (409), not a silent overwrite.
   §1.7's tenant ownership check landed first, as this phase's own dependency required.

### Telemetry

Without these, there is no way to know the feature does anything: `intent_written`,
`ambiguous_detected`, `ambiguous_resolved{auto,admin}`, `duplicate_suppressed_by_key`.

## 7. Test plan

This feature's entire history is of code that passes tests without running. The test plan is
therefore part of the design, not a follow-up.

**The crash harness comes first.** None of Tier 2 can be validated without one, and building
the feature before the harness is how the current situation arose. This is IMPROVEMENT-PLAN
2.4: `SIGKILL` a worker mid-call and restart it — a real signal to a real process, not a
simulated error return.

The fixture is a **counting service** that records how many times each operation was actually
invoked. Every assertion below is about that count, because the count is the thing the user
cares about.

| # | Scenario | Assertion |
|---|---|---|
| T1 | Crash mid-call, `AtLeastOnce` | count == 2. Pins the documented default rather than leaving it implied. |
| T2 | Crash mid-call, `IdempotentKey` | same key on both attempts; the service suppresses the duplicate; workflow completes normally. |
| T3 | Crash mid-call, `WriteAheadIntent` | replay reports `ErrAmbiguous` for that step, and **checksum verification passes** — the case 2.2 breaks. |
| T4 | T3 + a resolver hook | ambiguity resolved automatically; workflow completes; count == 1. |
| T5 | Intent durability | caller blocks; assert the intent row is visible from a second connection *before* the call returns. |
| T6 | Flusher matrix | T3 with the adaptive flusher on and off. |
| T7 | `--no-per-step-flush` + intent op | worker refuses to start, naming the conflict. |
| T8 | Replay determinism | keys stable across replays; `ContinueAsNew` yields fresh keys. |

**Non-vacuity is required for each**, per the standing rule: remove the mechanism, re-run, and
record the failure message. T1 is what T2/T3 must *stop* happening — if T3 passes with the
intent write removed, it is measuring something else.

## 8. Phasing, cost, and what to do now

| Phase | Work | Effort | Depends on |
|---|---|---|---|
| ~~**A**~~ | ~~Delete `flushCallIntent`/`completeCallEvent`; keep the detector; correct `durable-calls.md`; drop the baseline entries~~ ✅ **done** | — | — |
| ~~**B**~~ | ~~Tier 1 idempotency keys~~ ✅ **done** — `DurableCallIdempotencyKey` (`engine/idempotency.go`), an optional `IdempotentCaller` rather than a breaking `ServiceCaller` change, `dbServiceCaller` implements it and sends `Idempotency-Key` | ~1 session | `Caller` interface change |
| ~~**C**~~ | ~~2.4 crash harness + counting-service fixture~~ ✅ **done** — `tests/crash`, wired into the cluster job | ~1 session | — |
| ~~**D**~~ | ~~Tier 2 intent + schema migration~~ ✅ **done 2026-08-05** — migration `020` on three dialects, `WriteCallIntent`/`CompleteCallIntent`, `CallSemantics` + `WithWriteAheadIntentOps`, the `freshCall` branch, the detector retargeted off `pendingSentinel`, and `--write-ahead-intent-ops` on the worker | — | — |
| ~~**E**~~ | ~~Tier 3 resolution hook~~ ✅ **done 2026-08-05** — `AmbiguityResolver`, `ResolveCallIntent` on all three dialects, resolution persisted before replay continues. **Typed error still open** — see §6, item 2 | ~1 session | D |
| ~~**F**~~ | ~~Admin force-resolve~~ ✅ **done 2026-09-02** — `ResolveStep`, `POST /api/admin/instances/{id}/steps/{step}/resolve` | ~0.5 session | E, **and 1.7's ownership check** |

**Phase A was the fix that let everything else be trusted.** It removed 101 lines of engine
code and the 17 tests that were its only callers — code that read as a finished durability
feature, was cited by 48 test references, and could not be used.

**B shipped the best-value tier first, as planned.** Idempotency keys need no schema change,
cost no extra write, and solve the problem outright wherever the callee supports them — the
only tier that makes duplicates *impossible* rather than merely *visible*.

**The sequencing constraint held: D did not start before C.** The reason the original defect
survived was that nothing could observe it; §1.4's own history in IMPROVEMENT-PLAN found a
second instance of exactly that shape while building C (event writes silently dropped by
RLS, fixed first — see the archived entry) before D could have measured anything real.

All six phases are done. IMPROVEMENT-PLAN §1.4, archived to `IMPROVEMENT-PLAN-CLOSED.md`, is
the phase-by-phase record with tests and measurements; this section states only the shape.

## 9. Open questions, and what became of each

All four were open when this section was written. None still is; each was settled by
implementation rather than by revisiting this document, which is exactly how a section like
this goes stale silently — **corrected 2026-09-18, cleat#1865**, after one of the four was
cited as still-open evidence on cleat#1778 and was wrong. Re-check a list like this against
the tree before trusting it, the same rule CLAUDE.md gives for any other status claim.

- **Where is policy declared?** **Decided: service registration, not the call site.**
  `WithWriteAheadIntentOps` (`engine/callintent.go`) takes `service.operation` pairs, fed by
  the worker's `--write-ahead-intent-ops` flag. The call-site form this question also
  considered was not built — it would need a new argument on the `DurableCall` host function
  and every SDK that binds it, an ABI change that registration avoids and forecloses nothing
  about adding later.
- **Should `IdempotentKey` be the default for manifest-declared idempotent operations?**
  **Decided: no.** `HostFuncDef.Idempotent` (`plugin/manifest.go`) is declarative only and
  does not reach a registration's `FuncOptions` — a manifest author cannot set it, by design;
  see that field's own doc comment for why the natural assumption is the wrong one. Idempotency
  keys are opt-in per operation through `WithWriteAheadIntentOps`/`IdempotentCaller`, never
  inferred from a manifest.
- **What should a `WriteAheadIntent` workflow do when it cannot resolve?** **Built: suspend,
  into a real state.** `engine.ResolveStep` (`engine/admin_intent.go`) records an operator's
  answer for a step a crash left pending; replay then reads `EventRecord.ResolvedBy` instead
  of re-calling. `engine.ReReplay` returns the workflow to `ready`, keeping its history. Both
  are reachable over HTTP at `cmd/cleat-worker/api_admin.go`. Automatic resolution (an
  `AmbiguityResolver` hook, tried first, before any human is involved) shipped the same day.
- **Does the key belong in the event history?** **Decided: no, and this one held.**
  `EventRecord` (`engine/types.go`) carries no idempotency-key field. The key is re-derived
  wherever it is needed — including by `ResolveStep`
  (`engine/callintent.go:289`, `DurableCallIdempotencyKey(workflowID, runID, step)`) — because
  deriving it is free and it never changes, exactly the reasoning this question started from.
