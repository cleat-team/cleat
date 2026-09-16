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

```sql
INSERT INTO oauth_config (tenant_id, provider, client_id, client_secret, redirect_url, issuer, enabled)
VALUES (
  '00000000-0000-0000-0000-000000000001',
  'oidc',
  'cleat',
  'the-client-secret',
  'https://cleat.example.com/oauth/oidc/callback',
  'https://idp.example.com/realms/acme',
  true
);
```

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

## See also

- [The decision record](../enterprise-identity-decision.md) — why OIDC and not SAML, and what SCIM is waiting on
- [Egress policy](../operations/egress-policy.md) — the floor, and the operator allowlist
