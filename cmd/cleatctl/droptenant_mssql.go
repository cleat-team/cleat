package main

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/cleat-team/cleat/internal/tenantdrop"
	"github.com/cleat-team/cleat/plugin"
)

// ---------------------------------------------------------------------------
// drop-tenant on SQL Server
// ---------------------------------------------------------------------------
//
// cleat#1635. Two things differ from the PostgreSQL path, and both are forced
// by row-level security rather than by syntax.
//
// THE COUNTS NEED A PINNED CONNECTION. Every tenant-owned table on this dialect
// is bound to a security policy keyed on SESSION_CONTEXT('tenant_id'), and a
// filter predicate hides rows from a COUNT exactly as it hides them from a
// SELECT -- SQL Server applies RLS to sysadmin and db_owner too, so connecting
// with full privilege does not help. A count issued with no tenant key set
// returns 0 for every table, and this command would print "Nothing to delete"
// and return, having described a tenant with data as empty. That is the worst
// output this command can produce: a preview that does not describe the action.
//
// So the key has to be set -- and it has to be set on the SAME connection the
// counts run on. sp_set_session_context is per-connection state, and go-mssqldb
// calls ResetSession when a connection returns to the pool, which sets the
// RESETCONNECTION bit on the next TDS packet and CLEARS session context. On a
// *sql.DB the key would be set on one pooled connection and the counts issued
// on whichever came free. Hence db.Conn, held for the life of the command.
//
// THE TABLE SET IS DERIVED, not listed. migrations/mssql/074 sweeps every table
// carrying a tenant_id column, so asking sys.columns the same question is what
// makes the preview describe the deletion. dropTenantTables -- the hand-written
// list the PostgreSQL path uses -- has drifted three times (cleat#1644 is the
// third and is still open), and a preview built from a list that has drifted
// under-reports exactly the tables nobody remembered.

// mssqlTenantTables is tenantdrop.MSSQLTables in this command's counting shape. The
// derivation, and the reason it is derived rather than listed, moved with it.
func mssqlTenantTables(ctx context.Context, conn *sql.Conn) ([]struct{ label, query string }, error) {
	ts, err := tenantdrop.MSSQLTables(ctx, conn)
	if err != nil {
		return nil, err
	}
	out := make([]struct{ label, query string }, len(ts))
	for i, t := range ts {
		out[i] = struct{ label, query string }{t.Label, t.Query}
	}
	return out, nil
}

// setMSSQLTenantKey points the connection's security-policy key at the tenant
// whose rows are about to be counted.
//
// Rebind rather than a literal, so the tenant id is a bound parameter: this is
// the one value in this command that comes from the command line.
func setMSSQLTenantKey(ctx context.Context, conn *sql.Conn, tenantID string) error {
	stmt, args, err := plugin.RebindArgs(
		`EXEC sp_set_session_context @key = N'tenant_id', @value = $1`,
		plugin.DialectMSSQL, []any{tenantID})
	if err != nil {
		return fmt.Errorf("rebind tenant session key: %w", err)
	}
	if _, err := conn.ExecContext(ctx, stmt, args...); err != nil {
		return fmt.Errorf("set tenant session key: %w", err)
	}
	return nil
}
