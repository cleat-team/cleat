# Porting notes: b2b-saas-control-plane to DBOS

Differences from `examples/b2b-saas-control-plane` found while porting, in the
style of `examples/order-lifecycle-dbos-port/ISSUES.md`.

## No tenant scoping: a filter, not a boundary

On the cleat side the provisioning run *is* the tenant — the backend starts it
under the new tenant's own minted credential, and the engine scopes every host
call it makes, so there is no expression in the workflow that could name a
different tenant's data. Nothing had to be written for that; it is what
`apiServer.scopedStore` does to every request.

DBOS has no equivalent. `provisionTenant` receives a `tenantId` in its input
and threads it into each write, and `src/server.ts`'s tenant-scoped route works
because it looks the caller up by API key and puts the result in a `WHERE`
clause. That is application code, and a mistake in it is a cross-tenant read
with nothing to refuse it. The measurement doc's "tenancy: 17 (a filter, not a
boundary)" line is about exactly this, and this scenario is the one in the set
that makes it concrete rather than theoretical: every step of the workflow
touches tenant-scoped data by construction.

## No audit chain: an append the port owns

cleat records each milestone through `plugins/auditlog` — a hash-chained,
tenant-scoped table whose append is a single transaction taking a per-tenant
head lock, with a verifier that re-hashes what the database returns. A
workflow calls one typed client (`auditlog.RecordEvent.Call`) and gets all of
that.

Here, `recordMilestone` is a step that INSERTs into a table this port creates.
It is not tamper-evident, nothing verifies it, and there is no chain. This is
the cheapest of the three differences to *write* and the most expensive to
*replace*: the cleat side's guarantee is that two independent readings of the
same rows agree, which is a property no amount of care in this file can
confer.

## Replay idempotency is hand-written, and it has to be

A DBOS step that commits and then dies before its completion is checkpointed
**is re-run on recovery** — DBOS gives you the retry. It does not give you the
dedupe, so a naive `INSERT` in a step appends twice. The fix here is the one
cleat's auditlog arrived at for the same reason: a deterministic event id
(`workflowId:seq`) plus a primary key on `(tenant_id, event_id)`, so the
second run is `ON CONFLICT DO NOTHING`.

Worth noting *where* this is testable: not by restarting the workflow. DBOS
replays a completed step from its checkpoint rather than re-executing it, so
the hazard only appears at the commit/checkpoint boundary, and
`testMilestoneRecordingIsIdempotent` reaches it by calling the step's own
function twice. The port author has to know this is coming, because the
platform will not tell them.

## The sweep is a scheduled workflow, and the tests trigger it

cleat's trial-expiry sweep is a plugin background loop; the scenario script
backdates a trial and waits for the real loop. There is no plugin system here,
so the sweep is `DBOS.register...`/`createSchedule` — a cron — and
`DBOS.triggerSchedule` fires that same registered workflow on demand. The test
and the scenario runner use the trigger rather than waiting for a tick, so
what is skipped is the timer, not the sweep.

The division of responsibility is unchanged: suspension is the sweep's
decision, not the tenant's. `provisionTenant` never touches
`trial_expires_at`, the same way the cleat side's workflow cannot — a workflow
able to set its own trial expiry would let a tenant postpone the sweep meant
to constrain it.

## API churn inside "current docs": the same two styles, still

The two coexisting DBOS TypeScript styles the sibling port documented
(class decorators vs. the functional `DBOS.registerWorkflow`/`DBOS.runStep`)
are both still visible in `docs.dbos.dev`. This port uses the functional
style, for the sibling's reason: it is what the installed package's own
`README.md` presents as canonical.

The SDK has moved since the sibling was written — this port pins **5.2.11**
against the siblings' **5.1.10** — and the functional API is unchanged across
that bump, verified by building and running the sibling port's code unchanged
against 5.2.11 before writing any of this. See the pair's `README.md` for the
full reasoning, including why the three ports should be bumped together from
here rather than drifting apart.

## No rate limiting either: platform feature vs. application code

cleat's worker bounds its HTTP surface as a platform feature — `plugins/ratelimiter`,
plus an `ipRateLimiter` and a `keyedRateLimiter` in `cmd/cleat-worker/main.go` — so
`examples/b2b-saas-control-plane`'s backend is proxying to something already bounded and
never has to think about it. Here `src/server.ts` has to add `express-rate-limit`
itself, and it did so only after a code-scanning finding:

`js/missing-rate-limiting` failed this PR's CodeQL check on the two routes that touch the
database directly (`POST /api/signup`, `GET /api/tenant/lifecycle`). The sibling port's
routes are clean because they call DBOS APIs rather than SQL — so the rule fired on a
real difference in this port, not on a stylistic one.

**It was kept rather than dismissed**, and the way that was decided is the part worth
reusing: the finding was checked against the codebase's own convention before being
judged noise, and cleat's worker *does* rate-limit. A linter rule the project does not
follow and a real gap look identical in a red check; the convention is what tells them
apart. This is the fourth difference in the pair's list, and the only one that arrived
from tooling rather than from design.

## Not ported

- **The web frontend** (`examples/b2b-saas-control-plane/web/`). The sibling
  pair omits `order-lifecycle/web/` for the same reason — the comparison is
  between the workflow, the backend and the tests.
- **`docker-compose.yml` / `cleat.yaml` deployment wiring.** The scenario
  script starts the compiled server against a Postgres the CI job's
  `services:` block provides, the same way the sibling does.
- **The `cleat-worker --create-tenant` / `--generate-api-key` split.** On the
  cleat side, creating a tenant is an operator action on a privileged
  connection and the backend shells out to the worker binary for it; here it
  is one `INSERT` in `src/server.ts`. The *division* is preserved — the
  backend creates the tenant before starting the run, and the workflow never
  creates the tenant it runs as — but the cleat side's version has a real
  privilege boundary under it (a `CREATEROLE`-capable connection, deliberately
  not the one the worker serves on) that this port's single database
  connection does not have.
