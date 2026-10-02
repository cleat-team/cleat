package auth

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// OperatorStore is the operator credential's storage: the table
// migrations/{postgres/010,mysql/008,mssql/009}_operator_api_keys.sql creates,
// read and written with the same per-dialect split auth.TenantStore uses.
//
// It is a separate type rather than more methods on TenantStore for the reason
// the two credentials exist separately. Every statement here runs on a
// connection that is NOT tenant-scoped (see ResolveOperatorFromAPIKey), and a
// store that knew how to resolve both would invite a caller to hand it
// whichever pool was nearest -- which is precisely the mistake cleat#866
// records, where an API-key lookup ran through a tenant-scoped pool and every
// request 401'd on MySQL while the row sat in the base database the whole time.
type OperatorStore struct {
	db      *sql.DB
	dialect string
}

// NewOperatorStoreForDialect creates an OperatorStore for a named dialect.
//
// An unrecognised dialect is an error rather than a silent fallback to
// PostgreSQL: the statements below differ in their placeholders and in whether
// the table is schema-qualified, so a fallback would emit `$1` and `admin.` on
// a dialect that wants neither, and fail at run time on the one database the
// author is not running. Mirrors NewTenantStoreForDialect, including the shape
// of its message, because the two are built from the same `--driver` value and
// a reader comparing them should not have to check whether they agree.
func NewOperatorStoreForDialect(db *sql.DB, dialect string) (*OperatorStore, error) {
	switch dialect {
	case DialectPostgres, DialectMySQL, DialectMSSQL:
		return &OperatorStore{db: db, dialect: dialect}, nil
	default:
		return nil, fmt.Errorf("auth: unknown dialect %q (want %q, %q or %q)",
			dialect, DialectPostgres, DialectMySQL, DialectMSSQL)
	}
}

// ResolveOperatorFromAPIKey turns an operator key's hash into the credential it
// names. It implements OperatorResolver, and is the only method here the
// request path calls.
//
// THE CONNECTION IS NOT TENANT-SCOPED, and that is the whole reason this does
// not live on the engine's per-tenant stores -- the argument
// auth.TenantStore.ResolveTenantFromAPIKey sets out in full (auth/tenant_store.go),
// and the one the tenant keys table's own design rests on: resolving the key is
// what tells you which scope to open, so a lookup that must already know it is
// circular. On MySQL that is not a stylistic preference -- tenant isolation there is a database boundary, so a
// tenant-scoped pool looks in cleat_<tenant> while the row is in the base
// database, and the operator key would 401 on every request while sitting
// visible in a psql session. cleat#866, again, for the other credential.
//
// The WHERE clause excludes a disabled key and an expired one, in the same
// three spellings resolveAPIKeyStmt uses. Nothing else enforces either: the
// migration's partial index over key_hash WHERE disabled_at IS NULL is useful
// only to a query that filters on that column, and is not itself a check.
func (s *OperatorStore) ResolveOperatorFromAPIKey(ctx context.Context, keyHash []byte) (Operator, error) {
	var op Operator
	if err := s.db.QueryRowContext(ctx, resolveOperatorStmt(s.dialect), keyHash).Scan(&op.KeyID, &op.Description); err != nil {
		return Operator{}, err
	}
	return op, nil
}

// resolveOperatorStmt is the hash lookup for a dialect.
//
// key_id is projected as TEXT on PostgreSQL and CONVERTed on SQL Server, so the
// same Go string receives both. That is not tidiness: SQL Server returns
// UNIQUEIDENTIFIER in a byte order the drivers do not scan directly, which is
// why ResolveTenantFromAPIKey parses a CONVERT(NVARCHAR(36)) projection instead
// of selecting the column -- the same shape as engine/mssql_deployment.go,
// enforced tree-wide by TestMSSQLUUIDColumnsAreConvertedInProjections, which
// caught the first version of that function. A bare `SELECT key_id` on
// PostgreSQL would put the driver's own uuid decoding between the column and a
// Go string, which is a question about the driver rather than about this table.
//
// A function rather than three inline strings for the reason createAPIKeyStmt
// gives: the three can then be asserted without a database, which is the only
// thing that catches an unknown dialect falling through to the PostgreSQL
// default and emitting `$1` against a database that wants `?`.
func resolveOperatorStmt(dialect string) string {
	switch dialect {
	case DialectMySQL:
		// No admin schema: schema and database are one namespace in MySQL, and
		// the base database the DSN names is where the row lives.
		return `SELECT key_id, description FROM operator_api_keys WHERE key_hash = ? AND disabled_at IS NULL AND (expires_at IS NULL OR expires_at > NOW(6))`
	case DialectMSSQL:
		return `SELECT CONVERT(NVARCHAR(36), key_id), description FROM admin.operator_api_keys WHERE key_hash = @p1 AND disabled_at IS NULL AND (expires_at IS NULL OR expires_at > SYSUTCDATETIME())`
	default:
		return `SELECT key_id::text, description FROM admin.operator_api_keys WHERE key_hash = $1 AND disabled_at IS NULL AND (expires_at IS NULL OR expires_at > now())`
	}
}

