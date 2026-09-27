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
	"sort"
	"strings"

	"golang.org/x/crypto/hkdf"
)

// ErrNoDeploymentSecretDB is returned by a store built without a database
// handle. Mirrors ErrNoSecretDB, for the same reason: a nil db must be a
// returned error on the plugin call path, not a panic.
var ErrNoDeploymentSecretDB = errors.New("deployment secret store has no database handle")

// ErrDeploymentSecretNotFound is returned for a name that is not set, or is
// retired. It does not distinguish the two, the same as ErrSecretNotFound.
var ErrDeploymentSecretNotFound = errors.New("deployment secret not found")

// deploymentSecretInfo is the HKDF info string for deployment secrets --
// distinct from tenantKey's "cleat-tenant-secret-v1", so that even a
// deployment configured with the SAME master key for both stores (the
// default: see migration 103/102/102's header, "SAME KEY RING") derives a
// different key per store. See a_deployment_secret_domain_is_separate_test.go.
const deploymentSecretInfo = "cleat-deployment-secret-v1"

// DeploymentSecretStore holds deployment-wide credentials encrypted at rest:
// values that belong to the whole deployment rather than to any one tenant
// (cleat#1992 part 1) -- blobstore's S3 key pair, email's SendGrid key, one
// key per configured llm provider, slacknotify's request-signing secret,
// scheduledbackup's backup-target DSN.
//
// NO TENANT DIMENSION, unlike SecretStore. There is no tenant_id column
// (migration 103/102/102), no row-level security to route around, and so no
// execTenantScoped/beginTenantTx machinery here at all -- every statement is
// a plain, unscoped read or write against one global table.
//
// THE RING IS SHARED WITH SecretStore, deliberately: an operator configures
// one master key (CLEAT_SECRET_MASTER_KEY and its _PREVIOUS pair), not two.
// What keeps that safe is domain separation in seal/open below, not a
// second key -- see deploymentSecretInfo.
//
// WRITES ARE NOT GATED the way SecretStore.PutSecret's are (secret_key_gate.go,
// cleat#1991's write gate against a live worker that cannot yet open the new
// key version). That gate reads the SAME worker registry this store could
// reuse, and its absence here is a deliberate, narrower scope for this PR
// rather than an oversight: cleat#1992's acceptance criteria for this piece
// are the boot-time fail-closed check (setup.go) and the domain-separation
// tests, and a deployment secret is rotated far less often, by an operator
// already following the same "roll the ring out to every worker first"
// procedure use-secrets.md documents for tenant secrets. Flagged rather than
// silently dropped: a future PR wiring resealDeploymentSecrets/
// putDeploymentSecret through SecretStore's gate would close the same
// mid-rotation race #1991 closed for tenant_secrets.
type DeploymentSecretStore struct {
	db      *sql.DB
	dialect string
	ring    *KeyRing
}

// NewDeploymentSecretStore builds a store around ring, which is the SAME ring
// a caller builds for SecretStore -- see the type's doc comment for why. A
// nil ring is valid and produces a store whose seal/open return
// ErrNoSecretMasterKey, matching SecretStore's nil-ring convention.
func NewDeploymentSecretStore(db *sql.DB, dialect string, ring *KeyRing) *DeploymentSecretStore {
	return &DeploymentSecretStore{db: db, dialect: dialect, ring: ring}
}

// HasMasterKey reports whether deployment secrets can be used at all.
func (s *DeploymentSecretStore) HasMasterKey() bool { return s != nil && s.ring != nil }

// deploymentSecretKey derives this deployment's encryption key from one
// master key. UNLIKE tenantKey, the HKDF salt is fixed (nil) rather than a
// per-row identifier -- there is exactly one deployment, not one key per row
// -- and the per-row binding that tenantKey gets from AAD=tenantID, this gets
// from AAD=name in seal/open below. The info string is what separates this
// derivation from tenantKey's, even when master is the same key.
func deploymentSecretKey(master []byte) ([]byte, error) {
	out := make([]byte, 32)
	r := hkdf.New(sha256.New, master, nil, []byte(deploymentSecretInfo))
	if _, err := io.ReadFull(r, out); err != nil {
		return nil, fmt.Errorf("derive deployment secret key: %w", err)
	}
	return out, nil
}

// seal encrypts under the CURRENT key, with the secret's own name as
// additional authenticated data -- so a row copied to another name (or a
// tenant secret's ciphertext copied here) fails to open rather than
// decrypting to something. See tenant_secrets.go's seal for the same
// construction with tenantID in the AAD role.
func (s *DeploymentSecretStore) seal(name, plaintext string) (string, error) {
	if s.ring == nil {
		return "", ErrNoSecretMasterKey
	}
	key, err := deploymentSecretKey(s.ring.current.Key)
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
	ct := gcm.Seal(nonce, nonce, []byte(plaintext), []byte(name))
	return base64.StdEncoding.EncodeToString(ct), nil
}

