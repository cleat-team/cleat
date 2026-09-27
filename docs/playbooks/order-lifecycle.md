# Playbook 3 — Order and subscription lifecycle

**Status:** engineering reference. Drafted 2026-09-14 against `develop` at `654d6f84`; corrected
2026-09-25 against `develop` at `656aced4` (cleat#2051) — see
[What was verified](#what-was-verified) at the end for what changed. Nothing here has been built end
to end.

**Who this is for:** you take money and ship something. An order touches inventory, payment,
fulfilment and notification; a subscription renews, dunns, upgrades and cancels. Each of those is a
multi-step process across systems you do not control, where a failure halfway through leaves the
customer charged and unshipped — or shipped and uncharged.

This is the oldest use case for durable execution and the one an evaluator will recognise fastest.
Its distinctive win is not novelty. It is that **the code you do not write is the code that is
hardest to get right.**

**This playbook is deliberately the vendor's own flow** — you write the order pipeline. If your
product instead lets *customers* supply the logic, the primitives are the same but the framing is
not: read [`integration-hub.md`](integration-hub.md) for tenant-uploaded WASM steps via
`POST /api/definitions`, and [`b2b-saas-control-plane.md`](b2b-saas-control-plane.md) for the
multi-tenant control plane around them.

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

The Go SDK has a first-class saga helper (`NewSaga` / `Saga` / `SagaStep`, `cleat/runtime_workflow.go`):
`AddStep` takes a forward action and a `Compensate func(HostCalls) error`, and `AddParallel` covers
steps that fan out. **There is a generic form too** — `NewSagaTyped[T]` returns a `SagaTyped[T]`
whose steps carry a typed result (`SagaStepTyped[T]`) — which this page did not mention until
2026-09-25, and which is the one you want when your step results are not all `any`. It is worth
knowing about before you write the untyped version and cast at every step.

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

Cleat honours the conventional `Idempotency-Key` header on the start path, on dead-letter reprocess,
and on schedule creation (`cmd/cleat-worker/server.go`). Keys are stored per tenant in
`idempotency_keys`, and a duplicate start **returns the original response plus a replay flag**:
`idempotent_replay: true` on the replay, and `false` on the original — present on both, deliberately,
so a caller reads it unconditionally rather than inferring "original" from an absent field. That is
how a client tells "I created this" from "this already existed", and it is usually the first
casualty of a homegrown implementation and the one that makes retries safe to automate.

**A key reused with a different payload is refused** (`409 idempotency_key_input_mismatch`), not
answered with the first result — replaying means "you already did this", and a changed payload means
you did something else. A request with *no* key keeps its old behaviour: no header means no token,
so two callers who both send nothing do not collide.

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
one of the worker's **auth-exempt routes** — routes auth lets through without a cleat API key,
precisely so a third party that has no key can reach an endpoint with its own HMAC verification. The
production list is `pluginAuthExemptPatterns` (`cmd/cleat-worker/plugin_exempt_routes.go`), handed to
`auth.MiddlewareWithMux`; the middleware's own reasoning is on `isPublicRoute`
(`auth/middleware.go`).

`await_webhook` is the piece that makes this pleasant: a workflow *waits* for the webhook as a step,
rather than you writing a handler that has to find the right in-flight order and poke it. The
correlation is the workflow's, not yours.

**The tenant question is answered, and the answer is the safe direction.** This used to be an open
item carried forward from the design doc. `POST /ingest/{source_id}` is auth-exempt, so
`r.Context()` carries no tenant; the handler resolves it **from the row rather than the request**,
reading `webhook_sources` for `source_id` and taking `tenant_id` from it, through a *named*
cross-tenant read bound to its own context variable (`plugins/webhookingest/routes.go`, cleat#1538).
A caller cannot name a tenant, and `an_auth_exempt_route_cannot_assume_a_tenant_test.go` is what
keeps a future auth-exempt route from assuming one.

The design doc's open question 1 is still worth reading, for the general shape rather than for this
plugin: on those routes a client-supplied tenant header is not overwritten by auth, which is a trap
for the *next* auth-exempt route you add. See
[`docs/multi-tenant-serving-design.md`](../multi-tenant-serving-design.md).

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

*Event history grows with order volume — but not the way you would guess, and the guess is the
expensive part.* **"Every step of every order is retained" is backwards**, which is what this
paragraph said until 2026-09-25. The three outcomes differ:

| the order ends | its event history |
|---|---|
| `done` | **deleted at finalize** — there is no success trail unless you build one |
| `failed` | kept, then removed by `--retention-days` (default 30 days) |
| `dead_lettered` | kept until `--dead-letter-retention-days` (default 0, meaning off) |

So the growth curve you size first is *failed and dead-lettered* history, not successful-order
history — the opposite of the intuition, because successful orders are the ones you have most of.
And because a `done` order's history is already gone before any retention flag runs, **an operator
who wants a success trail must publish it as query state, or into their own store**; no flag keeps
it. Decide that deliberately rather than discovering it during an audit.

The two flags are not two names for one thing, and each governs only its own kind:
`--retention-days` bounds a **failed** order's history, `--dead-letter-retention-days` bounds a
**dead-lettered** one's, and neither touches the other. Setting one and not the other leaves the
other accumulating indefinitely. The dead-letter default of 0 is arguably right — a dead-lettered
order is the one you most want to inspect afterwards — but it is still a curve someone must own.

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

**But not every failure lands there, and the difference decides what you can do with it.** Only a
run whose last durable call *exhausted its retry policy* is dead-lettered. An order that fails
plainly — your compensation returned an error, a trap, a schema-drift parse — is `failed`: absent
from the dead-letter listing, not retryable, not reprocessable, still re-replayable, and still
holding its history for `--retention-days`. Query both surfaces, or half your failures are invisible
to the dashboard you built for failures.

**"This tenant's open orders" is a query, not a projection you build.** `GET /api/workflows` is
tenant-scoped and takes `status`, `def_name`, `id_prefix`, `concurrency_key`, `error_code` and
paging, so the read model a commerce team would otherwise hand-write — a side table maintained by
triggers, drifting from the truth it mirrors — is a query against the instance table itself:
`?def_name=PlaceOrder&status=running` is the whole of it.

**Content search over payloads shipped too, and it is priced honestly.** `input_contains`,
`result_contains` and `error_contains` scan the payload columns; the endpoint's own comment calls
them "unindexed payload scans", deliberately kept separate from `search` so the expensive question
is asked explicitly rather than hidden inside the cheap one. Reach for them during an incident, not
to drive a listing page. (cleat#1571 and cleat#1945, the read model these used to be missing.)

**Fewer systems to be on call for.** No queue broker, no scheduler host, no reconciliation job.

---

## Observability

The event history answers, per order, without instrumentation: what step it is on, how long each
step took, what each external call returned, how many times a step retried and why, and where a
compensation ran.

**For an order that is running or one that failed — not for a successful one.** A `done` order's
history is deleted at finalize (see *Where this loses* above), so "what happened to order 12345?" is
answerable in full for the orders you are worried about, and only as far as your own query state
goes for the ones you are not. That asymmetry is easy to miss and lands on the day someone asks you
to reconstruct a successful order for a dispute.

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

**A run you can see but cannot redrive.** Two redrive guards landed on 2026-09-23 (cleat#2038,
cleat#2039) and the consequence is the part to design around: **re-replay is only as good as what
retention has left.** A failed order whose history `--retention-days` has already swept can no
longer be re-replayed *at all* — the guard refuses, correctly, rather than resuming from a partial
history. So a retention window shorter than your incident-response window silently turns "we can
redrive that order" into "we cannot", at exactly the moment somebody is trying. `RetryWorkflow`
refuses a run with a pending durable intent for the same reason: resetting it would race a call that
may already be in flight. Size `--retention-days` against how long you actually take to act, not
against storage cost.

**The public-pattern tenant question** above — now resolved, and safely.

---

## What was verified

**Read from the tree at `654d6f84`:** the `Saga`, `SagaStep` and `Compensate` API
(`cleat/runtime_workflow.go`); `examples/saga-temporal-port/README.md` and the example
directories cited; the `Idempotency-Key` handling sites on start, reprocess and schedule creation,
and the duplicate-reports-outcome behaviour with its stated reasoning; the public-pattern list and
the auth middleware's early return; `webhookingest`'s `await_webhook` registration; the
`--dead-letter-retention-days` default of 0.

**Asserted, not measured:** every cost and operational claim. The "reconciliation job disappears"
argument is an argument about what durability makes unnecessary, not a measurement of a team that
removed one.

**Resolved since drafting:** whether `webhookingest` consults the context tenant on its public ingest
route. It does not — it derives the tenant from the `webhook_sources` row (cleat#1538), which is the
safe direction. See *Inbound webhooks*.

**Corrected since drafting (2026-09-25, cleat#2051):** the retention statement, which was backwards
("every step of every order is retained"); the redrive guards cleat#2038/cleat#2039, now closed and
written up as an operational consequence rather than as open gaps; the open-orders listing, which is
a first-class query and now includes payload-content search; and `NewSagaTyped`, which was shipped
and unmentioned.
