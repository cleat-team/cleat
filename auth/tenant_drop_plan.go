package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/internal/tenantdrop"
)

// DropPlanFacts is the part of a drop plan that is read on the ADMIN connection (cleat#3552, B3b-1b).
//
// The rest of a plan, the counts of a tenant's data, must be read in that tenant's own scope (see
// engine.TenantRowCounter). What is here is what that scope cannot read, measured on PostgreSQL with
// the RLS role: admin.tenant_api_keys, admin.tenant_roles and admin.tenant_egress_allow are refused to
// it. They carry no row-level security, so the admin connection counts them without a tenant key.
type DropPlanFacts struct {
	Name      string
	Suspended bool

	// CreatedBy is the principal that created the tenant, or "" for one made offline. The caller
	// compares it, as the suspend and confirm routes do.
	CreatedBy string

	// AdminTables are the admin.* counts. PostgreSQL only: SQL Server counts every table, admin ones
	// included, from one derived list in the tenant's scope, so it has nothing to put here.
	AdminTables []tenantdrop.RowCount

	// PluginTables are the registered per-tenant plugin tables that exist, and SkippedPluginTables the
	// registry rows naming a table that does not. PostgreSQL only: SQL Server's derived list already
	// includes plugin tables, because it asks the catalogue for every tenant_id column.
	PluginTables        []tenantdrop.PluginTable
	SkippedPluginTables []string
}

// DropPlanFacts reads what a drop plan needs from the admin connection. A tenant that does not exist
// answers ErrTenantNotFound. It changes nothing.
//
// PostgreSQL and SQL Server only; MySQL is single-tenant and has no drop to plan.
func (s *TenantStore) DropPlanFacts(ctx context.Context, tenantID uuid.UUID) (DropPlanFacts, error) {
	var facts DropPlanFacts
	var q string
	switch s.dialect {
	case DialectPostgres:
		q = `SELECT name, suspended, COALESCE(created_by, '') FROM admin.tenants WHERE tenant_id = $1`
	case DialectMSSQL:
		q = `SELECT name, suspended, ISNULL(created_by, N'') FROM admin.tenants WHERE tenant_id = @p1`
	default:
		return facts, fmt.Errorf("auth: a drop plan is not available on %s", s.dialect)
	}
	if err := s.db.QueryRowContext(ctx, q, tenantID.String()).Scan(&facts.Name, &facts.Suspended, &facts.CreatedBy); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return facts, ErrTenantNotFound
		}
		return facts, err
	}
	if s.dialect != DialectPostgres {
		return facts, nil
	}

	for _, t := range tenantdrop.PostgresAdminTables() {
		var n int64
		if err := s.db.QueryRowContext(ctx, t.Query, tenantID.String()).Scan(&n); err != nil {
			return facts, fmt.Errorf("count %s: %w", t.Label, err)
		}
		facts.AdminTables = append(facts.AdminTables, tenantdrop.RowCount{Table: t.Label, Rows: n})
	}

	// The registry, read the way admin.drop_tenant reads it: tenant_scoped rows only, and a row whose
	// table does not exist is skipped (drop_tenant warns and continues), so it is reported rather than
	// counted as an empty table.
	rows, err := s.db.QueryContext(ctx, `
		SELECT schema_name, table_name, to_regclass(format('%I.%I', schema_name, table_name)) IS NOT NULL
		FROM admin.plugin_tables
		WHERE tenant_scoped
		ORDER BY schema_name, table_name`)
	if err != nil {
		return facts, fmt.Errorf("read the plugin table registry: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var p tenantdrop.PluginTable
		var exists bool
		if err := rows.Scan(&p.Schema, &p.Table, &exists); err != nil {
			return facts, fmt.Errorf("read the plugin table registry: %w", err)
		}
		if exists {
			facts.PluginTables = append(facts.PluginTables, p)
		} else {
			facts.SkippedPluginTables = append(facts.SkippedPluginTables, p.Label())
		}
	}
	return facts, rows.Err()
}
