# Workflow lifecycle

Every state a workflow instance can be in, what moves it between them, and which of those states
are terminal.

Derived from the code on **2026-09-02**, revised **2026-09-04** when the defer phase gained its
first writer (IMPROVEMENT-PLAN §3.112), and re-verified against `develop` on **2026-09-19** — not
from intent. Every claim below names the symbol that implements it, and the enumerations carry the
command that re-derives them, because the natural mental model of this state machine is wrong in
two places and both are recorded here rather than left to be rediscovered.

**This file cites SYMBOLS, not `file:line`.** It used to cite lines, and on 2026-09-19 all twelve
were stale — several landing in a *different function this same document names*, so
`store_lifecycle.go:505` was offered as `MoveToDeadLetterQueue` and pointed into `CompleteWorkflow`.
A reader who follows that finds plausible code and concludes the document is right. The repo makes
the same argument for its cross-tenant ledger: *"Keyed by FUNCTION rather than by line,
deliberately: a line number changes every time anything above it moves."*

---

## The statuses

`workflow_instances.status` is a `TEXT` column with no `CHECK` constraint
(`migrations/postgres/001_schema.sql`, the `workflow_instances` definition, default `'ready'`). Eight values are written. The
seventh, `terminating`, gained its writer on 2026-09-04 (see the defer phase below); the eighth,
`cancelled`, on 2026-09-14 (cleat#1153).

| status | terminal? | meaning |
|---|---|---|
| `ready` | no | Runnable. A worker may claim it. Covers both "never started" and "sleeping until `next_wake_at`". |
| `running` | no | Claimed by a worker, which is heartbeating. |
| `done` | **yes** | The workflow returned a result. |
| `failed` | **yes** | The workflow failed terminally. |
| `terminated` | **yes** | Force-terminated by an operator through the admin API, or closed by its parent's `TERMINATE` close policy (cleat#1978 — before this the TERMINATE arm wrote `failed`, with a fixed `error_msg` regardless of why the parent closed; it now writes `terminated`, `error_op='parent_close'`, and an `error_msg` naming the parent's actual outcome, e.g. "parent workflow completed"/"parent workflow failed"/"parent workflow was terminated" — see `parentOutcomeMessage`, `engine/store_lifecycle.go`). |
| `cancelled` | **yes** | Stopped pre-emptively by an operator, through `POST /api/workflows/:id/cancel` with `{"preemptive": true}`. **Distinct from `done` deliberately** — cleat#1153: cancellation used to be cooperative and unobservable, so a run that honoured a cancellation and one that simply finished were both `done`, and nobody could tell which had happened. Like `terminated`, it runs the workflow's registered defers first, through the same two-phase transition. |
| `dead_lettered` | **yes** | Retries exhausted. On the Go SDK this is reachable only through a retry policy short enough to have run on the host — see `IMPROVEMENT-PLAN.md` §3.88. |
| `terminating` | no | The defer phase's window: a terminal outcome has been decided and the workflow is running its cleanup before it is applied. Claimable, non-terminal. Written by `TerminateWorkflow`, by `enforceParentClosePolicy`'s TERMINATE arm, and since 2026-09-13 by force-complete and force-fail — in each case only when the workflow has registered defers; cleared by `FinalizeDeferPhase` or by the deadline sweep. Schema in `migrations/postgres/038`, `mysql/037`, `mssql/041`. |

