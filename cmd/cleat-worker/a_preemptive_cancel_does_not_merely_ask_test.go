package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// cleat#1153 at the HTTP boundary: POST /api/workflows/:id/cancel takes a
// "preemptive" flag, and the two modes must reach DIFFERENT store verbs.
//
// WHY BOTH ARMS, AND WHY THEY ASSERT WHICH VERB RAN. The cheap version of this
// test checks the response string, which both arms would pass against a handler
// that always called RequestCancellation and merely printed a different word.
// The response is the symptom; the verb is the behaviour. So each arm records
// which store method was invoked and requires the other NOT to have been.
//
// THE DEFAULT IS THE COMPATIBILITY GUARANTEE. An existing client sends
// {"reason": "..."} with no flag and must still get the cooperative path and
// the "cancellation_requested" it has always parsed. Making pre-emption the
// default would silently convert every such caller into a forceful one.
func TestAPreemptiveCancelDoesNotMerelyAsk(t *testing.T) {
	for _, tc := range []struct {
		name          string
		body          string
		wantStatus    string
		wantPreempted bool
	}{
		{"absent flag stays cooperative", `{"reason":"user requested"}`, "cancellation_requested", false},
		{"explicit false stays cooperative", `{"reason":"r","preemptive":false}`, "cancellation_requested", false},
		{"preemptive stops the run", `{"reason":"r","preemptive":true}`, "cancelled", true},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			var asked, stopped bool
			ms := &mockStore{}
			ms.requestCancellationFn = func(ctx context.Context, workflowID, reason string) error {
				asked = true
				return nil
			}
			ms.cancelWorkflowFn = func(ctx context.Context, workflowID, reason string) error {
				stopped = true
				return nil
			}

			api := newTestAPIServer(ms)
			req := httptest.NewRequest(http.MethodPost, "/api/workflows/wf-1/cancel", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			api.handleCancel(w, req, "wf-1")

			if w.Code != 200 {
				t.Fatalf("status = %d, want 200", w.Code)
			}
			var resp map[string]string
			if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if resp["status"] != tc.wantStatus {
				t.Errorf("status = %q, want %q", resp["status"], tc.wantStatus)
			}

			// The verb, not the word. This is what a handler that only changed
			// its response string would fail.
			if stopped != tc.wantPreempted {
				t.Errorf("CancelWorkflow called = %v, want %v -- the response said %q, so the "+
					"reply and the behaviour disagree", stopped, tc.wantPreempted, resp["status"])
			}
			if asked == tc.wantPreempted {
				t.Errorf("RequestCancellation called = %v with preemptive=%v -- the two modes "+
					"must reach different store verbs, and exactly one of them",
					asked, tc.wantPreempted)
			}
		})
	}
}
