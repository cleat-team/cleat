# Improvement Plan — seam hardening

**Generated:** 2026-08-02 · **Last updated:** 2026-08-03 · **`develop` @ `c26c332`**

Derived from a nine-agent adversarial review. The finding that organises this plan:

> Unit-level quality is high. Architecture is sound. **Every serious defect is at a seam** —
> component to component, config to tested-config, claim to verification. And they persisted
> because the signal that would have reported them was itself broken.

So the ordering principle is: **restore the signal, then pair every fix with a test at the
layer that would have caught it.** Not "fix everything, then add tests." The whole lesson of
this codebase is that unit tests passed while the feature was dead.

Effort is given in solo+AI sessions (a session ≈ half a day of your attention).

---

## Handover, 2026-08-04

Work is now split across three concurrent sessions — see **`PARALLEL-WORKSTREAMS.md`** for
who owns which paths, the reserved migration ranges, the per-sandbox database, and the three
cross-stream couplings. Read that before this file; then read only your own items below.

Four things worth knowing before you start, none of which are derivable from the code:

1. **Build with CGO on.** `NewWasmtimeBackend` is behind `//go:build cgo`, so
   `CGO_ENABLED=0` removes the primary backend from the binary and runs everything on
   wazero. `CLAUDE.md` said to disable CGO; that was stale (fixed by `c26c332`, note never
   updated) and is now corrected. Matters most to the two streams working in `engine/`.
2. **§1.7 needs a live MySQL and SQL Server first.** It is the highest-severity open item and
   every recent session skipped it for the same reason: verifying an RLS migration needs
   databases to verify against, and shipping an unverified security migration is the
   anti-pattern those sessions were spent removing. Stand the databases up, or take §2.43
   instead. Do not write it blind.
3. **`examples/*/node_modules` is committed.** `npm install` in an example deletes tracked
   files. Build the AS examples anyway — that is what caught the §2.42 E005 false positive —
   but `git checkout -- examples/ tests/plugin-harness/` before committing.
4. **A wrong diagnosis is kept next to the right one** in §2.39, on purpose. If an item here
   turns out to be misdiagnosed, correct it in place and say so rather than quietly replacing
   it; the wrong reasoning is what stops the next person re-deriving it.

The recurring defect class across the last three sessions, and the reason to run things
rather than read them: **a signal attached to the wrong thing.** Suites nobody ran,
assertions that were `t.Log` calls, gates documented but never enabled, a complete
static-analysis layer with five numbered error codes and a passing test that had never once
executed on real source (§2.42). Every one of them looked correct in review.

---

## Start here — next session

PR #218 landed as `c26c332`: the CI signal is restored and every workflow is genuinely
green. What follows is ordered by yield, not by section number.

### 1. Audit the 166 environment-conditional skips  —  ✅ **done**, see §2.12

> **Done.** 231 sites (not 225 — see §2.12 for why the grep undercounted) reduced to 184,
> with two new guards to stop the number growing again. Three defects fell out of it, one
> of them a live cross-tenant gap on MySQL. The original framing is kept below.

`grep -rn "t.Skip" --include='*_test.go' .` returns 225 sites, 166 of which skip on an
environment condition. **Every one is currently indistinguishable from a pass**, and that
exact mechanism accounted for four separate findings in the last session:

| Finding | What the skip hid |
|---|---|
| 1.13 | Multi-DB CI never reached PostgreSQL — green for months |
| 1.9 | `test-go`'s Postgres service had no published port |
| 2.9 | `DURABLE_TEST_DB` had been renamed; nothing noticed |
| 1.11 | Cluster workers crash-looping while the job reported success |

The fix pattern is already in the tree — `engine/testutil.TestDB` now distinguishes *no
database was asked for* (skip) from *a database was asked for and is unreachable* (fail),
per dialect. Apply it everywhere:

1. Enumerate the 166 and classify: (a) genuinely optional capability, (b) configured-but-
   unreachable, (c) skip that should just be a `t.Fatal`, (d) dead skip whose condition can
   no longer be true.
2. Convert (b) and (c). Delete (d).
3. Add a guard — a CI step that fails when the skip count in a job exceeds a baseline, the
   same shape as `scripts/check-test-only-code.sh`. The cluster job already warns on
   skips; make it a number that cannot silently grow.

Mechanical, cheap, and it closes the single most productive defect-hiding mechanism in the
repo. Do it first.

### 2. `DurableCall` fails at the ABI boundary  —  ✅ **done**, see §2.10

> **Done, and the framing below was wrong in an instructive way.** There is no `DurableCall`
> ABI bug: the fixture passed an **empty operation name** and the host was right to refuse
> it. What made that hard to see was a real defect — the refusal came back as the raw
> `errBadParam` sentinel, which `cleat_call`'s 24/32/8 result layout cannot carry, so a
> malformed argument surfaced to workflow authors as a *retryable timeout* with a `Code` of
> 4278190080 that matches no enum member. That is fixed, along with a second defect found
> on the way (empty request payloads were refused outright, so a no-argument durable call
> could not be made). `TestIntegrationWorkflowMaxDuration` is un-skipped and now runs
> against a new pure-compute fixture, `testdata/spin`.
>
> Both open questions are answered: the `cleat_complete` closure warning is a universal
> false positive and **not** the same defect, and the wazero `clock_time_get` panic is a
> **separate** bug that needs `CGO_ENABLED=0` and has nothing to do with `DurableCall`.
> Three further defects were found and deliberately **not** fixed — see §2.13 (empty
> payloads refused across the rest of the ABI), §2.14 (`cleat_json_parse` panics on
> wasmtime — confirmed by running it), §2.15 (all durable-call failures classified as
> retryable timeouts). §2.14 is the one to do next: it is a crash on the backend of record.
>
> The original framing is kept below.

`testdata/basic`'s `LongRunning` gets error `0xFF000000` from the host-call path on
iteration 0, and under wazero the worker logs a nil-pointer panic in
`wasi_snapshot_preview1.clock_time_get` beneath `DurableCall`. Whether those are one bug or
two is not established. This is in the **examples**, not the tests, and it is why
`TestIntegrationWorkflowMaxDuration` is honestly skipped rather than passing.

Reproduce under **wasmtime** first (`CLAUDE.md`: wasmtime is the behaviour of record;
wazero has its own bug tail). Note `cleat build` emits
`host function "cleat_complete" imported from WASM env but not in computed closure` for
this fixture — establish whether that is the same defect before assuming it is.

### 3. Reproduce the `limit=3 → 10` over-claim  —  ✅ **done**, see §2.11

> **Done, and it does not reproduce.** The suspected mechanism is ruled out both
> mechanically and empirically: the sublink is uncorrelated, so PostgreSQL pulls it up into
> a semi-join and executes it **once** (`EXPLAIN (ANALYZE, VERBOSE)`, `loops=1`), and its
> `LockRows` node holds `FOR UPDATE` on the candidates before the outer UPDATE reaches
> them, so EvalPlanQual has nothing to fire on. 24,000 claims under 12 concurrent claimers
> and 10 transactions committing mid-claim never returned more than the limit.
>
> Per this item's own instruction the CTE is **not** upgraded to "fixed" — it stays, and
> the comments that asserted the false mechanism are corrected. What caused the original
> observation is still unknown.
>
> The investigation did find a real over-claim, in production-wired code and of a different
> kind: **§2.17**, `ShardedStore` handing every shard the full limit and stranding the
> excess as `running` with no executor. Demonstrated at 3 shards / limit 2: claimed 6,
> returned 2, stranded 4. Left unfixed on purpose — the three available fixes trade off
> differently on the hot claim path.
>
> The original framing is kept below.

The CTE in `ClaimWorkflows`/`ClaimStickyWorkflows` is **defensive and unfalsified**. Start
by reproducing the bug, not by trusting the fix. What has already been tried and did *not*
reproduce it: concurrent claimers; a background sweep updating the same rows without
`SKIP LOCKED`. What has not: a competing `UPDATE` inside an explicit transaction that
commits mid-claim, a larger candidate set, and `EXPLAIN (ANALYZE, VERBOSE)` on the old
sublink form under contention. If it cannot be reproduced, say so in 2.11 and leave the
CTE as documented defence — do not upgrade it to "fixed".

### 4. Six wasmtime host functions return empty results  —  ✅ **done**, see §2.18, §2.19

> **Done, and the count was wrong: 18, not 6.** Only `_core.go` had been audited. A class
> guard now covers all 31 out-parameter wrappers. §2.16 is now closed too — the real
> `execSession` pattern covers the ID functions, and the 36 `mockHostHandler` assertions
> elsewhere in `backend_wasmtime_test.go` now assert what the handler actually received.
> That change found 10 wrong argument lengths in the tests themselves.
>
> The original framing is kept below.


The highest-yield open item, and the cheapest. On the primary backend `cleat_uuid`,
`cleat_workflow_id`, `cleat_run_id`, `cleat_get_state`, `cleat_list_state` and `cleat_fetch`
all report success and write zero bytes, because their wrappers never call `ctxWithMem`.
This is §2.14's defect in six more places; §2.14 fixed two and nobody checked the rest.
`cleat_get_state` is the dangerous one — an empty read looks like "unset" to a workflow.

§2.19 is a separate bug in the same two ID functions, guest-side this time. They mask each
other: fix either alone and `WorkflowID()` still returns `""`. Fix both, in one change, with
one guest-level test that would have caught the pair.

Do §2.16 at the same time rather than after. It is the reason all eight went unseen — the
wasmtime closure tests install `mockHostHandler` and assert `got != 0`, which no
implementation defect can fail. It has now produced defects twice; treat it as confirmed.

### 5. Confirm or kill §2.20 before ranking it  —  ✅ **done**, see §2.20

> **Confirmed, and it did outrank item 4.** Reproduced under a genuinely RLS-enforcing
> connection: the insert is *rejected*, the transaction aborts, and child-workflow spawning
> fails outright wherever RLS is in force — which includes the shipped cluster deployment,
> since it connects workers as `cleat_app`. Fixed, with the regression test as the
> deliverable. One caveat in the entry was wrong and is corrected there: `FORCE ROW LEVEL
> SECURITY` means an *owner* connection is rejected too, not just a tenant role.
>
> The original framing is kept below.


`StartChildWorkflowAtomic` omits `tenant_id` from its `event_history` insert, and the RLS
policy has no explicit `WITH CHECK`, so PostgreSQL should *reject* the row rather than
default it — meaning child spawning fails outright under a real tenant role. That is
reasoned from the schema, not observed. **Reproduce it first.** If it holds it outranks
everything above; if the connection never runs as a tenant role in practice, it is a
one-line hygiene fix. Either way the test is the deliverable.

### 6. Then Phase 2's remaining seam tests

2.1 golden path, 2.2 two-worker race, 2.3 cancellation e2e, 2.4 crash recovery. 2.7's flag
contract is done (§2.27); what is left of it is booting the manifests, which needs a
cluster in CI and is the same infrastructure 2.2–2.6 want.
2.6 (tenant isolation through the HTTP API) is now worth more than it was: RLS
is genuinely enforced as of 1.10, so an end-to-end test can finally prove isolation rather
than prove a policy exists — and §2.20 gives it a concrete first target.

### Standing constraints, carried forward

- **~~`docker-compose.cluster.yml` is only ever exercised in CI.~~ Lifted 2026-08-04.**
  The constraint was real but its cause was colima, not the path: colima mounts only
  `$HOME`, and `/Users/Shared/localssd` sits outside it. Docker Desktop shares all of
  `/Users` and bind-mounts the repo fine (verified by reading `go.mod` and
  `IMPROVEMENT-PLAN.md` from inside a container). Compose — and `kind`, and therefore the
  2.2–2.7 boot tests — can now run locally.
  Two things carried over from when it did hold: it has already broken once through mount
  wiring, so verify scripts inside a container; and colima forwards host ports 5432–5434,
  which collide with the cluster's PostgreSQL if both are up.
- **Two DSNs now.** `--db` is the unprivileged `cleat_app`; `--migrate-db` is the owner.
  A worker that cannot run DDL is behaving correctly.
- **PR #208 is closed, unmerged** (2026-08-03). Its headline fix was already on `develop`
  verbatim; what was still worth having is written up in the salvage register at the end of
  Phase 2, along with what was deliberately dropped and why. `BRANCH-TRIAGE.md` covers the
  rest of the unmerged branches; several predate `3eeb74e` and will not merge cleanly.

---

## Phase 0 — Restore the signal

**Nothing below Phase 0 is verifiable until Phase 0 is done.** ~1 session.

| # | Task | Where |
|---|---|---|
| 0.1 | Add `set -o pipefail` (with `shell: bash`) to the two `go test … \| tee` steps. Better: drop the pipe, write `-json` to a file with `>` and let the exit code propagate. | `.github/workflows/ci.yml:179`, `:567` |
| 0.2 | ~~Merge PR #208~~ — **superseded, see below.** Instead: add the missing `AdminActionEvent` / `EventTypeAdminAction` and the mock methods directly. | `engine/`, `cmd/*/…_test.go` |
| 0.3 | Confirm `CGO_ENABLED=0 go vet ./...` is clean. Then confirm `go vet ./...` is clean. | — |
| 0.4 | Add `./engine/...` and `./wasm/...` to the `test-go` matrix. They are **absent entirely** — engine is only touched by jobs that were masked or ignored. | `.github/workflows/ci.yml` |
| 0.5 | Remove `continue-on-error: true` from the `lint` job. A lint job that can't fail isn't one. | `.github/workflows/ci.yml` |
| 0.6 | Fix hardcoded cgo paths (`/tmp/wasmtime-v45/…`, `/home/rcownie/go/pkg/mod/…`). Also rename the file — it is not `_test.go`, so it compiles into normal builds. | `engine/cgo_test_helpers.go:7` |

### Correction on 0.2 — do not merge PR #208

`BRANCH-TRIAGE.md` §2 called #208 "only 3 behind … can be merged essentially as-is." That was
inferred from commit counts and **is no longer true.** `gh pr view 208` reports
`mergeable: CONFLICTING`, `mergeStateStatus: DIRTY`. Since the triage was written, #217
landed the *overlapping* admin-API work on `develop`, so the two collide.

#208 is also not a clean unit: 19 commits that add the dispatcher model, remove it, restore
it, then revert parts of the revert, plus three `chore: trigger CI re-run` commits and a
`Merge branch 'develop'`.

The actual breakage is six mechanical errors in five test files — `AdminActionEvent` and
`EventTypeAdminAction` were never defined, three mocks lack `AdminForceComplete`, one lacks
an `adminForceCompleteFn` hook, and `runBuild` gained a 9th parameter its test call site
never got. Fixing those directly is far smaller and lower-risk than a conflicted 19-commit
merge, and it unblocks the tree today.

Definitions are taken from #208's commit `7ed38b6` so that a later merge of #208 — if it is
ever wanted — conflicts as little as possible. **Whether to land the rest of #208 is a
separate decision, not a Phase 0 prerequisite.**

> General lesson, worth carrying into Phase 3: `BRANCH-TRIAGE.md` explicitly warned that
> commit-message and commit-count inference is unreliable and that it had verified
> containment by blob hash instead. Step 0.2 ignored that warning and repeated the mistake.
> Check `gh pr view --json mergeable` before planning around any PR.

**Acceptance gate — do not skip.** Push a commit that deliberately breaks one engine test.
CI must go red. Revert. If it stayed green, Phase 0 is not done.

---

## Progress — 2026-08-02

Branch `fix/phase0-restore-ci-signal`, pushed, draft PR #218 against `develop`.

| Commit | What |
|---|---|
| `9a38f6e` | **Phase 0 complete.** pipefail, 8 missing packages added to the CI matrix, drift guard, `continue-on-error` off lint+build, cgo paths, engine compiles again |
| `6324015` | TinyGo removed. Includes a **replay-determinism fix** in `plugins/dag` — a hand-rolled JSON encoder that existed only for TinyGo serialised maps in randomised order, so task inputs recorded in event history differed run to run |
| `f4322e3` | `tests/integrity` and `tests/cross-language` now actually execute (they had skipped on a missing `tinygo` binary since they were written) |
| `b729cad` | **1.3 done** — cancellation wired, mock that discarded the argument fixed, tests proven to fail against the original bug |
| `9292e2d` | Admin + instance API routes registered; store methods are still stubs and the tenant-ownership gap is now documented in code |
| `465e142` | Falsifiable claims corrected: ABI version, the 88M benchmark, multi-DB parity, the TLA+ module terminator |
| `0452141` | gofmt (75 files) |
| `4de546c` | Dispatch-loop tests no longer hang the package to the 10-minute timeout |
| `82b5e44` | `TestReadMemTotal` made platform-aware (it read `/proc/meminfo` unconditionally) |
| `8d44300` | **1.1/1.2 done** — the unfenced `DELETE FROM event_history`; `finalize_workflow_status` now returns whether the fence held, and `FinalizeWorkflowSegment` rolls back on `ErrFenceLost` |
| `7faa157` | **1.5 done** — wasmtime epoch interruption, fuel, and `StoreLimits`; the `_start` path had been discarding errors and reporting `Result: "ok"` for interrupted infinite loops |
| `9980fc9` | The `core` CI matrix entry runs for the first time. `cleat/` is a separate module, so `./cleat/...` from the root always failed at setup — masked for its entire existence by the missing `pipefail` |
| `f9bce35` | The SQL fence guard is now itself verified, not just the Go rollback that masks it — see below |
| `93f8abf` | The four remaining red jobs: sticky-reclaim flake, grpc `GO-2026-6061`, `--namespace` crash-loop, stale `schema.sql` |
| `868ca39` | `ABI.md` corrected: output buffer under-documented 16x in 30 places, wrong scratch layout; **2.9** guard added |
| `a47eabb` | Seven Postgres product defects: `tenant_id` missing on three write paths, destructive `PollSignal`, `AssignedTo` clobbered on claim, `ContinueAsNew` never worked |
| `e13c2c8` | **1.9 done** — root `schema.sql` deleted; the shipped schema could not complete a workflow. Bootstrap seam test added |
| `b79afc5` | **2.8 done** — test-only code guard wired into CI with a 63-entry baseline |

**Acceptance gate: passed.** A deliberate breakage was pushed and `Test Go (core)` was
observed going failure → success. "CI is fixed" is now an observation, not an inference.

**Still open:** 1.4 (wire `flushCallIntent`), 1.7 (tenant scoping at the HTTP layer), the
whole of Phase 2, `cmd/cleat-worker` gofmt, and the four items below.

### Caveats carried by this branch

These are known-and-recorded, not fixed. Each is a place where the suite is greener than
the code.

1. ~~**`TestASTransform/compiles_to_wasm` never compiles anything.**~~ ✅ **Fixed** in
   `d732ea9`. The fixture now installs `@cleat/sdk` from the checkout, imports the real
   types instead of inline look-alikes, and a compile failure is a `t.Fatalf` rather than a
   `t.Skipf` — that skip is what made the subtest unfalsifiable, since any asc error at all
   was indistinguishable from the missing dependency. Proven to bite.

2. **`testutil.TestDB` skips instead of failing when Postgres is unreachable.** Its
   `MySQLTestDB`/`MSSQLTestDB` siblings already `t.Fatalf`; the Postgres path calls
   `t.Skipf` on any ping failure, even when the DSN came from an explicit
   `CLEAT_TEST_POSTGRES`/`CLEAT_TEST_DB` rather than its `localhost` fallback. A container
   that stops between runs therefore reports `ok`. `f9bce35` adds `requireBackendReachable`
   to *one* file. Roughly twelve others reach the same code path through
   `registeredBackends`: `fault_test.go`, `integration_test.go`, `plugin_migrations_test.go`,
   `store_backends_test.go`, `store_parent_wake_test.go`, the five
   `store_test_groups_*_test.go`, and `tenant_isolation_test.go`. Fix it in
   `engine/testutil/schema.go` and delete the local helper.

3. ~~**Migration 004 is verified on Postgres only.**~~ ✅ **Closed.** All three dialects now
   run it. SQL Server passed first time (`CREATE OR ALTER PROCEDURE` has no return-type
   problem). MySQL needed no `DROP PROCEDURE` — 004 already had one — and signals fence-held
   via a trailing `SELECT` row that the Go call site already read correctly. Getting the
   MySQL lane far enough to execute 004 is what exposed 1.8.

4. **`schema.sql` and `migrations/postgres/001_schema.sql` are two hand-maintained copies of
   one schema.** `93f8abf` resynchronised them. Nothing stops them diverging again, and the
   last divergence cost a debugging session (`generation` nullable in one, `NOT NULL DEFAULT
   0` in the other). Candidate for Phase 2: assert the two agree, or generate one.

**Process note for future sessions.** Two commits had to be rewound because `git add -A` was
run while subagents were mid-edit; one nearly shipped a call site an agent had *deliberately*
broken to prove a test bites. Use explicit paths, and run `git show --stat` before every
commit. A commit message asserting "docs only" over a diff full of functional code is the
same defect class this plan exists to fix.

## Phase 1 — Paired test + fix, by severity

For each item: **write the failing test first, watch it fail, then fix.** A passing unit test
is not evidence here; that is precisely how these survived.

### 1.1 Unfenced terminal side effects — data loss (~2 sessions)

`finalize_workflow_status` fences the status `UPDATE` on `assigned_to` + `generation`, then
runs the terminal block **unconditionally**, gated only on `p_final_status IN ('done','failed')`.
A zombie worker that correctly lost the fence still executes
`DELETE FROM event_history WHERE workflow_id = p_workflow_id` and injects its stale result
into the parent's `await_child` event.

- Repro chain (confirmed): `ClaimWorkflows` bumps `generation`; `ReapStaleInstances` does not.
  A→stall→reap→B claims→A finishes→A wipes B's live history.
- Fix: capture `ROW_COUNT`/`@@ROWCOUNT` from the fenced `UPDATE`; skip the entire terminal
  block if zero. All three dialects.
- Files: `migrations/postgres/003_procedures.sql:20-118`,
  `migrations/mysql/003_procedures.sql:13-108`, `migrations/mssql/003_procedures.sql:17+`
- Test: two-worker race harness (see 2.2).

**Done in `8d44300` + `f9bce35`, with one lesson worth keeping.** The first test written for
this — `TestFinalizeWorkflowSegment_ZombieWriterFence`, which drives the real store against a
real PostgreSQL — passes *whether or not the SQL guard exists.* Confirmed by deleting the
guard, reinstating the original bug in full, and re-running: still `ok`.

The reason is that `FinalizeWorkflowSegment` returns `ErrFenceLost` **before** `tx.Commit()`,
so the deferred `tx.Rollback()` discards everything the procedure did inside that transaction,
`DELETE` included. That is a real fix and a sound one — but it is a *Go-layer* fix, and it
means the SQL guard could be stripped from all three dialects without a single test noticing.

`TestFinalizeWorkflowStatus_SQLFenceGuard` closes the gap by calling the procedure directly on
a plain `*sql.DB`, outside any transaction, where the guard is the only thing standing between
a stale worker and the delete. With the guard removed it reports
`event_history was corrupted … got []` — the whole history gone.

The general form: **an end-to-end test can pass because of a layer other than the one you
think you are testing.**

**The same gap existed on SQL Server, and was not noticed when it was closed for MySQL.**
`TestFinalizeWorkflowStatus_SQLFenceGuard` and its `_MySQL` counterpart were added; the
`_MSSQL` one was not, so the §1.1 fix shipped for three dialects with proof for two.
Confirmed the hard way rather than assumed: with
`IF @rows_updated > 0 AND` stripped from `migrations/mssql/004_*.sql`,
`TestFinalizeWorkflowSegment_ZombieWriterFence/mssql` **still passes** — the Go-layer
rollback covers for the missing SQL guard exactly as documented for the other two dialects.
`TestFinalizeWorkflowStatus_SQLFenceGuard_MSSQL` catches it, reporting
`event_history was corrupted … got []`.

**And the MSSQL integration tests never installed the procedures they exercise.**
`setupMSSQLIntegrationTest` called `SetupMSSQLFullSchema` but not `applyMSSQLProcedures`, so
`TestMSSQLIntegration_FinalizeWorkflowSegment_{Done,Suspend}` passed only because some other
test had created `finalize_workflow_status` in the same database via `MSSQLBackend.Setup` —
and `CREATE PROCEDURE` persists, so after the first full run against any database the
dependency was invisible. On a **fresh** database a filtered run fails with
`Could not find stored procedure 'finalize_workflow_status'`. CI never saw it: it creates a
fresh database and runs the whole suite, so the installing test always goes first.

That is the same shape as everything else in this document — a test that passes because of
something other than the thing it names — and it is why this was found by pointing a real
SQL Server at a filtered run rather than by reading the setup helper. The only way to know which layer is holding is to break the specific
one and watch. This is the same defect class as the `tee` without `pipefail` and the mock that
discarded its argument — a green result produced by something other than the thing under test.

### 1.2 Systemic unchecked `RowsAffected` (~1 session)

Same anti-pattern in Go: fenced `UPDATE`, error checked, `RowsAffected()` never inspected,
then unconditional post-commit cleanup — `ClearStickyWorker`,
`ReleaseWorkflowConcurrencyKeys`, `enforceParentClosePolicy`. A stale writer can release a
concurrency key the legitimate owner depends on, or terminate live children off a phantom
completion.

- Files: `engine/store_lifecycle.go:302-491` (`CompleteWorkflow`, `FailWorkflow`,
  `MoveToDeadLetterQueue`, `ContinueAsNew`)
- Fix: check `RowsAffected()`, return a typed `ErrFenceLost`, and make callers in
  `cmd/cleat-worker/setup.go` handle it rather than fire-and-forget.

**Store half done. Caller half: one live bug found and fixed, one still open.**

All twelve store sites (the four methods above × three dialects) now inspect `RowsAffected`
and return `ErrFenceLost` before any post-commit cleanup. That much was already true when
this section was written.

