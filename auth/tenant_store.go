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
//
// It carries a dialect because its statements cannot be written once. The
// three databases disagree about all four things this file touches:
//
//	                 postgres                 mysql                 mssql
//	table            admin.tenant_api_keys    tenant_api_keys       admin.tenant_api_keys
//	placeholders     $1 $2 $3                 ? ? ?                 @p1 @p2 @p3
//	key_id           DEFAULT gen_random_uuid  no default            DEFAULT NEWID()
//	now()            now()                    NOW(6)                SYSUTCDATETIME()
//
// MySQL has no `admin` schema -- schema and database are the same namespace
// there, so `admin.tenant_api_keys` is read as a database called `admin` and
// fails with "Error 1049 (42000): Unknown database 'admin'". And MySQL's
// key_id has no default, so the INSERT has to supply one.
//
// The engine's READ path already knew all this: MySQLStore.ResolveTenantFromAPIKey
// queries an unqualified `tenant_api_keys` with `?`, and the MSSQL one carries a
// comment about the schema qualifier specifically. Only the write side was
// PostgreSQL-only -- IMPROVEMENT-PLAN issue #769's shape exactly, an unwired
// write path behind a complete read path.
type TenantStore struct {
	db      *sql.DB
	dialect string
}

// NewTenantStore creates a TenantStore for PostgreSQL.
//
// Kept for callers that know they are on PostgreSQL. Anything reachable from a
// --driver flag must use NewTenantStoreForDialect instead, or it writes
// PostgreSQL SQL to whatever it was actually given.
func NewTenantStore(db *sql.DB) *TenantStore {
	return &TenantStore{db: db, dialect: DialectPostgres}
}

// Dialect names for NewTenantStoreForDialect. They match cmd/cleat-worker's
// --driver values so a caller can pass the flag through unchanged.
const (
	DialectPostgres = "postgres"
	DialectMySQL    = "mysql"
	DialectMSSQL    = "mssql"
)

// NewTenantStoreForDialect creates a TenantStore for a named dialect.
//
// An unrecognised dialect is an error rather than a silent fallback to
// PostgreSQL: falling back is what produced the original defect's symptom, a
// message about a missing database rather than about the driver.
func NewTenantStoreForDialect(db *sql.DB, dialect string) (*TenantStore, error) {
	switch dialect {
	case DialectPostgres, DialectMySQL, DialectMSSQL:
		return &TenantStore{db: db, dialect: dialect}, nil
	default:
		return nil, fmt.Errorf("auth: unknown dialect %q (want %q, %q or %q)",
			dialect, DialectPostgres, DialectMySQL, DialectMSSQL)
	}
}

// CreateTenant creates a new tenant. Returns the tenant ID.
func (s *TenantStore) CreateTenant(ctx context.Context, name, displayName string) (uuid.UUID, error) {
	// PostgreSQL only, and it says so rather than emitting PostgreSQL SQL to
	// another database. RETURNING is the obstacle: MySQL has no equivalent and
	// SQL Server spells it OUTPUT, so this needs a different statement shape
	// per dialect rather than a different table name. It has no production
	// caller today (scripts/check-test-only-code.sh lists it), so the shape is
	// not written until something needs it -- but a silent PostgreSQL fallback
	// is exactly the failure this file was fixed for.
	if s.dialect != DialectPostgres {
		return uuid.Nil, fmt.Errorf("auth: CreateTenant is not implemented for %s", s.dialect)
	}
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
	stmt, needsKeyID := createAPIKeyStmt(s.dialect)
	args := []any{tenantID, keyHash[:], description}
	if needsKeyID {
		// MySQL's key_id column has no default, unlike PostgreSQL's
		// gen_random_uuid() and SQL Server's NEWID(), so the caller supplies
		// one. Generated here rather than added as a column DEFAULT in a
		// migration: the value is a UUID either way, and changing the schema
		// would make every existing deployment migrate for something the
		// caller can provide.
		args = append([]any{uuid.New()}, args...)
	}
	_, err := s.db.ExecContext(ctx, stmt, args...)
	return err
}

