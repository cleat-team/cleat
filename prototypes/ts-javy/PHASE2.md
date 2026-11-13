# Phase 2 — determinism (branch `proto/ts-javy`, remote `feature/proto-ts-javy`)

> **Provenance (2026-11-13).** Written from a run built from an uncommitted working tree. `evidence/phase2/` has been **regenerated from a clean checkout of `cdbfc39b`** (PHASE3.md), with cleat-review's seven checkpoint-1 corrections folded in (last section). Where a figure here differs from the regenerated evidence, the evidence is right; the largest moves are listed at the end of PHASE3.md.

Prototype only. No PR to `develop`, no `tiers.yaml` entry, no support claim anywhere. Plan §4.5, Q3, and the coordinator's
Phase 2 decisions. **For `cleat-review`, review checkpoint 1.**

Everything below is re-derived by one script from a fresh build, about 30 minutes, exit 0 only if every step is 0:

    prototypes/ts-javy/phase2-verify.sh        # rebuilds everything, reruns Phase 1, then Phase 2; evidence/phase1/ and evidence/phase2/

The committed evidence is the output of that script on the final tree, run once with exactly one worker alive at a time.

## Where the plan's Q3 pass line is and is not met

Q3: *every control fails without its shim and passes with it.* A control here is a plugin built with exactly one shim removed
(`CLEAT_SHIMS_OFF`, read at compile time), the same workflow run on it, and the same workflow run on the shipped plugin.
Each `nondet` run records a value, suspends on a sleep, then REPLAYS and recomputes it; `same` is recorded == recomputed.

| §4.5 row | Without its shim | With it | Pass line |
|---|---|---|---|
| `Math.random` | **fails**: `replay divergence at step 0` (both `random` and a 400-draw `randomRange`) | replay-stable, uniform in [0,1) (min 0.0018, max 0.9949, mean 0.499, 400 distinct) | **met** |
| `Date.now`, `new Date()` | **nothing changes.** All five date sources (`dateNow`, `newDate`, `dateElapsed`, `newDateElapsed`, `dateEqualsHostNow`) are replay-stable with the shim removed | same | **NOT met, by finding, not by omission** |
| `performance.now`, `crypto.getRandomValues` | the globals are **absent** from this Javy 9.1.0 build | same | cannot fail today |
| `WeakRef`, `FinalizationRegistry` | **absent** (`typeof` is `"undefined"`) | same | cannot fail today |
| `setTimeout`, `setInterval` | **absent** | same | cannot fail today |
| `JSON.stringify` order, floats / NaN | not Phase 2 (Q4, Phase 3) | | not measured |
| Engine version | see "Engine pin" below | | met in tooling, **not in the worker** |

