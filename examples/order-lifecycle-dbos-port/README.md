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
printed at the start of every test run): a genuinely executed DBOS port exists,
measurable against cleat's by the same line counter and carrying the same
approval gate and query state — not that DBOS wins forever, and **not that the
two are step-for-step identical** (see the four differences in the next
section). This pair asserts comparability, not an outcome, and not equality.
If a future cleat feature changes which side is smaller, that is not a broken
control; the control's job was honesty, and it is done either way.

## Scope: which side is the reference, and what still differs

Two sides have to be measuring the same application for a line-count comparison
to mean anything, and until cleat#2997 these were not. The cleat side carried
**five** saga steps, a **human-approval gate** and **published query state**;
this port carried **three** steps and neither of the last two. The headline
below was therefore comparing a 5-step application against a 3-step one, and
reported a ~2.7x ratio that was mostly scope.

**The cleat side is the reference here**, because it is the application the
scenario was built around and the port exists to be compared with it — so the
port moves to the cleat side, not the reverse. Brought across by cleat#2997:

- **A human-approval gate** above `approvalThresholdCents` (50,000 — the same
  number `order.go` gates on), placed **before the first spending step**, so a
  rejected order leaves no compensation trail.
- **Query state**, published with `DBOS.setEvent` and read with
  `DBOS.getEvent` — the counterpart of `SetQueryState`/`QueryState`, including
  the `awaiting_approval` -> `approved` transition a poller needs so it stops
  describing an order as still awaiting a decision while it is being charged.

**Four differences remain, and they are stated rather than left for a reader to
find.** Items 1 and 2 are shape rather than scope; **item 3 is scope**, and it
is the one that moves the number, because it is a step the *cleat* side has and
this port does not; item 4 is the platform's:

1. **No multi-signal wait, and this is IDIOMATIC rather than inherent.**
   `cleat`'s `AwaitSignals` takes a *list* of signal names; DBOS's `recv` takes
   a single topic, and `waitFirst`/`waitAll` operate on workflow handles rather
   than on messages. A two-topic race is conceivable, but matching DBOS's own
   idiom is the defensible port, so the decision arrives as one topic whose
   payload names it.
2. **The approval window is an input, not a constant.** cleat's test
   fast-forwards 24h of *simulated* time with `AdvanceTime`; DBOS has no
   simulated clock, so the timeout path is only reachable if the window can be
   shortened — and the durable input is the determinism-safe place for it.
3. **No notification step, and this one is SCOPE rather than shape.**
   `order.go`'s `notify_customer` calls the bundled `email-notify` plugin; DBOS
   has no bundled equivalent. It is deliberately **not** imitated with a
   placeholder — a placeholder would move the line-count comparison while
   measuring nothing — but that is an argument against a *fake* step, not
   against a real one, and a real one is expressible (a `runStep` POSTing to a
   webhook). It is left out of this PR because a faithful port needs a receiver
   on both sides rather than one line on this one, and the consequence is
   stated rather than hidden: **the step counts are still 5 and 3, and because
   the missing step is on the DBOS side, its absence makes this port SMALLER
   and the ratio LARGER. 1.67x is therefore a slight overstatement of DBOS's
   position, not a floor.**
4. **And one that is the platform's, not the port's, and that favours cleat:**
   `recv`/`getEvent` timeouts are **not durably checkpointed**
   (dbos-inc/dbos-transact-ts#451), so a process that dies mid-wait restarts
   the window, where cleat's `AwaitSignals` resumes into the same deadline.
   Recorded rather than averaged away.

**One structural difference, stated rather than hidden.** DBOS has no saga
primitive — confirmed against `docs.dbos.dev`'s workflow tutorial, which
describes retryable steps but not compensation — so the saga here is
hand-written: each step's compensation is a sibling function, called from a
`catch` block, in reverse order of which steps completed. `cleat.NewSaga`
declares this; DBOS requires writing it.

## Version and date, and why both are pinned

`package.json` pins `@dbos-inc/dbos-sdk` to `5.2.11`, the version its
`package-lock.json` also resolves to. Criterion 1
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

runs the scenarios end to end against a real DBOS runtime and a real
Postgres: shipped, declined (no confirmation), compensated (a charge unwound
after a failed reservation), compensation-failed (a held reservation whose
release itself fails), and the approval branch — approved, rejected, and the
timeout an unapproved above-threshold order gives up on.

```bash
DBOS_SYSTEM_DATABASE_URL=postgres://postgres:PASSWORD@127.0.0.1:5432/order_lifecycle_dbos_http \
  PORT=3000 npm start
```

starts the HTTP backend (`src/server.ts`):

- `POST /orders` — place an order, returns `{orderId, workflowID}`.
- `POST /webhooks/payment/:orderWorkflowId` — the PSP's confirmation webhook.
- `POST /orders/:workflowId/approve` — deliver the human decision
  (`{approve, reason}`), the counterpart of cleat's `POST /api/orders/{id}/approve`.
- `GET /orders/:workflowId` — poll status. `status` is the run's fate, and
  `state` carries the workflow's published query state (`awaiting_approval`,
  `approved`, `rejected`, …), kept separate for the reason cleat's backend keeps
  them separate.

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

