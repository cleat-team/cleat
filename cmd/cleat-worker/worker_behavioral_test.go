// Package main contains behavioral tests for the cleat-worker daemon.
// These tests use the mockStore from worker_daemon_test.go to simulate
// database operations without real PostgreSQL.
package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/rcownie/cleat/internal/host"
)

// ---------------------------------------------------------------------------
// Dispatch loop: capacity limit
// ---------------------------------------------------------------------------

func TestDispatchLoop_CapacityLimit(t *testing.T) {
	// When inflight reaches the concurrency limit, the dispatch loop should
	// skip claiming and sleep instead.
	ms := &mockStore{}
	claimAttempts := 0
	ms.claimStickyWorkflowsFn = func(ctx context.Context, workerID, namespace string, limit int) ([]*host.WorkflowInstance, error) {
		claimAttempts++
		return nil, nil
	}
	ms.claimWorkflowsFn = func(ctx context.Context, workerID, namespace string, limit int) ([]*host.WorkflowInstance, error) {
		return nil, nil
	}

	w := newTestWorkerWithConcurrency(ms, 1)

	// Fill inflight to capacity.
	w.inflight.Store("wf-busy-1", &host.WorkflowInstance{ID: "wf-busy-1", DefName: "test", DefVersion: 1})

	// Run briefly then cancel.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	w.ctx = ctx
	w.cancel = cancel

	done := make(chan struct{})
	go func() {
		w.wg.Add(1)
		w.dispatchLoop()
		close(done)
	}()

	<-done

	if claimAttempts > 0 {
		t.Errorf("expected 0 claim attempts when at capacity, got %d", claimAttempts)
	}
}

// ---------------------------------------------------------------------------
// Dispatch loop: sticky reclaim
// ---------------------------------------------------------------------------

func TestDispatchLoop_StickyReclaim(t *testing.T) {
	// When a sticky workflow exists for this worker, ClaimStickyWorkflows
	// returns it and the workflow is added to inflight.
	ms := &mockStore{}
	loadWASMCh := make(chan struct{})
	claimedCh := make(chan struct{})

	ms.claimStickyWorkflowsFn = func(ctx context.Context, workerID, namespace string, limit int) ([]*host.WorkflowInstance, error) {
		return []*host.WorkflowInstance{
			{ID: "wf-sticky-reclaim", DefName: "test", DefVersion: 1, Status: "ready"},
		}, nil
	}
	ms.claimWorkflowsFn = func(ctx context.Context, workerID, namespace string, limit int) ([]*host.WorkflowInstance, error) {
		return nil, nil
	}
	ms.loadWASMFn = func(ctx context.Context, defName string, defVersion int) ([]byte, error) {
		close(claimedCh)
		<-loadWASMCh
		return nil, nil
	}
	ms.failWorkflowFn = func(ctx context.Context, workflowID, workerID, errMsg, errorCode, errorOp string, queryState map[string]string) error {
		return nil
	}

	w := newTestWorker(ms)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		w.wg.Add(1)
		w.dispatchLoop()
	}()

	// Wait for the sticky workflow to be claimed (goroutine blocked in loadWASM).
	select {
	case <-claimedCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for sticky reclaim")
	}

	// Verify it's in inflight.
	_, ok := w.inflight.Load("wf-sticky-reclaim")
	if !ok {
		t.Error("expected wf-sticky-reclaim to be in inflight")
	}

	close(loadWASMCh)
	w.cancel()
	wg.Wait()
}

// ---------------------------------------------------------------------------
// Heartbeat loop: success preserves inflight
// ---------------------------------------------------------------------------

func TestHeartbeatLoop_SuccessPreservesInflight(t *testing.T) {
	// When Heartbeat returns alive=true, the workflow remains in inflight.
	ms := &mockStore{}
	var mu sync.Mutex
	heartbeatCount := 0
	ms.heartbeatFn = func(ctx context.Context, workflowID, workerID string) (bool, error) {
		mu.Lock()
		heartbeatCount++
		mu.Unlock()
		return true, nil
	}

	w := newTestWorker(ms)
	w.heartbeatInterval = 5 * time.Millisecond
	w.inflight.Store("wf-alive-1", &host.WorkflowInstance{ID: "wf-alive-1"})

	done := make(chan struct{})
	go func() {
		w.wg.Add(1)
		w.heartbeatLoop()
		close(done)
	}()

	time.Sleep(30 * time.Millisecond)
	w.cancel()
	<-done

	mu.Lock()
	count := heartbeatCount
	mu.Unlock()

	if count == 0 {
		t.Error("expected at least one heartbeat call")
	}

	// Workflow should still be in inflight after successful heartbeats.
	_, ok := w.inflight.Load("wf-alive-1")
	if !ok {
		t.Error("expected wf-alive-1 to remain in inflight after successful heartbeat")
	}
}