**The Date row, said plainly.** The plan asked "with the override removed, does WASI interception alone still pass? Record which
layer actually holds." Measured answer: **it passes, so the engine's `clock_time_get` interception holds on its own and the Date
shim is not load-bearing in this engine.** The two layers cannot be told apart by value either: `Date.now() === ctx.now()` is
true with and without the shim, and a busy loop between two reads gives `Date.now() - a > 0` = `false` both ways (and `ctx.now()` reads
0 across the same loop), i.e. the clock is frozen **between** recorded host calls under either layer, so a branch on elapsed time takes the same
path live and on replay. **It does advance at a recorded host call (a `sideEffect`), not at a `ctx.now()` read** (corrected 2026-11-13: cleat-review's wording was "fixed even across host
calls", and the probe says otherwise): across two recorded side effects `Date.now()` moved by real time (15 to 35 ms in the runs I made, with and without the shim; it is wall time, so the verdict asserts only "greater than 0 and equal on replay") and replay reproduced the very same figure. So time is a function of the
recorded events, not constant within a segment, and it moves forward across a suspension as well. I hypothesised
before running it that frozen-on-replay time would make live and replay diverge; the run says it does not. The shim stays as defence in
depth against the interception policy changing; it is not a proven control, and I am not counting it as one.

**The "absent" rows.** No shim exists to remove, because the runtime never defined them. What the evidence supports is narrower:
(a) they are absent under the shipped plugin (`typeof` checks), (b) the plugin's `lockdown` shim would delete them if a newer Javy
added one, and removing `lockdown` changes nothing today (stated in `controls.txt` as "this control cannot fail today"), and
(c) the *check for absence* is shown able to disagree: injecting a global from user code makes it report `"function"`, `"present"`
and `0` (`inject …` lines). That is a control on the test, not on a shim.

### Not determined
- **What seeds QuickJS `Math.random`: determined by READING, not measured** (the plan asked). The QuickJS in `plugin/Cargo.lock`
  (rquickjs-sys 0.12.2) runs `js_random_init` when a context is created and sets `ctx->random_state = js__gettimeofday_us()`
  (`quickjs.c`, the `xorshift64star` generator); there is no `random_get` use. On wasm32-wasip1 `gettimeofday` is the WASI realtime
  clock. In static mode the context is created during Wizer pre-initialisation, so the seed is whatever the clock said **at build
  time** (a fixed value under `-C deterministic`) and every run continues the same sequence from the snapshot. I did not trace it at
  run time; the shim replaces `Math.random` regardless.
- **Timezone/locale.** `Intl` is absent. `new Date(0).toLocaleString()` is `"01/01/1970, 12:00:00 AM"` and
  `(1234567.891).toLocaleString()` is `"1234567.891"`, both replay-stable and not shimmed. Only a macOS host with no `TZ` was
  tried; a Linux worker, or a worker with `TZ` set, is untested.

### A bug the controls found in my own shim
The first `Math.random` shim used `BigInt.asUintN(64, …)` and returned values in **[-0.5, 0.5)**, e.g. `-0.323…`. Every
verdict I had written said "replay-stable" and passed over it, because stable is not the same as correct. A 400-draw range check
(`randomRange`: min ≥ 0, max < 1, mean 0.4–0.6, > 380 distinct) caught it; the shim now maps the signed i64 to unsigned by
adding 2^63 in BigInt. The range check is part of the verdicts for the shipped plugin and for two control builds.

## Static check (`tools/tscheck`)

Plan: `cmd/cleat/ts_build_refuses_nondeterminism_test.go` "in the same shape as its siblings". Built in the prototype's nested module
because there is no `cleat build --target ts` yet (Phase 5); it is wired into `build.sh`, so a workflow cannot be built without it.
Shape copied from `python_build_refuses_nondeterminism_test.go`: assertions on the report's TEXT (code, name, file:line), a known
positive and a known negative (either alone passes a checker that always or never refuses), a known-limit arm that PINS what it
does not see, and three outcomes with three exit statuses: **0 clean** (says how many tokens it scanned), **1 finding**,
**2 UNMEASURED** (an unterminated comment, string or template; an empty file; an unreadable file). `go run` collapses 2 to 1, so
`build.sh` builds the binary and runs it directly; both 1 and 2 stop the build.

**What it refuses** (codes TS001–TS006): `setTimeout`/`setInterval`/…, `WeakRef`/`FinalizationRegistry`, `performance`, `crypto`,
`SharedArrayBuffer`/`Atomics`, `fetch`/`XMLHttpRequest`/`WebSocket`/`process`/`require`, `Intl`. **What it deliberately does not
refuse:** `Date.now`, `new Date()`, `Math.random` (shimmed).

**What it does not see** (pinned by `TestKnownLimitsAreStillLimits`, so it cannot move unrecorded):
computed access `globalThis["set"+"Timeout"]`; an alias `const g = globalThis; g.setTimeout`; indirect `(0, eval)(...)`; a
dependency's own code (only the workflow file is scanned, not bundled packages); a regex literal containing a quote (the
tokeniser has no regex mode, which is why UNMEASURED exists). At run time the first of these throws a deterministic `TypeError`
(`setTimeoutComputed`, replay-stable): the runtime is the second layer.
**Known over-reports** (pinned by `TestKnownOverReportsAreStillOverReports`): a method *declaration* in a type literal
(`{ setTimeout(n: number): void }`) and a local that shadows a refused name (`const performance = 1`). Telling them apart from a
real call needs a parser, and the permissive error is the worse one. My first negative fixture contained the type-literal case
labelled as "a method on something else"; the test failed on it, and I moved the construct rather than loosening the checker.

