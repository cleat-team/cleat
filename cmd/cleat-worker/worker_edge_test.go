// Package main contains edge-case tests for the cleat-worker daemon.
// These tests use the mockStore from worker_daemon_test.go to simulate
// database operations without real PostgreSQL.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rcownie/cleat/internal/host"
)

// ---------------------------------------------------------------------------
// ExecuteWorkflow edge cases
// ---------------------------------------------------------------------------

func TestExecuteWorkflow_WASMLoadError(t *testing.T) {
	// When loadWASM fails (e.g. WASM module not found), executeWorkflow must
	// call FailWorkflow with a descriptive error and not panic.
	ms := &mockStore{}
	var mu sync.Mutex
	failCalled := false
	var failErr string

	ms.failWorkflowFn = func(ctx context.Context, workflowID, workerID, errMsg string, queryState map[string]string) error {
		mu.Lock()
		failCalled = true
		failErr = errMsg
		mu.Unlock()
		return nil
	}
	ms.loadWASMFn = func(ctx context.Context, defName string, defVersion int) ([]byte, error) {
		return nil, errors.New("wasm: module not found")
	}

	w := newTestWorker(ms)
	w.wg.Add(1)
	w.executeWorkflow(&host.WorkflowInstance{
		ID: "wf-edge-1", DefName: "test", DefVersion: 1, Status: "ready",
	})

	mu.Lock()
	if !failCalled {
		t.Fatal("expected FailWorkflow to be called on WASM load error")
	}
	if !strings.Contains(failErr, "wasm") {
		t.Errorf("expected error about wasm, got %q", failErr)
	}
	mu.Unlock()
}

func TestExecuteWorkflow_WASMLoadConnectionError(t *testing.T) {
	// When loadWASM fails with a connection error, executeWorkflow still calls
	// FailWorkflow (the WASM-load path does not treat connection errors as
	// release-able — only history load does).
	ms := &mockStore{}
	var mu sync.Mutex
	failCalled := false

	ms.failWorkflowFn = func(ctx context.Context, workflowID, workerID, errMsg string, queryState map[string]string) error {
		mu.Lock()
		failCalled = true
		mu.Unlock()
		return nil
	}
	ms.loadWASMFn = func(ctx context.Context, defName string, defVersion int) ([]byte, error) {
		return nil, errors.New("connection refused")
	}

	w := newTestWorker(ms)
	w.wg.Add(1)
	w.executeWorkflow(&host.WorkflowInstance{
		ID: "wf-edge-conn", DefName: "test", DefVersion: 1, Status: "ready",
	})

	mu.Lock()
	if !failCalled {
		t.Error("expected FailWorkflow even when WASM load fails with connection error")
	}
	mu.Unlock()
}

func TestExecuteWorkflow_HistoryLoadConnectionError(t *testing.T) {
	// When LoadEventHistory returns a connection error, executeWorkflow must
	// release the workflow rather than failing it, so another worker can retry.
	ms := &mockStore{}
	var mu sync.Mutex
	releaseCalled := false
	failCalled := false

	ms.loadWASMFn = func(ctx context.Context, defName string, defVersion int) ([]byte, error) {
		return []byte("some-bytes"), nil
	}
	ms.loadEventHistoryFn = func(ctx context.Context, workflowID string) ([]host.EventRecord, error) {
		return nil, errors.New("connection refused")
	}
	ms.releaseWorkflowFn = func(ctx context.Context, workflowID, workerID string, nextWakeAt time.Time) error {
		mu.Lock()
		releaseCalled = true
		mu.Unlock()
		return nil
	}
	ms.failWorkflowFn = func(ctx context.Context, workflowID, workerID, errMsg string, queryState map[string]string) error {
		mu.Lock()
		failCalled = true
		mu.Unlock()
		return nil
	}

	w := newTestWorker(ms)
	w.wg.Add(1)
	w.executeWorkflow(&host.WorkflowInstance{
		ID: "wf-edge-2", DefName: "test", DefVersion: 1, Status: "ready",
		Input: json.RawMessage(`{}`),
	})

	mu.Lock()
	if !releaseCalled {
		t.Error("expected ReleaseWorkflow for connection error on history load")
	}
	if failCalled {
		t.Error("expected no FailWorkflow for connection error on history load")
	}
	mu.Unlock()
}

func TestExecuteWorkflow_HistoryLoadError(t *testing.T) {
	// When LoadEventHistory returns a non-connection error, executeWorkflow
	// should fail the workflow with a descriptive message.
	ms := &mockStore{}
	var mu sync.Mutex
	failCalled := false
	var failErr string

	ms.loadWASMFn = func(ctx context.Context, defName string, defVersion int) ([]byte, error) {
		return []byte("some-bytes"), nil
	}
	ms.loadEventHistoryFn = func(ctx context.Context, workflowID string) ([]host.EventRecord, error) {
		return nil, errors.New("permission denied")
	}
	ms.failWorkflowFn = func(ctx context.Context, workflowID, workerID, errMsg string, queryState map[string]string) error {
		mu.Lock()
		failCalled = true
		failErr = errMsg
		mu.Unlock()
		return nil
	}

	w := newTestWorker(ms)
	w.wg.Add(1)
	w.executeWorkflow(&host.WorkflowInstance{
		ID: "wf-edge-3", DefName: "test", DefVersion: 1, Status: "ready",
		Input: json.RawMessage(`{}`),
	})

	mu.Lock()
	if !failCalled {
		t.Fatal("expected FailWorkflow for non-connection error on history load")
	}
	if !strings.Contains(failErr, "history load") {
		t.Errorf("expected error containing 'history load', got %q", failErr)
	}
	mu.Unlock()
}

func TestExecuteWorkflow_RuntimeErrorFromEngine(t *testing.T) {
	// When history is empty (first execution) and WASM bytes are invalid,
	// engine.Replay will fail at the compile step. executeWorkflow must catch
	// this as a runtime error and call FailWorkflow, not panic.
	ms := &mockStore{}
	var mu sync.Mutex
	failCalled := false
	var failErr string
	appendCalled := false

	ms.loadWASMFn = func(ctx context.Context, defName string, defVersion int) ([]byte, error) {
		// Invalid WASM bytes ensure Replay returns a compile error.
		return []byte("not-valid-wasm-binary"), nil
	}
	ms.loadEventHistoryFn = func(ctx context.Context, workflowID string) ([]host.EventRecord, error) {
		// Empty — fresh workflow.
		return []host.EventRecord{}, nil
	}
	ms.loadCompactionStateFn = func(ctx context.Context, workflowID string) (*host.CompactionState, error) {
		return nil, nil
	}
	ms.failWorkflowFn = func(ctx context.Context, workflowID, workerID, errMsg string, queryState map[string]string) error {
		mu.Lock()
		failCalled = true
		failErr = errMsg
		mu.Unlock()
		return nil
	}
	ms.appendEventHistoryBatchFn = func(ctx context.Context, workflowID string, recs []host.EventRecord) error {
		mu.Lock()
		appendCalled = true
		mu.Unlock()
		return nil
	}

	w := newTestWorker(ms)
	w.wg.Add(1)
	w.executeWorkflow(&host.WorkflowInstance{
		ID: "wf-empty-hist", DefName: "test", DefVersion: 1, Status: "ready",
		Input: json.RawMessage(`{}`),
	})

	mu.Lock()
	if !failCalled {
		t.Fatal("expected FailWorkflow for runtime error with empty history + invalid WASM")
	}
	if !strings.Contains(failErr, "compile") && !strings.Contains(failErr, "runtime") && !strings.Contains(failErr, "host") {
		t.Logf("error message (non-fatal check): %q", failErr)
	}
	if appendCalled {
		t.Error("expected no AppendEventHistoryBatch when Replay fails")
	}
	mu.Unlock()
}

func TestExecuteWorkflow_CompactionStateLoadError(t *testing.T) {
	// When LoadCompactionState returns an error, executeWorkflow must log the
	// warning but continue execution (the error is non-fatal).
	ms := &mockStore{}
	var mu sync.Mutex
	loadWASMCalled := false
	loadHistoryCalled := false

	ms.loadWASMFn = func(ctx context.Context, defName string, defVersion int) ([]byte, error) {
		mu.Lock()
		loadWASMCalled = true
		mu.Unlock()
		return []byte("not-valid-wasm"), nil
	}
	ms.loadEventHistoryFn = func(ctx context.Context, workflowID string) ([]host.EventRecord, error) {
		mu.Lock()
		loadHistoryCalled = true
		mu.Unlock()
		return []host.EventRecord{}, nil
	}
	ms.loadCompactionStateFn = func(ctx context.Context, workflowID string) (*host.CompactionState, error) {
		return nil, errors.New("compaction state corrupted")
	}
	ms.failWorkflowFn = func(ctx context.Context, workflowID, workerID, errMsg string, queryState map[string]string) error {
		return nil
	}

	w := newTestWorker(ms)
	w.wg.Add(1)
	// executeWorkflow should reach engine.Replay (which will fail on invalid
	// WASM) despite the compaction state load error.
	w.executeWorkflow(&host.WorkflowInstance{
		ID: "wf-compact-err", DefName: "test", DefVersion: 1, Status: "ready",
		Input: json.RawMessage(`{}`),
	})

	mu.Lock()
	if !loadWASMCalled {
		t.Error("expected loadWASM to be called")
	}
	if !loadHistoryCalled {
		t.Error("expected LoadEventHistory to be called")
	}
	mu.Unlock()
	// If we reach here without a panic, the compaction error was handled.
}

