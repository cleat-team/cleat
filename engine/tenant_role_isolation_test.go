package engine

// What plugin.TenantPools is FOR, demonstrated rather than described.
// cleat#1307.
//
// The design is defence in depth for multi-tenancy: a PostgreSQL login role per
// tenant, so isolation does not depend on the application remembering to assert
// who it is. TenantPools' own comment puts it as "the connection IS the tenant".
// admin.create_tenant_role is the other half -- it creates the role, gives it a
// tenant_<uuid> schema, and sets cleat.tenant_id as a ROLE DEFAULT so the RLS
// variable arrives with the credential.
//
// Production today is the weaker model: set_config('cleat.tenant_id', ...) per
// transaction on the owner pool. The app asserts its identity and RLS trusts
// the assertion, so a path that forgets the set_config is a cross-tenant read.
// Under the role model there is nothing to forget.
//
// THIS TEST IS THE KNOWN-POSITIVE FOR THE WHOLE FEATURE. Migration 064 widened
// the grant set from six tables to eleven, and a test that only checked grants
// would pass on a mechanism that did not isolate anything. So this connects AS
// the tenant role and asserts what it can see -- which is the property the
// grants exist to serve, and the one that decides whether wiring TenantPools up
// is worth doing.

import (
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
)

// tenantRoleName mirrors admin.create_tenant_role's convention. Duplicated from
// the SQL deliberately: if the convention changes, this test fails at connect
// time with an unmistakable "role does not exist" rather than silently testing
// nothing.
func tenantRoleName(tenantID string) string {
	return "cleat_tenant_" + strings.ReplaceAll(tenantID, "-", "_")
}

