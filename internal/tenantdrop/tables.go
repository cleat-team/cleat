// Package tenantdrop is the list of tables a tenant drop removes rows from, and how to
// count them. It exists so that cleatctl's drop-tenant preview and the worker's drop plan
// (cleat#3552, B3b) read ONE list: the preview has been pinned against the migrations
// since cleat#1644, and a second hand-kept copy in the worker would be the drift that pin
// exists to prevent.
//
// It is a move, not a rewrite. The entries, their comments and their queries are what
// cmd/cleatctl carried; cmd/cleatctl's tests still pin them.
package tenantdrop

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// Table is one tenant-owned table and the statement that counts a tenant's rows in it.
// Query takes the tenant id as its only parameter.
type Table struct {
	Label string
	Query string
}

// PostgresTables mirrors the rows dropping a tenant removes, for the
// dry-run / confirmation / audit output. Keep this list in sync with
// admin.drop_tenant (migrations/postgres/001_schema.sql).
//
// "Removes", not "admin.drop_tenant deletes", because two of these go by
// foreign key rather than by a DELETE inside the function, and the operator
// does not care which mechanism took their data:
//
//   - tenant_settings, ON DELETE CASCADE from 039_tenant_settings.sql
//   - tenant_domains, ON DELETE CASCADE from
//     080_a_hostname_belongs_to_one_tenant.sql (cleat#1568)
//   - tenant_secrets, ON DELETE CASCADE from
//     081_a_secret_never_reaches_the_guest.sql (cleat#1570)
//   - workflow_defs, ON DELETE CASCADE from
//     059_a_dropped_tenants_definitions_go_with_it.sql (cleat#1201)
//   - queues, ON DELETE CASCADE from
//     093_a_queue_declares_its_own_concurrency_limit.sql (cleat#1116)
//
// All cascade off admin.tenants, which drop_tenant deletes LAST -- that
// ordering is what 039 relied on and what 059 relies on. Reading only the
// function's DELETE statements to maintain this list is how tenant_settings
// came to be missing from it for twenty migrations.
var PostgresTables = []Table{
	{"workflow_instances", `SELECT count(*) FROM workflow_instances WHERE tenant_id = $1`},
	// ON DELETE CASCADE from workflow_instances.id (workflow_leases_id_fkey and
	// workflow_payloads_id_fkey in migrations/postgres/001_schema.sql, where
	// #3358 folded the migration that added them) (cleat#3245
	// Phase 3 step 1). Listed for the count, as queue_holders is above: a
	// dropped tenant's lease and payload rows go with its workflow_instances,
	// and the operator wants to see that. PostgreSQL-only, like
	// payload_encryption_ever_enabled elsewhere in this package -- SQL Server
	// and MySQL have no equivalent migration and carry this state on
	// workflow_instances itself, so mssqlTenantTables (read from sys.columns)
	// never needs to know about either name.
	{"workflow_leases", `SELECT count(*) FROM workflow_leases WHERE tenant_id = $1`},
	{"workflow_payloads", `SELECT count(*) FROM workflow_payloads WHERE tenant_id = $1`},
	{"event_history", `SELECT count(*) FROM event_history WHERE tenant_id = $1`},
	{"workflow_signals", `SELECT count(*) FROM workflow_signals WHERE tenant_id = $1`},
	{"workflow_promises", `SELECT count(*) FROM workflow_promises WHERE tenant_id = $1`},
	{"concurrency_keys", `SELECT count(*) FROM concurrency_keys WHERE tenant_id = $1`},
	{"workflow_update_requests", `SELECT count(*) FROM workflow_update_requests WHERE tenant_id = $1`},
	{"workflow_schedules", `SELECT count(*) FROM workflow_schedules WHERE tenant_id = $1`},
	{"workflow_tags", `SELECT count(*) FROM workflow_tags WHERE tenant_id = $1`},
	{"workflow_routing", `SELECT count(*) FROM workflow_routing WHERE tenant_id = $1`},
	{"idempotency_keys", `SELECT count(*) FROM idempotency_keys WHERE tenant_id = $1`},
	{"tenant_settings", `SELECT count(*) FROM tenant_settings WHERE tenant_id = $1`},
	// Also ON DELETE CASCADE rather than a DELETE inside admin.drop_tenant --
	// from 080_a_hostname_belongs_to_one_tenant.sql, which followed 039's
	// reasoning deliberately. Listed here for the same reason 039 is: the
	// operator wants the count, not the mechanism.
	{"tenant_domains", `SELECT count(*) FROM tenant_domains WHERE tenant_id = $1`},
	// ON DELETE CASCADE from 081_a_secret_never_reaches_the_guest.sql. Listed
	// for the count, as tenant_settings is -- and this one matters more than
	// most: an operator deleting a tenant wants to see that its credentials
	// went with it.
	{"tenant_secrets", `SELECT count(*) FROM tenant_secrets WHERE tenant_id = $1`},
	// ON DELETE CASCADE from admin.tenants (queues_tenant_id_fkey in
	// migrations/postgres/001_schema.sql) (cleat#1116). Listed
	// for the count, as tenant_domains and tenant_secrets are above: a
	// dropped tenant's declared concurrency limits go with it, and the
	// operator wants to see that.
	{"queues", `SELECT count(*) FROM queues WHERE tenant_id = $1`},
	// ON DELETE CASCADE from workflow_instances (queue_holders_workflow_id_fkey
	// in migrations/postgres/001_schema.sql) (cleat#1116's semaphore holder).
	// Listed for the count, as queues is
	// above: a dropped tenant's queue holders go with its workflow_instances,
	// and the operator wants to see that.
	{"queue_holders", `SELECT count(*) FROM queue_holders WHERE tenant_id = $1`},
	// ON DELETE CASCADE from admin.tenants directly
	// (queue_rate_tokens_tenant_id_fkey in migrations/postgres/001_schema.sql)
	// (cleat#1918's rate-limit counter). Deliberately
	// NOT chained off workflow_instances the way queue_holders is -- a rate
	// token's lifetime must not depend on how long completed workflows are
	// retained, so it carries its own tenant FK. Listed for the count, as
	// queue_holders is above.
	{"queue_rate_tokens", `SELECT count(*) FROM queue_rate_tokens WHERE tenant_id = $1`},
	// cleat#1644. Both carry tenant_id since 056 and neither has a foreign key
	// to anything, so neither was deleted OR counted: a dropped tenant's memory
	// profile -- which workflows it ran, and how much memory each used --
	// survived, and did not appear in the preview an operator confirms.
	{"workflow_memory_stats", `SELECT count(*) FROM workflow_memory_stats WHERE tenant_id = $1`},
	{"workflow_memory_samples", `SELECT count(*) FROM workflow_memory_samples WHERE tenant_id = $1`},
	// cleat#3540. A DELETE in admin.drop_tenant, not a cascade: no foreign key reaches it.
	// PostgreSQL only here; SQL Server's preview is read from sys.columns and MySQL has no
	// such table.
	{"tenant_library_installs", `SELECT count(*) FROM tenant_library_installs WHERE tenant_id = $1`},
	{"workflow_defs", `SELECT count(*) FROM workflow_defs WHERE tenant_id = $1`},
	{"admin.tenant_api_keys", `SELECT count(*) FROM admin.tenant_api_keys WHERE tenant_id = $1`},
	{"admin.tenant_roles", `SELECT count(*) FROM admin.tenant_roles WHERE tenant_id = $1`},
	// ON DELETE CASCADE from admin.tenants, like tenant_settings above and for
	// the same reason: the operator wants the count, not the mechanism. Found
	// by the coverage test rather than by reading the function -- it is deleted
	// correctly and was simply never counted. cleat#1644.
	{"admin.tenant_egress_allow", `SELECT count(*) FROM admin.tenant_egress_allow WHERE tenant_id = $1`},
	// ON DELETE CASCADE from admin.tenants (slack_workspace_tenant_id_fkey in
	// migrations/postgres/001_schema.sql) (cleat#2230). Listed for
	// the count, as tenant_domains and tenant_secrets are above: a dropped
	// tenant's Slack workspace mapping(s) go with it, and the operator wants
	// to see that.
	{"slack_workspace", `SELECT count(*) FROM slack_workspace WHERE tenant_id = $1`},
	{"admin.tenants", `SELECT count(*) FROM admin.tenants WHERE tenant_id = $1`},
}

