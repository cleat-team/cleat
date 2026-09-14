# Serving web front-ends and HTTP APIs from a cleat worker pool

**Status:** exploration, open for review. No decision taken; nothing here is claimed in `tiers.yaml`
and nothing here is built.
**Drafted:** 2026-09-14, against `develop` at `654d6f84`.
**Scope:** whether a pool of cleat workers should also serve (a) a web front-end and (b) internal
microservice HTTP APIs, for many tenants at once, each tenant on its own URL.

Every file reference below carries a line number against that SHA and should be re-derived rather
than trusted. Where a claim was reasoned about rather than measured, it says so — see
[What was verified and what was not](#what-was-verified-and-what-was-not) at the end, which is the
section to read first if you are about to act on any of this.

**No vendor prices appear in the competitive analysis.** A published price is a census of a moving
population, and this repo's own experience is that every such number was wrong when re-checked. The
analysis is about cost *structure* — what scales with what — which is durable, plus a list of the
figures to go and measure.

---

## Contents

- [The question](#the-question)
- [What already exists](#what-already-exists)
- [Three tiers, and which are worth doing](#three-tiers-and-which-are-worth-doing)
- [One URL per tenant: the trust chain](#one-url-per-tenant-the-trust-chain)
- [Competitive analysis](#competitive-analysis)
- [Security](#security)
- [Denial of service](#denial-of-service)
- [Resource contention](#resource-contention)
- [How a front-end change gets deployed](#how-a-front-end-change-gets-deployed)
- [Recommendation](#recommendation)
- [Open questions](#open-questions)
- [What was verified and what was not](#what-was-verified-and-what-was-not)

---

## The question

Cleat is a durable workflow engine: a worker pool, a database, and WASM guests. It also already
runs an HTTP server and already ships a web UI. The question is whether that HTTP surface should
grow into a general serving tier — tenant-facing front-ends and internal APIs on the same pool —
or whether that is a category error.

The short answer is that cleat already owns the hard half of a multi-tenant application-delivery
platform and does not know it. What it is missing is the edge: the bytes, the certificates, and the
browser security model. The identity picture is better than it first appears — see the correction
under [One URL per tenant](#one-url-per-tenant-the-trust-chain) — but the enforcement boundary sits
higher than the database, which is the thing to be precise about.

---

## What already exists

**A versioned, content-addressed, tenant-scoped artifact pipeline.** `POST /api/definitions`
(`cmd/cleat-worker/server.go:2386`) accepts base64 WASM, auto-increments a version when none is
given, and stores it against the caller's tenant. Workers cache the *compiled* module keyed by
xxhash of the bytes (`engine/backend_wasmtime.go:68-70`, populated at `:723`), and the cache is
shared across executions — the code says so explicitly at `:727`, "Do NOT close the module — it's
cached and shared."

This is the expensive part of an edge-function or front-end delivery system, and it is already
built, already tenant-scoped, and already load-bearing for workflows.

**Traffic splitting.** Routing rules exist per definition name — `handleSetRoutingRule`
(`cmd/cleat-worker/server.go:1358`), `handleListRoutingRules` (`:1331`), `handleRemoveRoutingRule`
(`:1416`). That is canary and blue-green infrastructure, built for workflow versions, structurally
reusable for any other versioned artifact.

**A sandbox designed for untrusted guests.** `engine/wasi_policy.go` is an explicit allow/refuse
table over WASI preview1 with a stated reason per function: no preopened directories, no `sock_*`
family, clock and randomness intercepted and replaced by durable sources. A test
(`TestTheWasiPolicyCoversEveryOfferedFunction`, named in that file's header comment) fails if the
offered set and the table diverge in either direction, so a dependency bump cannot silently widen
it.

**Execution limits.** wasmtime with epoch interruption on a process-global 50 ms ticker
(`engine/backend_wasmtime.go:169-190`), per-store wall-clock deadlines (`:306`), opt-in fuel
metering (`:149-151`, `:309`), and a default linear-memory ceiling of 32 MB
(`DefaultWasmtimeMemoryLimitBytes`, `engine/wasmtime_options.go:33`).

**An HTTP server with tenant-scoped auth.** Bearer API keys of the form `cleat_sk_<key>`, SHA-256
hashed and resolved to a tenant (`auth/middleware.go:92-109`). Per-IP and per-tenant token buckets
(`cmd/cleat-worker/server.go:2504-2660`). Body limits that answer 413 naming the limit. Tenant
isolation enforced at the database — RLS, and optionally a connection pool per tenant
authenticating as that tenant's PostgreSQL role (`--tenant-isolation=role`).

**A front-end, served badly.** The admin dashboard is compiled into the binary —
`//go:embed web/dist` at `cmd/cleat-worker/server.go:28`, mounted as a catch-all at
`cmd/cleat-worker/app.go:69`. A front-end change therefore requires rebuilding and redeploying the
Go binary on every worker. This is the concrete wart that motivates the whole exercise.

**A cautionary tale in the same file.** `app.go`'s doc comment on `registerRoutes` records that the
SPA catch-all answered *every* unmatched path — `/api/` included — with 200 and `index.html`, which
hid seven registered-nowhere endpoints for the life of the file. Path-based separation of concerns
on a single shared mux has already failed here once, which is the direct argument for separate
listeners below.

---

## Three tiers, and which are worth doing

"Serving from the worker pool" is three different proposals with three different risk profiles.
Conflating them is the main way this discussion goes wrong.

### Tier A — static assets. Worth doing.

Replace `go:embed` with an asset bundle distributed exactly like a workflow definition: versioned,
content-addressed, stored in the database, cached locally by workers, switched atomically by a
manifest pointer.

Deploy becomes one API call. Rollback becomes a pointer flip. The admin dashboard stops being
coupled to engine releases, and a tenant-supplied front-end becomes possible without a new
subsystem — the version table, the routing rules and the module cache already do everything except
serve bytes.

Low risk, removes a real wart, and it is the prerequisite for everything else.

### Tier B — command-side APIs backed by durable runs. Already the thesis.

A `POST /orders` that starts a durable workflow gets idempotency, retry, dead-lettering and
exactly-once side effects for free. `Idempotency-Key` is already honoured on start
(`cmd/cleat-worker/server.go:815`), on dead-letter reprocess (`cmd/cleat-worker/app.go`, in
`handleDeadLetterReprocess`) and on schedule creation (`server.go:2246`).

This needs documenting and packaging far more than it needs building. It is the strongest thing
cleat can say about HTTP APIs and it is largely true today.

### Tier C — read APIs served by durable runs. Do not.

The latency and cost arithmetic does not work.

The default poll interval is 500 ms (`cmd/cleat-worker/config.go:82`), and the `LISTEN`/`NOTIFY`
fast path is PostgreSQL-only — `startNotifyListener` is built on `lib/pq`
(`cmd/cleat-worker/notify.go:27`). On MySQL or SQL Server the median wait before a started run is
even claimed is therefore around half the poll interval, before any work happens.

The write cost is worse than the latency. A run costs an insert into `workflow_instances`, a claim
UPDATE, event-history writes per step, and a completion write. Serving `GET /widgets` that way is
write amplification against the durable store, paid on every read, retained until a retention sweep
removes it.

Reads must come from somewhere else. Two candidates already exist or nearly do:

- **Published query state.** `GET /api/workflows/{id}/state?key=` (`server.go:1671`) reads a
  key a workflow published via `set_query_state`. No guest execution, one indexed read. This is
  the CQRS read side and it is already shipped.
- **A synchronous non-durable handler tier.** Same wasmtime backend, same module cache, but no
  instance row, no event history, no claim — and deliberately a *smaller* host ABI: no
  `cleat_call`, no promises, no durable sleep, no signals. A pure function of request and injected
  capabilities.

That second one is a different product sharing a runtime. It is coherent — the sandbox, the
artifact channel, the module cache and the tenancy model are all reusable — but it roughly doubles
the security surface and introduces a latency-sensitive workload into a throughput-tuned scheduler.
It should be its own decision with its own design document.

---

## One URL per tenant: the trust chain

This is where the design gets genuinely hard.

**A `Host` header is not a credential.** An API key is something the client proves; a hostname is
something the client asserts. `Host` may decide *which bundle of public bytes to serve*. It must
never decide *which tenant's data a query is scoped to*.

Cleat already contains a mechanism with exactly this shape. `--tenant-resolver header:<name>`
(`cmd/cleat-worker/config.go:184`) installs a middleware (`cmd/cleat-worker/main.go:1510-1522`)
that parses a tenant UUID out of a client-supplied header and puts it in the request context —
which is precisely what `tenantFor` (`cmd/cleat-worker/server.go:167`) and every scoped-store
lookup read.

In the default configuration that is safe, and it is worth being precise about *why*, because the
reason is ordering rather than validation. The header middleware is installed **outside** the auth
middleware, so it runs first; auth then overwrites the context tenant with the key-derived one
(`auth/middleware.go:108`). The header value is set and then clobbered.

That clobber depends on both middlewares writing the same context key, which they do:
`auth.WithTenantID` is a one-line wrapper over `tenantctx.With` (`auth/middleware.go:25-27`) and
`auth.TenantIDFromContext` over `tenantctx.From` (`:30-32`), both against
`internal/tenantctx`. Had they been distinct keys the two values would coexist and the winner would
be whichever `tenantFor` happened to read — so this is the load-bearing detail, not an aside.

It is **not** clobbered on the paths auth lets through without a key. `auth.Middleware` returns
early via `next.ServeHTTP` for `/healthz`, `/metrics`, and each public pattern —
`POST /ingest/{source_id}` and `GET /oauth/{provider}/callback`, wired at
`cmd/cleat-worker/main.go:1447-1450`. On those routes a client-supplied tenant header survives into
the handler. The same holds with `--require-auth=false`.

Whether that is exploitable depends on whether those handlers consult the context tenant or derive
one from their own path parameter and their own verification. **That was not traced**, and no test
covers the ordering — the only file naming the flag is `cmd/cleat/documented_flags_and_codes_test.go`,
a documentation check. This is a lead for someone to close, not a reported vulnerability.

The rule a vhost design has to adopt:

> **Tenant comes from the credential. The `Host` is then asserted to match it.**

Without the second half, tenant A's valid key works against tenant B's URL. That is a confused
deputy, and it is the seed of every multi-tenant cache-poisoning bug.

### The rest of the vhost subsystem

- **TLS per tenant** is ACME, SNI, certificate storage and renewal. The database is a natural
  shared cert store and `GetCertificate` on SNI is the natural hook, but the certificate
  authority's issuance rate limits then become a hard constraint on tenant onboarding velocity.
  This is a subsystem, not a flag.
- **Cookie scope.** Tenant subdomains under one shared parent mean a cookie set on the parent is
  readable by every tenant. The public suffix list does not help. Either every tenant gets a
  genuinely separate registrable domain, or the cookie discipline has to be airtight and tested.
- **Cache keying.** Anything in front must `Vary` on `Host`, or tenant A's `index.html` is served
  to tenant B. The compiled-module cache is content-addressed and therefore tenant-agnostic by
  construction: safe for integrity, but a cache-hit timing difference discloses that another tenant
  deployed identical bytes. Minor, and better written down than discovered.
- **Browser authentication exists, but stops at the HTTP layer.** An earlier draft of this
  paragraph said browser auth "does not exist here" and called it the single largest gap. That was
  wrong, and the correction is worth more than the claim was: `plugins/oauthprovider/` is an OIDC
  relying party with `GET /oauth/{provider}/login` and `/callback`, session listing and revocation
  (`routes.go:85-88`), and a middleware injecting
  `SessionInfo{TenantID, SessionID, UserEmail}` into the request context (`middleware.go:15-20`).
  So there **is** a session layer and there **is** a principal below the tenant.

  The real gap is narrower and more precise: **RLS is scoped by tenant and knows nothing about
  `UserEmail`.** Cross-tenant access is refused by the database; per-user authorisation *within* a
  tenant is application-layer, and the database will not catch a mistake there. That is the
  boundary to be honest about with an auditor, and it is a smaller and better-defined piece of
  design work than "build browser auth".

  Still genuinely absent: CSRF defence, CORS handling, and SAML/SCIM. And the warning about API
  keys stands — if a browser session maps to a *tenant API key* rather than to an OAuth session,
  the key is in the browser.

---

## Competitive analysis

The alternatives a team would actually weigh:

| | What it is |
|---|---|
| **V** — managed edge platform | Vercel, Netlify, Cloudflare Pages + Workers |
| **C** — cloud primitives | Object storage + CDN, plus serverless functions for the API |
| **K** — container orchestration | Kubernetes with an SSR or nginx deployment, ingress, cert-manager |
| **M** — classic monolith | Rails/Django/Laravel rendering templates from the application process |
| **S** — status quo | `go:embed` the bundle, rebuild and redeploy the worker binary |

### Cost

Argue about structure, not price. Four cost curves matter and they behave differently:

**Fixed floor.** M and S are near zero — the app process you already run serves the bytes. C is
near zero at rest (storage plus per-request). V has a per-seat and per-project floor. K has the
highest floor by a wide margin: a cluster, an ingress controller, cert-manager and the people who
understand them exist before the first byte is served. **Cleat inherits M's floor** — the worker
pool exists to run workflows, and serving is marginal on hardware already bought.

**Marginal cost per tenant.** This is where cleat's structural advantage is real and large. On V,
per-tenant custom domains with isolation is a pricing-tier conversation and often a per-domain
charge. On K it is an Ingress object, a Certificate object, and frequently a namespace. On cleat it
is a row and a certificate. **Tenant ten thousand costs an INSERT.** For a product whose shape is
"many tenants, each with a branded URL, each fairly low traffic", this is the dominant term and
cleat wins it outright.

**Marginal cost per byte served.** Cleat loses, and should concede it. Object storage behind a CDN
serves static bytes with no compute in the path at all. A cleat worker serving the same bytes burns
CPU, a connection budget and memory that were provisioned for workflow execution. **Using a durable
execution pool as a file server is using expensive compute to do cheap storage work.** The
mitigation is the standard one — a CDN in front, workers as origin — which converts the claim from
"cleat serves your front-end" into "cleat is your front-end's origin". That is much less exciting
and much more honest, and it is the right claim.

**Marginal cost per API request.** Cleat wins on the command side and loses on the read side, for
the reasons in Tier B and Tier C. A durable command is one request that would otherwise have been
a function invocation *plus* a workflow-engine invocation *plus* the egress between them. A durable
read is several database writes that a cached `GET` would not have made at all.

The figures to go and measure, rather than assume: bytes-per-deploy for a representative bundle;
worker CPU-seconds per thousand static requests at the intended cache hit rate; database rows and
bytes per durable command; and the per-tenant certificate issuance and renewal cost in both money
and operational attention.

### Operational simplicity

**Cleat's strongest non-cost argument, and it is a control-plane argument rather than a serving
argument.**

On a conventional split, the front-end deploys through one system (V), the API through another (C
or K), and the durable workflows through a third (a workflow engine). Three deploy pipelines, three
rollback procedures, three audit trails, three sets of credentials — and no way to roll the three
back *together*. A front-end release that assumes an API shape the API has not shipped yet is a
coordination problem those stacks structurally cannot solve; they solve it with process and
feature flags.

Cleat can make front-end version, API version and workflow version **one versioned artifact set,
promoted by one routing rule, rolled back by one pointer flip**, with the rollback being a database
write rather than an image pull and a rolling restart. The machinery for the promotion and the
rollback already exists for workflow versions. That is a genuinely hard thing to buy and it follows
directly from the design rather than being bolted on.

Against that: cleat adds an operational surface nobody else makes you run. ACME and certificate
renewal is the clearest example — V and the managed parts of C give it away free, K has
cert-manager, and cleat would be building and carrying it.

Ranking, best to worst on day-2 burden: **M, then V, then cleat-with-Tier-A, then C, then K.**
Cleat's placement is contingent on *not* building Tier C; a synchronous untrusted-code tier moves
it down the list.

### Observability

This one splits cleanly and the split is usually stated wrong.

**Backend-of-front-end: cleat wins, structurally.** Because durability requires recording every
external interaction for replay, the same record is already a trace. A request that starts a
durable command has an end-to-end causal history from the HTTP edge through every step, in one
store, with one identifier. On the conventional split you are correlating edge analytics, an APM
vendor and a workflow UI across three identifier schemes, and the join is usually approximate.
`docs/contributor/design/cleat-execution-design.md` makes this argument for workflows; it extends
to the request that triggered them at no extra cost.

**Front-end proper: cleat has nothing and will not.** Real user monitoring, Core Web Vitals,
browser error tracking, session replay — V and the CDN vendors give these away, and cleat's event
history knows nothing about a browser. You would still buy a front-end observability product.

So the honest claim is narrow and still worth making: **one trace from the edge to the durable
step**, not "unified observability". Overstating this is the fastest way to lose credibility with
the people who would adopt it.

Worth noting what already exists on the cleat side: `/metrics`, a wasm-cache hit/miss and occupancy
gauge family (`monitoring/prometheus/metrics.go:324-441`), the memory-controller state exposed via
healthz, and the recently added connection-budget census
(`cmd/cleat-worker/connection_budget.go:70-95`). The instinct to make resource consumption
enumerable is already present in the codebase, which is exactly the instinct a serving tier needs.

### Deploy velocity and rollback

Cleat wins narrowly against everything except V.

V is very hard to beat on developer experience — `git push` builds and deploys a preview. Cleat's
deploy is faster in wall-clock terms (a database write and a pointer flip, versus an image build
and a rolling restart) but the surrounding experience does not exist: no preview URLs, no build
pipeline, no framework integration.

K is the clear loser: image build, registry push, rollout, readiness gates, and a rollback that is
another rollout.

### Multi-tenancy with per-tenant URLs

**Cleat's best category, and the one that should drive the decision.**

Every alternative treats "a tenant" as an infrastructure object. V charges per domain and per
project. K gives you an Ingress, a Certificate and often a namespace per tenant, and a controller
reconciling thousands of them. C makes you build the mapping yourself on top of a CDN's
configuration API, with that API's own rate limits.

Cleat already models a tenant as a row with RLS, optional per-tenant database roles and pools, and
a versioned artifact set scoped to it. Extending that to "and a hostname, and a bundle, and a
certificate" is additive rather than architectural. **No alternative in the table gets per-tenant
isolation this cheaply, because none of them has tenancy in the data model to begin with.**

### Latency

Cleat loses and cannot win. Workers live where the database lives. Edge platforms serve from
hundreds of points of presence. A user far from your region pays the physics, and no amount of
engineering inside cleat changes it. Put a CDN in front for assets; accept regional latency for the
API, which is where every alternative except V and Cloudflare also lands.

### Security surface

Mixed, and the direction depends entirely on whether tenants supply code.

If tenants supply code, cleat's WASM plus a stated WASI allowlist is a **better** isolation story
than a container per tenant, on both density and cold start, and `engine/wasi_policy.go` is already
more rigorous than most container hardening in the field. If tenants do not supply code, cleat is
taking on browser-facing attack surface — sessions, CSRF, CORS, XSS, certificate handling — that a
managed platform absorbs for you, and it is starting from zero on all of it.

### Ecosystem fit

**Cleat's worst category by a distance, and the place not to fight.**

Nobody's front-end is a WASM module. Next.js, React server components, Vite, the bundler and npm
world all assume Node. Serving a compiled static bundle is fine and covers a large fraction of real
applications. Server-side rendering is not: it would mean either shipping a JavaScript runtime
inside WASM — slow, enormous — or not offering SSR.

V's entire value proposition is that `git push` runs your framework. **Cleat cannot match that and
should not claim to.** The correct scope is: cleat serves the application shell and the API. The
front-end framework experience stays wherever the team already has it.

### Portability

Cleat wins cleanly. Postgres plus a binary, no provider APIs in the critical path. That is a real
argument for on-premises, air-gapped and sovereign-cloud deployments, and it is one V structurally
cannot make.

### Summary

| Dimension | Cleat vs. the field |
|---|---|
| Fixed cost floor | **Wins** (marginal on hardware already bought) |
| Cost per tenant | **Wins decisively** — a tenant is a row, not an infrastructure object |
| Cost per static byte | **Loses** — expensive compute doing cheap storage work; put a CDN in front |
| Cost per command request | **Wins** — one durable call replaces a function plus an engine plus the egress |
| Cost per read request | **Loses** unless reads bypass the durable path entirely |
| Operational simplicity | **Wins on the control plane** (one deploy, one rollback, one audit trail); **loses** on TLS/ACME |
| Observability, backend | **Wins structurally** — durability and tracing are the same record |
| Observability, front-end | **Loses** — no RUM, no Web Vitals, no session replay, and no path to them |
| Deploy and rollback | **Wins on mechanics**, loses on developer experience |
| Multi-tenant per-tenant URLs | **Wins decisively** — the only option with tenancy in the data model |
| Latency | **Loses** — no edge presence, and physics is not negotiable |
| Security, tenant-supplied code | **Wins** — WASM plus a stated WASI allowlist beats a container each |
| Security, browser-facing | **Loses today** — no session, CSRF, CORS or certificate story exists |
| Ecosystem | **Loses badly** — do not fight here |
| Portability | **Wins** — Postgres and a binary |

**The shape of it: cleat wins the control plane, loses the edge, and should not fight on the
ecosystem.** A product framed as "one control plane for tenant-scoped application delivery, with
your front-end framework unchanged and a CDN in front" is defensible on every row it wins and
concedes every row it loses. A product framed as "a better Vercel" loses on ecosystem, latency and
developer experience simultaneously.

---

## Security

**Separate the listeners.** Today `--api-addr` serves the SPA, `/api/admin/*`, `/metrics` and
`/healthz` through one mux and one middleware chain (`cmd/cleat-worker/app.go:29-70`). Public
front-end traffic on that listener puts the internet one authentication bug away from
`POST /api/admin/instances/{id}/force-complete`. The precedent against path-based separation is in
that same file's doc comment. Use separate `http.Server` instances, separate ports, separate
interfaces, separate middleware chains.

**The `Host`-to-tenant trust chain** is covered above and is the single most important item.

**`fd_write` is allowed and goes to the host's stderr under `InheritStderr`.** The policy table
calls it "THE WEAKEST ENTRY IN THIS TABLE" and allows it deliberately, because refusing it removes
a guest's panic output. That trade is right for workflow debugging and wrong for a public request
path: a tenant-controlled per-request handler can write unbounded data to the worker's stderr,
which is a log-volume denial of service and a log-injection path into whatever ingests worker logs.
Routing it through the host logger is already noted in that file as the open follow-up.

**The browser security model is absent.** No session layer, no CSRF tokens, no CORS handling, no
Content-Security-Policy. Serving a browser application means owning all of it.

---

## Denial of service

**The rate limiters are per-worker, not per-fleet.** `keyedRateLimiter` and `ipRateLimiter` are
maps held in process memory (`cmd/cleat-worker/server.go:2511`, `:2586`). With N workers a tenant
receives N times the configured rate and there is no cluster-wide ceiling. This is the same problem
that #1556 ("workers are enumerable, so a cluster-global budget has something to count") is already
chasing for connections, and serving traffic makes it much more acute — a noisy tenant's front-end
is precisely the case a fleet-wide limit exists for.

**But a cluster-wide limiter already exists in the tree, and a serving tier should use it.** The
paragraph above is about the worker's *built-in* limiters and stands as written for those.
Separately, `plugins/ratelimiter/` has a `db` mode that coordinates through a `rate_counter` table:
`checkDBRateLimit` (`plugins/ratelimiter/middleware.go:211-268`) keeps per-second buckets and sums
them over a sliding window, portable across all three dialects.

Three things qualify it rather than withdraw it. It is **opt-in** — `mode` defaults to `"memory"`
(`plugins/ratelimiter/plugin.go:78`). It **falls back to memory without erroring** when no database
is available (`:91-97`), so a misconfiguration silently degrades to per-process limiting. And it
**fails open** on a database error (`middleware.go:186`), which is the right default for
availability and the wrong one if the limiter is a spend control.

So the accurate statement is not "cleat has no fleet-wide rate limiting" — it has one, off by
default, in a plugin. An earlier draft of this document said otherwise, and the playbooks repeated
it; corrected 2026-09-14.

**The dimension that matters is off by default.** `--rate-limit-per-tenant` defaults to 0, meaning
disabled (`cmd/cleat-worker/config.go:187`). The enabled default is per-IP at 100/s (`:185`).

**`clientIP` trusts `X-Forwarded-For` unconditionally** (`cmd/cleat-worker/server.go:2570-2580`),
taking the first element with no trusted-proxy list. Behind a controlled load balancer that is
fine. Facing the internet it means any client can forge a header and obtain a private bucket,
which defeats the one limiter that is on by default.

**The 50 ms epoch granularity is a floor on enforceable timeouts.** The ticker is process-global
and fires every 50 ms (`engine/backend_wasmtime.go:169-190`), so a per-request handler cannot be
given a 5 ms budget. Fuel metering, which would give instruction-level bounds, is engine-level
opt-in (`:149-151`) and therefore costs throughput on workflow execution too if enabled for the
benefit of request handlers.

**Memory is the binding constraint on request concurrency.** At a 32 MB default ceiling per store,
200 concurrent requests is 6.4 GB of headroom that must actually exist. Request concurrency needs
admission control against a declared memory budget, in the same spirit as the connection budget.

---

## Resource contention

**Shared fate is the strongest argument against one process doing both.** The `MemoryController`
(`cmd/cleat-worker/memory_controller.go`) reduces workflow concurrency under operating-system
memory pressure. A traffic spike on a tenant's front-end therefore *degrades the durable back-end*
— a public, unauthenticated input path that throttles the paid work. That coupling, more than any
individual vulnerability, is why serving belongs in a separate process.

**Connections.** `connectionBudget` (`cmd/cleat-worker/connection_budget.go`) exists because
nothing summed the worker's six pools, and the documented figure understated a default worker by a
factor of five. A serving role adds a seventh consumer that nothing sums. Whatever is built must
declare its ceiling into that struct so `checkConnectionBudget` refuses an impossible configuration
at boot rather than exhausting the database under load. `serverConnectionLimit`
(`cmd/cleat-worker/server_connection_limit.go`) is the other half and applies unchanged.

**Scheduling mismatch.** Workflow work is claimed in batches on a poll and is throughput-oriented.
HTTP arrives whenever and is latency-oriented. One `--concurrency` governing both means a batch of
long-running workflow steps head-of-line blocks request handling. Separate goroutine pools and
separate limits, not one number.

**The compiled-module cache has no eviction.** `engine/wasm_cache.go` implements LRU for the bytes
cache and `engine/plugin_loader.go` implements LRU for plugin modules, but `moduleCache` in
`engine/backend_wasmtime.go` is a bare `sync.Map` reached only by `Load` (`:697`, `:708`) and
`Store` (`:723`) — no `Delete`, no `Range`, no bound. Re-derive with:

    grep -n 'moduleCache' engine/backend_wasmtime.go

Today that is bounded by the number of distinct workflow versions a worker executes. Under
per-tenant front-end deploys it becomes unbounded compiled code resident in memory, growing with
deploy frequency times tenant count. **This must be bounded before any serving tier ships**, and
the two LRU implementations already in the tree are the model.

---

## How a front-end change gets deployed

Today: rebuild the binary, redeploy every worker, restart. That is the honest answer and it is the
wrong one.

Proposed: reuse the definition pipeline. Upload the bundle as a versioned artifact; workers fetch
and cache by digest; routing rules perform the canary; rollback is a pointer flip. Two properties
have to hold.

**The bundle flips atomically.** Content-hashed asset filenames, immutable `Cache-Control` on the
hashed assets, a short TTL on `index.html`, and the manifest pointer as the single switch.
Otherwise a browser receives `index.html` from version 8 and requests a chunk that only version 7
had.

**Asset retention must outlive the version pointer.** During a rolling deploy, workers hold
different versions and browsers hold older manifests. Deleting version 7's blobs the moment version
8 becomes current breaks live sessions. This is the same class of hazard as a retention sweep
removing something still referenced, and this repo has met it before.

**The database is not the right long-term home for large bundles.** The natural split is: the
database holds the manifest and the digests, object storage holds anything large, and the worker
caches locally. That also removes the availability coupling in which a marketing page depends on
the durable engine's database being up — which is, on its own, a worse availability story than
object storage, the single most reliable component in the stack.

---

## Recommendation

**Superseded in direction, retained for the analysis.** The recommendation below is to build a
separate serving role. The direction taken instead was to write playbooks that assemble cleat with
conventional pieces rather than to expand cleat's scope — see
[`docs/playbooks/`](playbooks/README.md), which is built on this document's comparison. The
reasoning here is what justifies that choice, so it is kept rather than rewritten.



**Build it as a separate role that shares the libraries, not as a mode of `cleat-worker`.**

The same repository, the same artifact pipeline, the same sandbox, the same module-cache code — a
different binary, a different listener, different database credentials, its own concurrency and
connection budget, fuel metering enabled, and a deliberately smaller host ABI.

Then "a pool of cleat workers serves both the front-end and the durable workflows" is true at the
fleet level and false at the process level. That is the correct place for it to be true: the
operational and control-plane wins survive, and a public traffic spike can no longer reach in and
throttle the durable engine through the memory controller.

Suggested sequence:

1. **Tier A, static assets.** Replaces `go:embed`, exercises the artifact pipeline for a
   non-workflow artifact, and delivers the per-tenant-URL win. Requires bounding the module cache
   and splitting the listeners first.
2. **Tier B, document and package** what already works. Mostly writing.
3. **Per-user authorisation below the tenant.** Sessions and a user principal already exist via
   `oauthprovider`; what does not is any database-level enforcement of them. Decide whether RLS
   learns about the user, or whether within-tenant authorisation stays application-layer and is
   documented as such.
4. **Tier C** only as a separate decision with its own design document.

---

## Open questions

1. Do the public-pattern handlers (`POST /ingest/{source_id}`, `GET /oauth/{provider}/callback`)
   consult the context tenant, which a client-supplied header can set on those routes? Not traced.
2. A principal below the tenant exists at the HTTP layer (`oauthprovider`'s `SessionInfo.UserEmail`),
   but RLS is scoped by tenant alone. Should RLS learn about the user, or does within-tenant
   authorisation stay application-layer and get documented as such?
3. Does the asset store live in the database, in object storage, or split by size — and what is the
   availability coupling each choice creates?
4. Is a per-tenant certificate subsystem in scope, or are tenants confined to subdomains of one
   wildcard-covered parent, with the cookie-scope consequences that implies?
5. `ratelimiter`'s `db` mode already provides a cluster-wide per-tenant limit. Should the worker's
   built-in limiters be retired in favour of it, or should they gain the same DB coordination? Two
   mechanisms for one job, with the weaker one on by default, is the state to resolve.

---

## What was verified and what was not

**Read directly from the tree at `654d6f84`**, and all file:line references above were opened
rather than recalled: the route table and SPA mount; the definition upload handler; the module
cache's `Load`/`Store` sites and the absence of any `Delete` or `Range`; the WASI policy table;
the wasmtime epoch, fuel and memory-limit sites; the auth middleware including its public-path
early return; the tenant-resolver middleware, its installation order relative to auth, and the
identity of the context key both of them write; `tenantFor` and `scopedStore`; both rate limiters and `clientIP`; the connection budget and server connection
limit; the memory controller; and the poll-interval and rate-limit flag defaults.

**Corrected after review, and both errors came from the same habit.** Two claims in earlier drafts
were wrong, both because a component was judged from its doc comments and neighbours rather than
from the code path that answers the question:

- *"Cleat has no fleet-wide rate limiting."* `plugins/ratelimiter/` has a `db` mode that coordinates
  through a `rate_counter` table. I read its package comment ("rebuilds the in-memory token bucket
  cache"), read the worker's own in-process limiters, and never opened `allowDB` — which is reached
  by a branch three lines into `Middleware`. A doc comment describing one of two code paths is not a
  description of the component.
- *"Browser authentication does not exist here."* `plugins/oauthprovider/` is an OIDC relying party
  with sessions and a user principal. Corrected in place under "One URL per tenant".

Both were caught by a reader who knew the system, not by anything in the method used to write this
document. That is the honest status of every remaining unverified claim here.

**Reasoned about, not measured.** Every latency and cost statement. The Tier C write-amplification
argument follows from reading the write path, not from running it. The competitive analysis is an
argument about cost structure and contains no measured figures by design.

**Explicitly not traced.** Whether the public-pattern handlers consume the context tenant, which is
open question 1 and the difference between a lead and a finding. No live request was driven against
any of this; no test was run.

**No negative control was constructed for any claim here.** In the terms this repository uses, the
document is a set of claims rather than a set of checks. Anything acted upon should first be given
a case that could have come out the other way.
