package engine

import (
	"context"
	"database/sql"

	"github.com/cleat-team/cleat/internal/tenantctx"
	"github.com/cleat-team/cleat/plugin"
)

// Tenant scoping for plugin statements. cleat#1277.
//
// WHY THIS IS HERE AND NOT IN EACH PLUGIN. Every plugin table carries a
// tenant_id and every plugin hand-writes `WHERE tenant_id = $1` to honour it.
// That is the same shape as the Rebind problem #1133 solved one layer up: a
// requirement authors meet most of the time, whose miss rate does not improve
// on its own. The difference is the consequence. A forgotten Rebind fails
// loudly on MySQL; a forgotten tenant predicate returns another tenant's rows
// and every test still passes.
//
// A database policy is the only thing that fails closed, and a policy needs
// `cleat.tenant_id` set on the connection running the statement. Nothing set
// it: plugins hold a bare *sql.DB (see getPluginDB in cmd/cleat-worker), not
// the store, so the engine's beginTxWithRLS never touched their statements.
// This supplies the missing half.
//
// WHY IT IS SAFE TO LAND BEFORE ANY POLICY EXISTS. Setting a GUC that no
// policy reads is a no-op. That ordering is deliberate and is the reverse of
// the obvious one: shipping policies first would fail every plugin statement
// closed on contact, because nothing would be setting the value they filter
// on.
//
// WHY IT IS GATED ON THE CONTEXT CARRYING A TENANT. A tenant reaches a plugin
// on exactly one path -- the HTTP middleware at cmd/cleat-worker/main.go,
// the only non-test caller of auth.WithTenantID. Host calls and background
// loops have none, and a scheduler sweeping every tenant's due rows is
// legitimately cross-tenant. Scoping only when a tenant is present leaves
// those paths exactly as they were, which is what keeps this change from
// reaching code it has no business reaching.
//
// POSTGRESQL ONLY, AND THE FIELD SAYS SO. MySQL has no row-level security at
// all. SQL Server scopes a tenant at the CONNECTOR (tenantSessionConnector in
// mssql_store.go builds one pool per tenant), so a per-request tenant does not
// fit its model without separate work; sp_set_session_context has no
// transaction-local scope and would leak to the next borrower of a pooled
// connection. Both are left untouched rather than half-covered.
func (a *SQLDBAdapter) tenantTx(ctx context.Context) (*sql.Tx, error) {
	return beginTenantTx(ctx, a.DB, a.Dialect, nil)
}

// beginTenantTx is the gate itself, shared by both plugin.PluginDB
// implementations. cleat#1285.
//
// It is a free function rather than a method because there are TWO adapters
// over the pool -- SQLDBAdapter and ReadOnlyDB -- and #1280 scoped only the
// first. That gap was not a leak but a hard failure: a read-only plugin
// reading a TenantScoped table got
//
//	cleat.tenant_id is not set -- tenant context required for RLS-scoped query
//
// even with a tenant in the request context, because nothing set the value
// its policy filters on. Keeping one implementation is what stops the two
// from drifting again.
//
// opts is passed through to BeginTx so ReadOnlyDB can keep its read-only
// transaction; nil gives the default read-write.
//
// Returns (nil, nil) when no scoping applies -- a dialect without row-level
// security, or a context with no tenant. Both are ordinary states, not
// errors: host calls and background loops legitimately have no tenant, and
// scoping them to the zero UUID would match nothing and read as an empty
// table.
func beginTenantTx(ctx context.Context, db *sql.DB, dialect plugin.Dialect, opts *sql.TxOptions) (*sql.Tx, error) {
	if dialect != plugin.DialectPostgres {
		return nil, nil
	}
	tid, ok := tenantctx.From(ctx)
	if !ok {
		return nil, nil
	}
	tx, err := db.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	// `true` is the is_local flag: the setting reverts when this transaction
	// ends, so it cannot follow the connection back into the pool. It is also
	// what makes this legal inside a READ ONLY transaction, where a
	// session-level SET would not be.
	if _, err := tx.ExecContext(ctx,
		`SELECT set_config('cleat.tenant_id', $1, true)`, tid.String()); err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	return tx, nil
}
