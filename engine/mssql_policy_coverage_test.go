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

// enginePredicateRe recognises the engine's own predicate in the form SQL Server
// stores it: sys.security_predicates.predicate_definition reads
// ([dbo].[fn_tenant_filter]([tenant_id])).
//
// ANCHORED ON THE BRACKETS, which is what makes it exact rather than nearly
// exact. A bare substring test for "fn_tenant_filter" would be one rename away
// from matching "fn_plugin_tenant_filter" -- it does not today, and only
// because the "fn_" happens not to be adjacent. Brackets are where the
// catalogue puts an identifier's boundary, so matching them is the same move as
// anchoring on a declaration site rather than on a name: it cannot match a
// longer identifier that merely contains this one.
var enginePredicateRe = regexp.MustCompile(`\[dbo\]\.\[fn_tenant_filter\]`)

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

	// Guard on the DSN before touching testutil.MSSQLTestDB, which does NOT
	// skip when SQL Server is absent: with CLEAT_TEST_MSSQL unset it falls back
	// to a default localhost:1433 DSN and t.Fatalf's on the failed ping. The
	// first version of this test omitted the guard and failed three CI jobs
	// that have no SQL Server -- a test asserting a database property must skip
	// where the database does not exist, and the neighbouring
	// TestMSSQLTenantIsolation_UnderRealSecurityPolicies guards exactly this way.
	if os.Getenv("CLEAT_TEST_MSSQL") == "" {
		t.Skip("CLEAT_TEST_MSSQL not set, skipping SQL Server tests")
	}
	if testing.Short() {
		t.Skip("Skipping MSSQL integration test in short mode")
	}

	db := testutil.MSSQLTestDB(t)
	testutil.SetupMSSQLFullSchema(t, db)

	// BOTH SIDES MUST ASK ABOUT THE SAME PREDICATE FUNCTION, and until cleat#1629
	// there was only one, so they did without anyone noticing.
	//
	// mssqlFilterPredicateRe above names dbo.fn_tenant_filter specifically. This
	// query used to return every row of sys.security_predicates regardless of
	// which function it called, so the two sides asked different questions: the
	// migrations side "which tables does fn_tenant_filter cover", the database
	// side "which tables have any predicate at all". cleat#1552 added a SECOND
	// function, dbo.fn_plugin_tenant_filter, installed at PLUGIN migration time
	// from Go rather than by any file in migrations/mssql -- so on any database
	// where plugin migrations have run, 25-odd plugin tables appeared in `got`,
	// could not appear in `want`, and the "extra" check below reported every one
	// of them as a schema disagreement.
	//
	// It is not one. Those predicates are correct, deliberate, and sourced from
	// plugin/migration.go; they are simply not this test's subject. That only
	// stayed green in CI because ./engine/ runs before ./plugins/ in every job
	// that runs both -- an ordering, not an invariant, and it failed the moment
	// a local run reused a database.
	//
	// The definition is read rather than filtered in SQL so the exclusion can be
	// REPORTED rather than silently applied; see the log at the end.
	rows, err := db.Query(`
		SELECT DISTINCT t.name, sp.predicate_definition
		FROM sys.security_predicates sp
		JOIN sys.tables t ON t.object_id = sp.target_object_id
		ORDER BY t.name`)
	if err != nil {
		t.Fatalf("reading sys.security_predicates: %v", err)
	}
	defer rows.Close()
	var got, otherPredicate []string
	for rows.Next() {
		var name, definition string
		if err := rows.Scan(&name, &definition); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if enginePredicateRe.MatchString(definition) {
			got = append(got, name)
		} else {
			otherPredicate = append(otherPredicate, name+" "+definition)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterating: %v", err)
	}
	// A parse that matches nothing passes vacuously, and this one would do it in
	// the direction that reports every shipped table as MISSING -- which reads
	// as a catastrophe rather than as a broken test. Same rule the `want` bound
	// above applies to the migration-side regex.
	if len(got) == 0 {
		t.Fatalf("no security predicate in this database calls dbo.fn_tenant_filter, but "+
			"%d predicates exist.\n\nEither enginePredicateRe no longer matches the form "+
			"SQL Server stores -- it was ([dbo].[fn_tenant_filter]([tenant_id])) when this "+
			"was written -- or the engine's policies are genuinely gone. Check one "+
			"predicate_definition by hand before assuming the second.\n\nnot matched: %v",
			len(otherPredicate), otherPredicate)
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
		// Say what was excluded, every time, rather than only when it is
		// empty. A check that quietly narrows its own population reads as
		// covering more than it does, and this one narrowed from "every
		// predicate" to "the engine's predicate" -- which is the right scope
		// and is worth being visible in the log of a passing run.
		if len(otherPredicate) > 0 {
			t.Logf("%d predicate(s) on other functions were excluded, correctly -- plugin "+
				"tables get dbo.fn_plugin_tenant_filter from plugin/migration.go, which no "+
				"file in migrations/mssql binds and this test does not cover: %v",
				len(otherPredicate), otherPredicate)
		}
	}
}
