# Playbook 2 — Multi-tenant B2B SaaS control plane

**Status:** engineering reference. Drafted 2026-09-14 against `develop` at `654d6f84`; corrected
2026-09-25 against `develop` at `656aced4` (cleat#2052) — see
[What was verified](#what-was-verified) at the end for what changed. Nothing here has been built end
to end.

**Who this is for:** you sell software to businesses. Each customer gets their own login, their own
data, their own settings, often their own branded URL, and an auditor will eventually ask you to
prove that customer A never saw customer B's data. You are otherwise about to buy an identity
vendor, a feature-flag vendor, an audit-log vendor, a workflow engine, and a job runner.

This is the playbook where the win is a **subtraction**.

---

## The conventional bill of materials

A B2B SaaS control plane is a remarkably consistent shopping list:

| Concern | What you would buy or run |
|---|---|
| Login, SSO, SCIM | Auth0, WorkOS, Okta |
| Feature flags and gradual rollout | LaunchDarkly, Flagsmith |
| Audit log for compliance | an audit vendor, or a homegrown table |
| Background jobs and schedules | Sidekiq, Celery, a cron box |
| Long-running business processes | Temporal, or homegrown state machines |
| Per-tenant rate limiting | an API gateway, or homegrown |
| Tenant isolation | your own `WHERE tenant_id = ?` discipline |

Each is a vendor relationship, an SDK, a failure mode, a bill, and a place where tenancy has to be
re-modelled from scratch — because none of them knows what a tenant is in your system.

**The cleat version is one binary and one Postgres**, and the reason is not that cleat reimplements
those products well. It is that **cleat already had to model a tenant to do its own job**, so
everything hung off that model inherits tenancy for free.

---

## What ties to the cleat, and what stays rope

**Ties to the cleat:**

- Tenant identity and isolation, enforced in the database rather than in your code
- User login and sessions
- Every business process that outlives a request
- Feature flags, evaluated *and recorded*
- The audit trail
- Per-tenant limits and settings
- Tenant provisioning and — importantly — tenant deletion

**Stays rope:**

- Your front-end, your component library, your CDN
- Your identity provider. `oauthprovider` is a relying party, not an IdP.
- Your billing system
- Your marketing site

---

## The assembly

| Concern | Component | Hitch point |
|---|---|---|
| Login and sessions | `oauthprovider` — `GET /oauth/{provider}/login`, `/callback`, session list and revoke; **generic OIDC since cleat#1582** (`providerOIDC`, `routes.go`) | edge middleware + routes |
| Per-tenant request limits | `ratelimiter` | edge middleware |
| Per-tenant volume quotas | `tenantquota` — counter-based, with `cleatctl quota` to set a plan without SQL. **`enforce` defaults to false**, deliberately: over the limit is recorded and reported, not refused | edge middleware |
| Audit trail | `auditlog`, plus `cleatctl audit export` for the streamed export | edge middleware + routes |
| Flags and rollout | `featureflags`: `evaluate_flag` | host function + routes |
| Business processes | your workflows — or your **tenants' own**, via `POST /api/definitions`. See *Tenant-supplied steps* below | — |
| Schedules | `scheduler` | routes + background loop |
| Settings and metadata | `kvstore` (versioned JSONB, optimistic concurrency) | routes |
| Per-tenant secrets | the tenant secret store (migration 081) — read by the host and **never handed to the guest**; `cleatctl set-secret`, rotation via `reseal-secrets` | host + admin |
| Hostname to tenant | `auth/host_binding.go` (cleat#1568) | edge middleware |
| Grouping a customer's tenants | orgs (cleat#1898, `091_an_org_groups_a_customers_tenants.sql`) | schema |
| Files and attachments | `blobstore` (S3-backed) | host functions |
| Notifications | `email`, `slacknotify`, `notifications` | host functions |

Every row from *Per-tenant volume quotas* down to *Grouping a customer's tenants* was shipped and
unmentioned when this table was written; the page listed them under "what you still have to build"
or did not list them at all.

---

## Isolation is the product feature, and it is enforced below your code

This is the part that is genuinely hard to buy, and the part an auditor cares about.

In the conventional stack, tenant isolation is a **convention**: every query carries
`WHERE tenant_id = ?`, and the guarantee is that nobody ever forgets. Cleat pushes it into
PostgreSQL row-level security, and the tree contains a fair amount of evidence that the difference
is real rather than theatrical — `plugins/featureflags/` carries a test named
`TestFeatureFlagsRowsAreScopedByAPolicyNotOnlyByTheQuery`, whose comment makes the argument
directly: every statement in that plugin *already* carries `WHERE tenant_id = $1`, and "that is the
reason this test exists rather than a reason it does not."

Three isolation postures are available, in increasing strength:

1. **RLS with a session tenant** — the default.
2. **A database role and connection pool per tenant** — `--tenant-isolation=role`, PostgreSQL only,
   with `--tenant-pool-max-conns` bounding each pool. The connection itself authenticates as the
   tenant, so a bug in cleat cannot reach across tenants either.
3. **Schema or database separation** — see `docs/schema-partitioning-design.md` and
   `docs/sharding.md`.

**All three are PostgreSQL, and this section read as though they were not.** Until 2026-09-25 it said
isolation is "enforced below your code" without qualification, which is true of PostgreSQL and false
of MySQL. **MySQL has no row-level security at all.** There, isolation is a **static predicate guard
over the SQL source**: every statement is required to carry its tenant predicate, enforced by a
source scan (`engine/mysql_tenant_predicate_test.go`) rather than by the database — and MySQL is
single-tenant by owner decision for exactly that reason.

That distinction is worth an auditor's attention rather than a footnote, because it changes what the
guarantee *is*: on PostgreSQL the database refuses a query that does not name a tenant, and on MySQL
a test refuses a *source file* that does not — so a statement reaching MySQL by a path the scan does
not read is not caught by anything. If per-tenant isolation with the database enforcing it is a
requirement you have to demonstrate, that is PostgreSQL, and it is the only one of the three
postures above that MySQL supports.

**The posture is checkable, and there is a command for it.** `cleatctl` includes an RLS-posture
check (`cmd/cleatctl/rlsposture.go`) distinguishing three states: `rlsExempt` (superuser or
`BYPASSRLS`), `rlsSubject` (policies apply), and `rlsUnprotected` — "no policy applies, but because
the DATABASE is not enforcing rather than because the role is privileged — policies missing,
row-level security switched off, or a table owner without FORCE." That third state is the dangerous
one, because reads succeed and nothing looks wrong.

**Read this asymmetry before you hand anyone a credential**, because it is genuinely
counter-intuitive and is spelled out in that file: since cleat#1204 **`cleat-worker` refuses to
start on a connection that bypasses RLS, by default**, while `cleatctl` *needs* such a connection
to answer cluster-wide questions. So an operator who follows the worker's error message and creates
an `cleat_app` role has thereby created the credential that makes `cleatctl` answer with only one
tenant's data. Two binaries, opposite requirements, one database.

---

## Tenant lifecycle, including the part everyone forgets

Provisioning a tenant is a row. **Deleting one is a first-class command**, which is rarer than it
should be and directly answers a GDPR erasure request.

**Between "nothing" and "gone" there is now `cleatctl suspend-tenant`.** A non-paying customer, a
tenant under investigation for abuse, and an offboarding grace period all want the same thing: the
work stopped and the data kept. Deleting is the wrong instrument for all three, and until this
existed it was the only one.

What suspension stops, and what it deliberately does not:

| | suspended |
|---|---|
| new workflows claimed | **no** |
| cron schedules fired | **no** |
| `POST /start` | **403**, naming the reason |
| runs already executing | **finish normally**, heartbeating as usual |
| reads — runs, history, state | **unaffected** |

The last two rows are the design. A run mid-flight is not frozen, so nothing is left half-done for
the reclaim loop to find and `cleat_workflows_stuck` never sees a population that is not actually
stuck — which is why suspension needs no special handling anywhere else in the engine. To stop work
already running, cancel it: suspension and cancellation are different instruments, and conflating
them would make the reversible one destructive.

Reads staying open is a product decision rather than an implementation limit. A tenant suspended for
non-payment can still see its own runs; blocking that punishes the wrong thing.

Enforcement is one predicate in the tenant enumeration, which both the dispatch claim and the
due-schedule read go through — so a single line stops work and cron together. That choke point is a
side effect of the per-tenant claim: before it, the claim was one widened query inside a
`SECURITY DEFINER` function and there was nowhere central to put this.

`admin.tenants.suspended` had been in the schema since migration 001 with **no Go code reading it**.
A column named `suspended` that does nothing is worse than an absent one — the first operator to
reach for it in an incident sets it, sees nothing happen, and has spent the minutes that mattered
finding that out.

`cleatctl drop-tenant` removes a tenant's rows across the full set of tenant-scoped tables and
reports counts per table, with a dry-run mode (`cmd/cleatctl/droptenant.go`). The list it reports
against is maintained deliberately, and the comment on it is worth reading as a lesson in how this
goes wrong:

> "Removes", not "admin.drop_tenant deletes", because two of these go by foreign key rather than by
> a DELETE inside the function, and the operator does not care which mechanism took their data.

Two tables — `tenant_settings` and `workflow_defs` — cascade off `admin.tenants` rather than being
deleted explicitly, and the note records that reading only the function's `DELETE` statements to
maintain the list "is how `tenant_settings` came to be missing from it for twenty migrations."

Re-derive the current list rather than trusting any copy of it:

    grep -n 'label string' -A25 cmd/cleatctl/droptenant.go

**What is not covered:** blobs in S3 via `blobstore`, and anything your own application wrote to
its own tables. Tenant deletion is complete with respect to cleat's tables and no further.

**`drop-tenant` also clears the tenant's domains and secrets** (`tenant_domains`,
`tenant_secrets`), which matters for erasure: a hostname mapping left behind is a record that the
customer existed, and a secret left behind is worse. Re-derive the current table list rather than
trusting any copy of it — the command above.

---

## Tenant-supplied steps, which is the actual wedge

Everything above has **you** writing the workflows. The stronger claim — the one that makes this a
platform rather than a product with an engine in it — is that a tenant supplies its own logic, and
that shipped: **`POST /api/definitions` accepts a tenant's own WASM workflow.** It is created through
`scopedStore`, so a tenant cannot write into another tenant's namespace, and it is auto-versioned
when no version is given.

**A worked case.** A customer wants its own approval rule — "orders over $10k need a second
approver" — and every customer's rule differs. You are not going to ship a rule builder:

1. The tenant uploads a `needs-second-approval` definition of its own via `POST /api/definitions`.
   Scoped to that tenant, invisible to every other.
2. Your `PlaceOrder` workflow invokes it as a child workflow, handing it the order. **Yours** owns the
   charge, the compensation, the audit trail and the idempotency; **theirs** owns the rule.
3. The customer revises its rule next quarter. That is a new version of *their* definition, routed by
   the same version pinning and routing rules you already use — not a change to your product, and not
   a deploy.

**WASM is what makes this safe to offer.** The tenant's step runs in the same sandbox as your own
workflows: no ambient filesystem, no network beyond the host functions you allow, and its durable
calls go through the same recorder — so it lands in the same history and the same audit trail, which
is the property an auditor asks about. You are not handing a customer a scripting hook inside your
own process; the isolation containing it is the one that already contains your code.

**Before you expose it: serving a tenant's workflow on a public route needs exposure classes**
(cleat#1986) — each definition declaring `public`, `auth` (the default) or `internal`, enforced on
every workflow route. Until that lands, keep tenant-supplied definitions behind authenticated routes.

---

## Cost

**Where this wins, structurally.**

*Marginal cost per tenant is a row.* This is the single strongest claim in the whole playbook
series. On the conventional stack, a tenant is priced by an identity vendor per monthly active
user, by a flag vendor per seat or per environment, and by your ingress per certificate. On cleat a
tenant is a row in `admin.tenants`, optionally a database role, and a certificate if you give them
their own hostname. **The thousandth tenant costs what the tenth did.**

*Five vendor bills become zero.* Identity, flags, audit, job runner and workflow engine are all
inside the binary you already run.

*No per-seat pricing anywhere.* The conventional stack's costs scale with *your customers' users*,
which is the axis you least want to be billed on, because it grows exactly when you are succeeding
and it is not the axis your own revenue necessarily follows.

**Where this loses.**

*You are now running an identity system.* `oauthprovider` is an OIDC relying party with sessions —
it is not SCIM provisioning, not directory sync, not a consent screen, not device management, and
not a compliance certification you can show a customer. Enterprise buyers ask for SAML and SCIM by
name.

**SAML has since been decided and the answer is not "build it"** — see
[`docs/enterprise-identity-decision.md`](../enterprise-identity-decision.md). A SAML proxy *is* an
OIDC provider, so a customer terminates SAML themselves and presents it to cleat as OIDC; cleat
never handles an XML signature. **The generic OIDC issuer that needs shipped** — this paragraph said
`oauthprovider` "does not yet have" one and that three providers were hardcoded, which was true when
drafted and stopped being true at cleat#1582; the provider is now `"oidc"` (`plugins/oauthprovider/routes.go`,
`providerOIDC`), so a customer's SAML proxy can be pointed at cleat without a new code change per
vendor. **SCIM remains genuinely absent** and is deferred rather than declined.

*Feature flags without the product around them.* `featureflags` evaluates rules with targeting and
percentage rollout. It has no experimentation platform, no metrics-linked rollout, no approval
workflow around flag changes.

*Your audit log is only as good as what you route through the middleware.* An audit vendor's value
is partly the schema and the retention guarantees and the export tooling; here you get a table.

*Infrastructure cost estimate:* `cleatctl cost` (`cmd/cleatctl/cost.go`) estimates monthly spend
from workflows/sec, average duration, events per workflow, retention and provider. Read the note it
prints about the retention flag being an assumption you supply rather than engine behaviour
(cleat#1295).

**Measure rather than assume:** connections per tenant under `--tenant-isolation=role` at your
tenant count, against the `connectionBudget` census — this is the term that decides how many
tenants a worker can serve, and `TenantHeadroom` deliberately reports the pessimistic figure.

---

## Operations

**One deploy pipeline.** The application's workflows, its flags and its schedules are all
versioned rows promoted by routing rules. A release is a pointer flip, per tenant if you want it
that way — which is exactly the staged-rollout capability people buy a flag vendor for, except it
covers your business logic rather than only your UI.

**One backup and one restore.** Tenant data, workflow state, audit log, flags, sessions and
settings are in one database. The conventional stack's equivalent is a restore that has to
reconcile five systems' notions of the same moment in time, which in practice nobody tests.

**The operational burden you take on** is the one from the design doc: per-tenant hostnames mean
TLS, ACME and renewal, and that is a subsystem. If tenants live on subdomains of one
wildcard-covered parent you avoid it — at the price of the cookie-scope hazard, since a cookie set
on the shared parent is readable by every tenant subdomain.

---

## Observability

You get, without instrumentation: every business process's full history, per tenant; every flag
evaluation that a workflow made, recorded in that workflow's history, so "why did this customer get
that behaviour" is answerable retrospectively; and an audit middleware on the HTTP edge.

`datadogexport` exists as a plugin if Datadog is where your dashboards live.

**The gap:** nothing here is front-end observability, and the audit middleware records HTTP-level
activity rather than semantic business events. "Who changed this customer's billing plan" is a
thing you have to route through a workflow for the history to answer it.

---

## What you still have to build or buy

**Two items this list used to carry have shipped, and the issue that asked for them to be added here
was overtaken by the same thing** — the quota admin surface (cleat#2046, now `cleatctl quota`) and
the audit export (cleat#2047, now `cleatctl audit export`) are in the assembly table above, not here.
Re-check a "still to build" list against the tree before acting on it; this one drifted in the
direction that costs you work you did not have to do.

1. **A terminator, if a customer needs SAML** — but **not** the issuer support. Generic OIDC shipped
   in cleat#1582 (`providerOIDC`), so a customer's SAML proxy terminates SAML itself and presents
   OIDC to cleat, with no per-vendor code. cleat does not implement SAML by decision, not by omission
   — [`docs/enterprise-identity-decision.md`](../enterprise-identity-decision.md). **SCIM remains
   genuinely absent**, deferred rather than declined, and is still yours to build or buy.
2. **Per-tenant TLS**, if tenants get their own domains. The host-to-tenant *binding* is shipped
   (`auth/host_binding.go`, cleat#1568); this is the certificate and its renewal, not the mapping.
3. **A user-level authorization model.** `SessionInfo` carries `TenantID`, `SessionID` and
   `UserEmail` (`SessionInfo`, `plugins/oauthprovider/middleware.go`), so a principal below the tenant
   exists at the HTTP layer — but **RLS is scoped by tenant and knows nothing about the user**.
   Role-based access within a tenant is application-layer, and the database will not catch your
   mistakes there the way it catches cross-tenant ones. This is the most important boundary in this
   playbook to be honest about with an auditor.
4. **Blob cleanup on tenant deletion**, and deletion of anything in your own tables.
5. **The front-end.** As designed.

---

## Failure modes to design against

**The `rlsUnprotected` posture.** Policies missing or `FORCE` not set — reads succeed, nothing
looks wrong, and isolation is not being enforced. Run the posture check in CI against a
production-shaped database, not just locally.

**The credential asymmetry above.** An operator holding the wrong credential gets plausible,
one-tenant answers from `cleatctl` and may act on them.

**A background loop with no tenant.** Plugin `Run` loops have no tenant and cannot get one; under
the fail-closed policy they **fail outright** rather than reading fewer rows, and must ask for
`plugin.AcrossAllTenants` by name (`plugins/kvstore/`, cleat#1278). That is the correct design and
it means any background sweep you write needs the exemption deliberately.

**Connection exhaustion at tenant count.** Per-tenant pools are not free. `checkConnectionBudget`
refuses an impossible configuration at boot; make sure `--connection-budget` is actually set, since
a zero budget means "not configured" and checks nothing.

---

## What was verified

**Read from the tree at `654d6f84`:** `oauthprovider`'s routes, `SessionInfo` shape and middleware;
the three-way plugin extension taxonomy; `featureflags`' policy-scoping test name and its comment;
`rlsposture`'s three states and the documented worker/cleatctl credential asymmetry; `drop-tenant`'s
table list and the cascade note; `cleatctl cost`'s parameters and its retention caveat;
`connectionBudget` and `checkConnectionBudget` semantics.

**Asserted, not measured:** every cost and operational claim, including the marginal-cost-per-tenant
argument, which follows from the data model rather than from a deployment.

**Since confirmed.** That `oauthprovider` lacks SAML was inferred here from the provider list in
`routes.go`; the map and its `validProviders` allowlist have since been read in full — and
`oauth_config` was already per-tenant. Recorded in
[`docs/enterprise-identity-decision.md`](../enterprise-identity-decision.md) and cleat#1582.

**Corrected since drafting (2026-09-25, cleat#2052).** Four corrections, each verified against the
tree rather than taken from the issue:

- **The generic OIDC issuer shipped** (cleat#1582). This page said `oauthprovider` "does not yet
  have" one and that three providers were hardcoded; the provider is now `"oidc"`.
- **The isolation claim needed a MySQL qualification it did not carry.** "Enforced below your code"
  is true of PostgreSQL row-level security and false of MySQL, where isolation is a static predicate
  guard over the SQL source and the database enforces nothing. See *Isolation is the product
  feature*.
- **Host binding (cleat#1568), orgs (cleat#1898), per-tenant secrets (cleat#1570, with rotation),
  `tenantquota` (cleat#1569) and the quota admin surface (cleat#2046) and audit export (cleat#2047)
  were all shipped and unmentioned or misfiled** as future work. They are in the assembly table now.
- **Tenant-supplied steps were the missing story.** `POST /api/definitions` lets a tenant upload its
  own WASM workflow; the page's "Business processes | your workflows" row read as vendor-authored
  only. See *Tenant-supplied steps*.

Line citations were replaced with symbol names throughout, per the precedent
`docs/reference/workflow-lifecycle.md` set — several had drifted.
