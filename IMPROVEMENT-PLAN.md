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

### 1.5 Primary WASM backend has no hang protection (~1–2 sessions) — 🟢 **CLOSED for deployments; developer tooling is the residual** (WS-3, 2026-09-02)

> **Re-opened and re-closed 2026-08-04 by §2.28.** The epoch-interruption fix below is real
> and tested, but it lived behind `//go:build cgo` and the shipped Dockerfile built with
> `CGO_ENABLED=0`, so no container had it: measured on the wazero backend the containers
> actually ran, a workflow with a 2-second budget ran for 2m35s and returned **success**.
> The image now builds with CGO on a glibc base and a `--verify-backend` build step keeps it
> that way.
>
> **2026-09-02: the residual this heading carried was stale, and it named a file that no
> longer exists.** It read *"Go guests are fenced in deployments; non-Go guests on wazero
> still are not."* There is no wazero backend to be unfenced on — `engine/backend_wazero.go`
> was deleted in #459 (2026-08-10), three weeks before this sentence was last read as current.
> Four things now hold, each re-derivable:
>
> | claim | command |
> |---|---|
> | no wazero backend exists | `ls engine/backend_wazero.go` → no such file |
> | all five shipped guest languages route to wasmtime | `grep -n 'WasmtimeLanguages =' engine/engine.go` → `go, assemblyscript, java, rust, python` |
> | the worker registers that backend and no other | `grep -rn 'WithBackends' cmd/cleat-worker/` → one site, `setup.go:1624` |
> | an unrouted language is refused, not fallen through | `Engine.resolveBackend`, `engine/executor.go:75` (#503) |
>
> The fence itself is not per-language: `configureStore` (`engine/backend_wasmtime.go:227`)
> applies `SetEpochDeadline` to every store before `Execute` branches on language at all. So
> in a deployment a guest either runs on wasmtime and is fenced, or is refused. **There is no
> third path**, which is what closes this for deployments — not a per-language test.
>
> **The real residual is developer tooling, and it is a different and much smaller thing.**
> `engine.Runtime` is still a wazero runtime and still executes guest code under
> `cleatctl replay`, `cleatctl debug`, `cleat run_embedded`, `cleat-bench` and
> `cleat/wasmtest`. Re-derive with
> `grep -rn "engine\.NewRuntime(" --include="*.go" . | grep -v _test.go` — **7** call sites
> (measured 2026-09-02), all of them CLI or test tooling, none in `cmd/cleat-worker`.
>
> Use that command and not the looser `grep -rn "NewRuntime("`, which prints **12**: it also
> catches `engine/runtime.go`'s own definition, two in-package `RunDefer` call sites in
> `engine/executor.go` that fire only when no backend is registered — those same tools — and
> a `wazero.NewRuntime` in the AssemblyScript test runner, which is a different symbol that
> merely ends in the same name. The first draft of this paragraph paired the loose command
> with the tight count, so anyone re-deriving it would have got 12 and concluded the note was
> wrong.
>
> wazero **cannot** be fenced for a compute-bound guest (CLAUDE.md records the three
> approaches that were tried and failed), so a runaway guest under `cleatctl replay` is not
> stopped. That bounds a developer's terminal, not a deployment, and removing wazero to close
> it was decided against on 2026-09-01 (§3.56).
>
> Read §2.28 for the deployment history.


**Everything below this line is the original 2026-08 report, kept for its history and no
longer describing the tree.** Two of its premises have since been falsified rather than fixed:
wazero is not "a fallback for languages wasmtime cannot host" — it is not a backend at all
(#459) — and wasmtime is no longer configured without a `Config`. Read it as the record of why
the fence was built, not as a description of what is there.

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

### 1.6 Generation not bumped on reap or terminate — ✅ **FIXED** (marker added 2026-08-06)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

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

  ~~SQL Server is written but **skipped, blocked on §2.71** — see below. Unskipping it is that
  item's acceptance test.~~ **Running since at least 2026-08-31.** Its only skip is the
  environmental `CLEAT_TEST_MSSQL not set`; `tiers.yaml`'s tier 1 includes `./cmd/...` and
  `tier1-gate.yml` sets that variable, so `TestTenantIsolationOverHTTP_MSSQL` runs on every PR.
  Confirmed from a real gate log rather than inferred — `ran=7056 pass=7054 fail=0 skip=2`, both
  skips the allowlisted `TestHelperProcess`, printed by name.
- ~~The ~89 unaudited `MySQLStore` `s.tenantID` call sites (see the `requireTenant` note
  elsewhere in this plan). Scoping the store does not audit them.~~

  **Not open — closed by D1, and this bullet contradicted the DECIDED block ~150 lines below it
  for 25 days.** `tiers.yaml` records `multi-tenancy-mysql` as
  `NOT SUPPORTED — single-tenant only`: "a documented product boundary, not an open engineering
  item". Auditing those sites *for tenant isolation* is work toward a feature the product has
  declined, and this bullet is what sends a reader at it — measured, on 2026-08-31, by a reader
  who read the bullet, stopped, and proposed the work.

  **What is genuinely left is smaller and differently shaped.** `requireTenant` is not a
  multi-tenancy guard; it is a wrong-answer guard, and it earns its place in a *single*-tenant
  deployment. An empty `tenantID` produces `WHERE tenant_id = ''`, which matches nothing and
  errors on nothing, so a query with no identity reads to the caller as "this tenant has no
  data". Whether every method that needs that guard has it is worth knowing on its own terms —
  it is just not the 89-site tenant-isolation audit this bullet described.
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

  > **DECIDED, 2026-08-06 — the rule is "multi-tenancy requires database-enforced RLS."**
  > PostgreSQL and SQL Server qualify and are supported multi-tenant. **MySQL is documented
  > single-tenant-only.** This closes §1.7's residual as a product decision rather than
  > leaving it as open engineering.
  >
  > A mechanism for MySQL does exist and was prototyped before deciding, so the decision is
  > against a measured alternative rather than an assumed one. MySQL refuses a user variable
  > in a view (`ERROR 1351: View's SELECT contains a variable or parameter`), but a view over
  > a stored function that reads the session variable works, and `WITH CASCADED CHECK OPTION`
  > enforces the write side. Measured on MySQL 8.4, 50k rows, all four properties held:
  >
  >   * SELECT isolation — each tenant sees only its own rows
  >   * cross-tenant INSERT — refused, `ERROR 1369: CHECK OPTION failed`
  >   * UPDATE reassigning `tenant_id` — refused, same error
  >   * DELETE of another tenant's row — 0 rows affected
  >
  > Renaming the physical tables and giving the views the original names would even leave the
  > existing SQL untouched. **The cost is what kills it: 200 full scans took 0.368 s against
  > the table and 2.238 s through the view — 6.1×**, because the stored function is evaluated
  > per row and cannot be hoisted (it reads session state, so it is not deterministic). For
  > comparison, SQL Server's native predicate costs **+20%** on the same shape of query
  > (§3.37). Paying 6× on every scan to emulate a feature the engine does not have is a worse
  > product than saying plainly which engines support the feature.
  >
  > Re-derive: the probe is not checked in — it was a scratch database, dropped after
  > measuring. The mechanism is four statements (function, view, `WITH CHECK OPTION`, session
  > variable) and takes about ten minutes to rebuild if anyone wants to challenge the number.

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

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

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

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 2.14 `cleat_json_parse` / `cleat_json_stringify` panicked on the primary backend — FIXED

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

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

> **Residual sharpened 2026-08-04 by reading the call sites, not the type.** The entry below
> says the seven `ErrorCode` values have no path into history. True, but the reason is more
> specific than "nobody supplies them", and there is a live trap in it.
>
> `CleatError` *does* participate in retry classification — it implements `RetryableError`, and
> `isDefinitelyNonRetryable` honours it through `errors.As`. So the plumbing is not missing. What
> collapses the taxonomy is the implementation:
>
> ```go
> func (e *CleatError) Retryable() bool { return e.Code == ErrTransient }
> ```
>
> Seven values, one bit, and **everything that is not `ErrTransient` reads non-retryable** —
> including `ErrUnknown`, which is the zero value, so a `CleatError` built without a `Code` is
> silently non-retryable.
>
> ~~**The trap is `ErrTimeout`.** It reads non-retryable, while the doc comment on the
> constructor immediately above it says `NewTransientError` creates a retryable error "(DB
> connection, **timeout**)". So the package documents timeouts as the canonical retryable case
> and classifies `NewTimeoutError` as non-retryable. An external caller gets the opposite of
> the documented behaviour, silently.~~
>
> **Wrong — retracted the same day, and kept here because it went out in a commit message
> before I checked.** The two are different concepts, not one concept classified two ways. The
> const block says so directly:
>
> ```go
> ErrTransient  // retryable (DB connection, timeout)
> ErrTimeout    // execution timeout
> ```
>
> `ErrTransient` covers a *network or database* timeout, which is retryable. `ErrTimeout` is
> the *workflow execution* timeout — the run exceeded its budget — and retrying that is exactly
> wrong. `TestCleatError_Retryable` already asserts it, deliberately, in a subtest named
> "timeout is not retryable". The classification is correct and there is nothing to fix.
>
> I reached the wrong conclusion by reading the two doc comments and not the word *execution*
> in the second. The tell I ignored was the existing test: a behaviour with a subtest asserting
> it is a decision, and the first question is what the decision was for, not whether it looks
> odd next to its neighbour.
>
> **In-repo only three of the six constructors are ever produced:** `NewPermanentError` and
> `NewTransientError` (`cmd/cleat-worker/setup.go`, the shipped `ServiceCaller`) and
> `NewCancelledError` (`engine/mssql_errors.go:229`). `NewTimeoutError`, `NewAmbiguousError` and
> `NewRetriesExhaustedError` have no non-test caller. That is *not* a dead-code finding —
> `scripts/check-test-only-code.sh` deliberately does not flag exported identifiers in library
> packages, and says so — but it does mean the effective in-repo taxonomy is three-valued, and
> that the three unused codes have never been exercised against a real retry decision.
>
> **What actually survives**, once the retraction above is taken out: the observations are
> right and the conclusion drawn from them was not. `Retryable()` does collapse seven values to
> one bit; `ErrUnknown`, the zero value, does read non-retryable — which is the conservative
> choice for a durable engine and defensible rather than defective; and three of the six
> constructors do have no in-repo producer, which is explicitly *not* a dead-code finding.
>
> None of that is a new defect. It is a more precise restatement of what this entry already
> said: **the full code has no path into history.** That remains the real residual, and it is
> schema work — persisting `error_code` per event so replay can recover the classification
> rather than re-deriving one bit of it. There is no cheap version hiding underneath.


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

> **The streaming half is fixed — 2026-09-03 (WS-2), and the asymmetry was wider than this
> paragraph says.** It is *three* of the four causes, not one. Measured by running both paths
> against the same condition:
>
> | condition | `PluginCall` | `PluginCallStreaming`, before |
> |---|---|---|
> | no registry configured | `callErrorUnknown` | `callErrorUnknown` ✅ |
> | function not registered | `callFailureCode` (retryable) | `callErrorUnknown` ❌ |
> | call guard rejects | `callFailureCode` (retryable) | `callErrorUnknown` ❌ |
> | the function returns an error | `callFailureCode` (retryable) | `callErrorUnknown` ❌ |
>
> So the identical deployment gap, or the identical plugin error, was retryable to a workflow
> *calling* a plugin and permanently fatal to one *streaming* from it.
>
> **What the fix stores is the code, not the cause.** `EventRecord.StreamErrCode` holds what the
> guest was actually told, so replay hands back a value rather than re-deriving a
> classification. A stored *cause* would still need a cause-to-code mapping on the replay side,
> and changing that mapping later would silently change the retryability of steps already
> recorded — which is the exact determinism bug the paragraph above is guarding against.
>
> **Why this did not break workflows in flight.** The key is written only when non-zero, so
> every chunk already in `event_history` keeps its byte-identical payload and its stored
> checksum, and decodes to 0 — which *is* `callErrorUnknown`, which is exactly what those
> failures reported when they were fresh. Determinism is per recorded step, not across eras: an
> event recorded before the change replays the way it always did, and only events recorded from
> now on carry the corrected code.
>
> **It was unfixable until §3.96 landed**, because the error branch could not fire on replay at
> all: `StreamFinish` was persisted by neither the payload nor a column, so a recorded stream
> failure came back as an ordinary chunk and replayed as a *success* whose content was the error
> text. Fixing the classification of a branch that never ran would have been a change with no
> observable behaviour and a test that could not fail.
>
> `freshPluginCallStreaming`'s four sites now go through one function, `streamFailure`, which
> records and returns from a single `code` argument. Four sites each writing a record and then
> packing a constant is four chances for those to drift, and the drift would not surface as a
> broken test — it would surface as a workflow that retried on the first run and gave up on the
> replay. `TestStreamFailureRecordsTheCodeItReturns` asserts the two halves agree.
>
> **Still open in this section:** the `ServiceCaller` half — the seven `ErrorCode` values and
> the one bit they collapse into. Nothing here changes that. What changed is that the streaming
> family no longer contradicts the non-streaming one while we decide.
>
> Not attempted: giving the causes *truthful* codes — `NotFound` for an unregistered function,
> `PermissionDenied` for a guard rejection. Both exist in the guest enum and both are
> non-retryable, and both would be better than `Unavailable`. But the non-streaming path reports
> `callFailureCode` for those same two conditions, so doing it here alone would re-open the
> asymmetry pointing the other way, and doing it on both paths changes the retry behaviour of
> workflows in flight. That is a separate decision about both paths at once, not a detail of
> this one.

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

~~**Still open: the richer taxonomy, and plugins.** `Retryable()` is one bit; `ErrorCode` carries
seven values that still have no path into the event history, because
`EventRecord.ErrNonRetryable` is a bool by design (see above).~~ The streaming plugin family
(`recordStreamError`) remains single-coded, and `PluginError` is still a bare string on replay.

**Update (2026-09-02): the taxonomy has its path into history. Plugins are what is left.**

`EventRecord.ErrCode` stores the class the caller supplied — `ErrorCode.String()`, written
through `recordedErrorClass` at both sites that record a call failure
(`durablecalls.go`'s retry loop and `heartbeats.go`'s cancellation path), carried in the
`payload` JSONB under `error_code`, and round-tripped through compaction as `ec`.

**No migration**, for the reason the bool needed none: `payload` is JSONB, so adding a key
changes checksums only for newly written events and existing rows still verify.

Three choices worth recording, because each is the kind that looks arbitrary later:

- **A string, not the int.** `ErrUnknown` is the iota zero value, so an int field could not
  distinguish "no ServiceCaller classified this" from "classified as unknown" — the exact
  collision that kept `ErrNonRetryable` a bool. Empty means nobody said. It also matches
  `workflow_instances.error_code`, which stores these same strings, so one vocabulary spans
  both tables; and it survives a value being inserted into the `ErrorCode` iota block.
- **It does not feed the guest-visible code.** `recordedFailureCode` still reads
  `ErrNonRetryable` and nothing else. **The class and the bit can legitimately disagree**:
  `DurableCallWithRetry`'s `nonRetryableErrors` list comes from the *guest's* retry policy
  across the ABI, so an author can declare a substring non-retryable for an error whose
  `CleatError` says `ErrTransient`. The bit is what the engine acted on, so the bit is what the
  guest is told. Deriving the code from the class would change the retry behaviour of workflows
  already in flight — this section's own determinism bug, reintroduced from the other side.
- **So this is history and operator surface, not a guest-facing change.** Worth being plain
  about: no workflow sees a different code than it did before. What changes is that the class
  survives, so an operator can query for cancelled or permanent call failures instead of
  substring-matching a message, and the guest-facing step is unblocked without a second schema
  decision.

Two comments were corrected in the same change, both true when written and false since:
`callerrors.go` said *"no ServiceCaller in the repo returns anything but a bare fmt.Errorf"*
— `dbServiceCaller` has returned `CleatError` since the update above it — and
`EventRecord.ErrNonRetryable`'s comment said a richer taxonomy "would mean inventing values no
caller supplies". **The first of those cost a session**: it reads as current, it is two
paragraphs above the function it describes, and it argues convincingly for not doing the work
this update just did.

Tests, each verified by breaking the thing it covers: `TestAFreshCallRecordsTheClassTheCallerSupplied`
drives the real retry path and reads the recorded event (the one that fails if the wiring is
removed — every other test here still passes without it),
`TestErrorClassSurvivesThePayloadRoundTrip` through the real encoder and decoder rather than
`json.Unmarshal`, which would test a serialisation the engine never performs,
`TestErrorClassSurvivesCompaction`, `TestLegacyEventHasNoClassAndKeepsItsRetryability`, and
`TestRecordedClassDoesNotMoveTheGuestVisibleCode` — whose breaking check is a compile error
rather than a red test, and says so.

---

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
in the engine and where a deadlock is most likely. Budget is 2 retries at 20ms/40ms
(`mssqlTxRetries`/`mssqlTxRetryDelay`): at most 60ms of added latency on a path that
previously failed outright. A deadlock victim claimed nothing, so the replay is a clean
retry.

**The count in this section was wrong.** There are ~20 transaction boundaries in the MSSQL
store, not 8.

**Second increment, 2026-08-04:** the five terminal writes — `CompleteWorkflow`,
`FailWorkflow`, `MoveToDeadLetterQueue`, `ContinueAsNew`, `FinalizeWorkflowSegment` — now use
the same wrapper. A deadlock on any of them previously lost the workflow's terminal write and
surfaced to the worker as an ordinary error, which `recordTerminalFailure` then reports and
drops (§1.2). These are the highest-consequence boundaries after the claim: the claim losing
a race costs one poll cycle, a terminal write losing one costs the record of the workflow
having finished.

Wrapping them does **not** disturb the fence. `ErrFenceLost` is returned before the commit
and is not an `mssql.Error`, so it falls through the rollback-guarantee check and is returned
on the first attempt — retrying it would be actively wrong, since the fence is lost because
another worker legitimately owns the workflow and that does not change on a second attempt.
`TestWithRollbackGuaranteedRetry_DoesNotRetryFenceLost` pins that, and the pre-existing
`TestFinalizeWorkflowSegment_ZombieWriterFence/mssql` covers the refactor end to end.

Post-commit cleanup is inside the retried closure and that is safe: a rollback-guaranteed
error means the commit failed, so the function returned before reaching the cleanup.

**Third increment, 2026-08-04 — every MSSQL boundary outside §2.60's files.** `mssql_operations.go`
(6), `mssql_deployment.go` (1), `mssql_schedules.go` (3) and the remaining `mssql_lifecycle.go`
boundaries (`ReleaseWorkflow`, `RequestCancellation`, `Heartbeat`, `StartNewRun`,
`enforceParentClosePolicy`). Two are worth calling out on consequence rather than volume:
`ReleaseWorkflowConcurrencyKeys`, where a lost deadlock leaves a concurrency key held until
its TTL and blocks every later run using that key, and `TerminateWorkflow`, where one leaves
a run that the HTTP layer already answered 409 for still runnable — the §1.2 defect fixed in
#263, reachable again through a different door.

**Still to do:** `mssql_events.go` and `mssql_signals_promises.go` (9 boundaries), which
§2.60 (#283) is changing. Do those after it lands rather than into a conflict.

### 2.50 Parent close policy fails silently on all three dialects — ✅ **FIXED**

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 2.27 Two of the three deployment manifests crash-loop on an undefined flag — ✅ **FIXED**

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

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

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 2.42 The AssemblyScript determinism checks had never run — ✅ **FIXED**

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 2.43 `cleat vet --lang as` cannot fail — ✅ **FIXED** (WS-3, 2026-08-04)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 2.60 Per-step event flush never ran on MySQL or SQL Server — ✅ **FIXED** (WS-2, 2026-08-04)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 2.70 Multi-DB CI ran entirely on wazero — ✅ **FIXED** (WS-3, 2026-08-04)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 2.71 MSSQL session context is cleared by connection pooling — 🟢 **BOTH HALVES FIXED, and the per-table coverage residual is closed** (found by WS-3, connection half fixed by WS-1 2026-08-04, schema half 2026-08-31, residual 2026-09-04)

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

~~**Still open:** the test schema is missing two tenant-scoped tables the shipped schema has —
`workflow_routing` and `workflow_tags` — so their policies cannot be applied at all. That set
is now asserted rather than assumed, so a *new* divergence fails the test instead of being
tolerated silently. Pointing `engine/testutil/` at the real migration remains the real fix.~~

**Closed 2026-08-31 — by Stream A, not here, which is why it sat stale.** "Pointing
`engine/testutil/` at the real migration" is exactly what Stream A did: `engine/testutil` now
applies `migrations/mssql/*.sql` through `migration.Runner`, and
`TestNoHandWrittenSchema` (`engine/testutil/schema_source_guard_test.go`) fails the build if a
`CREATE TABLE` literal comes back. There is no longer a "test schema" that can be missing a
table — it is the shipped schema.

Both tables and both filter predicates ship:

    grep -n "workflow_routing\|workflow_tags" migrations/mssql/001_schema.sql migrations/mssql/012_admin_role.sql

`001_schema.sql:465,470` and `012_admin_role.sql:146,151` each add
`FILTER PREDICATE dbo.fn_tenant_filter(tenant_id)` on `dbo.workflow_tags` and
`dbo.workflow_routing`.

> **Closed 2026-09-04 (WS-3): `TestEveryShippedTenantPolicyExistsInTheBuiltDatabase`**
> (`engine/mssql_policy_coverage_test.go`). The residual below described this exactly, and it
> was real — measured before fixing, **eight of the nine shipped predicates could be dropped
> and both existing guards stayed green**.
>
> Both sides are read, neither is listed: the intended set is parsed out of
> `migrations/mssql/*.sql`, the actual set from `sys.security_predicates` in the database those
> migrations just built. A literal list of the nine names here would be a third copy that goes
> stale in the direction that hides the defect — a policy deleted from a migration *and* from
> the list agrees with itself.
>
> Proven able to fail, against a real SQL Server rather than by reasoning: dropping
> `dbo.TenantFilter_Tags` and re-running reported `workflow_tags` shipped-but-absent, and the
> policy was restored immediately. `SetupMSSQLFullSchema` does not heal that — `migration.Runner`
> treats an applied migration as done regardless of what has since happened to the objects it
> created, which is the property this section already documents and the reason the mutation is
> observable at all.
>
> **What it deliberately does not assert**, because the number invites a wrong conclusion:
> measured the same day, **38 tables carry a `tenant_id` column and 9 have a filter predicate**.
> That is not a finding and must not be read as one. §3.86 (🟢, WS-1) covers the layer that
> matters — statement-level tenant predicates in the Go SQL — and records its remaining 27 as
> "an allowlist with reasons, not a backlog". RLS is off entirely for an admin connection, which
> is *why* §3.86 fixed statements rather than policies. This test pins the backstop that exists
> against erosion; it does not relitigate its scope.

~~**One thing this does *not* establish, stated rather than glossed.** That the predicates are
*created* is read off the migration files; nothing asserts per-table policy coverage.~~
`requireMSSQLPoliciesIntact` (`engine/testutil/mssql_schema.go`) counts
`sys.security_policies > 0`, and `mssql_rls_enforcement_test.go` checks `is_enabled` for one
policy by name. A migration that dropped the predicate on `workflow_tags` specifically would
pass both. That is a smaller, different gap from the one this residual described, and it is
recorded here rather than fixed because it wants a live SQL Server — which this machine cannot
run (see the note in `multi-db-ci.yml`'s test-mssql job).

#### Where the switch stands, 2026-08-05 — written, and the blocker is now named

Branch `fix/mssql-test-schema-real-2`, draft PR #333. `engine/testutil` builds the MSSQL schema
from `migrations/mssql/{001,010,011,020}.sql`, fingerprinted so it applies once per database —
001 is not re-appliable, because its own security policies bind `fn_tenant_filter` and its
`CREATE OR ALTER FUNCTION` then fails on the second call.

**What the switch bought on the way**, all merged: §3.16, §3.17, §3.18, §3.19 — four production
defects the hand-written schema's missing constraints had hidden.

**Correction to the previous note in this section.** It said a single failure "moved" between
`TestClaimWorkflow/mssql` and `TestFailWorkflow/mssql` and that the trigger was unexplained.
The trigger is now explained and it was measurement error: those runs were against a database
**poisoned by an earlier version of `mssql_rls_enforcement_test.go`**, which installed the
seven policies and dropped them on cleanup. That was correct while the test schema had none and
became destructive the moment the schema provided them — and because the fingerprint says "these
files have been applied", nothing ever reinstalled them. A database in that state runs every
later test without a backstop, forever. Two guards now exist: that file asserts the policies
instead of installing them, and `applyMSSQLSchemaFile` fails loudly if the fingerprint matches
while the policies are gone, which is the state that cost an hour to recognise.

**The real blocker, which is exactly what this residual predicted.** With the policies genuinely
live, a fresh database gives **141** failures, and they are one cause:

```
setupTestData: CreateSchedule: mssql: Violation of PRIMARY KEY constraint 'pk_workflow_schedules'
store A expected 3 active instances, got 6
```

`CleanupMSSQLTestData` deletes on a plain pool with **no session context**, so the filter
predicate hides every row it is trying to delete and it removes nothing. Rows accumulate across
tests: primary-key collisions in fixtures, and counts that are multiples of what the test
inserted. The residual's own sentence — *"turning them on suite-wide fails every test that
builds a store on a plain pool — that is the point"* — describes this precisely.

**The decision that unblocks it** is what to do about administrative access under RLS, and it
is a real choice rather than a patch:

- have the test cleanup toggle the policies (`ALTER SECURITY POLICY … WITH (STATE = OFF)`)
  around its deletes — supported, but global state that only works because `./engine/...` runs
  with `-p 1`;
- give `fn_tenant_filter` an escape hatch — a sentinel session-context value that means
  "administrative" — which changes the *shipped* predicate and therefore what production
  enforces;
- have every raw-SQL fixture and cleanup set a session context, which is the honest answer and
  the largest, since it means every such site learns which tenant it is acting as.

PostgreSQL sidesteps this because cleanup runs as the owning superuser, which RLS exempts;
SQL Server has no equivalent exemption, which is why the same suite shape works there and not
here.

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

### 3.36 errcheck's 283 findings, triaged — 🟢 **THE DEFECT WAS FIXED in §3.43 (2026-08-31); errcheck itself is still not enabled** (WS-3, 2026-08-05)

The companion to §3.33. `PARALLEL-WORKSTREAMS.md` — retired 2026-09-04, so this is a quotation
from history rather than a pointer — singles errcheck out as **"the class that
produced §1.2 and §2.50"** — both real defects, both literally a discarded error return — so
the question is not whether 283 is a lot but which of them are that class again.

| discarded call | n | verdict |
|---|---|---|
| `tx.Rollback` | 152 | **54% of the total.** `defer tx.Rollback()` after a commit returns `ErrTxDone` by design. The idiom, not a finding |
| `(*json.Encoder).Encode` | 22 | HTTP response writes. A failed write usually means the client left; worth a debug log, not an error |
| `p.db.Exec` | 18 | **database writes, discarded** — plugins/eventtriggers (7), scheduledbackup (6), webhookingest (5) |
| `s.ClearStickyWorker` | 14 | see below |
| `json.Unmarshal` | 14 | proceeding on zero values after a parse failure; worth a pass of its own |
| `fs.Parse` | 13 | `flag.ExitOnError` already exits. Noise |
| `s.ReleaseWorkflowConcurrencyKeys` | 12 | **see below — this is the real one** |
| `w.Write`, `srv.Shutdown`, `db.Exec`, `Row.Scan`, and ~20 others | 38 | long tail; the `Row.Scan` and `ExecContext` members belong with the write group |

**The finding: a failed lock release is invisible on every path but one.**

`ReleaseWorkflowConcurrencyKeys` releases a workflow's concurrency keys — its locks — after a
terminal transition, and `ClearStickyWorker` clears its worker affinity. Neither logs
internally on any dialect; both return an error. The callers do three different things:

| caller | treatment |
|---|---|
| `engine/db.go:1097` (Postgres) | checked, logged at warn ✅ |
| `engine/store_lifecycle.go:268-269, 335-336, 393-395` (Postgres) | `_ =`, silent |
| `engine/mysql_lifecycle.go:271-272, 326-327, 518-519, …` | bare call, silent |
| `engine/mssql_lifecycle.go:334-335, 398-399, 455-456, …` | bare call, silent |

So the same cleanup gets three treatments, and on MySQL and SQL Server a lock that fails to
release does so **without a word**. A concurrency key exists to stop two workflows running at
once; one that is never released blocks its successors until the TTL expires — and §3.34 has
just finished showing what TTL arithmetic on these keys was doing. This is §2.50's shape
exactly ("parent close policy fails silently on all three dialects"), one call over.

**Suggested fix, and why it is small:** each dialect repeats the same three-line block
(`ClearStickyWorker`, `ReleaseWorkflowConcurrencyKeys`, `enforceParentClosePolicy`) at four to
five sites. Collapsing it into one `bestEffortTerminalCleanup(ctx, workflowID)` per store fixes
every site at once, logs consistently, and makes the three dialects structurally identical —
which is the property §1.1 and §2.60 both wished for. `enforceParentClosePolicy` already
returns nothing and logs internally after §2.50, so only the two error-returning calls need
handling.

**Handed to WS-1** rather than patched here: `engine/mysql_*.go` and `engine/mssql_*.go` are
theirs and had three commits land in them today. Same treatment as §3.34, which they picked up
within the hour.

**The handoff was completed, and this heading did not say so for four days.** Recorded
2026-09-04. §3.43 fixed it on 2026-08-31 (#500), in the shape suggested above: one unexported
helper, `releaseWorkflowResources` in `engine/workflow_cleanup.go`, taking a two-method
interface so one copy serves all three stores, warning on each of the two errors. The
parent-close half followed in §3.80 (#583), where a closing parent had been stranding a
concurrency slot per child.

Verified 2026-09-04 rather than taken from §3.43:

```
$ go test -count=1 -run TestBestEffortCleanupGoesThroughTheHelper ./engine/
--- PASS: TestBestEffortCleanupGoesThroughTheHelper (0.03s)

$ grep -rn "releaseWorkflowResources(s.log()" --include="*.go" engine/ | grep -v _test.go | grep -c .
21
```

The guard fails if a new call site reintroduces either dropped-error form, so the three dialects
cannot drift apart again silently. (21 sites, not the 20 §3.43 measured — the count grew with the
codebase, which is what a dated number does.)

**Why this correction is worth its own paragraph.** A 🔶 saying *"handed to WS-1"* is
indistinguishable from open work assigned to a named stream, and it stayed that way for four days
over a body describing a fix that had shipped. This is the failure CLAUDE.md records for §1.1 and
§1.2 — a status marker read as "not started" while the fix sat on develop — with the additional
cost that this one names an owner, so it reads as *someone else's* outstanding task. It was found
by scanning for WS-1's open items and picking this one up to work on.

**What remains open here is the linter, not a defect.** The other groups in the table above were
classified, not fixed, and the classification stands: `tx.Rollback` (152) is the idiom,
`fs.Parse` (13) is noise after `flag.ExitOnError`, and the write-and-lock tail is the part worth
a pass. Nothing in it is a known defect.

**Why errcheck still should not be enabled yet.** 152 `tx.Rollback` findings would have to be
suppressed first, and blanket-suppressing the single most common shape in the codebase to turn
on a linter is how a guard becomes decoration. The honest sequence is: exclude `tx.Rollback` by
rule (errcheck supports an exclude list), fix the ~30 write-and-lock findings, then enable and
see what is left. That is a session, and it is a session with a known payoff, which is more
than could be said before this table existed.

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

### 3.38 `TestAdminForceResolve_*` failed once after an all-dialect run — 🔵 **MECHANISM DEMONSTRATED 2026-09-04; it is §2.60d, and the hypothesis recorded here was wrong** (WS-1, 2026-08-06)

Recorded because it cost twenty minutes to rule out of §3.37 and would cost the next person the
same. Three tests failed in a PostgreSQL-only run of `./engine/...`:

```
TestAdminForceResolve_AuditCollisionRollsBack
TestAdminForceResolve_RefusesAnotherTenant
TestAdminForceResolve_RefusesAnotherTenant/postgres
```

It is **not** a code defect in anything on this branch — the same command gives 0 failures both
on clean `develop` and with §3.37's changes applied. The one failing run is distinguished only
by what preceded it: a full three-dialect `go test ./engine/ ./migration/ ./cmd/cleat-worker/`
against the same shared `cleat` database.

So the hypothesis is cross-run state in the shared database rather than test ordering within a
run. Stated as a hypothesis on purpose: one failing observation against two passing ones does
not establish a mechanism, and the failure was not captured under a debugger. What it does
establish is that these tests are not independent of what ran before them, which for an
admin/tenant test is worth knowing on its own.

Anyone who sees this in CI should suspect the preceding job's residue before suspecting their
diff.

#### The hypothesis above is wrong, and the mechanism is §2.60d (2026-09-04)

**Cross-run residue cannot reach these tests.** `PostgresBackend.Setup`
(`engine/store_backends_test.go:48`) calls `testutil.CleanupPostgresTestData` *before* handing
the store over, and again at teardown. Whatever the preceding job left is deleted before the
test body runs. Demonstrated by trying to plant it — twenty `ready` workflows aged an hour, which
would push the fixture out of `ClaimWorkflows`' `ORDER BY priority ASC, created_at LIMIT 20`
window:

```
$ psql -c "INSERT INTO workflow_instances (id, def_name, def_version, status, created_at, next_wake_at)
           SELECT 'residue-338-'||g, 'admin-force-resolve', 1, 'ready',
                  now() - interval '1 hour', now() - interval '1 hour'
           FROM generate_series(1,20) g;"
$ psql -tAc "SELECT count(*) FROM workflow_instances WHERE status='ready';"    # 20
$ go test -count=1 -run 'TestAdminForceResolve_' ./engine/
ok      github.com/cleat-team/cleat/engine  0.701s
$ psql -tAc "SELECT count(*) FROM workflow_instances WHERE id LIKE 'residue-338-%';"   # 0
```

The residue is gone and the tests pass. That was the second wrong theory in this section, and it
is left in rather than deleted because the *shape* of both is the same: reaching for state
carried between runs, when the setup deletes it.

**What does reproduce it is a concurrent unqualified `DELETE`** — precisely what
`CleanupPostgresTestData` issues, and what another package's `Setup` runs while this one is
mid-test. Racing one against the suite fails the exact tests this section names, on demand:

```
$ ( for i in $(seq 1 400); do psql -q -c "DELETE FROM workflow_instances;"; done ) &
$ go test -count=6 -run 'TestAdminForceComplete_ResolvesAndAudits|TestAdminForceResolve_' ./engine/
--- FAIL: TestAdminForceResolve_AuditCollisionRollsBack
    admin_force_resolve_test.go:447: error = admin force_complete: workflow acr-... not found,
        want it to name the audit event as the reason
--- FAIL: TestAdminForceResolve_RefusesAnotherTenant/postgres
    admin_force_resolve_test.go:388: workflow axt-... not found
--- FAIL: TestAdminForceComplete_ResolvesAndAudits/postgres
    admin_force_resolve_test.go:159: workflow afc-... not found
```

Both tests this section names, and the subtest, from one cause. With the racer stopped,
`-count=3` is green.

**So this is §2.60d, not a defect of its own.** `SuiteTestDB`'s doc comment already states the
diagnosis — *"one suite's teardown deletes another's fixtures mid-test ... the failures are
timing-dependent ... so they read as flakes rather than as one cause, and the standing workaround
is `-p 1`"*. §3.38 is what that reads like from the other end, three weeks before the sentence
was written.

**What the demonstration adds to §2.60d.** Part 2's evidence table concluded, from ten runs, that
*"the failures were stale local state, not package parallelism"* — which was right about those
failures and is not a statement about whether the engine suite is sensitive to a concurrent wipe.
It is, on demand, and now there is a command that shows it. The two findings are consistent: the
mechanism is real and the natural window is narrow, which is exactly why it presents as an
unreproducible flake.

**And the narrow window was measured here too, as a negative result.** Two real packages sharing
the database, run the way a developer would:

```
$ go test -count=1 ./engine/ ./migration/          # no -p 1
ok  github.com/cleat-team/cleat/engine     204.423s
ok  github.com/cleat-team/cleat/migration    2.122s
```

Green. One run is not evidence of safety — `migration` finishes in 2s of a 204s engine run, so it
holds almost no rows across almost none of the window — but it is the honest counterweight to the
racer above, and it is why nobody has been able to catch this by re-running the suite.

**The adoption gap is the open part.** §2.60d part 2 built `SuiteTestDB` and records *"still
open: every other suite that shares the database."* Measured 2026-09-04:

```
grep -rn "SuiteTestDB(" --include="*.go" . | grep -v packagedb        #  9 sites, 2 packages
grep -rln "testutil.TestDB(" --include="*.go" . | sed 's|/[^/]*$||' | sort | uniq -c
     29 engine
      6 engine/testutil
      3 cmd/cleat-worker
      1 each: cmd/cleatctl, tests/{upgrade,soak,scale,plugin-harness,integrity,exhaustion}
```

`engine` — the largest database-backed package, and the one holding these tests — is entirely on
the shared database. Moving it is not a swap: `SuiteTestDB` is PostgreSQL-only by design (part 4
measured that MySQL and SQL Server did not need an equivalent *for the suites that had moved*),
while `forEachBackend` runs the engine suite on all three. That is a piece of work with its own
decision in it, and it stays §2.60d's rather than being restated here.

Until then `-p 1` is load-bearing for `./engine/`, and CLAUDE.md's rule about it is the
protection, not a style preference.

### 3.33 gosec's 283 findings, triaged — 🔶 **2 fixed, 281 classified** (WS-3, 2026-08-05)

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

### 3.70 A WASM defer has no callback to call — 🟢 **FIXED for the normal path; abnormal exit remains** (WS-3, 2026-09-01)

Found while measuring the prerequisite for §3.35 phase 3. It invalidates the premise of that
section's phases 2–5, so it is recorded before any of them are built on it.

**`cleat_defer_<id>` is constructed only on the host side. No SDK, in any language, and no
codegen, ever emits such an export.**

    grep -rn "cleat_defer_" --include="*.go" --include="*.ts" --include="*.rs" \
        --include="*.py" --include="*.java" . | grep -v _test.go

returns four hits, all host: `cmd/cleat-worker/setup.go:479`, `engine/flush.go:505`,
`engine/executor.go:623`, and one comment. The guest side of that name does not exist anywhere in
the repo.

Confirmed by building a real Go SDK fixture — `DurableDefer("release the lock")`, then a durable
call, then a returned error — with `cleat build --target go`. The generated adapter has the
**import** (`//go:wasmimport env cleat_defer`, registration) and the only `//go:wasmexport` is the
workflow entry point itself. Nothing named `cleat_defer_*` is exported.

**What the host sees is worse than "not found".** Measured 2026-09-01 against that fixture on the
wasmtime backend:

    event[0] type=defer deferID="defer-0" desc="release the lock"     <- registration works
    RunDefer("cleat_defer_defer-0")     -> result={"error":"unknown entry point: ..."} err=<nil>
    RunDefer("totally_nonexistent_export") -> result={"error":"unknown entry point: ..."} err=<nil>

A Go guest does not have its exports called by name: the host runs `_start`, the guest asks
`cleat_poll_work` what to run, and dispatches internally. An unknown name comes back as a JSON
error *body* with a **nil** Go error — so `runDefers`'s `if err != nil` never fires and **nothing
is logged at all.** The engine's own log output for that run was empty.

So the state of `defer` in WASM is not "runs without context" (§3.35 finding 2) or "runs twice"
(finding 3). It is: **every defer in every Go WASM workflow silently does nothing, the host
reports success, and a nonexistent export is indistinguishable from a real one.** The control is
the second line above — the two results are byte-identical.

`DurableDeferFunc` is not an escape hatch: the generated WASM adapter wires only `DurableDefer`,
so the closure form returns *"can only be called from within a workflow function (the HostCalls
runtime was not initialized)"* — a message that misdiagnoses its own cause, since the call **is**
inside a workflow function and the real reason is that the closure form is not implemented for
WASM at all.

#### What this does to §3.35

Phases 2–5 of that plan — give the defer body a session, run it on the instance that registered
it, restrict what it may do, make it exactly-once — are all improvements to the *invocation* of a
callback that has no target. They are not wrong, but none of them can be observed until a guest
exports something to call. **Phase 1 (§3.66, a defer surviving a suspension) stands**: it is
about the registration set, which does work.

The prior question is an ABI decision, and it is the author's:

| option | shape |
|---|---|
| Codegen emits `cleat_defer_<id>` | `cleat-gen` generates an export per registered defer. Defer IDs are minted at *runtime* (`defer-<stepCount>`), so the export name is not known at build time — this needs the ID to become a compile-time name, or a single dispatch export taking the ID. |
| One dispatch export | The guest exports `cleat_run_defer(deferID)` and routes internally, which is how entry points already work via `cleatDispatch`. Smallest change; fits the existing Go `_start` + `cleat_poll_work` model. |
| Defers are host-side only | `DurableDefer(description)` becomes a declaration the *host* acts on (release this lock, send this notification), with no guest callback. Removes the ABI problem entirely at the cost of the "arbitrary cleanup code" property §3.35 opens with. |

My recommendation is **one dispatch export**: it matches how the Go guest already receives work,
needs no compile-time knowledge of runtime-minted IDs, and makes `DurableDeferFunc` implementable
by keying a closure table on the returned ID.

#### The dispatch-export option is viable — measured, and against my own reasoning

Recorded because I reasoned my way to the opposite conclusion and was wrong, which is worth more
than the answer.

The Go path calls **only** `_start` (`engine/backend_wasmtime.go`); the entry point is routed
inside `main()` via `cleat_poll_work`, and `_start` returns having trapped on `proc_exit`. From
that I concluded a second export could not run afterwards — the Go runtime is finished — and was
about to report the dispatch-export option unviable. Measured instead, 2026-09-01, by calling two
`//go:wasmexport` entry points on the same store after `_start` returned:

    _start returned err=error while executing at wasm backtrace:
    post-_start "place_order"  -> err=<nil> ret=0x2C_00000001   errCode=1, 44-byte message
    post-_start "cancel_order" -> err=<nil> ret=0x02_00000000   errCode=0, 2-byte result

**Both executed guest code.** The differing error codes and non-zero output lengths are the proof
that matters: a call that merely returned would produce neither. `err=<nil>` alone would not have
been evidence of anything, which is the trap this file's "Is this result real?" section is about
— a call that returns without error is not a call that ran.

So the instance survives `_start`, and a post-hoc dispatch export can be invoked. Two things this
does **not** establish, and which the implementation has to check for itself: `cleat_complete`
was not called during either post-`_start` invocation (both completion variables stayed empty),
so the guest-side SDK context is not in the state the poll-work protocol leaves it in; and
nothing here shows that a closure table populated during the first dispatch is still reachable
from the second call. Both are testable once there is something to test.

**Whatever is chosen, `RunDefer` must stop reporting success for an entry point the guest did not
recognise.** That is true today regardless of the ABI decision and is the smallest independently
shippable piece: a guest that answers "unknown entry point" is a failure, and the host currently
cannot tell it from a completed defer.

Re-derive the measurement: build a fixture calling `h.DurableDefer(...)` under `testdata/`, run
`go run ./cmd/cleat build --target go -o <dir outside the module tree> ./testdata/<fixture>`, and
grep the generated `gen_wasm_exports.go` for `defer` — the only `//go:wasmexport` is the workflow
entry point.

#### Resolution: the guest runs its own defers, and the dispatch export was not needed

The measurement above established that a post-`_start` export **can** run, so the "one dispatch
export" design was viable. Building it showed it was also unnecessary, for a reason neither the
measurement nor the design discussion had reached: **the host does not reuse that instance.**
`RunDefer` calls `backend.PerExecution().Execute(ctx, wasmBytes, deferName, …)`, which compiles
and instantiates from bytes — a fresh instance every time (`engine/executor.go`). A closure lives
in the memory of the instance that registered it, so no export name, dispatched or otherwise,
can reach it from there. The live-module path in `invokeDefersOnTrap` (`engine/flush.go`) does
hold the right instance, but invokes by the same `cleat_defer_<id>` name, which no guest exports
and none can — the ID is minted by the host at runtime and an export name is fixed at compile
time.

So the fix is on the other side of the boundary. The guest already has the closures at the moment
a defer is for, and now runs them itself:

- `DurableDeferFunc` is wired into codegen for the first time. It was absent from the `wasm`
  package's `hostFunctions` table, so the generated adapter never set the `HostCallsOptions`
  field and the method returned *"the HostCalls runtime was not initialized"* — text that blames
  the caller for calling it from the wrong place. Verified by mutation: removing the table entry
  reproduces that exact string.
- `_cleatRegisterDefer` stores the body under the ID the host minted, so both sides key the same
  defer identically.
- `_cleatRunDeferred` runs the bodies in LIFO order once the entry point has finished, on the
  error path as well as the success path, and **before** the result is reported, so anything a
  defer records lands inside the segment that ran it.

Two boundaries are deliberate and both are tested. It does **not** run on suspension — a sleeping
workflow has not exited, and firing every cleanup at the first sleep is the silent failure this
most easily becomes; the segment that finally completes replays the entry point, re-registers the
same IDs, and runs them then. And it does not run for a workflow that never returns — cancelled,
failed terminally, or killed by the fence — because the guest is not executing then. That case
still needs a host-driven path, which needs replay, and is §3.35 phase 4.

Defers are at-least-once, like everything else here: a defer that itself suspends leaves the ones
after it unrun, and on resume the whole set runs again from the top. Durable calls inside them
replay from history rather than re-executing, so this is ordinary replay semantics, but a defer's
non-durable side effects can repeat.

#### The host now runs defers only for a guest that never ran its own — done

The invocation left behind above logged `defer execution failed … unknown entry point` for a
defer that had just run. Worse than the silence it replaced: an operator reading it concludes
their cleanup did not happen, and the log is the only evidence they have.

The rule is now one line. **Reaching `cleat_complete` means the guest came out through its entry
point wrapper, and that wrapper ran the defer bodies.** So:

- `Engine.executeWithBackend`'s error branch skips the defer pass when `callErr` is a
  `*GuestReturnedError` — the existing marker for "the guest stopped cleanly and said it had
  failed" (§3.23), already computed three lines below for the trap-vs-error distinction.
- The worker's success-path pass is **deleted**. `finalStatus == "done"` means the guest
  completed, so the condition was always false — and `scripts/check-test-only-code.sh` said so,
  refusing the gated version because it left `runDefers` reachable only from tests. Its fence
  test went with it: `TestDeferPassIsBoundedInAggregate` covers the same property at the layer
  that owns `RunDefer`, with three runaway defers instead of one.
- Trap, fence kill and timeout are untouched. Nothing ran those guests' defers, so the host's
  pass is the only chance they have and its failure log is a true statement.

The rule covers every SDK rather than special-casing Go: the other four have no defer bodies at
all, so "the host has nothing to add" holds for them for a different reason.

Both halves are mutation-tested. Suppressing the pass unconditionally fails the trapped-guest
control; leaving it unconditional fails the regression test with the false log it was written
for.

**A panic is on the guest's side of that line, which shrinks §3.35 phase 4.** "Panic" reads like
"trap", and the phase-4 scope was written assuming it was one. Measured 2026-09-02 on a real Go
SDK guest through wasmtime, entry point `defer_on_panic` in `testdata/deferfunc`:

    err    = host: export "defer_on_panic" failed: the workflow panicked   (GuestReturnedError)
    calls  = [on_panic]                                                    (the defer body ran)
    logs   = <empty>                                                       (no host pass)

A Go panic unwinds into the generated dispatcher's `recover`, which reports through
`cleat_complete`, so the guest still leaves via its own wrapper and the wrapper runs the bodies.
Pinned by `TestAPanickingGuestRunsItsOwnDefers`, which fails with `recorded []` when the
dispatcher's runner call is removed.

So what is actually left for phase 4 is the genuinely unrecoverable: a WASM trap, an
out-of-memory, and a fence kill or timeout — none of which return to guest code at all. **Note
what that means for the phase 2 fix described below** (`withHandler(context.Background(),
session)` so a defer body can make host calls on the trap path): it is still correct, but it
benefits no shipped guest, because the export it would reach — `cleat_defer_<id>` — is one no SDK
emits and none can. Do not schedule it as though it closed the trap case.

#### The fence case is reachable — both halves now measured

The obvious reading of "the guest never got to run its defers" is that nothing can be done: the
bodies are closures in guest memory, and the guest is dead. For a **fence kill** that reading is
wrong on its first step, and the difference is worth knowing before phase 4 is designed.

**Measured 2026-09-02, pinned by `engine.TestTheFenceLeavesTheInstanceUsable`:** after epoch
interruption stops a spinning guest, the wasmtime instance is *still callable*, and linear memory
written before the interrupt is *still there* — a second export read back the marker the first
one wrote before it was stopped. `SetEpochDeadline` is relative to the current epoch, so the
store needs a fresh budget first; without that the next call is interrupted immediately, which is
the mutation the test uses to prove the assertion is live.

That is unlike the fresh-instance problem §3.70 ran into. Here the instance that registered the
defers is the one still standing, so its closure table has not gone anywhere.

That module is hand-written WAT with no language runtime in it, though, and the half that
mattered was the other one: a **Go** guest fenced mid-loop also has a Go runtime interrupted at an
arbitrary point — scheduler, GC and stack in whatever state the interrupt found them — and
re-entering it through a `//go:wasmexport` might not be safe.

**Measured 2026-09-02 on a real Go SDK guest, pinned by `engine.TestAGoGuestSurvivesTheFence`**
(fixture `testdata/fencereentry`, harness `engine/fence_reentry_test.go`, which reproduces
`Execute`'s own setup sequence but keeps the store and instance open). A guest registers a defer,
spins until a 2s fence kills it, and is then re-entered through the module's *other* generated
export after a fresh epoch deadline. All three hold:

| | result |
|---|---|
| the re-entry call | succeeds — no trap |
| guest code actually ran | yes: it reached the host (`DurableCall` recorded by the mock) |
| **the fenced workflow's own defer ran** | **yes, and it reached the host too** |

Re-derive with `go test ./engine/ -run 'TestAGoGuestSurvivesTheFence|TestTheHarnessCanCallAnExportDirectly'`.

**The third row is the one that decides phase 4**, and it needed no production change to observe.
Codegen already emits `_cleatRunDeferred` at the end of every export (§3.70), so *any* subsequent
call into the instance drains the closure table — including one made after the fence. The
mechanism phase 4 needs already works end to end. What is missing is only the host deciding to
make that call, plus a defer-runner export it can name rather than borrowing an unrelated entry
point the way the test does.

**Two things keep this honest.** `TestTheHarnessCanCallAnExportDirectly` is the control: it runs
the same two-phase shape with the fence removed, so a failure in the measurement can be attributed
to the fence rather than to the harness. And every assertion is mutation-proven — dropping the
fresh `SetEpochDeadline` makes re-entry fail with `wasm trap: interrupt`; deleting the fixture's
`DurableDeferFunc` leaves re-entry passing and fails *only* the defer row, which is what shows the
two are independent rather than one assertion counted twice.

#### The other abnormal exits behave the same way — measured 2026-09-02

The paragraph here used to say the siblings were unmeasured and warned against generalising.
They have now been measured, with the same rig, in
`engine/abnormal_exit_reentry_test.go`. Every reachable abnormal exit for a Go guest leaves
the instance usable and its outstanding defer runnable:

| how the guest was stopped | mechanism | re-entered | ran guest code | ran the dead workflow's defer |
|---|---|---|---|---|
| execution fence (epoch) | `wasm trap: interrupt` | yes | yes | **yes** |
| instruction limit (fuel) | `wasm trap: all fuel consumed` | yes | yes | **yes** |
| memory limit (OOM) | `proc_exit(2)` — *not* a trap | yes | yes | **yes** |
| clean completion (control) | `proc_exit(0)` | yes | yes | n/a |

Re-derive: `go test ./engine/ -run 'Reentry|SurvivesTheFence|HarnessCanCall'`

**The OOM row is the one that was least safe to assume, and it is also the odd one out.** It
is the only exit that is not a wasmtime trap: the Go runtime asks for memory, is refused by
`store.Limiter`, dumps every goroutine to stderr and calls `proc_exit(2)` from its fatal path
— with the allocator mid-flight. The closure table survives that too.

**Each limit is refreshed differently, and getting it wrong is indistinguishable from a dead
instance.** Time takes `SetEpochDeadline`, instructions take `SetFuel`, and memory takes a
raised `store.Limiter` — the OOM arm initially failed with `failed to grow memory by 33` while
*setting up* the re-entry call, because a guest that died of OOM has by construction used
everything it was allowed and the call still needs scratch space. That was a harness artifact
reported as a finding, which is the same shape as forgetting `SetEpochDeadline` in the fence
arm.

**The boundary, stated rather than left as a gap:** there is no arm for a raw wasm trap
(`unreachable`, out-of-bounds) because a Go guest essentially cannot reach one from Go source.
Panics are recovered by the generated dispatcher and become `GuestReturnedError` (§3.70, and
the guest runs its own defers), and Go's unrecoverable failures — fatal OOM, stack exhaustion
— go out through `proc_exit`, which is the third row above. A hand-written WAT module can
trap, but it has no Go runtime and therefore no closure table, so the question this table asks
would be meaningless for it.

So phase 4 does not need a per-exit design. **One mechanism covers every case that a Go guest
can actually reach**, which is a materially simpler problem than the one this section was
written to scope. What remains is the host deciding to make the call, a named defer-runner
export to call, and a bounded budget for it.

---

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

### 3.82 Section numbers collide across workstreams — ✅ **GUARDED** (WS-3, 2026-09-02); four dangling references measured and left open

Three workstreams append `### N.M` sections to this file concurrently, each picking "the next
free number" against a `develop` that has already moved. The allocation is invisible in the file
you edited, so the collision exists only in the merge — and a duplicate heading renders
perfectly, so nothing catches it. Four landed on 2026-09-02 alone:

| number | allocated by | resolution |
|---|---|---|
| `3.77` | WS-1 (#579) and WS-3 (#580) | WS-3 moved to 3.80, then 3.81 |
| `3.78` | WS-1 (#582) and WS-2 (#583) | #587 moved WS-2's to 3.80 — onto the number WS-3 had just taken, forcing the second move above |
| `2.70` | twice | #588 |
| `1.3` | the cancellation section and a `1.3 residual` section 45 lines above it | this PR: the residual is now a `####` subsection of 1.3, which is what 2.15 and 2.28 already do with theirs |

**The cost is not the duplicate, it is the cross-references that rot around it.** §3.79 read
*"found by §3.77's vacuity guard"* while §3.77 was an unrelated WS-1 item about per-tenant names
— a pointer into real prose about the wrong thing, which is worse than a dead link because it
reads as correct. Renumbering is where this happens: re-grep the **old** number across this file
*and* `engine/*.go`, whose comments carry section numbers too, rather than fixing the references
you remember. A keyword-filtered pass over #580 missed two.

**Guarded** by `scripts/check-section-numbers.sh`, wired into the `Lint` job. It carries a
vacuity floor (fewer than 50 headings fails rather than passes), because a grep that matches
nothing reports success — a heading-format change would otherwise turn it into a no-op printing
OK forever. Watched failing for both reasons before landing: an injected duplicate, and `###`
rewritten to `##`.

Only `###` is checked. `####` subsections legitimately repeat a parent number — `#### 1.4 phase
D`, `#### 1.4 phase E`.

#### What is measured but not fixed: four dangling references

Duplicates are one way a pointer goes wrong; a pointer to a section that was renamed, renumbered
or never written is the other. Measured 2026-09-02 against `develop` at `eab1101`, before this
section existed — 482 `§N.M` references, 106 distinct, of which **four resolve to no heading**:
`§0.2` (x1), `§2.3` (x1), `§2.4` (x4), `§2.59` (x1).

**Those counts are pinned to that commit on purpose: this section names all four, so it inflated
the count it reports.** Run the command on the working tree and it returns 3/2/6/2 — the extra
occurrences are the sentences you are reading. The *set* is the stable part; the counts are not,
and any later count has to subtract this section or pin a commit as above. A measurement that
changes what it measures is worth stating rather than quietly correcting, because the naive
re-run disagrees with the doc and looks like rot.

Re-derive (against the pinned commit, so the numbers above reproduce):

```
git show eab1101:IMPROVEMENT-PLAN.md > /tmp/plan-at-eab1101.md
python3 -c "
import re
from collections import Counter
s=open('/tmp/plan-at-eab1101.md').read()
h=set(re.findall(r'^### (\d+\.\d+) ', s, re.M))
c=Counter(re.findall(r'§(\d+\.\d+)', s))
print([(r,n) for r,n in sorted(c.items()) if r not in h])
"
```

**Not fixed and not guarded here, because the fix is not mechanical.** Each needs a reading of
the surrounding prose to decide what it *meant* — `§2.4` is referenced four times from a summary
table that may predate a renumbering, and `§0.2` suggests a section block that no longer exists.
Guessing would produce exactly the failure described above: a confident pointer at the wrong
prose. A guard added before the four are resolved would be permanently red.

---

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

### 3.200 A Go guest was told "error 1 (timeout)" for every plugin failure, and the host's real message was in the buffer beside it — 🟢 **FIXED 2026-09-04** (WS-1, 2026-09-04); the other 13 adapters are 🔴 **OPEN**

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

CLAUDE.md records that all four prior defects at this boundary were "the value meant the wrong
thing on one side of the boundary", and none was an overflow. This is a fifth, and it is that
exactly — twice over: a length that was read and dropped, and a code read from the wrong field.

### 3.201 The Python SDK discarded the host's answer on 13 calls, so a refusal read as a success — 🟢 **FIXED 2026-09-04** (WS-2, 2026-09-04)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 3.202 A stop read as a timeout on `await_signals`, so a Python defer segment ran on — 🟢 **FIXED 2026-09-04** (WS-2, 2026-09-04)

Archived — full text in [`IMPROVEMENT-PLAN-CLOSED.md`](IMPROVEMENT-PLAN-CLOSED.md).

### 3.400 The boundary inventory — 🔷 **C1 DELIVERED, C2 RANKED** (WS-3, 2026-09-04)

WORKSTREAM.md's C1: *"enumerate the boundaries: host↔guest ABI (5 SDKs × ~60 calls),
store↔dialect, doc↔code. A table with a row per boundary and a column for 'guarded by'."*
Its argument for doing this at all: every guard built this week found a defect on its first run,
so the expected value of the next one is high — but where to look is currently chosen by
stumbling.

A boundary here is **two artifacts that must agree, maintained separately**. That is the shape
every defect this week had. It is not "a component", and the inventory is deliberately not a
list of packages.

#### Re-derive the whole table

    # guards that exist (the "guarded by" column)
    ls engine/*parity*_test.go engine/*coverage*_test.go engine/*correspondence*_test.go \
       engine/*completeness*_test.go
    ls scripts/check-*.sh

#### A. host ↔ guest ABI

| # | the two artifacts | guarded by | gap |
|---|---|---|---|
| A1 | wazero host imports ≡ wasmtime host funcs, by name | `hostabi_runtime_parity_test.go` — `TestWasmtimeSatisfiesEveryWazeroHostImport`, `TestNeitherRuntimeHasHostFunctionsTheOtherLacks` | — |
| A2 | …and by **arity**, which is what a guest actually links against | `TestCreatePromiseGuestLinksOnTheWorkerBackend` — **one** host function, by name | **1 of 55.** The name-level check above passed while `cleat_create_promise` was unlinkable (§3.55) |
| A3 | every wasmtime host func registered via `b.hostFunc` (epoch budget) | `scripts/check-hostfunc-budget.sh` | — |
| A4 | host stop site ≡ Go adapter `withSuspendCheck` ≡ WIT `result<string, call-failure>` | `stop_correspondence_guard_test.go` — `TestTheThreeStopSurfacesAgree` | component arm carries an open exemption, `reasonWitIsStillCoreABI` (§3.110) |
| A5 | each SDK's refusable-call list covers every host stop site | `sdk_stop_site_coverage_test.go` | **java, rust, assemblyscript only.** Go and Python are not rows in it |
| A6 | each SDK agrees on the stop bit's **value** (bit 31) | `{as,java,rust}_sdk_stop_bit_parity_test.go` | **3 of 5** |
| A7 | component dispatcher's pack shift ≡ its extractor's shift | `component_pack_extract_parity_test.go` (§3.33 mechanism) | 23 of 25 pairings; 2 hand-rolled exceptions named in the test |
| A8 | packed result's **errCode** ≡ what the guest observes | **nothing** | open: `extractStringFromPacked` drops it, so a refusal reaches Python as a success (WS-2, §3.113) |
| A9 | `ABI.md` ≡ the code it specifies | `scripts/check-doc-consistency.sh` — ABI version, `DefaultOutBufSize` | **2 numbers** against a document describing ~60 calls |

A5/A6 read worse than they are: Go is covered by A4's adapter arm, and Python by A4's WIT arm.
The uncovered thing is narrower — no SDK-level list for either — and A4 is the reason it has not
bitten.

#### B. store ↔ dialect

| # | the two artifacts | guarded by | gap |
|---|---|---|---|
| B1 | tenant filter predicates the migrations ship ≡ predicates in the built DB | `mssql_policy_coverage_test.go` (§2.71, #691) | mssql only |
| B2 | Postgres JSONB columns ≡ mssql JSON check constraints | `mssql_json_parity_test.go` | **postgres ↔ mysql: nothing** |
| B3 | required indexes exist, and the planner uses them | `mssql_index_parity_test.go` | mssql only |
| B4 | every read path returns the same row, and the time that was written | `read_path_parity_test.go` | — |
| B5 | migrations that define a routine ≡ migrations the test helper applies | **nothing** | see below — **8 vs 2** on Postgres |
| B6 | the migration *set* across dialects | **nothing** | 21 pg / 16 mysql / 23 mssql |
| B7 | event record fields ≡ what the stored payload carries | `payload_carrier_completeness_test.go` | — |
| B8 | statement-level tenant predicates in the Go SQL | §3.86's allowlist-with-reasons (WS-1) | — |

#### C. doc ↔ code

| # | the two artifacts | guarded by | gap |
|---|---|---|---|
| C1 | `tiers.yaml` support claims ≡ what CI runs | `tier-gate.sh`, `tier2-gate.sh` | its `open_items` prose is the part CI does not check (WORKSTREAM R4) |
| C2 | section numbers unique across the plan | `check-section-numbers.sh` | structurally cannot see collisions that exist only across open PRs (R2) |
| C3 | exported API ≡ something using it | `check-dead-exports.sh` | — |
| C4 | `CLAUDE.md`'s claims ≡ the code | **nothing** | four sessions were lost to one sentence describing a removed build tag |

#### B5, measured

`engine/store_backends_procedures_test.go` declares, per dialect, which migration files define
the stored routines, and applies exactly those against the shared test database:

    var postgresProcedureMigrations = []string{"003_procedures.sql",
        "004_fix_finalize_workflow_status_fence.sql"}

Three hardcoded lists of two. What is actually on disk, 2026-09-04:

    for d in postgres mysql mssql; do
      grep -lniE 'create (or replace )?(function|procedure)|create or alter procedure' \
        migrations/$d/*.sql | sed 's|.*/||' | sort | tr '\n' ' '; echo
    done

| dialect | migrations defining a routine | the helper applies |
|---|---|---|
| postgres | **8** — 001, 003, 004, 023, 024, 032, 034, 040 | 2 |
| mysql | **3** — 003, 004, 034 | 2 |
| mssql | 2 — 003, 004 | 2 |

`040_claim_terminating_workflows.sql` is the one that matters: it `DROP`s and re-`CREATE`s
`admin.claim_workflows` with an extra return column, superseding `023`. This is §1.1's trap
exactly — *for anything defined by `CREATE OR REPLACE`, find the highest-numbered migration that
defines it* — with the list frozen two migrations after the schema stopped agreeing with it.

**And it is currently clean, which is the finding.** Probed against this checkout's Postgres:

    docker exec cleat-postgres-manual2 psql -U postgres -d cleat -t -A -c \
      "SELECT pg_get_function_result(p.oid) LIKE '%pending_terminal_status%'
       FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace
       WHERE n.nspname='admin' AND p.proname='claim_workflows';"    # -> t

The database has 040's version. It has it because the database was built by an external
migration run over the whole directory, not because anything checked. The helper's list is
right about nothing and wrong about nothing — it is simply not the thing that determined the
answer. On a database that was **not** rebuilt, the same list produces the old routine and every
test that exercises it silently measures the superseded definition. That is the failure
`CLAUDE.md` already warns about ("when a schema migration lands, recreate your test databases")
with no guard attached to it.

The mysql entry is **not** a gap, checked rather than assumed: `034` defines
`cleat_drop_defs_fks()` and drops it again in the same file, and
`information_schema.ROUTINES` for the test database lists only `finalize_workflow_status`.

#### The ranking for C2

**B5 first.** It is the same shape as #691 — parse both sides, declare nothing — so the mechanism
is known to work and known to find things. Its failure is silent, machine-dependent, and
invisible to every existing check. It spans all three dialects, so one guard closes three rows.

**Then A2.** 55 host functions, one of which has a link-level check, and the name-level check
above it passed while a guest could not link. That is a measured false green, not a hypothetical.

Not B6: 21/16/23 is not evidence of anything on its own — dialects legitimately need different
migrations, and a row that cannot distinguish "different" from "missing" is a backlog generator,
not a guard. It stays in the table as an unguarded boundary with no claim attached.

#### What I got wrong building this

The first pattern for the B5 table was
`'CREATE OR REPLACE (FUNCTION|PROCEDURE)|CREATE PROCEDURE|CREATE OR ALTER PROCEDURE'`. It missed
`040` entirely, because `040` uses bare `DROP FUNCTION` + `CREATE FUNCTION` — no `OR REPLACE`.
So the first version of the postgres row read **7 vs 2** and did not contain the single migration
the whole finding turns on. What surfaced it was `store_lifecycle.go:1045` naming `040` in a
remediation string, which contradicted a list built by grep. **The narrower pattern was the one
that flattered the story** — it still showed a gap, just not the interesting one — which is why
it was not questioned until something outside the grep disagreed with it.
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
### 3.304 The Python SDK's only publish path cannot fire, so `cleat-sdk` has never reached PyPI — 🔴 **OPEN** (WS-2, 2026-09-04)

python is tier 1 and PyPI is the one registry `tier1-gate.yml` accepts as a dependency,
but `cleat-sdk` is not on PyPI:

    curl -s -o /dev/null -w '%{http_code}\n' https://pypi.org/pypi/cleat-sdk/json   # 404
    curl -s -o /dev/null -w '%{http_code}\n' https://pypi.org/pypi/requests/json    # 200 (control)

`.github/workflows/publish-pypi.yml` has **zero runs, ever**
(`gh run list --workflow publish-pypi.yml --json databaseId` → `[]`; control:
`--workflow ci.yml` is non-empty). It has two triggers and neither can fire here:

- `push: tags: ["python-sdk/v*"]` — no such tag exists (`git tag -l 'python-sdk/*'` is
  empty). Both tags in the repo are plain `v*`.
- `release: types: [published]` — the release *is* published, but by GoReleaser running
  under `secrets.GITHUB_TOKEN` (`.github/workflows/release.yml:115`). **GitHub does not
  trigger workflows from events created with `GITHUB_TOKEN`**, which is the recursion
  guard, so a GoReleaser-created release cannot start this workflow. Release `v0.2.0` was
  published 2026-08-10T19:45:42Z; the Release run that created it succeeded at
  19:42:57Z (`gh run list --workflow release.yml`); `publish-pypi` did not run.

**This is not fixed here, because the repair is a release-policy decision, not a
mechanical one**, and it publishes to an external registry that cannot be un-published.
The two options differ in what they couple:

1. Trigger on `push: tags: ["v*"]`. Simple, but it ships whatever `pyproject.toml` says
   at that moment — today a `v0.3.0` tag would publish `cleat-sdk 0.2.0`, because the
   Python version has never tracked the repo tag (see CONTRIBUTING, "SDK versions").
2. Keep the versions independent and start pushing the `python-sdk/v*` tags the workflow
   already expects. Nothing has ever pushed one, so this trigger has never been exercised
   either.

**A note on how nearly this went wrong.** The first reading here was "the release
automation has never executed", from `gh run list --limit 200` showing no Release run.
That listing reached back **2.5 hours** (`gh run list --limit 200 --json createdAt --jq
'[.[].createdAt] | min'` → `2026-09-05T00:02:38Z`), so it could not have seen a run from
2026-08-10 whatever the truth was. Querying the workflow directly showed one run, and it
succeeded. A limit-bounded listing answers "not in the last N", never "never" — take the
window's own lower bound before writing "never" down.
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

### 3.306 The Go adapter decodes the host's error message and then throws it away — 🔴 **OPEN** (WS-2 found, WS-1 diagnosed and holds the fix, 2026-09-04)

Same host, same 10 registered plugins, same 17 calls, four guest languages. Measured
2026-09-04 by dumping every key of `TestPluginCalls_Wasm_*`'s result:

| guest | failed `blobstore.put` | failed `pgvector.upsert` |
|---|---|---|
| Rust, AssemblyScript, Java | `blobstore: no tenant context` | `plugin function pgvector/upsert not registered. Check that…` |
| **Go** | `plugin_call: error 1 (0=unknown 1=timeout …)` | `plugin_call: error 1 (0=unknown 1=timeout …)` |

(Python could not be measured here: `componentize-py` is killed with signal 9 building this
workflow in this sandbox.)

**The mechanism, determined.** The host writes the text and the guest decodes its length
correctly; the Go adapter then discards both. `wasm/adapter_metadata.go`, `PluginCall`'s
`ResultStmts`:

    responseLen := uint32(uint64(result) >> 40)
    errCode := uint32(result & 0xFF)
    if errCode != 0 {
        return "", fmt.Errorf("plugin_call: error %d (0=unknown 1=timeout …)", errCode)
    }
    return unsafe.String(&responseBuf[0], int(responseLen)), nil

`responseLen` is computed and then unused on the error branch; `responseBuf` is never read
there. Three faults on one line:

1. **The message is discarded.** `engine/plugins.go:361` writes it —
   `s.writeResult(ctx, m, responsePtr, errStr, responseMaxLen)` — then packs the length.
   Rust/AS/Java read it. Only Go does not.
2. **The number printed comes from a different field than the legend describes.**
   `packDurableCallResult(responseLen, callErrorCode, errCode)` is
   `responseLen<<40 | callErrorCode<<8 | errCode` (`engine/memory.go:243`). `result & 0xFF`
   is `errCode`, which that same call site hardcodes to a literal `1` on every failure.
   The legend is the **CallErrorCode** legend, and CallErrorCode lives at bits 8-39. So
   `error 1` is a constant from one field printed against another field's legend — not a
   timeout, and not a classification.
3. **The real classification is discarded too**: `callFailureCode` sits in bits 8-39 and is
   never read.

So "why is it 1 for a not-registered plugin" has a flat answer: **it is 1 for everything.**

**This is a mechanism, not a bug.** Of the 23 adapters, **20 print that legend and 3 call
`callErrorMessage`** — and those 3 (`cleat_call`, `cleat_call_retry`,
`cleat_call_heartbeat`) are exactly the ones §2.10 fixed. The fix was applied to the calls
named in that report and never generalised. Re-derive:

    grep -c '0=unknown 1=timeout' wasm/adapter_metadata.go     # 20
    grep -n 'callErrorMessage' wasm/adapter_metadata.go        # 3

15 of the 20 have an out buffer and already decode its length before discarding it;
5 (`ContinueAsNew`, `ContinueAsNewWithVersion`, `AcquireLock`, `AcquireLockMs`,
`ReleaseLock`) have no out buffer, so the legend is all there is — but whether the number
matches the legend still needs per-packer checking there. `wasm/generator.go`'s
`hostErrMessage` already exists for precisely this case, and its doc comment warns that
printing the CallErrorCode legend beside a `packSimpleResult` return "would describe a
rejected cron expression as a timeout". **That comment describes the live defect in 15
other calls.** The abstraction was written, documented, and applied to three sites.

Held by WS-1; it needs per-call classification of which packer each host function uses, so
it is not a find-and-replace. Pinned meanwhile by `knownPluginFailures` in
`tests/plugin-harness/wasm_plugin_test.go` (§3.307), which carries the two `plugin_call*:
error ` entries explicitly so the divergence stays visible rather than absorbed.

**How this was first mis-diagnosed, because the mistake is reusable.** WS-2 reported the
symptom with two candidate mechanisms — "the host wrote no response bytes" or "the length
failed its bounds check" — and **both were wrong**, because both assumed `PluginCall` went
through `callErrorMessage`. It does not; it has its own literal copy of the format string.
The assumption came from

    grep -rn '0=unknown 1=timeout' --include='*.go' . | grep -v wasm_plugin_test | head

whose **first** hit is `wasm/generator.go:427` inside `callErrorMessage`, and whose
10th line is not its last. That string occurs **22 times in non-test Go across 3 files** (23 counting the one in a
test) — **20 of them in `wasm/adapter_metadata.go`**, including the `PluginCall` entry at
line 530 that `head` cut off. Re-derive the shape with

    grep -rn '0=unknown 1=timeout' --include='*.go' . | awk -F: '{print $1}' | sort | uniq -c

The first hit was a plausible decoy: `callErrorMessage`'s `%s: error %d` with
`callName="plugin_call"` renders byte-identically to the literal that actually produced it.
**A `| head` on a search for "who produces this string" answers a different question — who
produces it first in path order — and the two coincide only by luck.** Refusing to name a
mechanism was what kept the wrong one out of the record; the guess would have been written
down as fact.

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

Falsified three ways, each firing its own branch and no other:

| perturbation | result |
|---|---|
| drop the `no tenant context` reason | **12** keys report `failed for a reason not in knownPluginFailures` — matching the 12 measured |
| drop `llm.list_models` from `pluginCallsThatMustSucceed` | `llm.list_models now succeeds… add it` |
| add `blobstore.put` to `pluginCallsThatMustSucceed` | `blobstore.put must succeed in this environment and did not` |

Also removed a duplicated `"llm.chat_stream"` from three of the five key lists (Go, Rust,
Python) — 18 entries, 17 distinct, so one key was checked twice and the count in the log
line never matched the list.
