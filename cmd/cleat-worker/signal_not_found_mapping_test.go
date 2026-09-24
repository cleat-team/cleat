package main

// cleat#2239: four call sites in this package map engine.ErrWorkflowNotFound
// to 404, and none of them had a test -- found in review after the store-level
// fix landed. Each is on a different branch of handleSignal or
// handleGetAllowedSignals, and none of the existing signal tests exercises the
// not-found return from the store method the branch actually calls:
// TestASignalForAnUnownedWorkflowNeverReachesTheStore stops at callerOwnsTarget
// (GetWorkflowByID), which runs BEFORE any of these four and never reaches
// them; TestPutAllowedSignalsUnknownWorkflowIs404 covers the PUT side of
// allowed-signals, not the GET side tested here.
//
// All four run unauthenticated, matching newTestAPIServer's other direct
// mockStore tests: with no tenant on the request, callerOwnsTarget's ownership
// check (server.go's comment above handleSignal) returns early without calling
// GetWorkflowByID, so it cannot be what produces the 404 -- only the mapping
// under test can.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

// TestSignalAuthCheckUnknownWorkflowIs404 covers server.go's handleSignal
// branch that calls GetAllowedSignalCallers when --require-signal-auth is on
// (~server.go:1249). This is the one path cleat#2218 never touched:
// GetAllowedSignalCallers used to return (nil, nil) for a missing row, which
// read as "exists, but nobody is allowed" -- a 403, not a 404 -- and never
// unregistered the awaiter that triggered cleat#2213.
func TestSignalAuthCheckUnknownWorkflowIs404(t *testing.T) {
	ms := &mockStore{}
	ms.getAllowedSignalCallersFn = func(_ context.Context, _ string) ([]string, error) {
		return nil, engine.ErrWorkflowNotFound
	}
	api := newTestAPIServer(ms)
	on := true
	api.worker.requireSignalAuth = &on

	req := httptest.NewRequest(http.MethodPost, "/api/workflows/nope/signal",
		strings.NewReader(`{"signal_name":"approve","payload":"{}"}`))
	rec := httptest.NewRecorder()
	api.handleWorkflows(rec, req)

	if rec.Code != 404 {
		t.Errorf("status %d, want 404 for a workflow the auth check cannot find; body %s",
			rec.Code, rec.Body.String())
	}
}

// TestSignalAuthCheckStoreErrorIs500 is the negative control for the test
// above: a plain store error on the same call must still be a 500, not a 404 --
// otherwise the branch could be satisfied by mapping every error to 404
// instead of checking specifically for ErrWorkflowNotFound.
func TestSignalAuthCheckStoreErrorIs500(t *testing.T) {
	ms := &mockStore{}
	ms.getAllowedSignalCallersFn = func(_ context.Context, _ string) ([]string, error) {
		return nil, errors.New("connection reset")
	}
	api := newTestAPIServer(ms)
	on := true
	api.worker.requireSignalAuth = &on

	req := httptest.NewRequest(http.MethodPost, "/api/workflows/wf-1/signal",
		strings.NewReader(`{"signal_name":"approve","payload":"{}"}`))
	rec := httptest.NewRecorder()
	api.handleWorkflows(rec, req)

	if rec.Code != 500 {
		t.Errorf("status %d, want 500 for a plain store error", rec.Code)
	}
}

// TestIdempotentSignalUnknownWorkflowIs404 covers handleSignal's
// Idempotency-Key branch (~server.go:1282), calling
// DeliverSignalIdempotent -- the TOCTOU window server.go's comment there
// describes: callerOwnsTarget already confirmed the id exists, so
// ErrWorkflowNotFound here means the workflow was purged in between.
func TestIdempotentSignalUnknownWorkflowIs404(t *testing.T) {
	ms := &mockStore{}
	ms.deliverSignalIdempotentFn = func(_ context.Context, _, _, _, _ string) (bool, error) {
		return false, engine.ErrWorkflowNotFound
	}
	api := newTestAPIServer(ms)

	req := httptest.NewRequest(http.MethodPost, "/api/workflows/wf-purged/signal",
		strings.NewReader(`{"signal_name":"approve","payload":"{}"}`))
	req.Header.Set("Idempotency-Key", "key-1")
	rec := httptest.NewRecorder()
	api.handleWorkflows(rec, req)

	if rec.Code != 404 {
		t.Errorf("status %d, want 404 for a workflow purged between callerOwnsTarget and "+
			"DeliverSignalIdempotent; body %s", rec.Code, rec.Body.String())
	}
}

// TestPlainSignalUnknownWorkflowIs404 covers handleSignal's keyless delivery
// branch (~server.go:1305), calling DeliverSignal directly -- the same TOCTOU
// window as the idempotent path, without an Idempotency-Key.
func TestPlainSignalUnknownWorkflowIs404(t *testing.T) {
	ms := &mockStore{}
	ms.deliverSignalFn = func(_ context.Context, _, _, _ string) error {
		return engine.ErrWorkflowNotFound
	}
	api := newTestAPIServer(ms)

	req := httptest.NewRequest(http.MethodPost, "/api/workflows/wf-purged/signal",
		strings.NewReader(`{"signal_name":"approve","payload":"{}"}`))
	rec := httptest.NewRecorder()
	api.handleWorkflows(rec, req)

	if rec.Code != 404 {
		t.Errorf("status %d, want 404 for a workflow purged between callerOwnsTarget and "+
			"DeliverSignal; body %s", rec.Code, rec.Body.String())
	}
}

// TestGetAllowedSignalsUnknownWorkflowIs404 covers handleGetAllowedSignals
// (~server.go:1995), which has no callerOwnsTarget pre-check of its own -- this
// is the FIRST and ONLY place a missing id is discovered on this endpoint, not
// a narrow race window like the three handleSignal branches above. It is the
// GET-side sibling of TestPutAllowedSignalsUnknownWorkflowIs404
// (allowed_signals_api_test.go), which covers only the PUT half of this same
// resource.
func TestGetAllowedSignalsUnknownWorkflowIs404(t *testing.T) {
	ms := &mockStore{}
	ms.getAllowedSignalCallersFn = func(_ context.Context, _ string) ([]string, error) {
		return nil, engine.ErrWorkflowNotFound
	}
	api := newTestAPIServer(ms)

	req := httptest.NewRequest(http.MethodGet, "/api/workflows/nope/allowed-signals", nil)
	rec := httptest.NewRecorder()
	api.handleWorkflows(rec, req)

	if rec.Code != 404 {
		t.Errorf("status %d, want 404 for a workflow this tenant cannot see; body %s",
			rec.Code, rec.Body.String())
	}
}

// TestGetAllowedSignalsStoreErrorIs500 is the negative control for the test
// above, matching TestSignalAuthCheckStoreErrorIs500's reasoning.
func TestGetAllowedSignalsStoreErrorIs500(t *testing.T) {
	ms := &mockStore{}
	ms.getAllowedSignalCallersFn = func(_ context.Context, _ string) ([]string, error) {
		return nil, errors.New("connection reset")
	}
	api := newTestAPIServer(ms)

	req := httptest.NewRequest(http.MethodGet, "/api/workflows/wf-1/allowed-signals", nil)
	rec := httptest.NewRecorder()
	api.handleWorkflows(rec, req)

	if rec.Code != 500 {
		t.Errorf("status %d, want 500 for a plain store error", rec.Code)
	}
}
