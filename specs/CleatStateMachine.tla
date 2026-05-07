------------------------ MODULE CleatStateMachine ------------------------
(*
 * TLA+ / PlusCal specification of the Cleat workflow instance state
 * machine, based on the actual state transitions in the codebase.
 *
 * States (matching the workflow_instances.status DB column and derived
 * terminal states):
 *   ready     -- eligible for claiming by a worker
 *   running   -- claimed by a worker, being executed
 *   done      -- finished successfully (terminal)
 *   failed    -- execution error (terminal)
 *   cancelled -- cancellation requested and processed (terminal)
 *
 * NOTE: There is no 'suspended' or 'timed_out' DB status.  Suspended
 * workflows are 'ready' with a future next_wake_at.  The reaper resets
 * stale heartbeats to 'ready', not to a separate timeout state.
 * Cancellation is a boolean flag (cancellation_requested); it becomes a
 * state here only when the worker processes it and transitions from
 * running -> cancelled.
 *
 * Transitions with code references:
 *   Initial        : StartNewRun creates instances with status = 'ready'
 *                    db.go lines 727-813
 *
 *   ready -> running: Worker claims via SELECT ... FOR UPDATE SKIP LOCKED
 *                     SET status = 'running', assigned_to = $worker,
 *                         heartbeat_at = now()
 *                     WHERE status = 'ready' AND next_wake_at <= now()
 *                     db.go lines 296-345 (ClaimWorkflows)
 *
 *   running -> ready (suspend): Worker calls ReleaseWorkflow which sets
 *                     status = 'ready', assigned_to = NULL,
 *                     next_wake_at = future time
 *                     Used when workflow calls DurableSleep/AwaitSignals etc.
 *                     engine.go lines 794-822, 824-895
 *                     db.go lines 670-678 (ReleaseWorkflow)
 *
 *   running -> ready (stale): Reaper reclaims workflows whose heartbeat
 *                     has not been updated within the timeout interval.
 *                     SET status = 'ready', assigned_to = NULL
 *                     db.go lines 851-864 (ReapStaleInstances)
 *
 *   running -> done : Worker calls CompleteWorkflow after successful
 *                     execution of the WASM entry point.
 *                     SET status = 'done', result = $result
 *                     db.go lines 614-640 (CompleteWorkflow)
 *
 *   running -> failed: Worker calls FailWorkflow when execution returns
 *                     an error that is not a SuspendError.
 *                     SET status = 'failed', error_msg = $errMsg
 *                     db.go lines 642-668 (FailWorkflow)
 *
 *   running -> cancelled: Worker detects cancellation_requested flag
 *                     during execution via PollCancellation.
 *                     engine.go lines 933-947 (PollCancellation)
 *                     (In the real system, the worker would call
 *                      FailWorkflow with a cancellation reason; we
 *                      model a separate state for clarity.)
 *
 * Safety invariants:
 *   1. Each instance has exactly one valid status value.
 *   2. Terminal states (done, failed, cancelled) never transition out.
 *   3. A running workflow is always assigned to a worker.
 *   4. No two running workflows share the same assigned worker.
 *   5. A ready workflow with a future wake time is never assigned.
 *
 * Liveness (temporal) properties:
 *   1. A ready workflow whose next_wake_at has arrived is eventually
 *      claimed by some worker (provided workers exist).
 *   2. A running workflow eventually reaches a terminal state or is
 *      released back to ready (suspend or stale heartbeat reaping).
 *
 * Model constants (to set in TLC):
 *   Workers          -- set of worker ids, e.g. {"w1", "w2"}
 *   Instances        -- set of workflow instance ids, e.g. {"wf1", "wf2"}
 *   HeartbeatTimeout -- max clock ticks without heartbeat before reaping
 *   MaxSteps         -- max execution steps before forced termination
 *   MaxSleep         -- max clock ticks for a suspension duration
 *
 * Typical TLC model config (in a .cfg file):
 *   CONSTANTS
 *     Workers = {"w1", "w2"}
 *     Instances = {"wf1", "wf2"}
 *     HeartbeatTimeout = 3
 *     MaxSteps = 4
 *     MaxSleep = 3
 *   SPECIFICATION Spec
 *   INVARIANTS
 *     TypeInvariant
 *     InvTerminalNotAssigned
 *     InvRunningAssigned
 *     InvUniqueAssignment
 *     InvWakeTimeConstraint
 *   CHECK_DEADLOCK FALSE
 *   PROPERTIES
 *     LivenessReadyClaimed
 *     LivenessRunningTerminates
 * ======================================================================== *)

EXTENDS Integers, TLC, Sequences

CONSTANTS
    Workers,           \* Set of worker process IDs
    Instances,         \* Set of workflow instance IDs
    HeartbeatTimeout,  \* Max clock ticks without heartbeat before reaping
    MaxSteps,          \* Max execution steps before forced termination
    MaxSleep           \* Max clock ticks for a suspension duration

