package testutil

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// cleat#3173. restoreMSSQLPlainPredicate used to replay a hardcoded list --
// "002_defaults.sql", "003_procedures.sql", and (after cleat#3171's point
// fix) "013_a_promise_resolved_mid_segment_wakes_the_workflow.sql" -- because
// 003_procedures.sql is the generated baseline and bundles every routine
// together, so replaying it alone reverts any LATER migration that redefines
// one of those routines back to its pre-migration body. The hardcoded list
// needed a human to notice, for every future migration that redefines a
// bundled routine, that this file exists and needs an entry -- cleat#3171's
// own fix was found only by instrumenting a test failure that looked
// unrelated.
//
// This replaces the hardcoded tail with a derivation: scan
// migrations/mssql/ for which routines 003_procedures.sql defines, then scan
// every later-numbered file for a redefinition of any of those same
// routines. No later migration can be missed because nobody added a line for
// it -- the list is a property of the files on disk, re-derived every time
// this runs.

// mssqlRoutineDefRe matches a statement that DEFINES a SQL Server routine --
// CREATE PROCEDURE, CREATE FUNCTION, or either with OR ALTER (SQL Server has
// no OR REPLACE) -- and captures the routine's name, schema prefix included.
//
// Deliberately narrow, matching engine/procedure_migration_list_test.go's own
// procedureDDL: a statement defines a routine, a reference to the routine
// (CREATE ... AS SELECT dbo.finalize_workflow_status(...), or prose
// describing it) does not. Comments are stripped before this runs (see
// mssqlStripSQLComments below), for the same reason that file strips them --
// "a text search cannot tell a thing from a sentence about the thing", and
// this repository has made that mistake four times already.
var mssqlRoutineDefRe = regexp.MustCompile(
	`(?i)\bCREATE\s+(?:OR\s+ALTER\s+)?(?:PROCEDURE|FUNCTION)\s+([A-Za-z_][A-Za-z0-9_.]*)`)

// mssqlStripSQLComments removes `--` line comments and `/* ... */` block
// comments from sql, so mssqlRoutineDefRe cannot match one.
//
// A SEPARATE, SMALLER copy of the same precaution
// engine/procedure_migration_list_test.go's stripSQLComments takes --
// necessarily separate, because that one lives in a _test.go file in a
// different package (engine) and is not reachable from here. Not
// byte-for-byte identical, but doing the same job: drop `--`-to-end-of-line
// and `/* ... */`, nothing cleverer. No current MSSQL migration puts a
// CREATE statement inside a string literal containing `--` or `/*`, so this
// is a precaution against a future prose comment, not a load-bearing parser.
func mssqlStripSQLComments(sql string) string {
	var b strings.Builder
	b.Grow(len(sql))
	for i := 0; i < len(sql); i++ {
		if sql[i] == '-' && i+1 < len(sql) && sql[i+1] == '-' {
			for i < len(sql) && sql[i] != '\n' {
				i++
			}
			b.WriteByte('\n')
			continue
		}
		if sql[i] == '/' && i+1 < len(sql) && sql[i+1] == '*' {
			end := strings.Index(sql[i+2:], "*/")
			if end < 0 {
				break
			}
			i += 2 + end + 1
			b.WriteByte(' ')
			continue
		}
		b.WriteByte(sql[i])
	}
	return b.String()
}

// mssqlRoutinesDefinedIn returns the lowercased, schema-qualified routine
// names path's SQL defines (e.g. "dbo.finalize_workflow_status"), reading
// the real file content rather than a hand-maintained description of it.
func mssqlRoutinesDefinedIn(path string) (map[string]bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	stripped := mssqlStripSQLComments(string(data))
	names := map[string]bool{}
	for _, m := range mssqlRoutineDefRe.FindAllStringSubmatch(stripped, -1) {
		names[strings.ToLower(m[1])] = true
	}
	return names, nil
}

// mssqlNumberedMigrationFiles lists dir's top-level "NNN_*.sql" files
// (skipping subdirectories such as migrations/mssql/optional/, which no
// Runner applies unconditionally), as (number, basename) pairs sorted by
// number. Only the top-level numbered files are in scope -- the same universe
// restoreMSSQLPlainPredicate already replays from.
func mssqlNumberedMigrationFiles(dir string) ([]struct {
	num  int
	name string
}, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []struct {
		num  int
		name string
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		name := e.Name()
		if len(name) < 4 || name[3] != '_' {
			continue
		}
		// strconv.Atoi alone would also accept a leading '+' or '-', which
		// no real migration filename has but which would otherwise sort
		// into the wrong place rather than being excluded.
		prefix := name[:3]
		isDigits := true
		for _, c := range prefix {
			if c < '0' || c > '9' {
				isDigits = false
				break
			}
		}
		if !isDigits {
			continue
		}
		num, err := strconv.Atoi(prefix)
		if err != nil {
			continue
		}
		out = append(out, struct {
			num  int
			name string
		}{num, name})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].num < out[j].num })
	return out, nil
}

// mssqlPlainPredicateReplayFiles returns, in order, the migration files
// restoreMSSQLPlainPredicate must replay to put admin.rls_predicate_form
// back to 'plain' WITHOUT reverting a later migration's redefinition of a
// routine 003_procedures.sql also defines.
//
// Always "002_defaults.sql" (the MERGE that records the 'plain' form) then
// "003_procedures.sql" (the routine definitions, in their generated-baseline
// form), then every later-numbered file -- in filename order, matching how
// migration.Runner itself would have applied them -- that redefines any
// routine 003 defines. A file that touches none of those routines plays no
// part in the hazard this function exists to close and is correctly left
// out: replaying it would not be wrong, but it would not be measuring
// anything either, and every extra replay is one more thing that can fail
// for an unrelated reason during teardown.
func mssqlPlainPredicateReplayFiles(dir string) ([]string, error) {
	bundled, err := mssqlRoutinesDefinedIn(filepath.Join(dir, "003_procedures.sql"))
	if err != nil {
		return nil, err
	}

	files, err := mssqlNumberedMigrationFiles(dir)
	if err != nil {
		return nil, err
	}

	replay := []string{"002_defaults.sql", "003_procedures.sql"}
	for _, f := range files {
		if f.num <= 3 {
			continue
		}
		defined, err := mssqlRoutinesDefinedIn(filepath.Join(dir, f.name))
		if err != nil {
			return nil, err
		}
		for name := range defined {
			if bundled[name] {
				replay = append(replay, f.name)
				break
			}
		}
	}
	return replay, nil
}
