package engine

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/google/uuid"

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
// WHY IT IS GATED ON THE CONTEXT CARRYING A TENANT. Two paths supply one:
// the HTTP middleware at cmd/cleat-worker/main.go, and -- since cleat#1278 --
// the host-call boundary, where engine.pluginCallContext bridges the workflow's
// own tenant into tenantctx before invoking a plugin function. What still has
// no tenant is a background loop, and a scheduler sweeping every tenant's due
// rows is legitimately cross-tenant. Scoping only when a tenant is present
// leaves those paths exactly as they were, which is what keeps this change
// from reaching code it has no business reaching.
//
// THAT SENTENCE USED TO READ "a tenant reaches a plugin on exactly one path
// ... host calls and background loops have none", and it was the justification
// for the gate rather than a passing remark -- which is why it is rewritten
// here rather than left for the next reader to discover is false. cleat#1278
// is the issue that sentence named as future work.
//
// POSTGRESQL AND SQL SERVER. MySQL has no row-level security at all and is
// left alone; its tenancy is a database boundary rather than a policy (see
// cmd/cleat-worker/main.go).
//
// THIS PARAGRAPH USED TO SAY "POSTGRESQL ONLY, AND THE FIELD SAYS SO", and the
// reason it gave was false: "sp_set_session_context has no transaction-local
// scope and would leak to the next borrower of a pooled connection". It does
// not leak, and the fact that refutes it was already in this tree --
// engine/mssql_store.go records, from a defect that cost the store dearly, that
// database/sql calls ResetSession when it recycles a connection and go-mssqldb
// answers with sp_reset_connection, WHICH CLEARS SESSION_CONTEXT. Two comments
// in one repository disagreed about one mechanism for as long as nobody needed
// both at once. See setTenantOnTx for the measurement that settles it.
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
// security, or a context with no tenant. Both are ordinary states rather than
// errors: a background loop legitimately has no tenant. (Until cleat#1278 this
// sentence also named host calls, which now carry one.)
//
// "No tenant" means tenantctx.From returned !ok, and that is NOT the same as
// the zero UUID. This comment used to say scoping to the zero UUID "would
// match nothing and read as an empty table", which was already wrong when it
// was written: 00000000-0000-0000-0000-000000000000 is engine.DefaultTenantUUID,
// it is a real row in admin.tenants, and it is the column default for
// workflow_instances.tenant_id -- so in a single-tenant deployment it is the
// tenant every row carries. Scoping to it matches everything that exists, which
// is correct there and is why pluginCallContext bridges it like any other
// value. Verified against a migrated database rather than reasoned about.
func beginTenantTx(ctx context.Context, db *sql.DB, dialect plugin.Dialect, opts *sql.TxOptions) (*sql.Tx, error) {
	if dialect != plugin.DialectPostgres && dialect != plugin.DialectMSSQL {
		return nil, nil
	}
	// The bypass is tested BEFORE the tenant, so that marking a context which
	// already carries one widens rather than narrows. See the note at the end
	// of plugin.AcrossAllTenants: an admin endpoint that rebuilds an index for
	// every tenant runs on a request context, and resolving that the other way
	// would quietly scope the sweep to whoever called it. cleat#1278.
	if reason, ok := tenantctx.CrossTenant(ctx); ok {
		if strings.TrimSpace(reason) == "" {
			// Not a bypass, and not the fail-closed path either. Falling
			// through to the tenant branch here would produce
			// "cleat.tenant_id is not set" from a sweep that has no tenant to
			// set, sending its author after the wrong thing entirely.
			return nil, fmt.Errorf(
				"plugin: AcrossAllTenants needs a reason; the empty string is not one")
		}
		tx, err := beginScopedTx(ctx, db, dialect, opts)
		if err != nil {
			return nil, err
		}
		if err := markCrossTenantOnTx(ctx, tx, dialect, reason); err != nil {
			_ = tx.Rollback()
			return nil, err
		}
		return tx, nil
	}
	tid, ok := tenantctx.From(ctx)
	if !ok {
		return nil, nil
	}
	tx, err := beginScopedTx(ctx, db, dialect, opts)
	if err != nil {
		return nil, err
	}
	if err := setTenantOnTx(ctx, tx, dialect, tid); err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	return tx, nil
}

