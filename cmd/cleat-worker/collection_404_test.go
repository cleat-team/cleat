package main

// cleat#900: a collection under a run answers the same way the run itself does.
//
// Before this, GET /api/workflows/{id} and /api/instances/{id}/state answered
// 404 for an id naming no workflow, while /events, /history and /promises
// answered 200 with an empty array. A caller could not tell "this run has no
// events" from "this run does not exist", and a typo in an id looked like a
// healthy empty result.
//
// That mattered more here than in a typical REST API: an empty collection is a
// NORMAL state in cleat, because event history is buffered within a segment and
// purged at completion, so a legitimately finished run also reports []. The
// empty response already meant three things with nothing to separate them.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

// TestACollectionUnderAnUnknownRunIs404 drives the three endpoints through
// their routers rather than calling the handlers, because the defect this
// covers was partly about which router served what: cleat#830 had seven of
// these registered on a table the binary never used, and they answered the
// SPA's HTML fallback with 200.
func TestACollectionUnderAnUnknownRunIs404(t *testing.T) {
	for _, tc := range []struct {
		name  string
		path  string
		route func(*apiServer, http.ResponseWriter, *http.Request)
	}{
		{"history", "/api/workflows/nope/history",
			func(a *apiServer, w http.ResponseWriter, r *http.Request) { a.handleWorkflows(w, r) }},
		{"promises", "/api/workflows/nope/promises",
			func(a *apiServer, w http.ResponseWriter, r *http.Request) { a.handleWorkflows(w, r) }},
		{"events", "/api/instances/nope/events",
			func(a *apiServer, w http.ResponseWriter, r *http.Request) { a.handleInstancesRoutes(w, r) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The zero mockStore returns (nil, nil) from GetWorkflowByID, which
			// is "no such workflow" -- the same shape a real store returns.
			api := newTestAPIServer(&mockStore{})
			rec := httptest.NewRecorder()
			tc.route(api, rec, httptest.NewRequest(http.MethodGet, tc.path, nil))

			if rec.Code != 404 {
				t.Errorf("%s answered %d for a run that does not exist: %s\n\n"+
					"200 means the collection was served without checking the run "+
					"exists, so a caller cannot tell an empty run from a missing "+
					"one (cleat#900). It also means a typo in an id reads as a "+
					"healthy empty result.",
					tc.path, rec.Code, rec.Body.String())
			}
		})
	}
}

// TestACollectionUnderAKnownRunIsStillServed is the other direction, and the
// one that stops the fix above from being "404 everything". A run that exists
// with no events must still answer 200 with an empty array -- that is the
// normal state for a completed workflow, whose history is purged.
func TestACollectionUnderAKnownRunIsStillServed(t *testing.T) {
	ms := &mockStore{
		getWorkflowByIDFn: func(_ context.Context, id string) (*engine.WorkflowInstance, error) {
			return &engine.WorkflowInstance{ID: id, Status: "done"}, nil
		},
	}
	api := newTestAPIServer(ms)
	rec := httptest.NewRecorder()
	api.handleWorkflows(rec, httptest.NewRequest(http.MethodGet, "/api/workflows/wf-1/promises", nil))

	if rec.Code != 200 {
		t.Errorf("a collection under a run that DOES exist answered %d: %s\n\n"+
			"An empty collection is normal -- history is purged at completion -- "+
			"so this must not become 404 along with the unknown-run case.",
			rec.Code, rec.Body.String())
	}
}