## Measured, 2026-10-03

`cloc` 2.10, both sides from one invocation — `scripts/dbos-pair-loc.sh order-lifecycle`:

| role | file | code lines |
|---|---|---|
| workflow and compensation | `src/workflow.ts` | 163 |
| HTTP backend | `src/server.ts` | 98 |
| tests | `src/order.test.ts` | 178 |
| **total** | | **439** |

Against cleat's side, `cloc examples/order-lifecycle/{order.go,backend/main.go,order_test.go}`
on the same date: **731**. Re-derive both with `scripts/dbos-pair-loc.sh`, not
by re-quoting these numbers — they are a census of a file that will change.

**What the scope fix did to the headline.** Until cleat#2997 this table read
**274**, and the ratio it produced was ~2.7x. Adding the approval gate and the
query state took the port to **439**, and the ratio to **1.67x**.

Two attributions, stated separately because an earlier version of this paragraph
got the first one wrong and credited the second source with a sentence it does
not contain:

- **The fair figure of 433 is cleat#2597's.** Its body reads *"cleat is 729 code
  lines and a fair DBOS TypeScript counterpart is 433"* — 729 being the count
  before cleat#2627's later deletion; the tree counts 731 today.
- **The arithmetic on it is cleat#2997's**, whose body reads *"Against 433 the
  ratio is ~1.7×, not ~2.7×."*

**The executed port lands within 6 code lines of the 433 estimate — 1.4%** —
which is the closest thing this pair has to a prediction meeting its
measurement, and it is a fact only now that the counterpart actually runs. Note
the direction of the residual difference above (item 3): the missing
notification step makes this port *smaller*, so 1.67x *overstates* DBOS's
standing rather than flattering cleat.

**The counter is checked, but NOT enforced — do not read these numbers as
guarded.** `scripts/check-dbos-pair-loc.py` compares this table against the
script's output, and CI runs it — but under `continue-on-error: true`
(`ci.yml:1892`; owner decision cleat#2699, tracked for reversal as cleat#2700),
so a drift here reports without failing the build. Until cleat#2931 re-enables
it, an edit that breaks this table will not turn CI red.

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

**Corrected 2026-10-02 (cleat#2627): 759 → 731.** Most of the move is a deletion. The saga SDK now
publishes its own progress — the step it is about to run, the step that failed, and which
compensations ran and succeeded versus ran and failed — so `order.go` stopped maintaining three
parallel lists by hand across every step boundary and stopped issuing a `SetQueryState` call at
each one, which is `order.go` 283 → 231. Those are application lines every saga author had to write
and could get wrong; `cleat/runtime_workflow.go` carries the mechanism instead, and that file is not
counted here — this table is the app's lines, the same accounting that already keeps the generated
plugin clients out of it. `backend/main.go` is byte-identical to 759's measurement.

**24 lines come back in this same PR, and they are named rather than netted silently.** One is a
status write the approval gate now needs: the saga reports its progress as `current_step` and writes
`status` only at the end, so without it an approved order reads "awaiting_approval" for the whole
saga — through the very steps that charge it. The rest is the regression test for that, which has to
SAMPLE the published status while the workflow runs, because the finished state reads "done" whether
or not the gate cleared its own status, so a final-state assertion cannot see the difference.

**An interim version of this note said 707, and that number was the mechanism alone.** It measured
`order.go` plus its test before review found the approval bug; the two figures are different claims
and are recorded separately so neither is mistaken for the other.

## The pair whose headline this is not

**1.67x is this measurement set's WORST case, and the better case is already
built, already executed and already in CI.** That belongs here, where the
number is read, and not only in the other pair's README.

On the **integration-hub** pair — the one that exercises the WASM sandbox, the
differentiator #2597 names — **cleat's app code is SMALLER**: `app total` is
cleat **130** against DBOS **200**
(`examples/integration-hub-dbos-port/README.md:427`). cleat carries a one-off
124-line platform cost for it and DBOS carries none, because DBOS has no
primitive for tenant-supplied code at all. See that README's table for the
per-row breakdown, and `scripts/dbos-pair-loc.sh integration-hub` to re-derive
it.

**A measurement set whose headline is its worst case, while a better case is
already built and CI-run, under-sells the demonstrated advantage** — the
opposite of the failure cleat#2595/#2596 were filed for.

## The counter

`scripts/dbos-pair-loc.sh` runs `cloc` identically on this directory's `src/`
and on the cleat side's source files, so the two counts come from one pinned
invocation rather than "the same tool used twice slightly differently".
