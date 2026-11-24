package engine

// cleat#4323 and cleat#4231. cleat#4310 made compaction carry the drain stamps, and the replay refusal of cleat#4285 still exempted EVERY record expanded from a compaction,
// because nothing in a stored state said which build wrote it. CompactionState.StampedFrom says it, per EVENT RANGE and not per state: a state is extended by every compaction,
// so one run can hold events an old build compacted (stamps unknown) and events a new build compacted (stamps known).
//
// A single version number is unsound for that mixed state, and a row below is the case that shows it: extending an old state must leave its old events exempt and judge the new
// ones. The marker is an additive field, so a 0.5.x decoder (which never reads Version and ignores unknown fields) can still read a state this build wrote.

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// callEvents makes n distinct compacted call events, as an OLD build would have stored them: nothing stamped.
func callEvents(n int) []CompactedEvent {
	out := make([]CompactedEvent, n)
	for i := range out {
		out[i] = CompactedEvent{Type: EventCodeCall, Service: "svc", Op: "op"}
	}
	return out
}

func records(n int) []EventRecord {
	out := make([]EventRecord, n)
	for i := range out {
		out[i] = EventRecord{Step: i, EventType: EventTypeCall, Service: "svc", Op: "op"}
	}
	return out
}

// showMarker prints a StampedFrom for a failure message: the value, or nil, and never a pointer address.
func showMarker(p *int) string {
	if p == nil {
		return "nil"
	}
	b, _ := json.Marshal(*p)
	return string(b)
}

