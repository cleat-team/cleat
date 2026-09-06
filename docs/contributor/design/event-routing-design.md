# Event Routing in cleat

**Status: PROPOSED — not implemented.** Written 2026-09-06 (WS-2).

> This is a design document, not a description of the shipped system. Read it for
> intent; check `tiers.yaml` and the source for what is true now. The same banner is
> on the two older design documents in this directory, for the same reason: a design
> doc that is mistaken for a description is worse than no doc, and this directory has
> already produced that failure once.

---

## 0. Summary

Cleat can already route events. `plugins/eventtriggers` has subscriptions, idempotent
ingest, a filter language, retry with dead-lettering, and a wait primitive. What it
cannot do is tell **which run** an event belongs to — and, because the awaiter table is
keyed on `(workflow_id, event_type)`, it cannot represent one run waiting on the same
event type twice.

Four changes, in order of value:

1. **Correlated await.** A workflow declares *which* event it is waiting for, by a key
   drawn from its own state — not just the event's type.
2. **Wait-for-event becomes an engine suspend**, beside `AwaitSignals`, instead of a
   plugin call that returns `{"found": false}` and leans on the retry policy.
3. **Triggers are declared in source** and deployed in the same transaction as the WASM
   blob, so a trigger is versioned and rolled back with the code it triggers.
4. **Match expressions are checked at build time** against real Go types.

(4) is the one that is not merely catching up with Inngest. A `waitForEvent` match that
silently never matches is the most painful failure mode in that ecosystem — nothing
errors, the run simply sits until its timeout. Cleat's analyzer already resolves the
input type, so the same mistake can be a build failure.

---

## 1. Where we are

Read from the tree on 2026-09-06. Not run — every claim below is a reading of code or
schema, and each carries the file that shows it.

### 1.1 What exists

| | |
|---|---|
| `event_subscriptions` | `tenant_id, event_type, def_name, entry_point, input_template, filter_expr, enabled` |
| `ingested_events` | `id, tenant_id, event_type, event_data, processed, error_msg` — `id` is publisher-supplied, so ingest is idempotent |
| `event_awaiters` | `workflow_id, tenant_id, event_type` |
| routes | `POST /api/events/publish`, `POST\|GET /api/events/subscriptions`, `DELETE .../{id}`, `POST /api/events/{event_id}/retry` |
| filter language | `plugins/eventtriggers/filter.go` — a MongoDB-style JSON form and a text grammar, with a fuzz test |
| background | `plugins/eventtriggers/background.go` — retry with a per-subscription `max_retries`, dead-letter |
| wait | `await_event` host function, `plugins/eventtriggers/host_functions.go` |

This is more than a skeleton. The design below **keeps all of it** and changes three
things.

### 1.2 Defect 1 — there is no correlation

`awaitEvent`'s query, verbatim (`host_functions.go`):

```sql
SELECT id, event_type, event_data, received_at
FROM ingested_events
WHERE tenant_id = $1 AND event_type = $2 AND NOT processed
ORDER BY received_at DESC LIMIT 1
```

Tenant and type. Two runs both awaiting `payment.captured` race for the same row, and
the winner is whichever polls first — not whichever the event belongs to. The API name
gives no hint: `await_event(event_type, timeout_ms)` reads as "wait for my event".

It is safe only when at most one run per tenant awaits a type at a time. That is a
severe restriction, and it is invisible.

### 1.3 Defect 2 — `ORDER BY received_at DESC` is LIFO

The **newest** matching event is consumed. Two events arriving between polls: the older
is passed over by that awaiter, and nothing returns it to that awaiter later. A
wait-for-event queue wants `ASC`.

### 1.4 Defect 3 — it polls, it does not suspend

On no match, `awaitEvent` returns `{"found": false}` and — per its own doc comment —
"the workflow engine will retry according to its retry policy". Separately, the publish
handler broadcasts a signal named `__evt:<eventType>` to rows in `event_awaiters`.

Two mechanisms for one job, and the miss path spends a whole workflow invocation to
learn nothing. `AwaitSignals` in the same engine is a proper suspend: recorded in event
history, replayed deterministically, no invocation burned.

