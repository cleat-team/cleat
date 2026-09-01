# Three parallel workstreams — round 2

Written 2026-08-05 against `develop` at `6926f2e`, replacing the 2026-08-04 edition. That one
is done: every item on all three boards is closed or restated here, and there are **no open
PRs**. Companion to `IMPROVEMENT-PLAN.md`, which stays the source of truth for *what* each
item is **and whether it is still open**; this file is only about *who does what, where, and
without colliding*.

Baseline, verified before writing this rather than assumed: `go build ./...`,
`go vet ./engine/` and `go test -count=1 ./engine/` are green against **PostgreSQL, MySQL and
SQL Server together** (52.7s). Every item below is verifiable locally.

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
so `CGO_ENABLED=0` does not skip a check: it removes the primary backend from the binary and
runs everything on wazero. An engine result obtained that way is not evidence about the engine.

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
| `IMPROVEMENT-PLAN.md` | Edit only your own `§` sections. New sections: WS-1 takes §3.10+, WS-2 §3.20+, WS-3 §3.30+. |
| `scripts/skip-baseline.txt`, `scripts/skip-budget.txt` | Never hand-edit. Regenerate with `scripts/check-skips.sh --update` **after** rebasing. A count going *down* is the point; **a count going up needs a sentence.** `test-go/engine` and `cluster` move together — the cluster job also runs `./engine/...`. Currently 187 skip sites, 731 skipped tests. |
| `scripts/deadcode-baseline.txt` | Same; regenerate with `scripts/check-test-only-code.sh --update`. A shrinking baseline is the honest evidence that wiring landed — 12 entries left it when §2.26 was wired. Currently 59. |
| `migrations/{postgres,mysql,mssql}/` | Numbered per dialect; Postgres is at `005`, the other two at `004`. Reserved: **WS-1 010–019, WS-2 020–029, WS-3 030–039** — sparse and above the high-water mark so no stream renumbers another. |
| `.golangci.yml` | One linter per PR; say which one you are taking. See the standing item. |

---

## WS-1 — Tenancy below PostgreSQL

**Sandbox:** `/localssd/rcownie/cleat`   **Migrations:** `010–019`

One theme: PostgreSQL enforces tenant isolation with RLS and the other two backends do not,
so on MySQL and SQL Server every scope check is a Go-level `WHERE tenant_id = ?` with nothing
underneath it. §1.7 fixed the HTTP layer. This is everything below it.

**Owns:** `engine/mysql_*.go`, `engine/mssql_*.go`, `engine/testutil/*schema*.go`,
`migrations/{mysql,mssql}/`, and the idempotency block of `engine/store_lifecycle.go`.

| item | what |
|---|---|
| **§3.10** | ✅ **DONE.** Corrected 2026-09-01. The row used to open *"Idempotency keys are global across tenants, on all three dialects … `idempotency_keys` is keyed by `key_hash` alone and has no `tenant_id` column"*, and said **"Start here."** It has since been fixed by the column route rather than the hash route: `migrations/{postgres,mysql,mssql}/010_idempotency_keys_tenant_id.sql` exists in all three dialects, and every lookup is tenant-scoped — e.g. `engine/mssql_lifecycle.go:953`, `WHERE key_hash = @p1 AND tenant_id = @p2`, whose own comment records the old global behaviour. The "decision (below)" it refers to was therefore settled. |
| **§1.7 residual** | 🔴 **STILL REAL — the only one of these three that is.** Re-measured 2026-09-01: `grep -rh 'CREATE POLICY' migrations/mysql/*.sql \| grep -c .` → `0`, against PostgreSQL's `10` (this row said "seven"; that count had drifted too). `migrations/mysql/` has **zero** RLS policies against PostgreSQL's. On MySQL a missed `tenant_id` filter is a silent cross-tenant leak with no database backstop. MySQL 8.4 has no RLS: the options are a view layer, a connection-user scheme, or documenting MySQL as single-tenant-only. **The decision matters more than the code** — settle it before writing a migration. |
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

**Sandbox:** `/localssd/rcownie/cleat-agent1`   **Migrations:** `020–029`

One theme: the engine can detect a crash but cannot yet say what was in flight when it
happened, so recovery re-executes side effects it has already performed.

**Owns:** `engine/durablecalls.go`, `engine/flush*.go`, `engine/store_events*.go`,
`engine/idempotency.go`, `engine/callerrors.go`, `engine/lifecycle.go`, `tests/crash/`.

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
3. **`.github/workflows/` is WS-3's**, but any stream adding a dialect-conditional test moves
   the skip budget for `test-go/engine` **and** `cluster` together. Regenerate, never
   hand-edit, and expect a conflict if two streams add tests the same afternoon.

## Sequencing, if you want the highest yield first

> **Re-verified 2026-09-01, and most of this paragraph is spent.** `WS-1 §3.1` — the
> "cheapest real security fix on the board … remaining work is a migration and one decision"
> — describes §3.10, which is done: migration `010_idempotency_keys_tenant_id.sql` shipped on
> all three dialects. §2.71's residual is done too. Of the three residuals in the table above,
> **only §1.7 (MySQL has no RLS) is still real**, and it is a decision, not a migration —
> `tiers.yaml` already omits `mysql` from `multi_tenant`, so "document MySQL as
> single-tenant-only" may simply be recording what is already true.

**WS-2 §3.20** is about an hour and deletes an API that answers as though it works. **WS-3's
Python item** has the clearest finish line and the best existing notes. Both were checked on
2026-09-01 only to the depth of "the plan still marks them open" — verify before starting,
per the paragraph below.

Each lands on day one and gives its stream a green PR before it starts a multi-session item.

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
