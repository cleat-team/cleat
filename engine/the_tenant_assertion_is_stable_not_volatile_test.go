package engine

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
)

// TestTheTenantAssertionIsStableSoAPolicyCanReachAnIndex is cleat#1488, migration 071.
//
// Every RLS policy in this schema is `USING (tenant_id = cleat.assert_tenant_set())`.
// If that function is VOLATILE -- which is what plpgsql gives a function that
// names no volatility, and what it was for thirty-four migrations -- PostgreSQL
// may not put the call in an index condition and may not hoist it, so the
// tenant isolation predicate can only ever run as a per-row filter over a heap
// scan. Every one of those tables carries a tenant_id-leading index that the
// predicate cannot reach.
//
// TWO ASSERTIONS, AND THE SECOND IS THE ONE THAT CANNOT GO VACUOUS.
//
// The catalog assertion -- provolatile = 's' -- is exact and cheap, and it is
// the one that goes red if someone reverts the marker. It is also the weaker
// of the two, because it asserts a letter rather than a consequence: a reader
// has to already believe the story above for it to mean anything.
//
// So the plan assertion runs the consequence, and it runs BOTH SIDES. The same
// table, the same statistics, the same statement shape, twice: once through the
// shipped function and once through a byte-identical local copy declared
// VOLATILE. The copy must produce a Seq Scan. If it does not, this test fails --
// because a run where both sides reach the index has not shown that the marker
// matters, it has shown that the fixture is too small for the planner to care,
// and that is exactly the shape that passes forever while measuring nothing.
//
// WHAT THIS DOES NOT TEST, because the rest of the suite already does. STABLE
// would be a security bug if it let a tenant id be cached across statements, so
// the two properties that would break are checked here directly -- an unset
// tenant still RAISEs, and a tenant changed between two statements of one
// transaction is followed. Cross-tenant visibility through a real policy and a
// real non-superuser role is TestATenantRoleSeesOnlyItsOwnRows and the rls_*
// tests, which exercise this function on every postgres run.
func TestTheTenantAssertionIsStableSoAPolicyCanReachAnIndex(t *testing.T) {
	db := testutil.TestDB(t, testutil.DialectPostgres)
	t.Cleanup(func() { db.Close() })
	testutil.SetupFullSchema(t, db, testutil.DialectPostgres)

	ctx := context.Background()

	// ---- 1. the catalog says STABLE -------------------------------------
	var volatility string
	err := db.QueryRowContext(ctx, `
		SELECT p.provolatile
		FROM pg_proc p
		JOIN pg_namespace n ON n.oid = p.pronamespace
		WHERE n.nspname = 'cleat' AND p.proname = 'assert_tenant_set'`).Scan(&volatility)
	if err == sql.ErrNoRows {
		t.Fatal("cleat.assert_tenant_set() is not present in pg_proc -- the schema " +
			"this test measures was not applied, so nothing below would mean anything")
	}
	if err != nil {
		t.Fatalf("reading pg_proc for cleat.assert_tenant_set: %v", err)
	}
	if volatility != "s" {
		t.Fatalf("cleat.assert_tenant_set() is provolatile=%q, want \"s\" (STABLE).\n"+
			"  Every RLS policy in this schema calls this function, and a VOLATILE\n"+
			"  one may not appear in an index condition -- so every tenant-scoped\n"+
			"  query falls back to a sequential scan past the tenant_id-leading\n"+
			"  indexes the schema builds for exactly that predicate.\n"+
			"  See migrations/postgres/071 for the measurement.", volatility)
	}

	// ---- 2. the plan reaches an index, and a VOLATILE twin does not ------
	//
	// Named per-process: engine tests share one development database with other
	// checkouts, so a fixed name collides with a concurrent run rather than with
	// stale state.
	suffix := fmt.Sprintf("%d", os.Getpid())
	table := "vol_probe_" + suffix
	twin := "vol_probe_volatile_twin_" + suffix
	t.Cleanup(func() {
		db.ExecContext(ctx, `DROP TABLE IF EXISTS `+table)
		db.ExecContext(ctx, `DROP FUNCTION IF EXISTS `+twin+`()`)
	})

	// 20,000 rows over 200 tenants. The number is not arbitrary and it is not
	// load-bearing either: the VOLATILE twin below is what proves it is large
	// enough on whatever machine this runs on.
	for _, stmt := range []string{
		`DROP TABLE IF EXISTS ` + table,
		`CREATE TABLE ` + table + ` (id int, tenant_id uuid)`,
		`INSERT INTO ` + table + `
		   SELECT g, ('00000000-0000-0000-0000-' || lpad(((g % 200) + 1)::text, 12, '0'))::uuid
		   FROM generate_series(1, 20000) g`,
		`CREATE INDEX ` + table + `_tenant_idx ON ` + table + ` (tenant_id)`,
		`ANALYZE ` + table,
		`DROP FUNCTION IF EXISTS ` + twin + `()`,
		// Byte-identical to migration 034's body. The ONLY difference from the
		// function under test is the word VOLATILE.
		`CREATE FUNCTION ` + twin + `() RETURNS uuid AS $$
		 DECLARE tid text;
		 BEGIN
		     tid := current_setting('cleat.tenant_id', true);
		     IF tid IS NULL OR tid = '' THEN
		         RAISE EXCEPTION 'cleat.tenant_id is not set';
		     END IF;
		     RETURN tid::uuid;
		 END;
		 $$ LANGUAGE plpgsql VOLATILE`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("fixture %.60q...: %v", stmt, err)
		}
	}

	subject := explainOneQuery(ctx, t, db,
		`EXPLAIN (COSTS OFF) SELECT id FROM `+table+` WHERE tenant_id = cleat.assert_tenant_set()`)
	control := explainOneQuery(ctx, t, db,
		`EXPLAIN (COSTS OFF) SELECT id FROM `+table+` WHERE tenant_id = `+twin+`()`)

	// The known-positive FIRST. A run where the control reaches the index has
	// not measured the marker at all, and reading the subject's plan after that
	// would be reading a number whose denominator failed.
	if !strings.Contains(control, "Seq Scan") {
		t.Fatalf("the VOLATILE control did NOT produce a sequential scan, so this "+
			"test cannot tell the two states apart on this machine and its green "+
			"would mean nothing.\n  control plan:\n%s", indentPlan(control))
	}
	if strings.Contains(subject, "Seq Scan") {
		t.Fatalf("the tenant isolation predicate fell back to a sequential scan "+
			"even though cleat.assert_tenant_set() reports STABLE.\n"+
			"  subject plan:\n%s\n  VOLATILE control plan (expected, and identical):\n%s",
			indentPlan(subject), indentPlan(control))
	}

	// ---- 3. STABLE has not cached anything it must not -------------------
	//
	// One connection, one transaction, the tenant changed between statements.
	// This is the property that would make STABLE a security bug rather than an
	// optimisation, so it is checked rather than reasoned about.
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("checking out a connection: %v", err)
	}
	defer conn.Close()

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()

	const (
		tenantA = "aaaaaaaa-0000-0000-0000-0000000000a1"
		tenantB = "bbbbbbbb-0000-0000-0000-0000000000b1"
	)
	for _, step := range []struct{ set, want string }{
		{tenantA, tenantA},
		{tenantB, tenantB}, // <- a cached value would still report tenantA here
		{tenantA, tenantA},
	} {
		if _, err := tx.ExecContext(ctx, `SELECT set_config('cleat.tenant_id', $1, true)`, step.set); err != nil {
			t.Fatalf("set_config(%s): %v", step.set, err)
		}
		var got string
		if err := tx.QueryRowContext(ctx, `SELECT cleat.assert_tenant_set()::text`).Scan(&got); err != nil {
			t.Fatalf("assert_tenant_set after setting %s: %v", step.set, err)
		}
		if got != step.want {
			t.Fatalf("after setting cleat.tenant_id to %s the assertion returned %s.\n"+
				"  STABLE promises one value per STATEMENT, not per transaction. A value "+
				"that survives a set_config within one transaction is a cross-tenant read.",
				step.set, got)
		}
	}

	// ---- 4. and it still REFUSES ----------------------------------------
	//
	// A policy that stopped raising would look exactly like a policy that got
	// faster, which is the reason this is here and not left to inference.
	if _, err := tx.ExecContext(ctx, `SELECT set_config('cleat.tenant_id', '', true)`); err != nil {
		t.Fatalf("clearing cleat.tenant_id: %v", err)
	}
	var unreachable string
	err = tx.QueryRowContext(ctx, `SELECT cleat.assert_tenant_set()::text`).Scan(&unreachable)
	if err == nil {
		t.Fatalf("cleat.assert_tenant_set() returned %q with no tenant set; it must RAISE", unreachable)
	}
	if !strings.Contains(err.Error(), "cleat.tenant_id is not set") {
		t.Fatalf("cleat.assert_tenant_set() failed with an unexpected error, so this "+
			"control did not measure the refusal it exists to measure: %v", err)
	}
}

func explainOneQuery(ctx context.Context, t *testing.T, db *sql.DB, query string) string {
	t.Helper()
	// The planner needs a tenant in scope: without one the STABLE call is still
	// planned, but an executed EXPLAIN would raise. EXPLAIN without ANALYZE does
	// not execute, so this is belt and braces -- and it costs nothing.
	if _, err := db.ExecContext(ctx,
		`SELECT set_config('cleat.tenant_id', '00000000-0000-0000-0000-000000000007', false)`); err != nil {
		t.Fatalf("set tenant for EXPLAIN: %v", err)
	}
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		t.Fatalf("EXPLAIN: %v\n  query: %s", err, query)
	}
	defer rows.Close()

	var lines []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scanning EXPLAIN output: %v", err)
		}
		lines = append(lines, line)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading EXPLAIN output: %v", err)
	}
	if len(lines) == 0 {
		t.Fatalf("EXPLAIN returned no rows, so there is no plan to assert on: %s", query)
	}
	return strings.Join(lines, "\n")
}

func indentPlan(s string) string {
	return "    " + strings.ReplaceAll(s, "\n", "\n    ")
}
