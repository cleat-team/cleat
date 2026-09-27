package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

// cleat#1975 (D3): the HTTP layer's half of "settled is final". The engine
// tests (engine/settled_is_final_test.go) pin the SQL; these pin that
// ErrAdminStateConflict actually reaches the caller as 409 -- both terminate
// and pre-emptive cancel used to fall through to a bare 500 for any
// non-ErrWorkflowNotFound error, which would have swallowed this refusal into
// an opaque server error rather than the 409 state_conflict re-replay,
// force-complete and force-fail already give.

// TestHandleDeadLetterTerminate_RefusesASettledWorkflow: the only terminate
// route in the tree, refusing a settled row that isn't the dead_lettered
// exception it exists to serve.
func TestHandleDeadLetterTerminate_RefusesASettledWorkflow(t *testing.T) {
	ms := &mockStore{
		terminateWorkflowFn: func(_ context.Context, workflowID, reason string) error {
			return fmt.Errorf("workflow %s: already settled (status=done); refusing to write terminated over it: %w",
				workflowID, engine.ErrAdminStateConflict)
		},
	}
	api := newTestAPIServer(ms)
	mux := http.NewServeMux()
	registerRoutes(mux, api)

	req := httptest.NewRequest(http.MethodPost, "/api/dead-letters/wf-1/terminate", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", w.Code)
	}
	var body map[string]string
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["detail"] != "state_conflict" {
		t.Errorf("detail = %q, want %q (body: %v)", body["detail"], "state_conflict", body)
	}
}

// TestHandleCancelPreemptive_RefusesASettledWorkflow: same mapping, through
// the preemptive:true arm of POST /api/workflows/:id/cancel.
func TestHandleCancelPreemptive_RefusesASettledWorkflow(t *testing.T) {
	ms := &mockStore{
		cancelWorkflowFn: func(_ context.Context, workflowID, reason string) error {
			return fmt.Errorf("workflow %s: already settled (status=terminated); refusing to write cancelled over it: %w",
				workflowID, engine.ErrAdminStateConflict)
		},
	}
	api := newTestAPIServer(ms)
	req := httptest.NewRequest(http.MethodPost, "/api/workflows/wf-1/cancel",
		strings.NewReader(`{"reason":"r","preemptive":true}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	api.handleCancel(w, req, "wf-1")

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", w.Code)
	}
	var body map[string]string
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["detail"] != "state_conflict" {
		t.Errorf("detail = %q, want %q (body: %v)", body["detail"], "state_conflict", body)
	}
}
