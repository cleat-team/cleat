# B2B SaaS control plane — DBOS port

The DBOS counterpart to
[`examples/b2b-saas-control-plane`](../b2b-saas-control-plane/), for
cleat#2597's cleat-vs-DBOS pair harness. This is **not the control pair** —
`order-lifecycle` is that, and its job is to be an honest equally-scoped
comparison. This pair exists for the opposite reason: to exercise the
situations where cleat's edge is *structural*, so the comparison has something
to say beyond a line count.

**The claim this pair carries** (see `src/provision.test.ts`'s `CLAIM`
constant, printed at the start of every test run):

> An equally-scoped, idiomatic, EXECUTED DBOS port of the
> b2b-saas-control-plane scenario exists and runs, and building it shows that
> the tenant filter, the audit append and the append's replay idempotency are
> the port author's code rather than the platform's — where cleat supplies
> all three.

Every clause of that is carried by a test, and none of it asserts that DBOS is
worse. It asserts *where the code is*, which is a fact about the two platforms
rather than a preference. On line count DBOS is **smaller** here, as it is on
the control pair — the claim is not about size.

## Same scope as the cleat side

The cleat side is a durable tenant-provisioning workflow that runs AS the
tenant it provisions, recording a milestone per step to that tenant's own
hash-chained audit trail, plus a trial-expiry sweep. This port is the same
sequence:

- `provisionTenant` — the durable provisioning run: record started, provision
  the workspace behind a placeholder, record it, send a best-effort welcome
  email, record it, record completion. The failure path records
  `tenant.provisioning_failed` and publishes which step failed.
- `sendWelcomeEmail` — best-effort, returning a boolean rather than throwing,
  so an email that does not go out cannot unwind a tenant that otherwise
  provisioned cleanly.
- `suspendExpiredTrials` — the sweep, as a DBOS **scheduled workflow**.
- The HTTP backend: sign a business up, poll that run's status, and let a
  tenant read its own lifecycle row.

The milestone sequence is the cleat side's, event for event:
`tenant.provisioning_started` → `tenant.workspace_provisioned` →
`tenant.welcome_email_sent` → `tenant.provisioning_completed`, with
`tenant.provisioning_failed` replacing the last two on the failure path.

**No `web/` directory, matching the sibling ports.** `examples/order-lifecycle`
has a web UI and `examples/order-lifecycle-dbos-port` does not; this pair
follows that precedent rather than inventing a difference. The pair's
comparison is between the workflow, the backend and the tests — the files
`scripts/dbos-pair-loc.sh` counts.

## Three structural differences, stated rather than hidden

Each is a thing the cleat side gets from its platform and this port has to
write itself. `src/workflow.ts`'s header carries the same three.

1. **Tenancy is a filter here, not a boundary.** On the cleat side the run
   executes as the tenant it provisions, so the engine scopes every host call
   and the workflow *cannot* address another tenant. Here the workflow
   receives a `tenantId` and threads it into every write; a missed thread is a
   cross-tenant write nothing refuses. `src/server.ts` makes the same point
   about its one tenant-scoped route.
2. **The audit trail is a plain table.** cleat's `plugins/auditlog` is a
   hash-chained, tenant-scoped table whose append is one transaction taking a
   per-tenant head lock. Below, `recordMilestone` is a step that INSERTs into
   a table this port owns. Nothing verifies it and it is not tamper-evident.
3. **Replay idempotency is hand-written.** A DBOS step that commits and then
   dies before its completion is checkpointed **is re-run on recovery**. DBOS
   gives you the retry; it does not give you the dedupe. The fix is the one
   cleat's auditlog uses for the same reason — a deterministic id
   (`workflowId:seq`) plus a primary key — and it is asserted by
   `testMilestoneRecordingIsIdempotent`.

## Version and date, and why both are pinned

