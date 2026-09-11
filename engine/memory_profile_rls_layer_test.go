package engine

// Layer-separation proof for cleat#1098:
// migrations/postgres/061_the_memory_profile_has_a_policy_behind_its_predicate.sql
// adds Row-Level Security to workflow_memory_stats and workflow_memory_samples.
//
// CLAUDE.md's standing requirement for this class: prove the DB policy blocks
// cross-tenant access ON ITS OWN (Go filter removed), and prove the Go filter
// blocks it ON ITS OWN (policy bypassed). A test driving the store methods over
// a superuser connection passes for the wrong reason -- PostgreSQL never applies
// RLS to a superuser, and CLEAT_TEST_POSTGRES conventionally points at one.
//
// WHY THIS EXISTS RATHER THAN A SECOND ASSERTION IN THE EXISTING TEST.
// TestTheMemoryProfileIsScopedToTenant already asserts that one tenant cannot
// see another's memory profile, and it passed before this migration -- the Go
// predicate was doing all the work. Adding a policy that nothing exercises is
// indistinguishable from adding nothing, and cleat#1096 is the precedent for
// why that matters: on SQL Server, with the Go predicates removed, isolation
// still held because the policy alone carried it. PostgreSQL had no such
// property, and the same experiment would have returned every tenant's rows.
//
// The two halves here are that experiment, run in both directions.

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

// applyMemoryProfileRLSMigration applies this fix's migration directly, the way
// apply031RLSGapMigration does for its own. Redundant with testutil.TestDB,
// which now runs the whole migrations/postgres/ directory -- and kept anyway,
// because it states the dependency locally instead of relying on the reader to
// know that. The statements are idempotent (DROP POLICY IF EXISTS ... CREATE
// POLICY), so reapplying is a no-op.
func applyMemoryProfileRLSMigration(t *testing.T, db *sql.DB) {
	t.Helper()
	path := filepath.Join("..", "migrations", "postgres",
		"061_the_memory_profile_has_a_policy_behind_its_predicate.sql")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if _, err := db.Exec(string(data)); err != nil {
		t.Fatalf("apply %s: %v", path, err)
	}
}

func TestMemoryProfileRLS_LayerSeparation(t *testing.T) {
	adminDB := testutil.TestDB(t, testutil.DialectPostgres)
	defer adminDB.Close()
	testutil.SetupFullSchema(t, adminDB, testutil.DialectPostgres)
	testutil.CleanupPostgresTestData(t, adminDB)
	defer testutil.CleanupPostgresTestData(t, adminDB)
	applyMemoryProfileRLSMigration(t, adminDB)

	ctx := context.Background()
	const tenantA = "e0000000-0000-4000-8000-00000000000a"
	const tenantB = "e0000000-0000-4000-8000-00000000000b"
	defA := fmt.Sprintf("rls-mem-a-%d", time.Now().UnixNano())
	defB := fmt.Sprintf("rls-mem-b-%d", time.Now().UnixNano())

	// Seeded through the STORE, not raw SQL, so the write path is covered too:
	// RecordWorkflowMemorySample now opens an RLS transaction, and a policy it
	// could not satisfy would fail here rather than in the reads below.
	for tenant, def := range map[string]string{tenantA: defA, tenantB: defB} {
		if err := NewPostgresStore(adminDB).WithTenant(tenant).
			RecordWorkflowMemorySample(ctx, def, 1024); err != nil {
			t.Fatalf("RecordWorkflowMemorySample(%s): %v", tenant, err)
		}
	}
	t.Cleanup(func() {
		bg := context.Background()
		for _, tenant := range []string{tenantA, tenantB} {
			_, _ = adminDB.ExecContext(bg, `DELETE FROM workflow_memory_samples WHERE tenant_id = $1`, tenant)
			_, _ = adminDB.ExecContext(bg, `DELETE FROM workflow_memory_stats WHERE tenant_id = $1`, tenant)
		}
	})

	// --- Layer 1: the POLICY alone, with no Go filter at all. ---
	//
	// appDB is neither superuser nor table owner, so FORCE ROW LEVEL SECURITY
	// applies unconditionally. These queries carry NO tenant_id predicate --
	// standing in for a store method that forgot one -- so if they return only
	// tenant A's rows, the policy is what did it.
	appDB := testutil.OpenPostgresRLSTestDB(t, adminDB)
	defer appDB.Close()
	assertNotSuperuserBypass(t, appDB)

	conn, err := appDB.Conn(ctx)
	if err != nil {
		t.Fatalf("acquire connection: %v", err)
	}
	defer conn.Close()
	setSessionTenant(t, ctx, conn, tenantA)

	for _, table := range []string{"workflow_memory_stats", "workflow_memory_samples"} {
		rows, err := conn.QueryContext(ctx, `SELECT def_name FROM `+table) //nolint:gosec // table is a literal from the loop above
		if err != nil {
			t.Fatalf("query %s with no tenant predicate: %v", table, err)
		}
		var seen []string
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				rows.Close()
				t.Fatalf("scan %s: %v", table, err)
			}
			seen = append(seen, name)
		}
		rows.Close()
		if len(seen) != 1 || seen[0] != defA {
			t.Errorf("a tenant A session selecting from %s with NO WHERE tenant_id saw %v, want "+
				"exactly [%s].\n\nThe policy is not filtering this query, so the Go predicate is "+
				"the only layer -- which is the state cleat#1098 exists to leave behind.",
				table, seen, defA)
		}
	}

	// --- Layer 2: the Go predicate alone, with the policy bypassed. ---
	//
	// adminDB is a superuser connection, so the policy just applied cannot act
	// on it -- this is "policy removed" without dropping it. LoadMemoryEstimates
	// carries its own `WHERE tenant_id = $1`, so isolation here must come from
	// that.
	estA, err := NewPostgresStore(adminDB).WithTenant(tenantA).LoadMemoryEstimates(ctx)
	if err != nil {
		t.Fatalf("LoadMemoryEstimates(A): %v", err)
	}
	if _, ok := estA[defA]; !ok {
		t.Fatalf("CONTROL FAILED: tenant A cannot see its OWN memory estimate (%v). Nothing "+
			"below distinguishes isolation from a query that returns nothing at all.", estA)
	}
	if _, ok := estA[defB]; ok {
		t.Errorf("tenant A's store, over a superuser connection where the RLS policy is bypassed, "+
			"saw tenant B's estimate for %s -- the Go-level tenant_id filter is not isolating on "+
			"its own", defB)
	}
}
