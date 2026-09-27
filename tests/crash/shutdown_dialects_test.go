package crash

// cleat#2288: scenarios (a) and (b) of cleat#2285, on every dialect this repo
// supports.
//
// Why these two and not the rest: #2285's shutdown path touches the fenced
// finalize, ContinueAsNew, fail and release on all three dialects, and the
// suite that covers it was PostgreSQL-only. (a) and (b) are the two the issue
// names -- one run that finishes inside the grace, and one call that outlives
// it -- and they are the pair that separates "the drain works" from "the drain
// releases rather than fails".
//
// The bodies live here, once, and every dialect calls them. The PostgreSQL
// tests in sigterm_test.go previously held (a)'s and (b)'s bodies inline; they
// now call these, which is what keeps the three dialects measuring the same
// thing instead of three things that drift.

import (
	"testing"
	"time"
)

// shutdownScenarioA: a run finishes inside the grace, and every call it made is
// made exactly once. Body of TestSIGTERM_a.
func shutdownScenarioA(t *testing.T, tg *crashTarget) {
	t.Helper()
	suffix := uniqueSuffix()
	taskQueue, wfID := "queue-term-a-"+suffix, "term-a-wf-"+suffix

	tg.deployFixture(t, taskQueue)
	bin := buildWorker(t)
	svc := newChargeService(t)
	release := svc.holdOperation("Ship")
	defer release()

	w := tg.startWorker(t, bin, taskQueue, svc.srv.URL, "--shutdown-grace", "30s")
	tg.startWorkflowEntry(t, wfID, "order-"+suffix, taskQueue, "three_charges")
	svc.awaitHeldCall(t, w, startBudget)

	exited := w.term()
	select {
	case err := <-exited:
		t.Fatalf("worker exited (%v) while a run was in flight and 30s of grace remained\n--- worker log ---\n%s", err, w.output())
	case <-time.After(2 * time.Second):
	}
	release()
	awaitExit(t, exited, 30*time.Second, w)

	requireDoneOn(t, tg, wfID, 10*time.Second, w)
	if r, c, s := svc.allCounts(); r != 1 || c != 1 || s != 1 {
		t.Errorf("Reserve=%d Charge=%d Ship=%d, want 1/1/1: a graceful shutdown must not repeat a side effect", r, c, s)
	}
}

// shutdownScenarioB: a call outlives the grace, so the run is released for
// another worker rather than failed -- and the interrupted call replays, which
// is the at-least-once contract. Body of TestSIGTERM_b.
func shutdownScenarioB(t *testing.T, tg *crashTarget) {
	t.Helper()
	suffix := uniqueSuffix()
	taskQueue, wfID := "queue-term-b-"+suffix, "term-b-wf-"+suffix

	tg.deployFixture(t, taskQueue)
	bin := buildWorker(t)
	svc := newChargeService(t)
	release := svc.holdOperation("Ship")
	defer release()

	first := tg.startWorker(t, bin, taskQueue, svc.srv.URL, "--shutdown-grace", "2s")
	tg.startWorkflowEntry(t, wfID, "order-"+suffix, taskQueue, "three_charges")
	svc.awaitHeldCall(t, first, startBudget)

	exited := first.term()
	// Let the 2s grace expire with the call still in flight, then let the call
	// return to a worker that has by now been cancelled. That is the moment the
	// old code wrote the run FAILED.
	time.Sleep(4 * time.Second)
	release()
	awaitExit(t, exited, 30*time.Second, first)

	requireReleasedOn(t, tg, wfID, first)

	second := tg.startWorker(t, bin, taskQueue, svc.srv.URL)
	requireDoneOn(t, tg, wfID, completeBudget, second)
	r, c, s := svc.allCounts()
	t.Logf("%s: Reserve=%d Charge=%d Ship=%d", tg.name, r, c, s)
	if r != 1 || c != 1 {
		t.Errorf("Reserve=%d Charge=%d, want 1/1: the calls that had finished must not be repeated", r, c)
	}
	if s < 1 || s > 2 {
		t.Errorf("Ship=%d, want 1 or 2: at-least-once, the interrupted call may replay once", s)
	}
}

// requireDoneOn is requireDone against a dialect target.
func requireDoneOn(t *testing.T, tg *crashTarget, id string, budget time.Duration, w *worker) {
	t.Helper()
	status, msg := tg.awaitTerminal(t, id, budget)
	if status != "done" && status != "completed" {
		t.Fatalf("run ended %q (%s), want done\n--- worker log ---\n%s", status, msg, w.output())
	}
}

// requireReleasedOn is requireReleased against a dialect target.
func requireReleasedOn(t *testing.T, tg *crashTarget, id string, w *worker) {
	t.Helper()
	status, msg := tg.runRow(t, id)
	if status != "ready" && status != "running" {
		t.Fatalf("a run cut off by SIGTERM is %q (%s), want it released for another worker\n--- worker log ---\n%s", status, msg, w.output())
	}
}

// TestShutdownScenariosOnMySQLAndMSSQL runs (a) and (b) on the two dialects
// the PostgreSQL-only suite never covered.
//
// A subtest per dialect rather than a loop over targets built up front: each
// target opens its own database handle and registers its own cleanup, and a
// target built unconditionally would connect to a server the environment may
// not have. The skip is per dialect and is a genuine environmental
// precondition -- the DSN is not set, so there is nothing to measure -- which
// is the only kind of skip this repo accepts; see the suite's own note on
// t.Skipf.
func TestShutdownScenariosOnMySQLAndMSSQL(t *testing.T) {
	for _, tc := range []struct {
		name   string
		target func(*testing.T) *crashTarget
	}{
		{"mysql", mysqlTarget},
		{"mssql", mssqlTarget},
	} {
		t.Run(tc.name+"/a", func(t *testing.T) {
			shutdownScenarioA(t, tc.target(t))
		})
		t.Run(tc.name+"/b", func(t *testing.T) {
			shutdownScenarioB(t, tc.target(t))
		})
	}
}
