# Three parallel workstreams — round 2

> **Read this first, added 2026-09-02.** `WORKSTREAM.md` (2026-08-06) supersedes this file's
> *premise* — three concurrent streams — and the record bears it out: since 2026-09-01 only WS-3
> has stamped work on `IMPROVEMENT-PLAN.md` (WS-1 12 sections in August and 0 in September, WS-2
> 8 and 0, WS-3 10 and 11). **So do not read the per-stream schedules or the coordination
> rituals below as current.**
>
> **Amended the same day: the WS-1 and WS-2 boards *are* current, and are the exception to
> that sentence.** They were re-derived against `develop` at `81d8f8b` after this banner was
> written — see the second banner below. That does not restart either stream; it means that
> when the work is picked up, the board says what is actually open. The WS-3 board below is
> the one that is stale, and carries its own note.
>
> It does **not** supersede the parts of this file that say *who owns what and where*, which live
> nowhere else and are still in force:
>
> - the **checkout → stream map** below — this is how a session learns which stream its sandbox is;
> - the **database ports** per stream (WS-3 used Postgres `5434`, MySQL `3308`, SQL Server `1435`
>   for a full three-dialect engine run on 2026-09-02);
> - the **migration ranges** (`010–019` / `020–029` / `030–039`), still cited as reserved —
>   but **nearly full**, measured the same day: mssql is at `037` with two numbers left in the
>   `030` block. The rule this banner gives for `IMPROVEMENT-PLAN.md` section numbers is the
>   one to use for migrations too; see "On the migration ranges" below.
>
> The one rule here that did **not** survive is the `IMPROVEMENT-PLAN.md` section allocation
> (WS-1 `§3.10+`, WS-2 `§3.20+`, WS-3 `§3.30+`). WS-1 holds §3.34, §3.37 and §3.38 inside WS-3's
> block and §3.35 was allocated twice. Take the next free number **above the highest in the
> file** instead; see `WORKSTREAM.md` for why.

Written 2026-08-05 against `develop` at `6926f2e`, replacing the 2026-08-04 edition. That one
is done: every item on all three boards is closed or restated here, and there are **no open
PRs**. Companion to `IMPROVEMENT-PLAN.md`, which stays the source of truth for *what* each
item is **and whether it is still open**; this file is only about *who does what, where, and
without colliding*.

Baseline, verified before writing this rather than assumed: `go build ./...`,
`go vet ./engine/` and `go test -count=1 ./engine/` are green against **PostgreSQL, MySQL and
SQL Server together** (52.7s). Every item below is verifiable locally.

> ## Re-derived 2026-09-02 against `develop` at `81d8f8b`
>
> **WS-1's and WS-2's boards are empty.** Every item listed on either of them below is now
> closed, and the closure was checked against the tree rather than against the plan's status
> markers. New boards are in the two sections that follow; the old rows are kept with what
> closed them, because "this row is gone" is not something a reader can verify and "this row
> closed here" is.
>
> The one correction that matters, because this file made it on 2026-09-01 and got it wrong:
> **§1.7's MySQL-RLS residual is not an open engineering item.** The row below calls it
> *"🔴 STILL REAL — the only one of these three that is"*, off a `grep -c 'CREATE POLICY'`
> that returns `0`. The grep is right and the conclusion does not follow. `tiers.yaml` D1 has
> recorded `multi-tenancy-mysql: NOT SUPPORTED — single-tenant only` since **2026-08-06**, four
> weeks before that row was written, and `IMPROVEMENT-PLAN.md` §1.7 says so in the body
> (*"Not open — closed by D1, and this bullet contradicted the DECIDED block ~150 lines below
> it for 25 days"*). Re-derive: `grep -n 'D1' tiers.yaml`.
>
> That is the same failure mode this file's own closing paragraph warns about, one level up: a
> metric was re-run, the metric was fine, and **nobody re-read the decision the metric was
> supposed to inform.** Before calling a residual real, check `tiers.yaml` for a `decision:`
> covering it — a product boundary and an unimplemented gap grep identically.
>
> **This does not restart WS-1 and WS-2.** The banner above this one is right that only WS-3
> has been writing since 2026-09-01, and nothing here contradicts it: these are boards for
> whoever picks the work up, one stream at a time or otherwise, not a claim that three streams
> are running. An earlier draft of this note said "three streams are running again" — measured
> wrong, corrected here, and #563's `grep` over workstream labels is the measurement that
> settles it (WS-1 12→0, WS-2 8→0, WS-3 10→11 across August→September).
>
> Open PRs at the time of writing: #561, #562 (both WS-3, both since merged) and one dependabot
> bump. Nothing is in flight for WS-1 or WS-2 — `gh pr list --state open` to re-derive.

| | sandbox | theme |
|---|---|---|
| **WS-1** | `/localssd/rcownie/cleat` | Tenancy: isolation PostgreSQL enforces and the other two do not |
| **WS-2** | `/localssd/rcownie/cleat-agent1` | Durability: surviving a crash, and replaying what happened |
| **WS-3** | `/localssd/rcownie/cleat-agent2` | Execution boundaries: what stops a guest that will not stop |

---

## What round 1 established, and what it cost to learn

Read this before picking anything up. Most of yesterday's findings came from one mechanism,
and none came from reading code.

**Make the thing runnable, then run it.** Nine defects were found in a day by standing up
MySQL and SQL Server locally — which three prior sessions had recorded as impossible. It was
a colima profile flag. The written record was wrong in the direction that made the problem
look smaller, every time: §2.71 was recorded as "context lost after recycling" when it was
*every query*; §2.26 said 8 transaction boundaries when there were 20; parent close policy
had never been tested at all.

**Audit the ✅ markers.** Three were checked yesterday; all three were overstated. §1.1's
guard shipped for three dialects with proof for two. §2.17's regression test ran against a
mock, one layer above where its own evidence had come from. §2.26's count was less than half
the real number. A ✅ means someone fixed something; it does not mean anyone re-ran it.

**Prove the test can fail — and know what that misses.** Removing the fix and re-running
catches a test that *cannot* fail. It does not catch one that fails *sometimes*: a 2 ms sleep
in the zombie-writer scenario survived four CI runs and lost the fifth. If an assertion
depends on wall-clock time, remove the timing rather than widening it.

**Watch which layer is holding the test up.** Twice yesterday an assertion passed because of
a layer other than the one under test. §1.1's first fence test passed with the SQL guard
deleted, because a Go-level rollback covered for it. An MSSQL cross-tenant assertion passed
against a *wide-open* security policy, because `MSSQLStore`'s own SQL carries `tenant_id = @p`
and the Go filter did the work. Both were caught by breaking the specific layer and watching.

**`errcheck` would have caught two of yesterday's fixes.** §2.50 (parent close policy
discarding every error it produced) and half of §1.2 (fire-and-forget terminal writes) are
both literally unchecked-error findings. There are **307** more. See the standing item below.

