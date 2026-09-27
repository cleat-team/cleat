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

// normaliseTable strips the schema qualifier so the three lists can be compared
// on membership and order. Only tenant_api_keys carries one, on the two dialects
// that have schemas -- and it must, on both, for the same reason spelled
// differently: unqualified, the name resolves against something other than the
// admin schema. On SQL Server the DELETE fails outright while the existence
// check passes, because sys.tables is keyed on name alone. On PostgreSQL it is
// quieter and worse -- to_regclass follows search_path, so the entry either
// vanishes from the list or lands on a stray copy, and nothing fails at all.
//
// Because this function normalises the qualifier away, TestCleanupTableListsAgree
// is structurally blind to it: "admin.tenant_api_keys" and "tenant_api_keys"
// compare equal, which is why PostgreSQL kept an unqualified entry for as long
// as it did with three green lists. TestCleanupListsQualifyWhereTheDialectHasSchemas
// below is the assertion that comparison cannot make.
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

// TestCleanupListsQualifyWhereTheDialectHasSchemas asserts the axis
// TestCleanupTableListsAgree normalises away.
//
// tenant_api_keys is the only cleanup table that does not live beside the
// others. On PostgreSQL and SQL Server it is admin.tenant_api_keys and the entry
// has to say so; on MySQL there is no schema to qualify with -- a connection has
// exactly one default database -- so qualifying it there would be wrong, not
// merely redundant. Three lists, two of which must carry a prefix and one of
// which must not, is precisely what an equality check over stripped names cannot
// express.
func TestCleanupListsQualifyWhereTheDialectHasSchemas(t *testing.T) {
	for _, tc := range []struct {
		dialect string
		list    []string
		want    string
	}{
		{"postgres", postgresCleanupTables, "admin.tenant_api_keys"},
		{"mssql", mssqlCleanupTables, "admin.tenant_api_keys"},
		{"mysql", mysqlCleanupTables, "tenant_api_keys"},
	} {
		var found string
		for _, tbl := range tc.list {
			if normaliseTable(tbl) == "tenant_api_keys" {
				found = tbl
				break
			}
		}
		if found == "" {
			t.Errorf("%s: no tenant_api_keys entry in the cleanup list at all", tc.dialect)
			continue
		}
		if found != tc.want {
			t.Errorf("%s cleanup list spells it %q, want %q\n"+
				"  PostgreSQL and SQL Server keep this table in the admin schema, "+
				"so an unqualified entry does not resolve to it: SQL Server fails "+
				"the DELETE, PostgreSQL silently cleans a stray copy or nothing. "+
				"MySQL has no schema to qualify with. See IMPROVEMENT-PLAN 2.60d.",
				tc.dialect, found, tc.want)
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
