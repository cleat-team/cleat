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

The id-only tenant-scoped routes are checked by `callerOwnsTarget` before anything runs, and answer 404 (never
403) for another tenant's workflow, so they do not confirm it exists. They sit behind the flag because they are
destructive operator actions, not because they cross tenants — the tenant-named form below is the one that
crosses, and only for an operator.

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
