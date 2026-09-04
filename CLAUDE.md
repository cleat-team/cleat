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

**An unset DSN skips its dialect silently and the suite still prints `ok`.** Measured 2026-09-03,
`go test ./engine/` on one tree, each configuration run twice in opposite orders:

| `CLEAT_TEST_*` set | passed | skipped | wall (two runs) |
|---|---|---|---|
| none | 3462 | **876** | 158s, 148s |
| postgres only | 3813 | **581** | 182s, 163s |
| all three | 4510 | **4** | 206s, 222s |

All three printed `ok`. **Check the skipped count, not the clock:**

    go test ./engine/ -count=1 -json > /tmp/t.json
    grep '"Action":"skip"' /tmp/t.json | grep -c '"Test":'   # 4 means all three dialects ran
    grep '"Action":"fail"' /tmp/t.json | grep -c '"Test":'   # must be 0 -- see two paragraphs down

That column is exact, reproducible, and machine-independent: both orderings gave an identical
876 / 581 / 4.

**The wall-clock check this file used to recommend is dead — do not use it.** It read "roughly 20s
means Postgres only, roughly 60s means all three", off a 2026-08-06 measurement of 16s and 60s.
A no-DSN run now takes about 150s, so a run that tested *no database at all* clears the old
"all three" threshold by 2.5× — the exact false green this section exists to prevent. And the gap
between adjacent configurations (~19s from none to postgres-only) is now the same size as
run-to-run variance on one configuration (postgres-only measured 182s and 163s), so the clock
cannot separate them even in principle. The suite grew about 8% in test functions over that
month; the wall times grew tenfold, most of it machine and load.

**A DSN that is set but does not connect looks exactly like one that works.** Setting the variable
is what stops a test skipping. Connecting is a separate question, and neither the skipped count
nor the clock asks it — those tests fail on connect instead of skipping. Writing this section I
reconstructed all three DSNs from memory, got the database name and two passwords wrong, and
measured a tidy 876 → 581 → 4 progression whose final `4` matched the old table exactly. It was
1086 connection failures wearing the right costume, and the matching `4` read as corroboration.
**So count failures too**, or probe first:

    go test ./engine/ -run TestTenantIsolationAcrossDialects -count=1

The DSNs are written down: `WS3-STATUS.md` for this checkout's ports, `PARALLEL-WORKSTREAMS.md`
for the defaults. Read them rather than rebuilding them from memory.

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
total count, not on the absence of pending.

**But do not hardcode that total, because it is path-dependent.** Measured 2026-09-01, after
every check had settled:

| PR | touches | checks |
|---|---|---|
| #504 | `CLAUDE.md` only | 46 |
| #500 | docs + `engine/` | 46 |
| #503 | `engine/` + `cmd/` | **50** |

The four `{AssemblyScript,Java,Python,Rust} SDK Integration` jobs are the whole of the gap; they
trigger only on some paths. `Benchmarks` and `Coverage` report `skipping` on a normal PR, so a
green run shows two fewer passing than the total. Re-derive with
`gh pr checks <pr> | grep -c .`, and diff two PRs with `comm -23` over the sorted name column to
see which jobs a path triggers.

**"After every check had settled" is load-bearing, and I got it wrong writing this.** The first
draft of the table above said #504 ran 45, because I ran `grep -c .` 25 seconds after pushing —
before the 46th check had been registered. A total sampled while checks are still being created is
itself a "checks never started" reading, and it is the more dangerous kind, because it looks like
a settled fact rather than a pending state. If you are recording a total, take it from a PR whose
checks have all finished, not from one you are still watching.

A fixed floor is therefore weaker than it looks: gate at 46 and a PR that should run 50 passes the
moment its 46th check settles, with four SDK jobs not yet queued — which is precisely the
"checks never started" case this paragraph is about. **The reliable arbiter is the branch policy,
not a count.** `gh pr merge` refuses while required checks are outstanding, and
`gh pr view <pr> --json mergeStateStatus` reports `BLOCKED` rather than `CLEAN`. On
2026-08-31 that refusal was the only thing that caught a watcher reporting green over six
pending checks.

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

