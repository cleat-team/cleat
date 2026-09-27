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

	"github.com/cleat-team/cleat/internal/tenantctx"
	"github.com/cleat-team/cleat/plugin"
	"github.com/google/uuid"
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

	// ring holds the master keys this deployment can use. Nil means secrets
	// cannot be used, which is distinct from "there are none" -- see
	// CountSecrets and the worker's startup check.
	ring *KeyRing

	// beforeResealWrite, when set, runs between reseal's read of a row and its
	// conditional write. It exists so a test can land a concurrent set-secret
	// in exactly that window, which is the interleaving the compare-and-swap is
	// for and which a sequential test cannot otherwise reach. Nil in
	// production.
	beforeResealWrite func(tenantID, name string)

	// beforeGateCheck, when set, runs inside a write's gate span after the lock
	// is held and before the registry is read. It exists so a test can hold a
	// writer at the point where a booting worker must be excluded. Nil in
	// production.
	beforeGateCheck func()
}

// NewSecretStore builds a store around ONE key, which it treats as key_version
// 1. That is the shape of every deployment before rotation existed: rows carry
// key_version 1 by column default (migrations 081 / 069 / 073), so an existing
// deployment's single key is version 1 without anyone declaring it. master must
// be 32 bytes or nil.
func NewSecretStore(db *sql.DB, dialect string, master []byte) (*SecretStore, error) {
	if master == nil {
		return &SecretStore{db: db, dialect: dialect}, nil
	}
	ring, err := NewKeyRing(VersionedKey{Version: 1, Key: master})
	if err != nil {
		return nil, err
	}
	return &SecretStore{db: db, dialect: dialect, ring: ring}, nil
}

// NewSecretStoreWithRing builds a store around a key ring. A nil ring is the
// same as no master key.
func NewSecretStoreWithRing(db *sql.DB, dialect string, ring *KeyRing) *SecretStore {
	return &SecretStore{db: db, dialect: dialect, ring: ring}
}

// SecretKeyRingFromEnv reads the ring from the environment.
//
//	CLEAT_SECRET_MASTER_KEY                base64, 32 bytes: the current key
//	CLEAT_SECRET_MASTER_KEY_VERSION        its key_version; default 1
//	CLEAT_SECRET_MASTER_KEY_PREVIOUS       base64, 32 bytes: the key being retired
//	CLEAT_SECRET_MASTER_KEY_PREVIOUS_VERSION  its key_version; REQUIRED with the key
//
// It returns (nil, nil) when no key is configured at all, which is a legitimate
// state (a deployment that uses no secrets) and is distinct from a
// configuration that is present and wrong. Anything half-configured is an
// error: a version with no key, a previous key with no current one, a previous
// key with no version. Each of those has an obvious reading that is not the
// operator's, and guessing would seal rows under a key the operator did not
// pick.
//
// FROM THE ENVIRONMENT AND NOT A FLAG, for the reason MasterKeyFromEnv gives.
func SecretKeyRingFromEnv(getenv func(string) string) (*KeyRing, error) {
	const (
		curKey  = "CLEAT_SECRET_MASTER_KEY"
		curVer  = "CLEAT_SECRET_MASTER_KEY_VERSION"
		prevKey = "CLEAT_SECRET_MASTER_KEY_PREVIOUS"
		prevVer = "CLEAT_SECRET_MASTER_KEY_PREVIOUS_VERSION"
	)
	current, err := MasterKeyFromEnv(getenv(curKey))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", curKey, err)
	}
	previous, err := MasterKeyFromEnv(getenv(prevKey))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", prevKey, err)
	}
	if current == nil {
		for _, name := range []string{curVer, prevKey, prevVer} {
			if strings.TrimSpace(getenv(name)) != "" {
				return nil, fmt.Errorf("%s is set but %s is not: a key ring needs its current key", name, curKey)
			}
		}
		return nil, nil
	}
	version, err := KeyVersionFromEnv(getenv(curVer), 1, curVer)
	if err != nil {
		return nil, err
	}
	if previous == nil {
		if strings.TrimSpace(getenv(prevVer)) != "" {
			return nil, fmt.Errorf("%s is set but %s is not", prevVer, prevKey)
		}
		return NewKeyRing(VersionedKey{Version: version, Key: current})
	}
	pv, err := KeyVersionFromEnv(getenv(prevVer), 0, prevVer)
	if err != nil {
		return nil, err
	}
	if pv == 0 {
		return nil, fmt.Errorf("%s is set but %s is not: rows sealed under the previous key carry a "+
			"version, and guessing it would mislabel them", prevKey, prevVer)
	}
	return NewKeyRing(VersionedKey{Version: version, Key: current}, VersionedKey{Version: pv, Key: previous})
}

