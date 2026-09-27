package engine_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
)

// TestATenantPolicyKeepsItsIndexAndItsBoundary covers the policy shape
// plugin.applyTenantScoping emits after cleat#1490: two permissive policies,
// `TO PUBLIC USING (tenant_id = cleat.assert_tenant_set())` for the tenant path
// and `TO cleat_sweep USING (true)` for a cross-tenant sweep, replacing the
// single cleat.tenant_row_is_visible(tenant_id) CASE from migration 063.
//
// THE ASSERTION THAT CARRIES THIS TEST IS A ROW COUNT UNDER THE APPLICATION
// ROLE, and that is not the obvious choice. The failure this guards against
// produces no error, the correct number of policies, and a correct-looking
// EXPLAIN: PostgreSQL applies a `TO role` policy on MEMBERSHIP ALONE, with no
// SET ROLE, so granting cleat_sweep to the application role the ordinary way
// makes `USING (true)` apply to every one of its queries. Measured while
// developing this: a plain GRANT let the role read all 400000 rows of a
// 400-tenant table; WITH INHERIT FALSE returned its own 1000. Nothing about
// the schema distinguishes the two states. Only a count does.
//
// THE NEGATIVE CONTROL RUNS FIRST, for the reason the sibling test at
// a_plugin_statement_is_scoped_to_the_workflows_tenant_test.go records: a
// USING clause over an empty table is never evaluated (cleat#1285), so seeding
// only the visible row passes against a policy that is absent or bypassed.
//
// IT CONNECTS AS A ROLE RLS APPLIES TO. testutil.TestDB is a superuser and
// PostgreSQL lets a superuser past every policy, so the same assertions on
// that handle cannot fail.
func TestATenantPolicyKeepsItsIndexAndItsBoundary(t *testing.T) {
	su := testutil.TestDB(t, testutil.DialectPostgres)
	t.Cleanup(func() { su.Close() })
	testutil.SetupFullSchema(t, su, testutil.DialectPostgres)

	ctx := context.Background()

	var schema string
	if err := su.QueryRowContext(ctx, `SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatalf("resolving the engine schema: %v", err)
	}
	table := fmt.Sprintf("%s.plugin_index_probe_%d", schema, os.Getpid())
	bare := fmt.Sprintf("plugin_index_probe_%d", os.Getpid())
	t.Cleanup(func() { su.ExecContext(ctx, `DROP TABLE IF EXISTS `+table) })

	const (
		mine   = "3f2b1c00-0000-4000-8000-000000001490"
		theirs = "7c8d9e00-0000-4000-8000-000000001490"
	)

	for _, stmt := range []string{
		`DROP TABLE IF EXISTS ` + table,
		`CREATE TABLE ` + table + ` (tenant_id uuid NOT NULL, k text NOT NULL)`,
		`CREATE INDEX ON ` + table + ` (tenant_id)`,
		`ALTER TABLE ` + table + ` ENABLE ROW LEVEL SECURITY`,
		`ALTER TABLE ` + table + ` FORCE ROW LEVEL SECURITY`,
		// The shape applyTenantScoping emits. Kept in step with it by hand;
		// the policy names follow the same <table>_tenant_isolation /
		// <table>_cross_tenant convention.
		`CREATE POLICY ` + bare + `_tenant_isolation ON ` + table +
			` FOR ALL TO PUBLIC USING (tenant_id = cleat.assert_tenant_set())`,
		`CREATE POLICY ` + bare + `_cross_tenant ON ` + table +
			` FOR ALL TO cleat_sweep USING (true)`,
		// NEGATIVE CONTROL, seeded before anything is asserted.
		`INSERT INTO ` + table + ` (tenant_id, k) VALUES ('` + theirs + `', 'theirs')`,
		`INSERT INTO ` + table + ` (tenant_id, k) VALUES ('` + mine + `', 'mine')`,
	} {
		if _, err := su.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("fixture %.70q: %v", stmt, err)
		}
	}

	rls := testutil.OpenPostgresRLSTestDB(t, su)
	t.Cleanup(func() { rls.Close() })
	for _, g := range []string{
		`GRANT USAGE ON SCHEMA ` + schema + ` TO ` + testutil.PostgresRLSTestRole,
		`GRANT SELECT ON ` + table + ` TO ` + testutil.PostgresRLSTestRole,
		`GRANT SELECT ON ` + table + ` TO cleat_sweep`,
	} {
		if _, err := su.ExecContext(ctx, g); err != nil {
			t.Fatalf("granting access to the fixture: %v\n  %s", err, g)
		}
	}

	var visible bool
	if err := rls.QueryRowContext(ctx, `SELECT to_regclass($1) IS NOT NULL`, table).Scan(&visible); err != nil {
		t.Fatalf("asking the RLS connection whether it can see %s: %v", table, err)
	}
	if !visible {
		t.Fatalf("UNMEASURED: the RLS-role connection cannot resolve %s", table)
	}

	t.Run("the tenant path sees its own rows and no others", func(t *testing.T) {
		tx, err := rls.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer tx.Rollback()
		if _, err := tx.ExecContext(ctx,
			`SELECT set_config('cleat.tenant_id', $1, true)`, mine); err != nil {
			t.Fatalf("setting the tenant: %v", err)
		}
		var total, distinctTenants int
		if err := tx.QueryRowContext(ctx,
			`SELECT count(*), count(DISTINCT tenant_id) FROM `+table).Scan(&total, &distinctTenants); err != nil {
			t.Fatalf("counting under the application role: %v", err)
		}
		// 1, not 2. A passively-applied cross-tenant policy reports 2 here and
		// nothing else in this test would notice.
		if total != 1 || distinctTenants != 1 {
			t.Errorf("the application role saw %d rows over %d tenants, want 1 over 1 -- "+
				"more than one tenant means the cross_tenant policy is applying without "+
				"SET ROLE, which is what GRANT ... WITH INHERIT FALSE prevents (cleat#1490)",
				total, distinctTenants)
		}
	})

	t.Run("a sweep that enters cleat_sweep sees every tenant", func(t *testing.T) {
		tx, err := rls.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer tx.Rollback()
		if _, err := tx.ExecContext(ctx, `SET LOCAL ROLE cleat_sweep`); err != nil {
			t.Fatalf("entering cleat_sweep (needs GRANT ... WITH INHERIT FALSE): %v", err)
		}
		var total int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM `+table).Scan(&total); err != nil {
			t.Fatalf("counting under the sweep role: %v", err)
		}
		if total != 2 {
			t.Errorf("the sweep saw %d rows, want 2 -- a sweep that sees fewer has been "+
				"narrowed to one tenant, which is the failure mode that returns rows "+
				"rather than an error", total)
		}
	})

	t.Run("no tenant and no sweep raises rather than returning nothing", func(t *testing.T) {
		tx, err := rls.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer tx.Rollback()
		var n int
		err = tx.QueryRowContext(ctx, `SELECT count(*) FROM `+table).Scan(&n)
		if err == nil {
			t.Fatalf("counted %d rows with no tenant set and no sweep; want an error. "+
				"A policy that filters instead of raising turns a missing tenant "+
				"context into 'no data', which is the failure assert_tenant_set exists "+
				"to prevent", n)
		}
		if !strings.Contains(err.Error(), "cleat.tenant_id is not set") {
			t.Errorf("error was %v; want cleat.tenant_id is not set", err)
		}
	})

	t.Run("the tenant predicate reaches the index", func(t *testing.T) {
		tx, err := rls.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer tx.Rollback()
		if _, err := tx.ExecContext(ctx,
			`SELECT set_config('cleat.tenant_id', $1, true)`, mine); err != nil {
			t.Fatalf("setting the tenant: %v", err)
		}
		rows, err := tx.QueryContext(ctx, `EXPLAIN SELECT k FROM `+table)
		if err != nil {
			t.Fatalf("EXPLAIN: %v", err)
		}
		defer rows.Close()
		var plan strings.Builder
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				t.Fatalf("scanning the plan: %v", err)
			}
			plan.WriteString(line)
			plan.WriteString("\n")
		}
		// Asserted as "not a Filter" rather than "is an Index Cond": this
		// fixture holds two rows, and the planner will choose a Seq Scan for a
		// table that small however good the predicate is. What must not appear
		// is the CASE landing as a per-row Filter -- the defect in cleat#1490.
		//
		// NOTE the shape of the old defect, because it is not what anyone
		// looks for: BOTH the old and new predicates plan as Index Only Scans
		// on a large table. Grepping a plan for "Seq Scan" to detect the
		// regression finds nothing and reads as fixed.
		if strings.Contains(plan.String(), "tenant_row_is_visible") {
			t.Errorf("the plan still references tenant_row_is_visible, so this table "+
				"carries the pre-1490 CASE policy:\n%s", plan.String())
		}
	})
}
