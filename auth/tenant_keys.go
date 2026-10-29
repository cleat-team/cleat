package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// A tenant's own API keys: mint, list, revoke. cleat#3552, B2c.
//
// Rotation and revocation belong to the tenant, not to whoever created it (design on #3552, guard
// rule 2). Rotate is mint, deploy, revoke the old; many keys per tenant were always legal, and what
// was missing was the API. These methods are scoped to ONE tenant by construction: every statement
// carries tenant_id, so a key_id that belongs to another tenant is indistinguishable from one that
// does not exist, and no caller can reach across.
//
// All three dialects, unlike RevokeAPIKey and RevokeAPIKeyByHash, which refuse all but PostgreSQL
// because nothing called them. This is the caller. MySQL is single-tenant but a single-tenant
// deployment rotates keys too, and its key table has the same columns bar provisional.

// MaxLiveTenantKeys bounds the keys a tenant may hold at once, so a leaked key cannot be used to
// fill the table. Generous: rotation needs two, and an app with a key per replica needs few more.
const MaxLiveTenantKeys = 25

// maxListedTenantKeys bounds one listing. Revoked and expired keys stay in the table as history.
const maxListedTenantKeys = 200

var (
	// ErrKeyNotFound: the key id is not one of this tenant's. The same error for a key that belongs to
	// another tenant, so the answer is no oracle for which key ids exist.
	ErrKeyNotFound = errors.New("auth: api key not found")
	// ErrLastKey: revoking the tenant's last live key would lock it out. Rotation mints the new key
	// first; a caller that really means it passes force.
	ErrLastKey = errors.New("auth: that is the tenant's last live api key")
	// ErrKeyLimit: the tenant already holds MaxLiveTenantKeys live keys.
	ErrKeyLimit = errors.New("auth: the tenant already holds the maximum number of live api keys")
)

// TenantKey is a key's metadata. It never carries the secret or the hash: the secret exists once, in
// the response to the mint, and the database holds only the hash.
type TenantKey struct {
	KeyID       uuid.UUID
	Description string
	CreatedAt   time.Time
	ExpiresAt   *time.Time
	DisabledAt  *time.Time
	// Provisional: minted by the provisioning handshake and not yet confirmed. Always false on MySQL,
	// which cannot provision.
	Provisional bool
}

// Status is "revoked", "expired", "provisional" or "active". Computed against now, so it is a label
// for a listing, not a statement the database enforces; authentication decides by its own clock.
func (k TenantKey) Status(now time.Time) string {
	switch {
	case k.DisabledAt != nil:
		return "revoked"
	case k.ExpiresAt != nil && !k.ExpiresAt.After(now):
		return "expired"
	case k.Provisional:
		return "provisional"
	default:
		return "active"
	}
}

type keyStmts struct {
	lockTenant, liveCount, insertKey, list, keyRow, liveOthers, revoke string
}

