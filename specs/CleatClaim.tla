--------------------------- MODULE CleatClaim ----------------------------
(*
  Cleat Worker Claim Protocol
\*   ===========================

  Multiple workers claim workflow instances from a shared PostgreSQL queue
  using SELECT ... FOR UPDATE SKIP LOCKED. Each instance goes through the
  lifecycle: ready -> claimed by worker -> heartbeat loop -> completed /
  suspended / reaped.

  Implemented by (no line numbers -- CLAUDE.md's own rule on why a number
  like that rots faster than the fact it describes; CI's path filter matches
  on these file names, not on line ranges):
    - engine/store_lifecycle.go   (PostgresStore: ClaimWorkflows, ReapStaleInstances)
    - engine/mysql_lifecycle.go   (MySQLStore: ClaimWorkflows, HeartbeatBatchFenced, ReapStaleInstances)
    - engine/mssql_lifecycle.go   (MSSQLStore: ClaimWorkflows, HeartbeatBatchFenced)
    - engine/mssql_operations.go  (MSSQLStore: ReapStaleInstances)
    - engine/sharded_store.go     (ShardedStore: fans the above out per shard)
    - engine/db.go                (PostgresStore: HeartbeatBatchFenced)
    - cmd/cleat-worker/setup.go   (worker loops: heartbeatLoop, dispatch, executeWorkflow)

  This list moved from "internal/host/db.go" (a single file, pre-3eeb74e)
  to per-dialect files across a 2026-09 fencing rewrite (cleat#2008); the
  Complete/Fail/Release names in the original comment no longer match a
  single function each either. The model's actions are unaffected -- see
  "Known drift" in specs/README.md for what IS affected.

  State machine (instance lifecycle):

          +---> ready <--------+-------+
          |        |           |       |
          |        | claim     | reap  | release (suspend)
          |        v           |       |
          |     running --------+       |
          |        |                    |
          |        |--- complete --> done
          |        |--- fail ------> failed
          +--------+

  This is a raw TLA+ specification (not PlusCal) because the interaction of
  multiple concurrent workers, crashes, restarts, and a background reaper is
  clearest as a set of declarative actions with temporal fairness.

  HOW TO MODEL CHECK:

  1. Create CleatClaim.cfg (template in comments at bottom of file)
  2. Run: java -cp tla2tools.jar tlc2.TLC CleatClaim.tla -config CleatClaim.cfg
  3. Expect a few thousand distinct states with 3 workers, 5 instances.
*)

EXTENDS Integers, FiniteSets

CONSTANTS
    Workers,              \* Set of worker identifiers, e.g. {"w1", "w2", "w3"}
    NumInstances,         \* Number of workflow instances, modelled as 1..NumInstances
    HeartbeatInterval,    \* Logical time between heartbeats (model parameter)
    HeartbeatTimeout,     \* How much clock advance before a heartbeat goes stale
    MaxClaimBatch,        \* Maximum instances claimed in one batch query
    NULL                  \* Sentinel "unassigned" value, distinct from every worker --
                           \* a model value bound in CleatClaim.cfg, not derived, since
                           \* CHOOSE x : x \notin Workers is unbounded and TLC cannot
                           \* evaluate it.

ASSUME HeartbeatTimeout > HeartbeatInterval
ASSUME MaxClaimBatch >= 1
ASSUME NULL \notin Workers

\* =============================================================================
\* VARIABLES
\* =============================================================================

VARIABLES
    status,          \* [1..NumInstances -> {"ready","running","done","failed"}]
    assignedTo,      \* [1..NumInstances -> Workers U {NULL}]
    heartbeatAt,     \* [1..NumInstances -> Nat] clock value of last heartbeat
    nextWakeAt,      \* [1..NumInstances -> Nat] earliest clock for re-claim after release
    clock,           \* Nat global logical clock, advances on every action
    alive            \* [Workers -> BOOLEAN] whether each worker is alive

vars == <<status, assignedTo, heartbeatAt, nextWakeAt, clock, alive>>

\* =============================================================================
\* CONSTANT HELPERS
\* =============================================================================

Instances == 1..NumInstances

\* =============================================================================
\* TYPE INVARIANT
\* =============================================================================

TypeOK ==
    /\ status \in [Instances -> {"ready", "running", "done", "failed"}]
    /\ assignedTo \in [Instances -> Workers \cup {NULL}]
    /\ heartbeatAt \in [Instances -> Nat]
    /\ nextWakeAt \in [Instances -> Nat]
    /\ clock \in Nat
    /\ alive \in [Workers -> BOOLEAN]

\* =============================================================================
\* STATE PREDICATES (helpers used by actions and properties)
\* =============================================================================

\* Instances eligible for claiming: status is ready AND wake time has passed.
ReadyInstances == {i \in Instances : status[i] = "ready" /\ nextWakeAt[i] <= clock}

\* Currently claimed (running) instances.
RunningInstances == {i \in Instances : status[i] = "running"}

\* Terminal instances — done or failed, never to transition again.
TerminalInstances == {i \in Instances : status[i] \in {"done", "failed"}}

\* Instances whose heartbeat has gone stale. A stale instance can be reaped
\* back to ready. Conditions: status is running AND either the assigned worker
\* has crashed (no more heartbeats coming) OR the heartbeat is too old.
StaleInstances ==
    {i \in RunningInstances :
        ~alive[assignedTo[i]] \/ clock - heartbeatAt[i] >= HeartbeatTimeout}

\* =============================================================================
\* ACTIONS
\* =============================================================================

(*
  --- CLAIM: Worker atomically claims a batch of ready instances.

  Maps to SQL (db.go:295-312):
    UPDATE workflow_instances
    SET status = 'running', assigned_to = $1, heartbeat_at = now()
    WHERE id IN (
        SELECT id FROM workflow_instances
        WHERE status = 'ready' AND next_wake_at <= now()
        ORDER BY created_at
        LIMIT $limit
        FOR UPDATE SKIP LOCKED
    )
    RETURNING id

  The SKIP LOCKED clause means multiple workers claim disjoint subsets
  without blocking.  In TLA+ this is modelled naturally by the interleaving
  semantics: each Claim action atomically grabs a subset of the currently
  ready instances that haven't been taken by a concurrent action.  Because
  actions are atomic in TLA+, two concurrent Claim actions are serialised:
  the first picks its subset from ReadyInstances, the second picks from the
  remaining instances.  This is exactly what FOR UPDATE SKIP LOCKED
  guarantees — disjoint, non-blocking subsets.
*)
Claim(w) ==
    /\ alive[w]                                    \* dead workers don't claim
    /\ \E S \in SUBSET ReadyInstances :
        /\ S /= {}                                 \* claim at least one
        /\ Cardinality(S) <= MaxClaimBatch          \* respect batch limit
        /\ status'  = [i \in Instances |->
            IF i \in S THEN "running" ELSE status[i]]
        /\ assignedTo' = [i \in Instances |->
            IF i \in S THEN w ELSE assignedTo[i]]
        /\ heartbeatAt' = [i \in Instances |->
            IF i \in S THEN clock ELSE heartbeatAt[i]]
        /\ clock' = clock + 1
        /\ UNCHANGED <<nextWakeAt, alive>>

