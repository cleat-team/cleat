# Schema partitioning and migration rebaseline

**Status:** proposal, open for review. Decisions 1, 2, 4 and 5 resolved; Decision 3 (plugin
isolation scope) partly resolved — mechanism shipped in cleat#1280, remainder blocked on cleat#1278.
**Drafted:** 2026-09-11, against `develop` at `6b7f0790`.
**Measurement environment:** `postgres:16` and `mcr.microsoft.com/mssql/server:2022-latest`
containers, stock configuration (`maintenance_work_mem` 64MB, `shared_buffers` 128MB).

Partition the large tenant-scoped tables at schema-creation time, compact 43 migrations back into
three files per dialect, and take the migration step out of worker boot. With no deployments in the
field, all three are free to do now and expensive to retrofit later.

Every number below carries the command that re-derives it. Counts of a growing population —
table sizes, statement counts, partition timings — should be re-derived rather than quoted from
here.

---

## Contents

- [Why now](#why-now)
- [What the current schema costs](#what-the-current-schema-costs)
- [What partitioning does, precisely](#what-partitioning-does-precisely)
- [Table-by-table: what is worth partitioning](#table-by-table-what-is-worth-partitioning)
- [How many partitions is too many](#how-many-partitions-is-too-many)
- [Compacting 43 migrations into three](#compacting-43-migrations-into-three)
- [Plugin schemas](#plugin-schemas)
- [The other dialects](#the-other-dialects)
- [Prerequisites](#prerequisites)
- [Open decisions](#open-decisions)
- [Work plan](#work-plan)

---

## Why now

Every worker applies migrations at boot and exits on failure
(`cmd/cleat-worker/main.go:680-689`), so a migration is not a deploy step that can fail in
isolation — it is a condition of the fleet starting at all. That is survivable at current scale and
becomes the dominant uptime risk at a hundred tenants sharing one set of RLS-scoped tables, where
tenant count multiplies table size rather than splitting the work.

Partitioning does not fix all of that. It makes the **expensive** operations per-tenant while the
cheap ones stay global — and since the cheap ones are cheap at any size, that is the right split.

---

## What the current schema costs

Fixture: 1M `workflow_instances` (376 MB), 5M `event_history` (2.7 GB), 500k `idempotency_keys`,
200k `workflow_signals`.

Applying the real 040→062 upgrade against that database took **8.9s across 22 migrations**. Only
`051` was notable at 5.64s — a whole-table backfill joining `workflow_instances`. That is
reassuring about these particular migrations and says nothing about the next one, so the useful
number is the per-operation cost:

| Operation | Rows | Time | Lock taken |
|---|---|---:|---|
| `ADD COLUMN` nullable | 1M | 0.29s | ACCESS EXCLUSIVE, metadata only |
| `ADD COLUMN NOT NULL DEFAULT <const>` | 1M | 0.08s | ACCESS EXCLUSIVE, metadata only |
| `CREATE OR REPLACE FUNCTION` | — | 0.07s | trivial |
| `ADD CHECK` constraint | 1M | 0.29s | ACCESS EXCLUSIVE + full scan |
| `CREATE INDEX` | 1M | 0.68s | SHARE — blocks writes |
| `CREATE INDEX` | 5M / 2.7GB | 5.76s | SHARE — blocks writes |
| `ADD COLUMN` volatile default (rewrite) | 1M | 5.00s | ACCESS EXCLUSIVE, full rewrite |
| `ALTER COLUMN TYPE` (rewrite) | 1M | 8.02s | ACCESS EXCLUSIVE, full rewrite |

### It scales worse than linearly

The same `CREATE INDEX` on `event_history`: **5.76s at 5M rows (2.7 GB)**, **52.8s at 20M rows
(11 GB)**. Four times the data, 9.2× the time — a 64MB `maintenance_work_mem` forces an external
merge sort.

> Caveat: the 20M measurement ran on a filling disk and the confirming re-run was lost to disk
> exhaustion. Treat the superlinearity as *indicated*, not confirmed. It means a 100-tenant table
> cannot be estimated by extrapolating the small case.

### Three failure modes, worsening

| What happens | Measured | Consequence |
|---|---|---|
| A heartbeat-shaped `UPDATE` during an ACCESS EXCLUSIVE DDL | DDL 5.88s → stall **5.82s** | The stall equals the whole DDL. No errors, just blocked. |
| A metadata-only `ALTER` queues behind a 10s open reader | 0.08s → **9.43s**; `SELECT`s arriving after it 7.8–8.4s | Outage length is set by your longest open transaction, not by the DDL. Unbounded, since **no `lock_timeout` exists anywhere in the repo**. |
| A rewrite runs out of disk (needs 2× table size free) | `could not extend file`; clean rollback, data intact | Migration failure at boot is `os.Exit(1)` on **every** worker. The fleet does not start, rather than starting slowly. |

### The ten-second cliff

The reaper treats a workflow as stale after `max(2×heartbeat, 10s)` — ten seconds by default
(`cmd/cleat-worker/setup.go:2106`; heartbeat 5s at `config.go:73`). During a DDL the reaper is
blocked too; when the lock releases it sees the entire running set stale and reclaims it in one
statement.

Nothing is lost — event history is durable — but every in-flight workflow loses its fence and
replays at once, on a cold cache. **Any DDL over ten seconds on `workflow_instances` costs a
thundering-herd replay on top of the stall.**

---

## What partitioning does, precisely

PostgreSQL declarative partitioning: the table becomes a parent definition plus child tables, rows
routed automatically by `tenant_id`. Each child is a real relation with its own files, indexes, and
**its own locks**. That last property is the entire value.

Measured against a 4-partition fixture with live writers:

| Property | Result | Reading |
|---|---|---|
| Application SQL | **Unchanged** | 100k rows inserted through the parent, routed correctly. No routing code; the query sites are untouched. |
| RLS enforcement | **Intact** | Policy on the parent covers children. As a non-superuser: tenant 1 saw 100000 rows, tenant 2 saw **0**. |
| Pruning, RLS predicate only | Runtime | `Subplans Removed: 2` — planner keeps all partitions, executor discards. |
| Pruning, explicit `tenant_id` | Plan-time | No `Append` node at all — straight to the one child. |
| Per-partition index build (`ON ONLY` + `CONCURRENTLY` + `ATTACH`) | **0.07s writer latency** | Zero failures; parent index ends `indisvalid = t` with all partitions attached. Compare 5.82s fully blocked. |
| Partition-level `ALTER` | **0.10s for other tenants** | Blast radius is one tenant. |
| Parent-level `ADD COLUMN` | **3.06s for other tenants** | **Recurses.** `AccessExclusiveLock` on the parent and every child at once. |
| Grants | **Per-relation** | `GRANT ... ON event_history` did not cover children — `permission denied for table event_history_t3`. |

### RLS does not propagate to partitions — a hard constraint on this design

**Measured 2026-09-11, and it changes what the partitioned schema must emit.** Setting
`ENABLE`/`FORCE ROW LEVEL SECURITY` and a policy on the partitioned *parent* leaves every partition
unprotected:

```
relname  relrowsecurity  relforcerowsecurity
eh       t               t
eh_p0    f               f          <- partitions inherit neither
eh_p1    f               f
```

Read through the parent, RLS applies. Read a partition **directly**, it does not. As the table's
owner with tenant 1 in context, against 400 rows spread over 4 tenants:

| read | rows returned | |
|---|---:|---|
| through the parent `eh` | 100 | correct — tenant 1's rows |
| direct to `eh_p0` | 100 | **unfiltered** — and none of them tenant 1's |
| direct to `eh_p1` | 300 | **unfiltered** — three other tenants' data |

**Note what `eh_p0` does there, because it is the reason this is worth writing down.** It returned
100 rows — exactly tenant 1's row count — while containing a *different* tenant's rows entirely. A
spot-check against that partition returns a plausible number and a wrong answer. Only `eh_p1`, with
its 300, shows the leak. Checking one partition would have confirmed the wrong conclusion.

This was invisible to the earlier verification in this document ("RLS enforcement: intact, tenant 1
saw 100000, tenant 2 saw 0") for two compounding reasons: that test read through the **parent**, and
it connected as a **non-owner**, for whom `ENABLE` alone suffices and `FORCE` is unobservable. See
[cleat#1283](https://github.com/cleat-team/cleat/issues/1283), which is the same defect found
independently in a plugin test.

**Three conditions have to hold at once for an RLS check to mean anything, and each alone is
satisfied by a table with no protection on it:**

| | otherwise |
|---|---|
| Connect as the table's **owner** | `ENABLE` alone binds a non-owner, so `FORCE` is unobservable |
| …and that owner must **not be a superuser** | a superuser bypasses RLS unconditionally, so the check measures the bypass and reports nothing |
| Address the **object whose property you are claiming** | reading a partitioned parent answers a question about the parent, not about the partition holding the rows |
| The table must **contain a row the policy should exclude** | a policy's `USING` clause is a row-level predicate; against zero rows it is never evaluated and the read succeeds whether the policy is correct, wrong, or **absent** |

The middle row is not hypothetical for cleat: the shipped configurations connect as superuser often
enough that `engine.CheckRLSEnforced` exists to detect it. A test written against the real owner of
a cleat table would frequently be measuring a superuser, and would pass against a table with the
policy deleted. The measurement above used a deliberately `NOSUPERUSER` owner for that reason.

**What the schema must therefore do:** emit `ENABLE`, `FORCE` *and* the policy on **every
partition**, not only the parent — and the per-partition step has to run whenever a partition is
created, which under the hash decision is at schema-creation time rather than per tenant. Verified:
with all three applied per partition, direct reads filter correctly (`eh_p0` → 0, `eh_p1` → 100 for
tenant 1). `ENABLE`+`FORCE` without a policy default-denies, which is safe but useless.

**Partial mitigation, not a substitute.** Grants do not propagate either, so a role granted only on
the parent cannot address a partition by name. That leaves the owner — always — and any role granted
via `GRANT ... ON ALL TABLES IN SCHEMA`. Relying on the grant gap would make tenant isolation a
property of what nobody happened to grant.

So partitioning does **not** make `ADD COLUMN` per-tenant. That is acceptable because `ADD COLUMN`
is metadata-only and costs a fraction of a second at any size — its risk is lock *acquisition*, and
the fix for that is `lock_timeout`, not partitioning.

What it buys, in order of value:

1. **Index builds** become non-blocking and per-tenant.
2. **Backfills** run one child at a time, throttled — the `051`-shaped migration.
3. **Rewrites** need one tenant's free space, not the whole table's.
4. **Retention** becomes `DROP PARTITION`, O(1) metadata.
5. **`drop_tenant`** becomes a `DETACH` — with LIST partitioning only; see the decision below.

The index-build pattern, which is the one that matters:

```sql
CREATE INDEX ix_eh ON ONLY event_history (created_at);              -- 0.05s, invalid marker
CREATE INDEX CONCURRENTLY ix_eh_pN ON event_history_pN (created_at); -- per partition
ALTER INDEX ix_eh ATTACH PARTITION ix_eh_pN;                         -- 0.04-0.05s each
```

---

## Table-by-table: what is worth partitioning

The criterion is not size alone. A table is worth partitioning when its *maintenance cost* scales
with tenant count **and** it is never read across tenants on a hot path. Primary keys read from a
live catalog after applying all 43 migrations.

| Table | Drives row count | Bounded? | Cross-tenant reads | `tenant_id` in PK | Candidate |
|---|---|---|---|---|---|
| `event_history` | workflows × steps | pruned by compaction + retention | none — always keyed by `workflow_id` | **no** — `(workflow_id, step)` | **Strong** |
| `workflow_instances` | workflow runs | retention only | **yes** — `claim_workflows`, reaper, schedules | no — `(id)` | **No**, see below |
| `workflow_defs` | deployed versions | grows slowly | none | yes | Special case — few rows, **large bytes/row** (`wasm_bytes`) |
| `idempotency_keys` | keyed starts × 7d TTL | TTL-bounded | none | yes | Moderate |
| `workflow_promises` | promises per workflow | with the workflow | none | no | Moderate |
| `workflow_update_requests` | update requests | with the workflow | none | no | Moderate |
| `workflow_signals` | in-flight signals | **yes** — a queue, deleted on consume | none | no | Weak |
| `concurrency_keys` | in-flight limited workflows | **yes** — 13 delete sites | none | yes | Weak |
| `workflow_memory_samples` | samples per definition | **yes** — capped per def | none | no | Weak |
| `workflow_schedules` | one per schedule | tiny | **yes** — `get_due_schedules` | yes | No |
| `workflow_memory_stats` | one per (tenant, def) | tiny | none | yes | No |
| `workflow_tags`, `workflow_routing`, `plugin_defs`, `tenant_settings` | metadata | tiny | none | mixed | No |

### Why `workflow_instances` stays unpartitioned

`admin.claim_workflows` is a single query with a fixed 15-column contract
(`engine/store_lifecycle.go:1278`, scanned in exactly one place at `:1200` so it cannot drift). It
is deliberately cross-tenant, so partitioning it would put an all-partition plan on the hottest
path in the system.

This costs nothing: it is the *small* table. 376 MB against `event_history`'s 2.7 GB at only five
events per workflow, and that ratio widens with history length. **The migration pain is
concentrated in exactly the tables that can be partitioned.**

### Partition-key readiness

Seven of fifteen tables already carry `tenant_id` in their primary key — migrations 010, 035, 036,
037, 056 and 057 put them there. The schema has been trending toward partition-readiness for a
while. The notable exception is the table we most want to partition:
`event_history_pkey PRIMARY KEY (workflow_id, step)`. With no deployments, changing it to
`(tenant_id, workflow_id, step)` is free.

Re-derive:

```sql
SELECT c.relname,
       pg_get_constraintdef(k.oid) LIKE '%tenant_id%' AS tenant_in_pk,
       pg_get_constraintdef(k.oid)
FROM pg_constraint k
JOIN pg_class c ON c.oid = k.conrelid
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE k.contype='p' AND n.nspname='public'
ORDER BY 2 DESC, 1;
```

---

## How many partitions is too many

Measured through `lib/pq` with the exact call style the engine uses — `db.Query` with bind
parameters, no `*sql.Stmt` reuse — on a warm pooled connection. Median of 15 after 5 warm-up runs.
Partitions deliberately hold little data so the measurement isolates planning from execution.

| Partitions | **A** — `tenant_id` in the predicate | **B** — RLS supplies the tenant | B/A |
|---|---:|---:|---:|
| none (control) | 0.435 ms | 0.354 ms | 0.8× |
| 8 | 0.230 ms | 0.327 ms | 1.4× |
| 32 | 0.249 ms | 0.464 ms | 1.9× |
| **64** | 0.254 ms | **0.618 ms** | 2.4× |
| 128 | 0.238 ms | 0.989 ms | 4.2× |
| 512 | 0.497 ms | 4.702 ms | 9.5× |
| 1024 | 0.269 ms | 8.207 ms | 30.5× |

**Column A is flat** — a query naming its `tenant_id` prunes at plan time and costs the same at
1024 partitions as at 8. **Column B is linear at roughly 7.8 µs per partition** — the planner locks
and considers every partition before the executor discards them.

### The bound, stated as a budget

There is no single number; there is a number *per planning budget*, and it only binds on queries
that do not name their tenant:

- 0.5 ms of planning on a replay read → about **64 partitions**
- 1 ms → about **128**
- 5 ms → about **512**, already 10× the unpartitioned read

For queries that *do* name their tenant, planning imposes no practical bound — 1024 was still
0.27 ms.

### Two findings that matter more than the number

**1. Adding `tenant_id` to a predicate is worth more than any partition-count choice.** It is the
entire gap between column A and column B — 30× at 1024, still 2.4× at 64. See
[the work item](#work-plan); it is nine one-line changes.

**2. The cost does not amortise, because of the driver.** A PostgreSQL `PREPARE`d statement
switches to a generic plan from the sixth execution and planning collapses — 10.83 ms → **0.18 ms**
at 1024 partitions, with `Subplans Removed: 1023` confirming runtime pruning still works. Cleat
never gets this: `lib/pq` creates and destroys a named statement per call, so
`pg_prepared_statements` reads **0** after any number of queries, and every execution pays full
planning. Statement reuse — or a driver with a statement cache — is a lever on this bound entirely
independent of partitioning.

> Cold connections are far worse: the first query on a fresh connection measured 14 ms at 64
> partitions and 61 ms at 1024. Pool churn and worker restarts amplify this.

### The nine predicates

PostgreSQL statements touching `event_history` with no tenant predicate. Twenty of the
twenty-nine already carry one; these nine do not:

| Site | Path | What it is |
|---|---|---|
| `engine/store_event_write.go:183` | **hot** | replay checksum read — the hottest of the nine |
| `engine/defer_phase.go:84` | **hot** | `deferPhaseOwedSQL` correlated `EXISTS` |
| `engine/store_event_stream.go:170` | **hot** | event streaming read |
| `engine/db.go:646` | sweep | `CompactHistory` DELETE |
| `engine/db.go:1167` | sweep | retention DELETE via subquery |
| `engine/db.go:1296` | sweep | status + `EXISTS` probe |
| `engine/db.go:1481` | sweep | `DeleteDeadLetteredWorkflows` |
| `engine/db.go:1574` | sweep | `DeleteCompletedWorkflows` |
| `engine/store_admin.go:359` | admin | admin force audit read |

Re-derive (pair backticks first, then filter — a bound applied *during* pairing re-phases the rest
of the file):

```python
import re, subprocess
files = [f for f in subprocess.run(['git','ls-files','engine/*.go','cmd/**/*.go'],
         capture_output=True,text=True).stdout.split() if not f.endswith('_test.go')]
for f in files:
    if 'mysql' in f or 'mssql' in f: continue
    src = open(f).read()
    for m in re.finditer(r'`([^`]*)`', src):
        q = m.group(1)
        if 'event_history' not in q or 'tenant_id' in q: continue
        if not re.search(r'\b(SELECT|INSERT|UPDATE|DELETE)\b', q, re.I): continue
        start = src.rfind('\n', 0, m.start()) + 1
        if src[start:m.start()].lstrip().startswith('//'): continue   # prose about SQL is not SQL
        print(f, src[:m.start()].count('\n')+1)
```

> That comment filter is load-bearing. A first pass counted 18, then 12; two of those were
> sentences *describing* SQL inside comment blocks (`defer_phase.go:137`,
> `store_event_shadow.go:16`) and one was the MySQL arm in a shared file
> (`store_admin.go:471`, inside `func (s *MySQLStore) adminAppendAudit`). A text search cannot tell
> a thing from a sentence about the thing.

---

## Compacting 43 migrations into three

The target is the shape the tree already had before it accreted forty files — `001_schema.sql`'s
own header records the same move being made once before (*"Combines: 001_tables, 002_constraints,
005_priority, …"*):

```
001_schema.sql      tables (partitioned), indexes, RLS policies, grants
002_defaults.sql    seed data
003_procedures.sql  the 10 routines, final bodies only
```

The hazard is small but sharp. Four of ten routines are redefined across migrations, and each must
be taken at its **last** definition. Picking the wrong one ships an older body and fails silently:

| Routine | Definitions | Authoritative |
|---|---:|---|
| `finalize_workflow_status` | 8 | `053_the_finalize_procedure_stops_writing_the_result_column.sql` |
| `admin.drop_tenant` | 3 | `059_a_dropped_tenants_definitions_go_with_it.sql` |
| `admin.claim_workflows` | 3 | `055_a_run_records_when_a_worker_began_executing_it.sql` |
| `cleat.assert_tenant_set` | 2 | `034_assert_tenant_set_empty_string.sql` |

Re-derive (strip comments first, or a header quoting a `CREATE` counts as a definition):

```python
import re, glob, os, collections
defs = collections.defaultdict(list)
for f in sorted(glob.glob('migrations/postgres/*.sql')):
    src = re.sub(r'/\*.*?\*/', '', open(f).read(), flags=re.S)
    src = '\n'.join(re.sub(r'--.*$', '', l) for l in src.split('\n'))
    for m in re.finditer(r'CREATE\s+(?:OR\s+REPLACE\s+)?(?:FUNCTION|PROCEDURE)\s+([A-Za-z0-9_.]+)', src, re.I):
        defs[m.group(1).lower()].append(os.path.basename(f))
for name, fs in defs.items():
    if len(fs) > 1: print(name, '->', fs[-1])   # the LAST is authoritative
```

### Differential verification is non-negotiable

Compaction is a silent-regression machine and reading the SQL cannot verify it. The only
trustworthy check:

1. Build database **A** by applying `001`→`062` in sequence.
2. Build database **B** from the three compacted files.
3. Require an **empty** difference across columns, indexes, constraints, RLS policies, grants and
   `pg_get_functiondef` bodies.

Both must be **scratch databases**, never a shared one — see the work plan for why
([#1281](https://github.com/cleat-team/cleat/issues/1281)).

**Plugins run against neither side. This is a decision, and the harness must assert it rather than
assume it.** The compaction under test is of `migrations/` only; `plugin.RunMigrations` is a
separate path that creates its own tables *and, since
[#1280](https://github.com/cleat-team/cleat/pull/1280), its own policies* from
`plugin.Migration.TenantScoped`. Let plugins run against one database and not the other and you get
`kv_store`, a `pg_policy` row, and two `pg_class` booleans as spurious differences. So: no plugin
migrations on either side, and a precondition check that neither database contains a plugin object,
because "I did not run them" is exactly the kind of assumption that turns out to be wrong on someone
else's machine.

**Compare `relrowsecurity` and `relforcerowsecurity` explicitly, not just `pg_policy`.** A table can
carry an identical policy and differ in whether `FORCE` is set, and that difference is invisible to
any comparison of columns, indexes and policy rows — while being exactly the difference between a
policy that binds the owner and one the owner silently bypasses. This is not hypothetical: earlier
in this review a superuser connection read 100000 rows straight through a policy that looked correct
in `pg_policy`. A compaction that dropped a `FORCE` would be a serious regression that a
tables-and-columns diff reports as clean.

And the harness needs a **known-positive**: it must be shown *catching* a deliberately wrong
compaction — substitute `003`'s `finalize_workflow_status` body for `053`'s and confirm it fails. A
harness that has only ever passed is a claim, not a check. Add a second known-positive for the
paragraph above: drop a `FORCE` in one database and confirm the diff reports it.

> If the plugin migrations are rebaselined too (work plan step 5), that needs its **own**
> comparison, with plugins run against both sides — not this one widened. The two have different
> inputs and different authoritative sources.

**Do not supplement this with a behavioural isolation spot-check. It would look stronger and be
weaker.** Both databases here are freshly built and therefore empty, and a policy's `USING` clause
is a row-level predicate: against zero rows it never evaluates, so a read-side assertion succeeds
against a table whose policy is wrong or missing entirely. Measured elsewhere on 2026-09-11 —
`SELECT count(*)` through two different adapters both succeeded on an empty table and diverged the
moment one row was seeded (`cleat.tenant_id is not set`, P0001). The catalog comparison does not
have this failure mode, which is the argument for keeping it primary rather than adding a
behavioural check beside it. If a behavioural assertion is ever wanted, it must seed a row belonging
to a *different* tenant first — the row the policy is supposed to exclude.

---

## Plugin schemas

Eighteen of twenty-two plugins ship migrations, creating roughly twenty-five tables. They are not
files: they live in Go as `Migrations()` with `Up` / `UpMySQL` / `UpMSSQL` variants, tracked in
`plugin_migrations` and applied by `plugin.RunMigrations` right after the core migrations at boot.

**The isolation gap is tracked separately as [cleat#1277](https://github.com/cleat-team/cleat/issues/1277)**
— no plugin creates an RLS policy, and the `RegisterPluginTables` / `grant_plugin_to_tenant` path
that would have scoped them has no caller and targets a schema the tables are not in. That is not
blocked by this plan and should not be folded into it.

For *this* plan, two conclusions:

**Separate deploy: yes, but not the same mechanism.** A standalone migrator reads core migrations
from a directory; plugin migrations are compiled into a binary, so the migrator must be *built with
the same plugin set as the workers*. The practical form is a mode of the existing binary
(`cleat-worker --migrate-only`), not a separate tool. Two changes make it safe:

- **Fail closed on plugin health** during a migrate-only run. Today only loaded and healthy plugins
  are migrated (`plugin/migration.go:237`); an unhealthy one silently leaves its schema unbuilt
  while workers that *do* load it run against missing tables.
- **Record skipped separately from applied.** A missing dialect variant is skipped *and recorded as
  applied* (`plugin/migration.go:277-295`), so a variant added later can never run. (The related
  "declaration is recorded then discarded" problem was cleat#1157, fixed by #1273.)

**Partitioning: no, and do not push partition DDL into the plugin API.** Most plugin tables are
config-shaped with one row per tenant. A handful are genuine append-logs —`event_stream`,
`audit_events`, `kv_store`, the blobstore pair, and notably `webhook_events` and `ingested_events`,
which have **zero delete sites: unbounded growth with no retention at all**. But plugin authors
hand-write three dialect variants each; asking them to reason about partition keys and `ATTACH`
ordering is an interface that will be got wrong quietly. If those tables need it later, express it
**declaratively** — a `PartitionBy: "tenant_id"` field the runtime translates per dialect — so the
author states intent and the runtime owns the physical layout.

Since plugin migrations are Go rather than SQL, the equivalent of the three-file compaction is
**collapsing each plugin to a single version 1**. Only safe because there are no deployments, and
it clears the skipped-versus-applied mess at a stroke.

---

## The other dialects

SQL Server has partitioning and it works on every edition — but partitions are **not separate
objects**, so there is no equivalent of the per-partition `CONCURRENTLY` + `ATTACH` pattern.
Rebuilding one tenant's partition offline blocked readers of an *untouched* tenant for **8.09s**;
online kept traffic moving but still stalled 1.33s.

The online option is edition-gated:

```
Msg 1712: Online index operations can only be performed in
          Enterprise edition of SQL Server or Azure SQL Edge.
```

### Two CI traps worth fixing regardless of this plan

- The cleat test container reports **Developer Edition, EngineEdition 3** — the full Enterprise
  feature set. A green CI test of `ONLINE = ON` proves nothing about a customer on Standard, where
  the same statement is a hard refusal.
- The container creates databases with `is_read_committed_snapshot_on = 0`, and cleat never
  configures it. Azure SQL **Database** defaults it **on** (Managed Instance does not). Flipping
  only that setting: a reader hitting a row an uncommitted writer holds went from **3.19s blocked**
  to **0.17s**. The MSSQL claim path is built on `READPAST, UPDLOCK, ROWLOCK` hints
  (`engine/mssql_lifecycle.go:146, 193, 354, 555`), so every MSSQL concurrency test currently runs
  under an isolation regime the intended target does not use.

Also note: the migration advisory lock is Postgres-only (`migration/runner.go:200` returns the pool
unchanged for the others), so N workers booting against SQL Server apply DDL concurrently with no
serialisation. The code comment is explicit that this is deliberate given no multi-worker topology
ships for it — a bounded known gap, but it outranks partitioning if that changes.

**Recommendation:** design properly for Postgres; hold the other dialects to expand/contract plus
`SWITCH`-style swaps, verified on a scheduled real-service run rather than per-PR.

---

## Prerequisites

| Change | Why | Status |
|---|---|---|
| **Non-transactional migration support** (`migration/runner.go:343`) | Each file runs in one transaction, so `CREATE INDEX CONCURRENTLY` is structurally impossible — `cannot run inside a transaction block`. The whole per-partition maintenance story depends on lifting this. | **Blocking** |
| **Set `lock_timeout` on the migration session** | Zero occurrences in the entire repo. Without it one stray `idle in transaction` turns a 0.08s `ALTER` into an unbounded read outage. Cheapest fix, largest single win. | **Blocking** |
| **Migration as a deploy step, not worker boot** | Today the first new worker to boot applies DDL while old workers serve traffic, and a failure kills every worker's boot rather than one job. Workers should *verify* a supported schema range, not apply. | **Blocking** |
| **Add `tenant_id` to nine PostgreSQL predicates** | Converts them from runtime to plan-time pruning: 0.62 ms → 0.25 ms at 64 partitions, 8.2 ms → 0.27 ms at 1024. Raises the ceiling from ~64 to past 1024. Worth more than the bucket-count choice itself. | Measured |
| **Partition-aware grants** | `admin.create_tenant_role` and `grant_plugin_to_tenant` must cover child relations. Fails closed and noisily, which is the good direction. | From source |
| ~~Tenant onboarding gains a partition step~~ | Not required under the hash decision. Recorded because the claim was overstated earlier: `CREATE TABLE ... PARTITION OF` does block every other tenant (3.00 s), but the two-step `CREATE` + `NOT VALID` CHECK + `VALIDATE` + `ATTACH PARTITION` measured 0.10 s and blocks nobody. Relevant only if Decision 1 is ever revisited. | Measured |

---

## Open decisions

### Decision 1 — RESOLVED: hash, 64 buckets

**Re-examined after the 512-tenant cap was set (2026-09-11), because that cap weakens the original
argument.** The first version of this decision rested on LIST-per-tenant putting partition count on
an unbounded curve. With a deployment cap of 512 tenants it is not unbounded, and two further
measurements made LIST more attractive than it first looked:

- At 512 partitions, a query naming its `tenant_id` plans in **0.497 ms** — about 2× hash-64's
  0.254 ms, but small in absolute terms.
- **Tenant onboarding need not block anyone.** `CREATE TABLE ... PARTITION OF` blocks every other
  tenant for the length of its transaction (measured: 3.00 s hold → other tenants' reads blocked
  3.00 s). But `CREATE TABLE` standalone, plus a `CHECK` constraint added `NOT VALID` and then
  `VALIDATE`d, then `ALTER TABLE ... ATTACH PARTITION`, measured **0.10 s** for unrelated tenants —
  no blocking at all. `ATTACH PARTITION` takes only `SHARE UPDATE EXCLUSIVE`, and the pre-validated
  constraint lets it skip the scan.

So the honest position is that this is closer than it first appeared. **Hash 64 still wins, on
failure mode rather than on best case:**

| | hash, 64 buckets | LIST, up to 512 |
|---|---|---|
| planning, `tenant_id` given | 0.254 ms | 0.497 ms |
| planning, predicate missing | **0.618 ms** | **4.702 ms** |
| maintenance unit | 1/64 of the table; a large tenant sets its bucket's floor | exactly one tenant |
| `drop_tenant` | `DELETE` | `DETACH`, O(1) |
| onboarding | free — no DDL | 0.10 s via the two-step attach |

The deciding column is the second one. A missing `tenant_id` predicate costs 0.62 ms under hash and
4.70 ms under LIST — 7.6× worse, on the replay hot path. That is not a one-time risk: new queries
get written, and the nine predicates in this document are evidence that the codebase drifts toward
relying on RLS. **Hash degrades gracefully under exactly the mistake this codebase has already
made nine times; LIST punishes it.**

The price accepted: `drop_tenant` stays a `DELETE`, and one large tenant sets the floor for its
bucket's maintenance cost. Both are capacity and retention concerns rather than availability ones —
per-bucket index builds are non-blocking either way.

**64 rather than 32**, because bucket count is expensive to change later (re-hashing moves data) and
the measured cost of the larger number is 0.004 ms on the plan-time-pruned path. Distribution of 512
tenants over 64 buckets, modelled over 2000 trials: mean 8 per bucket, median worst bucket 15, p95
worst 18 — about 2.2× the mean. Note that this is *count* skew; *size* skew is unbounded and hash
cannot mitigate it, which is the real cost above.

**Flip condition.** If per-tenant `DETACH`-based retention, per-tenant data residency, or per-tenant
restore become requirements, LIST is affordable at a 512 cap and the onboarding objection is
solved — revisit then, and fix the nine predicates first.

### Decision 2 — RESOLVED: `event_history` only

It is the only strong candidate — unbounded growth in workflows × steps, never read cross-tenant,
and the table that dominates size (2.7 GB against `workflow_instances`' 376 MB at only five events
per workflow). The moderate candidates (`idempotency_keys`, `workflow_promises`,
`workflow_update_requests`) would be cheap to add at 64 buckets but buy little; the weak ones are
bounded queues. `workflow_defs` stays out as a different problem — few rows, large bytes per row.

Consequence for the schema: `event_history`'s primary key changes from `(workflow_id, step)` to
`(tenant_id, workflow_id, step)`. Free, with no deployments.

### Decision 3 — PARTLY RESOLVED: plugin isolation scope

Tracked as [cleat#1277](https://github.com/cleat-team/cleat/issues/1277). The mechanism landed in
[#1280](https://github.com/cleat-team/cleat/pull/1280) — a declarative `TenantScoped` field on
`plugin.Migration`, with the runtime emitting the policy — and kvstore is its one adopter. The rest
is blocked on [#1278](https://github.com/cleat-team/cleat/issues/1278).

**This entry as first drafted was backwards, and the correction is worth keeping.** It read
"minimum: policies on the plugin tables that already carry `tenant_id`" — the obvious shape, and
wrong. Plugins hold a bare `*sql.DB` rather than the store, so `cleat.tenant_id` is unset on every
plugin connection and `beginTxWithRLS` never touches their statements. **Policies added first would
not catch a leak; they would refuse the next plugin query.** The adapter has to set the tenant
first, where it is inert precisely because nothing reads the value yet. That is the order #1280
actually shipped in.

**And the blocker for the remaining plugins is not the policies at all.** A tenant reaches a plugin
on exactly one path — the HTTP middleware. Host calls and background loops carry none, so for any
plugin with a cross-tenant sweep, "add a fail-closed policy" and "silently empty the sweep" are the
same change. #1278 holds the classification and the two questions that must be answered before the
others can follow; one of them — how cross-tenant sweeps should be widened — wants an explicit
named bypass of the `admin.claim_workflows` shape rather than a missing predicate.

Do not quote a table count from here. The figures in circulation disagree (21-of-25 against
27-of-29) without either being load-bearing, because they are censuses of a growing population. The
finding that matters is a predicate and does not drift: **no plugin table had a policy on any
dialect.**

Still open under this decision: whether to wire up `RegisterPluginTables` or delete it along with
the `grant_plugin_to_tenant` half that depends on it — both have zero callers — and `blob_content`,
which holds payload bytes with no tenant column at all.
See `plugin-table-handling.md` for the fuller review.

### Decision 4 — RESOLVED: up to 512 tenants per deployment

Beyond that, operational considerations push toward multiple cleat deployments rather than one
larger one. That gives a hard ceiling to design against rather than an open-ended curve, and it is
what made Decision 1 worth re-examining.

Two consequences worth carrying forward:

- **Per-bucket sizing is total ÷ 64.** Whatever `event_history` reaches for a full deployment,
  each maintenance unit is a sixty-fourth of it — which bounds the free space a rewrite needs and
  the duration of a per-partition index build.
- **The cap is an operational invariant, not just a number.** If a deployment ever exceeds it, the
  answer is a second deployment, not more buckets — changing bucket count later means re-hashing
  and moving data.

The Azure platform question bundled here is now Decision 5.

### Decision 5 — RESOLVED: Azure SQL Database is the preferred Azure target, and RCSI gets set

Azure SQL **Database**, not Managed Instance. That means `READ_COMMITTED_SNAPSHOT` is **on** in
production, so the test harness must set it too.

**Why the trade is the right one for cleat.** Measured on SQL Server 2022 — a writer holding an
uncommitted `running → terminating` transition open for 3s, with a concurrent reader:

| | plain `SELECT` | `WITH (UPDLOCK)` |
|---|---|---|
| RCSI OFF (locking) | blocked **2.17 s**, then `terminating gen=8` | 0.17 s → `terminating gen=8` |
| RCSI ON (snapshot) | **0.19 s**, but `running gen=7` — **stale** | blocked 1.96 s → `terminating gen=8` |

Neither is a higher isolation *level* — both are READ COMMITTED. Locking is stronger on freshness;
RCSI is stronger on read consistency within a statement, and never blocks. The hazard RCSI adds is a
read-then-act sequence proceeding on stale data with no error and no delay.

cleat is well placed for that trade, for the two reasons the last column shows:

- The claim path uses `READPAST, UPDLOCK, ROWLOCK` (`engine/mssql_lifecycle.go:146, 193, 354, 555`),
  and **an explicit lock hint forces locking semantics under RCSI too** — it blocked 1.96 s and
  returned the fresh value.
- Writes are fenced on `generation`, so a worker that reads stale state and then writes is refused
  rather than silently winning. That is the correct defence against snapshot reads.

The residual risk surface is narrow: unhinted `SELECT`s whose result feeds a decision that is not
separately fenced.

**Other consequences of Database over Managed Instance:**

- `CREATE LOGIN` is server-level and unavailable from a user database on Azure SQL Database.
  `migrations/mssql/012_admin_role.sql` documents the cross-tenant admin setup as `CREATE LOGIN` →
  `CREATE USER ... FOR LOGIN` → `ALTER ROLE cleat_admin ADD MEMBER`, and the engine repeats that
  remediation in two error messages (`engine/mssql_lifecycle.go:519`,
  `engine/mssql_schedules.go:875`). All three need the contained-user form
  (`CREATE USER ... WITH PASSWORD`). It fails gracefully today — `ErrCrossTenantClaimUnsupported`,
  falling back to per-tenant claiming — so this is a docs gap, not a crash.
- Only the `PRIMARY` filegroup exists, so any future MSSQL partition scheme is `ALL TO ([PRIMARY])`.
  No impact today; cleat's MSSQL migrations use no filegroups.
- Online and resumable index operations are **available**, unlike on-prem Standard where they are
  Enterprise-only (measured: `Msg 1712` on Express). Azure SQL Database is a *better* migration
  target than on-prem Standard.

**Checked and not a problem:** no cross-database queries anywhere in the MSSQL path, no SQL Agent, no
linked servers, no `OPENQUERY`, no `msdb`. Per-tenant databases are the MySQL topology, not MSSQL.
Those are the big Azure SQL Database restrictions and cleat does not touch them.

> Provenance caveat: the container and edition behaviour above was measured locally. The Azure
> defaults themselves — Database on, Managed Instance off — were **not**, because this session had
> no Azure access. Verify against a real instance before relying on them.

---

## Work plan

Ordered so that nothing lands unverifiable.

- [ ] **1. Differential verification harness.** Old-vs-new catalog diff, proven against a
      deliberately broken compaction. Nothing else is safe to land without it.
      **It must build both databases from scratch, never against a shared test database.**
      [#1281](https://github.com/cleat-team/cleat/issues/1281) records that `tests/upgrade` adds
      `mig_test_col` and `idempotent_col` to the shared database and never drops them — leftovers
      like that would surface as a spurious catalog difference and send a reader hunting a
      compaction bug that does not exist. A harness whose whole purpose is to detect schema
      differences is the worst possible consumer of a polluted database.
- [ ] **2. Migration-runner prerequisites.** Non-transactional migration support, `lock_timeout` on
      the migration session, migration moved out of worker boot into `--migrate-only`. Independent
      of the partitioning design, so it can run in parallel with (1).
- [ ] **3. Add `tenant_id` to the nine PostgreSQL predicates.** Independent of everything else, and
      it is what makes partition count cheap. Nine one-line changes; list above.
- [ ] **4. The three compacted files**, with `event_history` hash-partitioned and its PK changed to
      `(tenant_id, workflow_id, step)`. Verified by (1).
- [ ] **5. Plugin migration rebaseline.** Collapse each plugin to version 1, record skipped
      separately from applied, fail closed on plugin health under `--migrate-only`.
- [ ] **6. Grants, onboarding, and `drop_tenant`** updated for partitions.

Two items are small, self-contained, and worth doing immediately regardless of whether any of the
above proceeds:

- [ ] **Set RCSI on every MSSQL test database at creation**, so the suite runs under the regime
      production uses. Five sites: `engine/testutil/packagedb.go:118`,
      `tests/plugin-harness/mssql_migration_rerun_test.go:265`, `tests/plugin-harness/testdb.go:65`,
      `tests/crash/harness_test.go:232`, `cmd/cleat-worker/tenant_isolation_mssql_test.go:84`. Also
      the two operator-facing "recreate the database" messages
      (`engine/testutil/mssql_schema.go:85`, `mssql_migration_rerun_test.go:116`).
      `ALTER DATABASE ... SET READ_COMMITTED_SNAPSHOT ON` needs exclusive access, which a
      freshly-created database has.
- [ ] **Expect concurrency tests to change behaviour, and read the failures.** That difference is
      the signal — it points at unhinted reads feeding unfenced decisions. Do not skip past it: a
      skip that hides a behaviour change is a failure wearing a skip's clothing.
- [ ] **Assert RCSI at worker startup** when the driver is `mssql`, mirroring
      `engine.CheckRLSEnforced` (`engine/rls_check.go:48`). Converts an assumption about the
      deployment into a check that fails loudly, rather than prose that rots.
- [ ] **Document the contained-user form** of the cross-tenant admin role in
      `migrations/mssql/012_admin_role.sql` and the two engine error messages that cite it.
- [ ] Plugin-table RLS — [cleat#1277](https://github.com/cleat-team/cleat/issues/1277).

---

## Provenance

All measurements taken 2026-09-11 against `develop` at `6b7f0790`, on `postgres:16` and
`mcr.microsoft.com/mssql/server:2022-latest` containers with stock configuration. Where a number
describes a growing population, the re-derivation command is given inline; prefer running it to
quoting the number.
