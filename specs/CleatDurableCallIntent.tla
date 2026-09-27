------------------------ MODULE CleatDurableCallIntent ------------------------
(*
  Cleat Durable Call Intent: write-ahead call intents, ambiguity, re-replay,
  retention.

  cleat#1999. engine/callintent.go's WriteAheadIntent semantics durably record
  a call BEFORE dispatching it, so a crash between dispatch and response
  recording is reported as [AMBIGUOUS] on replay rather than silently
  redispatched. This model checks whether that promise actually holds once
  two other mechanisms are allowed to touch the same durable row: an
  operator's AdminReReplay (which resumes a stopped workflow after checking
  for a pending intent) and --retention-days' event-history sweep (which
  deletes a terminal workflow's event_history unconditionally, including a
  still-pending intent). It also checks cleat#1984's not-yet-built declared
  resolver design, specifically its own stated open question: can a "the
  call was never sent" answer be told apart from "cannot say"?

  Every actor and every fencing rule below was read from
  github.com/cleat-team/cleat @ c65958c4 (2026-09-22), not from the issue
  body or from memory. Re-grep before trusting it against a later commit,
  per CLAUDE.md's own rule on numbers that rot.

  Implemented by (no line numbers -- see CleatClaim.tla's header for why):
    - engine/callintent.go           (CallSemantics, WriteAheadIntent, intentStore,
                                       EventRecord.isPendingIntent, freshCallWithIntent,
                                       AmbiguityResolver, ResolveCall, resolveAmbiguity)
    - engine/store_intent.go         (PostgresStore/MySQLStore/MSSQLStore:
                                       WriteCallIntent, CompleteCallIntent,
                                       ResolveCallIntent)
    - engine/admin_ops.go            (ReReplay -- the pending-intent guard:
                                       refuses iff any loaded EventRecord
                                       isPendingIntent(); a LoadEventHistory
                                       error skips the guard rather than
                                       failing it)
    - engine/store_admin_rereplay.go (PostgresStore: AdminReReplay -- the
                                       fenced UPDATE the guard above protects)
    - engine/db.go                   (PostgresStore: DeleteExpiredEvents --
                                       the --retention-days sweep)
    - cmd/cleat-worker/config.go     (the retention-days flag: default 30,
                                       ON; its own doc comment's claim about
                                       'failed' workflows is examined below)
    - cmd/cleat-worker/setup.go      (FinalizeWorkflowSegment's one call
                                       site -- computes finalStatus, and
                                       never passes "failed"; and
                                       recordTerminalFailureWithHistory,
                                       the real 'failed' path)
    - engine/store_lifecycle.go      (PostgresStore: FailWorkflow -- the
                                       real production path to 'failed',
                                       contains no event_history deletion)

  A DOCUMENTATION DEFECT THIS MODEL EXISTS TO SETTLE, stated up front rather
  than left implicit in the model's shape:

    engine/db.go's DeleteExpiredEvents doc comment and
    engine/retention_predicates.go's shared-predicate comment both assert
    that the sweep's event-deletion arm "can never match" for a 'failed'
    workflow, because finalize_workflow_status "already deleted those rows"
    and "the worker takes that path in production (FinalizeWorkflowSegment)".

    That is contradicted by cmd/cleat-worker/setup.go's OWN comment on
    FinalizeWorkflowSegment's one production call site: "finalStatus here
    is only ever 'done' or 'ready' ... never 'failed' -- a segment that
    fails does not reach FinalizeWorkflowSegment at all, it goes through
    recordTerminalFailureWithHistory instead." Grepping every call site of
    .FinalizeWorkflowSegment( confirms exactly one production caller, and
    that caller's finalStatus is computed as "done", conditionally
    reassigned to "ready", and never "failed".

    It is also contradicted empirically: engine/store_admin_rereplay_test.go's
    TestAdminReReplay_ResetsAStoppedWorkflowAndKeepsItsHistory fails a
    claimed workflow through store.FailWorkflow (the real production
    method -- confirmed by reading it to contain no event_history DELETE
    anywhere in its body) and then asserts exactly one preserved event
    survives.

    So db.go's "can never match" claim is true of a code path 'failed'
    workflows do not take, and false of the one they do. The practical
    consequence: --retention-days defaults to 30 and is ON by default, so
    on a default deployment, thirty days after a workflow fails, its
    event_history -- including any still-pending call intent -- is deleted
    by DeleteExpiredEvents' first (event-only) arm. This is the concrete
    mechanism RetentionSweep below models, and it is the reason this
    model's counter-examples are not a hypothetical worst case.

  SCOPE DECISIONS, stated rather than silently applied:

    - ONE WORKFLOW, ONE CALL STEP. The properties in question (S1-S3, L1)
      are all about a single (workflowID, runID, step) key's durable
      history; nothing about a second workflow or a second step changes
      the guard logic being checked, so modeling many is pure state-space
      cost with no discriminating power. Same reasoning CleatClaim.tla
      gives for NumInstances = 2 rather than N.

    - wfStatus IN {"active", "failed"}. 'terminated' and 'dead_lettered'
      share the exact same reReplayableStatuses gate and the exact same
      pending-intent guard in engine/admin_ops.go -- confirmed by reading
      ReReplay, which does not branch on which of the three the workflow
      is in. Collapsing them to "failed" loses no behavior this model
      checks. 'done' is deliberately excluded: it is not in
      reReplayableStatuses (store_admin.go's own comment: replay-to-
      completion would be redundant with dead-letter reprocess), so no
      redispatch hazard exists once a workflow is genuinely done.

    - THE SERVICE'S GROUND TRUTH IS COLLAPSED INTO ONE COUNTER,
      serviceApplyCount, INCREMENTED UNCONDITIONALLY BY Dispatch. A crash
      between "intent written" and "service actually called" is
      observationally identical to any other crash while historyRow =
      "pending" -- the engine cannot tell the two apart either, which is
      the entire reason ambiguity exists -- and a Dispatch that never
      really reached the service cannot violate S1 (nothing to duplicate).
      Modeling every Dispatch as "reached the service" is the sound,
      strictly-harder-for-safety choice: if S1 holds when every dispatch
      counts, it holds when some silently don't.

    - RetryWorkflow (dead_lettered -> ready, cmd/cleat-worker/app.go's
      st.RetryWorkflow call) HAS NO EQUIVALENT GUARD AT ALL -- confirmed
      by reading its one call site and finding no wrapping function
      analogous to ReReplay's isPendingIntent() check anywhere between the
      HTTP handler and the store method. This is arguably a WORSE gap than
      the one this model checks (no protection to begin with, rather than
      protection defeated by retention), and it is explicitly OUT OF
      SCOPE here: this model's AdminReReplayGuardAllows/Refuses actions
      are ReReplay's guard specifically. Filed as a separate follow-up
      rather than folded in, per CLAUDE.md's "one PR, one thing" and
      because a missing guard and a defeated guard are different defects
      needing different fixes.

    - THE MANUAL "reprocess" DEAD-LETTER PATH (a fresh run from
      definition+input, touching no existing history) is out of scope for
      the same reason: it is not a redispatch of a RECORDED intent, it is
      a deliberately new one.

    - RESOLVER ANSWER SEMANTICS follow cleat#1984's own proposed contract
      table exactly: 200 -> resolved with a response, 404 -> "never
      happened", 500/timeout -> "cannot say". The model's
      NotSentIsSafeRetry CONSTANT directly represents cleat#1984's own
      stated open design question -- "the current ResolveCall interface
      can express resolved=true+response or resolved=false; check whether
      it can express 'retry this step' before adding anything. If it
      can't, the 404 row starts as 'cannot say'" -- rather than silently
      picking an answer for it.
*)
EXTENDS Naturals, FiniteSets, TLC

CONSTANTS
    Dedupe,             \* BOOLEAN: does the downstream service dedupe by idempotency key?
    NotSentIsSafeRetry  \* BOOLEAN: cleat#1984's open question -- can a 404/"never sent"
                        \* answer legitimately clear the intent and permit a fresh
                        \* Dispatch, or does today's ResolveCall interface force it to
                        \* degrade to "cannot say"?

VARIABLES
    historyRow,               \* "absent" | "pending" | "complete" -- the ONLY durable,
                              \* replay-visible state of this call step (EventRecord's
                              \* presence/absence and its isPendingIntent()/complete shape)
    serviceApplyCount,        \* Nat -- the downstream service's own ground truth: how
                              \* many times it actually applied this idempotency key
    witnessedSinceLastDispatch, \* BOOLEAN -- has a [AMBIGUOUS] report OR a durably
                              \* recorded resolution occurred since the last real send?
                              \* This is S1's own exception clause, made a variable.
    s1Violated,                \* BOOLEAN -- latched TRUE the first time a real send
                              \* occurs with no witness since the previous one
    wfStatus                  \* "active" | "failed"

vars == <<historyRow, serviceApplyCount, witnessedSinceLastDispatch, s1Violated, wfStatus>>

TypeOK ==
    /\ historyRow \in {"absent", "pending", "complete"}
    /\ serviceApplyCount \in Nat
    /\ witnessedSinceLastDispatch \in BOOLEAN
    /\ s1Violated \in BOOLEAN
    /\ wfStatus \in {"active", "failed"}

Init ==
    /\ historyRow = "absent"
    /\ serviceApplyCount = 0
    /\ witnessedSinceLastDispatch = TRUE  \* vacuous: nothing has ever been sent yet
    /\ s1Violated = FALSE
    /\ wfStatus = "active"

(*
  ApplyCeiling / NextApplyCount: serviceApplyCount is SELF-CLAMPING, not
  bounded by a .cfg CONSTRAINT -- the cleat#2034 lesson (specs/README.md),
  paid for once already on CleatRunLifecycle.tla's ClockCeiling. Without a
  cap, serviceApplyCount is unbounded: Dispatch -> Crash -> RetentionSweep
  -> AdminReReplayGuardAllows -> Dispatch is a real cycle back to
  historyRow = "absent" with wfStatus = "active" again, so TLC's state
  space is infinite and it never terminates (measured: the shipped .cfg
  run below did not finish in 120s before this fix). A hard-wall
  CONSTRAINT excluding serviceApplyCount' > ApplyCeiling would silently
  discard every trace that would have exceeded it -- including, per the
  ClockCeiling comment this mirrors, possibly the very counter-example
  trace S1 exists to find. Clamping is safe here because nothing in this
  model ever reads serviceApplyCount's exact value, only serviceApplyCount
  > 0 (Dispatch's own violation guard) and serviceApplyCount <= 1 (S2) --
  both are decided identically whether the true count is 3 or 3000.
*)
ApplyCeiling == 3
NextApplyCount(c) == IF c < ApplyCeiling THEN c + 1 ELSE ApplyCeiling

-----------------------------------------------------------------------------
(*
  Dispatch: WriteCallIntent commits (historyRow -> "pending") and the call is
  actually placed with the downstream service, in engine/callintent.go's
  freshCallWithIntent -- write-then-call, no durable marker in between (that
  gap is exactly why ambiguity exists on a crash). Always counted as a real
  send; see the SCOPE DECISIONS note above for why that is the sound choice.

  A deduping service (Dedupe = TRUE) applies the idempotency key at most
  once, by construction: `applied` is FALSE for every send after the first,
  so serviceApplyCount never exceeds 1 and s1Violated's guard (which only
  latches on a REAL, i.e. applied, second send) can never fire. This encodes
  S2 structurally rather than searching for it -- the same kind of modeling
  shortcut CleatRunLifecycle.tla's OwesDefer takes, and is why this file
  ships ONE .cfg (Dedupe = FALSE, the harder case for S1) and documents the
  Dedupe = TRUE run as a separate, non-CI-gated local verification below.
*)
Dispatch ==
    /\ historyRow = "absent"
    /\ wfStatus = "active"
    /\ LET applied == IF Dedupe THEN serviceApplyCount = 0 ELSE TRUE IN
         /\ historyRow' = "pending"
         /\ serviceApplyCount' = IF applied THEN NextApplyCount(serviceApplyCount) ELSE serviceApplyCount
         /\ s1Violated' = IF applied /\ serviceApplyCount > 0 /\ ~witnessedSinceLastDispatch
                             THEN TRUE ELSE s1Violated
         /\ witnessedSinceLastDispatch' = IF applied THEN FALSE ELSE witnessedSinceLastDispatch
    /\ UNCHANGED wfStatus

(*
  CompleteResponse: the service replied and CompleteCallIntent durably
  records it before anything else goes wrong -- the ordinary, non-crash
  path. engine/callintent.go: recordEventPersisted after a successful call.
*)
CompleteResponse ==
    /\ historyRow = "pending"
    /\ wfStatus = "active"
    /\ historyRow' = "complete"
    /\ UNCHANGED <<serviceApplyCount, witnessedSinceLastDispatch, s1Violated, wfStatus>>

(*
  Crash: the worker dies with the call intent written and possibly in
  flight, unrecorded. Generalised to fire from any wfStatus = "active"
  state (not only historyRow = "pending") because a workflow can fail for
  an unrelated reason -- a different step, a guest bug -- at any point;
  narrowing the guard to "pending" only would silently assume the call
  step is the sole source of failure, which cmd/cleat-worker/setup.go's
  recordTerminalFailureWithHistory does not.
*)
Crash ==
    /\ wfStatus = "active"
    /\ wfStatus' = "failed"
    /\ UNCHANGED <<historyRow, serviceApplyCount, witnessedSinceLastDispatch, s1Violated>>

(*
  ReplaySeesAmbiguous: a worker claims the (now active again) workflow and
  replay walks into the pending row, surfacing [AMBIGUOUS] to the guest --
  engine/callintent.go's ambiguousCall / recordAmbiguity. This is the
  "report" half of S1's own exception clause: it does not resolve the row,
  it only counts as a witness that the operator/guest now KNOWS this step
  is ambiguous, which is the standing justification for the NEXT dispatch.
*)
ReplaySeesAmbiguous ==
    /\ historyRow = "pending"
    /\ wfStatus = "active"
    /\ witnessedSinceLastDispatch' = TRUE
    /\ UNCHANGED <<historyRow, serviceApplyCount, s1Violated, wfStatus>>

(*
  OperatorResolves: POST .../resolve -- a human records what actually
  happened. Durably completes the row and counts as a witness.
*)
OperatorResolves ==
    /\ historyRow = "pending"
    /\ historyRow' = "complete"
    /\ witnessedSinceLastDispatch' = TRUE
    /\ UNCHANGED <<serviceApplyCount, s1Violated, wfStatus>>

(*
  ResolverAnswersFound: cleat#1984's declared-lookup AmbiguityResolver, 200
  case -- "resolved=true, response=<found>". Same effect as an operator
  resolution: engine/callintent.go's resolveAmbiguity only reports success
  if the resolved outcome was durably persisted, so this and
  OperatorResolves are the same durable transition from two different
  callers.
*)
ResolverAnswersFound ==
    /\ historyRow = "pending"
    /\ historyRow' = "complete"
    /\ witnessedSinceLastDispatch' = TRUE
    /\ UNCHANGED <<serviceApplyCount, s1Violated, wfStatus>>

(*
  ResolverAnswersNotSent: cleat#1984's 404 case -- "the lookup op reports
  this idempotency key was never applied." This is the action that
  directly tests cleat#1984's own open design question.

  NotSentIsSafeRetry = TRUE: an improved contract where this answer both
  clears the intent (historyRow -> "absent", since we now KNOW nothing
  happened) and counts as a witness -- the subsequent Dispatch from
  "absent" is legitimate because a definite resolution occurred.

  NotSentIsSafeRetry = FALSE: today's actual ResolveCall interface, which
  per cleat#1984's own text can only express resolved=true+response or
  resolved=false -- there is no third outcome for "definitely not sent,
  safe to retry". This action is a deliberate NO-OP under that config: it
  changes nothing, exactly matching cleat#1984's fallback -- "the 404 row
  starts as cannot say". Its presence in Next with a real guard (rather
  than being omitted) is what lets L1 be checked, and fail, under this
  config: it shows there is currently NO action that ever resolves a
  "never sent" answer into anything.
*)
ResolverAnswersNotSent ==
    /\ historyRow = "pending"
    /\ historyRow' = (IF NotSentIsSafeRetry THEN "absent" ELSE historyRow)
    /\ witnessedSinceLastDispatch' = (IF NotSentIsSafeRetry THEN TRUE ELSE witnessedSinceLastDispatch)
    /\ UNCHANGED <<serviceApplyCount, s1Violated, wfStatus>>

