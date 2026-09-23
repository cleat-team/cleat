package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

// cleat#2108. handleStartWorkflow's top-level "entry_point" REST field used
// to WRAP the caller's input -- {"input": originalInput, "__entry_point":
// name} -- instead of flat-merging __entry_point into it. determineEntryPoint
// (cmd/cleat-worker/setup.go) still resolved the entry point correctly (it
// only reads the top-level __entry_point key), but the guest received the
// wrapped shape verbatim as its own typed parameter and failed to
// deserialize -- confirmed live against a real cargo-built Rust workflow
// while verifying cleat#2097/#2109.
//
// determineEntryPoint's own doc comment documents the correct shape as "an
// explicit __entry_point field in the start input" -- a sibling of the
// entry's own fields -- and cmd/cleat-bench/main.go already uses exactly
// that: `{"__entry_point":"%s","order_id":"bench"}`.
func TestStartWorkflowEntryPointFieldFlatMergesNotNests(t *testing.T) {
	var captured json.RawMessage
	st := &mockStore{
		startNewRunFn: func(_ context.Context, _ string, _ string, _ int, input json.RawMessage, _ string, _ string, _ int) (string, bool, error) {
			captured = input
			return "run-1", false, nil
		},
	}
	api := &apiServer{store: st, worker: newTestWorker(st), maxBodySize: 1 << 20}

	req := httptest.NewRequest(http.MethodPost, "/api/workflows/d/start",
		strings.NewReader(`{"input":{"user_id":"u1","cart":[{"sku":"widget"}]},"entry_point":"place_order"}`))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	api.handleStartWorkflow(resp, req, "d")

	if resp.Code != http.StatusCreated {
		t.Fatalf("start: got %d, want 201: %s", resp.Code, resp.Body.String())
	}

	var stored map[string]any
	if err := json.Unmarshal(captured, &stored); err != nil {
		t.Fatalf("stored input did not parse as JSON: %v: %s", err, captured)
	}

	// THE BUG: an "input" key present at the top level means the wrap
	// happened -- the guest's own fields are nested one level too deep and it
	// will fail to deserialize them from wf.Input directly.
	if _, wrapped := stored["input"]; wrapped {
		t.Fatalf("stored input is WRAPPED under an \"input\" key -- the guest's own fields "+
			"(user_id, cart) are nested and will not deserialize as the entry's typed parameter: %s", captured)
	}

	if stored["__entry_point"] != "place_order" {
		t.Errorf("stored input's __entry_point = %v, want \"place_order\": %s", stored["__entry_point"], captured)
	}
	if stored["user_id"] != "u1" {
		t.Errorf("stored input's user_id = %v, want \"u1\" (flat, a sibling of __entry_point): %s", stored["user_id"], captured)
	}
	if _, ok := stored["cart"]; !ok {
		t.Errorf("stored input has no top-level \"cart\" field: %s", captured)
	}
}

// The positive control this test's assertions are checked against: with no
// entry_point field at all, the caller's input must reach the store
// untouched -- proving the test above is checking what entry_point ADDS,
// not merely how mockStore happens to echo things back.
func TestStartWorkflowWithNoEntryPointFieldPassesInputThrough(t *testing.T) {
	var captured json.RawMessage
	st := &mockStore{
		startNewRunFn: func(_ context.Context, _ string, _ string, _ int, input json.RawMessage, _ string, _ string, _ int) (string, bool, error) {
			captured = input
			return "run-1", false, nil
		},
	}
	api := &apiServer{store: st, worker: newTestWorker(st), maxBodySize: 1 << 20}

	req := httptest.NewRequest(http.MethodPost, "/api/workflows/d/start",
		strings.NewReader(`{"input":{"user_id":"u1"}}`))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	api.handleStartWorkflow(resp, req, "d")

	if resp.Code != http.StatusCreated {
		t.Fatalf("start: got %d, want 201: %s", resp.Code, resp.Body.String())
	}
	if strings.TrimSpace(string(captured)) != `{"user_id":"u1"}` {
		t.Errorf("with no entry_point, input should pass through untouched, got: %s", captured)
	}
}

// A caller sending "entry_point" against a non-object "input" gets a clear
// 400 rather than the pre-fix behaviour: json.Unmarshal's error was
// discarded and the whole input silently became {"input":null,"__entry_point":
// "..."} -- the caller's actual input simply vanished with no indication why.
func TestStartWorkflowEntryPointAgainstNonObjectInputIsRefused(t *testing.T) {
	st := &mockStore{}
	api := &apiServer{store: st, worker: newTestWorker(st), maxBodySize: 1 << 20}

	req := httptest.NewRequest(http.MethodPost, "/api/workflows/d/start",
		strings.NewReader(`{"input":"just a string","entry_point":"place_order"}`))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	api.handleStartWorkflow(resp, req, "d")

	if resp.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400: %s", resp.Code, resp.Body.String())
	}
	var decoded map[string]string
	_ = json.Unmarshal(resp.Body.Bytes(), &decoded)
	if !strings.Contains(decoded["error"], "JSON object") {
		t.Errorf("refusal does not explain why: %q", decoded["error"])
	}
}

var _ engine.WorkflowStore = (*mockStore)(nil)