(*
  --- HEARTBEAT: Worker refreshes the heartbeat timestamp for an instance
      it currently owns.  This prevents the reaper from reclaiming it.

  Maps to SQL (db.go:601-612):
    UPDATE workflow_instances SET heartbeat_at = now()
    WHERE id = $1 AND assigned_to = $2

  Returns false if 0 rows affected (instance was reassigned — lost ownership).
  The model does not track "lost ownership" separately; instead, if the
  assigned worker crashes, the instance becomes stale and the reaper reclaims
  it.  If the worker remains alive, it heartbeats and the instance stays
  claimed.
*)
Heartbeat(w) ==
    /\ alive[w]
    /\ \E i \in RunningInstances :
        assignedTo[i] = w
        /\ heartbeatAt' = [heartbeatAt EXCEPT ![i] = clock]
        /\ clock' = clock + 1
        /\ UNCHANGED <<status, assignedTo, nextWakeAt, alive>>

(*
  --- COMPLETE: Worker marks a workflow as done successfully.

  Maps to SQL (db.go:614-624):
    UPDATE workflow_instances
    SET status = 'done', result = $3, completed_at = now(),
        assigned_to = NULL, query_state = $4
    WHERE id = $1 AND assigned_to = $2
*)
Complete(w) ==
    /\ alive[w]
    /\ \E i \in RunningInstances :
        assignedTo[i] = w
        /\ status' = [status EXCEPT ![i] = "done"]
        /\ assignedTo' = [assignedTo EXCEPT ![i] = NULL]
        /\ clock' = clock + 1
        /\ UNCHANGED <<heartbeatAt, nextWakeAt, alive>>

