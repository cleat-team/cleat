# Tenants are separate, and groupings are for observation only

**Status: DECIDED 2026-09-15** by the owner.
**Drafted against `develop` at `7501a33c`.**

Records what cleat's tenant model is today, what it is anticipated to grow, and the one invariant
that must hold if it grows — written now because nothing in the tree says it, and features have
begun assuming it in different directions.

---

## The decision

**Each tenant is separate. There are no groupings or relationships between tenants, and
interactions between them go through HTTP APIs. For running workflows, that is enough.**

Tenants are separate *but sharing resources* — one worker pool, one database, one control plane.
Separation is a property of the data and the request path, not of the infrastructure.

**Higher-level groupings are anticipated for accounting and for tracing and debugging.** They are
not designed, not built, and not committed to. What follows is the boundary they must respect if
they are.

---

## Why HTTP between tenants is a feature, not a gap

This is the part most likely to be "fixed" by someone who reads it as a limitation.

An inter-tenant call crosses the same boundary an external caller would: it presents a credential,
it is authenticated, it is rate limited, it is audited. **There is no privileged path that exists
only because two tenants happen to be related.**

A relationship model that added one — a direct call, a shared handle, a bypass — would trade the
isolation that currently works for convenience. The HTTP hop is the thing making the boundary real
rather than nominal, and it costs a network round trip, which is the correct price for a trust
boundary.

---

## The two planes

The distinction that makes the rest of this decidable.

| | Plane | Needs groupings? |
|---|---|---|
| Workflow execution, data access, RLS scoping, secrets, inter-tenant calls, hostname binding | **Execution** | **No — and groupings would be harmful here** |
| Tracing a chain across services, investigation, audit rollup, accounting and billing | **Observation** | **Yes, eventually** |

On the execution plane the separation is doing the work. On the observation plane the separation is
what is in the way: a bill and a trace are both about a system, and a system may be several tenants.

---

## The invariant

> **A grouping grants VISIBILITY. It never grants ACCESS.**

If membership of a group ever lets one tenant read another's rows, call it without HTTP, or share a
secret, the isolation has been undone to buy a reporting feature. Everything a grouping is wanted
for — a trace that spans services, a bill that covers them, an audit that reads across — is
satisfied by visibility alone.

Two supporting rules:

**A grouping is an operator assertion, never a tenant's.** A tenant that could declare itself into a
group would thereby gain visibility of the others, so the group would BE the attack. Membership is
configured by whoever runs the deployment.

**Visibility is not symmetric with the group's purpose.** Accounting wants aggregate figures;
debugging wants individual payloads. A design that grants one because the other was asked for is
too coarse — a finance rollup should not carry the ability to read another tenant's request bodies.

---

## What this means for what is already built

Recorded so the reasoning is not re-derived, and because one entry corrects a claim made while
building it.

**Secrets (cleat#1570) stay per-tenant, and the friction is correct.** It was suggested during
review that several tenants representing one operator's microservices would want a shared
credential, and that copying one key eight times is friction worth removing. Under this decision it
is not: it is exactly the friction two genuinely separate services would have. Sharing a secret is
an execution-plane capability, so a grouping must not confer it.

**Quotas (cleat#1569) are the case that wants a grouping**, and were filed without one. A customer
running eight service-tenants wants one budget across them, not eight. That is an accounting
rollup, which is squarely the observation plane — so the eventual shape is a per-tenant bound plus a
group-level total, not a relationship in the execution path.

**Tracing (cleat#1596, cleat#1597) and investigation (cleat#1575) are the motivating cases.** A
causal chain that crosses tenants is invisible today, and making it visible is precisely what a
grouping is for.

**Host binding (cleat#1568) is unaffected.** One hostname resolves to one tenant under either
reading.

**Enterprise identity (PR #1583) is unaffected.** A customer terminates SAML and presents OIDC per
tenant; nothing there assumes a relationship.

---

## The seam to watch

`plugin.AcrossAllTenants` is already the de facto relationship mechanism, and it is worth
understanding why that is the thing most likely to become load-bearing before anyone designs it.

It is a per-call-site assertion carrying a free-text reason — *"this sweep has no tenant, let it see
everything"*. So the statement "these tenants are one system" is being made one call site at a time,
by whoever writes the sweep, rather than once per deployment by whoever runs it.

**A grouping model would NARROW that, not loosen it.** Today the bypass is total: a sweep sees every
tenant in the deployment. Scoped to a group it becomes *"across these tenants"*, which is strictly
less. That inverts the usual reading of such a feature — adding groups would improve the isolation
posture rather than trade it away, and that is the strongest argument for eventually doing it
properly rather than leaving the bypass as the only mechanism.

---

## What this does NOT commit to

No schema, no column, no API. In particular **no `parent_tenant_id`**: a single hierarchy column
would serve one relationship kind and be quietly wrong for the others, and a column without agreed
semantics is worse than none, because three features will populate it meaning three different
things.

At least four distinct relationships are imaginable — same-owner grouping, hierarchy (reseller to
customer), a caller/callee authorization edge, and explicit revocable data sharing. They want
different mechanisms and different security properties. Nothing here picks between them; this
records only that the first is the one the anticipated uses need, and that whatever is built stays
on the observation plane.

**Nothing in `tiers.yaml` changes.** cleat does not claim tenant groupings today.

---

## What was verified

Read from the tree at `7501a33c`: `admin.tenants` carries `tenant_id`, `name`, `display_name`,
`created_at` and `suspended` and nothing else (`migrations/postgres/001_schema.sql`), and no
migration has ever altered it — so the flat model is the current model by construction rather than
by convention. `plugin.AcrossAllTenants` and its reason argument were read in
`plugin/crosstenant.go`, along with the tenant-scoping behaviour it bypasses in
`engine/plugindb_tenant.go`.

**Asserted, not measured:** that HTTP between tenants is adequate for execution at scale. It is the
owner's assessment of the current design, and no deployment has been measured against it.