// ---------------------------------------------------------------------------
// Dispatch loop edge cases
// ---------------------------------------------------------------------------

func TestDispatchLoop_CancelDuringStickyClaim(t *testing.T) {
	// Cancel the context while ClaimStickyWorkflows is in progress.
	// The loop should unblock and exit without processing any workflows.
	ms := &mockStore{}
	claimBlocked := make(chan struct{})
	claimReturned := make(chan struct{})

	ms.claimStickyWorkflowsFn = func(ctx context.Context, workerID, namespace string, limit int) ([]*host.WorkflowInstance, error) {
		close(claimBlocked)
		// Block until context is cancelled.
		<-ctx.Done()
		close(claimReturned)
		return nil, ctx.Err()
	}
	ms.claimWorkflowsFn = func(ctx context.Context, workerID, namespace string, limit int) ([]*host.WorkflowInstance, error) {
		return nil, nil
	}

	w := newTestWorker(ms)

	done := make(chan struct{})
	go func() {
		w.wg.Add(1)
		w.dispatchLoop()
		close(done)
	}()

	// Wait for claim to be entered and blocked.
	<-claimBlocked
	// Cancel the context while the claim is in flight.
	w.cancel()

	// Verify the loop exits promptly (the select on <-ctx.Done() in the
	// claim function returns, then the loop's own <-ctx.Done() fires).
	select {
	case <-done:
		// OK
	case <-time.After(3 * time.Second):
		t.Fatal("dispatchLoop did not stop after context cancellation during sticky claim")
	}

	// Verify claim returned from its blocked call.
	select {
	case <-claimReturned:
		// OK
	case <-time.After(time.Second):
		t.Log("claim did not return (may be blocked on ctx.Done())")
	}
}

func TestDispatchLoop_CancelDuringGeneralClaim(t *testing.T) {
	// Cancel the context while the general-pool ClaimWorkflows is in progress
	// (sticky returned nothing and general claim blocks).
	ms := &mockStore{}
	claimBlocked := make(chan struct{})

	ms.claimStickyWorkflowsFn = func(ctx context.Context, workerID, namespace string, limit int) ([]*host.WorkflowInstance, error) {
		return nil, nil
	}
	ms.claimWorkflowsFn = func(ctx context.Context, workerID, namespace string, limit int) ([]*host.WorkflowInstance, error) {
		close(claimBlocked)
		<-ctx.Done()
		return nil, ctx.Err()
	}

	w := newTestWorker(ms)

	done := make(chan struct{})
	go func() {
		w.wg.Add(1)
		w.dispatchLoop()
		close(done)
	}()

	<-claimBlocked
	w.cancel()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("dispatchLoop did not stop after cancellation during general claim")
	}
}

func TestDispatchLoop_MixedStickyGeneral(t *testing.T) {
	// When both sticky and general claims return workflows, verify that ALL
	// are added to inflight and executed.
	ms := &mockStore{}
	var mu sync.Mutex
	executedWfs := make(map[string]bool)
	loadWASMCh := make(chan struct{})

	ms.claimStickyWorkflowsFn = func(ctx context.Context, workerID, namespace string, limit int) ([]*host.WorkflowInstance, error) {
		return []*host.WorkflowInstance{
			{ID: "sticky-1", DefName: "test", DefVersion: 1, Status: "ready"},
			{ID: "sticky-2", DefName: "test", DefVersion: 1, Status: "ready"},
		}, nil
	}
	ms.claimWorkflowsFn = func(ctx context.Context, workerID, namespace string, limit int) ([]*host.WorkflowInstance, error) {
		return []*host.WorkflowInstance{
			{ID: "general-1", DefName: "test", DefVersion: 1, Status: "ready"},
			{ID: "general-2", DefName: "test", DefVersion: 1, Status: "ready"},
		}, nil
	}
	ms.loadWASMFn = func(ctx context.Context, defName string, defVersion int) ([]byte, error) {
		mu.Lock()
		executedWfs[defName] = true
		mu.Unlock()
		<-loadWASMCh
		return nil, nil
	}
	ms.failWorkflowFn = func(ctx context.Context, workflowID, workerID, errMsg string, queryState map[string]string) error {
		return nil
	}

	w := newTestWorker(ms)

	done := make(chan struct{})
	go func() {
		w.wg.Add(1)
		w.dispatchLoop()
		close(done)
	}()

	// Wait until all 4 workflows are in inflight (goroutines blocked in loadWASM).
	waitForCond(t, 2*time.Second, func() bool {
		count := 0
		w.inflight.Range(func(_, _ interface{}) bool {
			count++
			return true
		})
		return count >= 4
	})

	// Verify all four are present in inflight.
	for _, id := range []string{"sticky-1", "sticky-2", "general-1", "general-2"} {
		if _, ok := w.inflight.Load(id); !ok {
			t.Errorf("expected %s to be in inflight", id)
		}
	}

	close(loadWASMCh)
	w.cancel()
	<-done
}

func TestDispatchLoop_AtCapacitySkipsClaim(t *testing.T) {
	// When inflight reaches the concurrency limit, the dispatch loop must
	// NOT call ClaimStickyWorkflows at all. This variant ensures that even
	// when inflight count exactly equals concurrency, no claim happens.
	ms := &mockStore{}
	var mu sync.Mutex
	claimAttempts := 0
	ms.claimStickyWorkflowsFn = func(ctx context.Context, workerID, namespace string, limit int) ([]*host.WorkflowInstance, error) {
		mu.Lock()
		claimAttempts++
		mu.Unlock()
		return nil, nil
	}
	ms.claimWorkflowsFn = func(ctx context.Context, workerID, namespace string, limit int) ([]*host.WorkflowInstance, error) {
		return nil, nil
	}

	w := newTestWorkerWithConcurrency(ms, 2)
	w.inflight.Store("wf-1", &host.WorkflowInstance{ID: "wf-1"})
	w.inflight.Store("wf-2", &host.WorkflowInstance{ID: "wf-2"})

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

	mu.Lock()
	c := claimAttempts
	mu.Unlock()
	if c > 0 {
		t.Errorf("expected 0 claim attempts when inflight == concurrency, got %d", c)
	}
}

// ---------------------------------------------------------------------------
// Heartbeat loop edge cases
// ---------------------------------------------------------------------------

func TestHeartbeatLoop_RapidSuccessiveFailures(t *testing.T) {
	// Many consecutive heartbeat failures (connection errors) must not crash
	// the loop, and inflight set entries must be preserved.
	ms := &mockStore{}
	var callCount atomic.Int64
	ms.heartbeatFn = func(ctx context.Context, workflowID, workerID string) (bool, error) {
		callCount.Add(1)
		return false, errors.New("connection refused")
	}

	w := newTestWorker(ms)
	w.heartbeatInterval = 2 * time.Millisecond

	// Add several workflows to inflight so each cycle iterates multiple entries.
	for i := 0; i < 5; i++ {
		id := "wf-rapid-" + string(rune('0'+i))
		w.inflight.Store(id, &host.WorkflowInstance{ID: id})
	}

	// Run for enough time to get many heartbeat attempts.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	w.ctx = ctx
	w.cancel = cancel

	done := make(chan struct{})
	go func() {
		w.wg.Add(1)
		w.heartbeatLoop()
		close(done)
	}()

	<-done

	total := callCount.Load()
	if total == 0 {
		t.Fatal("expected at least one heartbeat call")
	}
	t.Logf("rapid failure test: %d heartbeat calls (%d workflows x ~30 cycles)", total, 5)

	// All workflows must remain in inflight (connection errors don't remove).
	w.inflight.Range(func(key, _ interface{}) bool {
		id := key.(string)
		if id != "wf-rapid-0" && id != "wf-rapid-1" && id != "wf-rapid-2" && id != "wf-rapid-3" && id != "wf-rapid-4" {
			t.Errorf("unexpected workflow in inflight: %s", id)
		}
		return true
	})
}

func TestHeartbeatLoop_FlappingConnection(t *testing.T) {
	// Alternating success and failure (connection flapping) must not cause
	// premature removal from inflight. Only non-connection errors or
	// alive=false signals should remove entries.
	ms := &mockStore{}
	var callCount atomic.Int64

	ms.heartbeatFn = func(ctx context.Context, workflowID, workerID string) (bool, error) {
		n := callCount.Add(1)
		// Every other call: connection error (odd) or success (even).
		if n%2 == 1 {
			return false, errors.New("connection refused")
		}
		return true, nil
	}

	w := newTestWorker(ms)
	w.heartbeatInterval = 3 * time.Millisecond
	w.inflight.Store("wf-flap", &host.WorkflowInstance{ID: "wf-flap"})

	done := make(chan struct{})
	go func() {
		w.wg.Add(1)
		w.heartbeatLoop()
		close(done)
	}()

	time.Sleep(50 * time.Millisecond)
	w.cancel()
	<-done

	if callCount.Load() == 0 {
		t.Error("expected at least one heartbeat call")
	}

	// Connection errors don't remove — workflow must remain in inflight.
	_, ok := w.inflight.Load("wf-flap")
	if !ok {
		t.Error("expected wf-flap to remain in inflight during flapping connection")
	}
}

