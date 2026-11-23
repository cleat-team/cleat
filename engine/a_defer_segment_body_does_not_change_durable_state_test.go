package engine

// cleat#4079 audit rows: set_query_state, create_promise, poll_update, set_scope.
//
// The audit probed each call from a defer segment's body with a test engine that
// had NO store behind it, and recorded "CONTINUED" -- that the guest ran on past
// the call. That shows the host did not refuse. It does not show the call changed
// anything: cleat#4083's probe read the same way and turned out, against a real
// store, to be a real deletion. So each row is measured here against a real store,
// and the question is the one that decides whether a guard is owed:
//
//	does the call, made by a defer segment's BODY (outside a defer body), change
//	state that outlives the segment?
//
// WHAT "OUTLIVES THE SEGMENT" MEANS HERE. The worker's finishDeferPhase persists
// exactly one thing from a segment: the history events it appended
// (resultHistory[len(history):]), in the same transaction as the terminal write.
// It takes no query state. So an in-session event is persistent, a store row
// written by the call is persistent, and a field of the in-memory session is not.
//
// TWO CASES PER CALL, because the second is what makes the first mean anything:
// an ordinary session shows the effect IS observable by this probe, so "no effect"
// for the segment body cannot be the probe failing to see it.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// bodyOfADeferSegment is a session whose workflow BODY is running past the end of
// the history a defer segment replayed -- the state stopBeforeNewWork refuses in.
// ordinary is the same session without the segment, the control.
func bodyOfADeferSegment(opts []EngineOption, inSegment bool, id string) *execSession {
	if inSegment {
		opts = append(opts, WithDeferPhase())
	}
	opts = append(opts, WithWorkflowID(id))
	return &execSession{
		engine:     NewEngine(nil, nil, opts...),
		workflowID: id,
		nowMs:      time.Now().UnixMilli(),
		deferrals:  map[string]string{},
		queryState: map[string]string{},
		isReplay:   inSegment, // replaying, with no history left: past the frontier
	}
}

// auditRun starts a run row for the call to belong to. It is not claimed: setupTestData seeds ready runs,
// so a ClaimWorkflow here takes whichever of them is first, which is another test's row.
func auditRun(t *testing.T, store WorkflowStore, idemKey string) string {
	t.Helper()
	id, _, err := store.StartNewRun(context.Background(), "", "test-workflow", 1, json.RawMessage(`{}`), idemKey, DefaultTenantUUID, 0)
	if err != nil {
		t.Fatalf("StartNewRun(%s): %v", idemKey, err)
	}
	return id
}

// recordingUpdateStore reports one pending request and counts how often it is asked.
type recordingUpdateStore struct{ asked int }

func (r *recordingUpdateStore) GetPendingUpdateRequests(context.Context, string) ([]UpdateRequestInfo, error) {
	r.asked++
	return []UpdateRequestInfo{{UpdateName: "u", Payload: `{}`}}, nil
}
func (r *recordingUpdateStore) CompleteUpdateRequest(context.Context, string, string, string, string) error {
	return nil
}
func (r *recordingUpdateStore) ResolvePromise(context.Context, string, string) error { return nil }
func (r *recordingUpdateStore) RejectPromise(context.Context, string, string) error  { return nil }

