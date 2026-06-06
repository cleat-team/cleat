package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"fmt"

	"github.com/cleat-team/cleat/plugin"
	"github.com/google/uuid"
)

var randRead = rand.Read

// TenantStore provides CRUD operations for tenants and their API keys.
type TenantStore struct {
	db      *sql.DB
	dialect plugin.Dialect
}

// NewTenantStore creates a new TenantStore for the given database dialect.
func NewTenantStore(db *sql.DB, dialect plugin.Dialect) *TenantStore {
	return &TenantStore{db: db, dialect: dialect}
}

// tableName returns the schema-qualified table name for the current dialect.
// Only PostgreSQL uses the admin. schema prefix.
func (s *TenantStore) tableName(table string) string {
	if s.dialect == plugin.DialectPostgres {
		return "admin." + table
	}
	return table
}

// CreateTenant creates a new tenant. Returns the tenant ID.
func (s *TenantStore) CreateTenant(ctx context.Context, name, displayName string) (uuid.UUID, error) {
	tid := uuid.New()
	query := plugin.Rebind(fmt.Sprintf(
		`INSERT INTO %s (tenant_id, name, display_name) VALUES ($1, $2, $3)`,
		s.tableName("tenants"),
	), s.dialect)
	_, err := s.db.ExecContext(ctx, query, tid, name, displayName)
	if err != nil {
		return uuid.Nil, err
	}
	return tid, nil
}

// CreateAPIKey creates an API key for a tenant. Returns the plaintext key
// (only returned once — caller must store it). The database stores sha256(key).
func (s *TenantStore) CreateAPIKey(ctx context.Context, tenantID uuid.UUID, description, rawKey string) error {
	keyHash := sha256.Sum256([]byte(rawKey))
	query := plugin.Rebind(fmt.Sprintf(
		`INSERT INTO %s (tenant_id, key_hash, description) VALUES ($1, $2, $3)`,
		s.tableName("tenant_api_keys"),
	), s.dialect)
	_, err := s.db.ExecContext(ctx, query, tenantID, keyHash[:], description)
	return err
}

// RevokeAPIKey revokes an API key.
func (s *TenantStore) RevokeAPIKey(ctx context.Context, keyID uuid.UUID) error {
	query := plugin.Rebind(fmt.Sprintf(
		`UPDATE %s SET revoked_at = now() WHERE key_id = $1 AND revoked_at IS NULL`,
		s.tableName("tenant_api_keys"),
	), s.dialect)
	_, err := s.db.ExecContext(ctx, query, keyID)
	return err
}

// GenerateAPIKey generates a random API key string.
func GenerateAPIKey() string {
	// cleat_sk_ prefix for easy identification in logs
	b := make([]byte, 32)
	_, _ = randRead(b)
	return fmt.Sprintf("cleat_sk_%x", b)
}
