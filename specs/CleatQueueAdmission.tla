----------------------- MODULE CleatQueueAdmission -----------------------
(*
  Cleat Queue Admission, Rate Tokens and Claim Fairness

  cleat#2000. Replaces specs/CleatConcurrencyKeys.tla, which modelled only the
  bare-key mutex (ON CONFLICT DO NOTHING, at most one holder per key) -- see
  specs/README.md for why that file is retired rather than fixed. This model
  covers what has been built since: a REGISTERED queue generalises the bare
  mutex to a semaphore (N holders, cleat#1116), gains an independent rate
  limiter on admission (cleat#1918) and a per-worker cap (cleat#1917), and the
  bare-key mutex itself is left to the earlier file's argument -- Acquire's
  ON CONFLICT DO NOTHING gives mutual exclusion structurally, by the same
  reasoning CleatConcurrencyKeys.tla's own MutualExclusion comment gave, and
  re-modelling it here would add state without adding coverage. What IS
  modelled is queue-scoped admission (S1, S3, and the #1917 worker cap that
  extends S1), the run-state holder-validity rule of cleat#1965 that replaced
  a fixed TTL (S2), and the per-worker rotation that spreads claims across
  tenants (L1, L2).

  Actors and state, from the code, not from memory -- re-run whenever this
  spec is touched:

    grep -rln "queue_holders\|queue_rate_tokens\|worker_concurrency\|tenantRotation\|claimRotating\|WorkerConcurrency" \
      engine/*.go cmd/cleat-worker/*.go | grep -v _test.go | sort

  gives, as of cleat#2025's merge (c44d5a48):
    cmd/cleat-worker/rotating_claim.go
    cmd/cleat-worker/setup.go
    engine/db.go
    engine/mssql_lifecycle.go
    engine/mssql_operations.go
    engine/mssql_signals_promises.go
    engine/mysql_lifecycle.go
    engine/mysql_ops.go
    engine/mysql_store.go
    engine/queue_store.go
    engine/store_lifecycle.go

  Implemented by (no line numbers -- CLAUDE.md's own rule on why a number
  rots faster than the fact it describes; the CI path filter matches on these
  names):
    - engine/store_lifecycle.go        (PostgresStore: ClaimWorkflows, lockRegisteredQueueLimits, acquireCandidateConcurrencyKey)
    - engine/mysql_lifecycle.go        (MySQLStore: the same three)
    - engine/mssql_lifecycle.go        (MSSQLStore: the same three)
    - engine/db.go                     (PostgresStore: ReleaseWorkflowConcurrencyKeys, ReapExpiredConcurrencyKeys)
    - engine/mysql_ops.go              (MySQLStore: ReleaseWorkflowConcurrencyKeys, ReapExpiredConcurrencyKeys)
    - engine/mssql_operations.go       (MSSQLStore: ReleaseWorkflowConcurrencyKeys)
    - engine/mssql_signals_promises.go (MSSQLStore: ReapExpiredConcurrencyKeys)
    - engine/mysql_store.go            (MySQLStore: queue-holder count read path)
    - engine/queue_store.go            (Queue config: concurrency_limit, rate_limit, worker_concurrency, and their validation)
    - cmd/cleat-worker/rotating_claim.go (tenantRotation cursor, claimRotating: the per-worker round robin)
    - cmd/cleat-worker/setup.go         (Worker.claimGeneral: dispatches to the rotating claim)

  What this model deliberately does NOT cover, and why:

  - The bare-key mutex (concurrency_keys, no registered queue). Structurally
    trivial (a primary key gives mutual exclusion); CleatConcurrencyKeys.tla
    already argued this and nothing built since changes that argument.

  - A HOLDER MOVING to a different worker when a parked run wakes and is
    claimed by someone else (cleat#1917 decision 4, acquireCandidateConcurrencyKey's
    "Move" branch). Every run in this model is claimed at most once and
    settles without parking. The move branch changes WHICH worker's cap a
    holder counts against, not whether the queue's total is respected -- S1
    holds identically whether a holder was inserted fresh or moved, since
    both are gated by the same locked count. Worth a second model if the
    move path itself is ever suspected, not needed for S1-S3/L1-L2 here.

  - claimedKeyTTL's 7-day time-based backstop in ReapExpiredConcurrencyKeys.
    Its own comment (concurrency_key_holder.go) says plainly what it is: "a
    long safety bound..., not what decides whether it is still held" -- a
    catch-all for a bug in the run-state join, a bypassed release, or
    anything "not yet found". It cannot be exercised by any state this model
    can reach, because every stranding this model produces (SettleWithoutRelease)
    is already caught by Reap's run-state disjunct on the very next Reap step.
    Modelling a backstop that only ever fires in a state the model can't
    reach is not a stronger model, it is a wider one for no coverage gained
    -- the same argument specs/README.md gives for keeping CleatClaim.tla's
    bounds small rather than "usually sufficient".

  - The retention-driven DELETE of a settled workflow_instances row (cascading
    to queue_holders via its ON DELETE CASCADE FK -- migrations/postgres/094).
    Reap's run-state disjunct (`NOT EXISTS ... status NOT IN (settled)`) is
    already true the instant status becomes terminal, before any retention
    sweep runs and whether or not the row is ever deleted at all -- retention
    changes nothing about which runs count as "live" for S1/S2/S3, it only
    changes whether the row physically exists later. Modelling it as a
    distinct action would duplicate Reap's effect on this model's variables
    for zero additional coverage of the properties below.

  Queue-slot state machine (per run, this queue only -- 'terminating' is the
  two-phase defer-phase hop; see engine/status_vocabulary.go's own distinction
  between SETTLED and CANNOT RUN GUEST, which is exactly ready/running vs.
  terminating vs. the five terminal values):

          +---> ready ----------claim---------> running
          |                                        |  |
          |                                        |  +--- BeginDeferPhase --> terminating
          |                                        |                              |
          |                                   SettleAndRelease            SettleAndRelease
          |                                   SettleWithoutRelease        SettleWithoutRelease
          |                                        |                              |
          |                                        v                              v
          |                                      done / failed / dead_lettered / terminated / cancelled
          |                                              |
          +<-------------------------- Reap (frees a stranded holder) --+

  HOW TO MODEL CHECK:

    java -cp tla2tools.jar tla2sany.SANY specs/CleatQueueAdmission.tla
    java -cp tla2tools.jar tlc2.TLC -config specs/CleatQueueAdmission.cfg specs/CleatQueueAdmission.tla

  `make tla` runs both, for every spec with a matching .cfg. Bounds and the
  measured state count are in specs/README.md, not here -- CI checks that file
  against this one every run, so a number copied here would rot the first
  time either changed alone (CLAUDE.md's own rule on why a live count is a
  query, not a sentence).
*)

EXTENDS Integers, FiniteSets

CONSTANTS
    NumTenants,        \* Tenants == 1..NumTenants; order is the rotation order,
                        \* mirroring ListTenantIDs' ORDER BY created_at, tenant_id.
    RunsPerTenant,     \* Runs == Tenants \X (1..RunsPerTenant).
    Workers,           \* Set of worker identifiers.
    ConcurrencyLimit,  \* This queue's concurrency_limit, same for every tenant
                        \* (queues.concurrency_limit is per (tenant_id, name); a
                        \* single constant models one representative tenant's
                        \* queue of that name, which is all any one tenant's
                        \* admission gate ever reads).
    WorkerCap,         \* This queue's worker_concurrency (cleat#1917). Always
                        \* declared in this model -- queue_store.go's own
                        \* validateWorkerConcurrency requires
                        \* 1 <= WorkerCap <= ConcurrencyLimit when it is set at
                        \* all, and NULL (undeclared) only widens the gate, so
                        \* checking the declared case is the one that can fail.
    RateLimit,         \* This queue's rate_limit (cleat#1918). Always declared,
                        \* same reasoning as WorkerCap.
    RatePeriod,        \* This queue's rate_period_seconds, in ticks. A token
                        \* mints at RatePeriod and COUNTS DOWN by one on every
                        \* step (see rateTokenRemaining below) rather than
                        \* being compared against a monotonic clock -- see the
                        \* "Why there is no clock variable" note below.
    NULL               \* Sentinel "no holder" value, distinct from every
                        \* worker -- a model value bound in the .cfg, exactly
                        \* as CleatClaim.tla's NULL is (CHOOSE x : x \notin Workers
                        \* is an unbounded CHOOSE TLC cannot evaluate).

ASSUME NumTenants >= 1
ASSUME RunsPerTenant >= 1
ASSUME ConcurrencyLimit >= 1
ASSUME WorkerCap >= 1 /\ WorkerCap <= ConcurrencyLimit  \* ck_queues_worker_concurrency_le_concurrency, migrations/postgres/098
ASSUME RateLimit >= 1
ASSUME RatePeriod >= 1
ASSUME NULL \notin Workers

\* =============================================================================
\* CONSTANT HELPERS
\* =============================================================================

Tenants == 1..NumTenants
Runs == Tenants \X (1..RunsPerTenant)

\* The five terminal values workflow_instances.status can hold, per
\* engine/status_vocabulary.go's own census -- settledStatusList, checked
\* identical at every call site by TestOneDefinitionOfSettled. 'terminating'
\* is deliberately excluded: it is mid-shutdown, still owed a terminal write,
\* and still holds its slot -- status_vocabulary.go's own file comment draws
\* exactly this line ("SETTLED" vs. "CANNOT RUN GUEST are not two spellings
\* of one idea").
TerminalStatuses == {"done", "failed", "dead_lettered", "terminated", "cancelled"}
NonTerminalStatuses == {"ready", "running", "terminating"}

\* =============================================================================
\* VARIABLES
\* =============================================================================

VARIABLES
    runStatus,             \* [Runs -> NonTerminalStatuses \cup TerminalStatuses]
    queueHolder,           \* [Runs -> Workers \cup {NULL}]  -- queue_holders membership
    rateTokenRemaining,    \* [Runs -> 0..RatePeriod]  a COUNTDOWN, not an absolute
                           \* expiry: 0 means "no active token"; minting sets it to
                           \* RatePeriod, and it decrements by one on every step
                           \* until it hits 0. See "Why there is no clock variable".
    rotCursor              \* [Workers -> Tenants \cup {0}]  -- tenantRotation.lastServed,
                           \* WORKER-LOCAL exactly as rotating_claim.go's own comment
                           \* insists it must be ("WORKER-LOCAL ON PURPOSE"). 0 means
                           \* "never served", the same role NULL plays for assignedTo
                           \* in CleatClaim, spelled differently because 0 \notin Tenants.

vars == <<runStatus, queueHolder, rateTokenRemaining, rotCursor>>

(*
  Why there is no clock variable, though the first version of this spec had
  one. cleat#2034 found CleatClaim.tla's ClaimProgress silently vacuous:
  deleting its WF(Claim(w)) entirely left "No error found" at an IDENTICAL
  state count. The same known-positive run against this spec's first version
  (a monotonic `clock`, rate tokens as absolute `expiresAt` compared against
  it, bounded by a CONSTRAINT `clock < 6`) reproduced the SAME failure on ALL
  THREE liveness properties (S2, L1, L2) before this spec ever merged -- see
  specs/README.md's "Known-positive" section for the measurements.

  The mechanism, and it is exactly what the TLC warning printed on every run
  says to go read (Specifying Systems section 14.3.5): every action in that
  version incremented clock unconditionally, so at the CONSTRAINT's boundary
  every REAL action's successor state was excluded from the graph TLC builds,
  leaving only the always-available stuttering step. From that point on
  nothing is ever "enabled" again in TLC's graph, so every WF_vars(...)
  condition is vacuously satisfied by an infinite tail with no visible
  progress -- independent of whether the fairness clause naming that action
  was present in the spec at all. A CONSTRAINT is sound for SAFETY invariants
  (which only need every reachable state visited) and unsound for LIVENESS
  under exactly this shape: an unboundedly growing variable, capped
  externally, that every action touches.

  The fix is not a bigger bound -- any finite bound reproduces the same
  boundary. It is removing the unbounded variable: rateTokenRemaining is a
  countdown in 0..RatePeriod, which is finite BY CONSTRUCTION from the
  CONSTANTS alone, so TLC needs no CONSTRAINT to keep the state space finite
  and liveness checking is sound without one. Re-run the same known-positive
  (drop a WF clause, compare the verdict AND the state count, per
  specs/README.md) before trusting any liveness property added here later.
*)

\* =============================================================================
\* TYPE INVARIANT
\* =============================================================================

TypeOK ==
    /\ runStatus \in [Runs -> NonTerminalStatuses \cup TerminalStatuses]
    /\ queueHolder \in [Runs -> Workers \cup {NULL}]
    /\ rateTokenRemaining \in [Runs -> 0..RatePeriod]
    /\ rotCursor \in [Workers -> Tenants \cup {0}]

\* A run holding a slot is always non-terminal, and a terminal run's slot (if
\* not yet reaped) is exactly the SettleWithoutRelease case -- stated as an
\* invariant so TLC cross-checks it against every reachable state, the same
\* "documentation and cross-checking" role CleatClaim.tla's own AtMostOnce
\* plays for a fact that is otherwise only implicit in the actions.
RunningRunsHoldSlots ==
    \A r \in Runs : runStatus[r] \in {"running", "terminating"} => queueHolder[r] # NULL

\* =============================================================================
\* STATE PREDICATES (per tenant -- queues.concurrency_limit, rate_limit and
\* worker_concurrency are all scoped to (tenant_id, name), so a tenant's
\* admission gate never reads another tenant's holders, tokens, or cap. This
\* is why L1 (rotation fairness) and L2 (same-tenant admission once a slot
\* frees) are genuinely different properties rather than restatements of one
\* fact: no tenant can be capacity-starved by another's traffic, only
\* attention-starved by a broken rotation, which is exactly what L1 checks.
\* =============================================================================

TenantReady(t) == {r \in Runs : r[1] = t /\ runStatus[r] = "ready"}

\* S1's holder set: queue_holders rows whose run is non-terminal, the
\* run-state predicate cleat#1965 made authoritative (store_lifecycle.go's
\* acquireCandidateConcurrencyKey and db.go's ReapExpiredConcurrencyKeys both
\* join on this, not on queue_holders.expires_at).
LiveHolders(t) == {r \in Runs : r[1] = t /\ queueHolder[r] # NULL /\ runStatus[r] \notin TerminalStatuses}

\* cleat#1917's per-worker cap: parked runs count too -- LiveHolders already
\* filters on run state alone, never on whether the run is currently
\* executing, which is decision 3 on #1917 ("what counts: slot-holders
\* attributed to a worker, INCLUDING parked runs") represented structurally
\* rather than by a separate flag.
WorkerLiveHolders(t, w) == {r \in LiveHolders(t) : queueHolder[r] = w}

\* cleat#1918's rate window: queue_rate_tokens rows not yet expired. Unlike
\* LiveHolders this does NOT join on run state -- migration 097's own comment
\* is explicit that a token's relevance ends with its window, not its run,
\* and ReapExpiredConcurrencyKeys' rate-token DELETE checks only expires_at.
ActiveTokens(t) == {r \in Runs : r[1] = t /\ rateTokenRemaining[r] > 0}

\* Every step's tick: each active countdown drops by one, floored at 0. Runs
\* the same on every action (Claim's admit branch overrides its own claimed
\* run to a fresh RatePeriod afterward), which is what makes one step here
\* the same unit of time the old `clock' = clock + 1` on every action used to
\* be -- just without an unbounded variable behind it.
DecrementTokens == [r \in Runs |-> IF rateTokenRemaining[r] > 0 THEN rateTokenRemaining[r] - 1 ELSE 0]

\* The three-conjunct gate acquireCandidateConcurrencyKey checks under the
\* queues row lock, for a FRESH admission (see the file header for why this
\* model has no "move" case).
CanAdmit(t, w) ==
    /\ Cardinality(LiveHolders(t)) < ConcurrencyLimit
    /\ Cardinality(WorkerLiveHolders(t, w)) < WorkerCap
    /\ Cardinality(ActiveTokens(t)) < RateLimit

\* Rotation order: 1, 2, ..., NumTenants, 1, ... -- ListTenantIDs' ORDER BY,
\* made cyclic by claimRotating's own modulo cursor arithmetic. NextTenant(0)
\* gives 1 with no special case, since 0 is never a member of Tenants.
NextTenant(c) == IF c = NumTenants THEN 1 ELSE c + 1

\* =============================================================================
\* ACTIONS
\* =============================================================================

(*
  --- CLAIM: worker w visits the tenant immediately after its own rotation
      cursor and, if that tenant has ready work AND the queue's three gates
      all admit, claims exactly one of its ready runs.

  Maps to rotating_claim.go's claimRotating, collapsed to one tenant per step
  rather than up to claimTenantsPerTick with a per-tenant share: the real
  per-tick batching is a THROUGHPUT optimisation (poll fewer tenants that
  turned out idle), not a FAIRNESS mechanism -- the fairness comes from the
  cursor advancing past every tenant it visits "VISITED, not returned work"
  (rotating_claim.go's own comment on tenantRotation.advance), which this
  action preserves exactly: rotCursor advances to the chosen tenant whether
  or not a claim happens.
*)
Claim(w) ==
    LET t == NextTenant(rotCursor[w])
        ready == TenantReady(t)
    IN
        /\ rotCursor' = [rotCursor EXCEPT ![w] = t]
        /\ IF ready /= {} /\ CanAdmit(t, w)
           THEN \E r \in ready :
                /\ runStatus' = [runStatus EXCEPT ![r] = "running"]
                /\ queueHolder' = [queueHolder EXCEPT ![r] = w]
                /\ rateTokenRemaining' = [DecrementTokens EXCEPT ![r] = RatePeriod]
           ELSE /\ UNCHANGED <<runStatus, queueHolder>>
                /\ rateTokenRemaining' = DecrementTokens

(*
  --- BEGIN DEFER PHASE: a running run enters its two-phase shutdown. Still
      holds its slot -- engine/status_vocabulary.go's own point about
      'terminating'.

  Maps to engine/defer_phase.go's transition into statusTerminating (not
  itself in this model's "Implemented by" list -- it writes no queue_holders
  or queue_rate_tokens row, only workflow_instances.status, which is exactly
  what this action represents).
*)
BeginDeferPhase(r) ==
    /\ runStatus[r] = "running"
    /\ runStatus' = [runStatus EXCEPT ![r] = "terminating"]
    /\ rateTokenRemaining' = DecrementTokens
    /\ UNCHANGED <<queueHolder, rotCursor>>

(*
  --- SETTLE AND RELEASE: the common shape of finalize, fail, DLQ, terminate,
      cancel, defer-phase expiry and parent-close (cleat#1976's notifyTerminal,
      cleat#1978's parent_close terminate) -- the run reaches a terminal
      status and its slot is freed in the same step.

  Maps to ReleaseWorkflowConcurrencyKeys' DELETE FROM queue_holders, called
  by the worker-side settle path alongside (not instead of) the terminal
  status write.
*)
SettleAndRelease(r) ==
    /\ runStatus[r] \in {"running", "terminating"}
    /\ \E s \in TerminalStatuses :
        runStatus' = [runStatus EXCEPT ![r] = s]
    /\ queueHolder' = [queueHolder EXCEPT ![r] = NULL]
    /\ rateTokenRemaining' = DecrementTokens
    /\ UNCHANGED rotCursor

(*
  --- SETTLE WITHOUT RELEASE: the terminal write lands but the release call
      never runs or never completes -- a crash between the two, or a release
      that itself fails. The slot is stranded until Reap frees it.

  Maps to the fixture engine/queue_holder_run_liveness_test.go's
  forceWorkflowTerminalWithoutRelease builds: a terminal status with the
  queue_holders row deliberately left in place, which is the exact case
  TestTheReaperFreesATerminalRegisteredQueueHolderWithoutWaitingForExpiry
  exercises.
*)
SettleWithoutRelease(r) ==
    /\ runStatus[r] \in {"running", "terminating"}
    /\ queueHolder[r] # NULL
    /\ \E s \in TerminalStatuses :
        runStatus' = [runStatus EXCEPT ![r] = s]
    /\ rateTokenRemaining' = DecrementTokens
    /\ UNCHANGED <<queueHolder, rotCursor>>

(*
  --- REAP: frees a stranded holder -- a queue_holders row whose run has
      already gone terminal (cleat#1965's run-state rule). The idle-sweep
      disjunct always keeps Reap enabled so WF(Reap) can force it to run
      when nothing is stale, matching CleatClaim.tla's own Reap shape.

      Reap advances rateTokenRemaining the same way every other action does
      (DecrementTokens, the countdown tick -- see "Why there is no clock
      variable" above), but does nothing ELSE to a run's token: it never
      resets or clears one. That mirrors ReapExpiredConcurrencyKeys, whose
      rate-token DELETE checks only expires_at, never run state -- migration
      097's own comment is explicit that a token's relevance ends with its
      window, unrelated to its run's lifecycle. ActiveTokens already treats
      a token at 0 as inactive on its own, so Reap has nothing to do here
      that CanAdmit does not already do implicitly.
*)
Reap ==
    \/ (\E r \in Runs :
        /\ queueHolder[r] # NULL
        /\ runStatus[r] \in TerminalStatuses
        /\ queueHolder' = [queueHolder EXCEPT ![r] = NULL]
        /\ rateTokenRemaining' = DecrementTokens
        /\ UNCHANGED <<runStatus, rotCursor>>)
    \/ (/\ rateTokenRemaining' = DecrementTokens
        /\ UNCHANGED <<runStatus, queueHolder, rotCursor>>)

\* =============================================================================
\* NEXT-STATE RELATION
\* =============================================================================

Next ==
    \/ (\E w \in Workers : Claim(w))
    \/ (\E r \in Runs : BeginDeferPhase(r))
    \/ (\E r \in Runs : SettleAndRelease(r))
    \/ (\E r \in Runs : SettleWithoutRelease(r))
    \/ Reap

\* =============================================================================
\* INITIAL STATE
\* =============================================================================

Init ==
    /\ runStatus = [r \in Runs |-> "ready"]
    /\ queueHolder = [r \in Runs |-> NULL]
    /\ rateTokenRemaining = [r \in Runs |-> 0]
    /\ rotCursor = [w \in Workers |-> 0]

\* =============================================================================
\* FAIRNESS
\* =============================================================================

(*
  1. WF on Claim(w) per worker. Claim(w) is a total function of state (it
     always has a next rotCursor value and, at worst, a no-op admission
     branch), so it is continuously enabled and WF forces infinitely many
     Claim(w) steps -- which is what drives rotCursor[w] through every tenant
     in cyclic order infinitely often, independent of the other workers.

  2. WF on the union of a run's own transitions (defer-phase entry, settle
     with or without release). Without this, a run could sit in "running"
     forever, and CanAdmit's antecedent in L2 would then hold vacuously --
     the property would be true for the wrong reason.

  3. WF on Reap, so a stranded holder does not wait forever, the same
     argument CleatClaim.tla's own Reap fairness gives for ReapProgress.
*)
Fairness ==
    /\ \A w \in Workers : WF_vars(Claim(w))
    /\ \A r \in Runs : WF_vars(BeginDeferPhase(r) \/ SettleAndRelease(r) \/ SettleWithoutRelease(r))
    /\ WF_vars(Reap)

\* =============================================================================
\* COMPLETE SPECIFICATION
\* =============================================================================

Spec == Init /\ [][Next]_vars /\ Fairness

\* =============================================================================
\* SAFETY INVARIANTS
\* =============================================================================

(*
  S1: a queue never has more holders than its declared limit -- both at the
  queue-wide grain (ConcurrencyLimit) and, since cleat#1917, at the
  per-worker grain within it (WorkerCap). Both are the same kind of
  statement -- a declared capacity never exceeded -- checked under the same
  queues-row lock in acquireCandidateConcurrencyKey, so one named invariant
  states both conjuncts.

  Go test: TestARegisteredQueueAdmitsAtMostItsLimit and
  TestARegisteredQueueLimitHoldsUnderConcurrentClaims (engine/queue_limit_claim_test.go)
  for the queue-wide half; TestARegisteredQueueWorkerConcurrencyAdmitsAtMostItsLimit
  and TestAParkedRunStillCountsAgainstItsWorkersConcurrency
  (engine/queue_worker_concurrency_claim_test.go) for the per-worker half.
*)
S1 ==
    /\ \A t \in Tenants : Cardinality(LiveHolders(t)) <= ConcurrencyLimit
    /\ \A t \in Tenants, w \in Workers : Cardinality(WorkerLiveHolders(t, w)) <= WorkerCap

(*
  S3: rate tokens never admit more than RateLimit per window, checked under
  the same lock as S1 and independent of it (a full rate window can refuse a
  candidate with a free concurrency slot, and vice versa -- cleat#1918's
  design explicitly calls this "composition: independent of concurrency_limit").

  Go test: TestARegisteredQueueRateLimitAdmitsAtMostItsLimit
  (engine/queue_rate_limit_claim_test.go).
*)
S3 == \A t \in Tenants : Cardinality(ActiveTokens(t)) <= RateLimit

Safety == TypeOK /\ RunningRunsHoldSlots /\ S1 /\ S3

\* =============================================================================
\* LIVENESS (TEMPORAL PROPERTIES)
\* =============================================================================

(*
  S2: every settle path releases the run's slot. Stated as a temporal
  property, not a state invariant, because SettleWithoutRelease deliberately
  strands a holder for one or more steps -- the real system tolerates exactly
  this (a crash between the terminal write and the release call), and what
  actually protects S1 during that window is LiveHolders' run-state filter,
  not the row being physically absent. What must still be true is that the
  stranding is never permanent: Reap eventually clears it.

  Go test: TestTheReaperFreesATerminalRegisteredQueueHolderWithoutWaitingForExpiry
  (engine/queue_holder_run_liveness_test.go) -- it forces exactly the
  SettleWithoutRelease case (forceWorkflowTerminalWithoutRelease) and asserts
  the reap clears it without waiting for expires_at.
*)
S2 ==
    \A r \in Runs :
        []( (runStatus[r] \in TerminalStatuses /\ queueHolder[r] # NULL)
            => <>(queueHolder[r] = NULL) )

(*
  L1: no tenant starves -- a tenant with continuously ready work is
  eventually served (some run of it reaches "running"), regardless of how
  much ready work other tenants have. This is the ROTATION's property, not
  the queue's: since S1/S3 are scoped per tenant (queues.concurrency_limit is
  per (tenant_id, name)), a busy tenant can never exhaust another tenant's
  capacity -- the only way one tenant could crowd out another is by
  monopolising claim ATTENTION, which is exactly what
  rotCursor's per-worker, always-advances-on-visit design prevents.

  This is the weaker (existential) form, mirroring CleatClaim.tla's own
  ClaimProgress next to its stronger NoStarvation -- see L2 below for the
  per-run form.

  Go test: TestTheRotatingClaimDoesNotLetABacklogTakeTheBatch and
  TestTheRotationResumesWhereItStopped
  (cmd/cleat-worker/the_rotating_claim_shares_the_batch_test.go).
*)
L1 ==
    \A t \in Tenants :
        []( TenantReady(t) /= {} => <>(\E r \in Runs : r[1] = t /\ runStatus[r] = "running") )

(*
  L2: a run waiting only on a queue slot is eventually admitted once a slot
  frees. The per-run, universal form -- every individual ready run, not just
  "some run of its tenant", eventually leaves "ready". Mirrors CleatClaim.tla's
  NoStarvation exactly, and for the same reason that property's own comment
  gives: bounded runs and workers under weak fairness mean a specific ready
  run cannot be skipped forever, because eventually the others have all run
  to completion (or are themselves stuck only behind THIS run's own
  tenant/worker/rate gates, which S1/S3 bound) and it becomes the only
  remaining ready work its tenant's rotation turn can serve.

  Go test: TestTheReaperFreesATerminalRegisteredQueueHolderWithoutWaitingForExpiry
  (engine/queue_holder_run_liveness_test.go) -- its final assertion
  ("The freed slot is usable: a second run now claims cleanly") is exactly
  this property's single-step case: a slot frees, and a waiting run is then
  admitted.
*)
L2 ==
    \A r \in Runs :
        [](runStatus[r] = "ready" => <>(runStatus[r] /= "ready"))

============================================================================
