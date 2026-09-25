# Rotate the payload encryption key

`--encryption-key-file` (with `--encrypt-sensitive-payloads`) encrypts
`event_history`'s sensitive columns (`engine.EncryptedEventColumns` --
workflow input, output and error, plugin call arguments and results) at
rest, per tenant, on PostgreSQL. This is a **different key from the
per-tenant secrets master key** ([`use-secrets.md`](use-secrets.md),
`CLEAT_SECRET_MASTER_KEY`) -- different table, different threat model
(workflow data your own tenants generated, not third-party credentials they
gave you), different flag.

The same key also backs `plugin.Payloads`, the interface a plugin uses to
seal a value that does not fit `Secrets`' shape -- high-churn, no fixed
name, no operator-set-it-once step. Nothing in the shipped plugin set uses
it today: `oauthprovider` was the first and only caller, and cleat#2295/
cleat#2296 removed it (a nil key with no `--encryption-key-file` set made
every login fail closed, since `Payloads.Seal` refuses rather than falling
back to plaintext -- and nothing read the sealed columns back, so the fix
was to stop sealing them rather than to require the flag). The mechanism
stays, documented here, for the next plugin that needs it -- everything
below applies to it identically, because it is the same `PayloadEncryption`
underneath.

## What rotating requires

`--encryption-key-file-previous` (cleat#1992 part 2, #2308) lets a worker
hold two keys at once: `--encryption-key-file` is the current key (every
new seal uses it), `--encryption-key-file-previous` is a previous one,
read-only -- `engine.OpenAndClassify` tries it after the current key fails,
and reports a value opened that way as `PayloadFormPreviousKey` so a sweep
knows it still needs converting. `engine.PayloadEncryption` has supported
this ring shape since before the flag existed (`engine.NewKeyRing`,
`engine.NewPayloadEncryptionWithRing` -- `cleatctl reseal-payloads` was
already built on it); the worker just did not expose it.

That makes a **rolling** rotation possible, but only if the fleet moves
through it in the right order -- see "Rolling rotation" below. If you would
rather not manage that sequencing, or you are rotating before every worker
in the fleet is confirmed on a build that carries `--encryption-key-file-previous`,
the **stop-the-world** procedure is still the simpler and safer choice:

1. Stop every worker that has `--encrypt-sensitive-payloads` set.
2. Generate the new key the same way the first one was made:
   `head -c 32 /dev/urandom | base64`.
3. Run the sweep with both keys, current and previous, while nothing is
   writing:

       cleatctl reseal-payloads --db "$DSN" \
         --encryption-key-file /path/to/new.key \
         --from-key-file /path/to/old.key

   `--dry-run` first if you want a report before it writes anything. The
   sweep is online-safe against readers in the sense that it verifies each
   re-seal before writing it and is safe to interrupt and re-run -- the
   stop-the-world requirement here is about the **worker**, which cannot
   yet hold two keys at once, not about the sweep itself.
4. Confirm it converged: the command's own output reports `unreadable: 0`,
   and its exit status is non-zero while any row is still on the old form
   (`cmd/cleatctl/resealpayloads.go`'s own doc comment: "Exit status is
   non-zero if anything was left unconverted, so this can be run to
   completion in a loop and its exit code trusted").
5. Restart every worker with `--encryption-key-file` pointing at the new
   key. `--encryption-key-file-previous` (or its equivalent) is not needed
   at this step, because step 3 already moved every row.

**If you cannot accept the downtime in step 1**, use the rolling procedure
below instead -- but read the caveat in its step 0 first. It trades the
stop-the-world window for a sequencing requirement, not for a free lunch.

## Rolling rotation

**The single-phase version of this -- roll every worker straight from
"key A only" to "B current, A previous", in any order -- is broken, and
used to be documented here as safe.** cleat-review measured it on a real
worker (tests/crash, #2308): while the rollout is in progress, a worker
still on the old flags (key A only, no previous) can pick up or replay a
run whose event was already sealed under B by a worker that had already
rolled. The A-only worker has no way to open a B-sealed field, and what
happens next is worse than "an error" -- see cleat#2311, still open at
this writing: with checksum verification on (the default), the run ends
**FAILED permanently**, not released for a worker that does hold the key;
with `--disable-checksum-verification`, it ends **DONE**, with
`[DECRYPTION_FAILED]` silently persisted into `workflow_instances.result`
in place of the real value. Neither is "fails closed, no stale data" --
that was this doc's old claim, and it was wrong.

The fix is to never let a worker that cannot open a B-sealed field coexist
with an already-B-sealed field. Do that by rolling every worker to a
**read-only** stage first, so the whole fleet can already decrypt B before
any worker starts writing it:

0. **This procedure does not close cleat#2311; it only avoids triggering
   it.** It works if every worker's phase-1 rollout genuinely completes,
   fleet-wide, before phase 2 starts. If your rollout tooling cannot
   guarantee that ordering -- a canary that might get bypassed, a worker
   that might restart on stale flags mid-rollout -- prefer the
   stop-the-world procedure above; #2311 is what a gap in this sequencing
   turns into.
1. Generate key B.
2. **Phase 1 -- read-only rollout.** Roll every worker to
   `--encryption-key-file A --encryption-key-file-previous B`. Current stays
   A: nothing is written under B yet, nothing on any table is sealed under
   B yet, so there is nothing for an unrolled worker to fail to decrypt.
   This phase exists purely to get B loaded, as a previous key, onto every
   worker in the fleet before step 3 begins.
3. **Confirm phase 1 is complete fleet-wide** before proceeding -- however
   your fleet tracks its own rollout; there is no `admin.workers`-style
   registry query for this the way tenant-secret rotation has. Do not start
   phase 2 against a fleet you cannot confirm has finished phase 1.
4. **Phase 2 -- flip to current.** Roll every worker to
   `--encryption-key-file B --encryption-key-file-previous A`. Because
   every worker already has B loaded (from phase 1) before any of them
   begins sealing new fields with it, a worker that has not yet reached
   phase 2 can still open a B-sealed field -- it is running phase 1's
   flags, current=A/previous=B, and previous is exactly what serves that
   read. This is the guarantee `docs/how-to/use-secrets.md`'s tenant-secret
   rotation gives, applied here by construction rather than by an
   `admin.workers` gate; it is proven at the ring level by
   `engine/payload_key_rotation_test.go`, and the two-phase sequence
   end-to-end by `tests/crash/payload_encryption_rotation_test.go`,
   `TestPayloadEncryptionKeyRotationWiresIntoARealWorker`.
5. Once every worker reports phase-2 flags, run
   `cleatctl reseal-payloads --encryption-key-file B --from-key-file A`
   to move every remaining A-sealed row onto B. Until this runs, A remains
   load-bearing: a row written before phase 2 began is still sealed under
   it, openable only because A is still in the ring as previous.
6. Once the sweep reports `unreadable: 0` and nothing older than the
   current form, drop `--encryption-key-file-previous` from every worker's
   flags on the next restart. **A test proving this final step** -- two
   independently-configured `PayloadEncryption` values sharing one live
   database, the sweep, and the drop leaving everything readable -- is
   `cmd/cleatctl/reseal_payloads_two_workers_test.go`,
   `TestPayloadKeyRotationAcrossTwoWorkers`.

## What is not covered, either way

- **An external KMS.** The key is supplied directly, from a file.
- **A plugin's own `Payloads`-sealed value, once one exists.**
  `cleatctl reseal-payloads` only rewrites `event_history`'s own encrypted
  columns (`engine.EncryptedEventColumns`) -- see `plugin.Payloads`'s own
  doc comment. A plugin storing a long-lived sealed value elsewhere is
  responsible for its own re-seal on rotation, the same way it is
  responsible for its own storage.
- **PostgreSQL only.** `--encrypt-sensitive-payloads` is refused unless
  `--driver=postgres`; the encrypting write path is Postgres-specific SQL.

## See also

- [`use-secrets.md`](use-secrets.md) -- the tenant-secrets master key, a
  different key with an already-shipped online rotation and an
  `admin.workers` gate this mechanism does not (yet) have.
- `cmd/cleatctl/resealpayloads.go` -- the sweep's own doc comment, for why
  it exists and why it cannot be a numbered migration.
- `engine/encryption.go` -- `PayloadEncryption`, `KeyRing`,
  `PayloadFormPreviousKey`.
- `tests/crash/payload_encryption_rotation_test.go` -- a real-worker crash
  test pinning the CLI wiring: a worker given only
  `--encryption-key-file-previous` refuses to start, and a worker crashed
  mid-flight under key A then restarted under key B with A as previous
  decrypts A's in-flight event and completes the workflow.
- cleat#2311 -- open, pre-existing on develop, not fixed by this doc or by
  #2308: a decrypt failure on replay is swallowed rather than returned as
  an error, which is what makes an incomplete phase-1 rollout above
  dangerous rather than merely inconvenient.
