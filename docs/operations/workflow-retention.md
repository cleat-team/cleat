# Workflow data retention

There are two independent retention sweeps, both run from the same
background loop (`retentionLoop`, once every 24 hours) and both configured
per-worker. They delete different things, they have different defaults, and
turning one on says nothing about the other.

| Flag | Deletes | Default |
|------|---------|---------|
| `--retention-days` | `event_history` rows for terminal workflows | `30` (on) |
| `--completed-workflow-retention-days` | `workflow_instances` rows for terminal workflows, plus any remaining `event_history` for them | `0` (off) |

See `docs/reference/worker-config.md` for the flag reference entries.

## Why the defaults differ

`--retention-days` deletes a workflow's *step-by-step replay log* once the
workflow is terminal. The workflow's outcome -- status, result, error,
`def_name` -- survives untouched in `workflow_instances`. Losing
`event_history` for an old, already-completed workflow costs you replay
detail and audit trail, not the fact that the workflow ran or what it
returned. That is a reasonable thing to do automatically, so it ships on by
default (30 days).

`--completed-workflow-retention-days` deletes the `workflow_instances` row
itself. After it runs, the workflow is gone from `ListWorkflows`, gone from
the admin dashboard, and there is no way to answer "did workflow X run, and
what did it return" ever again. That is a fundamentally more destructive
default to ship silently-on: it is deleting a user-visible record, not a
diagnostic log, and different deployments have very different requirements
here (a payments workflow's outcome may need to be retrivable for years; a
five-times-a-day internal notification workflow may not need to outlive the
week). Finding S2 (nothing ever deleted a completed `workflow_instances`
row, so the table grows forever bounded by lifetime workflow count rather
than active count) is real, but "the table is unbounded" and "silently
deleting user-visible records by default" are different classes of problem,
and only the first one is safe to default into existence. **This flag is off
by default (`0`) and must be opted into explicitly.**

If you enable it, pick a retention window at least as long as your own
audit/compliance/support requirements for "can we look up what a workflow
did." There is no undo.

## What gets deleted, and in what order

For `--completed-workflow-retention-days`, a workflow is eligible once its
status is `done`, `failed`, or `terminated` **and** `completed_at` is older
than the cutoff.

`dead_lettered` is deliberately excluded. It has its own lifecycle (workflows
land there after exhausting retries, generally because something needs human
attention) and its own deletion path,
`DeleteDeadLetteredWorkflows` -- as of this writing that path exists and is
tested but is not wired into any background loop, so dead-lettered workflows
are retained indefinitely regardless of either retention flag. That is a
separate, pre-existing gap outside the scope of this change.

**A Go workflow does not currently reach `dead_lettered` at all**, so on the
tier-1 SDK this exclusion is about a state nothing enters. The worker's
dead-letter branch is a substring test for `retries exhausted`
(`cmd/cleat-worker/setup.go`), and the engine mints that phrase only in its
*host-side* retry loop behind the `cleat_call_retry` import. `wasm/usage.go`
wires that import on the guest symbol `DurableCallWithRetry`, which
`HostCallsImpl` does not define -- the only guest-facing form is
`DurableCallWithOptions`, which falls back to SDK-level retry and produces a
different message. Rust's `HostCalls::cleat_call_with_retry` calls the import
directly, so the state is reachable there.

Measured by `engine.TestAnExhaustedRetryRunsItsDefersAndIsNotDeadLetterable`,
which exhausts a policy on a Go guest and asserts the terminal error does not
match the worker's predicate. Re-derive the wiring with

    grep -c "func (h \*HostCallsImpl) DurableCallWithRetry" cleat/runtime.go   # 0

This is recorded rather than fixed: whether the Go SDK should expose the
host-side retry loop is an open product question, IMPROVEMENT-PLAN.md 3.88
item 2. Until it is answered, do not plan around dead-lettering as a Go
workflow's failure mode.

Within one batch (bounded to 10,000 workflow IDs, to avoid a single
long-running transaction against a table that can be millions of rows deep):

1. Select up to 10,000 eligible workflow IDs.
2. Delete `event_history` rows for those IDs.
3. Delete the `workflow_instances` rows themselves.
4. Commit. Repeat until a batch selects zero rows.

Step 2 is not optional and not everywhere a genuine no-op:

- **PostgreSQL**: `event_history` has no foreign key back to
  `workflow_instances` at all. `migrations/postgres/003_procedures.sql` drops
  it deliberately, because `finalize_workflow_status()` already deletes a
  `done`/`failed` workflow's events itself when it reaches that status. But
  `TerminateWorkflow` (the path to `terminated`) does **not** call
  `finalize_workflow_status`, so a force-terminated workflow's events are not
  guaranteed to be gone by the time this runs, and would be orphaned forever
  the moment its `workflow_instances` row disappeared. PostgresStore deletes
  `event_history` explicitly, in the same transaction, for exactly this
  reason.
- **MySQL and SQL Server**: both still declare `event_history`'s FK
  `ON DELETE CASCADE`, so step 2 happens automatically when step 3 runs.
  Nothing extra is needed, and nothing extra is done.

The four other child tables (`workflow_signals`, `workflow_promises`,
`concurrency_keys`, `workflow_update_requests`) cascade on every dialect, so
they need no explicit handling anywhere.

Both sweeps run tenant-scoped (this worker's own tenant) and, on PostgreSQL,
inside `beginTxWithRLS`. `workflow_instances` carries
`FORCE ROW LEVEL SECURITY` with a fail-closed policy
(`cleat.assert_tenant_set()`); a plain-pool `DELETE` issued by a role that is
neither a superuser nor the table owner -- the shape `cleat_app` has in
production -- does not silently affect zero rows, it raises
`cleat.tenant_id is not set` and the whole call errors. The store methods
already handle this; nothing in this doc requires operator action, it is
here so a future change to these methods does not reintroduce the old
plain-pool bug (see `engine/completed_workflows_rls_test.go`, which asserts
this against a real non-superuser role).

## Metrics

`cleat_events_deleted_total` (pre-existing) counts `event_history` rows
deleted by `--retention-days`.

`cleat_workflows_purged_total` counts `workflow_instances` rows deleted by
`--completed-workflow-retention-days`. Both are `Int64Counter`s scraped like
any other `cleat_*` metric on `/metrics`; a flat line at zero on a worker
with the corresponding flag enabled and terminal workflows older than the
cutoff means the sweep is not doing anything, not that there is nothing to
do -- check the worker log for `retention: error deleting ...` first.

`cleat_retention_last_run_timestamp` (pre-existing, shared by both sweeps)
is a Unix-seconds gauge set at the end of every loop iteration, regardless of
whether either sweep found anything to delete. Use it to alert on the loop
itself having stopped running (e.g. `time() - cleat_retention_last_run_timestamp
> 2 * 86400`), independent of whether it is finding work.

## Indexes

See `migrations/postgres/033_completed_workflow_retention_indexes.sql` (and
the MySQL/SQL Server equivalents) for the indexes that support both the
retention queries above and the admin dashboard's `ListWorkflows` filters.
That migration's own comments explain which `ListWorkflows` query shapes are
indexed, which are deliberately left unindexed, and why -- in short: the
`status` filter and the dedicated `ErrorContains` filter are indexed;
free-text substring search across JSONB `input`/`result` columns (the
general "Search" box) is not, because no index configuration makes that
query fast while it also matches against those two unindexed columns in the
same `OR` -- the fix there is to narrow the query, not add an index, and is
out of scope for this change.
