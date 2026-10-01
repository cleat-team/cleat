package catalogdiff

// cleat#2432. The same method as
// the_mssql_snapshot_sees_the_attributes_it_selects_test.go, applied to four
// classes mssql.go did not compare at all until this change: security
// policies/predicates (SQL Server's row-level security), triggers, schemas,
// and database roles. Each case builds two scratch databases that differ in
// exactly one of these, confirms the difference independently of the
// comparator under test, and requires Diff to report it.
//
// These are deliberately NOT built through mssqlDiffCase: that helper issues
// each side's whole DDL string as one batch, and CREATE FUNCTION/TRIGGER/
// SCHEMA must be the first statement in their batch. Issuing each statement as
// its own db.ExecContext call sidesteps that restriction without needing the
// EXEC('...') indirection plugin/migration.go uses for the same reason.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/migration"
)

// mssqlExecAll runs each statement in stmts as its own batch against db,
// failing the test immediately on the first error.
func mssqlExecAll(t *testing.T, ctx context.Context, db *sql.DB, stmts ...string) {
	t.Helper()
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			t.Fatalf("exec %q: %v", s, err)
		}
	}
}

func TestTheMSSQLSnapshotSeesSecurityPolicies(t *testing.T) {
	scratchName := fmt.Sprintf("cleat_2432_rls_%d", time.Now().UnixNano()%1_000_000_000)
	a := mssqlScratchDB(t, scratchName+"_a")
	b := mssqlScratchDB(t, scratchName+"_b")
	ctx := context.Background()

	const table = `CREATE TABLE rls_target (id INT PRIMARY KEY, tenant_id UNIQUEIDENTIFIER NOT NULL);`
	mssqlExecAll(t, ctx, a, table)
	mssqlExecAll(t, ctx, b, table)

	// Only A gets the predicate function and the policy that binds it.
	mssqlExecAll(t, ctx, a,
		`CREATE FUNCTION dbo.cleat_2432_filter(@tenant_id UNIQUEIDENTIFIER)
		 RETURNS TABLE WITH SCHEMABINDING AS
		 RETURN SELECT 1 AS access WHERE @tenant_id = @tenant_id`,
		`CREATE SECURITY POLICY dbo.cleat_2432_policy
		 ADD FILTER PREDICATE dbo.cleat_2432_filter(tenant_id) ON dbo.rls_target
		 WITH (STATE = ON)`,
	)

	var countA, countB int
	if err := a.QueryRowContext(ctx, `SELECT COUNT(*) FROM sys.security_policies`).Scan(&countA); err != nil {
		t.Fatalf("count A's security policies: %v", err)
	}
	if err := b.QueryRowContext(ctx, `SELECT COUNT(*) FROM sys.security_policies`).Scan(&countB); err != nil {
		t.Fatalf("count B's security policies: %v", err)
	}
	if countA == 0 || countB != 0 {
		t.Fatalf("the two databases do not actually differ in security-policy count (A=%d B=%d), "+
			"so a zero diff would prove nothing", countA, countB)
	}
	t.Logf("established independently: A has %d security polic(ies), B has %d", countA, countB)

	ca, err := Snapshot(ctx, a, migration.DialectMSSQL)
	if err != nil {
		t.Fatalf("snapshot A: %v", err)
	}
	cb, err := Snapshot(ctx, b, migration.DialectMSSQL)
	if err != nil {
		t.Fatalf("snapshot B: %v", err)
	}
	d := Diff(ca, cb)
	if len(d) == 0 {
		t.Fatalf("Diff reported ZERO differences over a pair that demonstrably differs in " +
			"security policies -- the comparator is still blind to cleat#2432's gap")
	}
	joined := strings.Join(d, "\n")
	if !strings.Contains(joined, "cleat_2432_policy") && !strings.Contains(joined, "ROWSECURITY") {
		t.Fatalf("Diff reported differences but none names the policy or a ROWSECURITY line:\n%s", joined)
	}
	t.Logf("Diff reported %d line(s):\n%s", len(d), joined)
}

func TestTheMSSQLSnapshotSeesTriggers(t *testing.T) {
	scratchName := fmt.Sprintf("cleat_2432_trig_%d", time.Now().UnixNano()%1_000_000_000)
	a := mssqlScratchDB(t, scratchName+"_a")
	b := mssqlScratchDB(t, scratchName+"_b")
	ctx := context.Background()

	const table = `CREATE TABLE trig_target (id INT PRIMARY KEY);`
	mssqlExecAll(t, ctx, a, table)
	mssqlExecAll(t, ctx, b, table)

	mssqlExecAll(t, ctx, a,
		`CREATE TRIGGER dbo.trg_cleat_2432 ON dbo.trig_target AFTER INSERT AS BEGIN SET NOCOUNT ON; END`,
	)

	var countA, countB int
	if err := a.QueryRowContext(ctx, `SELECT COUNT(*) FROM sys.triggers WHERE name = 'trg_cleat_2432'`).Scan(&countA); err != nil {
		t.Fatalf("count A's triggers: %v", err)
	}
	if err := b.QueryRowContext(ctx, `SELECT COUNT(*) FROM sys.triggers WHERE name = 'trg_cleat_2432'`).Scan(&countB); err != nil {
		t.Fatalf("count B's triggers: %v", err)
	}
	if countA == 0 || countB != 0 {
		t.Fatalf("the two databases do not actually differ in trigger presence (A=%d B=%d), "+
			"so a zero diff would prove nothing", countA, countB)
	}
	t.Logf("established independently: A has the trigger, B does not")

	ca, err := Snapshot(ctx, a, migration.DialectMSSQL)
	if err != nil {
		t.Fatalf("snapshot A: %v", err)
	}
	cb, err := Snapshot(ctx, b, migration.DialectMSSQL)
	if err != nil {
		t.Fatalf("snapshot B: %v", err)
	}
	d := Diff(ca, cb)
	if len(d) == 0 {
		t.Fatalf("Diff reported ZERO differences over a pair that demonstrably differs in " +
			"triggers -- the routine query's type filter is still blind to TR")
	}
	joined := strings.Join(d, "\n")
	if !strings.Contains(joined, "trg_cleat_2432") {
		t.Fatalf("Diff reported differences but none names the trigger:\n%s", joined)
	}
	t.Logf("Diff reported %d line(s):\n%s", len(d), joined)
}

