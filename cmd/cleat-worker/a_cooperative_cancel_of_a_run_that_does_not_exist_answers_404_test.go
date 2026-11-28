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

// cleat#4503. With --require-auth=false nothing in front of the store reads the run before a cooperative cancel (callerOwnsTarget returns first when the request carries no tenant), so the
// store's answer for an id that matches no run is the client's answer. The stores used to say nil, and the handler said 200 "cancellation_requested" for a run that was not there; they now say
// engine.ErrWorkflowNotFound, and this is the half that turns it into 404, by both forms the handler can call. The store half is
// engine/a_cooperative_cancel_of_a_run_that_does_not_exist_is_not_found_test.go.

// refusingReceiptMock is a store whose receipt form answers a fixed error, so the handler's use of the receipt form is covered as well as the plain verb's.
type refusingReceiptMock struct {
	*mockStore
	err   error
	calls []string
}

func (m *refusingReceiptMock) CancelWorkflowReceipt(context.Context, string, string) (engine.SettleReceipt, error) {
	m.calls = append(m.calls, "cancel")
	return engine.SettleReceipt{}, m.err
}
func (m *refusingReceiptMock) TerminateWorkflowReceipt(context.Context, string, string) (engine.SettleReceipt, error) {
	m.calls = append(m.calls, "terminate")
	return engine.SettleReceipt{}, m.err
}
func (m *refusingReceiptMock) RequestCancellationReceipt(context.Context, string, string) (engine.SettleReceipt, error) {
	m.calls = append(m.calls, "request")
	return engine.SettleReceipt{}, m.err
}

func TestACooperativeCancelOfARunThatDoesNotExistAnswers404(t *testing.T) {
	notFound := fmt.Errorf("the store said: %w", engine.ErrWorkflowNotFound)

	t.Run("the plain verb", func(t *testing.T) {
		ms := &mockStore{}
		ms.requestCancellationFn = func(context.Context, string, string) error { return notFound }
		api := newTestAPIServer(ms)
		req := httptest.NewRequest(http.MethodPost, "/api/workflows/no-such-run/cancel", strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		api.handleCancel(w, req, "no-such-run")
		if w.Code != http.StatusNotFound {
			t.Fatalf("a cooperative cancel of a run that does not exist answered %d %s, want 404", w.Code, w.Body.String())
		}
		var body map[string]string
		_ = json.Unmarshal(w.Body.Bytes(), &body)
		if body["error"] != engine.ErrWorkflowNotFound.Error() {
			t.Errorf("the 404 says %q, want the sentinel's own text %q and nothing the store wrapped around it", body["error"], engine.ErrWorkflowNotFound.Error())
		}
	})

	t.Run("the receipt form", func(t *testing.T) {
		m := &refusingReceiptMock{mockStore: &mockStore{}, err: notFound}
		api := newTestAPIServer(m.mockStore)
		api.store = m
		api.factory = &fakeStoreFactory{fallback: m}
		mux := http.NewServeMux()
		registerRoutes(mux, api)
		req := httptest.NewRequest(http.MethodPost, "/api/workflows/no-such-run/cancel", strings.NewReader(`{}`))
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if len(m.calls) != 1 || m.calls[0] != "request" {
			t.Fatalf("the store was called %v, want [request]: the handler never used the receipt form, so this measures nothing", m.calls)
		}
		if w.Code != http.StatusNotFound {
			t.Fatalf("a cooperative cancel of a run that does not exist answered %d %s, want 404", w.Code, w.Body.String())
		}
	})

	t.Run("CONTROL: a store that accepts is still a 200", func(t *testing.T) {
		ms := &mockStore{}
		ms.requestCancellationFn = func(context.Context, string, string) error { return nil }
		api := newTestAPIServer(ms)
		req := httptest.NewRequest(http.MethodPost, "/api/workflows/wf-live/cancel", strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		api.handleCancel(w, req, "wf-live")
		if w.Code != http.StatusOK {
			t.Fatalf("an accepted cooperative cancel answered %d %s, want 200", w.Code, w.Body.String())
		}
	})
}
