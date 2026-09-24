package plugin

import "context"

// Secrets gives a plugin access to its own tenant's secrets (cleat#1992).
// engine.SecretStore is the implementation; this interface exists so plugin
// code, which cannot import engine, can reach it.
//
// NO tenantID PARAMETER ON THE REQUEST-PATH METHODS, and that is the whole
// design. engine.SecretStore's own GetSecret/PutSecret/RetireSecret take a
// tenantID string -- correct for engine's internal callers, which read it
// straight off a verified row -- but wrong for a plugin API, because a string
// argument is something a caller can get wrong. RLS on PostgreSQL and a
// SECURITY POLICY on SQL Server protect the SESSION's tenant; neither can
// protect a plain parameter, since setting the session's tenant is a
// side-effect of that same parameter (see
// engine/a_secret_never_crosses_tenants_at_the_store_test.go's "WHAT THIS
// DOES NOT PROVE" for the measurement this reasons from). A plugin serving
// tenant B's request that passed tenantA by mistake would have the database
// agree with it.
//
// So these methods take the tenant from ctx instead -- the same tenant
// plugin.ForTenant already marks a context with before a plugin runs its own
// tenant-scoped SQL (oauthprovider's getConfig is one example).
//
// THIS REMOVES ONE FAILURE MODE, NOT EVERY ONE, and cleat-review's #2163
// review is worth stating plainly rather than eliding: plugin.ForTenant(ctx,
// A) can RE-MARK an already-authenticated request context to name a
// DIFFERENT tenant A, and Secrets.Get on that re-marked ctx would then read
// A's value. That is not new here -- it is the same thing that already lets
// a plugin's own tenant-scoped SQL run against the wrong tenant if it calls
// plugin.ForTenant a second time on a ctx that already named its caller's
// tenant -- and Secrets inherits it rather than introducing it, because it
// reads the same ctx. What this design removes is a plugin PASSING the
// wrong tenant as a Get/Put/Retire ARGUMENT; it does not remove a plugin
// RE-MARKING ctx before calling them. The two are worth keeping distinct in
// review: an argument is visible at the call site that misuses it, a re-mark
// is visible at whatever earlier call built the ctx being passed down.
//
// Get returns ErrSecretNotFound (via the underlying store) for a name this
// tenant has not set. Put stores or replaces one secret; Retire disables one
// without removing the row (see engine.SecretStore.RetireSecret's own doc
// comment for why).
type Secrets interface {
	Get(ctx context.Context, name string) (string, error)
	Put(ctx context.Context, name, value string) error
	Retire(ctx context.Context, name string) (int64, error)

	// ForTenant is the escape hatch for the two shapes of caller that have a
	// tenant in hand but not in an authenticated request context:
	//
	//   - a background loop with no request at all (datadogexport's sweep,
	//     iterating every tenant). This mirrors plugin.ForTenant on the SQL
	//     side, not plugin.AcrossAllTenants: AllTenantIDs (plugin/tenant_enum.go,
	//     cleat#2125) discovers the list with no bypass needed at all --
	//     admin.tenants carries no row-level security on any dialect -- and
	//     the loop then marks EACH iteration with ForTenant(id), one tenant
	//     at a time. AcrossAllTenants is for a different shape: a single
	//     unscoped read or write spanning every tenant in one statement (a
	//     retention cutoff, a due-schedule claim) -- see
	//     plugin/a_cross_tenant_bypass_is_declared_test.go's crossTenantLedger
	//     for the taxonomy. Nothing here is that shape: every ForTenant call
	//     still names exactly one tenant, so it is not tracked in that
	//     ledger, the same way plugin.ForTenant's own call sites are not.
	//     It IS tracked in a sibling ledger, though, for the same reason
	//     cleat#2141 tracks plugin.AllTenantIDs call sites in
	//     perTenantLoopLedger rather than crossTenantLedger: a per-tenant
	//     loop is a second way to act on every tenant, distinct from a
	//     single cross-tenant statement, and a reviewer asking "does this
	//     plugin touch every tenant" needs both ledgers to get a complete
	//     answer. See plugin/a_secrets_for_tenant_is_declared_test.go's
	//     secretsForTenantLedger;
	//   - an unauthenticated request that NAMES a tenant as its own subject
	//     rather than discovering one (oauthprovider's handleLogin, which
	//     reads ?tenant_id= because a login has no session yet to derive one
	//     from -- the tenant here is not attacker-supplied in a way that
	//     matters, because the whole call is "fetch config for the tenant
	//     this request says it is logging into").
	//
	// A ctx already marked by plugin.AcrossAllTenants is refused outright by
	// every method here, request-path and ForTenant alike, rather than
	// silently mis-scoped or bypassed -- see engine's
	// checkNotCrossTenant/errCrossTenantContext (engine/plugin_secrets.go)
	// for the per-dialect failure modes that refusal replaces.
	//
	// A SEPARATE, NAMED METHOD rather than an optional argument on Get/Put/
	// Retire, for the same reason plugin.AcrossAllTenants is a distinct call
	// from plugin.ForTenant rather than a flag: a reviewer's question about
	// where a plugin crosses tenants is one grep, and every cross-tenant call
	// site names itself.
	ForTenant(tenantID string) TenantSecrets
}

