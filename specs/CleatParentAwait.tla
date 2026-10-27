-------------------------- MODULE CleatParentAwait --------------------------
(*
  Parents awaiting children, and parent-close cascades to descendants.

  cleat#1998. CleatRunLifecycle.tla (cleat#1997) models every writer of
  workflow_instances.status, including a FIXED, minimal two-level cascade
  (CascadeTerminate) deliberately scoped away from chasing its effect on an
  awaiting ancestor or a topology deeper than one level -- see that file's
  own "THE PARENT-CLOSE CASCADE" scope note, which names this issue as where
  both of those are picked up. This file is that pickup: it does not
  EXTEND CleatRunLifecycle.tla (no spec in this directory extends another --
  grep '^EXTENDS' specs/*.tla, every hit names only a standard module), it
  rebuilds the slice of the status machine this issue's own actor list
  needs, at the abstraction level that list implies.

  Every actor and fencing rule below was read from
  github.com/cleat-team/cleat @ 81fdd8984 (2026-10-07), not from the issue
  body or from memory -- and, unlike CleatRunLifecycle.tla's own header,
  every one of the four bug issues this model was written to check
  (#1974, #1976, #1978, #1108) was ALREADY CLOSED and its fix ALREADY LIVE
  on that commit:
    - #1974 (GetChildResult never settles for terminated/cancelled):
      fixed via childOutcomeForSettledStatus (engine/status_vocabulary.go),
      called from engine/store_children.go's GetChildResult.
    - #1978 (parent-close TERMINATE records 'failed' instead of
      'terminated'): fixed, engine/store_lifecycle.go:1080-1103 now writes
      status='terminated', error_op='parent_close', error_code=NULL on both
      the direct and defer-phase TERMINATE arms.
    - #1976 (a failed run skips the parent wake and finalize observers):
      fixed, cmd/cleat-worker/setup.go's notifyTerminal (:6640) is now the
      SINGLE shared post-settle helper -- parent wake, stranded-update
      failure, and observer notification -- called uniformly regardless of
      which terminal status was reached.
    - #1108 (a cascade reaches one level and stops): fixed,
      enforceParentClosePolicyAt (engine/store_lifecycle.go:993) now carries
      an explicit depth parameter and calls cascadeIntoClosedChildren, which
      recurses enforceParentClosePolicyAt onto every child the TERMINATE arm
      just closed.

  So this model's properties are expected to PASS against current develop,
  not fail -- which is the opposite of every other spec's discovery story in
  this directory and is why the acceptance bar in #1998's own issue body
  ("TLC reproduces #1974 as an L1 violation... and the trace is attached to
  #1974") needs a second configuration to produce anything to attach: see
  CleatParentAwaitPreFix1974.cfg below, a deliberate KNOWN-POSITIVE
  configuration (same .tla, one CONSTANT flipped) that reverts
  ObservedOutcome's mapping to the pre-#1974 shape and reproduces the
  violation on demand, the same way CleatQueueAdmission.md's three recorded
  mutations are known-positive controls rather than defect reports.

  Implemented by (no line numbers -- see CleatClaim.tla's header for why):
    - engine/children.go               (AwaitChild, AwaitAnyChild,
                                         AwaitAllChildren -- the suspend/
                                         re-poll shape; see SCOPE DECISIONS
                                         below for what is and is not carried
                                         into the model from this file)
    - engine/store_children.go         (PostgresStore.GetChildResult, the
                                         status-mapping function this model's
                                         ObservedOutcome restates)
    - engine/status_vocabulary.go      (childOutcomeForSettledStatus, the
                                         function ObservedOutcome below is an
                                         independent second derivation of --
                                         see its own comment for why a second
                                         derivation is the point)
    - engine/store_lifecycle.go        (PostgresStore.enforceParentClosePolicy
                                         /-At, cascadeIntoClosedChildren --
                                         the cascade this model's
                                         CascadeTerminate restates over a
                                         three-level topology)
    - cmd/cleat-worker/setup.go        (Worker.notifyTerminal -- the parent
                                         wake; see SCOPE DECISIONS on why its
                                         lossy, best-effort channel send is
                                         NOT modeled as a variable)

  Only the PostgreSQL store is cited, matching CleatRunLifecycle.tla's own
  choice and for the same reason: engine/mysql_store.go,
  engine/mssql_signals_promises.go carry the identical mapping, and the
  dialect-parity tests already in the tree (cited on CleatRunLifecycle.tla)
  are what keep them honest, not a second copy of this model per dialect.

  SCOPE DECISIONS, stated rather than silently applied (specs/README.md's
  own rule, and CLAUDE.md's "a decision not to do something should live
  where the doing would have gone"):

    - CHILD STATUS IS NOT DRIVEN THROUGH CleatRunLifecycle's CLAIM/HEARTBEAT/
      REAP MACHINERY. That machinery is #1997's own already-checked subject
      (SettledIsFinal, S4/AtMostOneClaimHolder). What #1998 adds is new on
      top of "a child eventually reaches a settled status, and settled is
      final" -- so this model takes that as GIVEN (SettleChild below writes
      a terminal status in one atomic step, standing in for whichever real
      action got it there: Claim+Complete, Claim+Fail, Claim+DeadLetter, an
      operator Terminate/Cancel, or FinalizeDeferPhase/ExpireDeferPhases
      after a two-phase MarkOrApply) rather than re-deriving it. Re-deriving
      it a second time is exactly the "one prefix assumption" shape
      CleatRunLifecycle.tla's own MarkOrApply comment warns against: writing
      the same fenced-settlement logic twice is how the two copies drift.

    - THE PARENT WAKE IS NOT A VARIABLE. notifyTerminal's send is
      `select { case w.parentWakeCh <- struct{}{}: default: }` -- non-
      blocking, lossy by construction (cmd/cleat-worker/setup.go:6640-6644).
      A liveness property built on a channel send that can silently drop is
      not sound to model as a guaranteed signal, and it does not need to be:
      the AWAIT TIMER is the documented backstop regardless of whether any
      wake arrives (#1976's own issue text: "It learns of the failed child
      on its own await timer... for the life of the deployment" describes
      the PRE-fix behaviour precisely because the timer always eventually
      fires; the wake only shortens the wait). So Repoll below is governed
      by fairness alone, the same way CleatRunLifecycle.tla's
      ExpireDeferPhases is -- modeling the wake as a variable would add a
      dimension to the state space that cannot make any property weaker to
      drop, since the timer alone already has to carry L1/L2.

    - AWAIT MODE IS A FIXED SET OF THREE SHAPES, not three separate specs.
      AwaitChild awaits exactly one named run; AwaitAnyChild and
      AwaitAllChildren both await a SET and differ only in which quantifier
      resolves them. awaitMode variable below carries all three without
      three copies of Repoll, matching MarkOrApply's own one-operator-not-
      five-actions argument in CleatRunLifecycle.tla.

    - THE TOPOLOGY IS FIXED, not a CONSTANT TLC is asked to vary -- the same
      call CleatRunLifecycle.tla makes for its own ParentOf/ClosePolicy, and
      for the same reason: this model exists to check a specific shape (an
      awaiting ancestor two levels above a TERMINATE grandchild), not every
      possible tree. Instance 1 is both the awaiting parent AND the cascade
      root; instances 2 and 3 are its direct children (also its await
      targets, so AwaitAny/AwaitAll over {2,3} has two distinct members to
      resolve against); instance 4 is a child of instance 2 alone -- the
      grandchild L3 exists to reach. All three parent-child edges carry
      TERMINATE. See ASSUME NumInstances = 4 below; this is not meant to be
      raised, only to document that four is exact, not a lower bound picked
      for state-space reasons.

    - error_code, error_op, generation, and the defer-phase two-phase
      mechanics are NOT modeled, same call as CleatRunLifecycle.tla makes
      for error_code and generation, for the same reason: CascadeTerminate
      here writes a settled status directly, standing in for either arm of
      enforceParentClosePolicyAt (the direct arm or the defer-phase mark),
      because #1998's own properties (L1/L2/L3/S1) are about WHICH status a
      descendant eventually reaches and whether an ancestor eventually
      observes it, not about the two-phase transition's OWN liveness, which
      is L1_EventualSettlement, already defined (deliberately ungated) on
      CleatRunLifecycle.tla.

    - ADMISSION, RETRY, RE-REPLAY, DB-UNAVAILABILITY and the dialect
      surfaces are out of scope for the identical reasons CleatRunLifecycle.
      tla's header states them out of scope -- this model does not reopen
      any of those scope calls, it inherits them.

  State machine (per instance; the await sub-state is ORTHOGONAL to status --
  an instance can be settled while a DIFFERENT instance awaits it, and an
  instance can be awaiting while unsettled itself):

    Awaiting parent (instance 1 only):
      awaitMode[1]: "none" --StartAwait--> "one"/"any"/"all"
                    "one"/"any"/"all" --Repoll, condition met--> "none"
                    (resumeOutcome[1] records the observed kind at that
                     transition; see S1 below)

    Every instance's status (reused, not re-derived, from CleatRunLifecycle.
    tla's own vocabulary -- see SCOPE DECISIONS above for why SettleChild
    collapses several real actions into one):
      "ready" --SettleChild(i, outcome)--> outcome, where
        outcome \in {"done", "failed", "dead_lettered", "terminated",
                     "cancelled"}
      settled, TERMINATE-policy child --CascadeTerminate(ParentOf[i])--
        -already covered by SettleChild once its parent settles; see
        CascadeTerminate's own comment for why this is Reap/ExpireDeferPhases-
        shaped (fair, unfenced-on-identity, sweep-style) rather than a second
        SettleChild call.

  HOW TO MODEL CHECK:

    java -cp tla2tools.jar tlc2.TLC -config CleatParentAwait.cfg CleatParentAwait.tla

  `make tla` runs this, and the known-positive configuration below, for
  every specs/*.cfg automatically. Bounds and the measured state count are
  in specs/CleatParentAwait.md, not here -- see CleatClaim.tla's own comment
  on why a number belongs in the file CI checks, not in prose that drifts
  the moment either file changes alone.
*)

EXTENDS Integers, FiniteSets

CONSTANTS
    NumInstances,          \* Fixed at 4 -- see SCOPE DECISIONS above.
    ClockCeiling,
    AwaitTimeout,          \* Logical-time bound on one repoll cycle.
    PreFix1974Mapping,     \* BOOLEAN. FALSE = current (fixed) ObservedOutcome.
                           \* TRUE = the pre-#1974 mapping, a deliberate
                           \* known-positive control -- see
                           \* CleatParentAwaitPreFix1974.cfg.
    NULL

ASSUME NumInstances = 4
ASSUME ClockCeiling > AwaitTimeout
ASSUME AwaitTimeout > 0
ASSUME PreFix1974Mapping \in BOOLEAN

\* =============================================================================
\* CONSTANT HELPERS
\* =============================================================================

Instances == 1..NumInstances

SettledStatuses == {"done", "failed", "dead_lettered", "terminated", "cancelled"}
Outcomes == SettledStatuses
AllStatuses == {"ready"} \cup SettledStatuses

IsSettled(s) == s \in SettledStatuses

\* ParentOf/ClosePolicy: the fixed three-level topology. See SCOPE DECISIONS.
\*   1 (root, also the awaiting parent)
\*   |-- 2 (direct child, TERMINATE, also an await target)
\*   |    |-- 4 (grandchild of 1, direct child of 2, TERMINATE)
\*   |-- 3 (direct child, TERMINATE, also an await target)
ParentOf == [i \in Instances |->
    CASE i = 1 -> NULL [] i = 2 -> 1 [] i = 3 -> 1 [] i = 4 -> 2]
ClosePolicy == [i \in Instances |-> IF i = 1 THEN "NONE" ELSE "TERMINATE"]

\* The await target sets this fixed topology gives Repoll something
\* non-trivial to resolve: {2} for "one" (AwaitChild on its first direct
\* child), {2,3} for "any"/"all" (AwaitAnyChild/AwaitAllChildren over both
\* direct children -- the grandchild, 4, is never awaited directly, which is
\* deliberate: nothing in engine/children.go awaits a non-adjacent run by
\* skipping its parent, so a model that let 1 await 4 directly would be
\* testing a call shape the SDK does not expose).
AwaitTargetFor(mode) ==
    IF mode = "one" THEN {2} ELSE {2, 3}

\* =============================================================================
\* VARIABLES
\* =============================================================================

VARIABLES
    status,          \* [Instances -> AllStatuses]
    awaitMode,       \* [Instances -> {"none","one","any","all"}], only 1 ever non-"none"
    resumed,         \* [Instances -> BOOLEAN], latched TRUE once Repoll resolves
    resumeOutcome,   \* [Instances -> Outcomes \cup {NoOutcome}], S1's witness
    nextPollAt,      \* [Instances -> Nat], when a non-"none" awaitMode may next repoll
    clock

vars == <<status, awaitMode, resumed, resumeOutcome, nextPollAt, clock>>

NoOutcome == "none"

\* =============================================================================
\* CLOCK (CleatRunLifecycle.tla's self-clamping shape, same reason: a
\* CONSTRAINT that hard-walls an unboundedly-incrementing clock silently
\* defeats liveness checking -- see that file's ClockCeiling/NextClock
\* comment for the two known-positive traces that caught it there. Re-used
\* here rather than re-discovered, since the mechanism is identical and this
\* model's clock plays the identical role: a liveness-bearing deadline
\* comparison, not a wall-clock measurement.)
\* =============================================================================

NextClock(c) == IF c < ClockCeiling THEN c + 1 ELSE ClockCeiling

FutureClock(c, offset) ==
    IF c + offset >= ClockCeiling THEN ClockCeiling - 1 ELSE c + offset

\* =============================================================================
\* OBSERVED OUTCOME -- an independent second derivation of
\* childOutcomeForSettledStatus (engine/status_vocabulary.go), not a copy.
\* See that function's own comment on why a shared helper was reverted for
\* settledStatusList: the same argument applies here in reverse -- a TLA+
\* model that imported the Go mapping verbatim would be checking that the
\* mapping agrees with itself. This is deliberately a plain restatement from
\* the ISSUE's own table (S1's row in #1998), so that a divergence between
\* the issue's stated contract and the code's actual mapping has something
\* to disagree against, the same "two derivations, not one, checked" shape
\* CLAUDE.md asks for everywhere else in this project.
\* =============================================================================

(*
  PreFix1974Mapping = FALSE (the shipped configuration): every settled
  status maps to a kind the awaiting parent can observe. This is #1974's
  fix, independently restated.

  PreFix1974Mapping = TRUE (the known-positive control,
  CleatParentAwaitPreFix1974.cfg): 'terminated' and 'cancelled' map to
  NoOutcome -- GetChildResult's pre-#1974 answer for those two statuses was
  ChildOutcome{}, indistinguishable from "still running" (see #1974's own
  issue text, quoted in this file's header). That is reproduced here as
  "ObservedOutcome returns NoOutcome", which Repoll's guard (below) never
  treats as a resolving answer -- exactly the hang #1974 reports.
*)
ObservedOutcome(s) ==
    CASE s = "done"                                        -> "done"
      [] s = "failed"                                       -> "failed"
      [] s = "dead_lettered"                                -> "dead_lettered"
      [] s = "terminated" /\ ~PreFix1974Mapping              -> "terminated"
      [] s = "cancelled"  /\ ~PreFix1974Mapping              -> "cancelled"
      [] s = "terminated" /\ PreFix1974Mapping               -> NoOutcome
      [] s = "cancelled"  /\ PreFix1974Mapping               -> NoOutcome
      [] OTHER                                               -> NoOutcome

\* Whether an await over `targets`, in `mode`, is resolved given the current
\* status vector -- ANY needs one settled member seen (ObservedOutcome /=
\* NoOutcome), ALL needs every member seen, ONE is ALL over a singleton.
AwaitResolved(mode, targets) ==
    IF mode = "any"
    THEN \E c \in targets : ObservedOutcome(status[c]) /= NoOutcome
    ELSE \A c \in targets : ObservedOutcome(status[c]) /= NoOutcome

\* The SINGLE member whose outcome an "any" await reports -- the first one
\* (by instance number) that has resolved, matching AwaitAnyChild's own
\* behaviour of returning on the first child found complete
\* (engine/children.go) rather than an arbitrary or combined answer.
FirstResolved(targets) ==
    CHOOSE c \in targets : ObservedOutcome(status[c]) /= NoOutcome
                           /\ \A d \in targets : d < c => ObservedOutcome(status[d]) = NoOutcome

\* =============================================================================
\* TYPE INVARIANT
\* =============================================================================

TypeOK ==
    /\ status \in [Instances -> AllStatuses]
    /\ awaitMode \in [Instances -> {"none", "one", "any", "all"}]
    /\ resumed \in [Instances -> BOOLEAN]
    /\ resumeOutcome \in [Instances -> Outcomes \cup {NoOutcome}]
    /\ nextPollAt \in [Instances -> Nat]
    /\ clock \in Nat

\* =============================================================================
\* ACTIONS -- a child settling (SCOPE DECISIONS: one atomic write stands in
\* for the real claim/complete/fail/terminate/cancel/defer-phase chain,
\* which is #1997's already-checked subject).
\* =============================================================================

(*
  SettleChild(i, outcome): any unsettled instance may reach any settled
  status once -- SettledIsFinal (CleatRunLifecycle.tla, S1/cleat#1975) is
  taken as given, so this action's own guard (~IsSettled(status[i])) is the
  one-line restatement of it this model needs, not a re-proof.
*)
SettleChild(i, outcome) ==
    /\ ~IsSettled(status[i])
    /\ status' = [status EXCEPT ![i] = outcome]
    /\ clock' = NextClock(clock)
    /\ UNCHANGED <<awaitMode, resumed, resumeOutcome, nextPollAt>>

\* =============================================================================
\* ACTIONS -- the parent-close cascade (D1/cleat#1978, D-depth/cleat#1108)
\* =============================================================================

(*
  CascadeTerminate(p): a settled parent p closes every TERMINATE-policy
  child that has not already settled -- CleatRunLifecycle.tla's own action,
  restated at a depth that lets it apply AGAIN once a cascaded child is
  itself settled, which is how instance 4 (grandchild of 1, child of 2) gets
  reached: CascadeTerminate(1) settles 2 (among others); 2 is now settled,
  so CascadeTerminate(2) becomes enabled in a LATER step and settles 4. Two
  separate steps, two separate fairness-governed firings of the SAME
  operator, not a single multi-level jump -- this is deliberately NOT
  collapsed into one atomic action, because cascadeIntoClosedChildren
  (engine/store_lifecycle.go:1139) is not atomic either: it is its own,
  separate best-effort call per level, exactly like the action structure
  here. #1108's own fix is this shape; a model that jumped straight to
  "every descendant settles in one step" would not be checking that the
  recursion actually happens, only that this model's author assumed it did.
*)
CascadeEligible(p) ==
    {c \in Instances : ParentOf[c] = p /\ ClosePolicy[c] = "TERMINATE" /\ ~IsSettled(status[c])}

CascadeTerminate(p) ==
    /\ IsSettled(status[p])
    /\ LET E == CascadeEligible(p) IN
        /\ E /= {}
        /\ status' = [i \in Instances |-> IF i \in E THEN "terminated" ELSE status[i]]
        /\ clock' = NextClock(clock)
        /\ UNCHANGED <<awaitMode, resumed, resumeOutcome, nextPollAt>>

\* =============================================================================
\* ACTIONS -- the await machinery (engine/children.go)
\* =============================================================================

(*
  StartAwait(mode): instance 1 begins awaiting, choosing which of the three
  SDK calls it made -- AwaitChild ("one"), AwaitAnyChild ("any") or
  AwaitAllChildren ("all"). Can only start once (awaitMode[1] = "none" and
  not already resumed) -- this model does not explore a SECOND await after
  the first resumes, because nothing in L1/L2/L3/S1 depends on sequencing
  two awaits, and adding it would double the state space to check a
  property none of the four names.
*)
StartAwait(mode) ==
    /\ awaitMode[1] = "none"
    /\ ~resumed[1]
    /\ awaitMode' = [awaitMode EXCEPT ![1] = mode]
    /\ nextPollAt' = [nextPollAt EXCEPT ![1] = clock]
    /\ clock' = NextClock(clock)
    /\ UNCHANGED <<status, resumed, resumeOutcome>>

(*
  Repoll: the await-timer backstop (SCOPE DECISIONS above explains why the
  lossy parentWakeCh send is not separately modeled). Fires at or after
  nextPollAt[1]; if the await's condition is not yet met, it reschedules
  itself AwaitTimeout ticks out (re-polling forever, matching #1976's own
  description of the pre-fix behaviour: "for the life of the deployment");
  if it is met, it resolves the await and records the observed outcome.

  "one" and "all" record resumeOutcome as the mapped status of their single
  resolving member (for "one", the one target; for "all", any settled
  member serves equally since ALL are known settled at that point -- S1
  below checks the recorded value against whichever member actually
  resolved it). "any" records FirstResolved's answer specifically, matching
  AwaitAnyChild returning on the first child found complete.
*)
Repoll ==
    /\ awaitMode[1] /= "none"
    /\ clock >= nextPollAt[1]
    /\ LET mode == awaitMode[1]
           targets == AwaitTargetFor(mode)
       IN
        IF AwaitResolved(mode, targets)
        THEN
            /\ resumed' = [resumed EXCEPT ![1] = TRUE]
            /\ awaitMode' = [awaitMode EXCEPT ![1] = "none"]
            /\ resumeOutcome' = [resumeOutcome EXCEPT ![1] =
                  IF mode = "any"
                  THEN ObservedOutcome(status[FirstResolved(targets)])
                  ELSE ObservedOutcome(status[CHOOSE c \in targets : TRUE])]
            /\ UNCHANGED nextPollAt
        ELSE
            /\ nextPollAt' = [nextPollAt EXCEPT ![1] = FutureClock(clock, AwaitTimeout)]
            /\ UNCHANGED <<awaitMode, resumed, resumeOutcome>>
    /\ clock' = NextClock(clock)
    /\ UNCHANGED status

Tick ==
    /\ clock' = NextClock(clock)
    /\ UNCHANGED <<status, awaitMode, resumed, resumeOutcome, nextPollAt>>

\* =============================================================================
\* NEXT-STATE RELATION
\* =============================================================================

Next ==
    \/ (\E i \in Instances, o \in Outcomes : SettleChild(i, o))
    \/ (\E p \in Instances : CascadeTerminate(p))
    \/ (\E m \in {"one", "any", "all"} : StartAwait(m))
    \/ Repoll
    \/ Tick

\* =============================================================================
\* INITIAL STATE
\* =============================================================================

Init ==
    /\ status = [i \in Instances |-> "ready"]
    /\ awaitMode = [i \in Instances |-> "none"]
    /\ resumed = [i \in Instances |-> FALSE]
    /\ resumeOutcome = [i \in Instances |-> NoOutcome]
    /\ nextPollAt = [i \in Instances |-> 0]
    /\ clock = 0

\* =============================================================================
\* FAIRNESS
\* =============================================================================

(*
  Fair: StartAwait (so instance 1 actually begins an await in every
  behaviour TLC has to consider for L1/L2 -- without this, "never calls
  AwaitChild" is a trivially satisfying but useless way to avoid a liveness
  obligation), Repoll (the backstop itself), and CascadeTerminate for every
  instance (so a settled parent's TERMINATE children, and in turn THEIRS,
  do not wait forever -- this is what lets instance 4 ever be reached).

  SettleChild IS fair for instance 1 (the root) -- unlike
  CleatRunLifecycle.tla's own Terminate/Cancel/AdminForce*/RetryWorkflow/
  AdminReReplay, which stay deliberately unfair there. The difference is
  what each model takes as its own subject versus as a GIVEN from
  elsewhere: CleatRunLifecycle.tla IS the model of whether a run eventually
  settles (L1_EventualSettlement, deliberately left ungated there for its
  own, separate reasons -- see that property's header comment), so making
  its operator verbs fair would be begging its own question. This model
  takes "every run eventually settles, absent a reason it would not" as
  GIVEN for the ROOT -- it is #1997's subject, not #1998's (see SCOPE
  DECISIONS above) -- and an unfair root tests nothing about the
  await/cascade layer this file exists to check: the first clean-spec run
  (blanket fairness over every instance, no known-positive mutation) found
  exactly this with instance 2 unfair -- it simply never settled, forever,
  vacuously defeating L1 by denying its own precondition. Measured
  2026-10-07: 34693 distinct states, counter-example at depth 5.

  SettleChild is DELIBERATELY NOT fair for instances 2, 3, 4 -- all three
  carry ClosePolicy = "TERMINATE" (see ClosePolicy above), and giving them
  the same blanket fairness makes L3 untestable rather than true: with
  every instance independently guaranteed to settle on its own, eventually,
  regardless of any cascade, a one-level-only CascadeTerminate (the pre-
  #1108 shape) could never be caught, because instance 4 would always
  eventually settle anyway, by its OWN fairness, cascade or no cascade.
  Measured 2026-10-07, confirming this is not hypothetical: restricting
  CascadeTerminate to the root only (p = 1), with SettleChild fair on every
  instance, left L3_CascadeReachesEveryDescendant reporting "No error has
  been found" -- a false green on exactly the bug #1998 exists to catch.
  After narrowing SettleChild's fairness to the root alone, the identical
  mutation produces a real counter-example (see specs/CleatParentAwait.md).
  SettleChild stays ENABLED for 2/3/4 regardless (a TERMINATE-policy child
  CAN still finish on its own, and sometimes will, in any one behaviour
  TLC explores) -- only the GUARANTEE is removed, which is the honest
  reading of a workflow nothing is forcing to progress, the same shape
  Crash/Restart get in CleatRunLifecycle.tla.

  This also means L1's own guarantee (instances 2 and 3 are the "all"/"any"
  await targets, and both carry ClosePolicy = TERMINATE) runs entirely
  through the cascade now: instance 1 settles under its own fairness,
  CascadeTerminate(1) -- fair -- then forces 2 and 3 to 'terminated'. That
  is not a coincidence of this fixed topology, it is the point: #1998's own
  actor list couples "a parent awaiting a child" to "the parent-close
  cascade" precisely because, for a TERMINATE-policy child, the cascade can
  be the ONLY thing standing between an await and waiting forever once the
  child's own workflow would otherwise never finish -- which is a stronger,
  more faithful claim than "children make progress for unrelated reasons",
  and it is the reason L1 and L3 share one Fairness clause rather than
  being checked in isolation.
*)
Fairness ==
    /\ \A m \in {"one", "any", "all"} : WF_vars(StartAwait(m))
    /\ WF_vars(Repoll)
    /\ \A p \in Instances : WF_vars(CascadeTerminate(p))
    /\ WF_vars(\E o \in Outcomes : SettleChild(1, o))

Spec == Init /\ [][Next]_vars /\ Fairness

\* No .cfg CONSTRAINT here, deliberately -- same reason as CleatRunLifecycle.
\* tla: clock is self-clamping (NextClock/ClockCeiling above), so there is
\* nothing for a CONSTRAINT to exclude, and excluding anything would risk
\* the exact silent liveness-defeat that file's header measured twice.

\* =============================================================================
\* SAFETY INVARIANTS
\* =============================================================================

\* ResumeOutcomeNeverChangesOnceSet: resumeOutcome[1], once set, is never again mutated, and
\* at the instant it is set every member relevant to the mode that produced
\* it was settled. Restated as an ACTION invariant (the same reason
\* CleatRunLifecycle.tla's SettledIsFinal is one, not a temporal `[]`-only
\* form: a plain state invariant over `resumed` alone cannot see the
\* transition, only the latched result, and TLC's own warning about
\* temporal forms interacting with a clamped clock does not apply to a pure
\* reachability check, which this is).
ResumeOutcomeNeverChangesOnceSet ==
    [][\A i \in Instances :
         (resumeOutcome[i] /= NoOutcome /\ resumeOutcome'[i] /= resumeOutcome[i]) => FALSE]_vars

\* Safety == TypeOK here: ResumeOutcomeNeverChangesOnceSet is an ACTION
\* formula (it has primed variables), so -- unlike CleatRunLifecycle.tla's
\* identically-shaped SettledIsFinal -- it cannot be folded into an
\* INVARIANT conjunction; TLC rejects that with "is not a state predicate".
\* It is declared under PROPERTIES in the .cfg instead, same as
\* SettledIsFinal is there.
Safety == TypeOK

\* =============================================================================
\* LIVENESS
\* =============================================================================

(*
  L1 -- AwaitEventuallyResumes: once instance 1 starts awaiting (whatever
  the mode), it eventually resumes. This is the property #1974 broke: with
  PreFix1974Mapping = TRUE, ObservedOutcome(status[c]) is permanently
  NoOutcome for a terminated or cancelled child, AwaitResolved can never
  become true for a target set that settles that way, and Repoll reschedules
  itself forever -- the exact "replays forever" #1974's own issue text
  names. See specs/CleatParentAwaitPreFix1974.cfg.
*)
L1_AwaitEventuallyResumes ==
    [](awaitMode[1] /= "none" => <>resumed[1])

(*
  L2 -- AwaitModeResolvesByItsOwnRule: "all" resumes only once EVERY target
  is settled, and "any"/"one" resume as soon as ONE relevant target is.
  Stated as a safety-shaped companion to L1 rather than a second leads-to:
  the interesting failure mode for L2 is not non-termination (L1 already
  covers that) but resuming on the WRONG condition -- e.g. "all" resuming
  with a target still unsettled, which AwaitResolved's own ALL quantifier
  must never get wrong, or "any" resuming before anything has settled at
  all. Checked at the instant of resumption via the same action-invariant
  shape as ResumeOutcomeNeverChangesOnceSet.
*)
L2_AwaitModeResolvesByItsOwnRule ==
    [][\A i \in Instances :
         (awaitMode[i] /= "none" /\ resumed'[i] /\ ~resumed[i]) =>
            LET mode == awaitMode[i]
                targets == AwaitTargetFor(mode)
            IN  IF mode = "all"
                THEN \A c \in targets : IsSettled(status[c])
                ELSE \E c \in targets : IsSettled(status[c])]_vars

(*
  L3 -- CascadeReachesEveryDescendant: once the root (instance 1) settles,
  EVERY descendant under a TERMINATE-policy path -- not just direct children
  -- eventually settles. This is #1108: the fixed topology's descendant set
  for instance 1 is {2, 3, 4} (4 via 2), and this property is what a
  one-level-only cascade (the pre-#1108 shape) would violate: with
  CascadeTerminate restricted to firing once per parent rather than
  re-firing once a child becomes settled, 4 would never be reached. Nothing
  in this model artificially recreates that restriction -- CascadeTerminate
  is already written to be callable again once ITS target is settled, which
  is the fix, not a toggle -- so this property is expected to hold at
  PreFix1974Mapping = FALSE and is not given a known-positive .cfg the way
  L1 is; #1108 and #1978 arrived in the same commit range and neither has
  an independent "pre-fix" switch worth adding for one property each.
*)
Descendants(p) == {c \in Instances : ParentOf[c] = p} \cup
                   {g \in Instances : \E c \in Instances : ParentOf[c] = p /\ ParentOf[g] = c}

L3_CascadeReachesEveryDescendant ==
    [](IsSettled(status[1])
        => <>(\A d \in Descendants(1) : ClosePolicy[d] = "TERMINATE" => IsSettled(status[d])))

(*
  S1 -- ObservedOutcomeMatchesFinalStatus: the safety half of the table in
  #1998 ("a settled child's outcome, as seen by the parent, matches its
  status"). Gated as the action invariant ResumeOutcomeNeverChangesOnceSet
  plus L2's own per-resumption check above decompose this into "the kind
  recorded never changes" and "the kind recorded was earned by the right
  rule"; this invariant is the third piece -- the recorded kind, at the
  moment it is set, equals ObservedOutcome of the status that justified it,
  not some other member's.
*)
S1_ObservedOutcomeMatchesFinalStatus ==
    [][\A i \in Instances :
         (awaitMode[i] /= "none" /\ resumed'[i] /\ ~resumed[i]) =>
            LET mode == awaitMode[i]
                targets == AwaitTargetFor(mode)
            IN  \E c \in targets :
                    IsSettled(status[c]) /\ resumeOutcome'[i] = ObservedOutcome(status[c])]_vars

===============================================================================