### 1.5 Defect 4 — an awaiter row cannot represent two waits of the same type

`event_awaiters` is keyed `PRIMARY KEY (workflow_id, event_type)`
(`plugins/eventtriggers/migrations.go`). A run that awaits `payment.captured` twice —
in a loop, or in a retry, or once per line item — **upserts over its own awaiter row**.
The second wait replaces the first rather than joining it.

That key is also why correlation cannot simply be added as a column: two runs are fine
(different `workflow_id`), but the same run at two steps collides, so the primary key
has to change with it.

One more, smaller: `workflow_id` is `TEXT`/`VARCHAR(255)` here while `tenant_id` is
`UUID`/`CHAR(36)`. Every other table in the engine treats a workflow id as a UUID.

### 1.6 The ergonomic gap

To wire *order placed → run `PlaceOrder`* today: deploy the WASM, then
`POST /api/events/subscriptions` with `def_name`, `entry_point`, `input_template` and
`filter_expr`. Four stringly-typed couplings to code, in a table versioned
independently of the blob they name.

Nothing checks that `input_template` produces a value shaped like the entry point's
input. Deploy a v2 with a changed input type and the subscription still points at it.
Roll the code back and the subscription does not roll back with it.

---

## 2. Goals and non-goals

### Goals

- **G1** A publisher never needs to know a `workflow_id`. It publishes a domain event;
  routing finds the run.
- **G2** A trigger is part of the deployed artifact — versioned with it, rolled back
  with it, visible in `git diff`.
- **G3** Waiting for an event is a real suspend. In `event_history` it is
  indistinguishable in kind from `await_signals`, and replays deterministically.
- **G4** A correlation or filter expression that cannot match fails the **build**.
- **G5** Delivery is FIFO per correlation key, and each event is delivered to a given
  awaiter at most once.

### Non-goals

- **N1** Not a general pub/sub bus for non-workflow consumers. Events exist to start and
  resume workflows.
- **N2** No global ordering across event types. Ordering is per correlation key.
- **N3** No new infrastructure. The database stays the only system of record — that is
  the whole product thesis and this must not erode it.
- **N4** No change to the closure rules. No dynamic or anonymous handlers; whatever is
  added stays statically analysable. `checkFuncValueCall` and `checkInterfaceDispatch`
  in `internal/closure/closure.go` keep their current strictness.
- **N5** Signals are not replaced. `AwaitSignals` addresses a known run; `AwaitEvent`
  addresses a run that the sender cannot name. Both are wanted.

---

## 3. Model

Three layers, deliberately separable:

```
  publish ──► INGEST ──► ROUTE ──────► DELIVER
              dedupe     match         start a run   (subscription)
              persist    by key        resume a run  (awaiter)
```

**Ingest** and **route** stay in the plugin. Only **deliver-to-a-suspended-run** moves
into the engine, because that is a suspend, and suspends are engine-level.

### 3.1 Event envelope

```json
{
  "id":          "01J...",         // publisher-supplied; the idempotency key
  "type":        "payment.captured",
  "tenant_id":   "…",
  "occurred_at": "2026-09-06T12:00:00Z",
  "data":        { "orderID": "A-991", "amountCents": 4200 }
}
```

`id` is required. Re-publishing the same `id` is a no-op — today's `ingested_events`
primary key already gives this.

`occurred_at` is publisher time and is **not** trusted for ordering; `received_at` is.
Two timestamps because debugging needs the first and correctness needs the second.

---

## 4. Detailed design

### 4.1 Triggers declared in source

```go
//cleat:entry
//cleat:trigger event="order.placed" where="event.region == \"us-east\""
func PlaceOrder(h cleat.HostCalls, in Order) (Receipt, error) { … }
```

**Extraction.** `wasm/usage.go` already scans source comments for `//cleat:require`
with a plain prefix match (`const prefix = "//cleat:require "`). `//cleat:trigger`
parses the same way, into a struct the build emits alongside the module.

**Deployment.** `cleat deploy` writes the WASM blob and the subscription rows **in one
transaction**. This is the load-bearing part:

