package catalogdiff

// cleat#2059. The differential verification harness has to be shown catching
// a deliberately wrong compaction before it can be trusted to clear a real
// one -- "a harness that has only ever passed is a claim, not a check"
// (docs/schema-partitioning-design.md). This file carries the two mandated
// known-positives:
//
//   - TestDiffCatchesAWrongRoutineBody: substitute an old finalize_workflow_status
//     body for the current one and confirm Diff reports it.
//   - TestDiffCatchesADroppedForce: drop FORCE ROW LEVEL SECURITY on one table
//     and confirm Diff reports it.
//
// Both are proven together with TestSnapshotIsIdenticalForTwoBuildsOfTheSameChain,
// the negative control: two INDEPENDENTLY BUILT databases from the identical
// migration chain must diff empty, or the known-positives above would be
// meaningless -- a harness that always reports SOME difference "catches"
// every known-positive for the wrong reason.
//
// The compacted migrations/*/001-003 files this harness exists to verify
// don't exist yet (cleat#2059's work plan step 4, gated behind a migration
// freeze) -- so every test here builds its known-positive by applying the
// full, current migrations/postgres/ chain and then mutating ON TOP of it,
// rather than by comparing against the not-yet-built compacted files. That
// is deliberate: it proves the harness itself works, independent of when the
// compaction lands.

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/lib/pq"

	"github.com/cleat-team/cleat/migration"
)

// postgresAdminDSN returns the admin connection string this package's tests
// build scratch databases against, or "" if neither env var is set. Both
// names, for the same reason cmd/cleat-worker's deployDialect.admin() reads
// both: the PostgreSQL-only ci.yml job sets only CLEAT_TEST_DB.
func postgresAdminDSN() string {
	if v := os.Getenv("CLEAT_TEST_POSTGRES"); v != "" {
		return v
	}
	return os.Getenv("CLEAT_TEST_DB")
}

// postgresMigrationsDir returns the parent of migrations/postgres, which is
// migration.NewRunner's own convention: it reads <dir>/<dialect>/*.sql.
func postgresMigrationsDir(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd() // .../migration/catalogdiff
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	dir := filepath.Join(wd, "..", "..", "migrations")
	if _, err := os.Stat(filepath.Join(dir, "postgres")); err != nil {
		t.Fatalf("migrations/postgres not reachable from migration/catalogdiff: %v", err)
	}
	return dir
}

// scratchPostgresDB creates a fresh, uniquely-named, empty PostgreSQL
// database, applies the full current migrations/postgres/ chain to it via
// migration.NewRunner -- the same runner a real deploy uses, not a
// hand-rolled substitute -- and returns a handle. The database is dropped in
// t.Cleanup.
func scratchPostgresDB(t *testing.T) *sql.DB {
	t.Helper()
	admin := postgresAdminDSN()
	adb, err := sql.Open("postgres", admin)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { adb.Close() })
	if err := adb.Ping(); err != nil {
		t.Fatalf("CLEAT_TEST_POSTGRES/CLEAT_TEST_DB is set but unreachable: %v", err)
	}

	name := fmt.Sprintf("cleat_2059_catalogdiff_%d", time.Now().UnixNano()%1_000_000_000)
	if _, err := adb.Exec(`CREATE DATABASE ` + name); err != nil {
		t.Fatalf("creating scratch database %s: %v", name, err)
	}
	t.Cleanup(func() {
		a, err := sql.Open("postgres", admin)
		if err != nil {
			return
		}
		defer a.Close()
		_, _ = a.Exec(`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()`, name)
		_, _ = a.Exec(`DROP DATABASE IF EXISTS ` + name)
	})

	u, err := url.Parse(admin)
	if err != nil {
		t.Fatalf("parsing admin DSN: %v", err)
	}
	u.Path = "/" + name
	db, err := sql.Open("postgres", u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatalf("connecting to freshly created %s: %v", name, err)
	}

	ctx := context.Background()
	if err := migration.NewRunner(db, migration.DialectPostgres, postgresMigrationsDir(t)).Run(ctx); err != nil {
		t.Fatalf("applying migrations/postgres/ to scratch database %s: %v", name, err)
	}
	return db
}