(*
  --- FAIL: Worker marks a workflow as failed.

  Maps to SQL (db.go:643-652):
    UPDATE workflow_instances
    SET status = 'failed', error_msg = $3, completed_at = now(),
        assigned_to = NULL, query_state = $4
    WHERE id = $1 AND assigned_to = $2
*)
Fail(w) ==
    /\ alive[w]
    /\ \E i \in RunningInstances :
        assignedTo[i] = w
        /\ status' = [status EXCEPT ![i] = "failed"]
        /\ assignedTo' = [assignedTo EXCEPT ![i] = NULL]
        /\ clock' = clock + 1
        /\ UNCHANGED <<heartbeatAt, nextWakeAt, alive>>

(*
  --- RELEASE: Worker suspends a workflow, putting it back to 'ready'
      with a future wake time.  The instance becomes claimable again
      once the logical clock passes nextWakeAt.

  Maps to SQL (db.go:671-678):
    UPDATE workflow_instances
    SET status = 'ready', assigned_to = NULL, next_wake_at = $3
    WHERE id = $1 AND assigned_to = $2
*)
Release(w) ==
    /\ alive[w]
    /\ \E i \in RunningInstances :
        assignedTo[i] = w
        /\ status' = [status EXCEPT ![i] = "ready"]
        /\ assignedTo' = [assignedTo EXCEPT ![i] = NULL]
        \* Suspend for HeartbeatTimeout logical time units.
        /\ nextWakeAt' = [nextWakeAt EXCEPT ![i] = clock + HeartbeatTimeout]
        /\ clock' = clock + 1
        /\ UNCHANGED <<heartbeatAt, alive>>

(*
  --- REAP: Background reaper reclaims stale instances.  In the
      implementation (db.go:852-864) this runs every 30s on every worker:

    UPDATE workflow_instances
    SET status = 'ready', assigned_to = NULL, heartbeat_at = NULL
    WHERE status = 'running' AND heartbeat_at < now() - interval '30s'

  The first disjunct reclaims any currently stale instance (worker crashed
  or heartbeat too old).  The second disjunct (idle sweep) always allows
  the reaper to fire so that weak fairness can be applied forcing it to
  run periodically even when there is nothing stale.
*)
Reap ==
    \/ (\E i \in StaleInstances :
        /\ status' = [status EXCEPT ![i] = "ready"]
        /\ assignedTo' = [assignedTo EXCEPT ![i] = NULL]
        /\ heartbeatAt' = [heartbeatAt EXCEPT ![i] = 0]
        /\ clock' = clock + 1
        /\ UNCHANGED <<nextWakeAt, alive>>)
    \/ (  \* Idle sweep — no stale instances, but time still passes.
        /\ clock' = clock + 1
        /\ UNCHANGED <<status, assignedTo, heartbeatAt, nextWakeAt, alive>>)

(*
  --- CRASH: A worker dies, ceasing all heartbeats.  Its assigned instances
      will eventually be reaped by the Reaper.
*)
Crash(w) ==
    /\ alive[w]
    /\ alive' = [alive EXCEPT ![w] = FALSE]
    /\ clock' = clock + 1
    /\ UNCHANGED <<status, assignedTo, heartbeatAt, nextWakeAt>>

