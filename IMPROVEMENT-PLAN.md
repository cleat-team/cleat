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
   PARALLEL-WORKSTREAMS.md's third cross-stream coupling and belongs to WS-2.

**Process note for future sessions.** Two commits had to be rewound because `git add -A` was
run while subagents were mid-edit; one nearly shipped a call site an agent had *deliberately*
broken to prove a test bites. Use explicit paths, and run `git show --stat` before every
commit. A commit message asserting "docs only" over a diff full of functional code is the
same defect class this plan exists to fix.

## Phase 1 — Paired test + fix, by severity

For each item: **write the failing test first, watch it fail, then fix.** A passing unit test
is not evidence here; that is precisely how these survived.

### 1.1 Unfenced terminal side effects — data loss — ✅ **FIXED** (heading marker added 2026-09-01)

> **This heading carried no status marker until 2026-09-01, and the body below has said
> "Done in `8d44300` + `f9bce35`" the whole time.** A scan for open work therefore reported
> the project's highest-severity data-loss bug as outstanding, and a session was spent
> re-deriving — from scratch, and independently reaching the same conclusions — what the body
> already recorded. CLAUDE.md says the marker is the source of truth; a heading with *no*
> marker over a completed body is the same trap as a ✅ over a stale one, and reads as worse
> because "no marker" is indistinguishable from "not started".
>
> The `Files:` bullet below made it stick. It named
> `migrations/{postgres,mysql,mssql}/003_procedures.sql`, and 003 is exactly the file the fix
> *superseded* — `004_fix_finalize_workflow_status_fence.sql` redefines the procedure.
> Checking the claim against the file the claim named confirmed the bug was still present,
> because that file is still, correctly, unguarded. Corrected in place below.

`finalize_workflow_status` fences the status `UPDATE` on `assigned_to` + `generation`, then
runs the terminal block **unconditionally**, gated only on `p_final_status IN ('done','failed')`.
A zombie worker that correctly lost the fence still executes
`DELETE FROM event_history WHERE workflow_id = p_workflow_id` and injects its stale result
into the parent's `await_child` event.

- Repro chain (confirmed): `ClaimWorkflows` bumps `generation`; `ReapStaleInstances` does not.
  A→stall→reap→B claims→A finishes→A wipes B's live history.
- Fix: capture `ROW_COUNT`/`@@ROWCOUNT` from the fenced `UPDATE`; skip the entire terminal
  block if zero. All three dialects.
- Files, **as fixed**: `migrations/{postgres,mysql,mssql}/004_fix_finalize_workflow_status_fence.sql`.
  Each redefines `finalize_workflow_status`, so **003 is superseded and still shows the original
  unguarded body** — read 004, not 003. (Postgres additionally has to `DROP FUNCTION` first,
  because the fix changes the return type from `VOID` to `BOOLEAN`; the comment in that file
  explains why `CREATE OR REPLACE` alone fails with 42P13.)
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

### 1.2 Systemic unchecked `RowsAffected` — ✅ **FIXED** (heading marker added 2026-09-01)

> Same omission as §1.1: no marker on the heading, while the body records both halves as
> done. Re-verified 2026-09-01 before adding the marker — `ErrFenceLost` is returned from
> `engine/store_lifecycle.go`, and the concurrency-conflict caller uses `TerminateWorkflow`
> at `cmd/cleat-worker/server.go:544` with the reasoning the body describes.

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

### 1.3 Cancellation is dead end-to-end — ✅ **FIXED**, and this section was stale

**The original entry, kept because the plan being wrong is itself the finding:**

> `PollCancellation(ctx, "")` — hardcoded empty string at all three call sites. The store does
> `WHERE id = $1`, so it never matches. `RequestCancellation` sets a flag nothing observes.
>
> - Files: `engine/durablecalls.go:51`, `engine/heartbeats.go:58`, `engine/signaller.go:121`
> - Fix: pass `s.engine.workflowID` — exactly as `PollSignal` already does twelve lines away
>   at `engine/signaller.go:133`.
> - **Also fix the mock**, or this recurs: `engine/host_test.go:2014` declares the parameter
>   `_ string` and discards it, which is why 2,560 engine tests passed against dead code.
> - Test: cancellation e2e (see 2.3).

**Correction, 2026-08-04.** Both prescribed fixes had already landed in `c26c332`, which also
added `TestCancellationObservedEndToEnd` and `TestCancellationNotObservedForDifferentWorkflow`
(`engine/host_dispatch_test.go:659`). All three call sites pass `s.engine.workflowID`;
`mockCancellationStore` now captures every ID it is polled with and the tests assert on it.
The section was never updated, so §1.3 sat at the top of a workstream as its "start here" item
while being done.

This is the same failure mode as the `CGO_ENABLED=0` note in `CLAUDE.md` — **fixed by the same
commit, and also left in place afterwards.** `c26c332` is worth auditing for a third.

What was genuinely missing was the test the entry asked for. Everything covering cancellation
supplied its own `SignalStore`, so the workflow ID was whatever the mock chose to accept —
including, for as long as it was there, `""`. `TestCancellationObservedEndToEnd` is a good test
but is not end-to-end despite the name: it drives `s.DurableCall` directly against an in-memory
store, with no database and no compiled module.

Added in `engine/cancellation_e2e_test.go`, against a real `PostgresStore` and a real workflow
compiled to WASM on wasmtime:

- **`TestCancellationEndToEnd`** — operator cancels via the same store method the worker's HTTP
  handler calls, then `place_order` runs. Asserts on the `ServiceCaller`, not the result string:
  cancellation exists to stop side effects. Proven to fail by restoring the `""`, which produced
  **6 side effects including `payments.Charge` and `shipping.CreateShipment`** on a cancelled
  workflow.
- **`TestCancellationGuestAPIEndToEnd`** — the guest-facing `h.PollCancellation()`
  (`engine/signaller.go:121`), which `examples/subscription` and `examples/travel` branch on and
  which had no end-to-end coverage at all, because no fixture called it. New fixture:
  `testdata/cancelpoll`.
- Both have **`_NotCancelled` controls**, without which either would pass against an engine that
  refuses every call for any reason.
- **`TestCancellationUnknownWorkflowIDIsNotSilent`** — pins the mechanism. `CheckCancellation`
  returns `sql.ErrNoRows` for an ID matching no row and every call site guards on
  `err == nil && cancelled`, so an ID that does not resolve reports "not cancelled" and the
  workflow proceeds. The `""` was not failing loudly; it was missing quietly. **This guard is
  still live and is the residual risk in this section** — see below.

**Two things found by running it that reading would not have given:**

1. **The two cancellation checks mask each other.** Breaking `signaller.go`'s poll left the
   side-effect count at 0, because `freshCall`'s own pre-call check still refused the call. Only
   the assertion on the cancellation *reason* failed. A test asserting solely on side effects
   cannot distinguish "the guest handled its cancellation" from "the engine aborted the guest".
2. **The wasmtime `t.Skip` in the older tests is the wrong category.** By
   `scripts/check-skips.sh`'s own taxonomy this is case (c) — always satisfiable in this repo,
   since CGO is on by default — so it must be `t.Fatal`. The new tests use `t.Fatalf`; the
   pre-existing ones in `host_test.go` still skip, which means a `CGO_ENABLED=0` run reports
   them green without exercising the primary backend. Not changed here: they are not this
   section's files and the baseline is shared. **Worth its own item.**

**Not covered: the heartbeat path.** `engine/heartbeats.go:58` cancels an *in-flight* call via
`cancelCall()`. Both new tests check cancellation *before* a call goes out; neither interrupts
one in progress, which is the "assert it actually stops within N seconds" half of §2.3. The call
site passes the right ID, so the §1.3 defect is not present there, but nothing exercises it
end-to-end. It needs a heartbeat-enabled call and a concurrent cancel — a different harness, not
another case in this file.

**Residual, still open:** the `err == nil && cancelled` guard at all three call sites. A poll
that errors — unresolvable ID, dropped connection, RLS returning no row — is indistinguishable
from "not cancelled", and the workflow proceeds to perform side effects. Making it loud is a
behaviour change (a transient DB blip would start halting workflows), so it needs the same
decision WS-1's §1.1 trap needs: establish what a failed *check* should do before changing what
it returns. Not attempted here.

#### Residual — a cancelled heartbeat call was reported as retryable — ✅ **FIXED** (WS-2, 2026-08-04)

The hardcoded `""` was fixed by `c26c332`; the missing end-to-end test landed in #264. This is
what was left underneath, and it is a behaviour defect rather than a dead call site.

Both call paths detect cancellation. `freshCall` reports it as `callErrorUnknown`, with a
comment saying why: *"Not retryable: the workflow was cancelled, so repeating the call is the
one thing the caller must not do."* `callerrors.go` agrees, naming a cancelled workflow as the
first of the three canonical non-retryable cases.

`freshCallWithHeartbeat` cancelled the in-flight call's context and then fell through to its
**generic** error branch, which returns `callFailureCode` — `callErrorUnavailable`, documented
as *"Retryable"*. So a workflow cancelled during a long call was told the call was worth trying
again, and a guest branching on `Retryable()` would re-issue the call it had just been
cancelled out of.

The recorded event was the durable half of the same defect:

```
callErrorCode = 2, want 0 (callErrorUnknown, non-retryable)
recorded Err  = "context canceled", want "workflow cancelled"
recorded ErrNonRetryable = false
```

Replay reads retryability off the event via `recordedFailureCode`. An event carrying the raw
context error with `ErrNonRetryable` unset replays as an ordinary retryable failure — so the
same step was non-retryable on the first run and retryable on the replay of it. That is
precisely the divergence `recordedFailureCode` was introduced to prevent (§2.35); this path
routed around it by never recording the classification at all.

Fixed by tracking why the call context was cancelled and reporting a cancellation on both the
guest-visible code and the recorded event. `cancelledCallError` is now a shared constant, since
two paths produce it and replay compares against what was written.

**Also:** `PollCancellation` errors were discarded at both sites. Failing open is right — a
database blip must not abort a workflow that has not been cancelled, and the poll repeats on
the next tick — but it was *silent*, so a persistently failing poll made cancellation quietly
stop working with nothing to see. Now logged at both sites, with
`TestDurableCallWithHeartbeat_PollErrorDoesNotCancel` pinning the fail-open behaviour so the
guard cannot be flipped by accident.

Three tests, watched failing before the fix: the cancellation case, an uncancelled control (so
the cancellation branch cannot be reached unconditionally and pass for the wrong reason), and
the poll-error case.

### 1.4 Crash-recovery: write-ahead intent — ✅ **FIXED** (heading corrected 2026-09-01)

> **The old heading was "the detector works, nothing writes what it detects", which is the
> original problem statement and is no longer true.** Phases B and D are both marked done in
> the body below, and the write side is wired end to end. Assessed 2026-09-01 rather than
> assumed, after §1.1 and §1.2 turned out to be the same kind of stale:
>
> - The dead `flushCallIntent`/`completeCallEvent` pair was **deleted**, not left in the tree
>   — the "~350 lines of tested-but-dead durability code" the body warns about is gone.
>   `grep -rn 'flushCallIntent' --include='*.go' engine/ | grep -v _test.go` returns only
>   comments recording the deletion.
> - It was reimplemented as `engine/callintent.go` + `engine/store_intent.go`, and the live
>   path routes through it: `engine/durablecalls.go:96` calls `freshCallWithIntent` for
>   declared operations.
> - It is reachable from a real deployment, which is what "nothing writes it" was about:
>   `--write-ahead-intent-ops` (`cmd/cleat-worker/config.go:112`) feeds
>   `engine.WithWriteAheadIntentOps` at `cmd/cleat-worker/setup.go:1767`.
> - **Opt-in per operation is the design, not an unfinished edge.** The flag costs one extra
>   synchronous round trip per call, so it is declared for operations that are unsafe to
>   repeat (a card charge, not a GET).
> - Covered by `TestCrashWithWriteAheadIntentDoesNotRepeatTheCall` (`tests/crash/`), which
>   SIGKILLs a worker mid-call and asserts the side effect is not repeated, and which carries
>   its own non-vacuity note: with `--write-ahead-intent-ops` removed it becomes the test that
>   demonstrates the bug. `go test ./tests/crash/ -count=1` → ok (63s, 2026-09-01).

> **Blocker found and fixed first, 2026-08-04 — ordinary event writes were being
> discarded.** Before wiring intent writes into the call path it is worth knowing that
> until `ddac7d1` the path's *ordinary* writes did not reach the database at all on the
> connection cleat ships.
>
> `flushEvent` wrote through `e.db` directly and its quota path opened an unscoped
> transaction; neither set `cleat.tenant_id`. `event_history`'s RLS policy is
> `tenant_id = assert_tenant_set()`, which raises on the unset setting, so as `cleat_app`
> — unprivileged, `NOBYPASSRLS`, mandatory since `c26c332` — every insert was rejected and
> `engine/lifecycle.go:179` logged it and continued. A worker ran three durable calls,
> failed three flushes, and finished with status `done`, a result, and **zero rows in
> `event_history`**.
>
> Not total, which is why it survived: `adaptive_flush.go` already set the context with a
> `WITH cfg AS (SELECT set_config(...))` CTE, so events persisted once a workflow's rate
> pushed the flusher into batch mode, and not below it. And no test could see it, because
> every database test in `engine/` connects as the owner, which on PostgreSQL is a
> superuser and exempt from RLS — §1.10's shape applied to a code path instead of a policy.
>
> **This reorders the phases.** B–F are all about making a crash *observable*. A workflow
> with no persisted events has nothing to replay from, so a crash re-executed every side
> effect it had already performed — a larger contract violation than the one §1.4 exists to
> fix, sitting underneath it. Regression tests in `engine/flush_rls_test.go`, with an
> owner-connection control.
>
> **Found by building the §2.4 harness, not by reading `flush.go`.** The plan's own
> instruction — do not start the intent work before the crash harness exists — turned out
> to be right for a reason it did not anticipate.
>
> **Measured, three ways.** `tests/crash` kills a worker during the third of three durable
> calls and counts what the external service was asked to *do* — not what it received:
>
> | | Reserve | Charge | Ship | events durable at crash |
> |---|---|---|---|---|
> | fix reverted | **2** | **2** | 2 | **0** |
> | with the flush fix | 1 | 1 | **2** | 2 |
> | + idempotency keys (phase B) | 1 | 1 | **1** | 2 |
>
> Row one is what shipped: a crash re-executed two charges that had **already completed
> successfully**. Row two is the documented at-least-once contract — only the interrupted
> call is retried. Row three is phase B: the duplicate request is still *sent*, and the
> service does not act on it twice.
>
> This is the reason the harness uses three calls rather than one: with a single call every
> row reads "2" and the cases are indistinguishable.

**Phase B — idempotency keys: ✅ done.**

`DurableCallIdempotencyKey` (`engine/idempotency.go`) derives
`base32(sha256(workflowID || 0x00 || runID || 0x00 || step))`. Every input is deterministic
on replay, so a resumed workflow derives the same key the original run used. All five
durable-call sites route through one `callService` helper — deriving the key per call site
is how the step number and the recorded event drift apart.

**Deviation from `docs/durable-call-intent-design.md` §4, deliberately.** The design adds
the key as a parameter to `ServiceCaller.Call`, and names the cost: *"a breaking change for
external callers and plugin authors. That is the main expense of this tier."* Implemented
instead as an **optional** `IdempotentCaller` interface: same mechanism, no existing
implementation stops compiling. The trade is that a caller which could honour keys but has
not been updated silently does not — so `CallerHonoursIdempotencyKeys` makes that
detectable, and the crash test asserts the service actually received keys before trusting
its result. If the interface is ever collapsed into `ServiceCaller`, nothing here forecloses
it.

Tests: key stability **across a real replay** (run to completion, truncate the history to
two events as a crash would leave it, resume, require the resumed steps' keys to match the
original run's) — proven to fail by making the derivation non-deterministic. Plus retry
attempts sharing one key, per-step/run/workflow distinctness, the NUL-separator ambiguity
case, and the fallback for plain callers.

**Cross-stream:** `cmd/cleat-worker/setup.go` is WS-3's. `dbServiceCaller` now implements
`IdempotentCaller` and sends the `Idempotency-Key` header, because the engine-side mechanism
is inert without a caller that implements it — and shipping a mechanism nothing calls is the
exact §1.4 shape this phase exists to avoid. Additive: `Call` is unchanged in behaviour.

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

#### 1.4 phase D — write-ahead intent — ✅ **DONE** (WS-2, 2026-08-05)

Migration `020` on all three dialects adds `event_history.intent_at`. **An event is pending iff
`intent_at IS NOT NULL AND checksum IS NULL`**, and the ordering that is the entire feature is

    commit intent  ->  dispatch  ->  commit outcome

A crash between the first and third leaves a pending row, which replay reports as ambiguous
instead of calling the service a second time.

**The three defects that made the deleted implementation unwirable are gone by construction, and
each is checked:**

- The sentinel is not in the `error` column, so the completion path's
  `WHERE response = '' AND error IS NULL` guard no longer refuses to overwrite it. `error` means
  only "the call failed".
- A pending row carries **no checksum at all**, so `VerifyWorkflowEvents` skips it rather than
  reporting corruption in the exact crash window this exists to handle.
  `TestCallIntent_PendingRowIsNotCorruption` fails if the intent write stores one.
- The chain is computed from `s.lastChecksum`, not from a database read, so it does not diverge
  under the adaptive flusher.

**The guarantee is observed from inside the call.** `TestDurableCall_CommitsIntentBeforeDispatch`
reads the workflow's history back through the store *while the call is in flight* and requires
the pending row to be visible. An implementation that wrote the intent afterwards would satisfy
every after-the-fact assertion and fails this one — proved by moving the write below the
dispatch, which fails all three dialects.

**Reachable from the shipped artifact, not only from an embedder.** `--write-ahead-intent-ops`
takes `service.operation` pairs. Without it the engine-side mechanism would be exactly what §1.4
is about: durability code that is tested, believed and unreachable. Cross-stream —
`cmd/cleat-worker/{config,setup}.go` is WS-3's, same justification as phase B's `dbServiceCaller`.

**A design-doc claim was wrong and is corrected there rather than worked around.** §5 said
`--no-per-step-flush` "defeats this entirely" and required rejecting the combination at startup.
That holds for an implementation routing the intent through `flushEvent`; this one writes through
the store and never consults `noPerStepFlush`. `TestDurableCall_IntentSurvivesNoPerStepFlush`
asserts the two are orthogonal on all three dialects. No startup check was added.

**Deviation on policy declaration.** The design leaves open whether semantics are declared at the
call site or on the service. The call site would need a new argument on the `DurableCall` host
function and every SDK that binds it — an ABI change, and `wasm/` and the SDKs are WS-3's. The
engine-level registration is the half that can be built without one, and forecloses nothing.

**Five falsifications, each failing only its own test:** the ordering, the pending row's absent
checksum, the detector, the completion fence, and the `pending` expression in `LoadEventHistory`.
The fence one corrected the test rather than confirming it — MySQL's `RowsAffected` counts rows
*changed*, not matched, so re-completing with identical values reported 0 whether or not the
guard was in the `WHERE` clause, and the assertion would have passed against a store with no
fence at all. The second completion now carries a different outcome, so all three dialects
discriminate.

**T3, the crash scenario, is done (2026-08-05)** and is the evidence this phase existed for.
`TestCrashWithWriteAheadIntentDoesNotRepeatTheCall` SIGKILLs a real worker with the third of
three durable calls committed at the service and unanswered, on the same fixture as §2.4's
test, and differs from it in exactly one flag:

| | Ship | what recovery did |
|---|---|---|
| at-least-once (§2.4's test) | **2** | repeated a call that may already have happened |
| `--write-ahead-intent-ops payments.Ship` | **1** | did not repeat it |

The pending row is asserted to exist *before* the crash, not after, so the claim is about
ordering rather than outcomes. Non-vacuity: with the flag removed the test fails at that
assertion — no pending row exists — and the sibling test pins `Ship=2` for the same fixture.

**It also found §3.22**, which is the reason the test records the workflow's terminal state
rather than asserting it: the ambiguity is detected, reported to the guest, reported back by
the guest — and then overwritten by a second completion, so the workflow reads as `done`.

#### 1.4 phase E — automatic resolution — ✅ **DONE** (WS-2, 2026-08-05)

Detection on its own converts a rare silent duplicate into a rare permanent failure, which for
some workloads is worse: a workflow that learns its outcome is unknown, and has no way to find
out, is stuck. `AmbiguityResolver` is the way out, and it costs nothing when unused.

When replay finds a pending intent row the engine asks the resolver about **the idempotency key
the original attempt actually sent** — derived through `DurableCallIdempotencyKey`, the same
call `callService` makes, so the resolver is looking up something that really happened. If the
resolver answers, the outcome is written over the pending row and replay carries on as though
the call had returned normally. Which it did: the crash lost the answer, not the effect.

**The resolution is persisted, and that is the load-bearing part.** A resolution that is used
but not recorded means the next replay finds the row still pending and asks again — and a
service that answers differently the second time, or is unreachable, makes the same step
resolve one way on one replay and another way on the next. `ResolveCallIntent` exists
separately from `CompleteCallIntent` for exactly one reason: it runs during replay, where the
session is reading history rather than building it and holds no checksum chain, so the previous
checksum is read from the row before it, inside the same transaction. That is safe here in a
way it was not for the deleted `flushCallIntent` — everything before a pending row is persisted
by definition, because the crash that created the row happened after them.

**Every way of declining leaves the ambiguity exactly as it was** — reported, not resolved, and
above all not repeated: no resolver configured, a resolver with no record, a resolver that
errored, and a resolution that could not be recorded. The last is the interesting one and has
its own test: using it would be the determinism bug described above.

Falsified two ways: never consulting the resolver, and using its answer without persisting it.
Both fail on all three dialects.

**Still open in phase E:** the typed error. The design asks for a structured value carrying
step, service, operation and key; today the detail is a formatted string inside the workflow
result. That is §3.22 step 3.

Three different things are called "ErrAmbiguous", and keeping them apart is what says how much
of this needs an ABI change — which is less than this entry previously claimed.

1. **`engine.ErrorCode.ErrAmbiguous`, value 5** (`engine/errors.go`). Host-side, stored in the
   `error_code` column, present since the first commit. `NewAmbiguousError` is declared and
   called from nothing but its own test.
2. **`cleat.CallErrorCode`**, the guest ABI enum. Has no ambiguous member; 5 there is
   `CallErrorPermissionDenied`. This is what a workflow author's `switch e.Code` reads, and an
   ambiguous call arrives as `[0]`, Unknown. Adding a member is possible — the wire field is 32
   bits and 6 is free — but every SDK carries its own copy (`python-sdk/cleat_sdk/host_calls.py`
   has a literal `{0..5}` dict), and those are WS-3's.
3. **`engine/callerrors.go`**, which redeclares only the three of the guest enum the engine can
   actually select.

An earlier revision of this entry said the design's "`ErrAmbiguous` already exists as error
code 5" does not hold in this tree. **That was wrong** — it holds exactly, for (1). The claim
was checked against (3), a different enum. What is true is narrower: the code exists, and
nothing populates it, because the ambiguity never reaches the host as a failure at all.

~~**Still open in the phase:** E (the resolution hook and a typed `ErrAmbiguous` carrying step,
service, operation and key — today ambiguity is reported inside the workflow result *string*)
and F (admin force-resolve for a pending step, whose prerequisite §3.20 now exists). §3.22
should be fixed before either: both are about delivering an answer that this discards.~~
~~`pendingSentinel` is still detected alongside `Pending` because
`tests/integrity` exercises it directly; retiring it belongs with E.~~
**Retired 2026-09-02, with phase F's tail.** `isPendingIntent` now reads `Pending` alone.

The sentinel was `"__CLEAT_PENDING_INTENT__"` in `EventRecord.Err`, the representation the
deleted `flushCallIntent` would have written. **Nothing in any deployment ever wrote it**: the
write side was deleted rather than wired in, because every completion path guarded its upsert on
`error IS NULL`, so a sentinel row could never be completed and would have stuck pending forever.
So the detector could not fire on any real crash, and `PendingSentinel` was exported for exactly
one consumer — the test that injected the value nothing produced.

**The tests were converted rather than deleted, which is the part worth stating.**
`TestPendingSentinelDetection` (now `TestPendingIntentDetection`) drives a real WASM workflow and
checks that an ambiguous step propagates through the call chain — coverage worth keeping. Only
its *injection mechanism* was dead, so it now sets `Pending` instead. Same for the three engine
tests.

Verified the conversion did not make them vacuous, which is the risk when a test's fixture
changes: neutering `isPendingIntent` to `return false` fails `TestPendingIntentDetection` and
three engine tests. A conversion that quietly stopped asserting would have passed that check
green.

**Phase F landed 2026-09-02, and phase E and §3.22 landed before it.** `engine.ResolveStep`
(`engine/admin_intent.go`) records an outcome for a call left pending by a crash, on the
authority of an operator who checked the external service, reachable at
`POST /api/admin/instances/{id}/steps/{step}/resolve`.

**It needed no new SQL, and that is the finding.** Phase E built `ResolveCallIntent` on all
three dialects — tenant-scoped, requiring the row to still be pending, computing the checksum
from the preceding row inside its own transaction — and left it reachable from exactly one
caller: `resolveAmbiguity`, which **returns immediately when no `AmbiguityResolver` is
configured**, as in most deployments. So the mechanism was complete and unreachable, and a
workflow reported `[AMBIGUOUS]` on every replay forever while its own error text told the
operator to go and check the external service. Phase F is the path to it, dialect-agnostic,
built on phase E's per-dialect work.

**The provenance lives on the resolved row, not only in the audit event.** The response is
written as though the call had returned it, because that is what replay must see — a workflow
cannot branch on "a human said so". `EventRecord.ResolvedBy` is written in the same statement
and says the outcome was *asserted rather than observed*. That placement is deliberate: the
resolution and the audit event are separate statements and cannot be made atomic without
per-dialect SQL, so the one fact a reader must not lose is the one that cannot be lost
separately. A failed audit append costs a timeline entry and is reported loudly, naming what
did happen so the operator does not retry a step that is no longer pending.

**Resolving twice is a conflict rather than a silent overwrite**, and the guard is in
`ResolveStep` rather than left to `ResolveCallIntent`'s pending predicate — both refuse, but
only the explicit one produces the "generation mismatch" wording `handleAdminOpError` maps to
409. The tests assert the wording for that reason: rewording these errors silently changes the
HTTP contract.

**Not verified on SQL Server** — the dialect skips in this sandbox (`WS2-STATUS.md`), though
the resolution goes through per-dialect SQL, so CI is doing real work here rather than
confirming a local result.

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

`ReapStaleInstances` (`engine/store_lifecycle.go:615-633`) and `TerminateWorkflow`
(`engine/db.go:1056-1076`) clear `assigned_to` but leave `generation`. Weakens the token to
defence-in-depth-in-name-only. Bump it in both.

**Status added 2026-08-06 — the work was done, the marker never was.** This heading read as
open planned work ("~0.5 session") long after it shipped, which is the inverse of the stale-✅
problem and just as misleading: an auditing agent reported it as "status genuinely
indeterminate from the document alone". Verified against the tree on 2026-08-06 — both paths
bump `generation` on all three dialects:

- terminate: `engine/db.go:1086`, `engine/mysql_ops.go:1177`, `engine/mssql_operations.go:192`
- reap: `engine/store_lifecycle.go:732`, `engine/mysql_lifecycle.go:721`,
  `engine/mssql_operations.go:35`

Re-derive with `grep -rn "generation = generation + 1" engine/*.go | grep -v test` (18 sites,
including `store_admin.go`'s six from §3.20's force-resolve).

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

**Correction, 2026-08-07.** "and had neither" was right. "Fixed in `HEAD`" was wrong about
**both** files, and the heading should be read as describing the parse error only.

Making each file *parse* made it run. It did not make either one do its job, and in both
cases the reason it did nothing was never the reason anyone fixed. Both are now deleted
rather than repaired a second time: `release-notes-check.yml` in §1.12a, `ai-pr-review.yml`
in §1.12b.

The generalisation, which is the part worth carrying forward: **a startup failure hides
every other defect in the file behind it.** Nobody can find the four reasons a check is
inert while the check is not running at all — so fixing the parse error feels like the
repair, and the repo gets a green check and a false belief instead of a red one and a true
one. When a workflow starts running for the first time, that is the moment to ask what it
actually does, not the moment to close the item.

---

### 1.12a The release-notes gate did nothing after it parsed either — deleted 2026-08-07

Parsing was the first of four reasons `release-notes-check.yml` never gated anything. The
other three each sufficed alone:

- **Its trigger label could not exist.** The job did nothing unless the PR carried a label
  named `[FEATURE]` or `[BUGFIX]`. Neither is among the repository's twelve labels, and no
  PR has ever carried any label at all:

      gh label list --limit 100
      gh pr list --state all --limit 200 --json number,labels \
        --jq '[.[] | select(.labels | length > 0)] | length'    # 0, 2026-08-07

- **Nothing could ever have applied them.** The auto-labeler removed in PR #366 (`a21de13`,
  itself never having applied a label) defined only `area/*` names —
  `git show a21de13^:.github/labeler.yml`. No workflow, template or human convention in this
  repository has ever produced a `[FEATURE]` label.

- **It read labels; the PR template asks for a checkbox.** `.github/PULL_REQUEST_TEMPLATE.md`
  offers a `[FEATURE]` tickbox under "Change type". Ticking it edits the PR *body*, which the
  label query at the top of the job never reads. So even full compliance with the template
  could not arm the check.

And the job carried `continue-on-error: true`, so the `exit 1` at the end was unreachable as
an outcome regardless.

The Release notes section of the PR template is kept — it is useful guidance, and it is now
honestly unenforced rather than appearing to be a gate. Enforcing it would mean keying on
something that exists, the natural candidate being the branch prefix that
`Validate branch name` already requires; that is a policy change, not a repair, and is not
made here.

---

### 1.12b The AI PR review posted only failure notices — deleted 2026-08-07

Removed at the owner's request: no GitHub-side AI review. Recorded here because of what
looking at it turned up, which is §1.12a's shape a second time.

`ai-pr-review.yml` parsed and ran after §1.12. Its last 29 completed runs all report
`success`, and every one of them posted the same comment:

    ## AI Review
    AI review unavailable (API error). A human reviewer must review this PR.

    gh run list --workflow=ai-pr-review.yml --limit 30 \
      --json conclusion,status --jq '[.[] | select(.status=="completed")] |
      group_by(.conclusion) | map({c: .[0].conclusion, n: length})'

Two independent reasons it reported success while doing nothing: `continue-on-error: true`
at job level, so failure was not an available outcome; and the script catches a non-ok API
response and substitutes that fallback text, so the step exits 0 even without it.

The API key was unset or invalid. **Supplying one would not have made it work**, and that is
the finding:

    git diff origin/main...HEAD > /tmp/pr.diff

The base branch is `develop`. That command diffs from `merge-base(main, HEAD)`, so for any
PR into develop it emits the whole accumulated develop-to-main delta alongside the change
under review — 370 commits, 1089 files, 241502 lines, measured 2026-08-07:

    git rev-list --count origin/main..origin/develop
    git diff origin/main...origin/develop | wc -l

The script then truncates to the first 50000 **characters**. A working key would have
produced a confident review of the first few files of an unrelated diff, on every PR — worse
than the failure notice, because it would look like a review. The three checks a reader
would apply to judge it (does it run? does it report success? did it post something?) all
answered yes.

`/code-review ultra` remains available and is user-triggered.

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

**Regression coverage extended to a real database, 2026-08-04.** The guard for this was
`TestShardedClaimWorkflows_DoesNotOverClaim`, which runs entirely against `mockShardStore`:
its claim function returns exactly the budget it is handed and increments a counter the test
itself maintains. §2.17's evidence, though, was a *database* observation — rows left
`running` with no executor.

`TestShardedClaimWorkflows_DoesNotOverClaim_RealDB` closes that gap: three real
`PostgresStore`s on separate pools, the real claim SQL under `FOR UPDATE SKIP LOCKED`
contention, and the assertion §2.17 actually made — how many rows the database is left
holding, not how long the returned slice is. Reinstating the bug reproduces the original
numbers exactly:

```
a claim for 2 left 6 row(s) 'running' in the database but returned 2 --
4 row(s) are claimed by a worker that will never run them
```

**Honest scope.** The mock test also fails against that mutation, so this is not filling a
hole the old test left for the classic bug. What it adds is that the assertion is tied to row
state rather than to a counter the test controls, and that the real store is exercised
respecting the budget it is given — the shape of §2.11's still-unexplained "asked for 3, got
10", which a mock returning exactly `l` cannot represent. The three shards share one database,
so it exercises the fan-out and row state, not cross-database routing.

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

### 2.23 `StartChildWorkflowInSchema` — same omission, but a one-line fix would be a false fix — ✅ **FIXED**, and ⬛ **SUPERSEDED 2026-09-02: the feature was removed (§3.78)**

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

Found while wiring §2.26. `enforceParentClosePolicy` is what applies a *closing* parent's
policy to its children: `TERMINATE` children are failed, `REQUEST_CANCEL` children get the
cancellation flag. It discarded every error it produced.

- **PostgreSQL** (`store_lifecycle.go`): neither `tx.ExecContext`'s nor `tx.Commit`'s return
  value is assigned, in either of its two transactions.
- **MySQL** (`mysql_lifecycle.go`): same, and it does not use a transaction at all — two bare
  `s.db.ExecContext` calls with the results dropped.
- **MSSQL**: same shape; **fixed** here, since a retry wrapper is meaningless on a function
  that cannot see its own failures.

The function is void and its callers treat it as best-effort post-commit cleanup, so nothing
downstream notices either. When it fails — a deadlock against a worker claiming one of those
children is the obvious way — **the children of a terminated parent keep running**, and there
is no log line, no metric and no error anywhere in the system.

This is the §1.2 shape one level further out: not an unchecked `RowsAffected`, but an
unchecked everything.

**All three dialects fixed, 2026-08-04.** Each checks its errors and logs what it could not
do, naming the consequence rather than the statement — *"children of a closed parent are
unaffected by its close policy"*. MSSQL additionally retries on a rollback-guaranteed error
(§2.26); PostgreSQL and MySQL have no equivalent retry infrastructure and a deadlock there is
still a hard failure, now at least a visible one. **MySQL also gains the transaction it never
had**: two bare `s.db.ExecContext` calls meant TERMINATE children could be failed while
REQUEST_CANCEL children went unflagged, with nothing to indicate a partial application.

**The feature itself was never tested, and it works.** Nothing in the repo exercised parent
close policy on any dialect — the only existing references assert this SQL is *absent* on the
fence-lost path, which says nothing about whether it does the right thing when it should run.
`TestEnforceParentClosePolicy` now covers all three policies at once against every configured
backend, so a change that handles one and breaks another cannot pass. Confirmed able to fail:
pointing the TERMINATE predicate at a policy name that matches nothing fails on all three with
`TERMINATE child status = "ready", want "failed"`.

**Observation, not yet a claim:** no dialect's `TerminateWorkflow` calls
`enforceParentClosePolicy`. The policy is applied by `CompleteWorkflow`, `FailWorkflow`,
`MoveToDeadLetterQueue`, `ContinueAsNew` and `FinalizeWorkflowSegment` — so a parent that is
*terminated* leaves its children running whatever their policy says. Whether that is a defect
depends on a contract this repo does not document anywhere: there is no user-facing
description of parent close policy at all. Worth settling before changing behaviour.

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

#### Original entry — as written before the fix. Not a live status.

> Demoted from `###` to `#####` on 2026-08-31, and its 🔴 removed. It kept a top-level
> heading carrying a live-looking marker directly *below* the ✅ entry that superseded it, so
> `grep '^### .*🔴'` — which is how anyone finds the open items — returned this and nothing
> else, and the repo read as having one open red item when it had none. The body below is the
> original text and describes code that no longer exists: `asDir`, `nodePath` and the
> `transformFile` candidate search are all gone, and `runVetAS` runs `npx asc … --noEmit` and
> propagates the exit status (`cmd/cleat/main.go`). The line numbers it cites resolve to
> unrelated code.

##### 2.43 `cleat vet --target assemblyscript` cannot fail — the original report

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

### 2.60 Per-step event flush never ran on MySQL or SQL Server — ✅ **FIXED** (WS-2, 2026-08-04)

Same shape as §1.4's RLS blocker, on a different axis. `engine/flush.go`'s `insertEventSQL`
is hand-written PostgreSQL — `$N` placeholders, `ON CONFLICT`, and since the RLS fix
`set_config`. `cmd/cleat-worker/main.go` opens whatever the `--db` DSN produces and
`setup.go:1580` passes it to `engine.WithDB` unconditionally, under the comment *"Always
provide DB so per-step flush and adaptive flusher work."* `flushEvent`'s only guard is
`e.db == nil`.

So on the other two dialects **every per-step flush failed at parse time**, and
`engine/lifecycle.go:180` logged it and carried on. Measured live on all three, with
PostgreSQL as the control:

| dialect | before | error |
|---|---|---|
| postgres | PASS | — (control) |
| mysql | FAIL | `Error 1064 … near 'CONFLICT (workflow_id, step) DO UPDATE SET …'` |
| mssql | FAIL | `'set_config' is not a recognized built-in function name` |

Nothing was lost outright — `FinalizeWorkflowSegment` appends the whole segment through the
dialect-correct store path at segment end. What was lost is the reason per-step flush
exists: surviving a crash **mid**-segment. A MySQL or SQL Server deployment silently got the
behaviour `docs/durable-calls.md` attributes to `--no-per-step-flush` ("higher throughput,
weaker crash safety") without setting the flag, and with nothing observable from outside.

Fixed with an unexported `perStepEventFlusher` interface. `MySQLStore` and `MSSQLStore`
implement it; `PostgresStore` deliberately does not, so the primary dialect keeps the path it
has always had.

**The judgement call worth reviewing is `event_count`.** The store append maintains it, but
`FinalizeWorkflowSegment` appends the same events again — idempotently for rows, but its
increment is unconditional. Counting in the per-step path too would count every event twice:
`GetEventCount` doubles and `--max-events-per-workflow` trips at half the configured limit,
and neither symptom looks like a flush bug. So `flushEventForStep` passes
`incrementCount=false`, matching PostgreSQL's raw insert, which has never touched
`event_count` either. `TestPerStepFlushDoesNotDoubleCountEvents` pins it; flipping the flag
fails it on both dialects.

**Found by trying to write §1.4 phase D's migration.** Phase D adds a column to
`event_history` in three dialects; standing up the three dialects to do that is what surfaced
this. The plan's instruction not to build the fix before the observation held again, for a
reason it did not anticipate — for the second time in two sessions.

#### 2.60a Local SQL Server on Apple Silicon — the §1.7 blocker is removable

`PARALLEL-WORKSTREAMS.md` records that §1.7 was deliberately skipped in every recent session
because verifying an RLS migration needs a live MySQL and SQL Server, and *"the first task in
§1.7 is not the migration — it is standing up MySQL and MSSQL you can actually test against."*

`mcr.microsoft.com/mssql/server:2022-latest` cannot do it on arm64. It is amd64-only and
QEMU rejects its address mapping outright:

```
/opt/mssql/bin/sqlservr: Invalid mapping of address 0x4005353000 in reserved
address space below 0x400000000000
```

**`mcr.microsoft.com/azure-sql-edge:latest` runs natively on arm64** and is a real SQL Server
engine — `Microsoft Azure SQL Edge Developer (RTM) - 15.0.2000.1574 (ARM64)`. Against it the
repo's MSSQL engine tests are **296 pass / 2 fail / 0 skip**, and both failures are the
`finalize_workflow_status` procedure that `engine/testutil` never defines — the gap already
recorded under Phase 0's caveat 4. `sp_set_session_context` is present, so §2.71's fix is
testable here too.

```
docker run -d --name cleat-ws2-mssql -e ACCEPT_EULA=1 \
  -e MSSQL_SA_PASSWORD='CleatTest123!' -p 1434:1433 \
  mcr.microsoft.com/azure-sql-edge:latest

CLEAT_TEST_MSSQL='sqlserver://sa:CleatTest123!@localhost:1434?database=cleat&encrypt=disable'
```

Two caveats, both load-bearing. `encrypt=disable` is required: Edge's self-signed certificate
has a negative serial number and current Go rejects it (`x509: negative serial number`) — an
opaque TLS handshake failure, not an auth error. And Edge is a **15.0/2019-era subset**, not
SQL Server 2022; it is enough to run this repo's suite and to stop writing MSSQL migrations
blind, but a green run on Edge is not a claim about 2022. CI still has the real thing.

#### 2.60b The engine suite is not deterministic against MySQL or SQL Server — ✅ **FIXED** (WS-2, 2026-08-04)

> ~~**Four MySQL engine tests fail on `develop`.** Surfaced by running the engine suite
> against a live MySQL for the first time. Verified pre-existing: they fail identically on a
> stashed tree. Not fixed here — they are unrelated to the flush path and each wants its own
> diagnosis.~~
>
> ~~`TestCascadeDelete/mysql` — `Error 1170: BLOB/TEXT column 'sticky_worker_id'…`, a
> `engine/testutil` schema defect. `TestDeliverSignal/mysql` — MySQL's `JSON` column
> normalises whitespace; the test compares bytes. `TestPollAndClaimSignal/mysql`,
> `TestPollSignal_NonDestructive/mysql` — `Error 3140: Invalid JSON text`. The first is a
> test-schema bug; the other three are the same question, whether a signal payload must be
> JSON, which PostgreSQL's `TEXT` accepts and MySQL's `JSON` does not.~~
>
> **Wrong, and kept here on purpose.** Two errors. *(a)* PostgreSQL's column is `JSONB`, not
> `TEXT` — all three dialects require JSON, so the dialects did not "disagree" about the
> requirement at all; see §2.60c for what the difference actually was. *(b)* "Verified
> pre-existing" was the worse mistake. The stashed-tree comparison was run against a database
> those same tests had already populated, so it established only that both trees hit the same
> accumulated state. On a **freshly created** MySQL database `develop`'s engine suite is
> **green**. There were never four deterministic failures to fix.

What is actually there is worse than four broken tests, because it does not show up as a
stable red. Four consecutive full runs against live MySQL and SQL Server produced four
different failure sets:

| run | failures |
|---|---|
| 1 | `TestCascadeDelete/mysql`, `TestDeliverSignal/mysql`, `TestPollAndClaimSignal/mysql`, `TestPollSignal_NonDestructive/mysql` |
| 2 | `TestMySQLIntegration_LoadDAGSpec`, `TestGetPendingUpdateRequests/mysql`, `TestCompleteUpdateRequest/mysql` |
| 3 | `TestFinalizeWorkflowSegment_ZombieWriterFence/mssql` — repeated three times, identical each time, so the non-determinism is between runs that change the database, not run-to-run coin-flipping |
| 4 (fresh DBs) | `TestTenantIsolation_ConcurrencyKeys/mysql` |

Both of the last two **pass in isolation, on the same database, immediately before and after
failing in the full suite** — and the fence test passed *with* a change applied and failed
*without* it, which is the inverted result that rules out attributing any of this to the code
under test.

**The mechanism.** `engine/testutil` holds two independent hand-written MySQL schemas —
`schema.go`'s `DialectMySQL` block and `mysql_schema.go` — plus a third definition in
`migrations/mysql/001_schema.sql`. All use `CREATE TABLE IF NOT EXISTS` against one shared
database, so **whichever test runs first defines the tables for the whole package**, and Go's
ordering decides which. Rows also accumulate: `CleanupPostgresTestData` is PostgreSQL-only
and `truncateAll` does not reach everything on SQL Server, so reaping and tenant-isolation
tests see other suites' leftovers.

This is §2.39's shape — schema DDL racing another package's DML against one shared database —
on the two dialects that never got §2.39's fix. PostgreSQL has the advisory lock *and* the
content fingerprint that makes the apply run once; MySQL and SQL Server have neither.

**Three real divergences were found underneath it and are fixed** (`engine/testutil/schema.go`,
each one this file's MySQL block disagreeing with the shipped migration):
`workflow_instances.sticky_worker_id` and `concurrency_keys.workflow_id` declared `TEXT` where
the migration says `VARCHAR(255)`, so the indexes this same file creates over them cannot be
built; and `workflow_update_requests.tenant_id` missing the migration's `DEFAULT`, so an
insert that omits it fails against a schema the product never ships.

**Fixed by collapsing to one definition per dialect.** `SetupMinimalSchema` and
`SetupFullSchema` now both route to the single schema for that dialect: the real migration file
for PostgreSQL, `SetupMySQLFullSchema` for MySQL, `SetupMSSQLFullSchema` for SQL Server. 368
lines of duplicated DDL deleted.

The index creation had to move with them, and that turned out to be the actual seam: the
dedicated files created **no** indexes and the `schema.go` arms created eight, so which entry
point a test called changed the schema it got, on top of which test ran first. `SetupFullSchema`
is kept as an alias rather than deleted — roughly forty call sites use it, and the
minimal/full distinction is precisely the line the duplication grew along.

**Three full runs against freshly created MySQL and SQL Server databases: green, green,
green.** That is the evidence for the fix, and it is deliberately not a single run — a single
green run on these dialects was never evidence, which is the part worth inheriting and why the
original wrong diagnosis above is struck through rather than deleted.

The fingerprint treatment `applyPostgresSchemaFile` has is *not* part of this and is still
worth doing: it would stop the DDL re-running per test, which is a cost and a DDL-versus-DML
deadlock risk (§2.39) rather than a correctness problem now that there is only one schema to
apply.

#### 2.60d `CleanupPostgresTestData` is an unqualified `DELETE FROM` on eleven tables — 🔶 **CLEANUP NOW FAILS LOUDLY (2026-08-31); ISOLATION STILL OPEN**

```go
for _, table := range tables {
	if _, err := db.Exec("DELETE FROM " + table); err != nil {
		t.Logf("cleanup: delete from %s: %v", table, err)
	}
}
```

No `WHERE`, no tenant qualification, and a failure is `t.Logf` rather than `t.Fatalf` — so a
cleanup that silently does nothing is indistinguishable from one that worked. Every package
that points at the same `CLEAT_TEST_DB` shares those eleven tables, and Go runs packages in
parallel by default, so one suite's cleanup deletes another suite's live rows mid-run. This is
what made `tests/crash` need a database of its own (§2.4) rather than a fix here.

**Two things make it worse than it reads.** The name says Postgres, but the SQL is
dialect-neutral, so `store_backends_test.go` calls it with the MySQL and SQL Server handles too
and wipes those databases as thoroughly. And it is the most likely remaining source of the
cross-suite state that §2.60b's schema collapse only half addressed — the collapse fixed *which
schema* you get, not *whose rows* are in it.

**Direct evidence, 2026-08-04.** After §2.60b landed, the engine suite is green on freshly
created MySQL and SQL Server databases and still fails on a *reused* one —
`TestFinalizeWorkflowSegment_ZombieWriterFence/mssql` and
`TestFinalizeWorkflowStatus_SQLFenceGuard_MSSQL`, both of which pass again the moment the
database is dropped and recreated. §2.60b fixed *which schema* you get; this is *whose rows*
are in it, and it is the whole of what remains.

Fixing it properly means deciding what test isolation is: a database per package (what
`tests/crash` does, and it works), a tenant per package with tenant-scoped deletes, or
transactions rolled back per test. That is a bigger decision than the ~40 call sites suggest,
which is why it is recorded rather than done here. The cheap intermediate step — make the
failure a `Fatalf` — is not obviously right either, because several callers currently rely on
the delete failing harmlessly on tables their dialect does not have.

#### Part 1 — loud cleanup (2026-08-31). The isolation decision is still open.

**Decision taken:** a database per package, extending what `tests/crash` already does. Rejected:
tenant-scoped deletes (two of the eleven tables have no `tenant_id` — measured below — and it
would mean editing ~74 call sites, each of which fails *silently* if it gets the tenant wrong),
and per-test transaction rollback (the system under test is a durability engine whose crash
recovery, fencing and reaping are *about* committed state; rolling back tests something else).
That work is **not done**; this entry covers the precursor only.

**What landed.** `t.Logf` → `t.Fatalf` on a failed delete, plus the check that actually matters:
after deleting, verify the tables are empty. An error was never the expensive failure here. A
`DELETE` on a connection whose rows are hidden from it removes nothing *and reports no error* —
PostgreSQL RLS filters it to the caller's tenant, and SQL Server applies its security policy to
every principal including sysadmin. §3.37 is precisely that: `CleanupMSSQLTestData` deleted
nothing, reported success, and rows piled up until an unrelated fixture collided on a primary
key, which is §2.71's 141-failure signature. `t.Fatalf` alone would not have caught it.

The emptiness check is one round trip (`UNION ALL` of counts) because cleanup runs on the order
of a hundred times per suite, and it names *every* dirty table rather than the first.

**The stated blocker did not exist** — *for the eleven tables in the list at the time.* Measured
2026-08-31 against live PostgreSQL 16 and MySQL 8: all fifteen candidate tables exist in both, so
no delete in either list could fail that way. SQL Server is the only dialect where a table can
legitimately be absent, and it already checked `sys.tables` before deleting.

**That claim was wrong the moment the list grew, and part 3 proves it.** The measurement was
taken against a *fully migrated* database. `SetupMinimalSchema` creates a subset, and four of the
tables PostgreSQL was missing are not in it — so widening the list without an existence check
fails every minimal-schema test on `relation "tenant_api_keys" does not exist`. The blocker this
entry recorded was real; it just did not bite the eleven tables that were already there. See
part 3.

**A coverage gap found while measuring, deliberately not fixed here.** The three lists have
drifted: MySQL and SQL Server clear 15 tables, PostgreSQL only 11. It is missing
`tenant_api_keys`, `workflow_tags`, `workflow_routing` and `plugin_defs`, all of which exist.
Adding them changes *what gets deleted* rather than how failures are reported, so it is a
separate change with its own blast radius — not bundled.

Re-derive:

    go test ./engine/testutil/ -run TestNonEmptyTables -v

Mutation-tested: making the verification blind to rows (dropping the `n > 0` branch) fails both
new tests, the first with "a table with a row in it was reported as empty".

#### Part 2 — a database per suite (2026-08-31). `-p 1` is still in place.

`SuiteTestDB(t, "<suite>")` (`engine/testutil/packagedb.go`) returns a PostgreSQL connection to
`cleat_test_<suite>`, creating it and applying the shipped migrations on first use. It
generalises `tests/crash`'s `ensureCrashDatabase`, which had already taken a database of its own
for this exact reason and recorded the same diagnosis.

`engine/testutil`'s own DB-backed tests now use it, which removes the collision inside
`./engine/...` — two packages, previously one database, and the reason that CI entry carries
`flags: -p 1`.

`TestTestDB` deliberately still calls `TestDB`: it is the test *for* `TestDB`, and swapping it
would leave the function uncovered while the name went on claiming otherwise. It is safe on the
shared database because it only reads.

**There is no fallback to the shared database**, by design. A suite that silently got the shared
one back would pass every test here and delete another package's fixtures mid-run, which is the
failure this exists to remove. A role without `CREATEDB` gets a `t.Fatalf` naming the connection.

**`-p 1` has NOT been removed**, deliberately. Removing it is a CI-wide behaviour change and
belongs in its own PR, once this has run in CI. Evidence gathered for that decision, all on this
machine:

| configuration | runs | result |
|---|---|---|
| `./engine/...`, no `-p 1`, **stale** local database | 3 | 1 failure (`TestClaimStickyWorkflows`) |
| `./engine/...`, **with** `-p 1`, stale database | 3 | 1 failure (`TestReleaseWorkflow`) |
| `./engine/...`, no `-p 1`, **freshly created** database | 4 | 4 green |

**The failures were stale local state, not package parallelism** — they occurred with and
without `-p 1`, and vanished when the database was dropped and recreated. That is the trap
CLAUDE.md records under "recreate your test databases"; it cost several runs here because MySQL
and SQL Server had been recreated earlier in the session and PostgreSQL had not. Four green runs
is a small sample, which is the other reason `-p 1` stays until CI has an opinion.

Re-derive:

    go test ./engine/testutil/ -run 'TestSuite|TestSwapDatabaseName' -v

Mutation-tested: making `SuiteTestDB` fall back to the shared DSN fails
`TestSuiteTestDBIsNotTheSharedDatabase` with `connected to "cleat", want "cleat_test_testutil"`.

**Still open:** every other suite that shares the database. The MySQL and SQL Server equivalents
of `SuiteTestDB` were the other item here until they were measured — see part 4, which is why
they are no longer listed.

`cmd/cleatctl` moved onto `SuiteTestDB` on 2026-08-31. It was the one remaining place where two
database-backed packages ran concurrently against one instance *without* `-p 1`: the `commands`
CI entry runs `./cmd/...`, and `cmd/cleatctl`'s teardown is the unqualified fifteen-table wipe
while `cmd/cleat-worker` has database-backed tests in four files.

**Characterise this honestly: a hazard, not a demonstrated failure.** The two packages provably
overlap — measured 2026-08-31, `cleatctl` 17:18:09.952→17:18:16.942 against `cleat-worker`
17:18:10.001→17:18:19.152, near-total overlap — and the wipe is unqualified, so the mechanism is
real. But six attempts to make it fail, including `./cmd/...` four times and both packages at
`-count=3` twice, produced no failure: `cleat-worker`'s database tests are ~0.04s each, so the
window in which it holds rows across `cleatctl`'s wipe is small. The change removes a hazard; it
is not known to have been causing red runs, and it should not be cited as though it were.

`cmd/cleat-worker` is the well-behaved one here — it uses the run-scoped
`CleanupTestData(…, runID)` rather than the blanket wipe.

#### Part 4 — MySQL and SQL Server did not need one (2026-08-31)

The next item on this list was "the MySQL and SQL Server equivalents of `SuiteTestDB`", on the
strength of `multi-db-ci.yml`'s own comment: both jobs run `./engine/... ./migration/...` under
`-p 1`, justified by *"three database-backed packages — engine, engine/testutil and migration —
against one database"*.

**That justification is false for those two dialects, and the work it implied has no user.**
Measured 2026-08-31:

- `engine/testutil` has no MySQL or SQL Server test. Every DB-backed test in it is PostgreSQL,
  and those jobs have no PostgreSQL, so all of them skip.
- Every MySQL and SQL Server test under `./migration/` creates a scratch database of its own
  (`newMySQLScratchDB`, `newMSSQLScratchDB`, and the fixed `cleat_migration_admin_role_test`)
  and drops it in cleanup. None touches the shared `cleat` database.
- `engine` is therefore the only package wiping that database — 24 call sites, which is the
  number `TestBlanketNonPostgresCleanupHasOneCaller` reports when `engine` is removed from its
  allowlist.

Probed rather than read, on MySQL 8.4: a sentinel row in `cleat.workflow_defs` survived
`go test ./engine/testutil/... ./migration/...` with `CLEAT_TEST_MYSQL` set, and no scratch
database was left behind.

    docker exec <mysql> mysql -uroot -pcleat cleat -e \
      "INSERT INTO workflow_defs (name, version, wasm_bytes) VALUES ('sentinel_probe', 1, 'x')"
    CLEAT_TEST_MYSQL=... go test -count=1 ./engine/testutil/... ./migration/...
    docker exec <mysql> mysql -uroot -pcleat cleat -e \
      "SELECT COUNT(*) FROM workflow_defs WHERE name='sentinel_probe'"   # 1

**The "3 failures in 8 runs" figure that comment carried was never a measurement of these jobs.**
It cannot have been: `engine/testutil`, the package it names as the other half of the collision,
has no MySQL test to collide with. It was a PostgreSQL measurement (`ci.yml`'s engine entry)
copied across, and it is what would have sent the next reader to build a MySQL `SuiteTestDB`
nothing would call.

**SQL Server was not probed locally, and the claim there is weaker.**
`mcr.microsoft.com/mssql/server:2022-latest` does not start under QEMU on arm64 (`Invalid mapping
of address ... in reserved address space below 0x400000000000`), and `azure-sql-edge`, which does
start, cannot apply the shipped migrations — `011_json_scalar_payloads.sql` needs `isjson`. The
SQL Server half rests on the call sites and on CI, not on a local measurement.

**`-p 1` stays in both jobs.** Removing it saves the ~2s `./migration/` takes beside
`./engine/...`, which is not a reason to drop a serialisation from a required check. What
changed is the comment, and the fact behind it is now checked rather than asserted:
`engine/testutil`'s `TestBlanketNonPostgresCleanupHasOneCaller` fails if any package outside
`engine` calls `CleanupMySQLTestData`, `CleanupMSSQLTestData` or `CleanupAllTestData` — the
condition that would make `-p 1` load-bearing and make a MySQL `SuiteTestDB` worth building.

Re-derive:

    go test ./engine/testutil/ -run TestBlanketNonPostgresCleanupHasOneCaller -v

Mutation-tested three ways, each failing for its own reason: adding a call site in `plugin/`
reports it by file and line; removing `engine` from the allowlist reports all 24 real sites
(proving the walk sees them); renaming the three helpers trips the separate "this guard is
measuring nothing" branch, so a rename cannot silently empty it.

#### `CleanupTestData` — errors were discarded, and its coverage was already right (2026-08-31)

The run-scoped helper deleted through `_, _ = db.Exec(...)` at all seven sites, so a cleanup that
failed outright was indistinguishable from one that worked — the same defect part 1 fixed for the
blanket cleanups, which this helper did not get at the time. Now checked, with the existence
filter so an absent table is still not an error.

**Its seven tables were never a gap, and I said otherwise before measuring.** They are exactly the
tables a workflow ID can select rows in: six carry a `workflow_id` column, and
`workflow_instances` carries it as `id`. Measured 2026-08-31 against the PostgreSQL schema, the
other eight are keyed by name or tenant (`workflow_defs`, `plugin_defs`, `workflow_schedules`,
`workflow_tags`, `tenant_api_keys`, `workflow_memory_stats`) or by a surrogate id unrelated to any
workflow (`workflow_routing` keys on `workflow_name`, `workflow_memory_samples` on `def_name`), so
none of them can be scoped this way at all. "7 of 15" was a complete list described as a partial
one.

`existingTables` gained SQL Server support for this — `store_backends_test.go` calls
`CleanupTestData` with all three dialects — matching on schema as well as name, because
`sys.tables` is keyed on name alone and an unqualified `DELETE` is not.

**A performance regression, found and fixed in the same change.** The first version of the
PostgreSQL branch issued one `to_regclass` round trip per candidate. Cleanup runs on the order of
a hundred times per suite, so 15 statements became ~1500 and the engine suite visibly slowed —
one interrupted local run sat past 600s. Now one round trip with N columns; engine is back to 74s
across three dialects, in line with every measurement before the change.

**Disclosed:** the new `t.Fatalf` on a failed delete has no test that fires it. Arranging a
delete that errors *and* survives the existence filter needs a contrived fixture, and a contrived
one would prove less than this sentence does.

**`-p 1` will not be removed, and that is a decision rather than a deferral.** Measured on
develop `63708d7`: the `engine` package takes 94–128s in CI and `engine/testutil` takes
0.35–1.5s, so running them concurrently saves about 1% of that job. Against that, dropping the
flag risks intermittent red on `develop` — a worse failure mode than a red PR — on the strength
of four local runs. What part 2 bought is that `-p 1` is no longer *load-bearing*; it stays as
cheap insurance.

#### Part 3 — the drifted lists, and the check that keeps them together (2026-08-31)

The three cleanup lists had diverged: MySQL and SQL Server cleared 15 tables, PostgreSQL 11. The
missing four — `tenant_api_keys`, `workflow_tags`, `workflow_routing`, `plugin_defs` — all exist
in the PostgreSQL schema, so they accumulated rows across every test in the package and surfaced
later as an unrelated test failing on a duplicate key.

Three things landed:

1. **The lists agree**, and `TestCleanupTableListsAgree` fails if they drift again. Membership
   *and* order, because they are deleted in foreign-key order and right-members-wrong-order fails
   at runtime on a constraint violation. The lists are now package-level vars so a test can see
   all three; previously they were function-local in three files that never referenced each other,
   which is why nothing could have noticed.
2. **An existence check for PostgreSQL and MySQL**, matching what SQL Server always did. This
   is what makes widening the list possible at all — see the correction above.

   **It has to answer the same question the DELETE asks, and the first version did not.** The
   cleanup issues `DELETE FROM <table>` unqualified, which PostgreSQL resolves through the whole
   `search_path`; the check filtered `information_schema.tables` on `current_schema()`, which is
   only the *first* entry on that path. Measured on a migrated database with
   `search_path = cleat, public` — the shape the Cluster job runs in, because
   `001_schema.sql` creates a `cleat` schema for `cleat.assert_tenant_set()`:

   | query | result |
   |---|---|
   | `current_schema()` | `cleat` |
   | `information_schema.tables WHERE table_schema = current_schema()` | **0 rows** |
   | `to_regclass('workflow_instances') IS NOT NULL` | **true** |

   So the check found nothing, cleanup deleted nothing, and `assertTablesEmpty` verified nothing
   — three successes, no work done — and the leftover rows surfaced in the Cluster job as
   `CreateSchedule: duplicate key value violates unique constraint`. That is the silent no-op
   §2.60d exists to remove, reintroduced by the check meant to support it, and it was caught only
   because CI runs a configuration the local database does not.

   Now `to_regclass`, which resolves exactly as the DELETE does. `existingTables` additionally
   fails loudly when *no* candidate resolves, because "nothing to clean" and "the schema is not
   on this connection's path" are indistinguishable from an empty result.
3. **`CleanupAllTestData(t, db, dialect)`**, which dispatches. `CleanupPostgresTestData` was
   being called with MySQL and SQL Server handles in `store_backends_test.go`'s dialect loop; the
   name said PostgreSQL, the SQL was dialect-neutral, and the mistake stayed invisible until the
   PostgreSQL path grew `current_schema()` and the other two failed with *"FUNCTION
   cleat.current_schema does not exist"*.

Re-derive:

    go test ./engine/testutil/ -run 'TestCleanupTableListsAgree|TestCleanupListsHaveNoDuplicates' -v

Mutation-tested: dropping `plugin_defs` from the PostgreSQL list — recreating the original drift
exactly — fails with `mysql clears 15 tables, postgres clears 14` and the same for mssql.

#### 2.60c A non-JSON signal payload was accepted on PostgreSQL and rejected elsewhere — ✅ **FIXED** (WS-2, 2026-08-04)

All three schemas require `workflow_signals.payload` to hold valid JSON, each saying so
differently: PostgreSQL `JSONB`, MySQL `JSON`, SQL Server `NVARCHAR(MAX)` with
`CHECK (ISJSON(payload) = 1)`. Only `PostgresStore.DeliverSignal` knew — it wrapped a non-JSON
payload in quotes on the way in, and `decodeSignalPayload` unwrapped it on the way out.
`MySQLStore` and `MSSQLStore` did neither, at six sites between them.

So `DeliverSignal(ctx, wf, "sig", "payload-1")` — reachable from the worker's signal endpoint —
succeeded on PostgreSQL and failed outright on the other two:

```
Error 3140 (22032): Invalid JSON text: "Invalid value." at position 0
```

Fixed by extracting `encodeSignalPayload` and applying it, with `decodeSignalPayload`, on all
three stores.

**A second defect fell out of it.** The PostgreSQL wrapping was `` `"` + payload + `"` ``,
which produces invalid JSON the moment the payload contains a quote or a backslash — and is
then rejected by the very column the wrapping exists to satisfy. `encodeSignalPayload` uses
`json.Marshal`. Demonstrated by restoring the concatenation:

```
DeliverSignal("he\"llo") on postgres:   pq: invalid input syntax for type json (22P02)
DeliverSignal("C:\\path\\to") on postgres: pq: invalid input syntax for type json (22P02)
```

So this was not only a cross-dialect inconsistency; PostgreSQL was rejecting ordinary payloads
too. `TestSignalPayloadRoundTripsOnEveryDialect` covers six payload shapes across all three
dialects and was watched failing on each half of the fix separately.

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

**One thing this does *not* establish, stated rather than glossed.** That the predicates are
*created* is read off the migration files; nothing asserts per-table policy coverage.
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
> **CORRECTION, 2026-08-06.** Points (1) and (2) in the bullet below are **no longer true**
> and have not been since #296: the wasmtime headers were vendored into
> `engine/wasmtimeinc` so the build tag could be dropped, and `engine/component_cgo.go` now
> carries only `//go:build cgo`. `grep -rn wasmtime_component_cgo` finds the string nowhere
> outside past-tense narrative, and `engine/engine.go:341` lists python in
> `WasmtimeLanguages`. Only point (3), `componentGetFunc`'s nil parent export index, is still
> open — see §3.31. **This paragraph is why the correction is here rather than a deletion:**
> it sat unchanged under a heading marked fixed and misled three sessions into reading the
> decomposition fallback's error as the state of Python-on-wasmtime, then a fourth on
> 2026-08-06 into reporting a working feature as broken.

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

#### Python: one blocker removed, one left, and it is a stale fixture

**Removed.** `registerEnvStubs` registered `env.abort` unconditionally as
`(msg, file, line, col i32)` — AssemblyScript's shape. A Linker holds one definition per
`(module, name)`, so a core module importing a *no-argument* abort, which is what the modules
inside a componentize-py component do, was rejected at instantiation:

```
incompatible import type for `env::abort`
expected type `(func)`, found type `(func (param i32 i32 i32 i32))`
```

The comment on that registration argued the mismatch was benign because
`DefineUnknownImportsAsTraps` would cover the other signature and "the first registration
wins". The first registration does win — that is the defect. Instantiation fails before any
trap-default can apply. `env.abort` is now registered with the type the module declares, read
from `Module.Imports()`, so both toolchains are served from one linker.

Measured effect on the checked-in Python component: instantiation advances from **instance 15
(module 3)** to **instance 81 (module 10)**.

**Left.** At instance 81 it fails on a different mismatch:

```
incompatible import type for `env::cleat_call`
expected type `(func (param i32 i32 i32 i32 i32 i32 i32))`
found type `(func (param i32 i32 i32 i32 i32 i32 i32 i32) (result i64))`
```

The module wants a 7-parameter `cleat_call` with no result. Both backends implement the same
8-parameter, `i64`-returning ABI — `engine/wasmtime_hostfuncs.go:24` and
`engine/imports.go:118` agree — so this is not a backend difference. **The checked-in
`call_all_plugins.wasm` predates the current host ABI.** It is a 19 MB artifact that no CI job
rebuilds, because no workflow installs `componentize-py`.

So Python-on-wasmtime cannot be settled with the fixture in the tree, and the next step is not
a code change: it is getting `componentize-py` into a CI job so the component is rebuilt
against the ABI the host actually implements. Until then the honest position is that the abort
blocker is fixed and verified, and what lies past it is unknown.

The abort fix is covered by `engine/wasmtime_abort_arity_test.go`, which builds its modules
from WAT rather than depending on that fixture — deliberately, so the regression test does not
inherit the staleness that blocks the thing it is testing.

**Correction, 2026-08-04, and it improves the picture.** The paragraph above concluded that
Python could not be settled because the checked-in fixture is stale. That is true of the
*fixture* and false as a statement about the repo: `e2e-cross-language.yml` installs
`componentize-py` and `engine/python_wasm_e2e_test.go` builds a component **fresh** on every
run. There has been a real signal all along.

It was invisible. `TestPythonWasmEndToEnd` has been **failing on `develop`** while the workflow
reported success, because the step running it pipes `go test` into `tee` without
`set -o pipefail` — so `tee`'s exit status is the step's. The very next step in the same file
carries that fix, with a comment explaining exactly this hazard. It was applied to one step
and not the other.

What the hidden failure says, on a freshly built component rather than the stale fixture:

```
instantiate instance 41 (module 4, 3 args, imports: [env GOT.mem GOT.func]):
  incompatible import type for `env::abort`
 (native component path first failed: wasmtime component CGo fast path not built)
```

That is this section's `env::abort` defect, and nothing further — the `env::cleat_call` ABI
skew is an artefact of the stale checked-in fixture, not of Python. So the abort fix and the
missing `pipefail` ship together: one makes the failure visible, the other fixes it.

Also worth keeping: `componentize-py`'s `componentize` step cannot run on macOS/arm64 here. It
dies with `EXC_GUARD / GUARD_TYPE_MACH_PORT — SET_EXCEPTION_BEHAVIOR on mach port`, which is
its embedded wasmtime installing a mach exception handler into a guarded port. Not OOM, which
was the first guess and was wrong. Linux runners have no such guard, so CI is the place this
gets exercised, and now it is the place it will be seen.

**What the now-visible signal actually says.** With the `env::abort` fix in place, a freshly
built component gets further and stops somewhere else:

```
before: instantiate instance 41 (module 4): incompatible import type for `env::abort`
after:  instantiate instance 52 (module 8): undefined element: out of bounds table access
```

Eleven instances further in. So the abort defect was real and is fixed, and **Python still
does not run on wasmtime** — it now fails in the decomposition path's table/element handling.
`backend_wasmtime.go` already has a retry for exactly that string ("Element segment / table
errors can result from adapter-provided tables conflicting with our placeholders"), and it
does not rescue this case. That is where the next attempt should start.

**The test was asking for the wrong thing.** `engine/python_wasm_e2e_test.go` had an
unconditional `if true` block registering `WithBackend("python", wt)` — forcing Python onto
wasmtime, which is a configuration the product does not use and Python does not survive. It
now registers `WasmtimeLanguages`, so it exercises what ships, and Python runs on wazero as it
does in the worker. When the component path can instantiate a Python component, adding
`"python"` to that one list switches this test over with no edit to it.

The sequence is worth keeping as a unit: a step without `pipefail` hid a failing test; the
test was failing because it forced a routing the product had already rejected; and the routing
had been rejected for a reason that was itself only half-diagnosed until the abort fix moved
the error. Four layers, each concealing the next.

#### Decision: Python stays on wazero — ✅ **SETTLED 2026-08-04**

Not an open item. The point of the work above was to find out *why* Python was on the fallback
runtime, and that is now known to the instance: it stops at `undefined element: out of bounds
table access` instantiating an inner core module, eleven instances past where the `env::abort`
defect used to mask it.

**Correction — "wazero runs Python correctly" was wrong, and CI said so within the hour.**
That sentence stood here and in the comment on `engine.WasmtimeLanguages`. It was inferred
from the product's routing, not from anything that ran. With the routing corrected so Python
actually reaches wazero, `TestPythonWasmEndToEnd` fails there too:

```
wasmtime: instantiate instance 52 (module 8): undefined element: out of bounds table access
wazero:   instantiate instance 8 (module 1): module[__main_module__] not instantiated
```

**Python is on wazero because that is where the product sends it, not because it is known to
work there.** The honest statement of the outcome: four of five languages run on wasmtime, and
the fifth runs on a backend where its one end-to-end test does not currently pass either.

That test is now skipped and recorded — `skip-baseline` 1→2 sites, `e2e-cross-language` budget
0→1 — rather than left failing or left hidden. Deleting it would lose the only thing in CI
that builds a Python component at all. Unskipping it is the acceptance test for whichever
instantiation path is fixed first.

Note the sequencing, because it is the whole lesson of this entry: the claim was written, and
the mechanism that would have contradicted it (`pipefail`) was fixed in the same change. The
correction arrived one CI run later. Had the `pipefail` fix not been part of this work, the
false claim would have sat in the source comment indefinitely, with a green workflow behind
it.

Two candidates remain for whoever wants to revisit, and neither is urgent:

- The `undefined element` retry in `backend_wasmtime.go` ("adapter-provided tables conflicting
  with our placeholders") is keyed on this exact error and does not rescue this case.
- The native component path in `engine/component_cgo.go`, behind the `wasmtime_component_cgo`
  build tag that no build sets, needs `CGO_CFLAGS` pointing at wasmtime-go's vendored headers
  and a `componentGetFunc` that can resolve exports nested inside an interface instance.

Adding `"python"` to `engine.WasmtimeLanguages` is the whole of the switch when one of those
lands; `engine/python_wasm_e2e_test.go` reads that list, so it moves over with no edit.

> **CORRECTION, 2026-08-06 — this landed.** #296 vendored the headers into
> `engine/wasmtimeinc` and dropped the tag, so the build-tag and `CGO_CFLAGS` halves of the
> second bullet are done; `componentGetFunc`'s nested-export bug is what remains (§3.31).
> `"python"` **is** in `engine.WasmtimeLanguages` (`engine/engine.go:341`) and has been since
> #296. See the longer correction at §1.5 for why this is annotated rather than deleted.
>
> Separately, and found on 2026-08-06: the reason Python skipped on developer machines was
> never this code path at all. `componentize-py` cannot run on macOS — its embedded wasmtime
> installs a mach exception handler into a guarded port and the process dies with
> `EXC_GUARD / GUARD_TYPE_MACH_PORT`, a Darwin kernel guard with no Linux equivalent. The
> Linux CI runners were always fine. `scripts/docker/python-toolchain.Dockerfile` removes the
> asymmetry by running the toolchain in a container, which is how
> `TestRunBuild_PythonTarget_WasmRoundtrip` was made to execute for the first time on a Mac.
> Note that the engine's Python tests need **`wasm-tools` as well**, checked separately, so
> installing `componentize-py` alone leaves them skipping with a different message.

---

### 2.73 Plugin-harness CI ran on wazero too, and a skip was hiding the cost — ✅ **FIXED** (WS-3, 2026-08-04)

`plugin-harness-ci.yml` set `CGO_ENABLED: "0"` in **all four** jobs. Same defect §2.70 fixed in
`multi-db-ci.yml`: `NewWasmtimeBackend` is behind `//go:build cgo`, so this did not skip a
check, it removed the primary backend and ran everything on wazero.

**Here it had a visible cost.** `TestPluginCalls_Wasm_Go` skipped unconditionally on
*"wazero v1.11.1 nil Sys context panic"* — and that panic only happens because the job forced
wazero. Measured both ways before changing anything:

| | |
|---|---|
| CGO on | **PASS** |
| CGO off | `invalid memory address or nil pointer dereference (recovered by wazero)` |

So the primary language had no WASM integration coverage, guarded by a skip describing a bug
in a runtime the product does not use for Go. CGO is now pinned to `"1"` in every job and the
skip is gone; `skip-baseline` drops that entry 2 sites → 1.

**`cleat build --target rust` is now exercised.** It was exercised nowhere:
`TestPluginCalls_Wasm_Rust` is the only test that runs it, and this job installed no Rust, so
it skipped. Worse, its guard checked for **`wasm32-wasip1`** while `build_rust.go:34` compiles
for `wasm32-unknown-unknown` — so the check was for a target the build does not use. A machine
with wasip1 and not unknown-unknown passed the guard and then failed inside cargo; one with
unknown-unknown and not wasip1 skipped a build that would have worked. Guard corrected, and
the job now installs the right target.

**Not fixed, and the skip stays: Layer 3.** `TestPluginCalls_MultiDB` carries the same
wazero-panic skip, but removing it does *not* pass with CGO on — postgres succeeds and the
other two dialects fail on migration handling that has nothing to do with wazero:

```
mysql: RunCoreMigrations: execute 003_procedures.sql: Error 1064 ... near 'DELIMITER //
mssql: RunCoreMigrations: execute 001_schema.sql: Could not create constraint or index
```

The MySQL one is the `DELIMITER` idiom that `splitMySQLDelimited` handles in
`engine/store_backends_procedures_test.go` and this runner does not; the MSSQL one has the
shape of the `GO` batch-separator problem. So that skip was concealing two real defects behind
a third, and `plugin-harness/multi-db` keeps its budget of 1 until they are fixed.

---

**Method note for Phase 3.** Every "already on develop" verdict above was settled by
diffing against `develop` and by `git apply --check`, not by reading commit messages — the
mistake §0.2's correction calls out. Three of the four highest-value findings (§2.18, §2.19,
§2.20) are defects on `develop` that the PR happened to touch, not features the PR added.
Assessing a stale branch for salvage turned out to be a decent defect-finding technique in
its own right, because it forces a line-by-line read of code nobody has looked at recently.

---


## Phase 3 items — round 2 (2026-08-05)

### 3.10 Idempotency keys are global across tenants — ✅ **FIXED** (WS-1, 2026-08-05)

Found while auditing the ~89 unaudited `MySQLStore` `s.tenantID` call sites (§1.7 / §2.12).
The audit's premise is that MySQL has no RLS, so a missing Go-level tenant filter is an
unbacked cross-tenant leak. This one is worse than a missing filter: **there is nothing to
filter on.**

`idempotency_keys` is keyed by `key_hash` alone on every dialect —
`key_hash BYTEA NOT NULL PRIMARY KEY` (postgres), `PRIMARY KEY (key_hash)` (mysql),
`CONSTRAINT pk_idempotency_keys PRIMARY KEY (key_hash)` (mssql) — and the table has **no
`tenant_id` column at all**. The hash is `sha256.Sum256([]byte(idempotencyKey))`, with the
tenant nowhere in it (`store_lifecycle.go:634`, `mssql_lifecycle.go:741`, and the MySQL
equivalent).

So an `Idempotency-Key` is global. Measured on postgres, mysql and mssql:

```
tenant B's first use of its own idempotency key "order-…" reported already-existing:
  tenant A's key collided with it. B's workflow was never started
tenant B was handed tenant A's workflow ID "idem-a-…" for its own idempotency key
  -- a cross-tenant information leak on a user-supplied value
tenant B has no workflow after StartNewRun returned "idem-a-…"
```

**Two impacts.** Tenant B receives tenant A's workflow ID — a cross-tenant information leak
on a value the client supplies. And tenant B's workflow is **silently never started**, while
the API answers `200 {"already_started": "true"}`. `Idempotency-Key` is a request header, so
this is the expected outcome of two customers both choosing `order-123`, not an attack.

Note that PostgreSQL's RLS cannot help here either: there is no tenant column to filter on.
This is the one tenancy defect in the set that all three backends share equally.

**The fix, and the decision in it.** Two options, differing only on upgrade:

- **Add `tenant_id` and make the primary key `(key_hash, tenant_id)`.** Existing rows take
  the default tenant, so single-tenant deployments keep deduplicating across the upgrade.
  Migration in WS-1's range, three dialects. **Recommended.**
- **Put the tenant into the hash.** One line per dialect, no migration — but every existing
  key stops matching, so a retried request after the upgrade starts a *second* workflow.
  That is precisely what idempotency exists to prevent, so the cheaper fix is the wrong one.

A failing three-dialect test is on `bugfix/mysql-tenant-scoping-audit`
(`engine/idempotency_tenant_test.go`), committed without a PR because it is red by design.

#### Resolution — the column, and what the existing test could not see

Fixed as recommended: `migrations/{postgres,mysql,mssql}/010_idempotency_keys_tenant_id.sql`
adds `tenant_id` defaulting to `DefaultTenantUUID` and widens the primary key to
`(key_hash, tenant_id)`; the three `StartNewRun` idempotency paths carry
`AND tenant_id = ?` on both lookups and the tenant on the insert. The hash is unchanged, so
keys written before the upgrade still match.

**The failing test was failing for the wrong reason on PostgreSQL.** As committed on
`bugfix/mysql-tenant-scoping-audit` it called `setupTestData`, which inserts under
`DefaultTenantUUID`, against a store scoped to tenant A — and `PostgresBackend.SetupForTenant`
returns a genuinely RLS-enforcing connection, which rejected the write:

```
setupTestData: StartNewRun ready: start new run: pq: new row violates
row-level security policy for table "workflow_instances" (42501)
```

So the postgres arm died in setup, one layer above the property, and the "measured on
postgres, mysql and mssql" line above was true of the *defect* but not of that test: only its
MySQL and SQL Server arms had ever reached the assertion. Dropping the two fixture calls (the
test deploys its own definitions) makes all three arms reproduce it. Two further corrections
the fix surfaced:

- The test asserted on `WorkflowInstance.TenantID`. Only `MySQLStore` selects `tenant_id`
  into that field; `PostgresStore` leaves it to RLS and `MSSQLStore` filters in SQL, so on
  those two it is `""` for every workflow and the assertion passed or failed for reasons
  unrelated to tenancy. It now identifies B's workflow by its input payload and adds the
  assertion that actually matters — neither store can read the other tenant's workflow at all.
- Both tenants deployed a definition of the same name, which collides on a primary key that
  has no tenant in it. Recorded separately as §3.12.

**Proof it can fail** (removing the fix and re-running, on all three dialects at once):
with the `AND tenant_id = ?` deleted from the three lookups, every arm reports
`tenant B's first use of its own idempotency key reported already-existing` and
`tenant B was handed tenant A's workflow ID`. With the migration dropped instead,
`migration.TestIdempotencyTenantMigrationPreservesExistingKeys` reports
`column "tenant_id" does not exist (42703)` / `Invalid column name 'tenant_id'`.

**The upgrade path is tested, not asserted.** `migration/idempotency_tenant_test.go` drives the
real `Runner` over the real files — 001 alone, then 001 + 010 — against a scratch database
with a pre-upgrade key already in it, and checks that the key survives, lands on the default
tenant, no longer blocks a second tenant, and still deduplicates within one. That is the
whole difference between the two candidate fixes, so it is the thing worth a test.
It runs on PostgreSQL and SQL Server; **its MySQL arm is skipped**, because the Runner cannot
parse `migrations/mysql/001_schema.sql` at all — §3.13, found by this test.

**A skipped 010 is loud, not silent.** Migration versions 6–15 were used by the numbering
`eb6b082` folded into 001, so a develop-tracking database migrated before that commit may
already have version 10 recorded and would skip this file. `StartNewRun` then names a column
that does not exist and every idempotent start errors, rather than quietly mis-scoping.

**Residual: PostgreSQL has no RLS policy on this table.** The seven policies in 001 cover the
tenant-scoped tables; `idempotency_keys` was not among them because it had no tenant column to
filter on. It has one now, so a policy is possible — but every access path would have to set
`cleat.tenant_id` first, and three do not: the pre-transaction lookup, the insert (which runs
before `setRLSOnTx`), and `cmd/cleat-worker/setup.go`'s expiry sweep, which has no tenant
context at all. Fail-closed policies would turn all three into errors. That is a separate
change to the transaction structure, not a line in a migration, and it is left open here
rather than half-done.

### 3.11 Four unscoped queries — ✅ **FIXED** (WS-1, 2026-08-05), and it was three dialects, not one

From the same audit: 109 statements in the MySQL store touch tenant-scoped tables, 16 carry
no `tenant_id` reference, and most of those 16 are false positives — `ClaimWorkflows`'
`UPDATE ... WHERE id IN (...)` is scoped transitively by its candidate `SELECT`. **Read the
enclosing function before believing the grep.**

The four that survive that reading:

| method | why it matters |
|---|---|
| `GetWASMLength` | `WHERE name = ? AND version = ?` — def names are user-chosen and collide across tenants, so this returns another tenant's WASM size |
| `QueueDepth` | counts `workflow_instances` across every tenant |
| `DeleteExpiredEvents` | **deletes** `event_history` across every tenant |
| `GetAllowedSignalCallers` | reads authorization data by workflow ID with no tenant scope |

None has a database backstop on MySQL (§1.7: zero RLS policies). Severity is bounded by the
HTTP layer's per-tenant scoping since §1.7, which is defence in depth working — but that is
the only thing standing between these and a leak.

#### Resolution — measured per dialect, because the answer differs per method

The audit read the MySQL store, so the item was written as a MySQL item. All four statements
are unscoped in the PostgreSQL and SQL Server stores too; what differs is what sits
underneath, and that turned out to vary by *method* rather than by dialect. Measured with two
tenants and real rows rather than reasoned about:

| method | postgres | mysql | mssql |
|---|---|---|---|
| `QueueDepth` | scoped — runs in `beginTxWithRLS` | **counted 5 of 5** | **counted 5 of 5** |
| `GetWASMLength` | **errored for everyone** (below) | **read the other tenant's 8 bytes** | **read the other tenant's 8 bytes** |
| `GetAllowedSignalCallers` | scoped by RLS | **read the other tenant's list** | **read the other tenant's list** |
| `DeleteExpiredEvents` | scoped — runs in `beginTxWithRLS` | **deleted the other tenant's history** | **deleted the other tenant's history** |

All four now carry an explicit `tenant_id` predicate on MySQL and SQL Server. On PostgreSQL
the two that already ran inside an RLS transaction are left alone — the policy is the filter
there, and a redundant predicate would only obscure that — and the two that did not now do.

**`GetWASMLength` on PostgreSQL was not a leak; it was a silent cache bug.** It ran on `s.db`
with no `cleat.tenant_id` set, so on the role the engine is meant to run as
(`005_app_role.sql`, §1.10) the policy could not be evaluated and every call failed with
`invalid input syntax for type uuid: "" (22P02)`. Its one caller is `Worker.loadWASM`, which
uses the length as a cache-freshness check on **every cache hit** and treats an error as
"keep serving the cache". So on PostgreSQL a redeployed definition was never picked up by a
worker with a warm cache: the staleness check could not fire, and the error path even records
a cache *hit* metric. Running it in an RLS transaction restores it.

**`GetWASMLength`'s tenant scope depends on §3.12.** Until a definition records the tenant
that deployed it, every definition is the default tenant's and the predicate matches nothing
useful — the two arms of this test that cover it only pass with §3.12's writer fix in place.
Two defects that had to be fixed in the same direction to make either observable.

### 3.18 SQL Server rejects the JSON the other two dialects require — ✅ **FIXED** (WS-1, 2026-08-05), floor raised to 2022

The third thing the §2.71 measurement found, and the first that cannot be fixed without
choosing something. It blocks the schema switch: with the shipped MSSQL schema in place, five
subtests still fail, all of them this.

`ISJSON(expression)` with no second argument returns 1 **only for a JSON object or array**. A
JSON scalar — `"payload-1"`, `123`, `true` — returns 0. Measured directly:

```
ISJSON('"payload-1"') = 0
ISJSON('{}')          = 1
```

§2.60c made all three stores encode a non-JSON signal payload with `json.Marshal`, which turns
`payload-1` into the scalar `"payload-1"`. PostgreSQL's `JSONB` and MySQL's `JSON` both accept
a scalar, so that fix is correct there — but the value it produces is exactly what SQL Server's
shipped `CHECK (ISJSON(payload) = 1)` refuses. So `DeliverSignal` and `CreateUpdateRequest`
fail on a SQL Server built from `migrations/mssql/001_schema.sql`, and §2.60c is only
two-thirds fixed. The test schema's missing CHECK constraint is why it read as complete.

**Why this is a decision and not a patch.** `ISJSON(payload, VALUE) = 1` accepts scalars and
is the obvious repair — but the second argument requires **SQL Server 2022**, and `README.md`
and `docs/reference/database-backends.md` both promise **2017+**. Fixing it that way silently
raises the floor. The options:

| | what it costs |
|---|---|
| `ISJSON(payload, VALUE)` in a migration | Requires SQL Server 2022; contradicts the documented 2017+ support unless that claim changes too |
| Drop the CHECK on those two columns | Keeps 2017; SQL Server loses a backstop the other two get from their column types. The engine already guarantees valid JSON through `encodeSignalPayload`, so the constraint is defence in depth rather than the only guard |
| Encode scalars as an object or array | Keeps both the constraint and 2017 — but changes the stored format on every dialect, needs a migration for existing rows, and changes `decodeSignalPayload` |

Not chosen here. Any of them is a product call about what cleat supports, and the first and
third change behaviour beyond the defect.

#### Resolution — 2022+, and now it is the claim CI tests

The owner chose the first option. `migrations/mssql/011_json_scalar_payloads.sql` changes both
constraints to `ISJSON(payload, VALUE) = 1`, which accepts every value PostgreSQL's JSONB and
MySQL's JSON accept — measured on the 2022 container: a string scalar and a number scalar pass,
an object and an array pass, and `payload-1` is still refused, so the guard is not traded for a
no-op. `README.md` and `docs/reference/database-backends.md` say 2022+.

Worth stating plainly, because it was the argument for choosing this way: the 2017+ claim was
**true in the code and tested nowhere**. The shipped SQL used only 2016/2017-era features, so
it was not already broken — but `multi-db-ci.yml`, the compose files and the docs' own examples
have only ever run 2022, and nothing anywhere asserts a version. The repo now promises what it
verifies. A server older than 2022 fails migration 011 with `Incorrect syntax near 'VALUE'`,
which is a better answer than silently rejecting every signal.

### 3.19 `CreateUpdateRequest` was §2.60c's defect, one table over — ✅ **FIXED** (WS-1, 2026-08-05)

§2.60c established that `workflow_signals.payload` must hold valid JSON on all three dialects
and that only `PostgresStore` knew; it extracted `encodeSignalPayload` and applied it
everywhere. It did not reach `workflow_update_requests.payload`, the sibling column with the
same requirement, where each store was wrong in a different way:

| store | what it did with a non-JSON payload |
|---|---|
| `PostgresStore` | wrapped with `` `"` + payload + `"` `` — the concatenation §2.60c itself identifies as producing invalid JSON when the payload contains a quote or a backslash |
| `MySQLStore` | nothing; `Error 3140` |
| `MSSQLStore` | nothing; CHECK constraint violation |

So `CreateUpdateRequest(ctx, wf, "name", "payload-1", …)` succeeded on PostgreSQL and failed on
the other two, and a payload containing a quote failed on all three. Found by pointing
`engine/testutil`'s MSSQL schema at the shipped migration (§2.71) — the only place the
constraint exists.

**And the readers disagreed too**, which the test caught rather than accommodating.
`PostgresStore` unwraps with `payload #>> '{}'`; the other two returned the quoted form. The
same call therefore answered differently per backend. `encodeSignalPayload`/`decodeSignalPayload`
are now `encodeJSONPayload`/`decodeJSONPayload` and both halves are applied on all three, so
`GetPendingUpdateRequests` returns what the caller passed in everywhere.



**The §2.71 switch is written and waiting on this.** `engine/testutil`'s MSSQL schema pointing
at the shipped migration works — the fingerprint-once shape from §3.16's note, the
hand-written DDL retained unreachable for the review diff — and takes the suite from 95 failing
subtests to 5. Those five are this item. The branch is `fix/mssql-test-schema-real`, unpushed;
it is ~60 lines and has been re-derived twice, so re-deriving it is cheap if it is lost. Two
smaller things also fall out of the switch when it lands:

- one fixture inserts a `tenant_api_keys` row for a tenant absent from `admin.tenants`, which
  the shipped schema has a foreign key for;
- `TestMSSQLTenantIsolation_UnderRealSecurityPolicies` asserts that exactly
  `[workflow_routing workflow_tags]` are missing from the test schema. Under the real schema
  nothing is missing, so it fails **because the drift is closed** — the assertion becomes "no
  tables are missing".

### 3.17 Completing a workflow wrote JSON `null` into `query_state`, and SQL Server refused it — ✅ **FIXED** (WS-1, 2026-08-05)

The second thing the §2.71 measurement found, after §3.16 removed the first. Twelve sites
across all three stores read:

```go
qsJSON, _ := json.Marshal(queryState)
if qsJSON == nil {
    qsJSON = []byte("{}")
}
```

`json.Marshal` of a nil map returns the four bytes `null`, **not nil**, so the guard never
fired and `null` is what reached the database — for every workflow with no query handlers,
which is most of them.

PostgreSQL's `JSONB` and MySQL's `JSON` accept it, because a JSON null is valid JSON: the row
goes in and the query state reads back as `null` instead of `{}`. SQL Server's shipped schema
does not — `CHECK (ISJSON(query_state) = 1)` and `ISJSON('null')` is `0` — so **`CompleteWorkflow`,
`FailWorkflow` and `ContinueAsNew` all failed outright** on a SQL Server built from
`migrations/mssql/001_schema.sql`:

```
The UPDATE statement conflicted with the CHECK constraint
"ck_workflow_instances_query_state"
```

13 of the 29 subtests still failing after §3.16 were this. Fixed with one helper,
`marshalQueryState`, at all twelve sites.

**The test is three-dialect on purpose.** Asserting only that SQL Server stops erroring would
leave PostgreSQL and MySQL writing `null` forever, so
`TestCompleteWorkflowStoresAnObjectForEmptyQueryState` reads the column verbatim on each
dialect — the store's `GetQueryState` takes a key and returns one entry, which cannot tell an
empty object from a JSON null. With the fix reverted: postgres and mysql report
`query state stored as null (raw "null"), want {}`, and mssql reports the constraint
violation. The `empty map` and `a handler` cases pass either way, which is the shape of the
defect: only the nil case was wrong, and only the nil case is common.

`ck_workflow_instances_query_state` is now in the test schema too, with a repair step for the
`null`s an existing test database already holds.

**Still open from the same measurement** (counts from the 29 remaining after §3.16):

- 6 × `ck_workflow_signals_payload` and 2 × `ck_workflow_update_requests_payload` — the same
  family, different columns. §2.60c covers signal payloads and was fixed on 2026-08-04, so
  read that before assuming this is the same thing.
- 1 × `fk_api_keys_tenant` — a fixture inserting an API key for a tenant that does not exist
  in `admin.tenants`, which the shipped schema has a foreign key for and the test schema does
  not.
- `TestMSSQLTenantIsolation_UnderRealSecurityPolicies` asserts that exactly
  `[workflow_routing workflow_tags]` are absent from the test schema. Under the real schema
  nothing is absent, so that assertion fails **because the drift is closed** — it needs to
  become "no tables are missing" as part of the switch.

### 3.16 `CreateSchedule` could not create a schedule on SQL Server — ✅ **FIXED** (WS-1, 2026-08-05)

Found by measuring the §2.71 residual rather than by reading code: pointing `engine/testutil`'s
MSSQL schema at the shipped `migrations/mssql/001_schema.sql` and running the engine suite
produced 95 failing subtests, and the single largest cause was not row-level security at all.

`json.RawMessage` is a `[]byte`, and go-mssqldb binds a `[]byte` as `VARBINARY`. So
`workflow_schedules.input` received the *binary rendering* of the JSON rather than the JSON,
and the shipped schema refuses it:

```
setupTestData: CreateSchedule: mssql: The INSERT statement conflicted with the
CHECK constraint "ck_workflow_schedules_input" ... column 'input'
```

`CONSTRAINT ck_workflow_schedules_input CHECK (ISJSON(input) = 1)` has been in
`001_schema.sql` all along, so **every scheduled workflow on a SQL Server built from the
shipped schema failed to be created.** `StartNewRun` had the same shape and was written
correctly — `CAST(@p4 AS NVARCHAR(MAX))` with `string(input)` — which is what the fix copies.

**Why nothing caught it:** `engine/testutil`'s hand-written MSSQL schema declares no CHECK
constraint on that column. The malformed value went in, the suite stayed green, and the defect
was visible only on a database built from the file that ships. That is the §2.71 schema
residual expressed as one concrete production failure, which is the argument for closing it.

The constraint is now in the test schema too, with a repair step for rows an existing test
database already holds (`ALTER TABLE ADD CONSTRAINT` validates existing rows and would fail on
them). `TestMSSQLCreateSchedule_SurvivesTheShippedInputConstraint` applies the shipped
constraint to the one table it is about rather than relying on the shared schema, and fails on
all three input shapes with the fix reverted.

#### What the §2.71 measurement says about the remaining work

Worth recording so the next session does not repeat it. Pointing the MSSQL test schema at the
real migration is a **suite migration**, as the residual says, but the failures are not what
the residual predicts:

- **95 failing subtests** from an empty database, before this fix.
- The dominant cause was §3.16 above, not a missing session context.
- A second cause is in the harness rather than the tests: `001_schema.sql` is **not
  re-appliable**, though its header claims to be. The seven security policies bind
  `dbo.fn_tenant_filter`, so the file's own `CREATE OR ALTER FUNCTION` fails the second time
  with `Cannot ALTER 'dbo.fn_tenant_filter' because it is being referenced by object
  'TenantFilter_Defs'`. The migration Runner never sees this because it applies each file once
  and records the version; a test helper that runs on every `Setup` call sees it immediately.
  Whoever does the switch needs the fingerprint-once shape `applyPostgresSchemaFile` already
  uses.

Re-measure after §3.16 lands: the number that matters is what is left once the schedule defect
is gone.

### 3.15 Signal authorization consults a list nothing can write — 🟢 **THE WRITER EXISTS** (WS-1, 2026-09-02); the default stays off, for a different reason

Found while scoping `GetAllowedSignalCallers` for §3.11: the method reads
`workflow_instances.allowed_signals`, and **nothing in the product ever writes that column.**
Searched the whole tree, every language, excluding tests: the only writes are two raw
`UPDATE`s inside test files. There is no store method, no API endpoint, no CLI verb, no SDK
call.

What consumes it is not optional. `cmd/cleat-worker/setup.go` installs the check whenever
`--require-signal-auth` is set, and that flag **defaults to `true`**:

```go
callers, err := w.store.GetAllowedSignalCallers(ctx, targetWorkflowID)
if len(callers) == 0 {
    return fmt.Errorf("signal auth denied: workflow %s has no allowed callers configured", ...)
}
```

`docs/reference/worker-config.md` documents the empty list as "deny all (fail-secure)" and
tells operators to "add `"*"` (wildcard) to `allowed_signals`" to permit external callers —
an instruction there is no way to follow.

So on a default deployment every cross-workflow signal, every plugin-originated signal and
every external HTTP signal is denied, and the documented way to allow one does not exist. The
only coverage is `TestWithSignalAuthCheck`, which passes a stub closure and asserts the option
plumbing — the §1.3 shape exactly: the test cannot see the defect because it replaces the
thing that has it.

**The fix is a decision, not a patch.** Either signal authorization gets a way to populate the
list (store method, API, and something in the SDKs), or `--require-signal-auth` defaults to
`false` until it does. Flipping the default is one line and turns a silently-broken security
feature into an absent one; adding a writer is the feature it was always supposed to be. Not
taken here because it is a product call rather than a defect fix.

#### Resolution — the default is off, and the denial is now observed rather than read

Taken the second way, on the owner's instruction to use judgement. `--require-signal-auth`
defaults to `false`; `docs/reference/worker-config.md` says plainly that the flag is not usable
yet and why; `CHANGELOG.md` carries it as a breaking upgrade note. **The feature is still
absent** — this makes that honest rather than making it work.

Before changing a security default I verified the denial rather than trusting the code path,
which meant giving the production wiring a name: the check was an anonymous closure inside
`newWorker`, so the only thing testable was `engine.TestWithSignalAuthCheck`, which passes a
*stub* closure and asserts the option plumbing — a test that replaces the thing under test.
`signalAuthCheckFor(store)` is now a named function, and three tests drive it against a real
PostgreSQL store:

- a workflow created through the ordinary path denies every caller, with the empty-list reason.
  That is the defect, pinned: as long as nothing can write the column, this is what enabling
  the flag does;
- the flag's default is `false`;
- the mechanism still enforces a list that *is* present — caller listed, caller absent,
  wildcard, empty list — set with raw SQL, because that remains the only way to set it. That
  guards against the check rotting while it is unreachable, so whoever adds a writer inherits
  something that works.

**Still open:** the writer. A store method, an API endpoint and SDK surface, at which point the
default goes back to `true`. The tests above are written so that the first one fails when that
lands, which is the signal to revisit them.

#### Resolution — the writer, 2026-09-02

`WorkflowStore.SetAllowedSignalCallers(ctx, workflowID, callers)` on all three dialects and
`ShardedStore`, plus `GET`/`PUT /api/workflows/:id/allowed-signals`. The documented instruction
— *add `"*"` (wildcard) to `allowed_signals`* — is now an operation an operator can perform.

**Replaces rather than merges**, and an empty list writes SQL NULL rather than `"[]"`. The
second one is the part worth recording: the getter normalises NULL, `""` and `"null"` to a nil
slice, so a Set/Get round trip cannot tell those apart, and a test written only through the
store would pass against a setter writing anything the getter forgives.
`TestSetAllowedSignalCallersEmptyWritesNull` reads the column directly for that reason.

**Five things the falsification pass established**, none of which was visible from reading the
code:

- **The tenant predicate is load-bearing on MySQL only.** Removing `AND tenant_id = ?` from the
  MySQL setter turns the cross-tenant test red *on mysql alone* — PostgreSQL's RLS and SQL
  Server's security policies cover for the same omission. That is §3.11's finding restated for
  a writer, where the consequence is worse: a missed predicate on a getter leaks a list, on a
  setter it grants a caller access to another tenant's workflow.
- **The not-found check is what makes a cross-tenant write honest, not merely harmless.** Under
  RLS the other tenant's row is invisible, so the `UPDATE` is not refused — it matches nothing
  and *succeeds*. Removing the `RowsAffected() == 0` check turns the PostgreSQL cross-tenant
  case red with "tenant B ... was told it succeeded". Without it the API would answer 200 to a
  grant that never happened.
- **`ErrWorkflowNotFound` deliberately does not distinguish "absent" from "another tenant's".**
  Splitting them makes the endpoint an existence oracle. The getter already had this property
  by construction; the writer had to be given it.
- **MySQL's `RowsAffected` reports 0 for "matched but unchanged"**, so the naive
  `n == 0 → not found` is wrong there: re-setting a list to the value it already holds reports
  a missing workflow. The MySQL implementation confirms absence with a follow-up `SELECT`.
  `TestSetAllowedSignalCallersIsIdempotent` covers it, and the other two dialects would not
  have caught it — they report 1.
- **One falsification did not go red, and the comment was corrected rather than the code.**
  Removing `setSessionContext` from the MSSQL setter leaves every test green, because within a
  single test nothing returns the connection to the pool and the connector's per-connection
  setting is still in force — which is precisely the moment §2.71 is about. The call stays,
  matching every other MSSQL write path; what changed is the comment, which had claimed a
  requirement the suite does not demonstrate.

**The API is tenant-scoped through `scopedStore`, asserted against a per-tenant factory** rather
than through `newTestAPIServer`'s single shared mock — §3.20's trap was a handler that checked
ownership on the scoped store and then operated on the process-wide one, and a grant endpoint is
where that would cost the most.

**Still open, and it is why the default stays `false`.** Nothing sets `allowed_signals` when a
workflow *starts*. Every workflow begins with an empty list, so enabling `--require-signal-auth`
today denies every signal until an operator makes a second call per workflow — usable, but not a
safe default. Start-time declaration (a field on the start request, and SDK surface for it) is
the follow-up, and flipping the default is a product call that should wait for it. The three
tests in `cmd/cleat-worker/signal_auth_test.go` have been rewritten accordingly: the one that
pinned the defect now asserts deny → grant → revoke through the check the worker installs, and
the enforcement table sets its list through the supported path while still reading the column
back.

### 3.78 Cross-schema child workflows, removed — ✅ **DONE** (WS-1, 2026-09-02, D8)

`cleat_child_workflow_in_schema` let a guest start a child workflow by writing a row directly
into another PostgreSQL schema, gated by an operator allowlist (`--peer-schemas`). Removed
entirely: host call, store methods, component interface, worker flag, and the surface in all
four guest SDKs.

The owner's call, and the reason is worth keeping in these words: *"it's adding complexity and
there's a more familiar way to manage microservices, so we shouldn't waste time on it."*

#### What made it not worth fixing

It came up while scoping D7 (§3.77), because per-tenant names break the feature's central
assumption. Three findings, each measured rather than read:

- **The definition lookup in the peer schema has no tenant predicate.** `SELECT MAX(version)
  FROM <peer>.workflow_defs WHERE name = $1 AND NOT deprecated`. That is unambiguous only
  while `(name, version)` is unique per schema — exactly what D7 removes. Afterwards a peer
  schema can hold two tenants' `order-processor` and the lookup picks by version number, not
  by owner, so a child could run another tenant's code.
- **It ran with no tenant context at all when the target tenant was not recoverable from the
  schema name.** `tenantIDForSchema` only mapped `tenant_<uuid>`; for an operator-chosen name
  like `svc_billing` the insert went through `s.db` with no transaction and no
  `cleat.tenant_id`, and the destination's column default applied. The function's own comment
  admitted the engine "genuinely does not know which tenant owns the destination" — which is
  an undefined security model for the feature's headline use case, not an edge.
- **It was the only place in the engine that deliberately wrote a row on behalf of another
  tenant.**

#### The design question it could not answer

A peer schema meant two different things in the same function. `tenantIDForSchema` followed
`admin.create_tenant_role`'s `tenant_<uuid>` convention — **schema per tenant** — while the
doc comment eleven lines above said "the target schema is a different microservice" — **schema
per service**. Those need different answers to everything downstream:

- *another tenant* → a cross-schema start is one tenant reaching into another's namespace,
  which needs an authorization model saying who may start what on whose behalf. There was
  none; there was a flag listing schema names.
- *another deployment* → that deployment has its own tenants and the caller must name one. The
  guest passed `(targetSchema, name)` and nothing else, and after D7 `name` no longer
  identifies a definition.

**The authorization asymmetry is the tell.** §3.15 established that one workflow *signalling*
another needs per-workflow authorization (`allowed_signals`). Starting a child **in another
schema** — which executes code rather than delivering a message — needed only an operator flag,
with no per-definition or per-caller granularity. The weaker control was on the more powerful
operation.

#### Why removal rather than a fix

Underneath both readings sits a question neither answers: writing into another deployment's
tables makes that deployment's schema part of your API, and its migrations part of your
compatibility surface. Going through its API instead is the familiar answer, already exists,
and is tenant-scoped end to end.

Removal was also cheap to get right, because there are no deployed guests importing the host
call. That is the only reason the ABI slot could go rather than being reserved.

#### What it cost, measured

| | |
|---|---|
| tests exercising it | 1 (`TestStartChildWorkflowInSchemaAttributesToTargetTenant`), covering tenant *attribution* rather than the cooperation story |
| end-to-end coverage | none — no two-schema, two-worker-pool test, no example, and neither the cluster nor compose deployments configured `--peer-schemas` |
| `tiers.yaml` claim | none, at any tier |

A guest-reachable capability that wrote across a tenant boundary, with no support claim, no
end-to-end test, and an unsettled security model. §2.23 fixed its tenant *attribution* in
2026-08; that entry stands as history and is superseded by this one.

#### Removal notes

- **ABI count 59 → 58** on both backends, re-derived with the commands in `ABI.md`'s own
  "Previously undocumented functions" note. No `CurrentABIVersion` bump: nothing that remains
  changed shape.
- **`ABI.md` §2.21 is left vacant** rather than renumbering §2.22–§2.59, because those numbers
  are cited from commit messages and plan entries; a gap reads more cheaply than a shift.
- The `cleat:host-calls/durable-extended-children` WIT interface held only this one function
  and went with it, along with its generated Python binding.
- `GetChildResultInSchema` went too — it existed only to read back what this wrote.

### 3.77 Names are per-tenant — D7, and it is three tables rather than one — ✅ **DONE 2026-09-03; all three tables**

The owner's decision, in their words: *"It doesn't make any sense for one tenant to need to
worry about clashes with some other tenant's workflows."* Recorded as **D7** in `tiers.yaml`.

That settles §3.12, which has sat as "the namespace is shared; taking a name is now loud
instead of silent" since 2026-08-05. It also turns out to be bigger than §3.12, and the extra
scope came from asking what the decision *means* rather than which table it was raised against.

#### The rule, and the two classes it separates

**A primary key over a user-chosen name must carry the tenant. A key over a generated id need
not, and should not be changed.** A UUID cannot be chosen, so it cannot be clashed with; adding
a tenant to those keys is churn that buys nothing and costs three dialects' worth of migration.

Derived rather than eyeballed — every table that has a `tenant_id` and a primary key that omits
it, from `migrations/postgres/001_schema.sql`:

| table | primary key | class |
|---|---|---|
| `workflow_defs` | `(name, version)` | **user-chosen — in scope** |
| `workflow_schedules` | `(name)` | **user-chosen — in scope** |
| `workflow_tags` | `(workflow_name, tag)` | **user-chosen — in scope** |
| `workflow_instances` | `(id)` | generated — leave |
| `workflow_routing` | `(id)` | generated — leave |
| `event_history` | `(workflow_id, step)` | generated — leave |
| `workflow_signals` | `(workflow_id, signal_name)` | generated prefix — leave |
| `workflow_promises` | `(workflow_id, promise_id)` | generated prefix — leave |
| `workflow_update_requests` | `(workflow_id, update_name)` | generated prefix — leave |

The last three are keyed on a workflow id first, so a name only has to be unique *within one
workflow*, which is one tenant's by construction.

#### Measured, not read — schedules collide on all three dialects

`workflow_schedules` was not on §3.12's radar at all. Two tenants each creating a schedule
called `nightly-report`, through the ordinary store API, on a per-tenant store apiece:

```
mssql: Violation of PRIMARY KEY constraint 'pk_workflow_schedules'.
       Cannot insert duplicate key in object 'dbo.workflow_schedules'.
       The duplicate key value is (nightly-report).
```

postgres and mysql fail the same way with their own wording. So this is not "theoretically
shared" — one tenant naming a schedule takes that name away from every other tenant on the
deployment, and the error they get names a constraint rather than the problem.

#### The migration hazard, and it is the part that needs deciding before any SQL is written

`workflow_defs` is referenced by **three foreign keys on every dialect**.

> **Corrected 2026-09-02, the same day this entry was written.** It said "three on PostgreSQL
> and three on MySQL — and **zero on SQL Server**, which the earlier 'three foreign keys per
> dialect' framing had wrong". §3.12's original framing was right and this correction was the
> error: SQL Server writes `REFERENCES dbo.workflow_defs`, and the grep below did not allow for
> the schema qualifier, so it matched nothing on that dialect. `migrations/mssql/001_schema.sql`
> has `fk_instances_def` at line 210 and two more at 408 and 424.
>
> Re-derive with a pattern that tolerates the qualifier, and note that the first form is the
> one that produced a confident wrong answer:
>
>     grep -rn 'REFERENCES workflow_defs' migrations/*/*.sql          # WRONG: misses mssql
>     grep -rEn 'REFERENCES [a-z]*\.?workflow_defs' migrations/*/*.sql # 3, 3, 3
>
> The lesson is the one this file keeps relearning in new clothes: a grep that returns zero for
> one member of a set it returns three for on the others is reporting a pattern problem, not a
> finding. Zero is the answer that most deserves a second look, and I wrote it into a section
> heading, a PR description and a commit message before checking.

They come from `workflow_instances(def_name, def_version)`, `workflow_tags(workflow_name,
version)` and `workflow_routing(workflow_name, target_version)`. All three referencing tables
already carry `tenant_id`, so widening them is mechanical.

**What is not mechanical is that `instance.tenant_id != def.tenant_id` is legal today.**
`canAdoptDef` (`engine/def_ownership.go:62`) admits `""`, the default tenant, or the deployer —
the adoption window §3.12 opened deliberately, so that the first redeploy after upgrading does
not break for every tenant at once. Until a definition is adopted, any tenant may start
workflows on it. Adding `tenant_id` to the foreign key therefore **breaks existing rows**: an
instance owned by tenant A pointing at a definition owned by the default tenant stops
satisfying the constraint.

Three ways out, and the choice belongs in review rather than in a commit:

1. **Fan out the definition per referencing tenant.** For each default-tenant definition, copy
   the row once per distinct tenant that references it, then repoint. Definitions are immutable
   once deployed, so the copies cannot diverge. Costs storage proportional to `wasm_bytes` ×
   tenants, and is the only option that leaves the constraint fully enforced.
2. **Adopt on the evidence.** Where every referencing instance shares one tenant, reassign the
   definition to that tenant. Cheap and correct for the common single-tenant upgrade, and it
   still needs option 1 or 3 for the genuinely-shared rows.
3. **Drop the foreign keys and enforce the relationship in Go.** ~~SQL Server already has no
   such constraint and is not obviously worse for it, which is an argument this option gets for
   free — the tree already runs one dialect this way.~~ **That argument was built on the wrong
   count and is withdrawn.** All three dialects enforce the constraint today, so this option is
   levelling *down* rather than codifying what one backend already does.

   What the foreign keys actually buy, measured: none of the six carries an `ON DELETE` clause,
   so they default to `NO ACTION` — they refuse to delete a definition that a workflow
   instance, tag or routing rule still references. That is real protection for an operation
   that really exists (`DELETE FROM workflow_defs` on all three dialects,
   `store_deployment.go:376`, `mysql_ops.go:847`, `mssql_deployment.go:419`). Dropping them
   would let an operator delete the code a running workflow is mid-replay of.

**All three options exist only to preserve existing rows**, and whether there are any is the
question that decides between them. If a deployment has no `workflow_defs` worth keeping, there
is a fourth option that dominates all of them:

4. **Change the key outright.** Drop the foreign keys, alter the primary key to
   `(tenant_id, name, version)`, recreate the foreign keys with `tenant_id` in them. Nothing to
   backfill, nothing duplicated, constraint fully enforced at the end. It also lets
   `canAdoptDef` and the whole default-tenant adoption window **be deleted** rather than carried
   forward — that machinery exists solely to smooth an upgrade, so with no upgrade to smooth it
   is dead weight. This option removes code instead of adding a migration.

**DECIDED 2026-09-02: option 4.** There is no deployed `workflow_defs` data to preserve, so
the migration changes the key outright and the adoption machinery goes with it. Options 1 and 2
are recorded above because the reasoning is worth keeping if that ever stops being true; option
3 is the weakest of the four rather than the free win it looked like.

**What option 4 deletes, and this is the part that makes it more than a migration.**
`canAdoptDef`, the default-tenant adoption window, and the ownership-error path that polices it
(`engine/def_ownership.go`, `ErrWorkflowDefOwnedByAnotherTenant`, and the repeated SQL guard on
PostgreSQL that maps `23505` onto it) all exist for one reason: `(name, version)` is a shared
namespace, so two tenants deploying the same name is a collision that has to be adjudicated.
Under `(tenant_id, name, version)` it is not a collision at all — each tenant gets its own row —
so there is nothing to adjudicate and the machinery has no job. Deleting it is not a
simplification bolted onto the migration; it is what the migration *means*.

**Sequenced as two PRs, because the first stands alone and de-risks the second:**

1. **Tenant-scope every `workflow_defs` lookup.** No schema change. Correct today — the local
   child-start path resolves a definition by name with no deliberate tenant check and is saved
   only by a `NOT NULL` constraint when `MAX(version)` comes back empty under RLS — and
   *required* the moment two tenants can hold one name.
2. **Change the key and delete the adoption window.** The migration, the PK, the three foreign
   keys per dialect, and the removal above.

Doing them in the other order leaves a window where the tree is incorrect rather than merely
under-defended.

#### Step 2 — `workflow_defs`, done 2026-09-02

Migrations `postgres/035`, `mysql/034`, `mssql/038`: drop the three foreign keys, swap the
primary key to `(tenant_id, name, version)`, re-add the foreign keys with `tenant_id` in them.
Guarded on current shape, following migration `010`'s precedent for `idempotency_keys`.

**The adoption machinery is gone**, which is what option 4 bought over the alternatives:
`engine/def_ownership.go` (`canAdoptDef`, `ErrWorkflowDefOwnedByAnotherTenant`,
`defOwnershipError`), the ownership lookup in all three deploy paths, PostgreSQL's repeated SQL
guard and its `23505` mapping, and the API's `409` arm. All three deploys are now plain upserts
on the tenant's own row, because there is no longer a collision to adjudicate.

**`TestDeployDoesNotOverwriteAnotherTenantsDefinition` became
`TestTwoTenantsEachHoldTheirOwnDefinitionOfOneName`.** Its old assertion — B's deploy is
*refused* — is not weaker now, it is wrong: B is not colliding with anything. The replacement
asserts what §3.12 said it deliberately could not, that both tenants deploy `order-processor`,
both succeed, and each reads back its own bytes.

**What the change actually required, beyond the schema.** The key swap made ~24 inline
`workflow_defs` subqueries ambiguous, in two classes that fail very differently:

| | how it fails |
|---|---|
| `(SELECT task_queue FROM workflow_defs WHERE name = ? AND version = ?)` in `StartNewRun` and friends — 6 sites | **Loudly.** `pq: more than one row returned by a subquery used as an expression (21000)` the moment two tenants hold one name. |
| `(SELECT MAX(version) ...)` resolving "latest" — 9 sites | **Silently.** An aggregate returns exactly one row either way. Unscoped it returns *another tenant's number*, so a child workflow starts on a version of its own definition that may not exist, or runs code its tenant never deployed. Nothing reports anything. |

The second is why `TestLatestVersionResolvesWithinTheCallersTenant` exists and why its fixture
is lopsided: tenant A holds v1–v3 of a name, tenant B holds only v1. An unscoped `MAX` picks 3;
B's own answer is 1. A symmetric fixture could not tell the two apart.

**Falsified, four ways, and two of them taught something:**

| removed | result |
|---|---|
| migration `035` (rebuild the schema without it) | 🔴 `TestTwoTenantsEachHoldTheirOwnDefinitionOfOneName/postgres` — the migration is what makes two tenants able to hold one name at all. |
| MySQL's tenant predicate on `ResolveLatestVersion` | 🔴 *"tenant B resolved `version-skew` to version 3, want its own 1 — 3 is A's number."* The silent class, caught. |
| PostgreSQL's tenant predicate on the `task_queue` subquery | 🔴 **only on tests using the admin/superuser connection**, with `pq: more than one row returned by a subquery (21000)`. |
| — the same, on RLS-enforcing paths | 🟢 **stays green.** PostgreSQL's policy covers the missing predicate there. |

The last two rows are the finding worth carrying: **the predicate is load-bearing exactly where
RLS is bypassed**, which is not only tests — it is the product's own admin and superuser paths.
A green result from an RLS-enforcing connection says nothing about those, and the first
falsification attempt here passed for precisely that reason before being re-run against the
right test.

Note that step 1's property test did **not** catch these, and could not have: it covers the
store-method *read* boundary, and these are inline subqueries inside `INSERT` statements. Two
different surfaces, and knowing which one a test covers is the difference between a green run
that means something and one that does not.

**A dialect divergence the migration exposed rather than created.** `001_schema.sql` declares
`workflow_defs.tenant_id` three ways — `NOT NULL DEFAULT` on PostgreSQL and SQL Server, plain
`CHAR(36)` (nullable, no default) on MySQL — and `002_defaults.sql` backfills it on the first
two with no MySQL counterpart. Latent until the column joined the primary key, which forces
`NOT NULL` on MySQL and made every insert omitting it fail with
`Error 1364: Field 'tenant_id' doesn't have a default value`. `mysql/034` backfills, then
constrains, in that order.

**Residual, named rather than relied on quietly.** Three name-to-version resolvers are *not*
tenant-scoped, because they are free functions on a bare `*sql.DB` with no tenant in hand:
`latestVersion` and `latestCompatibleVersion` (`engine/child_version.go`) and
`WorkflowLoader.ResolveLatestVersion` (`engine/versioned_loader.go`). Scoping them is a
signature change reaching their callers. In a deployment they run on the RLS-enforcing
connection and are covered — but that is a dependency on a layer rather than a predicate, which
is the shape this project keeps getting caught by, so it is written down here rather than
assumed.

#### Step 3 — `workflow_schedules`, done 2026-09-02

Migrations `postgres/036`, `mysql/035`, `mssql/039`: swap the primary key from `(name)` to
`(tenant_id, name)`. **No foreign keys to drop** — nothing references this table on any dialect
(`grep -rEn 'REFERENCES [a-z]*\.?workflow_schedules' migrations/*/*.sql` returns nothing, and the
schema qualifier in that pattern is the correction step 2 records having got wrong).

Guarded on current shape and **verified idempotent by applying each file a second time by hand**
after the runner had already applied it, since the runner records a migration as done and would
never re-run it. All three then report `(tenant_id, name)`.

**No Go changes were needed, and that is a result rather than an absence.** Checked rather than
assumed:

- the three stores' schedule statements already carry `tenant_id` — on SQL Server as of §3.86,
  which is why that landed first;
- the scheduler loop re-scopes `schStore` to `sch.TenantID` before its first store call, so the
  definition lookup, overlap check, run and CAS are all tenant-scoped;
- the cron idempotency key is already `cron:<tenant>:<name>:<instant>`, so two tenants firing
  the same schedule name at the same instant derive different keys;
- `admin.get_due_schedules()` already returns `tenant_id`, which is what the worker re-scopes on;
- no in-memory state anywhere is keyed on schedule name alone;
- guest-created schedules use `scheduleIDFor(tenantID, workflowID, stepCount)` and never
  collided. **The colliding case was always operator-named schedules** — `cleat schedule create
  nightly-report` — which is why §3.12's original survey did not see this table.

**Only the loud failure class exists here**, unlike step 2. `workflow_defs` had a silent one —
`MAX(version)` resolvers returning another tenant's number — because it is read by aggregate.
Every schedule read is a whole row or a listing, so a missing predicate surfaces as somebody
else's row rather than as a plausible wrong number. That is why this step's test file is short
and `def_lookup_tenant_property_test.go` is not.

**Falsified on all three dialects, each against a throwaway database built without the
migration** (not the shared test databases, which were already migrated — a falsification that
cannot rebuild the schema is measuring the database rather than the change):

| dialect | error |
|---|---|
| postgres | `duplicate key value violates unique constraint "workflow_schedules_pkey" (23505)` |
| mysql | `Error 1062 (23000): Duplicate entry 'nightly-report' for key 'workflow_schedules.PRIMARY'` |
| mssql | `Violation of PRIMARY KEY constraint 'pk_workflow_schedules' ... duplicate key value is (nightly-report)` |

Then restored and re-run **against those same databases**, so the guarded `ALTER` is exercised
on an already-built schema and not only on a fresh install.

**MySQL needed the identical `tenant_id` stanza `034` needed** — backfill, then `NOT NULL
DEFAULT` — because `001_schema.sql` declares the column nullable with no default there and
`002_defaults.sql` has no MySQL counterpart. Two tables in, that is no longer a quirk of one
migration: **the divergence belongs in `001_schema.sql`**, and `workflow_tags` will need the
stanza a third time if it is not fixed at the source first.

#### Step 4 — `workflow_tags`, done 2026-09-03. D7 complete.

Migrations `postgres/037`, `mysql/036`, `mssql/040`: primary key `(workflow_name, tag)` →
`(tenant_id, workflow_name, tag)`. No foreign keys to drop, and — unlike the other two —
`fk_workflow_tags_def` **already carried the tenant**, because `035`/`034`/`038` widened it when
they changed `workflow_defs`' key. One Go statement named the old key,
`PostgresStore.SetWorkflowTag`'s `ON CONFLICT (workflow_name, tag)`; MySQL's
`ON DUPLICATE KEY UPDATE` is implicit and follows the new key for free, and SQL Server's `MERGE`
already matched on the tenant as of §3.86.

**This closed a gap step 2 opened rather than one that predated it.** Before `035` two tenants
could not both hold a definition called `order-processor`, so both tagging it `stable` never
arose. After `035` they could hold the definition and still could not tag it — D7 was half true
for a day: names were per-tenant, the pointers into them were not.

**The three dialects answered that half-state three different ways, and the spread is the
finding:**

| dialect | what happened when tenant B tagged a name it legitimately held |
|---|---|
| postgres | `new row violates row-level security policy (USING expression) for table "workflow_tags" (42501)` — the `ON CONFLICT DO UPDATE` tried to update **A's** row and the policy refused the result. **Reads like a misconfiguration; was a key.** |
| mssql | `Violation of PRIMARY KEY constraint 'pk_workflow_tags' ... (order-processor, stable)`. Loud and honest. |
| mysql | **No error.** B's insert became an `UPDATE` of A's row, so A's `stable` moved to B's version — and B's own next read returned *nothing*, because the row still carries A's `tenant_id` while B's `SELECT` is scoped. |

The MySQL row is the one worth carrying: **the tenant that caused the damage is the one least
able to see it.** Bounded by D1 — MySQL is single-tenant-only for want of RLS — but the shape is
§3.12's defect on a different table, and the fix is the same: the statement is unchanged, the
key it matches on is not.

**Fixtures are lopsided on version throughout** (A holds v1–v2 tagged to v2, B holds v1 tagged
to v1) for the reason step 2's `MAX(version)` test records: both tenants tagging the same
version would pass against a key that ignored the tenant entirely.

**Falsified on all three dialects** against throwaway databases built without the migrations,
then restored and re-run against those same databases so the guarded `ALTER` is exercised on an
already-built schema; then each migration applied a second time by hand, since the runner
records one as done and would never re-run the guard. All three end at
`(tenant_id, workflow_name, tag)`.

**MySQL needed the `tenant_id` stanza a third time, with one difference worth reading.** On this
table `tenant_id` is part of a foreign key, and MySQL treats a NULL foreign-key column as
satisfying the constraint — so a row whose tenant is NULL today is *unchecked*, and the backfill
makes it checked. If the default tenant has no matching `workflow_defs` row the `UPDATE` fails
with `Error 1452` rather than silently repointing a tag. That is the right outcome and the
migration says so.

**Which leaves the divergence itself unfixed, and now measured.** `001_schema.sql` declares
`tenant_id` three ways and `002_defaults.sql` backfills it on two dialects; five MySQL tables
still carry it nullable with no default — `concurrency_keys`, `event_history`,
`workflow_routing`, `workflow_signals` and, until this migration, `workflow_tags`. It is **not**
a tidy-up to fold into a later step: fixing `001_schema.sql` helps no existing database
(`CREATE TABLE IF NOT EXISTS` never revises a column), so the mechanism would be one migration
constraining all of them — and making `event_history.tenant_id` NOT NULL is a real change to the
highest-volume table in the schema, on which nothing has checked whether a writer relies on NULL.
Decide it deliberately; it is no longer in D7's path.

#### Sequencing

One table per PR, `workflow_defs` first because it is the one with foreign keys and therefore
the one that settles the hazard above. `workflow_schedules` and `workflow_tags` are unreferenced
and should be cheap once the pattern exists.

**"Cheap" was right about the migration and wrong about the table.** Scoping
`workflow_schedules` turned up §3.86: five SQL Server schedule statements with no tenant
predicate, on a connection where the security policy is switched off, letting one tenant delete
and disable another's cron schedules over the HTTP API. That is a live defect on today's
schema, not a consequence of this change — but it is one this change would sharpen, because
under `PRIMARY KEY (name)` an unqualified statement at least matches a single row, and under
`(tenant_id, name)` a single `DELETE` would take every tenant's `nightly-report` at once. It is
fixed separately and first, for the same reason step 1 preceded step 2: doing it the other way
round leaves a window where the tree is incorrect rather than merely under-defended.

The general lesson for the two tables still to come: **check what the name-keyed statements
lean on before changing what the name means.** The predicate audit is the prerequisite, and it
is not visible from the migration. Migration numbers are **not** reserved here: take
the next free number above each dialect's high-water mark at the time of writing, per #563.

**Do not expect the query sites to be the expensive part.** `workflow_defs` appears in 103
non-test Go lines and 164 test lines (`grep -rn workflow_defs --include='*.go'`, 2026-09-02),
but most already carry `tenant_id` in their predicates — §3.10, §3.11 and §3.12's writer fix
went through them. The expensive part is the migration, and the count is a poor proxy for it.

### 3.86 SQL Server's cross-tenant exemption is per-connection, and the statements that lean on it have no predicate of their own — 🟢 **31 STATEMENTS FIXED across schedules, tags, definitions, the control plane and the claim path (§3.91); the gate is in place and the remaining 27 are an allowlist with reasons, not a backlog** (WS-1, 2026-09-03)

Found while scoping §3.77 step 3 (`workflow_schedules`), and it is not part of that change:
it is true on today's schema, needs no migration, and is reachable from the ordinary HTTP API.

#### The two facts, and why they only matter together

**1. `dbo.fn_tenant_filter` admits an entire connection.** `migrations/mssql/012_admin_role.sql`
redefines it as

```sql
RETURN SELECT 1 AS access
    WHERE @tenant_id = CAST(SESSION_CONTEXT(N'tenant_id') AS UNIQUEIDENTIFIER)
       OR IS_ROLEMEMBER(N'cleat_admin') = 1;
```

`IS_ROLEMEMBER` is a property of the login, not of the statement, so a `cleat_admin` connection
has **no filtering on any table the policy is bound to** — not just the ones it needs.

**2. A multi-tenant SQL Server deployment must grant that role.** Not "might have": a
non-default tenant's workflows do not run at all unless the dispatch loop can see across
tenants, which is what migrations 023/024 exist for. `GetDueSchedulesAcrossTenants` and the
cross-tenant claim both call `requireCleatAdminMembership` and fail loudly without it. And
`MSSQLStore.WithTenant` returns `cp := *s` — a copy sharing `s.db`. One pool, one login. So on
a working multi-tenant deployment, **every tenant-scoped store the worker hands to a request is
running unfiltered**, and the Go predicate is the whole of the isolation.

That is the same conclusion §3.11 reached for MySQL, arrived at from the opposite direction:
MySQL has no policy to lean on, SQL Server has one that is switched off for exactly the
deployments that need it most.

**PostgreSQL does not have this problem, and the contrast is the design lesson.** There the
exemption is a separate role, `cleat_dispatcher`, owning a `SECURITY DEFINER` function; the
application role keeps `BYPASSRLS` off, so the widening is scoped to one function body. SQL
Server puts the exemption in the predicate, and a predicate cannot tell which statement is
asking. `engine/mssql_schedules.go` recorded this as a simplification — *"a connection either
sees across tenants for both or for neither"*, offered as one grant to audit instead of
PostgreSQL's two. It is the same fact; only the sign was wrong.

#### Measured, not reasoned — four of five reachable over HTTP

Two tenant-scoped stores over one `cleat_admin` pool, calling the store methods
`cmd/cleat-worker/server.go:993-1005` reaches through `scopedStore` on an authenticated
request. **Names differ between the tenants**, so none of this depends on §3.77:

| statement | what tenant B did to tenant A |
|---|---|
| `DeleteSchedule` | deleted A's `tenant-a-nightly-report` outright |
| `SetScheduleEnabled` | disabled A's `tenant-a-billing-sweep` — still listed, still shows a `next_run_at`, never fires |
| `UpdateScheduleNextRun` | moved A's `tenant-a-reconcile` 72 hours out |
| `GetDueSchedules` | the *tenant-scoped* reader returned A's rows |
| `ClaimDueSchedule` | advanced A's schedule and reported the claim succeeded |

All five statements read `WHERE name = @p1` with no `tenant_id`. Fixed by adding the predicate
to each; `engine/mssql_admin_login_schedule_tenant_test.go` is the regression test, and each of
the five was watched red first and red for its own message.

The quiet ones are worse than the deletion. A deleted schedule is at least a thing that is
gone; a disabled one is listed in the dashboard, enabled-looking, with a future `next_run_at`,
and simply never fires — and nobody attributes that to another tenant.

**Why the test skips rather than fails when the pool is not a `cleat_admin` member.**
`MSSQLAdminDB` returns the plain pool unchanged when the schema carries no security policies,
and with no policy there is no exemption to be tested — a genuine environmental precondition.
The test asserts `IS_ROLEMEMBER` explicitly rather than assuming, because passing on a filtered
connection would be a green result measuring nothing.

#### Second table — `workflow_tags`, fixed 2026-09-02

Found by asking the question this entry ends with, of the next table §3.77 was going to touch.
Five more statements, same defect: `SetWorkflowTag`'s `MERGE`, `RemoveWorkflowTag`,
`GetWorkflowTag`, `GetWorkflowTags` and `ResolveVersionByTag`, all keyed on
`workflow_name`/`tag` with no tenant.

**Tags are a worse thing to get wrong than schedules, and it is worth being precise about
why.** A tag is not data *about* a workflow — it is the pointer that decides **which version
runs**, because `ResolveVersionByTag` turns `"stable"` into a version number at start time.
Repointing another tenant's `stable` does not corrupt a row; it changes the code their next run
executes. That is §3.12's outcome reached through a different table.

Measured on a `cleat_admin` pool with names tenant B does not own:

| statement | what tenant B did to tenant A |
|---|---|
| `GetWorkflowTag` | read A's `stable` → v2 |
| `GetWorkflowTags` | enumerated A's tags without needing to guess one: `map[stable:2]` |
| `ResolveVersionByTag` | resolved A's `stable` → v2 |
| `RemoveWorkflowTag` | deleted A's tag; A's own read then says *not found* |
| `SetWorkflowTag` | **repointed A's `stable` from v2 to v1, silently** |

**The last one could not have been found from a tenant-scoped connection, and would have
reported the opposite.** There `dbo.fn_tenant_filter` hides A's row, so the `MERGE` falls
through to `WHEN NOT MATCHED` and the `INSERT` trips the primary key — loud, and looks like
correct rejection. Only on a `cleat_admin` login is the filter off, the match succeeds, and the
update lands. Which connection a test runs on decides which answer it gets.

**The `MERGE` is shaped like `DeployWorkflowDef`'s on purpose.** Writing the tenant as a
projected source column —
`USING (SELECT ... CAST(@p4 AS UNIQUEIDENTIFIER) AS tenant_id) ... ON target.tenant_id =
source.tenant_id` — is equivalent SQL but trips
`TestMSSQLUUIDColumnsAreConvertedInProjections`, whose scan is textual and reads
`tenant_id = ... tenant_id` in a **join predicate** as a projection. It is a false positive,
and the fix is to match the existing `ON target.tenant_id = @p4` form rather than to weaken a
guard that exists because two real bugs got past review. Second time this has been hit; the
comment on `SetWorkflowTag` now says so at the site.

**A MySQL finding recorded here rather than acted on.** The same probe on MySQL showed
`SetWorkflowTag` silently overwriting the other tenant's row — B's write succeeds, A's `stable`
moves to B's version, and **B's own subsequent read returns nothing**, because the row it
updated carries A's `tenant_id` and B's `SELECT` is scoped. Bounded by D1: MySQL is documented
single-tenant-only for want of row-level security, so this is not a supported configuration.
§3.77 step 4 removes it anyway, by putting the tenant in the key that `ON DUPLICATE KEY`
matches on.

#### Third table — `workflow_defs`, fixed 2026-09-03, and this one is not a leak

The first two tables let one tenant read or damage another's rows. This one lets **a worker
execute the wrong tenant's compiled code**, and §3.77 is what made it reachable.

Before migration `035`, two tenants could not both hold a definition called `order-processor`,
so `WHERE name = @p1 AND version = @p2` identified exactly one row *even on a connection with no
tenant filtering*. After `035` it does not. On a `dbo.cleat_admin` login that predicate matches
both tenants' rows and `QueryRow` takes whichever comes back first.

Measured with tenant A holding `order-processor` v1 as `0xAA` and tenant B holding its own v1 as
`0xBB`:

```
LoadWASM(A)          -> 0xAA        correct
LoadWASM(B)          -> 0xAA        TENANT B WAS HANDED TENANT A'S COMPILED CODE
ListVersions(B)      -> [2 1 1]     B deployed exactly one version
ListWorkflowDefs(B)  -> 3 rows      B deployed exactly one definition
PurgeWorkflowDef(B)  -> deleted TENANT A's wasm_bytes as well
```

So the consequence is not that B learns something about A: B's workflow runs A's program, and
every check downstream — ABI version, replay history, result — is then about code the tenant
never deployed. `ListVersions` returning `[2 1 1]` is the visible tell, and nothing was looking.

**Twelve statements**, all in `engine/mssql_deployment.go`: `LoadWASM`, `ListVersions`,
`LoadWorkflowConfig`, `LoadDAGSpec`, `ListWorkflowDefs` (**both** branches — the empty-name one
listed every tenant's definitions), `GetWorkflowDef`, `MarkVersionDeprecated`,
`PurgeWorkflowDef`, `CountActiveInstances`, `ValidateVersion`, `GetRoutingRules`,
`RemoveRoutingRule`.

**The assertions had to be stronger than §3.77 step 1's, and that is the reusable point.**
`def_lookup_tenant_property_test.go` asserts *"tenant B sees nothing"* — correct when only one
tenant could hold a name, and **wrong now**: B is entitled to its own `order-processor`. The new
file asserts B sees **its own** answer, against a lopsided fixture (A holds v1+v2, B holds v1),
so "its own" and "the other's" are different values. A symmetric fixture would pass with no fix
at all. **A tenancy assertion written before D7 may now be asserting the wrong thing**, and that
is worth checking wherever one exists.

Step 1's file also could not have caught this whatever it asserted, because it runs on
tenant-scoped stores where the policy does the filtering. Same lesson as the tags file: which
connection the test runs on decides the answer.

#### The count, finally derived — and why the next step is a guard

Statements in `engine/mssql*.go` touching a table bound to `dbo.fn_tenant_filter`, classified by
whether the text carries `tenant_id`, tables derived from the `ADD FILTER PREDICATE` bindings in
`migrations/mssql/*.sql` rather than hardcoded:

| | 2026-09-03, before | after the three tables | after the control plane |
|---|---|---|---|
| carry a tenant predicate | 48 | 61 | 66 |
| do not | **47** | **34** | **29** |

Thirteen SQL literals, not twelve, because `ListWorkflowDefs` carries two -- the named branch
and the empty-name one. **I wrote 60/35 into this table from arithmetic and the re-derivation
said 61/34**; the command below is why that error lasted a paragraph instead of shipping.
These three columns were produced by `scripts/mssql-tenant-predicate-audit.py`, which asked
whether `tenant_id` appeared **anywhere** in the statement. **That script has been deleted** and
`engine/mssql_tenant_predicate_test.go` replaced it, because the substring question gave a wrong
answer twice — see §3.91 and the `DeliverSignal` MERGE below. The numbers above are kept as the
history they are; they are not re-derivable, and the basis changed rather than the tree. What is
re-derivable is the current state, and it is a list in the tree rather than a count:

```
go test ./engine/ -run TestMSSQLTenantScopedTablesAreQueriedWithATenantPredicate
```


The 34 that remained at that point were **not** the same class. They key on a *generated* id —
`workflow_instances(id)`, `event_history(workflow_id)`, `workflow_signals(workflow_id)` — where
§3.77's table already argued a UUID cannot be guessed. That is a weaker guarantee than a tenant
predicate (an id that leaks becomes a capability: on an unfiltered connection, knowing another
tenant's workflow id is enough to terminate it), but it is a different argument and it needed
deciding rather than sweeping.

**Decided 2026-09-03, and the decision split the population rather than answering it** — the
next subsection is that split. Five of the 34 take their id from an HTTP request and are fixed;
the remaining 29 are scoped by their caller and are deliberately left.

#### Fourth group — the control plane, fixed 2026-09-03, and the argument that was doing two jobs

The 34 above were left alone because they key on a generated id, and §3.77's table argued a UUID
cannot be guessed. Splitting that population by **where the id comes from** shows the argument
was covering two different things and is only true of one:

- **Five methods take the id from outside.** `TerminateWorkflow`, `RetryWorkflow`,
  `RequestCancellation`, `GetQueryState` and `DeliverSignal` are each reachable from an HTTP
  handler that reads the id straight out of the URL path — `cmd/cleat-worker/app.go`'s
  dead-letter terminate and workflow retry, `server.go`'s query, signal and cancel routes.
  Unguessability is a claim about what an attacker knows, not about the code, and a workflow id
  travels: into logs, a support ticket, a URL, a user who has since left the tenant.
- **The rest are plumbing**, and are safe for a *different* reason that had been hidden behind
  the same sentence: their ids come from rows the engine already read under a predicate, and
  `cmd/cleat-worker/setup.go:storeFor` re-scopes the store to each instance's own tenant before
  any of them run. They are scoped **by construction**, not by unguessability.

Six SQL literals across the five methods. Measured 2026-09-03 by removing each predicate on its
own — every removal turned exactly one case red, so the cases are independent rather than one
fixture failing together:

| statement | what tenant B did to tenant A |
|---|---|
| `TerminateWorkflow` | A's status is `terminated` |
| `RetryWorkflow` | A's dead-lettered workflow: status is `ready` |
| `RequestCancellation` | A's `cancellation_requested` is `1` |
| `GetQueryState` | B read `tenant-a-only` |
| `DeliverSignal` MERGE | **overwrote A's pending signal payload**: A reads `{"from":"tenant-b"}` |
| `DeliverSignal` wake | A's `next_wake_at` moved to now — A was **scheduled to run** on a signal it cannot read |

`engine/mssql_admin_login_control_plane_tenant_test.go` is the regression test.

**The `DeliverSignal` MERGE was not in the 34, and that is the finding worth keeping.** The
audit script asks whether `tenant_id` appears anywhere in the statement; it did — in the
`INSERT` column list, which scopes the row the call *creates* and says nothing about the row it
*matches*. A `MERGE` is an `UPDATE` when matched, so an unscoped `ON` clause let a caller
holding another tenant's workflow id overwrite that workflow's pending payload, and the wake
below it then ran the workflow. **A substring check cannot tell a filter from a projection**, so
the gate below needs to ask *where* the column appears. Re-derived with a position-aware probe:
seven statements name `tenant_id` outside any `WHERE`/`ON`/`HAVING` window, and six of those are
plain `INSERT`s (nothing to filter) plus the deliberately unscoped cross-tenant claim. The
`MERGE` was the only real one.

**Each case carries a positive control, and that is not decoration.** The failure mode of adding
`AND tenant_id` is not a leak, it is a statement that now matches nothing at all —
`TerminateWorkflow` does not check rows affected, so a predicate naming the wrong column would
make every terminate a silent no-op while the cross-tenant assertions still passed.

**One statement must NOT get a predicate, and the sweep would have broken it.** `BatchHeartbeat`
(`engine/mssql_lifecycle.go`) is keyed on `WHERE assigned_to = @p1` with no id at all, and is
called on the worker's own store (`cmd/cleat-worker/setup.go:1876`). Under `claim-across-tenants`
a worker legitimately holds instances of many tenants; a tenant predicate there would silently
stop heartbeating the others and they would be reaped as stale. Note also that the "a UUID
cannot be guessed" argument does not even *apply* to it — there is no id in the predicate to be
unguessable. Left alone deliberately, with the reason at the site.

**A residual, recorded rather than fixed.** Since the `MERGE`'s `ON` is now tenant-scoped, a
cross-tenant delivery to a signal the victim already holds falls through to the `INSERT` branch
and `pk_workflow_signals` refuses it — safe, but a 500, and distinguishable from delivering to
an id that exists nowhere (which still succeeds, creating a harmless orphan row under the
caller's own tenant). That is a weak cross-tenant existence oracle. The key itself is right and
should not change: `workflow_id` is generated and globally unique, so "one pending signal per
workflow per name" is a global truth, and putting `tenant_id` in that key would *allow* two
tenants to hold a signal for the same workflow. Turning the refusal into a clean not-found is a
change to an HTTP contract and belongs in its own PR.

**Three tables, five statements, five statements, twelve statements — all found by reading one
file while scoping something else.** That is not a method, and the fourth file would not be
either. **The guard is now written**: `engine/mssql_tenant_predicate_test.go`, shaped after
`TestMSSQLUUIDColumnsAreConvertedInProjections`. It derives the tenant-scoped tables from the
`ADD FILTER PREDICATE` bindings in the migrations, scans the store's SQL literals, and fails at
authoring time on any statement touching one without a tenant predicate or an allowlist entry
giving a reason. No SQL Server required, so it runs in every job.

It asks **where** the column appears — in a `WHERE`, `ON` or `HAVING`, or, for an `INSERT`
(which has no rows to leak), in the column list it writes. That is the difference that matters:
its substring-matching ancestor passed the `DeliverSignal` MERGE and both claim statements
(§3.91), and has been deleted rather than left to give a second opinion.

Falsified six ways, each red for its own reason: a real predicate removed, the MERGE `ON`
unscoped, §3.91's claim predicate removed, an allowlist entry deleted, a stale allowlist entry
added, and the migration parse broken (which `Fatal`s rather than passing vacuously).

#### What is NOT fixed

The 27 statements that remain. The guard that would stop this recurring **is done** — see
above. Those 27 are now a
**characterised** population rather than a leftover: every one is plumbing whose id the engine
read back from a row it had already scoped, running on a store `storeFor` scoped to the
instance's own tenant. Adding predicates there would be a no-op in every working path — 29
regression tests proving something the caller already guarantees — so they are deliberately not
swept. They are no longer a count to re-derive: since 2026-09-03 they are the 27 entries of
`tenantPredicateAllowlist` in `engine/mssql_tenant_predicate_test.go`, each carrying the reason
that is actually true of it, and the gate fails if one is added, removed or left stale.

**The allowlist the gate needs must say "scoped by construction", not "guessing is hard".** The
two are different claims and only the first is true of what remains; recording the weaker one
would re-merge the populations this entry just separated.

The sweep this needs is a property test over the boundary rather than an audit of statements,
for the reason CLAUDE.md gives and §3.77 step 1 demonstrated: reading SQL tells you which
statements have `AND tenant_id`, only running them tells you which ones leak. The shape is
already written — `def_lookup_tenant_property_test.go` — and wants generalising to *every*
store method on a `cleat_admin` connection, which is a different fixture rather than a
different idea.

Whether the exemption should be restructured to match PostgreSQL's — a `SECURITY DEFINER`-style
narrowing so the application login never holds it — is the larger question and is **not**
decided here. It is the fix that would make the sweep unnecessary rather than merely complete,
and it is a schema change to a security policy, so it belongs in review and not in this entry.

### 3.12 One tenant's deploy silently replaces another's workflow code — 🔵 **OVERWRITE CLOSED; THE NAMESPACE DECISION IS MADE** (WS-1, 2026-08-05; D7 2026-09-02)

Found while fixing §3.10: the two-tenant test could not deploy a definition of the same name
from both stores, and the reason it could not turned out to be worse than the inconvenience.

`workflow_defs`' primary key is `(name, version)` on every dialect — `PRIMARY KEY (name,
version)` (postgres, mysql), `CONSTRAINT pk_workflow_defs PRIMARY KEY (name, version)`
(mssql) — with no tenant in it, and definition names are chosen by whoever deploys. All three
`DeployWorkflowDef` implementations upsert on that key (`ON CONFLICT (name, version) DO
UPDATE`, `ON DUPLICATE KEY UPDATE`, `MERGE ... WHEN MATCHED THEN UPDATE`), so the second
tenant to deploy a given name does not collide — it **overwrites**.

Measured on postgres, mysql and mssql, with a per-tenant store apiece (the harness §1.7 and
§2.71 use, including PostgreSQL's genuinely RLS-enforcing connection). Tenant A deploys
`shared-def-name` v1, tenant B deploys its own v1 of the same name, then A reads its
definition back:

```
postgres  B deploy err = <nil>   A reads back: wasm_bytes = [1 2 3 4]   (B's bytes)
mysql     B deploy err = <nil>   A reads back: wasm_bytes = [1 2 3 4]   (B's bytes)
mssql     B deploy err = <nil>   A reads back: wasm_bytes = [1 2 3 4]   (B's bytes)
```

So this is not an information leak, it is code replacement: tenant B decides what tenant A's
workflows execute, by picking a name. Two things make it reachable rather than theoretical:

- **`PostgresStore.DeployWorkflowDef` hardcodes the default tenant.**
  `store_deployment.go:161` is a literal `tenantID := "00000000-0000-0000-0000-000000000000"`,
  ignoring `s.tenantID` — so on PostgreSQL every definition every tenant deploys is written as
  the default tenant's. `MSSQLStore`'s `MERGE` does not name `tenant_id` in its INSERT column
  list at all and takes the column default, which is the same value. Only `MySQLStore` passes
  `s.tenantID`.
- **The RLS policy on this table admits the default tenant by design**:
  `tenant_id = cleat.assert_tenant_set() OR tenant_id = '00000000-…'`, presumably for shared
  definitions. Combined with the line above, every definition is a shared definition.

Not investigated here: whether the HTTP layer's §1.7 ownership checks constrain which names a
tenant may deploy. That bounds the severity and does not change the store-level finding.

The fix is not only a wider primary key: `(name, version, tenant_id)` without correcting the
two writers above would put every definition in one tenant anyway. Expect a migration in
WS-1's range, the two writer fixes, and a decision about what "shared definition" should mean
now that it is the accidental default.

#### Resolution — the overwrite is closed, the namespace is not

The bounded half is done, chosen over the full redesign because the key change reaches three
foreign keys per dialect and ~96 query sites and wants its own review:

- **A definition records its owner.** `PostgresStore.DeployWorkflowDef` uses `s.tenantID`
  instead of the hardcoded default, and `MSSQLStore`'s `MERGE` names `tenant_id` in both its
  INSERT and its UPDATE. `MySQLStore` already did.
- **A deploy over someone else's definition is refused**, with an error wrapping
  `engine.ErrWorkflowDefOwnedByAnotherTenant`, in a transaction that locks the row (and the
  gap it would occupy) first. On PostgreSQL the guard is repeated in SQL, because under RLS
  the conflicting row is invisible to the read and the INSERT hits the primary key instead —
  so `23505` is mapped to the ownership error rather than surfacing as `duplicate key value
  violates unique constraint`, which says nothing about what went wrong.
- **What is not fixed:** two tenants still cannot each hold `order-processor`, and one
  tenant's definition is still readable by name from another. The namespace is shared; taking
  a name is now loud instead of silent. **Decided 2026-09-02 (D7): it becomes per-tenant**, and
  the same rule covers two further tables this entry never mentioned. The work, and the
  migration hazard that has to be settled before any SQL is written, are in §3.77.

**The soft edge, stated rather than buried.** Every definition in every existing database is
owned by the default tenant, so refusing those outright would break the first redeploy after
the upgrade for every tenant at once. A default-tenant definition is therefore *adopted* by
the first tenant to redeploy it. Until that happens, a tenant other than its creator can still
take it over. `CHANGELOG.md` carries this as a breaking upgrade note.

**Proven able to fail:** with the three writers reverted to `develop`'s, all three dialects
report `tenant B deployed over tenant A's definition "order-processor" and was told it
succeeded`, and postgres and mssql additionally report the owner as the default tenant. MySQL
passes the ownership half unchanged, which is the asymmetry recorded above.

**Two things the fix turned up in the test suite, both of which were the tests depending on
the defect:**

- Eight `TestTenantIsolation_*` fixtures deployed *one* `*WorkflowDef` to both tenants' stores.
  That only ever worked because the second deploy overwrote the first. They now give tenant B
  its own definition, which is what a multi-tenant deployment has to do until the key changes.
  None of their assertions moved.
- `TestMSSQLStore_StartNewRun_TenantID` inserted a `workflow_defs` row with an explicit NULL
  `tenant_id`. `migrations/mssql/001_schema.sql` declares that column `NOT NULL DEFAULT '000…'`
  and always has — the row the test inserted could not exist in a real database. It passed
  because `engine/testutil`'s MSSQL schema left the column nullable: the §1.9 drift class
  again, found by reading the column back rather than by reading the schema.

~~**Residual, and it is the same shape as §3.11:** a definition's *contents* are still readable
across tenants by name — `LoadWASM`, `GetWASMLength`, `LoadDAGSpec` and `LoadWorkflowConfig`
key on `(name, version)` with no tenant. PostgreSQL's RLS policy for this table admits the
default tenant deliberately, so pre-upgrade definitions stay globally readable by design; on
MySQL and SQL Server there is nothing underneath at all. Closing that is the same work as
putting the tenant in the key.~~

**Residual restated 2026-08-31 — the last sentence of that was wrong, and it overstated the
exposure.** The four methods do take `(defName, defVersion)` with no tenant argument, which is
what the old text was reading. But the *signature* is not the isolation, and every dialect
scopes the read:

| dialect | what scopes the read | re-derive |
|---|---|---|
| PostgreSQL | RLS — `LoadWASM` runs inside `beginTxWithRLS` | `grep -n "beginTxWithRLS" engine/store_deployment.go` |
| MySQL | the SQL itself — `AND tenant_id = ?` against `s.tenantID` | `grep -n "func (s \*MySQLStore) LoadWASM" -A4 engine/mysql_ops.go` |
| SQL Server | `FILTER PREDICATE dbo.fn_tenant_filter(tenant_id)` on `dbo.workflow_defs` | `grep -n "ON dbo.workflow_defs" migrations/mssql/001_schema.sql migrations/mssql/012_admin_role.sql` |

"On MySQL and SQL Server there is nothing underneath at all" was false on both counts.
`GetWASMLength`'s own comment on the MySQL side says the opposite in as many words — *"MySQL has
no row-level security, so this predicate is the whole of the isolation"* — so the evidence was
sitting in the code the residual was describing.

**What is actually left**, stated at its real size: the PostgreSQL policy admits default-tenant
rows (`tenant_id = cleat.assert_tenant_set() OR tenant_id = '000…'`), so definitions that predate
this item stay readable by every tenant until the first redeploy adopts them. That is a
deliberate migration window, documented in `CHANGELOG.md` as a breaking upgrade note, and it
narrows as deployments redeploy. Putting `tenant_id` in the primary key closes it and lets two
tenants hold the same definition name — still worth doing, but it is tidiness plus a shrinking
window, not the open cross-tenant read this entry claimed.

**Found on the way, and worse than the residual itself:** `migrations/postgres/001_schema.sql`
justified that policy with *"DeployWorkflowDef always writes `tenant_id = '00000000-…'`
regardless of the calling store's tenant, because workflow definitions are a shared/global
registry, not tenant-partitioned data."* This item changed that line to `tenantID := s.tenantID`
and the migration's prose was never updated — so a shipped schema file argued for a security
policy from a premise its own codebase contradicts, and cited a test comment
(`engine/tenant_isolation_test.go`, "visible to all tenants") as the evidence. Both corrected;
the test comment now says *why* that particular definition is visible to all tenants rather than
implying definitions in general are.

### 3.13 No cleat-worker can bootstrap a MySQL schema — ✅ **FIXED** (WS-1, 2026-08-05)

Found by §3.10's migration test, which could not build a "before" state for MySQL.

`migration.Runner` splits MySQL files on every `;` (`splitSQL`, `runner.go:415`) with no
regard for what the semicolon is inside. Line 7 of `migrations/mysql/001_schema.sql` is

```
-- CREATE INDEX has no IF NOT EXISTS in MySQL 8.0; re-runs error harmlessly.
```

so the runner sends `re-runs error harmlessly.` as a statement:

```
migration 001_schema.sql: execute: Error 1064 (42000): You have an error in your
SQL syntax; ... near 're-runs error harmlessly.
```

`cmd/cleat-worker/main.go:561` runs this Runner at boot and `os.Exit(1)`s on failure, so a
worker pointed at a MySQL database that has not already been schema'd by some other means
cannot start — the same shape as §1.11, on the other dialect. It has been invisible because
every MySQL test path in the repo builds its schema from `engine/testutil`'s Go copy instead,
which is exactly the divergence §1.9 is about: **the shipped schema is still not the tested
schema on MySQL.**

`003_procedures.sql` is very likely unapplicable too, for a second reason: it uses
`DELIMITER //`, which is a client directive rather than a server statement, and its procedure
bodies contain semicolons that `splitSQL` would cut. Not yet confirmed — 001 fails first.

The fix is comment- and string-aware splitting (or `multiStatements=true` and no splitting at
all), plus running the real files against a scratch MySQL database in CI so this cannot
recur. `migration/idempotency_tenant_test.go`'s MySQL arm is skipped pointing here, and that
skip is the acceptance test for this item.

#### Resolution — and the second reason, which was worse

`splitSQL` now tracks the four things a semicolon can be inside — a line comment (`--` with
the whitespace MySQL requires, and `#`), a block comment, a quoted string (both backslash and
doubled-quote escapes), and a backtick identifier — and honours `DELIMITER`, which is what the
file is asking the client to do. It is not a SQL parser and does not try to be; it knows
enough to find statement boundaries and leaves the rest to the server.

**The `DELIMITER` half was confirmed, and it is the more serious of the two.**
`003_procedures.sql` creates `finalize_workflow_status` — the procedure the engine calls on
every workflow completion, with no fallback. Its body is full of semicolons, so the old
splitter cut it into fragments and then sent `DELIMITER //` to a server that has never heard
of it. So even with 001 fixed, a MySQL deployment would have come up without the one procedure
it cannot run without.

**Tested three ways, deliberately:**

- `migration.TestRunner_AppliesShippedMySQLMigrations` runs the real Runner over the real
  `migrations/mysql/` against a scratch database and checks the tables, the composite
  idempotency key from §3.10, and `finalize_workflow_status`. `TestRunner_SecondMySQLRunAppliesNothing`
  covers the restart-against-a-migrated-database path separately, because "applies nothing on
  a fresh database" and "re-applies on a live one" fail in different directions.
- `TestSplitSQL` is a table of twelve cases against the pure function, which had **no unit
  tests at all** — most of why this survived, since reproducing it needed nothing but calling
  the function with a string that is checked into this repo. Reverting the function body to
  the old one-liner fails seven of them.
- `TestSplitSQL_ShippedMySQLFiles` runs the splitter over the real files with no database and
  asserts the properties that matter: no fragment is pure prose (MySQL answers an empty query
  with 1065), no fragment is a `DELIMITER` directive, and nothing containing `CREATE
  PROCEDURE` has lost its `END`. That is the check that would have caught this on a laptop
  with nothing installed.

**And it now runs in CI, which was the actual gap.** `multi-db-ci.yml`'s `test-mysql` and
`test-mssql` jobs ran `./engine/...` only; they are the only jobs with live MySQL and SQL
Server, so a migration test could not run anywhere that had a server to run it against. Both
now run `./migration/...` too. Without that, this fix would have been guarded by a test that
skips in every job — which is the shape of the problem, not a fix for it.
`migration/idempotency_tenant_test.go`'s MySQL arm is unskipped, as this item's acceptance
test required.

**Not done, and small:** `tests/plugin-harness/testdb.go` carries a *second*, independent
statement splitter (dollar-quote and `GO` aware) that applies the same migration files. It
copes with the shipped MySQL files today — verified against a live MySQL 8.4 — so this is
duplication rather than a defect, but two splitters means the next one to drift does so
silently.

### 3.30 What wazero is for — ✅ **DECIDED 2026-09-01: it stays, scoped to CLI and dev tooling**

> **The question is settled; see §3.56 for the decision and the guard that is its price.** wazero
> is kept deliberately rather than removed: #503 made routing fail closed, so the safety case for
> removal was gone, and removing it would force `cleat` onto CGO — ending pure-Go
> cross-compilation for the CLI — while breaking exported API (`engine.Runtime`,
> `engine.NewRuntime`, `wasmtest.WasmTestEnv.Runtime()`).
>
> **Point 1 below is wrong and is kept only because the correction is the interesting part.** It
> says a `CGO_ENABLED=0` binary "has no wasmtime at all and everything runs on wazero". A
> CGO-less `cleat-worker` does not run anything: it logs "there is no fallback" and exits 1
> (`cmd/cleat-worker/main.go:789`). That mattered in practice — every released worker binary was
> built `CGO_ENABLED=0` and was dead on arrival until §3.54.
>
> The prior marker read "ANSWERED … corrected 2026-08-31", which is a status about the *document*
> rather than the *question*, and it left the item looking open.

Raised because Python moving onto wasmtime (§2.72) emptied the set of languages wazero was
retained for. The question was whether it still has a stated, tested role.

Read off the tree rather than reasoned about. Three things still reach it:

1. **CGO-less builds.** `NewWasmtimeBackend` is behind `//go:build cgo`, so a
   `CGO_ENABLED=0` binary has no wasmtime at all and everything runs on wazero. This is the
   one legitimate remaining role. The shipped image is not in it — §2.28 moved the Dockerfile
   to a glibc base with CGO on, and `--verify-backend` fails the build if that regresses.
2. **`setup.go`'s `needsWazeroRuntime`**, which is `w.wasmtimeBackend == nil ||
   !runsOnWasmtime(DetectLanguage(wasmBytes))`. The second clause is now dead for every
   input: all five languages route to wasmtime, and `DetectLanguage` returns `"go"` when it
   cannot tell. So it reduces to case 1.
3. **Every deferred callback, unconditionally** — see §3.32. That is not a role, it is a
   defect, and it is the one place an unfenced backend still executes guest code on a
   fully-configured production worker.

**So the answer:** wazero is the CGO-less fallback and nothing else, plus one path that
reaches it by accident. `CLAUDE.md` has been corrected — it claimed wazero was "retained as a
fallback for the languages that do not work under wasmtime", and that set is empty.

Not proposed here: deleting it. A pure-Go build is a real distribution story and the CGO-less
path is the only thing keeping it available. What should not survive is §3.32.

> **Overtaken by events, 2026-08-10 — and this entry did not say so until 2026-08-31.**
> The answer above was right on 2026-08-05 and wrong five days later. **#459 deleted
> `engine/backend_wazero.go`**, so points 1 and 2 are both false now:
>
> - **Point 1 is inverted.** A `CGO_ENABLED=0` build does not "run everything on wazero" —
>   there is no backend left at all. `cleat-worker` logs *"wasmtime is the only WASM backend
>   cleat has, there is no fallback"* and exits 1 (`cmd/cleat-worker/main.go`). The
>   CGO-less distribution story this entry declined to give up was given up anyway.
> - **Point 2's `needsWazeroRuntime` no longer exists.** `grep -rn needsWazeroRuntime` returns
>   nothing.
>
> Re-derive: `ls engine/backend_wazero.go` (absent), `grep -rn needsWazeroRuntime --include='*.go' .`
>
> **What wazero still is, measured 2026-08-31:** not a backend, but `engine.Runtime`
> (`engine/runtime.go`) is still a wazero runtime, and it still executes guest code on seven
> production call sites — `RunDefer`'s fallback (`engine/executor.go`, twice),
> `cleat/wasmtest`, `cmd/cleat run_embedded`, `cmd/cleatctl replay`, `cmd/cleatctl debug` and
> `cmd/cleat-bench` (three). **20 non-test files still import wazero.** So the heading's
> "smaller than expected" has not aged well either: the *role* shrank to nothing while the
> *surface* did not shrink at all.
>
>     grep -rn "NewRuntime(" --include="*.go" . | grep -v _test.go
>     grep -rl "tetratelabs/wazero" --include="*.go" . | grep -v node_modules | grep -v _test.go | wc -l
>
> Removing the rest is "wazero removal, part 2" in `REMEDIATION-PLAN-2026-08-09.md`. **Its
> parked baseline is stale too:** that plan records 8 files importing wazero going to 4, against
> 20 non-test files today, so the stash is a map and not something to rebase.

### 3.31 The execution-limit story, per backend — ✅ **WRITTEN** (WS-3, 2026-08-05; closed 2026-09-01)

The item asked for the limit story to be written and tested per *backend* rather than per
language, on the grounds that §1.5 was a correct fix that reached no deployment for weeks
because it sat behind a build tag the shipped image did not set.

Writing it down is what found the gaps, so here it is in full. The wasmtime backend had
**three** execution paths, not one, and they had three different answers:

| path | entered when | fence before | fence now |
|---|---|---|---|
| core module (`Execute`) | Go, AssemblyScript, Java, Rust | caller's budget | unchanged |
| native component (`ExecuteComponentCGo`) | any Component Model guest, i.e. Python | **backend default, caller's budget dropped** | caller's budget |
| ~~decomposition (`ExecuteComponent`)~~ | ~~native path fails for a non-limit reason~~ | caller's budget | **deleted 2026-09-01 — §3.65** |
| defers (`RunDefer`) | every deferred callback, on every path | **none — wazero** | the fenced backend *when there is one*, since #338 — §3.32, **and bounded as a pass rather than per defer since §3.63** |

Two now, not three. Everything below is kept as written because the *findings* are what make the
remaining two paths' fences legible; only the decomposition row's disposition changed.

The component-path defect: `ExecuteComponentCGo` passed `context.Background()` to
`configureStore`, which takes the tighter of ctx's deadline and the backend's configured
timeout. With no ctx deadline to reconcile against, every component guest silently got the
backend-wide 30s default. Measured on a Python component that never returns: **a 2s budget
ran for 32.9s**. The fence was firing the whole time, on the wrong deadline, which is why it
did not look broken from outside.

Two things fell out of fixing it, both of which would have undone it:

- **The fallback handed a runaway guest a second budget.** `Execute` fell back from the
  native path to decomposition on any error, so an interrupted guest was started again from
  scratch and the effective bound became a multiple of the configured one. Limit traps stopped
  falling back; since §3.65 there is nothing to fall back *to*, so the class is closed by
  construction rather than by a conditional. `isExecutionLimit` is still load-bearing, though —
  it is what decides whether an operator is told the host stopped their workflow or that their
  workflow crashed.
- **`resourceLimitError` could not see a component-path limit at all.** It matched
  `*wasmtime.Trap`, and the Component Model C API returns no trap code — only a rendered
  message (`wasmtimeinc/wasmtime/error.h`). An exhausted budget arrived as a bare
  `wasm trap: interrupt` under a page of guest backtrace.

**What was still unwritten — closed 2026-09-01, both by the route this section predicted.**

The *decomposition* path's fence was inherited rather than verified: it passed ctx correctly,
but nothing exercised a runaway guest through it, because every component that reached it
failed to instantiate for other reasons. §3.65 deleted the path. This section's own addendum
called that outcome — *"resolves by deletion rather than by a test. Writing a fence test for it
first would be work spent on code that is on its way out"* — and that is what happened.

The defer gap below was the other one, and it is closed in §3.63 by measurement plus a fix.

And the defer row above is now true of `RunDefer` but not yet of the path that calls it after
a trap. `executor.go` passes `context.Background()` to the post-trap defers on purpose (three
sites: the `execCtx` deadline branch, the non-suspend-error branch, and the wazero path's
equivalent), so that cleanup still happens when the workflow's own context has timed out or
been cancelled — correct in intent, but it leaves `configureStore` with no deadline to
reconcile against, so those defers get the backend-wide 30s default rather than any
per-workflow budget. That is the same shape as the component-path defect above.

> **Measured 2026-09-01 — and it was worse than this paragraph predicted.** This used to end
> "**Read off the code, not measured** — noted that way deliberately, since every other finding
> in this section came from running something." Running it, with a backend timeout of 2s:
>
> | ctx handed to the defer | elapsed | budget the host reported |
> |---|---|---|
> | 200 ms deadline | 150 ms | `199.778625ms wall-clock budget` |
> | `context.Background()` | 2.001 s | `2s wall-clock budget` |
> | **3 runaway defers via `runDefers(Background)`** | **6.001 s** | `2s` each |
>
> The per-call behaviour is fine — with nothing tighter to reconcile against, the backend's own
> budget is the right answer. What nobody had noticed is that it is *per call*: each defer
> starts from a fresh copy, so the worst case grows without limit in the number of defers a
> workflow registered. On a worker that is 30s each, so twenty runaway defers held a slot for
> ten minutes. Fixed in §3.63.
>
> Re-derive: `go test ./engine/ -run TestDeferPass -count=1 -v`

The generalisable finding, which is the one worth carrying: **"which backend runs this" and
"which code path inside that backend runs this" are different questions, and the limit story
has to be told about the second.** Both defects above sat inside the backend that CLAUDE.md
calls the behaviour of record.

> **Correction to the defer row, 2026-08-31.** It read "the fenced backend, since #338", full
> stop, and that is only half of what the code does. `RunDefer` uses the fenced backend **when
> `backendForWasm` returns one**; when it returns nil it falls through to `engine.Runtime` —
> wazero — under a comment that says so in as many words:
>
> ```go
> // No backend for this guest: the CGO-less build, where wazero is the only
> // runtime there is. Unfenced, and unavoidably so.
> ```
>
> `CLAUDE.md` states this correctly and this section did not, which is the wrong way round for
> a section whose whole subject is where the fence is. The row now says "when there is one".
>
> The gap is not hypothetical-only-in-a-CGO-less-build either: since #459 deleted the wazero
> *backend*, a CGO-less worker exits at startup, so that branch is now reached by a guest whose
> language has no registered backend rather than by a whole build mode. Either way the fence is
> absent and nothing measures it.

> **Superseded, 2026-09-01.** #503 made `resolveBackend` fail closed on exactly that case, so
> the fall-through is now reached only by an engine that registers no backends at all —
> `cleatctl replay|debug`, `cleat run_embedded`, `cleat-bench`, `cleat/wasmtest` — where the
> wazero Runtime is the intended executor rather than a fallback. The unfenced surface is
> developer tooling, not anything a worker runs.
>
> **The re-derive command this block used to carry was a trap, and is the reason it is being
> corrected rather than deleted.** It was
> `grep -n "Unfenced, and unavoidably so" engine/executor.go`, and it still matches — at
> `engine/executor.go:795`, inside a comment that *quotes the old wording in order to say it
> was wrong*: "This comment used to say ... Both halves were wrong." A grep that confirms a
> stale claim by matching the text of its own retraction is the same failure as CLAUDE.md's
> §1.1 note, where checking a bug report against the migration file it named confirmed a bug
> that a later migration had already fixed. Read the match, not the exit code.

#### 3.31 addendum — the decomposition path has never successfully run anything (2026-08-05)

Chased because writing the table above raised the question "what actually reaches decomposition
now?", and the answer turns out to be stronger than "not much".

Three facts, each checked rather than reasoned:

1. **Decomposition is entered only for Component Model binaries.** `Execute` gates the whole
   branch on `isComponentWasm`, which tests for the `0d 00 01 00` version/layer at offset 4.
2. **Python is the only producer of one.** The four build targets are `build_as.go`,
   `build_java.go`, `build_python.go` and `build_rust.go`; AssemblyScript, TeaVM Java and the
   Rust cdylib all emit core modules. Scanning every `.wasm` tracked in the repo for the
   component magic returns exactly two files, both the same Python fixture
   (`tests/plugin-harness/testdata/pythonworkflow/call_all_plugins.wasm` and its
   `.component.wasm` twin).
3. **Python components fail decomposition.** The stale fixture failed there at instance 15,
   then 81 after the `env::abort` arity fix; a component built fresh fails at instance 52
   (§2.72). No Python component has ever come out the other side.

Put together: **the ~600 lines of hand-rolled shared-everything dynamic linking in
`backend_wasmtime.go` — the GOT.mem/GOT.func routing, the placeholder tables, the
"instance with the most exports is the CPython runtime" heuristic, the multi-pass instantiation
loop and its `undefined element` retry — have never successfully executed a workflow.** It is
not dead code in the `check-test-only-code.sh` sense, because it is wired and reached; it is
something rarer, code that is reached and has never once succeeded.

Since 2026-08-05 it is also no longer the path Python takes, so it is now reached only when the
native component path fails first.

**Not proposing deletion**, and the reason matters: the native path has one known limit —
`componentGetFunc` passes a nil parent export index, so it resolves only top-level exports and
cannot reach a function nested inside an exported interface (§2.72). A component shaped that
way would fall through to decomposition today. Deleting decomposition without first fixing that
would turn a bad error message into a hard failure.

So the honest disposition is: **fix `componentGetFunc`, then delete the decomposition path**,
in that order, and the §3.31 gap above ("the decomposition path's fence is inherited rather
than verified") resolves by deletion rather than by a test. Writing a fence test for it first
would be work spent on code that is on its way out — which is worth saying explicitly, because
"add the missing test" is the reflex the rest of this document encourages.

> **Correction, 2026-09-01: the first half of that ordering is not a prerequisite.** The
> `componentGetFunc` limit is real — measured, not read — but **nothing in the tree or the
> toolchain produces a component that hits it**, so "deleting decomposition would turn a bad
> error message into a hard failure" describes a component shape cleat cannot currently build.
>
> **The limit, measured.** A hand-written component exporting `run` both at the top level and
> through an interface instance `cleat:workflow/entry`, run through `ExecuteComponentCGo`:
>
> | `entryPoint` | result |
> |---|---|
> | `run` | ✅ `{Result:"ok"}` |
> | `cleat:workflow/entry` | `component export … not a function` |
> | `cleat:workflow/entry#run` | `component export … not found` |
> | `entry` | `component export … not found` |
>
> There is **no spelling** that reaches the nested function. Confirmed.
>
> **Why it is unexercised.** Two checks, either of which settles it:
>
> - The only Component Model binary in the repo — `call_all_plugins.wasm.component.wasm`,
>   19.3 MB — has exactly **two** component-level exports: `run` (sort byte 1, *func*) and
>   `exports` (sort byte 5, *instance*). The entry point is the top-level func.
>   Re-derive with `wasm.ParseComponentBundle` and print `bundle.Exports`.
> - `python-sdk/wit/cleat.wit`'s world says `export run: func(args: string) -> string;` at the
>   **world** level, not inside an interface. componentize-py is cleat's only component
>   producer, and this is the world it compiles against, so every component it emits exports
>   `run` at the top level by construction.
>
> A nested export would require someone to rewrite that world as `export cleat:…/…;` — a
> deliberate change nobody has made or proposed.
>
> **And `tiers.yaml` already parks the thing the prerequisite protects.** It excludes
> `TestComponentPythonBinary` as "tier 3 — component decomposition … Excluded because the path
> it covers is parked". Tier 3 is *"not built, not shipped, not claimed"*. So the ordering as
> written sends the next reader to write speculative code — an export-lookup path with no
> producer and a test only its author would ever exercise — in order to protect a path the
> support manifest has already declined.
>
> **Revised disposition.** `componentGetFunc` is a *documented limitation*, not a blocker.
> Fix it when something needs it, which today is nothing; the fix is to resolve the interface's
> export index first and pass it as the parent, and this note is enough to start from. The
> decomposition deletion stands on its own merits and its own risk assessment.

### 3.32 Every deferred callback runs on wazero, unfenced — ✅ **FIXED** (WS-3, 2026-08-05)

`Engine.RunDefer` does not consult `backendForWasm`. It reaches straight for `e.rt`, the
wazero Runtime, and when that is nil — which is exactly the case when wasmtime is handling
execution — it constructs a *fresh wazero Runtime* for the defer.

So every deferred callback in cleat runs on wazero, whatever the routing table says, on a
fully-configured production worker where the workflow it belongs to just ran on wasmtime.
wazero only observes context cancellation when the guest calls back into the host (§2.28), so
a defer that loops without doing so is never stopped: it holds its worker slot until the
process dies. A defer body is ordinary guest code — it can loop exactly like a workflow body
can, and `--concurrency=N` runaway defers wedge a worker just as thoroughly.

**Demonstrated, not read.** `cmd/cleat-worker/defer_backend_test.go` drives the real
`w.runDefers` with a defer export that never returns, against a 1s wasmtime budget. It is
still running 20 seconds later. The test is skipped and recorded (`test-go/commands` budget
6 → 7); unskipping it is the acceptance test.

**Worth keeping from writing that test**, because it is this repo's recurring failure mode in
miniature: the first version declared the defer export with no parameters and **passed in
0.01s with the fix reverted**, having never executed the guest. `CallExport` rejected it with
`expected 0 params, but passed 4`, and `runDefers` logs defer failures without propagating
them — so a test that ran nothing was indistinguishable from a test that proved something.
The fixture's logger discards output, which is what hid the rejection.

**Why it is recorded rather than fixed.** Routing defers through a backend is not a one-line
change. Defers today execute with **no `HostHandler` in ctx** — `runDeferCompiledWithRT` never
calls `withHandler` — so a defer that makes a host call is already broken, and it fails
differently on the two backends. What a defer body is allowed to do wants deciding before the
routing changes, or the fix will silently convert one failure mode into another. That decision
is the work item; the fence follows from it.

**Also fixed while in there** (`cmd/cleat-worker/setup.go`, both defer and workflow paths):
`rt.Metrics = w.Metrics` executed *before* the `if err != nil` check on `engine.NewRuntime`,
which returns `(nil, err)` on four paths. Every one of them was a nil dereference rather than
the logged failure the code below it intended. The tell was the `if rt != nil { rt.Close() }`
guard inside the error branch — written to handle a case it could never reach.

#### 3.32 progress — the net exists, and one option is eliminated (2026-08-05)

Two things that were prerequisites for fixing this, both done.

**No test had ever executed a defer body.** The three `TestRunDefers_*` in `flush_test.go` each
pass `wasmBytes = nil` and say so — *"wasmBytes is nil so RunDefer is not invoked; verify no
panic"* — so they cover the sorting and the nil guards and stop exactly where the guest begins.
`TestClosure_CleatDefer` covers the host function a workflow uses to *register* a defer, not
running one. So the change this item asks for would have been made against nothing.
`engine/defer_execution_test.go` closes that: a defer that returns cleanly, one that traps
(the trap is the cheapest proof the body was entered — it needs no host handler and no
output-ABI agreement, both of which a defer lacks), a missing export, and a wrong-signature
export. Each verified to fail when the WAT it runs is changed to remove the condition.

Two of those cases are not hypothetical. A defer whose export has the wrong signature is
rejected before it runs with `expected 0 params, but passed 4`, and `runDefers` logs that and
moves on — so a workflow author's cleanup silently never happens, and from outside it is
indistinguishable from cleanup that ran. That is the same shape as this item and it now has a
test.

**"Just fence wazero" is dead, and that is now measured rather than assumed.** Three mechanisms
have been tried against a guest that never calls into the host:

| mechanism | result |
|---|---|
| `WithCloseOnContextDone(true)` | breaks *every* execution with `wasm trap: exit(code=0)` (§2.28) |
| fuel metering (`fuelMeter`) | only decrements in `Before`, a function-entry hook — a tight loop inside one function never trips it |
| `mod.CloseWithExitCode` from a watchdog goroutine | **no effect**: still running 25s after the close, measured 2026-08-05 |

The third was the last plausible one, because `fuelMeter`'s own comment says closing the module
is how it stops a guest — true only for a guest that keeps calling functions. So there is no
way to bound a compute-bound wazero guest, and **routing to wasmtime is the only fix**, not one
option among several.

**What remains, and its cost.** Routing needs a non-nil `HostHandler`: `handlerFromContext` does
an unchecked type assertion, so a defer reaching the wasmtime backend without one panics rather
than failing. `HostHandler` has **54 methods**. So the work is either a rejecting
implementation of all 54 (mechanical, but 54 methods of new production surface, and it makes
"defers may not make host calls" explicit and permanent) or passing the live `execSession`,
which `executor.go` already has in scope at all three call sites — but `cmd/cleat-worker`'s own
`runDefers` has no session at all, so that route does not cover both callers.

Worth noting for whoever takes it: the two defer paths already disagree. `invokeDefersOnTrap`
runs defers on the **live module with the session's handler present**, so host calls from a
defer work there; `runDefers` runs them on a fresh module with no handler, where the same call
panics and is swallowed. Whatever is decided should make those two agree, because today
whether your defer can call a service depends on which way the workflow failed.

#### 3.32 resolution — option 1, the fence without the contract (2026-08-05)

`Engine.RunDefer` now asks `backendForWasm` and, when a backend serves the guest, runs the
defer through it. That is the whole fix; the analysis above is what made it a one-screen
change rather than a guess.

**Measured on the acceptance test this item named:** against a 1s budget,
`TestDefersRunOnTheFencedBackend` went from *still running after 20s* to returning in
**1.00s**. It is unskipped, and `test-go/commands` drops 7 → 6.

**The handler question is left open on purpose, and §3.35 is where it is answered.** Defers
still run with no `HostHandler`, exactly as before, so a defer that makes a host call still
fails. What changed is only *how*: it used to panic on an unchecked type assertion and be
swallowed; it now arrives as a recovered error that `runDefers` logs. That is a statement about
today's implementation and not a rule — a defer is meant to be a destructor with the context to
clean up, and §3.35 designs that. Verified rather than assumed — a defer body calling
`cleat_workflow_id` with a nil handler returns

```
host: wasmtime panic in "cleat_defer_defer-1": runtime error: invalid memory address or nil pointer dereference
```

which is recovered by the `fn.Call` guard, not a process kill. That is what made option 1
viable: the 54-method rejecting handler and the live-session route are both still open, and
both are now decisions about *what a defer may do* rather than prerequisites for bounding it.

**Two details worth keeping:**

- `PerExecution()`, not the backend itself. `Execute` stores the handler on the backend
  struct, so calling it on the shared root would race a concurrent workflow.
  `executeWithBackend` already took this precaution; RunDefer had to as well.
- `cmd/cleat-worker`'s `runDefers` registers backends again. That line was added and reverted
  once — on its own it changed nothing, because RunDefer performed no lookup, and shipping it
  would have read as a fix without being one. It does something now.

**Still true, and still 3.32's open tail:** `RunDeferCompiled` takes a pre-compiled wazero
module and cannot route, and the CGO-less build has no backend to route to. Both fall back to
the unfenced path, which for a build with no wasmtime in it is unavoidable rather than a gap.

### 3.35 What `defer` is supposed to be — 🔶 **PHASES 1–4 DONE; 5 PART-BUILT — the defer segment runs, the terminal transition does not use it yet (§3.81)** (WS-3, 2026-08-05; phases 2–4 landed 2026-09-02)

> **2026-09-02.** Phase 5's record shape is answered (§3.75), and the *execution* half is now
> built and measured: `WithDeferPhase` replays a workflow purely to run its outstanding defers,
> draining them on the suspension the replay ends in. What implementing it found first is that
> the mechanism §3.75 assumed — refusing the fresh call — destroys the cleanup rather than
> running it. See **§3.81** for both, and for what is left: the two-phase terminal transition,
> and the past-the-frontier replay.

> **2026-09-01, phases 2–3.** A WASM defer now has a body and it runs. §3.70 records the design
> and why the "one dispatch export" it was blocked on turned out to be unnecessary: the host
> instantiates a fresh module to run defers, so no export can reach a closure registered in the
> instance that is gone — the guest runs its own instead, at the point the entry point finishes.
> The first of the two intent properties above is now true for a workflow that returns, and the
> second is true *unconditionally* for those defers, because they run inside the live session
> with the whole workflow context available. What remains for phase 4 is the case where the guest
> never gets to run: cancellation, terminal failure, and the execution fence.
>
> **2026-09-02, phase 4 is DONE for the kill paths.** Shipped in #550 (the guest exports
> `__cleat_run_deferred`) and #551 (the host calls it). A workflow stopped by the execution
> fence, the instruction limit, or an unrecoverable runtime failure now runs its outstanding
> defers before the host reports the failure. Covered end to end through `Engine.Execute` by
> `engine/host_runs_defers_on_kill_test.go`, one test per kill mode.
>
> Three things about the implementation that were measured rather than assumed:
>
> | | |
> |---|---|
> | wall clock | always refreshed; `SetEpochDeadline` is relative, so without it the pass is interrupted immediately |
> | fuel | refreshed only when metering is on, and **required** there — without `SetFuel` the runner traps on its first instruction, `ran=0` |
> | memory ceiling | **deliberately not raised.** The export takes no arguments, so unlike an entry point it needs no scratch buffers, and an OOM-killed guest ran its defer with the ceiling untouched. Raising it would hand more memory to a guest that just proved it cannot be trusted with what it had. |
>
> The pass is bounded by `DefaultWasmtimeDeferBudget` (5s), tunable with
> `WithWasmtimeDeferBudget`. It is extra execution granted to a workflow the fence already
> stopped, so it is deliberately small next to the 30s instance timeout: the worst case a
> runaway workflow can hold a worker grows by about a sixth rather than doubling.
>
> **What holds the "no double cleanup" property up is not what it looks like.** Two layers
> prevent a workflow's cleanup running twice: the host only runs its pass on the kill paths,
> and `_cleatRunDeferred` is idempotent. Measured — adding the host pass to the
> `GuestReturnedError` branch leaves the control test PASSING, because idempotence absorbs it.
> So the gating is defence in depth and guest-side idempotence is load-bearing. Deleting the
> gating would fail no test; it is kept because relying on idempotence alone makes every
> future defer body's re-runnability a correctness requirement.
>
> **Still open: phase 5, and cancellation/terminal-failure.** This covers workflows the host
> KILLED mid-execution. A workflow cancelled or failed terminally between segments has no live
> instance to re-enter at all, so it needs the replay-based path, which is phase 5 and belongs
> with WS-2's crash-recovery record shape.

> **2026-09-02, phase 4 was only half wired and the other half was invisible.** #550 put
> `runGuestDefersAfterKill` in the **Go-on-wasmtime branch only**. #553, #557 and #558 then gave
> Rust, AssemblyScript and Java a `__cleat_run_deferred` export — and nothing called it, because
> every non-Go guest leaves through the *direct-export* path, which returned its error with no
> defer pass at all. **"The guest exports it" and "the host calls it" are two different facts**,
> and shipping the first while assuming the second is how a killed workflow's lock stays held
> with the export that would have released it sitting in the module.
>
> Measured before the fix, AssemblyScript `spin_forever` under a 2s fence: the workflow was
> killed, its defer did not run, and the engine's fallback pass logged
> `defer execution failed ... export=cleat_defer_defer-0 ... not found`. That message names an
> export naming convention **no guest in any language has ever had** (`grep -rn "cleat_defer_"`
> finds consumers and no producers), so the one signal an operator had said the cleanup failed
> for the wrong reason, about the wrong thing.
>
> **Java was measured separately rather than inferred**, exactly as the phase-3 note below asks:
> whether a second export can be called after a kill is a property of the guest's runtime, not of
> wasmtime, and a TeaVM module has a whole runtime — shadow stack, fiber system, thread-local
> globals — that an epoch interrupt stops mid-stride. It works: `defers_run=1` for both, with the
> kill error still returned. AssemblyScript under `--runtime stub` is the easy case and passing
> there is not evidence about Java.
>
> **Still not covered, and named here so it is not mistaken for done:** Python has no kill path
> at all (§3.73).
>
> **The misleading warning is fixed (2026-09-02).** `ErrExportNotFound` now distinguishes "this
> module has no such export" from "that export ran and failed", both producers wrap it
> (`engine/runtime.go`, `engine/backend_wasmtime.go`), and the legacy per-defer fallback logs the
> first at DEBUG. Matched with `errors.Is`, never by substring: matching the wording is the same
> mistake one layer up, and this repo has already had a check that matched an error message
> rather than the condition and reported a broken database as healthy.
>
> **The logger inconsistency is fixed (2026-09-02).** `runGuestDefersAfterKill` wrote to
> `slog.Default()` in all three of its branches — the success line, `could not be run`, and the
> refuel warning — so a worker with a configured handler saw *nothing at all* about the cleanup
> of a workflow it had just killed. That is the log an operator reads to answer "did the lock get
> released?", and it was the one going where they were not looking.
>
> The backend now has a `logger` field and a `log()` accessor mirroring `Engine.log()`, set with
> `WithWasmtimeLogger` and wired in `cmd/cleat-worker/main.go`. `WasmtimeOption` changed from
> `func(*wasmtimeLimits)` to `func(*wasmtimeConfig)` for it: **a logger is not a limit**, and
> putting one in a struct called `limits` is the kind of small dishonesty that later gets read as
> a fact about the type.
>
> **Why it stayed invisible so long is the part worth keeping:** `slog.Default()` still prints to
> stderr, so it looks correct in a terminal and vanishes only under a configured handler — which
> is exactly the case an operator runs and a developer does not. The regression test mutates the
> *`PerExecution` copy* rather than the constructor, because that copy is the one that executes
> workflows: dropping the logger there leaves the root backend looking right and silences every
> record from the path that matters.

> **2026-09-02, the wazero path had the same hole and a second one behind it — and phase 2 is
> now DONE.** `invokeDefersOnTrap` asked for `cleat_defer_<id>` too, so a guest trapped under
> `cleatctl replay`, `cleat run`, `cleat/wasmtest`, `cleat/cleattest` or `cleat/embedded` lost
> its cleanup. Measured on an AssemblyScript guest that traps with one defer outstanding: two
> warnings, zero cleanup.
>
> **Fixing that exposed phase 2 immediately, which is the part worth recording.** With the drain
> pointed at `deferRunnerExport` the body finally ran — and panicked on its first host call:
>
> ```
> interface conversion: interface is nil, not engine.HostHandler
>   engine.handlerFromContext  engine/imports.go:20
> ```
>
> exactly as the phase-2 note below predicts, with exactly the fix it names:
> `withHandler(context.Background(), session)`. The bare context stays — defers must run when
> `execCtx` is already cancelled, which is when they matter most — and it gains the session.
>
> **Two defects in series, and the outer one hid the inner one completely.** While the drain
> looked for an export no guest had, no body ever ran, so no body ever reached a host call, so
> the phase-2 panic had nothing to fire on. A defer that cannot call the host cannot release the
> lock it took, which is most of what a defer is for — and no test anywhere went red for it.
>
> `deferRunnerExport` moved out of `backend_wasmtime.go` for this. That file is `//go:build cgo`,
> so the wazero path **could not name the export it was supposed to call** — which is a large
> part of why it was calling the wrong one.
>
> The per-defer `cleat_defer_<id>` convention is **kept as a fallback**, reached only when a guest
> has no runner. No SDK emits it and the only producers in the tree are hand-written WAT
> fixtures, but its partial-failure semantics are load-bearing and pinned
> (`TestDeferBodyRunsOnceAfterATrap`): a defer that ran and *trapped* must not be retried on a
> fresh instance, because it is a destructor and half-applying a compensating action then
> applying it again is worse than not retrying.

> **2026-09-02, phase 4's fence case is buildable — measured, not argued.** A real Go SDK guest
> killed mid-loop by the fence can be re-entered, runs guest code, and its outstanding defer runs
> and reaches the host, with no production change required. §3.70's subsection "The fence case is
> reachable — both halves now measured" has the table, the control, and the mutations;
> `engine.TestAGoGuestSurvivesTheFence` pins it. This is a capability, not a design: the host
> still has to decide to make that call, and wants a named defer-runner export rather than the
> unrelated entry point the test borrows. **Traps, OOM and host-side timeouts remain unmeasured**
> and must not be assumed to behave the same way.

Written because §3.32 fenced the defer path and the fence made an uncomfortable question
visible: *bounded doing what, exactly?* The implementation turned out to be much further from
the intent than the fence discussion suggested, and the intent had never been written down.

**The intent, as stated by the author:** a defer in a durable execution system is a destructor.
Two properties, both load-bearing: it is **guaranteed to run**, and it has **access to the full
context of the workflow** so it can do the cleanup it exists to do. Releasing a lock,
compensating a saga step and notifying a service are all "cleanup" only if the cleanup code can
see what was acquired, what was done, and to whom.

#### What the implementation does instead

Three findings, each verified against the tree rather than inferred.

1. ~~**In the embedded (in-process Go) runner, defer closures never run.**~~ **Closed** —
   `deferFuncs` was given a reader on 2026-08-05, the same day this was written; the drain is at
   `cleat/embedded/runner.go:568` and the commit's own comment says "Until 2026-08-05 nothing
   did." Re-checked 2026-09-01 and left in place rather than deleted, because the "What to do in
   the meantime" note at the end of this section still asked for it.
   Re-derive: `grep -rn "deferFuncs" --include="*.go" .` — a reader outside `_test.go` means
   this is closed.
2. **In WASM, a defer cannot have context by construction.** `RunDefer` compiles the module and
   instantiates it *fresh*: new linear memory, so the workflow body never ran in it and nothing
   it captured exists; no `HostHandler` in ctx, so no durable calls, no lock release, no
   notification; and `nil` input.
3. **The four call sites disagree.** Only one invokes defers on the live instance with the
   session. `executeCompiled`'s non-suspend-error branch ran `invokeDefersOnTrap` *and*
   `runDefers` unconditionally — the comment said "fall back" but there was no conditional, so
   each defer body executed **twice**. The success path runs them from `cmd/cleat-worker`
   *after* `FinalizeWorkflowSegment`, by which point the instance is gone.

   **The doubling is fixed in §3.64** (2026-09-01), confirmed by running rather than by reading:
   two `defer execution failed` records for the same `defer_id`, each carrying the trap raised
   by the defer function itself. Line numbers in the original text have moved; the branch is
   the one guarded by `if len(session.deferrals) > 0` in `executeCompiled`. The rest of this
   finding — the four sites disagreeing about *when* and *with what context* — is the design
   below, and is untouched.

4. **A defer registered before a suspension was dropped permanently.** `DurableDefer`'s
   replay-match branch answered the guest with the recorded `DeferID` and never re-added it to
   `session.deferrals`, so every segment after the first lost it. **Fixed in §3.66**
   (2026-09-01), found while planning the implementation below and shipped ahead of it, because
   it is wrong under every row of the decision table. It is listed here because it changes what
   the design has to build: replay *does* reconstruct the deferral set now, which is the
   mechanism the design below depends on.

5. **There is no callback for any of this to invoke.** `cleat_defer_<id>` is a name the host
   constructs and no SDK exports. Measured 2026-09-01 with a real Go SDK fixture: a defer
   registers, and the invocation comes back `{"error":"unknown entry point"}` with a **nil** Go
   error, so nothing is logged and the host records success. **§3.70**, which is the prior
   question to everything below.

So today: "full context" holds on one error path, "guaranteed to run" holds nowhere, and on the
common paths a defer can only execute code that depends on nothing — and in WASM it executes
nothing at all, because there is nothing to execute.

#### The insight that makes this tractable

A fresh instance is **not** the problem. In a durable execution engine the workflow's state
*is* its event history; a resumed workflow always starts from a fresh instance and reconstructs
itself by replay. A defer running in a new instance is therefore ordinary, provided it runs in a
**replayed** instance rather than a virgin one.

And replay gives the hard part away for free: **replay re-runs the workflow body, so it
re-registers the defer closures on the way through.** `DurableDeferFunc(fn)` starts working not
because anything resurrects a closure, but because the closure is *rebuilt* — the same way every
other piece of guest state is.

The machinery is already there and already deterministic. `DurableDefer` is replay-matched
(`engine/durablecalls.go:368`): on replay it returns the **recorded** `DeferID` rather than
minting a new one, so IDs are stable across replays, and registration is a durable event
(`EventTypeDefer`) that survives a crash. `TestDurableDeferReplayMatch` and
`TestDurableDeferReplayPastEnd` already pin that behaviour. Nothing connects it to execution.

#### Proposed design: a defer is a replayed continuation, not a callback

Run defers as a **second execution phase of the same workflow**:

1. Instantiate fresh and **replay the recorded history** to the terminal step. Guest state is
   reconstructed exactly as it is for any resumption; defer closures re-register as a
   side effect; recorded durable calls return their recorded results and do not re-fire.
2. Invoke `cleat_defer_<id>` in **LIFO order**, with the live `execSession` in ctx. Host calls
   now work, because there is a session — the defer can release the lock it took.
3. Record the defer's own host calls as **new events after the workflow's terminal step**, in a
   distinguishable phase, so a retried defer replays deterministically like anything else.

This gets all three properties at once, and each from a mechanism that already exists: full
context from replay, host access from the session, and the execution fence for free, because
step 1–3 is an ordinary backend execution rather than a bespoke path.

**Guaranteed-to-run then becomes a durability question, not an execution one.** Registration is
already durable. If the defer phase is recorded as its own unit of work, a worker that dies
mid-defer leaves a resumable record and the reaper re-runs it — the same shape as §1.4's
crash-recovery and §3.20's force-resolve. That is the only way "guaranteed" can be true across a
`kill -9`: no in-process callback survives one, in any language.

#### Decisions this needs, with recommendations

These are the author's to make; the design changes shape depending on them.

| decision | options | recommendation |
|---|---|---|
| At-most-once or exactly-once? | best-effort, or retried until success | **Exactly-once**, retried. "Guaranteed" is the stated intent, and it is what makes defer worth more than a `finally` block. |
| Does a failing defer fail the workflow? | yes / no / record-and-continue | **Record and continue**, but surface it — a failed cleanup must be visible, which is what §3.32's logging fix started. |
| When do defers run? | on failure only / success too / cancellation and termination too | **All terminal transitions.** A destructor that skips the success path is not a destructor. |
| What may a defer body do? | anything / no new defers, no children, no continue-as-new | **Restricted**: no registering defers, no `continue_as_new`. Child workflows are arguable. |
| Replay cost | full replay per defer phase / reuse compaction | **Reuse compaction state** (`buildFullHistoryFromCompaction` already exists) — otherwise a long workflow pays its whole history again to release one lock. |
| Timeout | workflow budget / own budget | **Its own budget.** The workflow's remaining budget is often zero exactly when defers matter. |

#### Implementation plan — five phases (WS-3, written 2026-09-01)

**Decisions taken.** The table above is the author's to decide and the recommendations were
adopted as written, with one change of scope: exactly-once is *deferred*, not declined. Two of
the six were already settled by earlier work — "its own budget" shipped as
`WithDeferPassBudget` in §3.63, and "reuse compaction state" is what
`buildFullHistoryFromCompaction` already does.

| phase | delivers | status |
|---|---|---|
| 1 | A defer survives a suspension — replay re-registers it | ✅ §3.66, 2026-09-01 |
| 2 | A defer can make host calls: the session reaches the body | ✅ 2026-09-02 — `withHandler(Background, session)`; §3.70's ABI unblocked it and the wazero drain fix exposed it |
| 3 | Defers run on **all** terminal transitions, in the instance that registered them | ✅ 2026-09-02 for every transition that has a live instance; the rest **is** phase 5 |
| 4 | The body is restricted: no new defers, no `continue_as_new` | ✅ 2026-09-02, all five SDKs |
| 5 | Exactly-once: the defer phase as its own durable unit of work | **not scheduled — needs WS-2's record shape** |

**This table said "blocked, §3.70" for phases 2–4 until 2026-09-02, and the heading above said
"PHASES 1–4 DONE" at the same time.** Both were wrong, in opposite directions, for weeks. The
heading over-claimed phase 4, which had no implementation anywhere; the table under-claimed
phases 2 and 3, whose blocker had been removed by §3.70 and §3.73. A scan of either alone gives
a false answer, which is what this file's own rule about markers over stale bodies is for.

**And the shape shipped is not the shape this five-phase plan describes.** The plan below assumes
the host invokes a per-defer callback — an export named `cleat_defer_<id>`. What exists is the
*opposite*: the guest drains its own table inside the entry-point wrapper (§3.70, §3.73), and the
host has one whole-table export, `__cleat_run_deferred`, for guests it killed (phase 4 of §3.35,
#550/#553/#557/#558/#559/#560). **No guest in any language has ever exported `cleat_defer_<id>`**
— `grep -rn "cleat_defer_"` finds consumers and no producers. Read the phase notes below as the
reasoning that led here, not as a description of the code.

**What phase 3 does and does not cover, precisely.** Defers run on the success path, on the error
path, and on every kill the host performs — fence, instruction limit, memory ceiling, trap —
across wasmtime and wazero, in the instance that registered them. They do **not** run for a
workflow cancelled or failed terminally *between* segments, because there is no instance to run
them in. That case is not a gap in phase 3; it is the definition of phase 5.

**Phase 4's two restrictions were measured before they were built**, and both produced the same
failure: a workflow that reported SUCCESS while writing a durable record that could not be
honoured.

| a defer body that... | before, measured 2026-09-02 |
|---|---|
| registers another defer | host wrote `defer-3 "registered from inside a defer body"` into the history; the body never ran, because the table is drained before the first body starts. A completed workflow carrying a pending defer nothing could execute — §3.70's defect by a different road. |
| calls `continue_as_new` | host recorded a `continue_as_new` event at step 3 **and** the wrapper reported the already-decided result. One history, two contradictory terminal facts; the worker stores `done` and the continuation is silently never taken. |

Both are now refused **before the host call** in all five SDKs, which is the part that matters: a
guest-side refusal that still let `cleat_defer` reach the host would leave the durable event
behind, and the durable event is the defect. Pinned by `TestADeferBodyCannot*` (AssemblyScript,
hand-written guards) and `TestAGoDeferBodyCannot*` (Go, codegen-emitted guards) — different code,
same contract, so one passing says nothing about the other.

**Phase 2 — the session reaches the body.** `invokeDefersOnTrap` invokes the defer on the
still-live module with `context.Background()`, so `handlerFromContext`'s unchecked assertion
panics inside any host call the body makes; the panic is recovered by `CallExportWithSuspend`
and logged as a defer failure. The bare context is deliberate and must stay — defers have to run
when `execCtx` is already cancelled — so the fix is `withHandler(context.Background(), session)`,
which keeps the cancellation immunity and adds the handler. This is the cheapest of the three
properties to get, because on this one path the live instance and the live session both already
exist.

**Phase 3 — run them where they were registered.** This is the substance. On success the worker
runs defers *after* `FinalizeWorkflowSegment`, on a fresh instance, through a brand-new
`engine.NewEngine` (`cmd/cleat-worker/setup.go:435-487`), by which point there is no session and
no memory. The design's "instantiate fresh and replay" step is **already paid for on this path**:
when the workflow body returns, the instance that just ran it is still live and the session is
still installed, which is precisely the state step 2 wants. So the work is not to build a replay
phase — it is to invoke the defers before that instance is torn down, and to delete the worker's
post-finalization pass.

The obstacle is `WasmBackend.Execute` (`engine/backend.go:16-33`), which owns its store and
instance and destroys them before returning, so the engine cannot call a second export
afterwards. **Do not widen the interface for this.** The backend already holds the session — it
sets `b.handler = session` at `backend_wasmtime.go:412` — so it can ask the session for the
registered defers after the entry point returns and invoke `cleat_defer_<id>` itself, on the
same store, before teardown. No interface change, and the wazero path in `executeCompiled`
already has this shape via `invokeDefersOnTrap`; it is missing only the success path.

**Measure before building phase 3.** Whether a second export can be called on the same instance
after the entry point has returned, and separately after it has *trapped*, is a property of the
guest's runtime, not of wasmtime — a Go guest that unwound through `proc_exit` has no live
runtime left to call into. `invokeDefersOnTrap` doing this on wazero today is evidence it works
there, and is not evidence about wasmtime or about the success path. Establish it per backend
and per terminal transition before writing the wiring.

**Phase 5 is not scheduled, and the reason is a boundary rather than a cost.** "Guaranteed to
run" across a `kill -9` needs the defer phase to be its own durable, resumable unit with a
reaper — the same record shape as §1.4's crash recovery, which is **WS-2's**. Inventing a second
shape here is how two workstreams end up with two answers. Migration range 030–039 is reserved
for WS-3 and stays reserved for it. Phases 1–4 are worth shipping without phase 5: they take
defer from "runs somewhere, with no context, unless the workflow ever suspended" to "runs where
it was registered, with the session, on every terminal transition" — best-effort, which is what
the documentation can then honestly claim.

> **Answered 2026-09-02 — see §3.75 (WS-2).** The record shape this paragraph asks for does not
> exist, and the reason is that its premise has expired: every defer execution in the tree now
> runs *inside the claimed segment*, before `FinalizeWorkflowSegment`, so those paths already
> inherit §1.4's durability with no new record. What is left is the three sites that set a
> terminal status by direct `UPDATE` and so never build an instance to run defers in. The answer
> is a **two-phase terminal transition** — mark, run a defer segment, then finalize — whose only
> new durable state is a workflow-level marker swept by the existing reaper. §3.75 has the
> inventory, the ordering hazard that makes it urgent, and what to build.

#### Cost, and what it touches

The replay is the cost: reconstructing state means re-executing the workflow body. Compaction
bounds it, and the alternative — persisting guest memory — is far worse. Everything else is
wiring: `engine/executor.go` (phase), `engine/durablecalls.go` (step numbering for defer-phase
events), `cmd/cleat-worker/setup.go` (stop running defers post-finalization), and a migration if
the defer phase gets its own durable state. **Migration range 030–039 is unused and reserved for
WS-3.**

Cross-stream: the exactly-once half overlaps §1.4 (WS-2). Worth agreeing the record shape once
rather than inventing a second one.

#### What to do in the meantime

§3.32's fence is orthogonal and stands on its own: an unbounded defer should not hold a worker
slot under any semantics. It should land with **neutral framing** — it bounds today's
implementation and says nothing about what a defer may do — rather than describing "defers
cannot make host calls" as a contract, which is an accident of the current implementation and
is exactly what this design reverses.

The embedded runner's dropped closures (finding 1) should be fixed regardless of which way this
goes: today that API silently does nothing. **Done — see finding 1 above; it was already fixed
when this paragraph was written.** The doubled defer body (finding 3) is likewise orthogonal and
fixed in §3.64: running a destructor twice is wrong under every set of semantics in the decision
table above.

### 3.36 errcheck's 283 findings, triaged — 🔶 **1 real defect found, handed to WS-1** (WS-3, 2026-08-05)

The companion to §3.33. `PARALLEL-WORKSTREAMS.md` singles errcheck out as **"the class that
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

**Why errcheck still should not be enabled yet.** 152 `tx.Rollback` findings would have to be
suppressed first, and blanket-suppressing the single most common shape in the codebase to turn
on a linter is how a guard becomes decoration. The honest sequence is: exclude `tx.Rollback` by
rule (errcheck supports an exclude list), fix the ~30 write-and-lock findings, then enable and
see what is left. That is a session, and it is a session with a known payoff, which is more
than could be said before this table existed.

### 3.34 A concurrency key's TTL means three different things — ✅ **FIXED** (WS-1, 2026-08-05; found by WS-3)

Found by chasing an intermittent, and the intermittent is the least of it.

**Sub-second TTLs silently become zero on two of three dialects.** PostgreSQL
(`engine/db.go`) and SQL Server (`engine/mssql_signals_promises.go`) both build the expiry
from `int(ttl.Seconds())`, which truncates. Measured, not read:

```
ttl=1ns    -> interval "0 seconds"
ttl=500ms  -> interval "0 seconds"
ttl=999ms  -> interval "0 seconds"
ttl=1s     -> interval "1 seconds"
```

So `AcquireConcurrencyKey(ctx, key, wf, 500*time.Millisecond)` acquires a key that is already
expired, and the next caller takes it. For a mutual-exclusion primitive that is the failure
that matters: two workflows holding the same key at once, with nothing logged.

**And the three dialects disagree about whose clock decides:**

| dialect | expiry | clock |
|---|---|---|
| PostgreSQL | `now() + '<int seconds>'` | database |
| SQL Server | `int(ttl.Seconds())` | database |
| MySQL (`engine/mysql_ops.go`) | `time.Now().Add(ttl)` | **application** |

MySQL keeps sub-second precision, which is better, and computes on the host clock, which
makes it the one dialect where app/database clock skew changes whether a lock is held. Two
behaviours across three backends, in a locking primitive — the shape §1.1 and §2.60 both had.

**The intermittent that led here**, recorded because it has a name this time:
`TestAcquireConcurrencyKey_Expired` failed once in a full-suite run and passed in the three
that followed, and passes individually on all three dialects. It acquires with a 1 ns TTL —
which is 0 seconds on two dialects and 1 ns on the third — then sleeps 10 ms and asserts the
key can be re-acquired. It is asserting behaviour that differs per backend, using a sleep, so
it is the same "races for a precondition instead of stating it" shape as the two gates fixed
in #329. Fixing the truncation is what makes the test statable.

**Not fixed here.** `engine/db.go`, `engine/mysql_ops.go` and `engine/mssql_signals_promises.go`
are WS-1's, and the fix is a decision about the primitive rather than a patch: whether TTLs are
sub-second at all, and whether expiry belongs on the database clock (defensible, and what two
dialects do) or the application's. Whichever way it goes, all three should agree.

#### Resolution — the TTL is exactly what was asked for, on the database's clock

Both halves of WS-3's question, decided the same way for the same reason.

**Sub-second TTLs are real, and are not rounded in either direction.** The guest API is
specified in milliseconds — `engine/locking.go` passes `time.Duration(ttlMs)*time.Millisecond`
straight from the WASM caller — so truncating to whole seconds contradicts the contract callers
are written against. That settles WS-3's "whether TTLs are sub-second at all": they already
are, at the only layer a user sees.

**The database's clock owns expiry**, on all three. Every predicate that reads `expires_at`
already compares it against the database clock (`expires_at < now()`, `> SYSUTCDATETIME()`,
`<= NOW(6)`). MySQL computed the value on the application's and tested it against the
database's, which is the skew WS-3 identified; workers on different hosts also have to agree
about whether a lock is held.

  postgres  now() + make_interval(secs => $3)                 -- fractional seconds
  mysql     DATE_ADD(NOW(6), INTERVAL ? MICROSECOND)
  mssql     DATEADD(MICROSECOND, @us, DATEADD(SECOND, @s, …))  -- split so DATEADD's
                                                                 int argument cannot
                                                                 overflow on a long TTL

**Demonstrated, not reasoned about.** With the fix reverted, a 500 ms lock is stored *in the
past* — `a 500ms lock was stored already expired (-6.397ms remaining)` on PostgreSQL,
`-8.18ms` on SQL Server — and `TestConcurrencyKeyExcludesWhileHeld` shows the consequence
directly: a second workflow acquires a key the first is holding.

**No sleeps.** WS-3 noted that the intermittent which led them here asserts per-backend
behaviour through a 10 ms sleep. These tests read the stored expiry back and do arithmetic
against the database's own clock, and check exclusion by having a *second* workflow contend —
neither needs a race to be observable.

### 3.39 Re-acquiring a concurrency key you already hold answers differently per dialect — ✅ **FIXED** (2026-08-31)

> **Renumbered from §3.35 on 2026-08-06.** `§3.35` had been allocated twice — to this item and
> to WS-3's defer design, which is the one that keeps the number because two other passages
> cite it by section. WS-1 had already used §3.37 and §3.38, so this moved to §3.39. Anything
> written before 2026-08-06 that cites "§3.35" for concurrency-key re-entrancy means this
> section.

Found while writing §3.34's exclusion test, by contending a key against itself and getting
disagreement:

| dialect | `AcquireConcurrencyKey(key, wf)` when *wf itself* already holds `key` |
|---|---|
| MySQL | **true** — `return ownerID == workflowID` (`engine/mysql_ops.go`) |
| PostgreSQL | **false** — `ON CONFLICT DO NOTHING` returns no rows (`engine/db.go`) |
| SQL Server | **false** — the `WHERE NOT EXISTS` guard matches, so nothing is inserted |

So the same primitive is re-entrant on one backend and not on the other two, and neither
behaviour is written down anywhere. It matters in both directions: a workflow that re-acquires
its own lock is told it failed on two dialects (and may block itself or leak the lock until the
TTL runs out), while on the third it succeeds and a matching release count becomes the
caller's problem.

Not fixed here for the reason §3.34 was handed over in the first place — it is a decision about
what the primitive means, not a patch. The choice is between "re-entrant, and document it" and
"never re-entrant, and return false consistently". §3.34's test deliberately contends with a
*second* workflow so that it asks about mutual exclusion rather than about this.

#### Resolution — never re-entrant, and the release API is why

Decided and fixed 2026-08-31: **never re-entrant, on every dialect.** MySQL changed to match the
other two.

Re-measured before deciding, because the table above was 25 days old. It was exactly right:

    postgres  first=true  self-reacquire=false  other-workflow=false
    mysql     first=true  self-reacquire=true   other-workflow=false
    mssql     first=true  self-reacquire=false  other-workflow=false

Mutual exclusion held everywhere, so the divergence was only ever about self-re-entrancy.

**The deciding argument is not the majority vote.** `ReleaseConcurrencyKey(ctx, key)` takes only
the key and issues an unconditional `DELETE` — there is no hold count anywhere in the system, and
the delete is not even scoped to the owning workflow. Under MySQL's old answer,
`acquire(k); acquire(k); release(k)` left the key **free while the workflow still believed it held
it**: the exact failure a mutual-exclusion primitive exists to prevent. Making the other two
dialects re-entrant would first have required adding a hold count. Making MySQL non-re-entrant
required deleting a read-back.

`MySQLStore.AcquireConcurrencyKey` now reports whether *this call* inserted the row —
`INSERT IGNORE`'s rows-affected — rather than inserting, reading the owner back and comparing it
to `workflowID`.

**Contract:** `acquire` returns false whenever the key is not yours to take *now*, including when
you already hold it. One release per successful acquire.

Callers are unaffected. `engine/scope.go` releases the old key before acquiring, so re-entering
the same virtual-object scope still works; `engine/locking.go` passes the answer straight to the
guest, which is where the divergence was visible.

**A mock that encoded an impossible database.** `TestMySQLStore_AcquireConcurrencyKey_AlreadyHeld`
configured the INSERT as affecting 1 row and then relied on the follow-up SELECT finding nothing —
a row inserted and simultaneously absent. It passed for a reason that corresponded to no real
state. Corrected to rows-affected 0, which is what a held key actually produces.

`_VerifyError` covered the read-back that no longer exists, but its error path moved rather than
vanished: `RowsAffected` can fail independently of `Exec`. Replaced by
`_RowsAffectedError`, which pins that an unreadable outcome is an error rather than a silent
`false` — reporting false there would invite a second workflow to take a key the first may hold.
This needed one additive field on the shared mock (`affectedErr` in `db_methods_test.go`).

Re-derive, all three dialects:

    go test ./engine/ -run TestAcquireConcurrencyKeyIsNeverReentrant -v -p 1

Mutation-tested three ways: restoring `return ownerID == workflowID` fails **only** the mysql
subtest, which is the divergence itself; returning false unconditionally is caught by the
fresh-key assertion; swallowing the rows-affected error fails `_RowsAffectedError`.

### 3.40 The crash harness migrated a database it never reads — ✅ **FIXED** (2026-08-31)

`tests/crash` starts a real `cleat-worker` subprocess on the shipped two-DSN configuration.
`startWorker` passed `appDSN(t)` as `--db` and **`ownerDSN()`** as `--migrate-db`. Those are two
different databases:

- `appDSN(t)` → `cleat_crash`, the database this suite created for itself so that
  `engine/testutil`'s unqualified `DELETE FROM` could not reach a live worker's rows.
- `ownerDSN()` → the *base* database, `cleat`. The shared one.

`cmd/cleat-worker` opens `--migrate-db` as a separate connection and runs both
`migration.Runner` and `plugin.RunMigrations` against it (`main.go:556`). So every worker this
suite started migrated the shared database and migrated **nothing** in the one every assertion
here reads — and the isolation `ensureCrashDatabase` exists to provide was given up on every
worker start.

**Measured 2026-08-31, unfixed harness, after one worker start:**

| database | `schema_migrations` | `plugin_migrations` | public tables |
|---|---|---|---|
| `cleat` (`--migrate-db`) | present, 14 rows | present | 16 |
| `cleat_crash` (`--db`) | absent | absent | 14 |

Re-derive by pointing `CLEAT_CRASH_DB` at a scratch instance, dropping both databases, running
one test, and diffing `information_schema.tables` between them.

**Nothing was failing**, because the 14 content tables agreed. They agreed by luck: `cleat_crash`
was built by `ownerDB`'s own `ReadDir`-and-`Exec` loop over `migrations/postgres/*.sql`, a second
implementation of "apply the shipped migrations" with no `schema_migrations` bookkeeping and its
own statement splitting. That is the §1.9 / §2.60b mechanism — `engine/testutil` has a guard test
against reintroducing a hand-written schema, and this suite had quietly kept one. Had the two
routes ever disagreed, the symptom would not have been "the worker cannot migrate"; it would have
been an unrelated assertion in this suite failing against a schema nobody had thought about.

**Fixed both halves.** `--migrate-db` now names the database the worker serves from, and
`ownerDB` applies migrations through `migration.Runner` — the call `cmd/cleat-worker` makes at
boot — instead of its own loop.

Re-derive:

    CLEAT_CRASH_DB=... go test ./tests/crash/ -run TestWorkerMigratesTheDatabaseItServes -v

**The regression test is asserted on `plugin_migrations`, not on the flags.** The flag is not the
property; what matters is that the migration the worker performs at boot lands where the worker
reads. `schema_migrations` cannot witness that — `ownerDB` creates it — but `plugin_migrations`
can, because only a worker ever creates it.

**It runs in a database of its own, `cleat_crash_migrate_target`, which it drops first.** Two
mistakes made while writing it, both worth recording because both produced a *green* first
attempt at some point:

1. Sharing `cleat_crash`, it passed alone and failed in the suite: the four tests above it had
   already started workers, so `plugin_migrations` existed and there was nothing left to observe.
2. Without the drop, it passed on a clean database and then failed on every subsequent run of the
   same checkout, for the same reason.

A witness that is created once and then stays created cannot be reused; the test now guarantees
its own precondition rather than assuming it.

Mutation-tested two ways. Restoring `--migrate-db ownerDSN()` fails it with the worker's log
attached, showing a *healthy* worker — RLS enforced, wasmtime registered — that simply migrated
elsewhere, which is the defect and not a boot failure. Removing the drop makes the second
consecutive run trip the separate "cannot tell whether the worker created it" guard, so the
precondition check is load-bearing rather than decorative.

**Not done here:** `tests/crash` still carries `ensureDatabaseNamed` and `swapDatabase`, which
duplicate `engine/testutil`'s `ensureDatabase` and `swapDatabaseName` (§2.60d part 2). The
duplication is real but small, and folding it in would mean exporting two helpers and adding a
dependency to a suite that deliberately keeps few — a separate change, and not a prerequisite for
anything.

### 3.41 Status-marker audit — ✅ **DONE** (2026-08-31)

Every `🔴` and `🔶` heading checked against the code rather than read. Twelve items; **four
were wrong**, and the failure was not random — in each case the marker was accurate when
written and was overtaken by work done somewhere else, which is precisely the case nobody is
watching for.

Method:

    awk '/^### /{print NR": "$0}' IMPROVEMENT-PLAN.md | grep -E "🔴|🔶"

then, for each, extract the sentence stating what remains and run a command against the tree
that would falsify it.

| item | marker said | measured | verdict |
|---|---|---|---|
| 2.43 | 🔴 open | `runVetAS` runs `npx asc … --noEmit` and propagates the exit status | **wrong** — preserved original entry kept a live `###` heading below the ✅ that superseded it |
| 2.71 | residual: test schema lacks `workflow_routing`/`workflow_tags` | `engine/testutil` applies the shipped migrations; both tables and both predicates ship | **wrong** — closed by Stream A |
| 3.30 | wazero is "the CGO-less fallback and nothing else" | `engine/backend_wazero.go` deleted in #459; `needsWazeroRuntime` gone | **wrong** — inverted, a CGO-less worker now exits |
| 3.31 | defers "fenced since #338" | `RunDefer` still falls through to unfenced wazero when `backendForWasm` returns nil | **wrong** — half true |
| 1.7 | 🔶, ~89 unaudited `MySQLStore` `s.tenantID` sites | still unaudited | correct |
| 2.35 | 🔶, `ErrorCode` has no path into history | `EventRecord.ErrNonRetryable` is still a `bool`; no code field | correct |
| 2.40 | 🔶, four linters disabled | `errcheck`, `unused`, `gocyclo`, `gosec` still under `disable:` | correct |
| 3.12 | 🔶, namespace still shared | `LoadWASM`/`GetWASMLength`/`LoadDAGSpec`/`LoadWorkflowConfig` still `(name, version)` | correct |
| 3.15 | 🔶, no writer for `allowed_signals` | `GetAllowedSignalCallers` is the only interface method; `config.go` says the same | correct |
| 3.33, 3.36 | linter triage counts | not re-derivable — `golangci-lint` is not installed | **unverified**, see below |
| 3.38 | 🔶 observed, not reproduced | a dated record of a one-off; nothing to check | correct |

**§2.43 is the one worth learning from.** The fix *was* recorded — as a second `### 2.43`
section, ✅, immediately above the original. The original was kept as history under a
`#### Original entry` heading, but itself stayed a `###` carrying `🔴`. So the repo's own
way of finding open work returned exactly one red item, and it was an item that had been
fixed. Preserved history now sits at `#####` with no status marker.

**§3.33 and §3.36 both claim exactly "283 findings"**, for `gosec` and `errcheck`
respectively, while `.golangci.yml`'s measured table gives errcheck 307/280/283/878 and gosec
193/283/268/659 across four different columns. At least one heading is citing a number without
saying which measurement produced it, which is the thing CLAUDE.md's "any number carries a date
and the command that re-derives it" exists to prevent. **Not corrected by guessing** —
`golangci-lint` is not installed here, so re-deriving means fetching the pinned version first.
Flagged, deliberately, rather than silently rounded into agreement.

> **Closed 2026-08-31 — the pinned version was fetched and all four re-measured.** See §3.42.
> Both headings are stale rather than merely ambiguous: `errcheck` is 1028 and `gosec` is 693
> today. Neither heading number is worth patching in place, because the count is the wrong thing
> to lead with — §3.42 records why.

### 3.42 The four disabled linters, re-measured — ✅ **MEASURED, none enabled** (2026-08-31)

`golangci-lint@v1.64.7` — the version `ci.yml` pins — installed and run per module, with the
repo's own config and both report caps off.

| linter | 2026-08-08 (incl. tests) | **2026-08-31** | delta |
|---|---|---|---|
| errcheck | 878 | **1028** | +150 |
| gosec | 659 | **693** | +34 |
| gocyclo | 44 | **44** | 0 |
| unused | 55 | **66** | +11 |

`errcheck` grew by 150 in 23 days — the existing note's point with a bigger number: a disabled
linter is not a backlog being worked off, it is one being added to.

**The totals are the wrong number to decide on, and every previous entry recorded only totals.**
Split by test-vs-production, then by the single dominant idiom, and both of the big two collapse:

- **errcheck: 1028 → 345 production → 150.** 158 of the 345 (46%) are `tx.Rollback`, the
  deferred-rollback idiom that returns `ErrTxDone` after a successful commit; another 37 are
  `w.Write` / `json.Encoder.Encode` writing an HTTP response.
- **gosec: 693 → 272 production → 39.** 233 of the 272 (86%) are **G115**, integer overflow
  conversion.

150 and 39 are reviewable; 1028 and 693 are not, and the whole difference is in how the number
was cut.

**G115 must not be swept, and that was already decided.** CLAUDE.md: *"Four real defects have
come out of the ABI layer's integer-conversion sites and none of them was an overflow — in every
case the value meant the wrong thing on one side of the boundary, which a property test over that
boundary would find faster than reading the remaining sites."* Those property tests landed in
#485. So gosec's real backlog is 39, and enabling it would mean *excluding* G115, not fixing it.

**Why none was enabled here.** `gocyclo` is the only one small enough to sweep at 44 and the only
one that did not move — but its distribution runs 34 to 172, with three functions over 160, so a
`min-complexity` ratchet set where the tree is green today would sit at 173 and gate essentially
nothing. `unused` stays off for the reason already recorded: `scripts/check-test-only-code.sh`
runs the same U1000 analysis against its own baseline, and two mechanisms with two baselines is
the shape that let §2.72's routing tables drift.

**The one defect-shaped thing found on the way**, and the argument for the errcheck residue being
worth reading rather than excluded wholesale: `engine/db.go:1151` has two adjacent best-effort
cleanups after a commit, and treats them differently —

```go
// Best-effort cleanup.
s.ClearStickyWorker(context.Background(), workflowID)                       // error dropped
if err := s.ReleaseWorkflowConcurrencyKeys(...); err != nil {
    s.log().WarnContext(..., "release concurrency keys failed", ...)        // error logged
}
```

26 production sites across `db.go`, `store_lifecycle.go`, `mysql_lifecycle.go`,
`mssql_lifecycle.go` and `mssql_operations.go` drop one of these two. Whether best-effort cleanup
should log is a judgement, but doing it *both ways within two lines* is not one, and it is
exactly the §2.60d shape — a cleanup whose failure is invisible. Recorded, not fixed: it is a
behaviour change across three dialects and belongs in its own PR.

> **Fixed in §3.43** — and the "26" above is itself an example of this section's own thesis about
> how a number is cut. 26 is what *errcheck* sees, because errcheck cannot see `_ = f()`. Counted
> directly, it is **38 dropped calls across 20 sites**: 14 bare + 6 `_ =` for `ClearStickyWorker`,
> 12 bare + 6 `_ =` for `ReleaseWorkflowConcurrencyKeys`. The 12 the linter missed were the ones
> someone had already noticed enough to silence.

Re-derive (note the zsh trap — `for m in $mods` does not word-split, which reproduces the "tidy
table of four zeroes" this file already warns about, from a different cause):

    go install github.com/golangci/golangci-lint/cmd/golangci-lint@v1.64.7
    find . -name go.mod -not -path './node_modules/*' -not -path './.claude/*' \
      -not -path './benchmarks/comparative/*' | sed 's|/go\.mod$||; s|^\./||; s|^$|.|' | sort > mods
    while IFS= read -r m; do
      (cd "$m" && golangci-lint run --timeout=15m -c /path/to/<linter>.yml ./...)
    done < mods | grep -c '(<linter>)'

where `<linter>.yml` sets `linters: {disable-all: true, enable: [<linter>]}` and
`issues: {max-issues-per-linter: 0, max-same-issues: 0}`.

### 3.43 Post-commit cleanup dropped its errors at 38 of 40 calls — ✅ **FIXED** (2026-08-31)

The item §3.42 recorded and deferred. `ClearStickyWorker` and `ReleaseWorkflowConcurrencyKeys`
are called as an ordered pair after every commit that takes a workflow out of the runnable set —
completion, failure, termination, continue-as-new, and the three admin actions. 20 sites across
all three dialects, and the copies had drifted into three treatments that did not agree with each
other, or with themselves:

| | bare | `_ =` | logged |
|---|---|---|---|
| `ClearStickyWorker` | 14 | 6 | 0 |
| `ReleaseWorkflowConcurrencyKeys` | 12 | 6 | 2 |

**What the dropped error costs.** A failed `ReleaseWorkflowConcurrencyKeys` leaves rows in
`concurrency_keys` owned by a workflow that has already finished, and each row holds a slot that
live workflows queue behind. It is *not* a permanent leak — `expires_at` is `NOT NULL`
(`migrations/postgres/001_schema.sql:350`) and the worker's reaper deletes expired rows
(`cmd/cleat-worker/setup.go:2001`) — so the stall clears itself at the key's TTL. It clears
itself **silently**, which is the defect: an operator looking at workflows blocked behind a key
whose holder completed an hour ago had nothing in the log at 18 of the 20 sites.

**A mechanism, not a sweep** — CLAUDE.md's question, and here the answer was clearly the former,
since all 20 sites were the same ordered pair with the same argument and the same
`context.Background()`. One unexported helper, `releaseWorkflowResources` in
`engine/workflow_cleanup.go`, takes a two-method interface so a single copy serves
`PostgresStore`, `MySQLStore` and `MSSQLStore`. All 20 sites became one line.

**Guarded, so the 21st site cannot be another copy.** `engine/workflow_cleanup_guard_test.go`
walks the package AST and fails if any non-test file outside an allowlist calls either method
directly (`sharded_store.go` is allowed: its two methods route to a shard and *return* the error,
which is the opposite of dropping it). A second test fails if the helper has no callers at all,
because an AST guard's characteristic failure mode is passing while measuring nothing.

Both were mutation-tested: reverting one site to a bare pair produced
`store_lifecycle.go:379 calls ClearStickyWorker` / `:380 calls ReleaseWorkflowConcurrencyKeys`,
and rewriting all 20 back to `_ =` produced the "no callers" failure — each for its own reason,
not a shared one.

Verified against all three dialects on WS-3 (Postgres 5434, MySQL 3308, SQL Server 1435):
`go test ./engine/ -p 1 -count=1 -v` → **4131 run, 4127 passed, 4 skipped, 0 failed, 69s wall**.
The four skips are all `componentize-py` / `wasm-tools` not being installed. Per-dialect subtest
counts were 194 / 194 / 195, so no dialect silently sat out; the 69s confirms it against
CLAUDE.md's ~20s-means-Postgres-only rule.

> The first run of that suite failed 6 MySQL tests with
> `Error 1061 (42000): Duplicate key name 'idx_instances_ready'`. That is the stale-local-schema
> trap CLAUDE.md warns about, not a regression —
> `docker exec cleat-ws3-mysql mysql -uroot -pcleat -e "DROP DATABASE cleat; CREATE DATABASE cleat;"`
> and it went green. Recorded because the failure looks exactly like broken `develop`.

Re-derive the site count:

    grep -rn "releaseWorkflowResources(s.log()" --include="*.go" engine/ | grep -v _test.go | wc -l

### 3.44 Child-workflow checksums were chained off an RLS-blocked read — ✅ **FIXED** (2026-08-31)

Found by reading errcheck's `engine` residue after §3.42 made it legible. Of 283 findings, 108 are
in tests and 156 of the remaining 175 are the deferred `tx.Rollback` idiom, leaving **19** worth
reading. Three of those 19 were the same defect in three dialects.

`StartChildWorkflowAtomic` chains its event's checksum onto the previous step's. Every other
statement in it runs on the open `tx`; that one read ran on `s.db` — the raw pool, no RLS context —
and discarded its error:

```go
var prevCS string
if event.Step > 1 {
    s.db.QueryRowContext(ctx,
        `SELECT COALESCE(checksum, '') FROM event_history WHERE workflow_id = $1 AND step = $2`,
        parentID, event.Step-1).Scan(&prevCS)      // error dropped
}
checksum := computeEventChecksum(event, prevCS)
```

`event_history` carries `FORCE ROW LEVEL SECURITY` with `tenant_id = cleat.assert_tenant_set()`,
which raises when `cleat.tenant_id` is unset. Measured against a real `cleat_rls_test_role`
connection:

    pq: cleat.tenant_id is not set -- tenant context required for RLS-scoped query (P0001)

Error dropped ⇒ `prevCS` stays `""` ⇒ the event is checksummed as if it had no predecessor.

**The consequence is not the failed read.** `VerifyWorkflowEvents` recomputes the chain and
compares, so an untampered history fails verification with `checksum mismatch`. The integrity
feature reports tamper evidence against a row cleat wrote itself.

**Why two layers of existing coverage both missed it:**

- `TestStartChildWorkflowAtomicUnderRLS` drives this exact function through this exact
  non-superuser connection — with `Step: 1`. The bad read sits behind `if event.Step > 1`, so the
  one test aimed at this function under RLS skips the only statement in it that is not RLS-safe.
- Every other test connects as a superuser, which PostgreSQL exempts from RLS entirely.

Catching it needs both halves at once: a non-superuser role **and** a step greater than 1.

> **A trap for anyone re-deriving this.** The raise only happens when a candidate row exists —
> PostgreSQL evaluates the policy per row, so against an empty `event_history` the same read
> returns plain `sql.ErrNoRows` and never calls `assert_tenant_set`. My first probe forgot to seed
> a row and "proved" the read was harmless. In production a step-1 row is always present when
> `Step > 1`, which is exactly the case that raises.

**The fix was already written.** All three dialects have `previousStoredChecksum(ctx, tx, ...)` —
tx-scoped, tenant-qualified, and explicit about `ErrNoRows` — and all three copies ignored it. The
three sites now call it. That also fixes a silent semantic divergence: the helper reads the
immediately preceding *row* (`step < N ORDER BY step DESC LIMIT 1`), matching what
`VerifyWorkflowEvents` expects, while the hand-rolled copies read `step = N-1` exactly and
disagreed whenever the history had a gap.

Third instance today of one shape: an abstraction exists, and copies of it drifted (see §3.43).

Verified: `engine/store_children_checksum_rls_test.go` fails against the unfixed code with
`stored == computeEventChecksum(event, "")` and the exact chained value it should have had, and
passes after. Full suite all three dialects, `go test ./engine/ -p 1 -count=1` → **ok, 71s**.
errcheck's non-Rollback production residue in `engine`: **19 → 16**.

### 3.45 A guest-supplied string chose the execution runtime — ✅ **FIXED** (2026-09-01)

`wasm.DetectLanguage` returns the `Language` field of the guest's own `cleat.metadata` custom
section **verbatim, with no validation** against `WasmtimeLanguages` (`wasm/metadata.go:277`).
`Engine.backendForWasm` looks that string up in a 5-entry map and returns nil on a miss. So the
string that selects the execution path is supplied by whoever built the module.

Measured against an engine registered exactly as `cmd/cleat-worker` registers one
(`WithBackends(WasmtimeLanguages, ...)`, nil `Runtime`):

| declared language | resolves to a backend |
|---|---|
| `go`, `python`, `rust`, `java`, `assemblyscript` | yes |
| `cobol`, `tinygo` | **no** |
| `GO` | **no** — the lookup is exact, so case alone is enough |

That last row matters: this is a plausible toolchain slip, not only an adversarial input.

**And with no backend, the three call sites did three different things** — all from the same
`if backend != nil` shape:

- **`RunDefer`** created a wazero `Runtime` on demand and ran the guest on it. CLAUDE.md records
  that **wazero cannot be fenced for a compute-bound guest** — measured three ways, all failing.
  So a guest-chosen string selected an unstoppable runtime. Confirmed by mutation: with the fix
  reverted, `RunDefer` on a `cobol` module returns `host: export "defer-1" not found`, which is
  the wazero path reporting after it compiled and instantiated the module.
- **`Replay`** dereferenced `e.rt`, which both worker constructions set to nil, and **panicked**.
- **`Execute`** returned a clean error, having been given a nil check the other two never got.

**Three comments asserted this could not happen, and two were wrong:**

- `engine/executor.go` — "the CGO-less build, where wazero is the only runtime there is." A
  CGO-less worker exits at startup, so it never reaches that line; what did reach it was a **CGO**
  build.
- `cmd/cleat-worker/setup.go` — "`backendForWasm` lookup always resolves here — there is no
  runtime-less fallback left to reach."
- `engine/engine.go`'s `WithBackends` was the accurate one: "returns nil when it is absent, **which
  sends the workflow to the fallback runtime**."

**Fix.** One `Engine.resolveBackend` replacing three copies of the nil check, distinguishing the
two cases a bare nil conflated:

- `(nil, nil)` — the engine registers **no** backends, so its wazero `Runtime` is the intended
  executor. That is `cmd/cleatctl replay|debug`, `cmd/cleat run_embedded` and `cmd/cleat-bench`,
  and they keep working unchanged.
- `(nil, err)` — the engine **does** route but has no backend for this language. Fail closed.

`tiers.yaml` grants no language outside `WasmtimeLanguages` (tier 1 `[go, python]`, tier 2 adds
rust, java, assemblyscript), so failing closed rejects only what was never claimed.

**Behaviour change to disclose.** `cleat/wasmtest` registers backends *and* holds a `Runtime`, so
under CGO a module declaring an unsupported language now errors instead of silently running on
wazero. Without CGO `backendOptions()` returns nil, leaving no backends, so that path still falls
back as before.

Guarded by `engine/backend_fence_routing_test.go`, mutation-tested three ways, each failing for
its own reason: disabling fail-closed (the `export not found` above); breaking routing for real
languages (the control catches it, and asserts the backend was actually *called*, not merely that
no error came back); and failing closed with an empty registry (the CLI-path guard fires).

Verified: `go test ./engine/ ./cmd/... -p 1 -count=1` against all three dialects → **all ok**,
engine 70s.

### 3.46 A dropped Unmarshal turned "unreadable" into "you declared nothing" — ✅ **FIXED** (2026-09-01)

Second pass through errcheck's `engine` residue (16 left after §3.44 took three).

`cleat_call_retry` takes the workflow author's non-retryable error patterns as a JSON array.
`freshCallWithRetry` parsed it and dropped the error, so on malformed JSON the slice stayed nil —
and a nil slice is indistinguishable from "the author declared no non-retryable errors".
`isDefinitelyNonRetryable` then matches nothing and every failure becomes retryable, so a call
explicitly marked *do not retry* is issued `maxAttempts` times. For the non-idempotent operations
that declaration exists to protect, that is a duplicate side effect.

Measured with a counting caller and a plain error whose message matches the pattern:

| non-retryable list | calls issued |
|---|---|
| `["INSUFFICIENT_FUNDS"]` | 1 |
| `INSUFFICIENT_FUNDS` (not an array) | **5** |

> **The first version of that test proved nothing, and the reason is worth keeping.** It used the
> existing `nonRetryableErr` fixture, which implements `Retryable() bool { return false }`.
> `isDefinitelyNonRetryable` checks that interface *first* and short-circuits before ever reaching
> the pattern list, so the malformed case stopped after 1 call and the test went red only on the
> return-value assertion. The count assertion — the one that demonstrates the defect — was passing
> for a reason unrelated to the bug. Fixed by using a plain `errors.New` whose message matches, so
> classification has to go through the list. This is CLAUDE.md's "check that it went red *for the
> reason you expect*" catching a test that was half-measuring.

The argument crosses the ABI from five language SDKs, the layer CLAUDE.md records as the source of
four real defects, "in every case the value meant the wrong thing on one side of the boundary". An
SDK emitting a bare comma-separated string lands exactly here.

Fixed by refusing with `badParamDurableCall` — the §2.10 convention for this host-function family,
which encodes the refusal in the layout the guest adapter actually decodes. Failing closed is the
safe direction: *"I could not read your safety declaration"* must not be treated as *"you made no
safety declaration"*. An empty string is still a valid "no patterns" and keeps working; that is
its own subtest.

**Left for a follow-up, with a count errcheck cannot produce.** The same dropped-`Unmarshal`-means-
empty-value shape covers `plugin_deps` on the workflow-def read paths, where corrupt JSON reads as
*this workflow has no plugin dependencies*. errcheck reports **5** such sites
(`mysql_ops.go` ×2, `mssql_deployment.go` ×2, `versioned_loader.go`) — but `store_deployment.go`
does it twice more with `_ =`, which errcheck cannot see. **7 sites, not 5**, the same blind spot
recorded in §3.43.

Verified: `go test ./engine/ -p 1 -count=1` against all three dialects → **ok, 74s**.

### 3.47 Audit: every Postgres raw-pool read against an RLS table — ✅ **AUDITED + 2 FIXED** (2026-09-01)

§3.44 fixed one `s.db` read inside a function holding a `tx`. This is the sweep for the rest of
that shape, and **most of the answer is a negative result** — recorded so nobody re-derives it.

**Method.** RLS is Postgres-only, so the 171 `s.db.Query|Exec` sites across `engine/` reduce to the
7 Postgres files (18 sites). Each was checked against the 8 tables carrying
`FORCE ROW LEVEL SECURITY`:

    grep "FORCE ROW LEVEL SECURITY" migrations/postgres/001_schema.sql \
      | grep "^ALTER TABLE" | awk '{print $3}' | sort -u
    # event_history, workflow_defs, workflow_instances, workflow_promises,
    # workflow_routing, workflow_schedules, workflow_signals, workflow_tags

**Result — 16 of 18 are fine, for three distinct reasons:**

| sites | why safe |
|---|---|
| `db.go` ×4 | `workflow_memory_stats` / `workflow_memory_samples` — not RLS-forced |
| `store_lifecycle.go` ×2 | `idempotency_keys` — not RLS-forced, and carries an explicit `tenant_id = $2` |
| `store_lifecycle.go` ×3, `store_deployment.go` | `admin.claim_workflows()`, `admin.get_due_schedules()`, `admin.tenant_api_keys`, a catalog probe — admin schema, errors checked |
| `store_children.go` ×2 | cross-schema child workflows; one is explicitly the documented "no tenant to establish" path |
| `event_stream.go` ×2, `store_event_stream.go` | errors checked |
| `db_metrics.go` `EstimateEventHistorySize` | `pg_total_relation_size` reads catalog metadata, touches no rows |

**So the §3.44 class — raw pool *and* a dropped error, giving a silent wrong answer — has no other
instances.** That is the useful finding.

**2 real bugs, both in `db_metrics.go`.** `CountStalledWorkflows` (workflow_instances) and
`CountEventHistoryTotal` (event_history) read RLS-forced tables on the raw pool. Measured against a
`cleat_rls_test_role` connection with one genuinely stalled workflow seeded:

    ground truth (superuser):  1
    CountStalledWorkflows:     0, error "cleat.tenant_id is not set" (P0001)

They **check** their errors, so they fail loudly rather than reporting a confident wrong number —
broken metrics, not lying metrics, which is the better of the two failure modes and still a bug.
The cluster compose file connects workers as the NOSUPERUSER/NOBYPASSRLS `cleat_app` role, and
these are the counts an operator watches to notice stalled work. Both now use `beginTxWithRLS`.

> **The trap that made this hard to see, and that cost me two false passes.** The policy is
> evaluated *per candidate row*. Against an empty table the read returns 0 rows and never calls
> `assert_tenant_set` at all, so the query looks healthy. My first two probes seeded nothing — the
> second failed on a `workflow_defs` column that does not exist — and both "passed" against the
> broken code. The test now asserts its own fixture (`truth != 1` is a `t.Fatalf`) before asserting
> anything about the behaviour.

**Found on the way, not fixed — `assert_tenant_set` misses the empty string.** It raises on
`IS NULL`, but `setRLSOnTx` uses `set_config(..., true)`, which is *transaction-local*. After any
RLS transaction the pooled connection carries `cleat.tenant_id = ''` rather than NULL, so
`current_setting('cleat.tenant_id', true)` returns `''`, the `IS NULL` guard does not fire, and
`RETURN tid::uuid` fails with

    pq: invalid input syntax for type uuid: "" (22P02)

instead of the intended message. Same defect, two different errors depending on whether that
connection has been used before — which is exactly the kind of variation that makes a report hard
to reproduce. The fix is a one-line `IF tid IS NULL OR tid = ''` in a **new** migration (editing
the applied `001_schema.sql` would not re-run on existing databases); WS-3's reserved range is
030–039. Left for its own PR because it is a schema change, not a Go change.

Verified: `go test ./engine/ -p 1 -count=1` against all three dialects → **ok, 69s**.

### 3.48 `assert_tenant_set` missed the empty string — ✅ **FIXED** (2026-09-01)

The follow-up §3.47 recorded. `cleat.assert_tenant_set()` raises when `cleat.tenant_id` is
missing, so a query reaching an RLS-forced table without a tenant context fails loudly. It tested
only for NULL — but `setRLSOnTx` sets the GUC with
`set_config('cleat.tenant_id', $1, true)`, where the third argument is `is_local`: scoped to the
transaction and reset when it ends.

**Reset is not undefined.** Once a session has set the GUC even once, `current_setting` returns the
**empty string** on that connection rather than NULL, the `IS NULL` guard misses, and
`RETURN tid::uuid` fails. Measured on one pinned connection:

| connection state | error |
|---|---|
| fresh | `cleat.tenant_id is not set` (P0001) — the intended message |
| after one RLS transaction | `invalid input syntax for type uuid: ""` (22P02) |

Both correctly *refuse* the query, so this is a diagnostics defect and not a data-integrity one.
It still matters: connections come from a pool, so which error a given query produces depends on
whether that connection happened to serve an RLS transaction earlier — effectively random.
Identical bugs arrive wearing two different messages, and the 22P02 one never mentions tenants,
reading like malformed caller input rather than a query that forgot its tenant context.

Fixed in `migrations/postgres/034_assert_tenant_set_empty_string.sql` with
`IF tid IS NULL OR tid = ''`. `CREATE OR REPLACE` rather than DROP + CREATE, because every
`tenant_isolation_*` policy in `001_schema.sql` references the function. A **new** migration rather
than an edit to 001, because `migration.Runner` records by name and never re-runs, so editing 001
would fix only databases created afterwards and silently leave existing deployments on the old
definition — the same reasoning §3.12 records from the other direction.

Two things the regression test has to do, either of which would otherwise let it pass vacuously:

- **Pin one connection** with `db.Conn`. The pool is free to hand the second query a different,
  never-used connection, which takes the NULL path and passes against the unfixed function.
- **Assert the precondition**, that `current_setting` really is `""` after the transaction, before
  asserting anything about the error.

Proved red by moving 034 aside and restoring the old definition: it failed with
`invalid input syntax for type uuid: ""` against `want one containing "cleat.tenant_id is not
set"` — the exact expected reason. Green once the Runner applied 034 (recorded as version 34).

Verified: `go test ./engine/ ./migration/... -p 1 -count=1` against all three dialects →
**both ok**, engine 70s.
### 3.49 A fault that never reached the database reported itself as active — ✅ **FIXED** (2026-09-01)

The third of the errcheck classes from §3.46. `FaultInjector`'s three `ExecContext` calls discarded
their errors *and* set `active[ft] = true` unconditionally, so a fault whose write never landed was
indistinguishable from one that did, and `IsActive` returned true either way.

That is this repo's signature failure — a green result that measured nothing — sitting inside the
harness built to catch it. A test asserting "the system recovers from a worker crash" passes with
no crash injected, because `InjectWorkerCrash`'s UPDATE *is* the crash.

**Why it survived: the database path had no coverage at all.** Every existing test in
`unit_test.go` constructs `NewFaultInjector(nil)`, so only the in-memory `active` map was ever
exercised — nine tests over a type whose entire point is what it does to a database.

    grep -c "NewFaultInjector(nil)" engine/unit_test.go

Fixed on both halves, because either alone is insufficient: the three methods now **return** their
error, and mark the fault active **only on success**. Returning an error is source-compatible —
`fi.InjectClockSkew(x)` as a statement stays legal — and the only caller is `unit_test.go`.

Three new tests against a real database, asserting the rows actually changed rather than that no
error came back:

- `InjectWorkerCrash` really moves the instance to `ready` with `assigned_to` NULL
- `InjectClockSkew` really pushes `heartbeat_at` forward, and `Cleanup` really puts it back — a
  silent restore failure leaves every running instance with a future heartbeat, which the next
  test sharing the database inherits as an unexplained failure
- against a **closed pool**, injection reports the error and does *not* claim the fault is active

The third is what makes the other two mean anything: tests over a working database pass equally
well against the old error-discarding code. Mutation-tested by restoring the unconditional
behaviour — it fails with "IsActive(FaultWorkerCrash) is true after the injecting write failed",
the expected reason.

Verified: `go test ./engine/ -p 1 -count=1` against all three dialects → **ok, 73s**.
### 3.50 SQL Server's `plugin_deps` has never round-tripped — ✅ **FIXED** (2026-09-01)

The last of the errcheck classes from §3.46, and the dropped error turned out to be hiding a data
bug rather than guarding against a hypothetical one.

All three dialects marshal `PluginDeps` and pass the result as `[]byte`. go-mssqldb binds `[]byte`
as `VARBINARY`, and the implicit conversion into the column's `NVARCHAR(MAX)` reinterprets the
UTF-8 bytes as UTF-16:

    written    {"llm":"1.2.0"}
    read back  ≻汬≭∺⸱⸲∰}

PostgreSQL (`JSONB`) and MySQL (`JSON`) are unaffected — both are validating JSON types and both
drivers bind `[]byte` correctly for them. **Every `plugin_deps` row ever written by SQL Server is
mangled**, and it propagates: `cleatctl deploy` carries the previous version's `PluginDeps` onto
the new one, so the loss travels down the version chain and each new version records it as fact.

**Nothing noticed because the read discarded its `json.Unmarshal` error** and defaulted the nil map
to an empty one, so every caller saw the entirely plausible "this workflow declares no plugin
dependencies". This is the §3.46 class paying off twice: the first pass found a defect, and fixing
the *reporting* is what surfaced this one.

> What is *not* affected, since it would be easy to overstate: the worker's plugin compatibility
> gate (`cmd/cleat-worker/setup.go`) reads `PluginDeps` from the WASM metadata, not from this
> column. It was never at risk.

**Write fixed** with `string(pluginDepsJSON)` on the MSSQL path — one word, confirmed by a
round-trip test that fails on `mssql` alone when reverted.

**Reads deliberately stay permissive**, which is the weaker option and was a judgement call.
Returning the error is strictly more correct and is what §3.46 did for its own case. It cannot be
done here: every SQL Server row written before this change is mangled, so a read that errors turns
a latent data bug into an outage — `GetWorkflowDef` failing means the workflow cannot be loaded at
all. One `decodePluginDeps` helper now serves all 7 sites, logs the failure naming def and version,
and returns an empty map. The read self-heals on the next deploy of each definition. What changed
is that it is no longer *silent*.

`TestUnparseablePluginDepsIsLoggedNotFatal` pins that contract so a future tightening is a
deliberate decision rather than an accident.

**A schema asymmetry found on the way, recorded not fixed.** The column is a validating JSON type
on two of three dialects:

| dialect | column | validates |
|---|---|---|
| postgres | `JSONB NOT NULL` | yes |
| mysql | `JSON NOT NULL` | yes |
| mssql | `NVARCHAR(MAX)` | **no** |

So syntactically invalid JSON cannot be stored at all on Postgres or MySQL — my first attempt at
the test tried and both databases refused it (`pq: invalid input syntax for type json`,
`Error 3140 (22032): Invalid JSON text`). SQL Server alone will store anything. Adding a CHECK
constraint is a schema change and belongs in its own PR.

Both halves mutation-tested, each failing for its own reason: reverting the write fix fails
`TestPluginDepsRoundTrip/mssql` **only** — postgres and mysql still pass, which is what isolates
the defect to SQL Server — and making the read hard-fail trips the permissive test.

Verified: `go test ./engine/ -p 1 -count=1` against all three dialects → **ok, 71s**.
### 3.51 The one JSON column SQL Server did not validate — ✅ **FIXED** (2026-09-01)

The schema asymmetry §3.50 recorded. Every other dialect enforces `plugin_deps`'s shape at the
database, and SQL Server's own schema already applies the same pattern to its other JSON columns —
`ck_plugin_defs_config`, `ck_workflow_instances_input`, `ck_workflow_instances_result`,
`ck_workflow_instances_query_state`, `ck_workflow_signals_payload`, all in `001_schema.sql`.
`plugin_deps` was simply missed. `migrations/mssql/036_plugin_deps_isjson.sql` applies the existing
convention to the column that skipped it.

Not academic: a validating column would have rejected the `[]byte`→`VARBINARY`→`NVARCHAR(MAX)`
mojibake write on its first attempt, which is exactly why PostgreSQL and MySQL never had that bug.

**`WITH NOCHECK`, and it is load-bearing.** Measured against a live server:

    ISJSON('{"llm":"1.2.0"}')  = 1
    ISJSON('<the mojibake>')   = 0

Every `plugin_deps` row written before §3.50's fix is mangled, so a plain `ADD CONSTRAINT` —
which validates existing rows — would **fail on every existing deployment and block the upgrade**.
`WITH NOCHECK` enforces the rule going forward and leaves history to the self-heal path §3.50
already chose. The constraint is therefore *untrusted*
(`sys.check_constraints.is_not_trusted = 1`), which costs nothing here since nothing filters on
`plugin_deps`, and `TestPluginDepsCheckConstraintIsUntrusted` pins it so a later tidy-up to
`WITH CHECK` cannot silently reintroduce the failed upgrade.

**The constraint found a second, unrelated defect within minutes of existing** — which is the
argument for adding it. `json.Marshal` of a nil map returns the four bytes `"null"`, not nil, so

```go
pluginDepsJSON, _ := json.Marshal(def.PluginDeps)
if pluginDepsJSON == nil {          // never true
    pluginDepsJSON = []byte("{}")
}
```

was dead code in **all three dialects**, and every workflow declaring no plugin dependencies stored
the literal `null`. PostgreSQL `JSONB` and MySQL `JSON` both accept a bare JSON scalar, so nothing
noticed; `ISJSON('null')` is 0, which is what surfaced it. Fixed in all three: the guard now tests
`len == 0 || string == "null"`, and folds in the marshal error, because the column is
`NOT NULL DEFAULT '{}'` everywhere and "no dependencies" has one spelling.

> **Ordering constraint, verified not assumed.** 036 cannot land before §3.50's write fix. Against
> a tree without it, the constraint rejects every SQL Server deploy —
> `The MERGE statement conflicted with the CHECK constraint "ck_workflow_defs_plugin_deps"` — and
> five `TestAdminForce*` tests fail. That is the constraint doing its job, and it is why this PR
> was stacked rather than run in parallel.

Mutation-tested: with 036 removed and the constraint dropped, both new tests fail — one because a
non-JSON insert is accepted, the other because the constraint is absent entirely.

**Wider asymmetry recorded, not fixed — and counted per table, because a column name alone is not
the unit.** `result` carries a check on `workflow_instances`, `workflow_promises` and
`idempotency_keys` but not on `workflow_update_requests`, so any command that dedupes by column
name (my first attempt did) reports it as covered.

> **Superseded by §3.53, and the list that stood here was wrong.** It named all eight of
> `event_history`'s JSON-ish `NVARCHAR(MAX)` columns as candidates. Only **`payload`** is: the
> other seven — `response`, `signal_payload`, `child_input`, `new_input`, `plugin_input`,
> `plugin_output`, `promise_result` — are declared **`TEXT`** on PostgreSQL, not `JSONB`, so the
> project does not claim they are JSON and constraining them on SQL Server would reject writes the
> other dialects accept. The instinct in the next paragraph was right; the enumeration was not,
> because it was eyeballed from column names rather than derived from the PostgreSQL types.

Whether each *should* be validated is a per-column judgement rather than a sweep: `response` and
`plugin_output` may legitimately hold a non-JSON payload from a service or plugin, and constraining
them would be a behaviour change rather than a tightening. §3.53 settles the boundary.

### 3.53 JSON-column parity is now a checked invariant, not a sweep — ✅ **FIXED** (2026-09-01)

§3.51 fixed one column and left "the rest" as a per-column judgement. Deriving the boundary turned
that judgement into a rule that CI can enforce:

> **A column PostgreSQL declares `JSONB` must carry an `ISJSON` check on SQL Server.**

`JSONB` is where the project actually commits to a value being JSON, so it is the right line. The
seven `event_history` columns §3.51 listed as candidates — `response`, `signal_payload`,
`child_input`, `new_input`, `plugin_input`, `plugin_output`, `promise_result` — are **`TEXT`** on
PostgreSQL. A service or plugin may legitimately return something that is not JSON, so constraining
those on SQL Server would reject writes the other dialects accept: an inconsistency in the opposite
direction. §3.51's list has been corrected in place.

**Six columns were genuinely missing**, two of which I had not noticed at all until a script
enumerated them:

| column | PostgreSQL |
|---|---|
| `event_history.payload` | nullable |
| `workflow_defs.dag_spec` | nullable |
| `workflow_instances.compaction_state` | nullable |
| `workflow_instances.plugin_vers` | **NOT NULL** |
| `workflow_instances.allowed_signals` | nullable |
| `workflow_update_requests.result` | nullable |

`migrations/mssql/037_json_column_checks.sql` adds all six, `WITH NOCHECK` for the reason §3.51
records, tolerating NULL wherever PostgreSQL does.

**The deliverable is the guard, not the migration.**
`TestMSSQLValidatesEveryPostgresJSONBColumn` parses both schemas and fails if any PostgreSQL
`JSONB` column lacks a SQL Server check. It reads the migration *files* rather than a live
database — those files are the schema, and a file-based check needs no DSN, so it runs in every job
and adds no skips.

**It matches per table, and that is the whole subtlety.** `payload` and `result` each appear on
several tables and are checked on some of them, so matching `ISJSON(payload)` anywhere in the file
credits `event_history.payload` to `workflow_signals`. **I made that exact mistake twice** — once in
the §3.51 audit and once in the first draft of this one — which is why the guard is mutation-tested
against it specifically: deleting only the `event_history.payload` constraint must, and does,
produce a failure naming `event_history.payload` while `ISJSON(payload)` still exists elsewhere in
the file.

The rule is enforced in one direction only, deliberately: `JSONB` implies a check, not the reverse.
A column that should not be constrained should stop being declared `JSONB` on PostgreSQL, because
that declaration is the claim being enforced.

Verified: the six constraints applied to a live SQL Server and the full engine suite passes against
all three dialects, so nothing the engine writes violates them. `sys.check_constraints` confirms
all seven cleat-added checks are untrusted while the five original in-table ones stay trusted.

### 3.52 InitModule discarded the error it had a channel for — ✅ **FIXED** (2026-09-01)

Third pass through errcheck's `engine` residue, now down to **8** non-`Rollback` production findings
from 19. Two were real; the other six are recorded below as deliberate.

`Runtime.InitModule` dispatches `_start` in a goroutine and reports the result through `errCh`,
which is read in three places. But `errCh` was only ever *written* on panic:

```go
go func() {
    defer func() {
        if r := recover(); r != nil { errCh <- ... }
        close(done)
    }()
    start.Call(ctx)          // returned error discarded
}()
```

**wazero signals a trap by returning an error, not by panicking.** So the common failure was thrown
away while the rare one was caught, `close(done)` still fired, and `InitModule` returned nil for a
guest whose `_start` had trapped — the damage surfacing later on some unrelated export call. The
machinery to report it was already there and simply not connected: the same shape as §3.44, §3.45
and §3.50, a signal that existed but was attached to the wrong thing.

**`exit(0)` is not a failure**, and getting that wrong would break every Go guest: a Go wasip1
`_start` runs `main()` and terminates via `proc_exit`, which wazero surfaces as `*sys.ExitError`.
So the fix reports every error *except* exit code 0.

> **That carve-out was unverified when written, and the mutation proved it.** Inverting
> `ExitCode() == 0` to `== 999` left the **entire engine suite green** — nothing exercised a real
> `proc_exit(0)` through `InitModule`. Closed with `TestInitModuleTreatsExitZeroAsSuccess`, which
> hand-builds a module importing `wasi_snapshot_preview1.proc_exit` and calling it with 0. Re-run
> against the same mutation, it now fails with
> `InitModule reported a failure for a guest whose _start exited 0: ... wasm trap: exit(code=0)`.
> Without that test the fix would have shipped with an untested branch on the path every Go guest
> takes.

**Second finding, and it was mine.** §3.49 made `FaultInjector.Cleanup` return its error;
`Reset` — which is Cleanup under another name — still discarded it. errcheck found it only because
the call was bare, and `_ = fi.Cleanup()` would have hidden it again, which is §3.43's blind spot
arriving in code I had just written. `TestResetPropagatesCleanupError` pins it.

**The remaining six are deliberate**, and naming them is the point so the next reader does not
re-derive:

| site | why it stays |
|---|---|
| `backend_wasmtime.go` ×2 | component decomposition, tier 3 — parked, untested, changing it is not free |
| `runtime.go:427`, `durablecalls.go:79` `CloseWithExitCode` | close-error idiom; the module is being torn down |
| `sharded_store.go:85` `shard.Close()` | `ShardedStore.Close()` returns nothing; changing it is a public API break for no gain |
| `version_handler.go:245` `json.Encoder.Encode` | the HTTP-response-write idiom `.golangci.yml` already decomposes out |

Verified: `go test ./engine/ -p 1 -count=1` against all three dialects → **ok**. All six guard
scripts pass; skip budget 483 against 487, no new skips.

### 3.37 SQL Server has no administrative access under RLS — ✅ **FIXED** (WS-1, 2026-08-06)

> Numbering note: §3.35 is used twice already — WS-3's "What `defer` is supposed to be" and
> WS-1's concurrency-key item above. Skipping to 3.37 rather than adding to that.

SQL Server applies a security policy to **every** principal. Not "every principal except the
owner", the way PostgreSQL exempts a superuser — every one, sysadmin and `dbo` included.
Measured, not read: connected as `sa` with no session context, a table holding three rows reads
as empty.

So there was no connection that could see across tenants. Not for a support query, not for a
cross-tenant backfill, and not for test teardown — which is where it surfaced, as §2.71's
blocker. `CleanupMSSQLTestData` issues `DELETE` on a plain pool, the filter predicate hides
every row it means to delete, and it removes nothing **and reports no error**, so teardown
believes it ran. Rows accumulate until a later fixture collides on a primary key. That is the
141-failure signature recorded in §2.71's residual.

PostgreSQL never hit this, and not because its policies are weaker. `005_app_role.sql` spends
its entire header arguing the *opposite* case — keeping `cleat_app` `NOSUPERUSER`,
`NOBYPASSRLS` and owning nothing, so the application is subject to the policies
unconditionally. That design has two halves. SQL Server inherits the restricted half for free
and cannot inherit the privileged half at all, because the predicate is the only place an
exemption can live there.

**Fix:** `migrations/mssql/012_admin_role.sql` creates an empty `cleat_admin` database role and
adds `OR IS_ROLEMEMBER(N'cleat_admin') = 1` to `fn_tenant_filter`.

**Why role membership and not a sentinel session-context value.** A magic `tenant_id` would be
assumable by anything that can call `sp_set_session_context` — which is the application itself,
on every connection it opens. One bad code path or one injected value and tenancy is off. Role
membership is granted once by a DBA and cannot be assumed by a connection at runtime.

Three properties make it hard to acquire by accident, all verified against SQL Server 2022:

| property | consequence |
|---|---|
| the role ships with **no members** | applying 012 changes nothing until a deployment grants it |
| `db_owner` does not confer it — `sa` reads `IS_ROLEMEMBER = 0` and stays filtered | "the app connects as sa" does not silently lose isolation |
| `dbo` **cannot** be added — SQL Server refuses with *"Cannot use the special principal 'dbo'"* | membership requires a user someone created on purpose |

The second is the one that decided the design. Had `sa` been exempt, this migration would have
disabled tenant isolation for the default deployment the moment it applied, which is worse than
the gap it closes.

**Cost, measured rather than asserted** (50k rows, 300 queries, two interleaved rounds):

| predicate | point lookup | full scan |
|---|---|---|
| session context only | ~340 µs | 3.38 / 3.40 ms |
| **+ `IS_ROLEMEMBER` (shipped)** | ~350 µs | 4.06 / 4.10 ms — **+20%** |
| role checked first | ~350 µs | 4.49 / 4.52 ms — +33% |
| role in a scalar subquery | ~350 µs | 8.96 / 9.07 ms — +166% |

`IS_ROLEMEMBER` is evidently not folded to a per-query constant; the cost tracks rows scanned
(~14 ns/row). Point lookups — the store's dominant access pattern, by workflow ID — show no
measurable difference. The 20% lands on full scans of tenant-filtered tables, i.e. an
unqualified `ListWorkflows`. Two rewrites were tried and both were worse, so the form shipped
is the cheapest of the three; nobody needs to re-measure this. Accepted because the alternative
is having no administrative path at all.

**Tests:** `migration/mssql_admin_role_test.go`, four of them, against a scratch database
migrated by the real Runner. Only one asserts the new capability; the other three assert that
nothing else moved, and they pass with or without the fix **by design** — they are guards, not
regression tests. Falsification check: with the `OR` clause removed, exactly
`TestMSSQLAdminRoleSeesAndDeletesAcrossTenants` goes red (`saw 0 rows across two tenants, want
3`) and the three guards stay green.

**This unblocks §2.71 but does not complete it.** The schema switch still needs
`CleanupMSSQLTestData` to take an admin connection, and the harness needs to create the login —
which should fail closed the way `005_app_role.sql` does, shipping no credential. Draft #333
stays draft until that lands.

### 3.38 `TestAdminForceResolve_*` failed once after an all-dialect run — 🔶 **OBSERVED, not reproduced** (WS-1, 2026-08-06)

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

### 3.33 gosec's 283 findings, triaged — 🔶 **2 fixed, 281 classified** (WS-3, 2026-08-05)

`PARALLEL-WORKSTREAMS.md` calls gosec "unreviewed security findings in a codebase whose last
two days have been tenancy defects" and says an unreviewed 283 is worse than a reviewed 283
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

### 3.20 `AdminForceComplete` / `AdminForceFail` were stubs — ✅ **FIXED** (WS-2, 2026-08-05, #297)

> Recovered heading, added 2026-08-06. This section's body had been appended into §3.33's
> body with no heading of its own, so `§3.20` could not be found by section number even
> though three other places cite it — including WS-2's status doc and the round-2 sequencing
> table, which named it as WS-2's "start here" item. The content below was always here; only
> the heading was missing.

`AdminForceComplete` and `AdminForceFail` returned `"admin force-complete: not implemented
yet"` on all three dialects. `cmd/cleat-worker/api_admin.go` routed
`POST /api/admin/instances/{id}/force-{complete,fail}` to them behind the `X-Confirm` guard
and the ownership check §1.7 added, so an operator trying to unstick a workflow got a **500
from an endpoint whose route, confirmation header and authorization were all real** — the one
part that was missing was the operation.

Every existing test passed throughout, because all seven of them supply a `mockStore` whose
`adminForceCompleteFn` returns `nil`. §2.17's shape again: the tests ran one layer above the
thing that was broken.

**What shipped.** Real bodies for both operations on all three dialects
(`engine/store_admin.go`), each doing four things in one transaction:

1. A terminal status write fenced on **generation but not on `assigned_to`** — the workflow
   being force-resolved usually has no live owner to match, which is the whole reason the
   operation exists. Generation is what makes a stale operator request fail instead of
   resolving a workflow that has moved on.
2. A **generation bump**, so the write fences off a worker that still believes it owns the
   run. `ReapStaleInstances` bumps for the same reason. Tested: after a force-complete the
   previous owner's `CompleteWorkflow` returns `ErrFenceLost` and does not overwrite the
   operator's result.
3. An **`admin_action` audit event appended through `appendEventsInTx`**, in the same
   transaction — so the audit record joins the checksum chain rather than sitting beside it.
4. The post-commit cleanup a normal terminal write does: sticky worker, concurrency keys,
   parent close policy.

**Three things found while building it, none of which was the stub.**

- **The handlers applied the operation to the wrong store.** `callerOwnsTarget` checked
  ownership against the caller's tenant-scoped store and then every handler called
  `engine.ForceComplete(..., s.store, ...)` — the process-wide one. A force-resolve
  authenticated as tenant B ran against the default tenant's scope. Invisible until now
  because both stores answered `not implemented yet`, and because `newTestAPIServer` serves
  every tenant from one mock, so the two stores are the same object in every existing test.
  `callerOwnsTarget` now returns the store it checked against.
- **`eventRecordToPayload` has no `admin_action` arm**, so the audit event's payload would
  have been `{}` — and `computeEventChecksum` hashes payload alone. Who forced a workflow and
  what they did to it would have sat entirely outside the checksum, editable in the columns
  afterwards with `VerifyWorkflowEvents` still reporting the workflow clean. Note that
  `verifyShadowColumns` does **not** catch this and looks like it would: `populateFromPayload`
  only overwrites keys the payload carries, so a key the payload omits inherits the column's
  value and always compares equal. An empty payload reads as agreement. The unit test is what
  holds that arm up; removing the arm leaves every database test green.
- **The audit append can be silently displaced.** Every dialect's event append is an upsert
  that leaves an existing row alone, so if a concurrent writer takes the step number between
  the `MAX(step)+1` read and the insert, the audit event vanishes and the status change
  commits without it. The whole force-resolve is rolled back instead.

**Tenant scoping is explicit on all three dialects**, including PostgreSQL where RLS would
cover it. That is deliberate and is what makes the cross-tenant test mean anything: engine
tests connect as the owner, RLS is bypassed for superusers, so a PostgreSQL-only enforcement
would let the test pass against a store with no filter at all. The unmerged version of this
code in #208 dropped the `tenant_id` filter from the MSSQL `UPDATE` while keeping it on
MySQL, which is the gap this note existed to warn about.

**Every test was proved able to fail** by reverting the specific mechanism: the tenant filter,
the generation bump, the payload arm, the not-found/mismatch disambiguation, the collision
check, the scoped store, and the status-code mapping — eight reverts, each failing only its
own test. Two of those reverts corrected the work rather than confirming it: the collision
test was initially passing because the confirm lookup was tenant-scoped and returned "no
rows" for the planted row, never reaching the comparison it was meant to exercise (the lookup
is now by primary key, which is the right scope for "is the row at this step mine"); and the
payload arm's justification named `verifyShadowColumns`, which turned out not to detect it.

**Also.** `result` is JSON-typed on every dialect (JSONB / JSON / `ISJSON` check), so a
non-JSON result is now rejected as a 400 rather than surfacing as three different driver
errors reported as 500; an omitted result means JSON `null`. And `ErrAdminOpNotImplemented`
separates 501 from 500, so the one operation that genuinely is not built says so.

~~**`AdminReReplay` is still a stub, deliberately**~~ — **implemented 2026-09-02, once §1.4
phases D–F existed.** It answered **501** rather than 500 in the meantime, which is what made
the wait honest rather than a silent gap.

The reason it was not the same size as the other two held up: resetting a workflow to `ready`
means it replays its recorded history and continues, so it needed the replay semantics D–F are
about, not a fourth `UPDATE`. What that bought, concretely, is the refusal below — impossible to
write before phase F, because there was nowhere to send the operator.

**Three preconditions, and each needed its own words because the HTTP layer separates them.**
Only `failed`, `terminated` and `dead_lettered` can be re-replayed. `done` is excluded because
replay would walk a complete history to its end and finalize again; an operator who wants a
finished workflow to run again wants a *new run*, which is the dead-letter reprocess path
(`cmd/cleat-worker/app.go`) — from the definition and input rather than from history. **The two
are different operations**, and re-replay is the one that preserves completed steps: a workflow
that failed on its ninth call does not re-issue the first eight. The non-terminal statuses are
excluded because the dispatcher already owns them, and bumping the generation would pull the
workflow out from under whichever worker is about to claim it.

That third case is why `adminResolveMiss` could not be reused: it distinguishes *not found* from
*generation mismatch*, and a status refusal with the generation matching would have been
reported as a mismatch — sending an operator hunting a concurrent writer that does not exist.

**It refuses to re-replay into an unresolved ambiguity, and that is a judgement rather than
something §3.20 states.** A history whose last call was left mid-flight replays straight back
into `[AMBIGUOUS]`: same stop, same place, same reason, one generation spent learning nothing.
The alternative — proceed and let replay report it — is defensible and keeps re-replay a pure
status reset. It loses the one thing the operator needs, which is *which step* to reconcile, and
phase F now gives them somewhere to put the answer. The test asserts that resolving the step
then lets the re-replay through, so the guard is a redirect rather than a dead end.

**`ErrAdminOpNotImplemented` outlives the stubs it was built for.** No store in this repo returns
it any more. It and `handleAdminOpError`'s 501 branch are kept because `WorkflowStore` is a public
interface: an out-of-tree store implementing part of it has the same problem, and 501 is still the
honest answer. `store_admin_stubs.go` is gone — a file of that name containing no stubs is worse
than no file.

**A test bug worth recording, because it is the vacuity shape one level down.** The first draft
passed `wf.Generation+1` to `ReReplay`, assuming `FailWorkflow` bumps the generation. It does not.
That made the *stale generation* case pass **for the wrong reason** while the happy path failed —
so a suite containing only the refusal test would have been green and vacuous. The helper now
re-reads the instance after failing it rather than predicting what the write did.

### 3.14 `examples/dag` is red on `develop`, and no CI job runs it — ✅ **FIXED**

**Two of the three claims in that heading were wrong, and the third was half wrong.**

It was not the example. `cleat/cleattest` never wired `AwaitAnyChild` into
`HostCallsOptions`, so the hook was nil for every `TestEnv` and
`cleat/runtime_children.go` returned its "the HostCalls runtime was not initialized"
message — which is about workflow context and says nothing about the harness. The
diagnosis below took that message at face value. `plugins/dag/dag.go` is the only caller
of `AwaitAnyChild` in the repo, so all six tests failed on it; and no external SDK user
could test a workflow using that call either. So this was the shipped public test harness,
not example code, and "low severity" was wrong.

"No CI job runs it" was also wrong. **The Tier 2 Gate runs `./examples/...`** — the six
failures were recorded in `tier2.known_failures`, which is exactly the mechanism working.
What is true is the narrower claim: nothing in `.github/workflows/` runs `examples/` other
than `as-workflow` *outside* the tier gates, and the gate's known-failure list meant these
six were red-but-permitted rather than unwatched.

Fixing them emptied `tier2.known_failures` for the first time, and the gate is what forced
it — a stale entry fails the gate in the same way a new failure does.

`TestEveryHostCallIsWired` in `cleat/cleattest` is the mechanism, and it found **19 more**
unwired host calls: seven that hard-error the same way (`ResolvePromise`, `RejectPromise`,
`ScheduleCron`, `DeleteCron`, `ListCrons`, `ContinueAsNewWithVersion`, `DurableDeferFunc`)
and twelve with real fallbacks. `AwaitAnyChild` and `PollChild` were simply the two someone
happened to write an example against.

Of those seven, **one is left** as of 2026-08-08 (`DurableDeferFunc`, still waiting on the
question of when its closures drain). `ResolvePromise`, `RejectPromise` and
`ContinueAsNewWithVersion` were wired the same day. The three cron calls took longer and
were the more interesting case: the guard's own note said mocking them would be *inventing*
a specification, because `cleat.HostCalls` declared three methods **the engine did not
implement anywhere** — an AssemblyScript guest that called one failed at instantiation with
`unknown import`. That is now built (#430, #431, #432): the host calls exist on both
backends, journaled and replayed, and `cleattest` validates against the engine's own
`ValidateCronExpr` / `ValidateTimezone` rather than rules invented in a harness. Delivery is
at-least-once; see `tiers.yaml`, `workflow-callable-cron`, which also records that Python
still cannot make these calls.

A second defect was hiding behind the first: `dag.go` discarded `completedRunID` on the
error path, so a failed child reported `dag: await any child failed: <message>` without
naming which of the tasks failed.

<details><summary>The original entry, kept because its diagnosis is the thing worth
learning from</summary>

Noticed while sweeping `go test ./...` for §3.12 regressions, and confirmed against a clean
`develop` worktree at `2ee62d0` so it is not that change:

```
--- FAIL: TestDAGExecuteDiamond
    pipeline_test.go:80: Execute failed: dag: await any child failed:
    durable: AwaitAnyChild can only be called from within a workflow function
    (the HostCalls runtime was not initialized)
```

Six tests, all in `examples/dag`, all the same cause. `.github/workflows/` runs
`examples/as-workflow` and nothing else under `examples/`, and no job runs `./...`, so
nothing has ever reported this. Low severity — it is example code, not the engine — but it is
the §2.31/§2.33 pattern once more: a suite that exists, fails, and is watched by nobody.
Either wire it into a job or say in the tree that examples are not tested.

</details>

Two things that are **not** defects and are recorded so the next sweep does not re-derive them:

- `tests/exhaustion` used to fail hard locally with `cluster database unreachable … this test
  requires docker-compose.cluster.yml to be up`, even with nothing configured — that was
  tiers.yaml's blocker on gating it (fixed 2026-08-07, see tier2.gated_by). `clusterDB()` now
  applies §2.12's distinction itself: `CLEAT_TEST_POSTGRES`/`CLEAT_TEST_DB` unset skips (nobody
  asked), set-but-unreachable still fails naming the redacted DSN. It runs in the `cluster` job
  (ci.yml's "Cluster Integration Tests"), which sets `CLEAT_TEST_DB` explicitly for this step,
  so a skip there can only mean that override stopped taking effect — see
  `scripts/skip-budget.txt`'s `cluster/exhaustion` entry (budget 0).
- `tests/plugin-harness` fails when the repo is entered through `/localssd/rcownie/cleat`,
  the symlink `PARALLEL-WORKSTREAMS.md` tells all three streams to use:

  ```
  cleat build (go) failed:
  directory /localssd/rcownie/cleat/cmd/cleat outside main module or its selected dependencies
  ```

  The harness shells out to `go run <projectRoot>/cmd/cleat`, and the Go toolchain rejects a
  module path reached through a symlink. From `/Users/Shared/localssd/rcownie/cleat` the same
  suite passes. Worth knowing before someone spends a session on a phantom regression.

### 3.22 An ambiguous call is erased, not reported — ✅ **FIXED** (WS-2, 2026-08-05)

Found by building §1.4's T3 crash scenario. Not an intent-path defect: intent is only what made
it visible, by producing the first workflow in this repo that *should* end in failure after a
crash.

> **Correction, and then a correction of the correction — 2026-08-05.**
>
> This entry first said the mechanism was "a second `cleat_complete` with status 0 overwrites
> the first with status 1", inferred from two probe lines. It was then retracted as wrong,
> because instrumenting `Engine.Replay`'s boundary showed **one** execution returning
> `err == nil` with a 233-byte result rather than two competing completions.
>
> **The retraction was the error.** That observation is exactly what the original hypothesis
> predicts: one execution, `err == nil`, and the 233 bytes *are* the `{"error":…}` payload.
> A single instrumented boundary could not distinguish the two accounts, and it was read as
> though it could. There are two `cleat_complete` calls, and the second does win — not by
> overwriting a variable, but by winning a precedence check. See step 3 below, now read off the
> generator and the backend rather than inferred from probes.

**What actually happens**, end to end, each step measured:

1. The engine detects the pending intent row and returns
   `[AMBIGUOUS] call outcome unknown at step 2 …` to the guest with `errCode = 1`. Correct.
2. The guest's adapter maps that to a `cleat.CallError`; the fixture returns it. Correct.
3. **The guest reports the failure correctly and the host then discards the report.** The
   generated `cleatDispatch` error branch calls `cleatCompleteImport(1, errPtr, errLen)`
   (`wasm/exports.go:560`) — status 1, the failure — and *then returns the same error as its
   `[]byte` result*. Generated `main()` (`wasm/build.go:174`) unconditionally re-reports that
   return value:

   ```go
   result := cleatDispatch(entryName, args)
   resultPtr, resultLen := stringPtr(string(result))
   cleatCompleteImport(0, resultPtr, resultLen)   // no branch on whether it failed
   ```

   The host binding keeps both, correctly, in separate variables
   (`engine/wasmtime_hostfuncs.go:70-75`). `engine/backend_wasmtime.go:582` then checks
   `completeResult` **first**, so the status-0 report wins and `Execute` returns a success.
   `Engine.Replay` returns `err == nil`, and the worker takes the success path.

   Two things make this a defect rather than a design choice. The same file gets it right 100
   lines later: `backend_wasmtime.go:684`, the non-Go direct-export path, returns
   `fmt.Errorf("host: export %q failed: %s", …)`. So does wazero (`engine/runtime.go:531`).
   **Go-on-wasmtime — the primary backend for the primary language — is the one path that
   collapses the distinction**, and it is the path every Go workflow in production takes.
4. **That JSON is malformed.** `wasm/exports.go:579` emits
   `[]byte("{\"error\":\"" + encodeJSONString(__e.Error()) + "\"}")` while
   `encodeJSONString` **already wraps its argument in quotes** (`exports.go:229-241`, and its
   own doc comment says so). The result is doubled quotes:

   ```
   {"error":""durable call payments.Ship: [0] [AMBIGUOUS] call outcome unknown at step 2: …""}
   ```

   Line 376 uses the same helper correctly, which is why this is a one-line defect and not a
   design problem.
5. `FinalizeWorkflowSegment` replaced any result that is not valid JSON with `{}` — **silently,
   in a two-line conditional with no log statement, in all three stores.** The workflow is
   stored `done`, `result = {}`, `error_msg = ''`.

So the operator gets a clean success for a charge that may or may not have happened, and there
is no record of the ambiguity anywhere: not in the result, not in the error, not in the log.

**Fixed here (step 5).** `coerceResultJSON` in `engine/store_lifecycle.go`, used by all three
stores, still replaces an unstorable result — failing the terminal write would lose a whole
workflow over a formatting defect — but now logs at ERROR with the workflow ID and the
discarded value, truncated. The empty case stays silent because an entry point with no return
value produces it on every successful run. `TestCoerceResultJSON` covers it, including the
exact malformed string above, and fails if the log line goes away.

**Fixed (step 4).** `wasm/exports.go` now wraps the *key* only, because `encodeJSONString`
supplies the value's quotes — the same way the `__r` branch four lines below has always used it.
The adjacent unmarshal-error emission had the same class of defect by a different route
(raw concatenation of `err.Error()` with no escaping at all, and a `json.Unmarshal` error
routinely contains quotes), so both are fixed together.

`wasm/error_json_test.go` executes the emitted expressions rather than pattern-matching the
emitted text — the defect was in what the code *evaluated to*. That is now this fix's only
cover, and deliberately so: once step 3 landed, the crash path stopped depending on the
emission at all (the failure is carried by `completeErr`, and the `{"error":…}` bytes it used
to ride on are discarded). A test that had to go through a crash to observe a JSON-encoding
defect was the wrong instrument anyway.

**Cross-stream:** `wasm/` is WS-3's. Two lines, in the error-encoding path rather than the
component work they are in the middle of.

**Fixed (step 3).** `engine/backend_wasmtime.go` checks `completeErr` before `completeResult`,
and returns it as an error rather than as a result — which is what the direct-export branch 100
lines below it (every non-Go guest) and the wazero backend (`runtime.go:531`) already did.
**No ABI change was needed**, contrary to what this entry claimed for a day: the wire format was
never involved. Both completions already crossed it, in the right order, with the right status
bits. The host was choosing the wrong one.

Measured on `TestCrashWithWriteAheadIntentDoesNotRepeatTheCall`, before and after:

```
before   status="done"    error_msg=""
                          result={"error": "durable call payments.Ship: [0] [AMBIGUOUS] …"}
after    status="failed"  error_msg="… host: export "three_charges" failed: durable call
                                     payments.Ship: [0] [AMBIGUOUS] call outcome unknown at
                                     step 2: … Check the external service before retrying."
                          result=""
```

`guestErrorText` (`engine/guest_error.go`) decodes what the guest encoded: every Go guest passes
its message through `encodeJSONString` before `cleat_complete`, so the raw bytes are a quoted,
escaped JSON literal, and formatting that into `error_msg` verbatim leaves the escapes in front
of the operator. The `_start` panic recovery writes a plain Go string into the same variable, so
the pass-through fallback is load-bearing rather than defensive.

**The blast radius was the point, not a side effect.** This was never specific to ambiguity:
*every* Go workflow that returned an error was being stored `done`. Two existing tests asserted
that behaviour and were rewritten to assert the failure instead — `TestEngineReplayDivergence`
(which had tolerated the error with a `t.Logf("expected if divergence bails out")`) and
`TestCancellationEndToEnd`. Neither lost an assertion; the substance moved from `result` to
`err`. A cancelled workflow and a diverging replay now end `failed` rather than `done`.

Traps were never affected and still are not: fuel and epoch exhaustion reach the resource-limit
check, because a trapped guest never reaches `cleat_complete` at all. It was specifically the
guest that stopped cleanly and *said* it had failed that was not believed.

**Regression cover.** `TestGuestReturnedErrorIsAFailureNotAResult` runs `basic.PlaceOrder` with
an empty cart — a workflow that returns an error before touching a durable call, so no service,
database or crash is involved in reproducing it. Restoring the old precedence fails it with
`result = {"error":"cart is empty"}` and a nil error, which is the defect stated exactly.

**Cross-stream:** `engine/backend_wasmtime.go` is WS-3's. The change is a swapped precedence
and a comment; neither in-flight WS-3 branch touches the file.

Phase E's *resolver* half has since landed, which shrinks how often this matters: an ambiguity
a resolver can settle never reaches the workflow as an error at all.

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

`resolveWasmTrap` (`engine/dwarf_trap.go:19`) prefixes `wasm trap: ` onto **any** non-empty
message reaching `executor.go:151`, and `executor.go` wraps that again. An operator whose
workflow simply returned an error now reads:

```
host: workflow <id>: execution failed: wasm trap: host: export "three_charges" failed: <their error>
```

Two `host:` prefixes and a claim of a trap where there was none — the guest stopped cleanly and
*said* it had failed. The label sends a reader looking for a memory fault instead of at their
own error text.

**This was latent until §3.22.** Before it, a guest-returned error never reached this path — it
was returned as a success — so everything arriving here genuinely was a trap and the
unconditional prefix was right. §3.22 introduced a second class of error to a function that
labels all of them the same, so this is that change's debt, not a pre-existing defect.

Not fixed there because the fix belongs in `engine/executor.go`, which is WS-3's *and* is
touched by their in-flight `fix/ws3-defers-on-fenced-backend`. `engine/dwarf_trap.go` is
unowned, but `resolveWasmTrap` takes a `string`, so the "is this actually a trap" question can
only be asked at the executor call site. Shape: a sentinel error type from the backend, checked
with `errors.As` before the trap envelope is applied.

#### Resolution — the proposed shape, and a control that had to be rewritten

Fixed 2026-08-31, exactly as the shape above proposed. `GuestReturnedError` (`engine/runtime.go`)
marks a failure the guest reported by calling `cleat_complete` with a non-empty error. Both
backend sites that build that message now return it — `backend_wasmtime.go`'s Go-on-wasmtime
branch and the direct-export branch every non-Go guest takes — and `executor.go` checks it with
`errors.As` before applying the trap envelope. The marker carries no message of its own; the
backend has already built one, and wrapping would add another prefix.

`resolveWasmTrap` is untouched. The question it cannot answer is not asked of it.

**The control test was wrong the first time, and the mutation is what caught it.** The pair here
is "a guest error is not labelled a trap" plus "a real trap still is" — the second exists because
the cheapest way to pass the first is to stop labelling anything. The first version of that
control asserted `strings.Contains(err, "wasm trap")`, and it **passed against a build with the
trap envelope deliberately disabled**: the backend's own message already contains the words, so
the text survives even when the executor stops applying the envelope. It now asserts
`errors.As(err, &wasmTrapError{})`, the branch itself, which fails under that mutation. The
DWARF-enriched source locations are built in that branch, so the weak assertion would have let a
regression through that silently dropped them while the message still read plausibly.

Re-derive:

    go test ./engine/ -run 'TestGuestReturnedErrorIsNotLabelledATrap|TestRealTrapIsStillLabelledATrap|TestGuestReturnedErrorPreservesTheChain' -v

**Not fixed here:** the doubled `host:` prefix this entry also names. The message still reads
`host: workflow <id>: execution failed: host: export "x" failed: <their error>` — one prefix
from the backend, one from the executor. That is a message-format change with a wide blast
radius across test assertions, and it is not what sent readers to the wrong place; the false
trap label was. Left open deliberately rather than bundled.

Noticed while measuring, also unfixed: on the replay path `e.workflowID` is empty, so these
read `host: workflow : execution failed: ...`.

### 3.24 An ambiguous outcome is classified `unknown` — ✅ **FIXED** (2026-08-31)

`engine.ErrAmbiguous` (`engine/errors.go:30`) has existed since the first commit and
`NewAmbiguousError` is called by nothing but its own test. A workflow that ends because it could
not determine whether a charge happened is stored with `error_code = 'unknown'` — the same value
as every other engine-produced failure — so nothing can query for the one class that needs a
human to go and look at the external service.

Unblocked by §3.22 and cheap: `cmd/cleat-worker/setup.go:1703-1707` already does
`errors.As(err, &ce); errorCode = ce.Code.String()`. What is missing is the engine wrapping the
ambiguous failure as `*CleatError{Code: ErrAmbiguous}` on its way out, so the existing path
carries it. That is also the shape §2.35's residual wants, and it needs no ABI change.

Separate and genuinely blocked on the SDKs: a guest-visible `CallErrorAmbiguous`. The guest
enum (`cleat.CallErrorCode`) has no ambiguous member, so a workflow author's `switch e.Code`
sees `[0]`, Unknown. Value 6 is free and the wire field is 32 bits, but every SDK carries its
own copy of the enum — `python-sdk/cleat_sdk/host_calls.py` has a literal `{0..5}` dict — and
those are WS-3's. Worth doing; not what stands between an ambiguous crash and an operator being
told about it. **Still open**, and unchanged by the fix below.

#### Resolution — a structural record, because the message was the only record

Fixed 2026-08-31. The entry above proposed wrapping the failure as
`*CleatError{Code: ErrAmbiguous}` "on its way out", which is right, but it understates what was
missing: at the point the failure leaves the engine there is nothing left to key that decision
on. The ambiguity is detected deep in `replayCall`, which does not return a Go error at all —
it writes a message into guest memory and returns a packed `int64`. So the fix is two halves:

1. `execSession.recordAmbiguity` (`engine/callintent.go`), called at both sites that emit the
   `[AMBIGUOUS]` text — `durablecalls.go` and `heartbeats.go`. First one wins: the earliest
   unresolved call is the one whose side effect has been in doubt longest.
2. `execSession.classifyFailure`, applied in `engine/executor.go` where a guest failure becomes
   a Go error. `cmd/cleat-worker/setup.go` already does `errors.As(err, &ce)`, so the code
   reaches `error_code` with no change there.

The wrap uses an empty `Op` and `WorkflowID`, and `CleatError.Error` now passes that case
through verbatim. Without it the message gains a third prefix on top of the two §3.23 already
objects to.

**What this replaced.** The condition's only record was an English sentence. Every consumer
detected it by substring — `tests/integrity/ambiguity_detection_test.go` still contains
`strings.Contains(replayResult, "[AMBIGUOUS]")` — so rewording the message would have silently
switched the detection off. That is the same shape as the defects in [[verify-by-running]]: a
real signal attached to something that was never meant to carry it.

**Two things measured while fixing it, both of which contradicted a comment in the tree.**

- The integrity test asserted that a propagated ambiguity "comes back as part of the JSON result
  string, not as a Go-level error from `Engine.Replay`". Measured 2026-08-31: all five
  propagating steps come back through `replayErr`, none through `replayResult`. §3.22 changed
  that and the comment was never updated. Corrected in the same PR.
- Of the two returns in `executor.go`'s guest-failure branch, only the `wasmTrapError` one is
  reachable here — `resolveWasmTrap` returns "" only for an empty message. Removing
  `classifyFailure` from the *other* return does not fail the regression test. It is kept as
  defence, and this sentence is the disclosure that it is uncovered rather than verified.

Re-derive:

    go test ./engine/ -run 'TestAmbiguousReplayCarriesErrAmbiguous|TestAmbiguousClassificationPreservesMessage|TestClassifyFailureLeavesOtherFailuresAlone|TestRecordAmbiguityKeepsTheFirst' -v

Each of the three production edits was mutation-tested; removing `recordAmbiguity`, removing
`classifyFailure` from the trap return, or removing the `Error()` passthrough each fails a named
test, and the first two fail with the pre-fix reality in the message: `error_code='unknown'` on
a `*engine.wasmTrapError`.

---

### 3.54 Every released `cleat-worker` binary was dead on arrival — ✅ **FIXED** (2026-09-01)

`.goreleaser.yml` built `cleat-worker` with `CGO_ENABLED=0`. That compiles out
`engine/backend_wasmtime.go` (`//go:build cgo`), so `NewWasmtimeBackend` resolves to the stub in
`engine/backend_wasmtime_stub.go` and returns `ErrWasmtimeCGOUnavailable`.
`cmd/cleat-worker/main.go:789` logs *"wasmtime is the only WASM backend cleat has, there is no
fallback"* and calls `os.Exit(1)`.

So every `cleat-worker` ever attached to a GitHub release **exited 1 before reading a flag or
opening a database.** Not a degraded worker — a worker that does not start.

Measured 2026-09-01:

    CGO_ENABLED=0  ->  NewWasmtimeBackend err = "wasmtime backend requires CGO
                       (binary built with CGO_ENABLED=0)"
    CGO_ENABLED=1  ->  err = <nil>

Re-derive:

    CGO_ENABLED=0 go build -o /tmp/w0 ./cmd/cleat-worker && /tmp/w0 --verify-backend ; echo $?
    CGO_ENABLED=1 go build -o /tmp/w1 ./cmd/cleat-worker && /tmp/w1 --verify-backend ; echo $?

**Why it lived for months.** `CGO_ENABLED=0 go build ./...` exits 0 — the failure is at startup,
not at build time — so the release pipeline was green the entire way. Nothing ever executed the
artifact it published.

**It was already fixed once, in the other place.** The `Dockerfile` had been changed to
`RUN CGO_ENABLED=1 go build ...` and grew a `RUN /cleat-worker --verify-backend` gate, with a
comment explaining that the image used to be CGO-less. goreleaser was missed. The container was
fine and the tarballs were not, with nothing tying the two together — the same
fixed-in-one-place-not-the-other shape as §3.50 and §3.53.

**The fix, and the decision inside it.** `cleat-worker` is now `CGO_ENABLED=1` and **linux only**.
A CGO darwin binary cannot be linked on `ubuntu-latest` without osxcross, and every job in
`.github/workflows` runs `ubuntu-latest` — 36 of 36, measured 2026-09-01 with

    grep -rhoE 'runs-on: .*' .github/workflows/ | sort | uniq -c

Two binaries that provably start beat four that provably do not. `cleat` and `cleat-gen` are
unaffected: neither links wasmtime, both stay pure Go on all four targets. macOS users install
the worker with `go install`, which is the path `README.md:149` actually documents and which
builds locally with CGO on, or use the Docker image. Re-adding darwin needs a macOS runner, not
an edit to the `goos` list. A Homebrew formula that builds from source is the natural way to give
macOS a working worker back; it is deliberately not in this PR.

`wasmtime-go` v44 vendors static libs for all four targets (`linux-{x86_64,aarch64}`,
`macos-{x86_64,aarch64}`), so this is a linker constraint, not a missing-library one — a darwin
build works fine *on* a Mac. Confirmed by cross-compiling darwin/amd64 from darwin/arm64 with
`CC="clang -target x86_64-apple-macos11"`, which produced a working `Mach-O 64-bit executable`.

**Two guards, at two different costs.**

`scripts/verify-release-worker.sh` runs as a goreleaser post-build hook and **executes every
published artifact** with `--verify-backend`; post hooks run before the publish stage, so a
failure aborts the release. It has no skip branch — cross-architecture execution is a hard
requirement, and `release.yml` registers binfmt via `docker/setup-qemu-action` so the arm64 binary
runs under qemu. A "cannot execute this architecture, skip" branch would pass the arm64 binary
through unexamined and report success, which is the original bug wearing a skip's clothing.

Its own negative control, measured 2026-09-01 (CLAUDE.md: *a verification script needs its own
negative control*): `CGO_ENABLED=1` → exit 0, `CGO_ENABLED=0` → exit 1 with
`wasmtime backend requires CGO`.

`cmd/cleat-worker/goreleaser_cgo_test.go` is the cheap half: it parses `.goreleaser.yml` on every
PR, so a reintroduction is caught in review rather than at the next tag. It matches the build
block on `main:`, not on `id:`, because an id is a label a rename can turn every assertion into a
vacuous pass. Five mutations, each failing for its own reason, and the fifth is the vacuous-pass
control:

| mutation | result |
|---|---|
| `CGO_ENABLED=0` | `TestReleasedWorkerIsBuiltWithCGO` — "released with CGO_ENABLED=0" |
| `CGO_ENABLED` line deleted | `TestReleasedWorkerIsBuiltWithCGO` — "sets no CGO_ENABLED at all" |
| `darwin` re-added to `goos` | `TestReleasedWorkerIsLinuxOnly` — `released for goos "darwin"` |
| verify hook removed | `TestReleasedWorkerArtifactsAreExecuted` |
| `main:` renamed | **all three** fail with "no build has main: ./cmd/cleat-worker" |

Re-derive: `go test ./cmd/cleat-worker/ -run TestReleasedWorker -count=1`

**Stale prose fixed in the same PR.** `cmd/cleat-worker/verify_backend.go` told the operator the
binary "would fall back to wazero, where `--wasm-instance-timeout` cannot interrupt a WASM
guest". That stopped being true when the wazero *backend* was deleted in #459: there is no
fallback, the worker exits 1. The message now says so. It is the text a release engineer reads at
the moment the gate fires, and it was describing a failure mode the code no longer has.
### 3.55 Durable promises could not link on the worker — ✅ **FIXED** (2026-09-01)

**Found by the guard in the same PR, on its first run.** That is the point of the entry: the
decision to keep wazero (§3.56 below) was justified on the grounds that a second implementation is
harmless if something checks it. Something checked it, and the two implementations disagreed.

`engine/wasmtime_hostfuncs_plugins.go` registered `cleat_create_promise` with a fifth parameter,
`ttlMs int64`, and discarded it with `_ = ttlMs`. An arity mismatch is a hard link error, so a
guest calling `cleat_create_promise` **could not instantiate on the wasmtime backend at all** —
and the worker runs wasmtime exclusively. Measured 2026-09-01 through the production path
(`wasm.NeededEnvImports` → `registerAllImports` → `linker.Instantiate`):

    incompatible import type for `env::cleat_create_promise`
    types incompatible: expected type `(func (param i32 i32 i32 i32) (result i64))`,
                           found type `(func (param i32 i32 i32 i32 i64) (result i64))`

**Four params is correct, and everything except this one registration already agreed.** `ABI.md`
2.34 specifies `(param i32 i32 i32 i32) (result i64)`; the Go generator (`wasm/generator.go`:
`name` + `promise_id_out`), the Rust SDK (`crates/cleat-sdk/src/host_calls.rs`), the Java SDK
(`HostCalls.java`) and AssemblyScript (`packages/cleat-as/assembly/host-calls.ts`, whose comment
spells the signature out) all emit four, as does wazero's `engine/imports.go`.

The WIT interface (`python-sdk/wit/cleat.wit`) does carry a `ttl-ms: u64`, which is presumably
where the parameter came from. But that is the component path: `wasm.RewriteWitImports` rewrites
import *names* only, and the canonical lowering of `func(name: string, ttl-ms: u64) -> string`
would be `(i32, i32, i64, i32)` — neither this shape nor wazero's. The fifth parameter never
matched a real guest on any path.

**Two reasons it survived, and both are more interesting than the defect.**

1. **A test had codified it.** `TestClosure_CreatePromise` declared its guest with five
   parameters — written to fit the host rather than the spec. Host and test agreed with each
   other and with nothing else, so the suite was green while every real guest failed. Fixed to
   four in the same PR, with a comment saying why.
2. **Durable promises have no end-to-end coverage through a real guest on the production
   backend.** Re-derive: `grep -rln 'CreatePromise\|create_promise' examples/ tests/` returns
   `tests/soak` and `tests/integrity` (both handler-level, using `EventTypeCreatePromise`) and
   `examples/widget-store-as`. Nothing compiles a guest that calls it and runs it on wasmtime.
   **That gap is still open** — this PR adds a link-level regression test, not an execution one.

**The split that hid it.** wazero was correct, and wazero is what `cleat/wasmtest`, `cleat run`
and `cleatctl replay` use. wasmtime was wrong, and wasmtime is what the worker uses. Each was
self-consistent, so no single-runtime test could see it. This is the same shape as every other
defect this week: a signal that existed and was attached to the wrong thing.

Re-derive: `go test ./engine/ -run 'TestCreatePromiseGuestLinksOnTheWorkerBackend|TestClosure_CreatePromise' -count=1`

### 3.56 The host ABI is written twice, and now something checks it — ✅ **FIXED** (2026-09-01)

**Decision, 2026-09-01: wazero stays, scoped to CLI and dev tooling.** This closes
"wazero removal, part 2" in `REMEDIATION-PLAN-2026-08-09.md` as *will not do*, with reasons:

- **The safety case is gone.** The worker never constructs a wazero `Runtime` — `grep` for
  `NewRuntime` in `cmd/cleat-worker/*.go` is empty — and #503 made an unroutable language fail
  closed rather than fall back. What remains on wazero is `cleatctl replay|debug`, `cleat run`,
  `cleat-bench` and `cleat/wasmtest`.
- **The cost is real.** All three released binaries build CGO-less today. Removing wazero forces
  `cleat` onto wasmtime and therefore onto CGO, ending pure-Go cross-compilation for the CLI —
  see §3.54 for what that constraint costs in practice. It also breaks public API:
  `engine.Runtime`, `engine.NewRuntime` and `wasmtest.WasmTestEnv.Runtime()` are exported.

**The price of that decision is a guard, because two implementations that nothing compares will
drift.** They already had: `engine/imports.go` and `engine/wasmtime_hostfuncs*.go` register the
same 56 names, and the arity of one of them disagreed (§3.55).

`engine/hostabi_runtime_parity_test.go` (`//go:build cgo`), three tests:

- **`TestWasmtimeSatisfiesEveryWazeroHostImport`** — enumerates wazero's real host module via
  `ExportedFunctionDefinitions()`, generates a WAT module importing all 56 at exactly those
  signatures, and instantiates it against the wasmtime backend's real linker. A conformance test,
  not a text comparison: neither side's source is read, both registration paths are the
  production ones, and wasmtime names any offender itself.
- **`TestCreatePromiseGuestLinksOnTheWorkerBackend`** — the §3.55 regression test, asserting the
  *documented* signature rather than whatever the host declares.
- **`TestNeitherRuntimeHasHostFunctionsTheOtherLacks`** — closes the direction the first cannot
  see. A linker with spare definitions instantiates a narrower module happily, so a
  wasmtime-only function is invisible to instantiation. Extracted with `go/ast` over
  `linker.FuncWrap`/`FuncNew` calls.

**A name-only comparison would have been a vacuous pass.** The `comm`-over-`grep` check that
motivated this guard reported the two sets as identical — which they were. The defect was in a
signature, one level below what that check could see.

Mutation-tested, five ways:

| mutation | caught by |
|---|---|
| restore `ttlMs` (the original defect) | conformance **and** regression test |
| wazero `cleat_now` result `i64` → `i32` | conformance test, `env::cleat_now` |
| add a wasmtime-only `cleat_fake_new_op` | **set test only** — conformance passed |
| `wasmtime_hostfuncs*.go` glob unmatched | `t.Fatalf`, not a silent zero |
| either side falls below 40 functions | floor check |

The third row is why the set test exists: the conformance test passed, silently.

Re-derive: `go test ./engine/ -run 'TestWasmtimeSatisfies|TestNeitherRuntime|TestCreatePromiseGuest' -count=1`

---

### 3.57 macOS gets a working `cleat-worker` back, via Homebrew — ✅ **FIXED** (2026-09-01)

§3.54 stopped the release shipping four `cleat-worker` binaries that exited 1 at startup, at the
cost of dropping macOS: a CGO darwin binary cannot be linked on `ubuntu-latest` without osxcross,
and all 36 workflow jobs run ubuntu. This closes the hole that left, without adding a macOS runner
to CI.

**A source-build Homebrew formula moves the CGO link to the install machine, which is the one
place it is free.** Homebrew already requires the Xcode Command Line Tools, so a C toolchain is
guaranteed to be present. `packaging/homebrew/Formula/cleat.rb` builds `cleat-worker` with
`CGO_ENABLED=1` and `cleat`/`cleat-gen` with `CGO_ENABLED=0`, matching how `.goreleaser.yml`
ships the latter two so a Homebrew user and a tarball user get the same thing.

**goreleaser's `brews:` generator cannot be used here**, which is worth stating because it is the
obvious tool and it does not fit: it packages *built binaries*, and the missing macOS
`cleat-worker` binary is precisely the problem. A formula that repackages the release archives
would reproduce the gap it is meant to close.

**Verified end to end on darwin/arm64, 2026-09-01**, against the published v0.2.0 source tarball
(`sha256 40fc9126…`):

| step | result |
|---|---|
| `brew style` | no offenses |
| `brew audit --strict` | no findings |
| `brew install --build-from-source` | exit 0 |
| `file $(brew --prefix)/bin/cleat-worker` | `Mach-O 64-bit executable arm64` |
| `cleat-worker --verify-backend` | `OK: wasmtime backend available`, exit 0 |
| `brew test` | exit 0 |

Installed, tested, then `brew uninstall` / `brew untap`. **The first `brew test` failed**, and for
a real reason worth recording: the test block ran `system bin/"cleat-gen", "--help"`, but
`cleat-gen` has no `--help` and no zero-exit invocation at all — with no arguments it prints usage
and exits 1. Now asserted as `shell_output("#{bin}/cleat-gen 2>&1", 1)`, which pins the exit
status rather than assuming it.

**Note on `brew style`.** Run against `packaging/homebrew/cleat.rb` it reported three offenses
(`Sorbet/StrictSigil`, `Sorbet/TrueSigil`, `Style/FrozenStringLiteralComment`). Those are
path-detection artifacts, not defects: the same file at a `Formula/` path inspects clean. Hence
`packaging/homebrew/Formula/cleat.rb`. Re-derive by copying the file to a directory not named
`Formula/` and running `brew style` on it.

**What the guard covers, and what it cannot.** `packaging/homebrew/formula_test.go` runs in normal
CI and checks the things a release bump gets wrong silently: that `url` names a tagged tarball
rather than a branch, that a 64-hex `sha256` is present, that `CGO_ENABLED=1` is set *around the
worker build* rather than merely somewhere in the file, and that the test block both runs
`--verify-backend` and asserts on its output.

It **cannot** check that the `sha256` matches the tarball — that needs the network. That is a
release step, recorded in `docs/project/release-process.md` §3.

Mutation-tested, five ways:

| mutation | result |
|---|---|
| `CGO_ENABLED: "1"` → `"0"` | `TestFormulaBuildsTheWorkerWithCGO` |
| worker build moved outside the CGO=1 block | `TestFormulaBuildsTheWorkerWithCGO`, second assertion |
| test block stops running `--verify-backend` | `TestFormulaTestBlockExecutesTheWorker` |
| `url` points at a branch | `TestFormulaPinsATaggedSourceTarball` |
| formula file missing | **all three** `t.Fatalf` rather than passing |

Re-derive: `go test ./packaging/homebrew/ -count=1`

**Still open:** there is no published tap, so the formula is installed from a path in a clone
(`brew install --build-from-source packaging/homebrew/Formula/cleat.rb`). A
`cleat-team/homebrew-cleat` tap would make it `brew install cleat-team/cleat/cleat`; creating that
repository is a decision for the maintainers, not something to do in passing. README.md says so
rather than implying a tap exists.

---

### 3.59 Durable promises: linking was tested, meaning was not — ✅ **FIXED** (2026-09-01)

§3.55 fixed `cleat_create_promise`'s arity and added a link-level regression test. Linking is the
first of the things that have to be right, not all of them, and this closes the rest of the gap
that entry left open.

**Why the existing test could not see it.** `TestClosure_CreatePromise` already drives a guest
through the wasmtime backend, but `newClosureSetup` installs `mockHostHandler{ret: 0}`, whose
`CreatePromise` records the *name* and returns `h.ret`. So its assertion is `got == 0` — and the
real handler never returns 0 on success: `engine/promises.go:22` returns
`packSimpleResult(0, written)`, which is `written<<32`. Nothing checked that the promise ID
reached the guest's buffer, and nothing checked *which* pointer the wrapper passed.

**Subject and collaborator.** The code under test is the wasmtime host-function wrapper in
`engine/wasmtime_hostfuncs_plugins.go`, not the handler. The handler is therefore a fake — but one
that behaves the way `engine/promises.go` does, writing through `ctx.Value(wasmMemBufKey{})` and
packing its result identically, so a wrapper that swaps arguments or drops the return fails here.

`engine/create_promise_abi_test.go` asserts what survives the boundary: the name in, both
output-buffer arguments **in the right order**, the ID back into guest memory, the packed return
unpacked the way a guest unpacks it, and — separately — that the host respects the guest's
declared capacity, with sentinel bytes painted past the buffer so an overrun is observed rather
than inferred.

Mutation-tested against the wrapper, three ways:

| mutation | caught by |
|---|---|
| swap `promiseIDPtr` / `promiseIDMaxLen` | `promiseIDPtr = 64, want 200`; `promiseIDMaxLen = 200, want 64`; and the ID never reaches guest memory |
| drop the handler's return, `return 0` | `call returned 0x0, want 0xf00000000`; and `reported 0 bytes written, want 4` |
| stop passing memory on the context | "the wrapper did not put the guest's linear memory on the context" |

The first is the point. An argument swap **links fine** — both are `i32`, both in range — so §3.55's
link-level test passes and the guest reads a buffer the host never wrote. This is the class
CLAUDE.md names: *"in every case the value meant the wrong thing on one side of the boundary."*

Re-derive: `go test ./engine/ -run TestCreatePromiseABI -count=1`

**Still open, and narrower than before.** No test compiles a *real SDK guest* that calls
`create_promise` and runs it on the worker. What is covered now is the host side of the boundary
for one operation; the other output-buffer host calls have the same shape and the same absent
coverage.
### 3.58 The release path was only ever exercised by a release — ✅ **FIXED** (2026-09-01)

§3.54 fixed `cleat-worker`'s release build (`CGO_ENABLED=0` produced binaries that exited 1 at
startup) and added `scripts/verify-release-worker.sh` as a goreleaser post-build hook. But
`.github/workflows/release.yml` runs only on a `v*` tag, so the two genuinely new pieces — the
`aarch64-linux-gnu-gcc` cross-compile and executing an arm64 binary under qemu — would first have
run during an actual release. A toolchain mistake there is a blocked or broken release, not a red
check on a PR.

**The `Release Dry Run` job** runs `goreleaser build --snapshot --clean` on every PR with the same
cross compiler and `docker/setup-qemu-action` the release uses.

**`goreleaser build` runs post hooks — verified rather than assumed.** Temporarily attaching
`sh -c 'echo POSTHOOK-RAN-FOR {{ .Path }}'` to the `cleat` build and running
`goreleaser build --snapshot --clean --single-target` printed

    running hook  hook=sh -c 'echo "POSTHOOK-RAN-FOR .../dist/cleat_darwin_arm64_v8.0/cleat"'

So `verify-release-worker.sh` really does execute both published workers in the dry run. Without
that check the job would have looked like coverage while running no hook at all.

**A second assertion, because the hook has a blind spot.** The hook runs `--verify-backend` on an
x86-64 runner; a cross-compile that silently produced x86-64 for the arm64 target would pass it.
The job therefore also checks `file` output per target, and **counts the binaries it inspected** —
a `find | while read` loop that matches nothing exits 0, which is the "checks never started" shape
CLAUDE.md is about, so fewer than 2 fails.

**It failed on its first run, and the defect was in the release path itself.** The arm64 post
hook died with

    qemu-aarch64: Could not open '/lib/ld-linux-aarch64.so.1': No such file or directory

binfmt *was* registered — qemu started, and the amd64 hook had already passed. The problem is
that `gcc-aarch64-linux-gnu` links an aarch64 binary without installing the loader that binary
needs, so there was nothing for qemu to exec. **`release.yml` carried the identical gap**, so the
next tag would have aborted the same way — which is precisely the failure this job exists to move
off the release path. Fixed in both workflows with `libc6-arm64-cross` and
`QEMU_LD_PREFIX=/usr/aarch64-linux-gnu`.

Worth noting how it presents: the script's own diagnostic said *"This binary cannot construct the
wasmtime backend"*, which was wrong — the binary never reached its own `main`. The message now
names the missing-sysroot case explicitly, because a correct failure with a misleading
explanation costs as much as a wrong result.

**Scope, stated rather than implied.** `build` stops before archives, checksums, changelog and
upload, and does not run `Build Svelte UI` or `Validate no dirty dist/` — a stale dashboard is
still only caught at tag time. Recorded in `docs/project/release-process.md` §4a.

**Not a required check.** `.github/required-checks.txt` mirrors branch protection, and adding a
line there without changing the repository setting would be a doc that lies. Making it blocking is
a maintainer settings change.

`dist/` was not in `.gitignore` (`git check-ignore -v dist/probe` → no match, `git status` → `?? dist/`).
Now ignored, since this change makes `goreleaser build` the documented local reproduction.

---

### 3.60 §3.52's fix left a 50/50 race that discarded the error it connected — ✅ **FIXED** (2026-09-01)

§3.52 connected `InitModule`'s `errCh`, which had only ever been written on panic while wazero
signals a trap by returning an error. The plumbing was right and **the consumer was not**.

`InitModule` waits on two channels. The goroutine sends the failure on `errCh` (buffered, cap 1)
and only then runs the deferred `close(done)`, so on a trap **both select cases are ready at
once** and Go picks uniformly at random. `case <-done: return nil` therefore threw the error
away about half the times the poller reached that select — reinstating, in a different shape,
the exact defect §3.52 existed to fix.

Fixed by draining `errCh` with a non-blocking receive before treating a closed `done` as success.
The send happens-before the close, so anything there is visible.

**How it was found, and why that matters.** It surfaced as a CI failure of
`TestInitModuleReportsATrappingStart` on a **docs-only PR** (#521) — a test failing on a change
that touched no code is the signal, and re-running would have buried it.

**The behavioural test is a ~2% detector.** Measured 2026-09-01 with the fix removed:

| run | failures |
|---|---|
| `TestInitModuleReportsATrappingStart`, default GOMAXPROCS | 0 / 40 |
| same, `-cpu 1` | 4 / 200 |
| a 60-iteration repetition of the same call, `-cpu 1` | 1 / 30 |

On an idle machine the trap lands in `errCh` before the first 100µs backoff elapses and an
earlier select catches it, so the window never opens. That is why 40 local runs were green while
CI was not.

**A repetition test was written to make it deterministic and was not** — 1 failure in 30 — so it
was **deleted rather than shipped**. A test named `...EveryTime` that detects 3% of the time is
worse than no test: it reads as deterministic, and would be re-run until green and then believed.
Recording this because the tempting move was to keep it.

What *is* deterministic is the shape of the code, so the regression test is a source guard:
`engine/init_module_done_branch_guard_test.go` walks `InitModule` with `go/ast` and requires
every `case <-done:` clause to consult `errCh`. Mutation-tested — reverting to
`case <-done: return nil` fails **5 of 5** runs naming `runtime.go:297:4`, and the restored code
passes 5 of 5. Its vacuous-pass control fires too: pointed at a file with no `InitModule`, it
`t.Fatal`s rather than reporting success over nothing.

This is CLAUDE.md's rule applied to its own exception — *"proving a test can fail catches one
that cannot fail; it does not catch one that fails sometimes."*

Re-derive: `go test ./engine/ -run 'TestInitModuleDoneBranchConsultsErrCh|TestInitModule' -count=1`

---

### 3.61 The output-buffer ABI, as a property rather than 31 tests — ✅ **FIXED** (2026-09-01)

§3.59 gave `cleat_create_promise` a behavioural test after §3.55 found the host registering it
with a parameter no guest passed. **31 of the 56 `cleat_*` host functions take a
`(ptr, maxLen)` output-buffer pair**, and that PR covered one of them.

The mutation that mattered in §3.59 was **swapping `promiseIDPtr` with `promiseIDMaxLen`**: both
are `i32`, both in range, so the module still links, the host writes at an offset the guest never
nominated, and the guest reads a buffer nobody filled. Nothing in the suite would have seen that
in any of the other 30.

CLAUDE.md names the right shape for this layer — *"a backlog of 200 similar findings is usually
one missing abstraction, not 200 fixes … which a property test over that boundary would find
faster than reading the remaining sites."*

`engine/abi_output_buffer_property_test.go` asserts the invariant every wrapper shares, over
every wrapper: **the closure's last two parameters are `(ptr, maxLen)`, and they reach the
handler as its last two arguments, in that order, with the guest's memory on the context.**

Verified to hold with zero deviations before being encoded: of 56 `FuncWrap` registrations, 31
end in a `(Ptr, MaxLen)` pair and **none** ends in a `MaxLen` whose preceding parameter is not its
`Ptr`. The 5 host calls with no `*wasmtime.Caller` — `cleat_now`, `cleat_random`, `cleat_sleep`,
`cleat_version`, `cleat_min_version` — take no buffer.

Mutation-tested, including on calls §3.59 does **not** cover:

| mutation | result |
|---|---|
| swap ptr/maxLen in `cleat_get_state` | `SWAPPED — the handler receives (valueMaxLen, valuePtr)` |
| swap ptr/maxLen in `cleat_uuid` | `SWAPPED — the handler receives (uuidMaxLen, uuidPtr)` |
| stale `directWriters` entry | named and failed |
| break output-buffer recognition | `only 0 wrappers were recognised … expected at least 25` |
| glob matches no files | `t.Fatal` |

**Two things the test got wrong first, both worth recording.**

It reported four wrappers as making no handler call. They were fine: `cleat_call`,
`cleat_poll_work`, `cleat_json_parse` and `cleat_json_stringify` bind `callCtx := ctxWithMem(...)`
to a local instead of passing it inline, and the detector only matched the inline form. **A test
going red is not evidence the code is wrong** — three of those four were a defect in the test.

The fourth, `cleat_poll_work`, genuinely differs: it copies into the guest's memory itself and
calls no handler, so the argument-order property cannot apply. It is allowlisted in
`directWriters`, and the allowlist is asserted **exact in both directions** so that a wrapper
which starts or stops writing directly is a decision rather than a silent gap.

**Named here, closed in §3.62:** `cleat_poll_work` is the least-covered call in this file and the
one where a mix-up is most dangerous — it takes **two** output buffers, `(entryNamePtr,
entryNameMaxLen)` and `(argsPtr, argsMaxLen)`, and copies into both with hand-written bounds.
The bounds turned out not to exist.

**What this does not do.** It reads source, so it cannot catch a wrong value passed in the right
position. That is what §3.59's behavioural test does, for one call. Breadth here, depth there,
deliberately.

Re-derive: `go test ./engine/ -run TestOutputBufferHostCallsPassPtrAndMaxLenInOrder -count=1 -v`

### 3.62 `cleat_poll_work` wrote to guest pointers it never checked — ✅ **FIXED** (2026-09-01)

§3.61 allowlisted `cleat_poll_work` out of the output-buffer property because it copies into
guest memory itself instead of delegating to the handler, and named it as the one call still
needing a behavioural test of its own — "copies into both with hand-written bounds". Reading it
to write that test: **there were no bounds.**

```go
copy(buf[entryNamePtr:entryNamePtr+int32(entryLen)], entryBytes[:entryLen])
copy(buf[argsPtr:argsPtr+int32(argsLen)], b.workInput[:argsLen])
```

`entryNamePtr` and `argsPtr` come straight off the guest's stack as `i32`. Every other writer in
the wasmtime host layer checks: `wasmtimeWriteString` compares `uint64(ptr)+uint64(len)` against
`len(buf)` (`engine/wasmtime_memory.go`), `cleat_complete` does the same inline before slicing
(`engine/wasmtime_hostfuncs.go`), and `writeResult` was fixed explicitly with a comment naming
this class (`engine/flush.go`). This was the only one that did not, out of 31 output-buffer
calls.

Measured 2026-09-01 against a one-page (65536-byte) guest, via a probe module forwarding its four
parameters to the import:

| call | before |
|---|---|
| `entryNamePtr = -1` | `panic: slice bounds out of range [-1:]` |
| `entryNamePtr = 65530`, `entryNameMaxLen = 64` | `panic: slice bounds out of range [:65538] with capacity 65536` |
| `argsPtr = 65530`, `argsMaxLen = 1024` | `panic: slice bounds out of range [:65551] with capacity 65536` |

A negative pointer is the interesting one: it slices *backwards*, so a bounds check written in
signed arithmetic would not catch it either. The fix interprets the pointer as an unsigned WASM
address (`uint64(uint32(ptr))`), which is what it is.

**Severity, stated precisely rather than dramatically.** This is **not** a worker crash. All
three sites that call into a guest — `backend_wasmtime.go` at the Go `_start` branch, the
direct-export branch, and the component branch — wrap the call in a `recover`, so the panic
became a failed workflow reporting `wasmtime panic in "<entryPoint>"`. Two things are still
wrong with that: the message names the *guest's* entry point, so the fault looks like guest code
rather than a host function handed an argument it never validated; and those recovers were added
for "wasmtime-go internal panics", not for this — relying on them leaves every future direct
writer one unguarded `copy` away from the same thing.

A fourth defect, found while writing the test: a **negative** `entryNameMaxLen` went into
`if entryLen > int(entryNameMaxLen)` and set `entryLen` to `-1`. The copy was skipped (guarded by
`entryLen > 0`) but `-1` was still packed into the returned length, and the guest reads that high
word as unsigned — 0xFFFFFFFF bytes of entry name supposedly available.

**Fix.** Two helpers in `engine/wasmtime_memory.go`, `clampToMaxLen` and `guestRangeOK`, and both
ranges checked before *either* copy, so the call is all-or-nothing: a guest that gets its second
pointer wrong does not come back to a half-populated first buffer.

**Test.** `engine/poll_work_guest_pointer_test.go`. Four refusal cases, each proven to fail with
the fix removed and for the expected reason — the panic text in the table above. They must be run
one at a time to see all four, because before the fix the first one killed the test binary.

Two controls, both of which pass with the fix removed as well, and are there to stop the refusal
being satisfied by refusing too much:

- `TestPollWorkDeliversWorkAtValidPointers` — the ordinary call every Go guest makes. Without it,
  `return errBadParamInt64` unconditionally would make the file green and no Go workflow could
  start.
- `TestPollWorkTruncatesToTheCapacityTheGuestDeclared` — a short-but-valid buffer must still be
  filled and truncated, and a **sentinel byte** immediately past it must survive. Comparing the
  buffer's contents cannot see a one-byte overrun; the sentinel can.

**A note on the harness, which measured nothing on its first run.** `NewWasmtimeBackend` calls
`cfg.SetEpochInterruption(true)`, so a store built with a bare `wasmtime.NewStore` has an epoch
deadline of 0 and *every* call traps with `wasm trap: interrupt` before reaching the host
function — including the happy path. The harness calls `b.configureStore` and asserts guest
memory is the expected 65536 bytes, since the out-of-range pointers are chosen relative to it.

Re-derive: `go test ./engine/ -run TestPollWork -count=1 -v`

### 3.63 A cleanup pass was bounded per defer, not in total — ✅ **FIXED** (2026-09-01)

Closes the measurement §3.31 had deliberately left as "read off the code, not measured", and
fixes what measuring it found.

Every caller of `runDefers` passes `context.Background()`, on purpose: cleanup must still happen
when the workflow's own context has timed out or been cancelled. The consequence nobody had run
is that each `RunDefer` then reaches `configureStore` with no deadline to reconcile against, so
**each defer gets a fresh copy of the backend's per-invocation budget** and the total scales with
the number of defers, with no ceiling.

Measured 2026-09-01, backend timeout 2s:

| | elapsed | budget the host reported |
|---|---|---|
| `RunDefer` with a 200 ms ctx deadline | 150 ms | `199.778625ms wall-clock budget` |
| `RunDefer` with `context.Background()` | 2.001 s | `2s wall-clock budget` |
| `runDefers(Background)`, 3 runaway defers | **6.001 s** | `2s`, `2s`, `2s` |

On a worker the per-invocation budget is `DefaultWasmtimeExecutionTimeout` = 30s
(`--wasm-instance-timeout`), so twenty runaway defers held a worker slot for ten minutes.

**Fix — one `WithTimeout` over the loop.** `configureStore` already takes the tighter of ctx's
remaining time and the backend's own timeout, so a single deadline shared by every iteration
bounds both the pass and each defer within it, without touching the base context and therefore
without re-coupling cleanup to the workflow's cancellation. `DefaultDeferPassBudget` is 5
minutes, settable with `engine.WithDeferPassBudget`.

**The default is deliberately generous, and that is the point.** The bound that was missing is an
*aggregate* one. Five minutes leaves every plausible legitimate cleanup pass untouched while
turning "unbounded in N" into a fixed ceiling; choosing something tight enough to also shorten a
single slow defer would have been a second, unrelated behaviour change smuggled into this one.

**The assertion is on the reported budgets, not on the clock.** With one shared deadline the
first defer gets the full backend timeout, the second whatever remains, the third essentially
nothing — so **at most one defer can report the full per-invocation budget**. Without the fix all
three do. CLAUDE.md asks for timing to be removed from assertions rather than widened; here the
subject *is* wall-clock, so the timing was moved into a value the code under test reports rather
than one measured from outside. Proven to fail with the fix removed:

    3 of 3 defers were given the full 2s per-invocation budget; at most 1 can be.
    budgets reported: [2s 2s 2s]
    the pass took 6s for a 3s budget.

Elapsed time is still checked, with a 5s threshold against a 6s-vs-3s gap, purely so a pass that
reports the right budgets while still running long cannot slip through.

**Controls**, in `engine/defer_pass_budget_test.go`:

- `TestDeferPassRunsEveryDeferWhenTheyFit` — the important one. "Bound the pass" is trivially
  satisfiable by running fewer defers, which would silently drop the cleanup this path exists to
  perform. Three prompt defers under the same budget must produce no failure log.
- `TestRunDeferHonoursATighterContextDeadline` — pins the mechanism the fix rests on. If
  `configureStore` stopped preferring the tighter deadline, the aggregate bound would mean
  nothing *and would keep passing*, because a pass that ignores its deadline still reports one
  budget per defer.
- `TestRunDeferWithoutADeadlineFallsBackToTheBackendBudget` — characterises the per-call
  behaviour as correct, so the two halves stay visible together: the per-call answer was fine,
  the multiplication was not.

**Out of scope, and named rather than left implicit.** `invokeDefersOnTrap` runs defers on the
still-live module via `e.rt` — wazero — and is unfenceable for a compute-bound guest (CLAUDE.md,
measured three ways). It sits in `executeCompiled`, the path taken only by engines built with a
Runtime and no backends: `cleatctl replay|debug`, `cleat run_embedded`, `cleat-bench`,
`cleat/wasmtest`. The worker returns from `executeWithBackend` and never reaches it.

Re-derive: `go test ./engine/ -run 'TestDeferPass|TestRunDefer' -count=1 -v`

### 3.64 A defer body ran twice after a trap — ✅ **FIXED** (2026-09-01)

§3.35 finding 3, confirmed by running rather than by reading, and fixed. Its other two findings
were re-checked at the same time; finding 1 was already closed, on the day it was written.

`executeCompiled`'s non-suspend-error branch called `invokeDefersOnTrap` (the still-live module)
and then `runDefers` (a fresh module), unconditionally, under a comment reading *"Try running
defers on the still-live module first, then fall back to fresh-module defers."* **There was no
conditional.**

Measured 2026-09-01 with a guest that registers one defer and traps, whose defer body also
traps — the trapping body is what makes execution observable, since a defer that returned
cleanly would log nothing:

    "defer execution failed" defer_id=defer-0 description=cleanup
        error="wasm trap: unreachable ... .$2(i32,i32,i32,i32) i64"
    "defer execution failed" workflow_id=wf-probe defer_id=defer-0 export=cleat_defer_defer-0
        error="wasm trap: unreachable ... .$2(i32,i32,i32,i32) i64"

Two records, same `defer_id`, and each carries the trap raised by the *defer function* (`$2`),
so both had reached the body. A defer is a destructor, so a doubled body is a doubled effect: a
compensating saga step applied twice, a lock released twice, a notification sent twice.

**Scope, stated so nobody has to re-derive it: this is not the worker.** `executeCompiled` is
reached only when no backend is registered — `Replay` and `Execute` both route to
`executeWithBackend` first. That leaves `cleatctl replay|debug`, `cleat run`, `cleat-bench`, and
the public testing packages `cleat/wasmtest`, `cleat/cleattest`, `cleat/embedded`. **That last
group is the reason it matters rather than the reason it does not**: a user testing a
compensating defer saw it fire twice under the harness and once in production, so the harness
disagreed with the runtime in the direction that makes a real double-compensation look like a
test artifact.

**Fix.** `invokeDefersOnTrap` now returns the deferrals it did *not* invoke, and the caller falls
back for those and only those. **"Invoked" means the export was found and called, whatever the
outcome** — a defer that ran and trapped must not be retried on a fresh instance, because it may
already have applied part of its effect, which is the case where doubling does the most damage.

The other `invokeDefersOnTrap` call site discards the new return value deliberately: it has never
had a fall-back, and adding one there would be a behaviour change rather than this bug fix.

**One guest carries both halves of the property.** It registers two defers and exports a
`cleat_defer_*` for only the first:

| | live-module export | must |
|---|---|---|
| `defer-0` | present | run **once**, not be re-run |
| `defer-1` | absent | still reach the fall-back |

Without the second, "run each defer once" is satisfied by deleting the fall-back entirely and
silently dropping a defer the live module could not offer.

**Three mutations, each caught for its own reason:**

| mutation | what failed |
|---|---|
| fall-back made unconditional again | `defer-0's body executed 2 times, want 1` |
| a trapped defer reported as un-invoked | `defer-0's body executed 2 times` **and** `defer-0 was reported as not invoked, but its export exists and was called -- it trapped` |
| fall-back deleted entirely | `defer-1 reached the fresh-module fall-back 0 times, want 1` |

The second is the plausible-wrong fix — treating any failure as "not invoked" — and it is caught
by both the behavioural test and the unit-level one.

Re-derive: `go test ./engine/ -run 'TestDeferBodyRunsOnceAfterATrap|TestInvokeDefersOnTrap' -count=1 -v`

### 3.65 The component decomposition path, deleted — ✅ **DONE** (2026-09-01)

§3.31's addendum established that decomposition *"has never successfully executed a workflow …
code that is reached and has never once succeeded."* Re-measured before removing anything, and
it is worse than that: there were **two** decomposition implementations, not one, and they fail
at different points on the same binary.

Measured 2026-09-01 against the only Component Model binary in the repo — a 19.3 MB
componentize-py build — with the native path as the control:

| path | result |
|---|---|
| **native** (`ExecuteComponentCGo`) | reached CPython, ran guest code, returned the guest's own `type mismatch: expected string, found bool` from a deliberately wrong input — **it works** |
| **wasmtime decomposition** (`ExecuteComponent`) | failed at instance 81 of 85: `incompatible import type for env::cleat_call` — expected `(param i32 ×7)`, found `(param i32 ×8) (result i64)` |
| **wazero decomposition** (`Engine.executeComponent`) | failed at instance 8: `"memory" is not exported in module "env"` |

The control is what makes this decisive. The fixture is executable, the remaining path executes
it, and both decomposition implementations fail on it in unrelated ways. §3.31 recorded the
wasmtime one failing at instance 81 on 2026-08-05; the second implementation was never measured
at all.

**Deleted, 16 files, −4,005 / +317:**

| | lines |
|---|---|
| `wasmtimeBackend.ExecuteComponent` + `perExportRoute` + `defineWitDylib` | 620 + 100 + 325 |
| `engine/wit_dylib_stack.go` (+ its 624-line test) — the wit_dylib value-stack machine, reachable only from `defineWitDylib` | 664 |
| `Engine.executeComponent` — the wazero twin | 273 |
| `wasm/component.go` (+ its 461-line test) — `ParseComponentBundle` and the section parsers | 642 |
| `Runtime.instantiateModuleNamedWithWriters` | 12 |

**This removes exported API**: `wasm.ParseComponentBundle`, `wasm.ComponentBundle`,
`wasm.ComponentExport`, `wasm.CoreInstance`, `wasm.InstantiateArg`, `wasm.ExportSpec`,
`wasm.PatchEmptyImportModuleName`. Every caller of every one of them was a decomposition path;
after the deletion the file had no users at all. Keeping it would have meant a dead-exports
baseline entry for a parser with nothing to parse for.

**Two things the deletion surfaced that the plan had not.**

- `engine/wit_dylib_stack.go` — 664 lines of value-stack machinery plus 624 lines of tests —
  existed solely for decomposition. `scripts/check-test-only-code.sh` did not flag it, because
  it *was* reached from production code; it was reached from production code that never
  succeeded. That is the shape §3.31's addendum named — "not dead code in the
  `check-test-only-code.sh` sense … something rarer, code that is reached and has never once
  succeeded" — and the guard cannot see it by construction.
- `Runtime.instantiateModuleNamedWithWriters`'s doc said it was *"used by
  wazeroBackend.Execute()"*. That type was deleted in #459. Its real last caller was
  `executeComponent`. `check-test-only-code.sh` caught it the moment that went, which is the
  guard working as designed on the one it *could* see.

**What a caller meets now.** A native-path failure used to be a prelude: `Execute` logged it and
then ran decomposition, whose failure was what the caller actually saw — and it described
decomposition's problems assembling the module, reading like "wasmtime cannot run this
component" when the cause was something else. §2.72 records months of that error being taken as
wasmtime's verdict on Component Model guests. The native path's error is now the answer.

On the no-backend route (`cleatctl replay|debug`, `cleat run_embedded`, `cleat-bench`,
`cleat/wasmtest`), `"memory" is not exported in module "env"` is replaced by a message naming
the fix, which for that engine shape is real: register the wasmtime backend.

**Tests** — `engine/component_no_decomposition_test.go`, and the control is the important one:

- `TestComponentOnTheBackendTakesTheNativePathOnly` — a component must still **execute**.
  Without it, "no longer reaches decomposition" is satisfied by a backend that stopped running
  components at all, which is the most likely way to break this and would look like success.
  Python is tier 1.
- `TestComponentFailureIsNotFollowedByASecondWorseError` — the error names the entry point and
  contains none of decomposition's vocabulary (`instantiate instance`,
  `incompatible import type`, `component bundle`).
- `TestComponentWithoutABackendSaysHowToRunIt` — names `Component Model` and `WithBackend`, and
  is not the old instantiation failure.

**Deleted rather than kept:** `TestComponentStdoutStderrRace` covered a concurrency fix inside
`executeComponent`. With that gone the test's `Execute` call returns before instantiating
anything, so it would have passed while exercising nothing — a vacuous test is worse than none,
because it reads as coverage.

**Prose corrected in the same commit** (CLAUDE.md: fix what describes it, not just the marker):
`tiers.yaml` ×4 — the D5 skip-budget rationale, the tier-1 exclusion list, Python's two open
items, and the tier-3 parked entry; `engine/wasmtime_options.go`'s table-limit justification,
which derived its number from `tblMinSize` in the deleted function; `engine/runtime.go`'s struct
comment; `engine/component_fence_test.go`'s "third of three execution paths";
`cleat/wasmtest/wasmtest_backends.go`; `engine/python_wasm_e2e_test.go`.

**§3.31's remaining gap closes by deletion, as it predicted.** "The decomposition path's fence is
inherited rather than verified" needed no test: there is no longer a path to fence. The
`componentGetFunc` prerequisite it named turned out not to be one — see the correction in its
addendum.

Re-derive: `go test ./engine/ -run TestComponent -count=1 -v`

### 3.66 A defer registered before the workflow suspended never ran — ✅ **FIXED** (WS-3, 2026-09-01)

§3.35 finding 4, found while planning §3.35's implementation and shipped ahead of it: it is a
defect under every row of that section's decision table, so it does not wait on the decisions.

`DurableDefer` does two things on the fresh path — records an `EventTypeDefer` event, *and*
adds the ID to `session.deferrals`, which is the live set every terminal-transition call site
actually iterates. Its replay-match branch did only the first, by omission:

```go
if rec.EventType == EventTypeDefer {
    if !s.advanceReplayStep(ctx, &rec) { return 0 }
    written, _ := s.writeResult(ctx, m, deferIDPtr, rec.DeferID, deferIDMaxLen)
    return packSimpleResult(0, written)   // s.deferrals never written
}
```

Every segment after the first replays past the registration, so that branch is the **only** one
a previously-registered defer ever reaches again. The defer was dropped permanently the moment
the workflow suspended once.

**The blast radius is exactly the case defer exists for.** A defer on a workflow that never
suspends runs in the segment that registered it and was unaffected. A defer on a workflow that
sleeps, waits for a signal, or awaits a child — the long-running workflow whose locks and saga
steps are the reason destructors are worth having — was silently lost. Nothing logged:
`session.deferrals` was empty and every call site is guarded by `if len(deferrals) > 0`.

Measured 2026-09-01, replaying a one-event history through the session:

    after replayed DurableDefer: stepCount=1 isReplay=true deferrals=map[string]string{}

**The existing tests could not see it, and the shape of the gap is the reusable lesson.**
`TestDurableDeferReplayMatch` asserts `stepCount` and `isReplay` and never looks at
`s.deferrals`. `TestDurableDeferReplayPastEnd` *does* assert the map — but only on the
fallthrough, where replay has already ended and the fresh path runs. The one branch carrying the
bug was the one branch with no assertion on the map, and a reader counting tests would have said
`DurableDefer`'s replay behaviour was covered twice over.

The fix registers on replay as well as answering the guest, and reconstructs the ID as the fresh
path would have minted it when a pre-`DeferID` history replays.

**Regression tests:** `engine/defer_survives_suspension_test.go`. The history is one the engine
produced itself — segment 1 runs, registers, suspends — rather than written by hand. Both halves
mutation-proved 2026-09-01: with the map write removed, `...SurvivesIt` reports
`deferrals=map[string]string{}` and `...ActuallyRuns` reports the body was never invoked.

**Not fixed here, and deliberately:** `CompactionState.PendingDefers` is extracted, persisted in
every compaction state, asserted on by `TestExtractCompactionState_WithPendingDefers`, and
**read by nothing in production code**. It looks like the missing reader for this bug and is
not: past `DefaultMaxCompactedEvents` (10,000) `cs.Events` is truncated *from the front*, and a
replay whose first event no longer matches the guest's first durable call diverges immediately
and re-executes everything fresh — a far larger problem than defers, which seeding the deferral
set would paper over rather than fix. A seeding helper was written, wired, and then reverted for
exactly this reason: mutation testing showed nothing covered the wiring, and the scenario it
claimed to protect is already broken beyond it. **Either wire `PendingDefers` to something or
delete it; write-only persisted state is a trap for the next reader.**

Re-derive: `go test ./engine/ -run TestDeferRegisteredBeforeASuspension -count=1 -v`

### 3.67 A `cleat_sleep` at the replay frontier never resumes — ✅ **FIXED** (WS-3, 2026-09-01)

Found while building §3.66's regression test, which had to route around it.

`DurableSleep` (`engine/durablecalls.go:400`) records no event — "Sleep is local" — and completes
only when `s.replayJustEnded` is set. That flag is set by exactly one function, `exitReplay`
(`engine/lifecycle.go:141`), which is called only by a *durable call* that finds no matching
event at `s.stepCount`. `DurableSleep` never performs that check itself, and
`advanceReplayStep` does not end replay when it consumes the last event.

**So a sleep can only resume if some other durable call crosses the replay frontier first, in
the same segment.** In a workflow that replays faithfully, nothing does: every operation before
the sleep was recorded in the previous segment, so every one of them replay-matches, and the
sleep is always the operation that reaches the end of history. It suspends again, with a
byte-identical history.

Measured 2026-09-01, three successive segments each fed the previous segment's real output
history, through `Engine.Execute` then `Engine.Replay`:

| guest | seg 1 | seg 2 | seg 3 |
|---|---|---|---|
| `sleep` | `events=0 suspended=true` | identical | identical |
| `call; sleep` | `events=1 suspended=true` | identical | identical |
| `call; call; sleep` | `events=2 suspended=true` | identical | identical |

`SuspendUntil` is recomputed to the *same* wall-clock value every segment, because `nowMs` is
re-seeded from `replayHistory[0].TimestampMs` and replayed calls do not advance it.

**This is a regression with a date.** Commit `0a02a84` (2026-05-08), "deterministic durable time
with local sleep and WASI clock stubbing", made sleep local: it deleted `EventTypeSleep` from the
compaction codec and from `events.go`, and deleted `DurableSleep`'s own frontier check —
`if s.isReplay { ... if rec.EventType == EventTypeSleep { ... } }` — replacing it with the
`replayJustEnded` flag. Before that commit a sleep replay-matched its own recorded event and
needed nothing else. After it, sleep is the only durable operation that cannot end replay by
itself. Re-derive:
`git show 0a02a84 | grep -E "^[-+].*(replayJustEnded|EventTypeSleep)"`.

**Nothing covers it.** `grep -rn EventTypeSleep --include="*.go" . | grep -v _test.go` returns
nothing — sleep events are never written by production code. No test performs a genuine
two-segment resume-from-sleep: every test touching `replayJustEnded` sets it by hand on a
synthetic `execSession` (`engine/lifecycle_test.go:162` and ~15 assertions across
`host_test.go`, `host_dispatch_test.go`, `host_lifecycle_test.go`, `children_test.go`), and
`TestIntegrationMultiStepSleep` says in its own comment that it does not use `DurableSleep`.
The in-process simulators (`cleat/cleattest`, `cleat/embedded`, `cleat/localdev`) never replay,
so they cannot see it either — the same harness-disagrees-with-the-runtime shape as §3.64.

**The worker does not mask it, and makes it worse.** `cmd/cleat-worker/setup.go:1564` loads the
history verbatim and `:1786` passes it straight to `eng.Replay`; nothing writes a wake marker.
Because `SuspendUntil` is identical every segment, `next_wake_at` is rewritten to the same past
timestamp, and the claim query is `status='ready' AND next_wake_at <= now()`
(`engine/store_lifecycle.go:80,133`). Once the original wake time passes the workflow is
*immediately* re-claimable, so this is not a workflow stalled quietly — it is a hot re-claim /
re-replay / re-suspend loop burning CPU and DB writes indefinitely. **That claim is read from
the code, not yet measured against a running worker** — see below.

**Shipped examples depend on the broken path.** `examples/subscription/billing.go:59`
(`DurableSleep(30 days)` then `ContinueAsNew`) and `:123` (a `DurableSleep(24h)` loop with
`PollCancellation`) both put a sleep exactly at the frontier.

#### Confirmed with a real guest, and with a real store

The first write-up of this section listed what was *not* established: it had been measured only
with hand-written WAT guests on the no-backend wazero path. Both gaps are now closed.

**A real compiled Go SDK guest, through the wasmtime backend** -- the routing the worker
actually uses. The fixture is an ordinary workflow:

```go
first, err := h.DurableCall("stepA", "First", ...)
h.DurableSleep(10 * time.Second)
second, err := h.DurableCall("stepB", "Second", ...)
return "first=" + first + " second=" + second, nil
```

Built with `cleat build --target go -o <dir> <package>`, then driven through `Engine.Execute`
plus three `Engine.Replay` segments, each fed the previous segment's returned history:

| segment | events | suspended | SuspendUntil | host calls made |
|---|---|---|---|---|
| 1 (Execute) | 1 | true | `20:51:09.871` | `stepA.First` |
| 2 (Replay) | 1 | true | `20:51:09.871` | none |
| 3 (Replay) | 1 | true | `20:51:09.871` | none |
| 4 (Replay) | 1 | true | `20:51:09.871` | none |

History byte-identical from segment 2 on. **`stepB.Second` never ran, and the result was `""`
every time.** Reproduced independently in two trees.

**Build trap, recorded because it costs an hour:** `cleat build` writes a throwaway `go.mod` into
the output directory and runs `go build .` there, so the output directory must be *outside* this
repo's module tree -- otherwise Go's `go.work` discovery walks up, finds the repo's `go.work`,
and fails with `main module ... does not contain package ...`. Use `t.TempDir()`.

**The busy-loop half, against a real Postgres store.** Driving the real
`ClaimWorkflows` / `LoadEventHistory` / `FinalizeWorkflowSegment` in the loop
`cmd/cleat-worker/setup.go` runs, polling at 200ms:

| segment | wait to claim | `suspend_until` |
|---|---|---|
| 1 | 2.3ms | now + 10.18s -- a legitimate future timestamp |
| 2 | **9.93s** -- correctly waited for the real sleep | same absolute timestamp, now in the past |
| 3 | 2.3ms | same |
| 4 | 2.2ms | same |
| 5 | 2.2ms | same |

**Segment 2 is the control that makes the rest mean something:** the first wake is legitimate and
the sleep does work once. What never happens is *progress*. From segment 3 on, `next_wake_at` is
rewritten to the same already-elapsed instant every time, so the claim predicate
`status='ready' AND next_wake_at <= now()` matches immediately and forever. The precise
statement is therefore not "the workflow never wakes" but **"it wakes, re-executes to the same
suspend point, and re-arms itself in the past"** -- a hot re-claim loop burning CPU and DB writes,
not a quiet stall. Reproduced across three runs, each ~11-13s wall-clock, which matches the
intentional 10s sleep and confirms real time was exercised rather than a short-circuited path.

*Evidence level:* measured, against the real fenced claim/finalize SQL -- but driven by manual
store calls mirroring the worker's per-segment flow, **not** the `cleat-worker` daemon binary, so
goroutine scheduling, poll backoff and NOTIFY/LISTEN wakeup are not exercised.

**No regression test is committed for this.** A test asserting today's behaviour would pass
because the product is broken and go red when it is fixed. The fixture and command above rebuild
it in a couple of minutes; the test to commit is the inverted one, asserting that `stepB.Second`
*does* run, and it belongs with the fix.

#### The fix: elapsed time, not a flag

The first attempt had `DurableSleep` check the frontier itself
(`s.isReplay && s.stepCount >= len(s.history)`). That fixed a single sleep behind recorded
events and nothing else, because the check *infers* "this must be the sleep we stopped at", and
the inference only works when there is exactly one candidate.

The rule now is not an inference. **The anchor is the timestamp of the last recorded event -- a
real moment, written when that step ran -- and each sleep pushes a virtual deadline further past
it. If that deadline is already behind real time, the wait has happened**, whether it was spent
suspended, queued, or with the worker down. If it is still ahead, this is a new wait and the
workflow suspends for the remainder.

The accumulator this needs already existed and was being thrown away: `s.nowMs += durationMs`
already summed total virtual sleep time, and the code computed it and then decided on
`replayJustEnded` instead.

**`Now()` stays local, and structurally so.** Real time enters only the suspend-or-continue
decision. The guest-visible clock still reads `history[stepCount-1].TimestampMs` during replay
and the virtual accumulator after, so it advances by the durations the workflow *asked for*,
never by real elapsed time. That is what separates this from "just check the clock", which would
make replay non-deterministic.

`replayJustEnded` is deleted -- the field and `exitReplay`'s write. It asked a question sleep
cannot answer for itself ("did some *other* durable call just cross the frontier?"), and it was
the reason sleep was the one durable operation that could not end replay alone. Sleep is no
longer special, which retires the bug class rather than the bug.

**What this buys beyond the original defect:**

- **Consecutive sleeps work.** `call; sleep; sleep; call` now progresses. Previously the second
  sleep livelocked even after the frontier fix -- which matters because
  `examples/subscription/billing.go:122-127` loops `DurableSleep(24h)` with `PollCancellation()`
  between, and `PollCancellation` records no event, so that loop is exactly this shape.
- **Early resume is handled.** A reaper, manual retry or NOTIFY that hands the workflow back
  before its deadline now re-suspends for the remainder. Recording sleep events -- the other
  candidate design -- would have completed it regardless and cut the wait short.
- **Interior sleeps need no special case.** A sleep with a recorded event after it completes for
  a principled reason: that event could only have been written once the sleep returned, so its
  timestamp is at or beyond the deadline, and real time is beyond that again.
- **No history growth, no codec change, no migration**, so the event-cap question the
  record-the-sleeps option raised does not arise at all.

**Two deliberate behaviour changes, called out because they are not test churn.**

1. **A zero-duration sleep now completes instead of suspending.** The question is "has the
   requested delay passed", and for zero it has. Suspending for 0ms meant a full round trip
   through the store to wait no time; nothing could have relied on it as a checkpoint, because
   sleep records no event.
2. **Time between the last recorded event and the sleep counts toward it.** A workflow that burns
   five seconds of compute after its last durable call and then sleeps one second does not wait.
   This follows from the anchor being the last event rather than the moment of the call, and it
   is the intended reading -- virtual time is a lower bound on real time, and a workflow whose
   virtual clock is already behind reality is not ahead of schedule. It does differ from the old
   "at least N more seconds from now".

**The sleep-first case, closed separately.** A workflow whose first durable operation is a sleep
has no recorded event to anchor to, so the anchor fell back to the process wall clock -- which is
re-read every segment, so the deadline moved forward with it and never arrived. This is not an
edge case: "wait five minutes, then do the thing" is an ordinary delayed job, and it is the shape
most likely to be written as a sleep before anything else.

`WithWorkflowStartTime` supplies the workflow row's `created_at`, which the worker passes from
`wf.CreatedAt` and which is fixed for the life of the run, so the deadline stops moving.
`seedNowMs` takes the most deterministic anchor available -- first recorded event, then
`created_at`, then the process clock -- and the precedence has its own test, because
`created_at` winning over history would make a long-running workflow measure every sleep from the
moment it was created and read every deadline as already elapsed.

It also removes a determinism hole that predates all of this: `Now()` for a fresh workflow's
first steps was seeded from the wall clock, so two replays of the same empty history produced
different values. It is now `created_at` on every replay.

A related hazard was fixed on the way. The package-level `nowMs` seed is refreshed by the
worker's dispatch loop and by **nothing** in the CLI and embedded paths, where it stays zero. An
anchor at the epoch puts every deadline decades in the past, so every sleep would have completed
instantly under `cleat run` or `cleatctl replay`. `DurableSleep` falls back to real now when the
anchor is non-positive.

**Clock skew** is the residual risk: anchors are event timestamps written by `time.Now()` on
whichever worker ran that segment, compared against the resuming worker's clock, so a fast clock
completes sleeps early. This does not introduce a new class of skew -- event timestamps were
already worker-generated -- but sourcing both from the database would remove it, and is worth
doing if timers ever need to be tight.

Re-derive: `go test ./engine/ -run 'TestDurableSleep_|TestSleepAtTheReplayFrontier|TestConsecutiveSleeps|TestInteriorSleep|TestFreshSleepStillSuspends' -count=1 -v`

### 3.68 Replay released a virtual-object scope the workflow had already cleared — ✅ **FIXED** (WS-3, 2026-09-01)

The second instance of §3.66's class, found by auditing every `execSession` method for the same
shape after §3.66 shipped. ~~There are exactly two.~~ **There were three** — the audit missed one
in this very function, and §3.69 records both the third instance and why reading for it was the
wrong method.

`SetScope` has a fresh path and a replay path. Clearing a scope
(`objectType == "" && instanceKey == ""`) does two separable things on the fresh path, via
`ClearScope`: it **releases** the key on the concurrency-key store, and it **forgets** it —
splices it out of `s.heldScopes`. The replay branch zeroed the four scope fields inline and did
neither. Skipping the release is right: it is a side effect that already happened in the segment
that originally ran the step. Skipping the forget is not, and it is the bug.

`releaseHeldScopes` runs at the end of every execution and releases everything still in that
slice. Virtual objects use these keys to serialise access to one object instance — so a replayed
segment frees `vo:<type>:<key>` that this workflow gave up long ago, and **once another workflow
has acquired that object in the interval, the release frees somebody else's lock.** Two
workflows are then inside the same virtual object, which is the one thing the scope mechanism
exists to prevent.

Measured 2026-09-01, the same acquire-then-clear driven both ways:

    FRESH  after acquire+clear: heldScopes=[]string(nil)          scopeSet=false
    REPLAY after acquire:       heldScopes=[]string{"vo:cart:c1"} scopeSet=true
    REPLAY after clear:         heldScopes=[]string{"vo:cart:c1"} scopeSet=false

The fix adds `forgetHeldScope`, used by all three sites that previously open-coded the same
splice loop, and calls it from the replay clear branch. Naming the halves separately is the
point: "release" is the side effect, "forget" is the bookkeeping every replay of that step owes.

**The tests assert on releases, not on `heldScopes`.** A slice length is a proxy; the release is
the externally visible act, and it is what makes this a cross-workflow bug rather than a leak.
`TestFreshClearReleasesExactlyOnce` pins the behaviour replay reproduces, so "replay releases
nothing" cannot become true by the fresh path forgetting to release at all, and
`TestScopeStillHeldIsStillReleased` keeps the fix from degenerating into "never release
anything". Mutation-proved 2026-09-01: restoring the bug fails the regression test with
`replay released [vo:cart:c1]`, and an over-broad `forgetHeldScope` fails the splice control.

**The class, and what to do about it next.** Both instances have one shape: *the fresh path
mutates Go-side session state alongside `recordEvent`, and the replay branch reproduces the
event but not the state.* Everything else audited was written correctly on both paths
(`stateStore` via `SetState`/`DeleteState`/`IncrState`, and `SetScope`'s acquire branch). Two
instances is not obviously a missing abstraction yet, but it is enough to justify the cheaper
guard: **a property test over `execSession` that drives each durable call fresh and replayed and
asserts the resulting session state is identical.** That would have caught both without anyone
knowing to look, and it is the answer to CLAUDE.md's "sweep or mechanism" question here — the
sites are few, the invariant is one sentence, and the invariant is what keeps rotting.

Re-derive: `go test ./engine/ -run 'TestReplayDoesNotReleaseAScopeItAlreadyCleared|TestFreshClearReleasesExactlyOnce|TestScopeStillHeldIsStillReleased' -count=1 -v`

### 3.69 A third instance of the replay/fresh state class, found by a property test — ✅ **FIXED** (WS-3, 2026-09-01)

§3.68 ended by proposing the cheaper guard than more point fixes: *"a property test over
`execSession` that drives each durable call fresh and replayed and asserts the resulting session
state is identical."* This is that test, and the finding is what it caught on its first run.

**The third instance.** Clearing a scope is not the only way to give one up. Switching from one
virtual object to another releases the first key and drops it from the held set, both inside
`freshSetScope`. The replay branch appended the new key and left the old one in place, so
end-of-segment cleanup released an object this workflow had switched away from — and once
another workflow holds it, that is their lock. Identical harm to §3.68, a different branch of
the same function.

    fresh:  heldScopes=[vo:order:o9]
    replay: heldScopes=[vo:cart:c1 vo:order:o9]

**The method is the point, not the count.** §3.68 said "there are exactly two" on the strength of
a careful read of every `execSession` method, and it was wrong about the function it was fixing.
Reading found two instances and missed a third sitting eleven lines away; the property test found
that third one immediately, without anyone knowing to look for it. This is CLAUDE.md's "sweep or
mechanism" question answered by measurement: the invariant is one sentence, so encode it once
rather than auditing for violations.

**What the harness needs to be worth anything**, both of which cost more thought than the
property itself:

- **A vacuous-pass guard.** If the replayed session diverges — exits replay and re-runs the fresh
  path — the two states match trivially and the case proves nothing. Each case must consume every
  recorded event and still be in replay at the end. A case that cannot is a finding, not a case
  to relax.
- **A control for the control.** The property passes today, which is exactly what a harness
  comparing nothing would do. `TestParityHarnessCanFail` drives a deliberately asymmetric
  mutation — §3.66 reintroduced by hand — through the same comparison and requires it to be
  caught. Without it every future reader takes on faith that the property test can go red.

**Coverage is the calls that mutate Go-side collections**, which is where the invariant bites:
`DurableDefer`, `SetScope` (acquire, clear, and switch), `SetState`, `DeleteState`, `IncrState`,
singly and in the pairs where the property only appears across two calls — acquire-then-clear is
§3.68 and neither half alone shows it. Extending it to a new durable call is a table entry.

Re-derive: `go test ./engine/ -run 'TestReplayReproducesFreshSessionState|TestParityHarnessCanFail' -count=1 -v`

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

**Severity: silent wrong status, no error text anywhere.** A workflow that the Go runtime killed
for exceeding the configured memory limit came back from `Engine.Execute` as
`result="ok", err=nil`. The worker stores `status='done'`. Every step after the allocation never
ran, nothing retries it, and there is no error recorded to find it by.

Measured 2026-09-02 through `Engine.Execute`, `testdata/fencereentry`'s `allocate_forever` under
`WithWasmtimeMemoryLimits(64<<20, -1, -1)`:

| | before | after |
|---|---|---|
| `result` | `"ok"` | `""` |
| `err` | `<nil>` | `the guest exited with status 2 without reporting a result` |

Re-derive: `go test ./engine/ -run 'TestAGuestKilledByTheMemoryLimit|TestAHealthyGuestStillSucceeds'`

#### Why the existing guard missed it

The Go-on-wasmtime branch ignores the error from `_start` on purpose: `proc_exit` is how every
healthy Go guest leaves — `main()` returns and the wasip1 runtime exits — so a non-nil `startErr`
is the *normal* case. Before returning `"ok"` it asked `resourceLimitError` whether this was a
fence kill or fuel exhaustion.

That question is too narrow, and the case it misses is the one that matters. **When the Go
runtime cannot grow the heap past the limit it does not trap at all.** It prints a goroutine dump
and calls `proc_exit(2)` from its fatal path. That is not a `*wasmtime.Trap`, so
`resourceLimitError` returns nil, so the guest fell through to `"ok"`.

The fix reads the WASI exit status — `(*wasmtime.Error).ExitStatus()` — and treats **non-zero**
as the guest having been killed rather than having finished.

#### Notes for whoever touches this next

**Only non-zero.** `proc_exit(0)` is every healthy Go guest, so a fix keying on "`startErr` is
non-nil" rather than on the status fails every Go workflow in the system while looking like a
tightening of error handling. That is the mutation to try first on any change here.

**The control cannot prove the discrimination, and it is documented as unable to.**
`TestAHealthyGuestStillSucceeds` never reaches the changed block — a healthy guest reports through
`cleat_complete` and returns at the `completeResult != ""` branch above it (measured by probe:
an `fmt.Fprintf` at the top of the block printed nothing for that test). So the exit-0 half of the
guard is conservative by construction, not test-driven, and it is written to leave that path
behaving exactly as before.

**Not widened, deliberately.** A non-resource trap that is not a `proc_exit` still falls through
to `"ok"` — the same shape of hole. Nothing has demonstrated a Go guest reaching it: panics are
recovered into `cleat_complete` (§3.70) and unrecoverable failures leave through `proc_exit`.
Widening on an argument rather than a measurement is how the exit-0 path would get broken.

**Non-Go guests never had this hole.** The direct-export path returns its `callErr`
unconditionally; this was specific to the `_start` protocol, which is Go, the primary language.

#### How it was found

Not by looking for it. §3.35 phase 4's abnormal-exit measurement needed an OOM arm, and building
one meant reading what `Execute` does with a guest that dies that way. The measurement was about
defers; the defect was in the line above. **The same shape as §3.22** — a failing guest handed
back as a success — and worse, because §3.22 at least left the error text sitting in the result
column.

---

### 3.72 The engine suite poisoned its own database — ✅ **FIXED** (WS-3, 2026-09-02)

**A full `go test ./engine/` run finished green and left the database in a state where the NEXT
run failed.** The failure landed in `TestAssertTenantSetRejectsEmptyStringLikeNull` — a tenant/RLS
test with no visible connection to the test that caused it — so it appeared and disappeared
according to what had run against that database before, which is indistinguishable from a flake
and from "some unrelated change broke RLS".

#### Mechanism

`engine/drop_tenant_test.go`'s `resetToOriginal001DropTenant` re-applied the **entire**
`migrations/postgres/001_schema.sql` to recover one pre-032 function.

001 defines **five** functions with `CREATE OR REPLACE`, and exactly two of them have later
migrations that fix them:

| function | later definition |
|---|---|
| `cleat.assert_tenant_set` | **034** — collateral damage |
| `admin.drop_tenant` | 032 — the one the helper wanted |
| `admin.create_tenant_role` | none |
| `admin.grant_plugin_to_tenant` | none |
| `admin.revoke_plugin_from_tenant` | none |

Re-derive with `grep -n 'CREATE OR REPLACE FUNCTION' migrations/postgres/001_schema.sql`, then
check each name for later definitions.

So a helper aimed at one function reverted another, and **`schema_migrations` still recorded
version 34 as applied**. The runner only applies versions it has not recorded, so no migration
run repaired it — the database stayed broken for every subsequent run.

#### Fix

Extract only `admin.drop_tenant` from 001 rather than executing the whole file. The extraction
fails the test loudly if it finds nothing, finds no terminator, or finds more than one function:
a silently empty extraction would leave the *post*-032 function installed, and
`TestDropTenant_OldVersionLeavesDataBehind` — whose whole job is to show the pre-032 function
losing data — would then pass for the wrong reason.

#### The regression test asserts the invariant, not the symptom

`TestTheSuiteLeavesTheMigratedSchemaIntact` compares the live `cleat.assert_tenant_set` body
against what the highest-numbered migration defining it installs. Asserting instead that
`TestAssertTenantSetRejectsEmptyStringLikeNull` still passes would only ever catch the one
function that happened to be damaged this time.

Before: run green, body reverted to `IF tid IS NULL THEN`. After: run green, body stays
`IF tid IS NULL OR tid = ''`, and two consecutive full runs both pass.

#### A check that a retraction also satisfies

While diagnosing this I checked for the string `cleat.tenant_id is not set` and concluded the
database was healthy. **That string is in the exception message, which is present in BOTH the
001 and the 034 body** — so the check passes against the broken function. The guard is
`tid = ''`, and that is what the test matches on.

Same shape as §1.1's `Files:` pointer and the `ExecuteComponent` grep in CLAUDE.md: a check
whose evidence is equally consistent with the bug and with the fix. It cost most of a session,
and the tell was that the "healthy" reading never explained the failure.

---

### 3.73 Four SDKs document a `defer` that runs cleanup, and cannot run it — ✅ **ALL FOUR DONE** (WS-3, 2026-09-02)

§3.70 fixed this for Go. The other four SDKs have the same defect and it is still shipped.

`cleat_defer` carries a **description** across the boundary and nothing else. The host records
that a defer exists; no code anywhere can run it, because there is no body to run. Meanwhile the
SDKs say there is one:

- Python: *"Register a deferred cleanup action to run on workflow exit. Deferred actions are
  **executed** in LIFO order, analogous to Python's `try/finally` or Go's `defer`."*
  (`python-sdk/cleat_sdk/host_calls.py`)
- `docs/contributor/SDK_README_TEMPLATE.md`: `cleat_defer | (description) -> string | Register
  cleanup on exit`

**Python is tier 1** (`tiers.yaml`: `languages: [go, python]`), so this is a tier-1 correctness
gap, not only a tier-2 feature gap.

**§3.35 phase 4 made one consequence sharper.** `__cleat_run_deferred` was emitted by Go codegen
only, so the host's kill-path cleanup — added in #551 — silently no-oped for every non-Go guest.
`GetFunc` returned nil and `runGuestDefersAfterKill` returned without doing anything.

Where that stands as of 2026-09-02, one row per SDK:

| SDK | normal path | kill path (`__cleat_run_deferred`) |
|---|---|---|
| Go | ✅ §3.70 | ✅ #550 |
| Rust | ✅ #553 | ✅ #553 |
| Python | ✅ #554 | ❌ structural — see below |
| Java | ✅ #556 | ✅ #558 |
| AssemblyScript | ✅ #557 | ✅ #557 |

Python is the only gap left, and it is not an oversight: its WIT world exports one function and
it runs through `ExecuteComponentCGo`, which has no defer pass at all. Both are set out below.

#### The shape, established on Rust (#553)

Four pieces per language, and every one of them is needed:

1. a guest-side table mapping the host's defer ID to a body;
2. a `defer_func`-style call that registers a body alongside the existing description-only call;
3. a drain in LIFO order, run by the entry-point wrapper on the success and error paths but
   **not** on suspension;
4. a `__cleat_run_deferred` export so the host can drain it for a workflow it killed.

The suspend rule is where the two callers disagree, and it is the part most likely to be got
wrong: the wrapper's drain must let the suspend sentinel **out** so its segment suspends, and the
host's export must **swallow** it, because a workflow reached that way is already dead and has no
segment left. Rust's tests pin both halves; a mutation that swallows the sentinel in the wrapper
passes every other test in the file.

#### Per-language notes, measured before starting

| SDK | hook | risk |
|---|---|---|
| Rust | `#[cleat_entry]` proc macro wraps every export | **done** — real closures, `catch_unwind` already mirrors Go's sentinel handling |
| Python | `cleat_entry` decorator + `_dispatcher_run` (`python-sdk/cleat_sdk/entry.py`) | tier 1, so it must be right; componentize-py/WIT export surface needs checking |
| Java | TeaVM; `_start` initialises, exports called by name | unexamined |
| AssemblyScript | `@cleatEntry` marker + transformer plugin | **highest risk.** The AS SDK deliberately avoids closures — `saga.ts`: *"Uses function references, not closures"* — and builds with `--runtime stub`, which has no GC and no exceptions. A capturing `deferFunc(fn)` may not be expressible; it may only ever get non-capturing function references, which is a materially weaker feature and should be documented as such rather than papered over. |

**If a language cannot support a body, say so in its docs instead of claiming cleanup that
cannot happen.** That is the failure this item is about; re-creating it in a different form
would be worse than leaving the gap.

#### Python: the normal path is done, the kill path needs host work (#554)

`defer_func` runs cleanup when the entry point finishes — success and error paths, not
suspension — and `HostCalls.defer`'s docstring no longer claims execution it cannot deliver.
That closes the **tier-1** correctness gap.

**The kill path does not follow, and the reason is structural.** Two things block it, both
measured 2026-09-02:

1. **The WIT world exports exactly one function** — `export run: func(args: string) -> string`
   (`python-sdk/wit/cleat.wit`). There is nowhere for the host to call a defer runner. Adding one
   means changing the WIT world and regenerating the componentize-py bindings.
2. **Python does not use the execution path the kill-path cleanup lives on.** A component binary
   is dispatched to `ExecuteComponentCGo`, and `engine/component_cgo.go` has no defer pass at
   all. Re-derive: `grep -rn deferRunnerExport engine/*.go | grep -v _test` — it appears only in
   `backend_wasmtime.go`.

**So a Python workflow killed by the execution fence still loses its cleanup**, and §3.35 phase 4
covers Go, Rust and Java but not Python. Stated here rather than left to be inferred from the
phase-4 section.

The change is designed and cheap — `export run-deferred: func(args: string) -> string` reusing
`run`'s shape so `componentCall` needs no new marshalling, plus a component-path defer pass — and
was deliberately not shipped: componentize-py is SIGKILLed in the WS-3 sandbox, so it could not
be built even once, and the WIT change affects every Python build rather than only a test.

#### Java: done, and it needed §3.74 first (#556)

`HostCalls.deferFunc(Runnable)` runs cleanup when the entry point finishes, and the javadoc that
promised "a deferred cleanup callback" for an API with no callback parameter now says what
`cleatDefer` actually does.

**Writing this required fixing suspension first.** The control test every other SDK has — defers
must NOT run on suspension — could not be written against a Java SDK where suspension was
unreachable (§3.74). That is why §3.74 landed before this: the most important test in this item
had nothing to assert against.

The suspend branch is asserted defer-free by slicing the generated source between
`catch (cleat.SuspendSignal` and the next `catch` — a drain anywhere in the wrapper would satisfy
a looser check while still firing every cleanup at the first sleep. Mutation-proven: adding a
drain to that branch fails exactly that test.

**Java gets §3.35 phase 4's kill path too (#558), unlike Python.** TeaVM exports are declared
with `@Export`, so `CleatEntryProcessor` generates a `cleat.generated.CleatDeferRunner` carrying
one — once per compilation that has an entry point, because it is one export for the module and
per-entry would be a duplicate-export failure the moment a module declares two workflows.

**`CleatEntryIndex.WRAPPER_CLASSES` has to reference it, and that is the part that would have
been missed.** Nothing in the guest calls the runner: its only caller is the host, after the
workflow is dead. That makes it exactly what TeaVM's dead-code elimination removes, and a
tree-shaken export is indistinguishable from one that was never generated. It is deliberately
**not** in `getEntries()`, which lists workflow entry points — a caller enumerating those would
otherwise try to execute the defer runner as a workflow.

The runner calls `Defer.runDeferredForHost()`, not `Defer.runDeferred()`, and the difference is
the same one the suspend rule above is about: the wrapper needs `SuspendSignal` to escape so its
segment suspends, and this caller must swallow it, because a workflow reached this way is already
dead and has no segment left.

**`TestJavaExportsTheDeferRunner` goes through the real TeaVM build, not the generated source**,
because the two disagree in the direction that matters. `CleatEntryProcessorTest` can only show
that the source says `@Export`; only compiling shows whether TeaVM kept it. Verified by removing
the `generateDeferRunner()` call: the two processor tests go red, and so does the engine test,
with the module's real export list in the failure message — `_start`, `transfer_money`,
`get_transfer_status`, thirty-odd `teavm_*` and no `__cleat_run_deferred`.

#### AssemblyScript: done, including the kill path (#557)

The one the table above called "highest risk", and the risk was correctly identified — a
capturing `deferFunc(fn)` is **not** expressible under `--runtime stub`. It did not need to be.
`deferFunc(h, description, fn, payload)` takes a top-level function reference plus an explicit
payload string, which is the shape `saga.ts` has always used (`type CompensateFn = (h: HostCalls)
=> void`). So this is the SDK's existing idiom rather than a weaker version of the other three's.

**The suspend guard is the part that can actually break here, and it is the reverse of what the
per-language table anticipated.** Go, Rust, Python and Java suspend by *unwinding* — a panic, a
raise, a thrown `SuspendSignal` — so their drain sits on a path the unwind skips and the ordering
cannot be got wrong. AssemblyScript has no exceptions: suspension is a global flag
(`isWorkflowSuspended()`), the entry point **returns normally either way**, and the generated
wrapper's explicit check is the only thing between a sleeping workflow and its cleanup. Moving
the drain one line earlier compiles and passes every other test in the file;
`TestAssemblyScriptDefersDoNotRunOnSuspension` is what catches it, verified by making exactly
that mutation.

A second consequence of the flag model, worth knowing before writing an AS workflow: a suspending
host call **does not stop the workflow body**. `Saga.run` checks the flag after every step for
this reason, and `examples/as-workflow`'s `defer_suspend` had to do the same — without it the
line after `cleatSleep` executed during the suspending segment. That is not new and not a defect
of this item; it was simply not written down anywhere.

**`__cleat_run_deferred` is emitted too**, so AssemblyScript joins Go and Rust on §3.35 phase 4.
The transformer emits it once per module rather than once per entry point — per entry would be a
duplicate-export failure the moment a module declares two workflows.

**What this cost, and it is the finding worth keeping.** Two AS workflows imported the SDK by
relative path (`../../packages/cleat-as/assembly/index`) rather than as `@cleat/sdk`. Both
resolve to the same files, but `asc` treats them as **two distinct modules** — two `HostCalls`
types, and since this item, two defer registries. A defer registered through one would be drained
from the other and silently never run: the exact bug this item is about, reintroduced invisibly.

It surfaced only because the generated wrapper passes `h` to `runDeferred`, which makes the two
modules a type error:

```
ERROR TS2322: Type '../../packages/cleat-as/assembly/host-calls/HostCalls'
is not assignable to type '~lib/@cleat/sdk/assembly/host-calls/HostCalls'.
```

Nothing else in the wrapper crosses the boundary that way — `Memory.readString`,
`isWorkflowSuspended()` and `SUSPEND_SENTINEL` are all module-local — so a dual-module workflow
compiled cleanly before and would again if that argument were removed. Recorded in
`packages/cleat-as/README.md` and `LANGUAGE_SUPPORT.md`, and it is a compile error rather than a
convention, which is the only reason to trust it.

#### The latent break in the transformer — ✅ fixed 2026-09-02

`_hasCleatSdkImport` (`packages/cleat-as/transform/index.js`) probed four property paths for an
import's module name and returned false for all of them, so the transformer *always* injected its
own `@cleat/sdk` import — including into sources that already had one. That is why the wrapper's
`Memory` / `SUSPEND_SENTINEL` / `isWorkflowSuspended` / `runDeferred` references resolved at all.

**Measured rather than reasoned, 2026-09-02.** A probe on the real AS parser, against a fixture
whose first line is `import { HostCalls, cleatEntry } from "@cleat/sdk"`:

    [PROBE] _hasCleatSdkImport=false needsImport=true

So the guard never fired once. Every generated wrapper compiled *because the detector was
broken*, and a future `asc` that exposed one of those property paths would have started
suppressing the import and broken every wrapper whose author had not happened to import all five
symbols by hand.

**Detection was deleted rather than fixed, and the reason is worth keeping.** A *working*
detector is worse than none: it would have to inject only the names the author left out, name by
name, or else suppress an import the wrapper needs. Injecting unconditionally is what already
happened; making it deliberate costs nothing and removes the failure mode.

The decision rests on one fact, so that fact now has a test:
`TestASTransform/compiles_when_the_user_imports_the_same_symbols` compiles a workflow that
imports all five symbols itself, proving AssemblyScript tolerates the duplicate import from the
same module. Without it, "always inject" is an assumption.

---

### 3.74 Java workflows could not suspend — ✅ **FIXED** (WS-3, 2026-09-02)

A Java workflow that slept on a fresh execution **completed with a bogus result instead of
suspending**. The host recorded it as done; the sleep never happened; everything the workflow
would have done after waking never ran.

#### The two halves disagreed, and only one was missing

The **host** half has been ready all along — `engine/backend_wasmtime.go:855` checks
`if raw == (1 << 62)` and returns `Suspended: true`.

The **guest** half could not produce that value. `HostCalls.cleatSleepMs` returned `true` with a
javadoc telling the author *"the workflow should propagate the suspension by returning
`Memory.SUSPEND_SENTINEL` from the export"* — but **the author does not write the export.**
`CleatEntryProcessor` generates it, and the generated wrapper had no branch that could return
that value. It stringified whatever the workflow returned and reported
`encodeExportResult(0, written)`: a plain success.

Measured 2026-09-02 by generating a real wrapper for a workflow that sleeps
(`javac -processorpath` against the built SDK jar). Before:

```java
java.lang.String result = Probe.sleepy(hostCalls, input);
String resultJSON = JsonHelper.stringify(result);
int written = Memory.writeString(outPtr, maxOutLen, resultJSON);
return Memory.encodeExportResult(0, written);   // always success
```

`grep -c SUSPEND_SENTINEL` on that generated file: **0**.

#### Fix

`cleat.SuspendSignal`, an unchecked exception thrown by `cleatSleepMs`, caught by the generated
wrapper, which returns `Memory.SUSPEND_SENTINEL`. That is what every other SDK already does —
Go and Rust panic, Python raises — so Java stops being the one that asks the author to do
something they have no way to do.

**Catch order is load-bearing and is asserted.** `SuspendSignal` is a `RuntimeException`, so a
`catch (Exception)` placed first swallows it and reports a suspended workflow as one that
*failed* with the message "cleat: workflow suspended". The test pins the ordering, not just the
presence.

#### How it was found

Not by looking for it. §3.73 needed Java's defer hook, and writing the "defers do not run on
suspension" control test — the one that matters most in every other SDK — required knowing how
Java suspends. It turned out it does not. **The control test could not have been written
correctly before this was fixed**, which is why this landed first.

Re-derive: generate a wrapper and read it, or run
`CleatEntryProcessorTest.testGeneratedWrapperPropagatesSuspension`. Removing the processor's
suspend branch fails exactly that test and no other (282 tests, 1 failure).

### 3.75 The durable record for a resumable defer phase — 🔵 **DESIGN ANSWER, not yet built; one premise corrected and the execution half built in §3.81** (WS-2, 2026-09-02)

> **2026-09-02, read §3.81 before building this.** The record-shape answer below stands. One
> supporting claim does not: the inventory excludes `RequestCancellation` because "the guest
> observes it and exits through its own wrapper, so that path already runs defers", and
> measurement shows the drain runs but every call it makes is refused by the same check — so the
> cleanup is consumed rather than performed. The two-phase transition needs a guest→host
> defer-phase signal first, which does not exist in any SDK. §3.81 has the measurements.

§3.35 phase 5 is blocked on WS-2 by design: making `defer` survive a `kill -9` needs the defer
phase to be "its own durable, resumable unit with a reaper", and the plan states that is **the
same record shape as §1.4's crash recovery**, which is WS-2's (§3.35, "Phase 5 is not
scheduled"). WS-3 has phases 1–4 shipped and is deliberately not starting 5 without this
answer. This section is the answer. The migration that carries it is WS-3's.

**The answer is that phase 5 does not need a new record.** It needs the terminal transition to
become two-phase. What follows is why the obvious record designs are the wrong shape, and what
to build instead.

#### The question as it was posed, and why both options are wrong

The question WS-2 was asked to settle: does a defer phase become another `event_history` row
under phase D's pending discipline — `intent_at IS NOT NULL AND checksum IS NULL`, resolved
through `ResolveCallIntent` — or its own table with its own reaper?

Both answers assume a fact that is no longer true: that defers run *after* the workflow is
terminal, so their durability has to be arranged outside the workflow's lifecycle. **Every
defer execution in the tree today happens inside the claimed segment, before
`FinalizeWorkflowSegment`.** Measured 2026-09-02 at `develop` `9b4824f`:

| path | where defers run |
|---|---|
| success | inside the guest, in the entry-point wrapper, before it reports its result (§3.70). The worker's post-finalization pass was **deleted** — `cmd/cleat-worker/setup.go:1853`, "No defer pass here" |
| trap / fence / timeout | `engine/executor.go:491`, inside `Engine.Execute`, before it returns to the worker |

So on both paths the workflow is still `running`, its claim is live, `WriteCallIntent`'s
`workerID`+`generation` fence still matches, and a `kill -9` mid-defer commits nothing terminal
— `FinalizeWorkflowSegment` appends the segment's events and the terminal status in **one
fenced transaction** (`engine/store_lifecycle.go:263`). The instance stays `running` with a
stale heartbeat, `ReapStaleInstances` re-queues it, and replay resolves the pending row through
the machinery §1.4 already built. **These paths are already covered and need nothing.**

#### What is actually left

The plan names it exactly (§3.35): *"a workflow cancelled or failed terminally between segments
has no live instance to re-enter at all."* The residue is not a durability gap. It is the set
of places where a **terminal status is set by an `UPDATE workflow_instances`** rather than by a
segment finalizing, so no instance ever exists to run the defers:

| site | what it does |
|---|---|
| `TerminateWorkflow` (`engine/db.go:1128`, `mysql_ops.go:1188`, `mssql_operations.go:173`) | `SET status = 'terminated' … generation = generation + 1`, then `releaseWorkflowResources` — **on MySQL only since #571 (§3.76); see the correction below** |
| `enforceParentClosePolicy`, `TERMINATE` arm (`engine/store_lifecycle.go`) | `SET status = 'failed', error_msg = 'parent workflow terminated'` on every child |
| `adminForceResolve` (`engine/store_admin.go:154`, and the MySQL/MSSQL arms) | `SET status = 'done'`/`'failed' … generation = generation + 1`. §3.20's force-resolve, WS-2's own, and it skips defers the same way |

`RequestCancellation` is **not** in this set: it sets `cancellation_requested`, the guest
observes it and exits through its own wrapper, so that path already runs defers.

**The ordering hazard is live today and is the sharpest argument for fixing this.**
`TerminateWorkflow` calls `releaseWorkflowResources` — `ClearStickyWorker` and
`ReleaseWorkflowConcurrencyKeys` — immediately after the terminal `UPDATE`. So the host
releases the locks, and the defer that would have released them never runs. A terminated
workflow's cleanup is not merely skipped; it is pre-empted by the host doing a *different*
release, in the wrong order, with no record that anything was owed.

**Corrected 2026-09-02, and the correction is §3.76.** The sentence above says
`TerminateWorkflow` calls `releaseWorkflowResources`. That was true of PostgreSQL and SQL
Server and **not** of MySQL, which exec'd the `UPDATE` and returned — so this section described
two dialects out of three as though it were all of them, and #571 has since made it true by
fixing MySQL to match. Two things follow, and they pull in opposite directions:

- The hazard above now applies to **three** dialects rather than two, so the argument for the
  two-phase transition is stronger, not weaker.
- But §3.76 was found *by checking this section's inventory per dialect rather than reading the
  PostgreSQL implementation and generalising*, which is what this section did. The inventory's
  three sites were right; one of them was additionally skipping a release the other two
  performed. **Re-derive per dialect before building step 2 below** — the same generalisation
  could be hiding in the parent-close arm or `adminForceResolve`.

§3.76 is not a step toward this section's design, and reading it as one would be a mistake: it
makes the *current* ordering consistent across dialects, and step 1 below then has to move that
release on all three. Consistency first and the move second is the right order — a two-phase
transition built over one dialect that released and two that did not would have been three
behaviours, not two.

#### The shape: a two-phase terminal transition

A defer body needs a live instance **with a session** — §3.35 phase 2's finding was that a defer
without one panics inside any host call (`handlerFromContext`, `engine/imports.go:20`), and "a
defer that cannot call the host cannot release the lock it took". Getting a live instance means
replay, which means dispatch, which means the workflow must be claimable and non-terminal.

That constraint decides the design. The host-driven terminal transition splits in two:

1. **Mark, do not finalize.** `TerminateWorkflow` and the parent-close `TERMINATE` arm record
   the intended terminal outcome and leave the workflow **schedulable** rather than terminal.
   They must **not** call `releaseWorkflowResources` at this point.
2. **A defer segment.** The existing dispatch loop claims it like any other workflow, replays
   history to reconstruct the instance, runs the registered defers in it, and *then* finalizes
   with the outcome recorded in step 1 — at which point `releaseWorkflowResources` runs, after
   the defers that may have released those resources themselves.

Once step 2 is a segment, **every defer execution is again inside a claimed segment**, which is
the case the tree already handles. No new fence, no new reaper, no new pending discipline.

`FinalizeWorkflowSegment` already supports this: `validFinalStatus` accepts `ready` and
`suspended`, not only `done`/`failed`, so a segment that ends re-schedulable is an existing
shape rather than a new one.

#### What is durable, and where it lives

The only genuinely new durable state is the workflow-level fact **"this workflow owes a defer
phase, and here is the outcome to apply when it is done"**. That is:

- a marker and the pending terminal outcome on `workflow_instances` — the outcome fields
  (`error_msg`, `error_code`) already exist; what is new is the marker and a deadline;
- swept by the **existing** `reaperLoop` (`cmd/cleat-worker/setup.go:1910`), so a worker that
  dies mid-defer-segment leaves a workflow that the reaper re-queues exactly as it does now.

**No per-defer durable row is needed, in `event_history` or anywhere else.** What is owed is
already derivable from history — `DeferralsFromHistory` (`engine/helpers.go:36`) reconstructs
the registered set from the `EventTypeDefer` rows, which carry `defer_id` and
`defer_description` and are written on the normal path. And each defer body's own host calls
are durable calls that get their own event rows and their own intent handling, so a crash
*inside* the defer phase is already covered at the granularity that works.

#### Why not the other two

**Its own table with its own reaper** is the more expensive answer by a wide margin, and the
cost is hidden by the phrase. Re-running a defer requires re-instantiating a WASM guest from
history, so a separate reaper would have to duplicate dispatch, claim, lease and replay — a
second scheduler, not a second table. It would also be the second durable-resumable-record
shape, which is the specific outcome §3.35 asked WS-2 to prevent.

**A pending `event_history` row for the defer phase** is closer, and is the right mechanism for
the *host calls a defer makes* — but as the record of the phase itself it inherits three
problems that the workflow-level marker does not have. The fence is defined on a live claim and
`TerminateWorkflow` bumps `generation`, so it would have to be redefined. The row needs a step
number after the body's last, minted by a writer that is not the executing session — the
contention #297 already hit. And a row parked pending at the tail leaves the checksum chain
seeded from a `NULL`, since `previousStoredChecksum` reads the immediately preceding row and the
verifier resets on a missing checksum; harmless, but it means an unfinished defer permanently
shows a reset chain segment.

#### What WS-3 can build against

- ~~Add the marker + deadline column to `workflow_instances`~~ — ✅ **done 2026-09-03.**
  `migrations/postgres/038_defer_phase_marker.sql`, `mysql/037`, `mssql/041`:
  `pending_terminal_status` (the outcome to apply, `NULL` = no phase owed) and
  `defer_phase_deadline`. **The numbers above were already stale when read** — the note said
  postgres `034`, mysql `033`, mssql `037` as of 2026-09-02, and the high-water marks were
  `037`/`036`/`040` a day later. Re-derive rather than trusting a recorded number:

      for d in postgres mysql mssql; do ls migrations/$d/*.sql | sed 's#.*/##;s/_.*//' | tr '\n' ' '; echo; done

  No `CHECK` constraint to amend — `workflow_instances.status` is plain `TEXT`, checked before
  writing the migrations rather than assumed, since a constrained column would have made this
  three larger files. Verified by running: `go test ./migration/` green on all three dialects
  (which applies the shipped files and re-applies them to prove idempotency), plus a direct
  catalogue probe confirming 2 of 2 columns present on each. `engine/testutil` builds its schema
  from the shipped migrations, so there is no second schema definition to keep in step.
  `docs/reference/workflow-lifecycle.md` carries the vocabulary, including `terminating` as a
  seventh status with schema but no writer.
- Change the two host-driven terminal sites to mark rather than finalize, and move
  `releaseWorkflowResources` behind the defer segment on those paths.
- Add the defer segment to the executor: replay, run defers, finalize with the recorded outcome.
- Extend `reaperLoop` with the marker's deadline predicate.

#### Open, and deliberately not decided here

**The caller-visible status window — ✅ ANSWERED 2026-09-02, recorded as `tiers.yaml` D6.** Both
of the options below, not either: terminate becomes asynchronous **and** the window gets its own
status rather than reusing `ready`. The condition attached to the answer was that it be
documented and visible, which is `docs/reference/workflow-lifecycle.md` — the whole state machine,
because the window is only comprehensible inside it. Reusing `ready` was rejected specifically:
it would make "terminating, running its cleanup" indistinguishable from "runnable", which is the
failure the decision exists to avoid. The question as originally posed follows.

Between step 1 and step 2 a terminated workflow is not yet
`terminated`, so a client polling status sees it still in flight for the duration of its defer
phase. That is the same trade the two-phase transition buys everywhere else, and it is a product
call rather than an engineering one: either terminate becomes asynchronous and observably so, or
the marker gets a distinct status (`terminating`) that the API reports honestly. **Do not pick
one silently** — `tiers.yaml` and the admin API both describe terminate today, and this changes
what it means.

**The inventory above is the whole of it, measured rather than assumed** —
`grep -rn "SET status = '" --include='*.go' engine/ | grep -v _test` over all three dialects,
2026-09-02. Every other terminal `UPDATE` it finds is either a `FinalizeWorkflowSegment` arm
(so a segment ran and its defers ran with it), a `running`/`ready` transition, or a promise or
signal row rather than a workflow.

**One site needs checking rather than assuming, and it is the reason to run that grep again
before building.** `MoveToDeadLetterQueue` (`engine/store_lifecycle.go:505` and its two dialect
twins) sets `dead_lettered` by direct `UPDATE`, which puts it in the shape of the table above —
but unlike the three rows there it is **fenced on a live claim**
(`WHERE id = $1 AND assigned_to = $2 AND generation = $6`) and is called by the worker that
holds it, from `cmd/cleat-worker/setup.go:2565`, after a segment has failed. So its defers
should already have run on `Execute`'s trap path. That is a reading of the call site, not a
measurement: **confirm a dead-lettered workflow's defers actually ran before deciding it needs
no marker.** If it turns out a workflow can reach the dead-letter queue without a segment having
executed, it is a fourth row rather than an exception.
### 3.76 MySQL's TerminateWorkflow released nothing — ✅ **FIXED** (WS-3, 2026-09-02)

`releaseWorkflowResources` states its own contract: it runs the two best-effort cleanups that
follow *"every commit which takes a workflow out of the runnable set: completion, failure,
**termination**, continue-as-new, and the admin actions"*. `PostgresStore.TerminateWorkflow`
(`engine/db.go`) and `MSSQLStore`'s (`engine/mssql_operations.go`) call it after their commit.
**`MySQLStore.TerminateWorkflow` (`engine/mysql_ops.go`) execed the UPDATE and returned.**

Measured 2026-09-02 by `TestTerminateWorkflowReleasesConcurrencyKeys`, which acquires a key,
proves it is contended, terminates the holder, and tries to acquire it again:

| dialect | before |
|---|---|
| postgres | PASS |
| **mysql** | **FAIL — the key stayed held** |
| mssql | PASS |

**Bounded, not unbounded, and the bound is what keeps this out of §1.x.** `concurrency_keys`
`expires_at` is `NOT NULL` and the worker's reaper deletes expired rows, so the slot was never
leaked forever — it was held until the key's TTL, with every workflow queued on that key waiting
out the window for nothing, on a **tier-1 dialect**, while the other two freed it at once. The
sticky-worker assignment went the same way.

**Found while verifying §3.75's inventory rather than by looking for it.** §3.75 identifies
`TerminateWorkflow` as one of three sites that set a terminal status by direct `UPDATE` and so
never run defers. Checking that claim per dialect — rather than reading the Postgres
implementation and generalising — is what turned this up: the three sites are right, and one of
them was *additionally* skipping a release the other two performed.

The test asserts the released **state**, not the call. A dialect that freed the slot by some
other route would rightly pass, and the arm that failed is the mutation this fix needs — it went
red before the one-line change and green after, with the other two arms unmoved throughout.

---

### 3.80 A closed parent's children keep their concurrency slots — ✅ **FIXED** (WS-2, 2026-09-02)

> **Renumbered from §3.78 to §3.80, 2026-09-02, because §3.78 was allocated twice** — this and
> WS-1's "Cross-schema child workflows, removed" (#582), which landed first and is referenced
> from `CHANGELOG.md`, `ABI.md`, the execution-design doc and §2.23, so it kept the number and
> this one moved.
>
> **The "next free number above the highest in the file" rule did not prevent it, and could
> not.** Both writers followed it: §3.77 was the highest, both took §3.78, and neither could see
> the other's unmerged branch. The rule removes collisions between a writer and the *file*; it
> does nothing about two writers reading the same file at the same time. #563 retired prefix
> blocks for needing every writer to remember them — this is the residual failure of what
> replaced them, and it is cheaper than the alternative rather than a reason to go back.
>
> What actually caught it was reading `git log --oneline` after merging and noticing two
> subjects carrying the same section number. **Check the numbers in the merged log, not only in
> the file you edited**, because your own file is exactly where a concurrent allocation is
> invisible.


`releaseWorkflowResources` states its own contract: it runs the two best-effort cleanups that
follow *"every commit which takes a workflow out of the runnable set: completion, **failure**,
termination, continue-as-new, and the admin actions"*.

`enforceParentClosePolicy`'s TERMINATE arm commits exactly such a failure —
`SET status = 'failed', error_msg = 'parent workflow terminated'` — for every child of a closing
parent, and released nothing. **On all three dialects.**

**Same class as §3.76, found the same way, and it differs in the two respects that matter.**
§3.76 came out of checking §3.75's inventory per dialect; this came out of checking the same
inventory against the *contract*, which is the only thing that could have found it: MySQL,
PostgreSQL and SQL Server all agreed here, so no cross-dialect comparison would have flagged it.
And it is a **bulk** operation — one closing parent strands a slot per child, not one slot.

Bounded rather than unbounded, for the same reason §3.76 is a §3.x: `concurrency_keys.expires_at`
is `NOT NULL` and the reaper deletes expired rows, so the slots free themselves at the key's TTL.
They free themselves silently, with every workflow queued on those keys waiting out the window
for nothing.

**The fix collects the child ids before the UPDATE rather than returning them from it**, because
the dialects do not agree on returning affected rows — PostgreSQL has `RETURNING`, MySQL does
not. A child whose status changes between the two statements is released anyway, which is
harmless: releasing the resources of a workflow that finished on its own is what its own
terminal path would have done. If the TERMINATE step itself fails, the id list is dropped, so a
failed close does not trigger releases it did not earn.

`TestParentCloseTerminateReleasesChildConcurrencyKeys` asserts released *state* rather than the
call, so an implementation that frees the slot another way rightly passes. Red on PostgreSQL and
MySQL before the fix, green after; neutering only the PostgreSQL release fails only the
PostgreSQL arm with MySQL unmoved.

#### The vacuity guard earned its place, and the finding it produced is §3.79

The test first closed the parent with `TerminateWorkflow`, which reads as the obvious way to
close a parent. The child stayed `ready`, and the guard asserting *"the close policy actually
fired"* is what caught it: **`TerminateWorkflow` does not call `enforceParentClosePolicy` at
all** — only `FinalizeWorkflowSegment` and `adminForceResolve` do. Without that guard the key
assertion would have passed for the wrong reason, on an unfixed engine, and this section would
have shipped as a fix for a bug it never exercised.

### 3.79 `TerminateWorkflow` does not enforce the parent close policy — ✅ **FIXED** (WS-2, 2026-09-02)

Found by §3.80's vacuity guard rather than by looking. (This line said §3.77 until 2026-09-02 — a stale reference left by that section's own renumbering, pointing at an unrelated WS-1 item. Renumbering is where cross-references rot, so re-grep the old number after moving a section rather than fixing the ones you remember.) `enforceParentClosePolicy` is called from
`FinalizeWorkflowSegment` (for `done`/`failed`) and from `adminForceResolve`. It is **not**
called from `TerminateWorkflow`, on any dialect — so terminating a parent leaves its
`TERMINATE`-policy children running, while force-completing the same parent fails them.

Measured 2026-09-02: a child of a parent closed with `TerminateWorkflow` stayed `ready`; the same
child under `AdminForceComplete` went to `failed`.

~~**Not yet a defect claim.**~~ **Settled the same day, and it is a bug.** The two readings were:
terminate is supposed to close children like every other terminal path, or terminate is
deliberately the operator's "stop this one workflow" verb and the close policy is for the
workflow's own completion.

The instruction was to check for *stated* intent rather than infer it from the call sites. The
documentation answers it, though not unanimously — which is the reason it was worth reading
rather than guessing:

| source | says |
|---|---|
| `docs/contributor/design/cleat-execution-design.md:1830` | the policy governs what happens "when the parent **completes or fails**" — the narrow reading, and the only support for the second interpretation |
| same doc, `:1926` | *"`enforceParentClosePolicy` runs on parent **terminal transition**"* — and terminate is one |
| same doc, `:1932` | the design's stated goal: *"children are cancelled with their parent (**preventing orphan workflows**)"* |
| same doc, `:1830` | the model is *"matching Temporal's"*, and Temporal applies parent close policy on termination |

**The deciding argument is internal consistency rather than any of those.** `adminForceResolve`
is an operator verb, on an unclaimed workflow, setting a terminal status with a direct `UPDATE`
— the same shape as `TerminateWorkflow` in every respect the second reading depends on — and it
enforces the policy. Two paths that differ in no stated way behaved differently, and nothing in
the tree explained why.

Counter-evidence was looked for and not found: no test asserted that terminate leaves children
alone. `TestEnforceParentClosePolicy` closes its parent with `CompleteWorkflow`, so it covers
the policy and says nothing about this path.

**Fixed** by calling `enforceParentClosePolicy` after the terminal commit on all three dialects,
beside the `releaseWorkflowResources` call already there.
`TestTerminateWorkflowEnforcesParentClosePolicy` asserts all three policies at once, so a fix
that cascades to *everything* cannot pass — ABANDON is the control. Red before, green after;
removing the call from PostgreSQL alone fails only the PostgreSQL arm.

**This is a behaviour change, not only a fix.** Terminating a parent now fails its `TERMINATE`
children and flags its `REQUEST_CANCEL` children, where before it did neither. That is what the
design says should happen, and it removes the orphan the policy exists to prevent — but a
deployment relying on terminate being narrow will see children close that did not close before.

### 3.81 The defer segment — 🔶 **MECHANISM BUILT AND MEASURED; the two-phase transition remains** (WS-3, 2026-09-02)

§3.75 answered the record-shape question phase 5 was parked behind, and its answer stands: no
new durable record is needed, the terminal transition becomes two-phase, and the only new
durable state is a workflow-level marker on the existing reaper. Implementation started against
that design on 2026-09-02 and stopped at the first measurement, which found a hole in the step
*before* the record shape.

**§3.75 records one mechanism as already working, and it is not.** Its inventory excludes
`RequestCancellation` on the grounds that *"it sets `cancellation_requested`, the guest observes
it and exits through its own wrapper, so that path already runs defers."* Half of that is true.
Measured against a real Go SDK guest on wasmtime, `testdata/deferfunc`, pinned by
`engine/defer_phase_needs_a_signal_test.go`:

| | measured | meaning |
|---|---|---|
| refuse a fresh call → does the guest drain? | **3 fresh-call attempts** (body + both defer bodies) | ✅ the unwind-and-drain half holds |
| do the defer bodies' calls reach the caller? | **0 recorded calls** | ❌ they are refused by the same check |
| a sleeping workflow, replayed | **0 fresh-call attempts**, re-suspends | ❌ the refusal never fires at all |

**Refusing every fresh call does not skip the cleanup, it consumes it.** `_cleatRunDeferred`
takes the defer table before running anything, so the drain empties the table and every call
those bodies make is refused. The lock is not released, the charge is not refunded, and the
registrations are gone. That is worse than not running the defer phase, and it is what §3.75's
design does if built as written.

**Choosing the observable is the whole of the measurement, and the obvious one is blind.** A
`mockCaller` sees nothing in either case, because a defer body's `DurableCall` is refused before
it reaches the caller — "no calls recorded" is what a working drain and a broken one both look
like. The count of fresh-call *attempts* (`keyedCancellationStore.queriedWith`, one poll per
attempt, taken before the refusal) is what separates them. The first version of this measurement
asserted on the caller and reported the premise false; the drain had been working the whole time.

#### What is actually missing

The host has to distinguish **a call made by the workflow body** from **a call made by a defer
body**, and today it cannot. The guest knows — `_cleatInDeferPhase` (`wasm/exports.go:307`) is
what phase 4's guards already test to refuse `cleat_defer` and `continue_as_new` from inside a
defer body — but it is guest-side only and the guest never tells the host.

That signal is phase 5's real prerequisite, and it is independently required by §3.35's own
design, which asks for a defer's host calls to be recorded *"as new events after the workflow's
terminal step, in a distinguishable phase"*. Nothing in the tree can distinguish them.

**Second obstacle, and it is the common case.** A workflow worth terminating is usually one that
is waiting. Replay it and it re-suspends on its recorded sleep without ever reaching the end of
history, so no fresh call is ever attempted and nothing starts the defer phase. A defer segment
cannot be "replay until the first fresh call"; it has to handle a replay that ends in a
suspension. Note that this is the *helpful* direction once the signal exists:
`writeRunDeferred` is gated on `if !__susSuspended`, so a suspended guest deliberately does
**not** drain — the table survives, and the host can call `__cleat_run_deferred` on the live
instance with the refusal lifted, which is exactly phase 4's existing machinery.

#### What was built instead — ✅ `WithDeferPhase`, 2026-09-02

The obstacle is the mechanism. `writeRunDeferred` emits the guest's own drain under
`if !__susSuspended` (`wasm/exports.go`), so **a suspended guest deliberately does not drain** —
its defer table is still populated and its closures are still in the instance's memory. The host
calls `__cleat_run_deferred` itself, on the live instance, at the suspension. Nothing is refused,
so nothing consumes the cleanup, and the defer bodies' calls go through with ordinary semantics.

`WithDeferPhase()` marks an execution as a defer segment;
`wasmtimeBackend.runGuestDefersAfterSuspend` does the drain at all three of the backend's
suspend returns. Measured on a real Go SDK guest that registered a defer and slept:
`defers_run=1`, and the cleanup call reaches the `ServiceCaller`. Pinned by
`TestADeferSegmentDrainsOnTheSuspension`, whose control is the test directly above it — same
fixture, same history, no defer phase, no cleanup — and which goes red with
"the defer segment recorded []" when the drain is removed.

**No SDK change was needed, and the guest→host signal this section originally called phase 5's
prerequisite is not one.** It was the prerequisite for the *refusal* route, and the refusal
route is the one that consumes the cleanup. Note the shape of that error: the fix was reached by
asking what the guest already does on the path in question, not by adding a way to tell it
something.

#### What is still open

1. **The replay that runs past the end of history** — measured, and worse than this section
   first described. See §3.83. A workflow finalized `ready` rather than suspended replays to the
   frontier and then does *new* work, which a defer segment must not permit. Refusing the call is
   what this section measures as destructive, so the fix is to make that call **suspend**
   instead, after which the guest unwinds with `__susSuspended` set, skips its own drain, and
   lands on the path above.

   ~~bit 62 of the result word, exactly as `cleat_await_child` and `cleat_await_any_child`
   already decode it — the same bit rather than a new convention.~~ **That is wrong and would
   collide with a real response**; §3.83 has the measurement and the bits that are actually
   available. Reusing an established convention was the whole appeal of the proposal and it is
   what made it wrong: `cleat_await_child` packs a 32-bit length at bits 32-63, this layout packs
   a 24-bit one at bits 40-63, and the same bit means 1 GiB in the first and 4 MiB in the
   second.
2. **§3.75's two-phase terminal transition** — migration `038` across all three dialects
   (postgres, mysql and mssql high-water marks are 034/033/037 and are **not** aligned), the
   marker and deadline, the three `UPDATE`-driven sites, `reaperLoop`'s predicate, and the
   caller-visible `terminating` status that D6 in `tiers.yaml` settled. This is the largest
   piece and the most obviously shaped like progress; it is now unblocked, because the defer
   segment it dispatches into exists and is measured.
3. **The wazero path has no defer segment.** `WithDeferPhase` fails closed on a backend that
   cannot honour it rather than silently skipping the drain, so `cleatctl replay` and the other
   tooling paths report the refusal instead of reporting a cleanup that did not happen.

---

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

### 3.83 The sentinel §3.81 specified would collide with a real response — 🔶 **CORRECTED AND BUILT for `cleat_call` on Go guests; four SDKs and four other call paths remain** (WS-3, 2026-09-02)

> **Scope, added 2026-09-02 the same day: this closed the path its test covers, not the frontier.**
> `PluginCall`, `PluginCallStreaming`, `ChildWorkflow` and `DurableAwaitSignals` still start new
> work in a defer segment, and bit 39 is not available in their result layouts. **§3.84** has the
> per-layout measurement and the universal sentinel that replaces this one.

§3.81 left phase 5 with one correctness gap and named the fix in a sentence: make the
past-the-frontier call suspend, using *"bit 62 of the result word, exactly as
`cleat_await_child` and `cleat_await_any_child` already decode it"*. Reusing an established
convention was the whole appeal. It is also what made it wrong.

#### First, the defect is worse than §3.81 described

Measured 2026-09-02 on a defer segment with no recorded history — the past-the-frontier case in
its purest form — pinned by `TestADeferSegmentPastTheFrontierDoesNewWork`:

```
operations reaching the ServiceCaller: [body second first]
result: {"status":"ok"}   suspended: false   err: nil
```

| | |
|---|---|
| `body` ran | a defer segment performed the workflow's own side effect, not its cleanup |
| the segment **returned a successful completion result** | a terminated workflow is reported as having finished normally, by the machinery meant to clean up after it |

§3.81 recorded only the first. The second is the more serious: a wasted side effect is a bug, a
terminated workflow recorded as `done` is a status defect on the same axis as §1.1.

#### Why bit 62 is not available here

The two calls it was borrowed from pack a **32-bit** length at bits 32-63, where bit 62 means a
length of 1 GiB. `cleat_call` packs a **24-bit** `responseLen` at bits 40-63 (`packDurableCallResult`,
`engine/memory.go`), where the same bit means **4 MiB**.

4 MiB is reachable. `MaxWasmStringLen` and `OutBufSize` are package vars, not constants, set from
`cmd/cleat-worker`'s `-wasm-max-string-len` and `-wasm-output-buffer-size` — both `flag.Int` with
no upper-bound validation (`cmd/cleat-worker/config.go:127-128`, defaults 64 KB and 32 KB). An
operator who raises them past 4 MiB gets legitimate responses that decode as a suspend.

**This is the ABI-boundary class CLAUDE.md names**, and in the shape it names: not an overflow,
but a value that means something different on each side of the boundary. It is also the class
that file says a property test over the boundary finds faster than reading the sites.

#### What is available, and structurally rather than by a bound

Bits **16-39**. The guest decodes a 32-bit `callErrorCode` from bits 8-39, but the host parameter
is a `byte`, so 24 of those bits cannot be filled by any input at all — an asymmetry already
pinned by `TestPackDurableCallResult_HostCannotFillCallErrorCode`, which had recorded it as a
harmless curiosity ("nothing is broken -- it means the error-code space is 8 bits wide in
practice").

Measured exhaustively over the two byte fields and strided over the 24-bit one — the union of
every result the host can produce is **`0xffffff000000ffff`**. Re-derive with
`TestPackDurableCallResult_SentinelBitsTheHostCannotReach`, which asserts both halves: that bits
16-39 are free, and that bit 62 is **not**, so the day the layout changes the plan stops saying
something false. Proved able to fail both ways before landing — widening `callErrorCode` past a
byte fails the first, narrowing `responseLen` to 22 bits fails the second.

A sentinel there is safe regardless of how the buffer flags are set, which bit 62 is not. The
distinction that matters is not "wider margin" but **structural versus contingent**: bits 16-39
cannot be filled because of the function signature, not because a limit currently sits below
them.

#### What was built

`callSuspendSentinel` (`engine/memory.go`), **bit 39**, returned by `DurableCall` and
`DurableCallWithRetry` when `stopBeforeNewWork()` holds — a defer segment, a call the workflow
*body* is making, past the frontier. The guest unwinds with its suspend flag set, skips its own
drain, and the host runs `__cleat_run_deferred` on the live instance.

The value is exactly `1<<39` with every other field zero, so both SDK idioms recognise it: Rust's
whole-word `result == SUSPEND_SENTINEL` and AssemblyScript's mask. New decoders should mask.

**The host needs no guest signal, which §3.81 assumed it would.** The host invokes the defer
runner itself, so it brackets that call with `inDeferDrain` and already knows which side of the
boundary a call comes from. That was the "real prerequisite" §3.81 named, and it did not exist.

`TestADeferSegmentPastTheFrontierRunsOnlyTheDefers` asserts **both halves together**, and has to:
"the body was stopped" is satisfied by a segment that stops the cleanup too, which is exactly
§3.81's destructive outcome. Proved able to fail both ways — removing the sentinel gives *"the
segment did not suspend; it returned `{"status":"ok"}`"*, and removing the `inDeferDrain` bracket
gives *"the defer bodies recorded `[]`, want `[second first]`"*, which is §3.81's consumed
cleanup reproduced exactly.

#### Four SDKs cannot hear the stop, and are refused rather than run

Only the Go SDK decodes it so far (`wasm/adapter_metadata.go`). **An SDK that does not reads the
word through the ordinary layout and gets `responseLen=0, errCode=0` — an empty *successful*
response.** It would carry on past the stop, do the new work, and report the terminated workflow
as completed, silently.

So `deferSegmentLanguages` (`engine/engine.go`) gates the segment and the engine refuses a
language not in it. Membership means *verified to unwind on the sentinel*, in the same sense as
`WasmtimeLanguages`.

**The test for that gate was vacuous when first written, and its own mutation is what caught
it.** It carried a polite `t.Skipf("%s now decodes the sentinel; move it to the positive test")`
— so widening the map to all five made every subtest *skip*, and the suite printed `ok`. A guard
that cannot fail when the thing it guards is removed is not a guard.
`TestDeferSegmentLanguagesIsExactlyWhatHasBeenVerified` now pins the map exactly and the skip is
gone; both tests fail under that mutation.

#### What is still open

1. **The four remaining SDK decodes** — Rust, Python, Java, AssemblyScript. Each is a constant
   and a masked check before the fields are read, plus one test that crosses the boundary; add
   the language to `deferSegmentLanguages` in the same change, which
   `TestDeferSegmentLanguagesIsExactlyWhatHasBeenVerified` forces to be deliberate.
2. **`(*execSession).setDeferDrain` is in `scripts/deadcode-baseline.txt`.** It is not test-only:
   it is called from `runGuestDefersAfterSuspend` through an anonymous-interface assertion, which
   the dead-code analyser cannot follow. `Execute` takes `session HostHandler`, so the backend
   never sees the concrete type, and putting the method on that exported interface would break
   external implementors. Baselined rather than restructured, and this is the record of why.

---

### 3.84 A defer segment is stopped on `cleat_call` only; four other paths still start new work — ✅ **FIXED** (WS-3, 2026-09-02)

§3.83 stopped a defer segment doing new work through `cleat_call`, and that is the path its
fixture exercises. It is not the only way a guest starts work past the frontier. Measured by
reading the fresh paths, 2026-09-02:

| entry point | fresh path guarded by `stopBeforeNewWork()`? |
|---|---|
| `DurableCall` | ✅ §3.83 |
| `DurableCallWithRetry` | ✅ §3.83 |
| `PluginCall` (`engine/plugins.go:175`) | ❌ |
| `PluginCallStreaming` (`engine/plugins.go:366`) | ❌ |
| `ChildWorkflow` / `ChildWorkflowWithOptions` (`engine/children.go:15,19`) | ❌ |
| `DurableAwaitSignals` (`engine/signaller.go:12`) | ❌ |

So a terminated workflow's defer segment can still start a **child workflow** or call a
**plugin**. §3.83's own section should be read with that scope: it closed the path its test
covers, not the frontier.

> **The table above is one short, and this section needs the same caveat it applies to §3.83 —
> 2026-09-03.** `Fetch` (`engine/lifecycle.go`) was a seventh unguarded fresh path and is not in
> the inventory, because the inventory was built by reading the entry points a guest uses to
> reach a *service*, and `cleat_fetch` reaches one without going through the durable-call family.
> Fixed in §3.104.
>
> Go guests were never exposed: `cleat/runtime_workflow.go`'s `DurableFetch` routes through
> `DurableCall("http", "fetch", ...)`, which this section already guards. The exposure was to the
> guests that import `cleat_fetch` directly, which is Java and AssemblyScript. Re-derive the
> current set of guarded paths with:
>
> ```
> grep -rn "stopBeforeNewWork()" --include="*.go" engine/ | grep -v _test.go
> ```

Re-derive: `grep -n "func (s \*execSession) \(PluginCall\|ChildWorkflow\|DurableAwaitSignals\)" -A 6 engine/*.go`

#### Why this is a mechanism rather than four more `if` statements

Those paths return **different result layouts**, and §3.83's bit 39 is not available in them — it
sits inside `packSimpleResult`'s `runIDLen`. Reusing it would repeat exactly the mistake §3.83
exists to record, one layout over.

Measured by unioning every result each packer can produce, pinned by
`TestStopSentinelBitsAcrossEveryLayout`:

| packer | reachable | free |
|---|---|---|
| `packDurableCallResult` | `ffffff000000ffff` | `000000ffffff0000` |
| `packSimpleResult` | `00ffffff000000ff` | `ff000000ffffff00` |
| `packAwaitChildResult` | `00ffffff000000ff` | `ff000000ffffff00` |
| `packAwaitSignalsResult` | `ffffffff0001ffff` | `00000000fffe0000` |
| `packAwaitPromiseResult` | `00ffffff0001ffff` | `ff000000fffe0000` |
| `packAcquireLockResult` | `000000000000ffff` | `ffffffffffff0000` |
| `packSleepResult` | `ff0000fffffffc00` | `00ffff00000003ff` |

**There is no bit free in all seven.** `packSleepResult` is what rules it out — and it is also
the one that needs nothing, because a guest waiting on a sleep already suspends through its own
status byte (`TestASleepingWorkflowNeverReachesAFreshCall`). Excluding it, **bits 17-31 are free
in all six layouts that can start new work**, which is where a universal stop sentinel belongs.

§3.83's bit 39 is `cleat_call`-specific and correct only there. That is worth stating plainly
rather than quietly widening it later: the two facts look like the same fact and are not.

#### What to build

One sentinel at bits 17-31, honoured by every fresh path in the table above, replacing bit 39
rather than sitting beside it — ABI.md documents bit 39 today, so the SDK-visible contract
changes once and only once. The guest side is the same masked check §3.83 added, applied at each
decode site.

The test has to exercise a path that is **not** `cleat_call`, or it re-measures §3.83. A child
workflow is the clearest: `testdata/deferfunc` would need an entry point that starts one, and the
assertion is that no child instance row is created while the defers still run.

### 3.85 `--max-quota-events` killed the worker process, and the cap never counted the workflow — ✅ **FIXED** (WS-3, 2026-09-02)

`cleat-worker --max-quota-events N` is meant to bound a runaway workflow: at N events the engine
records a `ContinueAsNew` and the workflow restarts as a fresh run with a reset count. Measured
2026-09-02 on wasmtime with a Go SDK guest, it did something else — it segfaulted the worker
**process**, and it did so on a workflow that stays in the queue, so the next worker to claim it
died too.

Re-derive the crash on the tree before this fix:

```
git stash && go test ./engine/ -run TestTheEventCapContinuesAsNewInsteadOfKillingTheWorker
```

Note what that prints: not a red assertion but

```
--- FAIL: TestTheEventCapContinuesAsNewInsteadOfKillingTheWorker
panic: runtime error: invalid memory address or nil pointer dereference
[signal SIGSEGV: segmentation violation code=0x2 addr=0x10]
```

#### Two nil dereferences, and the first one hid the second

1. `freshCall` called `m.CloseWithExitCode(ctx, 0)` on the `api.Module` it was handed.
   `engine/wasmtime_hostfuncs.go:48` passes **`nil`** for that argument — every wasmtime host
   function does — so on the only backend a worker has, this was a method call on a nil
   interface. The wazero path passes a real module, which is why the CLI tooling never showed it.

2. That panic did not fail the workflow. `ContinueAsNew` on the line above had already set
   `session.suspendErr`, and `engine/executor.go`'s error branch is guarded by
   `callErr != nil && session.suspendErr == nil` — a deliberate "a suspension wins over the error
   that came with it". So control fell through to `res.Suspended` with **`res` nil**, because
   every error return in the wasmtime backend is `return nil, err`. That dereference is inside no
   `recover`.

The second is the one that kills the process, and it is not covered by the first: measured by
reverting one fix at a time, the refusal alone still segfaults, because a refused guest returns
an error and the backend answers `return nil, err` for that too. Both fixes are load-bearing.

#### The fix is a refusal, not a new ABI

The cap branch now writes `eventCapCallError` and returns `errCode=1` instead of closing the
module and handing the guest an empty **success**. A refusal needs no module handle and no new
sentinel: the guest's own error path unwinds it and drains its defer table on the way out, which
is the same shape an explicit `ContinueAsNew` already has, where the guest returns normally
through the wrapper that runs the defers. `suspendErr` is already set, so the executor still
reports a `continue_as_new` suspension rather than the error the guest returned.

This is deliberately *not* §3.84's stop sentinel. §3.84 needs a sentinel because a defer segment
must stop a guest that is *replaying* without failing it; the event cap is ending the run either
way, so the mechanism §3.83's tests already measured — "a refused fresh call makes the guest
unwind and drain" — is sufficient and is language-agnostic, which the sentinel is not.

#### The cap was also measuring the wrong thing

`executeWithBackend` — the backend path, the one a worker takes — never set two `execSession`
fields that `executeCompiled` (wazero, CLI tooling) does. Both defaults are silently wrong:

- **`eventCount`** restarted at `0` every segment instead of seeding from
  `WithInitialEventCount`, which `cmd/cleat-worker/setup.go:1682` loads from the stored
  `event_count`. So a cap meant to bound a whole workflow bounded a single segment of it, and a
  workflow that suspends often never reached it at all. Nothing reports a cap that does not fire.
- **`originalInput`** was `""`, and `freshCall` records the `ContinueAsNew` with it. A workflow
  the cap continued therefore **restarted with no input**. The continued run is a valid run of the
  same workflow, just one handed `""` instead of its arguments — so there is nothing to see beyond
  the wrong behaviour.

Both were one omission in one struct literal. The second is the more dangerous, and it is the
reason this section is not filed as a crash fix: the crash is loud, and losing a workflow's input
across a continue-as-new is not.

#### Tests

`engine/eventcap_test.go`, all three on real wasmtime with the Go SDK:

| test | proved able to fail by |
|---|---|
| `TestTheEventCapContinuesAsNewInsteadOfKillingTheWorker` | reverting either guard — SIGSEGV, twice |
| `TestTheEventCapDoesNotDispatchTheCallItRefused` | dropping the `eventCount` seed; the trip lands on a defer body and the workflow body's call is dispatched |
| `TestTheContinuedRunKeepsTheWorkflowInput` | dropping the `originalInput` seed; input comes back `""` |

The first test's second assertion is the half that would otherwise be missing: it requires **2**
dispatched calls, both defer bodies. Asserting only that the refused call is absent passes just
as well against a guest that was killed outright and ran no cleanup — which is what the old
`CloseWithExitCode` did on the runtime where it worked.

#### What was built — 2026-09-02

One sentinel, **bit 31**, replacing bit 39 rather than sitting beside it, honoured by all six
paths and decoded by seven Go SDK call sites.

Host side, each after its own replay check so a replaying guest is untouched:
`DurableCall`, `DurableCallWithRetry` (already), `PluginCall`, `PluginCallStreaming`,
`childWorkflowWithVersion` — which serves both `ChildWorkflow` and `ChildWorkflowWithOptions` —
and `DurableAwaitSignals`.

Guest side, `wasm/adapter_metadata.go`: the check moved out of `DurableCall`'s literal into
`withSuspendCheck`, which every stoppable decoder's `ResultStmts` now goes through. That is a
mechanism rather than seven copies for the reason CLAUDE.md gives — a decoder added later gets
the check by construction, and the ordering cannot be got wrong per-site.

#### `DurableCallWithRetry` was already broken, and this is how it was found

The host returned the sentinel from `DurableCallWithRetry` (§3.83, `engine/durablecalls.go:302`)
and **the Go SDK never decoded it there**. A workflow body that used the retrying call in a defer
segment read bit 39 as `responseLen = 0, errCode = 0` — an empty successful response — and ran
on. That is precisely the failure `deferSegmentLanguages` exists to prevent, inside the one
language the gate declares supported.

Nothing caught it because the gate is per-*language* and the omission was per-*call*. Worth
stating as a general shape: a fail-closed list keyed on one axis says nothing about a gap on
another. The `withSuspendCheck` wrapper is the answer to that, not a wider list.

Re-derive that it is closed: `grep -c "withSuspendCheck(" wasm/adapter_metadata.go` — **8**: one
per stoppable call plus the `func` that defines it. The first draft of this line said 7 and was
wrong for that reason, which is the rule two sections up about running the command before writing
the sentence about what it prints. For the seven call sites alone,
`grep -c "ResultStmts: withSuspendCheck("`.

#### Why the bit moved, and which test holds that up

"Free" was measured in #595 as *the host cannot produce it*. That is necessary and not
sufficient, and the gap between the two decided the design:

- Bit 39 is free in `packDurableCallResult` alone. In `packSimpleResult` and
  `packAwaitChildResult` it sits inside a 32-bit length at bits 32-63, where it means a
  **128-byte run ID** — an ordinary value a real child workflow produces.
- In the await-signals layout, bits 17-31 fall inside the **timed-out field**, which the Go SDK
  reads as `(r>>16) & 0xFFFF != 0`. A decoder that filled its fields before testing the sentinel
  would read a stop as an ordinary timeout and continue. So the ordering in `withSuspendCheck` is
  a contract, not a style choice, and ABI.md says so.

`TestStopSentinelBitsAcrossEveryLayout` now asserts `callSuspendSentinel` lies inside the region
free in all six layouts, and that it sets exactly one bit. **That is the only test that holds the
bit choice up** — measured 2026-09-02, setting the constant back to `1<<39` and updating the
decoder to match leaves `TestADeferSegmentDoesNotStartAChildWorkflow` green, because both sides
still agree and no end-to-end run produces a 128-byte run ID. The end-to-end test proves the stop
reaches a second path; the property test proves the bit is the right one. Neither substitutes for
the other, and the comment on each says which.

#### The new end-to-end test, and why a child workflow

`TestADeferSegmentDoesNotStartAChildWorkflow` with a new `defer_child_workflow` entry point in
`testdata/deferfunc`. A child workflow is the sharpest of the four: a durable call's side effect
belongs to another service, but a child workflow writes a row into this system's own
`workflow_instances`, scheduled and claimable, outliving the segment. A defer segment for a
terminated workflow that starts one has manufactured live work on behalf of a workflow that is
over.

The observable is the **store**, not the `ServiceCaller` — a child workflow never reaches the
caller, so `operationsCalled` cannot see one either started or refused. Same problem as the
cancellation probe at the top of that file, one path over.

Both halves are asserted together, as in §3.83: no child started, **and** the defer body's call
still made. Proved able to fail 2026-09-02 by removing the host guard (child starts) and by
removing `withSuspendCheck` from the `ChildWorkflow` decoder (the run fails instead of
suspending).

#### Still open

The four non-Go SDKs. `deferSegmentLanguages` remains `{"go": true}`, so a defer segment for a
Rust, Python, Java or AssemblyScript guest fails closed rather than silently running the body.
Their `SUSPEND_SENTINEL` is `1 << 62`, which is the *await-child* sentinel and a different
mechanism; none of them decodes the stop. Each language's decode and its entry in that map belong
in one change, with a test that crosses the two halves (§3.73).

### 3.87 The Rust SDK cannot suspend: `catch_unwind` never catches, and the host has been masking the trap — ✅ **FIXED** (WS-3, 2026-09-03)

`crates/cleat-sdk` suspends by `std::panic::panic_any(SuspendSentinel)`, and `#[cleat_entry]`
wraps the workflow body in `std::panic::catch_unwind` to intercept it and return
`memory::SUSPEND_SENTINEL` to the host. `crates/cleat-sdk/README.md` describes exactly that:
"catch_unwind so `SuspendSentinel` panics are safely intercepted".

**`wasm32-wasip1` builds with `panic=abort`.** There is no unwinding on that target, so
`catch_unwind` cannot catch anything: the panic aborts, which in WASM is the `unreachable`
instruction, which is a trap. Every Rust suspension is a trapped guest.

Ask the compiler rather than believing this paragraph:

```
rustc --print cfg --target wasm32-wasip1 | grep panic     # panic="abort"
```

#### Why it has never looked broken

The paths that suspend also set `session.suspendErr` on the **host** side — `cleat_sleep`,
`cleat_await_child` and `cleat_await_signals` all record their own suspension before returning to
the guest. `engine/executor.go` then deliberately lets a suspension win over the error that
accompanied it:

```go
if callErr != nil && session.suspendErr == nil {   // <- the trap is discarded here
```

So the trap is thrown away and the run is reported as a clean suspension. The guest died; the
workflow suspended; nothing anywhere says so. That is the same "a signal that is accurate but
attached to the wrong question" shape as §3.85's `res == nil`, one layer up — and it is why
§3.85's guard matters beyond the event cap it was found through.

The mask holds only where the host sets `suspendErr` itself. §3.84's stop sentinel does not: the
host refuses the call and leaves the guest to unwind on its own. **That is how this was found** —
adding the Rust decode for §3.84 turned an invisible trap into a visible one.

#### Measured

`engine/rust_suspend_test.go`, against a real `wasm32-wasip1` release build on wasmtime, with two
new entry points in `examples/rust-workflow` that exist only for it:

| test | entry point | result |
|---|---|---|
| `TestARustGuestCannotSuspendByPanicking` | `suspend_probe` — raises the SDK's suspend panic with no host call in the way | **traps**, `wasm trap: unreachable` |
| `TestThePanicTrapIsMaskedWhereverTheHostSetsSuspendErr` | `sleep_probe` — the same panic, reached through `cleat_sleep` | **clean suspension**, `err == nil` |

The pair is the point; either alone is misleading. The first without the second reads as a bug in
one probe. The second without the first reads as a working SDK.

`TestTheRustTargetCompilesWithPanicAbort` pins the mechanism by asking `rustc`, so the day the
target gains unwinding the assertion fails and points here.

The masking test needs `WithWorkflowStartTime` and `WithClock` pinned, and that is worth
recording rather than just doing. `seedNowMs` falls back to the package-level `nowMs` atomic when
there is no history and no start time, and other tests in `engine` move it; with a small seed the
sleep's deadline is already past, `DurableSleep` returns "completed" rather than suspending, and
the test fails **only when run alongside them**. It passed in isolation and failed in the full
suite, which is the reading this whole section is about, one level up.

#### What this blocks, and what it does not

It blocks **"rust" joining `deferSegmentLanguages`**, which is the §3.84 follow-up this was
started as. The gate is fail-closed, so a Rust guest is refused a defer segment today rather than
silently running its body — the correct behaviour, arrived at for a different reason than the one
recorded in §3.83.

It does not obviously block ordinary Rust workflows *today*, because the mask is load-bearing and
works. What it costs is unquantified and should not be guessed at here: a trapped guest's linear
memory is abandoned rather than unwound, so anything a Rust workflow expects `Drop` to do between
a suspension and the next segment does not happen. Measure that before claiming either way.

#### FIXED 2026-09-03 — suspension is a return value

Requested by WS-2, who were blocked: the §3.84 sentinel decode in Rust is
pointless until there is something to unwind into.

`CallError::{Suspended, Failed}` replaces the panic. The seven host calls that
can suspend return `Result<T, CallError>` and workflows propagate with `?`, so
**the compiler enforces the early return** — a body cannot use a value that was
never produced. `impl From<CallError> for String` keeps `?` working in the usual
`Result<T, String>` workflow signature.

A thread-local flag is the backstop, not the mechanism, for the one case the
type system cannot reach: `let _ = h.cleat_sleep_ms(..)` followed by a value of
the body's own. `#[cleat_entry]` clears it before the body, checks it after, and
checks again after the defer drain (a defer body can suspend too).

**`defer.rs` was broken the same way and is fixed with it** (both together, as
scoped). Its `catch_unwind` isolated a panicking cleanup body — dead under
`panic=abort`, so the first panicking defer aborted the instance and took the
remaining bodies with it. Bodies are now `FnOnce() -> Result<(), CallError>`.
A defer body still must not `panic!`: nothing can catch it on this target, and
that is documented rather than worked around.

**What the tests are, and one that was deleted.** The mask is why the shape
matters. `suspend_probe` sets the flag with **no host call in the way**, so
nothing sets `session.suspendErr` and nothing can hide a trap —
`TestARustGuestSuspendsCleanly` is therefore the mask-free regression test, and
under the old SDK its equivalent trapped with `wasm trap: unreachable`.

A Go-side test of the *backstop* was written and then removed, because it could
not fail: every suspending host call sets `suspendErr`, and `executor.go`
returns a `SuspendResult` with an empty result string whatever the guest
returned, so a discarding body looks identical through `Execute` either way.
Measured — with both flag checks deleted from `#[cleat_entry]` the Go assertion
still passed. The property is real and is now tested where it is observable,
`crates/cleat-sdk/tests/suspend_backstop.rs`, which calls the generated export
directly and reads its return value. Four tests, proven to fail: disabling both
flag checks fails three and leaves the control passing; removing only the
pre-body `clear_suspended()` fails exactly the leak test.

**`#[cleat_test]`'s `catch_unwind` is gone too, and it was the reason this
survived.** It runs on the HOST target, where unwinding works — so the
suspension mechanism tested green natively while the shipped `wasm32-wasip1`
build trapped. A test suite passing on a target the product does not ship is
this file's recurring shape, one layer further out.

`TestTheRustTargetCompilesWithPanicAbort` stays and its message changed: it no
longer pins why `catch_unwind` fails, but why the SDK cannot use unwinding at
all. If the target ever gains unwinding the `Result` design is still correct —
it just stops being forced.

**Still open, and deliberately not in this change:** adding `"rust"` to
`deferSegmentLanguages`. That is §3.84's follow-up, needs the Rust sentinel
decode WS-2 was blocked on plus a defer-segment test to back it, and the gate is
fail-closed today — a Rust guest is refused a defer segment rather than silently
running its body, which is the correct behaviour to keep until that lands.

#### The fix is a redesign, not a decode

The SDK needs a suspension path that does not unwind: a thread-local "suspended" flag set by the
host-call wrappers, checked by `#[cleat_entry]` after the body returns, with every intermediate
call site returning early. That is a change to the shape of every `HostCalls` method.

#### It is Rust alone — asked as a sweep, and the answer is one fix

The first draft of this section said "AssemblyScript and Python may have the same defect for the
same reason", on CLAUDE.md's "ask whether the answer is a sweep or a mechanism". Checked, it is
not a sweep. **AssemblyScript already has the design proposed above**, and says so in its own
docs (`packages/cleat-as/assembly/defer.ts`):

> Go, Rust, Python and Java all suspend by unwinding — a panic, a raise, a thrown `SuspendSignal`
> — so their drain sits on a path the unwind skips. This SDK has no exceptions, so suspension is
> a *flag* (`isWorkflowSuspended()`) and the entry point returns normally either way.

That grouping is right about the mechanism and misses the distinction that decides this section:
**whose unwinder**.

| SDK | suspends by | unwinder |
|---|---|---|
| Go | `panic(cleat.ErrSuspend)` | the Go runtime compiled into the guest |
| Python | `raise SuspendSentinel()` (`host_calls.py`) | CPython's, inside the interpreter |
| Java | `throw new SuspendSignal()` (`HostCalls.java`) | the JVM runtime's, inside the guest |
| AssemblyScript | a flag, no unwinding at all | — |
| **Rust** | `panic_any` + `catch_unwind` | **the wasm target's, which does not exist** |

Every other language ships its own unwinder inside the guest binary. Rust is the only one that
asks the *target* for it, and `wasm32-wasip1` answers `panic="abort"`. That is why this is one
SDK's defect and not four.

Read rather than run, and the distinction matters: the AssemblyScript conclusion is settled by
construction — a language with no exceptions cannot have an uncaught one. Python's and Java's rest
on where their unwinder lives, which is an argument about how those runtimes are compiled, not a
measurement. What would settle each is the same probe this section already has for Rust: an entry
point that raises the SDK's own suspend signal with no host call in the way, so nothing can set
`session.suspendErr` and mask the result. Neither is installed here — `componentize-py`,
`wasm-tools` and a gradle binary are all absent — so neither was run.

### 3.88 §3.75's two pre-build re-derivations: the inventory is clean, the dead-letter question changed — ✅ **steps 1 and 2 DONE; step 3 (§3.75) not started** (WS-3, 2026-09-03)

§3.75 tells whoever builds it to re-derive two things first. Both were done 2026-09-03. One comes
out clean and one does not come out the way the section expects, so this is recorded before the
migration rather than discovered during it.

#### 1. The per-dialect inventory — clean

§3.75 warns about its own method: the inventory was written by reading the PostgreSQL
implementation and generalising, which is exactly how §3.76 was found (MySQL's
`TerminateWorkflow` released nothing while the other two dialects did). It asks for a per-dialect
re-derivation before step 2, because "the same generalisation could be hiding in the parent-close
arm or `adminForceResolve`".

It is not. All three sites now call the release on all three dialects:

| site | postgres | mysql | mssql |
|---|---|---|---|
| `TerminateWorkflow` | `db.go:1150` | `mysql_ops.go:1200` | `mssql_operations.go:201` |
| parent-close `TERMINATE` | `store_lifecycle.go:499` | `mysql_lifecycle.go:938` | `mssql_lifecycle.go:1106` |
| `adminForceResolve` | `store_admin.go:230` | `:340` | `:450` |

The parent-close arm goes through `releaseTerminatedChildren`, which is one shared helper called
from three places — re-derive with

```
grep -rn "releaseTerminatedChildren(" --include="*.go" engine/ | grep -v _test.go   # 4: 3 callers + the definition
```

So step 1 has **three symmetric sites** to move the release behind the defer segment, not a mix.
That is the answer §3.75 wanted and it is the cheap half of this section.

#### 2. `MoveToDeadLetterQueue` — the question is not the one §3.75 asks

§3.75 reads this site as an exception to its inventory: fenced on a live claim, called by the
worker that holds it after a segment failed, so its defers should already have run. It says that
is "a reading of the call site, not a measurement", and asks for the measurement — "If it turns
out a workflow can reach the dead-letter queue without a segment having executed, it is a fourth
row rather than an exception."

Trying to build that measurement turned up something else. The worker's entire dead-letter
predicate is a substring match:

```
grep -n 'strings.Contains(errMsg, "retries exhausted")' cmd/cleat-worker/setup.go   # 2544
```

and that substring is minted in exactly one place — the **host-side** retry loop, behind the
`cleat_call_retry` import:

```
grep -n '"retries exhausted: "' engine/durablecalls.go                              # 422
```

A Go guest does not appear able to reach it. `wasm/usage.go:73` wires that import on the guest
symbol `DurableCallWithRetry`, and no such method exists:

```
grep -c "func (h \*HostCallsImpl) DurableCallWithRetry" cleat/runtime.go            # 0
```

The only guest-facing form is `DurableCallWithOptions` with `opts.Retry`, whose host branch is
guarded on the import having been wired — so it falls back to SDK-level retry and mints its own
message ("retry exhausted after N attempts"), which the predicate does not match. Rust's
`HostCalls::cleat_call_with_retry` calls the import directly, so the substring is reachable
there.

**That last sentence was a reading of a call chain, and it is now measured.**
`engine/rust_host_retry_test.go` runs a Rust guest whose `retry_probe` entry point exhausts a
2-attempt policy against an always-failing service, and asserts three things that only hold
together on the host path: **no suspension**, **two attempts dispatched inside the one segment**,
and a terminal error containing `retries exhausted`. All three hold. So the divergence is real
and measured on both sides — the identical policy is one segment and dead-letterable on Rust,
N segments and not dead-letterable on Go.

The Go half of the same assertion is written the other way round in
`TestAnExhaustedRetryRunsItsDefersAndIsNotDeadLetterable`, deliberately: each asserts its own
SDK's current behaviour, so whichever one changes first fails and names the other.

If that holds, the answer to §3.75's question is not "its defers already ran" but **"nothing on
the Go SDK reaches it"** — and `MoveToDeadLetterQueue` still needs no marker, for a different
reason, with a separate defect behind it.

#### Why this is 🔵 and not a fix

**The measurement is not finished, and the honest thing is to say which parts are which.**

Measured, one run on wasmtime with a Go guest: a workflow using `DurableCallWithOptions` with a
2-attempt policy against an always-failing service made **one** attempt and suspended, and its
terminal error carried the SDK-level message rather than the host's. Read, not run: everything
above about *why* — the usage-driven wiring, the missing method, the Rust contrast.

What was not established at the time was why the run suspends after one attempt instead of
exhausting. **Settled 2026-09-03; see the step 1 result below.** A test was written and then
reverted rather than shipped, because it would have asserted a state whose mechanism its author
could not explain — a green assertion over an unexplained state is worse than no test, because it
reads as understanding.

#### Step 1, settled — an SDK retry backoff suspends the workflow

`cleat/runtime.go`'s SDK-level retry loop backs off with `h.DurableSleep(backoff)`, a **durable**
sleep, which suspends whenever its deadline is ahead of the engine's wall clock. So an SDK-level
retry is not a loop inside one segment: **every backoff suspends the workflow and the retry
resumes in the next segment after replay.** A 3-attempt policy with 1s backoffs is three segments,
each replaying the history so far. Nothing had written that down.

Why the earlier clock pinning failed is the part worth keeping, because it is what cost the
session. `DurableSleep`'s anchor is `max(session nowMs, Now())`, and `Now()` reads the **last
recorded event's** timestamp — which the first attempt's call event has just written at real wall
time. That overrides any `WithWorkflowStartTime` seed in the past, so a clock pinned relative to
the seed is *behind* the anchor and the sleep suspends anyway. A backoff completes only when the
clock is ahead of **real** time.

Measured, `engine/retry_backoff_test.go`, both directions asserted because the failing one is the
one that misled:

| clock | outcome |
|---|---|
| pinned behind the deadline (`t0`, ~3y) | 1 attempt, suspends, reason `cleat_sleep(1ms)` |
| seeded start + clock ahead of the seed | same — the event timestamp wins |
| clock a day ahead of real time | exhausts in one segment |
| **real** | **either — a 1ms race, see below** |

**The `real` row was the first row of this table, asserted as a property, and it is not one.**
The fixture's backoff is 1ms and the suspend decision is `s.nowMs <= realNowMs()` with
`s.nowMs = anchor + 1ms`, so on a real clock the question is whether under one millisecond of
real time passed between `recordEvent` stamping `time.Now()` (`engine/lifecycle.go:153`) and the
deadline being evaluated. It suspended locally and on #609's own CI, then completed under load on
#610 — exhausting in-segment and failing with the message the *other* test expects. Fixed by
pinning that test's clock behind the deadline, trading a 1ms margin for a margin of years;
reproduced both ways first (`+5ms` → `ops=[op op after_exhaustion]`, exactly CI's failure). The
two tests now differ only in which side of the deadline the engine clock sits, with no timing in
either.

**Step 2's measurement follows from it**, and is in the same file: with the backoffs completing,
an exhausted policy runs its defers on the way out (the cleanup call is dispatched) and its
terminal error carries the SDK's own `retry exhausted after 2 attempts` — **not** the worker's
`retries exhausted` predicate. So the reading above holds: a Go workflow that exhausts its retries
is not dead-letterable, and `MoveToDeadLetterQueue` needs no marker because nothing on this SDK
reaches it.

That leaves item 2 of the order below as the live question, and it is a product one: whether the
Go SDK should expose the host retry loop at all. If it should, the dead-letter queue becomes
reachable and §3.75's original question needs answering with this same fixture.

#### What to do next, in order

1. ~~Settle the suspension~~ — ✅ done 2026-09-03, `engine/retry_backoff_test.go`. It is the
   backoff's own durable sleep; see the step 1 result above.
2. ~~Decide whether the Go SDK should expose the host retry loop at all~~ — ✅ **answered
   2026-09-03; see the decision below.** It should, for short policies.
3. Only then build §3.75 step 1 against the three symmetric sites above.

#### The decision, and what it turned into — ✅ built 2026-09-03

**A retry loop that completes within a few minutes should keep the worker while it runs, the way
non-durable code would.** That is frequent, ordinary behaviour and should not be surprising. Only
longer durations are worth paying multiple segments and a replay for.

Built as a threshold rather than a switch, because both paths are right for different policies:

| policy's worst-case total backoff | path | segments | worker | dead-letterable |
|---|---|---|---|---|
| ≤ `cleat.hostRetryBudget` (60s) | host loop, behind `cleat_call_retry` | 1 | held | yes |
| > that | SDK loop, `DurableSleep` per backoff | N | released | no |

Decided before the first attempt from the policy alone — a policy that switched paths part-way
through would produce a history whose shape depended on which services happened to fail.

Three changes, and the first is the whole of why this never worked:

1. **`wasm/usage.go` now maps `cleat_call_retry` on `DurableCallWithOptions`** (and the JSON
   form). It was mapped only on `DurableCallWithRetry`, a method `HostCallsImpl` did not define,
   so the import was never wired, so `cleat/runtime.go:1079`'s host branch was unreachable and
   every Go retry policy silently became an SDK-level loop. The delegation code was already
   there and had presumably never run.
2. **`HostCallsImpl.DurableCallWithRetry`** exists now — the explicit form, matching Rust's
   `HostCalls::cleat_call_with_retry`. It does *not* consult the threshold: a caller naming it
   has asked for the host loop specifically. It returns an error rather than falling back when
   the import is absent, because silently substituting different durability semantics is how this
   area went wrong in the first place.
3. **`hostRetryBudget = 60s`**, deliberately well below `--wasm-wall-clock-ceiling`'s 5m default
   rather than near it. The ceiling covers the *whole* invocation, so a threshold near it would
   let one retry policy consume the entire budget. The two are not independent: raising either
   means revisiting the other.

Why the ceiling matters here at all is §3.90. The host loop backs off with `time.After` **inside
a host call**, so before that item it spent the guest's execution budget and a policy longer than
30s killed the workflow it was retrying for.

Measured, `engine/retry_backoff_test.go`, both sides of the threshold:

| fixture | policy | outcome |
|---|---|---|
| `defer_on_retries_exhausted` | 2 × 1ms | no suspension, **both** attempts + the defer's cleanup in one segment, terminal error contains `retries exhausted` |
| `defer_on_long_retry_policy` | 3 × 2min | one attempt, suspends with reason `cleat_sleep`, SDK path |

Either alone would pass against a build that always chose one path.

**Operator-visible consequence, and the docs moved with it:** the dead-letter queue is now
reachable from Go — but only through a short policy.
`docs/operations/workflow-retention.md` and `docs/reference/worker-config.md` said "nothing on
this SDK enters that state"; they now carry the threshold table instead.

**The Rust SDK now applies the same threshold** (decided 2026-09-03: "Rust should get the same
behavior... no one is using the SDK yet"). It previously called the import directly for any
policy, so the same `RetryPolicy` held a worker for four minutes on Rust and suspended on Go —
and since §3.90 gave wall clock its own bound, the Rust path would have been *killed* at
`--wasm-wall-clock-ceiling` rather than completing. `cleat_call_with_host_retry` is the explicit
bypass, mirroring Go's `DurableCallWithRetry`.

Pinned from both sides on both SDKs, four tests, and each pair would pass against an
implementation that always chose one path:

| SDK | short policy | long policy |
|---|---|---|
| Go | `TestAShortRetryPolicyRunsOnTheHostInOneSegment` | `TestALongRetryPolicySuspendsInsteadOfHoldingTheWorker` |
| Rust | `TestARustGuestReachesTheHostRetryLoop` | `TestARustLongRetryPolicySuspendsInsteadOfHoldingTheWorker` |

Plus `cleat.TestBothSDKsAgreeOnTheHostRetryBudget`, which reads the Rust constant out of the
source and compares it to the Go one — the threshold is written twice, in two languages, and
nothing at compile time can make the two agree. **That test is a stopgap and says so: §3.94 moves
the threshold host-side, at which point both constants and the test disappear.**

#### The two rewritten tests, and why that is the design working

Both of the tests this section shipped earlier failed the moment the import was wired, each with
the instructions it had been written with:

- `TestAnSDKRetryBackoffSuspendsTheWorkflow` — "if the retry loop stopped using DurableSleep,
  that is an improvement worth recording in 3.88, not a silent change". Replaced by
  `TestAShortRetryPolicyRunsOnTheHostInOneSegment` and
  `TestALongRetryPolicySuspendsInsteadOfHoldingTheWorker`.
- `TestAnExhaustedRetryRunsItsDefersAndIsNotDeadLetterable` — "that is the BEHAVIOUR WE WANT ...
  rewrite this to assert the dead-letter path positively, and revisit §3.75's
  `MoveToDeadLetterQueue` question". Folded into the first of those, asserted positively.

A test asserting a known-broken state is only worth shipping if it says what to do when it flips.
These did, and neither change could have landed silently.

**§3.75's original dead-letter question is therefore live again**, on the merits rather than by
default: a Go workflow *can* now reach `MoveToDeadLetterQueue`, so whether it can get there with
defers outstanding needs the answer §3.75 asked for.

**Do not read this section as clearing `MoveToDeadLetterQueue`.** It moves the reason from one
unmeasured claim to another, better-supported one. The site is still the fourth candidate.
---

### 3.89 Resolving an ambiguous call broke the checksum chain above it — ✅ **FIXED** (WS-2, 2026-09-02)

> **Renumbered twice: §3.86 → §3.88 → §3.89, on 2026-09-03, because both earlier numbers were taken while this branch waited on CI.**
> §3.86 went to WS-1's SQL Server cross-tenant audit (#597, #604, #607), which landed first and
> is cited in three merged commit subjects. §3.88 then went to WS-3's §3.75 re-derivations
> (#606), which landed while this branch was rebasing onto the commit *before* it.
>
> **`scripts/check-section-numbers.sh` passed on this branch both times and could not have
> caught either.** It checks that the numbers within one file are unique, and they were — each
> duplicate lived on `develop`, in a section this branch had never seen. That is the residual
> §3.82 warns about, and it is now measured four times in one day.
>
> **The renumber is not the cost; the re-verification is.** Each collision forced a rebase, and
> each rebase forced a fresh local suite run before pushing, because the tree under the tests
> had changed. Four numbers, four rebases, four runs — for a defect whose fix never changed.
>
> **The mechanism, not the sweep.** Every stream appends its new section to the end of one file,
> so two streams appending on the same day collide in the text *and* in the number, every time,
> by construction. `git log --oneline HEAD..origin/develop` before pushing catches it one
> collision later rather than preventing it. What would prevent it is not allocating the number
> until merge — a placeholder in the branch, resolved when the PR lands — or splitting the
> backlog into per-stream files. Neither is this section's to decide, but recording the count is:
> **four allocations for one section is the cost of the current arrangement, not bad luck.**

**Mine, from #578**, found by checking a contract rather than by a failure report. The bug is
one sentence: resolving a pending row in place gives it a checksum it did not have, and every
row already stored *above* it was chained on that absence.

`previousStoredChecksum` reads the immediately preceding row and gets `""` for a pending one,
because a pending row's `checksum` is `NULL` — and `VerifyWorkflowEvents` walks the history the
same way, resetting the chain whenever a row's checksum is missing. The two agree, which is what
that function's doc comment is about. They stop agreeing the moment the pending row's checksum
is filled in after a later row has already been written against its absence.

**Measured 2026-09-02**, PostgreSQL and MySQL (this sandbox cannot run MSSQL — see
`WS2-STATUS.md`):

    verify events: workflow <id> step 1: checksum mismatch
      (expected e72f419bd3c13b2b, got 9f7782688aa8a164)

**`ResolveCallIntent`'s own doc comment contains the reasoning that stopped being true**, and it
is worth quoting because it was *correct when written*:

> everything before a pending row has been persisted by definition, because the crash that
> created the row happened after them. There is nothing in flight to disagree with.

That reasons about rows **before**. It held while the only caller was phase E's `resolveAmbiguity`,
which resolves the row replay just stopped at — the last row, with nothing above it. Phase F
(#578) added an operator path and never asked whether the row was still last.

**It is not last on the path an operator actually takes.** `adminForceResolve` appends an audit
event, so force-fail-then-reconcile — the obvious order, since the workflow is stuck and the
external service takes time to check — puts a row above the pending call before the operator
resolves it. A signal delivered to a stopped workflow does the same. So does phase E's own path,
in that case.

**Severity is set by who runs the verifier.** `cmd/cleat-worker/setup.go` wires
`VerifyWorkflowEvents` in by default (`--disable-checksum-verification` turns it off), and
`engine/executor.go` returns a hard error on mismatch when `failOnChecksumMismatch` is set.
So the rescue path bricked the workflow it was rescuing: resolve the ambiguity, re-replay, and
the worker refuses to run the workflow at all, reporting what reads as tampering. §3.20's
re-replay guard sends the operator through `resolve` first, which is exactly the order that
triggers this.

**Fixed** by re-chaining, in the same transaction as the resolve: `chainRepairsAfter` returns
the rows above the resolved step, and each dialect's `repairChainAfterResolve` rewrites their
checksums onto the newly written one. The repair stops at the first pending row above, because
that row is itself a reset point and everything above it is already chained on `""`.

The common case pays nothing — a pending row is normally last, `chainRepairsAfter` returns
empty, and the resolve is the single `UPDATE` it always was.

**On the falsification.** Passing `nil` for the repairs reproduces the mismatch on both
dialects at step 1, with the message above. A second control breaks only `PostgresStore`'s
repair (chaining from `""` instead of the resolved checksum) and only the PostgreSQL arm fails —
so the test is measuring each dialect's own SQL rather than a shared Go path.

`TestAdminResolveStep_VerifiesWhenThePendingRowIsLast` is the control in the other direction:
a "fix" that merely moved the breakage would pass the first test and fail this one.

**The vacuity guard earned its place again.** The first test asserts that something *is* stored
above step 0 before resolving. Without it, a change that stopped force-fail from appending its
audit event would leave the test passing while testing nothing at all — which is the shape
§3.80's guard caught, and how §3.79 was found.

**What this says about the method.** Nothing failed. No test was red, no operator reported it,
and the two features involved each work on their own. It came out of reading
`ResolveCallIntent`'s doc comment and asking whether its stated premise still held for a caller
added after it was written. **A doc comment that explains why something is safe is a contract,
and a new caller is the event that expires it.** Grep for callers when you change a function;
grep for *premises* when you add one.

### 3.108 The Tier 1 Gate ran on Go's 10-minute default and the engine suite outgrew it — 🟢 **FIXED 2026-09-03** (WS-1, 2026-09-03)

`scripts/tier-gate.sh` invoked `go test` with no `-timeout`, so Go's 10-minute per-package default
applied while the job itself allowed 45. Measured on a runner, 2026-09-03:

```
tier-gate: ran=6546 pass=6543 fail=0 skip=2
panic: test timed out after 10m0s
FAIL  github.com/cleat-team/cleat/engine  600.038s
```

**Zero failing tests, and the job went red.** That is CLAUDE.md's "Is this result real?" section read
in the other direction — a red that is not a failure. It arrived on an unrelated PR whose change
was confined to `cmd/cleat-worker`, so the first honest question was whether the change could have
caused it, and it could not: the engine suite does not import that package.

#### Why this gate and not the ci.yml matrix

Both run the same packages. The gate sets **all three DSNs**; `ci.yml`'s `Test Go (engine)` matrix
entry configures PostgreSQL only, and CLAUDE.md's own table records what that is worth — 2544
tests in 16s against no DSN, 3846 in 60s against three. The gate is simply the invocation running
the suite that got long. `ci.yml`'s entry carries `flags: -p 1` and no timeout either, so it has
the same latent defect and now carries an explicit 30m as insurance rather than as a fix.

#### 30m, and why not 45m

The workflow's `timeout-minutes` is 45. Go's timeout has to fire **first**, or the runner kills the
job and prints no goroutine dump — the difference between "which test was hanging" and "the job
stopped". It also has to cover two sequential invocations, the root module and each tier-1 module.

`GO_TEST_TIMEOUT` is overridable so a slow machine can raise it without editing the script.

#### Verification, and what it could not cover

**The gate CAN run on this machine, and the first version of this entry said it could not.** It
fails its preconditions when run on the host — `componentize-py` cannot run natively on macOS at
all — but `scripts/docker/python-toolchain.Dockerfile` exists for exactly that reason and is
`FROM golang:1.26-bookworm`, so it carries Go and both tools. Run 2026-09-03:

```
docker --context desktop-linux run --rm -v "$PWD":/src -w /src -e CGO_ENABLED=1 \
  -e CLEAT_TEST_POSTGRES='postgres://postgres:postgres@host.docker.internal:5432/cleat?sslmode=disable' \
  -e CLEAT_TEST_DB=...  -e CLEAT_TEST_MYSQL=...  -e CLEAT_TEST_MSSQL=... \
  -e CLEAT_CRASH_DB='postgres://cleat:cleat@host.docker.internal:5433/cleat?sslmode=disable' \
  cleat-py-toolchain bash scripts/tier-gate.sh
```

`ran=7435 pass=7218 fail=203 skip=14`. It runs.

**One dialect does not survive the hop, and it is worth writing down.** All 203 failures are the
`mssql` arm, every one `ping MSSQL test DB: EOF`. SQL Server lives in a *colima* context on this
machine while the toolchain image runs under Docker Desktop, so the path is container → Docker
Desktop VM → host → colima VM, and the TDS pre-login handshake does not survive it. Neither
`host.docker.internal` nor the host's LAN address helps, and `encrypt=disable` does not either.
PostgreSQL and MySQL are reachable and pass. Running SQL Server under Docker Desktop too would
close the gap; that is an environment change rather than a repo one.

**A port probe said the port was OPEN and that meant nothing.** `(exec 3<>/dev/tcp/host/1433)`
succeeds against a listener that accepts and then closes — which is precisely what `EOF` is. The
probe and the failure are the same event read two ways.

**The local run did not verify the fix, and CI then did.** The container run's engine package
finished in `361.962s`, comfortably inside the old 10-minute default, because the `mssql` arm
failed fast instead of running — zero timeout panics there is consistent with the fix working and
equally consistent with it being unnecessary at that speed.

The confirmation came from the gate itself, on the two PRs either side of the change:

| | `Tier 1 Gate` |
|---|---|
| #650, without the fix | `panic: test timed out after 10m0s`, `FAIL … engine 600.038s`, `ran=6546 pass=6543 fail=0` |
| #654, with it | **pass, 24m10s** |

Same gate, same suite, and the job ran well past the wall it previously hit. That is the
verification; it is a comparison across two branches rather than a controlled A/B, and it is worth
saying so, but the only difference the gate sees between them is the timeout.

What proves the *plumbing* — that `GO_TEST_TIMEOUT` reaches the invocation at all — is still the
direct check below, with the same quoting the script uses:

```
$ GO_TEST_TIMEOUT=1ms  go test -count=1 -p 1 -timeout "$GO_TEST_TIMEOUT" ./auth/
panic: test timed out after 1ms
$ GO_TEST_TIMEOUT=30m  go test -count=1 -p 1 -timeout "$GO_TEST_TIMEOUT" ./auth/
ok    github.com/cleat-team/cleat/auth  0.398s
```

**The first attempt at that check was vacuous and is worth recording.** It ran against `./wasmrw/`,
which has no test files, so `1ms` produced `[no test files]` and no timeout — a check that would
have passed against a broken flag. The package has to actually run tests for the assertion to mean
anything.

#### Related

This is the third timeout of the same family in two days. #629 raised the cluster job's 300s after
the engine suite outgrew it; this session hit Go's 10-minute default repeatedly on local
three-dialect runs (380s, 442s, 473s, 503s, 987s, 1533s — the spread is machine load, not the
suite). The suite is growing and the defaults were set when it was smaller; expect the next one
rather than being surprised by it.

### 3.101 Terminate and signal told the caller which workflow ids are real — 🟢 **FIXED 2026-09-03** (WS-1, 2026-09-03)

Two routes answered something other than 404 for a workflow the caller does not own, and the two
answers differed from each other and from what an unknown id produced:

```
POST /api/dead-letters/{id}/terminate   200 {"status":"terminated"}, having done nothing
POST /api/workflows/{id}/signal         500 "Violation of PRIMARY KEY pk_workflow_signals"
```

**Neither was a leak.** §3.86 put `AND tenant_id` on the terminate's `UPDATE` and on
`DeliverSignal`'s `MERGE` `ON` clause, so nothing crossed. What crossed was **information about
which ids are real**: an unknown id produced a different response from a real one belonging to
somebody else, so both routes answered a question the caller was not entitled to ask.

`cmd/cleat-worker/api_admin.go` had already written this reasoning down for the admin API —
*"It answers 404, never 403: 403 would confirm that the workflow exists, which is itself
information the caller is not entitled to"* — and these two routes predate it. The fix is to route
them through the same `callerOwnsTarget` helper rather than to invent a second mechanism.

**This depended on §3.99.** `callerOwnsTarget` compares `wf.TenantID`, which `GetWorkflowByID` did
not populate on PostgreSQL or SQL Server, so until that was fixed the helper answered 404 to
everyone and could not be reused for anything. That is why §3.99 came first.

#### What the test asserts, and why status codes alone would not do

`cmd/cleat-worker/terminate_signal_not_found_test.go` asserts three things per route:

- the **owner** still gets 200 and the store *is* reached — without this, a handler that 404s
  unconditionally passes everything else;
- another tenant's workflow gets 404 **and the store is not reached** — a handler can answer 404
  after having already applied the operation, which is the weakness
  `TestAdminRoutesRejectCrossTenantTarget` records;
- an unknown id and a foreign id produce **byte-identical bodies**. This is the one the file exists
  for. A test that only checked for 404 would pass against an implementation answering 404 for a
  foreign id and a differently-worded 404 for an unknown one, which is still an oracle.

Both handlers were reverted to `scopedStore` one at a time; each turned exactly one case red, with
*"the store was reached for another tenant's workflow; the status code says 200 but the operation
was applied"*.

**The 500 is not what the test demonstrates**, and the file says so. It was measured against a live
SQL Server while §3.86 was being written; the mock here does not simulate the primary-key
violation, so reverting the fix shows a 200. The test asserts the handler property, which holds
whatever the store would have gone on to do.

#### The three siblings, done 2026-09-03 — and only one of them was a hole

This entry originally said the same shape applied to `handleWorkflowRetry`, `handleCancel` and
`handleGetQueryState`, *"one line each"*. **That was wrong, and checking each one is what showed
it.** Only `handleCancel` needed anything:

| route | before | verdict |
|---|---|---|
| `handleCancel` | `200 {"status":"cancellation_requested"}` for a workflow it had not cancelled, and for one that did not exist | **a hole** — now routed through `callerOwnsTarget` |
| `handleWorkflowRetry` | already reads the workflow back through a tenant-scoped `GetWorkflowByID` and `404`s when it is nil | safe |
| `handleGetQueryState` | `GetQueryState` returns `("", nil)` for no rows on all three dialects | safe |

The query-state one is worth stating carefully, because "it returns 200" sounds like the defect
this entry is about and is not. A foreign id, an unknown id, and a real workflow whose key is
simply unset all produce the same `200 {"key":k,"value":""}`. Indistinguishable is the property
that matters; a 404 there would *add* an oracle rather than remove one.

`handleWorkflowRetry` answers `404 "workflow not found"` where `callerOwnsTarget` answers
`404 "not found"`. Different wording, same status, and — the part that counts — the same answer
for an unknown id as for someone else's, so it is not an oracle either. Left as it is rather than
churned for symmetry.

**Estimating from a shape rather than reading the code is what produced the wrong count**, and it
was an estimate offered as a plan. Two of the three were already correct by different mechanisms,
which is the same "two mechanisms, same outcome" pattern §3.91 and §3.99 both record for the
dialects.

Also recorded here and now **done**: `terminateWorkflowOnce` not checking `RowsAffected` before the
parent-close cascade — see §3.92's root-fix section.

### 3.99 The admin API answered 404 to its rightful owner on two of three dialects — 🟢 **FIXED 2026-09-03** (WS-1, 2026-09-03)

`GetWorkflowByID` did not `SELECT tenant_id` on PostgreSQL or SQL Server, so
`WorkflowInstance.TenantID` came back as `""`. `cmd/cleat-worker/api_admin.go`'s
`callerOwnsTarget` — the **only** enforcement point for `/api/admin/instances/*`, because the
admin store methods take no tenant parameter — compares exactly that field against the
authenticated caller. `"" != "<caller uuid>"` is true for everyone, including the owner.

So with `--enable-admin-api` and `--require-auth` both on, every admin instance route
(force-complete, force-fail, re-replay) answered **404 to the tenant that owned the workflow**, on
PostgreSQL and SQL Server. Measured 2026-09-03, one run across all three dialects:

```
postgres: wf.TenantID = ""                                      EMPTY
mysql:    wf.TenantID = "00000000-0000-0000-0000-000000000000"  populated
mssql:    wf.TenantID = ""                                      EMPTY
```

#### Why nothing caught it, which is the reusable part

**There is a test, it is correct, and it passes.** `TestAdminRoutesRejectCrossTenantTarget`
(written for §1.7) exercises the ownership gate with a caller tenant on the request context. Its
mock returns:

```go
return &engine.WorkflowInstance{ID: id, TenantID: ownedBy}, nil
```

The mock populates the field. The test therefore proves the **comparison** is right — it is — and
says nothing about whether any real store fills in the value being compared. Two of three do not,
and nothing anywhere compared the mock against the implementations it stands in for.

That is `hostabi_runtime_parity_test.go`'s lesson in a new place: two implementations of one
contract, each self-consistent, with nothing checking they agree. It is also CLAUDE.md's *"watch
which layer is holding the test up"* in its most literal form — the layer holding that assertion
up was the test's own fixture.

**And it failed closed**, which is why no operator report surfaced it: a gate that denies
everything looks like a gate doing its job. The tell was not a failure anywhere. It was that
MySQL — the dialect documented single-tenant-only, where this matters least — was the only one
that scanned the column.

#### The fix

`SELECT tenant_id` on both dialects, and scan it. On SQL Server that is
`LOWER(CONVERT(NVARCHAR(36), tenant_id))`:

- `CONVERT` because go-mssqldb scans `UNIQUEIDENTIFIER` into a Go string as 16 raw storage bytes
  (§3.55's defect, and `TestMSSQLUUIDColumnsAreConvertedInProjections` fails the build without it);
- `LOWER` because `CONVERT` returns **uppercase** and `uuid.UUID.String()` is lowercase, so the
  comparison would have gone on failing for a second, entirely different reason. This is the
  hazard §3.91 recorded after a probe passed while printing the leak it was probing for.

`callerOwnsTarget` now uses `strings.EqualFold` as well. Normalising at the source is the real
fix; the fold is there so a dialect that stops normalising fails visibly rather than by 404ing
every request forever.

`engine/workflow_tenant_id_populated_test.go` asserts the field at the **store** layer across all
three dialects, deliberately not at the HTTP layer: the HTTP layer is where a mock can supply the
value, and that is precisely what hid this. Each dialect was falsified on its own and turned
exactly one subtest red.

#### Three sqlmock fixtures had to move, and a fourth must not

`engine/db_methods_test.go` and `engine/mssql_store_test.go` drive `GetWorkflowByID` through
sqlmock with a **hardcoded column list**, so adding `tenant_id` broke their arity with the same
`expected 16 destination arguments in Scan, not 17` the falsification produced. That is the mock
being useful: it made the shape change deliberate rather than silent.

**But I then patched a fourth fixture that already had the column**, by matching on a
`// trace_id` comment rather than checking which test owned the block.
`engine/mssql_store_mock_test.go`'s row belongs to `ClaimWorkflow` — `match: "SET status =
'running'"` — and already carried `tenant_id` at position 8, so the extra value broke
`TestMSSQLStore_ClaimWorkflow_Success` with the mirror-image error, `expected 15 destination
arguments in Scan, not 14`. Read which test owns a fixture before editing it; a matching comment
is not ownership.

**One falsification had to be redone**, which is worth recording. Removing `tenant_id` from
PostgreSQL's `SELECT` without also removing its scan target made the test fail with
`sql: expected 16 destination arguments in Scan, not 17` — a driver arity error, not the
assertion. It was red, and red for the wrong reason. Replacing the column with `'' AS tenant_id`
keeps the arity and fails on the assertion, which is the thing being proven.

### 3.95 `cleatctl restore-workflow` is removed — 🟢 **DECIDED AND DONE 2026-09-03; the three questions below were answered by deleting the thing that raised them** (WS-1, 2026-09-03)

Found while investigating something else — whether any writer puts a NULL in MySQL's nullable
`tenant_id` columns. None does; the answer to that question is in the last section here. Looking
for one turned this up instead.

#### 1. The producer does not exist

`cmd/cleatctl/restore.go` names `cleatctl backup-workflow` twice as the tool that produces its
input — once in the doc comment, once in the user-facing help text `printRestoreUsage` prints.
**There is no such command.** `cleatctl`'s dispatch (`cmd/cleatctl/main.go`) has nine commands and
none of them is a backup. Re-derive:

```
grep -rn 'case "backup' --include="*.go" .        # no output
grep -rn "backup-workflow" --include="*.go" .     # only restore.go's own prose
```

**This is the same defect `tiers.yaml` already records against `scheduledbackup`** (line ~522):
*"claimed … 'manual backup/restore via HTTP API and CLI commands'. Neither was true: there is no
restore implementation anywhere in the package … An operator configuring this to satisfy an
off-host backup requirement found out only at restore time."* Here it is the mirror image — the
restore ships and the backup is the missing half — which suggests the pattern to check for is
"one end of a round trip documented in terms of the other", not "backup specifically".

`restore-workflow` itself is real: it is dispatched at `main.go:82`, listed in the help at
`main.go:114`, and has tests (`cmd/cleatctl/restore_test.go`). The NDJSON format is documented, and
the help says "or compatible tools", so an operator can hand-produce the file. That is the only way
to use it.

#### 2. Every restored row lands under the default tenant

None of the four inserters names `tenant_id`: `insertWorkflowInstance`, `insertEventHistory`,
`insertWorkflowSignal`, `insertWorkflowPromise`. On PostgreSQL — the only dialect this command
supports, per its own comment at `restore.go:191` and its `$1`/`ON CONFLICT` syntax —
`event_history.tenant_id` is `NOT NULL DEFAULT '00000000-0000-0000-0000-000000000000'`. Measured
2026-09-03:

```sql
CREATE TEMP TABLE probe (LIKE event_history INCLUDING DEFAULTS);
INSERT INTO probe (workflow_id, step, event_type) VALUES ('probe-wf', 0, 'x');
-- event_history omitting tenant_id -> 00000000-0000-0000-0000-000000000000
```

So restoring a **non-default** tenant's workflow silently writes it under the default tenant. The
command reports success and the owning tenant still cannot see the workflow.

**This is NOT a cross-tenant security defect, and the distinction matters.** `restore-workflow` is
DBA-only by design — `droptenant.go:31` groups it with `check-db` and `versions purge` as
operations that authenticate with a PostgreSQL DSN rather than a tenant API key. Whoever can run
it can already read every tenant's rows directly. Nothing is disclosed that the caller did not
already have. It is a data-integrity bug: a restore that reports success and did not restore
anything the owner can see.

#### 3. `def_version` is omitted too, and since D7 that is now load-bearing

`insertWorkflowInstance` writes `(id, def_name, status, input, result, error, created_at)` — no
`def_version`. Since D7 (§3.77, migration `postgres/035`) `workflow_instances`' foreign key is
`(tenant_id, def_name, def_version)`, so a restored row either binds to the **default tenant's**
definition of that name — a different program after D7 — or trips the FK. This was harmless while
`workflow_defs` was a global registry keyed by `(name, version)`; D7 is what made it matter, the
same way it made §3.86's `LoadWASM` reachable.

#### Decided 2026-09-03: removed

The owner's call, in their words: *"restore-workflow seems a half-baked idea. Let's remove it."*
`cmd/cleatctl/restore.go` and its tests are deleted, along with the dispatch case, the help line,
and the mention in `droptenant.go`'s list of DBA-only operations.

**What decided it was not the defect list above** — it was that the feature never cohered:

- It arrived as one line in a bulk port of forty-odd items (`5181ae2b`, *"CLI: cleat cost
  calculator, NDJSON restore tool"*). No design note, no `tiers.yaml` entry, nothing in the docs
  beyond its own help text.
- The producer it named never existed.
- All twelve of its tests ran against `mockRestoreDB` — *"a minimal SQL driver mock that always
  succeeds on Exec"* — asserting on **stdout** (`"2 event_history rows"`, `"Restore complete"`).
  No test ever inserted a real row, which is exactly why the missing `tenant_id` and `def_version`
  survived: the tests pinned the NDJSON parsing and the progress reporting, and nothing checked
  the SQL was right.

So it parsed a format nothing wrote, into a schema it did not fully populate, verified by tests
that did not use a database.

**And the comparison that settled it.** DBOS has no per-workflow export/import pair. Its CLI
operates on workflows already in the database — `cancel`, `resume` (from the last completed step),
`fork` (new execution from step N, copying prior results), `restart` (fork from step 0) — and for
actual data loss it delegates to PostgreSQL: a new instance from a backup, rolled forward through
archived logs via point-in-time recovery. That is an architectural position, not a gap. Durable
state lives in Postgres, so disaster recovery is a Postgres problem, and PITR restores the whole
database consistently. A single-workflow file export cannot: it replays one workflow's rows
outside the transaction that wrote them, and `restore-workflow` explicitly did not restore
`workflow_defs`, schedules or tenants — the rows its own rows point at.

cleat already has the useful half of that space in `cleatctl replay` and `cleatctl debug`.

**What is NOT removed, deliberately.** Nothing else changes. The `--db`/`CLEAT_DB_URL` DBA path
stays, and `check-db`, `drop-tenant` and `versions purge/gc` still use it. If per-workflow rescue
is wanted later, the shape to copy is DBOS's — operate on what is in the database — rather than
a file format with no writer.

### 3.92 §3.86 scoped the terminate and left the cascade — 🟢 **FIXED 2026-09-03, symptom and root; found by the gate's allowlist demanding a reason, not by its scan** (WS-1, 2026-09-03)

#### First and second: the close-policy cascade

`terminateWorkflowOnce` calls `enforceParentClosePolicy(ctx, workflowID)` **unconditionally after
its commit**, and does not look at how many rows the terminate touched. §3.86 added
`AND tenant_id` to the terminate itself, so a cross-tenant terminate now matches no parent — and
then cascades into that parent's **children** anyway, because both of the close-policy statements
key on `parent_workflow_id = @p1` with no tenant, as does `childrenClosedByTerminate`.

So after §3.86 the parent is protected and its children are not:

```sql
UPDATE workflow_instances SET status = 'failed', error_msg = 'parent workflow terminated'
    WHERE parent_workflow_id = @p1 AND parent_close_policy = 'TERMINATE' AND ...
UPDATE workflow_instances SET cancellation_requested = 1
    WHERE parent_workflow_id = @p1 AND parent_close_policy = 'REQUEST_CANCEL' AND ...
```

**How it was found is the point.** It was about to be written into
`engine/mssql_tenant_predicate_test.go`'s allowlist as `scopedByCaller` — "the id came from a row
already read under a predicate" — and that reason is false: the id is the one the HTTP caller
supplied, and nothing between the two checks whether it belonged to anyone. The scan did not find
this; **writing down why the exemption was safe** did. Both entries are marked `openFinding` and
point here, so the gate stays green without granting anything.

Fixed by a tenant predicate on all three statements. Children inherit the parent's tenant
(`StartChildWorkflow` and `StartChildWorkflowAtomic` both insert `tenant_id = s.tenantID`), and
`enforceParentClosePolicy` runs on the store that owns the parent, so scoping them changes nothing
on the working path. `engine/mssql_admin_login_cascade_tenant_test.go` is the regression test;
each predicate was removed on its own and turned exactly one case red, naming the harm.

**Every case carries a positive control, and here that is not a formality.** The cascade exists
because of §3.79 — a terminated parent used to leave its `TERMINATE` children running. A
predicate that disabled the cascade rather than scoping it would reintroduce exactly that,
silently, and the cross-tenant half of the test would still pass. So each case also asserts the
caller's *own* child is still closed.

**Why #616 could not have caught this.** That PR asserted on the parent, and the parent was
correct. The child is a different row, reached by a different statement, after the transaction has
already committed.

#### Measured 2026-09-03, on a `cleat_admin` pool

Tenant A holds parent `casc-parent-a` with one `TERMINATE` child. Tenant B, which owns neither,
calls the ordinary `TerminateWorkflow` on A's parent:

```
childrenClosedByTerminate(B on A's parent) -> [006f8661-1023-485d-913b-50a9cb1ca153]
AFTER: tenant A's child status="failed" error_msg="parent workflow terminated"
```

**The resulting state is incoherent, which makes it worse than a plain unauthorised write.**
Tenant A's parent is untouched and still running — §3.86 did that correctly — while its child is
failed with `parent workflow terminated`. The error message names a cause that did not happen,
and A's own audit trail contains nothing that explains it.

#### The same statements on the other two dialects — checked, and only one needs anything

| | the cascade is scoped by |
|---|---|
| PostgreSQL | nothing in the SQL, but `enforceParentClosePolicy` and `childrenClosedByTerminate` both run under `beginTxWithRLS`, and the application role does not hold `BYPASSRLS` |
| MySQL | **nothing, and nothing underneath it** — `engine/mysql_lifecycle.go:907` and `:946` key on `parent_workflow_id` alone, and MySQL has no row-level security |
| SQL Server | fixed here |

**The MySQL half is recorded rather than acted on**, following the precedent §3.86 set for exactly
this situation with `SetWorkflowTag`: D1 documents MySQL as single-tenant-only for want of
row-level security, so a multi-tenant MySQL deployment is not a supported configuration and the
statement cannot be reached across tenants in one that is. Doing it here would put two dialects
and two different rationales in one change — and the MySQL topology has a wrinkle worth its own
look, because `MySQLStore` also supports a database-per-tenant arrangement where `tenant_id` is
not the thing keeping tenants apart.

Note that the gate cannot ever catch the MySQL one: `engine/mssql_tenant_predicate_test.go` is
SQL Server-specific by construction, because the defect it exists for is the `cleat_admin`
exemption. Nothing plays that role for MySQL, and nothing needs to while D1 holds.

#### A third statement was flagged and is NOT a defect, which is the point of demanding reasons

The guard also flagged `engine/store_admin.go:482` —
`SELECT event_type, operation FROM event_history WHERE workflow_id = @p1 AND step = @p2`, no
tenant — and the first draft of this entry recorded it as a third leak. **It is not one.** Every
caller of `adminAppendAudit` (`adminForceResolve` and the three re-replay paths) reaches it only
after a tenant-scoped `UPDATE` reported `RowsAffected > 0`, so a foreign workflow id has already
been refused with `adminNotFound`. Its allowlist reason is `scopedByCaller` and that reason is
true.

Worth keeping because it is the counterexample to this entry's own method: **writing down why an
exemption is safe finds defects, and it also clears statements that only look like defects.** The
same discipline produced both results within an hour.

Note also what `adminForceResolve` does that `terminateWorkflowOnce` does not: it checks
`RowsAffected` and bails before reaching the cascade. That is the deeper fix, and it is **now
done** — see below.

#### The root fix, 2026-09-03: don't cascade for a workflow you didn't terminate

All three dialects now check `RowsAffected` and return `ErrWorkflowNotFound` instead of `nil`,
before committing and before the cascade. `adminForceResolve` has always worked this way; terminate
is now in line with it.

`ErrWorkflowNotFound` is reused rather than a new error invented, and its existing doc comment is
the reason: it *"deliberately does not distinguish 'no such workflow' from 'another tenant's
workflow'. Splitting them would make the endpoint an existence oracle."* That is the same boundary
§3.101 drew at the HTTP layer, so the store and the handler now agree by construction.
`handleDeadLetterTerminate` maps the sentinel to the same 404 `callerOwnsTarget` already returns.

Terminate stays **idempotent**: the `UPDATE` carries no status filter, so re-terminating an already
terminated workflow still matches its row and returns nil. Only a genuinely absent — or
other-tenant — row reports not-found.

**What this is and is not.** §3.101 routed the HTTP terminate through `callerOwnsTarget`, and
§3.92's predicates already scope the cascade statements, so the cross-tenant harm is closed twice
over. This change's observable contribution is the **error contract**: the call no longer tells a
caller it terminated something when it terminated nothing. The not-cascading is defence behind
those two, for library callers and for the day a predicate is removed.

**A test assertion was written and then deleted for being unfalsifiable**, which is worth
recording because it is the same trap §3.86's `DeliverSignal` wake fell into. The first draft
asserted that an unmatched terminate leaves a real child alone. It cannot fail: within one tenant
an unmatched terminate is an id no row carries, so the cascade's `UPDATE`s key on
`parent_workflow_id` and match nothing, and `releaseWorkflowResources`' two calls match nothing
either — there is no observable difference. Removing the guard on any dialect turned the test red
on the *error*, never on the child. The assertion was removed rather than kept as decoration; the
cross-tenant case it was reaching for is covered by
`engine/mssql_admin_login_cascade_tenant_test.go`.

What remains and does falsify: the error contract (each dialect turned red on its own), the
positive control that a real terminate still closes its `TERMINATE` children (removing the cascade
turns it red with *"the close policy is no longer being enforced at all"* — this is §3.79's
subject and the thing a careless fix would break), and idempotency.

**Three existing tests encoded the old contract, and two of them were written by this session.**
`TestGap_TerminateWorkflow` ran against `newNoopDB`, which reports zero rows affected for every
statement, and asserted `err == nil` — so "a terminate that matched nothing succeeded" was pinned
as correct behaviour. The other two are §3.86's and §3.92's own cross-tenant tests: both did

```go
if err := storeB.TerminateWorkflow(ctx, tenantAsWorkflow, "not yours"); err != nil {
    t.Fatalf("cross-tenant TerminateWorkflow: %v", err)
}
```

— asserting that a terminate aimed at another tenant's workflow **succeeds**, because at the time
it did: §3.86's predicate made the `UPDATE` match nothing and the call returned nil anyway. All
three now assert `ErrWorkflowNotFound`, which is strictly stronger, and their existing assertions
about what was *not* touched are unchanged and still carry the tenancy proof.

That two of them were mine is the point worth keeping: a test written against the behaviour of the
day pins that behaviour, including the part of it that was wrong. The first skip-budget measurement of this change was taken off that failing
run and discarded: `check-skip-budget.sh` reports `failed=1` for exactly this reason, and a skip
count off a failing run is not a measurement.

#### How the third was found at all

The first version of the gate globbed `engine/mssql*.go`. Four files outside it carry `@pN`
parameters — `store_admin.go`, `store_intent.go`, `store_admin_rereplay.go`,
`plugin/migration.go` — and the guard reported a clean tree for all four without opening them.
`TestMSSQLUUIDColumnsAreConvertedInProjections` had already learned this and stopped globbing; the
gate now uses its walk. **A guard defined by where it looks rather than by what it looks for is a
confident green over the files it does not open** — the same failure as CLAUDE.md's
`^\S+\s+pending` parse, in a different costume. Widening added exactly one flag and no noise.

### 3.91 The ordinary claim path took every tenant's work on SQL Server — 🟢 **FIXED 2026-09-03; the `-claim-across-tenants` flag was decorative on this dialect** (WS-1, 2026-09-03)

`claimWorkflowsOnce` and `claimStickyWorkflowsOnce` — the **ordinary** claim, not the one whose
name says `AcrossTenants` — carried no tenant predicate. On a `dbo.cleat_admin` login, which
§3.86 establishes every multi-tenant SQL Server deployment must use, they returned every tenant's
ready work.

Measured before the fix, tenant B's ordinary `ClaimWorkflows`:

```
returned 2 instances:
    id=probe-tenant-a-wf tenant=AAAAAAAA-AAAA-4AAA-AAAA-AAAAAAAAAAAA
    id=probe-tenant-b-wf tenant=BBBBBBBB-BBBB-4BBB-BBBB-BBBBBBBBBBBB
```

**The grant is always present exactly where this matters, which is what makes it more than
theoretical.** `requireCleatAdminMembership` checks `s.db` — the *same pool* `claimWorkflowsOnce`
runs on. So on any deployment where `ClaimWorkflowsAcrossTenants` works at all, the ordinary claim
was already unscoped, and `-claim-across-tenants` was gating a widening that had already happened
unconditionally. The flag changed which SQL ran; it did not change what the worker could see.

#### The three dialects disagreed, and that is what settled it

| | ordinary claim is scoped by |
|---|---|
| MySQL | `AND tenant_id = ?` in the candidate `SELECT`, explicitly |
| PostgreSQL | nothing in the SQL — it claims inside `beginTxWithRLS`, and the application role does **not** hold `BYPASSRLS`, so RLS really does it. `cross_tenant_claim_test.go` tests precisely that, using a deliberately non-owning role |
| SQL Server | **nothing, and nothing underneath it either** |

So this was not a judgement call about whether a predicate was warranted. SQL Server was the only
dialect with no enforcement at any layer, and the fix is to match MySQL.

#### Why four passes missed it, and what that says about the gate

`scripts/mssql-tenant-predicate-audit.py` asked whether `tenant_id` appeared anywhere in the
statement, and for this one it did — twice. The claim projects
`CONVERT(NVARCHAR(36), INSERTED.tenant_id)` in its `OUTPUT` clause, and carries a long comment
about that conversion. Neither scopes anything. **The script's count did not move when this was
fixed**: 66/29 before, 66/29 after.

That script has since been **deleted** in favour of `engine/mssql_tenant_predicate_test.go`,
which asks where the column appears and flags this statement. Re-derive with

```
go test ./engine/ -run TestMSSQLTenantScopedTablesAreQueriedWithATenantPredicate
```

That is the argument for §3.86's gate being **position-aware** rather than a substring test,
stated better than the DeliverSignal MERGE stated it. A prototype asking whether `tenant_id`
appears in a `WHERE`, `ON` or `HAVING` window flags 31 statements where the substring test flags
29, and the two it adds are these.

#### A case hazard, found by a probe that passed while printing the leak

`CONVERT(NVARCHAR(36), tenant_id)` returns **uppercase**. The first probe compared
`wf.TenantID == unscopedTenantA` against a lowercase constant, so it reported PASS while its own
log showed tenant A's workflow in tenant B's claim. Every assertion in the regression test is
`strings.EqualFold` now.

**The same hazard is live in production code and is not fixed here.**
`cmd/cleat-worker/setup.go:storeForTenant` compares tenant strings with `==`. If a worker's
configured tenant is lowercase and the claim hands back uppercase, the worker treats its own
tenant as foreign and opens a second pool for it — wasteful rather than wrong, since
`uuid.Parse` accepts either case, but it means the `tenantID == w.storeTenantID` fast path never
fires on SQL Server. Recorded rather than fixed: it is a different file and a different failure.

#### Regression test

`engine/mssql_admin_login_claim_tenant_test.go`. Both predicates were removed one at a time and
each turned exactly one case red, naming the claimed rows —
`tenant B's ORDINARY claim took tenant A's work: claimed [claim-a-plain claim-b-plain]`.

**The third case is the one that had to be there**: `ClaimWorkflowsAcrossTenants` must *still* see
both tenants. Scoping the ordinary path and accidentally scoping the deliberate one would stop a
multi-tenant deployment's dispatch loop seeing the tenants it exists to serve, and every
non-default tenant's workflows would stop running — a worse outcome than the defect. It stayed
green under both falsifications.

### 3.90 `--wasm-instance-timeout` is charged for time the guest spends blocked in the host — ✅ **FIXED** (WS-3, 2026-09-03)

The per-invocation wall-clock fence advances while the guest is parked inside a host call. A
workflow that makes slow service calls and computes almost nothing spends its runaway budget
waiting, and dies with `execution time limit exceeded` — a message that points at the wrong half
of the system.

#### Mechanism

`startEpochTicker` (`engine/backend_wasmtime.go:168`) increments the wasmtime engine epoch every
50ms on a free-running goroutine. `configureStore` sets a per-invocation deadline of
`timeout / 50ms` ticks (`:283`). Nothing brackets a host function:

```
grep -rn "store.SetEpochDeadline(" --include="*.go" . | grep -v _test   # 3: :283, :387, :454
```

Anchor on the call, not the name: `grep -rn "SetEpochDeadline"` returns **5**, and the two extra
lines are comments *describing* the mechanism — a grep its own documentation satisfies.

The other two call sites are worth knowing, because they are not setup and they are the reason
fix (1) below is cheap. `:387` and `:454` grant a **fresh** deadline for the defer pass on a
workflow the fence already killed, sized from `DefaultWasmtimeDeferBudget`. So re-granting a
deadline part-way through a run is an existing, exercised move, not a new capability — it is
simply never done around a host call.

So the deadline is real time since the invocation began, not guest execution. The sibling fence
does not behave this way — `--wasm-instruction-limit` is wasmtime fuel, which only decrements on
guest instructions. **The two fences answer different questions and only one of them says so.**

#### Measured

`engine/instance_timeout_hostwait_test.go`, wasmtime, darwin/arm64, 2026-09-03. The fixture is
`deferfunc`'s `defer_order`, which makes exactly one call; the caller blocks for `delay` on it.

| delay | budget | outcome |
|---|---|---|
| 0 | 1s | trap |
| 0 | 2s | completes |
| 4s | 5s | trap |
| 6s | 8s | completes |
| 9s | 8s | trap |

Host wait is charged roughly 1:1 on top of the fixture's own sub-2s cost.

**The first row is why the shipped test controls on the budget rather than on the delay.** The
obvious control — "no delay, same small budget, must still complete" — fails: a 1s budget is too
small for this fixture whatever the caller does, so the trap happens either way and the
experiment measures nothing. The shipped pair holds the delay fixed at 5s and varies only the
budget (3s → trap, 30s → completes), which is the only form in which the trap is attributable.

#### Why it matters

- **Default 30s.** Three 12s service calls in one segment is an ordinary workflow, not a runaway
  one, and it trips the fence.
- **The error misdirects.** `execution time limit exceeded (30s wall-clock budget; configure with
  --wasm-instance-timeout)` reads as a guest that would not stop. Nothing in it says the time was
  spent waiting on a service.
- **It bounds the in-host retry design.** §3.88 item 2's answer is that a retry completing within
  a few minutes should keep the worker rather than suspend, which is what the host-side loop
  behind `cleat_call_retry` already does. That loop backs off with `time.After` **inside a host
  call**, so it spends the guest's budget: an in-host retry longer than the remaining budget kills
  the workflow it was retrying for.

#### Fixed: the epoch fence now measures guest execution

`engine/wasmtime_hostbudget.go`. Every wasmtime host function is registered through
`b.hostFunc`, which brackets the guest's budget: entering a host function charges the guest for
what it just ran, leaving one re-arms the epoch deadline with what is left. Host wait costs the
guest nothing.

Uniform rather than selective, and deliberately so — picking out "the ones that block" is a
judgment call repeated 67 times, and the set is not obvious (a service call blocks, but so does a
DB write behind an event record, a plugin call, and a WASI stub that touches a file). The
invariant is "the guest is not running while the host is", true at every one of those boundaries.
`scripts/check-hostfunc-budget.sh` enforces it, because the failure mode of a missed site is
silent: the host function still works, it just quietly charges its wait.

Two things fell out of the same change:

- **Module compilation stopped being charged.** `configureStore` sets the deadline when the store
  is created, which on a cold module is before compilation. `arm()` immediately before
  `linker.Instantiate` resets it at the moment the guest actually starts.
- **The defer budget stopped being spent on the cleanup call.** A defer body's whole purpose is a
  cleanup call, so charging the wait for it against a 1s budget spent the budget on the very
  thing it exists to allow.

Tested in `engine/instance_timeout_hostwait_test.go`: an 8s host call completes under a 3s epoch
budget, and — the control that keeps it honest — a guest spinning with no host call still traps
under the same budget. Verified by mutation: with the bracket removed the first half fails with
`execution time limit exceeded`, the right reason.

#### The second enforcement, found by the first fix failing

`--wasm-instance-timeout` was applied **twice**:

| | mechanism | when it fires |
|---|---|---|
| epoch deadline | `configureStore` → `SetEpochDeadline` | interrupts the guest mid-execution |
| context deadline | `executor.go:163` and `:419` | noticed at `:251`, only once the call returns |

Both bounded **wall clock**, so fixing only the epoch half changed nothing an operator could
observe: the run that died with `execution time limit exceeded` died with `execution timed out`
instead, at the same wall-clock point. **The measurement above was of the epoch fence and was
correct; it was not the whole enforcement.** An end-to-end test through
`engine.WithWASMInstanceTimeout` cannot tell the two apart, which is why the epoch-only test sets
its budget via the backend option `WithWasmtimeExecutionTimeout` and gives the engine no instance
timeout at all.

#### Fixed: the two questions now have two numbers

A context deadline is immutable, so it cannot be credited the way an epoch deadline can. The fix
is to stop asking it the wrong question:

- **`--wasm-instance-timeout`** (default 30s) bounds **guest execution**. Epoch fence.
- **`--wasm-wall-clock-ceiling`** (default **5m**, new) bounds **wall clock including host wait**.
  Context deadline. This is what stops an invocation blocked on an unresponsive service from
  holding a worker slot indefinitely — the protection the old context deadline was really
  providing, now named for what it does.

`WithWasmWallClockCeiling(0)` falls back to `wasmInstanceTimeout`, so an embedder that sets only
the instance timeout keeps the wall-clock bound it already had rather than silently losing it.
There is deliberately no way to remove the ceiling entirely.

A ceiling **below** the instance timeout is a configuration that cannot mean what it says —
`configureStore` clamps the epoch deadline to whatever the context has left, so the guest's
execution bound silently becomes the ceiling. The worker warns at startup rather than rejecting
it, since it is a degradation rather than an error.

Measured, `engine/instance_timeout_hostwait_test.go`, three runs sharing one 3s instance timeout
so only the ceiling and what consumes the time vary:

| case | host wait | ceiling | outcome | bound that decided |
|---|---|---|---|---|
| ordinary slow dependency | 8s | 30s | completes | neither |
| unresponsive dependency | 8s | 4s | `execution timed out` | ceiling |
| runaway guest | none, spins | 60s | `execution time limit exceeded` | instance timeout |

The third row is the one that keeps the other two honest: a generous ceiling must not raise the
runaway bound. Case 1 fails without the fix — verified by mutation, reverting the executor to
`e.wasmInstanceTimeout` reproduces `execution timed out` exactly.

#### What this unblocks

§3.88's decision — a retry loop completing within a few minutes keeps its worker rather than
suspending — is now deliverable. The in-host loop's backoff is spent inside a host call, so it no
longer consumes the guest's execution budget, and the wall-clock ceiling that does cover it
defaults to 5m rather than 30s. The remaining work there is the missing
`HostCallsImpl.DurableCallWithRetry` method.

### 3.94 Execution limits are process-wide, and one of them is compiled into the guest — 🟡 **IN PROGRESS: steps 1–5 done, 6 open** (WS-3, 2026-09-03)

**Requirement, 2026-09-03:** each tenant must be able to override the default time thresholds
without affecting other tenants, so that several microservices — or several organisations —
sharing one cleat deployment can manage their own settings.

The design and the plan are below. Steps 1–5 have shipped — the settings table on three
dialects, and per-tenant resolution of all three limits: the wall-clock ceiling (5a), the
host-side retry threshold (4), and the epoch fence (5b). Step 6 is open. The
`What exists today` table below describes the state BEFORE this work and is kept as the
problem statement; read the step markers for status.

#### What exists today

| knob | scope | mechanism |
|---|---|---|
| `--wasm-instance-timeout` | whole worker process | flag (§3.90) |
| `--wasm-wall-clock-ceiling` | whole worker process | flag (§3.90) |
| `--max-workflow-duration` | whole worker process | flag |
| `--max-quota-events` | whole worker process | flag |
| `--concurrency` | whole worker process | flag |
| `hostRetryBudget` / `HOST_RETRY_BUDGET_MS` | **compiled into the guest** | SDK constant (§3.88) |

`admin.tenants` has five columns — `tenant_id`, `name`, `display_name`, `created_at`,
`suspended`. There is no tenant-scoped configuration table and no read path for one anywhere in
the tree. `suspended` is the only per-tenant behavioural switch that exists. Re-derive with

```
awk '/CREATE TABLE IF NOT EXISTS admin.tenants/,/^\);/' migrations/postgres/001_schema.sql
grep -rn "tenant_id" cmd/cleat-worker/config.go     # 0 -- no flag is tenant-scoped
```

#### The part that is a layering problem, not a missing table

Five of the six are flags, and a flag can be replaced by a per-tenant lookup. **The retry
threshold cannot**, because it is not a deployment setting at all: it is a compile-time constant
baked into each workflow's WASM module, in both SDKs. So an operator cannot change it, per-tenant
or globally, and two tenants differ only by compiling their workflows differently — which is the
workflow *author* deciding, not the platform administering. Adding a settings table would not
help, because the guest has already chosen a path before the host is consulted.

**The fix is to move the decision to where the information already is.** The host receives the
entire policy in `cleat_call_retry` — max attempts, initial interval, backoff coefficient, max
interval. It also knows the tenant. So the host can apply the tenant's threshold and, when a
policy is too long, **refuse the call with a sentinel** that tells the guest to fall back to its
own durable-sleep loop.

Three things follow, and the third is the reason this is worth the ABI change:

1. The threshold becomes tenant-administrable in the natural place.
2. The duplicated constant across two languages disappears, and so does
   `cleat.TestBothSDKsAgreeOnTheHostRetryBudget` — a test that exists only because nothing at
   compile time can make two languages agree on a number.
3. **Every future SDK gets the behaviour for free.** As a guest-side constant, Python, Java and
   AssemblyScript would each need their own copy and their own agreement test. As a host refusal,
   they need only honour a sentinel they must already honour for §3.84.

An ABI change was authorised for this on 2026-09-03.

#### Design

**A. The sentinel.** `cleat_call_retry` gains a refusal meaning "this policy is too long to run
here; retry it yourself". §3.84 built exactly this machinery — the stop sentinel at **bit 31**,
free in all six result layouts that can start fresh work, with a mandatory check-before-decode
ordering recorded in `ABI.md`. This is a second, distinct signal on one operation, so it needs
its own bit or its own encoding in the existing error space; `packDurableCallResult` already
carries an error code, which is the cheaper option and should be preferred over spending another
bit. **Decide that before writing code, and record the layout in `ABI.md` either way.**

Note what the guest must then do: fall back to the SDK-level loop *without* having consumed an
attempt. The host refusing must record no event, so replay sees nothing — the same
"refuse rather than half-do it" shape as §3.84's event-cap refusal (`eventCapCallError`).

**B. The settings store.** A tenant-scoped settings table, read at dispatch and carried on the
execution alongside `tenantID`, which the engine already has. Values, all optional with the flag
as the default:

- `wasm_instance_timeout_ms` — guest execution
- `wasm_wall_clock_ceiling_ms` — wall clock including host wait
- `host_retry_budget_ms` — the threshold above
- (`max_quota_events`, `max_workflow_duration_ms` are the obvious next two, and the design should
  not preclude them)

**C. Resolution and bounds.** A tenant must not be able to raise a limit past what the operator
allows, or a shared deployment has no protection at all. So each flag becomes a **ceiling**, and
the tenant's value is clamped to it rather than replacing it. `--wasm-wall-clock-ceiling`'s
relationship to `--wasm-instance-timeout` (§3.90: a ceiling below the instance timeout silently
becomes the guest's execution bound) has to hold per tenant too, and the same startup warning
becomes a per-tenant validation.

#### The constraint that shapes this: D1

**Per-tenant settings are only meaningful where tenant isolation is enforced.** `tiers.yaml` D1
grants multi-tenancy on **PostgreSQL and SQL Server only**, because MySQL has no row-level
security feature at all. A settings table on MySQL is readable and writable by every tenant, so
this feature is either postgres+mssql, or it needs an explicit story for how MySQL degrades —
most likely "MySQL is single-tenant, so the flags *are* the tenant's settings", which is
consistent and costs nothing.

**This is a tiers.yaml decision, not an implementation detail.** Settle it there before building.

#### Plan, in order

1. ~~**Decide the sentinel encoding**~~ — ✅ **decided 2026-09-03, recorded in `ABI.md`**
   ("Retry refusal — `cleat_call_retry` only, and NOT a sentinel bit"). A new `callErrorCode`
   classification, `6` = `RetryPolicyTooLong`, with `errCode` non-zero. **Not** a sentinel bit,
   and the reason is what an SDK that has not implemented it does — already-deployed guest
   modules cannot be upgraded in step with the host:

   | encoding | an SDK that does not know it |
   |---|---|
   | new bit, `errCode = 0` | reads a **successful** empty response and runs on with a bogus result — silently wrong |
   | new bit, `errCode != 0` | a generic call error — but then the bit adds nothing the classification does not |
   | **new `callErrorCode`** | a generic call error: the workflow fails loudly rather than proceeding on garbage |

   So a bit is either unsafe or redundant. The classification is also the documented extension
   path (`guestCallErrorCodes`: a member may be added, an existing value may never change) and
   costs no bits in a word where bit 31 is already spent by §3.84.

   Two constraints fell out of writing it down. `RetryPolicyTooLong` must be **non-retryable**,
   or an SDK treating it as retryable re-issues `cleat_call_retry`, is refused on the same
   grounds, and loops. And the host must record **no event** for a refusal, so replay sees
   nothing and the guest has not consumed an attempt — the same shape as the event-cap refusal.

   Worth noting why §3.84's answer does not transfer: the stop sentinel had to be free in **six**
   layouts because any host call can start fresh work. A retry refusal can only come from
   `cleat_call_retry`, so the constraint that forced a bit there does not apply here.
2. ~~**Settle the MySQL story in `tiers.yaml`**~~ — ✅ **decided 2026-09-03: option C.** The
   settings table ships on **all three dialects with one uniform API**, and on MySQL it holds
   the single tenant's row — the deployment's settings. No MySQL multi-tenancy work, no
   dialect-specific API.

   Recorded against D1 rather than as a new decision, because D1 already decides this; what
   was wrong was its *rationale*. "MySQL has no RLS" is true but is not what keeps MySQL
   single-tenant, and reading it that way suggests MySQL has no isolation mechanism at all.
   It has one, and structurally it is stronger than RLS: `MySQLStoreFactory` gives each tenant
   its own **database**, wired unconditionally at `cmd/cleat-worker/main.go:380`.

   **What keeps MySQL single-tenant is that the mechanism is only ever finished for one
   tenant.** The worker migrates exactly one tenant database at boot —
   `mf.TenantDB(ctx, defaultTenantID)`, `main.go:613` — so a second tenant gets a database
   created on first use, with no schema. Measured 2026-09-03 through the factory: database
   created, **0 tables**, `ListWorkflows` → `Table 'cleat_<uuid>.workflow_instances' doesn't
   exist`. `cmd/cleat-worker`'s `TestTenantIsolationOverHTTP_MySQL` passes only because it
   calls `testutil.SetupMySQLFullSchema` on each tenant database itself, and its own comment
   says so.

   That eliminated the option this step existed to consider — putting the settings table in
   MySQL's per-tenant database — since nothing migrates those databases.

   **And it turned up a second thing, now fixed rather than recorded.** D1's limitation was
   enforced by breakage at the point of *use*, not by a check at the point of creation: a
   second MySQL tenant could be created without complaint and failed later with a
   missing-table error, far from the cause. `migrations/mysql/038_single_tenant_guard.sql`
   refuses the insert with a duplicate-key error naming
   `uq_tenants_mysql_is_single_tenant_only_see_tiers_yaml_d1` — the rule and where the rule
   is written down.

   A singleton unique key rather than a trigger, and that is a MySQL restriction rather than
   a preference: a trigger may not read the table it is defined on, so
   `BEFORE INSERT ... IF (SELECT COUNT(*) FROM tenants) > 0 THEN SIGNAL` is not expressible.
   A declarative constraint also holds against any client, where a Go check would hold
   against one — and there is no Go chokepoint to use anyway:
   `auth.TenantStore.CreateTenant` has **no non-test callers** and its SQL is
   PostgreSQL-only (`admin.tenants`, `$1`, `RETURNING`).

   Tested as a pair, `migration/mysql_single_tenant_test.go`: MySQL refuses and the error
   names the key; PostgreSQL still accepts a second tenant, so the guard is MySQL's rule
   rather than something that broke tenants everywhere. Negative control observed rather
   than assumed — with the migration unapplied the MySQL half fails with "a second tenant
   was created on MySQL", the exact pre-fix behaviour.
3. ~~**The settings store**~~ — ✅ **done 2026-09-03.** `tenant_settings` on three dialects
   (`migrations/{postgres/039,mysql/039,mssql/042}_tenant_settings.sql`), a read path per
   dialect, and clamp-to-flag resolution in `engine/tenant_settings.go`. **One consumer is
   wired**: the wall-clock ceiling, at both `executor.go` sites. The instance timeout is step
   5b and the retry budget is step 4; see "what is deliberately not wired" below.

   Four decisions worth the words, because each of them was the difference between a feature
   and a hole:

   **`NULL` means "no override" and is distinct from zero.** Zero is already meaningful at the
   point of use — `executor.go` tests `ceiling > 0` before applying a bound, so zero means
   *unbounded*. Storing an absent override as 0 would both make "leave this to the operator"
   inexpressible and let a row written to set one field silently unbound the other two.

   **The `CHECK` constraints are a privilege boundary, not input validation.** Clamping is what
   keeps a tenant from raising its own limits, and clamping needs a positive value: with 0
   allowed, a tenant writes 0, resolution reads "unbounded", and the clamp hands back *more*
   than the operator granted. Refused in the database on all three dialects, and again in
   `tenantSettingsFromMillis`. MySQL is the dialect where this can silently not hold — `CHECK`
   is parsed and ignored before 8.0.16 — so `TestMySQLTenantSettings` asserts the refusal
   rather than trusting the pinned image.

   **Clamp, never substitute, and the rule is not general.** `ClampToCeiling` is safe only
   because every setting here bounds a resource the tenant consumes, so smaller is safer. A
   future setting where *larger* is the safe direction (a minimum retention, a floor on an
   interval) would need the opposite comparison, and reusing this function for it would grant
   exactly the escalation it exists to prevent. Its doc comment says so.

   **Deletion is by foreign key, not by a fifteenth line in `admin.drop_tenant`.** §3.32's
   function enumerates fourteen tables by hand; `ON DELETE CASCADE` cannot be forgotten and
   composes with it (the function deletes `admin.tenants` last). That relies on referential
   integrity actions not being subject to the FORCE'd RLS policy on the same table, which is
   asserted rather than assumed — `TestDroppingATenantDropsItsSettings`.

   `TenantSettingsReader` is an **optional** interface rather than a hundredth `WorkflowStore`
   method, because `WorkflowStore` has ten implementations and six of them are mocks. The cost
   is the failure mode: a store that stops satisfying it degrades to flag defaults with nothing
   failing. `engine/tenant_settings_wiring_test.go` closes that with compile-time assertions on
   the three real stores, plus a test that `ShardedStore` deliberately does *not* implement it —
   a sharded deployment has no single settings row, and picking one of the three defensible
   semantics by accident would give a tenant its settings on some workflows and the flags on
   others.

   **What is deliberately not wired, and how that stays honest.** An unconsumed field reads like
   a working knob, so `TestOnlyTheWallClockCeilingIsWiredYet` asserts that setting
   `WasmInstanceTimeout` or `HostRetryBudget` changes nothing observable. It fails the day
   either starts having an effect, which is the day this section and those doc comments need
   updating.

   **The migration was wrong in a way no local run could show, and CI caught it.** PostgreSQL's
   default `search_path` is `"$user", public`, and `001_schema.sql` creates a schema called
   `cleat` — so on any deployment connecting as role `cleat`, an unqualified `CREATE TABLE`
   lands in `cleat` rather than `public`. `001` through `004` each open with
   `SET search_path = public;` and `001`'s header records the trap costing "the shipped cluster
   deployment is broken by nothing more than its own username". 039 omitted it, because it is
   only the **third** PostgreSQL migration ever to create a table (001, 032, 039) — `ALTER` on
   an unqualified name falls through `cleat` to `public`, so 006–038 never needed the line and
   the convention had no recent example.

   It passed locally and failed `Cluster Integration Tests` and `Tier 1 Gate` with
   `relation "tenant_settings" does not exist` — from a test that had just built its schema,
   with its three neighbours in the same file passing. The difference is the DSN role: this
   sandbox connects as `postgres` (no such schema, so the path collapses to `public`), CI as
   `cleat`. Reproduced deterministically by creating a database owned by role `cleat` and
   pointing the suite at it — `cleat.tenant_settings` beside `public.workflow_instances`,
   twice out of two — then confirmed fixed the same way. **A migration that only creates
   tables under one role name is not covered by a local run under another.**

   That investigation also turned up a **separate, pre-existing defect not fixed here**:
   `applyAppRoleMigration` (engine/flush_rls_test.go) hands `005_app_role.sql` to `db.Exec`,
   and that file ends with a session-scoped `SET search_path = public`. Measured: before it,
   `SHOW search_path` returns `"$user", public`; after it, 40 consecutive queries on the same
   pool return `public`. Harmless wherever `"$user"` names no real schema, which is why it has
   survived — but it silently rewrites the search_path of every later query on a shared pool.
   One PR, one thing, so it is filed rather than bundled; the fix is a pinned `db.Conn` plus
   `RESET ALL`, with a regression test on the helper.

   **Two things went wrong writing the tests, both instances of this file's recurring theme.**
   The first negative control asserted that dropping the RLS policy makes the other tenant's row
   *visible*; PostgreSQL defaults to DENY when RLS is enabled with no policy at all, so it
   returned 0 and the control failed against a correct implementation. Replacing the predicate
   with `USING (true)` is the right break — and it is also the exact scenario CLAUDE.md names,
   a cross-tenant assertion passing against a wide-open policy. The second: fixture cleanup was
   registered with `t.Cleanup`, which runs *after* the test body's `defer owner.Close()`, so
   every delete ran against a closed pool and failed silently. Three rows survived a green run
   and the next run failed on a duplicate key rather than on anything it was testing. Both tests
   now pre-clean, which also survives a crashed run.
4. ~~**Move the retry threshold host-side**~~ — ✅ **done 2026-09-03.** The host applies the
   resolved budget in `execSession.DurableCallWithRetry` and refuses with `callErrorCode` 6,
   making no call and recording no event. `hostRetryBudget`, `HOST_RETRY_BUDGET_MS`,
   `retryFitsInOneSegment`, `retry_fits_in_one_segment` and
   `cleat.TestBothSDKsAgreeOnTheHostRetryBudget` are all deleted; `--host-retry-budget` is the
   operator's ceiling and `DefaultHostRetryBudget` (60s) keeps every engine built without it
   behaving as before. The §3.88 threshold tests are unmodified and pass.

   Four things worth keeping:

   - **The budget is judged on the policy the GUEST asked for**, before `--max-retries` trims the
     attempt count. Judging the trimmed policy would accept policies that used to suspend — a
     behaviour change wearing a refactor's clothes.
   - **`retryFitsInOneSegment(nil)` returned false**, which routed a nil policy to the SDK loop.
     The replacement condition would have dereferenced it; the guard is explicit and commented.
   - **The explicit escape hatches narrow deliberately.** `DurableCallWithRetry` /
     `cleat_call_with_host_retry` were documented as "the caller's choice" to hold a worker. The
     host refuses them too now, because a budget a guest can opt out of is advisory. Neither has
     a loop to fall back to, so both surface the `CallError`.
   - **The end-to-end Rust test could not tell old from new.** One attempt then a suspension is
     what BOTH designs produce, so its green proved nothing about this change — and would not
     have caught a stale `.wasm` either. Deleting the Rust fallback is what discriminates: the
     test then fails carrying the host's own refusal message, which is also the proof the guest
     is rebuilt and that the refusal crosses the language boundary.
5. **Resolve the two timeouts per tenant.** Split in two by where the bound lives, and only
   the first half is done:
   - ~~5a, the executor's **wall-clock ceiling**~~ — ✅ **done with step 3.** It is a context
     deadline, so it needed only `e.wallClockCeiling(ctx)` at both `executor.go` sites.
     `TestTwoTenantsGetDifferentWallClockCeilings` asserts the *difference* between two
     tenants, plus the clamp, through `executeWithBackend` rather than by calling the
     resolver — the failure this section warns about is a value read correctly and then not
     reaching the place that uses it. Proven to fail two ways, one mutation per claim:
     ignoring the tenant value trips the tightening assertion, substituting instead of
     clamping trips only the escalation assertion.
   - ~~**5b, the backend's instance timeout**~~ — ✅ **done 2026-09-03.** `PerExecution` now
     takes the tenant's value and the per-execution clone carries it into `configureStore`.
     Five implementers touched, four of them test doubles.

     **The clamp lives in the backend, not the engine, and that is the one design decision
     here.** The other two limits clamp in `engine.go` because the operator's value is an
     Engine field. This one's is not: it is `wasmtimeBackend.limits.executionTimeout`, set
     from `--wasm-instance-timeout` when the worker constructs the backend, and the Engine
     never sees it. So the engine passes the tenant's RAW value and the backend clamps, which
     is the only place both numbers are in scope. `e.wasmInstanceTimeout` is **not** the
     ceiling to clamp against — §3.90 stopped applying it as an epoch fence and it survives
     only as `wallClockCeiling`'s fallback, so clamping to it would bound a tenant by a number
     the operator's flag never set.

     Three tests, because three separate things can be wrong and any one of them green is
     consistent with the other two broken: the engine hands the value over, the backend clamps
     rather than substitutes, and the clamped value fences a real guest. The third runs the
     hand-written infinite loop against a 30s operator bound and a 200ms tenant bound and was
     fenced after **200.7ms**; had the tenant's value not reached `configureStore` it would
     have run for 30 seconds, so the elapsed time is the assertion. Both mutations bite only
     their own test — dropping the value in the executor fails the delivery test alone;
     ignoring it in the backend fails the clamp and the fence but not delivery.

     **`TestOnlyTheWallClockCeilingIsWiredYet` was replaced, not updated.** It promised to
     "fail the day either one starts having an effect" and could not: it measured only the
     wall-clock deadline, which neither of the other two settings bounds, so steps 4 and 5b
     both shipped underneath it while it stayed green. `TestTheThreeSettingsBoundDifferentThings`
     keeps the invariant that was actually worth having — the three limits bound three
     different things and must not leak into each other's bound, which is §3.90's finding.
6. **Per-tenant validation** replacing §3.90's startup warning.

#### What to be suspicious of when building this

- **A per-tenant limit that is never exercised.** The failure mode is a settings row that is
  written, read, and then ignored because the value reaches the executor but not the backend, or
  reaches the backend but after `configureStore`. §3.90 is the precedent: the timeout was applied
  in two places and fixing one changed nothing observable. **Test through the engine with two
  tenants whose values differ, and assert the difference — not that one tenant's value is
  honoured.**
- **A test that proves isolation against a wide-open policy.** CLAUDE.md's standing warning: a
  cross-tenant assertion passed against a wide-open security policy because the store's own SQL
  carried `tenant_id = ?`. Break the specific layer and watch.

### 3.96 A recorded plugin stream error replayed as a success — ✅ **FIXED** (WS-2, 2026-09-03)

> **Numbered 3.90 when it was committed, then 3.91, 3.93, 3.94, 3.95, and now 3.96 — renumbered
> five times on five successive rebases, because #613, #619, #624, #626 and #628 each took the
> number while this branch was open.** Tenth number collision in two days, and the seventh for
> this stream. §3.89 records the
> mechanism and it holds here unchanged: every stream appends its new section to the end of
> one file, so two streams appending on the same day collide in the text *and* in the number,
> by construction. `check-section-numbers.sh` passed on this branch before the rebase and
> could not have caught it — it checks uniqueness within one file, and the duplicate lived on
> `develop` in a section this branch had never seen. The renumber is cheap; the re-verification
> it forces is not.
>
> **The second renumber says something the first did not: picking "one past the highest" is
> not a fix, it is a race this branch loses every time it is open across a merge.** 3.91 was
> free when this branch claimed it and taken by the time the branch could be pushed. The
> number is only safe at the moment of merge, so the last thing to do before pushing is
> re-derive it against `origin/develop` — `git show origin/develop:IMPROVEMENT-PLAN.md |
> grep -oE '^### 3\.[0-9]+ ' | sort -t. -k2 -n | tail -1` — and not trust the one that
> passed `check-section-numbers.sh` locally, which by construction cannot see the other side.
>
> **The third and fourth renumbers found the failure mode that re-derivation does not cover:
> the number arrived in a rebase that did not conflict.** #624 appended its section far enough from this one that
> git merged both cleanly, so there was no marker to resolve and nothing to prompt a re-check —
> the tree was clean, the rebase said "Successfully rebased", and the file held two §3.93
> headings. `check-section-numbers.sh` is what caught it, which is the argument for running it
> after *every* rebase and not only after a conflicted one. A clean rebase is not evidence that
> the numbering survived; it is only evidence that the text did.
>
> **The fourth renumber is the same trap a second time, twenty minutes later, and it says the
> per-branch check cannot be made to work.** #626 took 3.94 while this branch held it, appended
> far enough away to merge cleanly again, and `check-section-numbers.sh` caught it again. Four
> renumbers on one unchanged patch is not four unlucky days; it is what a
> last-writer-wins-on-a-shared-counter looks like when three streams append to one file. The
> fixes are structural, and both are outside this section: **require branches to be up to date
> before merging**, so the second PR has to rebase and re-run rather than landing on a base its
> checks never saw, or stop numbering sections in a single global sequence at all. Recorded
> here rather than done here because it is a repository setting or a document-wide convention,
> and either is a call for the three streams together.
>
> **Fifth renumber, and this one was caused by a PR fixing the collisions.** #628 renumbered a
> duplicate onto 3.95, which is where this branch had just moved to escape #626 taking 3.94 —
> so the cleanup itself took the number the cleanup had freed. That is the clearest statement
> of the problem available: with a single global counter and no serialisation, even the repair
> collides, and picking "one past the highest" is a bet that nothing merges before the push.

`recordStreamError` marks a stream-level plugin failure as a single chunk with
`StreamFinish` set, and `replayPluginCallStreaming` recognises that failure by exactly
that flag:

```go
if len(collected) == 1 && collected[0].Finish {
    // return the error
}
```

**`StreamFinish` was never persisted.** Neither `eventRecordToPayload` nor any column
carried it, so it survived only for as long as the record stayed in memory. Replay on a
worker that loaded its history from the database sees `Finish: false`, the error branch
cannot fire, and the collected chunks are marshalled and returned as a **success** whose
content is the error text. `StreamChunkIndex` was lost the same way.

Measured before the fix, through the real store on both configured dialects:

| | |
|---|---|
| `StreamFinish` in, PostgreSQL and MySQL | `true` |
| `StreamFinish` out | **`false`** on both |
| the guest's error code on replay | **`0`** — success |

So this is §2.35's constraint violated in the direction that matters: fresh reports a
failure, replay reports a success, for the same step.

**The existing test could not see it, and how it could not is the general lesson.**
`engine/host_test.go`'s `TestPluginCallStreamingReplay_StreamError` covers this exact
branch and passed throughout, because it assigns `s.history` directly — a history the
store cannot produce. The assertion was held up by an in-memory struct field while the
layer under test dropped it. The new tests differ in one respect only: the record goes
through the real encoder and decoder first.

#### The fix, and why it needs no migration

Both fields join the `plugin_call_stream_chunk` arms of `eventRecordToPayload` and
`populateFromPayload`, **written only when set**. `payload` is JSONB and the stored
checksum is a hash of exactly these bytes, so an absent key leaves every stream chunk
already in `event_history` byte-identical and still verifying. Same discipline as
`error_non_retryable` (§2.35) and `resolved_by` (§1.4 phase F).

That compatibility claim is asserted rather than argued:
`TestAPre391StreamChunkStillMatchesItsStoredChecksum` pins the literal checksum of a
pre-3.96 chunk. **The literal was taken by running the computation against `develop`'s
encoder** — `git stash push engine/store_events.go`, run, `git stash pop` — not by
copying what the new code printed, which would have pinned the bug. The first draft of
that test carried an invented literal; it failed, which is the only reason the real
measurement got made.

Falsified, each for its own reason: reverting the fix turns the replay test red with
`errCode=0` (the guest told SUCCESS) and the round-trip test red on the dropped flag,
while both compatibility tests stay green — they guard against the fix over-reaching, so
they are falsified the other way, by writing the keys unconditionally, which turns both
red including the checksum.

**Not fixed here: §2.35's plugin residual**, which is the next item. All four stream
failure causes still report one code, so a stream function erroring is non-retryable to
the guest where the identical non-streaming failure is retryable (`callFailureCode`).
That is a real asymmetry and it is broader than §2.35 states — three of the four causes,
not one — but it is a separate change, and it was unfixable while the error branch could
not fire on replay at all.

### 3.97 `EventRecord.CreatedAt` comes back on one dialect of three — ✅ **FIXED 2026-09-03 in §3.102**, which turned out to be the smaller half of the defect (WS-2, 2026-09-03)

`event_history.created_at` is written by every backend and read back by one.

| dialect | `timestamp_ms` | `CreatedAt` | why |
|---|---|---|---|
| PostgreSQL | derived from `created_at` | ✅ populated | the SELECT lists `created_at` and scans it (`engine/store_events.go`) |
| MySQL | derived from `created_at` | ❌ zero time | the SELECT computes `UNIX_TIMESTAMP(created_at)*1000` and never reads the column (`engine/mysql_events.go`) |
| SQL Server | derived in Go | ❌ zero time | the SELECT *does* read `created_at`, scans it into a local, sets `rec.TimestampMs = createdAt.UnixMilli()`, and drops the value (`engine/mssql_events.go`) |

Measured 2026-09-03 through `AppendEventHistory` / `LoadEventHistory` on PostgreSQL and MySQL;
the SQL Server row is read from the code, because this sandbox cannot run that dialect. Found by
`TestThePayloadExemptFieldsAreCarriedByColumnsInstead`
(`engine/payload_carrier_completeness_test.go`), whose whole purpose is to check the "a column
carries it" claims in `payloadExemptFields` rather than restate them — the first exemption it
checked turned out to be true on one dialect out of three.

**Not a durability defect.** `CreatedAt` is documented on `EventRecord` as being for admin
timeline visualization and is not read by any replay path, which is why §3.98 exempts it from the
payload carrier rather than carrying it. What breaks is a timeline: an admin view of a workflow's
history shows the zero time on two of the three backends.

**Why it is not fixed alongside §3.98, when the patch is two lines.** Because two lines is the
wrong size for it. `LoadEventHistory` is one of three read paths per dialect —
`LoadEventHistoryPaginated` and `StreamEventHistory` are the others, and MySQL's streaming query
does not select `created_at` either. Fixing the one path the test happens to call would turn the
assertion green over five paths it never touched, which is the exact shape of green result
CLAUDE.md's "Is this result real?" section is about. The fix belongs with a test that holds
**every read path on every dialect to the same `EventRecord` for the same row**, which is a
different and more useful piece of work than three sed edits.

Re-derive the gap:

```
grep -n "created_at" engine/store_events.go engine/mysql_events.go engine/mssql_events.go
```

`parseTime=true` is not a blocker for the MySQL half: `cleat-worker`'s `--db` help text and
`docs/reference/database-backends.md` already require it, and `engine/mysql_ops.go` already scans
`sql.NullTime` in several places.

### 3.100 A merge's own verification is cancelled by the next merge — 🟢 **FIXED 2026-09-04; the 2026-09-03 fix was half of one** (WS-1, 2026-09-03)

Six workflows carried

```yaml
concurrency:
  group: ${{ github.workflow }}-${{ github.ref }}
  cancel-in-progress: true
```

and all six run on `push` to `develop` as well as on `pull_request`. On a push `github.ref` is
`refs/heads/develop` for **every** merge, so consecutive merges share one concurrency group and
each cancels the one before it. CLAUDE.md already names this — *"A merge's own `develop` run can be
cancelled by the next merge landing seconds later, and `cancelled` is not `success`"* — as
something to watch for when verifying. It was also true of the repo's own CI, unwatched.

#### Why it matters more than a lost run

**The post-merge run is the only place some guards can work at all.** `check-section-numbers.sh`
exists to catch collisions that, in its own CI comment's words, *"exist only in the merge"* — two
PRs each picking the next free number against a develop that has since moved. From a pre-merge base
it cannot see them by construction. So the run that can catch them is precisely the run being
cancelled.

Measured on develop, 2026-09-03:

```
gh run list --branch develop --workflow ci.yml --limit 20
completed/cancelled  16:50:09  1f79398a
completed/failure    16:05:39  c2b937d1
completed/failure    15:55:22  4575be85
completed/cancelled  15:46:59  9480ee50
completed/cancelled  12:52:48  b3ea16d4
```

Three cancelled and two failed in one afternoon. **The two failures are the more interesting
half**: the mechanism worked, develop went red on the duplicate `3.93`, and nobody — including the
author of one of the two colliding PRs — looked. So this entry fixes the cancellation and does not
claim to have fixed the noticing.

Seven section-number collisions and five additive skip-budget resolutions were resolved by hand on
2026-09-03 alone; every one was created by merging rather than authored against a shared base.

#### The fix

`cancel-in-progress: ${{ github.event_name == 'pull_request' }}` in all six. On a pull request it
still cancels, which is the setting's real purpose — pushing twice to a branch should not run the
suite twice. On a push it never does, so each merge's verification completes.

The cost is real and worth stating: develop now runs the full suite once per merge rather than once
per burst of merges. On a day like 2026-09-03 that is several more full runs. That is the price of
the post-merge run existing at all.

`scripts/check-workflow-concurrency.sh` keeps it from coming back, and runs in the lint job beside
the section-number guard it protects. Falsified both ways: restoring `cancel-in-progress: true` in
one workflow fails it by name, and pointing its glob at nothing makes it `exit 1` rather than
report a clean scan of nothing.

#### The first fix was half a fix, and the guard passed anyway (2026-09-04)

Everything above is still true and still necessary. It was not sufficient, and the way it failed is
worth more than the fix.

`cancel-in-progress: false` governs the **running** run. It says nothing about the **queued** one,
and GitHub keeps at most one pending run per concurrency group: when a third push arrives at an
occupied group, the one waiting is cancelled before it ever starts. Consecutive merges still shared
one group, so they still queued behind each other, so a merge landing during a long run still had
its verification thrown away — with `cancelled` reported exactly as before.

Measured on develop between 14:20 (when the first fix merged) and 00:57, 2026-09-03:

| workflow | push runs | cancelled |
|---|---|---|
| **Tier 1 Gate** | 36 | **11** |
| CI/CD Pipeline | 36 | 3 |
| Cross-Language E2E | 36 | 2 |
| Multi-DB CI | 36 | 2 |
| Plugin Harness Tests | 36 | 1 |
| Tier 2 Gate | 36 | 1 |

```
for wf in "CI/CD Pipeline" "Cross-Language E2E" "Multi-DB CI" \
          "Plugin Harness Tests" "Tier 1 Gate" "Tier 2 Gate"; do
  json=$(gh run list --workflow="$wf" --branch develop --event push --limit 60 \
           --json conclusion,createdAt)
  n=$(echo "$json" | jq '[.[] | select(.createdAt > "2026-09-03T14:20:48Z")] | length')
  c=$(echo "$json" | jq '[.[] | select(.createdAt > "2026-09-03T14:20:48Z")
                             | select(.conclusion=="cancelled")] | length')
  printf '%-22s push-runs=%-4s cancelled=%s\n' "$wf" "$n" "$c"
done
```

**Roughly a third of merges never reached the gate at all** — not "ran and passed", not "ran and
failed", but never started. The rate tracks duration: the gate takes 20-40 minutes and merges land
minutes apart, so its queue is almost always occupied, while Tier 2 drains fast enough to be hit
once.

The distinguishing observation, and the one that turns this from a plausible story into a measured
one, is that a cancelled run has **no jobs**:

```
gh run view 33822486816 --json jobs --jq '.jobs | length'   # 0   (cancelled, eb347bfd)
gh run view 33820005763 --json jobs --jq '.jobs | length'   # 0   (cancelled, b6562c93)
gh run view 33822626919 --json jobs --jq '.jobs | length'   # 1   (success,   2eeec9e3)
```

A run killed while *running* has jobs with a `cancelled` conclusion. Zero jobs means it never left
the queue, which is what separates mechanism (2) from mechanism (1) rather than leaving them as two
readings of the same evidence. The timestamps agree: each cancellation lands 1-3 seconds after the
next merge's run is created, with a third run still in progress ahead of both.

#### The second fix

Give each push a group of its own, so there is no queue to be evicted from:

```yaml
group: ${{ github.workflow }}-${{ github.event_name == 'pull_request' && github.ref || github.sha }}
cancel-in-progress: ${{ github.event_name == 'pull_request' }}
```

On a pull request the group stays per-ref and cancellation keeps its real purpose. On a push the
group is per-commit, so no two merges ever contend and `cancel-in-progress` never applies. The cost
stated above gets larger, honestly: develop can now run several gates concurrently during a burst
rather than serialising them. That is what "each merge is verified" costs.

#### What this says about the guard

`scripts/check-workflow-concurrency.sh` **passed the entire time.** It checked
`cancel-in-progress: true` and nothing else, so it was blind to the mechanism that was actually
discarding runs. Demonstrated directly — the old script against the exact configuration that lost
11 runs:

```
$ git show 658116d9:scripts/check-workflow-concurrency.sh > scripts/_old.sh
$ chmod +x scripts/_old.sh
$ ./scripts/_old.sh                   # with the ref-keyed group restored in tier1-gate.yml
OK: 9 push-triggered workflows, none cancels its own runs.
rc=0
```

It has to be run from inside `scripts/`: the script does `cd "$(dirname "$0")/.."`, so a copy in
`/tmp` walks to `/`, finds no workflows, and exits 1 on its own negative control. That red is not
the answer to the question — it is the check failing to run, which is the distinction this
document keeps making in the other direction.

That is this repo's recurring failure — a green result that measured nothing — in a script written
*to prevent* a green result that measured nothing, one day after writing it. The guard now checks
both mechanisms and both arms are falsified by name, and it carries a second negative control:
`with_concurrency == 0` fails, because a group check that finds no groups would pass whatever the
workflows said.

The narrower lesson is about how the first fix was verified. It was checked by reading the YAML and
confirming the expression evaluated to `false` on a push — which was true, and was not the
question. Nothing looked at whether develop's push runs actually completed afterwards. **A fix to
CI's behaviour is verified by CI's subsequent behaviour, not by its configuration**, and the
observation that would have caught this — the same `gh run list` in the table above — takes one
command.

#### Not fixed here

**Nobody watches develop.** A red post-merge run is now guaranteed to exist, and still nothing
routes it to a person. Options are a notification, or a job that opens an issue on failure, and
both are choices about how this project wants to be interrupted rather than a defect to close.

### 3.98 The database payload carried none of four fields the replay path reads — ✅ **FIXED** (WS-2, 2026-09-03)

An `EventRecord` passes through three carriers, and two of them had something checking that every
field survives.

| carrier | check |
|---|---|
| compaction (`compactedEvent`) | `FuzzCompactionEquivalence`, reflection over every exported field against `compactionExemptFields` |
| typed events (`engine/events.go`) | `TestEvent_RoundTrip` |
| **the database payload** (`eventRecordToPayload` / `populateFromPayload`) | **nothing** — per-event-type tests only, one written after each defect |

So a field that no arm wrote was invisible until something noticed the behaviour it broke. Four
were missing, and every one of them was found by reading a comment in `engine/compaction.go`
explaining why *compaction* carries the field:

| field | consequence, measured on PostgreSQL and MySQL |
|---|---|
| `NewVersion` | `ContinueAsNewWithVersion` reads it straight off the replayed event. Wrote 3, read 0 — and 0 means "current version", so a versioned continue-as-new replayed from the database **restarts as different code than the run that recorded it** |
| `StateKeys` | `ListState`'s replay path writes it to the guest verbatim. Wrote `["user:a","user:b"]`, read `""` — and `StateKey` and `StateOp` both survive, so the divergence guard ahead of it passes and the empty result is handed over **as a success** |
| `ParentWorkflowID`, `ParentClosePolicy` | not read on replay; carried for the reason `compactedEvent` gives for carrying the same two — a faithful copy rather than a replay-sufficient-but-lossy one, at a line each, and one fewer exemption |

§3.96 was the third instance of the same shape and was found the same way; this section is the
mechanism that would have found all of them.

**What the new test checks, and what it does not.**
`TestEveryEventRecordFieldTheDatabasePayloadMustCarryDoesCarry` asserts that every non-exempt
field survives the round trip for *at least one* event type. It does **not** assert that the field
survives for the event type that produces it — deriving that association needs a per-type table,
and the table is the thing that would go stale. The narrower claim still catches the defect: a
field written into no arm at all survives for zero event types.
`TestTheFieldsTheReplayPathReadsSurviveTheStore` covers the specific pairs through a real
database, which is what rules out "the payload drops it but a column saves it".

`payloadExemptFields` carries a reason per field, audited individually, and
`TestThePayloadExemptFieldsAreCarriedByColumnsInstead` checks the ones that claim a column —
without which a wrong reason reads as an audit. It immediately found §3.97.

**Compatibility.** All four keys are written only when the field is non-zero, so every event
already in `event_history` produces a byte-identical payload and still verifies against its stored
checksum. `TestRecordsWrittenBeforeTheseKeysStillMatchTheirStoredChecksums` pins three literals,
each **measured against the encoder without this change** (stash the file, run, unstash) rather
than copied from what the new code prints, which would pin whatever it happens to do.
`TestSettingTheNewFieldsChangesTheChecksum` is the other direction, and it is the one an
only-when-set guard can get wrong invisibly: a key written outside the checksummed payload leaves
the checksum unchanged, and the compatibility test alone would still pass.

**Also fixed here:** two comments in `engine/compaction.go` cited a property test named
`TestEventRecordFieldsSurviveCompaction`, which has never existed — the real mechanism is
`FuzzCompactionEquivalence` via `eventFieldsMatch`. One of the two was the stated justification for
carrying `ParentWorkflowID` and `ParentClosePolicy`, so grepping the name it gave returned nothing
and the justification read as fabricated. It cost a detour to establish that the mechanism was real
under another name.

### 3.102 Nine read paths, four different answers about the same row — ✅ **FIXED** (WS-2, 2026-09-03)

Every dialect has three ways to read event history — `LoadEventHistory`,
`LoadEventHistoryPaginated`, `StreamEventHistory` — so there are nine implementations, each with
its own `SELECT` and its own scan. Nothing held them to the same answer, and they did not give
one. Measured 2026-09-03 by writing `TimestampMs: 1756900000123` and reading it back:

| dialect | path | `TimestampMs` | `CreatedAt` |
|---|---|---|---|
| PostgreSQL | `LoadEventHistory` | **1756900000000** — truncated to the second | ✅ |
| PostgreSQL | `LoadEventHistoryPaginated` | **0** — never set | ✅ |
| PostgreSQL | `StreamEventHistory` | **1756900000000** — truncated | ✅ |
| MySQL | `LoadEventHistory` | ✅ 1756900000123 | **zero time** |
| MySQL | `LoadEventHistoryPaginated` | **0** | **zero time** |
| MySQL | `StreamEventHistory` | ✅ | **zero time** |
| SQL Server | `LoadEventHistory` | ✅ (Go `UnixMilli`) | **zero time** |
| SQL Server | `LoadEventHistoryPaginated` | **0** — `created_at` not in the `SELECT` at all | **zero time** |
| SQL Server | `StreamEventHistory` | ✅ | **zero time** |

**`TimestampMs` is the replay virtual clock.** `execSession.Now` (`engine/lifecycle.go:203`)
returns the previous history event's `TimestampMs`. So on PostgreSQL — the default dialect — a
workflow resuming after a crash saw a `Now()` up to **999 ms earlier than the run that recorded
it**, because `EXTRACT(EPOCH FROM created_at)::BIGINT * 1000` drops the milliseconds the write
path stored. That is engine-introduced replay non-determinism, and it is the same class
`compaction.go` already warns about for a *zero* `TimestampMs` — one file over, in prose, about a
different carrier.

The `0` from the paginated path is not a replay bug — nothing replays from it — but all three of
its callers are user-facing: `cmd/cleat-worker/api_instances.go`, `cmd/cleat-worker/server.go` and
`cmd/cleatctl debug`. `TimestampMs` has no `omitempty`, so the admin API returned
`"timestamp_ms": 0` for every event: not a missing field, a wrong value presented as a real one.

**The fix is one rule rather than nine patches.** Every read path now selects `created_at` and
nothing else time-shaped, and `applyCreatedAt` (`engine/store_events.go`) derives both fields from
it. The three different SQL-side `timestamp_ms` expressions are gone, so there is nothing left for
a tenth read path to copy wrongly. §3.97 — `CreatedAt` populated on PostgreSQL only — falls out of
the same change and is closed by it.

`engine/read_path_parity_test.go` is the mechanism, and it makes two separate claims on purpose:

- **the paths agree with each other**, by reflecting over every exported `EventRecord` field
- **and the answer is the one that was written**, because three paths that all return zero agree
  perfectly

The probe value carries milliseconds that are not a whole second. A round number round-trips
through a truncating read and proves nothing.

**Not covered, deliberately: `DBEventStream` (`engine/event_stream.go`), a fourth reader.** It is
worse than anything fixed here — it selects no `payload` column, so every payload-carried field
comes back empty, and its `WHERE` clause is `workflow_id = $1` with **no `tenant_id` predicate**,
so if it were wired up it would read across tenants. It has no production callers:

```
grep -rn "NewDBEventStream" --include="*.go" .    # only its own tests, 2026-09-03
```

Testing it would mean asserting against code nothing calls, which is the shape §2.35 refused when
it declined to add a mechanism ahead of a caller. Deleting it or fixing it is a decision for
whoever owns that type; the cross-tenant `WHERE` is the part worth someone's attention, and it is
the same class WS-1 has been closing in §3.86, §3.91 and §3.92.

**Behaviour change to be aware of.** PostgreSQL replays now see the millisecond `Now()` the
original run produced rather than a truncated one. That is a workflow-visible difference for
anything in flight — and it is a difference *towards* the recorded value: the fresh run always
used the full millisecond, and only the read was lossy, so this makes replay agree with the run it
is replaying instead of diverging from it.

### 3.103 The `EventStream` abstraction had no callers, and one of its two implementations read across tenants — ✅ **FIXED by deletion** (WS-2, 2026-09-03)

`engine/event_stream.go` declared an `EventStream` interface with two implementations,
`SliceEventStream` and `DBEventStream`, and 658 lines of tests for them. Nothing used any of it.
Measured 2026-09-03 against `develop` at `546788a`, for every exported name:

```
for n in EventStream SliceEventStream NewSliceEventStream DBEventStream NewDBEventStream; do
  grep -rn "\b$n\b" --include="*.go" . \
    | grep -vE "event_stream\.go|event_stream_test\.go|read_path_parity_test\.go" \
    | grep -v StreamEventHistory
done
```

Every one returns nothing. 976 lines, an exported interface and two exported constructors, and
the only code that ever called them was their own test file.

**Note the shape of that command, because the first version of it was wrong and said the
opposite.** It reported 11–25 "external references" for each name, because the `grep -v` filters
were written against `./engine/event_stream.go` while `grep -rn` was emitting
`engine/event_stream.go` — so the filters matched nothing and every self-reference counted as
external. A filter that silently fails to filter reports the code as *live*, which is the safe
direction to be wrong in but still a wrong number that was about to be written down. **Run the
command and read its output before writing the sentence about what it prints.**

**Why deletion rather than a fix.** `DBEventStream.ensureLoaded` was a fourth event-history read
path, and it was worse than the three §3.102 corrected:

- its `SELECT` had **no `payload` column**, so every payload-carried field — the plugin fields,
  the state fields, the stream fields, everything §3.98 and §3.102 were about — came back empty;
- its `WHERE` clause was `workflow_id = $1` with **no `tenant_id` predicate**, so wiring it up
  would have read another tenant's history. That is the same class WS-1 closed in §3.86, §3.91
  and §3.92, sitting unnoticed in a type nothing constructed.

And it was unfixable in place: `NewDBEventStream(db *sql.DB, workflowID string, pageSize int)`
takes a bare `*sql.DB` with no tenant context, so the missing predicate needed an exported
signature change. Fixing it would then have produced a PostgreSQL-only duplicate of
`StreamEventHistory`, which is already tenant-scoped, payload-carrying, implemented on all three
dialects, and covered by `engine/read_path_parity_test.go`.

**This is §1.4's lesson a second time.** That section records 350 lines of durability code in
`engine/flush.go` that had never run; §2.35 cites it as the reason it *"deliberately added no
mechanism ahead of"* a caller. This is the same thing one file over, and it had acquired a
tenant-isolation defect while nobody was looking — which is the argument for deleting dead code
rather than leaving it: an unused path is not inert, it is unreviewed.

`SliceEventStream` went with it. It had no callers either, and an interface with one dead
implementation is not an abstraction.

### 3.104 A defer segment could still make an outbound HTTP request — ✅ **FIXED** (WS-2, 2026-09-03)

§3.84 guarded six fresh paths so a defer segment cannot start new work past the frontier.
`execSession.Fetch` was the seventh and was not in its inventory: that table was built by reading
the entry points a guest uses to reach a *service*, and `cleat_fetch` reaches one without going
through the durable-call family, so it was never a candidate.

**It matters more than "one more path" suggests.** An outbound HTTP request is the most
externally visible kind of new work there is — it leaves a side effect on someone else's server
that no amount of unwinding takes back — and the workflow making it has already been recorded as
terminated. Every other unguarded path §3.84 closed was recoverable by comparison.

**Go guests were never exposed**, which is why the Go-only defer segment never surfaced it:
`cleat/runtime_workflow.go`'s `DurableFetch` builds a request map and calls
`DurableCall("http", "fetch", ...)`, so it goes through the path §3.84 guards. The guests that
import `cleat_fetch` directly reach `execSession.Fetch`, and those are Java
(`crates/cleat-java`, `cleatFetchRaw`) and AssemblyScript — neither of which can run a defer
segment yet, because `deferSegmentLanguages` is `{"go": true}`. So this was a hole waiting for
the SDK work rather than a live defect, and it is closed before that work rather than after.

The fix is the same one line the other six use:

```go
if s.stopBeforeNewWork() {
    return callSuspendSentinel
}
```

**And the layout question is answered rather than assumed.** `Fetch` returns `packSimpleResult`,
which is not the layout §3.83's `cleat_call` work used — and a bit that is free in one layout and
reachable in another is exactly the defect §3.83 exists to record.
`TestStopSentinelBitsAcrossEveryLayout` measures `packSimpleResult` as
`free=ff000000ffffff00`, so bit 31 is free; `TestTheStopSentinelIsNotAReachableSimpleResult` in
`engine/defer_segment_fetch_test.go` re-derives it locally rather than trusting the number in a
comment.

**Falsified in both directions**, which this one needs: removing the guard makes
`TestADeferSegmentCannotStartAnHTTPFetch` report the fetcher actually invoked and one event
recorded, and `TestAnOrdinarySegmentStillFetches` is the other direction — a guard that blocked
*every* fetch would satisfy the first test just as well. The fetcher is a recording double, so
"did the request go out" is measured rather than inferred from a return value.

### 3.105 The Java SDK could not run a defer segment, because it never decoded the stop sentinel — ✅ **FIXED, both halves; `java` is in `deferSegmentLanguages`** (WS-2, 2026-09-03)

`deferSegmentLanguages` was `{"go": true}` when this item opened, and five languages are
supported. The reason was not that the other four were unfinished in general — it is one specific thing: when the host refuses a call
in a defer segment it sets bit 31 of the result word (§3.84), and only the Go SDK tests that bit.
Everything else reads the word through whichever layout the call it made returns, and **every one
of those readings is a plausible ordinary result**.

This is the Java half of that, and it is deliberately not the whole item.

#### What the Go SDK gets for free and Java does not

`wasm/adapter_metadata.go` generates the Go guest adapter, and `withSuspendCheck` prefixes the
test onto every decoder that needs one, so a decoder added later cannot forget it. Java's
`HostCalls.java` is hand-written: 48 `long result = ...` sites, no structure holding any of them
to the rule. So the check is added *and* the structure is, in `engine/java_sdk_stop_bit_parity_test.go`.

#### Guarding everything would have been wrong, and finding out why is the useful part

The obvious rule — "test the bit at every host call" — is a defect.
`TestStopSentinelBitsAcrossEveryLayout` measures `packSleepResult` as
`reachable=ff0000fffffffc00`: **bit 31 is reachable there**, so a guard on `cleatSleepMs` would
read an ordinary sleep result as a stop. It is also the one call that does not need a sentinel —
a sleeping guest suspends through its own status byte and never reaches a fresh call.

So the rule is a list, and the list is checked from both ends:

| test | what it holds |
|---|---|
| `TestTheJavaSDKAgreesOnTheStopBit` | the SDK's `SUSPEND_STOP_BIT` is the engine's `callSuspendSentinel`, compared as bit *positions* |
| `TestEveryJavaCallTheHostCanRefuseChecksTheStopBit` | each of the ten methods reaching a host stop site guards, **and guards before decoding** |
| `TestTheJavaSleepPathDoesNotCheckTheStopBit` | `cleatSleepMs` does **not**, with the reachability reason |
| `TestTheRequiredJavaGuardsCoverEveryHostStopSite` | the engine's `stopBeforeNewWork()` site count still matches what the list was written against |

#### The count gate had slack, and falsifying it is what showed that

The last test first compared `len(javaCallsTheHostCanRefuse) >= stopSites`. Ten Java methods cover
seven sites — `PluginCall` has two callers — so **three new stop sites could land before it
fired**, and adding an eighth left it green. A gate with slack reports green through exactly the
change it exists to catch. It is an exact pinned count now, and the failure message says to check
the other side rather than just move the number.

#### Two defects found while auditing rather than by the tests

- `pluginCallOutcome` (`HostCalls.java`) calls the same `importPluginCall` as `pluginCall` and was
  unguarded. Found by listing all 48 sites rather than by reading the six the design named.
- §3.104 — `Fetch` was a seventh unguarded fresh path on the **host** side, found the same way.
  Fixed there, and it is why `cleatFetch` is on the list above.

#### The end-to-end half, which is what actually opened the fence

`examples/saga-java-port` gained a `defer_order` entry point — two defers, then one call of the
workflow's own — and `engine/java_defer_segment_e2e_test.go` builds the real TeaVM module and
runs it as a defer segment. Green: the segment suspends and records `[second first]`, the
cleanups in LIFO order, and not the body's call. `java` is now in `deferSegmentLanguages`, and
`TestDeferSegmentLanguagesIsExactlyWhatHasBeenVerified` pins the list at `{go, java}`.

The whole toolchain rather than a synthetic module, because the sentinel crosses three layers a
hand-built module does not have: TeaVM compiles `throw` into its own unwinding scheme, the
generated wrapper catches `SuspendSignal` in a `catch` TeaVM also compiles, and the host then
calls a second export into a runtime that has just thrown. None of those is exercised by reading
Java source with a regex, which is all the §3.105 SDK half could do.

#### Three falsifications, three different reds, and one of them changed a test

Deleting `Memory.throwIfStopped` from `cleatCall` and rebuilding produced:

    suspended: nil   result: {"status":"ok"}   operations: []

— the segment reported the terminated workflow as having **completed normally** and performed
**none** of its cleanup. The wrapper drained on its ordinary success path and every defer body's
call was refused in turn: §3.81's consumption, reached from the other direction.

**That falsification also showed one of the three assertions cannot fail on a guest-side
regression.** `body` was still absent from the caller, because `stopBeforeNewWork` refuses the
call *host*-side before the `ServiceCaller` is reached — the sentinel tells the guest, it is not
what does the refusing. The check is kept, because it still catches a host-side regression, but
the comment now says which half it guards instead of implying it covers both. A test whose stated
purpose and actual coverage differ is the §1.1 trap in miniature.

The other two falsifications went at the generated wrapper. Draining with `Defer.runDeferred()`
inside `catch (SuspendSignal)` **traps** — the first cleanup's call is refused, throws again, and
escapes the catch; draining with `runDeferredForHost()`, which swallows, gives `defers_run=0` and
the empty list. So the processor's choice between the two drains on each exit path is load-bearing
in both directions, not a stylistic one.

#### The test names are load-bearing, and the first version of them was dead

Written first as `TestAJavaDeferSegment...` and `TestAnOrdinaryJava...`. `e2e-cross-language.yml`
is the only job that installs Java and selects with
`-run "TestRust|TestPython|TestAssemblyScript|TestJava"` — which **neither name matches**, since
`TestAJava` does not contain `TestJava`. They would have skipped in every job that ran them and
never run in the one job that could, while the suite printed `ok`: a green result that measured
nothing, produced by a test written to stop exactly that. Renamed to `TestJavaDeferSegment...` and
`TestJavaOrdinarySegment...`. **Any cross-language test whose name lacks the language prefix is
dead code with a skip in front of it**, and nothing in CI says so.

Skip budget: +2 on `test-go/engine` and `cluster`, the toolchain-gated one-skip shape, counted by
name from a run with `gradle` and `java` off `PATH`.

#### What remains

Rust (§3.107) and AssemblyScript (§3.106) have their SDK halves and still need this same
end-to-end piece before they join the list. AssemblyScript's will be the interesting one: it has
no exceptions, so a stop is a flag rather than an unwind, and a body that ignores it keeps
running — its `runDeferred` would then consume the table exactly as the falsification above did.

Python is a different problem, and still blocked on a decision rather than on effort: it is a
Component Model guest whose WIT declares `durable-call: func(...) -> string`, so there is no
result word to carry bit 31 — and `extractStringFromPacked` (`engine/component_cgo.go`) turns the
sentinel into `""`, an empty successful response. Measured 2026-09-03:
`extractStringFromPacked(0x80000000, buf) == ""`. The two options are a sentinel prefix in the
string (matching the existing `"__CLEAT_ERROR__:"` convention, but re-opening exactly the
collision §3.83 exists to record) or changing the WIT so a stop is unrepresentable as a response.
The second is the right one and is a public-API change.

### 3.106 The AssemblyScript SDK could not run a defer segment either, and its stop cannot unwind — 🔶 **SDK HALF DONE; the end-to-end proof and `deferSegmentLanguages` remain** (WS-2, 2026-09-03)

The AssemblyScript half of §3.105's problem: the host marks a refused call in a defer segment
with bit 31 (§3.84), and this SDK never tested it. Same nine methods, same ordering contract,
same forbidden sleep path — `packSleepResult` is the one layout where bit 31 is *reachable*, so a
guard there would read an ordinary sleep result as a stop.

**What is different, and it is the part worth reading.** Go panics, Java throws, and Rust (after
§3.87) returns `Err(CallError::Suspended)`. Each of those takes the workflow body out of its own
control flow. **AssemblyScript has no exceptions** — it builds with `--runtime stub` — so a stop
can only set the suspension flag and hand back an error result. A workflow body that checks
neither keeps running.

That is a genuinely weaker guarantee and the SDK now says so rather than implying parity. What
makes it acceptable rather than a hole is **where the enforcement lives**: `stopBeforeNewWork()`
stays true for the whole segment, so the host refuses *every* subsequent call, not just the first.
A guest that runs on cannot reach anything durable — it can burn instructions and return a value
the host discards, because the segment's terminal outcome was decided before it started. The flag
is how the guest finds out; the host is what makes it true.

This is why `deferSegmentLanguages` membership is about being *verified to unwind on the
sentinel*: for this SDK the honest claim is narrower, and the end-to-end fixture is what will have
to establish which claim actually holds.

**Checked from both ends**, as §3.105 is:

| test | where | what it holds |
|---|---|---|
| `stop-bit.spec.ts` (5 tests) | as-pect | `stopRequested` fires on bit 31 and on nothing else, and sets the flag |
| `TestTheAssemblyScriptSDKAgreesOnTheStopBit` | Go | the SDK's constant is the engine's, parsed from the hex literal |
| `TestEveryASCallTheHostCanRefuseChecksTheStopBit` | Go | all nine methods guard, **and guard before decoding** |
| `TestTheASSleepPathDoesNotCheckTheStopBit` | Go | `cleatSleepMs` does not, with the reachability reason |

The as-pect tests deliberately do not drive `HostCalls` methods: that harness stubs every
`@external("env", ...)` import, so such a test would be measuring the stub's return value. The
existing `defer.spec.ts` says the same thing about `deferFunc`. The structural guarantee is held
from the Go side instead.

**Not done here, exactly as in §3.105:** `assemblyscript` is not added to
`deferSegmentLanguages`. The end-to-end proof — a fixture with a defer, run as a segment — is the
next piece, and until it lands the fence stays closed and AssemblyScript behaves as it does today.

Remaining: **Rust**, unblocked by §3.87 (#643) and different in shape again —
`Err(CallError::Suspended)` returned before any field is decoded, rather than a flag or an
unwind. **Python** is still blocked on an ABI decision and not on effort: it is a Component Model
guest whose WIT declares `durable-call: func(...) -> string`, so there is no result word to carry
bit 31, and `extractStringFromPacked` turns the sentinel into `""` — an empty *successful*
response. Measured 2026-09-03: `extractStringFromPacked(0x80000000, buf) == ""`.
### 3.107 The Rust SDK decodes the defer-segment stop sentinel — 🔶 **SDK HALF DONE; the end-to-end proof and `deferSegmentLanguages` remain** (WS-2, 2026-09-03)

The third of the four SDK halves (§3.105 Java, §3.106 AssemblyScript). Same eight-or-nine
methods, same ordering contract, same forbidden sleep path.

**This one was blocked on §3.87 (#643, WS-3) and is the better for it.** Before that landed, Rust
suspended by `panic_any` + `catch_unwind` on a target that builds `panic=abort` — so decoding a
stop would have had nothing to unwind into. #643 replaced the panic with
`CallError::{Suspended, Failed}` and `?`-propagation, and that makes this the **strongest** of the
three shapes:

| SDK | how a stop leaves the call | if the body ignores it |
|---|---|---|
| Go | `panic(cleat.ErrSuspend)` | cannot — the panic unwinds |
| Java | `throw SuspendSignal` | cannot — the throw unwinds |
| **Rust** | `Err(CallError::Suspended)` via `suspend()` | **backstop still ends the segment** |
| AssemblyScript | a flag plus an error result | keeps running (§3.106) |

The Rust row matters because **six of the eight guarded functions return
`(String, Option<String>)` rather than `Result`** — `cleat_call`, `child_workflow`,
`child_workflow_with_options`, `plugin_call`, `plugin_call_streaming`, `cleat_call_heartbeat` — so
a workflow body can discard the error half. `stop_requested` therefore routes through the existing
`suspend()` helper rather than constructing `CallError::Suspended` itself, because `suspend()`
also sets the `#[cleat_entry]` backstop, and that backstop is what still ends the segment.
`TestTheRustStopGoesThroughSuspend` asserts the routing rather than trusting the comment.

**Eight, not nine:** this SDK has no `call_with_retry`.

**Checked from both ends**, as the other two are: five `cargo test` cases that `stop_requested`
fires on bit 31 and on nothing else and marks the segment suspending, and four Go tests reading
the Rust source — the constant matches the engine's, all eight functions guard **and guard before
decoding**, `cleat_sleep_ms` does not, and `stop_requested` goes through `suspend()`.

The Rust unit tests deliberately do not drive the `HostCalls` methods: those call `extern "C"`
imports that exist only inside a cleat WASM runtime.

**Not done here, exactly as in §3.105 and §3.106:** `rust` is not added to
`deferSegmentLanguages`. The end-to-end proof is the next piece, and until it lands the fence
stays closed.

**Remaining after this: Python only, and it is blocked on a decision rather than on effort.** It
is a Component Model guest whose WIT declares `durable-call: func(...) -> string`, so there is no
result word to carry bit 31, and `extractStringFromPacked` turns the sentinel into `""` — an empty
*successful* response. Measured 2026-09-03: `extractStringFromPacked(0x80000000, buf) == ""`. The
two options are a sentinel prefix in the string (matching the existing `"__CLEAT_ERROR__:"`
convention, but re-opening exactly the collision §3.83 exists to record) or changing the WIT so a
stop is unrepresentable as a response. The second is the right one and is a public-API change.
