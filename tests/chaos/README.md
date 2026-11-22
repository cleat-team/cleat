# Fault-injection harness (cleat#4169)

For every step boundary of a scripted workflow: inject a fault, let the system recover, compare against a fault-free run.

**Black box.** A real `cleat-worker` subprocess, a real PostgreSQL, a recording service in the test process. It imports no engine internals, so
it can be pointed at a build from before a fix (`CLEAT_CHAOS_TREE`) and does not move when the replay logic is refactored.

**Workflows are data.** `testdata/chaosscript` is one Go guest that interprets a JSON list of operations (`call`, `call_retry`, `side_effect`,
`sleep`, `defer`). The scenario table in `chaos_test.go` is the list; seeds and a generator come later (step 4 of the plan).

**Boundaries.** Every request the fault-free run makes to the recording service, held `before` its effect (arrived, nothing performed) or `after`
it (performed, reply withheld).

| fault | what is injected |
|---|---|
| `TestChaosSigkillThenReclaimAtEveryBoundary` | SIGKILL at the boundary, then the next worker reclaims (lease aged, see below) |
| `TestChaosSigkillThenTerminateAtEveryBoundary` | SIGKILL, then the run is terminated before anyone reclaims it; the defer segment must run the registered cleanups and nothing else |
| `TestChaosEventWriteRefusedThenReclaimAtEveryBoundary` | INSERT on `event_history` revoked from the worker's roles for `CLEAT_CHAOS_DBHOLD` (default 5 s) from the boundary; no further effect may be performed |

**Oracle.** Counts at the recording service, not inferences from history: every effect exactly once, except the documented at-least-once window
(an effect performed whose event never became durable, which a reclaim may perform once more); a side effect's held value identical in every
observation; the final status as in the fault-free run (or `terminated` for the terminate fault). A fault that never fired is reported
`UNMEASURED`, not as a pass.

**Simulated reclaim.** After a kill the harness ages the dead worker's heartbeat in the database. The real reclaim window cannot go below
~37 s (`minimumReclaimAfter`), which at ~10 s per run would make the sweep unaffordable. The lease row ends up in the state the wait would have
left it in; the reaper's own timing is not exercised.

## Limits of the faults, stated

- A `before` hold that is aborted after the kill still counts as an ARRIVAL at the service: for a call with a scripted failure (`call_retry`) the aborted request consumes one of the failures. Measured against #4152 it hides nothing, but "nothing was performed" is a description of the effect, not of the service's counters, so the fault is weaker than the word suggests.
- A comparison across two checkouts (`CLEAT_CHAOS_TREE`) needs a FRESH database: each run applies the migrations of the tree under test to the database it is given, and `CREATE TABLE IF NOT EXISTS` never adds a column.

## Run

    CLEAT_CRASH_DB='postgres://cleat:cleat@localhost:55432/cleat?sslmode=disable' \
      go test -tags chaos ./tests/chaos -count=1 -timeout 60m -v

