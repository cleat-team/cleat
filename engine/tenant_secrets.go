package engine

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/cleat-team/cleat/plugin"
	"golang.org/x/crypto/hkdf"
)

// ErrNoSecretMasterKey is returned when a secret must be read or written and
// the deployment has no master key configured.
var ErrNoSecretMasterKey = errors.New("no secret master key is configured")

// ErrNoSecretDB is returned by a store built without a database handle.
//
// It exists because the alternative was a nil-pointer panic on the plugin call
// path, which is where this store is reached from. A test constructing a store
// with no database found it; in production db is never nil, so this is the
// difference between a latent crash and an error nobody ever sees.
var ErrNoSecretDB = errors.New("secret store has no database handle")

// ErrSecretNotFound is returned for a name this tenant has not set.
//
// It does NOT distinguish "no such name" from "a name another tenant owns",
// for the same reason tenant_domains does not: the store's statements are
// tenant-scoped, so a foreign name simply is not there.
var ErrSecretNotFound = errors.New("secret not found")

// secretRef matches ${secret:NAME} in a plugin call argument.
//
// The character class is deliberately narrow. A reference is substituted into
// JSON that a plugin then parses, so a name containing a quote or a backslash
// would let a workflow author reshape the document around the value it asked
// for. Restricting the name at the point of MATCH rather than validating after
// means a malformed reference is simply not a reference -- it is passed through
// as the literal text it is, and the plugin sees exactly what the workflow
// wrote.
var secretRef = regexp.MustCompile(`\$\{secret:([A-Za-z0-9_.-]{1,128})\}`)

// SecretStore holds per-tenant secrets encrypted at rest.
//
// WHY THE VALUE NEVER REACHES THE GUEST. A secrets host call returning a value
// to WASM would put the plaintext in guest memory and, worse, in whatever the
// guest then passes to a recorded call -- where engine.Redact is a field-name
// heuristic and would not catch it under a field named `config`. So a workflow
// passes a REFERENCE, ${secret:name}, and the host substitutes on the way in to
// the plugin.
//
// WHY THAT ALSO KEEPS IT OUT OF EVENT HISTORY, which is the property that makes
// this worth doing rather than a smaller version of the same problem:
// PluginCall records `PluginInput: inputJSON` -- the argument as the guest wrote
// it -- and separately passes that same string to the plugin function
// (engine/plugins.go). Substituting inside a WRAPPED function therefore cannot
// affect what was recorded, because the recorder reads the outer variable. The
// history keeps the reference. That is a structural guarantee rather than a
// redaction rule that has to keep up with field names.
type SecretStore struct {
	db      *sql.DB
	dialect string

	// master is the deployment key. Nil means secrets cannot be used, which is
	// distinct from "there are none" -- see CountSecrets and the worker's
	// startup check.
	master []byte
}

// NewSecretStore builds a store. master must be 32 bytes or nil.
func NewSecretStore(db *sql.DB, dialect string, master []byte) (*SecretStore, error) {
	if master != nil && len(master) != 32 {
		return nil, fmt.Errorf("secret master key must be 32 bytes, got %d", len(master))
	}
	return &SecretStore{db: db, dialect: dialect, master: master}, nil
}

// MasterKeyFromEnv decodes a base64 master key.
//
// FROM THE ENVIRONMENT AND NOT A FLAG, deliberately. A flag value is visible in
// `ps`, in /proc/<pid>/cmdline to any local user, and in whatever records the
// command line -- a container spec, a systemd unit, a shell history. None of
// those are places a deployment key should be. The environment is not a secure
// channel either, but it is not world-readable on a shared host.
func MasterKeyFromEnv(v string) ([]byte, error) {
	if strings.TrimSpace(v) == "" {
		return nil, nil
	}
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(v))
	if err != nil {
		return nil, fmt.Errorf("secret master key is not valid base64: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("secret master key must decode to 32 bytes, got %d", len(key))
	}
	return key, nil
}

