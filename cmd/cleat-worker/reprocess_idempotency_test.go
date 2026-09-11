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

// cleat#1167: POST /api/dead-letters/{id}/reprocess created a new run on every
// call, so a lost response followed by a retry re-drove the same failed work
// twice. The work being re-driven has already failed partway, so the duplicate
// repeats whatever partial side effects it left.
//
// Why the tests that already existed did not catch it, and could not:
// TestHandleDeadLetterReprocess_AlreadyExisted asserts the 200/already_started
// branch renders, using a startNewRunFn that returns alreadyExisted=true
// UNCONDITIONALLY and ignores its idempotencyKey argument, from a request that
// sets no header. It passes unchanged on the code that hard-codes "" into that
// argument -- that is, on code where the branch it asserts is unreachable.
// TestAPIStartWorkflow_WithIdempotencyKey had the same defect: replacing the
// start handler's header read with the empty string leaves it green.
//
// keyedRunStarter is the missing piece. It models what the store actually
// guarantees -- idempotency_keys is PRIMARY KEY (key_hash, tenant_id), so a
// key that has been seen returns the ORIGINAL run and reports alreadyExisted
// -- and it records every key it was handed, so a handler that drops the
// header fails these tests instead of sailing through them.
type keyedRunStarter struct {
	issued map[string]string // idempotency key -> the run it first created
	n      int
	seen   []string // every key the handler passed down, in order
}

func newKeyedRunStarter() *keyedRunStarter {
	return &keyedRunStarter{issued: map[string]string{}}
}

func (k *keyedRunStarter) start(_ context.Context, _, _ string, _ int, _ json.RawMessage, idempotencyKey, _ string, _ int) (string, bool, error) {
	k.seen = append(k.seen, idempotencyKey)
	// An empty key is the absence of a token, not a token whose value is "".
	// It must never collide with another caller who also sent nothing.
	if idempotencyKey != "" {
		if first, ok := k.issued[idempotencyKey]; ok {
			return first, true, nil
		}
	}
	k.n++
	runID := fmt.Sprintf("run-%d", k.n)
	if idempotencyKey != "" {
		k.issued[idempotencyKey] = runID
	}
	return runID, false, nil
}

// deadLetteredStore returns one dead-lettered workflow for any id, which is
// what both reprocess paths below need to get past their preconditions.
func deadLetteredStore(starter *keyedRunStarter) *mockStore {
	ms := &mockStore{}
	ms.getWorkflowByIDFn = func(_ context.Context, id string) (*engine.WorkflowInstance, error) {
		return &engine.WorkflowInstance{
			ID: id, DefName: "test-def", DefVersion: 1, Status: "dead_lettered",
			Input: json.RawMessage(`{}`),
		}, nil
	}
	ms.listVersionsFn = func(_ context.Context, _ string) ([]int, error) { return []int{1}, nil }
	ms.startNewRunFn = starter.start
	return ms
}

func postReprocess(t *testing.T, mux *http.ServeMux, id, key string) (int, map[string]string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/dead-letters/"+id+"/reprocess", nil)
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	var body map[string]string
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode reprocess response: %v", err)
	}
	return w.Code, body
}

