package auth

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// DefaultOrgUUID is the org cleat#1898's migration backfilled every existing
// tenant into -- the same all-zeros idiom engine.DefaultTenantUUID already
// established for the bootstrap tenant, carried into this package rather
// than imported from engine, which auth does not and should not depend on.
const DefaultOrgUUID = "00000000-0000-0000-0000-000000000000"

// CreateOrg creates a new org. Returns the org ID.
//
// admin.orgs carries identity only -- no plan, limit or usage column, by the
// owner's decision (cleat-internal/org-model-design-2026-09-18.md): billing
// systems key on org_id from outside, and cleat knows who, not what they
// bought.
//
// Lives on TenantStore rather than a separate OrgStore: it is one method,
// against the same connection and dialect a tenant is created with, and the
// two are created together in the common case (an org, then its first
// tenant) -- a second store type would duplicate NewTenantStoreForDialect's
// plumbing for no isolation this package needs.
func (s *TenantStore) CreateOrg(ctx context.Context, name string) (uuid.UUID, error) {
	stmt, err := createOrgStmt(s.dialect)
	if err != nil {
		return uuid.Nil, err
	}
	// Made here, not returned by the database: see CreateTenant.
	oid := uuid.New()
	if _, err := s.db.ExecContext(ctx, stmt, oid.String(), name); err != nil {
		return uuid.Nil, err
	}
	return oid, nil
}

// createOrgStmt is createTenantStmt's counterpart for admin.orgs. MySQL is
// refused for the same reason.
func createOrgStmt(dialect string) (string, error) {
	switch dialect {
	case DialectPostgres:
		return `INSERT INTO admin.orgs (org_id, name) VALUES ($1, $2)`, nil
	case DialectMSSQL:
		return `INSERT INTO admin.orgs (org_id, name) VALUES (@p1, @p2)`, nil
	default:
		return "", fmt.Errorf("auth: CreateOrg is not supported for %s: multi-tenancy is PostgreSQL and SQL Server only", dialect)
	}
}
