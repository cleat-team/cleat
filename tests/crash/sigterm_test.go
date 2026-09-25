package crash

import (
	"database/sql"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// cleat#2285: SIGTERM must drain, then cancel. It used to cancel at once, which aborted every in-flight
// durable wait and wrote the run FAILED (a terminal status nothing reclaims), so a rolling deploy lost every
// run it interrupted. SIGKILL (crash_test.go) is the ungraceful case and stays as it was; these are the
// graceful one, and each scenario counts the service's calls across BOTH workers, because "the run was not
// failed" is half of the contract and "no side effect was repeated or invented" is the other half.
//
//	a  the run finishes inside the grace          done; every call exactly once
//	b  a call outlives the grace                  released, never failed; the interrupted call replays (at-least-once)
//	c  a saga is cut off mid-backoff              its compensation NEVER runs; released; another worker finishes it
//	g  a run with a defer is cut off              the cleanup does not run on the worker that is going away
//	d  a finalize is in flight at the grace end   released (row lock held across the boundary), not failed
//	e  a continue-as-new is in flight likewise    exactly one continuation, nothing failed
//	h  a genuine failure during the drain         still recorded as FAILED: the drain does not swallow real errors
//	j  the admin drain, then SIGTERM              the drain is a cordon; the run in flight still finishes
//	m  a defer phase is cut off mid-backoff        released still terminating; the next worker completes the cleanup
//	   the draining worker keeps heartbeating     the run is not reclaimed and repeated while it drains
//
// A call that outlasts the grace still runs to its own timeout on the departing worker, and another worker may
// replay it meanwhile: the overlap is recorded as #2287, not fixed here.

func awaitExit(t *testing.T, ch <-chan error, budget time.Duration, w *worker) {
	t.Helper()
	select {
	case err := <-ch:
		if err != nil {
			t.Errorf("worker exited with %v after SIGTERM, want a clean exit\n--- worker log ---\n%s", err, w.output())
		}
	case <-time.After(budget):
		t.Fatalf("worker had not exited %v after SIGTERM\n--- worker log ---\n%s", budget, w.output())
	}
}

func runRow(t *testing.T, db *sql.DB, id string) (status, errMsg string) {
	t.Helper()
	var msg sql.NullString
	if err := db.QueryRow(`SELECT status, error_msg FROM workflow_instances WHERE id = $1`, id).Scan(&status, &msg); err != nil {
		t.Fatalf("reading %s: %v", id, err)
	}
	return status, msg.String
}

func requireReleased(t *testing.T, db *sql.DB, id string, w *worker) {
	t.Helper()
	status, msg := runRow(t, db, id)
	if status != "ready" && status != "running" {
		t.Fatalf("a run cut off by SIGTERM is %q (%s), want it released for another worker\n--- worker log ---\n%s", status, msg, w.output())
	}
}

func requireDone(t *testing.T, db *sql.DB, id string, budget time.Duration, w *worker) {
	t.Helper()
	status, msg := awaitTerminal(t, db, id, budget)
	if status != "done" && status != "completed" {
		t.Fatalf("run ended %q (%s), want done\n--- worker log ---\n%s", status, msg, w.output())
	}
}

func TestSIGTERM_a_LetsAnInFlightRunFinishInsideTheGrace(t *testing.T) {
	db := ownerDB(t)
	defer db.Close()
	suffix := uniqueSuffix()
	taskQueue, wfID := "queue-term-a-"+suffix, "term-a-wf-"+suffix

	deployFixture(t, db, taskQueue)
	bin := buildWorker(t)
	svc := newChargeService(t)
	release := svc.holdOperation("Ship")
	defer release()

	w := startWorker(t, bin, taskQueue, svc.srv.URL, "--shutdown-grace", "30s")
	startWorkflow(t, db, wfID, "order-"+suffix, taskQueue)
	svc.awaitHeldCall(t, w, startBudget)

	exited := w.term()
	select {
	case err := <-exited:
		t.Fatalf("worker exited (%v) while a run was in flight and 30s of grace remained\n--- worker log ---\n%s", err, w.output())
	case <-time.After(2 * time.Second):
	}
	release()
	awaitExit(t, exited, 30*time.Second, w)

	requireDone(t, db, wfID, 10*time.Second, w)
	if r, c, s := svc.allCounts(); r != 1 || c != 1 || s != 1 {
		t.Errorf("Reserve=%d Charge=%d Ship=%d, want 1/1/1: a graceful shutdown must not repeat a side effect", r, c, s)
	}
}

func TestSIGTERM_b_ReleasesARunThatOutlastsTheGraceInsteadOfFailingIt(t *testing.T) {
	db := ownerDB(t)
	defer db.Close()
	suffix := uniqueSuffix()
	taskQueue, wfID := "queue-term-b-"+suffix, "term-b-wf-"+suffix

	deployFixture(t, db, taskQueue)
	bin := buildWorker(t)
	svc := newChargeService(t)
	release := svc.holdOperation("Ship")
	defer release()

	first := startWorker(t, bin, taskQueue, svc.srv.URL, "--shutdown-grace", "2s")
	startWorkflow(t, db, wfID, "order-"+suffix, taskQueue)
	svc.awaitHeldCall(t, first, startBudget)

	exited := first.term()
	// Let the 2s grace expire with the call still in flight, then let the call return to a worker that has by
	// now been cancelled. That is the moment the old code wrote the run FAILED.
	time.Sleep(4 * time.Second)
	release()
	awaitExit(t, exited, 30*time.Second, first)

	requireReleased(t, db, wfID, first)

	second := startWorker(t, bin, taskQueue, svc.srv.URL)
	requireDone(t, db, wfID, completeBudget, second)
	r, c, s := svc.allCounts()
	t.Logf("Reserve=%d Charge=%d Ship=%d", r, c, s)
	if r != 1 || c != 1 {
		t.Errorf("Reserve=%d Charge=%d, want 1/1: the calls that had finished must not be repeated", r, c)
	}
	if s < 1 || s > 2 {
		t.Errorf("Ship=%d, want 1 or 2: at-least-once, the interrupted call may replay once", s)
	}
}

func TestSIGTERM_c_ACompensatingRunIsNotToldItsCallFailed(t *testing.T) {
	db := ownerDB(t)
	defer db.Close()
	suffix := uniqueSuffix()
	taskQueue, wfID := "queue-term-c-"+suffix, "term-c-wf-"+suffix

	deployFixture(t, db, taskQueue)
	bin := buildWorker(t)
	svc := newChargeService(t)
	svc.failFirstN("Ship", 1) // the first Ship attempt fails retryably: the run is now waiting out a 10s backoff

	first := startWorker(t, bin, taskQueue, svc.srv.URL, "--shutdown-grace", "1s")
	startWorkflowEntry(t, db, wfID, "order-"+suffix, taskQueue, "compensating")
	svc.awaitCount(t, first, "Ship", 1, startBudget)
	time.Sleep(300 * time.Millisecond) // into the backoff select

	exited := first.term()
	awaitExit(t, exited, 30*time.Second, first)

	if n := svc.count("Refund"); n != 0 {
		t.Fatalf("Refund=%d: the shutdown reached the guest as a failed Ship and it compensated for a fault that never happened\n--- worker log ---\n%s", n, first.output())
	}
	requireReleased(t, db, wfID, first)

	second := startWorker(t, bin, taskQueue, svc.srv.URL)
	requireDone(t, db, wfID, completeBudget, second)
	if n := svc.count("Refund"); n != 0 {
		t.Errorf("Refund=%d after another worker finished the run, want 0: Ship succeeded on it", n)
	}
	if r, c := svc.count("Reserve"), svc.count("Charge"); r != 1 || c != 1 {
		t.Errorf("Reserve=%d Charge=%d, want 1/1", r, c)
	}
}

func TestSIGTERM_g_ARunWithADeferDoesNotRunItsCleanupOnTheWorkerThatIsLeaving(t *testing.T) {
	db := ownerDB(t)
	defer db.Close()
	suffix := uniqueSuffix()
	taskQueue, wfID := "queue-term-g-"+suffix, "term-g-wf-"+suffix

	deployFixture(t, db, taskQueue)
	bin := buildWorker(t)
	svc := newChargeService(t)
	release := svc.holdOperation("Ship")
	defer release()

	first := startWorker(t, bin, taskQueue, svc.srv.URL, "--shutdown-grace", "2s")
	startWorkflowEntry(t, db, wfID, "order-"+suffix, taskQueue, "with_cleanup")
	svc.awaitHeldCall(t, first, startBudget)

	exited := first.term()
	time.Sleep(4 * time.Second) // grace over, call still in flight
	release()                   // the call returns; the body ends; the defer is next
	awaitExit(t, exited, 30*time.Second, first)

	if n := svc.count("Cleanup"); n != 0 {
		t.Fatalf("Cleanup=%d on a run that was handed to another worker\n--- worker log ---\n%s", n, first.output())
	}
	requireReleased(t, db, wfID, first)

	second := startWorker(t, bin, taskQueue, svc.srv.URL)
	requireDone(t, db, wfID, completeBudget, second)
	if n := svc.count("Cleanup"); n != 1 {
		t.Errorf("Cleanup=%d, want exactly 1: run once, by the worker that finished the run", n)
	}
}

func TestSIGTERM_h_AGenuineFailureDuringTheDrainIsStillRecorded(t *testing.T) {
	db := ownerDB(t)
	defer db.Close()
	suffix := uniqueSuffix()
	taskQueue, wfID := "queue-term-h-"+suffix, "term-h-wf-"+suffix

	deployFixture(t, db, taskQueue)
	bin := buildWorker(t)
	svc := newChargeService(t)
	release := svc.holdOperation("Ship")
	svc.answerHeldWith(400) // not retryable: Ship really fails
	defer release()

	w := startWorker(t, bin, taskQueue, svc.srv.URL, "--shutdown-grace", "30s")
	startWorkflow(t, db, wfID, "order-"+suffix, taskQueue)
	svc.awaitHeldCall(t, w, startBudget)

	exited := w.term()
	time.Sleep(time.Second)
	release()
	awaitExit(t, exited, 30*time.Second, w)

	// The worker was alive for the whole of this (the grace had not ended), so this is an ordinary failure on
	// an ordinary worker, and releasing it would loop it through every worker forever.
	status, msg := runRow(t, db, wfID)
	if status != "failed" {
		t.Errorf("run is %q (%s), want failed: a real failure during a drain must still be recorded\n--- worker log ---\n%s", status, msg, w.output())
	}
	if s := svc.count("Ship"); s != 1 {
		t.Errorf("Ship=%d, want 1: a non-retryable failure is not repeated", s)
	}
}

// The draining worker must keep heartbeating, or another worker reclaims its run after the reclaim window
// (max(2x heartbeat, 10s)) and re-executes the step while the first is still running it: the duplicate a
// graceful shutdown exists to avoid. The service holds the call for longer than that window.
func TestSIGTERMKeepsHeartbeatingSoTheRunIsNotReclaimedWhileItDrains(t *testing.T) {
	db := ownerDB(t)
	defer db.Close()
	suffix := uniqueSuffix()
	taskQueue, wfID := "queue-term-hb-"+suffix, "term-hb-wf-"+suffix

	deployFixture(t, db, taskQueue)
	bin := buildWorker(t)
	svc := newChargeService(t)
	release := svc.holdOperation("Ship")
	defer release()

	first := startWorker(t, bin, taskQueue, svc.srv.URL, "--shutdown-grace", "60s")
	startWorkflow(t, db, wfID, "order-"+suffix, taskQueue)
	svc.awaitHeldCall(t, first, startBudget)

	// A second worker on the same queue, ready to reclaim the moment the first looks dead.
	second := startWorker(t, bin, taskQueue, svc.srv.URL)

	exited := first.term()
	time.Sleep(25 * time.Second) // beyond the reclaim window with margin (measured 9.5-24.5s in review)
	release()
	awaitExit(t, exited, 30*time.Second, first)

	status, errMsg := awaitTerminal(t, db, wfID, completeBudget)
	if status != "done" && status != "completed" {
		t.Fatalf("run ended %q (%s)\n--- first ---\n%s\n--- second ---\n%s", status, errMsg, first.output(), second.output())
	}
	if r, c, s := svc.allCounts(); r != 1 || c != 1 || s != 1 {
		t.Errorf("Reserve=%d Charge=%d Ship=%d, want 1/1/1: the second worker reclaimed a run the draining worker was still executing", r, c, s)
	}
}

// lockRow holds a row lock on the run, from the owner connection, until unlock is called. Whatever the worker
// writes to that row (finalize, continue-as-new, and the release that follows a cancelled one) waits behind it.
func lockRow(t *testing.T, db *sql.DB, id string) (unlock func()) {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	var one int
	if err := tx.QueryRow(`SELECT 1 FROM workflow_instances WHERE id = $1 FOR UPDATE`, id).Scan(&one); err != nil {
		_ = tx.Rollback()
		t.Fatalf("locking %s: %v", id, err)
	}
	var once bool
	return func() {
		if !once {
			once = true
			_ = tx.Rollback()
		}
	}
}

// (d) and (e): the terminal write is IN FLIGHT when the grace ends. The row is locked, so the worker's
// finalize (d) or continue-as-new (e) is genuinely waiting when the worker is cancelled; it comes back
// `context canceled`, which used to be written FAILED. The release that follows is blocked by the same lock
// until the test lets go, and must then land: the run is released, never failed, and another worker finishes
// it.
func TestSIGTERM_d_AFinalizeInFlightWhenTheGraceEndsIsReleasedNotFailed(t *testing.T) {
	db := ownerDB(t)
	defer db.Close()
	suffix := uniqueSuffix()
	taskQueue, wfID := "queue-term-d-"+suffix, "term-d-wf-"+suffix

	deployFixture(t, db, taskQueue)
	bin := buildWorker(t)
	svc := newChargeService(t)
	release := svc.holdOperation("Ship")
	defer release()

	first := startWorker(t, bin, taskQueue, svc.srv.URL, "--shutdown-grace", "3s")
	startWorkflow(t, db, wfID, "order-"+suffix, taskQueue)
	svc.awaitHeldCall(t, first, startBudget)

	exited := first.term()
	time.Sleep(300 * time.Millisecond)
	unlock := lockRow(t, db, wfID)
	defer unlock()
	release() // the last call returns; the finalize starts and waits behind the lock
	time.Sleep(5 * time.Second)
	// Non-vacuity: with the lock held past the grace the finalize could not have landed, so the run is still
	// the worker's. Without the lock it would be done by now and this test would be measuring nothing.
	if st, _ := runRow(t, db, wfID); st != "running" {
		t.Fatalf("with the row locked the run is %q, want running: the finalize was not held in flight, so this scenario proves nothing", st)
	}
	unlock()
	awaitExit(t, exited, 30*time.Second, first)

	requireReleased(t, db, wfID, first)
	second := startWorker(t, bin, taskQueue, svc.srv.URL)
	requireDone(t, db, wfID, completeBudget, second)
	if r, c := svc.count("Reserve"), svc.count("Charge"); r != 1 || c != 1 {
		t.Errorf("Reserve=%d Charge=%d, want 1/1", r, c)
	}
}

func TestSIGTERM_e_AContinueAsNewInFlightWhenTheGraceEndsMakesExactlyOneNewRun(t *testing.T) {
	db := ownerDB(t)
	defer db.Close()
	suffix := uniqueSuffix()
	taskQueue, wfID := "queue-term-e-"+suffix, "term-e-wf-"+suffix

	deployFixture(t, db, taskQueue)
	bin := buildWorker(t)
	svc := newChargeService(t)
	release := svc.holdOperation("Reserve")
	defer release()
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM event_history WHERE workflow_id IN (SELECT id FROM workflow_instances WHERE task_queue = $1)`, taskQueue)
		_, _ = db.Exec(`DELETE FROM workflow_instances WHERE task_queue = $1`, taskQueue)
	})

	first := startWorker(t, bin, taskQueue, svc.srv.URL, "--shutdown-grace", "3s")
	startWorkflowEntry(t, db, wfID, "order-"+suffix, taskQueue, "continues_as_new")
	svc.awaitHeldCall(t, first, startBudget)

	exited := first.term()
	time.Sleep(300 * time.Millisecond)
	unlock := lockRow(t, db, wfID)
	defer unlock()
	release() // the guest continues as new; the store write waits behind the lock
	time.Sleep(5 * time.Second)
	// Non-vacuity: held behind the lock the continue-as-new cannot have committed, so the queue still holds only
	// the original run. Without the lock there would already be two and this scenario would prove nothing.
	var held int
	if err := db.QueryRow(`SELECT count(*) FROM workflow_instances WHERE task_queue = $1`, taskQueue).Scan(&held); err != nil {
		t.Fatal(err)
	}
	if held != 1 {
		t.Fatalf("%d runs on the queue while the row was locked, want 1: the continue-as-new was not held in flight", held)
	}
	unlock()
	awaitExit(t, exited, 30*time.Second, first)

	if status, msg := runRow(t, db, wfID); status == "failed" {
		t.Fatalf("a continue-as-new cut off by SIGTERM failed the run: %s\n--- worker log ---\n%s", msg, first.output())
	}

	second := startWorker(t, bin, taskQueue, svc.srv.URL)
	// The chain settles when nothing on the queue is ready, running or terminating any more.
	deadline := time.Now().Add(completeBudget)
	for {
		var live, total, failed int
		if err := db.QueryRow(`SELECT count(*) FILTER (WHERE status IN ('ready','running','terminating')),
			count(*), count(*) FILTER (WHERE status = 'failed') FROM workflow_instances WHERE task_queue = $1`, taskQueue).
			Scan(&live, &total, &failed); err != nil {
			t.Fatal(err)
		}
		if failed != 0 {
			t.Fatalf("%d run(s) failed\n--- first ---\n%s\n--- second ---\n%s", failed, first.output(), second.output())
		}
		if live == 0 && total > 0 {
			if total != 2 {
				t.Errorf("%d runs on the queue, want exactly 2 (the original and ONE continuation)", total)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the chain did not settle: live=%d total=%d\n--- second ---\n%s", live, total, second.output())
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// (j) The admin API's drain is a cordon, and SIGTERM after it still drains. POST /api/admin/drain stops the
// worker claiming and must not end the process (in Kubernetes an exit is a restart that resumes claiming); a
// run in flight when it is called finishes; a second run queued after it is not claimed; and the SIGTERM that
// follows waits for the first run instead of cancelling it.
func TestSIGTERM_j_ADrainThenSIGTERMStillLetsTheRunFinish(t *testing.T) {
	db := ownerDB(t)
	defer db.Close()
	suffix := uniqueSuffix()
	taskQueue, wfID, wf2 := "queue-term-j-"+suffix, "term-j-wf-"+suffix, "term-j-wf2-"+suffix

	deployFixture(t, db, taskQueue)
	bin := buildWorker(t)
	svc := newChargeService(t)
	release := svc.holdOperation("Ship")
	defer release()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	w := startWorker(t, bin, taskQueue, svc.srv.URL, "--shutdown-grace", "30s",
		"--api-addr", addr, "--require-auth=false", "--enable-admin-api",
		"--concurrency", "2") // room for a second run, so that only the cordon keeps it unclaimed
	startWorkflow(t, db, wfID, "order-"+suffix, taskQueue)
	svc.awaitHeldCall(t, w, startBudget)

	post, err := http.Post("http://"+addr+"/api/admin/drain", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("POST drain: %v\n--- worker log ---\n%s", err, w.output())
	}
	_ = post.Body.Close()
	if post.StatusCode != http.StatusAccepted {
		t.Fatalf("POST drain answered %d, want 202\n--- worker log ---\n%s", post.StatusCode, w.output())
	}

	// A run queued AFTER the cordon must not be claimed.
	startWorkflow(t, db, wf2, "order2-"+suffix, taskQueue)
	time.Sleep(2 * time.Second)
	if st, _ := runRow(t, db, wf2); st != "ready" {
		t.Errorf("a run queued after the cordon is %q, want ready: a cordoned worker must not claim", st)
	}

	exited := w.term()
	select {
	case err := <-exited:
		t.Fatalf("worker exited (%v) with a run in flight after drain then SIGTERM\n--- worker log ---\n%s", err, w.output())
	case <-time.After(2 * time.Second):
	}
	release()
	awaitExit(t, exited, 30*time.Second, w)

	requireDone(t, db, wfID, 10*time.Second, w)
	if r, c, s := svc.allCounts(); r != 1 || c != 1 || s != 1 {
		t.Errorf("Reserve=%d Charge=%d Ship=%d, want 1/1/1", r, c, s)
	}
}

// (m) A defer phase cut off by shutdown. A run parked on a sleep owes a cleanup; terminating it marks it
// `terminating` and the next worker to claim it runs the cleanup. Here the cleanup's call fails with 503 and
// waits out a 10s backoff, and SIGTERM (grace 1s) lands in the middle of that wait. The engine tells the guest
// to stop, so the segment comes back with the cleanup NOT done. That must be RELEASED. The defer-phase finish
// writes on context.Background() and would apply the recorded outcome at once, so without the shutdown check
// right after Replay the run ends `terminated` with its cleanup never having succeeded (Cleanup=1). Released,
// the next worker runs the cleanup again and it succeeds (Cleanup=2).
func TestSIGTERM_m_ADeferPhaseCutOffByShutdownIsReleasedNotTerminatedWithoutItsCleanup(t *testing.T) {
	db := ownerDB(t)
	defer db.Close()
	suffix := uniqueSuffix()
	taskQueue, wfID := "queue-term-m-"+suffix, "term-m-wf-"+suffix

	deployFixture(t, db, taskQueue)
	bin := buildWorker(t)
	svc := newChargeService(t)
	svc.failFirstN("Cleanup", 1) // the first Cleanup answers 503; the next succeeds

	first := startWorker(t, bin, taskQueue, svc.srv.URL, "--shutdown-grace", "1s")
	startWorkflowEntry(t, db, wfID, "order-"+suffix, taskQueue, "cleanup_with_backoff")

	// The run registers its defer, calls Reserve and parks on the sleep: ready, unowned, a defer in history.
	svc.awaitCount(t, first, "Reserve", 1, startBudget)
	deadline := time.Now().Add(startBudget)
	for {
		var n int
		var assigned sql.NullString
		if err := db.QueryRow(`SELECT (SELECT count(*) FROM event_history WHERE workflow_id = $1 AND event_type = 'defer'), assigned_to
			FROM workflow_instances WHERE id = $1`, wfID).Scan(&n, &assigned); err != nil {
			t.Fatal(err)
		}
		if n > 0 && !assigned.Valid {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the run never parked with a defer recorded\n--- worker log ---\n%s", first.output())
		}
		time.Sleep(200 * time.Millisecond)
	}

	// Terminate it: phase 1 of the two-phase transition, as engine.preemptivelySettle writes it for a run that
	// owes a defer phase. (The HTTP terminate route is the dead-letter queue's and does not reach this.)
	if _, err := db.Exec(`UPDATE workflow_instances
		SET status = 'terminating', pending_terminal_status = 'terminated',
		    defer_phase_deadline = now() + interval '300 seconds', error_msg = 'test terminate',
		    next_wake_at = now(), assigned_to = NULL, generation = generation + 1
		WHERE id = $1`, wfID); err != nil {
		t.Fatal(err)
	}

	// The worker claims the defer phase; the cleanup's first call fails and it is now in the 10s backoff.
	svc.awaitCount(t, first, "Cleanup", 1, startBudget)
	time.Sleep(300 * time.Millisecond)

	exited := first.term()
	awaitExit(t, exited, 30*time.Second, first)

	status, msg := runRow(t, db, wfID)
	if status == "terminated" {
		t.Fatalf("the run was terminated with its cleanup never done (Cleanup=%d): a defer phase cut off by shutdown was finalized\n--- worker log ---\n%s",
			svc.count("Cleanup"), first.output())
	}
	if status != "terminating" {
		t.Fatalf("run is %q (%s), want it released still terminating\n--- worker log ---\n%s", status, msg, first.output())
	}

	second := startWorker(t, bin, taskQueue, svc.srv.URL)
	// awaitTerminal would return at once: `terminating` is not ready or running. Wait for the outcome itself.
	settleBy := time.Now().Add(completeBudget)
	for {
		st, m := runRow(t, db, wfID)
		if st == "terminated" {
			break
		}
		if time.Now().After(settleBy) {
			t.Fatalf("after another worker took it the run is %q (%s), want terminated\n--- worker log ---\n%s", st, m, second.output())
		}
		time.Sleep(500 * time.Millisecond)
	}
	if n := svc.count("Cleanup"); n != 2 {
		t.Errorf("Cleanup=%d, want 2: one attempt cut off by the shutdown, then the one that succeeded on the next worker", n)
	}
}
