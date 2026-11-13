# Phase 4 — measurement, static mode (branch `proto/ts-javy`, remote `feature/proto-ts-javy`)

Prototype only. No PR to `develop`, no `tiers.yaml` entry, no support claim anywhere. Plan:
`cleat-internal/typescript-javy-prototype-plan-2026-11-08.md`, Q5 and section 5. Dynamic mode is **blocked** (it needs an engine change,
PHASE1.md "Dynamic mode"); only its file sizes are measured here, and nothing about dynamic mode was run.

## What produced these numbers, and how far to trust them

- **One run of `phase4-verify.sh` in a clean `git worktree` of commit `a223d39a`**, on its own PostgreSQL container, database and API
  port. All four steps exited 0. `evidence/phase4/provenance.txt`: worker and `cleatctl` built from that commit, `RESULT: every binary was
  built from the committed HEAD of a clean tree`; `inproc.json` records `tree_dirty_outside_evidence: false`.
- **The machine was not quiet, and I could not make it so.** Apple M4, 10 cores, macOS 26.5.2, shared with other sessions and OrbStack:
  the 1-minute load average was **10 to 15** throughout (`load1_at_start` 15.2, `load1_at_end` 13.4; recorded per sample in the JSON).
  So absolute times are indicative. What I did about it: languages are **interleaved** across rounds (5 rounds in-process, 3 for the
  database half) so drift hits all alike, and the best and worst round are reported beside each median. The in-process spreads are
  small (for every in-process metric the worst round median is within 23% of the best; no ratio changes sign in any round). A quiet-machine rerun would tighten the absolute figures; I do not expect it to change a threshold verdict below, but that is an
  expectation, not a measurement.
- Software: Javy 9.1.0 (static, plugin `bce47c28…`), wasmtime-go v48.0.0, rustc 1.96.0, go 1.27.3, componentize-py 0.25.1.

## The benchmark workflow (the same in four languages)

`bench_steps({n, sleep_ms})`: `n` recorded `sideEffect` host calls, then, if `sleep_ms > 0`, a durable sleep. Twins:
`workflows/bench/bench.ts` (TypeScript on static Javy), `bench/gobench` (Go via `cleat build`), `bench/rust` (via `cleat build --target
rust`, so it carries deployable metadata), `bench/python/bench_workflow.py` (componentize-py, via `cleat build --target python`).
`n=1` is the cold-start workflow, `n=1000` plus a 60 s sleep is the replay workflow, `n=10` with no sleep is the throughput workflow.

**My first draft used `random` host calls and measured nothing.** The harness reported `events_recorded_per_run: 0`: `random` is a stream
the engine derives per workflow, not a history event, so a "1,000-step history" would have been empty and every language would have looked
instantaneous. Found because the number was implausible (0.27 ms, 0 events) and I looked at the events before the time. All four twins
use `sideEffect`, which records one event each, and the harness refuses to continue unless the history holds at least `n` events and the
run suspended on the sleep (`1000` events, `cleat_sleep(60000ms)` in every language: `inproc.json`).

## Section 5, filled in (static mode)

Medians of per-round medians; "best/worst" are the best and worst round medians. Raw: `evidence/phase4/inproc.json`, `db.json`.

| Metric | TypeScript (static Javy) | Go | Rust | Python | Threshold | Verdict |
|---|---|---|---|---|---|---|
| Module size as deployed | **1,122,163 B** | 4,890,563 B | 83,820 B | 19,394,396 B | dynamic ≤ 100 KB, engine ≤ 3 MB | see below |
| Stored row (`pg_column_size`, compressed) | 651,823 B | 1,836,595 B | 46,312 B | 9,172,724 B | | |
| Cold start, cached compile: instantiate + first host call + finish | **0.274 ms** (0.247–0.289) | 0.698 ms | 0.084 ms | 9.890 ms (9.156–10.833) | ≤ 2× Python | **met**: TS is 0.028× Python, 0.39× Go |
| First run after deploy, includes compilation | 170 ms | 323 ms | 14 ms | 1,171 ms | | |
| Replay of a 1,000-step history to the suspension point | **6.77 ms** (6.72–8.04) | 8.04 ms | 2.81 ms | 13.63 ms | ≤ 5× Go | **met**: TS is 0.84× Go |
| (live execution of the same 1,000 steps, for the ratio) | 6.45 ms | 7.47 ms | 2.47 ms | 13.63 ms | | |
| Peak linear memory on that replay (smallest limit under which it succeeds) | **≤ 13 MiB** | ≤ 9 MiB | ≤ 13 MiB | ≤ 24 MiB | ≤ 16 MiB | met for TS, **with the limit below**; Python fails the line |
| End-to-end throughput, 1 worker, PostgreSQL, 10 calls, no sleep | **17.1/s** (16.2–17.7) | 16.1/s | 18.0/s | 12.0/s (8.7–13.4) | report only | |

