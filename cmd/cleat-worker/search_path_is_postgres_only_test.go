package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestSearchPathIsAppendedOnlyForPostgres.
//
// `search_path` is a PostgreSQL concept and --schema is documented as
// PostgreSQL-only, but two of the worker's three pools appended it regardless
// of --driver. Measured against live servers (cleat#1374):
//
//	root:...@tcp(...)/cleat?search_path=x   Error 1193: Unknown system variable
//	sqlserver://...?search_path=x            accepted and SILENTLY IGNORED
//
// The SQL Server row is the worse of the two. MySQL fails at the first query,
// which is loud and gets fixed. SQL Server accepts the parameter, ignores it,
// and leaves the flusher pool addressing dbo while the main pool addresses
// --schema -- two pools disagreeing about which tables they mean, with no
// error anywhere.
func TestSearchPathIsAppendedOnlyForPostgres(t *testing.T) {
	for _, tc := range []struct {
		driver string
		dsn    string
		want   bool // should search_path be appended?
	}{
		{"postgres", "postgres://u:p@h:5432/cleat?sslmode=disable", true},
		{"mysql", "root:p@tcp(127.0.0.1:3306)/cleat?parseTime=true", false},
		{"sqlserver", "sqlserver://sa:p@127.0.0.1:1433?database=cleat", false},
		{"mssql", "sqlserver://sa:p@127.0.0.1:1433?database=cleat", false},
	} {
		got := dsnWithSchema(tc.dsn, "pool_b", tc.driver)
		has := strings.Contains(got, "search_path=")
		if has != tc.want {
			t.Errorf("driver %q: search_path present = %v, want %v\n  dsn: %s",
				tc.driver, has, tc.want, got)
		}
		if !tc.want && got != tc.dsn {
			t.Errorf("driver %q: the DSN was modified at all:\n  before %s\n  after  %s",
				tc.driver, tc.dsn, got)
		}
	}
}

// TestTheDefaultSchemaNeverTouchesTheDSN is the control.
//
// Without it, the assertions above are satisfied by a function that never
// appends anything -- which would silently un-implement --schema on the one
// dialect that supports it.
func TestTheDefaultSchemaNeverTouchesTheDSN(t *testing.T) {
	const dsn = "postgres://u:p@h:5432/cleat?sslmode=disable"
	for _, schema := range []string{"", "public"} {
		if got := dsnWithSchema(dsn, schema, "postgres"); got != dsn {
			t.Errorf("schema %q modified the DSN: %s", schema, got)
		}
	}
	// And the positive half, so "never appends" cannot pass this file.
	if got := dsnWithSchema(dsn, "pool_b", "postgres"); !strings.Contains(got, "search_path=pool_b") {
		t.Errorf("a non-default schema on postgres did not reach the DSN: %s", got)
	}
}

// TestEveryDSNWithSchemaCallSitePassesTheDriver.
//
// The signature change is what makes the defect unwritable, but only while
// every call site passes the REAL driver. A call passing a literal "postgres"
// would compile, read plausibly, and restore the bug for the other two
// dialects -- so this asserts the argument is the flag rather than a constant.
//
// A source scan because the alternative is a runtime check that only fires on
// a worker actually started with MySQL and a non-default schema, which is the
// configuration nobody runs until a customer does.
func TestEveryDSNWithSchemaCallSitePassesTheDriver(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	calls := regexp.MustCompile(`dsnWithSchema\(([^)]*)\)`).FindAllStringSubmatch(string(src), -1)
	if len(calls) == 0 {
		t.Fatal("no dsnWithSchema call sites found in main.go: the scan would pass vacuously")
	}
	for _, c := range calls {
		args := c[1]
		if !strings.Contains(args, "*driver") {
			t.Errorf("a dsnWithSchema call does not pass *driver:\n  dsnWithSchema(%s)\n\n"+
				"Passing a literal would compile and read plausibly while restoring "+
				"cleat#1374 for MySQL and SQL Server.", args)
		}
	}
}
