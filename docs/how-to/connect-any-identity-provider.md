# Connect any identity provider (generic OIDC)

cleat authenticates users through OpenID Connect. Alongside the three providers
it names directly — `google`, `github`, `okta` — there is a generic `oidc`
provider that works with **any** conforming issuer: Entra ID, Keycloak,
Authentik, Zitadel, Auth0, Ping, your own.

If you need SAML, you terminate it yourself and present the result to cleat as
OIDC. cleat does not implement SAML, and [the decision
record](../enterprise-identity-decision.md) explains why: a SAML proxy *is* an
OIDC provider, so "support any IdP" and "support SAML" are one mechanism rather
than two — and cleat never handles an XML signature, which is the genuinely
dangerous part.

---

## Configure an issuer

Provider configuration is per tenant, in `oauth_config`. For `oidc` the issuer
URL is the new part; everything else is what you already supply for a named
provider.

The client secret does not live in `oauth_config` -- it is a tenant secret,
sealed the same way every other per-tenant credential is (cleat#1992).
`set-secret` requires the tenant to already exist -- it writes a row with a
foreign key to `admin.tenants`, and fails on a fresh database if that tenant
was never created:

```sql
INSERT INTO admin.tenants (tenant_id, name)
VALUES ('11111111-1111-1111-1111-111111111111', 'acme');
```

(Skip this if you already have a tenant -- any existing `tenant_id` works.)

Then set the secret and insert the rest of the row:

```
cleatctl set-secret 11111111-1111-1111-1111-111111111111 \
  --name oauthprovider.client_secret.oidc
# reads the value from stdin, or pass --from-file <path>
```

```sql
INSERT INTO oauth_config (tenant_id, provider, client_id, redirect_url, issuer, enabled)
VALUES (
  '11111111-1111-1111-1111-111111111111',
  'oidc',
  'cleat',
  'https://cleat.example.com/oauth/oidc/callback',
  'https://idp.example.com/realms/acme',
  true
);
```

A row with no matching secret is not silently treated as unconfigured: login
and callback both fail closed, with a generic `oauth config not found`
response. The distinction -- config row present but no secret set, versus no
config at all -- is logged server-side (`secret_not_found=true`), not
returned to the caller: `/login` is unauthenticated, so an error naming
`set-secret` or cleat's internal secret-naming scheme would hand an anonymous
caller both an existence oracle and detail about the deployment's own tooling
(cleat-review, cleat#2295). If login fails right after configuring a
provider, check the worker log for `secret_not_found` before assuming
anything else is wrong.

`issuer` is the **issuer URL**, not the discovery URL. cleat appends
`/.well-known/openid-configuration` to it and reads `authorization_endpoint`,
`token_endpoint`, `userinfo_endpoint` and `jwks_uri` from the response.

The path matters. An issuer of `https://idp.example.com/realms/acme` discovers
at `https://idp.example.com/realms/acme/.well-known/openid-configuration` — the
path is kept, not replaced. Multi-tenant IdPs that give each realm its own path
depend on this.

Register `https://<your-cleat-host>/oauth/oidc/callback` as a redirect URI with
the IdP.

### Requirements on the issuer

| Requirement | Why |
|---|---|
| `https` | Everything cleat trusts about the IdP arrives over this connection. Plain `http` is refused. |
| Discovery document declares the same issuer | Otherwise whatever answers for that host could point `token_endpoint` at a third party. Refused. |
| Reachable under the egress policy | The issuer URL is tenant-supplied and cleat fetches it. See below. |
| Publishes a JWKS | Required only if the IdP returns an `id_token`, which conforming ones do. |

---

## Egress: the issuer has to be reachable, deliberately

A tenant-supplied URL that the worker fetches is textbook SSRF — point it at
`169.254.169.254` and the worker fetches cloud credentials. cleat routes every
one of these fetches through the egress guard, which refuses link-local and
private ranges as a floor that no allowlist overrides.

For a **public** IdP nothing is needed. For an IdP on a private network — a
self-hosted Keycloak, a SAML terminator in the same VPC — the operator opts that
specific host in:

```
cleat-worker --plugin-egress-allow-private idp.internal
```

This is an operator decision on purpose. A tenant cannot allowlist a private
host for itself, because "which internal hosts exist" is not a tenant's to know.

---

## SAML, via a terminator

Run something that speaks SAML to your IdP and OIDC to cleat, then point cleat
at the terminator as an ordinary issuer. Keycloak is the common self-hostable
choice and needs no custom code:

1. Run Keycloak and create a realm, say `acme`.
2. In that realm, add a SAML v2.0 **identity provider** and import your IdP's
   SAML metadata.
3. In the same realm, create an OIDC **client** for cleat: client ID `cleat`,
   redirect URI `https://<your-cleat-host>/oauth/oidc/callback`. Copy the
   client secret.
4. Configure cleat with
   `issuer = https://keycloak.example.com/realms/acme` and that client.

Users then log in through cleat → Keycloak → your SAML IdP, and cleat only ever
speaks OIDC. Authentik and Zitadel work the same way; so does a hosted service
if you would rather not run one.

---

## Who may sign in: the allowlist

Completing a login produces a **real cleat API key** for that tenant. There is
no separate "OAuth credential" — the key is an ordinary `admin.tenant_api_keys`
row, so anything that accepts an API key accepts it.

> **A minted key carries FULL TENANT ACCESS.** `tenant_api_keys` has no role or
> scope column in this release, so an identity admitted here can do everything
> the tenant's API key can: deploy workflow code (`POST /api/definitions`,
> `/api/versions`), reprocess runs, manage schedules and plugins, and reach
> `/api/admin/*` where it is enabled. **OAuth login is operator SSO, and the
> allowlist is an operator list. Add only identities you would hand that
> tenant's API key to.** Nothing enforces that for you.

Because of that, **the allowlist is mandatory and fails closed**: a
`(tenant, provider)` pair with no rows refuses every login for that pair. There
is no switch to turn the check off, and "there is no list" and "this person is
not on the list" are deliberately the same answer — the alternative (treating an
empty list as "admit everyone") fails *open*, which is the direction an operator
mistyping a tenant id would never notice.