// createAPIKeyStmt returns the INSERT for a dialect, and whether that dialect
// needs key_id supplied.
//
// A function so the three statements can be asserted without a database. What
// they differ in is not cosmetic: the table is admin-qualified on two dialects
// and unqualified on the third, the placeholders differ three ways, and only
// MySQL needs key_id. Getting any one of them wrong fails at run time on a
// dialect the author probably is not running.
func createAPIKeyStmt(dialect string) (stmt string, needsKeyID bool) {
	switch dialect {
	case DialectMySQL:
		return `INSERT INTO tenant_api_keys (key_id, tenant_id, key_hash, description) VALUES (?, ?, ?, ?)`, true
	case DialectMSSQL:
		return `INSERT INTO admin.tenant_api_keys (tenant_id, key_hash, description) VALUES (@p1, @p2, @p3)`, false
	default:
		return `INSERT INTO admin.tenant_api_keys (tenant_id, key_hash, description) VALUES ($1, $2, $3)`, false
	}
}

// ResolveTenantFromAPIKey turns an API key hash into a tenant, on a connection
// that is NOT scoped to any tenant.
//
// That is the whole point of it living here rather than only on the engine's
// per-tenant stores. Resolving the key is what TELLS you which tenant's store
// to open, so a lookup that must already know the tenant is circular.
//
// migrations/postgres/031 says this outright about the table, explaining why it
// carries no RLS policy: it is read "specifically to *determine* the tenant
// from an API key, before any tenant is known ... authenticating is exactly the
// step that has not yet established a tenant. This table is correctly unscoped
// by design, not an accidental gap."
//
// On PostgreSQL and SQL Server a tenant-scoped pool reaches the table anyway --
// one database, isolation by RLS or by session context -- so running the lookup
// through such a pool was harmless and stayed invisible. On MySQL, tenant
// isolation IS a database boundary (one database per tenant), so the same code
// wrote keys to the base database and read them from the tenant's, and every
// authenticated request 401'd. See cleat#866.
func (s *TenantStore) ResolveTenantFromAPIKey(ctx context.Context, keyHash []byte) (uuid.UUID, error) {
	var tenantID uuid.UUID
	if err := s.db.QueryRowContext(ctx, resolveAPIKeyStmt(s.dialect), keyHash).Scan(&tenantID); err != nil {
		return uuid.Nil, err
	}
	return tenantID, nil
}

// resolveAPIKeyStmt mirrors createAPIKeyStmt: same table per dialect, same
// placeholder style. The two must agree about WHERE the row lives, and cleat#866
// is what happens when the write and the read disagree.
func resolveAPIKeyStmt(dialect string) string {
	switch dialect {
	case DialectMySQL:
		// No admin schema: schema and database are one namespace in MySQL, and
		// the keys live in the base database the DSN names.
		return `SELECT tenant_id FROM tenant_api_keys WHERE key_hash = ? AND revoked_at IS NULL`
	case DialectMSSQL:
		return `SELECT tenant_id FROM admin.tenant_api_keys WHERE key_hash = @p1 AND revoked_at IS NULL`
	default:
		return `SELECT tenant_id FROM admin.tenant_api_keys WHERE key_hash = $1 AND revoked_at IS NULL`
	}
}

// RevokeAPIKey revokes an API key.
func (s *TenantStore) RevokeAPIKey(ctx context.Context, keyID uuid.UUID) error {
	// Same reasoning as CreateTenant: no production caller, and now() /
	// NOW(6) / SYSUTCDATETIME() differ alongside the placeholders. Refuses
	// rather than guessing.
	if s.dialect != DialectPostgres {
		return fmt.Errorf("auth: RevokeAPIKey is not implemented for %s", s.dialect)
	}
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
