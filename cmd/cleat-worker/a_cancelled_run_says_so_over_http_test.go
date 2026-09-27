package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

// TestGETWorkflowReportsCancellation.
//
// The store half of cleat#1351 is asserted in engine/ across all three
// dialects. This asserts the half that was actually broken FOR THE OPERATOR:
// that the fields reach the HTTP response.
//
// It is a separate test rather than a line in the engine one because the two
// can fail independently, and the failure that prompted the issue lives here.
// `handleGetWorkflow` ends in `s.writeJSON(w, 200, wf)` -- it serialises
// engine.WorkflowInstance whole -- so a field added to that struct appears in
// the response with no handler change. That is a real property and it is also
// exactly the kind of reasoning CLAUDE.md records two sessions getting wrong in
// the other direction, when both concluded `error_code` was never returned
// because they enumerated response structs in cmd/cleat-worker/ and the field
// was one package away. Reading the handler tells you what should happen;
// decoding the body tells you what does.
func TestGETWorkflowReportsCancellation(t *testing.T) {
	const reason = "INCIDENT-4242 operator cancelled, duplicate submission"

	for _, tc := range []struct {
		name       string
		run        engine.WorkflowInstance
		wantFlag   bool
		wantReason string
		reasonKey  bool // is the key expected to be present at all?
	}{
		{
			name:      "a run nobody cancelled",
			run:       engine.WorkflowInstance{Status: "ready"},
			wantFlag:  false,
			reasonKey: false,
		},
		{
			name: "cancelled with a reason",
			run: engine.WorkflowInstance{
				Status:                "ready",
				CancellationRequested: true,
				CancellationReason:    reason,
			},
			wantFlag:   true,
			wantReason: reason,
			reasonKey:  true,
		},
		{
			name: "cancelled with no reason given",
			run: engine.WorkflowInstance{
				Status:                "ready",
				CancellationRequested: true,
			},
			wantFlag:  true,
			reasonKey: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := tc.run
			ms := &mockStore{}
			ms.getWorkflowByIDFn = func(_ context.Context, id string) (*engine.WorkflowInstance, error) {
				w := run
				w.ID = id
				return &w, nil
			}
			api := newTestAPIServer(ms)

			req := httptest.NewRequest(http.MethodGet, "/api/workflows/wf-cancel-http", nil)
			rec := httptest.NewRecorder()
			api.handleWorkflows(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("GET returned %d, body: %s", rec.Code, rec.Body.String())
			}

			// Decoded into a map, not into WorkflowInstance: unmarshalling back
			// into the same struct would pass even if the json tags were wrong,
			// since the round trip would agree with itself. The question is what
			// a CLIENT sees, and a client sees keys.
			var body map[string]any
			if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
				t.Fatalf("decode: %v", err)
			}

			flag, present := body["cancellation_requested"]
			if !present {
				t.Fatalf("the response has no cancellation_requested key at all.\n\n"+
					"An operator who cancels a run has to be able to confirm the "+
					"workflow was told; cleat's cancellation is cooperative, so "+
					"\"cancelled but still running\" is a normal state and status "+
					"alone cannot show it (cleat#1351).\nbody: %v", body)
			}
			if flag != tc.wantFlag {
				t.Errorf("cancellation_requested = %v, want %v", flag, tc.wantFlag)
			}

			got, hasReason := body["cancellation_reason"]
			if hasReason != tc.reasonKey {
				t.Errorf("cancellation_reason present = %v, want %v (body: %v)",
					hasReason, tc.reasonKey, body)
			}
			if tc.reasonKey && got != tc.wantReason {
				t.Errorf("cancellation_reason = %q, want %q", got, tc.wantReason)
			}
		})
	}
}
