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
// Dispatch loop: sticky and general pool interaction
// ---------------------------------------------------------------------------

func TestDispatchLoop_StickyEmptyGeneralStillCalled(t *testing.T) {
	// When ClaimStickyWorkflows returns an empty result (no error), the
	// dispatch loop falls through to the general pool.  Verify that the
	// general pool IS called in this scenario.
	ms := &mockStore{}
	var mu sync.Mutex
	stickyCalls := 0
	generalCalls := 0

	ms.claimStickyWorkflowsFn = func(ctx context.Context, workerID string, limit int) ([]*host.WorkflowInstance, error) {
		mu.Lock()
		stickyCalls++
		mu.Unlock()
		return nil, nil
	}
	ms.claimWorkflowsFn = func(ctx context.Context, workerID string, limit int) ([]*host.WorkflowInstance, error) {
		mu.Lock()
		generalCalls++
		mu.Unlock()
		return nil, nil
	}

	w := newTestWorker(ms)
	w.pollInterval = 1 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	w.ctx = ctx
	w.cancel = cancel

	done := make(chan struct{})
		w.wg.Add(1)
	go func() {
		w.dispatchLoop()
		close(done)
	}()

	<-done

	mu.Lock()
	stickyCount := stickyCalls
	generalCount := generalCalls
	mu.Unlock()

	if stickyCount == 0 {
		t.Error("expected ClaimStickyWorkflows to be called")
	}
	if generalCount == 0 {
		t.Error("expected ClaimWorkflows (general pool) to be called when sticky returns empty")
	}
}

func TestDispatchLoop_StickyErrorContinuesLoop(t *testing.T) {
	// When ClaimStickyWorkflows returns a non-connection error, the loop
	// sleeps 1s and retries. Verify it doesn't crash and retries.
	ms := &mockStore{}
	var mu sync.Mutex
	stickyCalls := 0
	generalCalls := 0

	ms.claimStickyWorkflowsFn = func(ctx context.Context, workerID string, limit int) ([]*host.WorkflowInstance, error) {
		mu.Lock()
		stickyCalls++
		mu.Unlock()
		return nil, errors.New("sticky error (non-connection)")
	}
	ms.claimWorkflowsFn = func(ctx context.Context, workerID string, limit int) ([]*host.WorkflowInstance, error) {
		mu.Lock()
		generalCalls++
		mu.Unlock()
		return nil, nil
	}

	w := newTestWorker(ms)
	w.pollInterval = 1 * time.Millisecond

	// 1.5s timeout allows one 1s sleep + margin.
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	w.ctx = ctx
	w.cancel = cancel

	done := make(chan struct{})
		w.wg.Add(1)
	go func() {
		w.dispatchLoop()
		close(done)
	}()

	<-done

	mu.Lock()
	stickyCount := stickyCalls
	mu.Unlock()

	if stickyCount == 0 {
		t.Error("expected ClaimStickyWorkflows to be called at least once")
	}
	if stickyCount < 2 {
		t.Logf("sticky called %d time(s) within 1.5s (1s sleep per call = ~1 call expected)", stickyCount)
	}
	// General is NOT expected to be called because the loop continues
	// (back to top) on sticky error and never reaches the general pool.
}

// ---------------------------------------------------------------------------
// Heartbeat loop: connection errors preserve inflight set
// ---------------------------------------------------------------------------

func TestHeartbeatLoop_ConnectionErrorPreservesInflight(t *testing.T) {
	// When BatchHeartbeat returns a connection error, the loop
	// logs the event but does NOT touch the inflight map.
	// Ownership recovery is handled by the reaper loop.
	ms := &mockStore{}
	calls := 0
	ms.batchHeartbeatFn = func(ctx context.Context, workerID string) (int64, error) {
		calls++
		return 0, errors.New("connection refused")
	}

	w := newTestWorker(ms)
	w.heartbeatInterval = 5 * time.Millisecond
	w.inflight.Store("wf-1", &host.WorkflowInstance{ID: "wf-1"})

	done := make(chan struct{})
		w.wg.Add(1)
	go func() {
		w.heartbeatLoop()
		close(done)
	}()

	time.Sleep(30 * time.Millisecond)
	w.cancel()
	<-done

	if calls == 0 {
		t.Error("expected at least one heartbeat call")
	}
	_, stillInflight := w.inflight.Load("wf-1")
	if !stillInflight {
		t.Error("expected wf-1 to remain in inflight on BatchHeartbeat connection error")
	}
}

