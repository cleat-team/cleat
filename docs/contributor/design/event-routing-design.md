# Event Routing in cleat

**Status: PROPOSED — not implemented.** Written 2026-09-06 (WS-2). Rewritten the same
day: the first draft is superseded in three substantive ways, recorded in §11 rather
than deleted, because each was killed by an objection worth keeping.

> This is a design document, not a description of the shipped system. Read it for
> intent; check `tiers.yaml` and the source for what is true now.

---

## 0. Summary

Cleat can already route events. `plugins/eventtriggers` has subscriptions, idempotent
ingest, a filter language with a fuzz test, retry with dead-lettering, and a wait
primitive. This design **keeps all of it** and changes four things:

1. **Correlation** — an event can be bound to the specific run that is waiting for it,
   by value, not just by type.
2. **A fixed set of indexed key slots** on both the event and the awaiter, so that
   matching is an ordinary indexed equality on ordinary typed columns.
3. **Wait-for-event becomes an engine suspend**, beside `AwaitSignals`.
4. **Triggers declared in source**, deployed in the same transaction as the WASM blob.

There is **no expression language on the correlation path**, no CEL, and no query
analysis. That is the main thing this document says, and §1 and §2 are why.

---

## 1. The constraint that decides the design

Cleat's product thesis is that your existing relational database *is* the orchestration
backend. Three consequences, and every design decision below follows from them:

**We do not own the index.** The index is a `CREATE INDEX` in a migration that must
behave identically on PostgreSQL 16, MySQL 8.0 and SQL Server 2022 — all tier 1.
Systems that analyse match expressions to derive an index (Inngest normalises CEL to an
AST and builds in-memory hashmaps and B-trees keyed by xxhash) own their storage layer.
We do not, and cannot copy that.

**We cannot index inside a JSON payload portably.** The only JSON-content index in this
repo is `USING GIN (input jsonb_path_ops)` (`migrations/postgres/001_schema.sql:475`) —
PostgreSQL only, with no MySQL or SQL Server equivalent anywhere in `migrations/`. Any
design whose matching depends on indexing into an event body is either Postgres-only or
a scan on two of three tier-1 dialects.

**We cannot project at ingest what a future workflow version will want.** Workflows are
versioned and long-lived; a run suspended for 30 days spans deploys by design. If
ingest extracts fields named by the *currently deployed* versions, then v2 correlating
on a field v1 never declared finds no column on events already written, and fixing it
means a backfill — unbounded work, at deploy time, on three dialects. Worse, a v1 run
and a v2 run can be suspended on the same event type *simultaneously* with different
correlation semantics, so there is no single "current rule" ingest could have honoured.

The resolution to the third is the load-bearing idea in this document:

> **Never let the event table's physical schema depend on what any workflow version
> wants.** The slots exist before any workflow declares a meaning for them. The
> *meaning* is a property of the event type. The *value* travels on the awaiter row,
> computed by the guest of whichever version created it.

That makes mixed-version fleets correct by construction rather than by coordination.

---

## 2. Why correlation is equality and filtering is not

Two costs, and they are in different complexity classes:

| | evaluated | cost |
|---|---|---|
| **filtering** — is this one event interesting at all? | once per event, at ingest/route | **O(events)** |
| **correlation** — which waiting run does this event belong to? | per (event × awaiter) pair | **O(events × awaiters)** |

Only correlation carries the quadratic risk. So:

- **Correlation must be an indexed equality.** Nothing else is affordable, whatever
  users would prefer to write.
- **Filtering can be arbitrarily expressive and needs no index at all** — it is
  evaluated in Go over a candidate set already narrowed by `(tenant_id, event_type)`,
  which is exactly what `plugins/eventtriggers/publish.go` does today.

This argument does not depend on measuring what people write. The measurement in §10
is corroboration, and it happens to agree: **27 of 27 real correlation expressions are
equality-only; the range and negation operators appear only in filters.**