The caller half turned out to contain a defect of a different shape than the one described
above, and a worse one. The concern here was a caller that *ignores a fence it genuinely
lost*. What was actually in the tree was a caller passing fence arguments that **cannot match
any row**, so its write was always skipped — with the error discarded, that is a write that
never happens and never reports:

```go
// cmd/cleat-worker/server.go, concurrency-key conflict path
s.store.FailWorkflow(context.Background(), runID, "", 0, "concurrency key conflict: "+key, "", "", nil)
s.writeError(w, 409, "workflow already running with key "+concurrencyKey)
```

`FailWorkflow` is the *owning worker's* terminal write, fenced on
`assigned_to = $2 AND generation = $7`. The run was inserted by `StartNewRun` moments earlier
as `'ready'` with `assigned_to` NULL, and `NULL = ''` is NULL rather than true — so no
`(workerID, generation)` this caller could pass will ever match. **The client is told 409
"workflow already running with key X" and the run executes anyway.** The HTTP layer is the
only enforcement point for `Cleat-Concurrency-Key`; `ClaimWorkflows` does not consult
`concurrency_keys`. Against a real PostgreSQL:

```
run conflict-rejected-… was rejected with 409 but a worker claimed and will execute it
(status="running" assigned_to="worker-after-conflict"); the concurrency key is not enforced
```

Fixed by rejecting with `TerminateWorkflow`, which matches on `id` alone — the existing
unowned-writer primitive, already present in all three dialects and on `ShardedStore` — and
by answering 5xx rather than 409 when that write fails, since a 409 whose rejection did not
apply is the same lie the fenced no-op told.

- Tests: `engine/fence_lost_callers_test.go` (real PostgreSQL: the run is claimed and
  executed after the 409; and `FailWorkflow` on an unclaimed run returns `ErrFenceLost` and
  leaves the row `'ready'`), `cmd/cleat-worker/concurrency_conflict_test.go` (which store
  call the handler chooses — the half the DB tests cannot see). All four were confirmed to
  fail with the fix removed; the store-behaviour one was confirmed against a deliberately
  permissive fence, since it passes both before and after and would otherwise be decoration.

**The same shape in `cmd/cleat-bench` — fixed, and it was overstating throughput by ~1.7×.**
`main.go` called `CompleteWorkflow`/`FailWorkflow` with `("", 0)` at five sites and discarded
the result with an explicit `_ =`. The benchmark never claimed the runs it started, so every
terminal write matched zero rows.

Measured on PostgreSQL 16, 20 executions at concurrency 5, `examples/as-workflow`:

| | fresh | replay | end state |
|---|---|---|---|
| before | 66.0/s, avg 15.1 ms | 54.1/s, avg 18.5 ms | **40 runs `ready`** — none completed |
| after | 39.7/s, avg 25.2 ms | 32.0/s, avg 31.3 ms | 40 runs `done` |

The benchmark had never completed a single run in its history, and reported the latency of
an execution whose terminal write was a no-op `UPDATE` matching nothing — cheaper than one
that writes a row.

The fix takes ownership through the sticky path (`UpdateStickyWorker` with a per-iteration
worker ID, then `ClaimStickyWorkflows`) so each goroutine claims the run it just started, and
passes the resulting `(workerID, generation)` to the terminal write. Part of the delta is
therefore the claim round-trip — which a real worker performs anyway — and part is the
`UpdateStickyWorker` write, which is bench scaffolding a worker does not do. The rest is the
completion write that was previously skipped. **Numbers from before this fix are not
comparable to numbers after it**, and any published figure taken from this tool before
2026-08-04 was measuring an incomplete path.

**The 16 fire-and-forget sites in `cmd/cleat-worker/setup.go` — fixed.** `FailWorkflow`,
`MoveToDeadLetterQueue` and `ReleaseWorkflow` passed correct fence arguments and discarded
the return. Not data loss: the store skips the write correctly. Two things were wrong anyway.

A lost fence was **invisible** — nothing logged it, so a worker losing every race looked
identical to one doing its job. And `RecordWorkflowFailed` was emitted *before* the store
call, so a workflow another worker went on to complete successfully was still counted as
failed: **the failure counter disagreed with the database, and the disagreement grew with
exactly the thing that causes lost fences** — workers stalling and being reaped.

Folded into `recordTerminalFailure` / `writeTerminalFailure` / `releaseWorkflow`, which log
the lost fence at debug and record the metrics only when the write applied. The precedent is
the two sites that already handled `ErrFenceLost` (the `ContinueAsNew` and
`FinalizeWorkflowSegment` paths): debug-log and return, having done nothing.

`releaseOrFail` deliberately does *not* route through `recordTerminalFailure`: it never
recorded the failed/duration pair and has no start time to report a duration from. It keeps
its dead-letter counter, now conditional on the write applying.

- Tests: `cmd/cleat-worker/terminal_failure_test.go`, asserting on the published
  `cleat_workflows_failed_total` rather than an internal counter — what an operator sees. It
  fails against the old ordering with
  `cleat_workflows_failed_total{…,workflow_name="fence-lost-wf"} 2`. Includes a positive
  control (a write that applied *is* counted, so the first test cannot pass by recording
  nothing at all), an assertion that the write receives `(w.id, wf.Generation)` rather than
  `("", 0)` — the §263 defect, which nothing else would catch — and the dead-letter routing
  that used to live inline at each site.

### 1.3 Cancellation is dead end-to-end (~1 session)

`PollCancellation(ctx, "")` — hardcoded empty string at all three call sites. The store does
`WHERE id = $1`, so it never matches. `RequestCancellation` sets a flag nothing observes.

- Files: `engine/durablecalls.go:51`, `engine/heartbeats.go:58`, `engine/signaller.go:121`
- Fix: pass `s.engine.workflowID` — exactly as `PollSignal` already does twelve lines away
  at `engine/signaller.go:133`.
- **Also fix the mock**, or this recurs: `engine/host_test.go:2014` declares the parameter
  `_ string` and discards it, which is why 2,560 engine tests passed against dead code.
- Test: cancellation e2e (see 2.3).

### 1.4 Crash-recovery: the detector works, nothing writes what it detects (~2–3 sessions)

> **Sharpened 2026-08-02 by empirical test, not grep.** The original framing here — "the
> whole feature is dead" — was too coarse. The *read* side is live and correct: a
> `pendingSentinel` in history is caught at `engine/durablecalls.go:150` and reported to the
> workflow as `[AMBIGUOUS] call outcome unknown at step N …`. `TestPendingSentinelDetection`
> now proves this for steps 0–4, with step 5 correctly showing no ambiguity because that
> workflow discards the call result with `_`.
>
> The gap is the *write* side. Nothing calls `flushCallIntent` before dispatching a real
> external call, so in an actual crash **no sentinel is ever written and the detector has
> nothing to find.** Detection is real but unreachable in production. ~~The fix is to wire the
> intent write into `freshCall` / `freshCallWithRetry` / `freshCallWithHeartbeat` — the
> detector needs no changes.~~
>
> **Correction, 2026-08-04 — that prescription is wrong, and following it would break every
> workflow that makes a durable call.** Full analysis and a replacement design in
> [`docs/durable-call-intent-design.md`](docs/durable-call-intent-design.md). In short:
>
> 1. Every completion path — `insertEventSQL` and both adaptive-flush batches — carries
>    `ON CONFLICT … DO UPDATE … WHERE event_history.response = '' AND event_history.error IS NULL`.
>    `flushCallIntent` writes `error = pendingSentinel`, which is not NULL, so the completion
>    is a **silent no-op** and the sentinel persists. Every replay then reports `[AMBIGUOUS]`
>    forever.
> 2. The intent row's checksum is computed over a record with an empty `Err` while the row
>    stores `pendingSentinel`, so in the exact crash window this feature exists to handle,
>    replay fails checksum verification instead of reporting ambiguity.
> 3. Both functions read the previous checksum from the database rather than `s.lastChecksum`,
>    which diverges under the adaptive flusher.
>
> None of these can appear until the code has a caller, which is why 48 test references are
> all green. **The 350 lines are not a head start.** This is the second time the plan's own
> prescribed fix has been wrong in the details; §2.26 was the first.
>
> The design doc's recommendation is **Phase A only for now**: delete the two writer
> functions, keep the detector, correct `docs/durable-calls.md:66` ("the write-side wiring
> will follow" reads as routine), and drop the baseline entries. Best value when this becomes
> a priority is deterministic **idempotency keys**, which need no schema change, cost no extra
> write, and make duplicates impossible rather than merely visible. Do not start the intent
> work before the 2.4 crash harness exists — building the fix before the observation is how
> this happened.
>
> Note also that both ambiguity and replay divergence are reported *inside the workflow
> result string*, not as a Go error from `Engine.Replay`. Any future test or operator
> tooling must check the result, not just `err`. Two separate test suites got this wrong.

`flushCallIntent` / `completeCallEvent` implement a real write-ahead-intent pattern so a
crash mid-external-call is detectable on replay as `[AMBIGUOUS]`. 48 test references,
**5 non-test references — all of which are its own definition and error strings.**
The live paths (`freshCall`, `freshCallWithHeartbeat`, `freshCallWithRetry`) call
`caller.Call(...)` directly and record only after return.

- Files: `engine/flush.go:182-282`; call sites `engine/durablecalls.go:40-108`, `:200-276`,
  `engine/heartbeats.go:20-89`
- Decide first: wire it in, or delete it. Shipping ~350 lines of tested-but-dead durability
  code is worse than either, because it reads as finished.
- Test: crash-recovery e2e (see 2.4).

### 1.5 Primary WASM backend has no hang protection (~1–2 sessions) — fixed for wasmtime, **still open for every deployment**

> **Re-opened and re-closed 2026-08-04 by §2.28.** The epoch-interruption fix below is real
> and tested, but it lives behind `//go:build cgo` and the shipped Dockerfile built with
> `CGO_ENABLED=0`, so no container had it: measured on the wazero backend the containers
> actually ran, a workflow with a 2-second budget ran for 2m35s and returned **success**.
> The image now builds with CGO on a glibc base and a `--verify-backend` build step keeps it
> that way. Go guests are fenced in deployments; non-Go guests on wazero still are not.
> Read §2.28 for the residual.


> **Raise this to the top of Phase 1.** wasmtime is the primary backend — it is the standard
> engine and materially more reliable than wazero, which is retained only as a fallback for
> languages wasmtime cannot host. So the engine with no execution bound is the one actually
> running production work. When the workflows are agent-generated, an unbounded loop is a
> routine occurrence, not a corner case, and there is currently no way to stop one.
> Together with 1.3 (cancellation) this is the emergency brake, and neither half works.

`wasmtime.NewEngine()` with no `Config` — no fuel, no epoch interruption, no `StoreLimits`.
`engine/executor.go:122` concedes the post-execution deadline check never runs because
`fn.Call` never returns. The one limiter (`fuelMeter`) is wazero-only and defaults to
unlimited, and `cmd/cleat-worker/main.go:704` prefers wasmtime whenever CGO is available.

- Files: `engine/backend_wasmtime.go:69`, `cmd/cleat-worker/config.go:78`
- Fix: `NewConfig()` + `SetEpochInterruption(true)`, ticker goroutine calling
  `IncrementEpoch`, `SetEpochDeadline` per invocation. Make the instruction-limit flag
  backend-agnostic.
- Test: resource-exhaustion (see 2.5).

### 1.6 Generation not bumped on reap or terminate (~0.5 session)

`ReapStaleInstances` (`engine/store_lifecycle.go:615-633`) and `TerminateWorkflow`
(`engine/db.go:1056-1076`) clear `assigned_to` but leave `generation`. Weakens the token to
defence-in-depth-in-name-only. Bump it in both.

### 1.7 Tenant isolation not enforced at the HTTP layer — 🔶 **CORE FIXED 2026-08-04**

`defaultTenantID := "00000000-0000-0000-0000-000000000000"` at
`cmd/cleat-worker/main.go:159`, used process-wide. Callers authenticate per-tenant; every
request is then served from one hardcoded scope. Real RLS exists underneath and is bypassed.

**Fixed.** Handlers now resolve a per-request store through `scopedStore`/`storeFor`
(`cmd/cleat-worker/server.go`) instead of using the process-wide `apiServer.store`, across all
45 call sites. Every backend already had the scoping this needed and none of it was being
asked for: Postgres sets `cleat.tenant_id` in `beginTxWithRLS`, SQL Server hands out a
per-tenant pool whose connector calls `sp_set_session_context`, MySQL routes to a per-tenant
database. All three cache pools, so the cost is a struct allocation and a map lookup.

Failure is closed: with `--require-auth` on, a handler reached without a tenant is refused
rather than defaulted — the fallback *was* the bug. With auth off the process-wide store is
returned deliberately, and `TestAuthOffStillServesDefaultTenant` guards that side so the fix
cannot be over-applied into a 401 for every single-tenant deployment. That is the same trap
§1.1/§1.2 carry in WS-1's list: the naive fix converts silent corruption into spurious
failure on the legitimate path.

Two cross-tenant **writes** that scoping the store does not close, because they take a tenant
as an *argument* rather than reading one:

- `handleStartWorkflow` read `tenant_id` straight from the request body. Any authenticated
  caller could start a workflow in any tenant by naming it in the JSON. The authenticated
  tenant is now authoritative, and a disagreeing body value is refused rather than silently
  overridden — a caller that asked for another tenant is either misconfigured or probing, and
  quietly writing somewhere other than where they asked is its own bug.
- `handleDeadLetterReprocess` passed `engine.DefaultTenantUUID` literally, so a tenant
  retrying its own dead-lettered workflow moved that run into the default tenant's scope.

**Found on the way.** The sharded startup path built `store` as a `ShardedStore` over all
shards but assigned `factory` the *first shard's* factory (`if i == 0`). Harmless while the
factory served background work; once handlers open stores from it, every tenant-scoped
request would have been narrowed to shard 0 — reads that silently miss data and report
success. `cmd/cleat-worker/sharded_factory.go` opens one store per shard and wraps them.

**The test that mattered failed first, for the right reason.** The DB-backed test
(`tenant_isolation_db_test.go`) initially showed both tenants seeing both rows. Not a defect
in the fix: PostgreSQL bypasses RLS **unconditionally for superusers**, and
`CLEAT_TEST_POSTGRES` conventionally points at one — the postgres image's `POSTGRES_USER`
bootstrap role is a superuser. This is exactly the gap `migrations/postgres/005_app_role.sql`
was written to close, reproduced live. Rebuilt on `testutil.OpenPostgresRLSTestDB`
(NOSUPERUSER, non-owning) it passes, and fails again with the fix reverted.

The general lesson is worth keeping: **a tenant-isolation test that connects as a superuser
proves nothing and looks green.** Any future backend's isolation test must assert the
connecting role is subject to RLS before asserting anything about tenants.

**Still open on §1.7:**

- ~~MySQL and SQL Server isolation tests.~~ **Both written.** MySQL passes against a live
  8.4 (`tenant_isolation_mysql_test.go`), and is kept as a separate test rather than folded
  into a shared multi-dialect one on purpose: MySQL has no row-level security at all, so its
  isolation is entirely structural — a per-tenant *database* — and a shared test would read
  as though it had the same backstop the other two do. It does not, which means on MySQL a
  bug in the HTTP layer is the whole of the exposure. Reverting the fix fails it with
  `Table 'cleat_00000000_0000_0000_0000_000000000000.workflow_instances' doesn't exist` —
  the defect stated in one line, the request served from the default tenant's database.

  SQL Server is written but **skipped, blocked on §2.71** — see below. Unskipping it is that
  item's acceptance test.
- The ~89 unaudited `MySQLStore` `s.tenantID` call sites (see the `requireTenant` note
  elsewhere in this plan). Scoping the store does not audit them.
- ~~Whether the shipped deployment actually connects as `cleat_app` rather than a
  superuser.~~ **Checked 2026-08-04 — it does.** `docker-compose.cluster.yml` gives every
  worker `--db=postgres://cleat_app:...` with a separate superuser `--migrate-db`, and
  `deploy/postgres/900-app-role.sh` refuses to proceed if `cleat_app` turns out to be a
  superuser or to hold `BYPASSRLS`. Worth stating because it is the precondition for
  everything above: the HTTP fix and the RLS policies are *both* no-ops on Postgres against a
  superuser connection. Single-node local runs pointed at `postgres://cleat:cleat@…` do get
  the superuser bypass, which is acceptable for single-tenant development but means a local
  run is not evidence about isolation.

- ~~Also: `migrations/mysql/` and `migrations/mssql/` have **zero** RLS policies against
  Postgres's seven.~~ **Half wrong — corrected 2026-08-04 against a live SQL Server 2022
  (CU26).** Only **MySQL** has zero. `migrations/mssql/001_schema.sql:405-458` defines
  **seven** `CREATE SECURITY POLICY` statements (`TenantFilter_Defs`, `_Instances`,
  `_EventHistory`, `_Signals`, `_Schedules`, `_Tags`, `_Routing`) over an inline TVF keyed on
  `SESSION_CONTEXT(N'tenant_id')` — the same seven tables Postgres covers.

  Applied to a scratch database and exercised, they are real and **fail closed**: with two
  rows under different tenants, each tenant sees exactly its own, and with no session context
  set the table reads as **empty** rather than as everything. `sa` does not bypass them —
  SQL Server filter predicates have no owner exemption, so MSSQL needs no equivalent of
  Postgres's `FORCE ROW LEVEL SECURITY`.

  This changes §1.7's scope materially. MSSQL does not need a policy migration written; it
  needs the *session context actually set per request*, which is the same `defaultTenantID`
  defect above. **MySQL is the only backend with no database backstop** — and it cannot get
  the same one, because MySQL has no row-level security feature at all (`CREATE POLICY` is a
  syntax error on 8.4). Its isolation has to come from the application layer or from
  per-tenant databases, which is what `buildTenantDSN`/`NewMySQLStoreFactory` already gesture
  at. Decide which before writing anything.

  Corollary worth stating: because MSSQL RLS reads as empty rather than erroring when no
  tenant is set, a missing session context on that backend is invisible in exactly the way a
  passing test cannot see. Any §1.7 test must assert *rows returned for the right tenant*,
  not merely "no error".
- ~~Also: the new admin API has no ownership check tying `workflowID` to the caller's tenant.~~
  ✅ **FIXED 2026-08-04.** `callerOwnsTarget` in `cmd/cleat-worker/api_admin.go` now gates all
  three destructive routes: it loads the workflow, compares `TenantID` against
  `auth.TenantIDFromContext`, and answers **404 rather than 403** — 403 would confirm the
  workflow exists, making the endpoint an oracle for valid IDs. Checked once at the router
  rather than in each handler, so a route added later cannot inherit the gap by omission.

  The check was previously left out for a documented reason: an unconditional ownership check
  "would 404 the success-path tests", whose mock store returns no workflow. That is a fixture
  shortcoming deciding a security question, so the fixtures were fixed instead.

  Note what the *existing* admin tests could not catch: none of them put a tenant on the
  request, so `TenantIDFromContext` returns false and the new check short-circuits. They all
  passed before and after the fix. The regression tests set a caller tenant explicitly and
  assert **the store is never reached** — status code alone would accept a handler that
  applied the operation and then returned 404. With the check removed, the audit log shows
  exactly the shape of the bug:

  ```
  WARN admin: force-complete workflow workflow_id=wf-owned-by-a operator=bbbbbbbb-…
  ```

  Tenant B's operation on tenant A's workflow, recorded faithfully and not prevented.
- Test: multi-tenant isolation (see 2.6).

### 1.8 MySQL never worked — fixed in `9fc2a81`

The most severe defect found so far, and the clearest illustration of the thesis.

`engine/mysql_lifecycle.go` passed a zero `time.Time{}` as `p_next_wake_at`.
`go-sql-driver/mysql` encodes that as MySQL's legacy `0000-00-00 00:00:00` sentinel, which
the default `sql_mode` rejects — `NO_ZERO_DATE` and `STRICT_TRANS_TABLES` have been on by
default since 5.7.

It is the *normal* path, not an edge case. `cmd/cleat-worker/setup.go:1704`:

```go
finalStatus := "done"
var nextWakeAt time.Time        // zero
if suspended != nil {
    finalStatus = "ready"
    nextWakeAt = suspended.SuspendUntil
}
```

Every workflow that finishes normally takes it. Verified against a real MySQL 8.4 with the
fix reverted:

```
Error 1292 (22007): Incorrect datetime value: '0000-00-00'
for column 'p_next_wake_at' at row 1
```

**On MySQL, no workflow could reach a terminal state.** Postgres and SQL Server accept a
year-1 timestamp, so nothing else surfaced it.

### 1.9 The shipped schema was not the tested schema — fixed in `e13c2c8`

Same shape as 1.8, on the deployment artifact rather than a code path.

`docs/explanation/postgresql-schema.md` called the root `schema.sql` "the canonical
schema"; `docker-compose.cluster.yml` mounted it into `initdb.d`. No Go code read it — and
none reads `migrations/postgres/` either, because **no migration runner exists**. Every
test built its schema through `engine/testutil`, so the artifact users deploy was covered
by nothing at all.

Verified against a live PostgreSQL 16, applying `schema.sql` exactly as documented:

```
ERROR:  function finalize_workflow_status(...) does not exist
policies: 0        rls_tables: 0
```

`FinalizeWorkflowSegment` calls that function on every workflow completion with no
fallback. **A database built the documented way could not complete a single workflow**, had
no tenant isolation whatsoever, and had none of the `admin.*` tables the Admin API from
\#217 depends on.

`schema.sql` was a strict subset — it contained no table the migrations lack — so it is
deleted rather than repaired. Two hand-maintained copies *is* the defect.

`engine/schema_bootstrap_test.go` now builds a scratch database from the shipped files
alone and asserts the engine's requirements. It found a second defect on its first run:
`003_procedures.sql` had the same 42P13 return-type bug as 004, so re-applying the set —
what an operator upgrading a deployment does, and what the docs promise is safe — failed.

### 1.11 No worker could start against PostgreSQL — fixed in `HEAD`

`cleat-worker` runs `migration.Runner` and `plugin.RunMigrations` at boot and exits if
either fails. Both failed, for two independent reasons, and neither had a single test.

**`SET search_path` leaked out of the migration file and broke the runner's own
bookkeeping.** `2a70373` added `SET search_path = public;` to the four
`migrations/postgres/*.sql` files, to stop the objects landing in a schema named after the
connecting role (1.9). A bare `SET` is *session*-scoped, so it outlived the transaction the
file ran in and changed name resolution for the runner's next statement — an unqualified
`INSERT INTO schema_migrations` on the connection that had just created that table under
the previous `search_path`:

```
[migration] applying 001_schema.sql
ERROR: relation "schema_migrations" does not exist (42P01)
```

That aborted the transaction, rolled `001` back and failed the boot. Every worker in
`docker-compose.cluster.yml` crash-looped. `SET LOCAL` is not available as a fix: the same
files are applied by `docker-entrypoint-initdb.d` through psql, where each statement is its
own implicit transaction and a `LOCAL` setting would be discarded immediately. The runner
now schema-qualifies its tracking table, which makes it independent of anything the
migration files do to `search_path`, and resets `search_path` before returning the
connection to the pool.

**Four workers applied migrations simultaneously.** `CREATE TABLE IF NOT EXISTS` is not
atomic against another session creating the same table, so the compose cluster produced
`duplicate key value violates unique constraint "pg_type_typname_nsp_index"`,
`relation "tenant_api_keys" does not exist`, and
`type "plugin_migrations" already exists`. Both runners now take a `pg_advisory_lock` for
the duration of a run. PostgreSQL only, deliberately: MySQL and SQL Server have
equivalents but no multi-worker topology is shipped for them, and untested locking would
be worse than none.

Why it survived: `migration/` had **no test file at all**, and nothing anywhere ran the
runner against `migrations/postgres/`. The two halves of the bootstrap were each covered
alone — `engine/schema_bootstrap_test.go` applies the files with psql semantics, and the
runner's logic was exercised by nothing — so the seam between them was unobserved. This is
the same shape as 1.9: an artifact that ships is verified by a path that does not.

Both fixes were falsified before being kept. Reverting the qualification reproduces the
verbatim boot failure in all four new tests; removing the lock makes three of four
concurrent runners fail with the exact constraint names seen in CI. New coverage:
`migration/runner_test.go` (5 tests, from nothing) and
`plugin/migration_concurrency_test.go`. End-to-end, four workers now boot against one
database, exactly one applies the migrations, and all four report healthy.

**Plugin tables were created in the wrong schema.** Found immediately after, because the
fix above let the workers boot and so changed what the cluster job's database contained.
`plugin.RunMigrations` applies each plugin's DDL unqualified (`CREATE TABLE kv_store ...`)
on a pooled connection whose `search_path` is the default `"$user", public` — so on the
configuration cleat ships (`POSTGRES_USER=cleat`, and `001_schema.sql` creates a schema
called `cleat`) all 27 plugin tables landed in the role's schema rather than `public`. The
run now happens on a pinned connection with `search_path = public`, reset on release.

`TestPluginMigrations_AllDialects` could not see this, for a reason worth recording: it
asserted "RunMigrations created these tables" while running against the shared
`CLEAT_TEST_DB` database, where the tables already existed. It was passing on evidence it
had not produced. Pointed at a database built from `migrations/postgres/` and nothing else,
it failed on all 27 tables — the assertion only became real once the precondition did.

**The cluster CI job could not tell a running cluster from no cluster.** It started the
compose file, slept 10 seconds and ran the tests. Every worker was crash-looping and the
only symptom was three unrelated-looking store tests failing. `.github/workflows/ci.yml`
now waits for each service's healthcheck and fails on any restart count above zero.

---

### 1.12 Two CI workflows had never run — fixed in `HEAD`

`ai-pr-review.yml` and `release-notes-check.yml` failed on every push, on every branch, for
as long as the branch history shows. Not a flaky job: a **startup failure**, which produces
a run with no jobs at all.

Both files were unparseable YAML. In each, a block indented *less* than its enclosing
block scalar ended that scalar early, and the following text was then read as YAML:

- `ai-pr-review.yml` — a JS template literal inside `script: |` continued at 10 spaces
  where the scalar was at 12.
- `release-notes-check.yml` — a here-document body written at column 0 inside `run: |`.
  `<<-` is not a fix, since it strips tabs only; the step now builds the comment with
  `printf` and posts it with `--body-file`.

So the repository advertised an automated first-pass code review and a release-notes gate,
and had neither. This is the sharpest instance of the pattern this document keeps
recording: not a check that was wrong, a check that never executed.

Nothing inside a workflow can catch this — there are no jobs to run the check in. The lint
job now parses every file under `.github/workflows/` and fails on any that does not load or
has no `jobs:` key. Falsified against the two files as they stood at `4de8f69`: the guard
reports both.

---

### 1.13 Multi-DB CI was green without ever connecting to PostgreSQL — fixed in `HEAD`

The `test-plugin-migrations` job in `.github/workflows/multi-db-ci.yml` declares a
`postgres:16` service and sets `CLEAT_TEST_POSTGRES` to it. The job runs directly on the
runner, not in a container, so it does not share the service network — and unlike the mysql
and mssql services beside it, the postgres service published no port. It was unreachable
for the workflow's entire existence.

The job was green throughout, because `testutil.TestDB` responded to an unreachable
database with `t.Skipf`. Identical in shape to the `test-go` job's missing `ports:` (fixed
earlier this session) and to `DURABLE_TEST_DB` in `cmd/cleat-worker/auth_test.go`: **a skip
that is indistinguishable from a pass.**

