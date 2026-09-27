package crash

import (
	"database/sql"
	"testing"
	"time"
)

// migrateTargetDatabase is this test's own database, separate from
// crashDatabase. See the note on why in the test itself.
const migrateTargetDatabase = "cleat_crash_migrate_target"

// TestWorkerMigratesTheDatabaseItServes is the regression test for a defect in
// this harness rather than in the engine: startWorker passed ownerDSN() as
// --migrate-db, and ownerDSN() names the *base* database (`cleat`), not the
// cleat_crash database the worker serves from.
//
// cmd/cleat-worker opens --migrate-db as a separate connection and runs both
// migration.Runner and plugin.RunMigrations against it (main.go, "On a separate
// connection when --migrate-db is set"). So every worker this suite started
// migrated the shared database -- the one ensureCrashDatabase exists to keep
// this suite out of -- and migrated nothing in the one every assertion here
// reads.
//
// Measured 2026-08-31 on the unfixed harness, after a single worker start:
//
//	cleat        schema_migrations present, 14 rows; plugin_migrations present
//	cleat_crash  neither table; its 14 tables came from ownerDB's own loop
//
// Nothing failed, because the two routes happened to produce the same 14
// tables. That is what makes it worth a test rather than a comment: the
// symptom of the two routes disagreeing would not have been "the worker cannot
// migrate", it would have been an unrelated assertion failing somewhere in this
// suite, against a schema no one had thought about.
//
// Asserted through plugin_migrations rather than by inspecting the flags,
// because the flag is not the property -- what matters is that the migration
// the worker performs at boot lands where the worker reads. plugin_migrations
// is the right witness because *only* a worker creates it: ownerDBFor runs
// migration.Runner, which creates schema_migrations, so schema_migrations
// cannot tell the two databases apart.
//
// It runs in a database of its own because that witness is created once and
// then stays created. Sharing crashDatabase, this test passed alone and failed
// in the suite -- the workers started by the four tests above it had already
// made plugin_migrations, so there was nothing left to observe. A test whose
// result depends on what ran before it is not measuring what it claims to.
func TestWorkerMigratesTheDatabaseItServes(t *testing.T) {
	// Dropped first, not merely created-if-absent. The witness survives the
	// run that produced it, so on a developer's machine the second invocation
	// of this test would find plugin_migrations already there and have nothing
	// left to observe. ownerDBFor recreates and migrates it below.
	dropDatabaseNamed(t, migrateTargetDatabase)

	db := ownerDBFor(t, migrateTargetDatabase)
	defer db.Close()

	// Not a Skip, and it should now be unreachable. If the witness is present
	// anyway, this test cannot distinguish a worker that migrated the right
	// database from one that did not, and reporting that as a pass is the
	// failure mode this whole suite is written against (§2.12).
	if hasTable(t, db, "plugin_migrations") {
		t.Fatalf("plugin_migrations exists in %s immediately after it was dropped and "+
			"rebuilt, so this test cannot tell whether the worker created it.\n\n"+
			"Nothing in this harness creates that table. Either the drop above did not "+
			"take effect, or something other than a cleat-worker is writing to %s.",
			migrateTargetDatabase, migrateTargetDatabase)
	}

	suffix := uniqueSuffix()
	taskQueue := "queue-migrate-target-" + suffix
	deployFixture(t, db, taskQueue)
	bin := buildWorker(t)
	svc := newChargeService(t)
	w := startWorkerOn(t, migrateTargetDatabase, bin, taskQueue, svc.srv.URL)

	// Polled, not read once. startWorker's doc comment says it "waits until it
	// claims work or the start budget expires"; it does not -- it calls
	// cmd.Start() and returns. The first draft of this test read the table 4ms
	// later and failed against the *fixed* harness, with an empty worker log
	// because the worker had not written anything yet. Every other assertion in
	// this suite reaches the database through awaitTerminal, which polls, so
	// nothing had ever depended on that sentence being true.
	deadline := time.Now().Add(startBudget)
	migrated := false
	for time.Now().Before(deadline) {
		if hasTable(t, db, "plugin_migrations") {
			migrated = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	if !migrated {
		t.Errorf("the worker did not run its migrations against %s, the database it "+
			"serves from.\n\n"+
			"cmd/cleat-worker runs migration.Runner and plugin.RunMigrations against "+
			"--migrate-db. If that flag names a different database than --db, the "+
			"worker migrates one database and reads another, and this suite's "+
			"isolation from the shared `cleat` database is defeated on every start.\n"+
			"--- worker log ---\n%s", migrateTargetDatabase, w.output())
	}
}

// dropDatabaseNamed drops a database if it exists, terminating any connections
// still open to it.
//
// WITH (FORCE) rather than a t.Fatalf on "database is being accessed by other
// users": a worker killed by a previous test's cleanup can still be holding a
// connection for a moment, and failing the run for that would be a flake, not a
// finding. PostgreSQL 13+; the repo's floor is 14 (see docs/deployment.md).
func dropDatabaseNamed(t *testing.T, dbName string) {
	t.Helper()
	if !validDatabaseName.MatchString(dbName) {
		t.Fatalf("database name %q must match %s -- it is interpolated into "+
			"DROP DATABASE, which cannot be parameterised", dbName, validDatabaseName)
	}
	base := ownerDSN()
	admin, err := sql.Open("postgres", base)
	if err != nil {
		t.Fatalf("opening %s: %v", redact(base), err)
	}
	defer admin.Close()
	if err := admin.Ping(); err != nil {
		t.Fatalf("database unreachable at %s: %v", redact(base), err)
	}
	if _, err := admin.Exec(`DROP DATABASE IF EXISTS ` + dbName + ` WITH (FORCE)`); err != nil {
		t.Fatalf("dropping the %s database: %v", dbName, err)
	}
}

// hasTable reports whether an unqualified table name resolves in db.
//
// to_regclass rather than information_schema.table_schema = current_schema():
// migrations/postgres/001_schema.sql creates a schema named `cleat`, so a
// connection whose search_path puts that first resolves unqualified names
// somewhere information_schema's equality test does not look. That exact
// mismatch made engine/testutil's cleanup silently delete nothing under the
// Cluster job's search_path (IMPROVEMENT-PLAN 2.60d, part 3).
func hasTable(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var present bool
	if err := db.QueryRow(`SELECT to_regclass($1) IS NOT NULL`, name).Scan(&present); err != nil {
		t.Fatalf("checking for the %s table: %v", name, err)
	}
	return present
}