**Red controls for the tests themselves** (`evidence/phase2/tscheck-mutants.txt`), each caught by the test expected to catch it:
rules emptied → positives fail; method calls on other objects refused → the negative arm and the limit arm fail; unterminated
comment treated as end of file (hiding a `setTimeout` behind it) → the UNMEASURED test fails. The restore is checked by content.

## Engine pin

The plan: pin the exact Javy/QuickJS build into the deployed version's metadata (`wasm/metadata.go`). `wasm.Metadata` has no such
field; adding one would be a change under `wasm/`. Instead `tsmeta -pin` appends a custom section `cleat.js-engine` holding
`{javy, rquickjs_core, plugin_sha256, sdk_sha256, deterministic_build}`, computed by `build.sh` from the real artefacts. **The stock
engine ignores a section it does not know, so this is not an engine mechanism, and it is also not enforced by the worker.**

What was measured (`evidence/phase2/pin.txt`, 13 of 13):
- The pin survives deployment: read back from `workflow_defs.wasm_bytes`, it equals the file's and passes the check. (Whole-file
  byte equality is NOT the test: deploy re-stamps the version in `cleat.metadata`, so stored bytes differ from the file for v ≥ 2.
  My first version compared whole files and passed only for v1.)
- `python3 phase2_pin.py check <module>` is the refusal tool. It **refuses**: a different plugin build, a module with the random
  shim removed, a module whose pin was edited, and a module with no pin section. It accepts the shipped one.
- **A bump does not move an in-flight run.** A run suspended on v*a* kept executing on v*a* (`def_version` read from the worker
  log) across the deploy of a changed engine as v*b*; a run started afterwards used v*b*.
