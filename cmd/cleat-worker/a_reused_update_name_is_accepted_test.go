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

// A reused update name is ACCEPTED, and the pending guard is now the whole of
// the concurrency control. cleat#1416.
//
// This file is TestAReusedUpdateNameIsA409 inverted rather than deleted. That
// test was right for the schema it was written against: the primary key was
// (workflow_id, update_name), completion is an UPDATE rather than a delete, and
// so a name was spent for the life of the workflow. cleat#1392 mapped the
// resulting driver error to a clean 409 and predicted its own obsolescence --
// "the new 409 becomes unreachable rather than wrong". The name is now
// reusable, so the case is gone and its opposite is asserted here.
//
// WHAT REMAINS, and it is the distinction worth keeping straight:
//
//	update_already_pending   one request under this name is in flight.
//	                         Still refused, still 409. CLEARS when it is answered.
//	update_name_used         removed. There is no state a name can be in that
//	                         permanently refuses the next request.
//
// The pending guard filters `status = 'pending'`, so it covers exactly the
// window before dispatch -- which used to be a limitation, the primary key
// covering everything after it, and is now the entire rule.
func TestAReusedUpdateNameIsAccepted(t *testing.T) {
	for _, c := range []struct {
		name       string
		pending    []engine.UpdateRequestInfo
		createErr  error
		wantStatus int
		wantDetail string
	}{
		{
			// THE CHANGE. The store no longer refuses a name that has been used
			// and completed, so the handler must answer 202 -- there is no
			// pending row, and CreateUpdateRequest succeeds.
			//
			// This case is the previous "already used" row inverted. It read
			// createErr: ErrUpdateNameUsed, wantStatus: 409,
			// wantDetail: update_name_used.
			name:       "a name used and completed earlier is accepted again",
			wantStatus: 202,
		},
		{
			// The one refusal that survives, and the ONLY one now. It carries a
			// detail so a caller can tell it from any other 409 the API grows.
			name:       "already pending",
			pending:    []engine.UpdateRequestInfo{{UpdateName: "bump", Status: "pending"}},
			wantStatus: 409,
			wantDetail: "update_already_pending",
		},
		{
			// The control. Without it, a handler that answered 409 to
			// everything would pass both cases above.
			name:       "a first request still succeeds",
			wantStatus: 202,
		},
		{
			// And a store failure that is NOT this one must still be a 500,
			// or the mapping has swallowed real errors into a client-error
			// class.
			name:       "an unrelated store error is still a 500",
			createErr:  fmt.Errorf("connection refused"),
			wantStatus: 500,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			ms := &mockStore{
				getWorkflowByIDFn: func(_ context.Context, id string) (*engine.WorkflowInstance, error) {
					return &engine.WorkflowInstance{ID: id, Status: "running"}, nil
				},
				getPendingUpdateRequestsFn: func(_ context.Context, _ string) ([]engine.UpdateRequestInfo, error) {
					return c.pending, nil
				},
				createUpdateRequestFn: func(_ context.Context, _, _, _, _ string) error {
					return c.createErr
				},
			}
			api := newTestAPIServer(ms)
			rec := httptest.NewRecorder()
			api.handleWorkflows(rec, httptest.NewRequest(http.MethodPost,
				"/api/workflows/wf-1/update/bump", strings.NewReader(`{"by":1}`)))

			if rec.Code != c.wantStatus {
				t.Fatalf("status = %d, want %d: %s", rec.Code, c.wantStatus, rec.Body.String())
			}
			if c.wantDetail == "" {
				return
			}
			var body map[string]string
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("response is not a JSON object: %q", rec.Body.String())
			}
			if body["detail"] == "" {
				t.Fatalf("the 409 carries no detail: %q.\n\n"+
					"A caller that learned to branch on detail under cleat#1330 keeps working "+
					"only if the field is still there. The message text is not a contract; "+
					"the detail is.", rec.Body.String())
			}
			if body["detail"] != c.wantDetail {
				t.Errorf("detail = %q, want %q", body["detail"], c.wantDetail)
			}
			if strings.Contains(rec.Body.String(), "pq:") || strings.Contains(rec.Body.String(), "constraint") {
				t.Errorf("the response leaks driver text: %q", rec.Body.String())
			}
		})
	}
}
