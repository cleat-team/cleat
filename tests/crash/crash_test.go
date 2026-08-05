package crash

import (
	"database/sql"
	"strings"
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

// TestCrashWithIdempotencyKeysDoesNotDuplicate is IMPROVEMENT-PLAN §1.4 phase B,
// measured the same way as the crash test above and differing in exactly one
// respect: the service honours Idempotency-Key.
//
// Same crash, same window, same worker. The interrupted call is still re-issued
// on recovery — the engine cannot know it succeeded — but it arrives carrying
// the key the original attempt used, so the service returns the original
// outcome instead of charging again.
//
//	without keys (the test above): Reserve=1 Charge=1 Ship=2
//	with keys (this test):         Reserve=1 Charge=1 Ship=1
//
// That difference is the entire value of phase B, and it is why this assertion
// is on work performed rather than on requests received. The service still
// receives the duplicate request; what it does not do is act on it twice.
func TestCrashWithIdempotencyKeysDoesNotDuplicate(t *testing.T) {
	db := ownerDB(t)
	defer db.Close()

	suffix := uniqueSuffix()
	taskQueue := "queue-idem-" + suffix
	wfID := "idem-wf-" + suffix
	orderID := "order-" + suffix

	deployFixture(t, db, taskQueue)
	bin := buildWorker(t)

	svc := newChargeService(t)
	svc.honourKeys = true
	release := svc.holdOperation("Ship")

	first := startWorker(t, bin, taskQueue, svc.srv.URL)
	startWorkflow(t, db, wfID, orderID, taskQueue)

	svc.awaitHeldCall(t, first, startBudget)

	first.kill()
	release()

	second := startWorker(t, bin, taskQueue, svc.srv.URL)

	status, errMsg := awaitTerminal(t, db, wfID, completeBudget)
	if status != "done" && status != "completed" {
		t.Fatalf("workflow ended %q (%s)\n--- worker log ---\n%s", status, errMsg, second.output())
	}

	reserve, charge, ship := svc.allCounts()
	t.Logf("with idempotency keys: Reserve=%d Charge=%d Ship=%d; %d requests, %d distinct keys",
		reserve, charge, ship, svc.total, svc.keyCount())

	// The service must actually have been sent keys. Without this the test
	// passes trivially against a worker that sends none and simply never
	// duplicated — and the mechanism under test would be untested.
	if svc.keyCount() == 0 {
		t.Fatalf("the service received no Idempotency-Key at all, so this test " +
			"proves nothing: the worker's ServiceCaller does not implement " +
			"engine.IdempotentCaller")
	}

	if ship != 1 {
		t.Errorf("Ship=%d, want 1: the interrupted call was re-issued with the "+
			"key its first attempt used, so a key-honouring service must not "+
			"perform the charge twice", ship)
	}
	if reserve != 1 || charge != 1 {
		t.Errorf("Reserve=%d Charge=%d, want 1/1", reserve, charge)
	}
}

// TestCrashWithWriteAheadIntentDoesNotRepeatTheCall is the design doc's T3
// (docs/durable-call-intent-design.md §7), and the evidence IMPROVEMENT-PLAN
// 1.4 phase D shipped without: a real SIGKILL, a real worker, and a service
// that counts what it was actually asked to do.
//
// Same fixture and same crash window as
// TestCrashMidDurableCallReplaysOnlyTheUnrecordedStep. The single difference is
// that this worker is started with --write-ahead-intent-ops payments.Ship, so
// the third call commits a pending event before it dispatches. The counts are
// what separate the two contracts:
//
//	              Ship  outcome
//	at-least-once   2   done         the interrupted call is repeated, silently
//	write-ahead     1   failed       the interrupted call is NOT repeated, and
//	                                 replay says the outcome is unknown
//
// The second row is the whole point of the feature. A charge that may already
// have happened is not retried; it is reported. That the workflow *fails* is
// the correct outcome and not a defect -- an unresolvable ambiguity is exactly
// what phase E exists to give an operator a way to settle.
//
// Non-vacuity: with --write-ahead-intent-ops removed this becomes the test
// above and Ship is 2, which is asserted rather than assumed by the sibling
// test running in the same package against the same fixture.
func TestCrashWithWriteAheadIntentDoesNotRepeatTheCall(t *testing.T) {
	db := ownerDB(t)
	defer db.Close()

	suffix := uniqueSuffix()
	taskQueue := "queue-crash-intent-" + suffix
	wfID := "crash-intent-wf-" + suffix
	orderID := "order-intent-" + suffix

	deployFixture(t, db, taskQueue)
	bin := buildWorker(t)

	svc := newChargeService(t)
	release := svc.holdOperation("Ship")

	const intentFlag = "payments.Ship"
	first := startWorker(t, bin, taskQueue, svc.srv.URL, "--write-ahead-intent-ops", intentFlag)
	startWorkflow(t, db, wfID, orderID, taskQueue)

	// The third call has reached the service and is committed there. This is
	// the window a durable call cannot see into.
	svc.awaitHeldCall(t, first, startBudget)

	if r, c, s := svc.allCounts(); r != 1 || c != 1 || s != 1 {
		t.Fatalf("before the crash: Reserve=%d Charge=%d Ship=%d, want 1/1/1", r, c, s)
	}

	// The pending row must be on disk *now*, before the crash -- that is the
	// guarantee, and reading it here rather than after the restart is what
	// makes this an assertion about ordering rather than about outcomes.
	pending := pendingIntentCount(t, db, wfID)
	durable := eventCount(t, db, wfID)
	if pending != 1 {
		t.Fatalf("%d pending intent rows while the call is in flight, want 1 (durable events: %d) -- "+
			"the side effect happened before its intent was committed", pending, durable)
	}

	first.kill()
	release()

	second := startWorker(t, bin, taskQueue, svc.srv.URL, "--write-ahead-intent-ops", intentFlag)

	status, errMsg := awaitTerminal(t, db, wfID, completeBudget)
	reserve, charge, ship := svc.allCounts()
	t.Logf("after recovery: status=%q Reserve=%d Charge=%d Ship=%d; pending at crash: %d, durable: %d",
		status, reserve, charge, ship, pending, durable)

	if ship != 1 {
		t.Errorf("Ship=%d, want 1.\n\n"+
			"The call was in flight when the worker died and its outcome is unknown, so the one "+
			"thing recovery must not do is perform it again. Ship=2 means the intent row was "+
			"written but not consulted on replay; the detector reads it from "+
			"intent_at IS NOT NULL AND checksum IS NULL.\n--- worker log ---\n%s",
			ship, second.output())
	}
	if reserve != 1 || charge != 1 {
		t.Errorf("Reserve=%d Charge=%d, want 1/1: the two completed calls were durable and replay "+
			"should have skipped them", reserve, charge)
	}

	// What the workflow's terminal state SHOULD be is "failed, naming the
	// ambiguity". It is not asserted here, because it is not what happens, and
	// a test that documents a defect by passing is worse than no test.
	//
	// Measured: the engine detects the ambiguity and reports it to the guest,
	// the guest reports an error back through cleat_complete(status=1), and a
	// second cleat_complete(status=0) then overwrites it -- so the workflow is
	// recorded `done` with a result it never produced. See IMPROVEMENT-PLAN
	// 3.22. Turning the two lines below into assertions is that item's
	// acceptance test.
	t.Logf("terminal state: status=%q error_msg=%q -- 3.22: the ambiguity is detected and then "+
		"discarded above the engine, so this reads as success", status, errMsg)
	if status != "done" && status != "completed" && !strings.Contains(errMsg, "AMBIGUOUS") {
		t.Logf("NOTE: the workflow did not complete and did not name the ambiguity either (%q/%q); "+
			"if 3.22 has been fixed, replace this with the assertion described above", status, errMsg)
	}
}

// pendingIntentCount counts the workflow's write-ahead intent rows that have
// not been completed: the exact predicate the engine and LoadEventHistory use.
func pendingIntentCount(t *testing.T, db *sql.DB, id string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(
		`SELECT count(*) FROM event_history
		 WHERE workflow_id = $1 AND intent_at IS NOT NULL AND checksum IS NULL`, id).Scan(&n); err != nil {
		t.Fatalf("counting pending intents for %s: %v", id, err)
	}
	return n
}
