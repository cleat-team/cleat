# Cleat playbooks

**Status:** engineering reference, open for review. Drafted 2026-09-14 against `develop` at
`654d6f84`. Nothing here claims support that `tiers.yaml` does not grant; where a playbook depends
on something unbuilt, it says so in its own "What you still have to build" section.

A cleat is the fitting you make a line fast to. It is not the rope, the sail or the boat — it is
the small, fixed, load-bearing thing that lets everything else be tied down and untied again.

That is the organising principle of these playbooks, and it is also the conclusion of
[`docs/multi-tenant-serving-design.md`](../multi-tenant-serving-design.md), which asked whether
cleat should grow into a general web-serving tier. The answer was no: every advantage cleat had in
that comparison was a **control-plane** advantage that needs no new cleat code, and every
disadvantage was an **edge or ecosystem** disadvantage that would need an enormous amount of it.

So each playbook answers two questions and keeps them apart:

- **What ties to the cleat** — the state, the sequencing, the retries, the tenancy, the audit
  trail, the things that must survive a crash.
- **What stays rope** — your front-end framework, your CDN, your object storage, your identity
  provider. Unchanged, unowned, and deliberately not reimplemented.

A playbook that cannot draw that line clearly is describing a bad fit.

---

## The four hitch points

Everything in these playbooks attaches through one of four plugin extension points. Knowing which
one you are using tells you what guarantees you get, and it is the single most useful thing to hold
in your head while reading.

| Extension point | Interface | What it gets you |
|---|---|---|
| **Host functions** | `RegisterHostFunctions(FuncRegistry)` | Callable from a workflow. **Recorded in event history and replayed deterministically** — the plugin author writes no replay code. |
| **HTTP routes** | `RegisterRoutes(*http.ServeMux)` | A control surface on the worker's mux, under the worker's auth and tenant scoping. |
| **Edge middleware** | `Middleware(http.Handler) http.Handler` | Wraps every request: identity, rate limiting, audit. |
| **Background loop** | `Run(ctx) error` | Sweeps, reconciliation, polling. No tenant of its own — needs `plugin.AcrossAllTenants` to read across tenants, by name and on purpose. |

Re-derive the membership of each rather than trusting a list; these grow:

    grep -rln 'func (p \*Plugin) RegisterHostFunctions' plugins/*/*.go | grep -v _test
    grep -rln 'func (p \*Plugin) RegisterRoutes'        plugins/*/*.go | grep -v _test
    grep -rln 'func (p \*Plugin) Middleware'            plugins/*/*.go | grep -v _test
    grep -rln 'func (p \*Plugin) Run'                   plugins/*/*.go | grep -v _test

The host-function point is the one that makes cleat different from a plugin system, and the
distinction is worth stating precisely because it is easy to skim past. `plugin/plugin.go:200-206`:

> These functions are automatically recorded in event history and replayed deterministically —
> plugin authors don't need to handle replay.

Two separate properties govern whether a call re-runs on replay, and they are deliberately not one
boolean (`FuncOptions`, `plugin/plugin.go:208-223`). *Idempotent* asks "is re-running this safe?";
*replay* asks "should this run at all?". Seven functions were once registered against the weaker
reading. An idempotent **write** reads as a yes and is still a live write issued while
reconstructing a past execution.

Host functions registered as of 2026-09-14, re-derivable with the command above:

| Plugin | Workflow-callable functions |
|---|---|
| `llm` | `chat`, `chat_stream`, `embed`, `list_models` |
| `pgvector` | `search`, `upsert`, `delete` |
| `blobstore` | `get`, `put` |
| `email` | `send`, `send_template`, `check_status` |
| `slacknotify` | `send_message` |
| `pagerdutyalert` | `trigger_incident`, `resolve_incident` |
| `notifications` | `send_webhook` |
| `kafkaconnect` | `produce` |
| `featureflags` | `evaluate_flag` |
| `eventtriggers` | `await_event` |
| `webhookingest` | `await_webhook` |

The edge-middleware set is small and is the whole browser-facing story: `oauthprovider`
(OIDC login, sessions), `ratelimiter` (per-tenant buckets), `auditlog`.

---

## The shared baseline

Every playbook assumes the same substrate, and none of them changes it:

- **Postgres** holds all state. Yours — managed, self-hosted, anywhere that speaks the wire
  protocol.
- **A pool of stateless cleat workers.** Add workers for throughput.
- **A tenant is a row**, with row-level security and optionally a database role and pool of its own
  (`--tenant-isolation=role`).
- **A workflow version is a row**, promoted by routing rules and rolled back by a pointer flip.
- **Your front-end is whatever it already is**, built by whatever already builds it, served from a
  CDN. Cleat is its API and its control plane, not its bundler.

The last point is not a limitation being apologised for. It is the line that keeps the other four
cheap.

---

## The playbooks

| | Use case | The distinctive win |
|---|---|---|
| 1 | [AI agent platform with per-tenant budgets](ai-agent-platform.md) | Replay *is* the audit trail; agent runs resume rather than restart |
| 2 | [Multi-tenant B2B SaaS control plane](b2b-saas-control-plane.md) | Five vendors collapse into one binary and a database |
| 3 | [Order and subscription lifecycle](order-lifecycle.md) | Compensations and idempotency you do not write |
| 4 | [Event-driven integration hub](integration-hub.md) | An integration per tenant costs a row |

---

## How the comparisons are made

Each playbook compares against the stack a team would otherwise assemble, on cost, operational
burden and observability. Three rules, all of them learned the hard way in this repository:

**No vendor prices.** A published price is a census of a moving population, and every such number
checked in this repo was wrong by the time anyone re-read it. The comparisons are about cost
*structure* — what scales with what — which is durable. Where a figure would decide something, the
playbook names the measurement to take instead of inventing one.

**Concessions are stated, not buried.** Each playbook has a "What you still have to build or buy"
section. A playbook with an empty one is wrong.

**Claims are separated from checks.** Each ends with what was read from the tree and what was
merely reasoned about. None of these has been built end to end; they are designs grounded in code
that exists, not reports of systems that run.
