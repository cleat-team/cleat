<!-- tla-index: issue=1991 -->
# CleatKeyRotation.tla — status

Models the rolling rotation of the tenant-secrets master key (cleat#1991): workers holding
different key sets (`{1}`, `{1,2}`, `{2}`) while `cleatctl set-secret` and the not-yet-built
`cleatctl reseal-secrets` write rows. **It describes code that does not exist yet** — the spec
is the design's evidence, and every mechanism in it is a CONSTANT so it can be switched off and
shown to matter. Read the header of the `.tla` for the actor list and the grep that produced it.

Checks two invariants, safety only (no clock, no liveness property — on purpose, see the header):

- **`S1_ServingWorkersCanOpenEveryRow`** — the owner's condition on #1991: no serving worker
  holds a key set that lacks the `key_version` of any row in the table. Checked statically,
  because which tenant's workflow resolves which secret, and when, is not the worker's choice.
- **`S2_NoLostWrite`** — a plaintext written by `set-secret` is never replaced by a staler one.
  Not about key versions; it is here because reseal is a read-modify-write and the model is the
  cheapest place to say whether its compare-and-swap is optional.

`CleatKeyRotation.cfg`: `Workers = {1,2}`, `Rows = {1,2}`, `GenMax = 2`,
`BootCheck = TRUE`, `ResealCAS = TRUE`, `WriteGate = "registry"` — the design the model supports.
Measured 2026-09-23: **61,299 distinct states, 861,280 generated, search depth 16, finished in 4s**;
`TypeOK`, S1 and S2 hold. Re-derive with `make tla`, not by trusting this paragraph.

## What each mechanism is for — the known-positives

Every row below is the shipped `.cfg` with ONE constant weakened, run on a scratch copy and
discarded (none is in the tree). Each fails, on the invariant named, and the trace was READ to
confirm it fails for the reason the mechanism exists — not merely that it went red.

| weakened | fails | trace (states) | what the counterexample is |
|---|---|---|---|
| `BootCheck = FALSE` | S1 | 4 | a row is written at version 1; a worker is deployed straight to `{2}` (the old key retired with a row still on it) and **serves**. The first resolution then fails. |
| `WriteGate = "none"` | S1 | 4 | a worker is mid-deploy on `{1}`; `set-secret` is run from an environment already on `{1,2}` and writes version 2. The worker still on `{1}` now serves a row it cannot open. This is "reseal before the deploy finished". |
| `WriteGate = "observed"` | S1 | 5 | the operator checks the rollout status ("every serving worker can open version 1"), then a worker that was **booting** on `{2}` — invisible in a rollout status — finishes booting, and the write lands. The check and the write are separate steps, so the check goes stale. |
| `ResealCAS = FALSE` | S2 | 8 | `set-secret` writes plaintext 1 at v1; reseal (ring `{1,2}`) reads it; `set-secret` is run again **from an environment not yet updated to `{1,2}`** and writes plaintext 2 at v1; reseal writes back plaintext 1 at v2. The newer value is gone. |
| all three off | S1 | 3 | the shortest of the above. |

**What the table decides.** `BootCheck` and the write gate protect against *opposite* directions
and neither can stand in for the other: a **new worker meeting old rows** is the boot check's; a
**new write meeting old workers** is the write gate's. Dropping either leaves a state in which a
serving worker holds a row it cannot open. And **the compare-and-swap in reseal's UPDATE is not
optional**: without it a concurrent `set-secret` from a stale environment is silently undone.

**The finding that changes the design.** `WriteGate = "observed"` — the plain operator procedure,
"deploy, wait for the rollout, then reseal" — is **not safe**, and the reason is not operator
carelessness: a rollout status cannot show a worker that is still booting, and any check made
before the write can go stale before the write happens. Only `"registry"` holds, and it needs:

1. each worker to **publish its key set to the database before it reads the table** (register,
   then boot-check — the other order has a window in which a write lands between the two);
2. a writer to refuse version `v` unless **every registered worker** can open `v`, evaluated in
   the same serialisable transaction as the write;
3. "registered" to mean live — a worker that crashed without deregistering would otherwise block
   every write until it expires. The model has no clock and does **not** check this.

`admin.workers` already exists on all three dialects (migrations: postgres 076, mysql 065, mssql 069, with a lease
and heartbeat from cleat#1487), so (1) is a column on a table that is already there, not a new
mechanism. It is still a schema change, which the text of #1991 said would not be needed. That
sentence was written before this model.

## What this model does not cover

- **Time and liveness.** No property says a rotation *finishes*, or that a dead worker's
  registration expires. Adding one needs a known-positive first (cleat#2034, cleat#2040: an
  unbounded clock under a `CONSTRAINT` made properties vacuous twice).
- **Three or more key versions**, i.e. a second rotation begun before the first ended.
- **Tenants.** The per-tenant HKDF derivation is orthogonal to which *master* key sealed a row.
- **A worker restarting under an unchanged ring.** It adds no reachable state.
- **Whether the implementation does what the model assumes.** TLA+ verifies the design,
  not the code. The obligation the model places on the implementation is that the two
  spans `WriteGate = "registry"` treats as atomic with respect to each other never
  interleave. See the next section for how the code discharges it, and which tests would
  go red if it stopped.

## Where the model meets the code

The design comment on cleat#1991 proposed "one serialisable transaction" for each span. The
implementation uses **one named database lock** instead (`engine/secret_key_gate.go`), because
the boot check reads one tenant per transaction under row-level security and because
SERIALIZABLE means three different things on the three dialects.

| model | code | refinement |
|---|---|---|
| worker span: `Register` then `BootCheck` | `WorkerRegistry.RegisterUnderKeyGate`: takes the gate **shared**, replaces the row (publishing the key set), runs the check, commits or withdraws | held from the first step through commit |
| writer span: read registry, then write | `SecretStore.gatedWrite`: takes the gate **exclusive**, reads the live key sets, refuses unless every one can open the version, writes on the same connection | held from the read through commit |
| "the spans do not interleave" | shared/exclusive exclusion on one lock name | a worker span and a writer span never overlap; two worker spans may |

**Why that is enough.** With the spans mutually exclusive, only two orders exist. Writer
first: its row is committed before the worker's span starts, so the worker's boot check reads
it (this is the counterexample the model finds for `WriteGate = "observed"`, 5 states, and
`TestAWorkerBootingDuringAWriteSeesTheRowTheWriterWrote` is that trace). Worker first: its
row is committed before the writer's span starts, so the writer's registry read sees it
(`TestAWriteDuringABootSeesTheWorkerThatWasBooting`). The other three counterexamples map to:
`BootCheck = FALSE` to `checkSecretsUsable` and its tests; `WriteGate = "none"` to
`TestAWriteIsRefusedWhileALiveWorkerCannotOpenItsVersion`; `ResealCAS = FALSE` to
`TestResealDoesNotOverwriteAConcurrentSetSecret`.

**Scope of the lock per dialect.** PostgreSQL `pg_advisory_xact_lock[_shared]` and SQL Server
`sp_getapplock` with a Transaction owner are released by COMMIT/ROLLBACK. MySQL `GET_LOCK` is
**session**-scoped, survives COMMIT, and is exclusive-only, so it is taken on a dedicated
connection and released in a defer on every exit; MySQL workers starting together take
turns. `TestAFailedBootCheckDoesNotLeaveTheGateHeld` and `TestAPanickingWriteDoesNotLeaveTheGateHeld`
read the lock's state from each database's own catalog, because on MySQL the connection that
leaked the lock re-acquires it re-entrantly and "the next writer succeeded" proves nothing.

**What the lock does not cover, and the model cannot say.** "Live" is a heartbeat within five
minutes (`SecretKeyLiveWindow`). A worker stalled for longer while still serving is invisible
to a writer; when its membership loop next runs it re-registers, re-checks and stops if it
cannot open something stored (`TestALapsedWorkerThatCannotOpenAStoredSecretStopsInsteadOfServing`),
which bounds the exposure without removing it. Workers older than the registry are invisible
unless they registered under the connection budget, and a registry row with no key set is read
as "opens version 1 only".

## Bounds

Two rows, two workers, two versions: enough for a partially resealed table (one row on each
version), one worker on each key set at once, and a `set-secret` landing between reseal's read
and its write. `GenMax = 2` is the smallest bound at which the last of those is reachable — a
row needs to be set twice around one snapshot.
