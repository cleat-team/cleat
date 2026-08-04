# Three parallel workstreams

Written 2026-08-04, against `develop` at `3c13fb7`. Companion to `IMPROVEMENT-PLAN.md`,
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

| | Postgres |
|---|---|
| WS-1 | `5432` — the existing `cleat-postgres-1` |
| WS-2 | `5433` |
| WS-3 | `5434` |

`docker compose down -v` destroys the user's database: the `cleat` project owns
`cleat-postgres-1`. Remove containers **by name**.

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
| **§1.1** | `finalize_workflow_status` fences the status `UPDATE` on `assigned_to` + `generation`, then runs the terminal block **unconditionally**. A zombie worker that correctly lost the fence still executes it. ~2 sessions. **Start here** — it is the largest live data-loss item in the plan. |
| **§1.2** | The same shape in Go: `ClearStickyWorker`, `ReleaseWorkflowConcurrencyKeys`, `enforceParentClosePolicy`. Fenced `UPDATE`, error checked, `RowsAffected()` never inspected, unconditional post-commit cleanup. |
| **§1.6** | `ReapStaleInstances` and `TerminateWorkflow` clear `assigned_to` but leave `generation`, making the token defence-in-depth in name only. ~0.5 session — cheap, do it early for the confidence. |
| **§2.17** | `ShardedStore` hands every shard the full limit and strands the excess as `running` with no executor. Demonstrated at 3 shards / limit 2: claimed 6, returned 2, stranded 4. Three candidate fixes trade off differently on the hot claim path — that choice is why it is still open. |
| **§2.26** | `mssqlRetry` is still test-only. Wiring it needs a per-transaction rollback-guarantee decision across 8 `BeginTx` boundaries. Do this last; it needs live SQL Server. |

**The trap:** §1.1 and §1.2 are easy to "fix" by checking `RowsAffected` and returning an
error, which converts silent corruption into a spurious failure on the *legitimate* path.
Establish first what a lost fence should do — it is usually "stop, quietly, having done
nothing", not "error".

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
   intent in the same procedure. WS-1 lands §1.1 first; WS-2 builds on the result.
3. **WS-3's §1.7 adds RLS to MySQL/MSSQL migrations**, and WS-1's §2.26 touches MSSQL
   operations. Different files, same dialect — coordinate before either lands a schema change
   to the MSSQL path.

Nothing else in the three lists overlaps.

## Sequencing, if you want the highest yield first

WS-2 §1.3 (cancellation, ~1 session) and WS-1 §1.6 (generation bump, ~0.5 session) are the
two cheapest real fixes on the board. Both are small enough to land on day one and give each
stream a green PR before starting its multi-session item.
