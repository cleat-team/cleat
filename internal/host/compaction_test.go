package host

import (
	"fmt"
	"testing"
)

// TestPluginCallCompactionRoundTrip verifies that plugin_call and
// plugin_call_stream_chunk events survive a compaction round-trip.
func TestPluginCallCompactionRoundTrip(t *testing.T) {
	events := []EventRecord{
		{
			Step:      0,
			EventType: EventTypePluginCall,
			PluginName:   "test-plugin",
			PluginFunc:   "DoSomething",
			PluginInput:  `{"key":"value"}`,
			PluginOutput: `{"result":"ok"}`,
			PluginError:  "",
		},
		{
			Step:      1,
			EventType: EventTypePluginCallStreamChunk,
			PluginName:   "test-plugin",
			PluginFunc:   "DoSomething",
			PluginInput:  `{"key":"value"}`,
			PluginOutput: `{"chunk":1}`,
			PluginError:  "",
		},
		{
			Step:      2,
			EventType: EventTypePluginCallStreamChunk,
			PluginName:   "test-plugin",
			PluginFunc:   "DoSomething",
			PluginInput:  `{"key":"value"}`,
			PluginOutput: `{"chunk":2,"finish":true}`,
			PluginError:  "",
		},
		{
			Step:      3,
			EventType: EventTypePluginCall,
			PluginName:   "test-plugin",
			PluginFunc:   "GetObject",
			PluginInput:  `{"bucket":"x","key":"y"}`,
			PluginOutput: "",
			PluginError:  "not found",
		},
	}

	// Compact at every possible split point and verify round-trip.
	for split := 1; split <= len(events); split++ {
		compacted := events[:split]
		tail := events[split:]

		cs := extractCompactionState(compacted)
		reconstructed := buildFullHistoryFromCompaction(tail, cs)

		if len(reconstructed) != len(events) {
			t.Errorf("split=%d: length mismatch: got %d events, want %d",
				split, len(reconstructed), len(events))
			continue
		}

		for i := range events {
			if !eventFieldsMatch(events[i], reconstructed[i]) {
				t.Errorf("split=%d: event %d (%s) mismatch", split, i, events[i].EventType)
				dumpEventDiff(t, events[i], reconstructed[i])
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Compaction unit tests (pure functions, no DB needed)
// ---------------------------------------------------------------------------

// TestCompactionBelowThreshold verifies that extractCompactionState handles a
// small number of events correctly and buildFullHistoryFromCompaction
// reconstructs them faithfully. The threshold logic is in
// CompactWorkflowHistory (store-level), so here we test the pure functions.
func TestCompactionBelowThreshold(t *testing.T) {
	events := []EventRecord{
		{Step: 0, EventType: EventTypeCall, Service: "svc", Op: "op1", Request: `{}`, Response: `{"ok":true}`},
		{Step: 1, EventType: EventTypeCall, Service: "svc", Op: "op2", Request: `{}`, Response: `{"ok":true}`},
		{Step: 2, EventType: EventTypeCall, Service: "svc", Op: "op3", Request: `{}`, Response: `{"ok":true}`},
	}

	cs := extractCompactionState(events)
	if cs == nil {
		t.Fatal("expected non-nil CompactionState")
	}
	if cs.CompactedStep != 3 {
		t.Errorf("expected CompactedStep=3, got %d", cs.CompactedStep)
	}
	if len(cs.Events) != 3 {
		t.Fatalf("expected 3 compacted events, got %d", len(cs.Events))
	}

	reconstructed := buildFullHistoryFromCompaction(nil, cs)
	if len(reconstructed) != 3 {
		t.Fatalf("expected 3 reconstructed events, got %d", len(reconstructed))
	}
	for i := range events {
		if !eventFieldsMatch(events[i], reconstructed[i]) {
			t.Errorf("event %d mismatch", i)
			dumpEventDiff(t, events[i], reconstructed[i])
		}
	}
}

// TestCompactionAboveThreshold verifies that a large set of events is
// correctly compacted and reconstructed with the same semantics used by
// CompactWorkflowHistory (keep the most recent threshold/2 as tail).
func TestCompactionAboveThreshold(t *testing.T) {
	threshold := 10
	nEvents := 25
	events := make([]EventRecord, nEvents)
	for i := 0; i < nEvents; i++ {
		events[i] = EventRecord{
			Step:      i,
			EventType: EventTypeCall,
			Service:   "svc",
			Op:        fmt.Sprintf("op%d", i),
			Request:   `{}`,
			Response:  fmt.Sprintf(`{"step":%d}`, i),
		}
	}

	// Simulate logic from CompactWorkflowHistory:
	// If len > threshold, keepStep = len - threshold/2
	keepStep := nEvents - threshold/2
	if keepStep < 0 {
		keepStep = 0
	}
	compacted := events[:keepStep]
	tail := events[keepStep:]

	cs := extractCompactionState(compacted)
	if len(cs.Events) != keepStep {
		t.Fatalf("expected %d compacted events, got %d", keepStep, len(cs.Events))
	}

	reconstructed := buildFullHistoryFromCompaction(tail, cs)
	if len(reconstructed) != nEvents {
		t.Fatalf("expected %d events, got %d", nEvents, len(reconstructed))
	}
	for i := range events {
		if !eventFieldsMatch(events[i], reconstructed[i]) {
			t.Errorf("event %d mismatch", i)
			dumpEventDiff(t, events[i], reconstructed[i])
		}
	}
}

// TestCompactionPreservesRecentEvents verifies that the tail (most recent
// events) survives compaction untouched — they are passed through directly
// without being transformed into CompactedEvent and back.
func TestCompactionPreservesRecentEvents(t *testing.T) {
	nEvents := 15
	tailSize := 5

	events := make([]EventRecord, nEvents)
	for i := 0; i < nEvents; i++ {
		events[i] = EventRecord{
			Step:      i,
			EventType: EventTypeCall,
			Service:   "svc",
			Op:        fmt.Sprintf("op%d", i),
			Request:   fmt.Sprintf(`{"req":%d}`, i),
			Response:  fmt.Sprintf(`{"step":%d}`, i),
		}
	}

	compacted := events[:nEvents-tailSize]
	tail := events[nEvents-tailSize:]

	cs := extractCompactionState(compacted)
	reconstructed := buildFullHistoryFromCompaction(tail, cs)

	// The tail events should be identical objects (not reconstructed from JSON).
	for i, te := range tail {
		if reconstructed[len(compacted)+i].Response != te.Response {
			t.Errorf("tail event %d response: expected %q, got %q",
				i, te.Response, reconstructed[len(compacted)+i].Response)
		}
	}
}

// TestCompactionOfCompletedWorkflow verifies that all events of a completed
// workflow can be compacted (no tail) and the reconstructed history is
// faithful.
func TestCompactionOfCompletedWorkflow(t *testing.T) {
	events := []EventRecord{
		{Step: 0, EventType: EventTypeCall, Service: "svc", Op: "start", Request: `{}`, Response: `{"id":"1"}`},
		{Step: 1, EventType: EventTypeCall, Service: "svc", Op: "process", Request: `{}`, Response: `{"done":true}`},
		{Step: 2, EventType: EventTypeCall, Service: "svc", Op: "finish", Request: `{}`, Response: `{"ok":true}`},
		{Step: 3, EventType: EventTypeSleep, DurationMs: 5000},
		{Step: 4, EventType: EventTypeSignalReceived, SignalName: "payment", SignalPayload: `{"paid":true}`},
		{Step: 5, EventType: EventTypeDefer, DeferID: "defer-0", DeferDescription: "cleanup"},
	}

	// Full compaction: all events are compacted, tail is nil.
	cs := extractCompactionState(events)
	if cs.CompactedStep != len(events) {
		t.Errorf("expected CompactedStep=%d, got %d", len(events), cs.CompactedStep)
	}

	reconstructed := buildFullHistoryFromCompaction(nil, cs)
	if len(reconstructed) != len(events) {
		t.Fatalf("expected %d events, got %d", len(events), len(reconstructed))
	}
	for i := range events {
		if !eventFieldsMatch(events[i], reconstructed[i]) {
			t.Errorf("event %d (%s) mismatch", i, events[i].EventType)
			dumpEventDiff(t, events[i], reconstructed[i])
		}
	}
}

// TestCompactionOfRunningWorkflow verifies that only old events are compacted
// while recent events (the tail) remain as full EventRecords. This simulates
// a running workflow where the DB still has recent events as rows while old
// ones have been collapsed into CompactionState.
func TestCompactionOfRunningWorkflow(t *testing.T) {
	// Simulate a running workflow with steps: call*5, sleep, await_signals
	oldEvents := []EventRecord{
		{Step: 0, EventType: EventTypeCall, Service: "svc", Op: "start", Request: `{}`, Response: `{"id":"1"}`},
		{Step: 1, EventType: EventTypeCall, Service: "svc", Op: "prepare", Request: `{}`, Response: `{"done":true}`},
		{Step: 2, EventType: EventTypeCall, Service: "svc", Op: "process", Request: `{}`, Response: `{"ok":true}`},
		{Step: 3, EventType: EventTypeSleep, DurationMs: 30000},
	}
	recentEvents := []EventRecord{
		{Step: 4, EventType: EventTypeAwaitSignals, SignalNames: "payment,approval", TimeoutMs: 60000},
		{Step: 5, EventType: EventTypeSignalReceived, SignalName: "payment", SignalPayload: `{"paid":true}`},
	}

	cs := extractCompactionState(oldEvents)
	reconstructed := buildFullHistoryFromCompaction(recentEvents, cs)

	expectedLen := len(oldEvents) + len(recentEvents)
	if len(reconstructed) != expectedLen {
		t.Fatalf("expected %d events, got %d", expectedLen, len(reconstructed))
	}

	// Verify old events match.
	for i := range oldEvents {
		if !eventFieldsMatch(oldEvents[i], reconstructed[i]) {
			t.Errorf("old event %d mismatch", i)
			dumpEventDiff(t, oldEvents[i], reconstructed[i])
		}
	}

	// Verify recent events are preserved (passed through directly).
	for i := range recentEvents {
		idx := len(oldEvents) + i
		if reconstructed[idx].SignalName != recentEvents[i].SignalName {
			t.Errorf("recent event %d signal name: expected %q, got %q",
				i, recentEvents[i].SignalName, reconstructed[idx].SignalName)
		}
	}
}

// TestCompactionRoundTripThenReplay verifies the full round-trip: extract
// compaction state from events, then reconstruct the full history, then
// verify that the reconstruction matches the original exactly for all
// supported event types.
func TestCompactionRoundTripThenReplay(t *testing.T) {
	// Include all supported event types to exercise every branch of
	// extractCompactionState and buildFullHistoryFromCompaction.
	events := []EventRecord{
		{Step: 0, EventType: EventTypeCall, Service: "svc", Op: "do", Request: `{}`, Response: `{"ok":true}`},
		{Step: 1, EventType: EventTypeCall, Service: "svc", Op: "fail", Request: `{}`, Response: ``, Err: "timeout"},
		{Step: 2, EventType: EventTypeSleep, DurationMs: 5000},
		{Step: 3, EventType: EventTypeAwaitSignals, SignalNames: "sig1,sig2", TimeoutMs: 10000},
		{Step: 4, EventType: EventTypeSignalReceived, SignalName: "sig1", SignalPayload: `{"data":"hello"}`},
		{Step: 5, EventType: EventTypeDefer, DeferID: "defer-0", DeferDescription: "cleanup DB"},
		{Step: 6, EventType: EventTypeChildWorkflow, ChildName: "child-wf", ChildInput: `{"x":1}`, RunID: "run-001"},
		{Step: 7, EventType: EventTypeAwaitChild, RunID: "run-001", Response: `{"result":"done"}`},
		{Step: 8, EventType: EventTypeAwaitChild, RunID: "run-002", Err: "child failed"},
		{Step: 9, EventType: EventTypeContinueAsNew, NewInput: `{"restart":true}`},
		{Step: 10, EventType: EventTypeHeartbeat, Service: "svc", Op: "long-op"},
		{Step: 11, EventType: EventTypeAwaitAllChildren, Response: `[{"run_id":"c1","result":"ok"}]`},
		{Step: 12, EventType: EventTypePluginCall, PluginName: "p", PluginFunc: "f",
			PluginInput: `{"x":1}`, PluginOutput: `{"result":"ok"}`, PluginError: ""},
		{Step: 13, EventType: EventTypePluginCall, PluginName: "p", PluginFunc: "g",
			PluginInput: `{}`, PluginOutput: ``, PluginError: "not found"},
		{Step: 14, EventType: EventTypeCreatePromise, PromiseName: "prom-1", PromiseID: "pid-001"},
		{Step: 15, EventType: EventTypeAwaitPromise, PromiseID: "pid-001"},
		{Step: 16, EventType: EventTypePromiseResolved, PromiseID: "pid-001", PromiseResult: `{"status":"ok"}`},
		{Step: 17, EventType: EventTypePromiseRejected, PromiseID: "pid-002", PromiseError: "card declined"},
		{Step: 18, EventType: EventTypeUpdateHandler, UpdateHandlerName: "update-shipping"},
		{Step: 19, EventType: EventTypeStateMutation, StateKey: "count", StateValue: "3", StateDelta: 1, StateOp: "increment"},
		{Step: 20, EventType: EventTypeRunDetached},
		{Step: 21, EventType: EventTypeAcquireLock, LockKey: "my-lock", LockTTLMs: 30000, LockAcquired: 1},
		{Step: 22, EventType: EventTypeReleaseLock, LockKey: "my-lock"},
		{Step: 23, EventType: EventTypeScopeAcquired, ScopeKey: "vo:order:123:"},
		{Step: 24, EventType: EventTypePluginCallStreamChunk,
			PluginName: "p", PluginFunc: "f", PluginOutput: `{"chunk":1}`, StreamChunkIndex: 0, StreamFinish: true},
	}

	// Compact all events (simulating a fully compacted workflow) and reconstruct.
	cs := extractCompactionState(events)
	if len(cs.Events) != len(events) {
		t.Fatalf("expected %d compacted events, got %d", len(events), len(cs.Events))
	}

	reconstructed := buildFullHistoryFromCompaction(nil, cs)
	if len(reconstructed) != len(events) {
		t.Fatalf("expected %d reconstructed events, got %d", len(events), len(reconstructed))
	}

	for i := range events {
		if !eventFieldsMatch(events[i], reconstructed[i]) {
			t.Errorf("event %d (%s) round-trip mismatch", i, events[i].EventType)
			dumpEventDiff(t, events[i], reconstructed[i])
		}
	}
}
