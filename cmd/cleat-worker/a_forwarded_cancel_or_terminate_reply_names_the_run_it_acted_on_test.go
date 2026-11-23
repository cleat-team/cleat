package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

// cleat#4340. A cancel or a terminate addressed to a run that has continued as new acts on the END of its chain, and the 200 says so: `workflow_id` is the run it acted on and
// `forwarded_from` the run the caller addressed. Both keys are absent when it acted where it was addressed, so a client that does not read them sees what it saw before. The store
// half is engine/a_cancel_or_terminate_to_a_continued_run_reaches_its_successor_test.go.

// settleReceiptMock is a store that can say which run a verb acted on, which the plain mockStore deliberately cannot (it keeps the plain-verb path covered).
type settleReceiptMock struct {
	*mockStore
	receipt engine.SettleReceipt
	calls   []string
}

func (m *settleReceiptMock) CancelWorkflowReceipt(context.Context, string, string) (engine.SettleReceipt, error) {
	m.calls = append(m.calls, "cancel")
	return m.receipt, nil
}
func (m *settleReceiptMock) TerminateWorkflowReceipt(context.Context, string, string) (engine.SettleReceipt, error) {
	m.calls = append(m.calls, "terminate")
	return m.receipt, nil
}
func (m *settleReceiptMock) RequestCancellationReceipt(context.Context, string, string) (engine.SettleReceipt, error) {
	m.calls = append(m.calls, "request")
	return m.receipt, nil
}

func TestAForwardedCancelOrTerminateReplyNamesTheRunItActedOn(t *testing.T) {
	cases := []struct{ name, path, body, wantStatus, wantCall string }{
		{"pre-emptive cancel", "/api/workflows/wf-old/cancel", `{"preemptive":true}`, "cancelled", "cancel"},
		{"cooperative cancel", "/api/workflows/wf-old/cancel", `{}`, "cancellation_requested", "request"},
		{"terminate", "/api/dead-letters/wf-old/terminate", ``, "terminated", "terminate"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			post := func(rec engine.SettleReceipt) (map[string]string, *settleReceiptMock) {
				m := &settleReceiptMock{mockStore: &mockStore{}, receipt: rec}
				api := newTestAPIServer(m.mockStore)
				api.store = m
				api.factory = &fakeStoreFactory{fallback: m}
				mux := http.NewServeMux()
				registerRoutes(mux, api)
				req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
				w := httptest.NewRecorder()
				mux.ServeHTTP(w, req)
				if w.Code != http.StatusOK {
					t.Fatalf("status %d, want 200; body %s", w.Code, w.Body.String())
				}
				var body map[string]string
				if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
					t.Fatalf("body is not JSON: %v: %s", err, w.Body.String())
				}
				return body, m
			}

			body, m := post(engine.SettleReceipt{SettledRun: "wf-end", Forwarded: true})
			if len(m.calls) != 1 || m.calls[0] != tc.wantCall {
				t.Fatalf("the store was called %v, want [%s]: the handler never used the receipt form, so this measures nothing", m.calls, tc.wantCall)
			}
			if body["status"] != tc.wantStatus || body["workflow_id"] != "wf-end" || body["forwarded_from"] != "wf-old" {
				t.Errorf("a forwarded reply is %v, want status=%s workflow_id=wf-end forwarded_from=wf-old", body, tc.wantStatus)
			}

			// CONTROL: acted where it was addressed, so the reply is what it always was.
			body, _ = post(engine.SettleReceipt{SettledRun: "wf-old"})
			if _, has := body["workflow_id"]; has {
				t.Errorf("an unforwarded reply carries workflow_id: %v", body)
			}
			if _, has := body["forwarded_from"]; has {
				t.Errorf("an unforwarded reply carries forwarded_from: %v", body)
			}
			if body["status"] != tc.wantStatus {
				t.Errorf("an unforwarded reply is %v, want status=%s", body, tc.wantStatus)
			}
		})
	}
}

// A cooperative cancel to a chain whose current run is closed for good (`done` or `cancelled`) is refused as the pre-emptive verbs are: 409 `state_conflict`, the answer a signal to it gets
// (the coordinator's ruling on cleat#4340). The handler used writeStoreError for this path, which has no 409 for it, so the store's refusal would have come out as a 500.
func TestACooperativeCancelToAClosedChainAnswers409StateConflictAndOtherErrorsStayWhatTheyWere(t *testing.T) {
	post := func(err error) (int, map[string]string) {
		ms := &mockStore{}
		ms.requestCancellationFn = func(context.Context, string, string) error { return err }
		api := newTestAPIServer(ms)
		req := httptest.NewRequest(http.MethodPost, "/api/workflows/wf-old/cancel", strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		api.handleCancel(w, req, "wf-old")
		var body map[string]string
		_ = json.Unmarshal(w.Body.Bytes(), &body)
		return w.Code, body
	}

	refusal := engine.ErrAdminStateConflict
	code, body := post(fmt.Errorf("workflow wf-old: continued as new, and its current run wf-end is already settled (status=done); refusing to write a cancellation request over it: %w", refusal))
	if code != http.StatusConflict || body["detail"] != "state_conflict" {
		t.Errorf("a closed chain's refusal answered %d %v, want 409 with detail=state_conflict", code, body)
	}
	if !strings.Contains(body["error"], "wf-end") {
		t.Errorf("the 409 does not name the run that refused (wf-end): %v", body)
	}

	// CONTROL: an unrelated store failure is still a 500 that does not carry the driver's text, as before; so the mapping above is not "every error is a 409".
	code, body = post(errors.New("driver: secret connection string"))
	if code != http.StatusInternalServerError {
		t.Errorf("an unrelated failure answered %d, want 500", code)
	}
	for _, v := range body {
		if strings.Contains(v, "secret connection string") {
			t.Errorf("the 500 leaks the driver's text: %v", body)
		}
	}
	// CONTROL: accepted still answers 200 cancellation_requested.
	code, body = post(nil)
	if code != http.StatusOK || body["status"] != "cancellation_requested" {
		t.Errorf("an accepted request answered %d %v, want 200 cancellation_requested", code, body)
	}
}