// open decrypts a stored value with the key that carries the row's
// key_version, authenticating against name exactly as seal does.
func (s *DeploymentSecretStore) open(name, stored string, keyVersion int) (string, error) {
	if s.ring == nil {
		return "", ErrNoSecretMasterKey
	}
	k, ok := s.ring.Key(keyVersion)
	if !ok {
		return "", &SecretKeyVersionError{Version: keyVersion, Configured: s.ring.Versions()}
	}
	key, err := deploymentSecretKey(k.Key)
	if err != nil {
		return "", err
	}
	raw, err := base64.StdEncoding.DecodeString(stored)
	if err != nil {
		return "", fmt.Errorf("stored deployment secret is not valid base64: %w", err)
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
		return "", errors.New("stored deployment secret is too short to contain a nonce")
	}
	nonce, ct := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
	pt, err := gcm.Open(nil, nonce, ct, []byte(name))
	if err != nil {
		return "", fmt.Errorf("deployment secret could not be decrypted with the master key for key_version %d", keyVersion)
	}
	return string(pt), nil
}

// normalizeDeploymentSecretName lowercases name before it touches the
// database, on every dialect -- not only the two that need it. MySQL and SQL
// Server default to case-INSENSITIVE collation, so `WHERE name = ?` with
// "EMAIL.sendgrid_api_key" matches the row stored as "email.sendgrid_api_key"
// on those two dialects but not PostgreSQL, and PutDeploymentSecret's
// UPDATE-then-INSERT upsert then takes the UPDATE branch: it reseals the
// EXISTING row's ciphertext, still keyed by the correct row, but with AAD
// bound to the WRONGLY-CASED name argument (seal(name, ...) uses the caller's
// name verbatim). The real secret is not lost from the table, but it stops
// opening under the lowercase name every plugin actually looks up --
// GetDeploymentSecret(ctx, "email.sendgrid_api_key") then fails decryption,
// and the fail-closed startup check refuses the worker. All five fixed names
// (docs/how-to/use-deployment-secrets.md) are lowercase, so normalizing here
// costs a correctly-cased caller nothing and makes every dialect behave
// alike rather than only the two whose collation happens to hide the bug on
// PostgreSQL. Found in cleat-review's #2202 pass.
func normalizeDeploymentSecretName(name string) string {
	return strings.ToLower(name)
}

// PutDeploymentSecret stores or replaces one deployment secret.
//
// cleatctl-only by convention, same as PutSecret: nothing on the worker's
// request or plugin-call path writes here (migration 103 also removes
// cleat_app's write grant on PostgreSQL, so the convention is enforced at
// the database layer there too).
func (s *DeploymentSecretStore) PutDeploymentSecret(ctx context.Context, name, value string) error {
	if s == nil || s.db == nil {
		return ErrNoDeploymentSecretDB
	}
	name = normalizeDeploymentSecretName(name)
	if !validSecretName(name) {
		return fmt.Errorf("deployment secret name %q must match [A-Za-z0-9_.-]{1,128}", name)
	}
	sealed, err := s.seal(name, value)
	if err != nil {
		return err
	}
	version := s.ring.current.Version
	res, err := s.db.ExecContext(ctx, putDeploymentSecretUpdateStmt(s.dialect), sealed, version, name)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n > 0 {
		return nil
	}
	_, err = s.db.ExecContext(ctx, putDeploymentSecretInsertStmt(s.dialect), name, sealed, version)
	return err
}

// GetDeploymentSecret returns one decrypted deployment secret.
func (s *DeploymentSecretStore) GetDeploymentSecret(ctx context.Context, name string) (string, error) {
	if s == nil || s.db == nil {
		return "", ErrNoDeploymentSecretDB
	}
	name = normalizeDeploymentSecretName(name)
	if !validSecretName(name) {
		return "", ErrDeploymentSecretNotFound
	}
	var sealed string
	var keyVersion int
	err := s.db.QueryRowContext(ctx, getDeploymentSecretStmt(s.dialect), name).Scan(&sealed, &keyVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrDeploymentSecretNotFound
	}
	if err != nil {
		return "", err
	}
	return s.open(name, sealed, keyVersion)
}

