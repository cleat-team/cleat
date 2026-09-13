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
//	winner not finished -> poll or wait
//	winner done         -> fetch the result, it is available now
//	winner failed       -> surface it; waiting will not improve it
//
// "not finished" rather than "running", and the difference is cleat#1325: this
// header said `winner running` and every case below supplied `running` or
// `done`, so the arm a real retry usually lands on was covered nowhere. A
// workflow parked in a durable sleep is `ready`. See
// TestADuplicateStartSaysTheWinnerIsSleeping.
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

// map[string]any since cleat#1169: idempotent_replay is a real bool, and a
// map[string]string decode fails on it outright rather than reporting a wrong
// value -- which is the better failure, but only once.
func decodeStart(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return body
}

// str reads a string field, failing rather than returning "" for a wrong type,
// so a field that changed type is not silently read as absent.
func str(t *testing.T, body map[string]any, key string) string {
	t.Helper()
	v, ok := body[key]
	if !ok {
		return ""
	}
	sv, ok := v.(string)
	if !ok {
		t.Fatalf("%s is %T (%v), want string", key, v, v)
	}
	return sv
}

// replayed reads the standard cleat#1169 flag, failing if it is absent: it is
// present on the original as well as the replay, so absence is a defect rather
// than a "no".
func replayed(t *testing.T, body map[string]any) bool {
	t.Helper()
	v, ok := body[idempotentReplayField]
	if !ok {
		t.Fatalf("%s is absent from %v. It is set on BOTH the original and the replay so "+
			"a caller can read it unconditionally.", idempotentReplayField, body)
	}
	b, ok := v.(bool)
	if !ok {
		t.Fatalf("%s is %T (%v), want bool", idempotentReplayField, v, v)
	}
	return b
}

func TestADuplicateStartSaysTheWinnerIsStillRunning(t *testing.T) {
	api, k := winnerStore(t, &engine.WorkflowInstance{Status: "running"})

	first := startWithKey(api, "K")
	if first.Code != 201 {
		t.Fatalf("first start: %d %s", first.Code, first.Body.String())
	}
	dup := decodeStart(t, startWithKey(api, "K"))

	if !replayed(t, dup) {
		t.Fatalf("the retry was not recognised as a duplicate: %v", dup)
	}
	if str(t, dup, "status") != "running" {
		t.Errorf("a duplicate start reports status %q, want \"running\".\n\n"+
			"Without it the caller cannot tell a run it should wait for from one whose "+
			"result is already available, and both answers were byte-identical.",
			str(t, dup, "status"))
	}
	// The double must have been asked once per call, or the handler is
	// answering from something other than the key.
	if len(k.seen) != 2 || k.seen[0] != "K" || k.seen[1] != "K" {
		t.Errorf("the store was handed %v, want [K K]", k.seen)
	}
}

// The common case, and it had no test anywhere. cleat#1325.
//
// `ready` is not an edge case that happens to be uncovered -- it is the value a
// retrying caller most often sees. A suspending segment is finalized with
// status "ready" and a next_wake_at (migrations/postgres/003_procedures.sql,
// `WHEN 'ready' THEN ... SET status = 'ready', assigned_to = NULL,
// next_wake_at = p_next_wake_at`), so any workflow that sleeps, awaits a child,
// waits on a signal or backs off a retry is `ready` for nearly all of its life.
// `running` covers only the slices when a worker actually holds it. Measured on
// a run whose single act is an 8s DurableSleepMs, polled every 500ms: twenty
// consecutive `ready` samples, then `done`. Not one `running`.
//
// WHY THE GAP SURVIVED, which is the part worth keeping. Every case in this
// file supplies the winner through a mock, so the handler and the test agree
// about which statuses occur BY CONSTRUCTION -- the same belief wrote both, and
// a double cannot disagree with the code it was written from. The gap was found
// against a real run in cleat-ports, not here, and the contract comment this
// file's header quoted named `running` too.
//
// This test asserts the pass-through rather than a mapping: `status` is
// workflow_instances.status verbatim, so a value the handler has never heard of
// must arrive unaltered rather than be flattened into a known one.
func TestADuplicateStartSaysTheWinnerIsSleeping(t *testing.T) {
	api, _ := winnerStore(t, &engine.WorkflowInstance{Status: "ready"})

	startWithKey(api, "K")
	dup := decodeStart(t, startWithKey(api, "K"))

	if !replayed(t, dup) {
		t.Fatalf("the retry was not recognised as a duplicate: %v", dup)
	}
	if str(t, dup, "status") != "ready" {
		t.Errorf("a duplicate start whose winner is sleeping reports status %q, want "+
			"\"ready\".\n\nA workflow parked in a durable sleep is `ready`, not `running`, "+
			"and that is the state a retrying caller most often finds. Reporting anything "+
			"else -- including flattening it to `running` -- tells the caller something "+
			"the run row does not say (cleat#1325).", str(t, dup, "status"))
	}
	// Non-terminal, so no result and no error: both would be the cleat#1115
	// ambiguity, a field present but meaningless.
	if _, present := dup["error"]; present {
		t.Errorf("a sleeping winner carried an error field: %v", dup)
	}
}

