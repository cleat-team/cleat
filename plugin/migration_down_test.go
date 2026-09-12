package plugin

// cleat#1290. Migration.Down was populated by 18 plugins across 29 sites,
// asserted non-empty by 14 of their test suites across 21 assertions, and read
// by nothing. These tests are the caller's contract.
//
// THE TWO REFUSALS ARE THE POINT, not the happy path. Reversing some of a
// plugin's migrations and not the rest leaves a schema that is neither the old
// shape nor the new one, and no later run can tell which half it is looking
// at. So both checks run BEFORE any statement executes, and each test below
// asserts that nothing was touched when the check fires -- which is the part a
// "returns an error" assertion would miss.

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"

	_ "github.com/lib/pq"
)

// downTestDB returns an EMPTY database of its own, via the package's existing
// newPluginScratchDB. Two reasons that is better than a shared one:
//
//   - No skip of my own. newPluginScratchDB already gates on "was PostgreSQL
//     requested" and FATALS on configured-but-unreachable, which is what
//     scripts/check-skips.sh asks for -- a second env check here would be a
//     new conditional skip guarding the same thing.
//   - These tests CREATE and DROP tables. On a shared database a failure
//     leaves debris that makes the next run's precondition lie.
func downTestDB(t *testing.T, name string) *sql.DB {
	t.Helper()
	// LOWERCASED. PostgreSQL folds an unquoted identifier, so
	// CREATE DATABASE cleat_down_ReversesNewestFirst creates
	// cleat_down_reversesnewestfirst -- and connecting to the mixed-case name
	// then fails with 3D000 "database does not exist", which reads as the
	// create having failed rather than as a case difference.
	db := newPluginScratchDB(t, strings.ToLower(name))
	// THE CORE SCHEMA IS REQUIRED, not incidental. A migration declaring
	// TenantScoped makes applyTenantScoping create a policy calling
	// cleat.tenant_row_is_visible, so an empty database fails with
	//
	//	pq: schema "cleat" does not exist (3F000)
	//
	// which reads as a fixture problem rather than a missing precondition.
	testutil.SetupFullSchema(t, db, testutil.DialectPostgres)
	if _, err := db.Exec(createPluginMigrationsTableSQL(DialectPostgres)); err != nil {
		t.Fatalf("create plugin_migrations: %v", err)
	}
	return db
}

func loaded(name string, migs ...Migration) *LoadedPlugin {
	// Healthy: true is required -- RunMigrations skips an unhealthy plugin
	// (migration.go:308), so without it the fixture applies nothing and every
	// assertion below would measure an empty database. The precondition check
	// in the first test is what caught that.
	return &LoadedPlugin{Healthy: true, Plugin: &testMigrationPlugin{
		info:       PluginInfo{Name: name, Version: "1.0.0"},
		migrations: migs,
	}}
}

// tableExists is the observation that separates "refused" from "partly done".
//
// RESOLVED BY search_path, NOT BY current_schema(). The first version asked
//
//	WHERE table_schema = current_schema()
//
// and passed locally while failing CI's Tier 1 gate with "PRECONDITION FAILED:
// the migrations did not create their tables" -- my own assertion, reporting
// that RunMigrations had run without error and created nothing findable.
//
// It had created them, in `public`. RunMigrations pins `SET search_path =
// <schema>, pg_temp` on ITS OWN connection (schema defaults to public, see
// pluginMigrationSession), and plugin DDL is unqualified, so that pin decides
// where the tables land. This query runs on a POOL connection, whose
// current_schema() is the first entry of its own search_path -- `"$user",
// public`. Locally the role is postgres and no schema of that name exists, so
// current_schema() falls through to public and the two agree by accident. In
// CI the role is `cleat` against a database where 001 created a schema called
// `cleat`, so current_schema() returns `cleat` and the tables are invisible.
//
// That is the same call-time-versus-migration-time trap cleat#1287 records on
// admin.create_tenant_role, arriving in a test rather than in a function.
// to_regclass resolves the unqualified name the way an unqualified query would,
// which is the question this test actually means to ask.
func tableExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var found bool
	if err := db.QueryRow(`SELECT to_regclass($1) IS NOT NULL`, name).Scan(&found); err != nil {
		t.Fatalf("check table %s: %v", name, err)
	}
	return found
}

