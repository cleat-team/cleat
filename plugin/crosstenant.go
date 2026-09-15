package plugin

import (
	"context"

	"github.com/google/uuid"

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
// MYSQL IS INERT; SQL SERVER IS NOT, AS OF cleat#1552. MySQL has no row-level
// security at all, so there is nothing there to lift and never will be. On SQL
// Server this now sets a `cross_tenant` key in SESSION_CONTEXT, for the same
// transaction's life and by the same route the tenant takes (see
// engine/plugindb_tenant.go's markCrossTenantOnTx). It lifts nothing YET --
// applyTenantScoping still installs no SQL Server policy to read it -- so the
// observable behaviour is unchanged until that lands. It is safe to write in a
// plugin that runs on all three.
//
// THE REASON THIS PARAGRAPH USED TO GIVE WAS FALSE: "SQL Server scopes a tenant
// at the connector, so ... plugin statements were never scoped". Plugins do not
// get a connector-scoped pool -- getPluginDB hands them the main or plugin pool
// -- and sp_set_session_context is cleared when database/sql recycles a
// connection, so a per-request tenant fits after all. Measured; see
// setTenantOnTx.
//
// Marking a context that already carries a tenant is allowed and the bypass
// wins. A sweep launched from a request handler is a real shape -- an admin
// endpoint that rebuilds an index for everyone -- and resolving it the other
// way would silently narrow the sweep to the caller's own tenant, which is the
// class of answer this whole mechanism exists to make impossible.
func AcrossAllTenants(ctx context.Context, reason string) context.Context {
	return tenantctx.WithCrossTenant(ctx, reason)
}

// ForTenant marks ctx as acting for one specific tenant, so that statements run
// with it see that tenant's rows and no others.
//
// IT IS THE COUNTERPART TO AcrossAllTenants, AND THE DISTINCTION IS THE POINT.
// Until this existed the only tenant API a plugin had was the bypass, so a
// background writer that had LOST its tenant and one that never had a tenant
// looked like the same problem and got the same blunt answer. They are not the
// same problem:
//
//	no tenant to be had        a retention sweep keyed on a timestamp, an index
//	                           rebuild, a reaper -- nothing owns the rows it
//	                           touches. Use AcrossAllTenants, with a reason.
//
//	a tenant that went missing a writer handed tenantID as a PARAMETER that
//	                           derives its context from context.Background() so
//	                           the write survives a cancelled request, and drops
//	                           the tenant doing so. Use ForTenant.
//
// Reaching for the bypass in the second case works, passes every test, and
// silently disables isolation for every write on that path -- which is the
// failure this whole mechanism exists to make impossible. The two live in one
// file so a reviewer can grep it and see every place a plugin asserted "this is
// global" or "this is tenant X", each with its reason attached.
//
// Found in two plugins independently on the same evening: auditlog's recordAudit
// and eventtriggers' retryEvent both take a tenant id and both throw it away at
// a context.Background(). cleat#1278.
//
// A BYPASS ALREADY IN SCOPE WINS, AND THIS IS SILENT. beginTenantTx tests
// CrossTenant before the tenant (engine/plugindb_tenant.go), deliberately, so
// that marking a request context widens rather than narrows -- an admin endpoint
// rebuilding an index for everyone must not be scoped to whoever called it. The
// consequence is that ForTenant inside an AcrossAllTenants scope is ignored
// without a word. If you need one statement scoped inside a sweep, build it from
// a context that is not the bypassed one.
//
// MYSQL IS INERT; SQL SERVER CARRIES THE TENANT AS OF cleat#1552. MySQL has no
// row-level security, so nothing reads what this sets and nothing ever will.
//
// On SQL Server the tenant now reaches the statement in SESSION_CONTEXT, which
// CHANGES WHAT A PLUGIN SEES ON A CORE TABLE and is worth stating plainly: every
// one of the shipped ADD FILTER PREDICATE statements binds dbo.fn_tenant_filter
// (20 of them, 13 distinct tables --
// `grep -rhoE 'ADD FILTER PREDICATE\s+dbo\.\w+\(' migrations/mssql/*.sql`), and
// that function's authoritative definition reads SESSION_CONTEXT(N'tenant_id')
// (migrations/mssql/012_admin_role.sql, the highest-numbered file defining it).
// So a plugin statement against workflow_instances under ForTenant used to
// match no rows and now matches that tenant's. That is a narrowing to the
// correct answer rather than a widening -- no tenant's rows become visible to
// anyone who could not already ask for them -- but it is a behaviour change on
// a dialect where plugins previously saw nothing.
//
// PLUGIN tables are still unprotected there, until applyTenantScoping grows its
// SQL Server arm. It is safe to write in a plugin that runs on all three.
//
// The claim this paragraph used to make -- that SQL Server scopes a tenant at
// the connector, so a per-request tenant does not fit -- is corrected at
// AcrossAllTenants above.
func ForTenant(ctx context.Context, tenantID uuid.UUID) context.Context {
	return tenantctx.With(ctx, tenantID)
}