---

## 3. Model

```
  publish ──► INGEST ──► ROUTE ──────────► DELIVER
              dedupe     equality lookup    start a run   (subscription)
              persist    on key slots       resume a run  (awaiter)
                         + filter in Go
```

Ingest and route stay in the plugin. Only *resume-a-suspended-run* moves into the
engine, because that is a suspend, and suspends are engine-level.

### 3.1 Event envelope

```json
{
  "id":          "01J...",          // publisher-supplied; the idempotency key
  "type":        "payment.captured",
  "tenant_id":   "…",
  "occurred_at": "2026-09-06T12:00:00Z",
  "data":        { "orderID": "A-991", "amountCents": 4200 },
  "keys":        { "key1": "A-991" }  // the indexed slots; see §4
}
```

`occurred_at` is publisher time, kept for debugging and **not** trusted for ordering.

---

## 4. Key slots

**Three slots, string-typed, on both the event and the awaiter.**

```sql
key1  VARCHAR(128)  NOT NULL DEFAULT ''
key2  VARCHAR(128)  NOT NULL DEFAULT ''
key3  VARCHAR(128)  NOT NULL DEFAULT ''
```

One composite index per side:

```sql
CREATE INDEX ... ON ingested_events (tenant_id, event_type, key1, key2, key3, seq);
CREATE INDEX ... ON event_awaiters  (tenant_id, event_type, key1, key2, key3);
```

### 4.1 Why three, and why string

**String, because that is what correlation keys are.** Measured (§10): of 32
correlation-key references, **30 are string identifiers** — `jobId`, `userId`,
`projectId`, `tenantId`, `appointmentId`, `executionId`, `workflowRunId`,
`call_session_id` — and **zero are numeric, zero temporal**.

**Three, because two covers everything observed and one does not.** 4 of 27 real
correlations are two-key tuples; once cleat's existing `tenant_id` column absorbs the
tenant component, two user slots cover 100% of the corpus. The third is one unit of
headroom, because the workaround for running out is hand-concatenating keys, which is
the standing complaint about Zeebe's single-key restriction.

Not more than three: every slot widens the index on both tables, and **MySQL supports
neither partial nor filtered indexes**, so unused slots cannot be excluded from the
index there the way they can on PostgreSQL and SQL Server.

### 4.2 No numeric or datetime slots

Deliberately absent, for the reason in §2: nothing ever *queries by* a filter value.
A `amount > 100` predicate is evaluated in Go, once, over a handful of subscriptions.
An indexed numeric column would be paid for on every write and seeked on never.

Adding typed slots later is a purely additive migration. This is a YAGNI call with an
escape hatch, not a claim that numeric correlation is inconceivable.

### 4.3 Why 128 bytes, and why not a hash

The cap is forced by MySQL. InnoDB's index key prefix limit is **3072 bytes** (DYNAMIC
row format). Under `utf8mb4`, `VARCHAR(255)` costs 1020 bytes per column, so
`tenant_id` + `event_type` + three 255-char slots is roughly **4224 bytes — over the
limit**. The index would be rejected on MySQL while building fine on the other two.

At `VARCHAR(128)` with an ASCII-compatible collation the composite lands near 550
bytes: comfortable against MySQL's 3072, SQL Server's 1700 nonclustered limit, and
PostgreSQL's ~2704 btree row limit.

**Store the raw value; hard-error above the cap; never truncate.** A silently truncated
correlation key is a silent-never-matches bug — the failure mode this whole design
exists to prevent. A caller who genuinely needs a longer key hashes it *in the guest*,
where the semantics are known.

Hashing every key into a `BIGINT` was considered and rejected as the *primary* form: at
128 bytes it buys index width that is not needed, and it costs the ability to `SELECT`
a stuck awaiter and read what it is waiting for — in a product whose thesis is that
your data is in your database. It remains available as an additive migration if real
key distributions turn out wide.

### 4.4 Two rules that will bite if unstated