// DeploymentSecretMeta reports whether a row exists and, if so, whether it is
// retired -- without decrypting anything, mirroring SecretStore.SecretMeta.
func (s *DeploymentSecretStore) DeploymentSecretMeta(ctx context.Context, name string) (exists bool, disabledAt sql.NullTime, err error) {
	if s == nil || s.db == nil {
		return false, sql.NullTime{}, ErrNoDeploymentSecretDB
	}
	name = normalizeDeploymentSecretName(name)
	if !validSecretName(name) {
		// Same "not found" shape as a genuine miss below, not an error: an
		// invalid name can never match a stored row, so there is nothing
		// distinct to report. Load-bearing on SQL Server specifically --
		// its default collation is PAD SPACE, so a name with trailing
		// whitespace would otherwise reach the query below and MATCH the
		// real row with the whitespace trimmed for comparison purposes,
		// which is exactly the bug this guards (see RetireDeploymentSecret's
		// doc comment). cleat-review's #2202 re-check.
		return false, sql.NullTime{}, nil
	}
	err = s.db.QueryRowContext(ctx, deploymentSecretMetaStmt(s.dialect), name).Scan(&disabledAt)
	if errors.Is(err, sql.ErrNoRows) {
		return false, sql.NullTime{}, nil
	}
	if err != nil {
		return false, sql.NullTime{}, err
	}
	return true, disabledAt, nil
}