- deploying v2 re-points the trigger atomically with the code;
- rollback re-points it back;
- there is no window in which the trigger names a version that is not deployed, and no
  way to deploy code whose triggers were forgotten.

**Compatibility.** The HTTP subscription API stays. Rows gain a `source` column
(`'declared'` or `'api'`); deploy only ever replaces `'declared'` rows for the def it
is deploying. An operator can still add an ad-hoc subscription and deploy will not
delete it.

### 4.2 Correlated await

```go
evt, ok := h.AwaitEvent(cleat.EventMatch{
    Type:      "payment.captured",
    Correlate: cleat.On("event.data.orderID", "input.orderID"),
    Where:     "event.data.amountCents > 0",   // optional, non-indexed
    Timeout:   24 * time.Hour,
})
```

Two separate things, and keeping them separate is the central design decision:

| | `Correlate` | `Where` |
|---|---|---|
| shape | equality between one extracted value on each side | arbitrary predicate |
| evaluated | at suspend time (left side) and publish time (right side) | at publish time, after the index lookup |
| storage | a hashed key column on the awaiter row | a string on the awaiter row |
| cost | index lookup | evaluated per candidate |

**Why not one general predicate?** Because a general predicate cannot be indexed.
Publishing an event would have to scan every awaiter of that type and evaluate an
expression against each. Restricting correlation to *equality on an extracted value*
turns delivery into a single index hit — and equality on a business key is what every
real correlation is. `Where` remains available for the rest, applied to the small
candidate set the index returns.

**The correlation key.** At suspend time the engine evaluates `input.orderID` against
the run's own state, and stores `corr_key = hash(type, extracted_value)`. At publish
time it evaluates `event.data.orderID` and looks up `WHERE corr_key = $1`. Hashing
rather than storing the raw value keeps the column fixed-width across three dialects
and avoids putting business data in a second place.

### 4.3 Suspend and resume

`AwaitEvent` mirrors `AwaitSignals` exactly, because that path is known good:

1. Fresh execution reaches the call. The engine writes an `await_event` record to
   `event_history` (request = type + correlation key + `where`), inserts the awaiter
   row **in the same transaction**, and returns a suspend.
2. The run is suspended. No worker is held.
3. Publish matches the awaiter, writes the payload as the *response* on that history
   record, and marks the run ready.
4. Replay returns the recorded payload. Deterministic, like every other replayed step.

**Step 1's transaction boundary is the whole correctness argument.** If the history
record and the awaiter row are not written together, there is a window where the run
believes it is waiting and nothing knows to wake it — a lost wakeup that presents as a
workflow that hangs until timeout. This is the failure this design exists to prevent,
so it must not be reintroduced by the implementation.

The symmetric window — an event published *between* the run starting and reaching the
await — is handled by the same query today's code already does: on suspend, look
backwards over unconsumed events of that type with a matching key before inserting the
awaiter. If one is found, the call returns immediately and never suspends.

### 4.4 Delivery semantics

- **FIFO**: `ORDER BY received_at ASC` (today: `DESC` — §1.3).
- **At most once per awaiter**: the event is claimed with the same
  `FOR UPDATE SKIP LOCKED` model the workflow claim loop already uses, so two workers
  publishing concurrently cannot both deliver it.
- **Consume and record together**: marking the event consumed and writing the response
  onto the history record are one transaction. Today's code marks consumed and then
  logs on failure with "Continue even if marking fails", which admits a double-consume.
- **Fan-out**: one event may match many awaiters and many subscriptions. Consumption is
  per *awaiter*, not global — an event delivered to run A must still be deliverable to
  run B that correlates on the same key. This means `ingested_events.processed` is the
  wrong shape; delivery state belongs in a join table, not a boolean on the event.
- **Retention**: an event matching nothing is retained, not dropped, and swept on a TTL.
  Dropping it makes "why didn't my workflow start" unanswerable.

### 4.5 Static validation

Checked at `cleat build` / `cleat vet`. Each rule paired with what it catches:

