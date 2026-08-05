package engine

// IMPROVEMENT-PLAN 1.4 phase D. These run against every configured backend,
// because write-ahead intent is two SQL statements whose whole value is their
// ordering, and the three dialects disagree about placeholders and clocks.
//
// The design doc's test plan (§7) is explicit that this feature's history is of
// code that passes tests without running, and that the assertions must be about
// what the external service was actually asked to do. T1 and T2 of that plan
// live in tests/crash, which kills a real worker. What is here is the layer
// below: that the intent row is committed *before* the call is dispatched, that
// a pending row does not read as corruption, and that replay reports ambiguity
// rather than repeating the call.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

const intentService = "billing"
const intentOperation = "charge"

// intentTestCaller records what it was asked to do and, while the call is in
// flight, reads the workflow's history back through the store.
//
// That read is the point. The guarantee is not "an intent row exists
// afterwards" -- a naive implementation that wrote the intent after the call
// would satisfy that -- it is that the row is durable *before* the side effect
// happens. The only moment that can be observed is from inside the call.
type intentTestCaller struct {
	store      WorkflowStore
	workflowID string

	calls          int
	pendingDuring  bool
	historyDuring  []EventRecord
	errDuringRead  error
	responseToGive string
	errToReturn    error
}

func (c *intentTestCaller) Call(ctx context.Context, service, operation, request string) (string, error) {
	c.calls++
	hist, err := c.store.LoadEventHistory(context.Background(), c.workflowID)
	if err != nil {
		c.errDuringRead = err
	}
	c.historyDuring = hist
	for _, rec := range hist {
		if rec.Pending {
			c.pendingDuring = true
		}
	}
	if c.errToReturn != nil {
		return "", c.errToReturn
	}
	return c.responseToGive, nil
}

// newIntentWorkflow creates a workflow row the event rows can hang off, and
// returns its ID.
func newIntentWorkflow(t *testing.T, ctx context.Context, store WorkflowStore, name string) string {
	t.Helper()
	const defName = "intent-workflow"
	if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
		Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d},
		ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}
	wfID := fmt.Sprintf("%s-%d", name, time.Now().UnixNano())
	if _, _, err := store.StartNewRun(ctx, wfID, defName, 1,
		json.RawMessage(`{}`), "", DefaultTenantUUID, 0); err != nil {
		t.Fatalf("StartNewRun: %v", err)
	}
	return wfID
}

func intentStoreOf(t *testing.T, store WorkflowStore) callIntentStore {
	t.Helper()
	st, ok := store.(callIntentStore)
	if !ok {
		t.Fatalf("%T does not implement callIntentStore", store)
	}
	return st
}

func stepRecord(t *testing.T, ctx context.Context, store WorkflowStore, wfID string, step int) EventRecord {
	t.Helper()
	hist, err := store.LoadEventHistory(ctx, wfID)
	if err != nil {
		t.Fatalf("LoadEventHistory: %v", err)
	}
	for _, rec := range hist {
		if rec.Step == step {
			return rec
		}
	}
	t.Fatalf("no event at step %d (history has %d events)", step, len(hist))
	return EventRecord{}
}