Two changes. The service now publishes 5432. And `testutil.TestDB` distinguishes the two
cases it had been conflating: with no DSN configured it still skips, but when a DSN *is*
configured and cannot be reached it fails, with the DSN (password redacted) in the message.
Asking for a database and not getting one is a broken configuration, not an absent one.

Falsified both ways: with an unreachable DSN set the test now fails and names it; with the
environment unset it still skips.

---

### 1.10 RLS was bypassed in every shipped configuration — fixed in `HEAD`

Every tenant-scoped table has row-level security enabled and `FORCE`d, and for
`GetWorkflowByID` and `ListWorkflows` those policies are the **only** tenant isolation
there is: neither carries an application-level `tenant_id` filter. PostgreSQL never applies
RLS to a superuser, and every configuration cleat shipped connected as one —
`docker-compose.cluster.yml` as `POSTGRES_USER=cleat`, CI and local development as
`postgres`. The policies were present, correct, tested, and bypassed in practice by every
connection that had ever run against them.

Demonstrated on one database, two roles, same query:

```
owner (superuser) sees: 2          -- both tenants' rows
as cleat_app, tenant a: 1 rows: a
as cleat_app, tenant b: 1 rows: b
```

Four parts:

1. **`005_app_role.sql`** creates `cleat_app`: owns nothing, no DDL rights,
   `NOSUPERUSER NOBYPASSRLS`, granted only DML plus `EXECUTE`. Ownership matters as much
   as superuser — an owner is exempt from its own policies unless `FORCE` is set, so a role
   that owns nothing is subject to them unconditionally rather than depending on a flag a
   later change could clear. The attributes are re-asserted on every run, so a role someone
   granted `SUPERUSER` to while debugging is corrected rather than preserved.

2. **No credential in the repository.** The role is created `NOLOGIN` and without a
   password; the deployment supplies one.
   `deploy/postgres/900-app-role.sh` does that for the compose file from
   `CLEAT_APP_PASSWORD` and *fails* when it is unset, so a missing password stops the
   deployment instead of quietly leaving it on a superuser connection. It then re-reads
   `pg_roles` and refuses if the role came out with `rolsuper` or `rolbypassrls`.

3. **`--migrate-db`.** `cleat_app` cannot run migrations, by design, and workers migrate at
   boot. The runtime and schema DSNs are now separate; `--migrate-db` defaults to `--db`,
   so an unsplit deployment is unaffected.

4. **A startup check.** `engine.CheckRLSEnforced` reports every way the runtime connection
   escapes RLS: superuser, `BYPASSRLS`, RLS switched off on a table that has policies, or
   ownership without `FORCE` — plus a database with *no* policies, which would otherwise
   pass every other check while isolating nothing. `--rls-check=auto` (default) refuses to
   start when `--require-auth` is set and warns otherwise; `require` always refuses; `off`
   skips.

Falsified from both sides, against the same database: the check reports the bypass on the
superuser connection every configuration used, and reports nothing on an unprivileged one.
A test that could only ever return one answer would fail one of the two. End to end, a real
worker refuses to start on the superuser DSN with the reason and the remedy, and starts
healthy on `cleat_app` logging `row-level security is enforced on this connection`.

**Found on the way:** `cmd/cleat-worker/main.go` counted API keys with
`SELECT COUNT(*) FROM tenant_api_keys`, unqualified. The table is `admin.tenant_api_keys`
and every other PostgreSQL caller says so; the default `search_path` does not include
`admin`, so this always failed with 42P01 and the only trace was a warning. The
auto-generated startup key was therefore **never created on any PostgreSQL deployment**,
while `--require-auth` defaults to true — a fresh cluster had no key and no way in. Now
qualified per-dialect, and logged at ERROR: if that read fails, the auth middleware reads
the same table on every request, so the API is unusable rather than merely missing a
convenience.

---

## Phase 2 — The seam test suite

This is the part that prevents recurrence, and the highest-value work in the plan.
~4–8 sessions total. Each is a CI job.

| # | Test | Catches |
|---|---|---|
| 2.1 | **Golden path.** Clean container, no repo knowledge, execute the README verbatim. | README drift, flag-order bugs, undocumented schema bootstrap, missing `--api-addr`, wrong endpoints — all 8 golden-path failures found today |
| 2.2 | **Two-worker race.** Real Postgres. Claim, stall worker A (SIGSTOP), let the reaper fire, let B claim, resume A. Assert event history intact, no duplicate side effects, no stale parent result. | 1.1, 1.2, 1.6 |
| 2.3 | **Cancellation e2e.** Start a long workflow, `RequestCancellation`, assert it actually stops within N seconds. | 1.3 |
| 2.4 | **Crash recovery.** SIGKILL the worker mid-`DurableCall`, restart, assert the documented semantics (exactly-once vs at-least-once — pick one and assert *that*). | 1.4 |
| 2.5 | **Resource exhaustion.** ✅ **Done** — `tests/exhaustion`, wired into the cluster job. See §2.29. Per-backend coverage is still wasmtime-only, matching what deployments run. | 1.5 |
| 2.6 | **Tenant isolation.** Two tenants; assert A cannot read, list, cancel, or admin-act on B's workflows through the HTTP API. Run against all three backends. | 1.7 |
| 2.7 | **Deploy manifests.** Flag contract ✅ **done**, see §2.27. Actually *starting* them and asserting the worker reaches ready still needs a cluster — open. | `--namespace`/`--tenant-id` crash-loop — confirmed in `k8s/` and `charts/cleat/`, **not** in `docker-compose.cluster.yml` |
| 2.8 | **Dead-code detector.** ✅ **Done** — `scripts/check-test-only-code.sh`. See below; it was indeed the highest-signal cheap check. | 1.4 class — the single highest-signal cheap check |
| 2.9 | **Doc/code consistency.** Assert `ABI.md` version == `wasm/metadata.go:47 CurrentABIVersion`; documented worker flags exist in the binary; documented buffer sizes match `engine/memory.go:39`. | ABI.md claiming v4/5 while code ships v1; the 65536-vs-1048576 buffer mismatch |

Note on 2.2–2.6: these need real databases and process control, so they belong in a
nightly/pre-merge job, not the fast unit lane. Accept the runtime. They are the only tests
that would have caught anything found today.

### 2.8 results — 89 findings, and one that changes a support claim

`scripts/check-test-only-code.sh` runs `staticcheck -checks=U1000 -tests=false ./...`.
Excluding `_test.go` files is the whole trick: anything reachable only from a test then
reads as unused. It cost one command and found 89 entries, 55 of them functions.

U1000 does not flag exported identifiers in library packages, so a public API with no
internal caller is correctly ignored. Everything below is unexported.

Two clusters matter.

**`(*Engine).flushCallIntent` and `(*Engine).completeCallEvent`** (`engine/flush.go:186`,
`:221`) — item 1.4, found automatically. The read side of crash recovery is live and
correct; it searches for a sentinel that nothing writes.

**SQL Server transient-error handling is entirely disconnected.** `mssqlRetry`
(`engine/mssql_retry.go:12`) plus the classification family in `engine/mssql_errors.go` —
`isMSSQLDeadlock`, `isMSSQLDuplicateKey`, `isMSSQLSnapshotError`, `isMSSQLTimeout`,
`isMSSQLRetryable`, `isMSSQLConnectionError`, `mapMSSQLError` — have **no production
caller.** `engine/mssql_retry_test.go` covers this code thoroughly: deadlock retry,
exponential backoff, context cancellation, retry exhaustion, roughly a dozen cases, all
passing. They pass because they are the only callers.

The consequence is a support claim: **on SQL Server, a deadlock is a hard error today.**
Nothing retries it. MySQL has the same shape in `engine/mysql_store.go` with
`isDuplicateKeyError` and `isLockWaitTimeout`. Given that Phase 4 positions MySQL and SQL
Server as the differentiator no competitor covers, this needs wiring before that claim is
made in public.

This is the thesis in one artifact: **a passing test suite is not evidence that code runs.**
Coverage measures what tests reach, and tests reached all of this.

The guard is baselined rather than zeroed — 89 pre-existing entries are recorded in
`scripts/deadcode-baseline.txt` and new ones fail the build. Clearing the backlog is
follow-up work; the point is to stop it growing.

---

### 2.10 `TestIntegrationWorkflowMaxDuration` never tested the duration limit — FIXED

Both defects are closed, and both open questions the plan raised are answered. The headline
finding is that **there was no `DurableCall` ABI bug**: the fixture was malformed and the
host was right to refuse it. What made that take so long to see is a real defect, and it is
fixed too.

**What actually happened.** `testdata/basic`'s `LongRunning` looped on
`h.DurableCall("noop", "", "")` — an **empty operation name**. Service and operation names
are validated against `[a-zA-Z0-9._-]+` (`engine/memory.go` `validServiceName`), so the
host rejected every call on the spot and the loop body never ran. The fixture now calls
`h.DurableCall("noop", "Noop", "{}")` and loops as intended.

**Why it read as an ABI failure.** The refusal was returned as the raw `errBadParam`
sentinel, `0xFFFFFFFF_00000001`. That value is fine for a decoder reading a low byte, but
`cleat_call`'s guest adapter splits the word 24/32/8, so the sentinel lands across all
three fields at once:

```
responseLen   = 0xFFFFFF     (16 MB, against a 64 KB response buffer)
callErrorCode = 0xFF000000   (4278190080 — not a cleat.CallErrorCode at all)
errCode       = 1
```

So a malformed argument surfaced as `[4278190080] cleat_call: error 1 (0=unknown
1=timeout ...)` — a **retryable timeout**, carrying a `Code` matching no enum member, so
every `switch e.Code` on the guest falls through. The oversized `responseLen` was contained
only by the generated `callErrorMessage`'s bounds check; that check was all that stood
between this and a 16 MB out-of-range read.

Fixed by `badParamDurableCall` (`engine/memory.go`), which encodes the refusal in the
layout the caller actually decodes: `responseLen=0`, `callErrorCode=CallErrorInvalidRequest`,
`errCode=1`. Applied to the five host functions whose guest adapter uses that layout —
`cleat_call`, `cleat_call_retry`, `cleat_call_heartbeat`, `plugin_call`,
`plugin_call_streaming` — on **both** backends.

The message the author reads was wrong in the same way, and separately. The generated
`callErrorMessage` was handed `errCode` — the bits 0-7 "did it fail" flag, which is 1 for
essentially every failure — but printed it against a legend enumerating **`CallErrorCode`**
values. So the two halves of the same error contradicted each other:

```
before:  durable call noop.Noop: [4278190080] cleat_call: error 1 (... 1=timeout ...)
after:   durable call noop.Noop: [4]          cleat_call: error 4 (... 4=invalid ...)
```

The three durable-call adapters now pass `callErrorCode`, pinned by
`TestHostAdapterReportsCallErrorCodeNotErrCode` and verified end-to-end through a real WASM
guest. Note the legend is pasted into ~20 other adapter defs where there is no
`callErrorCode` field at all — for those it is decorative and misleading, and removing it is
cosmetic follow-up rather than a correctness fix.

**A second, independent defect found on the way: empty payloads were refused.**
`readWasmStringValidated` treats length 0 as invalid, and every caller turns that into
`errBadParam`. But emptiness is a property of a payload, not a defect in it. A durable call
that takes no arguments could not be made at all. `readWasmPayload` / `wasmtimeReadPayload`
now accept a zero length (still rejecting negative lengths and out-of-range pointers), and
the durable-call family uses them for request payloads. Names and keys keep the strict rule.

**The duration limit now has an honest test.** The workload is a new fixture,
`testdata/spin`: a pure arithmetic loop that allocates nothing and never enters the host.
`LongRunning` is the wrong workload even when fixed — each durable call records an event
costing ~2.9 KB of host memory, so spinning one for a second means ~170k calls and ~500 MB
of heap. Epoch interruption instruments loop backedges, so it fires on a pure loop just the
same. The test asserts the trap *type* (`wasmtime.Interrupt`) rather than matching
substrings, and — the assertion that would have caught the original bug — that execution
actually ran for essentially the whole budget, rather than returning early. Verified by
falsification: with a short workload it fails with `got nil after 159ms`.

Two incidental fixes were needed to get there:

- `engine/executor.go` rebuilt trap errors from `resolveWasmTrap`'s enriched string with
  `%s`, discarding the cause, so `errors.Is`/`errors.As` stopped working for exactly the
  errors carrying the most information. `wasmTrapError.Unwrap` exists to preserve that
  chain; one layer was throwing away what the other kept. Both sites now wrap properly.
- The test no longer touches PostgreSQL. It inserted `workflow_defs` / `workflow_instances`
  rows that `Execute` never read, which put it in the "needs a database" class for nothing.

**Answering the two open questions.**

1. *Is the `cleat_complete` closure warning the same defect?* **No, and it never could be.**
   `cleat_complete` and `cleat_poll_work` are emitted unconditionally by `GenerateImports`
   (`wasm/generator.go`) and are absent from the `hostFunctions` table `AnalyzeUsage` walks,
   so `usage.Used` can never contain them — the warning fires on every Go-target build.
   Confirmed independently: it fires on `testdata/spin`, which uses **zero** host functions.
   Runtime host-function registration does not consult the closure at all; it rescans the
   built binary (`wasm.NeededEnvImports`, `engine/backend_wasmtime.go`). The warning is a
   universal false positive. Silencing it is cosmetic follow-up, not a correctness issue.
2. *Is the wazero `clock_time_get` nil-pointer panic the same bug?* **No — two bugs.** It
   does not reproduce on `LongRunning` at any iteration count, with or without the bad
   arguments. It reproduces only under `CGO_ENABLED=0`, inside the Go wasip1 allocator
   (`mallocgc` → `nanotime1`) during a **successful** `PluginCall` in a different workflow.
   It is the same panic `TestPluginCalls_Wasm_Go` and `TestPluginCalls_MultiDB` already skip
   on, and therefore the same thing `scripts/skip-budget.txt` records as
   `plugin-harness/multi-db 1`. wazero `v1.11.1-0.20260508161934-e6dd6c0c144f`.

---

### 2.11 Three store tests failed against a database live workers were mutating — fixed; one part still open

`TestClaimWorkflow`, `TestClaimSkipLocked` and `TestListWorkflows_ByStatus` failed in the
cluster CI job with `ClaimWorkflow returned nil`, `first claim returned 10, want 3` and
`expected at least 1 result`.

The instrumentation added for this (`describeClaimState`) answered it on its first
failure, which is the whole argument for adding it rather than guessing:

```
claim state: workflow_instances total=10 ready=0 ready+due=0 running=10
claim state:   task_queue="default" status="running" count=10
first claim returned 10, want 3
```

The job ran `go test ./engine/...` against the cluster's *own* database, so the store
tests -- which create rows and then assert on exactly those rows -- shared a table with
four live workers claiming from it. The tests are correct; the setup was not. They assume
exclusive ownership of the table, which nothing sharing a database with a running cluster
can have. The job now gets a database of its own.

Two further findings fell out of that:

- **`go test ./engine/...` runs two packages in parallel against one database.** `engine`
  and `engine/testutil` each build the schema in, and wipe rows from, whichever database
  they are given. On the cluster's already-migrated database this was invisible; against a
  fresh one they raced on the DDL in `001_schema.sql` (a duplicate key on
  `pg_extension_name_index`, and a deadlock) and deleted each other's fixtures. The job
  now runs with `-p 1`.

- **A claim for 3 returned 10 — still unexplained.** All ten rows were left `running`, so
  one statement updated all of them. The suspected mechanism is that PostgreSQL
  re-evaluates an UPDATE's WHERE clause against the new version of a concurrently-modified
  row (EvalPlanQual), and re-evaluating `id IN (SELECT ... LIMIT n FOR UPDATE SKIP LOCKED)`
  re-executes the sublink. `ClaimWorkflows` and `ClaimStickyWorkflows` now select
  candidates in a CTE, which is evaluated once.

  **The suspected mechanism has now been ruled out, and the observation is still
  unexplained.** Investigated as "Start here" item 3; see the correction below.

#### Correction — the EvalPlanQual explanation is wrong

The CTE was introduced on the theory that an EvalPlanQual recheck re-executes the
`id IN (SELECT ... LIMIT n FOR UPDATE SKIP LOCKED)` sublink, letting a claim for n update
more than n. That is not what PostgreSQL does. Two independent reasons, on 16.14:

- **The sublink is executed once.** It is uncorrelated, so the planner pulls it up into a
  semi-join. `EXPLAIN (ANALYZE, VERBOSE)` of the old form shows the candidate subquery as
  the *outer* side of a nested loop, `loops=1`, unique-ified through a HashAggregate, with
  a primary-key index scan on the inner side at `loops=n`. The UPDATE visits exactly the
  candidate rows. EvalPlanQual can only keep or drop a row the UPDATE already visits — it
  cannot add rows to the update set. Same plan shape at 10, 400 and 5010 candidate rows.
- **The candidates are already locked.** The sublink's `LockRows` node takes `FOR UPDATE`
  on them before the outer UPDATE reaches them, so no concurrent transaction can modify
  those rows mid-statement. There is nothing for EvalPlanQual to fire on.

