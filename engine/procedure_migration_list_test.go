package engine

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The three *ProcedureMigrations lists tell the test harness which migration
// files to apply so that finalize_workflow_status exists. They are
// hand-maintained, and a missing entry fails in the worst available way: every
// test going through the backend's Setup keeps running against the LAST listed
// version of the procedure, so a change to it is not merely untested -- it is
// contradicted by a suite that still passes.
//
// That is not hypothetical. The query_state-on-suspension fix was written,
// applied to a real database and verified over the HTTP API, and its own engine
// test still failed, because the harness was applying 004 and stopping there.
// The first reading was "the fix does not work".
//
// This is the same shape as the ShellCheck path list in #768 and the several
// allowlists in this tree: a hand-maintained list of what to check, checked in
// only one direction, silently covering less than its name implies.

// procedureDDL matches a statement that DEFINES the routine, in any dialect.
//
// Comments are stripped before this runs, by the package's existing
// stripSQLComments (mssql_uuid_projection_test.go) rather than a second copy.
//
// Measured, because the honest answer matters more than the tidy one: removing
// the stripping today changes nothing. No current migration writes a CREATE
// statement inside a comment, and the pattern is specific enough that
// migrations/postgres/032 -- which discusses finalize_workflow_status at length
// in prose -- does not match either way. So the stripping is a precaution, not
// load-bearing.
//
// It is here because the precaution is cheap and the failure it prevents is
// one this repository has made four times: a scanner reading a sentence ABOUT a
// thing as the thing. Most recently the guard for #827 matched its own
// explanatory comment, so backing the fix out left the test green.
// TestTheScannerIgnoresSQLComments below exercises the stripping directly, so
// it is a tested property rather than an untested habit.
var procedureDDL = regexp.MustCompile(
	`(?i)CREATE\s+(OR\s+REPLACE\s+|OR\s+ALTER\s+)?(FUNCTION|PROCEDURE)\s+(dbo\.)?finalize_workflow_status`)

func TestProcedureMigrationListsAreComplete(t *testing.T) {
	for _, tc := range []struct {
		dialect string
		listed  []string
	}{
		{"postgres", postgresProcedureMigrations},
		{"mysql", mysqlProcedureMigrations},
		{"mssql", mssqlProcedureMigrations},
	} {
		t.Run(tc.dialect, func(t *testing.T) {
			dir := filepath.Join("..", "migrations", tc.dialect)
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatalf("read %s: %v", dir, err)
			}

			var defining []string
			for _, e := range entries {
				if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
					continue
				}
				data, err := os.ReadFile(filepath.Join(dir, e.Name()))
				if err != nil {
					t.Fatalf("read %s: %v", e.Name(), err)
				}
				if procedureDDL.MatchString(stripSQLComments(string(data))) {
					defining = append(defining, e.Name())
				}
			}
			sort.Strings(defining)

			listed := append([]string(nil), tc.listed...)
			sort.Strings(listed)

			missing := difference(defining, listed)
			if len(missing) > 0 {
				t.Errorf("%s: these migrations define finalize_workflow_status but are not in "+
					"%sProcedureMigrations: %v\n"+
					"Add them, in migration order. Until then every test using this backend runs "+
					"against the last listed version, and a change to the procedure is contradicted "+
					"by a passing suite.", tc.dialect, tc.dialect, missing)
			}

			// The other direction: an entry naming a file that does not define
			// the routine is applied for nothing, and an entry naming a file
			// that no longer exists fails the harness at Setup with a read
			// error rather than here, where it can say why.
			stale := difference(listed, defining)
			if len(stale) > 0 {
				t.Errorf("%s: %sProcedureMigrations lists %v, which do not define "+
					"finalize_workflow_status. Remove them.", tc.dialect, tc.dialect, stale)
			}
		})
	}
}

// difference returns the elements of a that are not in b.
func difference(a, b []string) []string {
	inB := make(map[string]bool, len(b))
	for _, s := range b {
		inB[s] = true
	}
	var out []string
	for _, s := range a {
		if !inB[s] {
			out = append(out, s)
		}
	}
	return out
}

// TestTheScannerIgnoresSQLComments exercises the comment stripping the list
// check depends on, so that the precaution above is verified rather than
// assumed. A commented-out or merely discussed CREATE must not count as a
// definition; a real one must.
func TestTheScannerIgnoresSQLComments(t *testing.T) {
	for _, tc := range []struct {
		name  string
		sql   string
		match bool
	}{
		{"real definition", "CREATE OR REPLACE FUNCTION finalize_workflow_status(\n  p_id TEXT\n)", true},
		{"mssql definition", "CREATE OR ALTER PROCEDURE dbo.finalize_workflow_status\n  @p_id NVARCHAR(255)", true},
		{"mysql definition", "CREATE PROCEDURE finalize_workflow_status(\n  p_id TEXT\n)", true},
		{"commented out", "-- CREATE OR REPLACE FUNCTION finalize_workflow_status(\n", false},
		{"discussed in prose", "-- 003 defines finalize_workflow_status as RETURNS VOID.\nSELECT 1;\n", false},
		{"inside a block comment", "/*\nCREATE PROCEDURE finalize_workflow_status()\n*/\nSELECT 1;\n", false},
		{"named without defining", "SELECT finalize_workflow_status($1, $2);\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := procedureDDL.MatchString(stripSQLComments(tc.sql))
			if got != tc.match {
				t.Errorf("match = %v, want %v, for:\n%s", got, tc.match, tc.sql)
			}
		})
	}
}
