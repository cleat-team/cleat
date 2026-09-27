package upgrade

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// TestThisPackageNeverTakesTheSharedDatabase fails if any test here acquires
// the shared database instead of this suite's own.
//
// The comment on testDB explains why that matters; this exists because a
// comment cannot fail. cleat#1281 was a single `testutil.TestDB` call in a
// package that ALTERs three core tables, and the cost was paid in `engine`, a
// package with no connection to this one: a local full suite passed, then
// failed on the next run, naming a documentation file that was correct.
// Nothing in CI could see it, because every job gets a fresh database.
//
// It parses rather than greps deliberately, and this file is the reason:
//
//	grep -c "testutil\.TestDB" tests/upgrade/*.go
//	shared_database_test.go:3   schema_migration_test.go:0
//
// Every occurrence in the package is prose, in the guard written to forbid it,
// and there are no calls at all. A text search cannot tell a call from a
// sentence about one, so it reports this file -- which contains no Go
// statement naming TestDB -- and stays silent about the file that would.
//
// (I wrote that as "the testDB comment above contains the string" before
// running the command. It does not.)
func TestThisPackageNeverTakesTheSharedDatabase(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading the package directory: %v", err)
	}

	fset := token.NewFileSet()
	var offenders []string
	suiteCalls := 0
	parsed := 0

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		file, err := parser.ParseFile(fset, e.Name(), nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", e.Name(), err)
		}
		parsed++

		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != "testutil" {
				return true
			}
			switch sel.Sel.Name {
			case "TestDB":
				offenders = append(offenders,
					fset.Position(call.Pos()).String())
			case "SuiteTestDB":
				suiteCalls++
			}
			return true
		})
	}

	// Vacuity: a scan that parsed nothing, or found no database call at all,
	// reports a clean package for the same reason a correct one does. Both
	// halves are needed -- parsing every file in an empty directory succeeds.
	//
	// The second condition is "NEITHER kind of call", not "no SuiteTestDB
	// call", and the difference is not pedantic: written the narrow way this
	// fired FIRST when the defect was restored, so the falsification went red
	// with "the scan is looking in the wrong place" -- blaming the scan, at the
	// one moment the scan was working and had the offending line in hand. The
	// prose here said "neither" while the code said "no SuiteTestDB". Caught
	// because a falsification has to redden for the REASON you expect, not
	// merely redden.
	if parsed == 0 {
		t.Fatal("parsed no .go files -- this scanned nothing, it did not pass")
	}
	if suiteCalls == 0 && len(offenders) == 0 {
		t.Fatalf("parsed %d files and found no testutil database call of either "+
			"kind; this suite takes a database, so the scan is looking in the "+
			"wrong place rather than reporting a clean package", parsed)
	}

	if len(offenders) > 0 {
		t.Errorf("this package ALTERs workflow_defs, workflow_instances and "+
			"event_history and never drops the columns, so it must not open "+
			"the shared database. testutil.TestDB called at:\n  %s\n\n"+
			"Use testutil.SuiteTestDB(t, \"upgrade\") -- see testDB in "+
			"schema_migration_test.go and cleat#1281.",
			strings.Join(offenders, "\n  "))
	}
}
