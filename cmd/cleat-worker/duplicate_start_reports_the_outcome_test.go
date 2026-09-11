package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

// A re-sent start says what became of the winner. cleat#1151.
//
// The whole purpose of an idempotency key is to make a retry safe when the
// caller does not know whether the first attempt landed. On being told
// `already_started`, what the caller should do next depends on something the
// response did not carry:
//
//	winner running   -> poll or wait
//	winner done      -> fetch the result, it is available now
//	winner failed    -> surface it; waiting will not improve it
//
// Measured before this change, with the run confirmed terminal in between, the
// two responses were BYTE-IDENTICAL. So every caller needed a second request to
// learn which of three situations it was in, and one that skipped it would
// either poll a finished run forever or treat a running one as complete.
//
// Complementary to #1169's replay decision rather than part of it. That makes
// the duplicate response the ORIGINAL response plus a marker -- fixing the
// SHAPE. This fixes the OUTCOME, which replay cannot: the original response to
// a start is `{"id": X}`, written before the workflow has done anything.
//
// The fields are ADDITIVE. `workflow_id` and `already_started` keep their names
// and meanings, so nothing that reads them breaks, and #1169 can later fold the
// shape without fighting this.
func startWithKey(api *apiServer, key string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/workflows/test-workflow/start",
		strings.NewReader(`{"input":{}}`))
	req.Header.Set("Idempotency-Key", key)
	rec := httptest.NewRecorder()
	api.handleWorkflows(rec, req)
	return rec
}

// winnerStore answers StartNewRun like keyedRunStarter -- one key, one run --
// and lets a test say what that run's CURRENT state is, which is the thing
// under test.
//
// A double that returned alreadyExisted=true while knowing nothing about the
// run would be useless here: the question is not whether the handler noticed a
// duplicate, it is what the handler then says about the winner. Two tests in
// this package were built the other way and passed on broken code.
func winnerStore(t *testing.T, winner *engine.WorkflowInstance) (*apiServer, *keyedRunStarter) {
	t.Helper()
	k := newKeyedRunStarter()
	ms := &mockStore{}
	ms.startNewRunFn = k.start
	ms.getWorkflowByIDFn = func(_ context.Context, id string) (*engine.WorkflowInstance, error) {
		if winner == nil {
			return nil, nil
		}
		w := *winner
		w.ID = id
		return &w, nil
	}
	return newTestAPIServer(ms), k
}

func decodeStart(t *testing.T, rec *httptest.ResponseRecorder) map[string]string {
	t.Helper()
	var body map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return body
}

func TestADuplicateStartSaysTheWinnerIsStillRunning(t *testing.T) {
	api, k := winnerStore(t, &engine.WorkflowInstance{Status: "running"})

	first := startWithKey(api, "K")
	if first.Code != 201 {
		t.Fatalf("first start: %d %s", first.Code, first.Body.String())
	}
	dup := decodeStart(t, startWithKey(api, "K"))

	if dup["already_started"] != "true" {
		t.Fatalf("the retry was not recognised as a duplicate: %v", dup)
	}
	if dup["status"] != "running" {
		t.Errorf("a duplicate start reports status %q, want \"running\".\n\n"+
			"Without it the caller cannot tell a run it should wait for from one whose "+
			"result is already available, and both answers were byte-identical.",
			dup["status"])
	}
	// The double must have been asked once per call, or the handler is
	// answering from something other than the key.
	if len(k.seen) != 2 || k.seen[0] != "K" || k.seen[1] != "K" {
		t.Errorf("the store was handed %v, want [K K]", k.seen)
	}
}

func TestADuplicateStartSaysTheWinnerFinished(t *testing.T) {
	api, _ := winnerStore(t, &engine.WorkflowInstance{Status: "done"})

	startWithKey(api, "K")
	dup := decodeStart(t, startWithKey(api, "K"))

	if dup["status"] != "done" {
		t.Errorf("a duplicate start reports status %q, want \"done\"", dup["status"])
	}
	// A finished winner must not carry an error field: an empty `error` on a
	// success is exactly the ambiguity cleat#1115 was, one layer up.
	if _, present := dup["error"]; present {
		t.Errorf("a successful winner carried an error field: %v", dup)
	}
}

func TestADuplicateStartSurfacesAFailedWinner(t *testing.T) {
	api, _ := winnerStore(t, &engine.WorkflowInstance{
		Status: "failed", Error: "downstream refused", ErrorCode: "E_DOWNSTREAM",
	})

	startWithKey(api, "K")
	dup := decodeStart(t, startWithKey(api, "K"))

	if dup["status"] != "failed" {
		t.Fatalf("a duplicate start reports status %q, want \"failed\"", dup["status"])
	}
	if dup["error"] != "downstream refused" {
		t.Errorf("the failed winner's error is %q, want \"downstream refused\".\n\n"+
			"A caller retrying a start it believes may have been lost should learn that "+
			"the original FAILED without a second request -- waiting will not improve it.",
			dup["error"])
	}
	if dup["error_code"] != "E_DOWNSTREAM" {
		t.Errorf("error_code is %q, want E_DOWNSTREAM", dup["error_code"])
	}
}

// The winner may legitimately be GONE by the time a retry arrives, and that is
// newly reachable: #1264 moved key cleanup onto expires_at across all three
// dialects, and #1258 made retention delete the key alongside the workflow. So
// a key can outlive the run it names.
//
// "Unknown" is stated rather than implied by an absent field. A caller
// branching on status must be able to tell "I cannot tell you" from "I forgot
// to tell you", and an omitted key reads as the second.
func TestADuplicateStartSaysSoWhenTheWinnerIsGone(t *testing.T) {
	api, _ := winnerStore(t, nil)

	startWithKey(api, "K")
	dup := decodeStart(t, startWithKey(api, "K"))

	if dup["already_started"] != "true" || dup["workflow_id"] == "" {
		t.Fatalf("the duplicate lost its existing fields: %v", dup)
	}
	if dup["status"] != "unknown" {
		t.Errorf("with the winner gone the duplicate reports status %q, want \"unknown\".\n\n"+
			"An absent field would read as an oversight; the caller needs to be able to "+
			"distinguish \"cannot tell\" from \"did not say\".", dup["status"])
	}
}

// The fields are additive: a caller reading only the two original keys is
// unaffected. This is what keeps #1169's replay decision free to fold the
// shape later without unpicking this one.
func TestADuplicateStartKeepsItsOriginalFields(t *testing.T) {
	api, _ := winnerStore(t, &engine.WorkflowInstance{Status: "running"})

	first := decodeStart(t, startWithKey(api, "K"))
	dup := decodeStart(t, startWithKey(api, "K"))

	if first["id"] == "" {
		t.Fatalf("the first start stopped returning id: %v", first)
	}
	if dup["workflow_id"] != first["id"] {
		t.Errorf("the duplicate names %q, the original created %q",
			dup["workflow_id"], first["id"])
	}
	if dup["already_started"] != "true" {
		t.Errorf("already_started is %q, want \"true\"", dup["already_started"])
	}
}