// TestCallIntent_PendingRowIsNotCorruption is the defect that broke the
// deleted implementation, checked directly.
//
// flushCallIntent computed the checksum over a record whose Err was empty and
// then stored a sentinel in the error column, so in the exact crash window the
// feature exists to handle, replay failed checksum verification instead of
// reporting ambiguity. A pending row here carries no checksum at all, and
// VerifyWorkflowEvents already skips rows that have none.
func TestCallIntent_PendingRowIsNotCorruption(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store WorkflowStore) {
		ctx := context.Background()
		wfID := newIntentWorkflow(t, ctx, store, "intent-pending")
		st := intentStoreOf(t, store)

		intent := EventRecord{
			Step: 0, EventType: EventTypeCall,
			Service: intentService, Op: intentOperation, Request: `{"amount":100}`,
		}
		if err := st.WriteCallIntent(ctx, wfID, intent); err != nil {
			t.Fatalf("WriteCallIntent: %v", err)
		}

		rec := stepRecord(t, ctx, store, wfID, 0)
		if !rec.Pending {
			t.Error("the row is not reported as pending, so a crash here would be indistinguishable " +
				"from a call that never ran")
		}
		if rec.Service != intentService || rec.Op != intentOperation {
			t.Errorf("intent row lost its identity: service=%q op=%q", rec.Service, rec.Op)
		}
		if err := store.VerifyWorkflowEvents(ctx, wfID); err != nil {
			t.Errorf("a pending row reads as corruption: %v", err)
		}

		done := rec
		done.Response = `{"charged":true}`
		done.TimestampMs = time.Now().UnixMilli()
		payload, _ := eventRecordToPayload(done)
		checksum := computeEventChecksum(done, "")
		if err := st.CompleteCallIntent(ctx, wfID, done, payload, checksum); err != nil {
			t.Fatalf("CompleteCallIntent: %v", err)
		}

		after := stepRecord(t, ctx, store, wfID, 0)
		if after.Pending {
			t.Error("the row is still pending after completion, so every replay would report ambiguity forever")
		}
		if !strings.Contains(after.Response, "charged") {
			t.Errorf("response = %q, want the completed outcome", after.Response)
		}
		if err := store.VerifyWorkflowEvents(ctx, wfID); err != nil {
			t.Errorf("VerifyWorkflowEvents after completion: %v", err)
		}
	})
}

// TestCallIntent_CompletionIsFenced pins the guard on the completing UPDATE.
// A completion that matches no pending row must say so: the two ways it
// happens are "the intent was never written" and "something else already
// resolved this step", and both are worth an error rather than a no-op.
func TestCallIntent_CompletionIsFenced(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store WorkflowStore) {
		ctx := context.Background()
		wfID := newIntentWorkflow(t, ctx, store, "intent-fence")
		st := intentStoreOf(t, store)

		rec := EventRecord{
			Step: 0, EventType: EventTypeCall,
			Service: intentService, Op: intentOperation, Request: `{}`,
			Response: `{"ok":true}`, TimestampMs: time.Now().UnixMilli(),
		}
		payload, _ := eventRecordToPayload(rec)
		checksum := computeEventChecksum(rec, "")

		// No intent was written, so there is nothing to complete.
		if err := st.CompleteCallIntent(ctx, wfID, rec, payload, checksum); !errors.Is(err, errIntentNotPending) {
			t.Errorf("completing a step with no intent returned %v, want errIntentNotPending", err)
		}

		if err := st.WriteCallIntent(ctx, wfID, EventRecord{
			Step: 0, EventType: EventTypeCall, Service: intentService, Op: intentOperation, Request: `{}`,
		}); err != nil {
			t.Fatalf("WriteCallIntent: %v", err)
		}
		if err := st.CompleteCallIntent(ctx, wfID, rec, payload, checksum); err != nil {
			t.Fatalf("first CompleteCallIntent: %v", err)
		}

		// The second completion carries a DIFFERENT outcome, deliberately.
		// MySQL's RowsAffected counts rows *changed*, not rows matched, so a
		// re-completion with identical values reports 0 there whether or not
		// the pending guard is in the WHERE clause -- the assertion would pass
		// against a store with no fence at all. A different response makes the
		// row change if the guard is missing, so all three dialects discriminate.
		conflicting := rec
		conflicting.Response = `{"ok":false,"by":"a second writer"}`
		conflictingPayload, _ := eventRecordToPayload(conflicting)
		conflictingChecksum := computeEventChecksum(conflicting, "")
		if err := st.CompleteCallIntent(ctx, wfID, conflicting, conflictingPayload, conflictingChecksum); !errors.Is(err, errIntentNotPending) {
			t.Errorf("second completion returned %v, want errIntentNotPending", err)
		}
		if after := stepRecord(t, ctx, store, wfID, 0); strings.Contains(after.Response, "second writer") {
			t.Errorf("the second completion overwrote the first outcome: response = %q", after.Response)
		}
	})
}