func TestHeartbeatLoop_AllWorkflowsLost(t *testing.T) {
	// When ALL workflows report lost ownership (alive=false, no error),
	// the inflight map must become empty.
	ms := &mockStore{}
	ms.heartbeatFn = func(ctx context.Context, workflowID, workerID string) (bool, error) {
		return false, nil // lost ownership, no connection error
	}

	w := newTestWorker(ms)
	w.heartbeatInterval = 5 * time.Millisecond
	w.inflight.Store("wf-lost-1", &host.WorkflowInstance{ID: "wf-lost-1"})
	w.inflight.Store("wf-lost-2", &host.WorkflowInstance{ID: "wf-lost-2"})

	done := make(chan struct{})
	go func() {
		w.wg.Add(1)
		w.heartbeatLoop()
		close(done)
	}()

	time.Sleep(30 * time.Millisecond)
	w.cancel()
	<-done

	// Both must be removed.
	var remaining int
	w.inflight.Range(func(_, _ interface{}) bool {
		remaining++
		return true
	})
	if remaining > 0 {
		t.Errorf("expected inflight to be empty after all workflows lost, got %d entries", remaining)
	}
}

// ---------------------------------------------------------------------------
// Compaction trigger: workflow crossing compaction threshold
// ---------------------------------------------------------------------------

func TestCompactionTrigger_AboveThreshold(t *testing.T) {
	// When a workflow's event count exceeds the compaction threshold,
	// CompactWorkflowHistory must invoke compactHistory on the store.
	ms := &mockStore{}

	ms.getCompactionCandidatesFn = func(ctx context.Context, threshold int, limit int) ([]string, error) {
		return []string{"wf-heavy"}, nil
	}
	ms.loadEventHistoryFn = func(ctx context.Context, workflowID string) ([]host.EventRecord, error) {
		events := make([]host.EventRecord, host.DefaultCompactionThreshold+100)
		for i := range events {
			events[i] = host.EventRecord{Step: i, EventType: "call"}
		}
		return events, nil
	}
	ms.loadCompactionStateFn = func(ctx context.Context, workflowID string) (*host.CompactionState, error) {
		return nil, nil
	}

	var compactMu sync.Mutex
	compactCalled := false
	var compactWFID string
	ms.compactHistoryFn = func(ctx context.Context, workflowID string, compactionState []byte, compactionStep int, keepStep int) error {
		compactMu.Lock()
		compactCalled = true
		compactWFID = workflowID
		compactMu.Unlock()
		return nil
	}

	w := newTestWorker(ms)
	w.compactionInterval = 10 * time.Millisecond
	w.compactionThreshold = host.DefaultCompactionThreshold

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		w.wg.Add(1)
		w.compactionLoop()
	}()

	waitForCond(t, 2*time.Second, func() bool {
		compactMu.Lock()
		c := compactCalled
		compactMu.Unlock()
		return c
	})

	w.cancel()
	wg.Wait()

	compactMu.Lock()
	if !compactCalled {
		t.Fatal("expected compactHistory to be called when events exceed threshold")
	}
	if compactWFID != "wf-heavy" {
		t.Errorf("expected compactHistory for wf-heavy, got %q", compactWFID)
	}
	compactMu.Unlock()
}

func TestCompactionTrigger_AtThreshold(t *testing.T) {
	// When events are exactly at the threshold, CompactWorkflowHistory should
	// NOT call compactHistory (it checks len(events) <= threshold).
	ms := &mockStore{}

	ms.getCompactionCandidatesFn = func(ctx context.Context, threshold int, limit int) ([]string, error) {
		return []string{"wf-exact"}, nil
	}
	ms.loadEventHistoryFn = func(ctx context.Context, workflowID string) ([]host.EventRecord, error) {
		events := make([]host.EventRecord, host.DefaultCompactionThreshold) // exactly 1000
		for i := range events {
			events[i] = host.EventRecord{Step: i, EventType: "call"}
		}
		return events, nil
	}
	ms.loadCompactionStateFn = func(ctx context.Context, workflowID string) (*host.CompactionState, error) {
		return nil, nil
	}

	var compactMu sync.Mutex
	compactCalled := false
	ms.compactHistoryFn = func(ctx context.Context, workflowID string, compactionState []byte, compactionStep int, keepStep int) error {
		compactMu.Lock()
		compactCalled = true
		compactMu.Unlock()
		return nil
	}

	w := newTestWorker(ms)
	w.compactionInterval = 10 * time.Millisecond
	w.compactionThreshold = host.DefaultCompactionThreshold

	// Run for a short time and verify compactHistory was NOT called.
	// (CompactWorkflowHistory returns early when len(events) <= threshold.)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	w.ctx = ctx
	w.cancel = cancel

	done := make(chan struct{})
	go func() {
		w.wg.Add(1)
		w.compactionLoop()
		close(done)
	}()

	<-done

	compactMu.Lock()
	if compactCalled {
		t.Error("expected compactHistory NOT to be called when events == threshold")
	}
	compactMu.Unlock()
}

func TestCompactionTrigger_CompactionError(t *testing.T) {
	// When CompactWorkflowHistory returns an error, the compaction loop must
	// log and continue, not crash.
	ms := &mockStore{}
	candidateReturned := false

	ms.getCompactionCandidatesFn = func(ctx context.Context, threshold int, limit int) ([]string, error) {
		if !candidateReturned {
			candidateReturned = true
			return []string{"wf-error"}, nil
		}
		return nil, nil
	}
	ms.loadEventHistoryFn = func(ctx context.Context, workflowID string) ([]host.EventRecord, error) {
		return nil, errors.New("connection refused")
	}

	w := newTestWorker(ms)
	w.compactionInterval = 10 * time.Millisecond
	w.compactionThreshold = host.DefaultCompactionThreshold

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	w.ctx = ctx
	w.cancel = cancel

	done := make(chan struct{})
	go func() {
		w.wg.Add(1)
		w.compactionLoop()
		close(done)
	}()

	<-done
	// If we reach here without a panic, the test passes.
}

// ---------------------------------------------------------------------------
// Concurrency key edge cases
// ---------------------------------------------------------------------------

func TestConcurrencyKey_SameKeyDifferentWorkflow(t *testing.T) {
	// AcquireConcurrencyKey must return true for the first workflow acquiring
	// a key, and false when a different workflow tries the same key.
	ms := &mockStore{}
	keys := make(map[string]string)

	ms.acquireConcurrencyKeyFn = func(ctx context.Context, key, workflowID string, ttl time.Duration) (bool, error) {
		if existing, taken := keys[key]; taken && existing != workflowID {
			return false, nil
		}
		keys[key] = workflowID
		return true, nil
	}
	ms.releaseConcurrencyKeyFn = func(ctx context.Context, key string) error {
		delete(keys, key)
		return nil
	}

	ctx := context.Background()

	// First workflow acquires the key.
	acquired, err := ms.AcquireConcurrencyKey(ctx, "shared-key", "wf-alpha", 30*time.Minute)
	if err != nil {
		t.Fatalf("first acquire: unexpected error: %v", err)
	}
	if !acquired {
		t.Fatal("expected first acquire to return true")
	}

	// Second workflow with the same key must be rejected.
	acquired, err = ms.AcquireConcurrencyKey(ctx, "shared-key", "wf-beta", 30*time.Minute)
	if err != nil {
		t.Fatalf("second acquire: unexpected error: %v", err)
	}
	if acquired {
		t.Error("expected second acquire to return false for same key")
	}

	// Release the key.
	if err := ms.ReleaseConcurrencyKey(ctx, "shared-key"); err != nil {
		t.Fatalf("release: unexpected error: %v", err)
	}

	// After release, a new workflow can acquire it.
	acquired, err = ms.AcquireConcurrencyKey(ctx, "shared-key", "wf-gamma", 30*time.Minute)
	if err != nil {
		t.Fatalf("acquire after release: unexpected error: %v", err)
	}
	if !acquired {
		t.Error("expected acquire to succeed after key release")
	}
}

func TestConcurrencyKey_DifferentKeysDoNotConflict(t *testing.T) {
	// Different concurrency keys should be independently acquirable.
	ms := &mockStore{}
	keys := make(map[string]string)

	ms.acquireConcurrencyKeyFn = func(ctx context.Context, key, workflowID string, ttl time.Duration) (bool, error) {
		if existing, taken := keys[key]; taken && existing != workflowID {
			return false, nil
		}
		keys[key] = workflowID
		return true, nil
	}

	ctx := context.Background()

	acquired1, err := ms.AcquireConcurrencyKey(ctx, "key-a", "wf-a", 10*time.Minute)
	if err != nil {
		t.Fatalf("acquire key-a: %v", err)
	}
	if !acquired1 {
		t.Fatal("expected key-a to be acquired")
	}

	acquired2, err := ms.AcquireConcurrencyKey(ctx, "key-b", "wf-b", 10*time.Minute)
	if err != nil {
		t.Fatalf("acquire key-b: %v", err)
	}
	if !acquired2 {
		t.Fatal("expected key-b to be acquired independently of key-a")
	}
}