func TestADeferSegmentBodyDoesNotCreateAPromise(t *testing.T) {
	for _, backend := range registeredBackends {
		for _, inSegment := range []bool{false, true} {
			name := "ordinary session"
			if inSegment {
				name = "defer segment body"
			}
			t.Run(backend.Name()+"/"+name, func(t *testing.T) {
				store, teardown := backend.Setup(t)
				defer teardown()
				setupTestData(t, store)
				ps, ok := store.(PromiseStore)
				if !ok {
					t.Fatalf("UNMEASURED: %T is not a PromiseStore", store)
				}
				id := auditRun(t, store, "audit-create-promise")

				s := bodyOfADeferSegment([]EngineOption{WithPromiseStore(ps)}, inSegment, id)
				got := s.CreatePromise(context.Background(), nil, "audit", 0, 0)

				var promiseID string
				for _, e := range s.history {
					if e.EventType == EventTypeCreatePromise {
						promiseID = e.PromiseID
					}
				}
				rowExists := false
				if promiseID != "" {
					_, _, _, err := ps.GetPromise(context.Background(), id, promiseID)
					switch {
					case err == nil:
						rowExists = true
					case errors.Is(err, ErrPromiseNotFound):
					default:
						t.Fatalf("UNMEASURED: GetPromise(%q) failed with something other than not-found: %v", promiseID, err)
					}
				}

				if !inSegment {
					// The control: the probe CAN see a created promise, as an event and as a row.
					if promiseID == "" || !rowExists {
						t.Fatalf("control: an ordinary create_promise left event=%q row=%v, so this probe cannot see a created promise",
							promiseID, rowExists)
					}
					return
				}
				if promiseID != "" || rowExists {
					t.Errorf("a defer segment's body created a promise: history event id=%q, store row=%v (returned %#x).\n\n"+
						"The event is appended to the terminal transaction by finishDeferPhase and the row is written at once, "+
						"so both outlive a segment that exists only to run cleanup.", promiseID, rowExists, got)
				}
				if got != callSuspendSentinel {
					t.Errorf("returned %#x, want the stop sentinel %#x", got, callSuspendSentinel)
				}
			})
		}
	}
}

func TestADeferSegmentBodyDoesNotAcquireAScope(t *testing.T) {
	for _, backend := range registeredBackends {
		for _, inSegment := range []bool{false, true} {
			name := "ordinary session"
			if inSegment {
				name = "defer segment body"
			}
			t.Run(backend.Name()+"/"+name, func(t *testing.T) {
				store, teardown := backend.Setup(t)
				defer teardown()
				setupTestData(t, store)
				cks, ok := store.(ConcurrencyKeyStore)
				if !ok {
					t.Fatalf("UNMEASURED: %T is not a ConcurrencyKeyStore", store)
				}
				id := auditRun(t, store, "audit-set-scope")

				s := bodyOfADeferSegment([]EngineOption{WithConcurrencyKeyStore(cks)}, inSegment, id)
				got := s.SetScope(context.Background(), nil, "audit-type", "audit-key", 0, 0)

				// Release is the probe: it reports whether a row held by THIS workflow was removed.
				held, err := cks.ReleaseConcurrencyKey(context.Background(), "vo:audit-type:audit-key", id)
				if err != nil {
					t.Fatalf("UNMEASURED: ReleaseConcurrencyKey: %v", err)
				}
				if !inSegment {
					if !held {
						t.Fatalf("control: an ordinary set_scope left no held key, so this probe cannot see one")
					}
					return
				}
				if held {
					t.Errorf("a defer segment's body took a virtual-object scope key (returned %#x).\n\n"+
						"The key is a 24-hour row in the concurrency-key store, held while the segment runs, and any live "+
						"workflow that wants that virtual object is blocked on it. Whether it is freed afterwards is the "+
						"terminal transition's doing, not this call's: the suspend exit path does not release held scopes.", got)
				}
				if got != callSuspendSentinel {
					t.Errorf("returned %#x, want the stop sentinel %#x", got, callSuspendSentinel)
				}
			})
		}
	}
}

// A defer BODY must still be able to take a scope: cleanup that runs under the
// virtual object it manages is legitimate. Without this the test above would pass
// against a guard that refuses everything.
func TestADeferBodyCanStillAcquireAScope(t *testing.T) {
	for _, backend := range registeredBackends {
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)
			cks, ok := store.(ConcurrencyKeyStore)
			if !ok {
				t.Fatalf("UNMEASURED: %T is not a ConcurrencyKeyStore", store)
			}
			id := auditRun(t, store, "audit-set-scope-defer-body")

			s := bodyOfADeferSegment([]EngineOption{WithConcurrencyKeyStore(cks)}, true, id)
			s.inDeferDrain = true
			s.SetScope(context.Background(), nil, "audit-type", "audit-key", 0, 0)
			held, err := cks.ReleaseConcurrencyKey(context.Background(), "vo:audit-type:audit-key", id)
			if err != nil || !held {
				t.Errorf("a defer body's set_scope did not take the key (held=%v, err=%v): the guard has removed the one legitimate use", held, err)
			}
		})
	}
}

