package plugin

import (
	"context"

	"github.com/cleat-team/cleat/internal/tenantctx"
)

// AcrossAllTenants marks ctx as a deliberate cross-tenant operation, so that
// statements run with it see every tenant's rows rather than one tenant's.
// It is the answer to cleat#1278.
//
//	ctx := plugin.AcrossAllTenants(p.ctx, "retention sweep: expiry is global")
//	rows, err := p.db.Query(ctx, `DELETE FROM job_queue WHERE expires_at < now()`)
//
// WHY THIS EXISTS. A tenant reaches a plugin on exactly one path -- the HTTP
// request middleware -- so a plugin's background loop has no tenant in its
// context and no way to obtain one. A tenant-scoped table's policy RAISES when
// no tenant is set, by design, so without this a sweep over such a table does
// not return fewer rows: it fails outright. That is why sixteen of the
// seventeen plugins with a table could not adopt TenantScoped after #1277.
//
// WHY IT IS A NAMED CALL RATHER THAN A CONFIGURATION. The property that makes
// the scoping worth having is that cross-tenant access cannot be reached by
// FORGETTING something -- a missing WHERE clause, an unset option, a default
// that leans open. Every one of those is a plausible accident. Typing
// AcrossAllTenants is not. A reviewer's whole question about a plugin's tenant
// handling is therefore one grep, and its answer is a list of call sites with
// a written reason attached to each.
//
// WHY IT IS NOT A SECURITY BOUNDARY, stated plainly because the name could
// suggest otherwise. Plugins are Go compiled into the worker binary. Anything
// that can write a plugin can call this, open its own pool, or bypass the
// adapter entirely. This defends against a mistake, not against an author. The
// database-side half is migrations/postgres/063, which explains what the
// engine does instead where a real bound is available -- a NOLOGIN BYPASSRLS
// role owning two functions whose bodies are the whole of the exemption.
//
// WHY THE REASON IS REQUIRED. An empty reason is an error, not a bypass: the
// adapter refuses the statement and says so. The string is written to
// cleat.cross_tenant for the life of the transaction, so a session holding a
// lock at three in the morning can be asked which sweep it is, and the policy
// tests it for emptiness rather than for presence -- which is what stops a
// pooled connection carrying a spent bypass to its next borrower.
//
// POSTGRESQL ONLY, like the scoping it lifts. MySQL has no row-level security
// and SQL Server scopes a tenant at the connector, so on both dialects plugin
// statements were never scoped and this is inert. It is safe to write in a
// plugin that runs on all three; it simply has nothing to lift there.
//
// Marking a context that already carries a tenant is allowed and the bypass
// wins. A sweep launched from a request handler is a real shape -- an admin
// endpoint that rebuilds an index for everyone -- and resolving it the other
// way would silently narrow the sweep to the caller's own tenant, which is the
// class of answer this whole mechanism exists to make impossible.
func AcrossAllTenants(ctx context.Context, reason string) context.Context {
	return tenantctx.WithCrossTenant(ctx, reason)
}
