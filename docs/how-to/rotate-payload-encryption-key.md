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

## What rotating today actually requires

**There is no online rotation yet.** `cmd/cleat-worker` builds exactly one
key from `--encryption-key-file` (`engine.NewPayloadEncryption`, a
single-key ring) -- there is no `--encryption-key-file-previous` flag for it
to read a second one from. `engine.PayloadEncryption` itself already
supports a ring with a current key and a previous one (`engine.NewKeyRing`,
`engine.NewPayloadEncryptionWithRing`) -- `cleatctl reseal-payloads` is
built on exactly that -- but the worker never constructs one. Until
`--encryption-key-file-previous` ships (cleat#1992 part 2, tracked
separately from the tenant-secrets rotation this doc's sibling describes),
a worker holding the old key cannot read a row written under a new one, and
a worker holding the new key cannot read a row written under the old one
without also being given it.

So today, rotating means a **stop-the-world** window, not a rolling
restart:

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

**If you cannot accept the downtime in step 1**, the honest alternative
today is: don't rotate yet. There is no supported way to keep workers
serving encrypted-payload traffic across a key change until the flag in the
next section exists.

## Once `--encryption-key-file-previous` ships

This section describes the target shape, not shipped behavior -- check
`cmd/cleat-worker --help` for `--encryption-key-file-previous` before
following it; if the flag is not listed, follow the stop-the-world
procedure above instead.

The mechanism is the ring `cleatctl reseal-payloads` already uses,
extended to the worker: `--encryption-key-file` is the current key (every
new seal uses it), `--encryption-key-file-previous` is a previous one
(read-only -- `engine.OpenAndClassify` tries it after the current key
fails, and reports a value opened that way as `PayloadFormPreviousKey` so a
sweep knows it still needs converting).

To move from key A to key B with no downtime:

1. Generate key B.
2. Roll out every worker with `--encryption-key-file B
   --encryption-key-file-previous A`. Order does not matter **for
   readability**: a worker on the old flags (key A only) and a worker
   already on the new ones (B current, A previous) can run at the same
   time, and the already-rolled worker reads what the not-yet-rolled one
   wrote, via the previous-key fallback. This is the same guarantee
   `docs/how-to/use-secrets.md`'s tenant-secret rotation gives, and it is
   proven at the ring level (not yet at the worker-flag level, since the
   flag does not exist to test) by `engine/payload_key_rotation_test.go`.
3. **Unlike tenant-secret rotation, there is no `admin.workers`-style gate
   for this.** `reseal-secrets` refuses to write at a version some live
   worker cannot open, and names the worker; nothing here does that check.
   The asymmetric half of the guarantee is real and worth stating plainly:
   a worker still on key A alone **cannot** read a row a worker already on
   B wrote, until it restarts. If some other worker or tool (a replay, a
   debug read, a worker that fails and comes back up still on the old
   flags) touches that row before every worker has rolled, it gets an
   error, not stale data -- `PayloadEncryption` fails closed on a key it
   was not given, it does not silently skip the field. Roll the fleet
   promptly; do not leave a mixed fleet running for an extended period.
4. Once every worker reports the new flags (however that fleet tracks its
   own rollout -- there is no registry query for it the way
   `admin.workers` answers for tenant-secret versions), run
   `cleatctl reseal-payloads --encryption-key-file B --from-key-file A`
   to move every row off A. Until this runs, A remains load-bearing: a row
   written before the rollout is still sealed under it, openable only
   because A is still in the ring as previous.
5. Once the sweep reports `unreadable: 0` and nothing older than the
   current form, drop `--encryption-key-file-previous` from every worker's
   flags on the next restart. **A test proving this specific sequence** --
   two independently-configured `PayloadEncryption` values sharing one
   live database, the gap in step 3, the sweep in step 4, and the drop in
   step 5 leaving everything readable -- is
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
