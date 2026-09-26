package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"time"

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
//	placeholders     $1 $2 …                  ? ? …                 @p1 @p2 …
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

// CreateTenant creates a new tenant under the given org. Returns the tenant ID.
//
// orgID IS REQUIRED, and immutable once set -- enforced by a database
// trigger (cleat#1898's migration), not just this signature. Every tenant
// records its org at creation; the alternative is a backfill over tenants
// whose org nobody recorded, which is exactly what this avoids. See
// cleat-internal/org-model-design-2026-09-18.md for the model:
// a microservice maps to a tenant, an org groups a customer's tenants and
// is the billing and ownership entity.
func (s *TenantStore) CreateTenant(ctx context.Context, name, displayName string, orgID uuid.UUID) (uuid.UUID, error) {
	// PostgreSQL only, and it says so rather than emitting PostgreSQL SQL to
	// another database. RETURNING is the obstacle: MySQL has no equivalent and
	// SQL Server spells it OUTPUT, so this needs a different statement shape
	// per dialect rather than a different table name. It DOES have a
	// production caller now -- cmd/cleat-worker's --create-tenant flag,
	// cleat#1114 -- so a silent PostgreSQL fallback here would not be an
	// inert gap, it would be the exact failure this file was fixed for,
	// reaching a real deployment.
	if s.dialect != DialectPostgres {
		return uuid.Nil, fmt.Errorf("auth: CreateTenant is not implemented for %s", s.dialect)
	}
	var tid uuid.UUID
	err := s.db.QueryRowContext(ctx,
		`INSERT INTO admin.tenants (name, display_name, org_id) VALUES ($1, $2, $3) RETURNING tenant_id`,
		name, displayName, orgID).Scan(&tid)
	return tid, err
}

// CreateAPIKey creates an API key for a tenant, with no expiry and no OAuth
// identity. Returns the plaintext key (only returned once — caller must store
// it). The database stores sha256(key).
//
// This is the permanent-service-key path. OAuth login mints through
// CreateOAuthAPIKey below, which cannot produce a key without an expiry.
//
// rawKey must be a high-entropy, randomly generated value — in practice
// always the output of GenerateAPIKey below (32 bytes from crypto/rand).
// SHA-256 is used, not a slow password KDF, because every production caller
// passes a GenerateAPIKey() value: a 256-bit random token has no meaningful
// offline brute-force surface even hashed with a fast function, unlike a
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
	return s.createAPIKey(ctx, tenantID, description, rawKey, nil, nil)
}

// CreateOAuthAPIKey creates an API key that expires, and records the OAuth
// identity it was minted for. cleat#2340.
//
// expiresAt IS A VALUE, NOT A *time.Time, and that is why this is a separate
// entry point rather than two more parameters on CreateAPIKey. The design's
// rule is that an OAuth-minted key must never be permanent: an IdP that omits
// expires_in must not produce a forever credential by omission, and the sweep
// (design item 7) selects on `expires_at < now()`, so a key without one is
// never collected. A pointer parameter would leave "no expiry" expressible at
// this call site; a value makes it unrepresentable, so the invariant is
// enforced by the signature rather than by every future caller remembering it.
//
// oauthIdentity is "<provider>:<identity>" (migrations/postgres/105,
// migrations/mysql/104). A key that carries one IS an OAuth-minted key --
// design item 4 revokes on it when an identity leaves the allowlist, and item
// 7's sweep selects on it -- so there is no second flag that could disagree.
//
// Both entry points share one INSERT, below, so they cannot drift on the
// per-dialect spellings that createAPIKeyStmt's own comment is about.
func (s *TenantStore) CreateOAuthAPIKey(ctx context.Context, tenantID uuid.UUID, description, rawKey string, expiresAt time.Time, oauthIdentity string) error {
	return s.createAPIKey(ctx, tenantID, description, rawKey, &expiresAt, &oauthIdentity)
}

