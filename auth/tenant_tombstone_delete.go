package auth

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// ErrTombstoneNotFound means there is no tombstone for the id: it was never dropped, its tombstone was
// already deleted, or the id belongs to a live tenant. Those are one answer on purpose, so a caller learns
// nothing about a tenant from it.
var ErrTombstoneNotFound = errors.New("auth: no tombstone for that tenant id")

// deleteTombstoneStatement removes one tombstone and returns the row it removed, in the column order
// scanTombstone reads. One statement and not a read followed by a delete, so the row the caller is shown
// is the row that was removed even if a second delete is racing: exactly one of them gets it.
//
// SQL Server uses OUTPUT DELETED, which is allowed here because admin.tenant_tombstones has no trigger
// (007_a_dropped_tenant_leaves_a_tombstone.sql); a trigger on it would make this statement fail until it
// OUTPUTs INTO a table variable instead. dropped_tenant_id is the column name on both dialects, for the
// reason that migration gives.
func deleteTombstoneStatement(dialect string) (string, error) {
	switch dialect {
	case DialectPostgres:
		return `DELETE FROM admin.tenant_tombstones WHERE dropped_tenant_id = $1
			RETURNING name, org_id::text, COALESCE(created_by, ''), dropped_by, dropped_at, audit_chain_seq, audit_chain_hash`, nil
	case DialectMSSQL:
		return `DELETE FROM admin.tenant_tombstones
			OUTPUT DELETED.name, LOWER(CONVERT(NVARCHAR(36), DELETED.org_id)), ISNULL(DELETED.created_by, N''),
			       DELETED.dropped_by, DELETED.dropped_at, DELETED.audit_chain_seq, DELETED.audit_chain_hash
			WHERE dropped_tenant_id = @p1`, nil
	default:
		return "", fmt.Errorf("auth: tombstones exist for PostgreSQL and SQL Server only, not %s", dialect)
	}
}

// DeleteTombstone removes a dropped tenant's tombstone, which is what allows its id to be provisioned again
// (cleat#3552, B3b-2b), and returns what the tombstone held. ErrTombstoneNotFound when there is none.
//
// IT DISCARDS THE ONLY RECORD OF WHERE THE DROPPED TENANT'S AUDIT CHAIN ENDED. The chain itself was deleted
// with the tenant, so after this nothing says where it stopped, and a copy of it taken earlier can no longer
// be checked against its end. The returned Tombstone is the last place those facts exist: the caller must put
// them somewhere that outlives this call (the HTTP route returns them in its reply and logs them).
//
// It checks nothing about WHO may do this. Deleting a tombstone is an admin-only act, and that is decided
// by the scope gate and by the handler; this method is the mechanism, like DropTenant's OnlyCreator is not.
func (s *TenantStore) DeleteTombstone(ctx context.Context, tenantID uuid.UUID) (Tombstone, error) {
	stmt, err := deleteTombstoneStatement(s.dialect)
	if err != nil {
		return Tombstone{}, err
	}
	if tenantID == uuid.Nil {
		return Tombstone{}, ErrTombstoneNotFound
	}
	t, found, err := scanTombstone(s.db.QueryRowContext(ctx, stmt, tenantID.String()).Scan, tenantID)
	if err != nil {
		return Tombstone{}, fmt.Errorf("auth: delete the tombstone: %w", err)
	}
	if !found {
		return Tombstone{}, ErrTombstoneNotFound
	}
	return t, nil
}
