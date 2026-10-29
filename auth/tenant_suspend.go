package auth

import (
	"context"
	"database/sql"
	"errors"

	"github.com/google/uuid"
)

// SuspendTenantByCreator suspends a tenant on behalf of a caller that may act only on the tenants it
// created (cleat#3552, B3a): a provisioner suspends the tenants its principal made, as the first step of
// dropping one, and no other tenant.
//
// IT CAN SUSPEND AND NEVER RESUME, and that asymmetry is the point. A suspension has more than one possible
// author: an admin suspends a tenant for non-payment, the trial-expiry sweep suspends one, cleatctl
// suspend-tenant suspends one, and a provisioner suspends one it is about to drop. admin.tenants.suspended
// is one bit and records none of them. A provisioner that could clear it could lift a suspension an
// operator made, merely because it created the tenant (cleat#3582 review, measured on both dialects). So
// the method takes no "suspended" argument: resuming is SetTenantSuspended, reached only by an admin key.
// Recording who suspended was rejected for now because every other writer of the flag would have to keep
// that record true, and a stale record would reopen the hole.
//
// It is a separate method from SetTenantSuspended rather than that method with "" meaning "any creator",
// because a sentinel that widens access is one an empty variable reaches by accident. An empty creator is
// an error here; acting on any tenant is the other method, and the caller that may do so says so by calling
// it.
//
// A tenant that does not exist, one this creator did not create, and one made offline (no creator recorded)
// all answer ErrTenantNotFound, so the response confirms nothing about a tenant the caller has no claim to.
// The creator is compared here, not in the UPDATE's WHERE clause, so it is compared exactly as
// ConfirmProvisionedTenant compares it: SQL Server's default collation is case-insensitive and
// PostgreSQL's is not, and a predicate would make the same principal string mean two things.
//
// Suspending a tenant that is already suspended succeeds and changes nothing, including when an admin made
// the suspension: it takes nothing from anyone.
//
// PostgreSQL and SQL Server only: created_by exists on neither of MySQL's tables.
func (s *TenantStore) SuspendTenantByCreator(ctx context.Context, tenantID uuid.UUID, creator string) error {
	if creator == "" {
		return errors.New("auth: a creator is required; use SetTenantSuspended to act on any tenant")
	}
	st, err := provisionStatements(s.dialect)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var createdBy string
	if err := tx.QueryRowContext(ctx, st.lockTenant, tenantID.String()).Scan(&createdBy); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrTenantNotFound
		}
		return err
	}
	if createdBy == "" || createdBy != creator {
		return ErrTenantNotFound
	}
	if _, err := tx.ExecContext(ctx, setTenantSuspendedStmt(s.dialect), true, tenantID); err != nil {
		return err
	}
	return tx.Commit()
}
