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

2.1 golden path, 2.2 two-worker race, 2.3 cancellation e2e, 2.4 crash recovery, 2.7 deploy
manifests. 2.6 (tenant isolation through the HTTP API) is now worth more than it was: RLS
is genuinely enforced as of 1.10, so an end-to-end test can finally prove isolation rather
than prove a policy exists — and §2.20 gives it a concrete first target.

### Standing constraints, carried forward

- **`docker-compose.cluster.yml` is only ever exercised in CI.** colima cannot share this
  repo's path (`/Users/Shared/localssd/...`), so compose changes cannot be tested locally.
  Verify scripts inside a container, expect a CI round trip for mount wiring, and remember
  it has already broken once that way.
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
think you are testing.** The only way to know which layer is holding is to break the specific
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
> nothing to find.** Detection is real but unreachable in production. The fix is to wire the
> intent write into `freshCall` / `freshCallWithRetry` / `freshCallWithHeartbeat` — the
> detector needs no changes.
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

### 1.5 Primary WASM backend has no hang protection (~1–2 sessions)

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

### 1.7 Tenant isolation not enforced at the HTTP layer (~2–3 sessions)

`defaultTenantID := "00000000-0000-0000-0000-000000000000"` at
`cmd/cleat-worker/main.go:159`, used process-wide. Callers authenticate per-tenant; every
request is then served from one hardcoded scope. Real RLS exists underneath and is bypassed.

- Also: `migrations/mysql/` and `migrations/mssql/` have **zero** RLS policies against
  Postgres's seven. On those backends a missed `tenant_id` filter is a silent cross-tenant
  leak with no database backstop.
- Also: the new admin API has no ownership check tying `workflowID` to the caller's tenant.
  Currently latent only because the store methods are stubs (`engine/store_admin_stubs.go`).
  **Fix before implementing them.**
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
| 2.5 | **Resource exhaustion.** Deploy an infinite-loop workflow. Assert the worker survives and the workflow is terminated. Run per backend. | 1.5 |
| 2.6 | **Tenant isolation.** Two tenants; assert A cannot read, list, cancel, or admin-act on B's workflows through the HTTP API. Run against all three backends. | 1.7 |
| 2.7 | **Deploy manifests.** Actually start `k8s/`, `charts/cleat/`, `docker-compose.cluster.yml` and assert the worker reaches ready. | `--namespace`/`--tenant-id` crash-loop; all three are currently broken |
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

### 2.15 Durable call failures are all classified as retryable timeouts — OPEN

`cleat.CallErrorCode` exists "so callers can distinguish retryable from non-retryable errors
without string-matching" (`cleat/runtime.go`). It cannot currently do that. Every failure
path in `engine/durablecalls.go` and `engine/heartbeats.go` packs `callErrorCode = 1`,
which the guest enum reads as `CallErrorTimeout` — **retryable** — whether the underlying
cause was a service error, a replay divergence, a cancellation or an ambiguous result.
`engine/plugins.go` packs `0` (`CallErrorUnknown`) throughout instead.

So a permanent failure is reported to workflow authors as a transient one. Fixing it means
classifying errors at the `ServiceCaller` boundary, which is a design change rather than a
patch, and is why it was left out of the 2.10 work.

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


### 2.17 `ShardedStore` claims `limit` from *every* shard and strands the excess — OPEN

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

Left unfixed deliberately: choosing between these changes the latency characteristics of
the claim path, and that is a decision worth making on purpose rather than as a side effect
of a documentation fix.

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

### 2.23 `StartChildWorkflowInSchema` — same omission, but a one-line fix would be a false fix — OPEN

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


### 2.25 Nothing prevents a red PR from merging into `develop` — OPEN (needs repo admin)

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

**The fix is a repo setting, not a code change,** so it is left for whoever administers the
repository: require the jobs that matter on `develop`. Until then, any automation that
merges must gate on **workflow runs for the head SHA** rather than the check-name rollup:

```sh
SHA=$(gh pr view "$PR" --json headRefOid -q .headRefOid)
runs=$(gh run list -c "$SHA" --limit 100 --json status,conclusion,name)
busy=$(echo "$runs" | jq '[.[] | select(.status != "completed")] | length')
```

and require the run count to hold steady across several consecutive polls. Do not hardcode
an expected total: path filters mean different PRs trigger different workflows (#227 saw 36
checks, #229 saw 42), so any fixed threshold either fires early or never fires.

---

## Salvage register — PR #208, closed unmerged

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
