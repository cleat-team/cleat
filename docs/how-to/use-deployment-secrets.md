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
| `llm` | `llm.providers.<provider>.api_key`, one per **enabled**, non-`ollama` provider that has not opted out (below) |
| `slack-notify` | `slacknotify.signing_secret` |
| `scheduled-backup` | `scheduledbackup.dsn` |

`email-notify` and `llm` refuse to start the worker if their required name is
missing or cannot be opened — see "Fail-closed at boot" below.

**`slack-notify` is conditional, not unconditional like the other two — and
not off the boot check entirely either.** A `slack-notify` deployment with no
history of `/slack/interactive` usage starts fine with no signing secret set
at all, and answers every outgoing Slack webhook normally; the interactive
callback route just refuses, with a 401, on every request until one is
configured. It refuses unconditionally at the request level: missing,
unreadable, empty, or retired all take the same path, and there is no config
that makes it accept an unsigned request (cleat#2172). `--require-auth` does
not gate this route either: `/slack/interactive` is on cleat's
hand-maintained public-route list (`cmd/cleat-worker/main.go`, next to
`/ingest/{source_id}` and the OAuth callback) because Slack's own request
carries no cleat API key, so the signature check is the only gate once a
request reaches the handler.

**But if `--plugin-config` still carries the legacy `slack_signing_secret`
field, `slacknotify.signing_secret` becomes required at boot, the same as
`email-notify`'s and `llm`'s names.** That field's presence is the signal
that this deployment used `/slack/interactive` before cleat#2172 moved the
secret out of `--plugin-config` — upgrading it with no replacement secret set
would otherwise go from "button clicks accepted" to "button clicks silently
401" with nothing at boot saying why. A deployment that has never set
`slack_signing_secret` — outbound-only, or new — is never asked for one.

**`scheduled-backup` follows the same conditional shape, by the owner's
`slack-notify` precedent (cleat#2172 1A), extended here on cleat-review's
#2236 finding.** A `scheduled-backup` deployment with no history of DSN
configuration boots cleanly with no `scheduledbackup.dsn` set at all, and its
background loop polls unconditionally regardless — see `Run`'s own doc
comment (`plugins/scheduledbackup/background.go`). An unresolvable DSN then
surfaces per backup attempt, recorded as a `failed` row in `backup_history`
with a generic tenant-facing message, exactly like a `pg_dump` failure would
be.

**But if `--plugin-config` still carries the legacy `dsn` field,
`scheduledbackup.dsn` becomes required at boot, the same as `slack-notify`'s
`slack_signing_secret`.** That field's presence is proof this deployment ran
scheduled backups against a real database before upgrading — a fresh
database does not reset an operator's `--plugin-config` — so a missing
`scheduledbackup.dsn` would otherwise fail every backup attempt silently
(into `backup_history`, not the operator's face) until someone happens to
need a restore and finds nothing there.

**`email-notify` needs `"email_enabled": true` in its `--plugin-config`
section, not just a non-empty file.** Every plugin's `Init` receives the
SAME raw `--plugin-config` bytes — there is no per-plugin section — so a
worker configured only for `llm`, or for any other plugin, would otherwise
have no way to tell "no config for me" from "a config file that happens to
exist". `email_enabled` is the explicit signal; `default_from` alone is not
enough, since it is legitimately optional.

**An `llm` provider that needs no deployment key** — a keyless self-hosted
`base_url` (vLLM, LM Studio), or one used only with a request-level
`api_key` (BYOK) — sets `"requires_deployment_key": false` on that
provider. Omitted, it defaults to `true`, today's behavior for every
enabled provider except `ollama`.

**A leftover `sendgrid_api_key`, `providers.*.api_key`, `slack_signing_secret`,
or `dsn` in `--plugin-config` does nothing** — none of the four structs has a
field for it anymore. What a worker does about a leftover one differs by
plugin:

- `llm` always logs a WARN naming the dead field and the
  `set-deployment-secret` command to use instead, at boot.
- `slack-notify` and `scheduled-backup` WARN the same way, and EACH also
  refuses to start if its own name — `slacknotify.signing_secret` or
  `scheduledbackup.dsn` — cannot be resolved in this case; see above.
- `email-notify` WARNs the same way, but **only if `email_enabled: true` is
  also set.** A leftover `sendgrid_api_key` with `email_enabled` still
  absent (or explicitly `false`) instead **refuses to start the worker** —
  that shape means a deployment was sending email before this table existed
  and would otherwise silently stop, rather than merely fail to notice a
  dead config field. Fix: add `"email_enabled": true`, move the key with
  `cleatctl set-deployment-secret --name email.sendgrid_api_key`, then
  remove `sendgrid_api_key` from `--plugin-config`.

**Not yet converted**, and still read from `--plugin-config` at `Init` the way
every plugin's credentials used to be: `blobstore` (its S3 key pair). Tracked
as a checklist item on cleat#1992. Do not write `blobstore.access_key_id` or
`blobstore.secret_access_key` here yet — nothing reads them from this table
until that plugin's own conversion lands. Unlike the others, blobstore's S3
client is built once at `Init` from a `minio-go` static-credential provider,
not fetched per call — converting it needs either a custom
`credentials.Provider` or reconstructing the client per use, which is why it
has not landed alongside the rest of this table.

`checkRequiredDeploymentSecrets` (below) consults `plugin.HasRequiredDeploymentSecrets`
per plugin rather than a single fixed list — `email-notify` and `llm`
implement it unconditionally, `slack-notify` and `scheduled-backup` implement
it conditionally (see above), and `blobstore` does not implement it at all
yet, which is a separate fact from whether a plugin is converted to read
from this table.

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
same reason `set-secret`'s value is not one.

**Writes are operator-only on PostgreSQL, not on MySQL or SQL Server.**
Migration 103 (PostgreSQL) revokes `cleat_app`'s INSERT/UPDATE/DELETE on this
table, so a worker's own database role cannot write here even if something
reachable through it tried. MySQL and SQL Server have **no equivalent role
split to revoke from** — there is one login per database on MySQL, and no
`cleat_app`-equivalent application role on SQL Server at all (unlike
`tenant_secrets`, which uses SQL Server's security-policy mechanism instead;
this table carries no `tenant_id` for that mechanism to key a predicate on).
So on those two dialects, **the serving login keeps full read/write access to
this table**, and encryption at rest — the master key never touches the
database on any dialect — is what actually protects it, not a database-level
write restriction. Owner decision 4A, recorded on #1992: a separate,
least-privilege login for MySQL and SQL Server is deferred to #2203, not
built here.

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
`plugin.HasRequiredDeploymentSecrets`; `email-notify` and `llm` do
unconditionally, `slack-notify` and `scheduled-backup` conditionally (only
when the legacy `slack_signing_secret`/`dsn` field is present in
`--plugin-config` — see above). A plugin with no config section at all is
not enabled, and this check never runs against it — the same
`plugin.ErrNotConfigured` gate that already decides whether a plugin's
ordinary `Init` runs. Neither `slack-notify` nor `scheduled-backup` has such
a gate of its own (neither `Config` carries an enablement flag), so each is
always "enabled" once loaded, and `RequiredDeploymentSecrets` is what
carries the conditional logic instead of `Init` refusing to run at all.

## What is not covered

- **An external KMS.** The master key is supplied directly, the same as
  per-tenant secrets.
- **Reading a secret back.** There is no `get-deployment-secret`. Values go
  in and are used; they do not come out.
- **A write gate against workers mid-rotation.** See "Rotate the master key"
  above.
