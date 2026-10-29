package auth

import (
	"context"
	"database/sql"
	"errors"

	"github.com/google/uuid"
)

// SetTenantSuspendedByCreator is SetTenantSuspended for a caller that may act only on the tenants it
// created (cleat#3552, B3a): a provisioner suspends and resumes the tenants its principal made, as the
// first step of dropping one, and no other tenant.
//
// It is a separate method rather than SetTenantSuspended with "" meaning "any creator", because a
// sentinel that widens access is one an empty variable reaches by accident. An empty creator is an
// error here; acting on any tenant is the other method, and the caller that may do so says so by
// calling it.
//
// A tenant that does not exist, one this creator did not create, and one made offline (no creator
// recorded) all answer ErrTenantNotFound, so the response confirms nothing about a tenant the caller
// has no claim to. The creator is compared here, not in the UPDATE's WHERE clause, so it is compared
// exactly as ConfirmProvisionedTenant compares it: SQL Server's default collation is case-insensitive
// and PostgreSQL's is not, and a predicate would make the same principal string mean two things.
//
// PostgreSQL and SQL Server only: created_by exists on neither of MySQL's tables.
func (s *TenantStore) SetTenantSuspendedByCreator(ctx context.Context, tenantID uuid.UUID, suspended bool, creator string) error {
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
	if _, err := tx.ExecContext(ctx, setTenantSuspendedStmt(s.dialect), suspended, tenantID); err != nil {
		return err
	}
	return tx.Commit()
}