// beginScopedTx opens the transaction the scoping below is applied to.
//
// It exists to drop sql.TxOptions.ReadOnly on SQL Server, which has no
// read-only transaction and whose driver does not treat the option as a no-op.
// Measured against go-mssqldb v1.10.0 and SQL Server 2022:
//
//	db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
//	  -> read-only transactions are not supported
//
// ReadOnlyDB is the only caller that sets it, and its guarantee is enforced in
// Go rather than by the database -- Exec is denied on the adapter and on the
// transaction it hands back -- so dropping the option here loses nothing SQL
// Server was providing.
//
// WHAT THIS DOES NOT DO is make ReadOnlyDB.Begin work on SQL Server. The next
// statement that method runs is `SET TRANSACTION READ ONLY`, PostgreSQL and
// MySQL syntax that SQL Server rejects outright ("Incorrect syntax near the
// keyword 'READ'"), so Begin still fails there -- one line later, with a
// different message. That is cleat#1615 and it is deliberately left alone:
// what a read-only transaction should mean on SQL Server is a decision, not a
// line to change on the way past. Query and QueryRow, which do not run that
// statement, do become tenant-scoped here.
func beginScopedTx(ctx context.Context, db *sql.DB, dialect plugin.Dialect, opts *sql.TxOptions) (*sql.Tx, error) {
	if dialect == plugin.DialectMSSQL && opts != nil && opts.ReadOnly {
		relaxed := *opts
		relaxed.ReadOnly = false
		opts = &relaxed
	}
	return db.BeginTx(ctx, opts)
}

// setTenantOnTx puts the tenant where this dialect's policies look for it.
//
// POSTGRESQL. `true` is the is_local flag: the setting reverts when this
// transaction ends, so it cannot follow the connection back into the pool. It
// is also what makes this legal inside a READ ONLY transaction, where a
// session-level SET would not be.
//
// SQL SERVER. sp_set_session_context is SESSION-scoped, not transaction-scoped,
// and reverts by a different route: database/sql calls ResetSession when it
// recycles a pooled connection, go-mssqldb answers by setting the TDS
// RESETCONNECTION status bit on the next packet (mssql_go110.go:19,
// buf.go:136-143), and that clears SESSION_CONTEXT. engine/mssql_store.go
// records the same behaviour from the other side, where the clearing was a
// DEFECT: setting the context once per connection rather than once per use
// meant tenant-scoped reads returned nothing (IMPROVEMENT-PLAN 2.71).
//
// Measured on one physical connection (SetMaxOpenConns(1)), so every borrow
// after the first is a reuse, against SQL Server 2022 (16.0.4275.2) and
// go-mssqldb v1.10.0 -- the first two rows are the control, establishing that
// the reader can tell "set" from "unset" at all:
//
//	fresh connection, never set                    -> NULL
//	same connection, after sp_set_session_context   -> the value
//	next borrow, after the conn returned to the pool -> NULL
//	next borrow, after a COMMITTED tx set it         -> NULL
//	next borrow, after a ROLLED BACK tx set it       -> NULL
//	next borrow, after the tx's context was CANCELLED -> NULL
//
// The last row is the one worth keeping: a cancelled request is the realistic
// way a transaction ends without reaching either Commit or Rollback, and it is
// the case a reader will worry about.
//
// So the lifetime matches PostgreSQL's by a different mechanism, and that is
// the whole risk in this change: the property belongs to the DRIVER, not to
// cleat. TestATenantKeyDoesNotSurviveTheSQLServerConnectionPool exists to fail if
// go-mssqldb ever stops answering ResetSession, because the symptom otherwise
// is one tenant reading another's rows with no error anywhere.
//
// The value is BOUND, not interpolated. engine/mssql_store.go's
// applyTenantSessionContext interpolates and says why -- it runs on a raw
// driver.Conn, which go-mssqldb does not give an ExecerContext. Here the
// statement goes through *sql.Tx, so an ordinary parameter works; verified that
// the bound value still CASTs to UNIQUEIDENTIFIER the way dbo.fn_tenant_filter
// needs, which is the only thing interpolation was buying.
func setTenantOnTx(ctx context.Context, tx *sql.Tx, dialect plugin.Dialect, tid uuid.UUID) error {
	switch dialect {
	case plugin.DialectPostgres:
		_, err := tx.ExecContext(ctx,
			`SELECT set_config('cleat.tenant_id', $1, true)`, tid.String())
		return err
	case plugin.DialectMSSQL:
		_, err := tx.ExecContext(ctx,
			`EXEC sp_set_session_context @key = N'tenant_id', @value = @p1`, tid.String())
		return err
	}
	return nil
}

