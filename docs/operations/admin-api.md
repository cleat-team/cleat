# The admin API (`--enable-admin-api`)

Every route under `/api/admin/` is **off by default**. While `--enable-admin-api` is unset, each of them
answers `404 {"error":"not found"}`, exactly as an unregistered `/api/` path does, so a caller cannot tell a
gated route from one that does not exist.

## Read this before you turn it on

cleat has an operator credential — a `cleat_op_…` key, distinct from a tenant key, which resolves to no tenant and
is confined to `/api/admin/*` (cleat#2169, delivered in two parts). It changes what the **tenant-scoped** instance
routes can reach, and nothing at all about the worker-level ones. **The routes that act on the worker do not check
whose key it is**: they are not tenant-scoped, so an operator credential confers nothing on them and any
authenticated key reaches them just as it always did. So while the flag is on:

- **any authenticated API key of any tenant can drain the worker** (`POST /api/admin/drain`), which takes it
  out of rotation (`/readyz` answers 503 `draining`); it stops claiming and keeps running until SIGTERM;
- **any authenticated key can read this worker's detailed health** (`GET /api/admin/health`): stale loops,
  the last database error, and other plugins' health messages;
- **any authenticated key can trigger a retention sweep** (`POST /api/admin/retention/sweep`), which runs the
  worker's own retention over every tenant's data, with a cutoff the caller chooses (`older_than`). It does
  not enable a retention arm that is switched off, but for an arm that is on, a short `older_than` deletes
  every tenant's completed history older than that.

With `--require-auth=false` the same routes are open to everyone who can reach the port. The worker logs a
warning at startup that says so. Enable the flag on a deployment whose keys you trust to this degree, keep
the port off the public network, and leave it off otherwise.

## Which routes are worker-level and which are tenant-scoped

Audited 2026-09-24 from `cmd/cleat-worker/app.go`. The scope column is the property that matters:

| Route | What it acts on | Scope |
|---|---|---|
| `POST`, `GET /api/admin/drain` | this worker's claim loop and lifetime | **worker-level**: any key, any tenant |
| `GET /api/admin/health` | this worker's loops, database error text and every plugin's health message | **worker-level**: any key, any tenant; unreadable during a database outage (its key lookup needs the database) |
| `POST /api/admin/retention/sweep` | retention over every tenant's data on this worker's store | **worker-level**: any key, any tenant |
| `POST /api/admin/instances/{id}/force-complete` | one workflow | tenant-scoped: 404 unless the caller's tenant owns `{id}` |
| `POST /api/admin/instances/{id}/force-fail` | one workflow | tenant-scoped |
| `POST /api/admin/instances/{id}/re-replay` | one workflow | tenant-scoped |
| `POST /api/admin/instances/{id}/steps/{n}/resolve` | one workflow's step | tenant-scoped |
| `POST /api/admin/tenants/{tenant}/instances/{id}/force-complete` | one workflow, in the named tenant | tenant-scoped, **and cross-tenant for an operator** |
| `POST /api/admin/tenants/{tenant}/instances/{id}/force-fail` | one workflow, in the named tenant | as above |
| `POST /api/admin/tenants/{tenant}/instances/{id}/re-replay` | one workflow, in the named tenant | as above |
| `POST /api/admin/tenants/{tenant}/instances/{id}/steps/{n}/resolve` | one workflow's step, in the named tenant | as above |
| `POST /api/admin/tenants` | creates a tenant (and its org, by name) and mints its first key | **operator credential only**: a tenant key gets `403` |
| `POST /api/admin/tenants/{tenant}/confirm` with `{"key_id": …}` | makes that key permanent | **operator credential only**, and only the principal that created the tenant |

The id-only tenant-scoped routes are checked by `callerOwnsTarget` before anything runs, and answer 404 (never
403) for another tenant's workflow, so they do not confirm it exists. They sit behind the flag because they are
destructive operator actions, not because they cross tenants — the tenant-named form below is the one that
crosses, and only for an operator.

## Creating a tenant

`POST /api/admin/tenants` creates a tenant, and `POST /api/admin/tenants/{tenant}/confirm` ends the handshake.
Both need an **operator credential** (a tenant key is refused with `403`, because a tenant key reaches
`/api/admin/*` when this flag is on and must not create tenants), and both answer `501` on a worker that cannot
provision: MySQL, which is single-tenant, or a sharded worker.

A service that only creates tenants should hold a **provision-scoped** operator key, not an admin one:

```bash
cleatctl operator-key create --scope provision --principal clew-service --description "tenant provisioning"
```

That key reaches exactly these two routes, and nothing else under `/api/admin/` (drain, force-complete and the
rest are `403`). The `principal` is a name you choose; rotating the key keeps the principal, so it keeps
authority over the tenants it created.

```bash
curl -X POST -H "Authorization: Bearer $PROVISION_KEY" -H 'Content-Type: application/json' \
  http://worker:8080/api/admin/tenants \
  -d '{"tenant_id":"<uuid, optional>","display_name":"Acme","org_name":"Acme Org","key":{"description":"first key"}}'
```

| Field | |
|---|---|
| `tenant_id` | optional. A UUID **you** choose makes the create idempotent. The nil UUID is refused (it is the default tenant). Omitted, one is generated. |
| `name` | optional, unique across the whole deployment (not per org). Omitted, it is the tenant id. |
| `display_name` | optional, not part of identity. |
| `org_id` **or** `org_name` | exactly one. `org_name` finds the org or creates it. |
| `key.description` | optional label for the first key. |

**The key is provisional until you confirm it.** The create returns `201` with a key that authenticates
immediately and **lapses after 15 minutes** (`valid_for_seconds`). Store it durably, then
`POST /api/admin/tenants/{tenant}/confirm` with `{"key_id": "<the key_id the create returned>"}`: that key becomes
permanent. **Confirm names the key**, because a repeated create can replace it: if a retry and a stalled original both
reach the worker, the second is issued a new key and the first is revoked, and a confirm that took no key id would
make permanent whichever one was live, possibly one nobody holds. A confirm of a replaced key is `409`. If the response is lost, or you crash
before storing, do nothing: the key lapses on its own, and repeating the same create returns `200` with a **fresh**
key (the previous provisional one is revoked). Once a key is confirmed, repeating the create returns `200` with the
tenant and **no key**, so a replay can never be used to obtain a second working key.

| Request | Result |
|---|---|
| id free, name free | `201`, the tenant and a provisional key |
| id exists, same name and org, created by this principal, no confirmed key | `200`, a fresh provisional key |
| the same, with a confirmed key | `200`, no key |
| id exists with a different name or org | `409 tenant_differs`, with `fields` naming which |
| id exists and **another principal** created it, or it was made offline | `409 tenant_exists`, with nothing about the tenant |
| name held by another tenant | `409 name_taken` |
| `org_id` names no org | `404` |
| confirm, the named key lapsed, was replaced by a repeat, or is not this tenant's | `409 no_provisional_key`: repeat the create |
| confirm, not this principal's tenant | `404` |
| confirm without a `key_id`, or a malformed one | `400` |

A tenant made offline (`cleat-worker --create-tenant`) has no recorded creator, so no provisioner can replay or
confirm it: this route can only ever mint a key for a tenant it created. The creation is audited on the new
tenant's own chain, with the principal as the subject.

### From Go: `backendkit`

`backendkit` wraps both calls, and the handshake's ordering is yours to keep: **store the key, then confirm it.**

```go
admin := backendkit.NewProvisioner(backendkit.New(workerURL, backendkit.WithAPIKey(provisionKey)))

// TenantID is a UUID you choose, and it is required: it is what makes a retry safe.
tenant, key, err := admin.EnsureTenant(ctx, backendkit.TenantRequest{TenantID: id, OrgName: "Acme Org"})
switch {
case errors.Is(err, backendkit.ErrTenantExists), errors.Is(err, backendkit.ErrNameTaken):
    return err // someone else's tenant, or a taken name: repeating it unchanged cannot help
case err != nil:
    return err // anything else: repeating the same call is safe
}
if key != nil { // nil once a key has been confirmed
    if err := saveKey(ctx, tenant.ID, key.Secret); err != nil { // durably, before confirming
        return err
    }
    if err := admin.ConfirmKey(ctx, tenant.ID, key.KeyID); err != nil {
        return err
    }
}

// Then act for the tenant with its own key. The library keeps no keys: lookupKey is yours.
clients := &backendkit.TenantClients{Base: backendkit.New(workerURL), KeyResolver: lookupKey}
c, err := clients.For(ctx, tenant.ID)
```

A `FirstKey` is redacted when it is printed, logged with `slog` or JSON-encoded, so read `key.Secret` to store it and never an encoded copy. The type's doc says what that does not cover: `fmt` cannot call a method on an unexported struct field, so a `FirstKey` held in one still prints its secret under `%v`. Each refusal in the table above has a sentinel (`ErrTenantExists`, `ErrNameTaken`, `ErrTenantDiffers` with the differing fields in a `*TenantDiffersError`, `ErrOrgNotFound`, `ErrNoProvisionalKey`, `ErrProvisioningUnavailable`), wrapped beside the `*UpstreamStatusError`. A **confirm's `404` has no sentinel**: the worker answers it identically for a missing tenant, a tenant this credential did not create, and a worker with the admin API off, so the client cannot say which.

## Rotating a tenant's own key (`/api/keys`)

A tenant manages its own API keys with its own key. These routes are **not** under `/api/admin/` and do not need
`--enable-admin-api`; they need authentication (`--require-auth`, the default), because a tenant that the worker
did not authenticate has no identity to rotate. With authentication off they answer `403`. An **operator** key is
refused with `403` here too: an operator has no tenant, so there is no key set for it to manage.

| Request | Result |
|---|---|
| `POST /api/keys` with `{"description"?, "expires_at"?}` | `201` with the key's metadata and its **`secret`, shown once**, `Cache-Control: no-store`. `expires_at` is RFC 3339 and must be in the future. The key is permanent from the start (there is no handshake). |
| `GET /api/keys` | `200 {"keys": [...]}`, newest first: `key_id`, `description`, `created_at`, `expires_at`, `disabled_at`, `provisional` and a `status` of `active`, `provisional`, `expired` or `revoked`. Never a secret or a hash. |
| `DELETE /api/keys/{key_id}` | `200 {"key_id", "revoked": true, "already_revoked"}`. A key id that is not this tenant's is `404`; revoking a revoked key is `200` with `already_revoked`. |
| a revoke that would leave the tenant with **no live key** | `409 last_key`. Repeat with `?force=true` to do it anyway. |
| a mint when the tenant holds 25 live keys | `409 key_limit`: revoke one first. |
| a malformed body, an unknown field, trailing data, a past `expires_at`, a `key_id` that is not a UUID, a `force` that is not a boolean | `400`; a body over 16 KiB is `413`. |

To rotate: mint the replacement **with the old key**, store it, check it works, then revoke the old key **with
the new one**. The old key stops authenticating at once. The tenant is always the one the credential proves; there
is no way to name another in the request.

The last-key rule serialises on the tenant's row, so two concurrent revokes of a tenant's last two keys leave
exactly one standing (measured: without the lock both succeed, on PostgreSQL and on SQL Server). It protects
against a mistake, not against a determined caller: `?force=true` removes the last key, after which only
`cleat-worker --generate-api-key` (offline) or a new operator-provisioned key restores access.

## Suspending and resuming a tenant

`POST /api/admin/tenants/{tenant}/suspend` and `POST /api/admin/tenants/{tenant}/resume` set and clear a tenant's
`suspended` flag. A suspended tenant may not start new workflows; `cleatctl suspend-tenant` documents exactly what
else it does and does not stop (runs already executing are not stopped). Suspending deletes nothing, and resume
undoes it. Both answer `200 {"tenant_id": "...", "suspended": true|false}` and are idempotent. They exist because
dropping a tenant (a later step) requires the tenant to be suspended first, and a service that creates tenants must
be able to take that step for the ones it made without holding a key that can suspend anyone's.

**What suspension does to work that arrives while it lasts.** A start by the API is refused with a `403`. A start by a
plugin on the tenant's behalf (a cron schedule, an event trigger, a job-queue dispatch) is refused too, and what it
was going to start is **dropped, not deferred**: resuming the tenant does not release a backlog of firings that
came due while it was suspended. Runs that were already executing finish, and reads are unaffected (cleat#3594).

| Caller | `suspend` | `resume` |
|---|---|---|
| admin operator key | **any** tenant, `200` | **any** tenant, `200` |
| provision operator key | only a tenant **its principal created**, `200`. Any other tenant, one made offline, and one that does not exist are all `404` with the same body | `403`: **never** |
| tenant key | `403` | `403` |

**A provision key can suspend and can never resume**, and that is deliberate. The flag is one bit and does not record
who set it. An admin suspends a tenant for non-payment, the trial-expiry sweep suspends one, `cleatctl suspend-tenant`
suspends one: if the service that created the tenant could clear the flag it could lift any of them, merely because
it created the tenant. To undo a provisioner's own suspension, use an admin key. (Recording who suspended was
considered and rejected for now: every writer of the flag would have to keep that record true, and a stale record
would reopen the hole.) Suspending a tenant an admin has already suspended succeeds and changes nothing.

A tenant key reaches `/api/admin/*` when the flag is on, and must not suspend a tenant, so the handler refuses it
itself. A provision key reaches `suspend` only with the tenant id in its one canonical spelling (lower-case,
hyphenated) and only with `POST`; any other spelling is `403` from the scope. Like provisioning, these routes answer
`501` on MySQL and on a sharded worker. The change is audited on the tenant's own chain, with the operator as the
subject.

## Acting on another tenant

`/api/admin/tenants/{tenant}/instances/{id}/…` is the same four operations addressed at an explicit tenant.
That form is what an operator credential is for:

| Caller | May name | Result |
|---|---|---|
| `cleat_op_…` operator credential | **any** tenant | the operation runs against the named tenant. An operator has no tenant, so the id-only routes above refuse it with `401` — that is deliberate, and the cross-tenant capability exists only where the URL says which tenant. |
| tenant API key | **its own tenant only** | naming another tenant is `404`, not `403` (a `403` would confirm the tenant and the workflow exist). Naming its own tenant is identical to the id-only route. |
| **no identity at all**, with `--require-auth=false` | **nothing** | `401`. This is the one row where the named form differs from the id-only one, and the difference is the point: the id-only route falls back to the process-wide store, which is **one constant scope** no request can redirect, so serving an identity-less request there is safe. This route opens a store for whatever the URL names, so the same fall-back would let an unauthenticated caller reach **any** tenant. Naming a tenant requires something to name it. |

**Measured before that refusal existed**, with `--require-auth=false` on an identity-less request: naming another tenant opened that tenant's store and force-completed its workflow, `200 {"status":"completed"}`, audited as `operator=unknown`. The id-only route's posture under the same flag is unchanged.

**The tenant is in the path rather than looked up from the workflow id, and that is a security property rather
than a style choice.** Resolving it would need a read spanning tenants, which the store has no way to make:
PostgreSQL scopes by row-level security on `cleat.tenant_id`, MySQL by the tenant's own database, SQL Server by
its tenant predicates. Naming the tenant makes the **scope** the authorization — the store is opened for the
named tenant, so a workflow that tenant does not own is not visible at all and the caller gets a `404`. There
is no second comparison to keep in step with the scope, and no new cross-tenant read to get wrong.

A `{tenant}` that is not a UUID is `400`: it is bound to `cleat.tenant_id`, where a non-UUID is a cast error
raised by the policy rather than a clean refusal.

**Not included, on purpose:** tenant-level roles (`admin` vs `member`). The operator credential is one axis of
authorization and tenant roles are another; #2047 decision 2A still lets any authenticated tenant caller export
and verify its own audit log.

Routes outside `/api/admin/` (`/api/workflows`, `/api/definitions`, `/api/versions/...`, and the rest) are the
ordinary tenant API and are not affected by this flag.

## Calling them

```bash
cleat-worker --db "$DATABASE_URL" --enable-admin-api ...

curl -X POST -H "Authorization: Bearer $CLEAT_API_KEY" http://worker:8080/api/admin/drain
```

## The Helm chart

`adminApi.enabled` (default `false`) is what passes `--enable-admin-api`. It is a separate value on purpose:
a drain key being configured does not turn on a route that lets any tenant drain workers.

| `adminApi.enabled` | the pod's `preStop` hook |
|---|---|
| `false` (default) | `sleep` for `worker.preStopSleepSeconds` (5), so the pod leaves Service endpoints before SIGTERM arrives |
| `true`, with `auth.adminApiKey` or `auth.existingSecret` | `POST /api/admin/drain` with that key |
| `true`, no key | the same `sleep`: the drain call cannot authenticate |

The kubelet sends SIGTERM as soon as the `preStop` command returns, and the drain hook returns when the POST is
answered (202), not when the drain is complete. What lets a run in flight finish is the worker's own SIGTERM
handling: it drains for `--shutdown-grace` (chart: `worker.shutdownGrace`, 20s) before it cancels anything. The
pod's `terminationGracePeriodSeconds` (chart: `worker.terminationGracePeriodSeconds`, 60) has to outlast the
`preStop` command plus that drain, and a template test fails if it does not. See
[zero-downtime-deploy.md](zero-downtime-deploy.md#what-sigterm-does) for what happens to a run that outlasts the grace.

Set `adminApi.enabled` only where every API key you have issued is trusted to drain workers and to run the
all-tenant retention sweep.

## `GET /api/admin/drain` is read-only, and `POST` is a cordon

`POST` stops the worker claiming and reports `/readyz` 503 `draining`. It does not stop the process, so a
Kubernetes container is not restarted (and does not resume claiming) because something called it. `GET` reports
progress, and `complete` once nothing is in flight; it changes nothing. Before cleat#2285 the first `GET` after
the last run finished also cancelled the worker, so a status poll from any authenticated key stopped a worker
that had been asked to drain, and a drain nobody polled never finished. What ends a process now is SIGTERM.

## Adding an admin route

Register it in `registerRoutes` through `api.adminAPIOnly(...)`.
`TestEveryAdminRouteIsAbsentUntilTheAdminAPIIsEnabled` reads the registrations and fails on a
`/api/admin/` route that is not wrapped, and on one registered anywhere else. Say in the table above whether
it is worker-level or tenant-scoped.