---

## Rules for all three

**Branch from `develop`, rebase before every push.** Three streams merging into one branch
means `develop` moves under you several times a day. #283 sat long enough to go
`CONFLICTING`; rebasing it cost more than landing early would have.

**Never merge on red, never re-run a failing job hoping it passes.** Read the log. If it is
genuinely infrastructure — yesterday's was `npm ci` dying with `ECONNRESET` — say so out loud
and re-run *that*, rather than quietly retrying a test.

**Build and test with CGO on — the default.** `NewWasmtimeBackend` is behind `//go:build cgo`,
so `CGO_ENABLED=0` does not skip a check. ~~It removes the primary backend from the binary and
runs everything on wazero.~~ **Corrected 2026-09-02:** there is no wazero backend to fall
through to — `engine/backend_wazero.go` was deleted in #459 (2026-08-10), so at `CGO_ENABLED=0`
there is no `WasmBackend` at all and `cleat-worker` exits 1 at startup
(`cmd/cleat-worker/main.go`). `CGO_ENABLED=0 go build ./...` still exits 0, so nothing tells
you at build time. Either way, an engine result obtained that way is not evidence about the
engine. `ls engine/backend_wazero.go` to re-derive; `CLAUDE.md`'s "Is this result real?"
section carries the long version.

**One PR, one thing.** Every PR yesterday that bundled a second concern was harder to review
than the two would have been apart.

### Databases — all three run locally

Not a constraint on any item here.

| | Postgres | MySQL | SQL Server |
|---|---|---|---|
| WS-1 | `5432` | `3306` | `1433` |
| WS-2 | `5433` | `3307` | `1434` |
| WS-3 | `5434` | `3308` | `1435` |

```
CLEAT_TEST_POSTGRES=postgres://postgres:postgres@localhost:5432/cleat?sslmode=disable
CLEAT_TEST_MYSQL='root:cleat@tcp(127.0.0.1:3306)/cleat?tls=false&parseTime=true'
CLEAT_TEST_MSSQL='sqlserver://sa:CleatTest123!@localhost:1433?database=cleat'
```

`go test ./engine/` takes ~21 s with Postgres alone and ~50 s with all three — **check that
delta**, because an unset DSN skips its dialect silently and the suite still prints `ok`.

**A name in that table is not unique across docker contexts, and 2026-09-02 there are two
containers called `cleat-ws1-mssql`** — one in the `colima-cleat-ws1` profile on `1433`, which
is the one the DSN above reaches, and another in the default `colima` profile on `1435`, which
is WS-3's port. Both answer. So `docker exec cleat-ws1-mssql ...` hits a *different database*
depending on which context is current, with no error either way. Address SQL Server containers
as `docker --context colima-cleat-ws1 ...` explicitly, and re-derive the mapping with
`for c in $(docker context ls -q); do docker --context $c ps --format "$c {{.Names}} {{.Ports}}"; done`
rather than trusting the table. Ports on the host are what the DSN sees; the table is right
about those.

SQL Server needs a Rosetta context: bare `docker` resolves to the default colima profile,
which has `rosetta: false`, and `sqlservr` aborts there under QEMU. The `cleat-ws1` and
`cleat-ws3` profiles have it, as does Docker Desktop. Create a profile rather than restarting
`default`, which would stop every other stream's database:
`colima start --profile <name> --vm-type vz --vz-rosetta --cpu 2 --memory 4 --disk 20`, then
`docker context use colima`.

`docker compose down -v` destroys the user's database — the `cleat` project owns
`cleat-postgres-1`. Remove containers **by name**.

### Shared files, and a protocol for each