(*
  ResolverAnswersUnknown: cleat#1984's 500/timeout case -- "cannot say".
  Not fatal, leaves the ambiguity exactly as it was; engine/callintent.go's
  resolveAmbiguity comment: an error here is non-fatal and the row stays
  pending.
*)
ResolverAnswersUnknown ==
    /\ historyRow = "pending"
    /\ UNCHANGED vars

(*
  RetentionSweep: cmd/cleat-worker/setup.go's --retention-days cron calling
  DeleteExpiredEvents, once a workflow is terminal. Deletes ALL event_history
  rows for the workflow_id UNCONDITIONALLY -- engine/db.go's DeleteExpiredEvents
  has no per-row branch on pending vs. complete, per the DOCUMENTATION DEFECT
  note in this file's header. Modeled as a single nondeterministic firing,
  abstracting "enough days have passed" the same way every other timeout in
  CleatRunLifecycle.tla is a plain enabling condition rather than a clock
  comparison -- there is nothing here for a clock to add, since the sweep's
  hazard is which STATE it fires in, not when.
*)
RetentionSweep ==
    /\ wfStatus = "failed"
    /\ historyRow /= "absent"
    /\ historyRow' = "absent"
    /\ UNCHANGED <<serviceApplyCount, witnessedSinceLastDispatch, s1Violated, wfStatus>>

