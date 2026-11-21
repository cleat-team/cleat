# Keeping a suspended run resident on its worker (cleat#4162)

**Status: PARTLY BUILT, and OFF by default. The wake source, the admission policy and the flags exist (`engine/resident.go`, the `--resident-*` flags, cleat#4162 PR 1) and hold only a Go guest's durable sleep, and only when `--resident-max-runs` is above zero; a held run's wall-clock limits (the ceiling and the workflow deadline) and its execution fence are paused for the hold and restart at their full duration when it ends, as a suspended run's next segment would have fresh ones, and a cancel ends a held run at once (all measured on real wasmtime, see `docs/reference/worker-config.md`); eviction on a lost fence is built (a held run checks its claim before it returns to the guest, section 9b) and a kill while held is run in `tests/chaos`, where it showed every effect once and a delayed wake (40 to 41 s against 9.2 to 9.5 s with residency off, section 9b); the signal wake source and the other waits are still to come, and until the series merges nothing here is claimed as supported.** The rest of this document is still the proposal it was: nothing else in it is built, and nothing in it is claimed as supported (`tiers.yaml` is the source of truth for what is). It exists so the owner can choose a policy before any code is written. Written 2026-11-19 by WS-cleat-2. Every line of code it cites was read on develop `1157baf3` (re-derive a citation with `git grep -n '<symbol>' origin/develop`); a fact I could not establish is marked **NOT ESTABLISHED** rather than guessed.

## 1. The short version

- **A suspended run cannot "stay resident" today, because a suspend unwinds the guest.** Every durable wait makes the host call return a stop, the guest's call stack is unwound, and the instance has nothing left to continue. So "resident" can mean two different things, and they save very different amounts:
  - **PARK** (section 4): the wait **blocks inside the host call** and the instance stays exactly where it was. A wake continues the guest in place: no claim, no instantiation, no replay. This is what the owner described.
  - **WARM RE-ENTRY** (section 5): suspend as today, and keep things warm for the next claim (history in memory, a pre-built instance). The guest still replays, so this saves the database reads and some of the start-up, not the replay.
- **Recommendation: PARK, as an optimisation layered on the existing suspend, never instead of it.** Evicting a parked run is "return the stop the host would have returned anyway", so the fallback at every moment is today's behaviour, a crash recovers by replay exactly as now, and the history a parked run writes is the history a suspended run writes.
- **Recommended policy** (section 6, for the owner to choose): park only a wait whose expected wake is inside the window; window = `clamp(10 x estimated replay time, 10 s, 5 min)`; evict least-recently-used under memory pressure or at a resident-count limit that is **small by default because a parked instance holds an OS thread** (section 4.6).
- **Three things had to be measured before the constants are chosen** (section 9). Two now are: the memory of one parked instance (section 9a) and whether blocked host time counts against the Python component's instance fence (section 4.5; it did, measured since this was written: cleat#4174, fixed in cleat#4179). **Still not measured: what the 0.4 to 0.7 s per claim is made of.**

## 2. What happens today (facts, with where they were read)

| | |
|---|---|
| A wait suspends and the guest unwinds | `DurableSleep` sets `suspendErr` and returns a suspend result (`engine/durablecalls.go`); `await_signals`, `await_promise`, `await_child` and `wait_for_event` return `callSuspendSentinel` (`engine/signaller.go`, `engine/promises.go`, `engine/wait_for_event.go`). The Go guest panics `ErrSuspend` (`wasm/adapter_metadata.go`), Python raises on `CALL_SUSPEND_SENTINEL` (`python-sdk/cleat_sdk/memory.py`), the component `run` export returns `runOutcomeSuspended` (`engine/component_cgo.go`). The executor builds a `SuspendResult` and the worker writes the segment's events and releases the row with `next_wake_at` (`cmd/cleat-worker/setup.go`, `FinalizeWorkflowSegment`). |
| The next wake is a claim | `ClaimWorkflows` takes `status IN ('ready','terminating') AND next_wake_at <= now()` with `FOR UPDATE SKIP LOCKED` and sets `assigned_to`, `heartbeat_at` and `generation = generation + 1` (`engine/store_lifecycle.go`). There is no lease-expiry column. |
| How the claim loop learns of work | Polling: `--poll` 500 ms backing off to 3 s idle (`cmd/cleat-worker/config.go`, `setup.go`). **PostgreSQL only** also has `LISTEN`/`NOTIFY` (`cmd/cleat-worker/notify.go`, `engine/store_notify.go`), sent on signal delivery, promise resolution, child start and `finalize_workflow_status`. MySQL and SQL Server have no notification and poll for everything. |
| Heartbeat and fencing | One worker-level loop heartbeats everything in `w.inflight` every `--heartbeat` (5 s) with a batched, generation-checked `HeartbeatBatchFenced` that requires `status = 'running'` (`setup.go`, `engine/db.go`). The reaper reclaims `status = 'running'` rows whose `heartbeat_at` is older than the stale window (about 47 s by default), sets `ready` or `terminating`, nulls `assigned_to` and bumps `generation` (`ReapStaleInstances`). **A suspended run is `ready` with `assigned_to` NULL, so the reaper never sees it.** The fence is `(assigned_to, generation)` on `FinalizeWorkflowSegment`, `ReleaseWorkflow`, the heartbeat and `flushEvent`. |
| Wake sources | Timer: `SuspendError.Until` becomes `next_wake_at`. Signal and promise: a store method sets `next_wake_at = now()` and, on PostgreSQL, notifies. Child finished: `finalize_workflow_status` wakes the parent. **Cooperative cancel has no wake and no notification** (`RequestCancellation` only sets a flag; it is read by `PollCancellation` on the next fresh call). **Terminate** (`preemptivelySettle`, `engine/db.go`) bumps `generation`, nulls `assigned_to`, and for a run that owes defers sets `status = 'terminating'`, again with no notification. |
| Limits | `--wasm-instance-timeout` (an epoch deadline), `--wasm-wall-clock-ceiling` (default 5 min, a context timeout around one execution) and `--max-workflow-duration` are all **per segment/invocation**: a run that suspends and resumes gets a fresh budget each time. |
| Concurrency and memory | `--concurrency` (default 10) minus the size of `w.inflight` is what a claim may take; `MemoryController` scales it down under pressure. Per-run accounting is only the process-memory delta around one `executeWorkflow`, kept as an average per (tenant, definition). The guest memory limit is 256 MiB by default since cleat#4200 (`--wasm-memory-max-mb`; it was 32 MiB, with a 64 MiB floor for C#, when this section was written). A compiled Python component is about 46 MB shared by all instances (`docs/reference/worker-config.md`). **The size of one Python instance is NOT ESTABLISHED.** |
| Shutdown | `gracefulShutdown` sets `draining`, waits until `inflightCount() == 0` for `--shutdown-grace` (20 s), then cancels the hard-stop context so durable calls abort and runs suspend and requeue. |
| What is already cached | The compiled component and module bytes (`newWasmLRUCache`, the compiled-module LRU, `--wasm-cache-dir`). **There is no instance pool**: every execution builds a new linker and store and instantiates, which for Python runs CPython's own module initialisation (`engine/component_cgo.go`). |

**Measured baseline (2026-11-19, mine, not a benchmark):** a real worker harness (Python component, wasmtime, PostgreSQL on the same host), a loop of one durable step plus a 150 ms sleep, 120 iterations: **105 claims** (one per iteration, minus the few that completed inline), **0.44 to 0.67 s per claim** with no growth over the run (poll loop 671 to 519 ms, durable-call loop 436 to 436 ms), so each wait costs about 0.3 to 0.5 s over the wait itself. The harness claims immediately; a production claim loop adds the poll interval (up to 3 s idle) on MySQL and SQL Server and for anything PostgreSQL does not notify. **What the 0.4 to 0.7 s is made of is NOT ESTABLISHED**: the worker logs `setup_ms`, `replay_ms` and `finalize_ms` at debug level and I did not read them.

## 3. What a design has to preserve

1. **Durability does not change.** Events are recorded per step, a crash recovers by replay, and the history a resident run leaves is byte-for-byte the history a suspended run leaves.
2. **The fallback is always today's behaviour.** If anything about residency is wrong, evicting must be possible without losing the run.
3. **A resident run is fenced like a running one.** Another worker must not be able to run it, and this worker must not write for it after it has lost it.
4. **Nothing user-visible depends on it.** Same results, same events, same timeouts; only latency changes.

## 4. Option PARK (recommended)

### 4.1 The mechanism

The wait host calls (`DurableSleep`, `await_signals`, `await_promise`, `await_child`, `wait_for_event`) today set `suspendErr` and return a stop. With residency, **when the run is eligible and the budget allows, the host function blocks** on a per-run wake channel until the wake condition or the window expires:

- the condition arrives: the host function **returns the call's result as if the wait had completed inline** (the same value a replay would return), records the same event, and the guest carries on;
- the window expires, the run is evicted, the worker drains, or a fence/cancel check fails: the host function returns **today's stop** (`callSuspendSentinel`, with `suspendErr` set as today), the guest unwinds, and the worker releases the row exactly as now.

There is precedent for a host call that blocks: host-side retry backoff waits inside the call (`engine/durablecalls.go`, `--host-retry-budget`, default 60 s), service calls block, and WASI `poll_oneoff` really blocks (`engine/wasmtime_poll_oneoff.go`). There is **no precedent for blocking for minutes**, which is what sections 4.5 and 4.6 are about.

### 4.2 Staying claimed

Three ways, in order of how much they change:

| | what changes | cost |
|---|---|---|
| **a. Stay `running`** (recommended) | Nothing in the schema. The run stays in `w.inflight`, keeps its `status = 'running'` and `generation`, and the existing heartbeat keeps it from the reaper. A worker that dies is recovered by the reaper after about 47 s and replays, exactly as for any running run. | A resident run is indistinguishable from a busy one in the API and the dashboard, and it occupies an `inflight` slot, so it needs its own accounting (4.7) or parked runs starve `--concurrency`. |
| b. A new status `resident` | Schema, the heartbeat and reaper predicates, the dashboard. | Honest to operators; every consumer of `status` has to learn it. |
| c. Keep it `ready` and hold a local reservation | Another worker can claim it, so the reservation needs a lease column and a fence. | Duplicates the claim protocol; rejected. |

Recommendation: **(a) now, with a `resident` flag exposed through metrics and the run's API representation**, and (b) only if operators need the distinction in the status itself.

### 4.3 Waking

- **A timer** wakes a parked run from a local timer at its `Until`, not from the poll: the lateness of a timer that today is up to about 3 s becomes the timer's own resolution.
- **A signal, a promise, a child, an update:** (an update's `update_received` event is written by the API side, so it is a wake source too, and one I left out of my first draft) on PostgreSQL the existing `NOTIFY` reaches the worker's listener, which maps the run id to its parked channel. On **MySQL and SQL Server there is no notification**, so the worker runs one **batched cheap check** per resident set on a short interval (the same sequence columns the claim path already compares, `signal_seq` / `promise_seq`, and the child's status): a query over the resident run ids, not a replay. This is the design's main extra cost on those two dialects and has to be measured.
- **Cooperative cancel and terminate have no wake today.** A parked run would not see them. They are noticed at the next heartbeat tick (5 s): a terminate bumps `generation` and nulls `assigned_to`, so the fenced heartbeat reports the run lost; cancel needs the batched check above to read `cancellation_requested`. Either way the worker evicts, which for a lost fence means **abandoning the parked instance without writing** (it no longer owns the run).
- **Heartbeat presumed lost** (`engine/durablecalls.go` refuses fresh calls) must make a parked run evict too, for the same reason.

### 4.4 Which waits are eligible

| wait | eligible? | note |
|---|---|---|
| durable sleep with a finite `Until` | yes **if `Until` is inside the window**; a three-day sleep is never parked | the common case of the baseline measurement |
| `await_signals` / `await_promise` / `await_child` with a timeout | yes, park for `min(timeout, window)` | on expiry within the window the result is the timeout, as inline |
| the same with no timeout | yes, park for the window, then evict | the unbounded wait is the main beneficiary and the main memory cost |
| `wait_for_event` | yes, with its total budget | it has its own contract (a total budget); keep it |
| a zero-or-less timeout poll | never | it does not wait (cleat#4144), so there is nothing to park |
| a continue-as-new, a completed run | never | not waits |
| **anything inside a defer segment** (`deferPhase`) | never | a segment is a fresh claim that replays to the last stoppable event on a new instance, so it is never on a parked one; a wait in its drain suspends as today. A terminate or cancel evicts the parked instance (its fence is lost) and the segment is claimed fresh |
| a durable call's retry backoff | already blocks | unchanged |

### 4.5 The execution limits (the part most likely to bite)

- **`--wasm-wall-clock-ceiling` (default 5 min)** is a context timeout around one execution, so a parked run is inside it. A window of 5 minutes **is the ceiling**: a run parked for the full window dies of the ceiling, not of eviction. The ceiling must be paused while parked, or rebased at wake.
- **`--wasm-instance-timeout` (the epoch fence).** On the core-module path the host charges the fence only for guest time (`hostBudget`, `engine/wasmtime_hostbudget.go`, bracketed in `b.hostFunc`), so a blocked host call is free. **On the component (Python) path I found no `hostBudget`**: `configureStore` sets a fixed epoch deadline (`engine/backend_wasmtime.go`) and the component callbacks do not bracket host time, so by reading, **blocked time counts against the 30 s instance timeout on exactly the path with the most to gain**. This contradicts the flag's help text (`cmd/cleat-worker/config.go`). **Measured after this was written, and it was true** (cleat#4174): on a stock worker a Python durable call to a service that took 25 s completed and one that took 40 s failed after 44 s with `execution time limit exceeded (30s wall-clock budget)`. The same code path serves C#. The fix, cleat#4179, brackets host time in the component callback dispatcher as `b.hostFunc` does on core, so a parked Python run's blocked host call is no longer charged to the fence (merged 2026-11-20).
- **`--max-workflow-duration`** is per segment: parked time must not count, or it becomes a hidden run-length limit.

### 4.6 What a parked run costs

- **An OS thread.** A host call made through the wasmtime C API blocks the calling thread inside C (cgo); that thread is not available to Go's scheduler for the time it blocks. **A thousand parked runs is on the order of a thousand blocked threads**, and section 9a measured it: 1053 threads for 1024 parked Go instances. **That did not turn out to bound the resident count**: no trend in a 1 ms ticker's lateness was visible up to 1024 parked, so on that evidence memory bounds it and not threads. The default limit should still be small (tens, not thousands) until the stage-1 build is measured under load, because 9a measured the Go scheduler's responsiveness on one 10-CPU Linux VM, not claim or database throughput with instances parked.
- **Memory.** Each instance keeps its linear memory (capped at 256 MiB by default since cleat#4200; it was 32 MiB, with a 64 MiB floor for C#, when this section was written) plus the `wasmtime.Store`, plus whatever CPython holds. The compiled module (46 MB for Python) is shared. **The per-instance figure is NOT ESTABLISHED**; it is the number that sets the resident budget.
- **One process-global to check before building:** `cbRegistry` in `engine/component_cgo.go` stores a single `store`/`storeID` and, by my read, `registerCB` appends and nothing deletes. If a parked instance outlives a segment, anything that assumes one live store per process is wrong. I did not check what reads it.

### 4.7 Accounting

- A **resident budget separate from `--concurrency`**: a parked run is not executing, so it must not occupy a slot, but it is in `w.inflight` for the heartbeat. Count executing and resident separately and admit claims on the executing count.
- **Eviction order: least recently used**, triggered by the resident-count limit, by the resident-memory budget, and by `MemoryController` pressure (a resident run is the cheapest thing to give back: eviction is a return of the stop, no work lost).
- **Shutdown:** at drain start every parked run is evicted (stop returned, row released), so the drain does not wait on a window.

### 4.8 Failure and recovery

| event | result |
|---|---|
| worker killed or crashed | the reaper reclaims the run after about 47 s and it replays: **the same as today** |
| database unreachable while parked | the heartbeat fails, fresh calls are refused as presumed lost, the run evicts |
| the fence is lost (terminate, cancel, reaper) | **the heartbeat reports it within one `--heartbeat` and evicts without writing; and the run checks the store itself every time a wait ends, before it goes back to the guest.** The second part was NOT in this design and was added after a measurement (section 9b): a run whose timer fired before the heartbeat did acted after it had been terminated, which a suspended run cannot. The other owner proceeds from the durable history |
| a wake arrives as the window expires | the run is evicted and released with `next_wake_at = now()`: a normal claim, nothing lost |
| the host goroutine/thread leaks | the resident limit and a watchdog (a parked run older than window + margin is force-evicted) |

### 4.9 Fit with the central step dispatcher (cleat#4170)

WS-cleat3's design (`docs/design/replay-step-dispatcher.md`, draft PR #4170) moves every host call's replay, refusal, recording and counting into one `step()`. Residency changes **when** a replay happens; the dispatcher decides **what** a step does. They fit, with three requirements on the dispatcher:

1. **`position()` (`InHistory | AtFrontier | Fresh`) needs no fourth state.** A wait is reached at the frontier, so the session is already `Fresh`; under PARK it blocks and, after the wake, is still `Fresh`. Nothing replays. A resident session is a `Fresh` session that lives longer.
2. **Residency belongs in one stage, and the dispatcher's own design already names it.** The `Wait` kind's `Fresh` returns `StillWaiting{until, wake}`, and the dispatcher turns that into the suspension today. Make it **park-or-suspend**: if the run is eligible and the budget allows, block on the wake and return `Done` through the same `Decode` a replay would use against the live store; otherwise return the stop as now. Then a spec says only what it waits for, and residency is implemented once. If `Wait` stayed outside the dispatcher, residency would have to be implemented in every wait's helper.
3. **A wait must record its `await_*` event BEFORE it blocks.** Durability requires that a crash while parked finds the event and its `TimestampMs` anchor in history; recorded after the wake, a recovery would re-run the wait and restart its timeout clock. So `Wait` records inside `Fresh` (as the design already has `Stream` do) or returns the record through a pre-block hook. `DurableSleep` records no event and stays outside the dispatcher, but its last step, "set `suspendErr` and return the stop", must go through the same park-or-suspend helper, or a sleep can never park.

A defer segment's body is never resident (section 4.4), so "a body past the end of history" keeps its meaning and the dispatcher's refusal rule does not change. The fault the harness (cleat#4169) needs for this is **a kill while a run is parked**, which today does not exist as a boundary.

## 5. Option WARM RE-ENTRY (cheaper, saves less)

Keep today's suspend, and make the next claim cheaper: cache the loaded history in memory for a window (skip the reads), and/or keep a pre-built instance for the next claim of the same definition. **The guest still replays**, so what is saved is the database reads and, with an instance pool, part of instantiation (for Python, CPython start-up). It needs no blocking host call, no thread per run, no change to the heartbeat, the reaper or the limits, and an unclaimed cache entry costs memory but no thread. It does not remove the claim: the poll lateness, the `ClaimWorkflows` round trip and the replay stay.

Use it as **stage 1** if the owner wants a result sooner: it is independent of PARK, and its measurement (how much of the 0.4 to 0.7 s is reads and instantiation) is the same measurement PARK needs.

## 6. The policy, with options for the owner

The owner's starting point: about 5 minutes by default; refined to about 10x the run's estimated replay time, estimated from its event count; evict under memory pressure or at a count limit, least recently used first.

| option | the window | what it favours | what it risks |
|---|---|---|---|
| **A. fixed 5 minutes** | the same for every run | simple; a wake inside 5 minutes is a hit | a cheap run holds a thread and memory for 5 minutes to save 0.4 s; the resident set is the number of runs that suspended in the last 5 minutes |
| **B. 10x estimated replay** | `10 x (fixed start-up + k x events)` | runs that are expensive to replay stay longer, cheap ones leave quickly, so the resident set tracks the benefit | with the baseline above (0.44 to 0.67 s per claim) 10x is **4 to 7 seconds**: a typical wait longer than that gets no benefit at all, so the hit rate may be low. The estimate needs the cost model (section 9) |
| **C. A or B, but only if the expected wake is inside the window** | `Until - now <= window` for timers | never parks a wait that will not wake in time | does nothing for an open-ended signal wait, which is the case that matters most for entity-style workflows |
| **D. B with a floor and a cap, plus C** | `clamp(10 x estimate, 10 s, 5 min)`; timers parked only if due inside it; unbounded waits parked for the window | tracks benefit, keeps a useful minimum, bounded above | three constants to choose from measurement |

**Recommendation: D**, with the resident-count limit small by default (section 4.6) and raised on evidence. The floor exists because 10x of a Go run's tiny replay estimate is shorter than the time a typical signal takes to arrive, which would make residency inert for exactly the runs that are cheapest to hold. **I have no measurement that says 10 s is the right floor; it is a proposal for the owner to adjust, not a result.**

Decisions the owner is asked for:
1. A, B, C or D, and the constants (floor, cap, multiplier).
2. The default resident-count limit (and whether to expose `--max-resident-runs`, `--resident-memory-budget-mb`, `--resident-window`).
3. Whether the resident run is shown as `running` (4.2a, the recommendation) or as its own status (4.2b).
4. Whether WARM RE-ENTRY (section 5) goes first as a smaller stage.

## 7. Metrics

`cleat_resident_runs` (gauge), `cleat_resident_wakes_total{source=timer|signal|promise|child}`, `cleat_resident_evictions_total{cause=window|pressure|count|drain|fence|cancel|watchdog}`, `cleat_resident_hits_total` (woken while resident) against `cleat_resident_misses_total` (a wait that was eligible and was not parked, by reason), `cleat_resident_replay_seconds_saved_total` (the replay time the run would have paid, from the same estimate the policy uses), `cleat_resident_memory_bytes`, and the cost side: `cleat_resident_check_queries_total` for the MySQL and SQL Server batched check.

## 8. Rollout

Off by default, per worker (`--resident-window 0` is today's behaviour), so a canary worker can run it beside unchanged workers on one database: nothing in the history or the row changes, and a resident run on a worker that dies is recovered by replay on any other. No migration is required by option 4.2a.

## 9. What has to be measured first (items 2 and 4 are measured, section 9a; item 3 is done, cleat#4174; the rest are not yet)

WS-cleat1 will run the fuller latency benchmark; these are the numbers the design depends on, in the order they decide things:
1. **What the per-claim 0.4 to 0.7 s is made of** (claim, history load, instantiation, replay, finalize) for Go, Python and C#, on all three databases: the `setup_ms`/`replay_ms`/`finalize_ms` debug log is the starting point. This decides whether PARK or WARM RE-ENTRY buys the most, and it fixes the cost model for policy B.
2. **Memory of one parked instance** for Go, Python and C# (resident set before and after parking N runs). **Measured, section 9a.**
3. **Blocked host time against the Python instance fence** (4.5): **done**, as cleat#4174 (it was charged; a 40 s call failed after 44 s with the 30 s fence's error) and its fix cleat#4179 (merged 2026-11-20), which also covers plugin and streaming plugin calls and a C# guest. What is still unmeasured here is a PARKED run (as opposed to one slow call) across the fence.
4. **Blocked cgo calls**: how many parked host calls a worker tolerates before scheduling suffers (4.6). **Measured up to 1024 parked, section 9a.**
5. **The MySQL and SQL Server batched-check cost** per resident set size.
6. **The hit rate**: of real waits in a representative workload, what fraction wake inside 10 s, 1 minute and 5 minutes. This is the number that chooses between options A and B and it needs real wait-length data, which I do not have.

## 9a. Stage-0 results: what a parked instance costs (measured 2026-11-19)

Items 2 and 4 of section 9, measured. **This is the cost of an instance blocked in a host call, which is what a parked run is.** It is not a measurement of PARK itself (nothing here parks a run; the instances simply never return from their first durable call until released), and the guests are minimal, so read the per-instance figures as what a parked instance costs BEFORE the workflow has built up state of its own, plus the garbage case below.

**Method.** One process per configuration (so the allocator's retention from one run cannot leak into the next), in the Python toolchain container (Linux, Docker on a macOS host, 10 CPUs, 12.6 GB). The engine runs with a wasmtime backend with an EXPLICIT per-store memory cap (so a language's default floor does not apply), and each of N guests makes one durable call to a service that does not answer until released. One instance is first run to completion, which compiles and caches the component; then the baseline RSS is read from `/proc/self/status`; then N instances are started and, when all N are blocked in the call, RSS, thread count and a scheduling probe (a 1 ms ticker over 3 s) are read. **Per-instance cost = (RSS with N parked minus the baseline) / N.** The probe is a scratch test, not in the tree; it is described here so it can be rebuilt. Its source, the churn guests and the raw output of every run behind the tables below are kept in the cleat-internal checkout under `stage0-probe/` (`results/` holds the nine raw outputs).

**Parked instance, minimal guest (one durable call, nothing else):**

| guest | cost per parked instance | N measured | process RSS after one warm-up run (shared, not per instance) |
|---|---|---|---|
| Go core module | **2.5 MiB** (3.0 to 3.2 at N=1) | 1, 16, 64, 256, 512, 1024 | about 160 MiB |
| Python component | **9 to 10 MiB** at N up to 16 (9.9 at N=1, 9.0 at N=8 and 16); **12 MiB** at N=64 and 128 | 1, 8, 16, 64, 128 | about 510 to 580 MiB (the 19 MB component, compiled and cached) |
| C# component | **49 to 50 MiB** | 1, 16, 64 | about 170 MiB |

**The memory cap does not change those figures.** At N=16, Python is 8.99 MiB at a 32 MiB cap and 9.00 at 256; C# is 49.53 at a 64 MiB cap and 49.48 at 256; Go is 2.68 at 32 and 2.67 at 256. A C# guest does not start at an EXPLICIT 32 MiB cap (`.NET runtime failed to initialize`; a .NET guest needs 52 MiB at least, which `engine/a_default_memory_limit_clears_the_largest_runtime_test.go` records now that the language floor that used to document it is gone); until cleat#4200 a stock worker, with no limit set, gave a C# store the 64 MiB language floor (`engine.languageMemoryFloorBytes`) rather than 32; the 256 MiB default now clears it for every language and the floor is gone. In the raw outputs the C# rows print as `"lang":"file"` (the probe's mode for a prebuilt component).

**A guest that has made garbage does depend on the cap, for C# only.** Each guest allocated and dropped about 256 MiB in 4 MiB chunks, then made its durable call and parked (N=16):

| guest | small cap | large cap |
|---|---|---|
| Python | 16.2 MiB (cap 32) | 16.2 MiB (cap 256) |
| Go | 19.6 MiB (cap 32) | 19.6 MiB (cap 256) |
| **C#** | 49.5 MiB (cap 64) | **65.7 MiB (cap 256)** |

So the suspicion that a garbage-collected runtime sizes its heap to the limit is **true of C#** (about 16 MiB, a third, more per parked instance at a four times larger cap, after this much churn) and **not true of Python or Go**. A parked guest does not run, so it never collects what it left behind. Two things this does not tell us: how far C# retention grows with more churn or a cap above 256 (one churn size and two caps were measured), and whether a collection requested just before parking would recover it (not tried; it is the obvious thing to try, and it would belong in the SDK's park path).

**Threads and scheduling.** A parked instance holds one OS thread, because the guest is blocked inside a cgo callback: **1053 threads for 1024 parked Go instances, 147 for 128 Python, 80 for 64 C#**, so about one per instance. **No trend in a 1 ms ticker's lateness with the number of parked instances was visible, up to 1024**: across all 28 configurations the p99 lateness was 4 to 12 ms (the ticker's own median is 1 to 3 ms in this container), and Go at 256, 512 and 1024 parked gave a p99 of 4.1, 5.0 and 5.0 ms and a worst single gap of 16, 12 and 6 ms. **One outlier is unexplained**: the Go churn run at a 32 MiB cap with N=16 had a p99 of 12.0 ms and a worst single gap of 117 ms (the next worst gap in any run is 38 ms); at N=16 it does not bear on a trend in N, but it was not investigated. On this evidence the limit on resident runs is **memory, not threads or scheduling**.

**What this does not cover.** It is the Go scheduler's responsiveness, not claim or database throughput under load while instances are parked. It is a 10-CPU Linux VM; the thread count at several thousand and a real worker with a database were not tried. The Python figure rose from 9 to 12 MiB between N=16 and N=64 and was not explained. The C# guest is the conformance component (a small program); a real C# workflow holding state would cost more. The cost of a workflow that has built up its own state is its own state plus the figures above, bounded by the cap.

**Consequences for the starting constants (recommendations, not decisions).**
- **Worst case for a resident-count limit** at the measured costs after churn: 64 resident runs would hold about 1.2 GiB of Go instances, 1.0 GiB of Python, or **4.1 GiB of C# at a 256 MiB cap** (3.1 GiB at a 64 MiB cap), on top of the shared baselines above. A count limit in the tens is affordable for Go and Python; C# is the expensive one and the one a higher cap makes more expensive.
- **`--memory-soft-limit` (`MemoryController` over `MemoryMonitor`, which stops claiming at a fraction of memory) sees a parked instance's memory only on some hosts.** `MemoryMonitor` reads cgroup v2 `memory.current` when it exists (the worker's own container, which includes wasmtime's linear memory), otherwise the host-wide `/proc/meminfo` (everything on the machine), and otherwise the Go heap (`readGoHeap`), which cannot see wasmtime's memory at all: parked instances are invisible to it there, which includes macOS. So it is an aggregate guard for the worker's own parked memory only on cgroup v2; whether residency should account for parked instances itself, by their measured cost, is a question for the design of the limit.
- **Raising `--wasm-memory-max-mb` to 256 (the default since cleat#4200) costs Go and Python nothing here and C# about a third more per parked instance after churn.** Together with the point above, that argues for a per-language or per-workflow-type memory limit rather than one number (cleat#4171 is the decision; the per-workflow-type limit is a separate design).
- **Until cleat#4200 the worker passed the memory limit to the engine only when `--wasm-memory-max-mb` was explicitly set** (`wasmtimeMemoryLimitFromFlags`), so changing the flag's default alone would have left a stock worker on the engine's own 32 MiB. #4200 changed the engine constant and removed that gate, which is why a stock worker now runs at 256 MiB.

## 9b. Terminate and cancel while held (measured 2026-11-20, cleat#4162 PR 2c)

**Question.** Does a terminate or a cancel recorded in the store reach a held run as soon as it reaches a suspended one? The bar: no later than with residency off, on the same worker.

**Method.** A real worker (real wasmtime, real Postgres), a guest that sleeps 8 s (durably) and then makes one outbound call, residency off and on, the run terminated or cancelled in the store while it sleeps. The worker's heartbeat is called by hand, either straight after the settle (the best case) or not before the timer fires (the worst case inside one `--heartbeat` interval, made deterministic). Both settles bump the lease's generation and clear `assigned_to` on a running run, so the heartbeat reports the run lost.

| | residency off (control) | on, heartbeat right after | on, heartbeat not yet run, **before the check** | on, heartbeat not yet run, **after the check** |
|---|---|---|---|---|
| terminate | stopped in 38 to 86 ms (five runs), never reached the call | stopped in 39 to 153 ms (two runs), never reached the call | stopped when the timer fired, **6.0 and 7.2 s later; reached the call** | stopped at the timer, 7.3 s; **never reached the call** |
| cancel | stopped in 40 to 285 ms (five runs), never reached the call | stopped in 43 to 77 ms (two runs), never reached the call | stopped at the timer, 7.1 to 7.2 s later (three runs); **reached the call** | stopped at the timer, 7.3 s; **never reached the call** |

**Reading it.** The existing lost-fence path works: the heartbeat reports the run lost, `reg.cancel()` reaches the hold, and eviction was as fast as the control. The gap was the interval between the settle and the next heartbeat: a held run whose timer fired first went back to its guest, which made an outbound call that a suspended run could not (it cannot be claimed once terminated). The store write that follows is fenced, so the final status was right; the side effect had already happened. The fix is a check of the store at the one place a held run returns to its guest, for every kind of wake. The check runs the worker's heartbeat tick itself, so it costs one tick per wake (one batched query per tenant that has runs in flight, and ticks are serialized), not one query for the one run.

**What was not measured.** A guest with real defers; a reclaim by the reaper (it bumps the same generation, so the predicate is the same, but it was not run as its own row); a store that is slow and not unreachable. With the check in place, "evicted at the wake" means the instance stays in memory until the heartbeat or the timer, whichever is first.

**Kill while held (cleat#4162 PR 2d; the scenario is `tests/chaos/a_kill_while_held_test.go`, merged in #4273).** The worker is killed with SIGKILL in the middle of a hold (call A, a sleep of 8 s, call B, residency on) and a second worker reclaims the run. **Every effect happened exactly once and the run finished `done`**, so a held run does not depend on anything only memory had. **What it costs, measured by cleat-review3 on #4273 from the recording service's own timestamps (their measurement, not mine):** the time from effect A to effect B was **9.2 to 9.5 s with residency off** and **40 to 41 s with it on**. With residency off the sleeping run is a *parked row* (ready, unassigned, a wake time ahead), so the next worker claims it at its wake time. A held run is still `running`, assigned to the dead worker, so nothing claims it until the reaper does, after its start-up grace. **The harness ages the dead worker's lease (`ageLease`) instead of waiting out the real reclaim window, because that window cannot go below about 37 s and a sweep over it would be unaffordable, so the production window (about 47 s by default, which is read from how the reaper works and NOT measured) comes on top.** This is latency and not correctness. A faster path (the reaper preferring held runs whose wake time has passed, or a shorter lease while a run is held) is cleat#4275, for 0.7.0. The case is in the nightly `Chaos full sweep` and in no pull-request job, so it has passed on two machines (the author's, and twice on cleat-review3's), and not yet in CI. The timing figures above are from cleat-review3's machine alone, under heavy load (load average 15 to 40), and have not been reproduced elsewhere.

**A caveat that applies to every measurement above and below this one: a cold start is slow.** Compiling the module on a worker that has not seen it took **2.8 s** here (`replay_ms=2789`), and a sleep's deadline is anchored at the last recorded event's timestamp (`DurableSleep` takes `max(s.nowMs, s.Now(ctx))`), which is when the run was **claimed** only when no step precedes the sleep, and not when the guest reached the sleep. A short sleep can therefore be over before the guest gets to it: a 300 ms sleep completed instantly and the run finished without ever sleeping, which is how the first version of this test measured nothing. Any claim about a hold window, a short sleep, or "residency off suspends here" has to say whether the module was warm.

## 9c. Python and C# are admitted (cleat#4162 part A)

**What changed.** Python and C# guests are held through a durable sleep like Go ones. A run is charged a figure per held instance and, for these two languages, a **fixed** figure once per distinct component while any run of it is held (`defaultResidentInstanceBytes`, `defaultResidentFixedBytes` in `engine/resident.go`). Java, Rust and JavaScript are still refused: nothing was measured for them.

**Where the numbers come from, and what each one is.** WS-cleat1's measurement of components held inside a host callback (2026-11-20, `cleat-internal/measurements/component-held-instances-2026-11-20/README.md`; native arm64 Linux, **x86-64 not measured**) and section 9a above.

| | per held instance | charged | fixed, once per component | charged |
|---|---|---|---|---|
| Go | 2.5 MiB; 19.6 MiB after a guest made garbage | **24 MiB** | none measured | none |
| Python | 1.3 to 1.6 MiB (python-hello, private); 12.0 to 12.2 MiB (call_all_plugins, unshared); 16.2 MiB after garbage (9a) | **17 MiB** | +493 MiB (python-hello), +510 MiB (call_all_plugins) | **512 MiB** |
| C# | 48.8 MiB private, 49.2 to 49.3 MiB RSS, the same at every N from 1 to 60; 65.7 MiB after garbage at a 256 MiB cap (9a) | **66 MiB** | +81 MiB | **82 MiB** |

**The rule for the per-instance figure is the one Go already used: the larger measured figure, rounded up.** The coordinator asked for the 12.2 and 49.2 figures; the 16.2 and 65.7 are this document's own measurements of a guest that had allocated and dropped about 256 MiB, and 256 MiB is the default memory cap since cleat#4200, so they are the more conservative reading and are what is charged. A too-high charge can only make residency park less.

**What the figures are NOT.**
- **They are the cost of the runtime, not of the guest.** WS-cleat1 measured what a guest adds by what it holds before it blocks: about one for one (16 MiB touched cost +16.0 in Python and +16.2 in C#; 64 MiB cost +63.9 and +64.9; 200,000 small records cost +66.5 and +32.4). So a charge is a floor for a guest that does real work. **The default budget (`MaxRuns` times the dearest instance plus one fixed cost per language) is what the table implies and no more**: it is not a promise that nothing is refused on memory, and real guests can exceed it. `--memory-soft-limit` is the second guard.
- **The fixed figure is the whole process after one warm-up run**, so it also counts the Go runtime and wasmtime baseline, which was not separated. It is charged per distinct component (the worker passes the definition name and version) because the module cache is keyed by the component's bytes; **two distinct components held together were not measured**. It double counts against a worker's own memory monitor, because a worker that has run a Python workflow at all has the component cached whether or not anything is held. Both are the safe direction.
- **A component the build did not stamp is detected as Python** (`DetectLanguage`'s fallback) and so would be charged as Python, 17 MiB where C# needs 66. `cleat build --target csharp` stamps and reads the stamp back (`cmd/cleat/build_csharp.go`, exit 2 on a mismatch); the checked-in C# test fixture is NOT stamped, so tests stamp it first.
- **Threads and mappings are not in the charge.** WS-cleat1 measured about one OS thread and about nine memory mappings per held instance (no limit reached at 400 Python instances; C# stopped at 60 for memory); on a stock kernel `vm.max_map_count` (65,530) would bind at about 7,000 instances. `--resident-max-runs` is the guard, and its default is small.
- **Standard-library Python only; no third-party import was measured.**

**What was run on a real component.** A real C# component and a real Python guest are each held through a durable sleep on the worker's real wasmtime backend and complete in place with both calls in one execution, where the same run with residency off suspends after the first; what is charged while held is that language's instance plus fixed figure and nothing is left charged afterwards. The Python guest's sleep length is an input and the test shows the hold follows it (a 400 ms and a 1500 ms request, measured as the gap between the guest's two calls, which excludes the component compile that dominates an execution's wall time). A guest that ignores its input was run as the known-positive and turns that row red, both with a short and with a long default.

**The execution fence on a component (cleat#4278).** The fence **does** fire on busy guest code on the component path: the fixture's `spin` mode (C#) and the SDK's `spin_workflow.py` (Python) are stopped at 5 ms, 300 ms and 2 s fences on native macOS and linux arm64 (C# in 0.15 to 2.4 s whole-`Execute`; Python's overshoot is larger and not explained, because a 19 MB component compile and CPython start are in that time). A first control that seemed to show it did NOT, a 5 ms fence that did not stop the C# `sleep`/`hello` modes, was a guest that uses under one epoch tick (50 ms) of guest time before its first host call, so there was nothing to interrupt. Rows now run: a busy C# guest is stopped by a 300 ms and a 2 s fence (and `hello` under the same fence completes, the control that the fence stops the spin and not everything); a C# run held for 400 ms completes under a 250 ms fence, so the hold is not charged to it (with the spin stopped at that same fence as the liveness control); and a Python guest that wakes from a hold into a runaway loop is stopped 1.96 s after the wake against a 2 s fence.

**What was NOT shown: that the fence is RESTARTED at the wake, as opposed to re-armed.** Tested and found undiscriminating: **neutering the restart itself** (`restartFence`'s `budget.restart`) leaves the Python row green (1.936 s after the wake against a 2 s fence, as with it at 1.959 s). **An earlier version of this paragraph said removing the component builder's `onLimitsRestart` registration left it green; that mutant is EQUIVALENT**, because `Execute` registers the same restart before dispatching to the component path and the closure reads `b.budget` when it runs, so the component's own registration is redundant and removing it changes nothing. Mutate `restartFence`, or remove both registrations. A restart differs from a re-arm only for a guest that had already used part of its fence when the hold began, and this guest has not (componentize-py pre-initialises the interpreter at build time, so it has almost no guest time before its first host call; a hypothesis about that guest, not a measurement). The restart is registered by the component builder, its arithmetic is tested without a clock, and a structural test pins that every builder registers it; it is not exercised on a held component. Showing it needs a guest that computes, sized to the machine, before the hold, as the core path's timing row does.

## 10. Staging if approved

| stage | what | depends on |
|---|---|---|
| 0 | the measurements of section 9 | none |
| 1 (optional) | WARM RE-ENTRY: cached history and/or a pre-built instance | stage 0 item 1 |
| 2 | PARK for the core-module guests (Go), timers and signals, `--resident-window`, LRU and count limit, drain eviction, metrics | stage 0 items 2, 4 |
| 3 | the component path (Python, C#): the fence bracketing | stage 0 item 3 |
| 4 | MySQL and SQL Server batched wake check | stage 0 item 5 |
| 5 | the policy (B/D): the estimate, the floor and cap | stage 0 items 1, 6 |

## 11. Not covered

Residency across a worker restart (a resident run does not survive it; that is by design, recovery is replay); a resident run handed to a different worker; any change to how durable calls retry; and the dashboard presentation of a resident run.
