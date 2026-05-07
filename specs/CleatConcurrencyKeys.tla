-------------------------- MODULE CleatConcurrencyKeys --------------------------
\* Cleat Concurrency Key Protocol
\*
\* Models Restate's single-writer-per-key advisory locking pattern using
\* PostgreSQL ON CONFLICT DO NOTHING, as implemented in:
\*   internal/host/db.go lines 1220-1275
\*
\* The protocol provides key-level mutual exclusion for workflow execution.
\* Each key can be held by at most one workflow at any time. Keys have a
\* TTL-based lease that prevents indefinite holds.
\*
\* CONSTANTS:
\*   Workers - set of workflow identifiers
\*   Keys    - set of concurrency key texts
\*   TTL     - lease duration in integer time units (must be > 0)
\*
\* To run in TLC:
\*   1.  Set Workers to a small set like {w1, w2}
\*   2.  Set Keys to a small set like {k1, k2}
\*   3.  Set TTL to a small integer like 3
\*   4.  Add TypeInvariant and MutualExclusion as state invariants
\*   5.  Add EventualRelease, ReapProgress, EventualAcquisition as
\*       temporal properties to check
\*   6.  Select the Spec definition (already includes weak fairness)
\*
\* See also: https://docs.restate.com/operate/concurrency

EXTENDS Naturals, TLC, FiniteSets

CONSTANTS
    Workers,
    Keys,
    TTL

ASSUME
    ∧ Workers ≠ {}
    ∧ Keys ≠ {}
    ∧ TTL ∈ Nat \ {0}

\* Sentinel value meaning "no workflow holds this key".
\* Chosen outside Workers so it can never collide with a real workflow id.
NONE ≜ CHOOSE x : x ∉ Workers

VARIABLES
    held,        \* [k ∈ Keys -> Workers ∪ {NONE}]
    expiresAt,   \* [k ∈ Keys -> Nat]  absolute time when the lease expires
    now          \* Nat  monotonically increasing global clock

vars ≜ ⟨held, expiresAt, now⟩

\* ─── Type Invariant ─────────────────────────────────────────────────────

TypeOK ≜
    ∧ held ∈ [Keys → Workers ∪ {NONE}]
    ∧ expiresAt ∈ [Keys → Nat]
    ∧ now ∈ Nat

\* ─── Init ───────────────────────────────────────────────────────────────

Init ≜
    ∧ held = [k ∈ Keys ↦ NONE]
    ∧ expiresAt = [k ∈ Keys ↦ 0]
    ∧ now = 0

\* ─── Helpers ────────────────────────────────────────────────────────────

\* A key is expired if it is currently held AND its expiration has passed
\* the current time.
Expired(k) ≜ (held[k] ≠ NONE) ∧ (expiresAt[k] < now)

\* ─── Actions ────────────────────────────────────────────────────────────

\* Acquire key k for worker w.
\*
\* Models the atomic SQL transaction (db.go:1222-1246, five lines of Go):
\*
\*   DELETE FROM concurrency_keys
\*    WHERE key_hash = digest($1, 'sha256') AND expires_at < now();
\*
\*   INSERT INTO concurrency_keys (key_hash, key_text, workflow_id, expires_at)
\*   VALUES (digest($1, 'sha256'), $1, $2, now() + $3::interval)
\*   ON CONFLICT (key_hash) DO NOTHING
\*   RETURNING workflow_id;
\*
\* Step 1 (DELETE) removes any expired lease for this key hash.
\* Step 2 (INSERT ON CONFLICT) takes the lease iff the key is now free.
\* Both steps execute atomically in one database transaction.
\*
\* If RETURNING returns a row  -> acquire succeeded.
\* If RETURNING returns nothing -> key held by another workflow; acquire fails.
Acquire(w, k) ≜
    ∧ w ∈ Workers
    ∧ k ∈ Keys
    \* Key is free (unheld) or expired after step 1's cleanup -> acquire succeeds.
    ∧ LET canAcquire ≜ (held[k] = NONE) ∨ Expired(k)
    IN
    IF canAcquire THEN
        ∧ held' = [held EXCEPT ![k] = w]
        ∧ expiresAt' = [expiresAt EXCEPT ![k] = now + TTL]
    ELSE
        ∧ UNCHANGED ⟨held, expiresAt⟩
    ∧ UNCHANGED now

\* Release key k.
\*
\* Models (db.go:1249-1255):
\*   DELETE FROM concurrency_keys WHERE key_hash = digest($1, 'sha256');
\*
\* Any workflow can release any key — the SQL does not filter by holder.
\* Releasing an already-free key is a harmless no-op (DELETE matches zero rows).
Release(k) ≜
    ∧ k ∈ Keys
    ∧ held' = [held EXCEPT ![k] = NONE]
    ∧ expiresAt' = [expiresAt EXCEPT ![k] = 0]
    ∧ UNCHANGED now

