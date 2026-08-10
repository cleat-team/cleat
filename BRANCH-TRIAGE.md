# Branch Triage — remaining feature branches

**Generated:** 2026-08-02 · **`develop` @ `a2b220c`** · 47 non-default remote branches

This document exists so a future session can decide what to merge without re-deriving
the analysis. It describes state and risk; it does **not** recommend merging anything
without a human deciding the work is still wanted.

A prior cleanup deleted 68 branches whose content was already in `develop` (verified by
tree-equality or patch-id). Everything listed here has **genuinely unmerged commits**.

---

## 1. Read this first: the structural cliff

Commit **`3eeb74e` (2026-06-01)** — *"refactor: promote internal packages to public —
engine as a library"* — relocated entire package trees:

| Old path | New path |
|---|---|
| `internal/host/` | `engine/` |
| `internal/wasm/` | `wasm/` |
| `internal/plugin/` | `plugin/` |
| `internal/wasmrw/` | `wasmrw/` |

`internal/` still exists but now holds only `analyzer`, `callgraph`, `closure`,
`plugingen`, `telemetry`, `transform`.

**25 of 47 branches predate this commit.** Sixteen of them modify files under the moved
paths. For those, "merge" is the wrong verb — a merge will resurrect the old directory
tree alongside the new one, and the result will compile only by accident. They need their
change **re-applied by hand** against the new layout, or `git merge` with explicit rename
detection and careful review.

`develop` is 118 commits past the refactor. Every branch below is tagged **PRE** or
**POST** accordingly. This is the single most important field in the tables.

> ⚠️ `CLAUDE.md` is stale on this point. Line 8 describes `internal/` as containing
> "host, wasm, plugin", and line 29 points at `internal/plugin/plugin.go`. Both were
> true before `3eeb74e`. Worth fixing regardless of what happens to these branches.

---

## 2. Tier 1 — Live work

### ~~`fix/wasm-build-replace-propagation`~~ — **PR #208 CLOSED unmerged, branch deleted** (2026-08-03)

> **Superseded. The assessment below was wrong and is kept only as a record of how.**
> "Only 3 behind" was inferred from commit counts; by the time it was acted on the branch
> was 19 ahead / 9 behind and `CONFLICTING`, and its headline fix — replace-directive
> propagation — had already landed on `develop` verbatim via `c26c332`
> (`git diff develop...df1119a -- wasm/build.go` is empty). See the salvage register at the
> end of Phase 2 in `IMPROVEMENT-PLAN.md` for what was worth keeping, what was dropped, and
> the head SHA `df1119a` if any of it needs recovering.

19 commits · 49 files · +2304/−87 · only 3 behind

The only open PR and by far the most current branch. Bundles several distinct things:
`go.mod` replace-directive propagation for WASM builds, an admin API backend, a
`--latency-histogram-buckets` flag, `--version` on `cleat build`, and a
`StartWorkflow` plugin-capability check.

Note the history is noisy — three `chore: trigger CI re-run` commits and a `Merge
branch 'develop'`. Several commits also revert earlier ones in the same branch
("revert unintended changes to main.go"). **Squash-merge; do not preserve this history.**

Because it is only 3 commits behind, this is the one branch that can be merged
essentially as-is. Do this one first — it reduces the delta for everything else.

---

## 3. Tier 2 — Post-refactor, small, mergeable

Low risk. Modest conflict surface. Each is a self-contained fix.

| Branch | Date | Behind | Size | What it does |
|---|---|---|---|---|
| `feature/benchmark-round3` | 06-22 | 10 | 3f +144 | Dispatch loop drains in-flight workflows on shutdown; flusher closed after engine shutdown rather than in the signal handler; retry transient DB errors with backoff |
| `feature/benchmark-round2` | 06-22 | 11 | 4f +45 | Dedicated flusher pool, xxhash checksums, pool tuning; demotes hot-path `Info` logs to `Debug` |
| `feature/ci-multi-db-fixes` | 06-09 | 27 | 8f +126 | MySQL/MSSQL CI green — restores a `tenant_id` filter on `GetWorkflowByID`, converts `test-mssql` to a service container to dodge an MCR pull block |
| `feature/data-boundary` | 06-03 | 113 | 4f +19 | Qualifies admin schema tables, adds a `generation` column migration, adds a wasmtime stub |
| `bugfix/missing_pg_isready_user` | 06-05 | 123 | 1f +1/−1 | One-line `docker-compose.dev.yml` fix — use default postgres user for `pg_isready`. PRE-refactor but touches no moved path, so it is safe |

