package engine

import (
	"database/sql"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
)

// docs/explanation/postgresql-schema.md must describe the schema this repo
// actually ships.
//
// cleat#1093. Measured 2026-09-09, before this guard existed: all 8 documented
// tables had drifted, 44 columns were missing across them, and two tables
// documented a `namespace` column that no migration creates and no code reads.
// The phantom had propagated into an index, `idx_instances_namespace_ready`,
// given as current in two places.
//
// # Why a test and not a script
//
// The claim is about the schema, so it needs a database rather than a second
// file to diff against. This is the same shape as
// routine_definition_drift_test.go, which compares the database against the
// routines the migrations ship, and it runs wherever that one does.
//
// # Why the CREATE TABLE blocks and not the prose tables
//
// The per-column prose tables below each block are deliberately a SELECTION and
// say so. Their value is the description of each column, which cannot be
// generated and must not be invented to make a table look complete. The DDL
// block is the enumeration, and it is the thing checked here.
//
// Anchoring to the block also solves a problem a name search cannot: the same
// document legitimately names `idx_instances_ready` -- as what
// `idx_instances_claimable` USED to be before migration 040 -- so a scan for
// identifiers would have to tell a historical aside from a current claim. A
// name inside a CREATE TABLE block cannot be an aside.
//
// # What this deliberately does NOT check
//
// Types, defaults, nullability and constraint syntax. The block is allowed to
// be illustrative about those; a guard that failed on cosmetic edits would be
// deleted, which is how the file reached the state above. The column-name SET
// is the invariant.
func TestTheSchemaDocDescribesTheDatabaseWeShip(t *testing.T) {
	docPath := filepath.Join("..", "docs", "explanation", "postgresql-schema.md")
	raw, err := os.ReadFile(docPath)
	if err != nil {
		t.Fatalf("reading %s: %v", docPath, err)
	}
	documented := parseDocumentedTables(t, string(raw))

	// Anti-vacuity floor, not a target. Deliberately loose so adding a table
	// does not fail here, deliberately non-zero so losing the parse does.
	// Measured 2026-09-09: 8 tables.
	const minTables = 5
	if len(documented) < minTables {
		t.Fatalf("parsed only %d CREATE TABLE blocks out of %s; there were 8 on "+
			"2026-09-09.\n\nA parse that matches almost nothing passes vacuously, so "+
			"this is a failure: fix the pattern rather than lowering this floor.",
			len(documented), docPath)
	}

	db := testutil.TestDB(t, testutil.DialectPostgres)
	testutil.SetupFullSchema(t, db, testutil.DialectPostgres)

	var names []string
	for name := range documented {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, table := range names {
		docCols := documented[table]
		if len(docCols) == 0 {
			t.Errorf("the CREATE TABLE block for %q parsed to zero columns; the "+
				"pattern stopped matching rather than the table becoming empty", table)
			continue
		}
		realCols := columnsOf(t, db, table)
		if len(realCols) == 0 {
			t.Errorf("%s: documented, but the database has no such table.\n\n"+
				"Either the table was dropped and the doc kept it, or the block "+
				"names something that never existed -- which is what `namespace` "+
				"was, on two tables (cleat#1093).", table)
			continue
		}

		// difference() lives in procedure_migration_list_test.go; sorted here
		// so the failure message is stable across runs.
		missing := difference(realCols, docCols)
		phantom := difference(docCols, realCols)
		sort.Strings(missing)
		sort.Strings(phantom)
		if len(missing) == 0 && len(phantom) == 0 {
			continue
		}
		// Both directions, always, and by NAME. A count answers "did it go up";
		// it cannot see a surplus at all, and a surplus is the more expensive
		// kind -- a reader looking up how tenancy is stored found `namespace`,
		// which has never existed, and did not find `tenant_id`, which is real.
		t.Errorf("%s: the doc and the database disagree.\n\n"+
			"  in the database, NOT documented (%d): %s\n"+
			"  documented, NOT in the database (%d): %s\n\n"+
			"Update the CREATE TABLE block in %s. The prose table under it is a "+
			"selection and does not have to list every column.",
			table, len(missing), strings.Join(missing, ", "),
			len(phantom), strings.Join(phantom, ", "), docPath)
	}
}

var (
	docTableRe  = regexp.MustCompile(`(?s)CREATE TABLE (?:IF NOT EXISTS )?(\w+)\s*\((.*?)\n\);`)
	docColumnRe = regexp.MustCompile(`^\s{4}([a-z_][a-z0-9_]*)\s+[A-Za-z]`)
)

// parseDocumentedTables returns the column names of every CREATE TABLE block in
// the document.
//
// A line is a column when it is indented and its second token starts a type.
// That excludes the table constraints -- PRIMARY KEY, FOREIGN KEY, UNIQUE,
// CHECK -- because those begin with an uppercase keyword rather than a
// lowercase identifier.
func parseDocumentedTables(t *testing.T, doc string) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for _, m := range docTableRe.FindAllStringSubmatch(doc, -1) {
		var cols []string
		for _, line := range strings.Split(m[2], "\n") {
			if c := docColumnRe.FindStringSubmatch(line); c != nil {
				cols = append(cols, c[1])
			}
		}
		out[m[1]] = cols
	}
	return out
}

func columnsOf(t *testing.T, db *sql.DB, table string) []string {
	t.Helper()
	rows, err := db.Query(`
		SELECT column_name FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = $1`, table)
	if err != nil {
		t.Fatalf("reading columns of %s: %v", table, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatalf("scanning column of %s: %v", table, err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterating columns of %s: %v", table, err)
	}
	return out
}
