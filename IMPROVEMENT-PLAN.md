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

Work is now split across three concurrent sessions — see **`WORKSTREAM.md`** for who owns which
paths, the per-sandbox database, and the cross-stream couplings. Read that before this file; then
read only your own items below. (It absorbed `PARALLEL-WORKSTREAMS.md`, which this line used to
name, on 2026-09-04. The *reserved migration ranges* it also used to name are retired: take the
next free number above the dialect's high-water mark.)

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

> ~~**Still open:** 1.4 (wire `flushCallIntent`), 1.7 (tenant scoping at the HTTP layer), the
> whole of Phase 2, `cmd/cleat-worker` gofmt, and the four items below.~~
>
> **Audited 2026-08-04. Three of the five claims here are stale, and one is actively
> misleading.** This paragraph was written by `c26c332` about the tree `c26c332` produced, and
> nothing has revisited it since.
>
> - **1.4 "wire `flushCallIntent`" is the wrong instruction now.** `docs/durable-call-intent-design.md`
>   §2 establishes that wiring it in would break every workflow that makes a durable call —
>   the completion upsert guards on `error IS NULL` and the intent row's `error` is the
>   sentinel, so the row could never be completed and every replay would report `[AMBIGUOUS]`
>   forever. Phase A deleted both writer functions. §1.4 below carries the replacement design.
> - **`cmd/cleat-worker` gofmt:** closed by `d75ac51`. `gofmt -l cmd/cleat-worker/` is empty.
> - **1.7:** now 🔶 partly done, not open — see §1.7.
> - Caveats 2 and 4 below are both closed; see the strikethroughs there.
>
> Only "the whole of Phase 2" was still true when written, and Phase 2 has since moved a long
> way as well. **The audit that produced this was flagged twice in earlier sessions and run in
> neither** — which is the same shape as everything else in this document: the signal existed
> and nobody attached it to anything.

### Caveats carried by this branch

These are known-and-recorded, not fixed. Each is a place where the suite is greener than
the code.

1. ~~**`TestASTransform/compiles_to_wasm` never compiles anything.**~~ ✅ **Fixed** in
   `d732ea9`. The fixture now installs `@cleat/sdk` from the checkout, imports the real
   types instead of inline look-alikes, and a compile failure is a `t.Fatalf` rather than a
   `t.Skipf` — that skip is what made the subtest unfalsifiable, since any asc error at all
   was indistinguishable from the missing dependency. Proven to bite.

2. ~~**`testutil.TestDB` skips instead of failing when Postgres is unreachable.**~~ ✅
   **Closed** — verified 2026-08-04, not inferred. `engine/testutil/schema.go:661` now selects
   `t.Fatalf` over `t.Skipf` whenever a DSN for *that dialect* was configured explicitly, so
   all twelve-odd callers below get the behaviour centrally and the local
   `requireBackendReachable` helper is gone (both surviving mentions are comments recording
   that it used to exist). The per-dialect `configured` flag is the part worth keeping: the
   Multi-DB MySQL job has no PostgreSQL at all, so a single "some DSN was set" flag would have
   failed every PostgreSQL subtest there for the right reason in the wrong job. Original text:
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

4. ~~**`schema.sql` and `migrations/postgres/001_schema.sql` are two hand-maintained copies of
   one schema.** `93f8abf` resynchronised them. Nothing stops them diverging again, and the
   last divergence cost a debugging session (`generation` nullable in one, `NOT NULL DEFAULT
   0` in the other). Candidate for Phase 2: assert the two agree, or generate one.~~ ✅
   **Closed by the same commit that recorded it.** `c26c332` deleted the root `schema.sql`
   outright (see `e13c2c8`, "1.9 done") — there is no second copy left to diverge, and
   `git ls-files` finds no `schema.sql` anywhere in the tree. The caveat describes a hazard
   that the diff it was attached to had already removed.

   The residual worth keeping is a *different* pair: `engine/testutil/mssql_schema.go`
   hand-writes its tables independently of `migrations/mssql/001_schema.sql` and defines none
   of the seven security policies, so no MSSQL test has a tenant backstop. That is recorded in
   PARALLEL-WORKSTREAMS.md's third cross-stream coupling (that file was retired
   2026-09-04; the couplings are in WORKSTREAM.md) and belongs to WS-2.

**Process note for future sessions.** Two commits had to be rewound because `git add -A` was
run while subagents were mid-edit; one nearly shipped a call site an agent had *deliberately*
broken to prove a test bites. Use explicit paths, and run `git show --stat` before every
commit. A commit message asserting "docs only" over a diff full of functional code is the
same defect class this plan exists to fix.

## Phase 1 — Paired test + fix, by severity

For each item: **write the failing test first, watch it fail, then fix.** A passing unit test
is not evidence here; that is precisely how these survived.

### 1.1 Unfenced terminal side effects — data loss — ✅ **FIXED** (heading marker added 2026-09-01)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 1.2 Systemic unchecked `RowsAffected` — ✅ **FIXED** (heading marker added 2026-09-01)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 1.3 Cancellation is dead end-to-end — ✅ **FIXED**, and this section was stale

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 1.4 Crash-recovery: write-ahead intent — ✅ **FIXED** (heading corrected 2026-09-01)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 1.6 Generation not bumped on reap or terminate — ✅ **FIXED** (marker added 2026-08-06)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 1.8 MySQL never worked — fixed in `9fc2a81`

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 1.9 The shipped schema was not the tested schema — fixed in `e13c2c8`

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 1.11 No worker could start against PostgreSQL — fixed in `HEAD`

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 1.12 Two CI workflows had never run — fixed in `HEAD`

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 1.13 Multi-DB CI was green without ever connecting to PostgreSQL — fixed in `HEAD`

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 1.10 RLS was bypassed in every shipped configuration — fixed in `HEAD`

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

## Phase 2 — The seam test suite

This is the part that prevents recurrence, and the highest-value work in the plan.
~4–8 sessions total. Each is a CI job.

| # | Test | Catches |
|---|---|---|
| 2.1 | **Golden path.** Clean container, no repo knowledge, execute the README verbatim. | README drift, flag-order bugs, undocumented schema bootstrap, missing `--api-addr`, wrong endpoints — all 8 golden-path failures found today |
| 2.2 | **Two-worker race.** Real Postgres. Claim, stall worker A (SIGSTOP), let the reaper fire, let B claim, resume A. Assert event history intact, no duplicate side effects, no stale parent result. | 1.1, 1.2, 1.6 |
| 2.3 | **Cancellation e2e.** 🔶 **Partly done** — `engine/cancellation_e2e_test.go` covers both *pre-call* paths (`freshCall` and the guest `h.PollCancellation()`) against a real Postgres and a real WASM module, with controls, each proven to fail. **Still open: cancelling an already-in-flight call** — the heartbeat path at `engine/heartbeats.go:58`, which is the "stops within N seconds" half. See §1.3. | 1.3 |
| 2.4 | **Crash recovery.** ✅ **Done** — `tests/crash`. A real `cleat-worker` subprocess on the shipped two-DSN configuration, SIGKILLed with a call in flight, against a real PostgreSQL with the external service counting its own invocations. Three calls, so the counts discriminate: `1/1/2` (documented at-least-once) vs `2/2/2` (nothing durable). Measured both ways — see §1.4. Includes a clean-run control and a no-crash durability test. | 1.4 |
| 2.5 | **Resource exhaustion.** ✅ **Done** — `tests/exhaustion`, wired into the cluster job. See §2.29. Per-backend coverage is still wasmtime-only, matching what deployments run. | 1.5 |
| 2.6 | **Tenant isolation.** Two tenants; assert A cannot read, list, cancel, or admin-act on B's workflows through the HTTP API. Run against all three backends. | 1.7 |
| 2.7 | **Deploy manifests.** Flag contract ✅ **done**, see §2.27. Actually *starting* them and asserting the worker reaches ready still needs a cluster — open. | `--namespace`/`--tenant-id` crash-loop — confirmed in `k8s/` and `charts/cleat/`, **not** in `docker-compose.cluster.yml` |
| 2.8 | **Dead-code detector.** ✅ **Done** — `scripts/check-test-only-code.sh`. See below; it was indeed the highest-signal cheap check. | 1.4 class — the single highest-signal cheap check |
| 2.9 | **Doc/code consistency.** Assert `ABI.md` version == `wasm/metadata.go:47 CurrentABIVersion`; documented worker flags exist in the binary; documented buffer sizes match `engine/memory.go:39`. | ABI.md claiming v4/5 while code ships v1; the 65536-vs-1048576 buffer mismatch |

Note on 2.2–2.6: these need real databases and process control, so they belong in a
nightly/pre-merge job, not the fast unit lane. Accept the runtime. They are the only tests
that would have caught anything found today.

### 2.10 `TestIntegrationWorkflowMaxDuration` never tested the duration limit — FIXED

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 2.13 Empty-string payloads were refused across the rest of the ABI — FIXED

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 2.14 `cleat_json_parse` / `cleat_json_stringify` panicked on the primary backend — FIXED

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 2.16 Most wasmtime closure tests cannot see a handler defect — FIXED

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 2.17 `ShardedStore` claims `limit` from *every* shard and strands the excess — ✅ **FIXED**

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 2.18 Six wasmtime host functions fetch the guest memory and throw it away — ✅ **FIXED (and it was 18, not 6)**

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 2.19 `WorkflowID` / `RunID` decode the wrong half of the result word — ✅ **FIXED**

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 2.20 Child-workflow spawning inserts an event with no `tenant_id` — ✅ **CONFIRMED and FIXED**

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 2.21 `applyPostgresSchemaFile` races itself, and its doc comment says it cannot — ✅ **FIXED**

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 2.22 `flushCallIntent` omits `tenant_id` too — ✅ **FIXED** (latent, no production caller)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 2.23 `StartChildWorkflowInSchema` — same omission, but a one-line fix would be a false fix — ✅ **FIXED**, and ⬛ **SUPERSEDED 2026-09-02: the feature was removed (§3.78)**

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 2.24 The wasmtime epoch ticker races `Close` — ✅ **FIXED**

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 2.25 Nothing prevents a red PR from merging into `develop` — ✅ **FIXED** 2026-08-04

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 2.50 Parent close policy fails silently on all three dialects — ✅ **FIXED**

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 2.27 Two of the three deployment manifests crash-loop on an undefined flag — ✅ **FIXED**

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 2.29 Resource exhaustion, end to end against the shipped image — ✅ **DONE**

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 2.30 The event checksum chain is rebuilt from scratch on every write — ✅ **FIXED**

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 2.31 `tests/integrity` had never run — ✅ **DONE**

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 2.32 The checksum covers `payload`; every SQL consumer reads the shadow columns — ✅ **FIXED**

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 2.33 `tests/upgrade` had never run either — ✅ **DONE**

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 2.34 `tests/scale` — ✅ **DONE**

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 2.36 `tests/cluster` — ✅ **DONE**

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 2.37 `tests/cross-language` — ✅ **DONE**, and it passed as written

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 2.38 `tests/soak` tested `math/rand` — ✅ **DONE**

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 2.39 The DB-backed suites cannot share a database concurrently — ✅ **FIXED**

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 2.41 `cleat dev --watch` rebuilt itself forever — ✅ **FIXED**

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 2.42 The AssemblyScript determinism checks had never run — ✅ **FIXED**

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 2.43 `cleat vet --lang as` cannot fail — ✅ **FIXED** (WS-3, 2026-08-04)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 2.60 Per-step event flush never ran on MySQL or SQL Server — ✅ **FIXED** (WS-2, 2026-08-04)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 2.70 Multi-DB CI ran entirely on wazero — ✅ **FIXED** (WS-3, 2026-08-04)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 2.72 Two languages ran on wasmtime because a parser was broken — ✅ **FIXED** (WS-3, 2026-08-04)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 2.73 Plugin-harness CI ran on wazero too, and a skip was hiding the cost — ✅ **FIXED** (WS-3, 2026-08-04)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

## Phase 3 items — round 2 (2026-08-05)

### 3.10 Idempotency keys are global across tenants — ✅ **FIXED** (WS-1, 2026-08-05)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 3.11 Four unscoped queries — ✅ **FIXED** (WS-1, 2026-08-05), and it was three dialects, not one

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 3.18 SQL Server rejects the JSON the other two dialects require — ✅ **FIXED** (WS-1, 2026-08-05), floor raised to 2022

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 3.19 `CreateUpdateRequest` was §2.60c's defect, one table over — ✅ **FIXED** (WS-1, 2026-08-05)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 3.17 Completing a workflow wrote JSON `null` into `query_state`, and SQL Server refused it — ✅ **FIXED** (WS-1, 2026-08-05)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 3.16 `CreateSchedule` could not create a schedule on SQL Server — ✅ **FIXED** (WS-1, 2026-08-05)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 3.15 Signal authorization consults a list nothing can write — 🟢 **THE WRITER EXISTS** (WS-1, 2026-09-02); the default stays off, for a different reason

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 3.78 Cross-schema child workflows, removed — ✅ **DONE** (WS-1, 2026-09-02, D8)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 3.77 Names are per-tenant — D7, and it is three tables rather than one — ✅ **DONE 2026-09-03; all three tables**

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 3.86 SQL Server's cross-tenant exemption is per-connection, and the statements that lean on it have no predicate of their own — 🟢 **31 STATEMENTS FIXED across schedules, tags, definitions, the control plane and the claim path (§3.91); the gate is in place and the remaining 27 are an allowlist with reasons, not a backlog** (WS-1, 2026-09-03)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 3.12 One tenant's deploy silently replaces another's workflow code — 🔵 **OVERWRITE CLOSED; THE NAMESPACE DECISION IS MADE** (WS-1, 2026-08-05; D7 2026-09-02)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 3.13 No cleat-worker can bootstrap a MySQL schema — ✅ **FIXED** (WS-1, 2026-08-05)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 3.30 What wazero is for — ✅ **DECIDED 2026-09-01: it stays, scoped to CLI and dev tooling**

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 3.31 The execution-limit story, per backend — ✅ **WRITTEN** (WS-3, 2026-08-05; closed 2026-09-01)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 3.32 Every deferred callback runs on wazero, unfenced — ✅ **FIXED** (WS-3, 2026-08-05)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 3.35 What `defer` is supposed to be — 🟢 **ALL FIVE PHASES DONE: every terminal transition is resolved, two by building and one by decision (D10)** (WS-3, 2026-08-05; phases 2–4 landed 2026-09-02; phase 5 closed 2026-09-04)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 3.34 A concurrency key's TTL means three different things — ✅ **FIXED** (WS-1, 2026-08-05; found by WS-3)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 3.39 Re-acquiring a concurrency key you already hold answers differently per dialect — ✅ **FIXED** (2026-08-31)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 3.40 The crash harness migrated a database it never reads — ✅ **FIXED** (2026-08-31)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 3.41 Status-marker audit — ✅ **DONE** (2026-08-31)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 3.42 The four disabled linters, re-measured — ✅ **MEASURED, none enabled** (2026-08-31)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 3.43 Post-commit cleanup dropped its errors at 38 of 40 calls — ✅ **FIXED** (2026-08-31)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 3.44 Child-workflow checksums were chained off an RLS-blocked read — ✅ **FIXED** (2026-08-31)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 3.45 A guest-supplied string chose the execution runtime — ✅ **FIXED** (2026-09-01)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 3.46 A dropped Unmarshal turned "unreadable" into "you declared nothing" — ✅ **FIXED** (2026-09-01)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 3.47 Audit: every Postgres raw-pool read against an RLS table — ✅ **AUDITED + 2 FIXED** (2026-09-01)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 3.48 `assert_tenant_set` missed the empty string — ✅ **FIXED** (2026-09-01)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 3.49 A fault that never reached the database reported itself as active — ✅ **FIXED** (2026-09-01)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 3.50 SQL Server's `plugin_deps` has never round-tripped — ✅ **FIXED** (2026-09-01)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 3.51 The one JSON column SQL Server did not validate — ✅ **FIXED** (2026-09-01)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 3.53 JSON-column parity is now a checked invariant, not a sweep — ✅ **FIXED** (2026-09-01)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 3.52 InitModule discarded the error it had a channel for — ✅ **FIXED** (2026-09-01)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 3.37 SQL Server has no administrative access under RLS — ✅ **FIXED** (WS-1, 2026-08-06)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 3.33 gosec's 283 findings, triaged — 🟢 **ENABLED 2026-09-04; 3 fixed, G115 + G306 excluded, 17 //nolint'd** (WS-3, 2026-08-05)

`PARALLEL-WORKSTREAMS.md` — retired 2026-09-04, quoted here from history — calls gosec
"unreviewed security findings in a codebase whose last two days have been tenancy defects" and says an unreviewed 283 is worse than a reviewed 283
with 280 suppressions. This is the review. It does **not** enable the linter — G115 alone
would block that — but it replaces a number with a distribution, which is what the decision
needs.

| rule | n | what it is | verdict |
|---|---|---|---|
| G115 | 229 | integer overflow conversion (`int` → `uint32` etc.) | **81% of the total.** Mostly flag and length conversions. Needs its own pass; see below |
| G306 | 23 | `WriteFile` perms > 0600 | build outputs — `.wasm` artifacts and lockfiles at 0644. Not secrets |
| G202 | 8 | SQL string concatenation | all in `engine/testutil/schema.go`, concatenating **constant** table names and placeholder strings in a test helper |
| G204 | 6 | subprocess with variable args | `docker`, `vault`, `aws`, `npx` invoked with operator-configured paths, not request data |
| G404 | 2 | weak RNG | jitter in `fault_injector.go` and sampling in `plugin/audit.go`. Neither is a security decision |
| G101 | 2 | hardcoded credentials | a test DSN and a test role password constant |
| G602 | 1 | slice bounds out of range | `wasmBytes[0:8]` in `RewriteWitImports`, which returns an error on `len < 8` in its first statement. gosec cannot see the guard |
| G201 | 1 | SQL string formatting | `StartChildWorkflowInSchema` interpolates a schema name — but through `pq.QuoteIdentifier`, which is the correct tool, since an identifier cannot be a bind parameter |
| G108 | 1 | pprof endpoint exposed | see below |
| G114 / G112 | 2 | HTTP server without timeouts | **the only two actionable findings.** Fixed |

**The two that were real**, both slowloris exposure rather than anything exotic:
`cmd/cleat-worker`'s pprof listener used `http.ListenAndServe`, which cannot set a timeout at
all, and `cmd/cleat run`'s inspection server built an `http.Server` without
`ReadHeaderTimeout`. Both now set one.

**On G108, which reads worse than it is.** `cmd/cleat-worker` blank-imports `net/http/pprof`,
which registers `/debug/pprof` on `DefaultServeMux`. That is deliberate and it is *not* reachable
on the API port: the API server is constructed with its own mux, and pprof is served only when
`--pprof-addr` is set, which is empty by default. Worth keeping that way and now commented at
the site, because a heap profile from a worker contains workflow payloads — the separation is
load-bearing, not stylistic.

**G115 is the real backlog item, and it is not obviously noise.** 229 conversions that could
truncate. Most are flags (`uint32(*wasmOutputBufferSize)`) where a hostile value is already an
operator problem, but the class includes every `int` → `uint32` in the ABI layer, where a
truncated length is a memory-safety-adjacent bug rather than a style point. Reviewing it is a
session on its own and should not be folded into a lint sweep — but "229 integer conversions in
the WASM boundary layer, unreviewed" is a more useful thing to carry forward than "283 gosec
findings".

**One slice of G115 now has a mechanism instead of a sweep, 2026-09-04 (WS-3).** CLAUDE.md's
ruling on this backlog is that the defects here have never been overflows — "in every case the
value meant the wrong thing on one side of the boundary, which a property test over that
boundary would find faster than reading the remaining sites". The component bridge is the
boundary where that is cheapest to check and worst to get wrong.

A host call's result word carries the response length, and the two layouts disagree about where:
`packDurableCallResult` at bits 40-63, `packSimpleResult` at 32-63. `component_callbacks.go` has
one extractor per layout and **25 dispatchers each pick one by hand**. Pick wrong and nothing
errors — a bit-32 length read at bit 40 is zero for any response under 256 bytes, so the guest
receives an empty *successful* response. That shipped once, for one of the 25;
`TestComponentShortStringResultsAreNotTruncated` is its regression test.

`engine/component_pack_extract_parity_test.go` covers the other 24 and every one added later. It
resolves 23 of the 25 pairings by following delegation through the AST, and **found no mismatch**
— its value is the next one, not a live bug.

**Both sides are measured rather than declared**, which is the part worth copying. A table saying
"`packSimpleResult` means 32" would be a third copy of the thing under test and would agree with
a shift that had changed underneath it — the §1.1 trap. Instead the test packs a distinctive
length and finds where it landed, and hands each extractor words built at each candidate shift to
see which it honours. Nothing in the test states a shift.

It fails rather than passing quietly when it stops measuring: fewer than 20 dispatchers parsed,
fewer than 15 pairings compared, both extractors reading the same bit, or any handler it cannot
resolve. `PollCancellation` and `PollSignal` build their word inline instead of calling a packer
and are listed as named exceptions with the reason, because an unresolvable site is exactly where
the next mispairing would hide.

Proven able to fail: mispairing `dispatchDurableDefer` reports "extracts the length from bit [40],
but its handler DurableDefer writes it at bit 32".


#### ENABLED, 2026-09-04 (WS-3)

The triage above ends "it does **not** enable the linter". It does now. `gosec` moved from
`disable` to `enable` in `.golangci.yml`, and the tree is green across all seven first-party
modules with **zero** findings.

**Re-measured first, because the 2026-08-05 table was not re-derivable** — golangci-lint was
not installed when it was written. On v1.64.7, the version `lint-go` pins:

| | 2026-08-05 | 2026-08-31 | **2026-09-04** |
|---|---|---|---|
| total | 283 | 693 | **671** |
| production (non-`_test.go`) | — | 272 | **253** |
| production excluding G115 | — | 39 | **40** |

    GOTOOLCHAIN=go1.25.11 golangci-lint run --timeout=15m -c gosec-only.yml ./... \
      > out.txt 2> err.txt
    grep -c 'level=error' err.txt      # MUST be 0 -- see the toolchain trap below
    grep -c '(gosec)' out.txt

**The toolchain is part of the command.** golangci-lint v1.64.7 cannot read export data from
Go 1.27 and exits 3 having found **nothing**: `internal error in importing "internal/goarch"
… export data version 4 is greater than maximum supported version 2`. That is a tidy zero
that looks exactly like a clean tree. `GOTOOLCHAIN=go1.25.0` fails differently and just as
quietly (`go.work requires go >= 1.25.11`). Both were hit writing this. Check stderr for
`level=error`, never the count alone — the same rule `.golangci.yml` already records for the
`--disable-all` trap that produced "a tidy table of four zeroes".

**What it took to reach zero**, and the reasoning lives in `.golangci.yml` beside each:

* **G115 excluded** (213 of the 253). Not a deferral — a decision that predates this work.
  CLAUDE.md rules that these have never been overflows, and #485 landed the property tests
  that cover the boundary properly. Reading 213 conversion sites is the sweep that ruling
  exists to prevent.
* **G306 excluded** (23), scoped to gosec rather than re-adding a global test exclusion,
  which is the move `.golangci.yml` explicitly asks for. 0644 on `cleat init` scaffolding,
  generated code and build outputs is intended; 0600 would be wrong, not safer. **This is the
  one exclusion taken on breadth rather than on having read every site**, and its cost is
  recorded there: a future credential written to disk would not be flagged.
* **Test files excluded, for gosec only** (418 of 671, 140 of them surviving the two rules
  above). What gosec finds in `_test.go` here is test DSNs, harnesses shelling out to
  docker/cargo/npx, and 0644 fixtures — all properties of being a test. The cost, also
  recorded: a real credential committed in a test file has nothing else catching it.
* **17 `//nolint:gosec` at the site with the reason**, matching how ineffassign, gosimple and
  staticcheck were handled. All 17 were read, not pattern-matched: G202 concatenates only
  compile-time constants (`statusTerminating`, `deferPhaseOwedSQL`, `sqlPlaceholders`, and two
  in-package literal table lists in `testutil/schema.go`); G204 uses a fixed binary with array
  args and no shell; G602 is guarded by a `len < 8` early return in the same function; G108 is
  the pprof separation this section already documents.

**One new finding, and it was real** — `examples/widget-store-as/host/main.go` built an
`http.Server` with no `ReadHeaderTimeout` (G112). Fixed rather than suppressed. Note what that
says about the 2026-08-05 table calling G112/G114 "the only two actionable findings … Fixed":
that pass measured the root module, and this one was in `examples/`, a separate module that
`lint-go` also covers. **A count is scoped to what was walked**, and the earlier row did not
say what it had walked.

**Negative control, because a green from a linter is the easiest false green in this repo.**
A file with a deliberate `rand.Intn` was dropped into `engine/` and the run went red on it:

    engine/zz_gosec_probe.go:9:42: G404: Use of weak random number generator … (gosec)

so the zero above is gosec running and finding nothing, not gosec not running. Removed after.

**Still open:** G115 is not fixed, it is ruled out of scope, and this section's earlier
paragraph on it stands — "229 integer conversions in the WASM boundary layer, unreviewed" is
still the honest description of what excluding it means, at 213.

### 3.20 `AdminForceComplete` / `AdminForceFail` were stubs — ✅ **FIXED** (WS-2, 2026-08-05, #297)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 3.14 `examples/dag` is red on `develop`, and no CI job runs it — ✅ **FIXED**

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 3.22 An ambiguous call is erased, not reported — ✅ **FIXED** (WS-2, 2026-08-05)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

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
- Head-to-head numbers still do not exist. `benchmarks/comparative/` was removed from this
  repo in favour of [cleat-bench](https://github.com/cleat-team/cleat-bench), which already
  has the runners, seven workload specs and the AWS infrastructure — **run them there.**
  Real head-to-head numbers would be a genuine asset.

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

---

### 3.23 A guest that returned an error is reported as a "wasm trap" — ✅ **FIXED** (2026-08-31)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 3.24 An ambiguous outcome is classified `unknown` — ✅ **FIXED** (2026-08-31)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 3.54 Every released `cleat-worker` binary was dead on arrival — ✅ **FIXED** (2026-09-01)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 3.55 Durable promises could not link on the worker — ✅ **FIXED** (2026-09-01)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 3.56 The host ABI is written twice, and now something checks it — ✅ **FIXED** (2026-09-01)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 3.57 macOS gets a working `cleat-worker` back, via Homebrew — ✅ **FIXED** (2026-09-01)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 3.59 Durable promises: linking was tested, meaning was not — ✅ **FIXED** (2026-09-01)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 3.58 The release path was only ever exercised by a release — ✅ **FIXED** (2026-09-01)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 3.60 §3.52's fix left a 50/50 race that discarded the error it connected — ✅ **FIXED** (2026-09-01)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 3.61 The output-buffer ABI, as a property rather than 31 tests — ✅ **FIXED** (2026-09-01)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 3.62 `cleat_poll_work` wrote to guest pointers it never checked — ✅ **FIXED** (2026-09-01)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 3.63 A cleanup pass was bounded per defer, not in total — ✅ **FIXED** (2026-09-01)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 3.64 A defer body ran twice after a trap — ✅ **FIXED** (2026-09-01)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 3.65 The component decomposition path, deleted — ✅ **DONE** (2026-09-01)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 3.66 A defer registered before the workflow suspended never ran — ✅ **FIXED** (WS-3, 2026-09-01)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 3.67 A `cleat_sleep` at the replay frontier never resumes — ✅ **FIXED** (WS-3, 2026-09-01)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 3.68 Replay released a virtual-object scope the workflow had already cleared — ✅ **FIXED** (WS-3, 2026-09-01)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 3.69 A third instance of the replay/fresh state class, found by a property test — ✅ **FIXED** (WS-3, 2026-09-01)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 3.71 A workflow killed by the memory limit was recorded as having succeeded — ✅ **FIXED** (WS-3, 2026-09-02)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 3.72 The engine suite poisoned its own database — ✅ **FIXED** (WS-3, 2026-09-02)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 3.73 Four SDKs document a `defer` that runs cleanup, and cannot run it — ✅ **ALL FOUR DONE** (WS-3, 2026-09-02)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 3.74 Java workflows could not suspend — ✅ **FIXED** (WS-3, 2026-09-02)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 3.75 The durable record for a resumable defer phase — 🟢 **DONE 2026-09-04: two transitions built (§3.112, §3.114), the third declined (D10)** (WS-2, 2026-09-02)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 3.76 MySQL's TerminateWorkflow released nothing — ✅ **FIXED** (WS-3, 2026-09-02)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 3.80 A closed parent's children keep their concurrency slots — ✅ **FIXED** (WS-2, 2026-09-02)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 3.79 `TerminateWorkflow` does not enforce the parent close policy — ✅ **FIXED** (WS-2, 2026-09-02)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 3.81 The defer segment — 🟢 **MECHANISM BUILT, MEASURED, AND NOW USED BY EVERY TERMINAL TRANSITION THAT TAKES ONE** (WS-3, 2026-09-02; closed 2026-09-04)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 3.83 The sentinel §3.81 specified would collide with a real response — 🟢 **THE REMAINDER IS DONE: all four SDKs and all four call paths landed by 2026-09-04** (WS-3, 2026-09-02)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 3.84 A defer segment is stopped on `cleat_call` only; four other paths still start new work — ✅ **FIXED** (WS-3, 2026-09-02)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 3.85 `--max-quota-events` killed the worker process, and the cap never counted the workflow — ✅ **FIXED** (WS-3, 2026-09-02)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 3.87 The Rust SDK cannot suspend: `catch_unwind` never catches, and the host has been masking the trap — ✅ **FIXED** (WS-3, 2026-09-03)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 3.88 §3.75's two pre-build re-derivations: the inventory is clean, the dead-letter question changed — ✅ **steps 1 and 2 DONE; step 3 (§3.75) DONE 2026-09-04 — §3.112 and §3.114 built two of the three transitions this section's inventory named, and D10 declined the third** (WS-3, 2026-09-03)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 3.89 Resolving an ambiguous call broke the checksum chain above it — ✅ **FIXED** (WS-2, 2026-09-02)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 3.113 The Python SDK discards the host's result on fire-and-forget calls, so a refusal is reported as success — 🟢 **FIXED 2026-09-04 by §3.201**; the finding was real, most of its evidence table was not (WS-1, 2026-09-04)

Filed 11:41, fixed 13:02 the same day — `15f83ca` to `e4de0a4`, 81 minutes. The marker stayed 🔴
after that and was the **only** 🔴 left in the plan, so a scan for open work reported an
already-closed item as the project's top outstanding defect. That is §1.1's failure mode with the
sign flipped: not a ✅ over a stale body but a 🔴 over a fixed one, and it costs the same session.

**The defect was real.** `execSession.SignalWorkflow` (`engine/signaller.go:266`) returns
`errSignalAuthRequiredInt` when `signalAuthCheck` refuses the send; the SDK returned `None` on the
refusal and `None` on success. WS-2 took it as **§3.201**, which is wider than this entry measured
— it found `SetState` and `DeleteState` discarding a non-determinism report, the worse half — and
closed it with one helper rather than thirteen edits.

**The close condition, re-verified 2026-09-04 against `develop` at `699c010`.**
`_check_host_result` (`python-sdk/cleat_sdk/host_calls.py:437`) masks bit 31 first, then reads the
low-byte error code; twelve call sites pass their result to it. `errSignalAuthRequiredInt` is
`-4294967294` = `0xFFFFFFFF_00000002`, so the mask misses and `err_code` is 2 — a refused signal
now raises. Falsified by deleting the `_raise_if_stopped(r)` line from that helper: three tests go
red, and red *the right way* — the stop is reported as `CallErrorUnavailable (code 2)` rather than
raising `SuspendSentinel`, which is the misdecode the ordering exists to prevent, not merely an
absent check.

    cd python-sdk && python3 -m pytest tests/test_host_result_binding.py -q   # 7 passed

#### What this entry got wrong, and the instrument that caused it

Its scan classified a call site by whether the result was **bound to a variable it could name**:

    binds = bool(re.search(r'\bresult\s*=|\bres\s*=|\brc\s*=', body))

Binding is not using. `return _import_cleat_schedule_cron(...)` hands the word to the caller and
`resp = _import_side_effect(...)` binds it under a name the pattern does not list; both were
reported as discards. The only shape that truly throws a result away is a bare `ast.Expr` whose
value is an `_import*` call, so run that over the same file at the same commit:

    HC=$(mktemp)   # not a fixed /tmp name: a stale one from an earlier session
                   # overwrote this very file while this entry was being written
    git show 15f83ca:python-sdk/cleat_sdk/host_calls.py > "$HC"
    python3 -c "
    import ast,pathlib,sys
    cls=[n for n in ast.parse(pathlib.Path(sys.argv[1]).read_text()).body
         if isinstance(n,ast.ClassDef) and n.name=='HostCalls'][0]
    print(sorted({f.name for f in cls.body if isinstance(f,ast.FunctionDef)
      for n in ast.walk(f) if isinstance(n,ast.Expr) and isinstance(n.value,ast.Call)
      and isinstance(n.value.func,ast.Name) and n.value.func.id.startswith('_import')}))" "$HC"

That prints the 14 methods that genuinely discarded, at the commit the table was written from.
Intersect it with the table's nine rows and **four of its eight `NO` rows are wrong**:
`side_effect`, `schedule_cron`, `send_signal_and_wait` and `acquire_lock` all used their result
when the row was written. Two of
those four name methods that **do not exist** — they are `send_signal_and_wait_ms` and
`acquire_lock_ms` — so those rows were not measured at all, by that command or any other. The four
rows that were right, `send`, `schedule_invoke`, `reply_to_signal` and `signal_workflow`, are the
finding, and they are what §3.201 fixed.

The body filter compounds it. `'_import_cleat_' in body` cannot match `_import_side_effect`, so the
command cannot produce the `side_effect` row the table shows above it. §3.201 found that half
independently while re-deriving the same scan.

**A regex over source cannot tell "the result went nowhere" from "the result went somewhere under a
name I did not guess", and that distinction is the entire finding.** The AST walk is now a
structural guard — `TestNoScalarHostCallDiscardsItsResult` in
`python-sdk/tests/test_host_result_binding.py` — so a fourteenth discarding call site fails on
arrival rather than when someone thinks to write a test for it.

#### §3.111's remaining seven are no longer blocked

This entry's operative claim was that guarding them host-side would set a bit the Python SDK does
not read. It reads it now. §3.201 also corrected the shape: **the seven are not uniform.** Four
return `u64`/`s64` — `durable-signal-workflow`, `durable-send`, `durable-acquire-lock`,
`durable-schedule-invoke` — and three return `string`: `durable-send-signal-and-wait`,
`side-effect`, `durable-schedule-cron`. For those three a `string` has nowhere to put a sentinel, so
`result<string, call-failure>` *is* the right rule and a signature change *is* the fix — §3.110's
situation, and the opposite of what this entry concluded. Split the seven before touching the rule.

That split is now the shape of what is left. Measured 2026-09-04 on `develop` at `699c010`, the
four scalar calls are guarded and the three string ones are not:

    for f in SignalWorkflow SendSignalAndWait DurableSend SideEffect AcquireLock \
             ScheduleCron DurableScheduleInvoke; do
      echo -n "$f "; sed -n "/func (s \*execSession) $f(/,/^}/p" engine/*.go \
        | grep -c callSuspendSentinel
    done
    # 19:55 -- SignalWorkflow 1  SendSignalAndWait 0  DurableSend 1  SideEffect 0
    #          AcquireLock 1  ScheduleCron 0  DurableScheduleInvoke 1
    # 20:30 -- all seven 1, after §3.300

**That reading was true when taken and false nineteen minutes later.** §3.300 (`1d70483`, 20:14)
guarded the three string-returning calls; the paragraph above was measured at about 19:55 and
merged at 20:2x, so it shipped describing a remainder that no longer existed. §3.111 is now
complete: all seven return the sentinel.

The measurement is left standing rather than rewritten, because the failure it illustrates is not
in the number. **A dated measurement stays true; a dated *remainder* does not.** "Four of seven are
guarded" is a fact about 19:55 and still is. "The remainder is one WIT change gating three calls"
was a claim about the future of a shared frontier, and a peer stream closed it while this entry was
in review. When writing about what is left on something two other streams are also working, date
the measurement and re-derive the remainder at merge, not at authoring — this file's own
stale-marker rule, applied to the sentence rather than to the heading.

The rule correction itself has landed: `TestTheThreeStopSurfacesAgree` no longer demands
`result<string, call-failure>` of a scalar-returning stop site — `witCallOutcomeFuncs` reports a
third category, and the reasoning is on the `"AcquireLock"` entry of `stopSurfaces` in
`engine/stop_correspondence_guard_test.go` (named rather than cited by line, because a line number
into a living file is a dead citation with a delay). §3.201 supplied the case this entry deferred
it for — `durable-acquire-lock`, which returns `s64`.

#### One citation to this entry is left for its owner

§3.400's **A8** row — "packed result's errCode ≡ what the guest observes" — records its gap as
"open: `extractStringFromPacked` drops it, so a refusal reaches Python as a success (WS-2,
§3.113)". Two things about that are now stale and **neither is edited here, because §3.400 is
WS-3's**. The citation points at a closed entry; and `extractStringFromPacked`
(`engine/component_cgo.go:746`) has **no production callers** — every reference to it outside its
own definition is in a `_test.go` file:

    grep -rn "extractStringFromPacked(" --include="*.go" . | grep -v _test.go   # 1 line, the func decl

The gap A8 names may well still be real by another route; what is not real is the mechanism the row
attributes it to. For WS-3 to re-derive when they next touch that table.

### 3.111 A defer segment could still call a service through `cleat_call_heartbeat` — 🟢 **FIXED 2026-09-04** (WS-1, 2026-09-04)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 3.108 The Tier 1 Gate ran on Go's 10-minute default and the engine suite outgrew it — 🟢 **FIXED 2026-09-03** (WS-1, 2026-09-03)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 3.101 Terminate and signal told the caller which workflow ids are real — 🟢 **FIXED 2026-09-03** (WS-1, 2026-09-03)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 3.99 The admin API answered 404 to its rightful owner on two of three dialects — 🟢 **FIXED 2026-09-03** (WS-1, 2026-09-03)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 3.95 `cleatctl restore-workflow` is removed — 🟢 **DECIDED AND DONE 2026-09-03; the three questions below were answered by deleting the thing that raised them** (WS-1, 2026-09-03)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 3.92 §3.86 scoped the terminate and left the cascade — 🟢 **FIXED 2026-09-03, symptom and root; found by the gate's allowlist demanding a reason, not by its scan** (WS-1, 2026-09-03)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 3.91 The ordinary claim path took every tenant's work on SQL Server — 🟢 **FIXED 2026-09-03; the `-claim-across-tenants` flag was decorative on this dialect** (WS-1, 2026-09-03)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 3.90 `--wasm-instance-timeout` is charged for time the guest spends blocked in the host — ✅ **FIXED** (WS-3, 2026-09-03)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 3.94 Execution limits are process-wide, and one of them is compiled into the guest — 🟢 **FIXED 2026-09-03: all six steps shipped** (WS-3, 2026-09-03)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 3.96 A recorded plugin stream error replayed as a success — ✅ **FIXED** (WS-2, 2026-09-03)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 3.97 `EventRecord.CreatedAt` comes back on one dialect of three — ✅ **FIXED 2026-09-03 in §3.102**, which turned out to be the smaller half of the defect (WS-2, 2026-09-03)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 3.100 A merge's own verification is cancelled by the next merge — 🟢 **FIXED 2026-09-04; the 2026-09-03 fix was half of one** (WS-1, 2026-09-03)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 3.98 The database payload carried none of four fields the replay path reads — ✅ **FIXED** (WS-2, 2026-09-03)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 3.102 Nine read paths, four different answers about the same row — ✅ **FIXED** (WS-2, 2026-09-03)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 3.103 The `EventStream` abstraction had no callers, and one of its two implementations read across tenants — ✅ **FIXED by deletion** (WS-2, 2026-09-03)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 3.104 A defer segment could still make an outbound HTTP request — ✅ **FIXED** (WS-2, 2026-09-03)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 3.105 The Java SDK could not run a defer segment, because it never decoded the stop sentinel — ✅ **FIXED, both halves; `java` is in `deferSegmentLanguages`** (WS-2, 2026-09-03)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 3.106 The AssemblyScript SDK could not run a defer segment either, and its stop cannot unwind — ✅ **FIXED, both halves; a defect found in the second one** (WS-2, 2026-09-03)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 3.107 The Rust SDK decodes the defer-segment stop sentinel — ✅ **FIXED, both halves; `rust` is in `deferSegmentLanguages`** (WS-2, 2026-09-03)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 3.109 Three tests ran in no job at all, and the tier-1 gate was building tier-2 toolchains — ✅ **FIXED** (WS-2, 2026-09-03)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 3.110 A stop was not expressible on the component ABI, and neither was a failure — ✅ **FIXED: the WIT says it in the type, and `python` is in `deferSegmentLanguages`** (WS-2, 2026-09-04)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 3.112 Terminate ran no defers, and released the locks the defers were for — 🟢 **FIXED 2026-09-04** (WS-2, 2026-09-04)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 3.114 A closing parent pre-empted every child's cleanup at once — 🟢 **FIXED 2026-09-04** (WS-2, 2026-09-04)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 3.200 A Go guest was told "error 1 (timeout)" for every plugin failure, and the host's real message was in the buffer beside it — 🟢 **FIXED 2026-09-04**; the remaining 18 adapters closed 2026-09-05 (WS-1)

Found by WS-2 while dumping every plugin key in every language for §3.306, and handed over as
ABI-adjacent. Same host, same 10 plugins, same 17 calls:

| guest | what it reported |
|---|---|
| Rust / AS / Java | `plugin function pgvector/upsert not registered. Check that...` |
| Go | `plugin_call: error 1 (0=unknown 1=timeout 2=transient ...)` |

WS-2 posed two candidates — the host wrote no response bytes, or the length failed
`callErrorMessage`'s bounds check — and deliberately did not guess between them. **It was
neither.** The host writes the text and the guest decodes the length correctly. The Go adapter
then discards it:

```go
responseLen := uint32(uint64(result) >> 40)
errCode := uint32(result & 0xFF)
if errCode != 0 {
	return "", fmt.Errorf("plugin_call: error %d (0=unknown 1=timeout ...)", errCode)
}
return unsafe.String(&responseBuf[0], int(responseLen)), nil
```

`responseLen` is computed and then unused on the error branch; `responseBuf` is never read there.
The host side is `engine/plugins.go`:

```go
written, _ := s.writeResult(ctx, m, responsePtr, errStr, responseMaxLen)
return packDurableCallResult(int(written), callFailureCode, 1)
```

**Three things were wrong at once, which is why the symptom looked like a length bug.**

1. *The message is discarded.* Nothing reads `responseBuf` on the failure path.
2. *The printed number comes from a different field than the legend describes.*
   `packDurableCallResult` is `responseLen<<40 | callErrorCode<<8 | errCode`, so `result & 0xFF`
   is `errCode` — which the host hardcodes to literal `1` on **every** failure path. Simulating
   the packer with `callErrorCode` varied over 0/2/3/5 prints `1` every time. The legend beside
   it enumerates `CallErrorCode`, which lives at bits 8–39.
3. *The real classification is discarded too.* `callFailureCode = callErrorUnavailable = 2`,
   never decoded.

So "why is it 1 for a not-registered plugin" has a flat answer: **it is 1 for everything.** Not a
timeout, not a classification — a constant.

**This is a mechanism, not a bug, and the scope is the finding.** 20 of the 23 adapters in
`wasm/adapter_metadata.go` print that legend; three call `callErrorMessage`. Those three —
`DurableCall`, `DurableCallWithRetry`, `DurableCallWithHeartbeat` — are exactly the calls named in
§2.10. **The fix was applied to the report's examples and never generalised.**

The set that legend can *ever* be right for is decidable, because only one packer carries a
`CallErrorCode`:

    grep -rn 'packDurableCallResult(' --include='*.go' engine/ | grep -v _test.go

reaches `durablecalls.go`, `heartbeats.go` and `plugins.go` — five adapters. The three above, plus
`PluginCall` and `PluginCallStreaming`. **Those two are this fix.** Both now decode
`callErrorCode` from bits 8–39 and pass the buffer to `callErrorMessage`, which is what the other
three have done since §2.10.

**Still open: the other 13.** `packSimpleResult`, `packAwaitChildResult`, `packAwaitPromiseResult`,
`packAwaitSignalsResult` and `packAcquireLockResult` each carry an `errCode` and **no
`callErrorCode` field at all** — so `DurableAwaitSignals`, `DurableDefer`, `DurableDeferFunc`,
`PollSignal`, `ChildWorkflow`, `ChildWorkflowWithOptions`, `AwaitChild`, `AwaitAllChildren`,
`PollChild`, `AwaitAnyChild`, `CreatePromise`, `AwaitPromise` and `SideEffect` print a legend for
a field that does not exist. That is a different defect with a different fix — `hostErrMessage`,
or no legend — and it is not taken here. `wasm/generator.go` already says so in
`hostErrMessage`'s doc comment, which warns that printing the `CallErrorCode` legend beside a
simple-result code "would describe a rejected cron expression as a timeout". **That comment
describes the live defect in thirteen other calls.** Five more adapters —
`ContinueAsNew`, `ContinueAsNewWithVersion`, `AcquireLock`, `AcquireLockMs`, `ReleaseLock` — print
the legend with no output buffer at all, so they have nothing better to print and need the legend
removed rather than replaced.

## The other 18, closed 2026-09-05

The open half above is done. Every adapter now either reads the host's message or reports a bare
code; no adapter prints the `CallErrorCode` legend inline.

**The classification had to be done per call, and two of my assumptions above were wrong.**

*First*, the split was 13-with-buffer and 5-without, but the boundary that matters is not "does the
legend apply" — it is "did the host write something to read". Checked against the host rather than
inferred: `AwaitChild`'s replay path is `writeResult(ctx, m, resultPtr, rec.Err, resultMaxLen)`
then `packAwaitChildResult(written, 1)`; `SideEffect`'s is an `errMsg` then
`packSimpleResult(1, written)`; `AwaitPromise`'s is `rec.PromiseError` then
`packAwaitPromiseResult(written, false, 1)`. **In each case the reason is in the buffer and the
guest was returning before reading it** — the same defect as `PluginCall`, on a different packer.

*Second*, I expected several of these error branches to be dead. `CreatePromise`'s handler returns
`packSimpleResult(0, written)` on every path, so from the handler alone `errCode` is never
non-zero. **That reasoning stops one layer too early.** `engine/imports.go` returns `errBadParam`
= `0xFFFFFFFF_00000001` from **64 sites** when it cannot read a guest string, before the handler
runs at all. Its low byte is 1, so the branch is reachable for every one of these calls — and
every such failure printed "error 1", which the legend reads as a **timeout** rather than a bad
parameter.

`hostErrMessage` is safe on exactly those paths, and not by accident: it bounds-checks the length
against the buffer, so `errBadParam`'s `0xFFFFFFFF` decodes to a length no buffer satisfies and it
returns "no detail reported by the host" instead of reading out of range.

**The sharpest single case is `AwaitPromise`.** `packAwaitPromiseResult(written, false, 1)` is a
*rejected promise* — an ordinary application outcome, carrying `rec.PromiseError`. A Go guest was
told `error 1 (1=timeout)`. The rejection reason was in the buffer the whole time.

The five with no output buffer — `ContinueAsNew`, `ContinueAsNewWithVersion`, `AcquireLock`,
`AcquireLockMs`, `ReleaseLock` — have nothing to read, so they report the bare code with the
legend removed rather than a legend for an enum they do not carry.

`ScheduleCron` and `ListCrons` already used `hostErrMessage`, which is what makes this the house
pattern rather than a new one — and `hostErrMessage`'s doc comment had described the defect in the
other 18 since it was written.

Two guards, both falsified by restoring the legend on `AwaitChild` alone, which reddens both while
the other 17 stay green: `TestNoAdapterPrintsTheCallErrorCodeLegendForAnotherLayout` and
`TestAdaptersWithAnOutputBufferReportWhatTheHostWroteThere`.

**One thing this did not get: nothing in `./wasm/` compiles generated code.** A wrong buffer name
would pass every test in that package. Verified instead by checking that each buffer and length
identifier used in an error branch is also used elsewhere in the same adapter, where it compiles
today — 15 of 15, zero mismatches. A test that builds a workflow exercising every host call would
be strictly better and does not exist; `examples/dag` does not currently build, for an unrelated
reason (the HostCalls threading verifier rejects four of its functions).

**Falsification.** Reverting `adapter_metadata.go` and keeping the test reddens
`TestDurableCallAdaptersReportTheHostsMessageNotJustACode` on `PluginCall` and
`PluginCallStreaming` — both assertions, both adapters, naming the discarded message — and
`TestPluginCallDecodesCallErrorCodeFromTheRightBits` on the shift. **`DurableCall`,
`DurableCallWithRetry` and `DurableCallWithHeartbeat` stay green in the same run**, which is the
negative control: the test discriminates the two broken adapters from the three correct ones
rather than merely firing.

**Why the existing guard did not catch it.** `TestHostAdapterReportsCallErrorCodeNotErrCode`
(§2.10) pins exactly this property — its doc comment describes a call that "reported Code 4
(invalid request) and then said 'error 1', which the legend reads as a *timeout*". Its assertion
is a substring match on `callErrorMessage("cleat_call", ...)`. **The comment states the general
rule and the assertion names one call**, so it stayed green while two other adapters on the same
layout carried the same defect. This is CLAUDE.md's "a test whose NAME asserts the mechanism"
in its other form: here the *comment* asserted the mechanism and the test checked an instance.

**Follow-up, same day — the fix prefixed what `callErrorMessage` already names.** Wrapping its
result in `fmt.Errorf("plugin_call: %s", ...)` doubles the call name on the fallback path
(`plugin_call: plugin_call: error 2 (...)`) and, on the success path, prepends a name the other
guests do not print. The second half is the one that matters: this section exists to make a Go
guest report what Rust, AS and Java report, and `plugin_call: blobstore: no tenant context`
against their `blobstore: no tenant context` is still a divergence — a smaller one than
`error 1`, but the same kind. Measured by WS-2 on the harness: `llm.chat_stream` read
`plugin_call_streaming: plugin_call_streaming: no plugin stream registry configured`. Fixed by
returning `callErrorMessage`'s result verbatim; the fallback keeps the call name because
`callErrorMessage` puts it there itself, which is exactly why the wrapper must not. Pinned by
`TestPluginAdaptersDoNotPrefixWhatCallErrorMessageAlreadyNames`. **This decides the question for
the remaining 13 too** — whatever `hostErrMessage` does about prefixing will do it for all of
them at once, so settling it here is cheaper than unpicking it later.

CLAUDE.md records that all four prior defects at this boundary were "the value meant the wrong
thing on one side of the boundary", and none was an overflow. This is a fifth, and it is that
exactly — twice over: a length that was read and dropped, and a code read from the wrong field.

### 3.203 §2.26's last two files were deferred pending §2.60, which landed a month earlier — 🟢 **FIXED 2026-09-05** (WS-1, 2026-09-05)

§2.26 wrapped the MSSQL store's transaction boundaries in `withRollbackGuaranteedRetry` one file
at a time, and its last paragraph reads:

> **Still to do:** `mssql_events.go` and `mssql_signals_promises.go` (9 boundaries), which
> §2.60 (#283) is changing. Do those after it lands rather than into a conflict.

**§2.60 landed as #283 at 2026-08-04T22:23 and the deferral was never lifted.** The instruction
was correct when written — the paragraph is dated 17:27 the same day, five hours before the thing
it was waiting for. It then outlived its own precondition by a month. Same shape as §3.113: a
marker that was accurate when filed and stopped being accurate without anyone editing it.

Three boundaries wrapped: `AppendEventHistoryBatch`, `SetAllowedSignalCallers` and
`PollAndClaimSignal`, each split into a `…Once` body the way `CompactHistory` and
`DeleteExpiredEvents` already were. `PollAndClaimSignal` returns `(string, bool, error)` and the
wrapper takes `func() error`, so its results are captured in the closure.

**Retrying `PollAndClaimSignal` cannot claim a signal twice**, and the reason is the wrapper's
whole design rather than anything about this call: `withRollbackGuaranteedRetry` gates on
`isMSSQLRollbackGuaranteed` — deadlock victim (1205), snapshot conflicts (3960, 41301–41325) —
where SQL Server has definitively undone the transaction, so the claiming `DELETE` did not happen.
A double claim needs the commit to have *succeeded*, which is the unknown-outcome case that
wrapper excludes by construction and `mssqlRetry` does not. **Do not substitute `mssqlRetry` here.**

## The "9 boundaries" was counting something else, and it reproduces exactly

Not recorded as "9 was wrong", because it was not — it answered a different question than the
sentence around it asks. Measured at `f0074d46`, the commit that wrote the line:

| file, at `f0074d46` | `BeginTx` | `s.db.ExecContext` |
|---|---|---|
| `mssql_signals_promises.go` | 1 | 8 |
| `mssql_events.go` | 1 | 0 |

**1 + 8 = 9 is `mssql_signals_promises.go` alone**, counting every DB write site rather than every
transaction boundary. The sentence attributes the 9 to both files, so on its own terms the figure
should have been 10.

The units matter more than the total. **No bare `s.db.ExecContext` is wrapped anywhere in the
MSSQL store**, including in the files §2.26 declared done — `mssql_deployment.go` still has 7 and
`mssql_schedules.go` 8. Single-statement autocommit calls were consistently out of scope for every
increment, so reading the 9 as work-remaining would reopen every finished file. Re-derive with:

    for f in engine/mssql_*.go; do case "$f" in *_test.go) continue;; esac
      printf "%-34s BeginTx=%s dbExec=%s wrapped=%s\n" "$(basename $f)" \
        "$(grep -c BeginTx $f)" "$(grep -c 's\.db\.ExecContext' $f)" \
        "$(grep -c 'withRollbackGuaranteedRetry(' $f)"; done

This is WS-3's correction, and it generalises past this number: **check what a stale count was
counting, not only whether it reproduces.** Two counts went wrong the same way on 2026-09-04 —
§3.33's "only two actionable findings" was true of the root module and missed a G112 in
`examples/`, and a gosec comment claimed 0 findings where the config actually run gave 67. A count
is scoped to what was walked, and the scope is the part nobody writes down.

## The guard is structural, and it is the part that outlives the fix

`TestEveryMSSQLTransactionBoundaryIsRetried` walks every `engine/mssql_*.go` with `go/ast` and
fails on any function that opens **and commits** a transaction without being reached through
`withRollbackGuaranteedRetry`. A set-membership baseline would not have caught the original
defect, because the deferred boundaries were never in a baseline to begin with.

It fired on its first run, on a false positive worth keeping in the definition: keying on
`BeginTx` alone flags `beginTxWithContext`, which opens a transaction and hands it back, and
`tenantSessionConn.BeginTx`, a driver passthrough. Neither commits. **A boundary is a function
that opens and commits**; its callers are checked on their own.

Deliberately structural rather than behavioural. Proving a retry happens needs a live deadlock,
which `engine/mssql_deadlock_test.go` does for the paths it covers — and which skips wherever
`CLEAT_TEST_MSSQL` is unset, so it cannot be the thing that stops a boundary being added
unwrapped.

**Falsified:** removing the wrapper from `AppendEventHistoryBatch` alone reddens the guard naming
that function, with the other two still passing.

`engine/mssql_tenant_predicate_test.go`'s allowlist caught the refactor within seconds — its entry
keyed on `PollAndClaimSignal` and the unscoped statement had moved to `pollAndClaimSignalOnce`.
Renamed rather than deleted, matching `compactHistoryOnce` and `deleteExpiredEventsOnce` from
§2.26's earlier increments, which is evidence the split follows the house pattern.

Verified 4623 pass / 0 fail / 6 skip on `./engine/ -p 1` with all three dialects connected.
**The first attempt at that run reported 825 failures** because the DSNs were reconstructed from
memory and named a database `cleat_test` that does not exist — the exact failure CLAUDE.md's
"Is this result real?" section describes itself committing. The DSNs are written down in
`WORKSTREAM.md`; read them. The probe used afterwards has a negative control: the good DSN passes
`TestPluginMigrations_AllDialects` and a wrong password fails it.

### 3.204 Locks, promises and side effects could not be compiled from any Go WASM workflow — 🟢 **FIXED 2026-09-05** (WS-1, 2026-09-05)

Four host calls generated Go that does not compile. **Not a subtle failure mode — `cleat build`
exits 1** on any workflow that touches them:

```
h.AcquireLock("k", time.Second)
  gen_host_adapter.go: undefined: ttl_ms
  gen_host_adapter.go: unknown field AcquireLockMs in struct literal of type cleat.HostCallsOptions

h.AwaitPromise(id, 5*time.Second)
  undefined: promise_idPtr, promise_idLen, timeout_ms, resultOutBuf

h.SideEffect(func() (string, error) { ... })
  cannot use func(fn func() (string, error)) (string, error) as
  func(computedResult string) (string, error) value in struct literal
```

Distributed locks, durable promises and side effects are three of the primitives a durable
workflow engine exists to provide. All three were unusable from Go, the tier-1 guest language.

**Found while writing the test §3.200 said was missing**, which is the whole reason to write the
test a report admits it did not do rather than filing the gap and moving on.

## Three independent causes

**1. `adapterDefs` had an entry for a field that does not exist.** `AnalyzeUsage` keys `info.Used`
by *ImportName*, so once `cleat_acquire_lock` is used, **every** `hostFunctions` row sharing that
import contributes a struct field — and `AcquireLockMs` is not a field on `cleat.HostCallsOptions`.
`HostCallsImpl.AcquireLockMs` reaches the host through `opts.AcquireLock`, so the entry was never
needed. Removed. The other two multi-row imports, `cleat_defer` and `cleat_sleep`, are both real
fields and are fine:

    grep -c '"AcquireLockMs": {' wasm/adapter_metadata.go   # 0

**2. The import spec and the adapter spec had to agree on parameter names, and nothing made them.**
For a scalar the generator emits the *import's* name as the call argument, and for a string it
emits `<importName>Ptr`/`<importName>Len`. The import specs were snake_case and the adapters
camelCase, so five names never resolved: `ttl_ms`, `promise_id`, `timeout_ms`, `result_out`,
`promise_id_out`. Renamed to match the adapters, which is the convention the working calls already
used (`timeoutMs` in `cleat_send_signal_and_wait`).

**3. Two adapters declared a closure the options struct will not accept.**
`HostCallsOptions.AwaitPromise` is `func(promiseID string, timeout time.Duration)` and the adapter
declared `timeoutMs int64`. `HostCallsOptions.SideEffect` is `func(computedResult string)` — the
SDK's `HostCallsImpl.SideEffect` calls the closure itself and passes the computed string on — and
the adapter declared `fn func() (string, error)`.

## Why every existing test passed

**Nothing compiled generated code.** Every test in `./wasm/` inspects the generated source *as a
string*, so an identifier that does not exist and a closure of the wrong type both pass.

And the gap was known. `TestRunBuild_GoTargetBuildDir` in `cmd/cleat` runs the whole pipeline —
analyze, `BuildOutputs`, `PrepareBuildDir` — and stops one step short, saying so in its own
comment: *"Verify the build directory setup for the go target without requiring actual go build to
compile."* It then asserts the generated files **exist**. Four broken host calls sat behind that
sentence.

§3.200 recorded the same gap from the other side and, having recorded it, did not close it: "a
wrong buffer name would pass every test in that package… a test that builds a workflow exercising
every host call would be strictly better and does not exist." Its substitute check — is each
identifier used elsewhere in the same adapter — reported **15 of 15 clean**, because
`AwaitPromise` used `resultOutBuf` consistently in both branches of an adapter that had never
compiled. **A consistency check cannot see a name that is consistently wrong.**

## The test

`TestGeneratedAdapterCompilesForEveryHostCall` runs the real pipeline over
`testdata/allhostcalls`, a workflow calling every `HostCalls` method, then invokes the Go
compiler for `wasip1/wasm` on the result. All 37 `adapterDefs` fields are exercised — asserted,
not assumed, by the companion test below — and 53 host functions reach the adapter.

`TestEveryHostCallIsExercisedByTheCompileFixture` keeps it honest — a call the fixture never makes
is a call nobody compiles. It caught three on its first run (`DurableCallWithHeartbeat`,
`DurableDeferFunc`, `RegisterUpdateHandler`), which were then added.

**Falsified:** restoring `ttl_ms` alone fails the compile test with
`gen_host_adapter.go:388:53: undefined: ttl_ms` — the generator's own output, not a proxy for it.

**CI caught two things this section's own reasoning had missed.**

`engine/stop_correspondence_guard_test.go`'s `stopSurfaces` table named `"AcquireLockMs"` as a Go
adapter, so removing the entry made that guard fail with *`stopSurfaces["AcquireLock"] names Go
adapter "AcquireLockMs", which is not in adapterDefs`*. The table is right to notice — it
cross-references three surfaces — and the entry is now `{"AcquireLock"}` with the reason recorded
beside it.

And the test shipped with a `-short` skip, one paragraph below a comment saying it must never
learn to skip. `scripts/check-skips.sh` rejected it, and its taxonomy names the error exactly:
this is case (c), *"the precondition is always satisfiable in this repo"*. There is no
environmental question to ask, so a skip here is a decision not to run the test. Removed rather
than baselined.

The test must not learn to skip. `wasip1` ships with the standard toolchain, so there is no
environmental precondition to detect; a skip here restores exactly the blind spot the test removes.
**It has no skip at all** — the `-short` guard this sentence used to claim lived for about an hour
before CI rejected it, and the paragraph above records why. Confirm with
`grep -n 'testing.Short' cmd/cleat/generated_adapter_compiles_test.go` → nothing.
### 3.205 The Python end-to-end test's coverage was one host call wide — 🟢 **FIXED 2026-09-05** (WS-1, 2026-09-05)

The Python half of §3.204. `TestPythonWasmEndToEnd` compiles
`python-sdk/examples/durable_call_workflow.py`, which calls `h.call()` and nothing else. Passing
it means **"Python can make a durable call"**, not "the Python host-call surface builds" — so a
binding that does not exist, or does not accept what the SDK passes it, reaches users rather than
CI. `cleat_sdk.HostCalls` has **73** public methods; one was covered.

**Python is better defended than the Go side was, and that was deliberate.** `tiers.yaml` puts
python in `tier1.languages`, and `.github/workflows/tier1-gate.yml` installs `componentize-py`
with a comment saying exactly why:

> python is tier 1 (D2), so this is a tier-1 precondition rather than a convenience — without it
> `TestPythonWasmEndToEnd` and `TestPythonComponentExecutionFence` skip, and the gate fails on the
> skip rather than letting the run go quietly green.

The gap was never the toolchain. It was the fixture.

## Three guards, in two places, for two different costs

`python-sdk/tests/test_all_host_calls_fixture.py` — **no toolchain, runs everywhere**:

1. every public `HostCalls` method appears in the fixture;
2. every `h.<name>` in the fixture is a real `HostCalls` method (a typo would otherwise sit there
   looking like coverage);
3. every call **binds** to the real signature, via `inspect.signature().bind`.

`engine/python_all_host_calls_test.go` — **needs `componentize-py`**, and builds the fixture for
real through `python-sdk/scripts/build_wasm.py`, the same path `TestPythonWasmEndToEnd` uses. Its
prerequisite handling is copied from that test deliberately, including the `toolchainRequired`
escalation: a job declaring `python` in `CLEAT_REQUIRE_TOOLCHAINS` **fails** rather than skips.

Both halves use AST rather than text. A `grep` for `h.<name>(` would count names inside this
fixture's own docstrings, which name host calls.

**Guard 3 is the one worth having, and it found a defect in the fixture on its first run.**
`componentize-py` cannot catch an arity error — Python binds arguments at call time, so a wrong
call count compiles into the component happily and fails only when the workflow runs. This fixture
is never run. `h.log_kv("m", k="v")` was wrong; `log_kv(self, message: str, *kvs: Any)` takes
positional pairs. Without guard 3 that line would have looked like coverage forever.

**Falsified, each separately:** deleting `h.release_lock("k")` fails guard 1 naming it; adding
`h.nonexistent_call("x")` fails guard 2 naming it; dropping `acquire_lock`'s second argument fails
guard 3 with `missing a required argument: 'ttl_seconds'`.

## Verified locally, in the container the repo already provides

    docker --context desktop-linux run --rm -v "$PWD":/src -w /src -e CGO_ENABLED=1 \
      cleat-py-toolchain go test ./engine/ -run TestPythonAllHostCallsWorkflowCompiles -count=1

    --- PASS (1.71s).  Build SUCCESS, 17.94 MB component, all 73 calls.

**This section first said the fixture "has not been compiled on this machine", and that hedge was
wrong in an instructive way.** `componentize-py` does die here with exit `-9`, and the control was
sound — the *existing* `durable_call_workflow.py` dies identically, so it is the environment rather
than the fixture. **But "environmental" is not "unavoidable", and stopping at the first
correct-sounding answer is what made it look like one.**

`scripts/docker/python-toolchain.Dockerfile` has documented the cause and the fix since 2026-08-06,
in its header: componentize-py's embedded wasmtime "installs a mach exception handler into a
guarded port and the process dies with EXC_GUARD / GUARD_TYPE_MACH_PORT. That guard is a Darwin
kernel feature with no Linux equivalent, which is why the Linux CI runners have always been able to
build Python components while a developer's Mac could not." Deterministic, platform-specific, and
already solved. The image was prebuilt on this machine.

So the diagnosis in the first draft — memory pressure, a sandbox limit — was wrong, and it was
passed to WS-2 as agreement with their own signal-9 report rather than checked against the tree.

**Two caveats, both from the Dockerfile and both already paid for by someone.**
`--context desktop-linux` is not optional on a Mac that also runs colima: colima cannot bind-mount
these paths and **says nothing**, so `-v "$PWD":/src` yields an *empty* directory and the run fails
with `go: go.mod file not found`, which reads as a broken checkout. Sanity-check the mount before
believing any failure from this image. And two warnings — `wasm-tools component decompose not
available` and `metadata stamping failed (non-fatal)` — appear identically when building
`durable_call_workflow.py`, so they are pre-existing rather than anything this fixture introduced.
That control is the only reason they are not recorded here as a finding.

The calls sit in `_exercise_every_host_call`, which the entry point reaches only when its request
says so. `continue_as_new`, `extend_timeout` and `release_lock` would change a running workflow's
fate, and this file is on the compile path, not the behaviour path.

### 3.208 The Java host-call surface: 8 of 70 compiled, now all 70 — 🟢 **FIXED 2026-09-05**; surface corrected 68→70 by #753 (WS-1, 2026-09-05)

Second tier-2 row of §3.206. `examples/java-workflow` and the plugin-harness fixture between them
called **8 of 68** `cleat.HostCalls` methods.

`crates/cleat-java/src/test/java/cleat/AllHostCallsCompileTest.java` calls all 68. Like Rust, it
**compiled with one fixable error** and no SDK defect — `RetryPolicy` is a nested
`HostCalls.RetryPolicy` and needs qualifying, which is a fixture mistake, not an SDK one.

**It lives in the SDK's own test source set rather than in a new example crate, and that choice is
the useful part.** `gradle test` in `crates/cleat-java` is already the **required** `Java Tests`
check, so the surface is compiled by a job that exists, on a toolchain CI already provisions, with
no TeaVM step, no new workflow wiring and no `tiers.yaml` exclusion. The Rust equivalent (§3.207)
needed all four because it had to build a `cdylib` for `wasm32-wasip1`.

`exerciseEveryHostCall` is never invoked — each call would trap without a host, and
`continueAsNew` and `releaseLock` would change a running workflow's fate. `javac` accepting the
calls is the assertion; the `@Test` makes the JVM load and verify the bytecode, and fails visibly
if the method is deleted rather than silently dropping the coverage.

**This is a compile check and not a behaviour check, and the distinction is load-bearing for
Java specifically.** It says every method exists with the signature a workflow can call. It says
nothing about the TeaVM WASM codegen — which is exactly where the Java-specific defects have been:
§3.303 found 16 of 17 plugin calls failing while all five language tests passed, and #455 fixed a
Java workflow returning JSON-in-a-string. The plugin-harness tests cover that path; this covers the
one nothing covered.

**Falsified:** adding `h.noSuchHostCall("x")` fails `compileTestJava` with
`error: cannot find symbol … method noSuchHostCall(String)`.

**Verified the test actually ran, rather than trusting `BUILD SUCCESSFUL`.** The first run printed
that and nothing else, which is what an up-to-date task also prints. `--rerun-tasks` plus the XML
report:

    build/test-results/test/TEST-cleat.AllHostCallsCompileTest.xml
    tests="1" skipped="0" failures="0" errors="0"

Coverage after §3.207 and this: **rust 63/71, java 70/70** (both corrected 2026-09-05 by #753 —
the surface scans under-counted). AssemblyScript (11/66) is the remaining
tier-2 row; go and python reach 37/37 and 73/73 when §3.204 and §3.205 land.

### 3.209 The AssemblyScript host-call surface: 11 of 66 compiled, now all 66 — 🟢 **FIXED 2026-09-05** (WS-1, 2026-09-05)

Last tier-2 row of §3.206. `examples/as-workflow`, `examples/widget-store-as` and the
plugin-harness fixture between them called **11 of 66** `HostCalls` methods.

`packages/cleat-as/assembly/__compile__/all-host-calls.ts` calls all 66, type-checked by
`asc --noEmit`. As with Rust (§3.207) and Java (§3.208), **no SDK defect** — three for three on
the hand-written SDKs, against four uncompilable host calls in the one SDK whose adapter is
generated (§3.204). The pattern is now well enough evidenced to state: **the Go breakage was a
property of code generation, not of breadth.**

**It began as an as-pect spec and that was wrong, for a reason worth keeping.** as-pect
*instantiates* the module it compiles, and the host imports are not callable in that runner —
`LinkError: Import "env" "cleat_call": function import requires a callable`. The package's other
specs say so in their own header: they test "pure functions and constants that do not require
`@external` host function imports". So the fixture lives in `assembly/__compile__/`, outside the
spec directory, and is type-checked rather than run. **The compile was already the assertion; the
harness was adding an instantiation nobody wanted.**

Wired through `package.json` rather than through a workflow: `test` now runs
`check:host-calls && asp`. The **required** `AssemblyScript Tests` job already does `npm ci &&
npm test`, so the surface is compiled by a job that exists with **no change to
`.github/workflows/`** — the same move as §3.208's, which used `gradle test` in the required
`Java Tests` job. Only §3.207 needed workflow-adjacent wiring, because only Rust had to build a
`cdylib` for `wasm32-wasip1`.

**The coverage script found the one method a hand-written fixture missed.** After the first pass it
read 65/66, uncovered: `childWorkflowWithOptions` — declared across a single long line with a
defaulted `options` parameter, which the signature extraction I was reading from had skipped while
the surface extraction caught it. **Two extractors disagreeing is what surfaced it**; one alone
would have reported 65 as complete.

**Falsified:** adding `h.noSuchHostCall("x")` fails with
`ERROR TS2339: Property 'noSuchHostCall' does not exist on type 'assembly/host-calls/HostCalls'`,
and `asc` exits 1 where the clean fixture exits 0.

Coverage after §3.207–§3.209: **rust 63/71, java 70/70, assemblyscript 66/66** (rust and java
corrected by #753; the earlier 61/61 and 68/68 came from surface scans that under-counted, so
"all five SDKs at 100%" was never true). Go and Python
reach 37/37 and 73/73 when §3.204 and §3.205 land, which completes compile coverage for all five
SDKs. **What remains unmeasured is execution** — see §3.210.

### 3.211 A guard can be in the tree, green, and selected by no CI pattern at all — 🟢 **FIXED 2026-09-05** (WS-1, 2026-09-05)

The host-call execution harness (§3.210's remedy, plan item A1) was committed to
`tests/plugin-harness/`, passed locally, and **ran nowhere in CI.** The Layer 2 step in
`plugin-harness-ci.yml` selects its tests by name:

    go test ... ./... -run 'TestPluginCalls_Wasm'

and `TestHostCallsGo` does not match that. So the guard existed, was green, and was not run — in the
PR whose entire subject is the difference between a call that compiles and a call that runs.

**This is the mirror of the trap CLAUDE.md already carries, and the harder direction of it.** That
one is `TestTenantIsolationAcrossDialects`: a `-run` pattern naming a test that does not exist,
where `go test` prints `ok … [no tests to run]` and exits 0. Here the test *does* exist, the file
*is* committed, and the selector silently excludes it. **A reader checking "is it in the tree" gets
yes**, and every question that gets asked about a new guard — is it written, does it pass, can it
fail — returns the right answer. The one that does not get asked is whether anything runs it.

Measured both ways rather than assumed, which is the whole of the fix:

    cd tests/plugin-harness
    go test ./... -run 'TestPluginCalls_Wasm'               -v | grep -c '^=== RUN   TestHostCallsGo/'   # 0
    go test ./... -run 'TestPluginCalls_Wasm|TestHostCalls' -v | grep -c '^=== RUN   TestHostCallsGo/'   # 24

**The general rule, which is not the same as the `-run`-matches-nothing rule:** where a job selects
tests by name, adding a test file is not adding a test. Count the subtests the job's own pattern
selects, before and after. `grep` for the test's name in the workflow is not enough either — an
alternation that names it can still be wrong, and the count is what settles it.

Two things this did *not* fail on, both of which read as verification and are not:

  * `go vet ./...` and `go test .` both passed — they compile and run the package directly, which is
    a different question from what a job's `-run` selects.
  * The skip budget is unaffected. A test that never runs does not skip, so `check-skip-budget.sh`
    is blind to it by construction. This file's whole "is this result real?" discipline is about
    skips; **a test that is not selected is a third state next to pass and skip**, and nothing in
    the tree currently counts it.

**Where it runs, and why that is not settled.** Layer 2 rather than `Cross-Language WASM E2E`.
WS-3's C1 recommended E2E because it installs every guest toolchain and the alternatives install
none — but Layer 2 already installs Rust, Python with `componentize-py`, Java and Gradle, and
already runs this module, which is its own Go module and so is not reached by a pattern from the
repo root. With only the Go reference in place, the job that already runs the module is the smaller
change. When B2 and C2 add the other four languages this should be re-decided on measurement, and
per WS-2 the deciding term will not be toolchain install: **a Python invocation is ~0.93s and does
not amortise**, so cost scales with invocation count rather than fixture count and the two jobs
diverge as fixtures land.

`.github/workflows/` is WS-3's file; adding there follows WORKSTREAM.md's protocol — another stream
may when leaving the mechanism unwired would be worse — and the comment says so at the change.

### 3.214 Go's state reads never reach the host, and the docs promise they do — 🟢 **CLOSED 2026-09-05 by §3.216**, which removed the feature rather than repairing it (WS-1, 2026-09-05)

**Resolved by removal, not by repair.** §3.216 deleted the whole state family on the same day: the
question below — whether the guest-local read was a deliberate scratchpad or a data-loss bug — was
answered by establishing that a run-scoped key-value API is equivalent to a local variable in every
SDK, so neither reading justified keeping it. The analysis is kept because the *evidence* is what
decided the removal, and because the shape it describes recurs.

`HostCallsImpl` in `cleat/runtime_workflow.go` offers `SetState`, `GetState`, `HasState`,
`IncrState`, `ListState` and `DeleteState`. **None of them is a host binding.**

- `SetState` writes `h.stateMap`, a `map[string]interface{}` **inside the guest**, and then
  one-way-persists through `set_query_state`.
- `GetState` reads that map and nothing else. So do `HasState`, `IncrState` and `DeleteState`.
- Every write to `stateMap` is the guest's own, and the map is only ever created empty. **No path
  anywhere populates it from the host.**

The host side is not missing. `cleat_get_state`, `cleat_has_state`, `cleat_incr_state`,
`cleat_list_state` and `cleat_delete_state` are all exported by `engine/imports.go`, implemented,
and tested — `engine/lifecycle_test.go:686` exercises the host's `GetState` directly. **Rust and
AssemblyScript bind all six**, as real declarations rather than wrappers:

| | rust `pub fn`, `host_calls.rs` | AS `@external`, `host-calls.ts` |
|---|---|---|
| `cleat_set_state` | 244 | 412 |
| `cleat_get_state` | 247 | 424 |
| `cleat_delete_state` | 250 | 436 |
| `cleat_incr_state` | 253 | 446 |
| `cleat_has_state` | 256 | 457 |
| `cleat_list_state` | 259 | 467 |

**Only Go cannot read them.**

Those Rust line numbers were first written as `194,243,247,253`, and two of the four pointed at
**comments** — `// cleat_set_scope` and `// cleat_set_state` — rather than at declarations. Cited
before being opened. It is the same failure as counting a name in prose as a binding, in a section
about exactly that, so the numbers above were taken from
`grep -nE '^\s+pub fn cleat_(set|get|has|incr|list|delete)_state\('` rather than from a search for
the name.

    grep -n 'stateMap' cleat/runtime_workflow.go       # every write guest-side; no host read
    grep -n 'setQueryState' cleat/runtime_workflow.go  # the family's only host call, write-only

## What that means at runtime

Within one execution of one instance, set-then-get works, because the map is still there. Across a
`continue_as_new`, or in any second instance, `GetState` returns `durable: state not found for
key: <k>` for a key **the host is holding**. `IncrState` is a read-modify-write over a map that
starts empty, so it restarts from zero rather than continuing.

**This is worse than a missing method, which is why it is filed separately from §3.213's count.**
An absent method fails at compile time, at the desk of the person writing the workflow. This one
compiles, passes its tests, works in development against a single instance, and returns the wrong
answer later on a different instance. §3.213's matrix records Go at 35 of 55 and a reader will take
that as "Go supports fewer features"; for this family the truth is that Go has the method and it
means something else.

## The existing test cannot fail on this

`TestHostCallsImpl_StateOperations` (`cleat/runtime_behavioral_test.go:1765`) does
`SetState` then `GetState` on **one** `HostCallsImpl` and asserts the value comes back. A
`map[string]interface{}` satisfies that. So does a correct host binding. The test cannot
distinguish them, and it is green today for the same reason it would be green if the feature were
deleted and replaced with a local cache — which is what it is.

That is this repo's most familiar shape: an assertion held up by a layer other than the one under
test. The falsification is cheap and has not been written: set state, cross an instance boundary,
read it back.

## The documented contract is durable, stated three times, with no caveat

This section first left intent open, on the strength of `docs/determinism.md:173` presenting
`SetQueryState` as "the mechanism that actually fits how cleat runs". **That passage is about query
state, which is a different API**, and reading it as though it covered `SetState`/`GetState` was a
conflation, not a finding. The docs on the state family are not ambiguous:

| where | what it says |
|---|---|
| `docs/reference/sdk-api.md:31` | `StateManager` — **durable key-value state** |
| `docs/reference/sdk-api.md`, StateManager | "Full key-value state management scoped to the current workflow." |
| `docs/migration/from-restate.md:23` | maps Restate's `ctx.get`/`ctx.set`/`ctx.clear` to `get_state()`/`set_state()`/`delete_state()`, "Similar key-value state" |
| `docs/migration/from-temporal.md:22` | Memo / Search Attributes → "Use Cleat's state API" |

No caveat exists anywhere. Searched:

    git grep -n -i "state.*not durable\|in-memory state\|state.*per-execution\|state is local" -- 'docs/**' '*.md'

The Restate row is the sharpest of the four, because **Restate's virtual-object state is durable
across invocations — that is the entire point of it** — and the migration guide tells a Restate
user their `ctx.get` ports to `get_state()`. In Go it ports to a map lookup that returns not-found
on the second invocation.

**And the design doc draws exactly the distinction Go's implementation collapses.**
`docs/contributor/design/cleat-execution-design.md:1152`:

> `SetQueryState` merges the key-value pair into a JSON object (`query_state`) … **`SetQueryState`
> is NOT recorded in the event history — it is derived state, not durable state.**

So the architecture separates derived, queryable state from durable state on purpose. Go's
`SetState` is implemented **on top of `set_query_state`** — the mechanism that doc classifies as
not durable — while `GetState` reads a guest-local map. Go's durable state API is therefore not
durable in either direction: the read never consults the host, and the write goes to the store the
design explicitly says is derived.

## So this is a defect against a written contract, not an undocumented design choice

That resolves what this section originally declined to guess at, and it resolves it the less
comfortable way. The remaining question is only which repair:

1. **Bind the five host calls Go cannot reach**, making the docs true. `cleat_get_state`,
   `cleat_has_state`, `cleat_incr_state`, `cleat_list_state` and `cleat_delete_state` are exported,
   implemented, tested, and bound by Rust and AssemblyScript already.
2. **Change the docs and rename the methods**, making the code true — and accept that
   `from-restate.md` is inviting a migration that silently loses state.

Option 2 is a product decision to ship a weaker feature than the reference SDKs offer and than the
docs promise; it is not a smaller version of option 1. **Either way `tiers.yaml` has to say which**,
because right now the manifest grants support that the Go implementation does not provide.

What is not open: whether the current state is acceptable. An undocumented divergence from a
written contract, whose only test cannot detect it, is not a documented gap in the sense the
release rule means.

Found by WS-3 while re-deriving §3.213's Go figure of 35, and confirmed here independently. It is
the mirror of §3.207: there a strict extractor missed generics and **inflated** Rust's coverage;
here a loose scan of Go method names finds nine methods that exist and would **credit bindings that
do not**. Anchoring on the import table rather than on method names is what makes 35 correct.
### 3.216 The durable-state family is removed — 🟢 **DONE 2026-09-05** (WS-1, 2026-09-05)

`cleat_set_state`, `cleat_get_state`, `cleat_delete_state`, `cleat_incr_state`, `cleat_has_state`
and `cleat_list_state` are gone, with the `cleat:host-calls/durable-stream-state` component
interface and every SDK wrapper. This closes §3.214, which asked whether Go's guest-local reads
were a bug or a design, by removing the feature both readings were about.

**The count is 58 → 52 exports total, of which 49 are `cleat_`-prefixed**, and saying which is not
pedantry: `plugin_call`, `plugin_call_streaming` and `set_query_state` carry no prefix, so both
numbers are true and will be quoted interchangeably forever unless a doc commits. A `grep 'cleat_'`
over this surface undercounts by three, which is the defect that produced §3.213's wrong
denominator and the blind spot in the runtime parity guard (#759). Flagged here by the
conformance-port session, which reached 55 where this branch reached 58 and chased the difference
rather than assuming one of us was wrong.

    grep -oE '\.Export\("[^"]+"\)' engine/imports.go | sort -u | grep -c .   # 52
    grep -oE '\.Export\("cleat_[^"]+"\)' engine/imports.go | sort -u | grep -c .   # 49

**And the blind spot covers half of the only surface cleat has ever proved in practice.**
`testdata/clew-lifecycle/workflow.go` is a real workflow from the one workload cleat has carried,
preserved as test data. It uses **four** host calls out of ~55:

    h.DurableLog       3 sites   -> cleat_log
    h.SetQueryState    1 site    -> set_query_state      UNPREFIXED
    h.PluginCall       1 site    -> plugin_call          UNPREFIXED
    h.SignalWorkflow   1 site

**Two of the four are among the three unprefixed exports.** So "#759's parity guard compared 55 of
58 names and never saw `plugin_call`" is not an abstract tidiness point — the guard was blind to
half the calls the only real user actually made. A prefix-derived surface omits three names, and
those three are not a random three.

Found by the conformance-port session while establishing whether §3.215's signal overwrite was
hypothetical. **It is not, and the first version of this paragraph named the wrong evidence.** It
cited `h.SignalWorkflow(parent, "child_done", taskID)` at line 235 of the fixture as the affected
fan-in. With access to `cleat-team/clew` the same session established that **nothing consumes
`child_done`** — the parent fans in via `AwaitAllChildren`/`AwaitAnyChild`, so that signal is
advisory and §3.215 does not hurt it. Recorded rather than quietly replaced, because "the shape is
present in the code" and "the shape is load-bearing" are different claims and only the second is
evidence.

The real instances are worse. `workflows/leafphase/workflow.go:404` and
`workflows/review/workflow.go:452` each run

    received := 0
    for received < len(pending) {
        signal := h.AwaitSignals([]string{"agent_result"}, timeout)

— N concurrent tasks, each replying with **one signal under the same name**, counted in and matched
by a `task_id` the application puts in the payload because it had to solve that problem itself. The
workflow is careful and correct; the storage layer cannot deliver what it asks for.

See §3.215 for why the consequence is a hang and not only a lost payload.

## The comparison that decided it

| engine | state scoped beyond one workflow? |
|---|---|
| **Temporal** | No. Workflow state is local variables made durable by replay. Memo and Search Attributes are per-execution visibility metadata. |
| **DBOS** | No framework API. Durable state is your own tables inside `@DBOS.transaction`; `setEvent`/`getEvent` is `set_query_state`, which `from-dbos.md:17` maps correctly. |
| **Restate** | **Yes** — virtual objects have keyed state durable across invocations. |

The repo's own migration guides had already voted. References to the state family:
`from-restate.md` **6**, `from-temporal.md` **0**, `from-dbos.md` **0**. Only the guide for the one
engine that has the feature had any use for it.

    for f in docs/migration/from-*.md; do
      echo "$f $(grep -cE 'get_state|set_state|GetState|SetState' "$f")"; done

## Why a run-scoped key-value API is worse than none

Measured before deciding, not assumed: cleat's state was rebuilt from that run's event history, and
`s.stateStore` was **never seeded from persistence** — for every SDK, not just Go. So it did not
persist across `continue_as_new` or between instances for anybody.

**Within a run it was therefore exactly equivalent to a local variable**, because replay
re-executes the workflow and rebuilds either one. That equivalence is not a theory: it is why a Go
guest-local map passed a set-then-get probe indistinguishably from the real host calls (§3.214),
and why `TestHostCallsImpl_StateOperations` was green for as long as it existed.

So the API offered nothing a variable did not, while carrying Restate's names and shape. A reader
coming from Restate would find `set_state`/`get_state` where they expected keyed cross-invocation
state and get a per-run scratchpad, with no doc saying so. **That is confusion with no benefit, and
it is a worse failure mode than absence** — absence fails at compile time, at the desk of whoever
is writing the workflow.

## What survives, and why none of it is this feature

- **`set_query_state`.** The queryable-state mechanism and the DBOS `setEvent` equivalent, which
  `docs/contributor/design/cleat-execution-design.md:1152` explicitly calls "derived state, not
  durable state". Untouched.
- **`set_scope` / `get_scope` / `clear_scope`.** These call `AcquireConcurrencyKey`, so they give
  at most one workflow per `objectType:instanceKey`. **That is virtual-object mutual exclusion
  without virtual-object state**, and it stands on its own merits. Checked rather than assumed —
  `scopedKey()` had exactly six callers and all six were state methods, so the question "does
  scoping still have a purpose" had to be answered from the engine, not the SDK.
- **`EventCodeStateMutation = 16`, retired rather than removed.** The compaction decoder has no
  `default:` case, so an unknown code falls through and yields a record with only the common fields
  set. Deleting the arm would make any history recorded before today decode **silently** into empty
  state records rather than failing loudly. The number is reserved and must never be reused.

Also removed: `cleat/virtualobject.go` — six of its eight methods were state accessors and nothing
outside its own test used it — and the `VirtualObjectDef` registry, which had no callers at all.

## Docs corrected, not just updated

`from-restate.md` is the one that mattered. It described the gap as **ergonomic** — Restate scopes
automatically, "cleat requires explicit `set_scope()` calls" — and showed a worked example calling
`h.get_state("items", list)` inside a scope. **That example never worked**: two invocations for one
key are two workflow runs with two histories, so nothing was shared. The gap was not ergonomic and
the workaround was not a workaround.

`sdk-api.md` no longer says "durable key-value state" in two places. ABI.md loses §2.28-§2.33,
which are left vacant per the §2.21 precedent.

## What this does not do

Cross-instance shared state, of the kind Restate's virtual objects provide, **still does not exist
in cleat for any language** — it never did. This removes an API that implied otherwise; it does not
add the capability. If that capability is ever wanted it is engine work — persisting state keyed by
scope and seeding `stateStore` at session start — and a much larger change than these six calls.
### 3.218 A promise the store refused was reported as created, and the workflow then hung — 🟢 **FIXED 2026-09-05** (WS-1, 2026-09-05)

`CreatePromise` (`engine/promises.go`) logged a store failure and continued, returning `errCode 0`
to the guest. The obvious reading is that history and the promise store end up disagreeing. The
actual consequence is worse and is a hang:

1. `s.recordEvent(rec)` writes `EventTypeCreatePromise` **before** the store call, so history
   already asserts the promise exists.
2. The store refuses. The error is logged and execution continues.
3. The guest receives a promise ID and `errCode 0` and proceeds.
4. The later `AwaitPromise` calls `GetPromise`, which finds nothing, so **neither** the `resolved`
   nor the `rejected` branch is taken.
5. Control falls through to "Record await and suspend".

The workflow then waits for a promise **no external caller can ever resolve**, because the row they
would resolve against was never written. Nothing errors, and the log line is the only trace.

**The ABI always had somewhere to put this.** `cleat_create_promise` returns `errCode` in bits 0-31
(ABI.md 2.34). The failure was not unreportable; it was unreported. The fix returns
`packSimpleResult(1, …)` with the store's own message in the output buffer.

## A test asserted the defect

`TestCreatePromiseFreshStoreError` read:

    // Store error is logged, not surfaced. Function should still succeed.
    if result != 0 {
        t.Errorf("expected 0 (error is logged, not surfaced), got %d", result)
    }

That is the third instance in one day of a test **codifying** wrong behaviour rather than
specifying right behaviour, after `TestHostCallsImpl_StateOperations` (§3.216) and the
`AwaitAllChildren` row (#758). All three share a shape worth naming: **they assert the code as
written and justify neither half**, so they cannot fail on the thing they appear to cover, and they
make the defect look deliberate to the next reader. `engine/children_test.go:806` already carries
that lesson in its own words — "It asserted the code as written and justified neither half, which
is why it held the defect in place rather than catching it."

The assertion is now inverted, with the old text quoted in place so the change is legible.

## Not settled here

This fixes the reporting, not the ordering. The event is still recorded before the store write, so a
crash between the two leaves history asserting a promise the store never received — and on that
path there is no error to return, because nothing failed. Making the two atomic is a larger change
and is not attempted here.

Also unaddressed: **every other swallowed store error on this pattern.** This section fixes
`CreatePromise` because that is where the hang was traced. Whether `recordEvent`-then-store appears
elsewhere with the same swallow is an open question and a cheap sweep.

Found by the conformance-port session while running a three-dialect SQL comparison. The dialect
question it started from turned out to be latent — `CreatePromise`'s conflict handling differs
across the three stores (`DO NOTHING` / `INSERT IGNORE` / plain insert) but promise IDs are freshly
generated UUIDs, so a duplicate is unreachable in practice. **The swallowed error sitting two lines
away was the live defect, and it is dialect-independent.** Worth recording as a method note: the
sweep's value was not the answer to the question it asked.

### 3.220 `SendSignalAndWait` never sends the signal it then waits for — 🟢 **CLOSED 2026-09-06: composed in all five SDKs, host calls removed** (WS-1, 2026-09-05)

Found while wiring signal consumption for §3.215, and deliberately not fixed there: it is a
different defect and a different change.

`execSession.SendSignalAndWait` (`engine/signaller.go`) does, in order: the replay check, the
`stopBeforeNewWork` guard, the signal-authorization check, a `PollSignal` against `targetRunID`,
and then — finding nothing — records an `await_signals` event and suspends. **There is no
`DeliverSignal` call anywhere in the function.** Compare `SignalWorkflow` immediately below it,
which does call `s.engine.signalStore.DeliverSignal(ctx, targetRunID, signalName, payload)`.

    grep -n "signalStore.DeliverSignal" engine/signaller.go
    # one hit, line 304, inside SignalWorkflow

Anchor on `signalStore.DeliverSignal`, not on `DeliverSignal`. The bare name now has two hits,
because the second is a comment in `SendSignalAndWait` explaining that it does not call it — a
grep a *denial* satisfies, which is the §1.1 trap, and the first draft of this section reported
"one hit" off a command that returns two.

So the `payload` argument is accepted, passed through authorization, and dropped. A workflow
calling `send_signal_and_wait` waits for a reply to a message that was never sent, until its
timeout.

**The poll is also aimed oddly, and the two facts are probably one bug.** It reads
`targetRunID`'s queue for `signalName` — the queue it would have written to had it sent — rather
than this workflow's queue for a reply. Written as a request/response, the target replies to the
*caller*; written as a send, the delivery goes to the target and there is nothing to poll. The
function reads like the second half of a design whose first half is missing.

**This is why §3.215 left this path non-consuming** while moving both `DurableAwaitSignals` paths
onto consume-after-record. Consuming here would delete a row on a guess about whose delivery it is,
and the two candidate answers imply different rows.

Whoever fixes it should decide the semantic first, because the two readings need different code:

| reading | what is missing |
|---|---|
| fire-and-wait-for-reply | the `DeliverSignal` to the target, and the poll should be on the caller's own queue with a correlation id |
| the target writes back under the same name | the `DeliverSignal`, and the poll is right but must consume |

`ReplyToSignal` is the other half to look at: it records a `signal_received` event and also calls
no store method, so a reply is durable in the replier's history and invisible to anyone else.

**Note what a name-based scan says about all of this: nothing.** `SendSignalAndWait` registers,
dispatches, has tests, and appears in every SDK's surface list. Its being wired end to end is what
the parity guards check, and it is wired — to a function that does not do the thing.

---

#### The decision (2026-09-06): the reply address is a promise ID

The repo owner chose composition over a repaired host call. `SendSignalAndWait` is now an SDK
composite over `CreatePromise` + `SignalWorkflow` + `AwaitPromise`, and `ReplyToSignal` is
`ResolvePromise`. **No new engine mechanism was needed**, which is only true because §3.233 had
just landed: settling by promise ID alone (#813) made an ID a globally unique token any holder
can settle, #818 made a settle that matches no row report `ErrPromiseNotFound` instead of
succeeding silently, and the settle wakes the creator. Those three properties are exactly a reply
channel. §3.233 recorded settle-by-ID as a design question with a cost; this is the payment.

**Both reference systems agree, and neither has the primitive.** This repo's own
`docs/migration/from-dbos.md` maps the whole of DBOS's communication surface — `send`, `recv`,
`setEvent`, `getEvent` — and all four are one-way; DBOS composes request/reply by putting the
requester's workflow ID in the message. Temporal's signals are likewise one-way. Neither
migration guide mentions `SendSignalAndWait` or `ReplyToSignal` at all
(`grep -rn 'SendSignalAndWait\|ReplyToSignal' docs/migration/` → nothing), so no one porting
from either would look for them. **In both systems the reply address is data, not protocol** —
which is the argument for making it a promise ID rather than inventing a correlation namespace.

The rejected option was a correlation-ID signal delivered to the caller's queue. It needs the
caller's workflow ID to reach the replier — by encoding it into the ID, making it a parseable
capability token, or by a mapping table, a new durable object with its own lifecycle — and it
rebuilds inside the signal table what `workflow_promises` already does.

#### Three implementations that disagreed, replaced by one

Nothing about this was a single broken function. The pair had three implementations and no two
matched:

| environment | `SendSignalAndWait` | how a reply arrived |
|---|---|---|
| engine (real guest) | inert — never called `DeliverSignal` | nothing |
| `cleattest` (Go) | spliced `_correlation_id` into the payload object | in-memory Go channel |
| `cleat/embedded` (Go) | returned a canned `{"status":"delivered"}` without waiting | nothing |
| `cleat-sdk` `test.rs` (Rust) | minted `corr-<target>-<name>-<n>` | private channel map |
| `cleat-test` (Rust) | minted `corr-<target>-<name>-<n>` | private channel map |
| `local_host.py` (Python) | returned a canned `{"status":"signal_sent", …}` without sending | nothing |

**Six implementations, no two alike, and not one of them could work in production.** The count is
the finding. Each was written to make a test pass in one environment, and because the real host
call was inert there was never anything to disagree with -- so nothing pulled them together. Three
of the six returned a constant.

So a workflow that passed under `cleattest` could not work in production, and `embedded`'s
version satisfied any assertion trivially. **Two shipped tests asserted those fakes**:
`TestSendSignalAndWait` checked `resp == '{"status":"delivered"}'` — a constant, unreachable by
any real reply — and embedded's `TestReplyToSignal` polled the response back as an inbound signal
named by the correlation ID. Both are now round-trip tests.

The composite is one implementation for all three, because `CreatePromise`, `SignalWorkflow`,
`AwaitPromise` and `ResolvePromise` already behave the same way in each. That is the substantive
argument for composition over a fourth implementation, and it is worth more than the ABI saving.

#### The envelope, and why it wraps rather than splices

The reply address travels under a reserved key, `cleat_reply_to`, in a two-key envelope whose
other key carries the caller's payload **as a JSON string**. `AwaitSignals` and `PollSignals`
strip it, so a receiver reads `SignalResult.Payload` unchanged and gets the address in a new
`SignalResult.ReplyTo` field, empty for a one-way signal.

Wrapping rather than splicing is the correction of a real failure mode in the `cleattest`
version: splicing a key requires the payload to *be* a JSON object, and for a bare scalar, an
array, or an empty string that code silently sent **no correlation ID at all** — so the receiver
had nothing to reply to and the sender waited out its whole timeout with no error anywhere.
`TestSignalEnvelopeRoundTripsAnyPayload` covers those shapes.

The decoder requires **exactly** the two keys with a non-empty address, because auto-stripping
offers every inbound payload to it: an over-matching decoder would hand a receiver a truncated
payload plus an address pointing at no promise. `TestSignalEnvelopeDoesNotMisreadAnOrdinaryPayload`
is the negative control, and it earns its place — deleting the key-count check alone makes
`{"cleat_reply_to":"p1","payload":"x","extra":1}` read as an envelope, silently discarding
`extra`. It also covers a payload that merely *mentions* the key, which is this file's recurring
lesson: a text match cannot tell a thing from a sentence about the thing. The discriminator is
the object's shape, not the presence of a string.

#### What the guards caught, in the direction that matters

Two guards from #820/#823 failed on this change, both on their **second** direction — the one
that reports a check no longer describing anything:

  * `TestEveryClosureBackedMethodIsWiredOrTracked` reported `unwiredClosureMethods` as having two
    entries that no longer describe an unwired method, and named both. The list is now empty.
    Its own comment predicted this: *"delete this entry when 3.220 lands."*
  * `TestEveryCompositeHostCallHasAnImportRow` reported all four new call edges —
    `SendSignalAndWait → CreatePromise / SignalWorkflow / AwaitPromise` and
    `ReplyToSignal → ResolvePromise` — as reaching imports no `compositeRequires` row granted.
    Without those rows a guest calling only `SendSignalAndWait` would compile with the imports
    missing, which is §3.234's defect (`h.NowMs()` → epoch 0) exactly.

Neither is a failure this change would have found by testing; both were found by a guard failing
because its exemption stopped being true. That is the case for writing the remedy into the
failure message.

#### A stale mock caught on the way

`cleattest`'s `resolvePromiseImpl` returned nil unconditionally, under a comment reading *"Matches
the engine: engine/promises.go logs rather than returns a store error, and the store's UPDATE
matching no rows is not an error in SQL. A harness that failed here would let a test assert a
failure mode production cannot produce."* Both halves were true when written; **#818 made the
"failure mode production cannot produce" the documented one**, and `engine/promises.go:280` turns
any store error into `packSimpleResult(1, 0)`. The mock was left more permissive than production,
held there by a test named `TestResolvePromise_UnknownIDIsNotAnError`. Both are inverted, and the
public `TestEnv.ResolvePromise` driver keeps the no-op contract with a paired test saying so, so
the two paths cannot be confused again. `cleat/embedded` had no way to settle a promise at all —
`CreatePromise` and `AwaitPromise` and nothing else — so a promise created there could only ever
time out; `ResolvePromise`/`RejectPromise` are now wired.

This is the §3.218 shape once more: my own fix changed the engine, and the mock that models the
engine was not brought along. It surfaced only because a reply address became a promise ID, which
made "a stale reply address" and "an unknown promise" the same case.

#### "Byte for byte" was wrong, and the property that matters had no test

The Go, Rust and Python envelope modules each claimed the three encoders "must agree byte for
byte". They do not. `encoding/json` HTML-escapes `<`, `>` and `&` by default; `serde_json` and
`json.dumps` do not, so one payload takes two shapes:

    GO={"cleat_reply_to":"p1","payload":"{\"q\":\"a\u003cb\u0026c\u003ed\"}"}
    PY={"cleat_reply_to":"p1","payload":"{\"q\":\"a<b&c>d\"}"}

The pinned literal contains no HTML characters, so it passed and the claim went unchecked. A
payload with `&` in it -- a query string -- differs between a Go sender and a Python one.

**Interop is not broken**: both decode to the identical string, because the consumer is a JSON
parser. So the pin guards STRUCTURE -- key names, key order, compact separators, which is a real
Python hazard since `json.dumps` defaults to `", "`. It is not byte identity, and describing it as
such sends the next reader chasing a difference that is correct.

The property cross-language request/reply actually depends on -- *every decoder accepts every
other encoder's output* -- **had no test in any SDK**. It does now, in all three, with both forms
pinned and an `assert_ne` on the pair: without that, the test would still pass if the escaped
literal had been written unescaped by mistake, since both would decode fine and nothing about
escape handling would be proved. That is the known-positive rule from CLAUDE.md applied to a
fixture rather than to a guard.

**How the difference was nearly lost.** The first attempt to display Go's bytes used
`echo "$(cat file)"`, and zsh's `echo` interprets `\u003c`, rendering it as `<` -- which made Go's
output look identical to Python's and contradicted a correct earlier measurement. The file was
right throughout; the display was not. Resolved by comparing `encodeSignalEnvelope` against
`json.Marshal` directly rather than trusting either rendering. Same rule as the rest of this
section: a tool applied to a format it does not model.

#### A defer-segment orphan the composition introduces

Composing costs one thing the single host call did not. `SendSignalAndWait` in a defer segment now
creates the reply promise **before** the refusal: of its three callees only `SignalWorkflow` calls
`stopBeforeNewWork`, so `CreatePromise` succeeds, the send is refused, and the composite returns an
error having left a pending promise row nobody will ever await. The old host call was refused
before any state changed.

    for f in CreatePromise AwaitPromise ResolvePromise SignalWorkflow; do
      awk "/func \(s \*execSession\) $f\(/,/^}$/" engine/promises.go engine/signaller.go \
        | grep -c stopBeforeNewWork; done
    # 0 0 0 1

It is garbage rather than corruption — a row in `workflow_promises` that stays `pending` — and the
ordering cannot simply be swapped, because the envelope needs the promise ID before the send. The
cheap fix is to reject the promise when the send fails, which works even in a defer segment
precisely because `RejectPromise` is not refusable. Not done in either SDK yet; it is a small
change and it should land in both at once, since the behaviour is identical in Go and Rust.

#### Not done here

**All five SDKs now compose it**, as of 2026-09-06: Go (#825), Rust (#828), Python (#831), Java
(#834) and AssemblyScript. Rust dropped its `extern`s, Java its `@Import`s and AssemblyScript its
`@external`s, so **no guest imports either name any more**. Python still declares WIT bindings for
both and simply no longer calls them; removing those means editing generated `_wit/` bindings,
`wit/cleat.wit` and `WitToEnvImport`, so it belongs with the export removal.

Python's WIT declarations went on 2026-09-06 as well, so **no guest of any language imports either
name now**. The bindings were not hand-edited: `python-sdk/cleat_sdk/_wit/` says "not intended for
manual editing", so the two functions were removed from `wit/cleat.wit` and componentize-py was
re-run in Docker against the edited world. The regenerated `durable_signals.py` differs from the
committed one by **exactly the two deletions and nothing else**.

That last part is narrower than it looks. No available componentize-py reproduces the committed
bindings: 0.13/0.16/0.17/0.18 cannot parse the current world at all (they reject
`backoff-coefficient-100x`), and 0.19/0.20/0.25 each add a `Raises:` docstring line — a 35-line
diff across 7 files. That docstring belongs to `durable-send-signal-and-wait`, the only
`result<...>` function in the interface, so it leaves *with* the function and regenerating this one
file is clean. Regenerating the whole tree would have been a toolchain bump wearing a signal change.

**The engine's two exports are gone as of 2026-09-06**, which closes this item. The precondition
was every SDK dropping the import first, because a module importing a name the engine does not
export fails at *instantiation*, not at the call. Exports went from 52 to 50, and `ABI.md` moved
with them — both give 50 with an empty set difference. ABI 2.14 and 2.15 are marked removed rather
than reused, so an older document's "2.16" still means `cleat_signal_workflow`.

Removing them took out: the two `.Export` registrations, their wasmtime `hostFunc` registrations,
the two `execSession` methods, two `HostFunctions` interface entries, two Component Model
dispatchers with their `cbType` constants and callback-table rows, and eleven tests of the removed
behaviour. Two guards that had been *carrying* the pair reported themselves stale and were removed
by name — `sdkStopSiteExemptions` for all three SDKs and `stopSurfaces["SendSignalAndWait"]` —
each of which had said in its own text that it would go when the export did. The engine's stop-site
count fell 16 → 15.

#### The crash that reported four failures

Removing the pair segfaulted the engine test binary, and the interesting part is what that looked
like: `go test ./engine/` reported **4 failures**, which is a plausible number for a change this
size. It had run **676 of 2978 tests** before dying, so 77% of the package was never measured.

The cause was a real defect the removal walked into. `cgotestDispatchStr` guards a missing
dispatcher —

    if dispatch == nil {
        return fmt.Errorf("cgotestDispatchStr: no dispatcher for method %d", method)
    }

— under a comment saying *"a missing key yields a nil func value, and calling it segfaults rather
than failing a test … an unknown method must be an error, not a crash."* Its twin
`cgotestDispatchU64` had no such check, and `cleat_reply_to_signal` was index 18 in **that** map.
The comment stated the rule for both; only one obeyed it. `cgotestDispatchU64` now has the guard.

The lesson is not "add nil checks". It is that **a crash in a test helper does not fail a test, it
stops measuring** — and the truncated run still exits 1 with a believable failure list, so it reads
like an ordinary red rather than a stopped one. After the fix the same command ran 4587 tests. If a
failure count looks small for the size of a change, check how many tests ran.

Every import removal was verified by diffing the **sets**, never the counts —
`removed: [cleat_reply_to_signal, cleat_send_signal_and_wait]`, `added: []` — and each SDK's floor
in `tests/plugin-harness/sdk_import_names_test.go` moved only after that check. Rust and Java went
45 -> 44; **AssemblyScript's floor did not move and should not**, because that SDK declares more
imports than the others (49 -> 47) and 47 still clears 45. A floor exists to catch the extractor
breaking, not to freeze a count.

#### A falsification that stays green because the code is REDUNDANT, not because the test is weak

Porting to AssemblyScript produced a new variant of the "it stayed red / it stayed green" rules
above. Removing `if (obj.objKeys.length !== 2) return null;` from `decodeSignalEnvelope` left the
whole AS suite green. So did removing the `else { return null; }` that rejects an unknown key.
Removing **both** failed, with `a non-envelope was read as an envelope`.

Neither line is dead and the test is not weak: the two guards independently cover the same case,
so a one-line falsification can never move it. The reading "it stayed green, so that line does
nothing" would have deleted a real guard — the same wrong repair the fence-predicate case warns
about, arrived at by a different route. **Before concluding a line is inert, check whether another
line covers the case you are testing with.**

This is worth separating from the Java result on the same day, which looked identical and was not:
there, deleting the key-count check left 288 tests green because Java had **no negative control at
all**. Same symptom, opposite cause — redundancy in one, absence in the other — and only writing
the missing test told them apart. Removing them is a separate change with the §3.216 shape (SDK imports first, then
the engine export — a module importing a name the engine does not export fails at
instantiation, not at the call). Until then the ABI is unchanged and those four SDKs keep the
inert behaviour described above.

### 3.221 Every child result was lost to a brace scan that did not model strings — 🟢 **FIXED 2026-09-05** (WS-1, 2026-09-05)

Reported by the conformance-port session as #778: `AwaitAllChildren` returns a `ChildResult` per
child with the right `RunID`, an **empty** `Result`, no error, and a workflow that reports success.
`AwaitAnyChild` returns the same children's results correctly.

**It is not in the engine.** `parseChildResultArray`, emitted into every Go guest from
`wasm/adapter_component.go`, split the array into objects with

    open  := strings.Index(json[i:], "{")
    close := strings.Index(json[i+open:], "}")

A child's result is itself a JSON object, so the outcome the engine marshals is

    {"run_id":"child-a","result":"{\"tag\":\"child-0\"}"}

and the first `}` after the opening brace is **the escaped one inside the result value**. Run
verbatim on that input rather than read:

    obj[0] = {"run_id":"child-a","result":"{\"tag\":\"child-0\"}

Each object is cut before the closing quote of its own `result`. `extractJSONString` then reads an
unterminated string and returns `""` — correctly; it is escape-aware and was never the problem.
`run_id` survives only because it sits before the cut. An error message containing a brace was
dropped the same way, which is worse: the one thing that could have reported the failure.

`AwaitAnyChild` is unaffected because it returns a **single** object and never splits — same engine
read (`GetChildResult`), same bytes, different parser. That asymmetry is what made it look like one
await path being broken.

**The host half was verified separately rather than inferred from the guest symptom.**
`TestAwaitAllChildrenCarriesEachChildsResult` drives `freshAwaitAllChildren` against a store with
three completed children and asserts the recorded event's `Response` carries each payload; it
passes, and goes red on all three when the fake store returns `("", true, nil)`. The `Response` is
the assertion target because it is the durable artifact — `replayAwaitAllChildren` hands those same
bytes back on every future replay.

**Why a name scan could not see it, which is the transferable part.** The existing coverage was

    TestWriteManualJSONHelpers:  strings.Contains(code, "func parseChildResultArray")

satisfied by a function that is emitted and wrong. There was also no engine-level test over
`AwaitAllChildren` with completed children at all: the coverage was dispatch and linker
registration — that the call is *reachable*, not that it *answers*. Same shape as §3.215(d), where
a store method was implemented in four dialects, tested, and called by nothing.

The replacement, `wasm/adapter_json_helpers_exec_test.go`, **compiles and runs** the emitted
helpers. It substitutes only the `cleat.ChildResult` type name so the throwaway program needs no
dependencies — the parser bodies are byte-identical to what a guest gets — and it fails loudly if
the helpers ever reference anything else in that package.

**Scope checked rather than assumed**: only the Go SDK hand-rolls this. Rust's
`await_all_children` returns the raw JSON string, and the AssemblyScript extern does too, so both
leave parsing to a real decoder. `python-sdk`'s is on the local-host path, not the guest. One
mechanism in one place, not a sweep.

**Postscript, and it is the same defect one layer up.** The first version of this fix broke every
Go guest build:

    ./gen_host_adapter.go:8:2: "strings" imported and not used

`patchAdapterImports` (`wasm/build.go`) decided whether the generated adapter needs `"strings"` with

    if !strings.Contains(content, "strings.") { return }

and the comment this fix added to `parseChildResultArray` — prose explaining that the code *was*
`strings.Index(json, ...)` — satisfied it. **A retraction read as a use**, in the fix for a scanner
that could not tell a brace from a brace inside a string, written by someone who had spent the day
citing that exact rule. It is now a `go/parser` walk for a `strings.X` selector, because comments
are not in the AST.

The guard's known-positive is `TestPatchAdapterImportsIgnoresProse`: a file whose only mention of
the package is a comment, and a second whose only mention is inside a string literal. Both go red
under the substring check and green under the parser one, while a file with a real call stays green
under both. Neither existing check could have caught it — the generator tests assert on emitted
*text* and never compile it, and the guard's happy path (a file that really does use `strings`) kept
passing the whole time.

### 3.223 The Go SDK's `Scoper` never reaches the host, so it takes no lock — ✅ **FIXED 2026-09-09** (WS-1; documented 2026-09-05, closed by cleat#984)

Started as a stale-comment cleanup and turned into a parity gap. **The first version of this
section was wrong in the flattering direction and is corrected below rather than preserved.**

`docs/reference/sdk-api.md` lists `Scoper` among the SDK interfaces, which prompted "does this
still exist after §3.216 deleted the state family?" It does, and the ENGINE half is entirely live:
`freshSetScope` (`engine/scope.go`) takes a concurrency key `vo:<objectType>:<instanceKey>` and
releases it on clear or replace, with replay bookkeeping split deliberately between releasing and
forgetting.

**So I wrote a comment saying "on a worker the engine takes a concurrency key" — and did not check
that a Go guest can ask it to.** It cannot.

    grep -n "Scope" cleat/runtime.go            # Scoper interface, no HostCallsOptions field
    grep -n "SetScope" wasm/usage.go            # no row in hostFunctions
    grep -n "SetScope" wasm/adapter_metadata.go # no adapter def

`HostCallsImpl.SetScope` sets three local fields and returns. There is nothing to generate a call
to `cleat_set_scope`, so the host is never told and **no lock is taken**. Combined with §3.216
removing the state calls the prefix used to prefix, the Go SDK's three `Scoper` methods are now a
local variable with an interface around it.

**Confirmed by compilation, which is stronger than the greps above.** The conformance-port session
built a Go workflow whose entire body is `h.SetScope(obj, key)` plus one log, and read the produced
binary:

    imports wired:   cleat_complete, cleat_log, cleat_poll_work
    adapter fields:  DurableLog

So it is not merely that the lock is not taken — **`cleat_set_scope` is not in the binary at all**,
and `HostCallsImpl.SetScope` sets its three fields against a host call that was never generated.
The static reading and the compiled artifact agree, which is the pair worth having: the tables say
it cannot be wired, and the binary shows it was not.

**Go is alone in this.** Verified at declaration and call sites, not by name search:

| SDK | binding |
|---|---|
| Rust | `pub fn cleat_set_scope` (`crates/cleat-sdk/src/host_calls.rs:195`), called at `:943`, `:988` |
| Java | `@Import(module = "env", name = "cleat_set_scope")` (`crates/cleat-java/.../HostCalls.java:266`) |
| AssemblyScript | `@external("env", "cleat_set_scope")` (`packages/cleat-as/assembly/host-calls.ts:582`), called at `:2053`, `:2117` |
| Python | a stub only (`python-sdk/cleat_sdk/host_calls.py:3275`) — **not verified as wired** |
| **Go** | **nothing** |

**Fixed 2026-09-09.** `wasm/generator.go` gained `importDefs` entries for both calls,
`wasm/usage.go` the three `hostFunctions` rows (`SetScope`, `ClearScope`, `GetScope` —
`ClearScope` maps to `cleat_set_scope`, since clearing *is* the empty pair),
`wasm/adapter_metadata.go` the two `adapterDefs`, and `cleat/runtime.go` the
`HostCallsOptions` fields. `HostCallsImpl` now calls through when wired.

**Verified with the same instrument that established the gap.** This section concluded
from a compiled binary, not from tables — which matters, because the tables are exactly
what a fix edits, so a table-reading test would pass on a tree where the generator still
emitted nothing. `TestACompiledGoWorkflowImportsTheScopeCalls` builds the fixture and
reads its import section: both `cleat_set_scope` and `cleat_get_scope` are present.

**The obvious behavioural test was vacuous, and it was measured to be so rather than
reasoned about.** Running the fixture and checking `SetScope`/`GetScope`/`ClearScope`
return sensible values passes *with the wiring removed* — `HostCallsImpl` still keeps a
local mirror, so the mirror answers every assertion. That version was written first and
went green on an unwired tree, which is this section's own defect reappearing inside its
fix: local fields standing in for a host call that was never made.
`TestACompiledGoWorkflowActuallyReachesTheHostForScope` asserts the
`EventTypeScopeAcquired` records instead, which only `freshSetScope` can write after it
takes the concurrency key, and that one does go red when unwired.

One thing deliberately **not** changed: `SetScope` still cannot report a host error, so a
concurrency-store failure leaves the local mirror untouched and returns the previous scope.
Rust is identical — `set_scope(...) -> String`, discarding `_err_code` — and diverging in
one SDK would be worse than the shared gap. Tracked separately.

So virtual-object mutual exclusion works from three SDKs and silently does not from the one this
repo's own examples are written in.

**This is an instance of a larger, measured gap.** `cleat build` picks a Go guest's imports by
scanning the user's AST against `wasm/usage.go`'s `hostFunctions` table, so an export with no row
there can never be wired. Re-derive:

    python3 - <<'EOF'
    import re
    u=open('wasm/usage.go').read(); i=u.index('var hostFunctions = []HostFunction{'); j=u.index('\n}', i)
    rows=re.findall(r'\{"([^"]+)",\s*"([^"]+)"\}', u[i:j])
    imports={r[0] for r in rows}
    e=set(re.findall(r'\.Export\("([^"]+)"\)', open('engine/imports.go').read()))
    print(len(e), len(imports), sorted(e-imports))
    EOF

Measured 2026-09-05: the engine exports **52**, the table names **35** distinct imports across
**53 rows** and **51 method names** (many-to-one is normal — `cleat_call` alone serves nine
methods), and **17 exports have no row at all**. Excluding D12's three non-workflow calls, **14
workflow-facing exports are unreachable from a Go guest**: `cleat_fetch`, `cleat_get_scope`,
`cleat_json_parse`, `cleat_json_stringify`, `cleat_reject_promise`, `cleat_reply_to_signal`,
`cleat_resolve_promise`, `cleat_run_detached`, `cleat_schedule_invoke`, `cleat_send`,
`cleat_send_signal_and_wait`, `cleat_set_scope`, `cleat_signal_workflow`, `cleat_uuid`.

**That list is not 14 defects and must not be reported as such.** Some are plausibly
external-only by design — promises are normally resolved by an API caller, not by the workflow that
created them. Each needs triage against whether the SDK exposes a public method for it, which is
the discriminator: `Scoper` is a defect precisely because the method is public, documented, and
inert. Triage is not done here.

**It is the import-space half of the conformance session's #775**, which measured the same
mechanism in method-space and found ten public methods with no row — `h.NewUUID()` returning
`00000000-0000-4000-8000-000000000000` in every compiled workflow because `cleat_random` is never
wired. The two counts are different denominators of one defect and neither subsumes the other:
`NewUUID` is missing from the table as a METHOD, and `cleat_uuid` is missing as an IMPORT.

**Two method notes worth keeping.**

The first count I derived for the table was **53**, from a regex that also matched unrelated
`{"x", "Y"}` pairs elsewhere in the file; a tighter one gave **35**. Both were "right" about what
they matched and neither was the number I wanted, which is *distinct imports*. Parsing the
`hostFunctions` block by its delimiters rather than grepping the file resolved it — and the three
figures (53 rows, 51 methods, 35 imports) are all true of the same table.

And the earlier claim about `cleat/embedded` survives, sharpened: its `setScope` does not touch the
in-memory lock map that its own `AcquireLock` uses.

    sed -n '/func (e \*execution) setScope/,/^}/p' cleat/embedded/runner.go | grep -c 'locks'   # 0
    grep -c 'e\.locks' cleat/embedded/runner.go                                                 # 3

The first command I wrote there was `grep -c concurrencyKey` → **0**, which invited "the runner has
no locking, so of course scope takes none." A looser read found `// lock state (in-memory
concurrency keys)`. **The strict grep flattered the conclusion I was already writing** — the same
direction as the engine-side error at the top of this section, twice in one change.

### 3.224 Seven public Go SDK methods compile to nothing, and the build says OK — 🟡 **1 FIXED, 1 CLEAN, 5 NEED DECISIONS 2026-09-06** (WS-1, 2026-09-05)

§3.223 measured that **17 of the engine's 52 exports have no row** in `wasm/usage.go`'s
`hostFunctions` table, and said explicitly that the number was **not** a defect count and that the
triage was undone. This is the triage, and it splits 7 / 7.

## Proven by compiling, not by reading the table

A probe workflow whose entire body is four calls:

    h.DurableLog("probe start")
    h.SignalWorkflow("00000000-...-0001", "ping", "{}")
    h.RunDetached(func(hh cleat.HostCalls) error { return nil })
    h.SetScope("obj", "key")

built with `cleat build --target go`. The output:

      Generating WASM imports (2 host functions used)... OK
      ...
      Wrote /tmp/probeout/probe.wasm (3.1 MB)

    gen_wasm_imports.go:  cleat_log, cleat_complete, cleat_poll_work
    gen_host_adapter.go:  one field, DurableLog

`cleat_complete` and `cleat_poll_work` are the wasip1 handshake, so **one** of the four calls was
wired. `SignalWorkflow`, `RunDetached` and `SetScope` produced no import, no adapter field, and no
diagnostic. **The build succeeded.** The only warnings it printed were about the two handshake
imports being present and *not* in the computed closure — noise pointing the opposite way from the
actual problem.

## The triage: 7 of the 17 are not defects

| import | why it is fine |
|---|---|
| `cleat_json_parse`, `cleat_json_stringify`, `cleat_send`, `cleat_resolve_promise`, `cleat_reject_promise` | no public `HostCalls` method exists for them at all — nothing can be broken |
| `cleat_fetch` | `DurableFetch` and `FetchGet` are wired, to `cleat_call`. The fetch import is a path this SDK does not take |
| `cleat_uuid` | `UUID(seed)` is computed **locally** in the Go SDK, and the algorithm is identical to the host's — same `workflowID + ":" + seed`, same SHA-256, same version/variant bits, same format string (`cleat/runtime_workflow.go:189` vs `engine/lifecycle.go:223`). Duplication, not divergence |

`cleat_uuid` is worth dwelling on, because I predicted it was the worst of the 17 and it turned out
to be one of the harmless ones. I had linked it to `h.NewUUID()` returning the all-zeros UUID
(#775). Wrong: `NewUUID` goes through `Random()` → `cleat_random`, which is what #786 fixes.
`cleat_uuid` serves the *seeded* `UUID(seed)`, which never calls the host. **The two were unrelated
and I connected them because both had "uuid" in the name.**

## The other 7 are real, and each has a public, documented method wired to nothing

| import | method | note |
|---|---|---|
| `cleat_signal_workflow` | `SignalWorkflow` | **the worst of the seven** — see below |
| `cleat_run_detached` | `RunDetached` | fire-and-forget child execution, unreachable |
| `cleat_schedule_invoke` | `ScheduleInvoke` | unreachable |
| `cleat_set_scope` | `SetScope` | §3.223 |
| `cleat_get_scope` | `GetScope` | §3.223 |
| `cleat_send_signal_and_wait` | `SendSignalAndWait` | also inert engine-side (§3.220) |
| `cleat_reply_to_signal` | `ReplyToSignal` | also inert engine-side (§3.220) |

**`SignalWorkflow` is the worst because the engine implements it fully.** It is the one signalling
path that does call `DeliverSignal` (`engine/signaller.go:304`) — the half of §3.220 that is *not*
broken. So the engine can deliver a signal from one workflow to another, and a Go workflow cannot
ask it to.

The last two are broken at both ends: unreachable from the guest **and** inert in the engine. Fixing
either end alone changes nothing observable, which is worth knowing before someone starts.

## Go is the only SDK missing these

Verified at declaration and call sites rather than by name count — Rust `pub fn` externs with call
sites, Java `@Import`, AssemblyScript `@external`:

    crates/cleat-sdk/src/host_calls.rs:188   pub fn cleat_signal_workflow(   (called :918)
    crates/cleat-java/.../HostCalls.java:185 @Import(module="env", name="cleat_signal_workflow")
    packages/cleat-as/.../host-calls.ts:328  @external("env", "cleat_signal_workflow")

Rust, Java and AssemblyScript bind all seven. **The gap is Go's alone** — which is the opposite of
the assumption most people bring, since Go is the language this repo's examples are written in and
the one `--target go` is the default for.

## Why this survived

The host-call execution harness covers `wave1Calls` — 24 method names resolving to 23 imports
(`tests/plugin-harness/hostcall_harness_test.go:37`). Every one of them is in the table, so every
one of them wires. **The harness cannot find a call that is missing from the table, because it only
runs calls that are in it.** These seven sit in the untested remainder, and nothing distinguishes
"we have not got to it yet" from "it does not work".

## Fixing one takes FOUR pieces, not three

This section said three — a `hostFunctions` row, an `adapterDefs` entry, and the SDK's
`HostCallsOptions` field plus delegation. **That is wrong, and adding only those produces a guest
that does not compile:**

    ./gen_host_adapter.go:21:14: undefined: cleatSignalWorkflowImport

`GenerateImports` emits the `//go:wasmimport` declarations from a **third** table — `importDefs` in
`wasm/generator.go` — and a field whose import has no entry there generates a call to a function
that was never declared. So:

| piece | file | what it does |
|---|---|---|
| `importDefs` | `wasm/generator.go` | declares the `//go:wasmimport` stub |
| `hostFunctions` | `wasm/usage.go` | makes the AST scan request that import, and names the field |
| `adapterDefs` | `wasm/adapter_metadata.go` | emits the closure that calls it |
| `HostCallsOptions` + delegation | `cleat/runtime.go` | lets the SDK method reach the closure |

The fourth was already present for all seven, which is why they look like table omissions rather
than missing features. Found by doing it: #790 wired `SignalWorkflow` and hit the missing
`importDefs` entry on the first build.

## Triage of the seven, after attempting them

Three of the seven are **not** recipe cases, and finding that out is why the recipe should be
attempted per method rather than applied in bulk.

**`SignalWorkflow` — fixed (#790).** Clean: SDK signature matches the ABI exactly. Before, a guest
whose only host call was `h.SignalWorkflow(...)` imported nothing but the wasip1 handshake.

**`ScheduleInvoke` — clean, not yet done.** `ScheduleInvoke(service, operation, requestJSON string,
delayMs int64) error` against `cleat_schedule_invoke: (ptr,len x3, i64) -> i64`. Signature matches,
error return exists, nothing to decide.

**`RunDetached` — NOT a wiring omission. It is a signature mismatch, and the SDKs disagree about
what the feature is.**

    Go SDK    RunDetached(fn func(h HostCalls) error) error        cleat/runtime_workflow.go:269
    engine    cleat_run_detached(name, inputJSON)                  engine/imports.go:735
    Rust SDK  run_detached(name: &str, input_json: &str)           crates/cleat-sdk/.../host_calls.rs:1131

A closure cannot cross the WASM ABI, so the Go method **cannot** be wired to that import at all —
and its unwired branch is `return nil`, a silent success. Worse, the Rust method's doc comment says
*"Mirrors Go's RunDetached"*, which it does not: it takes a name and input, and Go takes a
function. Anyone porting between the two reads that comment and is misled.

Fixing it is a public API decision — almost certainly changing Go's signature to `(name,
inputJSON)` to match every other SDK, which is a breaking change to an exported method. **Not a
table row, and not to be done silently.**

**`SetScope` / `GetScope` — the signature cannot express the outcomes.** `freshSetScope`
(`engine/scope.go`) has three: success, an error from the concurrency-key store
(`packSimpleResult(1, 0)`), and — when the scope is **held by another workflow** — a *suspension*,
with a five-second retry. The SDK method is

    SetScope(objectType, instanceKey string) (previousScope string)

No error return. So wiring it as-is would silently swallow a store failure, and the "held by
another workflow" case is the *normal* one for a mutual-exclusion primitive — it is what the
feature is for. **The store-failure half of that is closed by §3.407** — not by changing the
signature, but by having the host suspend, as it already did for contention, so the swallowed
errCode no longer decides anything. The signature question remains open and is now only about
whether a guest should be *told*. Also a signature change, and it needs deciding alongside §3.223's question of what
scope means in this SDK at all.

**`SendSignalAndWait` / `ReplyToSignal` — blocked on §3.220.** Both are inert engine-side too, so
wiring the guest half alone changes nothing observable. §3.220 needs a reply protocol decided
first.

**So the seven are: one fixed, one clean and pending, two needing a public API decision, one pair
needing a design decision, and two blocked.** The count in this section's title was right about
what compiles to nothing; it was silent about the fact that fixing them is four different kinds of
work.

### 3.225 Nothing compiled the generated adapter, and an eighth method turned up when something did — 🟢 **GUARD ADDED 2026-09-06**; the method it found was closed by #786 (WS-1, 2026-09-06)

Two failures on 2026-09-05, hours apart, both one-line compile errors in generated code, both
caught only by CI:

  * **#780** — a comment mentioning `strings.Index` made `patchAdapterImports` inject an import
    nothing referenced; every guest build failed with `"strings" imported and not used`.
  * **#786** (conformance-port session) — a wrapper row invented a closure field carrying a
    parameter the inner import's body never reads; **eleven** CI jobs failed with
    `declared and not used: heartbeatIntervalMs`.

**Neither was visible to a unit test, and #786's own new unit test passed throughout.** It asserted
that each wrapper ends up with the right *import* — true after the broken change; the *field* was
the problem. Third instance in one day of a guard measuring the legible half of a two-halved thing
(cf. §3.223's engine-versus-guest, and the port session's payload-carriage-versus-column-persistence
in #777).

The generator's tests assert on emitted **text** — `strings.Contains(code, "func parseChildResultArray")`
— and the emitted file is only really checked by *building a guest*, which happens in integration
jobs, for one fixture, using whichever calls that fixture happens to make.

`TestEveryAdapterDefCompiles` (`wasm/adapter_compiles_test.go`) closes that: it builds a synthetic
`UsageInfo` naming **every** `adapterDefs` and `hostWrapperDefs` entry, runs the real
`PrepareBuildDir`, and compiles the result for `wasip1`. Covering every def rather than a fixture's
subset is the point — the failure mode is one field in isolation, so a fixture that does not use
that field cannot see it. Known-positive: giving `DurableLog`'s def a parameter its body never
reads turns it red (with a signature mismatch rather than an unused-variable error, since that
field's type is pinned by `HostCallsOptions` — either way, generated code that does not compile).

## What it found on its first run

`DurableCallTypedWithOptions` has an adapter definition and **no `hostFunctions` row**, so no build
can ever emit it. Verified the way §3.224's seven were — by compiling a workflow whose only host
call is that method:

    Generating WASM imports (0 host functions used)... OK
    gen_wasm_imports.go: cleat_complete, cleat_poll_work

Zero. Only the wasip1 handshake. It is public (`cleat/runtime.go:64`), has a `HostCallsOptions`
field, a `HostCallsImpl` method, a `hostWrapperDefs` entry, and appears in `cleat/localdev`.

**That makes it an eighth instance of §3.224, and the worst of them.** The other seven are
signalling, scoping and scheduling. This one is a **durable call** — the operation the system
exists to provide. A durable call that silently does not happen leaves the workflow proceeding as
though the external effect occurred.

## Closed by #786, and the assertion is live

`compositeRequires` (added by #786) covers `DurableCallTypedWithOptions`, so the call now happens.
Proven on the branch by the port session, not recalled:

    before:  Generating WASM imports (0 host functions used)  -> cleat_complete, cleat_poll_work
    after:   Generating WASM imports (14 host functions used) -> cleat_call, cleat_call_retry,
                                                                 cleat_sleep, ...

The guard's reachability check is therefore an assertion (`t.Errorf`), not a report — but it had to
learn to read **both** tables first, and the distinction is the one #786 exists to make.
`hostFunctions` is bidirectional: a row requests an import **and** emits a field named `FieldName`
implemented by that import's body. `compositeRequires` only requests imports, for SDK-level
wrappers that must *not* get a field. **Checking `hostFunctions` alone reported
`DurableCallTypedWithOptions` as unreachable when it is reachable through the second table** —
so the first version of this assertion would have been a false positive on a fixed tree.

Falsified by deleting `{"cleat_log", "DurableLog"}` from `hostFunctions`: red, naming `DurableLog`.

**It is fixed by wiring the fallback, which is not the same as fixed properly, and the difference is
a live determinism defect that #786 made reachable.**

`HostCallsImpl.DurableCallTypedWithOptions` (`cleat/runtime.go:1177`) checks its own
`HostCallsOptions` field first and only falls through to an SDK implementation over
`DurableCallWithOptions` when that field is nil. `compositeRequires` wires the **fallback's**
imports; it does not emit the field. **Measured, on a guest compiled from a workflow whose only
host call is that method with a `Timeout` set:**

    Generating WASM imports (14 host functions used)... OK
    imports:  cleat_call, cleat_call_retry, cleat_sleep, cleat_complete, cleat_poll_work
    grep -c DurableCallTypedWithOptions gen_host_adapter.go   ->  0

Zero. No field is emitted, so `h.durableCallTypedWithOptions` is nil in every compiled Go guest and
**the fallback is the path that runs.** That fallback is:

    ch := make(chan callResult, 1)
    go func() { ... h.DurableCallWithOptions(...) ... }()
    select {
    case r := <-ch:                     // the durable call finished
    case <-time.After(opts.Timeout):    // WALL CLOCK, inside a workflow
        return &CallTimeoutError{...}
    }

The durable call records its event whichever branch wins, because the goroutine makes it before the
select resolves. **So the original run and the replay can take different branches**: on replay the
call returns from history immediately, `<-ch` wins, and the workflow receives the response where the
original received `CallTimeoutError`. Different branch, different subsequent host calls.

**One of two things is true and which one is not measured.** Either the timer can preempt a
blocking `//go:wasmimport` under wasip1 — in which case the divergence above is real — or it
cannot, in which case `opts.Timeout` never fires and is **silently ineffective**. Both are defects;
this section does not claim to know which. What it does claim, and has measured, is that the
fallback is what a Go guest runs.

Note the direction: this was harmless while the method was unreachable (§3.224's eighth instance).
**Making it reachable is what made the hazard live**, which is the ordinary cost of fixing a wiring
gap and an argument for emitting the field rather than wiring the fallback. Emitting it is a
`hostFunctions` row plus an `adapterDefs` entry — the §3.224 recipe — and removes the question
entirely, because the direct path has no goroutine and no `time.After`.

`cleat vet` will not help: its rules scan the user's workflow code, and this `go` statement is in
the SDK. That is a **scope** the tool does not have rather than a rule it is missing, and it is the
second instance of the same limit — E003 told authors to use `h.Now()` for deterministic time and
could not see that `h.Now()` was itself the broken clock (#776, #787).

## It is not one obscure method: `DurableCallWithOptions` is the same, and it is mainstream

Checked because the fix looked like a one-line recipe and the port session asked what would enforce
`opts.Timeout` afterwards. **The answer widened the defect rather than the fix.**

`HostCallsImpl.DurableCallWithOptions` (`cleat/runtime.go:1013`) has the identical shape — field
check first, then a fallback that spawns a goroutine and selects it against
`time.After(opts.Timeout)`. And it is in `hostWrapperDefs`, not `adapterDefs`, so **no field is
emitted for it either.** Measured on a guest whose only host call is
`h.DurableCallWithOptions(CallOptions{Timeout: 5 * time.Second}, ...)`:

    Generating WASM imports (5 host functions used)... OK
    emitted adapter fields:  DurableCallWithRetry, DurableSleep, DurableSleepMs

No `DurableCallWithOptions`. So the fallback runs for **every Go workflow that calls
`DurableCallWithOptions` with a timeout** — a documented, mainstream API, not the obscure typed
variant this section started from. `DurableCallJSONWithOptions` is in the same map and worth
checking the same way.

## So do NOT emit the field — the ABI has nowhere to put the deadline

Emitting a field for these delegates to `cleat_call` or `cleat_call_retry`, and **neither carries a
per-call deadline**:

    cleat_call        (svc, op, req, resp)                                    engine/imports.go:158
    cleat_call_retry  (svc, op, req, maxAttempts, initialIntervalMs,
                       backoffCoefficient100x, maxIntervalMs,
                       nonRetryableErrorsJSON, resp)                          engine/imports.go:324

Retry policy, not a deadline. `cleat_await_signals` is the only call in this family that takes a
`timeoutMs`. So emitting the field would **silently drop `opts.Timeout`** — trading a determinism
defect for a quieter dropped-option one, which is worse.

The real fix is a host-side deadline, and **it is a semantics decision before it is a signature
one** — a distinction worth stating because "add a `timeoutMs` parameter" looks like the whole job
and is not. `cleat_await_signals` already carries `timeoutMs`, so a deadline can cross the ABI
today; the open question is what it *means* on the host. A durable call's timeout has to be
**replayable**: the second execution must reach the same verdict as the first, or the divergence
this section is about has simply been rebuilt somewhere new. That points at recording the timeout
*outcome* in the call event rather than re-evaluating a deadline against a fresh clock.

**And the guest-visible clock is not currently fit for it.** Measured by the conformance-port
session while writing the replay-determinism test: `h.Now()` does **not advance across a
suspension** — 3ms observed across a 3000ms sleep, because it returns the previous event's
timestamp and the sleep event is stamped when the sleep *begins*. A host-side deadline needs a clock
that moves; anything built on the current one would measure the wrong interval. Related, and also
still open after #787: `seedNowMs` takes the session seed from `replayHistory[0]` on a resume and
from the wall clock on a first execution, so a workflow whose **first** action reads the clock still
diverges (109ms measured). #787 fixed events after the first, which is what made the replay test
possible; it did not close that leg.

**Recorded here rather than attempted**, because it is a different size of change from §3.224's
seven and needs those two answers first.


## Resolved 2026-09-13: the option was removed, and the unmeasured half is now measured

"One of two things is true and which one is not measured" above has an answer. **`opts.Timeout`
never fires.** The timer cannot preempt a blocking `//go:wasmimport` under wasip1, so the
divergence this section worried about was never reachable -- the defect was the quieter one, a
silently ineffective option, which is cleat#1006.

Measured with one slow call and one short timeout, each row carrying a control that proves the
delay was really in the path:

| path | `Timeout` | `StartToCloseTimeout` |
|---|---|---|
| compiled WASM (`cleat build --target go`), 2s call vs 50ms timeout | inert, returned at 2.00s | inert |
| `cleat/cleattest`, 400ms call vs 20ms timeout | inert, returned at 400ms | inert |
| `cleat/localdev`, 400ms call vs 20ms timeout | inert, returned at 401ms | inert |
| hand-built `HostCalls`, `DurableCallWithOptions` nil | **fires at 21ms** | **fires at 20ms** |

The last row is the positive control and it is `cleat/runtime_test.go` and nothing else, which is
why every test of this field was green for as long as the field existed. Note the two middle rows:
`cleattest` and `localdev` are the two things that DO populate the import this section is about,
and both discard the option -- `cleattest` takes it as `_`, `localdev` reads only `Retry`. So
emitting the adapter field would not have been the fix either; the option had no honest reader
anywhere.

Both fields are removed as of cleat#1006, along with `CallTimeoutError`, which nothing could
produce once they were gone. A host-reported timeout still arrives, as `*CallError` with
`Code == CallErrorTimeout`.

**What survives from this section is its conclusion, not its worry.** A real per-call deadline is
still a semantics decision before a signature one, still needs a replayable outcome recorded in the
call event, and still needs a guest clock that advances across a suspension. Removing the field
does not build any of that -- it stops claiming it.



### 3.227 `ci-check.sh` had a blocking step that could never pass, so it always exited 1 — 🟢 **FIXED 2026-09-06** (WS-1, 2026-09-06)

Found by finally running it, after a day in which **six separate scope gaps** cost a CI round each
— `go vet` not seeing `testdata/`, `cargo check` being lib-only, `pytest` not being the linter,
`ruff` never run, a `\b` grep that cannot match after an underscore, and `gofmt` not being covered
by build-vet-test. The conclusion recorded at the time was *"the defence is a single local script
mirroring the lint job, not six commands someone remembers."*

**That script already existed.** `scripts/ci-check.sh` runs gofmt, `go vet`, `ruff`,
`cargo clippy --all-targets`, `pytest`, `npm test` and the cargo builds — exactly the six. The
problem was never that it was missing.

    run_step "cleat vet (go) ./... (blocking)" \
        go run ./cmd/cleat vet --lang go --json ./...

`./...` from the repo root has **no workflow entry points** — the root module is the engine, not a
workflow — so this printed

    Error loading package: no workflow entry points found in ./...

on every run, and being **blocking**, took the whole script to exit 1. So anyone who ran it saw it
fail immediately on a step unrelated to their change, concluded it was broken, and stopped.

**This is the second time this file has rotted into always-failing**, and its own header describes
the first: until 2026-08-09 it tested `./durable/...` and built `crates/durable-*`, five steps
pointing at paths that no longer existed, *"so it exited 1 at the first test step and had done for
months, while its header claimed to run the full pipeline."* The preflight added then catches a
path that **disappears**. It cannot catch a target that was **never valid**, which is what this
was — and the distinction is the transferable part: rot-detection that watches for things going
away is blind to things that never worked.

Fixed by pointing it at `./testdata/basic`, the smallest package with real entry points, which
vets clean (3 entry points, 9 durable leaves, OK). A second step,
`shellcheck scripts/*.sh benchmarks/*.sh`, failed on a glob matching nothing — there are no shell
scripts under `benchmarks/` — so it reported a filesystem error rather than a finding.

**What is still not clean, recorded rather than changed:** on a machine without `pip`, `ruff` or
`shellcheck` the script still fails those steps with exit 127. That is arguably right — install the
tools — but combined with the blocking defect above it is why the script was unusable, and a reader
cannot tell "tool absent" from "check failed" in the summary. Whether a missing toolchain should
report as a distinct outcome is a design question about someone else's script, and this repo is
strict about skips for good reason, so it is left as an observation.

**And a finding on the way, not pursued:** `cleat vet ./examples/dag` **fails** with three errors
of the form *"extractText is reachable from a workflow entry point (it calls durable SDK methods)
but does not have a HostCalls parameter."* A shipped example that the project's own linter rejects
is either a broken example or a false positive in `cleat vet`, and which it is has not been
established here.

### 3.228 Six of the eight Go example workflows do not build — 🟢 **ALL EIGHT BUILD, AND A TEST NOW SAYS SO 2026-09-06** (WS-1, 2026-09-06)

Found by pulling on §3.227's loose end: `ci-check.sh`'s vet step failed, and one of the things it
could have been pointed at was `./examples/dag`, which turned out to fail too.

Swept every example directory containing `.go` files, each into a **fresh** output directory:

| example | `cleat build --target go` |
|---|---|
| `saga-temporal-port` | OK |
| `subscription` | OK |
| `datapipeline` | returns `*PipelineResult`; an entry point must return a string |
| `onboarding` | returns `*Profile`; an entry point must return a string |
| `travel` | returns `*BookingResult`; an entry point must return a string |
| `dag` | `Verifying HostCalls threading... 4 error(s)` — **not an example defect**, see §3.229 |
| `fooddash` | `Verifying HostCalls threading... 1 error(s)` — **not an example defect**, see §3.229 |
| `event-driven` | `E003: time.Now() ... breaking determinism` |
| ~~`third-party-plugin`~~ | excluded — it is a plugin, not a workflow, so "no entry points" is correct |

**Two of eight.** Three distinct causes — and this section said **all six are defects in the
examples**, which is wrong about two of them. Four are: three struct-pointer returns (#801) and one
`time.Now()` (#802), all now fixed. `dag` and `fooddash` turned out to be **false positives in the
threading check**, corrected in §3.229. Kept here as written rather than quietly edited, because
the mistake is the same one this file keeps recording: a category assigned to a group after
checking some of it.

## Cause 1: three examples return a struct pointer, and an entry point must return a string

**The string return type is deliberate, not a codegen limitation.** It is what works across all
five language SDKs — a WASM entry point hands back bytes, and `string` is the one shape every SDK
can express identically. So this is an **example defect**, not a missing feature, and the three
failures line up exactly with it:

| example | entry point | builds |
|---|---|---|
| `saga-temporal-port` | `TransferMoney(h, TransferDetails) error` | **yes** |
| `subscription` | `ManageSubscription(h, SubscriptionInput) (string, error)` | **yes** |
| `datapipeline` | `RunPipeline(h, PipelineInput) (*PipelineResult, error)` | no |
| `travel` | `BookTravel(h, BookingInput) (*BookingResult, error)` | no |
| `onboarding` | `RegisterUser(h, SignupInput) (*Profile, error)` | no |

Note that a struct **input** is fine — `TransferDetails`, `SubscriptionInput` and `PipelineInput`
are all structs, and two of those build. It is only the return.

**What is a defect on the tooling side is how the violation is reported.** `GenerateExports`
declares `var __r string` (`wasm/exports.go:731`) and emits `return []byte(__r)` (`:794`), so a
non-string return produces

    ./gen_wasm_exports.go:340:28: cannot convert __r (variable of type *BookingResult) to type []byte

a Go type error in **generated code the author never wrote**, naming a variable absent from their
source and a file they did not create. The rule is real and the diagnostic should state it:
*"entry point RunPipeline returns *PipelineResult; an entry point must return string, (string,
error) or error, because that is the shape every language SDK can express."* `cleat vet` is the
natural place, since it already refuses E001-E007 before the build gets this far.

## Cause 2 and 3 are in the examples themselves

`dag` and `fooddash` fail HostCalls threading — functions reachable from an entry point that call
durable SDK methods without an `h cleat.HostCalls` parameter. `event-driven` uses `time.Now()`
inside a workflow and is rejected by the project's own E003, the rule that exists for exactly that.

**`examples/dag`'s doc comment names the command that fails on it:**

    // Build:
    //	cleat build -o /tmp/out ./examples/dag/

## Closed

All eight build, and `TestEveryGoExampleBuilds` (`cmd/cleat`) compiles every one on every run —
about seven seconds wall clock, in parallel. Four fixes and two tooling defects:

| | |
|---|---|
| #801 | `datapipeline`, `travel`, `onboarding` returned a struct pointer |
| #802 | `event-driven`: `time.Now()`, and a struct return hidden behind it |
| #805 | `fooddash`: three more struct returns, hidden behind a threading error |
| #800 | `cleat vet` now rejects a non-string entry-point result up front |
| #807 | the threading check credits HostCalls in a parameter's struct field (§3.229) |
| #809 | the build gate no longer fails on pre-transform reports; auto-threading no longer renames the SDK import (§3.229, §3.230) |

**Which directories are workflows is decided by building them**, not by a pattern: there is no
`//cleat:entry` marker to look for — `IsEntryPoint` says an entry point is an exported non-method
function whose first parameter is `cleat.HostCalls` — so the test treats the tool's own
"no workflow entry points found" as "not a workflow". `third-party-plugin` is the one such
directory. A floor of five guards the vacuous case, because "every example built" is also what a
run that built nothing reports.

## Why this rotted: they are valid Go, and nothing compiles them to WASM

`cd examples && go build ./... && go vet ./...` is **clean**. So every Go job in CI is happy. The
only CI reference to `examples/` is `examples/as-workflow`, which is AssemblyScript
(`.github/workflows/ci.yml:1126`); tier2-gate mentions `./examples/...` only in a comment about how
that gate was falsified. **No job runs `cleat build` on a Go example**, so the one command the
examples document is the one nobody runs.

## A methodological note, because the first version of this section was wrong

The first sweep reused **one** output directory for all nine builds. `cleat build` copies the
source into the build directory and does not clear it, so each example inherited the previous one's
files, and the errors were things like `toJSON redeclared in this block` naming files from a
different example. **It reported nine failures out of nine**, and "every Go example is broken" was
one sentence from being written down.

What caught it was reading an error rather than counting: `travel`'s failure named `billing.go`,
`pipeline.go` and `signup.go`, and `ls examples/travel/` shows only `booking.go` and `README.md`. A
failure citing files that are not there is a harness fault, not a finding — and the corrected sweep
gives two passes, so the wrong number was wrong in the direction that made the finding look
bigger.

### 3.229 The HostCalls threading check rejects two working patterns, one of them a first-party SDK's own — 🟢 **BOTH FIXED 2026-09-06** (WS-1, 2026-09-06)

§3.228 found six Go examples that do not build and called all six example defects. **Four were**
— three struct-pointer returns (#801) and one `time.Now()` (#802). **The other two are not**, and
this section is the correction.

`dag` and `fooddash` fail `VerifyThreading` (`internal/closure/threading.go`) with

    X is reachable from a workflow entry point (it calls durable SDK methods) but does not
    have a HostCalls parameter. Add 'h cleat.HostCalls' as the first parameter, or declare a
    package-level 'var h cleat.HostCalls' that this function can reference.

Both reach HostCalls perfectly well. The check credits four routes — a first parameter of type
`cleat.HostCalls` (phase 1), a method whose receiver struct has a HostCalls field (phase 3), a
reference to a package-level `var h` (phase 0), or a threaded caller that *passes* HostCalls as an
argument (phase 2). Neither of these is any of those.

## `examples/dag`: HostCalls arrives in a struct PARAMETER's field

    func extractText(ctx *dagplugin.TaskContext) (string, error) {
        result, err := ctx.H.DurableCall("docproc", "Extract", string(data))

`TaskContext.H` is `cleat.HostCalls` (`cleat/dagrun/dagrun.go:59`). Phase 3 credits a struct with a
HostCalls field only when it is the **receiver**; here it is a **parameter**. Four task bodies, four
errors.

**This is the `dagrun` package's designed shape, and its own doc comment names this exact caller:**

    // TaskContext.H is passed through to every user-written task body ... That means
    // TaskContext cannot be narrowed to a small interface without breaking real callers
    // (see examples/dag, which calls ctx.H.DurableCall).

So `cleat vet` rejects the pattern a first-party cleat SDK package documents itself as requiring.

## `examples/fooddash`: the remedy the message suggests is already there

`order.go:36` declares `var h cleat.HostCalls`. `validateMenuItems` does not reference it; its
callee `lookupMenuItem` does (`:313`, `h.DurableCallTyped`). Phase 0 credits **users** of the
global, not every function in a package that has one — but once a package-level `h` exists, every
function in that package can reach the host without taking anything, so the requirement is
satisfiable by adding a reference that would be dead code.

**The error tells the author to do something already done.**

## Both are fixed, and the second was not the defect it looked like

**`dag`** — phase 3b credits a HostCalls field on a **parameter**, symmetric with phase 3's rule for
receivers (#807).

**`fooddash`** — and here the diagnosis above was wrong in an instructive way. The section proposed
crediting every function in a package with a global `h`, and warned that doing so "makes the
threading guarantee vacuous for that package". **Both the proposal and the warning were beside the
point**, because `internal/transform` *already* auto-threads every durable function in such a
package — referencers and pass-throughs alike — and rewrites their call sites.

`internal/closure`'s own test says so:

    // validateAndReserve and processPayment are pass-through functions in the closure that
    // don't reference the global var h directly, so they are correctly reported as
    // unthreaded BEFORE the transform runs. After the transform they get h added as a
    // parameter.

So the check is right and the **build gate** was wrong: `cmd/cleat` exited 1 on a pre-transform
report, rejecting packages the very next stage was designed to fix. `dropAutoThreaded` now filters
errors for functions the transform gave an `h` to, and fails on the remainder.

**Changing the check, as this section proposed, would have broken that test and been the wrong
layer.** The measurement that settled it was bypassing the gate and watching the transform
auto-thread `validateMenuItems` and ten others — the finding was in what happened *after* the point
where I had stopped looking.

## What the earlier framing got right and wrong

## Why this is filed rather than fixed

Widening the check is a design decision with a real downside on each side. Crediting any function
in a package that declares a global `h` makes the threading guarantee vacuous for that package.
Crediting any parameter whose type has a HostCalls field is closer to right — it is how `dagrun`
works — but it is a new rule about transitive reachability through struct fields, and getting it
wrong in the permissive direction silently removes a guard rather than loudly breaking a build.

**Note which direction this one errs, because it is the opposite of the day's other findings.**
Nearly every measurement error recorded in this file **flatters** — a circular denominator reading
100%, a guard that cannot see the thing it guards. This one is a **false positive**: it reports
work that is not needed. That makes it cheap in consequence and expensive in trust, because the
remedy it names is either impossible (`dag`) or already present (`fooddash`), and an author who
follows the message and sees no change learns to disbelieve the tool.

**Neither example has been shown to run.** They compile as Go and their host access is coherent,
but nothing in CI builds a Go example to WASM (§3.228), so "these two are fine" means "the check's
objection does not hold", not "these examples work".

### 3.230 Auto-threading renamed the SDK import and broke every reference to it — 🟢 **FIXED 2026-09-06** (WS-1, 2026-09-06)

Found underneath §3.229: with the threading gate bypassed, the transform auto-threaded eleven
functions in `examples/fooddash` and the result did not compile.

    ./order.go:133:10: undefined: cleat
    ./order.go:141:10: undefined: cleat
    ... 19 references in that file

`ensureHostCallsImport` ran this on the SDK import, **unconditionally**:

    if imp.Name == nil || imp.Name.Name != "durable" {
        imp.Name = ast.NewIdent("durable")
    }

including when it was **unaliased**, which is the normal spelling. Every existing `cleat.X`
reference in the file then failed to compile. `addHostCallsParam` and `isHostCallsField` hardcoded
`"durable"` to match.

**`durable` was the SDK's package name before the 2026-06-01 rename** (commit `3eeb74e`, "promote
internal packages to public"). The transform kept it for three months.

The fix threads the file's **own** local name through: `ensureHostCallsImport` returns it and never
renames an existing import, `addHostCallsParam` qualifies with it, `isHostCallsField` compares
against it. A newly added import is unaliased, because the package is named `cleat` and an alias
would be noise in a file the user reads.

## Why nothing caught it for three months

Auto-threading engages **only** for a package that declares a global `var h` — `needsH` is populated
solely under `hasGlobalH`. And for exactly those packages, the threading check rejected the build
first (§3.229). **So the transform's output was never compiled by anything.** Two defects in series,
each hiding the other, and neither reachable without fixing the one in front.

That is the same shape as §3.228's `event-driven` (E003 hid a struct return) and #805's `fooddash`
(a threading error hid three), but a layer deeper: not a linter halting on the first error, but a
*stage* halting before the stage that would have failed.

**The test that catches it has to compile.** `TestBuildAutoThreadedPackage` runs `cleat build` on
`testdata/autothread` end to end — the check reports, the gate filters, the transform threads, and
the emitted Go has to build. Falsified both ways: removing the gate filter gives "cleat build
failed", and restoring the `durable` rename gives `./order.go:29:7: undefined: cleat`. **No unit
test on any single stage would have seen either**, which is the argument for having one test that
crosses all of them.

There is also a unit test on `dropAutoThreaded`, because a filter that removes too much would let a
genuine threading error through, and that failure is silent.

### 3.231 All four promise host calls reported success when there was no promise store — 🟢 **FIXED 2026-09-06** (WS-1, 2026-09-06)

The conformance-port session found that `cmd/cleat-worker/setup.go` never called
`engine.WithPromiseStore` — so on a real deployment the store was **always** nil, `workflow_promises`
had never held a row, and every `AwaitPromise` hung forever (their #812). Verified independently
before building on it: `grep -n "With.*Store" cmd/cleat-worker/setup.go` lists `WithSignalStore`,
`WithWorkflowStore`, `WithChildWorkflowStore` and `WithConcurrencyKeyStore`, and no promise store.

**This section is the engine half, and it is a correction to §3.218 — my own fix.** §3.218 made
`CreatePromise` report a store *failure* instead of logging it, and its comment says

    The failure was not unreportable, it was unreported.

**It left the missing-store branch reporting success**, one line above that sentence. All four calls
had the same shape:

    if s.engine.promiseStore != nil { ... }

| call | with no store, before |
|---|---|
| `CreatePromise` | skipped the insert, returned **success** with a promise ID |
| `AwaitPromise` | fell past the store check and **suspended forever** |
| `ResolvePromise` | returned **0**, and even *with* a store logged an error and returned 0 |
| `RejectPromise` | identical |

So §3.218 guarded the legible half: the branch that had an error object in hand. The branch with no
object at all — which was the one every shipped worker took — stayed silent. **That is the same
asymmetry as §3.223's "the engine is where the mechanism is legible": a failure that produces
something to report gets reported, and an absence does not.**

All four now report `errCode 1` and name the missing option. `cleat_resolve_promise`'s adapter
already decodes `errCode := uint32(result)` and turns non-zero into an error, so — again — the ABI
had somewhere to put it.

## Six tests asserted the defect, one by name

`TestAwaitPromiseReplayAwaitThenFreshNoStore` set `s.engine.promiseStore = nil` **deliberately**,
commented *"Fresh path with no promiseStore -> suspend"*, and asserted the hang. So did
`TestAwaitPromiseFreshNilStore`, which went on to check the deadline encoding of a suspend that
should never happen. Four more asserted `result == 0` from `CreatePromise`, `ResolvePromise` and
`RejectPromise` on a session with no store.

**Three others were repaired rather than inverted, and the distinction matters.**
`TestAwaitPromise_FreshPending`, `TestAwaitPromiseReplayDivergence` and
`TestAwaitPromiseReplayPastEnd` reached the suspend path *by having no store*, while being named for
pending promises and replay divergence. They now use a pending `mockPromiseStore`: same assertion,
correct reason. A test that reaches its outcome through an unrelated defect is not wrong about the
outcome — it is wrong about what it is testing, and inverting it would have thrown away real
coverage.

That is the sixth, seventh, eighth and ninth test found codifying a defect in this run.

### 3.234 `h.NowMs()` compiled to nothing and returned epoch 0 — 🟢 **FIXED 2026-09-06** (WS-1, 2026-09-06)

Found while doing §3.226's triage — classifying the public Go methods that sit outside the coverage
guard's denominator, so the guard fix would be mechanical rather than a judgement call.

Of **29** such methods, **23** are in `hostFunctions`, `compositeRequires`, or both: legitimate SDK
wrappers with no adapter definition of their own. **Six were in neither table.** Three are the
scope trio (§3.223, local-only by construction), two are §3.220's inert signal-reply pair, and one
was new.

    // a workflow whose only host call is h.NowMs()
    Generating WASM imports (0 host functions used)... OK
    imports:         cleat_complete, cleat_poll_work
    adapter fields:  (none)

`NowMs` finds `h.now == nil`, logs to a guest's stdout, and **returns 0**. Every workflow using it
gets an epoch timestamp. Same family as #775's `h.NewUUID()` returning the all-zeros UUID, and
arguably worse: a zero UUID looks wrong, a zero timestamp looks like a date.

## Why the composite guard could not see it

`TestEveryCompositeHostCallHasAnImportRow` (#786) walks the SDK for methods that call another
`h.X(...)` and checks the wrapper ends up with the inner method's import. Its pattern is

    \bh\.([A-Z]\w*)\(

**`NowMs` calls `h.now()`** — the closure *field*, lowercase, exactly as `Now()` does. It is not a
composite in that sense at all, so the scan does not consider it. `Now` has a `hostFunctions` row;
`NowMs` had nothing.

The generalisation, found by asking the question the guard does not: **a `HostCallsImpl` method that
invokes a closure field, makes no `h.Uppercase(` call, and appears in neither table.** Seven exist:

| method | verdict |
|---|---|
| `SetScope`, `GetScope`, `ClearScope` | touch only *local* value fields, no closure — §3.223, inert by construction |
| `HandleUpdate` | falls back to locally registered handlers and errors clearly; not in the public `HostCalls` interface |
| `NowMs` | **the defect**, fixed here |
| `ReplyToSignal`, `SendSignalAndWait` | invoke real closure fields that are never wired — §3.220 |

## The fix, and why `compositeRequires` is the right table

`"NowMs": {"cleat_now"}`. A `hostFunctions` row would *emit a field* named `NowMs`
(#786's lesson), and `HostCallsOptions` has no such field, so it would not compile. Marking the
import is enough because `info.Funcs` is `hostFunctions` filtered by `Used` — so `cleat_now` being
used pulls in `{"cleat_now", "Now"}`, and the emitted `Now` field is what populates `h.now`.

Verified by compiling: `cleat_now` imported, `Now` field emitted. Falsified by removing the row:
red with `imports wired: []`.

## The guard this wants — written, and its blocker since cleared

The rule above — *every method invoking a closure field must be named by a table* — is mechanical
and would have caught this. It shipped as `TestEveryClosureBackedMethodIsWiredOrTracked` (#823)
with the two methods it could not yet be green on, `ReplyToSignal` and `SendSignalAndWait`,
carried in an `unwiredClosureMethods` map whose entries read *"delete this entry when 3.220
lands."*

**3.220 landed on 2026-09-06 and the guard collected on that promise itself.** Both methods became
composites over promises, stopped touching a closure field, and the guard went red — not on a new
defect, but on its own exemptions no longer describing anything, naming both and saying "delete
them". The list is now empty. The list-may-only-shrink property is what turned a note-to-self into
a check that reported its own obsolescence, and it is the argument for writing the remedy into a
failure message rather than into a comment.

### 3.239 Workflow updates, implemented end to end — 🟢 **DONE 2026-09-06** (WS-1, 2026-09-06)

Closes the substantive half of [#849](https://github.com/cleat-team/cleat/issues/849). §3.238 stopped
a stranded update hanging its caller; this makes updates actually work.

#### What was there before

Nothing on the path. Three independent breaks, all recorded on #849:

1. **No guest entry point, in any SDK.** Both SDKs registered handlers into a map read only by a
   test harness — Go's `HandleUpdate` was reached from `cleattest` alone, Python's
   `_handle_update` had zero callers. `cleat_register_update_handler` recorded a *name* and there
   was no way to ask a running guest to run one.
2. **The worker never called `engine.WithUpdateHandler`**, so `DispatchUpdate` returned
   `no update handler configured for this engine`.
3. **Delivery was a 5s ticker over `w.inflight`**, populated only for the lifetime of one segment.

Note the ordering trap that made 3 dangerous to fix alone: with 2 unfixed, a scheduling fix would
have completed each request with that error and **rejected** the caller's promise. A fix verified
by "the request is no longer pending" would have read as success while delivering nothing.

#### The design, and the constraint that forced it

An update handler is a **closure in guest memory**. Only guest code can call it, so an arriving
update cannot interrupt the workflow — something in the guest has to ask.

And it has to ask at a fixed **program position**, not at a moment in time, because replay
re-executes the guest and matches host calls against history in order. The constraint is hard
rather than stylistic: `recordEvent` **appends** (`s.history = append(s.history, rec)`), so an
event can only ever land at the frontier. There is no way to insert a delivery into the middle of
an existing history — which is why an update cannot be dispatched at handler-registration time, as
the first design attempt proposed: on every segment after the first, registration is replayed from
deep inside history that is already written.

So: **dispatch points**. The SDK calls `DispatchUpdates()` immediately before each suspension
(`DurableSleep`, `AwaitSignals`, `AwaitPromise`, `AwaitChild`, `AwaitAllChildren`,
`AwaitAnyChild`), and exports it for workflows that want more. Two new host calls carry it:

| call | replaying | fresh |
|---|---|---|
| `cleat_poll_update` | return `history[stepCount]` if it is `update_received`, else not-found. **The table is never consulted** | read the table, record `update_received`, return it |
| `cleat_complete_update` | replay the `update_completed` event and settle **nothing** | record it, complete the row, settle the caller's promise |

`DurableAwaitSignals`' shape exactly, for the same reason.

**Record before settle, not after.** A crash between the two leaves the event in history and the
row still `pending`, so the next replay finds the delivery there and never re-reads the table —
at-least-once delivery with idempotent replay. The other order loses the update entirely.

**The handler re-runs on every replay, and that is the point.** The durable facts are its input and
its output, not its execution; re-running it is what rebuilds the state it mutated.

The cost, stated rather than hidden: an update is handled at the next dispatch point, not on
arrival. A workflow in a tight loop of durable calls with no suspension does not service updates
until it suspends.

#### What else this needed

- `CreateUpdateRequest` now wakes the workflow (`next_wake_at = now()`), like `DeliverSignal`. Not
  optional: a suspended workflow reaches no dispatch point, so without it the request waits for
  something else to wake the workflow — for one waiting on a signal, possibly never. Three dialects.
- The 5s ticker and `dispatchPendingUpdates` are **deleted**, along with the three tests that
  covered them. #849 named those tests as the reason this went unnoticed: each built the
  precondition by hand (`w.inflight.Store(...)`, `WithUpdateHandler(...)`) and so asserted the
  function worked *given* a state that never held when the ticker fired.
- `engine.WithUpdateHandler` and `Engine.DispatchUpdate` are **removed**. They were exported API
  with no caller anywhere outside their own four tests — the option existed so the worker could
  configure an update handler, and the worker never did. They were briefly kept and documented as
  "an embedder hook that is not the workflow-update path", which is a fair description and still
  leaves two exported names that read as the update path and are not. The names were the whole
  problem, so the names are gone.

#### The other SDKs

Rust, Java and AssemblyScript followed in the core-ABI shape; Python is the component path. Every
one of the `absentToken` exemptions this section created is gone, and **the mechanism worked on
its first real use**: adding each binding failed `TestEverySDKCoversEveryHostStopSite` until the
exemption was removed and the method added to that SDK's refusable-call list. The only exemptions
left are the three that predate this work.

Python needed three things the others did not, all worth recording:

  * **`result` is a WIT keyword**, so `durable-complete-update`'s parameter is `outcome`.
    componentize-py refuses the file otherwise, with the column of the offending token.
  * **`dispatch_updates` must be a no-op when there is no host.** Go guards on a nil closure;
    Python's non-WASM import is a stub that *raises*, and dispatch runs before every suspension —
    so without the guard any local run that slept died inside a dispatch it never asked for.
    Caught by two existing stop tests, not by anything new.
  * **`json.dumps` needs `separators=(",", ":")`.** Its default `", "` / `": "` would make the
    Python harness's envelope differ from every other SDK's for no reason — the same trap §3.220
    hit.

Regenerating the bindings also surfaced **pre-existing docstring drift in four unrelated generated
files**: they still described a `DurableCallWithHeartbeat` limitation that §3.111 removed. The
generated artefacts had not been regenerated when the WIT prose changed. Docstrings only — verified
no signature changed before copying, rather than after.

#### Two guards this change had to repair, both silent

**The stop-correspondence guard caught the new calls immediately** — both consult
`stopBeforeNewWork` and were not declared stop surfaces. That one worked as designed.

**The compaction fuzzer could not reach them, and had already rotted twice.**
`parseFuzzEvents` clamped its type byte with a literal `% 30`; the new codes are 34 and 35. The
comment above that line documented the *previous* instance (`% 27` left three cron codes unfuzzed,
fixed 2026-08-09) as though it were the last — but `AwaitAnyChild` (31), `PollChild` (32) and
`AdminAction` (33) had been added since and were unreachable in exactly the same way. **Six event
types unfuzzed across two occurrences, neither of which failed anything**, because an unreachable
code makes the fuzzer explore *less* and nothing measures that. The bound is now derived from
`codeToEventType`.

That was not sufficient either. `compaction_fuzz_test.go` also carried
`UpdatePayload`/`UpdateResponse`/`UpdateError` as exempt "dead fields with nothing to lose" — prose
that stopped being true the moment these events began carrying a handler's input and result.
Removing the exemptions changed nothing observable: deleting `rec.UpdatePayload = ce.Request` from
compaction left the fuzz test **green**, because `FuzzCompactionEquivalence` run without `-fuzz`
executes only its seed corpus and no seed produces those codes. `TestCompactionPreservesTheUpdateEvents`
asserts the round trip directly and fails on that deletion. **A fuzzer finds cases nobody thought
of; it does not assert the case you already know about.**

### 3.236 The plugin harness stripped the tenant RLS policies off a shared SQL Server database — 🟢 **FIXED 2026-09-06** (WS-1, 2026-09-06)

`tests/plugin-harness`'s `RunCoreMigrations` split each migration file on `GO` and executed the
batches on a bare connection, with no transaction. Running its MSSQL arm against a database that
already carried the schema left **7 of 9 tenant SECURITY POLICYs permanently dropped**, and every
tenant-scoped MSSQL test in the repo afterwards ran with no RLS backstop.

Reproduced in one command, twice, against a database freshly built from the shipped migrations:

    # before: 9    after: 2 -- TenantFilter_Promises, TenantFilter_Settings
    go test ./tests/plugin-harness/ -run TestPluginCalls_MultiDB -count=1

#### The mechanism was written down in advance

`migrations/mssql/001_schema.sql` drops the seven base policies at the *top*, before the
`CREATE OR ALTER` of the function they are schemabound to, and recreates them at the bottom. Its
header explains why that is safe and names the single condition:

> The condition: that atomicity is the runner's, not this file's. Applying this file by hand —
> sqlcmd, a GUI, **any tool that treats GO as a real batch separator and autocommits each batch** —
> does leave tenant-scoped tables unfiltered from here to the CREATE SECURITY POLICY block at the
> end.

`RunCoreMigrations` was that tool. And it is worse than the header's warning, which describes a
*window*: 001 never reaches the recreate block on a re-run, because migration 031 adds
`TenantFilter_Promises` — which 001 predates and therefore does not drop, and which holds a hard
dependency on `dbo.fn_tenant_filter`. So `CREATE OR ALTER FUNCTION` fails, the seven drops before
it are already committed, and the loss is permanent. The two survivors are exactly the two 001
does not know about.

#### Why CI could not see it, and a developer always could

`plugin-harness-ci.yml:354` points `CLEAT_TEST_MSSQL` at `database=master` on a fresh SQL Server
container. The migrations are applied exactly **once**, so there is never a second application to
fail. A developer following CLAUDE.md and setting all three DSNs has the opposite: a long-lived
database that already carries the schema. **This whole class of defect — anything that only goes
wrong on re-application — is invisible to a CI that starts from an empty container every time.**

The MSSQL arm of `OpenTestDB` creates a `SCHEMA`, not a database (MySQL gets its own database),
so this ran against the shared `cleat` database in `dbo`.

#### Fixed

One transaction per migration file, which restores the atomicity 001's header depends on. The
re-run still fails — that is a separate defect, [§3.237](#3237) — but it now fails the way 001's
header calls "the OLD ordering failed safely", leaving the database as it was found.

#### The regression test's first version passed the falsification

Worth recording, because it is CLAUDE.md's "a falsification that stays green is telling you which
case you did not write", and the flaw was invisible by inspection. The test measured the policy
count after one application and compared it against the count after a second. Backing the
transaction out left it **green**: on an already-migrated database the *first* application is the
damaging one, so the test compared 2 against 2 and found them equal. The `before == 0` vacuity
guard did not fire, because 2 is not 0.

What it needed was not a delta but an **absolute** — the set of policies the migrations bind,
parsed from the migrations — asserted after *both* applications. Two known-positives now fail
without the fix, where the first version failed on neither:

| starting state | without the transaction |
|---|---|
| fresh database | `RE-APPLYING … left 2 of the 9 … standing` |
| already-stripped database | `after applying … carries 2 of the 9 …` + the drop-and-recreate instruction |

The parse is anchored at line start (`^CREATE SECURITY POLICY dbo\.`) for the reason this document
keeps re-learning: unanchored, it also matches 001's own header, which *discusses* the statement in
prose. Measured 2026-09-06 — unanchored returns 16 distinct "names", 7 of them fragments of English
sentences; anchored returns the 9 that exist.

### 3.240 `TestPythonWasmAbiBoundary` compared two hardcoded lists in the same file — 🟢 **FIXED 2026-09-07** (WS-1, 2026-09-07)

The test's stated job is to catch a Python SDK that imports a host function the engine does not
register — which is not a degradation but a hard failure, since a guest importing an unregistered
name does not instantiate at all.

It never read either side. `pythonExpectedImports` was a literal in
`engine/python_wasm_e2e_test.go`, and so was `registeredImportNames()` in the same file, carrying
the comment:

    // This list must stay in sync with the Export("...") calls in registerHostFunctions
    // in imports.go. When adding new host functions, add them here too.

Nothing read `imports.go`. Nothing read the Python SDK. The test asked whether the file agreed
with itself, and it always did.

**It had rotted in both directions, and reported green throughout.** Measured 2026-09-07:

| | count | what |
|---|---|---|
| named by both lists, not exported | **8** | the six durable-state calls ([§3.216](#3216)) and the two inert signal calls ([§3.220](#3220)) |
| exported, named by `registeredImportNames` | missing **13** | including `cleat_poll_update` and `cleat_complete_update`, added days earlier |

The first row is the exact condition the test exists to detect: eight names it asserted a Python
workflow needs and the engine does not have. Had the Python SDK really still imported them, every
Python workflow would have failed to instantiate and this test would have said fine.

It did not — the SDK had dropped all eight — so **no live defect, only a guard that could not
have found one.** Confirming that took care of its own, because the sole occurrence of
`cleat_send_signal_and_wait` in `host_calls.py` is a *retraction*:

    # There is no _import_cleat_send_signal_and_wait or
    # _import_cleat_reply_to_signal here (removed 2026-09-06, ...)

A name scan reads that as a confirmation. It is the [§3.213](#3213) AssemblyScript trap verbatim,
in a second SDK.

**Both sides are now derived.** The engine side calls `wazeroCleatABI`, which instantiates the
host module and enumerates `ExportedFunctionDefinitions()` — the real registration, already used
by `engine/hostabi_runtime_parity_test.go`. The Python side is read out of `host_calls.py` twice,
in two ways chosen to fail in opposite directions:

- **strict** — line-anchored, accepting an alias only where an `import ... as _import_X` can
  legally appear. A comment line begins with `#` and cannot match.
- **loose** — the alias token anywhere at all, prose included. Wrong by construction; its only
  job is to disagree.

The test fails if they differ, naming which reading saw what. Both returned the same 44 names,
and a third reading — Python's own `ast` module over the same file, collecting `alias.asname` —
returned the identical 44. Two readings agreeing is evidence; the strict one alone was a claim.

**The alias is not the ABI name**, which a single reading would have got wrong: six aliases drop
the `cleat_` prefix (`_import_uuid`, `_import_fetch`, `_import_side_effect`, `_import_get_scope`,
`_import_set_scope`, `_import_continue_as_new_versioned`). Treating the alias as the name reports
six phantom gaps. The mapping rule is validated by its own output rather than asserted: applied
to all 44 it lands every one on a real engine export, with nothing missing.

**Five real Python SDK gaps fell out**, held in `pythonUnboundBaseline` as shrink-only so that
closing one is noticed. `cleat_await_any_child`, `cleat_poll_child`, `cleat_json_parse`,
`cleat_json_stringify`, `cleat_run_detached` — things a Go workflow can do and a Python one
cannot. Three further unbound names are in the baseline and are not gaps: `cleat_poll_work` and
`cleat_complete` are the worker handshake, and `cleat_register_query_handler` is deliberately
unbindable.

**Four known-positives, one per mechanism** — because the broken version passed too, so "it
passes" was never evidence:

| control | result |
|---|---|
| add a Python binding the engine does not export | fails, names `cleat_not_a_real_host_call` |
| add a retraction *comment* naming a phantom alias | fails as a strict/loose **disagreement** — not counted as a binding |
| add an engine export with no Python binding | fails, names it as beyond the baseline |
| break the strict regex so it matches nothing | fails on the floor, rather than passing vacuously |

The third is the widen-detector; the fourth is the [§3.213](#3213) lesson made mechanical — an
extractor that sees less inflates every metric built on it, and with zero names "every Python
import is registered" is vacuously true.

**A note on method, since it cost a rewrite.** Restoring after known-positive four with
`git checkout HEAD -- engine/python_wasm_e2e_test.go` discarded the entire uncommitted change,
because the file's committed state was the *old* test. CLAUDE.md's "a falsification has two steps,
and only one of them announces failure" applies to an uncommitted rewrite as much as to a reverted
fix: the revert is loud, the restore is silent. Commit before falsifying, or restore from a copy.

**This was a single instance, not a sweep.** Every other Go test file holding ten or more literal
`cleat_` names uses them as fixtures that make a real call, so a stale name fails to link;
`grep -rn "must stay in sync" engine/*_test.go wasm/*_test.go` now returns only this section's own
quotation of the comment that was removed.
### 3.246 The OAuth middleware refused every bearer token that was not its own, so no API key worked — 🟢 **FIXED 2026-09-07** (WS-1, 2026-09-07)

Reported by WS-3 as #912 after the `cleat-ports` suite went from 70 passing to failing every test.
**Green on `dc543263`, broken on `bf199c23`** — a regression on `develop`, not in a branch.

`plugins/oauthprovider`'s middleware intercepts any `Authorization: Bearer …`, looks the token up
in `oauth_sessions`, and returned **401 `{"error":"invalid session"}`** when it was absent. A cleat
API key is presented exactly that way (`auth/middleware.go:48`: *"Supports: Authorization: Bearer
cleat_sk_<key>"*), so it never reached `auth.Middleware`.

It became live when [§3.315](#3315) linked all 20 bundled plugins: `cmd/cleat-worker/main.go:747`
wraps the whole handler chain in **every** plugin implementing `HasMiddleware`. `--require-auth`
defaults to true, so a default deployment served an API where **no key worked at all** — the same
practical outcome as §3.66, by a different route.

**The middleware was correct in isolation and its own tests passed**, because they exercise it with
OAuth tokens. It only becomes wrong sitting in front of a handler that accepts a *different* bearer
scheme, which is exactly what linking it did. The general form is worth naming: **a middleware
cannot ask a database "is this token mine?" — only "is this token a live session of mine?" — and a
NO to the second was read as a NO to the first.**

**The fix is not the obvious fall-through-on-error, and the difference matters.** Falling through on
any lookup failure fixes #912 but also stops this plugin refusing a token that IS its own and is
expired or revoked. The two schemes do not collide, so they can be told apart by shape:
`generateSessionToken` emits the hex of 32 random bytes — 64 lowercase hex characters — and an API
key carries a `cleat_sk_` prefix. `looksLikeSessionToken` gates the lookup, so:

| token | before | after |
|---|---|---|
| `cleat_sk_…` | **401 invalid session** | falls through; `auth.Middleware` decides |
| 64-hex, live session | session injected | session injected |
| 64-hex, expired or unknown | 401 | **401** — still this plugin's business |

Nothing is loosened. The middleware only ever *adds* `SessionInfo`, grants nothing on its own, and
sits outside `auth.Middleware`, which still refuses a request carrying no valid credential.

**Four existing tests had to change, and what they revealed is the point.** They authenticated with
`"valid-mw-token"`, `"expired-mw-token"`, `"nonexistent-token"` — strings no production path can
issue. Under a shape gate they fall through, so they were updated to real 64-hex tokens. A test
whose fixture could never occur in production is a test that cannot see a defect about token shape,
which is the defect that happened.

`TestAFreshlyGeneratedSessionTokenLooksLikeOne` ties the predicate to the generator it describes,
in both directions: 20 freshly generated tokens must be accepted, and `cleat_sk_…`, a 63-character
hex string and a 64-character non-hex string must all be refused. Without it, a change to
`generateSessionToken` would silently stop this plugin recognising its own sessions.

**Falsified**: removing the gate returns the exact symptom, `401 {"error":"invalid session"}`.

**Swept, and it is not a class.** Three plugins implement `Middleware` — `oauthprovider`,
`ratelimiter`, `auditlog`. Only this one rejects on a credential decision; `ratelimiter` answers 429
on a rate decision and consumes no shared auth header, `auditlog` never rejects. Re-derive with
`grep -rln "func (p \*Plugin) Middleware(" plugins/`.

**The standing lesson, which is WS-3's and worth recording as theirs:** linking 19 dormant plugins
activated 19 sets of assumptions that had never been tested against each other. Three separate
problems came out of that one change — the shared config blob, `email`'s unconditional Init failure,
and this — and none of them is a defect in the plugin that carries it.
### 3.257 The all-dialect plugin test asserted tables exist, not that a row can be written — 🟢 **FIXED 2026-09-08** (WS-1, 2026-09-08)

cleat#963, routed by WS-3. This is why [§3.255](#3255) could happen: `TestPluginMigrations_AllDialects`
runs every plugin's migrations on every dialect and then asserts `tableExists`. That passed
throughout — the table *was* there. It was the INSERT that could not succeed.

**"The schema was created" and "the schema is usable" are different claims**, and an existence check
only answers the first. It is the same split as *the value round-trips* versus *the value is
honoured*, and *the reader is live* versus *the writer exists* — and a guard that asserts existence
passes for every one of them.

**The mechanism, not the sweep.** A loose regex over `plugins/*/migrations.go` reported 13 plugins
with per-dialect asymmetry, and the first checked precisely was already compensated. **That number
was never a finding.** The new check asks each *real* database what it did with the DDL and reports
**two**:

    audit_events.id         mysql requires a value, mssql supplies one   (audit-log)
    event_subscriptions.id  mysql requires a value, mssql supplies one   (event-triggers)

The property asserted is not "does plugin X's INSERT work" — that needs a write path per plugin and
only covers statements someone has already written. It is:

> the three dialects must **agree** about which columns a writer must supply.

If they agree, a statement that works on one works on all, which is exactly the invariant §3.255
broke. `information_schema` is the oracle because it sees identity columns, generated columns and
defaults that a pattern over SQL text cannot.

Both current asymmetries are compensated and were **verified at source rather than taken on report**:
audit-log supplies `uuid.NewString()` on every dialect (§3.255), and event-triggers carries a
MySQL-specific INSERT at `queries.go:41` listing `id` where the other two omit it. The second is the
weaker shape — the statement that must supply the value and the schema that requires it are in
different files with nothing tying them together.

**The guard read a stale schema, and falsifying it is what showed that.** Removing the MySQL default
from `audit_events.timestamp` did *not* redden the test: `RunMigrations` records applied versions and
skips them, and the MySQL and SQL Server backends hand back the **shared** test database whose plugin
tables some earlier run created. So the mutated DDL never ran. That is CLAUDE.md's *"when a schema
migration lands, recreate your test databases"* — and **a guard reporting agreement from a stale
schema is cleat#963 one level up.** Fixed with a scratch database per dialect; the mutation then
fails as it should.

**What the baseline does not assert, said rather than implied:** it compares *schemas*, so an entry
means "the dialects disagree and I have read the code that compensates". Deleting event-triggers'
MySQL INSERT would not redden this test. Where that matters the compensation needs its own write
test — audit-log has one (§3.255, which drives `recordAudit` against a real MySQL), event-triggers
does not, and that gap is named here rather than left implicit.

`./engine/` 4685 pass / 0 fail / 7 skip.

### 3.255 Audit logging recorded nothing on MySQL, and the empty table looked like a quiet system — 🟢 **FIXED 2026-09-08** (WS-1, 2026-09-08)

cleat#958, found by WS-3 running the `samples-go` port against a second and third dialect.

`plugins/auditlog/middleware.go` inserted without an `id`, relying on a database-side default. Two
of three dialects have one:

| dialect | `audit_events.id` | rows after a full port run |
|---|---|---:|
| postgres | `UUID PRIMARY KEY DEFAULT gen_random_uuid()` | 18,657 |
| mssql | `UNIQUEIDENTIFIER PRIMARY KEY DEFAULT NEWID()` | 473 |
| **mysql** | `CHAR(36) NOT NULL`, **no default** | **0** |

Reproduced directly before changing anything:

    insert WITHOUT id -> Error 1364 (HY000): Field 'id' doesn't have a default value
    insert WITH id    -> <nil>

**Not "fewer rows" — the table had never held one on that dialect.** The error is logged and
swallowed: the request succeeds, the middleware returns, the worker carries on. The only symptom is
an empty audit table, which is indistinguishable from an audit log for a quiet system. That is the
one failure mode audit logging exists not to have, because the absence of an entry is read as
evidence the event did not happen.

**Fixed by supplying the id, not by adding a MySQL default.** The narrower fix works; this one
removes the class. Three databases no longer have to agree about UUID generation for one statement
to succeed, it needs no MySQL 8.0.13+ for `DEFAULT (uuid())`, and a fourth dialect gets it right on
day one. `github.com/google/uuid` was already imported in the same file.

**The fake was the reason nothing caught it, and that is the more useful half.** The behavioural
tests use a driver double that **invented** an id with `uuid.New()` while reading the caller's other
arguments by ordinal. *A double that supplies what the database will not is indistinguishable from a
database that supplies it* — so a statement MySQL rejects looked fine. It now parses the id the
caller actually sends and refuses anything that is not a UUID, so reverting the fix reddens the
fake-backed suite too.

**And the first version of the new test was weaker than it looked.** It issued the `INSERT` itself,
which asserts the shipped DDL accepts *a statement I wrote* — it would have passed unchanged with
the plugin regressed. It now drives `recordAudit`, and because that logs and swallows, the assertion
is on the **row count**, which is also the honest shape: an operator's only signal was an empty
table. Falsified: reverting gives *"the plugin recorded 0 audit rows on MySQL, want 1"*.

Two fixture problems were fixed rather than worked around: the DDL names its indexes after the
table, so a renamed table collided (`Error 1061`), and the shared test database already held a copy
whose shape this test would then have been asserting against. It builds a scratch **database** from
the shipped `UpMySQL` migration.

**The port suite passes on all three dialects on top of a subsystem that worked on two**, which is
WS-3's observation and worth keeping: coverage of the engine said nothing about this, and nothing in
either port asserts anything about audit events.

### 3.254 A routing-rule removal reported success for a rule that never existed — 🟢 **FIXED 2026-09-07** (WS-1, 2026-09-07)

cleat#946's **second half**. #948 fixed the first — `ShardedStore` routed the removal by rule ID
while rows are placed by workflow name, so it deleted from the wrong shard — and left this: **the
removal reported success either way.**

No implementation checked rows-affected. A `DELETE` matching nothing succeeds, so the store returned
nil and `handleRemoveRoutingRule` answered `200 {"status":"removed"}` for a rule that never existed.
`PickVersionByRouting` runs on every workflow start, so an operator tearing down a canary was told
it was gone while it went on shifting live traffic.

**I duplicated #948 before noticing it.** The issue was unassigned, I self-assigned and built the
whole fix — including a `tryEachShard` rewrite — and found the merge conflict only at rebase. The
sharding half is theirs; this entry is what their fix did not cover, rebuilt on top of it rather
than forced over it.

**The two halves interact, and that is the part worth a test.** #948 has `ShardedStore` ask **every**
shard, and `forEachShard` **returns on the first error**. The moment a store starts returning
`ErrRoutingRuleNotFound` — which n−1 shards legitimately do — a naive pass-through aborts the walk
at shard 0 and never reaches the holder, **reintroducing #948's defect by way of fixing the
reporting**. The sentinel is swallowed per shard and re-raised only if no shard claimed the row.

`TestTheNotFoundSentinelDoesNotAbortTheShardWalk` puts the rule on the **last** shard on purpose, so
a pass-through fails deterministically rather than depending on where the rule happens to sit.
Falsified: passing it through fails with *"the walk stopped early"*.

**The reporting half needed real databases.** Every mock in the suite returns nil, which is why
nothing saw this — a Go nil has no opinion about how many rows were touched. Neutering the check
makes all three dialects go silent again.

The handler now answers **404**: a rule ID naming nothing is a bad request path, not a server fault.

`./engine/` 4674 pass / 0 fail; `cmd/cleat-worker` green; gofmt clean.

### 3.253 Python's `run_detached` ran the work inline and called it detached — 🟢 **FIXED 2026-09-07** (WS-1, 2026-09-07)

`HostCalls.run_detached(fn)` took a callable and executed it with `fn(self)`. It made **no host call
at all**, while its docstring said *"the host would ensure the detached execution continues even if
the parent workflow is cancelled"*. The work ran inside the caller and was cancelled with it — the
one thing the method existed to prevent.

Left open by [§3.252](#3252) as a public API decision rather than a wiring change. **Breaking
changes were authorised on 2026-09-07**, so it is now
`run_detached(name: str, input_json: str)`, wired to `cleat_run_detached` and matching Rust's
`run_detached(name, input_json)`.

**Go was already fixed.** `cleat/runtime_workflow.go` takes `(name, inputJSON)` and returns an
error when unwired, carrying a comment that describes the closure version as previous — and Go's
row in [§3.241](#3241)'s parity matrix never listed `cleat_run_detached`. Checked before writing
anything, which is why this entry is Python-only.

**There is no mechanical migration, and that is the honest thing to say about it.** The old
signature's whole point was to run *local code*; the host cannot run local code. A caller passing a
function has to name a deployed workflow instead. The docstring says so rather than implying a
rename.

**A test asserted the defect, and passed for as long as it existed.**
`test_run_detached_executes_fn` passed a function and checked it had been **executed** — which is
precisely the behaviour that made the method a silent no-op. It now asserts the call is *recorded*
and that nothing runs inline. A test can pin a defect as firmly as a feature, and this one did.

**Five layers, and the guards found the two I would have missed.** WIT, regenerated bindings,
`WitToEnvImport`, `HostCalls`, `LocalHostCalls`, plus the fixture, the README and the local-host
test. `TestEveryImportedWitFunctionHasAnEnvMapping` and both Python baselines were the checks that
made the set complete rather than my memory of it — the rewrite row went into
`durable-extended-lifecycle` because that is where the WIT declares it, a distinction that cost a CI
round trip in §3.252.

Falsified in both directions: removing the `_import_` alias makes the engine-side guard report
`cleat_run_detached` unbound; removing the rewrite row makes the harness guard report it unreached.

**Python's unreached set is now `cleat_json_parse` and `cleat_json_stringify` — and neither is a
gap.** Every real capability gap in every SDK is closed:

| SDK | real gaps |
|---|---|
| rust | 0 |
| java | 0 |
| assemblyscript | 0 |
| python | **0** |
| go | 3 — `cleat_fetch`, `cleat_get_scope`, `cleat_set_scope` |

`python-sdk` 458 pass / 1 skip; `./engine/` 4662 pass / 0 fail; `tests/plugin-harness` green;
`./wasm/` green.

### 3.251 Rust and Java reported a bare error code where the host wrote a message — 🟢 **FIXED 2026-09-07** (WS-1, 2026-09-07)

[§3.200](#3200)'s defect in two more SDKs, and the follow-up [§3.244](#3244) named as its own
prerequisite: fixing these in Rust *first* would have added fifteen more out-of-bounds reads.

A call with an output buffer usually puts its failure reason there — `engine/children.go` writes
`rec.Err`, `AwaitPromise` writes `rec.PromiseError`, `SideEffect` writes its `errMsg` — and a guest
reporting the bare `errCode` throws away the only thing that says what went wrong. **15 of 22 Rust
wrappers and 11 of 20 Java wrappers did.**

Now 13 and 11. The two Rust holdouts are `json_parse` and `json_stringify`, which return
`Option<String>` and have **no error channel at all** — nothing to improve, and excluded for a
reason rather than missed.

**The obvious implementation is wrong, and it is worse than what it replaces.** "Read the buffer,
fall back if empty" returns **65536 NUL characters** as the error text on a bad-parameter refusal:
`errBadParam` is returned *before* the handler runs so nothing is written, yet a length is still
decoded from the sentinel's bits — 4294967295 — which clamps to the whole zeroed buffer, and
`is_empty()` is false. Measured before shipping it:

    decoded len = 4294967295   returned len = 65536   is fallback? = false   all NUL? = true

`host_message_or` therefore **truncates at the first NUL**. The host writes UTF-8 with no interior
NUL and an unwritten buffer is zeroed, so "up to the first NUL" is exactly what it wrote.
Falsified: removing the truncation fails with *"an unwritten buffer with a bogus length must fall
back, not return 65536 NUL bytes"*.

**A helper rather than an edit per site, because the return shapes differ.** The first attempt
rewrote each branch to `return Err(...)` and did not compile: these wrappers return
`(String, Option<String>)`, `(String, bool, Option<String>)`, `Result<_, CallError>` and more.
Wrapping the `format!` each site already had preserves every signature and makes the diff show
exactly what was kept.

**`await_signals_ms` was skipped by the sweep for a mechanical reason, not a principled one** — two
buffers and an `as u32` cast the extractor's pattern did not match. Wired by hand, reading the
signal-NAME buffer with its own clamp preserved: without it an over-long reported length reads past
the name region into the payload buffer beside it. Java's equivalent had the same shape and the same
fix.

**Not covered end-to-end, and that is stated rather than papered over.** The plugin harness drives
no error through any of these paths — in an in-memory environment they succeed or suspend — so the
recorded outcomes are unchanged and nothing else in the tree would notice a mis-wiring.

What *is* checked is the property the bulk edit could actually get wrong:
`every_error_branch_reads_the_same_buffer_as_its_success_path`. A wrapper reading a **neighbouring**
buffer still compiles, still returns a `String`, and reports another call's data as this call's
error message.

**Falsifying that guard took three attempts, and the failures are the interesting part.** Pointing a
branch at an undeclared name did not compile, so the test never ran and the empty output read as a
pass. Inserting a decoy buffer landed *inside* a `vec![0u8; N]` macro, because the pattern stopped
at the semicolon within it. Only the third — a correctly placed second buffer — compiled and made
the guard report `create_promise: error branch reads decoy_buf, success path reads id_buf`.

**Every wrapper except `await_signals_ms` has exactly one buffer today**, so the mismatch is
currently structurally impossible; the guard earns its keep when the next two-buffer wrapper is
added. That is worth saying plainly rather than implying it catches something live.

clippy clean on all three crates; `gradle test` clean; `tests/plugin-harness` green.

### 3.250 A backed-off worker now says when runnable work exists — 🟢 **FIXED 2026-09-07** (WS-1, 2026-09-07)

The visibility half of [§3.249](#3249), implemented after WS-3's user decided against a behavioural
change. `ClaimWorkflows` uses `FOR UPDATE SKIP LOCKED`, so "nothing to run" and "every candidate was
locked" arrive at the dispatch loop identically, and the second backs off to 6 × `pollInterval`
while the work sits there.

**`CountRunnableWorkflows` on `WorkflowStore`, and the cost objection is answered by WHERE it is
paid.** The information needs a query, and a query on every idle poll is the objection that ruled
out the behavioural fix — the idle poll is this loop's most common path. So it is asked only when
`idleTicks == maxIdleTicks`: one query per six poll intervals, in the only state where the answer
changes what anyone would do. A briefly-idle worker does not need to know; one fully backed off for
minutes with runnable rows does.

`TestTheRunnableCountIsAskedOnlyAtFullBackoff` pins that, and it is the test that protects the
design rather than the behaviour. `idleTicks` resets only on a parent wake or a `NOTIFY`, so
`== maxIdleTicks` is true **exactly once** per idle streak. Measured: **22 claim cycles, 1 count
query.** Falsified both ways — `>=` in place of `==` gives **17**, and removing the call gives 0.
A regression to `>=` would put a query on the hot path while every other test still passed.

**The predicate must match the claim's, and the two ways to get it wrong are opposite:**

- drop `task_queue` and it **over-reports** — rows a worker is correctly declining read as work it
  is failing to claim. This was the one flaw in cleat#923 as filed, and it is now WS-3's note on
  the issue.
- drop the tenant scoping and it over-reports across tenants, which on SQL Server is not
  hypothetical: `dbo.fn_tenant_filter` is off for the admin role, so `AND tenant_id` **is** the
  whole of the scoping there ([§3.91](#391)).

Asserted by **agreement with the claim** rather than by reading the SQL, because the SQL differs per
dialect — PostgreSQL leans on RLS inside `beginTxWithRLS`, MySQL and SQL Server carry an explicit
predicate. Agreement is the invariant; the spelling is not. Falsified per clause: dropping
`task_queue` fails the blindness test, dropping the status predicate fails the agreement test.

**A deliberate interface addition with four implementers** — `PostgresStore`, `MySQLStore`,
`MSSQLStore`, `ShardedStore` (which sums across shards, because the claim walks all of them). Named
as a decision rather than left as drift: unlike the options [§3.889](#3889) is about, this one has a
production caller in `cmd/cleat-worker/setup.go` from the first commit. The alternative — logging
the backoff state alone — was rejected because a genuinely idle worker emits the identical line: it
makes the *state* visible without making the *distinction* visible, which is the entire point.

**One test failure was its own fault, and the harness had already said so.** The task-queue test's
raw `UPDATE` matched zero rows, and the count "failed to move" — because SQL Server's filter
predicate hides every row from a connection with no tenant session context. `pluginDepsBackends`
carries `prepareRawAccess` for exactly this, with a comment saying the test would otherwise "report
a failure that is really its own". It now also asserts `RowsAffected() > 0`, so a fixture that moves
nothing fails as a fixture rather than as a finding.

`maxIdleTicks` moved from a function-local `const` to package scope so the test binds to the real
value rather than a copy of it.

`./engine/` on all three dialects: 4651 pass / 0 fail / 7 skip.

### 3.248 Nothing pinned that a dispatch point wires the update imports — 🟢 **GUARDED 2026-09-07** (WS-1, 2026-09-07)

A workflow that registers an update handler and then waits **never names the imports it needs**.
`AwaitSignals` calls `DispatchUpdates`, which calls the `pollUpdate` and `completeUpdate` *closure
fields* — so nothing in such a workflow's own source mentions `PollUpdate` or `CompleteUpdate`, and
the wiring rests entirely on a `compositeRequires` row in `wasm/usage.go`.

Remove that row and `DispatchUpdates` returns at its `h.pollUpdate == nil` guard: the workflow
accepts updates forever and handles none, **silently**. No error, no log, no failed call — the
guard exists precisely because a guest compiled before updates existed must not crash.

**This was the first thing worth checking when WS-3 reported #910**, a delivery failure that looked
exactly like it. Ruling it out took a hand-built fixture and a temporary test row, because nothing
in the tree pinned it. #910 turned out to be a timing artefact — the workflow had completed 0.31s
before the update was created — but the elimination cost more than it should have.

`testdata/updatedispatch` is that fixture, made permanent as a row of
`TestEachRewiredMethodWiresItsImport`. Its shape is the real one: register a handler, then wait in
slices so there is a dispatch point to service it, and **never call `DispatchUpdates` explicitly**.

**The fixture shipped in [§3.247](#3247) already, unreferenced**, with a doc comment describing a
test row that did not exist — a comment that was false about the tree the moment it landed. This
adds the row it names.

**Falsified**: dropping `AwaitSignals`'s `compositeRequires` row fails with *"a workflow whose only
host call is h.AwaitSignals(...) wires no cleat_poll_update import; it would compile and be unable
to act."*

### 3.247 The update request key used a NUL separator, which PostgreSQL refuses inside JSONB — 🟢 **FIXED 2026-09-07** (WS-1, 2026-09-07)

Reported by WS-3 as #914, on a run that had already proved delivery works — the caller's promise
came back `resolved` and the handler ran. Then the workflow failed:

    finalize workflow: append events: step 2:
    pq: unsupported Unicode escape sequence (22P05)

`updateRequestKey` joined the update name and the promise ID with a literal `\x00`. That key is
recorded as `UpdateRequestID` on the `update_received` event, `store_events.go` puts it in the
event payload, and `event_history.payload` is **JSONB** on PostgreSQL.

**The precise mechanism matters, because the obvious statement of it is wrong.** PostgreSQL does
not reject a raw NUL *byte* here — the byte never reaches it. `json.Marshal` escapes a NUL to the
six characters `\u0000`, so what the engine sends is valid JSON *text*. PostgreSQL rejects that
**escape**, because `jsonb` cannot represent the codepoint even escaped. Credit to WS-3 for
separating the two questions: their first probe asked about a raw byte, which never occurs, and got
`ISJSON = 0` from SQL Server — the right answer to the wrong question.

**Unconditional** — the NUL is the separator, not a property of the data — so it was every update
that reached a dispatch point.

**The ordering is what makes it harmful.** `runUpdate` settles the caller's promise *before* the
segment finalizes, so the caller was told the update succeeded and *then* the workflow failed and
the state the handler produced was discarded. A caller polling that promise is told the update
landed when it was thrown away.

**Dialect-divergent, and measured rather than assumed** — the input is what `json.Marshal` really
produces, `{"k":"a\u0000b"}`, not a raw byte:

| dialect | storage model | result |
|---|---|---|
| **postgres** | `jsonb`, a parsed representation | **`ERROR: unsupported Unicode escape sequence (22P05)`** |
| mysql | `JSON`, parsed but permits the codepoint | accepted |
| mssql | `NVARCHAR` + `ISJSON()`, a text check | accepted, `ISJSON` = 1 |

Three storage models, three answers, one input. The escape is well-formed JSON text, so a validator
that checks *text* passes it and a type that must *represent* the value cannot.

So a single-dialect test on MySQL would have passed while the primary backend was broken — and the
two that accept it were silently storing a NUL in a column the third validates. WS-3 explicitly
declined to guess this, citing [§3.245](#3245), where reading `001_schema.sql` and concluding gave
the wrong answer about a CHECK that lived in `037`.

**The fix is not a rarer separator.** `UpdateName` is chosen by the workflow author and can contain
anything, so no delimiter is safe — a rarer one moves the collision rather than removing it. The
key is now **length-prefixed**, which is unambiguous for every possible input:

    "add"   + "p-1"  ->  "3:addp-1"
    "a:b"   + "p-1"  ->  "3:a:bp-1"
    "3:add" + "p-1"  ->  "5:3:addp-1"

`splitUpdateRequestKey` is the only reader, so the encoding was free to change. It still accepts the
NUL form: those keys cannot exist on PostgreSQL — the write that would have persisted one is the
write that failed — but MySQL and SQL Server accepted them, so a workflow suspended mid-update on
either has one in its history and must still replay. The two forms cannot be confused, because a
length-prefixed key never contains a NUL.

**The test that would have caught it, and did not exist.**
`TestAnUpdateDeliveryEventPersists` writes the delivery event through a real store on all three
dialects. Falsified by restoring the NUL, it reproduces both failure modes at once:

    postgres  append one event: exec step 0: pq: unsupported Unicode escape sequence (22P05)
    mysql     the persisted request key contains a NUL: "bump\x0001234567-..."
    mssql     the persisted request key contains a NUL: "bump\x0001234567-..."

**This is the third defect in one feature traceable to one missing test**, with [§3.245](#3245) and
WS-3's withdrawn #910. `WithUpdateStore` had exactly one test, against a fake whose
`GetPendingUpdateRequests` ignores its `workflowID` argument, and the end-to-end update tests run
against `cleattest.NewTestEnv()` — no engine, no store, no guest. **A Go string holds a NUL
happily; only a database objects.** Every assertion about this path was being held up by the layer
that could not fail.

### 3.245 A failed update could not be recorded as failed, so the caller's promise never settled — 🟢 **FIXED 2026-09-07** (WS-1, 2026-09-07)

Reported by WS-3 as #908 against the stranded-update sweep. It is wider than that: **every** failing
update, not only stranded ones.

`workflow_update_requests.result` is JSONB on PostgreSQL and JSON on MySQL, and `""` is not valid
JSON. Every failing update completes with an empty result **by construction** —
`cleat/runtime_updates.go` passes `""` on all three failure paths (no handler registered, validator
refusal, handler error) and `cmd/cleat-worker/setup.go:2667` passes it for a stranded one.

So the `UPDATE` errored, the row stayed `pending`, and the caller's promise was never settled —
[§3.238](#3238)'s defect restored on the failure path, reaching every caller whose update was
refused. In the stranding sweep the error also skipped `RejectPromise` via `continue`, and the
summary logged `count: len(updates)` a line below the ERROR, so it reported success.

**All four call sites discarded the error with `_ =`**, which is why it was silent in the guest as
well as in the log. They now go through `completeOrLog`, which cannot recover — the row is the
host's and the next poll redelivers — but turns a silent hang into something a worker log shows.

**Confirmed by measurement rather than by reading**, since the claim is about a database:

    SELECT ''::jsonb   ->   pq: invalid input syntax for type json (22P02)

**Fixed at the store layer, not at the four callers.** `jsonOrNull` renders `""` as SQL NULL in all
three dialects. The column is nullable everywhere and `GetPendingUpdateRequests` already reads it
back through `COALESCE(..., '')`, so NULL round-trips to `""` and nothing above the store sees a
difference. Fixing the callers would have left the next one to rediscover it.

**Why nothing caught it: `CompleteUpdateRequest` had four test doubles and no test.**
`fakeUpdateStore`, `stubWorkflowStore`, `mockCollectMetricsStore`, `mockGCStore` — every appearance
in the suite was a mock implementing the interface. A double accepts `""` happily, because a Go
string has no opinion about JSON; only a database does. That is CLAUDE.md's *"watch which layer is
holding the test up"*: the assertion passed on the strength of the layer that could not fail.

`TestAFailedUpdateCompletesAndSettlesTheCallersPromise` runs against real databases on all three
dialects, and asserts the row actually leaves `pending` — an `UPDATE` matching zero rows also
returns nil.

**A claim in the first draft was wrong, and the falsification caught it.** I wrote that SQL Server
*accepted* `""`, having read `migrations/mssql/001_schema.sql`, which CHECKs `payload` and not
`result`. The constraint is added by `037_json_column_checks.sql`. All three dialects refuse it,
each in its own way:

    postgres  pq: invalid input syntax for type json (22P02)
    mysql     Error 3140 (22032): Invalid JSON text: "The document is empty."
    mssql     conflicted with CHECK constraint "ck_workflow_update_requests_result"

Reading the first migration and concluding is exactly what the *Project state* section warns
against, and it was a paragraph I had quoted earlier the same day.

### 3.244 The Rust SDK read past its own buffer whenever the host refused a bad parameter — 🟢 **FIXED 2026-09-07** (WS-1, 2026-09-07)

`memory::read_string(ptr, len)` takes a raw pointer and does
`slice::from_raw_parts(ptr, len as usize)`. It bounds nothing. All 40 call sites in
`host_calls.rs` passed it a length the **host** reported.

On the success path that is fine — the host wrote that many bytes. On a **bad-parameter refusal**
it is not. `engine/imports.go` returns `errBadParam` = `0xFFFFFFFF_00000001` from **54 sites**,
*before the handler runs*, so nothing has been written to the buffer at all — and the guest decodes
a length out of the very bits carrying the sentinel:

| layout | decoded length | buffer | overrun |
|---|---|---|---|
| `decode_simple_result` | 4,294,967,295 | 65,536 | ~65,535× |
| `decode_cleat_call_result` | 16,777,215 | 65,536 | ~256× |

Any wrapper that reads its buffer on the error path — which is the **right** thing to do, since the
host's real message is usually there — therefore read far out of bounds. Seven did:
`cleat_call`, `cleat_call_heartbeat`, `cleat_fetch`, `plugin_call`, `plugin_call_streaming`, and
`schedule_cron` / `list_crons`.

**Two of those seven are mine, from [§3.242](#3242), merged hours earlier.** That PR argued at
length that reading the buffer is correct and that following the file's majority would be "the easy
call and the wrong one". It was right about that and wrong about the read: it cited
[§3.200](#3200), which says in as many words that `hostErrMessage` *"bounds-checks the length
against the buffer, so `errBadParam`'s `0xFFFFFFFF` decodes to a length no buffer satisfies"* — I
read that sentence, quoted the section, and did not apply it.

**Guest-reachable, not theoretical.** `cleat_schedule_cron` alone returns `errBadParam` from 4
sites in its own wrapper, on a workflow name, cron expression, timezone or input that fails
validation — all guest-supplied.

**Java was already safe**, which is why this is Rust-only: `readOutput` has always clamped with
`Math.min(maxLen, OUT_BUF_SIZE)` and returns `""` for a non-positive length, and
`decodeSimpleExtra` renders `0xFFFFFFFF` as `-1`. Go's `hostErrMessage` bounds-checks. Rust was the
one SDK with no bound anywhere.

**Fixed as a mechanism rather than at the seven sites.** `memory::read_result(buf: &[u8], len: u32)`
takes the **slice**, so the capacity travels with the data and there is no second argument to get
wrong; it is entirely safe code, because these buffers are ordinary `Vec<u8>` and reading them back
never needed `unsafe` at all. All 40 sites converted; `host_calls.rs` now contains zero
`read_string` calls.

`read_string` itself is kept, deliberately: `cleat-macro`'s generated entry point calls it on
`(args_ptr, args_len)` handed in by the host, where there is no slice to bound against. That is a
different situation from reading back a guest-allocated buffer, and conflating them is what the
guard exists to prevent.

**Falsified in both directions**, because a clamp has two ways to be wrong and only one of them is
the bug being fixed:

| control | result |
|---|---|
| remove the clamp | `read_result_clamps_a_bogus_length_to_the_buffer` panics on a slice-index |
| clamp to the whole buffer always | `read_result_does_not_round_a_short_length_up` fails — a short length must not be rounded up, or every success gains thousands of NULs |
| reintroduce one `read_string` | the guard names the file and line |

The second is the one worth having: it fails a "fix" that passes the first.

The sentinel test asserts against `errBadParam`'s **real value** rather than a made-up large number,
so it stays true only while the engine's constant does.

`tests/plugin-harness` on all three dialects: 145 pass / 0 fail / 2 skip, unchanged — the Rust
harness drives 27 host calls through the converted reads.

**Still open, and this was the prerequisite for it:** 15 of 22 Rust and 11 of 20 Java wrappers
still report a bare error code where the host wrote a message ([§3.200](#3200)'s defect in two more
SDKs). Fixing those in Rust *requires* this change first, or it would have added 15 more
out-of-bounds reads.

**A citation error went out with [§3.242](#3242) and is corrected here.** Four merged files and a
plan section cited "IMPROVEMENT-PLAN 3.258" for the host-message defect. **There is no §3.258.** I
had run `sed -n '3250,3268p' IMPROVEMENT-PLAN.md`, read the passage about `hostErrMessage` at
**line** 3258, and written it down as a **section** number. The real section is [§3.200](#3200),
whose heading — *"A Go guest was told 'error 1 (timeout)' for every plugin failure, and the host's
real message was in the buffer beside it"* — is the thing I was describing all along.

A number that looks like a section number and came from a line-numbered tool is the same shape as
this file's `TestTenantIsolationAcrossDialects`: a name that existed only in prose, cited
confidently, matching nothing. **Check that a `§` you cite resolves to a heading** — one grep, and
it would not have shipped:

    grep -c '^### 3\.258 ' IMPROVEMENT-PLAN.md     # 0

The same pass corrected two denominators that [§3.242](#3242) itself had made stale within the
hour: the Rust and Java wrapper counts read "of the 20" and "of the 18", the totals *before* cron
added two wrappers to each.

### 3.243 Java's executed host-call coverage skipped its own fixture, so 8/70 was not a fact about Java — 🟢 **FIXED 2026-09-07** (WS-1, 2026-09-07)

`scripts/sdk-host-call-coverage.py` reported java at **8/70 executed** against rust's 27/70 and
go's 26/45. That gap was not about Java.

`tests/plugin-harness/testdata/hostcallsjava/` exists, is built and executed by `TestHostCallsJava`,
and exercises two dozen host calls. It was simply **not in java's `EXECUTED` globs**, where go, rust
and assemblyscript all list their own:

| SDK | lists its own `hostcalls*` fixture |
|---|---|
| go | ✅ | 
| rust | ✅ |
| assemblyscript | ✅ |
| **java** | ❌ — only `javaworkflow/**` and `saga-java-port/**` |

Adding it moves java from **8 to 26**, with **no Java code changed at all**. So this is a
measurement correction, not an improvement, and the distinction is the whole entry: the number was
never a statement about the SDK's coverage. It was a statement about which files the scan opened,
and it read as the former.

**Found because [§3.242](#3242) made it visible.** Binding cron in Rust *and* Java raised rust's
executed count 25 → 27 and left java's at 8. Two SDKs, the same two calls wired into the same
harness, one number moving — which is the shape that says the instrument is wrong rather than the
subject.

**Fixed as a mechanism, not a sweep.** `check_hostcall_fixtures_are_counted()` walks
`tests/plugin-harness/testdata/hostcalls*` and requires each SDK's `EXECUTED` entry to cover its own
fixture. A per-SDK list of globs is exactly the kind of thing that gets four entries right and omits
the fifth, and nothing in the output says which happened. **Anchored on the fixture directory
existing**, not on a name appearing in the script, so a comment cannot satisfy it.

Known-positives, both directions: removing java's glob reports java; removing rust's reports rust.
The check is general, not fitted to the case that prompted it.

**One stale sentence went with it.** The rust entry's `why` read *"22 of the 24 wave-1 arms make a
call ... the two cron arms have no Rust binding"* — true when written, false the moment §3.242
landed an hour earlier. Corrected to 24 of 24, with the old claim and its expiry recorded rather
than silently overwritten.

### 3.242 The cron family is bound in Rust and Java — 🟢 **FIXED 2026-09-07** (WS-1, 2026-09-07)

`tiers.yaml` holds `workflow-callable-cron` at **tier 2** for one stated reason: *"rust and java
SDKs declare no cron surface at all"*. Both now do.

`cleat_schedule_cron`, `cleat_delete_cron` and `cleat_list_crons` are declared in
`crates/cleat-sdk/src/host_calls.rs` and `crates/cleat-java/.../HostCalls.java`, with safe wrappers
mirroring Go's `ScheduleCron(workflowName, cronExpr, timezone, inputJSON) -> (scheduleID, error)`.

**Only `ScheduleCron` checks the stop bit, and that asymmetry is measured rather than copied.**
`engine/schedules.go` calls `stopBeforeNewWork` in `ScheduleCron` and in neither of the other two —
a cron schedule is new work with the longest reach of anything in this family, since it registers a
*recurring* trigger, while deleting and listing are not new work. Verified 2026-09-07 by reading
all three handlers; AssemblyScript already had it this way.

**The error branches read the OUTPUT BUFFER, which is deliberately not what the rest of either file
does.** `engine/schedules.go` writes its message into the id buffer and returns
`packSimpleResult(1, written)`, so a guest printing the bare code discards the only thing that says
what went wrong — [§3.200](#3200), fixed there for the generated Go adapters. Measured across both
SDKs on 2026-09-07, counting only `read_string`/`readOutput` **inside** the error branch:

| SDK | wrappers with an output buffer | read it on error | report a bare code |
|---|---|---|---|
| rust | 20 | 5 | **15** |
| java | 18 | 7 | **11** |

Following the majority would have been the easy call and the wrong one. **The first measurement of
this said 20 of 20 read the buffer**, because the detector looked for `read_string` anywhere after
`err_code != 0` — and every one of these reads the buffer on the *success* path, immediately below.
The corrected detector matches braces. That is this document's recurring shape again, and again in
the flattering direction: the wrong answer said the SDK was already doing the right thing
everywhere.

The remaining 26 are a real defect and are **not** fixed here — one PR, one thing.

**Proven by execution, not by declaration.** `ScheduleCron` and `ListCrons` are wave-1 calls, so
both SDKs already had rows asserting the *gap*: `statusUnsupported`, "no cleat_schedule_cron
import". Java's row said, in as many words, that the day Java gained the binding somebody would
have to decide the right answer. The answer is that the call reaches the host and is refused by it,
which is a different fact from having no binding. Recorded with `CLEAT_HOSTCALL_RECORD=1`:

    RECORD  ScheduleCron  error  no workflow store configured: workflow <run-id> cannot schedule "harness-workflow"
    RECORD  ListCrons     error  no workflow store configured: workflow <run-id> cannot list schedules

**Byte-identical between Rust and Java**, which is the strongest thing this pair can say: two SDKs
that spell the import differently encoded four arguments into the same host answer. The rows assert
the tail rather than the whole string, because the host's text embeds the run ID; the substring
chosen is the one that proves the *argument* crossed — `harness-workflow` is what the fixture
passed, coming back inside a message the host composed.

**Falsified.** Reverting the Rust error branch to a bare code fails the row with *"status error as
expected, but the detail changed"* — so the row asserts the host's message specifically, not merely
that something failed. Restored from a saved copy and re-verified, not with `git checkout`.

**[§3.241](#3241)'s baseline worked on its first real use.** Applying the bindings turned that test
red with *"sdkUnreachedBaseline[\"rust\"] names 3 host export(s) this SDK now reaches"*, naming all
three, for Rust and Java both. The shrink-only direction is the half that is easy to get wrong,
because nothing else notices an improvement.

`tests/plugin-harness` on all three dialects: 145 pass / 0 fail / 2 skip. `cargo clippy
--all-targets -- -D warnings` clean; `gradle test` clean.

**What this does NOT do: it does not move `workflow-callable-cron` to tier 1.** That entry gives
two reasons for its tier, and this closes one. The gate coverage it asks for is a real cron
end-to-end on each SDK — `engine.TestPythonCronEndToEnd` is the model — and the harness rows here
run against an env with no workflow store, so they prove the binding and the boundary, not the
scheduling. Changing `tiers.yaml` is a separate decision with its own evidence.

### 3.241 Nothing checked whether an SDK can reach every host call — 🟢 **GUARDED 2026-09-07** (WS-1, 2026-09-07)

`TestEverySDKImportIsAHostExport` checks that every name an SDK imports exists on the host. That
direction fails loudly — the guest does not instantiate. **Nothing checked the reverse**, and the
reverse fails silently: the capability simply does not exist in that language, and nothing
anywhere says so.

The two are not redundant. **An SDK that binds nothing passes the forward test perfectly.**

`TestEverySDKReachesEveryHostExport` adds the reverse, reusing the same five extractors — which
are now a shared `sdkImportSources` table, so a fix to a parse improves both directions. Measured
2026-09-07, over the 49 workflow-facing exports (52 less the worker handshake pair and the
deliberately unbindable `cleat_register_query_handler`):

| SDK | cannot reach | what |
|---|---|---|
| **assemblyscript** | **0** | full parity |
| rust | 3 | `cleat_schedule_cron`, `cleat_list_crons`, `cleat_delete_cron` |
| java | 3 | the same cron trio |
| python | 5 | but only **3** are gaps — the same `json` caveat as Go, see below |
| go | 6 | but only **3** are gaps — see below |

**AssemblyScript, not Go, is the only SDK at full parity.** That is not what anyone would have
guessed, and it is the reason this direction was worth checking. It is also the one row here that
no existing document states.

**The raw count overstates Go, and saying "6" would have been the flattering-direction error this
document keeps recording.** Three of Go's six are reached another way and adding the host call
would be redundant:

- `cleat_json_parse` / `cleat_json_stringify` — `encoding/json` is in the standard library. The
  host call exists for guests whose language has no JSON. Verified pure rather than assumed:
  `JsonParse` and `JsonStringify` in `engine/lifecycle.go` unmarshal, re-marshal and write the
  result — no `recordEvent`, no store, nothing durable — so a guest using its own JSON diverges
  from nothing.

  **The same applies to Python, which this section originally got wrong.** It listed all five of
  Python's as real gaps, saying "unlike Go's, none of these has a native or composed substitute".
  Python has the `json` module — `host_calls.py` imports it three times — so two of the five are
  the same non-gap they are in Go. Python's real count is **3**:
  `cleat_await_any_child`, `cleat_poll_child`, `cleat_run_detached`.

  The error is worth recording rather than quietly fixing, because it ran the *opposite* way to
  this document's usual one: it made the project look worse rather than better, which is why
  nothing about it felt like it needed re-deriving. A number that flatters goes unchecked; so, it
  turns out, does one that indicts.
- `cleat_uuid` — already durable as `SideEffect(func() string {...})`, and Go binds
  `cleat_side_effect`. Worth stating precisely, because the near-miss is a determinism bug: a
  native `uuid.New()` is **not** replay-safe. The host call is a convenience over the safe form,
  not the only safe form.

Go has **no real gaps left**. `cleat_get_scope` / `cleat_set_scope` were the last two and were
bound on 2026-09-09 ([§3.223](#3223), cleat#984); the `sdkUnreachedBaseline` entry shrank
accordingly rather than being re-labelled.

**This said "three real gaps", counting `cleat_fetch`, until 2026-09-08. Go reaches durable HTTP.**
`DurableFetch`, `DurableFetchJSON`, `FetchGet` and `FetchGetJSON` all map to `cleat_call`
(`wasm/usage.go:119-123`, whose own comment says "all map to durable_call import"), issuing
`DurableCall("http", "fetch")`. **Both** `ServiceCaller` implementations intercept that pair
*before* any plugin lookup — `cmd/cleat-worker/setup.go:155`, the production worker, with
idempotency-key support, and `cleat/embedded/runner.go:394` — and
`cmd/cleat-worker/service_caller_errors_test.go` drives it against a live `httptest` server. It is
durable and replayable through the `cleat_call` event rather than `EventTypeFetch`.

The reason it survived is the one this section already names, in its third form. The parenthetical
*"`net/http` in a guest is neither durable nor replayable"* is **true**, and answers whether a
**native** substitute exists. It sat under a heading asserting no **composed** one does either —
a different claim, never separately checked. Worse, the tree offers false corroboration: there is
no `http` plugin in `plugins/` and nothing registers that name, so the obvious check agrees with
the wrong answer. The interception lives in the `ServiceCaller`, above the registry, where a
search for a plugin cannot find it.

So the sentence above about a number that indicts going unchecked has a companion: **a true
sentence filed under the wrong question is not checked either**, because re-deriving it confirms
it. What settles this one is not a better grep but a different question — not "is there an http
plugin" but "what handles `cleat_call` before the registry".

**The cron trio was already known, and this test did not discover it.** `tiers.yaml` holds
`workflow-callable-cron` at **tier 2 for exactly this reason** — "rust and java SDKs declare no
cron surface at all", with tier 1 requiring only `[go, python]` — and §3.170's coverage table
already recorded rust at 52/55 naming the same three. Re-deriving it independently is
corroboration, not a finding, and presenting it as new would be its own kind of inflation.

What is new is that it is now **guarded**. The gap was recorded in two places that a code change
cannot fail, so nothing stopped a third SDK from drifting the same way, or these three from
widening to four. The baseline is shrink-only and lives next to the extractors, so the next
regression is a red test rather than a paragraph someone has to remember to re-read.

The one substantive addition to what tiers.yaml says: **nothing composes cron.** Unlike
request/reply after [§3.220](#3220), there is no combination of other host calls that schedules
one, so the gap cannot be worked around in-language.

Held per-SDK in `sdkUnreachedBaseline`, shrink-only, each entry carrying its reason.

**Known-positives**, because the empty-baseline version passed for four of five SDKs:

| control | result |
|---|---|
| add a host export bound by no SDK | all **5** SDKs report it as widening |
| put a name in a baseline the SDK does reach | fails, demanding the baseline shrink |
| break an extractor | the floor fires — and note a broken extractor here reports the ABI as *unreachable*, the opposite direction from the forward test |

**The new test was selected by no CI job**, and `-list` is what showed it. The workflow's term was
`EverySDKImportIsAHostExport`, which stops matching one character into
`TestEverySDKReachesEveryHostExport` — `I` against `R`. Exactly the `TestHostCalls` /
`TestHostCallTable…` case this document already records. Widened to `EverySDK`:

    cd tests/plugin-harness && go test . -list 'EverySDK' ./...   # 2, was 1

### 3.238 A pending update request outlived the workflow it was for, and its promise never settled — 🟢 **FIXED 2026-09-06** (WS-1, 2026-09-06)

The smaller half of [#849](https://github.com/cleat-team/cleat/issues/849)'s suggested direction.
`POST /api/workflows/:id/update/:name` returns `202` and a `promise_id`. An update is dispatched
only while its workflow is mid-segment, so a request still pending when the workflow reaches a
terminal status can never be handled — and the caller is left holding a `promise_id` for a promise
nothing will ever settle, against a workflow that no longer exists. The wait was permanent and
silent.

`Worker.failStrandedUpdates` now completes each such request with a reason and rejects its promise
with the same reason, from all three terminal paths: a successful terminal finalize, a terminal
failure (`recordTerminalFailure`), and the defer phase applying its recorded outcome
(`finishDeferPhase`).

**Composed, not added to the store.** `GetPendingUpdateRequests`, `CompleteUpdateRequest` and
`RejectPromise` are all already on `engine.WorkflowStore`, so this needed no SQL and no
per-dialect work — a new store method would have been four implementations (postgres, mysql,
mssql, `ShardedStore`) to express something the existing three already say. Same reasoning as
§3.220's composites.

**The test drives the terminal path, not the helper**, because the defect being guarded is a
*missing call*: the helper could be perfect and every caller still hang. Removing the
`recordTerminalFailure` call site fails it with `the stranded update was completed 0 times, want
1`; removing the rejection alone fails it with `the update's promise was rejected 0 times`. A
negative control asserts the path is silent when nothing is pending, which is almost every
workflow — a version that wrote unconditionally would pass the other two.

#### What this deliberately does NOT fix, and what is still broken underneath

**Updates are still never delivered.** This makes a stranded request answer rather than hang; it
does not make the feature work. Three independent breaks, in the order they have to be fixed —
recorded on #849 with the greps:

1. **No guest entry point, in any SDK.** Both SDKs register handlers into a map (Go
   `cleat/runtime_promises.go:91`, Python `python-sdk/cleat_sdk/host_calls.py:2333`) and the only
   thing that ever reads either is a **test harness** — Go's `HandleUpdate` is reached only from
   `cleattest`, and Python's `_handle_update` has zero callers. `cleat_register_update_handler` is
   a real host call the engine records, but no WASM export exists that would let the host ask a
   running guest to run one.
2. **The worker never calls `engine.WithUpdateHandler`.** Three references in the tree outside
   tests: the definition, its doc comment, and the error string naming it. So `DispatchUpdate`
   returns `no update handler configured for this engine`.
3. **Delivery is a 5s ticker over `w.inflight`**, which is populated only for the lifetime of one
   segment (#849's own finding).

Note the ordering trap: fixing 3 alone converts a silent hang into a silent *rejection*, because
2 makes `DispatchUpdate` fail and `dispatchPendingUpdates` then completes the request with that
error and rejects the promise. **A fix for 3 verified by "the request is no longer pending" would
read as success while delivering nothing.**

This is the `RegisterQueryHandler` shape (removed 2026-08-09, "it recorded a handler name but
nothing in the worker ever routed an external query to it") — and it is the whole story here
rather than a parallel. Whether to implement updates end-to-end or stop advertising the API is a
product decision; the `202` is untouched here for the same reason.

### 3.237 `migrations/mssql/001_schema.sql` cannot be re-applied once migration 031 has run — 🟢 **FIXED 2026-09-07** (WS-1, 2026-09-06)

Split out of [§3.236](#3236), which fixed the damage this causes but not the failure itself.

001 drops the seven policies it owns by name, then `CREATE OR ALTER`s `dbo.fn_tenant_filter`.
Migration 031 adds `TenantFilter_Promises` and 042 adds `TenantFilter_Settings`; both are
schemabound to that function and neither is in 001's drop list, because 001 predates them. So a
second application of 001 fails:

    Cannot ALTER 'dbo.fn_tenant_filter' because it is being referenced by object 'TenantFilter_Promises'

**The obvious repair is wrong.** Making 001's drop list dynamic — "drop every policy whose
predicate references `fn_tenant_filter`" — would drop `TenantFilter_Promises` and
`TenantFilter_Settings` *without recreating them*, because the migrations that create those are
031 and 042 and they are already recorded as applied. That turns a loud failure into a quiet loss
of two policies, which is the same shape as §3.236 with a smaller number.

The real answer is that nothing should re-apply a recorded migration. `engine/testutil` uses
`migration.Runner`, which records what it has applied and skips it; `tests/plugin-harness` has its
own loop that re-applies everything unconditionally. Moving it onto the Runner is the fix, and the
complication to measure first is `schemaPrefix` — plugin-harness prepends a per-file
`SET search_path` / `USE` and pins one connection, which the Runner does not do.

**Correction, same day: the blast radius above is wrong, and it was wrong in the direction that
makes it look smaller.** This does not affect only `TestPluginCalls_MultiDB/mssql`. It fails any
migration run against an MSSQL database that already carries the schema while its
`schema_migrations` does not record it — and when that database is the shared test one, it takes
**552 of `./engine/`'s tests** with it, every `/mssql` subtest, all reporting:

    apply mssql migrations from …/migrations: migration 001_schema.sql: execute:
    mssql: Cannot ALTER 'dbo.fn_tenant_filter' because it is being referenced by
    object 'TenantFilter_Promises'

Measured 2026-09-06 on clean `develop`; `cleat.dbo.schema_migrations` had 0 rows against 48 tables
and 9 policies. The cure is CLAUDE.md's, unchanged: drop and recreate, then 4611 pass / 0 fail.

The narrow claim was made from the one failing test that was in front of me, and generalised
without being checked against anything else — which is this document's own "a count answers 'did
this go up', it never answers 'is anything still missing'" in the shape of a blast radius.

CI stays green on all of it for the reason §3.236 gives: a fresh container never has a second
application to fail.

**Fixed 2026-09-07 (#890).** `tests/plugin-harness/testdb.go`'s `runCoreMigrations` now calls
`migration.NewRunner(...).Run(ctx)` — the same call `engine/testutil` makes — instead of its own
read-dir-and-exec loop. The Runner records applied versions in `schema_migrations` and skips them,
so a second call is a no-op and 001 is never applied twice. **Two implementations of "apply the
shipped migrations" was the defect**, not a detail of either of them; the alternative repairs all
kept both loops and tried to make the second one survive re-application.

The `schemaPrefix` complication the paragraph above says to measure first turned out to be real
for exactly one dialect:

| dialect | what the old loop's prefix did | under the Runner |
|---|---|---|
| postgres | `SET search_path TO public` | `schemaName` is hardcoded `"public"`, so the prefix was already a no-op |
| mssql | nothing — `schemaPrefix` returns `""` for it | unchanged |
| mysql | `USE <db>`, on a pinned `*sql.Conn` | **needed work** |

MySQL's current database is a per-connection property and the Runner uses the pool, so the fix
pins it for the run: `db.SetMaxOpenConns(1)` plus one `USE`, restored by `defer`. That is the
smallest change that keeps the per-test database guarantee the pinned connection used to give.
`schemaPrefix` and `setSearchPath` had no callers left afterwards and are deleted.

**The regression test got stronger, not weaker.**
`TestReapplyingTheCoreMigrationsLeavesTheTenantPoliciesStanding` (#853) was written expecting the
second application to *fail*, and asserted only that the failure left the nine policies standing.
It now asserts the second application **succeeds**. That is the sharper claim: "it does not damage
anything" has become "it does not even try", and a regression to re-applying fails on the error
rather than on a policy count.

Known-positive, measured 2026-09-07 — deleting the rows from `schema_migrations` before the second
call forces the Runner to re-apply, and the test reports it:

    re-applying the core migrations failed: apply mssql migrations from ../../migrations:
    migration 001_schema.sql: execute: mssql: Cannot ALTER 'dbo.fn_tenant_filter' because it is
    being referenced by object 'TenantFilter_Promises'

Verified on all three dialects against an already-migrated database — the condition that used to
fail — with `tests/plugin-harness` at 139 pass / 0 fail / 2 skip (`TestBlobstore_S3` and
`TestPluginCalls_Wasm_Python`, both environmental). MSSQL policy count stayed at 9.

**And the first run of that suite was measured against two DSNs reconstructed from memory**, which
reported postgres and mysql failing with `28P01` / `1045` — this file's opening section exactly,
caught only because the errors were authentication rather than schema. WS-1's rows are 5432/3306/
1433 with `postgres:postgres` and `root:cleat`; read them from WORKSTREAM.md.

### 3.201 The Python SDK discarded the host's answer on 13 calls, so a refusal read as a success — 🟢 **FIXED 2026-09-04** (WS-2, 2026-09-04)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 3.202 A stop read as a timeout on `await_signals`, so a Python defer segment ran on — 🟢 **FIXED 2026-09-04** (WS-2, 2026-09-04)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 3.401 The scale suite's wall-clock thresholds measured the CI host, not cleat — 🟢 **FIXED 2026-09-04** (WS-3, 2026-09-04)

`tests/scale/latency_test.go` carried two wall-clock assertions: `p50 > 100ms` and
`p99 > 500ms`. The second failed on develop at `491a0f7` (#720) with
**P99 623.876463ms over a P50 of 2.676839ms**, one failure in the scale job's last 20 develop
runs.

The thresholds are **removed, not widened** — CLAUDE.md: *"If an assertion depends on wall-clock
time, remove the timing rather than widening it."*

#### The hypothesis that had to be refuted first

Removal is the cheap answer, and the cheap answer is wrong if the number means something. The
opening read of that failure was that it did: two near-identical outliers (623.876ms and
625.203ms) over a 2.7ms median look like a **fixed stall** — a lock wait, a retry backoff, a pool
timeout — rather than a slow machine, and a fixed stall deserves its own section rather than a
deleted line. That is the right instinct and it is worth writing down that it did not survive
contact with more than one run.

Three measurements over the scale job's last 20 develop runs. Re-derive all three with
`scripts/scale-latency-history.py`:

1. **The magnitude is not fixed.** `TestLatencyP99`'s P99 across those runs is a continuum over
   three orders of magnitude with no cluster anywhere, least of all at 624ms:

       4.4, 4.8, 5.2, 5.2, 5.9, 6.0, 6.4, 6.5, 7.3, 13.7, 14.5, 15.1, 15.2, 16.2,
       18.4, 27.1, 33.0, 92.1, 377.4, 623.9   (ms; median 14.1, max 44x the median)

2. **The "near-identical pair" recurs at other magnitudes.** `b35c52f`'s top two were 377.4ms and
   387.9ms — the same shape at 60% of the size. Four goroutines are in flight at once, so
   whatever is in flight during one host stall window all records that window's length. The pair
   is a signature of the concurrency, not of a constant in the code.

3. **The sequential test shows the same tail.** `TestLatencyP50` runs one goroutine — no lock
   contention, no pool competition, and no retry path anywhere in `AppendEventHistory`, which
   `BeginTx → setRLS → SELECT prev checksum → INSERT → UPDATE → Commit` with no loop. Its Max was
   **219.3ms on the failing run** and 126.9ms on `b35c52f`, and across the 20 runs it correlates
   with the concurrent test's Max at **Pearson r = 0.937**.

(3) is the one that settles it. No lock, backoff or pool timeout inside the code under test can
slow down a single goroutine that contends with nothing; a slow host slows down both tests, which
is exactly what the correlation says happened. The tail is a per-run property of the runner, and
any fixed threshold under ~700ms sits below its noise floor.

#### What replaced them

Not nothing, and not a bigger number. Both tests now call `assertAllSampled`, which fails if any
slot in the fixed-size latency slice was never written. That closes a real hole the thresholds
never looked at: in `TestLatencyP99` a goroutine that fails its INSERT calls `t.Errorf` and
returns **without writing `latencies[idx]`**, and a zero left behind sorts to the FRONT, pulling
both the median and the P99 down. A run that measured fewer samples than it claimed would report
itself as *faster*, not as broken.

Falsified before landing: mutating one goroutine to skip its record produced
`1 of 200 samples were never recorded`, with `Min: 0s` in the logged distribution above it —
red for the stated reason, not for a neighbouring one.

`TestLatencyUnderConcurrency` in the same file already worked this way — it measures, logs, and
asserts nothing about the clock. The other two now match it, so the file is internally consistent
for the first time.

#### What this does not fix

The scale job stays a **required** status check while `./tests/scale/...` is tier 2, which is a
separate defect — see §3.402. Removing a flaky assertion makes that gate quieter; it does not
make it correct.
### 3.402 Five required checks gated tier-2 code, and nothing in the tree said so — 🟢 **CLOSED 2026-09-05; both packages now tiered** (WS-3, 2026-09-04)

Branch protection on `develop` lists 32 required status contexts. That list lives in
GitHub, this repo's tier claims live in `tiers.yaml`, and **nothing compared them.** The
reported symptom was one context — `Test Go (scale) on 1.26` gating `./tests/scale/...`,
which `tiers.yaml` puts in tier 2, whose contract says "may fail". Measuring it found
five, plus two packages in no tier at all.

#### What is actually required, measured 2026-09-04

Re-derive with `scripts/check-required-contexts.py --report`:

| required context | runs | tier |
|---|---|---|
| `Test Go (plugins) on 1.26` | `./plugins/...` | **2** |
| `Test Go (manifests) on 1.26` | `./tests/manifests/...` | **2** |
| `Test Go (scale) on 1.26` | `./tests/scale/...` | **2** |
| `Test Go (support) on 1.26` | `./migration/...` | **2** (mixed with three tier-1 packages) |
| `Cluster Integration Tests` | `./tests/cluster/...` | **2** |
| `Test Go (support) on 1.26` | `./monitoring/...`, `./packaging/...` | **none** |

And the mirror image, which is *not* a hole but reads like one: **`Test Go (crash)` is the
only matrix entry that is not required**, and it runs a tier-1 package. `./tests/crash/...`
is in `tier1.packages`, and the required `Tier 1 Gate` runs that whole list, so it is
covered — by a different context than the one whose name suggests it.

#### The contract was already stricter than its own prose

`tier2.contract` says "must run; may fail, against `known_failures`". `known_failures` is
**empty**, and `scripts/tier2-gate.sh` fails on any failure not in it, and `Tier 2 Gate` is
required. So tier 2 is enforced as must-pass today, by a mechanism independent of the five
contexts above. "May fail" is a door, not a state: it opens one test at a time, and only
with an item reference and an owner.

That is the honest reading of the reported defect. It is not that scale is special. It is
that **tier 2 is gated as must-pass in two independent ways and the manifest described
neither**, so any of its packages going red blocks the queue while `tiers.yaml` says it is
permitted to fail.

#### What changed

**The enforcement was not loosened.** Tempting, and wrong: the prose was the inaccurate
half. Loosening the gates to match the sentence would delete real coverage to make a
sentence true.

1. `tier2.contract` now describes what is enforced, with the empty list and the required
   gate named.
2. `tiers.yaml` gains a **`required_contexts:` block** — all 32, each mapped to its
   workflow and job id, each classified `tier1` / `tier2` / `undeclared` / `infra`, and
   every non-tier-1 entry carrying a `why_required` that has to argue for itself.
3. `scripts/check-required-contexts.py` enforces it, wired into `Lint`.

On **`Test Go (scale)`** specifically: it stays tier 2 and stays required. Tier 1 is not
available to it — tier 1 means green on every dialect, and every file in `tests/scale`
calls `engine.NewPostgresStore` against `testutil.DialectPostgres`, so there is nothing
for the other two to run. Promoting it would mean granting tier 1 to a Postgres-only
package, which is the sort of claim `tiers.yaml` exists to refuse. It is defensible as a
required check only because §3.401 removed its wall-clock assertions; requiring a green
from an assertion the runner controls is how a gate teaches people to re-run instead of
read.

#### What the guard cannot do, and one thing it caught in itself

It cannot see branch protection. Reading it needs admin scope, which `GITHUB_TOKEN` does
not have — the same limitation `tier2.gated_by` already records. `--check-live` does the
diff for a caller who has the scope (run 2026-09-04: *"32 contexts, declared list matches
exactly"*), and `--report` prints the command for anyone else.

Its third check — "is this `covers:` claim true?" — resolves a context against the
`test-go` matrix, so it can only reach `Test Go (...)` contexts. `covers:` on the other 21
is a hand claim nothing verifies. **That limit was found by the negative control, not
declared:** the self-test's first version asked check 3 to catch a relabelled `Tier 2
Gate` and printed `MISSED`. A guard shipped without one would have carried the gap
silently.

#### Closed 2026-09-05 — both packages assigned, and the guard had a hole

`./monitoring/...` → **tier 1**, `./packaging/...` → **tier 2**. Decided on what they are, not
on where they sit in the matrix:

* `monitoring/prometheus` (1734 LOC, 1 test, 0.25s, no DSN) is imported by
  `cmd/cleat-worker/main.go`, `cmd/cleat-worker/setup.go` and `cleat/backendkit/metrics.go`, so
  it **ships inside the worker** and is reachable from the public Go API. A break already failed
  the tier-1 gate at *compile* time through `./cmd/...`; what tier 1 adds is that its own test
  now runs there.
* `packaging/homebrew` (141 LOC, 3 tests) has **zero runtime imports** — it asserts the Homebrew
  formula pins a tagged tarball, builds the worker with CGO, and runs it in its test block.
  Real, and not a product support claim, so tier 2.

Verified rather than assumed: `scripts/tier2-gate.sh` ran green with the new entry
(`ran=1546 pass=1534 fail=0 skip=12, 0 regressions`) and its JSON shows all three
`TestFormula*` tests actually executing, so the new pattern is not matching nothing.
`scripts/tier-gate.sh` **refused to run locally** — `wasm-tools` is not on PATH and it fails
closed rather than printing a green that measured nothing — so the tier-1 half is verified by
running `./monitoring/...` directly (`ok, 0.200s`) plus CI.

**`Test Go (crash)` stays unrequired, decided rather than left.** `./tests/crash/...` is tier 1
and the required `Tier 1 Gate` runs the whole list, so correctness is gated. What is genuinely
non-blocking is narrower than "the crash tests": the matrix job runs `go test -race` and the
gate does not, so a **data race** in them would not stop a merge. Judged not worth a required
context; recorded because "the only one of twelve that is not required" reads as an oversight
and has now sent two readers chasing it.

#### The guard shipped with a hole, and assigning the tiers is what exposed it

`check-required-contexts.py` checked a stale `covers: tier1` and a stale `covers: tier2`, and
**not** a stale `covers: undeclared`. So the moment the two packages got tiers, the
`Test Go (support)` entry went on claiming they had none — and the guard passed. It printed
`OK, 32 required contexts declared and consistent` over a manifest whose own header still said
"IN NO TIER".

That is the failure mode this script's docstring is about, in the script itself. It is also a
particular shape worth naming: **an `undeclared` label rots by being *fixed*.** The other two
labels rot when someone changes a tier; this one rots when someone closes the gap it exists to
report, which is exactly when nobody is looking for it.

Fixed, with a seventh self-test case (`a context still marked undeclared after its packages got
a tier`), which fails against the pre-fix script and passes after.

#### And a second hole: the list already existed somewhere else

`.github/required-checks.txt` has held the same 32 context names since 2026-08-07, and
`scripts/check-workflow-guards.py` reads it. **The `required_contexts` block shipped in #729 as a
second hand-maintained copy of that list, and nothing compared them.** Found while wiring §3.403,
not by any guard.

That is the exact shape `.golangci.yml` refuses for `unused`: *"one class of finding two
mechanisms with two baselines, which is the shape that let the routing tables in 2.72 drift
apart."* The irony is not incidental — this block exists because branch protection and
`tiers.yaml` were two uncompared copies of one fact, and closing that gap introduced a third
copy.

They were identical when checked (32 = 32, empty symmetric difference), so nothing had drifted
yet. Check 5 now asserts equality in both directions, with the two failure modes named
separately: a context only in the file is undeclared here, and a context only here is **not
checked against the workflow jobs at all**, since `check-workflow-guards.py` reads the file.

Its self-test case drops a context **and decrements `total`**, so only check 5 fires —
WORKSTREAM.md's protocol: *"A falsification that fires two assertions proves neither."*

The division of labour is now explicit rather than accidental: the `.txt` is the list of *which*
contexts are required, and the `tiers.yaml` block is *what each one covers and why*.


### 3.403 Nothing in CI looked for committed credentials — 🟢 **FIXED 2026-09-05** (WS-3, 2026-09-05)

Enabling gosec (§3.33, #731) excluded G306 and every `_test.go`, and both exclusions carry a
written note that they stop catching a credential written to disk or committed in a test.
Nothing else covered it — measured 2026-09-05, `grep -rilE "gitleaks|trufflehog|secret.scan"
.github/ scripts/` returned **nothing**. gosec's G101 was never that tool: it fired three times
on this tree and was wrong all three (a usage string, an ephemeral test-role password, a
localhost default DSN).

`gitleaks` v8.30.1 now runs in `Lint`, pinned, against `.gitleaks.toml`.

#### What the measurement found, and why it decided the design

| scan | findings | what they are |
|---|---|---|
| `gitleaks dir .` (working tree) | **7** | all placeholders — `cleat_sk_testvalidkey123`, `tok_abc123def456`, `cleat_sk_abc123...` (a docs example, ellipsis and all), two `a1b2c3d4e5f6g7h8` idempotency keys, `incident_abc123`, and a JWT in `engine/redact_test.go` that exists *so the redaction test has one to redact* |
| `gitleaks git .` (1234 commits) | **16**, 9 unique locations | 8 the same placeholders, and **one real 64-hex `cleat_sk_` agent key** in `clew-agent.json`, plus its copy in `cmd/cleatctl/revokeapikey_test.go` until #480 removed it |

**The gate scans the working tree, never the history**, and that is the load-bearing decision
rather than a default. The real historical key is **known and closed**: public since `1dcf116`
(2026-06-06), and measured 2026-08-30 it is absent from the production database, so every
lookup's `WHERE key_hash = ? AND revoked_at IS NULL` misses it and revocation is a no-op. There
is a standing decision not to rewrite history for it — that would invalidate the `v0.2.0` tag,
`main` and every clone, and GitHub serves unreferenced blobs by SHA until support purges them,
so the rewrite would not even un-expose it.

So a history scan is permanently red on something nobody intends to fix. The only ways to green
it are to baseline a real-looking credential — the one entry that should never be easy to add —
or to leave the check red, which trains people to ignore it. A tree scan fails on anything
**new**, which is the actual goal.

#### The allowlist is by VALUE, not by rule and not by path

Seven regexes matching the exact placeholder strings. Disabling the `generic-api-key` rule, or
excluding `auth/` and `plugins/`, would each also hide the next real secret in the same file.

**Falsified, because an allowlist is exactly the thing that can silently pass everything.** Two
secrets were planted — one in a new file, and one appended to `auth/middleware_test.go`, the
same file as an allowlisted placeholder, specifically to test that the allowlist is value-scoped
and not file-scoped:

    generic-api-key    auth/middleware_test.go:639
    generic-api-key    zz_probe_secret.go:4

Both caught; removed; re-scan clean. A third probe, `AKIAIOSFODNN7EXAMPLE`, was **not** flagged
— that is gitleaks' own default allowlist for the canonical AWS documentation key, which is
correct, and is recorded here so the next person does not use it as a probe and conclude the
scanner is broken.

#### The first version of the CI step failed, and local verification could not have caught it

`go install` then bare `gitleaks` -> **`gitleaks: command not found`, exit 127**, *after* the
install succeeded. `go install` writes to `$(go env GOPATH)/bin`, which is not on `PATH` in the
`lint` job. What made it look safe is that `lint-go` two jobs down does exactly this with
`golangci-lint` and works; both jobs run `actions/setup-go@v7`, so the difference is not the
obvious one and was not worth chasing.

**Locally it could not fail**, because every local run invoked the binary by absolute path out of
`$(go env GOPATH)/bin` — so the local command and the CI command were different commands, and
only the one that was never run locally was wrong. Now `"$(go env GOPATH)/bin/gitleaks"` in CI
too, which does not depend on whatever the PATH difference is and makes the two identical.

Verified by extracting the step's `run:` block straight out of the parsed YAML and executing
*that*, rather than a hand-retyped approximation of it: `bash -n` clean, then `no leaks found`,
exit 0.

#### What it does not cover

History, deliberately, per the above. And the scan is ~10s over 320 MB, so it is cheap enough
that the cost is not the reason for any future narrowing.

### 3.301 A defer segment could still take a distributed lock — 🟢 **FIXED 2026-09-04** (WS-2, 2026-09-04)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 3.302 A defer segment could still fire three fire-and-forget calls — 🟢 **FIXED 2026-09-04** (WS-2, 2026-09-04)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 3.300 A defer segment could still reach the three string-returning calls — 🟢 **FIXED 2026-09-04** (WS-2, 2026-09-04)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 3.303 Five assertion-shaped skips in the plugin harness reported a broken Java/Python build as a pass — 🟢 **FIXED 2026-09-04** (WS-2, 2026-09-04)

`tests/plugin-harness/wasm_plugin_test.go` decoded the Java/TeaVM result with two
`t.Skipf`s. A module returning the wrong shape — the exact defect class #455 fixed for
`examples/saga-java-port` — was reported as SKIP, which CI reads as a pass. Three more
skips in the build helpers had the same shape: two `reading cleat build output` reads
that happen *after* a build the same function has already declared successful, and a
Python `produced no .wasm` whose Java twin (`buildJavaWorkflowWasm`) already used
`t.Fatalf`. All five are now `t.Fatalf`. Baseline 214 → 209 skip sites
(`scripts/check-skips.sh`).

**The falsification target originally proposed for this does not work, and the way it
fails is the interesting part.** The plan said to revert #455 and watch the new assertion
go red. #455 touched `crates/cleat-java/src/main/java/cleat/{HostCalls,JsonHelper}.java`,
`examples/saga-java-port/.../MoneyTransfer.java`, `engine/java_workflow_e2e_test.go` and
`tiers.yaml` — and its two SDK edits are **javadoc only**:

    git show 115b421 -- crates/cleat-java/ | grep -E '^[+-]' | grep -vE '^(\+\+\+|---)' \
      | sed 's/^[+-]//;s/^[[:space:]]*//' | grep -vE '^(\*|/\*\*|\*/|$)'    # prints nothing

None of it is an input to `TestPluginCalls_Wasm_Java`, which compiles
`tests/plugin-harness/testdata/javaworkflow/`. Reverting #455 therefore leaves this test
green — and a green falsification would have been read as "the new assertion is dead", the
precise misreading CLAUDE.md's "a falsification that stays green is telling you which case
you did not write" warns about. **A fix and a test can be about the same defect and still
share no code.** Check that the revert reaches the test's build inputs before believing
either outcome.

What was falsified instead — the same defect, applied to the code this test actually
compiles. Both perturbations were reverted; both went red on the intended line:

| perturbation to `PluginHarnessWorkflow.callAllPlugins` | fires | message |
|---|---|---|
| return `Map` via `JsonHelper.parseObject` (i.e. #455's own fix, applied here) | outer | `not the JSON-encoded string the ABI contract requires: json: cannot unmarshal object into Go value of type string` |
| return `"cleat-falsification-not-json"` | inner | `unwrapped to text that is not a JSON object: invalid character 'c'` |
| `os.ReadDir(tmpDir + "/no-such-dir")` | ReadDir | `reading cleat build output <path>: ... no such file or directory` |

Each fired on its own line, so the two decode assertions are independent rather than one
assertion reached two ways. Under the old code all three printed SKIP.

**Note what this workflow's return type says about #455.** It still returns a hand-built
JSON `String`, the idiom #455's javadoc now argues against — so the double unwrap here is
correct *for this workflow*, and the first row above is a shape change, not a bug fix.
`grep -rn 'public static String.*HostCalls' --include='*.java' .` returns 10 lines
(2026-09-04) — and they are not all workflows. Two are javadoc that #455 missed while
rewriting the same example in `HostCalls.java`: `CleatEntry.java:26` and
`TerminalError.java:14` still show `public static String placeOrder(...)` returning
hand-built JSON. Read the output rather than the count; five of the ten are string
literals inside `CleatEntryProcessorTest.java`.

Two things seen while doing this and **not** fixed here, both needing their own change:

- `TestPluginCalls_Wasm_Java`'s `expectedKeys` loop checks each key is *present*, not that
  its value is a success. The raw result printed by the first perturbation above contains
  `{"error":"plugin function pgvector/upsert not registered..."}` and
  `{"error":"blobstore: no tenant context"}` under expected keys, and the test passes.
  This is the same "only checked the field was PRESENT" trap #455's own commit message
  confesses to. Some of those errors may be legitimate for an in-memory env; deciding
  which is the work.
- `TestPluginCalls_Wasm_AS` rewrites the checked-in
  `tests/plugin-harness/testdata/asworkflow/dist/workflow.wasm` on every run, so any test
  run leaves `git status` dirty. `testdata/javaworkflow/prebuilt/README.md` documents
  having solved exactly this for Java by moving the fixture out of the build directory.
### 3.305 A checked-in test fixture was rewritten by its own test, and was stale under the rewrite — 🟢 **FIXED 2026-09-04** (WS-2, 2026-09-04)

`tests/plugin-harness/testdata/asworkflow/dist/workflow.wasm` is read by
`wasm/import_section_test.go` and `cmd/cleat-worker/backend_routing_test.go`, neither of
which can build it (a Go-only CI job has no `npx`). But `TestPluginCalls_Wasm_AS` compiles
the same workflow on every run, and `asc` writes `dist/workflow.wasm` — so a test run
overwrote the fixture and left `git status` dirty. Moved to `prebuilt/`, out of the
build's reach, exactly as `javaworkflow/prebuilt/` already was for the same reason;
`dist/` is now gitignored whole.

`.gitignore` had reasoned about this case and got one step wrong. Its rule named only
`dist/workflow.stamped.wasm`, on the grounds that "only dist/workflow.wasm is a fixture"
— a true statement about **which file has readers** used to answer a question about
**which files the build writes**.

**The interesting part is what the overwrite was hiding.** Measured 2026-09-04:

| | bytes | sha256 (16) |
|---|---|---|
| committed | 13369 | `36c46f1395c1092a` |
| after one `TestPluginCalls_Wasm_AS` | 13672 | `17cb617f1563a736` |
| after a second run | 13672 | `17cb617f1563a736` |

The AS build is reproducible — unlike TeaVM, where `javaworkflow/prebuilt/README.md`
records successive builds of unchanged source differing in hash. So the 303-byte gap was
**age, not nondeterminism**: the committed fixture predated its own source or toolchain
(`asc` inside the `^0.28.19` pin resolves to 0.28.20). Nothing had noticed, because every
AS test run silently refreshed it in place.

**That makes "move it" and "refresh it" one change rather than two.** Moving the stale
bytes to `prebuilt/` would have frozen the staleness permanently, with the mechanism that
had been concealing it now removed — strictly worse than leaving it. Both reader tests
pass against either version, so the refresh changed no assertion; falsified by hiding the
fixture, which fails exactly the `assemblyscript` subtest in each and nothing else.

`workflow.js` and `workflow.d.ts` are generated glue with no reader
(`grep -rn 'dist/workflow\.\(js\|d\.ts\)' --include='*.go' --include='*.md'
--exclude-dir=node_modules .` finds none), so they are untracked rather than moved — the
same call `.gitignore` already made for `examples/*/dist/`.

### 3.306 The Go adapter decoded the host's error message and threw it away — 🟢 **FIXED by #730 (§3.200) 2026-09-04** (WS-2 found, WS-1 diagnosed and fixed)

Found by dumping every key of `TestPluginCalls_Wasm_*`'s result. Same host, same 10
registered plugins, same 17 calls, **five** guest languages — and one of them said something
completely different:

| guest | failed `blobstore.put` | failed `pgvector.upsert` |
|---|---|---|
| Rust, AssemblyScript, Java | `blobstore: no tenant context` | `plugin function pgvector/upsert not registered…` |
| Python | `cleat call plugin:blobstore.put: [2] blobstore: no tenant context` | `cleat call plugin:pgvector.upsert: [2] plugin function pgvector/upsert not registered…` |
| **Go**, before #730 | `plugin_call: error 1 (0=unknown 1=timeout …)` | `plugin_call: error 1 (0=unknown 1=timeout …)` |
| **Go**, after #730 | `plugin_call: blobstore: no tenant context` | `plugin_call: plugin function pgvector/upsert not registered…` |

**The Python row was measured 2026-09-05 and it settles the question the Go row raised.**
Python carries the host's text *and* surfaces the classification, and it takes that number
from the right place: `python-sdk/cleat_sdk/host_calls.py:305` formats
`[{call_error_code}]`, which is the CallErrorCode field at bits 8-39 — the field Go's
adapter was printing the *legend* for while reading `errCode` from bits 0-7. So `[2]` is
`callErrorUnavailable` (`engine/callerrors.go`, `Retryable: true`), and `llm.chat_stream`
answers `[0]`. Both are correct: `[2]` is `callFailureCode`, and the streaming path's `[0]`
for a missing registry is deliberate — see the comment at `engine/plugins.go:431`, which
matches the non-streaming path's answer for the same condition because "a worker with no
registry is not a service that might succeed next time". **Python was never affected**, and
the five guests had four different answers where only Python printed a classification at all.

**"Could not be measured" was wrong, and worth reading twice.** This said
`componentize-py` is killed with signal 9 building this workflow — true as a symptom, and
useless as a conclusion. The cause is not memory pressure and not a sandbox limit:
`scripts/docker/python-toolchain.Dockerfile` has documented it since 2026-08-06 —
componentize-py's embedded wasmtime installs a mach exception handler into a guarded port,
so the process dies with `EXC_GUARD` / `GUARD_TYPE_MACH_PORT`, a Darwin kernel feature with
no Linux equivalent. It is deterministic, platform-specific, and **already solved in this
repo**. The whole measurement above takes six seconds:

    docker --context desktop-linux run --rm -v "$PWD":/src -w /src -e CGO_ENABLED=1 \
      cleat-py-toolchain go test ./tests/plugin-harness/ \
      -run TestPluginCalls_Wasm_Python -count=1 -v

`--context desktop-linux` is not optional on a Mac that also runs colima, and getting it
wrong does not look like a mount problem: colima cannot bind-mount these paths and says
nothing, so `-v "$PWD":/src` yields an *empty* directory and the run fails with `go: go.mod
file not found`, which reads as a broken checkout. Sanity-check the mount before believing
any failure, and check it is *this* tree rather than another checkout:

    docker --context desktop-linux run --rm -v "$PWD":/src -w /src cleat-py-toolchain ls /src

The general lesson is the one this section already carries in another form: **"environmental"
is not the same as "unavoidable."** Establishing that a failure was not caused by my change
is a control, not an answer, and stopping there left a row of this table blank for a day
while the fix sat in the tree. (WS-1 hit the identical stop on §3.205 the same week and
found the Dockerfile only when asked "don't you run componentize-py in docker?")

**The mechanism.** The host wrote the text and the adapter decoded its length; the adapter
then discarded both. `PluginCall`'s `ResultStmts` computed `responseLen` and never read
`responseBuf` on the error branch. Three faults on one line: the message discarded
(`engine/plugins.go:361` writes it, and the other guests read it); the number printed taken
from `result & 0xFF`, which is `errCode` — **hardcoded to a literal 1 on every failure** —
and printed against the **CallErrorCode** legend, whose field is bits 8-39
(`packDurableCallResult` is `responseLen<<40 | callErrorCode<<8 | errCode`,
`engine/memory.go:243`); and the real classification, `callFailureCode`, never read. So
"why is it 1 for a not-registered plugin" had a flat answer: **it was 1 for everything.**

**Fixed for the two adapters this finding pointed at**, `PluginCall` and
`PluginCallStreaming`, by #730 (recorded as §3.200). They now decode `callErrorCode` from
bits 8-39 and pass `responseBuf` to `callErrorMessage`, as the three §2.10 adapters have
since §2.10. Measured on develop after #730:

    grep -c '0=unknown 1=timeout' wasm/adapter_metadata.go   # 18, was 20
    grep -c 'callErrorMessage' wasm/adapter_metadata.go      # 5, was 3

**Both of those follow-ups are now closed by #734 (2026-09-05), and the counts above are
frozen at #730 — re-derive before quoting them.** On develop at `fa6dd10` the first command
returns **0**: the legend is gone from `wasm/adapter_metadata.go` entirely, and survives in
exactly two places in non-test Go, both correct — `wasm/generator.go:427`, the
`callErrorMessage` helper used by the five adapters whose result word really does carry a
CallErrorCode, and an explanatory comment at `engine/memory.go:310`.

    grep -rn '0=unknown' --include='*.go' . | grep -v _test.go | grep -c .   # 2, both intended
    grep -c 'hostErrMessage' wasm/adapter_metadata.go                        # 15

A zero from that first command deserves suspicion rather than belief: this file's struct
literals defeat the obvious regex, so a grep over it can return zero for a pattern that was
never going to match and "confirm" whatever was being claimed. What makes this zero real is
that #734 exists and says so, not the zero itself. Cross-check against
`wasm.AdapterFieldNames()`, which is exported for this.

**What the 18 were, and why they were a *different* defect** — deliberately
not taken in #730. `packDurableCallResult` is the only packer with a `CallErrorCode` field
and it reaches exactly the five above. The other 18 sit over `packSimpleResult`,
`packAwaitChildResult`, `packAwaitPromiseResult`, `packAwaitSignalsResult` and
`packAcquireLockResult`, which have no such field at all, so there is nothing to decode:
13 of them have an output buffer and want `hostErrMessage`; 5 (`ContinueAsNew`,
`ContinueAsNewWithVersion`, `AcquireLock`, `AcquireLockMs`, `ReleaseLock`) have no buffer
and want the legend **removed** rather than replaced. `hostErrMessage` already exists in
`wasm/generator.go` for exactly this, and its doc comment warns the legend "would describe
a rejected cron expression as a timeout" — describing what was then the live defect in 13
other calls.

#734 took both halves, and corrected two things this section had inferred rather than
checked. The useful split was not "does the legend apply" but "did the host write something
to read" — AwaitChild, SideEffect and AwaitPromise all write the reason into the buffer on
the replay path and the guest returned before reading it, which is this section's own defect
on a different packer. And branches that looked dead were not: `engine/imports.go` returns
`errBadParam = 0xFFFFFFFF_00000001` from 64 sites before a handler runs, and its low byte is
1, so every one of those failures printed "error 1" — read by the legend as a timeout rather
than a bad parameter.

**§2.10 is why this survived: comment general, test specific.**
`TestHostAdapterReportsCallErrorCodeNotErrCode` pins this exact property and its doc
comment states the general rule, but the assertion substring-matches one call name. That
is the inverse of the trap CLAUDE.md names — there, a test's *name* claimed a mechanism its
body did not check.

**How it was first mis-diagnosed, because the mistake is reusable.** WS-2 reported the
symptom with two candidate mechanisms — "the host wrote no response bytes" or "the length
failed its bounds check" — and **both were wrong**, because both assumed `PluginCall` went
through `callErrorMessage`. It did not; it had its own literal copy of the format string.
The assumption came from

    grep -rn '0=unknown 1=timeout' --include='*.go' . | grep -v wasm_plugin_test | head

whose **first** hit is `wasm/generator.go:427` inside `callErrorMessage`, and whose 10th
line is not its last. The string occurred **22 times in non-test Go across 3 files**, 20 of
them in `wasm/adapter_metadata.go` — including the `PluginCall` entry that `head` cut off.
The first hit was a plausible decoy: `callErrorMessage`'s `"%s: error %d"` with
`callName="plugin_call"` renders byte-identically to the literal that actually produced it.
**A `| head` on "who produces this string" answers "who produces it first in path order",
and the two coincide only by luck.** Re-derive the shape, not the first line:

    grep -rn '0=unknown 1=timeout' --include='*.go' . | awk -F: '{print $1}' | sort | uniq -c

Refusing to name a mechanism is what kept the wrong one out of the record. Had the section
asserted "the host wrote no response bytes", the search would have gone to `engine/` and
the adapter line would have stayed unread.

### 3.307 Five plugin tests checked that a key was present, not that the call worked — 🟢 **FIXED 2026-09-04** (WS-2, 2026-09-04)

`TestPluginCalls_Wasm_{Go,Rust,AS,Python,Java}` each verified their 17 expected keys with

    if _, ok := results[key]; !ok { t.Errorf("missing result key: %s", key) }

so `{"error":"plugin function pgvector/upsert not registered…"}` under an expected key
passed. Measured 2026-09-04: **16 of the 17 calls fail in every language**, and all five
tests were green. The one that works is `llm.list_models` — which is also the only key any
of them checked for success, in Go alone, behind two `if …; ok` guards that pass silently
when the shape is unexpected.

This is the same shape as the skips of §3.303 — a check that reports success without
checking — and #455's own commit message confesses to the identical trap: *"my own shape
assertion missed it because it only checked the field was PRESENT."* Known, written down,
and still shipped in five more places.

**The fix is not to demand success.** The failures are honest: the in-memory harness has
no tenant context, does not register pgvector, and wires no plugin stream registry. So
`assertPluginOutcomes` requires instead that every failure match a reason **written down
with why**, and that `llm.list_models` keeps working. `pluginCallsThatMustSucceed` is the
list meant to grow; `knownPluginFailures` is the one meant to shrink. A call that starts
succeeding is an error telling you to lock it in, which is how the second list gets
smaller rather than staler.

Falsified four ways, each firing its own branch and no other:

| perturbation | result |
|---|---|
| drop the `no tenant context` reason | **12** keys report `failed for a reason not in knownPluginFailures` in Go **and** 12 in Java — matching the 12 measured |
| drop `llm.list_models` from `pluginCallsThatMustSucceed` | `llm.list_models now succeeds… add it` |
| add `blobstore.put` to `pluginCallsThatMustSucceed` | `blobstore.put must succeed in this environment and did not` |
| revert #730's `wasm/adapter_metadata.go` change | **Go alone** reddens, with `"plugin_call: error 1 ("` — Java and Rust stay green in the same run |

That last one is a discriminating negative control rather than a mere trigger: this test
now *depends* on #730, because Go's two bespoke reasons were deleted once it carried the
host's text like every other guest. The perturbation proves a regression of #730 would be
caught here, and that the other guests are not covering for it.

**Those two Go entries were removed, not left harmless.** Before #730 `knownPluginFailures`
carried `plugin_call: error ` and `plugin_call_streaming: error ` to keep the divergence
visible; re-measured after #730, all 16 of Go's failures match the three host reasons and
neither entry can fire. A dead reason in a list whose whole purpose is to shrink is exactly
the rot the list exists to prevent, so the file now carries a comment saying why there is
no Go-specific entry rather than an entry nothing matches.

Also removed a duplicated `"llm.chat_stream"` from three of the five key lists (Go, Rust,
Python) — 18 entries, 17 distinct, so one key was checked twice and the count in the log
line never matched the list.

### 3.308 `cleat build --target python` wrote its output into the user's source directory — 🟢 **FIXED 2026-09-05** (WS-2, 2026-09-05)

`cmd/cleat/build_python.go` passed `--output <name>.wasm` — a bare **relative** name, with
`outDir` playing no part in it. `python-sdk/scripts/build_wasm.py` resolves a relative
`--output` against the **entry file's directory** (its "Resolve output path to absolute"
block, because componentize-py runs with that directory as CWD), so the component landed
beside the user's `.py` file along with the `<name>.wasm.component.wasm` backup copied next
to it. `-o` was honoured only as the destination of a later copy, and the code that made
that copy searched two places — `.` first, then the entry directory — which is the shape of
a symptom worked around rather than a path anyone believed in.

**Why it mattered here.** `tests/plugin-harness/testdata/pythonworkflow/` is *tracked*, so
`TestPluginCalls_Wasm_Python` overwrote two committed 19 MB fixtures every time it ran —
including on every run of `plugin-harness-ci.yml`, which installs componentize-py. It was
invisible in CI because a runner's checkout is discarded, and invisible locally because the
build could not run on a Mac at all (see the container recipe below).

**Do not fix this by committing the new bytes.** componentize-py's output is not
reproducible. Five consecutive builds of an unchanged source, `__pycache__` cleared between
the fourth and fifth to rule out a warming cache:

| run | size | sha256 (first 16) |
|---|---|---|
| 1 | 20482296 | — |
| 2 | 20443810 | `e2ba0fb2a785a1d1` |
| 3 | 20421353 | `01d2dd3bd94d4933` |
| 4 | 20448164 | `3e082ea08494310d` |
| 5 | 20398088 | `e05fa134b0f8c59a` |

Five distinct digests, sizes moving in both directions. A committed copy cannot be kept
current even in principle, which is what rules out the fix §3.305 used for the
AssemblyScript fixture — move it out of the build's reach and refresh it. Re-derive with

    docker --context desktop-linux run --rm -v "$PWD":/src -w /src -e CGO_ENABLED=1 \
      cleat-py-toolchain go test ./tests/plugin-harness/ -run TestPluginCalls_Wasm_Python -count=1
    shasum -a 256 tests/plugin-harness/testdata/pythonworkflow/call_all_plugins.wasm

**And those particular bytes were worth protecting rather than regenerating**, which is the
part that inverted the fix. `engine/imports.go:108` cites this fixture as the reason
`RegisterQueryHandler` survives as a no-op host import: removing the import would break
guests already compiled against it, and that file *is* the witness. Every SDK's public
wrapper around the call was removed 2026-08-09, so the committed artifact carries **9**
occurrences of `register_query_handler` and a fresh build carries **1**. The test was
quietly replacing the evidence for an ABI decision with a guest that no longer witnesses it.
Deleting the fixtures as unreferenced build output — no test loads them, only prose and that
comment refer to them — was the first plan, and it was wrong for exactly this reason.

    strings -a tests/plugin-harness/testdata/pythonworkflow/call_all_plugins.wasm \
      | grep -c register_query_handler        # committed: 9, freshly built: 1

**Fix.** Build into `os.MkdirTemp` via an absolute path, so the build script has nothing to
re-root and `-o` receives the only copy. The two-place search is gone with it.

**Not a sweep.** All four language targets build beside the source and copy to `-o`, but
Rust writes into `target/`, Java into `build/` and AssemblyScript into `dist/` — all
conventionally ignored. Python was the only one whose "beside the source" *is* the source
directory.

**Guard.** `plugin-harness-ci.yml` gains a `git diff --exit-code` over
`tests/plugin-harness/testdata` after the WASM integration tests, scoped to the whole
directory rather than to `pythonworkflow/` because the same defect in another language is
the thing most worth catching — §3.305 already found one in the AssemblyScript fixture.
Falsified both ways: with the fix reverted the guard exits 1 naming both files, with it
applied the guard exits 0. **The test itself reports `ok` in both runs** — it never read
those bytes, so nothing but the guard can see the difference.

---

### 3.404 The guest-execution harness's cost is set by fixture shape, not by call count — 🟢 **ANSWERED 2026-09-05** (WS-3, 2026-09-05)

**Question (WS-1's C1).** Does executing wave 1 — the host calls whose result a guest must
decode — fit the existing tier-2 jobs, or does it need its own? Answer wanted with measured job
times, before A1 finalised the harness, because a bad answer would force sampling into the design
as a retrofit.

**Answer.** It fits an existing job. No new job, no sampling, at wave 1 and at
wave 1 + wave 2 together — **provided the harness builds each guest once and invokes it N times.**
One fixture per host call costs 3× as much and makes the job co-critical-path with `Tier 1 Gate`.
That conditional is the whole finding: the cost driver is fixture shape, and the call count barely
matters.

#### The waves, re-derived

24 wave 1 / 13 wave 2 / 37 total. (23/14 when first derived; `AcquireLock` was promoted after —
see below.)

`wasm.AdapterFieldNames()` returns names only, and the split needs `ReturnType` and `ResultStmts`,
which are unexported. So the table was dumped from **inside** the package — a temporary
`wasm/*_test.go` marshalling `adapterDefs` to JSON, removed after — rather than pattern-matched.
That was not fastidiousness: the obvious regex over `wasm/adapter_metadata.go` returns **zero**
entries, because the struct literal defeats it, and zero would have silently confirmed whatever
was claimed.

The discriminator has no middle. Every wave-1 entry shifts a length out of the packed result,
reads an out buffer, or decodes a host message — most do all three. Every wave-2 entry does none
of the three.

#### The rule got better by being wrong about one row

The first rule was *"does the error path read a buffer"*, and under it `AcquireLock` was wave 2.
Recorded at the time as **the one placement I would defend least**, because it decodes
`acquired := (result>>8)&0x1` — a bit the host computed — even though no length or buffer is
involved.

WS-1 promoted it and rewrote the rule to **"does the guest have to decode something the host
computed"**, which is better generally. The case that separates the two rules is exactly this
one: a guest that returns a constant `true` for `acquired` compiles, passes every
compile-coverage check, and is silently wrong about holding a lock — the §3.200 class.

**Its harness row is honest but weak, and #744's table says so.** Exercising `AcquireLock` twice
in one invocation returns `first=true second=true`: the in-memory lock is re-entrant for the same
holder, so the row cannot distinguish a decoded bit from a hardcoded one. Doing that needs a
second holder, which one workflow invocation cannot provide. The same ceiling applies to any
row whose value depends on another party's state.

#### Measured job times

Run `33973787289` (`Cross-Language E2E`, sha `fa6dd10a`, all green). Re-derive with

    gh run list --workflow "<name>" --branch develop --status success --limit 5 \
      --json databaseId,createdAt,updatedAt,headSha
    gh api repos/:owner/:repo/actions/runs/<id>/jobs \
      --jq '.jobs[] | .steps[] | "\(.name)\t\((.completed_at|fromdateiso8601)-(.started_at|fromdateiso8601))s"'

| job | wall |
|---|---|
| Tier 1 Gate | **848s** — the critical path |
| CI/CD Pipeline | 612s |
| Cross-Language WASM E2E | **364s** — 20-minute timeout |
| Tier 2 Gate | 269s — installs no guest toolchain |
| Ecosystem CI | 82s — 4 jobs, 13–50s, SDK unit tests only, no host |

#### The number that decided it, and it is not a build time

Per-test durations in that run show each language's caching, and the caching is the underlying
build tool's rather than the harness's — there is no `sync.Once` in `buildRustWasm`,
`buildAssemblyScriptWasm` or `buildJavaWasm`, each shells out on every call. Rust looks free only
because every test builds the *same* crate.

Local measurements (this Mac, aarch64) separate build from execute, which the CI log cannot do —
Go buffers `-v` output per test, so every line inside a test carries the same timestamp:

| measurement | value |
|---|---|
| Rust, no-op rebuild | 0.03s |
| Rust, rebuild after a one-line source edit | 1.21s |
| AssemblyScript, `npm run build` (×2) | 1.05s, 0.86s — no caching |
| Python, `componentize-py` build in `cleat-py-toolchain` (×2) | 1.82s, 1.54s |
| **Python, execute a prebuilt 19.87 MB component, 5 consecutive in one process** | **995, 937, 934, 891, 935 ms** |

**Python's per-invocation cost is flat and does not amortise.** One engine, one wasmtime backend,
one already-built component, five executions — no downward trend. The build costs 1.8s once;
every invocation after costs another 0.93s and no cache in the path touches it. This is the term
that survives every shape change, and at wave 1 it is roughly half the added cost.

Reproduce with a scratch test in `./engine/` that reads a prebuilt component, constructs
`NewEngine(rt, caller, WithBackends(WasmtimeLanguages, wt))` once, and calls `engine.Execute(ctx,
wasmBytes, "run", input)` in a loop, logging each duration.

#### Costed, added to the 364s job

| | shape B: one fixture per call | shape C: build once, invoke N |
|---|---|---|
| Python | 205s | 75s |
| Java | 176s | 47s |
| AssemblyScript | 51s | 14s |
| Rust | 30s | 10s |
| **added** | **~460s → 825s** | **~145s → ~510s** |

Shape B is under the 1200s timeout but is 2.3× the job and lands within ~25s of `Tier 1 Gate`, so
`Cross-Language E2E` becomes co-critical-path and wave 2 has nowhere to go. Shape C is 8.5
minutes, stays 340s below the critical path, and leaves room for wave 2.

**Shape C was not a new capability.** `examples/as-workflow` already exports 8 entrypoints from
one module, `examples/rust-workflow` carries ~8 `#[cleat_entry]`, and the Java tree generates a
`CleatEntryIndex`. Python differs in mechanism only: `componentize-py` takes one
`--entry file.py:func`, and the component *"always exports `run` as the sole entry point, which
dispatches to `@cleat_entry` functions"* (`engine/python_wasm_e2e_test.go:121`), so Python reaches
shape C by dispatching on input inside one entry rather than by exporting 24 symbols. The cost
profile is identical either way, because Python's cost is per-invocation and not per-export.
A1 landed Go the same way — `buildGoHostCallWasm` called once outside the loop, dispatch inside a
single `exercise_host_call` entry, **24 invocations plus the build in 3.6s** — so the harness is
uniform across all five rather than Python being the exception.

#### Which job — corrected 2026-09-05, after it landed somewhere else

**This section named `Cross-Language WASM E2E` and the harness went to `Layer 2 — WASM
Integration` (#744, #751).** The criteria were right and the job was wrong, so the criteria are
kept and the name is corrected rather than the paragraph deleted.

What the criteria asked for, and Layer 2 satisfies every one: it already installs Go, Rust with
`wasm32-unknown-unknown`, Python + componentize-py, Java 17 and Gradle; it already runs
`check-skip-budget.sh` at a budget of **0** (`scripts/skip-ledger.tsv`, key `plugin-harness/wasm`);
its `Layer 2 — WASM Integration` context is already required; and `tiers.yaml` already declares it
`covers: tier2` under `tier2.gated_by`. It gained `Setup Node` in #751, because
`buildASHostCallWasm` skips rather than fails on a missing `npx` and the runner image shipping Node
is not a promise the job was making.

**The reason it had to be Layer 2 is stronger than the reason it could be, and this section did
not have it.** The harness needs `NewTestPluginEnvInMemory`, which lives in the
`tests/plugin-harness` module. `Cross-Language WASM E2E` runs `./engine/...` and
`tests/cross-language`; it does not run that module at all. So the harness could not have gone
where this section pointed without moving the environment it depends on.

`Tier 2 Gate` remains ruled out for the reason given: it installs no guest toolchain, so putting
the harness there costs 27s + 137s of setup to duplicate what another job already pays, more than
doubling a 269s job to buy nothing.

**What this cost.** Nothing, because A1 chose correctly without the section. But
"host it in X" read as settled for four hours while X was not where it went, and a later reader
reconciling the plan against CI would have found a job that runs none of it.

#### On sampling, which is the part that was designed out rather than designed in

§3.401's lesson was not that sampling is hard. It was that **a metric which silently loses samples
reports itself as better** — an unrecorded latency stayed zero, zeros sort to the front, and a run
that measured less looked faster. So the requirement handed to A1 was not "sample carefully" but
"assert the count of calls actually invoked, and fail short."

A1 implemented it as a guard that fails `invoked 23 of 24`, negative-controlled by making one
subtest skip. **Worth recording what it does not catch**, because WS-1 checked rather than
assumed: dropping a call from the list shrinks `invoked` and `len(wave1Calls)` together, so the
count guard is blind to that, and a separate table-drift guard covers it — negative-controlled by
dropping `AcquireLock`. Two guards, two different failures, neither covering the other.

#### A committed Python fixture is off the table for good

Two `componentize-py` builds of identical source produced **19,928,335** and **19,872,748**
bytes. WS-2 had measured five builds with five distinct digests on their machine; this reproduces
it on a different machine and a different architecture, which moves it from "an environment" to
"the tool". Any harness that wants to compare Python must compare **outcomes**, never bytes.

#### Two numbers here are soft, and are labelled soft

* **Java's build/execute split is not measured.** CI totals are 6.6–8.3s per test; what fraction
  is Gradle is unknown, so shape C's ~1s/invocation for Java is the weakest figure above. It does
  not change the verdict — Java could be 3× that and shape C still fits — but it must not be
  quoted as measured.
* **The Python CI split (5.9s build / 3.0s execute) is a model**, not a measurement: the Mac's
  1.8 : 0.93 ratio applied to the measured 8.9s CI total. It is the only modelled number in the
  section.

---

### 3.309 `AwaitAllChildren` does not await — it records "child not completed" as the child's permanent outcome — 🟢 **FIXED 2026-09-05** (WS-2, 2026-09-05)

Found while settling a question WS-1 raised for #744's B1: "AwaitAllChildren returns ok on a
run ID that AwaitChild SUSPENDS on — either a real inconsistency or a design difference
nobody has written down." It is the first, and the no-backend framing understates it. **The
divergence is on the ordinary path, with a store configured and children genuinely running.**

Three siblings, one of which does not do what its name says:

| host function | child not complete | `engine/children.go` |
|---|---|---|
| `AwaitChild` | **suspends** | 299 |
| `AwaitAnyChild` | **suspends** | 435 |
| `AwaitAllChildren` | returns `{"error":"child not completed"}` with `errCode 0` | 504 |

    grep -rn "packAwaitChildResultSuspend" --include='*.go' . | grep -v _test.go
    # exactly two callers, and AwaitAllChildren is not one of them

**Why "returns an error string" is not the defect.** `freshAwaitAllChildren` marshals those
outcomes into the `EventRecord` it records at `children.go:521`, and `replayAwaitAllChildren` hands
`rec.Response` back to the guest verbatim on every future replay. So "this child had not
finished when I looked" is written into the workflow's permanent history **as the child's
result**. The child then completes, and no replay will ever say so.

The irony is local: the 30-line comment in that same function, three lines above the
`else` that produces this, exists to explain why `context.Background()` is used rather than
`ctx` — because cancelling those queries would write `"context canceled"` into permanent
history and "a transient shutdown would become a durable wrong answer, which is a strictly
worse failure than the one cancellation avoids." That is precisely what the `else` branch
below it does, unconditionally, and not on shutdown.

**Every specification of this call says it waits.** The implementation is alone:

- `ABI.md` §2.53 — "Companion to `cleat_await_all_children` (§2.23), **which waits for all
  of them**."
- `ABI.md` §2.22, for the sibling — "If the child is not complete, the workflow should
  suspend." §2.23 says nothing about the incomplete case at all.
- `cleat/runtime.go:265` — "AwaitAllChildren **waits for all child workflows** identified by
  runIDs **to complete**. ... Unlike calling AwaitChild in a loop, all children are awaited
  concurrently." The stated difference is concurrency, not whether it waits.
- `python-sdk/README.md:86` and `crates/cleat-sdk/README.md:132` — "await multiple children
  concurrently".

**What pins the current behaviour is a test that asserts it without justifying it.**
`TestAwaitAllChildren_SomeRunning` (`engine/children_test.go:796`) sets `run-b` still
running, asserts `errCode 0`, and asserts the outcome for `run-b` is exactly
`"child not completed"`. The name states the scenario, not a mechanism, and no comment
anywhere says why this sibling alone must not suspend. It reads as a test written to match
the code.

**The fix is two halves, and the second is why this is filed rather than fixed.**

1. `freshAwaitAllChildren`: when any child is incomplete, record the event without a
   response and suspend, as `AwaitChild` does at `children.go:287-299`.
2. `replayAwaitAllChildren` **cannot currently replay such a record.** It has no equivalent
   of `AwaitChild`'s "no cached result yet — fall through to fresh" path
   (`children.go:238-246`); it serves `rec.Response` unconditionally, so a suspended record
   would replay as an empty result. Half 1 without half 2 converts a durable wrong answer
   into a durable empty one.

**Fixed 2026-09-05, both halves, on the owner's decision.**

Half 1, `freshAwaitAllChildren`: any child still running records the event **without a
response** and suspends, with `pending` tracked in a slice beside `outcomes` rather than as a
field on `childOutcome` — that struct is marshalled into the result the guest reads, so a new
field would change the wire format for every caller. The no-store branch suspends too, which
is the specific divergence that started this section: `AwaitChild` reaches its suspend on
that same condition.

Half 2, `replayAwaitAllChildren`: a record with an empty response falls through to fresh and
re-checks, mirroring `AwaitChild`'s "no cached result, exitReplay to fresh". Checked **before**
`advanceReplayStep` and without advancing `stepCount`, so the fresh execution overwrites the
empty event at the same step; everything past that point keeps its existing order, and the
four pre-existing replay tests (`Match`, `MismatchType`, `IDsMismatch`, `PastEnd`) still pass.
An empty response cannot arise any other way — the completed path records `json.Marshal` of a
slice, `"[]"` at its shortest — so no pre-existing history is reinterpreted.

**Falsified one half at a time, which is what demonstrates "both halves or neither":**

| perturbation | result |
|---|---|
| half 1 reverted (fresh does not suspend) | `TestAwaitAllChildren_SomeRunning` and `TestFreshAwaitAllChildren_NoStore` fail: `a still-running child must suspend: got 0x6300000000, want 0x4000000000000000` |
| half 2 reverted (replay does not fall through) | **only** the replay test fails: `replay returned an EMPTY result -- the suspend record was served verbatim` |

The second row is the argument for shipping them together: half 1 alone trades a durable
wrong answer for a durable **empty** one, which is worse because empty reads as success.

**Two tests asserted the old behaviour and were converted, not deleted.**
`TestAwaitAllChildren_SomeRunning` asserted `errCode 0` with `"child not completed"` for the
running child; `TestFreshAwaitAllChildren_NoStore` asserted `errCode 0` with
`"no child workflow store"`. Both now assert the suspend, and each carries a comment saying
what it asserted until 2026-09-05 and why that held the defect in place. A third test,
`TestAwaitAllChildren_ReplayOfSuspendRecordFallsThroughToFresh`, is new and covers half 2.

**A third test encoded the old behaviour, in another stream's work.** WS-1's host-call
harness (#744/#749) carried `AwaitAllChildren` under "calls that succeed with no backend"
with `status: statusOK` and the raw result `[{"run_id":"…","error":"child not completed"}]`,
and its `why` said the fix "is not this PR's". It is now, so the row moves to the suspend
section beside `AwaitChild` and `AwaitAnyChild` — the two calls it was flagged for
disagreeing with. `TestHostCallTableCoversEveryWave1Call` still passes, so the move did not
drop it from the table. That row is how this section began: WS-1 flagged the disagreement,
WS-2 traced it off the no-backend path onto the ordinary one.

Measured: `go test ./engine/` 0 failures; child/replay/suspend tests against a real
PostgreSQL (`-p 1`, WS-2's 5433) 400 pass, 0 fail, with the DSN confirmed to connect by
`TestPluginMigrations_AllDialects` rather than assumed; `TestHostCallsGo` and
`TestHostCallTableCoversEveryWave1Call` pass.

### 3.406 Three SDKs, three different amounts of the truth about the same host call — 🟢 **MEASURED 2026-09-05** (WS-3, 2026-09-05)

C2. Rust and AssemblyScript fixtures through the host-call execution harness (§3.210), 24 wave-1
calls each, one call per invocation, tables recorded from measurement rather than predicted.

The one-line result: **`ListCrons` returns the host's message in Go, cannot be called at all from
Rust, and arrives as `null` with the message already gone in AssemblyScript.** Every one of those
three guests compiles. Compile coverage cannot see any of it, which was the argument for this
round.

#### 1. The Rust SDK cannot make two of the 24 calls

    grep -c cron crates/cleat-sdk/src/host_calls.rs                      # 0
    grep -c cleat_schedule_cron packages/cleat-as/assembly/host-calls.ts # non-zero
    grep -oE 'Export\("[a-z_0-9]+"\)' engine/imports.go | sort -u | grep cron   # 3

No `cleat_schedule_cron` import, no `cleat_list_crons`, and the string "cron" appears zero times
in the file. The host exports both and the AssemblyScript SDK binds both, so this is a guest-side
gap and not a host limitation.

Recorded through a **new `statusUnsupported`** on the harness rather than as an error. The two are
different facts: an error is a binding that ran and was refused, and this is a binding that does
not exist. Collapsing them files a missing SDK feature under "the host said no", which is the
distinction the whole harness exists to draw. It also reddens usefully — when Rust gains cron the
row stops reporting `unsupported`, stops matching, and someone has to decide what the right answer
is, rather than the gap closing silently.

Falsified: making the fixture pretend the binding exists gives
`rust/ListCrons: status ok, table says unsupported`.

#### 2. The AssemblyScript SDK discards the host's message and prints its own legend

`ScheduleCron` reports

    failed: timeout (code 1)

It is not a timeout. Patching `scheduleCron` to read the output buffer and re-recording shows what
the host actually said:

    no workflow store configured: workflow <uuid> cannot schedule "harness-workflow"

which names the workflow *and* the schedule, and is strictly more than Go's own row asserts. Go
gets that message; AssemblyScript throws it away and substitutes a wrong word.

`packages/cleat-as/assembly/host-calls.ts` does this at **25 call sites**
(`grep -c errorCodeName`), and `errorCodeName` is a six-entry legend —
unknown / timeout / transient / not_found / invalid_request / permission_denied. That is
`cleat_call`'s `callErrorCode` legend, applied to `decodeSimpleResult`'s `errCode` from a
different result layout. **This is §3.200 in another language, and broader**: the Go defect was
one adapter class, and `wasm/adapter_hostmessage_test.go`'s
`TestNoAdapterPrintsTheCallErrorCodeLegendForAnotherLayout` exists precisely to stop the Go side
doing it. Nothing checks this side.

**Not fixed here** — one PR, one thing. The table row asserts the *wrong* text deliberately, so
the day the SDK is fixed the row reddens and has to be rewritten to assert the host's message.
That is how a known defect stays visible instead of being forgotten. Falsified by exactly that
route: patching the SDK to carry the message reddens the row with `detail changed`.

#### 3. Four AssemblyScript bindings lose the message by signature, not by choice

`awaitAllChildren`, `awaitAnyChild`, `listCrons` and `sideEffect` all return `string | null`, so a
failure arrives with the host's words already gone before the guest can see them. There is no bug
to fix in a fixture here; the API has nowhere to put the text.

Only `listCrons` is observed failing in this environment, so it is the only one the table can pin.
Its recorded detail is written by the *fixture* and says so:
`<no host message: listCrons returns string|null and discards it>`. Recording that as an ordinary
error would have put invented text where the Go table puts the host's own words, and made the two
tables look comparable when they are not.

#### What the AssemblyScript table shows that the Go and Rust tables hide

Go and Rust both report `1 child result(s)` for `AwaitAllChildren`. The AssemblyScript row reports
the host's raw JSON:

    [{"run_id":"00000000-0000-0000-0000-000000000001","error":"child not completed"}]

**Both counts are green over a child result that is an error.** A count is a lossy rendering, and
the two rows that use one cannot fail on the contents of what they counted.

That `"error":"child not completed"` is **§3.309**, filed by WS-2 the same day and still open:
`AwaitAllChildren` does not await, it records the not-yet-completed state as the child's permanent
outcome. Two streams reached it independently and from opposite directions — WS-2 by reading the
branch, this table by printing what the host returned instead of counting it. Worth noting which
one would have caught it alone: WS-2's would, this one only because the row stopped summarising.

The route to that row is worth recording too, because it is the same lesson from the other side.
The fixture's first version counted commas to match Go's wording and returned **2** for that
one-element array — the element is an object with a comma inside it. AssemblyScript has no JSON
parser in scope there (the SDK's `jsonParse` is itself a host call, which would put a second call
inside every measurement of this one), so there is no honest way to produce the count. It reports
what it received instead. **A fixture that computes a wrong number and reports it confidently is
the exact failure this harness exists to catch**, and the near-miss was in the harness's own
fixture.

#### What the three tables agree on, which is also evidence

All four suspending calls produce byte-identical suspend reasons in all three languages, rendered
arguments included — `await_signals(["harness-signal"], 10ms)` among them. Three SDKs encode the
same values the same way on the wire: Go passes an `int64` of milliseconds, Rust a `Duration`,
AssemblyScript a hand-built JSON string and an explicit `…Ms` variant. Reaching that agreement in
AssemblyScript required `awaitSignalsMs`, `awaitPromiseMs` and `acquireLockMs`; the plain forms
take **seconds** and would have rounded the harness's 10ms to 0 and asked a different question.

Three caveats reproduce in all three languages and are recorded in every table rather than in one:
`RunID` and `WorkflowID` return the **same value** in this environment, so no row can catch a guest
that returns one for the other; the in-memory lock is **re-entrant for the same holder**, so
`AcquireLock`'s `first=true second=true` still cannot distinguish a decoded bit from a hardcoded
one; and `ChildWorkflowWithOptions` is indistinguishable from `ChildWorkflow` because the in-memory
store ignores `Version`.

#### Cost, against §3.404's model

All three languages, 72 host-call executions, **5.3s** locally. Rust builds in 2.7s and executes
in 0.17s; AssemblyScript is 3.65s end to end. Build-once-invoke-N held.

#### What it did to the executed-coverage guard, and two calls that guard cannot see

`scripts/sdk-host-call-coverage.py --check-executed` (§3.207) moved **rust 7 → 23** and
**assemblyscript 7 → 25**, and *failed* on the rise until the baseline was recorded — the
bidirectional ratchet landed in #749 hours before this, on the reasoning that a guard which only
forbids shrinking cannot tell a stale baseline from an accurate one.

**Two wave-1 calls this fixture exercises are invisible to both coverage metrics, and the reason
is one character of regex.** `rust_surface()` matches `^\s{4}pub fn ([a-z_][a-z0-9_]*)\s*\(` —
the name must be followed by `(`. A Rust generic method is `pub fn name<T: …>(`, so every generic
method on `HostCalls` is excluded:

    counted (surface) = 61
    all `pub fn`      = 71
    excluded (10): await_child_typed, child_workflow_typed, cleat_call_heartbeat_typed,
                   cleat_call_typed, cleat_call_with_host_retry, cleat_call_with_retry,
                   defer_func, plugin_call_streaming_typed, plugin_call_typed, side_effect_typed

Two of those ten are the **only** Rust bindings for a wave-1 call: `cleat_call_with_retry` is the
whole of `DurableCallWithRetry` (there is no string-in/string-out form) and `defer_func` is the
whole of `DurableDeferFunc`. So this fixture calls them, the table asserts their outcomes, and
both coverage numbers are computed as though neither existed. **The Rust surface of 61 understates
by 10, and the direction is the dangerous one** — a smaller denominator makes coverage look
*higher*, and an uncountable call reads as a call nobody needs to write.

**Fixed in #753 (`26e6333e`), and the corrected numbers are worse than the section predicted.**
Rust's surface is **71**, and compile coverage is **63/71 = 88.7%** — it was never 100%, and that
100% had been quoted into four places and to the user before anyone read the file a second way.
Executed coverage moved 23 → 25. Java's surface went 68 → **70**: the two members were
`awaitSignalsWithQuorum` and `awaitSignalsWithQuorumMs`, whose return type
`CleatResult<java.util.List<AwaitSignalsResult>>` fell outside a character class missing `.`. The
77-versus-68 gap flagged above was therefore **2, not 9** — the other seven `public` lines are
fields and constructors. Flagging it rather than asserting it was right; asserting it would have
been wrong by seven.

**One caveat this harness's Rust numbers still carry, and it is the same defect on the other
side.** #753 fixed the *declaration* side. The *call* side still ends `\s*\(`, and
`DurableCallWithRetry` calls `h.cleat_call_with_retry::<Value, Value>(..)` — a turbofish sits
exactly where the pattern expects a paren. Measured over this fixture's globs alone: it credits
**21** of the 24 arms, and `cleat_call_with_retry` is **not** among them. Two of the other three
are the `unsupported` cron arms, which make no call at all.

The total is nevertheless right today, and only by accident: `examples/rust-workflow` calls the
same method in a matchable form, so the one turbofish site is credited by a neighbour. **Delete or
rewrite that example and Rust's executed count drops by one, reading as a fixture regression
rather than a scanner one.** Recorded next to `SDKS` with its known-positive — over this fixture's
globs alone the strict pattern finds 0 for `cleat_call_with_retry` and a loosened one finds 1 —
because "zero missed" is otherwise indistinguishable from a loose pattern that matches nothing.

### 3.311 `TestBlobstore_S3` now exists, so Layer 4 runs a test for the first time — 🟢 **FIXED 2026-09-05** (WS-2, 2026-09-05)

§3.310 found that `plugin-harness-ci.yml`'s Layer 4 ran `-run 'TestBlobstore_S3'` against a
test that existed nowhere, so it printed `ok … [no tests to run]` and exited 0 while
provisioning a MinIO service container and five `CLEAT_TEST_S3_*` variables that no Go code
read. It was filed rather than resolved because the honest options were to write the test or
delete the job, and which one is right is a product question. **The owner chose to write it.**

    cd tests/plugin-harness && go test ./... -list 'TestBlobstore_S3' | grep -c '^Test'
    # 1, was 0

**What it covers that the existing S3 tests cannot.**
`plugins/blobstore/blobstore_s3_backend_test.go` constructs the unexported `s3Backend` with a
mock `RoundTripper`, so it asserts the plugin's own call sequence and can never observe how a
real server answers it; it also lives in the **root** module, which Layer 4's
`working-directory` cannot reach. This test drives the plugin's *registered host functions* —
the surface a workflow reaches through `cleat_plugin_call` — with a genuine endpoint and a
real SQL index underneath.

**The load-bearing assertion is the one that reads the bucket directly.** `Config.Backend`
defaults to `"memory"`, so a plugin that ignored or failed to parse the S3 config would still
`put` and `get` successfully, out of a map, with nothing in object storage. Falsified by
setting `"backend": "memory"` and re-running against real MinIO:

| perturbation | result |
|---|---|
| `backend: "s3"` (as shipped) | PASS |
| `backend: "memory"` | **put and get both still succeed**; fails only at `the blob is not in the bucket at cleat-test-harness/2849…: The specified key does not exist` |

So a round-trip test would have been green with nothing reaching S3, which is what makes the
`StatObject` check the test rather than an extra.

**Two skips, both `check-skips.sh` case (a).** `CLEAT_TEST_S3_ENDPOINT` unset, and
`CLEAT_TEST_POSTGRES` unset — blobstore stores its index in SQL, so the S3 backend cannot be
exercised through the plugin without a database. Layer 4 provides both. Baseline updated:
`tests/plugin-harness  TestBlobstore_S3  2`.

**Two things it does not do, deliberately.** It does not call `RunCoreMigrations`: the
engine's workflow tables are irrelevant here, and `033_completed_workflow_retention_indexes.sql`
needs the `pg_trgm` extension, which would fail this test on a database lacking it for a
reason having nothing to do with S3. `RunPluginMigrations` creates its own tracking table, so
it stands alone. And the payload is random rather than fixed, so a repeat run against a
shared bucket cannot pass by reading an object an earlier run left behind — the assertions
are round-trip equality, so the outcome does not vary.

Measured against MinIO on `localhost:9000` and WS-2's PostgreSQL 5433: PASS in 0.07s, and
SKIP with an accurate message when the endpoint is unset.
### 3.312 A Java host-call fixture, and the arity defect it found on its first run — 🟢 **FIXED 2026-09-05** (WS-2, 2026-09-05)

Java was the weakest "bound but never executed" case in the tree: 8 host calls executed
against a 70-method surface, every one of the 70 compile-checked, which is exactly why it read
as covered. This adds the Java fixture to the host-call harness (§3.211's shape: one fixture,
built once, invoked once per call, dispatching on `{"call":"<Name>"}`), bringing it to four
languages over the 24 wave-1 calls.

**It found a defect on its first run, and the defect is why compile-time coverage could not
have found it.** `crates/cleat-java`'s `cleat_child_workflow_with_options` import declared
**nine** parameters against the host's **ten** — no `priority`, the second `i64`
(`engine/imports.go`, `(ptr,len x3, i64, i64, ptr,len, ptr,maxLen)`). The Java side compiled
perfectly against a signature the host does not have.

A WASM import whose arity disagrees does not fail at that call. **The module fails to
instantiate**, so every one of the 24 calls died together:

    incompatible import type for `env::cleat_child_workflow_with_options`
    expected (i32 i32 i32 i32 i64 i32 i32 i32 i32) -> i64      [the guest's import]
    found    (i32 i32 i32 i32 i64 i64 i32 i32 i32 i32) -> i64  [the host]

So **any Java workflow that so much as referenced `childWorkflowWithOptions` could not run at
all** — and nothing noticed, because TeaVM tree-shakes unreferenced imports and no Java test
called it. Same defect class as §3.55, where `cleat_create_promise` was registered on wasmtime
with a parameter no guest passed and durable promises could not link on the worker.

Fixed by adding `priority` to the raw import and passing `0L`, which the Go SDK documents as
the default and the highest (`cleat/runtime_children.go`, "0 = highest priority"). Exposing
priority in Java's *public* API is additive and deliberately not bundled here — the arity is
what stops the module linking.

**Falsified both ways.** Reverting the SDK fix reddens every row with the instantiation error
above; perturbing a single table row (`PollSignal` to `statusOK, "present=false"`) reddens
that row alone, with the row's `why` printed beside it.

**One cross-language divergence, recorded and NOT endorsed.** `PollSignal` is `ok` with
`present=false` on Go and an **error** on Java — `CleatResult<String>` has no `present`
channel, so "no signal pending" has nowhere to go but the error case, and a Java workflow
cannot distinguish "not yet" from "broken". The row asserts the text so that a Java binding
growing a `present` flag must change it.

**Two rows are `statusUnsupported`.** `grep -rn cron crates/cleat-java/src/main/java/cleat/`
returns nothing, while the host exports both cron functions and the AssemblyScript SDK binds
them — a guest-side gap, not a host limitation, and the same position Rust is in.

**Two things the fixture had to teach the shared harness.** Java exports the `@CleatEntry`
name **verbatim** where Go, Rust and AssemblyScript snake_case theirs, so the fixture spells
its entry `exercise_host_call` to match the one name `executeOneCall` asks for; getting that
wrong builds cleanly and traps with `export "exercise_host_call" not found`. And Java returns
the entry's value as a **JSON-encoded string** — a JSON string containing the object — where
the other three return the object, so `executeOneCall` now unwraps exactly once, and only when
the payload really is a JSON string, so a malformed object still fails rather than being
massaged into shape. That is the Java SDK's ABI contract, the same one
`TestPluginCalls_Wasm_Java` unwraps for and §3.303 turned from a skip into a failure.

**Table conventions, per §3.211's lesson.** Every `why` says what the row would **catch**, not
what it asserts — a `why` that restates the assertion is how a row goes green through the
defect it points at. No row asserts a count. Three rows carry an explicit LIMITATION:
`AcquireLock` cannot tell a decoded bit from a hardcoded `true` while the in-memory lock is
re-entrant for one holder; `RunID` and `WorkflowID` are the same value in this environment;
and Java's `sideEffect` takes an already-computed value rather than a closure, so its row does
not prove replay suppression the way Go's can.

`tests/plugin-harness/testdata/hostcallsjava/build/` is gitignored with the directory rather
than after the first dirty `git status`, which is how §3.305 and §3.308 both started.

**The first version of the harness change was wrong in review, and the way it was wrong is
worth more than the fix.** Handling Java's wrapper by unwrapping *whenever the payload happened
to be a JSON string* is one line shorter and made the harness blind in the other three
languages: a Go SDK that regressed to returning a JSON-encoded string decoded cleanly and
reported `ok`, where the day before it had failed as `undecodable result`. Measured on all
three inputs — object, Java-wrapped, and Go-wrapped-by-mistake — the third was
indistinguishable from correct. The comment sitting directly above that code said *"a tolerant
decoder is how a harness stops being able to tell a wrong answer from a differently-shaped
right one"*, so the principle was stated and then not applied to the line beneath it.

The shape is now a parameter the caller states, enforced in **both** directions: a wrapped
payload from an object language fails, and an unwrapped payload from Java fails too. The second
half is not symmetry for its own sake — without it, the Java SDK silently dropping its wrapper
would read as correct, which is the same defect one language over. Falsified both ways:

| perturbation | result |
|---|---|
| Go call site claims `resultJSONWrapped` | `this SDK returns the outcome as a JSON-encoded string and this result is not one` |
| Java call site claims `resultObject` | `this SDK returns the outcome object directly and this result is a JSON-ENCODED STRING` |

**This is the same shape as §3.213's parity guard**, found by WS-1 in review: that one widened
what it accepted by dropping a prefix filter, this one widened what it accepted to accommodate
one language's real contract. Both are correct locally and lose a distinction globally, and
neither is visible from inside the change — the tests stay green either way.

---

### 3.313 The README advertised a WASM backend that was deleted four weeks earlier — 🟢 **FIXED 2026-09-06** (WS-2, 2026-09-06)

`README.md`'s "WASM workflows" bullet read *"wasmtime is the backend of record … wazero is a
pure-Go, CGO-less fallback with no compute-bound fencing."* There is no second backend.
`engine/backend_wazero.go` was deleted in #459 (2026-08-10); a `CGO_ENABLED=0` build gets the
`//go:build !cgo` stub, constructs no backend at all, and `cleat-worker` exits 1 at startup.

    ls engine/backend_wazero.go                                    # No such file or directory
    grep -rn "there is no fallback" cmd/cleat-worker/main.go       # the exit

**The direction of the error is the point.** It does not merely misdescribe an internal — it
advertises, in the file a prospective user reads first, a *pure-Go deployment path that does not
exist*, and it downgrades the CGO requirement from "the worker will not start" to "you lose some
fencing". That is the one claim on which someone evaluating cleat against a competitor would
form a portability judgement. It survived 28 days.

**The correction it replaced was itself a correction, and went stale in one day.**
`docs/explanation/security-model.md` carried a dated callout — *"Corrected 2026-08-09 … wazero is
the CGO-less fallback only … Treat a wazero-only deployment as running without CPU/wall-clock
enforcement"* — which was accurate when written and falsified the next day by #459. Its advice
was the harmful half rather than its description: it told an operator to plan for a
degraded-but-running mode that cannot exist. **A dated correction is not durable; it is a
measurement, and it decays at whatever rate the thing it measured changes.** This one had a
one-day half-life, and the date on it is what made it look safe.

Both files now say wasmtime is the only backend, record what they said before, and separate the
two questions that were being run together: *which backend runs a worker* (wasmtime, always) and
*what still executes guest code on wazero* (`engine.Runtime`, on CLI and test paths only —
`cleat run_embedded`, `cleatctl replay|debug`, `cleat-bench`, `cleat/wasmtest`, and `RunDefer`
when no backend is registered, which is those same tools). Re-derive with

    grep -rn "NewRuntime(" --include="*.go" . | grep -v _test.go

**Writing that list, `cleat dev` went into it on the strength of its name and had to come out.**
`cleat dev` does not use WASM at all — `buildDevRun` generates a Go runner and `go run`s it as a
native subprocess. It is absent from the `NewRuntime` grep above, which is the check that caught
it, and it is the tool whose name most suggests otherwise. Same lesson as the `-run` probe in
CLAUDE.md: **the command answers, the name only implies.**

Two further stale facts were corrected in `security-model.md` because they sit in lines being
rewritten: wasmtime has **two** execution paths, not three — decomposition was deleted in #528
(2026-09-01), and `engine/component_no_decomposition_test.go` guards it
(`grep -rn "func.*ExecuteComponent" --include="*.go" .` → one line, `ExecuteComponentCGo`).

**Still open, deliberately not in this PR** (one PR, one thing — but it is the same claim and
should not be lost):

  * `docs/explanation/architecture.md` carries the *identical* stale callout, plus two mermaid
    sequence diagrams that label the **worker's** runtime `wazero WASM`, and a node
    `WR[WASM Runtime wazero]` on the main architecture diagram. That is the primary picture a
    reader forms of what a worker runs, and it names the wrong runtime.
  * Its host-function count says 59. **It is 52** — measured 2026-09-06, on the develop this
    branch is rebased onto:

        python3 -c "import re;print(len(set(re.findall(r'\.Export\("([^"]+)"\)',
          open('engine/imports.go').read()))))"      # 52 = 49 cleat_ + 3 unprefixed

    **This paragraph said 58 in the first push of this PR, and 58 was right when measured.**
    #767 landed on develop between that measurement and the rebase, removing the six-call
    durable-state family (`cleat_{set,get,delete,incr,has,list}_state`): 58 − 6 = 52. The
    number was not wrong through carelessness — it was wrong because a number is a
    measurement with a timestamp, and this one aged out inside a single PR. CLAUDE.md's
    stale 58/55 pair, and §3.213's, are the same casualty.
  * `engine/backend_wasmtime_stub.go`'s doc comment still instructs *"callers that fall back to
    wazero on error MUST check for `ErrWasmtimeCGOUnavailable`"*. There are no such callers.
    A comment telling a future reader how to write code that must not exist.

Other tracked files still match a wazero-as-fallback pattern. Measured 2026-09-06, after this
PR's edits, with `/usr/bin/grep` under `bash -c` rather than the interactive shell's ugrep
(CLAUDE.md, *"`grep` in an interactive shell here is not the `grep` your script gets"*) — **9**
files:

    git ls-files | xargs /usr/bin/grep -lniE \
      'wazero (is|as) (a |the )?(pure-go, )?(cgo-less )?fallback|fall(s|ing)? back to wazero'

and **18** if `|wazero backend` is added to the alternation, which pulls in dated historical
records (`IMPROVEMENT-PLAN-CLOSED.md`, `REVIEW-2026-08-09.md`, `BRANCH-TRIAGE.md`) and retraction
prose in CLAUDE.md, all correct as records. **The two counts are given together on purpose:** the
first draft of this paragraph published the broad number beside the narrow command — 16 against a
pattern that returns 9 — because the count was taken from one pattern and the command written
from another. That is the failure this document keeps recording, committed while writing the
section about it.

**Note that this grep now matches the retractions too, including the two written here.** That is
the trap CLAUDE.md names under *Build* — a text search cannot tell a thing from a sentence
denying the thing — and it is unavoidable in prose, where a retraction has to quote what it
retracts. The usable discriminator is position, not wording: in both files corrected here the
stale text survives only inside a `>` blockquote or an HTML comment, never in a claim line.

---

### 3.314 Every doc that said a worker runs on wazero — a sweep, and three things it found — 🟢 **FIXED 2026-09-06** (WS-2, 2026-09-06)

§3.313 fixed the README's wazero-as-fallback claim and the one doc it delegated to. This is the
rest of the tree. **24 files changed** (`git diff --name-only develop.. | grep -v IMPROVEMENT-PLAN`);
the classification mattered more than the count, because most wazero mentions are correct and
rewriting them would have destroyed records.

| class | treatment | examples |
|---|---|---|
| a worker's runtime named as wazero | rewritten to wasmtime, with a dated note | `SECURITY.md`, `docs/explanation/architecture.md`, `docs/worker-architecture.md` |
| wazero named as *a backend* | rewritten; there is one backend | `ARCHITECTURE.md`, `ABI.md`, `tiers.yaml` |
| original design documents | banner added, **body untouched** | `docs/contributor/design/*`, `docs/explanation/go-wasm-plan.md` |
| dated historical records | left exactly as written | `CHANGELOG.md`, `REVIEW-2026-08-09.md`, `IMPROVEMENT-PLAN-CLOSED.md` |
| wazero correct | left | plugin loader docs, `cleat/wasmtest` |

**Rewriting a design document to match what was built destroys the only record of what was
intended.** That is why the third row gets a banner instead of an edit — the distinction between
"this is wrong" and "this was a plan" is not visible from the text alone, and a sweep that cannot
make it will quietly launder history into documentation.

#### Three diagrams said the worker ran wazero

`docs/explanation/architecture.md` had `WR[WASM Runtime wazero]` on the main architecture diagram
and `participant WZ as wazero WASM` in both the first-run and replay sequence diagrams. **A
diagram is read faster and doubted less than a paragraph**, and these three were the primary
picture a reader forms of what a worker executes. Grep for stale claims will not find a mermaid
node label unless you go looking for one.

#### The host-function count is 52, and it was 58 four days ago

Every doc carrying it was wrong, by three different amounts (59, 58, 15, 14). Measured
2026-09-06:

    python3 -c "import re;print(len(set(re.findall(r'\.Export\(\"([^\"]+)\"\)',
      open('engine/imports.go').read()))))"      # 52 = 49 cleat_ + 3 unprefixed

58 → 52 is #767 (2026-09-05) removing the six-call durable-state family; 59 → 58 was #582. **This
is why §3.313 shipped a wrong number in its own first push** — 58 was measured at `c5c30286` and
was correct there; the rebase moved the tree underneath it.

**And `ABI.md` still documents seven host calls that no longer exist** — the six `cleat_*_state`
calls and `cleat_child_workflow_in_schema` (`grep -c cleat_set_state ABI.md engine/imports.go` →
2 and 0). An SDK author who binds one gets a module that **fails to instantiate**, which is
exactly the §3.312 Java failure: not an error at the call site, a dead module. Recorded in
`ABI.md` and left for its own PR rather than folded into a sweep about runtime names.

#### A limits table for a sandbox nothing runs

`docs/contributor/plugins/plugin-security.md` documented `--plugin-memory-limit` and
`--plugin-gas-limit`. **Neither flag exists.** Following that: `PluginLoader.LoadPlugin` has no
non-test callers, the only two non-test `NewPluginLoader` calls pass a nil `*Runtime`, and
`cmd/cleat-worker` constructs no loader — so no plugin module is compiled or instantiated on any
path a workflow reaches.

The wazero attribution there is **correct** — `PluginLoader` is genuinely wazero-typed — which is
why a sweep keyed on "wazero is wrong" would have passed over it. It was found by checking the
sentence *next to* the word, not the word. **A resource-limit table for an unwired path is the
most flattering error available**: it reads as defence-in-depth and measures nothing, and no one
re-derives a number that makes the system look safer.

Both plugin docs now carry the measurement inline rather than being quietly deleted, so the gap
cannot be closed by editing prose.

#### What is deliberately still open

  * `ABI.md`'s seven entries for removed host calls (above).
  * WASM plugin execution is not wired; two contributor guides describe it as if it were.
  * `engine/backend_wasmtime_stub.go`'s doc comment still tells callers how to fall back to
    wazero. There are no such callers.
  * `Engine.Execute`'s doc comment still says it decomposes Component Model binaries; the body
    directly below says that path was deleted (#528).
  * CLAUDE.md's own `58` / `55` export counts are now stale for the same reason as everything
    above. Left for a PR against CLAUDE.md rather than smuggled into a docs sweep.

---

### 3.407 A failed scope acquisition was reported to the guest through a field no SDK decodes — ✅ **FIXED 2026-09-09** (cleat#1062)

`freshSetScope` (`engine/scope.go`) returned `packSimpleResult(1, 0)` when `AcquireConcurrencyKey`
returned an error. That errCode is decoded by nobody. Verified at the call sites, not by name
search:

| SDK | what it does with the result |
|---|---|
| Rust | `let (prev_len, _err_code) = memory::decode_simple_result(result);` — discarded by name |
| AssemblyScript | reads `decoded.extra` for the length, never `decoded.errCode` |
| Java | ignores `result` entirely and returns its own `_scopePrefix` mirror |
| Python | `_import_set_scope` is a stub raising `NotImplementedError` |
| Go | discards it deliberately (#1060), to match Rust rather than diverge alone |

So a concurrency-key store failure returned to every guest as an ordinary success and the workflow
continued believing it held the key — a mutual-exclusion violation of the one guarantee scope
exists to provide.

**The fix is not to report it better. It is to stop reporting it as the mechanism.** Three things
already in the tree say so:

  * **The contention branch, ten lines below, enforces host-side and tells the guest nothing.**
    `!acquired` returns errCode **zero** — apparent success — and arms `s.suspendErr`. So
    `freshSetScope` already contained a working answer to "this workflow must not proceed believing
    it holds the scope", and the branch two lines above did not use it.
  * **`replaySetScope` was already written for the retry that nothing produced.** On a replayed
    `EventTypeScopeAcquired` carrying `Err` it declines to set the scope fields, calls
    `exitReplay()` and re-enters `freshSetScope` — commented *"switch to fresh to retry
    acquisition"*. Close to unreachable before this: the fresh path returned errCode 1, the guest
    ignored it and ran to completion, so there was no suspension and no replay to arrive there. The
    retry machinery was terminated at both ends with no wire between.
  * **The sibling caller of the same store method puts the failure where guests already look.**
    `freshAcquireLock` (`engine/locking.go`) returns `packAcquireLockResult(false, 1)` on a store
    error — `acquired=false` **and** errCode 1. A guest checking `!acquired`, which is the entire
    point of that API, is correct without reading the error code at all. `cleat_set_scope` has no
    `acquired` field; its only "did I get it" channel was the errCode, which is why the same
    oversight is a defect there and not in `acquire_lock`.

So the store-failure branch now arms `suspendErr`, matching its neighbour. **No SDK signature
changes and `ABI.md` is unchanged** — errCode 1 stays on the wire for anyone who later decides to
read it. Reporting through the return value could not have fixed this on its own: it makes mutual
exclusion contingent on five SDKs each choosing to check, and the host is the only party that can
refuse. Same asymmetry as §3.223's *"the engine is where the mechanism is legible"*.

The `Reason` deliberately names the store error and the scope key, because contention suspends too
and a suspend reason was otherwise the only thing an operator would see — a documented failure mode
needs a stated way to tell it apart from its neighbours (CLAUDE.md).

## The test named for this path tested the happy path, and said so

`TestSetScopeAcquisitionFailure` (`engine/host_dispatch_test.go`) set up `mockConcurrencyKeyStore`,
which always returns `(true, nil)`, and carried the comment *"The mock always returns
acquired=true, so this tests the happy path. For the failure path we'd need a different mock."* So
the one test bearing the defect's name never entered the branch, and the defect survived under a
green test that appeared to cover it. This is CLAUDE.md's *"the sharpest form is a test whose NAME
asserts the mechanism"*, in its cheapest possible form: the test even documented the gap.

Renamed to `TestSetScopeAcquisitionSucceeds` rather than deleted — a success control is worth
keeping beside the failure tests — and the four in `engine/scope_acquire_failure_test.go` cover the
branch it named. Both stores those tests need already existed in `locking_test.go`
(`acquireErrorStore`, `acquireNotAcquiredStore`), which is the finding in miniature: the two callers
of `AcquireConcurrencyKey` were given opposite treatments of the same two outcomes, and only one of
them had been tested for either.

**Falsification.** Removing the `suspendErr` assignment fails `TestSetScopeStoreFailureSuspends` at
the intended line with the intended message, and leaves all three controls green — including the
contention control, which confirms the mutation is specific to the branch changed rather than to
non-acquisition generally. `TestSetScopeReplayOfRecordedFailureRetriesAcquisition` passes under the
mutation too, and is reported as what it is: a characterisation of the pre-existing replay retry
that this change makes reachable, not a regression test for the change.

### 3.408 A guard keyed its baseline on a dialect PAIR, so a two-dialect run reported the same asymmetries as both new and closed — ✅ **FIXED 2026-09-09** (cleat#1087)

`TestEveryDialectAgreesWhichColumnsAWriterMustSupply` compared dialects against a base picked by
sort order — `sort.Strings(names); base := names[0]` — and rendered each finding as
`"<table>.<col>: X requires a value, Y supplies one"`, naming **both**. `knownColumnAsymmetries`
stored those rendered strings verbatim, so the baseline was keyed on whichever configured dialect
sorted first: `mssql` with all three up, `mysql` without SQL Server.

Measured 2026-09-09 in one environment, all three dialects available, varying only `CLEAT_TEST_MSSQL`:

| | two dialects (pg + mysql) | three dialects |
|---|---|---|
| before | **FAIL** | pass |
| after | pass | pass |

The two-dialect failure reported the **same two asymmetries in both directions at once**:

    dialects disagree about which columns a writer must supply:
      audit_events.id: mysql requires a value, postgres supplies one (plugin audit-log)
      event_subscriptions.id: mysql requires a value, postgres supplies one (plugin event-triggers)

    knownColumnAsymmetries records 2 asymmetr(ies) that no longer exist:
      audit_events.id: mysql requires a value, mssql supplies one (plugin audit-log)
      event_subscriptions.id: mysql requires a value, mssql supplies one (plugin event-triggers)

MySQL requires `audit_events.id`; PostgreSQL and SQL Server both supply it. That fact did not
change. Only the counterpart named in the string did, and the baseline matched on the string.

**The under-reporting half is the finding, and it is the one that would have been missed.** The
over-report costs an hour. The stale report says *"Good news, and the list must shrink to match"*
while pointing at two entirely correct entries — so the invited repair is to delete them, go green,
and permanently retire a guard that exists because cleat#958 recorded zero audit events on MySQL
for as long as it went unnoticed. The failing half looks like work to do; the passing half looks
like progress. Same selection effect as the flattering-number class in CLAUDE.md.

## Two changes, and the second is not cosmetic

**Key on the requiring dialect, one entry per dialect that requires the value.** `columnAsymmetry`
replaces the rendered string; which dialects *supply* it is derivable from what is configured and
belongs in the rendering. Both original entries already said so in their own comments — one noted
*"where Default and MSSQL omit it"* while the string could only name one of the two.

**Scope the stale check to what this run actually measured.** Keying alone does not fix it. A run
that did not configure a dialect gathered no evidence about it, so calling its baseline entry "no
longer exists" is a claim about something unmeasured — the same defect one size smaller. Narrowed,
**not removed**: an entry whose dialect *was* compared and whose asymmetry is gone must still be
reported, or the list stops shrinking.

The same question has a second axis, found by re-reading the fix rather than from any failure: a
table **absent from some configured dialect is skipped whole**, so it produces no finding — which
is indistinguishable from a closed asymmetry unless asked separately. The `missing` path is not
hypothetical (its own comment records a 20-line false report in CI), so a baseline entry on such a
table would have been called stale on the strength of a comparison that never ran. Both conditions
now gate the stale check, for one reason: **report an entry as gone only where this run had the
evidence to say so.** The general form is that the check was answering *"did we find this?"* when
the question is *"did we look?"*

## The skip that would have been the wrong fix

Worth recording because it was proposed and is the natural reading of the failure output. The test
already refuses it, in its own source, above the line:

> Every configured dialect must be reachable: a skip here would compare two dialects and call it
> agreement, which is the failure mode the whole test is about.

A two-dialect comparison is a real comparison. This was a baseline-keying bug, not a
skip-condition bug. **The error message is what misleads**: both assertions are written in the
imperative and neither can express "the comparison ran under a configuration the baseline was not
written for", so every reading of the output points away from the cause.

## Three falsifications, because one direction is not enough

The comparison is lifted into `compareRequiredColumns`, a pure function over
`map[string]schemaFacts`, and tested with synthetic facts and no database — the only way to assert
the two-dialect and three-dialect cases in the same run, on any machine.

A stale check that reports **nothing ever** also removes the false positive, and it passes a green
tree and a negative control identically. So each mutation is recorded with the tests it fails:

| mutation | fails |
|---|---|
| put the counterpart back in the key (the original defect) | the two configuration-independence tests, plus three more |
| drop the `configured[]` guard on the stale check | `…EntryForAnUnconfiguredDialectIsNotReportedStale`, and only that |
| `if false &&` on the stale check — **the overshoot** | `…ClosedEntryIsStillReportedStale` |
| drop the `compared[]` guard (the table axis) | `…SkippedTableIsNotEvidenceTheEntryClosed`, and only that |

Each of the last three fails a **different** test, which is what shows the suite separates "too
narrow" from "too broad" rather than merely noticing that something moved.

---

### 3.410 Terminating a workflow reaches its children and stops there — ✅ **FIXED 2026-09-09 by §3.412** (cleat#1108)

`TestTerminateWorkflowEnforcesParentClosePolicy` proves the close-policy cascade fires at **one**
level. Nothing addressed the level below — no test, no doc, no issue.

It stops at one. Root → child → grandchild, every edge `parent_close_policy = TERMINATE`, terminate
the root:

| | postgres | mysql | mssql |
|---|---|---|---|
| root | `terminated` | `terminated` | `terminated` |
| child | `failed` | `failed` | `failed` |
| **grandchild** | **untouched** | **untouched** | **untouched** |

#### The mechanism

`enforceParentClosePolicy`'s TERMINATE arm sets `status = 'failed'` with a direct `UPDATE`. That
does not go through `FailWorkflow`, and `FailWorkflow` is one of the four callers of
`enforceParentClosePolicy` — so closing a child by cascade never triggers the cascade for *its*
children.

#### Why this is `MEASURED` and not `FOUND`

Whether one level is *wrong* is a product question, for the reasons ports ISSUES 29 gives about the
neighbouring `cancel` case: a subtree terminate is a recursive `UPDATE` per dialect, and a detached
child must not inherit it. A comparable engine treats it as a choice — `durabletask-go` has
`WithRecursiveTerminate(bool)` and tests both branches over a three-level tree. cleat already has
the vocabulary (`parent_close_policy` is per child, so `TERMINATE` everywhere is `recurse=true` and
`ABANDON` is `recurse=false`); what it lacks is the depth.

#### A second reading, deliberately NOT measured

The defer-phase arm sets `status = 'terminating'` rather than failing outright, and those children
are finalised later through a path that **can** cascade again. If so, depth depends on whether an
intermediate workflow happened to owe defers. **The fixture's children owe no defers**, so they take
the direct arm and the test says nothing about it. Flagged in the test's own comment as a reading.

#### Falsification — three mutations, each moving a different assertion

| mutation | what went red |
|---|---|
| the grandchild is parented to the **root** instead of the child | the fixture control: "the tree is not three levels" |
| `enforceParentClosePolicy` recurses into each terminated child (3 lines) | the depth assertion: "the grandchild WAS reached" |
| `enforceParentClosePolicy` returns immediately | the cascade control: "the cascade did not fire at all and this test cannot say anything about depth" |

The third is the one that matters. Without it, an engine whose cascade was broken outright would
leave the grandchild untouched too, and the test would report "one level" while measuring zero —
the same vacuity as a retry test whose budget is one attempt.

---

### 3.411 A terminate reaches a grandchild only when the child in the middle owed a defer phase — ✅ **FIXED 2026-09-09 by §3.412** (cleat#1108)

§3.410 measured the plain case: terminate a root, its `TERMINATE` child is failed, the grandchild is
untouched. It recorded a **reading** that the defer arm might behave differently and said someone
should build the fixture. Built, and the reading holds — on all three dialects.

Identical tree, identical policies, identical terminate. **One `defer` row in the middle** is the
only difference:

| the child in the middle | the grandchild |
|---|---|
| owes no defers | **untouched** — orphaned (§3.410) |
| owes a defer phase | **`failed`** |

#### Why, and it is one asymmetry

`enforceParentClosePolicy` has two arms and they close a child by different mechanisms.

- **Plain arm:** `UPDATE ... SET status = 'failed' WHERE parent_workflow_id = $1`. A bulk write. It
  does not go through `FailWorkflow`, and `FailWorkflow` is one of the calls that *fires* the
  cascade — so the recursion point is bypassed by the mechanism doing the closing.
- **Defer arm:** `SET status = 'terminating'` with the outcome recorded. The child is claimed again,
  runs its defers, and is finalised by `FinalizeDeferPhase` — which **does** call
  `enforceParentClosePolicy` (`engine/store_defer_phase.go:78`; `ExpireDeferPhases` at `:142` too).

So one arm terminates the subtree and the other terminates one level.

#### Why this is worse than a flat one level

One level is a contract. This is not: **the depth of a terminate is a property of whether a
workflow in the middle happened to have deferred work** — its own code, invisible to whoever pressed
terminate, and changing when that workflow gains or loses a `defer`. An operator cannot predict how
much of a tree a terminate will close, and the same tree answers differently on different runs.

The attribution is pinned rather than assumed: the test asserts the grandchild is **still untouched**
after the terminate and before the defer phase completes, so the reach cannot be a two-level cascade
being credited to the wrong mechanism.

#### What this does not say

Which arm is *right*. The defer arm's behaviour is arguably the intended one and the plain arm the
defect; §3.410's framing — that a subtree terminate is a recursive `UPDATE` per dialect, and a
detached child must not inherit it — is unchanged. What is settled is that they **disagree**, and
that neither the code nor any document said so.


---

### 3.412 Closing a workflow now does what FailWorkflow does: release, then enforce its own close policy — ✅ **FIXED 2026-09-09** (cleat#1108)

Fixes §3.410 and §3.411 together, because they are one defect seen from two sides.

**The rule, decided by the owner:** closing a workflow must go through the path that enforces the
close policy. `FailWorkflow`'s post-commit is exactly two calls — release the workflow's resources,
then `enforceParentClosePolicy` on it. A child closed by the cascade got only the first.

**Why it could not literally call `FailWorkflow`, which is the constraint that shaped the fix.**
`FailWorkflow` fences on `WHERE id = $1 AND assigned_to = $2 AND generation = $7` and returns
`ErrFenceLost` otherwise. The cascade closes children it does **not** own — often unclaimed, or held
by another worker — and deliberately *breaks* their fence (`assigned_to = NULL`,
`generation = generation + 1`) so the holder cannot overwrite the termination. Calling `FailWorkflow`
would have returned `ErrFenceLost` for every child.

So the **post-commit half** is applied to each closed child instead: `releaseTerminatedChildren` was
already doing the release; `cascadeIntoClosedChildren` now does the enforce. One shared helper in
`engine/workflow_cleanup.go`, three thin call sites, rather than three copies of the recursion — the
drift that produced the original asymmetry.

**Only the plain arm's children are passed.** A defer-owing child is excluded from
`childrenClosedByTerminate` by construction and reaches its own children later through
`FinalizeDeferPhase`, so nothing cascades twice.

#### The depth bound is defensive and says so

`parent_workflow_id` is only ever written at INSERT, to a row that already exists, so the graph is
built in creation order and a workflow cannot become its own ancestor; continue-as-new *inherits* its
predecessor's parent rather than pointing at it. **That is a reading of the schema, and a terminal
path is the wrong place to discover it was wrong**, so `maxParentCloseDepth` (64) removes the
possibility for one comparison. Hitting it logs an ERROR naming the workflows left running rather
than failing silently.

#### The tests were inverted, not replaced

`TestTerminateCascadeDepthIsOneLevel` said in its own failure message to invert it rather than delete
it if the grandchild was ever reached. It is now
`TestTerminateCascadeReachesEveryDescendant`, and the history is in its header.

`TestBothCloseArmsReachTheSameDepth` replaces §3.411's measurement and is **one test, not two, on
purpose**: the disagreement survived because each arm looks correct from inside itself and nothing
compared them. Two tests — one per arm, in separate files — is the arrangement that let it happen,
since both passed. This builds both subtrees in one run and asserts the same outcome, with a control
that each arm was actually taken.

#### Falsification

Removing the recursion from all three dialects — the pre-fix state — reddens both, each on its own
assertion:

| test | message |
|---|---|
| `…ReachesEveryDescendant` | *"the grandchild is `ready` and unflagged: the cascade stopped at one level and it is running with no parent"* |
| `…BothCloseArmsReachTheSameDepth` | *"the two arms disagree on depth: plain-arm grandchild is `running` (reached=false), defer-arm grandchild is `failed` (reached=true)"* |

The second reproduces the original asymmetry exactly, which is what shows the test is about the
disagreement rather than about either arm.

---

### 3.413 The orphan-import scan judged the export wrapper's imports against the workflow's closure — ✅ **FIXED 2026-09-09** (cleat#1125)

Every WASM build warned that `cleat_complete` and `cleat_poll_work` were orphaned imports, including
builds of a workflow the toolchain itself reported as using **zero** host functions.

- `wasm/generator.go` writes both into every shim unconditionally — *"Always include
  `cleat_complete` — the export wrapper calls it"*. They are the **wrapper's**, not the workflow's.
- `wasm/scan.go`'s `FindCleatOrphanedImports` compares every `cleat_`-prefixed import against
  `usage.Used`, the **workflow's** computed closure, which cannot contain them by construction.

Two halves each correct about what they own, with nothing reconciling them. The same shape as
§3.410's cascade and as the `GetWorkflowByID` / `ListWorkflows` field asymmetry.

#### A guard that never disagrees carries no information, in either direction

Everything this file has recorded lately is a check that was too **quiet** — silent about a region,
blind in its denominator. This one is too **loud**, and it is the same defect: the output does not
depend on the input, so it can be produced without looking.

**And the harm is not the noise.** A true `W003` — *"your single string parameter receives the ENTIRE
input JSON"* — was emitted correctly and predicted the exact failure that surfaced two layers later
as a result stored as `{}`. It went unread and was nearly filed as a cleat defect, because it
arrived **third in a list whose first two entries are always wrong**. A channel whose first two
entries are always wrong trains its readers to skip it, and what the noise stands in front of is the
cost.

#### Why the names live in one place

A hand-maintained skip-list in `scan.go` reproduces the defect one level over: the generator stays
free to add a third unconditional import, the scan does not know, and the warning returns with
nobody having touched it. `normalizeImportName`'s variant map already has that weakness.

`generatorEmittedImports` is the reconciliation, and
`TestTheGeneratorEmitsExactlyTheImportsTheScanExempts` asserts the generator's emitted block declares
exactly that set **in both directions** — a third import fails at authoring time, and an exemption
covering nothing is reported as a grant waiting to cover something else.

The exemption is also narrow: these names are skipped only when absent from the closure, which is the
case the generator creates. A workflow that genuinely calls one has it in `Used` and never reaches
the check.

#### Falsification — three mutations, three distinct failure modes

| mutation | what went red |
|---|---|
| remove the exemption (the pre-fix tree) | the bug's own case, reporting exactly the two warnings from #1125 |
| `isGeneratorEmitted` returns true for everything | the control: *"expected exactly the one real orphan, got 0 — the generator exemption has swallowed the whole check"* |
| generator gains a third unconditional import | the reconciliation test, naming `cleat_log` |

The second is the one that matters. **Suppressing two warnings and deleting the scan produce
identical output on the case being fixed** — only an import that *should* warn separates them.

---

### 3.416 A retention sweep an operator can trigger, with the window override that stops it being inert — ✅ **DONE 2026-09-10** (cleat#1130)

Retention was unobservable from outside the engine. The window is integer **days** with `0` meaning
disabled, the predicate is `completed_at < cutoff`, and nothing on the HTTP surface started a sweep —
so an out-of-process observer could not produce a swept row without waiting a day or ageing
`completed_at` in the database. Operators had the same problem from the other side: no way to see a
configuration change take effect for up to `--retention-interval`.

`POST /api/admin/retention/sweep`, beside `/api/admin/drain`, gated on `--enable-admin-api` so it
inherits that exposure decision rather than making a new one.

#### The override is the feature; the trigger alone would be inert

`{"older_than": "5s"}` supplies a cutoff the flags cannot express. **Without it, an endpoint that
ran the configured sweep would match nothing for any run completed today, on every call, and report
success** — a feature that ships working and is provably inert. That is the sharpest form of the
pattern this file has recorded all night: the operation reports success without doing the thing, and
the report is the *correct* report.

Proven rather than argued, against a real database, in one test with two halves:

| request | a run completed moments ago |
|---|---|
| no override (configured 30-day window) | **survives** |
| `{"older_than":"1ns"}` | **deleted** |

The first half is not a formality. If both deleted, the configured window would be reaching live
work; if neither did, the endpoint would be the button.

#### What an override may not do

**Enable an arm the configuration disabled.** `--completed-workflow-retention-days` deletes the
`workflow_instances` row itself — status, result, error, def_name, not just step history — and is
off by default for that reason; its own flag help calls it materially more destructive. A request
body is not where a deployment's decision to leave it off gets reversed. Disabled arms are named in
`skipped`.

**That constraint was untested until a mutation found it.** Flipping the guard to
`completedWorkflowRetentionDays > 0 || window > 0` was caught by nothing;
`TestAnOverrideDoesNotEnableADisabledArm` now catches it.

#### Counts are per arm and never summed

Four arms — events, compaction state, completed workflows, dead-lettered — across three flags that
default differently (30 on, 0, 0). One total would be a number meaning four things, and an operator
could not tell *"nothing was old enough"* from *"that arm is off"*. `runRetentionSweep`'s own comment
already refuses to sum two of them; this carries that to the API. Partial failure answers **207**,
not 200, because the arms are independent and the counts beside a failure are real.

#### Verification

- End-to-end on live PostgreSQL, both halves.
- Falsified: ignoring the override reddens *"the run survived a sweep with older_than=1ns"*; letting
  the override enable a disabled arm reddens the new constraint test.
- **0 skips added under CI's own `CLEAT_TEST_DB`**, checked rather than assumed.
---

### 3.417 Every UUID read from SQL Server was a different UUID, and nothing errored — ✅ **FIXED 2026-09-10** (cleat#1137)

SQL Server returns `UNIQUEIDENTIFIER` in mixed-endian byte order. `uuid.UUID`'s `Scan` accepts those
16 bytes **without error** and yields a different id — so every plugin reading an id from SQL Server
got the wrong one, silently. `plugin.GUID` existed to swap them; what was open was how many sites
still scanned into `uuid.UUID` directly.

#### The count was 85, and every lexical reading undercounted

`&x` is a **name**; the defect is a **type**. Four regex-shaped scans gave four answers — 6, 27, 13
and 54 — and none of them was the question. Both documented readings fail in opposite directions:
the narrow one **misses the confirmed bug** (the scheduler's fault was on struct *fields*), and the
wide one **flags the fix**, because `dueSchedule` still has fields named `id`/`tenantID` typed
`uuid.UUID` beside the new `plugin.GUID` locals.

Resolved with `go/types`: **85 arguments across 15 plugins**. Confirmed by a second, differently
traversed reading — every address-of expression whose operand is `uuid.UUID`, without looking at
`Scan` at all — which returns 85 and reports **all 85 inside Scan calls, none outside**. Two
traversals agreeing on the total *and* the membership.

#### One abstraction, not 85 edits

`plugin.ScanRow(rows, dest...)` substitutes a `GUID` for any `*uuid.UUID` destination, scans, and
copies back on success only. Non-uuid destinations pass through untouched, so it applies to a whole
`Scan` call rather than to selected arguments — which matters, because deciding per-argument is what
a reader gets wrong.

85 hand edits would be 85 chances to err on paths no test exercises, and would leave the next author
free to write the 86th. *"A backlog of 200 similar findings is usually one missing abstraction"* —
this is that. **59 call sites rewritten mechanically** from the same type information that found
them, so the edit set and the finding set cannot disagree.

#### Falsification

| mutation | what went red |
|---|---|
| revert one call site to `rows.Scan` | the guard, naming `plugins/scheduler/routes.go:174` |
| `ScanRow` stops substituting | `TestScanRowCorrectsMixedEndianBytes` |

**The first falsification did not apply on its first attempt** — the regex required a trailing space
and the call is `plugin.ScanRow(rows,` followed by a newline — and the guard's resulting pass was
briefly read as a result. A falsification that does not apply is not a falsification that passed;
the mutation is now checked to have changed the file before the outcome is read.

`TestScanRowIsTheThingThatCorrects` is the control: it asserts a **direct** `uuid.UUID` scan of the
same bytes produces the *wrong* id, so the fixture is known to reproduce the defect rather than
being satisfied by any implementation.

---

### 3.418 Rebind could not tell SQL from a string inside SQL, so booleans were unsafe to add — ✅ **FIXED 2026-09-10** (cleat#1133)

**Scope note first, because this is one of four parts and a reader will otherwise
credit it with the rest.** cleat#1133 is 56 plugin SQL sites that fail on a dialect they
can reach. This section covers the *rewriter*. Applying it at the adapter, the guard, and
the 15 structural sites are separate.

`plugin.Rebind` translates the primary dialect's spelling into the target's — `$N` to
`?`/`@pN`, `now()` to `SYSUTCDATETIME()`. It did that with two regexes over the whole
statement, and **a regex cannot tell SQL from a string that appears inside SQL**:

    Rebind(`SELECT * FROM t WHERE label = 'costs $100' AND id = $1`, MSSQL)
      -> SELECT * FROM t WHERE label = 'costs @p100' AND id = @p1
                                                ^^^^ user-visible data, corrupted

No shipped plugin has a `$N` inside a string literal (`grep -rnoE "'[^']*\$[0-9][^']*'"`
over `plugins/` → nothing), so this was latent rather than live. **It was about to stop
being latent, for two independent reasons**: the rewrite moves from the 166 call sites
that opt in to the adapter, where it meets every plugin statement; and `TRUE`/`FALSE`
joins the substitution list, which is a token that appears in ordinary English in ordinary
columns — `WHERE note = 'set this to true'` is not exotic.

So `Rebind` now scans, copying through every quoted region and comment untouched, in all
four spellings the three dialects use (`'…'`, `"…"`, `` `…` ``, `[…]`) plus `--` and
`/* */`. That is more code than two regexes, and it is what keeps the rewrite in the
category of things an adapter may safely do.

**A TEST ASSERTED THE CORRUPTION, AND ITS NAME ASSERTED THE FIX.** `query_test.go` carried:

    name:    "mysql with dollar sign not a param",
    query:   "SELECT '$1' as price FROM users",
    want:    "SELECT '?' as price FROM users",

The name is right and has always been right — `$1` inside quotes is a *price*, not a
placeholder. The `want` then asserts it is rewritten anyway. The expectation was captured
from what the implementation did rather than derived from what the name says, so it locked
the defect in while the name went on describing the repair. This is the shape CLAUDE.md
records for `TestFinalizeDeferPhaseIsFencedOnTheClaimAndOnTheMarker`, arriving through a
golden value instead of a mechanism.

**Booleans: 38 tokens, 17 contexts, two shapes.** Derived from SQL string literals reached
via the Go parser, not from a text grep, because Go source is full of `true`:

    go run scripts/… # or: parse plugins/**/*.go, keep BasicLits matching
                     # ^\s*(SELECT|INSERT|UPDATE|DELETE|WITH), scan those for \b(TRUE|FALSE)\b

  * `= true` / `= false` — 28
  * a bare `true`/`false` in a `VALUES` list — 10

**A regex keyed on `=\s*true` sees the first shape and is blind to the second**, which is
why the scan was written against the literals rather than against an assumed spelling.
MySQL needs no boolean rewrite at all — `TRUE`/`FALSE` are documented aliases for `1`/`0`
— and does not get one.

**Why this is a binding error and not a syntax error, which is why a parse sweep says it
is fine.** T-SQL has no boolean type, so `enabled = true` resolves `true` as a *column
name*. `SET PARSEONLY ON` accepts it; only `SET NOEXEC ON`, which binds, rejects it. That
is 22 of #1133's sites and the reason §3.414's guard is lexical.

**Verified on a live SQL Server 2022, with a negative control**, against the real
`slack_config` table rather than a fixture built to fit the assumption (§3.415 is what
happens without that discipline):

| statement | result |
|---|---|
| shipped `Rebind` — `… AND enabled = true` | **REJECTED**: `mssql: Invalid column name 'true'.` |
| this change — `… AND enabled = 1` | **ACCEPTED** |

The control is load-bearing: without it, a server that accepts anything produces the same
pass. This reproduces from the other direction the `Invalid column name 'true'` ×9 that a
peer session attributed to `kafka-connect` in a four-minute worker log.

**Idempotence is asserted, not argued** (`TestRebindIsIdempotent`), because the adapter
will apply `Rebind` to statements whose call site already did: `@p1` contains no `$N`,
`SYSUTCDATETIME()` does not match `now()`, `1` does not match `TRUE`.

**What this fixes on its own: 8 sites** — the ones that already call `Rebind` and carry a
boolean (`eventtriggers/publish.go`, `kafkaconnect/host_functions.go`,
`oauthprovider/routes.go`, `pagerdutyalert/host_functions.go` ×2,
`slacknotify/host_functions.go`, `webhookingest/host_functions.go`,
`webhookingest/routes.go`). The other 48 wait on the adapter change and the structural work.

### 3.419 Plugins had to remember to translate their own SQL, and 40 sites did not — ✅ **FIXED 2026-09-10** (cleat#1133)

**cleat#1133, part 2 of 4.** §3.418 made the rewriter safe; this applies it. The guard and
the 15 structural sites are separate.

`plugin.Rebind` was opt-in at the call site. **166 sites called it and 40 did not**, so those
40 sent PostgreSQL `$N` placeholders to MySQL and SQL Server, where they are not placeholders.

**The tempting diagnosis is wrong and worth recording, because it would have produced a
different fix.** "The dialect was not available at the plugin" is false: **19 of 21 plugins
already hold a `plugin.Dialect` field**, and the only one that does not is `pgvector`
(`grep -rlE '(dialect|Dialect)\s+plugin\.Dialect' --include='*.go' plugins/`). These sites had
everything they needed. It was never a plumbing problem; it was a remembering problem, and the
fix for a remembering problem is to stop requiring the memory.

So `engine.SQLDBAdapter` and `engine.ReadOnlyDB` now rebind every statement on the way to the
driver, along with their transaction types. Safe for the 166 sites that already do it
themselves because Rebind is idempotent, which §3.418 asserts rather than argues.

**SCOPE IS A DELIBERATE LINE, NOT A LIMIT WE RAN INTO.** The adapter handles the frequent,
mechanical differences — placeholders, `now()`, boolean literals: 75 of the 90 token instances.
It deliberately does **not** attempt `LIMIT`/`TOP`, `ON CONFLICT`/`MERGE` or
`RETURNING`/`OUTPUT`. Those change the *shape* of the statement rather than a token in it —
`LIMIT n` → `TOP n` moves to a different clause, and T-SQL's `OFFSET…FETCH` additionally
requires an `ORDER BY`. A rewrite that ambitious buried in an adapter would be unreviewable.
Plugins handle those 15 with conditional code on `Environment.Dialect`.

**THE MULTI-DIALECT SUITE COULD NOT HAVE DETECTED THIS CHANGE, WHICH IS THE FINDING.** Every
multi-backend plugin test built its adapter as:

    p.db = &engine.SQLDBAdapter{DB: be.DB}          // be.Dialect, right there, dropped

`PluginTestBackend` carries `DB` **and** `Dialect`. Six sites across four files took the first
and dropped the second, so the plugin under test received its statements unrewritten on every
backend — and the entire multi-dialect plugin suite would have passed identically with the
adapter's rewrite present or absent. `tests/plugin-harness/harness.go:50` had the same omission
**with the dialect in its own function signature**, which means the harness could not detect
the class of defect it exists to catch.

That is the shape this file keeps recording: not a check that is wrong, a check that is
**silent** — and it is why part 2 is not finished by the adapter change alone.

**A ZERO DIALECT IS A NO-OP THAT LOOKS LIKE A WORKING REWRITE**, which is why the constructors
now take it as a *required parameter* rather than a settable field. `getPluginDB(db, pluginDB,
dialect)` cannot be called with it omitted; the struct literal could be, and was, four times.

**The guard covers where omission is definitely wrong, not everywhere.** There are ~235 adapter
constructions in the tree and nearly all are single-backend PostgreSQL fixtures, where no
rewrite is correct. Requiring the field universally would be a sweep that teaches people to
type `Dialect: DialectPostgres` without meaning it. `TestEveryDialectSensitiveAdapterCarriesItsDialect`
covers two cases: non-test code, and test files that use `NewPluginTestBackends`. It reports
**11 dialect-sensitive constructions across 1102 tracked files**, and uses `git ls-files`
rather than a walk — this checkout has fourteen worktrees under it, and a walk attributes
their contents to the main tree, a scope error that makes a guard *more* likely to pass as the
working tree gets messier.

**Verified with a known-positive on a real site**, not only a synthetic one: reverting
`kvstore_multidb_test.go:72` makes the guard fail naming that file and line. The synthetic
control proves the AST matcher works; the real one proves the guard does.

**`engine/testutil` cannot import `plugin` or `engine`** — both packages' own tests import
`testutil`, so either would be an import cycle *in the test binary*. `go build` does not
notice; `go vet` does. That is why `testutil.Dialect` is a duplicated type and why the fix is
`plugin.Dialect(be.Dialect)` at six sites rather than a helper that hands out a configured
`PluginDB`. `TestDialectConstantsAgree` compares the two constant **sets**, not their count.

**A grep for the import path found two `engine/*.go` files mentioning `engine/testutil` and
both were comments about it**, which would have made the cycle look pre-existing and settled.
Checked before concluding.

### 3.420 Three plugin statements were valid on PostgreSQL and nowhere else — ✅ **FIXED 2026-09-10** (cleat#1133)

**cleat#1133, part 4 of 4.** §3.418 made the rewriter safe, §3.419 applied it at the adapter.
These are the statements the adapter deliberately does not touch, because they change the
*shape* of a statement rather than a token in it.

| site | fault | rejected by |
|---|---|---|
| `eventtriggers/queries.go` **MSSQL arm** | `WHERE NOT processed` | SQL Server |
| `eventtriggers/host_functions.go:74` | `NOT processed`, `LIMIT 1` | SQL Server |
| `webhookingest/background.go:45` | `NOT e.processed`, `NOW() - INTERVAL '10 seconds'`, `LIMIT 100` | SQL Server **and MySQL** |

**THE MSSQL ARM IS THE INSTRUCTIVE ONE.** In one literal, `LIMIT 100` had been translated to
`OFFSET 0 ROWS FETCH NEXT 100 ROWS ONLY` and `NOW() - INTERVAL` to `DATEADD` — and
`NOT processed` was left as written. Someone translated this arm carefully and stopped at the
constructs they were thinking about. That is the failure the `plugin.Query` shape invites and
which §3.414 records generally: naming what a variant is *for* narrows the reviewer to that
purpose, and the rest of the literal inherits the primary dialect unexamined.

**THE ERROR MESSAGE NAMES THE WRONG CONSTRUCT**, which is why nobody followed it here.
Measured against SQL Server 2022, same table, three statements:

| statement | server says |
|---|---|
| `WHERE NOT processed`, no `OFFSET/FETCH` | `Msg 4145` non-boolean type — **the cause** |
| `WHERE NOT processed` **+** `OFFSET…FETCH` | `Msg 4145` near `'ORDER'` **and** `Msg 153` |
| `WHERE processed = 0` + `OFFSET…FETCH` | succeeds |

SQL Server reports **both**; the Go driver surfaces only the **last**. So the worker log says
`Invalid usage of the option NEXT in the FETCH statement` — naming a clause that is correct
T-SQL — while the defect is the boolean two lines above. A reader who trusts the message goes
to the row-limit clause and finds nothing wrong.

**That invalidates error-text census as a way to size this work**, and one was in use: a peer
session's four-minute log survey grouped 163 errors into six categories. At least one category
is a *consequence*, and `webhook-ingest` appears twice — once as non-boolean, once as
`Error 1064` — which is **one statement seen from two dialects**, not two defects. Withdrawn by
its author on cleat#1143 once reproduced. The useful output is **statement identity**, not
error text.

**Why the adapter does not do these.** A boolean *column* is not a boolean *literal*: rewriting
`NOT x` would have to leave `NOT EXISTS`, `NOT IN`, `NOT LIKE`, `NOT NULL` and `NOT (a AND b)`
alone. And `LIMIT n` → `TOP n` relocates a token to a different clause, while `OFFSET…FETCH`
additionally requires an `ORDER BY`. Both belong in an explicit arm.

**A COUNT I PUBLISHED AND HAD TO RETRACT, IN THE INFLATING DIRECTION.** I reported **5** bare
boolean sites; it is **3**. The scan flagged every `NOT <col>` without asking which dialect arm
it sat in — and `NOT processed` in a `Default` or `MySQL` arm is *correct for that dialect*. It
also counted `WHEN NOT MATCHED` from `MERGE` statements, which is not a boolean at all.

    A construct is only a defect if it can REACH a dialect that rejects it.

For a `plugin.Query` that means asking which arm, **including the fallback**: `Query.For`
returns `Default` for MSSQL when no MSSQL arm exists, so a `Default` arm is not automatically
PostgreSQL-only. Re-derive arm-aware, never by matching the token alone.

**Verified by execution, not by reading.** `TestEveryQueryArmRunsOnItsOwnDialect` and
`TestTheBatchQueryRunsOnEveryDialect` run each arm against a real server of that dialect, on
the schema built by the plugin's own migrations — not a fixture written to suit the query,
which is what §3.415 cost. The webhookingest test also asserts the column *count* the caller
scans, since a valid statement returning the wrong shape fails later, at `Scan`, in the same
silent loop.

**Falsifications, each red for its own reason:** reverting the boolean reddens `mssql`;
reverting the interval reddens `mysql` with `Error 1064`. Two faults in one statement need two
falsifications, or the second is only assumed.

**THE REMAINING FOUR, DONE IN THE SAME PASS**, because they are the same decision applied to
four more statements:

| site | fault | arm written |
|---|---|---|
| `notifications/background.go` | `LIMIT 100` | `SELECT TOP 100` |
| `eventstore/routes.go` | `LIMIT $4` — a **parameter**, not a literal | `OFFSET 0 ROWS FETCH NEXT $4 ROWS ONLY` |
| `jobqueue/background.go` | `LIMIT 10` | `SELECT TOP 10` |
| `blobstore/host_functions.go` | `ON CONFLICT DO NOTHING` | `INSERT … SELECT … WHERE NOT EXISTS` |

`jobqueue`'s is `pollPending`, and it is the one that shows why a guard over `plugin.Query`
*declarations* could never have closed this. §3.414 and §3.415 fixed the reaper's `UPDATE`,
twice. This `SELECT` sits **eight lines above** one of the values they were fixing, as a raw
literal, and was in neither. So after both repairs the reaper was correct and had nothing to
reap on SQL Server, because no job could reach `running` there. **`plugin.Query` was never the
boundary of the defect, only the boundary of the fix.** A check has to anchor on where SQL is
*executed*, not where a dialect table is *declared*. Found by a peer session reading the file;
confirmed here.

`eventstore`'s is the one with a shape worth noting: the limit is a **parameter**, which rules
out the usual `SELECT TOP n` (a variable needs `TOP (@p4)`), and the `$4` is deliberately left
as `$4` in every arm — placeholders are the adapter's job, clause structure is the arm's.

`blobstore`'s T-SQL arm uses `NOT EXISTS` rather than `MERGE`. `MERGE` is the textbook answer
and the wrong one here: heavier, with documented concurrency caveats, for a best-effort
reference count whose failure is already only logged.

**ONE HELPER, NOT SIX COPIES.** `plugins/plugintest.RunEveryArm` runs each arm against a real
server of its dialect, on the schema the plugin's own migrations build. It lives in its own
package because the natural home cannot host it: `engine/testutil` is imported by `engine`'s
and `plugin`'s own tests, so it can import neither — an import cycle **in the test binary**,
which `go build` does not notice and `go vet` does. `plugins/*` are leaves, so a helper there
can import everything it needs.

**Falsifications, each red for its own reason and each naming an error a peer had measured in
a live worker log:** reverting the eventtriggers boolean → `mssql`; reverting the webhookingest
interval → `mysql`, `Error 1064`; reverting the jobqueue `TOP 10` → `mssql`,
`Incorrect syntax near 'LIMIT'` — which was 61 of that log's errors, all attributed to
`jobqueue: poll failed`.

**Still open in #1133 after this:** `pgvector`'s six, which are PostgreSQL-only by declaration —
it ships no `UpMySQL`/`UpMSSQL` migration arms, so its tables never exist elsewhere and its
queries fail on a missing table either way. That the declaration is recorded and then never read
is cleat#1157.

### 3.421 A guard anchored on where SQL is executed, not where a dialect table is declared — ✅ **DONE 2026-09-10** (cleat#1133)

**cleat#1133, part 3 of 4** — written last, because a guard is worth more once the tree it
guards is clean. §3.418 made the rewriter safe, §3.419 applied it at the adapter, §3.420 fixed
the seven statements it deliberately does not rewrite. This stops the class returning.

**THE EXISTING GUARD WAS NECESSARY AND NOT SUFFICIENT, AND THE GAP HAS A PRICE ATTACHED.**
`plugin/dialect_sql_test.go` (§3.414) checks that each **arm** of a `plugin.Query` is valid for
the dialect it names. `jobqueue`'s `pollPending` is a raw literal carrying `LIMIT 10`, sitting
**eight lines above** a `plugin.Query` that §3.414 and §3.415 both edited. Neither touched it,
because neither was looking at call sites — so after two rounds of fixing, the reaper was
correct and had nothing to reap on SQL Server, because no job could reach `running` there.

    plugin.Query was never the boundary of the defect, only the boundary of the fix.

So the unit here is the **execution site**: every string literal handed to `db.Query`, `db.Exec`
or `db.QueryRow`. 135 of them.

**WHAT IT CHECKS, AND WHY EACH IS NOT THE ADAPTER'S JOB.** `plugin.Rebind` handles `$N`, `now()`
and boolean *literals* centrally. These change the *shape* of a statement rather than a token in
it: `LIMIT`, `ON CONFLICT`, `RETURNING`, `INTERVAL '…'`, `ILIKE`, and a bare boolean *column*
(`NOT processed`). The last is a **binding** error in T-SQL (Msg 4145), not a syntax error, so
`SET PARSEONLY ON` reports it clean — a parse-based sweep cannot substitute for this.

**THE EXEMPTION IS DERIVED, NOT LISTED**, which is the part worth reusing. A plugin that declares
migrations and ships no `UpMySQL`/`UpMSSQL` arm has already said it is PostgreSQL-only —
`plugin/migration.go`'s own doc names `pgvector` as the case. Its tables never exist elsewhere,
so its SQL is only required to be valid PostgreSQL. That predicate lives in the code, so it
cannot go stale: add a MySQL arm to `pgvector` and this guard begins requiring portable SQL of
it on the same commit. One package is exempt today, and the guard prints which.

An allowlist of names would have needed a hand-written reason per entry, and a reason nobody can
falsify reads as review having happened.

**THREE DEFECTS IN THE GUARD ITSELF, ALL FOUND BEFORE IT SHIPPED, ALL PERMISSIVE:**

  * **RE2 has no lookahead.** `NOT\s+(?!EXISTS|IN|…)` does not compile, and `regexp.MustCompile`
    *panics* rather than failing a vet — `go vet` passed on it. Rewritten as a match plus an
    explicit operator set.
  * **The dialect keys live in the ELEMENT literals.** `[]plugin.Migration{{Up: …}}` — the inner
    literals carry no type, so `lit.Type` is nil for them and a scan keyed on the type name never
    sees them. Every plugin therefore looked PostgreSQL-only and every package was skipped.
  * That was caught **only** by the guard's own `checked == 0` assertion, which refuses to report
    a pass when the scan matched nothing. Without it, the guard would have shipped green,
    exempting the entire tree, and looked exactly like this one does.

**A COUNT I PUBLISHED AND RETRACTED, IN THE INFLATING DIRECTION.** I reported **5** bare-boolean
sites for §3.420; it was **3**. The scan flagged every `NOT <col>` without asking which dialect
arm it sat in — and `NOT processed` in a `Default` or `MySQL` arm is *correct for that dialect*
— and it counted `WHEN NOT MATCHED` from `MERGE`, which is not a boolean at all.

    A construct is only a defect if it can REACH a dialect that rejects it.

For a `plugin.Query` that means asking which arm, **including the fallback**: `Query.For` returns
`Default` for MSSQL when no MSSQL arm exists, so a `Default` arm is not automatically
PostgreSQL-only. Both mistakes are now in the guard's own control.

**Controls, both directions.** `TestTheReachabilityGuardSeesEachConstruct` asserts each construct
*is* flagged, and that nine lookalikes are *not* — `NOT EXISTS`, `NOT IN`, `NOT LIKE`,
`IS NOT NULL`, `WHEN NOT MATCHED`, MySQL's unquoted `INTERVAL 10 SECOND`, `SELECT TOP`,
`OFFSET…FETCH`, and `processed = 0`. It runs on synthetic strings, so it keeps working once the
tree is clean, when a real-site mutation no longer exists to perform. **Also verified with a
known-positive on a real site**: putting a raw `LIMIT` literal back into
`notifications/background.go` makes the guard fail naming that file and line.
### 3.422 A TTL assertion that a slow runner fails, in the test written to remove timing dependence — ✅ **FIXED 2026-09-10**

`TestConcurrencyKeyTTLKeepsSubSecondPrecision/mssql/500ms` went red on `Test SQL Server`:

    a 500ms lock was stored already expired (-163.188ms remaining): the next caller
    takes it, and two workflows hold the same key

**The implementation was correct.** A 500 ms TTL with a database-side round trip of about
663 ms yields a negative remainder, and so does a *correct* implementation on a loaded
runner. The check asserted a property of the machine.

**THE SAME DEFECT, IN THE SAME TEST, AS THE ONE ITS OWN COMMENT DESCRIBES.** The lower bound
used to be `remaining < ttl/2` and failed identically on 2026-08-07 (run 31145314648, *"a
500ms lock expires in 202.46ms"*) with nothing wrong. It was replaced by `ttl - dbElapsed`,
which needs no slack and no tuning, and the file carries a long comment on why guessing a
fraction is wrong. **The `remaining <= 0` check three lines above was left timing-dependent in
that same edit** — and it is the one that fired.

The fix is the guard, not slack: `remaining <= 0 && dbElapsed < ttl`. If the round trip took
longer than the TTL, a correct implementation *also* yields a negative remainder and the lock
really has expired.

**NOTHING IS LOST, AND THAT IS MEASURED RATHER THAN ARGUED.** Restoring the historical
truncation defect on PostgreSQL (`float64(int(ttl.Seconds()))`) still reddens the test, and
the two assertions divide the space exactly:

| case | round trip | caught by |
|---|---|---|
| 500 ms | 9.7 ms | the zero check |
| 999 ms | 3.8 ms | the zero check |
| 1.5 s → 1 s | 3.9 ms | the **lower bound** — remainder is positive, so the zero check cannot see it |
| 30 s | — | correctly passes; truncation is a no-op |

A truncated TTL stores `expires_at == acquire time`, so `remaining ≈ -dbElapsed`, which is
below `ttl - dbElapsed` for every positive `ttl` **at any speed**. The lower bound therefore
catches truncation the zero check must skip.

**And the slow runner was reproduced rather than reasoned about.** Inserting a 700 ms sleep
between the acquire and the read-back, with a *correct* implementation:

| | |
|---|---|
| guard removed (today's code) | **FAIL** — `-208.262ms remaining, database-side round trip 709.663ms` |
| guard present | **PASS**, all four TTLs |

That reproduces the CI failure's shape and magnitude locally, which is what distinguishes
"this is a flaky test" from "this is a flake". The failure message now prints `dbElapsed`, so
the next reader is not left to infer the round trip from the size of the negative number.

### 3.423 A statement that reaches an RLS table with no tenant set, and why a tenant predicate does not save it — ✅ **DONE 2026-09-10** (cleat#1178)

`PostgresStore.successorOfRun` issued `SELECT id FROM workflow_instances WHERE continued_from
= $1` through `s.db.QueryRowContext` — no transaction, so no
`set_config('cleat.tenant_id', …)`. That table is `ENABLE` + `FORCE ROW LEVEL SECURITY` with a
fail-closed policy whose `USING` calls `cleat.assert_tenant_set()`, which `RAISE`s when the
tenant is unset. Every continue-as-new chain with a successor errored (cleat#1177, fix in
flight as cleat#1179). This is the guard that keeps the class at zero.

**THE DECIDING VARIABLE IS A PROPERTY OF THE TRANSACTION, NOT OF THE SQL**, and the obvious
criterion is wrong in the direction that makes a guard useless. Measured by cleat#1178 on a
live database as `cleat_app`:

    SELECT id FROM workflow_instances WHERE continued_from='…' AND tenant_id='2222…';
    ERROR:  cleat.tenant_id is not set -- tenant context required for RLS-scoped query

A policy is applied **in addition** to the query's own predicates, never instead of them. A
guard keyed on "the SQL mentions `tenant_id`" passes both known faults, including the one it
exists to prevent.

**That is asserted, not just described.** Deleting `AND tenant_id = $3` from a statement that
runs inside a proper RLS transaction leaves the guard **silent** — the second half of the
known-positive, and the half that proves it is keyed on the right variable. The first half:
unexempting `terminal_run.go:132` makes it report exactly that site; moving a safe `tx`
statement onto `s.db` makes it report the new one.

**WHY STATIC AND NOT AN INTEGRATION TEST.** A policy's `USING` is evaluated **per candidate
row**, so the same check against an empty table returns `(0 rows)` and no error under every
role — indistinguishable from working. Two sessions were caught by that on 2026-09-10. And CI
could not find these anyway: every job hands the Go suite a superuser DSN, and a superuser
bypasses RLS unconditionally, `FORCE` included.

**THREE DEFECTS IN THE GUARD, ALL FOUND BY RUNNING IT, ALL IN THE OVER-REPORTING DIRECTION** —
which is the cheaper direction to have, but a guard that reports non-faults gets switched off:

  * **Filtering by filename cannot separate the dialects.** All three `successorOfRun`
    implementations live in `terminal_run.go`, so a scan skipping `mysql_*`/`mssql_*` files
    still scored the MySQL and MSSQL arms — neither of which has RLS. The **receiver type** is
    the only thing that can separate them.
  * **A statement can establish the tenant in its own SQL.** The adaptive flusher carries
    `WITH cfg AS (SELECT set_config('cleat.tenant_id', …, true))` ahead of its `INSERT`
    (`flush.go:102` explains why the CTE form is needed there). A guard looking only for
    Go-level `setRLSOnTx`/`beginTxWithRLS` calls scores both flusher statements a fault.
  * **A table name inside a string literal is not a reference to the table.**
    `pg_total_relation_size('event_history')` takes the name and reads no rows, so no policy is
    evaluated. Stripping quoted literals before matching handles that shape wherever it
    appears, rather than exempting one call site.

**MY CENSUS AND THE ISSUE'S DISAGREED ON MEMBERSHIP, AND THAT WAS THE USEFUL PART.** cleat#1178
reports one live and one latent, scoped to `PostgresStore`. Scanning every receiver found nine
more latent instances — `WorkflowLoader` ×6 and `FaultInjector` ×3 — all reaching RLS tables
outside a transaction. **Neither type has a production constructor**: every call is in
`engine/unit_test.go` and every one passes a `nil` db.

    grep -rn 'NewWorkflowLoader(\|NewFaultInjector(' --include='*.go' . | grep -v 'func New'

So they are excluded, by name and with that command recorded, rather than exempted — guarding
them would add nine allowances for no safety. The live count is one, and it agrees with the
issue.

**Exemptions are required to stay LIVE, and that fired within minutes of being written.**
An allowlist that may only shrink is the usual goal; one that *cannot outlive its cause* is the
enforceable form. cleat#1179 merged while this branch was open, and the guard said so itself:

    the exemption for terminal_run.go:132 no longer matches any statement. It has
    been fixed or moved -- delete the entry rather than leaving an allowance whose
    cause is gone

Nobody had to remember. **A stale allowance is not inert**: `terminal_run.go:132` is now an
ordinary line, and an exemption still naming it would silently cover whatever statement arrives
there next.

The entry is deleted, and the known-positive moved with it — re-breaking `successorOfRun` back
to `s.db.QueryRowContext` now makes the guard report `terminal_run.go:156`. That is the stronger
control: it shows the guard **protects the fix**, not merely that it once described the bug.

**The RLS table list is read from `migrations/postgres/`, not written here.** Eleven today; a
literal silently stops covering the twelfth. Comments are stripped first, or a header quoting
an `ALTER TABLE … ENABLE ROW LEVEL SECURITY` counts as a declaration.

### 3.424 A concurrency key was global across tenants — the second instance of a class that already had a written fix — ✅ **FIXED 2026-09-10** (cleat#1189)

`concurrency_keys` was `PRIMARY KEY (key_hash)` with the hash computed from the key text alone
— `digest(<key>, 'sha256')`, no tenant. **The key namespace was global while every operation on
the table is tenant-scoped and the table is under RLS**, so the uniqueness dimension and the
access dimension disagreed.

The consequence is worse than a refusal. Tenant 2 could not acquire; could not *release*,
because its `DELETE` carries `AND tenant_id = <its own>` and matched nothing; and could not
*see* the blocking row, because RLS correctly hides another tenant's. **Blocked, unclearable
and invisible** until the TTL ran out. The colliding names are the ones everyone picks —
`nightly`, `sync`, `cleanup`.

**THIS IS THE SECOND INSTANCE OF A CLASS THAT ALREADY HAD A WRITTEN FIX**, which is the part
worth carrying. `idempotency_keys` had the identical shape and was repaired by migration 010;
its post-mortem is quoted in `store_lifecycle.go:770` — two customers both choosing
`order-123`, and the second handed the first's workflow ID with `alreadyExisted = true` while
its own workflow was never started. Same client-supplied string, same global namespace, same
outcome. **The sibling table was left behind**, and nothing existed to notice that.

**Why not fold the tenant into the hash, which needs no migration at all.** Migration 010
considered and rejected that for `idempotency_keys`: it changes every hash, so no existing key
matches after the upgrade and a retried request starts a second workflow. The reasoning
transfers with the consequence changed — here, existing locks would become invisible to their
holders and a second workflow could acquire a key someone is still holding, a double-acquire
window for the length of the TTL in a table whose whole purpose is mutual exclusion. Changing
the key preserves every row, and `concurrency_keys` already carries `tenant_id NOT NULL`, so
unlike 010 there is no column to add.

**EACH DIALECT REFUSED FOR A DIFFERENT REASON, so the fix is not one change** — and a test
running only on PostgreSQL would have reported the SQL Server path fixed:

| dialect | how it refused | fix |
|---|---|---|
| postgres | `ON CONFLICT (key_hash) DO NOTHING` — conflict target was the whole key | conflict target |
| mysql | `INSERT IGNORE`, which relies on the PRIMARY KEY | **migration alone** |
| mssql | `WHERE NOT EXISTS (… key_hash = @p1 …)` — **no tenant predicate at all** | + `AND tenant_id` |

MySQL needed one step the others did not: its `tenant_id` is `CHAR(36)` with no `NOT NULL`,
while PostgreSQL and SQL Server both declare it `NOT NULL` with the all-zero default. A
nullable column cannot sit in a primary key, and MySQL would silently coerce it and take the
implicit `''` default — **a different tenant id from the one every other dialect uses**. The
migration makes it `NOT NULL` with the matching default, explicitly, first.

**Three falsifications, one per dialect, because there are three distinct fixes.** Reverting
the PostgreSQL conflict target reddens postgres; removing the MSSQL tenant predicate reddens
mssql naming the key; and reverting the *schema* in the live MySQL database — the migration
being the entire fix there — reddens mysql. One falsification would have proved one third of
this.

**The test also asserts the mutex still excludes.** "Two tenants can hold it" is satisfied by a
lock that excludes nobody, so the same test re-acquires as the *same* tenant and requires a
refusal, and checks that one tenant's release does not free another's row.

**AND THE DEFECT WAS ALREADY WRITTEN DOWN — AS A SPECIFICATION, IN THE TEST NAMED FOR THE
PROPERTY IT VIOLATES.** `TestTenantIsolation_ConcurrencyKeys` failed on all three dialects after
this fix, on an assertion that reads:

    // Now storeA cannot acquire — key is held by storeB (PK conflict).
    if acquired {
        t.Error("storeA should not acquire iso-key while storeB holds it")
    }

and its Part 1 opened by stating the premise outright:

    // concurrency_keys has PRIMARY KEY (key_hash) alone, so two tenants cannot
    // simultaneously hold the same key name. The test works within this
    // constraint, verifying tenant-scoped release isolation and sequential
    // reuse across tenants.

That is an accurate description of the schema, correctly attributing the mechanism, and it is
**not a constraint** — it is this defect, recorded as a design property and then built around.
*"The test works within this constraint"* is how a defect becomes a specification: the author
saw it, described it precisely, and shaped the assertions to accommodate it.

This is the same shape as §3.418's `"mysql with dollar sign not a param"`, where an expectation
captured from the implementation sat under a name describing the fix — but a degree worse,
because here the accommodation is *documented and reasoned about* rather than merely typed. A
reviewer reading that comment would have found it persuasive.

The assertion is now its own negation, the premise is rewritten, and every other assertion in
that test was correct and is unchanged: a tenant must still exclude itself, and one tenant's
release must not reach another's row.

**A census, with two corrections to my own first pass — both over-reporting.** Scanning every
tenant-scoped table for a primary key omitting `tenant_id` first reported ten. `workflow_schedules`
and `workflow_tags` are **already fixed** (`036_workflow_schedules_tenant_in_key.sql`); my scan
matched `ADD CONSTRAINT <name> PRIMARY KEY (...)` and missed the bare `ADD PRIMARY KEY (...)`
form. **The final key is what matters, and migrations restate it in more than one syntax.** I
nearly filed a fixed bug.

Corrected, eight remain — and **a PK omitting `tenant_id` is not itself the defect**. The
defect needs the key to be **derived from a client-supplied string**, so two tenants naturally
choose the same value. `tenant_api_keys.key_id` and `workflow_routing.id` are generated. The
remaining five (`event_history`, `workflow_instances`, `workflow_promises`, `workflow_signals`,
`workflow_update_requests`) all reduce to one unanswered question: whether a **run ID** is
client-supplied. `StartNewRun` generates a UUID when `runID == ""` but accepts one. That is
recorded on cleat#1189 as a question, not a finding — the HTTP path has not been traced.

---

### 3.425 blobstore's expiry phase decremented `ref_count` on PostgreSQL only, for two different reasons — ✅ **FIXED 2026-09-10** (cleat#1148, cleat#1142)

`cleanupExpired` phase 2 deletes expired and soft-deleted `blob_index` rows and subtracts one
from `blob_content.ref_count` per row removed. Phase 3 then collects any content whose count
reached zero. **Phase 2 performed no decrement at all on MySQL or SQL Server**, so phase 3 found
nothing to collect on either, and blob storage grew without bound on both.

Two dialects, two unrelated causes, one property.

| | cause | symptom |
|---|---|---|
| SQL Server | a `DELETE` inside a CTE | one `ERROR` an hour, phase 3 unreachable |
| MySQL | the `DELETE` ran **before** the `UPDATE` that counts its rows | **none** |

**T-SQL requires a `WITH` body to be a `SELECT`.** The MSSQL arm was the PostgreSQL statement with
`RETURNING` swapped for `OUTPUT` — the right token translation through a structural difference
that does not survive it. Measured on SQL Server 2022, the server returns *two* errors and the Go
driver surfaces only the last, so the log reads `Incorrect syntax near ')'` and points at the CTE's
closing bracket rather than at the `DELETE` two lines above:

    Msg 156 ... Incorrect syntax near the keyword 'DELETE'.
    Msg 102 ... Incorrect syntax near ')'.

The repair is `DELETE … OUTPUT DELETED.sha256 INTO @deleted` followed by an `UPDATE … FROM` over
the table variable. That is two statements where PostgreSQL has one: a crash between them leaves
the index rows gone and `ref_count` too high, which is *the leak this removes*, not a new failure
mode. Stated in the code rather than papered over with a transaction inside a plugin query string.

**The MySQL half is the one worth remembering, because nothing reported it.** The arm is valid SQL
and the fault is the order of two `Exec` calls in `background.go`. Its subquery counts index rows
matching the expiry predicate; the `DELETE` had just removed them, so the join was empty, the
update touched nothing, and `affected` — reported as `expiredEntries` — was always 0. A comment
above the branch stated the order plainly, and the order was the defect.

Measured on live servers, same fixture, three contents at `ref_count` 3 / 1 / 2 with two of the
first's three references expired and the second's sole reference soft-deleted:

| | PostgreSQL | MySQL before | MySQL after | SQL Server before | SQL Server after |
|---|---|---|---|---|---|
| two of three refs expired | 3 → 1 | 3 → **3** | 3 → 1 | error | 3 → 1 |
| sole ref soft-deleted | 1 → 0 | 1 → **1** | 1 → 0 | error | 1 → 0 |
| nothing expiring | 2 → 2 | 2 → 2 | 2 → 2 | error | 2 → 2 |

**A silent wrong answer is worse than a loud one**, and this pair is the clean demonstration.
cleat-ports' worker-log check found the SQL Server half precisely because SQL Server complains.
Nothing could have found the MySQL half that way; it took asking what the count *became*.

**Why the existing arm test could not have caught it.** `RunEveryArm` (3.42x, cleat#1133 part 4)
executes each dialect arm on a real server of that dialect and asks whether it is accepted. That
finds the SQL Server half the moment the arm is listed — and it is structurally blind to the MySQL
half, where both statements are accepted and the defect is which runs first. **A check on one
statement cannot see a defect that lives between two.** The new test asserts the resulting
`ref_count`, and additionally that the content whose last reference expired is *collected* — so
phase 3 being reachable is part of what is pinned.

**The regression test's first falsification was void, and the cause is worth the line.** It went
red on all three dialects with `duplicate key value violates unique constraint` — leftover fixture
rows, because the cleanup was registered with `t.Cleanup` while the pool is closed by
`defer be.Cleanup()`. **`t.Cleanup` runs after the function's defers**, so every delete ran against
a closed database, failed, and was discarded by a `_`. Two discoveries in one: the fixture leak,
and that a cleanup whose errors are dropped cannot report its own failure. Now a `defer`, and the
errors are checked. The rows it had already leaked were purged and counted — 1 index row and 1
content row per dialect for the first content, 2 and 1 for the third, and **zero for the second on
every dialect**, which is independent confirmation that the fix collected it.

**And it passed locally on three dialects while failing CI on two, which is the sharper half.**
`NewPluginTestBackends` opens a connection and applies *nothing*; the schema a test gets is
whatever `plugin.RunMigrations` builds plus whatever the database already had. `cleanupExpired`'s
phase 1 joins `workflow_instances` — an **engine** table, because a plugin runs inside the
engine's database — which no plugin migration creates. My local MySQL and SQL Server had it from
earlier engine-suite runs. CI's are fresh, and both failed at phase 1:

    Error 1146 (42S02): Table 'cleat.workflow_instances' doesn't exist
    mssql: Invalid object name 'workflow_instances'.

PostgreSQL passed in both places because `TestDB` applies the schema and `MySQLTestDB` /
`MSSQLTestDB` do not — an asymmetry invisible from the call site, which reads as one helper
returning three equivalent backends. Fixed with `testutil.SetupMinimalSchema`, and **verified by
reproducing the CI condition rather than by reasoning about it**: a fresh `cleat_bp` database on
each of the three servers, where the pre-fix test fails on exactly mysql and mssql with exactly
those two messages, and the fixed one passes twice in a row.

**A fixture that depends on what an earlier test left behind is not a fixture**, and a local run
cannot tell you it is doing that — the residue is invisible and it always helps.

### 3.426 A failed child was reported to its parent as a success with an empty result — ✅ **FIXED 2026-09-11** (cleat#1115)

A parent that spawned a child, awaited it, and branched on the error took the **success** branch
when the child failed. The natural guest shape is silently wrong:

```go
result, err := h.AwaitChild(childID)
if err != nil { /* never reached */ }
```

and an empty result is a plausible success value, so nothing downstream looks wrong either. **The
parent was not denied the reason — it was told the opposite.**

**The information never left the store.** `finalize_workflow_status` writes a failed run's message
to `error_msg` (migration 053 routes the payload there on the `'failed'` branch);
`GetChildResult` selected `COALESCE(result, '{}')` and `status`, tested
`status == "done" || status == "failed"` as one condition, and returned
`(resultJSON string, completed bool, err error)` — **a triple with nowhere to say "completed, and
failed."** `err` is a *store* error. A failed child arrived as `("{}", true, nil)`.

So this is one missing fact, not four bugs. Every caller was wrong the same way:

| call site | what a completed child got |
|---|---|
| `AwaitChild` | `packAwaitChildResult(written, 0)` — the success flag |
| `AwaitAnyChild` | `out.Result = result`, `out.Error` left empty |
| `AwaitAllChildren` | `childOutcome{RunID: rid, Result: result}` |
| `PollChild` | fell through to `completed` |

The fix returns a `ChildOutcome{Completed, Failed, Result, Error}` from one query, so the child's
status and its `error_msg` reach all four.

**`PollChild` had a guess in place of the missing fact, and the guess was wrong in the other
direction.** Its last branch read `pollResult{Status: "failed", Error: "child workflow failed
(empty result)"}` — so a child that *succeeded and returned nothing* was reported as failed. That
guess is removed rather than kept beside an answer it can contradict.

**Its test stated the defect as a specification**, which is the same shape as §3.424's tenant
comment: the mock in `TestPollChild_EmptyResult` carried the comment *"completed but empty result
== failed"*. That is not a fact about a child; it is a description of the guess. Corrected, with
`TestPollChild_ChildFailed` added for the case the guess stood in for.

**The in-memory fake was MORE correct than every real store, and that is why nothing caught it.**
`wasmtest.InMemoryChildWorkflowStore.GetChildResult` reported a registered child error as
`("", true, fmt.Errorf(msg))` — the child's failure smuggled through the **store error** return.
`AwaitChild`'s `err != nil` branch then did the right thing. So every test driven by the fake saw
correct behaviour, produced by a mechanism the production path does not have. A fake that is right
for the wrong reason cannot fail with the thing it stands in for. It now returns a failed outcome,
and `SetError` — public API that nothing exercised — has a test.

**Cancellation needs no third case, checked rather than assumed:** it is an **error code**, not a
status (`WorkflowFilter.ErrorCode` says so, and no `status = 'cancelled'` exists in any dialect's
migrations), so a cancelled child is a failed one.

**Three fixture faults, all found by running rather than reading, and each void in a different
way:**

  * **Seeding through the wrong path.** The first version failed the child with
    `FinalizeWorkflowSegment(..., "failed", marker)`. That path runs its payload through
    `coerceResultJSON`, which replaces anything that is not valid JSON with `{}` — so the test read
    `Err="{}"` and would have measured the coercion rather than the fix. The worker fails a run
    through `FailWorkflow`, whose `errorMsg` reaches `error_msg` as written.
  * **An order dependence that reads as a dialect problem.** `claimSpecific` loops on
    `ClaimWorkflow` until the id matches, and every claim it discards *consumes* a workflow — so
    claiming one child left the other claimed and unclaimable. It passed on PostgreSQL and MySQL,
    where the order happened to suit, and failed on SQL Server. Both children are now claimed in
    one pass.
  * **A mechanical rewrite that hit a function it was not aimed at.** Updating ~17 mock
    implementations with a regex on `return X, true, nil` also rewrote `StartNewRun`, which happens
    to share the shape. Found by auditing every changed line for its **enclosing function** rather
    than by trusting the pattern — the compiler caught two of the three, and the third was in a
    branch the compiler accepted.

**Both halves are falsified separately**, because either alone leaves the defect: making the store
stop reporting `Failed` fails PostgreSQL only; making the session stop acting on it fails all three.
### 3.427 A duplicate idempotency key with a different payload silently discarded the second request — ✅ **FIXED 2026-09-11** (cleat#1170)

A second request presenting a key that was already held got `200`, `already_started`, and the
first run's id. **Its own input was dropped without a word**, and the run behind that id carries
somebody else's arguments:

    call 1   Idempotency-Key: K   {"input":{"n":7}}     -> 201 {"id":"28e97a21-…"}
    call 2   Idempotency-Key: K   {"input":{"n":999}}   -> 200 {"already_started":"true", …}
    the run's stored input: {"n": 7}

**This is quieter than the duplicate execution idempotency keys exist to prevent.** A duplicate
execution at least leaves a row behind. A discarded request leaves nothing anywhere: the caller
believes its request ran, and the only trace is a run it did not start.

**The mechanism already existed one column over.** Migration 051 (cleat#1047) added `def_name` so
a key reused for a *different workflow definition* is refused — cleat already fingerprints part of
the request and compares it on replay. It simply stopped at the name. `input_digest` is the same
migration, the same NULL-means-unknown rule, and the same refusal.

**Not backfilled, and that is the one place this departs from 051.** 051 could derive `def_name`
from the owning workflow because a name is a name. A digest is the output of a *specific function*,
so computing it in SQL would be **a second derivation of the same value** — free to disagree with
the Go one over JSON text rendering (PostgreSQL's `jsonb` output inserts a space after every `:`
and `,`; MySQL and SQL Server do not) and to do so silently for an entire upgrade. Existing rows
keep NULL, which reads as "unknown, allow" and degrades to the old behaviour rather than to a
refusal a caller cannot act on.

**The digest is a property of the VALUE, not of the bytes.** `IdempotencyInputDigest` canonicalises
through `json.Unmarshal`/`json.Marshal` — which sorts map keys — before hashing, so reordered keys
and added whitespace are the same request. A byte-wise digest would refuse a caller that merely
reformatted its JSON, and **that refusal would look exactly like the feature working**, which is
why the test carries it as a control rather than trusting the reasoning.

**What the digest cannot see, stated rather than hidden.** `server.go` calls `engine.Redact` before
the store sees the input, so a sensitive field arrives as `"[REDACTED]"` and two requests differing
*only* in a secret digest equal and replay. Closing that means digesting before redaction, which
means computing it in the handler and passing it down — a ninth parameter on a function that takes
eight. Raised on the issue as a decision rather than taken quietly.

**The refusal now returns 409, and so does the one that already existed.** `handleStartWorkflow`
mapped *every* `StartNewRun` error to `500`, including a deliberate refusal — cleat#832's shape, a
client error reported as a server fault, which sends an operator to look at cleat for a request
cleat handled exactly right. Both cases carry a `detail` so a client can branch without parsing
prose. **Fixing only the new one would have standardised the old one on 500 by omission**, at the
moment the code path gained a second caller.

**Executed on three databases, where the existing check could only be asserted about source.**
`idempotency_def_scope_test.go` reads the three stores' text, for a reason that is sound — the
behaviour is one comparison written out once per dialect, and what breaks is one store being edited
and the others not. That is second best, and it is no longer necessary:
`TestAKeyReusedForAnotherDefinitionIsRefused` runs the statements, and additionally pins the two
refusals as **distinguishable by `errors.Is`**, which a single shared error would have satisfied
every other way.

**The source guard it replaces was generalised rather than deleted**, and it earned that: its
patterns named the exact column list, so adding one column reported *"def_name is missing"* when
def_name was right there. It now asserts that every lookup reads **both** discriminators and that
the INSERT writes both — matched loosely, so the next column does not produce a false red.
Known-positives, run in an isolated worktree so the suite in flight was untouched: dropping
`input_digest` from one of MySQL's two lookups reports *"2 idempotency lookup(s); 2 read def_name
and 1 read input_digest"*, and dropping it from SQL Server's INSERT reports *"does not WRITE
input_digest"*.

**The scheduler is exempted, and finding out why is the part worth keeping.** Its key is
`cron:<tenant>:<schedule>:<scheduled instant>` by design, so two workers racing one firing derive
the same key — and if the schedule's input was edited between their reads of the row, they present
different payloads for the same firing. A refusal there is not a caller's mistake, and treating it
as an error would be **worse than useless**: the existing branch leaves the schedule due, the retry
reads the *new* input, and it mismatches the stored digest again for as long as the key lives —
**30 days**. A schedule edited at the wrong moment would wedge. The scheduler now reads the refusal
as the suppression it is. `runID` stays empty there, which `ClaimDueSchedule` already models:
`last_run_id = CASE WHEN $5 = '' THEN last_run_id ELSE $5 END`.

**A refusal is only correct where the caller can act on it**, and that is the line between the two
callers of `StartNewRun`, not a property of the check.

Falsified per layer: removing the PostgreSQL check fails postgres alone, returning
`existed=true, err=<nil>` — the reported symptom exactly; removing the 409 mapping fails the HTTP
test with `500`. The HTTP test carries its own control, because "a refusal returns 409" is
otherwise satisfied by returning 409 for everything, which reports a genuine server fault as the
caller's fault.

### 3.428 The workflow-memory tables were tenant-scoped on PostgreSQL with nothing behind the predicate — ✅ **FIXED 2026-09-11** (cleat#1098)

§3.x/cleat#1096 gave `workflow_memory_stats` and `workflow_memory_samples` a `tenant_id`, scoped
all twelve statements, and bound both to `dbo.fn_tenant_filter` on SQL Server. **PostgreSQL got the
column and the Go predicate and no policy** — a tenant-scoped table carrying one layer on the one
dialect where a second was available.

| dialect | Go predicate | database backstop |
|---|---|---|
| PostgreSQL | yes | **no** ← this item |
| MySQL | yes | none exists |
| SQL Server | yes | `TenantFilter_MemoryStats` / `_MemorySamples` |

**It could not be done in #1096, and that is the whole shape of the work.** All four access sites
read outside an RLS transaction — `RecordWorkflowMemorySample` on `s.db.BeginTx`, the other three
straight onto `s.db.QueryContext`/`ExecContext`. `cleat.tenant_id` is set per **transaction** by
`setRLSOnTx`, so a fail-closed `tenant_id = cleat.assert_tenant_set()` policy raises the moment any
of them runs. The restructuring onto `beginTxWithRLS` is the change; the policy is what it buys.
`QueueDepth` sits eighty lines away in the same file already doing it.

`CleanupMemorySamples` needed more than a swapped opener: its def listing and its per-def `DELETE`s
ran on **two unrelated connections**. One per-transaction setting cannot cover two connections, so
they are now one transaction.

**A policy nothing exercises is indistinguishable from no policy, so the proof is a PAIR.** The
existing `TestTheMemoryProfileIsScopedToTenant` passed before this item — the Go predicate was
doing all the work — so its passing afterwards says nothing on its own. Measured, same mutation
(the Go predicate deleted from `LoadMemoryEstimates`) run twice:

| | `TestTheMemoryProfileIsScopedToTenant/postgres` |
|---|---|
| policy dropped (the pre-fix state) | **FAIL** — tenant A's estimate reads `9000000`, want `1000000` |
| policy present | **PASS** — the policy alone carried it |

`9000000` is tenant B's sample arriving in tenant A's EWMA. That pair is the property SQL Server
already had and PostgreSQL did not, and it is the one #1096 named as the reason a second layer
earns its keep.

`TestMemoryProfileRLS_LayerSeparation` pins both directions permanently, in the shape
`engine/rls_gap_concurrency_and_update_requests_test.go` established: the policy filtering a query
carrying **no tenant predicate at all** on a non-superuser connection, and the Go predicate
filtering over a superuser connection the policy cannot reach. Both halves falsified; dropping the
policy makes Layer 1 report both tables by name.

**The guard extended itself, which is the argument for deriving a list rather than writing one.**
`TestNoPostgresStatementReachesAnRLSTableWithoutTheTenantSet` (§3.x, cleat#1178) reads its table set
from `ENABLE ROW LEVEL SECURITY` in the migrations, so this migration brought both tables under it
with no edit to the guard. On the pre-fix tree it now names **all six** offending statements —
`db.go:945, 952, 969, 990, 1044, 1065` — each with the reason (`runs on a transaction from
s.db.BeginTx, and RecordWorkflowMemorySample never calls setRLSOnTx`). A hand-written table list
would have silently kept passing.

**Scope is two tables, deliberately.** 031's header reasons about each table it declined —
`idempotency_keys` is read before any RLS context exists, `admin.tenant_api_keys` before a tenant is
known, `kv_store` and `feature_flags` are plugin-owned — and none of those reasons has changed. A
blanket apply would make the migration a claim rather than a check.
(`idempotency_keys` stopped being one of them on 2026-09-15: cleat#1534 reordered `startNewRun` onto
RLS transactions and migration 083 gave the table its policy. The other three stand.) cleat#1097, the in-memory gauge,
is a metric-labelling decision and stays where it is.

### 3.429 `cleatctl deploy plugin` wrote to a table that has never existed — ✅ **FIXED 2026-09-11** (cleat#1226)

Three statements against **`plugin_registry`**, a name no migration has ever created, so the command
could not work at all. It was not a rename: `plugin_defs` is keyed `(name, version)` and has
`config` rather than `metadata`, no `id`, and no `updated_at`, so every assumption about the shape
was wrong too — including the one that mattered, **one row per name**.

A correct writer already existed (`engine.PluginLoader.DeployPlugin`, upserting on `(name, version)`),
so the work was an argument surface and a decision, not a deployment path.

**The decision: require the version.** `cleatctl deploy plugin <name> <version> <wasm-file>`.

It looks like taste and is not, because the column already has a consumer that **parses** it.
`ResolvePlugin` compares versions as semver and *silently skips* a row it cannot parse
(`if !semver.IsValid(v) { continue }`), so an invented default risks a plugin that is in the table,
listed by `cleat plugin list`, and resolvable by nothing. Measured against the same
`ensureVPrefix` + `golang.org/x/mod/semver`:

| candidate | valid | verdict |
|---|---|---|
| `1.0.0` — what `cleat plugin install` already writes | yes | the existing convention |
| `a3f9c2b1` — a content hash | **no** | deployed and permanently unresolvable |
| `1` | yes | but `semver.Compare("v1","v1.0.0") == 0`: a **distinct primary key** that is the **same version** to the resolver |
| auto-increment | yes | `plugin_cmd.go:416` picks the latest with `ORDER BY version DESC`, a TEXT sort where `"9" > "10"` |

Requiring it is also the only option that adds no second convention: `cleat plugin uninstall
<name> <version>` already takes this shape.

**A non-semver version is refused at deploy time**, rather than accepted and skipped later. Taking
one would move this exact defect — a write that reports success and produces something nothing can
read — one step downstream.

**Why every existing test passed.** All five `deployPlugin` tests drive a fake `driver.Connector`
that returns a canned result for any query, so they accept SQL no database would — the same failure
mode `plugins/*_dialect_arms_multidb_test.go` was built for, and the reason
`TestEveryInlineStatementParsesOnPostgres` exists. They reported `Deployed plugin` for a statement
naming a table that does not exist. They are kept, because they cover argument handling, refusals
and output; what they cannot do is notice the write went nowhere.

So the new test asserts the **round trip** against a real PostgreSQL: deploy, then *resolve*. It
additionally pins the `(name, version)` model that the decision is about — a second version adds a
row rather than replacing, both remain resolvable, and redeploying a version replaces its bytes
without adding a row. Falsified two ways: pointing the writer back at `plugin_registry` kills the
command at the deploy step, and writing under a different name lets the deploy *succeed* and the
resolve fail — the sharper one, because it isolates exactly the property the old code's intent
claimed and its effect did not have.

**The three pins in `TestEveryInlineStatementParsesOnPostgres` are deleted**, and that is a check
rather than bookkeeping: it fails in both directions, so a pin outliving its defect fails the run.

**Found on the way, filed as cleat#1243:** an exact-version plugin constraint matches nothing.
`parseConstraint` maps a bare or `=` version to `{Min: v, Max: v}` and `versionInRange` excludes the
upper bound, so `ResolvePlugin(name, "1.0.0")` cannot return 1.0.0. Nothing in the tree passes an
exact version — every internal caller uses `""` or a range — so the two broken forms are precisely
the ones a human reaches for first. This test uses `^1.0.0` with a comment pointing at the issue,
rather than quietly avoiding the form that fails.

### 3.319 A release matched any row with the key, so one workflow freed another's lock — ✅ **FIXED 2026-09-11** (cleat#1188)

`ReleaseConcurrencyKey` took only the key. Its statement carried `AND tenant_id` and no
`workflow_id`, on all three dialects. Within one tenant, any workflow that knew a key string
removed the row whoever held it — so B released A's lock, C then acquired it, and A carried on
believing it held mutual exclusion. Nothing errored on any side.

Reproduced on a scratch database before the fix: `DELETE 1` against a row the caller did not own,
then a successful acquire by a third workflow, with the holder still `status='running'`.

#### The hazard was already written down, in this package, as a comment

`concurrency_key_reentrancy_test.go` explains why re-entrancy must keep returning false:

> ReleaseConcurrencyKey takes only the key and has no hold count, so acquire+acquire+release
> frees a lock the workflow still believes it holds.

That is this defect, described accurately, in the tree, before it was filed. It was load-bearing
for a *different* test's reasoning and was never turned into an assertion of its own. **A sentence
in a test cannot fail.** The new test can, and the hold-count half of that sentence remains true
and remains pinned where it was.

**It appears twice in that file, and the second occurrence is worse than the first.** The file-level
comment states it flatly — *"takes only the key and deletes the row unconditionally"*. The other is
a **string literal inside the failure message of a different assertion**, printed only in the branch
where re-entrancy misbehaves. So it is not merely an unasserted claim: it could not be *read* at all
unless an unrelated assertion broke first. Both are corrected in this change, because a comment
describing the pre-fix behaviour is worse than no comment — the rule this repo already applies to
`✅` markers over stale bodies.

**The obvious mechanical guard for this class does not work, and that is worth recording so nobody
builds it.** The tempting predicate is *"a test's failure message names a production identifier the
test never calls"*. It would not have caught this one: `concurrency_key_reentrancy_test.go:83` calls
`ReleaseConcurrencyKey` as cleanup, so the identifier *is* called — it is just never the subject.
The real predicate is *"this sentence states a property, and no assertion anywhere depends on that
property holding"*, and neither of us has a mechanical form for it. Left as a stated open question
rather than a weak guard: **a check that would not have caught the case that inspired it is worse
than none, because it makes the class look handled.**

#### The fourth route into one end state

| | |
|---|---|
| §3.34 | a TTL truncated to whole seconds; the key was born expired — *"two workflows holding the same mutual-exclusion key, with nothing logged"* |
| §3.39 | re-acquiring a key you already hold answered differently per dialect |
| §3.318 (cleat#1189) | the key namespace was global across tenants |
| this | a release matched a row it did not own |

Each of the first three was fixed where it surfaced. What none of them asserted is the property
itself, which is why the regression test here checks the *consequence* — C acquires — and not only
the symptom. Asserting `released == false` alone would pass against a store that reported false and
deleted the row anyway.

#### Releasing a key you do not hold is still a success, deliberately

`TestPostgresStore_ReleaseConcurrencyKey_NonExistent` has asserted that contract since before this
change, and the contract is right rather than merely established: a key whose TTL has passed is
already gone, the workflow releasing it has done nothing wrong, and an error there is one the guest
cannot act on. The visible-error alternative was proposed and declined for that reason.

What was missing was any *trace*. A release that freed a lock and one that matched nothing were the
same event. `EventRecord.LockNotHeld` now distinguishes them, and `eventRecordToPayload` emits
`lock_not_held` **only when true** — `computeEventChecksum` runs over that map, so an unconditional
key would have rewritten the checksum of every release event in every existing history.

#### Falsification

Each dialect's predicate reverted alone, to the exact pre-fix statement:

| reverted | result |
|---|---|
| postgres | `postgres` FAIL, `mysql` and `mssql` PASS |
| mysql + mssql | both FAIL, `postgres` PASS |

and both failures name both assertions — *"B released a key held by A"* and *"C acquired a key A
still holds"*. **The first attempt at the postgres mutation was not faithful**: it deleted `$2` from
the SQL while still passing three arguments, so the test went red on
`pq: could not determine data type of parameter $2 (42P18)` — a red for the wrong reason, which is
the outcome the "read *why* it failed" rule exists to catch. The mutation was rewritten to restore
the pre-fix statement exactly.

---

### 3.320 A crash mid-retry granted a fresh MaxAttempts, because nothing was recorded until the call finished — ✅ **FIXED 2026-09-11** (cleat#1145)

The host retry loop recorded an event only when the call *finished*. A failed attempt persisted
nothing, so a worker lost mid-backoff replayed into a step with no history, restarted the policy at
attempt 1, and spent the caller's whole budget a second time. `MaxAttempts` bounded attempts **per
incarnation**, not per workflow, and a run that crashed repeatedly was bounded by nothing.

Measured on the port harness before the fix, with the control that makes the number readable:

| | attempts | crash | calls |
|---|---:|---|---:|
| control | 3 | no | **3** |
| probe | 3 | mid-backoff | **4** |

The unit test reproduces that exact pair, and its falsification message prints `Total across both
incarnations: 4`.

#### The fix is one event, and the constraint that shaped it is not in the issue

`EventTypeCallAttemptFailed` is recorded before each backoff — only when another attempt follows, so
a history ends on one **only** when the run was interrupted. That is the signal replay reads.

The issue proposed "record the failed attempt" and stopped there. Three properties of this engine
decide what that can mean, and none is obvious from the call site:

* **`recordEvent` advances `stepCount`, and replay is positional** (`s.history[s.stepCount]`, then
  `advanceReplayStep` increments). One event per step. A new event therefore shifts every
  subsequent step in *new* histories — harmless, because old histories are replayed positionally
  against themselves and never compared across versions, but it rules out any "extra event on the
  same step" design.
* **The compaction codec is a pair of exhaustive maps**, so a new type needs a code in both or it
  round-trips as unknown.
* **Attempts deliberately share one step** so every attempt carries one idempotency key. Resuming
  therefore has to carry the *first* incarnation's step, or the key changes at exactly the moment a
  duplicate is most likely. `freshCallWithRetry` takes `resumeStep` for that reason.

#### Three guards caught what the tests did not

The suite passed and then three existing guards failed — each about the *completeness* of adding an
event type, which is precisely what a behavioural test cannot see:

| guard | what it caught |
|---|---|
| `TestTheCarrierAuditCoversEveryEventTypeConstant` | the new type was in `eventTypeToCode` but in no audit list, so no field was checked against its payload arm |
| `TestEveryEventRecordFieldTheDatabasePayloadMustCarryDoesCarry` | `Attempt` was carried unconditionally rather than guarded on non-zero |
| `TestTheRequiredJavaGuardsCoverEveryHostStopSite` | the resume path adds an 18th `stopBeforeNewWork()` site |

The third is the interesting one, and it is a case its own comment did not anticipate: the new site
is a **second site for a call the Java SDK already guards**. Java needs no new method, because
`javaCallsTheHostCanRefuse` covers the *call*, not the *site*. A count of sites and a list of
methods are different things, and this is the case that separates them — recorded at the constant.

#### Falsification

Passing `0` instead of the spent count reddens both tests: *"a crash after attempt 1 of a 3-attempt
policy made 3 further calls, want 2 — total across both incarnations: 4"*, which is the field
measurement reproduced in a unit test.

**The first attempt at that mutation did not compile** — removing the value left `spent` unused, and
the filtered output printed nothing, which reads exactly like a pass. Caught by reading the
unfiltered tail. The mutation was rewritten to keep the tree compiling (`_ = spent`), which is the
repair CLAUDE.md prescribes for exactly this.

#### What this does not change

The **wait** is still worker-local (§3.317, cleat#1111): a resumed policy fires immediately rather
than re-waiting the remainder of its backoff. That was a decision and it stands. This changes only
which attempt it resumes at.

---

### 3.431 `RunDetached` discarded the run id it already computed — ✅ fixed

**cleat#1154.** A workflow that starts a detached run got back nothing but a status, so it had no
handle to what it started: it could not poll it, signal it, or record the id anywhere durable. The
id was not missing — `engine/children.go` computes it from `StartChildWorkflow` and throws it away.

#### Why a new host call rather than a wider one

`cleat_run_detached` is **unchanged**, and both calls stay registered. A host call's arity is part
of its import type, and a mismatch is a **hard link error** that stops a module instantiating at
all — not a failure of the one call. §3.55 measured that here, through the production path:

    incompatible import type for `env::cleat_create_promise`
    types incompatible: expected type `(func (param i32 i32 i32 i32) (result i64))`,
                           found type `(func (param i32 i32 i32 i32 i64) (result i64))`

Every deployed binary imports the four-parameter form, including in-flight runs pinned to an older
version that `tests/upgrade` exists to protect. So the capability arrives as `cleat_start_detached`,
which is how `cleat_poll_update` and `cleat_complete_update` arrived in #868.

`RunDetached` and `StartDetached` share one body (`runDetached`) rather than being two
implementations, because a detached run's **replay** behaviour has to be identical whichever call
the guest used: a workflow with `run_detached` events already in its history, recompiled to call the
new one, must replay against that history. Both record and match `EventTypeRunDetached`, and on
replay the id comes from the record — starting it again would be a second run.

#### The test, and why "non-empty" would have proved nothing

`TestStartDetachedReturnsTheIDThatAddressesTheRun` does the round trip the issue asked for: it takes
the returned id back to the store and asserts the run behind it is the run that was started, on all
three dialects.

That is not fussiness. `children.go` mints a fallback id — `fmt.Sprintf("detached-%s-%d", name,
s.stepCount)` — whenever the store call produces nothing, and that string is non-empty,
well-formed, and **addresses nothing at all**. Falsified two ways, both red on all three dialects:

| mutation | what the test said |
|---|---|
| stop writing the id (`wantID=false`) | *"wrote 0 bytes, so it reported success and handed back nothing"* |
| skip the store, take the fallback | *"returned `detached-detached-reconcile-0`, which is the fallback id"* |

A test asserting only that a non-empty string came back passes the second mutation.

#### Three guards fired, and one had been covering a real defect

| guard | what it caught |
|---|---|
| `check-doc-consistency.sh` | the export was registered and undocumented — red until `ABI.md` §2.24a |
| `TestEverySDKReachesEveryHostExport` | named all four SDKs that could not reach the new call |
| `TestTheThreeStopSurfacesAgree` | the stop site moved to the shared body, **and** the entry covering it was stale |

The third is the one worth reading. `stopSurfaces["RunDetached"]` carried
`adapterWhy: reasonNoGoAdapter` and `witWhy: reasonNotInTheComponentWorld`, and **both exemptions
were true when written and false by now**: #806 gave Go a real signature and a `wasm/usage.go` row,
and §3.253 added `durable-run-detached` to `cleat.wit` and wired the Python method. Neither change
removed the exemption, and nothing failed — because an exemption is only ever consulted when
something is missing.

What it was covering was a live defect: the `RunDetached` Go adapter did **not** call
`withSuspendCheck`, so a guest refused mid-segment read bit 31 as `errCode = 0x80000000` and got
`cleat_run_detached: error 2147483648` instead of `ErrSuspend` — an ordinary error a workflow may
well swallow, in the defer segment that refusal exists to protect. Fixed here, with the entry
re-keyed to the shared body and both adapters named.

#### Python is the one gap, and it is recorded rather than papered over

Bound in Go, Rust, Java and AssemblyScript. **Not Python**, and the reason is structural rather than
effort: `cleat_start_detached` returns a string, so its WIT cannot be the `-> u64` that
`durable-run-detached` uses. A core-ABI guest receives the id through an out-pointer into its own
linear memory; component dispatch writes into a **host** buffer, so out-pointers do not survive the
crossing — the same defect `stopSurfaces` already records as OPEN for `durable-await-signals`, which
is declared with out-pointers and has therefore never worked on a component.

Declaring the `u64` form anyway would compile and be **worse than nothing**: the guest would read
whatever happened to sit at `OUTPUT_OFFSET` and return it as a run id. So the entry is in
`sdkUnreachedBaseline`, which is shrink-only, with that reasoning written at it.

#### Stale prose corrected in passing

Three comments described the old world and would have misled the next reader:

- `engine/run_detached_stop_test.go` said *"Go guests cannot reach `cleat_run_detached` at all"*.
  Measured false by generating the imports for `testdata/allhostcalls`: both detached calls are
  emitted, with `cleat_fetch` as the negative control, which is not — matching its own baseline
  entry.
- `crates/cleat-sdk/src/host_calls.rs` documented Go's signature as `RunDetached(fn func(h
  HostCalls) error)`, which #806 replaced.
- `tests/plugin-harness/sdk_import_names_test.go` opened the Python baseline with a **count** of its
  entries; three of them were bound within days and the sentence has been wrong ever since. Replaced
  with a pointer to the list, per CLAUDE.md's rule about censuses of growing populations.

---

### 3.432 A `413` that named neither the limit nor which knob moves it — ✅ fixed

**cleat#1332.** This server enforces **two** body ceilings — the configurable `--max-body-size` and
the compile-time `signalMaxBodySize` — and all eight `413` sites in `cmd/cleat-worker/server.go`
said only `"request body too large"`. A caller could not tell which one had refused them, what its
value was, or whether anything they control would change it.

**The expensive case is not the missing number.** It is an operator who raises `--max-body-size`,
still gets `413` from `/signal`, and has nothing in the response to suggest that endpoint does not
use the flag. `signalMaxBodySize` is a `const`; the flag does not move it.

#### The doc named two of the three endpoints

`docs/reference/worker-config.md` read *"Signal endpoints have a fixed 64 KB limit."* The constant
guards **three** handlers — `handleSignal`, `handleCancel`, `handleWorkflowUpdate` — and the
constant's own comment made the same omission, saying "signal and update endpoints". Cancel was
missing from both while being the one most likely to be reached in practice: its field is a
free-text `reason`.

#### Eight sites, not the three the issue described

The issue named three. There are eight, and five guard the *other* limit:

| limit | sites |
|---|---|
| `signalMaxBodySize` (64 KB, const) | `handleSignal`, `handleCancel`, `handleWorkflowUpdate` |
| `s.maxBodySize` (configurable) | `handleStartWorkflow`, `handleSetAllowedSignals`, `handleResolvePromise`, `handleRejectPromise`, `handleCreateSchedule` |

Fixing only three would leave the other five silent while their neighbours name a limit, which is
worse than uniform silence — a caller who learns the body carries the limit would reasonably read
its absence as meaning something.

#### Two helpers rather than one with a description parameter

`bodyTooLargeConfigured` and `bodyTooLargeFixed`. One function taking a sentence would let the
choice of limit and the sentence describing it drift apart at a call site, and **naming the wrong
knob is worse than naming none**: a caller who learns the response identifies the knob will act on
it.

#### The test asserts the PAIRING, which is what makes it falsifiable

Checking that *a* limit appears passes against a handler naming the wrong one. So each case pins
the value **and** requires the other limit's knob to be absent. Falsified three ways, each red for
its own reason:

| mutation | what failed |
|---|---|
| swap the helper at the `cancel` site | all three assertions — wrong value, missing knob, *and* "contains the OTHER limit's knob" |
| revert one site to the bare string | the completeness guard, at both its bare-string count and its per-helper floor |
| shrink the general limit to 1 byte | the **control** — "an 53-byte body was refused with 413, so the oversized assertion below would prove nothing" |

The control runs first in every case. Without it, "an oversized body is 413" passes equally against
a handler that answers 413 to everything, which is exactly what a misconfigured `MaxBytesReader`
produces.

#### What this deliberately does not decide

Whether `signalMaxBodySize` should track `--max-body-size` is a product call. The doc fix makes the
asymmetry **visible**; a code change would paper over the question instead of putting it to whoever
owns it.

#### Not fixed here, and filed separately

**Seven `MaxBytesReader` sites have no `MaxBytesError` branch at all** — three in `server.go`
(`handleSetRoutingRule`, `handleSetWorkflowTag`, `handleCreateDefinition`) and four in
`api_admin.go`. An oversized body there is a **400** whose text begins `"invalid JSON"`, for the
same condition that is a 413 everywhere else. `handleCreateDefinition` is the WASM upload endpoint,
where an oversized body is the most legitimate 413 in the API, and its ceiling is a *fourth*
distinct value — `10*1024*1024` written inline. That is a status-code change on live endpoints and
deserves its own decision.

The completeness guard here is written to require a limit in every site that **does** return 413,
not to require a 413 at every `MaxBytesReader` — so it does not fail for the reason it is not about.
### 3.433 Dead-letter terminate discarded its decode error, erasing the failure it was recording — ✅ fixed

**cleat#1337.** `handleDeadLetterTerminate` called `json.NewDecoder(r.Body).Decode(&req)` and threw
the result away. A body it could not read left `req.Reason` empty, the terminate proceeded, and the
caller got `200 {"status":"terminated"}`.

#### The damage is not the missing note

`TerminateWorkflow` writes `reason` into `workflow_instances.error_msg` **unconditionally, in both
of its branches** — the defer-phase transition and the direct one. So an empty reason is an
**overwrite**, not a no-op, and what it overwrites is the message recording why the run
dead-lettered. Measured on one row by the session that filed the issue:

| | |
|---|---|
| before | `dead_lettered`, `error_msg` 270 bytes — `"host: workflow a0493c8a-…: execution failed: …"` |
| request | a **25-byte** truncated-JSON body → `200 {"status":"terminated"}` |
| after | `terminated`, `error_msg` **0 bytes** |

with a short-reason control landing in the column intact, so the zero means something.

**Twenty-five bytes, against a 1 KB cap.** The issue was first framed as "over 1 KB or malformed",
which reads as two symptoms of one limit and invites a fix that raises or documents the cap. Any
decode failure does this; the cap is one way in, not the defect.

#### It was the only one

    grep -nE '^\s*json\.NewDecoder\([^)]*\)\.Decode\(' cmd/cleat-worker/*.go

One hit. Every other request-body decode in the worker assigns the error and branches, usually to
`400 "invalid JSON: …"`. A lone deviation from the file's own pattern, which is why the fix is to
match the pattern rather than invent one.

#### The naive fix is a regression, and that is the interesting part

Branching on *every* error breaks a supported call: **a terminate with no body at all**.
`TestHandleDeadLetterTerminate_Success` has posted a nil body and asserted `200` since before this
handler had any error handling.

`http.NoBody` decodes to exactly `io.EOF`; a truncated body decodes to `io.ErrUnexpectedEOF`, which
`errors.Is(err, io.EOF)` does **not** match. Measured before relying on it, because the whole fix
turns on those two being distinguishable:

| body | error | `errors.Is(err, io.EOF)` |
|---|---|---|
| `""` | `EOF` | **true** |
| `"   "` | `EOF` | **true** |
| `{"reason": "trunc` | `unexpected EOF` | **false** |
| `{"reason":"r"}` | `nil` | — |

So the carve-out is `err != nil && !errors.Is(err, io.EOF)`, and it is load-bearing: replacing
`io.EOF` with an unrelated sentinel turns both the new empty-body control **and** the pre-existing
`TestHandleDeadLetterTerminate_Success` red.

#### The test asserts the store is not reached, not that the status is non-200

A handler that returned `400` *after* calling `TerminateWorkflow` would satisfy a status-only check
and destroy the column just the same. Falsified by reinstating the original defect — and the first
mutation attempt did not compile (`"io" imported and not used`, `undefined: err`), which is the
trap CLAUDE.md names: a filtered read of that output looks like a pass. Rewritten to keep the tree
compiling, all four bad-body cases go red with `status = 200 "terminated"` and *"TerminateWorkflow
was called with reason ""*.

Both controls stay green under that mutation, correctly — they are what stops "never reaches the
store" being satisfied by a handler that reaches it never.

#### 400 rather than 413, deliberately

Whether an oversized body should be a `413` here is [§3.432](#3432)'s neighbour, cleat#1338, which
covers **seven** other sites with this exact shape. Answering it for one endpoint would make that
decision twice, and the second time by accident. If #1338 lands as 413, this site gets it with the
others.

---

### 3.434 A generic function was classified as a workflow entry point, and one of them shipped — ✅ fixed

**cleat#1313.** `IsEntryPoint` was purely structural: exported, not a method, first parameter
`cleat.HostCalls`. A **generic** function matching that shape was classified as an entry point, so
`testdata/generics` — the repository's own generics fixture — reported **3 entry points where it
intends 1** and did not survive `cleat build`.

#### Two functions, caught two different ways, and only one was caught at all

| function | signature | what happened |
|---|---|---|
| `Process[T]` | `(h, item T) (T, error)` | `verifyEntryPointResults` rejected it — the **right refusal for the wrong reason**: the problem is that it is not an entry point, not that `T` is not a `string` |
| `GenericLeaf[T]` | `(h, items []T) error` | returns `error` alone, so the result check cannot see it — classified as an entry point and **exported**, silently |

The second is why the fix belongs in `IsEntryPoint` rather than in the result verifier. An entry
point is exported with a concrete signature — `wasm/exports.go` declares `var __r string` and emits
`return []byte(__r)` — so there is nothing to instantiate `T` with.

Both fixture functions say what they are in their own doc comments: *"Process is a generic workflow
helper … in the durable closure (called by EntryPoint)"* and *"GenericLeaf demonstrates a generic
durable leaf"*.

After: **1 entry point**, threading OK, `entry_point.wasm` (3.1 MB) written.

#### Why five tests referencing the fixture all passed

None ran the stage that fails. `wasm/generics_build_test.go` calls `BuildOutputs` directly, which
skips `VerifyThreading`; the other four stop earlier. And no CI job runs `cleat build` on a **Go**
example — `git grep 'cleat build' .github/workflows/` finds only `--target rust` and
`--target python`.

**The gap was already documented at the site.** `internal/closure/threading.go` says *"nothing in CI
runs `cleat build` on a Go example"*, and saying so changed nothing for weeks. A comment cannot go
red. That is the argument for the guard below over a better comment.

#### The guard is a table of expected outcomes, not "they must all build"

`testdata/errors` exists to be **rejected** — its package comment says it *"contains deliberately
invalid workflow code to test the transformer's validation rules"*. A blanket must-build rule could
only accommodate it by skipping it, which is how a fixture stops being checked.

Kept as an entry with an expected *reason*, it becomes the guard's **known-positive**: the one case
proving the test can report a failure at all. A version asserting only success passes equally
against a checker that has stopped checking — the defect this issue is about, one level up.

The population is **discovered** from `testdata/` rather than listed twice, so a new fixture that is
not in the table fails rather than being silently uncovered, and an entry naming a fixture that no
longer exists fails too.

#### The first draft of the guard was wrong, and a fixture caught it

It modelled "does this build" as "`VerifyThreading` returned no errors", and `testdata/autothread`
went red. `VerifyThreading` reports the **pre-transform** state deliberately, and `cmd/cleat` calls
`dropAutoThreaded` before deciding — failing on those once made `cleat build` reject packages the
next stage was designed to repair ([§3.229](#3229)). The predicate is the CLI's whole decision, not
its first half.

#### Falsification

| mutation | what failed |
|---|---|
| revert the generics exclusion | both guards — `Process is a workflow entry point returning T` |
| wrong expected reason on `errors` | *"rejected, but not for the expected reason"* |
| drop `spin` from the table | *"testdata/spin holds Go files and is not in goFixtureExpectations"* |

**The third mutation was a no-op on its first attempt** — a `sed` with hardcoded whitespace that
`gofmt` had realigned, so the file was unchanged and the test passed. That reads exactly like a
guard that does not work. Re-run with an assertion that the edit applied, it fails as intended. A
mutation that does not apply is not a falsification, and it fails in the flattering direction.

#### Two measurement errors worth recording, both caught by controls

- `cleat build … | tail` reported **exit 0**; `$?` after a pipeline is the last command's. Redirected,
  it is 1 for generics and 0 for basic.
- A sweep over `testdata/*/` reported **21 of 21 failing**, because the loop omitted the `./` prefix
  and every invocation died as a bad package pattern rather than a build failure. Only a
  known-good control from ninety seconds earlier caught it.

#### Out of scope

Replacing signature-based detection with an explicit marker. The issue raises it for the quieter
half — *any* helper sharing the shape becomes a deployable entry point, generic or not — and that is
an API decision for whoever owns the authoring surface. Excluding generics is provably safe because
a generic function cannot have a concrete `string` result; a marker changes how every workflow is
written.

---

### 3.435 `--size-report` multiplied the file's length by hardcoded constants and printed the products as measurements — ✅ fixed

**cleat#1314.** `cleat build --size-report` is documented as *"output WASM binary size breakdown by
package"*. It did not read the binary. Every line was `totalSize × a literal`:

```go
{"runtime", int64(float64(totalSize) * 0.15)},
knownContributions := map[string]float64{"reflect": 0.25, "encoding/json": 0.12, ...}
```

The only input from the artifact was its length, so every binary ever built got the same answer in
different absolute numbers — and the **Recommendations** block quoted the same literals back as
findings: *"Remove \"reflect\" import: reduces binary ~25%"*.

#### How wrong the headline number was

Measured on two real artifacts with the new implementation:

| | `testdata/basic` | `testdata/minimal-wf` |
|---|---|---|
| `reflect`, as the old report claimed | 25% | 25% |
| `reflect`, measured | **4.9%** | **1.9%** |
| `runtime` claimed / measured | 15% / **24.0%** | 15% / **35.6%** |
| `encoding_json_v2` | 9.1% | **absent entirely** |

An author acting on the old advice would do real work to reclaim a stated quarter of the binary and
recover a twentieth of it — with no way to tell, because the next run reports the same percentage of
a new total.

#### A fourth consequence the issue did not list: the percentages could exceed 100%

`accounted` starts at 0.20 and adds a constant per matching import. The constants sum to **1.43**,
and an ordinary import set — `reflect`, `encoding/json`, `fmt`, `net/http`, `crypto/tls`, `time`,
`os`, `strings` — reaches **1.08**. The remainder line is guarded by `if unaccounted > 0`, so at that
point *"other (stdlib + deps)"* silently vanishes and the reader sees per-package rows summing to
108% of a binary, with nothing indicating anything is wrong.

#### Measuring it was feasible, and that was checked rather than assumed

| probe | result |
|---|---|
| `go tool nm <artifact>.wasm` | **fails** — `unrecognized object file` |
| Go symbol names present in the artifact | **3332**, incl. `runtime.mapaccess1`, `reflect.ArrayOf` |

The toolchain route is closed; the data is in the binary. `wasm.AnalyzeSize` parses the code
section for per-function body sizes and the custom `name` section for symbols, and joins them.
**99.1%** of the code section attributes to real packages on a live artifact.

#### The mangling is reported, not guessed at

The Go linker encodes `/`, `:`, `(`, `)` and `*` all as `_`, so `internal/abi.NoEscape` appears as
`internal_abi.NoEscape`. This does **not** invert it. `internal_runtime_math` is provably
`internal/runtime/math`, but the inverse is ambiguous in general, and a size report that silently
guesses at identifiers is the genre of defect being removed. The mangled form is printed and the
header says so.

Compiler-generated families — `type_.eq.[3]string`, `go_buildid`, `gcbits_*` — are counted as
**unattributed** rather than invented into a package named `type_`.

#### The honest fallback is the load-bearing part

A binary with no name section yields no attribution, and the report says so:

> Per-package breakdown unavailable: this binary carries no WASM name section, so its functions
> cannot be attributed to packages.

Falling back to the constants there would put the defect back in the one path nobody exercises.
`TestAStrippedBinaryReportsNoBreakdownRatherThanAModel` pins it, including that the bytes are still
reported as unattributed rather than dropped.

#### Falsification

The acceptance property is that **different inputs produce different outputs**, which the old
implementation could not satisfy at any input — and which a test asserting *"the report mentions
reflect"* would have passed against it unchanged.

| mutation | what failed |
|---|---|
| drop the import offset on function indices | exact byte counts in two tests, plus *"attributed 72 bytes to \"wrong\""* |

That mutation is the one this parser was most likely to get wrong, because function indices in the
name section count imports first and ignoring them shifts every attribution by a constant — producing
a plausible report rather than an error. The first attempt at it **did not compile** (`declared and
not used: importedFuncs`) and was rewritten to keep the tree building.

---

### 3.436 A backup config's name reached `pg_dump -f` unvalidated — ✅ fixed

**cleat#1305.** `plugins/scheduledbackup` checked a config's `name` only for non-emptiness, then
interpolated it into a dump filename joined to `DumpDir` and passed to `pg_dump -f`. An
**authenticated tenant** could direct a full **cross-tenant** database dump outside the dump
directory, as the worker's OS user. The plugin is compiled into the shipped worker.

#### The exploit is narrower than it first reads, and that shaped the fix

The originating review gave `"name": "../../../../var/spool/cron/crontabs/root"` as writing a dump
over the crontab. Run through the real expression, it does not:

| name | resolves to |
|---|---|
| `../../../../var/spool/cron/crontabs/root` | `/var/lib/var/spool/cron/crontabs/root_2026….dump` |
| `../../../../../../srv/www/html/leak` | `/srv/www/html/leak_2026….dump` |
| `/etc/passwd` | `/var/lib/cleat/dumps/manual_/etc/passwd_2026….dump` |

Three constraints: the `manual_` prefix eats one traversal level (`manual_..` is a literal directory
name); the `_<timestamp>.dump` suffix is always appended, so no exact filename can be landed on; and
a leading `/` does not escape, because `filepath.Join` treats it as relative.

What remains is still HIGH: **a complete database dump written into any directory the worker's user
can write, under an attacker-chosen prefix.** A web root, a shared volume, anywhere world-readable.
That is a data-exfiltration primitive and an unbounded disk fill, not a tidiness bug.

#### Two more doors than the issue named

| | |
|---|---|
| `routes.go:322` — the **UPDATE** route | took a new name with no validation at all. Validating only on create leaves the hole open through a rename. |
| `commands.go:94` — the **CLI command** | a third filename construction site, reading the name back out of `backup_config`. |

So three construction sites — `background.go:172` (cron), `commands.go:94`, `routes.go:514` — and
two of the three never pass through an HTTP handler.

#### That is why the fix is two guards, not one

**`ValidConfigName`** at both doors, matching the charset `engine/memory.go`'s `validServiceName`
already enforces. `.` and `..` are rejected **explicitly**: both are made entirely of allowed
characters, so a charset-only rule admits the exact payload — an allowlist that looks complete and
is not.

**`SafeDumpPath`** at the join, shared by all three sites. This is the half that covers **rows
already stored** with a traversing name, which input validation cannot reach and which the cron
sweep executes on a schedule with nobody watching. It compares against `base + separator`, so a
sibling directory sharing the prefix — `/var/lib/cleat/dumps-evil` against `/var/lib/cleat/dumps` —
is not accepted by a bare `HasPrefix`.

A refused backup now records `failed` through `markBackupFailed` rather than leaving a
`backup_history` row at `running` forever, which would read as a hung backup rather than a refused
one.

#### Falsification

| mutation | what failed |
|---|---|
| drop the explicit `.`/`..` rejection, leaving the charset | `ValidConfigName("..") = true, want false` |
| `HasPrefix(full, base)` without the separator | *"accepted a sibling directory sharing the dump directory's prefix"* |
| revert the cron path to a bare `filepath.Join` | *"background.go:192 joins a filename to the dump directory directly"* |

The third is the completeness guard: it reads the source, so a **fourth** call site added later
fails rather than shipping unchecked — which is how three sites came to exist with one check.

The control matters here more than usual: every assertion above is satisfied by a `SafeDumpPath`
that refuses everything, which would break backups rather than secure them. `nightly`, `prod-db`,
`tenant_42` and `v1.2.3-weekly` are asserted to pass, because a rename that starts failing for a
legitimate name is a worse outcome for an existing operator than the bug.

#### Credit where due

This is **not** command injection. Arguments are array-passed via `exec.CommandContext`, and the
database password is deliberately kept out of `argv` and passed through `PGPASSWORD`, with a comment
explaining the `/proc/*/cmdline` reasoning. That part was already right.
### 3.437 A panic in any plugin background goroutine killed the worker — ✅ fixed (the process-death half)

**cleat#1304.** `cmd/cleat-worker/main.go` spawned each plugin's background loop in a bare goroutine
with no `recover()`. An unrecovered panic in a goroutine cannot be caught by its parent, so **one
panic in one plugin terminated the whole worker process**, taking every workflow in flight on it.

Twelve plugins ship a `background.go` and none of them contains a `recover()`:

    auditlog  blobstore  datadogexport  eventstore  eventtriggers  jobqueue
    kafkaconnect  notifications  ratelimiter  scheduledbackup  scheduler  webhookingest

**The asymmetry is the finding.** The worker is hardened against *its own* loops panicking —
`withPanicRecovery` at `setup.go:875`, applied at `:1127` and `:3369` to every one — and was not
hardened against the loops it runs **on behalf of third-party code**, which is the weaker trust
assumption of the two.

#### The suggested fix was not available as written

The issue asked to route the spawn through `withPanicRecovery`. That is a method on `*Worker`, and
both lines are inside `func main()`:

    main.go:825   go func(bg plugin.HasBackground) { ... bg.Run(ctx) ... }
    main.go:951   w := &Worker{

**The Worker does not exist yet when the plugin loops start.** So `withPanicRecovery`,
`healthTracker.recordPanic` and `Metrics.RecordBackgroundLoop` are all out of reach at the spawn
site, and reaching them requires the plugin loops to be started *by* the Worker — a startup-ordering
change whose risk is that something between those two lines assumes plugin background work is
already running.

So this fixes the half that needs no decision: **the process survives**. Health-tracker integration,
metrics and watchdog restart are tracked separately, together with the backoff question the issue
already raises — a loop that panics every iteration must not spin.

#### Extracted into a named function so it can be tested

`runPluginBackground` rather than an inline closure. A closure inside `main()` cannot be driven by a
test, and **a guard nothing exercises is how the gap lasted** — the machinery to prevent this
existed for a year and was applied everywhere except here.

#### Falsification, and it is unusually blunt

Removing the `recover` does not make the test fail. It **crashes the test binary**, exactly as it
crashed the worker:

    panic: plugin background loop exploded
    …runPluginBackground(…)  main.go:1335
    created by …TestAPluginBackgroundPanicDoesNotKillTheWorker in goroutine 25
    FAIL  github.com/cleat-team/cleat/cmd/cleat-worker

That is the defect reproduced verbatim, which is why the panicking case is driven through a real
goroutine rather than called directly.

Two further assertions, because surviving is not sufficient:

- **the panic is logged with its stack and the plugin's name.** Recovering silently would pass the
  test above and leave an operator with a plugin whose background work has stopped and nothing
  saying so — trading a loud failure for a silent one, which is not obviously the better trade.
- **the control**: an ordinary `Run` error still reaches the log and is *not* reported as a panic.
  Without it, "the goroutine returns" is satisfied by a `runPluginBackground` that never calls `Run`.

---

### 3.438 `renovate.json` configured a bot that had never run; Dependabot now covers what ships — ✅ fixed

**cleat#1321.** The issue asked for a `wasmtime-go` rule in `renovate.json`, on the argument that
the config gave a grouping and a schedule to wazero — removed as the worker backend in #459 — and
none to the only production WASM backend. The argument is right. **The fix it implies is a no-op.**

    gh api "search/issues?q=repo:cleat-team/cleat+is:pr+author:app/renovate"   --jq .total_count  ->  0
    gh api "search/issues?q=repo:cleat-team/cleat+is:pr+author:app/dependabot" --jq .total_count  -> 29

Renovate has never opened a pull request in this repository. Adding a rule to that file would have
changed nothing observable and read, to the next person, as the gap having been closed.

#### What was actually running

`.github/dependabot.yml` covered **`github-actions` only**. Every Go module, every npm package and
every crate got updates solely when a Dependabot **security** alert fired — which needs an advisory
to exist, to be in GitHub's database, and to match the pinned version's range. Routine patch
releases, including ones that fix a problem before an advisory is published, arrived never.

That is why #1033 (grpc, in `tests/cross-language`) and #1064 (vitest, in `web`) are the only
non-Actions dependency PRs in the repository's history. Both are security updates, and both landed
in directories the config did not mention — which is also the evidence that excluding a directory
costs no advisory coverage.

#### The decision, and what it covers

`renovate.json` **deleted**, `dependabot.yml` extended to `gomod`, `npm`, `cargo` and `pip` across
**11 directories**. wasmtime gets its own group rather than being batched, because burying it in a
twenty-module PR is how its advisory cadence stops being visible — which is the issue's real point.

Deliberately excluded: `examples/`, `testdata/`, `tests/`, `benchmarks/`, `cmd/cleat/templates/` —
fixtures pinned on purpose, where a bump is churn rather than a release.

#### The guard, because both failure directions are silent

`scripts/check_dependabot_coverage.py`, wired into `lint`:

| direction | why nothing would say so |
|---|---|
| an entry names a directory with no manifest | Dependabot skips it with no error — the `renovate.json` failure at entry scale |
| a shipped manifest is in no entry | it gets security updates only, which looks like coverage until an advisory is late |

`--self-test` runs **two known-positives** — doctored configs the guard must report — rather than a
clean run, which every broken version of a guard also passes. Falsified against the real config
both ways: dropping `/cleat` reports *"has a go.mod and is in no gomod entry"*; adding
`/crates/does-not-exist` reports *"which has no Cargo.toml"*.

It uses `git ls-files` rather than a filesystem walk, so `.claude/worktrees/` — a second copy of the
repository — cannot contribute a manifest.

#### Two stale claims corrected, and they were the same defect one level down

`plugin-harness-ci.yml` and `tier1-gate.yml` both justified a pinned service-image digest with
*"renovate.json extends config:recommended … so a newer digest arrives as a reviewable PR"*. That
was never true, and measurably so: **no Dependabot PR has ever touched a file under
`.github/workflows/`**, and the SQL Server digest has been unchanged since 2026-08-07. The pins are
still right — they are the whole difference from `:latest` — but a human has to move them, and the
comments now say so.

#### One thing checked rather than assumed

The guard imports `yaml`, and the step is placed **after** `Workflow files parse`, which installs
pyyaml. The `Required-context guard` at step 4 also imports yaml and passes today, so pyyaml is
evidently preinstalled on `ubuntu-latest` — but that install is the only thing in the file which
*guarantees* it, and depending on a runner image's contents is how a guard stops running without
failing. The `run:` block was extracted from the parsed YAML and executed verbatim before being
relied on.

### 3.439 The payload encoding was inferred at read time, and inference cannot work — ✅ fixed

**cleat#1319.** `tryDecodeBase64` base64-decoded a stored `request`/`response` and fell back to the
raw string when decoding **failed**. That is the wrong question. Plenty of ordinary text decodes
successfully: `base64.StdEncoding` accepts any string whose length is a multiple of 4 whose bytes
are all in `[A-Za-z0-9+/]` with valid padding, so **every four-character alphanumeric string
decodes**.

Publish the predicate, not the sample — the rule generates the set, and a sample can be argued with
by choosing a different one:

    "test" -> "\xb5\xeb-"   "user" -> "\xba\xc7\xab"   "true" -> "\xb6\xbb\x9e"
    "abcd" -> "i\xb7\x1d"   "1234" -> "\xd7m\xf8"      "null" -> "\x9e\xe9e"

#### Most rows were never at risk, and finding that out took a falsification

The `payload` JSON column already records the encoding explicitly — `eventRecordToPayload` writes
`request_b64`/`response_b64` — and `populateFromPayload` runs **after** the scanned columns on every
read path. So wherever `payload` is present, the columns and `tryDecodeBase64` are shadowed.

This was found by breaking the writer and watching the round-trip test **pass anyway**. A test that
checks only that a value survives cannot see which of two paths delivered it.

#### The exposure is real, ongoing, and confined to call intents

Three writers in `store_intent.go` store `rec.Request` **raw** — there is no `tryEncodeBase64`
anywhere in that file — and write **no `payload` column**. So every call intent lands in the
vulnerable class by construction, on every durable call. Measured before the fix, identically on
all three dialects:

| request | read back |
|---|---|
| `true` | `\xb6\xbb\x9e` |
| `null` | `\x9e\xe9e` |
| `1234` | `\xd7m\xf8` |
| `{"a":1}` | **intact** |

JSON objects were never at risk: `{` and `"` are not in the base64 alphabet. A JSON **scalar** is —
`true` and `null` are four characters drawn entirely from it. And these rows are what the ambiguity
resolver reads **after a crash**, so the corruption surfaced during recovery.

#### The column, and why NULL is a state rather than a gap

`event_history.payload_encoding`, nullable, on all three dialects:

| value | meaning |
|---|---|
| `NULL` | the row predates the column; the encoding is genuinely unknown, so the read keeps the historical guess |
| `1` | base64 |
| `0` | plaintext |

**Deliberately not backfilled.** A backfill would have to answer, for every existing row, the exact
question the column exists because nobody can answer. The ambiguity stays confined to rows that are
genuinely ambiguous, and everything written from now on is unambiguous. `SMALLINT` rather than
`BOOLEAN`: same storage, room for a future encoding without a second three-dialect migration.

#### A `b64:` prefix was considered and rejected

It does not remove the ambiguity, it **relocates** it — *"does this legacy value start with
`b64:`"* is a rarer guess, still a guess, and still silently wrong when it lands. It also costs 8
bytes per row on PostgreSQL and MySQL and **16 on SQL Server**, where these columns are UTF-16,
against 0-1 byte for a nullable column that sits in a null bitmap the row already has.

One thing that could have decided it and did not: the integrity checksum is computed over the
**plaintext** record (`flush.go`, before `tryEncodeBase64`), so neither option disturbs replay.

#### Falsification

| mutation | what failed |
|---|---|
| intents stop recording the encoding | the corruption returns verbatim — `"true" -> "\xb6\xbb\x9e"` on all three dialects |
| one read path reverts to `tryDecodeBase64` | the completeness guard names the file and line |

The JSON-object case is the **control**: without it, "nothing was corrupted" is equally satisfied by
a reader that stopped decoding altogether.

---

### 3.321 An update name was consumed for the life of the workflow, and the row's identity was its name — ✅ **FIXED 2026-09-13** (cleat#1416)

**Decided by the repo owner: an update name is reusable. "An update is a request."**

`workflow_update_requests` was `PRIMARY KEY (workflow_id, update_name)` on all three dialects, and
completion is an `UPDATE … SET status = 'completed'` rather than a delete — so a workflow could
accept each update name exactly **once in its entire lifetime**. cleat#1330 measured the three
dialects answering the second request three different ways, one of them a `202` over a promise that
could never settle; cleat#1392 made them agree on a `409` and said in its own commit message that
the refusal would become *"unreachable rather than wrong"* if the name were later made reusable.
It is, and it did.

**What replaced the key, and why it is not `promise_id`.** The row needed an identity that survives
a duplicated name. `promise_id` was the tempting reuse — it is already unique and already carried
through the request key — and it is the wrong one: the column is nullable and means *"someone is
waiting"*. `engine/updater.go` has an explicit branch for a request with no promise and three tests
create one, so making it the key would render a caller-less request unrepresentable. A generated
`request_id` was added instead, minted Go-side so all three dialects behave identically and the
value exists before the INSERT.

**The migrations backfill `request_id` from `update_name`, and that choice is load-bearing twice
over.** Under the old primary key `(workflow_id, update_name)` is unique *by construction*, so the
copy satisfies the new key without inventing anything — and it is what lets a workflow suspended
mid-update across the upgrade complete against the right row with no special case, because the
request key it replays carries the name and nothing else.

**The risk was never "is the second request accepted".** That is the easy assertion. Two rows
sharing a name is a state that could not previously exist, and every reader keyed on
`(workflow_id, update_name)` silently addresses *one of them* in it. Measured on PostgreSQL against
the migration with the old predicate still in place:

    two pending rows, r1 and r2, both named "bump"
    UPDATE ... WHERE workflow_id = ? AND update_name = 'bump' AND status = 'pending'
    -> UPDATE 2

One completion closes both. `DurableCompleteUpdate` then settles **one** promise, and the other
caller holds a promise nothing will ever settle — not even `failStrandedUpdates`, which sweeps rows
that are still `pending`, and that row now says `completed`. That is the exact failure
`cleat/runtime_updates.go` names as the reason updates exist.

Two sites had to change, and the second is the one that would have gone unnoticed:

| site | keyed on | why it matters |
|---|---|---|
| `CompleteUpdateRequest`, ×3 dialects | `request_id` | the guest completing its own update |
| `failStrandedUpdates` | `upd.RequestID` | the sweep whose whole job is that nobody is left waiting |

**The request key gained a version marker rather than a third inferred field.** A v2 key is
`<len>:<name>` followed by the promise id; v3 is `<len>:<name><len>:<requestID>` followed by it.
Telling them apart by looking for digits-then-colon after the name means asking whether a *promise
id* happens to start that way — arbitrary in the store's contract, and the tests alone use
`"prom-1"`, `"promise-a"` and `""`. `u3:` costs three bytes and removes the question;
`TestAPromiseIDCannotBeMistakenForAV3Marker` pins it.

**Falsification.** Both guards, both restored by content against the commit as a separate step:

| mutation | caught by |
|---|---|
| all three `CompleteUpdateRequest` predicates back to `update_name` | the three-dialect test, on **all three** — `2 requests named "bump" are still pending … want exactly 1` |
| `failStrandedUpdates` back to `upd.UpdateName` | `completed [bump bump], want [ureq-a ureq-b]` |

Note the first mutation's signature is `2 pending`, not `0` — the store matched on the name while
the caller passed an id, so it matched *nothing*. The `0` case is the historical one and is what
the raw-SQL measurement above shows. Both are failures and both are caught; recorded because the
number differs from the story.

**Migrations** `postgres/068`, `mysql/062`, `mssql/066`, each applied **twice** against a database
built from the full set, because `SetupFullSchema` re-applies everything in tests.

---

### 3.322 A workflow result that succeeds on one backend can fail on another, and nothing said so — ✅ **FIXED 2026-09-13** (cleat#1025)

**Decided by the repo owner: document the intersection. The contract for a workflow result is what
all three dialects accept; cleat does not normalise them to agree.**

`workflow_instances.result` is a different type per backend, and the type is the constraint —
`JSONB`, `JSON`, and `NVARCHAR(MAX)` with `CHECK (ISJSON(...) = 1)`. Nothing upstream catches a
violation: `coerceResultJSON` checks `json.Valid` and object shape and *reports* rather than
rejects, so every divergent payload passes it. The rejection lands in `FinalizeWorkflowSegment`,
**after the workflow body and its side effects have run** — the work is done and the record says
`failed`.

**Two of the three sources I was given were wrong, and re-measuring is what found it.**

| claim | source | measured |
|---|---|---|
| MySQL rejects a lone surrogate with `3141` | cleat#1025 | **`3140`** |
| SQL Server stores every divergent payload | a summary handed to me | **false** — it refuses nesting past **128** |
| "PostgreSQL reorders keys; the other two preserve bytes" | same summary | **false** — MySQL reorders too; only SQL Server preserves |

The issue itself said SQL Server was *"not established … likely a third answer, worth measuring"*.
A contract document is precisely the artefact that must not infer that column, so it was measured.

**The limits, measured 2026-09-13 on PostgreSQL 16.15, MySQL 8.4.11, SQL Server 16.0.4275.2:**

    NUL escape        pg REJECTED 22P05   my accepted        ms accepted
    lone surrogate    pg REJECTED 22P02   my REJECTED 3140   ms accepted
    depth 100/101     pg ok / ok          my ok / REJECTED   ms ok / ok
    depth 128/129     pg ok / ok          my REJECTED        ms ok / REJECTED
    integer 2^64      pg exact            my 1.8446744073709552e19   ms exact

So: no NUL escape, no unpaired surrogate, depth ≤ 100, integers exact only within ±(2^64−1) — and
**not** "must fit `BIGINT`", which cleat#1022 originally said and which is wrong by a factor of two
on the positive side, since `9223372036854775808` is past signed `BIGINT` and every backend keeps it.

**Acceptance is not the whole contract.** A result accepted everywhere still reads back differently:
`{"b":1,"a":2}` becomes `{"a": 2, "b": 1}` on PostgreSQL **and MySQL**, and `{"a":1,"a":2}` becomes
`{"a": 2}` on both. Only SQL Server preserves bytes — the backend that validates least. So the
contract also forbids depending on key order or on duplicate keys surviving.

**The measurement had a silent-failure of its own worth recording**, because it is this repo's
recurring shape and it produced a complete-looking table with one column measuring nothing:
`dbo.TenantFilter_Instances` is a FILTER PREDICATE, so with no session context the seeded row is
invisible, the `UPDATE` matches zero rows and **reports success**, and every SQL Server case read as
"no rows". Nothing errored. `database/sql` made it worse by clearing the context between statements
— it calls `ResetSession` when a connection returns to the pool and go-mssqldb implements that as
`sp_reset_connection`. The fix is a held `sql.Conn`; the test carries a precondition that fails
loudly if the row is not visible, so this cannot recur quietly.

**The document is guarded rather than trusted.** `TestAWorkflowResultContractIsTheIntersection`
asserts every row of both tables on all three dialects, so a backend changing its limits turns
§7.4 red instead of stale. Falsified twice, each restored by content: claiming MySQL accepts depth
101 fails with `REJECTED on mysql, want ACCEPTED`; claiming SQL Server normalises fails on both
normalisation cases.

---

### 3.323 One boolean answered two questions, so replay re-invoked seven plugin functions live — ✅ **FIXED 2026-09-13** (cleat#1318)

**Decided by the repo owner: split `FuncOptions.Idempotent` into two properties.** Not "drop the
flag", not "re-classify against the existing one".

`Idempotent` was documented as *"safe to re-invoke during replay"*, and `engine/plugins.go` acted on
it literally: on replay it discarded `rec.PluginOutput` and called the function live. So a word that
only promises **no new side effects** was licensing a determinism claim — **returns the same value
on replay**. They come apart exactly where the wording is most inviting.

Replay now re-invokes only when a registration sets **both**:

    Idempotent          calling again has no additional effect
    SameValueOnReplay   calling again returns what the first call returned

**`SameValueOnReplay` is a claim about the WORLD, not the function**, and that framing is what makes
each registration answerable. A perfectly deterministic function fails it if its inputs can change
in between — which is why "read-only" was the wrong predicate and why four read-only functions were
wrong.

**The classification, one stated reason each rather than a bulk assignment:**

| registration | Idempotent | SameValueOnReplay | why |
|---|---|---|---|
| `blobstore.get` | ✅ | ✅ | write-once keys — **a convention the plugin does not enforce** |
| `llm.embed` | ✅ | ✅ | near-deterministic for a fixed model — **"near" is doing work** |
| `llm.list_models` | ✅ | ❌ | a provider's catalogue is not stable over a run |
| `pgvector.search` | ✅ | ❌ | reads a mutable index |
| `featureflags.evaluate_flag` | ✅ | ❌ | a flag exists in order to be toggled |
| `eventtriggers.await_event` | ❌ | ❌ | selects the latest UNPROCESSED event **and writes** `registerAwaiter` |
| `webhookingest.await_webhook` | ❌ | ❌ | an await over mutable state |

The two survivors are marked with what their claim rests on, because both are assertions rather
than properties of the code — `blobGet` takes a key and no version, and every store it targets will
overwrite that key. If either convention fails in a deployment, those are the next wrong entries
and they will be wrong the same way.

**Three findings that changed the work, each recorded on the issue:**

- **The set was seven, not eight.** #1408 had already removed `pgvector.delete` — the headline case.
- **Re-invocation saves nothing.** Every plugin call's output is recorded on the original run;
  only the *replay* re-invocation skips recording. So the recorded value was always available and
  re-invoking could only ever return something else. Reported before building rather than after.
- **The manifest path was never connected**, and I had claimed the opposite when taking this on.
  `HostFuncDef.Idempotent` reaches `plugingen`'s IR and is read by nothing outside tests, so
  `idempotent: true` in a manifest has always produced a registration with the field unset. That is
  a latent trap — the opposite is the natural assumption — so it is documented on the field.

**Two adapters had to carry the split**, `engine/app.go` and `cmd/cleat-worker/setup.go`. Missing
either would have left that path on the old meaning **silently**: the registry would read
`SameValueOnReplay` as false and simply stop re-invoking, which looks exactly like the fix working.

**Falsified three ways, each restored by content:**

| mutation | caught by |
|---|---|
| `evaluate_flag` claims `SameValueOnReplay` | both allowlist tests, the second printing the recorded argument against it |
| the scan regex stops matching | `matched no FuncOptions literals at all … every assertion built on this scan is vacuous` |
| `MayReInvokeOnReplay` returns `Idempotent` alone | the behavioural test, at `(1, 2)` want `(0, 0)` |

**The behavioural test is written to be positive**, because *"the live function did not run"* is an
absence and an absence is what a broken harness reports too. The live function is registered to
**return an error**, so "recorded output used" and "live call made" are two visible outcomes rather
than a presence and a nothing — and a control asserts the live path is reachable in that harness, so
the main case cannot pass vacuously.

**Docs carried the advice that produced this.** `plugin-developer-guide.md` said *"Use
`Idempotent: true` for read-only functions"* and, separately, recommended the flag to avoid storing
large outputs — which never worked, since the output is recorded either way. Both corrected.

---

### 3.324 Three endpoints answered a duplicate call three different ways, and none of them was written down — ✅ **FIXED 2026-09-13** (cleat#1169)

**Owner decision, 2026-09-10:** *"a standard policy of return-the-original-result, but with a
standard extra flag in the result … Mostly they don't care, and shouldn't need separate code to
handle the cached result."* The load-bearing clause is the last one: the property the old design
lacked was **uniformity**, not information.

**Refined by the owner 2026-09-13, and it changed the scope.** I proposed excluding `POST
/api/schedules` because it takes no key and a name collision is not a retry. The answer:

> if schedule stuff is creating something, then it ought to have the idempotency key, and the
> associated idempotency replay policy should apply

That **dissolves** the objection rather than trading it away. My argument was an artefact of
schedules having no key to tell the two cases apart with; given one, `409 schedule_exists` keeps
answering *someone else's name is in the way* and stops being conscripted to answer *I am retrying*.

**Measured first, on `develop@32a8b90b`, because the issue's table is from 09-10 and #1247 landed
since.** Three *different mistakes about one question*, not three arbitrary policies:

    start       201 {"id":X}               -> 200 {"already_started":"true","workflow_id":X,"status":…}
    reprocess   201 {"id":X}               -> 200 {"already_started":"true","workflow_id":X}
    signal      200 {"status":"delivered"} -> 200 {"status":"already_delivered"}
    schedules   201 {"status":"created"}   -> 409 {"detail":"schedule_exists"}

`start` and `reprocess` changed status, shape **and** the identifier's field name. `signal` had the
right status and shape but welded the marker into `status`, so one field carried *what happened* and
*was this a replay*. `schedules` refused.

**The rename was the expensive one, and its failure mode is an inversion rather than an error.** A
caller reading only `id` gets nothing from a deduplicated response, concludes its retry started a
**second** workflow, and may compensate, alert, or start a third. cleat's own DBOS port carries a
shim for it, and its docstring records that the bug was found by a control rather than by reading.

**The guard derives its population rather than listing it**, which is the point. #1167 was found by
auditing endpoints one at a time; a hand-written list has the same defect — it covers what its
author remembered. `TestEveryKeyBearingEndpointCarriesTheReplayFlag` reads the handlers that call
`Header.Get("Idempotency-Key")` out of the source and requires each to set the flag, so a new
endpoint is covered the day it is written. It found `handleStartWorkflow`, `handleSignal` and
`handleDeadLetterReprocess` unaided.

**Why that population and not the owner's rule.** *"Every endpoint that creates work"* is the right
policy and cannot be decided by reading source — "creates work" is a judgement. Reading the header
is the observable commitment, so a handler that creates work and does **not** read it is #1167's
defect, caught by review; one that reads it and omits the flag is this one, caught mechanically.

**Schedules is split out, and the split is safe because the guard grows into it.** It needs a store
primitive that does not exist — idempotency is baked into `StartNewRun`, and
`idempotency_keys.workflow_id` is `NOT NULL`, so a schedule name cannot go there without a migration
and without disturbing cleat#1258's retention. Schedules does not read the header today, so it is
legitimately outside the guard's population; the moment it gains one, the guard requires its flag
with no edit.

**One test got better by accident and it is worth recording.** `TestReprocessIsIdempotentUnderTheSameKey`
asserted a literal `200` under the message *"a retry after a lost response must not create a second
run"* — but the status code never established that; the id match and `starter.n == 1` did. It now
asserts **status parity with the first call**, which is the actual contract and does not need editing
the next time a status changes.

**Flag design, stated because each part was a choice:** a **bool**, not the string `"true"`
`already_started` used to be — a typed client reading that as a boolean gets a type error. Present
on the **original as well as the replay**, because an absent field cannot be told from an old server
that does not send one, so `absent means original` would be unreadable by exactly the cautious
client most likely to check. And **not a response count**, which would mean storing how many times a
key was replayed — new persistent state for a case the decision says callers mostly do not care
about.

---

### 3.325 A tenant could not be deleted at all on SQL Server, and the manual cleanup reported success while deleting nothing — ✅ **FIXED 2026-09-15** (cleat#1635)

`admin.drop_tenant` existed only on PostgreSQL, so `cleatctl drop-tenant` refused on SQL Server
(`cmd/cleatctl/ported.go`) and there was no supported way to remove a customer's data on a tier-1
dialect.

**The issue was filed with two claims that turned out to be wrong, both mine.** They are recorded
because the corrections are the useful part.

*"There is no ordinary path to those rows."* Measured on SQL Server 2022, against the predicate
`applyTenantScopingMSSQL` emits, two tenants seeded each under its own key:

| session | rows visible |
|---|---|
| `sa`, `IS_SRVROLEMEMBER('sysadmin') = 1`, no session context | **0** |
| `tenant_id` = the dropped tenant | **1** |
| `cross_tenant` set to any non-empty string | **2** |

The first row is real and is why the residue looked unreachable: SQL Server applies RLS to
`sysadmin` and `db_owner` too, with no `BYPASSRLS` counterpart. But the predicate's `cross_tenant`
disjunct admits, and so does the dropped tenant's own key — the predicate never consults
`admin.tenants`, so a tenant being gone does not enter into it. The rows were always reachable.

*"`admin.plugin_tables` does not exist on SQL Server."* It has existed since
`migrations/mssql/001_schema.sql:117`. What is true is narrower: it carries the pre-066 two-column
shape and has never had a producer on that dialect.

**The defect that was actually left is a silent no-op, and it is worse than the one in the title.**
A filter predicate hides rows from `DELETE` exactly as it hides them from `SELECT`, so the cleanup
an operator reaches for after being refused by cleatctl:

    DELETE FROM dbo.kv_store WHERE tenant_id = '<dropped tenant>';
    -- (0 rows affected)

No error, nothing deleted, and `(0 rows affected)` is indistinguishable from *already clean*. With
the tenant key set first the identical statement reports `1 row affected`. Both directions
measured, and pinned by `TestADeleteWithoutTheTenantKeyRemovesNothingOnSQLServer` — a
characterisation test, so if a future SQL Server raises instead, it goes red and migration 074's
header needs rewriting.

**The table set is DERIVED rather than listed, and that is the design decision worth carrying.**
PostgreSQL's `admin.drop_tenant` deletes from a hand-maintained list, and that list has drifted
three times:

| | added by | missing from `drop_tenant` until |
|---|---|---|
| `tenant_settings` | 039 | "twenty migrations", per its own comment in `droptenant.go` |
| `workflow_defs` | 001 | #1201 |
| `workflow_memory_{stats,samples}` | 056 | **still open as #1644**, found while writing this |

A list cannot answer *"is there a tenant-owned table I do not know about"*; `sys.columns` can. So
`migrations/mssql/074` deletes five foreign-key-ordered tables by name and then sweeps every
remaining table carrying a `tenant_id` column — core, plugin, and anything a later migration adds,
with nobody editing the procedure. It needs no plugin registry, because on SQL Server plugin
tables live in `dbo` alongside the core ones.

The test reads the same universe from a different place, so it can disagree with the procedure.
Falsified twice, each on a fresh database built by the migration runner — removing the sweep, and
removing the session-key set — and both times it named `dbo.mssql_drop_tenant_probe` **and**
`dbo.workflow_memory_stats`, which is the #1644 class being caught on the dialect that has the
derived sweep.

**`SET QUOTED_IDENTIFIER ON` is in the migration on purpose.** A procedure captures that setting at
CREATE time, and every table this one deletes from is bound to a `WITH SCHEMABINDING` predicate, so
a `DELETE` compiled with it OFF fails with `Msg 1934`. Measured: created from `sqlcmd`, which
leaves it OFF for a `-i` script, the procedure was unusable. The nine other procedures in
`migrations/mssql/` carry no such SET and work only because go-mssqldb's login turns it on — a
client default holding a schema decision up.

**No "no such tenant" guard**, deliberately: the state this procedure most needs to work in is the
one the issue describes — the `admin.tenants` row already deleted by hand, the data still there —
and a guard on that row would refuse exactly the cleanup it exists to perform.

Files: `migrations/mssql/074_a_dropped_tenants_rows_go_with_it.sql`,
`cmd/cleatctl/droptenant_mssql.go`, `cmd/cleatctl/droptenant.go`, `cmd/cleatctl/ported.go`,
`engine/a_tenant_can_be_dropped_on_sql_server_test.go`.

---

### 3.326 A dropped tenant's memory profile survived, and the list that missed it had already drifted twice — ✅ **FIXED 2026-09-15** (cleat#1644)

`admin.drop_tenant` deleted from a hand-maintained array of seven core tables.
`workflow_memory_stats` and `workflow_memory_samples` have carried `tenant_id` since migration 056
(cleat#1040), neither has a foreign key to anything, and neither was named — so nothing deleted
them and nothing cascaded them.

**Measured** on a database built from `migrations/postgres/*.sql`, two tenants seeded, in migration
066's own three-number shape:

```
SEEDED  stats A=1 B=1   samples A=1 B=1
AFTER   stats A=1 B=1   samples A=1 B=1   admin.tenants A=0 B=1
```

`A=1` after is the bug. `B=1` says a fix must not become "delete everything". `admin.tenants A=0`
says the drop genuinely ran — without it, a drop that silently did nothing produces the same
surviving rows and reads as the same bug.

What survived is not an implementation detail: `workflow_memory_samples` is keyed
`(tenant_id, def_name)` and `def_name` is the tenant's own workflow name, so what outlived the
tenant was a list of the workflows it ran and how much memory each used.

**The two names are the small half.** The list has now drifted three times, and so has its twin in
`cmd/cleatctl/droptenant.go`:

| | added by | missing until |
|---|---|---|
| `tenant_settings` | 039 | "twenty migrations", per `droptenant.go`'s own comment |
| `workflow_defs` | 001 | #1201 |
| `workflow_memory_{stats,samples}` | 056 | this |

Migration 056 named the mechanism, about a different guard: *"it answers 'is every statement
against a KNOWN tenant-scoped table scoped?' and cannot answer 'is every table that should be
tenant-scoped actually one?'"* `admin.drop_tenant` has that blind spot with the roles reversed.

**So the durable half is two tests whose universe comes from `information_schema.columns`** — a
derivation that can disagree with the lists rather than one that restates them:

* `TestEveryTenantOwnedTableIsEmptiedByDropTenant` (engine) requires a **seed for every member of
  the universe** before it checks emptiness. That ordering is the whole design: "zero rows
  afterwards" is satisfied by a table that was never seeded, which is how cleat#1265 published
  "4 of 4 clean" off a run where two of six seeds had failed. A new tenant-owned table fails in
  the seed precondition, naming itself.
* `TestThePreviewNamesEveryTenantOwnedTable` (cleatctl) compares `dropTenantTables` against the
  same universe in both directions.

**The preview was short by THREE, not two**, and the third is the argument for deriving rather than
reading: `admin.tenant_egress_allow` is deleted correctly, by `ON DELETE CASCADE` from
`admin.tenants`, and had never been counted. No amount of reading `admin.drop_tenant` finds it —
nothing in the function names it. That matters because `drop-tenant` prints those counts twice,
once as the thing the operator confirms and once as the audit record the command has instead of an
audit table, so an uncounted table is deleted silently and recorded as not having existed.

**Falsified five ways**, each restored by content as its own step:

| mutation | which assertion fired |
|---|---|
| the two names removed from the array | emptiness, naming both tables |
| a brand-new `tenant_id` table created in the database | the seed ratchet, naming it |
| one seed redirected to a different tenant | the precondition, naming the table |
| one entry removed from `dropTenantTables` | preview coverage |
| a preview label transposed onto another table's query | the label/query agreement check |

The second is the known-positive: the case the test exists for, and the one no previous guard
could see.

**Not derived the way SQL Server's is**, and deliberately. `migrations/mssql/074` (§3.325) sweeps
`sys.columns` and carries no list at all. PostgreSQL cannot copy that as cheaply: `--schema`
(cleat#1287) means an install's tables are not all in one known schema, and per-tenant
`tenant_<uuid>` schemas hold tables with a `tenant_id` column belonging to *other* tenants — two
were present on the test database while this was written. Rewriting how the most destructive
routine in the schema chooses its tables is a change with a different risk profile from adding two
names, and the tests close the drift class either way.

Files: `migrations/postgres/082_a_dropped_tenants_memory_profile_goes_with_it.sql`,
`engine/a_dropped_tenants_rows_all_go_with_it_test.go`,
`cmd/cleatctl/the_preview_names_every_tenant_owned_table_test.go`,
`cmd/cleatctl/droptenant.go`, `engine/drop_tenant_test.go`.
### 3.327 A coverage guard compared two different questions, and only job ordering kept it green — ✅ **FIXED 2026-09-16** (found while landing §3.325)

`TestEveryShippedTenantPolicyExistsInTheBuiltDatabase` parses
`migrations/mssql/*.sql` for tables bound to **`dbo.fn_tenant_filter`**, then reads the built
database with

```sql
SELECT DISTINCT t.name FROM sys.security_predicates sp JOIN sys.tables t ON ...
```

— **every** predicate, whatever function it calls. Two sides, two questions. That was harmless for
as long as the schema had exactly one predicate function, which was true when the test was written
and stopped being true with §3.216's plugin policies: `dbo.fn_plugin_tenant_filter` is installed
from `plugin/migration.go` at plugin-migration time, so no file in `migrations/mssql` binds it and
it cannot appear in the `want` set.

The result is that on any database where plugin migrations have run, ~25 correct, deliberate plugin
predicates were reported as *"the database has a tenant filter predicate on [...], which no
migration binds one to"* — a schema disagreement that is not one.

**It stayed green in CI on an ordering, not an invariant.** Every job that runs both sweeps
`./engine/...` before `./plugins/...`, so the guard runs before the plugin tables exist. Nothing
states that dependency and nothing enforces it; it failed twice locally the first time a database
was reused, which is how it was found.

**The fix is to make both sides name the same function**, and the anchor matters:
`predicate_definition` reads `([dbo].[fn_tenant_filter]([tenant_id]))`, so the match is on
`\[dbo\]\.\[fn_tenant_filter\]` — brackets included. A bare substring test for `fn_tenant_filter`
is one rename away from also matching `fn_plugin_tenant_filter`; it does not today only because the
`fn_` happens not to be adjacent. Brackets are where the catalogue puts an identifier's boundary,
which is the same move as anchoring on a declaration site rather than on a name.

**The narrowing is REPORTED, not applied silently.** A passing run now logs what it excluded and
why. A guard that quietly shrinks its own population reads as covering more than it does — and this
one shrank from "every predicate" to "the engine's predicate", which is the right scope and is
worth seeing.

**Falsified three ways**, the third being the one a careless fix would fail:

| mutation | result |
|---|---|
| the pre-fix test, same database | RED — reports the plugin table as a disagreement |
| a real engine policy dropped | RED — still names `workflow_tags`, so the guard is not blinded |
| a **plugin** predicate added to `workflow_tags` while its engine policy is missing | RED — still names `workflow_tags` |

The third is the axis test. A fix that excluded *tables which also carry a plugin predicate*,
rather than *predicates on another function*, would pass the first two and fail this one silently —
it would let a plugin predicate stand in for a missing engine policy on the same table.

Files: `engine/mssql_policy_coverage_test.go`.

---

### 3.328 Nothing checked that a plugin table with a tenant column is declared TenantScoped — ✅ **FIXED 2026-09-16**

`Migration.TenantScoped` is the **only** input to `applyTenantScoping`: a plugin table gets a
row-level security policy because it is declared, and for no other reason. So a table carrying a
`tenant_id` column and missing the declaration is tenant-owned data with no policy on any dialect,
and nothing said so — the declaration is an opt-in, and an omitted opt-in is indistinguishable from
a table that does not need one.

**Nothing is wrong today, and that is why this is a guard rather than a fix.** Measured on develop:
19 plugin directories, 26 tables carrying a `tenant_id` column, 26 declarations, matching per
plugin with no gap in either direction. What was missing is anything that keeps it that way.

Migration 056 states the shape, about a different guard:

> so it answers "is every statement against a KNOWN tenant-scoped table scoped?" and cannot answer
> "is every table that should be tenant-scoped actually one?"

Every existing check is on the first side of that. `TestAPluginTableIsFilteredToItsTenantOnSQLServer`
(cleat#1629) proves a **declared** table gets its policy;
`TestEveryTenantScopedStatementNamesItsContext` (cleat#1640) proves a statement **names a
context**. This is the second side.

Cited by test name rather than by section number deliberately: three §-references written into the
first draft of this entry — 3.216, 3.243, 3.324 — were all wrong, each naming a real section about
something else, and each looked plausible enough to ship. A test name is greppable and moves with
the thing it names.

**The file set is the part that took the work.** `TestPluginDialectArmsDeclareTheSameColumns` scans
`plugins/*/migrations.go`, and `plugins/pgvector` keeps its `Migration` literal in `plugin.go` — so
it is outside that set entirely, and it is the plugin whose table is easiest to forget, being the
only deliberately PostgreSQL-only one. That costs the older guard nothing, checked rather than
assumed: pgvector declares only an `Up` arm, and that guard compares a table only when two or more
arms declare it.

It would have cost this guard its most likely finding. Measured by narrowing the set and re-running:

| file set | verdict | population |
|---|---|---|
| `plugins/` (shipped) | PASS | **26 tables across 19 plugins** |
| `plugins/*/migrations.go` | **PASS** | 25 tables across 18 plugins |

Both green. The narrowed one simply covers one plugin fewer and says nothing about it — a check
telling you it is consistent with itself while not looking at the thing most likely to be wrong.
So `TestTheTenantScopedScanReachesEveryPlugin` asserts the scan's own scope, from a **different
command** (`git grep -l TenantScoped:`) rather than a restatement of the glob, and falsifying it by
narrowing the set names `plugins/pgvector/plugin.go` exactly.

**A regex cannot read this and the neighbouring guard already paid to learn it.** A Go raw string
cannot contain a backtick, so a MySQL arm with a reserved column name is written as
`` `CREATE TABLE ...` + "`key`" + ` ...` ``, and a textual scan captures the first fragment and
stops — which would silently drop the `tenant_id` column and score the table as needing no
declaration. This reuses `evalStringExpr` and `createTableColumns` from
`TestPluginDialectArmsDeclareTheSameColumns` (cleat#1291) rather than reimplementing them, and the
fixture carries a concatenated case so that reading is exercised.

My own first census of this question used a regex and reported **21** tables against 26
declarations. The disagreement was the tell: a body captured with `.{0,4000}?\)` stops at the first
`NVARCHAR(200)`.

**Falsified three ways**: removing a real declaration (`kvstore.kv_store` — reported by name);
narrowing the file set (pgvector — reported by path); and a `testdata` fixture carrying all three
shapes at once, where the undeclared and concatenated tables must be reported and the declared one
must not, so the scanner is shown to discriminate rather than to flag everything.

Files: `plugin/every_plugin_tenant_table_is_declared_tenant_scoped_test.go`,
`plugin/testdata/tenantscoped/undeclared.go`.

### 3.258 `idempotency_keys` had no policy because one function read it too early — ✅ **FIXED 2026-09-15** (cleat#1534)

**cleat#1534.** `idempotency_keys` has carried `tenant_id` since migration 010 and an explicit
`AND tenant_id = $N` on every statement since. It never carried a policy. Migration 031 declined it,
migration 061 repeated the decline, and both gave the same reason: the table is *read before any RLS
context exists*. That was accurate, and it was about **one function**.

`PostgresStore.startNewRun` reaches the table three times. Enumerated by parsing SQL literals rather
than grepping — two derivations, 67 loose and 60 tight, with all 7 dropped rows inspected and none a
statement:

| | ran on | now |
|---|---|---|
| the live-key lookup | `s.db` — no transaction at all | `tx` from `beginTxWithRLS` |
| `INSERT ... ON CONFLICT DO NOTHING` | `tx`, with `setRLSOnTx` **eleven lines later** | the same `tx` |
| the concurrent re-read | `s.db`, after `tx.Rollback()` | `tx2`, its own RLS transaction |

The PostgreSQL production surface is **eight** statements and five were already under
`beginTxWithRLS`, so 031's stated reason was exactly right and exactly complete. Migration 083 is
what the reorder buys — the same restructure-then-protect shape 061 used and named.

#### The guard would have passed the broken tree

`TestNoPostgresStatementReachesAnRLSTableWithoutTheTenantSet` asked
`functionEstablishesTenant(fn)` — a boolean over the **whole function body**, with a comment saying
so deliberately, because `StartNewRun` legitimately did its idempotency_keys work first. The comment
was right that a line window is wrong and wrong that a boolean is the alternative. It is **order-blind**
(a statement before the call scores covered) and **transaction-blind** (`setRLSOnTx(tx1)` covers a
statement on `tx2`), and `startNewRun` is both at once — it has two transactions in sibling scopes,
**both named `tx`**, so name alone cannot separate them either.

Measured against develop with `idempotency_keys` in the RLS set and no other change:

| | faults named |
|---|---|
| guard as it was | **2** — `store_lifecycle.go:980`, `:1026` |
| guard as it is now | **3** — and `:1013`, the INSERT |

The one it missed is the statement its own comment pointed at. The guard now records *where* and
*on which variable* the tenant was established and requires a match on both.

#### It was also barely looking

Widening it moved the examined count from **17 to 168** on an unchanged tree. The tx arm was gated on
`!tenantSet && functionOpensRawTx`, a combination almost nothing in the store satisfies, so the guard
had been very nearly a "no statement on the pool" check wearing a broader name. 19 of the 168 are
still unreadable — non-literal SQL — and are counted as examined; that is cleat#1672, filed rather
than folded in.

#### The sweep is cross-tenant and stays cross-tenant

`idempotencyCleanupLoop` deletes every tenant's expired keys on one tick, with no tenant predicate
and wanting none. A fail-closed policy stops it dead — measured as `cleat_app` with no tenant set,
`cleat.tenant_id is not set (P0001)`. It now enters `cleat_sweep` for the duration of its
transaction, which is migration 077's shape. **Not** a fallback to the plain statement on failure:
that would succeed exactly where it is not needed (a connection bypassing RLS) and fail silently
into a warning everywhere else.

#### Prose corrected in four places, none of which would have failed

031, 050, 061 and this document each asserted the absence as a standing fact — 061 in the words
"none of those reasons has changed". Each is now marked superseded rather than rewritten, because
031's reasoning is what 083 had to answer. 050 also carried a census ("PostgreSQL's RLS covers 11
tables"); it is dropped rather than corrected, since the predicate — *this table was not among them* —
is what the sentence needed.

### 3.259 The RLS guard counted 19 statements it could not read as examined, and one was a live fault — ✅ **FIXED 2026-09-16** (cleat#1672)

`TestNoPostgresStatementReachesAnRLSTableWithoutTheTenantSet` read a statement's SQL by taking the
first argument that is an `*ast.BasicLit` matching `^\s*(SELECT|INSERT|UPDATE|DELETE|WITH)`. When
that found nothing it returned `""`, the scan moved on — **and the statement had already been
counted.** So the floor assertion, `stmts == 0`, was satisfied by statements the guard had no
opinion about.

#### It was hiding a live fault, which is why this is not tidiness

`adaptive_flush.go:253` wraps its query in `fmt.Sprintf`, so the argument is an `*ast.CallExpr` and
`sqlArgOf` returned `""`. The statement is

    UPDATE workflow_instances wi SET heartbeat_at = now() FROM claims c WHERE ...

on `af.db` — **the pool**, no transaction, no `set_config` — against a table that is `ENABLE` +
`FORCE ROW LEVEL SECURITY` with a fail-closed policy. Measured, PostgreSQL 16.15, with a positive
control:

| connection | result |
|---|---|
| `cleat_app`, no transaction, no tenant — the production condition | `ERROR: cleat.tenant_id is not set … (P0001)` |
| `cleat_app`, tenant set in a transaction | returns its row |

Same role, same statement, same seeded row. It is reachable from `cmd/cleat-worker`. Filed as
cleat#1677 and listed in `knownRLSFaults`, whose liveness check forces the entry out when the fix
lands. **This guard exists for exactly that defect class** (cleat#1177, `successorOfRun`) and it
walked the line, could not read it, and reported a clean run.

#### The issue's own characterisation was wrong, and correcting it shrank the work

cleat#1672 said the 19 were queries "built, held in a constant, or passed through a rewrite". True
of six. The rest were readable all along:

| class | n | why it was invisible |
|---|---|---|
| string concatenation | 3 | an `*ast.BinaryExpr` fails the `*ast.BasicLit` type assertion |
| `fmt.Sprintf` | 1 | an `*ast.CallExpr`, same |
| first line is a SQL comment | 3 | `^\s*(SELECT\|…)` does not match a leading `--` |
| package constant | 4 | an `*ast.Ident` |
| built at runtime | 5 | irreducible |
| not DML at all | 3 | `SAVEPOINT`, `CREATE SCHEMA` — correctly not checked, wrongly indistinguishable from the above |

`db.go:1028` is the sharpest: a complete `UPDATE workflow_instances` sitting in a plain literal,
invisible because its first line is `-- No AND tenant_id, deliberately: RLS bounds this.`

#### What changed

`sqlTextOf` resolves literals, concatenations, package constants, function-local constants and
`fmt.Sprintf` format strings. **A partial resolution is refused**: one unresolvable part makes the
whole expression unreadable, because the missing half could be the `FROM` clause, which is the
failure this file exists to prevent reintroduced as a convenience.

`argKind` replaces the empty string with three outcomes — `argSQL`, `argNonSQL`, `argUnreadable` —
because "not a statement to check" and "a statement I cannot read" were the same value.

**19 unreadable-and-counted became 1 unreadable-and-reported.** The remaining one, `db.go:1900`
(`CREATE SCHEMA IF NOT EXISTS ` + a runtime schema name), is in `knownUnreadableStatements`.

Three maps, three invariants, kept apart on purpose: `knownRLSFaults` must shrink to zero,
`statementsWithoutATenantByDesign` does not shrink (`flush.go:453` is the untenanted path, gated by
`if e.tenantID != ""` and never reached by a worker), and `knownUnreadableStatements` shrinks toward
the irreducible. Merging them would retire the "may only shrink" property that makes the first
worth having.

The floor now counts only statements the guard has an opinion about, and the log line states both
numbers — `cleared 167 … could not read 1` — because a single figure was correct on every run and
read as ordinary while the guard was near-blind.

### 3.260 An expired idempotency key made the next start fail, on all three dialects — ✅ **FIXED 2026-09-16** (cleat#1671)

A key whose TTL had passed, and whose row the sweeper had not yet collected, made the **next** start
with that key return `sql.ErrNoRows` instead of starting a new run.

#### The defect is neither the expiry filter nor the ON CONFLICT

`RowsAffected() == 0` from the key insert has **two causes**, and the code assumed one:

| cause | is there a winner to re-read? |
|---|---|
| a concurrent starter won the race | yes |
| an expired row is still sitting there | **no** |

Both report no rows affected, through three different idioms that all say nothing about expiry —
`ON CONFLICT (key_hash, tenant_id) DO NOTHING`, `INSERT IGNORE`, and
`INSERT … WHERE NOT EXISTS (key_hash AND tenant_id)`. The re-read that follows filters on
`expires_at > now()`, so in the second case it looks for a row it cannot see.

So the branch is entered because `ON CONFLICT` cannot tell *"another request won"* from *"a dead row
is still there"*. A fix aimed at the filter or at the TTL does not touch it.

#### All three dialects, and that is the point

`store_lifecycle.go`, `mysql_lifecycle.go` and `mssql_lifecycle.go` each carry the same shape. The
issue was filed as PostgreSQL; reading the other two to confirm the shape is what found them.
cleat#1256 is this table's precedent for the one-dialect fix: the sweeper ran on PostgreSQL only, so
a key was honoured forever on the other two for the life of the deployment.

#### The fix, and the fix that would have been wrong

Delete the row **before** the insert, scoped by expiry. That collapses the two causes: a conflict
now means a **live** row, and the re-read's own filter will find it.

Two rejected alternatives, both of which pass an expired-row test:

- **Drop `expires_at > now()` from the re-read.** Makes the expired row visible and hands the caller
  a workflow id whose key the TTL already retired — silently joining an expired run, which is worse
  than the error.
- **Delete by key alone.** Removes a *live* row a concurrent starter just wrote, so two callers each
  get their own run for one key — the defect migration 010 exists to prevent, reintroduced by the
  fix.

#### Both arms, because one alone ships the second mistake

| falsification | what went red |
|---|---|
| remove the delete | the expired-key test, with `sql: no rows in result set` |
| keep it, drop its expiry scope | the **concurrency** test — `2 of 8 starters were told they started the run` |
| remove it from MySQL only | the cross-dialect assertion, naming `mysql_lifecycle.go` |
| remove it from SQL Server only | the same, naming `mssql_lifecycle.go` |

The expired-row fixture is deterministic and needs no race: an expired row is invisible to the
lookup and still collides with the insert. The concurrency arm is the one that is tempting to skip,
and skipping it is exactly how the second mistake above ships green.

#### The fix broke its own neighbour's fixture, and the neighbour said so

`TestTheConcurrentIdempotencyRereadRunsWithTheTenantSet` (cleat#1534) reached the concurrent-re-read
branch by seeding an **expired row** — invisible to the lookup, still colliding with the insert.
This fix deletes the dead row before the insert, which removed that test's only way in. It did not
go quietly green:

    PRECONDITION FAILED: the start succeeded, so the INSERT did not conflict
    and the concurrent re-read was never reached. Nothing below was measured.

That is the whole argument for printing preconditions beside verdicts, demonstrated inside one PR: a
test asserting only "no error" would have passed while measuring nothing, and the RLS coverage
cleat#1534 added for that statement would have been silently gone.

Rewritten to reach the branch through **real contention**, which is also more faithful: a raw
transaction inserts the key and does not commit, the store's lookup cannot see it, and the store's
insert blocks on the unique index. The test waits for that block to appear in `pg_locks` rather than
sleeping — a conflicting insert waits on the inserting *transaction*, so `pg_locks.relation` is null
and the waiting backend's own query text is what identifies it. Scoped to this database and this
statement, because the engine suite shares a database and a bare `NOT granted` count is true of any
contention anywhere in the instance.

The precondition is then self-proving: the store can only return the competitor's workflow id by
having re-read it, since its own lookup ran before that row was committed. Verified in both
directions — reverting cleat#1534's `tx2` still produces `cleat.tenant_id is not set (P0001)`, and
committing the competitor early makes the precondition fire rather than passing through the lookup.

The cross-dialect check is a **source** assertion, the same shape as
`TestEveryDialectRefusesAnIdempotencyKeyReusedForAnotherDefinition` beside it, and for the same
reason: executing it needs all three databases, while what actually breaks is one store edited and
the others not. It asserts order as well as presence — a delete placed after the insert would
satisfy a contains-check and remove the row the insert just wrote.
---

### 3.329 Nothing asserted that a durable call's event is on disk before the call returns — ✅ **FIXED 2026-09-16** (cleat#1670)

`recordEvent` blocks until its event is durable — a receive on the flusher's `done` channel on the
batch arm, an inline `flushEvent` on the direct one — and `freshCall` records before it returns
(`durablecalls.go:158` `callService`, `:175` `recordEvent`). That ordering is what **bounds** the
crash window `docs/durable-calls.md` §2 describes: the window opens when the external service
returns and closes when the flush commits, so it cannot outlive the host call. Once the guest
resumes, the event is on disk.

Nothing asserted it. Making the flush fire-and-forget is an obvious performance change — it takes a
commit off the hot path — and it would extend that window across the guest's next durable step, its
next sleep, and everything after, **with every existing test still green**.

The nearest test is `tests/crash/crash_test.go`'s `TestEventsArePersistedDuringExecution`, which
sleeps **two seconds** and says why: *"the adaptive flusher batches with an 8ms window, so this is
generous by three orders of magnitude"*. Two seconds proves durability *eventually*; it cannot
separate "durable before the call returned" from "durable within two seconds of it" — identical
today, divergent after the change.

**No clock in the test, deliberately.** "Durable within N ms" goes green on a machine that is merely
fast. The direct arm asserts an ORDER from a counter both sides stamp, and it is sound both ways:

|  | flush stamps | return stamps | verdict |
|---|---|---|---|
| synchronous | 1 | 2 | passes |
| fire-and-forget | 2 | 1 | fails |

The wait on `flushed` before comparing is load-bearing: without it a fire-and-forget flush leaves the
stamp at zero, and `0 < returnSeq` is true — the check would pass the very thing it exists to catch.

**The batch arm is tested separately and asserts the row, not a stand-in.** The instant `recordEvent`
returns, the event must already be `SELECT`able. That arm matters more: its wait is a bare
`if err := <-done`, whose own comment notes it *"was a select with one case and no default"*, and a
reader asking why the hot path blocks on a batch would not be obviously wrong.

**Falsified against the engine, not just against the harness** — both mutations on `lifecycle.go`,
each restored by content:

| mutation | result |
|---|---|
| direct flush spawned in a goroutine | RED — *"the flush completed at 2 and recordEvent returned at 1"* |
| `<-done` replaced by a non-blocking discard | RED — *"event_history holds 0 rows … the instant recordEvent returned"* |

**The exception is asserted as the exception.** `recordEvent` blocks and then *continues* on failure:
`ErrFenceLost` at Debug, anything else at Error, checksum unadvanced, guest resumes with no durable
event. The unqualified property is therefore false today — on purpose for the fence-lost case, where
the claim was lost and this worker must not write. A test written without that qualifier fails
against correct behaviour, which is the known-positive trap arriving from the other direction.

**Two harness defects found by measuring rather than assuming**, both of which would have been read
as engine defects:

* The batch arm first reported 0 rows. Instrumenting `Flush` directly gave
  `pq: invalid input syntax for type uuid: ""` — an empty tenant in the fixture, which `recordEvent`
  logs and swallows. `<-done` had returned *after* the failed attempt, so the ordering held all
  along.
* The test first gated on `CLEAT_TEST_POSTGRES` alone. **CI sets `CLEAT_TEST_DB`**, so it would have
  skipped in every job and reported `ok`. `testutil.TestDB` already gates on either and skips
  itself; the private check is gone.

Files: `engine/a_durable_calls_event_is_durable_before_the_call_returns_test.go`.

---

### 3.330 cleat's own spans were in a different trace from the transaction they orchestrated — ✅ **FIXED 2026-09-16** (cleat#1669)

`WorkflowSpan` attached the caller's trace with `trace.WithLinks`. That does not make `Start` adopt
it: the span opened as a **new root in a new trace-id**, with the caller reachable only by following
a link. Meanwhile `plugin.SetTraceparentFromContext` sends the **inbound** trace-id downstream.

So a collector held the caller's spans and the downstream service's spans correctly joined in one
trace, and cleat — the thing in the middle that orchestrated both — in another. `WHERE trace_id = T`
returned both ends and not the middle.

**Measured before the change**, in-memory exporter, exact spans:

```
INBOUND  traceparent   00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01
OUTBOUND traceparent   00-4bf92f3577b34da6a3ce929d0e0e4736-8f8109e582e314e2-01
cleat's own spans       trace bd8e8ad6c644dfd249c381fea1b0d1c1   <- a different trace
```

`ContextWithRemoteSpanContext` makes `Start` adopt it. The per-worker tree underneath was always
correct — `workflow.execute` over the `event.*` spans — and is unchanged; this attaches it to the
right root.

**The parent span-id is still fabricated, and that is §1597 rather than this.** The inbound parse
keeps the trace-id and discards `parts[2]`, so there is no real caller span-id to name. A collector
renders a parent it never receives as a second root *within* the trace — a far smaller loss than a
missing trace. "Show me everything in this transaction" now answers; "what called what" across that
one edge still does not.

**Sampling is unchanged, and it is asserted rather than argued.** `TraceFlags(1)` is inert on a link
and decisive on a parent — the default sampler is `ParentBased(AlwaysSample)` — so the same constant
changed job. Hardcoding sampled preserves today's outcome exactly, because a root span under
`ParentBased` already falls through to `AlwaysSample`. Forwarding the caller's real flags would be a
behaviour change and is not available anyway: they are discarded at the inbound parse, which is the
reason `SetTraceparent` already gives for hardcoding `01` outbound.

**The link is dropped rather than kept beside the parent.** It would point at the same trace by a
*different* fabricated span-id — `spanContextFromTraceID` mints a fresh random one per call — so it
would be a self-referential edge to a second span that also does not exist.

Four assertions, all on what a collector **receives** rather than on what the code appears to do,
which is the distinction that earned its place here: the defect was invisible from the inbound parse
the originating issue quoted, because `WithLinks` is three frames away from it.

| assertion | what it stops |
|---|---|
| cleat's span carries the caller's trace-id | the defect itself |
| the parent is marked **remote**, and is not the caller's real span-id | "joined the trace" confused with "started inside an ambient local span" |
| a run with no inbound trace still gets a valid trace and no parent | scheduled and swept work becoming orphans |
| a malformed inbound trace-id is not adopted | inventing a trace nobody is in |

**Falsified** by reverting the one line to `WithLinks`: RED, naming both trace-ids.

Section number taken by hand as 3.330 — `scripts/next-section-number.sh` reads `origin/develop` and
returned 3.329, which is claimed by the still-open #1680. WORKSTREAM.md's protocol table describes
exactly this case.

Files: `internal/telemetry/tracing.go`,
`internal/telemetry/a_workflow_span_joins_the_callers_trace_test.go`.

---

### 3.331 cleat named a parent span that does not exist, and §3.330 moved that lie somewhere it looks true — ✅ **FIXED 2026-09-16** (cleat#1669, corrected)

`spanContextFromTraceID` invented a random span-id so the caller's trace could be attached. Before
§3.330 that was invisible: cleat's spans sat in a trace of their own, so there was nothing in the
tree to be wrongly parented. **Joining the caller's trace made it visible and worse** — every cleat
span then hung off a span-id that does not exist and never will arrive, in a trace that otherwise
looks complete.

**A zero span-id still joins the trace.** That is the fact that makes the fabrication unnecessary,
and it is documented SDK behaviour rather than a quirk — `otel/sdk@v1.44.0/trace/tracer.go:97`:

```go
// If there is a valid parent trace ID, use it to ensure the continuity of
// the trace. Always generate a new span ID ...
if !psc.TraceID().IsValid() { tid, sid = ...NewIDs(ctx) } else { tid = psc.TraceID() ... }
```

It branches on `psc.TraceID().IsValid()`, **not** `psc.IsValid()`. Measured:

```
                   trace-id                          parentValid  parentSpan
workflow.execute   4bf92f3577b34da6a3ce929d0e0e4736  false        0000000000000000
event.call         4bf92f3577b34da6a3ce929d0e0e4736  true         <workflow.execute>
```

Right trace, no parent, subtree intact, still sampled — `ParentBased` treats an invalid parent as a
root and falls through to `AlwaysSample`, so the outcome is unchanged.

**Four states, not two**, which is the framing that made the cheap fix visible:

| | trace | parent | what a collector is told |
|---|---|---|---|
| before §3.330 | cleat's own | none | two unrelated traces |
| after §3.330 | the caller's | **invented** | a parent that never arrives |
| **now** | the caller's | none | cleat is a root *within* the transaction — true |
| cleat#1597 | the caller's | the real caller | the actual edge |

§3.330's comment said *"one fabricated parent is the honest minimum; two is noise"*. **That was
wrong — the minimum is none**, and the sentence is corrected in place rather than left to be read.

**The existing assertion was weak and could not have caught the regression it was written for.**
`a_workflow_span_joins_the_callers_trace_test.go:91` asserted the parent is not the **caller's**
span-id — which a freshly-invented **random** one also satisfies. Strengthened to assert the span-id
is invalid at all, with distinct messages for the two ways it can become valid: the caller's real
one means cleat#1597 landed and the test should be flipped, anything else means the phantom is back.

**A pre-existing test had to be rewritten rather than made to pass.**
`TestSpanContextFromTraceIDValid` asserted `sc.IsValid()`, which is false without a span-id — so it
was, precisely, a test that a parent had been invented. It is now
`TestSpanContextFromTraceIDCarriesATraceAndNoSpan`, pinning the valid trace-id and the absent
span-id separately.

**Falsified** by reinstating a fabricated span-id: both tests go red, naming it.

This does **not** close cleat#1597, which remains gated by its author pending evidence that the
missing caller→cleat edge is actually missed. What it removes is the falsehood; the edge is still
absent and now says so.

Files: `internal/telemetry/tracing.go`, `internal/telemetry/telemetry_test.go`,
`internal/telemetry/a_workflow_span_joins_the_callers_trace_test.go`.
### 3.261 A Python guest's clock and entropy were the host's, not the workflow's — ✅ **FIXED 2026-09-16** (cleat#1410)

componentize-py's CPython satisfies `time.time()`, `random.random()` and `os.urandom()` through WASI
Preview 2 interfaces — `wasi:clocks/wall-clock` and `wasi:random/random` — which cleat did not
register on the component linker. So they reached the host's real clock and real entropy, and a
replay diverged.

#### The gate question was answered by running a guest, not by reading its imports

Prior sessions established that `wasi:clocks/wall-clock@0.2.9` appears in a real guest's **import
section**. That establishes *reachable*, not *used* — and the issue's own history records why the
distinction matters: on the preview1 side, concluding "Go guests import `poll_oneoff` without
calling it" from three zero-count runs made an OOM-killed workflow report `result="ok"`.

Two executions of one workflow id, durable clock pinned to 2001-09-09T01:46:40Z:

| | before | after |
|---|---|---|
| `h.now()` (cleat's own) | `1e12` / `1e12` | same |
| `h.random()` (cleat's own) | identical | same |
| `time.time()` | **1.7895622924e9 / 1.7895622951e9** | the pinned value |
| `random.random()` | **0.367… / 0.357…** | reproducible |
| `os.urandom(8)` | **715cc1e5… / fd6da07d…** | reproducible |

**The first two rows are why the other three mean anything.** They are cleat's host calls, read by
the same guest in the same two runs, and they were already stable — so the divergence was the guest
reaching past cleat, not a harness that reproduces nothing.

#### The work was the value layer, which is why the issue was retitled

The linker seam already existed — `allow_shadowing` on, `add_wasip2` before cleat's own
registration. What did not exist was any way to construct the values:

| function | returns | constructor |
|---|---|---|
| `wall-clock.now()` | `datetime { seconds: u64, nanoseconds: u32 }` | a record — none, but `component_val_set_call_failed` is a worked example one level deeper |
| `random.get-random-bytes(len)` | `list<u8>` | **none anywhere in the tree** |
| `random.get-random-u64()` | `u64` | existed |

A component list is a **vec of `wasmtime_component_val_t`, one full val per byte** — not a byte
buffer — so it costs 16 host bytes per guest byte, and `get-random-bytes` is bounded host-side
because the guest supplies the length.

Ownership runs the opposite way to the neighbouring string helper's first impression: wasmtime
converts a callback's results **by reference and then drops them recursively**, so every buffer is
C-allocator heap because wasmtime frees it. Static storage would be a `free()` of a literal. That is
recorded above `component_val_set_call_ok`, in a comment that notes it previously said the opposite.

#### The one-function slice would have compiled, registered, passed and achieved nothing

`get-random-u64` is the function that looks like the RNG. CPython seeds the Mersenne Twister from
`os.urandom` at import, so shadowing the scalar alone leaves `random.random()` live while every
check of the shadowed function passes.

#### The version is part of the name, and a wrong one is silent

Measured by setting `wasiDeterminismVersion` to `@0.2.0` against a guest importing `@0.2.9`:
**registration returns no error** and the guest keeps the real clock. That is the known-positive for
the tests — a case already proven broken, checked to confirm they report it — and they do, on both
the clock and the entropy.

A separate "the registered names match the guest's imports" guard was planned and **not built**: a
mismatched name produces exactly that failure, because the tests assert the effect rather than the
registration, so the guard would add a clearer message and no detection. The message went into the
failure text instead, with the `wasm-tools` command that confirms it.

#### Not shadowed: `monotonic-clock`

cleat#1386 measured why — a durable-sourced monotonic clock ran 3 GC cycles instead of 17 and
reached a 256 MB heap against a 32 MB limit. It is *also* the interface whose functions return
resource-typed pollables, so leaving it alone is both correct and the cheap option; the coincidence
is stated in the code, because the cheap reason would otherwise read as the whole reason.

---

### 3.332 The Python container recipe named a docker context that cannot connect on this machine — ✅ **FIXED 2026-09-16** (cleat#1694)

Four live instruction files told a reader to run `docker --context desktop-linux …`. Measured
today:

| | |
|---|---|
| `docker --context desktop-linux ps` | **FAILS** — `failed to connect to the docker API at unix:///Users/rcownie/.docker/run/docker.sock` |
| `docker run -v "$PWD":/src -w /src alpine:3` | `go.mod` **visible**, 63 entries |

So the prescribed flag does not merely name the wrong runtime — **it cannot work here at all**,
because Docker Desktop is not running. A reader following the recipe got a connection error, not a
subtly wrong tree. The platform change behind it is §3.328's correction: this machine runs OrbStack
and has no colima.

**The fix is not deleting the flag.** `scripts/tier-gate.sh` said *"--context desktop-linux is the
whole fix"*, and that explained a **real** defect: colima bind-mounts these paths as an empty
directory *without failing*, so the run dies with `go.mod file not found` and reads as a broken
checkout. Deleting the flag without replacing the reasoning loses why it was ever there, and the
hazard returns for anyone who runs colima again.

So the named runtime is replaced by **the property it was standing in for** — does your runtime
actually bind-mount this path? — with the two runtime observations kept as dated evidence rather
than as instructions:

```
docker run --rm -v "$PWD":/src cleat-py-toolchain test -f /src/go.mod
```

Verified against the real toolchain image, **with a negative control**: exit 0 on the repo path,
non-zero on a path the runtime will not mount. A check that cannot fail would be worse than the
instruction it replaces.

This is the same move CLAUDE.md prescribes for counts, applied to configuration: publish the
predicate, not the census. *"The instruction that was right in August could not work in
September"* is exactly what a named-runtime instruction buys.

**Scope was narrower than the issue's title, and that is measured rather than assumed.** The grep
returns **16 lines across 6 files**, but two of those files are history, not instruction:

* `IMPROVEMENT-PLAN-CLOSED.md` is an archive under WORKSTREAM.md R3 — editing it rewrites history.
* all six `IMPROVEMENT-PLAN.md` hits sit inside **closed** sections (§3.205, §3.306, §3.308, each
  🟢 **FIXED** and dated), which are history for the same reason.

That leaves **four** live files, not the five a first pass suggests or the six the count implies.
`engine/python_all_host_calls_test.go`'s mention is a doc comment rather than a code gate — checked
— so nothing here changes behaviour.

Files: `scripts/docker/python-toolchain.Dockerfile`, `scripts/tier-gate.sh`, `WORKSTREAM.md`,
`engine/python_all_host_calls_test.go`.

---

### 3.333 The cancelled-twin detector answered "no twin" three ways — ✅ **FIXED 2026-09-16** (cleat#1703)

`CLAUDE.md` published the detector twice and `WORKSTREAM.md`'s R9 candidacy test a third time, all
in the name-based form:

    gh run list --commit <sha> --json name --jq '.[].name' | sort | uniq -d

Three failure modes, measured 2026-09-16 against `431737a0fc24f6571d72114cc3c02c39383263cb`
(cleat#1355, `BLOCKED` for 50+ samples) as known-positive and
`cab6353741afd57203d338f06b79b24334baee34` (cleat#1699, merged an hour earlier) as negative
control. **All three report the safe answer**, which is the asymmetry *Is this result real?*
already names — a measurement error that flatters is one nobody re-derives.

| | what it does | why it is silent |
|---|---|---|
| an **abbreviated** SHA | returns **0 runs**, so `uniq -d` is empty | `head_sha=` is an exact string match; `total_count` is 0, not an error |
| **`uniq -d` over names** | reports `CLA Assistant` as a twin | `pull_request_target`'s `closed` type fires a second run at merge, by design |
| **`?per_page=100`** | 38 cancelled reported of **121** check runs | the page cap truncates and says nothing |

**The first is the expensive one, and not because a prefix is an unreasonable thing to paste.** The
sibling endpoint in the same API family resolves one perfectly well — `…/commits/<abbrev>/check-runs`
and `…/commits/<full>/check-runs` both return 49 on cleat#1699's head — so the surrounding practice
actively teaches that abbreviations are fine here. Every publication site spelled the argument
`<sha>`, and `git log --oneline` and `git rev-parse --short` are what hand you one.

**The second lands at the worst available moment.** Correlating `merged_at` against CLA run times
for four PRs merged that day:

| PR | `merged_at` | CLA runs on that head SHA | delta |
|---|---|---|---|
| #1695 | 16:01:30Z | 15:24:29Z, **16:01:33Z** | +3s |
| #1698 | 16:08:30Z | 15:34:01Z, **16:08:33Z** | +3s |
| #1700 | 16:41:38Z | 16:02:55Z, **16:41:41Z** | +3s |
| #1699 | 17:36:12Z | 16:59:38Z, **17:36:14Z** | +2s |

Four for four, both members `success`. So a watcher polling to `MERGED` sees a "twin" on the very
last sample it takes — this session's own watcher printed `twin='CLA Assistant,'` on the line that
read `MERGED`, and survived only because it happened to test `MERGED` first. The duplicate is
invisible on an **open** PR, which is why cleat#1688's mechanism section recorded "plus one
`pull_request_target` for CLA Assistant" — singular, and correct, because those three had not
merged. A detector whose false positive appears only at the finish line is one you cannot discover
by watching it work.

**The fix is to ask about the conclusion rather than the name** — it names the hazard instead of a
proxy for it, resolves an abbreviated SHA, and has no benign-duplicate class:

    gh api --paginate "repos/<o>/<r>/commits/<FULL-40-char-sha>/check-runs?per_page=100" \
      --jq '.check_runs[].conclusion' | sort | uniq -c

| | known-positive `431737a0…` | control `cab63537…` |
|---|---|---|
| `cancelled` | **55** | none |
| outcome | `BLOCKED` 50+ samples | merged |

Run verbatim under `bash -c`, per *"run it the way the reader will run it"*.

**What this does NOT claim.** The truncation was found on one SHA and the CLA correlation on four
PRs from one day; neither is a claim about other repositories or other workflow sets. And
cleat#1688's own body still carries the name-based form — it is not mine to rewrite, so the
measurement went there as a comment instead.

Files: `CLAUDE.md`, `WORKSTREAM.md`.

### 3.262 A run's live token stream works on every worker — ✅ fixed in cleat#1639

`GET /api/workflows/{id}/stream` (#1572) held its live tail in memory on the worker executing the
run, so a request landing anywhere else got the durable history and then silence. Behind a load
balancer with N workers that is roughly (N-1)/N of readers.

**Routing was priced first, because the issue's own question 4 said the polling mode was redundant
if routing was feasible.** It is not feasible, and the reason is a measurement rather than an
effort estimate:

| | |
|---|---|
| `admin.workers` columns (migration 076) | `worker_id, hostname, pid, concurrency, connection_budget, started_at, last_heartbeat_at` |
| a port or scheme among them | **none** — "membership, and nothing else" |
| `--api-addr` default | **empty**, and it is a BIND address (`:8080`) |

So a worker can hold a live tail while serving no HTTP at all, and no dialable URL can be derived
for one that does. Redirecting needs the client to reach an individual worker, which behind a load
balancer is exactly what is not true; proxying would be cleat's first worker-to-worker link and
would turn an honest degraded stream into a hard failure whenever the owning worker is unreachable.

**The answer was already in the tree, and it is neither of the issue's two options.**
`engine/store_notify.go` and `cmd/cleat-worker/notify.go` solve "one worker must learn promptly
about another's work" as *poll for correctness, NOTIFY to collapse the latency where the dialect
has it* — `mysql_store.go:70` and `mssql_store.go:181` already carry the disabled `notifyChannel`
with that reasoning. This change lands the polling half, which is the correctness floor and works
on all three dialects; NOTIFY is a follow-up, not a prerequisite.

**One correction to the issue's costing, and it changed the design.** The issue priced this as
"one query per reader per interval". The query the handler had was `LoadEventHistory` — the WHOLE
history, 31 columns, decrypt and redact per row, no step predicate. Three runs, 20 reps, postgres
16:

| chunks in run | `LoadEventHistory` | `LoadStreamChunksAfter`, steady state |
|---|---|---|
| 100 | 2.46 / 3.25 / 3.06 ms | 0.95 / 1.12 / 0.75 ms |
| 1000 | 12.49 / 9.54 / 10.91 ms | 1.91 / 0.68 / 1.19 ms |
| 5000 | 42.55 / 39.53 / 44.74 ms | 1.05 / 0.87 / 1.24 ms |

The full read is linear; the cursor read is **flat** — its spread within one history size is as
large as its spread across all three. One reader polling the full read on a 5000-chunk run would
spend ~17% of a core. It needs no new index: `idx_event_history_tenant_wf` is an exact prefix
match, and `EXPLAIN` reports 2-3 buffers with execution at 0.016-0.018 ms.

**The number worth carrying forward is that execution is 2% of the cost.** A poll measures 0.7-1.9
ms in Go against a 0.016 ms query, because `beginTxWithRLS` makes it BEGIN + `set_config` + SELECT
+ COMMIT — four round trips, one carrying data. At 1024 readers and 250ms that is ~4k polls/sec but
~16k round trips/sec, so the ceiling is a connection-pool question, not a query-cost one.

**Three things found while building it, none of them the subject:**

- **`replay` had two definitions in `stream-tokens-to-a-client.md`, in consecutive sentences** —
  provenance ("came from event_history") and novelty ("a re-sent token from a new one"). They
  already disagreed before this change: history past a reconnecting reader's cursor is emitted
  `replay: true` and that reader has never seen it. Resolved towards provenance, which is what the
  code does, and the doc now says to dedupe on `step`.
- **#1572 took a hub slot for a subscription that could never deliver.** It subscribed whenever a
  hub existed rather than when this worker owned the run. Measured by reverting the condition:
  `hub.Readers()` reads 1 for a reader of another worker's run.
- **A refusal test that hangs reports the clock, not the guard.** The ceiling test used a bare
  `<-done`; with the ceiling removed the handler streams forever, so falsification cost 362
  seconds and produced `panic: test timed out` naming nothing. With a 5s `waitDone` it is 5.6
  seconds and names the guard. Every test here whose subject is a refusal now uses it.

**Falsified, ten mutations, each red for its own reason** — the durable tail never chosen, the
cursor not carried, `?mode=live` ignored, the ceiling never refusing, the status never read, an
unknown mode ignored, a non-executing reader subscribing anyway, an inclusive cursor (all three
dialects), no event-type filter (all three), and `-1` clamped to 0.

Re-derive the costing:

    go test ./engine/ -run TestTheStreamChunkTailIsAnExclusiveCursorOnEveryDialect -count=1 -v
---

### 3.334 The test-only-code guard reported OK and exited 0 when it could not install staticcheck — ✅ **FIXED 2026-09-16** (cleat#1707)

`scripts/check-test-only-code.sh` exists to catch a vacuous pass. It had one. With the tool
uninstallable it printed its own error and then passed:

    $ GOPROXY=off ./scripts/check-test-only-code.sh ; echo "exit=$?"
    go: honnef.co/go/tools/cmd/staticcheck@2026.2.1: module lookup disabled by GOPROXY=off
    ERROR: could not install honnef.co/go/tools/cmd/staticcheck@2026.2.1
    OK: no new test-only code (0 known entries in the baseline).
    exit=0

**The author had already written the hazard down, in the same function.** Sixty lines below the
defect, `scan()` carries a comment explaining that `exit` cannot work there — *"scan runs inside a
command substitution, so exit would only leave the subshell and the caller would carry on with an
empty result and report OK — a vacuous pass by the guard against vacuous passes"* — and a
`SCAN_FAILED` sentinel built for exactly that. The install path a few lines up used a bare
`exit 1`. So this is not a missing insight; it is one path that did not get the insight.

**What decides it is a shell option, and that is what makes the blast radius small.** An `exit`
inside `$( … )` ends only the subshell; whether the parent then stops depends on `-e`:

| | parent after `v="$(f)"` where `f` exits 1 |
|---|---|
| `set -uo pipefail` (this script) | **continues, exits 0** |
| `set -euo pipefail` | dies, exits 1 |

A survey of every `scripts/*.sh` for a command-substituted function containing a bare `exit`
returned exactly two: this one and `check-unreachable-main.sh`. **The second is not affected** —
it sets `-e`, *and* its caller rejects an empty scan explicitly. It was checked rather than
assumed, and the mechanism table above is why it could be cleared without a second fix.

The repair is the sentinel the file already defines. `SCAN_FAILED` also moved above `scan()`,
since under `set -u` a reference before assignment is fatal and the install path now uses it.

**The regression test is a `--self-test`, following the convention of
`check_migration_numbers.py` and `check-required-contexts.py`, wired in CI ahead of the real run.**
Two details are load-bearing:

  * **It forces the failure with an empty `GOMODCACHE` as well as `GOPROXY=off`.** Proxy-off alone
    is not deterministic: where staticcheck is already in the module cache `go install` succeeds
    offline, and the self-test would quietly stop exercising the path it exists to exercise —
    passing, of course.
  * **Both assertions are on PRESENCE.** A non-zero exit alone cannot separate *"the guard failed
    for the right reason"* from *"the harness never started"*. The `ERROR: could not install` line
    is the evidence the install path was reached; the exit status is only meaningful once it is
    there. (The obvious control, `PATH=/nonexistent`, hides the shell itself and produces silence
    that reads identically to success.)

Falsified by restoring the bare `exit 1`: the self-test fails, names cleat#1707, and prints the
captured `OK … exit=0`. The presence assertion still passed during that run, which is how the
failure is known to be the mutation rather than a dead harness. Restore verified by content as its
own step, per *Ground rules for changes*.

Files: `scripts/check-test-only-code.sh`, `.github/workflows/ci.yml`.

---

### 3.335 One contract for the non-workflow entities, enforced by total coverage — ✅ **GUARD LANDED 2026-09-16** (cleat#1702)

The design was approved by the repository owner on 2026-09-16: `created_at`, `updated_at`,
`disabled_at TIMESTAMPTZ` as the single retirement spelling, and none of the four older ones. This
section covers the **guard and its grandfather list**; the migrations that make members conform
land one at a time after it, `workflow_schedules` last because it carries an API break.

**The class is thirteen, not eleven, and the two missing ones were found by applying the issue's
own rule instead of reading the list it produced.** The rule — *does the row carry `workflow_id`,
and does it carry `expires_at`* — reproduces its own first claim exactly: two tables carry both,
`concurrency_keys` and `idempotency_keys`. It then does **not** yield the stated membership. 17
carry neither, against a list of eleven. Four of the six extra are correctly out (the run table,
two stats tables, `admin.workers`). Two were missed:

| | migration | shape |
|---|---|---|
| `admin.tenant_egress_allow` | 079 | `tenant_id`, `host`, `created_at` |
| `public.tenant_domains` | 080 | `hostname`, `tenant_id`, `created_at` |

**Note the migration numbers.** These are the two newest entities before `tenant_secrets` at 081,
which *is* in the list. So this was not a stale corner — the list was assembled from what came to
mind, and what came to mind omitted the most recent arrivals. The staging plan's "new entities
conform immediately" would have started from a baseline that already excluded them. And it
flatters: an undercount makes the conversion look 18% smaller than it is.

**Membership cannot be derived structurally, and that is what decided the design.** It was tested
rather than assumed: `admin.tenant_egress_allow` and `public.tenant_domains` are column-identical
to `public.workflow_routing` and `public.workflow_tags`, both members. No predicate over columns
separates them, so membership is a semantic judgement.

A guard built on a *members list* is therefore only ever as complete as whoever wrote the list —
and that list had already gone wrong by hand once. So the guard asserts **total coverage**: every
table it finds in the migrations must be classified `member`, `exempt` or `not-an-entity`, and an
unclassified table is an error. Entity number twelve inherits the rule without anyone rewriting the
eleven, which was the stated requirement, and the specific omission above becomes impossible rather
than merely corrected.

**Two of the eight named in the issue are in the `admin` schema** — `admin.tenant_api_keys` and
`admin.tenant_roles`. Addressed as bare names they resolve to nothing in `public` and a guard
reports clean: the zero-members trap arriving through the *name* rather than through the count.
The registry is qualified throughout.

**Exit status is three-valued on purpose**: 0 conforming, 1 a violation, **2 the scan could not
establish what it was measuring**. A guard that cannot parse its input must not be able to report
what a clean tree reports.

**Three things the falsification found that reading did not:**

  * **A stale registry entry crashed the clause loop** with a `KeyError` instead of reporting the
    stale name. Caught by the self-test on its first run — a guard that dies gives a traceback
    where the finding should be.
  * **Vacuity was checked after the ceiling**, so grandfathering the whole tree tripped the
    ceiling and returned 1. Exit 2 means *this told you nothing*, and it was unreachable in the one
    case it exists for. Reordered.
  * **Alignment padding in the registry made two readers disagree.** The file was tab-aligned for
    readability; `read_tsv` drops empty fields and read it correctly, while
    `awk -F'\t' '$2=="member"'` returns **zero** rows, because `$2` is a padding tab. This is not
    hypothetical — it silently emptied a mutation *during this guard's own falsification*, and the
    resulting red was read as the mutation working. The file now uses exactly one tab and the guard
    **refuses** consecutive tabs, so the trap is a check rather than a comment.

That third one is the section's own subject turned on itself: a census that disagreed between two
readers, inside the guard written to stop censuses disagreeing between two readers.

**Coverage today: 17 of 40 clauses enforced** (10 members × 4 clauses, 23 grandfathered). The
grandfather list was generated from the guard's own parse rather than typed, so it cannot disagree
with what the guard checks, and its ceiling lives in `check-entity-contract.py` rather than in the
list — growing it is an edit to a different file that a reviewer sees.

**Scope limit, stated rather than left to be discovered:** the guard reads
`migrations/postgres/` only. Postgres is the reference dialect for this contract and
`TIMESTAMPTZ` is a Postgres spelling. A member that exists only in the MySQL or SQL Server
migrations would not be seen. Nothing here claims otherwise.

Files: `scripts/check-entity-contract.py`, `scripts/entity-contract.tsv`,
`scripts/entity-contract-grandfathered.tsv`, `.github/workflows/ci.yml`.

---

### 3.336 A table defined in only one dialect was invisible, not unclassified — ✅ **FIXED 2026-09-16** (cleat#1719)

§3.335's guard asserts total coverage over `migrations/postgres/` and states that limit. It bites
once today: `admin.rls_predicate_form` exists only in `migrations/mssql/`, so the guard never saw
it. Invisible is worse than unclassified — the whole design turns on an unknown table being an
error, and this was the one place it could be silent instead.

**The right verdict was already known, which is the argument for fixing it now.** It is a
single-row config table — `only_row BIT`, `CHECK (only_row = 1)` — read from
`engine/mssql_schedules.go:912` in dialect-specific code, because SQL Server has no equivalent of
the Postgres predicate mechanism. `not-an-entity`. So the parity code could be verified against a
case whose answer was settled rather than written alongside a judgement call.

**Membership is compared on the BARE name, and that is the substantive finding.** MySQL cannot
express a schema: it writes `CREATE TABLE IF NOT EXISTS tenants` where Postgres and SQL Server
write `admin.tenants`, and `grep -c 'admin\.' migrations/mysql/*.sql` returns **0**. Comparing
qualified names reports **twelve** differences — the same six tables in both directions — every one
spurious, which would bury the one that is real.

That is §3.335's schema-qualification trap arriving by the opposite route. There,
`admin.tenant_api_keys` addressed bare found nothing. Here, qualifying what cannot be qualified
manufactures gaps. Twice in one day in opposite directions, so the lesson is *schema qualification
is dialect-dependent*, not either individual fix. Bare-name keying is sound only while bare names
are unique, which the guard now asserts rather than assumes.

**A correction to the census that prompted this, because it is the trap generalising.** The
reported table counts were 25 / 24 / 24. They are **23 / 23 / 24**. The Postgres 25 counted two
comments:

    001_schema.sql:6           -- All CREATE TABLE statements include the final column set.
    032_drop_tenant_...:22     -- ... rather than the CREATE TABLE text in

`statements` and `text`, read as table names — in the same message that warned about a MySQL
`guards` table coming from `-- CREATE TABLE IF NOT EXISTS guards idempotency.` A regex that cannot
model SQL comments reads prose about a definition as a definition, in whichever dialect it is
pointed at. Both guards strip comments before matching.

**A second defect, which the fix itself exposed.** §3.335's staleness check compared the registry
against Postgres tables only. Classifying `admin.rls_predicate_form` correctly then reported it as
*"no longer exists"* — the guard refused the fix for the hole it had just reported. Staleness now
spans every dialect.

**And a prediction of mine that measurement killed.** I claimed §3.335's plain-and-quoted
identifier pattern would match nothing in `migrations/mssql/`, parse to zero tables, and report
clean — the zero-members trap a third time. **False.** Reverting the pattern still parses all 24,
because no `CREATE TABLE` in this repo quotes its identifier in any dialect:

    # NOT this -- it is line-anchored and cannot see a name on a continuation line:
    #   grep -rhcE 'CREATE[[:space:]]+TABLE[^(]*[][`"]' migrations/$d/*.sql
    python3 - <<'EOF'
    import glob, re
    pat = re.compile(r"CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?([^\s(]+)", re.I)
    for d in ("postgres", "mysql", "mssql"):
        q = 0
        for f in glob.glob("migrations/%s/*.sql" % d):
            src = re.sub(r"/\*.*?\*/", "", open(f).read(), flags=re.S)
            src = "\n".join(re.sub(r"--.*$", "", l) for l in src.split("\n"))
            q += sum(1 for m in pat.finditer(src) if re.search(r'[\[\]`"]', m.group(1)))
        print(d, "quoted identifiers:", q)
    EOF
    # 0, 0, 0 on 2026-09-16

The widened pattern stays, because all three quotings are legal and a scan that cannot read one
parses to nothing rather than failing. But it is **defensive, not a fix**, and the code comment
says so. The first version of that comment asserted the bug was real; it had been reasoned from
`[dbo].[x]` appearing in *queries* rather than checked against the migrations.

**And the zero itself needed a second measurement before it meant anything.** The command first
published for it was `grep -E 'CREATE[[:space:]]+TABLE[^(]*[][`"]'`, which is **line-anchored**, so
it scores **0** on a file that does exactly what it looks for:

| file | both define `[dbo].[workers]` | that grep |
|---|---|---|
| name on a continuation line | yes | **0** |
| name on the same line | yes | 1 |

So *"0 quoted identifiers"* and *"0 quoted identifiers I could see"* rendered identically. What
turns the first into an answer is a separate check the instrument could not make about itself —
**no `CREATE TABLE` in the tree puts its name on a later line**, 0 across all three dialects. Raised
by a peer session scanning the same tree with a statement-aware parser and a positive control over
all four quotings; the conclusion held and the instrument did not deserve to be believed alone.

This is the same shape as the mutation check below, one level out: there the precondition is *did
the mutation apply*, here it is **could this instrument have disagreed**. The guard's own parser
does not share the defect — Python's `\s+` spans newlines, verified on the same two fixtures — so
only the published command was blind, which is the worse place for it, because a command in a
comment is what the next reader runs.

**The widened pattern is a control rather than a hope, and that was checked rather than asserted.**
Dropping the bracket and backtick alternatives fails four self-test cases, each reporting
`parsed 0 tables` — the zero-members signature. So it cannot silently regress to matching nothing.

**What caught the prediction was verifying the mutation applied before reading its result** — the same
discipline §3.335 records, arriving one step earlier. The first falsification of that pattern
returned exit 0 and the natural reading was "the trap is real and the guard now covers it". The
mutation had applied; the prediction was simply wrong. An assertion that the anchor matched and
the file changed is what separated the two.

**And the inverse of that trap was found in the same function, twice.** `strip_sql_comments`
modelled comments and not string literals — a tool applied to a format it does not model, which is
what this guard exists to catch. Both demonstrated through `parse_tables`, the real consumer, not a
proxy:

| fixture | committed (two regexes) | this branch (a walk) |
|---|---|---|
| `DEFAULT 'see migration 064 -- nothing to sync'` | `disabled_at` **gone**, `errors=none` | all three columns |
| `DEFAULT 'engine/*.go'` + a later `*/` | **NO TABLES**, unbalanced-paren error | both tables |

**The first is the dangerous one and it points at §3.335's own subject.** `disabled_at` is the
column cleat#1702's conversion adds to thirteen tables. The guard would have reported *"table X has
no disabled_at"* — blaming the schema for a fault in its own parser — under precisely the
migrations it exists to check.

**The second was one unrelated edit from firing.** The old code applied the `/* */` rule first,
over the whole file with `re.S`, before the per-line `--` rule ran, so a `/*` inside a line comment
was unprotected. `migrations/postgres/072:18` and `migrations/mysql/070:62` each contain one, inert
only because neither file contains a `*/`. Appending one ordinary block comment to 072 took its
stripped length from **375 characters to 18**. A defect armed by an edit elsewhere in an unrelated
file arrives with nothing connecting it to its cause.

Neither fired today: old and new parses of all three dialects are **identical** in table names and
column sets. Raised by a peer session; the fix is a walk that copies `'...'` and `$$...$$` bodies
through verbatim, and two self-test cases pin it.

**And a third defect in the same walk, dialect-independent: an unterminated `/*` swallowed the rest
of the file and reported nothing.**

    unterminated /*   ->  tables=['public.gadgets']   errors=[]

`public.widgets` is simply absent. The loop exits on `i >= n` with `depth` still 1 and nothing
downstream learns the walk ended inside a comment. That is worse than a wrong count under this
section's own logic — a table missing from one dialect classifies as *"defined in only one
dialect"* — and **Postgres and SQL Server both reject such a file**, so the guard would report a
clean, complete schema for a migration the database will not run. Running off the end inside a
comment is now an error.

**The nesting comment beside it was wrong, and it was corrected by measurement rather than
recall.** It claimed Postgres nests and SQL Server does not, and said counting was "harmless
elsewhere". Run against live engines, `/* see engine/*.go */ SELECT 1 AS survived;`:

| engine | result | nests? |
|---|---|---|
| postgres 16 | `ERROR: unterminated /* comment` | **yes** |
| mysql 8.0 | `1` | **no** |
| mssql 2022 | `Msg 113 … Missing end comment mark '*/'` | **yes** |

Inverted for SQL Server, and it omitted the one dialect that actually does not nest. The second
sentence was falsified by the case that started this: a comment whose *text* contains a glob is
unnested in MySQL's reading and depth 2 to the counter, so it does not "close at depth 1 either
way". Per-dialect counting would be airtight and is not worth it now that the residue is loud.
A plain `/* … */` was run on each engine first as a positive control.

**And the diagnosis that fix emits was itself wrong for one dialect.** It said *"Postgres and SQL
Server both reject such a file"* unconditionally. True for those two; **false for MySQL, which is
the dialect that produces it.** Measured directly:

    /* see engine/*.go */ CREATE TABLE widgets (id INT PRIMARY KEY);
    mysql 8.0     table created            -> the file is VALID
    postgres 16   ERROR: unterminated /*   -> the file is rejected

MySQL does not nest, so it closes that comment at the first `*/`. On a MySQL migration the file is
fine and **the scanner is what disagrees with the engine** — and the message sent its author to
audit a correct migration. A failure message that asserts a cause nobody checked is the same fault
as a check that cannot fail, moved one step downstream: the run it fires on need not be the run it
describes. The hint is now chosen by the directory being read, which also documents the residue
left by not doing per-dialect counting, at the only place anyone will meet it.

**The falsification of that fix is the clearest case in this section for asserting on TEXT and not
only on status.** With the unterminated check disabled, both new self-test cases still exit 2 —
*"parsed 0 tables"*, the right status for entirely the wrong reason. Only the assertion that the
output says `never closed` tells them apart. A status-only self-test would have passed a guard that
had lost the check.

**A proxy disagreed with the real consumer while this was being checked, which is the section's own
lesson once more.** The first faithful comparison scored columns with a line-oriented regex over
the stripped text, and it reported `disabled_at` as *present* under the broken version — because
the truncated line leaves `disabled_at` intact as a line, while `parse_tables` splits on top-level
commas and glues it onto the previous column. Convenient instrument, wrong answer, in the direction
that said there was no bug.

Coverage: 23 Postgres tables, 23 MySQL, 24 SQL Server; 17 of 40 clauses enforced, unchanged — this
adds membership reach, not clause reach. Clause checks remain single-dialect by design.

Files: `scripts/check-entity-contract.py`, `scripts/entity-contract.tsv`,
`.github/workflows/ci.yml`.

---

### 3.337 A condition that never decides anything cannot be observed to be wrong — ✅ **RECORDED 2026-09-16** (cleat#1723)

Graduates the finding of cleat#1719/#1723 to `CLAUDE.md`, per WORKSTREAM R3. It is a fourth entry
under *"could this check have disagreed?"*, and it differs from the three already there: those are
checks that gave the **wrong** answer. This one gives the **right** answer every time, because
something else is answering.

Four instances in one day, three inside a single PR:

| the check | why its verdict was right | what was actually deciding |
|---|---|---|
| self-test for an unterminated `/*` | exit 2, as asserted | the vacuity check — a swallowed file leaves 0 tables |
| self-test for a dialect-specific hint | it errored, as asserted | the error fired; only its *explanation* was wrong |
| a watcher's "nothing pending, nothing red" | never merged early | `mergeStateStatus` refusing first, every time |
| a scan for quoted identifiers | reported 0, and 0 was right | the tree happens to put every name on one line |

**Two remedies, and they are not the same one.** Assert on the **text**, not only the status —
disable the check under test and the first two cases still exit 2, because a correct second
mechanism supplies the expected status. And gate on a **denominator the run cannot shrink**: the
reconstruction on cleat#1718's head found a 24-second window where 2 of an eventual 49 check-runs
existed, both complete and non-red, with **0 of 32** required contexts green.

**Where to look is the actionable half.** Not the checks you doubt — the ones that have never yet
refused anything. The watcher's green-set assertion had run on every PR of this session with
`mergeStateStatus` refusing ahead of it every single time.

**A number in the new text was imprecise and was corrected before merge**, which is the section's
own rule biting its own paragraph. It said "a whitespace split reports 56" without saying which
split: `set(...split())` gives 56, a raw token count gives 112, and the answer is 32. All three
commands are now published beside their results.

Files: `CLAUDE.md`.

---

### 3.338 A resolver that answers every time is a lookup of who talks most — ✅ **RECORDED 2026-09-16** (cleat#1715)

Graduates to `CLAUDE.md` the attribution lesson from mis-crediting cleat#1715's claim, per
WORKSTREAM R3. It sits under *"Claim outright or not at all"*, because claiming only works if the
next reader can resolve **whose** claim it is, and every marker that would let them is optional at
the point of writing.

**The measurement that makes it a rule rather than a resolution.** Resolving the claimant of the
ten open issues two ways:

| how the claimant is resolved | result |
|---|---|
| any `Claude-Session` id present in the thread | a confident id for **10 of 10** |
| the marker on the claim comment itself | a marker on **4 of 19** claim comments; UNKNOWN for 15 |

The honest resolver declines four times out of five; the presence-based one never declines. The
known-positive was #1717, whose claimant I had been told independently — presence returns the wrong
session for it.

**Three scans, three ways to be wrong, and one anchor that is wrong in neither direction.** Measured
over every commit message in `develop` at `b6e88452`:

    B=$(git log --format=%B origin/develop)
    grep -cE '^Claude-Session:' <<<"$B"                                       # 741 — the answer
    grep -cE '^Claude-Session:[[:space:]]*https://claude\.ai/code/' <<<"$B"   # 735 — loses 6
    grep -oE 'session_[A-Za-z0-9_]+' <<<"$B" | sort -u | grep -c .            # 10 — invents 5

The 6 it loses are one participant's consistent habit, not a uniform miss rate. Among the 5 it
invents is `session_01` — a real id, truncated, quoted in `74b6bcd0`'s own message as it removes a
literal `"session_01..."` ellipsis from a doc comment.

**Two further traps recorded with it.** `git log` indents bodies by four spaces, so the correct
field anchor returns **0 of 741** if pointed at `git log` rather than `git log --format=%B` — a
blank that reads as "nobody here uses session trailers". And a squash concatenates its
constituents' bodies: 121 of the 554 trailered commits carry the marker more than once, every one a
single id, which a per-line count reads as multi-session collaboration.

**Why no hook fixes this.** `CLAUDE_CODE_SESSION_ID` is exported to hooks but is a UUID, and 0 of
the 741 trailers use that form; the id they do use lives in `CLAUDE_CODE_BRIDGE_SESSION_ID`, whose
presence depends on how the session was launched. A hook keyed on the obvious name emits a
well-formed marker that resolves to nothing and passes every scan above.

Files: `CLAUDE.md`.

### 3.263 A worker that cannot serve a run releases it instead of destroying it — ✅ fixed in cleat#1710

Both pre-flight checks in `executeWorkflow` ran **after** the claim and answered
`recordTerminalFailure(..., engine.ErrPermanent, ...)`. So a worker that could not serve a run
claimed it and killed it. A pool where 3 of 10 workers satisfy a workflow's `plugin_deps` did not
run it at 30% throughput — it permanently failed roughly 70% of its runs, decided by claim races.

**The predicate, which is the part worth keeping: a pre-flight check that fails on a WORKER-LOCAL
fact must release, not terminate.** The discriminator is where the fact lives, not how serious it
is:

| | example | verdict |
|---|---|---|
| run-intrinsic | a malformed def, an undecodable input | terminate — wrong on every worker |
| worker-local | `w.plugList`, `wfMeta` from the loaded binary | **release** — a sibling may serve it |

The issue was filed against the plugin check alone. The version check one line above has the same
shape and the same consequence — `wfMeta` is read from the binary *this worker* loaded, which
during a rolling deploy is exactly as worker-local as its plugin list — so fixing one and leaving
the other would have produced a guard that looks complete and is not. Both converted.

**What happens when the satisfying worker never arrives, stated because the failure this replaces
was at least visible.** The run does not circulate hot: `ReleaseWorkflow` writes `next_wake_at` on
the **row**, so the backoff throttles the whole pool rather than one worker — a run nothing can
serve costs one claim-and-release per interval *cluster-wide*, stays `ready`, keeps its history,
and is visible to `ListWorkflows` and to a log line naming the unmet requirement.

That quiescent stuck state is deliberate, and the argument against bounding it is already written
down in this repo for `ReclaimCount` (`engine/store_types.go`): the count exists so an operator can
*see* a loop and is deliberately not a bound, because "dead-lettering past a threshold would turn a
node being redeployed into permanent failure of a workflow that did nothing wrong". A rollout
window is precisely when a threshold would destroy the work it is meant to preserve. No counter and
no threshold were added; a bound needs persisted per-run state, and §1702 is sequencing that shape.

**The finding worth more than the fix: five behavioural tests of the new helper cannot see the
bug.** They exercise `releaseForAnotherWorker`, and the defect lives at the *call site* —
`executeWorkflow` needs a real WASM binary to reach either check, so no unit test reaches them.
Measured by reintroducing the exact bug with the AST guard excluded:

    F1b  the #1710 bug, AST guard excluded  ->  ok  (all five green)
    F1   the #1710 bug, AST guard included  ->  FAIL "plugin_check" is passed to recordTerminalFailure

So `TestNeitherPreflightCheckTerminatesTheRun` is the only thing standing between the repo and a
silent regression, and it keys on the op strings because that is what such a regression has to
carry. A test that cannot fail on the bug it was written for is the thing this plan keeps
rediscovering; here it was five of them, and only the deliberately different reading caught it.

Falsified: both checks reverted to terminating (red on the guard, green without it), the backoff
removed, and the zero-backoff fallback removed.

    go test ./cmd/cleat-worker/ -run TestNeitherPreflightCheckTerminatesTheRun -count=1

### 3.339 A figure derived from the inputs cannot notice that the run skipped work — ✅ **FIXED 2026-09-17** (cleat#1730)

`check-entity-contract.py`'s clause loop skipped any member missing from the reference
dialect's parse, on a comment that was true only sometimes:

    for table in members:
        if table not in tables:
            # Already reported as stale above.
            continue

`stale` is computed against the union of all three dialects' bare names, so a member whose
Postgres `CREATE TABLE` the scan failed to read — while MySQL or SQL Server still defines it —
is **not** stale, is skipped past every clause, and the guard exits 0.

Measured by making `workflow_tags`'s Postgres CREATE unmatchable and leaving the siblings
intact, with the mutation asserted applied before the result was read:

| | clean | one member unparsed in Postgres |
|---|---|---|
| `tables parsed` | 23 | **22** |
| `clauses enforced this run` | 17 of 40 | **17 of 40** |
| verdict | `OK`, exit 0 | `OK`, exit 0 |

**The second half is the one worth carrying.** `clauses enforced this run` existed to stop a
run that agrees with every schema because it checked nothing — the §3.335 vacuity gate. It
could not, because it was arithmetic over two TSV files:

    enforced = len(members) * len(CLAUSES) - len(gf)

Whether the loop evaluated a single column never reaches it. `CLAUDE.md` says to gate on a
quantity **the run cannot shrink**; this gated on one **the run cannot touch**, which is the
degenerate case — a restatement of the inputs wearing the costume of a measurement. The
distinction is not academic: the figure is the only thing standing between a partial run and a
green one, and it was inert.

The repair is to count what the loop evaluated and report *that*; the arithmetic becomes an
expectation to disagree with rather than the answer. A member absent here but present in a
sibling is now named at exit 2. `ALTER_ADD_RE`/`ALTER_DROP_RE` also take the same `IDENT`
alternation `CREATE_TABLE_RE` uses, and an ALTER naming an unparsed table is recorded rather
than discarded — the widening closed a gap that discarded nothing (0 unattributable ALTERs in
all three dialects at `b6e88452`), and the recording is the half that catches the form nobody
thought of.

**Three self-test cases, each falsified by reverting ONE part at a time.** Reverting the whole
fix goes red and reads as confirmation of all of it; reverting one part at a time is what
attributes each case to its own mechanism:

    revert widening (ALTER_ADD_RE grammar)      exit=1  names its own case
    revert recording (else: errors.append)      exit=1  names its own case
    revert unparsed-member detection            exit=1  names its own case
    restored                                    26/26 pass, rc=0

**One branch is untested and says so in the source.** The `evaluated != expected` backstop is
unreachable by any fixture — every current skip is either stale or unparsed — so by this repo's
own rule it has never been observed to be wrong. It was observed deliberately by injecting a
skip the `unparsed` list cannot see, and the injection and its output sit in the comment beside
it, with instructions to delete the comment when someone makes it fixture-reachable. A recorded
observation is weaker than a test and much stronger than a branch nobody has run.

Found while prototyping a different fix. The original report (the `ALTER` identifier asymmetry)
overstated its own impact in the flattering direction, and the correction is in cleat#1730's
comments rather than silently edited away.

Files: `scripts/check-entity-contract.py`.

### 3.266 A guard printed a count it had never fetched, so the one context it could not see gated every merge for three days — ✅ **FIXED 2026-09-20** (cleat#1937)

`scripts/check-required-contexts.py` is the guard over `tiers.yaml: required_contexts`, the
in-tree record of what blocks a merge into `develop`. On 2026-09-17 `Web Dashboard` was added
to branch protection. Every run of the guard between then and 2026-09-20 ended:

    check-required-contexts: OK, 32 required contexts declared and consistent with
    tiers.yaml and .github/workflows/.

while GitHub required 33.

**Nothing in that sentence is false, and it cannot be read correctly.** It means *32 things are
declared and those 32 are internally consistent*. It reads as *32 is the number* — a numerator
whose denominator the default path never fetched. The comparison against branch protection
lived in `--check-live`, a separate mode, and the script said so about itself in its own
docstring ("WHAT IT CANNOT CHECK ... whether the declared list still equals what GitHub
actually requires"). A limitation written in a docstring does not reach the person reading the
success line.

So `Web Dashboard` gated every PR into `develop` as must-pass for three days with no `covers:`
classification and no `why_required` — which is, word for word, the state §3.402 created this
block to end: *"a tier-2 package held to must-pass is a fine thing to decide and a bad thing to
discover."* The drift arrived through the one gap that section had recorded in itself and left
open.

#### The decision #1937 deferred, and how it was resolved

The report stopped short of choosing whether `--check-live` should run by default, on the
grounds that it needs network and admin scope. Resolved as: **the default run attempts the read
and says which of the two happened.**

* It **fails on a disagreement** — the live list and the declared one differing is a real
  finding whoever is looking.
* It **never fails on an inability to read.** `GITHUB_TOKEN` has no admin scope, so CI will
  normally land here, as will a fork PR and an offline laptop. A guard that goes red because
  the network was slow teaches people to re-run rather than to read; §3.401 removed a required
  check's wall-clock assertions for exactly this reason.
* When it cannot read it prints `NOT CHECKED`, names why (`gh: Bad credentials (HTTP 401)`,
  `--no-live was passed`, `gh is not on PATH`), and prints the date the two lists were last
  compared. **The count is never handed over unqualified.** That is the half that works with no
  credentials at all, and it is the half that matters: the failure here was not that nobody
  could check, it was that the output did not say nobody had.

`fetch_live` and `diff_live` are split so the comparison is a pure function over two sets.
`--self-test` falsifies it with literals on any machine — a self-test that skips when a
credential is absent prints the same thing whether it worked or never ran, which is this
script's own docstring about itself.

#### Verified by putting the defect back

The declaration was reverted to its pre-fix state (`tiers.yaml` and
`.github/required-checks.txt` at `2b9adac1`, so `total: 32` still matched its own list and only
the new check could fire) and the new guard run against live branch protection:

    check-required-contexts: FAIL
      required on develop but NOT declared in tiers.yaml: 'Web Dashboard'

One finding, from the one check that is new. Restored, all four modes green:
default-with-scope (`requires exactly these 33`), `--no-live`, `--check-live`
(`33 contexts, declared list matches exactly`), and `--self-test` — 8 negative controls, 2 new
ones for the live diff, and a positive control for each half.

#### And the new output was wrong on its first CI run, in its own subject

The `NOT CHECKED` block prints why the live list could not be read, taken from
`gh`'s stderr. The first version took the LAST line. In the `Lint` job `GH_TOKEN`
is not set at all, so `gh` answers with a four-line hint whose last line is a
YAML fragment — and the guard printed:

    check-required-contexts: NOT CHECKED -- whether branch protection on develop still
      requires exactly these 33. Reading it needs admin scope, and this run did not read it:
            GH_TOKEN: ${{ github.token }}

A variable name where a reason belongs, in the paragraph this whole section is
about. Fixed to prefer the line carrying an HTTP status and otherwise the first
line, as `gh_error_reason`, which is pure and has four self-test cases — three
of them built from the actual stderr shapes rather than invented. The third case
puts the HTTP line **between** a preamble and a hint, so neither `lines[0]` nor
`lines[-1]` satisfies it: reverting to either is red, which is what makes the
case set pin a rule rather than a position.

Worth recording rather than quietly amending: the defect was found because the
change made the guard print something in CI that nobody had seen before. An
output nobody reads cannot be wrong in a way anyone notices, which is most of
why the original `OK, 32` survived three days.

#### The second finding: the count was written down eleven times

`grep -rn '32 required contexts\|32 contexts'` found the number asserted in nine workflow
files, `docs/project/release-process.md`, and `tiers.yaml`'s own header — none of them
generated, all of them wrong. `.golangci.yml` already records what two copies of one fact do
("two mechanisms with two baselines, which is the shape that let the routing tables in 2.72
drift apart"); eleven copies drift eleven ways.

They are not guarded, they are **deleted**. The nine workflow comments said "There are 32
required contexts on develop and every one of them has to report here" — the number was
decorative and the sentence is true without it. `release-process.md` points at
`tiers.yaml: required_contexts` instead of restating a total. The count now lives in exactly
two places that cannot disagree: `required_contexts.total`, which check 4 compares against the
length of the list below it, and the guard's own output, which computes it.

Historical counts in `CLAUDE.md` and §3.402 are left alone. "0 of 32 required contexts green"
is a record of a measurement on a date, not a claim about today, and rewriting it would destroy
the evidence to tidy a number.

Files: `scripts/check-required-contexts.py`, `tiers.yaml`, `.github/required-checks.txt`,
`docs/project/release-process.md`, nine files under `.github/workflows/`.
