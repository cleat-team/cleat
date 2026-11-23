# One place for per-step replay logic: a step dispatcher

**Status: DRAFT, revised 2026-11-19 after WS-cleat-2's review (see "Revision 1" at the end).** Author: WS-cleat-3. Reviewer: WS-cleat-2 (this touches defer segments, and the
residency design in #4162 depends on it). Gate: WS-cleat-1's fault-injection harness (#4169). **Nothing here is built, and
nothing is built until the harness exists and this design is agreed.**

Owner decision (2026-11-19, via the coordinator): before 0.6.0, move the per-step replay logic into one place, because the
bugs of the last weeks keep coming from every host call doing it separately.

This is a design about *where code lives*, not about behaviour. Section 7 says how the ABI, WIT, history format and database
schema stay unchanged. Section 6 is the migration, in small PRs.

Every number below carries the command that re-derives it, and a date. Read the two derivations in section 2 as a pair: they
are meant to disagree about detail and agree about order of magnitude.

---

## 1. The problem, as the bugs saw it

A *step* is one host call the guest makes. For each one the engine has to answer five questions, and each host call answers
them itself:

| # | question | where it is answered today |
|---|---|---|
| 1 | Is this step **replayed** from history, or **fresh**? When does replay end? | `s.isReplay` plus `exitReplay()` at the end of a replay block, in every host call |
| 2 | Is the recorded event **this call's** event? If not, report a divergence. | an `if rec.EventType != ...` ladder in every host call, each with its own message |
| 3 | Is this call **refused** (a defer segment's body, or a shutdown)? | `stopBeforeNewWork()` in the fresh branch, in *some* host calls |
| 4 | **Record** the outcome, and did the write **persist**? | `s.recordEvent(...)`, whose return value most callers ignore |
| 5 | **Count** it (event cap) | `freshCall` only |

The bug family of the last weeks is one question skipped in one host call, found by reading code or by luck:

| issue / PR | the question that was missing, in the host call that missed it |
|---|---|
| #4143, #4150 | (3) `DurableCall` at the replay frontier: `replayCall` handed to `freshCall` and the refusal sat in the entry |
| #4152, #4156 | (3) and (1) `DurableCallWithRetry` at the frontier went to `replayCall`: one plain attempt instead of the policy |
| #4143, #4158 | (3) `SideEffectRecorded` (the lookup half of side effects) answered "not recorded" at the frontier with no refusal |
| #4153 | (1) `await_signals(0)` was not replayed, and left replay with history unread |
| #4157, #4163 (open) | (1)+(2) a replay block that leaves replay with history unread runs later steps fresh; the class of #4153 |
| #4159, #4161 | (3) `ContinueAsNew` recorded in a defer segment, losing the cleanup |
| #4160 | (4) a recorded event whose write is refused (quota) does not stop the guest: the run carries on with the step missing |
| #4117, #4165 | (2) a one-chunk stream and a stream failure are the same row, so replay read the wrong one |
| #4177, #4178 (open) | (1)+(4) **an outcome recorded at the wrong step**: `await_promise` records its outcome at the NEXT step and leaves the pending row, and `set_scope`'s retry does the same, so replay of the pending record left replay with history unread and every later call ran again on every later replay. The stored history read correctly (flush's immutability guard hid the duplicates), so only an effects count saw it. Found by review, not by a guard |

Re-derive the size of the duplication (2026-11-19, develop `1157baf3`, non-test `engine/*.go`):

    for f in 's\.exitReplay\(\)' 'advanceReplayStep\(' 'stopBeforeNewWork\(\)' 's\.recordEvent\(' 'replay divergence'; do
      printf "%-24s sites=%s files=%s\n" "$f" \
        "$(git grep -h -E "$f" -- 'engine/*.go' ':!engine/*_test.go' | grep -v '^\s*//' | wc -l | tr -d ' ')" \
        "$(git grep -l -E "$f" -- 'engine/*.go' ':!engine/*_test.go' | wc -l | tr -d ' ')"
    done
    # exitReplay 40 sites/12 files; advanceReplayStep 41/12; stopBeforeNewWork 30/12; recordEvent 52/11; "replay divergence" 20/10

Forty places decide when replay ends. Each is a chance to leave it with history unread, which is the whole of #4153 and #4157.

---

## 2. What the code does today: a machine-derived inventory

`HostHandler` (`engine/imports.go`) is the one interface all three marshalling layers call: `engine/imports.go` for the wazero
core path, `engine/wasmtime_hostfuncs*.go` for wasmtime, and `engine/component_callbacks.go` for components, which calls
`b.handler.DurableCall(ctxWithMem(...), nil, ...)` and the like. **So there are 54 host-handler methods, not three times that,
and a dispatcher placed at the `execSession` level reaches all three paths without touching any of them.** (Re-derive the 54
with the script in appendix B.)

Appendix A is the generated table: for each method, which of the five concerns appear in its body or in an `execSession`
method it calls (two levels deep). **It is a text scan and it is wrong in both directions**: a helper reached through a
function value is missed, and a match in a branch that never runs is counted. Use it as a map. The second reading is the
site count in section 1, which counts call sites rather than methods and does not depend on how deep the scan looks.

What the two readings agree on:

- **34 of 54** methods record an event; **20 do not** (appendix A, "records").
- **26** methods call `stopBeforeNewWork()` directly or through a helper. Of the 34 that record, **nine do not**:
  `ContinueAsNew` and `ContinueAsNewWithVersion` (fixed by #4161, merged; not on develop at the time of the scan),
  `AwaitChild`, `AwaitAllChildren`, `AwaitAnyChild`, `AwaitPromise`, `RegisterUpdateHandler`, `ReleaseLock` and `ListCrons`.
  Whether each *should* is a separate question per call (section 5): a wait that only reads a recorded outcome is not "new
  work". The point is that nothing but a reader decides it, and a reader decided it nine different ways.
- **35** methods advance replay and **35** mention divergence: wherever replay is consumed there is a hand-written check.

### The shape every step takes today

Reconstructed from `DurableCall`, `replayCall`, `freshCall`, `DurableDefer`, `CreatePromise`, `SideEffect`, `SignalWorkflow`:

    HostCall(args):
      if s.isReplay:
        if frontier (stepCount >= len(history)):  exitReplay(); fall through to fresh      # (1)
        else:
          rec := history[stepCount]
          if rec.Type != expected or rec fields mismatch:  report divergence               # (2)
          advanceReplayStep(rec)                                                            # consumes, moves the clock, extends the checksum chain
          return decode(rec)
      if stopBeforeNewWork():  return callSuspendSentinel                                   # (3), in SOME of them
      result := do the work
      persistence := recordEvent(EventRecord{...})                                          # (4), result ignored by most
      return encode(result)

and the specific ways it has gone wrong: the refusal at (3) sits on the non-replay path, so a call that arrives through the
frontier branch skips it (#4143); a block at (1) that returns before consuming everything leaves replay with history unread
(#4153); a block at (2) that matches on type alone cannot tell two records of one type apart (#4117).

---

## 3. The proposal

One method on `execSession` runs every step. A host call stops *being* the step and becomes **a description of one**.

```go
// step is the one place the five questions are answered. It is unexported and reached only from HostHandler methods.
func (s *execSession) step(ctx context.Context, sp stepSpec) stepResult

type stepSpec struct {
    Name   string       // "durable_call", for divergence messages and the inventory
    Kind   stepKind     // see section 4
    Effect effectClass  // what the step does to the outside world: drives (3), see below

    // Replay side. A pure function of the recorded event(s); it must not touch the world.
    Want   func(rec *EventRecord) mismatch   // (2): nil if rec is THIS call's event; else a typed reason
    Decode func(recs []EventRecord) stepResult

    // Fresh side. Does the work and says what to record. It is only called when the dispatcher has decided fresh work is allowed.
    Fresh  func(ctx context.Context) (stepResult, []EventRecord)
}
```

and the dispatcher is, in full:

    step(sp):
      pos := s.position()                       # InHistory | AtFrontier | Fresh          -- (1), computed in ONE place
      if pos == InHistory:
        recs, why := s.takeRecords(sp.Kind)     # takeRecords reads 1..n records; it is where "consume everything this step owns" lives
        if why != nil:  return s.divergence(sp, why)                                      -- (2), one message format, one metric
        return sp.Decode(recs)
      if pos == AtFrontier:  s.leaveReplay()    # the ONLY exitReplay in the engine for steps; it asserts stepCount == len(history)
                                                #  unless stopIdx fast-forward is armed (a segment's stop)     -- the #4153/#4157 invariant
      if sp.Effect != Pure && s.stopBeforeNewWork():  return callSuspendSentinel          -- (3), once, for every non-pure step, on both paths
      res, recs := sp.Fresh(ctx)
      for rec in recs:  s.recordEvent(rec)      # a failed write is noted ON THE SESSION (see below), not returned per step   -- (4)
      s.countStep()                                                                         -- (5)
      return res

**Superseded in two places by Revision 2 (end of this document):** `Fresh` is handed a recorder (`st.Record`) and does not return records, because several calls record first and act second; and `stepSpec` carries a park field from the start.

The five questions become five lines. The properties the bugs violated become properties of this function, not of 54 callers:

- **No refusal is skippable.** A step with `Effect != Pure` is refused in a defer segment's body whichever way it arrived,
  because there is one arrival path after `leaveReplay`. #4143, #4152, #4158 and #4159 cannot recur *by construction*, which is
  stronger than a guard that scans for them.
- **Replay never ends with history unread.** `leaveReplay` asserts it, once. #4153 and #4157 are one assertion.
- **An outcome is recorded where replay will look for it, and replay is proven to consume exactly what fresh recorded (cleat#4177).**
  This is the invariant #4177 broke, named because it is the one a per-call guard cannot state: a step's `Fresh` records its outcome at
  some step, its `Decode` looks for it at some step, and nothing tied the two together, so each pair was right or wrong by reading.
  Today a completion can be recorded OVER the pending row (`await_child`, `await_all_children`: same step, the store's conflict clause
  reopens it) or at the NEXT step (`await_promise`, `set_scope`'s retry, `await_any_child`), and the replay has to know which. In the
  dispatcher the spec says which (`Outcome: OverPending | AtNext`), `takeRecords` reads exactly that shape, and the dispatcher's
  property test is a **round trip over every spec**: run `Fresh` into a real history, replay that history, and assert (a) replay ends
  with `stepCount == len(history)` and nothing unread, (b) the effect count is 1, (c) the answer is the one the original run got, and
  (d) a second replay changes nothing. That one test is #4153, #4157 and #4177 together, and it is not a text scan, so it cannot
  read a denial as a confirmation. It must run against a **real event_history** and not a hand-written one: #4177's issue records that
  the store tests hand-wrote the completing record at the same step as the pending one, which is the shape the code did not produce.
- **A divergence has one shape**: one message, one metric, one place a replay tester hooks.
- **A recorded event's write failing stops the guest, without a per-step check.** The first draft of this design put a
  `persistenceFailure` hook after every record. WS-cleat-2's #4166 (stacked on #4151; **both merged**, checked 2026-11-20) chose the
  #4160 policy differently and better: a failed write (other than `ErrFenceLost`) sets `s.eventWriteFailure` and suspends the run,
  and `stopBeforeNewWork` returns true while it is set, so **the next fresh step is refused by the refusal in line (3)**. The
  dispatcher needs no extra code for #4160; it needs line (3) to be on every non-`Pure` step, which is its main property. A quota
  refusal is a typed `ErrEventQuotaExceeded` answered with the cap's continue-as-new (their description, not re-measured here).

### `effectClass`, and why it is not a boolean

| class | meaning | refused in a segment's body? | examples (from the inventory) |
|---|---|---|---|
| `Pure` | derived from the session and the history; no world access, no event | no | `Now`, `Random`, `UUID`, `WorkflowID`, `RunID`, `Version`, `JsonParse` |
| `Read` | reads recorded or live state; records an event only so replay can return the same answer | **decided per call** (section 5) | `ListCrons`, `GetScope`, `PollSignal`, `PollChild` |
| `Effect` | reaches the outside world or mutates durable state | **yes** | `DurableCall`, `PluginCall`, `SignalWorkflow`, `ResolvePromise`, `ScheduleCron`, `AcquireLock` |
| `Terminal` | ends the run | **yes, and records nothing** | `ContinueAsNew*` |

The only thing a class decides is whether `stopBeforeNewWork` applies. The nine record-without-refusal methods in section 2 fall into `Read`, `Terminal` and `Wait` rows, or nobody decided them; the design forces each into a row, and section 5 says which are open.

---

## 4. What is *not* a plain step: the exceptions, named

A dispatcher that takes every host call through one shape ends up special-casing the awkward ones inside itself. The design
instead gives the awkward ones a **kind**, so the exception is in the spec and not in the dispatcher.

| kind | host calls | what is different | how the dispatcher treats it |
|---|---|---|---|
| `Single` | `DurableCall`, `DurableSend`, `DurableDefer`, `CreatePromise`, `SideEffect`, `PluginCall`, `SignalWorkflow`, `ScheduleCron`, `DeleteCron`, `ListCrons`, `AcquireLock`, `ReleaseLock`, `SetScope`, `Fetch`, `RegisterUpdateHandler`, `ResolvePromise`, `RejectPromise`, `DurableScheduleInvoke`, `DurableCompleteUpdate` | one call, one event | the base case |
| `Multi` | `DurableCallWithRetry`, `DurableCallWithHeartbeat` | one call, 1..n events (`call_attempt_failed`..., then `call`); a history can end **inside** the call (a run lost mid-policy), and the call then *resumes* | `takeRecords` reads a run of `call_attempt_failed` then at most one terminal; "ends inside" returns a `Resume{spent}` the spec's `Fresh` continues from (#1145, #4152) |
| `Stream` | `PluginCallStreaming` | n chunk events for one call; the *fresh* side publishes each chunk to a live reader as it is recorded, and a failure is one synthetic chunk | `Kind = Stream` has its own `takeRecords` (read chunks until finished) and its `Fresh` records as it goes. This is the one place the dispatcher's "record after Fresh returns" is wrong, so `Stream` records inside `Fresh` and returns an empty list. #4117's marker (`StreamData`) belongs to this spec. |
| `Wait` | `DurableAwaitSignals`, `AwaitPromise`, `AwaitChild`, `AwaitAllChildren`, `AwaitAnyChild` | the outcome depends on **time and the world** at replay: the recorded `await_*` event is followed by the delivery or by nothing, and "nothing" means *wait again*, not "diverge". `await_signals` re-polls the store on replay when its event is the last one. **Wait is also where residency (#4162) attaches.** | `Kind = Wait`: `Decode` returns `Done(result)`, `StillWaiting{until, wake}` or `TimedOut`. The dispatcher turns `StillWaiting` into **park-or-suspend**: if the session may park it blocks on `wake` and then returns `Done` through the *same* `Decode` run against the live store; otherwise it returns the stop as today. **Residency lives in this one place and the spec only says what it waits for.** Two consequences (WS-cleat-2): a Wait **records its `await_*` event BEFORE it blocks** (a crash while parked must find the await event and its `TimestampMs` anchor in history, or recovery restarts the timeout clock), so Wait, like Stream, records inside `Fresh` or through a pre-block hook; and `update_received`, written by the API side, is a new wake source. |
| `Sleep` | `DurableSleep` | **records no event**: it decides from the clock (`IMPROVEMENT-PLAN 3.67`), which is why it appears in neither the replay nor the record column | stays outside the dispatcher: it has no history access, and putting it through would *add* an event and change the history format. **But its final "set `suspendErr` and return the stop" goes through the same helper as a Wait's suspension (`s.suspendOrPark(until)`),** or a sleep can never park (WS-cleat-2). |
| `Terminal` | `ContinueAsNew`, `ContinueAsNewWithVersion` | records the event that **ends** the run and sets the suspension; nothing may follow it | the dispatcher refuses it in a segment (#4159) and records nothing after it; the `Terminal` class carries that |
| `Lookup` | `SideEffectRecorded` | the **lookup half** of a two-call protocol (lookup, then record): an SDK that asks first runs the user's function *between* the two. It records nothing; it answers "recorded value / not recorded" | its `Fresh` is "answer not-recorded", and it is refused at the frontier like an `Effect`, because answering "not recorded" is what makes the SDK run the function (#4158). This is why the class is decided by what the *caller will do next*, not by what the call itself does. |
| `Live` | `PollSignal`, `PollChild`, `PollCancellation` | **return the world's current state with no event** | outside the dispatcher. Their determinism is a property of when the guest asks (`PollCancellation` returns 0 during replay by construction), not of the engine's history. They are listed as the exceptions the dispatcher *must not* absorb, and as the first place a future "unrecorded read" bug will be. |
| `Spawn` | `ChildWorkflow`, `ChildWorkflowWithOptions`, `RunDetached`, `StartDetached` | the **record and the effect are one database transaction**: the child row and the event are inserted together (`StartChildWorkflowAtomic`, `children.go`, so a crash cannot leave a child with no parent event or the reverse; #777). The dispatcher's "do the work, then record" is two steps where this call has one | `Fresh` returns with the event already written by the store, and the spec says so (`RecordedBy: Store`); the dispatcher then skips its own `recordEvent` but still runs refusal, the persistence outcome and the count. The atomicity is the exception and stays where it is. |
| `Poll` | `DurablePollUpdate` | a replay poll **legitimately finds nothing**: when the record at this step is not an `update_received`, it reports "nothing found" *without advancing and without leaving replay*, because the guest polls at every suspension and treating that as a mismatch would end replay at the first one (`updater.go`). It is the one step where a non-matching record is not a divergence | `Want` returns a third answer beyond *match* and *diverge*: *not mine, leave it*. `takeRecords` then consumes nothing. This is the reason the "replay never ends with history unread" assertion has to be "at leave-replay", not "at every step". |
| `Update` | `RegisterUpdateHandler`, `DurableCompleteUpdate` | an update's *delivery* record (`update_received`) is written by the **API side**, not by the guest; `DurableCompleteUpdate` records the guest's answer. The `update_received` event type is shared between "delivered" and "polled", which is why `stoppableByOp` exists in the stop inventory (`engine/an_event_is_stoppable_only_if_a_host_site_can_refuse_it_test.go`) | `Single` steps; the dispatcher inherits the distinction and does not resolve it. |

`Pure` host calls (`Now`, `Random`, `UUID`, `GetScope`, `Version`, `MinVersion`, `WorkflowID`, `RunID`, `JsonParse`,
`JsonStringify`, `SetQueryState`, `RegisterQueryHandler`, `SetDeferPhase`, `DurableLog`, `ServeWasiSleep`) do not go through
the dispatcher at all. The list is appendix A's "records" column read backwards: 20 methods record nothing.

**The count of exceptions is the honest cost of this design: nine kinds beyond `Single` (`Multi`, `Stream`, `Wait`, `Sleep`, `Terminal`, `Lookup`, `Live`, `Spawn`, `Poll`), and of 54 methods 19 are `Single`, 4 are `Spawn`.** If a reviewer thinks a kind should dissolve into `Single` plus a flag, that is the argument to have, and section 9
lists the two I am least sure of.

### A composite call needs no kind: `cleat_wait_for_event` (G3, added 2026-11-22)

`cleat_wait_for_event` (`WaitForEvent`, `engine/wait_for_event.go`) is **not one of the exceptions above, and not a step**. It writes no record of its own and has no replay half. It is a
loop over two calls that are already on the dispatcher: it claims through `PluginCall`, and when the claim finds nothing it waits through `DurableAwaitSignals`, then claims again. The records it
leaves are the ones those two write (`plugin_call`, `await_signals`, `signal_received`, `plugin_call`, ...), and the loop never chooses between a fresh and a replayed arm: it calls the thing that
chooses. So a divergent, compacted, suspended or stopped history behaves, through this call, as it does through those two, and **a spec for it would have nothing to describe**: there is no record to
declare, no `Decode` to write and no replay code to delete. The G3 plan listed it as the last call to move; there is nothing to move.

Two tests keep that true rather than leaving it a sentence here. `TestWaitForEventStaysComposite` fails if `wait_for_event.go` ever names an `EventType`, takes anything from the session (under whatever name a
function holds it) beyond the clock, the suspension, the two calls, the failure writer and the one stop guard, hands the session to another function or aliases it, or reads `isReplay` anywhere but
that guard. It has a known positive for each way of breaking the rule, and the day it fails the right response is to give the new record a spec and move the call. **Its limit is that the allowed
names are matched by name and the scan reads this file, not the files it calls into**: `s.PluginCall` passes because `PluginCall` is on the list, whatever it does elsewhere. `TestWaitForEventComposesTheCallsThatAreOnTheDispatcher` and
`TestACompactedHistoryReplaysThroughWaitForEvent` pin what the composition does today, row by row, with the expectations written before the first run.

What that table found, **recorded and not fixed here** (cleat#4368): a divergence inside the call's `plugin_call` is wrapped by the loop as "the output is not claim-shaped" (the dispatcher's
answer has a call-error code of 0 and an error byte of 1, and the loop tests the first), with the divergence text further inside the message; a divergence at the await is not
surfaced at all, because the await's answer is neither the stop word nor a suspension and the loop takes it for an expired wait; and a replayed wake logs one `host call output truncated` warning,
because the loop gives the await zero-capacity scratch buffers.

---

## 5. The refusal rule, and what it couples to

The first draft listed nine host calls that record an event and never refuse in a segment, with a recommendation per call.
WS-cleat-2's review replaced that with **a rule**, and the rule is better because the per-call list is the shape of #4143 and #4158:

> In a defer segment's body, **refuse every non-`Pure`, non-`Live` step at the frontier.** A segment's job is to stop the body
> and run the defers. An exemption list buys nothing.

So `AwaitChild`, `AwaitAllChildren`, `AwaitAnyChild`, `AwaitPromise`, `ReleaseLock` (a body release past the end of history is
the body running on; the defers' own calls go through via `inDeferDrain`, and a lock TTL covers a leak), `RegisterUpdateHandler`
and `ListCrons` are all refused, unless the harness shows a case that needs one, and `ContinueAsNew*` already is (#4161).
**None of the waits' or `ReleaseLock`'s refusals has been measured**; the rule is argued, not observed, and the PR that
adds each says so. The dispatcher makes each a one-field change (`Effect:`), which is what makes the rule cheap to follow.

**`ReleaseLock` measured, cleat#4211, 2026-11-20: the rule is wrong for it, and the argument above was from a segment's body alone.**
`stopBeforeNewWork` is true in three situations, not one: a segment's body, after a worker shutdown, and after an event that could not be
written. Measured on develop before the move (`a_lock_call_in_each_stop_state_test.go`, 12 cases, each fresh and at the replay frontier),
`AcquireLock` is refused in all four states that matter (a segment's body, a shutdown, **a defer draining on a shutting-down worker**, an
unwritable event) and `ReleaseLock` in none. The third is the case that decides it: `inDeferDrain` exempts a defer's own calls from the first
condition and from nothing else, so a cleanup draining on a shutting-down worker cannot take a lock but can give one back today, and the
blanket rule would leave it held until its lease ran out, which is the leak the exemption was written to prevent. So `ReleaseLock` moved
onto the dispatcher declaring `Refuse: neverRefuse` (the hook of cleat#4220), which is never refused, and the exemption entry in
`a_replay_to_fresh_hand_off_refuses_or_is_named_test.go` was deleted because the class says it where the spec is declared. Whether a segment's
BODY should release a lock is a separate decision, not made here; the other calls in the list above are still unmeasured.

**`SetScope` moved, cleat#4211, 2026-11-20, and three of its decisions are not visible from the code.** (1) *Only taking a scope is a step.* `SetScope("", "")`
clears the scope, records no event and is never refused (measured in every stop state, fresh and replayed), so it stays in the `SetScope` method: a fresh clear
releases the key, a replayed one only forgets it. (2) *The previous scope is written into the guest's buffer BEFORE the step*, on both arrivals, before history is
read and before a refusal (a refused call has already written it, measured), and its packed result travels in `stepArgs.i1`. A `Before` hook was planned for this
and was not needed. (3) *`outPtr` and `outMaxLen` stay ZERO on purpose*: that buffer holds the previous scope, and the divergence message a replay writes to
`outPtr` would replace it. Its history is a run (failed attempts, then the acquisition), read whole by the run reader; the two shapes that used to be accepted or
answered with a bare error (a first record naming another scope; a failed run followed by another scope's or another type's record) are now a counted divergence
that consumes nothing. The fresh half records through `c.Record` in the spec's own `Fresh`, as the exemption test requires, and a method of its own
(`freshSetScope`) arms the two suspensions, which the suspension-sites guard counts by that name.

**One coupling to write down, because it is easy to miss: refusal and stoppability are linked.** The stoppable set
(`engine/stop_at_last_stoppable.go`, `stoppableByOp`, `TestEveryHostStopSiteIsClassifiedForTheStoppableSet`) is derived from the
refusing sites intersected with the component world's typed outcomes, so **every new refusal can move the stop point later and
change which prefix a cleanup replays.** Each widened refusal therefore needs a **kill-after-it test** in the real-worker kill
test; `cmd/cleat-worker/a_killed_python_component_runs_its_defers_through_the_worker_test.go` is the pattern (a kill right after
the call, and the cleanup must see the call before it).

**Event cap (5).** On develop (`1157baf3`) only `freshCall` counts toward `maxEventsPerWorkflow` and triggers auto-ContinueAsNew
(`grep -n 's.eventCount\|maxEventsPerWorkflow' engine/*.go`: three sites, all in `DurableCall`). **WS-cleat-2 reports this is already
changing:** on #4151 (head `35f9eec0`; **merged**, the no-wait awaits now count) the no-wait `await_signals` and `await_promise` record an event, count
toward the cap and stop at it (`refuseAtEventCap`, `triggerEventCapContinueAsNew`), after a poll loop at `--max-quota-events 25`
failed at the 30 s fence. Counting *every* recorded event in the dispatcher would still be **a behaviour change** (a workflow of
plugin calls would start hitting the cap), so the migration does not do it silently: PR 1 keeps the count where it is on the day it
is written (`DurableCall`, plus the two no-wait awaits, since #4151 has merged), through a spec field, and a later, separately
announced PR decides whether to widen it.

---

## 6. Migration: small PRs, each checked

**Rule for every PR below: the PR changes where code lives, and nothing else.** A step's recorded events, the checksum chain,
the guest-visible result and the suspension are byte-identical before and after. That is checkable, and the first PR builds the
check.

| PR | change | checked by |
|---|---|---|
| 0 | **Characterization fixtures, no production change.** For each host call, on current develop: run it fresh, then replay each prefix of its history, and record `(result bytes, events recorded, isReplay after, stepCount after)` as a golden table, plus the same for a defer segment and a defer body. Written once, from the *old* code. | the fixtures themselves, with a known-positive (mutate a host call, watch its row fail) |
| 1 | The dispatcher and its spec types, with **no users**, plus property tests over a fake spec table: leave-replay asserts history is read; a non-`Pure` step is refused in a segment on both arrival paths; one message for a divergence; the outcome round trip above over every spec in the fake table; a session with a recorded write failure refuses the next non-`Pure` step (the #4160 policy, #4166, merged). | the property tests; the existing guards untouched |
| 2 | ~~`DurableCall` through the dispatcher, as the reference: simplest shape.~~ **Superseded by Revision 2: `DurableCall` is the heaviest step, not the simplest.** The first users are the three promise singles; `DurableCall` follows with pre-check fields. It keeps the event-cap counter and the most guards pointing at it. | PR 0's rows for `DurableCall`; `replay_fresh_parity_test`; the harness's golden run for `call` |
| 3 | The `Single` effect calls in two or three batches, ordered by how many guards cover them: `SideEffect`+`SideEffectRecorded`, `CreatePromise`+`ResolvePromise`+`RejectPromise`, locks and scope, crons, `SignalWorkflow`, `DurableSend`, `DurableDefer`, `Fetch` | PR 0 rows; the harness's per-op kill/terminate rows |
| 4 | `Multi`: `DurableCallWithRetry`, `DurableCallWithHeartbeat` | PR 0 rows plus #4156's and #4168's tables (the mid-retry rows are exactly this kind's hard case) |
| 5 | `PluginCall`; then `Stream` (`PluginCallStreaming`) on its own, because it is the kind that does not fit "record after Fresh" | #4165's tests; the live-reader tests (`a_live_reader_never_stalls_a_workflow_test`, `stream_chunk_tail_test`) |
| 6 | `Wait` and its callers, one call per PR: `DurableAwaitSignals` first (it has the re-poll and the most history), then promises, then children | the harness's signal and promise scripts; the time-dependent branch needs the harness's kill-while-waiting row |
| 7 | `Terminal` and `Update` | #4161's real-worker test; the update dispatch tests |
| 8 | Delete the per-call helpers that nothing calls (`replayCall`, `replayRetryAttempts`, the per-call frontier blocks) and **shrink the guards** (section 8) | the dead-exports guard; the guards that remain |

Each of PRs 2 to 7 is one reviewer-sized change, and any of them can be reverted alone because the old code for the *other*
calls is still there. **The two old shapes coexist for the length of the migration**, which is the cost: during it the
engine has the five questions answered in two ways. The ordering puts the calls with the most shared guards first so that the
coexisting period is the one with the most checking.

---

## 7. What does not change

- **ABI and WIT.** `HostHandler` keeps every method and signature. The three marshalling layers (wazero imports, wasmtime
  hostfuncs, component callbacks) call the same 54 methods and are not edited. `ABI.md` and `scripts/check-doc-consistency.sh`
  are unaffected; the export set is untouched.
- **History format.** The dispatcher records through the existing `recordEvent`. `EventRecord`, `eventRecordToPayload`,
  `populateFromPayload`, the compaction state and the typed events are not edited, so the payload-carrier and
  compaction-equivalence guards stay as they are. The checksum chain is extended by the same `advanceReplayStep` (the
  dispatcher calls it; it does not reimplement it).
- **Database schema.** No column, no migration. `insertEventSQL` and the flush paths are not touched.
- **Behaviour on any recorded history.** PR 0's fixtures are the claim and PR 2 onward are held to them.

The one thing the dispatcher *could* change is the event cap and the nine refusal decisions of section 5. Both are listed so
that they are decisions, not side effects.

---

## 8. What shrinks

Guards that exist because a host call can forget a question become properties of `step`, and are deleted once PR 8 lands.
Guards that check a **cross-language** fact stay, because the dispatcher cannot see another language.

| guard (develop, 2026-11-19) | after | why |
|---|---|---|
| `a_replay_to_fresh_hand_off_refuses_or_is_named_test` (the frontier-answer scan, `frontierAnswersThatNeedNoRefusal`) | **replaced by a test of `step`**: any spec with `Effect != Pure` is refused | the scan reads host-call source for a refusal; the property is now a fact about one function |
| `a_host_call_never_leaves_replay_with_history_unread_test` (#4163, **merged**; it also added `engine/replay_divergence.go`, a first shared divergence helper) | replaced by the `leaveReplay` assertion; the helper becomes `step`'s divergence path | one assertion, not a scan |
| `engine/stop_correspondence_guard_test.go` (the engine-side half) | **stays**, shrinks | the SDK side (every SDK treats a stop as a stop) is cross-language, and so are the Java/Rust parity counts: `stopSitesOn20260904` counts engine *stop sites*, and the dispatcher turns many hand-written sites into one check per effect step, so how that guard derives its count has to be re-pointed at the specs. I have not worked out how; it is the part of this section I am least sure of. |
| `an_event_is_stoppable_only_if_a_host_site_can_refuse_it_test` | stays, reads `Effect` instead of scanning | the inventory of "which event types can a stop point land on" is about event types, not call sites |
| the per-call frontier tests (`replay_frontier_test`, `a_retry_policy_applies_at_the_replay_frontier_test`) | stay as behavioural tests; their *reason for existing* moves into PR 1's properties | they are the characterization that PR 0 starts from |

A guard is removed only after a mutation of the dispatcher makes its replacement fail, in the same PR.

---

## 9. Risks, and what I am least sure of

1. **The coexistence period (section 6).** Two answers to the five questions live in the engine until PR 8. The migration
   order is the mitigation; the risk is that PR 8 never lands and the engine keeps both. *Mitigation: PR 8's deletion list is
   written in PR 1 and its absence is a visible TODO, not an implicit one.*
2. **`Wait` is the least likely to fit, and WS-cleat-2's point makes it necessary to try.** `DurableAwaitSignals` re-polls the
   signal store on replay and decides from `rec.TimestampMs + TimeoutMs` against the wall clock. If `Decode` returning
   `Done | StillWaiting | TimedOut` cannot express it without the spec reaching into the session, `Wait` stays outside the
   dispatcher, **and then residency has to be implemented in every wait's helper instead of once.** That argues for making
   `Wait` fit. **Decide after PR 5, with `DurableAwaitSignals` as the test case, not before; the bar is that it fits.**
3. **`Stream` and `Wait` both record inside `Fresh`** (Wait must record its `await_*` event before blocking). That breaks the
   dispatcher's "record after Fresh returns" for two kinds. Because a failed write now stops the guest through line (3) and
   not through a per-step return, nothing needs to be reachable from inside a spec for persistence; the remaining surface is
   `recordEvent` itself. If that is too much, `Stream` stays outside. *The kinds I would most like a second opinion on are
   `Stream` and `Wait`.*
4. **Resident workflows (#4162).** Settled by WS-cleat-2's review, and worth keeping as a stated assumption. Under PARK (their
   `docs/design/resident-suspended-runs.md`, a draft) the wait **blocks instead of returning the stop**, so a parked session never
   leaves the code path it is on: a wait happens at the frontier, the session is already past replay (`Fresh`), and after the wake
   it is still `Fresh`. **`position()` needs no fourth state**; a resident session is a `Fresh` session that lives longer. What it
   needs is a *stage* in the dispatcher (park-or-suspend, section 4), not a position. A defer segment's body is **never** on a
   resident session: terminate/cancel bumps the generation and nulls `assigned_to`, so the parked instance loses its fence and is
   evicted, and the segment is a fresh claim that replays to the last stoppable event on a new instance; a segment is also made
   ineligible to park. So "a body past the end of history" stays the definition. A kill-while-resident boundary is a new fault the
   harness needs (said on #4169).
5. **Performance.** A spec built per call allocates closures. A host call is already a database round trip when it records;
   there is **no host-call benchmark in the tree today** (`git grep -n 'func Benchmark' -- 'engine/*_test.go' 'benchmarks/*.go'` finds database, compilation and end-to-end ones only), so PR 1 adds one for `DurableCall` replayed and fresh, and PR 2 must show it unchanged within noise. If a spec allocation
   shows up, specs are package-level values and the per-call data goes in a small struct, not in closures.
6. **A text scan for the inventory is a loose instrument** (section 2), and the 54-method count rests on one interface read.
   The cross-check is `go doc -all`-style: the `HostHandler` method set against the `Export` set (`scripts/check-doc-consistency.sh`
   compares exports against `ABI.md`), which is a different denominator. They differ by the exports with no handler method.

## 10. Questions

**Answered by WS-cleat-2** (their review of this draft): position() survives (no fourth state); a segment's body is never on a
resident session; refuse by rule (section 5); keep `Spawn` and `Poll` as kinds (`Spawn`'s one-transaction record is a durability
property, `RecordedBy: Store` is the honest spec, and `Poll` is the reason the assertion is "at leave-replay").

**Still open, for WS-cleat-1:** (a) PR 0's golden fixtures are in-process; the harness is black-box. I want both, and the
harness's golden run to be the same table at process level: can its op-list oracle emit `(events, effects)` per op in a form PR 0
can compare against? (b) A counter for "steps refused in a segment" would let the harness assert that a segment ran its defers and
nothing else, which a count of effects cannot.

**Still open, for me:** how the Java/Rust stop-site count is re-derived from the specs (section 8); and, from WS-cleat-2, that
the transitional guard `engine/every_recorded_event_site_is_classified_test.go` (every function that calls `recordEvent` either
checks `stopBeforeNewWork` or carries a written reason, on develop since #4166) becomes, when the dispatcher lands, "every
`HostHandler` method is stepped through `step()` or is on the `Pure`/`Live` list". I agree, and PR 8 should say so.

---

## Revision 1 (2026-11-19): after WS-cleat-2's review of the first draft

What changed, and what did not.

- **#4160 no longer needs a hook.** #4166 (since merged) chose a policy that stops the guest through the existing refusal; the
  `persistenceFailure` step is gone from the dispatcher (section 3).
- **Wait is where residency attaches**, so it records before it blocks, and its suspension is a *park-or-suspend* stage; sleep's
  stop goes through the same helper (section 4).
- **Section 5 is a rule, not nine answers**, and it names the refusal/stoppability coupling and the kill-after-it test it needs.
- **The event cap moved** with #4151 (since merged); the migration's PR 1 is written against develop.
- **Unchanged:** the dispatcher shape, the nine kinds (WS-cleat-2 agreed `Spawn` and `Poll` stay), the migration order, and
  section 7.
- **Facts taken from WS-cleat-2 about open PRs (#4151, #4166, #4162) are theirs and have not been re-measured here.**

## Revision 2 (2026-11-20): after reading the code the first migrations touch, and WS-cleat-2's review of sections 3 to 6

**What I found building the core, and what it changes.** Section 6 said PR 2 migrates `DurableCall` first because it is "the reference: simplest shape". That was wrong, and I wrote it from the inventory and not from the function. Read now, `DurableCall` is the *heaviest* step: its fresh path polls for cancellation, checks `canStartNewWork` (cleat#2008), applies the event cap, branches on write-ahead intent, handles a hard stop mid-call and opens a telemetry span; its replay path resolves a pending call intent through idempotency-key replay and an ambiguity resolver and records the ambiguity. None of that belongs in a first migration, and none of it fits the sketch in section 3 without pre-check hooks in the spec.

- **The core PR migrates the three promise singles, `CreatePromise`, `ResolvePromise` and `RejectPromise`.** Each is one event, replayed by type (`CreatePromise` and `ResolvePromise` by type alone: **neither checked Service/Op on replay when this revision was written, and the first migration kept that as it was; SUPERSEDED by Revision 3, which adds the identity check and keeps only the by-type acceptance of a record with no Service/Op**, the discriminator being the stop-point's business and not the replay's, cleat#4079), then `exitReplay`, then the refusal, then record, then act. `DurableCall` moves later, as its own PR, with the pre-checks as explicit spec fields (`Pre []stepPre`: cancel poll, start-new-work, event cap), and the ambiguity resolution as a `Decode` extension. `SetScope` is also later: it is stateful (scope fields, releasing the old key, store calls) and is the real `AtNext` retry shape, so its place is the group that owns locks and scope.
- **`Fresh` gets a recorder and does not return records.** The sketch had `Fresh` return `(result, []EventRecord)` and the dispatcher record after. `CreatePromise` recorded the event FIRST and then wrote the promise store, and reported a store failure to the guest while the event stayed recorded; `ResolvePromise` and `RejectPromise` did the same. Recording after would have changed that order, which is a behaviour change, and this migration changed where code lives and nothing else. (cleat#4335 then changed the order on purpose, after this migration: the three calls write the store first and record how it went, so that a failed write is on the event and the replay answers it. That is the case `Fresh` calling `Record` where the call records is for: the dispatcher imposed no order, so the change was one spec at a time.) So the spec's `Fresh(ctx, st *stepCtx)` calls `st.Record(rec)` where the call records today, and the dispatcher checks afterwards that it recorded what the spec declared (`Records: One | None | Many`), which is also what the round trip pins. This also removes "Stream and Wait record inside `Fresh`" as a special case: every kind records through the handle, and `Stream` and `Wait` only declare `Many`.
- **Failure handling stays inside `recordEvent`** (WS-cleat-2). On a quota refusal `stopAfterFailedEventWrite` runs `triggerEventCapContinueAsNew` BEFORE it sets `eventWriteFailure`, behind a re-entrancy latch (`handlingEventWriteFailure`), because ContinueAsNew refuses once the failure is set and its own write fails on the quota again. A dispatcher that moved or reordered that would reintroduce the first version's bug found in review of #4166. The dispatcher calls `recordEvent` and nothing else.
- **After a failed write inside a `Many` step the loop stops** (WS-cleat-2's question, decided here and not tested here): `st.Record` returns the persistence outcome, a `Many` spec's `Fresh` stops at the first failed write, and the next fresh step is refused by line (3). Its round-trip row, a write failing mid-step, belongs to PR 4 and 5 (`Multi`, `Stream`), where it can be measured.
- **Line (3) refuses on three conditions, not two** (WS-cleat-2): a defer segment's body, a recorded-event write failure, and **graceful shutdown** (`shutdownObserved()`, `engine/durablecalls.go`). The round trip needs a shutdown row, or a refusal-by-shutdown is read as a refusal-by-segment.
- **The round trip must cover both outcome shapes** (WS-cleat-2). The three core calls are all "record IS the outcome"; `OverPending` (`await_child`, `await_all_children`) and `AtNext` (`await_promise`, the `set_scope` retry) are Wait and scope shapes that arrive in later groups. In the core PR the round trip therefore runs over a **fake spec table with one spec of each shape**, and each later group adds its real calls to the table. A shape with no real call in the table is a gap the PR says so about.
- **Park-or-suspend is a spec field, named here and built with PR 6** (WS-cleat-2): section 9 names it and section 3's pseudocode does not, so `stepSpec` carries `Park *parkSpec` from PR 1, unused until `Wait` migrates.
- **Migration order is changed**: PR 0 (characterization of the calls a PR migrates, from the OLD code, in the same PR as the migration, since the table is that PR's evidence), PR 1 (dispatcher, spec types, fake-table property tests), then the three promise singles in the same PR as the first users, so the dispatcher is never unused code. Each later group adds its calls, its characterization rows and its harness scripts.

**Facts taken from WS-cleat-2 and re-checked against develop 7d1d0e20 on 2026-11-20:** #4151, #4163 and #4166 are merged; `stopBeforeNewWork` has the third condition; `stopAfterFailedEventWrite` orders the quota path as described. Appendix A (generated at `1157baf3`) is regenerated in this revision or its date says it is not.

## Revision 3 (2026-11-20): replay identity is a dispatcher invariant (cleat#4211), and what the core PR (#4207) built

**The rule.** On replay, after the recorded event's TYPE matches, the dispatcher asks the spec's `Identity` function whether the record names THIS call. A difference is a divergence in the one shared shape (`replayIdentityDivergence`), and it consumes nothing. This closes by construction the class cleat#4211 found for locks and scope (a replay that matched on type alone accepted a record of another lock, consumed it and answered as though it were this call) instead of fixing it one call at a time. **Every spec sets exactly one of `Identity` and `NoIdentity` (a reason string); `validate()` enforces it at start-up and a test with controls checks the check.** "This call has nothing to compare" is therefore a decision a reviewer reads in the spec, never an omission.

**What identifies a call** is what cannot legitimately differ between a run and its replay. Decided by the coordinator on 2026-11-20 for the cases that were open:

| call | identity | why |
|---|---|---|
| `DurableCall`, `PluginCall`, `PluginCallStreaming` | service + op (plugin + method); **not the request body** | a body can legitimately differ on replay (a value the workflow computed itself); the body is still printed in the divergence message. Temporal treats activity input the same way |
| `SideEffect`, `SideEffectRecorded` | `NoIdentity`: "the value is the event" | the recorded value is the non-deterministic output by design |
| `ChildWorkflow*` | the workflow type, and the child id **when the caller supplies it**; a generated id is not identity | a generated id differs per run |
| `SignalWorkflow`, `DurableSend` | the target and the signal (or message) name | |
| `CreatePromise` | the promise **name** (the id is generated, and replay returns the recorded one) | built in #4207 |
| `ResolvePromise`, `RejectPromise` | the promise id, and the cleat#4079 Service/Op when the record carries it | built in #4207; a record without the marker (before #4079, or `await_promise`'s own) is still accepted for a matching id |
| `AcquireLock`, `ReleaseLock` | the lock key | cleat#4211 |
| `SetScope` | the scope key (`vo:<type>:<key>`), for the argument shape that records an event. **`SetScope("", "")` is `ClearScope` and records NO event**, so that shape is a no-event kind (like `Sleep` and the live reads), not an Effect step with an Identity; and its replay rebuilds the PREVIOUS scope and writes it to the guest's buffer BEFORE it reads history. `stepArgs` carries the output pointer and length, but `step` reads history first and has no hook for work before it: **an open question for the locks-and-scope group, not built in the core** (WS-cleat-2, checked in `locking.go` and `scope.go`) | cleat#4211 |
| crons | the cron name | |
| `ContinueAsNew*` | `NoIdentity` on the input: "terminal; the input may be computed". Event type only | |
| `Live` (`PollSignal`, `PollChild`, `PollCancellation`) and `Pure` calls | outside `step`: they record no event to compare | section 4 |

**Generated ids are not identity; the stable name next to them is.** A group owner who finds a call with a field that is neither in this table nor obviously one of these kinds sets `NoIdentity` with the reason, and says so in the PR; the reviewer decides, and the table here is amended.

**A behaviour change, and said so.** The first migrations were specified as "changes where the code lives and nothing else". This is the one exception: replay of the promise calls used to accept a record naming a different promise. It was **measured on the old code first** (all three calls accepted, consumed and answered success) and is pinned by three characterization rows that move from "accepted" to "divergence, not consumed" with no other row moving. It is **upgrade-visible**: a history replayed under code that now asks for a different promise fails loudly instead of being silently answered, so the PR carries a `changelog.d` upgrade fragment. Each group that adds identity to its calls has the same obligation: measure the old behaviour first, in the same PR, and say what changes.

**What #4207 built, as it differs from the sketch in section 3 and Revision 2.** `step` reads ONE record per step; `Fresh` records through `c.Record`; `stepCtx` carries per-call data (`stepArgs`) so specs are package-level values; the refusal is applied to every non-Pure step on both arrival paths, on three conditions (a defer segment's body, a failed event write, graceful shutdown); leaving replay asserts the history was all read, and REPORTS it (`stepViolations`, logged) rather than stopping the run. **Not built and said so:** the park-or-suspend field (it has no consumer until a Wait moves), pre-check fields (`DurableCall`), multi-record steps (`takeRecords`), and so the `OverPending` and `AtNext` outcome shapes: each arrives with the first group that needs it, with its own round-trip row.

**For `Multi` (read in the code, not measured).** In `freshCallWithRetry` the attempt loop records a failed attempt and then calls the service again with no `stopBeforeNewWork()` check between attempts; the checks are before the loop only (WS-cleat-2, `durablecalls.go`, read). So after a refused write of one attempt's event the loop would go on to perform the next attempt. That is the reason the decision in Revision 2 stands ("a `Multi` step stops its loop at the first failed write") and the reason the first test row of the `Multi` group is a mid-step write failure. **Whether this is what cleat#4210's failing cases measure is NOT established:** that issue records a symptom (`status=failed` at finalize, `permission denied for table event_history`) and states that its cause was not diagnosed; a diagnosis of this loop was reported by WS-cleat1 in conversation and was not yet posted on the issue when this was written. The row of WS-cleat1's fix is where it gets measured.

**Facts, re-derived 2026-11-20:** `step` is called by exactly 3 host calls today (`grep -n 'return s.step(' engine/*.go | grep -v _test.go`); the Java stop-site count is unchanged at 28 with the three calls counted through `step`.

## Appendix A. Inventory (generated; regenerated 2026-11-20, develop `7d1d0e20`; the 2026-11-19 table was `1157baf3`)

Columns: **replay-aware** the body reads `s.isReplay`; **consumes** reaches `advanceReplayStep`; **exits replay** reaches
`exitReplay`; **refuses in segment** reaches `stopBeforeNewWork`; **records** reaches `recordEvent`; **reads persistence**
mentions the flush outcome; **divergence** mentions a divergence report. "Reaches" means in the body or in an `execSession`
method it calls, two levels deep. A text scan: a map, not a proof.

**What changed against the 2026-11-19 table** (8 of 54 rows, re-derived by diffing the two script outputs): `ContinueAsNew` and `ContinueAsNewWithVersion` now reach `stopBeforeNewWork` (#4161); `DurableCall`, `DurableCallWithRetry`, `SetScope`, `SideEffect`, `AwaitAllChildren` and the two `ContinueAsNew*` rows now read the persistence outcome (#4166); `AcquireLock` no longer does, by this scan. The last is a text-scan result and I have not read why.

| host call | file | replay-aware | consumes | exits replay | refuses in segment | records | reads persistence | divergence |
|---|---|---|---|---|---|---|---|---|
| `DurableCall` | durablecalls.go | x | x | x | x | x | x | x |
| `DurableSleep` | durablecalls.go |  |  |  |  |  |  |  |
| `DurableAwaitSignals` | signaller.go | x | x | x | x | x | x | x |
| `DurableDefer` | durablecalls.go | x | x | x | x | x | x | x |
| `SetDeferPhase` | durablecalls.go |  |  |  |  |  |  |  |
| `DurableLog` | durablecalls.go | x |  |  |  |  |  |  |
| `PollCancellation` | signaller.go | x |  |  |  |  |  |  |
| `PollSignal` | signaller.go |  |  |  |  |  |  |  |
| `ContinueAsNew` | lifecycle.go | x | x | x | x | x | x | x |
| `ContinueAsNewWithVersion` | lifecycle.go | x | x | x | x | x | x | x |
| `ChildWorkflow` | children.go | x | x | x | x | x | x | x |
| `ChildWorkflowWithOptions` | children.go | x | x | x | x | x | x | x |
| `AwaitChild` | children.go | x | x | x |  | x | x | x |
| `AwaitAllChildren` | children.go | x | x | x |  | x | x | x |
| `PollChild` | children.go |  |  |  |  |  |  |  |
| `AwaitAnyChild` | children.go | x | x | x |  | x | x | x |
| `DurableCallWithRetry` | durablecalls.go | x | x | x | x | x | x | x |
| `DurableCallWithHeartbeat` | heartbeats.go | x | x | x | x | x |  | x |
| `Version` | lifecycle.go |  |  |  |  |  |  |  |
| `MinVersion` | lifecycle.go |  |  |  |  |  |  |  |
| `SetQueryState` | lifecycle.go |  |  |  |  |  |  |  |
| `Now` | lifecycle.go |  |  |  |  |  |  |  |
| `Random` | lifecycle.go |  |  |  |  |  |  |  |
| `ServeWasiSleep` | durablecalls.go |  |  |  |  |  |  |  |
| `CreatePromise` | promises.go | x | x | x | x | x | x | x |
| `AwaitPromise` | promises.go | x | x | x |  | x | x | x |
| `PluginCall` | plugins.go | x | x | x | x |  |  | x |
| `PluginCallStreaming` | plugins.go | x | x | x | x | x | x | x |
| `RegisterUpdateHandler` | lifecycle.go | x | x | x |  | x | x | x |
| `DurablePollUpdate` | updater.go | x | x | x | x | x | x | x |
| `DurableCompleteUpdate` | updater.go | x | x | x | x | x | x | x |
| `SignalWorkflow` | signaller.go | x | x | x | x | x | x | x |
| `SetScope` | scope.go | x | x | x | x | x | x | x |
| `GetScope` | scope.go |  |  |  |  |  |  |  |
| `UUID` | lifecycle.go |  |  |  |  |  |  |  |
| `AcquireLock` | locking.go | x | x | x | x | x |  | x |
| `ReleaseLock` | locking.go | x | x | x |  | x | x | x |
| `SideEffect` | lifecycle.go | x | x | x | x | x | x | x |
| `SideEffectRecorded` | lifecycle.go | x | x | x | x |  |  | x |
| `WorkflowID` | lifecycle.go |  |  |  |  |  |  |  |
| `RunID` | lifecycle.go |  |  |  |  |  |  |  |
| `ResolvePromise` | promises.go | x | x | x | x | x | x | x |
| `RejectPromise` | promises.go | x | x | x | x | x | x | x |
| `DurableSend` | durablecalls.go | x | x | x | x | x | x | x |
| `DurableScheduleInvoke` | durablecalls.go | x | x | x | x | x | x | x |
| `RegisterQueryHandler` | lifecycle.go |  |  |  |  |  |  |  |
| `RunDetached` | children.go | x | x | x | x | x | x | x |
| `StartDetached` | children.go | x | x | x | x | x | x | x |
| `Fetch` | lifecycle.go | x | x | x | x | x | x | x |
| `JsonParse` | lifecycle.go |  |  |  |  |  |  |  |
| `JsonStringify` | lifecycle.go |  |  |  |  |  |  |  |
| `ScheduleCron` | schedules.go | x | x | x | x | x | x | x |
| `DeleteCron` | schedules.go | x | x | x | x | x | x | x |
| `ListCrons` | schedules.go | x | x | x |  | x | x | x |

## Appendix B. The script

Run from the repository root. It reads the `HostHandler` interface from `engine/imports.go`, finds each method on `execSession`,
and scans its body and the `execSession` methods it calls.

```python
import re, glob, subprocess, sys
# Which methods are host-handler methods: the HostHandler interface in imports.go.
src = open('engine/imports.go').read()
a = src.index('type HostHandler interface')
b = src.index('\n}\n', a)
iface = src[a:b]
names = re.findall(r'^\t([A-Z]\w*)\(', iface, re.M)
files = [f for f in sorted(glob.glob('engine/*.go')) if not f.endswith('_test.go')]
bodies = {}   # func name -> (file, body)
allfuncs = {}
for f in files:
    s = open(f).read()
    for m in re.finditer(r'^func (?:\(s \*execSession\) )?(\w+)\(', s, re.M):
        i = s.index('{', s.index('\n', m.end()) if False else m.end())
        # find the opening brace of the body: first '{' at end of a line after the signature
        j = m.end()
        k = s.index(') {\n', j) if ') {\n' in s[j:j+2000] else None
        sig_end = re.compile(r'\{\n').search(s, j)
        if not sig_end: continue
        depth = 0; p = sig_end.start()
        while p < len(s):
            c = s[p]
            if c == '{': depth += 1
            elif c == '}':
                depth -= 1
                if depth == 0: break
            p += 1
        allfuncs.setdefault(m.group(1), []).append((f, s[sig_end.start():p+1], m.group(0).startswith('func (s *execSession)')))
FEATS = [
 ('isReplay', r'\bs\.isReplay\b'),
 ('advanceReplayStep', r'advanceReplayStep\('),
 ('exitReplay', r'\bexitReplay\('),
 ('stopBeforeNewWork', r'stopBeforeNewWork\('),
 ('callSuspendSentinel', r'callSuspendSentinel'),
 ('recordEvent', r'\brecordEvent\(|recordEventPersisted\(|recordStreamError\('),
 ('persistence', r'eventFlushFailed|eventNotAttempted|eventPersisted|persistence'),
 ('divergence', r'replay divergence|RecordReplayFailure|divergen'),
 ('history', r'\bs\.history\b'),
 ('stepCount', r'\bs\.stepCount\b'),
 ('frontier', r'stepCount\s*>=\s*len\(s\.history\)'),
]
def callees(body, depth=0, seen=None):
    seen = seen if seen is not None else set()
    out = ''
    for fn in set(re.findall(r'\bs\.(\w+)\(', body)):
        if fn in seen or fn not in allfuncs: continue
        seen.add(fn)
        for (f, b, isx) in allfuncs[fn]:
            if isx and depth < 2:
                out += b + callees(b, depth+1, seen)
    return out
rows = []
for n in names:
    cands = [(f, b) for (f, b, isx) in allfuncs.get(n, []) if isx]
    if not cands:
        rows.append((n, '(none)', [], [])); continue
    f, b = cands[0]
    direct = [k for k, rx in FEATS if re.search(rx, b)]
    deep = [k for k, rx in FEATS if re.search(rx, b + callees(b)) and k not in direct]
    rows.append((n, f.replace('engine/', ''), direct, deep))
print('%d host-handler methods' % len(rows))
for r in rows:
    print('%-28s %-26s direct=%s  via-callees=%s' % (r[0], r[1], ','.join(r[2]), ','.join(r[3])))
```
