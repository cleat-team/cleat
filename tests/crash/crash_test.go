package crash

import (
	"testing"
	"time"
)

// TestCrashMidDurableCallReplaysOnlyTheUnrecordedStep is IMPROVEMENT-PLAN §2.4.
//
// The fixture makes three sequential durable calls. The worker is SIGKILLed
// with the third genuinely in flight: the service has recorded it and has not
// yet replied, which is the window a durable call cannot see into. A new worker
// then picks the workflow up.
//
// Three calls rather than one, because the counts discriminate between two
// hypotheses a single call cannot separate:
//
//	Reserve=1 Charge=1 Ship=2  — the documented contract. Steps 1 and 2 were
//	                             durably recorded, so replay resumed at step 3
//	                             and only the interrupted call ran twice.
//	Reserve=2 Charge=2 Ship=2  — nothing was durably recorded, so replay
//	                             re-executed the entire workflow.
//
// docs/durable-calls.md §1-2 promises the first. Anything else means durable
// history is not durable, and every crash re-runs every side effect the
// workflow has ever performed — which is a different and much larger contract
// violation than the documented one.
func TestCrashMidDurableCallReplaysOnlyTheUnrecordedStep(t *testing.T) {
	db := ownerDB(t)
	defer db.Close()

	suffix := uniqueSuffix()
	taskQueue := "queue-crash-" + suffix
	wfID := "crash-wf-" + suffix
	orderID := "order-" + suffix

	deployFixture(t, db, taskQueue)
	bin := buildWorker(t)

	svc := newChargeService(t)
	release := svc.holdOperation("Ship")

	first := startWorker(t, bin, taskQueue, svc.srv.URL)
	startWorkflow(t, db, wfID, orderID, taskQueue)

	// Wait until the third call has actually reached the service. Killing
	// earlier would test a crash before dispatch, which is the easy case.
	svc.awaitHeldCall(t, first, startBudget)

	if r, c, s := svc.allCounts(); r != 1 || c != 1 || s != 1 {
		t.Fatalf("before the crash: Reserve=%d Charge=%d Ship=%d, want 1/1/1 — "+
			"the workflow did not reach the third call cleanly", r, c, s)
	}

	// How much of the workflow was durable at the moment of the crash. Read
	// before the restart, because finalize_workflow_status deletes the history
	// of a terminal workflow (003_procedures.sql:134), so after completion this
	// is always 0 and says nothing.
	durable := eventCount(t, db, wfID)

	// The crash. The worker dies holding an HTTP response it never received.
	first.kill()

	// Release the held request so the handler goroutine does not outlive the
	// test. Nothing is listening — the worker is gone.
	release()

	second := startWorker(t, bin, taskQueue, svc.srv.URL)

	status, errMsg := awaitTerminal(t, db, wfID, completeBudget)
	if status != "done" && status != "completed" {
		t.Fatalf("workflow ended %q (%s) after the restart\n--- worker log ---\n%s",
			status, errMsg, second.output())
	}

	reserve, charge, ship := svc.allCounts()
	t.Logf("after recovery: Reserve=%d Charge=%d Ship=%d; events durable at crash time: %d",
		reserve, charge, ship, durable)

	if ship != 2 {
		t.Errorf("Ship=%d, want 2: the interrupted call must be retried exactly "+
			"once (at-least-once, per docs/durable-calls.md §1)", ship)
	}

	if reserve != 1 || charge != 1 {
		t.Errorf("Reserve=%d Charge=%d, want 1/1.\n\n"+
			"Both completed and returned before the crash, so both should have "+
			"been in event_history and replay should have skipped them. "+
			"Re-running them means the engine persisted no history at all, and "+
			"every crash re-executes every prior side effect — not the "+
			"at-least-once-per-step contract docs/durable-calls.md documents.\n"+
			"Events durably recorded at the moment of the crash: %d (expected 2).",
			reserve, charge, durable)
	}

	if durable < 2 {
		t.Errorf("only %d events were durable when the worker died, want >= 2: "+
			"two calls had completed, so two events should have been persisted",
			durable)
	}
}

// TestCleanRunCallsEachOperationOnce is the control.
//
// Without it, the test above passes against a worker that duplicates every call
// regardless of crashes — a worse bug that would look identical in the counts.
func TestCleanRunCallsEachOperationOnce(t *testing.T) {
	db := ownerDB(t)
	defer db.Close()

	suffix := uniqueSuffix()
	taskQueue := "queue-clean-" + suffix
	wfID := "clean-wf-" + suffix
	orderID := "order-" + suffix

	deployFixture(t, db, taskQueue)
	bin := buildWorker(t)

	svc := newChargeService(t)
	w := startWorker(t, bin, taskQueue, svc.srv.URL)
	startWorkflow(t, db, wfID, orderID, taskQueue)

	status, errMsg := awaitTerminal(t, db, wfID, completeBudget)
	if status != "done" && status != "completed" {
		t.Fatalf("workflow ended %q (%s)\n--- worker log ---\n%s", status, errMsg, w.output())
	}

	if r, c, s := svc.allCounts(); r != 1 || c != 1 || s != 1 {
		t.Errorf("clean run: Reserve=%d Charge=%d Ship=%d, want 1/1/1 — "+
			"duplication is not specific to the crash\n%s\n--- worker log ---\n%s",
			r, c, s, svc.diagnose(), w.output())
	}
}

// TestEventsArePersistedDuringExecution isolates the durability question from
// the crash entirely.
//
// A workflow is held at its third call and, while it is still running, the test
// asks the database how many events exist. Two calls have completed and
// returned, so two events must be there. This needs no crash and no restart: if
// it fails, per-step persistence is broken outright, and every conclusion the
// crash test reaches about recovery is downstream of that.
func TestEventsArePersistedDuringExecution(t *testing.T) {
	db := ownerDB(t)
	defer db.Close()

	suffix := uniqueSuffix()
	taskQueue := "queue-persist-" + suffix
	wfID := "persist-wf-" + suffix
	orderID := "order-" + suffix

	deployFixture(t, db, taskQueue)
	bin := buildWorker(t)

	svc := newChargeService(t)
	release := svc.holdOperation("Ship")
	defer release()

	w := startWorker(t, bin, taskQueue, svc.srv.URL)
	startWorkflow(t, db, wfID, orderID, taskQueue)

	svc.awaitHeldCall(t, w, startBudget)

	// The workflow is now blocked inside its third call, with the first two
	// complete. Give the flusher a moment: the adaptive flusher batches with an
	// 8ms window, so this is generous by three orders of magnitude.
	time.Sleep(2 * time.Second)

	n := eventCount(t, db, wfID)
	if n < 2 {
		t.Errorf("event_history has %d rows for a workflow that has completed two "+
			"durable calls, want >= 2.\n\n"+
			"Per-step flush is not reaching the database. engine/lifecycle.go:179 "+
			"logs the flush error and continues, so the workflow runs to "+
			"completion and reports success with no durable history at all.\n"+
			"--- worker log ---\n%s", n, w.output())
	}
}