**A merge's own `develop` run could be cancelled by the next merge** landing seconds later, and
`cancelled` is not `success`. Verifying `develop` after merging means verifying the *current
head*, which contains your commit — not your own SHA.

That cancellation was fixed in two halves — #634 scoped `cancel-in-progress` to pull requests,
#661 gave each push its own concurrency group — so it should no longer happen. The reading skill
outlives the defect, and it is one line:

    gh run view <run-id> --json jobs --jq '.jobs | length'

**A `cancelled` run with zero jobs never started.** It was evicted from a concurrency queue
before any runner picked it up; a run killed mid-flight has jobs, each with a `cancelled`
conclusion. The two look identical in `gh run list` and have completely different causes, and
telling them apart is what separated the two halves above: of 22 cancelled `Tier 1 Gate` runs on
2026-09-03, the 17 before #634 had jobs (one exception) and all 5 after it had none. Measured
2026-09-04; see IMPROVEMENT-PLAN §3.100.

**Read it as zero versus non-zero, never as a count.** The number grows while a run proceeds, so
it is only final once the run is. The same run sampled twenty minutes apart gave `1` and then `3`
here, and the `1` went into a table before this sentence was written.

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

**The sharpest form is a test whose NAME asserts the mechanism.**
`TestFinalizeDeferPhaseIsFencedOnTheClaimAndOnTheMarker` (§3.112) stayed green with the marker
predicate deleted. What refused the repeated finalize it pointed at was the finalize clearing
`assigned_to`, so the ordinary fence no longer matched — the predicate in the name had nothing to
do with it. Three cases in one test, three different things doing the refusing, and the name
attributed all three to one.

**And when a falsification comes back green, the obvious repair is usually the wrong one.** "It
did not go red, so the line is dead code" would have deleted a predicate that has a real case:
a claimed workflow that owes no defer phase, where the fence *is* satisfied and only the marker
stops a `status = NULL` write over a running workflow. Once that case was written, the same
falsification failed — with a NOT NULL violation where `ErrFenceLost` was expected, which is a
refusal by database constraint rather than by the code under test. **A falsification that stays
green is telling you which case you did not write, not which line to remove.**

**When you fix something, fix the prose that describes it — not just the status marker.** A ✅ on
a heading over a stale body is worse than no marker at all, because it stops the next reader from
checking. Four separate sessions were lost to one sentence describing a build tag that had
already been removed; three of them concluded that a working feature was broken.

**Any number you write down carries a date and the command that re-derives it.** If you cannot
write the command, do not write the number. Every count in this repo's docs was wrong when
checked — linter totals, finding counts, skip counts, branch counts, all of them.

**And the command has to answer the question you think it does.** A command that runs clean is
not a command that is right. `git log --date=iso` prints a *local* time with an offset —
`2026-09-03 14:20:48 -0400` — and pasting that clock reading into a UTC comparison moves the
window four hours:

    gh run list ... --jq '[.[] | select(.createdAt > "2026-09-03T14:20:48Z")]'   # wrong by 4h

`gh` compares strings, so nothing errors; it silently answers about a different window. That one
inflated a measured count from 5-of-24 to 11-of-36 and the conclusion from "a fifth" to "roughly
a third", in the direction that flattered the finding — which is why it was not questioned. Use
`%cI`, which carries the offset, and convert:

    git log -1 --format=%cI <sha>                    # 2026-09-03T14:20:48-04:00
    python3 -c "import datetime,sys; print(datetime.datetime.fromisoformat(sys.argv[1])
      .astimezone(datetime.timezone.utc).strftime('%Y-%m-%dT%H:%M:%SZ'))" "$(git log -1 --format=%cI <sha>)"

**What caught it was checking the story, not the number.** The mechanism being claimed — eviction
from a queue — can only produce runs with zero jobs, and six of the eleven had one job. A number
that supports your conclusion is the one to re-derive, not the one to keep.

**And `grep` in an interactive shell here is not the `grep` your script gets.** It is a shell
function wrapping **ugrep 7.5.0**; a script with `#!/usr/bin/env bash` gets `/usr/bin/grep` (BSD),
and CI gets GNU grep. Confirm with `type grep`, which reports the function, then `grep --version`.

