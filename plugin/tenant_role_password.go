package plugin

// Tenant role passwords are DERIVED, not stored. cleat#1307.
//
// admin.create_tenant_role used to generate a password with gen_random_bytes
// and INSERT it into admin.tenant_roles.password as plaintext, with no RLS on
// that table -- so a database backup contained every tenant's login credential,
// and the credential protecting tenant isolation sat inside the database it was
// protecting.
//
// Deriving removes the at-rest secret entirely: the worker holds one key, and a
// tenant's password is HMAC-SHA256(key, tenant_id). Nothing per-tenant is
// persisted, so there is nothing per-tenant to leak, rotate individually, or
// forget to delete when a tenant is dropped.
//
// WHY HMAC RATHER THAN A HASH OF key||tenant_id. HMAC is built for exactly this
// -- keyed derivation from a fixed-length secret and arbitrary-length label --
// and is not vulnerable to the length-extension shape that a bare
// sha256(key || data) has. The label is the tenant UUID, which is unique per
// tenant by construction, so no additional salt is needed.
//
// WHAT THIS DOES NOT PROTECT AGAINST, stated because the change is a security
// one and the boundary should be explicit:
//
//   - An attacker who obtains the worker's key can derive EVERY tenant's
//     password. That is the trade: one secret to protect instead of N, held in
//     the worker rather than in the database. It is the same trade
//     --encryption-key-file already makes for payload encryption.
//   - PostgreSQL has no way to bind a parameter in DDL, so `ALTER ROLE ...
//     PASSWORD` interpolates a literal. With log_statement=all or
//     log_min_duration_statement low enough, the derived password reaches the
//     server log. That was equally true of the generated passwords this
//     replaces; it is not made worse here, and it is a reason to keep DDL
//     statement logging off on a database that provisions tenant roles.
//   - Rotation is all-or-nothing: a new key changes every tenant's password, so
//     every role needs an ALTER. ReconcileTenantRolePasswords exists for that
//     and is idempotent, so a worker boot after a key change repairs the set.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// TenantRoleSecretMinBytes is the shortest key TenantRolePassword accepts.
//
// 32, matching --encryption-key-file's requirement, and enforced rather than
// documented: a short key here silently weakens every tenant's password, and
// the failure is invisible because the derivation still produces a plausible
// 64-character hex string.
const TenantRoleSecretMinBytes = 32

// ErrTenantRoleSecretTooShort is returned by TenantRolePassword when the key is
// shorter than TenantRoleSecretMinBytes.
var ErrTenantRoleSecretTooShort = errors.New(
	"plugin: tenant role secret must be at least 32 bytes")

// TenantRolePassword derives the login password for a tenant's PostgreSQL role.
//
// The result is 64 lowercase hex characters (256 bits). It is deterministic:
// the same key and tenant always produce the same password, which is what lets
// any worker open a tenant pool without reading a stored credential.
func TenantRolePassword(secret []byte, tenantID string) (string, error) {
	if len(secret) < TenantRoleSecretMinBytes {
		return "", fmt.Errorf("%w, got %d", ErrTenantRoleSecretTooShort, len(secret))
	}
	// Normalised, because the same tenant must derive the same password however
	// its UUID was spelled on the way in. A mixed-case or padded id would
	// otherwise produce a different password and an authentication failure that
	// looks like a wrong key rather than a formatting difference.
	id := strings.ToLower(strings.TrimSpace(tenantID))
	if id == "" {
		return "", errors.New("plugin: tenant role password: empty tenant id")
	}
	mac := hmac.New(sha256.New, secret)
	// Domain-separated: the same key could reasonably be reused for another
	// derivation later, and a bare tenant id as the only input would make the
	// two interchangeable.
	mac.Write([]byte("cleat/tenant-role-password/v1\x00"))
	mac.Write([]byte(id))
	return hex.EncodeToString(mac.Sum(nil)), nil
}

