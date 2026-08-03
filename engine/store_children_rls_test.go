package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

// TestStartChildWorkflowAtomicUnderRLS is a regression test for a defect that
// every existing test was structurally incapable of catching.
//
// StartChildWorkflowAtomic writes two rows in one transaction. The
// workflow_instances insert passes s.tenantID; the event_history insert did
// not name the tenant_id column at all, leaving it to the column default
// (the zero UUID).
//
// That is not a cosmetic omission. migrations/postgres/001_schema.sql declares
//
//	CREATE POLICY tenant_isolation_events ON event_history
//	    FOR ALL USING (tenant_id = cleat.assert_tenant_set());
//
// with no explicit WITH CHECK. PostgreSQL reuses the USING expression as the
// WITH CHECK expression in that case, so the row is REJECTED on INSERT rather
// than stored under the wrong tenant -- and because the insert shares a
// transaction with the child's workflow_instances row, the whole transaction
// aborts. Child workflows cannot be spawned at all.
//
// Why nothing noticed: PostgreSQL never applies RLS to a superuser, and
// CLEAT_TEST_DB/CLEAT_TEST_POSTGRES conventionally point at one (the postgres
// image's POSTGRES_USER is a superuser). Every other test therefore exercises
// this path with RLS switched off in effect. The one deployment that does
// enforce it -- the cluster compose file, which connects workers as the
// NOSUPERUSER/NOBYPASSRLS cleat_app role -- has no child-workflow coverage.
// So the defect sat between the two.
//
// This test closes that gap by driving the real store through
// testutil.OpenPostgresRLSTestDB, a connection that is neither superuser nor
// table owner and so cannot bypass RLS. Against the unfixed code it fails with
// "new row violates row-level security policy for table \"event_history\"".
func TestStartChildWorkflowAtomicUnderRLS(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping RLS integration test in short mode")
	}

	adminDB := testutil.TestDB(t, testutil.DialectPostgres)
	defer adminDB.Close()
	testutil.SetupFullSchema(t, adminDB, testutil.DialectPostgres)
	testutil.CleanupPostgresTestData(t, adminDB)

	appDB := testutil.OpenPostgresRLSTestDB(t, adminDB)
	defer appDB.Close()
	// Deferred after appDB.Close() so it runs first (defers are LIFO).
	defer testutil.CleanupPostgresTestData(t, adminDB)

	ctx := context.Background()
	tenant := "cccccccc-cccc-4ccc-cccc-cccccccccccc"
	store := NewPostgresStore(appDB).WithTenant(tenant)

	const defName = "rls-child-parent"
	const childDefName = "rls-child-child"
	for _, name := range []string{defName, childDefName} {
		def := &WorkflowDef{
			Name:       name,
			Version:    1,
			WASMBytes:  []byte{0x00, 0x61, 0x73, 0x6d},
			ABIVersion: 1,
			MinVersion: 1,
		}
		if err := store.DeployWorkflowDef(ctx, def); err != nil {
			t.Fatalf("DeployWorkflowDef(%s): %v", name, err)
		}
	}

	parentID := fmt.Sprintf("rls-child-parent-%d", time.Now().UnixNano())
	if _, _, err := store.StartNewRun(ctx, parentID, defName, 1, json.RawMessage(`{}`), "", tenant, 0); err != nil {
		t.Fatalf("StartNewRun(parent): %v", err)
	}

	childID := fmt.Sprintf("rls-child-child-%d", time.Now().UnixNano())
	event := EventRecord{
		Step:        1,
		EventType:   EventTypeChildWorkflow,
		TimestampMs: time.Now().UnixMilli(),
		ChildName:   childDefName,
		ChildInput:  `{"k":"v"}`,
	}

	gotID, err := store.StartChildWorkflowAtomic(ctx, childID, parentID, childDefName, `{"k":"v"}`, 1, "ABANDON", event, 0)
	if err != nil {
		// Name the specific failure so a future reader does not have to
		// rediscover which of the two inserts RLS rejected.
		if strings.Contains(err.Error(), "row-level security") {
			t.Fatalf("StartChildWorkflowAtomic rejected by RLS -- the event_history insert is "+
				"not passing tenant_id, so the row fails the tenant_isolation_events WITH CHECK "+
				"and aborts the transaction: %v", err)
		}
		t.Fatalf("StartChildWorkflowAtomic: %v", err)
	}
	if gotID != childID {
		t.Errorf("StartChildWorkflowAtomic returned id %q, want %q", gotID, childID)
	}

	// Verify through adminDB, not appDB. adminDB is the superuser connection,
	// which PostgreSQL always exempts from RLS, so it reports what was actually
	// stored. Reading back through appDB would only prove the row is visible to
	// this tenant -- a row written under the wrong tenant would simply be
	// filtered out and read as "missing", which is a weaker and more confusing
	// signal than reading the tenant_id and comparing it.
	var childTenant string
	if err := adminDB.QueryRow(
		`SELECT tenant_id::text FROM workflow_instances WHERE id = $1`, childID,
	).Scan(&childTenant); err != nil {
		t.Fatalf("reading child workflow_instances row: %v", err)
	}
	if childTenant != tenant {
		t.Errorf("child instance tenant_id = %q, want %q", childTenant, tenant)
	}

	// The point of the test: the event row must exist AND carry the real
	// tenant. Asserting only "the call returned nil" would pass against a
	// version that stored the row under the zero UUID, which is the other
	// way this defect could have manifested had the policy carried an
	// explicit permissive WITH CHECK.
	var eventTenant, eventRunID string
	err = adminDB.QueryRow(
		`SELECT tenant_id::text, COALESCE(run_id, '') FROM event_history WHERE workflow_id = $1 AND step = $2`,
		parentID, event.Step,
	).Scan(&eventTenant, &eventRunID)
	if err != nil {
		t.Fatalf("reading parent event_history row (step %d): %v", event.Step, err)
	}
	if eventTenant != tenant {
		t.Errorf("event_history tenant_id = %q, want %q", eventTenant, tenant)
	}
	if eventRunID != childID {
		t.Errorf("event_history run_id = %q, want child id %q", eventRunID, childID)
	}
}