// A status this handler has never been told about must pass through unchanged.
//
// The four terminal values and `terminating` are all reachable here and none is
// enumerated in the handler; the response is a copy. This pins that, so a later
// change that switches on a known set -- and silently maps everything else to
// one bucket -- goes red rather than quietly narrowing the vocabulary. It is
// the guard the `running`-only coverage above needed and did not have.
func TestADuplicateStartDoesNotNarrowTheStatusVocabulary(t *testing.T) {
	for _, status := range []string{"terminating", "terminated", "dead_lettered"} {
		t.Run(status, func(t *testing.T) {
			api, _ := winnerStore(t, &engine.WorkflowInstance{Status: status})

			startWithKey(api, "K")
			dup := decodeStart(t, startWithKey(api, "K"))

			if dup["status"] != status {
				t.Errorf("a duplicate start reports status %q for a winner in %q.\n\n"+
					"`status` is workflow_instances.status verbatim; a value missing from "+
					"the response, or mapped onto a neighbour, is the handler inventing a "+
					"vocabulary the lifecycle document does not have (cleat#1325).",
					dup["status"], status)
			}
		})
	}
}

func TestADuplicateStartSaysTheWinnerFinished(t *testing.T) {
	api, _ := winnerStore(t, &engine.WorkflowInstance{Status: "done"})

	startWithKey(api, "K")
	dup := decodeStart(t, startWithKey(api, "K"))

	if str(t, dup, "status") != "done" {
		t.Errorf("a duplicate start reports status %q, want \"done\"", str(t, dup, "status"))
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

	if str(t, dup, "status") != "failed" {
		t.Fatalf("a duplicate start reports status %q, want \"failed\"", str(t, dup, "status"))
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

	if !replayed(t, dup) || str(t, dup, "id") == "" {
		t.Fatalf("the duplicate lost its fields: %v", dup)
	}
	if str(t, dup, "status") != "unknown" {
		t.Errorf("with the winner gone the duplicate reports status %q, want \"unknown\".\n\n"+
			"An absent field would read as an oversight; the caller needs to be able to "+
			"distinguish \"cannot tell\" from \"did not say\".", str(t, dup, "status"))
	}
}

// The duplicate returns the ORIGINAL response, same field names, plus the flag.
// cleat#1169.
//
// Renamed from TestADuplicateStartKeepsItsOriginalFields, which asserted the
// thing this replaced: that the duplicate kept `workflow_id` and
// `already_started`. Those WERE the original fields of the duplicate response,
// and the whole defect was that they were not the original fields of the
// ORIGINAL response -- the first call answered `{"id":X}`. A caller reading
// only `id` got nothing back from a deduplicated call and concluded its retry
// had started a second workflow.
//
// So this asserts the identifier is the SAME KEY in both, which is the property
// that makes a retry transparent, and that the flag is what distinguishes them.
func TestADuplicateStartReturnsTheOriginalShapePlusTheFlag(t *testing.T) {
	api, _ := winnerStore(t, &engine.WorkflowInstance{Status: "running"})

	firstRec := startWithKey(api, "K")
	dupRec := startWithKey(api, "K")

	// Same status, too. The 200/201 split used to be the duplicate signal; the
	// flag carries it now, so a caller branching on the status code sees one
	// answer for both.
	if firstRec.Code != dupRec.Code {
		t.Errorf("first start answered %d and the duplicate %d; a replay returns the "+
			"original STATUS as well as the original body", firstRec.Code, dupRec.Code)
	}

	first := decodeStart(t, firstRec)
	dup := decodeStart(t, dupRec)

	if str(t, first, "id") == "" {
		t.Fatalf("the first start stopped returning id: %v", first)
	}
	if str(t, dup, "id") != str(t, first, "id") {
		t.Errorf("the duplicate names %q under key \"id\", the original created %q.\n\n"+
			"Reading only `id` must work on both responses -- it did not before cleat#1169, "+
			"and a caller that did so concluded its retry had started a SECOND workflow.",
			str(t, dup, "id"), str(t, first, "id"))
	}
	if _, gone := dup["workflow_id"]; gone {
		t.Errorf("the duplicate still carries workflow_id: %v.\n\nThe rename is what "+
			"cleat#1169 removed; keeping it as well would leave two names for one thing.", dup)
	}
	if _, gone := dup["already_started"]; gone {
		t.Errorf("the duplicate still carries already_started: %v.\n\nIt was replaced by "+
			"the standard flag, which is spelled the same on every endpoint.", dup)
	}

	// The flag distinguishes them, and it is present on both.
	if replayed(t, first) {
		t.Error("the FIRST start reports itself as a replay")
	}
	if !replayed(t, dup) {
		t.Error("the duplicate does not report itself as a replay")
	}
}
