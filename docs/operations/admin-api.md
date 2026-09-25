# The admin API (`--enable-admin-api`)

Every route under `/api/admin/` is **off by default**. While `--enable-admin-api` is unset, each of them
answers `404 {"error":"not found"}`, exactly as an unregistered `/api/` path does, so a caller cannot tell a
gated route from one that does not exist.

## Read this before you turn it on

cleat has no operator credential yet (tracked in cleat#2169). The admin routes are authenticated like every
other route, with an ordinary tenant API key, and **the routes that act on the worker do not check whose key
it is**. So while the flag is on:

- **any authenticated API key of any tenant can drain the worker** (`POST /api/admin/drain`), which takes it
  out of rotation (`/readyz` answers 503 `draining`) and, once its in-flight work finishes, stops it;
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

| `adminApi.enabled` | the pod's `preStop` hook | at shutdown |
|---|---|---|
| `false` (default) | `sleep` for `worker.preStopSleepSeconds` (5), so the pod leaves Service endpoints before SIGTERM | SIGTERM cancels the worker's context. A run waiting inside a durable call aborts, and another worker picks the run up after the reclaim window (`--reclaim-timeout`). Runs are durable, so nothing is lost, but they resume later than a drain would have let them finish |
| `true`, with `auth.adminApiKey` or `auth.existingSecret` | `POST /api/admin/drain` with that key: the worker stops claiming and finishes its in-flight runs for as long as `terminationGracePeriodSeconds` allows | as above, for whatever is still running when the grace period ends |
| `true`, no key | the same `sleep`: the drain call cannot authenticate | as in the first row |

Set `adminApi.enabled` only where every API key you have issued is trusted to drain workers and to run the
all-tenant retention sweep.

## Adding an admin route

Register it in `registerRoutes` through `api.adminAPIOnly(...)`.
`TestEveryAdminRouteIsAbsentUntilTheAdminAPIIsEnabled` reads the registrations and fails on a
`/api/admin/` route that is not wrapped, and on one registered anywhere else. Say in the table above whether
it is worker-level or tenant-scoped.
