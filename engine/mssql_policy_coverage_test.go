package engine

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
)

// The tenant filter predicates the migrations ship must exist in the database
// they build. IMPROVEMENT-PLAN 2.71's last residual.
//
// # What was not covered before
//
// §2.71 ends by naming this gap in its own words: "That the predicates are
// *created* is read off the migration files; nothing asserts per-table policy
// coverage." The two checks that existed are both satisfied by a single
// surviving policy:
//
//   - requireMSSQLPoliciesIntact (engine/testutil/mssql_schema.go) runs
//     `SELECT COUNT(*) FROM sys.security_policies` and tests `> 0`;
//   - mssql_rls_enforcement_test.go reads `is_enabled` for ONE policy by name.
//
// Measured 2026-09-04: the migrations bind a filter predicate to nine tables.
// Eight of the nine could be dropped and both checks stay green.
//
// # Why both sides are read rather than one being listed
//
// A literal list of the nine table names here would be a third copy of
// something the migrations already state, and it would go stale in the
// direction that hides the defect: a policy deleted from a migration AND from
// the list agrees with itself. So the intended set is parsed out of
// migrations/mssql/*.sql and the actual set is read from sys.security_predicates
// in the database those migrations just built. Nothing is declared.
//
// # What this deliberately does NOT assert
//
// That every tenant-scoped table has a predicate. Measured the same day: 38
// tables carry a `tenant_id` column and 9 have one. That is not a finding here
// and must not be read as one -- §3.86 (🟢, WS-1) is the section that covers
// the layer that matters, statement-level tenant predicates in the Go SQL, and
// it records the remaining 27 statements as "an allowlist with reasons, not a
// backlog". RLS on SQL Server is a backstop that is off entirely for an admin
// connection, which is why §3.86 fixed the statements rather than the policies.
// This test pins the backstop that does exist against erosion; it does not
// relitigate its scope.
var mssqlFilterPredicateRe = regexp.MustCompile(
	`ADD FILTER PREDICATE dbo\.fn_tenant_filter\(tenant_id\) ON dbo\.(\w+)`)

// tablesWithShippedPolicies parses migrations/mssql/*.sql for the tables a
// filter predicate is bound to.
//
// Drop-then-recreate is the shipped idiom (`DROP SECURITY POLICY IF EXISTS`
// followed by `CREATE SECURITY POLICY`), so taking the union of every ON
// clause is right: a table is intended to be covered if any migration binds a
// predicate to it, and none of them drops a policy without recreating it.
// Verified 2026-09-04 -- the union is nine tables and the built database has
// exactly those nine, which is the equality this test asserts.
func tablesWithShippedPolicies(t *testing.T) []string {
	t.Helper()
	dir := filepath.Join("..", "migrations", "mssql")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	seen := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("reading %s: %v", e.Name(), err)
		}
		for _, m := range mssqlFilterPredicateRe.FindAllStringSubmatch(string(b), -1) {
			seen[m[1]] = true
		}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestEveryShippedTenantPolicyExistsInTheBuiltDatabase(t *testing.T) {
	want := tablesWithShippedPolicies(t)

	// A regex that matches nothing would make every assertion below vacuously
	// true, which is the failure this whole section is about. Nine when
	// written; the bound is deliberately loose so adding a policy does not
	// fail here, and deliberately non-zero so losing the parse does.
	if len(want) < 5 {
		t.Fatalf("parsed only %d filter-predicate bindings out of migrations/mssql/*.sql; "+
			"there were 9 on 2026-09-04.\n\nA parse that matches almost nothing "+
			"passes vacuously, so this is a failure: re-point "+
			"mssqlFilterPredicateRe rather than lowering this bound.", len(want))
	}

	db := testutil.MSSQLTestDB(t)
	testutil.SetupMSSQLFullSchema(t, db)

	rows, err := db.Query(`
		SELECT DISTINCT t.name
		FROM sys.security_predicates sp
		JOIN sys.tables t ON t.object_id = sp.target_object_id
		ORDER BY t.name`)
	if err != nil {
		t.Fatalf("reading sys.security_predicates: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterating: %v", err)
	}

	gotSet := map[string]bool{}
	for _, g := range got {
		gotSet[g] = true
	}
	wantSet := map[string]bool{}
	for _, w := range want {
		wantSet[w] = true
	}

	var missing []string
	for _, w := range want {
		if !gotSet[w] {
			missing = append(missing, w)
		}
	}
	if len(missing) > 0 {
		t.Errorf("the migrations bind a tenant filter predicate to %v, but the built "+
			"database has none on it.\n\n"+
			"This is §2.71's residual firing. The existing guards cannot see it: "+
			"requireMSSQLPoliciesIntact tests COUNT(*) > 0 over sys.security_policies, "+
			"and mssql_rls_enforcement_test.go checks is_enabled for one policy by "+
			"name -- so a predicate lost on one table specifically passes both.\n\n"+
			"shipped: %v\nin database: %v", missing, want, got)
	}

	var extra []string
	for _, g := range got {
		if !wantSet[g] {
			extra = append(extra, g)
		}
	}
	if len(extra) > 0 {
		t.Errorf("the database has a tenant filter predicate on %v, which no migration "+
			"binds one to.\n\nThat is not harmless: it means the tested schema and "+
			"the shipped schema disagree, which is the §1.9 shape §2.71 spent its "+
			"life on -- a test environment whose extra protection makes a real gap "+
			"invisible.\n\nshipped: %v\nin database: %v", extra, want, got)
	}

	// Only on success, and reporting what the DATABASE has rather than what the
	// migrations say. The first version logged `want` unconditionally, so a
	// failing run ended with a line listing all nine tables as present --
	// directly contradicting the error above it and giving a reader scanning
	// output the wrong answer.
	if !t.Failed() {
		t.Logf("%d tables carry a shipped tenant filter predicate and the built database "+
			"has one on each: %v", len(got), got)
	}
}
