# Enterprise identity: cleat speaks OIDC, the customer terminates SAML

**Status: DECIDED 2026-09-14** by the owner. Supersedes the "SAML/SCIM — or an explicit decision
not to" item in [`docs/full-stack-readiness.md`](full-stack-readiness.md), which named it the
highest-value unfiled question.
**Drafted against `develop` at `654d6f84`.** Implementation tracked in #1582.

---

## The decision

**cleat does not implement SAML.** `oauthprovider` gains support for a **generic OIDC issuer**
configured per tenant. A customer who needs SAML terminates it themselves — with a self-hostable
proxy, a hosted service, or their own IdP — and presents the result to cleat as OIDC.

SCIM is a separate decision and is **deferred**, not declined.

---

## Why, in one paragraph

A SAML proxy *is* an OIDC provider: it terminates SAML on one side and speaks OIDC on the other.
So "support a self-hostable proxy" and "plug `oauthprovider` into the customer's preferred
identity system" are not two features. They are one mechanism — generic OIDC — with the terminator
chosen by the customer. cleat never handles an XML signature, which is the only genuinely dangerous
part of SAML.

| Customer wants | Configured in cleat as |
|---|---|
| Their own Okta / Entra / Google | an OIDC issuer |
| SAML, terminated by a self-hosted proxy | an OIDC issuer |
| SAML, terminated by a hosted service | an OIDC issuer |
| Self-hosted Keycloak / Authentik / Zitadel | an OIDC issuer |

---

## What was considered and rejected

**Implement SAML 2.0 directly.** Rejected. The flow is easy; **XML signature validation is not** —
canonicalisation, signature wrapping, XXE, and the comment-truncation class that broke essentially
every major SAML library at once in 2018. Go's options have carried CVEs. The failure mode is
**authentication bypass**, and on top of that sits an indefinite compatibility tail across Okta,
Entra, Ping, ADFS and Google. Roughly 2–4 weeks to a happy path and an unbounded security liability
afterwards, to reach a capability obtainable without writing any of it.

**Depend on a SaaS-only provider.** Rejected as a *requirement*, fine as a customer's choice.
`docs/full-stack-readiness.md` identifies portability as a genuine cleat win — Postgres and a
binary, no provider APIs in the critical path, viable on-prem, air-gapped and sovereign-cloud. A
mandatory cloud dependency **for authentication** would undercut exactly the customers most likely
to demand SAML. The buy option conflicts with the differentiator; the decision above does not,
because a customer can self-host their terminator next to cleat and have no external dependency at
all.

**Do nothing and stay OIDC-only without saying so.** Rejected. The readiness doc's argument is that
an honest concession is nearly free and ambiguity is expensive. This decision *is* the concession,
written down.

---

## What this does and does not claim

**Does:** a customer with a SAML IdP can use cleat, by running or buying a terminator. cleat's side
is configuration.

**Does not:** cleat is not "SAML-compatible" in a checkbox sense, and nothing here should be written
into `tiers.yaml` as SAML support. If a sales conversation needs the word, the honest sentence is:
*"cleat speaks OIDC; SAML is terminated by a proxy you run or buy, and here is the integration."*

**Does not solve enterprise readiness.** SAML is the cheapest item on that list. The real gate is
SOC 2 Type II — an observation window measured in months, an external auditor, and real money —
plus security questionnaires and pen test reports. IdP marketplace listings (the Okta Integration
Network, the Entra gallery) have their own review processes requiring multi-tenancy and passing
automated suites. **Shipping OIDC-with-a-terminator does not open the enterprise door on its own**,
and deciding whether to pursue enterprise at all is a separate and much larger question than this
one.

---

## What the decision costs to implement

Small, because the difficult part already exists.

**Already built:** `oauth_config` is per-tenant — `PRIMARY KEY (tenant_id, provider)` carrying
`client_id`, `redirect_url` and `domain`; the client secret is a tenant secret, sealed via
`plugin.Secrets` rather than a plaintext column on this table (cleat#1992, cleat#2295). In B2B that
per-tenant credential model is the genuinely hard multi-tenant requirement. Sessions, revocation,
the login and callback routes,
and the middleware injecting `SessionInfo{TenantID, SessionID, UserEmail}` are all present. Okta is
already effectively generic-per-tenant, its endpoints `%s`-templated against the tenant's own
domain.

**Missing:** a hardcoded allowlist of three (`validProviders` in `plugins/oauthprovider/routes.go`,
with a matching `endpoints` map) and no OIDC discovery. That is the whole gap.

**Needed:** a generic `oidc` provider kind storing an **issuer** URL; discovery via
`/.well-known/openid-configuration`; ID token validation against the discovered JWKS; and relaxing
the allowlist while keeping the three named providers as sugar over the same path.

---

## Consequences, and one hard prerequisite

**A tenant-supplied issuer URL that cleat fetches is textbook SSRF.** A tenant could point it at
link-local or RFC1918 addresses and have the worker fetch cloud credentials. **#1565, egress
policy, is a prerequisite rather than an adjacent nicety.** `redirect_url` carries the same shape
as an open-redirect risk.

ID token validation has to be strict — `iss` matching the configured issuer, `aud`, `exp`, nonce,
and signature against the *discovered* JWKS — because a permissive validator here is an
authentication bypass, which is the same failure class the decision avoids by not writing SAML.

Beyond that: a customer running a terminator is a component cleat does not operate, which is the
point and also a support surface. Document the integration for at least one self-hostable option so
"run a proxy" is a followable instruction rather than an exercise.

---

## SCIM

**Deferred, not declined.** SCIM is inbound provisioning rather than login, so it does not ride on
this decision and can be decided independently. Unlike SAML it is tractable to own — a REST/JSON
API over `/Users` and `/Groups` with a filter grammar and PATCH semantics, fiddly but not dangerous.
A directory-sync proxy can also cover it. Revisit when a real deal requires it.

---

## What was verified

**Read from the tree at `654d6f84`:** `oauth_config`'s per-tenant primary key and columns in
`plugins/oauthprovider/migrations.go`; the `endpoints` and `validProviders` maps and the Okta `%s`
templating in `routes.go`; the four registered routes; `SessionInfo` and the middleware in
`middleware.go`.

**From web search on 2026-09-14**, not from the vendors: the self-hostable and hosted terminator
landscape and its pricing shape, and the Okta Integration Network submission requirements. Vendor
capabilities and prices move faster than this file will be updated — **re-verify before anything
depends on a specific product**, and treat the structural argument as the durable part. No specific
vendor is named as a requirement here, deliberately.

**Asserted, not measured:** the implementation estimate, and the claim that XML signature validation
is the dangerous part of SAML. The latter is well-established in the literature rather than
something measured here.