\* Sentinel value meaning "no worker assigned".
\* Must not collide with any actual worker ID.
NULL == CHOOSE x : x \notin Workers \cup {"reaper", "clock"}

\* String values matching workflow_instances.status in the DB.
\* The DB uses: 'ready', 'running', 'done', 'failed'.
\* 'cancelled' models the worker action after detecting the
\* cancellation_requested flag.
ready     == "ready"
running   == "running"
done      == "done"
failed    == "failed"
cancelled == "cancelled"

TerminalStates == {done, failed, cancelled}

(*--algorithm CleatStateMachine

variables
    \* ---- State variables (conceptually one row per instance) ----
    status      = [i \in Instances |-> ready],   \* DB: workflow_instances.status
    assignedTo  = [i \in Instances |-> NULL],     \* DB: workflow_instances.assigned_to
    nextWakeAt  = [i \in Instances |-> 0],        \* DB: workflow_instances.next_wake_at
    heartbeatAt = [i \in Instances |-> 0],        \* DB: workflow_instances.heartbeat_at

    \* ---- Global variables ----
    clock       = 0;   \* Logical clock (monotonically increasing)

define

    \* ====================================================================
    \* INVARIANTS (Safety Properties)
    \* ====================================================================

    \* Invariant 1: Each instance has a valid status value.
    TypeInvariant ==
        \A i \in Instances :
            status[i] \in {ready, running, done, failed, cancelled}

    \* Invariant 2: Instances in terminal states have no worker assigned.
    InvTerminalNotAssigned ==
        \A i \in Instances :
            status[i] \in TerminalStates => assignedTo[i] = NULL

    \* Invariant 3: A running workflow is always assigned to a worker.
    InvRunningAssigned ==
        \A i \in Instances :
            status[i] = running => assignedTo[i] # NULL

    \* Invariant 4: At most one running workflow is assigned to any given
    \* worker.  This mirrors the unique-assignment semantics of
    \* SELECT ... FOR UPDATE SKIP LOCKED.
    InvUniqueAssignment ==
        \A i1, i2 \in Instances :
            (i1 # i2 /\ status[i1] = running /\ status[i2] = running)
            => assignedTo[i1] # assignedTo[i2]

    \* Invariant 5: A ready workflow with a future wake time is not
    \* assigned.  Mirrors the WHERE next_wake_at <= now() guard in
    \* ClaimWorkflows.
    InvWakeTimeConstraint ==
        \A i \in Instances :
            (status[i] = ready /\ nextWakeAt[i] > clock)
            => assignedTo[i] = NULL

    \* ====================================================================
    \* LIVENESS (Temporal Properties)
    \* ====================================================================

    \* Liveness L1: A ready workflow whose wake time has arrived is
    \* eventually claimed.  Relies on fairness of worker processes.
    LivenessReadyClaimed ==
        \A i \in Instances :
            []((status[i] = ready /\ nextWakeAt[i] <= clock)
               => <> (status[i] = running))

    \* Liveness L2: A running workflow eventually reaches a terminal
    \* state or is released back to ready (suspend via ReleaseWorkflow
    \* or stale-heartbeat reaping via ReapStaleInstances).
    LivenessRunningTerminates ==
        \A i \in Instances :
            [](status[i] = running
               => <> (status[i] \in TerminalStates \/ status[i] = ready))

end define;

\* ======================================================================
\* WORKER PROCESSES
\*
\* Each worker loops: claim a ready workflow, execute it (which may
\* complete, fail, cancel, suspend, or heartbeat), repeat.
\*
\* The runSteps counter bounds how many heartbeats the worker can issue
\* before it must take a terminating action, keeping the state space
\* finite for model checking.
\* ======================================================================
fair process worker \in Workers
variable
    executing = NULL;  \* Instance currently being executed, or NULL
    runSteps  = 0;     \* Steps taken in the current execution run
begin

Claim:
    \* Only attempt to claim when not already executing a workflow.
    \* SQL: UPDATE workflow_instances
    \*       SET status = 'running', assigned_to = $1, heartbeat_at = now()
    \*       WHERE id IN (
    \*           SELECT id FROM workflow_instances
    \*           WHERE status = 'ready' AND next_wake_at <= now()
    \*           LIMIT 1 FOR UPDATE SKIP LOCKED)
    \*       RETURNING ...
    \* Ref: db.go lines 296-345 (ClaimWorkflows)
    if executing = NULL then
        with i \in {i \in Instances : status[i] = ready /\ nextWakeAt[i] <= clock} do
            status[i]      := running;
            assignedTo[i]  := self;
            heartbeatAt[i] := clock;
            executing      := i;
            runSteps       := 0;
        end with;
    end if;

ExecOneStep:
    \* If we have a running workflow, take one execution step.
    \* The guard ensures the workflow is still running (the reaper may
    \* have reclaimed it between our claim and this step).
    if executing # NULL /\ status[executing] = running then
        if runSteps < MaxSteps then
            \* May continue executing (heartbeat) or terminate.
            either \* Complete normally.
                \* SQL: SET status = 'done', result = $3, assigned_to = NULL
                \*       WHERE id = $1 AND assigned_to = $2
                \* Ref: db.go lines 614-640 (CompleteWorkflow)
                status[executing]      := done;
                assignedTo[executing]  := NULL;
                executing              := NULL;
                runSteps               := 0

            or \* Fail with error.
                \* SQL: SET status = 'failed', error_msg = $3, assigned_to = NULL
                \*       WHERE id = $1 AND assigned_to = $2
                \* Ref: db.go lines 642-668 (FailWorkflow)
                status[executing]      := failed;
                assignedTo[executing]  := NULL;
                executing              := NULL;
                runSteps               := 0

            or \* Cancel (worker processed cancellation_requested).
                \* Engine detects cancellation via PollCancellation
                \* during execution.
                \* Ref: engine.go lines 933-947 (PollCancellation)
                status[executing]      := cancelled;
                assignedTo[executing]  := NULL;
                executing              := NULL;
                runSteps               := 0

            or \* Suspend -- workflow called DurableSleep / AwaitSignals
                \* etc.  Worker releases back to ready with a future
                \* wake time.
                \* SQL: SET status = 'ready', assigned_to = NULL,
                \*       next_wake_at = $3
                \*       WHERE id = $1 AND assigned_to = $2
                \* Ref: db.go lines 670-678 (ReleaseWorkflow)
                status[executing]      := ready;
                assignedTo[executing]  := NULL;
                with delta \in 1..MaxSleep do
                    nextWakeAt[executing] := clock + delta;
                end with;
                executing              := NULL;
                runSteps               := 0

            or \* Heartbeat -- continue executing next cycle.
                \* SQL: SET heartbeat_at = now()
                \*       WHERE id = $1 AND assigned_to = $2
                \* Ref: db.go lines 600-612 (Heartbeat)
                heartbeatAt[executing] := clock;
                runSteps               := runSteps + 1
            end either

        else
            \* MaxSteps reached -- must terminate (heartbeat disabled).
            either \* Complete normally.
                status[executing]      := done;
                assignedTo[executing]  := NULL;
                executing              := NULL;
                runSteps               := 0

            or \* Fail with error.
                status[executing]      := failed;
                assignedTo[executing]  := NULL;
                executing              := NULL;
                runSteps               := 0

            or \* Cancel.
                status[executing]      := cancelled;
                assignedTo[executing]  := NULL;
                executing              := NULL;
                runSteps               := 0

            or \* Suspend.
                status[executing]      := ready;
                assignedTo[executing]  := NULL;
                with delta \in 1..MaxSleep do
                    nextWakeAt[executing] := clock + delta;
                end with;
                executing              := NULL;
                runSteps               := 0
            end either
        end if

    else
        \* Either nothing to execute (executing = NULL) or the workflow
        \* was reclaimed by the reaper.  Clear the local reference.
        executing := NULL;
        runSteps  := 0
    end if;
    goto Claim;

end process;

\* ======================================================================
\* REAPER PROCESS
\*
\* Periodically reclaim workflow instances with stale heartbeats and
\* release them back to the ready queue.
\*
\* SQL: UPDATE workflow_instances
\*       SET status = 'ready', assigned_to = NULL, heartbeat_at = NULL
\*       WHERE status = 'running'
\*         AND heartbeat_at < now() - $interval
\* Ref: db.go lines 851-864 (ReapStaleInstances)
\* ======================================================================
fair process reaper = "reaper"
begin

ReapOne:
    \* Pick one running instance with a stale heartbeat and reset it
    \* to ready.  In the actual code this is a single UPDATE that
    \* affects ALL stale instances at once; here we model one-at-a-time
    \* to keep atomic steps simple.  Fairness ensures all stale
    \* instances are eventually reaped.
    with i \in {i \in Instances :
                    status[i] = running
                    /\ clock - heartbeatAt[i] > HeartbeatTimeout} do
        status[i]     := ready;
        assignedTo[i] := NULL
    end with;
    goto ReapOne;

end process;

\* ======================================================================
\* CLOCK PROCESS
\*
\* Advances the logical clock.  Time progression is required for
\* wake-time and heartbeat-timeout transitions to become enabled.
\* ======================================================================
fair process clock = "clock"
begin

Tick:
    clock := clock + 1;
    goto Tick;

end process;

end algorithm; *)

\* ======================================================================
\* Automatically translated TLA+ from PlusCal goes here when you run the
\* translator.  To translate from the command line:
\*
\*   cd specs/
\*   java -jar tla2tools.jar CleatStateMachine.tla
\*
\* Then load CleatStateMachine.tla (the same file) into TLC and set the
\* CONSTANTS and INVARIANTS in the model config.
\* ======================================================================

=============================================================================