func keyStatements(dialect string) (keyStmts, error) {
	switch dialect {
	case DialectPostgres:
		return keyStmts{
			lockTenant: `SELECT 1 FROM admin.tenants WHERE tenant_id = $1 FOR UPDATE`,
			liveCount:  `SELECT count(*) FROM admin.tenant_api_keys WHERE tenant_id = $1 AND disabled_at IS NULL AND (expires_at IS NULL OR expires_at > now())`,
			insertKey:  `INSERT INTO admin.tenant_api_keys (key_id, tenant_id, key_hash, description, expires_at) VALUES ($1, $2, $3, $4, $5)`,
			list:       `SELECT key_id::text, description, created_at, expires_at, disabled_at, provisional FROM admin.tenant_api_keys WHERE tenant_id = $1 ORDER BY created_at DESC, key_id LIMIT 200`,
			keyRow:     `SELECT disabled_at FROM admin.tenant_api_keys WHERE tenant_id = $1 AND key_id = $2`,
			liveOthers: `SELECT count(*) FROM admin.tenant_api_keys WHERE tenant_id = $1 AND key_id <> $2 AND disabled_at IS NULL AND (expires_at IS NULL OR expires_at > now())`,
			revoke:     `UPDATE admin.tenant_api_keys SET disabled_at = now(), updated_at = now() WHERE tenant_id = $1 AND key_id = $2 AND disabled_at IS NULL`,
		}, nil
	case DialectMSSQL:
		return keyStmts{
			lockTenant: `SELECT 1 FROM admin.tenants WITH (UPDLOCK, ROWLOCK) WHERE tenant_id = @p1`,
			liveCount:  `SELECT COUNT(*) FROM admin.tenant_api_keys WHERE tenant_id = @p1 AND disabled_at IS NULL AND (expires_at IS NULL OR expires_at > SYSUTCDATETIME())`,
			insertKey:  `INSERT INTO admin.tenant_api_keys (key_id, tenant_id, key_hash, description, expires_at) VALUES (@p1, @p2, @p3, @p4, @p5)`,
			list:       `SELECT TOP (200) LOWER(CONVERT(NVARCHAR(36), key_id)), description, created_at, expires_at, disabled_at, provisional FROM admin.tenant_api_keys WHERE tenant_id = @p1 ORDER BY created_at DESC, key_id`,
			keyRow:     `SELECT disabled_at FROM admin.tenant_api_keys WHERE tenant_id = @p1 AND key_id = @p2`,
			liveOthers: `SELECT COUNT(*) FROM admin.tenant_api_keys WHERE tenant_id = @p1 AND key_id <> @p2 AND disabled_at IS NULL AND (expires_at IS NULL OR expires_at > SYSUTCDATETIME())`,
			revoke:     `UPDATE admin.tenant_api_keys SET disabled_at = SYSUTCDATETIME(), updated_at = SYSUTCDATETIME() WHERE tenant_id = @p1 AND key_id = @p2 AND disabled_at IS NULL`,
		}, nil
	case DialectMySQL:
		return keyStmts{
			lockTenant: `SELECT 1 FROM tenants WHERE tenant_id = ? FOR UPDATE`,
			liveCount:  `SELECT count(*) FROM tenant_api_keys WHERE tenant_id = ? AND disabled_at IS NULL AND (expires_at IS NULL OR expires_at > NOW(6))`,
			insertKey:  `INSERT INTO tenant_api_keys (key_id, tenant_id, key_hash, description, expires_at) VALUES (?, ?, ?, ?, ?)`,
			list:       `SELECT key_id, description, created_at, expires_at, disabled_at, 0 FROM tenant_api_keys WHERE tenant_id = ? ORDER BY created_at DESC, key_id LIMIT 200`,
			keyRow:     `SELECT disabled_at FROM tenant_api_keys WHERE tenant_id = ? AND key_id = ?`,
			liveOthers: `SELECT count(*) FROM tenant_api_keys WHERE tenant_id = ? AND key_id <> ? AND disabled_at IS NULL AND (expires_at IS NULL OR expires_at > NOW(6))`,
			revoke:     `UPDATE tenant_api_keys SET disabled_at = NOW(6), updated_at = NOW(6) WHERE tenant_id = ? AND key_id = ? AND disabled_at IS NULL`,
		}, nil
	default:
		return keyStmts{}, fmt.Errorf("auth: unknown dialect %q", dialect)
	}
}

// MintKey creates a permanent key for the tenant and returns its metadata and its secret, which exists
// nowhere else. expiresAt is optional; a key with none does not expire.
//
// The tenant's row is locked first, so the live-key count and the insert are one decision: two mints
// racing at the cap cannot both succeed.
func (s *TenantStore) MintKey(ctx context.Context, tenantID uuid.UUID, description string, expiresAt *time.Time) (TenantKey, string, error) {
	st, err := keyStatements(s.dialect)
	if err != nil {
		return TenantKey{}, "", err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return TenantKey{}, "", err
	}
	defer func() { _ = tx.Rollback() }()
	if err := lockTenantRow(ctx, tx, st, tenantID); err != nil {
		return TenantKey{}, "", err
	}
	var live int
	if err := tx.QueryRowContext(ctx, st.liveCount, tenantID.String()).Scan(&live); err != nil {
		return TenantKey{}, "", err
	}
	if live >= MaxLiveTenantKeys {
		return TenantKey{}, "", ErrKeyLimit
	}
	secret := GenerateAPIKey()
	keyID := uuid.New()
	var exp any
	if expiresAt != nil {
		exp = expiresAt.UTC()
	}
	if _, err := tx.ExecContext(ctx, st.insertKey, keyID.String(), tenantID.String(), sha256Hash(secret), description, exp); err != nil {
		return TenantKey{}, "", err
	}
	if err := tx.Commit(); err != nil {
		return TenantKey{}, "", err
	}
	k := TenantKey{KeyID: keyID, Description: description, CreatedAt: time.Now().UTC()}
	if expiresAt != nil {
		e := expiresAt.UTC()
		k.ExpiresAt = &e
	}
	return k, secret, nil
}

