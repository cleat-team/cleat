package testutil

import (
	"database/sql"
	"fmt"
	"testing"
	"time"
)

func seedInstanceForLeaseTest(t *testing.T, db *sql.DB, id string) {
	t.Helper()
	if _, err := db.Exec(`
		INSERT INTO workflow_defs (name, version, wasm_bytes)
		VALUES ('testutil-lease-seed-def', 1, '\x00'::bytea)
		ON CONFLICT (tenant_id, name, version) DO NOTHING
	`); err != nil {
		t.Fatalf("seeding workflow_defs for the lease fixture: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO workflow_instances (id, def_name, def_version, status)
		VALUES ($1, 'testutil-lease-seed-def', 1, 'ready')
	`, id); err != nil {
		t.Fatalf("seeding workflow_instances for %s: %v", id, err)
	}
}

func uniqueLeaseTestID(label string) string {
	return fmt.Sprintf("testutil-lease-%s-%d", label, time.Now().UnixNano())
}

func readLeaseRowForTest(t *testing.T, db *sql.DB, id string) (tenantID, taskQueue, status, assignedTo string) {
	t.Helper()
	var assigned sql.NullString
	if err := db.QueryRow(`
		SELECT tenant_id::text, task_queue, status, assigned_to FROM workflow_leases WHERE id = $1
	`, id).Scan(&tenantID, &taskQueue, &status, &assigned); err != nil {
		t.Fatalf("reading workflow_leases row for %s: %v", id, err)
	}
	return tenantID, taskQueue, status, assigned.String
}

// TestSeedWorkflowLeaseDefaultsAgreeWithWorkflowInstances is the reason this
// helper exists: every field left unset must come out the SAME as
// workflow_instances' own default, not merely some value, or a fixture that
// omits a field here would silently disagree with its own
// workflow_instances insert.
func TestSeedWorkflowLeaseDefaultsAgreeWithWorkflowInstances(t *testing.T) {
	db := TestDB(t, DialectPostgres)
	id := uniqueLeaseTestID("defaults")
	seedInstanceForLeaseTest(t, db, id)
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM workflow_instances WHERE id = $1`, id) })

	SeedWorkflowLease(t, db, id, WorkflowLeaseSeed{})

	tenantID, taskQueue, status, _ := readLeaseRowForTest(t, db, id)
	if tenantID != "00000000-0000-0000-0000-000000000000" {
		t.Errorf("tenant_id = %q, want the zero UUID (workflow_instances' own default)", tenantID)
	}
	if taskQueue != "default" {
		t.Errorf("task_queue = %q, want %q (workflow_instances' own default)", taskQueue, "default")
	}
	if status != "ready" {
		t.Errorf("status = %q, want %q (workflow_instances' own default)", status, "ready")
	}
}

// TestSeedWorkflowLeaseSetsExactlyTheFieldsGiven is the sharper companion:
// a field that IS set must land, and nothing else should change alongside
// it as a side effect of however the column list is built.
func TestSeedWorkflowLeaseSetsExactlyTheFieldsGiven(t *testing.T) {
	db := TestDB(t, DialectPostgres)
	id := uniqueLeaseTestID("explicit")
	seedInstanceForLeaseTest(t, db, id)
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM workflow_instances WHERE id = $1`, id) })

	SeedWorkflowLease(t, db, id, WorkflowLeaseSeed{
		TaskQueue:  "lease-test-queue",
		Status:     "running",
		AssignedTo: "lease-test-worker",
	})

	_, taskQueue, status, assignedTo := readLeaseRowForTest(t, db, id)
	if taskQueue != "lease-test-queue" {
		t.Errorf("task_queue = %q, want %q", taskQueue, "lease-test-queue")
	}
	if status != "running" {
		t.Errorf("status = %q, want %q", status, "running")
	}
	if assignedTo != "lease-test-worker" {
		t.Errorf("assigned_to = %q, want %q", assignedTo, "lease-test-worker")
	}
}

// TestSeedWorkflowLeaseIsIdempotent matters because a shared test helper
// gets called from fixtures that retry, and ON CONFLICT DO NOTHING is the
// whole reason this is safe to call twice rather than something to assume.
func TestSeedWorkflowLeaseIsIdempotent(t *testing.T) {
	db := TestDB(t, DialectPostgres)
	id := uniqueLeaseTestID("idempotent")
	seedInstanceForLeaseTest(t, db, id)
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM workflow_instances WHERE id = $1`, id) })

	SeedWorkflowLease(t, db, id, WorkflowLeaseSeed{TaskQueue: "q1"})
	SeedWorkflowLease(t, db, id, WorkflowLeaseSeed{TaskQueue: "q2"})

	_, taskQueue, _, _ := readLeaseRowForTest(t, db, id)
	if taskQueue != "q1" {
		t.Errorf("task_queue = %q after a second call, want %q (DO NOTHING on conflict, first call wins)", taskQueue, "q1")
	}
}

// TestWorkflowLeasesRequiresAMatchingInstanceRow is the negative control for
// the whole file: it proves workflow_leases' FK onto workflow_instances is
// real, which is what makes every call to SeedWorkflowLease above meaningful
// rather than a write into a table with no constraints. Exercises the raw
// INSERT directly rather than through SeedWorkflowLease, which calls
// t.Fatalf on error and would end this test rather than let it assert.
func TestWorkflowLeasesRequiresAMatchingInstanceRow(t *testing.T) {
	db := TestDB(t, DialectPostgres)
	id := uniqueLeaseTestID("orphan")

	_, err := db.Exec(`INSERT INTO workflow_leases (id) VALUES ($1)`, id)
	if err == nil {
		t.Fatal("inserting a workflow_leases row with no matching workflow_instances row succeeded, want a foreign key violation")
	}
}