func TestTheMSSQLSnapshotSeesSchemas(t *testing.T) {
	scratchName := fmt.Sprintf("cleat_2432_schema_%d", time.Now().UnixNano()%1_000_000_000)
	a := mssqlScratchDB(t, scratchName+"_a")
	b := mssqlScratchDB(t, scratchName+"_b")
	ctx := context.Background()

	mssqlExecAll(t, ctx, a, `CREATE SCHEMA cleat_2432_schema`)

	var countA, countB int
	if err := a.QueryRowContext(ctx, `SELECT COUNT(*) FROM sys.schemas WHERE name = 'cleat_2432_schema'`).Scan(&countA); err != nil {
		t.Fatalf("count A's schemas: %v", err)
	}
	if err := b.QueryRowContext(ctx, `SELECT COUNT(*) FROM sys.schemas WHERE name = 'cleat_2432_schema'`).Scan(&countB); err != nil {
		t.Fatalf("count B's schemas: %v", err)
	}
	if countA == 0 || countB != 0 {
		t.Fatalf("the two databases do not actually differ in schema presence (A=%d B=%d), "+
			"so a zero diff would prove nothing", countA, countB)
	}
	t.Logf("established independently: A has the schema, B does not")

	ca, err := Snapshot(ctx, a, migration.DialectMSSQL)
	if err != nil {
		t.Fatalf("snapshot A: %v", err)
	}
	cb, err := Snapshot(ctx, b, migration.DialectMSSQL)
	if err != nil {
		t.Fatalf("snapshot B: %v", err)
	}
	d := Diff(ca, cb)
	if len(d) == 0 {
		t.Fatalf("Diff reported ZERO differences over a pair that demonstrably differs in " +
			"schemas -- Catalog.Schemas is still not being compared")
	}
	joined := strings.Join(d, "\n")
	if !strings.Contains(joined, "cleat_2432_schema") {
		t.Fatalf("Diff reported differences but none names the schema:\n%s", joined)
	}
	t.Logf("Diff reported %d line(s):\n%s", len(d), joined)
}

func TestTheMSSQLSnapshotSeesRoles(t *testing.T) {
	scratchName := fmt.Sprintf("cleat_2432_role_%d", time.Now().UnixNano()%1_000_000_000)
	a := mssqlScratchDB(t, scratchName+"_a")
	b := mssqlScratchDB(t, scratchName+"_b")
	ctx := context.Background()

	mssqlExecAll(t, ctx, a, `CREATE ROLE cleat_2432_role`)

	var countA, countB int
	if err := a.QueryRowContext(ctx, `SELECT COUNT(*) FROM sys.database_principals WHERE name = 'cleat_2432_role'`).Scan(&countA); err != nil {
		t.Fatalf("count A's roles: %v", err)
	}
	if err := b.QueryRowContext(ctx, `SELECT COUNT(*) FROM sys.database_principals WHERE name = 'cleat_2432_role'`).Scan(&countB); err != nil {
		t.Fatalf("count B's roles: %v", err)
	}
	if countA == 0 || countB != 0 {
		t.Fatalf("the two databases do not actually differ in role presence (A=%d B=%d), "+
			"so a zero diff would prove nothing", countA, countB)
	}
	t.Logf("established independently: A has the role, B does not")

	ca, err := Snapshot(ctx, a, migration.DialectMSSQL)
	if err != nil {
		t.Fatalf("snapshot A: %v", err)
	}
	cb, err := Snapshot(ctx, b, migration.DialectMSSQL)
	if err != nil {
		t.Fatalf("snapshot B: %v", err)
	}
	d := Diff(ca, cb)
	if len(d) == 0 {
		t.Fatalf("Diff reported ZERO differences over a pair that demonstrably differs in " +
			"roles -- Catalog.Roles is still not being compared")
	}
	joined := strings.Join(d, "\n")
	if !strings.Contains(joined, "cleat_2432_role") {
		t.Fatalf("Diff reported differences but none names the role:\n%s", joined)
	}
	t.Logf("Diff reported %d line(s):\n%s", len(d), joined)
}