func lockTenantRow(ctx context.Context, tx *sql.Tx, st keyStmts, tenantID uuid.UUID) error {
	var one int
	if err := tx.QueryRowContext(ctx, st.lockTenant, tenantID.String()).Scan(&one); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrTenantNotFound
		}
		return err
	}
	return nil
}

// ListKeys returns the tenant's keys, newest first, revoked and expired ones included as history. It
// never selects the hash.
func (s *TenantStore) ListKeys(ctx context.Context, tenantID uuid.UUID) ([]TenantKey, error) {
	st, err := keyStatements(s.dialect)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, st.list, tenantID.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TenantKey
	for rows.Next() {
		var (
			raw, desc   string
			created     time.Time
			exp, dis    sql.NullTime
			provisional bool
		)
		if err := rows.Scan(&raw, &desc, &created, &exp, &dis, &provisional); err != nil {
			return nil, err
		}
		id, err := uuid.Parse(raw)
		if err != nil {
			return nil, fmt.Errorf("auth: list keys: parse key id: %w", err)
		}
		k := TenantKey{KeyID: id, Description: desc, CreatedAt: created.UTC(), Provisional: provisional}
		if exp.Valid {
			t := exp.Time.UTC()
			k.ExpiresAt = &t
		}
		if dis.Valid {
			t := dis.Time.UTC()
			k.DisabledAt = &t
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// RevokeResult says what a revoke did.
type RevokeResult struct {
	// AlreadyRevoked: the key had been revoked before this call, which is not an error.
	AlreadyRevoked bool
}

// RevokeKey revokes one of the tenant's keys. A key id that is not this tenant's is ErrKeyNotFound.
// Revoking the last live key is ErrLastKey unless force. Revoking a key that is already revoked, or
// that has already expired, never counts as removing a live key.
//
// The tenant's row is locked, so two revokes of the last two keys cannot each see the other as still
// live and both succeed.
func (s *TenantStore) RevokeKey(ctx context.Context, tenantID, keyID uuid.UUID, force bool) (RevokeResult, error) {
	st, err := keyStatements(s.dialect)
	if err != nil {
		return RevokeResult{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RevokeResult{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := lockTenantRow(ctx, tx, st, tenantID); err != nil {
		return RevokeResult{}, err
	}
	var disabled sql.NullTime
	if err := tx.QueryRowContext(ctx, st.keyRow, tenantID.String(), keyID.String()).Scan(&disabled); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return RevokeResult{}, ErrKeyNotFound
		}
		return RevokeResult{}, err
	}
	if disabled.Valid {
		return RevokeResult{AlreadyRevoked: true}, nil
	}
	if !force {
		var others int
		if err := tx.QueryRowContext(ctx, st.liveOthers, tenantID.String(), keyID.String()).Scan(&others); err != nil {
			return RevokeResult{}, err
		}
		if others == 0 {
			// The target may itself be expired, in which case it is not a live key and revoking it removes
			// nothing. That case is rare enough, and a lock-out costly enough, that it is refused too:
			// the caller passes force to say it means it.
			return RevokeResult{}, ErrLastKey
		}
	}
	if _, err := tx.ExecContext(ctx, st.revoke, tenantID.String(), keyID.String()); err != nil {
		return RevokeResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return RevokeResult{}, err
	}
	return RevokeResult{}, nil
}
