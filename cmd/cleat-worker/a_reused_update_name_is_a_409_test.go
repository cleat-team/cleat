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

// The pending guard in handleWorkflowUpdate filters `status = 'pending'`, so it
// covers only the window before dispatch. Once the update has COMPLETED the
// guard passes and the insert runs into the primary key -- and that is the
// COMMON case, because a caller retrying is far more likely to do so after the
// first finished than during the pending window.
//
// The answer was `500 {"error":"pq: duplicate key value violates unique
// constraint \"workflow_update_requests_pkey\" (23505)"}` -- a driver string
// in a server-error class, for a well-formed request the state refuses, which
// is what the sibling 409 two blocks above is for. cleat#1330.
func TestAReusedUpdateNameIsA409(t *testing.T) {
	for _, c := range []struct {
		name       string
		pending    []engine.UpdateRequestInfo
		createErr  error
		wantStatus int
		wantDetail string
	}{
		{
			name:       "already used",
			createErr:  fmt.Errorf("%w: bump", engine.ErrUpdateNameUsed),
			wantStatus: 409,
			wantDetail: "update_name_used",
		},
		{
			// The sibling condition, which already answered 409 but carried no
			// detail -- so a caller could not tell "wait for the in-flight one"
			// from "this name is spent for the life of this workflow".
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
					"Both conditions answer 409 and they are not the same thing -- one clears "+
					"when the in-flight update finishes, the other never does. A caller "+
					"branching on status alone cannot tell them apart.", rec.Body.String())
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
