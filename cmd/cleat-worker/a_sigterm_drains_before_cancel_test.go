package main

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
)

// cleat#2285: SIGTERM used to cancel the worker's lifetime context at once. Every in-flight durable call
// aborted, and the run was written FAILED -- permanently, because a terminal run is never reclaimed -- so a
// rolling deploy lost every run it interrupted. These tests pin the four parts of the repair; the
// end-to-end version, with a real process and a real signal, is tests/crash.

// waitFor polls cond for up to 2s. Every use is on a quantity that only ever changes one way, so it cannot
// pass early.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestGracefulShutdown_DoesNotCancelWhileARunIsInFlight(t *testing.T) {
	w := newTestWorker(&mockStore{})
	w.drainCh = make(chan struct{})
	w.inflight.Store("wf-1", &engine.WorkflowInstance{ID: "wf-1"})

	got := make(chan string, 1)
	go func() { got <- w.gracefulShutdown(time.Minute, make(chan struct{})) }()

	waitFor(t, "the worker to start draining", w.draining.Load)
	// The property: with a run in flight and grace remaining, the lifetime context stays alive, which is
	// what keeps heartbeats, the API and the run's own writes working.
	time.Sleep(250 * time.Millisecond)
	if w.ctx.Err() != nil {
		t.Fatal("the worker was cancelled while a run was still in flight and grace remained")
	}
	select {
	case why := <-got:
		t.Fatalf("gracefulShutdown returned early: %s", why)
	default:
	}

	w.inflight.Delete("wf-1")
	select {
	case why := <-got:
		if !strings.Contains(why, "finished") {
			t.Errorf("reason = %q, want it to say the runs finished", why)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("gracefulShutdown did not return after the last run finished")
	}
	if w.ctx.Err() == nil {
		t.Error("the worker was not cancelled after the drain completed")
	}
	select {
	case <-w.drainCh:
	default:
		t.Error("drainCh was not closed when the drain completed")
	}
}

func TestGracefulShutdown_CancelsWhenTheGraceEnds(t *testing.T) {
	w := newTestWorker(&mockStore{})
	w.drainCh = make(chan struct{})
	w.inflight.Store("wf-stuck", &engine.WorkflowInstance{ID: "wf-stuck"})

	start := time.Now()
	why := w.gracefulShutdown(150*time.Millisecond, make(chan struct{}))
	if !strings.Contains(why, "grace period") {
		t.Errorf("reason = %q, want it to name the grace period", why)
	}
	if e := time.Since(start); e < 140*time.Millisecond {
		t.Errorf("cancelled after %v, before the 150ms grace", e)
	}
	if w.ctx.Err() == nil {
		t.Error("a run that outlasts the grace period must be cancelled")
	}
}

func TestGracefulShutdown_ASecondSignalCancelsAtOnce(t *testing.T) {
	w := newTestWorker(&mockStore{})
	w.drainCh = make(chan struct{})
	w.inflight.Store("wf-stuck", &engine.WorkflowInstance{ID: "wf-stuck"})

	force := make(chan struct{})
	got := make(chan string, 1)
	go func() { got <- w.gracefulShutdown(time.Minute, force) }()
	waitFor(t, "the worker to start draining", w.draining.Load)
	if w.ctx.Err() != nil {
		t.Fatal("cancelled before the second signal")
	}
	close(force)
	select {
	case why := <-got:
		if !strings.Contains(why, "second signal") {
			t.Errorf("reason = %q", why)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a second signal did not end the wait")
	}
	if w.ctx.Err() == nil {
		t.Error("a second signal must cancel the worker")
	}
}

func TestGracefulShutdown_NothingInFlightFinishesImmediately(t *testing.T) {
	w := newTestWorker(&mockStore{})
	w.drainCh = make(chan struct{})
	start := time.Now()
	w.gracefulShutdown(time.Minute, make(chan struct{}))
	if time.Since(start) > time.Second {
		t.Error("an idle worker should not wait out the grace period")
	}
	if w.ctx.Err() == nil {
		t.Error("an idle worker must still be cancelled")
	}
}

// POST /api/admin/drain is a cordon. In Kubernetes a POST that made the process exit would be a container
// restart, and the restarted worker would resume claiming: "take out of rotation" must not end the process.
func TestDispatchLoop_ACordonedWorkerStopsClaimingButDoesNotExit(t *testing.T) {
	w := newTestWorker(&mockStore{})
	w.drainCh = make(chan struct{})
	w.draining.Store(true)

	done := make(chan struct{})
	w.wg.Add(1)
	go func() { w.dispatchLoop(); close(done) }()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("dispatchLoop did not stop claiming for a cordoned worker")
	}
	select {
	case <-w.drainCh:
		t.Error("a cordon completed the drain: it must leave that to SIGTERM or an operator")
	default:
	}
	if w.ctx.Err() != nil {
		t.Error("a cordon cancelled the worker: in Kubernetes that is a restart that resumes claiming")
	}
}

