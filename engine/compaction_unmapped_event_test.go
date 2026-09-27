package engine

import "testing"

// TestCompactionPreservesAwaitAnyChild asserts that an await-any-child event
// survives a compaction round trip with its payload.
//
// It did not. eventTypeToCode had no entry for EventTypeAwaitAnyChild, the
// lookup was a bare map index, and a Go map miss returns 0 -- which was
// EventCodeCall. So the event was stored as a durable call with empty
// Service/Op/Request/Response and decoded back as one. Compaction REWRITES
// stored history, so the child's result was gone permanently, with no error
// anywhere. IMPROVEMENT-PLAN 3.217.
//
// Measured before the fix, by exactly this round trip:
//
//	in : type=await_any_child  response="{\"runID\":\"run-b\",...}"
//	out: type=call             response=""
func TestCompactionPreservesAwaitAnyChild(t *testing.T) {
	const result = `{"runID":"run-b","result":"THE CHILD RESULT"}`
	in := []EventRecord{{
		Step: 0, EventType: EventTypeAwaitAnyChild,
		Request: `["run-a","run-b"]`, Response: result,
	}}

	cs, err := extractCompactionState(in)
	if err != nil {
		t.Fatalf("extractCompactionState: %v", err)
	}
	out := buildFullHistoryFromCompaction(nil, cs)
	if len(out) != 1 {
		t.Fatalf("got %d events back, want 1", len(out))
	}
	if out[0].EventType != EventTypeAwaitAnyChild {
		t.Errorf("event type round-tripped as %q, want %q.\n\n"+
			"A wrong type here is not a cosmetic mislabel: the decoder copies "+
			"fields per type, so the payload goes with it.",
			out[0].EventType, EventTypeAwaitAnyChild)
	}
	if out[0].Response != result {
		t.Errorf("response round-tripped as %q, want %q", out[0].Response, result)
	}
}

// TestCompactionRefusesAnUnmappedEventType asserts the general guard: an event
// type with no code must abort compaction rather than be stored as something
// else.
//
// This is the half that outlives the three specific gaps. Zero is now
// EventCodeInvalid and never a valid code, so a map miss can no longer collide
// with a real event -- but a miss must still be loud, because compaction
// rewrites history and "not compacted" is always recoverable while "compacted
// wrongly" is not.
func TestCompactionRefusesAnUnmappedEventType(t *testing.T) {
	in := []EventRecord{{Step: 0, EventType: EventType("no_such_event_type")}}

	cs, err := extractCompactionState(in)
	if err == nil {
		t.Fatalf("compaction accepted an unmapped event type and returned %+v.\n\n"+
			"It must refuse: storing an event it cannot represent is silent, "+
			"permanent data loss, and not compacting costs only space.", cs)
	}
	if cs != nil {
		t.Errorf("returned a non-nil state alongside an error: %+v", cs)
	}
}

// TestEveryDeclaredEventTypeHasACode is the drift guard. The three gaps existed
// because nothing tied the declared types to the code map, so adding a type
// and forgetting the entry was silent.
func TestEveryDeclaredEventTypeHasACode(t *testing.T) {
	// Every type this package emits, taken from the map's own inverse so the
	// test cannot pass by agreeing with the thing it checks.
	for code, typ := range codeToEventType {
		got, ok := eventTypeToCode[typ]
		if !ok {
			t.Errorf("codeToEventType has %d -> %q but eventTypeToCode has no "+
				"entry for %q: the two maps disagree, and the direction that "+
				"matters is the one compaction uses to WRITE.", code, typ, typ)
			continue
		}
		if got != code {
			t.Errorf("round trip disagrees for %q: code %d -> type -> code %d", typ, code, got)
		}
	}
	if _, ok := codeToEventType[EventCodeInvalid]; ok {
		t.Errorf("EventCodeInvalid (%d) is present in codeToEventType. It must "+
			"never decode to anything: it exists so that a map miss, a zero "+
			"struct field or an unset column cannot look like a real event.",
			EventCodeInvalid)
	}
}