**`feature/ci-multi-db-fixes` deserves priority** — if CI is currently red on MySQL/MSSQL,
this is the branch that fixes it, and it gates confident merging of everything else.

---

## 4. Tier 3 — The test-coverage fleet (POST-refactor)

Fifteen branches of pure test additions, nearly all `engine/*_test.go`, produced by what
looks like a parallel coverage push in early June. Almost all are additive (`−0` deletions),
so they conflict with `develop` rarely but **with each other constantly** — many write the
same test files.

**Do not merge these one at a time.** Cherry-pick the union into a single branch, resolve
duplicate test-function names once, and land it as one commit.

### Verified containment (checked file-by-file, not inferred from commit titles)

- `feature/cov-small-008c` ⊂ **`feature/cov-small-007`** — 0 files differ. Drop 008c.
- `cleat-241-wasm-file-gaps` ⊂ **`cleat-004-small-files`** — 0 files differ. Drop the former.
- `feature/engine-zero-coverage-tests` is **NOT** a subset of `cov-small-007` despite a
  near-identical commit subject — 3 files genuinely differ. Both must be examined.

### The fleet

| Branch | Behind | Size | Target |
|---|---|---|---|
| `feature/wasm-cache-tests` | 44 | 4f +3127 | `runtime.go`, `imports.go`, WASM LRU cache |
| `feature/cleat-240-mysqli` | 82 | 6f +1744 | MySQL + MSSQL backends |
| `feature/cleat-240-cli-coveragep` | 75 | 1f +1516 | `cmd/cleat-plugin-verify` |
| `feature/cov-mid-003-wasmtime` | 46 | 2f +1212 | wasmtime backend, `ContinueAsNew` |
| `feature/engine-zero-coverage-tests` | 40 | 3f +1205 | `app.go`, `wasm_cache.go`, `wasm_disk_cache.go` |
| `feature/cov-store-001` | 44 | 3f +1123 | `WasmDiskCache`, MySQL/MSSQL stores |
| `feature/cov-mid-004-query-builder` | 45 | 5f +1099 | `readonlydb`, `plugindb_adapter`, `db_metrics`, QueryBuilder |
| `feature/cov-small-007` | 45 | 6f +1001 | `app.go`, wazero backend, version-management error paths |
| `feature/cov-small-008c` | 45 | 3f +672 | ⊂ cov-small-007 — **drop** |
| `cleat-004-small-files` | 62 | 5f +571 | WorkflowLoader mock-DB, `cleatDispatch` unwrap fix |
| `feature/cleat-010` | 37 | 1f +357 | `SignalWorkflow`, `DurableScheduleInvoke`, `RegisterUpdateHandler` |
| `feature/cleat-240-small-files-zero` | 115 | 5f +301 | Small zero-coverage engine files |
| `feature/cov-engine-002c` | 39 | 1f +256 | `AwaitAnyChild` |
| `cleat-241-wasm-file-gaps` | 63 | 4f +196 | ⊂ cleat-004-small-files — **drop** |
| `feature/cleat-240-worker-cli-cleatk` | 78 | 3f +246/−799 | Worker metrics, `checkdb`. Note the −799: this one **deletes** substantially, unlike the rest of the fleet. Review before assuming it is purely additive |

`cleat-004-small-files` also carries a **non-test fix** — `fix(wasm): unwrap
DispatchWrapper inputJSON in cleatDispatch generated code`. Don't lose it in a
test-only triage. (A same-titled fix appears on PR #208; check whether they are the
same change before applying both.)

---

## 5. Tier 4 — Large post-refactor branches needing real review

### `feature/review-quality-fixes` · POST · 06-09 · 12 commits · 261 files · +11370/−8644
The biggest mergeable item. Contains a genuine architectural change — **splits `engine.go`
into 14 focused files** — plus `interface{}` → `any` across the whole tree, `doc.go` for
10 packages, a CHANGELOG, a troubleshooting guide, Mermaid diagrams, and enables
`golangci-lint` in CI.

The refactor and the docs are independent. **Consider splitting**: the docs/CHANGELOG/lint
commits are near-zero-risk and could land immediately; the `engine.go` split and the
tree-wide `any` migration need real review and will conflict with anything else touching
`engine/`. Only 25 behind, so it is still tractable — but it gets worse with every merge
that touches `engine/`. **If you want this, take it early.**

### `feature/coverage-thresholds` · POST · 06-10 · 2 commits · 16 files · +3828
~200 new tests plus a `Makefile` change raising thresholds (engine 60, wasm 75, plugin 60,
auth 90). The threshold bump will **fail CI unless the coverage work from Tier 3 lands
first**. Sequence it after the fleet, not before.

