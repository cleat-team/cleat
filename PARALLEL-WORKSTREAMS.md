# Three parallel workstreams

Written 2026-08-04, against `develop` at `3c13fb7`; WS-1's item list corrected the same day
against `9df4396` (see "Already landed"). Companion to `IMPROVEMENT-PLAN.md`,
which stays the source of truth for *what* each item is; this file is only about *who does
what, where, and without colliding*.

Three sessions run concurrently, one per sandbox:

| | sandbox | theme |
|---|---|---|
| **WS-1** | `/localssd/rcownie/cleat` | Fencing: a worker that lost the race still does damage |
| **WS-2** | `/localssd/rcownie/cleat-agent1` | Persistence: things the engine decides and then forgets |
| **WS-3** | `/localssd/rcownie/cleat-agent2` | Above the engine: tenancy, CLI, toolchain |

The split is by **file ownership**, not by topic. Topic-based splits read well and then
collide in the diff. Each stream below lists the paths it owns; if you need to change a path
another stream owns, say so rather than editing it.

---

## Rules for all three

**Branch from `develop`, rebase before every push.** Three streams merging into one branch
means `develop` moves under you several times a day.

**Never merge on red, never re-run a failing job hoping it passes.** If CI fails, read the
log and report the failure. That rule is the reason the last two sessions found anything.

**Prove the test can fail.** Before shipping a regression test, remove the fix and re-run it.
If it still passes, the test is decoration. Several of tonight's findings were tests that
had never been able to fail — do not add more.

**Build and test with CGO on — the default.** `NewWasmtimeBackend` is behind
`//go:build cgo`, so `CGO_ENABLED=0` does not skip a check, it removes the primary backend
from the binary and runs everything on wazero instead. `CLAUDE.md` told you to disable CGO
until 2026-08-04; that instruction was stale (fixed by `c26c332`, note never updated) and is
now corrected. This matters most to **WS-1 and WS-2, who live in `engine/`** — an engine
result obtained under `CGO_ENABLED=0` is not evidence about the engine.

**Verify by running, not by reasoning.** Tonight, in this repo: a heap-leak detector that
missed an injected 140 MB leak, a diagnosis that was wrong about the *cause* while its fix
was still load-bearing, and an E005 check that failed a real example the first time it ran
for real. All three were caught by executing, none by reading.

### Shared files — three of them, and a protocol for each

| file | protocol |
|---|---|
| `IMPROVEMENT-PLAN.md` | Edit only your own `§` sections. New sections: WS-1 takes §2.50+, WS-2 §2.60+, WS-3 §2.70+. Rebase before push; markdown conflicts are cheap but constant otherwise. |
| `scripts/skip-baseline.txt`, `scripts/test-only-baseline.txt` | Never hand-edit. Regenerate with `--update` **after** rebasing, and say in the commit why the count moved. A count going *down* is the point; a count going up needs a sentence. |
| `migrations/{postgres,mysql,mssql}/` | Numbered sequentially per dialect, currently at `005`. Reserved ranges: **WS-1 006–009, WS-2 010–013, WS-3 014–017.** Do not renumber someone else's. |

### One database each — do not share

Concurrent suites in a single checkout are safe since §2.39 (per-suite task queues and a
schema fingerprint), but three checkouts on different branches can hold *different schemas*.
Give each stream its own instance.

| | Postgres | MySQL (`CLEAT_TEST_MYSQL`) | SQL Server (`CLEAT_TEST_MSSQL`) |
|---|---|---|---|
| WS-1 | `5432` — the existing `cleat-postgres-1` | `3306` | `1433` |
| WS-2 | `5433` | `3307` | `1434` |
| WS-3 | `5434` | `3308` — `cleat-ws3-mysql` | `1435` — `cleat-ws3-mssql` |

