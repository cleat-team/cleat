# Integration hub — DBOS port (the wedge, DBOS side)

The DBOS counterpart to [`examples/integration-hub`](../integration-hub/)'s wedge, for
cleat#2597's second cleat-vs-DBOS pair. Unlike the [order-lifecycle
pair](../order-lifecycle-dbos-port/), which is the **control** (an honest,
equally-scoped comparison where DBOS is not worse), this pair exists to
locate a specific structural boundary: **DBOS has no primitive for accepting
a tenant's own code at runtime and executing it, sandboxed or not.**

**Read both pairs' claims as a set, not in isolation.** The order-lifecycle
pair's claim is that the comparison itself is honest. This pair's claim is
about where a real boundary sits. Neither is phrased as a win for either
side — a claim phrased that way would not survive the next release of
either project, and would not be worth writing down.

## The finding, checked against current `docs.dbos.dev` (2026-09-28)

Pages checked, with what each one actually says:

- `/architecture` — describes annotating workflows and steps in **your own**
  application. Nothing about accepting code from an external party.
- `/typescript/tutorials/step-tutorial`, `/typescript/tutorials/workflow-tutorial`
  — `DBOS.registerWorkflow(fn)` / `DBOS.runStep(fn)` both take a function
  **reference**, resolved when your process starts. There is no analogue of
  loading a module from a database row, an uploaded file, or any other
  runtime source.
- `/typescript/tutorials/queue-tutorial` — "multi-tenant" here means
  **partitioned concurrency** ("this fair queue runs at most one task per
  user"), explicitly a fairness feature. The page never mentions sandboxing
  or isolating what a queued task is allowed to do.
- `/typescript/tutorials/upgrading-workflows` — covers deploying a new
  **version of your own code** across in-flight workflows (patching vs.
  versioning). Not third-party code intake.

**So making DBOS express cleat's `POST /api/definitions` + sandboxed
`ChildWorkflow` at all needs two things DBOS supplies neither of:** a
code-upload/storage mechanism, and an isolation layer (a WASM runtime, an
isolate library, a container per tenant). See `ISSUES.md` for the full
citation trail.

## Why the scope had to narrow, and why this port does not fake an upload path

Building an upload endpoint into this port would attribute infrastructure
*this author wrote* to a platform capability DBOS does not have — the
opposite of what a pair is for. So `src/workflow.ts`'s `runTenantStep` does
not attempt one: it composes a "tenant step" the only way DBOS composes any
step, `DBOS.runStep` over a plain function that is already part of this
file at compile time. `stepName` selects one of the **operator's own**
functions, not a tenant's — there is nothing on the DBOS side resolving
tenant-supplied code, because DBOS has nothing that would run it if there
were.

This also means the scope is narrower than `examples/integration-hub/hub.go`'s
full `SyncCustomer` (webhook ingestion, connector dispatch, rate limiting).
Matching the cleat-side sandbox pair's own precedent
(`scripts/run-integration-hub-tenant-sandbox-scenario.sh`, which also skips
the webhook pipeline), this port is scoped to exactly the one property that
script measures: whether a "tenant step" reaches the host filesystem.

## The two steps, mirroring the cleat side's positive/negative control pair

- `normalizeOrder` — the legitimate transform, matching
  `examples/integration-hub/tenant-steps/normalize-order/main.go` at the
  same scope. **This is the mandatory positive control**, carried over from
  the cleat-side sandbox pair's own rule
  (`scripts/run-integration-hub-tenant-sandbox-scenario.sh`'s header:
  *"a FAIL on the adversarial arm alone would be worthless"*). Without a
  step that demonstrably succeeds, a failure on `readHostFile` would be
  indistinguishable from a DBOS runtime that fails every step.
- `readHostFile` — matches the *intent* of
  `examples/integration-hub/tenant-steps/malicious-read-host-file/main.go`:
  the simplest thing a tenant-supplied step could do to reach the host
  filesystem. It reads `/etc/hosts` rather than `/etc/hostname` — the latter
  does not exist on macOS, and an `ENOENT` there is indistinguishable from a
  refusal without reading the message, which cost a local test run while
  writing this port. `/etc/hosts` exists on every POSIX host this runs on.

Under cleat, `read_host_file` traps — `engine/wasi_policy.go` lists WASI's
`path_open` as `wasiFatal`, and `engine/wasi_policy_wasmtime.go` turns that
into a hard trap before the guest's own error-handling code ever runs.
Under DBOS, `readHostFile` **succeeds**.

## Why a trivially-true result is evidence about DBOS, not about Node

`fs.readFileSync` succeeding inside a DBOS step is unsurprising by itself —
Node has full host access by design, and nobody familiar with it expects
otherwise. **That observation is not what this pair is evidence for.** The
claim is about DBOS's platform *surface*: DBOS *could* have interposed
something between a step and the process it runs in — a wrapped executor, a
required isolate, a capability check — the way cleat's engine interposes
`engine/wasi_policy_wasmtime.go` between a guest and every WASI import. It
does not. Nothing in `DBOS.runStep`, `DBOS.registerWorkflow`, or anywhere
else in the SDK's public API touches what a step function is allowed to do.

So `readHostFile`'s success is evidence that the **platform's** contribution
to isolation is zero — not evidence about what Node can do. DBOS could have
built a boundary here; it doesn't, so the entire burden of isolating a
per-tenant step falls on the application author, every time, from scratch.
Cleat's boundary is built once, in the engine, and applies to every tenant
step forever.

## Reading this pair's exit codes — not the usual shape

`src/wedge.test.ts` and `scripts/run-integration-hub-dbos-scenario.sh` both
use:

    0   both assertions held: the positive control ran, and readHostFile
        succeeded exactly as documented above.
    1   A FINDING, not "the test failed" in the ordinary sense. readHostFile
        was refused or threw. That would FALSIFY the claim this file
        documents — meaning today's DBOS does not behave the way this pair
        says it does — and needs investigating (did the SDK change? did
        something OUTSIDE DBOS, e.g. a container's read-only filesystem,
        intervene?), never silently accepted or loosened away.
    2   UNMEASURED: the positive control itself did not hold, or the harness
        crashed before reaching a verdict. Neither says anything about the
        boundary this pair exists to measure.

**This is stated explicitly because `1` conventionally reads as "the test
failed."** A future reader who sees exit 1 and assumes a broken run, rather
than a possible change in DBOS's own behaviour worth re-checking against
current docs, has drawn exactly the wrong conclusion from this pair's own
design.

## The second counterpart: DBOS plus a real sandbox (`src/isolated-workflow.ts`)

**Added after cleat-review's review of the bare-step counterpart above,
because the comparison it makes on its own is one-sided.** Bare
`DBOS.runStep` with no sandboxing shows what happens if a team does
*nothing* special — and no real team ships tenant-supplied code that way.
The owner's feedback, relayed by the coordinator: *"bare DBOS is not the
competitor... The honest counterpart is 'DBOS plus a sandbox service', and
the pair should measure that."* This section is that counterpart, and it is
**complementary to the one above, not a replacement for it** — the bare
version's finding (DBOS's platform surface supplies zero isolation) is still
true and is the entire reason this section's extra machinery exists at all.