**Residency.** `CLEAT_CHAOS_RESIDENT_MAX_RUNS=N` starts every worker with `--resident-max-runs N`, so the whole fault suite can run the way 0.6.0 ships (cleat#4162, PR 3); a `workflow_dispatch` of the Chaos workflow takes it as the `resident_max_runs` input. Unset, the harness passes nothing and a worker runs at the product's own default. A row that is *about* residency being off (the kill-while-held control, the held-vs-off `off` arm, the parked-run executor) says so with an explicit `--resident-max-runs 0` (`residencyOff`) and is never overridden by the knob; a caller's own flag always wins. `TestChaosTheResidencyKnobPassesOnlyWhatItWasGivenAndNeverOverridesACaller` and `TestChaosAWorkerIsStartedWithTheResidencyTheKnobAndTheCallerSay` pin that.

**The guest is read from the bytes.** `CLEAT_CHAOS_GUEST=python` builds a component and every row that deploys `env.wasm` runs it (the kill-while-held row included: both its arms pass on the Python guest, the held arm reading `cleat_resident_runs=1`). `TestChaosTheDeployedScriptGuestIsTheLanguageThatWasAskedFor` checks the deployed binary's version word (core module `01 00 00 00`, component `0d 00 01 00`), and the nightly's Python step runs the held row and that check, each required to report a pass.

One case: `-run 'Reclaim/retry/sigkill@before:R#1'` with `CLEAT_CHAOS_SCENARIO=retry`. A failure prints the scenario, the fault, the request order,
and the worker log. Against another commit: `CLEAT_CHAOS_TREE=<checkout>` (copy `testdata/chaosscript` into it).

## Proof it works

See the table on cleat#4169: against the builds before the fixes it finds #4143 (4 of 4 cases), #4152 (2 of 10) and #4160 (3 of 4), and the same
cases pass on develop (#4143, #4152) and on #4166 (#4160).

## Flake log

`scripts/chaos-flake-log.py` re-derives a table of every ATTEMPT of every Chaos run from the API (a re-run that passed after a failure is two rows and a flake; `gh run list` alone shows only the latest attempt). `--self-test` checks it offline, and the live known-positive is a run whose attempt 1 failed and attempt 2 passed:

    scripts/chaos-flake-log.py --workflow "Ecosystem CI" --since 2026-11-19T19:55:00Z --limit 40   # run 44604503158: failure, then success

`scripts/chaos-failed-cases.py` reads the failed cases out of a `go test -json` job log; its known-positive is a real failing CI log in `testdata/`.

## Promise ops (cleat#4207)

`promise_create`, `promise_resolve` and `promise_reject` (both guests) create a promise `p-<name>` and, for the last two, settle it. The recording service
cannot see a promise-store write, so the oracle is the `workflow_promises` rows after the run (`promiseRows`, `promiseViolations`): **each op's promise exists
exactly once, in the scripted status** (pending, resolved, rejected). Two rows under one name is a create that ran twice; the wrong status is a settle that did
not take. The fault-free run asserts it unconditionally, so an op that did not do what it claims reads as a failed precondition, not a green; a run that ended
`done` after a fault asserts it too. The scenario `promises` (call, resolve, reject, create, call) puts the kill boundaries on the calls AROUND the three ops,
so a kill after them and before the last call forces a replay of all three.

**What this cannot reach, stated so it is not discovered later:** a fault INSIDE one of the three calls (between its event being recorded and its store
write). The boundaries are service requests, and these calls make none. A mutant where a replayed `CreatePromise` runs fresh was caught here, but by the
final-status check (a replay divergence), not by the row check; the row check's two branches were each proven by a guest mutant (a resolve that never happens;
a create made twice).

## Ops whose effect the recording service cannot see (cleat#4241)

`signal`, `cron_create`, `cron_delete`, `cron_list` (both guests) and `send` (both guests). Their effect lands in a table or on a goroutine, so each has an oracle of its own,
and each oracle was proven by an engine mutant that makes the specific double effect real (below); a clean sweep alone says nothing about whether the oracle could see.

| op | where the effect lands | what is read back |
|---|---|---|
| `signal` (a signal `sig-<name>` to the run itself) | `workflow_signals` | exactly one row per send, across a kill and a reclaim (scenario `signals`) |
| `cron_create`, `cron_delete` | `workflow_schedules` | see the next paragraph (scenario `crons`) |
| `cron_list` | the recorded list, answered on replay | the count the guest held, reported on two calls (scenario `cron_list`) |
| `send` (fire-and-forget to the service as `send_<name>`) | the service, from a goroutine | AT MOST once; never repeated by a reclaim, never made by a terminated run (scenarios `sends`) |

**A row count cannot see a replay that re-ran a cron call,** and this is why the cron scenarios change the world between the run and its replay. `schedule_cron` derives its id from
`(tenant, workflow, step)` and creates the row idempotently, so a replay that ran it fresh finds the row it made and the count stays at one. In `crons` the harness deletes cron C's row
after the run created it and puts cron D's row back after the run deleted it, and the run must end with C absent and D present: a replay that re-created C or re-deleted D leaves a
mark a count never would. In `cron_list` the harness adds a schedule only AFTER the run's `list_crons` event is durable, so any later answer that includes it was read from the table and
not from the record. Every wait in these helpers is on what is DURABLE (a row, an event in `event_history`), never on what the worker merely did: a worker killed before its event was
flushed owes the replay nothing, and acting earlier would make a legitimate fresh read look like a bug.

**`send` has its own scenario list** (`fireAndForgetScenarios`) because the engine records a send BEFORE it dispatches it and a replay dispatches nothing, so a kill in between loses the send
for good. The generic oracle demands exactly once and would call that a failure; the two verifiers read the exemption off the scenario's own `send` ops (`atMostOnceOps`), which leaves every other
scenario as it was. The terminate sweep skips the send's OWN boundaries, for a measured reason: the send's request is made by a goroutine concurrently with the body, which has already moved on,
so with the send held the body made `B`, `send_T` and `C` within 14 ms, before the kill, and "what the body had done at the kill" cannot be read off the golden order there.

**The reclaim sweep does not skip those boundaries, but it allows for the same fact (cleat-review3, measured on a loaded machine).** With the send held, the body may already have PERFORMED the next call
(`B`) before the kill, and its event is not yet durable, so the reclaim repeats it: the ordinary at-least-once window, for the call the kill actually interrupted. `verifyReclaim` therefore allows ONE extra
success in total across the plain `call` ops scripted after the send, because the body moves past a call only once its record is durable and so at most one call is in that state. Everything else stays
exact: a call before the send, two calls, a call three times, or the send itself repeated are still violations. **A retried call after a send gets no allowance** (it can show more than one success in that
window), so a scenario that puts one there fails loudly until the allowance is extended on purpose. The window is milliseconds wide, so `a_send_boundary_reclaim_oracle_test.go` drives the verifier with the
measured request sequences instead of waiting for the machine to be loaded enough.

**A worker that exits before the boundary ends the case at once** (cleat#4291). Workers run until the harness kills them, so one that exits on its own (it refused to start, or died) can never reach a boundary. `execute` selects on that, returns in about a second, and `runFault` reports `UNMEASURED` with the exit status and the worker's last ERROR line, and `pg_stat_activity` as it was at that moment. Before this, such a case waited `runBudget` twice (240 s) and reported "the run did not reach the boundary", which read like a generator problem; four in a row cost 16 minutes in the 2026-11-21 nightly seeded step, where the worker had refused to start for want of a connection slot (53300). `connSnapshot` is also logged before each case and at the boundary (a worker is alive then), so a count that grows from case to case shows as a leak. **Its limit:** it cannot tell the harness's own idle connections from a worker's (neither sets `application_name`), so read it as the total against `max_connections`, not as a per-owner count.

**A hook that drives a scenario (`Scenario.Drive`) returns the reason it did not act**, and a reclaim run that finished with a Drive that did not act is reported UNMEASURED (a failure of the check, not a
finding about the engine), as is the golden run. The two cron hooks report; `resolveWhenSuspended` still returns `""` always, which is unchanged behaviour on purpose.

**Known positives, each made by mutating the engine and reading the failure (all restored by content afterwards):**

| mutant | caught by | boundaries that fail | that pass |
|---|---|---|---|
| a replay of `signal_workflow` delivers again | `signal:S: 2 row(s)` | the four at or after the first signal | the two before it (nothing replayed) |
| a replay creates the schedule again at the SAME id | `cron:C: 1 row(s), want 0` and nothing else | all four | none |
| a replay deletes the schedule again | `cron:D: 0 row(s), want 1` | the two after the delete was recorded | the two before it |
| a replay of `list_crons` reads the table afresh | `carried held value "n=2", want "n=1"` | the four where a replayed list feeds a fresh request | the two after both calls |
| a replay dispatches the recorded send again | `succeeded 2 time(s), at most 1`, in both sweeps | every boundary after the first send | the two before it |

The fourth row needed a stronger oracle than the harness had. "One op reported two different held values" cannot see a replay that is wrong EVERY time it is asked: in a `before` case the
aborted first arrival is not in the request list, so the only value the service sees is the wrong one, and the first version of this scenario passed that mutant at three boundaries of
four. The count is deterministic, so `heldExpect` compares it with the right answer.

**Not covered, and why:** `fetch` (G1b) cannot be driven by this harness at all. Both guests' SDKs route `fetch` through `call("http", "fetch")`, the ordinary durable-call path, so neither
imports the `cleat_fetch` host call the dispatcher moved (`python-sdk/cleat_sdk/host_calls.py`, `Routes through call('http', 'fetch', ...) — no separate WASM import needed`; the Go SDK's
`DurableFetch` says the same). And a worker's egress floor always refuses loopback (`--enable-workflow-fetch`'s help text), so a counting endpoint in the test process is unreachable for a
black-box worker; the only override, `AllowLoopback`, is test-only and `TestNoProductionCodeAllowsLoopbackEgress` forbids setting it in production code. Driving `cleat_fetch` needs a guest
language that imports it directly (Rust or Java) and an endpoint the floor permits. Also not covered: a kill INSIDE one of these calls (between the store write and the event being recorded: the
boundaries are service requests and these calls make none), `schedule_invoke`, and MySQL and SQL Server.

## Kill while a run is held (cleat#4162)

`TestChaosSigkillWhileAHeldRunSleepsThenReclaim`. The other faults land on a request to the recording service, and a run held in memory through a sleep makes none, so none of
those boundaries is "while held". This one kills the worker in the middle of a hold: `call A`, `sleep 8s`, `call B`, residency on (`--resident-max-runs 2`), SIGKILL once A has
happened and the run is asleep in memory, then the lease is aged and a second worker (residency off) reclaims. **Oracle:** the run ends `done` with A and B each exactly once.
What it pins: holding a run adds no durable state, so crash recovery is the same as for any run in flight (a sleep records no event and its deadline is served by the clock on
replay). A change that made a held run depend on something only memory had would fail here.

**The precondition is the hard part, and it is checked, not assumed.** A kill that lands after the run was *suspended* would pass for the reason a kill of an idle worker does and
measure nothing about residency. So the worker is killed only once it says it is holding the run: `cleat_resident_runs` on its `/metrics` (the case starts it with
`--api-addr` on a free loopback port) reads at least 1 AND the store does not have the run parked (`parkedNow`). The **control** is the same scenario with residency off, and has to
read the opposite on the same detector: `cleat_resident_runs` 0 and the run parked. A case whose run was never in the state it needs reports `UNMEASURED`, which fails.

**Two facts about the detector.** The counter exports no series at all until its first increment, so a worker that has never held a run has no `cleat_resident_runs` line: "the page
is real (it carries other `cleat_` series) and the series is absent" is zero, and an unreadable page is unknown, not zero (the first version treated absence as unreadable and could
not tell its own control from a broken scrape, and waited out the whole budget). And **the 8 s sleep is not there for the cold-start compile**: in this scenario the compile happens before call A, not during the sleep (WS-cleat1's review of #4273), and the sleep's
deadline is anchored at A's recorded event, not at the claim (`DurableSleep` takes `max(s.nowMs, s.Now(ctx))`, and `Now` is the last history event's timestamp; cleat-review3's reading of the
code at `3c662e32`, consistent with one measurement and **not measured directly**). So 8 s only has to outlast the detection (a 100 ms poll plus a metrics scrape) and the kill.

