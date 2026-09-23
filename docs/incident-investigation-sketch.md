# A read-only investigation agent over the durable record

**Status:** sketch, open for review. No decision taken, nothing built, nothing claimed in
`tiers.yaml`. Drafted 2026-09-14 against `develop` at `654d6f84`.

An AI agent with **read-only** access to cleat's event history, the source, and the surrounding
telemetry — able to run complex queries and correlate them, and **not** able to repair anything.
Repair stays with a human.

**On the competitive section:** the product claims in it come from three web searches run
2026-09-14, and the queries are recorded so they can be repeated. Vendor capabilities move faster
than this document will be updated. Treat every product claim as needing re-verification before it
is used anywhere it matters, and treat the *structural* arguments — which are about data models
rather than feature lists — as the durable part.

---

## Contents

- [The idea](#the-idea)
- [Why the substrate is different: a spine, not another stream](#why-the-substrate-is-different-a-spine-not-another-stream)
- [Read-only as an enforceable boundary](#read-only-as-an-enforceable-boundary)
- [What already exists](#what-already-exists)
- [Architecture sketch](#architecture-sketch)
- [Competitive landscape](#competitive-landscape)
- [Security model](#security-model)
- [What this does not do](#what-this-does-not-do)
- [Prerequisites](#prerequisites)
- [Open questions](#open-questions)
- [What was verified](#what-was-verified)

---

## The idea

Cleat records every external interaction a workflow makes, because durability requires it. That
record is complete enough to deterministically re-execute the run. Separately, a deployment has the
ordinary telemetry — logs, metrics, traces — that every system has.

Give an agent read-only access to the first, the ability to reach the second, and the source code,
and the proposition is that incidents get diagnosed faster and more cheaply than by a human reading
JSON.

The agent proposes. A human decides and acts.

---

## Why the substrate is different: a spine, not another stream

This is the whole architectural argument, and it is the reason to think this works better here than
the general category suggests.

**Conventional incident AI correlates N telemetry streams and looks for patterns.** That problem is
badly posed, which is why the category disappoints: during an incident everything correlates with
everything, and the tool's confidence is unrelated to its correctness.

Cleat has something none of those streams is: **a causal record.** The event history is the only
artifact in the stack that knows *why* — this step ran because that one returned this value.
Logs, metrics and traces know *what happened on a machine*.

So the agent should not be asked to find patterns across five systems. It should use the event
history as an **index into** them:

> Run `f92a49f9` failed at step 4, at 19:57:19.231, on worker-alpha, tenant X, trace `abc…`.
> Fetch the infrastructure telemetry for exactly that slice.

That converts an unbounded pattern-matching problem into a bounded lookup. **Query power is for
narrowing; external telemetry is for explaining.**

It also answers the obvious objection — that cleat cannot see why PostgreSQL got slow or why a
worker was OOM-killed. It cannot. But it can say precisely *when to look* and *at what*, and on most
incidents that is the majority of the work.

---

## Read-only as an enforceable boundary

The instinct to keep repair under human control is right, and it is stronger than caution.

"Propose but do not apply" is a boundary the **agent** respects. "Read-only" is a boundary the
**database** enforces. Those are different classes of guarantee, and the difference matters
specifically here, because the event history contains attacker-controlled content — webhook
payloads, model outputs, customer-submitted fields. An investigating agent reads untrusted input as
part of its job. Its blast radius should be bounded by something that does not reason.

Cleat already has the adapter. `engine/readonlydb.go`:

- `Exec` returns `read-only: Exec denied`
- `Begin` opens a tenant-scoped transaction and issues `SET TRANSACTION READ ONLY`

Beneath that, a PostgreSQL role without write grants is the real fence, and RLS is the tenant fence.

**The property this design leans on has since been checked, and the answer has three parts.**
An earlier draft left it open, citing CLAUDE.md's record of cleat#1285/#1286 — that `Query` called
`Inner.QueryContext` directly and so carried no tenant scope.

**1. On PostgreSQL, with a tenant in context, it is scoped — cleat#1285/#1286 is fixed.** `Query`
and `QueryRow` now both call `beginTenantTx` first (`engine/readonlydb.go:47-78`), which opens a
read-only transaction and issues
`SELECT set_config('cleat.tenant_id', $1, true)` — transaction-local, so the setting cannot follow
the connection back into the pool (`engine/plugindb_tenant.go:150-167`).

**2. With no tenant in context, the code does not scope — the database refuses.** `beginTenantTx`
returns `(nil, nil)` when `tenantctx.From(ctx)` finds nothing (`:150-153`), and the caller falls
through to an unscoped `Inner.QueryContext`. What stops that being a cross-tenant read is the RLS
policy's own assertion, which raises `cleat.tenant_id is not set`. So this **fails closed at the
database rather than in the adapter** — a distinction worth holding, because it is only true while
the connecting role is subject to RLS. `cleat-worker` refuses to start on a connection that
bypasses RLS by default (cleat#1204), which is what keeps that precondition true.

Note the limit CLAUDE.md records: a `USING` clause is a row-level predicate, so **against an empty
table it never fires**. An agent's clean read of an empty result set is not evidence that scoping
worked.

**3. On MySQL and SQL Server there is no scoping here at all, by design.** `beginTenantTx` returns
`(nil, nil)` immediately for any non-PostgreSQL dialect (`:92-94`), so every read goes through the
unscoped path. Isolation on those dialects comes from elsewhere — a database per tenant on MySQL,
explicit predicates on SQL Server. **An investigation agent must not assume this adapter is its
tenant fence outside PostgreSQL.**

---

## What already exists

More than a sketch of this usually assumes.

| Piece | Where |
|---|---|
| Read-only, tenant-scoped DB adapter | `engine/readonlydb.go` |
| Replay of a run from its history | `cleatctl replay` (`cmd/cleatctl/replay.go:13-60`) |
| Divergence detection during replay | the stub caller that errors if replay diverges into fresh execution |
| Interactive inspection | `cleatctl debug` |
| Failed runs with full history retained | any `failed` workflow, not only dead-lettered ones -- `store.FailWorkflow` (`engine/store_lifecycle.go`) never purges `event_history`; it survives until `--retention-days` sweeps it, default 30 days (cleat#1973) |
| W3C trace correlation | `trace_id` column, from the `traceparent` header (`cmd/cleat-worker/server.go:970`, generated at `setup.go:1786-1791`, carried by `engine.WithTraceID`) |
| Metrics export | `monitoring/prometheus`, and the `datadogexport` plugin |
| Audit of actions | `auditlog` |

The `trace_id` is the interesting one: the join key to external tracing is already present, already
populated, and already a standard.

**Replay survives the read-only constraint**, which is worth stating because it looks like it
should not. `cleatctl replay` mutates nothing — it loads history and WASM and re-executes in a
sandbox. So a read-only agent can still *run experiments* rather than only speculate, and the
write boundary stays hard.

---

## Architecture sketch

Three stages, deliberately in this order:

**1. Narrow (SQL over the durable record).** Which runs, which steps, which window, which tenant.
This is where the distinctive power is: `event_history` and `workflow_instances` are typed rows
with `error_code`, `service`, `operation`, `step`, so "17 runs failed at step 4 with the same error
code against the same service" is a query rather than a log-clustering heuristic.

**2. Explain (fetch external telemetry for that slice).** Having a specific worker, a specific
40ms window and a specific `trace_id`, pull the traces, the metrics and the logs for exactly that.
Bounded by construction.

**3. Propose (a hypothesis, and ideally a replay that tests it).** The output is a written finding,
a candidate diff, and — where the hypothesis is about workflow code — a replay result. A human
applies anything.

Two non-negotiables that fall out of the repo's own culture: the agent **states what it did not
look at**, and any claim it makes that could have been checked and was not is marked as such. A
report that cannot say "unmeasured" will say "fine" instead.

---

## Competitive landscape

Three adjacent categories. The honest conclusion is in the fourth section, and it is not the
flattering one.

### A. AI SRE / AIOps agents

The category is real and well funded as of 2026. Autonomous investigation agents include
[Resolve.ai](https://resolve.ai/glossary/what-is-ai-sre) — which raised a $125M Series A in
February 2026 and runs a graduated trust model that executes fixes for well-defined patterns —
and [NeuBird](https://neubird.ai/blog/top-ai-sre-tools), with Kubernetes-focused entrants
(Komodor, Metoro) and incident-management-first tools (Rootly). Gartner is quoted projecting 70% of
enterprises deploying agentic infrastructure agents by 2029, from under 5% in 2025.

**What they have that this would not:** maturity, integrations with every telemetry vendor,
Kubernetes and cloud-infrastructure knowledge, and remediation runbooks.

**What they lack:** a causal record. They reason over logs, metrics and traces — sampled,
unstructured, and lossy. They cannot replay anything, and they cannot check their own hypotheses.

### B. Durable execution peers — and this is where the honesty is

**The replayable event history is not a cleat advantage.** Temporal has had it for years, ships
[time-travel debugging](https://temporal.io/blog/time-travel-debugging-production-code) — download
an execution's history, check out the code that was running in production, and step through it in a
debugger — and its Web UI shows a complete visual trace of inputs, outputs, retries and failures.
DBOS goes further in a different direction with
[database time travel](https://www.dbos.dev/blog/database-time-travel), querying application state
as of a point in time.

So any pitch of the form "cleat can do this because it has a durable record" is a pitch Temporal
and DBOS can make too, and Temporal can make it with far more maturity. **The substrate is table
stakes among durable execution engines, not a differentiator.**

What appears genuinely different is narrower, and it is two things:

1. **The engine knows what each external call *is*.** A plugin host call is registered with
   `FuncOptions` declaring whether it is idempotent and whether it yields the same value on replay
   (`plugin/plugin.go:208-243`). So the record does not merely say "an activity ran" — it says this
   was `llm.chat`, which is neither safe to re-run nor stable, and that was `pgvector.search`, which
   reads a mutable index. An investigating agent can reason about **what is safe to retry** instead
   of guessing. Temporal activities are opaque user code; the platform cannot know one of them
   charged a card.

2. **One database.** Workflow state and plugin state — `kvstore`, `auditlog`, `rate_counter`,
   feature flags, OAuth sessions, blob metadata — are tables in the same PostgreSQL instance, under
   the same RLS. A single SQL statement can join a failing run against the flag that was set, the
   rate limit that fired and the audit row for who changed what. Temporal's history lives in the
   Temporal service's own store, separate from application data, so that join is a join across
   systems. DBOS, being Postgres-embedded, is the closest competitor on this axis and the one to
   watch.

### C. Time-travel and deterministic debuggers

`rr`, Undo LiveRecorder, replay.io, and Antithesis are the intellectual relatives — record-and-replay
with deterministic re-execution. They are stronger than cleat at what they do and are aimed
elsewhere: single-process or single-application debugging, mostly in development, without
multi-tenancy, production telemetry, or a notion of a tenant at all.

### D. The gap

The AI SRE tools have **agents over a weak substrate**. The durable execution engines have a
**strong substrate with human-driven tooling** — Temporal's time-travel debugging puts a human in a
debugger, not an agent over a query interface.

Three searches on 2026-09-14 surfaced no product joining them:

    "AI SRE agent incident investigation autonomous root cause analysis 2026"
    "Temporal DBOS durable execution time travel debugging event history AI agent"
    "AI agent query workflow execution history debug durable workflows Temporal Cloud assistant"

The third is worth noting for what it *did* return: Temporal markets heavily on **durable execution
for AI agents** — run your agent reliably on Temporal. That is the inverse of this proposal, which
is **an AI agent for durable execution** — investigate the durable record. The two are easily
confused in a sentence and are opposite directions.

**An absence of search results is not proof that nobody is doing this.** Three queries, one day,
one engine, US-only results. The claim is that no such product surfaced, not that none exists.

---

## Security model

**Read-only is the fence, and it is not sufficient.** Two things it does not stop:

**Exfiltration.** Read access to event history is read access to every recorded prompt, payload and
credential. `engine.Redact` does run over recorded call arguments and responses
(`cmd/cleat-worker/setup.go:2226-2227`, `:3840-3841`) — but it is a field-name heuristic
(`isSensitiveField`, `engine/redact.go:36-44`), so a secret in a field named `config`, or PII inside
an `llm.chat` `messages` array, is stored in the clear. An agent that can be induced to summarise
tenant B's data into tenant A's incident report is a cross-tenant breach with read-only access
throughout.

**Resource exhaustion.** "Complex queries over the whole event history" against the production
database is a denial of service, and it competes for the same connections the workers need — see
`connectionBudget` (`cmd/cleat-worker/connection_budget.go`). A read replica is not optional.

The control set that follows:

- a PostgreSQL role with no write grants — the fence, below the adapter
- RLS tenant scoping, verified on the `Query` path and not only on `Begin`
- a read replica, with `statement_timeout` and result-row caps
- prompt-injection treated as a given: the agent's **tool permissions** are the boundary, never its
  judgment
- **every query the agent runs is audited** — `auditlog` exists for this, and an investigator that
  is not itself investigable is not acceptable in the environments that would buy this

---

## What this does not do

- **Infrastructure root cause.** Cleat does not know why the database got slow or the network
  partitioned. It knows when to look.
- **Repair.** By design, per the above.
- **Incidents with no workflow involvement.** If nothing durable ran, there is no spine, and this
  degrades to an ordinary AIOps tool with fewer integrations.
- **Replace an observability vendor.** It consumes telemetry; it does not collect it.

---

## Prerequisites

This is a **ceiling** feature — it makes someone want cleat. `docs/full-stack-readiness.md` argues
the currently filed gaps only raise the **floor**: they remove reasons to reject and add no
magnetism. This is the first thing discussed that adds magnetism, and it should still come second,
because it depends on floor items:

- **#1570, per-tenant secrets** — otherwise the agent reads credentials in the clear, and the
  redaction weakness above becomes this feature's headline risk rather than a footnote.
- **#1571, a queryable read model** — the narrowing stage is exactly the cross-run querying that
  issue is about.
- **#1565, egress policy** — if the agent fetches external telemetry, it makes outbound calls.
- ~~Verification of the `ReadOnlyDB.Query` tenant-scoping property.~~ **Done** — see above. It
  holds on PostgreSQL with a tenant in context; elsewhere the fence is the database or the dialect's
  own scheme, not this adapter.

---

## Open questions

1. **Is read-only SQL the right interface, or a curated tool surface?** Raw SQL is maximally
   powerful and maximally abusable; a fixed set of parameterised queries is safer and blunter.
2. **Where does the agent run?** In the cluster with database access, or outside with a narrow API?
   The second is far easier to secure and much less capable.
3. **Which telemetry integrations first?** `datadogexport` and `monitoring/prometheus` already
   exist and are the obvious two.
4. **Does it see across tenants?** An operator debugging a platform-wide incident wants to. A
   tenant's own support engineer must not. These are two different products with one name.
5. **Open or paid?** Plausibly a paid tier: it is additive rather than a crippled core, it has
   honest marginal cost, and it leaves the event history and `cleatctl replay` open. Gating tenant
   isolation would be poisonous; gating an investigator is not.
6. **How is its output evaluated?** A confident wrong root cause is worse than none. The replay
   check gives machine-verifiable evidence for workflow-code hypotheses and nothing for
   infrastructure ones.

---

## What was verified

**Read from the tree at `654d6f84`:** `engine/readonlydb.go`'s `Exec` denial and its
`SET TRANSACTION READ ONLY` / `beginTenantTx` path; `cmd/cleatctl/replay.go`'s doc comment and its
loading of instance, history and WASM with a divergence-detecting stub caller; the `trace_id`
plumbing at `server.go:970` and `setup.go:1786-1791`; `FuncOptions`' two replay properties; every
`engine.Redact` call site and `isSensitiveField`.

**From web search on 2026-09-14**, with the three queries reproduced above: the AI SRE vendor
landscape and the Resolve.ai funding figure; Temporal's time-travel debugging and event-history
positioning; DBOS's database time travel. **None of this was verified against the products
themselves.** Vendor marketing is the source, and it is the least reliable input in this document.

**Since verified**, and it was the single property this design most depends on: `ReadOnlyDB.Query`
and `QueryRow` do carry tenant scope on PostgreSQL when a tenant is in context. Read from
`engine/readonlydb.go:47-78` and `engine/plugindb_tenant.go:91-167`, including both branches on
which `beginTenantTx` returns `(nil, nil)`. This was a code read, not a live probe against a
seeded foreign-tenant row — which is the stronger check CLAUDE.md prescribes and which nobody has
run here.

**Not verified:** that no competing product exists, which three searches cannot establish.

**Asserted, not measured:** that this approach diagnoses incidents faster. Nobody has built it.
