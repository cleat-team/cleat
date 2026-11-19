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

## Not yet

Signal and promise polls (#4153), `continue_as_new` (#4159), cancel, kill while resident, the Python guest, a slow-service op (a response longer
than the instance timeout), seeds, the CI sample/full split, MySQL and SQL Server.
