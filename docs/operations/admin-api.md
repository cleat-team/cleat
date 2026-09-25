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

The chart's `preStop` hook drains the pod through this route. When `auth.adminApiKey` or `auth.existingSecret`
is set (the hook cannot authenticate without one), the chart therefore passes `--enable-admin-api`. With
neither set it does not, the hook's call answers 404, and the pod stops on SIGTERM without a drain, which is
what already happened when the key was missing. See `charts/cleat/values.yaml`.

## Adding an admin route

Register it in `registerRoutes` through `api.adminAPIOnly(...)`.
`TestEveryAdminRouteIsAbsentUntilTheAdminAPIIsEnabled` reads the registrations and fails on a
`/api/admin/` route that is not wrapped, and on one registered anywhere else. Say in the table above whether
it is worker-level or tenant-scoped.
