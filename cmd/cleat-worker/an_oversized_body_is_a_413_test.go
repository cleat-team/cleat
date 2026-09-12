package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
)

// Seven handlers bounded their request body and never translated
// *http.MaxBytesError, so an oversized body came back as
//
//	400 {"error":"invalid JSON: http: request body too large"}
//
// -- a status asserting the body was malformed, on a body that was never read,
// with the real reason buried inside a string that says the opposite. Nine
// other sites returned 413 for the identical condition, so the API's surprise
// was the inconsistency rather than any one endpoint. cleat#1338.
//
// The eighth, handleCreateDefinition, is the one with a real client story: the
// WASM upload, which said "body too large" *under a 400* -- message and status
// contradicting each other in one response.
//
// EACH CASE CARRIES ITS OWN CONTROL, and that is the part that makes this
// worth more than "not 200". A small body on the same route with the same
// headers must reach the body read and be refused for its CONTENT, with a
// 400. Without it, an endpoint that 413s because of a route typo, a missing
// X-Confirm, or an auth failure would pass every assertion here.
func TestAnOversizedBodyIsA413(t *testing.T) {
	enabled := true
	old := enableAdminAPI
	enableAdminAPI = &enabled
	defer func() { enableAdminAPI = old }()

	const limit = 4096

	for _, c := range []struct {
		name    string
		method  string
		path    string
		confirm string
		// wantKnob is the sentence the 413 must carry, so a caller learns
		// which ceiling it hit and what moves it. cleat#1332.
		wantKnob string
		limit    int64
	}{
		{"set routing rule", http.MethodPost, "/api/workflows/d/routing", "",
			"set by --max-body-size", limit},
		{"set workflow tag", http.MethodPut, "/api/workflows/d/tags", "",
			"set by --max-body-size", limit},
		{"create definition", http.MethodPost, "/api/definitions", "",
			"fixed for the definition upload endpoint", definitionMaxBodySize},
		{"admin force-complete", http.MethodPost, "/api/admin/instances/wf-1/force-complete",
			"force-complete", "set by --max-body-size", limit},
		{"admin force-fail", http.MethodPost, "/api/admin/instances/wf-1/force-fail",
			"force-fail", "set by --max-body-size", limit},
		// The step travels in the path: /steps/{n}/resolve, not /resolve-step.
		{"admin resolve-step", http.MethodPost, "/api/admin/instances/wf-1/steps/0/resolve",
			"resolve-step", "set by --max-body-size", limit},
		{"admin re-replay", http.MethodPost, "/api/admin/instances/wf-1/re-replay",
			"re-replay", "set by --max-body-size", limit},
	} {
		t.Run(c.name, func(t *testing.T) {
			call := func(body string) (int, string) {
				t.Helper()
				ms := &mockStore{
					getWorkflowByIDFn: func(_ context.Context, id string) (*engine.WorkflowInstance, error) {
						return &engine.WorkflowInstance{
							ID: id, DefName: "d", Status: "running", CreatedAt: time.Now()}, nil
					},
					// /routing and /tags are name-scoped and 404 through
					// defExists before the body is read (cleat#942), so the
					// definition has to exist or the control below reports a
					// route that never reaches its body.
					listWorkflowDefsFn: func(_ context.Context, name string) ([]engine.WorkflowDef, error) {
						return []engine.WorkflowDef{{Name: name, Version: 1}}, nil
					},
				}
				api := newTestAPIServer(ms)
				api.maxBodySize = limit
				mux := http.NewServeMux()
				registerRoutes(mux, api)

				req := httptest.NewRequest(c.method, c.path, strings.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				if c.confirm != "" {
					req.Header.Set("X-Confirm", c.confirm)
				}
				w := httptest.NewRecorder()
				mux.ServeHTTP(w, req)
				return w.Code, w.Body.String()
			}

			// The control first: a SMALL body that is not valid JSON. Reaching
			// a 400 that names the parse proves the request got past routing,
			// admission and X-Confirm, and that the body was actually read.
			if code, body := call(`{not json`); code != 400 || !strings.Contains(body, "invalid JSON") {
				t.Fatalf("PRECONDITION FAILED: a small malformed body gave %d %q, want 400 naming "+
					"the parse. This endpoint is not reaching its body read, so a 413 below "+
					"would not be about the size of anything.", code, body)
			}

			// Sized from THIS endpoint's ceiling. A single fixed body sized
			// for 4 KB sails straight through the 10 MiB definition upload and
			// is refused later for its contents, which reads as a missing 413.
			// That happened on the first run.
			code, body := call(`{"x":"` + strings.Repeat("y", int(c.limit)+1024) + `"}`)
			if code != http.StatusRequestEntityTooLarge {
				t.Errorf("an oversized body gave %d %q, want 413.\n\n"+
					"A 400 here tells a client the request was malformed; it was not read at "+
					"all. A client branching on status has no way to learn to retry smaller.",
					code, body)
			}
			if !strings.Contains(body, c.wantKnob) {
				t.Errorf("the 413 body %q does not say %q, so the caller learns a number but not "+
					"which ceiling it is or what moves it", body, c.wantKnob)
			}
			if !strings.Contains(body, "request body too large") {
				t.Errorf("the 413 body %q does not say what went wrong", body)
			}
		})
	}
}