---

## 6. Tier 5 — Pre-refactor, needs re-application

These carry real work, but against `internal/host/` and friends. Treat each as "port the
change", not "merge the branch". Ordered by apparent value.

| Branch | Date | Ahead | What it does | Note |
|---|---|---|---|---|
| `feature/sdk-improvements` | 05-22 | 16 | Python Component Model on the wasmtime backend — WIT-to-env import rewriting, `__wasm_call_ctors`, multi-pass instantiation, per-export GOT routing | Substantial and specialised. Touches `internal/wasm` (6 files). Almost certainly the hardest to port, and the hardest to reproduce if lost |
| `feature/competitive-gaps` | 05-16 | 27 | OTel span hierarchy, per-tenant rate limiting, MSSQL RLS policy, configurable WASM size limits, `cleatctl replay`, queue-depth/latency metrics, cross-language replay determinism | A whole feature program on one branch, with `develop` merges baked in. 210 behind. Realistically: mine for individual features, do not merge |
| `pr/parent-child-atomic-wake` | 05-26 | 2 | **Atomic parent wake on child completion for reliable `AwaitChild`**, plus a DAG `AddWorkflow` method | Small and sounds like a genuine correctness fix. Verify whether the bug still exists in `engine/` before porting — it may have been fixed independently |
| `feature/worker-busy` | 05-26 | 3 | Worker CPU busy-loop mitigation across 10 sites; watchdog self-monitoring, TOCTOU fixes, poison-pill handling; OTel + slog | Note `develop` already has "convert remaining log.Printf to structured slog (#237)", so the logging half may be redundant |
| `fix/cleat-220-short-gates` | 05-18 | 9 | DB performance indexes, `query_state` in `ListWorkflows`, `tenant_id` filtering on MSSQL methods missing tenant scoping, `testing.Short()` gates | The missing-tenant-scoping fixes are a **potential data-isolation issue** — worth checking against current `engine/` even if the branch is abandoned |
| `feature/wasm-multi-language` | 05-24 | 6 | Java/TeaVM and AssemblyScript on wasmtime; Rust → `wasm32-unknown-unknown`; multi-DB migration fixes | Overlaps `feature/sdk-improvements` in intent |
| `feature/single-tenant-neon` | 05-16 | 3 | Single-tenant mode for managed PostgreSQL (Neon) | Self-contained feature; port if the use case is still live |
| `feature/cleat-218` | 05-16 | 2 | Tenant isolation tests for reaper and concurrency keys; PK constraint fixes | Tests only |
| `feature/cleat-214-fix-mssql-tenant-id` | 05-16 | 2 | `tenant_id` in MSSQL `StartNewRun` INSERT | Small correctness fix; check if already fixed |
| `fix/clew-executor-contract-and-timeout` | 05-16 | 3 | clew-executor contract, removes subprocess timeout, phase-based role selection | Only touches `plugins/clewexecutor` — **no moved paths**, so this one may merge more easily than its PRE tag suggests |
| `feature/cleat-217-doc-worker-config-flags` | 05-16 | 1 | Documents 20 undocumented worker flags, removes 2 phantom ones | Docs only, but the flag list is 208 commits stale — **re-derive rather than merge** |
| `fix/semantic-pr-title-token` | 05-16 | 2 | CI: allow `fix/` branch prefix, `github.token` over `secrets.GITHUB_TOKEN` | Check whether CI already does this |
| `dogfood-20260525` | 05-25 | 4 | Race-safe backend execution, mutex protection for concurrent `execSession` map access | The race fixes may matter; `−4820` lines is mostly file removal |
| `dogfood-20260522` | 05-23 | 2 | `validatePhaseOutputs` in executor | |
| `feature/cleat-004c` | 06-08 | 2 | 600 files, +34730/−8024 — one real commit (`component_cgo.go` tests) plus a **"Merge develop into main"** commit that dragged in everything | Do not merge. Cherry-pick the single test commit if wanted |

---

## 7. The `clew-133` family — resolve before touching

Three overlapping branches building the same `plugins/clewservice` HTTP plugin. All PRE-refactor.

| Branch | Date | Ahead | Files | Size |
|---|---|---|---|---|
| `clew-133b` | 05-23 | 3 | 13 | +1728 |
| `clew-133f` | 05-24 | 6 | 19 | +1339/−230 |
| `clew-133c` | **05-27** | 11 | 15 | +2803 |

