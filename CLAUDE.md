# cleat — Durable Workflow Engine

The core cleat engine: workflow execution, WASM runtime, plugin system, worker daemon, CLI,
admin dashboard.

**This file carries rules that stay true for months.** Volatile facts — what is supported, what
is broken, what the current counts are — live in `tiers.yaml` and `IMPROVEMENT-PLAN.md`, which
are checked. If you find a fact in here that has a date on it, treat the date as part of the
fact. If you find one without a date that turns out to be wrong, fix it in the same PR.

---

## What is supported: `tiers.yaml`

`tiers.yaml` is the source of truth for what this project claims to support. Three tiers:

- **Tier 1 — core.** Must pass. **A skip is a failure.** If it is in tier 1 and it does not run
  green on every dialect and backend named there, that is a release blocker.
- **Tier 2 — frontier.** Must *run*. May fail, against a tracked list of known failures that can
  only shrink without a written justification.
- **Tier 3 — parked.** Not built, not shipped, not claimed. Excluded from the default build or
  deleted.

**Do not claim support in a doc that `tiers.yaml` does not grant.** Every prior attempt to record
status in prose here has rotted within days — `docs/review-status.md` declares the project
production-ready off a stale audit, `README.md` has claimed backend parity that was not true, and
`IMPROVEMENT-PLAN.md` has carried ✅ headings over unfixed prose. The manifest exists so status is
a property CI checks rather than a sentence someone wrote.

---

## Is this result real?

Most expensive recurring failure in this project: **a green result that measured nothing.** Check
these before believing any test outcome.

**An unset DSN skips its dialect silently and the suite still prints `ok`.** Measured 2026-08-06
on the same tree, same command:

| | tests run | skipped | wall |
|---|---|---|---|
| no `CLEAT_TEST_*` set | 2544 | 166 | 16s |
| all three dialects set | 3846 | 4 | 60s |

Both printed `ok`. **Check the wall-clock delta** — roughly 20s means Postgres only, roughly 60s
means all three. A green engine run that took 16 seconds tested no database at all.

**Build and test with CGO on — the default.** `CGO_ENABLED=0` does not skip a check. It swaps
`NewWasmtimeBackend` for the `//go:build !cgo` stub in `engine/backend_wasmtime_stub.go`, which
returns `ErrWasmtimeCGOUnavailable`, so **there is no backend left at all** — `cleat-worker`
logs "wasmtime is the only WASM backend cleat has, there is no fallback" and exits 1
(`cmd/cleat-worker/main.go:790`). **An engine result obtained that way is not evidence about the
engine.** If a genuine toolchain failure forces it, say so in the PR rather than leaving the
reader to assume wasmtime was exercised.

Note what that does *not* do: `CGO_ENABLED=0 go build ./...` still exits 0 (measured
2026-08-30). Nothing tells you at build time; the failure is at worker startup. This paragraph
used to say a CGO-less build "silently runs everything on wazero" — that stopped being true
when the wazero backend was deleted, and it is the opposite of what happens now.

**Use `-p 1` when running more than one database-backed package in one invocation.**
`engine/testutil`'s `CleanupPostgresTestData` is an unqualified `DELETE FROM` across eleven
tables. Run concurrently against one database, packages delete each other's fixtures mid-test and
the failures look like unrelated flakes.

**A skip that hides a crash is not a skip.** `t.Skipf("... crashed")` and
`t.Skipf("... compatibility issue")` are failures wearing a skip's clothing, and they make a
regression invisible forever. A skip is legitimate only for a genuine environmental precondition
(a toolchain that is not installed, a DSN that is not set).