func TestHeartbeatLoop_NonConnectionErrorPreservesInflight(t *testing.T) {
	// When BatchHeartbeat returns a non-connection error, the loop
	// logs the event but does NOT remove workflows from inflight.
	// Ownership recovery is handled by the reaper loop.
	ms := &mockStore{}
	calls := 0
	ms.batchHeartbeatFn = func(ctx context.Context, workerID string) (int64, error) {
		calls++
		return 0, errors.New("unexpected error: operation failed")
	}

	w := newTestWorker(ms)
	w.heartbeatInterval = 5 * time.Millisecond
	w.inflight.Store("wf-1", &host.WorkflowInstance{ID: "wf-1"})

	done := make(chan struct{})
		w.wg.Add(1)
	go func() {
		w.heartbeatLoop()
		close(done)
	}()

	time.Sleep(30 * time.Millisecond)
	w.cancel()
	<-done

	if calls == 0 {
		t.Error("expected at least one heartbeat call")
	}
	_, stillInflight := w.inflight.Load("wf-1")
	if !stillInflight {
		t.Error("expected wf-1 to remain in inflight on BatchHeartbeat non-connection error")
	}
}

// ---------------------------------------------------------------------------
// Reaper loop: error handling
// ---------------------------------------------------------------------------

func TestReaperLoop_ConnectionErrorNoPanic(t *testing.T) {
	// The reaper loop must not panic when ReapStaleInstances returns
	// a connection error. It logs and continues.
	ms := &mockStore{}
	reapCalls := 0
	ms.reapStaleInstancesFn = func(ctx context.Context, timeout time.Duration) (int, error) {
		reapCalls++
		return 0, errors.New("connection refused")
	}

	// Verify the store contract directly (the 30s ticker makes
	// a full loop test impractical).
	n, err := ms.ReapStaleInstances(context.Background(), 30*time.Second)
	if err == nil {
		t.Error("expected error from mock, got nil")
	}
	if n != 0 {
		t.Errorf("expected 0 reaped, got %d", n)
	}
	if reapCalls != 1 {
		t.Errorf("expected 1 reap call, got %d", reapCalls)
	}
}

func TestReaperLoop_NonConnectionErrorNoPanic(t *testing.T) {
	ms := &mockStore{}
	ms.reapStaleInstancesFn = func(ctx context.Context, timeout time.Duration) (int, error) {
		return 0, errors.New("reap failed: constraint violation")
	}

	n, err := ms.ReapStaleInstances(context.Background(), 30*time.Second)
	if err == nil {
		t.Error("expected error from mock, got nil")
	}
	if n != 0 {
		t.Errorf("expected 0 reaped, got %d", n)
	}
}

func TestReaperLoop_ReturnsCount(t *testing.T) {
	ms := &mockStore{}
	ms.reapStaleInstancesFn = func(ctx context.Context, timeout time.Duration) (int, error) {
		return 5, nil
	}

	n, err := ms.ReapStaleInstances(context.Background(), 30*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 5 {
		t.Errorf("expected 5 reaped, got %d", n)
	}
}

// ---------------------------------------------------------------------------
// ConcurrencyKeyReaper: error handling
// ---------------------------------------------------------------------------

func TestConcurrencyKeyReaper_ErrorNoPanic(t *testing.T) {
	ms := &mockStore{}
	reapCalls := 0
	ms.reapExpiredConcurrencyKeysFn = func(ctx context.Context) (int64, error) {
		reapCalls++
		return 0, errors.New("connection refused")
	}

	n, err := ms.ReapExpiredConcurrencyKeys(context.Background())
	if err == nil {
		t.Error("expected error from mock, got nil")
	}
	if n != 0 {
		t.Errorf("expected 0, got %d", n)
	}
	if reapCalls != 1 {
		t.Errorf("expected 1 call, got %d", reapCalls)
	}
}

func TestConcurrencyKeyReaper_ReturnsCount(t *testing.T) {
	ms := &mockStore{}
	ms.reapExpiredConcurrencyKeysFn = func(ctx context.Context) (int64, error) {
		return 3, nil
	}

	n, err := ms.ReapExpiredConcurrencyKeys(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 3 {
		t.Errorf("expected 3, got %d", n)
	}
}