(*
  --- RESTART: A previously crashed worker comes back online and can
      begin claiming instances again.
*)
Restart(w) ==
    /\ ~alive[w]
    /\ alive' = [alive EXCEPT ![w] = TRUE]
    /\ clock' = clock + 1
    /\ UNCHANGED <<status, assignedTo, heartbeatAt, nextWakeAt>>

(*
  --- TICK: Pure time passage.  No worker or reaper acts; the logical
      clock simply advances.  This models the fact that wall-clock time
      advances even when no database action occurs, allowing heartbeats
      to go stale and released instances to become eligible again.
*)
Tick ==
    /\ clock' = clock + 1
    /\ UNCHANGED <<status, assignedTo, heartbeatAt, nextWakeAt, alive>>

\* =============================================================================
\* NEXT-STATE RELATION
\* =============================================================================

\* The full next-state relation: any action can fire at any step.
Next ==
    \/ (\E w \in Workers :
        Claim(w) \/ Heartbeat(w) \/ Complete(w) \/ Fail(w) \/ Release(w) \/ Crash(w))
    \/ (\E w \in Workers : Restart(w))
    \/ Reap
    \/ Tick

\* =============================================================================
\* INITIAL STATE
\* =============================================================================

Init ==
    /\ status    = [i \in Instances |-> "ready"]
    /\ assignedTo = [i \in Instances |-> NULL]
    /\ heartbeatAt = [i \in Instances |-> 0]
    /\ nextWakeAt  = [i \in Instances |-> 0]
    /\ clock     = 0
    /\ alive     = [w \in Workers |-> TRUE]

\* =============================================================================
\* FAIRNESS (TEMPORAL)
\* =============================================================================

(*
  Fairness is split into three components:

  1. WF on Claim(w) for each worker w.
     If a worker can continuously claim (alive AND ready instances exist),
     it must eventually claim.  Heartbeat / Complete / Fail / Release do
     NOT satisfy this WF, so a worker cannot avoid claiming indefinitely
     by heartbeating forever.

  2. WF on management actions for each worker w.
     If a worker can continuously manage its running instances (heartbeat,
     complete, fail, or release them), it must eventually do so.  This
     prevents a worker from ignoring its claimed instances.

  3. WF on Reap.
     The reaper fires infinitely often (always enabled via the idle sweep).
     When stale instances exist, the first disjunct reclaims them.
*)

Fairness ==
    /\ \A w \in Workers : WF_vars(Claim(w))
    /\ \A w \in Workers : WF_vars(Heartbeat(w) \/ Complete(w) \/ Fail(w) \/ Release(w))
    /\ WF_vars(Reap)

\* =============================================================================
\* COMPLETE SPECIFICATION
\* =============================================================================

Spec == Init /\ [][Next]_vars /\ Fairness

\* State constraint bounding the logical clock, so TLC's search stays finite.
\* Referenced by name (not inline) from CleatClaim.cfg's CONSTRAINT clause --
\* a .cfg CONSTRAINT takes an operator defined in the spec, not a raw
\* expression.
ClockBound == clock < 8

\* =============================================================================
\* SAFETY INVARIANTS
\* =============================================================================

(*
  AtMostOnce:
    No instance is ever claimed by two workers simultaneously.  At any point
    in time, for any instance, at most one worker has it assigned.

    In our model this is structurally guaranteed by the single-valued
    assignedTo function, but we express it as a state invariant for
    documentation and cross-checking with TLC.
*)
AtMostOnce ==
    /\ \A i \in Instances :
        status[i] = "running" => assignedTo[i] \in Workers
    /\ \A i \in Instances :
        status[i] \in {"ready", "done", "failed"} => assignedTo[i] = NULL

(*
  TerminalStable:
    An instance in 'done' or 'failed' state never transitions again.
    This state invariant checks that terminal instances always have
    NULL assignment.  The impossibility of further transitions from
    a terminal status is enforced by the action preconditions (none of
    the actions target terminal instances in their effect — they all
    check for "ready" or "running" status).
*)
TerminalStable ==
    \A i \in Instances :
        status[i] \in {"done", "failed"} => assignedTo[i] = NULL