// poll_update ALREADY consults stopBeforeNewWork, so this row is a control for the
// audit, not a defect: the audit's engine had no update store, and DurablePollUpdate
// returns 0 on a nil store BEFORE it reaches the guard -- which reads as "the guest
// continued" whether or not the guard exists.
func TestADeferSegmentBodyDoesNotDeliverAnUpdate(t *testing.T) {
	for _, inSegment := range []bool{false, true} {
		name := "ordinary session"
		if inSegment {
			name = "defer segment body"
		}
		t.Run(name, func(t *testing.T) {
			us := &recordingUpdateStore{}
			s := bodyOfADeferSegment([]EngineOption{WithUpdateStore(us)}, inSegment, "wf-audit-poll-update")
			got := s.DurablePollUpdate(contextWithRawMemBuf(context.Background(), make([]byte, 4096)), nil, 0, 4000)

			delivered := false
			for _, e := range s.history {
				if e.EventType == EventTypeUpdateReceived {
					delivered = true
				}
			}
			if !inSegment {
				if us.asked == 0 || !delivered {
					t.Fatalf("control: an ordinary poll_update asked the store %d time(s) and delivered=%v, so this probe cannot see a delivery", us.asked, delivered)
				}
				return
			}
			if us.asked != 0 || delivered {
				t.Errorf("a defer segment's body polled for updates: store asked %d time(s), delivered=%v", us.asked, delivered)
			}
			if got != callSuspendSentinel {
				t.Errorf("returned %#x, want the stop sentinel %#x", got, callSuspendSentinel)
			}
		})
	}
}

// set_query_state writes one in-memory map and nothing else: no event, no store.
// finishDeferPhase persists a segment's NEW HISTORY EVENTS and takes no query state
// (its signature has none), so nothing a body publishes here outlives the segment.
// This pins that: if set_query_state ever records an event or reaches a store, a
// segment body's call starts to persist, and this test is where that gets decided.
func TestADeferSegmentBodySetQueryStateChangesNothingDurable(t *testing.T) {
	s := bodyOfADeferSegment(nil, true, "wf-audit-query-state")
	before := len(s.history)
	got := s.SetQueryState(context.Background(), nil, "k", "v")

	if len(s.history) != before {
		t.Errorf("set_query_state appended %d history event(s) from a segment body; finishDeferPhase persists those", len(s.history)-before)
	}
	if got != 0 {
		t.Errorf("returned %#x; a call that changes nothing durable has no reason to refuse", got)
	}
	if s.queryState["k"] != "v" {
		t.Errorf("control: the in-memory state was not set (%v), so this probe cannot see what the call does", s.queryState)
	}
}

// A defer BODY must still be able to create a promise: cleanup that hands a result
// to something waiting on it is legitimate. Without this, the create_promise test
// above passes against a guard that refuses everything in a segment.
func TestADeferBodyCanStillCreateAPromise(t *testing.T) {
	for _, backend := range registeredBackends {
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)
			ps, ok := store.(PromiseStore)
			if !ok {
				t.Fatalf("UNMEASURED: %T is not a PromiseStore", store)
			}
			id := auditRun(t, store, "audit-create-promise-defer-body")

			s := bodyOfADeferSegment([]EngineOption{WithPromiseStore(ps)}, true, id)
			s.inDeferDrain = true
			got := s.CreatePromise(context.Background(), nil, "audit", 0, 0)

			var promiseID string
			for _, e := range s.history {
				if e.EventType == EventTypeCreatePromise {
					promiseID = e.PromiseID
				}
			}
			if got == callSuspendSentinel || promiseID == "" {
				t.Fatalf("a defer body's create_promise was refused (returned %#x, event id %q): the guard has removed the one legitimate use", got, promiseID)
			}
			if _, _, _, err := ps.GetPromise(context.Background(), id, promiseID); err != nil {
				t.Errorf("a defer body's create_promise left no row for %q: %v", promiseID, err)
			}
		})
	}
}