**The five `terminal? yes` rows above are a run's outcomes**, and each carries a contract — what it
keeps and for how long, which redrive verbs apply to it, what a parent awaiting it sees, and which
counter moves. See [Outcomes](#outcomes) below.

### Settled is final

**A run leaves the "terminal? yes" rows above only through a documented redrive verb** (re-replay,
retry, reprocess — not covered by this section), never by a second call to an operator or
pre-emptive-stop verb landing on the same row. `done`, `failed`, `dead_lettered`, `terminated` and
`cancelled` are collectively **settled** (`settledStatusList`, `engine/status_vocabulary.go`).
Terminate, pre-emptive cancel, force-complete and force-fail all refuse with `ErrAdminStateConflict`
→ HTTP 409 `{"detail":"state_conflict"}` when the target is already settled, rather than silently
overwriting it. cleat#1975 (decision D3, 2026-09-22).

The one documented exception is `POST /api/dead-letters/:id/terminate` moving `dead_lettered` →
`terminated` — that route's entire purpose is to take a dead-lettered run off the queue, and it is
the only caller of `TerminateWorkflow` reachable over HTTP. No other transition out of a settled
status is permitted through these four verbs; in particular, terminating or cancelling an
already-`terminated`/`cancelled`/`done`/`failed` row is now a 409, not the idempotent no-op it used
to be.

`terminating` is **not** settled — it is the cleanup window above, and a second terminate landing
on a `terminating` row still cuts the defer phase short rather than being refused, exactly as
before.

Re-derive the written set with:

```
grep -rn "UPDATE workflow_instances" -A 3 --include="*.go" engine/ \
  | grep -v _test.go | grep -oE "SET status = '[a-z_]+'" | sort -u
grep -rn "UPDATE workflow_instances" -A 3 migrations/postgres/*.sql \
  | grep -oE "status = '[a-z_]+'" | sort -u
```

plus the two that no literal-matching grep can find:

```
grep -rn "preemptivelySettle\|preemptivelySettleOnce" --include="*.go" engine/ \
  | grep -v _test.go | grep -oE "status(Terminated|Cancelled)" | sort -u
```

**Three halves are needed, and the third is the one that bites.** Some transitions are written in
Go, some inside `finalize_workflow_status` (a PL/pgSQL function), and `terminated` and `cancelled`
are written through a **parameter** — `preemptivelySettle(ctx, id, reason, statusTerminated)` —
rather than a literal, so the first command cannot see them.

That is not a hypothetical gap. The first command returns six values, this document describes
eight, and until 2026-09-19 the sentence here said "the Go command returns all six" — written when
six was the whole vocabulary, and left standing as two more were added. Anyone re-deriving the set
from the published command got six and had no way to know what was missing.

The SQL command returns `done`, `failed`, `ready` and `running` only: `terminated`,
`cancelled` and `dead_lettered` are never set from a procedure.

**Read that command's output with the migration-004 caveat above already applied, not naively.**
It greps every file in `migrations/postgres/`, so `failed` in that list includes text from
004's and 075's now-superseded bodies — `finalize_workflow_status` has not set `status = 'failed'`
since migration 101 (cleat#1973). The live procedure sets only `done` and `ready`; `failed` is
still one of the eight statuses, but as of 101 it reaches `workflow_instances` exclusively through
`FailWorkflow`'s own Go `UPDATE`, never through this procedure.

**Scope the grep to `UPDATE workflow_instances`.** An unscoped
`grep -rhoE "SET status = '[a-z_]+'"` also sweeps `promises`, `signals` and the plugins' own tables,
which have their own `status` columns with their own vocabularies (`completed`, `resolved`,
`rejected`, `pending`, `delivered`, `dispatched`, …). What comes back is the union of all of them,
and nothing in the result says which values belong to *this* column.

**No total is quoted here on purpose.** This sentence gave one until 2026-09-25 ("nine values for
what is a six-value column") and both halves had rotted: the column is eight-valued, and the
unscoped command returns 17. A count over a growing set is a census, and this document's own rule is
to state the question instead — *does this value come from `UPDATE workflow_instances`?* — and keep
the command, which re-derives either way. Confirm the scoped command is returning 7 and not 6 before
trusting its output, since the eighth and ninth values are written through a parameter (see below).

### `suspended` is not a workflow status

This is the first place the obvious model is wrong. **A sleeping workflow is `ready`, not
`suspended`** — the worker finalizes a suspending segment with `finalStatus = "ready"` and a
`next_wake_at` (`executeWorkflow`'s suspend arm, `cmd/cleat-worker/setup.go`). Nothing anywhere executes
`SET status = 'suspended'`.

The name survives in three places, and none of them make it a status:

- `validFinalStatus` (`validFinalStatus`, `engine/store_lifecycle.go`) accepts `"suspended"` — but the Postgres
  `finalize_workflow_status` function has `WHEN 'done' / 'ready'` and `RAISE
  EXCEPTION` on anything else (`migrations/postgres/101_the_finalize_procedure_stops_deleting_failed_history.sql`).
  Passing `"suspended"` would pass the Go check and raise in the database. No caller does: the
  worker passes only `"done"` or `"ready"`.
- Read predicates of the form `WHERE id = $1 AND status IN ('ready', 'suspended')`
  (`engine/store_signals.go`, `engine/store_promises.go`, and the MSSQL equivalents)
  admit a value that is never present. They are correct but the second term is dead.
- Suspension *is* a real concept — it is what the guest does, and the host reports it through
  `SuspendResult`. It is simply not represented in this column.

> Note for migration 101: it is the highest-numbered migration that **defines**
> `finalize_workflow_status`, superseding 004. Later migrations reference it. For anything created
> with `CREATE OR REPLACE`, find the highest-numbered definition before concluding what the
> procedure does — 004 and 003 both still contain earlier bodies, and 004's had a `WHEN 'failed'`
> arm that 101 removed (cleat#1973: nothing ever called the procedure with `finalStatus = "failed"`
> — a real failure goes through `FailWorkflow`, a plain Go `UPDATE`, not this procedure — so as of
> 101 `validFinalStatus` no longer accepts `"failed"` either; it now raises in Go before reaching
> the database, the same way `"suspended"` already did).

### There are no EXPORTED status constants, and the unexported ones do not cover everything

This section said "there are no status constants" until cleat#1560, which added
`engine/status_vocabulary.go`. What is there now is narrow, and the distinction matters more than
the correction:

- **Unexported constants exist for the settled statuses** — `statusDone`, `statusFailed`,
  `statusDeadLettered`, `statusTerminated`, `statusCancelled` — plus `statusTerminating` beside
  the transition that writes it. They build `settledStatusList`, the canonical SQL spelling.
- **There is still no `engine.StatusReady`**, and no constant for `ready` or `running` at all.
  Callers outside the package still compare strings.
- **SQL still spells every list literally**, on purpose. Embedding the constant would split each
  query into a concatenation, and `engine/mssql_tenant_predicate_test.go` reads statements from
  the backtick literal with no constant folding — it would see only the first fragment of a
  statement whose tenant predicate lives in the second. So the rule is *enforced* by
  `engine/one_definition_of_settled_test.go` rather than shared.

A typo in a status string is therefore still caught by a failing test, if one covers that path,
and not by the compiler — except in a settled-status list, where the guard above fails on any
spelling that is not the canonical one.

### Duplicate calls: one policy, every endpoint

**A request repeated under the same `Idempotency-Key` returns the original response — same status,
same field names — plus one standard flag.** cleat#1169.

```
idempotent_replay: false    this is the original call
idempotent_replay: true     this is a replay of one
```

The flag is a **boolean**, present on both, and spelled the same on every endpoint. A caller that
does not care about retries needs no special case; one that cares reads one field.

| endpoint | first call | repeated under the same key |
|---|---|---|
| `POST /api/workflows/<name>/start` | `201 {"id":…}` | `201 {"id":…, "idempotent_replay":true, "status":…}` |
| `POST /api/dead-letters/<id>/reprocess` | `201 {"id":…}` | `201 {"id":…, "idempotent_replay":true}` |
| `POST /api/workflows/<id>/signal` | `200 {"status":"delivered"}` | `200 {"status":"delivered", "idempotent_replay":true}` |
| `POST /api/schedules` | `201 {"status":"created"}` | `201 {"status":"created", "idempotent_replay":true}` |

**Two things this deliberately does not change.**

*A key reused with a **different** payload is still refused* — `409 idempotency_key_input_mismatch`
(cleat#1170). Replay answers "you already did this"; a changed payload means you did something else,
and returning the first result would hide that.

*A request with **no** key keeps its old behaviour.* No header means no token, not an empty one — so
two callers who both send nothing do not collide, and a caller may still send the same signal twice
on purpose. The flag is present and `false`.

**`POST /api/schedules` answers two different questions, and the key is what separates them.**
cleat#1495. Until it read the header it could not be asked the second one at all: a retried request
and a genuine name collision both arrive as a second create under a taken name, and both were
answered `409 schedule_exists` — correct for the collision, and wrong for the retry in the direction
that makes a client give up on work it successfully submitted.

| request | answer |
|---|---|
| `Idempotency-Key: K`, first time | `201 {"status":"created", "idempotent_replay":false}` |
| the same request again under `K` | `201 {"status":"created", "idempotent_replay":true}` — nothing is created |
| `K` again with a **different** request | `409 idempotency_key_input_mismatch` |
| **no key**, name already taken | `409 schedule_exists` — unchanged |

`ErrScheduleExists` was not reversed by this. It keeps answering *someone else's name is in the way*
and stops being conscripted to answer *I am retrying my own request*, which it was never the right
answer to.

**The comparison covers the whole request, not just `input`.** Name, definition, entry point, cron
expression, input, enabled, timezone, misfire policy, catch-up limit and overlap policy — a key
reused with a different cron expression is a mismatch, not a replay. `next_run_at` is deliberately
**excluded**: the server computes it from the cron expression and the clock on every request, so two
identical retries a second apart always disagree on it, and including it would make every retry fail
closed.

**A schedule created without a key keeps no key**, and nothing is invented for it retroactively. The
key is stored on the schedule row and lives exactly as long as the schedule; it is never returned by
`GET /api/schedules`, because knowing a key lets a caller join or displace another caller's retry.

### The HTTP API returns this column verbatim

A duplicate start — `POST /api/workflows/<name>/start` re-sent with the same `Idempotency-Key` —
answers with **the original response plus the standard replay flag** (cleat#1169): same `201`, same
`id` field, so a caller that never thinks about retries needs no special case.

```json
{"id": "e39ede1b-…", "idempotent_replay": true, "status": "ready"}
```

The first call answers the same shape with `"idempotent_replay": false`. The flag is present on
both, so a caller can read it unconditionally rather than inferring "original" from an absent field.

> **Changed in cleat#1169.** This used to answer `200` with
> `{"already_started": "true", "workflow_id": …}` — a different status, a different marker and a
> different name for the identifier. A caller reading only `id` got nothing back from a
> deduplicated response and concluded its retry had started a *second* workflow, which is wrong in
> the alarming direction. `reprocess` and the signal endpoint changed with it; all three now carry
> `idempotent_replay`.

**`status` is `workflow_instances.status` copied, not a separate API vocabulary.** Every value in
the table above can appear, plus one that is not a workflow status at all:

| value | meaning |
|---|---|
| any of the eight in the status table above | the winner's current lifecycle status |
| `unknown` | the winner could not be read. Stated rather than omitted, so a caller can tell "I cannot tell you" from "I forgot to tell you". |

`unknown` is reachable on a correct tree: a start that is *rejected* records an `idempotency_keys`
row carrying an `error_msg` and no surviving instance, which is why that column carries no foreign
key to `workflow_instances`.

**Branch on terminal versus non-terminal, not on `running`.** A retrying caller is, by definition,
asking about work that may still be outstanding — and outstanding work is usually `ready`, not
`running`. Anything that sleeps, awaits a child, waits on a signal or backs off a retry is `ready`
for nearly all of its life; `running` covers only the slices when a worker holds it. Measured on a
run whose single act is an 8-second `DurableSleepMs`, polled every 500 ms: twenty consecutive
`ready` samples, then `done` — not one `running` (cleat#1325).

A client that needs to separate "never claimed" from "sleeping" must read `next_wake_at`; the
status alone does not distinguish them, per the `ready` row above. That conflation has already
cost one test its discriminating power: a case written to hold a winner open kept passing when its
sleep parameter went unbound, because a 50 ms run and a 20 s sleep are both `ready`.


### Writing a client that retries a start

Everything above says what the server answers. This says what a client should do with it, because
the shape is not obvious and getting it wrong is silent.

**The whole recipe is two loops, and it needs only `id`.**

```go
res, err := c.StartWorkflowWithOptions(ctx, "charge", input,
    backendkit.StartOptions{IdempotencyKey: key})
// Loop 1: retry TRANSPORT failures. The same key makes that safe.
// Do NOT retry ErrIdempotencyKeyInputMismatch or …DefinitionMismatch.

for {
    wf, err := c.GetWorkflow(ctx, res.ID)   // Loop 2: poll until terminal.
    …                                        // wf.Result is here, not in res.
}
```

**Two loops, not one, because they have different budgets.** *"Did my POST land?"* is bounded —
seconds, and a failure means something is wrong. *"Is the work finished?"* is unbounded: a durable
workflow may legitimately sleep for days. Collapse them and a bounded retry budget silently governs
an unbounded wait, so "gave up after five attempts" becomes indistinguishable from "the workflow
failed".

#### Why a blind retry is always safe

A client that sends a `POST` and gets no reply cannot tell three cases apart:

| | what happened | what the retry does |
|---|---|---|
| **a** | the server never received it | finds no key, **starts the run** |
| **b** | received, still processing | **waits** on the key, then returns the winner |
| **c** | finished, the reply was lost | finds the key, returns the winner |

**It does not need to tell them apart.** It retries with the same key, and the server's answer *is*
the disambiguation. That is what the key is for.

Case **b** is the one implementations usually get wrong, and it is handled by the database rather
than by application logic: the key and the workflow row are inserted in **one transaction**, so a
concurrent retry blocks on the key's unique index until the first request commits or rolls back.
Committed, the retry reads the winner; rolled back — which is where a disconnected client lands,
since the request context is cancelled and the transaction is rolled back — the retry's own insert
succeeds and it creates the run. Either way exactly one run exists, and the key is never left
naming a run that was not created.

#### Four things that bite

**The not-ready case is a `201`, not an error.** A retry loop that branches on the status code —
retry `5xx`, treat `2xx` as done — will sail straight past a run that has not finished. Read
`idempotent_replay` and `status` from the body; the transport layer says nothing about whether work
is outstanding.

**A retry can block for as long as the original request's transaction lives.** It is not a fast
"already exists" lookup. Set the client timeout to allow for it, or the retry is cancelled at
exactly the moment it was about to tell you the truth.

**The start response never carries `result`** — on a replay or otherwise. `GET /api/workflows/<id>`
is the only source for it. This is deliberate: one place to look for an outcome, rather than two
that can disagree.

**`status: "unknown"` is not terminal.** It means the winner could not be read, not that it
finished. Treat it as *ask again*. `StartResult.Terminal()` implements this, and the status to
branch on is terminal-versus-not — never `running`, which most outstanding work never reports (see
above).

#### What `idempotent_replay` is for

Nothing in the recipe above needs it: the client polls either way, so correctness does not depend on
knowing which call created the run.

It answers a different question — **did my original request land?** `false` means this call created
the run, so the earlier attempt never arrived (case **a**). `true` means an earlier one did (cases
**b** and **c**). That is worth reading for logging, metrics, or deciding whether to warn a user
that their action was already submitted; it is not worth branching the happy path on.

---

## Outcomes

The five settled statuses are a run's **outcomes**. Everything else in this document says how a run
gets to one; this section says what each one commits to — what it means, what it keeps, what an
operator can do with it, and what it costs to store.

This is also the **invariant set of the lifecycle model** (`specs/CleatRunLifecycle.tla`, cleat#1997).
Keep the two in step: a change here is a change to what the model is supposed to prove.

### What each outcome means

| outcome | means | decided by |
|---|---|---|
| `done` | the entry point returned a result | the workflow |
| `failed` | stopped on an error from the workflow or its own setup, and **not** held for an operator: guest error, trap, timeout, replay divergence, WASM load failure, no entry point, store-rejected result, panic, admin force-fail | the workflow, or cleat on its behalf |
| `dead_lettered` | a failure whose **last durable act was a call that exhausted its retry policy** — a sub-kind of failure, held for an operator | cleat |
| `terminated` | stopped by a person or a policy, not because the workflow erred — includes parent-close since cleat#1978 | operator or policy |
| `cancelled` | stopped by a pre-emptive cancel, and only that | operator |

**Failure is terminal on first occurrence.** There is no workflow-level retry. Retry is per durable
call only (`--max-retries`, `CleatError.Retryable()`), so a run that fails does not try again on its
own — redrive is a manual act.

**`failed` and `dead_lettered` are not the same thing, and the difference decides three behaviours**
(what an operator can do, what it is stored as, and what a parent sees). The predicate is
`deadLettered = eligibleForDLQ && endedOnAnExhaustedCall(history)`
(`cmd/cleat-worker/setup.go`, `writeTerminalFailure`). `endedOnAnExhaustedCall` reads the *history*,
never the error text: it was a `strings.Contains(errMsg, "retries exhausted")` substring test until
cleat#902, which is why prose elsewhere in this repo that still describes dead-lettering as "a
non-retryable error" is wrong — a plain failure is not in the dead-letter queue at all.

### What each outcome keeps, and for how long

| outcome | `event_history` | the `workflow_instances` row |
|---|---|---|
| `done` | **deleted at finalize** — `finalize_workflow_status` does it | `--completed-workflow-retention-days` (default 0, off) |
| `failed` | **kept**, then removed by `--retention-days` (default 30) | `--completed-workflow-retention-days` |
| `terminated` | removed by `--completed-workflow-retention-days` only | `--completed-workflow-retention-days` |
| `cancelled` | removed by `--completed-workflow-retention-days` only | `--completed-workflow-retention-days` |
| `dead_lettered` | removed by `--dead-letter-retention-days` (default 0, off) | `--dead-letter-retention-days` |

Three notes, each of which is a place the obvious reading is wrong:

- **A `failed` run keeps its history.** It is not purged at finalize; `--retention-days` removes it
  later. That is cleat#1973, and it is the correction that makes a failed run inspectable at all.
- **A `done` run's history is already gone** by the time any retention flag runs, so an operator who
  wants a success trail must publish it as query state or into their own store. `--retention-days`
  does first-hand work only for `failed`.
- **The three flags are not three names for one thing.** `--retention-days` touches history only;
  the other two delete the workflow *record* (status, result, error, def_name) and are off by
  default for that reason. `--dead-letter-retention-days` deliberately never touches a run that is
  not dead-lettered, because the dead-lettered run is the one an operator most wants to inspect
  afterwards.

`--completed-workflow-retention-days` sweeps four statuses — `done`, `failed`, `terminated` and
`cancelled` — and the fourth is deliberate rather than incidental:

```
engine/retention_predicates.go — all three dialects, identical
    WHERE status IN ('done', 'failed', 'terminated', 'cancelled')
```

Its own flag help said `(done/failed/terminated)` until 2026-09-25, omitting `cancelled`; the
implementation includes it on every dialect and a comment beside the predicate explains why (an
operator collecting terminated runs expects to collect cancelled ones — both are imposed by a person
on a run that did not finish on its own). The list is authoritative, not the help string.

### What a parent sees

A parent awaiting a settled child **always gets an answer**, whatever the outcome. The mapping is
`childOutcomeForSettledStatus` (`engine/status_vocabulary.go`), applied identically from all three
dialects rather than once per dialect:

| child's outcome | what the parent's `AwaitChild` sees |
|---|---|
| `done` | `Completed`, with the child's `result` |
| `failed` | a failed outcome carrying `error_msg` |
| `dead_lettered` | a failed outcome carrying `error_msg` |
| `terminated` | an error **prefixed `[TERMINATED] `** |
| `cancelled` | an error **prefixed `[CANCELLED] `** |

The prefixes are the point. Before cleat#1974 a parent awaiting a terminated or cancelled child never
got an answer at all, because `GetChildResult` had its own narrower idea of "terminal" than the
settled set. The kind now travels as a stable message prefix, the way `"[AMBIGUOUS]"` already does,
so no SDK surface changed. It also guarantees a non-empty error regardless of `error_msg`
(`nonEmptyChildError`) — see that function's comment for why an empty one would reopen cleat#1379.

Every settled outcome also wakes the parent, fails stranded updates, and notifies finalize
observers. That was cleat#1976, and it was needed because four code paths can settle a run and only
one of them did all three.

### Metrics

| outcome | counter |
|---|---|
| `done` | `cleat_workflows_completed_total` |
| `failed` | `cleat_workflows_failed_total` |
| `dead_lettered` | `cleat_workflows_failed_total` **and** `cleat_workflows_dead_lettered_total` |
| `terminated`, `cancelled` | neither outcome counter; `cleat_workflows_duration_seconds` carries the status as a label |

**A dead-lettered run counting in both counters is deliberate** (owner decision D6, 2026-09-22) and
should be read as such rather than as double-counting. It is a failure *and* it is dead-lettered; an
operator alerting on either question needs it, and the alternative — counting it only once — makes
one of the two questions unanswerable. The call sites are both in
`recordTerminalFailureWithHistory` (`cmd/cleat-worker/setup.go`): `RecordWorkflowFailed` fires
unconditionally on the terminal-failure path, and `RecordWorkflowsDeadLettered` fires beside it
inside the `if deadLettered` arm.

### Redriving a settled run

Three verbs, and they are not interchangeable. Which one applies is decided by the outcome and by
what the run's history still holds.

| verb | endpoint | applies to | what it does |
|---|---|---|---|
| **re-replay** | `POST /api/admin/instances/{id}/re-replay` | `failed`, `terminated`, `dead_lettered` | the **same run**, resuming from recorded history. Only as good as what retention has left. |
| **retry** | `POST /api/workflows/{id}/retry` | `dead_lettered` | the same run, in place, back to `ready` |
| **reprocess** | `POST /api/dead-letters/{id}/reprocess` | `dead_lettered` | a **new** run, from the definition and input — not from history |

The re-replay and retry endpoints are gated by `--enable-admin-api` for the admin routes and by
tenant ownership for the rest; while the admin API is off, its routes answer 404.

`reReplayableStatuses` is `['failed', 'terminated', 'dead_lettered']` (`engine/store_admin.go`).
`done` is excluded because replaying a complete history would walk to its end and finalize again,
writing a second terminal transition for nothing — an operator who wants a finished run to run again
wants a *new* run, which is what reprocess is for.

**`cancelled` is excluded, and that is a decision rather than an omission** (owner decision D4,
2026-09-22). `cancelled` is reached only by an explicit pre-emptive cancel from the run's own owner
(`POST /api/workflows/{id}/cancel` with `preemptive: true`); nothing in the engine writes it.
Resuming it would override a deliberate choice, and nothing is lost by refusing — the owner can
start a new run with the same input.

---

## The state machine

```mermaid
stateDiagram-v2
    [*] --> ready: enqueue (default 'ready')

    ready --> running: ClaimWorkflow / ClaimWorkflows
    running --> ready: segment suspends (next_wake_at set)
    running --> ready: released without error
    running --> ready: reaper reclaims a stalled worker

    running --> done: CompleteWorkflow
    running --> failed: FailWorkflow
    running --> dead_lettered: MoveToDeadLetterQueue (retries exhausted)

    ready --> terminated: TerminateWorkflow (admin, no defers)
    running --> terminated: TerminateWorkflow (admin, no defers)
    dead_lettered --> terminated: TerminateWorkflow (dead-letter terminate route; sole exception to "settled is final")
    ready --> cancelled: CancelWorkflow (preemptive, no defers)
    running --> cancelled: CancelWorkflow (preemptive, no defers)
    ready --> failed: parent close policy TERMINATE
    running --> failed: parent close policy TERMINATE

    ready --> terminating: outcome decided, defers registered
    running --> terminating: outcome decided, defers registered
    terminating --> terminated: FinalizeDeferPhase
    terminating --> cancelled: FinalizeDeferPhase
    terminating --> failed: FinalizeDeferPhase / ExpireDeferPhases
    terminating --> done: FinalizeDeferPhase (force-complete)

    done --> [*]
    failed --> [*]
    terminated --> [*]
    cancelled --> [*]
    dead_lettered --> [*]
```

`terminating` was absent from this diagram until 2026-09-19, while the status table above has
described it since 2026-09-04. It is the only non-terminal state a workflow can be in that a
worker will not start new work for, and leaving it out made the diagram agree with the *obvious*
model of the state machine rather than with the code — which is the failure this whole document
exists to prevent.

### What performs each transition

| transition | implementation |
|---|---|
| → `ready` (enqueue) | column default, `migrations/postgres/001_schema.sql` |
| `ready` → `running` | `ClaimWorkflow` / `ClaimWorkflows`, `engine/store_lifecycle.go` |
| `running` → `ready` (suspend, release, reap) | `finalizeWorkflowSegmentInner`, `finalize_workflow_status` `WHEN 'ready'`, `ReapStaleInstances` |
| `running` → `done` | `CompleteWorkflow` / `FinalizeWorkflowSegment`, `engine/store_lifecycle.go` |
| `running` → `failed` | `FailWorkflow`, `engine/store_lifecycle.go` |
| `*` → `failed` (parent) | `enforceParentClosePolicy` TERMINATE arm, `engine/store_lifecycle.go` |
| `running` → `dead_lettered` | `MoveToDeadLetterQueue`, `engine/store_lifecycle.go` |
| `ready`/`running`/`suspended`/`terminating` → `terminated`, or `dead_lettered` → `terminated` | `TerminateWorkflow` → `preemptivelySettle`, `engine/db.go`, `mysql_ops.go`, `mssql_operations.go` — **not** `*` since cleat#1975 (D3): a settled row other than `dead_lettered` refuses with `ErrAdminStateConflict`. See *Settled is final* above. |
| `ready`/`running`/`suspended`/`terminating` → `cancelled` | `CancelWorkflow`, `engine/db.go`, `mysql_ops.go`, `mssql_operations.go` — all three share `TerminateWorkflow`'s body (`preemptivelySettle`), parameterised on the outcome. No dead-letter exception: every settled status is refused. |

### Which terminal transitions close the workflow's children

`enforceParentClosePolicy` runs on every transition that takes a parent out of
the runnable set, on all three dialects: `CompleteWorkflow`, `FailWorkflow`,
`FinalizeWorkflowSegment`, `MoveToDeadLetterQueue`, `ContinueAsNew` (which
closes the current run), `adminForceResolve` (force-complete and force-fail),
`preemptivelySettle` (which is both `TerminateWorkflow` and `CancelWorkflow`,
since 2026-09-02), and the two defer-phase exits — `FinalizeDeferPhase` and
`ExpireDeferPhases`, the deadline sweep.

Re-derive with
`grep -rn "enforceParentClosePolicy(" --include='*.go' engine/ | grep -v _test`
and map each hit to its enclosing `func`. **Map it, rather than reading the
hit count**: each dialect spells the same call twice, once in the exported
method and once in its `…Once` retry body, so eighteen hits are nine callers.

The list above is that mapping on **2026-09-19**. It gained the two defer-phase
exits since the 2026-09-02 version of this list, which is the expected way for
it to change: a new terminal path that forgets this call leaves a parent's
children running forever, and nothing else notices.

Terminate was the exception until then, and nothing stated why: force-completing
a parent failed its `TERMINATE` children while terminating the same parent left
them running. That is a **behaviour change** for anyone who relied on terminate
being the narrow "stop this one workflow" verb — see `CHANGELOG.md` and
IMPROVEMENT-PLAN §3.79. `ABANDON` children are unaffected on every path.

### Fenced and unfenced transitions — the distinction that matters

Most terminal transitions are **fenced** on `(assigned_to, generation)`: the write applies only
if the worker still owns the claim, and returns `ErrFenceLost` otherwise. That is what makes a
stalled-then-reaped worker unable to overwrite the new owner's result.

Three transitions are **not** fenced, because no worker holds the workflow when they happen. They
set a terminal status with a direct `UPDATE`:

- `TerminateWorkflow` — **only when the workflow owes no cleanup.** Since 2026-09-04 a workflow
  with registered defers takes the two-phase transition below instead, and its terminal status is
  then written by `FinalizeDeferPhase`, which *is* fenced on the defer segment's own claim.
- `enforceParentClosePolicy`'s TERMINATE arm — **same qualification, same date.** A child that
  owes cleanup goes to `terminating` carrying `pending_terminal_status = 'failed'`; a child that
  owes none is failed here as before.
- `adminForceResolve` (force-complete and force-fail) — **same qualification since 2026-09-13.**
  A workflow that owes cleanup goes to `terminating` carrying the operator's intended outcome in
  `pending_terminal_status` (`'done'` or `'failed'`), with the result or error written at the same
  time; `FinalizeDeferPhase` applies it once the defers have run. A workflow that owes no cleanup
  is resolved directly, as before.

  **This reverses `tiers.yaml` D10**, which decided on 2026-09-04 that force-resolve would stay
  one-phase. The repository owner reversed it on 2026-09-13 (cleat#1152). D10's objection was not
  wrong, and it is now the documented operator-visible cost — see "What it costs" in the
  defer-phase section below.

Re-derive with `grep -rn "SET status = '" --include='*.go' engine/ | grep -v _test` across all
three dialects. These three are the reason the defer phase below needs a design at all: a
workflow that reaches a terminal status this way never had a live instance, so **its registered
defers never ran** (IMPROVEMENT-PLAN §3.75). All three now run them.

### Which terminal transitions owe a defer phase, and why

The rule is **not** operator-versus-engine, which is the natural but wrong reading. It is whether a
guest is running to drain its defers:

| | who runs the defers | takes the defer phase |
|---|---|---|
| guest exits — `done`, `failed`, `dead_lettered` | the guest, on its way out of the entry point | no |
| host imposes — `terminate`, **pre-emptive `cancel`**, parent-close `TERMINATE`, force-complete, force-fail | nobody, unless the host replays a segment | **yes** |

Pre-emptive `cancel` sits on the second row for exactly the stated reason, and it is the whole of
the constraint the repository owner attached to cleat#1153: the guest is not on its way out of the
entry point, so nobody drains its defers unless the host replays a segment. Cooperative
`RequestCancellation` is not on either row — it sets a flag and changes no status, so the workflow
exits through whichever guest-exit row it chooses.

`MoveToDeadLetterQueue` is the case most often misread as a gap. It writes `dead_lettered` with a
plain `UPDATE` and no defer check, and it needs none: it is called from `writeTerminalFailure`
(`cmd/cleat-worker/setup.go`) *after* the guest has returned an error out of its entry point, so
the defers have already drained. Asserted by
`ports/dbos-transact-py`'s `test_a_defer_runs_on_a_propagated_failure_and_costs_the_run_its_dlq_place`
in the cleat-ports suite.

---

## The defer phase, and the status window it introduces

**Status: live for all three host-driven terminal transitions.** `TerminateWorkflow` (§3.112) and
the parent-close `TERMINATE` arm (§3.114) since 2026-09-04; `adminForceResolve` (force-complete and
force-fail) since 2026-09-13, when `tiers.yaml` D14 reversed D10. This section describes what they
do, and ends with what the last one costs an operator.

The durable record:

| | |
|---|---|
| `workflow_instances.pending_terminal_status` | the outcome to finalize with once the defer phase completes. `NULL` on every row today, which is what "no defer phase is owed" means. |
| `workflow_instances.defer_phase_deadline` | when the phase gives up. Separate from heartbeat staleness on purpose, and it is a different sweep: a phase whose worker *vanished* is caught by the heartbeat sweep and gets another attempt, so this one bounds the number of ATTEMPTS — a workflow cannot sit in `terminating` forever because its defers trap on every replay. Past it, `ExpireDeferPhases` applies the recorded outcome without the cleanup. |
| `terminating` | the status for the window, per the visibility condition below. |

`migrations/postgres/038_defer_phase_marker.sql`, `mysql/037`, `mssql/041`. Note the numbers do
not align across dialects and are not meant to; take the next free number above each dialect's
own high-water mark. `migrations/postgres/040` widened the cross-tenant claim function to match
the inline claims — a deployment on `--claim-across-tenants` that applied 038 without 040 would
never dispatch a defer phase at all.

**Which workflows take it.** A terminate enters the defer phase only when the workflow has
registered defers — an `EventTypeDefer` row in its history, or a compaction state, which is the
conservative answer because compaction prunes the rows it folds. A workflow with no defers has no
cleanup to run and terminates in one step exactly as before, which is every workflow in most
deployments. `engine/defer_phase.go`'s `deferPhaseOwed` is the whole of that decision.

§3.35 phases 1–4 made a workflow's `defer` bodies run on every path where a live instance exists:
success, error, and every kill the host performs. The three unfenced transitions above are the
remainder. Making their defers run requires a live instance, which requires dispatch, which
requires the workflow to be **claimable and non-terminal** — so the terminal transition splits in
two:

1. **Mark, do not finalize.** Record the intended terminal outcome; leave the workflow
   schedulable. Do *not* release its resources yet.
2. **Run a defer segment.** A worker claims it, replays history, runs the defers, and only then
   finalizes with the outcome from step 1 — at which point the resources are released, *after*
   the defers that may have released them.

### The window

Between step 1 and step 2 the workflow is **not yet terminal**. A caller that terminates a
workflow and immediately reads its status will not see `terminated` — it will see the workflow
still in flight, for as long as its defer phase takes.

**This is accepted, deliberately, and is documented rather than hidden.** The decision (2026-09-02)
is that a not-yet-terminal window is acceptable *provided it is explained and visible*. What
follows from that:

- **`terminate` is asynchronous.** It records an intent that will be honoured; it does not
  guarantee the workflow is terminal by the time the call returns. Any caller that currently
  reads status straight after terminating and expects `terminated` needs to poll.
- **So is pre-emptive `cancel`**, for the same reason and through the same two halves
  (cleat#1153). `POST /cancel` with `{"preemptive": true}` answers `{"status": "cancelled"}`,
  which names the OUTCOME rather than the current state — exactly as terminate answers
  `"terminated"` for a workflow that is at that moment `terminating`. A caller that needs to know
  the run has finished polls the status.
- **The window has its own name.** It is *not* reported as `ready`, which would be indistinguishable
  from a workflow that is simply runnable. A distinct status is what makes the state
  visible — a caller can tell "terminating, running its cleanup" from "running normally".
- **The workflow is not re-executed.** The defer segment replays history to reconstruct the
  instance and runs only the registered defers; the workflow body does not run again.
- **Bounded by a deadline.** Two different sweeps, and the distinction is the point.
  A worker that *dies* mid-phase is caught by the ordinary heartbeat sweep, which returns the
  workflow to `terminating` for another attempt. `defer_phase_deadline` bounds the number of
  attempts: past it, `ExpireDeferPhases` applies the recorded outcome without the cleanup, so a
  guest that traps on every replay cannot leave a workflow in `terminating` forever. Five minutes
  (`engine/defer_phase.go`).
- **A defer phase never fails the workflow.** Every failure inside it — a trap, a timeout,
  unreadable history, WASM that will not load, a panic — applies the recorded outcome and reports
  the cleanup as lost. Turning a terminate into a `failed` because its *cleanup* went wrong would
  replace an outcome the database had already committed to.

**Which outcome is applied depends on which transition marked the phase**, and the marker is
where that is recorded rather than something the finalize is told: `TerminateWorkflow` records
`terminated`, the parent-close arm records `failed`. One finalize, two outcomes, and nothing
between the phases can substitute a third.

### `adminForceResolve` takes this path too, since 2026-09-13

Force-complete and force-fail were terminal-and-immediate until `tiers.yaml` **D14** reversed
**D10** (cleat#1152). They now mark like the other two when the workflow owes cleanup: the
operator's intended outcome goes into `pending_terminal_status` — `'done'` or `'failed'` rather
than `'terminated'` — with the result or error written alongside it, and `FinalizeDeferPhase`
applies it once the defers have run. `FinalizeDeferPhase` sets `status = pending_terminal_status`
and does not touch `result` or `error_msg`, which is what lets a mechanism built for terminate
carry a force-*complete*'s payload.

A workflow owing no defers is still resolved in one `UPDATE`. Force-resolve has not become
asynchronous in general — only for runs with cleanup outstanding.

**What it costs, and D10 was right about it.** A defer phase requires the guest to replay, so
routing the escape hatch through one makes it depend on the thing that may already be failing:

| the workflow being force-resolved | when the operator's outcome lands |
|---|---|
| owes no defers | immediately, one `UPDATE` |
| owes defers and can replay | after its defer segment runs — seconds |
| owes defers and **cannot** replay | after `defer_phase_deadline`, up to 5 minutes |

`ExpireDeferPhases` applies the recorded outcome once the deadline passes, so an operator always
has recourse — it is no longer instant. **A status of `terminating` straight after a
force-complete is expected**; the row already carries the outcome that will be applied.

What was gained is the other half of D10's own argument. The cleat#1152 census found that
MARK/FINALIZE had **never had a defer to run** — 9,868 workflows, 47 `terminated`, every one
either never executing a body or registering none. These two endpoints were the only ones that
would ever exercise it, so under D10 the two-phase path was untested in production precisely
*because* of D10.

**Force-resolving a workflow that is already in its defer phase** still takes the one-phase arm and
clears the marker, cutting the cleanup short. `deferPhaseOwed` returns false for `terminating`, so
this is the same behaviour as a second `TerminateWorkflow`, and it is what keeps force-resolve
authoritative over a cleanup that is itself stuck. A workflow is never left terminal carrying a
marker the deadline sweep would later act on.

---

## Related

- `IMPROVEMENT-PLAN.md` §3.35 — what `defer` is supposed to be; phases 1–4 shipped.
- `IMPROVEMENT-PLAN.md` §3.75 — the durable record for the defer phase, and why the obvious
  designs are the wrong shape.
- `docs/explanation/execution-engine.md` — how a segment executes.
- `docs/troubleshooting.md` §6.2, "Engine Error Codes" — the runtime values of `error_code`, and
  which of them are retried automatically.
  **Not** `docs/reference/error-codes.md`, which this line pointed at until 2026-09-25: that file is
  the `cleat vet` *static-analysis* catalog (`E001`–`E021`, determinism violations found before a run
  exists). The two share a word and nothing else.
