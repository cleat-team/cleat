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

// TestSeedWorkflowLeaseAlsoSeedsWorkflowPayloads is the regression test for
// cleat#3245 Phase 3 step 3 piece F's finding: this helper's own doc comment
// called it a "dual-write" mirror of StartNewRun before it actually was one.
// GetWorkflowByID/ListWorkflows/CountWorkflows now INNER JOIN
// workflow_leases to workflow_payloads, so a caller that only got the lease
// row from this function would see every seeded workflow report "not
// found" -- tests/scale's TestMaxConcurrentWorkflows did exactly that
// before this fix, with all 1000 of its seeded rows.
func TestSeedWorkflowLeaseAlsoSeedsWorkflowPayloads(t *testing.T) {
	db := TestDB(t, DialectPostgres)
	id := uniqueLeaseTestID("payloads")
	seedInstanceForLeaseTest(t, db, id)
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM workflow_instances WHERE id = $1`, id) })

	SeedWorkflowLease(t, db, id, WorkflowLeaseSeed{})

	var tenantID string
	var input string
	if err := db.QueryRow(`SELECT tenant_id::text, input::text FROM workflow_payloads WHERE id = $1`, id).
		Scan(&tenantID, &input); err != nil {
		t.Fatalf("SeedWorkflowLease did not create a matching workflow_payloads row for %s: %v", id, err)
	}
	if tenantID != "00000000-0000-0000-0000-000000000000" {
		t.Errorf("workflow_payloads.tenant_id = %q, want the zero UUID, matching the lease row's own default", tenantID)
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

// cleat#3245 B1a: the payload row must carry the instance's own payload
// columns. SeedWorkflowLease used to write input = '{}' whatever the
// instance held, and once the claim read input from workflow_payloads,
// tests/exhaustion's guest started with no input at all.
func TestSeedWorkflowLeaseCopiesTheInstancesPayloadColumns(t *testing.T) {
	db := TestDB(t, DialectPostgres)
	id := uniqueLeaseTestID("payload-copy")
	seedInstanceForLeaseTest(t, db, id)
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM workflow_instances WHERE id = $1`, id) })
	if _, err := db.Exec(`UPDATE workflow_instances SET input = '{"iterations": 7}', trace_id = 'trace-7' WHERE id = $1`, id); err != nil {
		t.Fatalf("set instance input: %v", err)
	}

	SeedWorkflowLease(t, db, id, WorkflowLeaseSeed{})

	var input, traceID string
	if err := db.QueryRow(`SELECT input::text, COALESCE(trace_id, '') FROM workflow_payloads WHERE id = $1`, id).
		Scan(&input, &traceID); err != nil {
		t.Fatalf("read workflow_payloads: %v", err)
	}
	if input != `{"iterations": 7}` || traceID != "trace-7" {
		t.Fatalf("workflow_payloads input=%s trace_id=%q, want the instance's {\"iterations\": 7} and trace-7", input, traceID)
	}
}

// SyncWorkflowPayload overwrites an existing payload row from a later
// fixture write to workflow_instances.
func TestSyncWorkflowPayloadCarriesALaterInstanceWrite(t *testing.T) {
	db := TestDB(t, DialectPostgres)
	id := uniqueLeaseTestID("payload-sync")
	seedInstanceForLeaseTest(t, db, id)
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM workflow_instances WHERE id = $1`, id) })
	SeedWorkflowLease(t, db, id, WorkflowLeaseSeed{})
	if _, err := db.Exec(`UPDATE workflow_instances SET completed_at = now() - interval '3 hours' WHERE id = $1`, id); err != nil {
		t.Fatalf("set instance completed_at: %v", err)
	}

	SyncWorkflowPayload(t, db, id)

	var older bool
	if err := db.QueryRow(`SELECT completed_at < now() - interval '2 hours' FROM workflow_payloads WHERE id = $1`, id).Scan(&older); err != nil {
		t.Fatalf("read workflow_payloads.completed_at: %v", err)
	}
	if !older {
		t.Fatal("workflow_payloads.completed_at did not take the instance's later write")
	}
}

// TestSyncWorkflowLeaseCreatesAndOverwritesTheLeaseRow covers both shapes a
// fixture hands it: an instance inserted raw, with no lease row at all, and an
// instance whose status was then changed raw, behind an existing lease row.
func TestSyncWorkflowLeaseCreatesAndOverwritesTheLeaseRow(t *testing.T) {
	db := TestDB(t, DialectPostgres)
	id := uniqueLeaseTestID("sync")
	seedInstanceForLeaseTest(t, db, id)
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM workflow_instances WHERE id = $1`, id) })

	SyncWorkflowLease(t, db, id)
	if _, _, status, _ := readLeaseRowForTest(t, db, id); status != "ready" {
		t.Fatalf("after the first sync, workflow_leases.status = %q, want the instance's ready", status)
	}

	if _, err := db.Exec(`UPDATE workflow_instances SET status = 'done', assigned_to = 'w-sync' WHERE id = $1`, id); err != nil {
		t.Fatalf("change the instance: %v", err)
	}
	SyncWorkflowLease(t, db, id)
	if _, _, status, assigned := readLeaseRowForTest(t, db, id); status != "done" || assigned != "w-sync" {
		t.Fatalf("after the second sync, workflow_leases = (%q, %q), want (done, w-sync) copied from the instance", status, assigned)
	}
}
