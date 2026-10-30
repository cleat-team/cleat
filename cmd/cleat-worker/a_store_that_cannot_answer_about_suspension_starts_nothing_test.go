package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/plugin"
)

// cleat#3552. Both start paths used to ask "is this tenant suspended" with a bare type assertion and skip the
// check when the store failed it, so a store that could not answer started work for a suspended tenant
// without a word. They now require a store to be a reader or to declare engine.NoTenantSuspensionMarker, and
// refuse one that is neither.

// unansweringStore is a mockStore that is neither. Embedding the INTERFACE rather than *mockStore is what
// keeps mockStore's NoTenantSuspension out of this type's method set: a promoted method would make the type
// opt out, and the tests below would be asserting against a store that answers.
type unansweringStore struct{ engine.WorkflowStore }

func TestAStoreThatCannotAnswerAboutSuspensionStartsNothingOverHTTP(t *testing.T) {
	startWith := func(t *testing.T, st engine.WorkflowStore, mock *mockStore) (code int, msg string) {
		t.Helper()
		api := &apiServer{store: st, worker: newTestWorker(mock), maxBodySize: 1 << 20}
		req := httptest.NewRequest(http.MethodPost, "/api/workflows/d/start", strings.NewReader(`{"input":{}}`))
		req.Header.Set("Content-Type", "application/json")
		resp := httptest.NewRecorder()
		api.handleStartWorkflow(resp, req, "d")
		var decoded map[string]string
		_ = json.Unmarshal(resp.Body.Bytes(), &decoded)
		return resp.Code, decoded["error"]
	}

	// The control comes first and differs only in the declaration. Without it the refusal below passes against a
	// handler that refuses every start.
	t.Run("control: a store that opts out starts normally", func(t *testing.T) {
		mock := &mockStore{}
		code, msg := startWith(t, mock, mock)
		if code != http.StatusCreated {
			t.Errorf("got %d %q; want 201", code, msg)
		}
	})

	t.Run("a store that neither answers nor opts out is refused and creates nothing", func(t *testing.T) {
		created := 0
		mock := &mockStore{startNewRunFn: func(context.Context, string, string, int, json.RawMessage, string, string, int) (string, bool, error) {
			created++
			return "run-1", false, nil
		}}
		code, msg := startWith(t, unansweringStore{mock}, mock)
		if code != http.StatusInternalServerError {
			t.Errorf("got %d %q; want 500 -- a store that cannot say must not be treated as not suspended", code, msg)
		}
		if !strings.Contains(msg, "unansweringStore") {
			t.Errorf("the refusal %q does not name the store type, so an operator cannot tell which store to fix", msg)
		}
		if created != 0 {
			t.Errorf("%d run(s) were created for a tenant the store could not vouch for", created)
		}
	})
}

func TestAStoreThatCannotAnswerAboutSuspensionStartsNothingFromAPlugin(t *testing.T) {
	start := func(st engine.WorkflowStore, created *int, mock *mockStore) error {
		mock.startNewRunFn = func(context.Context, string, string, int, json.RawMessage, string, string, int) (string, bool, error) {
			*created++
			return "run-1", false, nil
		}
		_, err := startPluginWorkflow(context.Background(), st, plugin.StartRequest{
			DefName: "d", Input: json.RawMessage(`{}`), IdempotencyKey: "k", TenantID: uuid.NewString(),
		})
		return err
	}

	t.Run("control: a store that opts out starts normally", func(t *testing.T) {
		created := 0
		mock := &mockStore{}
		if err := start(mock, &created, mock); err != nil || created != 1 {
			t.Errorf("err = %v, created = %d; want nil and 1", err, created)
		}
	})

	t.Run("a store that neither answers nor opts out is refused and creates nothing", func(t *testing.T) {
		created := 0
		mock := &mockStore{}
		err := start(unansweringStore{mock}, &created, mock)
		if !errors.Is(err, engine.ErrTenantSuspensionUnanswerable) {
			t.Errorf("err = %v; want engine.ErrTenantSuspensionUnanswerable", err)
		}
		if created != 0 {
			t.Errorf("%d run(s) were created for a tenant the store could not vouch for", created)
		}
	})
}