Empirically, against the old form: **24,000 claims**, 12 concurrent claimers, 10 disrupting
transactions committing mid-claim — including ones mutating `status`, which the sublink's
own WHERE clause reads — over candidate sets of 40, 400 and 5010 rows. The largest number
of rows any single claim ever returned was exactly the limit. This covers the scenario 2.11
listed as untried ("a competing UPDATE inside an explicit transaction that commits
mid-claim") and the two it listed as already tried.

Per the plan's own instruction, the CTE is **not** upgraded to "fixed". It stays, because
it is evaluated once by construction rather than by argument, but the comments in
`store_lifecycle.go` and `claim_limit_concurrency_test.go` that asserted the false
mechanism are corrected. A plausible-sounding wrong explanation in a comment is worse than
no explanation: it stops the next person looking.

**What actually caused "asked for 3, got 10" is still unknown.** Ruled out: the SQL in
either form; `ShardedStore` (the test builds a plain `NewPostgresStore`, and its truncation
predates the failure); a retry wrapper (there is none). If it recurs, capture the statement
and its plan rather than reasoning from the row count.

**A recurrence is now capturable, and no longer strands rows.** Nothing in the claim path
checked the invariant, so an over-claim was indistinguishable from a normal claim and the
instruction above could not be followed — the evidence was gone by the time anyone looked.
`enforceClaimLimit` (`engine/claim_limit.go`) is wired into all six claim entry points
(`ClaimWorkflows` and `ClaimStickyWorkflows` × three dialects) via each store's
`finishClaim`. On violation it logs at ERROR with dialect, worker, limit, returned count and
the excess IDs, and **releases the excess back to `ready`** rather than truncating it away.

Truncation is the part that matters: silently dropping the excess is exactly what made §2.17
a bug rather than a nuisance — the rows stay `running` with `assigned_to` set, held by a
worker that will never execute them, until the lease expires.

This is a backstop for a defect believed fixed, not a fix, and it is deliberately cheap: a
length comparison on a path that already allocates a slice per claim. `limit <= 0` is not
enforced, since no caller means "claim zero rows" by it.

- Tests: `engine/claim_limit_invariant_test.go`. The decision is unit-tested including the
  2.11 shape (limit 3, ten rows) and an assertion that the log line carries what a diagnosis
  would need. The release is tested against a real PostgreSQL by claiming ten rows and
  handing `finishClaim` a limit of 3 — the trigger is not reproducible (24,000 attempts
  failed to make the SQL over-claim), so what is tested is the recovery. With the release
  replaced by truncation it fails with seven rows left `status="running"
  assigned_to=worker-over-claim`.

---

### 2.12 The conditional-skip audit — 231 → 184, and three defects behind the skips

Two guards now exist, both shaped after `scripts/check-test-only-code.sh`:

- **`scripts/check-skips.sh`** — every skip site in the tree is baselined as
  `<package><TAB><enclosing func><TAB><count>`. A new skip, or a growing count, fails the
  lint job. A count that *falls* never fails; it prints a note to tighten the baseline.
- **`scripts/check-skip-budget.sh`** — the runtime half, since a static scan cannot see
  that a skip *fired*. Per-job ceilings in `scripts/skip-budget.txt`, checked against
  `go test -json` output. A missing report, or one with no test results at all, fails:
  a job that died before producing output has skipped everything.

The three `Warn on skipped tests` steps are gone. A warning on a green job is not a signal
— the multi-DB Postgres bug (1.13) emitted one for its entire existence.

**The grep in §1 undercounted, in both directions.** 225 included four prose comments
discussing `t.Skipf`, and missed the two `unavailable := t.Skipf` sites where the skip is
taken as a *function value* — which is the shape of the already-fixed `TestDB` pattern
itself — plus five in `engine/testutil/`, which is not a `_test.go` file but decides
whether every database-backed test in the repo runs. Real total: 231. The guard counts all
three forms.

**Three defects were behind skips, each found by converting one.**

1. **MySQL had no unauthenticated-query rejection at all.** `TestUnauthenticatedQueryRejection`'s
   type switch had no `case *MySQLStore`, so the MySQL subtest fell to `default:` and
   skipped — unconditionally, every run, including in `multi-db-ci.yml`'s `test-mysql` job,
   which exists to test MySQL. Writing the case proved the gap against a real MySQL 8.4:
   `GetActiveInstanceCountsByVersion` with an empty `tenantID` returned **no error**,
   just an empty result. `MSSQLStore` has had this check since it was written
   (`setSessionContext`); MySQL had **90 references to `s.tenantID` and not one guard**.
   Since MySQL also has zero RLS policies (1.7), nothing else was scoping the query.
   `requireTenant` added and applied to that one method. **The other ~89 call sites are
   not audited** — that is 1.7, and the helper's doc comment says so explicitly rather
   than letting its presence imply the problem is solved.

2. **33 skips that could not mean what they said.** `engine/backend_wasmtime_test.go` and
   `..._limits_test.go` are `//go:build cgo`; the `!cgo` stub returning "requires CGO" is
   unreachable from them. So `t.Skipf("wasmtime backend not available")` could only fire
   on a genuine init failure of the **primary** backend — silently deleting the whole
   suite, including the four regression tests for the runaway-workflow hang (1.5). Now
   `t.Fatalf`.

3. **`TestVetPython` was vacuous in every environment.** `runVetPython` exits 0 when it
   finds violations, so the test's `if err != nil { skip }` never meant "vet found the
   violation" — it meant `cleat_sdk` was unimportable, which it always was, because
   `findPythonSDKDir()` resolves relative to a cwd that is never the repo root under
   `go test`. The non-skip branch asserted nothing either. It now sets `PYTHONPATH` and
   asserts `PY002` is present; deliberately breaking the expectation was confirmed to fail it.

**Found on the way, not fixed — these are the honest leftovers.**

- **Six of the seven `tests/` suites are run by nothing.** `tests/cluster`, `integrity`,
  `upgrade`, `soak`, `scale` and `cross-language` are named by no workflow file. The
  `cluster` CI job runs `./engine/...`, not `tests/cluster/`; `e2e-cross-language.yml`
  runs `./engine/...` with a `-run` filter, not `tests/cross-language/`. Only
  `tests/plugin-harness` is actually executed. Note this contradicts `f4322e3`'s claim
  that `tests/integrity` and `tests/cross-language` "now actually execute" — they execute
  if run, and nothing runs them.
- **`check-ci-package-coverage.sh` exempted `tests` on the stated grounds that its suites
  are "driven by their own dedicated CI jobs".** That was an assertion, not a check, and
  it was false — the same rot the guard exists to catch, inside the guard's own exemption
  list. It now verifies the claim, with the six unwired suites baselined.
- **`plugin-harness-ci.yml`'s `test-multi-db` job is entirely vacuous.** It provisions
  PostgreSQL, MySQL and SQL Server, sets all three DSNs, and runs exactly one test —
  `TestPluginCalls_MultiDB` — whose first statement is an unconditional `t.Skip` for a
  wazero v1.11.1 nil-Sys panic. Budgeted at 1 so it is recorded rather than breaking the
  build; drop to 0 when the skip goes.
- **`Makefile`'s `test-cluster` ran `./internal/host/...`**, a path dead since `3eeb74e`.
  Repointed at `./engine/...` with `-p 1`, matching the CI job.
- **`TestMySQLStoreFactory` gates on `CLEAT_TEST_MYSQL` and then ignores its value**,
  hardcoding `tcp(127.0.0.1:3306)`. Harmless in CI, but the same config-drift family.
- **`plugins/scheduler`'s `TestNextRun_Feb29NonLeapYear`** is not environment-conditional
  at all: `nextRun`'s one-year search window cannot find a Feb 29 more than a year out.
  A real scheduler gap wearing a skip's clothing.

**What did not change, deliberately.** The 112 `mysql` and 112 `mssql` dialect subtests
that skip in the `engine` job are correct: that job configures PostgreSQL only. A skip is
allowed to mean "nobody asked for this" and nothing else — that is the whole rule, and
the budget of 347 records those rather than hiding them.

Budgets: core 0, engine 347, cluster 347, wasm 1, internal 0, plugins 1, support 2,
commands 4, fuzz 1, plugin-harness/multi-db 1.

**Two were wrong on the first CI run, in a way worth recording.** Both were seeded from a
local machine and both failed for the same underlying reason — a budget is a claim about
an environment, and the environment used to measure it was not the one it describes.

- `engine` was seeded at 343 from a darwin machine that had **cargo installed**. The four
  `TestRustWorkflow*` tests passed there and skip on the runner, which installs Rust only
  for the `internal` matrix entry. The measuring environment was *richer* than CI, so the
  budget was too tight. Corrected to 347.
- `cluster` was seeded at 0 on the reasoning that a job bringing up four workers and a
  database has provisioned everything its tests need. That was wrong about *which* tests
  it runs: it executes `go test ./engine/...`, which carries the entire MySQL and SQL
  Server suite, and the job configures neither. Corrected to 347. Cluster health is
  asserted by the healthcheck and restart-count steps, not by the skip count.

Neither run had a single test *failure* — `passed=2984 failed=0` in both. The guard did
exactly what it was built to do on its first outing, which was to disagree with a number
somebody had asserted without observing.

---

### 2.13 Empty-string payloads were refused across the rest of the ABI — FIXED

`readWasmStringValidated` treats a zero length as invalid and every caller turns that into
`errBadParam`, so any parameter whose emptiness is meaningful was unreachable from a guest.
2.10 fixed the durable-call family; this closes the rest. 15 parameters on each backend, 30
call sites, all verified against the handler code rather than assumed.

**Three documented behaviours were unreachable.** These are the real bugs, not tidiness:

| host function | parameter | what could not be asked for |
|---|---|---|
| `cleat_set_scope` | `objectType` + `instanceKey` | clearing the scope (`scope.go` `freshSetScope`) |
| `cleat_list_state` | `prefix` | listing every key — `HasPrefix(k, "")` is true for all `k` |
| `cleat_child_workflow_in_schema` | `targetSchema` | the local-schema fallback (`children.go`) |

**And one live backend divergence.** `cleat_child_workflow_in_schema`'s `parentClosePolicy`
was guarded by an inline `policyLen > 0` on wazero and read unconditionally on wasmtime, so
the same guest call succeeded on one backend and was refused on the other. Its sibling
`cleat_child_workflow_with_options` guarded it on both, which is what makes this a slip
rather than a decision.

`readOptionalServiceName` / `wasmtimeReadOptionalServiceName` handle the name-shaped cases:
they relax *only* emptiness, so a non-empty value still has to pass the `[a-zA-Z0-9._-]+`
check. The payload-shaped cases use the existing `readWasmPayload` / `wasmtimeReadPayload`.

The remaining 12 parameters are payloads whose handlers store or forward them opaquely:
`cleat_log` message (`DurableLog` does not read it at all), `cleat_set_state` /
`set_query_state` value, `cleat_uuid` seed (concatenated into a hash input),
`cleat_side_effect` result (compared with `!=` on replay, so `""` round-trips),
`cleat_resolve_promise` value, `cleat_reject_promise` errMsg, the three signal payloads,
`cleat_send` / `cleat_schedule_invoke` requestJSON, `cleat_defer` description, and
`cleat_fetch` body / headersJSON.

Two things checked specifically because they looked like they could break, and did not:

- Signal payloads reach a JSONB column, but `engine/store_signals.go` already wraps a
  non-JSON payload with `if !json.Valid(...)`, so `""` becomes the valid JSON literal `""`.
- `cleat_fetch`'s `headersJSON` is never unmarshalled — because **there is no production
  implementation of the `Fetcher` interface in this repo at all**. Only test stubs
  implement it, and nothing in `cmd/`, `cleat/` or `plugins/` wires one. So the earlier
  claim that "a GET with no body is impossible" was true but moot: the whole `cleat_fetch`
  path is unimplemented in production. The ABI boundary is fixed regardless, so a future
  `Fetcher` starts from a correct contract — but that implementation should guard its own
  `json.Unmarshal(headersJSON)` against `""`.

**Four tests asserted the bug as the contract** and were updated, not worked around: the two
`cleat_log` empty-message tests now assert acceptance, and the `cleat_side_effect` and
`cleat_defer` rows were removed from the all-zero-argument error tables (as `cleat_list_state`
was). Every other `errBadParam` assertion still holds, because those calls fail on an earlier,
still-required name parameter before reaching the relaxed one.

New ABI-level tests drive the *registered host function* with a real `execSession` behind it
for all three unreachable behaviours, and all three fail against the unfixed readers. That
level matters: `TestSetScopeEmptyClears` had asserted the clear-scope path for a long time by
calling `execSession.SetScope` directly, and passed throughout, because the gap was in the
seam between wrapper and handler rather than in either one. See 2.16.

---

### 2.14 `cleat_json_parse` / `cleat_json_stringify` panicked on the primary backend — FIXED

A nil-pointer dereference, reachable from real guest code: the Rust SDK calls
`cleat_json_parse` (`crates/cleat-sdk/src/host_calls.rs`).

`engine/wasmtime_hostfuncs_core.go` passed a literal `nil` for the `api.Module` argument
and `engine/lifecycle.go` immediately called `m.Memory()` on it. The `nil` was not the bug —
it is the wasmtime convention, documented on `writeResult` in `engine/flush.go`: the memory
travels in the context instead. The bug is that these two handlers were the **only** ones
that read their own input out of guest memory rather than being handed a decoded string,
so they were the only ones that reached for `m` on a path where it is deliberately nil.

Fixed by removing the anomaly rather than patching around it. `JsonParse` and
`JsonStringify` now take `input string`, each backend's wrapper reads it the way every
other wrapper already does, and output goes through the context-aware `writeResult`. wazero
was unaffected throughout (it passes a real module) and stays that way.

**Why nothing caught it.** `TestClosure_JsonParse` and `TestClosure_JsonStringify` existed
and passed. They installed `mockHostHandler`, whose `JsonParse` returns a canned `0` without
touching memory, wrote an input into guest memory, and then asserted only that the result
was `0` — which is what the mock returns unconditionally. They would have passed if the
function did nothing at all, and they did pass while every real call crashed. Both are
deleted and replaced by tests that drive the real `execSession` through the same
registration path and assert on the bytes written back, on **both** backends. Verified by
falsification in the honest direction: the new wasmtime test panics against the unfixed
handler.

---

### 2.15 Durable call failures are all classified as retryable timeouts — ✅ **FIXED** (with a bounded residual, 2.35)

`cleat.CallErrorCode` exists "so callers can distinguish retryable from non-retryable errors
without string-matching" (`cleat/runtime.go`). It cannot currently do that. Every failure
path in `engine/durablecalls.go` and `engine/heartbeats.go` packs `callErrorCode = 1`,
which the guest enum reads as `CallErrorTimeout` — **retryable** — whether the underlying
cause was a service error, a replay divergence, a cancellation or an ambiguous result.
`engine/plugins.go` packs `0` (`CallErrorUnknown`) throughout instead.

So a permanent failure is reported to workflow authors as a transient one.

**Fixed 2026-08-04.** The failures the *engine* produces are now classified honestly, and the
one that cannot be is documented rather than guessed at.

Non-retryable now (`CallErrorUnknown`), where all three used to say "timeout, try again":

| Failure | Why retrying is wrong |
|---|---|
| Workflow cancelled | Repeating the call is the one thing the caller must not do |
| Replay divergence | A bug in the workflow code; the same call diverges again |
| Ambiguous outcome | The call **may already have succeeded** — retrying risks a duplicate side effect |
| No plugin registry configured | A deployment problem; no amount of retrying supplies one |

A call the *service* failed keeps reporting as retryable (`CallErrorUnavailable`), deliberately:
the previous hardcoded `CallErrorTimeout` was retryable too, so nothing branching on
`Retryable()` changes behaviour. What changed is that it stops claiming the call timed out when
the engine has no idea what happened.

**A separate defect found on the way.** `DurableCallWithRetry` returned
`packDurableCallResult(0, 0, 0)` when the context was cancelled mid-backoff. The generated
guest adapter branches on `errCode != 0`, so an abandoned retry loop reached the workflow as a
**successful call with an empty response**. It now returns the context error with a nonzero
`errCode`.

### 2.35 The call error class is not persisted, so replay cannot recover it — 🔶 **PARTLY FIXED**

The constraint that bounds §2.15, and it is a real one rather than an excuse.

A recorded call failure is replayed from `EventRecord.Err` — a bare string. If the fresh path
derived a classification the replay path cannot, **the same step would be retryable on the
first run and non-retryable on the replay of it**: a determinism bug in the engine, introduced
in the name of better error reporting. So every *recorded* failure — a plain call failure,
retries exhausted, a plugin function erroring — has to use one constant on both paths, and
does. `TestFreshAndReplayAgreeOnRecordedFailure` drives both and requires them to match.

The same constraint forces the streaming plugin family to a single code: every stream failure,
whether a missing registry, a blocked guard or the function itself erroring, is recorded by
`recordStreamError` and comes back through one replay site that cannot tell them apart.

**The fix is to persist the code alongside the event** (a column, or a field in the `payload`
JSONB). Once it round-trips, classification at the `ServiceCaller` boundary becomes possible —
a caller that knows a 404 from a connection reset can say so, and replay will agree.

Deliberately **no mechanism was added ahead of that**. An interface nothing can call yet is
how `engine/flush.go` accumulated 350 lines of durability code that had never run (§1.4,
`docs/durable-call-intent-design.md`).

**Update (2026-08-04): the constraint was hiding a live contradiction, and that half is
fixed.**

Re-reading the call path to scope this turned up something the section had missed. There *is*
already a machine-readable signal a `ServiceCaller` can send — `RetryableError`
(`engine/types.go:301`), a duck-typed `Retryable() bool` that any error may implement — and
`isDefinitelyNonRetryable` (`engine/helpers.go:89`) already honours it. `DurableCallWithRetry`
calls it at `durablecalls.go:249` and **breaks out of the retry loop** when it says the error
is not worth retrying.

And then reported `callFailureCode` — `callErrorUnavailable`, which `cleat.CallError.Retryable()`
says **is** retryable.

So the engine stopped retrying *because the error was non-retryable*, and then told the
workflow the call was retryable. A workflow branching on `err.Retryable()`, which is precisely
what the guest SDK offers, goes on to retry a call the engine has already decided against. For
a non-idempotent operation a caller marks non-retryable, that is a duplicate side effect.
Demonstrated before fixing:

```
engine stopped retrying because the error is non-retryable; it reported code 2, Retryable()=true
```

This could not be fixed without the persistence this section is about — that part of the
original diagnosis was exactly right. Classifying on the fresh path alone would make the same
step non-retryable on the first run and retryable on the replay of it.

**What landed.** `EventRecord.ErrNonRetryable`, round-tripped through the `payload` JSONB
(`error_non_retryable`, written only when true). Both paths now go through one function,
`recordedFailureCode`, so they cannot drift.

Three deliberate choices:

- **A bool, not a code.** It is the only part of a classification the engine can populate
  today, and a code field's zero value would collide with `callErrorUnknown`, which is a real
  class. The bool's zero value is instead exactly the pre-2.35 behaviour.
- **No migration.** `payload` is JSONB. Adding a key changes checksums only for newly written
  events; existing rows keep their stored payload and still verify.
- **`callErrorUnknown`, not `callErrorInvalidRequest`.** Both are non-retryable, which is the
  bit that matters — but the engine does not know *why* the caller declined the retry, and
  `InvalidRequest` would tell the author their request was malformed, a claim nothing supports.

Backward compatibility is asserted, not assumed: `TestLegacyFailureReplaysAsRetryable` drives a
payload with no such key and requires `callFailureCode`, because every call failure in every
existing `event_history` was written that way and upgrading must not change the retry behaviour
of workflows already in flight. `TestFreshAndReplayAgreeOnNonRetryableFailure` replays *the
event the fresh run actually recorded* rather than a hand-written literal — a literal would let
the test agree with itself while the writer and reader disagreed. Each of the three arms was
verified by breaking it.

**Update: the worker's caller now classifies.** With the persistence in place, the
`ServiceCaller` boundary was unblocked, and `dbServiceCaller` (`cmd/cleat-worker/setup.go`) —
the only `ServiceCaller` that runs in production — now returns `engine.NewPermanentError` /
`engine.NewTransientError` instead of bare `fmt.Errorf`. No new type: `CleatError` already
implements `Retryable()`, which is what `isDefinitelyNonRetryable` consults.

The case that mattered: **`service %s.%s not configured: no endpoint registered`**. A workflow
calling a service that does not exist burned its entire retry budget on a deployment mistake,
with backoff, and was then told the failure was retryable — so a workflow with its own retry
wrapper went round again, forever. It now fails on the first attempt and says so.

Classified as permanent: unconfigured service, malformed `http.fetch` request (bad JSON,
missing URL, invalid method), and 4xx from bench-svc except 408 and 429. Transient: connection
failures, response-read failures, and every 5xx. Error messages are byte-identical to before,
because operators grep them and `DurableCallWithRetry`'s `nonRetryableErrors` patterns match on
substrings.

Two things pinned deliberately. `TestHTTPFetchNetworkFailureStaysRetryable` guards the
over-eager direction — marking a failed connection permanent would turn every transient blip
into a failed workflow. `TestHTTPFetchStatusIsNotAnError` pins that `http.fetch` reports the
status *in its response*, so a 404 is a successful call that returned 404 and is not classified
at all.

**Still open: the richer taxonomy, and plugins.** `Retryable()` is one bit; `ErrorCode` carries
seven values that still have no path into the event history, because
`EventRecord.ErrNonRetryable` is a bool by design (see above). The streaming plugin family (`recordStreamError`) remains
single-coded, and `PluginError` is still a bare string on replay.

---

### 2.16 Most wasmtime closure tests cannot see a handler defect — FIXED

Generalising the previous item. 34 of the 48 `TestClosure_*` tests in
`engine/backend_wasmtime_test.go` follow the shape that hid 2.14: call the host function
with valid arguments and assert the result is `0`, against `newClosureSetup`'s
`mockHostHandler{ret: 0}`.

They are not worthless — a wrapper that wrongly rejected its arguments would return
`errBadParamInt64` and trip the assertion, so they do cover registration, the import
signature, and the wrapper's validation on the happy path. What they cannot cover is the
**handler**, because the mock replaces it. Any defect on the far side of that boundary —
2.14's nil dereference being the extreme case — is invisible to them.

The fix is not to rewrite all 34. It is to decide, per host function, whether its handler
behaviour is worth a test that drives the real `execSession`, as
`json_hostfuncs_cgo_test.go` now does, and to stop treating the `TestClosure_*` family as
evidence that a host function *works*. It is evidence that it is *wired up*.

**Resolution (2026-08-04).** Kept the family and made "wired up" an assertable claim
instead of an unasserted one. `mockHostHandler` now records every call — method name and
the string arguments the wrapper decoded out of guest memory — and `closureSetup.expectCall`
asserts the wrapper reached the handler *exactly once*, as the *expected method*, carrying
the strings the test wrote. 36 assertion sites converted.

This is deliberately narrower than driving the real `execSession`, which stays the right
tool for handler *behaviour* (§2.14's JSON pair, §2.18's ID functions). What it does cover
is the seam these wrappers exist to implement, and it now fails on all three ways that seam
breaks. Verified by injecting each defect against the new assertions:

| Injected defect | Result |
| --- | --- |
| wrapper returns without calling the handler | `want exactly 1 host-handler call to HasState, got 0` |
| `keyLen-1` when decoding the argument | `HasState received string args ["cleat_has_stat"] … do not include "cleat_has_state"` |
| wrong handler method wired up (`DeleteState` for `HasState`) | `host handler saw DeleteState, want HasState` |

A fourth attempt — deleting the `h.HasState(...)` call outright — left `key` unused and
failed to *compile*, which proves nothing about the assertion. That trap is worth naming:
a revert that breaks the build is an inconclusive check, not a passing one.

**It immediately found 10 live defects — in the tests themselves.** Ten call sites passed a
byte length that did not match the string literal they had just written into guest memory,
so the handler was receiving truncated or NUL-padded arguments: `{"p":"load"}` sent as 11
bytes, `{"in":"put"}` as 14, `["run-1","run-2"]` as 19, `cleanup-task` as 13. Every one had
been wrong since the test was written and passed the whole time, because `got == 0` is what
the mock returns no matter what arrives. That is the §2.16 shape producing bad tests rather
than hiding bad code, and it is the more insidious direction — those tests were *reporting*
coverage of argument decoding while feeding the decoder inputs nobody had checked.


### 2.17 `ShardedStore` claims `limit` from *every* shard and strands the excess — ✅ **FIXED**

Found while investigating 2.11. This is a real over-claim, in production-wired code
(`cmd/cleat-worker/main.go`), and it is not the one 2.11 was chasing.

`ShardedStore.ClaimWorkflows` and `ClaimStickyWorkflows` fan out to every shard
concurrently, passing each the **full** limit, then truncate the merged slice:

```go
wfs, err := sh.Store.ClaimWorkflows(ctx, workerID, limit)   // every shard, full limit
...
if len(all) > limit { all = all[:limit] }                   // return value respects it
```

The return value respects the limit, which is why nothing noticed. But the rows beyond it
have already been updated to `status='running'` with `assigned_to` set to this worker, in
their own shards, in committed transactions. Truncating the slice does not release them:
they are claimed by a worker that will never run them, and stay that way until the lease or
heartbeat reaper takes them back.

With S shards and limit L, one poll can strand up to `(S-1)*L` workflows. Demonstrated by
`TestShardedClaimWorkflows_OverClaimsAcrossShards`: 3 shards, limit 2 — **claimed 6,
returned 2, stranded 4**. A single-shard deployment cannot hit it, which is presumably how
it survived.

Note that `ClaimWorkflows`'s own doc comment describes the correct behaviour — "Iterates
through shards collecting workflows until limit is reached or shards exhausted" — which is
not what the code does.

Options, none of them free:

1. **Sequential with a decreasing budget**, which is what the doc comment already claims.
   Correct and simple; serialises the fan-out on a hot polling path.
2. **Apportion the limit** across shards up front. Keeps the parallelism; under-claims when
   ready work is skewed towards one shard.
3. **Release the excess** after truncation. Keeps both, but needs an unclaim path and is
   racy against the reaper.

**Fixed 2026-08-04 with option 1**, plus a rotating start.

The latency objection to sequential does not survive being looked at. Claims stop as soon as
the budget is spent, so **when work is available the first shard usually fills it and the loop
does one round-trip — fewer than the fan-out made**. The serial walk only happens when the
shards are empty, which is exactly when claim latency does not matter. The parallel fan-out was
optimising the case that matters least, and paying for it with stranded work in the case that
matters most.

Option 2 (apportion up front) under-claims under skew: a worker with capacity 10 across 5
shards asks each for 2 and gets 2 when only one shard has work. Option 3 (release the excess)
needs an unclaim path and races the reaper.

The one thing sequential does introduce is unfairness — a fixed starting shard drains shard 0
first and starves the tail under sustained load. `claimCursor` rotates the starting shard per
call; `TestShardedClaimWorkflows_RotatesStartingShard` covers it.

Both `ClaimWorkflows` and `ClaimStickyWorkflows` now go through one `claimAcrossShards` helper,
which also **errors** if a shard returns more than its budget rather than truncating. Truncating
is what hid this for its whole existence, and the excess is already committed in that shard.

`TestShardedClaimWorkflows_DoesNotOverClaim` asserts on what reached the *stores*, not on what
was returned — the returned slice was correct throughout, which is precisely why the defect was
invisible. Restoring the fan-out:

```
--- FAIL: TestShardedClaimWorkflows_DoesNotOverClaim/ClaimWorkflows
    6 rows were claimed in the shard databases but only 2 were returned:
    4 workflows are 'running' with no executor until the reaper takes them back
--- FAIL: TestShardedClaimWorkflows_DoesNotOverClaim/ClaimStickyWorkflows
    (the same)
--- FAIL: TestShardedClaimWorkflows_RotatesStartingShard
```

---

### 2.18 Six wasmtime host functions fetch the guest memory and throw it away — ✅ **FIXED (and it was 18, not 6)**

> **Confirmed empirically, then fixed across the whole class.** A closure test with the real
> `execSession` installed shows `cleat_workflow_id` returning raw result
> `0x0000000000000000` — errCode 0, zero bytes written. Success by every signal except the
> one that matters.
>
> **The count in the heading was wrong.** Auditing all four `wasmtime_hostfuncs*.go` files
> rather than just `_core.go` found **31 wrappers with an out-parameter, 19 of them missing
> `ctxWithMem`**. Eighteen were fixed; the nineteenth, `cleat_poll_work`, is a true negative
> — it copies into `buf` itself and never calls a handler.
>
> Fixing only the six named here would have repeated §2.14's mistake exactly: fix the known
> instances, leave the siblings, wait for the next round to find them. That has now happened
> twice (§2.14 → §2.18), so the fix is the class, not the list.
>
> **Guard added**, because runtime coverage cannot close this cheaply — most of these
> handlers need a live engine, store and session to reach their write path, which is
> precisely why the existing closure tests settle for `mockHostHandler` (§2.16).
> `TestWasmtimeWrappersPassGuestMemory` is a source-level invariant instead: any wrapper with
> an out-length parameter must either pass `ctxWithMem` or write into `buf` directly. It
> covers all 31 today and every wrapper added later. Verified non-vacuous by reverting one
> wrapper and watching it name the regression.
>
> Note on method: a first pass tried to decide which handlers write by transitively chasing
> `writeResult` through the call graph. It returned "18 of 18 affected", which is not a
> result — a depth-6 walk through shared helpers implicates nearly everything, including
> error-only paths. That was discarded in favour of the empirical test plus a uniform rule.
>
> The original framing is kept below.

#### Original framing

Found while assessing PR #208 for salvage (see the salvage register below), and verified
directly against `develop`. **This is the same defect as §2.14, in six more places.** §2.14
fixed `cleat_json_parse` / `cleat_json_stringify`; nobody checked whether the pattern
repeated. It does.

`writeResult` (`engine/flush.go:17`) writes through one of two channels: a raw buffer
carried in the context under `wasmMemBufKey{}`, or `m.Memory()`. On the wasmtime backend
`m` is **always** `nil` — memory travels in the context. So a wasmtime wrapper that does not
call `ctxWithMem` cannot write anything, and `writeResult` returns `(0, nil)` — no error.

`engine/wasmtime_hostfuncs_core.go` gets this right for the two functions §2.14 touched:

```go
callCtx := ctxWithMem(context.Background(), buf)          // line 365, cleat_json_parse
return h.JsonParse(callCtx, nil, input, ...)
```

and wrong for six others. `registerCleatWorkflowID` is the clearest case — it fetches the
buffer purely to test the error, then discards it:

```go
_, _, err := callerMemBuf(caller)                          // line 111: buf dropped on the floor
if err != nil {
    return errBadParamInt64
}
return h.WorkflowID(context.Background(), nil, uint32(idPtr), uint32(idMaxLen))
```

The handler then does `written, _ := s.writeResult(ctx, m, ...)` with `ctx` empty and `m`
nil, gets `0`, and returns `packSimpleResult(0, 0)` — **errCode 0, length 0. Success, no
bytes.** Affected, all in `engine/wasmtime_hostfuncs_core.go`:

| line | host function | guest sees |
|---|---|---|
| 97 | `cleat_uuid` | empty string |
| 113 | `cleat_workflow_id` | empty string |
| 129 | `cleat_run_id` | empty string |
| 229 | `cleat_get_state` | empty value — indistinguishable from "unset" |
| 310 | `cleat_list_state` | empty key list |
| 343 | `cleat_fetch` | empty response body |

`cleat_get_state` is the one to worry about: an empty read is not obviously wrong to a
workflow, so this corrupts state-machine logic silently rather than failing.

**Why no test caught it — this is §2.16 again.** `TestClosure_UUID`, `TestClosure_WorkflowID`
and friends (`engine/backend_wasmtime_test.go:1950-2100`) install
`b.handler = &mockHostHandler{ret: 0}` and assert only `got != 0`. The mock returns a canned
value without touching memory, so the assertion is vacuous — exactly the shape §2.14
documented for the JSON pair. §2.16 is now less a hypothesis than a confirmed generator of
defects; it should be promoted above the remaining Phase 2 work.

Fix: `callCtx := ctxWithMem(context.Background(), buf)` at all six sites, and port the
`json_hostfuncs_cgo_test.go` pattern — real `execSession`, assert on the bytes written — to
cover them. The patch applies cleanly to `develop` today.

### 2.19 `WorkflowID` / `RunID` decode the wrong half of the result word — ✅ **FIXED**

> **Fixed.** `wasm/adapter_metadata.go` now generates `uint32(uint64(result) >> 32)` for both,
> matching the thirteen sibling entries and `decodeExportResult` in `engine/memory.go:259`,
> which is unambiguous: `errCode = low 32, actualLen = high 32`.
>
> `Version` and `MinVersion` keep `uint32(result)` and are **not** defects — those host
> functions return a plain value, not a packed length/errCode word.
>
> `TestWasmtimeIDResultLayout` pins the ABI contract on the host side, so flipping the layout
> fails a test rather than silently un-fixing the generated adapter. The masking noted below
> is real and was the reason to do both halves in one change.
>
> The original framing is kept below.

#### Original framing

Independent of §2.18, same two functions, and it would still bite after §2.18 is fixed.

`packSimpleResult` (`engine/memory.go:247`) packs the written length into the **high** 32
bits and the error code into the low bits:

```go
v = uint64(extra[0]) << 32
return int64(v | uint64(errCode))
```

Every generated adapter in `wasm/adapter_metadata.go` decodes that correctly —
`uint32(uint64(result) >> 32)` — at thirteen call sites. Two do not:

```go
"idLen := uint32(result)",                    // lines 338 and 346
"return unsafe.String(&idBuf[0], int(idLen))",
```

`uint32(result)` takes the **low** half, which is the error code. On success that is 0, so
the guest builds a zero-length string: `WorkflowID()` and `RunID()` return `""` on the
Go target regardless of what the host wrote. The file's own thirteen-to-two split is the
proof — no reasoning about the ABI is needed, just contrast.

Fix is two characters of shift, but it should land with a guest-level assertion, not just a
host-level one; §2.18 and §2.19 mask each other, and fixing either alone leaves
`WorkflowID()` still returning `""`. That mutual masking is probably why neither was noticed.

### 2.20 Child-workflow spawning inserts an event with no `tenant_id` — ✅ **CONFIRMED and FIXED**

> **Reproduced, then fixed.** Driving the real `PostgresStore` through
> `testutil.OpenPostgresRLSTestDB` (a role that is neither superuser nor table owner)
> against PostgreSQL 16.14, `StartChildWorkflowAtomic` fails outright:
>
> ```
> start child workflow atomic: insert event:
> pq: new row violates row-level security policy for table "event_history" (42501)
> ```
>
> Rejected, not defaulted — the schema reasoning below held. A raw-SQL A/B isolated the
> cause to the single column: the `store_children.go` form is rejected, the
> `store_event_write.go` form with `tenant_id` supplied returns `INSERT 0 1`, everything
> else identical.
>
> **It is live in the shipped multi-tenant configuration.** `StartChildWorkflowAtomic`
> calls `setRLSOnTx(tx)` at `store_children.go:55` — it activates the tenant GUC on the
> very transaction whose next insert omits the column. `migrations/postgres/005_app_role.sql:68`
> makes `cleat_app` `NOSUPERUSER … NOBYPASSRLS` and non-owning, `deploy/postgres/900-app-role.sh`
> raises if it ever gains an exemption, and `ci.yml:810` connects cluster workers as it.
>
> **Correction to the caveat below:** it says "an owner connection … inserts the zero-UUID
> successfully." That is wrong. `FORCE ROW LEVEL SECURITY` exists precisely to apply RLS to
> the table owner, so an owner connection with `cleat.tenant_id` set is rejected too. Only a
> **superuser** bypasses, forced or not. The exemption is narrower than this entry claimed.
>
> **Why CI never saw it** — the two halves miss each other. Test DSNs point at `postgres`,
> a superuser, so RLS is a no-op in every existing test; and the one deployment that does
> enforce RLS has no child-workflow coverage at all (`grep -rl child tests/cluster/` is
> empty).
>
> Fix: `tenant_id` added as `$9`, matching the sibling insert. Safe — `setRLSOnTx` already
> refuses an empty `tenantID`, so the insert is unreachable with one. Regression test at
> `engine/store_children_rls_test.go`, verified to fail against the unfixed code and pass
> with the fix. The `engine` package is green.
>
> The original framing is kept below.

#### Original framing

`StartChildWorkflowAtomic` (`engine/store_children.go`) does two inserts in one transaction.
The first passes `tenant_id` (`$7 = s.tenantID`, line 70). The second, into `event_history`
at line 91, **omits the column entirely**:

```go
INSERT INTO event_history (workflow_id, step, event_type, child_name, child_input,
                           run_id, created_at, checksum)
```

Every other `event_history` insert in the engine passes it — `engine/store_event_write.go:46`
and `:86`, `engine/flush.go:41`. This one site is the outlier.

The consequence is more severe than "the row gets the default zero-UUID". `event_history`
carries `FORCE ROW LEVEL SECURITY` (`migrations/postgres/001_schema.sql:549`) and the policy
is declared `FOR ALL USING (tenant_id = cleat.assert_tenant_set())` with **no explicit
`WITH CHECK`** (line 515). PostgreSQL reuses the `USING` expression as the `WITH CHECK`
expression when the latter is omitted, so the insert does not quietly land as zero-UUID —
**it is rejected**, the transaction aborts, and child-workflow spawning fails outright for
any connection using a real tenant role.

Two caveats before treating this as a P0. It bites only where RLS is actually in force —
a non-owner tenant role with `cleat.tenant_id` set; an owner connection or single-tenant
deployment inserts the zero-UUID successfully and merely leaves an unattributed row. And
this is reasoned from the schema, not from a run. **Confirm it with an actual RLS-enabled
child spawn before ranking it** — that reproduction is the first task, not the fix. Note
§1.10 records that RLS was bypassed in every shipped configuration until recently, which
would explain how this survived: the enforcement that exposes it is new.

The fix is one column and one parameter. The test is the valuable part, and there is
currently no test that spawns a child workflow under an RLS-enforcing connection.

### 2.21 `applyPostgresSchemaFile` races itself, and its doc comment says it cannot — ✅ **FIXED**

> **Reproduced deliberately, then fixed.** Eight goroutines applying the schema to one
> database fail without serialisation and pass with it. Fixed with a session-level advisory
> lock on a pinned `*sql.Conn` — pinned because advisory locks belong to a *session*, and
> `database/sql` hands out arbitrary pooled connections, so locking via `db` can take the
> lock on one connection and fail to release it on another.
>
> **The race has more than one symptom.** This entry records only
> `duplicate key value violates unique constraint "pg_extension_name_index"`, from
> `CREATE EXTENSION`. The reproduction also produces
> `pq: tuple concurrently updated (XX000)`, from `CREATE OR REPLACE FUNCTION`. Same
> non-atomic-DDL cause, different loser. Anyone matching on the first string alone will
> conclude the second is a new bug.
>
> That nearly cost something: the non-vacuity check was `grep -c "duplicate key value"`,
> which returned 0 and briefly looked like the test had gone vacuous. The test was fine —
> the *check* was too narrow. **Grep for FAIL, not for a remembered error string.**
>
> Doc comment corrected, per this entry's own instruction — it now says the file is safe to
> reapply *sequentially* and explicitly not concurrently.
>
> The original framing is kept below.

#### Original framing

Caught flaking CI on the docs PR that recorded §2.18–§2.20. Same commit, three Multi-DB CI
runs, **success / failure / success** — so it is a flake, and the kind that erodes exactly
the signal Phase 0 spent effort restoring.

```
kvstore_multidb_test.go:36: apply migrations/postgres/001_schema.sql:
  pq: duplicate key value violates unique constraint "pg_extension_name_index" (23505)
```

`engine/testutil/schema.go:73-79` claims the file is safe to reapply:

> All statements in it are idempotent (CREATE ... IF NOT EXISTS, CREATE OR REPLACE,
> DROP POLICY IF EXISTS ... CREATE POLICY), so it is safe to call more than once against
> the same database

That is true **sequentially and false concurrently**, which is the only way CI runs it.
PostgreSQL's `IF NOT EXISTS` forms are not atomic: two sessions both observe the object
missing, both insert the catalog row, and one loses on the unique index.
`CREATE EXTENSION IF NOT EXISTS pgcrypto` (`migrations/postgres/001_schema.sql:24`) is the
one that lost here, but `CREATE TABLE IF NOT EXISTS` has the same hazard.

`go test ./plugins/...` compiles and runs distinct packages in parallel (`-p` defaults to
NumCPU), and every one of them points at the same `CLEAT_TEST_POSTGRES` database, so several
call `applyPostgresSchemaFile` at once against one server. Nothing serialises them.

Options: take a Postgres advisory lock around the apply (`pg_advisory_lock` on a fixed key,
released on close) — smallest change, keeps one shared database; or give each package its
own database; or apply the schema once in the workflow before the test step and stop
applying it per-package. The advisory lock is probably right: it is three lines, needs no CI
change, and matches the existing "one shared DB" assumption.

Whatever the fix, **correct the doc comment**. A comment asserting a safety property the
code does not have is worse than no comment, and it is why the failure reads as mysterious
rather than obvious.

### 2.22 `flushCallIntent` omits `tenant_id` too — ✅ **FIXED** (latent, no production caller)

Found by auditing every PostgreSQL insert into an RLS-protected table after §2.20, on the
theory that a defect shape appearing twice is worth grepping for rather than waiting for.
`engine/flush.go:202` had the identical omission — `event_history` insert, no `tenant_id`,
with `e.tenantID` available and used twice elsewhere in the same file.

It is **latent, not live**: `flushCallIntent` has no production caller. Every reference is
either its own definition or `flush_test.go`. Wired up as-is it would have reproduced
§2.20's failure exactly.

Worth noting how it would have been caught, which is to say not at all: the five
`TestFlushCallIntent_*` tests **pass identically before and after** adding the column. They
assert the call returns `nil` against a mock, never the SQL. That is §2.16's pattern for the
third time — a test suite that cannot distinguish the fix from the defect.

The rest of the audit came back clean. All other inserts into the eight RLS-protected tables
(`store_lifecycle.go`, `store_signals.go`, `store_promises.go`, `store_versioning.go`,
`adaptive_flush.go`, `db.go`, and the remaining `store_event_write.go` sites) pass
`tenant_id` correctly. `workflow_defs` is the deliberate exception — its policy also admits
the zero-UUID default tenant, so the omissions in `store_deployment.go:161` and
`versioned_loader.go:176` are by design, not defects.

### 2.23 `StartChildWorkflowInSchema` — same omission, but a one-line fix would be a false fix — ✅ **FIXED**

`engine/store_children.go:169` omits `tenant_id` from its `<targetSchema>.workflow_instances`
insert, with `s.tenantID` available and unused — superficially §2.20 again. **It is not, and
patching the column alone would be worse than leaving it.**

The function calls `s.db.QueryRowContext` directly: no transaction, no `setRLSOnTx`, so
`cleat.tenant_id` is never set on the session. If the target schema's table carries the same
policy, `cleat.assert_tenant_set()` raises *"cleat.tenant_id is not set"* **regardless of
whether the column is supplied**. Adding `tenant_id` would look like a fix, change nothing,
and retire the entry.

Severity is genuinely unknown and should not be guessed at. Nothing in this repository
creates `workflow_instances` outside `public` — all five migrations open with
`SET search_path = public`, and the RLS policies are attached to the public tables only. So
whether the target schema's table even has a `tenant_id` column or a policy depends on
provisioning that lives outside this repo. Either it has both (the insert fails), or it has
neither (the insert succeeds and cross-schema children are simply unattributed). Establish
which before ranking.

One thing that is settled: there is no session-variable leak. `setRLSOnTx` uses
`set_config(..., true)`, which is transaction-local, and this function opens no transaction —
so a pooled connection cannot carry a previous tenant's value into it.

The real question underneath is a design decision, not a mechanical one: **which tenant owns
a cross-schema child** — the parent's, or the target schema's? Until that is answered there
is no correct value to pass. Reachable from `engine/children.go:230` whenever a workflow
requests a target schema.

**Resolution (2026-08-04).** The design question was answered by the project owner: the child
belongs to the **target schema's** tenant. The motivating case is two microservices, and the
child runs as part of the destination service, so it is the destination's workflow.

Both halves of the analysis above held up when finally exercised. Reverting to the original
code against a real peer schema fails with

```
pq: cleat.tenant_id is not set -- tenant context required for RLS-scoped query (P0001)
```

— i.e. exactly as predicted, the missing column was never the operative problem, and a
column-only patch would have changed nothing.

**The mapping.** Peer schemas are configured by name alone (`--peer-schemas`), with no tenant
attached, so the engine has no direct way to learn the destination's tenant. It does have a
convention: `admin.create_tenant_role` names each tenant's schema
`'tenant_' || replace(tenant_id::text, '-', '_')`. `tenantIDForSchema` inverts that. Where it
succeeds, `StartChildWorkflowInSchema` opens a transaction, sets `cleat.tenant_id` to the
**target** tenant, and writes that value into the column.

**Where it fails — an operator-chosen name like `svc_billing` — nothing is written**, and the
destination table's own `DEFAULT` applies. This is deliberate. Writing the *parent's* tenant
would be the false fix in its second form: it makes the insert succeed while filing one
service's workflow under another service's tenant, which is a silent cross-tenant
misattribution in a system whose entire isolation story is `tenant_id`. If the destination
enforces RLS, the insert is refused instead — the correct outcome for "we cannot say who this
belongs to".

**On coverage — this is the part worth keeping.** The only existing tests
(`TestGap_StartChildWorkflowInSchema{,_Error}`) use a mock DB matching on the string
`"gen_random_uuid"`. They never touch a database, so they could not observe tenant
attribution at all, and passed throughout. Writing a real one exposed why: **nothing in the
repo provisions a peer schema.** Every migration pins `SET search_path = public`, and
`admin.create_tenant_role` creates `tenant_<uuid>` as an empty namespace whose grants all
point back at `public.*`. So the cross-schema feature writes to
`<schema>.workflow_instances`, a table this project never creates. The new test builds one by
hand — tables, `FORCE ROW LEVEL SECURITY`, and the same fail-closed policy — which is the
only way to run this path at all today.

The regression test is verified against **both** wrong implementations: the original (fails
with the `assert_tenant_set` error above) and the plausible false fix (fails with *"child was
attributed to the PARENT tenant"*). A test that only caught the first would have ranked the
false fix as correct.

**Still open:** peer-schema provisioning itself. `GetChildResultInSchema` reads back from the
peer schema with no tenant context either, and would hit the same wall against an
RLS-enforcing destination; it is untested for the same reason. Neither the `k8s/`, `charts/`
nor compose deployments configure `--peer-schemas`, so the feature has no end-to-end exercise
anywhere.

### 2.24 The wasmtime epoch ticker races `Close` — ✅ **FIXED**

Found the honest way: it failed CI on PR #227, on a change that has nothing to do with
wasmtime.

```
panic: object has been closed already
  wasmtime-go.(*Engine).IncrementEpoch
  engine.(*wasmtimeBackend).startEpochTicker.func1
```

`Close` did `close(b.epochStop)` and then called `b.engine.Close()` immediately. Closing the
channel is a *request* to stop, not an acknowledgement that the goroutine has stopped: one
already committed to the `case <-ticker.C` branch goes on to call `IncrementEpoch` on a
freed engine. The window is a single scheduling quantum once every `epochTickInterval`
(50ms), which is why it reads as a rare flake rather than a bug.

Fixed with an `epochDone` channel the goroutine closes as it returns, and a `<-b.epochDone`
join in `Close` before `engine.Close()`.

**Note on the test, because the first one was worthless.** It called `NewWasmtimeBackend`,
`Close`, then checked whether `epochDone` was closed — and **passed with the join removed**.
Once `epochStop` closes, the real goroutine almost always gets scheduled and exits before the
check runs, so it measured scheduler luck, not ordering. The replacement stands a stub
backend in for the ticker so the wait is directly observable: `Close` must block, then
return once the test closes `epochDone`. Verified to fail without the join.

That is the second vacuous assertion caught in this session by the same habit of removing
the fix and re-running. Both would have passed review.


### 2.25 Nothing prevents a red PR from merging into `develop` — ✅ **FIXED** 2026-08-04

`develop` has branch protection enabled, and it enforces almost nothing:

```console
$ gh api repos/cleat-team/cleat/branches/develop/protection --jq 'has("required_status_checks")'
false
$ gh api repos/cleat-team/cleat/branches/develop/protection --jq 'has("required_pull_request_reviews")'
false
$ gh api repos/cleat-team/cleat/rulesets
[]
```

No required status checks, no required reviews, no rulesets. `gh pr merge` will merge a PR
whose entire test suite is failing, and **`mergeStateStatus == CLEAN` carries no CI
information at all** — it reports the absence of conflicts, and reads CLEAN while every job
is still red or has not started.

This is the structural version of the Phase 0 finding. §0 is about CI reporting green while
the `engine` package does not compile — a signal that lies. This is about the signal not
being *connected to anything* even when it tells the truth. Both have the same effect:
merges are gated by whoever is watching, not by the pipeline.

**How it surfaced.** A merge-on-green watcher merged PR #227 on the condition
`pending == 0 && fail == 0 && total > 30`. It fired at 31 checks when the full set for that
PR was 42, because GitHub registers checks progressively and `pending == 0` routinely means
"not created yet" rather than "finished". The next PR made the pattern unmistakable —
`pending` was 0 at *every* poll while the total climbed 20 → 30 → 31 → 32 → 34 → 36. The
merge happened to be fine (all six workflow runs on the merge commit succeeded), but that
was luck.

**Resolution.** `develop` now requires 16 status checks, with `enforce_admins: true` — a hard
gate that binds the repository owner too. Verified live after applying:

```console
$ gh api repos/cleat-team/cleat/branches/develop/protection \
    --jq '{contexts: (.required_status_checks.contexts|length), enforce_admins: .enforce_admins.enabled, strict: .required_status_checks.strict}'
{"contexts":16,"enforce_admins":true,"strict":false}
```

Required: `Lint`, `lint-go`, the eight `Test Go (<pkg>) on 1.26` matrix jobs,
`Cluster Integration Tests`, `Build`, `Fuzz Tests`, `Vulnerability Check`,
`Developer Certificate of Origin`, `Validate branch name`.

Deliberately **not** required: everything that reaches an external registry — Java/TeaVM,
the Plugin Harness layers, MySQL/SQL Server, and the Rust/Python/AssemblyScript SDK
integrations. The Maven Central 403 that reddened `Layer 2 — WASM Integration` earlier the
same day would otherwise have blocked every merge until a third party recovered, with no
second maintainer to unblock it. `Benchmarks` and `Coverage` are excluded for a harder
reason: they are skipped or push-to-main only, and a required check that never runs blocks
its PR forever. `strict: false` so a busy day does not force a rebase per PR.

**One trap, caught before it bit.** Required contexts are **check-run (job) names, not
workflow names.** The workflow is called "Branch Naming Check"; the context is
`Validate branch name`. Requiring the former — under `enforce_admins: true` — would have made
every PR in the repository permanently unmergeable, including by the owner. It was caught by
diffing the proposed list against the check-runs actually present on three real PRs of
different shapes (docs-only, code, CI-config), which is the check to repeat before adding any
context: path filters can skip jobs, so a docs-only PR is the one that finds the gap.

Automation that merges should still gate on **workflow runs for the head SHA** rather than
the check-name rollup — GitHub is now the real gate, but a watcher that reports honestly is
still worth having:

```sh
SHA=$(gh pr view "$PR" --json headRefOid -q .headRefOid)
runs=$(gh run list -c "$SHA" --limit 100 --json status,conclusion,name)
busy=$(echo "$runs" | jq '[.[] | select(.status != "completed")] | length')
```

and require the run count to hold steady across several consecutive polls. Do not hardcode
an expected total: path filters mean different PRs trigger different workflows (#227 saw 36
checks, #229 saw 42), so any fixed threshold either fires early or never fires.


### 2.26 The SQL Server error classifier matched error numbers as substrings — ✅ **FIXED** (wiring still OPEN)

**This corrects §2.8's recommendation.** §2.8 found `mssqlRetry` and the whole
`engine/mssql_errors.go` classification family had no production caller, and concluded they
should be wired in before the MySQL/SQL Server support claim is made publicly. Wiring them
**as they stood would have been worse than leaving them dead.** The classifier was wrong in
both directions at once.

**Wrong direction 1 — permanent errors classified as retryable.** Every predicate matched
the decimal error number as a bare substring of the error text, and SQL Server error text
carries workflow IDs, row numbers, column names and interpolated business data:

```go
strings.Contains(msg, "258")    // isMSSQLTimeout
strings.Contains(msg, "3960")   // isMSSQLSnapshotError
strings.Contains(msg, "2627")   // isMSSQLDuplicateKey
```

Verified against the old implementation:

| Error | Classified as | Retryable? |
| --- | --- | --- |
| `permission denied for workflow "wf-2589abc"` | timeout (258) | **yes** |
| `Invalid column name 'col3960'.` | snapshot conflict (3960) | **yes** |
| `invalid column value at row 26270` | duplicate key (2627) | — |
| `workflow input rejected: amount 2601 exceeds limit` | duplicate key (2601) | — |

`isMSSQLConnectionError` was broader still: `strings.Contains(msg, "connection")` made
`invalid connection string: missing database` — a configuration error that fails identically
on every attempt — retryable. Had `mssqlRetry` been wired up, each of these would be retried
until the budget was exhausted, converting a clear failure into a slow one and consuming the
retry budget a real deadlock needed.

**Wrong direction 2 — real errors missed.** This only became visible on testing against the
type the driver actually returns. `mssql.Error{Number: 258}` renders as
`mssql: Wait operation timed out.` — the digits `258` appear **nowhere** in the text. So the
substring matcher missed genuine server-reported timeouts and in-memory OLTP write conflicts
(41302 and friends) entirely, while catching fabricated ones. It also missed
`driver.ErrBadConn`, `io.ErrUnexpectedEOF` and `*net.OpError`, none of which mention
"connection" in their text.

**Why the tests did not catch it.** `engine/mssql_errors_test.go` feeds only `fmt.Errorf`
strings — never an `mssql.Error`. The tests and the implementation shared the same wrong
model, that a SQL Server error *is* text, so they agreed with each other perfectly. This is
the §2.16 shape in a different package: a test that confirms the implementation rather than
the requirement.

**The fix.** Classify on `mssql.Error.Number` via `errors.As`, on `driver.ErrBadConn` /
`net.OpError` / `net.Error.Timeout()` for transport faults, and on `errors.Is` for context
errors. Text matching survives only as a fallback for errors that lost their type through a
wrapper, and every remaining phrase is distinctive — no bare numbers, no bare `"connection"`
or `"duplicate"`. `context.Canceled` is now explicitly **not** retryable: cancellation is a
decision, not a fault.

**The wiring — first increment done, 2026-08-04.** The distinction below is what made it
possible to wire anything at all without a per-transaction idempotency audit:

- **Deadlock (1205) and snapshot conflict (3960, 41301–41325)** guarantee the server rolled
  the transaction back. Replaying is sound even when the work is not idempotent.
- **Timeout (258) and dropped connections** leave the outcome *unknown* — the commit may have
  succeeded with only the acknowledgement lost. Blindly replaying a non-idempotent statement
  can double-apply it, which for a workflow engine means a duplicated side effect.

So `mssqlRetry` cannot simply be wrapped around all 113 `ExecContext`/`QueryContext` sites,
and it should not be wrapped around the transaction boundaries either without deciding, per
transaction, which of those two categories it tolerates.

**The way through is to only retry the rollback-guaranteed set.** If the server has
definitively undone the transaction, replaying it is sound *whether or not the work is
idempotent* — so that retry needs no per-transaction analysis and is safe at any boundary.
Unknown-outcome errors stay hard failures, exactly as before. `withRollbackGuaranteedRetry`
in `engine/mssql_retry.go` is that narrower wrapper; `mssqlRetry` itself remains unwired and
correctly still baselined, because it gates on `isMSSQLRetryable`, which includes the
unknown-outcome class.

Wired into `ClaimWorkflows` and `ClaimStickyWorkflows` — the highest-contention transactions
in the engine and where a deadlock is most likely. Budget is 2 retries at 20ms/40ms: at most
60ms of added latency on a path that previously failed outright. A deadlock victim claimed
nothing, so the replay is a clean retry.

**The count in this section was wrong.** There are ~20 transaction boundaries in the MSSQL
store, not 8. The remaining ones are a follow-up, and each needs the same one-line judgement:
a rollback-guaranteed retry is always safe, so the only question per boundary is whether it
is worth the wrapper.

**Validated against a real server, which is the check this section says was never made.**
`TestMSSQLDeadlock_ClassifiedFromTheRealDriverError` provokes a genuine deadlock — two
transactions taking row locks in opposite order — and asserts on the error the driver
actually returns rather than a fabricated one:

```
driver error: Number=1205 Message="Transaction (Process ID 75) was deadlocked on lock
resources with another process and has been chosen as the deadlock victim. Rerun the transaction."
```

`isMSSQLDeadlock`, `isMSSQLRetryable` and `isMSSQLRollbackGuaranteed` all classify that
correctly. The original defect was a classifier and a test that shared the same wrong model,
so the classification is now pinned to reality at one end and the retry policy unit-tested at
the other.

**Evidence the wiring is real, not merely present:** twelve entries left
`scripts/deadcode-baseline.txt` — `isMSSQLRollbackGuaranteed`, `isMSSQLDeadlock`,
`isMSSQLSnapshotError`, `hasNumber`, `mssqlErrNumber`, `mssqlSnapshotConflictNumbers` and the
five error-number constants are now reachable from production code. `mssqlRetry`,
`isMSSQLRetryable`, `isMSSQLTimeout`, `isMSSQLConnectionError` and `isMSSQLDuplicateKey`
remain baselined, which is the correct outcome for the path deliberately left unwired.

The §2.8 support position is now narrower: **on SQL Server a deadlock on the claim path is
retried; everywhere else it is still a hard error.**

---

### 2.27 Two of the three deployment manifests crash-loop on an undefined flag — ✅ **FIXED**

Phase 2's row 2.7 said "all three are currently broken". Two are. The third is not, and the
difference matters, so it is recorded rather than rounded off.

**Confirmed, by running the binary with each manifest's own arguments:**

| Manifest | Flags it passes | `cleat-worker` exit |
|---|---|---|
| `k8s/deployment.yaml` | `--namespace=default` | **2** — `flag provided but not defined: -namespace` |
| `charts/cleat/templates/deployment.yaml` | `--tenant-id=…`, `--namespace=…` | **2** — `flag provided but not defined: -tenant-id` |
| `docker-compose.cluster.yml` | all defined | 1 — parses, starts, then fails on the bogus DSN I gave it |

Go's `flag` package treats an unknown flag as fatal: usage to stderr, `os.Exit(2)`. In
Kubernetes that is a CrashLoopBackOff on every pod of both deployments, permanently. Not an
edge case, not a misconfiguration — the manifests as committed cannot start the binary they
name.

**Provenance.** `--namespace` was real once; `dfa8702` deleted the namespace concept from
the store interface and removed the flag, and neither manifest followed. `--tenant-id` is
different: **no commit has ever registered it in `cmd/cleat-worker`.** The chart shipped a
`worker.tenantId` value, documented as "Tenant ID for RLS and isolation", that has never
reached a running process. Tenancy is resolved per request via `--tenant-resolver`
(default `single-tenant`); the chart does not expose it, which is a real gap but a separate
one — filed, not silently invented here.

**The test** is `tests/manifests/manifests_test.go`, wired into the `test-go` matrix as
`manifests` with a skip budget of 0. It builds `cmd/cleat-worker`, reads the flag set from
`--help`, and checks every `args:`/`command:` entry in all three manifests against it.

Reading `--help` rather than scanning the source for `flag.String(...)` was not stylistic. I
wrote the source scan first; compared against the real binary it **missed five flags that
exist** (`max-body-size`, `memory-hard-limit`, `memory-soft-limit`, `rate-limit`,
`rate-limit-per-tenant`). A check built on it would have failed manifests that work. The
binary's usage output is what the container actually gets.

**Verified non-vacuous** against three injected defects:

| Injected | Result |
|---|---|
| The real bug — manifests as committed | fails, naming `--namespace` and `--tenant-id` |
| `args:` renamed so the extractor finds nothing | fails: "extracted only 0 flags … the manifest's arg block is no longer being read" |
| `--db` line deleted, so the wrong block is read | fails: "…does not include `--db` — the arg block being read is not the worker's" |

The last two matter because this test's whole failure mode is silence: a regex that stops
matching turns it green. That is §2.16 in a different costume.

**What this does not cover.** Row 2.7 asks that the manifests be *started* and the worker
reach ready. That still needs a cluster — `helm`, `kubectl` and `kind` are all absent
locally, and per the standing constraints `docker-compose.cluster.yml` cannot be exercised
here at all. This covers the failure that was shipped; the boot test remains open.

---

### 2.28 The execution-time fence does not exist in any deployment — ✅ **FIXED for Go WASM**, residual gap below

Found by doing what 2.7 asks and actually booting `docker-compose.cluster.yml` (all five
containers healthy in under 20s, zero restarts — the compose file is fine). The finding was
in the first worker's startup log:

```
"msg":"wasmtime backend unavailable, using legacy wazero for Go WASM",
"error":"wasmtime backend requires CGO"
```

**The chain.** `engine/backend_wasmtime.go` is `//go:build cgo`. `Dockerfile:21` builds with
`CGO_ENABLED=0`. All three manifests run `cleat-worker:latest` from that Dockerfile. So the
backend CLAUDE.md calls *"the primary backend … the standard engine … the behaviour of
record"* is compiled out of every container in every deployment path.

**And CI tests the other one.** The `test-go` matrix does not set `CGO_ENABLED`, so CGO is on
and `TestWasmtimeBackend_InfiniteLoop_GoStartPath`, `TestIntegrationWorkflowMaxDuration` and
the rest of the wasmtime suite all run. This is §1.9's shape — *the shipped X was not the
tested X* — moved from the schema to the execution engine. The protection is tested in the
configuration nobody ships and absent from the one everybody ships.

**Measured, not inferred.** `testdata/spin` (a pure arithmetic loop that never enters the
host) under `WithDefaultWorkflowTimeout(2 * time.Second)`, wazero backend, `CGO_ENABLED=0`:

| iterations | elapsed | error |
|---|---|---|
| 1,000 | 499ms | nil |
| 100,000,000 | 628ms | nil |
| 100,000,000,000 | **2m35s** | **nil** |

A workflow with a two-second budget ran for **two and a half minutes** and was reported as a
**success**. The fence did not fire late; it did not fire. `executor.go:271` puts the
deadline on `execCtx` and passes it to `CallExport`, but wazero only observes context
cancellation when the guest calls back into the host, and this guest never does. In a worker
there is no `go test -timeout` to end it: the goroutine holds its concurrency slot until the
process dies, so ten runaway workflows wedge a `--concurrency=10` worker completely.

This is item **1.5**, which was raised to the top of Phase 1 and then fixed for wasmtime only.
It should be read as still open for every deployed configuration.

**The obvious fix does not work.** wazero has `WithCloseOnContextDone(true)`, absent at
`engine/runtime.go:93`, which makes it interrupt guest code on cancellation. Setting it makes
*every* execution fail — 1,000 iterations included — with `wasm trap: exit(code=0)` in ~500ms.
It is not a fence firing; it breaks execution outright, presumably against the suspend/resume
protocol in `Runtime.CallExportWithSuspend`. Tried, measured, reverted. **Not committed.**

**So the fix is real design work, not a one-liner,** and there are two routes that should be
costed against each other rather than picked by reflex:

1. Make wazero interruptible — understand the `CloseOnContextDone` interaction with suspend,
   or bound the guest some other way. Keeps the deployment story unchanged.
2. Ship the backend that already works — build the image with CGO so containers get wasmtime
   and the epoch fence that is already tested. Changes the base image and binary linkage.

Either way the invariant worth adding afterwards is that **a backend without a working
execution fence must not be selectable in production**, so this cannot recur silently.

A regression test is written and parked at
`scratchpad/backend_wazero_fence_test.go.pending` — the wazero half of
`TestIntegrationWorkflowMaxDuration`, with a bounded wait so a regression names the defect
instead of hanging the package. It is deliberately **not** committed: it fails today, and
landing a known-red test would put CI back in the state Phase 0 just dug it out of.

#### Resolution — route 2, ship the backend that already works

Chosen over making wazero interruptible, because the wasmtime fence exists and is tested and
the alternative meant inventing a second mechanism.

**Alpine was the actual blocker, not CGO.** Building the existing image with `CGO_ENABLED=1`
fails at link time: `undefined reference to fstat64`, `ftruncate64`. Those are glibc LFS
symbols musl does not export, and `wasmtime-go` ships a prebuilt glibc `libwasmtime.a`. So
the Dockerfile's `CGO_ENABLED=0` was not a preference — Alpine made it the only option that
compiled, and the comment above it ("a fully static binary, no libc dependency") described
the consequence as though it were the goal. Builder and runtime are now `bookworm` /
`bookworm-slim`.

**Verified on the shipped artifact, not the build log:**

```
$ docker run --rm cleat-worker:latest --verify-backend
verify-backend: OK: wasmtime backend available          # exit 0

$ docker run -d --network … cleat-worker:latest --db=… ; docker logs …
"msg":"wasmtime backend registered for Go WASM","instance_timeout":30000000000
```

**The guard.** `--verify-backend` constructs a real wasmtime engine and exits 0/1, and the
Dockerfile runs it as a build step, so a future `CGO_ENABLED=0` or musl base fails the build
instead of shipping silently. It is asserted in both directions — `verify_backend_cgo_test.go`
requires 0, `verify_backend_nocgo_test.go` requires non-zero *and* that the message names
`CGO_ENABLED=0`. A guard that only ever reports OK is not a guard, which is §2.16's lesson.

Also removed: the cluster job's `CGO_ENABLED=0 go build -o cleat-worker` step. Nothing ran the
binary — the cluster runs containers — and sitting directly above the image build it read as
though `CGO_ENABLED=0` were the shipped configuration. That belief is what let this survive.

**The cost, stated plainly.** The image goes from **69.7 MB to 231 MB** (the binary alone is
55 MB, mostly `libwasmtime.a`). That is a 3.3× increase and it is the price of the fence. If
it matters, the runtime layer is the cheaper half to attack — a distroless base would save
~50 MB, but the compose healthcheck shells out to `wget`, so that is a change with its own
tail.

#### Residual: non-Go guests are still unfenced

The log line reads *"wasmtime backend registered for **Go WASM**"*. Modules in the languages
wazero is retained for still execute on wazero, where the fence still does not fire. This is
narrower than before — Go is the common case and is now bounded — but it is not zero, and the
parked test above stays parked for exactly this reason. Closing it needs route 1 after all,
for those guests only.

---

### 2.29 Resource exhaustion, end to end against the shipped image — ✅ **DONE**

Phase 2 row 2.5, and the test that closes the loop on §2.28. #237 was merged on the strength
of a log line and a `--verify-backend` exit code; neither shows that a runaway workflow is
actually *killed* in a container.

**Observed against the running cluster:**

```
execution time limit exceeded (29.999847291s wall-clock budget; configure with --wasm-instance-timeout)
    0: 0x1b7d4f - <unknown>!main.Spin
Caused by:
    wasm trap: interrupt          <- epoch interruption
```

The worker held `restarts=0` and `/healthz 200` throughout, and completed an ordinary
workflow immediately afterwards. That second part is the half a fence test usually forgets:
terminating a runaway workflow by wedging or crashing the worker would satisfy every other
assertion.

**Verified non-vacuous against the real defect,** by rebuilding the pre-#237 image
(`git show ff7e759^:Dockerfile`), pointing worker-1 at it, and re-running. The worker logged
`wasmtime backend unavailable, using legacy wazero` and the test failed with:

> workflow spin-runaway-… was still "running" after 1m30s — a runaway workflow was not
> terminated, so it is holding a worker's concurrency slot indefinitely

Not a stubbed backend or a deleted line: the actual image that shipped until today.

**Where it lives, and why not `tests/cluster`.** That suite is run by *nothing at all*
(`UNWIRED_SUITES` in `scripts/check-ci-package-coverage.sh`), so a test added there would
never execute. `tests/exhaustion` is its own package, wired into ci.yml's cluster job — which
already builds the image and brings the cluster up, and until now put no work through it. Its
existing steps prove the cluster *comes up*; this is the first that proves it *runs
anything*.

Two things this cost, worth knowing before extending it:

- It needs `__entry_point` in the instance input. The definition's `entry_points` array does
  not resolve it, and without it the workflow fails instantly with "cannot determine entry
  point" — which resembles a fence firing closely enough to fool a looser assertion. The test
  asserts on the limit message and on elapsed time for that reason.
- The existing `tests/cluster` fixtures insert mock WASM (`"mock-wasm-v1"`), so nothing in the
  repo had run real WASM through a worker container before this.

### 2.30 The event checksum chain is rebuilt from scratch on every write — ✅ **FIXED**

Found by pointing a real database at `tests/integrity`, which is in `UNWIRED_SUITES` and had
therefore never run. `VerifyWorkflowEvents` recomputes the checksum chain over the whole
history in step order. `appendEventsInTx` built it two different ways from that:

- **It restarted the chain at every call.** `prevChecksum` was a fresh `var prevChecksum
  string` per invocation, so the first event of a second write chained from `""` rather than
  from the previous event's stored checksum. The single-event path passed `""`
  unconditionally, so `AppendEventHistory` never chained at all.
- **It chained in slice order, not step order.** Verification reads `ORDER BY step`, so a
  batch handed over in any other order persisted a chain that could not be reproduced.

The first one reaches production. `cmd/cleat-worker/setup.go:1669` computes
`newEvents = resultHistory[len(history):]` and hands *only that segment* to
`FinalizeWorkflowSegment`, which calls the same helper — so **every workflow that suspends and
resumes** (sleep, await-signal, timer) had a broken chain from its second segment onward, and
`VerifyWorkflowEvents` reported it as corrupt. A verifier that cries corruption on healthy
data is worse than no verifier: it is the noise a real corruption would hide in.

Fixed in all three dialects: `chainOrder` walks the batch in step order, and a new
`previousStoredChecksum` seeds from the row immediately preceding the batch — the row, not the
last row *with* a checksum, because that is precisely what the verifier chains from.

**Why it survived.** The eight existing `VerifyWorkflowEvents` tests in
`engine/db_regression_test.go` drive sqlmock: they supply both the stored checksum and the row
it is recomputed from, so the two can never disagree. Nothing wrote a chain and read it back.
The replacement tests in `engine/store_event_chain_test.go` use a real database, and all three
fail on the pre-fix tree:

```
verify events: workflow chain-...: step 1: checksum mismatch
    (expected 3ae6e6ebac5922fd, got 261d761d2ec3f10b)
step 1 was written by a second call, so its checksum must chain from step 0's
stored checksum, not from an empty string
```

`TestAppendEventHistory_ChainDetectsTampering` is the counterweight: the fix must not buy
agreement by weakening what verification catches, so it rewrites a persisted event's `payload`
and requires the mismatch to still be reported.

### 2.31 `tests/integrity` had never run — ✅ **DONE**

Thirty tests covering replay determinism, checksum-chain verification, WAL corruption
detection, compaction and the durable-call ambiguity detector — including
`TestPendingSentinelDetection`, the only evidence that the detector kept in 1.4 Phase A
actually works. All of it in `UNWIRED_SUITES`, run by no job.

Run locally with no database it reports `ok 5.074s`. **All thirty tests skip.** That is the
whole result: a green line with nothing behind it.

Pointed at a real PostgreSQL, 22 of the 30 failed at once, every one of them on
`workflow_instances_def_name_def_version_fkey`. The suite built its own schema with
`CREATE TABLE IF NOT EXISTS`, and the `workflow_instances` it invented had no foreign key to
`workflow_defs` — so the fixture and production had diverged, and nothing ran to notice. The
same shape `engine/fault_test.go` documents in its own history.

The helper now takes its connection from `engine/testutil`, which builds the schema from
`migrations/postgres/` and *fails* rather than skips when `CLEAT_TEST_DB` is set but
unreachable. Sixty lines of hand-rolled DDL deleted.

What the remaining eight failures were, once the schema was real:

| Failure | Cause |
|---|---|
| 3 × checksum mismatch | A real engine defect — §2.30 |
| `TestConcurrentStatusUpdates` | Passed a hardcoded `generation = 0` to `CompleteWorkflow` after `ClaimWorkflow` had bumped it, so it counted the fence *working* as an error |
| `TestWalCorruption_PayloadTampering` | Tampered with the `operation` column, which the checksum does not cover — §2.32 |

**Wired into the `test-go` matrix**, which already provides both things it needs: a PostgreSQL
service, and CGO (via `-race`) for the wasmtime backend. Budget `test-go/integrity 0`, and
that number is load-bearing: with CGO off, seven tests skip on "wasmtime backend requires
CGO", so a nonzero count means the job stopped testing the primary backend.

Final: **68 pass, 0 skip, 0 fail** (67s, CGO on, live PostgreSQL 16).

Five suites remain in `UNWIRED_SUITES`: `cluster`, `cross-language`, `scale`, `soak`,
`upgrade`. On this evidence, assume each contains failures rather than coverage.

### 2.32 The checksum covers `payload`; every SQL consumer reads the shadow columns — ✅ **FIXED**

`event_history` stores each event twice: in the individual columns (`service`, `operation`,
`request`, `response`, …) and again in a `payload` JSONB. `LoadEventHistory` scans the columns
first and then overwrites them from `payload` whenever it is non-NULL, so **`payload` is
authoritative** and it is the only copy `computeEventChecksum` covers.

Consequence: `UPDATE event_history SET operation = 'something-else'` is undetectable.
`VerifyWorkflowEvents` reports the workflow clean, replay is unaffected — and every SQL
consumer that reads the columns (the admin dashboard, `cleatctl`, ad-hoc queries, metrics)
shows the altered value. An integrity checker that certifies a row whose displayed contents
are a lie is doing half the job.

`TestWalCorruption_PayloadTampering` asserted the opposite and could never have passed; it now
tampers with `payload`, and carries a second assertion pinning the gap so that closing this
item forces the test to be updated rather than leaving a stale claim.

**Not a drop-in fix.** Extending the checksum to the shadow columns invalidates every checksum
already stored, so it needs a migration or a versioned checksum. The alternatives are to have
verification compare columns against `payload` (cheap, no migration, detects divergence
without changing the chain), or to stop writing the duplicates at all and treat `payload` as
the sole record. The third is the real fix and the largest.

**Taken: the second.** `engine/store_event_shadow.go` adds `verifyShadowColumns`, called from
`VerifyWorkflowEvents` after the chain check. It rebuilds each row from its columns, applies
`populateFromPayload` to a copy, and compares. Because `populateFromPayload` only assigns keys
the payload actually carries, the two records can differ *only* where payload disagrees with a
column — so the comparison needs no per-event-type knowledge and no migration, and every
checksum already stored stays valid.

`TestWalCorruption_PayloadTampering`'s pinning assertion did exactly what it was put there to
do: closing this item failed it, and it has been inverted rather than deleted.

**Two deliberate limits, both asserted rather than assumed.**

- **Only unencrypted, unredacted fields.** `Request`, `Response`, `Err`, `SignalPayload`,
  `ChildInput`, `NewInput`, `PluginInput`, `PluginOutput`, `PromiseResult` and `PromiseError`
  pass through `decryptAndRedactEventRecord` and `RedactOnRead` on the column path, while the
  payload path is decrypted but *never redacted*. Comparing those two would report a
  divergence for every redacted field in the database. The 14 fields covered are metadata —
  `service`, `operation`, `signal_names`, `child_name`, `plugin_name`, … — which is exactly
  the set a dashboard displays.
- **Only keys the payload carries.** `eventRecordToPayload` omits several when empty
  (`duration_ms` on a call), and `populateFromPayload` cannot overwrite a key that is not
  there. Tampering with a column whose payload counterpart was omitted is still invisible. The
  headline case from this section, `operation` on a call event, is always present.
  `TestVerifyWorkflowEvents_ShadowCheckIsNotVacuous` asserts the payload actually carries
  `operation` and `service`, so if the writer ever stops populating them the detection test
  cannot start passing vacuously.

Verified by removing the call and re-running: `VerifyWorkflowEvents accepted an event whose
operation column no longer matches its payload`. `TestVerifyWorkflowEvents_ShadowCheckSurvives\
CleanHistory` drives one event of every shape that carries mirrored metadata as the
false-positive guard — a verifier that fails on untampered data would be worse than the gap it
closes. Full `./engine/` and `./tests/integrity/` suites pass against PostgreSQL 16.

**Still open: the underlying duplication.** `payload` remains authoritative and the columns
remain a second copy that nothing keeps in sync at write time. This detects divergence; it does
not prevent it. Treating `payload` as the sole record is still the real fix, and it is still
the largest — `populateFromPayload` has ten call sites across the three dialects.

### 2.33 `tests/upgrade` had never run either — ✅ **DONE**

Eight tests, and **all eight failed** the first time a database was pointed at them. Four
distinct causes, none of them subtle:

1. **Five `INSERT INTO workflow_defs` statements had a syntax error.** A missing `)` after the
   column list, and a fifth value for a four-column insert:
   ```sql
   INSERT INTO workflow_defs (name, version, wasm_bytes, entry_points
       VALUES ($1, 1, $2, '{old_entry}', 'default')
   ```
   `pq: syntax error at or near "VALUES"`. Nothing compiles SQL in a string literal, and
   nothing ever executed it.
2. **Three "no data loss" assertions compared JSONB formatting.** `input::text` returns
   PostgreSQL's normalised rendering — `{"key": "value"}`, with a space — which never equalled
   the Go literal `{"key":"value"}`. They now let PostgreSQL do the comparison
   (`input = $2::jsonb`) and read the text only for the failure message.
3. **The rolling-restart tests passed a hardcoded `generation = 0`** after `ClaimWorkflow` had
   bumped it, so every completion lost the fence. Same defect as `tests/integrity`'s
   `TestConcurrentStatusUpdates`: the test counted the fence *working* as a worker error.
4. **An order dependency between packages.** The rolling tests insert instances with
   `def_name='test'` but nothing in the package creates that definition. They passed on a
   machine where another suite had already made the row and failed on a fresh database —
   verified by deleting the row and re-running. `testDB` now seeds it.

**Two of the tests were vacuous even with the fence bug in place.** `TestRollingWorkerRestart`
logged `0/50 workflows completed` and passed: it asserted only that the workers did not error.
`TestRollingRestartNoDuplicateExecution` reported `0 workflows processed, 0 total executions,
0 duplicates` — and "no duplicates" is also what you get when nothing runs. Both logs are now
assertions, and the first drains any remaining work and requires every workflow to reach
`done`, which is the property a rolling restart is supposed to have.

`testDB` moved to `engine/testutil` for the same reasons as §2.31.

Result: **8 pass, 0 skip, 0 fail**, wired into the `test-go` matrix with budget
`test-go/upgrade 0`. Before: `50/50` and `30 executions` where it had been `0/50` and `0`.

### 2.34 `tests/scale` — ✅ **DONE**

Cheapest of the three. 15 of 16 passed once the schema was real; the one failure was the same
generation fence, with an extra defect underneath it:

```go
for wfID := range workCh {
    wf, err := store.ClaimWorkflow(ctx, workerID)   // claims *some* workflow
    ...
    store.AppendEventHistoryBatch(ctx, wfID, events)          // writes to a different one
    store.CompleteWorkflow(ctx, wfID, workerID, 0, ...)       // and completes it, ungenerationed
}
```

The channel is a work counter, not an assignment — `ClaimWorkflow` decides what this worker
gets. The loop wrote to `wfID` while holding `wf`, so every completion was against a workflow
the worker did not own, and the hardcoded `0` lost the fence on top of that. It now uses
`wf.ID` and `wf.Generation`.

`testDB` moved to `engine/testutil` and seeds `('test', 1)`, as in §2.31 and §2.33.

**16 pass, 0 skip, 0 fail**, ~23s. Budget `test-go/scale 0`. The throughput numbers stay
logged rather than asserted, so it does not become a benchmark gate that fails on a slow
runner; what it asserts is correctness under concurrency.

### Where the unwired suites stand

| Suite | State |
|---|---|
| `integrity` | ✅ wired — §2.31 |
| `upgrade` | ✅ wired — §2.33 |
| `scale` | ✅ wired — §2.34 |
| `cluster` | ✅ wired — §2.36 |
| `cross-language` | ✅ wired — §2.37 |
| `soak` | 🔶 open, and unwired twice over: gated behind the `soak_test` build tag, so `go vet ./tests/soak/...` reports no packages at all |

Across all five suites: **110 tests that no job had ever run**, of which **42 failed** the
first time real infrastructure appeared, and two of those were live defects in production code
(§2.30, and the `docker compose` call in §2.36). None of it was visible from a green CI.

**Follow-up, and it matters:** the new matrix entries create new check-run contexts
(`Test Go (integrity) on 1.26`, `… (upgrade) …`, `… (scale) …`) that are **not** in the
required-status-check list configured in §2.25. Until they are added, a failure in any of them
does not block a merge — which is the same defect as everything above, one level up. Add them
once each has produced a real check-run to name.

### 2.36 `tests/cluster` — ✅ **DONE**

Eleven tests: worker registration, workflow spread across queues, failover when a worker
dies, PostgreSQL kill-and-restart, full cluster restart, replay determinism, WASM version
isolation, and scale-up. **All eleven failed** the first time they were pointed at the
running cluster.

| Cause | Count |
|---|---|
| Missing `workflow_defs` row → FK violation | 10 |
| A `workflow_defs` INSERT with the same missing `)` as §2.33 | 1 |

Behind those, once the schema was satisfied, four more:

1. **`docker-compose`, not `docker compose`.** `helpers.go` shelled out to the hyphenated v1
   binary in three places. It is gone from current GitHub runners and from Docker Desktop, and
   ci.yml's cluster job has used the v2 plugin form throughout — so the first thing this suite
   would have done on a runner is fail to find the command.
2. **The tests raced the live workers.** They inserted on `queue-1/2/3`, which is exactly what
   the three compose workers serve, then claimed and expected to win.
   `TestFullClusterRestart` released a workflow and asserted it could claim it back;
   `cleat-worker-1` got there first. They now use `queue-cluster-tests-{1,2,3}`, which no
   worker serves — these are store-level tests running against the cluster's database, and the
   live workers' behaviour is `tests/exhaustion`'s job.
3. **`generation = 0` again**, in `CompleteWorkflow` and four `ReleaseWorkflow` calls. Fifth
   suite in a row.
4. **`ListWorkflows(Status: "running", Limit: 1000)`** in the scale test returned every running
   workflow in the cluster, so under a full run the test's own rows fell outside the limit and
   were never released — and the release error was discarded.

**Three tests could not fail.** This is the part worth keeping:

| Test | What it printed, and passed |
|---|---|
| `TestKillWorkerMidExecution` | `Note: no worker-1 workflows were reclaimed by remaining workers (may be timing)` |
| `TestKillPostgresAndRestart` | `No workflows to claim after restart (may have been consumed by another worker)` |
| `TestScaleUpWorkers` | `Note: 3 workers claimed 0 vs 1 worker claimed 50 (may be fewer due to timing)` |

Each is the exact output a completely broken failover, recovery or claim path produces. All
three are assertions now. The scale test asserts that **no work went missing** — every released
workflow is claimed again — rather than that three workers are faster than one, which is not
something to assert on a shared runner.

Result: **11 pass, 0 skip, 0 fail**, 4.2s. Wired into ci.yml's cluster job, after the
exhaustion step, since `TestKillPostgresAndRestart` restarts the database.

**One more thing the first CI run caught.** The cluster job sets `CLEAT_TEST_DB` to a separate
`cleat_tests` database, deliberately, so that `./engine/...` does not share a table with four
live workers. This suite wants the opposite — it restarts the postgres container and asserts on
failover, so it has to be looking at the database the cluster actually runs on. Against
`cleat_tests` every test failed with `relation "workflow_defs" does not exist`: nothing builds a
schema there until `engine/testutil` does, and this package does not use it. The step overrides
the variable, with the reason recorded next to it.

### 2.37 `tests/cross-language` — ✅ **DONE**, and it passed as written

Seven tests, and the only one of the five suites with **nothing wrong with it**. It covers the
thing that is hardest to get right and easiest to break silently: a workflow executed under one
language runtime and then *replayed* under another from the recorded history, in both
directions, plus divergence detection across the boundary.

`7 pass, 0 skip, 0 fail` on the first run. It had simply never been run — no workflow file
named it.

The `Cross-Language E2E` workflow already installs Rust, Python, AssemblyScript and Java, then
runs `-run "TestRust|TestPython|TestAssemblyScript|TestJava" ./engine/...`, which never touches
`tests/cross-language/`. Wiring it there is one step, because that job is the only one with the
toolchain the suite needs.

Skip budget `e2e-cross-language 0`, and the number is the point: without cargo all seven skip,
so a nonzero count means the toolchain setup stopped working and the suite quietly went back to
testing nothing — the state it was already in.

**It runs but does not gate.** `Cross-Language E2E` is deliberately outside the required-check
list (§2.25) because it pulls from external registries — the same reasoning as the Maven
exclusion. Recorded rather than quietly accepted: a crates.io outage should not block every
merge, and the cost is that a regression here surfaces on `develop` rather than on the PR.

### What the five suites cost, and what they were worth

**110 tests that no job had ever run.** 42 failed the first time real infrastructure appeared.
Two were live defects in production code — the event checksum chain (§2.30) and the
`docker-compose` v1 call (§2.36). The rest were fixtures that had drifted from the schema,
`generation = 0` in five separate suites, four SQL statements that had never been executed, and
five assertions that were `t.Log` calls printing the exact output a total failure produces.

The one suite with nothing wrong with it is the one testing the hardest thing.

### 2.38 `tests/soak` tested `math/rand` — ✅ **DONE**

The sixth suite, and the worst of them. It was unwired twice over: gated behind the
`soak_test` build tag, so `go vet ./tests/soak/...` reported *no packages at all* and
`go test ./...` never compiled it.

Wiring it up would have been a mistake, because the suite did not test cleat. It opened a
database, pinged it, and then discarded the handle:

```go
_ = db // used only for connectivity check; actual workload uses in-memory simulation
```

The "workload" underneath that comment was:

```go
time.Sleep(time.Duration(rand.Intn(50)) * time.Millisecond)
success := rand.Float64() > 0.05 // 95% success rate
```

So the error rate it checked against its 10% threshold was a hardcoded 5% coin flip — the
threshold was chosen to sit above the constant on the line above it. The `workflowType` it
selected was passed into the goroutine and never referenced. `EventsPerWorkflow` was set and
never read. The `memSamples` field was documented as "RSS (read from `/proc/self/status`)"
and only ever fed `MemStats.Alloc`. The memory-leak check was a `t.Logf("WARNING: %v")`.

An hour of that asserts that Go's scheduler and `math/rand` work. Scheduling it weekly would
have produced a green **Soak Test** badge for an engine nothing had soaked — worse than
leaving it unwired, because the badge is a claim.

**What it does now.** The workload drives the real `PostgresStore` lifecycle — insert, claim,
append the event mix, complete — at bounded concurrency, continuously, and re-verifies the
checksum chain (§2.30) on one workflow in every hundred. Every error counted is an error the
store returned. Measured against PostgreSQL 16: **37,804 workflows in 60s, 37,765 reaching
`done`, 377 chain-verified, 0 errors**, with heap and goroutines flat.

Four things had to be got right, and each was verified by breaking it:

- **`NewPostgresStore(db, soakQueue)`.** `ClaimWorkflows` filters on `task_queue = ANY($2)`,
  the store's own queue list, and the no-argument constructor polls `"default"` only. Without
  the queue the claim returns `nil` forever — and a `nil` claim means "another worker got it
  first", which is not an error. Removing it: **51,012 successes, 0 completions, 0.000% error
  rate.** The suite would have run for an hour and reported perfect health having executed
  nothing. That is why the final assertion counts rows in `'done'` from the database rather
  than trusting the workload's own counters; only `CompleteWorkflow` puts them there.
- **The leak windows.** The original monotonic-run check fired on 5 consecutive increases —
  probability 1/5! per window, near-certain to fire by chance across the ~120 windows of an
  hour, which is why its memory arm had been demoted to a log line. Replaced with a windowed
  median comparison. The first version pinned the baseline at samples 5..9, which made
  sensitivity depend on run length: an injected 4KB-per-workflow leak (140 MB over 30s) was
  **not caught**. Anchoring the baseline to a *fraction* of the run gives a linear leak the
  same lever arm at any duration; the same injected leak then fails at 3.7x. An injected
  parked goroutine per workflow fails at 2.0x.
- **The error threshold** dropped from 10% to 1%. Against a healthy database every one of
  these operations should succeed; 10% existed to clear a simulated coin flip and would have
  let one workflow in twelve fail unnoticed.
- **`-timeout`.** The obvious spelling, `-timeout="${SOAK_DURATION}"`, kills the binary at the
  exact moment the test stops its workload, so the panic races the assertions and a passing
  run reports as a hang. The workflow computes it (`+25% and 15 minutes`) and the *test itself*
  refuses to start if the harness timeout does not exceed its duration — caught in the first
  millisecond rather than hours later. The computation is also bounded by the job's own budget,
  because GitHub-hosted runners cancel a job at 360 minutes and a 24h dispatch would otherwise
  burn 5.5 hours before being killed and reported as `cancelled`.

Scheduled weekly (Mondays 06:00 UTC) plus `workflow_dispatch`, with skip budget `soak 0`.
Deliberately not a PR gate: an hour per pull request would be absurd, and a leak detector is a
trend instrument that should not block a merge on one noisy run.

**`UNWIRED_SUITES` is now empty.** Every suite under `tests/` is run by a workflow.

### 2.39 The DB-backed suites cannot share a database concurrently — ✅ **FIXED**

`go test ./tests/integrity/... ./tests/upgrade/... ./tests/scale/...` produced **17 failures**.
Run one at a time, all three passed. CI never hit it — each is a separate matrix job with its
own database — but the obvious local command gave a screen of red that meant nothing, which is
its own kind of false signal.

**The first diagnosis recorded here was wrong**, and is kept rather than quietly replaced.
It said `ClaimWorkflow` does not care which suite inserted a row, so `tests/scale` claims
`tests/integrity`'s workflows. That is true, and it is *not* what caused most of the failures.
Giving every suite its own `task_queue` fixed **one** test. The other sixteen kept failing, and
the actual error said so plainly:

```
apply migrations/postgres/001_schema.sql: pq: deadlock detected (40P01)
append events in tx: increment event_count: pq: deadlock detected (40P01)
```

`testutil.TestDB` called `applyPostgresSchemaFile` on **every** invocation — 24 times for
`tests/integrity` alone — and each application takes `ACCESS EXCLUSIVE` on tables another
package is reading and writing at that moment. The advisory lock already in place serialises
schema application against schema application, which is not the collision that bites: it is DDL
against *DML*, from a different process, that deadlocks.

**Both fixes were needed, and each was verified to be load-bearing** by running without it:

- **`applyPostgresSchemaFile` now fingerprints the schema file** (SHA-256, recorded in a
  `cleat_test_schema` table) and skips the DDL when that exact file has already been applied.
  Fixes the 16 deadlocks. Also takes `tests/integrity` from **22.7s to 5.8s**, because applying
  the full schema 24 times was never doing anything the first application had not.
- **Per-suite `task_queue`** (`queue-integrity-tests`, `queue-upgrade-tests`,
  `queue-scale-tests`) in the store constructor and the inserts. Fixes
  `TestMaxConcurrentWorkflows`, which without it still failed on 2 of 2 runs *after* the
  fingerprint fix. `tests/soak` already did this, which is why it was never affected.

Three consecutive concurrent runs green. Tests that add their own columns (all
`IF NOT EXISTS`) or drop objects they created are unaffected — the fingerprint tracks the
schema *file*.

### 2.40 `lint-go` ran one linter and advertised ten — 🔶 **PARTLY FIXED**

`ci.yml`'s header listed `errcheck, gosimple, govet, ineffassign, staticcheck, unused,
misspell, unconvert, gocyclo, gofmt`. `.golangci.yml` disables eight of them, and `gofmt` was
never enabled — it is not in golangci-lint's default set, so listing it under `disable:` was a
no-op. The job ran **`govet` and nothing else**, and `cmd/cleat-worker/config.go` sat
unformatted on `develop` with CI green.

Fixed: a `gofmt` step of its own (not via golangci-lint, whose config excludes `_test.go` from
every linter and would have left most of the repo out), `misspell` actually enabled, and the
header corrected to describe the job that exists.

**The backlog is now measured rather than asserted.** The old note — "the engine refactoring
introduced hundreds of pre-existing issues" — was true when written and had since become
unfalsifiable: no way to tell which linters were still hundreds and which had quietly become
tractable. Measured against the repo's own exclusions, with golangci-lint's default caps
removed (`max-issues-per-linter` and `max-same-issues` silently truncate — `errcheck` reads as
50 with them on and 307 with them off):

| linter | issues | |
|---|---|---|
| `misspell` | **0** | enabled |
| `ineffassign` | 8 | see below |
| `gosimple` | 9 | |
| `unused` | 16 | |
| `staticcheck` | 17 | |
| `unconvert` | 23 | |
| `gocyclo` | 28 | |
| `gosec` | 193 | |
| `errcheck` | 307 | |

**`ineffassign`'s eight are worth reading before enabling it, because three are real
defects** — the shape being *a fallback that is computed and then never used*:

- ~~`cmd/cleat/dev.go:387`~~ — **fixed, see §2.41.** This one was not merely dead: the unread
  `moduleDir` was the fingerprint of a filter that was never written, and its absence made
  `cleat dev --watch` unusable.
- `cmd/cleat/main.go:901` — the transform-file candidate search finds an alternative,
  assigns `transformFile`, and nothing reads it afterwards; only the `found` flag survives.
  So the fallback locates the file and then ignores it.
- `cmd/cleat/main.go:852` — `asDir = dir`, likewise never read.

  **Both are in `runVetAS`, and chasing them turned up §2.42 and §2.43.** They are not
  independent: the function never runs the validation those variables were resolved for. See
  §2.43 — and note that the reason it could not have been fixed earlier is §2.42, where the
  validation it would have called turned out never to have run at all.

Three more are trailing `argIdx++` at the end of a block (`plugins/scheduledbackup/commands.go`,
`routes.go`, `plugins/webhookingest/host_functions.go`). Those are dead but *defensive* —
removing them would make adding the next clause a silent bug — so enabling `ineffassign` means
`//nolint` on them, not deleting them.

The eighth, `cmd/cleatctl/checkdb.go:125`, turned out to be benign and is fixed here anyway:
`healthy = false` is never read, because everything after the ping check keys off
`len(issues) > 0`. Behaviour was correct — the same branch also appends to `issues` — but a
future check that set `healthy` without appending would have silently failed to fail. Now
there is one source of truth.

---

### 2.41 `cleat dev --watch` rebuilt itself forever — ✅ **FIXED**

Chased down from the first of §2.40's three dead fallbacks, and it was the interesting kind:
the unread variable was not the bug, it was the *evidence* of the bug.

`buildDevRun` writes its generated runner as `cleat_dev_*.go` **into the module directory** —
it has to, so `go run` can resolve the workflow package import through `go.mod`. Whenever the
workflow package *is* the module root (a standalone workflow module: the common shape for
`cleat dev`), that directory is inside the tree `runDevWithWatch` is watching. The watch loop
matched every `*.go`. So: build writes a `.go` file → fsnotify reports a `.go` file → 200 ms
debounce fires → build writes another.

`runDevWithWatch` resolved `moduleDir` and defaulted it — the obvious reason being to exclude
exactly these files — and then never used it. `buildDevRun` re-derives the module dir for
itself, so the binding was pure residue of a filter that never got written.

**Measured on a standalone module, nobody touching anything, 25 seconds:**

| | before | after |
|---|---|---|
| rebuilds | **76** | 1 |
| abandoned `cleat_dev_*.go` in the user's source dir | **34** | 0 |

Two further defects fell out of reproducing it:

- **A data race.** `rebuildAndRun` runs on a `time.AfterFunc` goroutine, so two closely-spaced
  edits overlap on `currentCmd`/`currentTmpPath`: both read the old temp path, one wins the
  write, and the loser's generated file is orphaned with nothing left holding its path. That
  is why 76 rebuilds left 34 files rather than 1. Now under a mutex.
- **Ctrl-C left a file behind every time.** The deferred cleanup never runs on a signal, and
  Ctrl-C is how a watch session normally *ends*. Now handled, exiting 130.

The regression test is a pair, and only works as a pair:

- `TestDevWatch_SkipsTheFileItGenerates` runs the real generator and filters its real output,
  so it pins the coupling that actually broke rather than asserting the constant equals
  itself. Restoring the old `HasSuffix(".go")` filter fails it.
- `TestDevWatch_RebuildsOnUserSources` is the non-vacuity half. A filter returning `false` for
  everything satisfies the first test while turning `--watch` into a silent no-op; verified
  that this fails it.

---

### 2.42 The AssemblyScript determinism checks had never run — ✅ **FIXED**

Found while chasing §2.40's second and third dead fallbacks (`runVetAS`). The transform
carries determinism checks numbered **E001–E005** — `Math.random()`, `Date.now()`,
`console.log()`, `process.*`, missing-HostCalls. On real AssemblyScript source, all of them
were inert.

**Two independent reasons, either one sufficient.**

1. **The walker could not read AssemblyScript.** `_walkStatements` enumerated child
   properties by ESTree/Babel name. AssemblyScript's AST uses different ones, and for the
   node that matters most it has no such property at all:

   | transform looked for | AssemblyScript has |
   |---|---|
   | `node.callee` (the call test itself) | `node.expression` |
   | `callee.object` | `expression.expression` |
   | `consequent` / `alternate` | `ifTrue` / `ifFalse` |
   | `declaration` | `declarations` (array) |
   | `init` | `initializer` |

   `node.callee` was never truthy on a real parse, so every call graph came back empty,
   `_findDurableLeaves` returned an empty set, and `if (durableLeaves.size === 0) continue`
   skipped validation for every file. Measured on `examples/as-workflow`: `place_order`
   calls `h.cleatCall` seven times and its recorded callee set was `[]`.

2. **Violations were `console.error` and nothing else.** Even when a diagnostic did fire,
   nothing consumed it. Verified before the fix: `Math.random()` inside a `@cleatEntry`
   function → **asc exit 0, no diagnostic, a deployable `.wasm` produced.**

**Why no test caught it.** `detects_math_random` hands `_validateDurableFunction` a synthetic
AST literal built in the shape the walker assumed, and calls it directly — bypassing
`afterParse`, the call graph, and the durable-leaf gate. It asserted the diagnostic *string*
was producible, which was true the whole time. The fixture agreed with the bug.

**Fixed.** The walk is now shape-agnostic — it visits every own property rather than a list
of names, so it cannot miss a node kind the way a name list rots when AssemblyScript adds
one. Skipping the back-references (`node.range.source.statements` is the whole file) and a
cycle guard are the cost. Violations are collected and thrown at the end of `afterParse`,
which is what makes `asc` fail. `CLEAT_AS_ALLOW_NONDETERMINISM=1` downgrades them to loud
warnings so a false positive cannot block anyone.

**Two further defects fell out of it, both found only by running the thing:**

- **Entry points were not in the durable closure.** It was seeded only from functions making
  an `h.*` call, so a `@cleatEntry` workflow that happens not to call the host was never
  validated — exactly the workflow whose only nondeterminism is a bare `Math.random()`. An
  entry point *is* durable; it now seeds the closure.
- **E005 false positive, caught on a real example.** The first real run failed the
  `examples/widget-store-as` build: `checkoutWorkflow` is a hand-written raw ABI export
  taking `(argsPtr, argsLen, outPtr, maxOutLen)` that does `let h = new HostCalls()` in its
  body. It has host access, it just did not receive it from a caller. E005 exists to catch a
  durable helper that *cannot* reach the host, so it now also accepts one that constructs its
  own.

**Verified:** E001/E002/E003 each fail the build with a real `line:column` (location never
resolved before either — `Range.start` is a character offset, and the code tested
`range.start.line !== undefined`, which was never true). The escape hatch lets a build
through while still printing. All three AS projects in the repo build clean. The new
`rejects_nondeterminism` test was confirmed to fail under each of the three fixes reverted
separately.

**Not verified, and left open:** I could not construct a *compilable* true positive for E005.
The obvious fixture — a durable helper referencing `h` without receiving it — is rejected by
AssemblyScript's own type checker first. E005 may be unreachable in code that compiles at
all. It is left enabled, since the false positive above is fixed and it costs nothing, but
nobody should treat it as a check known to work.

**Also still open:** the durable closure propagates *upward* only (callers of a durable
function become durable). A pure helper called *by* a workflow, making no host calls of its
own, is not validated — so `Math.random()` inside it is still missed. Downward propagation is
the obvious fix but risks false positives across the SDK, and I had no evidence about how
noisy it would be.

---

### 2.43 `cleat vet --lang as` cannot fail — ✅ **FIXED** (WS-3, 2026-08-04)

> **Renamed.** This entry said `cleat vet --target assemblyscript`. There is no `--target`
> flag on `vet`; it is `--lang`, and the value is `as` (`cmd/cleat/main.go:137`). `--target` is
> `cleat build`'s flag. Minor, but the command as written never existed, so anyone trying to
> reproduce the defect from this heading would have got an unrelated error.

**Fixed by making `runVetAS` compile the project.** It now runs

```
npx asc assembly/index.ts --runtime stub --transform @cleat/transform --noEmit
```

and propagates the exit status. `--noEmit` performs the whole compilation — parse, transform,
diagnostics — without writing a `.wasm`, so vet gets exactly the checks build gets, and the
two agree about what compiles. Everything else in the invocation mirrors
`runBuildAssemblyScript` for that reason.

This was only possible because of §2.42: the transform's E001–E005 determinism checks used to
be `console.error` and nothing else, so even `cleat build` exited 0 on a violation. They throw
from `afterParse` now, and that throw is what fails `asc`.

**A missing toolchain is now an error, not a pass.** `cleat build` already exits 1 when npx is
absent, so there is precedent; and a vet that returns 0 because it could not look is precisely
the defect being fixed. Same for a missing `package.json` or `assembly/index.ts`, both of
which used to return 0.

**Tests:** `cmd/cleat/vet_as_test.go`. The load-bearing assertion is the exit code on a
*violating* workflow — a test that only checked a clean project passes would have been
satisfied by the old always-0 implementation, which is the trap. Confirmed against the old
behaviour: three of four subtests fail, and "accepts a deterministic workflow" passes either
way, exactly as predicted.

No CI job is affected: `scripts/ci-check.sh` is the only caller of `cleat vet`, and it runs
`--lang go`.

**§2.40 residual, measured after this change.** `cmd/cleat` is now clean under `ineffassign`
— the two dead fallbacks this entry describes were two of its eight findings. Four unique
findings remain repo-wide in non-test code, and enabling the linter needs each addressed:

| file | assignment |
|---|---|
| `internal/closure/threading.go:42` | `usesGlobalH` |
| `plugins/scheduledbackup/commands.go:211` | `argIdx` |
| `plugins/scheduledbackup/routes.go:420` | `argIdx` |
| `plugins/webhookingest/host_functions.go:87` | `argIdx` |

The three `argIdx` ones are the defensive trailing increments §2.40 says to keep and annotate
with `//nolint` rather than delete — removing one makes adding the next clause a silent bug.
Neither `internal/` nor `plugins/` is WS-3's, so this is left measured rather than done.

---

#### Original entry

### 2.43 `cleat vet --target assemblyscript` cannot fail — 🔴 **was OPEN**

The remaining two of §2.40's three dead fallbacks are both in `runVetAS`, and they are
symptoms of the same thing: **the function never vets anything.**

- `cmd/cleat/main.go:852` — `asDir = dir` is computed and never read.
- `cmd/cleat/main.go:901` — the transform-file candidate search finds an alternative, assigns
  `transformFile`, and only the `found` flag survives.
- `nodePath` is resolved and then discarded with `_ = nodePath`.

The comment says "Run the AS transform's vet validation via Node.js" — no node process is
ever started. Every path returns 0, after printing a line that reads like a check ran. The
three dead assignments are the residue of the validation that was going to use them.

Now that §2.42 makes the transform's checks real, the fix is available in a way it was not
before: run `asc --noEmit` with the transform and let it fail. Left open here because it
needs a decision about requiring `node_modules`/`asc` for `cleat vet`, which changes the
command's contract.

---

PR #208 (`fix/wasm-build-replace-propagation`) was closed without merging on 2026-08-03.
Recorded here so nothing below has to be rediscovered from scratch.

- **PR:** https://github.com/cleat-team/cleat/pull/208 (closed, not deleted — the diff is
  still readable on GitHub)
- **Head SHA:** `df1119a14adaab9d6ec730f30c2de1f28dc1f540`
- **Merge base:** `1e10460`

**Why it was closed rather than merged.** 19 commits that add the dispatcher model, remove
it, restore it, then revert parts of the revert, plus three `chore: trigger CI re-run`
commits and a merge of `develop`. `mergeable: CONFLICTING`. By the time it was assessed it
was 19 ahead / 9 behind, missing #215–#223, and its last CI run was 31 pass / 2 fail — the
two failures being MySQL and SQL Server, the backends it modified. Most importantly, its
headline fix had already landed independently: `git diff develop...df1119a -- wasm/build.go
wasm/exports.go` is **empty**. The replace-directive propagation the branch was named for is
on `develop` verbatim via `c26c332`.

Three agents assessed the diff by area. §2.18, §2.19 and §2.20 above came out of that and
are recorded as defects in their own right — those are the real yield, and none of them
needs the branch.

**Worth rebuilding (not worth cherry-picking):**

1. **Real `AdminForceComplete` / `AdminForceFail` / `AdminReReplay` bodies.** The largest
   coherent chunk of work in the PR: ~445 lines across `engine/db.go`, `mysql_ops.go`,
   `mssql_operations.go`, each with a generation check, not-found-vs-stale disambiguation,
   an `admin_action` audit event in the same transaction, and post-commit
   `ClearStickyWorker` / `ReleaseWorkflowConcurrencyKeys`. #217 landed the interface,
   the event type and every mock; `engine/store_admin_stubs.go` is still literal
   `"not implemented yet"`. **Do not port as written:** the inserts use columns `op`,
   `err` and `timestamp_ms`, none of which exist — the schema has `operation`, `error` and
   `created_at`. The MSSQL variant also drops the `tenant_id` filter from its `UPDATE`
   while the MySQL sibling keeps it, which is precisely the ownership gap §1.7 says to
   close first. There were no tests for any of the three bodies.
2. **Plugin `StartWorkflow` capability gating** (`plugin/plugin.go`, `plugin/registry.go`,
   `plugins/{eventtriggers,jobqueue,scheduler}/plugin.go`). Applies cleanly; three genuine
   tests. The three plugin declarations are *required*, not optional — those plugins call
   `env.StartWorkflow` today (`plugins/eventtriggers/publish.go:138`,
   `plugins/jobqueue/background.go:154`, `plugins/scheduler/background.go:154`) and would
   break under the gate without them. Related latent hazard worth fixing alongside:
   `InitAll` does `pluginEnv := env` (`plugin/registry.go:81`), so every ReadOnly/ReadWrite
   plugin shares one `*Environment`. Harmless today because nothing mutates per-plugin
   fields — the capability feature is exactly what would make it load-bearing, which is why
   the branch added a `shallowCopy()`.
3. **`--migration-lock-timeout` + retry** (`migration/runner.go`, ~79 lines). Adds
   `maxMigrationRetries = 3`, `runMigrationWithRetry` (1s delay, honours ctx cancellation)
   and per-dialect lock timeouts — Postgres `SET LOCAL lock_timeout`, MySQL
   `SET SESSION innodb_lock_wait_timeout`, MSSQL `SET LOCK_TIMEOUT`. Real tests:
   `TestRunMigrationWithRetry_SuccessAfterFailures` counts actual attempts,
   `TestLockTimeoutSQL_*` assert exact per-dialect SQL. Needs a rebase (`develop`'s runner
   grew a `trackingTable()` helper), a 4th `NewRunner` param at both call sites
   (`cmd/cleat-worker/main.go:550,593`), and a decision on the default — 0, i.e. today's
   behaviour, opt-in.
4. **`--latency-histogram-buckets`.** Absent from `develop`, applies cleanly, wraps 8
   latency histograms and correctly leaves execution-duration histograms alone. 13 of its
   14 tests are genuine — real `sdkmetric.ManualReader` assertions on observed bucket
   bounds. Two catches: the `metrics.go` hunk smuggles in an unrelated
   `cleat_canary_routing_total` counter that must be dropped, and full wiring needs a
   `prometheus.Config.LatencyHistogramBuckets` field that does not exist yet.
5. **`cleat build --dump-ir`** + `internal/closure` `DebugInfo`. Useful for answering "why
   was this function pulled into the durable closure". The `internal/closure` half applies
   cleanly; the `cmd/cleat/main.go` half collides with the already-landed `--version` flag
   and needs one more bool threaded through `runBuild`. `TestDebugInfoPopulated` checks real
   `Tag`/`Reasons` content. Lowest priority of the five.

**Deliberately dropped, with reasons — do not resurrect:**

- *Replace-directive propagation, the dispatcher model, `--version` on `cleat build`,
  `ABIVersion: 1`, idempotent upsert-deploy, `>=` version-compat, `prompts/cto-agent.md`* —
  all already on `develop`, byte-identical, via `c26c332` / `9cb5d01` / `6f6cdf1` / #216.
- *Admin route gating.* #208 registers `/api/admin/instances/` only when the flag is on;
  `develop` (`cmd/cleat-worker/app.go:79-85`) always registers and gates destructive
  operations at request time. `develop`'s design is better — it keeps read-only admin
  inspection working with the flag off, where #208 would 404 the whole namespace.
- *Canary version routing* (`canary_weight` column + `ResolveVersionWithCanary` /
  `SetCanaryWeight`, Postgres-only). `develop` already has the strictly more general
  `workflow_routing` table with full CRUD (`engine/store_versioning.go:108-224`, mirrored
  in MySQL and MSSQL): N-way weighted routing, versus #208's binary stable/canary split.
  Adding `canary_weight` would be a second, weaker concept for the same job. **But there is
  a real gap underneath it:** `resolveChildVersion` (`engine/children.go:58-131`) has no
  routing case at all — its `stable` branch calls `ResolveVersionByTag` and nothing ever
  consults `workflow_routing`. Worth fixing by wiring the existing `PickVersionByRouting`
  in — no new column, no new migration.
- *Cumulative WASM allocation limit.* Superseded by a better-integrated equivalent on
  `develop`: `WithWasmCumulativeAllocationMax` (`engine/engine.go:92`) plus
  `tryClaimCumulativeAllocation`, already flag-wired and tested. #208's version calls
  `wasm.ReadMemoryInitialPages`, which does not exist on `develop`, and its
  `cumulative_alloc_test.go` collides by name with `develop`'s.
- *`ARCHITECTURE.md` edits.* Stale and self-contradictory — the diff *reverts* the correct
  wasip1 description back to TinyGo while adding a TinyGo-deprecation note two paragraphs
  later, and lists the Admin API as "incoming" after #217 shipped it. Salvage only the note
  that cleat-238 (`--dump-ir`) and cleat-241 (canary routing) remain unbuilt.

### 2.70 Multi-DB CI ran entirely on wazero — ✅ **FIXED** (WS-3, 2026-08-04)

`multi-db-ci.yml` prefixed all four of its test steps with `CGO_ENABLED=0`. Because
`NewWasmtimeBackend` is behind `//go:build cgo`, that does not skip a check — it compiles the
primary backend out and runs everything on the fallback. The workflow whose stated purpose is
validating MySQL and SQL Server *under workflow execution* was never executing a workflow on
the engine of record.

Measured rather than argued — `go test -list` against `./engine/`:

| | tests compiled in |
|---|---|
| `CGO_ENABLED=0` | 2401 |
| CGO on | 2557 |

The **156-test difference** is the entire `TestClosure_*` family — the real WASM
host-function paths — plus 28 wasmtime-specific tests, including the four §1.5
runaway-workflow regressions. None of them had ever run in this workflow.

The failure mode is this repo's signature shape — a green signal attached to nothing:

```
$ CGO_ENABLED=0 go test -count=1 -run 'Wasmtime' ./engine/
ok  	github.com/cleat-team/cleat/engine	0.370s
```

That `ok` is what multi-DB CI reported. Zero tests matched, because the file defining them is
`//go:build cgo`. Counting the §1.5 runaway-workflow regressions specifically: `-run
InfiniteLoop` executes **0** tests under `CGO_ENABLED=0` and **2** with CGO on. A suite that
cannot run cannot fail, and reports success either way.

Fixed by hoisting `CGO_ENABLED: 1` to a workflow-level `env:` and deleting the four inline
prefixes. **Set explicitly, not merely removed:** Go defaults `CGO_ENABLED` to 1 only when a C
compiler is present and to 0 otherwise, so relying on the default silently reinstates the same
failure on a runner without gcc. Pinned to 1, that case fails loudly instead.

Timeouts raised (300s→900s on the two full-engine jobs, 600s→900s on `MultiBackend`) because
the step now runs 156 more tests, several of which compile WASM modules. Not tuning — a
timeout here would present as a test failure and send the next reader down the wrong path.

Verified locally against the live MySQL 8.4 and SQL Server 2022 stood up for §1.7, with
`go env CGO_ENABLED` confirmed as `1` first: full `./engine/...` green apart from the two
port-hardcoded factory tests noted below; `TestPluginMigrations` green; both
`*_MultiBackend` plugin tests green across all three backends; wasmtime suites confirmed
executing rather than skipping.

Two caveats stated rather than buried:

- The two failures in the local `./engine/...` run are `TestMySQLStoreFactory` and
  `TestMySQLIntegration_FactoryOpenStore`, which ignore `CLEAT_TEST_MYSQL` and hardcode
  `tcp(127.0.0.1:3306)` (already recorded above as config drift). They fail only because the
  WS-3 sandbox runs MySQL on `3308`; CI publishes `3306`, so they pass there. **This means
  the CI-green claim for that one step is inferred, not observed** — the honest boundary of
  what was verified locally.
- `ci.yml:145` (`go vet ./...`), `ci.yml:629` (`go build ./cmd/...`) and
  `ecosystem-ci.yml:24` (`go install ./cmd/cleat`) still pass `CGO_ENABLED=0`. Not touched
  here: they are build/vet steps, not execution tests, so the wazero substitution does not
  apply the same way. They are worth a separate look — `ci.yml:731` already carries a comment
  about a CGO-less binary that "read as though `CGO_ENABLED=0` were the shipped
  configuration, which is the belief that let 2.28 survive," so the hazard was recognised in
  that file and missed in this one.

---

### 2.71 MSSQL session context is cleared by connection pooling — 🔶 **PARTLY FIXED** (found by WS-3, fixed by WS-1, 2026-08-04)

`MSSQLStoreFactory` gives each tenant a pool whose wrapped connector runs
`sp_set_session_context`. The doc comment at `engine/mssql_store.go:270-272` says this happens
"on every new connection, so RLS is enforced automatically."

It happens once per connection, and does not survive the connection being recycled.
`database/sql` calls `ResetSession` when a connection is returned to the pool, `go-mssqldb`
issues `sp_reset_connection`, and that clears `SESSION_CONTEXT`. Measured directly against
SQL Server 2022 with `SetMaxOpenConns(1)`, so the reacquired connection is provably the same
one:

```
same connection, right after setting: 11111111-1111-1111-1111-111111111111
after return to pool and re-acquire:  <NULL>
```

**Consequence.** With the shipped schema's seven filter predicates in place and no session
context, every tenant-scoped *read* matches nothing. Writes are unaffected — the write paths
call `setSessionContext(tx)` inside their own transaction (`mssql_lifecycle.go:521`,
`mssql_events.go:407`, `mssql_signals_promises.go:92`). Reads such as `ListWorkflows` and
`GetWorkflowByID` rely on the connector alone, and `ListWorkflows`'s own
`WHERE tenant_id = @p1` returns the right rows only for RLS to then discard them.

It fails **closed**, so this is a correctness and availability defect rather than a leak. But
on SQL Server with the real schema, tenant-scoped reads return nothing.

**Why no test caught it.** `engine/testutil/mssql_schema.go` hand-writes its `CREATE TABLE`
statements and defines **none** of the seven `CREATE SECURITY POLICY` statements the real
migration carries. Every MSSQL test in the repo therefore runs against a schema with no
tenant backstop, where a cleared session context has no observable effect. Same shape as the
PostgreSQL superuser trap in §1.7: the mechanism under test is absent from the test
environment, and its absence looks like success.

**Fix direction** (not taken here — `engine/mssql_store.go` and `engine/testutil/` belong to
other workstreams, and cross-stream coupling #3 says coordinate before touching the MSSQL
path): establish the session context per transaction as the write paths already do, or
re-apply it on `ResetSession`. Separately, the MSSQL test schema should apply the real
migration so the policies exist, or every future isolation test on that backend will pass
without meaning anything.

#### The connection half — fixed, and it was worse than described

Measured against a real SQL Server 2022 rather than reasoned about, and the first
measurement corrected the diagnosis. The session context was not merely lost *after* a busy
period: it was absent from **every** query, including the first one on a brand-new pool.

`getOrCreateTenantPool` calls `PingContext` when it builds the pool. That returns the
connection to the pool, so the very first application query is already being handed a
recycled connection and has already been through `sp_reset_connection`. "Tenant-scoped reads
sometimes return nothing" is really "tenant-scoped reads never work".

```
session context on a fresh connection = "", want "11111111-1111-1111-1111-111111111111"
```

Fixed by wrapping the driver connection in `tenantSessionConn`, whose `ResetSession` lets the
driver do its own reset first — that *is* the `sp_reset_connection` that clears the context —
and then re-applies `sp_set_session_context`. An error there is returned rather than
swallowed, so `database/sql` discards the connection: one whose tenant could not be
established must never serve a query.

The wrapper forwards the optional interfaces go-mssqldb's `*Conn` implements (`Prepare`,
`Close`, `Begin`, `PrepareContext`, `BeginTx`, `Ping`, `IsValid`, `CheckNamedValue`).
`QueryerContext` and `ExecerContext` are deliberately not claimed: the driver does not
implement them either, and claiming them would break the fallback `database/sql` relies on.

Two existing tests asserted `Connect` returns the *identical* connection object. That is an
identity assertion the fix necessarily breaks, and it pinned an implementation detail rather
than a behaviour; they now assert the wrapper carries the right connection and the right
tenant.

- Tests: `engine/mssql_session_context_test.go`. `SetMaxOpenConns(1)` makes the reuse
  provable — a pass cannot come from a fresh connection being opened instead. A second test
  interleaves two tenants' pools so that a reset hook which cached one tenant, or read from
  a shared variable, would pass the first test and fail this one. Both confirmed to fail with
  the re-apply removed.

#### The schema half — now observable, and the missing tables are recorded

`engine/testutil/mssql_schema.go` hand-writes 334 lines of `CREATE TABLE` and defines none of
the seven security policies. Pointing it at the real migration is the right fix and is **not**
a small change: it switches RLS on for every MSSQL test in the repo, and any test that does
not establish a session context will start returning nothing. That is the point — but it is a
test-suite migration, not a one-liner, and `engine/testutil/` is WS-2's.

Rather than block on that, MSSQL now gets the shape PostgreSQL already uses for exactly this
problem: leave the default test schema alone and give the tests that care about RLS a scope
where it is genuinely switched on — the analogue of `testutil.OpenPostgresRLSTestDB`.
`engine/mssql_rls_enforcement_test.go` reads `fn_tenant_filter` and the seven
`CREATE SECURITY POLICY` statements **out of the real migration** and applies them, so the
predicate under test is the shipped one and cannot drift from it. It drops them again on
cleanup, and drops any left by an interrupted earlier run before it starts — a filter
predicate left behind blanks every later MSSQL test in the binary.

That closes the verification gap without the suite-wide migration: `TestMSSQLTenantIsolation_UnderRealSecurityPolicies`
fails with the §2.71 fix reverted, reporting the production symptom rather than a mechanism —
`round 0: tenant A got <nil> for its own workflow`.

**One thing this nearly got wrong, which is worth keeping.** The cross-tenant half of the
test was first written as "tenant A's *store* must not return tenant B's workflow". That
assertion passes against a **wide-open filter predicate**, because `MSSQLStore`'s own SQL
carries `tenant_id = @p` and the Go layer does the filtering regardless of what RLS does —
the same "a test can pass because of a layer other than the one you think you are testing"
defect as §1.1's first fence test. Caught by making the predicate permissive and watching the
test stay green. It now runs the cross-tenant check as a raw statement on the tenant's own
pool, where no Go-level filter exists and the policy is the only thing that can hide the row;
with a permissive predicate that fails with
`tenant A's connection can see the other tenant's workflow … (1 row(s))`.

**Still open:** the test schema is missing two tenant-scoped tables the shipped schema has —
`workflow_routing` and `workflow_tags` — so their policies cannot be applied at all. That set
is now asserted rather than assumed, so a *new* divergence fails the test instead of being
tolerated silently. Pointing `engine/testutil/` at the real migration remains the real fix.

**One exception, as of 2026-08-04.** `cmd/cleat-worker/tenant_isolation_mssql_test.go` was
written and shipped skipped against this item; unskipping it was the recorded acceptance
test, and it now passes. It sidesteps the shared helper entirely — it applies
`migrations/mssql/001_schema.sql` to a dedicated database of its own, and asserts the
policies are enabled *before* asserting anything about tenants, so it cannot pass against a
schema without RLS. So there is now exactly one MSSQL test that observes a security policy
enforcing tenant isolation end-to-end, through the HTTP layer.

That does not close the schema half. One test carrying its own migration is a workaround for
a shared helper that lacks the policies, not a replacement for fixing it: every *other* MSSQL
test in the repo still runs without a backstop. `scripts/skip-baseline.txt` drops this entry
from 2 skip sites to 1, the remaining one being the ordinary "CLEAT_TEST_MSSQL not set"
guard.

---

### 2.72 Two languages ran on wasmtime because a parser was broken — ✅ **FIXED** (WS-3, 2026-08-04)

`readImportSection` (`wasm/metadata.go`) advanced past an import descriptor's **kind byte**
and left its payload unread. For a function import the payload is a type index, so the next
iteration read that index as the following import's module-name length. The parser
desynchronised after the *first* import, and every module with two or more failed:

```
AssemblyScript   readImportSection err=corrupt WASM import 1: field name overflows section
Java (TeaVM)     readImportSection err=corrupt WASM import 2: name overflows section
```

Both fixtures parse cleanly with an independent parser — 3 and 7 imports, `env.abort` present
in the AssemblyScript one, `teavm.*` in the Java one, which are exactly the two patterns
`detectLanguageFromImports` looks for. **Those branches had never once been reached.**

Invisible because both callers read an error as "cannot tell": `NeededEnvImports` falls back
to registering every host function (safe, but the optimisation never applied), and
`detectLanguageFromImports` returns `""`, which `DetectLanguage` turns into its `"go"` default.

**The trap, and why the fix is two changes in one commit.** AssemblyScript and TeaVM Java
reached the wasmtime backend *because* of this bug — misidentified as `"go"`, they matched the
only entry in the backend map. Fixing the parser alone would have identified them correctly,
matched nothing, and silently dropped both onto wazero. A bug fix that downgrades two
languages onto the fallback runtime is worse than the bug, and nothing in the suite would have
said so. `cmd/cleat-worker/backend_routing.go` now states the routed set explicitly, and
`TestRealFixturesRouteToWasmtime` asserts detection *and* destination together — verified to
fail with the parser fixed and the list left at `{"go"}`.

**Also fixed: the discarded component error.** `backend_wasmtime.go` had
`if result, err := b.ExecuteComponentCGo(...); err == nil`, throwing the error away. Every
native-component failure surfaced only as whatever the decomposition fallback happened to
report — typically an unresolved-import error, which reads like "wasmtime cannot run this
component" when the cause was something else entirely. It is now logged and attached to the
fallback's error.

**What that revealed, for whoever takes §2.28's residual further:**

- **Rust loads and executes on wasmtime today.** `place_order` from `examples/rust-workflow`
  ran and returned the guest's own `{"error": "EOF while parsing a value at line 1 column 0"}`
  from a nil input. `wasm/metadata.go:353-355` routes it away deliberately — *"so Rust modules
  fall through to the default runtime instead of crashing in wasmtime"* — which does not
  reproduce now. Same shape as the stale `CGO_ENABLED=0` note in `CLAUDE.md`: a reason that
  was true when written and outlived its cause. **Not moved**, because `tests/cross-language`
  is one of the six suites this plan records as run by no CI job, so there is no signal to
  regress against. Wire that up first; the routing change is then one line in
  `wasmtimeLanguages`.
- **Python is closer than it looks, and blocked by three separate things.** With the native
  component path enabled, the component *compiles and links* on wasmtime. Its failures are
  (1) `engine/component_cgo.go` sits behind the `wasmtime_component_cgo` build tag, which
  **no build, CI job, Makefile or Dockerfile sets** — every build gets the stub; (2) it needs
  `CGO_CFLAGS=-I<wasmtime-go>/build/include`, documented at `component_cgo.go:26` and
  automated nowhere; (3) `componentGetFunc` passes `nil` as the parent export index, so it
  resolves only flat top-level exports and cannot reach functions nested inside an exported
  interface, which is how componentize-py emits them. Probing export names, `run` was found
  and called — it trapped in execution rather than being missing. That trap is inconclusive:
  the probe passed a nil `HostHandler`, so host calls could not work. What it does establish
  is that the wall is not wasmtime's component support.

**Method note.** None of this was visible by reading. The parser looked correct, the language
branches looked correct, and the routing looked correct; the fixtures were the only thing that
disagreed. Every claim above came from executing against checked-in toolchain output.

#### 2.72 follow-up — one routing table, and Rust moved onto wasmtime (2026-08-04)

The routing decision existed **twice**, and the two copies disagreed:

| | registered for wasmtime |
|---|---|
| `cmd/cleat-worker` | `go` |
| `cleat/wasmtest` | `go, assemblyscript, python, java` |

So the test harness ran Python on a backend the worker never sends it to, and neither routed
Rust. A harness whose routing differs from the worker's is exercising a configuration nobody
runs — which is worse than exercising the wrong one loudly, because it looks like coverage.

Now single: `engine.WasmtimeLanguages` / `engine.RunsOnWasmtime`, with both consumers reading
from it. `cmd/cleat-worker/backend_routing.go` forwards rather than re-listing, so the two
cannot drift again.

**Python removed from the harness's list.** It fails on wasmtime — its component reaches the
decomposition path and dies on `incompatible import type for env::abort`, reproduced through
`cleat/wasmtest` with a real `HostHandler`, so not a probe artefact. Nothing caught the
mismatch because nothing ran it: `plugin-harness-ci.yml` installs no Python toolchain, so
`TestPluginCalls_Wasm_Python` skips and that registration had never once been exercised.

**Rust moved onto wasmtime**, and the sequencing is the point. It could not be justified
before, because `tests/cross-language` built `wasm32-wasip1` while `cleat build --target rust`
ships the `wasm32-unknown-unknown` cdylib (`build_rust.go:34`) — so the suite covered an
artifact no user runs, and could not have tested the claim that kept Rust out:

> wasmtime-go v44 still crashes on fn.Call for Rust cdylib core modules

That claim was structurally uncheckable by the only suite that would have checked it, because
the suite never built a cdylib. It does not reproduce. With the suite switched to the shipped
target, all seven of its tests pass on wasmtime — including both cross-replay directions,
executing under one runtime and replaying the recorded history under the other.

**Correction to a first draft of this entry:** `TestPluginCalls_Wasm_Rust` was cited as
additional coverage. It is not, in CI. `plugin-harness-ci.yml` installs no Rust toolchain at
all, so that test skips there and only passes locally. `tests/cross-language` is the whole of
the gate.

**And the gate needed a toolchain fix nobody had needed before.** Every workflow installs
`wasm32-wasip1` only — five of them — while `cleat build --target rust` has always required
`wasm32-unknown-unknown` (`build_rust.go:34`). Pointing the suite at the shipped target made
CI fail with `error[E0463]: can't find crate for 'std'`, after passing locally on a machine
that happened to have both targets installed. A local pass on a richer toolchain than the
runner's is not evidence the job works, which is the same lesson the `test-go/engine` skip
budget records from the other direction.

Worth someone's attention, not fixed here: **no CI job installs the target `cleat build
--target rust` needs**, so that build path — the one users actually invoke — is exercised
nowhere. And `tests/plugin-harness/wasm_plugin_test.go` skips on
`"wasmtime-go compatibility issue with this WASM module"`, a skip that would swallow precisely
the regression this section is about.

Same shape as the stale `CGO_ENABLED=0` note in `CLAUDE.md`: a reason that was true when
written, outlived its cause, and stayed because the thing that would have contradicted it was
pointed somewhere else.

**Where each language now runs:** wasmtime for `go`, `assemblyscript`, `java`, `rust`; wazero
for `python` alone, until the native component path (§2.72 above) is buildable.

**Observed, not chased:** `TestPluginCalls_Wasm_Go` skips locally while the AS, Java and Rust
siblings pass. Not investigated, and not caused by this change — Go's routing is unaltered —
but a Go-path test skipping in the harness for the primary language is worth someone's
attention.

---

**Method note for Phase 3.** Every "already on develop" verdict above was settled by
diffing against `develop` and by `git apply --check`, not by reading commit messages — the
mistake §0.2's correction calls out. Three of the four highest-value findings (§2.18, §2.19,
§2.20) are defects on `develop` that the PR happened to touch, not features the PR added.
Assessing a stale branch for salvage turned out to be a decent defect-finding technique in
its own right, because it forces a line-by-line read of code nobody has looked at recently.

---


## Phase 3 — Put falsification in the loop

The economic finding: **~$900 of generation, ~$0 of falsification.** Compute was 4–12% of
total project cost; your attention was the rest. Compute is the one input that can substitute
for attention at the seams, and it went unspent there.

- **Budget rule:** allocate ~15% of token spend to agents whose only job is to find why
  something doesn't work. Adversarial, not confirmatory — prompt them to refute.
- **Standing fresh-eyes run:** weekly, an agent with no repo context follows the README from
  a clean checkout and reports where it breaks. This found 8 independent failures today.
- **Pre-merge skeptic:** before any feature branch lands, one agent tries to prove the feature
  is not actually wired in. Cheap; would have caught 1.3 and 1.4 at the source.
- **Claim audit:** an agent that checks assertions in docs against code. Every doc number in
  this repo that I checked was wrong or stale.
- **Guard the mocks.** 1.3 survived because a mock discarded the parameter under test. When a
  mock ignores an argument, the test is asserting nothing about it.

---

## Phase 4 — Claims, positioning, hygiene

**Correct the overclaims** (~1 session). Each is currently falsifiable by a reader:

- `DX_COMPARISON.md:30` — "88M steps/sec core throughput means WASM overhead is negligible."
  That benchmark's `durableCall` returns a hardcoded `{"status":"ok"}` with no DB and **no
  WASM** (`benchmarks/cleat_bench_test.go:118`). The file's own package doc says so. Delete
  the claim or requalify it as an in-process framework microbenchmark.
- `README.md:62` — "full feature parity across all three" backends. Not true for RLS.
- `ARCHITECTURE.md:17` — names wasmtime; README names wazero; reality is two backends.
  The whole module table also still uses pre-refactor `internal/` paths.
- `docs/review-status.md` — declares the project production-ready off an audit of 11 plugins
  and pre-refactor paths.
- `specs/CleatClaim.tla` — uses `=====` as decorative separators, which is TLA+'s module
  terminator. First one is at line 53 of 495, so 89% of the spec is outside the module. No
  `.cfg` files exist for any spec and TLC never runs in CI. Either fix + run them, or move
  them to `docs/` as design notes.
- `benchmarks/comparative/results/` contains only `template.md`. The Temporal and DBOS
  harnesses are written — **run them.** Real head-to-head numbers would be a genuine asset.

**Positioning decision** (needs you, not an agent):

Golem Cloud — same WASM-durable-execution bet, funded, founded 2023 — publicly exited the
general durable-execution market in May 2025 and narrowed from polyglot to TypeScript+Rust
only, citing WASM immaturity. `DX_COMPARISON.md:74` independently reached the same
conclusion: "Go is the only production-ready SDK."

Recommendation: **lead with Go; make the differentiator MySQL and SQL Server.** No competitor
runs durable workflows on either — DBOS is Postgres-only, Temporal needs its own cluster.
Three real dialect implementations is hard, unglamorous, and genuinely yours. Label the other
SDKs experimental rather than carrying them as headline features.

**Branch triage** — follow `BRANCH-TRIAGE.md` §10 ordering. Take
`feature/review-quality-fixes` early if you want it at all (it splits `engine.go` — the one
real churn hotspot, 78/402 commits, 18.9× churn ratio — into 14 files); cost grows with every
merge touching `engine/`.

**Repo hygiene** (~0.5 session): `.git` is 184MB. Nine unstripped ELF binaries and two `.wasm`
files are tracked (`bin/*`, `cmd/cleat-worker/cleat-worker`, `durable-worker`,
`durable-bench`); 445 `node_modules` files stayed tracked after the ignore rule was added.
Untrack, extend `.gitignore`. This is also why line-counting tools report nonsense.

---

## If you only do three things

1. **Phase 0** — half a day. Without it nothing else is verifiable, and you are steering
   without instruments.
2. **Tests 2.1 and 2.2** — the golden path and the two-worker race. Between them they cover
   the highest-severity defect (data loss) and the entire class of onboarding failures.
3. **Fix 1.1** — the unfenced `DELETE FROM event_history`. It is the only finding that
   destroys user data, and it fires in exactly the scenario the product exists to survive.

---

## What this does not cover

- Whether the work on the 47 unmerged branches is still wanted. State assessment only.
- Any judgment about whether to continue the project at all.
- Load, soak, or scale testing beyond the resource-exhaustion case in 2.5.
- The Java SDK, which has no Go-side cross-language e2e test comparable to Python's
  (`engine/python_wasm_e2e_test.go`) or AssemblyScript's — verify independently before
  treating it as a production target.
