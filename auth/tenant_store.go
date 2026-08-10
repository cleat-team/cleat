package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"fmt"

	"github.com/google/uuid"
)

var randRead = rand.Read

// TenantStore provides CRUD operations for tenants and their API keys.
type TenantStore struct {
	db *sql.DB
}

// NewTenantStore creates a new TenantStore.
func NewTenantStore(db *sql.DB) *TenantStore {
	return &TenantStore{db: db}
}

// CreateTenant creates a new tenant. Returns the tenant ID.
func (s *TenantStore) CreateTenant(ctx context.Context, name, displayName string) (uuid.UUID, error) {
	var tid uuid.UUID
	err := s.db.QueryRowContext(ctx,
		`INSERT INTO admin.tenants (name, display_name) VALUES ($1, $2) RETURNING tenant_id`,
		name, displayName).Scan(&tid)
	return tid, err
}

// CreateAPIKey creates an API key for a tenant. Returns the plaintext key
// (only returned once — caller must store it). The database stores sha256(key).
//
// rawKey must be a high-entropy, randomly generated value — in practice
// always the output of GenerateAPIKey below (32 bytes from crypto/rand).
// SHA-256 is used, not a slow password KDF, because the only two
// production callers (cmd/cleat-worker/main.go) always pass a
// GenerateAPIKey() value: a 256-bit random token has no meaningful offline
// brute-force surface even hashed with a fast function, unlike a
// low-entropy user-chosen password. The hash exists so the plaintext key
// is never persisted and so lookups can use a DB equality index
// (ResolveTenantFromAPIKey does `WHERE key_hash = $1`); it is not a
// password-verification barrier. CodeQL go/weak-sensitive-data-hashing
// alert #12 flags this call; dismissed with that reasoning. If a caller
// is ever added that lets a tenant supply their own key text, that
// precondition breaks and this needs to move to bcrypt/scrypt/argon2
// (golang.org/x/crypto is already a dependency) with a versioned-hash
// migration path for existing rows.
func (s *TenantStore) CreateAPIKey(ctx context.Context, tenantID uuid.UUID, description, rawKey string) error {
	keyHash := sha256.Sum256([]byte(rawKey))
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO admin.tenant_api_keys (tenant_id, key_hash, description) VALUES ($1, $2, $3)`,
		tenantID, keyHash[:], description)
	return err
}

// RevokeAPIKey revokes an API key.
func (s *TenantStore) RevokeAPIKey(ctx context.Context, keyID uuid.UUID) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE admin.tenant_api_keys SET revoked_at = now() WHERE key_id = $1 AND revoked_at IS NULL`, keyID)
	return err
}

// GenerateAPIKey generates a random API key string.
func GenerateAPIKey() string {
	// cleat_sk_ prefix for easy identification in logs
	b := make([]byte, 32)
	_, _ = randRead(b)
	return fmt.Sprintf("cleat_sk_%x", b)
}