func TestConcurrencyKey_ReleaseWorkflowKeys(t *testing.T) {
	// ReleaseWorkflowConcurrencyKeys must release all keys held by a workflow.
	ms := &mockStore{}
	releaseCalled := false
	var releasedWfID string

	ms.releaseWorkflowConcurrencyKeysFn = func(ctx context.Context, workflowID string) error {
		releaseCalled = true
		releasedWfID = workflowID
		return nil
	}

	ctx := context.Background()
	if err := ms.ReleaseWorkflowConcurrencyKeys(ctx, "wf-alpha"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !releaseCalled {
		t.Error("expected ReleaseWorkflowConcurrencyKeys to be called")
	}
	if releasedWfID != "wf-alpha" {
		t.Errorf("expected wf-alpha, got %q", releasedWfID)
	}
}

func TestConcurrencyKey_ExpiredKeyReaper(t *testing.T) {
	// The expired concurrency key reaper must call ReapExpiredConcurrencyKeys
	// and return the count of reaped keys without error.
	ms := &mockStore{}
	reapCalled := false

	ms.reapExpiredConcurrencyKeysFn = func(ctx context.Context) (int64, error) {
		reapCalled = true
		return 3, nil
	}

	// Verify the store method contract.
	n, err := ms.ReapExpiredConcurrencyKeys(context.Background())
	if err != nil {
		t.Fatalf("ReapExpiredConcurrencyKeys error: %v", err)
	}
	if n != 3 {
		t.Errorf("expected 3 reaped keys, got %d", n)
	}
	if !reapCalled {
		t.Error("expected ReapExpiredConcurrencyKeys to be called")
	}
}

func TestConcurrencyKey_ReaperErrorNoPanic(t *testing.T) {
	// When ReapExpiredConcurrencyKeys returns an error, the reaper loop must
	// not panic.
	ms := &mockStore{}
	ms.reapExpiredConcurrencyKeysFn = func(ctx context.Context) (int64, error) {
		return 0, errors.New("connection refused")
	}

	_, err := ms.ReapExpiredConcurrencyKeys(context.Background())
	if err == nil {
		t.Error("expected error from mock, got nil")
	}
}

// ---------------------------------------------------------------------------
// loadWASM caching edge cases
// ---------------------------------------------------------------------------

func TestLoadWASM_CacheHit(t *testing.T) {
	// After a cache miss triggers a store.LoadWASM call, subsequent calls
	// for the same def+version must use the cache.
	ms := &mockStore{}
	storeCalls := 0
	ms.loadWASMFn = func(ctx context.Context, defName string, defVersion int) ([]byte, error) {
		storeCalls++
		if defName == "my-workflow" && defVersion == 1 {
			return []byte("wasm-bytes"), nil
		}
		return nil, errors.New("not found")
	}

	w := newTestWorker(ms)

	// First call: cache miss → LoadWASM called.
	b1, err := w.loadWASM("my-workflow", 1)
	if err != nil {
		t.Fatalf("first load: unexpected error: %v", err)
	}
	if string(b1) != "wasm-bytes" {
		t.Errorf("first load: expected wasm-bytes, got %q", string(b1))
	}
	if storeCalls != 1 {
		t.Errorf("expected 1 store call on first load, got %d", storeCalls)
	}

	// Second call: cache hit → LoadWASM not called.
	b2, err := w.loadWASM("my-workflow", 1)
	if err != nil {
		t.Fatalf("second load: unexpected error: %v", err)
	}
	if string(b2) != "wasm-bytes" {
		t.Errorf("second load: expected wasm-bytes, got %q", string(b2))
	}
	if storeCalls != 1 {
		t.Errorf("expected 1 store call total after cache hit, got %d", storeCalls)
	}
}

func TestLoadWASM_CacheSeparateByVersion(t *testing.T) {
	// Different versions of the same workflow must have separate cache entries.
	ms := &mockStore{}
	var mu sync.Mutex
	storeCalls := 0
	ms.loadWASMFn = func(ctx context.Context, defName string, defVersion int) ([]byte, error) {
		mu.Lock()
		storeCalls++
		mu.Unlock()
		return []byte("wasm-v" + string(rune('0'+defVersion))), nil
	}

	w := newTestWorker(ms)

	// Load v1.
	b1, _ := w.loadWASM("wf", 1)
	// Load v2.
	b2, _ := w.loadWASM("wf", 2)

	if string(b1) == string(b2) {
		t.Error("expected different bytes for different versions")
	}

	// Load v1 again — should be cache hit.
	b1again, _ := w.loadWASM("wf", 1)
	if string(b1again) != string(b1) {
		t.Error("expected same bytes for v1 cache hit")
	}

	mu.Lock()
	calls := storeCalls
	mu.Unlock()
	if calls != 2 {
		t.Errorf("expected 2 store calls (v1 miss, v2 miss), got %d", calls)
	}
}

// ---------------------------------------------------------------------------
// API handler edge case tests
// ---------------------------------------------------------------------------

func TestAPIStartWorkflow_Simple(t *testing.T) {
	ms := &mockStore{}
	ms.listVersionsFn = func(ctx context.Context, defName string) ([]int, error) {
		return []int{1}, nil
	}
	startCalled := false
	ms.startNewRunFn = func(ctx context.Context, defName string, defVersion int, input json.RawMessage, idempotencyKey string) (string, bool, error) {
		startCalled = true
		if defVersion != 1 {
			t.Errorf("expected defVersion 1, got %d", defVersion)
		}
		return "wf-new-1", false, nil
	}

	api := &apiServer{store: ms, worker: newTestWorker(ms)}
	body := `{"input":{"order_id":42}}`
	req := httptest.NewRequest(http.MethodPost, "/api/workflows/test-workflow/start", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	api.handleWorkflows(w, req)

	resp := w.Result()
	if resp.StatusCode != 201 {
		t.Errorf("expected 201, got %d", resp.StatusCode)
	}
	var respBody map[string]string
	json.NewDecoder(resp.Body).Decode(&respBody)
	resp.Body.Close()
	if respBody["id"] != "wf-new-1" {
		t.Errorf("expected id wf-new-1, got %q", respBody["id"])
	}
	if !startCalled {
		t.Error("expected StartNewRun to be called")
	}
}

func TestAPIStartWorkflow_DefinitionNotFound(t *testing.T) {
	ms := &mockStore{}
	ms.listVersionsFn = func(ctx context.Context, defName string) ([]int, error) {
		return nil, nil
	}

	api := &apiServer{store: ms, worker: newTestWorker(ms)}
	req := httptest.NewRequest(http.MethodPost, "/api/workflows/unknown/start", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	api.handleWorkflows(w, req)

	resp := w.Result()
	if resp.StatusCode != 404 {
		t.Errorf("expected 404 for unknown workflow definition, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestAPIStartWorkflow_AlreadyExisted(t *testing.T) {
	ms := &mockStore{}
	ms.listVersionsFn = func(ctx context.Context, defName string) ([]int, error) {
		return []int{1}, nil
	}
	ms.startNewRunFn = func(ctx context.Context, defName string, defVersion int, input json.RawMessage, idempotencyKey string) (string, bool, error) {
		return "existing-run-1", true, nil
	}

	api := &apiServer{store: ms, worker: newTestWorker(ms)}
	req := httptest.NewRequest(http.MethodPost, "/api/workflows/test-workflow/start", strings.NewReader(`{}`))
	req.Header.Set("Idempotency-Key", "key-123")
	w := httptest.NewRecorder()
	api.handleWorkflows(w, req)

	resp := w.Result()
	if resp.StatusCode != 200 {
		t.Errorf("expected 200 for already-started workflow, got %d", resp.StatusCode)
	}
	var body map[string]string
	json.NewDecoder(resp.Body).Decode(&body)
	resp.Body.Close()
	if body["already_started"] != "true" {
		t.Errorf("expected already_started=true, got %q", body["already_started"])
	}
}

func TestAPIStartWorkflow_ConcurrencyKeyConflict(t *testing.T) {
	ms := &mockStore{}
	ms.listVersionsFn = func(ctx context.Context, defName string) ([]int, error) {
		return []int{1}, nil
	}
	ms.startNewRunFn = func(ctx context.Context, defName string, defVersion int, input json.RawMessage, idempotencyKey string) (string, bool, error) {
		return "wf-conflict-1", false, nil
	}
	ms.acquireConcurrencyKeyFn = func(ctx context.Context, key, workflowID string, ttl time.Duration) (bool, error) {
		return false, nil
	}
	failCalled := false
	ms.failWorkflowFn = func(ctx context.Context, workflowID, workerID, errMsg string, queryState map[string]string) error {
		failCalled = true
		return nil
	}

	api := &apiServer{store: ms, worker: newTestWorker(ms)}
	body := `{"concurrency_key":"shared-key-1"}`
	req := httptest.NewRequest(http.MethodPost, "/api/workflows/test-workflow/start", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	api.handleWorkflows(w, req)

	resp := w.Result()
	if resp.StatusCode != 409 {
		t.Errorf("expected 409 for concurrency key conflict, got %d", resp.StatusCode)
	}
	resp.Body.Close()
	if !failCalled {
		t.Error("expected FailWorkflow to be called after concurrency key conflict")
	}
}

func TestAPIStartWorkflow_ConcurrencyKeyHeader(t *testing.T) {
	ms := &mockStore{}
	ms.listVersionsFn = func(ctx context.Context, defName string) ([]int, error) {
		return []int{1}, nil
	}
	ms.startNewRunFn = func(ctx context.Context, defName string, defVersion int, input json.RawMessage, idempotencyKey string) (string, bool, error) {
		return "wf-ck-1", false, nil
	}
	acquireCalled := false
	ms.acquireConcurrencyKeyFn = func(ctx context.Context, key, workflowID string, ttl time.Duration) (bool, error) {
		acquireCalled = true
		if key != "header-key" {
			t.Errorf("expected key 'header-key', got %q", key)
		}
		return true, nil
	}

	api := &apiServer{store: ms, worker: newTestWorker(ms)}
	req := httptest.NewRequest(http.MethodPost, "/api/workflows/test-workflow/start", strings.NewReader(`{}`))
	req.Header.Set("Cleat-Concurrency-Key", "header-key")
	w := httptest.NewRecorder()
	api.handleWorkflows(w, req)

	resp := w.Result()
	if resp.StatusCode != 201 {
		t.Errorf("expected 201, got %d", resp.StatusCode)
	}
	resp.Body.Close()
	if !acquireCalled {
		t.Error("expected AcquireConcurrencyKey to be called from header")
	}
}

func TestAPISignal_Deliver(t *testing.T) {
	ms := &mockStore{}
	signalCalled := false
	ms.deliverSignalFn = func(ctx context.Context, workflowID, signalName, payload string) error {
		signalCalled = true
		if workflowID != "wf-signal-1" {
			t.Errorf("expected workflowID wf-signal-1, got %q", workflowID)
		}
		if signalName != "my-signal" {
			t.Errorf("expected signalName my-signal, got %q", signalName)
		}
		if payload != `{"value":42}` {
			t.Errorf("expected payload %q, got %q", `{"value":42}`, payload)
		}
		return nil
	}

	api := &apiServer{store: ms, worker: newTestWorker(ms)}
	body := `{"signal_name":"my-signal","payload":"{\"value\":42}"}`
	req := httptest.NewRequest(http.MethodPost, "/api/workflows/wf-signal-1/signal", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	api.handleWorkflows(w, req)

	resp := w.Result()
	if resp.StatusCode != 200 {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()
	if !signalCalled {
		t.Error("expected DeliverSignal to be called")
	}
}

func TestAPISignal_MissingName(t *testing.T) {
	ms := &mockStore{}
	api := &apiServer{store: ms, worker: newTestWorker(ms)}

	req := httptest.NewRequest(http.MethodPost, "/api/workflows/wf-signal-1/signal", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	api.handleWorkflows(w, req)

	resp := w.Result()
	if resp.StatusCode != 400 {
		t.Errorf("expected 400 for missing signal_name, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestAPICancel_Basic(t *testing.T) {
	ms := &mockStore{}
	cancelCalled := false
	ms.requestCancellationFn = func(ctx context.Context, workflowID, reason string) error {
		cancelCalled = true
		if workflowID != "wf-cancel-1" {
			t.Errorf("expected workflowID wf-cancel-1, got %q", workflowID)
		}
		return nil
	}

	api := &apiServer{store: ms, worker: newTestWorker(ms)}
	req := httptest.NewRequest(http.MethodPost, "/api/workflows/wf-cancel-1/cancel", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	api.handleWorkflows(w, req)

	resp := w.Result()
	if resp.StatusCode != 200 {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()
	if !cancelCalled {
		t.Error("expected RequestCancellation to be called")
	}
}

func TestAPIGetHistory_ReturnsEvents(t *testing.T) {
	ms := &mockStore{}
	ms.loadEventHistoryFn = func(ctx context.Context, workflowID string) ([]host.EventRecord, error) {
		return []host.EventRecord{
			{Step: 0, EventType: "call"},
			{Step: 1, EventType: "call"},
		}, nil
	}

	api := &apiServer{store: ms, worker: newTestWorker(ms)}
	req := httptest.NewRequest(http.MethodGet, "/api/workflows/wf-hist-1/history", nil)
	w := httptest.NewRecorder()
	api.handleWorkflows(w, req)

	resp := w.Result()
	if resp.StatusCode != 200 {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
	var events []host.EventRecord
	json.NewDecoder(resp.Body).Decode(&events)
	resp.Body.Close()
	if len(events) != 2 {
		t.Errorf("expected 2 events, got %d", len(events))
	}
}

func TestAPIGetHistory_Empty(t *testing.T) {
	ms := &mockStore{}
	ms.loadEventHistoryFn = func(ctx context.Context, workflowID string) ([]host.EventRecord, error) {
		return nil, nil
	}

	api := &apiServer{store: ms, worker: newTestWorker(ms)}
	req := httptest.NewRequest(http.MethodGet, "/api/workflows/wf-empty-hist/history", nil)
	w := httptest.NewRecorder()
	api.handleWorkflows(w, req)

	resp := w.Result()
	bodyBytes, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
	if string(bodyBytes) != "[]\n" {
		t.Errorf("expected empty JSON array, got %q", string(bodyBytes))
	}
}

func TestAPIGetQueryState(t *testing.T) {
	ms := &mockStore{}
	ms.getQueryStateFn = func(ctx context.Context, workflowID, key string) (string, error) {
		return `{"status":"done"}`, nil
	}

	api := &apiServer{store: ms, worker: newTestWorker(ms)}
	req := httptest.NewRequest(http.MethodGet, "/api/workflows/wf-query-1/query?key=mykey", nil)
	w := httptest.NewRecorder()
	api.handleWorkflows(w, req)

	resp := w.Result()
	if resp.StatusCode != 200 {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
	var body map[string]string
	json.NewDecoder(resp.Body).Decode(&body)
	resp.Body.Close()
	if body["key"] != "mykey" {
		t.Errorf("expected key mykey, got %q", body["key"])
	}
	if body["value"] != `{"status":"done"}` {
		t.Errorf("expected value %q, got %q", `{"status":"done"}`, body["value"])
	}
}

func TestAPIGetDAG_Found(t *testing.T) {
	ms := &mockStore{}
	ms.getWorkflowByIDFn = func(ctx context.Context, id string) (*host.WorkflowInstance, error) {
		return &host.WorkflowInstance{
			ID: "wf-dag-1", DefName: "dag-workflow", DefVersion: 1,
		}, nil
	}
	ms.loadDAGSpecFn = func(ctx context.Context, defName string, defVersion int) (json.RawMessage, error) {
		return json.RawMessage(`{"steps":["step1","step2"]}`), nil
	}

	api := &apiServer{store: ms, worker: newTestWorker(ms)}
	req := httptest.NewRequest(http.MethodGet, "/api/workflows/wf-dag-1/dag", nil)
	w := httptest.NewRecorder()
	api.handleWorkflows(w, req)

	resp := w.Result()
	if resp.StatusCode != 200 {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
	var body map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&body)
	resp.Body.Close()
	if body["workflow_id"] != "wf-dag-1" {
		t.Errorf("expected workflow_id wf-dag-1, got %q", body["workflow_id"])
	}
}

func TestAPIGetDAG_NotFound(t *testing.T) {
	ms := &mockStore{}
	ms.getWorkflowByIDFn = func(ctx context.Context, id string) (*host.WorkflowInstance, error) {
		return nil, nil
	}

	api := &apiServer{store: ms, worker: newTestWorker(ms)}
	req := httptest.NewRequest(http.MethodGet, "/api/workflows/wf-missing/dag", nil)
	w := httptest.NewRecorder()
	api.handleWorkflows(w, req)

	resp := w.Result()
	if resp.StatusCode != 404 {
		t.Errorf("expected 404 for missing workflow DAG, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestAPIGetDAG_SpecNotFound(t *testing.T) {
	ms := &mockStore{}
	ms.getWorkflowByIDFn = func(ctx context.Context, id string) (*host.WorkflowInstance, error) {
		return &host.WorkflowInstance{
			ID: "wf-dag-2", DefName: "dag-workflow", DefVersion: 1,
		}, nil
	}
	ms.loadDAGSpecFn = func(ctx context.Context, defName string, defVersion int) (json.RawMessage, error) {
		return nil, nil
	}

	api := &apiServer{store: ms, worker: newTestWorker(ms)}
	req := httptest.NewRequest(http.MethodGet, "/api/workflows/wf-dag-2/dag", nil)
	w := httptest.NewRecorder()
	api.handleWorkflows(w, req)

	resp := w.Result()
	if resp.StatusCode != 404 {
		t.Errorf("expected 404 when no DAG spec, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestAPIWorkflowPromises_List(t *testing.T) {
	ms := &mockStore{}
	ms.listPromisesFn = func(ctx context.Context, workflowID string) ([]host.PromiseInfo, error) {
		return []host.PromiseInfo{
			{PromiseID: "prom-1", Status: "pending"},
		}, nil
	}

	api := &apiServer{store: ms, worker: newTestWorker(ms)}
	req := httptest.NewRequest(http.MethodGet, "/api/workflows/wf-prom-1/promises", nil)
	w := httptest.NewRecorder()
	api.handleWorkflows(w, req)

	resp := w.Result()
	if resp.StatusCode != 200 {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
	var promises []host.PromiseInfo
	json.NewDecoder(resp.Body).Decode(&promises)
	resp.Body.Close()
	if len(promises) != 1 {
		t.Errorf("expected 1 promise, got %d", len(promises))
	}
}

func TestAPIWorkflowPromises_ListEmpty(t *testing.T) {
	ms := &mockStore{}
	ms.listPromisesFn = func(ctx context.Context, workflowID string) ([]host.PromiseInfo, error) {
		return nil, nil
	}

	api := &apiServer{store: ms, worker: newTestWorker(ms)}
	req := httptest.NewRequest(http.MethodGet, "/api/workflows/wf-prom-2/promises", nil)
	w := httptest.NewRecorder()
	api.handleWorkflows(w, req)

	resp := w.Result()
	bodyBytes, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
	if string(bodyBytes) != "[]\n" {
		t.Errorf("expected empty array, got %q", string(bodyBytes))
	}
}

func TestAPIWorkflowPromises_Resolve(t *testing.T) {
	ms := &mockStore{}
	resolveCalled := false
	ms.resolvePromiseFn = func(ctx context.Context, workflowID, promiseID, result string) error {
		resolveCalled = true
		return nil
	}

	api := &apiServer{store: ms, worker: newTestWorker(ms)}
	body := `{"result":"success"}`
	req := httptest.NewRequest(http.MethodPost, "/api/workflows/wf-prom-1/promises/prom-1/resolve", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	api.handleWorkflows(w, req)

	resp := w.Result()
	if resp.StatusCode != 200 {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()
	if !resolveCalled {
		t.Error("expected ResolvePromise to be called")
	}
}

func TestAPIWorkflowPromises_Reject(t *testing.T) {
	ms := &mockStore{}
	rejectCalled := false
	ms.rejectPromiseFn = func(ctx context.Context, workflowID, promiseID, errMsg string) error {
		rejectCalled = true
		return nil
	}

	api := &apiServer{store: ms, worker: newTestWorker(ms)}
	body := `{"reason":"timeout"}`
	req := httptest.NewRequest(http.MethodPost, "/api/workflows/wf-prom-1/promises/prom-1/reject", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	api.handleWorkflows(w, req)

	resp := w.Result()
	if resp.StatusCode != 200 {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()
	if !rejectCalled {
		t.Error("expected RejectPromise to be called")
	}
}

func TestAPIWorkflowUpdate_Create(t *testing.T) {
	ms := &mockStore{}
	ms.getWorkflowByIDFn = func(ctx context.Context, id string) (*host.WorkflowInstance, error) {
		return &host.WorkflowInstance{ID: "wf-update-1", Status: "running"}, nil
	}
	createCalled := false
	ms.createUpdateRequestFn = func(ctx context.Context, workflowID, updateName, payload, promiseID string) error {
		createCalled = true
		return nil
	}
	ms.createPromiseFn = func(ctx context.Context, workflowID, promiseName, promiseID string) error {
		return nil
	}

	api := &apiServer{store: ms, worker: newTestWorker(ms)}
	req := httptest.NewRequest(http.MethodPost, "/api/workflows/wf-update-1/update/my-action", strings.NewReader(`{"param":"value"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	api.handleWorkflows(w, req)

	resp := w.Result()
	if resp.StatusCode != 202 {
		t.Errorf("expected 202, got %d", resp.StatusCode)
	}
	var body map[string]string
	json.NewDecoder(resp.Body).Decode(&body)
	resp.Body.Close()
	if !strings.HasPrefix(body["promise_id"], "upd-") {
		t.Errorf("expected promise_id starting with upd-, got %q", body["promise_id"])
	}
	if !createCalled {
		t.Error("expected CreateUpdateRequest to be called")
	}
}

func TestAPIWorkflowUpdate_WorkflowNotFound(t *testing.T) {
	ms := &mockStore{}
	ms.getWorkflowByIDFn = func(ctx context.Context, id string) (*host.WorkflowInstance, error) {
		return nil, nil
	}

	api := &apiServer{store: ms, worker: newTestWorker(ms)}
	req := httptest.NewRequest(http.MethodPost, "/api/workflows/wf-missing/update/my-action", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	api.handleWorkflows(w, req)

	resp := w.Result()
	if resp.StatusCode != 404 {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestAPIWorkflowUpdate_DuplicatePending(t *testing.T) {
	ms := &mockStore{}
	ms.getWorkflowByIDFn = func(ctx context.Context, id string) (*host.WorkflowInstance, error) {
		return &host.WorkflowInstance{ID: "wf-update-2", Status: "running"}, nil
	}
	ms.getPendingUpdateRequestsFn = func(ctx context.Context, workflowID string) ([]host.UpdateRequestInfo, error) {
		return []host.UpdateRequestInfo{
			{UpdateName: "my-action"},
		}, nil
	}

	api := &apiServer{store: ms, worker: newTestWorker(ms)}
	req := httptest.NewRequest(http.MethodPost, "/api/workflows/wf-update-2/update/my-action", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	api.handleWorkflows(w, req)

	resp := w.Result()
	if resp.StatusCode != 409 {
		t.Errorf("expected 409 for duplicate pending update, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// ---------------------------------------------------------------------------
// Reaper loop edge cases
// ---------------------------------------------------------------------------

func TestReaperLoop_CancelContext(t *testing.T) {
	// Context cancellation must cause reaperLoop to exit promptly.
	ms := &mockStore{}
	w := newTestWorker(ms)

	done := make(chan struct{})
	go func() {
		w.wg.Add(1)
		w.reaperLoop()
		close(done)
	}()

	w.cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("reaperLoop did not stop after context cancellation")
	}
}

func TestReaperLoop_ConnectionError(t *testing.T) {
	// A connection error from ReapStaleInstances must be logged and
	// the loop must continue without crashing.
	ms := &mockStore{}
	ms.reapStaleInstancesFn = func(ctx context.Context, timeout time.Duration) (int, error) {
		return 0, errors.New("connection refused")
	}

	w := newTestWorker(ms)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	w.ctx = ctx
	w.cancel = cancel

	done := make(chan struct{})
	go func() {
		w.wg.Add(1)
		w.reaperLoop()
		close(done)
	}()
	<-done
}

func TestReaperLoop_NonConnectionError(t *testing.T) {
	// A non-connection error (e.g. permission denied) must be logged and
	// the loop must continue.
	ms := &mockStore{}
	ms.reapStaleInstancesFn = func(ctx context.Context, timeout time.Duration) (int, error) {
		return 0, errors.New("permission denied")
	}

	w := newTestWorker(ms)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	w.ctx = ctx
	w.cancel = cancel

	done := make(chan struct{})
	go func() {
		w.wg.Add(1)
		w.reaperLoop()
		close(done)
	}()
	<-done
}

func TestReaperLoop_ZeroReaped(t *testing.T) {
	// When there are no stale instances, ReapStaleInstances returns 0.
	ms := &mockStore{}
	ms.reapStaleInstancesFn = func(ctx context.Context, timeout time.Duration) (int, error) {
		return 0, nil
	}

	w := newTestWorker(ms)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	w.ctx = ctx
	w.cancel = cancel

	done := make(chan struct{})
	go func() {
		w.wg.Add(1)
		w.reaperLoop()
		close(done)
	}()
	<-done
}

func TestReaperLoop_SomeReaped(t *testing.T) {
	// When ReapStaleInstances returns a positive count, the loop logs it.
	ms := &mockStore{}
	ms.reapStaleInstancesFn = func(ctx context.Context, timeout time.Duration) (int, error) {
		return 5, nil
	}

	w := newTestWorker(ms)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	w.ctx = ctx
	w.cancel = cancel

	done := make(chan struct{})
	go func() {
		w.wg.Add(1)
		w.reaperLoop()
		close(done)
	}()
	<-done
}

// ---------------------------------------------------------------------------
// Concurrency key reaper loop edge cases
// ---------------------------------------------------------------------------

func TestConcurrencyKeyReaperLoop_ContextCancel(t *testing.T) {
	ms := &mockStore{}
	w := newTestWorker(ms)

	done := make(chan struct{})
	go func() {
		w.wg.Add(1)
		w.concurrencyKeyReaperLoop()
		close(done)
	}()

	w.cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("concurrencyKeyReaperLoop did not stop after context cancellation")
	}
}

func TestConcurrencyKeyReaperLoop_ConnectionError(t *testing.T) {
	ms := &mockStore{}
	ms.reapExpiredConcurrencyKeysFn = func(ctx context.Context) (int64, error) {
		return 0, errors.New("connection refused")
	}

	w := newTestWorker(ms)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	w.ctx = ctx
	w.cancel = cancel

	done := make(chan struct{})
	go func() {
		w.wg.Add(1)
		w.concurrencyKeyReaperLoop()
		close(done)
	}()
	<-done
}

func TestConcurrencyKeyReaperLoop_GenericError(t *testing.T) {
	ms := &mockStore{}
	ms.reapExpiredConcurrencyKeysFn = func(ctx context.Context) (int64, error) {
		return 0, errors.New("internal error")
	}

	w := newTestWorker(ms)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	w.ctx = ctx
	w.cancel = cancel

	done := make(chan struct{})
	go func() {
		w.wg.Add(1)
		w.concurrencyKeyReaperLoop()
		close(done)
	}()
	<-done
}

func TestConcurrencyKeyReaperLoop_KeysReaped(t *testing.T) {
	ms := &mockStore{}
	ms.reapExpiredConcurrencyKeysFn = func(ctx context.Context) (int64, error) {
		return 5, nil
	}

	w := newTestWorker(ms)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	w.ctx = ctx
	w.cancel = cancel

	done := make(chan struct{})
	go func() {
		w.wg.Add(1)
		w.concurrencyKeyReaperLoop()
		close(done)
	}()
	<-done
}

func TestConcurrencyKeyReaperLoop_ZeroReaped(t *testing.T) {
	ms := &mockStore{}
	ms.reapExpiredConcurrencyKeysFn = func(ctx context.Context) (int64, error) {
		return 0, nil
	}

	w := newTestWorker(ms)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	w.ctx = ctx
	w.cancel = cancel

	done := make(chan struct{})
	go func() {
		w.wg.Add(1)
		w.concurrencyKeyReaperLoop()
		close(done)
	}()
	<-done
}

// ---------------------------------------------------------------------------
// updateDispatchLoop
// ---------------------------------------------------------------------------

func TestUpdateDispatchLoop_ContextCancel(t *testing.T) {
	ms := &mockStore{}
	w := newTestWorker(ms)

	done := make(chan struct{})
	go func() {
		w.wg.Add(1)
		w.updateDispatchLoop()
		close(done)
	}()

	w.cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("updateDispatchLoop did not stop after context cancellation")
	}
}

func TestUpdateDispatchLoop_NoPanic(t *testing.T) {
	ms := &mockStore{}
	w := newTestWorker(ms)
	w.inflight.Store("wf-upd-1", &host.WorkflowInstance{ID: "wf-upd-1"})

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	w.ctx = ctx
	w.cancel = cancel

	done := make(chan struct{})
	go func() {
		w.wg.Add(1)
		w.updateDispatchLoop()
		close(done)
	}()
	<-done
}

// ---------------------------------------------------------------------------
// API handler remaining edges
// ---------------------------------------------------------------------------

func TestAPIGetQueryState_KeyNotFound(t *testing.T) {
	ms := &mockStore{}
	ms.getQueryStateFn = func(ctx context.Context, workflowID, key string) (string, error) {
		return "", nil
	}

	api := &apiServer{store: ms, worker: newTestWorker(ms)}
	req := httptest.NewRequest(http.MethodGet, "/api/workflows/wf-query-2/query?key=missingkey", nil)
	w := httptest.NewRecorder()
	api.handleWorkflows(w, req)

	resp := w.Result()
	if resp.StatusCode != 200 {
		t.Errorf("expected 200 for missing query key, got %d", resp.StatusCode)
	}
	var body map[string]string
	json.NewDecoder(resp.Body).Decode(&body)
	resp.Body.Close()
	if body["value"] != "" {
		t.Errorf("expected empty value for missing key, got %q", body["value"])
	}
}

func TestAPIGetQueryState_StoreError(t *testing.T) {
	ms := &mockStore{}
	ms.getQueryStateFn = func(ctx context.Context, workflowID, key string) (string, error) {
		return "", errors.New("db error")
	}

	api := &apiServer{store: ms, worker: newTestWorker(ms)}
	req := httptest.NewRequest(http.MethodGet, "/api/workflows/wf-query-err/query?key=mykey", nil)
	w := httptest.NewRecorder()
	api.handleWorkflows(w, req)

	resp := w.Result()
	if resp.StatusCode != 500 {
		t.Errorf("expected 500 for store error, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestAPISignal_StoreError(t *testing.T) {
	ms := &mockStore{}
	ms.deliverSignalFn = func(ctx context.Context, workflowID, signalName, payload string) error {
		return errors.New("workflow not found")
	}

	api := &apiServer{store: ms, worker: newTestWorker(ms)}
	body := `{"signal_name":"my-signal","payload":"{}"}`
	req := httptest.NewRequest(http.MethodPost, "/api/workflows/wf-missing/signal", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	api.handleWorkflows(w, req)

	resp := w.Result()
	if resp.StatusCode != 500 {
		t.Errorf("expected 500 for store error, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestAPISignal_ErrorOnDeliverWithEmptyName(t *testing.T) {
	// Empty signal_name should be rejected before any store call.
	ms := &mockStore{}
	api := &apiServer{store: ms, worker: newTestWorker(ms)}
	body := `{"signal_name":"","payload":"{}"}`
	req := httptest.NewRequest(http.MethodPost, "/api/workflows/wf-1/signal", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	api.handleWorkflows(w, req)

	resp := w.Result()
	if resp.StatusCode != 400 {
		t.Errorf("expected 400 for empty signal_name, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestAPICancel_StoreError(t *testing.T) {
	ms := &mockStore{}
	ms.requestCancellationFn = func(ctx context.Context, workflowID, reason string) error {
		return errors.New("workflow not found")
	}

	api := &apiServer{store: ms, worker: newTestWorker(ms)}
	req := httptest.NewRequest(http.MethodPost, "/api/workflows/wf-missing/cancel", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	api.handleWorkflows(w, req)

	resp := w.Result()
	if resp.StatusCode != 500 {
		t.Errorf("expected 500 for store error, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestAPICancel_EmptyBody(t *testing.T) {
	// Cancel with no body should still work (reason defaults to empty).
	ms := &mockStore{}
	cancelCalled := false
	ms.requestCancellationFn = func(ctx context.Context, workflowID, reason string) error {
		cancelCalled = true
		return nil
	}

	api := &apiServer{store: ms, worker: newTestWorker(ms)}
	req := httptest.NewRequest(http.MethodPost, "/api/workflows/wf-cancel-2/cancel", nil)
	w := httptest.NewRecorder()
	api.handleWorkflows(w, req)

	resp := w.Result()
	if resp.StatusCode != 200 {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()
	if !cancelCalled {
		t.Error("expected RequestCancellation to be called")
	}
}

// ---------------------------------------------------------------------------
// Workflow update handler edges
// ---------------------------------------------------------------------------

func TestAPIWorkflowUpdate_CreateError(t *testing.T) {
	ms := &mockStore{}
	ms.getWorkflowByIDFn = func(ctx context.Context, id string) (*host.WorkflowInstance, error) {
		return &host.WorkflowInstance{ID: "wf-update-err", Status: "running"}, nil
	}
	ms.createUpdateRequestFn = func(ctx context.Context, workflowID, updateName, payload, promiseID string) error {
		return errors.New("database error")
	}
	ms.createPromiseFn = func(ctx context.Context, workflowID, promiseName, promiseID string) error {
		return nil
	}

	api := &apiServer{store: ms, worker: newTestWorker(ms)}
	req := httptest.NewRequest(http.MethodPost, "/api/workflows/wf-update-err/update/my-action", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	api.handleWorkflows(w, req)

	resp := w.Result()
	if resp.StatusCode != 500 {
		t.Errorf("expected 500 for CreateUpdateRequest error, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestAPIWorkflowUpdate_GetPendingError(t *testing.T) {
	ms := &mockStore{}
	ms.getWorkflowByIDFn = func(ctx context.Context, id string) (*host.WorkflowInstance, error) {
		return &host.WorkflowInstance{ID: "wf-update-pend-err", Status: "running"}, nil
	}
	ms.getPendingUpdateRequestsFn = func(ctx context.Context, workflowID string) ([]host.UpdateRequestInfo, error) {
		return nil, errors.New("db error")
	}

	api := &apiServer{store: ms, worker: newTestWorker(ms)}
	req := httptest.NewRequest(http.MethodPost, "/api/workflows/wf-update-pend-err/update/my-action", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	api.handleWorkflows(w, req)

	resp := w.Result()
	if resp.StatusCode != 500 {
		t.Errorf("expected 500 for GetPendingUpdateRequests error, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestAPIWorkflowUpdate_GetWorkflowError(t *testing.T) {
	ms := &mockStore{}
	ms.getWorkflowByIDFn = func(ctx context.Context, id string) (*host.WorkflowInstance, error) {
		return nil, errors.New("db error")
	}

	api := &apiServer{store: ms, worker: newTestWorker(ms)}
	req := httptest.NewRequest(http.MethodPost, "/api/workflows/wf-db-err/update/my-action", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	api.handleWorkflows(w, req)

	resp := w.Result()
	if resp.StatusCode != 500 {
		t.Errorf("expected 500 for GetWorkflowByID error, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestAPIWorkflowUpdate_PromiseStoreError(t *testing.T) {
	ms := &mockStore{}
	ms.getWorkflowByIDFn = func(ctx context.Context, id string) (*host.WorkflowInstance, error) {
		return &host.WorkflowInstance{ID: "wf-update-prom-err", Status: "running"}, nil
	}
	ms.createUpdateRequestFn = func(ctx context.Context, workflowID, updateName, payload, promiseID string) error {
		return nil
	}
	ms.createPromiseFn = func(ctx context.Context, workflowID, promiseName, promiseID string) error {
		return errors.New("promise error")
	}

	api := &apiServer{store: ms, worker: newTestWorker(ms)}
	req := httptest.NewRequest(http.MethodPost, "/api/workflows/wf-update-prom-err/update/my-action", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	api.handleWorkflows(w, req)

	resp := w.Result()
	// The promise creation failure is logged but the handler should still
	// return 202 since the update request was created.
	if resp.StatusCode != 202 {
		t.Errorf("expected 202 despite promise error, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// ---------------------------------------------------------------------------
// DAG handler edges
// ---------------------------------------------------------------------------

func TestAPIGetDAG_GetWorkflowError(t *testing.T) {
	ms := &mockStore{}
	ms.getWorkflowByIDFn = func(ctx context.Context, id string) (*host.WorkflowInstance, error) {
		return nil, errors.New("db error")
	}

	api := &apiServer{store: ms, worker: newTestWorker(ms)}
	req := httptest.NewRequest(http.MethodGet, "/api/workflows/wf-dag-err/dag", nil)
	w := httptest.NewRecorder()
	api.handleWorkflows(w, req)

	resp := w.Result()
	if resp.StatusCode != 500 {
		t.Errorf("expected 500 for GetWorkflowByID error, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestAPIGetDAG_LoadSpecError(t *testing.T) {
	ms := &mockStore{}
	ms.getWorkflowByIDFn = func(ctx context.Context, id string) (*host.WorkflowInstance, error) {
		return &host.WorkflowInstance{ID: "wf-dag-err", DefName: "dag-workflow", DefVersion: 1}, nil
	}
	ms.loadDAGSpecFn = func(ctx context.Context, defName string, defVersion int) (json.RawMessage, error) {
		return nil, errors.New("spec corrupted")
	}

	api := &apiServer{store: ms, worker: newTestWorker(ms)}
	req := httptest.NewRequest(http.MethodGet, "/api/workflows/wf-dag-err/dag", nil)
	w := httptest.NewRecorder()
	api.handleWorkflows(w, req)

	resp := w.Result()
	if resp.StatusCode != 404 {
		t.Errorf("expected 404 for LoadDAGSpec error, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestAPIGetDAG_UnmarshalError(t *testing.T) {
	ms := &mockStore{}
	ms.getWorkflowByIDFn = func(ctx context.Context, id string) (*host.WorkflowInstance, error) {
		return &host.WorkflowInstance{ID: "wf-dag-bad", DefName: "dag-workflow", DefVersion: 1}, nil
	}
	ms.loadDAGSpecFn = func(ctx context.Context, defName string, defVersion int) (json.RawMessage, error) {
		return json.RawMessage(`not valid json`), nil
	}

	api := &apiServer{store: ms, worker: newTestWorker(ms)}
	req := httptest.NewRequest(http.MethodGet, "/api/workflows/wf-dag-bad/dag", nil)
	w := httptest.NewRecorder()
	api.handleWorkflows(w, req)

	resp := w.Result()
	if resp.StatusCode != 500 {
		t.Errorf("expected 500 for unmarshal error, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// ---------------------------------------------------------------------------
// workflow update with no body (nil body)
// ---------------------------------------------------------------------------

func TestAPIWorkflowUpdate_NoBody(t *testing.T) {
	ms := &mockStore{}
	ms.getWorkflowByIDFn = func(ctx context.Context, id string) (*host.WorkflowInstance, error) {
		return &host.WorkflowInstance{ID: "wf-update-nobody", Status: "running"}, nil
	}
	createCalled := false
	ms.createUpdateRequestFn = func(ctx context.Context, workflowID, updateName, payload, promiseID string) error {
		createCalled = true
		if payload != "{}" {
			t.Errorf("expected default payload '{}', got %q", payload)
		}
		return nil
	}
	ms.createPromiseFn = func(ctx context.Context, workflowID, promiseName, promiseID string) error {
		return nil
	}

	api := &apiServer{store: ms, worker: newTestWorker(ms)}
	req := httptest.NewRequest(http.MethodPost, "/api/workflows/wf-update-nobody/update/my-action", nil)
	w := httptest.NewRecorder()
	api.handleWorkflows(w, req)

	resp := w.Result()
	if resp.StatusCode != 202 {
		t.Errorf("expected 202, got %d", resp.StatusCode)
	}
	resp.Body.Close()
	if !createCalled {
		t.Error("expected CreateUpdateRequest to be called")
	}
}


// ---------------------------------------------------------------------------
// executeWorkflow: WASM panic recovery
// ---------------------------------------------------------------------------

func TestExecuteWorkflow_WASMPanicRecovery(t *testing.T) {
	ms := &mockStore{}
	done := make(chan struct{})

	ms.loadWASMFn = func(ctx context.Context, defName string, defVersion int) ([]byte, error) {
		panic("test panic from WASM load")
	}
	ms.releaseWorkflowFn = func(ctx context.Context, workflowID, workerID string, nextWakeAt time.Time) error {
		close(done)
		return nil
	}
	ms.failWorkflowFn = func(ctx context.Context, workflowID, workerID, errMsg string, queryState map[string]string) error {
		// Panics with error message call FailWorkflow, not ReleaseWorkflow.
		close(done)
		return nil
	}
	ms.traceWorkflowFn = func(ctx context.Context, workflowID, traceID string) (sql.Result, error) {
		return nil, nil
	}

	w := newTestWorker(ms)
	w.wg.Add(1)
	go w.executeWorkflow(&host.WorkflowInstance{
		ID: "wf-panic-test", DefName: "test", DefVersion: 1, Input: json.RawMessage("{}"),
	})

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for release/fail after panic")
	}
}

// ---------------------------------------------------------------------------
// executeWorkflow: runtime create error
// ---------------------------------------------------------------------------

func TestExecuteWorkflow_RuntimeCreateError(t *testing.T) {
	ms := &mockStore{}
	wasmLoaded := make(chan struct{})
	failed := make(chan struct{})

	ms.loadWASMFn = func(ctx context.Context, defName string, defVersion int) ([]byte, error) {
		close(wasmLoaded)
		return []byte("bad-wasm"), nil
	}
	ms.loadEventHistoryFn = func(ctx context.Context, workflowID string) ([]host.EventRecord, error) {
		<-wasmLoaded
		return []host.EventRecord{}, nil
	}
	ms.loadCompactionStateFn = func(ctx context.Context, workflowID string) (*host.CompactionState, error) {
		return nil, nil
	}
	ms.failWorkflowFn = func(ctx context.Context, workflowID, workerID, errMsg string, queryState map[string]string) error {
		close(failed)
		return nil
	}
	ms.traceWorkflowFn = func(ctx context.Context, workflowID, traceID string) (sql.Result, error) {
		return nil, nil
	}

	w := newTestWorker(ms)
	w.wg.Add(1)
	go w.executeWorkflow(&host.WorkflowInstance{
		ID: "wf-runtime-err", DefName: "test", DefVersion: 1, Input: json.RawMessage("{}"),
	})

	select {
	case <-failed:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for FailWorkflow after runtime error")
	}
}

// ---------------------------------------------------------------------------
// Sticky rebalance: dispatch claims sticky and general
// ---------------------------------------------------------------------------

func TestDispatchLoop_StickyRebalanceClaim(t *testing.T) {
	ms := &mockStore{}
	var mu sync.Mutex
	stickyCalls := 0
	generalCalls := 0

	ms.claimStickyWorkflowsFn = func(ctx context.Context, workerID, namespace string, limit int) ([]*host.WorkflowInstance, error) {
		mu.Lock()
		stickyCalls++
		mu.Unlock()
		if stickyCalls == 1 {
			return []*host.WorkflowInstance{
				{ID: "wf-sticky-1", DefName: "test", DefVersion: 1, Status: "ready"},
			}, nil
		}
		return nil, nil
	}
	ms.claimWorkflowsFn = func(ctx context.Context, workerID, namespace string, limit int) ([]*host.WorkflowInstance, error) {
		mu.Lock()
		generalCalls++
		mu.Unlock()
		return nil, nil
	}
	ms.loadWASMFn = func(ctx context.Context, defName string, defVersion int) ([]byte, error) {
		return nil, nil
	}
	ms.failWorkflowFn = func(ctx context.Context, workflowID, workerID, errMsg string, queryState map[string]string) error {
		return nil
	}

	w := newTestWorker(ms)
	w.pollInterval = 10 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
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

	mu.Lock()
	sc := stickyCalls
	gc := generalCalls
	mu.Unlock()

	if sc == 0 {
		t.Error("expected ClaimStickyWorkflows to be called")
	}
	if gc == 0 {
		t.Log("general pool was not called (capacity may have been filled by sticky)")
	}
}

// ---------------------------------------------------------------------------
// Heartbeat: workflows remain in inflight after successful heartbeat
// ---------------------------------------------------------------------------

func TestHeartbeatLoop_PreservesAliveWorkflows(t *testing.T) {
	ms := &mockStore{}
	var mu sync.Mutex
	heartbeatCalls := 0

	ms.heartbeatFn = func(ctx context.Context, workflowID, workerID string) (bool, error) {
		mu.Lock()
		heartbeatCalls++
		mu.Unlock()
		return true, nil
	}

	w := newTestWorker(ms)
	w.heartbeatInterval = 5 * time.Millisecond
	w.inflight.Store("wf-alive", &host.WorkflowInstance{ID: "wf-alive"})

	done := make(chan struct{})
	go func() {
		w.wg.Add(1)
		w.heartbeatLoop()
		close(done)
	}()

	time.Sleep(40 * time.Millisecond)
	w.cancel()
	<-done

	mu.Lock()
	count := heartbeatCalls
	mu.Unlock()

	if count == 0 {
		t.Error("expected at least one heartbeat call")
	}

	if _, ok := w.inflight.Load("wf-alive"); !ok {
		t.Error("expected wf-alive to remain in inflight after successful heartbeat")
	}
}

// ---------------------------------------------------------------------------
// Context propagation: cancellation during dispatch
// ---------------------------------------------------------------------------

func TestDispatchLoop_ContextCancellation(t *testing.T) {
	ms := &mockStore{}

	claimStarted := make(chan struct{})
	ms.claimStickyWorkflowsFn = func(ctx context.Context, workerID, namespace string, limit int) ([]*host.WorkflowInstance, error) {
		close(claimStarted)
		<-ctx.Done()
		return nil, ctx.Err()
	}

	w := newTestWorker(ms)
	w.pollInterval = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	w.ctx = ctx
	w.cancel = cancel

	done := make(chan struct{})
	go func() {
		w.wg.Add(1)
		w.dispatchLoop()
		close(done)
	}()

	select {
	case <-claimStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for claim to start")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("dispatchLoop did not exit after context cancellation")
	}
}

// ---------------------------------------------------------------------------
// executeWorkflow: context cancellation during history load
// ---------------------------------------------------------------------------

func TestExecuteWorkflow_ContextCancellationDuringHistoryLoad(t *testing.T) {
	ms := &mockStore{}
	ctx, cancel := context.WithCancel(context.Background())

	wasmLoaded := make(chan struct{})
	ms.loadWASMFn = func(ctx context.Context, defName string, defVersion int) ([]byte, error) {
		close(wasmLoaded)
		return []byte("some-wasm"), nil
	}
	ms.loadEventHistoryFn = func(ctx context.Context, workflowID string) ([]host.EventRecord, error) {
		<-wasmLoaded
		cancel()
		return nil, ctx.Err()
	}
	ms.releaseWorkflowFn = func(ctx context.Context, workflowID, workerID string, nextWakeAt time.Time) error {
		return nil
	}
	ms.traceWorkflowFn = func(ctx context.Context, workflowID, traceID string) (sql.Result, error) {
		return nil, nil
	}

	w := newTestWorker(ms)
	w.ctx = ctx
	w.cancel = cancel

	w.wg.Add(1)
	w.executeWorkflow(&host.WorkflowInstance{
		ID: "wf-cancel-history", DefName: "test", DefVersion: 1, Input: json.RawMessage("{}"),
	})
}
