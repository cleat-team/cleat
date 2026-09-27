package engine

import (
	"errors"
	"testing"
)

// Deleting or disabling a schedule that does not exist must not report
// success. cleat#1297.
//
// All three stores discarded the statement result, so zero rows matched was
// indistinguishable from one and the API answered 200 {"status":"disabled"}
// for a name that was never there. An operator who mistypes a name during an
// incident is told the schedule is disabled while it keeps firing.
//
// These use the mock driver rather than a server because the property under
// test is the STORE's decision, not the database's: given a row count, does
// the method return not-found or nil? That is exactly what a fake can answer,
// and it lets the MySQL arm be tested on a machine with no MySQL.
func TestMySQLSetScheduleEnabledReportsNotFoundForAnAbsentSchedule(t *testing.T) {
	store := newMySQLStoreForTest(t,
		[]mockRowsResult{queryRowOk("SELECT count(*) FROM workflow_schedules", int64(0))},
		nil)

	err := store.SetScheduleEnabled(testCtx, "does-not-exist", false)
	if !errors.Is(err, ErrScheduleNotFound) {
		t.Errorf("SetScheduleEnabled on an absent schedule returned %v, want ErrScheduleNotFound.\n\n"+
			"A nil error here is what the API turns into 200 {\"status\":\"disabled\"}.", err)
	}
}

func TestMySQLDeleteScheduleReportsNotFoundForAnAbsentSchedule(t *testing.T) {
	store := newMySQLStoreForTest(t,
		[]mockRowsResult{queryRowOk("SELECT count(*) FROM workflow_schedules", int64(0))},
		nil)

	err := store.DeleteSchedule(testCtx, "does-not-exist")
	if !errors.Is(err, ErrScheduleNotFound) {
		t.Errorf("DeleteSchedule on an absent schedule returned %v, want ErrScheduleNotFound", err)
	}
}

// THE TRAP THIS FIX EXISTS TO AVOID, encoded so it cannot come back.
//
// The obvious implementation is `RowsAffected == 0 -> not found`. On MySQL an
// UPDATE that sets a column to the value it already holds reports **0**
// affected rows unless the connection sets CLIENT_FOUND_ROWS, which cleat does
// nowhere. So that implementation turns disabling an ALREADY-disabled schedule
// into a 404 -- on MySQL only, while passing on the dialect whoever wrote it
// was running.
//
// The exec result below is exactly that case: the row exists (count 1) and the
// UPDATE changes nothing (affected 0). The method must still succeed.
func TestMySQLDisablingAnAlreadyDisabledScheduleSucceeds(t *testing.T) {
	store := newMySQLStoreForTest(t,
		[]mockRowsResult{queryRowOk("SELECT count(*) FROM workflow_schedules", int64(1))},
		[]mockExecResult{{match: "UPDATE workflow_schedules SET enabled", affected: 0}})

	if err := store.SetScheduleEnabled(testCtx, "already-disabled", false); err != nil {
		t.Errorf("disabling an already-disabled schedule returned %v, want nil.\n\n"+
			"The row exists and the UPDATE changed nothing, which MySQL reports as 0 "+
			"affected rows without CLIENT_FOUND_ROWS. Reading that as not-found makes "+
			"an idempotent re-disable a 404 on this dialect alone -- and idempotent "+
			"pause is the asserted behaviour upstream (temporalio/sdk-go "+
			"TestSchedulePause).", err)
	}
}

// The mirror: a schedule that exists and does change is unaffected by the new
// check. Without this, a version of the check that returned not-found for
// everything would pass the two tests above.
func TestMySQLSetScheduleEnabledSucceedsForAPresentSchedule(t *testing.T) {
	store := newMySQLStoreForTest(t,
		[]mockRowsResult{queryRowOk("SELECT count(*) FROM workflow_schedules", int64(1))},
		[]mockExecResult{{match: "UPDATE workflow_schedules SET enabled", affected: 1}})

	if err := store.SetScheduleEnabled(testCtx, "present", true); err != nil {
		t.Errorf("SetScheduleEnabled on a present schedule returned %v, want nil", err)
	}
}

func TestMySQLDeleteScheduleSucceedsForAPresentSchedule(t *testing.T) {
	store := newMySQLStoreForTest(t,
		[]mockRowsResult{queryRowOk("SELECT count(*) FROM workflow_schedules", int64(1))},
		[]mockExecResult{{match: "DELETE FROM workflow_schedules", affected: 1}})

	if err := store.DeleteSchedule(testCtx, "present"); err != nil {
		t.Errorf("DeleteSchedule on a present schedule returned %v, want nil", err)
	}
}