// mssqlTablesSQL names every table carrying a tenant_id column.
//
// Deliberately the same predicate as the tenant_tables cursor in
// migrations/mssql/003_procedures.sql (admin.drop_tenant's sweep), minus
// that procedure's exclusions: the procedure skips admin.tenants because it
// deletes it last, and skips its five foreign-key-ordered tables because it
// names them explicitly. Both are deleted, so both belong in the preview.
const mssqlTablesSQL = `
	SELECT s.name, t.name
	FROM sys.tables t
	JOIN sys.schemas s ON s.schema_id = t.schema_id
	WHERE EXISTS (SELECT 1 FROM sys.columns c
	              WHERE c.object_id = t.object_id AND c.name = 'tenant_id')
	ORDER BY s.name, t.name`

// MSSQLTables returns one count query per tenant-owned table, in the shape PostgresTables
// has, so the printing and confirmation code is shared.
func MSSQLTables(ctx context.Context, conn *sql.Conn) ([]Table, error) {
	rows, err := conn.QueryContext(ctx, mssqlTablesSQL)
	if err != nil {
		return nil, fmt.Errorf("enumerate tenant-owned tables: %w", err)
	}
	defer rows.Close()

	var tables []Table
	for rows.Next() {
		var schema, name string
		if err := rows.Scan(&schema, &name); err != nil {
			return nil, fmt.Errorf("enumerate tenant-owned tables: %w", err)
		}
		tables = append(tables, Table{
			Label: schema + "." + name,
			Query: fmt.Sprintf("SELECT count(*) FROM %s.%s WHERE tenant_id = @p1",
				BracketQuote(schema), BracketQuote(name)),
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

// BracketQuote renders an identifier as a SQL Server delimited identifier.
//
// These names come from sys.tables rather than from a request, so this is not
// the primary defence against injection -- it is here so that a plugin table
// whose name needs quoting produces a quoted identifier instead of a syntax
// error, and so that a name containing `]` cannot end the identifier early.
// Doubling `]` is how T-SQL escapes it, and is what QUOTENAME does server-side;
// the same statement is built client-side here because the table name has to be
// part of the statement text rather than a parameter.
func BracketQuote(ident string) string {
	return "[" + strings.ReplaceAll(ident, "]", "]]") + "]"
}
