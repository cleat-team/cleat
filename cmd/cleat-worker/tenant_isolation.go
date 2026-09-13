package main

// --tenant-isolation and its secret. cleat#1307.
//
// The repo owner's decision on the open question was REFUSE TO BOOT: a worker
// started with --tenant-isolation=role and no usable secret must not start.
// Starting would mean every tenant pool fails at first use -- one failure per
// workflow, arriving minutes later, on the path that was supposed to be
// carrying the isolation -- and a worker that is up and cannot do its job is a
// worse failure than one that refused.
//
// WORKSTREAM.md gives cmd/cleat-worker/ to WS-3, and its rule for another
// stream adding here is to say why in the comment. The reason: the mechanism
// this flag switches on -- plugin.TenantPools, admin.create_tenant_role and
// the HMAC derivation -- is WS-2's, landed over cleat#1350 and cleat#1361, and
// every one of those pieces was UNREACHABLE until something parsed a flag.
// Leaving the flag to another stream would leave a tenant-isolation mechanism
// shipped and unwired, which is the state cleat#1307 was filed about.

import (
	"encoding/base64"
	"fmt"
	"os"
	"strings"

	"github.com/cleat-team/cleat/plugin"
)

// tenantIsolationMode is the validated form of --tenant-isolation.
type tenantIsolationMode string

const (
	// isolationRLS is what production does today: set_config per transaction
	// on the shared owner pool.
	isolationRLS tenantIsolationMode = "rls"
	// isolationRole opens a pool per tenant authenticating as that tenant's
	// PostgreSQL login role.
	isolationRole tenantIsolationMode = "role"
)

// resolveTenantIsolation validates the flags and loads the derivation key.
//
// Returns the mode and, for isolationRole, the secret. Every failure is an
// error rather than a fallback: silently degrading "role" to "rls" would leave
// an operator believing they had per-tenant credentials when they had the
// shared owner pool, which is the same silent-downgrade shape as the
// owner-pool fallback removed from TenantPools.For.
func resolveTenantIsolation(mode, secretFile, driver string) (tenantIsolationMode, []byte, error) {
	switch tenantIsolationMode(mode) {
	case isolationRLS:
		// A secret with rls is a misconfiguration worth naming rather than
		// ignoring: whoever set it expected role-per-tenant isolation.
		if secretFile != "" {
			return "", nil, fmt.Errorf(
				"--tenant-role-secret-file is set but --tenant-isolation is %q. The secret is "+
					"only used by --tenant-isolation=role; leaving it here would look like "+
					"per-tenant credentials were in force when they are not", mode)
		}
		return isolationRLS, nil, nil

	case isolationRole:
		// PostgreSQL only, and refused rather than skipped. plugin.TenantPools
		// is PostgreSQL-only in its implementation -- it opens
		// sql.Open("postgres", ...) against a libpq keyword DSN -- and MySQL is
		// single-tenant by decision (tiers.yaml). Accepting the flag on another
		// dialect would hand those workers a postgres connection builder.
		if driver != "postgres" {
			return "", nil, fmt.Errorf(
				"--tenant-isolation=role requires --driver=postgres, got %q. Role-per-tenant "+
					"isolation is built on PostgreSQL login roles; MySQL is single-tenant by "+
					"decision and SQL Server uses session context", driver)
		}
		if secretFile == "" {
			return "", nil, fmt.Errorf(
				"--tenant-isolation=role requires --tenant-role-secret-file. Without the key " +
					"no tenant password can be derived, so every tenant pool would fail at " +
					"first use rather than at startup")
		}
		raw, err := os.ReadFile(secretFile)
		if err != nil {
			return "", nil, fmt.Errorf("read --tenant-role-secret-file: %w", err)
		}
		secret, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
		if err != nil {
			return "", nil, fmt.Errorf(
				"--tenant-role-secret-file is not valid base64: %w. Generate one with "+
					"`head -c 32 /dev/urandom | base64`", err)
		}
		// Length checked HERE as well as in TenantRolePassword, because the
		// two failures land in different places. There it is one workflow's
		// error; here it is a refusal to start, which is what an operator can
		// act on. A short key still produces a plausible 64-character password,
		// so nothing downstream would look wrong.
		if len(secret) < plugin.TenantRoleSecretMinBytes {
			return "", nil, fmt.Errorf(
				"--tenant-role-secret-file decodes to %d bytes; at least %d are required. "+
					"A shorter key still derives plausible-looking passwords, so this cannot "+
					"be detected later", len(secret), plugin.TenantRoleSecretMinBytes)
		}
		return isolationRole, secret, nil

	default:
		return "", nil, fmt.Errorf(
			"--tenant-isolation=%q is not recognised; want 'rls' or 'role'", mode)
	}
}
