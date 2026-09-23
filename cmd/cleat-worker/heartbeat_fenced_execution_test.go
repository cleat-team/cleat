package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
)

// cleat#2008: heartbeatAndFenceInFlight is what actually stops a fenced-out
// execution -- it is the piece HeartbeatBatchFenced's own store-level tests
// (engine/heartbeat_batch_fenced_test.go) cannot cover, since they never
// touch a Worker. These prove the wiring: a run HeartbeatBatchFenced reports
// lost gets its execCancel called, a run it does not report lost is left
// alone, and a call that errors entirely cancels nothing (decision 2's
// stand-down is the separate, slower response to sustained failure -- a
// single failed heartbeat must not cancel every in-flight execution).

// registerInflightExecution puts one run into both w.inflight (what
// heartbeatAndFenceInFlight snapshots to build its batch) and w.execCancel
// (what it calls to actually stop the execution), mirroring what
// executeWorkflow does at the top of a real run. It returns the run's own
// context so the test can observe cancellation.
func registerInflightExecution(w *Worker, id, defName string, generation int64) context.Context {
	w.inflight.Store(id, &engine.WorkflowInstance{ID: id, DefName: defName, Generation: generation})
	execCtx, execCancel := context.WithCancel(context.Background())
	w.execCancel.Store(id, execCancel)
	return execCtx
}

func TestHeartbeatAndFenceInFlightCancelsARunHeartbeatBatchFencedReportsLost(t *testing.T) {
	ms := &mockStore{
		heartbeatBatchFencedFn: func(ctx context.Context, workerID string, runs []engine.GenerationKey) ([]string, error) {
			return []string{"run-lost"}, nil
		},
	}
	w := newTestWorker(ms)
	execCtx := registerInflightExecution(w, "run-lost", "test-def", 7)

	w.heartbeatAndFenceInFlight()

	select {
	case <-execCtx.Done():
	default:
		t.Fatal("execCtx for run-lost was not cancelled -- HeartbeatBatchFenced reported it lost")
	}
	if !errors.Is(execCtx.Err(), context.Canceled) {
		t.Fatalf("execCtx.Err() = %v, want context.Canceled", execCtx.Err())
	}
	// The assertion that actually matters: fencedRuns, not execCtx, is what
	// freshCall refuses on via WithCanStartNewWork -- see fencedRuns' doc
	// comment on why execCtx cancellation alone does not stop a real guest's
	// next durable call.
	if !w.runIsFenced("run-lost") {
		t.Fatal("runIsFenced(\"run-lost\") = false -- HeartbeatBatchFenced reported it lost")
	}
}

func TestHeartbeatAndFenceInFlightLeavesARunAloneWhenNotReportedLost(t *testing.T) {
	ms := &mockStore{
		heartbeatBatchFencedFn: func(ctx context.Context, workerID string, runs []engine.GenerationKey) ([]string, error) {
			return nil, nil
		},
	}
	w := newTestWorker(ms)
	execCtx := registerInflightExecution(w, "run-live", "test-def", 3)

	w.heartbeatAndFenceInFlight()

	select {
	case <-execCtx.Done():
		t.Fatal("execCtx for run-live was cancelled -- HeartbeatBatchFenced did not report it lost")
	default:
	}
	if w.runIsFenced("run-live") {
		t.Fatal("runIsFenced(\"run-live\") = true -- HeartbeatBatchFenced did not report it lost")
	}
}

func TestHeartbeatAndFenceInFlightCancelsNothingWhenTheStoreCallErrors(t *testing.T) {
	ms := &mockStore{
		heartbeatBatchFencedFn: func(ctx context.Context, workerID string, runs []engine.GenerationKey) ([]string, error) {
			return nil, errors.New("connection reset")
		},
	}
	w := newTestWorker(ms)
	execCtx := registerInflightExecution(w, "run-during-outage", "test-def", 1)

	w.heartbeatAndFenceInFlight()

	select {
	case <-execCtx.Done():
		t.Fatal("execCtx was cancelled on a HeartbeatBatchFenced error -- a failed call says nothing about which runs are lost, only that we could not ask")
	default:
	}
	if w.runIsFenced("run-during-outage") {
		t.Fatal("runIsFenced(\"run-during-outage\") = true on a HeartbeatBatchFenced error -- a failed call says nothing about which runs are lost")
	}
}

