package main

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

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

// mssqlTenantTablesSQL names every table carrying a tenant_id column.
//
// Deliberately the same predicate as the cursor in migrations/mssql/074, minus
// that procedure's exclusions: the procedure skips admin.tenants because it
// deletes it last, and skips its five foreign-key-ordered tables because it
// names them explicitly. Both are deleted, so both belong in the preview.
const mssqlTenantTablesSQL = `
	SELECT s.name, t.name
	FROM sys.tables t
	JOIN sys.schemas s ON s.schema_id = t.schema_id
	WHERE EXISTS (SELECT 1 FROM sys.columns c
	              WHERE c.object_id = t.object_id AND c.name = 'tenant_id')
	ORDER BY s.name, t.name`

// mssqlTenantTables returns one count query per tenant-owned table, in the
// shape dropTenantTables has, so the printing and confirmation code is shared.
func mssqlTenantTables(ctx context.Context, conn *sql.Conn) ([]struct{ label, query string }, error) {
	rows, err := conn.QueryContext(ctx, mssqlTenantTablesSQL)
	if err != nil {
		return nil, fmt.Errorf("enumerate tenant-owned tables: %w", err)
	}
	defer rows.Close()

	var tables []struct{ label, query string }
	for rows.Next() {
		var schema, name string
		if err := rows.Scan(&schema, &name); err != nil {
			return nil, fmt.Errorf("enumerate tenant-owned tables: %w", err)
		}
		tables = append(tables, struct{ label, query string }{
			label: schema + "." + name,
			query: fmt.Sprintf("SELECT count(*) FROM %s.%s WHERE tenant_id = @p1",
				bracketQuote(schema), bracketQuote(name)),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("enumerate tenant-owned tables: %w", err)
	}
	if len(tables) == 0 {
		// Not a "healthy empty". No table in this database carries a tenant_id
		// column at all, which means the migrations have not been applied --
		// and counting zero rows would print the same "Nothing to delete" as a
		// tenant that genuinely has none.
		return nil, fmt.Errorf("no table in this database has a tenant_id column -- has this database been migrated?")
	}
	return tables, nil
}

// bracketQuote renders an identifier as a SQL Server delimited identifier.
//
// These names come from sys.tables rather than from a request, so this is not
// the primary defence against injection -- it is here so that a plugin table
// whose name needs quoting produces a quoted identifier instead of a syntax
// error, and so that a name containing `]` cannot end the identifier early.
// Doubling `]` is how T-SQL escapes it, and is what QUOTENAME does server-side;
// the same statement is built client-side here because the table name has to be
// part of the statement text rather than a parameter.
func bracketQuote(ident string) string {
	return "[" + strings.ReplaceAll(ident, "]", "]]") + "]"
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