\* Release all keys held by worker w.
\* Called when a workflow completes or fails (best-effort bulk cleanup).
\*
\* Models (db.go:1257-1267):
\*   DELETE FROM concurrency_keys WHERE workflow_id = $1;
ReleaseAll(w) ≜
    ∧ w ∈ Workers
    ∧ held' = [k ∈ Keys ↦ IF held[k] = w THEN NONE ELSE held[k]]
    ∧ expiresAt' = [k ∈ Keys ↦ IF held[k] = w THEN 0 ELSE expiresAt[k]]
    ∧ UNCHANGED now

\* Reap all expired keys (background janitor loop).
\*
\* Models (db.go:1269-1275):
\*   DELETE FROM concurrency_keys WHERE expires_at < now();
\*
\* Runs periodically every 60 seconds in production.  In the model this is
\* just another action subject to weak fairness so that liveness holds.
ReapExpired ≜
    ∧ held' = [k ∈ Keys ↦ IF Expired(k) THEN NONE ELSE held[k]]
    ∧ expiresAt' = [k ∈ Keys ↦ IF Expired(k) THEN 0 ELSE expiresAt[k]]
    ∧ UNCHANGED now

\* Advance the global clock by one tick.
\*
\* Time is discrete and monotonic.  The TTL lease duration is measured in
\* the same units.  Without AdvanceTime, expiry-based liveness would never
\* trigger; weak fairness on AdvanceTime forces time to keep moving.
AdvanceTime ≜
    ∧ now' = now + 1
    ∧ UNCHANGED ⟨held, expiresAt⟩

\* ─── Next-state Relation ────────────────────────────────────────────────

Next ≜
    ∨ ∃ w ∈ Workers, k ∈ Keys : Acquire(w, k)
    ∨ ∃ k ∈ Keys : Release(k)
    ∨ ∃ w ∈ Workers : ReleaseAll(w)
    ∨ ReapExpired
    ∨ AdvanceTime

\* ─── Fairness ───────────────────────────────────────────────────────────

\* Weak fairness on every action ensures all parts of the system make
\* progress.  Without it the spec allows any action to starve — e.g. time
\* could stall forever, meaning expired leases are never cleaned up.
Fairness ≜
    ∧ ∀ w ∈ Workers, k ∈ Keys : WF_vars(Acquire(w, k))
    ∧ ∀ k ∈ Keys : WF_vars(Release(k))
    ∧ ∀ w ∈ Workers : WF_vars(ReleaseAll(w))
    ∧ WF_vars(ReapExpired)
    ∧ WF_vars(AdvanceTime)

\* ─── Spec ───────────────────────────────────────────────────────────────

Spec ≜ Init ∧ □[Next]vars ∧ Fairness

\* ─── Safety Invariants ──────────────────────────────────────────────────

\* At most one workflow holds any given key at any time.
\*
\* This is the core safety property.  In the SQL it is guaranteed by the
\* ON CONFLICT DO NOTHING on the primary key (key_hash).  In the model it
\* follows from Acquire only transitioning held[k] to a worker w when
\* held[k] = NONE or Expired(k), and every other action only ever sets
\* held[k] to NONE.
MutualExclusion ≜
    ∀ k ∈ Keys :
        Cardinality({w ∈ Workers : held[k] = w}) ≤ 1

\* The held/expiresAt/now triple always respects its type.
TypeInvariant ≜ TypeOK

\* ─── Temporal Properties (Liveness) ─────────────────────────────────────

\* Every held key will eventually be released (by Release, ReleaseAll,
\* ReapExpired, or Acquire cleaning up the expired entry).
\*
\* This holds because:
\*   1. TTL is finite, so every lease eventually expires.
\*   2. AdvanceTime keeps running under weak fairness.
\*   3. ReapExpired (or a subsequent Acquire) then removes the expired
\*      lease.
EventualRelease ≜
    ∀ k ∈ Keys :
        □(held[k] ≠ NONE ⇒ ◆(held[k] = NONE))

\* An expired key is eventually freed.  This is strictly weaker than
\* EventualRelease — it only requires progress once the key has already
\* passed its expiration time.
ReapProgress ≜
    ∀ k ∈ Keys :
        □(Expired(k) ⇒ ◆(held[k] = NONE))

\* A key that is free will eventually be acquired by some worker.
\*
\* This holds because Acquire(w,k) is weakly fair for all workers and keys,
\* so whenever the key is free, eventually some Acquire(w,k) fires and
\* succeeds.
EventualAcquisition ≜
    ∀ k ∈ Keys :
        □(held[k] = NONE ⇒ ◆(∃ w ∈ Workers : held[k] = w))

\* ─── Theorems (documentation — TLC ignores these) ──────────────────────

THEOREM Spec ⇒ □TypeInvariant
THEOREM Spec ⇒ □MutualExclusion

=============================================================================
\* vim: tw=80 ts=4 sw=4 et