// markCrossTenantOnTx records that this transaction is a deliberate
// cross-tenant sweep.
//
// POSTGRESQL sets BOTH a GUC and a role, because the two cover different
// policies.
//
// The GUC is what migration 063's CASE predicate tests, and policies written
// before migration 077 still carry it -- a plugin whose migrations have not
// been re-run, or a database upgraded but not yet swept. The role is what 077's
// `TO cleat_sweep USING (true)` policy matches. Setting only one of them
// silently narrows the sweep to whichever half of the tree happens to be on
// that form, and a narrowed sweep returns FEWER rows rather than an error.
//
// SET LOCAL, like the set_config, so it reverts with the transaction and cannot
// follow the connection back into the pool. The connecting role needs
// membership in cleat_sweep, granted WITH INHERIT FALSE by migration 077 --
// enough to SET ROLE, not enough to match the sweep policy passively. A plain
// GRANT there is a silent cross-tenant leak: measured, the application role
// then reads every tenant's rows with no error and the correct number of
// policies. cleat#1490.
//
// SQL SERVER HAS ONLY THE FIRST HALF, and the reason is structural rather than
// an omission: there is no SET ROLE. Database role membership is a property of
// the connection, so the PostgreSQL trick -- hold a membership you cannot use
// passively, enter it for one transaction -- has no counterpart. The remaining
// options are a second pool under a privileged login (a second credential in
// every deployment's config) or a session-context key, and this takes the key.
//
// WHY THAT IS NOT THE SENTINEL migrations/mssql/012_admin_role.sql REFUSES.
// 012 rejects a magic tenant_id VALUE, because anything that can call
// sp_set_session_context could then assume a particular tenant's identity. A
// separate key is not an identity: it grants no tenant, and the shipped engine
// predicate dbo.fn_tenant_filter reads `tenant_id` and nothing else, so setting
// this key cannot widen access to any of the thirteen engine tables. What it
// will admit, once the policies of cleat#1552 exist, is exactly the plugin
// tables that opt in.
//
// It is also the trust level PostgreSQL already grants these same tables:
// migration 063's cleat.cross_tenant is set by the application with no DBA
// involved. PostgreSQL is moving to the stricter role form (077) and SQL Server
// cannot follow it. That asymmetry is real and is written here rather than
// smoothed over.
func markCrossTenantOnTx(ctx context.Context, tx *sql.Tx, dialect plugin.Dialect, reason string) error {
	switch dialect {
	case plugin.DialectPostgres:
		// The policy tests this setting for EMPTINESS rather than presence,
		// because a reverted is_local setting reads as "" and not as NULL --
		// migration 034 is the same PostgreSQL behaviour, found the same way.
		if _, err := tx.ExecContext(ctx,
			`SELECT set_config('cleat.cross_tenant', $1, true)`, reason); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `SET LOCAL ROLE cleat_sweep`); err != nil {
			return fmt.Errorf(
				"plugin: cross-tenant sweep %q could not enter cleat_sweep: %w "+
					"(the connecting role needs GRANT cleat_sweep ... WITH INHERIT FALSE; "+
					"see migrations/postgres/077)", reason, err)
		}
		return nil
	case plugin.DialectMSSQL:
		_, err := tx.ExecContext(ctx,
			`EXEC sp_set_session_context @key = N'cross_tenant', @value = @p1`, reason)
		return err
	}
	return nil
}
