# Porting notes: order-lifecycle to DBOS

Differences from `examples/order-lifecycle` found while porting, in the style
of `examples/saga-temporal-port/ISSUES.md`.

## No saga primitive

`cleat.NewSaga` / `AddStep` declares forward and compensate callbacks; the
engine runs the compensations for you, in reverse order, on failure. DBOS has
no equivalent — confirmed against `docs.dbos.dev`'s workflow tutorial, which
covers retryable steps but not compensation. The saga in `src/workflow.ts` is
hand-written: a `try`/`catch` around each risky step, calling that step's
compensation function directly. This is the concrete shape of the
measurement doc's "tenancy: 0 (the platform enforces it)" vs. DBOS's "17 (a
filter, not a boundary)" line, one level up the stack: a durable-execution
platform can give you compensation as a declaration, or it can give you
`runStep` and leave the rest to you.

## No blocking wait, worked around correctly this time

Unlike cleat's current `await_webhook` (which returns `found: false` and
needs a hand-written polling loop — the single largest driver of the line
count difference the measurement doc found), DBOS's `DBOS.recv` genuinely
blocks the workflow until a matching `DBOS.send` arrives or a timeout elapses.
This is a real DBOS advantage over cleat **today**, not a porting
inconvenience — it is why `orderLifecycle`'s payment-confirmation wait is one
line (`await DBOS.recv(...)`) rather than a loop. Item (a) of the 0.4.0
levers list (cleat#2597) is cleat adding the same primitive.

## API churn inside "current docs"

Two different DBOS TypeScript coding styles are visible in `docs.dbos.dev` at
once: an older class-based decorator style (`@DBOS.workflow()`,
`@DBOS.step()` — the style `examples/DX_COMPARISON.md`'s existing DBOS
snippet uses) and a newer functional style (`DBOS.registerWorkflow(fn)`,
`DBOS.runStep(fn)`). This port uses the functional style, because it is the
one `@dbos-inc/dbos-sdk@5.2.11`'s own `README.md` presents as canonical —
checked against the installed package's `node_modules/@dbos-inc/dbos-sdk/README.md`,
not only what a documentation search returns. See the pair's own `README.md`
for why the version is pinned rather than left to float.

## Two workflow functions, not a flag-driven state machine

`orderLifecycle` and `orderLifecycleFulfilmentFailsAfterReservation` are
separate functions rather than one function switching on every simulate
flag, because the two differ in **which step fails** (the reservation itself,
vs. the shipment after a successful reservation) — not merely in a flag
value. Collapsing them into one function would need a state machine bigger
than cleat's own saga declaration; two short functions is the more faithful
port of "declare each path", even though cleat's single `PlaceOrder` entry
point looks like one function from the outside.

## Scope, after cleat#2997

This port originally carried three saga steps and neither a human-approval gate
nor published query state, while `order.go` carried five steps and both — so the
pair's line-count headline was not like-for-like. cleat#2997 added the approval
gate and the query state. Three differences remain and are stated in the pair's
README rather than here, so there is one place to read them; the one that
matters most for behaviour is that `recv`/`getEvent` timeouts are not durably
checkpointed (dbos-inc/dbos-transact-ts#451), where cleat's `AwaitSignals` is.

## Not ported

- **The notification step.** `order.go`'s `notify_customer` calls the bundled
  `email-notify` plugin and is best-effort by design. DBOS ships no equivalent,
  and a placeholder standing where an email would go would add lines to the
  comparison without measuring anything, so it is left out and the step-count
  difference is stated instead.
- The web frontend (`examples/order-lifecycle/web/`). cleat's own
  four-role breakdown for this scenario (workflow/backend/tenancy/tests) in
  the measurement doc has no UI row for order-lifecycle either, so this is
  matching the comparison that already exists rather than narrowing it.
- Docker Compose / `cleat.yaml`-equivalent deployment wiring. The scenario
  script (`scripts/run-order-lifecycle-dbos-scenario.sh`) starts the compiled
  server directly against a Postgres the CI job's `services:` block provides,
  the same way the cleat-side scenario relies on `docker-compose.yml`'s
  service definitions rather than reproducing them in the script.
