package migration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// splitSQL had no unit tests, which is most of why IMPROVEMENT-PLAN 3.13
// survived: the defect was a one-line pure function whose input is checked into
// this repo, and reproducing it needed nothing but calling it.
//
// The first two cases are the two shipped files, reduced to the shape that
// broke. The rest are the things a semicolon can hide inside.
func TestSplitSQL(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{
			name: "semicolon inside a line comment does not end the statement",
			// migrations/mysql/001_schema.sql line 7, verbatim.
			in: "-- CREATE INDEX has no IF NOT EXISTS in MySQL 8.0; re-runs error harmlessly.\n" +
				"CREATE TABLE t (id INT);",
			want: []string{"CREATE TABLE t (id INT)"},
		},
		{
			name: "DELIMITER lets a procedure body keep its semicolons",
			// The shape of migrations/mysql/003_procedures.sql.
			in: "DELIMITER //\n" +
				"CREATE PROCEDURE p()\nBEGIN\n  SELECT 1;\n  SELECT 2;\nEND //\n" +
				"DELIMITER ;\n" +
				"SELECT 3;",
			want: []string{
				"CREATE PROCEDURE p()\nBEGIN\n  SELECT 1;\n  SELECT 2;\nEND",
				"SELECT 3",
			},
		},
		{
			name: "semicolon inside a string literal",
			in:   "INSERT INTO t VALUES ('a;b');\nSELECT 1;",
			want: []string{"INSERT INTO t VALUES ('a;b')", "SELECT 1"},
		},
		{
			name: "semicolon inside a quoted identifier",
			in:   "CREATE TABLE `we;ird` (id INT);",
			want: []string{"CREATE TABLE `we;ird` (id INT)"},
		},
		{
			name: "escaped quote does not end the string",
			in:   `INSERT INTO t VALUES ('it\'s; fine');` + "\nSELECT 1;",
			want: []string{`INSERT INTO t VALUES ('it\'s; fine')`, "SELECT 1"},
		},
		{
			name: "doubled quote does not end the string",
			in:   "INSERT INTO t VALUES ('it''s; fine');",
			want: []string{"INSERT INTO t VALUES ('it''s; fine')"},
		},
		{
			name: "semicolon inside a block comment",
			in:   "/* one; two */\nSELECT 1;",
			want: []string{"SELECT 1"},
		},
		{
			name: "hash comment",
			in:   "# a; comment\nSELECT 1;",
			want: []string{"SELECT 1"},
		},
		{
			name: "a trailing comment block is not a statement",
			// MySQL answers an empty query with error 1065, so a file that
			// ends in prose must not produce a fragment.
			in:   "SELECT 1;\n\n-- and that is all\n",
			want: []string{"SELECT 1"},
		},
		{
			name: "-- without following whitespace is not a comment in MySQL",
			in:   "SELECT 1--2;",
			want: []string{"SELECT 1--2"},
		},
		{
			name: "a statement need not be terminated",
			in:   "SELECT 1",
			want: []string{"SELECT 1"},
		},
		{
			name: "empty input yields no statements",
			in:   "\n  \n",
			want: nil,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			got := splitSQL(tt.in)
			if len(got) != len(tt.want) {
				t.Fatalf("got %d statements, want %d\ngot:  %q\nwant: %q",
					len(got), len(tt.want), got, tt.want)
			}
			for i := range got {
				if strings.TrimSpace(got[i]) != strings.TrimSpace(tt.want[i]) {
					t.Errorf("statement %d:\ngot:  %q\nwant: %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

// TestSplitSQL_ShippedMySQLFiles runs the splitter over the real files and
// asserts the properties that actually matter, without a database: nothing it
// emits may be pure prose, and the procedure has to survive in one piece.
//
// This is the check that would have caught 3.13 with no MySQL running at all.
func TestSplitSQL_ShippedMySQLFiles(t *testing.T) {
	dir := filepath.Join(migrationsRoot(t), "mysql")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}

	sawProcedure := false
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		stmts := splitSQL(string(data))
		if len(stmts) == 0 {
			t.Errorf("%s split into no statements at all", e.Name())
			continue
		}
		for i, s := range stmts {
			if isAllComments(s) {
				t.Errorf("%s statement %d is only a comment, which MySQL rejects as an "+
					"empty query:\n%q", e.Name(), i, s)
			}
			if strings.EqualFold(strings.TrimSpace(s), "DELIMITER") ||
				strings.HasPrefix(strings.ToUpper(strings.TrimSpace(s)), "DELIMITER ") {
				t.Errorf("%s statement %d is a DELIMITER directive, which is a client "+
					"instruction and not something a server accepts:\n%q", e.Name(), i, s)
			}
			if strings.Contains(strings.ToUpper(s), "CREATE PROCEDURE") {
				sawProcedure = true
				// A procedure cut on its body's semicolons loses its END.
				if !strings.Contains(strings.ToUpper(s), "END") {
					t.Errorf("%s statement %d contains CREATE PROCEDURE but no END: "+
						"the body was cut on one of its own semicolons:\n%q", e.Name(), i, s)
				}
			}
		}
	}
	if !sawProcedure {
		t.Error("no CREATE PROCEDURE found in migrations/mysql/: this test is " +
			"asserting nothing about the case it exists for")
	}
}
