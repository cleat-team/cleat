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
not tell its own control from a broken scrape, and waited out the whole budget). And a cold start compiles the module (about 2.8 s measured), so the sleep is 8 s, long enough to be
asleep when the kill lands.

**Not covered, stated so it is not discovered later:** a kill at the instant the timer fires (the wake and the claim check); a held run with real defers or a long history; a signal
wait (the signal wake source is a later change); a second worker with residency on; MySQL and SQL Server. About 75 s with the harness's build, so it runs in the nightly full sweep
(`chaos-full`), not in the pull-request sample.

## Not yet

Signal and promise polls (#4153), `continue_as_new` (#4159), cancel, the Python guest, a slow-service op (a response longer
than the instance timeout), seeds, the CI sample/full split, MySQL and SQL Server.