func TestStampedFromSaysWhichCompactedEventsCarryTheStamps(t *testing.T) {
	ptr := func(i int) *int { return &i }

	// A state written before this field existed: the JSON has no stamped_from, so every event in it is unstamped, whatever its length.
	const v1JSON = `{"version":1,"compacted_step":3,"events":[{"t":1,"svc":"a","op":"x"},{"t":1,"svc":"a","op":"x"},{"t":1,"svc":"a","op":"x"}]}`
	var v1 CompactionState
	if err := json.Unmarshal([]byte(v1JSON), &v1); err != nil {
		t.Fatalf("decoding a state written before the field existed: %v", err)
	}
	if v1.StampedFrom != nil {
		t.Errorf("a state written before the field existed decoded to StampedFrom=%d, want nil (none stamped)", *v1.StampedFrom)
	}
	if got := v1.unstampedRecords(); got != 3 {
		t.Errorf("a state written before the field existed has %d unstamped records, want 3 (every one of them)", got)
	}

	// A first compaction by this build: every event it holds is stamped.
	first, err := extractCompactionStateFrom(nil, records(4))
	if err != nil {
		t.Fatal(err)
	}
	if first.StampedFrom == nil || *first.StampedFrom != 0 || first.unstampedRecords() != 0 {
		t.Errorf("a first compaction: StampedFrom=%s unstamped=%d, want 0 and 0", showMarker(first.StampedFrom), first.unstampedRecords())
	}

	// THE CASE A VERSION NUMBER GETS WRONG: an OLD state extended by this build. Its 3 old events stay exempt and the 2 new ones are judged.
	extendedOld, err := extractCompactionStateFrom(&v1, records(2))
	if err != nil {
		t.Fatal(err)
	}
	if len(extendedOld.Events) != 5 || extendedOld.StampedFrom == nil || *extendedOld.StampedFrom != 3 || extendedOld.unstampedRecords() != 3 {
		t.Errorf("an old state extended: %d events, StampedFrom=%s, unstamped=%d, want 5 events, 3 and 3", len(extendedOld.Events), showMarker(extendedOld.StampedFrom), extendedOld.unstampedRecords())
	}

	// Extending a state that already carries stamps leaves the boundary where it was.
	extendedNew, err := extractCompactionStateFrom(extendedOld, records(2))
	if err != nil {
		t.Fatal(err)
	}
	if len(extendedNew.Events) != 7 || *extendedNew.StampedFrom != 3 || extendedNew.unstampedRecords() != 3 {
		t.Errorf("a stamped state extended: %d events, StampedFrom=%d, unstamped=%d, want 7 events, 3 and 3", len(extendedNew.Events), *extendedNew.StampedFrom, extendedNew.unstampedRecords())
	}

	// TRUNCATION drops the OLDEST events, so the boundary has to move up with them. A full state of old events extended by one more than the cap drops every old event
	// (and some new ones): what is left is all stamped, so the boundary is 0 and nothing is exempt.
	atCap := &CompactionState{Version: 1, CompactedStep: DefaultMaxCompactedEvents, Events: callEvents(DefaultMaxCompactedEvents)}
	truncated, err := extractCompactionStateFrom(atCap, records(DefaultMaxCompactedEvents+1))
	if err != nil {
		t.Fatal(err)
	}
	if len(truncated.Events) != DefaultMaxCompactedEvents || truncated.StampedFrom == nil || *truncated.StampedFrom != 0 || truncated.unstampedRecords() != 0 {
		t.Errorf("a truncated extension that drops every old event: %d events, StampedFrom=%s, unstamped=%d, want %d events, 0 and 0",
			len(truncated.Events), showMarker(truncated.StampedFrom), truncated.unstampedRecords(), DefaultMaxCompactedEvents)
	}
	// ... and when only SOME old events are dropped, the survivors are still unstamped: 9990 old events plus 20 new is 10 over the cap, so 10 are dropped from the front.
	partial := &CompactionState{Version: 1, CompactedStep: DefaultMaxCompactedEvents - 10, Events: callEvents(DefaultMaxCompactedEvents - 10)}
	partialOut, err := extractCompactionStateFrom(partial, records(20))
	if err != nil {
		t.Fatal(err)
	}
	if len(partialOut.Events) != DefaultMaxCompactedEvents || *partialOut.StampedFrom != DefaultMaxCompactedEvents-20 || partialOut.unstampedRecords() != DefaultMaxCompactedEvents-20 {
		t.Errorf("a partly truncated extension: %d events, StampedFrom=%s, want %d events and %d (the boundary moves down by the 10 dropped from the front)",
			len(partialOut.Events), showMarker(partialOut.StampedFrom), DefaultMaxCompactedEvents, DefaultMaxCompactedEvents-20)
	}

	// The accessor is total.
	var nilState *CompactionState
	if nilState.unstampedRecords() != 0 {
		t.Error("a nil state has unstamped records")
	}
	if got := (&CompactionState{Events: callEvents(2), StampedFrom: ptr(9)}).unstampedRecords(); got != 2 {
		t.Errorf("a boundary past the end gave %d, want 2 (clamped to the events held)", got)
	}
	if got := (&CompactionState{Events: callEvents(2), StampedFrom: ptr(-4)}).unstampedRecords(); got != 0 {
		t.Errorf("a negative boundary gave %d, want 0", got)
	}

	// The marker survives the store's JSON, and an unmarked state does not grow one.
	raw, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"stamped_from":0`) {
		t.Errorf("a first compaction's JSON lacks stamped_from:0: %s", raw)
	}
	rawOld, _ := json.Marshal(&v1)
	if strings.Contains(string(rawOld), "stamped_from") {
		t.Errorf("a state with no marker grew one when re-encoded: %s", rawOld)
	}
}

// ROLLBACK. An operator who upgrades and rolls back to 0.5.x must still replay a state this build wrote. v0.5.0's CompactionState has a Version it only writes and never reads
// and is decoded with a plain json.Unmarshal (read from the tag's files: `git show v0.5.0:engine/compaction.go`, `engine/db.go` LoadCompactionState; no
// DisallowUnknownFields anywhere in engine/ or cmd/ at that tag). What matters is what 0.5.x does when it next compacts: it re-marshals through ITS structs, which have none of the
// new fields, so it drops stamped_from AND every per-event stamp and send target together. The result must read as "none stamped", never as a marker claiming stamps that
// were removed.
//
// THIS IS A MIRROR, NOT THE TAG'S CODE. It carries the tag's top-level field names and only the per-event fields the rows touch, with the tag's JSON tags. The local checkout is
// shallow and CI has no tag checkout, so the tag's own decoder cannot be run here; the claim that it matches is by reading, and the test says so.
type v050CompactionState struct {
	Version       int               `json:"version"`
	CompactedStep int               `json:"compacted_step"`
	Events        []v050Event       `json:"events"`
	PendingDefers []json.RawMessage `json:"pending_defers,omitempty"`
}
type v050Event struct {
	Type    int    `json:"t"`
	Service string `json:"svc,omitempty"`
	Op      string `json:"op,omitempty"`
	Request string `json:"req,omitempty"`
}

func TestAStateThisBuildWroteSurvivesARollbackToTheOldDecoderAsNoneStamped(t *testing.T) {
	written, err := extractCompactionStateFrom(nil, []EventRecord{
		{Step: 0, EventType: EventTypeCall, Service: "svc", Op: "op", InDeferPhase: true, InDeferSegment: true},
		{Step: 1, EventType: EventTypeSignalReceived, Service: SignalEventService, Op: "send", SignalName: "go", RunID: "target-run", InDeferPhase: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(written)
	if err != nil {
		t.Fatal(err)
	}
	// PRECONDITION: the state really carries the new fields, so a pass below is not "there was nothing to drop".
	for _, must := range []string{`"stamped_from":0`, `"idp":true`, `"ids":true`, `"rid":"target-run"`} {
		if !strings.Contains(string(raw), must) {
			t.Fatalf("PRECONDITION FAILED: the state this build wrote lacks %s, so the rollback row would measure nothing:\n%s", must, raw)
		}
	}
	var old v050CompactionState
	if err := json.Unmarshal(raw, &old); err != nil {
		t.Fatalf("the v0.5.0-shaped decoder REFUSED a state this build wrote: %v", err)
	}
	if len(old.Events) != 2 || old.Version != 1 {
		t.Fatalf("the v0.5.0-shaped decoder read %d events at version %d, want 2 at 1", len(old.Events), old.Version)
	}
	rewritten, err := json.Marshal(&old) // what 0.5.x writes when it next compacts
	if err != nil {
		t.Fatal(err)
	}
	var back CompactionState
	if err := json.Unmarshal(rewritten, &back); err != nil {
		t.Fatalf("this build could not read a state 0.5.x rewrote: %v", err)
	}
	if back.StampedFrom != nil {
		t.Errorf("a state 0.5.x rewrote reads StampedFrom=%d: a marker claiming stamps its rewrite removed", *back.StampedFrom)
	}
	if got := back.unstampedRecords(); got != 2 {
		t.Errorf("a state 0.5.x rewrote has %d unstamped records, want 2 (every one: none stamped)", got)
	}
	for i, ev := range buildFullHistoryFromCompaction(nil, &back) {
		if ev.InDeferPhase || ev.InDeferSegment || ev.RunID != "" {
			t.Errorf("event %d kept a stamp or target through the 0.5.x rewrite (%+v): the fields must go together with the marker", i, ev)
		}
	}
}

// cleat#4231: a signal send's target survives compaction.
func TestACompactedSignalSendKeepsItsTarget(t *testing.T) {
	in := EventRecord{Step: 0, EventType: EventTypeSignalReceived, Service: SignalEventService, Op: "send", SignalName: "go", SignalPayload: `{}`, RunID: "target-run", Response: "ok"}
	cs, err := extractCompactionStateFrom(nil, []EventRecord{in})
	if err != nil {
		t.Fatal(err)
	}
	out := buildFullHistoryFromCompaction(nil, cs)
	if len(out) != 1 || out[0].RunID != "target-run" {
		t.Fatalf("a compacted signal send came back as %+v: the target must survive", out)
	}
	if d := diffEventFields(in, out[0]); len(d) != 0 {
		t.Errorf("fields that did not survive: %v", d)
	}
	// A DELIVERY (no Op, no target) still round trips with an empty target and an unchanged encoding.
	delivery := EventRecord{Step: 0, EventType: EventTypeSignalReceived, Service: SignalEventService, Op: "deliver", SignalName: "go"}
	cs2, _ := extractCompactionStateFrom(nil, []EventRecord{delivery})
	if raw, _ := json.Marshal(cs2.Events[0]); strings.Contains(string(raw), `"rid"`) {
		t.Errorf("a delivery grew a target key: %s", raw)
	}
}

// The consequence, through the call that checks it: a COMPACTED send to ANOTHER target is a divergence now, where it used to replay as the recorded one because only the name
// was compared. The control row is the same send to the SAME target, which must still replay.
func TestACompactedSignalSendToAnotherTargetIsADivergence(t *testing.T) {
	signalCall := g1bCalls[0]
	if signalCall.name != "signal_workflow" {
		t.Fatalf("PRECONDITION FAILED: g1bCalls[0] is %q, not signal_workflow", signalCall.name)
	}
	for _, c := range []struct {
		name     string
		runID    string
		diverges bool
	}{
		{"CONTROL: compacted, the same target", "target-run", false},
		{"compacted, another target, the same name", "another-run", true},
	} {
		c := c
		t.Run(c.name, func(t *testing.T) {
			rec := g1bOwn(signalCall)
			rec[0].RunID = c.runID
			cs, err := extractCompactionStateFrom(nil, rec)
			if err != nil {
				t.Fatal(err)
			}
			expanded := buildFullHistoryFromCompaction(nil, cs)
			w := newG1bWorld()
			s := g1bSession(w, expanded, true)
			got := g1bObserve(signalCall, w, s)
			if diverged := s.stepCount == 0; diverged != c.diverges {
				t.Errorf("diverged=%v, want %v:\n  %s", diverged, c.diverges, got)
			}
		})
	}
}

// THE REPLAY REFUSAL, through the real executor (cleat#4285 step 3): [a pending child await, a call recorded after it] is a divergence unless the call's stamp is UNKNOWN. A state
// that carries stamps judges it; a state compacted before them does not.
func TestTheRefusalOfACallAfterAPendingAwaitFollowsTheStampsThatAreKnown(t *testing.T) {
	ctx := context.Background()
	wasmBytes, err := os.ReadFile(buildFixtureWasm(t, "ignoredsuspend"))
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	t.Cleanup(func() { rt.Close(ctx) })
	wt, err := NewWasmtimeBackend(ctx)
	if err != nil {
		t.Fatalf("NewWasmtimeBackend: %v", err)
	}
	t.Cleanup(func() { wt.Close(ctx) })
	newEngine := func(opts ...EngineOption) *Engine {
		eng := NewEngine(rt, &mockCaller{}, append([]EngineOption{WithBackends(WasmtimeLanguages, wt), WithWorkflowID("wf-stamped-state")}, opts...)...)
		eng.childWfStore = &mockChildStore{getChildResultFn: func(context.Context, string) (ChildOutcome, error) {
			return ChildOutcome{Completed: true, Result: `{"r":1}`}, nil
		}}
		return eng
	}
	_, hist, _, _, _, err := newEngine().Execute(ctx, wasmBytes, "ignore_child_suspend", json.RawMessage(`{}`))
	if err != nil || len(hist) != 2 || hist[1].EventType != EventTypeCall {
		t.Fatalf("setup: could not derive a call record: err=%v history=%v", err, hist)
	}
	pending := EventRecord{Step: 0, EventType: EventTypeAwaitChild, RunID: "child-1"}
	bad := hist[1]
	bad.Step = 1
	drain := bad
	drain.InDeferPhase = true // a healthy drain records its later calls after the pending await, stamped
	tail := []EventRecord{{Step: 2, EventType: EventTypeSignalReceived, Service: SignalEventService, Op: "deliver"}}

	// stateOf builds the state the way a given build would have written it: events as the compaction captured them, and the boundary it recorded.
	stateOf := func(stampedFrom *int, evs ...EventRecord) *CompactionState {
		cs, err := extractCompactionStateFrom(nil, evs)
		if err != nil {
			t.Fatalf("setup: compaction: %v", err)
		}
		cs.StampedFrom = stampedFrom
		return cs
	}
	ptr := func(i int) *int { return &i }

	for _, c := range []struct {
		name     string
		cs       *CompactionState
		diverges bool
	}{
		// the rule's two halves for an OLD state and a NEW one, the same bad shape in each
		{"a state compacted BEFORE the stamps (no marker): the call's stamp is unknown, so it is not refused", stateOf(nil, pending, bad), false},
		{"a state that carries the stamps, the same bad shape: refused like an uncompacted history", stateOf(ptr(0), pending, bad), true},
		{"a state that carries the stamps, a healthy drain (stamped): not refused", stateOf(ptr(0), pending, drain), false},
		// THE MIXED STATE a single version number gets wrong: the boundary is exact on both sides of it
		{"an old state EXTENDED: the call is in the NEW range (boundary 1), so it is judged and refused", stateOf(ptr(1), pending, bad), true},
		{"an old state extended: the call is in the OLD range (boundary 2), so it is still exempt", stateOf(ptr(2), pending, bad), false},
	} {
		c := c
		t.Run(c.name, func(t *testing.T) {
			_, _, _, _, _, err := newEngine(WithCompactionState(c.cs)).Replay(ctx, wasmBytes, "ignore_child_suspend", json.RawMessage(`{}`), tail)
			// The text is the generic one: this guest swallows the await's own divergence and the error that reaches the caller is the next call's (see the row this file extends).
			diverged := err != nil && strings.Contains(err.Error(), "replay divergence")
			if diverged != c.diverges {
				t.Errorf("diverged=%v, want %v (err=%v)", diverged, c.diverges, err)
			}
		})
	}
}
