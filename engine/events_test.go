package engine

import (
	"testing"
)

// ---------------------------------------------------------------------------
// Step and Type tests — one case per event type
// ---------------------------------------------------------------------------

func TestEvent_StepAndType(t *testing.T) {
	tests := []struct {
		name     string
		event    Event
		wantStep int
		wantType EventType
	}{
		{"CallEvent", CallEvent{step: 1}, 1, EventTypeCall},
		{"AwaitSignalsEvent", AwaitSignalsEvent{step: 2}, 2, EventTypeAwaitSignals},
		{"SignalReceivedEvent", SignalReceivedEvent{step: 3}, 3, EventTypeSignalReceived},
		{"DeferEvent", DeferEvent{step: 4}, 4, EventTypeDefer},
		{"ChildWorkflowEvent", ChildWorkflowEvent{step: 5}, 5, EventTypeChildWorkflow},
		{"AwaitChildEvent", AwaitChildEvent{step: 6}, 6, EventTypeAwaitChild},
		{"AwaitAllChildrenEvent", AwaitAllChildrenEvent{step: 7}, 7, EventTypeAwaitAllChildren},
		{"ContinueAsNewEvent", ContinueAsNewEvent{step: 8}, 8, EventTypeContinueAsNew},
		{"HeartbeatEvent", HeartbeatEvent{step: 9}, 9, EventTypeHeartbeat},
		{"PluginCallEvent", PluginCallEvent{step: 10}, 10, EventTypePluginCall},
		{"PluginCallStreamChunkEvent", PluginCallStreamChunkEvent{step: 11}, 11, EventTypePluginCallStreamChunk},
		{"CreatePromiseEvent", CreatePromiseEvent{step: 12}, 12, EventTypeCreatePromise},
		{"AwaitPromiseEvent", AwaitPromiseEvent{step: 13}, 13, EventTypeAwaitPromise},
		{"PromiseResolvedEvent", PromiseResolvedEvent{step: 14}, 14, EventTypePromiseResolved},
		{"PromiseRejectedEvent", PromiseRejectedEvent{step: 15}, 15, EventTypePromiseRejected},
		{"UpdateHandlerEvent", UpdateHandlerEvent{step: 16}, 16, EventTypeUpdateHandler},
		{"StateMutationEvent", StateMutationEvent{step: 17}, 17, EventTypeStateMutation},
		{"RunDetachedEvent", RunDetachedEvent{step: 18}, 18, EventTypeRunDetached},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.event.Step(); got != tt.wantStep {
				t.Errorf("Step() = %d, want %d", got, tt.wantStep)
			}
			if got := tt.event.Type(); got != tt.wantType {
				t.Errorf("Type() = %s, want %s", got, tt.wantType)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Round-trip: Event → EventRecord → Event for all 18 types
// ---------------------------------------------------------------------------

func TestEvent_RoundTrip(t *testing.T) {
	// Verify that converting a typed event to EventRecord and back
	// preserves all fields.

	t.Run("CallEvent", func(t *testing.T) {
		orig := CallEvent{step: 1, Service: "svc", Op: "op", Request: "req", Response: "resp", Err: "err"}
		rec := EventRecordFromEvent(orig)
		if rec.Step != 1 || rec.EventType != EventTypeCall {
			t.Errorf("EventRecord: step=%d type=%s", rec.Step, rec.EventType)
		}
		if rec.Service != "svc" || rec.Op != "op" || rec.Request != "req" || rec.Response != "resp" || rec.Err != "err" {
			t.Errorf("EventRecord fields: service=%q op=%q request=%q response=%q err=%q", rec.Service, rec.Op, rec.Request, rec.Response, rec.Err)
		}
		got, ok := EventFromRecord(rec).(CallEvent)
		if !ok {
			t.Fatalf("EventFromRecord returned %T, want CallEvent", EventFromRecord(rec))
		}
		if got.step != 1 || got.Service != "svc" || got.Op != "op" || got.Request != "req" || got.Response != "resp" || got.Err != "err" {
			t.Errorf("round-trip mismatch")
		}
	})

	t.Run("AwaitSignalsEvent", func(t *testing.T) {
		orig := AwaitSignalsEvent{step: 2, SignalNames: "a,b", TimeoutMs: 5000}
		rec := EventRecordFromEvent(orig)
		if rec.Step != 2 || rec.EventType != EventTypeAwaitSignals {
			t.Errorf("EventRecord: step=%d type=%s", rec.Step, rec.EventType)
		}
		if rec.SignalNames != "a,b" || rec.TimeoutMs != 5000 {
			t.Errorf("EventRecord fields mismatch")
		}
		got, ok := EventFromRecord(rec).(AwaitSignalsEvent)
		if !ok {
			t.Fatalf("EventFromRecord returned %T", EventFromRecord(rec))
		}
		if got.step != 2 || got.SignalNames != "a,b" || got.TimeoutMs != 5000 {
			t.Errorf("round-trip mismatch")
		}
	})

	t.Run("SignalReceivedEvent", func(t *testing.T) {
		orig := SignalReceivedEvent{step: 3, SignalName: "sig1", SignalPayload: `{"x":1}`}
		rec := EventRecordFromEvent(orig)
		if rec.Step != 3 || rec.EventType != EventTypeSignalReceived {
			t.Errorf("EventRecord: step=%d type=%s", rec.Step, rec.EventType)
		}
		if rec.SignalName != "sig1" || rec.SignalPayload != `{"x":1}` {
			t.Errorf("EventRecord fields mismatch")
		}
		got, ok := EventFromRecord(rec).(SignalReceivedEvent)
		if !ok {
			t.Fatalf("EventFromRecord returned %T", EventFromRecord(rec))
		}
		if got.step != 3 || got.SignalName != "sig1" || got.SignalPayload != `{"x":1}` {
			t.Errorf("round-trip mismatch")
		}
	})

	t.Run("DeferEvent", func(t *testing.T) {
		orig := DeferEvent{step: 4, Description: "cleanup", DeferID: "def-1"}
		rec := EventRecordFromEvent(orig)
		if rec.Step != 4 || rec.EventType != EventTypeDefer {
			t.Errorf("EventRecord: step=%d type=%s", rec.Step, rec.EventType)
		}
		if rec.DeferDescription != "cleanup" || rec.DeferID != "def-1" {
			t.Errorf("EventRecord fields mismatch")
		}
		got, ok := EventFromRecord(rec).(DeferEvent)
		if !ok {
			t.Fatalf("EventFromRecord returned %T", EventFromRecord(rec))
		}
		if got.step != 4 || got.Description != "cleanup" || got.DeferID != "def-1" {
			t.Errorf("round-trip mismatch")
		}
	})

	t.Run("ChildWorkflowEvent", func(t *testing.T) {
		orig := ChildWorkflowEvent{step: 5, DefName: "child1", Input: "in", ChildID: "cid-1", ParentWorkflowID: "pid-1"}
		rec := EventRecordFromEvent(orig)
		if rec.Step != 5 || rec.EventType != EventTypeChildWorkflow {
			t.Errorf("EventRecord: step=%d type=%s", rec.Step, rec.EventType)
		}
		if rec.ChildName != "child1" || rec.ChildInput != "in" || rec.RunID != "cid-1" || rec.ParentWorkflowID != "pid-1" {
			t.Errorf("EventRecord fields mismatch")
		}
		got, ok := EventFromRecord(rec).(ChildWorkflowEvent)
		if !ok {
			t.Fatalf("EventFromRecord returned %T", EventFromRecord(rec))
		}
		if got.step != 5 || got.DefName != "child1" || got.Input != "in" || got.ChildID != "cid-1" || got.ParentWorkflowID != "pid-1" {
			t.Errorf("round-trip mismatch")
		}
	})

	t.Run("AwaitChildEvent", func(t *testing.T) {
		orig := AwaitChildEvent{step: 6, RunID: "run-1", Response: "resp", Err: "err"}
		rec := EventRecordFromEvent(orig)
		if rec.Step != 6 || rec.EventType != EventTypeAwaitChild {
			t.Errorf("EventRecord: step=%d type=%s", rec.Step, rec.EventType)
		}
		if rec.RunID != "run-1" || rec.Response != "resp" || rec.Err != "err" {
			t.Errorf("EventRecord fields mismatch")
		}
		got, ok := EventFromRecord(rec).(AwaitChildEvent)
		if !ok {
			t.Fatalf("EventFromRecord returned %T", EventFromRecord(rec))
		}
		if got.step != 6 || got.RunID != "run-1" || got.Response != "resp" || got.Err != "err" {
			t.Errorf("round-trip mismatch")
		}
	})

	t.Run("AwaitAllChildrenEvent", func(t *testing.T) {
		orig := AwaitAllChildrenEvent{step: 7, RunIDsJSON: `["a"]`, OutcomesJSON: `{"a":"ok"}`}
		rec := EventRecordFromEvent(orig)
		if rec.Step != 7 || rec.EventType != EventTypeAwaitAllChildren {
			t.Errorf("EventRecord: step=%d type=%s", rec.Step, rec.EventType)
		}
		if rec.Request != `["a"]` || rec.Response != `{"a":"ok"}` {
			t.Errorf("EventRecord fields mismatch")
		}
		got, ok := EventFromRecord(rec).(AwaitAllChildrenEvent)
		if !ok {
			t.Fatalf("EventFromRecord returned %T", EventFromRecord(rec))
		}
		if got.step != 7 || got.RunIDsJSON != `["a"]` || got.OutcomesJSON != `{"a":"ok"}` {
			t.Errorf("round-trip mismatch")
		}
	})

	t.Run("ContinueAsNewEvent", func(t *testing.T) {
		orig := ContinueAsNewEvent{step: 8, NewInput: "new-input"}
		rec := EventRecordFromEvent(orig)
		if rec.Step != 8 || rec.EventType != EventTypeContinueAsNew {
			t.Errorf("EventRecord: step=%d type=%s", rec.Step, rec.EventType)
		}
		if rec.NewInput != "new-input" {
			t.Errorf("EventRecord fields mismatch")
		}
		got, ok := EventFromRecord(rec).(ContinueAsNewEvent)
		if !ok {
			t.Fatalf("EventFromRecord returned %T", EventFromRecord(rec))
		}
		if got.step != 8 || got.NewInput != "new-input" {
			t.Errorf("round-trip mismatch")
		}
	})

	t.Run("HeartbeatEvent", func(t *testing.T) {
		orig := HeartbeatEvent{step: 9}
		rec := EventRecordFromEvent(orig)
		if rec.Step != 9 || rec.EventType != EventTypeHeartbeat {
			t.Errorf("EventRecord: step=%d type=%s", rec.Step, rec.EventType)
		}
		got, ok := EventFromRecord(rec).(HeartbeatEvent)
		if !ok {
			t.Fatalf("EventFromRecord returned %T", EventFromRecord(rec))
		}
		if got.step != 9 {
			t.Errorf("round-trip mismatch")
		}
	})

	t.Run("PluginCallEvent", func(t *testing.T) {
		orig := PluginCallEvent{step: 10, PluginName: "p", FuncName: "f", Input: "in", Output: "out", Err: "err", Idempotent: true}
		rec := EventRecordFromEvent(orig)
		if rec.Step != 10 || rec.EventType != EventTypePluginCall {
			t.Errorf("EventRecord: step=%d type=%s", rec.Step, rec.EventType)
		}
		if rec.PluginName != "p" || rec.PluginFunc != "f" || rec.PluginInput != "in" || rec.PluginOutput != "out" || rec.PluginError != "err" || !rec.Idempotent {
			t.Errorf("EventRecord fields mismatch")
		}
		got, ok := EventFromRecord(rec).(PluginCallEvent)
		if !ok {
			t.Fatalf("EventFromRecord returned %T", EventFromRecord(rec))
		}
		if got.step != 10 || got.PluginName != "p" || got.FuncName != "f" || got.Input != "in" || got.Output != "out" || got.Err != "err" || !got.Idempotent {
			t.Errorf("round-trip mismatch")
		}
	})

	t.Run("PluginCallStreamChunkEvent", func(t *testing.T) {
		orig := PluginCallStreamChunkEvent{step: 11, PluginName: "p2", FuncName: "f2", Input: "in", Output: "out", ChunkIndex: 3, Finish: true}
		rec := EventRecordFromEvent(orig)
		if rec.Step != 11 || rec.EventType != EventTypePluginCallStreamChunk {
			t.Errorf("EventRecord: step=%d type=%s", rec.Step, rec.EventType)
		}
		if rec.PluginName != "p2" || rec.PluginFunc != "f2" || rec.PluginInput != "in" || rec.PluginOutput != "out" || rec.StreamChunkIndex != 3 || !rec.StreamFinish {
			t.Errorf("EventRecord fields mismatch")
		}
		got, ok := EventFromRecord(rec).(PluginCallStreamChunkEvent)
		if !ok {
			t.Fatalf("EventFromRecord returned %T", EventFromRecord(rec))
		}
		if got.step != 11 || got.PluginName != "p2" || got.FuncName != "f2" || got.Input != "in" || got.Output != "out" || got.ChunkIndex != 3 || !got.Finish {
			t.Errorf("round-trip mismatch")
		}
	})

	t.Run("CreatePromiseEvent", func(t *testing.T) {
		orig := CreatePromiseEvent{step: 12, PromiseName: "pname", PromiseID: "pid-1"}
		rec := EventRecordFromEvent(orig)
		if rec.Step != 12 || rec.EventType != EventTypeCreatePromise {
			t.Errorf("EventRecord: step=%d type=%s", rec.Step, rec.EventType)
		}
		if rec.PromiseName != "pname" || rec.PromiseID != "pid-1" {
			t.Errorf("EventRecord fields mismatch")
		}
		got, ok := EventFromRecord(rec).(CreatePromiseEvent)
		if !ok {
			t.Fatalf("EventFromRecord returned %T", EventFromRecord(rec))
		}
		if got.step != 12 || got.PromiseName != "pname" || got.PromiseID != "pid-1" {
			t.Errorf("round-trip mismatch")
		}
	})

	t.Run("AwaitPromiseEvent", func(t *testing.T) {
		orig := AwaitPromiseEvent{step: 13, PromiseID: "pid-2"}
		rec := EventRecordFromEvent(orig)
		if rec.Step != 13 || rec.EventType != EventTypeAwaitPromise {
			t.Errorf("EventRecord: step=%d type=%s", rec.Step, rec.EventType)
		}
		if rec.PromiseID != "pid-2" {
			t.Errorf("EventRecord fields mismatch")
		}
		got, ok := EventFromRecord(rec).(AwaitPromiseEvent)
		if !ok {
			t.Fatalf("EventFromRecord returned %T", EventFromRecord(rec))
		}
		if got.step != 13 || got.PromiseID != "pid-2" {
			t.Errorf("round-trip mismatch")
		}
	})

	t.Run("PromiseResolvedEvent", func(t *testing.T) {
		orig := PromiseResolvedEvent{step: 14, PromiseID: "pid-3", Result: "ok"}
		rec := EventRecordFromEvent(orig)
		if rec.Step != 14 || rec.EventType != EventTypePromiseResolved {
			t.Errorf("EventRecord: step=%d type=%s", rec.Step, rec.EventType)
		}
		if rec.PromiseID != "pid-3" || rec.PromiseResult != "ok" {
			t.Errorf("EventRecord fields mismatch")
		}
		got, ok := EventFromRecord(rec).(PromiseResolvedEvent)
		if !ok {
			t.Fatalf("EventFromRecord returned %T", EventFromRecord(rec))
		}
		if got.step != 14 || got.PromiseID != "pid-3" || got.Result != "ok" {
			t.Errorf("round-trip mismatch")
		}
	})

	t.Run("PromiseRejectedEvent", func(t *testing.T) {
		orig := PromiseRejectedEvent{step: 15, PromiseID: "pid-4", Err: "rejected"}
		rec := EventRecordFromEvent(orig)
		if rec.Step != 15 || rec.EventType != EventTypePromiseRejected {
			t.Errorf("EventRecord: step=%d type=%s", rec.Step, rec.EventType)
		}
		if rec.PromiseID != "pid-4" || rec.PromiseError != "rejected" {
			t.Errorf("EventRecord fields mismatch")
		}
		got, ok := EventFromRecord(rec).(PromiseRejectedEvent)
		if !ok {
			t.Fatalf("EventFromRecord returned %T", EventFromRecord(rec))
		}
		if got.step != 15 || got.PromiseID != "pid-4" || got.Err != "rejected" {
			t.Errorf("round-trip mismatch")
		}
	})

	t.Run("UpdateHandlerEvent", func(t *testing.T) {
		orig := UpdateHandlerEvent{step: 16, HandlerName: "h1"}
		rec := EventRecordFromEvent(orig)
		if rec.Step != 16 || rec.EventType != EventTypeUpdateHandler {
			t.Errorf("EventRecord: step=%d type=%s", rec.Step, rec.EventType)
		}
		if rec.UpdateHandlerName != "h1" {
			t.Errorf("EventRecord fields mismatch")
		}
		got, ok := EventFromRecord(rec).(UpdateHandlerEvent)
		if !ok {
			t.Fatalf("EventFromRecord returned %T", EventFromRecord(rec))
		}
		if got.step != 16 || got.HandlerName != "h1" {
			t.Errorf("round-trip mismatch")
		}
	})

	t.Run("StateMutationEvent", func(t *testing.T) {
		orig := StateMutationEvent{step: 17, Key: "k", Value: "v", Delta: 5, Op: "set"}
		rec := EventRecordFromEvent(orig)
		if rec.Step != 17 || rec.EventType != EventTypeStateMutation {
			t.Errorf("EventRecord: step=%d type=%s", rec.Step, rec.EventType)
		}
		if rec.StateKey != "k" || rec.StateValue != "v" || rec.StateDelta != 5 || rec.StateOp != "set" {
			t.Errorf("EventRecord fields mismatch")
		}
		got, ok := EventFromRecord(rec).(StateMutationEvent)
		if !ok {
			t.Fatalf("EventFromRecord returned %T", EventFromRecord(rec))
		}
		if got.step != 17 || got.Key != "k" || got.Value != "v" || got.Delta != 5 || got.Op != "set" {
			t.Errorf("round-trip mismatch")
		}
	})

	t.Run("RunDetachedEvent", func(t *testing.T) {
		orig := RunDetachedEvent{step: 18}
		rec := EventRecordFromEvent(orig)
		if rec.Step != 18 || rec.EventType != EventTypeRunDetached {
			t.Errorf("EventRecord: step=%d type=%s", rec.Step, rec.EventType)
		}
		got, ok := EventFromRecord(rec).(RunDetachedEvent)
		if !ok {
			t.Fatalf("EventFromRecord returned %T", EventFromRecord(rec))
		}
		if got.step != 18 {
			t.Errorf("round-trip mismatch")
		}
	})
}

// ---------------------------------------------------------------------------
// Error paths
// ---------------------------------------------------------------------------

func TestEventFromRecord_UnknownType(t *testing.T) {
	rec := EventRecord{Step: 1, EventType: "nonexistent"}
	got := EventFromRecord(rec)
	if got != nil {
		t.Errorf("EventFromRecord(unknown type) = %T, want nil", got)
	}
}

// unknownEvent is a minimal Event implementation for testing the default
// branch of EventRecordFromEvent.
type unknownEvent struct {
	step int
	typ  EventType
}

func (e unknownEvent) Step() int       { return e.step }
func (e unknownEvent) Type() EventType { return e.typ }

func TestEventRecordFromEvent_UnknownEvent(t *testing.T) {
	orig := unknownEvent{step: 42, typ: EventType("custom_type")}
	rec := EventRecordFromEvent(orig)
	if rec.Step != 42 {
		t.Errorf("Step = %d, want 42", rec.Step)
	}
	if rec.EventType != EventType("custom_type") {
		t.Errorf("EventType = %s, want custom_type", rec.EventType)
	}
}

// ---------------------------------------------------------------------------
// Batch conversion
// ---------------------------------------------------------------------------

func TestEventsFromRecords(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		events := EventsFromRecords(nil)
		if len(events) != 0 {
			t.Errorf("len = %d, want 0", len(events))
		}
	})

	t.Run("single", func(t *testing.T) {
		records := []EventRecord{{Step: 1, EventType: EventTypeCall, Service: "s", Op: "o"}}
		events := EventsFromRecords(records)
		if len(events) != 1 {
			t.Fatalf("len = %d, want 1", len(events))
		}
		ce, ok := events[0].(CallEvent)
		if !ok {
			t.Fatalf("events[0] is %T, want CallEvent", events[0])
		}
		if ce.step != 1 || ce.Service != "s" || ce.Op != "o" {
			t.Errorf("fields mismatch")
		}
	})

	t.Run("multiple", func(t *testing.T) {
		records := []EventRecord{
			{Step: 1, EventType: EventTypeCall, Service: "a"},
			{Step: 2, EventType: EventTypeCall, Service: "b"},
			{Step: 3, EventType: EventTypeCall, Service: "c"},
		}
		events := EventsFromRecords(records)
		if len(events) != 3 {
			t.Fatalf("len = %d, want 3", len(events))
		}
		for i, e := range events {
			ce, ok := e.(CallEvent)
			if !ok {
				t.Fatalf("events[%d] is %T", i, events[i])
			}
			if ce.step != i+1 {
				t.Errorf("events[%d].step = %d, want %d", i, ce.step, i+1)
			}
		}
	})

	t.Run("unknown_type_yields_nil", func(t *testing.T) {
		records := []EventRecord{{Step: 1, EventType: "nonexistent"}}
		events := EventsFromRecords(records)
		if len(events) != 1 {
			t.Fatalf("len = %d, want 1", len(events))
		}
		if events[0] != nil {
			t.Errorf("events[0] = %v, want nil", events[0])
		}
	})
}

func TestEvent_RecordsFromEvents(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		records := RecordsFromEvents(nil)
		if len(records) != 0 {
			t.Errorf("len = %d, want 0", len(records))
		}
	})

	t.Run("single", func(t *testing.T) {
		events := []Event{CallEvent{step: 1, Service: "s", Op: "o"}}
		records := RecordsFromEvents(events)
		if len(records) != 1 {
			t.Fatalf("len = %d, want 1", len(records))
		}
		if records[0].Step != 1 || records[0].Service != "s" || records[0].Op != "o" {
			t.Errorf("fields mismatch")
		}
	})

	t.Run("multiple", func(t *testing.T) {
		events := []Event{
			HeartbeatEvent{step: 1},
			HeartbeatEvent{step: 2},
			HeartbeatEvent{step: 3},
		}
		records := RecordsFromEvents(events)
		if len(records) != 3 {
			t.Fatalf("len = %d, want 3", len(records))
		}
		for i, r := range records {
			if r.Step != i+1 || r.EventType != EventTypeHeartbeat {
				t.Errorf("records[%d] = {step:%d type:%s}", i, r.Step, r.EventType)
			}
		}
	})
}
