------------------------- MODULE CleatKeyRotation -------------------------
(*
  Cleat tenant-secrets master-key rotation, rolling deploy

  cleat#1991. The owner's condition on that issue was: "Before building: model
  the rolling rotation in TLA+ (#1996). During a deploy, workers hold different
  key sets: {old}, {old,new} and {new}, while `reseal-secrets` runs. Invariant:
  no live worker ever meets a secret whose key_version it cannot open. That
  decides the safe operator sequence, and whether the boot check must refuse a
  worker whose ring lacks a version still present in the table."

  THIS SPEC DESCRIBES CODE THAT DOES NOT EXIST YET. `tenant_secrets.key_version`
  exists on all three dialects and no Go code reads or writes it, and there is
  no key ring and no `reseal-secrets` command. Every mechanism below is a
  DESIGN CHOICE the model is asked to judge, controlled by a CONSTANT so that
  each one can be switched off and the invariant shown to fail without it.
  Nothing here claims current behaviour is correct.

  What is modelled, and the actor list, from the code rather than from memory
  (re-run when this spec is touched):

    git grep -n -E 'PutSecret|GetSecret|SecretStore\b' -- 'engine/*.go' 'cmd/*.go' \
      ':!*_test.go' | grep -v 'engine/tenant_secrets.go'

  WORKERS ONLY READ. Nothing in cleat's request path calls PutSecret
  (engine/tenant_secrets.go says so: "operator-only by convention"); a worker
  resolves `${secret:...}` through GetSecret and never writes. The WRITERS are
  the operator tools -- `cleatctl set-secret` today, `cleatctl reseal-secrets`
  once built -- and each is a SEPARATE PROCESS carrying its OWN key ring, read
  from ITS environment, which is not the workers' and can be stale. They are
  modelled as two rings for that reason: `setRing` may change at any time (a
  different operator, a different laptop), `resealRing` may change only while
  no reseal is in flight, because one process has one environment for its whole
  life. Sharing one ring between them would let the model reconfigure a reseal
  between its read and its write, which no real run can do; the first version
  of this spec did exactly that and its S2 counterexample was an artefact.

  So there are two directions in which an unopenable row can arise, and they
  need different mechanisms:

    - a NEW WORKER meets OLD ROWS      (a worker starts with a ring that lacks a
                                        version a row already carries)
    - a NEW WRITE meets OLD WORKERS    (a tool writes version v while some
                                        worker's ring lacks v)

  Implemented by (no line numbers -- CLAUDE.md's own rule on why a number rots
  faster than the fact it describes; the CI path filter matches on these names):
    - engine/tenant_secrets.go   (SecretStore: the key ring, seal/open by key_version, PutSecret, GetSecret)
    - cmd/cleat-worker/main.go   (loads CLEAT_SECRET_MASTER_KEY and its previous-key sibling at boot)
    - cmd/cleat-worker/setup.go  (the boot check: a row whose key_version no configured key can open)
    - cmd/cleatctl/setsecret.go  (a WRITER, with its own environment's ring)
  and, to be created by cleat#1991, cmd/cleatctl/resealsecrets.go (the second
  writer). It is not listed as a bullet above because the CI path filter is
  generated from files that exist; add it here when it lands.

  Time is not modelled. There is no clock, no expiry, and no liveness
  property. That is deliberate and is the safe choice this week: an unbounded
  clock under a CONSTRAINT made properties vacuous twice (cleat#2034,
  cleat#2040), and a rotation's safety question is about which STATES are
  reachable, not how long anything takes. Every counter below is
  self-clamped by a constant bound rather than by a .cfg CONSTRAINT.

  WHAT EACH CONSTANT MEANS

    BootCheck   A worker that starts with a ring lacking the key_version of any
                row present REFUSES TO SERVE (the process exits). FALSE: it
                serves and fails on the first resolution instead.

    WriteGate   What a writer must establish before writing version v:
                  "none"      nothing.
                  "observed"  the operator looked at the fleet EARLIER
                              (Observe) and saw every SERVING worker able to
                              open v. This is the operator PROCEDURE with no
                              system support -- "deploy, wait for the rollout,
                              then reseal" -- and its check and its write are
                              separate steps.
                  "registry"  the write itself is refused unless EVERY
                              REGISTERED worker (booting or serving) can open
                              v, evaluated atomically with the write. This
                              REQUIRES that a worker publish its ring to the
                              database before it checks the rows, and that the
                              write and the read of the registry be one
                              serialisable transaction. Neither exists today;
                              it is a schema change, which cleat#1991's text
                              said was not needed. That sentence was written
                              before this model.

    ResealCAS   reseal's UPDATE is conditional on the row still being what
                reseal read (same key_version, same ciphertext). FALSE: an
                unconditional write of the re-encrypted OLD plaintext.

  INVARIANTS

    S1_ServingWorkersCanOpenEveryRow   the owner's invariant.
    S2_NoLostWrite                     a value written by set-secret is never
                                       replaced by a staler one. Not about key
                                       versions at all; it is here because
                                       reseal is a read-modify-write on rows a
                                       second writer can touch, and the model
                                       is the cheapest place to say whether
                                       the CAS is optional.

  The shipped .cfg is the design the model supports:
  BootCheck = TRUE, ResealCAS = TRUE, WriteGate = "registry". specs/CleatKeyRotation.md
  records what happens under each single-constant weakening, which is the
  evidence that every mechanism is load-bearing rather than decoration.

  Bounds and what they cannot see. Two rows, two workers, two key versions and
  GenMax plaintext generations: enough for a PARTIALLY RESEALED table (one row
  on each version), for one worker on each ring at once, and for the
  set-secret-during-reseal interleaving (which needs a row to be written twice
  around one snapshot). It does NOT model three or more key versions (a second
  rotation begun before the first finished), tenants, or a worker restarting
  under an unchanged ring; none of those adds a reachable state that a
  two-version, two-row table does not already have, but that is an argument and
  not a measurement.
*)
EXTENDS Naturals, FiniteSets

CONSTANTS
    Workers,     \* worker ids, as numbers: {1, 2}
    Rows,        \* secret rows, as numbers: {1, 2}
    GenMax,      \* how many distinct plaintexts a row may be set to
    BootCheck,   \* BOOLEAN
    ResealCAS,   \* BOOLEAN
    WriteGate    \* "none" | "observed" | "registry"

ASSUME BootCheck \in BOOLEAN /\ ResealCAS \in BOOLEAN
ASSUME WriteGate \in {"none", "observed", "registry"}
ASSUME GenMax \in Nat /\ GenMax >= 2

Versions == {1, 2}
\* The three key sets a worker or a tool can hold. {1} is the deployment before
\* rotation, {1,2} is "new key current, old key previous", {2} is after the old
\* key is retired.
Configs  == {{1}, {1, 2}, {2}}
\* The newest key in a ring is the one that writes.
Cur(cfg) == IF 2 \in cfg THEN 2 ELSE 1

VARIABLES
    wst,       \* worker -> "down" | "booting" | "serving"
    ring,      \* worker -> the key set it was started with
    rver,      \* row -> key_version stored, 0 = no such row
    rgen,      \* row -> which plaintext the stored ciphertext holds
    latest,    \* row -> newest plaintext ever set (ghost; for S2 only)
    setRing,   \* `cleatctl set-secret`'s key set, from ITS environment
    resealRing, \* `cleatctl reseal-secrets`' key set, fixed for one run
    snap,      \* reseal's in-flight read: [row, ver, gen], row = 0 for none
    obsv       \* the version the operator last saw the serving fleet able to open, 0 = never

vars == <<wst, ring, rver, rgen, latest, setRing, resealRing, snap, obsv>>

NoSnap == [row |-> 0, ver |-> 0, gen |-> 0]

TypeOK ==
    /\ wst \in [Workers -> {"down", "booting", "serving"}]
    /\ ring \in [Workers -> Configs]
    /\ rver \in [Rows -> {0} \cup Versions]
    /\ rgen \in [Rows -> 0..GenMax]
    /\ latest \in [Rows -> 0..GenMax]
    /\ setRing \in Configs
    /\ resealRing \in Configs
    /\ snap \in [row: {0} \cup Rows, ver: {0} \cup Versions, gen: 0..GenMax]
    /\ obsv \in {0} \cup Versions

Init ==
    \* Before any rotation: every worker serving under {1}, no secrets yet, both
    \* tools also on {1}. Starting with NO rows is deliberate -- it lets the
    \* model create rows at whatever version the tool then holds, including a
    \* stale one, which a pre-populated table would hide.
    /\ wst = [w \in Workers |-> "serving"]
    /\ ring = [w \in Workers |-> {1}]
    /\ rver = [r \in Rows |-> 0]
    /\ rgen = [r \in Rows |-> 0]
    /\ latest = [r \in Rows |-> 0]
    /\ setRing = {1}
    /\ resealRing = {1}
    /\ snap = NoSnap
    /\ obsv = 0

\* -------------------------------------------------------------- helpers ----

\* A row this ring can read. An absent row (0) is trivially fine.
Openable(cfg, r) == rver[r] = 0 \/ rver[r] \in cfg

\* Registered = has published its ring: booting counts, because it has
\* announced a ring even though it is not yet serving.
Registered(w) == wst[w] /= "down"

FleetCanOpen(v) == \A w \in Workers : Registered(w) => v \in ring[w]

WriteOK(v) ==
    CASE WriteGate = "none"     -> TRUE
      [] WriteGate = "observed" -> obsv = v
      [] WriteGate = "registry" -> FleetCanOpen(v)

\* -------------------------------------------------------------- actions ----

\* A rolling deploy, a rollback, an autoscaler adding a replica from an old
\* image, or the operator retiring the old key: a worker (re)starts under ANY
\* key set. Deliberately unconstrained -- the model must not assume a
\* well-behaved rollout, because the boot check exists for the ill-behaved one.
DeployStart(w, cfg) ==
    /\ wst[w] \in {"serving", "down"}
    /\ wst' = [wst EXCEPT ![w] = "booting"]
    /\ ring' = [ring EXCEPT ![w] = cfg]
    /\ UNCHANGED <<rver, rgen, latest, setRing, resealRing, snap, obsv>>

\* The boot check: read the table under the ring the worker announced. With
\* the check the worker refuses; without it, it serves and meets the row later.
BootFinish(w) ==
    /\ wst[w] = "booting"
    /\ IF BootCheck /\ ~(\A r \in Rows : Openable(ring[w], r))
          THEN wst' = [wst EXCEPT ![w] = "down"]
          ELSE wst' = [wst EXCEPT ![w] = "serving"]
    /\ UNCHANGED <<ring, rver, rgen, latest, setRing, resealRing, snap, obsv>>

Crash(w) ==
    /\ wst[w] = "serving"
    /\ wst' = [wst EXCEPT ![w] = "down"]
    /\ UNCHANGED <<ring, rver, rgen, latest, setRing, resealRing, snap, obsv>>

\* set-secret is run from whichever environment the operator happens to be in --
\* including a stale one (a laptop with last month's env, a runner image not yet
\* updated). Unconstrained, for the same reason as DeployStart.
SetReconfigure(cfg) ==
    /\ setRing' = cfg
    /\ UNCHANGED <<wst, ring, rver, rgen, latest, resealRing, snap, obsv>>

\* A reseal run is one process with one environment, so its ring cannot change
\* between its read and its write. It CAN differ between runs (a rerun after
\* the operator fixed the env), which is what this action is.
ResealReconfigure(cfg) ==
    /\ snap.row = 0
    /\ resealRing' = cfg
    /\ UNCHANGED <<wst, ring, rver, rgen, latest, setRing, snap, obsv>>

\* The operator looks at the rollout status: version v is "safe" if every
\* SERVING worker can open it. A booting worker is not visible in a rollout
\* status yet, and that is exactly the gap this action preserves.
Observe(v) ==
    /\ v \in Versions
    /\ \A w \in Workers : wst[w] = "serving" => v \in ring[w]
    /\ obsv' = v
    /\ UNCHANGED <<wst, ring, rver, rgen, latest, setRing, resealRing, snap>>

Forget ==
    /\ obsv' = 0
    /\ UNCHANGED <<wst, ring, rver, rgen, latest, setRing, resealRing, snap>>

\* `cleatctl set-secret`: a new plaintext for a row, sealed under ITS ring's
\* current key.
SetSecret(r) ==
    LET v == Cur(setRing) IN
    /\ latest[r] < GenMax
    /\ WriteOK(v)
    /\ rver' = [rver EXCEPT ![r] = v]
    /\ rgen' = [rgen EXCEPT ![r] = latest[r] + 1]
    /\ latest' = [latest EXCEPT ![r] = latest[r] + 1]
    /\ UNCHANGED <<wst, ring, setRing, resealRing, snap, obsv>>

\* `cleatctl reseal-secrets`, first half: read one row that is not yet on its
\* ring's current key, and that its ring can open. A row the tool cannot open is
\* NOT skipped silently in the design (it is reported); here it simply cannot
\* be read, which is the same as not being resealed.
ResealRead(r) ==
    /\ snap.row = 0
    /\ rver[r] /= 0
    /\ rver[r] \in resealRing
    /\ rver[r] /= Cur(resealRing)
    /\ snap' = [row |-> r, ver |-> rver[r], gen |-> rgen[r]]
    /\ UNCHANGED <<wst, ring, rver, rgen, latest, setRing, resealRing, obsv>>

\* Second half: write the row back under the current key. What it writes is
\* the plaintext it READ, snap.gen -- which is why an unconditional write
\* loses a set-secret that landed in between.
ResealWrite ==
    LET r == snap.row
        v == Cur(resealRing) IN
    /\ snap.row /= 0
    /\ WriteOK(v)
    /\ ResealCAS => (rver[r] = snap.ver /\ rgen[r] = snap.gen)
    /\ rver' = [rver EXCEPT ![r] = v]
    /\ rgen' = [rgen EXCEPT ![r] = snap.gen]
    /\ snap' = NoSnap
    /\ UNCHANGED <<wst, ring, latest, setRing, resealRing, obsv>>

\* A crash, a Ctrl-C, or a CAS miss followed by a retry: the in-flight read is
\* dropped and nothing was written.
ResealAbort ==
    /\ snap.row /= 0
    /\ snap' = NoSnap
    /\ UNCHANGED <<wst, ring, rver, rgen, latest, setRing, resealRing, obsv>>

Next ==
    \/ \E w \in Workers, cfg \in Configs : DeployStart(w, cfg)
    \/ \E w \in Workers : BootFinish(w)
    \/ \E w \in Workers : Crash(w)
    \/ \E cfg \in Configs : SetReconfigure(cfg)
    \/ \E cfg \in Configs : ResealReconfigure(cfg)
    \/ \E v \in Versions : Observe(v)
    \/ Forget
    \/ \E r \in Rows : SetSecret(r)
    \/ \E r \in Rows : ResealRead(r)
    \/ ResealWrite
    \/ ResealAbort

\* No fairness: this spec claims SAFETY only. See the header for why no
\* liveness property is stated.
Spec == Init /\ [][Next]_vars

\* ------------------------------------------------------------ invariants ----

\* The owner's invariant, checked STATICALLY: a serving worker can be asked to
\* resolve any row at any moment (which tenant's workflow calls it, and when, is
\* not something the worker chooses), so "could open every row" is the right
\* strength, not "has not yet failed to".
S1_ServingWorkersCanOpenEveryRow ==
    \A w \in Workers : wst[w] = "serving" => \A r \in Rows : Openable(ring[w], r)

\* A row's stored plaintext is the newest one anyone set.
S2_NoLostWrite ==
    \A r \in Rows : rver[r] /= 0 => rgen[r] = latest[r]

=============================================================================