**Use `''`, never `NULL`, for an unused slot.** `NULL = NULL` is unknown in SQL, and the
portable fix (`IS NOT DISTINCT FROM`) is not portable. A sentinel empty string makes
equality behave identically on all three dialects. Low cardinality in the trailing
columns is harmless: the leading columns discriminate.

**Slots are used left to right.** A composite index serves a prefix only — correlating
on `key2` alone gets no index. Rather than three separate indexes, make "constrain
`key1` before `key2`" a rule and check it at build time.

### 4.5 Slot meaning binds to the event type, not the workflow version

If v1 puts `orderID` in `key1` and v2 decides `key1` means `customerID`, the semantic
drift of §1 returns — and worse than before, because the old rows still *look* valid.

**The slot mapping is declared with the event type. Changing a slot's meaning is a new
event type.** The publisher populates the slots, and the publisher is usually not the
workflow author, so the mapping is part of the event's published contract — the same
coordination cost as any message schema, and worth naming as a cost.

---

## 5. Correlated await

```go
evt, ok := h.AwaitEvent(cleat.EventMatch{
    Type:    "payment.captured",
    Keys:    cleat.Keys(order.ID),          // ordinary Go values, left to right
    Timeout: 24 * time.Hour,
})
```

**There is no expression language here.** The key is computed by the guest, in ordinary
Go, before suspending — so the type check is the Go compiler's, and it is free. This is
also what makes §1's version-robustness work: the awaiter row carries the *value*, so a
v1 run holding `orderID="A-991"` and a v2 run holding `paymentIntentID="pi_x"` coexist
in one table, each self-describing.

### 5.1 Suspend and resume

Mirrors `AwaitSignals`, which is the known-good path:

1. **T1** — write an `await_event` record to `event_history` and insert the awaiter row
   (with its key slots), **in one transaction**. Commit.
2. **T2** — *then* scan for an event that already arrived (§6). If found, consume and
   continue without suspending.
3. Otherwise suspend. No worker is held.
4. Publish finds the awaiter by indexed lookup, writes the payload as the *response* on
   that history record, marks the run ready.
5. Replay returns the recorded payload, deterministically, like any other step.

---

## 6. The race, and why this needs two mechanisms

### 6.1 The interleaving

The obvious implementation loses events:

```
publisher:  INSERT event .......... scan awaiters → none
run:                   scan events → none .... INSERT awaiter
```

Both correct in isolation, both commit, event unconsumed, run waits to timeout.
Making the run's writes atomic does **not** fix this: the missing conflict is between
one side's *read* and the other side's *write*.

### 6.2 Write-then-read closes it, with no locks and no SERIALIZABLE

Require each side to **commit its write before doing its read**:

- publisher: commit the event, *then* scan awaiters
- run: commit the awaiter (T1), *then* scan events (T2)

Suppose neither sees the other. The publisher missed the awaiter, so `R_a < W_a`. The
run missed the event, so `R_e < W_e`. The ordering rule gives `W_e < R_a` and
`W_a < R_e`. Chaining:

```
W_e < R_a < W_a < R_e < W_e
```

A cycle — contradiction. **At least one side always sees the other.** Both may, which
is a double delivery, resolved by making the consume a conditional `UPDATE` that only
one winner takes.

### 6.3 Why two transactions is mandatory, not stylistic

The proof holds only if the read happens in a transaction that *began after* the write
committed.

**cleat sets no isolation level anywhere** — no `sql.LevelSerializable`, no
`SET TRANSACTION ISOLATION` in `engine/` or `plugin/`. So every transaction runs at its
dialect's default, and the three disagree: PostgreSQL and SQL Server default to READ
COMMITTED, MySQL InnoDB to **REPEATABLE READ**. Any argument that depends on snapshot
timing *within* one transaction is a three-way divergence. Two transactions makes the
proof isolation-independent, which is the only version worth trusting across tier 1.