// TenantSecrets is Secrets scoped to one named tenant, returned by
// Secrets.ForTenant. See that method's doc comment for when to reach for it.
type TenantSecrets interface {
	Get(ctx context.Context, name string) (string, error)
	Put(ctx context.Context, name, value string) error
	Retire(ctx context.Context, name string) (int64, error)
}

// Payloads gives a plugin access to the same tenant-derived, rotatable
// encryption engine's own payloads use (engine.PayloadEncryption), for values
// that do not fit Secrets' shape: high-churn, one per something-other-than-a-
// fixed-name, no operator step. oauthprovider's session/access/refresh tokens
// are the first caller -- one row per session, minted on every login, with no
// fixed name and no `cleatctl set-secret` involved.
//
// Same reasoning as Secrets for the missing tenantID parameter: the tenant
// comes from ctx, via the same plugin.ForTenant marker.
//
// NOT COVERED BY `cleatctl reseal-payloads`: that command only rewrites
// event_history's own encrypted columns (engine.EncryptedEventColumns), so a
// value a plugin sealed through here stays readable after a key rotation
// only for as long as the key it was sealed under remains in the ring as a
// previous key -- a plugin storing a long-lived Sealed value is responsible
// for its own re-seal, the same way it is responsible for its own storage.
type Payloads interface {
	Seal(ctx context.Context, plaintext []byte) ([]byte, error)
	Open(ctx context.Context, sealed []byte) ([]byte, error)
}

// DeploymentSecrets gives a plugin access to credentials that belong to the
// whole deployment rather than to any one tenant (cleat#1992 part 1):
// blobstore's S3 key pair, email's SendGrid key, one key per configured llm
// provider, slacknotify's request-signing secret, scheduledbackup's
// backup-target DSN.
//
// READ-ONLY, unlike Secrets. There is no Put/Retire here because the write
// path is cleatctl's (set-deployment-secret / retire-deployment-secret), not
// a plugin's -- the same "operator-only by convention" rule Secrets.Put
// documents, made structural here rather than left to convention: a plugin
// given this interface has no method that could write.
//
// PER-USE, NOT PER-Init. A plugin must call Get at the moment it is about to
// use the credential (a dial-out, a client construction, a verification) --
// never cache the result across calls the way env.Config was read once at
// Init -- so that a credential rotated with `cleatctl set-deployment-secret`
// takes effect on the next call without a worker restart. That is the whole
// reason this exists rather than a fifth read of env.Config.
//
// FIXED, DOCUMENTED NAMES, not operator-chosen: "blobstore.access_key_id",
// "blobstore.secret_access_key", "email.sendgrid_api_key",
// "llm.providers.<provider>.api_key" (one per configured provider),
// "slacknotify.signing_secret", "scheduledbackup.dsn". A plugin looks one up
// by the name docs/how-to/use-deployment-secrets.md documents for it; no new
// config field names one.
type DeploymentSecrets interface {
	Get(ctx context.Context, name string) (string, error)
}