The same offset scheme in all three dialects: WS-1 keeps the defaults, WS-2 and WS-3 step up
from there. WS-3 stood up its MySQL 8.4 and SQL Server 2022 on 2026-08-04 and they are the
only two that exist so far; `§2.26` and the MySQL/MSSQL halves of any fence work stay
unverified until a stream has its own — say so rather than claiming dialect coverage you did
not run.

**Do not `docker compose -f docker-compose.dev.yml up`** — it binds Postgres to `5432` and
would collide with WS-1's live instance. Start containers individually with explicit
`--name` and `-p`.

`docker compose down -v` destroys the user's database: the `cleat` project owns
`cleat-postgres-1`. Remove containers **by name**.

### SQL Server needs its own colima VM on Apple Silicon

The default colima profile is `vmType: vz` with `rosetta: false`, so amd64 images fall back
to QEMU — and `mcr.microsoft.com/mssql/server:2022-latest` **cannot start under QEMU**. It
exits immediately with `Invalid mapping of address ... in reserved address space`. There is
no arm64 SQL Server image; Rosetta is the only route.

Enabling Rosetta on the default profile means restarting that VM, which would stop all three
streams' Postgres containers. So WS-3 created a **second profile** instead:

```
colima start cleat-ws3 --vm-type vz --vz-rosetta --cpu 2 --memory 6 --disk 40
docker --context colima-cleat-ws3 run -d --name cleat-ws3-mssql --platform linux/amd64 \
  -e ACCEPT_EULA=Y -e 'MSSQL_SA_PASSWORD=CleatTest123!' -e MSSQL_PID=Developer \
  -p 1435:1433 mcr.microsoft.com/mssql/server:2022-latest
```

**`colima start` rewrites the global docker context** in `~/.docker/config.json`, which would
silently repoint every other stream's `docker` command at the new VM. It was set back with
`docker context use colima` immediately, and all SQL Server management uses an explicit
`--context colima-cleat-ws3`. If you start a colima profile, do the same. Port forwarding
reaches `127.0.0.1:1435` from the host regardless of context, so Go tests need no context flag.

### Connection strings

```
CLEAT_TEST_MYSQL='root:cleat@tcp(127.0.0.1:3308)/cleat?tls=false&parseTime=true&multiStatements=true'
CLEAT_TEST_MSSQL='sqlserver://sa:CleatTest123!@127.0.0.1:1435?database=cleat'
```

SQL Server needs `CREATE DATABASE cleat` once; MySQL gets it from `MYSQL_DATABASE`. Neither
needs migrations applied by hand — the suites install what they need (`applyMySQLProcedures`,
`applyMSSQLProcedures` in `engine/store_backends_procedures_test.go`).

Two known potholes, neither introduced by this setup:

- `TestMySQLStoreFactory` and `TestMySQLIntegration_FactoryOpenStore` **fail on any port
  other than 3306.** They gate on `CLEAT_TEST_MYSQL` and then ignore its value, hardcoding
  `tcp(127.0.0.1:3306)`. Already recorded in `IMPROVEMENT-PLAN.md` as config drift that was
  "harmless in CI" — it stops being harmless off the default port. `engine/` is WS-1/WS-2
  territory, so WS-3 left it alone.
- `TestMSSQLIntegration_FinalizeWorkflowSegment_{Done,Suspend}` **fail when run under a
  `-run` filter that excludes the procedures test**, because they need
  `finalize_workflow_status` installed by another file's `sync.Once` and do not call
  `applyMSSQLProcedures` themselves. They pass in a full `go test ./engine/...`. An
  order-dependency, not a product bug.

---

## WS-1 — Fencing and lost updates

**Sandbox:** `/localssd/rcownie/cleat`   **Migrations:** `006–009`   **Postgres:** `5432`

One theme: the engine fences a write, ignores whether the fence held, and then performs the
side effect anyway. Every item is a variation on an unchecked `RowsAffected`.

**Owns:** `engine/store_lifecycle.go`, `engine/db.go`, `engine/sharded*.go`, the sticky and
concurrency store files, `engine/mysql_ops.go`, `engine/mssql_operations.go`,
`migrations/*/00[6-9]_*`.

