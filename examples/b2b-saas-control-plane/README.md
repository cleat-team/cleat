# B2B SaaS control plane

A durable tenant-provisioning workflow that runs **as the tenant it
provisions**, recording every milestone to that tenant's own audit trail —
plus the background sweep that suspends a tenant whose trial has expired.

This is the reference implementation behind cleat#2534/#2681: the fourth of
cleat's playbook scenarios, and the one built specifically to exercise all
four of the "hitch points" a plugin-backed SaaS control plane needs —
a durable workflow, a tenant-scoped HTTP control surface, a host function
that records an audit event, and a background sweep — using capabilities
that already shipped in three earlier stages (cleat#2590, #2616, #2683)
rather than inventing new ones for this example alone.

## It runs, and that is the point

`scripts/run-b2b-saas-control-plane-scenario.sh` deploys this workflow to a
real `cleat-worker` on a real PostgreSQL, signs up two tenants, provisions
one successfully and fails the other on purpose, backdates a trial and
waits for the real background sweep to suspend the tenant it belongs to,
then confirms that tenant's own key can no longer start a run. It runs on
every pull request. See [Which dialects this runs on](#which-dialects-this-runs-on)
for why there is only one arm.

## Which tenant does this run as, and why that is the whole design

**The provisioning workflow runs as the tenant it provisions, never as the
operator.** Concretely: the backend creates the tenant and mints its API
key *before* starting any run, then starts `ProvisionTenant` using that
tenant's own freshly-minted credential — never its own. This is load-bearing
rather than a detail:

- `cmd/cleat-worker`'s `POST /api/workflows/:name/start` resolves the run's
  tenant from the API key that authenticated the request
  (`apiServer.scopedStore`, `cmd/cleat-worker/server.go`) — there is no
  other way to say "this run belongs to tenant B." Starting the run under
  the operator's own key would attribute every `record_event` call this
  workflow makes to the *operator's* audit chain, not the new tenant's —
  silently wrong in a way nothing would flag, since the calls would still
  succeed.
- The one thing a workflow genuinely cannot do for itself is create the
  tenant it is about to run as — see the next section.

## Tenant creation is CLI-only, and the backend shells out to it

`auth.TenantStore.CreateTenant` has no plugin grant and no HTTP surface
anywhere in cleat. That is a deliberate decision from cleat#2534 Stage 1's
own review, not an oversight: under `--tenant-isolation=role`, a plugin
grant wrapping `CreateTenant` on the worker's own (necessarily
non-superuser) runtime connection would create tenants no worker could
actually serve, because provisioning a tenant's login role needs
`CREATEROLE`, which that connection deliberately does not have. The
sanctioned surface is `cleat-worker --create-tenant`/`--generate-api-key`,
run by an operator (or an operator's own automation) with its own
privileged database connection.

`backend/main.go` **shells out to that exact binary** rather than importing
`auth.TenantStore` directly, for two reasons, not one:

1. `auth/` is an internal package of the root module. `examples/` is its
   own, separate Go module — `go.work` is what lets `go build` resolve it
   *inside this repository*; a copy of this directory taken outside it
   could not. Every other example's backend imports only `cleat/backendkit`,
   the public SDK, for exactly this reason.
2. Calling the same binary an operator would run — rather than a second Go
   implementation of what it does — is what keeps the dialect gate and the
   role-provisioning logic from having two places to drift apart in.

`cleat-worker --create-tenant`/`--generate-api-key` have no `--json` or
machine-readable output form; the backend parses their human-readable
`=== CLEAT ... ===` banner for the one field it needs (`tenantAdmin.run` in
`backend/main.go`). This is the awkward, real integration point — the same
honesty `examples/order-lifecycle/README.md` gives its own webhook-source
setup step, for the same reason: the sanctioned surface prints for a human,
and there is no other one to call instead.

## Build

```bash
cleat build -o /tmp/out ./examples/b2b-saas-control-plane/
```

The artifact is named for the entry point, not the directory:
`provision_tenant.wasm` — confirmed by running `cleat build`, not assumed;
see `cleat.yaml`'s own comment, and `examples/order-lifecycle/README.md` for
the same rule stated against `place_order.wasm`.

## Run it against a real worker

```bash
# 1. The database and the worker. PostgreSQL only -- see below.
docker compose up -d
docker compose logs cleat-worker | grep -i 'Key:'   # informational; this scenario mints its own keys

# 2. Deploy the compiled workflow.
cleat deploy --db "postgres://cleat:cleat@localhost:5432/cleat?sslmode=disable" \
  --name b2b-saas-control-plane /tmp/out/provision_tenant.wasm

# 3. One org, once per deployment -- cleat-worker --create-tenant requires one.
go build -o .bin/cleat-worker ./cmd/cleat-worker
.bin/cleat-worker --create-org b2b-demo \
  --db "postgres://cleat:cleat@localhost:5432/cleat?sslmode=disable"
# -> Org ID: <ORG_ID>

# 4. The backend. It creates tenants and mints their keys via the binary
#    built above, then re-deploys step 2's wasm under each new tenant's own
#    id -- workflow_defs is per-tenant (see backend/main.go's
#    deployWorkflowDef), so a freshly signed-up tenant has none of its own
#    until this happens.
CLEAT_URL=http://localhost:8080 \
CLEAT_ADMIN_DB_URL="postgres://cleat:cleat@localhost:5432/cleat?sslmode=disable" \
CLEAT_ORG_ID=<ORG_ID> \
CLEAT_WORKER_BIN=.bin/cleat-worker \
CLEAT_WASM_PATH=/tmp/out/provision_tenant.wasm \
  go run ./backend
```

Open http://localhost:9091 and sign up a business. The response — shown
**exactly once** — carries the tenant's id and its API key; cleat stores
only a hash of a key, so there is no "show it to me again" surface, here or
anywhere in cleat.

### Which dialects this runs on

**PostgreSQL only, and it is not an omission.** `cleat-worker --create-tenant`
refuses outright on MySQL and SQL Server — `auth.CreateTenant`'s own dialect
gate — so there is no second arm to run: the backend's signup path, which
every part of this scenario depends on, simply does not exist on those two
dialects. Compare `examples/order-lifecycle/README.md`, whose saga
assertions genuinely are dialect-independent and run on all three; this
scenario's *provisioning* path is not, by construction, one level below the
workflow engine itself.

The cross-tenant isolation this scenario's audit trail depends on **is**
tested on the dialect that matters, exhaustively, elsewhere:
`plugins/auditlog/audit_rows_are_scoped_to_their_tenant_test.go`'s
`TestAuditRowsAreScopedToTheirTenant` connects as a role that cannot bypass
row-level security — the whole point of that test, stated in its own
comment — and proves a tenant-scoped read sees exactly its own tenant's
rows, across four separate failure-mode arms. Re-deriving that here, against
one running worker process, would be strictly weaker than the test that
already exists; this scenario's own script cites it rather than duplicating
it, and instead exercises the property that test cannot see:
`GET /api/tenant/lifecycle` never accepts a tenant id as an argument at all
(it is resolved from the caller's own API key,
`plugins/tenantlifecycle/routes.go`), so there is no cross-tenant call to
even attempt through this route.

**Known gap, filed rather than silently left:** `audit_events` is described
as carrying a row-level security policy on SQL Server too (cleat#1552), but
no dedicated regression test exercises it there the way
`TestAuditRowsAreScopedToTheirTenant` does for PostgreSQL — see cleat#2714
for tracking. This scenario does not depend on it (it is PostgreSQL-only
for an unrelated reason, above), so it is filed rather than fixed as part
of this PR.

## What each hitch point is, and where

| Hitch point | Where |
|---|---|
| A durable provisioning workflow | `provision.go`, `ProvisionTenant` |
| A host function recording an audit event | `record_event`, called through its **typed** client (`cleat/pluginclients/auditlog`, cleat#2626/#2681) — `recordMilestone` in `provision.go` |
| A tenant-scoped HTTP control surface | `plugins/tenantlifecycle`'s `GET /api/tenant/lifecycle` (Stage 3, cleat#2683), proxied by `backend/main.go`'s `getTenantLifecycle` |
| A background sweep for expired/suspended tenants | `plugins/tenantlifecycle`'s own sweep (Stage 1, cleat#2590) — this scenario does not reimplement it, it demonstrates it: see the scenario script's step 4 |

**No cross-tenant admin HTTP surface exists, and that is a decision, not a
gap.** cleat#2169 (owner decision, 2026-09-28) settled that a cross-tenant
HTTP surface is wanted eventually but not now; every route this scenario's
backend or `plugins/tenantlifecycle` exposes acts only on the caller's own
tenant, resolved from its own API key.

## The rope side is a placeholder

**Workspace provisioning — seeding a default project, warming a cache,
whatever "ready to use" means for your product — is not called.** It is a
recorded `DurableSleep` standing where the round trip goes, the same shape
`examples/order-lifecycle/order.go` uses and for the same reason: a durable
call that reaches an external system resolves by name to a plugin
(`engine/app.go`), and this is one example directory, not a plugin set.

The welcome email **is** real (the bundled `email-notify` plugin), and
**best-effort** for the identical reason `order-lifecycle`'s own
`notify_customer` is: a tenant that provisioned cleanly should not be
unwound because an email did not go out — there is nothing to unwind here,
since nothing was spent.

## Files

- `provision.go` — the workflow: the provisioning sequence, its audit
  milestones, and the query state
- `provision_test.go` — unit tests, driven through `cleattest`
- `backend/` — the API and page server; also the one place this scenario
  does anything an ordinary tenant-scoped worker API cannot (tenant
  creation, via `cleat-worker`'s own CLI, never a direct database write)
- `web/` — the page. Served under `default-src 'none'`, so no inline script
  or style — unlike `order-lifecycle`'s page, the API key genuinely lives
  here, in memory, after signup; see `web/index.html`'s own comment for why
- `docker-compose.yml` — PostgreSQL and a worker
- `cleat.yaml` — the workflow's name and entry points

## Tests

```bash
cd examples && go test ./b2b-saas-control-plane/... -count=1
```

## DBOS TypeScript counterpart

`examples/b2b-saas-control-plane-dbos-port/` is the idiomatic DBOS
equivalent, counted against this side by `scripts/dbos-pair-loc.sh
b2b-saas-control-plane` and executed in CI the same way
`order-lifecycle-dbos-port` is. See that directory's own README for what
DBOS's primitives map to on that side.
