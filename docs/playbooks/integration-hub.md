# Playbook 4 — Event-driven integration hub (per-tenant iPaaS)

**Status:** engineering reference. Drafted 2026-09-14 against `develop` at `654d6f84`; corrected
2026-09-25 against `develop` at `656aced4` (cleat#2050) — see
[What was verified](#what-was-verified) at the end for what changed and what is still unverified.
Nothing here has been built end to end.

**Who this is for:** your product has to talk to your customers' other systems. Each customer wants
their CRM, their warehouse, their ERP, their Slack — and each wants slightly different rules about
what syncs where. You are being asked to build "integrations" as a feature, and you can feel it
becoming a product inside your product.

This is the playbook where cleat's cost-per-tenant advantage is largest, because an integration
hub's defining characteristic is that **the number of distinct configured things grows with the
number of customers**, and every conventional option prices exactly that axis.

---

## Why integrations are where per-tenant costs explode

Three conventional routes, and each fails on a different axis:

**Buy an iPaaS** (Zapier, Workato, Tray). Priced per task or per operation, so your bill scales with
your customers' event volume — an axis you do not control and cannot cap. Multi-tenancy is usually
modelled as "one account per customer", which becomes an administrative problem at three digits.

**Build on a broker** (Kafka, or a queue plus consumers). Operationally heavy, and multi-tenancy is
entirely yours to model: topic-per-tenant hits broker limits, shared topics need filtering you write
and can get wrong.

**Build it ad hoc.** A table of webhook URLs, a retry loop, and a cron job. Works for five
customers. At fifty, "why didn't customer X's order sync" becomes a daily question with no good way
to answer it.

The structural observation is that an integration is **a long-lived, per-tenant, versioned,
failure-prone, auditable process** — which is a workflow with extra words. Cleat already models
every one of those adjectives.

---

## What ties to the cleat, and what stays rope

**Ties to the cleat:**

- Each integration, as a versioned workflow definition
- Per-tenant configuration and credentials
- Event subscription and filtering
- Delivery with retry, backoff and dead-lettering
- The replayable record of every sync, which is the support tool
- Per-tenant rate limits, so one customer's bulk import does not starve the others

**Stays rope:**

- The customers' systems themselves — Salesforce, NetSuite, whatever
- Your broker, if you already have one. `kafkaconnect` connects to Kafka; it does not replace it.
- Your admin UI for configuring integrations
- Secret storage, if you have a vault

---

## The assembly

| Concern | Component | Hitch point |
|---|---|---|
| Inbound events from customers | `webhookingest`: `await_webhook`, HMAC ingest route — signing secret required as of 0.3.0 (cleat#1992/#2172) | host function + routes |
| Event subscription and filtering | `eventtriggers` — subscribe via HTTP, filter expressions, auto-start matching workflows | host function + routes + loop |
| Outbound webhooks | `notifications`: `send_webhook` — HMAC-SHA256 signed, with retry and delivery tracking | host function + routes + loop |
| Kafka publish and consume | `kafkaconnect`: `produce`, plus a consumer loop | host function + routes + loop |
| Append-only streams, with SSE | `eventstore` — `event_stream` table, tenant-isolated, SSE endpoint | routes + loop |
| Per-tenant limits | `ratelimiter` | edge middleware |
| Scheduled syncs | `scheduler` | routes + loop |
| Large payloads | `blobstore` | host functions |
| Per-integration config | `kvstore` (versioned JSONB, optimistic concurrency) | routes |

`eventtriggers` is the keystone: it "lets workflows subscribe to domain events via HTTP API,
evaluates filter expressions, and automatically starts workflow instances when matching events are
published." That is the routing layer of an iPaaS, already built.

---

## The distinctive win: an integration per tenant costs a row

A workflow definition is a row, versioned, scoped to a tenant. Routing rules pick which version a
given caller gets. Put those two facts together and you have the thing an iPaaS charges most for:

- **Customer A runs v3 of the Salesforce sync while customer B runs v5.** Not a special case — it is
  what version pinning and routing rules already do for workflows.
- **A customer-specific variant is a definition, not a deployment.** No new container, no new topic,
  no new tenant account in a vendor's console.
- **Rolling out a fix to one customer first is a routing rule**, and rolling it back is a pointer
  flip.

The thousandth integration costs the same as the tenth: an `INSERT`. Compare with topic-per-tenant
on a broker, account-per-tenant on an iPaaS, or service-per-integration on Kubernetes — all three
price the axis that grows.

---

## The wedge: the tenant's own step, not yours

Everything above is still **you** writing the integrations — a definition per customer, authored by
your team. The stronger claim is that a tenant supplies its own logic, and that is shipped:
**`POST /api/definitions` lets a tenant upload its own WASM workflow.** It is created through
`s.scopedStore`, so a tenant cannot write into another's namespace, and it is auto-versioned when no
version is given (`handleCreateDefinition`, `cmd/cleat-worker/server.go`) — each tenant owns its own
`(tenant_id, name, version)` rows.

So the shape is: your workflow owns the parts that must not vary — auth, retry policy, the audit
record, per-tenant limits — and calls the tenant's step for the part that must.

**A worked case.** A customer's CRM emits `order.created` and wants it normalized before your
`SyncOrder` workflow stores it:

1. The tenant uploads a `normalize-order` definition of its own through `POST /api/definitions`,
   carrying the transform it actually wants. It is scoped to that tenant and invisible to every
   other.
2. Your `SyncOrder` workflow awaits the event, then invokes the tenant's step as a child workflow,
   handing it the raw payload.
3. The tenant revises its transform next month. That is a new version of *their* definition, routed
   by the version pinning and routing rules the platform already uses — not a change to your
   product's code, and not a deploy.

**WASM is what makes this safe to offer.** The tenant's step runs in the same sandbox as every other
workflow: no ambient filesystem, no network beyond the host functions you allow, and its durable
calls go through the same recorder. You are not handing a tenant a scripting hook inside your own
process; the isolation containing it is the one that already contains your own workflows.

**Two things to get right before exposing it.**

*Exposure is not free.* Serving a tenant's workflow on a **public** route needs the exposure-class
work in cleat#1986 — each definition declaring `public`, `auth` (the default) or `internal`,
enforced on every workflow route. Until that lands, keep tenant-supplied definitions behind
authenticated routes.

*Webhook ingest is already safe, and it is the exception.* `POST /ingest/{source_id}` is one of
exactly two auth-exempt routes, and it learns its tenant **from the row rather than the request**:
the handler reads `webhook_sources` for `source_id` and takes `tenant_id` from it, through a *named*
cross-tenant read bound to its own context variable (`plugins/webhookingest/routes.go`, cleat#1538).
A caller cannot name a tenant. `an_auth_exempt_route_cannot_assume_a_tenant_test.go` is what stops a
future auth-exempt route from assuming one — which answers the question this playbook used to carry
forward as unverified.

---

## The second win: replay is the support tool

Integrations fail constantly, and almost always for reasons outside your control — the customer's
API was down, their schema changed, their credential expired, they rate-limited you. The recurring
operational question is therefore always the same: *what exactly did we send, what came back, and
can we do it again now that it is fixed?*

Conventionally that needs deliberate logging of request and response bodies, retention of those
logs, a way to find the right one, and a re-drive mechanism someone builds. Here all four are
by-products:

- Every external call is recorded with inputs and outputs, because durability requires it.
- A plain failure keeps its whole history, and can be re-replayed while retention holds it.
- A run whose last durable call exhausted its retry policy is dead-lettered, and is the one an
  operator can retry or reprocess.
- `cleatctl replay` and `cleatctl debug` exist for inspecting a run offline.

**Those last two are different runs, and the difference decides what a support engineer can do.**
Only a run whose last durable act was a call that **exhausted its retry policy** reaches the
dead-letter queue — the predicate is `endedOnAnExhaustedCall`, which reads the run's history and
does not look at the error text. A plain failure — the workflow returned an error, a trap,
schema-drift parsing, an `[AMBIGUOUS]` call — is `failed`: it is **not** in
`GET /api/dead-letters`, it cannot be retried or reprocessed, it **keeps its history** until
`--retention-days` sweeps it (default 30 days), and it **can** be re-replayed. The two reach
different verbs:

| the run's outcome | where it is | what re-drives it |
|---|---|---|
| `failed` | not in the DLQ | re-replay — same run, from recorded history |
| `dead_lettered` | in the DLQ | retry (in place), or reprocess (a new run from definition and input) |

This is worth stating precisely because getting it wrong fails quietly in the expensive direction:
a support engineer who goes looking for a plainly-failed sync in the dead-letter queue will not
find it, and the natural conclusion — that the run was never recorded — is wrong.
[Workflow lifecycle → Outcomes](../reference/workflow-lifecycle.md#outcomes) is the full contract.

**This is the feature your support team will actually notice**, and it is worth more than the cost
argument in day-to-day terms.

---

## Cost

**Where this wins.**

*No per-task pricing.* The dominant cost of a commercial iPaaS is per-operation billing on an axis
you do not control. Here, marginal cost per event is a database write on hardware you already own.
For a product embedding integrations as a feature rather than selling them, this is usually the
whole argument.

*No broker to run.* If you do not already have Kafka, not needing it is a large saving in both money
and operational attention. If you do, `kafkaconnect` uses it rather than duplicating it.

*Idle integrations cost nothing.* A sync that runs when an event arrives is a subscription row, not
a consumer process holding a connection and a partition assignment.

**Where this loses, and concede it clearly.**

*Postgres is not Kafka.* At genuinely high event throughput — sustained tens of thousands of events
per second, fan-out to many independent consumers, long retention for replay by offset — a broker is
the right tool and this is not it. The honest boundary is that cleat suits **per-tenant, moderate
volume, high-value, failure-prone** integrations; it does not suit a firehose.

*You are building the admin UI.* A commercial iPaaS's real product is the visual builder and the
connector catalogue. You get neither. Every connector is code you write.

*No connector marketplace.* Hundreds of pre-built connectors are the reason people buy Workato.
`plugins/` has Kafka, Slack, PagerDuty, email and generic webhooks, and that is the catalogue.

*`jobqueue` is not finished.* Its own package comment says the background worker "polls for pending
jobs and logs dispatch events (actual workflow dispatch comes later)". Do not build on it expecting
dispatch; use `eventtriggers` or `scheduler`.

**Measure rather than assume:** sustained events per second per tenant, and the event-history bytes
per sync at your payload sizes — these two decide whether the "Postgres is not Kafka" boundary bites
you. Then `cleatctl cost --workload <events/sec> --events-per-wf <N>`, reading its retention caveat.

---

## Operations

**One system rather than a broker plus consumers plus a scheduler plus a retry service.** The
operational surface is the worker pool and Postgres, both of which you already run for everything
else in this playbook series.

**Per-tenant blast radius is the design's, not yours.** RLS scopes the data; `ratelimiter` scopes
the request rate; per-tenant execution ceilings via `cleatctl set-tenant-setting` scope the compute.
A customer doing a bulk backfill is bounded without you writing the bounding.

**Use the plugin's `db` mode, not its default.** `ratelimiter` defaults to `memory` — in-process
buckets, so N workers give a tenant N times the configured rate. Mode `db` coordinates through a
`rate_counter` table with per-second buckets summed over a sliding window
(`checkDBRateLimit`, `plugins/ratelimiter/middleware.go`), which is cluster-wide and
dialect-portable. For an
integration hub, where a runaway customer backfill is the archetypal incident, that distinction is
the difference between a limit and a suggestion.

One thing to watch: **the DB path fails open on a database error** (`checkDBRateLimit` in
`plugins/ratelimiter/middleware.go`), so a database problem removes the limiter rather than stopping
traffic.

**It no longer silently downgrades**, and that matters if you have read an older copy of this
playbook. `Init` used to log a warning and fall back to per-process memory buckets when `mode: db`
was configured with no database reachable; it now **refuses to start** (cleat#1581). The reasoning
is that a warning is not a refusal — the worker came up, every tenant got a per-process limiter, and
N workers served N times the configured rate while the configuration said otherwise. An
unrecognised mode string is refused for the same reason, because `middleware.go` treated everything
that was not exactly `"db"` as memory. So the startup log is no longer where you catch this: a
deployment that asked for a cluster-wide limit and cannot have one does not start at all.

**Background loops need their tenant exemption deliberately.** Several plugins here run `Run` loops,
and a plugin loop has no tenant and cannot get one. Under the fail-closed policy it **fails outright**
rather than reading fewer rows, and must request `plugin.AcrossAllTenants` by name with a reason
(cleat#1278). Correct, and something to know before writing your own sweep.

---

## Observability

Per sync, with no instrumentation: what was sent, what came back, how many attempts, what the
backoff was, where it failed, and whether a compensation ran. Per tenant, by construction.

`eventstore`'s SSE endpoint (`handleSSE`, `plugins/eventstore/routes.go`, using `http.Flusher`) gives a
live tail of an event stream, which is a genuinely useful thing to point an admin UI at while
debugging a customer's integration.

`datadogexport` exists if that is where your dashboards live.

**The gap:** no aggregate integration health view — "which customers' syncs are failing most this
week" is a query you write. And nothing monitors the *customers'* systems for you.

---

## What you still have to build or buy

1. **Every connector.** The catalogue is small and generic.
2. **The admin UI** for configuring integrations per tenant.
3. **Credential storage and rotation.** `kvstore` holds configuration; it is not a secrets manager,
   and integration credentials are the most sensitive data in this playbook.
4. **Aggregate health reporting.**
5. **A broker**, if your volume genuinely needs one.

---

## Failure modes to design against

**A customer's bulk import starves the pool.** Per-process rate limits are not fleet limits. Bound
it with per-tenant execution ceilings as well, and consider a dedicated task queue — `--task-queue`
segments workers, so bulk work can be given its own pool.

**A poisonous event retried forever.** Set `--dead-letter-retention-days`; it defaults to 0, meaning
off, so dead-lettered runs accumulate indefinitely. **That flag bounds only the dead-lettered ones.** A
poison event that fails *plainly* — where no call ever exhausted a retry policy — is `failed`, and
is bounded by `--retention-days` (default 30) instead, which removes its history but leaves the run
record alone. The record itself is removed by `--dead-letter-retention-days` or
`--completed-workflow-retention-days`, depending on which outcome it settled as; both are off by
default. Setting only one of the two leaves the other accumulating.

**Replaying a sync that is no longer safe to replay.** Reprocess re-drives a **dead-lettered** run,
from its definition and input. If the customer has since fixed the record by hand, re-driving may
overwrite it. Honour idempotency keys downstream, and make re-drive an operator decision rather than
an automatic sweep.

**Re-replay is only as good as what retention has left.** Two redrive gaps closed on 2026-09-23
(cleat#2038, cleat#2039) and their consequences are worth designing around rather than reading past.
`RetryWorkflow` (`dead_lettered` → `ready`) had no pending-intent guard at all, and re-replay's
guard was defeated once `--retention-days` had swept a failed run's history. Both now refuse rather
than proceeding — which means a **retention window shorter than your incident-response window**
silently converts "we can re-drive it" into "we cannot". Size `--retention-days` against how long
you actually take to act on a failed sync, not against storage cost.

**Webhook ingest and the tenant question.** The ingest route is a public pattern, so auth does not
overwrite a client-supplied tenant header on it. See
[`docs/multi-tenant-serving-design.md`](../multi-tenant-serving-design.md), open question 1, before
exposing it.

**Webhook ingest now requires a signing secret.** As of 0.3.0 (cleat#1992/#2172), `POST
/ingest/sources` rejects a request with no `secret` (400), and `POST /ingest/{source_id}` refuses
every request with no valid `X-Hub-Signature-256` (401) or whose secret cannot be read from the
tenant secrets store (503) — an unsigned source, and an unsigned request to a signed one, are both
gone. This is a breaking change from 0.2.0, where a source with no secret accepted unsigned
payloads; see `CHANGELOG.md`'s breaking-changes entry for the migration note. `notifications`'
outbound `send_webhook` delivery got the same requirement the same PR: `POST /webhooks` also
rejects a missing secret, and `PUT /webhooks/{id}` can rotate one but can no longer clear it.

**Schema drift in a customer's system.** Nothing detects it. Your integration fails, and *where it
lands depends on how it failed*: a call that exhausted its retry policy is dead-lettered, while a
parse error or an error returned by the workflow is a plain `failed` run that never reaches the DLQ.
Either way the offending payload is in the history and someone reads it — which is the good version
of this failure, but it is still a human in the loop. Query both (`GET /api/dead-letters` and the
failed-status listing) or you will miss half of them.

---

## What was verified

**Read from the tree at `654d6f84`:** the package doc comments for `eventtriggers`, `eventstore`,
`kafkaconnect`, `notifications` and `jobqueue`, including `jobqueue`'s own statement that workflow
dispatch "comes later"; the registered host functions for each; `eventstore`'s SSE implementation
with `http.Flusher`; the plugin extension-point taxonomy; the public-pattern list; the
`--dead-letter-retention-days` default of 0; the `--task-queue` flag.

**Asserted, not measured:** every cost and throughput claim, including the "Postgres is not Kafka"
boundary, which is a judgement about where the crossover lies rather than a measured crossover.
Nobody has run this at volume.

**Not verified:** whether `eventtriggers`' filter expressions are expressive enough for real routing
rules — the package comment says filters exist; their grammar was not read.

**Resolved since drafting:** the `webhookingest` tenant question, which used to be carried forward
unverified. The tenant comes from the `webhook_sources` row and not from the request, through a
*named* cross-tenant read (`plugins/webhookingest/routes.go`, cleat#1538), and
`an_auth_exempt_route_cannot_assume_a_tenant_test.go` keeps a future auth-exempt route from assuming
one. See [The wedge](#the-wedge-the-tenants-own-step-not-yours).

**Corrected since drafting (2026-09-25, cleat#2050):** the claim that every failed run is in the
dead-letter queue (it is not — only retry-exhausted ones are, and the two kinds reach different
redrive verbs); the claim that `ratelimiter` falls back to memory when no database is reachable (it
refuses to start, cleat#1581); the claim that `--dead-letter-retention-days` alone bounds a poison
event (a plainly-failed one is bounded by `--retention-days`); and the tenant-supplied-logic gap,
which was shipped and unnarrated.
