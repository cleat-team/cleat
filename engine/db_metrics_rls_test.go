package engine

import (
	"context"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

// TestMetricsQueriesWorkUnderRLS covers the two db_metrics.go queries that read
// RLS-forced tables on the raw connection pool.
//
// CountStalledWorkflows reads workflow_instances and CountEventHistoryTotal
// reads event_history. Both carry FORCE ROW LEVEL SECURITY with
// `tenant_id = cleat.assert_tenant_set()`, and both queries ran on s.db rather
// than through beginTxWithRLS, so nothing had called set_config on the
// connection. Measured against a real cleat_rls_test_role connection with one
// genuinely stalled workflow present:
//
//	ground truth (superuser):  1
//	CountStalledWorkflows:     0, error "cleat.tenant_id is not set" (P0001)
//
// Unlike the defect in §3.44 this one *checks* its error, so it fails loudly
// rather than reporting a confident wrong number -- metrics break in an
// RLS-enforcing deployment instead of lying about it. That is the better of the
// two failure modes and still a bug: the cluster compose file connects workers
// as the NOSUPERUSER/NOBYPASSRLS cleat_app role, and these are the counts an
// operator watches to notice stalled work.
//
// Why it went unnoticed: every other test connects as a superuser, which
// PostgreSQL exempts from RLS entirely, so the raw-pool read succeeds there.
//
// A note for anyone re-deriving this. The policy is evaluated per candidate row,
// so with an empty table the read returns 0 rows and never calls
// assert_tenant_set at all -- the query looks fine. Seeding a matching row is
// what makes the failure appear, and my first two attempts at this test seeded
// nothing (the def insert failed on a column that does not exist) and therefore
// "passed" against the broken code.
func TestMetricsQueriesWorkUnderRLS(t *testing.T) {
	adminDB := testutil.TestDB(t, testutil.DialectPostgres)
	defer adminDB.Close()
	testutil.SetupFullSchema(t, adminDB, testutil.DialectPostgres)
	testutil.CleanupPostgresTestData(t, adminDB)
	defer testutil.CleanupPostgresTestData(t, adminDB)

	ctx := context.Background()
	tenant := "eeeeeeee-eeee-4eee-eeee-eeeeeeeeeeee"

	// Seed as the superuser so the fixture itself is not subject to the
	// behaviour under test.
	if _, err := adminDB.Exec(
		`INSERT INTO workflow_defs (name, version, wasm_bytes, min_version, abi_version, tenant_id)
		 VALUES ('rls-metrics-def', 1, '\x0061736d', 1, 1, $1)`, tenant); err != nil {
		t.Fatalf("seed workflow_defs: %v", err)
	}
	stalled := time.Now().Add(-2 * time.Hour)
	if _, err := adminDB.Exec(
		`INSERT INTO workflow_instances
		   (id, def_name, def_version, status, input, tenant_id, created_at, heartbeat_at)
		 VALUES ('rls-metrics-wf', 'rls-metrics-def', 1, 'running', '{}', $1, $2, $2)`,
		tenant, stalled); err != nil {
		t.Fatalf("seed workflow_instances: %v", err)
	}
	if _, err := adminDB.Exec(
		`INSERT INTO event_history (workflow_id, step, event_type, tenant_id)
		 VALUES ('rls-metrics-wf', 1, 'call', $1)`, tenant); err != nil {
		t.Fatalf("seed event_history: %v", err)
	}

	// Guard the fixture. Without a matching row the RLS policy is never
	// evaluated and this test passes against the unfixed code.
	var truth int
	if err := adminDB.QueryRow(
		`SELECT COUNT(*) FROM workflow_instances WHERE status = 'running' AND tenant_id = $1`,
		tenant).Scan(&truth); err != nil {
		t.Fatalf("reading ground truth: %v", err)
	}
	if truth != 1 {
		t.Fatalf("fixture seeded %d stalled workflows, want 1; with none, the policy is "+
			"never evaluated and this test cannot fail", truth)
	}

	appDB := testutil.OpenPostgresRLSTestDB(t, adminDB)
	defer appDB.Close()
	store := NewPostgresStore(appDB).WithTenant(tenant)

	n, err := store.CountStalledWorkflows(ctx, time.Minute)
	if err != nil {
		t.Errorf("CountStalledWorkflows under a non-superuser role: %v\n\n"+
			"The query reads workflow_instances on s.db, the raw pool, so no set_config "+
			"has run on that connection and assert_tenant_set raises. Route it through "+
			"beginTxWithRLS.", err)
	} else if n != truth {
		t.Errorf("CountStalledWorkflows = %d, want %d", n, truth)
	}

	events, err := store.CountEventHistoryTotal(ctx)
	if err != nil {
		t.Errorf("CountEventHistoryTotal under a non-superuser role: %v", err)
	} else if events != 1 {
		t.Errorf("CountEventHistoryTotal = %d, want 1", events)
	}

	// EstimateEventHistorySize reads pg_total_relation_size, which touches no
	// rows, so it is unaffected by RLS. Asserted so a future change that makes
	// it read rows does not slip through with the other two now covered.
	if _, err := store.EstimateEventHistorySize(ctx); err != nil {
		t.Errorf("EstimateEventHistorySize under a non-superuser role: %v", err)
	}
}