Two independent divergences, both measured 2026-09-04, same directory and same `LANG`:

**1. `-c` combined with `-o` means different things.** ugrep counts *matches*, BSD grep counts
*lines*. On `§[0-9]+\.[0-9]+` over `IMPROVEMENT-PLAN.md`:

| | ugrep | BSD |
|---|---|---|
| `grep -oE ... \| wc -l` | 819 | 819 |
| `grep -cE ...` | 731 | 731 |
| `grep -coE ...` | **819** | **731** |

The pattern is not the problem and neither tool is wrong; `-co` is simply underspecified. Write
`-o \| wc -l` when you mean matches and `-c` when you mean lines, and never combine them. No
tracked script or doc currently does (`grep -rn 'grep -[a-z]*c[a-z]*o\b' --include='*.sh'`).

**2. A multibyte character inside a bracket expression parses differently.** On
`IMPROVEMENT[- ]PLAN[^§0-9]{0,3}§?[0-9]+\.[0-9]+` over `--include='*.go'`: ugrep **399**,
`/usr/bin/grep` **1379**, Python `re` **1379**. The interactive tool is the outlier, which is the
wrong way round — every number derived by hand is measured with the one that disagrees.

Plain-ASCII patterns were checked rather than assumed, and agree: `^### [0-9]+\.[0-9]+ `,
`^\| [0-9]+\.[0-9]+ \|`, and a `✅|FIXED|DONE` alternation all return identical counts under both.

So the rule above — write the command that re-derives the number — needs one more clause: **run it
the way the reader will run it.** For any pattern with non-ASCII or `{n,m}`, check it under
`bash -c '...'` before writing the number down, or compute it in Python, whose `re` is the same
everywhere. A guard that greps for something exotic should not be a shell script at all.

This surfaced as a script reporting 1627 citations where the identical pipeline pasted into the
terminal reported 546 — and neither was right. A survey in Python found 2354, because the pattern
missed four of the six forms a citation actually takes. Three tools, three answers, one command.

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

- `cleat/wasmtest`, `cmd/cleat run_embedded`, `cmd/cleatctl replay`, `cmd/cleatctl debug`,
  `cmd/cleat-bench` — all call `engine.NewRuntime`. **Dev and CLI tooling, all of it.**
- `RunDefer`, but only when the engine registers *no backends at all* — which is those same
  tools. The worker calls `RunDefer` too (`cmd/cleat-worker/setup.go:480`) and never reaches
  this path, because it registers wasmtime.
- `RunDeferCompiled`, which is wazero by signature — it takes a `wazero.CompiledModule`, so no
  backend can serve it. **It currently has no callers**, and is listed in
  `scripts/deadexports-baseline.txt`.

Re-derive: `grep -rn "NewRuntime(" --include="*.go" . | grep -v _test.go`

**This section used to describe the `RunDefer` bullet as an unfenced hazard, quoting a comment
that said "the CGO-less build, where wazero is the only runtime there is. Unfenced, and
unavoidably so" at `engine/executor.go:706`.** Every part of that was wrong by 2026-09-01: the
comment had already been rewritten in the tree, line 706 is now unrelated `continue_as_new`
code, and #503 closed the case that made it dangerous. Confirm the quote is gone with
`grep -n 'CGO-less build, where wazero is the only runtime' engine/executor.go`.

What made it dangerous was **guest-controlled**: `wasm.DetectLanguage` returns the guest's own
`cleat.metadata` Language field verbatim, so a module declaring `"tinygo"` — or `"GO"`, since
the lookup is exact — matched no backend and fell through to wazero. `Engine.resolveBackend`
now fails closed on exactly that, distinguishing "this engine does no routing" from "this
engine routes but not for this language". Read its doc comment for the measurements.

So "wazero is gone" is still wrong, but the reason has changed. **wazero cannot be fenced for a
compute-bound guest** — measured three ways, all failing: `WithCloseOnContextDone` breaks all
execution, fuel only decrements on function entry, and closing the module has no effect on a
tight loop. That now bounds *developer tooling*, not anything a worker runs: a runaway guest
under `cleatctl replay` or `cleat run` is not stopped.