| rule | catches |
|---|---|
| **R1** `input.X` names a field of the entry point's input type | the mis-typed correlation key — the Inngest bug that costs hours |
| **R2** `event.X` names a field of the event's declared schema, if declared | a filter that can never be true |
| **R3** both sides of a `Correlate` have the same type | `string == int64`, which silently never matches |
| **R4** a `//cleat:trigger` names an entry point in the same package | a trigger pointing at a function that was renamed |
| **R5** two `declared` triggers with the same (event type, where) on one def | ambiguous double-start |
| **R6** no schema declared for a referenced event type | **warning**, not an error — R2 cannot run, and the author should know |

R6 is deliberately a warning. Making it an error would force schema declarations on
every event before any of this is usable, and the migration path matters more than the
purity.

### 4.6 Schema changes

`event_awaiters` already exists (§1.5) and is **altered**, not created: the primary key
changes from `(workflow_id, event_type)` to a surrogate `id`, which is what lets one run
hold two waits on one type. That is a destructive migration on a tier-2 table; see the
`IF NOT EXISTS` note below.

```sql
-- altered: PK (workflow_id, event_type) -> surrogate id
CREATE TABLE event_awaiters (
    id             UUID PRIMARY KEY,
    tenant_id      UUID NOT NULL,
    workflow_id    UUID NOT NULL,
    event_type     TEXT NOT NULL,
    corr_key       BYTEA,            -- NULL = correlate on type alone (legacy)
    where_expr     TEXT DEFAULT '',
    step           INT NOT NULL,     -- the history step to write the response onto
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at     TIMESTAMPTZ
);
CREATE INDEX idx_event_awaiters_lookup ON event_awaiters(tenant_id, event_type, corr_key);

CREATE TABLE event_deliveries (      -- replaces ingested_events.processed
    event_id       UUID NOT NULL,
    awaiter_id     UUID,
    subscription_id UUID,
    delivered_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (event_id, awaiter_id, subscription_id)
);

-- changed
ALTER TABLE event_subscriptions ADD COLUMN source TEXT NOT NULL DEFAULT 'api';
ALTER TABLE event_subscriptions ADD COLUMN def_version INT;
```

MySQL and SQL Server variants follow the existing per-dialect pattern in
`plugins/eventtriggers/migrations.go`.

> **`CREATE TABLE IF NOT EXISTS` never adds a column.** Per CLAUDE.md, the existing
> migrations are all `IF NOT EXISTS`, so the `ALTER`s above must be a **new numbered
> migration**, and any long-lived test database must be dropped and recreated rather
> than debugged. This has cost time in this repo before.

**Tenancy.** All new tables carry `tenant_id` and get the same FORCEd RLS policy on
PostgreSQL and `SECURITY POLICY` on SQL Server as the eight existing tables. On MySQL
there is no RLS feature, so — consistent with the documented product boundary — MySQL
remains single-tenant and these tables are not an exception to that.

---

## 5. What it costs

Adding one host call to cleat is not one edit. The full checklist, from how
`cleat_await_signals` is wired today:

| | |
|---|---|
| `engine/imports.go` | wazero registration + the `HostCallHandler` interface method |
| `engine/wasmtime_hostfuncs_workflow.go` | wasmtime registration (the two are compared by `engine/hostabi_runtime_parity_test.go`) |
| `engine/types.go` | `EventTypeAwaitEvent` |
| `engine/compaction.go` | the `EventType ↔ EventCode` tables, **both directions** |
| `engine/events.go` | typed event + the replay switch |
| `ABI.md` | §2 entry, and the count |
| SDKs | Go, Rust, Python, Java, AssemblyScript |
| `tests/plugin-harness` | a fixture row in each of the four language tables |

**This is the argument for the phasing below.** §3.312 in `IMPROVEMENT-PLAN.md` is a
worked example of what one missed SDK signature costs: a nine-parameter import against
a ten-parameter host, which does not fail at the call — the **module fails to
instantiate**, so every call in it dies at once, and it compiled cleanly.

---

## 6. Phasing

Each phase is useful on its own and shippable as one PR.