// ---------------------------------------------------------------------------
// Heartbeat loop: lost ownership
// ---------------------------------------------------------------------------

func TestHeartbeatLoop_LostOwnership(t *testing.T) {
	// When Heartbeat returns alive=false (non-connection error), the workflow
	// is removed from inflight.
	ms := &mockStore{}
	callCount := 0
	ms.heartbeatFn = func(ctx context.Context, workflowID, workerID string) (bool, error) {
		callCount++
		if workflowID == "wf-lost-own" {
			return false, nil
		}
		return true, nil
	}

	w := newTestWorker(ms)
	w.heartbeatInterval = 5 * time.Millisecond
	w.inflight.Store("wf-lost-own", &host.WorkflowInstance{ID: "wf-lost-own"})
	w.inflight.Store("wf-keep-1", &host.WorkflowInstance{ID: "wf-keep-1"})

	done := make(chan struct{})
	go func() {
		w.wg.Add(1)
		w.heartbeatLoop()
		close(done)
	}()

	time.Sleep(30 * time.Millisecond)
	w.cancel()
	<-done

	if callCount == 0 {
		t.Error("expected at least one heartbeat call")
	}

	_, lost := w.inflight.Load("wf-lost-own")
	if lost {
		t.Error("expected wf-lost-own to be removed from inflight after lost ownership")
	}

	_, kept := w.inflight.Load("wf-keep-1")
	if !kept {
		t.Error("expected wf-keep-1 to remain in inflight")
	}
}

// ---------------------------------------------------------------------------
// waitForDB retries on error
// ---------------------------------------------------------------------------

func TestWaitForDB_RetriesOnError(t *testing.T) {
	// waitForDB loops calling ClaimWorkflow until it succeeds or 20 retries.
	// Set up the mock to return a connection error a few times, then succeed.
	ms := &mockStore{}
	attempts := 0
	const connErrMsg = "connection refused"
	ms.claimWorkflowFn = func(ctx context.Context, workerID, namespace string) (*host.WorkflowInstance, error) {
		attempts++
		if attempts <= 2 {
			return nil, errors.New(connErrMsg)
		}
		return nil, nil // nil result + nil error means DB is reachable but empty
	}

	w := newTestWorker(ms)
	// Use a short context to cap the test duration.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	w.ctx = ctx
	w.cancel = cancel

	w.waitForDB()

	if attempts < 2 {
		t.Errorf("expected at least 2 retries (connection errors), got %d", attempts)
	}
	if attempts > 20 {
		t.Errorf("expected no more than 20 retries, got %d", attempts)
	}
}

func TestWaitForDB_ImmediateSuccess(t *testing.T) {
	// When ClaimWorkflow succeeds immediately, waitForDB should return
	// after a single call.
	ms := &mockStore{}
	attempts := 0
	ms.claimWorkflowFn = func(ctx context.Context, workerID, namespace string) (*host.WorkflowInstance, error) {
		attempts++
		return nil, nil
	}

	w := newTestWorker(ms)
	w.waitForDB()

	if attempts != 1 {
		t.Errorf("expected exactly 1 claim attempt on immediate success, got %d", attempts)
	}
}

// ---------------------------------------------------------------------------
// releaseOrFail: release without error
// ---------------------------------------------------------------------------

func TestReleaseOrFail_NoError(t *testing.T) {
	// When errMsg is empty, releaseOrFail should call ReleaseWorkflow
	// (not FailWorkflow).
	ms := &mockStore{}
	released := false
	failed := false
	ms.releaseWorkflowFn = func(ctx context.Context, workflowID, workerID string, nextWakeAt time.Time) error {
		released = true
		return nil
	}
	ms.failWorkflowFn = func(ctx context.Context, workflowID, workerID, errMsg, errorCode, errorOp string, queryState map[string]string) error {
		failed = true
		return nil
	}

	w := newTestWorker(ms)
	w.id = "test-worker"

	nextWake := time.Now().Add(time.Hour)
	w.releaseOrFail(&host.WorkflowInstance{ID: "wf-release-1", NextWakeAt: nextWake}, "")

	if !released {
		t.Error("expected ReleaseWorkflow to be called")
	}
	if failed {
		t.Error("expected FailWorkflow NOT to be called when errMsg is empty")
	}
}