| file | protocol |
|---|---|
| `IMPROVEMENT-PLAN.md` | Edit only your own `§` sections. ~~New sections: WS-1 takes §3.10+, WS-2 §3.20+, WS-3 §3.30+.~~ **That allocation is breached and retired (#563, 2026-09-02)** — WS-1 holds §3.34, §3.37 and §3.38 inside WS-3's block and §3.35 was allocated twice. **Take the next free number above the highest in the file.** |
| `scripts/skip-baseline.txt`, `scripts/skip-budget.txt` | Never hand-edit. Regenerate with `scripts/check-skips.sh --update` **after** rebasing. A count going *down* is the point; **a count going up needs a sentence.** `test-go/engine` and `cluster` move together — the cluster job also runs `./engine/...`. Currently 187 skip sites, 731 skipped tests. |
| `scripts/deadcode-baseline.txt` | Same; regenerate with `scripts/check-test-only-code.sh --update`. A shrinking baseline is the honest evidence that wiring landed — 12 entries left it when §2.26 was wired. Currently 59. |
| `migrations/{postgres,mysql,mssql}/` | Numbered per dialect. **The 010/020/030 blocks are nearly full — take the next free number above the dialect's high-water mark, not the next one in a block.** See the note below. |
| `.golangci.yml` | One linter per PR; say which one you are taking. See the standing item. |

**On the migration ranges — they are nearly full, and #563 already wrote the rule that
replaces them.** The blocks were "sparse and above the high-water mark so no stream renumbers
another", and four weeks of merges consumed most of them. Measured 2026-09-02 with
`for d in postgres mysql mssql; do ls migrations/$d/*.sql | sed 's#.*/##;s/_.*//' | tr '\n' ' '; echo; done`:

| dialect | numbers in use | high-water | left in the `030` block |
|---|---|---|---|
| postgres | 001–005, 010, 020–024, 031–034 | `034` | 5 |
| mysql | 001–004, 010, 020–022, 030, 033 | `033` | 6 |
| mssql | 001–004, 010–013, 020–022, 031, 033–037 | `037` | **2** |

Note what that shows and the block scheme did not survive: numbers are **not** aligned across
dialects — `033` is a different migration in each — and the `030` block was used by whoever
needed the next number, not by WS-3. The block is a collision-avoidance device between
concurrent writers, not a per-stream namespace, so a migration numbered in another stream's
block is not a defect to go fix.

**Do not re-reserve fresh blocks.** An earlier draft of this section did exactly that
(`040/050/060`) before #563 landed, and #563's rule is better for the same reason it gives for
`IMPROVEMENT-PLAN.md` section numbers: **take the next free number above the highest in the
file rather than the next one in a reserved block.** A reservation needs every writer to
remember it every time, and the evidence that it does not survive contact is in the table
above. The per-stream `Migrations:` line on each board below is kept only as the historical
record of which numbers a stream took.

## WS-1 — Tenancy below PostgreSQL

**Sandbox:** `/localssd/rcownie/cleat`   **Migrations:** took `010–019`; new ones go above the dialect high-water mark

**Owns:** `engine/mysql_*.go`, `engine/mssql_*.go`, `engine/testutil/*schema*.go`,
`migrations/{mysql,mssql}/`, and the idempotency block of `engine/store_lifecycle.go`.

### The theme changed, 2026-09-02 — read this before the board

The old theme was *"PostgreSQL enforces tenant isolation with RLS and the other two backends
do not."* Half of that is now a settled product boundary rather than a gap, so it stops
generating work:

- **MySQL** is `NOT SUPPORTED — single-tenant only` by D1, decided 2026-08-06. It has no RLS
  and will not get it; a mechanism was prototyped and rejected on cost (6.1x on scans) before
  the decision, so this was chosen against a measured alternative. README and
  `docs/reference/multi-tenancy.md` already say so — checked, not assumed.
- **SQL Server** does have a backstop, and the row below that said it did not was reading a
  grep that inverted when the fix landed. Measured 2026-09-01 on a freshly created database —
  *not* re-run today, because the "freshly created" is the whole measurement and a long-lived
  database shows 8 either way: `SetupMSSQLFullSchema` installs **8** enabled security policies.
  What *was* re-checked today is the code path that makes it true: `mssql_schema.go` hand-writes
  no schema and `SetupMSSQLFullSchema` → `applyMSSQLSchemaFile` → `applyMigrations` runs the
  shipped `migrations/mssql/*.sql`, which carry 42 `CREATE/SECURITY POLICY` statements.

So the surviving theme is narrower and worth stating on its own: **the tenant is not yet part
of every identity the product exposes.** A definition's name is the live instance of that
(§3.12), and it is where a decision is needed before code.

### The board

| item | what |
|---|---|
| **§3.12** | 🔴 **The one substantial open item, and it needs a decision first.** The overwrite half is closed — a definition records its deploying tenant and a deploy over someone else's is refused with `ErrWorkflowDefOwnedByAnotherTenant`. What is not closed: `workflow_defs`' primary key is still `(name, version)` on all three dialects, so two tenants cannot each hold `order-processor`, and one tenant's definition is readable by name from another. **The decision is product, not engineering: is the workflow-definition namespace per-tenant or global?** If per-tenant, the key change reaches three foreign keys per dialect and ~96 query sites and wants its own review — a multi-session item, not a migration. Verify with `grep -n 'PRIMARY KEY' migrations/postgres/001_schema.sql` around `workflow_defs`; the plan's §3.12 has the full resolution note including the default-tenant adoption window it opened. |
| **§3.15** | 🔴 **Open, and it is a live denial rather than a latent gap.** `GetAllowedSignalCallers` reads `workflow_instances.allowed_signals`, and nothing in the product writes it — no store method, no endpoint, no CLI verb, no SDK call. The consumer is installed whenever `--require-signal-auth` is set. **The bleeding is stopped, correctly and on both sides** — the flag defaults to `false` with the item named in its own help text (`cmd/cleat-worker/config.go:90`), and `docs/reference/worker-config.md:197` opens the section with "**Not usable yet.** Nothing in cleat can write `allowed_signals`". So this is no longer a trap; it is an advertised flag that cannot be turned on. That makes it the cleanest piece of *new feature* work on either board rather than a defect to fix: a write path (store method → API → CLI), after which the flag's default becomes a decision. |
| **§2.60d** | 🔶 **Shared with WS-2, and `engine/testutil/` is WS-1's, so the mechanism lands here.** `CleanupPostgresTestData` is still an unqualified `DELETE FROM` across eleven tables. The 2026-08-31 change made a failed cleanup loud; it did not make the suites isolated, so a green run on a reused database still means little and the engine matrix job still needs `-p 1`. WS-2's standing recommendation is a tenant per package with tenant-scoped deletes, on the argument that it makes the suite exercise tenant scoping instead of routing around it — which is a WS-1 argument, and the reason this sits here. Note that §3.72 (WS-3, 2026-09-02) fixed a *different* cross-run poisoning in the same suite; it does not subsume this. |
| **§3.38** | 🔶 **Observed once, not reproduced.** `TestAdminForceResolve_*` failed in a PostgreSQL-only run that followed a full three-dialect run against the same database. Almost certainly §2.60d's shape, which is the argument for taking §2.60d rather than this. Recorded so the next person does not spend twenty minutes ruling it out of an unrelated diff. |
| **tiers.yaml `admin-dashboard`** | 🔶 **Small, and it is a stale support claim rather than a defect.** All three `open_items` on that component read as open and at least the first is provably closed: `web/src/lib/api.ts:23` sends `Authorization: Bearer ${token}`, `web/src/lib/auth.ts` and `web/src/components/ApiKeyGate.svelte` are the operator-pasted-key path D3 asked someone to choose, and they landed in #458. Re-derive with `grep -n Authorization web/src/lib/api.ts`. Whether that promotes the component out of tier 2 is a product call and should be made deliberately — but the manifest should not go on asserting a gap that closed three weeks ago. |

### Closed since this board was written — with what closed it

| item | closed by |
|---|---|
| **§3.10** idempotency keys global across tenants | `migrations/{postgres,mysql,mssql}/010_idempotency_keys_tenant_id.sql`; every lookup carries `tenant_id`. |
| **§2.71 residual** MSSQL test schema has no policies | `SetupMSSQLFullSchema` applies the shipped migrations instead of a hand-written schema; 8 policies on a fresh database. **The grep that "proved" this open returns `0` either way** — see this file's closing paragraph. |
| **§1.7 residual** MySQL has no RLS | D1, `tiers.yaml`, 2026-08-06. Not code. |
| **§3.11** four unscoped MySQL queries | All four carry `AND tenant_id = ?` today, each with a comment naming the item: `mysql_ops.go:626` (`GetWASMLength`), `:1074` (`QueueDepth`), `:1143` (`DeleteExpiredEvents`), `mysql_store.go:476` (`GetAllowedSignalCallers`). Line numbers drift — `grep -n 'IMPROVEMENT-PLAN 3.11' engine/*.go` finds them by comment. |
| **§3.36's one real defect** silent lock-release failure | `engine/workflow_cleanup.go` — one helper both dialects call, which is the "mechanism, not a sweep" answer rather than the 26-site version. |
| **§3.39** concurrency-key re-entrancy | Fixed 2026-08-31. It was on `WORKSTREAM.md`'s blocked-on-you list; it is not blocked and not open. |

### The 2026-09-01 board, kept for its evidence

**Every row here is closed** — the table above says what closed each one. Kept because the
measurements under them are the ones a future reader will want to re-run, and two of the rows
are case studies in a metric that reads the same before and after its fix.

| item | what |
|---|---|
| **§3.10** | ✅ **DONE.** Corrected 2026-09-01. The row used to open *"Idempotency keys are global across tenants, on all three dialects … `idempotency_keys` is keyed by `key_hash` alone and has no `tenant_id` column"*, and said **"Start here."** It has since been fixed by the column route rather than the hash route: `migrations/{postgres,mysql,mssql}/010_idempotency_keys_tenant_id.sql` exists in all three dialects, and every lookup is tenant-scoped — e.g. `engine/mssql_lifecycle.go:953`, `WHERE key_hash = @p1 AND tenant_id = @p2`, whose own comment records the old global behaviour. The "decision (below)" it refers to was therefore settled. |
| **§1.7 residual** | ✅ **CLOSED BY D1, and this row was wrong on the day it was written.** ~~🔴 STILL REAL — the only one of these three that is.~~ The measurement below is correct and the inference from it is not: MySQL having no RLS policies is the *documented product boundary*, not evidence of an unimplemented gap. `tiers.yaml` decided it 2026-08-06. Original text follows. Re-measured 2026-09-01: `grep -rh 'CREATE POLICY' migrations/mysql/*.sql \| grep -c .` → `0`, against PostgreSQL's `10` (this row said "seven"; that count had drifted too). `migrations/mysql/` has **zero** RLS policies against PostgreSQL's. On MySQL a missed `tenant_id` filter is a silent cross-tenant leak with no database backstop. MySQL 8.4 has no RLS: the options are a view layer, a connection-user scheme, or documenting MySQL as single-tenant-only. **The decision matters more than the code** — settle it before writing a migration. |
| **§2.71 residual** | ✅ **DONE — and the metric below inverted, so re-read it before believing it.** Corrected 2026-09-01. The row used to say: *"The MSSQL test schema defines none of the seven security policies (`grep -c "SECURITY POLICY" engine/testutil/mssql_schema.go` → `0`), so ~220 MSSQL tests run with no tenant backstop."* **That grep still returns `0`, and it now means the opposite.** The fix was exactly what the row asked for — read the policies out of the real migration rather than restating them — so `mssql_schema.go` no longer *contains* a schema to grep. `SetupMSSQLFullSchema` → `applyMSSQLSchemaFile` → `applyMigrations` runs the shipped `migrations/mssql/*.sql`, and `requireMSSQLPoliciesIntact` fails loudly if the policies are recorded-as-applied but absent. Measured on a **freshly created** database, because MSSQL objects persist and a long-lived one proves nothing: `SetupMSSQLFullSchema` → **8 policies**, all enabled (`TenantFilter_{Defs,EventHistory,Instances,Promises,Routing,Schedules,Signals,Tags}`). |
| **§3.11** | Four unscoped MySQL queries from the `s.tenantID` audit: `GetWASMLength` (keyed on a user-chosen def name — cross-tenant metadata), `QueueDepth` (counts every tenant's rows), `DeleteExpiredEvents` (**deletes across all tenants**), `GetAllowedSignalCallers` (authorization data, unscoped). |

**The §3.10 decision.** Two fixes, and the difference only shows on upgrade. Adding `tenant_id`
with a composite primary key `(key_hash, tenant_id)` lets existing rows take the default
tenant, so single-tenant deployments keep deduplicating across the upgrade — recommended.
Putting the tenant into the hash is one line and no migration, but every existing key stops
matching, so a retried request after upgrade starts a *second* workflow, which is exactly what
idempotency exists to prevent.

**The trap:** `ClaimWorkflows` looks unscoped — `UPDATE ... WHERE id IN (...)` — and is not:
its candidate `SELECT` filters `tenant_id`, so the ids are already restricted. A static sweep
for "statements without `tenant_id`" gives 16 hits of which most are false positives. Read
the enclosing function before believing the grep.

---

## WS-2 — Durability: crash recovery and replay

**Sandbox:** `/localssd/rcownie/cleat-agent1`   **Migrations:** took `020–029`; new ones go above the dialect high-water mark

**Owns:** `engine/durablecalls.go`, `engine/flush*.go`, `engine/store_events*.go`,
`engine/idempotency.go`, `engine/callerrors.go`, `engine/lifecycle.go`,
`engine/callintent.go`, `engine/store_intent.go`, `engine/store_admin*.go`, `tests/crash/`.

### The theme is achieved; what is left is what history can *say*

The old theme was *"the engine can detect a crash but cannot yet say what was in flight when
it happened, so recovery re-executes side effects it has already performed."* That is done, and
it is worth being precise about done, because it is the stream's whole argument:
`tests/crash` SIGKILLs a worker during the third of three durable calls and counts what the
external service was asked to **do**. With the fix reverted a crash re-executed two charges
that had already completed successfully; with intent writes and idempotency keys in place only
the interrupted call is retried and the service does not act on the duplicate twice.
`go test ./tests/crash/ -count=1` to re-derive.

What remains is one level up and is a single recurring question: **a durable record can say
that something happened, but not yet what kind of thing it was, or that a phase of it is still
owed.** §2.35 is the first half; §3.35 phase 5 is the second, and it is now a request from
another stream rather than a hypothetical.

### The board

| item | what |
|---|---|
| **The defer-phase record shape (§3.35 phase 5)** | 🔴 **Take this first, and it is a design answer, not a PR.** WS-3 has phases 1–4 of `defer` shipped and is explicitly declining to start phase 5 without WS-2: making the defer phase survive a `kill -9` needs it to be *its own durable, resumable unit with a reaper*, which the plan states is **the same record shape as §1.4's crash recovery** (`IMPROVEMENT-PLAN.md` §3.35, "Phase 5 is not scheduled"). Both streams and the plan now independently agree on that, so the coordination cost has already been paid and what is missing is the answer. The concrete question: does a defer phase become another `event_history` row under phase D's existing pending discipline — `intent_at IS NOT NULL AND checksum IS NULL`, resolved through `ResolveCallIntent` — or its own table with its own reaper? **WS-2 should answer it before starting §2.35, not after**, because §2.35 changes the same rows and doing them in the other order means designing the record twice. Deliverable is a written shape in §1.4 or §3.35 that WS-3 can build against; the migration to carry it is WS-3's, in their range. |
| **§2.35 residual** | 🔴 **The real remaining engineering item, and it is schema work with no cheap version.** `ErrorCode`'s seven values still have no path into history: `event_history` has an `error TEXT` column and no `error_code` — verify with `awk '/CREATE TABLE.*event_history/,/^\);/' migrations/postgres/001_schema.sql`. `workflow_instances.error_code` exists and is populated, so the classification survives per *workflow* and is lost per *event*, which is why replay re-derives one bit where the fresh run had seven values. The governing constraint is unchanged and is what makes this worth doing: **fresh and replay must classify identically**, or retryability changes between a run and its replay. |
| **§1.4 phase F** | 🔶 **Open, unblocked, and smaller than it was.** Admin force-resolve for a *pending* step. Its prerequisite §3.20 shipped (#297) and `adminForce` in `engine/store_admin.go` is the shape it extends — including the three things #297 learned the hard way: apply the operation to the tenant-scoped store rather than `s.store`, give the audit record an `eventRecordToPayload` arm or it sits outside the checksum chain, and expect a concurrent writer to be able to take the step number. Worth sequencing after the record-shape answer above, since "resolve a pending unit" is the same verb phase 5 needs. |
| **§2.60d** | 🔶 **Now carried on WS-1's board**, because the mechanism lands in `engine/testutil/`, which is WS-1's. Still WS-2's constraint in practice: it is the reason a green run on a reused database means little and the reason a multi-package invocation needs `-p 1`. |
| **`AdminReReplay`** | 🔶 **Still a stub, and it now answers 501 rather than 500** — `engine/store_admin_stubs.go`. Honest, and not fixable by a fourth `UPDATE`: it needs the replay semantics phases D–F build. Naturally lands after phase F. |
| **retire `pendingSentinel`** | 🔶 Still detected alongside `Pending` because `tests/integrity` exercises it directly — `engine/types.go:399`, exported as `PendingSentinel` for that test. Cleanup, belongs with phase E's tail. |
| **§2.11 residual** | ⚪ **Watch, do not hunt.** "A claim for 3 returned 10" is still unexplained; the SQL in both forms, `ShardedStore` and a retry wrapper are all ruled out, and §2.17's backstop now detects an over-claim, releases the excess and logs the evidence. Do not spend a session re-deriving what 24,000 claims failed to reproduce. |

### Closed since this board was written — with what closed it

| item | closed by |
|---|---|
| **§3.20** force-resolve stubs | #297. Real bodies on all three dialects in `engine/store_admin.go`; `grep -rn 'not implemented yet' engine/*.go` now returns only comments recording the removal. |
| **§1.4 phases B, C, D, E** | #308 (D, `event_history.intent_at` + `commit intent → dispatch → commit outcome`), #316 (the crash scenario), #335 (E, `AmbiguityResolver` + persisted resolution). The dead `flushCallIntent`/`completeCallEvent` pair was deleted and reimplemented as `engine/callintent.go` + `engine/store_intent.go`, reachable from a real deployment via `--write-ahead-intent-ops`. |
| **§3.22** an ambiguous call is erased, not reported | #343. |
| **§3.24** an ambiguous outcome classifies as `unknown` | Fixed 2026-08-31. `WS2-STATUS.md` still lists it first among "open, in the order I would take it". |
| **§3.23** guest error reported as a wasm trap | Fixed 2026-08-31 — was listed as "not mine, blocked on WS-3", and WS-3 landed it. |

| item | what |
|---|---|
| **§3.20** | `AdminForceComplete` and `AdminForceFail` are stubs returning `not implemented yet` on every dialect, while `cmd/cleat-worker/api_admin.go` routes to them behind the ownership check §1.7 added. **The admin API answers as though force-resolve exists.** Small, and it removes a live lie from the API surface. It is also §1.4 phase F's prerequisite. **Start here.** |
| **§1.4 C–F** | Crash recovery. **Phase B and the §2.4 harness are done** — `tests/crash` kills a worker mid-call and counts what the external service was asked to *do*, not what it received. Remaining: **D** intent + schema, **E** resolution, **F** admin force-resolve. D was deliberately blocked on C so the schema could be judged against a harness instead of a design doc; that harness now exists. |
| **§2.35 residual** | `ErrorCode`'s seven values still have no path into history, so a failure classified on the fresh run is reclassified from a bare string on replay. `CleatError` already participates in retry classification through `errors.As`, so the plumbing exists — persistence is what is missing. The constraint governing the whole stream: **fresh and replay must classify identically**, or retryability changes between a run and its replay. |
| **§2.11 residual** | "A claim for 3 returned 10" is **still unexplained**. Ruled out: the SQL in both forms, `ShardedStore`, a retry wrapper. §2.17's backstop now detects an over-claim, releases the excess and logs the evidence the plan asked for — so a recurrence is finally diagnosable. This is *watch for it*, not *go find it*; do not spend a session re-deriving what 24,000 claims failed to reproduce. |

**Cross-stream:** §1.4 phase F wants store methods on `engine/db.go`, which is WS-1's. Add
them in a new file rather than editing `db.go`.

---

## WS-3 — Execution boundaries

**Sandbox:** `/localssd/rcownie/cleat-agent2`   **Migrations:** `030–039`

One theme: an unbounded guest. When workflows are agent-generated a runaway loop is routine
rather than exotic, and the emergency brake has to work on every backend a guest can land on.

**Owns:** `engine/backend_*.go`, `engine/runtime.go`, `engine/executor.go`,
`engine/wasmtime_*.go`, `engine/component_*.go`, `wasm/`, `packages/`, `crates/`,
`python-sdk/`, `Dockerfile`, `.github/workflows/`.

> **A session is live on this board (2026-09-02) and the three rows below are all closed. Do
> not work from them; the live board is `WS3-STATUS.md` and §3.35 in the plan.** Left in place
> rather than rewritten, because this section is being edited by someone else right now and a
> conflict here costs more than a stale table does with this note over it.
>
> - **§1.5 / §2.28** — ✅ closed for deployments 2026-09-02. Python is in `WasmtimeLanguages`,
>   the decomposition path the row describes was **deleted** (§3.65), and an unrouted language
>   is refused rather than falling through. The residual is developer tooling only.
> - **§3.30** — ✅ decided 2026-09-01: wazero stays, scoped to CLI and dev tooling. There is
>   no wazero *backend* at all — `engine/backend_wazero.go` was deleted in #459.
> - **§3.31** — ✅ written 2026-08-05, closed 2026-09-01.
>
> **§3.35 phase 5 is the open one, and it is blocked on WS-2 by design** — its durable
> resumable record is the same shape as §1.4's, so it is on WS-2's board as the first item.

| item | what |
|---|---|
| **§1.5 / §2.28 residual** | **Python runs on wazero, unfenced — and does not work there either.** `WasmtimeLanguages` is `{go, assemblyscript, java, rust}`; Python is absent. Its component reaches the decomposition path and stops at `undefined element: out of bounds table access` at instance 52 — eleven deeper than before the `env::abort` arity fix, so that fix helped and did not finish. On wazero it fails differently: `module[__main_module__] not instantiated`. **Start here** — `engine/engine.go:292-323` records where the next attempt begins, including a `backend_wasmtime.go` retry keyed on a neighbouring error that does not rescue this case, and the native component path behind a build tag no build sets. |
| **§3.30** | Once Python is on wasmtime or explicitly dropped, decide what wazero is *for*. It is currently the fallback for one language that does not work on it. Either it has a stated, tested role or it is a second execution engine carrying a known bug tail for nobody. `CLAUDE.md` still calls it the fallback "for the languages that do not work under wasmtime" — that set may now be empty. |
| **§3.31** | Write the execution-limit story per *backend* and test it per backend rather than per language. §1.5's history is a fix that existed, was correct, and reached no deployment for weeks because it sat behind a build tag the shipped image did not set. **A limit not verified on the artifact users run is not a limit.** |

---

## Standing item for everyone — the linter backlog

Not a workstream, because it touches every file; a protocol instead. **One linter per PR,
repo-wide, and say in the PR which one you are taking** so two streams do not collide.
Measured 2026-08-04 with golangci-lint's default caps removed — they silently truncate, and
`errcheck` reads as 50 with them on and 307 with them off:

```
ineffassign  8      staticcheck  17     gosec    193
gosimple     9      unconvert    23     errcheck 307
unused      16      gocyclo      28
```

Take them off the top. `ineffassign` needs three `//nolint` on defensive trailing `argIdx++`
— **do not delete them**; removing one makes adding the next clause a silent bug.

The two at the bottom are the ones that matter and should not be deferred forever on size.
**`errcheck` (307) is the class that produced §1.2 and §2.50**, both real defects fixed
yesterday, both literally an unchecked error return. `gosec` (193) is unreviewed security
findings in a codebase whose last two days have been tenancy defects. Neither must be cleared
in one pass — `//nolint` with a reason is a fine answer per site, and an unreviewed 307 is
worse than a reviewed 307 with 250 suppressions.

### The table above is four measurements out of date — and this file is the wrong place for it

**Five of the eight linters listed are enabled now** (`misspell`, `ineffassign`, `gosimple`,
`staticcheck`, `unconvert`), so the protocol worked. The four still disabled have been
re-measured four times since, and `.golangci.yml` carries every round with its date and the
reasoning — **read it there rather than trusting a count here.** Restating a number in two
files is how the two disagree.

Re-measured 2026-09-02 against `develop` at `81d8f8b`, for the record and because the shape
matters more than the totals:

| linter | total | production | after the dominant idiom |
|---|---|---|---|
| errcheck | 859 | 247 | **89** — 158 of the 247 are `defer tx.Rollback()`, which returns `ErrTxDone` by design |
| gosec | 666 | 256 | **38** — 218 of the 256 are G115, and G115 is decided: do not sweep it |
| gocyclo | 39 | 31 | 31 |
| unused | 56 | 28 | 28 — same U1000 analysis `scripts/check-test-only-code.sh` already runs |

All four fell (08-31: 1028 / 693 / 44 / 66), largely because #459 and #528 deleted code rather
than because anyone fixed findings.

**And a new false-green, which is the reason this section is worth updating at all.** On this
machine the pinned `golangci-lint` v1.64.7 cannot read the installed Go 1.27 toolchain's export
data (`export data version 4 is greater than maximum supported version 2`). It does not fail —
it emits typecheck errors instead of findings, and every type-aware linter reads **0**. That is
the same tidy-table-of-zeroes `.golangci.yml` already warns about from the `--disable-all`
cause, reached a second way. CI is unaffected: `lint-go` pins `go-version: '1.25'`. Locally:

    GOTOOLCHAIN=go1.25.11 golangci-lint run --timeout=20m -c <one-linter.yml> ./... | grep -c '(<linter>)'

and check the count is non-zero for a linter you know has findings before believing a zero for
one you hope does not.

---

## Cross-stream couplings — the three that are real

1. **`engine/store_lifecycle.go` is shared.** WS-1 owns its idempotency block (§3.10); WS-2
   owns the event and flush paths. Different regions of one file — expect textual conflicts,
   not semantic ones, and rebase often.
2. **`engine/testutil/` is WS-1's this round** (it was WS-2's), because §2.71's schema
   residual is the blocking piece and lives there. WS-2 and WS-3 should ask before adding
   test-schema columns: #283 consolidated two hand-written MySQL schemas into one, and a
   third divergence would undo that.

   **The stated reason for that assignment is gone** (2026-09-01): §2.71's schema residual is
   done, and `mssql_schema.go` no longer hand-writes a schema at all — it applies the shipped
   migrations. The "third divergence" risk this coupling exists to prevent is correspondingly
   smaller, since there is now one fewer hand-written copy to diverge. Re-assign or drop the
   coupling deliberately rather than inheriting it.

   **Re-assigned deliberately, 2026-09-02: `engine/testutil/` stays WS-1's, for a new reason.**
   The old reason is spent, but §2.60d is now the blocking piece that lives there, and its
   recommended fix — a tenant per package with tenant-scoped deletes — is a tenancy design
   choice, which is WS-1's competence. Moving it back to WS-2 would put the isolation
   mechanism in the stream that most wants to route around it.
3. **`.github/workflows/` is WS-3's**, but any stream adding a dialect-conditional test moves
   the skip budget for `test-go/engine` **and** `cluster` together. Regenerate, never
   hand-edit, and expect a conflict if two streams add tests the same afternoon.
4. **The durable-record shape for a resumable phase is WS-2's to define, and WS-3 is waiting
   on it** (added 2026-09-02). `§3.35` phase 5 needs the defer phase to be its own durable
   resumable unit with a reaper; the plan, WS-3's session and this file all now say that is
   the same shape as §1.4's crash-recovery record. **WS-3 is deliberately not starting it**,
   which is the one-stream rule working rather than a blockage to route around. WS-2 owes an
   answer, not an implementation; the migration that carries it is WS-3's, in their range.

## Sequencing, if you want the highest yield first

> **Re-derived 2026-09-02, and now all of it is spent. The current answer:**
>
> One stream is writing, so this is a **priority order across both boards**, not two parallel
> assignments:
>
> | | item | why here |
> |---|---|---|
> | 1 | **the defer-phase record shape** (WS-2, §3.35 phase 5) | The only item with someone waiting behind it: WS-3 has `defer` phases 1–4 shipped and is deliberately not starting 5 without this. It is a written answer rather than a PR, and taking §2.35 before it means designing the same rows twice. |
> | 2 | **§3.15, the signal-authorization write path** (WS-1) | The only item on either board that is neither blocked on a product decision (§3.12) nor a multi-package refactor (§2.60d). Additive feature work — store method, endpoint, CLI verb — after which the flag's default becomes a decision. |
> | 3 | **§2.35**, `error_code` per event (WS-2) | The substantial engineering item, and it follows item 1 by construction. |
>
> **Neither board has a "lands on day one" item any more, and that is the change worth naming.**
> Every previous edition of this paragraph could offer one, because the boards were full of
> things that were simply not done. What is left is a decision, a schema change, or a refactor.
> Sequencing by cheapness has run out; sequence by what unblocks someone else instead — which
> is why item 1 is first despite producing no code.
>
> The `tiers.yaml` `admin-dashboard` correction on WS-1's board is the closest thing to a quick
> win left, and it is a doc fix guarding a support claim, not engineering.

> **Re-verified 2026-09-01, and most of this paragraph is spent.** `WS-1 §3.1` — the
> "cheapest real security fix on the board … remaining work is a migration and one decision"
> — describes §3.10, which is done: migration `010_idempotency_keys_tenant_id.sql` shipped on
> all three dialects. §2.71's residual is done too. Of the three residuals in the table above,
> **only §1.7 (MySQL has no RLS) is still real**, and it is a decision, not a migration —
> `tiers.yaml` already omits `mysql` from `multi_tenant`, so "document MySQL as
> single-tenant-only" may simply be recording what is already true.
>
> **2026-09-02: it was already true, and had been for 26 days.** The "may simply be" is the
> tell — the paragraph got within one grep of the answer and stopped. D1 decided it on
> 2026-08-06, `tiers.yaml` records `NOT SUPPORTED — single-tenant only` with its reasoning,
> and README and `docs/reference/multi-tenancy.md` both already say it. Nothing was owed.

~~**WS-2 §3.20** is about an hour and deletes an API that answers as though it works. **WS-3's
Python item** has the clearest finish line and the best existing notes. Both were checked on
2026-09-01 only to the depth of "the plan still marks them open" — verify before starting,
per the paragraph below.~~

~~Each lands on day one and gives its stream a green PR before it starts a multi-session item.~~

**Both were already done when that was written, 2026-09-01.** §3.20 shipped in #297 on
2026-08-05, and Python was in `WasmtimeLanguages`. The stated check — "the plan still marks
them open" — is the check the paragraph immediately below says is not enough, applied to the
two items the paragraph was recommending. Neither costs a session to verify:
`grep -rn 'not implemented yet' engine/*.go` and `grep -n 'WasmtimeLanguages =' engine/engine.go`.

**And check the `§` heading's status marker in `IMPROVEMENT-PLAN.md` before starting anything
here.** Last round's table was wrong about three of five items on the day it was written,
because it was derived from the plan's headings and the derivation dropped the ✅. This one
was built by reading the tree — `grep -c "SECURITY POLICY"`, `WasmtimeLanguages`, the
migration listing, a green three-dialect run — but it starts going stale the moment someone
lands a fix. The plan is the source of truth for status; this file is only the split.

**It went stale exactly as predicted, and one of those greps inverted rather than drifting.**
Re-verified 2026-09-01: two of the three residuals were already fixed. `grep -c "SECURITY
POLICY" engine/testutil/mssql_schema.go` still returns `0` — the number this file recorded as
proof the backstop was missing — but it returns `0` now *because the fix landed*, having
replaced the hand-written schema with a call that applies the shipped migrations. **A metric
that reads identically before and after the fix is worse than no metric**, because re-running
it confirms the stale conclusion. Prefer one that has to change: here,
`SELECT COUNT(*) FROM sys.security_policies` **on a freshly created database** — 0 before, 8
after. The "freshly created" is load-bearing; MSSQL objects persist, so a long-lived test
database shows 8 whether or not the helper installs them.