### 6.4 The hole that remains, and the sweeper

Crash between T1 and T2 and the awaiter is registered but the scan never ran. An event
that arrived *before* registration is invisible to that run forever.

**So the backward scan cannot be correct on its own.** A background sweeper must
periodically match unconsumed events against registered awaiters. That converts a
correctness bug into a latency bound.

**This is two mechanisms, not one, and the design says so here explicitly**, because an
implementer who builds only the scan will believe they are done. Precedent to follow:
`runRetentionSweep` (`cmd/cleat-worker/setup.go`) and `processBatch`
(`plugins/eventtriggers/background.go`).

**The sweeper's failure mode is inverted and must be instrumented.** Its cost is
proportional to the unconsumed set, so it gets slower exactly when delivery is broken
and it becomes load-bearing. Export unconsumed-event count and oldest-unconsumed age;
alert on the age, not the count.

### 6.5 Ordering: `seq` is safe for ordering, unsafe as a watermark

`BIGSERIAL` / `IDENTITY` / `AUTO_INCREMENT` assign at INSERT and become visible at
COMMIT, so a reader ordered by `seq` can see 10 before 9 commits. Scanning `seq > X`
therefore skips row 9 permanently — the classic outbox/CDC gap. Portable fixes are all
unpleasant (`pg_snapshot_xmin` is Postgres-only; a lag window trades latency for a
guess).

It causes no lost wakeups **provided the sweeper does not use `seq` for eligibility** —
it must re-check the unconsumed set directly. Those two uses are easy to conflate,
which is why this has its own subsection.

`received_at` is **not** an ordering key: PostgreSQL's `now()` is transaction-start
time, so commit order and `received_at` order differ whenever ingests overlap, and ties
are possible at any clock resolution. `event_history` already carries an unused
`global_seq BIGINT` column with no non-test Go referencing it — read that before
inventing a second mechanism.

### 6.6 Delivery state is a per-run cursor, not a table

An earlier draft proposed an `event_deliveries` join table, because fan-out means an
event is never globally consumed. It is not needed. A *fresh* awaiter has consumed
nothing, so "unconsumed by me" is vacuous. The only case needing memory is a run
awaiting the same type repeatedly in a loop — and that is a **per-run cursor**.

Put `last_consumed_seq` in the run's own `event_history`. It is already durable,
already replayed, already versioned with the run. That removes a table and an anti-join
whose cost would have grown without bound.

---

## 7. Triggers declared in source

```go
//cleat:entry
//cleat:trigger event="order.placed" where="event.data.region == 'us-east'"
func PlaceOrder(h cleat.HostCalls, in Order) (Receipt, error) { … }
```

`wasm/usage.go` already scans source comments for `//cleat:require` with a plain prefix
match; `//cleat:trigger` parses the same way. `cleat deploy` writes the WASM blob and
the subscription rows **in one transaction**, so a trigger versions and rolls back with
its code and appears in `git diff`.

`where` is a *filter*, so it may be arbitrarily expressive (§2) and is evaluated in Go.

**Boundary:** `cleat deploy` connects only to PostgreSQL today;
`cmd/deploy-workflow --driver mysql|mssql` is the multi-dialect path and covers deploy
only. The one-transaction guarantee must hold in both programs or it is Postgres-only,
and that must be stated the way the MySQL tenancy boundary is.

**Compatibility:** the HTTP subscription API stays. Rows gain `source`
(`'declared'` | `'api'`); deploy replaces only `'declared'` rows for the def it is
deploying, so an operator's ad-hoc subscription survives.

---

## 8. Schema changes

`event_awaiters` already exists with `PRIMARY KEY (workflow_id, event_type)`, which
means **one run cannot await the same event type twice** — in a loop, or per line item,
the second wait upserts over the first. It is altered, not created: the key becomes a
surrogate `id`.

