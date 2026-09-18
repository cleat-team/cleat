# What "good enough" means for the full-stack playbooks

**Status:** assessment, open for review. Drafted 2026-09-14 against `develop` at `654d6f84`.
Companion to [`docs/playbooks/`](playbooks/README.md) and
[`docs/multi-tenant-serving-design.md`](multi-tenant-serving-design.md).

Written after drafting the four playbooks, to answer two questions the exercise raised: **what
would make cleat materially better at these designs**, and **is it actually a win against how the
industry builds them today**.

Every file reference was opened against that SHA. Where a claim was reasoned rather than measured,
it says so; the closing section separates the two. One claim here is backed by a probe that was run
and then deleted, and its output is reproduced verbatim.

---

## Contents

- [Why "good enough" is the deciding concept](#why-good-enough-is-the-deciding-concept)
- [Differentiators and table stakes](#differentiators-and-table-stakes)
- [Where each area stands against its bar](#where-each-area-stands-against-its-bar)
- [The gap list](#the-gap-list)
- [Is cleat a real win?](#is-cleat-a-real-win)
- [What was verified](#what-was-verified)

---

## Why "good enough" is the deciding concept

The playbooks all make one argument: replace five vendors with one binary and a database. That
argument has a specific failure mode, and it is worth stating precisely because it governs
everything else.

**Consolidation value is a product, not a sum.** A buyer does not average the pieces. They evaluate
each area against their own most demanding requirement, and a single zero collapses that area's
contribution entirely — *and costs more than never having offered it*, because they now run a
vendor **and** a half-used subsystem, with the integration seam between them.

Concretely: a prospect needs SAML. `oauthprovider` speaks OIDC. They buy a vendor. Now cleat's
identity story is not "one less vendor" but "an unused table and a middleware we had to reason
about anyway".

*That example has since been resolved, and the resolution illustrates the rule rather than escaping
it — see [`enterprise-identity-decision.md`](enterprise-identity-decision.md). The answer was not to
build SAML but to accept a generic OIDC issuer and let the customer terminate SAML, which clears the
bar without owning the dangerous part. Clearing a bar cheaply is exactly the move this section
argues for.*

Two consequences follow, and they point in opposite directions:

- **Clearing the bar in one more area is worth more than improving an area that already clears
  it.** The return is not linear; it is the difference between an area counting and not counting.
- **Being honest about an area you have not cleared is nearly free.** A playbook that says "buy
  WorkOS for SAML" loses nothing — the buyer was going to anyway — and it buys credibility for the
  areas where the claim *is* strong.

So the strategy that follows from this document is not "build everything". It is: **pick the areas
where the bar is genuinely reachable, clear them properly, and be explicit about the rest.**

**"Good enough" is also not the same as "feature parity".** Nobody needs cleat's flags to beat
LaunchDarkly. They need them to be good enough that adding LaunchDarkly is not worth the
integration. That bar is much lower, and it is reachable in several areas that currently miss it by
a small margin.

---

## Differentiators and table stakes

Not every playbook is trying to win. Two of the four exist to *clear a bar*, and conflating those
jobs leads to over-investment in the wrong place.

| Playbook | Job | Why |
|---|---|---|
| Integration hub | **Differentiator** | Per-tenant integrations is exactly where every conventional option prices badly. Nobody has a good answer. |
| B2B control plane | **Differentiator** | The only option with tenancy in the data model, so marginal cost per tenant is structurally lower. |
| AI agent platform | **Differentiator, short half-life** | The replay-as-audit-trail property is genuinely distinct and timely — and the category moves fast. |
| Order lifecycle | **Table stakes** | Vanilla. Every approach does this well, and Temporal does it with more maturity and a bigger ecosystem. |

The order-lifecycle playbook's job is therefore **not** to be compelling. It is to demonstrate that
cleat is not *worse* at the thing everyone will check first, so that the conversation can move to
the areas where it is better. Judged as a differentiator it is weak; judged as a bar-clearing
exercise it is doing its job, and that is the right frame.

This is the same distinction as the section above, applied to playbooks instead of components. Most
of the value is in clearing bars; a smaller and more visible part is in the differentiators.

---

## Where each area stands against its bar

The bar is the buyer's most demanding common requirement, not parity with the category leader.

| Area | The bar | Status | Gap |
|---|---|---|---|
| Durable workflows | Survives crashes, compensates, replays | **Clears** | — |
| Multi-tenant isolation | Enforced below application code | **Clears on PostgreSQL and SQL Server** | MySQL has no row-level security; see below |
| Rate limiting | Cluster-wide, per tenant, observable | **Clears, badly configured** | `db` mode exists; default is `memory`, fallback is silent, fails open |
| Feature flags | Targeting, percentage rollout, kill switch | **Probably clears** | No experimentation platform — rarely the deciding requirement |
| Audit | Who did what, retained, exportable | **Partial** | HTTP-level, not semantic; no tamper-evidence or export tooling |
| Identity | SSO, and SAML/SCIM for enterprise | **Decided, not yet built** | SAML via a customer-run terminator (#1582); SCIM deferred |
| Secrets | Encrypted at rest, rotatable | **Does not clear** | `kvstore` is config storage, not a secrets manager |
| Outbound network | Cannot be used to attack the host | **Does not clear** | No SSRF guard, no egress allowlist |
| Reading your own data | Query a tenant's entities | **Does not clear** | One key at a time from a JSONB column |
| Deploy and rollback | Atomic, fast, reversible | **Clears** | Pointer flip, version-pinned in-flight runs |

**Four of ten miss.** Two of those four — egress and secrets — are the kind that end an evaluation
rather than lose a comparison, because they are security answers rather than feature answers. The
read model is the one that costs every adopter the most hand-written code.

### Tenant isolation is not the same mechanism on every dialect

"Enforced below application code" is true on two of the three supported databases and not the third, and the row above says "Clears" without that distinction being obvious.

| | How a tenant boundary is enforced |
|---|---|
| **PostgreSQL** | Row-level security policies, plus optional per-tenant roles |
| **SQL Server** | `CREATE SECURITY POLICY ... WITH (STATE = ON)` — a FILTER predicate and BLOCK predicates after INSERT and UPDATE, reading `SESSION_CONTEXT` |
| **MySQL** | **Nothing in the database.** `applyTenantScoping` returns without acting, and `grep -ril "row.level security\|CREATE POLICY" migrations/mysql` finds nothing |

On MySQL, isolation is the `AND tenant_id = ?` predicate written into every query — which is real, and is guarded: `engine/mysql_tenant_predicate_test.go` (cleat#1031) refuses any MySQL statement touching a tenant-scoped table without one.

But the guard is **static analysis over the source**, not enforcement by the database. A missing predicate on PostgreSQL or SQL Server returns no rows; on MySQL it returns another tenant's. As that test's own comment puts it: *"on this dialect a missing predicate has nothing whatever behind it, on any connection, for every tenant-scoped table."*

That is a defensible position — MySQL offers nothing better — but it is a different guarantee, and a buyer comparing deployment targets should be told which one they are getting rather than reading "RLS" and assuming it is uniform.

Identity was the fourth and is the one that has moved: it is no longer an open question but a
recorded decision ([`enterprise-identity-decision.md`](enterprise-identity-decision.md)) awaiting a
small piece of work (#1582) — cleat accepts a generic OIDC issuer and the customer terminates SAML.
It still counts as missing here because **a decision is not a shipped capability**: the row changes
when the issuer support exists, not when the reasoning is written down. SCIM stays deferred, and
enterprise identity is not cleared by SAML alone in any case — SOC 2 is the larger gate.

---

## The gap list

Eight proposals, ranked by value ÷ effort. Each is filed as its own issue; the issue carries the
evidence and the open design questions, and this section is the summary and the ordering argument.

| | Gap | Issue | Kind | Clears which bar |
|---|---|---|---|---|
| 1 | Egress policy for outbound HTTP — **shipped** | [#1565](https://github.com/cleat-team/cleat/issues/1565) | security | Outbound network |
| 2 | Deterministic plugin order — **shipped** | [#1566](https://github.com/cleat-team/cleat/issues/1566) | bug | — (correctness) |
| 3 | `cleat init --template fullstack` — **shipped** | [#1567](https://github.com/cleat-team/cleat/issues/1567) | adoption | — (time to evaluate) |
| 4 | `Host` → tenant binding — **shipped** | [#1568](https://github.com/cleat-team/cleat/issues/1568) | security | Per-tenant URLs |
| 5 | Counter-based tenant quotas — **shipped**, defaults off | [#1569](https://github.com/cleat-team/cleat/issues/1569) | feature | Commercial packaging |
| 6 | Per-tenant secrets — **shipped**; rotation absent | [#1570](https://github.com/cleat-team/cleat/issues/1570) | security | Secrets |
| 7 | A queryable read model — **declined** | [#1571](https://github.com/cleat-team/cleat/issues/1571) | feature | Reading your own data |
| 8 | `chat_stream` → SSE bridge — **shipped** | [#1572](https://github.com/cleat-team/cleat/issues/1572) | feature | Interactive AI |

Related and filed separately while writing the playbooks:
[#1563](https://github.com/cleat-team/cleat/issues/1563), the compiled-module cache having no
eviction — a prerequisite for any per-tenant artifact deploy.

**Four of the eight are security or correctness**, and those are the ones whose absence ends an
evaluation rather than losing a comparison. Items 1, 4 and 6 are the security three; item 2 is a
plain bug and the cheapest thing on the list.

> **Status, 2026-09-18.** Seven of the eight have shipped. Item 7, the queryable read model, was
> **declined** rather than built (#1571, closed NOT_PLANNED) — its gap is intact and this row says so
> rather than quietly dropping it. `scripts/check-readiness-doc.py` now fails CI if a row here
> disagrees with the issue it cites, in either direction.
>
> This table had gone 125 commits without an update and still said four of ten bars missed, while
> nine of the ten gaps had closed. The failure direction is worth naming: most stale documents
> oversell, and an overclaim gets contradicted the first time someone tries the feature. This one
> **undersold**, and an underclaim just quietly loses an evaluation with nothing to contradict it.

### 1. Egress policy for outbound HTTP — *security, highest priority*

`http.fetch` is reachable from any workflow: `dbServiceCaller.call` dispatches on
`service == "http" && operation == "fetch"` (`cmd/cleat-worker/setup.go:172-174`), and
`handleHTTPFetch` (`:236-280`) builds the request from a guest-supplied URL and method with a plain
`&http.Client{Timeout: 30 * time.Second}`. There is no allowlist, no denied-range check, and no
restriction on redirects — so a guest can reach `169.254.169.254`, any RFC1918 address, and the
worker's own admin port.

That is acceptable while every workflow is yours. It is disqualifying the moment a tenant supplies
one, which is the premise of three of the four playbooks. A second implementation exists at
`cleat/embedded/runner.go:417` and has the same shape.

The model to copy is already in the tree: `engine/wasi_policy.go` enumerates what is permitted with
a stated reason per entry. Egress deserves the same treatment.

### 2. Deterministic plugin order — *a bug, and the cheapest fix here*

`Discover()` builds its plugin list via `topologicalSort` (`plugin/registry.go:155-196`), whose
Kahn's-algorithm queue is seeded by `for name, deg := range inDegree` (`:172`) — a map iteration,
which Go randomises. There is no tie-break. Plugins with no `Requires` edges therefore come out in
an arbitrary order that **differs between worker processes and between restarts**.

That order determines plugin `Init` order and, because each middleware wraps the previous
(`cmd/cleat-worker/main.go:1001-1010`), the **nesting order of `oauthprovider`, `ratelimiter` and
`auditlog`**. Measured with a throwaway probe over 500 sorts of three independent plugins:

    PROBE distinct orderings over 500 calls: 3
    PROBE   oauth-provider,rate-limiter,audit-log         131
    PROBE   audit-log,oauth-provider,rate-limiter         239
    PROBE   rate-limiter,audit-log,oauth-provider         130

The built-in `auth.Middleware` wraps outermost (`main.go:1447`), so the tenant is always in context
before any plugin middleware runs — which is why this has not bitten. What is unstable is ordering
*among* the plugin middlewares: whether `auditlog` sees `oauthprovider`'s session depends on the
run. A sort is a one-line fix; declared ordering is the fuller one.

### 3. `cleat init --template fullstack` — *highest leverage per hour*

`cleat init` already has a template mechanism with four templates — `basic`, `agent`,
`agent-python`, `workflow` (`cmd/cleat/init.go:24`, `:33-45`). Adding a `fullstack` template that
wires `oauthprovider` + `ratelimiter` in `db` mode + `auditlog` + a workflow + a front-end build is
therefore a template, not a subsystem.

This matters more than its size suggests. The playbooks describe assembly that currently takes real
effort, and the gap between "documented" and "scaffolded" is most of what decides whether anyone
finishes an evaluation.

### 4. `Host` → tenant binding — *unblocks the vhost story*

There is no table mapping a hostname to a tenant, and no middleware asserting that the
authenticated tenant owns the requested `Host`. Without it, per-tenant URLs cannot be done safely:
tenant A's valid key works against tenant B's URL, which is a confused deputy and the seed of
multi-tenant cache poisoning.

A `tenant_domains` table plus one assertion in the middleware chain. Small, well-defined, and its
absence is why the serving design doc could not make the vhost story concrete.

### 5. Counter-based tenant quotas — *the commercial unit*

`tenant_settings` has exactly three knobs, all durations: `wasm_instance_timeout_ms`,
`wasm_wall_clock_ceiling_ms`, `host_retry_budget_ms`
(`migrations/postgres/039_tenant_settings.sql`), clamped by `ClampToCeiling(tenant, ceiling
time.Duration)` (`engine/tenant_settings.go:97`).

The clamp semantics are right — a tenant may lower and can never raise — but the dimension is
wrong for how products are actually sold. The commercial unit is a count: runs per month, tokens,
GB stored, events ingested. Duration ceilings bound a single invocation and say nothing about
aggregate consumption.

Note this is a genuinely new mechanism rather than three more columns: a counter needs accumulation
and a reset period, which a `min()` clamp does not.

### 6. Per-tenant secrets — *does not clear the bar*

Every playbook needs per-tenant credentials — PSP keys, CRM tokens, model API keys. `kvstore` is a
versioned JSONB store, not a secrets manager: no envelope encryption, no rotation, no access
audit.

One thing I expected to be worse and is not: **event-history redaction does reach plugin
host-function arguments.** `newEvents[i].Request` and `.Response` are passed through
`engine.Redact` before persisting (`cmd/cleat-worker/setup.go:2226-2227`, and again at
`:3840-3841`). So the open question left by the AI playbook is answered — the recorded call
arguments are redacted.

The qualification is that `Redact` is **field-name heuristic**: `isSensitiveField`
(`engine/redact.go:36-44`) substring-matches a pattern list, plus a JWT-shape check. A credential in
a field named `config`, or PII inside an LLM prompt's `messages` array, passes through untouched.
That is a reasonable default and it is not a storage solution.

### 7. A read model worth the name — *largest design work, largest product payoff*

`query_state` is a **JSONB column on `workflow_instances`**, written by the finalize procedure
(`migrations/postgres/003_procedures.sql:52`, `:65`, and redefined in later migrations), and read
one key at a time by `GetQueryState(ctx, id, key)` behind
`GET /api/workflows/{id}/state?key=` (`cmd/cleat-worker/server.go:1671`). Listing keys is refused
deliberately (cleat#1119).

So "show me this tenant's open orders" — which every playbook needs — has no answer except scanning
`workflow_instances` and extracting JSONB yourself. Every adopter writes the same projection glue.

A first-class projection mechanism, where workflows publish into a tenant-scoped, indexed,
queryable view, would remove the single largest piece of hand-written code from all four playbooks.
This is the item most likely to change how the product *feels*, and the one needing the most design.

### 8. A `chat_stream` → SSE bridge — **shipped**, cleat#1572

`GET /api/workflows/{id}/stream`. See
[Streaming tokens to a client](how-to/stream-tokens-to-a-client.md).

**The design question this section raised was answered, and the answer was not the one implied
here.** This said "a streamed token is by definition not yet a recorded step". It is: every chunk
is written to `event_history` as it arrives, under a `(step, index)` pair — measured on the issue,
with a negative control, after the same claim had been retracted once by reading the call graph
and got wrong twice by reading alone.

That changed the feature rather than just the wording. On the premise written here, a reconnecting
client could only be told "start again"; on the measured one it is served exactly what it missed,
from the durable record. The decision that stands — **the live tail is a preview, the recorded step
is the truth** — is the same sentence with a much smaller gap behind it: preview and truth differ
in latency, not in content.

**What remains open:** the live tail is worker-local. A reader that lands on a worker not executing
the run gets the durable history and is told `"live": false`. Routing a reader to the right worker
is not solved.

---

## Is cleat a real win?

Qualified yes, and the qualification carries more information than the yes.

**What is genuinely defensible.**

*The control-plane consolidation is real, not marketing.* One versioned artifact set with atomic
promotion and rollback across front-end, API and workflow is something you cannot buy. Teams burn
serious effort on exactly that coordination, and no conventional stack can roll three systems back
together.

*Marginal cost per tenant being a row is structurally true*, and it is the sharpest edge cleat has.
Every competitor prices tenancy as an infrastructure object because none of them has tenancy in the
data model.

*The `llm.chat` replay semantics are the best thing in the codebase.* Cost control, determinism and
the audit trail from one mechanism, arriving as AI governance becomes a procurement question.

**Where to be skeptical.**

*Consolidation competes on "fewer vendors" against products that are each better at their one
thing.* See the opening section: the bar is set by the buyer's most demanding requirement, and one
miss costs more than never having offered the area.

*Depth beats breadth, and the plugins are mostly thin.* `jobqueue`'s own package comment says
workflow dispatch "comes later". Twenty shallow plugins are worth less than three deep ones.

*The WASM guest model is a real adoption tax.* Go-compiled-to-WASM with determinism constraints is
harder than writing a Python function, and Temporal's SDKs run native code. The payoff is real; the
cost lands on every developer on day one.

*"Postgres for everything" has a crossover point*, and for event-heavy workloads it is not
especially high. Being precise about where it lies is worth more than avoiding the subject.

**Where the win actually is.** A company building a multi-tenant B2B product, with per-tenant
workflows or integrations, already on Postgres, currently assembling four to six vendors and
feeling the coordination pain. That is a real and sizable segment, and the two differentiator
playbooks speak directly to it.

**The strongest signal is the codebase, not the feature list.** The `FuncOptions` two-property
distinction, the WASI table with a reason per entry, tests asserting that a *policy* scopes rows
rather than only the query, and this repository's whole documented epistemics. That is unusually
careful engineering, and it is the best available predictor that the "good enough" bar keeps
getting met in more areas over time. Most of the gaps above are gaps of *scope*, not of care — and
that is the cheaper kind to close.

---

## What was verified

**Read from the tree at `654d6f84`:** the `http.fetch` dispatch and handler, and the second
implementation in `cleat/embedded/runner.go`; `Discover`, `topologicalSort` and the plugin
middleware wrapping loop; `cleat init`'s template list; the absence of any hostname-to-tenant
table; `tenant_settings`' three columns and `ClampToCeiling`'s signature; every `engine.Redact`
call site and `isSensitiveField`'s implementation; `query_state` as a JSONB column plus the
single-key read path; the `chat_stream` registration and `eventstore`'s SSE route.

**Measured:** the plugin-ordering non-determinism, by a probe registering three independent plugins
and sorting 500 times. Its output is quoted verbatim above. The probe was deleted rather than
committed; the fix's own regression test belongs with the fix.

**Reasoned about, not measured:** every effort estimate, every cost claim, and the whole of
[Is cleat a real win?](#is-cleat-a-real-win). No deployment, no benchmark, no user interview.

**A caveat on the judgment in the final section.** It is an assessment of architecture and code
quality, not of market fit or operational reality. Three claims made while producing the playbooks
were wrong and were corrected by a reader who knows the system — the rate limiter, browser
authentication, and SSE support — each time because a component was judged from its doc comments
and its neighbours rather than from the code path that answers the question. Weight this document
accordingly.