**Why `isolated-vm`, specifically.** It is the lowest-friction idiomatic
choice for a Node/TypeScript team that wants to run untrusted JavaScript
without standing up a container fleet or a separate sandboxing service: no
extra infrastructure, no network hop, just a library dependency. A V8
isolate has its own heap and starts with **no** access to Node's built-in
modules (`require`, `fs`, `process`, network sockets) unless the host code
explicitly injects a reference into the isolate's global object — a real
boundary, verified empirically below rather than merely asserted. A
container-per-tenant or a separate sandbox microservice were the other two
options the owner's framing named; `isolated-vm` was chosen because it needs
no additional infrastructure to demonstrate in a CI job, which keeps this
pair reproducible the same way the bare version is. The cost/durability
analysis below would look qualitatively the same for either alternative —
an out-of-process boundary makes the durability question *sharper*, not
weaker, since more state then crosses the boundary between the step and its
checkpoint.

**The tenant's code is a string here, not a function reference** — a
deliberate difference from `src/workflow.ts`'s `runTenantStep`. A function
already compiled into `workflow.ts` at build time is not what a tenant
uploads; a string evaluated at runtime inside an isolate
(`src/isolated-workflow.ts`'s `NORMALIZE_ORDER_SOURCE` /
`READ_HOST_FILE_SOURCE`) is the closest idiomatic DBOS analogue to cleat's
`POST /api/definitions` payload. It is still not a full analogue — nothing
here *persists* an uploaded string across a process restart, the way
cleat's WASM module storage does — so this remains scoped to the one
question both counterparts measure: what happens to a tenant step's host
access, not the full upload/storage lifecycle.

