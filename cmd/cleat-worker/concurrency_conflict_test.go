package main

// Caller-side regression test for the IMPROVEMENT-PLAN.md 1.2 residual: the
// concurrency-key conflict path in handleStartWorkflow answered 409 while
// leaving the losing run runnable, because its rejection write was
// FailWorkflow -- the owning worker's fenced terminal write -- applied to a
// run no worker owned yet.
//
// engine/fence_lost_callers_test.go pins the two store-side facts this test
// depends on, against a real PostgreSQL:
//
//   - FailWorkflow on an unclaimed run returns ErrFenceLost and leaves the
//     row 'ready' (TestFailWorkflowFenceCannotMatchUnclaimedRun), and
//   - a run left 'ready' after a rejected enqueue is claimed and executed
//     (TestConcurrencyKeyConflict_RejectedRunIsNotClaimable).
//
// So the fence rule encoded in the mock below is not an assumption about the
// database; it is a rule the database is separately tested to follow. What
// this test adds is the half those cannot reach: which store call the HTTP
// handler chooses. Reverting server.go to FailWorkflow fails this test and
// neither of those.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
)

// TestConcurrencyKeyConflict_LosingRunIsActuallyRejected asserts that a 409
// is accompanied by a rejection write that actually applied.
//
// The assertion is deliberately not "TerminateWorkflow was called" -- that
// would pin the implementation rather than the property. It is "some store
// call left the run non-runnable", which any correct rejection satisfies and
// a fenced no-op does not.
func TestConcurrencyKeyConflict_LosingRunIsActuallyRejected(t *testing.T) {
	const (
		runID = "losing-run"
		key   = "order-42"
	)

	var mu sync.Mutex
	rejected := false    // a rejection write applied
	fenceLostWrites := 0 // rejection writes that matched no row

	ms := &mockStore{}
	ms.startNewRunFn = func(ctx context.Context, id, defName string, defVersion int, input json.RawMessage, idempotencyKey, tenantID string, priority int) (string, bool, error) {
		// StartNewRun inserts the run 'ready' with assigned_to NULL.
		return runID, false, nil
	}
	ms.acquireConcurrencyKeyFn = func(ctx context.Context, k, workflowID string, ttl time.Duration) (bool, error) {
		return false, nil // the key is held by another workflow
	}
	ms.failWorkflowFn = func(ctx context.Context, workflowID, workerID string, generation int64, errorMsg, errorCode, errorOp string, queryState map[string]string) error {
		// The real fence: `WHERE id = $1 AND assigned_to = $2 AND
		// generation = $7` against a row whose assigned_to is NULL. No
		// (workerID, generation) an unowning caller can pass will match,
		// and `NULL = ''` is NULL rather than true.
		mu.Lock()
		defer mu.Unlock()
		fenceLostWrites++
		return engine.ErrFenceLost
	}
	ms.terminateWorkflowFn = func(ctx context.Context, workflowID, reason string) error {
		// Matches on id alone, so it applies to an unclaimed run.
		mu.Lock()
		defer mu.Unlock()
		rejected = true
		return nil
	}

	api := newTestAPIServer(ms)

	req := httptest.NewRequest(http.MethodPost, "/api/workflows/my-wf/start", strings.NewReader(`{"input":{}}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cleat-Concurrency-Key", key)
	resp := httptest.NewRecorder()
	api.handleStartWorkflow(resp, req, "my-wf")

	if resp.Code != 409 {
		t.Fatalf("status = %d, want 409 for a held concurrency key (body: %s)", resp.Code, resp.Body.String())
	}

	mu.Lock()
	defer mu.Unlock()
	if !rejected {
		t.Errorf("handler answered 409 but no rejection write applied: the run stays 'ready' "+
			"with next_wake_at in the past and the next worker to poll executes it "+
			"(%d rejection write(s) matched no row)", fenceLostWrites)
	}
	if fenceLostWrites > 0 {
		t.Errorf("rejection used a fenced owner-only write %d time(s); it cannot match an unclaimed run", fenceLostWrites)
	}
}

// TestConcurrencyKeyConflict_UnrejectableRunIsNot409 covers the other half of
// the contract: if the rejection write fails, the client must not be told the
// key was enforced. A 409 there would be the same lie the fenced no-op told,
// just with an error in the log.
func TestConcurrencyKeyConflict_UnrejectableRunIsNot409(t *testing.T) {
	ms := &mockStore{}
	ms.startNewRunFn = func(ctx context.Context, id, defName string, defVersion int, input json.RawMessage, idempotencyKey, tenantID string, priority int) (string, bool, error) {
		return "losing-run", false, nil
	}
	ms.acquireConcurrencyKeyFn = func(ctx context.Context, k, workflowID string, ttl time.Duration) (bool, error) {
		return false, nil
	}
	ms.terminateWorkflowFn = func(ctx context.Context, workflowID, reason string) error {
		return context.DeadlineExceeded
	}

	api := newTestAPIServer(ms)

	req := httptest.NewRequest(http.MethodPost, "/api/workflows/my-wf/start", strings.NewReader(`{"input":{}}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cleat-Concurrency-Key", "order-42")
	resp := httptest.NewRecorder()
	api.handleStartWorkflow(resp, req, "my-wf")

	if resp.Code == 409 {
		t.Errorf("status = 409 although the losing run could not be rejected and is still runnable; "+
			"want a 5xx (body: %s)", resp.Body.String())
	}
	if resp.Code < 500 {
		t.Errorf("status = %d, want 5xx when the rejection write failed (body: %s)", resp.Code, resp.Body.String())
	}
}