// The choke point. Everything that ends a run because the worker is going away arrives at
// writeTerminalFailure looking like a failure; during shutdown it must release, not record.
func TestWriteTerminalFailure_ReleasesInsteadOfFailingDuringShutdown(t *testing.T) {
	for _, shuttingDown := range []bool{true, false} {
		name := "worker running (known-positive: this must still fail the run)"
		if shuttingDown {
			name = "worker shutting down"
		}
		t.Run(name, func(t *testing.T) {
			var failed, released atomic.Int32
			var releaseCtxErr atomic.Value
			ms := &mockStore{}
			ms.failWorkflowFn = func(ctx context.Context, _, _ string, _ int64, _, _, _ string, _ map[string]string) error {
				failed.Add(1)
				return nil
			}
			ms.releaseWorkflowFn = func(ctx context.Context, _, _ string, _ int64, _ time.Time) error {
				released.Add(1)
				releaseCtxErr.Store(ctx.Err() == nil)
				return nil
			}
			w := newTestWorker(ms)
			if shuttingDown {
				w.cancel()
			}
			wf := &engine.WorkflowInstance{ID: "wf-1", DefName: "d", DefVersion: 1, Generation: 1}
			applied, _ := w.writeTerminalFailure(wf, "call aborted: context canceled", engine.ErrUnknown.String(), "", false, nil)

			if shuttingDown {
				if failed.Load() != 0 || applied {
					t.Errorf("a run cut off by shutdown was recorded FAILED (failed=%d applied=%v)", failed.Load(), applied)
				}
				if released.Load() != 1 {
					t.Fatalf("released %d times, want 1: the run must go back to the queue", released.Load())
				}
				if ok, _ := releaseCtxErr.Load().(bool); !ok {
					t.Error("the release ran on a cancelled context, so it would fail at `begin tx` exactly like the finalize did")
				}
				return
			}
			if failed.Load() != 1 || released.Load() != 0 {
				t.Errorf("a genuine failure on a running worker must still be recorded (failed=%d released=%d)", failed.Load(), released.Load())
			}
		})
	}
}

// A finalize that was in flight when the grace ended comes back `context canceled`. That routes to a release and
// is not retried. On MySQL the finalize may in fact have committed after the client gave up, which cleared
// assigned_to, so the fenced release matches no row and reports ErrFenceLost: it must be a quiet no-op, never
// an error that fails the run.
func TestAFinalizeCancelledByShutdownIsReleasedNotRetriedNotFailed(t *testing.T) {
	var finalized, failed, released atomic.Int32
	ms := &mockStore{}
	ms.finalizeWorkflowSegmentFn = func(context.Context, string, string, int64, []engine.EventRecord, string, string, string, string, map[string]string, time.Time) error {
		finalized.Add(1)
		return context.Canceled
	}
	ms.failWorkflowFn = func(context.Context, string, string, int64, string, string, string, map[string]string) error {
		failed.Add(1)
		return nil
	}
	ms.releaseWorkflowFn = func(context.Context, string, string, int64, time.Time) error {
		released.Add(1)
		return engine.ErrFenceLost // the finalize committed after the client gave up
	}
	w := newTestWorker(ms)
	w.cancel()

	wf := &engine.WorkflowInstance{ID: "wf-1", DefName: "d", DefVersion: 1, Generation: 1}
	applied, dead := w.writeTerminalFailure(wf, "finalize workflow: begin tx: context canceled", engine.ErrUnknown.String(), "", false, nil)

	if applied || dead || failed.Load() != 0 {
		t.Errorf("a cancelled finalize was recorded as a failure (applied=%v dead=%v failWorkflow calls=%d)", applied, dead, failed.Load())
	}
	if released.Load() != 1 {
		t.Errorf("release attempted %d times, want exactly 1", released.Load())
	}
	if finalized.Load() != 0 {
		t.Errorf("finalize was retried %d times on the shutdown path", finalized.Load())
	}
}
