package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestQualifiedTableMatchesEachDialectsMigrations checks the mapping in
// dialect.qualifiedTable against the migrations of the dialect it claims to
// describe.
//
// coreTables is the PostgreSQL spelling, and TestCoreTablesMatchTheMigrations
// already holds it to migrations/postgres. That says nothing about the other
// two, and the mapping between them is exactly the kind of hand-written
// correspondence that rots: it encodes that MySQL has no `admin` schema and
// that SQL Server puts core tables in `dbo`, neither of which is visible from
// the PostgreSQL files.
//
// Without this, a future migration that adds a table to `admin` on two dialects
// and forgets the third would leave check-db reporting a MISSING table on a
// correct database, or -- worse, and this is the direction that stays quiet --
// looking for a table under a schema that does not exist and reporting nothing
// at all.
func TestQualifiedTableMatchesEachDialectsMigrations(t *testing.T) {
	for _, tc := range []struct {
		d       dialect
		dir     string
		floor   int
		wantAdm string // what the admin tables should map to, as a sanity anchor
	}{
		{dialectPostgres, "postgres", 30, "admin"},
		{dialectMySQL, "mysql", 20, ""},
		{dialectMSSQL, "mssql", 20, "admin"},
	} {
		t.Run(tc.d.name, func(t *testing.T) {
			created := tablesCreatedByDialect(t, tc.dir, tc.floor)
			var missing []string
			for _, core := range coreTables {
				schema, name := tc.d.qualifiedTable(core)

				if strings.HasPrefix(core, "admin.") && schema != tc.wantAdm {
					t.Errorf("%s: %s mapped to schema %q, want %q",
						tc.d.name, core, schema, tc.wantAdm)
				}

				// The migrations name a table either bare or schema-qualified,
				// and which one is itself dialect-dependent, so BOTH spellings
				// are accepted here. The question this test answers is "does a
				// table by this name exist in this dialect's migrations at
				// all", not "is it written the same way" -- the schema half is
				// checked by the assertion above, against a fixed expectation.
				qualified := name
				if schema != "" {
					qualified = schema + "." + name
				}
				if !created[name] && !created[strings.ToLower(qualified)] {
					missing = append(missing, core+" -> "+qualified)
				}
			}
			if len(missing) > 0 {
				sort.Strings(missing)
				t.Errorf("%s: %d core table(s) that dialect.qualifiedTable maps to a name "+
					"migrations/%s never creates:\n  %s\n\n"+
					"Either the mapping is wrong, or this dialect is genuinely missing the "+
					"table -- check which before editing either side.",
					tc.d.name, len(missing), tc.dir, strings.Join(missing, "\n  "))
			}
		})
	}
}

// tablesCreatedByDialect is tablesCreatedByMigrations for an arbitrary dialect
// directory, returning a set rather than a sorted slice.
//
// The floor is on the INPUT for the same reason the original gives: a wrong
// path or a changed suffix reads nothing, and an empty expected set makes every
// lookup fail in a way that blames the mapping instead of the reader.
func tablesCreatedByDialect(t *testing.T, dialectDir string, floor int) map[string]bool {
	t.Helper()
	dir := filepath.Join("..", "..", "migrations", dialectDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	seen := map[string]bool{}
	var files int
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		files++
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("reading %s: %v", e.Name(), err)
		}
		body := lineComment.ReplaceAllString(string(raw), "")
		for _, m := range createTableRE.FindAllStringSubmatch(body, -1) {
			full := strings.ToLower(m[1])
			seen[full] = true
			// Also index the bare name, so `dbo.workflow_defs` answers a
			// lookup for `workflow_defs`.
			if _, bare, ok := strings.Cut(full, "."); ok {
				seen[bare] = true
			}
		}
	}
	if files < floor {
		t.Fatalf("read only %d .sql files from %s: the extraction is broken, not the mapping", files, dir)
	}
	return seen
}
