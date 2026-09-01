package engine

import (
	"database/sql"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

// The FaultInjector's database path had no coverage at all: every existing test
// in unit_test.go constructs NewFaultInjector(nil), so only the in-memory
// `active` map was ever exercised. That is what let the three ExecContext calls
// sit there with their errors discarded.
//
// The consequence is the one this repo cares most about. active[ft] was set
// unconditionally, so a fault whose write never reached the database was
// indistinguishable from one that landed, and IsActive reported true either way.
// A test asserting "the system recovers from a worker crash" would pass without
// any crash having been injected -- a green result that measured nothing, inside
// the harness built to catch exactly that.
//
// These tests assert the database actually changed, not merely that no error
// came back.

func faultInjectorTestDB(t *testing.T) (*sql.DB, string) {
	t.Helper()
	db := testutil.TestDB(t, testutil.DialectPostgres)
	testutil.SetupFullSchema(t, db, testutil.DialectPostgres)
	testutil.CleanupPostgresTestData(t, db)
	t.Cleanup(func() {
		testutil.CleanupPostgresTestData(t, db)
		db.Close()
	})

	tenant := "ffffffff-ffff-4fff-ffff-ffffffffffff"
	if _, err := db.Exec(
		`INSERT INTO workflow_defs (name, version, wasm_bytes, min_version, abi_version, tenant_id)
		 VALUES ('fi-def', 1, '\x0061736d', 1, 1, $1)`, tenant); err != nil {
		t.Fatalf("seed workflow_defs: %v", err)
	}
	return db, tenant
}

// TestInjectWorkerCrashActuallyReleasesWorkflows is the important one: releasing
// the workflows *is* the fault, so a write that did not happen means the crash
// did not happen.
func TestInjectWorkerCrashActuallyReleasesWorkflows(t *testing.T) {
	db, tenant := faultInjectorTestDB(t)

	const worker = "fi-worker-1"
	if _, err := db.Exec(
		`INSERT INTO workflow_instances (id, def_name, def_version, status, input, tenant_id, assigned_to, heartbeat_at)
		 VALUES ('fi-wf-1', 'fi-def', 1, 'running', '{}', $1, $2, now())`, tenant, worker); err != nil {
		t.Fatalf("seed workflow_instances: %v", err)
	}

	fi := NewFaultInjector(db)
	if err := fi.InjectWorkerCrash(worker); err != nil {
		t.Fatalf("InjectWorkerCrash: %v", err)
	}

	var status string
	var assigned sql.NullString
	if err := db.QueryRow(
		`SELECT status, assigned_to FROM workflow_instances WHERE id = 'fi-wf-1'`).
		Scan(&status, &assigned); err != nil {
		t.Fatalf("reading instance: %v", err)
	}
	if status != "ready" || assigned.Valid {
		t.Errorf("after InjectWorkerCrash: status=%q assigned_to=%v; want ready / NULL.\n\n"+
			"The fault reported success without changing the database, so any test "+
			"asserting crash recovery from here is measuring an untouched system.",
			status, assigned)
	}
	if !fi.IsActive(FaultWorkerCrash) {
		t.Error("IsActive(FaultWorkerCrash) is false after a successful injection")
	}
}

// TestInjectClockSkewActuallyMovesHeartbeats pairs with the above, and also
// covers Cleanup putting the heartbeat back -- a silent restore failure leaves
// every running instance with a future heartbeat_at, which the next test
// sharing the database inherits as an unexplained failure.
func TestInjectClockSkewActuallyMovesHeartbeats(t *testing.T) {
	db, tenant := faultInjectorTestDB(t)

	if _, err := db.Exec(
		`INSERT INTO workflow_instances (id, def_name, def_version, status, input, tenant_id, heartbeat_at)
		 VALUES ('fi-wf-2', 'fi-def', 1, 'running', '{}', $1, now())`, tenant); err != nil {
		t.Fatalf("seed workflow_instances: %v", err)
	}

	fi := NewFaultInjector(db)
	if err := fi.InjectClockSkew(2 * time.Hour); err != nil {
		t.Fatalf("InjectClockSkew: %v", err)
	}

	var ahead bool
	if err := db.QueryRow(
		`SELECT heartbeat_at > now() + interval '1 hour' FROM workflow_instances WHERE id = 'fi-wf-2'`).
		Scan(&ahead); err != nil {
		t.Fatalf("reading heartbeat: %v", err)
	}
	if !ahead {
		t.Error("InjectClockSkew reported success but heartbeat_at was not moved forward")
	}

	if err := fi.Cleanup(); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if err := db.QueryRow(
		`SELECT heartbeat_at > now() + interval '1 hour' FROM workflow_instances WHERE id = 'fi-wf-2'`).
		Scan(&ahead); err != nil {
		t.Fatalf("reading heartbeat after cleanup: %v", err)
	}
	if ahead {
		t.Error("Cleanup left heartbeat_at in the future; the next test sharing this " +
			"database inherits it as an unexplained failure")
	}
}

// TestFailedInjectionIsReportedAndNotMarkedActive is the half that the returned
// error exists for. Against a closed pool every write fails, so the injector
// must say so and must not claim the fault is active.
//
// Without this, the fix above is only half-tested: the tests over a working
// database would pass equally well against the old code that discarded errors.
func TestFailedInjectionIsReportedAndNotMarkedActive(t *testing.T) {
	db := testutil.TestDB(t, testutil.DialectPostgres)
	testutil.SetupFullSchema(t, db, testutil.DialectPostgres)
	db.Close() // every subsequent write fails

	fi := NewFaultInjector(db)

	if err := fi.InjectWorkerCrash("fi-worker-gone"); err == nil {
		t.Error("InjectWorkerCrash against a closed pool reported success")
	}
	if fi.IsActive(FaultWorkerCrash) {
		t.Error("IsActive(FaultWorkerCrash) is true after the injecting write failed. " +
			"That is the defect: a fault that never reached the database is " +
			"indistinguishable from one that did.")
	}

	if err := fi.InjectClockSkew(2 * time.Hour); err == nil {
		t.Error("InjectClockSkew against a closed pool reported success")
	}
	if fi.IsActive(FaultClockSkew) {
		t.Error("IsActive(FaultClockSkew) is true after the injecting write failed")
	}
}

// TestResetPropagatesCleanupError pins the other half of the Cleanup change.
//
// Reset is Cleanup under another name, and it discarded the error -- errcheck
// found it only because the call was bare; writing `_ = fi.Cleanup()` would
// have hidden it again, which is the blind spot recorded in §3.43.
//
// A silently failed Reset leaves every running instance with a future
// heartbeat_at, and the next test sharing the database inherits that as an
// unexplained failure -- the exact cost the Cleanup change was made to avoid.
func TestResetPropagatesCleanupError(t *testing.T) {
	db, tenant := faultInjectorTestDB(t)

	if _, err := db.Exec(
		`INSERT INTO workflow_instances (id, def_name, def_version, status, input, tenant_id, heartbeat_at)
		 VALUES ('fi-wf-reset', 'fi-def', 1, 'running', '{}', $1, now())`, tenant); err != nil {
		t.Fatalf("seed workflow_instances: %v", err)
	}

	fi := NewFaultInjector(db)
	if err := fi.InjectClockSkew(2 * time.Hour); err != nil {
		t.Fatalf("InjectClockSkew: %v", err)
	}

	// Break the injector's pool so the restore write cannot succeed, using a
	// SECOND handle to the same database. Closing `db` itself would take the
	// helper's t.Cleanup down with it, and the test would fail on teardown
	// rather than on its assertion.
	broken := testutil.TestDB(t, testutil.DialectPostgres)
	broken.Close()
	fi = NewFaultInjector(broken)
	if err := fi.InjectClockSkew(time.Hour); err == nil {
		t.Fatal("precondition: injecting against a closed pool should fail")
	}
	// Mark the fault active by hand so Cleanup has restore work to attempt;
	// the failed injection above deliberately did not.
	fi.active[FaultClockSkew] = true

	if err := fi.Reset(); err == nil {
		t.Error("Reset returned nil after its restore write failed. The caller now " +
			"believes the database was put back when it was not.")
	}
}
