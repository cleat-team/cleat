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
