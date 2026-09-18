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
	// PostgreSQL only, for the same reason CreateTenant is: RETURNING has no
	// MySQL equivalent and SQL Server spells it OUTPUT, so this needs a
	// different statement shape per dialect rather than a different table
	// name, and that shape is not written until something needs it on
	// another dialect.
	if s.dialect != DialectPostgres {
		return uuid.Nil, fmt.Errorf("auth: CreateOrg is not implemented for %s", s.dialect)
	}
	var oid uuid.UUID
	err := s.db.QueryRowContext(ctx,
		`INSERT INTO admin.orgs (name) VALUES ($1) RETURNING org_id`,
		name).Scan(&oid)
	return oid, err
}