func snapshotOrFatal(t *testing.T, db *sql.DB) *Catalog {
	t.Helper()
	cat, err := Snapshot(context.Background(), db, migration.DialectPostgres)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	return cat
}

// TestSnapshotIsIdenticalForTwoBuildsOfTheSameChain is the negative control
// this harness needs before either known-positive below means anything: two
// SEPARATE scratch databases, each independently built from the identical
// migration chain, must diff empty. If they did not -- because, say, OIDs or
// map iteration order leaked into a definition -- every known-positive test
// would also fail, but for the wrong reason, and nobody would notice because
// "the test failed" is exactly what a known-positive expects.
func TestSnapshotIsIdenticalForTwoBuildsOfTheSameChain(t *testing.T) {
	if postgresAdminDSN() == "" {
		t.Skip("CLEAT_TEST_POSTGRES/CLEAT_TEST_DB not set, skipping")
	}
	a := snapshotOrFatal(t, scratchPostgresDB(t))
	b := snapshotOrFatal(t, scratchPostgresDB(t))

	if len(a.Tables) == 0 {
		t.Fatal("scratch database A has zero tables after applying the full migration chain -- Snapshot or the migration run is broken, this isn't a clean pass")
	}

	diff := Diff(a, b)
	if len(diff) != 0 {
		t.Fatalf("two independently-built databases from the identical migration chain differ (%d lines) -- the harness itself is unreliable:\n%s",
			len(diff), joinLines(diff))
	}
}

// TestSnapshotExcludesTheRunnersOwnBootstrapTable locks in the
// schemaMigrationsTable exclusion this package's Snapshot functions apply.
// Without it, TestSnapshotIsIdenticalForTwoBuildsOfTheSameChainMSSQL fails on
// every run -- see schemaMigrationsTable's doc comment for the measurement.
func TestSnapshotExcludesTheRunnersOwnBootstrapTable(t *testing.T) {
	if postgresAdminDSN() == "" {
		t.Skip("CLEAT_TEST_POSTGRES/CLEAT_TEST_DB not set, skipping")
	}
	cat := snapshotOrFatal(t, scratchPostgresDB(t))
	if _, present := cat.Tables["public."+schemaMigrationsTable]; present {
		t.Fatalf("Snapshot captured %s -- it must be excluded, see schemaMigrationsTable's doc comment", schemaMigrationsTable)
	}
}

// TestDiffCatchesAWrongRoutineBody is the design doc's first mandated
// known-positive: "substitute 003's finalize_workflow_status body for 053's
// and confirm it fails." migrations/postgres/003_procedures.sql's original
// body is no longer authoritative (10 migrations have redefined the routine
// since; see migrations/postgres/101_..., the current authoritative
// definition) -- so reapplying 003's own file on top of a fully-migrated
// database reintroduces exactly the old body as a live regression, which is
// the same shape of mistake a bad compaction would make: shipping an
// earlier, superseded definition.
func TestDiffCatchesAWrongRoutineBody(t *testing.T) {
	if postgresAdminDSN() == "" {
		t.Skip("CLEAT_TEST_POSTGRES/CLEAT_TEST_DB not set, skipping")
	}
	clean := scratchPostgresDB(t)
	cleanCat := snapshotOrFatal(t, clean)

	broken := scratchPostgresDB(t)
	oldBody, err := os.ReadFile(filepath.Join(postgresMigrationsDir(t), "postgres", "003_procedures.sql"))
	if err != nil {
		t.Fatalf("reading 003_procedures.sql: %v", err)
	}
	if _, err := broken.Exec(string(oldBody)); err != nil {
		t.Fatalf("reapplying 003_procedures.sql's superseded body: %v", err)
	}
	brokenCat := snapshotOrFatal(t, broken)

	diff := Diff(cleanCat, brokenCat)
	if len(diff) == 0 {
		t.Fatal("known-positive did not fire: reapplying 003_procedures.sql's superseded finalize_workflow_status body produced an EMPTY diff -- the harness cannot catch a wrong routine body, which is the design doc's first mandated known-positive")
	}
	if !anyLineContains(diff, "finalize_workflow_status") {
		t.Fatalf("diff is non-empty (%d lines) but none mentions finalize_workflow_status -- the known-positive fired for the wrong reason:\n%s",
			len(diff), joinLines(diff))
	}
}