```sql
-- altered: PK (workflow_id, event_type) -> surrogate id, plus key slots
CREATE TABLE event_awaiters (
    id          UUID PRIMARY KEY,
    tenant_id   UUID NOT NULL,
    workflow_id UUID NOT NULL,          -- was TEXT/VARCHAR(255); every other table uses UUID
    event_type  VARCHAR(128) NOT NULL,
    key1        VARCHAR(128) NOT NULL DEFAULT '',
    key2        VARCHAR(128) NOT NULL DEFAULT '',
    key3        VARCHAR(128) NOT NULL DEFAULT '',
    step        INT NOT NULL,           -- the history step to write the response onto
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at  TIMESTAMPTZ
);

ALTER TABLE ingested_events ADD COLUMN key1 VARCHAR(128) NOT NULL DEFAULT '';  -- x3
ALTER TABLE ingested_events ADD COLUMN seq  BIGSERIAL;
ALTER TABLE event_subscriptions ADD COLUMN source TEXT NOT NULL DEFAULT 'api';
ALTER TABLE event_subscriptions ADD COLUMN def_version INT;
```

MySQL and SQL Server variants follow the existing per-dialect pattern in
`plugins/eventtriggers/migrations.go`.

> **`CREATE TABLE IF NOT EXISTS` never adds a column.** The existing migrations are all
> `IF NOT EXISTS`, so these `ALTER`s must be a **new numbered migration**, and any
> long-lived test database must be dropped and recreated rather than debugged.

**Tenancy — and this is a correction, not a restatement.** RLS in this repo covers
**11** tables (`grep -rho 'ENABLE ROW LEVEL SECURITY' migrations/postgres/*.sql | wc -l`),
and **none of them is a plugin table**. `event_subscriptions`, `ingested_events` and
`event_awaiters` carry `tenant_id` columns with **no policy enforcing them** today. An
earlier draft asserted these tables "get the same policy as the eight existing tables",
which was wrong twice — wrong count, and a mechanism that has never been extended to
plugin migrations. Whether a `plugin.Migration` can install an RLS policy at all is an
open question (§9, D6), and until it is answered this design claims no
database-enforced isolation for event tables.

---

## 9. Open questions

- **D1 — eligibility window.** How far back does the §6 scan look? A run-start
  watermark is deterministic and replay-safe but can be arbitrarily old for a
  long-running or `continue_as_new` workflow, making the range large and letting stale
  events wake new steps. A time window is bounded but introduces a clock. Interacts
  with D2.
- **D2 — retention vs eligibility.** These must be two windows, not one. Retained for
  debugging ≠ eligible for delivery; conflating them means a six-month-old payload can
  wake a brand-new run.
- **D3 — `ingested_events` vs `event_stream`.** Two tables for two things both called
  "event", in two plugins (`eventtriggers`, `eventstore`). **Needs an answer before
  P1**, which alters one of them.
- **D4 — ASCII or binary slots.** UUIDs and Stripe-style IDs are ASCII, so
  `VARCHAR(128)` under an ASCII collation is fine. A non-ASCII key would need
  `VARBINARY(128)`. Cheap now, expensive after data exists.
- **D5 — declared event schemas.** Slot mapping needs somewhere to live. Does a
  declared trigger imply a declared event schema, or is the mapping standalone?
- **D6 — RLS on plugin tables.** Can a `plugin.Migration` install a FORCEd policy? If
  not, event tables are application-enforced only, and that is a documented boundary
  rather than a gap (as MySQL tenancy is).

---

## 10. The measurement behind §2 and §4

Harvested 2026-09-06. GitHub code search for Inngest `waitForEvent` expressions, which
are the closest public corpus of real correlation predicates:

```
gh api -X GET search/code -f q='"if: \"event.data" language:typescript' \
   -f per_page=100 -f page=N          # 250 unique files, 134 repos, 3 pages
```

Each file fetched raw, `if:` string literals extracted, filtered to those referencing
`event.`/`async.`, split on top-level `&&`/`||`, classified by operator.

