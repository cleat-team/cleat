package engine

// cleat#1990: plugin/tenant_role_password.go's doc comment claimed
// "ReconcileTenantRolePasswords exists for that [rotation] and is
// idempotent, so a worker boot after a key change repairs the set" while no
// such function existed anywhere in the tree. Measured before it did: after
// --tenant-role-secret-file's key changed, every existing tenant's pool
// failed to authenticate (28P01), because TenantPools.open derives a
// password under the NEW key while the role's actual PostgreSQL password is
// still the old one, and nothing revisits a tenant --create-tenant already
// provisioned.
//
// This is the database-backed proof for plugin.ReconcileTenantRolePasswords,
// mirroring TestATenantRoleSeesOnlyItsOwnRows in tenant_role_isolation_test.go:
// connect AS the tenant role with a real driver, because that is the property
// a reconciler exists to restore, not a row this test could read back and
// call equivalent.

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
)

func TestReconcileTenantRolePasswordsRestoresConnectivityAfterAKeyRotation(t *testing.T) {
	admin := testutil.TestDB(t, testutil.DialectPostgres)
	defer admin.Close()
	testutil.SetupFullSchema(t, admin, testutil.DialectPostgres)

	dsn := os.Getenv("CLEAT_TEST_POSTGRES")
	if dsn == "" {
		dsn = os.Getenv("CLEAT_TEST_DB")
	}
	if dsn == "" {
		t.Fatal("testutil.TestDB resolved a PostgreSQL connection but neither " +
			"CLEAT_TEST_POSTGRES nor CLEAT_TEST_DB is set; this test rewrites the DSN's " +
			"credential and has nothing to rewrite")
	}

	const (
		tenantA = "cccccccc-0000-0000-0000-00000000c001"
		tenantB = "dddddddd-0000-0000-0000-00000000d001"
	)
	t.Cleanup(func() {
		for _, id := range []string{tenantA, tenantB} {
			admin.Exec(`DELETE FROM admin.tenant_roles WHERE tenant_id = $1`, id)
			admin.Exec(`DELETE FROM admin.tenants WHERE tenant_id = $1`, id)
			admin.Exec(fmt.Sprintf(`DROP ROLE IF EXISTS %q`, tenantRoleName(id)))
		}
	})

	for _, id := range []string{tenantA, tenantB} {
		if _, err := admin.Exec(
			`INSERT INTO admin.tenants (tenant_id, name, display_name) VALUES ($1, $2, $2)
			 ON CONFLICT (tenant_id) DO NOTHING`, id, "t-"+id[:8]); err != nil {
			t.Fatalf("seed tenant %s: %v", id, err)
		}
	}

	keyOld := make([]byte, plugin.TenantRoleSecretMinBytes)
	keyNew := make([]byte, plugin.TenantRoleSecretMinBytes)
	for i := range keyOld {
		keyOld[i] = byte(i + 1)
		keyNew[i] = byte(i + 101) // disjoint from keyOld's byte range
	}

	// Provision both tenants under the OLD key, exactly as --create-tenant
	// would at the time each was onboarded.
	for _, id := range []string{tenantA, tenantB} {
		password, err := plugin.TenantRolePassword(keyOld, id)
		if err != nil {
			t.Fatalf("derive old password for %s: %v", id, err)
		}
		var role sql.NullString
		if err := admin.QueryRow(
			`SELECT admin.create_tenant_role($1::uuid, $2)`, id, password).Scan(&role); err != nil {
			t.Fatalf("create_tenant_role for %s: %v", id, err)
		}
		if !role.Valid {
			// Fatal, not Skip -- same reasoning as
			// TestATenantRoleSeesOnlyItsOwnRows: CI's PostgreSQL service is a
			// superuser, so skipping here would silently drop the only test of
			// this mechanism on exactly the deployments that can run it.
			t.Fatalf("admin.create_tenant_role returned NULL for %s: this connection cannot "+
				"CREATE ROLE. Point CLEAT_TEST_POSTGRES at a superuser or CREATEROLE connection.", id)
		}
	}

	connectAs := func(t *testing.T, id string, key []byte) error {
		t.Helper()
		password, err := plugin.TenantRolePassword(key, id)
		if err != nil {
			t.Fatalf("derive password for %s: %v", id, err)
		}
		role := tenantRoleName(id)
		tenantDB, err := sql.Open("postgres",
			testutil.TagPostgresDSN(rewriteDSNCredential(t, dsn, role, password)))
		if err != nil {
			t.Fatalf("open tenant connection for %s: %v", id, err)
		}
		defer tenantDB.Close()
		return tenantDB.Ping()
	}

	// BASELINE, before rotating: the old key still connects. Without this,
	// a broken derivation could make every case below fail for the wrong
	// reason and the test would still look like it proved something.
	for _, id := range []string{tenantA, tenantB} {
		if err := connectAs(t, id, keyOld); err != nil {
			t.Fatalf("baseline: tenant %s could not connect under the key it was provisioned "+
				"with: %v", id, err)
		}
	}

	// THE MEASUREMENT cleat#1990 asked for, reproduced as the regression's
	// negative control: a key rotation with no reconciliation breaks both
	// tenants' pools.
	for _, id := range []string{tenantA, tenantB} {
		if err := connectAs(t, id, keyNew); err == nil {
			t.Fatalf("tenant %s connected under the NEW key before reconciliation ran; "+
				"either the role's password already matched it, or this test seeded state "+
				"that does not model a rotation", id)
		}
	}

	// THE FIX. Idempotent by construction (see the function's doc comment):
	// called twice here, back to back, with no state in between that would
	// make a second call behave differently from the first.
	for i := 0; i < 2; i++ {
		n, err := plugin.ReconcileTenantRolePasswords(context.Background(), admin, keyNew)
		if err != nil {
			t.Fatalf("ReconcileTenantRolePasswords (call %d): %v", i, err)
		}
		if n < 2 {
			t.Fatalf("ReconcileTenantRolePasswords (call %d) reconciled %d roles, want at "+
				"least the 2 this test seeded", i, n)
		}
	}

	// After reconciliation, the NEW key connects for both tenants --
	for _, id := range []string{tenantA, tenantB} {
		if err := connectAs(t, id, keyNew); err != nil {
			t.Fatalf("tenant %s still cannot connect under the new key after "+
				"ReconcileTenantRolePasswords: %v", id, err)
		}
	}
	// -- and the OLD key no longer does, proving the role's actual password
	// changed rather than the new key coincidentally also working.
	for _, id := range []string{tenantA, tenantB} {
		if err := connectAs(t, id, keyOld); err == nil {
			t.Fatalf("tenant %s still connects under the OLD key after reconciliation; "+
				"the role's password was not actually altered", id)
		}
	}
}