func TestRunDownMigrationsReversesNewestFirst(t *testing.T) {
	db := downTestDB(t, "cleat_down_"+t.Name()[len("TestRunDownMigrations"):])
	defer db.Close()
	ctx := context.Background()

	p := loaded("down-order",
		Migration{Version: 1, Up: `CREATE TABLE down_one (id INT, tenant_id UUID)`, Down: `DROP TABLE down_one`,
			TenantScoped: []string{"down_one"}},
		Migration{Version: 2, Up: `CREATE TABLE down_two (id INT, tenant_id UUID)`, Down: `DROP TABLE down_two`,
			TenantScoped: []string{"down_two"}},
	)

	if err := RunMigrations(ctx, db, DialectPostgres, nil, []*LoadedPlugin{p}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !tableExists(t, db, "down_one") || !tableExists(t, db, "down_two") {
		t.Fatal("PRECONDITION FAILED: the migrations did not create their tables, so the " +
			"reversal below would report success against nothing")
	}

	res, err := RunDownMigrations(ctx, db, DialectPostgres, p, []*LoadedPlugin{p})
	if err != nil {
		t.Fatalf("reverse: %v", err)
	}

	// Newest first: 2 before 1, so a migration is undone before whatever it
	// was built on top of.
	if len(res.Reversed) != 2 || res.Reversed[0] != 2 || res.Reversed[1] != 1 {
		t.Errorf("reversed %v, want [2 1] -- applied order backwards", res.Reversed)
	}
	if tableExists(t, db, "down_one") || tableExists(t, db, "down_two") {
		t.Error("a table survived the reversal; the Down SQL did not run")
	}
	// Un-tracked, so the migration can be applied again. A Down that ran while
	// its tracking row survived would make the migration un-re-appliable.
	var tracked int
	if err := db.QueryRow(`SELECT count(*) FROM plugin_migrations WHERE plugin_name = $1`,
		"down-order").Scan(&tracked); err != nil {
		t.Fatalf("count tracking rows: %v", err)
	}
	if tracked != 0 {
		t.Errorf("%d tracking rows survive; the migration cannot be re-applied", tracked)
	}
}

// A GAP IS A REFUSAL, and nothing is touched.
func TestRunDownMigrationsRefusesWhenAnAppliedVersionHasNoDown(t *testing.T) {
	db := downTestDB(t, "cleat_down_"+t.Name()[len("TestRunDownMigrations"):])
	defer db.Close()
	ctx := context.Background()

	p := loaded("down-gap",
		Migration{Version: 1, Up: `CREATE TABLE down_gap_one (id INT)`, Down: ``},
		Migration{Version: 2, Up: `CREATE TABLE down_gap_two (id INT)`, Down: `DROP TABLE down_gap_two`},
	)

	if err := RunMigrations(ctx, db, DialectPostgres, nil, []*LoadedPlugin{p}); err != nil {
		t.Fatalf("apply: %v", err)
	}

	_, err := RunDownMigrations(ctx, db, DialectPostgres, p, []*LoadedPlugin{p})
	if err == nil {
		t.Fatal("reversing a plugin with a Down-less applied version returned no error")
	}

	// THE ASSERTION THAT MATTERS. Version 2 HAS a Down, so a check that ran
	// per-migration instead of up front would have dropped down_gap_two before
	// discovering version 1's gap -- leaving a schema that is neither shape.
	if !tableExists(t, db, "down_gap_two") {
		t.Error("down_gap_two was dropped despite the refusal.\n\n" +
			"The gap check must run before ANY statement: a partial teardown leaves a " +
			"schema no later run can identify (cleat#1290).")
	}
	if !tableExists(t, db, "down_gap_one") {
		t.Error("down_gap_one was dropped despite the refusal")
	}
}

// A SHARED TABLE IS A REFUSAL, for the same reason and also nothing is touched.
func TestRunDownMigrationsRefusesATableAnotherPluginAlsoDeclares(t *testing.T) {
	db := downTestDB(t, "cleat_down_"+t.Name()[len("TestRunDownMigrations"):])
	defer db.Close()
	ctx := context.Background()

	mine := loaded("down-mine",
		Migration{Version: 1, Up: `CREATE TABLE down_shared (id INT, tenant_id UUID)`,
			Down: `DROP TABLE down_shared`, TenantScoped: []string{"down_shared"}},
	)
	theirs := loaded("down-theirs",
		Migration{Version: 1, Up: `SELECT 1`, Down: `SELECT 1`,
			TenantScoped: []string{"down_shared"}},
	)

	if err := RunMigrations(ctx, db, DialectPostgres, nil, []*LoadedPlugin{mine}); err != nil {
		t.Fatalf("apply: %v", err)
	}

	_, err := RunDownMigrations(ctx, db, DialectPostgres, mine, []*LoadedPlugin{mine, theirs})
	if err == nil {
		t.Fatal("reversing a plugin whose table another plugin also declares returned no error")
	}
	if !tableExists(t, db, "down_shared") {
		t.Error("down_shared was dropped despite the refusal.\n\n" +
			"Plugin tables share one flat namespace (cleat#1288), so dropping a table two " +
			"plugins declare would take the other plugin's rows with it.")
	}

	// The negative control: without the colliding plugin in the set, the same
	// reversal succeeds. Without it, a function that refused everything would
	// pass both refusal tests.
	if _, err := RunDownMigrations(ctx, db, DialectPostgres, mine, []*LoadedPlugin{mine}); err != nil {
		t.Fatalf("reversal refused with no collision present: %v", err)
	}
	if tableExists(t, db, "down_shared") {
		t.Error("down_shared survived a reversal that reported success")
	}
}

// Nothing applied is not an error, and must not be reported as work done.
func TestRunDownMigrationsOnAnUnappliedPluginIsANoop(t *testing.T) {
	db := downTestDB(t, "cleat_down_noop")
	defer db.Close()

	p := loaded("down-never-applied",
		Migration{Version: 1, Up: `CREATE TABLE down_never (id INT)`, Down: `DROP TABLE down_never`},
	)
	res, err := RunDownMigrations(context.Background(), db, DialectPostgres, p, nil)
	if err != nil {
		t.Fatalf("reversing an unapplied plugin errored: %v", err)
	}
	if len(res.Reversed) != 0 {
		t.Errorf("reported %v reversed for a plugin that was never applied", res.Reversed)
	}
}