// RetireDeploymentSecret sets disabled_at, mirroring RetireSecret. No master
// key is needed: retiring is a metadata change.
//
// Validates name after normalizing, the same as Put/Get, rather than letting
// an invalid name reach the query -- found by cleat-review's #2202 re-check
// as a real bug on SQL Server, whose default collation is PAD SPACE: trailing
// whitespace is insignificant in a `WHERE name = @p1` comparison there (not
// on PostgreSQL or MySQL's usual collations), so
// RetireDeploymentSecret(ctx, "email.sendgrid_api_key ") -- a name with a
// trailing space, never a name anything could have PUT -- matched and
// retired the REAL row on mssql instead of affecting nothing. validSecretName
// rejects the space, so this now returns (0, nil): the same "no such secret"
// shape a genuine non-match already returns, since an invalid name can never
// correspond to a stored one.
func (s *DeploymentSecretStore) RetireDeploymentSecret(ctx context.Context, name string) (rowsAffected int64, err error) {
	if s == nil || s.db == nil {
		return 0, ErrNoDeploymentSecretDB
	}
	name = normalizeDeploymentSecretName(name)
	if !validSecretName(name) {
		return 0, nil
	}
	res, err := s.db.ExecContext(ctx, retireDeploymentSecretStmt(s.dialect), name)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func putDeploymentSecretUpdateStmt(dialect string) string {
	switch dialect {
	case "mysql":
		return `UPDATE deployment_secrets SET ciphertext = ?, key_version = ?, disabled_at = NULL WHERE name = ?`
	case "mssql":
		return `UPDATE deployment_secrets SET ciphertext = @p1, key_version = @p2, disabled_at = NULL WHERE name = @p3`
	default:
		return `UPDATE deployment_secrets SET ciphertext = $1, key_version = $2, disabled_at = NULL, updated_at = now() WHERE name = $3`
	}
}

func putDeploymentSecretInsertStmt(dialect string) string {
	switch dialect {
	case "mysql":
		return `INSERT INTO deployment_secrets (name, ciphertext, key_version) VALUES (?, ?, ?)`
	case "mssql":
		return `INSERT INTO deployment_secrets (name, ciphertext, key_version) VALUES (@p1, @p2, @p3)`
	default:
		return `INSERT INTO deployment_secrets (name, ciphertext, key_version) VALUES ($1, $2, $3)`
	}
}

func getDeploymentSecretStmt(dialect string) string {
	switch dialect {
	case "mysql":
		return `SELECT ciphertext, key_version FROM deployment_secrets WHERE name = ? AND disabled_at IS NULL`
	case "mssql":
		return `SELECT ciphertext, key_version FROM deployment_secrets WHERE name = @p1 AND disabled_at IS NULL`
	default:
		return `SELECT ciphertext, key_version FROM deployment_secrets WHERE name = $1 AND disabled_at IS NULL`
	}
}

func deploymentSecretMetaStmt(dialect string) string {
	switch dialect {
	case "mysql":
		return `SELECT disabled_at FROM deployment_secrets WHERE name = ?`
	case "mssql":
		return `SELECT disabled_at FROM deployment_secrets WHERE name = @p1`
	default:
		return `SELECT disabled_at FROM deployment_secrets WHERE name = $1`
	}
}

func retireDeploymentSecretStmt(dialect string) string {
	switch dialect {
	case "mysql":
		return `UPDATE deployment_secrets SET disabled_at = NOW(6) WHERE name = ? AND disabled_at IS NULL`
	case "mssql":
		return `UPDATE deployment_secrets SET disabled_at = SYSDATETIMEOFFSET() WHERE name = @p1 AND disabled_at IS NULL`
	default:
		return `UPDATE deployment_secrets SET disabled_at = now() WHERE name = $1 AND disabled_at IS NULL`
	}
}

// DeploymentSecretReseal is the report from ResealDeploymentSecrets, mirroring
// SecretReseal in shape (see that type's doc comment for why every field
// exists even at zero).
type DeploymentSecretReseal struct {
	Rows       int
	Current    int
	Resealed   int
	Changed    int
	Unreadable []UnreadableDeploymentSecret
}

// UnreadableDeploymentSecret names a row ResealDeploymentSecrets could not
// open: a name, the key_version it is sealed under, and why.
type UnreadableDeploymentSecret struct {
	Name       string
	KeyVersion int
	Reason     string
}

// Converged reports whether a second run would have nothing left to do.
func (r DeploymentSecretReseal) Converged() bool { return len(r.Unreadable) == 0 && r.Changed == 0 }

// ResealDeploymentSecrets re-encrypts every deployment secret not already
// sealed under the current key, mirroring ResealSecrets: row by row, online,
// with the same read-verify-conditional-write shape (see ResealSecrets' doc
// comment for why each step is there -- it applies identically here, minus
// the per-tenant loop this store has no need for).
func (s *DeploymentSecretStore) ResealDeploymentSecrets(ctx context.Context, dryRun bool) (DeploymentSecretReseal, error) {
	var res DeploymentSecretReseal
	if s == nil || s.db == nil {
		return res, ErrNoDeploymentSecretDB
	}
	if s.ring == nil {
		return res, ErrNoSecretMasterKey
	}
	cur := s.ring.current.Version

	type stored struct {
		name       string
		ciphertext string
		keyVersion int
	}
	var rows []stored
	rs, err := s.db.QueryContext(ctx, listDeploymentSecretsForResealStmt(s.dialect))
	if err != nil {
		return res, fmt.Errorf("read deployment secrets: %w", err)
	}
	for rs.Next() {
		var r stored
		if err := rs.Scan(&r.name, &r.ciphertext, &r.keyVersion); err != nil {
			_ = rs.Close()
			return res, fmt.Errorf("scan deployment secret: %w", err)
		}
		rows = append(rows, r)
	}
	if err := rs.Err(); err != nil {
		_ = rs.Close()
		return res, err
	}
	_ = rs.Close()

	for _, r := range rows {
		res.Rows++
		if r.keyVersion == cur {
			res.Current++
			continue
		}
		plaintext, err := s.open(r.name, r.ciphertext, r.keyVersion)
		if err != nil {
			res.Unreadable = append(res.Unreadable,
				UnreadableDeploymentSecret{Name: r.name, KeyVersion: r.keyVersion, Reason: err.Error()})
			continue
		}
		next, err := s.seal(r.name, plaintext)
		if err != nil {
			return res, fmt.Errorf("re-seal %q: %w", r.name, err)
		}
		back, err := s.open(r.name, next, cur)
		if err != nil {
			return res, fmt.Errorf("re-sealed %q does not open: %w", r.name, err)
		}
		if back != plaintext {
			return res, fmt.Errorf("re-sealed %q does not round-trip; nothing further was written", r.name)
		}
		if dryRun {
			res.Resealed++
			continue
		}
		out, err := s.db.ExecContext(ctx, resealDeploymentSecretStmt(s.dialect),
			next, cur, r.name, r.keyVersion, r.ciphertext)
		if err != nil {
			return res, fmt.Errorf("write re-sealed %q: %w", r.name, err)
		}
		affected, err := out.RowsAffected()
		if err != nil {
			return res, err
		}
		if affected == 0 {
			res.Changed++
		} else {
			res.Resealed++
		}
	}
	sort.Slice(res.Unreadable, func(i, j int) bool { return res.Unreadable[i].Name < res.Unreadable[j].Name })
	return res, nil
}

func listDeploymentSecretsForResealStmt(dialect string) string {
	return `SELECT name, ciphertext, key_version FROM deployment_secrets ORDER BY name`
}

// resealDeploymentSecretStmt is the conditional write -- the compare-and-swap
// resealSecretStmt uses, minus the tenant predicate. See that function's doc
// comment for why the last two predicates are not optional.
func resealDeploymentSecretStmt(dialect string) string {
	switch dialect {
	case "mysql":
		return `UPDATE deployment_secrets SET ciphertext = ?, key_version = ? WHERE name = ? AND key_version = ? AND ciphertext = ?`
	case "mssql":
		return `UPDATE deployment_secrets SET ciphertext = @p1, key_version = @p2 WHERE name = @p3 AND key_version = @p4 AND ciphertext = @p5`
	default:
		return `UPDATE deployment_secrets SET ciphertext = $1, key_version = $2 WHERE name = $3 AND key_version = $4 AND ciphertext = $5`
	}
}