**"wazero removal, part 2" was decided against on 2026-09-01 — do not start it.** See
IMPROVEMENT-PLAN.md §3.56. The safety case was gone once #503 made routing fail closed, and
removal would force `cleat` onto CGO (ending pure-Go cross-compilation for the CLI) while
breaking exported API — `engine.Runtime`, `engine.NewRuntime`, `wasmtest.WasmTestEnv.Runtime()`.
The parked WIP stash and the `REMEDIATION-PLAN-2026-08-09.md` section describing it are
superseded.

The price of keeping two implementations is that something must compare them, because the host
ABI is written twice — `engine/imports.go` for wazero, `engine/wasmtime_hostfuncs*.go` for
wasmtime. `engine/hostabi_runtime_parity_test.go` does. It found a real defect on its first run:
`cleat_create_promise` was registered on wasmtime with a parameter no guest passed, so durable
promises could not link on the worker at all (§3.55). **Note what a name-only comparison would
have said** — both sides register the same 56 names, and did then too.

**"Which backend runs this" and "which code path inside that backend runs this" are different
questions.** The wasmtime backend has **two** execution paths — core module and native
component — and they had different answers about limits until IMPROVEMENT-PLAN §3.31 wrote the
story down for each. Tell the limit story about the second question, not the first.

There were three. Decomposition was deleted in #528 (2026-09-01) after being measured against
the only Component Model binary in the repo: the native path reached CPython and ran guest
code, while decomposition failed at instance 81 of 85. A second, mirror implementation on
wazero failed at instance 8. Confirm with `grep -rn "func.*ExecuteComponent" --include="*.go" .`
— exactly one line, `ExecuteComponentCGo`.

**Two things went wrong writing that one-line command, both worth the warning.** The first
version was `grep -rn "ExecuteComponent\b"`, which returns a hit: a comment in
`engine/wasmtime_options.go` explaining that the function it names was deleted. A grep a
*retraction* satisfies is the §1.1 trap, in a file that documents the §1.1 trap. The second was
`func.*ExecuteComponent(` — anchoring on the open paren, which does not follow the name in
`ExecuteComponentCGo(`, so it matched nothing at all and the "only X should match" claim beside
it was false in the other direction. **Run the command and read its output before writing the
sentence about what it prints.**

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

  **A heading with *no* marker is a defect, and it costs more than a wrong one.** §1.1 and §1.2
  — the two highest-severity items in the document — carried no marker until 2026-09-01 while
  their bodies had recorded the fixes as done for weeks. A scan for open work reported a
  closed data-loss bug as the project's top outstanding item, and a session went into
  re-deriving what the body already said. "No marker" is indistinguishable from "not started",
  so it is read as the latter. Prose in the heading (`— fixed in 9fc2a81`) counts; nothing at
  all does not. Measured 2026-09-01: 87 of 99 headings carried a status, and re-derivable with

      grep -cE '^### [0-9]+\.[0-9]+ ' IMPROVEMENT-PLAN.md

  **When a section names the files it will change, those names go stale too, and in the
  direction that fools you.** §1.1's `Files:` bullet pointed at
  `migrations/*/003_procedures.sql`. The fix shipped as `004_fix_finalize_workflow_status_fence.sql`,
  which *redefines* the procedure — so 003 still contains the original unguarded body, exactly
  as the bug report described. Checking the claim against the file the claim named confirmed
  the bug, and the confirmation was worthless. For anything defined by `CREATE OR REPLACE`,
  find the highest-numbered migration that defines it before concluding anything.
- **`WORKSTREAM.md`** — what is being worked on now, by whom, and in which sandbox.
- **`BRANCH-TRIAGE.md`** — assessment of unmerged remote branches. Its method
  (`git rev-list --left-right --count`) cannot see through a squash-merge, so it has reported
  already-merged branches as outstanding. Re-derive with
  `git merge-base --is-ancestor <PR-merge-commit> develop` before acting on it.
