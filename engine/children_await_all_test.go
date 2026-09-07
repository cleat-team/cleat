package engine

import (
	"context"
	"encoding/json"
	"testing"
)

// ---------------------------------------------------------------------------
// AwaitAllChildren carries each child's result
// ---------------------------------------------------------------------------

// fakeChildResultStore answers GetChildResult from a map and does nothing else.
type fakeChildResultStore struct {
	results map[string]string // runID -> result JSON; absent means still running
}

func (f *fakeChildResultStore) StartChildWorkflow(context.Context, string, string, string, int, string, int) (string, error) {
	return "", nil
}

func (f *fakeChildResultStore) StartChildWorkflowAtomic(context.Context, string, string, string, string, int, string, EventRecord, int) (string, error) {
	return "", nil
}

func (f *fakeChildResultStore) GetChildResult(_ context.Context, runID string) (string, bool, error) {
	r, ok := f.results[runID]
	if !ok {
		return "", false, nil
	}
	return r, true, nil
}

func (f *fakeChildResultStore) ResolveVersionByTag(context.Context, string, string) (int, error) {
	return 0, nil
}

// TestAwaitAllChildrenCarriesEachChildsResult pins the reason to fan out at all.
//
// The recorded event's Response is the assertion target rather than the guest's
// buffer, and deliberately: it is the DURABLE artifact. freshAwaitAllChildren
// writes it to the guest and replayAwaitAllChildren hands the same bytes back
// on every later replay, so a Response missing a child's result is missing it
// permanently. Asserting on the write to the module would test only the first
// of those two paths, and a nil module (which every host-call test here passes)
// cannot be read back anyway.
//
// There was no engine-level test over this call with COMPLETED children. The
// existing coverage is dispatch and linker registration -- that AwaitAllChildren
// is reachable, not that it answers. That is the gap this fills, and it is the
// same shape as IMPROVEMENT-PLAN 3.215(d): a path can be wired end to end,
// registered, dispatched and tested, and still not do the thing.
func TestAwaitAllChildrenCarriesEachChildsResult(t *testing.T) {
	store := &fakeChildResultStore{results: map[string]string{
		"child-a": `{"tag":"child-0"}`,
		"child-b": `{"tag":"child-1"}`,
		"child-c": `{"tag":"child-2"}`,
	}}

	s := newTestExecSession()
	s.engine = NewEngine(nil, nil, WithChildWorkflowStore(store))

	runIDs := `["child-a","child-b","child-c"]`
	s.AwaitAllChildren(context.Background(), nil, runIDs, 0, 0)

	if len(s.history) != 1 {
		t.Fatalf("expected exactly one recorded event, got %d", len(s.history))
	}
	rec := s.history[0]
	if rec.EventType != EventTypeAwaitAllChildren {
		t.Fatalf("recorded %q, want %q", rec.EventType, EventTypeAwaitAllChildren)
	}
	if rec.Response == "" {
		t.Fatal("no response recorded: every child was complete, so this should not have suspended")
	}

	var outcomes []struct {
		RunID  string `json:"run_id"`
		Result string `json:"result"`
		Error  string `json:"error"`
	}
	if err := json.Unmarshal([]byte(rec.Response), &outcomes); err != nil {
		t.Fatalf("response is not a JSON array of outcomes: %v\n  response: %s", err, rec.Response)
	}
	if len(outcomes) != 3 {
		t.Fatalf("expected 3 outcomes, got %d: %s", len(outcomes), rec.Response)
	}
	for _, o := range outcomes {
		want, ok := store.results[o.RunID]
		if !ok {
			t.Errorf("outcome names an unknown child %q", o.RunID)
			continue
		}
		if o.Error != "" {
			t.Errorf("child %s reported error %q, but the store said it completed", o.RunID, o.Error)
		}
		if o.Result != want {
			t.Errorf("child %s: result is %q, want %q -- collecting child results is the "+
				"reason to fan out, and an empty result reads as success", o.RunID, o.Result, want)
		}
	}
}

// GetChildCompletedAtMs satisfies the store interface. Added with #847, which
// made PollChild derive its answer from the child's completion instant rather
// than querying live. Returning ok=false means "never completed", which keeps
// every existing test's PollChild answer at "running".
func (f *fakeChildResultStore) GetChildCompletedAtMs(ctx context.Context, runID string) (int64, bool, error) {
	return 0, false, nil
}
