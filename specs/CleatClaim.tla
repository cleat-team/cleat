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

(*
  ClockCeiling / NextClock / FutureClock: clock is SELF-CLAMPING rather than
  bounded by a .cfg CONSTRAINT. cleat#2034: a CONSTRAINT that hard-walls an
  unboundedly-incrementing clock (the previous ClockBound == clock < 8, which
  every action's clock' = clock + 1 eventually hits) creates a state with NO
  constraint-satisfying successor at all once clock reaches the wall. TLC's
  liveness engine treats that as a state that must stutter forever to satisfy
  [][Next]_vars, and a "just stutter at the wall" path is fairness-vacuous
  whenever a property's own witness action is not separately forced -- which
  silently defeats a liveness check rather than erroring, exactly as TLC's
  own "Declaring state or action constraints during liveness checking is
  dangerous" print warns. Known-positive confirmed it on the PRE-FIX spec
  (clock bounded by a .cfg CONSTRAINT, Fairness still WF_vars(Claim(w))):
  removing that clause left ClaimProgress reporting "No error has been
  found" at an IDENTICAL state count to the unmutated pre-fix run (87180
  distinct states, 130501 generated) -- this is the finding that motivated
  filing cleat#2034 in the first place, cited by specs/CleatRunLifecycle.tla's
  own Fairness comment.

  Re-run AFTER this fix (clock self-clamping, Fairness now SF_vars(Claim(w))):
  removing SF_vars(Claim(w)) still makes ClaimProgress fail, and now at an
  IDENTICAL state count to the FIXED baseline too (8738 distinct states,
  59229 generated -- both numbers unchanged from the clean run, since no
  CONSTRAINT means nothing about the reachable graph depends on which
  liveness formula is being checked). That equality is the actual
  evidence the fix is honest: same graph, verdict flips only with the
  fairness clause -- see specs/README.md's "Known-positive" section for
  the full measurement, including ReapProgress's and TerminalStableLiveness's
  own known-positive re-runs.

  This is the identical mechanism cleat#2000/#2032 found and fixed in
  specs/CleatQueueAdmission.tla, and the identical fix specs/CleatRunLifecycle.tla
  (cleat#1997/#2035) applied first -- ClockCeiling/NextClock/FutureClock is
  copied from that file's own comment on this exact pattern rather than
  reinvented, since it was already measured to work there. Unlike
  CleatQueueAdmission.tla's rate token (a single per-run countdown), this
  file's clock feeds TWO deadline-shaped quantities read by comparison
  (heartbeatAt, compared as `clock - heartbeatAt[i] >= HeartbeatTimeout`; and
  nextWakeAt, compared as `nextWakeAt[i] <= clock`) rather than one
  independent countdown per run, so eliminating clock entirely (as
  CleatQueueAdmission.tla did) does not fit as cleanly -- self-clamping the
  clock itself, as CleatRunLifecycle.tla does, is the more direct fit here.

  NextClock clamps at ClockCeiling instead of a CONSTRAINT excluding
  clock'=ClockCeiling+1 -- every action stays enabled forever once clock is
  pinned (it just clamps again), so there is no artificial dead end for a
  fair path to hide in. heartbeatAt is always set to clock directly (a
  snapshot, not an offset), so pinning clock still leaves it in a bounded
  range and `clock - heartbeatAt[i] >= HeartbeatTimeout` stays meaningful --
  a pinned clock reads as "very late", the correct, conservative reading.

  nextWakeAt is a genuine FUTURE deadline (clock + HeartbeatTimeout, set by
  Release), so it needs FutureClock, not a bare clock' = NextClock(clock)
  reuse: CleatRunLifecycle.tla's own comment on this found a real
  counter-example from clamping the deadline TO the ceiling (a deadline set
  right as clock pins there makes ReadyInstances' `nextWakeAt[i] <= clock`
  permanently false, since a strict-vs-non-strict boundary can still fail to
  align) -- clamping one tick short of the ceiling, not to it, is what makes
  every deadline this model can compute reachable once clock is pinned. No
  .cfg CONSTRAINT is declared for this spec as a result -- there is nothing
  for one to exclude.
*)
ClockCeiling == 5
NextClock(c) == IF c < ClockCeiling THEN c + 1 ELSE ClockCeiling
FutureClock(c, offset) == IF c + offset >= ClockCeiling THEN ClockCeiling - 1 ELSE c + offset

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
        /\ clock' = NextClock(clock)
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
        /\ clock' = NextClock(clock)
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
        /\ clock' = NextClock(clock)
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
        /\ clock' = NextClock(clock)
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
        /\ nextWakeAt' = [nextWakeAt EXCEPT ![i] = FutureClock(clock, HeartbeatTimeout)]
        /\ clock' = NextClock(clock)
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
        /\ clock' = NextClock(clock)
        /\ UNCHANGED <<nextWakeAt, alive>>)
    \/ (  \* Idle sweep — no stale instances, but time still passes.
        /\ clock' = NextClock(clock)
        /\ UNCHANGED <<status, assignedTo, heartbeatAt, nextWakeAt, alive>>)

