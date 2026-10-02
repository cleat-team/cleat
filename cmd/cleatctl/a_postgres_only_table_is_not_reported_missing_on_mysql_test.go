package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
)

// cleat#2324: coreTables carries payload_encryption_ever_enabled, which
// exists only in migrations/postgres -- encrypt-sensitive-payloads is a
// PostgreSQL-only feature, so MySQL and SQL Server have no migration defining
// this table at all.
//
// TestQualifiedTableMatchesEachDialectsMigrations's own postgresOnlyTables
// exemption (coretables.go) kept that test green, but runCheckDB
// (checkdb.go) loops over the SAME coreTables slice to build its
// operator-facing "TABLES:" report and, before this fix, had no idea
// postgresOnlyTables existed. cleat-review2 found this by building cleatctl
// and running check-db -v against a real, freshly-migrated MySQL database
// (#2924 round 7): a genuinely healthy deployment was reported as
// "1 missing / MISSING: payload_encryption_ever_enabled" -- a false alarm on
// every MySQL and SQL Server deployment once coreTables carries any
// postgres-only entry.
//
// MySQL only, deliberately, same reasoning as
// TestCheckDBCountsTheTenantsDatabaseOnMySQL beside it: there is nothing to
// check on PostgreSQL, where the table is real and
// TestRunCheckDB_Tables_AllAccessible already covers it, and a passing SQL
// Server subtest here would only assert the absence of a mechanism SQL
// Server shares with MySQL rather than exercise anything different.
func TestCheckDBDoesNotReportAPostgresOnlyTableMissingOnMySQL(t *testing.T) {
	db := testutil.TestDB(t, testutil.DialectMySQL)
	ctx := context.Background()

	dsn := os.Getenv("CLEAT_TEST_MYSQL")
	if dsn == "" {
		t.Fatalf("CLEAT_TEST_MYSQL is unset while testutil.TestDB returned a MySQL database, " +
			"so it connected via its built-in default and this test has no DSN to pass to " +
			"runCheckDB. Set CLEAT_TEST_MYSQL to the same server.")
	}

	// The REAL shipped migrations, the exact path cmd/cleat-worker/main.go
	// runs at boot -- not a hand-written schema that could happen to agree
	// with coreTables by coincidence.
	testutil.SetupMySQLFullSchema(t, db)

	want := 0
	for _, core := range coreTables {
		if !postgresOnlyTables[core] {
			want++
		}
	}

	stdout, _ := withExitPanicOutput(t, func() {
		runCheckDB(ctx, db, dialectMySQL, dsn, []string{"--verbose"})
	})

	if strings.Contains(stdout, "MISSING: payload_encryption_ever_enabled") {
		t.Fatalf("check-db reported the PostgreSQL-only table payload_encryption_ever_enabled "+
			"as missing on a genuinely healthy, freshly-migrated MySQL database -- this is "+
			"exactly the false alarm cleat-review2 found, reappearing:\n%s", stdout)
	}
	wantLine := fmt.Sprintf("all %d accessible", want)
	if !strings.Contains(stdout, wantLine) {
		t.Fatalf("expected %q in check-db's TABLES line (every non-postgres-only core table "+
			"present, nothing reported missing), got:\n%s", wantLine, stdout)
	}
}