`package.json` pins `@dbos-inc/dbos-sdk` to **5.2.11**, current as of
**2026-10-02**. Criterion 1 (cleat#2597) requires "written from current docs",
and "current" is a moving target, so the date is part of the pin.

The two sibling ports pin **5.1.10**, and this port deliberately does not:
before writing anything, the sibling's `order-lifecycle-dbos-port` source was
built and run unchanged against 5.2.11 here, and it passed all four scenarios.
The functional style (`DBOS.registerWorkflow`, `DBOS.runStep`, `DBOS.sleep`,
`DBOS.recv`) is therefore stable across that bump, so pinning current costs no
idiom divergence from the ports beside it. **If the siblings are later bumped,
they should move together with this one rather than leaving three ports on two
versions by neglect.**

## Build and run

```bash
npm install
npm run build
```

Needs `DBOS_SYSTEM_DATABASE_URL` (a plain Postgres connection string; DBOS
creates its own system-database schema there on launch, and this port creates
its own `b2b_tenants` and `audit_events` tables in the same database — a real
deployment would keep the two apart, and that separation is rope this example
does not model).

```bash
DBOS_SYSTEM_DATABASE_URL=postgres://postgres:PASSWORD@127.0.0.1:5432/b2b_dbos \
  npm test
```

runs six scenarios end to end against a real DBOS runtime and a real Postgres:
a clean provisioning run, a workspace failure, a best-effort email that does
not go out, replay idempotency, the cross-tenant filter, and the sweep
suspending an expired trial while leaving an active one alone.

```bash
DBOS_SYSTEM_DATABASE_URL=postgres://postgres:PASSWORD@127.0.0.1:5432/b2b_dbos_http \
  PORT=3000 npm start
```

starts the HTTP backend (`src/server.ts`):

- `POST /api/signup` — creates the tenant and starts its provisioning run,
  returning `{tenantId, workflowID, apiKey}`.
- `GET /api/provisioning/:workflowId` — poll that run's status.
- `GET /api/tenant/lifecycle` — the caller's own tenant row, keyed by its
  Bearer token.

`scripts/run-b2b-saas-control-plane-dbos-scenario.sh`, at the repo root, drives
this exact HTTP surface end to end and is what CI runs.

## Measured

`scripts/dbos-pair-loc.sh b2b-saas-control-plane`, cloc 2.10, on 2026-10-02.
Application lines, non-comment:

| side | files | code |
|---|---|---|
| cleat: workflow, backend, tests | `provision.go`, `backend/main.go`, `provision_test.go` | 641 |
| DBOS: workflow, backend, tests | `src/workflow.ts`, `src/server.ts`, `src/provision.test.ts` | 458 |
| **total** | | **458** |

Against cleat's side, `scripts/dbos-pair-loc.sh b2b-saas-control-plane`
on the same date: **641**.

Re-derive both with that script rather than re-quoting these numbers — they are
a census of files that will change, and `scripts/check-dbos-pair-loc.py` fails
CI if this table and the script disagree.

**DBOS is smaller here, and that is not the pair's finding.** The control pair
(`order-lifecycle`) already establishes that a line count alone does not favour
cleat. What this pair adds is which side *the code sits on*: three of the
scenario's requirements are platform behaviour on cleat's side and application
code on this one — see the three structural differences above.

## Which dialects this runs on

**PostgreSQL only, and not by omission.** The DBOS SDK's system database is
Postgres-only: there is no second dialect on this side to diverge on, which is
also why the sibling ports' CI jobs run one arm rather than cleat's
`order-lifecycle-scenario`'s three.

That is a different reason from the **cleat** side's, which is PostgreSQL-only
because it creates tenants via `cleat-worker --create-tenant` and that refuses
on MySQL and SQL Server. So the pair is single-dialect from both directions at
once, and neither side's reason is the other's — see
`examples/b2b-saas-control-plane/README.md`'s
[Which dialects this runs on](../b2b-saas-control-plane/README.md#which-dialects-this-runs-on)
for the cleat half.

**MySQL is excluded twice over on the cleat side**: it is single-tenant by
construction (`migrations/mysql/038`), so a scenario whose whole subject is
per-tenant provisioning has nothing to provision there.

## Reading the sweep

The cleat-side scenario script drives the sweep by backdating a trial and
waiting for the real background loop. This port's sweep is a DBOS scheduled
workflow on a per-minute cron, and the two callers reach it differently on
purpose:

- The **unit test** fires it with `DBOS.triggerSchedule`, which runs the same
  registered workflow on demand instead of waiting up to a minute for a tick.
  What is skipped there is the timer, not the sweep.
- The **scenario runner** waits for the real cron, the same way the cleat-side
  script waits for the real loop — so the schedule's *registration* is
  exercised too, not only the workflow behind it.