**The result: the isolate refuses the read, empirically.**
`src/isolated-wedge.test.ts` runs three tenant behaviours
(`normalize-order`, `read-host-file`, and — cleat#2628 — `infinite-loop`)
through `DBOS.runStep` wrapping `isolated-vm` execution instead of a plain
function call, with the same mandatory positive control this pair's bare
version requires. Verified 2026-09-29, against a real DBOS runtime and
Postgres:

    ok: the legitimate tenant step completed through the isolate
    require is not defined
    ok: readHostFile was refused by the isolate (ReferenceError: require is not defined)
    Script execution timed out.
    ok: the runaway loop was interrupted by the isolate's timeout (Error: Script execution timed out.)
    all assertions passed -- the isolate boundary holds

The refusal is `isolated-vm`'s doing, not DBOS's: the isolate simply has no
`require` in its global scope. The interruption is a second, independent
mechanism of the same library — a per-call `timeout`, not the missing
`require` — bounding a tenant script that never tries to escape at all, only
never returns; see `src/isolated-workflow.ts`'s `INFINITE_LOOP_SOURCE` for
why that is a different adversarial shape from `read-host-file` and needs
its own test. **Falsified by mutation, in all the directions this file's
three assertions admit**, same discipline as the bare counterpart:

| mutation | result |
|---|---|
| (none) | exit 0 — positive control holds, both isolation checks hold |
| break the positive control (force `normalize-order` to throw) | exit 2 — UNMEASURED, both isolation tests correctly skipped |
| leak `require` into `readHostFileIsolated` (simulate a misconfigured sandbox) | exit 1 — FINDING, correctly reports the isolation did NOT hold |
| remove `timeout` from the `fn.apply` call that runs the loop (cleat#2628) | exit 2 — the isolate genuinely never returns; `DBOS.shutdown()` and even `process.exit()` cannot terminate a process with a wedged native isolate thread (both measured), so the harness self-SIGKILLs rather than hang the CI job forever. See `isolated-wedge.test.ts`'s `SAFETY_MARGIN_MS` comment. |

The third row is the one worth dwelling on from the earlier revision of this
pair: it is not a hypothetical. Simulating a team that wires its sandbox up
incorrectly (or a future `isolated-vm` release that changes its default
global scope) produces exactly the same exit code as a real regression,
which is the entire point of the 3-way contract — a broken sandbox is a
**finding**, not a silent green. **The fourth row is a different shape of
the same lesson, found by running it rather than reading it**: the obvious
fix for "this might hang" — call `process.exit()` — does not actually work
once a native thread is truly wedged, and the only way that was discovered
was letting the falsification run to completion under an external hard
bound instead of trusting that `process.exit()` does what its name implies.

## What this counterpart costs, and where the DBOS side's number understates it

**An idiomatic team does not get this boundary for the cost of `npm install
isolated-vm`.** The line count below (see "Counting this pair,
role-symmetric") is real, but it undercounts the actual burden in one
specific way that is worth stating rather than leaving for a reader to
discover: `src/isolated-workflow.ts`'s two tenant behaviours are **pure
functions** with no side effects, chosen to mirror `workflow.ts`'s
`normalizeOrder`/`readHostFile` exactly. That choice is what makes the LOC
comparison fair — but it is also why this counterpart cannot demonstrate the
single most important cost of the isolate boundary: **what it takes to keep
durability across it.**

**The mechanism, cited from DBOS's own documentation
(`docs.dbos.dev/architecture`, checked 2026-09-28):** DBOS steps are
**at-least-once**, not exactly-once. *"Eventually, the recovered workflow
reaches a step with no checkpoint... The recovered workflow executes that
step normally... resuming from the last completed step."* And from
`/typescript/tutorials/step-tutorial`: *"Steps should be idempotent... If a
workflow fails while executing a step, it retries the step during recovery.
However, once a step completes and is checkpointed, it is never
re-executed."*

**Composing `DBOS.runStep` with an isolate execution does not change that
contract — it just moves the boundary a crash can land inside.**
`runTenantStepIsolated` treats the whole isolate run (spin up, eval, invoke,
marshal the result, dispose) as one opaque unit from DBOS's point of view.
If the process crashes *after* the isolate has finished — a tenant's code
has already run, its side effect (a charge, a webhook, a write to another
system) has already happened — but *before* DBOS checkpoints the step, the
recovered workflow re-executes `runTenantStepIsolated` from the start, and
the isolate runs the tenant's code again. DBOS's idempotency requirement is
stated for *the step function*, and here the step function is "run whatever
the tenant uploaded" — DBOS has no way to inspect or enforce idempotency on
code it does not control, and neither does `isolated-vm`: the isolate
enforces what the tenant's code can *reach*, not how many times it *runs*.

**Why this pair's own two test functions cannot exercise that risk, and why
that is not a gap in the test — it is a fact about what pure functions
are.** `normalizeOrder`/`normalizeOrderIsolated` and
`readHostFile`/`readHostFileIsolated` are pure: re-running either one
twice, from scratch, produces the same observable result and no double
effect. That is exactly why they are safe choices for a line-count
comparison (their DBOS-side and cleat-side versions do the same work, so
the LOC delta isolates the execution-boundary cost rather than smuggling in
different business logic) — and exactly why re-execution risk is invisible
to them. Demonstrating the risk for real would need a tenant step with an
external side effect (a charge, an outbound webhook) and a way to crash the
process between the isolate finishing and DBOS's checkpoint write — a
fault-injection harness, not a difference in tenant logic. Building that
was judged out of scope for what this pair measures (an execution-boundary
comparison, not a fault-injection study of either platform), so the risk is
**documented here, with its exact mechanism and citation, rather than
fabricated as a passing test that doesn't actually exercise it.**

**Cleat does not have this problem, and the reason is worth stating
precisely rather than as a slogan.** cleat's engine records every host call
a WASM guest makes (including sandbox-boundary-crossing ones) as part of
the workflow's durable event log, and replay re-delivers recorded results
rather than re-executing the call — so a crash between a guest's host call
and the engine's checkpoint does not re-run the guest's side effect on
recovery. That mechanism is `engine/` infrastructure (part of the 124
platform lines counted below), not something either DBOS counterpart's
application code reproduces. **This is the cost this pair's numbers most
understate**: it does not show up as a line count difference at all,
because nothing on the DBOS side has a comparable mechanism to count lines
*of* — the gap is a missing capability, not a smaller implementation of the
same one.

## The timeout that makes this counterpart competent, not just present

**A sandbox with no CPU bound is not a sandbox a real team would ship.**
The first version of `src/isolated-workflow.ts` called `fn.apply` with no
`timeout` option — cleat-review's review caught it: without one, a tenant
step containing `while(true){}` hangs the isolate, the DBOS step, and the
workflow forever. That is not a finding about DBOS or about `isolated-vm`;
it is a finding about this counterpart being unfinished, and an unfinished
sandboxed counterpart makes the gap it measures look larger than it really
is — the same "flattering error" direction this README's other sections
have been careful to check for elsewhere.

**Fixed by adding `timeout: 5000` to both `context.eval` and `fn.apply`**
(`runInIsolate`, `src/isolated-workflow.ts`) — the eval call needs one too,
not only the invocation, because a tenant could hang the isolate with a
top-level infinite loop in the source string itself, before any function is
ever called. **Verified empirically, not just cited from `isolated-vm`'s
docs**: a scratch isolate running `(function(){ while(true){} })()` with
`timeout: 2000` threw `Script execution timed out.` after 2008ms. cleat
bounds guest execution the same way, at the engine level
(`tenant_settings.wasm_wall_clock_ceiling_ms`, plus further engine limits);
this is that same defence, added on the DBOS-isolated side rather than
omitted. The 5000ms figure is counted in the "host runner" row below, since
it is part of the plumbing that invokes the tenant's code, not the tenant's
own logic.

**Not shipped in this PR: a third tenant behaviour (an infinite loop) run
on both sides, asserting each platform actually bounds it.** cleat-review
suggested this as a strong-but-optional addition — it would turn "bounded
execution" into a tested claim on both sides rather than one verified only
by a scratch script here. Filed as a follow-up rather than silently
dropped: cleat#2628. cleat's own side would need its own
`tenant_settings.wasm_wall_clock_ceiling_ms` case added to
`scripts/run-integration-hub-tenant-sandbox-scenario.sh`, which is outside
this PR's scope (fixing the isolated-vm counterpart's missing timeout, not
extending cleat's own sandbox scenario).

## The tenant step's own durability: a workflow vs. one atomic step

**On cleat, a tenant step is not merely sandboxed code — it is a full child
workflow**, per `hub.go`'s `h.ChildWorkflow(in.TenantStepName, ...)` /
`h.AwaitChild(runID)`. That means a tenant's own step can itself make
durable host calls, sleep, wait on an event, and be replayed independently
of the parent workflow — the same durability guarantees any cleat workflow
gets, because a tenant step IS a cleat workflow, running under the same
engine.

**On the DBOS-isolated counterpart, a tenant step is one opaque,
atomic, at-least-once `DBOS.runStep` call** wrapping the isolate's entire
execution. There is no durable operation available *inside* the isolate:
`isolated-vm`'s isolate has no access to `@dbos-inc/dbos-sdk` (nor should
it — handing a sandboxed isolate a reference to the host's DBOS client
would defeat the sandbox), so a tenant step that needed to sleep, wait for
an event, or call another durable operation from *within* its own logic
has no path to do so without the host bridging specific DBOS APIs into the
isolate's global scope — which is more plumbing, and another boundary to
secure (each bridged function is a new surface the isolate could call in a
way its author did not intend).

**This pair's two tenant behaviours don't exercise this difference
either**, for the same reason they cannot exercise the durability-boundary
risk two sections up: both are synchronous, single-call pure functions with
no need for a nested durable operation. The claim is structural rather than
something a passing test demonstrates here — stated with its mechanism,
the same discipline as the cost this pair cannot measure directly.

## Counting this pair, role-symmetric

**Re-derive with `scripts/dbos-pair-loc.sh integration-hub`** (`cloc` 2.10).
Snapshot dated **2026-10-03** — re-run the command rather than re-quoting
these rows, per CLAUDE.md's rule on numbers in prose.

**This is the fourth shape this table has taken, and each move fixed a
real asymmetry someone found, not a preference:** an initial "app lines
vs. platform lines" split compared cleat's two tenant-step files against
**all four** DBOS files (both variants, both test files) — 49 vs. 240.
Making it role-symmetric (tenant code / host runner / tests / platform,
bare DBOS as an excluded CONTROL) gave 442 vs. 136 — but that total
**summed cleat's 124 platform lines into an app comparison**, and its
"tests" row counted cleat's ~198-line HTTP-deployed-worker harness while
giving DBOS's analogous ~42-line harness no line at all; the fix was
**platform on its own line, never summed**, and **both end-to-end
harnesses shown, neither summed into either app total**, giving 120 vs.
140. The coordinator then asked a fair question about that table: cleat's
"host runner" row was a *fragment* of `hub.go` (21 of 176 lines) while
DBOS-isolated's was nearly the *whole* of `isolated-workflow.ts`, so the
two extractions were not obviously asking the same question — and a
sweep of every `tenant`-related line in `hub.go`
(`grep -ni tenant examples/integration-hub/hub.go`) turned up real wedge
wiring the first dispatch-block-only extraction had missed: the
`TenantStepName`/`TenantStepRan` struct fields, the result assignment, and
the flag's initialisation — none of them inside the `if` block itself, all
of them load-bearing. Adding them moved cleat's host-runner row from 21 to
**25**, from 120 to 124 against cleat's total. **That is not a trend, and
this table does not claim one**: the correction two sentences earlier —
442 vs. 136 collapsing to 120 vs. 140 — moved far further in
cleat's favour. The corrections found here have run in both directions;
the result should not depend on which one a given round happens to be
(cleat-review caught an earlier draft of this section implying otherwise,
which the coordinator had already corrected once in review and not yet in
this file).

**This is the fifth shape, and the direction it moves in is not the
lesson — where each side's cost landed in this table is.** cleat#2628
added a THIRD tenant behaviour to both sides — an infinite loop, bounding
a runaway tenant step rather than one that tries to escape through a host
call — after cleat-review noted the original two behaviours tested only
the CPU-unbounded counterpart's `require` refusal, never the `timeout` it
also carries.

**Both sides paid almost the same CODE cost for the new behaviour's own
assertions, and this table used to say otherwise, wrongly.** A first draft
of this paragraph attributed DBOS-isolated's larger jump to "comments" —
`cloc`'s **code** column, which every number below is, does not count
comments at all, so that reasoning could not have been right regardless of
whether the comments existed (cleat-review caught this, and noted the same
*placement* asymmetry -- a real behaviour-test cost landing unevenly
across this table's rows -- had already slipped through un-caught for
`read-host-file` in #2621's own review of this table; the comments
misattribution itself is new here). The real, code-level comparison,
each side's dedicated test file against its base-commit self:
`isolated-wedge.test.ts` **+50** code lines (72 → 122);
`run-integration-hub-tenant-sandbox-scenario.sh` **+41** (198 → 239). Close
enough that the honest reading is "the same behaviour, tested at
comparable cost on both sides" — the residual 9-line gap is
`isolated-wedge.test.ts` also carrying a **result-sentinel mechanism**
(`run-tests.js`'s `CLEAT_STUCK_LOOP_SENTINEL`) that translates a
self-inflicted `SIGKILL` back into an exit code, which cleat's side has no
equivalent problem to solve: `wait_for_terminal`'s HTTP poll never has to
distinguish a genuine result from a process that killed itself to avoid
hanging. **Where those costs get COUNTED, not their size, is what actually
moved this table, and it is the defect cleat#2642 fixes**: DBOS's +50
landed in **unit tests**, a row the app total sums; cleat's +41 landed in
**e2e harness**, a row it does not (see "Why the two harness scripts get
their own excluded row" below) — so the app-total gap widened even though
the underlying test-writing effort did not, in either direction,
meaningfully diverge.

Adding the third behaviour also moved the smaller rows a small, genuinely
asymmetric amount: cleat's tenant code (a third `main.go`, +6) and
DBOS-isolated's tenant code (a third template literal, +5) and host
runner (the new wrapper function and `switch` case, +5) — noise at this
size, the same read the table already gives the pre-existing rows below.

**This is the sixth shape, and it removes that placement difference
rather than describing it.** cleat#2642's finding — that the same work was
summed on one side and not the other — is fixed by extracting each side's
**behaviour assertions** into their own row and summing that row on BOTH
sides. The extraction is mechanical and bounded by each source's own
structure (`scripts/dbos-pair-loc-extract.py`, its `sh-banner-block` and
`ts-func` modes): cleat's three `# ---- <behaviour>` banner blocks out of
`run-integration-hub-tenant-sandbox-scenario.sh`, and DBOS-isolated's three
`test…()` functions out of `isolated-wedge.test.ts`. Each side's file is
then what it always was minus those blocks — cleat's e2e harness machinery
(the real-worker driver, still excluded, see below) and DBOS's remaining
test file (its `main()` driver and helpers, still summed).

**Every row moves the way the fix implies, and in the direction that costs
cleat rather than flattering it.** cleat's app total goes **130 → 215**;
DBOS-isolated's is **unchanged at 200**, because its 122-line test file
simply splits into 72 machinery + 50 behaviour — the DBOS side was never
the one missing a row. **So cleat's app code is no longer *smaller* on
this pair. It is larger: 215 against 200.** That claim ("cleat's app code
is SMALLER") was read as evidence in
`examples/order-lifecycle-dbos-port/README.md`, and is corrected there in
this same PR.

**Corrected 2026-10-08 (cleat#3041):** this section used to say the
residual below was left in the sum. DBOS's counted `unit tests` row was
its test file minus the three behaviour functions, and that remainder
still included the hand-rolled `main()` driver, config/launch and
exit-code plumbing `isolated-wedge.test.ts` needs because the port runs
`node run-tests.js` rather than a test framework, where Go's `testing`
package supplies cleat's equivalent for free — so part of the old 72 was
scaffolding cleat's side excludes, and leaving it in the sum flattered
cleat by up to those lines, the wrong direction for a pair whose whole
purpose is to be the CONTROL. The owner ruled **A** on cleat#3041: adjust
the DBOS column so the two rows measure the same kind of code. The
boundary drawn is the driver orchestration (`DBOS.setConfig`/`launch`,
the positive-control gate, `shutdown`, the two `process.exit` codes) plus
the top-level `main().catch(...)` crash handler — **34** lines, now its
own excluded row below, in the same shape as `e2e harness machinery`. The
stuck-loop/SIGKILL/sentinel branch stays IN `unit tests`: it asserts a
BEHAVIOUR (a genuinely wedged isolate cannot be terminated) that cleat's
side has an analogous assertion for, not scaffolding. `unit tests` is now
**38**, and the DBOS-isolated app total moves from 200 to **166** —
re-derive both with `scripts/dbos-pair-loc.sh integration-hub`.

| role | cleat | DBOS-isolated |
|---|---:|---:|
| tenant code | **55** | **24** |
| host runner | **25** | **54** |
| unit tests | **50** | **38** |
| behaviour assertions | **85** | **50** |
| **app total** (tenant + host + unit tests + behaviour assertions) | **215** | **166** |
| platform (own line — not summed above) | **124** | **0** |
| e2e harness machinery (own line — not summed above, see below) | **154** | **42** |
| unit test driver (own line — not summed above, see below — cleat#3041) | **0** | **34** |

*Bare `DBOS.runStep` (no sandbox) is deliberately not a row or a column
here either — it is the CONTROL, reported separately below, at **104**
lines.*

**What each row actually contains, since several are extracted fragments
rather than whole files** (`scripts/dbos-pair-loc-extract.py`, bounded by
the source's own structure — a brace block, a named function, a template
literal's closing backtick — not a frozen line range, so the extraction
tracks edits rather than silently drifting):

- **tenant code**: cleat's three tenant-steps `main.go` files, whole (a
  cleat tenant author writes nothing else). DBOS-isolated's three source
  **string literals** (`NORMALIZE_ORDER_SOURCE`, `READ_HOST_FILE_SOURCE`,
  `INFINITE_LOOP_SOURCE`) — the closest analogue to tenant-authored code on
  that side, since DBOS has no upload mechanism and this port stands the
  string in for what a tenant would upload.
- **host runner**: the `if in.TenantStepName != ""` dispatch block
  extracted from `hub.go`, **plus every other line the dispatch depends
  on** — the `TenantStepName`/`TenantStepRan` struct field declarations,
  the `TenantStepRan: tenantStepRan` result assignment, and the
  `tenantStepRan := false` initialisation, none of which live inside the
  block itself. The rest of `hub.go` is far larger and out of this pair's
  scope (see "Not a full SyncCustomer port"); `scripts/dbos-pair-loc.sh`'s
  comment shows the `grep -ni tenant` sweep that this row's five fragments
  come from. DBOS-isolated's is `isolated-workflow.ts` **minus** the three
  tenant-code string literals — `runInIsolate` (isolate setup, the CPU
  timeout, teardown), the three async wrapper functions, and the workflow
  registration.
- **unit tests**: cleat's two `hub_test.go` functions that exercise
  `TenantStepName` (`TestSyncCustomer_RunsTheTenantsOwnStep`,
  `TestSyncCustomer_ATenantStepThatFailsNamesItsStep`). DBOS-isolated's is
  `isolated-wedge.test.ts` **minus its three `test…()` behaviour
  functions** (counted in the row below instead) **and minus its
  hand-rolled test driver** (counted in `unit test driver` below instead
  — cleat#3041; see the correction above). What remains is the shared
  scaffolding (imports, the `CLAIM` string, the state flags, `log`,
  `SAFETY_MARGIN_MS`, `delay`) plus the stuck-loop/SIGKILL/sentinel
  branch, which asserts a behaviour rather than driving the test run.
- **behaviour assertions**: the three tenant behaviours each side tests —
  positive control, `read-host-file` refusal, and the `infinite-loop`
  timeout. cleat's are the three `# ---- <behaviour>` banner blocks inside
  `scripts/run-integration-hub-tenant-sandbox-scenario.sh`; DBOS-isolated's
  are `testPositiveControlNormalizeOrderSucceedsThroughIsolate`,
  `testReadHostFileIsRefusedByTheIsolate` and
  `testRunawayLoopIsInterruptedByTheTimeout` in `isolated-wedge.test.ts`.
  This is the row cleat#2642 added, and the one that is genuinely
  like-for-like: the same three behaviours, tested at the same scope, on
  both sides — which is why it is counted in the app total.
- **platform**: `engine/wasi_policy.go` + `engine/wasi_policy_wasmtime.go`
  for cleat, nothing for DBOS-isolated (see below).
- **e2e harness machinery**: `scripts/run-integration-hub-tenant-sandbox-scenario.sh`
  **minus** the three behaviour blocks counted above (so the two rows do
  not double-count it) for cleat, and `scripts/run-integration-hub-dbos-scenario.sh`
  for DBOS-isolated — see immediately below for why these are shown but
  not summed.
- **unit test driver** (cleat#3041): nothing for cleat, which gets this for
  free from Go's `testing` package — `isolated-wedge.test.ts`'s driver
  orchestration (`DBOS.setConfig`/`launch`, the positive-control gate,
  `shutdown`, the two `process.exit` codes) plus the top-level
  `main().catch(...)` crash handler for DBOS-isolated. See the correction
  above for why this is its own row rather than part of `unit tests`.

**Why the two harness scripts get their own excluded row instead of being
folded into "unit tests" or dropped.** cleat's harness script is 239 lines
in full; the **154** shown is that file minus the three behaviour blocks
counted above, so nothing is counted twice. It starts a real
`cleat-worker`, provisions a tenant, and uploads a WASM module through
`POST /api/definitions` over HTTP — it is the only place this pair
exercises **runtime code intake**, because that is how a tenant's code
actually reaches a cleat deployment. DBOS's harness script (~42 lines) is
an npm install/build/test wrapper with no assertions of its own; the
equivalent end-to-end proof on that side lives inside `isolated-wedge.test.ts`
— its three behaviour assertions, counted above — calling
`DBOS.startWorkflow` **in-process** — there is nothing analogous to upload
over HTTP, because "tenant code" here is a string evaluated inside the same
process. Neither harness is doing unnecessary work; they reflect two
different deployment topologies, and the topology cleat proves against is
the one a real multi-tenant deployment actually has. Showing both numbers,
rather than dropping cleat's or inventing a DBOS-side equivalent that
doesn't exist, is the version of this table that survives a skeptical
re-read.

**Why DBOS's platform row is 0, and why that is not a rounding artefact.**
`@dbos-inc/dbos-sdk` contributes no isolation code of its own.
`isolated-vm` *does* supply the actual boundary, but it is a third-party
library the application author chose, installed, and wired up — counted in
the **host runner** row, not platform, the same way a team choosing a
container-per-tenant would write and maintain their own orchestration
rather than receive it from DBOS.

**So the honest reading of this table is not a single number — it is
several separate claims, each real:**

1. **The like-for-like application-code TOTAL is cleat larger by 215 to
   166, and that gap is a real reading now, not a placement or scaffolding
   artefact.** An earlier version of this table reported **130 vs. 200,
   with cleat SMALLER**; that gap was a placement artefact rather than a
   cost difference, and cleat#2642 removed it (see "the sixth shape" above
   for the mechanism and the direction). The total then read 215 vs. 200
   until cleat#3041 found that DBOS-isolated's side of it still included
   34 lines of hand-rolled test-driver scaffolding cleat's side never
   counted (see the correction above) — removing it moved DBOS-isolated's
   total to 166 without touching cleat's. 215 vs. 166 is
   tenant code + host runner + unit tests + behaviour assertions on both
   sides, up from 124 vs. 140 before cleat#2628's third tenant behaviour.
   Read this as "the pair's case for cleat does not rest on line-count
   arithmetic" — it rests on the three capability claims this README
   states with their mechanisms: platform-enforced isolation (this
   section), durable tenant code ("The tenant step's own durability"), and
   runtime code intake (the e2e-harness note above).
2. **cleat ships an isolation boundary as a platform capability** (124
   lines, on its own line, written once, in `engine/`) that every tenant
   step gets automatically. Those 124 lines are not free — someone at
   cleat wrote and maintains them — but they are not paid by the
   tenant-step's own author, which is the comparison the app total is
   about. An idiomatic DBOS deployment has no equivalent platform line to
   count, because the capability does not exist there.
3. **cleat's tenant-code row is bigger than DBOS-isolated's, and its
   host-runner row is close** (55 vs. 24 is reversed from DBOS's favour
   because DBOS's tenant code is a minimal source-string stand-in, not a
   real upload payload; 25 vs. 54 reflects `isolated-vm`'s
   setup/teardown/timeout plumbing against cleat's
   `ChildWorkflow`/`AwaitChild` call plus the struct-field wiring around
   it). Largely Go-program-vs-JS-string and library-plumbing differences,
   not a cleat-vs-DBOS platform difference — read them as noisy at this
   size, not as a trend. **The behaviour-assertions row (85 vs. 50) is the
   one genuinely like-for-like row, and cleat#2642 is what makes it so**:
   the SAME three tenant behaviours, extracted from each side's own file
   by the same counter on both sides. cleat's figure is the larger because
   each of its shell blocks carries the HTTP-and-JSON plumbing for its own
   assertion where DBOS's TypeScript calls a helper — a shape difference,
   not a proportional difference in how much testing each side had to
   write. The evidence is cleat#2628's new behaviour: both sides paid
   nearly the same code cost for it, DBOS-isolated's `isolated-wedge.test.ts`
   +50 lines and cleat's `run-integration-hub-tenant-sandbox-scenario.sh`
   +41 (see "the fifth shape" above), and the residual 9-line gap is
   `isolated-wedge.test.ts` alone needing a result-sentinel mechanism
   cleat's HTTP-polling harness has no equivalent problem to solve.
4. **Neither counterpart's line count captures the durability-boundary
   cost** ("What this counterpart costs") or the tenant-step-as-workflow
   difference ("The tenant step's own durability"), because both are
   missing capabilities rather than a smaller implementation of an
   existing one.

A smaller number on either side is not, by itself, a win for that side —
the order-lifecycle pair's own rule (*"Tenancy: 0 lines, and it is not a
compliment"*) applies here too: a 0 in DBOS's platform row is a capability
gap, not an efficiency, and DBOS-isolated's non-zero host-runner and
tenant-code rows are the honest price of an application author closing
part of that gap themselves.

## Version and date

`package.json` pins:

- `@dbos-inc/dbos-sdk` to `5.2.11` — the version **all three** DBOS ports now
  pin (`order-lifecycle-dbos-port`, this one, `b2b-saas-control-plane-dbos-port`),
  in `package.json` and in `package-lock.json`'s resolver entry alike.
  **Corrected 2026-10-04 (cleat#2955):** this bullet said `5.1.10` — the
  version current when the port was written, confirmed then with
  `npm view @dbos-inc/dbos-sdk version` — and closed with "the same version
  the order-lifecycle pair already pins. No drift between the two pairs to
  record." That closing clause is the part that had gone false while the rest
  of the bullet stayed true and unremarkable: `order-lifecycle-dbos-port`'s
  README had already been updated when the siblings were bumped, and the
  claim left behind here was that no drift needed recording. **Two sites
  still carried it** — this bullet and `src/workflow.ts`'s header comment —
  and a first pass of this correction fixed only this one, because the sweep
  behind it searched `*.md` and `workflow.ts` is not markdown. That pass also
  wrote, here, that "this file was the last one still asserting the drift was
  absent" — a claim about a search that had not been run, which the second
  pass falsified. Both are corrected; the *sweep* is the part worth naming,
  because a scoped search and a whole-tree claim look identical in the
  sentence they produce.
- `isolated-vm` to `6.1.2`, paired with **Node 24** ("Krypton", the current
  LTS line as of 2026-09-28) in both CI (`.github/workflows/ci.yml`) and
  `@types/node`. Both were verified empirically in this PR, not chosen from
  a changelog: `isolated-vm@7.x` requires Node ≥26, which as of this date is
  still the bleeding-edge "Current" release rather than LTS — the less
  defensible pin for a job meant to demonstrate what an idiomatic team would
  actually ship. `6.1.2` ships a prebuilt native binary for `darwin-arm64`
  and Linux (`node_modules/isolated-vm/prebuilds/`), so CI needs no C++
  toolchain to install it. See `src/isolated-workflow.ts`'s header comment
  for the full version rationale.

## Build and run

```bash
npm install
npm run build
```

Needs `DBOS_SYSTEM_DATABASE_URL` (a plain Postgres connection string; DBOS
creates its own system-database schema there on launch):

```bash
DBOS_SYSTEM_DATABASE_URL=postgres://postgres:PASSWORD@127.0.0.1:5432/integration_hub_dbos \
  npm test
```

`npm test` runs **both** counterparts (`dist/wedge.test.js`, the bare
version, then `dist/isolated-wedge.test.js`, the sandboxed one) via
`run-tests.js`, which combines their exit codes by taking the more severe
one (2 beats 1 beats 0) — a 2 from either run means at least one of this
pair's two claims was never actually checked, which is worse than a 1 (a
claim that was checked and came back false), so it must win regardless of
which run produced it. `scripts/run-integration-hub-dbos-scenario.sh`, at
the repo root, is what CI runs — it installs, builds, and runs `npm test`,
translating the combined exit code into the three-way status documented
above.
