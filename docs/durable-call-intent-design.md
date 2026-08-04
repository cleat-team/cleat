# Design: crash recovery for external calls

Status: **design, not implemented.** Supersedes the approach that was sketched in
`engine/flush.go` (`flushCallIntent` / `completeCallEvent`) and **deleted in Phase A** rather
than wired in — see §2 for why. The detector and the `pendingSentinel` constant were kept:
they are the read half of this design and they work.

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
- `--no-per-step-flush` defeats this entirely — it defers persistence to batch finalization, so
  the intent is not durable before dispatch. Using it with any `WriteAheadIntent` operation must
  be **rejected at startup**, not warned about. (`--synchronous-commit-off` is not a threat: it
  applies only to finalize transactions.)
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
3. **Admin force-resolve.** Supply an outcome for a pending step by hand. This lands on the
   admin API, whose store methods are still stubs — and which has **no tenant ownership check**
   (IMPROVEMENT-PLAN 1.7). That check must exist first; otherwise this ships a cross-tenant
   write primitive.

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
| **B** | Tier 1 idempotency keys | ~1 session | `Caller` interface change |
| **C** | 2.4 crash harness + counting-service fixture | ~1 session | — |
| **D** | Tier 2 intent + schema migration | ~2 sessions | **C** |
| **E** | Tier 3 resolution hook + typed error | ~1 session | D |
| **F** | Admin force-resolve | ~0.5 session | E, **and 1.7's ownership check** |

**Phase A is done.** It removed 101 lines of engine code and the 17 tests that were its only
callers — code that read as a finished durability feature, was cited by 48 test references,
and could not be used. Everything below waits for durable-call correctness to become a
priority.

**Best value when it does: B.** Idempotency keys need no schema change, cost no extra write,
and solve the problem outright wherever the callee supports them. It is the only tier that
makes duplicates *impossible* rather than *visible*.

**Sequencing constraint: do not start D before C.** The reason this defect survived is that
nothing could observe it. Building the fix before the observation is repeating the mistake.

## 9. Open questions

- **Where is policy declared** — at the call site (per-call argument, most precise) or on the
  service registration (less repetition, coarser)? Probably both, with the call site winning.
- **Should `IdempotentKey` be the default** for operations that declare themselves idempotent
  in a plugin manifest? Attractive, but it makes the guarantee depend on a third-party
  declaration.
- **What should a `WriteAheadIntent` workflow do when it cannot resolve?** Fail the workflow,
  or suspend it for operator attention? Suspension is probably right — the information needed
  to decide is outside the system — but it needs a state to suspend into.
- **Does the key belong in the event history?** Deriving it is free and it never changes, so
  storing it is redundant — except for admin tooling, which would otherwise have to recompute
  it to talk to the external service.
