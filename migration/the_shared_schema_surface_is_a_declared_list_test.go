package migration_test

import (
	"context"
	"sort"
	"testing"

	"github.com/cleat-team/cleat/migration"
)

// The objects in `admin` and `cleat` are shared by every pool in a database,
// so this list is a declaration and not an inventory. cleat#1375.
//
// `--schema` gives each worker pool its own tables and its own
// schema_migrations, but `admin` and `cleat` are FIXED names: one copy per
// database. So a migration that adds a function there adds it for every pool,
// and a pool upgrading rewrites behaviour another pool is executing.
// cleat#1363 settled the product side -- multi-pool is a future feature, one
// DATABASE per pool -- and this guard exists to keep the shared surface from
// growing silently while that stays true, because the set is what has to be
// made per-pool if multi-pool is ever built.
//
// THE SET, NOT THE COUNT. Between #1375 being filed and this test being
// written, the admin routines went from six to seven. A count going 6 -> 7
// says something moved and not which thing, and a guard that only reported a
// number would have been satisfied by a rename.
var (
	sharedTables = []string{
		"admin.plugin_tables",
		"admin.tenant_api_keys",
		"admin.tenant_roles",
		"admin.tenants",
	}
	sharedRoutines = []string{
		"admin.claim_workflows",
		"admin.create_tenant_role",
		"admin.drop_tenant",
		"admin.get_due_schedules",
		"admin.grant_core_tables_to_tenant_role",
		"admin.grant_plugin_to_tenant",
		"admin.revoke_plugin_from_tenant",
		"cleat.assert_tenant_set",
		"cleat.tenant_row_is_visible",
	}
)

// Read from the CATALOG rather than by scanning the migration files, and that
// is deliberate. A source scan cannot see an object created through
// `EXECUTE format(...)` -- migration 066 creates one that way -- so the files
// are not the authority on what exists. The database is.
func TestTheSharedSchemaSurfaceIsADeclaredList(t *testing.T) {
	// ITS OWN DATABASE, NOT ITS OWN SCHEMA IN A SHARED ONE. cleat#1479, and
	// this test is the one that was actually caught doing it.
	//
	// The migration below rebinds the SHARED admin functions: 001 creates them
	// `SET search_path FROM CURRENT`, freezing the migrating pool's schema onto
	// them, and --schema does not move admin -- there is one copy per DATABASE.
	// So after this ran against the engine suite's database,
	//
	//	claim_workflows | {"search_path=shared_surface_1375, pg_temp"}
	//
	// and admin.claim_workflows looked for workflow_instances in a schema the
	// engine knows nothing about. It found none and returned an empty list WITH
	// NO ERROR, failing eight cross-tenant and RLS tests in ./engine/ as "the
	// rows were not there". That schema name is how the cause was identified.
	//
	// Measured: pristine database, the eight pass; one `go test ./migration/`;
	// the eight fail. Two ALTER FUNCTION statements and they pass again --
	// confirmed by repair, not by diagnosis alone.
	//
	// Nothing about what this test asserts needs a shared database: it migrates,
	// then reads the catalog for the objects the migration created.
	db := newScratchDB(t, "cleat_shared_surface_1375")

	const schema = "shared_surface_1375"
	if err := migration.NewRunner(db, migration.DialectPostgres, "../migrations").
		WithSchema(schema).Run(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	check := func(what, query string, want []string) {
		t.Helper()
		rows, err := db.Query(query)
		if err != nil {
			t.Fatalf("list %s: %v", what, err)
		}
		defer rows.Close()
		var got []string
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatalf("scan %s: %v", what, err)
			}
			got = append(got, s)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows %s: %v", what, err)
		}
		sort.Strings(got)

		// Vacuity guard. An empty result would agree with any declaration that
		// happened to be empty, and would also be what a wrong schema filter
		// or an unmigrated database produces.
		if len(got) == 0 {
			t.Fatalf("found no %s in admin/cleat at all -- the query or the migration run is "+
				"wrong, not the declaration", what)
		}

		added, removed := diffSets(got, want)
		for _, a := range added {
			t.Errorf("UNDECLARED shared %s: %s\n"+
				"    It lives in admin or cleat, so every pool in the database gets it and a "+
				"pool upgrading rewrites it for the others (cleat#1375). If that is intended, "+
				"add it to the list in this file in the same PR.", what, a)
		}
		for _, r := range removed {
			t.Errorf("DECLARED but missing shared %s: %s\n"+
				"    Dropping a shared object is the same hazard with the sign flipped -- "+
				"another pool may still be calling it. If the removal is intended, take it out "+
				"of the list here.", what, r)
		}
	}

	check("table", `SELECT table_schema||'.'||table_name FROM information_schema.tables
	                 WHERE table_schema IN ('admin','cleat')`, sharedTables)
	check("routine", `SELECT n.nspname||'.'||p.proname FROM pg_proc p
	                   JOIN pg_namespace n ON n.oid = p.pronamespace
	                  WHERE n.nspname IN ('admin','cleat')`, sharedRoutines)
}

// diffSets returns what is in got-but-not-want and want-but-not-got.
func diffSets(got, want []string) (added, removed []string) {
	inWant := map[string]bool{}
	for _, w := range want {
		inWant[w] = true
	}
	inGot := map[string]bool{}
	for _, g := range got {
		inGot[g] = true
		if !inWant[g] {
			added = append(added, g)
		}
	}
	for _, w := range want {
		if !inGot[w] {
			removed = append(removed, w)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	return
}
