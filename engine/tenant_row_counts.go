package engine

import (
	"context"
	"fmt"

	"github.com/cleat-team/cleat/internal/tenantdrop"
)

// TenantRowCounter counts a tenant's rows with the tenant's own scope applied (cleat#3552, B3b-1b).
//
// It exists for the drop plan, and it is a store method rather than a query on a shared connection
// because both row-level-security dialects make a COUNT depend on the session: PostgreSQL's policy
// reads cleat.tenant_id and SQL Server's reads SESSION_CONTEXT('tenant_id'), and a count issued
// without it returns 0 for every table. A plan that read zero for a tenant with data would say there
// is nothing to drop, which is the worst thing it can say. So the counts run inside a transaction
// that sets the key first, the way every other tenant-scoped read in this package does.
//
// Each statement takes the tenant id as its only parameter and returns one number.
type TenantRowCounter interface {
	CountTenantRows(ctx context.Context, tables []tenantdrop.Table) ([]int64, error)
}

// CountTenantRows implements TenantRowCounter. The statements are the caller's (tenantdrop's), not
// built here, so this method cannot widen what is counted.
func (s *PostgresStore) CountTenantRows(ctx context.Context, tables []tenantdrop.Table) ([]int64, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return nil, fmt.Errorf("count tenant rows: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	counts := make([]int64, len(tables))
	for i, t := range tables {
		if err := tx.QueryRowContext(ctx, t.Query, s.tenantID).Scan(&counts[i]); err != nil {
			return nil, fmt.Errorf("count %s: %w", t.Label, err)
		}
	}
	return counts, nil
}

// CountTenantRows implements TenantRowCounter for SQL Server.
func (s *MSSQLStore) CountTenantRows(ctx context.Context, tables []tenantdrop.Table) ([]int64, error) {
	tx, err := s.beginTxWithContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("count tenant rows: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	counts := make([]int64, len(tables))
	for i, t := range tables {
		if err := tx.QueryRowContext(ctx, t.Query, s.tenantID).Scan(&counts[i]); err != nil {
			return nil, fmt.Errorf("count %s: %w", t.Label, err)
		}
	}
	return counts, nil
}

// DerivedTenantTables lists every table in this database that carries a tenant_id column, as count
// statements. SQL Server derives its list from the catalogue instead of keeping one, because
// migrations/mssql sweeps every such table and a hand-kept list has drifted under that (see
// tenantdrop.MSSQLTables). It needs no tenant scope: the catalogue is not tenant data.
func (s *MSSQLStore) DerivedTenantTables(ctx context.Context) ([]tenantdrop.Table, error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("derive tenant tables: %w", err)
	}
	defer func() { _ = conn.Close() }()
	return tenantdrop.MSSQLTables(ctx, conn)
}
