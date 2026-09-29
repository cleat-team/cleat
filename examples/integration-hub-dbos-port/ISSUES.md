# Porting notes: integration-hub's wedge to DBOS

Differences from `examples/integration-hub` found while building this pair,
in the style of `examples/order-lifecycle-dbos-port/ISSUES.md`.

## No primitive for tenant-supplied code, at all

This is the whole reason this pair exists, so it is recorded here rather
than only in the README: `docs.dbos.dev` has no page, tutorial, or reference
describing a mechanism for accepting code from an external party at runtime
and executing it as a step of a workflow -- sandboxed or not. Checked:

- `/architecture` -- describes annotating workflows and steps in your OWN
  application; nothing about accepting code from elsewhere.
- `/typescript/tutorials/step-tutorial`, `/typescript/tutorials/workflow-tutorial`
  -- `DBOS.registerWorkflow` / `DBOS.runStep` both take a function
  REFERENCE, resolved at process startup. There is no analogue of loading a
  module from a database row, a file uploaded over HTTP, or any other
  runtime source.
- `/typescript/tutorials/queue-tutorial` -- "multi-tenant" here means
  partitioned concurrency ("at most one task per user"), explicitly a
  fairness feature, not code isolation.
- `/typescript/tutorials/upgrading-workflows` -- covers deploying a new
  VERSION of your own application's code across in-flight workflows
  (patching vs. versioning); not third-party code intake.

**So making DBOS express cleat's `POST /api/definitions` + sandboxed
`ChildWorkflow` at all needs two things DBOS supplies neither of:** a
code-upload/storage mechanism, and an isolation layer (a WASM runtime, an
isolate library, a container per tenant). Building either into this port
would attribute infrastructure this author wrote to a platform capability
DBOS does not have -- which is why `src/workflow.ts`'s `runTenantStep` does
not attempt it. See README.md, "Why the scope had to narrow", for what this
port measures instead.

## Not a full SyncCustomer port

`examples/integration-hub/hub.go`'s `SyncCustomer` also does webhook
ingestion, connector dispatch, and rate-limited HTTP -- none of that is what
this pair is about. This port is scoped to exactly the property the
cleat-side sandbox pair measures
(`scripts/run-integration-hub-tenant-sandbox-scenario.sh`): whether a
"tenant step" reaches the host filesystem. Matching that script's own
precedent, there is no HTTP surface here either -- both call their workflow
directly rather than through a webhook-driven pipeline neither side's
comparison needs.

## Not ported

- Webhook ingestion, connector dispatch, rate limiting -- out of scope for
  this pair; see above.
- Any upload endpoint -- see "No primitive for tenant-supplied code" above.
  Its absence is the finding, not an omission to fix.

## Second counterpart added: DBOS + isolated-vm (2026-09-28)

Added after cleat-review's review of the first counterpart identified the
bare-`DBOS.runStep` comparison as one-sided -- no real team ships
tenant-supplied code with zero sandboxing, so the honest counterpart is
"DBOS plus a sandbox library", not bare DBOS. See README.md, "The second
counterpart: DBOS plus a real sandbox", for the full writeup. Notes that
belong here rather than there:

- **`isolated-vm@6.1.2` was chosen over the current `7.x` line specifically
  because of Node version pinning, not because it is newer or more capable.**
  `7.x` requires Node >=26, which is Node's bleeding-edge "Current" release
  as of 2026-09-28, not the LTS line -- pinning CI to a non-LTS Node to make
  a newer library version installable would be a worse-idiomatic choice than
  the whole point of this counterpart is meant to demonstrate. `6.1.2` needs
  only Node >=22 and ships prebuilt binaries for both `darwin-arm64` and
  Linux, verified empirically (installed and run against real DBOS +
  Postgres) before being pinned, not read off `npm view`.
- **`isolated-vm@6.1.2` failed to build from source against Node 26** during
  version selection (`gyp ERR! build error`, `serializer_nortti.o Error 1`)
  -- its native addon's prebuilt binaries are ABI-locked to specific Node
  majors, and no prebuild existed for 26 at the time. This is *why* Node 24
  is pinned in `.github/workflows/ci.yml` for this job specifically, rather
  than whatever Node version other jobs in this CI happen to use.
- **The durability-boundary cost DBOS's own docs describe (at-least-once
  step execution, requiring tenant-side idempotency DBOS cannot enforce) is
  real but not exercised by this pair's tests**, because both tenant
  behaviours ported here are pure functions with no side effects -- the
  same property that makes them a fair LOC comparison makes re-execution
  risk invisible to them. See README.md, "What this counterpart costs", for
  why building a fault-injection harness to exercise this was judged out of
  scope rather than silently ignored.
