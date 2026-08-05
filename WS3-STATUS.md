# WS-3 status

As of 2026-08-06, against `develop` at `3f94018`. Companion to `PARALLEL-WORKSTREAMS.md`
(who owns what) and `IMPROVEMENT-PLAN.md` (what each `§` item is). This file is only the
state of WS-3.

> **Re-verified 2026-08-30 against `develop` at `68c16f6`**, 120 commits later, before
> merging. Every code-anchored claim below was re-checked by running the command rather than
> by reading. One row was falsified in that interval and is marked ⛔ in the board; one line
> number had drifted and is corrected. Everything else held. The `§3.30` falsification is the
> point of doing this: it is a claim about what the project intends, made three weeks before
> the project did the opposite.

**Round 2's theme is execution boundaries: what stops a guest that will not stop.** The
round-1 contents of this file (tenancy, CLI, toolchain — §1.7, §2.43, §2.70–§2.73) are all
closed and are summarised in one line below rather than repeated.

**All three named round-2 items are landed or answered.** What is open is a backlog with a
measured size, two decisions that are the author's rather than mine, and one residual I have
read but not yet run.

---

## The board

| item | status |
|---|---|
| **§1.5 / §2.28 residual** Python unfenced on wazero | ✅ **Fixed** (#296). Python runs on wasmtime. The native component path was behind a build tag no build set. |
| **§3.30** what wazero is *for* | ⛔ **Answered, then overtaken.** #299 concluded it was the CGO-less fallback and nothing else, and explicitly did *not* propose deleting it. #459 deleted it on 2026-08-10 — `engine/backend_wazero.go` no longer exists and wasmtime is the only `WasmBackend`. See the note under the limit table. |
| **§3.31** the limit story, per backend | 🔶 **Partly written** (#298, #305). Three of four paths verified; see below. |
| **§3.32** defers ran on wazero, unfenced | ✅ **Fixed** (#303 coverage, #338 fix, #339 the embedded twin). |
| **§3.33** gosec's 283 findings | 🔶 **2 fixed, 281 classified** (#315). G115 is the residual backlog — see below. |
| **§3.35** what `defer` is supposed to be | 📐 **Design written, not implemented** (#337). Blocked on six decisions. |
| **§3.36** errcheck's 283 findings | 🔶 **Triaged, 1 real defect** (#340), handed to WS-1. |
| **§2.40** the linter backlog | 🔶 Four taken by WS-3 and enabled; four remain. |
| round-1 items (§1.7, §2.43, §2.70, §2.71, §2.72, §2.73) | ✅ All closed. |

## Merged: 25 PRs

| PR | |
|---|---|
| #296 | Python runs on wasmtime; the component path was compiled out (§1.5/§2.28, §2.72) |
| #298 | the execution fence ignored the caller's budget on the component path (§3.31) |
| #299 | every deferred callback runs on wazero, unfenced — recorded (§3.30–§3.32) |
| #301 | take `ineffassign` off the linter backlog |
| #303 | no test had ever executed a defer body, and wazero cannot be fenced (§3.32) |
| #304 | claim `gosimple`, and re-measure the rest of the backlog |
| #305 | the decomposition path has never successfully run anything (§3.31) |
| #309 | `cleat build --target python` failed after succeeding, and the test it hid |
| #311 | claim `staticcheck`; two of its findings were real |
| #313 | claim `unconvert`; and `unused` should not be enabled at all |
| #315 | triage gosec's 283 findings; two were real, both slowloris (§3.33) |
| #318 | a guest-supplied output pointer panicked the host (via G115) |
| #327 | the host's scratch buffers could land inside the guest's heap (via G115) |
| #329 | two CI gates that raced for a precondition instead of stating it |
| #330 | a failover test could claim nothing and call it a failure (cluster) |
| #332 | correct a comment that was true when written and false when it landed |
| #334 | a concurrency key's TTL means three different things (§3.34, for WS-1) |
| #337 | what `defer` is supposed to be — a design (§3.35) |
| #338 | defers now run on the fenced backend (§3.32) |
| #339 | the embedded runner collected defer closures and never ran them |
| #340 | errcheck's 283 findings, triaged; one is a silent lock leak (§3.36) |
| #341 | a truncated signal payload reported its full length, so the guest over-read |
| #342 | a truncated cancellation reason reported its full length too |
| #344 | an unlocked map write crash-looped a worker in the cluster job |
| #290 | WS-3's round-1 board |

## Where each guest language runs

```go
var WasmtimeLanguages = []string{"go", "assemblyscript", "java", "rust", "python"}
```

All five, since #296. `engine.WasmtimeLanguages` is the single source of truth — the worker
and `cleat/wasmtest` both read it, and membership means *verified to load and execute on
wasmtime*, not *ought to*. Each entry was run before it was added; `engine/engine.go:285`
records what was run for each.

Python is the entry worth knowing about. It is a Component Model guest, so it takes the
component branch, and that branch has two implementations: the native one
(`component_cgo.go`, wasmtime's own Component Model runtime) and a hand-rolled decomposition
path. **Only the second ever ran** — the native one sat behind the `wasmtime_component_cgo`
build tag, which no build, CI job, Makefile or Dockerfile set, so every build got a stub
returning "not built" and fell through. Three sessions read decomposition's
`undefined element: out of bounds table access` as the state of Python-on-wasmtime. It was the
state of the fallback. With the headers vendored (`engine/wasmtimeinc`) so the tag could be
dropped, the same component executes, records its durable call and returns. No logic changed.

## The execution-limit story, per backend path

The item asked for this per *backend*, and writing it down is what found the gaps: the
wasmtime backend has **three** execution paths, not one, and they had three different answers.

| path | entered when | fence |
|---|---|---|
| core module (`Execute`) | Go, AssemblyScript, Java, Rust | caller's budget ✅ |
| native component (`ExecuteComponentCGo`) | any Component Model guest, i.e. Python | caller's budget, since #298 ✅ |
| decomposition (`ExecuteComponent`) | native path fails for a non-limit reason | inherited, never exercised ⚠️ |
| defers (`RunDefer`) | every deferred callback | the fenced backend, since #338 — but see the residual below |

The generalisable finding: **"which backend runs this" and "which code path inside that
backend runs this" are different questions, and the limit story has to be told about the
second.** Both defects fixed here sat inside the backend `CLAUDE.md` calls the behaviour of
record.

**wazero cannot be fenced for a compute-bound guest.** Measured three ways, all failing:
`WithCloseOnContextDone` breaks all execution; fuel only decrements on function entry; closing
the module has no effect on a tight loop. This is why §3.32 mattered — it was the one path on
which a fully-configured production worker still ran guest code unfenced.

> **What happened next (2026-08-30).** #459 acted on that finding harder than §3.30 proposed:
> the wazero *backend* is gone, and a CGO-less worker now exits 1 at startup
> (`cmd/cleat-worker/main.go:790`) rather than falling back. But wazero is still in the tree as
> `engine.Runtime`, and it still executes guest code on `RunDefer`'s no-backend path
> (`engine/executor.go:706`, whose own comment says "Unfenced, and unavoidably so") and in
> `cleat/wasmtest`, `cmd/cleat run_embedded`, `cmd/cleatctl replay`, `cmd/cleatctl debug` and
> `cmd/cleat-bench`. So the sentence above is still load-bearing; it is now about those paths
> rather than about a backend. Re-derive with
> `grep -rn "NewRuntime(" --include="*.go" . | grep -v _test.go`. Finishing the removal is
> "wazero removal, part 2" in `REMEDIATION-PLAN-2026-08-09.md`, parked.

## Outstanding, in WS-3's files

**1. G115 in the ABI layer — the highest-yield thing left.** Re-measured today: ~200 non-test
sites in WS-3's files, concentrated where the last four defects were.

```
engine/imports.go                     72
engine/wasmtime_hostfuncs_workflow.go 35
engine/wasmtime_hostfuncs_core.go     19
engine/wasmtime_hostfuncs_plugins.go   8
```

134 of them in the host-call boundary. gosec is a poor overflow detector here but an excellent
*index of places where a length was converted*, which is the thing worth reading: in #318,
#327, #341 and #342 the overflow was never the bug — the value meant the wrong thing. Four
defects from the sites reviewed so far.

**2. `packAwaitSignalsResult`'s 16-bit length fields, deliberately not fixed.** Above 64 KiB
the payload length overruns into the signal-name field above it and corrupts it; the
guest-side decoders mask on the way out, so it surfaces as a plausible wrong length rather
than an error. Reachable, because the guest chooses the buffer size. Left alone because the
honest fix is an ABI decision — widen the fields, or return an error code for a payload the
word cannot describe — and masking would only move the lie. Documented at the function
(`engine/helpers.go:54`).

**3. Defers after a trap get the backend default, not the workflow's budget — read, not
run.** `executor.go:361` (`:355` when this was written) deliberately passes
`context.Background()` to the post-trap defer
path so cleanup still happens after a workflow timeout. Correct in intent, but with no ctx
deadline `configureStore` has nothing to reconcile against and falls back to the backend-wide
30s. That is exactly the shape #298 fixed on the component path. **This is read off the code
and has not been measured** — recorded that way on purpose, because every real finding this
session came from executing rather than reading, and this one has not been.

**4. `RunDeferCompiled` has no non-test caller** and still constructs a fresh wazero Runtime
when `e.rt` is nil — the pre-#338 shape, surviving in a function nothing calls. Delete or
route it; either is fine, leaving it is what lets §3.32 grow back.

> Still true on 2026-08-30, and now more pointed: it has **no caller at all**, test or
> otherwise (`grep -rn RunDeferCompiled --include='*.go' .` returns only its own declaration
> and doc comment), and it survived the deletion of the wazero backend in #459 while still
> taking a `wazero.CompiledModule`. It is the last uncalled wazero entry point on `Engine`.

**5. Decomposition should be deleted, in this order.** It has never successfully executed a
workflow — not dead code, something rarer: code that is wired, reached, and has never once
succeeded. But the native path's `componentGetFunc` passes a nil parent export index, so it
cannot reach a function nested inside an exported interface, and such a component falls
through to decomposition today. **Fix `componentGetFunc`, then delete**, and §3.31's remaining
gap resolves by deletion rather than by a test — worth saying explicitly, because "add the
missing test" is the reflex the rest of this document encourages.

## Outstanding, and the author's decision rather than mine

**§3.35, the defer design.** Written up (#337) and unimplemented. The design exists because
#338's fence made an uncomfortable question visible — *bounded doing what, exactly?* — and the
intent had never been written down. The stated intent is that a defer is a **destructor**: it
is guaranteed to run, and it has access to the full workflow context so it can do the cleanup
it exists to do. The current implementation is much further from that than the fence
discussion suggested: a defer runs in a fresh instance with no session, so a closure passed to
`DurableDeferFunc` cannot be honoured at all. Since workflow state lives in event history, the
proposed answer is to run defers in a *replayed* instance with a live session, reconstructing
state the way any resumption does.

Six decisions block implementation:

1. at-most-once or exactly-once
2. does a failing defer fail the workflow
3. which terminal transitions run defers
4. what a defer body may legally do
5. how replay cost is bounded
6. is there a defer budget

The first two are the load-bearing ones; the rest mostly follow.

**The linter backlog.** WS-3 has taken four (`ineffassign`, `gosimple`, `staticcheck`,
`unconvert` — all enabled in `.golangci.yml`, with the three defensive `argIdx++` sites
`//nolint`'d rather than deleted). Four remain: `gocyclo` (28, unclaimed, lowest value),
`unused` (16 — argued in `.golangci.yml` that it should stay off), and `gosec`/`errcheck`,
both now triaged rather than enabled. Re-measure before believing any of those numbers; the
table in `.golangci.yml` is a measurement with a date on it, not a running total.
`ineffassign` was recorded as 8 and measured as 4.

## Found here, owned elsewhere

- **WS-1, §3.36** — `ReleaseWorkflowConcurrencyKeys` discards its error on MySQL and SQL
  Server while PostgreSQL logs it. It releases a workflow's *locks*, so a silent failure
  strands them until TTL. §2.50's shape again. Call sites and a suggested
  `bestEffortTerminalCleanup` helper are in the PR.
- **WS-1, §3.34** — a concurrency key's TTL meant three different things across dialects; a
  500 ms key was born expired on two of them. Handed over as #334, **fixed by WS-1 within the
  hour** as #336.

## Environment

Three colima VMs exist; **`cleat-ws1` and the `default` profile are not WS-3's**.

| | port | container | VM |
|---|---|---|---|
| PostgreSQL 16 | `5434` | `cleat-postgres-manual2` | `default` |
| MySQL 8.4 | `3308` | `cleat-ws3-mysql` | `default` |
| SQL Server 2022 | `1435` | `cleat-ws3-mssql` | `cleat-ws3` |

```
CLEAT_TEST_POSTGRES='postgres://postgres:postgres@localhost:5434/cleat?sslmode=disable'
CLEAT_TEST_MYSQL='root:cleat@tcp(127.0.0.1:3308)/cleat?tls=false&parseTime=true&multiStatements=true'
CLEAT_TEST_MSSQL='sqlserver://sa:CleatTest123!@127.0.0.1:1435?database=cleat'
```

Four things that will bite, all of them cost a debugging session at least once:

- **The Postgres role.** The container was built with `POSTGRES_USER=cleat`, and that name
  collides with the `cleat` *schema*: `search_path="$user",public` then resolves to a
  per-user schema, and the test tables end up split across two of them. Fixed by creating a
  `postgres` superuser role — hence the DSN above, which differs from the one in the round-1
  version of this file. Connect as `cleat` and you will see half a schema.
- **`CREATE TABLE IF NOT EXISTS` never adds a column.** After another stream lands a
  migration, a long-lived MySQL or MSSQL test database keeps its old shape and 31 tests fail
  on `Unknown column 'intent_at'`. Drop and recreate the database; do not debug the code.
- **SQL Server needs its own VM.** It cannot start under QEMU
  (`Invalid mapping of address …`); the `cleat-ws3` profile has Rosetta. Manage it with an
  explicit `docker --context colima-cleat-ws3`. **`colima start` rewrites the global docker
  context** — set it back with `docker context use colima` or every other stream's `docker`
  silently retargets.
- **Do not build or test with `CGO_ENABLED=0`.** It does not skip a check, it removes the
  wasmtime backend from the binary and runs everything on wazero. A result obtained that way
  is not evidence about the engine. `CLAUDE.md` carries the long version.

`componentize-py` cannot be run on this machine: its `componentize` step dies with
`EXC_GUARD / GUARD_TYPE_MACH_PORT — SET_EXCEPTION_BEHAVIOR on mach port`, its embedded
wasmtime installing a mach exception handler into a guarded port. Linux runners have no such
guard, and `e2e-cross-language.yml` builds Python components there on every run.

## Method notes

Everything found this session came from executing, not reading, and one shape recurred often
enough to be worth naming: **a signal that existed and was attached to the wrong thing.**

- A fix that was correct, complete, and behind a build tag no build set (§1.5). Three sessions
  read the fallback's error as the primary path's state.
- A fence that fired the whole time, on the wrong deadline: a 2 s budget ran for 32.9 s
  (#298). It did not look broken from outside, because it *was* firing.
- Two host functions that wrote a truncated buffer and reported the full length (#341, #342),
  each one line below a sibling call that used the written count correctly. Same call, one
  right, one wrong.
- A map written without the lock every other accessor takes (#344). `concurrent map read and
  map write` is a runtime **fatal**, so `withPanicRecovery` cannot catch it — which is why it
  crash-looped a container instead of failing a test.
- Three tests that passed while executing nothing: a 0-parameter defer export, a cancellation
  probe that returned before the write, and a defer-backend test that passed with the fix
  reverted. Each was caught by being suspiciously fast, and each was fixed by proving the test
  could fail.

`#341` and `#342` are the pair worth remembering: the second was found by grepping for the
*shape* of the first rather than by reading on. Five sites matched; four were the payload.

### Claims made here that were later falsified

Kept because they are the same failure mode as the stale notes above, and this file will age
the same way.

- *"An unrecovered cgo panic will kill the worker"* (#318). Predicted before testing;
  `fn.Call`'s recover catches it. The defect was real, the severity was not.
- *"The 25 remaining G115 sites"* — a hand-written list treated as a count. Re-measured today
  at roughly 200 non-test sites in WS-3's files.
- The round-1 falsifications (*"wazero runs Python correctly"*, the `env::cleat_call` ABI
  theory, *"tests/cross-language needs wiring"*, *"the Rust fixture is checked in"*) are in
  this file's history at `2444aae` and are all now resolved.
