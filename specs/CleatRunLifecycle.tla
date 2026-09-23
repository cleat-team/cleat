----------------------- MODULE CleatRunLifecycle -----------------------
(*
  Cleat Run Lifecycle: every writer of workflow_instances.status

  cleat#1997. CleatClaim.tla models the claim/heartbeat/reap protocol for one
  status transition (ready -> running -> done/failed). Since it was written,
  the status vocabulary grew to eight values and eleven more code paths
  learned to write it: the two-phase defer transition (D6), five operator
  verbs, a retry/re-replay pair, and a parent-close cascade. None of those
  are covered by any spec. This model covers them, reusing CleatClaim's
  claim/heartbeat/reap shape rather than re-deriving it.

  Every actor and every fencing rule below was read from
  github.com/cleat-team/cleat @ c44d5a48 (2026-09-23), not from the issue
  body or from memory -- five fixes landed on develop in the 24 hours before
  this file was written (cleat#1974, #1975, #1976, #1977, #1978), and the
  issue's own description of "the broken behaviour" predates all five. The
  model below is ground truth as of that commit; re-grep before trusting it
  against a later one, per CLAUDE.md's own rule on numbers that rot.

  Implemented by (no line numbers -- see CleatClaim.tla's header for why):
    - engine/status_vocabulary.go      (isSettledStatus, childOutcomeForSettledStatus,
                                         the canonical settled-status list)
    - engine/defer_phase.go            (deferPhaseOwed, claimableStatus, the
                                         two-phase transition's shared rules)
    - engine/store_lifecycle.go        (PostgresStore: ClaimWorkflows, FinalizeWorkflowSegment,
                                         CompleteWorkflow, FailWorkflow, MoveToDeadLetterQueue,
                                         RetryWorkflow, ReleaseWorkflow, ReapStaleInstances,
                                         enforceParentClosePolicy/-At)
    - engine/store_defer_phase.go      (PostgresStore: FinalizeDeferPhase, ExpireDeferPhases)
    - engine/store_admin.go            (PostgresStore: adminForceResolve, adminForceMark --
                                         force-complete and force-fail)
    - engine/store_admin_rereplay.go   (PostgresStore: AdminReReplay)
    - engine/db.go                     (PostgresStore: TerminateWorkflow, CancelWorkflow,
                                         preemptivelySettle)
    - cmd/cleat-worker/setup.go        (notifyTerminal's four call sites decide which of
                                         Complete/Fail/DeadLetter/FinalizeDeferPhase a
                                         finished execution reaches; not a status writer
                                         itself, listed because tla.yml's path filter must
                                         see it move)

  Only the PostgreSQL store is cited. MySQL and SQL Server carry the same
  statements (engine/mysql_lifecycle.go, engine/mysql_store.go,
  engine/mssql_lifecycle.go, engine/mssql_operations.go,
  engine/mssql_signals_promises.go) and TestTheSQLPredicateAgreesWithTheGoOne
  plus the dialect-parity tests this issue asks for are what keep them
  honest; this model checks one dialect's shape of the state machine, not
  three copies of it.

  SCOPE DECISIONS, stated rather than silently applied (specs/README.md's own
  rule: "actors from the code, not from memory", and a decision not to model
  something belongs beside the thing it would have covered):

    - generation IS NOT A VARIABLE. Every real generation bump (Claim, Reap,
      ExpireDeferPhases, both arms of Terminate/Cancel/AdminForce*/
      CascadeTerminate) also clears or reassigns assigned_to in the SAME
      statement -- verified by reading every UPDATE that bumps it. TLA+
      actions read current state atomically, so a "stale generation" write
      cannot arise inside one action; assignedTo[i] = w alone is therefore a
      faithful fence check for every action below, exactly the simplification
      CleatClaim.tla already makes (it has no generation variable either).
      AdminReReplay and the two AdminForce* actions fence on generation ALONE
      in the real code (no assigned_to check) -- modeled here as having no
      assignedTo guard at all, which is the same thing once generation is
      gone: an operator action that is not settlement-refused always applies.

    - error_code IS NOT MODELED (failure-model-2026-09-22.md's I5, D5/cleat#1977).
      Restricting force-fail's error_code to a documented set is a data-format
      validation in Go, not a concurrency or ordering property -- it has
      nothing for TLC to explore. Out of scope for the same reason a JSON
      schema check would be.

    - 'suspended' DOES NOT APPEAR as a status. CLAUDE.md's own measurement:
      thirty-five predicates read status IN ('ready','suspended') and nothing
      ever writes it; a suspension is 'ready' with a future next_wake_at.
      claimableStatus's own 'suspended' arm is therefore dead in production
      and is not given a transition here.

    - OwesDefer[i] is a per-instance BOOLEAN fixed at Init and never changed
      by any action, standing in for `hasDeferEvents || compacted`
      (deferPhaseOwed's own two inputs). Nothing in this model's scope adds a
      defer registration or compacts a workflow, so "fixed since creation" is
      exact for what the model exercises, not merely convenient. TLC explores
      every combination via Init's own existential choice (the same pattern
      CleatClaim.tla uses for `alive`'s initial values).

    - THE PARENT-CLOSE CASCADE (D1/cleat#1978) is modeled as a STATUS WRITER
      -- CascadeTerminate below -- because it writes 'terminated' to a child
      exactly like TerminateWorkflow does. What this model does NOT do is
      chase the cascade's effect on an AWAITING ancestor (AwaitChild,
      AwaitAnyChild, GetChildResult) or explore an arbitrary parent/child
      topology: that is cleat#1998's own stated scope ("parents awaiting
      children and parent-close cascades, checking liveness"), which depends
      on this model for what statuses get written and builds the await
      machinery on top. ParentOf/ClosePolicy below are therefore a FIXED,
      minimal two-level topology (one designated TERMINATE child), not a
      constant TLC is asked to vary -- just enough for CascadeTerminate to
      have a child to act on.

    - REQUEST_CANCEL close policy writes NOTHING to workflow_instances.status
      (it sets cancellation_requested, a cooperative flag the workflow itself
      reads) and is out of scope for a model of status writers, the same
      scope call the issue's own body makes for admission's reprocess and
      duplicate-start paths (see below).

    - ADMISSION is out of scope (cleat#2000's own area). The concurrency-key
      loser calls ordinary TerminateWorkflow (already modeled as Terminate);
      dead-letter reprocess creates a brand-new row via StartNewRun and never
      touches the old row's status; a duplicate start under an idempotency
      key writes nothing. None of the three is an independent status-writing
      action, so none gets one here -- this is a scope decision, not an
      oversight, per CLAUDE.md's rule that a decision not to do something
      should live where the doing would have gone.

    - DB-UNAVAILABILITY (paused vs refused, cleat#2005) and FENCED-OUT-NO-
      FURTHER-DURABLE-CALLS (cleat#2008) are NOT separate properties here.
      #2008 already shipped (cmd/cleat-worker/setup.go, "a reclaimed run's
      execution stops issuing calls", merged as #2015) as a Go-level
      guarantee inside ONE execution, which is not a state this model's
      per-instance status variable can distinguish from any other running
      instance -- it is a property of the worker's call loop, not of
      workflow_instances.status, and belongs in a Go regression test
      (already shipped) rather than in this state machine. #2005 is a
      connectivity property of the STORE, which this model does not have a
      variable for at all (every action here assumes the store answers).
      Composing either with this model is future work, not silently dropped.

  State machine (per instance; W = a worker holds it; -> reachable statuses):

          +-------------------------------------------------------+
          |                                                        |
          v                                                        |
       ready <---------+------------------+                        |
        |  ^            \\                 \\                       |
        |  | Release/Reap \\ Claim           \\ Terminate/Cancel/     |
        |  | (no pending)  v                  \\ AdminForce* (owed)  |
        |  |            running ---------------+-----> terminating -+
        |  |             | | |  Complete/Fail/DeadLetter              |
        |  |             | | +----------------------------> done/     |
        |  |             | |    (pendingTerminalStatus=none)  failed/ |
        |  |             | |                              dead_lettered
        |  |             | +--- Terminate/Cancel/AdminForce*         |
        |  |             |      (direct arm, not owed) --> terminated/
        |  |             |                                 cancelled/
        |  |             |                                 done/failed
        |  +-------------+  Release/Reap (pendingTerminalStatus set)
        |                     -> terminating (CASE, not ready)
        |
        +--- RetryWorkflow (from dead_lettered) / AdminReReplay
             (from failed, terminated, or dead_lettered -- D4 excludes
             cancelled and done)

  terminating --Claim--> running (carries pendingTerminalStatus forward)
  terminating --FinalizeDeferPhase(w), holder only--> the recorded outcome
  terminating --ExpireDeferPhases, unfenced, past deadline--> the recorded
     outcome (fences out whoever holds it, worker or nobody)
  terminating --Terminate/Cancel/AdminForce* AGAIN, not owed from
     'terminating'--> immediately applies the NEW outcome, cutting the
     first cleanup short. This is not a bug this model is checking for: it
     is deferPhaseOwed's own documented behaviour (engine/defer_phase.go),
     restated here because it is the least obvious transition in the
     diagram and the one most likely to be modeled wrong by guessing.

  HOW TO MODEL CHECK:

    java -cp tla2tools.jar tlc2.TLC -config CleatRunLifecycle.cfg CleatRunLifecycle.tla

  `make tla` runs this for every specs/*.cfg automatically. Bounds and the
  measured state count are in specs/README.md, not here -- see CleatClaim.tla's
  own comment on why a number belongs in the file CI checks, not in prose
  that drifts the moment either file changes alone.
*)

EXTENDS Integers, FiniteSets

CONSTANTS
    Workers,              \* Set of worker identifiers, e.g. {"w1"}
    NumInstances,         \* Number of run instances, modelled as 1..NumInstances
    HeartbeatInterval,
    HeartbeatTimeout,
    DeferPhaseTimeout,    \* Logical-time bound on the two-phase transition (deferPhaseTimeout)
    MaxClaimBatch,
    NULL

ASSUME HeartbeatTimeout > HeartbeatInterval
ASSUME DeferPhaseTimeout > 0
ASSUME MaxClaimBatch >= 1
ASSUME NULL \notin Workers
ASSUME NumInstances >= 2   \* CascadeTerminate needs a parent and a child to have any effect

\* =============================================================================
\* VARIABLES
\* =============================================================================

VARIABLES
    status,                  \* [Instances -> Statuses]
    assignedTo,               \* [Instances -> Workers U {NULL}]
    heartbeatAt,              \* [Instances -> Nat]
    nextWakeAt,               \* [Instances -> Nat]
    pendingTerminalStatus,    \* [Instances -> Outcomes U {NoOutcome}]
    deferDeadline,            \* [Instances -> Nat]
    clock,                    \* Nat
    alive,                    \* [Workers -> BOOLEAN]
    OwesDefer                 \* [Instances -> BOOLEAN], fixed at Init -- see header

vars == <<status, assignedTo, heartbeatAt, nextWakeAt,
          pendingTerminalStatus, deferDeadline, clock, alive, OwesDefer>>

\* =============================================================================
\* CONSTANT HELPERS
\* =============================================================================

Instances == 1..NumInstances

Statuses == {"ready", "running", "terminating",
             "done", "failed", "dead_lettered", "terminated", "cancelled"}

SettledStatuses == {"done", "failed", "dead_lettered", "terminated", "cancelled"}

Outcomes == {"done", "failed", "terminated", "cancelled"}
NoOutcome == "none"

IsSettled(s) == s \in SettledStatuses

\* claimableStatus, minus the dead 'suspended' arm -- see header.
Claimable(s) == s \in {"ready", "running"}

\* deferPhaseOwed(status, hasDeferEvents, compacted), OwesDefer[i] standing in
\* for (hasDeferEvents || compacted).
DeferOwed(i) == Claimable(status[i]) /\ OwesDefer[i]

\* reReplayableStatuses (engine/store_admin_rereplay.go). Excludes 'done'
\* (replaying a finished run is pointless) and 'cancelled' (D4: an explicit
\* pre-emptive cancel is never re-replayable).
ReReplayable == {"failed", "terminated", "dead_lettered"}

\* A fixed, minimal two-level topology: only the LAST instance has a parent,
\* and it is instance 1, with close policy TERMINATE. See the header's scope
\* note on why this is fixed rather than a CONSTANT TLC is asked to vary.
ParentOf == [i \in Instances |-> IF i = NumInstances THEN 1 ELSE NULL]
ClosePolicy == [i \in Instances |-> IF i = NumInstances THEN "TERMINATE" ELSE "NONE"]

(*
  ClockCeiling / NextClock: clock is SELF-CLAMPING rather than bounded by a
  .cfg CONSTRAINT. Measured 2026-09-23, while known-positive-testing this
  file's liveness PROPERTIES (the same discipline SettledIsFinal's own
  comment above records for an INVARIANT): a CONSTRAINT that hard-walls an
  unboundedly-incrementing clock creates a state with NO constraint-
  satisfying successor at all once clock reaches the wall (every action
  here does clock'=clock+1, so every state at the wall is a dead end under
  the constraint). TLC's liveness engine treats that as a state that must
  stutter forever to satisfy [][Next]_vars, and a "just stutter at the
  wall" path is fairness-vacuous whenever the property's own witness action
  is not separately forced -- which silently defeats a liveness check
  rather than erroring.

  This was caught TWICE on this exact model. First on SettledIsFinal's
  original `[](P=>[]Q)` temporal form -- see its own comment. Second while
  known-positive-testing L2_ClaimProgress: removing WF_vars(Claim(w)) from
  Fairness left `<>(\E i: status[i]="running")` reporting "No error has
  been found" -- and a throwaway `ProbeNeverRunning` INVARIANT (ordinary
  reachability, not the liveness engine) confirmed "running" IS reachable
  in one Claim step, so the miss was not "Claim can't happen", it was the
  same wall-stuttering artifact defeating a `[](P=><>Q)` (leads-to) shape
  too -- a DIFFERENT temporal shape than the one that broke SettledIsFinal,
  which means this is not one property's bug, it is the CONSTRAINT
  mechanism itself, exactly as TLC's own "Declaring state or action
  constraints during liveness checking is dangerous" print warns.

  The fix: clock never reaches a value with no successor. NextClock clamps
  at ClockCeiling instead of a CONSTRAINT excluding clock'=ClockCeiling+1 --
  every action stays enabled forever once clock is pinned (it just clamps
  again), so there is no artificial dead end for a fair path to hide in.
  heartbeatAt/nextWakeAt/deferDeadline are unaffected: they are always set
  to clock or clock+<a fixed small CONSTANT>, so pinning clock still leaves
  them in a bounded range, and every comparison against them (`clock -
  heartbeatAt[i] >= Timeout`) stays meaningful -- a pinned clock reads as
  "very late", which is the correct, conservative reading, not a broken
  one. No .cfg CONSTRAINT is declared for this spec as a result -- there is
  nothing for one to exclude.

  Re-verified after the fix: the SAME two known-positive mutations (deleting
  Terminate's settled-refusal guard for SettledIsFinal; removing
  WF_vars(Claim(w)) for L2_ClaimProgress) are both caught -- see the
  dated measurements beside PROPERTIES below.
*)
ClockCeiling == 5
NextClock(c) == IF c < ClockCeiling THEN c + 1 ELSE ClockCeiling

(*
  FutureClock: every DEADLINE computed as clock+<timeout> (nextWakeAt,
  deferDeadline) must be clamped the same way clock' itself is, or the
  fix above is incomplete. Measured 2026-09-23, immediately after adding
  NextClock and dropping the CONSTRAINT: the full clean check (no mutation
  at all) found a REAL counter-example -- a Cancel at clock=5 sets
  deferDeadline[1]=7, clock then never advances past ClockCeiling=5, so
  ExpireDeferPhases's `clock >= deferDeadline[i]` guard is permanently
  false and L1_EventualSettlement is genuinely violated in THIS MODEL. Not
  a fact about the real system -- deferDeadline in production is a
  wall-clock timestamp that always eventually arrives; the violation is an
  artifact of pinning clock without also pinning the deadlines computed
  from it.

  Clamping to ClockCeiling itself (tried first) was STILL wrong, and a
  second clean-spec counter-example caught it, same day: deferDeadline
  clamped to 5 and clock pinned at 5 leaves `deferDeadline[i] < clock`
  (ExpiredDeferPhases' own comparison, line ~338 -- STRICT, matching
  engine/defer_phase.go) as `5 < 5`, false forever. Clamping one tick
  short of the ceiling fixes it without weakening the comparison to match
  a modeling convenience: once clock reaches ClockCeiling, any deadline
  clamped to ClockCeiling-1 satisfies both the strict deferDeadline
  comparison and nextWakeAt's non-strict `<=` (line ~329).
*)
FutureClock(c, offset) ==
    IF c + offset >= ClockCeiling THEN ClockCeiling - 1 ELSE c + offset

\* =============================================================================
\* TYPE INVARIANT
\* =============================================================================

TypeOK ==
    /\ status \in [Instances -> Statuses]
    /\ assignedTo \in [Instances -> Workers \cup {NULL}]
    /\ heartbeatAt \in [Instances -> Nat]
    /\ nextWakeAt \in [Instances -> Nat]
    /\ pendingTerminalStatus \in [Instances -> Outcomes \cup {NoOutcome}]
    /\ deferDeadline \in [Instances -> Nat]
    /\ clock \in Nat
    /\ alive \in [Workers -> BOOLEAN]
    /\ OwesDefer \in [Instances -> BOOLEAN]

\* =============================================================================
\* STATE PREDICATES
\* =============================================================================

ReadyInstances == {i \in Instances :
    status[i] \in {"ready", "terminating"} /\ nextWakeAt[i] <= clock}

RunningInstances == {i \in Instances : status[i] = "running"}

StaleInstances ==
    {i \in RunningInstances :
        ~alive[assignedTo[i]] \/ clock - heartbeatAt[i] >= HeartbeatTimeout}

ExpiredDeferPhases ==
    {i \in Instances : pendingTerminalStatus[i] /= NoOutcome /\ deferDeadline[i] < clock}

\* Instances eligible for a TERMINATE-policy cascade from parent p: p's
\* direct TERMINATE-policy children that are not already settled. Mirrors
\* enforceParentClosePolicyAt's WHERE clause (status NOT IN <settled list>),
\* which does NOT exclude 'terminating' -- a child already mid-defer-phase
\* for an unrelated reason is eligible too, and (per DeferOwed's own
\* Claimable guard) always lands in the direct arm, cutting its own cleanup
\* short exactly as a second Terminate/Cancel would.
CascadeEligible(p) ==
    {c \in Instances : ParentOf[c] = p /\ ClosePolicy[c] = "TERMINATE" /\ ~IsSettled(status[c])}

\* =============================================================================
\* ACTIONS -- claim / heartbeat / reap (CleatClaim.tla's own shape, extended)
\* =============================================================================

(*
  --- CLAIM: extends CleatClaim.tla's Claim(w) to also pick up 'terminating'
  instances -- cleat#1965/#2002 widened the WHERE clause to
  status IN ('ready','terminating') so a marked defer phase gets dispatched
  through the ordinary claim path. pendingTerminalStatus is untouched: the
  UPDATE's SET list never mentions it (engine/store_lifecycle.go
  ClaimWorkflows).
*)
Claim(w) ==
    /\ alive[w]
    /\ \E S \in SUBSET ReadyInstances :
        /\ S /= {}
        /\ Cardinality(S) <= MaxClaimBatch
        /\ status' = [i \in Instances |-> IF i \in S THEN "running" ELSE status[i]]
        /\ assignedTo' = [i \in Instances |-> IF i \in S THEN w ELSE assignedTo[i]]
        /\ heartbeatAt' = [i \in Instances |-> IF i \in S THEN clock ELSE heartbeatAt[i]]
        /\ clock' = NextClock(clock)
        /\ UNCHANGED <<nextWakeAt, pendingTerminalStatus, deferDeadline, alive, OwesDefer>>

Heartbeat(w) ==
    /\ alive[w]
    /\ \E i \in RunningInstances :
        /\ assignedTo[i] = w
        /\ heartbeatAt' = [heartbeatAt EXCEPT ![i] = clock]
        /\ clock' = NextClock(clock)
        /\ UNCHANGED <<status, assignedTo, nextWakeAt, pendingTerminalStatus,
                       deferDeadline, alive, OwesDefer>>

(*
  --- COMPLETE / FAIL / DEADLETTER: an ordinary segment finishing. Guarded on
  pendingTerminalStatus[i] = NoOutcome, because a claimed execution that
  DOES carry a pending outcome is a defer-phase replay, and the worker's own
  dispatch never routes its success or failure through these -- both go
  through FinalizeDeferPhase instead (cmd/cleat-worker/setup.go: the
  `wf.PendingTerminalStatus != ""` branch in writeTerminalFailure, and the
  symmetric success path documented at notifyTerminal's third call site).
  None of the three bumps a fence -- verified against
  finalizeWorkflowSegmentInner/CompleteWorkflow/FailWorkflow/
  MoveToDeadLetterQueue's own UPDATE statements, none of which SETs
  generation.
*)
Complete(w) ==
    /\ alive[w]
    /\ \E i \in RunningInstances :
        /\ assignedTo[i] = w
        /\ pendingTerminalStatus[i] = NoOutcome
        /\ status' = [status EXCEPT ![i] = "done"]
        /\ assignedTo' = [assignedTo EXCEPT ![i] = NULL]
        /\ clock' = NextClock(clock)
        /\ UNCHANGED <<heartbeatAt, nextWakeAt, pendingTerminalStatus,
                       deferDeadline, alive, OwesDefer>>

Fail(w) ==
    /\ alive[w]
    /\ \E i \in RunningInstances :
        /\ assignedTo[i] = w
        /\ pendingTerminalStatus[i] = NoOutcome
        /\ status' = [status EXCEPT ![i] = "failed"]
        /\ assignedTo' = [assignedTo EXCEPT ![i] = NULL]
        /\ clock' = NextClock(clock)
        /\ UNCHANGED <<heartbeatAt, nextWakeAt, pendingTerminalStatus,
                       deferDeadline, alive, OwesDefer>>

DeadLetter(w) ==
    /\ alive[w]
    /\ \E i \in RunningInstances :
        /\ assignedTo[i] = w
        /\ pendingTerminalStatus[i] = NoOutcome
        /\ status' = [status EXCEPT ![i] = "dead_lettered"]
        /\ assignedTo' = [assignedTo EXCEPT ![i] = NULL]
        /\ clock' = NextClock(clock)
        /\ UNCHANGED <<heartbeatAt, nextWakeAt, pendingTerminalStatus,
                       deferDeadline, alive, OwesDefer>>

(*
  --- RELEASE: a voluntary suspend (DurableSleep etc.). Uses the same CASE
  ReapStaleInstances does -- status goes to 'terminating' rather than 'ready'
  if a terminal outcome is already recorded, so a release never masks a
  pending decision as ordinary runnable work (engine/store_lifecycle.go
  ReleaseWorkflow's own comment: "the CASE ... for the same reason
  ReapStaleInstances does"). No fence bump -- the current holder is
  voluntarily relinquishing, not being reclaimed.
*)
Release(w) ==
    /\ alive[w]
    /\ \E i \in RunningInstances :
        /\ assignedTo[i] = w
        /\ status' = [status EXCEPT ![i] =
              IF pendingTerminalStatus[i] /= NoOutcome THEN "terminating" ELSE "ready"]
        /\ assignedTo' = [assignedTo EXCEPT ![i] = NULL]
        /\ nextWakeAt' = [nextWakeAt EXCEPT ![i] = FutureClock(clock, HeartbeatTimeout)]
        /\ clock' = NextClock(clock)
        /\ UNCHANGED <<heartbeatAt, pendingTerminalStatus, deferDeadline, alive, OwesDefer>>

(*
  --- REAP: the same CASE as Release, for the same instance. Bumps the fence
  (assignedTo cleared -- see header on why that alone models the real
  generation bump) because this is an INVOLUNTARY reclaim: the worker that
  held it may still believe it does.
*)
Reap ==
    \/ (\E i \in StaleInstances :
        /\ status' = [status EXCEPT ![i] =
              IF pendingTerminalStatus[i] /= NoOutcome THEN "terminating" ELSE "ready"]
        /\ assignedTo' = [assignedTo EXCEPT ![i] = NULL]
        /\ heartbeatAt' = [heartbeatAt EXCEPT ![i] = 0]
        /\ clock' = NextClock(clock)
        /\ UNCHANGED <<nextWakeAt, pendingTerminalStatus, deferDeadline, alive, OwesDefer>>)
    \/ (/\ clock' = NextClock(clock)
        /\ UNCHANGED <<status, assignedTo, heartbeatAt, nextWakeAt,
                       pendingTerminalStatus, deferDeadline, alive, OwesDefer>>)

\* =============================================================================
\* ACTIONS -- the two-phase defer transition (D6, engine/defer_phase.go)
\* =============================================================================

(*
  --- MARKORAPPLY: the shared shape of preemptivelySettle and adminForceResolve
  (engine/db.go, engine/store_admin.go). Both split identically on
  DeferOwed(i): mark it 'terminating' and record the outcome (phase 1), or
  apply the outcome to a claimable-but-not-owing instance directly
  (finalizeInstance below with the one-phase shape). Kept as one operator
  because every one of Terminate/Cancel/AdminForceComplete/AdminForceFail/
  CascadeTerminate is this same split with a different outcome and a
  different settlement guard -- writing it five times is how the six-member
  set-difference defect earlier in this file's own project (CLAUDE.md's
  "one prefix assumption produced all three errors") happens again.
*)
MarkOrApply(i, outcome) ==
    IF DeferOwed(i) THEN
        /\ status' = [status EXCEPT ![i] = "terminating"]
        /\ pendingTerminalStatus' = [pendingTerminalStatus EXCEPT ![i] = outcome]
        /\ deferDeadline' = [deferDeadline EXCEPT ![i] = FutureClock(clock, DeferPhaseTimeout)]
        /\ nextWakeAt' = [nextWakeAt EXCEPT ![i] = clock]
        /\ assignedTo' = [assignedTo EXCEPT ![i] = NULL]
        /\ UNCHANGED heartbeatAt
    ELSE
        /\ status' = [status EXCEPT ![i] = outcome]
        /\ assignedTo' = [assignedTo EXCEPT ![i] = NULL]
        /\ pendingTerminalStatus' = [pendingTerminalStatus EXCEPT ![i] = NoOutcome]
        /\ deferDeadline' = [deferDeadline EXCEPT ![i] = 0]
        /\ UNCHANGED <<heartbeatAt, nextWakeAt>>

(*
  --- TERMINATE: preemptivelySettle(finalStatus=terminated). cleat#1975 (D3):
  settled is final, with the ONE documented exception -- a dead-lettered run
  taken off the queue by the DLQ's own terminate route.
*)
Terminate(i) ==
    /\ ~IsSettled(status[i]) \/ status[i] = "dead_lettered"
    /\ MarkOrApply(i, "terminated")
    /\ clock' = NextClock(clock)
    /\ UNCHANGED <<alive, OwesDefer>>

(*
  --- CANCEL: preemptivelySettle(finalStatus=cancelled). Same D3 refusal,
  with NO dead-letter exception -- CancelWorkflow's finalStatus is never
  "terminated", so preemptivelySettle's one carve-out never applies here.
*)
Cancel(i) ==
    /\ ~IsSettled(status[i])
    /\ MarkOrApply(i, "cancelled")
    /\ clock' = NextClock(clock)
    /\ UNCHANGED <<alive, OwesDefer>>

(*
  --- ADMINFORCECOMPLETE / ADMINFORCEFAIL: adminForceResolve. cleat#1975
  (D3): settled is final with NO exception at all -- unlike Terminate, there
  is no dead-letter carve-out (TestAdminForceComplete_ClearsAnEarlierFailure's
  own retirement is the comment on this in engine/store_admin.go).
*)
AdminForceComplete(i) ==
    /\ ~IsSettled(status[i])
    /\ MarkOrApply(i, "done")
    /\ clock' = NextClock(clock)
    /\ UNCHANGED <<alive, OwesDefer>>

AdminForceFail(i) ==
    /\ ~IsSettled(status[i])
    /\ MarkOrApply(i, "failed")
    /\ clock' = NextClock(clock)
    /\ UNCHANGED <<alive, OwesDefer>>

(*
  --- FINALIZEDEFERPHASE: the worker that claimed a 'terminating' instance
  (now 'running', per Claim above) applies the recorded outcome after
  running the defer segment. Fenced on assignedTo[i] = w -- see header on why
  that alone models the real (assigned_to, generation, pending_terminal_status
  IS NOT NULL) fence.
*)
FinalizeDeferPhase(w) ==
    /\ alive[w]
    /\ \E i \in RunningInstances :
        /\ assignedTo[i] = w
        /\ pendingTerminalStatus[i] /= NoOutcome
        /\ status' = [status EXCEPT ![i] = pendingTerminalStatus[i]]
        /\ pendingTerminalStatus' = [pendingTerminalStatus EXCEPT ![i] = NoOutcome]
        /\ assignedTo' = [assignedTo EXCEPT ![i] = NULL]
        /\ deferDeadline' = [deferDeadline EXCEPT ![i] = 0]
        /\ clock' = NextClock(clock)
        /\ UNCHANGED <<heartbeatAt, nextWakeAt, alive, OwesDefer>>

(*
  --- EXPIREDEFERPHASES: the unfenced sweep. Applies to ANY instance whose
  deadline has passed, regardless of current status or holder -- the real
  UPDATE's WHERE clause is `pending_terminal_status IS NOT NULL AND
  defer_phase_deadline < now()`, nothing else. This is what bounds a
  guest that traps on every replay attempt: the phase ends even if no
  worker ever finishes it. Bumps the fence (a worker still mid-phase loses
  its FinalizeDeferPhase race and gets ErrFenceLost, per the doc comment on
  ExpireDeferPhases in store_defer_phase.go).
*)
ExpireDeferPhases ==
    /\ \E i \in ExpiredDeferPhases :
        /\ status' = [status EXCEPT ![i] = pendingTerminalStatus[i]]
        /\ pendingTerminalStatus' = [pendingTerminalStatus EXCEPT ![i] = NoOutcome]
        /\ assignedTo' = [assignedTo EXCEPT ![i] = NULL]
        /\ deferDeadline' = [deferDeadline EXCEPT ![i] = 0]
        /\ clock' = NextClock(clock)
        /\ UNCHANGED <<heartbeatAt, nextWakeAt, alive, OwesDefer>>

\* =============================================================================
\* ACTIONS -- redrive verbs
\* =============================================================================

(*
  --- RETRYWORKFLOW: dead_lettered only, in place. Fenced on status alone --
  no assignedTo/generation check at all (RetryWorkflow's WHERE clause is
  `id = $1 AND status = 'dead_lettered'`), which is safe because a
  dead_lettered run's assignedTo is already NULL by construction.
*)
RetryWorkflow(i) ==
    /\ status[i] = "dead_lettered"
    /\ status' = [status EXCEPT ![i] = "ready"]
    /\ nextWakeAt' = [nextWakeAt EXCEPT ![i] = clock]
    /\ clock' = NextClock(clock)
    /\ UNCHANGED <<assignedTo, heartbeatAt, pendingTerminalStatus, deferDeadline, alive, OwesDefer>>

(*
  --- ADMINREREPLAY: resumes from recorded history rather than starting a
  new run. reReplayableStatuses excludes 'done' (pointless) and 'cancelled'
  (D4: an explicit pre-emptive cancel is never re-replayable).
*)
AdminReReplay(i) ==
    /\ status[i] \in ReReplayable
    /\ status' = [status EXCEPT ![i] = "ready"]
    /\ assignedTo' = [assignedTo EXCEPT ![i] = NULL]
    /\ nextWakeAt' = [nextWakeAt EXCEPT ![i] = clock]
    /\ clock' = NextClock(clock)
    /\ UNCHANGED <<heartbeatAt, pendingTerminalStatus, deferDeadline, alive, OwesDefer>>

\* =============================================================================
\* ACTIONS -- the parent-close cascade (D1, cleat#1978)
\* =============================================================================

(*
  --- CASCADETERMINATE: a settled parent p closes every TERMINATE-policy
  child that has not already settled, in one bulk step -- matching
  enforceParentClosePolicyAt's own two UPDATE statements (direct and
  defer-owed arms), each a set-based write over every eligible child at
  once, not a per-child loop. error_op='parent_close' (cleat#1978) is a data
  field, not a status, and is not modeled -- same call as error_code above.
*)
CascadeTerminate(p) ==
    /\ IsSettled(status[p])
    /\ LET E == CascadeEligible(p)
           Direct == {c \in E : ~DeferOwed(c)}
           Mark == {c \in E : DeferOwed(c)}
       IN
        /\ E /= {}
        /\ status' = [i \in Instances |->
              IF i \in Direct THEN "terminated"
              ELSE IF i \in Mark THEN "terminating"
              ELSE status[i]]
        /\ assignedTo' = [i \in Instances |->
              IF i \in (Direct \cup Mark) THEN NULL ELSE assignedTo[i]]
        /\ pendingTerminalStatus' = [i \in Instances |->
              IF i \in Mark THEN "terminated"
              ELSE IF i \in Direct THEN NoOutcome
              ELSE pendingTerminalStatus[i]]
        /\ deferDeadline' = [i \in Instances |->
              IF i \in Mark THEN FutureClock(clock, DeferPhaseTimeout)
              ELSE IF i \in Direct THEN 0
              ELSE deferDeadline[i]]
        /\ nextWakeAt' = [i \in Instances |->
              IF i \in Mark THEN clock ELSE nextWakeAt[i]]
        /\ clock' = NextClock(clock)
        /\ UNCHANGED <<heartbeatAt, alive, OwesDefer>>

\* =============================================================================
\* CRASH / RESTART / TICK (CleatClaim.tla, unchanged)
\* =============================================================================

Crash(w) ==
    /\ alive[w]
    /\ alive' = [alive EXCEPT ![w] = FALSE]
    /\ clock' = NextClock(clock)
    /\ UNCHANGED <<status, assignedTo, heartbeatAt, nextWakeAt,
                   pendingTerminalStatus, deferDeadline, OwesDefer>>

Restart(w) ==
    /\ ~alive[w]
    /\ alive' = [alive EXCEPT ![w] = TRUE]
    /\ clock' = NextClock(clock)
    /\ UNCHANGED <<status, assignedTo, heartbeatAt, nextWakeAt,
                   pendingTerminalStatus, deferDeadline, OwesDefer>>

Tick ==
    /\ clock' = NextClock(clock)
    /\ UNCHANGED <<status, assignedTo, heartbeatAt, nextWakeAt,
                   pendingTerminalStatus, deferDeadline, alive, OwesDefer>>

\* =============================================================================
\* NEXT-STATE RELATION
\* =============================================================================

Next ==
    \/ (\E w \in Workers :
        Claim(w) \/ Heartbeat(w) \/ Complete(w) \/ Fail(w) \/ DeadLetter(w)
        \/ Release(w) \/ FinalizeDeferPhase(w) \/ Crash(w))
    \/ (\E w \in Workers : Restart(w))
    \/ Reap
    \/ ExpireDeferPhases
    \/ (\E i \in Instances : Terminate(i))
    \/ (\E i \in Instances : Cancel(i))
    \/ (\E i \in Instances : AdminForceComplete(i))
    \/ (\E i \in Instances : AdminForceFail(i))
    \/ (\E i \in Instances : RetryWorkflow(i))
    \/ (\E i \in Instances : AdminReReplay(i))
    \/ (\E p \in Instances : CascadeTerminate(p))
    \/ Tick

\* =============================================================================
\* INITIAL STATE
\* =============================================================================

Init ==
    /\ status = [i \in Instances |-> "ready"]
    /\ assignedTo = [i \in Instances |-> NULL]
    /\ heartbeatAt = [i \in Instances |-> 0]
    /\ nextWakeAt = [i \in Instances |-> 0]
    /\ pendingTerminalStatus = [i \in Instances |-> NoOutcome]
    /\ deferDeadline = [i \in Instances |-> 0]
    /\ clock = 0
    /\ alive = [w \in Workers |-> TRUE]
    /\ OwesDefer \in [Instances -> BOOLEAN]

\* =============================================================================
\* FAIRNESS
\* =============================================================================

(*
  Fair: every worker's claim and management actions, Reap, ExpireDeferPhases
  (so a marked phase does not stall forever if no worker ever finalizes it),
  and CascadeTerminate for every instance (so a settled parent's TERMINATE
  children do not wait forever).

  NOT fair, deliberately, matching CleatClaim.tla's own choice for Crash/
  Restart: Terminate, Cancel, AdminForceComplete, AdminForceFail,
  RetryWorkflow, AdminReReplay. These are operator/HTTP-invoked, and nothing
  in the real system forces an operator to ever call them -- L1 below states
  what IS guaranteed once one of them fires, not that one ever will.

  STRONG fairness (SF), not weak (WF), on the two worker-gated clauses --
  this is a correction, not a stylistic choice, and it was NOT copied from
  CleatClaim.tla (which uses WF for the identical clause; see below).
  Measured 2026-09-23, on the CLEAN spec, no mutation: with WF, a
  known-positive check of L2_ClaimProgress (removing Claim's fairness
  entirely, to confirm the property could detect a genuine stall) instead
  turned up a DIFFERENT, real counter-example that needed no mutation at
  all -- Workers={w1}, and an unfair Crash/Restart lets w1 toggle
  alive[w1] every single tick forever (Crash immediately after every
  Restart), so alive[w1] is never CONTINUOUSLY true and WF_vars(Claim(w1))
  is never forced, even though an instance sits in ReadyInstances the
  entire time. That is a real gap in WF, not a CONSTRAINT artifact: Claim(w1)
  IS enabled infinitely often (every state where the loop passes through
  "alive[w1]=TRUE, something ready" -- which recurs forever), just not
  continuously -- exactly the condition SF is defined to force and WF is
  not. Switching these two clauses to SF closes it: confirmed the same
  scenario no longer produces a counter-example, and confirmed SF is still
  the CORRECT strength by re-running the ORIGINAL known-positive (deleting
  Terminate's settled-refusal guard) against SettledIsFinal -- an action
  invariant, unaffected by either WF or SF since it does not depend on
  Fairness at all -- to confirm this change did not accidentally paper over
  a real bug by weakening what gets explored.

  CleatClaim.tla's OWN ClaimProgress carries the identical exposure and is
  NOT fixed by this change (it is a separate file). Verified 2026-09-23: a
  known-positive test against CleatClaim.tla itself (removing its
  WF_vars(Claim(w)) the same way) ALSO reports "No error has been found"
  -- filed as cleat#2034 rather than silently patched here, since
  CleatClaim.tla is cleat#1996's already-merged artifact and this file's
  own scope is #1997, not a re-audit of #1996 (CLAUDE.md's "one PR, one
  thing").
*)
Fairness ==
    /\ \A w \in Workers : SF_vars(Claim(w))
    /\ \A w \in Workers :
        SF_vars(Heartbeat(w) \/ Complete(w) \/ Fail(w) \/ DeadLetter(w)
                 \/ Release(w) \/ FinalizeDeferPhase(w))
    /\ WF_vars(Reap)
    /\ WF_vars(ExpireDeferPhases)
    /\ \A p \in Instances : WF_vars(CascadeTerminate(p))

\* =============================================================================
\* COMPLETE SPECIFICATION
\* =============================================================================

(*
  FleetEventuallyStable: eventually, permanently, at least one worker is
  alive. NOT action fairness (Crash/Restart stay deliberately unfair, per
  the header above) -- a raw temporal ASSUMPTION on the whole system,
  added after the SF fix above (Fairness comment) still left a genuine,
  no-mutation-needed counter-example: Workers={w1}, Crash w1 once, and
  simply never Restart it. `\E w: alive[w]` is true for finitely many
  early states (so L2's obligation IS created) then FALSE forever after
  (so nothing can ever discharge it) -- SF cannot help, because
  SF_vars(Claim(w1)) only fires if Claim is enabled INFINITELY OFTEN, and
  once w1 is permanently dead it is enabled exactly zero more times.

  This is not a defect in L1/L2/L3's wording, it is a genuine scope
  question, and "the entire fleet crashes and NO orchestrator ever
  restarts anything, forever" is not a scenario any of the three
  properties should be expected to survive -- of course ready work sits
  forever if literally every worker is permanently gone. The standard
  TLA+ answer is exactly this: state the assumption the properties
  actually depend on (the fleet is not permanently, irrecoverably dead)
  as its own conjunct, rather than folding it into per-action fairness.
  Re-verified after adding it: the same "Crash once, never Restart"
  scenario is no longer a valid behavior of Spec (FleetEventuallyStable
  rules it out directly), and the two-worker/alternating-crash-loop
  scenario from the Fairness comment above is unaffected (SF still
  carries that one).
*)
FleetEventuallyStable == <>[](\E w \in Workers : alive[w])

Spec == Init /\ [][Next]_vars /\ Fairness /\ FleetEventuallyStable

\* No .cfg CONSTRAINT here, deliberately -- unlike CleatClaim.tla, whose
\* clock is genuinely unbounded and needs one. This model's clock is
\* self-clamping (NextClock/ClockCeiling, in CONSTANT HELPERS above); see
\* that comment for why a CONSTRAINT was tried first and found to silently
\* defeat liveness checking.

\* =============================================================================
\* SAFETY INVARIANTS
\* =============================================================================

(*
  AtMostOneClaimHolder (S4): a 'running' instance has exactly one worker
  holding it; every other status has no holder. 'terminating' is included in
  the "no holder" side deliberately -- Claim(w) is what turns a marked
  instance into 'running' with a holder; it is never left as 'terminating'
  with assignedTo set.
*)
AtMostOneClaimHolder ==
    /\ \A i \in Instances : status[i] = "running" => assignedTo[i] \in Workers
    /\ \A i \in Instances : status[i] /= "running" => assignedTo[i] = NULL

(*
  FencedWriteNeverApplies (S2): every action above is guarded on
  assignedTo[i] = w (worker actions) or on no residual holder at all
  (operator/sweep actions never check a worker identity, matching the real
  fences described in the header). Stated as an invariant for
  cross-checking with TLC, the same way CleatClaim.tla's AtMostOnce
  documents a structural guarantee rather than searching for a
  counterexample no action can produce.
*)
FencedWriteNeverApplies == AtMostOneClaimHolder

(*
  PendingOutcomeConsistency: pendingTerminalStatus is set if and only if the
  instance is either 'terminating' or a 'running' claim replaying a marked
  phase -- never on a settled instance (every settling action clears it) and
  never on a plain 'ready' instance (nothing sets it without also moving the
  status off 'ready').
*)
PendingOutcomeConsistency ==
    \A i \in Instances :
        pendingTerminalStatus[i] /= NoOutcome => status[i] \in {"terminating", "running"}

Safety == TypeOK /\ AtMostOneClaimHolder /\ PendingOutcomeConsistency

\* =============================================================================
\* LIVENESS
\* =============================================================================

(*
  NOT YET GATED IN CleatRunLifecycle.cfg -- L1/L2/L3 below are DEFINED, not
  CHECKED. INVARIANTS (TypeOK, Safety) and SettledIsFinal (an action
  invariant, immune to the issue below) are gated and verified; these three
  are not, and shipping them gated would have been a false claim.

  Why, stated plainly rather than left for a reader to reconstruct: every
  one of these three is a `[](P => <>Q)` leads-to property, and this
  model -- unlike CleatClaim.tla, which it extends -- has several UNFAIR,
  single-step operator/redrive actions (Terminate, Cancel, AdminReReplay,
  AdminForceComplete/Fail; unfair deliberately, see the Fairness comment)
  that can ESTABLISH an antecedent P and then REVOKE it one step later,
  before any fairness-dependent mechanism -- weak OR strong, both are
  asymptotic guarantees over a PERSISTING precondition -- gets even one
  qualifying window to react. Three DISTINCT, real, no-mutation-needed
  counter-examples of this shape were found on the CLEAN spec in one
  session, 2026-09-23, each fixed by adding an honest "or the antecedent
  itself later became false" escape clause (see L2 and L3's own comments
  below for the fix and the trace that demanded it) -- and each fix
  uncovered a DIFFERENT instance of the same root cause rather than
  converging: L2 first via a single-worker Crash/Restart alternation
  (fixed: WF -> SF on the worker-gated Fairness clauses), then via a
  worker permanently crashing and never restarting (fixed:
  FleetEventuallyStable), then L3 via AdminReReplay racing
  CascadeTerminate's own IsSettled(status[p]) guard (fixed: the `\/
  <>(~IsSettled(status[p]))` escape).

  That pattern not converging after three fixes -- each one closing a
  specific trace rather than the general hazard -- is the reason gating
  stopped here rather than continuing to iterate: this repo's own rule
  applies directly ("A count answers 'did this go up'. It never answers
  'is anything still missing'" / "could this check have disagreed?" from
  CLAUDE.md's own "Is this result real?"). Nothing here demonstrates L1
  itself has an unfound counter-example of the same shape (none was found
  for it specifically, and a first read suggests its antecedent --
  pendingTerminalStatus[i]/=NoOutcome -- is NOT revocable by AdminReReplay,
  since ReReplayable excludes "terminating") -- but "not yet found" is not
  "verified", and reading through every remaining unfair-action
  interaction to be certain was the piece this session ran out of budget
  for. Tracked for a follow-up rather than left implicit, per CLAUDE.md's
  "a decision not to do something lives where the doing would have gone".
*)

(*
  L1 -- EventualSettlement: once an instance has a recorded pending outcome
  (marked by Terminate, Cancel, AdminForceComplete/Fail, or CascadeTerminate),
  it eventually reaches a settled status. Guaranteed by FinalizeDeferPhase
  under fairness, and by ExpireDeferPhases as the backstop when no worker
  ever finishes the phase -- this is the property the whole two-phase
  mechanism exists to provide (engine/defer_phase.go's own words: "a
  terminate that cannot run its defers must still terminate").
*)
L1_EventualSettlement ==
    \A i \in Instances :
        [](pendingTerminalStatus[i] /= NoOutcome => <>(IsSettled(status[i])))

(*
  L2 -- ClaimProgress: NOT CleatClaim.tla's own ClaimProgress verbatim,
  despite an earlier version of this comment claiming it was -- and the gap
  is a real modeling
  defect, caught on the CLEAN, UNMUTATED spec 2026-09-23 (no known-positive
  mutation needed): CleatClaim.tla has no way to resolve a 'ready' instance
  except Claim, so "eventually something is running" is the right statement
  there. This model added five operator verbs that CAN resolve a ready
  instance without ever claiming it -- Terminate and Cancel both apply
  directly from 'ready' (see their guards), and AdminReReplay can put an
  instance back in 'ready' only to have a second Terminate/Cancel take it
  again. A found counter-example: instance 2 starts ready, an alive worker
  exists, but Crash disables Claim's continuous-enabledness before
  WF_vars(Claim(w1)) ever has to fire, and Terminate/Cancel/AdminReReplay
  (deliberately UNFAIR -- see the Fairness comment) resolve both instances
  to 'cancelled' without either ever passing through 'running'. That is
  completely legitimate real-system behaviour -- an operator terminating
  unclaimed work is not a bug -- so "\E i: running" is simply too strong a
  consequent once operator verbs are in scope.

  The corrected consequent: either something gets claimed, OR the ready set
  drains some other way. This is still a genuine liveness statement (a
  stall where ReadyInstances stays nonempty AND alive[w] stays true AND
  Claim never fires forever, and no operator drains it, DOES catch a real
  progress bug -- WF_vars(Claim(w)) would be continuously enabled and force
  it), and it is exactly what actually holds of the real system: ready work
  does not sit forever untouched, but "resolved by an operator before being
  claimed" is a legitimate way for it to stop being untouched.
*)
L2_ClaimProgress ==
    []( (\E w \in Workers : alive[w]) /\ ReadyInstances /= {}
        => <>(\E i \in Instances : status[i] = "running") \/ <>(ReadyInstances = {}) )

(*
  L3 -- CascadeProgress: once a parent settles, every TERMINATE-policy child
  it has eventually settles too. This is the property #1998 depends on this
  model for -- it says the cascade's WRITE eventually happens; #1998 is
  where an awaiting ancestor eventually SEEING it is checked.

  Carries the SAME escape L2 needed, for the same reason, found the same
  way: a clean-spec (no mutation) counter-example, 2026-09-23. AdminReReplay
  is a single UNFAIR action (deliberately, per the Fairness comment -- it is
  operator-invoked) that can revoke IsSettled(status[p]) the very next step
  after it becomes true. CascadeTerminate(p)'s own guard IS
  IsSettled(status[p]) -- if that is only momentarily true and then
  redriven away before CascadeTerminate's fairness gets even one
  continuously/infinitely-often-enabled window, NEITHER weak NOR strong
  fairness can force it: both are asymptotic guarantees over a persisting
  precondition, and an adversary needing only one step to revoke it defeats
  either equally. Found trace: AdminForceFail settles instance 1 (parent),
  AdminReReplay redrives it back to 'ready' on the very next step -- the
  cascade obligation toward instance 2 (the TERMINATE child) was created at
  the settle and is then never met, because the thing meant to meet it lost
  its own precondition before it could react.

  The corrected consequent allows the same honest exit L2 uses: either the
  children settle, OR the parent itself stops being settled (the redrive
  voids the cascade obligation this specific settlement created -- a
  redriven parent is, correctly, no longer "a settled parent with
  unresolved TERMINATE children" at all; whatever cascade its NEXT
  settlement owes is a fresh obligation, covered by the next time this
  property's antecedent fires).
*)
L3_CascadeProgress ==
    \A p \in Instances :
        [](IsSettled(status[p])
            => <>(\A c \in Instances :
                    (ParentOf[c] = p /\ ClosePolicy[c] = "TERMINATE") => IsSettled(status[c]))
               \/ <>(~IsSettled(status[p])))

(*
  SettledIsFinal (S1, failure-model-2026-09-22.md's I1, cleat#1975/D3):
  once an instance reaches a settled status, no action ever moves it to a
  DIFFERENT value -- not merely "stays in the settled set", which would let
  'terminated' silently become 'cancelled'. This is the property the
  pre-#1975 code broke (B1 in the failure-model doc): preemptivelySettle's
  UPDATE had no status predicate at all, so a 'done' run could become
  'cancelled' with its result untouched underneath the new status.

  Written as an ACTION invariant ([][A]_vars), NOT as the more natural-looking
  temporal form `[](status[i]=v => [](status[i]=v))`. That form was tried
  first and is WRONG to use here -- TLC prints "Declaring state or action
  constraints during liveness checking is dangerous" for a reason, and this
  is the reason: measured 2026-09-23, deliberately reintroducing the pre-#1975
  B1 bug (deleting Terminate's settled-refusal guard entirely) left the
  `[](P=>[]Q)` form reporting "No error has been found" -- 48076 states, an
  IDENTICAL count to the unmutated run. The mutation reached a genuine 'done'
  -> 'terminated' transition (confirmed by tracing it with a throwaway
  `[][...]_vars` probe, which caught it in 2 steps, instantly, with no
  liveness warning at all) that the temporal form's interaction with
  CONSTRAINT ClockBound silently swallowed. The action form below checks
  purely during ordinary reachability analysis, the same way an INVARIANT
  does, and is not exposed to that interaction. Known-positive re-verified
  against this exact form after rewriting: same mutation, caught immediately
  -- but the FIRST version of this action form was ALSO wrong, and in the
  interesting direction: it said a settled instance's status may never
  change AT ALL, which is false. RetryWorkflow and AdminReReplay are
  DOCUMENTED redrive verbs (see the state machine diagram above) and both
  legitimately move a settled instance back to 'ready' -- re-replaying a
  'terminated' run is exactly what AdminReReplay is for. The all-or-nothing
  form flagged that as a violation on the CLEAN, unmutated spec (Terminate
  then AdminReReplay, 3 steps -- no mutation needed to trip it). "Settled is
  final" (D3) means no action OVERWRITES a settled status with a DIFFERENT
  terminal outcome; it does not mean a settled run can never be redriven.
  The refinement below says a settled instance's status may change only to
  'ready' (every redrive verb's target) -- and this SECOND version was also
  wrong, caught on the clean spec again with no mutation: Claim, DeadLetter,
  Terminate is a real 4-state trace (ready -> running -> dead_lettered ->
  terminated) via the ONE documented exception D3 states explicitly --
  "the dead-letter queue's own terminate route" -- which this file's own
  Terminate action encodes (`status[i] = "dead_lettered"` in its guard).
  Two known-positives on the clean spec before the invariant matched the
  code it is meant to check, not one; the second was found by the same
  mechanism as the first, just further down the same list of legitimate
  exceptions D3's own text names.
*)
SettledIsFinal ==
    [][\A i \in Instances :
         (IsSettled(status[i]) /\ status[i] /= status'[i])
            => status'[i] = "ready" \/ (status[i] = "dead_lettered" /\ status'[i] = "terminated")]_vars

====