`clew-133f`'s commit message claims it *"canonicalize[s] clew-service plugin — merge
clew-133c + clew-141a"*, which reads as though `f` supersedes the others. **That claim does
not hold up.** Checked file-by-file: 12 files differ between `133b` and `133f`, and 13
between `133c` and `133f`. Furthermore `133c` is the **newest** of the three (05-27, three
days after `133f`) and has the most commits, including security-relevant work — path
traversal hardening, a poll mutex, TOCTOU fixes.

A human who knows the Clew project needs to decide which lineage is authoritative. Do not
assume the commit message. `clew-133c` also touches `internal/wasmrw/wasmrw.go`, which
moved to `wasmrw/` in the refactor.

---

## 8. The `cleat-216` triplet — three variants, one fix

Three branches, one commit each, all titled identically:
`fix(host): add tenant scoping to GetActiveInstanceCountsByVersion (cleat-216)`

| Branch | Files | Size |
|---|---|---|
| `cleat-216-tenant-active-counts` | 4 | +201/−4 |
| `cleat-216-tenant-scope-active-instance-counts` | 4 | +195/−3 |
| `feat/cleat-216-tenant-active-instance-counts` | 3 | +173/−4 |

**All three have different trees and different patch-ids** — they are not copies. They
differ mainly in `tenant_isolation_test.go` (~162 lines differ between the first two) and
in whether they include a `projects/cleat/cleat-216/artifacts/implementation.md`.

The underlying function `GetActiveInstanceCountsByVersion` no longer exists at the path
these branches modify. **Recommendation:** discard all three, check whether current
`engine/` tenant-scopes that call correctly, and if not write the fix once against the new
layout. Salvage the best of the three test files if useful.

---

## 9. Two branches flagged as ambiguous

Both had every commit subject match something already in `develop`, but **failed a
patch-id check** — meaning a similarly-titled commit landed from a different branch with
different content. They were deliberately excluded from the earlier deletion.

- **`feature/cleat-219`** — `fix: replace == sql.ErrNoRows with errors.Is for
  error-wrapping safety`, 30 files across `internal/host` and 5 plugins. A same-titled fix
  is in `develop`, but coverage may differ. Worth grepping current code for surviving
  `== sql.ErrNoRows` comparisons.
- **`fix/go-wasm-pointer-and-childworkflow`** — `fix: allow empty parentClosePolicy in
  ChildWorkflowWithOptions`, 1 file, +8/−3. Check whether the current `engine/` accepts an
  empty `parentClosePolicy`.

In both cases the useful action is to verify the *behaviour* in `develop`, not to merge.

---

## 10. Suggested order

1. ~~**`fix/wasm-build-replace-propagation`** (PR #208) — squash-merge. Only 3 behind.~~
   **Done differently: closed unmerged 2026-08-03**, its fix having landed independently.
2. **`feature/ci-multi-db-fixes`** — get CI green so later merges are trustworthy.
3. **`feature/review-quality-fixes`** — decide now; cost grows with every `engine/` change.
4. **Tier 3 coverage fleet** — consolidate into one branch, drop the two proven subsets.
5. **`feature/coverage-thresholds`** — only after step 4, or CI fails.
6. **Tier 2 small fixes** — `benchmark-round2/3`, `data-boundary`, `pg_isready`.
7. **Triage Tier 5 / clew-133 / cleat-216** — human decisions about whether the work is
   still wanted. Port, don't merge.

---

## Appendix — method, and what is *not* established

Every branch here was checked for unmerged content three ways: `git merge-tree` for
tree-level no-ops, commit-subject matching against `develop`, and `git cherry` patch-id
comparison. Containment claims in §4 and §7 were verified by comparing blob hashes
per file — **not** inferred from commit messages, which proved unreliable (see §7).

Not established, and left for a human or a follow-up session:

- **Whether any of this work is still wanted.** This is a state and risk assessment. A
  branch being cleanly mergeable is not an argument that it should be merged. Several
  branches represent abandoned directions.
- **Whether the bugs these branches fix still exist.** Sizeable fixes may have been
  independently re-fixed in the 118 commits since the refactor. Flagged where suspected
  (§6 `pr/parent-child-atomic-wake`, `feature/cleat-214`, §9).
- **Build/test status.** Nothing here was compiled or tested. Sizes and paths only.
- **Authorship and intent.** No branch owners were consulted.

To regenerate:

```sh
git fetch --prune
git for-each-ref --format='%(refname:short)' refs/remotes/origin \
  | grep -Ev '^origin$|^origin/(develop|main)$' | sed 's|^origin/||' \
  | while read b; do
      printf '%s\t%s\n' "$b" "$(git rev-list --left-right --count origin/develop...origin/$b)"
    done
```
