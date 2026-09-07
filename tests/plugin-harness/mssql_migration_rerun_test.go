package pluginharness

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/plugin"
)

// TestReapplyingTheCoreMigrationsLeavesTheTenantPoliciesStanding is the
// regression test for a defect that could only ever bite a developer, never
// CI.
//
// migrations/mssql/001_schema.sql drops the seven tenant SECURITY POLICYs at
// the top and recreates them at the bottom, and its header states the one
// condition that makes that safe: the migration runner wraps the file in a
// transaction, so "any tool that treats GO as a real batch separator and
// autocommits each batch" breaks it. RunCoreMigrations was that tool.
//
// It is permanent rather than a window, because 001 cannot re-run at all once
// migration 031 has been applied: 031 adds TenantFilter_Promises, which 001
// predates and so does not drop, and which is schemabound to
// dbo.fn_tenant_filter -- so the CREATE OR ALTER FUNCTION fails and the seven
// drops before it stay committed. Measured 2026-09-06 against a freshly
// migrated database: 9 security policies before this suite's MSSQL arm, 2
// after, the two survivors being exactly the two 001 does not know to drop.
//
// # Why CI could not see it
//
// plugin-harness-ci.yml points CLEAT_TEST_MSSQL at `database=master` on a
// fresh SQL Server container, so the migrations are applied exactly once and
// there is never a second application to fail. Every developer following
// CLAUDE.md and setting all three DSNs has the opposite: a long-lived database
// that already carries the schema.
//
// # The shipped count, not a before/after delta
//
// The first version of this test measured the policy count after one
// application and compared it to the count after a second, on the database the
// DSN named. Backing the transaction out left it GREEN, because on an
// already-migrated database the FIRST application is the damaging one: the
// count was 2 both times and the test compared 2 against 2. The `before == 0`
// vacuity guard did not fire, because 2 is not 0.
//
// Two changes, and both were needed. The comparison is against an absolute --
// the set of policies the migrations bind, parsed from the migrations -- which
// both applications must leave standing. And the database is a scratch one
// this test creates, so the starting state is known rather than inherited.
func TestReapplyingTheCoreMigrationsLeavesTheTenantPoliciesStanding(t *testing.T) {
	connStr := os.Getenv("CLEAT_TEST_MSSQL")
	if connStr == "" {
		t.Skip("CLEAT_TEST_MSSQL not set, skipping SQL Server test")
	}
	if testing.Short() {
		t.Skip("Skipping database test in short mode")
	}

	ctx := context.Background()

	// Its own database, not the one the DSN names.
	//
	// This test applies the core migrations, which is exactly what
	// TestPluginCalls_MultiDB/mssql does -- and Go runs tests in file order,
	// so `mssql_migration_rerun_test.go` runs before `multi_db_plugin_test.go`.
	// Sharing a database would leave that test facing an already-migrated one,
	// turning ITS first application into a re-application and failing it in CI,
	// where today it starts from a fresh container and passes. A test that
	// deliberately migrates twice must not do it where anything else is
	// looking.
	db, dbName := openScratchMSSQLDatabase(t, ctx, connStr)

	// Fatal, not Skip. migrations/mssql/ is a tracked directory of this repo,
	// so its absence is never a legitimate environmental precondition -- it
	// means coreMigrationDir's relative-path search has stopped finding it, and
	// a skip there would silently retire this guard. See scripts/check-skips.sh
	// case (c).
	dir := coreMigrationDir(t, plugin.DialectMSSQL)
	if dir == "" {
		t.Fatal("no MSSQL migration directory found: coreMigrationDir searches " +
			"../../migrations/mssql and migrations/mssql relative to the test's " +
			"working directory, and migrations/mssql is tracked in this repo")
	}
	want := shippedSecurityPolicies(t, dir)

	// First application. The scratch database is empty, so this one succeeds
	// and creates the policies; the assertion below is a precondition rather
	// than the subject. It is a check and not an assumption because the whole
	// point of the second application is that it must not be measured against
	// a database that is already stripped -- which is precisely how the first
	// version of this test passed its own falsification.
	_ = runCoreMigrations(ctx, db, plugin.DialectMSSQL, dbName, dir)

	if got := securityPolicyNames(t, db); !equalStrings(got, want) {
		t.Fatalf("after applying the core migrations the database carries %d of the "+
			"%d tenant security policies the migrations bind.\n\n"+
			"  shipped: %v\n  present: %v\n\n"+
			"migrations/mssql/001_schema.sql drops the seven base policies at the "+
			"top and recreates them at the bottom, which is only safe inside one "+
			"transaction -- read its header. If this database was already stripped "+
			"before this run, the migration runner cannot heal it (it records 001 "+
			"as applied regardless of what happened to the objects it created):\n\n"+
			"    DROP DATABASE cleat; CREATE DATABASE cleat;\n\n"+
			"See IMPROVEMENT-PLAN 2.71 and engine/testutil's requireMSSQLPoliciesIntact.",
			len(got), len(want), want, got)
	}

	// Second application. It is EXPECTED to fail against a database that
	// already carries migration 031 -- see the doc comment. The assertion is
	// about what it leaves behind, not whether it succeeds.
	reErr := runCoreMigrations(ctx, db, plugin.DialectMSSQL, dbName, dir)

	if got := securityPolicyNames(t, db); !equalStrings(got, want) {
		t.Fatalf("RE-APPLYING the core migrations left %d of the %d tenant security "+
			"policies standing.\n\n  shipped: %v\n  present: %v\n\n"+
			"Every tenant-scoped MSSQL test that runs after this one is now running "+
			"with no RLS backstop. See the doc comment on this test and on "+
			"runCoreMigrations.\n\nre-application returned: %v",
			len(got), len(want), want, got, reErr)
	}
}