// createAPIKey is the single INSERT behind both entry points. expiresAt and
// oauthIdentity are nil for a permanent, non-OAuth key; the drivers write NULL
// for a nil pointer, which is what those columns hold for every row created
// before they existed.
func (s *TenantStore) createAPIKey(ctx context.Context, tenantID uuid.UUID, description, rawKey string, expiresAt *time.Time, oauthIdentity *string) error {
	keyHash := sha256.Sum256([]byte(rawKey))
	stmt, needsKeyID := createAPIKeyStmt(s.dialect)
	args := []any{tenantID, keyHash[:], description, expiresAt, oauthIdentity}
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
		return `INSERT INTO tenant_api_keys (key_id, tenant_id, key_hash, description, expires_at, oauth_identity) VALUES (?, ?, ?, ?, ?, ?)`, true
	case DialectMSSQL:
		return `INSERT INTO admin.tenant_api_keys (tenant_id, key_hash, description, expires_at, oauth_identity) VALUES (@p1, @p2, @p3, @p4, @p5)`, false
	default:
		return `INSERT INTO admin.tenant_api_keys (tenant_id, key_hash, description, expires_at, oauth_identity) VALUES ($1, $2, $3, $4, $5)`, false
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
	// SQL Server returns UNIQUEIDENTIFIER in a byte order the uuid package does
	// not scan directly, so the column is converted in the projection and parsed
	// here -- the same shape as MSSQLStore.ResolveTenantFromAPIKey
	// (engine/mssql_deployment.go:122). engine's TestMSSQLUUIDColumnsAreConverted
	// InProjections enforces this across the tree and caught the first version
	// of this function, which selected the raw column.
	if s.dialect == DialectMSSQL {
		var raw string
		if err := s.db.QueryRowContext(ctx, resolveAPIKeyStmt(s.dialect), keyHash).Scan(&raw); err != nil {
			return uuid.Nil, err
		}
		tenantID, err := uuid.Parse(raw)
		if err != nil {
			return uuid.Nil, fmt.Errorf("resolve tenant from api key: parse uuid: %w", err)
		}
		return tenantID, nil
	}

	var tenantID uuid.UUID
	if err := s.db.QueryRowContext(ctx, resolveAPIKeyStmt(s.dialect), keyHash).Scan(&tenantID); err != nil {
		return uuid.Nil, err
	}
	return tenantID, nil
}

// resolveAPIKeyStmt mirrors createAPIKeyStmt: same table per dialect, same
// placeholder style. The two must agree about WHERE the row lives, and cleat#866
// is what happens when the write and the read disagree.
//
// cleat#2352: each statement also excludes an expired key, the same way it
// already excludes a disabled one. expires_at IS NULL means "no expiry" (the
// default for every key created before migration 105/104/105 landed, and for
// any manually-provisioned service key since), so that half of the OR is what
// keeps every existing key authenticating exactly as before this change.
func resolveAPIKeyStmt(dialect string) string {
	switch dialect {
	case DialectMySQL:
		// No admin schema: schema and database are one namespace in MySQL, and
		// the keys live in the base database the DSN names.
		return `SELECT tenant_id FROM tenant_api_keys WHERE key_hash = ? AND disabled_at IS NULL AND (expires_at IS NULL OR expires_at > NOW(6))`
	case DialectMSSQL:
		return `SELECT CONVERT(NVARCHAR(36), tenant_id) FROM admin.tenant_api_keys WHERE key_hash = @p1 AND disabled_at IS NULL AND (expires_at IS NULL OR expires_at > SYSUTCDATETIME())`
	default:
		return `SELECT tenant_id FROM admin.tenant_api_keys WHERE key_hash = $1 AND disabled_at IS NULL AND (expires_at IS NULL OR expires_at > now())`
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
		`UPDATE admin.tenant_api_keys SET disabled_at = now() WHERE key_id = $1 AND disabled_at IS NULL`, keyID)
	return err
}