- 269 raw extractions → **53 unique expression texts** (heavy duplication; a naive
  percentage over the raw set would have been meaningless).
- Provenance: 258 of 269 from **application code**; 6 from Inngest's own repos, 5 from
  docs/examples.

| | unique | shape |
|---|---|---|
| **correlation** (references `async.`) | 28 | **100% equality-only** |
| **filter** (no `async.`) | 25 | 72% equality; **28% use `!=`, `>`, `has()`** |

Restricted to application code: **27 unique correlation expressions across 120 repos,
27 of 27 equality-only.** Zero ranges, zero negation, zero disjunction, zero function
calls. Key types: **30 string identifiers, 0 numeric, 0 temporal.** Two-key tuples:
4 of 27.

**The selection bias runs toward the conclusion's opposite, which is why it is
credible.** Inngest's `match:` shorthand already covers single-key equality, so anyone
writing `if:` for a correlation had a reason to reach past the shorthand. This is the
population most likely to need expressive matching, and none of it does.

**Two repos ship `event.data.jobId === async.data.jobId`.** CEL has no `===` operator.
Those correlations cannot work, in application code, in production repos — the
silent-never-matches failure observed in the wild rather than hypothesised, and the
cheapest possible thing to catch at build time.

**Limits:** 27 unique correlation expressions is a small sample; GitHub code search
indexes a subset of public code and no private code; the extractor is a regex over
`if:` string literals, so it misses dynamically-built expressions and does not separate
`cancelOn` from `waitForEvent`.

---

## 11. What the first draft got wrong

Three substantive reversals in one day, each from an objection the draft could not
answer. Recorded because the reasoning is the reusable part, and because each one
**removed** machinery.

**1. "Analyse the match expression and derive an index."** Imported from Inngest, whose
index is an in-memory structure in a service that owns its storage. Ours is a
`CREATE INDEX` that must work on three dialects (§1). Removed: CEL, AST extraction,
the "fail the build if unindexable" rule, and validators R1 and R3 — the type check is
now the Go compiler's, for free.

**2. "Project declared fields into indexed columns at ingest."** Schema-on-write against
read-time requirements that drift with every deploy (§1). Removed: ingest-side
extraction rules entirely. Events are stored raw and immutable.

**3. "Split `Correlate` (indexable) from `Where` (general) and let the user choose."**
The split survives; the *justification* does not. It was asserted from intuition
(*"equality on a business key is what every real correlation is"* — an unearned
universal), then briefly abandoned when aggregate `if:` counts appeared to refute it.
Both moves were wrong and happened to cancel. The real basis is the cost asymmetry in
§2, which does not depend on the corpus at all.

Also removed: the `event_deliveries` table (§6.6), and a claim that new tables inherit
an RLS policy that does not exist for plugin tables (§8).

---

## 12. What it costs

Adding one host call is not one edit:

| | |
|---|---|
| `engine/imports.go` | wazero registration + `HostCallHandler` method |
| `engine/wasmtime_hostfuncs_workflow.go` | wasmtime registration (compared by `engine/hostabi_runtime_parity_test.go`) |
| `engine/types.go` | `EventTypeAwaitEvent` |
| `engine/compaction.go` | the `EventType ↔ EventCode` tables, **both directions** |
| `engine/events.go` | typed event + the replay switch |
| `ABI.md` | §2 entry, and the count — **do not write the number here** (see below) |
| SDKs | Go, Rust, Python, Java, AssemblyScript |
| `tests/plugin-harness` | a fixture row in each of the four language tables |

§3.312 is the worked example of skipping one: a nine-parameter import against a
ten-parameter host, which does not fail at the call — the **module fails to
instantiate**, so every call in it dies at once, and it compiled cleanly.

