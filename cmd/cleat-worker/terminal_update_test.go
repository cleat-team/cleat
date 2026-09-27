package main

// cleat#910: an update against a workflow that has already finished is refused
// rather than accepted with a promise that cannot settle.
//
// An update is delivered at a dispatch point inside a segment. A terminal
// workflow has no future segment, so a request created after it finished can
// never be delivered -- and nothing sweeps it: failStrandedUpdates runs AS a
// workflow goes terminal, so a request created afterwards is collected by
// nothing. The caller held a 202 and a promise id forever.
//
// That is cleat#849's original complaint in residual form. The scheduling half
// was fixed; accepting a request that provably cannot be delivered was not.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

func updateTestStore(status string, created *bool) *mockStore {
	return &mockStore{
		getWorkflowByIDFn: func(_ context.Context, id string) (*engine.WorkflowInstance, error) {
			return &engine.WorkflowInstance{ID: id, Status: status}, nil
		},
		createUpdateRequestFn: func(_ context.Context, _, _, _, _ string) error {
			if created != nil {
				*created = true
			}
			return nil
		},
	}
}

func TestAnUpdateAgainstATerminalWorkflowIsRefused(t *testing.T) {
	// Every status in which no further guest code can run. 'terminating' is
	// included because the defer phase refuses new work -- see isTerminalStatus.
	for _, status := range []string{"done", "failed", "terminated", "dead_lettered", "terminating"} {
		t.Run(status, func(t *testing.T) {
			created := false
			api := newTestAPIServer(updateTestStore(status, &created))
			rec := httptest.NewRecorder()
			api.handleWorkflows(rec, httptest.NewRequest(http.MethodPost,
				"/api/workflows/wf-1/update/bump", strings.NewReader(`{"by":1}`)))

			if rec.Code != 409 {
				t.Errorf("an update against a %s workflow answered %d, want 409: %s\n\n"+
					"202 means a promise was created that can never settle: there is no "+
					"future segment to deliver in, and failStrandedUpdates has already "+
					"run, so nothing will ever reject it.",
					status, rec.Code, rec.Body.String())
			}
			if created {
				t.Errorf("the request was written for a %s workflow; the refusal must come "+
					"before the insert or the unsettleable promise exists anyway", status)
			}
		})
	}
}

// TestAnUpdateAgainstARunningWorkflowIsStillAccepted is the control, and it is
// what stops the check above from becoming "refuse everything". A running
// workflow is the entire point of the feature.
func TestAnUpdateAgainstARunningWorkflowIsStillAccepted(t *testing.T) {
	for _, status := range []string{"running", "ready"} {
		t.Run(status, func(t *testing.T) {
			api := newTestAPIServer(updateTestStore(status, nil))
			rec := httptest.NewRecorder()
			api.handleWorkflows(rec, httptest.NewRequest(http.MethodPost,
				"/api/workflows/wf-1/update/bump", strings.NewReader(`{"by":1}`)))

			if rec.Code != 202 {
				t.Errorf("an update against a %s workflow answered %d, want 202: %s",
					status, rec.Code, rec.Body.String())
			}
		})
	}
}