| item | what |
|---|---|
| **§2.26** | `mssqlRetry` is still test-only. Wiring it needs a per-transaction rollback-guarantee decision across 8 `BeginTx` boundaries. Unblocked once WS-3's `CLEAT_TEST_MSSQL` instance is up (WS-1 uses the default `1433`). **Start here — it is the only WS-1 item still open.** |

### Already landed — do not start these

Four of the five items originally listed here are done. Three were merged **before this
document was written**, in `c26c332` (#218), which is an ancestor of the `3c13fb7` it was written against.
The table was built from `IMPROVEMENT-PLAN.md`'s `§` headings without their ✅ markers, so it
pointed WS-1 at finished work. Verified against the tree on 2026-08-04:

| item | doc said | actual |
|---|---|---|
| **§1.1** | "start here, largest live data-loss item" | landed: `migrations/{postgres,mysql,mssql}/004_fix_finalize_workflow_status_fence.sql` captures the fenced `UPDATE`'s row count (`GET DIAGNOSTICS … ROW_COUNT` / `@@ROWCOUNT`) and skips the terminal block when it is zero. `engine/fence_lost_integration_test.go` covers both the Go rollback and the SQL guard on its own. |
| **§1.6** | open, ~0.5 session | landed in all three dialects: `generation = generation + 1` in `ReapStaleInstances` (`store_lifecycle.go:705`, `mysql_lifecycle.go:723`, `mssql_operations.go:13`) and in `TerminateWorkflow` (`db.go:1056`). |
| **§2.17** | "still open — three candidate fixes" | `IMPROVEMENT-PLAN.md` §2.17 is marked ✅ **FIXED**. |
| **§1.2** | "the same shape in Go", open | the store half had already landed; the caller half is now closed too — #263 (a concurrency-key conflict returned 409 and ran the workflow anyway), #265 (`cleat-bench` had never completed a run), #267 (a lost fence was invisible and still counted as a failure). |

**The trap:** §1.2 is easy to "fix" by making every caller treat `ErrFenceLost` as a failure,
which converts silent corruption into a spurious failure on the *legitimate* path. The two
call sites that already handle it establish the precedent, and it is the right one — log at
debug and `return`, because losing a fence means another worker legitimately owns the
workflow. "Stop, quietly, having done nothing", not "error".

---

## WS-2 — Persist what the engine decides

**Sandbox:** `/localssd/rcownie/cleat-agent1`   **Migrations:** `010–013`   **Postgres:** `5433`

One theme: the engine computes something correct, acts on it, and does not write it down —
so replay cannot reach the same conclusion.

**Owns:** `engine/durablecalls.go`, `engine/heartbeats.go`, `engine/signaller.go`,
`engine/store_events*.go`, `engine/types.go`, `engine/callerrors.go`, `engine/testutil/`,
`migrations/*/01[0-3]_*`.

| item | what |
|---|---|
| **§1.3** | Cancellation is dead end-to-end. `PollCancellation(ctx, "")` — hardcoded empty string at all three call sites, and the store does `WHERE id = $1`, so it never matches. `RequestCancellation` sets a flag nothing observes. ~1 session, and the highest ratio of user-visible value to effort in the whole plan. **Start here.** |
| **§1.4 B–F** | Crash recovery: the detector works, nothing writes what it detects. Phases in order — B idempotency keys, C the 2.4 crash harness, D intent + schema, E resolution, F admin force-resolve. **D must not come before C**: without the harness you cannot tell whether the schema is right. |
| **§2.35 residual** | `ErrorCode`'s seven values still have no path into history, and the streaming plugin family (`recordStreamError`, `PluginError`) is a bare string on replay. The `ErrNonRetryable` bool that landed is the narrow version of this. |
| **§2.32 residual** | The payload/column duplication itself. Treating `payload` as the sole record means 10 `populateFromPayload` call sites across 3 dialects. Shadow-column verification now *detects* divergence; this removes the possibility. |

**The constraint that governs all of it:** a recorded failure replays from history, so fresh
and replay must classify identically. Any new field has to round-trip through the payload or
retryability changes between the original run and the replay — which is the §2.35 defect in
its general form.

---

## WS-3 — Above the engine

**Sandbox:** `/localssd/rcownie/cleat-agent2`   **Migrations:** `014–017`   **Postgres:** `5434`

**Owns:** `cmd/cleat-worker/` (HTTP, auth, tenancy), `auth/`, `cmd/cleat/`, `packages/`,
`wasm/`, `.github/workflows/`, `.golangci.yml`, `Dockerfile`, `migrations/*/01[4-7]_*`.

| item | what |
|---|---|
| **§1.7** | Tenant isolation is not enforced at the HTTP layer. `defaultTenantID` is hardcoded at `cmd/cleat-worker/main.go:159` and used process-wide: callers authenticate per-tenant, every request is then served from one scope, and the real RLS underneath is bypassed. MySQL and MSSQL migrations have **zero** RLS policies. ~2–3 sessions. **Highest severity item in the plan.** |
| **§2.43** | `cleat vet --target assemblyscript` cannot fail — it never starts a node process, and every path returns 0 after printing a line that reads like a check ran. Now fixable, because §2.42 made the transform's checks real: run `asc --noEmit` with the transform and let it fail. Needs a decision on whether `cleat vet` may require `asc`; note `cleat build` already exits 1 without `npx`, so there is a precedent. |
| **§2.28 residual** | Non-Go guests still run on wazero and remain unfenced. Related: §1.5 is fixed for wasmtime but the shipped image must stay CGO + glibc or it silently ships the unfenced backend. |
| **§2.40 residual** | Enabling `ineffassign` needs `//nolint` on three benign trailing `argIdx++` (defensive, do not delete them — removing one makes adding the next clause a silent bug). The two remaining dead fallbacks are both in `runVetAS` and are subsumed by §2.43. |

**Read this before starting §1.7.** It is the one item deliberately skipped in every recent
session, and the reason is worth inheriting rather than rediscovering: verifying an RLS
migration needs a live MySQL and SQL Server, which were not available locally. **Shipping an
unverified security migration is the exact anti-pattern the last three sessions were spent
removing.** So the first task in §1.7 is not the migration — it is standing up MySQL and
MSSQL you can actually test against. If you cannot, say so and take §2.43 first; do not
write the migration blind.

---

## Cross-stream couplings — the three that are real

1. **`engine/db.go` is WS-1's**, but WS-2's §1.4 phase F (admin force-resolve) wants store
   methods there. Add them in a new file rather than editing `db.go`, or ask WS-1.
2. **`finalize_workflow_status` is WS-1's** (§1.1). WS-2's §1.4 phase D may want to write
   intent in the same procedure. §1.1 has already landed (`c26c332`), so WS-2 can build on the
   result now rather than waiting on WS-1.
3. **WS-3's §1.7 adds RLS to MySQL/MSSQL migrations**, and WS-1's §2.26 touches MSSQL
   operations. Different files, same dialect — coordinate before either lands a schema change
   to the MSSQL path.

Nothing else in the three lists overlaps.

## Sequencing, if you want the highest yield first

WS-2 §1.3 (cancellation, ~1 session) is the cheapest real fix left on the board — small
enough to land on day one and give that stream a green PR before starting its multi-session
item. WS-1's day-one item was §1.6, which turns out to be already merged; its equivalent is
the §1.2 caller residual above.

**Check the `§` heading's status marker in `IMPROVEMENT-PLAN.md` before starting anything
here.** This table was wrong about three of five items on the day it was written, for the
ordinary reason: it was derived from the plan's headings and the derivation dropped the ✅.
The plan is the source of truth for *what* an item is **and whether it is still open**.