**On the export count specifically: carry the command, not the number.** CLAUDE.md's
rule is that any number carries a date and the command that re-derives it. For this
number that is not enough. It moved **three times on 2026-09-06 alone** — 58 → 52 when
#767 removed the durable-state family, → 50 when #843 removed the two inert signal
calls — and each intermediate value was correct when measured and stale within hours.
Two of the three were written into documentation before they aged out, including by
this author. A count with a half-life measured in hours should not appear in prose at
all:

    python3 -c "import re;print(len(set(re.findall(r'\.Export\("([^"]+)"\)',
      open('engine/imports.go').read()))))"

That is also why §14's falsification table has no row asserting a count.

---

## 13. Phasing

**P0 — fix today's semantics.** `ORDER BY received_at DESC` → ordered ascending by a
monotonic column; consume-and-record in one transaction (today's code marks consumed
and logs on failure with "Continue even if marking fails", admitting a double-consume).
Cheap *now*: `plugins` is a **tier 2** component, so nothing may describe it as
production-ready and nothing should yet depend on these semantics.

**P1 — key slots and correlation, inside the plugin.** The §8 schema, the surrogate
primary key, three slots on both tables, the composite indexes, and the `Keys`
parameter on `await_event`. **No ABI change** — it goes through `plugin_call` — but it
is a schema change including a primary-key change, so it is not free. Proves the model
before spending a host call.

**P2 — promote to an engine suspend.** The §12 checklist, plus §6's write-then-read
ordering and the sweeper. **This carries the risk**, and the earlier draft's claim that
P1 "proves the whole model" was false: P1 proves correlation, which was never the
doubtful part.

**P3 — `//cleat:trigger` and atomic deploy.**

**P4 — build-time checks.** Slot exists on the declared event type; types agree; the
left-to-right prefix rule is honoured. All trivial, all cheap.

---

## 14. How each claim is falsifiable

Prove every regression test *can* fail, and read *why* it failed.

| claim | test | falsify by | must fail with |
|---|---|---|---|
| correlation | two runs, two events, different `key1` | dropping `key1` from the predicate | the run got **the other run's event** — not merely "no event" |
| ordering | two events, one awaiter | ascending → descending | the second event delivered first |
| §6.2 race | publish concurrently with a suspend | doing the read before the write commits | the event is unconsumed and the run times out |
| §6.4 crash | kill the worker between T1 and T2 | disabling the sweeper | the run times out; with the sweeper, it resumes late |
| double consume | two awaiters, same key, one event | non-conditional `UPDATE` | both consume it |
| §4.3 cap | a 129-char key | truncating instead of erroring | **silent non-match** rather than an error |
| left-to-right | correlate on `key2` only | removing the check | builds, then scans in production |
| known-positive | a correct two-key correlation | — | **builds and matches** |
| multi-dialect | every row above | — | run on postgres **and** mysql **and** mssql |

Two rows earn their place. The correlation row demands the failure be *the other run's
event*: a run that receives nothing is consistent with four unrelated bugs, and only
the wrong-event failure proves correlation is load-bearing. The known-positive row is
not filler — a validator that rejects everything passes every negative control, and
three permissive bugs in one guard were found exactly that way (#749).

The multi-dialect row is the one the first draft omitted entirely, in a repo whose
history says dialect divergence is where the bugs live.

---

## 15. Explicitly not in this design

- **No broker.** Not Kafka, not NATS, not Redis. The database stays the only system of
  record — that is the product thesis, and it must not erode.
- **No expression language on the correlation path.** §2.
- **No inline lambda steps.** Inngest's `step.run("name", async () => {…})` is its
  best-loved API and is exactly what `checkFuncValueCall` in
  `internal/closure/closure.go` forbids. That check is the guarantee; a named durable
  function costs one line more and keeps whole-program analysis possible.
- **No HTTP-callback execution.** It would move the durability boundary outside the
  sandbox and end static analysis.
- **No change to `AwaitSignals`.** `AwaitSignals` addresses a run the sender can name;
  `AwaitEvent` addresses a run it cannot. Both are wanted.
