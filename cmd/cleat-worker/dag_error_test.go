package main

// cleat#899 follow-up: a DAG load FAILURE is not the same answer as no DAG.
//
// handleGetDAG reported a LoadDAGSpec error as 404 with the raw error text,
// alongside the legitimate 404 for a workflow with no dag_spec. It was the only
// error path in server.go answering anything but 500; the other twelve handlers
// already distinguished them.
//
// Why it mattered rather than being a tidiness point:
// web/src/pages/WorkflowDetail.svelte fetches the DAG on EVERY workflow detail
// view, inside a try whose comment reads "silently ignore if not available".
// So while a store failure answered 404, a connection loss or permission error
// reached a user as an empty panel and nothing else.
//
// #899 fixed the cause -- LoadDAGSpec no longer errors on the NULL dag_spec
// every non-DAG workflow has. This fixes the erasure above it, which is what
// makes the dashboard's swallow correct rather than currently harmless.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

func dagTestStore(loadErr error, spec json.RawMessage) *mockStore {
	return &mockStore{
		getWorkflowByIDFn: func(_ context.Context, id string) (*engine.WorkflowInstance, error) {
			return &engine.WorkflowInstance{ID: id, DefName: "wf", DefVersion: 1}, nil
		},
		loadDAGSpecFn: func(_ context.Context, _ string, _ int) (json.RawMessage, error) {
			return spec, loadErr
		},
	}
}

func TestADAGLoadFailureIsAServerErrorNotA404(t *testing.T) {
	api := newTestAPIServer(dagTestStore(errors.New("connection reset by peer"), nil))
	rec := httptest.NewRecorder()
	api.handleWorkflows(rec, httptest.NewRequest(http.MethodGet, "/api/workflows/wf-1/dag", nil))

	if rec.Code != 500 {
		t.Errorf("a store failure answered %d, want 500: %s\n\n"+
			"404 here is indistinguishable from 'this workflow has no DAG', which "+
			"the dashboard displays as nothing at all -- so a connection loss "+
			"reaches a user as an empty panel.", rec.Code, rec.Body.String())
	}
}

// TestAMissingDAGSpecIsStillA404 is the control, and it is what stops the fix
// above from becoming "500 everything". A workflow with no dag_spec is the
// overwhelmingly common case -- every workflow that is not a DAG -- and it must
// stay a clean 404 that the dashboard can go on ignoring.
func TestAMissingDAGSpecIsStillA404(t *testing.T) {
	api := newTestAPIServer(dagTestStore(nil, nil))
	rec := httptest.NewRecorder()
	api.handleWorkflows(rec, httptest.NewRequest(http.MethodGet, "/api/workflows/wf-1/dag", nil))

	if rec.Code != 404 {
		t.Errorf("a workflow with no DAG answered %d, want 404: %s", rec.Code, rec.Body.String())
	}
}