// SecretKeyVersionError is returned when a stored secret is sealed under a
// key_version this deployment holds no key for.
//
// It names the version and what IS configured, and nothing else: that is what
// an operator needs to fix it (add the key that carries that version, or run
// reseal-secrets under a ring that holds it), and neither number is secret. The
// message it replaces -- "could not be decrypted with this deployment's master
// key" -- was accurate and told nobody which key.
type SecretKeyVersionError struct {
	Version    int
	Configured []int
}

func (e *SecretKeyVersionError) Error() string {
	return fmt.Sprintf("secret is sealed under key_version %d, and no configured master key carries that "+
		"version (configured: %v); add the key that has version %d as CLEAT_SECRET_MASTER_KEY or "+
		"CLEAT_SECRET_MASTER_KEY_PREVIOUS", e.Version, e.Configured, e.Version)
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

// tenantKey derives this tenant's encryption key from one master key.
//
// PER TENANT, so that a key recovered from one tenant's ciphertext -- by
// cryptanalysis, by a bug, by a disclosed plaintext -- does not decrypt
// another's. HKDF with the tenant id as salt is the standard construction for
// exactly this, and it is cheap enough to do per call rather than cache, which
// avoids holding derived keys in memory longer than one operation.
//
// The master key is a PARAMETER and not a field because a store now holds
// several: the current one seals, and whichever key carries a row's
// key_version opens it. The derivation is the same for every version, on
// purpose -- the info string does not carry the version, so a row resealed
// under a new master key is derived by exactly the code that derived it before.
func tenantKey(master []byte, tenantID string) ([]byte, error) {
	out := make([]byte, 32)
	r := hkdf.New(sha256.New, master, []byte(tenantID), []byte("cleat-tenant-secret-v1"))
	if _, err := io.ReadFull(r, out); err != nil {
		return nil, fmt.Errorf("derive tenant key: %w", err)
	}
	return out, nil
}

// seal encrypts under the CURRENT key. The version it belongs to is
// s.ring.Current().Version, which is immutable for the life of the store, so a
// caller that writes the version beside the ciphertext cannot disagree with it.
func (s *SecretStore) seal(tenantID, plaintext string) (string, error) {
	if s.ring == nil {
		return "", ErrNoSecretMasterKey
	}
	key, err := tenantKey(s.ring.current.Key, tenantID)
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

// open decrypts a stored value with the key that carries the row's
// key_version. A version this ring has no key for is a *SecretKeyVersionError,
// which is not the same as a key that is present and wrong: the first is a
// configuration the operator can complete, the second is corruption or a
// mislabelled row.
func (s *SecretStore) open(tenantID, stored string, keyVersion int) (string, error) {
	if s.ring == nil {
		return "", ErrNoSecretMasterKey
	}
	k, ok := s.ring.Key(keyVersion)
	if !ok {
		return "", &SecretKeyVersionError{Version: keyVersion, Configured: s.ring.Versions()}
	}
	key, err := tenantKey(k.Key, tenantID)
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
		return "", fmt.Errorf("secret could not be decrypted with the master key for key_version %d", keyVersion)
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
	// The version is written WITH the ciphertext, in the same statement, on
	// both arms below. A row whose key_version disagrees with the key that
	// sealed it opens under the wrong key or under none, and no later read can
	// tell which -- so the two values are never written separately.
	version := s.ring.current.Version
	return s.gatedWrite(ctx, version, func(q querier) error {
		_, err := q.ExecContext(ctx, putSecretUpdateStmt(s.dialect), sealed, version, tenantID, name)
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
			_, err = q.ExecContext(ctx, putSecretInsertStmt(s.dialect), tenantID, name, sealed, version)
			return err
		}
		return err
	})
}

// querier is the subset of *sql.DB and *sql.Tx these statements need, so one
// body serves both the transaction-scoped path and the direct one.
type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
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
	var keyVersion int
	err := s.execTenantScoped(ctx, func(q querier) error {
		return q.QueryRowContext(ctx, getSecretStmt(s.dialect), tenantID, name).Scan(&sealed, &keyVersion)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrSecretNotFound
	}
	if err != nil {
		return "", err
	}
	return s.open(tenantID, sealed, keyVersion)
}

// CountSecrets reports how many secrets exist across all tenants, retired
// ones included.
//
// For the worker's startup check only, and deliberately not scoped to one
// tenant: it is asked before any request exists. A deployment holding secrets
// and started without a master key should be told at boot, not on the first
// workflow that needs one -- which would surface as a plugin call failing for
// a reason with no obvious connection to the missing configuration.
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
//
// READ TENANT BY TENANT, NOT "ACROSS ALL TENANTS" (cleat#2123). This used to
// mark the context plugin.AcrossAllTenants and run one unscoped
// SELECT count(*), and that read cannot see tenant_secrets on two of the
// three dialects. Measured against a real database per dialect, PostgreSQL as
// cleat_app so that the policy applied, with two rows seeded, one of them under
// a suspended tenant:
//
//	PostgreSQL   0, with "cleat.tenant_id is not set (P0001)"
//	SQL Server   0, with no error at all
//	MySQL        1  (MySQL is single-tenant, so one row was all it could hold)
//
// PostgreSQL: AcrossAllTenants does SET LOCAL ROLE cleat_sweep and sets
// cleat.cross_tenant, but this table's policy is
// `tenant_id = cleat.assert_tenant_set()` (migration 081) and that function
// raises when cleat.tenant_id is unset -- it never reads the marker. SQL
// Server: the policy binds dbo.fn_tenant_filter, whose default form
// (migration 075) reads only SESSION_CONTEXT('tenant_id'), so the marker is
// ignored and the filter returns nothing. The marker is honoured by PLUGIN
// tables, which is what plugin.AcrossAllTenants documents; it is not honoured
// by this core table, and nothing said so.
//
// So the answer is assembled the way the rotating claim does it: enumerate
// admin.tenants, which carries no row-level security, then read each tenant's
// rows under that tenant's own context. That works for the role a worker
// actually runs as, with no cleat_sweep grant and no BYPASSRLS.
//
// EVERY TENANT, SUSPENDED ONES INCLUDED, which is why this does not call
// TenantLister.ListTenantIDs: that excludes suspended tenants, on purpose,
// because it decides where NEW work goes. A suspended tenant's secrets are
// still ciphertext sealed under some key, and revival needs that key exactly
// as a retired secret's does. The enumeration is complete by construction --
// tenant_secrets.tenant_id is a foreign key to the tenants table on all three
// dialects (migrations 081 / 069 / 073), so no row can belong to a tenant this
// does not list.
//
// A READ THAT FAILS IS AN ERROR AND NOT A ZERO. The caller must treat it as
// "could not establish", never as "none"; see checkSecretsUsable.
func (s *SecretStore) CountSecrets(ctx context.Context) (int, error) {
	total := 0
	err := s.forEachTenant(ctx, func(tctx context.Context, tid string) error {
		var n int
		if err := s.execTenantScoped(tctx, func(q querier) error {
			return q.QueryRowContext(tctx, countTenantSecretsStmt(s.dialect), tid).Scan(&n)
		}); err != nil {
			return fmt.Errorf("count secrets for tenant %s: %w", tid, err)
		}
		total += n
		return nil
	})
	return total, err
}

// forEachTenant calls fn once per tenant, suspended ones included, with a
// context that carries that tenant. It is the one place that enumerates
// tenants for a secrets operation (see CountSecrets for why it does not use
// ListTenantIDs), so the boot check, the version census and reseal all agree on
// which tenants exist. An error from fn stops the walk and is returned as is.
func (s *SecretStore) forEachTenant(ctx context.Context, fn func(ctx context.Context, tenantID string) error) error {
	if s == nil || s.db == nil {
		return ErrNoSecretDB
	}
	tenants, err := s.allTenantIDs(ctx)
	if err != nil {
		return err
	}
	for _, tid := range tenants {
		id, perr := uuid.Parse(tid)
		if perr != nil {
			return fmt.Errorf("tenant id %q is not a UUID: %w", tid, perr)
		}
		// THE CANONICAL FORM, NOT WHAT THE DATABASE PRINTED. The tenant id is
		// both the HKDF salt and the GCM additional data, so it is part of the
		// key: "71AF8836-..." and "71af8836-..." are different keys. SQL Server
		// returns a UNIQUEIDENTIFIER in upper case (CONVERT(varchar(36), ...)),
		// while every secret was sealed with the lower-case form that
		// uuid.UUID.String() gives -- cleatctl set-secret parses the argument, and
		// the worker takes the tenant from its request context. Passing the raw
		// string to open() made a suspended tenant's secret unreadable on SQL
		// Server, in a test that had sealed it a moment earlier. A tenant id that
		// reaches cryptography is always uuid.UUID.String().
		if err := fn(tenantctx.With(ctx, id), id.String()); err != nil {
			return err
		}
	}
	return nil
}

// allTenantIDs lists every tenant, suspended or not. See CountSecrets for why
// this is not ListTenantIDs.
//
// THE STATEMENT IS A LITERAL AT EACH CALL SITE, not the result of a helper.
// TestNoPostgresStatementReachesAnRLSTableWithoutTheTenantSet reads the query
// string out of the source and refuses one it cannot resolve -- "an unreadable
// statement is not a safe statement, it is one nothing has checked". This table
// has no row-level security, so nothing here needs the guard's protection, but
// the guard cannot know that without reading it. SQL Server converts the id
// because TestMSSQLUUIDColumnsAreConvertedInProjections refuses a bare
// UNIQUEIDENTIFIER in a SELECT list.
func (s *SecretStore) allTenantIDs(ctx context.Context) ([]string, error) {
	var rows *sql.Rows
	var err error
	switch s.dialect {
	case "mysql":
		rows, err = s.db.QueryContext(ctx, `SELECT tenant_id FROM tenants`)
	case "mssql":
		rows, err = s.db.QueryContext(ctx, `SELECT CONVERT(varchar(36), tenant_id) FROM admin.tenants`)
	default:
		rows, err = s.db.QueryContext(ctx, `SELECT tenant_id FROM admin.tenants`)
	}
	if err != nil {
		return nil, fmt.Errorf("list tenants: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("list tenants: scan: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list tenants: %w", err)
	}
	return ids, nil
}

func countTenantSecretsStmt(dialect string) string {
	switch dialect {
	case "mysql":
		return `SELECT count(*) FROM tenant_secrets WHERE tenant_id = ?`
	case "mssql":
		return `SELECT count(*) FROM tenant_secrets WHERE tenant_id = @p1`
	default:
		return `SELECT count(*) FROM tenant_secrets WHERE tenant_id = $1`
	}
}

// HasMasterKey reports whether secrets can be used at all.
func (s *SecretStore) HasMasterKey() bool { return s != nil && s.ring != nil }

// KeyVersions lists the key versions this store can open, ascending; nil when
// it holds no key. It is what a worker publishes to the registry.
func (s *SecretStore) KeyVersions() []int {
	if s == nil {
		return nil
	}
	return s.ring.Versions()
}

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
		return `UPDATE tenant_secrets SET ciphertext = ?, key_version = ?, disabled_at = NULL WHERE tenant_id = ? AND name = ?`
	case "mssql":
		return `UPDATE tenant_secrets SET ciphertext = @p1, key_version = @p2, disabled_at = NULL WHERE tenant_id = @p3 AND name = @p4`
	default:
		return `UPDATE tenant_secrets SET ciphertext = $1, key_version = $2, disabled_at = NULL, updated_at = now() WHERE tenant_id = $3 AND name = $4`
	}
}

func putSecretInsertStmt(dialect string) string {
	switch dialect {
	case "mysql":
		return `INSERT INTO tenant_secrets (tenant_id, name, ciphertext, key_version) VALUES (?, ?, ?, ?)`
	case "mssql":
		return `INSERT INTO tenant_secrets (tenant_id, name, ciphertext, key_version) VALUES (@p1, @p2, @p3, @p4)`
	default:
		return `INSERT INTO tenant_secrets (tenant_id, name, ciphertext, key_version) VALUES ($1, $2, $3, $4)`
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
		return `SELECT ciphertext, key_version FROM tenant_secrets WHERE tenant_id = ? AND name = ? AND disabled_at IS NULL`
	case "mssql":
		return `SELECT ciphertext, key_version FROM tenant_secrets WHERE tenant_id = @p1 AND name = @p2 AND disabled_at IS NULL`
	default:
		return `SELECT ciphertext, key_version FROM tenant_secrets WHERE tenant_id = $1 AND name = $2 AND disabled_at IS NULL`
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
