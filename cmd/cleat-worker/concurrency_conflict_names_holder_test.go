package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
)

// holderAwareStore is a mockStore that can answer who holds a key.
//
// A separate type rather than a field on mockStore: the handler reaches this
// through an OPTIONAL interface assertion, so whether a store implements the
// method is the thing under test. Adding the method to mockStore would make
// every existing test take the new branch, and the fall-through case -- a store
// that cannot answer -- would stop being covered anywhere.
type holderAwareStore struct {
	*mockStore
	holder engine.ConcurrencyKeyHolder
	err    error
	asked  int
}

func (h *holderAwareStore) GetConcurrencyKeyHolder(ctx context.Context, key string) (engine.ConcurrencyKeyHolder, error) {
	h.asked++
	return h.holder, h.err
}

// A concurrency-key refusal names the run that holds the key, and until when.
//
// cleat#1172: the 409 said "workflow already running with key K", where K is
// the value the CALLER supplied -- so the response restated the request. The
// two questions at that moment are what holds the key and for how long, and
// neither was answerable from the refusal, by filtering the listing, or by
// paging. It is worst where the feature earns its keep: a holder that has hung
// blocks every later start under that key, and the only signal was a 409
// naming the key back at you.
func TestConcurrencyKeyConflict_NamesTheHolder(t *testing.T) {
	const (
		losingRun = "the-loser"
		holderRun = "the-holder"
		key       = "nightly-sync"
	)
	until := time.Now().UTC().Add(17 * time.Minute).Truncate(time.Second)

	newStore := func() *holderAwareStore {
		ms := &mockStore{}
		ms.startNewRunFn = func(ctx context.Context, id, defName string, defVersion int, input json.RawMessage, idempotencyKey, tenantID string, priority int) (string, bool, error) {
			return losingRun, false, nil
		}
		ms.acquireConcurrencyKeyFn = func(ctx context.Context, k, workflowID string, ttl time.Duration) (bool, error) {
			return false, nil // held by someone else
		}
		ms.terminateWorkflowFn = func(ctx context.Context, workflowID, reason string) error { return nil }
		return &holderAwareStore{mockStore: ms}
	}

	post := func(t *testing.T, st *holderAwareStore) (int, map[string]string) {
		t.Helper()
		// Built here rather than through newTestAPIServer, which takes a
		// concrete *mockStore. fakeStoreFactory.fallback is typed as the
		// interface, so the wrapper reaches the handler with its extra method
		// intact -- which is the whole point.
		api := &apiServer{
			store:       st,
			worker:      newTestWorker(st.mockStore),
			maxBodySize: 1 << 20,
			factory:     &fakeStoreFactory{fallback: st},
		}
		req := httptest.NewRequest(http.MethodPost, "/api/workflows/my-wf/start", strings.NewReader(`{"input":{}}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Cleat-Concurrency-Key", key)
		resp := httptest.NewRecorder()
		api.handleStartWorkflow(resp, req, "my-wf")
		var body map[string]string
		if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
			t.Fatalf("decoding the 409 body: %v (raw: %s)", err, resp.Body.String())
		}
		return resp.Code, body
	}

	t.Run("the holder is named", func(t *testing.T) {
		st := newStore()
		st.holder = engine.ConcurrencyKeyHolder{WorkflowID: holderRun, ExpiresAt: until, Held: true}

		code, body := post(t, st)
		if code != 409 {
			t.Fatalf("status = %d, want 409", code)
		}
		if st.asked != 1 {
			t.Errorf("the holder was looked up %d times, want 1", st.asked)
		}
		if body["held_by"] != holderRun {
			t.Errorf("held_by = %q, want %q.\n\nThis is cleat#1172: without it the caller is "+
				"told only the key it already sent.", body["held_by"], holderRun)
		}
		if body["held_until"] != until.Format(time.RFC3339) {
			t.Errorf("held_until = %q, want %q.\n\n\"For how long\" is the second of the two "+
				"questions a refused caller has; a holder with no expiry is indistinguishable "+
				"from a stuck one.", body["held_until"], until.Format(time.RFC3339))
		}
		// The prose message carries it too, because that is what a human sees
		// in a log or a CLI that prints only `error`.
		if !strings.Contains(body["error"], holderRun) {
			t.Errorf("error = %q, does not name the holder. A client that surfaces only this "+
				"field is the common case.", body["error"])
		}
	})

	t.Run("a lookup failure still refuses, and does not become a 500", func(t *testing.T) {
		st := newStore()
		st.err = context.DeadlineExceeded

		code, body := post(t, st)
		if code != 409 {
			t.Fatalf("status = %d, want 409.\n\nThe diagnostic is best-effort. A caller that "+
				"was refused must still be told it was refused -- turning a refusal into a 500 "+
				"because the EXPLANATION could not be fetched inverts which fact matters.", code)
		}
		if _, ok := body["held_by"]; ok {
			t.Errorf("held_by present after a failed lookup: %v -- an absent holder must be "+
				"absent, not empty", body)
		}
		if !strings.Contains(body["error"], key) {
			t.Errorf("error = %q, no longer names the key either", body["error"])
		}
	})

	t.Run("a key released between the acquire and the lookup is not an error", func(t *testing.T) {
		// Held=false with no error: the acquire failed, then the holder
		// finished before we read. A legitimate race, not a fault.
		st := newStore()
		st.holder = engine.ConcurrencyKeyHolder{}

		code, body := post(t, st)
		if code != 409 {
			t.Fatalf("status = %d, want 409", code)
		}
		if _, ok := body["held_by"]; ok {
			t.Errorf("held_by present for an unheld key: %v", body)
		}
	})
}