**Size, said plainly.** Static mode produces a 1.12 MB module: 4.4× smaller than Go's and 17× smaller than Python's, 13× larger than
Rust's. That is the Javy plugin (1,074,724 B, QuickJS plus the SDK bindings) carried inside every module. The plan's pass line is a
**dynamic-mode** line (≤ 100 KB per workflow, ≤ 3 MB shared engine). Measured file sizes in dynamic mode: the module is **5,398 B** and the
shared plugin is **1,074,724 B**, so on paper both are met. **But dynamic mode was not run**: the stock engine would refuse it ("module
has no exported memory", PHASE1.md), so this is a statement about two files, not about a system that works. Static mode has no ≤ 100 KB
claim to meet or miss. PostgreSQL compresses the stored row by 0.38 to 0.58.

**Cold start.** In-process, on the wasmtime backend the worker uses, with the compiled module cached (`engine/backend_wasmtime.go`'s
module LRU). The workflow makes one `sideEffect` and returns, so this is instantiate + one host call + completion, not "first host call"
alone: the engine exposes no hook at that instant and `random` (which would not need one) records nothing. The compile cost
("first run after deploy") is separate and large for every language but Rust; it is paid once per worker per module version.

**Replay.** `engine.Replay` of the 1,000-event history captured from a live `engine.Execute` of the same input, 20 timed samples per round
(10 for Python), fixed workflow id (the engine derives the random stream from it, so a replay under a different id is a different
computation). In-process, so **no database read of the history** is in it; that cost is the same for every language. Replay is almost
exactly as fast as live execution because live execution here has no network or database either.

**Peak memory.** The engine has no gauge, so this is found by search: replay under a wasmtime linear-memory limit, 1 MiB steps up to the
engine's 32 MiB ceiling, smallest limit that succeeds. A known-negative guards the search (one MiB below the answer must fail).
Probes are in `inproc.json`. **Limit: the number for TypeScript and Rust cannot show the guest's own footprint.** For every non-Go module
the engine places two 1 MiB scratch buffers at no less than a fixed 10 MiB offset (`engine/memory.go`, `legacyScratchOffset`), so 12 MiB is
the floor the engine imposes whatever the guest does, and both land at 13. What the guest itself needs is lower than that and not
resolved. Supplementary: the modules *start* with 22 pages (1.4 MiB) for TypeScript, 17 for Rust, 50 for Go, 376 (23.5 MiB) for Python
(`wasm-tools print`). Python's 24 MiB is its real footprint, because its own heap exceeds the floor; it is the only language over 16 MiB.
The engine also records `workflow_memory_samples`, which I read and did not use: it is a host-side delta, not guest linear memory.

**Throughput.** 200 workflows per round (60 for Python) started by 8 client threads against one stock `cleat-worker`, polled to
completion; all runs `done` with the right result in every round (each of 1,980 workflows was claimed exactly once and completed once).
The three fast languages are **indistinguishable within their spread** (16–18 per second), which says the figure is bounded by the
worker's claim and database path (its poll interval is 200 ms, `-poll 200ms`), not by the guest. I did not vary the poll interval, so that
is a reading of the result, not a measurement. Python's lower figure (12/s) has the widest spread (8.7 to 13.4) and a smaller sample.

## Observations

1. **One start request was refused, once.** In the first full run, round 3 of the Python guest, `POST /api/workflows/bench_python/start`
   answered `404 {"error":"workflow definition not found"}` while the definition was in `workflow_defs` (`version 1`, not disabled; the
   handler returns that message when `ListVersions` returns no versions, `cmd/cleat-worker/server.go`). Rounds 1 and 2 of the same
   language, and every later request, were fine. The worker's log for that run carries "recent database trouble on this worker"
   warnings from 10 seconds after it started. **Not reproduced** in the rerun that produced the numbers above (3 rounds, 1,980 starts,
   0 refused), and **not explained**. I did not retry it away: the driver now records every refused start and fails the step. The raw
   output of that first run went with the worktree I recreated for the rerun, so the excerpt here is from the console, not a file in
   `evidence/`. Worth a look by whoever owns the start path, because a deployed definition vanishing for one request under load is the
   kind of thing a retry in a client hides.
2. **The worker warned during both runs** (`evidence/phase4/db.json`, `worker_warn_error_lines`): 18 "execution fenced out: run superseded
   by a later generation" (with `sent_generation` equal to `generation`: 7 TypeScript, 6 Go, 1 Rust, 4 Python, so not a guest effect);
   4 "Reaper: skipping this tick -- recent database trouble on this worker"; 1 "Reaper: skipped its last several ticks in a row"; 1
   "this worker's pools may open 74 of the 97 connections the server allows". No run was lost or duplicated, but the machine was loaded
   and these are likely related to that. Not investigated.
3. **Python module size varies between builds of identical source** (18.4 MB, 19.3 MB, 19.4 MB across my builds), as the repo's own
   comment on componentize-py says. The figure above is the module that was deployed and measured.

## Not measured (limits of this phase)

- **Dynamic mode, at all** (blocked). Only the two file sizes above exist.
- **The optional StarlingMonkey comparison** (owner's call; not done).
- Cold start through the **worker** (process start, API, claim): the harness is in-process. The throughput run includes all of that but
  does not isolate cold start.
- A **quiet** machine, a second machine, a second architecture, a Linux Javy build.
- Guest-only peak memory for TypeScript and Rust (floor set by the engine, above).
- History **read from PostgreSQL** in the replay timing.
- Sensitivity of throughput to the worker's poll interval and connection pools.

## Re-deriving

    git worktree add --detach ../fresh <commit>
    cd ../fresh/prototypes/ts-javy
    CLEAT_PROTO_PG_CONTAINER=… CLEAT_PROTO_PG_PORT=… CLEAT_PROTO_PG_DB=… CLEAT_PROTO_API_PORT=… ./bootstrap.sh
    CLEAT_PROTO_COMPONENTIZE_PY_DIR=<dir containing componentize-py> (and the same four variables) ./phase4-verify.sh   # ~12 min

Without `componentize-py` the script exits 2 (UNMEASURED); it never drops Python quietly.
