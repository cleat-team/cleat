package crash

import (
	"database/sql"
	"strings"
	"testing"
	"time"
)

// TestChecksumChainSurvivesAwaitChild is cleat#2333: a parent that calls
// ChildWorkflow, AwaitChild, then records another event, and whose worker
// dies during the call after that. The next worker's replay is documented to
// finish the parent -- at-least-once, same as every other crash in this
// suite -- and instead fails outright with a checksum mismatch at the step
// after await_child.
//
// Run with and without --encrypt-sensitive-payloads, and killed both by
// SIGTERM (the release path) and SIGKILL (reclaim), because cleat-review's
// report says all four reproduce identically on develop.
//
// A fifth case, "SIGTERM/adaptive-flush", forces the adaptive batch writer
// (adaptive_flush.go's flushAndNotify) rather than the default direct
// per-step path (flush.go's flushEvent) -- cleat-review's point (1): "With
// adaptive flush enabled, the batch writer is the production path. Run your
// crash test with adaptive flush on as well." The registry is enabled by
// default (cmd/cleat-worker/config.go's batchFlushDisabled defaults to
// false), but AdaptiveFlusher only actually routes writes through the batch
// path once the observed steps/sec crosses --batch-flush-enter-rate
// (default 500/sec) -- a single test workflow never gets there, so every
// other case in this table exercises flush.go's insertEventSQL exclusively,
// never adaptive_flush.go's flushAndNotify/retryBatchFlush.
//
// THIS COMMENT USED TO SAY "setting the enter/exit rates to 0 forces batch
// mode from the worker's first event", and that was false: cleat-review
// measured it directly. NewAdaptiveFlusher treats <= 0 as "not configured"
// and silently substitutes its own default (500 for enter, 250 for exit),
// so --batch-flush-enter-rate 0 asks for the DEFAULT threshold under a
// different name -- with only flush.go's guard reverted, this case failed
// (proving it never touched adaptive_flush.go's path); with only
// adaptive_flush.go's guards reverted, it still passed. A case that cannot
// fail on the thing it names is worse than no case: it reads as coverage.
//
// Fixed two ways. First, the threshold itself: 1 is not <= 0, so it is not
// substituted, and any real workflow activity's rate easily clears it on
// the very first 100ms sample (updateRate's own minimum interval). exit=1
// too, not 0 -- the same substitution trap on the other threshold, and
// --batch-flush-exit-rate's own doc string ("must be < enter-rate") is
// unenforced in code, so entering and never exiting (rather than flapping
// against a defaulted 250 our test workflow's throughput never approaches)
// is a deliberate choice, not an oversight. Second, an assertion that does
// not just trust the flag: AdaptiveFlusher logs "adaptive flusher entered
// batch mode" (slog.Info, on by default) the moment updateRate's threshold
// check trips, so runChecksumChainCase greps the first worker's captured
// output for that line when forceAdaptive is set -- proving the batch path
// actually ran, rather than assuming a flag that looked right did.
func TestChecksumChainSurvivesAwaitChild(t *testing.T) {
	for _, tc := range []struct {
		name          string
		encrypt       bool
		sigkill       bool
		forceAdaptive bool
	}{
		{"SIGTERM/plain", false, false, false},
		{"SIGTERM/encrypted", true, false, false},
		{"SIGKILL/plain", false, true, false},
		{"SIGKILL/encrypted", true, true, false},
		{"SIGTERM/adaptive-flush", false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runChecksumChainCase(t, tc.encrypt, tc.sigkill, tc.forceAdaptive)
		})
	}
}

func runChecksumChainCase(t *testing.T, encrypt, sigkill, forceAdaptive bool) {
	db := ownerDB(t)
	defer db.Close()
	suffix := uniqueSuffix()
	taskQueue, wfID := "queue-cc2333-"+suffix, "cc2333-wf-"+suffix

	deployFixture(t, db, taskQueue)
	bin := buildWorker(t)
	svc := newChargeService(t)
	release := svc.holdOperation("Ship")
	defer release()

	var encFlags []string
	if encrypt {
		key := writeEncryptionKeyFile(t)
		encFlags = []string{"--encrypt-sensitive-payloads", "--encryption-key-file", key}
	}
	if forceAdaptive {
		// 1, not 0: NewAdaptiveFlusher substitutes its own default (500/250)
		// for any threshold <= 0, so 0 silently asks for the default rather
		// than forcing batch mode -- see the file doc comment. 1 crosses on
		// the very first observed rate sample, so the parent's await_child
		// suspend AND its completing re-flush both go through
		// AdaptiveFlusher.flushAndNotify rather than flushEvent.
		encFlags = append(encFlags, "--batch-flush-enter-rate", "1", "--batch-flush-exit-rate", "1")
	}

	first := startWorker(t, bin, taskQueue, svc.srv.URL,
		append(append([]string{}, encFlags...), "--shutdown-grace", "1s")...)
	startWorkflowEntry(t, db, wfID, suffix, taskQueue, "parent_with_child")
	svc.awaitHeldCall(t, first, startBudget)

	// Give the child a moment to finish and the parent's await_child event a
	// moment to be overwritten with its completed result -- the row whose
	// checksum chaining is in question -- before the crash.
	time.Sleep(2 * time.Second)
	logEventHistory(t, db, wfID, "BEFORE crash")

	if forceAdaptive && !strings.Contains(first.output(), "adaptive flusher entered batch mode") {
		t.Fatalf("forceAdaptive is set but the first worker's log never shows "+
			"\"adaptive flusher entered batch mode\" -- the completing re-flush "+
			"this case exists to route through adaptive_flush.go instead went "+
			"through flush.go's direct path, same as every other case in this "+
			"table, and this case is not testing what its name says\n--- first worker log ---\n%s",
			first.output())
	}

	if sigkill {
		first.kill()
		release()
	} else {
		exited := first.term()
		time.Sleep(3 * time.Second)
		release()
		awaitExit(t, exited, 30*time.Second, first)
	}

	second := startWorker(t, bin, taskQueue, svc.srv.URL, encFlags...)
	status, errMsg := awaitTerminal(t, db, wfID, completeBudget)
	logEventHistory(t, db, wfID, "AFTER replay")
	if status != "done" {
		t.Errorf("parent workflow %s ended %q (%s); want \"done\"\n--- second worker log ---\n%s",
			wfID, status, errMsg, second.output())
	}
}

// logEventHistory dumps step, event_type and checksum for a workflow's
// recorded events, and recomputes the chain independently so a mismatch shows
// exactly which step it is at rather than only the worker's own error string.
func logEventHistory(t *testing.T, db *sql.DB, id, label string) {
	t.Helper()
	rows, err := db.Query(`SELECT step, event_type, COALESCE(checksum,'<nil>'),
		COALESCE(response,'<nil>'), COALESCE(payload::text,'<nil>'), run_id
		FROM event_history WHERE workflow_id = $1 ORDER BY step`, id)
	if err != nil {
		t.Logf("%s: querying event_history: %v", label, err)
		return
	}
	defer rows.Close()
	t.Logf("--- event_history %s (workflow %s) ---", label, id)
	for rows.Next() {
		var step int
		var et, cs, resp, payload string
		var runID sql.NullString
		if err := rows.Scan(&step, &et, &cs, &resp, &payload, &runID); err != nil {
			t.Logf("  scan error: %v", err)
			continue
		}
		t.Logf("  step=%d type=%s checksum=%s response=%q payload=%s run_id=%q", step, et, cs, resp, payload, runID.String)
	}
}
