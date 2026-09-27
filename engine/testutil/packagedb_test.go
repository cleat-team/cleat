package testutil

import (
	"strings"
	"testing"
)

// IMPROVEMENT-PLAN 2.60d, isolation half.
//
// The cleanup helpers issue unqualified DELETE FROM across every table. That
// is correct inside a database only one suite uses and destructive in a shared
// one. These tests pin that SuiteTestDB really does hand back a separate
// database, because the failure it prevents is invisible: a suite that
// silently got the shared database back would pass everything here and delete
// another package's fixtures mid-run.

func TestSuiteTestDBIsNotTheSharedDatabase(t *testing.T) {
	suiteDB := SuiteTestDB(t, "testutil")

	var got string
	if err := suiteDB.QueryRow(`SELECT current_database()`).Scan(&got); err != nil {
		t.Fatalf("reading current_database: %v", err)
	}
	if want := suiteDatabaseName("testutil"); got != want {
		t.Fatalf("connected to %q, want %q", got, want)
	}

	// And it must differ from whatever the shared DSN points at -- the point
	// of the exercise, not merely that the name looks right.
	shared := TestDB(t, DialectPostgres)
	var sharedName string
	if err := shared.QueryRow(`SELECT current_database()`).Scan(&sharedName); err != nil {
		t.Fatalf("reading the shared database name: %v", err)
	}
	if got == sharedName {
		t.Fatalf("the suite database and the shared database are both %q; "+
			"this package's unqualified DELETEs would run against the same "+
			"tables the engine package is using", got)
	}
}

// TestSuiteTestDBHasTheMigratedSchema guards the other half. An empty database
// would also be "not the shared one", and would fail later and more
// confusingly than here.
func TestSuiteTestDBHasTheMigratedSchema(t *testing.T) {
	db := SuiteTestDB(t, "testutil")

	for _, table := range []string{"workflow_defs", "workflow_instances", "event_history", "concurrency_keys"} {
		var n int
		if err := db.QueryRow("SELECT count(*) FROM " + table).Scan(&n); err != nil {
			t.Errorf("table %s is missing from the suite database: %v", table, err)
		}
	}
}

func TestSuiteDatabaseNameIsRejectedWhenUnsafe(t *testing.T) {
	// The name is interpolated into CREATE DATABASE, which cannot be
	// parameterised, so the guard is the only thing between a caller and an
	// injected statement.
	for _, bad := range []string{"", "Testutil", "test-util", "test util", `x"; DROP DATABASE cleat; --`} {
		if validSuiteName.MatchString(bad) {
			t.Errorf("suite name %q was accepted; it is interpolated into CREATE DATABASE", bad)
		}
	}
	for _, ok := range []string{"testutil", "engine", "cleatctl", "a", "a_b_9"} {
		if !validSuiteName.MatchString(ok) {
			t.Errorf("suite name %q was rejected", ok)
		}
	}
}

func TestSwapDatabaseName(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"postgres://u:p@h:5432/cleat?sslmode=disable", "postgres://u:p@h:5432/cleat_test_x?sslmode=disable"},
		{"postgres://h:5432/cleat", "postgres://h:5432/cleat_test_x"},
		{"host=h port=5432 dbname=cleat sslmode=disable", "host=h port=5432 dbname=cleat_test_x sslmode=disable"},
		// Keyword form with no dbname at all: appending is the only correct
		// answer, and getting it wrong would silently reuse the default
		// database, which is the shared one.
		{"host=h port=5432 sslmode=disable", "host=h port=5432 sslmode=disable dbname=cleat_test_x"},
	}
	for _, c := range cases {
		got, err := swapDatabaseName(c.in, "cleat_test_x")
		if err != nil {
			t.Errorf("swapDatabaseName(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("swapDatabaseName(%q)\n  got  %s\n  want %s", c.in, got, c.want)
		}
		if !strings.Contains(got, "cleat_test_x") {
			t.Errorf("swapDatabaseName(%q) lost the database name: %s", c.in, got)
		}
	}
}