(*
  AdminReReplayGuardAllows: engine/admin_ops.go's ReReplay, guard passed.
  The real guard loads event history and refuses iff ANY record
  isPendingIntent() is found -- so it passes when historyRow = "absent"
  (nothing recorded -- indistinguishable from "genuinely never attempted")
  OR historyRow = "complete" (properly resolved). The "absent because
  RetentionSweep deleted a genuinely-pending row" case is exactly the one
  this guard cannot tell apart from "never attempted", which is the
  suspected gap this model exists to confirm or refute.
*)
AdminReReplayGuardAllows ==
    /\ wfStatus = "failed"
    /\ historyRow /= "pending"
    /\ wfStatus' = "active"
    /\ UNCHANGED <<historyRow, serviceApplyCount, witnessedSinceLastDispatch, s1Violated>>

(*
  AdminReReplayGuardRefuses is deliberately NOT a separate action: when
  historyRow = "pending", the real guard returns ErrAdminStateConflict and
  changes nothing, which TLA+ already represents as no action being enabled
  to leave wfStatus = "failed" via re-replay in that state. Nothing needs
  to stutter it explicitly.
*)

Next ==
    \/ Dispatch
    \/ CompleteResponse
    \/ Crash
    \/ ReplaySeesAmbiguous
    \/ OperatorResolves
    \/ ResolverAnswersFound
    \/ ResolverAnswersNotSent
    \/ ResolverAnswersUnknown
    \/ RetentionSweep
    \/ AdminReReplayGuardAllows

