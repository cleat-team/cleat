package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
)

// cleat#3245 B4: testutil.SeedRun is meant to be the only place a PostgreSQL
// fixture writes workflow_instances, so B5 changes one statement when it drops
// the redundant columns. That only holds if a seeded run is the run the store
// would have made. These tests compare the two rows by rows, and then use the
// seeded run through the store.

// seedComparable reads one table's row for id as JSON, without the columns
// that differ between any two runs: id, and the two set to now() at insert.
func seedComparable(t *testing.T, db *sql.DB, table, id string) string {
	t.Helper()
	var s string
	if err := db.QueryRow(`SELECT (to_jsonb(r) - 'id' - 'created_at' - 'next_wake_at')::text
		FROM `+table+` r WHERE id = $1`, id).Scan(&s); err != nil {
		t.Fatalf("read %s %s: %v", table, id, err)
	}
	return s
}

func TestASeededRunHasTheLeaseAndPayloadRowsStartNewRunWrites(t *testing.T) {
	store, db := newRunTestStore(t)
	started := startPayloadReaderRun(t, store, "b4-seed-compare")

	seeded := "b4-seeded-" + t.Name()
	testutil.SeedRun(t, db, testutil.RunSeed{
		ID: seeded, TenantID: DefaultTenantUUID, DefName: "b4-seed-compare", Input: `{"from":"start"}`,
	})

	for _, table := range []string{"workflow_leases", "workflow_payloads"} {
		if got, want := seedComparable(t, db, table, seeded), seedComparable(t, db, table, started); got != want {
			t.Errorf("%s: the seeded run's row differs from StartNewRun's\n seeded:  %s\n started: %s", table, got, want)
		}
	}
}

// The instance row is what B5 left: id, tenant_id, parent_close_policy,
// event_count and history_swept_at. The seed writes the first three, and the
// two counters stay at their defaults, as they do for a run StartNewRun makes.
func TestASeededRunWritesOnlyTheNarrowInstanceColumns(t *testing.T) {
	store, db := newRunTestStore(t)
	startPayloadReaderRun(t, store, "b4-narrow") // deploys the def the foreign key needs
	id := "b4-narrow-" + t.Name()
	testutil.SeedRun(t, db, testutil.RunSeed{
		ID: id, TenantID: DefaultTenantUUID, DefName: "b4-narrow",
		Input: `{"x":1}`, TaskQueue: "b4-q", Status: "running", AssignedTo: "w", Generation: 7,
		ParentWorkflowID: "b4-parent", ParentClosePolicy: "TERMINATE",
	})
	defaults := "b4-defaults-" + t.Name()
	if _, err := db.Exec(`INSERT INTO workflow_instances (id, tenant_id, parent_close_policy)
		VALUES ($1, $2, 'TERMINATE')`, defaults, DefaultTenantUUID); err != nil {
		t.Fatalf("insert the all-defaults row: %v", err)
	}
	if got, want := seedComparable(t, db, "workflow_instances", id), seedComparable(t, db, "workflow_instances", defaults); got != want {
		t.Errorf("SeedRun wrote a workflow_instances column outside the narrow set\n seeded:   %s\n defaults: %s", got, want)
	}
	var queue, status, assigned, parent string
	var gen int64
	if err := db.QueryRow(`SELECT task_queue, status, assigned_to, generation, parent_workflow_id FROM workflow_leases WHERE id = $1`, id).
		Scan(&queue, &status, &assigned, &gen, &parent); err != nil {
		t.Fatalf("read the lease row: %v", err)
	}
	if queue != "b4-q" || status != "running" || assigned != "w" || gen != 7 || parent != "b4-parent" {
		t.Errorf("lease row = (%q, %q, %q, %d, %q), want (b4-q, running, w, 7, b4-parent)", queue, status, assigned, gen, parent)
	}
}

func TestASeededRunIsReadAndClaimedThroughTheStore(t *testing.T) {
	store, db := newRunTestStore(t)
	ctx := context.Background()
	startPayloadReaderRun(t, store, "b4-seed-claim") // deploys the def
	clearInstances(t, db)

	id := "b4-claim-" + t.Name()
	testutil.SeedRun(t, db, testutil.RunSeed{ID: id, TenantID: DefaultTenantUUID, DefName: "b4-seed-claim", Input: `{"seeded":true}`})

	wf, err := store.GetWorkflowByID(ctx, id)
	if err != nil || wf == nil {
		t.Fatalf("GetWorkflowByID(%s) = %v, %v", id, wf, err)
	}
	var in map[string]any
	if err := json.Unmarshal(wf.Input, &in); err != nil || in["seeded"] != true {
		t.Errorf("input read back as %s (%v), want {\"seeded\":true}", wf.Input, err)
	}
	claimed, err := store.ClaimWorkflow(ctx, "b4-worker")
	if err != nil || claimed == nil || claimed.ID != id {
		t.Fatalf("ClaimWorkflow = %v, %v; want %s", claimed, err, id)
	}
}