// TestDurableCall_CommitsIntentBeforeDispatch is the headline: the ordering
// that makes the whole feature worth its extra round trip.
//
// The assertion is made from inside the call, because that is the only moment
// at which "the intent was durable before the side effect" is observable. An
// implementation that wrote the intent after the call, or in the same
// transaction as the outcome, would pass every after-the-fact check and fail
// this one.
func TestDurableCall_CommitsIntentBeforeDispatch(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store WorkflowStore) {
		ctx := context.Background()
		wfID := newIntentWorkflow(t, ctx, store, "intent-order")

		caller := &intentTestCaller{
			store: store, workflowID: wfID, responseToGive: `{"charged":true}`,
		}
		s := &execSession{
			engine: NewEngine(nil, caller,
				WithWorkflowStore(store),
				WithWorkflowID(wfID),
				WithWriteAheadIntentOps(intentService+"."+intentOperation)),
			workflowID: wfID,
		}

		result := s.DurableCall(ctx, nil, intentService, intentOperation, `{"amount":100}`, 0, 0)
		if errCodeOf(result) != 0 {
			t.Fatalf("DurableCall reported failure (code %d)", callErrorCodeOf(result))
		}
		if caller.calls != 1 {
			t.Fatalf("the service was called %d times, want 1", caller.calls)
		}
		if caller.errDuringRead != nil {
			t.Fatalf("reading history during the call: %v", caller.errDuringRead)
		}

		if !caller.pendingDuring {
			t.Errorf("no pending intent row was visible while the call was in flight (history held %d events) -- "+
				"the side effect happened before its intent was durable, which is the whole guarantee",
				len(caller.historyDuring))
		}

		after := stepRecord(t, ctx, store, wfID, 0)
		if after.Pending {
			t.Error("the row is still pending after a successful call")
		}
		if !strings.Contains(after.Response, "charged") {
			t.Errorf("stored response = %q, want the outcome the service returned", after.Response)
		}
		if err := store.VerifyWorkflowEvents(ctx, wfID); err != nil {
			t.Errorf("VerifyWorkflowEvents after an intent call: %v", err)
		}
	})
}

// TestDurableCall_IntentPathRecordsFailures checks the other outcome: a call
// that fails still completes its row, rather than leaving it pending. A failed
// call is not an ambiguous one -- the service answered.
func TestDurableCall_IntentPathRecordsFailures(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store WorkflowStore) {
		ctx := context.Background()
		wfID := newIntentWorkflow(t, ctx, store, "intent-failure")

		caller := &intentTestCaller{
			store: store, workflowID: wfID, errToReturn: errors.New("card declined"),
		}
		s := &execSession{
			engine: NewEngine(nil, caller,
				WithWorkflowStore(store),
				WithWorkflowID(wfID),
				WithWriteAheadIntentOps(intentService+"."+intentOperation)),
			workflowID: wfID,
		}

		result := s.DurableCall(ctx, nil, intentService, intentOperation, `{}`, 0, 0)
		if errCodeOf(result) == 0 {
			t.Error("a failed call reported success")
		}

		after := stepRecord(t, ctx, store, wfID, 0)
		if after.Pending {
			t.Error("a call the service answered with an error was left pending, so replay would " +
				"report it as ambiguous and the workflow could never make progress")
		}
		if !strings.Contains(after.Err, "declined") {
			t.Errorf("stored error = %q, want the service's error", after.Err)
		}
	})
}

