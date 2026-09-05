package engine

import (
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
)

// The Postgres cleanup list named `tenant_api_keys` unqualified, and the table
// is `admin.tenant_api_keys`. Nothing failed, because the name resolves through
// `search_path` and what it resolves to depends on the database:
//
//	5434 (role postgres, no stray)   to_regclass -> NULL
//	5432 (role postgres, stray)      to_regclass -> public.tenant_api_keys
//	5433 (role cleat,    stray)      to_regclass -> cleat.tenant_api_keys
//
// So on a clean database existingTables drops the entry and cleanup skips it,
// and on a database carrying a stray copy the DELETE lands on the decoy while
// `admin.tenant_api_keys` keeps its rows. Measured 2026-09-04 across all three
// local instances: on none of them was the real table ever cleared.
//
// That is exactly the accumulation IMPROVEMENT-PLAN 2.60d added this entry to
// stop -- the entry has been in the list, and inert, ever since.
//
// TestCleanupTableListsAgree could not catch it: normaliseTable strips the
// schema qualifier before comparing, so "admin.tenant_api_keys" on SQL Server
// and "tenant_api_keys" on Postgres compare equal. The one axis that mattered
// is the one the comparison normalises away.
//
// This test asserts the observable thing -- a row in admin.tenant_api_keys does
// not survive cleanup -- rather than asserting how the list is spelled, so it
// stays honest if the fix is implemented some other way.
func TestCleanupPostgresTestDataClearsAdminTenantAPIKeys(t *testing.T) {
	db := testutil.TestDB(t, testutil.DialectPostgres)
	defer db.Close()
	testutil.SetupFullSchema(t, db, testutil.DialectPostgres)
	testutil.CleanupPostgresTestData(t, db)

	const tenantID = "d1d1d1d1-d1d1-4d1d-d1d1-d1d1d1d1d1d1"
	if _, err := db.Exec(
		`INSERT INTO admin.tenants (tenant_id, name) VALUES ($1, $2)
		 ON CONFLICT (tenant_id) DO NOTHING`,
		tenantID, "cleanup-api-key-fixture"); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM admin.tenant_api_keys WHERE tenant_id = $1`, tenantID)
		_, _ = db.Exec(`DELETE FROM admin.tenants WHERE tenant_id = $1`, tenantID)
	})

	if _, err := db.Exec(
		`INSERT INTO admin.tenant_api_keys (tenant_id, key_hash, description)
		 VALUES ($1, $2, $3)`,
		tenantID, []byte("cleanup-fixture-hash"), "2.60d fixture"); err != nil {
		t.Fatalf("seed api key: %v", err)
	}

	var before int
	if err := db.QueryRow(`SELECT count(*) FROM admin.tenant_api_keys`).Scan(&before); err != nil {
		t.Fatalf("count before: %v", err)
	}
	if before == 0 {
		t.Fatal("fixture did not land in admin.tenant_api_keys; the test would " +
			"pass vacuously")
	}

	testutil.CleanupPostgresTestData(t, db)

	var after int
	if err := db.QueryRow(`SELECT count(*) FROM admin.tenant_api_keys`).Scan(&after); err != nil {
		t.Fatalf("count after: %v", err)
	}
	if after != 0 {
		t.Fatalf("CleanupPostgresTestData left %d row(s) in admin.tenant_api_keys "+
			"(had %d before). The cleanup list's entry does not resolve to this "+
			"table, so the rows leak into every later test in the package. "+
			"See IMPROVEMENT-PLAN 2.60d.", after, before)
	}
}
