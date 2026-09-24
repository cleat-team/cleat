# Deployment-wide secrets

Plugin credentials that belong to the whole deployment rather than to any one
tenant — an S3 key pair, a SendGrid key, an LLM provider's API key — stored
encrypted, with no tenant dimension at all, and read by a plugin **per use**
rather than cached once at worker startup. That is the property this adds:
rotating one of these no longer needs every worker restarted.

**This is a different table from [per-tenant secrets](use-secrets.md)**, not
an optional tenant column on the same one (owner decision, cleat#1992). There
is nothing here for a row-level policy to key on, so `deployment_secrets`
carries none.

## What is covered today, and what is not yet

Converted to read from here, live, on every call:

| plugin | name |
|---|---|
| `email-notify` | `email.sendgrid_api_key` |
| `llm` | `llm.providers.<provider>.api_key`, one per **enabled**, non-`ollama` provider |

Both refuse to start the worker if their required name is missing or cannot
be opened — see "Fail-closed at boot" below.

**Not yet converted**, and still read from `--plugin-config` at `Init` the way
every plugin's credentials used to be: `blobstore` (its S3 key pair),
`scheduledbackup` (its backup-target DSN), `slacknotify` (its request-signing
secret). Each is tracked as a checklist item on cleat#1992. Do not write
`blobstore.access_key_id`, `scheduledbackup.dsn` or `slacknotify.signing_secret`
here yet — nothing reads them from this table until that plugin's own
conversion lands, and the fixed-name list above is the one actually checked by
`checkRequiredDeploymentSecrets` at boot.

## Set up a master key, once

**The same key ring [per-tenant secrets](use-secrets.md) uses** —
`CLEAT_SECRET_MASTER_KEY` and its `_PREVIOUS` pair — not a second one to
generate and roll out separately. Sharing the ring is safe because sealing
here is domain-separated from tenant secrets: a distinct HKDF info string, and
each ciphertext is authenticated against its own `name` rather than a tenant
id, so a row copied between the two tables — or between two names in this one
— fails to open rather than decrypting to something
(`engine/deployment_secrets.go`, and
`engine/a_deployment_secret_domain_is_separate_test.go`).

## Write a secret

```sh
head -c 32 /dev/urandom | base64          # once, if there is no ring yet
printf %s "$SENDGRID_KEY" | cleatctl --db "$DSN" set-deployment-secret --name email.sendgrid_api_key
```

The value is read from **stdin**, or `--from-file` — never a flag, for the
same reason `set-secret`'s value is not one. Writes are **operator-only**:
migration 103 (PostgreSQL) additionally revokes `cleat_app`'s INSERT/UPDATE/
DELETE on this table, so a worker's own database role cannot write here even
if something reachable through it tried. MySQL and SQL Server have no
equivalent role split to revoke from; encryption at rest is what protects the
table on those two.

## Retire a secret

```sh
cleatctl --db "$DSN" retire-deployment-secret --name email.sendgrid_api_key
```

Stops it resolving — the next `Get` fails exactly like a name that was never
set. Needs no master key: retiring only sets `disabled_at`, never touches the
ciphertext, so cutting off a leaked credential during an incident is not
blocked on the key being available. Reversible: run `set-deployment-secret`
again for the same name.

## Rotate the master key

```sh
cleatctl --db "$DSN" reseal-deployment-secrets --dry-run
cleatctl --db "$DSN" reseal-deployment-secrets
```

Same shape as `reseal-secrets`, minus the per-tenant loop there is no tenant
to have: reads and verifies every row, writes nothing on `--dry-run`, and its
write is conditional on the row still being what it read. Exits non-zero
while anything is left — a row this ring cannot open, one that changed while
the sweep ran, or (on `--dry-run`) simply having found work to do — which is
what a script polls on to know a rotation is finished. See
[per-tenant secrets' rotation section](use-secrets.md#rotate-the-master-key)
for the variables (`CLEAT_SECRET_MASTER_KEY_VERSION`,
`CLEAT_SECRET_MASTER_KEY_PREVIOUS`, `CLEAT_SECRET_MASTER_KEY_PREVIOUS_VERSION`);
they are the same four, read the same way, because it is the same ring.

**Unlike `reseal-secrets`, a write here is not gated against a live worker
that cannot yet open the new key version** (`deployment_secrets.go`'s type
doc comment explains the scope decision). Roll the new key out to every
worker before running this without `--dry-run`, the same operational order
`use-secrets.md` documents, just not enforced by a refusal at the database
layer for this table yet.

## Fail-closed at boot

A worker refuses to start if an **enabled** plugin's required deployment
secret is missing or cannot be opened — `checkRequiredDeploymentSecrets`
(`cmd/cleat-worker/setup.go`), run once after every plugin's `Init` completes.
The alternative is a worker that starts fine and then fails every call that
plugin serves, one at a time, with an error that does not say why.

A plugin declares what it needs by implementing
`plugin.HasRequiredDeploymentSecrets`; `email-notify` and `llm` do today. A
plugin with no config section at all is not enabled, and this check never
runs against it — the same `plugin.ErrNotConfigured` gate that already
decides whether a plugin's ordinary `Init` runs.

## What is not covered

- **An external KMS.** The master key is supplied directly, the same as
  per-tenant secrets.
- **Reading a secret back.** There is no `get-deployment-secret`. Values go
  in and are used; they do not come out.
- **A write gate against workers mid-rotation.** See "Rotate the master key"
  above.
