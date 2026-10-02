package main

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

// cleat#2942 -- the fence resolves what to cancel BY RUN ID, after the
// snapshot it judged against has been invalidated by a same-worker re-claim.
//
// heartbeatAndFenceInFlight snapshots w.inflight into `runs` and only THEN
// calls HeartbeatBatchFenced, which reports a run lost when the generation
// SENT differs from the row's CURRENT one (engine/db.go:490). A suspend makes
// a worker re-claim its OWN run milliseconds after releasing it, and claiming
// Stores by wf.ID, so the successor overwrites both w.inflight and w.execCancel
// in place. The fence loop then reads the generation live and loads execCancel
// by id -- so it cancels the execution that claimed moments ago, not the one
// it judged. That execution then dies in its pre-replay reads, and
// engine/executor.go:249 turns the resulting context.Canceled into a fatal
// "checksum verification failed" -- the run is FAILED rather than handed over.
//
// The store double below performs that re-claim INSIDE the call, which is the
// one place the race is defined. probeBoundedCall invokes fn synchronously, so
// the reproduction is deterministic rather than a timing hope.
//
// THIS TEST ASSERTS THE FIXED BEHAVIOUR and is deliberately RED on the tree as
// of 550bb41e. Its second half is the control that stops it being satisfied by
// a fence which simply never cancels anything.

// reclaimRacingStore returns a store double whose HeartbeatBatchFenced performs
// a same-worker re-claim mid-call: it records the (id, generation) pairs it was
// sent, then replaces the live entries for `id` with generation newGen and its
// own cancel func, exactly as Store-by-wf.ID on claim does. It reports `id`
// lost, which is what the real store does when the sent generation no longer
// matches the row's.
func reclaimRacingStore(w **Worker, id string, newGen int64, successorCtx *context.Context) *mockStore {
	return &mockStore{
		heartbeatBatchFencedFn: func(ctx context.Context, workerID string, runs []engine.GenerationKey) ([]string, error) {
			var cancel context.CancelFunc
			*successorCtx, cancel = context.WithCancel(context.Background())
			(*w).inflight.Store(id, &engine.WorkflowInstance{ID: id, DefName: "test-def", Generation: newGen})
			(*w).execCancel.Store(id, cancel)
			return []string{id}, nil
		},
	}
}

func TestTheFenceDoesNotCancelTheSuccessorWhenAReclaimRacesTheHeartbeat(t *testing.T) {
	var buf bytes.Buffer
	var w *Worker
	var successorCtx context.Context

	ms := reclaimRacingStore(&w, "run-1", 2, &successorCtx)
	w = newTestWorker(ms)
	w.logger = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	judgedCtx := registerInflightExecution(w, "run-1", "test-def", 1)

	w.heartbeatAndFenceInFlight()

	t.Logf("fence log: %s", strings.TrimSpace(buf.String()))

	if successorCtx == nil {
		t.Fatal("the store double never ran -- no successor was registered")
	}
	// THE DEFECT. The fence judged generation 1 and cancelled generation 2.
	if successorCtx.Err() != nil {
		t.Error("the SUCCESSOR's context was cancelled -- the fence landed on a run that had just claimed and was not the one it judged")
	}
	// The hand-over this fence exists to perform had already happened locally:
	// the judged generation released the run before the re-claim could occur.
	if judgedCtx.Err() != nil {
		t.Error("the judged execution's context was cancelled -- nothing of that generation was still registered to stop")
	}
	// fencedRuns being keyed by id alone means the successor inherits the
	// predecessor's fence marker, which is what makes the NEW execution's
	// durable calls refuse -- the same defect one level down.
	if w.runIsFenced("run-1") {
		t.Error("the run was marked fenced -- the marker landed on the successor's registration")
	}
	// WS-1's diagnostic: the WARN must carry the generation SENT beside the one
	// read live, so a hit reads sent=1 beside generation=2 rather than the
	// self-contradicting "superseded by a later generation ... generation=2".
	if got := buf.String(); !strings.Contains(got, "sent_generation=1") || !strings.Contains(got, "generation=2") {
		t.Errorf("the fence WARN does not carry the sent generation beside the live one; got:\n%s", got)
	}
}

// The control. Without it the test above is satisfied by a fence that never
// cancels anything -- which is the "condition that never decides anything"
// failure, and is exactly the shape the defect's fix invites.
func TestTheFenceStillCancelsTheRunItJudgedWhenNothingRacesIt(t *testing.T) {
	ms := &mockStore{
		heartbeatBatchFencedFn: func(ctx context.Context, workerID string, runs []engine.GenerationKey) ([]string, error) {
			if len(runs) != 1 || runs[0].Generation != 4 {
				t.Errorf("snapshot handed to the store = %+v, want one run at generation 4", runs)
			}
			return []string{"run-1"}, nil
		},
	}
	w := newTestWorker(ms)
	judgedCtx := registerInflightExecution(w, "run-1", "test-def", 4)

	w.heartbeatAndFenceInFlight()

	if judgedCtx.Err() == nil {
		t.Error("the judged execution was NOT cancelled -- a fence that never cancels protects nothing")
	}
	if !w.runIsFenced("run-1") {
		t.Error("runIsFenced(\"run-1\") = false -- the judgement did not reach fencedRuns")
	}
}
