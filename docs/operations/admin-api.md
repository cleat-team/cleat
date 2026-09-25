# The admin API (`--enable-admin-api`)

Every route under `/api/admin/` is **off by default**. While `--enable-admin-api` is unset, each of them
answers `404 {"error":"not found"}`, exactly as an unregistered `/api/` path does, so a caller cannot tell a
gated route from one that does not exist.

## Read this before you turn it on

cleat has no operator credential yet (tracked in cleat#2169). The admin routes are authenticated like every
other route, with an ordinary tenant API key, and **the routes that act on the worker do not check whose key
it is**. So while the flag is on:

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

The tenant-scoped routes are checked by `callerOwnsTarget` before anything runs, and answer 404 (never 403)
for another tenant's workflow, so they do not confirm it exists. They sit behind the flag because they are
destructive operator actions, not because they cross tenants.

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
