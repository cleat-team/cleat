# Order lifecycle — DBOS port

The DBOS counterpart to [`examples/order-lifecycle`](../order-lifecycle/), for
cleat#2597's cleat-vs-DBOS pair harness. This is the **control** pair: the
measurement doc that started the 0.4.0 direction
(`cleat-internal/cleat-vs-dbos-build-and-operate-2026-09-28.md`) found that on
this one scenario, an unexecuted DBOS counterpart already looked smaller than
cleat's — and an unexecuted counterpart is not evidence. This directory exists
to make that comparison real: both sides built, both sides run, both counted
the same way, in CI.

**The claim this pair carries** (see `src/order.test.ts`'s `CLAIM` constant,
printed at the start of every test run): a genuinely executed, equally-scoped
DBOS port exists and is comparable to cleat's — not that DBOS wins forever.
This pair asserts comparability, not an outcome. If a future cleat feature
changes which side is smaller, that is not a broken control; the control's
job was honesty, and it is done either way.

## Same scope as the cleat side

Four steps, matching `examples/order-lifecycle/order.go`'s shape:

- `chargeCard` / `refundCard` — the PSP round trip and its compensation.
- `reserveInventory` / `releaseInventory` — the inventory hold and its
  compensation. `releaseInventory` returns `false` rather than throwing on
  failure, because a compensation that fails must be reported, not treated as
  an ordinary step error — matching the cleat side's
  `SimulateCompensationFailure` distinction ("the compensation ran" vs. "the
  compensation worked").
- `shipOrder` — the terminal step.
- A payment-confirmation wait over `DBOS.recv`, delivered by a webhook
  handler — the DBOS equivalent of the cleat side's `await_webhook` against
  the bundled `webhook-ingest` plugin.

**One structural difference, stated rather than hidden.** DBOS has no saga
primitive — confirmed against `docs.dbos.dev`'s workflow tutorial, which
describes retryable steps but not compensation — so the saga here is
hand-written: each step's compensation is a sibling function, called from a
`catch` block, in reverse order of which steps completed. `cleat.NewSaga`
declares this; DBOS requires writing it.

## Version and date, and why both are pinned

`package.json` pins `@dbos-inc/dbos-sdk` to `5.1.10`, the latest stable
release as of **2026-09-28**, the day this port was written. Criterion 1
(cleat#2597) requires "written from current docs" — and "current" is a
moving target. The DBOS API itself moved under this port while it was being
written: `docs.dbos.dev`'s tutorial pages show the newer functional style
(`DBOS.registerWorkflow(fn)`, `DBOS.runStep(fn)`), while some reference pages
and this repo's own `examples/DX_COMPARISON.md` still show the older
class-decorator style (`@DBOS.workflow()`, `@DBOS.step()`). This port uses
the functional style because it is what `node_modules/@dbos-inc/dbos-sdk`'s
own `README.md` uses as its canonical example — checked against the
installed package, not only the docs site. Pinning the version and recording
the date means the next person who reopens this file knows what "current"
meant when it was written, rather than rediscovering the same drift.

## Build and run

```bash
npm install
npm run build
```

Needs `DBOS_SYSTEM_DATABASE_URL` (a plain Postgres connection string; DBOS
creates its own system-database schema there on launch).

```bash
DBOS_SYSTEM_DATABASE_URL=postgres://postgres:PASSWORD@127.0.0.1:5432/order_lifecycle_dbos \
  npm test
```

runs four scenarios end to end against a real DBOS runtime and a real
Postgres: shipped, declined (no confirmation), compensated (a charge unwound
after a failed reservation), and compensation-failed (a held reservation
whose release itself fails).

```bash
DBOS_SYSTEM_DATABASE_URL=postgres://postgres:PASSWORD@127.0.0.1:5432/order_lifecycle_dbos_http \
  PORT=3000 npm start
```

starts the HTTP backend (`src/server.ts`):

- `POST /orders` — place an order, returns `{orderId, workflowID}`.
- `POST /webhooks/payment/:orderWorkflowId` — the PSP's confirmation webhook.
- `GET /orders/:workflowId` — poll status.

`scripts/run-order-lifecycle-dbos-scenario.sh`, at the repo root, drives this
exact HTTP surface end to end and is what CI runs.

## Tenancy: 0 lines, and it is not a compliment

Same line count as cleat's side for the opposite reason. Cleat's worker
enforces row-level tenant isolation underneath every request; this backend
enforces **nothing** — there is one flat DBOS application namespace. A real
multi-tenant deployment would need a tenant filter written into every query
(a `WHERE tenant_id = ?` an author must remember on every new endpoint) or a
database/schema per tenant. That is the measurement doc's "tenancy: 17 (a
filter, not a boundary)" line, generalised: a filter is application code a
developer can forget; a boundary is the platform's job. This scenario's
tenancy role happens to need zero lines here because it never implements one
at all, which is the gap the comparison is naming, not a feature.

## Measured, 2026-09-28

`cloc src/*.ts`, `cloc.py` 2.10:

| role | file | code lines |
|---|---|---|
| workflow and compensation | `src/workflow.ts` | 114 |
| HTTP backend | `src/server.ts` | 70 |
| tests | `src/order.test.ts` | 90 |
| **total** | | **274** |

Against cleat's side, `cloc examples/order-lifecycle/{order.go,backend/main.go,order_test.go}`
on the same date: **707**. Re-derive both with `scripts/dbos-pair-loc.sh`, not
by re-quoting these numbers — they are a census of a file that will change.

**Corrected 2026-09-28 (cleat#2622): `server.ts` grew from 57 to 70 lines after this table was
first written, and the table was not re-derived when it did — the total quoted here was 261 for
however long that drift went unnoticed.** `scripts/check-dbos-pair-loc.py` now runs the counter
and fails CI if this table and the script disagree again, so the next drift is a red build rather
than a quiet one.

**Corrected 2026-09-29 (cleat#2626): 729 → 749**, net of two changes that pull in opposite
directions: converting `order.go`'s two hand-rolled `h.PluginCall` JSON call sites
(`webhook-ingest.await_webhook`, `email-notify.send`) to generated typed clients
(`webhookingest.AwaitWebhook.Call`, `email.Send.Call`) removes 12 lines of call-site ceremony
from `order.go` itself; adding `order_test.go`'s
`TestPlaceOrder_NotifyCustomerSendsARealBody` (a regression test for a real bug this conversion
found -- see below) adds more than that back. **749 is the honest APP figure**, not a partial
one: the generated clients
(`cleat/pluginclients/{webhookingest,email}/client.go`, 60 code lines combined) live in the
`cleat` SDK module, not in this example -- they are shared platform code every workflow calling
these two plugins reuses, the same way every workflow already reuses the rest of the `cleat`
package, not a cost this example's own line count should carry. (An earlier version of this note,
briefly, put the generated clients inside `examples/order-lifecycle/` itself and counted them as
this example's own cost, 775 against the 729 baseline. That placement turned out to be
mechanically wrong on top of being the wrong accounting: `examples/` is cleat's own separate Go
module, unpublished and untagged for local changes, and `cleat build`'s workflow staging only
ever copies files from the workflow's own source directory -- so a sibling subpackage under
`examples/order-lifecycle/` could not be resolved at all, and `cleat build` failed outright.
`cleat/`, by contrast, is the one module every `cleat build` already locally replaces for every
workflow, which is what makes SDK placement both the honest accounting and the working one. See
cleat#2626 and cleat#2658 for the full story.) See cleat#2626 for why the lever is still worth it
independent of any of this: converting `notifyCustomer` turned a real, previously silent bug (a
hand-written payload's `"body"` field, which `SendInput` never had) into a compile error.

**Corrected 2026-09-29 (cleat#2649): 749 → 759.** `awaitPaymentConfirmation` moved from a single
`AwaitWebhook.Call` attempt to a bounded loop that suspends on `h.AwaitSignals` and re-attempts the
claim, closing a review-found gap (cleat-review, #2695/#2697): a wake from `AwaitSignals` is not
proof of a claimable event, so the loop re-checks the CONDITION across a fixed attempt count rather
than trusting the first signal. That is more `order.go` than the single call it replaced, net of the
same PR's doc comments explaining why. `backend/main.go` and `order_test.go` are unchanged.

**Corrected 2026-10-02 (cleat#2627): 759 → 707, and this is the largest move yet because it was not
a tuning of `order.go` but a deletion of what the file no longer had to do.** The saga SDK now
publishes its own progress — the step it is about to run, the step that failed, and which
compensations ran and succeeded versus ran and failed — so `order.go` stopped maintaining three
parallel lists by hand across every step boundary and stopped issuing a `SetQueryState` call at
each one. It is the same published contract (the example's own tests assert it unchanged), moved
from the application into the platform; the 52 lines that left `order.go` are application lines
that every saga author had to write and could get wrong. `cleat/runtime_workflow.go` carries the
mechanism instead, and that file is not counted here — this table is the app's lines, the same
accounting that already keeps the generated plugin clients out of it. `backend/main.go` and
`order_test.go` are byte-identical to 759's measurement.

## The counter

`scripts/dbos-pair-loc.sh` runs `cloc` identically on this directory's `src/`
and on the cleat side's source files, so the two counts come from one pinned
invocation rather than "the same tool used twice slightly differently".