**What a kill costs, measured (cleat-review3 on #4273, from the recording service's own `request.At` timestamps, not by the author of this section):** the time from effect A to effect B was
**9.2 to 9.5 s with residency off** and **40 to 41 s with it on**. With residency off the sleeping run is a *parked row* (ready, unassigned, a wake time ahead), so the next worker claims it
at its wake time. A held run is still `running`, assigned to the dead worker, so nothing claims it until the reaper does, after its start-up grace. The harness ages the dead worker's
lease (`ageLease`) instead of waiting out the real reclaim window, because that window cannot go below about 37 s and a sweep over it would be unaffordable, so **the production window
comes on top and is NOT measured**. **These timing figures are from cleat-review3's machine alone, under heavy load (load average 15 to 40), and have not been reproduced on another.** Exactly-once holds either way; this is latency (cleat#4275 is the fast path, for 0.7.0).

**Why the second worker has residency off, and what a residency-on second worker needs.** The reclaim lands about 40 s after A, so a replayed 8 s sleep is already due and would never
park: a residency-on reclaimer would measure nothing about residency. A row for it needs a sleep longer than the reclaim delay.

**Not covered, stated so it is not discovered later:** a kill at the instant the timer fires (the wake and the claim check); a held run with real defers or a long history; a signal
wait (the signal wake source is a later change); a second worker with residency on; MySQL and SQL Server. About 75 s with the harness's build, so it runs in the nightly full sweep
(`chaos-full`), not in the pull-request sample.

## Not yet

Signal and promise polls (#4153), `continue_as_new` (#4159), cancel, a slow-service op (a response longer
than the instance timeout), seeds, the CI sample/full split, MySQL and SQL Server.
