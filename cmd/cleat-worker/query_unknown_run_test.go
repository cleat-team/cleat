package main

// cleat#900, applied to the sixth per-run read.
//
// /events, /history and /promises answered 200 with an empty COLLECTION for an
// id that did not exist; #917 made all three 404. /query answered 200 with an
// empty VALUE and was not among them, so the same defect survived in a
// different container.
//
// Found by the samples-go port in cleat-team/cleat-ports, which asserted 404
// here because #900 had settled the question for the other reads. It had never
// been asked of this one:
//
//	GET /api/workflows/00000000-.../query?key=counter
//	200 {"key":"counter","value":""}

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

func TestAQueryOnAnUnknownRunIsA404(t *testing.T) {
	queried := false
	ms := &mockStore{
		getWorkflowByIDFn: func(_ context.Context, _ string) (*engine.WorkflowInstance, error) {
			return nil, nil // no such run
		},
		getQueryStateFn: func(_ context.Context, _, _ string) (string, error) {
			queried = true
			return "", nil
		},
	}
	api := newTestAPIServer(ms)
	rec := httptest.NewRecorder()
	api.handleGetQueryState(rec,
		httptest.NewRequest(http.MethodGet, "/api/workflows/nope/query?key=counter", nil), "nope")

	if rec.Code != 404 {
		t.Errorf("a query on an unknown run answered %d, want 404: %s", rec.Code, rec.Body.String())
	}
	if queried {
		t.Error("the store was asked for the key anyway; the existence check must come first")
	}
}

// TestAnUnpublishedKeyOnARealRunIsStill200 is the control.
//
// Making an unknown RUN a 404 is only correct if an unknown KEY on a real run
// stays 200. Without this, a fix that 404'd whenever the value came back empty
// would pass the test above and break every caller polling for a key that has
// not been published yet -- which is the normal way this endpoint is used.
func TestAnUnpublishedKeyOnARealRunIsStill200(t *testing.T) {
	ms := existingWorkflow(&mockStore{})
	ms.getQueryStateFn = func(_ context.Context, _, _ string) (string, error) {
		return "", nil // published nothing under this key yet
	}
	api := newTestAPIServer(ms)
	rec := httptest.NewRecorder()
	api.handleGetQueryState(rec,
		httptest.NewRequest(http.MethodGet, "/api/workflows/wf-1/query?key=later", nil), "wf-1")

	if rec.Code != 200 {
		t.Fatalf("an unpublished key on a real run answered %d, want 200: %s",
			rec.Code, rec.Body.String())
	}
	var out map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("response was not a JSON object: %v", err)
	}
	if out["key"] != "later" || out["value"] != "" {
		t.Errorf("got %v, want key=later with an empty value", out)
	}
}