// TestDiffCatchesADroppedForce is the design doc's second mandated
// known-positive. A table can carry an identical policy and differ only in
// whether FORCE is set -- invisible to a columns/indexes/policy-rows
// comparison, and exactly the difference between a policy that binds a
// table's owner and one the owner silently bypasses.
func TestDiffCatchesADroppedForce(t *testing.T) {
	if postgresAdminDSN() == "" {
		t.Skip("CLEAT_TEST_POSTGRES/CLEAT_TEST_DB not set, skipping")
	}
	clean := scratchPostgresDB(t)
	cleanCat := snapshotOrFatal(t, clean)

	broken := scratchPostgresDB(t)
	if _, err := broken.Exec(`ALTER TABLE workflow_instances NO FORCE ROW LEVEL SECURITY`); err != nil {
		t.Fatalf("dropping FORCE on workflow_instances: %v", err)
	}
	brokenCat := snapshotOrFatal(t, broken)

	diff := Diff(cleanCat, brokenCat)
	if len(diff) == 0 {
		t.Fatal("known-positive did not fire: dropping FORCE ROW LEVEL SECURITY on workflow_instances produced an EMPTY diff -- the harness cannot catch a dropped FORCE, which is the design doc's second mandated known-positive")
	}
	if !anyLineContains(diff, "ROWSECURITY") {
		t.Fatalf("diff is non-empty (%d lines) but none mentions ROWSECURITY -- the known-positive fired for the wrong reason:\n%s",
			len(diff), joinLines(diff))
	}
}

// TestNeitherScratchDatabaseContainsAPluginObject is the precondition check
// docs/schema-partitioning-design.md requires be run, not assumed, before
// any Diff between two databases is trusted: "Plugins run against neither
// side. This is a decision, and the harness must assert it rather than
// assume it." A freshly built scratch database that only ever had
// migration.NewRunner applied to it has never given plugin.RunMigrations a
// chance to run, so this is expected to pass on the ordinary path -- what it
// guards against is a FUTURE caller of this package building its "clean"
// side by wiring up a full worker (which does run plugin migrations at
// boot) rather than a bare Runner.
func TestNeitherScratchDatabaseContainsAPluginObject(t *testing.T) {
	if postgresAdminDSN() == "" {
		t.Skip("CLEAT_TEST_POSTGRES/CLEAT_TEST_DB not set, skipping")
	}
	db := scratchPostgresDB(t)
	if err := AssertNoPluginObjects(context.Background(), db, migration.DialectPostgres); err != nil {
		t.Fatalf("a scratch database built by migration.NewRunner alone should never contain a plugin object: %v", err)
	}

	// Known-positive for the precondition check itself: create the table
	// plugin.RunMigrations would have, and confirm the assertion now refuses.
	if _, err := db.Exec(`CREATE TABLE plugin_migrations (plugin_name TEXT)`); err != nil {
		t.Fatalf("creating a fake plugin_migrations table: %v", err)
	}
	if err := AssertNoPluginObjects(context.Background(), db, migration.DialectPostgres); err == nil {
		t.Fatal("AssertNoPluginObjects returned nil after plugin_migrations was created -- the precondition check cannot see the thing it exists to catch")
	}
}

func anyLineContains(lines []string, substr string) bool {
	for _, l := range lines {
		if strings.Contains(l, substr) {
			return true
		}
	}
	return false
}

func joinLines(lines []string) string {
	out := ""
	for _, l := range lines {
		out += "  " + l + "\n"
	}
	return out
}
