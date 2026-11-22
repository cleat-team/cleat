//go:build chaos

package chaos

import "testing"

// cleat#4350. A fault-free (golden) run is executed by the PARENT test, so startWorker's t.Cleanup never fired until the whole sweep ended: every golden worker stayed alive
// holding a pool of about 11 connections, and eight scripts held 88 of a CI database's 100 slots for ~200 fault cases. execute now kills a golden run's worker when the run is over.
// This asserts it directly: after a golden run returns, the database holds no more connections than before it started.
//
// Falsified by deleting the `if f == nil { first.kill() }` block in execute: the count then stays above its baseline and this fails with the number it stuck at.
func TestChaosAGoldenRunsWorkerDoesNotOutliveItsRun(t *testing.T) {
	e := newEnv(t)
	before, _, err := connSnapshot(e.db)
	if err != nil {
		t.Fatalf("UNMEASURED: the baseline snapshot failed: %v", err)
	}
	g := execute(t, e, scenarios[0], nil)
	if g.Status != "done" && g.Status != "completed" {
		t.Fatalf("UNMEASURED: the golden run ended %q, so it is not a finished run whose worker should be gone", g.Status)
	}
	var after int
	if !waitFor(func() bool {
		after, _, _ = connSnapshot(e.db)
		return after <= before
	}) {
		t.Fatalf("the database still holds %d connections after a finished golden run, against %d before it: its worker outlived the run (cleat#4350)", after, before)
	}
}
