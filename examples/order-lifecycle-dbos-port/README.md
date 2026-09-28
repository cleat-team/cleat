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
| HTTP backend | `src/server.ts` | 57 |
| tests | `src/order.test.ts` | 90 |
| **total** | | **261** |

Against cleat's side, `cloc examples/order-lifecycle/{order.go,backend/main.go,order_test.go}`
on the same date: **729**. Re-derive both with `scripts/dbos-pair-loc.sh`, not
by re-quoting these numbers — they are a census of a file that will change.

## The counter

`scripts/dbos-pair-loc.sh` runs `cloc` identically on this directory's `src/`
and on the cleat side's source files, so the two counts come from one pinned
invocation rather than "the same tool used twice slightly differently".
