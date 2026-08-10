package migration

// Split out of runner_test.go when the rest of this package's tests moved to
// an external `migration_test` package (see runner_test.go's header for why).
// What stays here is exactly what needs unexported access: migrationsRoot is
// shared with split_test.go (which stays internal because splitSQL and
// isAllComments are unexported), and TestTrackingTable_QualifiedOnlyForPostgres
// pokes Runner's unexported dialect field directly.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

// TestDialectValuesMatchEngine pins the claim Dialect's doc comment makes:
// migration.Dialect and engine.Dialect are declared separately (to avoid an
// import cycle -- see Dialect's doc comment) but must carry identical string
// values, because callers convert between them with a plain string
// conversion (cmd/cleat-worker/main.go: migration.Dialect(factory.Dialect()))
// rather than a lookup table. If the two ever diverge, that conversion starts
// silently sending the wrong dialect's migrations directory suffix.
func TestDialectValuesMatchEngine(t *testing.T) {
	cases := []struct {
		migration Dialect
		engine    engine.Dialect
	}{
		{DialectPostgres, engine.DialectPostgres},
		{DialectMySQL, engine.DialectMySQL},
		{DialectMSSQL, engine.DialectMSSQL},
	}
	for _, c := range cases {
		if string(c.migration) != string(c.engine) {
			t.Errorf("migration.%s = %q, engine.%s = %q: values must match for the string "+
				"conversion at cmd/cleat-worker/main.go's NewRunner call sites to be correct",
				c.migration, c.migration, c.engine, c.engine)
		}
	}
}

// migrationsRoot returns the repo's migrations/ directory (the runner appends
// the dialect subdirectory itself).
func migrationsRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd() // .../migration
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	root := filepath.Join(wd, "..", "migrations")
	if _, err := os.Stat(filepath.Join(root, "postgres")); err != nil {
		t.Fatalf("migrations/postgres not found at %s: %v", root, err)
	}
	return root
}

// TestTrackingTable_QualifiedOnlyForPostgres guards the dialect split: MySQL
// and SQL Server have no schema of that shape and would reject "public.".
func TestTrackingTable_QualifiedOnlyForPostgres(t *testing.T) {
	cases := []struct {
		dialect Dialect
		want    string
	}{
		{DialectPostgres, "public.schema_migrations"},
		{DialectMySQL, "schema_migrations"},
		{DialectMSSQL, "schema_migrations"},
	}
	for _, c := range cases {
		r := &Runner{dialect: c.dialect}
		if got := r.trackingTable(); got != c.want {
			t.Errorf("dialect %s: trackingTable() = %q, want %q", c.dialect, got, c.want)
		}
	}
}