// tenantKey derives this tenant's encryption key from the master key.
//
// PER TENANT, so that a key recovered from one tenant's ciphertext -- by
// cryptanalysis, by a bug, by a disclosed plaintext -- does not decrypt
// another's. HKDF with the tenant id as salt is the standard construction for
// exactly this, and it is cheap enough to do per call rather than cache, which
// avoids holding derived keys in memory longer than one operation.
func (s *SecretStore) tenantKey(tenantID string) ([]byte, error) {
	if s.master == nil {
		return nil, ErrNoSecretMasterKey
	}
	out := make([]byte, 32)
	r := hkdf.New(sha256.New, s.master, []byte(tenantID), []byte("cleat-tenant-secret-v1"))
	if _, err := io.ReadFull(r, out); err != nil {
		return nil, fmt.Errorf("derive tenant key: %w", err)
	}
	return out, nil
}

func (s *SecretStore) seal(tenantID, plaintext string) (string, error) {
	key, err := s.tenantKey(tenantID)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("nonce: %w", err)
	}
	// The tenant id is additional authenticated data, so a ciphertext moved to
	// another tenant's row fails to open rather than decrypting to something.
	ct := gcm.Seal(nonce, nonce, []byte(plaintext), []byte(tenantID))
	return base64.StdEncoding.EncodeToString(ct), nil
}