// CreateOperatorKey mints an operator credential and returns it.
//
// rawKey is supplied by the caller rather than generated here, matching
// createAPIKey and for the same reason: the caller is the only place that can
// show the operator the secret once, and a store that generated it would have
// to return the plaintext to be useful. cleatctl operator-key create calls
// GenerateOperatorKey and passes the result.
//
// expiresAt is nil for a key that never expires.
func (s *OperatorStore) CreateOperatorKey(ctx context.Context, description, rawKey string, expiresAt *time.Time) (Operator, error) {
	keyHash := sha256.Sum256([]byte(rawKey))

	// key_id is generated here and INSERTed explicitly on all three dialects,
	// rather than left to PostgreSQL's gen_random_uuid() and SQL Server's
	// NEWID(). RETURNING would be the tidy way to learn an id back, and SQL
	// Server has no RETURNING -- so the alternative is one shape on two
	// dialects and a read-back on the third. One statement, one argument list,
	// and the value is identical either way: a UUID the caller would otherwise
	// have to fetch.
	id := uuid.New().String()
	if _, err := s.db.ExecContext(ctx, createOperatorStmt(s.dialect), id, keyHash[:], description, expiresAt); err != nil {
		return Operator{}, err
	}
	return Operator{KeyID: id, Description: description}, nil
}

// createOperatorStmt returns the INSERT for a dialect. The statements differ
// only in their placeholders and in whether the table is schema-qualified --
// unlike createAPIKeyStmt there is no key_id column-default difference, because
// every dialect here is given one.
func createOperatorStmt(dialect string) string {
	switch dialect {
	case DialectMySQL:
		return `INSERT INTO operator_api_keys (key_id, key_hash, description, expires_at) VALUES (?, ?, ?, ?)`
	case DialectMSSQL:
		return `INSERT INTO admin.operator_api_keys (key_id, key_hash, description, expires_at) VALUES (@p1, @p2, @p3, @p4)`
	default:
		return `INSERT INTO admin.operator_api_keys (key_id, key_hash, description, expires_at) VALUES ($1, $2, $3, $4)`
	}
}

// ListOperatorKeys returns every credential, newest first, INCLUDING revoked
// and expired ones.
//
// It is deliberately not filtered. An operator asking to list keys is asking
// what exists, and a revoked key that has vanished from the list is
// indistinguishable from one that was never created -- the same reason
// RevokeOperatorKey disables rather than deletes. The live/dead distinction is
// carried per row instead, so a reader sees it rather than inferring it from an
// absence.
func (s *OperatorStore) ListOperatorKeys(ctx context.Context) ([]Operator, error) {
	rows, err := s.db.QueryContext(ctx, listOperatorStmt(s.dialect))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Operator{}
	for rows.Next() {
		var op Operator
		var disabledAt, expiresAt sql.NullTime
		if err := rows.Scan(&op.KeyID, &op.Description, &disabledAt, &expiresAt); err != nil {
			return nil, err
		}
		op.Disabled = disabledAt.Valid
		if expiresAt.Valid {
			t := expiresAt.Time
			op.ExpiresAt = &t
		}
		out = append(out, op)
	}
	return out, rows.Err()
}

func listOperatorStmt(dialect string) string {
	switch dialect {
	case DialectMySQL:
		return `SELECT key_id, description, disabled_at, expires_at FROM operator_api_keys ORDER BY created_at DESC`
	case DialectMSSQL:
		return `SELECT CONVERT(NVARCHAR(36), key_id), description, disabled_at, expires_at FROM admin.operator_api_keys ORDER BY created_at DESC`
	default:
		return `SELECT key_id::text, description, disabled_at, expires_at FROM admin.operator_api_keys ORDER BY created_at DESC`
	}
}

// RevokeOperatorKey disables a key, by its key_id.
//
// Disabling rather than deleting, for the reason the table carries a disabled_at
// column at all: a deleted credential cannot be told from one that never
// existed, so an audit record naming it becomes unreadable. The UPDATE is
// idempotent on purpose -- `AND disabled_at IS NULL` means a second revoke
// changes no row rather than moving the timestamp and rewriting when the key
// died -- so re-running it is safe. ErrOperatorKeyNotFound then means "no live
// key with that id", which is the union of "never existed" and "already
// revoked"; a caller that must tell those apart reads the row, because this
// statement cannot observe the difference.
//
// The row count is the test, NOT the "0 rows affected" a caller might read as
// "no such key". MySQL reports rows CHANGED rather than rows MATCHED, so the
// two answers differ there the moment the UPDATE is a no-op -- but here the
// WHERE clause makes a matched row always a changed row, which is what makes
// this one place the distinction does not bite.
func (s *OperatorStore) RevokeOperatorKey(ctx context.Context, keyID string) error {
	res, err := s.db.ExecContext(ctx, revokeOperatorStmt(s.dialect), keyID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		// Not fatal: the statement ran. Refusing a successful revoke because
		// its row count could not be read would trade a real outcome for a
		// reporting detail.
		return nil
	}
	if n == 0 {
		return ErrOperatorKeyNotFound
	}
	return nil
}

// ErrOperatorKeyNotFound is returned when a revoke matched no live row.
var ErrOperatorKeyNotFound = errors.New("auth: no live operator key with that key id")

func revokeOperatorStmt(dialect string) string {
	switch dialect {
	case DialectMySQL:
		return `UPDATE operator_api_keys SET disabled_at = NOW(6) WHERE key_id = ? AND disabled_at IS NULL`
	case DialectMSSQL:
		return `UPDATE admin.operator_api_keys SET disabled_at = SYSUTCDATETIME() WHERE key_id = @p1 AND disabled_at IS NULL`
	default:
		return `UPDATE admin.operator_api_keys SET disabled_at = now() WHERE key_id = $1 AND disabled_at IS NULL`
	}
}