func TestHeartbeatAndFenceInFlightSkipsARunAlreadyDeregistered(t *testing.T) {
	// The run finished (and deregistered its execCancel) between the
	// snapshot inside heartbeatAndFenceInFlight and the store call
	// returning it as lost -- a legitimate race, not a bug. Reported lost
	// with no execCancel entry must not panic.
	ms := &mockStore{
		heartbeatBatchFencedFn: func(ctx context.Context, workerID string, runs []engine.GenerationKey) ([]string, error) {
			return []string{"run-already-gone"}, nil
		},
	}
	w := newTestWorker(ms)
	w.inflight.Store("run-already-gone", &engine.WorkflowInstance{ID: "run-already-gone", DefName: "test-def", Generation: 1})
	// Deliberately no w.execCancel entry.

	w.heartbeatAndFenceInFlight()

	// No entry to clean up (executeWorkflow's defer already ran), so none
	// must be created -- see fencedRuns' doc comment on why it is set only
	// alongside a still-present execCancel.
	if w.runIsFenced("run-already-gone") {
		t.Fatal("runIsFenced(\"run-already-gone\") = true for a run whose execution had already finished")
	}
}

// cleat#2008 decision 2: a worker whose own heartbeats have been failing (or
// timing out) longer than the reclaim window must stop starting new durable
// calls -- not because any SPECIFIC run's fence is known lost (that is
// decision 1 above), but because it can no longer vouch for any of them.
// heartbeatPresumedLost is the predicate; the four tests below are its truth
// table against reclaimAfter().

func TestHeartbeatPresumedLostIsFalseRightAfterASuccessfulHeartbeat(t *testing.T) {
	w := newTestWorker(&mockStore{})
	w.reclaimTimeout = 100 * time.Millisecond
	w.lastHeartbeatOK.Store(time.Now().UnixNano())

	if w.heartbeatPresumedLost() {
		t.Fatal("heartbeatPresumedLost() = true immediately after a successful heartbeat")
	}
}

func TestHeartbeatPresumedLostIsTrueOnceTheReclaimWindowHasFullyElapsed(t *testing.T) {
	w := newTestWorker(&mockStore{})
	w.reclaimTimeout = 50 * time.Millisecond
	w.lastHeartbeatOK.Store(time.Now().Add(-time.Hour).UnixNano())

	if !w.heartbeatPresumedLost() {
		t.Fatal("heartbeatPresumedLost() = false an hour after the last successful heartbeat, reclaimAfter() far shorter")
	}
}

func TestHeartbeatAndFenceInFlightSuccessAdvancesLastHeartbeatOK(t *testing.T) {
	ms := &mockStore{
		heartbeatBatchFencedFn: func(ctx context.Context, workerID string, runs []engine.GenerationKey) ([]string, error) {
			return nil, nil
		},
	}
	w := newTestWorker(ms)
	w.reclaimTimeout = 50 * time.Millisecond
	w.lastHeartbeatOK.Store(time.Now().Add(-time.Hour).UnixNano())
	registerInflightExecution(w, "run-live", "test-def", 1)

	w.heartbeatAndFenceInFlight()

	if w.heartbeatPresumedLost() {
		t.Fatal("heartbeatPresumedLost() = true right after heartbeatAndFenceInFlight succeeded -- lastHeartbeatOK was not advanced")
	}
}

func TestHeartbeatAndFenceInFlightErrorDoesNotAdvanceLastHeartbeatOK(t *testing.T) {
	ms := &mockStore{
		heartbeatBatchFencedFn: func(ctx context.Context, workerID string, runs []engine.GenerationKey) ([]string, error) {
			return nil, errors.New("connection reset")
		},
	}
	w := newTestWorker(ms)
	w.reclaimTimeout = 50 * time.Millisecond
	staleSince := time.Now().Add(-time.Hour)
	w.lastHeartbeatOK.Store(staleSince.UnixNano())
	registerInflightExecution(w, "run-during-outage", "test-def", 1)

	w.heartbeatAndFenceInFlight()

	if !w.heartbeatPresumedLost() {
		t.Fatal("heartbeatPresumedLost() = false after a failed HeartbeatBatchFenced call -- an error must not advance lastHeartbeatOK")
	}
	if got := time.Unix(0, w.lastHeartbeatOK.Load()); !got.Equal(staleSince) {
		t.Fatalf("lastHeartbeatOK moved on error: got %v, want unchanged %v", got, staleSince)
	}
}

// The engine-level half -- proving WithCanStartNewWork actually refuses a
// fresh durable call -- lives in engine/can_start_new_work_test.go, next to
// the cancellation test it mirrors. This package cannot reach execSession or
// freshCall (unexported, and this is package main, not package engine).