Spec == Init /\ [][Next]_vars /\ WF_vars(OperatorResolves \/ ResolverAnswersFound \/ ResolverAnswersNotSent)

-----------------------------------------------------------------------------
(*
  S1 (I1-shaped): a non-deduping service never receives a second send for
  the same key without either a [AMBIGUOUS] report or a recorded resolution
  having occurred since the previous send. s1Violated is latched by
  Dispatch's own guard above; this is a plain state INVARIANT, checked on
  every reachable state by ordinary BFS regardless of fairness -- so the
  WF_vars clause on Spec (needed for L1 below) cannot suppress the very
  trace that violates this.

  NOT GATED IN THE SHIPPED .cfg. Per the DOCUMENTATION DEFECT note above,
  this is EXPECTED to fail on the clean spec: Dispatch -> Crash ->
  RetentionSweep (historyRow: pending -> absent) -> AdminReReplayGuardAllows
  (wfStatus: failed -> active, since historyRow /= "pending") -> Dispatch
  again, with no ReplaySeesAmbiguous/OperatorResolves/resolver action ever
  having fired, sets s1Violated. This is the trace CLAUDE.md's own
  "could this check have disagreed?" question demands before believing
  a clean run -- and the answer here is a clean run would be the wrong
  answer, because the trace is real and short. `make tla` finding it is
  this model doing its job, not a broken model; see specs/README.md's
  entry for this file for the confirmed trace and its Go-level follow-up.
*)
S1_NoUnwitnessedRedispatch == ~s1Violated