func (s *SecretStore) open(tenantID, stored string) (string, error) {
	key, err := s.tenantKey(tenantID)
	if err != nil {
		return "", err
	}
	raw, err := base64.StdEncoding.DecodeString(stored)
	if err != nil {
		return "", fmt.Errorf("stored secret is not valid base64: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(raw) < gcm.NonceSize() {
		return "", errors.New("stored secret is too short to contain a nonce")
	}
	nonce, ct := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
	pt, err := gcm.Open(nil, nonce, ct, []byte(tenantID))
	if err != nil {
		// Deliberately not wrapped with the underlying error: GCM failures are
		// uniform on purpose, and the reason a value did not open is not
		// something to report to whoever triggered the read.
		return "", errors.New("secret could not be decrypted with this deployment's master key")
	}
	return string(pt), nil
}

// PutSecret stores or replaces one secret for a tenant.
//
// Operator-only by convention rather than by grant: nothing in cleat's request
// path calls it. The same reasoning as tenant_domains' operator-only writes --
// a tenant able to write here could probe the namespace, and self-service
// secrets need an ownership story this change does not have.
func (s *SecretStore) PutSecret(ctx context.Context, tenantID, name, value string) error {
	if s == nil || s.db == nil {
		return ErrNoSecretDB
	}
	if !validSecretName(name) {
		return fmt.Errorf("secret name %q must match [A-Za-z0-9_.-]{1,128}", name)
	}
	sealed, err := s.seal(tenantID, value)
	if err != nil {
		return err
	}
	return s.execTenantScoped(ctx, func(q querier) error {
		_, err := q.ExecContext(ctx, putSecretUpdateStmt(s.dialect), sealed, tenantID, name)
		if err != nil {
			return err
		}
		// Insert only if the update matched nothing. Two statements rather than
		// one upsert because every dialect's upsert syntax either hides the
		// tenant predicate behind a conflict target or projects tenant_id
		// through a USING clause -- and cleat's own guards refuse both, for
		// good reason: a predicate a reader cannot see is a predicate a review
		// cannot check. TestMSSQLTenantScopedTablesAreQueriedWithATenantPredicate
		// and TestMSSQLUUIDColumnsAreConvertedInProjections both fired on the
		// MERGE this replaces.
		var exists int
		err = q.QueryRowContext(ctx, getSecretExistsStmt(s.dialect), tenantID, name).Scan(&exists)
		if errors.Is(err, sql.ErrNoRows) {
			_, err = q.ExecContext(ctx, putSecretInsertStmt(s.dialect), tenantID, name, sealed)
			return err
		}
		return err
	})
}

// querier is the subset of *sql.DB and *sql.Tx these statements need, so one
// body serves both the transaction-scoped path and the direct one.
type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// execTenantScoped runs fn inside a transaction carrying this request's tenant,
// where the dialect needs one.
//
// WHY A TRANSACTION AT ALL, since every statement here already carries
// `AND tenant_id = ?`: on PostgreSQL tenant_secrets has a row-level policy
// calling cleat.assert_tenant_set(), and that raises on the first candidate row
// WHATEVER the WHERE clause says. A predicate does not substitute for the
// session setting. TestNoPostgresStatementReachesAnRLSTableWithoutTheTenantSet
// says so in as many words, and it caught this: the first version used
// s.db.QueryRowContext directly and would have failed at runtime on every
// PostgreSQL deployment.
//
// beginTenantTx returns (nil, nil) when there is no tenant in context or the
// dialect is not PostgreSQL. Both are correct here rather than an error:
// cleatctl connects as an administrative role the policy does not apply to, and
// MySQL and SQL Server scope by other means.
func (s *SecretStore) execTenantScoped(ctx context.Context, fn func(querier) error) error {
	tx, err := beginTenantTx(ctx, s.db, plugin.Dialect(s.dialect), nil)
	if err != nil {
		return err
	}
	if tx == nil {
		return fn(s.db)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// GetSecret returns one decrypted secret.
func (s *SecretStore) GetSecret(ctx context.Context, tenantID, name string) (string, error) {
	if s == nil || s.db == nil {
		return "", ErrNoSecretDB
	}
	if !validSecretName(name) {
		return "", ErrSecretNotFound
	}
	var sealed string
	err := s.execTenantScoped(ctx, func(q querier) error {
		return q.QueryRowContext(ctx, getSecretStmt(s.dialect), tenantID, name).Scan(&sealed)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrSecretNotFound
	}
	if err != nil {
		return "", err
	}
	return s.open(tenantID, sealed)
}

// CountSecrets reports how many secrets exist across all tenants.
//
// For the worker's startup check only, and deliberately not tenant-scoped: it
// is asked before any request exists. A deployment holding secrets and started
// without a master key should be told at boot, not on the first workflow that
// needs one -- which would surface as a plugin call failing for a reason with
// no obvious connection to the missing configuration.
//
// DELIBERATELY COUNTS RETIRED ROWS TOO (cleat#1989). Retiring a secret
// (disabled_at) stops it resolving; it does not touch the ciphertext or
// remove the row, and set-secret revives it in place by clearing disabled_at
// -- the same master key that sealed it originally is what a revival needs to
// keep being readable. So a deployment with only retired secrets and no
// master key configured is exactly as stuck as one with only active
// secrets: reviving anything, or reading what's already there before
// retiring more of it, needs the key either way. Filtering retired rows out
// here would tell that operator "no secrets" while their ciphertext sits
// unreadable.
func (s *SecretStore) CountSecrets(ctx context.Context) (int, error) {
	if s == nil || s.db == nil {
		return 0, ErrNoSecretDB
	}
	// ACROSS ALL TENANTS, BY NAME. This asks a question that has no tenant --
	// "does this deployment hold any secrets at all" -- before any request
	// exists. On PostgreSQL tenant_secrets carries a policy whose
	// cleat.assert_tenant_set() raises on the first candidate row whatever the
	// WHERE clause says, so a direct read is not merely unscoped: it errors.
	// TestNoPostgresStatementReachesAnRLSTableWithoutTheTenantSet caught the
	// first version doing exactly that, and the consequence was worse than an
	// error, because the caller treats an error as "cannot tell" and the check
	// would have silently never fired.
	ctx = plugin.AcrossAllTenants(ctx,
		"startup check: whether this deployment holds any secrets, asked before any request and therefore for no tenant")
	var n int
	err := s.execTenantScoped(ctx, func(q querier) error {
		return q.QueryRowContext(ctx, `SELECT count(*) FROM tenant_secrets`).Scan(&n)
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}

// HasMasterKey reports whether secrets can be used at all.
func (s *SecretStore) HasMasterKey() bool { return s != nil && s.master != nil }

func validSecretName(name string) bool {
	if name == "" || len(name) > 128 {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '_' || r == '.' || r == '-':
		default:
			return false
		}
	}
	return true
}

// putSecretUpdateStmt also clears disabled_at (cleat#1989): re-running
// set-secret for a retired name is the documented way to revive it -- "Re-run
// set-secret to make it live again" is what retire-secret prints -- so the
// write path that already runs on every set-secret is where that has to
// happen, rather than a separate revive command nothing would remind an
// operator exists.
func putSecretUpdateStmt(dialect string) string {
	switch dialect {
	case "mysql":
		return `UPDATE tenant_secrets SET ciphertext = ?, disabled_at = NULL WHERE tenant_id = ? AND name = ?`
	case "mssql":
		return `UPDATE tenant_secrets SET ciphertext = @p1, disabled_at = NULL WHERE tenant_id = @p2 AND name = @p3`
	default:
		return `UPDATE tenant_secrets SET ciphertext = $1, disabled_at = NULL, updated_at = now() WHERE tenant_id = $2 AND name = $3`
	}
}

func putSecretInsertStmt(dialect string) string {
	switch dialect {
	case "mysql":
		return `INSERT INTO tenant_secrets (tenant_id, name, ciphertext) VALUES (?, ?, ?)`
	case "mssql":
		return `INSERT INTO tenant_secrets (tenant_id, name, ciphertext) VALUES (@p1, @p2, @p3)`
	default:
		return `INSERT INTO tenant_secrets (tenant_id, name, ciphertext) VALUES ($1, $2, $3)`
	}
}

// getSecretExistsStmt selects a literal rather than tenant_id, so no UUID
// column is projected -- TestMSSQLUUIDColumnsAreConvertedInProjections refuses
// a bare tenant_id in a SELECT list on SQL Server.
func getSecretExistsStmt(dialect string) string {
	switch dialect {
	case "mysql":
		return `SELECT 1 FROM tenant_secrets WHERE tenant_id = ? AND name = ?`
	case "mssql":
		return `SELECT 1 FROM tenant_secrets WHERE tenant_id = @p1 AND name = @p2`
	default:
		return `SELECT 1 FROM tenant_secrets WHERE tenant_id = $1 AND name = $2`
	}
}

// getSecretStmt refuses a retired row the same way it refuses a missing one
// (cleat#1989): AND disabled_at IS NULL, on all three dialects. Without it, a
// row an operator retired -- during a leak, exactly when it matters most --
// kept resolving via ${secret:NAME} as though nothing had happened.
func getSecretStmt(dialect string) string {
	switch dialect {
	case "mysql":
		return `SELECT ciphertext FROM tenant_secrets WHERE tenant_id = ? AND name = ? AND disabled_at IS NULL`
	case "mssql":
		return `SELECT ciphertext FROM tenant_secrets WHERE tenant_id = @p1 AND name = @p2 AND disabled_at IS NULL`
	default:
		return `SELECT ciphertext FROM tenant_secrets WHERE tenant_id = $1 AND name = $2 AND disabled_at IS NULL`
	}
}

// retireSecretStmt sets disabled_at, but only on a row that is not already
// retired -- so RowsAffected distinguishes "retired just now" (1) from
// "already retired, or no such row" (0), the same way revoke-api-key's own
// UPDATE does for admin.tenant_api_keys.
func retireSecretStmt(dialect string) string {
	switch dialect {
	case "mysql":
		return `UPDATE tenant_secrets SET disabled_at = NOW(6) WHERE tenant_id = ? AND name = ? AND disabled_at IS NULL`
	case "mssql":
		return `UPDATE tenant_secrets SET disabled_at = SYSDATETIMEOFFSET() WHERE tenant_id = @p1 AND name = @p2 AND disabled_at IS NULL`
	default:
		return `UPDATE tenant_secrets SET disabled_at = now() WHERE tenant_id = $1 AND name = $2 AND disabled_at IS NULL`
	}
}

// secretMetaStmt reads disabled_at without touching ciphertext, so looking a
// secret up to report its status needs no master key -- retire-secret and
// revoke-api-key both print the row before mutating it, on purpose, so an
// operator sees what they are about to cut off.
func secretMetaStmt(dialect string) string {
	switch dialect {
	case "mysql":
		return `SELECT disabled_at FROM tenant_secrets WHERE tenant_id = ? AND name = ?`
	case "mssql":
		return `SELECT disabled_at FROM tenant_secrets WHERE tenant_id = @p1 AND name = @p2`
	default:
		return `SELECT disabled_at FROM tenant_secrets WHERE tenant_id = $1 AND name = $2`
	}
}

// SecretMeta reports whether a secret row exists and, if so, whether it is
// retired -- without decrypting anything, so it needs no master key. Used by
// cleatctl retire-secret to show what it is about to change before changing
// it.
func (s *SecretStore) SecretMeta(ctx context.Context, tenantID, name string) (exists bool, disabledAt sql.NullTime, err error) {
	if s == nil || s.db == nil {
		return false, sql.NullTime{}, ErrNoSecretDB
	}
	err = s.execTenantScoped(ctx, func(q querier) error {
		return q.QueryRowContext(ctx, secretMetaStmt(s.dialect), tenantID, name).Scan(&disabledAt)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return false, sql.NullTime{}, nil
	}
	if err != nil {
		return false, sql.NullTime{}, err
	}
	return true, disabledAt, nil
}

// RetireSecret sets disabled_at on one tenant's secret, so it stops resolving
// via ${secret:NAME} -- the not-found error, the same as a name that was
// never set. It needs no master key: retiring is a metadata change, not a
// read or write of the ciphertext, and an operator cutting off a leaked
// secret during an incident must not be blocked on CLEAT_SECRET_MASTER_KEY
// being available right now.
//
// Reversible: re-running set-secret for the same name clears disabled_at
// (putSecretUpdateStmt, above).
//
// Returns rowsAffected = 0 for BOTH "no such secret" and "already retired" --
// callers that need to tell those apart (cleatctl's operator-facing message
// does) call SecretMeta first, the same way revoke-api-key looks its row up
// before deciding what to print.
func (s *SecretStore) RetireSecret(ctx context.Context, tenantID, name string) (rowsAffected int64, err error) {
	if s == nil || s.db == nil {
		return 0, ErrNoSecretDB
	}
	err = s.execTenantScoped(ctx, func(q querier) error {
		res, execErr := q.ExecContext(ctx, retireSecretStmt(s.dialect), tenantID, name)
		if execErr != nil {
			return execErr
		}
		rowsAffected, execErr = res.RowsAffected()
		return execErr
	})
	if err != nil {
		return 0, err
	}
	return rowsAffected, nil
}

// ResolveSecretRefs replaces every ${secret:NAME} in input with its value.
//
// Returns the input unchanged, and touches no database, when it contains no
// reference -- which is every call in a deployment that uses no secrets, so the
// cost of this feature on those is one regexp scan of a string already in hand.
//
// A reference naming a secret the tenant does not have is an ERROR rather than
// an empty substitution. Silently replacing it with "" would send a plugin an
// empty credential, and the failure would arrive from the third-party service
// as an authentication error naming nothing -- the least debuggable form of
// this mistake.
func ResolveSecretRefs(ctx context.Context, store *SecretStore, tenantID, input string) (string, error) {
	if store == nil || !strings.Contains(input, "${secret:") {
		return input, nil
	}
	if secretRef.MatchString(input) && !store.HasMasterKey() {
		return "", fmt.Errorf("this call references a secret but %w", ErrNoSecretMasterKey)
	}
	return resolveRefsWith(input, func(name string) (string, error) {
		return store.GetSecret(ctx, tenantID, name)
	})
}

// resolveRefsWith is the substitution itself, separated from the lookup.
//
// SPLIT SO THE ESCAPING IS TESTABLE WITHOUT A DATABASE, and that is not
// tidiness. The first version of this had the loop inline and a test asserting
// jsonEscape's behaviour directly; deleting the jsonEscape CALL from the loop
// left every test green, because the test exercised the function and not its
// use. That is the "which layer is holding the test up" defect, and the fix is
// a seam a test can drive end to end.
func resolveRefsWith(input string, lookup func(name string) (string, error)) (string, error) {
	matches := secretRef.FindAllStringSubmatchIndex(input, -1)
	if len(matches) == 0 {
		return input, nil
	}

	var b strings.Builder
	last := 0
	for _, m := range matches {
		name := input[m[2]:m[3]]
		value, err := lookup(name)
		if err != nil {
			if errors.Is(err, ErrSecretNotFound) {
				// Names the reference, never a value, and never whether some
				// OTHER tenant has it.
				return "", fmt.Errorf("secret %q is not set for this tenant", name)
			}
			return "", fmt.Errorf("resolving secret %q: %w", name, err)
		}
		b.WriteString(input[last:m[0]])
		b.WriteString(jsonEscape(value))
		last = m[1]
	}
	b.WriteString(input[last:])
	return b.String(), nil
}

// jsonEscape makes a secret value safe to splice into a JSON string literal.
//
// A reference is written inside quotes -- {"api_key":"${secret:openai}"} -- so a
// value containing a quote or a backslash would otherwise terminate the string
// and let the REST of the value be read as JSON structure. That turns a secret
// into a way to reshape the document a plugin receives, which is a worse
// outcome than the credential leaking.
//
// Only the characters JSON requires escaping inside a string. Control
// characters go to \u00XX; everything else passes through, so a value with
// non-ASCII text is unharmed.
func jsonEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(&b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	return b.String()
}