(*
  ClaimGuard:
    A worker can only complete / fail / release / heartbeat an instance
    it currently holds.  This is enforced by action preconditions
    (assignedTo[i] = w in every management action).  As an invariant we
    verify that every running instance has a valid owner.
*)
ClaimGuard ==
    \A i \in Instances :
        status[i] = "running" => assignedTo[i] \in Workers

(*
  Combined safety invariant for model checking.
  TypeOK is also checked to catch modelling errors.
*)
Safety == AtMostOnce /\ TerminalStable /\ ClaimGuard

\* =============================================================================
\* LIVENESS (TEMPORAL PROPERTIES)
\* =============================================================================

(*
  ClaimProgress:
    If there are ready instances and at least one alive worker, some worker
    eventually claims an instance (transitions it to 'running').

    Under WF on Claim(w), this holds because when an alive worker exists
    and ready instances exist, at least one Claim(w) action is continuously
    enabled, and WF forces it to fire.
*)
ClaimProgress ==
    []( (\E w \in Workers : alive[w]) /\ ReadyInstances /= {}
        => <>(\E i \in Instances : status[i] = "running") )

(*
  ReapProgress:
    If a worker crashes (stops heartbeating), its claimed instances are
    eventually reaped back to 'ready'.  Under WF on Reap, the reaper
    fires infinitely often; when stale instances exist (due to a crashed
    worker), the first disjunct of Reap reclaims them.
*)
ReapProgress ==
    []( \A i \in Instances :
        (status[i] = "running" /\ ~alive[assignedTo[i]])
        => <>(status[i] = "ready") )

(*
  TerminalStableLiveness:
    Temporal version of TerminalStable.  Once an instance reaches 'done'
    or 'failed', it stays there forever (no transition to any other status).
    Since our actions never target terminal instances, this is guaranteed
    by construction, but we state it explicitly for TLC to verify.
*)
TerminalStableLiveness ==
    \A i \in Instances :
        []( status[i] \in {"done", "failed"} => [](status[i] \in {"done", "failed"}) )

(*
  NoStarvation:
    Eventually, every ready instance (past its wake time) is claimed,
    provided some worker is still alive.  This expresses the idea that
    no instance is permanently starved in a system with active workers.

    NOTE: This property requires the strongest fairness.  With WF on
    Claim(w), if multiple workers and instances exist, a specific instance
    could theoretically be skipped repeatedly while others are claimed.
    In practice, with bounded instances and workers under WF, every
    instance eventually transitions because the system cannot avoid a
    specific ready instance forever — the other instances run to
    completion and this one becomes the only remaining ready work.
*)
NoStarvation ==
    []( \A i \in Instances :
        (status[i] = "ready" /\ nextWakeAt[i] <= clock /\ \E w \in Workers : alive[w])
        => <>(status[i] /= "ready") )

\* =============================================================================
\* MODEL CHECKING CONFIGURATION (TLC)
\* =============================================================================
(*

  The actual model config is CleatClaim.cfg, checked into this directory --
  read it rather than this comment, which used to duplicate it and drifted
  (see below). Run with:

    java -cp tla2tools.jar tlc2.TLC -config CleatClaim.cfg CleatClaim.tla

  `make tla` runs this. Bounds and the measured state count are in
  specs/README.md, not here, for the same reason CLAUDE.md gives for not
  carrying a live count in prose: it is checked by CI on every run, and a
  number written here would rot the first time either file changed alone.

  This comment previously suggested Workers = {w1, w2, w3}, NumInstances = 5,
  CONSTRAINT clock < 30, and estimated "a few thousand distinct states,
  completing in seconds." Measured 2026-09-23, before this file had ever
  been run through TLC: that configuration reached 1.9M+ distinct states and
  was still growing past two minutes, because clock < 30 bounds the clock but
  not the reachable heartbeatAt/nextWakeAt combinations beneath it -- the
  state space this config actually explores is far larger than a bound on
  one variable suggests. CleatClaim.cfg uses much smaller bounds so the
  model checks in seconds, per CI's own requirement.

  For initial debugging, run WITHOUT the PROPERTIES line first to
  check that TypeOK and Safety hold, then add properties one at a
  time to isolate any liveness violations.
*)

====