// TenantRoleName returns the PostgreSQL role name for a tenant.
//
// Mirrors admin.create_tenant_role's convention. Exported so callers derive the
// name from one place rather than each rebuilding the replace(...) expression --
// a mismatch here produces "role does not exist" at connect time, which reads
// as a provisioning failure rather than a naming one.
func TenantRoleName(tenantID string) string {
	return "cleat_tenant_" + strings.ReplaceAll(
		strings.ToLower(strings.TrimSpace(tenantID)), "-", "_")
}

// ReconcileTenantRolePasswords re-derives every already-provisioned tenant's
// role password under secret and re-ALTERs it via admin.create_tenant_role,
// which is idempotent for an existing role -- migrations/postgres/064 ALTERs
// rather than CREATEs once pg_roles already has the role name. cleat#1990.
//
// Measured before this existed: after --tenant-role-secret-file's key
// changes, EVERY existing tenant's pool fails to authenticate --
// TenantPools.open derives a password under the new key while the role's
// actual PostgreSQL password is still the old one. Provisioning a NEW tenant
// (--create-tenant) was unaffected, because it derives and ALTERs under
// whichever key that run has; nothing revisited a tenant provisioned
// earlier. Calling this once at boot, for every row in admin.tenant_roles,
// closes that gap.
//
// UNCONDITIONAL ON PURPOSE: this runs every boot under --tenant-isolation=role,
// whether or not the key actually changed. An ALTER ROLE to the password a
// role already has is a cheap no-op query, and running it regardless means a
// rotation's correctness does not depend on an operator remembering a
// separate reconciliation step -- the same "boot repairs the set" property
// the doc comment on this file already promised. ALTER ROLE does not
// terminate sessions connected under the old password; only a connection
// attempt made AFTER this call needs the new one, so the outage window a
// rotation costs is bounded by how long this call takes, not by how long a
// previously-opened pool happens to stay idle.
//
// Returns the number of roles reconciled. Every failure here is returned
// rather than logged and continued past: a worker that could not confirm a
// tenant's password matches its own key must not start believing it does,
// matching resolveTenantIsolation's refuse-to-boot stance
// (cmd/cleat-worker/tenant_isolation.go) for the same flag.
func ReconcileTenantRolePasswords(ctx context.Context, db *sql.DB, secret []byte) (int, error) {
	rows, err := db.QueryContext(ctx, `SELECT tenant_id, role_name FROM admin.tenant_roles`)
	if err != nil {
		return 0, fmt.Errorf("reconcile tenant role passwords: list roles: %w", err)
	}
	type provisioned struct{ tenantID, roleName string }
	var all []provisioned
	for rows.Next() {
		var p provisioned
		if err := rows.Scan(&p.tenantID, &p.roleName); err != nil {
			rows.Close()
			return 0, fmt.Errorf("reconcile tenant role passwords: scan: %w", err)
		}
		all = append(all, p)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("reconcile tenant role passwords: %w", err)
	}
	rows.Close()

	for _, p := range all {
		password, err := TenantRolePassword(secret, p.tenantID)
		if err != nil {
			return 0, fmt.Errorf("reconcile tenant role passwords: derive for tenant %s: %w", p.tenantID, err)
		}
		var roleName sql.NullString
		if err := db.QueryRowContext(ctx,
			`SELECT admin.create_tenant_role($1::uuid, $2)`, p.tenantID, password,
		).Scan(&roleName); err != nil {
			return 0, fmt.Errorf("reconcile tenant role passwords: alter role for tenant %s: %w", p.tenantID, err)
		}
		if !roleName.Valid {
			// create_tenant_role RAISEs a warning and returns NULL rather than
			// erroring when the connection cannot ALTER ROLE -- the same
			// single-tenant-mode escape hatch --create-tenant already handles.
			// Fatal here too: a worker configured for role isolation that
			// cannot confirm the password it just derived is what its tenant's
			// role actually holds must not start.
			return 0, fmt.Errorf(
				"reconcile tenant role passwords: tenant %s's role %q was not altered: "+
					"this connection cannot ALTER ROLE (needs a superuser or CREATEROLE connection)",
				p.tenantID, p.roleName)
		}
	}
	return len(all), nil
}