(*
  --- CRASH: A worker dies, ceasing all heartbeats.  Its assigned instances
      will eventually be reaped by the Reaper.
*)
Crash(w) ==
    /\ alive[w]
    /\ alive' = [alive EXCEPT ![w] = FALSE]
    /\ clock' = NextClock(clock)
    /\ UNCHANGED <<status, assignedTo, heartbeatAt, nextWakeAt>>

(*
  --- RESTART: A previously crashed worker comes back online and can
      begin claiming instances again.
*)
Restart(w) ==
    /\ ~alive[w]
    /\ alive' = [alive EXCEPT ![w] = TRUE]
    /\ clock' = NextClock(clock)
    /\ UNCHANGED <<status, assignedTo, heartbeatAt, nextWakeAt>>

(*
  --- TICK: Pure time passage.  No worker or reaper acts; the logical
      clock simply advances.  This models the fact that wall-clock time
      advances even when no database action occurs, allowing heartbeats
      to go stale and released instances to become eligible again.
*)
Tick ==
    /\ clock' = NextClock(clock)
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

  1. SF on Claim(w) for each worker w -- STRONG fairness, not weak, and this
     is a correction (cleat#2034), not a stylistic choice. Copied from
     specs/CleatRunLifecycle.tla's identical fix, which found this measured,
     not assumed: after fixing the clock-CONSTRAINT vacuity above, a
     known-positive re-run (removing Claim's fairness entirely) turned up a
     DIFFERENT, real counter-example needing no mutation at all --
     Workers={w1}, and an unfair Crash immediately followed by Restart every
     single tick lets w1 toggle alive[w1] forever, so alive[w1] is never
     CONTINUOUSLY true and WF_vars(Claim(w1)) is never forced, even though
     an instance sits in ReadyInstances the entire time. Claim(w1) IS
     enabled infinitely often (every state where the alternation passes
     through "alive[w1]=TRUE, something ready" -- which recurs forever),
     just not continuously -- exactly the condition SF is defined to force
     and WF is not. Crash/Restart themselves stay deliberately UNFAIR (see
     FleetEventuallyStable below) -- nothing in the real system forces an
     operator to ever restart a worker; SF only strengthens what Claim(w)
     itself must do once it keeps becoming enabled.

  2. SF on management actions for each worker w, for the identical reason:
     if a worker can be made enabled-infinitely-often-but-not-continuously
     for its own running instances (the same Crash/Restart alternation),
     WF would let it dodge managing them forever the same way.

  3. WF on Reap.
     The reaper fires infinitely often (always enabled via the idle sweep,
     which depends on no worker's aliveness), so WF suffices here -- Reap's
     own enabledness is never toggled by Crash/Restart the way Claim(w)'s
     effective availability is.
*)

Fairness ==
    /\ \A w \in Workers : SF_vars(Claim(w))
    /\ \A w \in Workers : SF_vars(Heartbeat(w) \/ Complete(w) \/ Fail(w) \/ Release(w))
    /\ WF_vars(Reap)

(*
  FleetEventuallyStable: eventually, permanently, at least one worker is
  alive. NOT action fairness on Crash/Restart, which stay deliberately
  unfair (nothing in the real system forces an operator to ever restart a
  crashed worker) -- a raw temporal ASSUMPTION on the whole system, copied
  from specs/CleatRunLifecycle.tla's identical fix. Added after the SF fix
  above still left a genuine, no-mutation-needed counter-example:
  Workers={w1}, Crash w1 once, and simply never Restart it. `\E w: alive[w]`
  is true for finitely many early states (so ClaimProgress's/NoStarvation's
  obligation IS created) then FALSE forever after (so nothing can ever
  discharge it) -- SF cannot help, because SF_vars(Claim(w1)) only fires if
  Claim is enabled INFINITELY OFTEN, and once w1 is permanently dead it is
  enabled exactly zero more times. This is not a defect in the properties'
  wording: "the entire fleet crashes and no operator ever restarts anything,
  forever" is not a scenario ClaimProgress or NoStarvation should be
  expected to survive -- of course ready work sits forever if literally
  every worker is permanently gone. The standard TLA+ answer is to state the
  assumption the properties actually depend on as its own conjunct, rather
  than folding it into per-action fairness.
*)
FleetEventuallyStable == <>[](\E w \in Workers : alive[w])

\* =============================================================================
\* COMPLETE SPECIFICATION
\* =============================================================================

Spec == Init /\ [][Next]_vars /\ Fairness /\ FleetEventuallyStable

\* No .cfg CONSTRAINT here, deliberately -- clock is self-clamping
\* (ClockCeiling/NextClock/FutureClock, in CONSTANT HELPERS above); see that
\* comment for why a CONSTRAINT was tried first (that was this file's
\* original design) and found to silently defeat liveness checking.

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
    If a worker crashes (stops heartbeating), its claimed instance is
    eventually resolved -- it does not sit "running", attributed to a dead
    owner, forever.

    cleat#2034: the consequent used to require specifically `status[i] =
    "ready"` (implying Reap must be the one to act), and that is too
    strong a claim -- a real, no-mutation-needed counter-example on the
    fixed (non-vacuous) spec: Claim, then Fail on instance 2; Claim
    instance 1; Crash(w1) while it holds instance 1 (antecedent becomes
    true); Restart(w1); w1 itself (now alive again) directly Fails
    instance 1 without Reap ever firing. That is completely legitimate
    real-system behaviour -- a worker crashing briefly and recovering to
    finish its own claimed work is not a bug the reaper needs to prevent --
    so "eventually ready" is too strong a consequent, exactly the same
    shape of error specs/CleatRunLifecycle.tla's own L2_ClaimProgress
    comment describes ("Terminate and Cancel... resolve... without either
    ever passing through 'running'... too strong a consequent"). The
    corrected consequent: the instance eventually stops being
    running-with-this-antecedent -- either reaped to 'ready', or resolved
    directly (done/failed) by whoever manages it next, including its own
    recovered owner. Under WF on Reap, the reaper still fires infinitely
    often and reclaims genuinely abandoned instances; under SF on the
    management actions, a recovered owner is also forced to eventually act
    on what it still holds.

    Still too strong, and fixed a second time the same day: a real,
    no-mutation-needed counter-example on the fixed spec (TLC, 2026-09-23) --
    instance 2 is running, assigned to w2; w2 Crashes (antecedent becomes
    true for instance 2) and Restarts on the very next step, before either
    WF_vars(Reap)'s stale-reclaim disjunct or SF on w2's own management
    actions gets a continuously/infinitely-often-enabled window to react
    (the antecedent is revoked -- alive[w2] goes back to TRUE -- one step
    after it appears). w2 then spends forever exclusively cycling
    Release/Claim on instance 1, which alone satisfies its disjunctive
    SF_vars(Heartbeat(w2)\/Complete(w2)\/Fail(w2)\/Release(w2)) fairness
    clause, while instance 2 sits "running" untouched. This is EXACTLY the
    shape specs/CleatRunLifecycle.tla's own L2_ClaimProgress/L3_CascadeProgress
    comments describe: a momentary, adversarial crash/restart (or redrive)
    can establish then revoke a leads-to antecedent faster than any
    fairness -- weak or strong, both are asymptotic guarantees over a
    PERSISTING precondition -- can act on it. Fixed there by adding an
    honest "or the antecedent itself later became false" escape, applied
    here for the identical reason: a worker coming back alive before Reap
    or its own next management action fires is not starvation, it is the
    antecedent ceasing to hold. (Unlike ClaimProgress, which stayed clean
    under known-positive testing without needing this -- see its own
    comment -- ClaimProgress's antecedent, "some ready instance and some
    alive worker exist", is not itself revocable by the single-step action
    that made it true, so it never needed the escape RunLifecycle's
    operator-verb actions and this file's Crash/Restart both create.)
*)
ReapProgress ==
    []( \A i \in Instances :
        (status[i] = "running" /\ ~alive[assignedTo[i]])
        => <>(status[i] /= "running")
           \/ <>(~(status[i] = "running" /\ ~alive[assignedTo[i]])) )

(*
  TerminalStableLiveness:
    Once an instance reaches 'done' or 'failed', it stays there forever --
    unlike CleatRunLifecycle.tla, this file has no redrive verb (no
    RetryWorkflow/AdminReReplay), so "stays there" means status never
    changes AGAIN at all, not merely "stays in the terminal set".

    Written as an ACTION invariant ([][A]_vars), NOT the original
    `[](P=>[]Q)` temporal form above this comment until cleat#2034 -- that
    form is EXACTLY the shape specs/CleatRunLifecycle.tla's own header
    identifies as the first place the clock-CONSTRAINT vacuity bit it
    (SettledIsFinal's original form, before that file's own fix). Since our
    actions never target terminal instances, this is guaranteed by
    construction, but the action-invariant form checks that purely during
    ordinary reachability analysis, the same way an INVARIANT does, and is
    not exposed to the liveness engine's stuttering-at-the-wall interaction
    a `[]`-only or `[](P=><>Q)`/`[](P=>[]Q)` temporal formula is.
*)
TerminalStableLiveness ==
    [][\A i \in Instances :
         status[i] \in {"done", "failed"} => status'[i] = status[i]]_vars

(*
  NoStarvation:
    Eventually, every ready instance (past its wake time) is claimed,
    provided some worker is still alive.  This expresses the idea that
    no instance is permanently starved in a system with active workers.

    NOT GATED IN CleatClaim.cfg -- DEFINED, not CHECKED. The paragraph this
    replaces argued "in practice... every instance eventually transitions
    because the system cannot avoid a specific ready instance forever",
    and TLC (2026-09-23) proved that argument wrong with a real,
    no-mutation-needed counter-example: w1 crashes permanently (never
    restarted -- legal, nothing requires an operator to; FleetEventuallyStable
    only requires SOME worker stay alive, not every worker), leaving w2 as
    the sole claimant with MaxClaimBatch=1. Claim(w2) is a disjunction over
    WHICH ready instance to take; SF_vars(Claim(w2)) forces the DISJUNCTION
    to fire infinitely often, not any particular disjunct. w2 can
    nondeterministically always choose instance 2 (Claim it, Release it,
    Claim it again, forever), which alone satisfies SF_vars(Claim(w2)) --
    and instance 1 sits "ready", past its wake time, with an alive worker
    the whole time, forever unclaimed. This is the same root cause
    ReapProgress's second fix and specs/CleatRunLifecycle.tla's
    L2_ClaimProgress/L3_CascadeProgress both name: fairness on a
    DISJUNCTIVE action is a guarantee about the disjunction, not about any
    one disjunct or any one entity the disjunction ranges over.

    Unlike ReapProgress, no "or the antecedent later became false" escape
    repairs this: nothing here ever revokes instance 1's antecedent (w2
    never dies, instance 1 never stops being ready past its wake time) --
    the antecedent stays true for the entire infinite trace, so the escape
    clause would be exactly as false as the original consequent. Genuinely
    fixing this needs PER-INSTANCE fairness (something like
    SF over the specific Claim transition that picks instance i, for each
    i), which is a real model change, not a rewording -- out of scope for
    cleat#2034, whose mandate is removing the CONSTRAINT-clamping vacuity,
    not building new fairness machinery. Left defined and ungated rather
    than deleted, per specs/CleatRunLifecycle.tla's own precedent for
    L1/L2/L3 (same file, "NOT YET GATED" comment): shipping this gated
    would have been a false claim, and the honest state is "found a real
    gap, tracked it, did not chase convergence within this issue's scope."
    Follow-up: cleat#2034 was filed for the vacuity; this finding is new
    and should get its own issue before anyone attempts the per-instance-
    fairness fix.
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