- **Replay under a bumped engine is identical or refused, for a bump that changes a host-call input.** (Corrected 2026-11-13 on
  cleat-review's checkpoint: this bullet used to say "never silent", which is wider than what was measured.) The same persisted
  history, replayed in a Go harness on wasmtime: the recorded engine (twice) and a benign bump (different bytes, same behaviour)
  give identical outcomes; a bump that changes a host-call input (random shim removed, so the recorded `cleat_random` call is
  not made) is **refused with `replay divergence at step 0`**. **A bump that changes only pure computation feeding the result
  (nothing passed to a host call) replays without error and returns the new value.** That is consistent with how the engine
  compares a replayed call (service, operation and type, not the request bytes; PHASE3.md) but I did not run such a bump.
  The first version of this test reported a divergence even for the engine the run was recorded on, because the harness gave the
  replay a fresh random workflow id and the engine's random stream is derived per workflow. A replay must run under the run's own
  identity (`WithWorkflowID`, `WithDefName`, `WithDefVersion`).
- **Deploy identity (coordinator, decision 2).** A rebuild of unchanged source deploys as `WASM unchanged … (skipped)`. Red
  control: with `-C deterministic` off, the same source deployed twice becomes two new versions.

**The honest limit.** "Refuse" is delivered by tooling (build and deploy-time check), not by the worker. Because the engine is
statically linked into every module, a run cannot change engine underneath itself, which is why the bump test passes by
construction; what a worker-side refusal would add is protection against someone deploying a module with a wrong or absent pin.
*Estimate by reading, not built:* a `Metadata` field, a worker-side list of accepted pins, and a check beside
`warnIfModuleWantsANewerABI` (`engine/executor.go`). That function is documented as "WARNS, AND DELIBERATELY DOES NOT REFUSE"
(cleat#1054), so the precedent in this tree argues for warning first.

## Build reproducibility (coordinator, decision 2)

Cause found: Wizer pre-initialisation reads a clock and an RNG. `javy build -C deterministic` and `javy init-plugin
--deterministic` fix both. `evidence/phase2/repro.txt`: module, three builds → **1** hash with the flag, **3** without; plugin
binary, three builds → **1** hash with the flag, **3** without. Scope: one machine, one toolchain. A from-scratch `cargo clean`
rebuild, another path and another host are untested; the plugin variants were rebuilt incrementally.

## kill -9 in the middle of execution (coordinator, decision 3)

`evidence/phase2/midexec.txt`, 10 of 10. A busy loop with no host call (120M iterations), the worker killed about 3 s in after
the run was confirmed `running`; then restarted. Two shapes: the burn **after** a recorded side effect (1 persisted history row at
the kill) and **before** any host call (0 rows). Both completed, with the same computed sum as an uninterrupted run, were claimed
again after the restart, and logged exactly one completion. Times from kill to done were 59.6 s and 68.6 s; that includes the
lease expiry and a full re-execution, and I did not separate them. The two baselines differ (8.5 s and 17.1 s) because the second
shape re-burns on its post-sleep replay; *inference, not tested:* a sleep's deadline is anchored to the last recorded event, so in
the first shape the burn time has already passed it.

## The Go control and cleat#3779 (coordinator, decision 1)

The sleep → signal → child sequence on a Go guest logs the short-history WARN **10 of 10**, the JS guest 10 of 10, interleaved in one
session, one worker, all `done` and correct, `event_count=6` against 4 persisted rows (`evidence/phase2/go-control.txt`).
Per the coordinator's rule it is an engine behaviour, filed as **cleat#3779** (unclaimed, WS-cleat2 as filer, cites #3391/#1507).
The issue was **corrected** after I found my harness had been wrong; see the next section.

## Harness defects found in this phase (so they are not repeated)

- **Stale workers.** Scripts that never stopped their worker left it alive; the next script's `/healthz` answered from the old
  worker, two workers shared the database, and log-derived counts were short. Effects: Phase 1's "3 of 5" and "unexplained
  variance" (wrong, corrected in PHASE1.md and in #3779), a Go control that read 0 of 10, and mid-execution claim counts of 0.
  Found because an end-to-end run left two `cleat-worker` processes alive. Fixed in `phase1_lib.py`: stop stale workers on
  construction, refuse to run if one survives, refuse to start if `/healthz` already answers, stop at exit via `atexit`;
  `phase2-verify.sh` flags a leak between steps.
- A `Math.random` shim returning negative numbers (above); a verdict on "removal changes nothing" that compared live values and
  reported noise; an absence check "noticing" my own `TypeError` rather than a present global; a computed-property check that
  expected a quoted string; a "no pin" control that used a module later rebuilt with the pin (now built by stripping the section).
- A completion check that read the worker log the instant the API said `done`, before the line existed (now waits, bounded).
- Hard-coded `def_version` 1 and 2 against a database that persists across runs (now taken from the deploy output).
- `phase1_replay.py` ran every test in the replay package, so the new bumped-engine test skipped and the skip detector failed the
  step (now `-run` the one test).

## Not measured

- `Math.random` seeding source; timezone/locale beyond a macOS host with no `TZ`; JSON key order and NaN (Phase 3).
- Reproducibility across machines, paths and a from-scratch cargo build.
- A worker-side pin refusal (estimate only). Dynamic mode (not built, by decision).
- The static checker on bundled third-party code (Phase 5's npm packages); it sees only the workflow file.
- Kill timing finer than "about 3 s into a 12 s loop".

## What to check at checkpoint 1

1. `evidence/phase2/controls.txt`: the `off-random` rows FAIL with a divergence and the `full` rows pass; the `off-date` rows are all
   stable (the row that does NOT meet the pass line); 12 of 12 verdicts, including the range check.
2. `evidence/phase2/pin.txt`: the three engines in T3, and the four refusals in T1.
3. `evidence/phase2/tscheck-mutants.txt` and `tscheck-tests.txt`.
4. That the Date finding is reported as a finding and not folded into "controls pass".
5. `PHASE1.md`'s corrected observation 1, and cleat#3779's correction comment.

## Layout additions

    prototypes/ts-javy/
      PHASE2.md  phase2-verify.sh  build-all.sh  build-plugin-variant.sh
      phase2_repro.sh  phase2_controls.py  phase2_pin.py  phase2_midexec.py  phase2_go_control.py  phase2_tscheck_mutants.sh
      plugin/src/shims.js             the determinism shims (gated by CLEAT_SHIMS_OFF for controls)
      workflows/{nondet,busy}/        the probes
      gocontrol/                      the Go twin (for cleat#3779)
      tools/tscheck/                  the static check, its fixtures and tests
      evidence/phase2/                raw output of the final run


## Corrections after checkpoint 1 (cleat-review FINAL OK on f8674aca, seven corrections; all folded in 2026-11-13)

| # | Asked | Done | Evidence |
|---|---|---|---|
| 1 | build from a fresh checkout | `bootstrap.sh` builds every input from the committed tree and now FAILS when a binary was not built from the committed HEAD (it exited 0 before) | `evidence/phase2/bootstrap.txt` |
| 2 | Date facts as verdicts, with a known-positive | verdicts on VALUES for both `full` and `off-date`: `Date.now() === ctx.now()` is `true`; `dateElapsed` and `newDateElapsed` are `false`; `ctx.now()` reads 0 across the busy loop; known-positive `dateNative`: `false` on full, `true` on off-date and off-all. **One finding against the ask, below.** | `controls.txt` |
| 3 | inject `crypto` and `setInterval` too | all seven absence sources now have an injected known-positive (added `setInterval`, `crypto`, `setTimeoutCall`) | `controls.txt`, `inject` lines |
| 4 | the random shim must fail loudly | it now throws `cleat: Math.random: the host did not return a number: …`. Run against a plugin built with an injected fault (`CLEAT_FAULT=random-nonnumeric`, a host answering `{"suspended":true}`): strict build throws; the same build with the strict check removed returns `0`, replays identically and the run completes (the silent case) | `controls.txt` |
| 5 | wording | the replay-bump bullet above and the Date paragraph above are corrected. **The Date wording asked for was not what the probe measured:** time is fixed between host calls (busy loop: 0) but ADVANCES at a host call and replays identically | `controls.txt` |
| 6 | plugin hash independent of build path | **done**; cause was not what was guessed. See below | `evidence/phase3/pathindep.txt` |
| 7 | midexec baseline too long | `ITERS` 120M to 60M (about 6 s unloaded); the kill is still sent 3 s in, so the run is still executing | `midexec.txt` |

**Item 6, the cause.** `--remap-path-prefix` was tried first and changed nothing (same source, two directories, two hashes; 149,317
bytes differ, and with `-C strip=symbols` as well 129,398 still do). The difference is in the code, not in strings: `plugin/Cargo.toml`
reached the repository's Rust SDK as `path = "../../../crates/cleat-sdk"`, which lies **outside** the plugin's workspace root, and
cargo hashes such a dependency by its absolute path. That hash goes into every symbol and into the order the linker lays things out.
Reaching the same crates through symlinks **inside** the workspace (`plugin/vendor/cleat-sdk`, `plugin/vendor/cleat-macro`, relative
symlinks, committed) removes it: one hash from two directories with no remapping at all. `phase3_pathindep.sh` rebuilds the plugin
from a copy in a different directory and compares; **its known-positive** rebuilds the copy with the old dependency form and must
differ (it does). A remap of the cargo home is still passed (`plugin-cargo.sh`) so a user's name is not embedded in panic strings;
that is untested across users and only affects strings. **Not shown: two machines, two users, or a different CARGO_HOME.** What is
shown is two directories, one user, one machine. `plugin-cargo.sh` is the one place the plugin is compiled; `build.sh` used to
continue after a failed plugin compile (`|| true`) and so could init a stale artefact; it no longer does.