**P0 — fix the semantics that are wrong today.** `DESC` → `ASC`; consume-and-record in
one transaction. Small. Cheap *right now*: `plugins` is a **tier 2** component, so by
`tiers.yaml`'s own contract nothing may describe it as production-ready and nothing
should yet depend on these semantics. Every week this gets more expensive.

**P1 — correlation, inside the plugin.** Add `corr_key`, the awaiter index, the
surrogate primary key from §4.6, and the `Correlate` parameter to `await_event`. **No
ABI change** — it goes through `plugin_call`, which already exists — but it *is* a
schema change, including a primary-key change on `event_awaiters`, so it is not free.
This proves the whole model against real workloads before spending a host call on it.
If the design is wrong, this is where it is cheap to find out.

**P2 — promote to an engine suspend.** The §5 checklist. Only worth paying once P1 has
shown the shape is right.

**P3 — `//cleat:trigger` and atomic deploy.** Analyzer extraction, deploy in one
transaction, the `source` column.

**P4 — static validation.** R1–R6. Needs P3 for the trigger side; the `AwaitEvent` side
can land with P2.

---

## 7. How each claim is falsifiable

Per CLAUDE.md: prove every regression test *can* fail, and read *why* it failed. A test
whose name asserts a mechanism is the one most likely to be held up by something else.

| claim | test | falsify by | must fail with |
|---|---|---|---|
| G1 correlation | two runs, two events with different `orderID` | deleting the `corr_key` predicate | the run got the *other* run's event — not merely "no event" |
| G5 FIFO | two events, one awaiter, assert order | `ASC` → `DESC` | the second event delivered first |
| G5 at-most-once | kill the worker between consume and record | removing the shared transaction | the event delivered twice, or lost |
| §4.3 lost wakeup | publish between run start and the await | splitting the history/awaiter writes | the run hangs to timeout |
| G4 R1 | a fixture with `input.orderId` (wrong case) | — | **build** fails, naming the field and the type |
| G4 known-positive | a fixture that is *correct* | — | builds clean |

The last row is not filler. A validator that rejects everything passes every negative
control; per CLAUDE.md, *"a negative control is not enough — it needs a KNOWN-POSITIVE
too"*, and three permissive bugs in one guard were found exactly that way (#749).

For the correlation test, note what the "must fail with" column demands: **not** that
the run received nothing, but that it received the *wrong* event. Those are different
failures, and only the second one proves correlation is doing the work — a run that
receives nothing might be blocked by any of four other things.

---

## 8. Open questions

- **D1** Correlate on a single key, or a tuple? Single is simpler and indexable; a
  tuple covers `(tenantID, orderID)`-shaped keys without a synthetic concat. Proposal:
  single for P1, tuple deferred until something needs it.
- **D2** TTL for events that match nothing. Retention is cheap and "why didn't it
  start" is a common question. Proposal: retain 30 days, configurable, swept by the
  existing background loop.
- **D3** `ingested_events` (eventtriggers) and `event_stream` (eventstore) are two
  tables for two ideas both called "event". Either they are an ingest buffer and a
  durable stream and should say so in both packages, or one should go. **This needs a
  decision before P1**, because P1 adds a delivery table next to one of them.
- **D4** Should `Where` share `plugins/eventtriggers/filter.go`'s grammar, or should
  correlation use a narrower expression language that is easier to type-check? The
  filter grammar is fuzz-tested, which argues for reuse; but R1–R3 need type
  resolution, which that grammar has no notion of.
- **D5** Does a declared trigger imply a declared event schema? R6 says no (warning).
  Revisit once there is usage.

---

## 9. Explicitly not in this design

- **No broker.** Not Kafka, not NATS, not Redis. N3.
- **No inline lambda steps.** Inngest's `step.run("name", async () => {…})` is its
  best-loved API and is exactly what `checkFuncValueCall` forbids. That check is the
  guarantee; a named durable function costs one line more and keeps whole-program
  analysis possible.
- **No HTTP-callback execution.** It would move the durability boundary outside the
  sandbox and end static analysis.
- **No change to `AwaitSignals`.** Different primitive, different job (N5).
