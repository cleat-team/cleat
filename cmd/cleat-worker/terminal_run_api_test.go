package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

// TestAPITerminalRunIsASeparateRouteFromTheWorkflowItself is cleat#887's HTTP
// half.
//
// The two routes must answer DIFFERENTLY for a continued workflow, and that is
// the whole point: GET /api/workflows/:id returns the row named, and
// GET /api/workflows/:id/terminal follows the chain. A single route that
// followed the chain -- whether always (option B) or behind a query parameter
// -- would make one of these assertions unwritable.
func TestAPITerminalRunIsASeparateRouteFromTheWorkflowItself(t *testing.T) {
	const head, tail = "run-head", "run-tail"

	ms := &mockStore{}
	ms.getWorkflowByIDFn = func(_ context.Context, id string) (*engine.WorkflowInstance, error) {
		if id != head {
			return nil, nil
		}
		return &engine.WorkflowInstance{ID: head, Status: "done", Result: "{}"}, nil
	}
	ms.getTerminalRunFn = func(_ context.Context, id string) (*engine.WorkflowInstance, error) {
		if id != head {
			return nil, nil
		}
		return &engine.WorkflowInstance{
			ID: tail, Status: "done", Result: `{"outcome":"finished"}`, ContinuedFrom: "run-middle",
		}, nil
	}
	api := newTestAPIServer(ms)

	// The plain GET returns the row it names, with the empty result that is
	// exactly the symptom #826 reported.
	req := httptest.NewRequest(http.MethodGet, "/api/workflows/"+head, nil)
	w := httptest.NewRecorder()
	api.handleGetWorkflow(w, req, head)
	if w.Code != 200 {
		t.Fatalf("GET /api/workflows/%s: expected 200, got %d", head, w.Code)
	}
	var named engine.WorkflowInstance
	if err := json.NewDecoder(w.Body).Decode(&named); err != nil {
		t.Fatalf("decoding the plain GET: %v", err)
	}
	if named.ID != head {
		t.Errorf("GET /api/workflows/%s returned id %q; it must return the row it names",
			head, named.ID)
	}

	// The terminal route follows the chain.
	req = httptest.NewRequest(http.MethodGet, "/api/workflows/"+head+"/terminal", nil)
	w = httptest.NewRecorder()
	api.handleGetTerminalRun(w, req, head)
	if w.Code != 200 {
		t.Fatalf("GET /api/workflows/%s/terminal: expected 200, got %d", head, w.Code)
	}
	var term engine.WorkflowInstance
	if err := json.NewDecoder(w.Body).Decode(&term); err != nil {
		t.Fatalf("decoding the terminal GET: %v", err)
	}
	if term.ID != tail {
		t.Errorf("GET /api/workflows/%s/terminal returned id %q, want %q", head, term.ID, tail)
	}
	if term.Result != `{"outcome":"finished"}` {
		t.Errorf("the terminal route must carry the chain's real result, got %q", term.Result)
	}

	// continued_from must survive JSON, which is the half of #887 that makes
	// the chain visible in the admin UI. Asserted on the wire rather than on
	// the struct: the field is omitempty, and a mis-tagged field would be
	// invisible to a struct-level check that round-trips through the same tag.
	w = httptest.NewRecorder()
	api.handleGetTerminalRun(w, httptest.NewRequest(http.MethodGet, "/x", nil), head)
	var raw map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decoding raw JSON: %v", err)
	}
	if got, ok := raw["continued_from"]; !ok || got != "run-middle" {
		t.Errorf("response JSON has continued_from = %v (present=%v), want \"run-middle\". "+
			"The field is what makes a chain visible to anyone reading the API.", got, ok)
	}

	// THE ROUTE, not just the handler. Everything above calls
	// handleGetTerminalRun directly, which proves the handler works and says
	// nothing about whether any URL reaches it -- a handler nothing routes to
	// is the same defect as an export nothing calls. Go through handleWorkflows
	// so the switch in server.go is what selects it.
	req = httptest.NewRequest(http.MethodGet, "/api/workflows/"+head+"/terminal", nil)
	w = httptest.NewRecorder()
	api.handleWorkflows(w, req)
	if w.Code != 200 {
		t.Fatalf("routing GET /api/workflows/%s/terminal through handleWorkflows: got %d, "+
			"want 200 -- the handler exists but no URL reaches it", head, w.Code)
	}
	var routed engine.WorkflowInstance
	if err := json.NewDecoder(w.Body).Decode(&routed); err != nil {
		t.Fatalf("decoding the routed response: %v", err)
	}
	if routed.ID != tail {
		t.Errorf("the routed request returned id %q, want %q -- the URL reached something, "+
			"but not the terminal-run handler", routed.ID, tail)
	}

	// A POST to the same path must not reach it: the route is GET-only, and a
	// switch arm that ignored the method would answer writes with a read.
	w = httptest.NewRecorder()
	api.handleWorkflows(w, httptest.NewRequest(http.MethodPost, "/api/workflows/"+head+"/terminal", nil))
	if w.Code == 200 {
		t.Error("POST /api/workflows/:id/terminal was served; the route must be GET-only")
	}

	// An id naming nothing is a 404 on both routes, not a 200 with a null body.
	w = httptest.NewRecorder()
	api.handleGetTerminalRun(w, httptest.NewRequest(http.MethodGet, "/x", nil), "no-such-run")
	if w.Code != 404 {
		t.Errorf("GET terminal for an unknown id: expected 404, got %d", w.Code)
	}
}
