package auth

// cleat#3578: an operator mints a recovery key for a tenant that has no working key.
//
// A tenant can revoke its own last key (DELETE /api/keys/{key_id}?force=true), and after that nothing it
// holds reaches /api/keys; the only way back was `cleat-worker --generate-api-key` against the database,
// which needs a shell on a worker. This is the same act over HTTP, for an operator credential, and the
// design (accepted on the issue) is built so that it is unusable against a tenant that can still act for
// itself:
//
//   - THE GUARD IS ONE FACT, READ IN THE TRANSACTION THAT MINTS: the tenant has no live key. It is read
//     under the tenant's row lock, the lock MintKey and the provisioning handshake take, so a tenant minting
//     or revoking at the same moment and this are one serial decision and not two reads of a count.
//   - THE KEY IS PROVISIONAL (ProvisionalKeyTTL, 15 minutes). A permanent key whose reply was lost would be
//     live and held by nobody, and the guard would then refuse every retry. A provisional one lapses, the
//     tenant is keyless again, and the operator can call again. It also means at most one is outstanding.
//   - IT ADDS ONE AND REVOKES NOTHING. With no live key there is nothing to revoke; disabled and expired
//     rows stay as history.
//   - A SUSPENDED TENANT IS REFUSED, so this is not a way round a suspension: resume first.
//
// A tenant that does not exist, the default tenant and a dropped one (its row is gone with it) are all
// ErrTenantNotFound. PostgreSQL and SQL Server only, like the rest of tenant provisioning.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// RecoveryKey is a minted recovery key: its id, the secret (which exists nowhere else) and how long it
// authenticates before it lapses.
type RecoveryKey struct {
	KeyID    uuid.UUID
	Secret   string
	ValidFor time.Duration
}

// Reasons a recovery mint is refused, carried by RecoveryRefusedError.
const (
	// RecoveryRefusedLiveKeys: the tenant has a working key, so it does not need one. Includes a
	// recovery key minted a moment ago that has not lapsed.
	RecoveryRefusedLiveKeys = "tenant_has_live_keys"
	// RecoveryRefusedSuspended: an operator suspended the tenant; resume it first.
	RecoveryRefusedSuspended = "tenant_suspended"
)

// RecoveryRefusedError is a recovery mint refused because of the tenant's state. Nothing was written.
type RecoveryRefusedError struct {
	Reason string
	// LiveKeys is how many live keys the tenant holds, set for RecoveryRefusedLiveKeys.
	LiveKeys int
}

func (e *RecoveryRefusedError) Error() string {
	switch e.Reason {
	case RecoveryRefusedSuspended:
		return "auth: the tenant is suspended; resume it before minting a recovery key"
	default:
		return fmt.Sprintf("auth: the tenant has %d live api key(s), so it is not locked out", e.LiveKeys)
	}
}

func tenantSuspendedStmt(dialect string) string {
	if dialect == DialectMSSQL {
		return `SELECT suspended FROM admin.tenants WHERE tenant_id = @p1`
	}
	return `SELECT suspended FROM admin.tenants WHERE tenant_id = $1`
}

// MintRecoveryKey mints a provisional key for a tenant that has no live key, and returns its secret, which
// exists nowhere else. It is refused with a *RecoveryRefusedError, writing nothing, when the tenant is
// suspended or holds any live key (permanent, or provisional and not yet lapsed).
func (s *TenantStore) MintRecoveryKey(ctx context.Context, tenantID uuid.UUID, description string) (RecoveryKey, error) {
	if tenantID == uuid.Nil {
		return RecoveryKey{}, ErrTenantNotFound
	}
	st, err := provisionStatements(s.dialect)
	if err != nil {
		return RecoveryKey{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RecoveryKey{}, err
	}
	defer func() { _ = tx.Rollback() }()

	// The tenant's row, locked. Everything below is read and written under it.
	var createdBy string
	if err := tx.QueryRowContext(ctx, st.lockTenant, tenantID.String()).Scan(&createdBy); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return RecoveryKey{}, ErrTenantNotFound
		}
		return RecoveryKey{}, err
	}
	var suspended bool
	if err := tx.QueryRowContext(ctx, tenantSuspendedStmt(s.dialect), tenantID.String()).Scan(&suspended); err != nil {
		return RecoveryKey{}, err
	}
	if suspended {
		return RecoveryKey{}, &RecoveryRefusedError{Reason: RecoveryRefusedSuspended}
	}
	// st.liveKeys counts the unexpired, undisabled keys split into confirmed and provisional; a provisional
	// key that has not lapsed authenticates, so it is live.
	var confirmed, provisional int64
	if err := tx.QueryRowContext(ctx, st.liveKeys, tenantID.String()).Scan(&confirmed, &provisional); err != nil {
		return RecoveryKey{}, err
	}
	if live := confirmed + provisional; live > 0 {
		return RecoveryKey{}, &RecoveryRefusedError{Reason: RecoveryRefusedLiveKeys, LiveKeys: int(live)}
	}

	secret := GenerateAPIKey()
	keyID := uuid.New()
	if err := insertProvisionalKey(ctx, tx, st, keyID, tenantID, secret, description); err != nil {
		return RecoveryKey{}, err
	}
	if err := tx.Commit(); err != nil {
		return RecoveryKey{}, err
	}
	return RecoveryKey{KeyID: keyID, Secret: secret, ValidFor: ProvisionalKeyTTL}, nil
}
