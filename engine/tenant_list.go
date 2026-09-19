package engine

import (
	"context"
	"fmt"
)

// TenantLister enumerates the tenants a worker could claim work for.
//
// # Why this is not a cross-tenant read
//
// Every other "across tenants" operation on a store needs an RLS exemption and
// says so. This one does not, and the difference is the table: `admin.tenants`
// carries no row-level security, while `workflow_instances` has it enabled and
// FORCEd.
//
//	admin.tenants        relrowsecurity = f   relforcerowsecurity = f
//	workflow_instances   relrowsecurity = t   relforcerowsecurity = t
//
// `cleat_app` already holds SELECT on it. So a worker can learn WHICH tenants
// exist through its ordinary connection, and then claim each tenant's work
// under that tenant's own RLS context -- which is what the rotating claim does,
// and why it needs no `cleat_dispatcher` role.
//
// That matters beyond tidiness. `BYPASSRLS` can only be granted by a true
// superuser, and managed PostgreSQL does not have one: RDS's master role is
// documented as `NOSUPERUSER ... CREATEDB CREATEROLE`, and Cloud SQL and Azure
// are equivalent. Measured on PostgreSQL 16, a role of exactly that shape gets
//
//	ERROR:  permission denied to create role
//	DETAIL:  Only roles with the BYPASSRLS attribute may create roles with
//	         the BYPASSRLS attribute.
//
// applying migration 023. So the function-based cross-tenant claim is not
// merely unconfigured on those platforms, it is unavailable -- and enumerating
// here is what makes a multi-tenant worker possible on them at all.
//
// # What it deliberately does not do
//
// It does not report which tenants HAVE runnable work. That read is against
// `workflow_instances`, which is RLS-scoped, so answering it across tenants
// would need the exemption this exists to avoid. The caller pays for that by
// polling tenants that turn out to be idle, which is what the per-tick ceiling
// and the rotation cursor are for.
type TenantLister interface {
	ListTenantIDs(ctx context.Context) ([]string, error)
}

// ListTenantIDs returns every tenant id in `admin.tenants`, oldest first.
//
// ORDERED, and by a stable key, because the caller rotates through the result
// with a cursor it carries between ticks. An unordered scan would let the
// cursor land on a different tenant each tick for reasons unrelated to what it
// served last, which is not round robin -- it is a slower random selection with
// extra state.
//
// `created_at` rather than `tenant_id`: a new tenant then joins at the END of
// the rotation rather than in the middle of it, so admitting one does not
// reorder the tenants already being served.
//
// SUSPENDED TENANTS ARE INCLUDED, deliberately. `admin.tenants.suspended`
// exists in the schema and no Go code in the tree reads it -- so filtering on
// it here would make this the first thing in the product to act on the column,
// which is a behaviour change that belongs to whoever implements suspension
// rather than to the claim path.
func (s *PostgresStore) ListTenantIDs(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT tenant_id FROM admin.tenants ORDER BY created_at, tenant_id
	`)
	if err != nil {
		return nil, fmt.Errorf("list tenant ids: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("list tenant ids: scan: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list tenant ids: %w", err)
	}
	return ids, nil
}

// ListTenantIDs is the SQL Server form. See the PostgreSQL implementation for
// why this needs no exemption.
//
// The same reasoning holds here for a different reason: `fn_tenant_filter` is
// bound to the tenant-scoped tables, and `admin.tenants` is not one of them.
func (s *MSSQLStore) ListTenantIDs(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT CONVERT(varchar(36), tenant_id) FROM admin.tenants ORDER BY created_at, tenant_id
	`)
	if err != nil {
		return nil, fmt.Errorf("list tenant ids: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("list tenant ids: scan: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list tenant ids: %w", err)
	}
	return ids, nil
}
