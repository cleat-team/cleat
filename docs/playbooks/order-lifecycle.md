# Playbook 3 — Order and subscription lifecycle

**Status:** engineering reference. Drafted 2026-09-14 against `develop` at `654d6f84`.
Nothing here has been built end to end; see [What was verified](#what-was-verified) at the end.

**Who this is for:** you take money and ship something. An order touches inventory, payment,
fulfilment and notification; a subscription renews, dunns, upgrades and cancels. Each of those is a
multi-step process across systems you do not control, where a failure halfway through leaves the
customer charged and unshipped — or shipped and uncharged.

This is the oldest use case for durable execution and the one an evaluator will recognise fastest.
Its distinctive win is not novelty. It is that **the code you do not write is the code that is
hardest to get right.**

---

## The failure that defines the problem

Charge the card, then reserve the stock, then email the customer. The process dies after the
charge.

Every conventional answer to this is a variation on the same theme: a state machine in a database,
a job queue with a retry count, a reconciliation job that runs nightly and fixes what it can, and a
runbook for what it cannot. The logic is not conceptually hard. It is that there are a dozen places
to put it and each one is a place to get it wrong, and you only find out in production, with money
involved.

The durable-execution answer is that the process **is** the code, and the engine guarantees the
code resumes where it stopped.

---

## What ties to the cleat, and what stays rope

**Ties to the cleat:**

- The order or subscription lifecycle itself — the sequence, the branches, the waits
- Compensation: what to undo, and in what order, when step four fails
- Idempotency, so a retried request does not become a second charge
- Inbound webhooks from your payment provider
- Scheduled retries and dunning
- Human approval for refunds or exceptions
- The audit trail of what happened to this order

**Stays rope:**

- Stripe, Adyen, your PSP. Cleat calls them; it does not replace them.
- Your inventory, shipping and tax systems
- Your storefront and your customer portal front-end
- Your ledger and your accounting system

---

## The assembly

| Concern | Component | Hitch point |
|---|---|---|
| The lifecycle | your workflow, with `cleat.NewSaga` | SDK |
| Duplicate suppression | `Idempotency-Key` header on start | HTTP API |
| One process per order | concurrency keys | engine |
| Payment webhooks | `webhookingest`: `await_webhook`, HMAC-verified ingest route | host function + routes |
| Retries and dunning | `scheduler` | routes + background loop |
| Customer email | `email`: `send`, `send_template`, `check_status` | host functions |
| Internal alerting | `slacknotify`, `pagerdutyalert` | host functions |
| Human approval | workflow signals | engine |
| Downstream events | `kafkaconnect`: `produce`, `eventstore` | host function + routes |

Worked examples already in the tree: `examples/subscription/billing.go`,
`examples/fooddash/order.go`, `examples/travel/booking.go`,
`examples/saga-temporal-port/workflow.go`.

---

## Compensation you declare rather than orchestrate

The Go SDK has a first-class saga helper (`cleat/runtime_workflow.go:400-441`): `cleat.NewSaga()`,
`AddStep` taking a forward action and a `Compensate func(HostCalls) error`, and `AddParallel` for
steps that fan out.

You declare each step with its undo. If a later step fails, the completed steps' compensations run
in reverse. You do not write the unwinding, the ordering, the bookkeeping of which steps completed,
or the persistence of that bookkeeping across a crash.

`examples/saga-temporal-port/` is a port of Temporal's money-transfer saga and exists specifically
to validate that this coverage is real — withdraw, deposit, reverse on failure, with a retry policy
matching the original. If you are evaluating against Temporal, that directory is the honest
comparison to read, along with `examples/DX_COMPARISON.md`.

**What compensation does not give you.** A compensation is your code, and it can fail too. A refund
that fails leaves you in the state the saga was trying to avoid, and the engine's answer is to
retry it and eventually dead-letter the run — not to fix it. Design compensations to be idempotent
and to converge, and treat the dead-letter queue as an operational surface rather than a graveyard.

---

## Idempotency, which is where money is actually lost

A customer double-clicks. A mobile client retries on a flaky network. Your load balancer replays a
request. Each of those is a second charge if the write path is not idempotent, and idempotency is
the thing every team intends to add and adds unevenly.

Cleat honours the conventional `Idempotency-Key` header on the start path
(`cmd/cleat-worker/server.go:815`), on dead-letter reprocess, and on schedule creation
(`server.go:2246`). Keys are stored per tenant in `idempotency_keys`, and a duplicate start
**reports that it was a duplicate** rather than silently returning the original — the response
carries a replay flag alongside the original run id, so a client can tell "I created this" from "this
already existed". That distinction is usually the first casualty of a homegrown implementation, and
it is the one that makes retries safe to automate.

The key is client-supplied, deliberately. The comment on the reprocess path spells out the reasoning
and it generalises: deriving a key from the entity id would protect callers that send nothing, but
would also refuse a *deliberate* second attempt — fix the downstream, try again — and "answering
that with the first run is worse than the duplicate this guards against."

**Concurrency keys** are the complementary primitive: they stop two workflows holding the same key
running at once, so "one active fulfilment per order" is a property rather than a hope. A conflict
names the holder, which matters when you are debugging why an order is not progressing.

---

## Inbound webhooks

Payment providers tell you about the world asynchronously, and webhook handling is its own small
disaster area: you must verify a signature, respond in milliseconds, tolerate duplicates, tolerate
out-of-order delivery, and not lose anything.

`webhookingest` provides the ingest route and the `await_webhook` host function. The ingest path is
one of the worker's **public patterns** — routes auth lets through without a cleat API key,
precisely so a third party that has no key can reach an endpoint with its own HMAC verification
(`cmd/cleat-worker/main.go:1447-1450`, and the reasoning in `auth/middleware.go:55-73`).

`await_webhook` is the piece that makes this pleasant: a workflow *waits* for the webhook as a step,
rather than you writing a handler that has to find the right in-flight order and poke it. The
correlation is the workflow's, not yours.

**Read the design-doc caveat before exposing this**: those public patterns are also where a
client-supplied tenant header is not overwritten by auth. Whether `webhookingest` consults the
context tenant or derives it from `source_id` was not traced — see
[`docs/multi-tenant-serving-design.md`](../multi-tenant-serving-design.md), open question 1.

---

## Cost

**Where this wins.**

*The reconciliation job disappears.* Most teams running order pipelines have a nightly job that
finds stuck orders and fixes them, plus the human process around it. That job exists because the
pipeline can lose track of where an order was. It is pure cost — engineering to build, engineering
to maintain, and an on-call burden — and it is not needed when the process state is durable by
construction.

*No separate queue or scheduler infrastructure.* Retries, delays, dunning schedules and timeouts are
engine features rather than a Redis, a Sidekiq and a cron box with their own availability stories.

*Idle waits are free.* A subscription waiting 30 days to renew, or an order waiting on a supplier,
is a row. Conventional implementations that hold a worker, a container or a connection during a wait
pay for the wait.

**Where this loses.**

*A conventional job queue is cheaper per unit of throughput.* If your "workflow" is genuinely two
steps with no compensation and no waiting, the event-history write cost buys you nothing. Do not
route high-volume, low-value, stateless work through a durable engine because it is there.

*Event history grows with order volume.* Every step of every order is retained. For a high-volume
commerce business this is the term to size first, and the retention defaults deserve a deliberate
decision — `--dead-letter-retention-days` defaults to 0, meaning off, so failed orders accumulate
indefinitely unless you set it. That default is arguably right (a dead-lettered order is the one you
most want to inspect) but it is a growth curve someone must own.

*Your PSP's own idempotency still matters.* Cleat's idempotency protects the start of your workflow.
It does not make Stripe's charge endpoint idempotent — you still pass an idempotency key downstream.

**Measure rather than assume:** event rows per completed order at your step count; the same for a
subscription over its lifetime including renewals; and `cleatctl cost --workload <orders/sec>
--events-per-wf <N>` for the infrastructure envelope, reading its retention caveat first.

---

## Operations

**Rollback is a pointer flip, and in-flight orders are safe.** This is worth spelling out because it
is the anxiety every commerce team has about deploying: workflow instances carry a
`(def_name, def_version)` pointer, so **an order that started on version 7 replays against version
7** even after version 8 is deployed. You are not rewriting the rules underneath orders that are
halfway through. Rolling forward and rolling back are both safe operations against a live pipeline,
which is not true of a state machine whose transitions live in the code you just replaced.

**The dead-letter queue is the operational surface.** A run that exhausts its retries lands there,
inspectable, with its full history, and can be reprocessed — honouring an idempotency key so the
re-drive is deliberate rather than accidental. Terminate carries a reason. This is materially better
than a poison-message queue holding a JSON blob and no context.

**Fewer systems to be on call for.** No queue broker, no scheduler host, no reconciliation job.

---

## Observability

The event history answers, per order, without instrumentation: what step it is on, how long each
step took, what each external call returned, how many times a step retried and why, and where a
compensation ran.

The question this makes cheap is the one support teams ask constantly — "what happened to order
12345?" — and which conventionally requires correlating application logs, queue metrics and payment
provider dashboards across three retention policies.

**The gap:** business-level aggregates. "What is our fulfilment success rate this week" is a query
you write against the history; nothing provides it. Nor is there revenue or funnel analytics.

---

## What you still have to build or buy

1. **Your PSP integration itself.** Cleat provides the durable scaffolding and the webhook ingest.
   The API calls and the money semantics are yours.
2. **Business analytics.** The history has the data; there is no reporting layer.
3. **A customer-facing portal front-end.** As designed.
4. **Downstream idempotency keys** to your PSP and other external systems.
5. **A retention decision**, particularly for dead letters.

---

## Failure modes to design against

**A compensation that fails.** Make them idempotent and convergent; monitor the dead-letter queue.

**A saga whose forward step is not idempotent under retry.** The engine retries steps. A forward
action that charges without an idempotency key charges twice.

**Version pinning surprises.** In-flight orders replaying against old code is the *correct*
behaviour and is occasionally not what someone expects when they ship an urgent fix. An urgent fix
reaches in-flight orders only if you migrate them deliberately.

**Webhook replay and ordering.** Providers resend. `await_webhook` correlates, but your handler
still has to tolerate duplicates and out-of-order arrival.

**The public-pattern tenant question** above.

---

## What was verified

**Read from the tree at `654d6f84`:** the `Saga`, `SagaStep` and `Compensate` API
(`cleat/runtime_workflow.go:400-441`); `examples/saga-temporal-port/README.md` and the example
directories cited; the `Idempotency-Key` handling sites on start, reprocess and schedule creation,
and the duplicate-reports-outcome behaviour with its stated reasoning; the public-pattern list and
the auth middleware's early return; `webhookingest`'s `await_webhook` registration; the
`--dead-letter-retention-days` default of 0.

**Asserted, not measured:** every cost and operational claim. The "reconciliation job disappears"
argument is an argument about what durability makes unnecessary, not a measurement of a team that
removed one.

**Not verified:** whether `webhookingest` consults the context tenant on its public ingest route.
Carried forward from the design doc as an open question rather than restated as a property.
