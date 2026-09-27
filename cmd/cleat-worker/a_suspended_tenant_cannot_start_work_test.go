package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
)

// suspendableStore is a mockStore that also answers IsTenantSuspended.
type suspendableStore struct {
	*mockStore
	suspended bool
	err       error
	asked     int
}

func (s *suspendableStore) IsTenantSuspended(context.Context, string) (bool, error) {
	s.asked++
	if s.err != nil {
		return false, s.err
	}
	return s.suspended, nil
}

func startAgainst(t *testing.T, st engine.WorkflowStore) (int, string) {
	t.Helper()
	ms, ok := st.(*suspendableStore)
	if !ok {
		t.Fatalf("unexpected store type %T", st)
	}
	api := &apiServer{store: st, worker: newTestWorker(ms.mockStore), maxBodySize: 1 << 20}
	req := httptest.NewRequest(http.MethodPost, "/api/workflows/d/start", strings.NewReader(`{"input":{}}`))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	api.handleStartWorkflow(resp, req, "d")
	var decoded map[string]string
	_ = json.Unmarshal(resp.Body.Bytes(), &decoded)
	return resp.Code, decoded["error"]
}

// A suspended tenant cannot start new work, and the refusal is a 403.
//
// 403 rather than 503 because this is a decision about the caller rather than
// a condition of the server: a retry will not help until an operator resumes
// the tenant. The draining and memory-pressure branches in the same handler
// are 503 for exactly the opposite reason, and a client that backs off and
// retries on one should not do so on the other.
func TestASuspendedTenantCannotStartWork(t *testing.T) {
	t.Run("suspended is refused", func(t *testing.T) {
		st := &suspendableStore{mockStore: &mockStore{}, suspended: true}
		code, msg := startAgainst(t, st)
		if code != http.StatusForbidden {
			t.Errorf("got %d %q; want 403", code, msg)
		}
		if !strings.Contains(msg, "suspended") {
			t.Errorf("the refusal does not say why: %q", msg)
		}
		// It must also say what suspension does NOT do, because the first
		// question an operator asks on seeing this is whether their in-flight
		// runs just died.
		if !strings.Contains(msg, "finish") {
			t.Errorf("the refusal does not say that running work is unaffected: %q", msg)
		}
	})

	t.Run("not suspended starts normally", func(t *testing.T) {
		// The control. Without it the assertion above passes against a handler
		// that refuses every start.
		st := &suspendableStore{mockStore: &mockStore{}, suspended: false}
		code, msg := startAgainst(t, st)
		if code != http.StatusCreated {
			t.Errorf("got %d %q; want 201", code, msg)
		}
		if st.asked == 0 {
			t.Error("the handler never asked whether the tenant was suspended, so the " +
				"refusal above may not be reached by this path at all")
		}
	})

	t.Run("an error reading the flag is not silently a pass", func(t *testing.T) {
		// Failing open here would make suspension advisory: a database blip
		// would let a suspended tenant start work, and nothing would say so.
		st := &suspendableStore{mockStore: &mockStore{}, err: errors.New("db down")}
		code, _ := startAgainst(t, st)
		if code == http.StatusCreated {
			t.Error("a start succeeded while the suspension check was failing; " +
				"suspension must not fail open")
		}
	})
}

// Suspension does not block reads.
//
// Deliberate, and worth a test because "suspended" reads like "locked out".
// A tenant suspended for non-payment should still be able to see its own runs
// and history; blocking that punishes the wrong thing and makes the state
// harder to reason about rather than easier.
func TestASuspendedTenantCanStillRead(t *testing.T) {
	st := &suspendableStore{mockStore: &mockStore{
		getWorkflowByIDFn: func(_ context.Context, id string) (*engine.WorkflowInstance, error) {
			return &engine.WorkflowInstance{ID: id, DefName: "d", Status: "running", CreatedAt: time.Now()}, nil
		},
	}, suspended: true}

	api := &apiServer{store: st, worker: newTestWorker(st.mockStore), maxBodySize: 1 << 20}
	req := httptest.NewRequest(http.MethodGet, "/api/workflows/wf-1", nil)
	resp := httptest.NewRecorder()
	api.handleGetWorkflow(resp, req, "wf-1")

	if resp.Code != http.StatusOK {
		t.Errorf("reading a workflow under a suspended tenant returned %d; want 200. "+
			"Suspension stops new work, not visibility.", resp.Code)
	}
}
