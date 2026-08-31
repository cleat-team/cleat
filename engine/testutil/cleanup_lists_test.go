package testutil

import (
	"strings"
	"testing"
)

// IMPROVEMENT-PLAN 2.60d: the three cleanup lists had drifted apart.
//
// MySQL and SQL Server cleared 15 tables; PostgreSQL cleared 11, missing
// tenant_api_keys, workflow_tags, workflow_routing and plugin_defs — all of
// which exist in the PostgreSQL schema. So those four accumulated rows across
// every test in the package, and the rows surfaced later as an unrelated test
// failing on a duplicate key.
//
// Nothing could have noticed. The lists live in three files, none of them
// referenced the others, and each was individually plausible. This test is the
// mechanism: one assertion that the three agree, so the next table added to one
// of them has to be added to all three.
//
// Not a sweep of the current contents — those are already reconciled — but the
// thing that keeps them reconciled.

// normaliseTable strips the schema qualifier SQL Server needs. Only
// admin.tenant_api_keys carries one, and it must: unqualified, the DELETE
// resolves against the connecting principal's default schema and fails, while
// the existence check passes because sys.tables is keyed on name alone.
func normaliseTable(s string) string {
	if i := strings.IndexByte(s, '.'); i >= 0 {
		return s[i+1:]
	}
	return s
}

func TestCleanupTableListsAgree(t *testing.T) {
	lists := map[string][]string{
		"postgres": postgresCleanupTables,
		"mysql":    mysqlCleanupTables,
		"mssql":    mssqlCleanupTables,
	}

	// Compare against PostgreSQL's, arbitrarily — the assertion is agreement,
	// not that any one of them is authoritative.
	want := make([]string, 0, len(postgresCleanupTables))
	for _, tbl := range postgresCleanupTables {
		want = append(want, normaliseTable(tbl))
	}

	for name, list := range lists {
		got := make([]string, 0, len(list))
		for _, tbl := range list {
			got = append(got, normaliseTable(tbl))
		}

		if len(got) != len(want) {
			t.Errorf("%s clears %d tables, postgres clears %d\n  %s: %v\n  postgres: %v",
				name, len(got), len(want), name, got, want)
			continue
		}
		// Order matters as much as membership: these are deleted in
		// foreign-key order, and a list with the right members in the wrong
		// order fails at runtime on a constraint violation.
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("%s differs from postgres at position %d: %q vs %q\n"+
					"  The lists are deleted in foreign-key order, so both "+
					"membership and order have to match.",
					name, i, got[i], want[i])
				break
			}
		}
	}
}

// TestCleanupListsHaveNoDuplicates guards the other way a list goes wrong:
// deleting the same table twice is harmless, but it means someone edited one
// list without reading it, and the count assertion above would then pass
// against a list missing a different table.
func TestCleanupListsHaveNoDuplicates(t *testing.T) {
	for name, list := range map[string][]string{
		"postgres": postgresCleanupTables,
		"mysql":    mysqlCleanupTables,
		"mssql":    mssqlCleanupTables,
	} {
		seen := make(map[string]bool, len(list))
		for _, tbl := range list {
			n := normaliseTable(tbl)
			if seen[n] {
				t.Errorf("%s lists %q more than once", name, n)
			}
			seen[n] = true
		}
	}
}