(*
  S2: a deduping service applies the operation at most once per key.
  Structural under Dedupe = TRUE (see Dispatch's own comment) rather than
  found by search -- confirmed non-vacuous by running TLC with
  Dedupe = TRUE locally (not shipped as a second .cfg; Makefile's tla:
  target pairs one .cfg to one .tla by exact basename, so a second
  configuration cannot be auto-discovered without a second file, and this
  finding does not need one). serviceApplyCount <= 1 throughout under that
  config is the concrete statement.
*)
S2_DedupeAtMostOnce == Dedupe => serviceApplyCount <= 1

(*
  S3: a resolution recorded for this step is what replay returns for it --
  no action ever silently moves historyRow from "complete" back to
  "pending" (only RetentionSweep may take it to "absent", which is a
  DELETION, a different and separately-named hazard, not a silent
  overwrite). True by construction of every action above (Dispatch's own
  guard requires historyRow = "absent"), stated here as an ACTION-level
  PROPERTY so a future edit that adds a complete -> pending edge is caught
  by TLC rather than by re-reading every action by hand -- the same
  reasoning CleatRunLifecycle.tla gives for making SettledIsFinal a
  checked property instead of a comment.
*)
S3_ResolutionIsDurable ==
    [][historyRow = "complete" => historyRow' \in {"complete", "absent"}]_vars

(*
  L1: a pending ambiguity, once an operator or resolver is available to act
  on it, eventually reaches a properly recorded resolution (historyRow =
  "complete") -- NOT merely "eventually stops being pending", since
  RetentionSweep can also take it out of "pending" by deleting it, which is
  the opposite of settling it.

  WF_vars on OperatorResolves \/ ResolverAnswersFound \/
  ResolverAnswersNotSent is the right fairness here and NOT the WF-is-
  insufficient trap CleatRunLifecycle.tla hit three times on its own L1-L3:
  there, an adversarial single-step action could revoke the antecedent
  (leads-to's trigger condition) before a fairness-gated action got a
  qualifying window. Here, once historyRow = "pending", NOTHING disables
  OperatorResolves except historyRow itself changing -- there is no
  toggling precondition to race. WF is exactly "if continuously enabled,
  must eventually fire", and OperatorResolves is continuously enabled for
  as long as historyRow = "pending" holds, so no extra escape clause is
  needed the way L2/L3 needed one there.

  NOT GATED IN THE SHIPPED .cfg (NotSentIsSafeRetry = FALSE), for the same
  reason S1 is not: this is the EXPECTED, CONFIRMED-BY-SOURCE finding, not
  a surprise to hunt down. Two independent ways it fails on the clean spec:

    (a) RetentionSweep can fire while historyRow = "pending" (wfStatus must
        be "failed" for it to be enabled, but Crash can establish that
        while historyRow is still "pending", before any resolver acts),
        taking historyRow to "absent" -- WF's antecedent (historyRow =
        "pending") becomes false, discharging the fairness obligation on
        OperatorResolves/ResolverAnswersFound/ResolverAnswersNotSent
        WITHOUT either ever firing. This is the SAME root cause as S1's
        counter-example -- an unconditional sweep of a pending row -- shown
        here as a liveness violation instead of a safety one.

    (b) Under NotSentIsSafeRetry = FALSE specifically: ResolverAnswersNotSent
        is enabled and can fire (satisfying WF's disjunction) while being a
        pure no-op, so WF is satisfied by an action that changes nothing --
        historyRow can stay "pending" forever with the fairness obligation
        met every single step. This is cleat#1984's own open question,
        answered: NO, a 404 cannot currently settle an ambiguity, and this
        is the trace that shows it.

  Running with NotSentIsSafeRetry = TRUE (not shipped as a second .cfg, per
  S2's note above) removes hazard (b) but not (a) -- confirming the two are
  independent causes, not one bug wearing two names.
*)
L1_PendingEventuallySettles ==
    [](historyRow = "pending" => <>(historyRow = "complete"))

=============================================================================