**"No pending checks" also matches "checks never started."** Guard `gh pr checks` wait-loops on a
total count, not on the absence of pending. The repo runs **46** checks (measured 2026-08-31 on
#500; re-derive with `gh pr checks <pr> | grep -c .`), of which `Benchmarks` and `Coverage`
report `skipping` on a normal PR, so 44 is a green run and not a truncated one.

**And parse that output with `awk -F'\t'`, because check names contain spaces.** The total-count
guard above is necessary but not sufficient: it does not help if the *pending* count is itself
silently zero. `gh pr checks` is tab-delimited with names like `Tier 1 Gate` and
`Test Go (engine) on 1.26`, so the natural-looking

    pend=$(echo "$out" | grep -cE '^\S+\s+pending')     # blind to 40 of 46 checks

cannot match them — `^\S+` takes `Tier`, `\s+` takes the space, and the next token is `1`, not
`pending`. Measured 2026-08-31 on #500, **40 of the 46 names contain a space**
(`gh pr checks <pr> | awk -F'\t' '$1 ~ / /' | grep -c .`); the six it can still see are `Build`,
`CodeQL`, `Lint`, `lint-go`, `Benchmarks` and `Coverage`, none of which are the ones that matter.
`Tier 1 Gate` and `Test Go (engine) on 1.26` are both in the blind 40 — so the count does not
degrade, it reads zero for exactly the checks worth waiting on. That reported #500 as green with
six still running; `gh pr merge` refusing with "the base branch policy prohibits the merge" was
the only thing that caught it. Use

    pend=$(echo "$out" | awk -F'\t' '$2=="pending"' | grep -c .)
    fail=$(echo "$out" | awk -F'\t' '$2!="pass" && $2!="pending" && $2!="skipping"' | grep -c .)

The general rule this is an instance of: **a verification script needs its own negative control.**
Before trusting a new watcher, run its parse once against a PR that has known-pending checks and
confirm the count is non-zero. A loop that cannot see the state it looks for does not fail —
it prints a confident green, which is the same failure this file's whole "Is this result real?"
section is about.

**A merge's own `develop` run can be cancelled by the next merge** landing seconds later, and
`cancelled` is not `success`. Verifying `develop` after merging means verifying the *current
head*, which contains your commit — not your own SHA.

**When a schema migration lands, recreate your test databases.** `CREATE TABLE IF NOT EXISTS`
never adds a column, so a long-lived database keeps its old shape and dozens of tests fail on a
missing column. Drop and recreate; do not debug the code.

---

## Ground rules for changes

**Prove every regression test can fail — and read *why* it failed.** Remove the fix, watch it go
red, put it back. This catches a test that *cannot* fail; it does not catch one that fails
*sometimes*. If an assertion depends on wall-clock time, remove the timing rather than widening
it. Twice this has caught a test passing for the wrong reason, which is why "it went red" is not
enough on its own — check that it went red *for the reason you expect*.

**Watch which layer is holding the test up.** An assertion can pass because of a layer other than
the one under test: a fence test passed with its SQL guard deleted because a Go-level rollback
covered for it; a cross-tenant assertion passed against a wide-open security policy because the
store's own SQL carried `tenant_id = ?`. Break the specific layer and watch.

**When you fix something, fix the prose that describes it — not just the status marker.** A ✅ on
a heading over a stale body is worse than no marker at all, because it stops the next reader from
checking. Four separate sessions were lost to one sentence describing a build tag that had
already been removed; three of them concluded that a working feature was broken.

**Any number you write down carries a date and the command that re-derives it.** If you cannot
write the command, do not write the number. Every count in this repo's docs was wrong when
checked — linter totals, finding counts, skip counts, branch counts, all of them.

**One PR, one thing.** Every PR that bundled a second concern was harder to review than the two
would have been apart.

**Never merge on red, and never re-run a failing job hoping it passes.** Read the log. If it is
genuinely infrastructure, say so out loud and re-run *that*.

**Branch prefixes are exactly** `feature/`, `bugfix/`, `fix/`, `docs/`, `release/`, `hotfix/`.
Not `feat/`, not `test/`. `Validate branch name` fails the whole PR, and a PR's head branch
cannot be renamed, so each mistake costs a close-and-reopen.

**Ask whether the answer is a sweep or a mechanism.** A backlog of 200 similar findings is
usually one missing abstraction, not 200 fixes. Four real defects have come out of the ABI
layer's integer-conversion sites and none of them was an overflow — in every case the value meant
the wrong thing on one side of the boundary, which a property test over that boundary would find
faster than reading the remaining sites.

---

## Repo structure

- `cmd/` — CLI entrypoints (cleat, cleatctl, cleat-worker, cleat-bench, cleat-gen,
  cleat-gen-plugin, cleat-plugin-verify, deploy-workflow, wit-rewrite)
- `engine/` — Core engine: workflow execution, host functions, DB backends (~174 files)
- `wasm/` — WASM build, module loading, and codegen
- `wasmrw/` — WASM read/write helpers (small; production code duplicates this inline)
- `plugin/` — Plugin runtime and interface
- `auth/` — Tenant and auth stores
- `pluginapi/` — Public re-exports for external plugin authors
- `internal/` — Non-public support packages (analyzer, callgraph, closure, plugingen,
  telemetry, transform)
- `cleat/` — Public Go API (cleattest, embedded, localdev, wasmtest, ai, backendkit)
- `plugins/` — 21 built-in plugins (llm, slacknotify, pagerdutyalert, scheduler, etc.)
- `web/` — Svelte 5 admin dashboard
- `crates/` — Rust SDK + Java SDK
- `python-sdk/` — Python SDK
- `packages/` — AssemblyScript SDK
- `examples/` — Example workflows
- `tests/` — Integration test suites (cluster, cross-language, integrity, plugin-harness,
  scale, soak, upgrade)
- `benchmarks/` — Go benchmarks + comparative Temporal/DBOS benchmarks

> **Note on paths in older commits and branches.** Commit `3eeb74e` (2026-06-01),
> "promote internal packages to public — engine as a library", moved `internal/host/` →
> `engine/`, `internal/wasm/` → `wasm/`, `internal/plugin/` → `plugin/`, and
> `internal/wasmrw/` → `wasmrw/`. Anything referring to those `internal/` paths predates
> that commit. Branches based before it will not merge cleanly.

---

## Build

- Go 1.25+, module `github.com/cleat-team/cleat`
- WASM workflows are compiled with the standard Go toolchain (`--target go`, default)
- Tests use `go test`, fuzz tests, and behavioral test suites

### One WASM backend, and a wazero runtime that is not it

**wasmtime** (`engine/backend_wasmtime.go`, `//go:build cgo`) is the only `WasmBackend` cleat
has. `engine.WasmtimeLanguages` is the single source of truth for which guest languages run on
it, and membership there means *verified to load and execute*, not *ought to*.

There is no second backend and no fallback. `engine/backend_wazero.go` was **deleted** in #459
(2026-08-10) — this file described it as "the CGO-less fallback" for twenty days after it
stopped existing. Confirm with `ls engine/backend_wazero.go`.

**wazero has not left the tree, though, and the distinction matters.** `engine.Runtime`
(`engine/runtime.go`) is still a wazero runtime, and it still executes guest code on these
paths:

- `RunDefer`, when `backendForWasm` returns nil — its own comment says "the CGO-less build,
  where wazero is the only runtime there is. Unfenced, and unavoidably so"
  (`engine/executor.go:706`)
- `cleat/wasmtest`, `cmd/cleat run_embedded`, `cmd/cleatctl replay`, `cmd/cleatctl debug`,
  `cmd/cleat-bench` — all call `engine.NewRuntime`

Re-derive: `grep -rn "NewRuntime(" --include="*.go" . | grep -v _test.go`

So "wazero is gone" is wrong in the direction that matters: **wazero cannot be fenced for a
compute-bound guest.** Measured three ways, all failing — `WithCloseOnContextDone` breaks all
execution, fuel only decrements on function entry, and closing the module has no effect on a
tight loop. A runaway guest on any of the paths above is not stopped. Removing the rest is
"wazero removal, part 2" in `REMEDIATION-PLAN-2026-08-09.md`, deliberately parked with the WIP
in a stash — read that section before starting it, it records why `go vet` failed and got 85%
of the way there.

**"Which backend runs this" and "which code path inside that backend runs this" are different
questions.** The wasmtime backend has three execution paths — core module, native component, and
decomposition — and they have had three different answers about limits. Tell the limit story
about the second question, not the first.

---

## Plugin development

Fuller guidance lives outside this repo at
`cleat-internal/prompts/plugins-and-apps-guidance.md`. That checkout is not present on
every machine — if it is missing, the conventions below are sufficient to start; do not
spend turns hunting for it.

Key conventions:
- Plugin names are hyphenated: `"slack-notify"`, `"email-notify"`, `"pagerduty-alert"`
- HostCall operations are snake_case: `"send_message"`, `"trigger_incident"`
- Plugins share the main go.mod (no separate go.mod per plugin)
- Study `plugins/slacknotify/` and `plugins/scheduler/` as reference implementations
- The Plugin interface is in `plugin/plugin.go`

---

## Project state

- **`tiers.yaml`** — what is supported, and at what tier. Start here.
- **`IMPROVEMENT-PLAN.md`** — the item backlog. Each `§` heading carries a status marker; the
  marker is the source of truth, not any summary table derived from it. Read the body too — it
  has been stale under a fixed heading more than once.
- **`WORKSTREAM.md`** — what is being worked on now, by whom, and in which sandbox.
- **`BRANCH-TRIAGE.md`** — assessment of unmerged remote branches. Its method
  (`git rev-list --left-right --count`) cannot see through a squash-merge, so it has reported
  already-merged branches as outstanding. Re-derive with
  `git merge-base --is-ancestor <PR-merge-commit> develop` before acting on it.