// mssqlSecurityPolicyRe matches a CREATE SECURITY POLICY at the start of a
// line.
//
// The anchor is load-bearing and is not a style choice. Without it the pattern
// also matches migrations/mssql/001_schema.sql's own header, which discusses
// the statement in prose -- an unanchored scan over migrations/mssql/*.sql
// returns 16 distinct "names", 7 of them fragments of English sentences. A
// declaration can only appear at a declaration site; prose about one cannot.
var mssqlSecurityPolicyRe = regexp.MustCompile(`(?m)^CREATE SECURITY POLICY dbo\.(\w+)`)

// shippedSecurityPolicies is the set of tenant security policies the shipped
// MSSQL migrations create, read from the migrations rather than listed here.
//
// A literal list would be a second copy of something the migrations already
// state, and it would go stale in the direction that hides the defect: a
// policy dropped from a migration AND from the list agrees with itself.
func shippedSecurityPolicies(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	seen := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("reading %s: %v", e.Name(), err)
		}
		for _, m := range mssqlSecurityPolicyRe.FindAllStringSubmatch(string(b), -1) {
			seen[m[1]] = true
		}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)

	// A pattern that matches nothing would make every assertion above pass
	// vacuously, which is the whole failure mode this test exists inside.
	// Nine when written; the bound is loose so adding a policy does not fail
	// here, and non-zero so losing the parse does.
	if len(out) < 5 {
		t.Fatalf("parsed only %d CREATE SECURITY POLICY statements out of %s; "+
			"there were 9 on 2026-09-06. Re-point mssqlSecurityPolicyRe rather "+
			"than lowering this bound.", len(out), dir)
	}
	return out
}

func securityPolicyNames(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM sys.security_policies ORDER BY name`)
	if err != nil {
		t.Fatalf("reading sys.security_policies: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scanning sys.security_policies: %v", err)
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterating sys.security_policies: %v", err)
	}
	sort.Strings(out)
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// openScratchMSSQLDatabase creates a database of its own from connStr's server
// and returns a pool pointed at it, dropping it when the test ends.
//
// OpenTestDB's MSSQL arm creates a SCHEMA inside whatever database the DSN
// names -- unlike its MySQL arm, which creates a database. That is fine for
// tests that only add tables, and wrong for this one, which applies the core
// migrations into dbo.
func openScratchMSSQLDatabase(t *testing.T, ctx context.Context, connStr string) (*sql.DB, string) {
	t.Helper()

	name := "cleat_migration_rerun_test"

	admin, err := sql.Open("sqlserver", connStr)
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	defer admin.Close()
	if err := admin.PingContext(ctx); err != nil {
		t.Fatalf("configured SQL Server at %s is unreachable: %v", redactMSSQLDSN(connStr), err)
	}

	// DROP first: a previous run killed part-way through leaves this behind,
	// and a leftover carries the schema, which is the state this test must not
	// start from -- see the doc comment on the first assertion.
	if _, err := admin.ExecContext(ctx, dropScratchSQL(name)); err != nil {
		t.Fatalf("drop scratch database %s: %v", name, err)
	}
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create scratch database %s: %v", name, err)
	}
	t.Cleanup(func() {
		cleanup, err := sql.Open("sqlserver", connStr)
		if err != nil {
			return
		}
		defer cleanup.Close()
		_, _ = cleanup.ExecContext(context.Background(), dropScratchSQL(name))
	})

	scratchDSN, err := swapMSSQLDatabase(connStr, name)
	if err != nil {
		t.Fatalf("derive scratch DSN: %v", err)
	}
	db, err := sql.Open("sqlserver", scratchDSN)
	if err != nil {
		t.Fatalf("open scratch database: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db, name
}

// dropScratchSQL drops a database, evicting anything still connected to it.
// A plain DROP DATABASE blocks behind an open pool.
func dropScratchSQL(name string) string {
	return "IF DB_ID('" + name + "') IS NOT NULL BEGIN " +
		"ALTER DATABASE " + name + " SET SINGLE_USER WITH ROLLBACK IMMEDIATE; " +
		"DROP DATABASE " + name + "; END"
}

// swapMSSQLDatabase returns dsn with its `database` query parameter replaced.
func swapMSSQLDatabase(dsn, name string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("database", name)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// redactMSSQLDSN strips the password so a DSN can appear in failure output.
func redactMSSQLDSN(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil || u.User == nil {
		return "the configured SQL Server"
	}
	u.User = url.User(u.User.Username())
	return u.String()
}