// TestReplay_PendingIntentIsAmbiguous is the read half, against a row a crash
// would actually leave behind.
//
// The distinction that matters: a pending row carries no response and no error,
// so without the detector replay would hand the workflow an empty success --
// which is worse than a duplicate call, because the workflow would proceed on a
// result the service never returned.
func TestReplay_PendingIntentIsAmbiguous(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store WorkflowStore) {
		ctx := context.Background()
		wfID := newIntentWorkflow(t, ctx, store, "intent-replay")
		st := intentStoreOf(t, store)

		// Exactly what a crash between dispatch and completion leaves.
		if err := st.WriteCallIntent(ctx, wfID, EventRecord{
			Step: 0, EventType: EventTypeCall,
			Service: intentService, Op: intentOperation, Request: `{"amount":100}`,
		}); err != nil {
			t.Fatalf("WriteCallIntent: %v", err)
		}

		history, err := store.LoadEventHistory(ctx, wfID)
		if err != nil {
			t.Fatalf("LoadEventHistory: %v", err)
		}

		caller := &intentTestCaller{store: store, workflowID: wfID, responseToGive: `{"charged":true}`}
		s := &execSession{
			engine:     NewEngine(nil, caller, WithWorkflowStore(store), WithWorkflowID(wfID)),
			workflowID: wfID,
			isReplay:   true,
			history:    history,
		}

		result := s.DurableCall(ctx, nil, intentService, intentOperation, `{"amount":100}`, 0, 0)
		if errCodeOf(result) == 0 {
			t.Fatal("replaying a pending intent reported success, so the workflow would continue on a " +
				"result the service never returned")
		}
		if got := callErrorCodeOf(result); got != callErrorUnknown {
			t.Errorf("callErrorCode = %d, want callErrorUnknown (%d): an ambiguous outcome is not a "+
				"failure the guest may retry", got, callErrorUnknown)
		}
		if caller.calls != 0 {
			t.Errorf("replay called the service %d times; the point of detecting ambiguity is not to "+
				"repeat the call", caller.calls)
		}
	})
}

// TestWriteAheadIntent_FailsLoudlyWhenUnsupported covers the configuration
// mistake. A store that cannot honour the guarantee must produce an error, not
// a silent downgrade to at-least-once: a durability guarantee that is
// configured, believed and absent is the exact shape of 1.4 itself.
func TestWriteAheadIntent_FailsLoudlyWhenUnsupported(t *testing.T) {
	e := NewEngine(nil, &mockCaller{},
		WithWorkflowStore(&stubWorkflowStore{}),
		WithWriteAheadIntentOps(intentService+"."+intentOperation))

	if got := e.callSemantics(intentService, intentOperation); got != WriteAheadIntent {
		t.Fatalf("callSemantics = %v, want WriteAheadIntent", got)
	}
	if got := e.callSemantics(intentService, "read"); got != AtLeastOnce {
		t.Errorf("an undeclared operation got %v, want AtLeastOnce -- the default must stay free", got)
	}

	_, err := e.intentStore()
	if err == nil {
		t.Fatal("a store with no intent support was accepted")
	}
	if !strings.Contains(err.Error(), "write-ahead call intent") {
		t.Errorf("error = %v, want it to name the missing capability", err)
	}
}

// TestDurableCall_IntentSurvivesNoPerStepFlush is a deliberate deviation from
// the design doc, checked rather than asserted.
//
// docs/durable-call-intent-design.md §5 says --no-per-step-flush "defeats this
// entirely -- it defers persistence to batch finalization, so the intent is not
// durable before dispatch", and requires the combination be rejected at
// startup. That is true of an implementation that routes the intent through
// flushEvent, which is what the design assumed. This one does not: it writes
// through the store's own WriteCallIntent, which never consults
// e.noPerStepFlush.
//
// So the two are orthogonal, and the honest response is a test that says so
// rather than a startup check forbidding a combination that works. If this ever
// starts failing, the startup rejection the design asks for is the fix.
func TestDurableCall_IntentSurvivesNoPerStepFlush(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store WorkflowStore) {
		ctx := context.Background()
		wfID := newIntentWorkflow(t, ctx, store, "intent-noflush")

		caller := &intentTestCaller{store: store, workflowID: wfID, responseToGive: `{"charged":true}`}
		s := &execSession{
			engine: NewEngine(nil, caller,
				WithWorkflowStore(store),
				WithWorkflowID(wfID),
				WithNoPerStepFlush(true),
				WithWriteAheadIntentOps(intentService+"."+intentOperation)),
			workflowID: wfID,
		}

		if result := s.DurableCall(ctx, nil, intentService, intentOperation, `{}`, 0, 0); errCodeOf(result) != 0 {
			t.Fatalf("DurableCall reported failure (code %d)", callErrorCodeOf(result))
		}
		if !caller.pendingDuring {
			t.Error("with --no-per-step-flush the intent was not durable before dispatch; the two settings " +
				"are not orthogonal after all, and the design's startup rejection is needed")
		}
		if after := stepRecord(t, ctx, store, wfID, 0); after.Pending {
			t.Error("the row is still pending after a successful call")
		}
	})
}
