# Changelog

> **UPGRADE NOTES** — Breaking changes are called out at the top of each
> release section. Read them before upgrading between versions.

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### UPGRADE NOTES — breaking

- **Security: a child workflow's `child_workflow` event was written in plain text in the
  parent's `event_history`, with `--encrypt-sensitive-payloads` on.** (cleat#2312, cleat#2328)

  `PostgresStore.StartChildWorkflowAtomic` -- the function the live `ChildWorkflow` host call
  actually uses (`engine/children.go:244`) -- built its own hand-rolled `INSERT INTO
  event_history` for the `child_workflow` event and never called `encodeEventForStorage`, the
  one function every other write path routes through. So a child workflow's `child_input`, and
  the `payload` column carrying its `parent_workflow_id`/`parent_close_policy`, were plaintext on
  disk regardless of the flag -- despite `child_input` being named in `EncryptedEventColumns` and
  `TestEncryptedEventColumnsIsComplete` implying full coverage; that test only ever calls
  `encodeEventForStorage` directly, so it had no visibility into a write path that bypassed it.
  Found doing the measure-and-document follow-up on cleat#2312, confirmed live with a marker
  string before being fixed. `engine/store_children.go`'s `StartChildWorkflowAtomic` now calls
  `encodeEventForStorage` like every other writer
  (`TestAChildWorkflowEventIsEncryptedLikeAnyOther`). The MySQL and SQL Server twins of this
  function (`engine/mysql_store.go`, `engine/mssql_signals_promises.go`) had the identical
  hand-rolled INSERT and were converted too, for parity -- a no-op on those dialects today, since
  encryption at rest is not supported there and `--encrypt-sensitive-payloads` is refused at
  worker startup unless `--driver=postgres`. All three were also missing the `payload_encoding`
  column outright -- omitted from the INSERT's column list, rather than explicitly written. Its
  value is still `NULL` for a `child_workflow` event either way: `payloadEncodingFor` returns `nil`
  when the record's `Request`/`Response` are both empty, which is always true for this event type.
  The fix makes that `NULL` the encoder's explicit, reviewed answer rather than an accident of the
  column being left out of the statement -- the same value, reached the correct way, which matters
  once any dialect other than Postgres gains real encryption and this stops being a no-op.
  `TestEveryEventHistoryWriteRoutesThroughTheEncoder` guards every `INSERT INTO event_history` in
  `engine/` against this regressing again: each site must call the encoder or be named on an
  exemption list with a reason (MySQL/MSSQL sites with nothing to encrypt, and Postgres sites that
  consume an already-encoded value built elsewhere). Its own first version was blind to two of the
  thirteen production sites: MySQL spells this `INSERT IGNORE INTO event_history`, not
  `INSERT INTO`, so `mysql_store.go` and `mysql_events.go` evaded a guard whose pattern only knew
  the other two dialects' spelling -- found re-deriving the site count against cleat-review's
  independent sweep, not by review alone.

  **To upgrade:** `cleatctl reseal-payloads` does **not** repair rows already affected by this.
  Measured directly: seeded a `child_workflow` event with a plaintext `child_input` and ran
  `reseal-payloads --dry-run` against it -- it reported `not ciphertext: 2` (the `child_input` and
  `payload` columns) and left both untouched. A JSON object is not valid base64 (`{`, `"`, `:` are
  outside the alphabet), so `resealValue` classifies plaintext JSON the same way it classifies a
  column that was never supposed to be encrypted, and there is nothing in the stored row that
  tells the two apart. If a deployment ran with `--encrypt-sensitive-payloads` on and used child
  workflows before this fix, treat those `child_workflow` events' `child_input` and `payload` as
  unencrypted for that period; there is no tool in this release that finds or reseals them.

  Also see the entry below (cleat#2305) for the sharded-worker plaintext bug found the same
  week -- a different code path, the same shape of gap, and the same answer on `reseal-payloads`.

- **Security: a sharded worker (`--shards-file`) with `--encrypt-sensitive-payloads` and
  `--encryption-key-file` set wrote `event_history` in plain text, with no error, and the
  affected workflows still ended `done`.** (cleat#2305)

  `cmd/cleat-worker/main.go`'s sharded branch declared its own `payloadEncryption` variable that
  shadowed the one `loadPayloadEncryption` populated from the flags, so every shard's store
  factory was built with `WithEncryption(nil, true)` — encryption silently off — regardless of
  what `--encryption-key-file` pointed at. The non-sharded path was unaffected; this was specific
  to `--shards-file`. Found by cleat-review reviewing #2308; a real sharded worker with a real
  crash test now asserts against it
  (`tests/crash/payload_encryption_rotation_test.go`,
  `TestPayloadEncryptionShardedWorkerDoesNotWritePlaintext`).

  **To upgrade:** a sharded deployment that ran with `--encrypt-sensitive-payloads` before this
  fix has `event_history` rows written in plain text that believed they were encrypted.
  `cleatctl reseal-payloads` does **not** repair them: it converts ciphertext sealed under a
  previous key, and classifies a plain-text row as `unreadable` rather than as something to
  reseal, since nothing distinguishes "plaintext" from "ciphertext under a key I don't have" at
  that layer. There is no tool in this release that finds or reseals these rows; treat any
  sharded deployment that ran with encryption on before this fix as having unencrypted sensitive
  data in `event_history` for that period.

- **`cmd/cleat-worker` now catches `SIGHUP` and logs it rather than exiting.** (cleat#1992)

  Previously `SIGHUP` had no handler installed, so it fell through to the default action and
  terminated the process — the same as an unhandled `SIGTERM`, but without draining in-flight
  work first. It is now caught alongside `SIGINT`/`SIGTERM` and logged as "config/key hot-reload
  is not implemented yet (cleat#1992), ignoring"; the process keeps running. **Who is affected:**
  any deployment or process supervisor that sent `SIGHUP` to reload configuration, or relied on it
  to restart the worker, now gets neither — send `SIGTERM` for a graceful drain-and-exit instead.

- **cleat now requires Go 1.27 (`go 1.27.0`, `toolchain go1.27.1`), and CI, the images and the linter are pinned to it.** (cleat#2216)

  All eight `go.mod`/`go.work` files moved from `go 1.26.0` to `go 1.27.0` with `toolchain go1.27.1`, so the
  release is built, tested and linted on the toolchain users will run. **To upgrade:** building from source
  needs Go 1.27; with `GOTOOLCHAIN=auto` (the default) an older `go` downloads 1.27.1 itself. The container
  images build on `golang:1.27.1-bookworm`, pinned by digest. Nothing about the runtime, the WASM ABI or the
  wire format changed.

  For contributors: every `actions/setup-go` step now reads `go-version-file: go.mod` (go.work for the
  lint job); `go-version: stable` and hard-coded versions are gone, and `scripts/check-go-toolchain-pins.py`
  fails a PR that brings one back (it also checks that every module carries the same `toolchain` line and
  that every `FROM golang:` is that exact version). The job names `Test Go (... ) on 1.26` are unchanged
  because they are required status checks; the `1.26` in them is a label, not the toolchain. golangci-lint
  moved from v1.64.7, which could not read Go 1.27's export data and reported nothing, to v2.14.0, with
  the same set of checks (`.golangci.yml` is now the v2 format) and a planted known-positive that must be
  reported on every run.

- **The seven tenant-facing `/backups/*` HTTP routes are gone; backup configuration is
  operator-only, via `cleatctl backup`.** (cleat#2247)

  `POST/GET/PUT/DELETE /backups/configs`, `GET /backups/configs/{id}`, `GET /backups/history` and
  `POST /backups/configs/{id}/run` are all removed, along with `backup_config`/`backup_history`'s
  `tenant_id` column and row-level security on every dialect (migration v4). Any tenant could
  previously schedule an unfiltered `pg_dump` against the whole deployment DSN on a cron of its own
  choosing. **To upgrade:** replace any caller of those routes with `cleatctl backup
  config-create`/`config-list`/`config-update`/`config-delete`/`run`/`history`, run by an operator
  with database access, not a tenant API key. Existing `backup_history` rows are preserved; a
  config's own `tenant_id` value is dropped along with the column, since nothing reads it once the
  table is no longer tenant-scoped.

- **Every `/api/admin/*` route is off until `--enable-admin-api` is set, and answers 404 while it is off.** (cleat#2267)

  `POST`/`GET /api/admin/drain` answered 202 to any tenant's ordinary API key, so any tenant could take
  workers out of rotation. `--enable-admin-api` gated only force-complete, force-fail, re-replay and resolve
  (the retention sweep checked it inside its own handler), and the drain route was registered bare. All of
  them are now registered through one gate. **To upgrade:** a script or runbook that calls
  `/api/admin/drain` must start the worker with `--enable-admin-api`. **Helm:** the chart's `preStop` hook
  used to drain through this route and now only sleeps, because `adminApi.enabled` defaults to `false`;
  set it to `true` (with `auth.adminApiKey` or `auth.existingSecret`) to get the drain call back, knowing what
  that turns on. (Until cleat#2285 was fixed, neither setting let a run in flight finish: SIGTERM failed it.)

  **While the flag is on, any authenticated key of any tenant can drain the worker and trigger a retention
  sweep**, because cleat has no operator credential yet (cleat#2169). The worker logs a warning at startup
  that says so. The tenant-scoped operations (force-complete, force-fail, re-replay, resolve) still only act on
  the caller's own workflows. See [The admin API](docs/operations/admin-api.md) for the audit of which routes
  are worker-level and which are tenant-scoped.

- **A `--plugin-config` file carrying a leftover `sendgrid_api_key` with no `email_enabled` now
  refuses to start the worker, instead of silently disabling email.** (cleat#1992)

  `email-notify`'s SendGrid key moved out of `--plugin-config` into a deployment secret, gated
  behind a new `email_enabled` field. A config file still carrying `sendgrid_api_key` but not yet
  `email_enabled` used to read as "email is not configured" and disable the plugin quietly, with
  only an INFO log line — a deployment that had genuinely been sending email would stop, with no
  clear signal why. It now refuses to boot instead. To upgrade: add `"email_enabled": true` to
  `--plugin-config`, move the key with
  `cleatctl set-deployment-secret --name email.sendgrid_api_key`, then remove `sendgrid_api_key`
  from `--plugin-config` (a leftover key alongside `email_enabled: true` logs a WARN instead, and
  is otherwise harmless). See `docs/how-to/use-deployment-secrets.md`.

- **`dd_config.api_key` and `pd_config.routing_key` move into tenant secrets; the plaintext
  columns are dropped.** (cleat#1992)

  datadog-export and pagerduty-alert used to store the Datadog API key and the PagerDuty
  routing key in plain SQL columns. They now go through the same `plugin.Secrets` envelope
  encryption every other tenant secret uses, under the names
  `datadogexport.api_key.<config-id>` and `pagerdutyalert.routing_key.<config-id>` (one secret
  per config, not one per tenant, since a tenant can have more than one config of each kind).
  Admin routes are unchanged (`POST`/`PUT .../configs` still take `api_key`/`routing_key` in the
  request body, and responses still redact it) — only where the value lives has changed.

  No migration procedure is needed: **0.3.0 requires a fresh database, with no upgrade path
  from v0.2.0** (cleat#2058, owner decision 3). A fresh database never has a plaintext
  `api_key`/`routing_key` row to move, so `datadog-export`'s v4 migration and `pagerduty-alert`'s
  v3 migration simply drop the columns (`plugin.Migration.Up`, plain SQL) with nothing to carry
  forward.

- **`webhook_sources.secret` and `webhook_config.secret_configured`'s underlying secret move into
  tenant secrets, and a signing secret is now REQUIRED, not optional, on both plugins.**
  (cleat#1992, cleat#2172, owner decision (b))

  `webhook-ingest` and `notifications` follow the same `plugin.Secrets` envelope-encryption move
  as `dd_config`/`pd_config` above, under `webhook-ingest.source_secret.<source-id>` and
  `notifications.webhook_secret.<webhook-id>` respectively. **Unlike that change, this one also
  changes behaviour, not just storage:**

  - `POST /ingest/sources` (webhook-ingest) and `POST /webhooks` (notifications) now **reject a
    request with no `secret`** (400). Previously the secret was optional, and a source/webhook
    created without one accepted unsigned deliveries.
  - `POST /ingest/{source_id}` (webhook-ingest's public, auth-exempt ingest route) now refuses
    **every** request that lacks a valid `X-Hub-Signature-256` header (401), or whose secret
    cannot be read from the tenant secrets store — not found, retired, or any other lookup
    error (503). A lookup failure is never treated as "no secret configured."
  - `notifications`' outbound delivery loop applies the same rule in the other direction: a
    webhook whose secret cannot be read fails that delivery attempt (retried, then eventually
    marked failed) rather than sending an unsigned payload.
  - `PUT /webhooks/{id}` (notifications) can still **rotate** a secret, but can no longer
    **clear** one back to empty — `{"secret":""}` on an existing webhook is now rejected (400).
    `webhook-ingest` has no update route for sources, so this half does not apply there.

  **Who is affected:** any caller that created a source/webhook with no secret, or that relied on
  ingest/delivery accepting unsigned payloads — none can exist on a fresh 0.3.0 database, since
  creation without a secret is now refused at the door, but an integration built against the
  0.2.0 API contract (secret optional) will need to start sending one. See
  `docs/playbooks/integration-hub.md`, "Webhook ingest now requires a signing secret," for the
  operational detail.

  No migration procedure is needed for the same reason as `dd_config`/`pd_config`: **0.3.0
  requires a fresh database, with no upgrade path from v0.2.0** (cleat#2058, owner decision 3).

- **`slack-notify`'s request-signing secret moves to a deployment secret, `POST /slack/interactive`
  is now reachable without a cleat API key, and it refuses every unsigned request.**
  (cleat#1992, cleat#2172, owner decision, option A)

  Two problems, one fix. `--require-auth`'s default-on exemption list did not include
  `/slack/interactive`, so on a default deployment Slack's own POST — which carries no cleat API
  key — was 401ed before `slack-notify`'s own signature check ever ran; the interactive-approval
  flow could not work at all. And that signature check itself only ran `if p.slackSigningSecret
  != ""`, so an unset secret meant every request was accepted **unverified**, which would have
  been the live behaviour the moment (1) was fixed on its own.

  `/slack/interactive` is now on the same hand-maintained public-route list as
  `POST /ingest/{source_id}` and the OAuth callback (`cmd/cleat-worker/main.go`), and the
  signature check is unconditional: a signing secret that is missing, unreadable, empty, or
  retired refuses the request (401) the same way a bad signature does. There is no configuration
  under which an unsigned request is accepted. The request body is now bounded (1 MiB — a Slack
  interactive payload is a few KB) before anything is read from it, since the route no longer
  requires authentication to reach; the stale-request window is now symmetric (a request stamped
  more than 5 minutes in the future is refused, not just one more than 5 minutes in the past). The
  secret itself moves to `slacknotify.signing_secret`, read live on every request rather than
  cached at `Init` — see `docs/how-to/use-deployment-secrets.md`, which also explains why
  `slack-notify` does **not** refuse to start the worker unconditionally the way `email-notify`
  and `llm` do: it also serves outbound webhook notifications that do not need this secret, so an
  ordinary deployment with no history of `/slack/interactive` usage starts fine with none
  configured. It refuses to start **conditionally**: if `--plugin-config` still carries the
  legacy `slack_signing_secret` field — proof this deployment used interactive callbacks before —
  and `slacknotify.signing_secret` cannot be resolved, the worker refuses to start rather than
  silently 401ing every button click after the upgrade.

  **Who is affected:** any deployment using Slack's interactive (button-click) callbacks must set
  `slacknotify.signing_secret` via `cleatctl set-deployment-secret` before those callbacks will
  work — previously they silently accepted unsigned requests (or, with auth on, could not be
  reached by Slack at all). Such a deployment will now also refuse to start if it upgrades without
  moving the secret first. A leftover `slack_signing_secret` in `--plugin-config` otherwise does
  nothing and logs a WARN at boot naming the replacement command.

- **`scheduled-backup`'s Postgres backup-target DSN moves to a deployment secret,
  `scheduledbackup.dsn`, fetched fresh on every backup attempt rather than cached at `Init`, and
  it now refuses to start the worker under the same conditional rule as `slack-notify`.**
  (cleat#1992)

  `Config.DSN` is gone; a worker configured for scheduled backups no longer needs the DSN in
  `--plugin-config` at all. This also removed an unconditional boot-time gate: `Run`'s background
  loop used to refuse to start when `Config.DSN` was empty, which would have made setting the
  deployment secret after the worker was already running silently do nothing until the next
  restart — the opposite of what a deployment secret is for. The loop now polls unconditionally,
  the same as the `scheduler` plugin's own always-on loop; an unresolvable `scheduledbackup.dsn`
  surfaces per attempt, recorded as a `failed` row in `backup_history` with a generic
  tenant-facing message ("backup target credentials unavailable; contact the operator" — the
  detailed error goes to the operator log instead), exactly like any other `pg_dump` failure,
  rather than silencing scheduled backups deployment-wide.

  `scheduled-backup` now implements `plugin.HasRequiredDeploymentSecrets`
  **conditionally**, by the same precedent `slack-notify` set above (cleat#2172, owner decision
  option A): if `--plugin-config` still carries the legacy `dsn` field — proof this deployment ran
  scheduled backups against a real database before upgrading, since a fresh database does not
  reset an operator's `--plugin-config` — and `scheduledbackup.dsn` cannot be resolved, the worker
  refuses to start rather than silently failing every backup attempt until someone needs a
  restore and finds nothing there. A deployment that has never set `dsn` is never asked for
  `scheduledbackup.dsn` and boots exactly as before. See `docs/how-to/use-deployment-secrets.md`.

  **Who is affected:** any deployment using scheduled backups must set `scheduledbackup.dsn` via
  `cleatctl set-deployment-secret`. A leftover `dsn` in `--plugin-config` otherwise logs a WARN at
  boot naming the replacement command; a deployment carrying a leftover `dsn` with no
  `scheduledbackup.dsn` set will now refuse to start on upgrade, where it previously started
  fine and failed backups silently.

- **`blobstore`'s S3 access key pair moves to deployment secrets, and its `minio-go` client
  now re-resolves credentials on a 60s TTL instead of holding a static pair for the client's
  entire lifetime.** (cleat#1992)

  `access_key_id`/`secret_access_key` are no longer read from `--plugin-config`; they move to
  `blobstore.access_key_id`/`blobstore.secret_access_key`. Unlike every other plugin converted so
  far, the S3 client is still built once at `Init`, not per call — a custom `credentials.Provider`
  (`deploymentSecretsCredentialsProvider`, `plugins/blobstore/backend.go`) gates minio-go's own
  credential cache with a 60s TTL instead, so a rotated or retired key pair still takes effect
  without a worker restart, just not on the very next S3 call the way a bare `Get` would. A
  deployment secret that cannot be resolved fails the S3 request outright — it is never treated as
  an unsigned (i.e. effectively anonymous) request, and is never chained to a different credential
  source. A new `use_iam_credentials: true` config flag opts a deployment with no static keys at
  all — EC2 instance profile, ECS task role, or `AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY` in the
  worker's own environment — out of this table entirely, using the original credential chain
  unchanged. See `docs/how-to/use-deployment-secrets.md`.

  **BREAKING CHANGE: the AWS env-var/instance-profile/task-role credential chain is no longer the
  default for an s3 backend with no static keys — it is now opt-in via `"use_iam_credentials":
  true`.** Previously, `{"backend":"s3"}` with no `access_key_id` in `--plugin-config` fell back to
  that chain silently. As of this change, the same config now requires
  `blobstore.access_key_id`/`blobstore.secret_access_key` in the deployment secret store and, with
  neither those secrets nor `use_iam_credentials: true` set, **refuses to start the worker** — the
  same fail-closed check `email-notify`/`llm`/`slack-notify` already had, and unconditional: it does
  not depend on whether `--plugin-config` carries a leftover key pair.

  **Who is affected:** any deployment running `blobstore` with `"backend":"s3"` and no
  `"use_iam_credentials": true` — whether or not it ever had a static `access_key_id`/
  `secret_access_key` in `--plugin-config`. This includes every IAM-role deployment that relied on
  the old silent fallback: **before upgrading, either set both**
  `blobstore.access_key_id`/`blobstore.secret_access_key` with `cleatctl set-deployment-secret`, **or
  add `"use_iam_credentials": true`** to `--plugin-config` to keep using the instance-profile/task-role
  chain explicitly. The boot refusal names both options. A leftover key pair alongside the default
  memory backend, or alongside `"use_iam_credentials": true`, was already unused before this
  conversion and does not block startup — it still logs a WARN at boot naming the replacement
  commands, but is not treated as an upgrade hazard.

- **`oauth_config.client_secret` moves into tenant secrets; the plaintext column is dropped.
  `oauth_sessions.session_token`/`access_token`/`refresh_token` are no longer stored in any
  form, plaintext or sealed.** (cleat#1992, cleat#2295, cleat#2296)

  `client_secret` follows the same `plugin.Secrets` envelope-encryption move as `dd_config`/
  `pd_config` above, under `oauthprovider.client_secret.<provider>` (one secret per provider per
  tenant — `oauth_config`'s own primary key is already `(tenant_id, provider)`, so no
  per-config-id scheme is needed the way `dd_config`/`pd_config` require). No migration procedure
  is needed for the same reason as those: **0.3.0 requires a fresh database, with no upgrade path
  from v0.2.0** (cleat#2058, owner decision 3).

  The three session-table columns took a different path than an ordinary `plugin.Secrets` move.
  An earlier version of this change sealed them via `plugin.Payloads` instead (cleat#1992);
  cleat-review found that broke every login on a deployment with no `--encryption-key-file`
  set — which was every deployment shipped so far, since a nil `Payloads` fails closed rather
  than falling back to plaintext. Since nothing reads these three columns back (session lookup
  is by `token_hash`, a separate column, unaffected by any of this), the fix is to stop storing
  them at all rather than to fix the seal. A config row that has no matching secret is now
  distinguished from every other lookup failure internally (`plugin.ErrSecretNotFound`), logged
  server-side as `secret_not_found=true` — but `/login` and `/callback` still return the same
  generic "oauth config not found" to the caller either way. `/login` is unauthenticated, so a
  response naming `cleatctl set-secret` or cleat's internal secret-naming scheme would hand an
  anonymous caller both an existence oracle and detail about the deployment's own tooling
  (cleat-review, cleat#2295). Check the worker log for `secret_not_found` when diagnosing.

  **Who is affected:** anyone who queried `oauth_config.client_secret` directly, or who relied on
  `oauth_sessions.session_token`/`access_token`/`refresh_token` holding a value (sealed or
  plaintext) after a login — no shipped code path ever read the latter three back, so this is
  not expected to affect a real integration. Set the client secret with
  `cleatctl set-secret <tenant> --name oauthprovider.client_secret.<provider>` before inserting
  or updating an `oauth_config` row. See `docs/how-to/connect-any-identity-provider.md`.

- **A `cleatctl quota set` that creates a new tenant-quota row now enforces it by default.**
  (cleat#2046)

  `enforce` used to default to `false` on a fresh row, whether written by `cleatctl` or by the
  column's own SQL default: a quota counted and reported but never refused a run. `quota set
  --tenant X --limit-count N --window-seconds N` with no `--enforce` flag now creates the row
  with `enforce=true`, and the `tenant_quota.enforce` column's SQL default changes from
  `FALSE`/`0` to `TRUE`/`1` on all three dialects (a new plugin migration,
  `plugins/tenantquota/migrations.go` v3).

  **Who is affected: an operator who runs `quota set` on a tenant/resource that has no row
  yet, without passing `--enforce`.** That command now enforces the limit it creates, where it
  previously only tracked it. Pass `--enforce=false` to keep the old soft-tracking behaviour for
  a new row. **Existing rows are untouched** — the migration changes only the column default,
  which SQL applies solely to a row that omits the value, and `cleatctl` always supplies
  `enforce` explicitly; an `UPDATE` (an existing row, `quota set` with no `--enforce`) keeps
  whatever the row already had. A tenant with no `tenant_quota` row still has no limit at all —
  there is no tenant-creation hook that writes one (`cleatctl --create-tenant` is Postgres-only,
  cleat#1114, and unrelated to this).

- **A worker no longer migrates the database when it starts; migration is a deploy step.**
  (cleat#2117)

  `cleat-worker --migrate-only` applies the core and plugin migrations and exits `0` (any
  failure is non-zero). It is idempotent, needs no master key, and is safe if two run at
  once. A normal start now **verifies** the schema and refuses to start, with the
  remediation in the message, if a migration this binary ships is not applied. **One-line
  migration:** run `cleat-worker --migrate-only --db "$CLEAT_DATABASE_URL"` (with
  `--migrate-db` for a role that has DDL rights) before starting workers, or start a
  single-node or development worker with `--migrate-on-start`, which restores the old behaviour.

  **A schema ahead of the binary still starts**, with a warning naming both versions, so a
  rolling upgrade (which migrates to the new version while old workers are still running)
  does not wedge on the workers it is replacing. A schema *behind* is refused.

  Concurrent migrators are now safe on **every** dialect. Measured before this change, four
  concurrent runs against an empty database: PostgreSQL 4 of 4 succeeded; **MySQL 1 of 4 and
  SQL Server 1 of 4** (the rest failed with `Duplicate key name`, a deadlock, and "already an
  object named"). MySQL and SQL Server now queue on a named lock, as PostgreSQL always did.

  Shipped launch sites updated: the Helm chart (a pre-install/pre-upgrade hook Job, `migration.*`
  values), `k8s/migrate-job.yaml`, the `.deb` systemd unit (`ExecStartPre`),
  `docker-compose.cluster.yml` (a one-shot `migrate` service), and the three `cleat init`
  templates and `make` dev target (`--migrate-on-start`). Anything of your own that starts a
  worker against a fresh or older database needs one of the two.

- **A `TERMINATE` close-policy child is now recorded `status='terminated'`, not
  `status='failed'`.** (cleat#1978)

  When a closing parent's `TERMINATE`-policy child is closed by
  `enforceParentClosePolicy`, its row now gets `status='terminated'`,
  `error_op='parent_close'`, `error_code=NULL`, and an `error_msg` naming the
  **parent's actual outcome** — "parent workflow completed", "parent workflow
  failed", "parent workflow was dead-lettered", "parent workflow was
  terminated", "parent workflow was cancelled", or, for `ContinueAsNew`,
  "parent continued as new (run \<id\>)" (`parentOutcomeMessage`,
  `engine/store_lifecycle.go`).

  Previously it wrote `status='failed'` with a fixed `error_msg` of "parent
  workflow terminated" **regardless of why the parent actually closed** — a
  parent that completed successfully still left its TERMINATE children
  reading "terminated" in their error text.

  **Consequences for callers.**
  - A parent or grandparent awaiting such a child via `GetChildResult`/
    `AwaitChild` now sees `Error` prefixed `"[TERMINATED] "` (per cleat#1974's
    existing convention for a directly-terminated child) rather than the
    ordinary failure shape. The child still reports `Completed=true,
    Failed=true` either way, so nothing that only checks those two fields
    needs to change.
  - Any code, dashboard, or alert that filters `workflow_instances` on
    `status='failed'` and expected TERMINATE-policy children to be included
    must now also check `status='terminated'`.
  - `docs/reference/workflow-lifecycle.md`'s status table is updated to
    match.

- **`--max-quota-events` now defaults to 50,000 instead of unlimited.** (cleat#1829)

  A workflow run that writes more than 50,000 events is now **continued as
  new** at that point rather than growing without bound. This is a rollover,
  not a failure: the durable call is refused before dispatch, so no side effect
  happens, the guest's defers drain as they do for an explicit
  `ContinueAsNew`, and the executor records a `continue_as_new` suspension.

  Previously the default was `0` = unlimited, and `--retention-days` did not
  help — it sweeps *terminal* runs, and a runaway is not terminal, so one
  looping workflow could fill `event_history`.

  **If you have a legitimate run that exceeds 50,000 events**, set
  `--max-quota-events` higher, or `0` to restore the old unbounded behaviour.
  The number is a starting point rather than a measurement.

  `--max-quota-children`, `--max-quota-concurrency-keys` and
  `--max-quota-schedules` are **unchanged and still unlimited**, deliberately:
  exceeding those fails the workflow rather than rolling it over, so a default
  would break working deployments.

- **A start payload that omits a declared entry-point parameter is now refused,
  instead of binding the zero value.** (cleat#1065)

  Previously an absent `string` bound `""` and an absent `int` bound `0`, in Go
  and AssemblyScript. Python has always refused, and Rust refuses unless the
  field is `Option<T>` or carries `#[serde(default)]` — so the SDKs disagreed
  about the same payload.

  Zero-binding **cannot tell "sent zero" from "sent nothing"**, permanently, for
  every caller. The workflow runs, the result is plausible, and nothing records
  that the value is not the one that was sent. The information belongs to the
  caller and was destroyed at the boundary.

  **Declaring a parameter optional is how a workflow says absence is meaningful:**

  | SDK | spelling |
  |---|---|
  | Go | `*T` — binds `nil` when absent |
  | AssemblyScript | a declaration-site default — `note: string = "x"` |
  | Python | a Python-level default |
  | Rust | `Option<T>` or `#[serde(default)]` |

  **Consequences.**
  - **This is a compile-time change, not a runtime one.** The binding lives in
    code generated into the guest module, so an already-deployed `.wasm` keeps
    the old behaviour until it is rebuilt. There is no flag day: a workflow
    adopts the new contract when someone recompiles it.
  - **No ABI bump.** The wire protocol, function signatures and memory contract
    are unchanged; what changed is the acceptance rule inside generated guest
    code, which `CurrentABIVersion` does not describe.
  - **A single `string` parameter is unaffected.** It receives the whole payload
    rather than a value looked up by name, so "absent" does not apply to it.
    Python has no such fast path and still refuses; that divergence is recorded
    in `tests/conformance/entry_point_binding_cases.json`.
  - **A composite parameter already refused** on absence. This makes absence
    uniform across types rather than adding a rule for scalars.
  - **A STORED payload is bound by whichever guest is current when it fires,
    not by the one that was current when it was written.** Rebuilding a target
    workflow changes the contract every payload already persisted for it is
    judged against. It is not re-validated at write time and cannot be: the
    module's `cleat.metadata` section carries no entry-point parameter list, so
    the host has nothing to check an input against.

    **Three dispatch paths do this, not one.** A cron schedule is the one that
    surfaced it (cleat#1705); naming only that one would describe the exposure
    as narrower than it is.

    | plugin | stored input | written by |
    |---|---|---|
    | `scheduler` | `schedules.input` | whoever registered the schedule |
    | `jobqueue` | `task_queue.input` | whoever enqueued the job |
    | `eventtriggers` | `event_subscriptions.input_template`, merged with the event body | an operator, plus the publisher |

        grep -rln 'env.StartWorkflow' --include='*.go' plugins/ | grep -v _test.go

    `jobqueue` is the sharpest of the three: a row whose `input` is NULL is
    dispatched as `{}` (`plugins/jobqueue/background.go`), which refuses **every**
    declared parameter rather than one. `eventtriggers` is the most exposed,
    because the workflow author controls neither half of the payload — the
    template is an operator's and the event body is a publisher's.

    There is no "bind it the old way" mode, and the reason is structural rather
    than a decision deferred: the binding lives in generated guest code, so a
    payload has no contract version to pin to. Pinning one would mean carrying
    a declared-parameter list and a binding epoch through every SDK's metadata.
    **The migration is the same as for any caller** — send the parameter, or
    declare it optional.

    **Where it surfaces.** The schedule fires, the run is claimed, and the guest
    refuses it, once per occurrence for as long as the schedule exists. The
    refusal reaches `workflow_instances.error_msg` and the API's `error` field,
    so it is queryable — but the scheduler counts the firing as *started*,
    because it only reports failures from `StartWorkflow`, and nothing links a
    failed run back to the schedule that started it.

  - **The pre-landing measurement did not cover this, and the denominator is
    why.** It was quoted as "0 confirmed omissions in 112 literal starts",
    which is accurate and answers a narrower question than it appears to: it
    scanned literal **start** calls. A cron payload is not a start call — it is
    a `ScheduleCron` argument, or a `POST /api/schedules` body — so the whole
    population of stored inputs was outside the scan. Re-measured across
    `cleat-ports` after the fact (cleat#1705):

    | | count |
    |---|---|
    | literal starts scanned before landing | 112, **0** omissions |
    | `ScheduleCron` call sites | 1, and it **omits** a declared parameter |
    | `create_schedule` **with** an input | 5, all complete |
    | `create_schedule` **without** an input | 7 |

    The one `ScheduleCron` site is what broke. The 7 input-less schedules are
    latent rather than failing: their crons (`*/5 * * * *`, `0 7 * * *`, daily)
    do not fire inside a test run, so nothing exercises them.

    Parse that population rather than grepping it — three of those
    `create_schedule` calls carry `inp=` on a continuation line, and a
    line-oriented count reports them as input-less, which inflates the
    omission count in the alarming direction.

- **`plugin.HasRoutes.RegisterRoutes` now takes a `plugin.Router`, not `*http.ServeMux`.**
  (cleat#2232)

  Twenty-six plugin HTTP handlers across 17 plugins read a request body with no size ceiling at
  all, reachable anonymously through `POST /ingest/{source_id}` and `POST /slack/interactive` —
  both exempt from tenant auth by design. A plugin cannot be handed a body-size limit through a
  mux it registers *on*, only through one it registers *with*, so `RegisterRoutes`'s parameter
  changed from `*http.ServeMux` to the new `plugin.Router` interface (`Handle`/`HandleFunc`,
  structurally satisfied by `*http.ServeMux` and by the host's own body-limiting adapter alike).

  Every `RegisterRoutes` implementation in this tree took the one-line signature update in the
  same change. A plugin built outside this tree against the old signature silently stops
  satisfying `plugin.HasRoutes` — its routes never register, and previously nothing said why; the
  worker now logs an ERROR at boot naming the plugin and the fix (`RegisterRoutes(mux
  plugin.Router) error`).

  New request-body ceiling: `--plugin-max-body-size` (default 1 MiB) bounds every plugin route
  that does not declare its own. A route declares a tighter cap with `plugin.MaxBody(n, h)`
  (effective limit is `min(n, --plugin-max-body-size)`, so the flag can always tighten it
  further), or an unconditional, operator-config-owned ceiling with
  `plugin.MaxBodyFromConfig(n, knob, h)` (ignores the flag entirely, and PANICS at registration --
  refusing to boot -- if used on `POST /ingest/{source_id}`, `GET /oauth/{provider}/callback` or
  `POST /slack/interactive`, since the flag must always bound a route no credential guards).
  `blobstore`'s `PUT /blobs/{key...}` uses
  `MaxBodyFromConfig` against its own `max_blob_size` setting (default 10 MiB);
  `slacknotify`'s `POST /slack/interactive` uses `MaxBody` against its fixed 1 MiB callback size.

- **`plugin.Rebind` no longer rewrites `$N` placeholders to `?` for MySQL. A caller that pairs
  `Rebind`'s output with a raw `*sql.DB`/`*sql.Tx`/`*sql.Conn` (bypassing `plugin.PluginDB`) must
  switch to `plugin.RebindArgs`.** (cleat#2259)

  MySQL's `?` placeholder binds by **text occurrence order**, not by the `$N` number that was in
  the source before rewriting — any statement whose `$N` tokens are not already written in
  ascending order silently mis-bound its arguments on MySQL, and `Rebind` had no way to reorder
  them (it returns one string, not a reordered argument list). `Rebind` is now the identity for
  MySQL; the real `$N` → `?` rewrite, together with the argument reorder MySQL's positional
  binding needs, happens only inside the new `plugin.RebindArgs(query, dialect, args) (string,
  []any, error)`.

  **Who is affected:** nobody using `plugin.PluginDB`/`plugin.PluginTx` (`p.db.Exec`,
  `p.db.QueryRow`, and their transactional equivalents) — those already call `RebindArgs`
  internally, so an existing `Rebind()` call at one of those ~150 sites is now a redundant no-op
  for MySQL rather than a rewrite, and does not need to be removed. Only a plugin (in this repo
  or out of tree) that hands `Rebind`'s output straight to a raw driver handle is affected; it
  must call `RebindArgs` directly and use the reordered argument list it returns, not the
  original one.

  `RebindArgs` also fails closed on two shapes no permutation of arguments can make correct: a
  query mixing a literal `?` with `$N` placeholders, and an argument no `$N` in the query
  references.

- **A literal secret in a plugin's secret-only field (e.g. `llm.chat`'s `api_key`) is now refused
  before the plugin runs, instead of reaching `event_history` in plain text.** (cleat#2043)

  `${secret:NAME}` substitution (cleat#1987) only ever *resolved* a reference — nothing stopped a
  workflow writing the literal credential directly, and the plugin itself only ever saw the
  post-resolution string, so it had no way to tell a literal from a resolved reference either
  (cleat#1988/#2023 added the `api_key` field to `llm.chatRequest` but explicitly could not close
  this half). `plugin.FuncOptions.SecretOnlyFields` now lets a registration name which top-level
  JSON fields must hold exactly a `${secret:NAME}` reference; `llm`'s `chat` and `chat_stream`
  declare `["api_key"]`. A call whose declared field holds a literal, an ambiguous case-variant
  duplicate (`api_key` and `API_KEY` present together), an exact duplicate (the same key spelled
  identically twice), or input that is not a JSON object at all — malformed JSON, or valid JSON
  that is an array, a string, or a top-level `null` — is refused before the plugin function is
  invoked, and the `event_history` row records the refusal with the field's value replaced by a
  fixed marker rather than the caller's input.

  **Who is affected:** any workflow passing a literal instead of `${secret:NAME}` for `llm.chat`'s
  or `llm.chat_stream`'s `api_key` now gets a refusal instead of the call proceeding. The failure
  is classified the same as a plugin call the service itself failed (retryable by classification),
  but retrying with the same literal input refuses identically every time — the fix is to switch
  to `${secret:NAME}`. See [SecretOnlyFields](docs/contributor/plugins/plugin-developer-guide.md#hashostfunctions--workflow-callable-functions)
  for plugin authors declaring their own credential fields.

  **To upgrade:** this closes the leak going forward; it does not repair history already written.
  A literal `api_key` recorded to `event_history` before this fix is still there in plain text,
  for the same reason a plaintext row from before cleat#1988/#2023 still is. There is no tool in
  this release that finds or redacts these rows. **Treat any credential ever passed as a literal
  to `llm.chat`'s or `llm.chat_stream`'s `api_key` field as compromised and rotate it.**

  **Finding rows to confirm the scope before rotating.** These are read-only surveys, not a
  repair tool — they tell you whether any row exists, not how to fix it. Run as a role that can
  read across tenants (a plain `psql`/`mysql`/`sqlcmd` connection as an admin, not through a
  tenant-scoped RLS pool — on MSSQL that means a session with `sp_set_session_context` set, or
  `cleat_admin` role membership, or the query returns nothing regardless of what's in the table),
  and exclude the redaction marker this fix itself now writes (`[secret-only field, literal
  value refused]`) so a POST-fix refusal doesn't read as a PRE-fix leak. Each query below was run
  2026-09-25 against a row seeded with a literal, one with a resolved `${secret:...}` reference,
  one already redacted by this fix, and one on an unrelated plugin — only the literal row
  matched, on all three dialects:

  ```sql
  -- PostgreSQL. If this deployment ever ran --encrypt-sensitive-payloads, plugin_input on
  -- encrypted rows is ciphertext and this regex will not match them -- it can only clear rows,
  -- never rule a deployment in or out, once encryption has been in use.
  SELECT tenant_id, workflow_id, step, created_at
  FROM event_history
  WHERE plugin_name = 'llm' AND plugin_func IN ('chat', 'chat_stream')
    AND plugin_input ~* '"api_key"\s*:\s*"(?!\$\{secret:)'
    AND plugin_input NOT LIKE '%secret-only field, literal value refused%';

  -- MySQL. Payload encryption is not implemented on this dialect (see
  -- engine/event_storage_encoding.go), so plugin_input is always plain text here.
  SELECT tenant_id, workflow_id, step, created_at
  FROM event_history
  WHERE plugin_name = 'llm' AND plugin_func IN ('chat', 'chat_stream')
    AND plugin_input REGEXP '"api_key"[[:space:]]*:[[:space:]]*"(\\$\\{secret:)?'
    AND plugin_input NOT REGEXP '\\$\\{secret:'
    AND plugin_input NOT LIKE '%secret-only field, literal value refused%';

  -- SQL Server. No regex operator; LIKE with the literal quote-colon shape, same caveats.
  SELECT tenant_id, workflow_id, step, created_at
  FROM event_history
  WHERE plugin_name = 'llm' AND plugin_func IN ('chat', 'chat_stream')
    AND plugin_input LIKE '%"api_key"%'
    AND plugin_input NOT LIKE '%${secret:%'
    AND plugin_input NOT LIKE '%secret-only field, literal value refused%';
  ```

  A hit means a literal reached this row; a JSON field is unordered and can hold escaped
  characters these patterns do not account for, so treat a clean result as inconclusive rather
  than as proof nothing was ever recorded, and a row with the field spelled `API_KEY` or another
  case variant as a hit too (case-insensitive match on PostgreSQL and MySQL above; add a second
  `LIKE '%"API_KEY"%'` clause on SQL Server if auditing for that).

- **`--uninstall-plugin` now refuses up front on MySQL and SQL Server for any plugin whose Down
  chain is not proven by its own end-to-end test.** (cleat#2306)

  Nothing had ever run a plugin's Down SQL against a real database until cleat#1290 gave it a
  caller; measured 2026-09-25, 7 of 18 plugins' uninstall fails outright on MySQL and 17 of 18 on
  SQL Server, and a failed reversal is not a no-op — `RunDownMigrations` stops at the first error,
  and a partial reversal has already left a SQL Server database permanently unmigratable
  (`notifications`: `Cannot find the object webhook_delivery (4902)`, with no further migration
  able to run). `--uninstall-plugin` now checks the plugin against a short allow-list before
  touching anything (including before `--uninstall-dry-run`'s report) and refuses with a message
  naming what dialect it ran on and which plugins are verified there. `scheduled-backup` is the
  only plugin listed today — it is the only one with a real end-to-end uninstall test
  (`plugins/scheduledbackup/a_v4_down_keeps_uninstall_working_test.go`). PostgreSQL is unaffected
  (cleat-review's measurement found all 18 clean there). **To upgrade:** `--uninstall-plugin` on
  MySQL or SQL Server for any plugin other than `scheduled-backup` now refuses where it previously
  would have attempted the reversal — including for a plugin whose Down happens to work today but
  has never been proven by a test. There is no override; the fix is a test in the shape of
  `TestUninstallSchedulerBackupOnEveryDialect`, after which the plugin is added to
  `plugin.provenPluginDialects` (cleat#2306's phase 2 tracks doing this for every plugin).

### Added

- **`--uninstall-plugin` on MySQL: 10 more plugins move from refused to verified.** (cleat#2306
  phase 2)

  `audit-log`, `datadog-export`, `eventstore`, `feature-flags`, `kvstore`, `notifications`,
  `pagerduty-alert`, `rate-limiter`, `slack-notify` and `tenant-quota` join `scheduled-backup` in
  `plugin.provenPluginDialects` for MySQL: the phase-2 sweep
  (`TestUninstallDownChainIsClassifiedOnEveryDialect`,
  `cmd/cleat-worker/a_uninstall_down_chain_is_classified_on_every_dialect_test.go`) found their
  Down chains already clean there, matching cleat-review's original #2290 measurement, and the
  table-driven harness itself is what proves it now for every plugin, on every dialect, rather
  than the one-off test the phase-1 entry above required per plugin. Every other loaded plugin is
  classified too — as `outcomeRecoverable` or `outcomeUnrecoverable` in the same file's
  `knownBrokenPluginDown` — so an unclassified (plugin, dialect) pair fails CI outright instead
  of shipping silent, the same guarantee phase 1 gave `--uninstall-plugin` at the operator
  boundary.

  The harness's recoverable/unrecoverable split is schema-verified, not just exit-code-verified:
  it snapshots the schema after the initial install and compares it (via the new
  `migration/catalogdiff` package) against the schema after a failed Down's follow-up
  `--migrate-only` run, rather than trusting that run's `nil` error. That caught three pairs
  where the follow-up succeeds but the recovered schema is silently missing an object the failed
  Down destroyed — `blobstore`/MySQL (loses the `workflow_blob_refs` table),
  `oauth-provider`/MSSQL (loses `oauth_sessions.nonce`), `webhook-ingest`/MSSQL (loses
  `webhook_events.error_msg`) — found during cleat-review's PR #2346 verification and
  reclassified `outcomeUnrecoverable` before landing, rather than advertised as recoverable via
  `--migrate-only`.

- **API keys can expire and carry an `oauth_identity`: `admin.tenant_api_keys` gains `expires_at` and `oauth_identity` columns on all three dialects, and an expired key stops authenticating.** (cleat#2352)

  `expires_at` is nullable and `NULL` means "no expiry", so every key created before this migration
  keeps authenticating unchanged. `ResolveTenantFromAPIKey` now rejects an expired key exactly as it
  already rejected a disabled one, on PostgreSQL, MySQL and SQL Server alike. New
  `TenantStore.RevokeAPIKeyByHash` revokes by sha256 hash (Postgres-only, mirroring `RevokeAPIKey`) for
  the OAuth logout path in #2340, which holds only the hash and never the DB-generated key id.
  `cleatctl revoke-api-key` (and `--list`) gain an `EXPIRES` column and distinguish `revoked`/`expired`/
  `active`; the worker's startup key-count now excludes expired and disabled keys. `oauth_identity` is
  added on all three dialects for schema-shape parity (matching the `disabled_at` precedent) and left
  unwired — the #2340 minting path is what populates it. This is the schema/plumbing half of #2340; the
  oauthprovider plugin logic that mints short-lived keys lands separately.

- **`--encryption-key-file-previous`: `cmd/cleat-worker` can hold a previous payload-encryption key alongside the current one, for rolling key rotation.** (cleat#1992, #2308)

  `engine.PayloadEncryption` has supported a two-key ring since before this flag existed
  (`cleatctl reseal-payloads` was already built on it); the worker never exposed it, so rotating
  the key set by `--encryption-key-file` required stopping every worker first. `--encryption-key-file-previous`
  is read-only — every new seal still uses `--encryption-key-file` — and requires
  `--encryption-key-file` to also be set; a worker started with only the previous-key flag now
  refuses to start rather than silently ignoring it. See
  [`docs/how-to/rotate-payload-encryption-key.md`](docs/how-to/rotate-payload-encryption-key.md)
  for the rollout sequence a rolling rotation needs to stay safe, and cleat#2311 for a
  pre-existing gap the sequencing works around rather than closes.

- **`/livez`, `/readyz`, database-reachability metrics and alert rules: a database incident now looks different from a worker incident.** (cleat#2007)

  `/livez` says the process and its background loops are ticking and never looks at the database, so a
  database outage does not restart workers. `/readyz` is 503 while the worker has not finished starting, is
  draining, or its database did not answer its last deadline-bounded call, so a load balancer stops sending
  traffic a worker cannot serve. `/healthz` stays as an alias of `/livez`. The Helm chart and
  `k8s/deployment.yaml` now use `/livez` for liveness and `/readyz` for readiness (both used `/healthz`, so a
  worker that could not reach its database stayed "ready"). New metrics: `cleat_db_reachable`,
  `cleat_db_last_success_timestamp_seconds`, `cleat_db_consecutive_failures`, `cleat_db_probe_duration_seconds`
  (each with a `dialect` label). A call counts as failed if it errors or runs past its deadline, and the
  deadline is now enforced by a timer: against a real `docker pause` the gauge used to flip only at unpause.
  Two log lines mark the transitions: `database unreachable (deadline exceeded)` and `database reachable again
  after 43s`. `monitoring/prometheus/alerts.yml` tells the two incidents apart (every worker reports 0: the
  database; one worker reports 0 among healthy peers: that worker), suppresses the reaper's echo of an outage,
  and is unit-tested with `promtool test rules` in CI.

  **Breaking:** the unauthenticated health bodies contain only `ok`, `degraded` and reason codes
  (`background_loop_stuck`, `database_unreachable`, `starting`, `draining`, `memory_pressure`,
  `plugin_unhealthy`). `stale_loops` (loop names) and `pressure` are gone from them, and `reasons` lists every
  degraded reason (memory pressure no longer hides an unhealthy plugin). The detail is on the new
  authenticated `GET /api/admin/health`. Degraded states (memory pressure, an unhealthy plugin) are 200 on every
  probe; only the database, draining, starting and a stuck loop are 503. `backendkit`'s `Health()` now calls
  `/readyz`.

  `/livez` does not fail because a background loop is stuck in a call the database is holding: a stale loop
  is put down to the database only while the database has not answered since it went quiet and that is
  still being observed, with a 30-second grace after recovery. Measured against a real `docker pause`.

  **The audit log no longer records the infrastructure probes** (`/healthz`, `/livez`, `/readyz`, `/metrics`): a
  fixed list, not configurable, and the same one that is exempt from authentication.

- **`/metrics` is valid Prometheus text again: histograms had doubled label braces and non-cumulative
  buckets, so Prometheus dropped every scrape.** (cleat#2266)

  Every histogram line was written `name_bucket{{a="b"},le="1"}`, and the buckets were per-bucket counts
  with `le="+Inf"` not equal to `_count`. Prometheus rejects a scrape as a whole, so with the bundled
  stack `up` read 0 (`CleatWorkerDown` fired on healthy workers) and no cleat metric was stored, including
  `cleat_db_reachable`. The braces are emitted once, buckets are cumulative and `+Inf` is `_count`, so
  `histogram_quantile` over `cleat_claim_latency_seconds`, `cleat_poll_wait_seconds` and the rest now gives
  real answers. CI runs `promtool check metrics` against what the three booted cluster workers actually serve.

- **`cleatctl quota get|set|list`, the operator surface for `tenant-quota`.** (cleat#2046)

  `quota get --tenant X [--resource R]` reads a tenant's quota row(s); `quota set --tenant X
  --resource R --limit-count N --window-seconds N [--enforce=true|false]` creates or updates
  one; `quota list [--tenant X]` lists rows for one tenant (every dialect) or every tenant
  (Postgres and MySQL — refused on SQL Server, where `tenant_quota`'s row-level security policy
  has no cross-tenant bypass, so an unscoped connection would read back an empty table rather
  than an honest error). All three work on Postgres, MySQL and SQL Server.

  **Stale-write refusal, mirroring `set-tenant-setting`.** `quota set` reads the current row
  first; a concurrent writer's change since that read refuses the write (`409`-shaped, not a
  silent overwrite) rather than clobbering it, and says so with a re-read command.

### Changed

- **The rate limiter refuses a cluster-wide limit it cannot honour, instead of quietly giving you
  a per-process one.** (cleat#1581)

  `mode: "db"` with no database configured used to log a warning and fall back to `memory`. The
  worker started, and because the memory limiter is an in-process map, **every worker served the
  full configured rate** — a four-worker deployment enforced four times the limit it was told to.
  An unrecognised mode did the same thing more quietly: the middleware tests `mode == "db"` and
  treats everything else as memory, so `"DB"`, `"database"` and any other near-miss also selected
  per-process limiting.

  Both now return an error from the plugin's `Init`, naming the reason.

  **Who is affected: only deployments that are already not getting what they asked for.** The
  default is unchanged (`memory`), and a config that does not set `mode` behaves exactly as
  before. If a worker now refuses to start, it was silently enforcing the wrong limits before.
  The fix is to supply a database or to say `mode: "memory"` and mean it.

### Fixed

- **A busy MySQL or SQL Server worker no longer drops events: adaptive batch flushing is PostgreSQL-only.** (cleat#2348)

  The batch writer's fence check and INSERT are PostgreSQL SQL (`set_config`, `$1::jsonb`, `jsonb_populate_recordset`,
  `ON CONFLICT`). Batch mode is on by default and is entered on the step rate alone (above `--batch-flush-enter-rate`,
  default 500 steps/sec), on every driver. Measured against the real worker on MySQL 8.4 and SQL Server 2022: every event
  routed through it failed (`Error 1305 ... set_config does not exist`; `'set_config' is not a recognized built-in function
  name`), was retried for the whole flush retry window (750 ms by default) and then dropped, and the run carried on, so
  history came up short with only an `adaptive flush failed` ERROR line to say so. On MySQL under default settings and
  1,500 fast workflows it entered batch mode at ~1,180 steps/sec and logged ~4,400 failed flushes for ~4,500 steps; on SQL
  Server it also flapped in and out of batch mode (55 transitions in one 200-workflow run). Runs held open mid-flight showed
  11 of 40 events missing on SQL Server and every event after entry missing on MySQL. Earlier builds did this on those two
  drivers whenever a worker crossed the rate.

  Those two drivers now always flush each step directly (`Engine.getAdaptiveFlusher` refuses the batch path for a MySQL or
  SQL Server store, and the worker does not build the flusher registry or open its connection pool for them, so the
  connection census drops by `--batch-flush-max-connections` on those drivers). The worker logs one INFO line at startup,
  `adaptive batch flushing is PostgreSQL-only: ...`, unless batching was already disabled. `--batch-flush-*` flags have
  no effect on those drivers. PostgreSQL is unchanged. Real-database tests on all three dialects
  (`TestAWorkerAboveTheEnterRateKeepsPersistingEveryEvent`, with PostgreSQL as the control that must use the batch writer)
  and a source scan that fails when a store implementing `perStepEventFlusher` is not named in the gate.

- **The fullstack template's page works: `make web` serves it through a same-origin proxy that holds the API key.** (cleat#2307)

  The page called the worker directly, which cannot work: the worker sends no CORS headers, so a browser refuses
  the calls from any other origin, and the worker requires a key that a page must never hold. The template now
  ships `proxy/main.go` (standard library only, run by `make web`, on `http://127.0.0.1:3000`): it serves the page
  and forwards exactly two calls, `POST /api/workflows/my-fullstack-app/start` and
  `GET /api/workflows/{id}/query?key=status`, adding the key from `CLEAT_API_KEY` or the file named by
  `CLEAT_API_KEY_FILE` (the file wins; with neither it refuses to start). Every other path is a 404 and every
  other method a 405; the browser's `Authorization` and `Cookie` are dropped, upstream `Set-Cookie` and every
  response header but `Content-Type` are dropped (and that is served as JSON or plain text, never HTML, under a sandbox CSP), redirects are not followed, and the key is never logged. It
  listens on loopback by default (any other address needs `-allow-remote`), serves the page's script as its own
  file so the page needs no inline script, refuses an oversized body before calling the worker, and while it
  listens on loopback it refuses a foreign `Origin` (403), a non-JSON `POST` (415)
  and a non-loopback `Host` (421, the DNS-rebinding case). The template's CI test now runs `make web` and drives
  the page's calls through it to `complete`, and asserts each of those refusals and that the key appears in no
  response and no log line. Worker-side CORS is not added.

- **`runDueBackups` advanced `next_run_at` from inside the due-rows scan loop, on the same
  transaction while its own result set was still open.** (cleat#2291)

  On PostgreSQL the second statement failed outright ("there is already a query being processed
  on this connection"), leaving `next_run_at` unadvanced, so the config stayed due and a later
  poll (or a concurrent worker) could dispatch it again. On MySQL the failed statement poisoned
  the connection ("driver: bad connection"), and since that error was only logged, the config
  was fired anyway on every single poll — never advancing, dispatching every ~60s indefinitely.
  The advance now runs after the due-rows cursor closes, and its error is no longer just logged:
  a config whose advance fails is dropped from that poll's dispatch rather than fired blind.

  On PostgreSQL specifically, an isolated per-config failure also used to abort the *whole*
  claim transaction — unlike MySQL and SQL Server, where a single statement error leaves the
  transaction usable, Postgres marks the entire transaction aborted, so one bad config's
  `UPDATE` silently took every *other* due config in that poll down with it too. The advance now
  runs under a `SAVEPOINT` on Postgres, rolled back on error, so a failing config no longer
  blocks its siblings.

  `executeScheduledBackup` no longer recomputes and overwrites `next_run_at`/`last_run_at` after
  a backup finishes — the claim transaction above is now the only place either column is
  written. Previously, a manual "run now" (`cleatctl backup-run`, which just sets
  `next_run_at = now()`) issued while a scheduled backup was still in flight could have its
  `next_run_at` clobbered by that backup's own completion-time write, silently dropping the
  manual trigger; measured losing it on PostgreSQL. One consequence: `LAST_RUN_AT` in
  `cleatctl backup-list`'s output now means when the backup was *dispatched* (claimed), not when
  it *completed* — check `backup-history` for completion status and timing.

- **The migration runners no longer race database/sql over their pinned connection.** (cleat#2215)

  A migration whose context ended mid-transaction (a test or a library caller that passes a deadline or
  cancellable context to the runner, or a lock wait that outlasted it) could panic with a nil pointer
  dereference in `Runner.session`'s release: database/sql starts a goroutine for every transaction begun on a
  cancellable context and, when the context ends, closes the pinned `*sql.Conn` from that goroutine while the
  runner is still using it to `RESET` its session settings and unlock. It is a nanosecond window
  (`Conn.grabConn` checks `done`, then takes the lock), so it showed up in CI under load and twice ejected a PR
  from the merge queue. The worker's own `--migrate-only` is not a trigger: its context has no deadline and the
  signal handler is installed after that mode exits, so SIGTERM ends the process outright (exit 143).

  The cancellation does not need a driver without session-reset hooks: with lib/pq 1.12.3, which has them, the
  cancel watcher marks the connection bad, so the async `Rollback` returns `driver.ErrBadConn` and database/sql
  closes the pinned connection from that goroutine all the same. A fake driver copying that behaviour panicked
  10, 6 and 6 times per 4.8M iterations on the old pattern and never on the new one. Migration transactions are now begun on a context the
  run's context cannot cancel (`internal/pinnedtx`); statements inside them still take that context, so a
  blocked statement is cancelled as before and the runner's own `Rollback` ends the transaction. The wait for a
  lock is still bounded by `lock_timeout` on PostgreSQL and by `GET_LOCK`/`sp_getapplock` (15 minutes) on MySQL
  and SQL Server; nothing that was bounded became unbounded. The same change covers plugin migrations.

- **The fullstack template's documented path reaches a `done` run.** (cleat#2067)

  Re-measured on `develop` on a stock `postgres:16`: of the six breaks in the issue, the superuser worker, the
  silent `make deploy` and the missing entry point had already been fixed by other changes, and three were not.
  Fixed here, in `cmd/cleat/templates/fullstack/`:
  - **The first run ended `failed`.** `SubmitOrder` called `http.fetch` on `https://example.invalid/validate`, which
    the egress policy refuses, and a comment said the failure dead-letters (it does not: the run ends `failed`; only
    a durable call that exhausts its retries dead-letters). The workflow now validates its input and waits three
    durable seconds where your payment call goes, so the first run finishes `done` and `charging` is visible to a
    poller. The README shows the `DurableCall` that replaces the sleep.
  - **The README's rate-limiter warning was stale.** `db` mode with no database refuses to start (cleat#1581); it
    no longer "warns and falls back to `memory`". The README now says what to look for
    (`rate-limiter: initialized mode=db`).
  - **The published-state route was wrong.** The README, `web/index.html` and a comment named
    `GET /api/workflows/{id}/state`, which does not exist (404); it is `/query?key=`. `web/index.html` also cannot call
    the worker as a file (the worker sends no CORS headers; cleat#2307) and says so.
  - `docker-compose.yml`: the host ports (`CLEAT_PG_PORT`, `CLEAT_API_PORT`) and the worker image
    (`CLEAT_WORKER_IMAGE`, for the days before a release has published `ghcr.io/cleat-team/cleat-worker`) can be
    overridden, and the obsolete `version:` key, which made every `docker compose` command warn, is gone from all
    three templates.

  Still open: the image is not on ghcr until a release publishes it (cleat#2064 added the step; `v0.2.0` predates it).

  The template's CI test no longer steps around any of this: it builds the repository's Dockerfile and runs the
  scaffold's own `make up`, `make logs`, `make deploy` and `make run` against its own `docker-compose.yml` (only the
  image and the host ports substituted), asserts the run ends `done` with `status` = `complete`, and runs the
  scaffold's own `go test ./...` (whose `SubmitOrder` test now advances the simulated clock; the first draft hung on
  the durable sleep). Each of the superuser worker, a `make deploy` with no database, the failing placeholder, a
  missing `--plugin-config` and a `/state` route in the README was reinstated one at a time and fails it.

- **`oauth-provider` builds an identity provider's EC signing key with `ecdsa.ParseUncompressedPublicKey`.** (cleat#2300)

  `parseJWK` made the key by setting `X` and `Y` on an `ecdsa.PublicKey` and calling `IsOnCurve`, which Go 1.26
  deprecates in favour of the parse function (the linter reported it once the toolchain moved to 1.27, cleat#2216).
  Behaviour is unchanged for every key that was accepted or refused before: a coordinate is still read as a number
  (a stripped or zero-padded one is accepted, since some providers send those), and one that does not fit the curve
  is refused with the existing `EC point is not on <curve>` message. The new function also refuses the point at
  infinity. Nothing else in the plugin changed.

- **A literal route sibling of a public wildcard is no longer treated as public, and a plugin stuck on
  the pre-cleat#2232 route signature now refuses to boot instead of silently registering nothing.**
  (cleat#2274, cleat#2277)

  `auth.Middleware`/`auth.HostBindingMiddleware` decided whether a request was public by matching it
  against a throwaway `*http.ServeMux` built from only the exempt patterns, not the real serving mux.
  A literal route that happens to share a path segment with a public wildcard — `POST /ingest/sources`
  beside the public `POST /ingest/{source_id}` — matched the wildcard on the throwaway mux and was
  let through with no credential at all. `auth.MiddlewareWithMux`/`auth.HostBindingMiddlewareWithMux`
  now resolve the exemption against the same `*http.ServeMux` the worker actually serves from, so a
  request is public only when the real mux itself would route it to an exempt pattern.
  `HostBindingMiddleware`, the mux-less wrapper, is removed — every caller now passes the real mux.

  Separately, `checkPluginRouteSignatures` (added with cleat#2232's `plugin.Router` interface) already
  detected a plugin still implementing the old `RegisterRoutes(mux *http.ServeMux) error` signature,
  but only logged a warning: the worker kept booting with that plugin's routes silently unregistered.
  It now returns an error that `cleat-worker`'s `main` treats as fatal, refusing to start rather than
  serving with a route table that does not match what a plugin author believes is wired up.

- **A worker that cannot decrypt a run's history now releases the run instead of failing it, or completing it on garbage.** (cleat#2311)

  The store swallowed a payload decryption failure: it put `"[DECRYPTION_FAILED]"` in the field and carried
  on. A worker holding the wrong key (a mis-ordered key rotation, a mis-deploy) therefore ended the run
  FAILED permanently on the checksum chain, or, with `--disable-checksum-verification`, ended it DONE having
  run the guest on the placeholder. A worker holding the right key could have finished it either way.
  `LoadEventHistory` (the read replay acts on) now returns an error wrapping `engine.ErrPayloadDecryption`
  for any of the ten encrypted event fields, or for a sealed `payload` column, that will not open; a value
  that is not shaped like a sealed one is plaintext and is read as it is, so runs whose history was written
  before `--encrypt-sensitive-payloads` was switched on, by sharded workers before cleat#2308, or as
  plaintext child events before cleat#2328, still load (one edge: a plaintext string field that happens to be
  valid base64 of 28 or more bytes is read as sealed and refused, since there is no envelope to tell them
  apart); the worker
  treats that like a plugin it lacks (cleat#1710) and releases the run with `--unservable-release-backoff`,
  so a worker with the key picks it up. The history stream's `error` event says so in one sentence, and the
  paginated and streaming-chunk reads that display history still show the placeholder for a field they cannot
  read.

  Also from the same change, because a worker that cannot read a history must not write to it either: every
  admin verb that appends an event (`POST /api/admin/instances/{id}/force-complete`, `.../force-fail`,
  `.../re-replay`, the dead-letter retry) checks, inside the transaction that would write it, that the history
  can be decrypted, and answers 409 (`state_conflict`, "this worker cannot read workflow ... Nothing was
  changed") otherwise, with the status change rolled back. `.../steps/{n}/resolve` refuses the same way.
  Before, force-fail and force-complete answered 200 and appended an `admin_action` sealed under the wrong key,
  after which no single-key worker could re-replay, retry or resolve the run; re-replay did the same when its
  history load failed, and resolve returned a 500 carrying the decrypt text. A run released this way is counted
  in the new `cleat_workflow_releases_total{check}` (`check="history_decrypt"` for this case; the version and
  plugin checks are counted too) and logged at WARN once per five minutes per run (a stuck run wrote about 23 MB
  of identical lines a day; the store's own per-field decrypt WARN is now silent on the replay read, which
  returns the failure instead). The worker's own store and the shard stores now carry the metrics instance, so
  `cleat_decryption_errors_total` moves on the default and the sharded paths (it was a no-op there).

  Not covered (cleat#2324): a worker started WITHOUT `--encrypt-sensitive-payloads` has no key ring to fail
  against, and nothing in a sealed column marks it as ciphertext (by design, engine/encryption.go has no
  envelope). Measured: such a worker ends the run FAILED on the checksum chain, or DONE with
  `--disable-checksum-verification`, exactly as before this change.

- **SIGTERM drains before it cancels, and a run cut off by shutdown is released, never failed.** (cleat#2285)

  The signal handler cancelled the worker's context at once. Every in-flight durable wait was aborted and the
  run was written FAILED (`finalize workflow: begin tx: context canceled`), a terminal status nothing
  reclaims, so a rolling deploy lost every run it interrupted. The worker now stops claiming and waits up to
  `--shutdown-grace` (default 20s; Helm `worker.shutdownGrace`) with heartbeats alive; a run that finishes in
  time is finalized normally. What is still running when the grace ends is cancelled and **released** for another
  worker to replay, on every path that ends a run because the worker is going away (the outcome that comes back,
  a finalize or continue-as-new that could not begin, the defer phase). The guest is told to stop rather than
  that a call failed, so a workflow that compensates on error does not run its compensation on a fault that
  never happened.

  **What this does not promise.** A durable call still in flight when the grace ends can run twice: the worker
  does not abort an HTTP call already on the wire, and another worker may replay the step (cleat#2287). A
  genuine failure that lands during shutdown is delayed rather than lost: the run is released and fails again,
  for real, on the worker that picks it up. If your calls run longer than 20s, raise `--shutdown-grace` and the
  orchestrator's kill deadline with it.

  Also changed: `GET /api/admin/drain` no longer completes the drain or stops the worker (it is read-only), and
  `POST /api/admin/drain` is a cordon that does not exit the process. The chart sets
  `terminationGracePeriodSeconds: 60` (was the 30s default), `k8s/deployment.yaml` likewise, and the compose
  files set `stop_grace_period: 60s` (Docker's 10s default would cut the drain off).

- **`--require-host-match` now boots and serves as the role a deployment runs as (cleat#2258).**
  The boot check counted `tenant_domains`, and every authenticated request looked its Host up in
  it, both on a connection that carried no tenant. As `cleat_app` on PostgreSQL the count raised
  `cleat.tenant_id is not set` and the worker exited; on SQL Server the security policy filtered
  every row, so the worker refused with "tenant_domains is empty" while a domain was registered.
  Both reads now run under the tenant they are about, and the boot check counts across every
  tenant (suspended ones included). It still refuses when no domain is registered, and a Host
  another tenant owns is still answered exactly as an unregistered one. MySQL, which is a
  database per tenant, was not affected.

- **`GET /api/workflows/{id}/stream` answered 500 "streaming not supported by this server" on every default build.** (cleat#2254)

  Every plugin middleware wraps the core mux, and the audit-log's response-writer wrapper embedded
  `http.ResponseWriter` without a `Flush()`, so every handler behind it that asks for an
  `http.Flusher` was refused: the workflow stream route, eventstore's SSE route and the audit NDJSON
  export. The wrappers in audit-log, tenant-quota and backendkit now pass `Flush` and `Unwrap` through.
  A test serves a real request through the real plugin middleware chain, and another fails the build
  for any type that embeds `http.ResponseWriter` without both methods. The stream tests never met a
  wrapper before, because they call the handler directly with a recorder that has a `Flush`.

- **The audit log no longer drops events silently when the database is slow or down.** (cleat#2168)

  A request that finds the audit buffer full now waits up to 1s for room (`audit_enqueue_wait_ms`), and an
  event whose insert fails is retried with backoff until `audit_retry_deadline_ms` (60s) instead of being
  discarded on the first error. A retry checks by event id that the first attempt did not commit before
  appending, so a lost commit acknowledgement does not record the event twice. What is still given up is
  counted, logged at Error (at most once a second per reason), and reported: the new metric
  `cleat_plugin_events_lost_total{plugin,reason}` (reasons `buffer_full`, `insert_failed`, `shutdown`,
  `shutdown_inflight`, with a Grafana panel), and `/healthz` answers `200` with `"degraded": true, "reason":
  "plugin_unhealthy"` (a reason code only; the endpoint is unauthenticated) for five minutes after a loss (not
  503: a stalled audit table must not restart the worker). Plugin health is computed by a background loop and
  cached, so a probe never runs plugin code. Four workers now drain the buffer, and shutdown drains it for up
  to 10s on a timer that does not wait for a database call the driver will not cancel: what is queued is
  counted `shutdown`, and what is inside such a call is counted `shutdown_inflight` (an upper bound, since it
  may still commit).
  A row's timestamp is now the time of the request rather than of the append, so **chain order (seq) may
  differ from timestamp order**; the chain follows commit order and verifies either way. Anything that polls
  `/audit/events` or `/audit/export` by time can miss a retried row and should follow `seq`. There is still
  no durable spool: a killed process loses what is in its buffer. The five config keys are prefixed
  (`audit_buffer_size`, `audit_workers`, `audit_enqueue_wait_ms`, `audit_retry_deadline_ms`,
  `audit_shutdown_drain_ms`), because the plugin config is one flat object; zero or negative means the
  default, and buffer 1,000,000, 64 workers, enqueue wait 30s, retry deadline 1h and shutdown drain 25s are
  the caps. New public surface: `plugin.Environment.EventsLost`, the metric, and the `/healthz` shape.

- **A worker with no `CLEAT_SECRET_MASTER_KEY` now refuses to start on PostgreSQL and SQL Server when the
  database holds secrets, as it always did on MySQL.** (cleat#2123)

  The startup check read `tenant_secrets` across all tenants, and that read cannot see the table there: on
  PostgreSQL it raised (and the caller treated the error as "cannot tell"), on SQL Server it returned 0. So a
  worker started without the key against a database full of secrets booted normally and failed on the first
  workflow that resolved one, from inside a plugin call, with an error that does not mention keys.

  The check now reads each tenant's rows under that tenant's own context, **suspended tenants included**, and
  runs after the migrations. **A read that fails now refuses to start** rather than passing: with no key and a
  table it cannot read, the worker cannot tell whether it would fail on its first plugin call.

  **Upgrade note.** A deployment that was silently running without the key while holding secrets will now
  refuse to start. That is the check working; set `CLEAT_SECRET_MASTER_KEY` to the key the secrets were sealed
  with.

- **Write-ahead call intent now works on a sharded deployment, and an operator can resolve an
  ambiguous call there.** (cleat#1778)

  `ShardedStore` implemented `WorkflowStore` and not the two call-intent interfaces, so on a
  sharded worker:

  - Declaring any operation in `--write-ahead-intent-ops` made that operation **fail outright**.
    The engine refuses rather than downgrading to at-least-once — deliberately, since a
    durability guarantee that is configured, believed and absent is the failure this feature
    exists to remove — so the call was not dispatched and the error named the store.
  - `ResolveStep`, the documented way out of an ambiguous call, answered
    `store *engine.ShardedStore cannot resolve call intents`. An operator holding an answer from
    the external service had no way to record it.

  All three intent methods now route to the shard that owns the workflow, like every other
  per-workflow operation. The pending row, its completion, the replay that reports it ambiguous
  and the operator's resolution all land in the same database.

  **Who is affected: sharded deployments that declared a write-ahead operation.** Unsharded
  deployments are unchanged — the three dialect stores always implemented these methods. A shard
  whose store cannot honour the guarantee still fails loudly, and now names the shard.

- **A sharded deployment now honours per-tenant and per-run limit overrides, and says so when it
  can't.** (cleat#1853)

  `ShardedStore` was also missing `GetTenantSettings` and `GetRunLimits`, which all three dialect
  stores implement. Both are reached by a type assertion that returns **with no log at all** on
  failure, so every workflow on a sharded deployment silently got the worker's flag values for
  `WasmInstanceTimeout`, `WasmWallClockCeiling`, `HostRetryBudget` and `MaxWorkflowDuration`,
  regardless of what a tenant or a run had configured.

  **This is not a limit escape.** A tenant's settings are already clamped to the operator's, and a
  run's to its tenant's, so the fallback can only ever be wider than intended, never past the
  operator's ceiling.

  Both now route to the shard that has the answer. `GetRunLimits` routes by workflow ID, like the
  rest of `ShardedStore`. `GetTenantSettings` has no workflow ID to route by — every shard was
  opened for the same tenant — so it tries each shard and uses the first non-empty result, which
  survives an operator having written the override to only one shard (the writer, `cleatctl
  set-tenant-setting`, takes one connection and has no fan-out across shards). A shard that
  genuinely cannot answer now logs a warning naming itself, once per request, instead of nothing.

### Added

- **The audit log is now tamper-evident: each tenant's rows form a SHA-256 hash chain, and `cleatctl audit verify` checks it.** (cleat#2047)

  Every `audit_events` row carries `seq`, `prev_hash` and `row_hash`, and each tenant has a head
  row in `audit_chain_heads`. An edited row, a row removed from the middle, and rows removed from
  the end are each reported, by kind, at the first place they occur. `cleatctl audit verify
  (--tenant <id> | --all-tenants) [--json]` exits `0` clean, `1` on a break, and `2` when it could
  not look, and the two non-zero values are different on purpose.

  **What it is not:** the chain proves the integrity of what was recorded, not that everything was
  recorded, and it does not stop a database administrator who rewrites a whole chain and its head
  together. `docs/reference/audit-log.md` states both, with the encoding an offline verifier needs.

  **Export.** `GET /audit/export` streams the caller's tenant as JSON Lines (any authenticated caller of
  the tenant, like `/audit/events`), ending in a `checkpoint` record so a truncated export is visible,
  and `GET /audit/verify` reports the chain. `cleatctl audit export (--tenant | --all-tenants)` is the
  operator variant: there is deliberately no cross-tenant HTTP endpoint. Every event record is
  verifiable offline with `plugins/auditlog/testdata/audit_chain_reference.py verify-export`, and the
  record schema is documented as a contract in `docs/reference/audit-log.md`.

  **An export never ends in a checkpoint over a hole.** Rows removed while it runs (a retention sweep) fail
  the export (`409` before anything was sent, otherwise the connection is aborted and `cleatctl` says
  `INCOMPLETE`) rather than leave a gap that verifies. The checkpoint records `from`, `to` and `after_seq`, so
  `verify-export` knows which completeness rules apply. The checkpoint itself is unsigned, so an edit can
  present a full export as a range or a resumed one: the options say what the caller knows (`--require-full`,
  `--expect-head`, `--expect-floor`, `--expect-after`, `--expect-unchained`), each pins the kind of export
  expected, and a bare run prints a `NOTE` saying what it did not establish. An anchor is a point the chain
  passes through, so one recorded earlier still verifies an honest later export; one that retention has
  removed is reported as retired. At least one anchor must match a record in the file, or the run is
  INCONCLUSIVE (exit 2), because retirement is decided by the checkpoint and a forger writes that; only
  `--expect-head` can, so `--expect-floor` and `--expect-after` always need it beside them. The chain is unkeyed, so records above the highest anchor can
  be rewritten and re-hashed: the run says how many. An unchained record after a chained one is refused, as is
  any in a resumed export, and the checkpoint records `unchained`. The behaviour of every kind x option x edit
  is one table, `chain_export_matrix_test.go`.

  **`GET /audit/events` no longer answers 500 on SQL Server:** it wrote a literal `LIMIT`.

  Behaviour changes to know about:
  - **Existing rows are not backfilled.** They stay unchained and are outside the guarantee.
  - **Retention is per tenant.** It deletes an expired prefix of a tenant's chain (at most 5,000 rows
    per tenant per hourly sweep) and records a floor, instead of one cross-tenant `DELETE`. A backlog
    of expired rows now drains over several sweeps.
  - **Each request is a short transaction that locks its tenant's head row,** so a tenant's appends
    serialise. Different tenants do not contend.
  - **Text that cannot be stored (invalid UTF-8, NUL) is replaced with U+FFFD instead of dropping the
    whole event.**
  - **A value too long for its column is truncated with a `...[truncated]` marker instead of failing the
    insert** (`path` 700 characters and 800 UTF-16 units, `method` 255, the free-text columns 4,096). On
    MySQL and SQL Server an over-long path used to leave no audit row at all.
  - **The MySQL `audit_events.timestamp` column becomes `DATETIME(6)` holding UTC** (it was
    `TIMESTAMP(6)`, which stops at 2038).
  - **`cleatctl audit verify --retention-days N`** also reports a retention floor that covers rows too
    young to have expired.
  - **Verify also reports `rows_below_floor`:** chained rows that survive at or below the recorded floor.
    Retention deletes them in the same transaction that moves the floor, so a floor moved by a single
    `UPDATE` no longer hides an edit beneath it (cleat#2188).

- **Tenant-secrets master-key rotation: a key ring and `cleatctl reseal-secrets`.** (cleat#1991)

  A key is now named by an integer version. `CLEAT_SECRET_MASTER_KEY_VERSION` (default `1`, which is what every
  existing row carries), `CLEAT_SECRET_MASTER_KEY_PREVIOUS` and `CLEAT_SECRET_MASTER_KEY_PREVIOUS_VERSION` let a
  worker open rows sealed under the key being retired while sealing new ones under the new key. `cleatctl
  reseal-secrets [--dry-run]` re-seals every row online, verifying before it writes and writing conditionally so a
  concurrent `set-secret` is not undone. A worker that cannot open some stored version refuses to start and names it.
  See `docs/how-to/use-secrets.md`.

  **The system now checks it.** Every worker publishes the key versions it can open in `admin.workers`, and a
  write at version *v* (`set-secret`, and each row `reseal-secrets` moves) is refused while any live worker
  cannot open *v*, naming the worker. The worker's start (publish its keys, read every stored secret) and a
  write (read the registry, write the row) are serialised by one named database lock, so a worker cannot start
  while a write is landing that it would then be unable to read. A worker whose membership loop stalled longer
  than its stale window re-registers and re-checks, and stops if it now holds a secret it cannot open.

  **Upgrade notes.**
  - **Every worker now registers in `admin.workers`**, not only those started with
    `--cluster-connection-budget`. The connection share still counts only workers that have a budget, so a
    mixed fleet divides what it did before.
  - **Complete the upgrade before the first rotation.** A worker from before this release is invisible to the
    gate unless it registered under the connection budget, and a registry row with no key set is read as
    "opens version 1 only".
  - A worker stalled for more than five minutes while still serving is invisible to a writer; see
    `docs/how-to/use-secrets.md`.

- **`audit-log` now records who: `user_id` on every row was the empty string, always.** (cleat#1881)

  The identity was already available — `oauth-provider` resolves an OAuth session to an email and
  puts it in request context — but nothing read it. A new neutral `auth.SubjectFromContext`
  (populated by `oauth-provider`, readable by any plugin) closes the gap without one plugin
  importing the other; an unauthenticated request still records an empty `user_id` and is never
  refused over a missing identity.

  **The ordering this depends on is now declared, not inherited from the alphabet.** `audit-log`
  reads context after the request has passed through every plugin wrapped around it, so it only
  sees `oauth-provider`'s value because `"oauth-provider"` happens to sort after `"audit-log"` in
  the tie-break `plugin.Discover()` falls back to when neither plugin declares a relationship.
  Renaming either plugin would have silently reverted `user_id` to always-empty with no test
  failing. A new plugin-contract clause (C14) and guard pin the real registered order instead —
  `plugin.PluginInfo.Requires` was considered and rejected for this, since it makes plugin
  discovery fail outright if the required plugin isn't registered, which is the wrong coupling
  between two independently-optional features.

- **`cleat build --target rust` refuses a workflow that iterates a `HashMap` or `HashSet` (R008).**
  (cleat#1864)

  Iterating a map is idiomatic Rust requiring no unusual act, unlike every other rule this checker
  enforces (opening a file, spawning a thread, reading the clock) — so it was the rule an author was
  least likely to suspect was missing. `HashMap`/`HashSet` default to a per-process random hash
  seed, so their enumeration order is not guaranteed by the language.

  **This is hardening, not a fix for an observed divergence.** cleat intercepts the WASI
  `random_get` import a `HashMap`'s hasher seeds from and binds it to a value deterministic in
  (workflow ID, step), so two replays of one workflow see the same order today regardless. The rule
  guards a language guarantee cleat is not relying on staying true, and its message does not claim
  otherwise.

  Construction, insertion, lookup and removal on a `HashMap`/`HashSet` are unaffected — only
  enumerating one (`for x in m`, `.iter()`, `.keys()`, `.values()`, `.into_iter()`, `.drain()`)
  triggers R008, which suggests `BTreeMap`/`BTreeSet`.

  **Detected via syntactic binding tracking, not a full type checker**, matching this checker's
  existing no-toolchain design: a function parameter's declared type or a `let`'s type
  annotation/constructor call is tracked within that function, and an enumerating use of a tracked
  name is reported. Two shapes are known, documented limits rather than silent misses — a map
  reached through a struct field, and one enumerated inline off a `.collect::<HashMap<_, _>>()`
  chain with no intermediate binding — see `testdata/vet-checks/rust/known_limit_*`.

- **Pre-emptive cancellation, with a terminal status of its own.**
  `POST /api/workflows/:id/cancel` accepts `{"preemptive": true}`, which stops the workflow and
  records **`cancelled`** rather than asking it to stop. (cleat#1153)

  **Why it exists.** Cancellation was cooperative *and unobservable*: `RequestCancellation` set a
  flag and left both stopping and reporting to the workflow, and `cancelled` was an error code
  rather than a status the engine ever wrote. So a run that honoured a cancellation and one that
  simply finished were **both `done`**, and an operator could not answer "did this stop because I
  asked it to?".

  **It runs the defers it owes.** A workflow with registered `defer` bodies goes to `terminating`
  carrying `cancelled` as its recorded outcome, is re-claimed, replays its history as a defer
  segment, and only then becomes `cancelled`. That is the same two-phase transition `terminate`
  uses, shared rather than rebuilt.

  **Asynchronous, like terminate**, and for the same reason (`tiers.yaml` decision D6). The
  endpoint answers `{"status": "cancelled"}`, which names the *outcome*; a caller that needs to
  know the run has finished polls the status.

  **Not a breaking change.** `preemptive` defaults to `false`: a client sending `{"reason": "..."}`
  gets the cooperative path and the `cancellation_requested` response exactly as before.

  **`cancelled` is a new terminal status**, so a client that switches exhaustively on workflow
  status should add a case. It is excluded from the active-child count, from parent close
  policies, and from every other "is this run settled?" predicate — see
  `docs/reference/workflow-lifecycle.md`.

- **`completed_by` on the workflow object** — the worker that performed the terminal write.
  `assigned_to` is a *lease*, not an audit field: every terminal write clears it while fencing on
  it, so it is blank on every finished run and cannot answer "which worker ran this" after the
  fact. Measured on a live database, `assigned_to` was blank on 185 of 185 terminal runs.
  (cleat#1118)

  **Schema change**, applied by `postgres/074`, `mysql/064` and `mssql/068`: `workflow_instances`
  gains a nullable `completed_by`. `postgres/075` additionally re-emits `finalize_workflow_status`,
  whose `done` and `failed` branches record it; the `ready` branch deliberately does not, because
  that run goes back on the queue and recording there would name whoever yielded.

  Returned by both read paths — `GET /api/workflows/:id` and the workflow listing.

  **Nothing is backfilled.** The column is NULL on every row written before the migration, and on
  any run that reached a terminal state without ever being claimed — no worker ran it. A run
  terminated through its *defer phase* names the worker that ran the defer phase rather than the
  workflow body: that transition clears the lease deliberately, to fence the old owner out.

### Changed

- **An update name is now reusable.** `POST /api/workflows/:id/update/:name` may be called any
  number of times over a run's life; each request is a row of its own with its own `promise_id`.
  Previously a name was consumed for the life of the workflow and the second request was refused
  with `409 update_name_used`. That `detail` value no longer occurs — `update_already_pending` is
  the only remaining 409 on this endpoint, and it clears when the in-flight request is answered.
  A client that branches on `detail` keeps working. (cleat#1416)

  **Schema change**, applied by `postgres/068`, `mysql/062` and `mssql/066`:
  `workflow_update_requests` gains a `request_id` column and is keyed
  `(workflow_id, request_id)` instead of `(workflow_id, update_name)`. Existing rows are backfilled
  from `update_name`, which is unique per workflow under the old key, so a workflow suspended
  mid-update across the upgrade completes against the correct row.

  Updates are **not** idempotent: a caller that retries after its first request was answered gets a
  new update rather than a replay. Carry your own key in the payload if you need at-most-once.


### UPGRADE NOTES — breaking

- **Terminating a workflow that has registered `defer` bodies is now asynchronous,
  and runs those bodies before the workflow becomes terminal.** Migrations
  `postgres/040`, `mssql/043` (MySQL needs none).

  Previously `TerminateWorkflow` wrote `status = 'terminated'` and then released
  the workflow's sticky assignment and concurrency keys. The registered defers
  never ran — and the resources a defer body would have released were dropped
  anyway, by the host, in the wrong order, with nothing recording that anything
  was owed.

  Now such a workflow goes to a new non-terminal status, **`terminating`**,
  carrying the outcome it will be given. A worker claims it like any other
  workflow, replays its history as a *defer segment* — the body does not run
  again; it is refused any new work — runs the outstanding defer bodies, and only
  then applies the recorded outcome and releases the resources.

  **Consequences.**
  - A caller that terminates and immediately reads `status` may see `terminating`
    rather than `terminated`, and must poll. This is `tiers.yaml`'s decision D6.
  - A workflow with **no** registered defers still terminates in one step, as
    before. Most deployments will see no change at all.
  - Terminating a workflow that is already in its defer phase terminates it
    immediately, cutting the cleanup short.
  - A defer phase never changes the outcome: if it traps, times out, or cannot
    start, the recorded outcome is applied anyway and the lost cleanup is logged.
    `defer_phase_deadline` (5 minutes) bounds it.
  - **Apply the migrations.** `postgres/040` widens `admin.claim_workflows` and
    the claim's partial indexes; `mssql/043` widens the filtered ones. A
    deployment running this code against the older schema keeps working — the
    cross-tenant claim falls back with a warning naming the migration — but its
    defer phases are never claimed, so every terminate waits out its deadline and
    skips the cleanup.

  **A closing parent's `TERMINATE` children work the same way**, and the change
  matters more there because it is a bulk operation: one closing parent used to
  drop every child's concurrency keys and sticky assignment at once, before any
  of their defers had run. A child that owes cleanup now goes to `terminating`
  carrying `pending_terminal_status = 'failed'` — the close policy's own
  outcome, not `terminated` — and is failed once its defers have run. A child
  that owes none is failed immediately, as before.

  The admin force-resolve verbs are unchanged: they still terminate in one step
  and still skip their defers.

  See IMPROVEMENT-PLAN §3.75, §3.112 and §3.114, and
  `docs/reference/workflow-lifecycle.md` for the whole state machine.

- **Workflow definition names are now per-tenant.** `workflow_defs`' primary key
  becomes `(tenant_id, name, version)`, and the three foreign keys that reference
  it — from `workflow_instances`, `workflow_tags` and `workflow_routing` — carry
  `tenant_id` too. Migrations `postgres/035`, `mysql/034`, `mssql/038`.

  Two tenants can now each hold their own `order-processor`. Previously the name
  was a shared namespace: the second tenant to deploy one was refused, and before
  that (pre-0.2.0) it silently overwrote the first.

  **Consequences.** A deploy no longer returns `409` for a name another tenant
  holds — there is no conflict to report. `ErrWorkflowDefOwnedByAnotherTenant` is
  removed, along with the default-tenant adoption window that let a definition
  deployed before per-tenant ownership stay reachable by every tenant; on
  PostgreSQL that also removes `OR tenant_id = '00000000-...'` from
  `tenant_isolation_defs`, bringing it into line with SQL Server, which never had
  it. A workflow started for a tenant that has not deployed the definition it
  names is refused by the foreign key.

  **MySQL only:** `workflow_defs.tenant_id` was nullable with no default, unlike
  the other two dialects. `mysql/034` backfills `NULL`s to the default tenant and
  makes the column `NOT NULL DEFAULT`, as a primary-key column must be.

  See IMPROVEMENT-PLAN §3.77 and D7 in `tiers.yaml`.

- **Cross-schema child workflows are removed.** The `cleat_child_workflow_in_schema`
  host call, the `cleat:host-calls/durable-extended-children` component interface,
  the `--peer-schemas` worker flag and the corresponding surface in the Go, Rust,
  Java, Python and AssemblyScript SDKs are all gone. A worker started with
  `--peer-schemas` now fails on an unknown flag.

  It let a workflow start a child by writing a row directly into another
  PostgreSQL schema. That makes the other deployment's schema part of your API and
  its migrations part of your compatibility surface, and it had no settled answer
  for whose tenant the child belonged to — the definition lookup in the peer schema
  carried no tenant predicate, and where the target tenant could not be recovered
  from the schema name the insert ran with no tenant context at all.

  **Use the other pool's API instead**, the same way any two services talk. Nothing
  in `tiers.yaml` claimed this feature at any tier and no end-to-end test exercised
  it. See IMPROVEMENT-PLAN §3.78.

  ABI host-function count goes 59 → 58 on both backends. `CurrentABIVersion` is
  unchanged: nothing that remains changed shape.

- **Terminating a parent now closes its children.** `TerminateWorkflow` never
  called `enforceParentClosePolicy`, on any dialect, so terminating a parent
  left its `TERMINATE`-policy children running and its `REQUEST_CANCEL`
  children unflagged — while force-completing or force-failing the *same*
  parent closed them. Measured 2026-09-02: a child of a parent closed with
  `TerminateWorkflow` stayed `ready`; the same child under `AdminForceComplete`
  went to `failed`.

  Who this breaks: a deployment that relied on `terminate` being the narrow
  "stop this one workflow" verb. Its children now close with it —
  `TERMINATE` children are failed with `parent workflow terminated`, and
  `REQUEST_CANCEL` children have cancellation requested. `ABANDON` children are
  unaffected, as they always were.

  The design document says this is what should happen (*"`enforceParentClosePolicy`
  runs on parent terminal transition"*, *"children are cancelled with their
  parent, preventing orphan workflows"*), and the deciding argument is internal
  consistency: `adminForceResolve` is an operator verb on an unclaimed workflow
  setting a terminal status with a direct `UPDATE` — the same shape as
  `TerminateWorkflow` in every respect — and it enforced the policy. See
  IMPROVEMENT-PLAN §3.79.

### Added

- **`allowed_signals` can be set.** `GET` and `PUT
  /api/workflows/{id}/allowed-signals` read and replace the list
  `--require-signal-auth` checks a caller against, backed by
  `WorkflowStore.SetAllowedSignalCallers` on all three dialects. Until now
  nothing in cleat could write that column, so 0.2.0's note below — that the
  flag denied every signal with no supported remedy — described a gap that is
  now closed.

  `PUT` replaces the whole list; send it without a caller to revoke, or `[]` to
  clear. Both verbs are scoped to the calling tenant, and a workflow belonging
  to another tenant answers `404` rather than `403`, so the endpoint cannot be
  used to find out which ids exist.

  **`--require-signal-auth` still defaults to `false`.** Every workflow starts
  with an empty list and nothing sets one at start time, so enabling the flag
  denies every signal until callers are granted per workflow. Grant first, then
  enable. See `docs/reference/worker-config.md`.

- **An operator can resolve a call left ambiguous by a crash.** `POST
  /api/admin/instances/{id}/steps/{step}/resolve`, with `X-Confirm:
  resolve-step` and `{"response": "..."}`, records an outcome for a durable
  call that was in flight when the process died.

  Such a call leaves a pending row, and replay reports it as `[AMBIGUOUS]` and
  says to check the external service before retrying — with nowhere to put the
  answer. An `AmbiguityResolver` could supply one, but the resolve path returns
  immediately when none is configured, which is most deployments, so the
  workflow reported the same ambiguity on every replay forever.

  The response is written to the event as though the call had returned it,
  because that is what replay has to see. What keeps it honest is the new
  `resolved_by` field, written to the same row in the same statement: the
  outcome is usable by replay and permanently marked as **asserted by an
  operator rather than observed**. The row must still be pending, so an
  operator racing a worker cannot overwrite a real result — whoever writes
  first wins. IMPROVEMENT-PLAN §1.4 phase F.

- **An operator can re-replay a stopped workflow.** `POST
  /api/admin/instances/{id}/re-replay`, with `X-Confirm: re-replay` and
  `{"generation": N}`, returns a `failed`, `terminated` or `dead_lettered`
  workflow to `ready`: the claim, heartbeat and error fields are cleared and
  the generation is bumped, so a stale worker's late write cannot land. History
  is **kept** — the workflow resumes from what it recorded rather than starting
  over. The non-terminal statuses are refused because the dispatcher owns them.

  Re-replay refuses a workflow whose history holds an unresolved ambiguous
  call, and names the step: replaying one stops again in the same place for the
  same reason, so it points at the resolve endpoint above instead. This was the
  last of the three admin operations that was a stub returning
  `not implemented` on all three dialects. IMPROVEMENT-PLAN §3.20.

- **`GET /api/workflows/{id}/history` reports `err_code` and `resolved_by` per
  event.** `err_code` is how the caller classified a failed call — the
  `ErrorCode` a `ServiceCaller` supplied through `CleatError` — recorded at the
  time of the failure, where history previously collapsed the whole
  classification to a single retryable-or-not bit. Its vocabulary is the one
  `workflow_instances.error_code` already stores, so one operator query matches
  in both tables.

  It is for reading, not for deciding: `err_non_retryable` stays authoritative
  for retry behaviour, deliberately. The two can legitimately disagree, because
  the guest's own retry policy travels across the ABI — and deriving
  retryability from the class instead would let an upgrade change the retry
  behaviour of workflows already in flight, which is the determinism bug §2.35
  exists to prevent. Both fields survive history compaction.
  IMPROVEMENT-PLAN §2.35.

### Fixed

- **Closing a workflow left concurrency slots and sticky-worker assignments
  held until their TTL.** `releaseWorkflowResources` runs the two best-effort
  cleanups that follow every commit taking a workflow out of the runnable set.
  Two paths committed such a transition without it:

  - `MySQLStore.TerminateWorkflow` execed its `UPDATE` and returned, where
    PostgreSQL and SQL Server both released. One slot per terminated workflow,
    on a tier-1 dialect (IMPROVEMENT-PLAN §3.76).
  - `enforceParentClosePolicy`'s TERMINATE arm failed every child of a closing
    parent and released nothing, **on all three dialects** — so one closing
    parent stranded a slot per child (IMPROVEMENT-PLAN §3.80).

  Bounded rather than leaked: `concurrency_keys.expires_at` is `NOT NULL` and
  the reaper deletes expired rows, so the slots freed themselves at the key's
  TTL. They freed themselves silently, with every workflow queued on those keys
  waiting out the window for nothing.

- **The reaper's reclaim-timeout default was too tight to safely cover a single
  failed-then-retried heartbeat, and an idle worker had no way to detect a
  database stall at all.** After a database stall, every running instance's
  `heartbeat_at` ages past the stale threshold at once; whichever worker's
  reaper reaches the database first after recovery could reclaim a run that is
  still alive, including its own. `engine.DBPinger` gives an otherwise-idle
  worker (nothing in flight, so no heartbeat write to prove liveness with) a
  real liveness signal, and `reapingIsSafe()` now requires both a recent
  confirmed contact-OK and an elapsed grace period since the last recorded
  trouble before trusting a stale `heartbeat_at` as evidence of a dead holder.

  **`--reclaim-timeout`'s derived default changes from `2*heartbeat` (floored
  at 10s) to `heartbeat + 3*dbCallDeadline(heartbeat) + heartbeatRetryInterval(heartbeat)
  + reclaimSlack` (floored at 10s) — about 14.5s at the default 5s
  `--heartbeat`, up from 10s.** This is a wider safety margin, not a
  behaviour anyone has to opt into: the old value undercounted a single
  heartbeat call that fails and is retried. A real network-level stall (not
  just a slow-but-reachable server) also keeps a failing call blocked until
  the stall itself clears rather than until its own client-side deadline —
  measured directly against a real PostgreSQL container under `docker
  pause` — so the invariant also accounts for the wait before that retry is
  even issued, which at `--heartbeat` below one second gets no faster a
  retry than the worker's own ordinary cadence. Finally, the modeled worst
  case has zero margin at `--heartbeat` >= 4s (the two formulas are
  algebraically identical there), and does not account for the retry
  needing a fresh connection — real and documented on SQL Server, which
  marks a connection bad after a cancelled call whose own cancel-drain also
  fails, exactly what a genuine stall produces — so `reclaimSlack` (a fixed
  1s) covers what the model leaves out rather than widening a term that
  means something else. An explicit `--reclaim-timeout` is unaffected.

  MySQL's `ReapStaleInstances` also now runs inside an explicit transaction —
  under `interpolateParams=true`, the previous autocommit statement could keep
  committing server-side after the caller's context was cancelled, so a
  caller-visible "deadline exceeded" did not mean the reclaim had not
  happened. Postgres and SQL Server were already transactional here.
  cleat#2005.

- **The reaper can no longer be fooled into reclaiming a live run by a
  whole-fleet database stall, only by a genuinely dead worker.** #2166
  covers a worker that itself observed database trouble; this covers the
  complementary gap — a fleet-wide stall silences heartbeat *writes* while
  reads keep working, so every running row ages past the reclaim threshold
  together, and whichever worker's reaper reaches the database first after
  recovery reclaims runs that are still alive, including its own, with no
  trouble ever recorded on its own side.

  A new optional `DBStallDetector` capability (implemented on all three
  dialect stores) reports the shape of the currently-stale set: how many
  running rows have missed at least one heartbeat, how many distinct
  workers they belong to, and whether not even one running row anywhere in
  scope has a heartbeat newer than the detection threshold. The reaper
  suppresses reclaiming for one tick whenever no recent heartbeat has
  landed anywhere, across more than one worker — a single fresh survivor
  anywhere blocks suspicion outright — and keeps suppressing until either
  the stale set genuinely clears or the full reclaim window elapses a
  second time, at which point it reclaims anyway and logs once: a
  persistent stall-shaped set is by then more likely a genuine mass
  failure than a database outage. A sharded deployment evaluates and
  suppresses each shard independently, so one stalled shard cannot pause
  reclaiming on a healthy sibling. If the shape probe itself fails, that
  shard's reclaim is skipped for the tick rather than proceeding
  unsuppressed — a slow or failing read over the very table about to be
  updated is itself stall-shaped, so "could not check" fails closed.

  Detection deliberately uses a **shorter** threshold than reclaim
  eligibility itself: gating suspicion on the same window `reclaimAfter()`
  reclaims at would miss a fleet stall lasting somewhat less than that
  window, because individual rows cross it staggered rather than all at
  once, and each gets reclaimed the instant it does — precisely the harm
  this exists to prevent. **Full protection holds for stalls up to about
  23s at the default `--heartbeat` (detection latency plus the reclaim
  window), degrading to none by about 33s** (one more reaper tick, the
  worst case for when the stall is first observed) — see
  `stallProtectionLower`/`stallProtectionUpper` in `cmd/cleat-worker`.
  Suppression is sticky once an episode opens: a tick where one worker's
  heartbeat lands first — un-suspecting the shape while its siblings are
  still individually stale — keeps suppressing on the same episode clock
  rather than releasing the laggards on that survivor's heartbeat alone.
  This protection is per-episode, not per-row: a reaper that never
  observed the stall's opening tick has no episode to be sticky about, and
  can still reclaim a laggard within about one heartbeat retry interval
  plus reconnect time of one worker's heartbeat landing while its
  siblings' have not — the gap between one worker's recovery and the rest
  is not itself modeled here.

  **Worst case, a genuinely dead worker's run now takes up to about 39s to
  reclaim at the default `--heartbeat`** (twice the ~14.5s reclaim window
  plus one reaper tick), up from that window alone, if its discovery
  happens to coincide with an unrelated fleet-wide stall being suppressed.
  New metric `cleat_suspected_db_stall_total`, labeled by shard, counts
  every tick this suppression fires. cleat#2006.

- **`ReapStaleInstances` truncated its reclaim timeout to whole seconds on
  all three dialects, halving #2166's 1s `reclaimSlack` at the default
  ~14.5s `--reclaim-timeout`** (PG: `"%d seconds"` over
  `int(timeout.Seconds())`; MySQL: `INTERVAL ? SECOND` over `int(...)`;
  MSSQL: `DATEADD(SECOND, ...)` over `int(...)`). At the default, a row
  actually became reclaimable at 14s rather than 14.5s; at `--heartbeat
  2.9s` (R=10.9s), only 0.1s of the documented slack remained. #2180 fixed
  the same truncation in `StaleSetShape` only, so the stall detector's
  `Stale` count (millisecond-precise) and the reap statement it feeds
  (second-truncated) could disagree about which rows were reclaimable, up
  to just under a second apart. Now millisecond-precise (microsecond on
  MySQL) on all three, matching `StaleSetShape`. cleat#2189.

- **A crash or rolling deploy between AwaitChild's (or AwaitPromise's, or
  AwaitAllChildren's) pending write and its completing write could
  permanently FAIL the parent with a step-N checksum mismatch on replay.**
  (cleat#2333)

  All three suspend on a step with a "pending" event holding nothing, then
  write a second, completing event to the SAME step once the real outcome
  is known. Every dialect's completing write was an unconditional
  INSERT/UPSERT with no guard distinguishing "complete this still-pending
  row" from "silently re-accept a stray duplicate" — so a retried pending
  write, replayed after the real completion had already landed and been
  checksummed, would overwrite it and desync the chain `VerifyWorkflowEvents`
  recomputes on every replay-with-history.

  Each dialect's completing write is now restricted to rows that are still
  genuinely pending (`event_type IN ('await_child','await_promise',
  'await_all_children')` and `response`/`error`/`promise_result`/
  `promise_error` all `NULL`): a `WHERE` clause on Postgres's
  `DO UPDATE`, per-column `IF()` guards on MySQL's
  `ON DUPLICATE KEY UPDATE` (which has no `WHERE`), and MSSQL's
  `INSERT ... SELECT` rewritten as a `MERGE` with a `WHEN MATCHED` guard.

  Two related gaps surfaced while fixing this and are fixed alongside it:
  MySQL's INSERT was missing `payload_encoding` from its column list
  outright, and none of the six completing-write sites ever transitioned
  `event_type` from `'await_promise'` to `'promise_resolved'`/
  `'promise_rejected'` — a pre-existing bug independent of the crash
  scenario, since `AwaitPromise`'s own replay branches on that column to
  decide whether to trust history or re-check the promise store. Fixing the
  transition also closes a residual raised in review: `PromiseResult`/
  `PromiseError` have no non-empty guarantee the way `AwaitChild`'s
  `Response` does, so a promise resolved or rejected with the empty string
  is column-for-column identical to a still-pending row — but once
  `event_type` correctly leaves the pending set, the row is immutable
  regardless of what those columns hold.

  `AwaitChild` has the same residual on its error side, with no comparable
  escape hatch: its `event_type` never transitions between pending and
  complete, both writes use `EventTypeAwaitChild`. `ForceFail` validates
  `workflowID`/`generation`/`operator`/`errorCode` but never `errorMsg`, so
  an operator can force-fail a child with an empty message; that reaches
  the completing write as `Err == ""`, column-identical on both `response`
  and `error` to the pending row it completes. A new `nonEmptyChildError`
  helper substitutes a sentinel message whenever a failed or dead-lettered
  child's error would otherwise be empty, closing the gap the same way
  `COALESCE(result, '{}')` already does for a done child's result.

- **`GET /oauth/{provider}/login` accepted any `?tenant_id=` from any host, so with
  `--require-host-match` set an anonymous caller could start a login against a tenant
  whose host they were not on.** (cleat#2340)

  cleat#2319 added `/login` to `pluginAuthExemptPatterns` so an anonymous browser could
  start a login at all: the auth middleware refused it with 401, because a browser
  arriving at `/login` has no cleat credential to present. That list is shared, and
  `auth.HostBindingMiddlewareWithMux` is handed it as its `publicPatterns`
  (`cmd/cleat-worker/main.go`) — where a listed route is a **skip**, not an additional
  check: host binding returns before it has looked at the request's Host. So exempting
  the route did not merely let an unauthenticated request through, it removed the host
  check from the one route that both chooses its own tenant from a request parameter and
  mints a credential.

  `handleLogin` now performs that comparison itself, against
  `plugin.Environment.HostResolver`, using the same `auth.NormalizeHost` the middleware
  uses. A tenant that does not own the request's host is refused with `400`, before any
  redirect is constructed. A worker with `--require-host-match` set but no resolver
  refuses with `500` rather than reading "cannot check" as "check passed", as does a
  resolver that returns an error. The check is conditional on `--require-host-match`,
  which defaults to off, so a deployment that does not set it behaves exactly as before.

  `/callback` deliberately does not carry this check: its tenant comes from the `state`
  row it looks up, not from a request parameter, so there is no caller-chosen tenant for
  a host check to disagree with — the gap was specifically `?tenant_id=`, which only
  `/login` takes. Binding the callback's `state` to the browser with a CSRF cookie is a
  separate, later piece of cleat#2340 and is not part of this change.

- **An abandoned OAuth login left its `oauth_sessions` row behind forever.** (cleat#2340)

  `handleLogin` writes a row when a login starts and `handleCallback` completes it. One
  that is never completed — the tab is closed, the identity provider refuses, the user
  returns after the 5-minute PKCE window — leaves a row whose `token_hash` is still
  `NULL`, and nothing in the plugin ever touched that row again. The row count is driven
  by how many logins are *started*, which no operator controls, so it grew without bound.

  A background sweep now deletes rows that are both `token_hash IS NULL` and past
  `expires_at`, every 5 minutes. `token_hash IS NULL` is the discriminator rather than
  `expires_at` alone, because a *completed* session's row also goes past its expiry once
  the session lapses, and that row must not be swept here: session expiry is enforced by
  reading `expires_at` on each request, not by deleting the row, which leaves a completed
  row readable after it expires for anything that later needs it. The sweep runs on all
  three dialects and is a no-op on MySQL and SQL Server, where `/login` refuses before a
  row is ever written.

### Changed

- **OAuth login is Postgres-only in this release: `/oauth/{provider}/login` and
  `/oauth/{provider}/callback` return `501` on MySQL and SQL Server.** (cleat#2340)

  A login mints a credential, and `auth.TenantStore.RevokeAPIKey` already refuses on
  non-Postgres — there is no revocation path there for a session token issued by a login
  (`auth.RevokeAPIKeyByHash` does not exist at all), so a credential minted on MySQL or
  SQL Server could not be withdrawn once issued. Rather than ship a login whose
  credentials cannot be revoked, both handlers refuse at their first statement and the
  worker logs the limitation once at startup.

  The refusal is per-request rather than at plugin `Init`: every bundled plugin
  initializes on every boot against one flat configuration with no reliable "am I
  configured" signal, so refusing at `Init` would stop every MySQL or SQL Server worker
  from starting at all, whether or not anyone there uses OAuth — the failure mode
  cleat#2202 hit with the email plugin.

  **Who is affected:** a MySQL or SQL Server deployment that uses OAuth login. Use API
  keys on those dialects for this release. Nothing else about such a deployment changes,
  and no other plugin's routes are affected.

## [0.2.0] - 2026-08-10

### UPGRADE NOTES — breaking

- **SQL Server 2022 is now the minimum.** `migrations/mssql/011` uses
  `ISJSON(payload, VALUE)`, whose second argument requires 2022, so that the
  payload columns accept the JSON scalars PostgreSQL and MySQL have always
  accepted — without it, `DeliverSignal` and `CreateUpdateRequest` failed on
  any SQL Server built from the shipped schema. `README.md` and
  `docs/reference/database-backends.md` previously said 2017+; nothing has ever
  tested below 2022.
- **`--require-signal-auth` now defaults to `false`.** It gates a check that
  reads `workflow_instances.allowed_signals`, and nothing in cleat can write
  that column — no store method, no API endpoint, no CLI verb, no SDK call. The
  check denies when the list is empty, so with the flag on by default every
  cross-workflow signal, every plugin-originated signal and every external HTTP
  signal was denied, and the documented remedy (add `"*"` to `allowed_signals`)
  could not be carried out. A deployment that wants the old behaviour can pass
  `--require-signal-auth=true`, but should know that it denies every signal.
  The default goes back to `true` when there is a way to populate the list.

- **A deploy no longer overwrites a workflow definition owned by another
  tenant; it fails instead.** `workflow_defs` is keyed by `(name, version)`
  with no tenant in the key, and all three backends upserted on that key — so
  the second tenant to deploy a given name silently replaced the first
  tenant's WASM bytes, and the first tenant's workflows then executed the
  second tenant's code. `DeployWorkflowDef` now records the deploying tenant
  and refuses to write over a definition that belongs to someone else,
  returning an error wrapping `engine.ErrWorkflowDefOwnedByAnotherTenant`.

  Who this breaks: a multi-tenant deployment in which two tenants deploy the
  same definition name. That previously "worked" in the sense that one row
  survived and served both; it now fails for whichever tenant does not own the
  name. If you were relying on one shared definition across tenants, deploy it
  as the default tenant (`00000000-0000-0000-0000-000000000000`) and do not
  redeploy it as a specific tenant — a definition owned by the default tenant
  stays readable by every tenant, which is what this table's PostgreSQL RLS
  policy has always allowed.

  What upgrades cleanly: every definition in an existing database is owned by
  the default tenant, because `PostgresStore` hardcoded that value and
  `MSSQLStore`'s `MERGE` omitted the column. Such a definition is *adopted* by
  the first tenant that redeploys it, so ordinary redeploys keep working and
  ownership takes effect from then on. Until a definition has been redeployed
  once, a tenant other than its creator can still take it over.

  This does not make two tenants able to hold the same name — that needs the
  tenant in the primary key, and with it three foreign keys per dialect. The
  name remains a global namespace; squatting one is now loud instead of
  silent. IMPROVEMENT-PLAN §3.12.

- **Workers now refuse to start on a PostgreSQL connection that bypasses
  row-level security.** Every tenant-scoped table has RLS enabled and FORCEd,
  and for `GetWorkflowByID` and `ListWorkflows` those policies are the only
  tenant isolation there is — neither carries an application-level `tenant_id`
  filter. PostgreSQL never applies RLS to a superuser, so a superuser
  connection returned every tenant's data from those calls. Every
  configuration previously shipped connected as one.

  With `--require-auth` (default true), a worker whose `--db` role is a
  superuser, has `BYPASSRLS`, or owns the tables without `FORCE` will now log
  the reason and exit rather than serve traffic it cannot isolate.

  To upgrade:

  1. Apply `migrations/postgres/005_app_role.sql`, which creates the
     `cleat_app` role and grants it what the engine needs — no ownership, no
     DDL.
  2. Give it a password: `ALTER ROLE cleat_app LOGIN PASSWORD '...';` The
     migration deliberately does not, so no credential lives in the
     repository. (`docker-compose.cluster.yml` does this from
     `CLEAT_APP_PASSWORD` via `deploy/postgres/900-app-role.sh`, but
     `docker-entrypoint-initdb.d` only runs on a *first* initialisation, so an
     existing deployment must run it by hand.)
  3. Point `--db` at `cleat_app`, and pass the previous owner DSN as
     `--migrate-db` — migrations need DDL rights that `cleat_app` does not
     have. `--migrate-db` defaults to `--db`.

  `--rls-check=off` restores the old behaviour for a single-tenant deployment
  that does not want this. `--rls-check=require` refuses regardless of
  `--require-auth`.

- **The root `schema.sql` has been deleted.** `migrations/postgres/` is the
  only schema source. The deleted file was a second, hand-maintained copy that
  had drifted into a strict subset: no `finalize_workflow_status` (which the
  engine calls on every workflow completion, with no fallback), no RLS
  policies, and no `admin.tenants`. A database built from it could not complete
  a workflow. Apply every file in `migrations/postgres/` in lexical order.

- **`docker-compose.cluster.yml` mount layout changed.** `migrations/postgres`
  is now mounted read-only at `/opt/cleat/migrations` and applied by
  `deploy/postgres/100-apply-migrations.sh`; `deploy/postgres` is
  `/docker-entrypoint-initdb.d`. Deployments that copied the old volume block
  must update it.

- **`DURABLE_TEST_DB` is renamed to `CLEAT_TEST_DB`.** The old name still works
  and warns.

- **`engine/testutil.TestDB` now fails instead of skipping** when a DSN is
  configured for its dialect but cannot be reached. Asking for a database and
  not getting one is a broken configuration, not an absent one — this is how a
  CI job stayed green for months without ever connecting. With no DSN
  configured it still skips.

### Added
- Engine test coverage for WASM backends (wasmtime, wazero) exceeding 40%
- Integration tests for MySQL and MSSQL store backends
- Unit tests for SignalWorkflow, DurableScheduleInvoke, and RegisterUpdateHandler
- WasmDiskCache and in-memory WASM cache unit tests
- CGO dispatch layer unit tests for component_cgo.go
- Engine streaming, deferral, dispatch, and callbacks unit tests
- Mock-DB coverage for WorkflowLoader DB methods
- Multi-backend coverage for MySQL and MSSQL store methods
- Concurrency key and shard error-path tests for ShardedStore
- WASM lock, memory, and scan unit tests
- Plugin loader, migration, events, credentials, and encryption tests
- Engine/app.go comprehensive test suite (22 functions, 100% coverage)
- QueryBuilder and Dialect SQL helper unit tests
- Regression tests for critical-path PostgresStore methods

### Changed
- Project renamed from "durable" to "cleat" across the codebase

### Removed
- TinyGo support for compiling Go workflows to WASM. TinyGo is an
  embedded-systems toolchain and lacked the standard library coverage this
  project needs. The standard Go toolchain targeting `wasip1` (`--target go`)
  is now the only supported way to compile Go workflows to WASM.

### Fixed
- Multi-database CI failures in MySQL and MSSQL integration tests
- Tenant isolation: restore tenant_id filter on GetWorkflowByID
- JSONB handling in PostgreSQL store
- All 54 MySQL integration tests now pass
- MSSQL test container configuration to avoid MCR pull block
- Error message quality improvements for CLI, worker, and engine
- Race condition fixes for concurrent workflow execution
- Wasmtime memory buffer and signal handling fixes
- Plugin test failures in manifest and eventtriggers
- Auth middleware and tenant store test repairs
- Project root detection in engine tests
- ABIVersion type comparison in mssql_store_test.go
- Expand fake SQL driver coverage for admin-prefixed queries
- Corrected CompleteWorkflow status assertion from "completed" to "done"
- MySQL test schema TEXT to VARCHAR for assigned_to column
- **`CreateUpdateRequest` rejected ordinary payloads.** A non-JSON update
  payload failed outright on MySQL and SQL Server, and one containing a quote
  or a backslash failed on all three — `workflow_update_requests.payload` never
  received the JSON encoding signals got in the same fix. Both the encode and
  the decode are now shared, so every dialect stores and returns what the
  caller passed in.
- **Workflows could not complete on SQL Server.** `json.Marshal` of a nil map
  returns `null`, not nil, so a workflow with no query handlers wrote the JSON
  value `null` into `query_state` — which PostgreSQL and MySQL accept and the
  shipped SQL Server schema rejects with
  `CHECK (ISJSON(query_state) = 1)`. `CompleteWorkflow`, `FailWorkflow` and
  `ContinueAsNew` all failed there. On the other two dialects the query state
  was silently stored as `null` rather than `{}`.
- **`CreateSchedule` could not create a schedule on SQL Server.**
  `json.RawMessage` binds as `VARBINARY`, so `workflow_schedules.input` was
  written as the binary rendering of the JSON, and the shipped schema's
  `CHECK (ISJSON(input) = 1)` rejected the row. Every scheduled workflow on a
  SQL Server built from `migrations/mssql/001_schema.sql` failed to be
  created. The test schema declared no such constraint, which is why the suite
  never showed it.
- **No `cleat-worker` could start against MySQL either.** The migration runner
  split each file on every `;`, including semicolons inside comments and inside
  stored-procedure bodies, so neither shipped MySQL file could be applied:
  `001_schema.sql` failed with `Error 1064` on a semicolon in a comment, and
  `003_procedures.sql` — which creates `finalize_workflow_status`, the
  procedure the engine calls on every workflow completion with no fallback —
  was cut into fragments and its `DELIMITER` directive sent to the server. A
  worker pointed at a MySQL database whose schema had not been built by hand
  logged the error and exited. Statement splitting is now comment-, string- and
  `DELIMITER`-aware, and `multi-db-ci.yml` runs the migration tests against
  live MySQL and SQL Server.
- No `cleat-worker` could start against PostgreSQL: a session-scoped
  `SET search_path` in the migration files broke the migration runner's own
  bookkeeping, and concurrent workers raced each other's DDL at boot. Both
  migration runners now hold an advisory lock, and the core runner
  schema-qualifies its tracking table.
- The shipped schema created its objects in a schema named after the
  connecting role rather than `public`, because `POSTGRES_USER=cleat` collides
  with a schema `001_schema.sql` creates.
- `ContinueAsNew` had never worked on PostgreSQL (an INSERT listed nine columns
  and supplied eight values).
- `AssignedTo` was overwritten with an empty string on every claim.
- `tenant_id` was not written by `CreatePromise`, `DeliverSignal` or
  `CreateSchedule`, so those rows were invisible to the tenant that created
  them.
- `PollSignal` deleted the signals it read, contradicting its own contract and
  both other backends. It is on the live signaller path.
- The auto-generated startup API key was never created on any PostgreSQL
  deployment: the count query named `tenant_api_keys` unqualified while the
  table is `admin.tenant_api_keys`. With `--require-auth` defaulting to true, a
  fresh cluster had no key and no way in.
- `cleat build` chose its output `.wasm` filename at random, because entry
  points were read from a map in iteration order.
- The `kvstore` and `feature-flags` plugins did not work on MySQL or SQL
  Server: unquoted `key` (a reserved word in both), no `LIMIT` on SQL Server,
  and JSON columns that could be neither read nor written there.

## [0.1.0] - 2026-05-13

### Added
- Durable execution engine with deterministic replay model
- Multi-database support: PostgreSQL, MySQL, SQL Server (MSSQL)
- WASM compilation pipeline (Go to wasip1, TinyGo support)
- 22 built-in plugins (blobstore, event-triggers, feature-flags, kafka-connect,
  llm, notifications, pagerduty-alert, slack-notify, webhook-ingest, and more)
- 5 language SDKs: Go, Rust, Python, Java, AssemblyScript
- Svelte 5 admin dashboard with embedded web UI
- CLI toolchain: build, vet, deploy, versions, schedule
- Prometheus metrics endpoint and OpenTelemetry tracing support
- wazero WASM runtime (pure Go, no CGo required)
- PostgreSQL-backed work queue, timer service, and blob store
- Deterministic replay from event history (Temporal-style model)
- Saga pattern with compensating transactions
- Durable promises for cross-workflow coordination
- Virtual Object (entity workflow) pattern for stateful actors
- Continue-as-new for workflow history compaction
- Heartbeat support for long-running operations
- Server-side retry with configurable backoff policy
- Signal patterns (fire-and-forget, request-response, polling)
- Update handler pattern (bi-directional RPC with validation)
- Query handlers for read-only workflow state inspection
- Sharded store for horizontal scalability
- Tenant isolation with schema-per-tenant support
- Workflow versioning with minimum version support

[Unreleased]: https://github.com/cleat-team/cleat/compare/v0.2.0...HEAD
[0.2.0]: https://github.com/cleat-team/cleat/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/cleat-team/cleat/releases/tag/v0.1.0
