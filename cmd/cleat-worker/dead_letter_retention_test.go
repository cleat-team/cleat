package main

import (
	"context"
	"testing"
	"time"
)

// cleat#1023: DeleteDeadLetteredWorkflows existed on all three stores,
// cascaded correctly to child rows, was named in DeleteCompletedWorkflows'
// doc as dead-lettered work's "own deletion path" -- and had no flag and no
// caller. The design said these rows have their own lifecycle and then shipped
// no way to express one.
//
// Retaining them longer than completed work IS deliberate, stated twice: in
// --completed-workflow-retention-days' help text and in the interface doc. So
// the fix is a knob, not a default.

func TestDeadLetterRetentionSweepsWhenEnabled(t *testing.T) {
	var cutoff time.Time
	called := false
	ms := &mockStore{}
	ms.deleteDeadLetteredWorkflowsFn = func(ctx context.Context, olderThan time.Time) (int64, error) {
		called, cutoff = true, olderThan
		return 3, nil
	}
	w := newTestWorker(ms)

	w.runRetentionSweep(0, 0, 30)

	if !called {
		t.Fatal("DeleteDeadLetteredWorkflows was not called with " +
			"--dead-letter-retention-days set.\n\n" +
			"Before cleat#1023 this method had no caller anywhere in production: " +
			"it was implemented, documented as dead-lettered work's own deletion " +
			"path, and unreachable.")
	}
	if age := time.Since(cutoff); age < 29*24*time.Hour || age > 31*24*time.Hour {
		t.Errorf("cutoff is %v old, want ~30 days -- the flag is days, and a "+
			"wrong unit here deletes far more than an operator asked for", age)
	}
}

// TestDeadLetterRetentionIsOffByDefault is the half that protects the
// documented decision.
//
// --completed-workflow-retention-days says "dead_lettered workflows are never
// touched by this flag", and a dead-lettered run is the one an operator most
// wants to inspect. Defaulting the new knob on would silently reverse that on
// every existing deployment at upgrade -- the same trap avoided in #1010's
// startup sweep, and the reason both flags are opt-in.
func TestDeadLetterRetentionIsOffByDefault(t *testing.T) {
	ms := &mockStore{}
	ms.deleteDeadLetteredWorkflowsFn = func(ctx context.Context, olderThan time.Time) (int64, error) {
		t.Error("DeleteDeadLetteredWorkflows ran with --dead-letter-retention-days " +
			"at its default of 0. Dead-lettered workflows are retained on purpose; " +
			"deleting them destroys exactly the record they were kept for.")
		return 0, nil
	}
	w := newTestWorker(ms)

	// The other two knobs on, this one off: the sweep must run and still not
	// touch dead-letters. Asserting it in isolation would pass for a sweep
	// that did nothing at all.
	ms.deleteExpiredEventsFn = func(ctx context.Context, olderThan time.Time) (int64, error) { return 0, nil }
	ms.deleteCompletedWorkflowsFn = func(ctx context.Context, olderThan time.Time) (int64, error) { return 0, nil }

	w.runRetentionSweep(30, 90, 0)
}

// TestDeadLetterRetentionAloneStartsTheLoop pins the loop's guard. Enabling
// only this knob must be enough to start the loop: the early return lists the
// knobs, and a third one added to the sweep but not to the guard is a flag
// that silently does nothing.
func TestDeadLetterRetentionAloneStartsTheLoop(t *testing.T) {
	swept := make(chan struct{}, 2)
	ms := &mockStore{}
	ms.deleteDeadLetteredWorkflowsFn = func(ctx context.Context, olderThan time.Time) (int64, error) {
		swept <- struct{}{}
		return 0, nil
	}
	w := newTestWorker(ms)
	w.retentionInterval = time.Hour // so only the startup sweep can fire

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.ctx = ctx

	w.wg.Add(1)
	go w.retentionLoop(0, 0, 30)

	select {
	case <-swept:
	case <-time.After(3 * time.Second):
		t.Fatal("retentionLoop returned without sweeping when only " +
			"--dead-letter-retention-days was set: the loop's early-return " +
			"guard does not know about the new knob, so the flag is accepted " +
			"and does nothing")
	}
	cancel()
}
