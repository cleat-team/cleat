package crash

import (
	"strings"
	"syscall"
	"testing"
	"time"
)

// cleat#2298: SIGHUP reloads the secret and payload key rings, with no restart. Go's default
// action for SIGHUP is TERMINATE, so without the handler an operator sending `kill -HUP` to roll
// a worker onto a new key would kill the worker instead. This test pins that a real worker
// process survives SIGHUP and logs that a reload ran, for a worker started with NEITHER ring
// configured -- the common case (an operator rotating one fleet-wide secret while most workers'
// local files are untouched) and the shape that proves the handler is wired to something rather
// than relying on log inspection of a mechanism that might not run at all (the "wired to nothing"
// trap CLAUDE.md warns about for exactly this shape of feature). The reload's actual mechanics
// (M4's candidate check, the republished secret_key_versions, the payload ring's safe-refusal) are
// covered at the unit level in cmd/cleat-worker/a_sighup_reload_test.go, which can assert on the
// database and the ring's own state directly; a subprocess test can only watch the log and that
// the worker kept serving.
func TestSIGHUPReloadsWithNoKeysConfiguredAndKeepsServing(t *testing.T) {
	db := ownerDB(t)
	defer db.Close()
	suffix := uniqueSuffix()
	taskQueue := "queue-sighup-" + suffix

	deployFixture(t, db, taskQueue)
	bin := buildWorker(t)
	svc := newChargeService(t)
	defer svc.srv.Close()

	w := startWorker(t, bin, taskQueue, svc.srv.URL)

	// Wait until the worker has actually started claiming (a real readiness signal, not a fixed
	// sleep) before signalling it -- signal.Notify runs late in main(), after migration and setup,
	// and a SIGHUP that arrives before it is installed gets Go's DEFAULT disposition for SIGHUP,
	// which is TERMINATE. That would kill the worker before this test's handler is even wired,
	// which is a different failure than the one this test exists to catch.
	startWorkflow(t, db, "sighup-readiness-"+suffix, "order-"+suffix, taskQueue)
	svc.awaitCount(t, w, "Reserve", 1, startBudget)

	// Waits for the RELOAD to complete, not just the signal to arrive -- the
	// handler logs "received SIGHUP" first and "key reload complete" once
	// reloadKeyRingsOnSIGHUP actually returns, and only the second proves
	// the mechanism ran rather than merely received the signal.
	awaitSighupCount := func(n int) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for strings.Count(w.output(), "SIGHUP: key reload complete") < n {
			if time.Now().After(deadline) {
				t.Fatalf("worker had not completed %d SIGHUP reload(s) within 10s\n--- worker log ---\n%s", n, w.output())
			}
			time.Sleep(20 * time.Millisecond)
		}
	}

	w.hup()
	awaitSighupCount(1)

	// A SECOND signal, sent only after the first was drained and logged -- proves the handler loops
	// (`for range sighupCh`) rather than firing once and falling out. Two signals sent back-to-back
	// with nothing in between would not prove this: the OS/runtime is free to coalesce a second
	// SIGHUP that arrives before Go's signal package has drained the first one off the channel, so
	// an incorrect one-shot handler could still pass a rapid-fire version of this check by accident.
	w.hup()
	awaitSighupCount(2)

	// The property under test: the process is still alive, checked BEFORE anything else signals
	// it. Signal(syscall.Signal(0)) delivers nothing; it only probes whether the pid still exists
	// (returns an error once the process has exited). Checking this only after term() -- which
	// itself sends SIGTERM -- would prove nothing about SIGHUP, since a quick exit there could be
	// the SIGTERM's doing, not evidence a SIGHUP got through.
	if err := w.cmd.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("worker is not alive after two SIGHUPs (SIGHUP's default action is terminate, so this means one of them ended it): %v\n--- worker log ---\n%s", err, w.output())
	}

	awaitExit(t, w.term(), 30*time.Second, w)
}