// TestReprocessIsIdempotentUnderTheSameKey reproduces the issue's measurement:
// two calls, same Idempotency-Key, same dead-lettered run. Before the fix these
// returned two independent run ids, both re-driving the same failed work.
func TestReprocessIsIdempotentUnderTheSameKey(t *testing.T) {
	starter := newKeyedRunStarter()
	mux := http.NewServeMux()
	registerRoutes(mux, newTestAPIServer(deadLetteredStore(starter)))

	const key = "operator-redrive-7f3a"

	code1, first := postReprocess(t, mux, "wf-1", key)
	if code1 != http.StatusCreated {
		t.Fatalf("first reprocess: got %d, want 201", code1)
	}
	if first["id"] == "" {
		t.Fatalf("first reprocess returned no id: %v", first)
	}

	code2, second := postReprocess(t, mux, "wf-1", key)
	if code2 != http.StatusOK {
		t.Errorf("retry under the same key: got %d, want 200 -- a retry after a "+
			"lost response must not create a second run", code2)
	}
	if second["already_started"] != "true" {
		t.Errorf("retry: already_started=%q, want \"true\" (%v)", second["already_started"], second)
	}
	if got := second["workflow_id"]; got != first["id"] {
		t.Errorf("retry returned run %q, want the first run %q -- two ids here is "+
			"the same failed work re-driven twice (cleat#1167)", got, first["id"])
	}
	if starter.n != 1 {
		t.Errorf("store created %d runs, want 1", starter.n)
	}

	// The mechanism, asserted directly: the handler must hand the header down.
	// Without this a future change could satisfy the assertions above by some
	// other means and leave the key unread again.
	for i, got := range starter.seen {
		if got != key {
			t.Errorf("call %d passed idempotency key %q to the store, want %q", i+1, got, key)
		}
	}
}

// TestReprocessWithoutAKeyStartsEachTime pins the behaviour deliberately left
// alone. Deriving a key from the dead-letter id would protect callers that send
// no header, but it would also refuse a DELIBERATE second re-drive -- fix the
// downstream, re-drive again -- and answering that with the run from an hour
// ago is worse than the duplicate #1167 is about. Making reprocess idempotent
// by identity removes an operation and needs to be its own decision; this test
// is here so that decision is made on purpose rather than by drift.
func TestReprocessWithoutAKeyStartsEachTime(t *testing.T) {
	starter := newKeyedRunStarter()
	mux := http.NewServeMux()
	registerRoutes(mux, newTestAPIServer(deadLetteredStore(starter)))

	_, first := postReprocess(t, mux, "wf-1", "")
	_, second := postReprocess(t, mux, "wf-1", "")

	if first["id"] == "" || second["id"] == "" {
		t.Fatalf("expected both calls to create runs: %v, %v", first, second)
	}
	if first["id"] == second["id"] {
		t.Errorf("two keyless reprocesses both returned %q; a second re-drive "+
			"with no token must remain a real second re-drive", first["id"])
	}
}

// TestStartWorkflowUsesTheIdempotencyKey repairs the guard on the path that was
// already correct. TestAPIStartWorkflow_WithIdempotencyKey cannot fail when the
// handler stops reading the header -- verified by replacing that read with ""
// and watching it stay green -- which is how #1167 went unnoticed next door.
func TestStartWorkflowUsesTheIdempotencyKey(t *testing.T) {
	starter := newKeyedRunStarter()
	ms := &mockStore{}
	ms.listVersionsFn = func(_ context.Context, _ string) ([]int, error) { return []int{1}, nil }
	ms.startNewRunFn = starter.start
	api := newTestAPIServer(ms)

	const key = "client-token-91b2"
	call := func() (int, map[string]string) {
		req := httptest.NewRequest(http.MethodPost, "/api/workflows/my-wf/start", strings.NewReader(`{"input":{}}`))
		req.Header.Set("Idempotency-Key", key)
		w := httptest.NewRecorder()
		api.handleStartWorkflow(w, req, "my-wf")
		var body map[string]string
		json.NewDecoder(w.Body).Decode(&body)
		return w.Code, body
	}

	code1, first := call()
	if code1 != http.StatusCreated && code1 != http.StatusOK {
		t.Fatalf("first start: got %d, want 200 or 201 (%v)", code1, first)
	}
	code2, second := call()
	if code2 != http.StatusOK || second["already_started"] != "true" {
		t.Errorf("retry under the same key: got %d %v, want 200 already_started=true", code2, second)
	}
	if starter.n != 1 {
		t.Errorf("store created %d runs under one key, want 1", starter.n)
	}
	for i, got := range starter.seen {
		if got != key {
			t.Errorf("start call %d passed key %q to the store, want %q", i+1, got, key)
		}
	}
}
