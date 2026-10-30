package tenantdrop

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The plan's `retained` list says what a drop keeps, and nothing used to check that list against the schema
// (cleat#3552, B3b-2). The drop's own behaviour is pinned elsewhere -- a real drop leaving
// admin.plugin_dep_overrides behind -- but the plan's REPORT of it was not: deleting that entry from Retained
// failed no test, so the plan could say a drop keeps less than it does.
//
// WHAT MAKES A TABLE RETAINED IS DISCOVERABLE. SQL Server's admin.drop_tenant deletes from every table with a
// column named tenant_id, so a record that must outlive its tenant has to be keyed by a different name, and
// every such table in `admin` so far says which tenant it names in a column that CONTAINS "tenant" but is not
// "tenant_id": dropped_tenant_id on the tombstone, for_tenant_id on the override record. The next audit table
// will do the same or its rows are deleted on SQL Server, so a table found this way that Retained does not
// list is a drop whose report is wrong.
//
// This is a floor and not a definition: a retained table may be named some other way, and Retained is then
// the only place that says so. It catches the one way the list drifts that has an observable cause.
//
// KNOWN BLIND SPOT, found by the reviewer with planted migrations: the parse wants the closing ")" after a
// newline and each column at the start of a line, so a CREATE TABLE written on ONE line is invisible to it.
// Every table in the tree is laid out over several lines, and a one-line table would fail the known-positive
// check below only if it were one of the two named there; this is a limit to know about, not one that bites.

var (
	createTableRe = regexp.MustCompile(`(?is)CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?([\w.\[\]"` + "`" + `]+)\s*\((.*?)\n\s*\)\s*(?:;|\n|$)`)
	columnRe      = regexp.MustCompile(`(?im)^\s*[\[\"` + "`" + `]?([a-z_][a-z0-9_]*)[\]\"` + "`" + `]?\s+(?:uuid|text|varchar|nvarchar|uniqueidentifier|char)\b`)
	blockComment  = regexp.MustCompile(`(?s)/\*.*?\*/`)
	lineComment   = regexp.MustCompile(`--[^\n]*`)
)

// adminTablesNamingATenantElsewhere returns, per dialect directory, the admin.* tables that have a column
// containing "tenant" other than tenant_id.
func adminTablesNamingATenantElsewhere(t *testing.T, dialect string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("..", "..", "migrations", dialect, "*.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no migrations found for %s (glob err %v): the scan would pass over nothing", dialect, err)
	}
	found := map[string]bool{}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		// Comments first: a header that quotes a CREATE TABLE is prose about a table, not a table.
		src := lineComment.ReplaceAllString(blockComment.ReplaceAllString(string(raw), ""), "")
		for _, m := range createTableRe.FindAllStringSubmatch(src, -1) {
			name := strings.Trim(strings.ToLower(m[1]), `[]"`+"`")
			if !strings.HasPrefix(name, "admin.") {
				continue
			}
			for _, c := range columnRe.FindAllStringSubmatch(m[2], -1) {
				col := strings.ToLower(c[1])
				if strings.Contains(col, "tenant") && col != "tenant_id" {
					found[name] = true
				}
			}
		}
	}
	var out []string
	for n := range found {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func TestAnAdminTableThatNamesATenantElsewhereIsListedAsRetained(t *testing.T) {
	for _, dialect := range []string{"postgres", "mssql"} {
		t.Run(dialect, func(t *testing.T) {
			found := adminTablesNamingATenantElsewhere(t, dialect)

			// The known positives. A scan that returns nothing, or misses one of these, has stopped reading the
			// schema, and "everything found is retained" would then pass over an empty set.
			for _, want := range []string{"admin.tenant_tombstones", "admin.plugin_dep_overrides"} {
				seen := false
				for _, f := range found {
					seen = seen || f == want
				}
				if !seen {
					t.Fatalf("the migration scan did not find %s (found %v): it can no longer see the tables it exists to check", want, found)
				}
			}

			for _, table := range found {
				if !IsRetained(table) {
					t.Errorf("%s names a tenant in a column other than tenant_id, which is how a record is kept past a "+
						"SQL Server drop, but tenantdrop.Retained does not list it: the plan would report a drop as keeping less "+
						"than it does. Add it to Retained with a reason, or, if the drop does remove it, rename the column "+
						"tenant_id", table)
				}
			}
		})
	}
}