```bash
# Admit an address, and see what you are granting:
cleatctl --db "$DSN" oauth-allow add <tenant-uuid> --provider oidc ada@example.com

# Admit a stable subject instead. Use this when the issuer publishes no verified
# address, or to survive an address being reassigned:
cleatctl --db "$DSN" oauth-allow add <tenant-uuid> --provider oidc --type subject 00u1a2b3c

# See who is admitted (this also prints the warning above):
cleatctl --db "$DSN" oauth-allow list <tenant-uuid> --provider oidc

# Remove someone — and revoke the keys that row minted, in the same command:
cleatctl --db "$DSN" oauth-allow remove <tenant-uuid> --provider oidc ada@example.com
```

`--type email` (the default) matches only an address the provider **vouches
for**: for OIDC, the ID token or userinfo must assert `email_verified`, and for
GitHub cleat consults `/user/emails` and takes only an entry that is both
`primary` and `verified`. An unverified address is carried as a label and never
used as a key — an IdP that lets a user set an unverified address would
otherwise let anyone who claimed `ceo@yourcompany.com` into the tenant.
Comparison is case-folded for an address and case-**sensitive** for a subject
(OIDC Core §2 defines `sub` that way). `--type subject` is therefore both the
stricter key and the more durable one, and a tenant can hold both kinds and
migrate at leisure.

`remove` is not just a delete: it revokes the live keys that row minted. A
removal that only deleted the row would leave the person's credential
authenticating until it expired, which is not what an operator means by
"remove".

**PostgreSQL only, in this release.** On MySQL and SQL Server, `/login` and
`/callback` answer `501` with a message naming the limit, and `cleatctl
oauth-allow` refuses, rather than half-working. A minted key expires at
`min(expires_in, 24h)` — a provider reporting a week does not get a week, and one
reporting nothing gets 24 hours rather than a permanent credential.

---

## What cleat checks on the way in

Identity comes from the `userinfo` endpoint, which is the path the named
providers have always used. When the IdP also returns an `id_token` — and a
conforming one does — cleat validates it, and a token that fails **fails the
login**. There is no path that downgrades a failed check to a warning, because
a validator you can skip is not a validator.

What is checked: the signature, against a key from the issuer's published JWKS;
`iss`; `aud` against your client ID; `exp`, which must be present; and the
`nonce`, against the one minted for that specific login, so a token captured
from another session cannot be replayed.

Only asymmetric signatures are accepted (RS/PS/ES). `alg: none` and HMAC are
refused: the JWKS is public, so accepting HMAC would let anyone who can read it
mint an identity.

A rotated signing key recovers on its own — an unrecognised key triggers one
refetch of the JWKS before the token is rejected — so a routine rotation costs a
fetch rather than an outage.

---

## Troubleshooting

| Symptom | Cause |
|---|---|
| `provider discovery failed` at login | cleat could not fetch or parse the discovery document. Check the issuer URL, that it is `https`, and that egress permits the host. |
| `discovery document declares issuer …` | The `issuer` in the document does not match what you configured. Use the value the document reports, exactly. |
| `id_token validation failed` | Signature, `iss`, `aud`, `exp` or `nonce` did not check out. The worker log names which. `aud` mismatch usually means `client_id` differs from the client the IdP issued the token to. |
| `issuer … publishes no signing keys` | The discovery document has no `jwks_uri`, or the set held no usable key, but an `id_token` was returned. |
| `provider "oidc" requires an issuer URL` | The `oauth_config` row has an empty `issuer`. |
| `501` from `/login` or `/callback` | This worker is not on PostgreSQL. OAuth login is Postgres-only in this release; the response names the limit. |
| `403 identity_not_allowlisted` | That identity is not on the tenant's allowlist for this provider. `cleatctl oauth-allow list <tenant> --provider <p>` shows who is. The worker log names the identity that was refused. |

## See also

- [The decision record](../enterprise-identity-decision.md) — why OIDC and not SAML, and what SCIM is waiting on
- [Egress policy](../operations/egress-policy.md) — the floor, and the operator allowlist
