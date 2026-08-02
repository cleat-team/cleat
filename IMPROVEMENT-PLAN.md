# Improvement Plan — seam hardening

**Generated:** 2026-08-02 · **`develop` @ `a2b220c`**

Derived from a nine-agent adversarial review. The finding that organises this plan:

> Unit-level quality is high. Architecture is sound. **Every serious defect is at a seam** —
> component to component, config to tested-config, claim to verification. And they persisted
> because the signal that would have reported them was itself broken.

So the ordering principle is: **restore the signal, then pair every fix with a test at the
layer that would have caught it.** Not "fix everything, then add tests." The whole lesson of
this codebase is that unit tests passed while the feature was dead.

Effort is given in solo+AI sessions (a session ≈ half a day of your attention).

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

### 1.4 Crash-recovery machinery has zero callers (~2–3 sessions)

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
| 2.8 | **Dead-code detector.** Static check: functions with test callers but zero production callers, failing the build on new instances. | 1.4 class — the single highest-signal cheap check |
| 2.9 | **Doc/code consistency.** Assert `ABI.md` version == `wasm/metadata.go:47 CurrentABIVersion`; documented worker flags exist in the binary; documented buffer sizes match `engine/memory.go:39`. | ABI.md claiming v4/5 while code ships v1; the 65536-vs-1048576 buffer mismatch |

Note on 2.2–2.6: these need real databases and process control, so they belong in a
nightly/pre-merge job, not the fast unit lane. Accept the runtime. They are the only tests
that would have caught anything found today.

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