// RevokeAPIKeyByHash revokes an API key by its sha256 hash rather than its
// key_id.
//
// cleat#2352. RevokeAPIKey exists for an operator who has a key_id from
// --list; this is for a caller that only ever has the hash -- oauthprovider's
// planned logout path (#2340 design v2 §(1)) computes rawKey's hash at mint
// time and links it via oauth_sessions.api_key_hash, and never sees the
// DB-generated key_id (CreateAPIKey does not return it -- see that function's
// own comment on why). Same Postgres-only scope as RevokeAPIKey, for the same
// reason: no production caller on another dialect yet, and now() / NOW(6) /
// SYSUTCDATETIME() differ alongside the placeholders. Refuses rather than
// guessing.
func (s *TenantStore) RevokeAPIKeyByHash(ctx context.Context, keyHash []byte) error {
	if s.dialect != DialectPostgres {
		return fmt.Errorf("auth: RevokeAPIKeyByHash is not implemented for %s", s.dialect)
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE admin.tenant_api_keys SET disabled_at = now() WHERE key_hash = $1 AND disabled_at IS NULL`, keyHash)
	return err
}

// RevokeOAuthAPIKeys soft-disables every live key minted for one OAuth
// identity, and reports how many it disabled. cleat#2340, design item 4.
//
// THE CALLER RENDERS THE TAG, and the only safe renderer is oauthprovider's
// OAuthIdentityTag. The WHERE below is exact string equality against what the
// MINT wrote, and the mint normalises both the value and the type; a caller
// that assembles "provider:identity" by hand from a stored row -- which is
// whatever an operator typed -- matches nothing, silently, and the keys it
// meant to kill keep authenticating until they expire on their own. That is
// cleat#2410, measured, and it is only fixed while every caller uses the
// renderer rather than the layout it produces.
//
// Postgres-only, matching RevokeAPIKeyByHash's scope AND its explicit refusal:
// OAuth login answers 501 on the other two dialects, so no key this could
// disable exists there. A silent fallback would emit PostgreSQL SQL at a
// database that reads `admin.` as a DATABASE name.
func (s *TenantStore) RevokeOAuthAPIKeys(ctx context.Context, oauthIdentity string) (int64, error) {
	if s.dialect != DialectPostgres {
		return 0, fmt.Errorf("auth: RevokeOAuthAPIKeys is not implemented for %s", s.dialect)
	}
	if oauthIdentity == "" {
		// LOUD RATHER THAN EMPTY-HANDED. An empty tag means a caller failed to
		// render one, and the statement would then disable every row an
		// operator had happened to write with an empty oauth_identity -- a
		// revocation nobody asked for, reported as success. Revoking nothing
		// is the correct answer to "revoke the keys of the empty identity",
		// and a caller that meant that can say so directly.
		return 0, fmt.Errorf("auth: RevokeOAuthAPIKeys needs an identity tag")
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE admin.tenant_api_keys SET disabled_at = now()
		 WHERE oauth_identity = $1 AND disabled_at IS NULL`, oauthIdentity)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// RevokeExpiredOAuthAPIKeys soft-disables every OAuth-minted key whose expiry
// has passed, across every tenant, and reports how many it disabled.
// cleat#2340, design v2 §(7).
//
// IT IS NOT WHAT ENFORCES EXPIRY. resolveAPIKeyStmt carries
// `AND (expires_at IS NULL OR expires_at > now())`, so an expired key is
// already refused at read time and this changes no request's outcome. What it
// keeps true is the invariant the table's other readers assume: for a key
// OAuth login minted, `disabled_at IS NULL` means "this key authenticates".
// Without it, an operator asking how many live keys an identity holds -- and
// RevokeOAuthAPIKeys' own rows-affected count, which filters on disabled_at
// alone -- counts credentials that stopped working whenever their expiry went
// by, and that number only ever grows.
//
// THE PREDICATE IS SCOPED TO oauth_identity IS NOT NULL, so a
// manually-provisioned key with an operator-set expires_at is left exactly as
// it is, disabled_at NULL and all. That asymmetry is chosen rather than
// overlooked: un-revoking is not a supported operation in this release, so
// flipping an operator's service key would be a surprise with no supported way
// back, while the read-time check already refuses it correctly.
//
// NO TENANT PARAMETER AND NO RLS CONTEXT, matching the resolver above it: this
// table is read before a tenant is known (migration 061 declined it a policy
// for exactly that reason), so there is nothing to scope by.
//
// IT IS HERE RATHER THAN IN oauthprovider BECAUSE OF A GRANT, and that was
// measured rather than assumed. A plugin reaching this table through
// plugin.AcrossAllTenants runs `SET LOCAL ROLE cleat_sweep` (engine's
// plugin_secrets.go), and cleat_sweep holds no privilege on
// admin.tenant_api_keys at all -- not SELECT, not UPDATE:
//
//	SELECT r.rolname, has_table_privilege(r.rolname, 'admin.tenant_api_keys', 'UPDATE')
//	FROM pg_roles r WHERE r.rolname IN ('cleat_app','cleat_sweep');
//	-- cleat_app  | t
//	-- cleat_sweep| f
//
// So the design's statement, written into the plugin's cross-tenant sweep,
// fails with `permission denied for table tenant_api_keys (42501)` on every
// tick and disables nothing, while logging an error into a background loop
// nobody reads. The worker's own connection is the one that holds the grant --
// the same connection, and the same reason, that Environment.MintOAuthAPIKey
// exists for. The sweep is the same operation on the same table, so it belongs
// on the same side of that boundary.
//
// Postgres-only, matching RevokeOAuthAPIKeys' scope and its explicit refusal:
// OAuth login answers 501 on the other two dialects, so no key this could
// disable exists there, and a silent fallback would emit PostgreSQL SQL at a
// database that reads `admin.` as a DATABASE name.
func (s *TenantStore) RevokeExpiredOAuthAPIKeys(ctx context.Context) (int64, error) {
	if s.dialect != DialectPostgres {
		return 0, fmt.Errorf("auth: RevokeExpiredOAuthAPIKeys is not implemented for %s", s.dialect)
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE admin.tenant_api_keys SET disabled_at = now()
		 WHERE oauth_identity IS NOT NULL
		   AND expires_at < now()
		   AND disabled_at IS NULL`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// GenerateAPIKey generates a random API key string.
func GenerateAPIKey() string {
	// cleat_sk_ prefix for easy identification in logs
	b := make([]byte, 32)
	_, _ = randRead(b)
	return fmt.Sprintf("cleat_sk_%x", b)
}