func TestATenantRoleSeesOnlyItsOwnRows(t *testing.T) {
	dsn := os.Getenv("CLEAT_TEST_POSTGRES")
	if dsn == "" {
		dsn = os.Getenv("CLEAT_TEST_DB")
	}
	if dsn == "" {
		t.Skip("CLEAT_TEST_POSTGRES is not set")
	}

	admin := testutil.TestDB(t, testutil.DialectPostgres)
	defer admin.Close()
	testutil.SetupFullSchema(t, admin, testutil.DialectPostgres)

	const (
		mine   = "aaaaaaaa-0000-0000-0000-00000000a001"
		theirs = "bbbbbbbb-0000-0000-0000-00000000b001"
	)
	t.Cleanup(func() {
		for _, id := range []string{mine, theirs} {
			admin.Exec(`DELETE FROM workflow_defs WHERE tenant_id = $1`, id)
			admin.Exec(`DELETE FROM admin.tenant_roles WHERE tenant_id = $1`, id)
			admin.Exec(`DELETE FROM admin.tenants WHERE tenant_id = $1`, id)
			admin.Exec(fmt.Sprintf(`DROP ROLE IF EXISTS %q`, tenantRoleName(id)))
		}
	})

	for _, id := range []string{mine, theirs} {
		if _, err := admin.Exec(
			`INSERT INTO admin.tenants (tenant_id, name, display_name) VALUES ($1, $2, $2)
			 ON CONFLICT (tenant_id) DO NOTHING`, id, "t-"+id[:8]); err != nil {
			t.Fatalf("seed tenant %s: %v", id, err)
		}
		// Deleted first and inserted with ON CONFLICT: this test runs against a
		// long-lived development database, and a previous failed run leaves its
		// rows behind. A bare INSERT fails on the second run with a primary-key
		// violation that has nothing to do with what is being measured.
		if _, err := admin.Exec(`DELETE FROM workflow_defs WHERE name = $1`, "def-"+id[:8]); err != nil {
			t.Fatalf("clear stale def for %s: %v", id, err)
		}
		if _, err := admin.Exec(
			`INSERT INTO workflow_defs (name, version, wasm_bytes, entry_points, abi_version, min_version, tenant_id)
			 VALUES ($1, 1, '\x0061736d', '{}', 1, 1, $2)
			 ON CONFLICT DO NOTHING`, "def-"+id[:8], id); err != nil {
			t.Fatalf("seed def for %s: %v", id, err)
		}
	}

	// Provision only the first tenant's role. create_tenant_role returns NULL
	// and RAISEs a warning when the connection cannot create roles, so a
	// non-superuser DSN produces a clear skip rather than a confusing failure.
	// The password is DERIVED and passed in, never stored (cleat#1307). This
	// test therefore proves the whole chain: derive, provision, re-derive,
	// authenticate. A stored password would prove only that the row round-trips.
	secret := make([]byte, plugin.TenantRoleSecretMinBytes)
	for i := range secret {
		secret[i] = byte(i + 1)
	}
	password, err := plugin.TenantRolePassword(secret, mine)
	if err != nil {
		t.Fatalf("derive the tenant password: %v", err)
	}

	var role sql.NullString
	if err := admin.QueryRow(
		`SELECT admin.create_tenant_role($1::uuid, $2)`, mine, password).Scan(&role); err != nil {
		t.Fatalf("create_tenant_role: %v", err)
	}
	if !role.Valid {
		t.Skip("this connection cannot CREATE ROLE (create_tenant_role returned NULL); " +
			"role-per-tenant isolation needs a superuser or CREATEROLE DSN")
	}

	// Re-derived rather than read back, because there is nothing to read back:
	// 064 drops admin.tenant_roles.password. Asserted, so that a future change
	// reintroducing a stored credential fails here rather than quietly working.
	reDerived, err := plugin.TenantRolePassword(secret, mine)
	if err != nil {
		t.Fatalf("re-derive: %v", err)
	}
	if reDerived != password {
		t.Fatalf("derivation is not deterministic: %q then %q", password, reDerived)
	}
	var storedCols int
	if err := admin.QueryRow(`SELECT count(*) FROM information_schema.columns
		WHERE table_schema = 'admin' AND table_name = 'tenant_roles'
		  AND column_name = 'password'`).Scan(&storedCols); err != nil {
		t.Fatalf("check for a stored password column: %v", err)
	}
	if storedCols != 0 {
		t.Error("admin.tenant_roles has a password column again.\n\n" +
			"Tenant passwords are derived from the worker's key so that no per-tenant " +
			"credential exists at rest; a column to put one in is how that comes back " +
			"(cleat#1307).")
	}

	// Connect AS the tenant. The DSN keeps the host/port/dbname of the admin
	// one and swaps the credential -- which is exactly what
	// plugin.TenantPools.For does with baseDSNFromURL's output.
	tenantDB, err := sql.Open("postgres", rewriteDSNCredential(t, dsn, role.String, reDerived))
	if err != nil {
		t.Fatalf("open tenant connection: %v", err)
	}
	defer tenantDB.Close()

	var who, tenantVar string
	if err := tenantDB.QueryRow(
		`SELECT current_user, current_setting('cleat.tenant_id', true)`).Scan(&who, &tenantVar); err != nil {
		t.Fatalf("query as the tenant role: %v", err)
	}

	if who != role.String {
		t.Errorf("connected as %q, want %q", who, role.String)
	}
	// THE POINT. No set_config ran on this connection: the RLS variable is a
	// role default applied at login, so the credential carries the identity.
	if tenantVar != mine {
		t.Fatalf("cleat.tenant_id on a fresh tenant connection is %q, want %q.\n\n"+
			"admin.create_tenant_role sets it with ALTER ROLE ... SET cleat.tenant_id. "+
			"If it does not arrive, the connection is not self-identifying and the whole "+
			"role-per-tenant model reduces to the set_config one (cleat#1307).", tenantVar, mine)
	}

	var visible int
	// Scoped to the two names this test seeded. Counting every row would make
	// the assertion depend on whatever else lives in a shared database -- and
	// would pass for the wrong reason on an empty one.
	if err := tenantDB.QueryRow(
		`SELECT count(*) FROM workflow_defs WHERE name IN ($1, $2)`,
		"def-"+mine[:8], "def-"+theirs[:8]).Scan(&visible); err != nil {
		t.Fatalf("count defs as the tenant role: %v", err)
	}
	if visible != 1 {
		t.Errorf("a tenant role sees %d workflow_defs rows; two tenants were seeded, so it "+
			"must see exactly its own 1. Seeing 2 means RLS is not scoping by the role's "+
			"cleat.tenant_id and the isolation is not there.", visible)
	}

	// And the withheld grant, asserted rather than assumed. Migration 064
	// deliberately does not grant tenant_settings writes: a run reads its own
	// limits and must not raise them.
	if _, err := tenantDB.Exec(
		`UPDATE tenant_settings SET wasm_wall_clock_ceiling_ms = 999999999 WHERE tenant_id = $1`, mine); err == nil {
		t.Error("a tenant role was able to UPDATE tenant_settings.\n\n" +
			"064 grants SELECT only, on purpose: raising your own limits is not a " +
			"tenant-level operation. A successful write here means the grant set is wider " +
			"than the migration's comment claims.")
	} else if !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("UPDATE tenant_settings failed for the wrong reason: %v\n\n"+
			"want a permission error; anything else means this assertion is not "+
			"measuring the grant", err)
	}
}

// rewriteDSNCredential swaps the user and password in a postgres:// URL,
// keeping everything else. Kept local to this test: it is not the production
// DSN builder, and production's own path is plugin.TenantPools.
func rewriteDSNCredential(t *testing.T, dsn, user, password string) string {
	t.Helper()
	const scheme = "postgres://"
	if !strings.HasPrefix(dsn, scheme) {
		t.Skipf("CLEAT_TEST_POSTGRES is not a postgres:// URL (%q); this test rewrites the "+
			"credential and cannot do so for a keyword DSN", dsn)
	}
	rest := dsn[len(scheme):]
	if at := strings.Index(rest, "@"); at >= 0 {
		rest = rest[at+1:]
	}
	return scheme + user + ":" + password + "@" + rest
}
