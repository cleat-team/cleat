package engine

// Properties of the replay step dispatcher (cleat#4152, docs/design/replay-step-dispatcher.md, PR 1). Each is one thing the bugs of this
// class violated and that step() now answers in one place. The fake specs here are the dispatcher's own: they test the MECHANISM, and the
// promise calls' behaviour is pinned separately by the characterization table (a_promise_single_step_behaves_as_recorded_before_the_dispatcher_test.go).
//
// NOT covered, and said so: steps that read more than one record (the `OverPending` shape of await_child, the `AtNext` shape of await_promise
// and the set_scope retry). step() reads exactly one record per step, so those shapes cannot be written as a spec yet and a fake of them here
// would be a claim; they arrive with takeRecords in the group that first needs them, and the round trip below is table-driven so they plug in.

import (
	"context"
	"os"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const fakeEvent EventType = "fake_step_event"

// fakeSpec builds a one-event spec whose Fresh counts how often it ran.
func fakeSpec(effect stepEffect, shape recordShape, ran *int32, record func(c *stepCtx)) *stepSpec {
	return &stepSpec{
		Name: "fake", Effect: effect, Records: shape, Want: fakeEvent,
		Decode: func(*stepCtx, *EventRecord) int64 { return 7 },
		Fresh: func(c *stepCtx) int64 {
			atomic.AddInt32(ran, 1)
			record(c)
			return 9
		},
	}
}

func recordOneFake(c *stepCtx) {
	c.Record(EventRecord{Step: c.s.stepCount, EventType: fakeEvent})
}

func stepSession(opts []EngineOption, history []EventRecord) *execSession {
	return &execSession{
		engine: NewEngine(nil, nil, opts...), workflowID: "wf-step", nowMs: time.Now().UnixMilli(),
		isReplay: len(history) > 0, history: history,
		deferrals: map[string]string{}, queryState: map[string]string{},
	}
}

// A non-Pure step is refused when the session is stopping, on BOTH arrival paths, and a Pure one is not.
func TestAStepIsRefusedWhenTheSessionIsStoppingWhicheverWayItArrived(t *testing.T) {
	ctx := context.Background()
	closedShutdown := make(chan struct{})
	close(closedShutdown)
	reasons := []struct {
		name string
		opts []EngineOption
		mod  func(*execSession)
	}{
		{"a defer segment's body", []EngineOption{WithDeferPhase()}, nil},
		{"a recorded event that could not be written", nil, func(s *execSession) { s.eventWriteFailure = context.DeadlineExceeded }},
		// The third condition of stopBeforeNewWork, which a reader of the design's first draft would not have known to test (WS-cleat-2).
		{"graceful shutdown", []EngineOption{WithShutdownSignal(closedShutdown)}, nil},
	}
	arrivals := []struct {
		name    string
		history func() []EventRecord
	}{
		{"fresh entry", func() []EventRecord { return nil }},
		// A segment over a run with history starts in replay, so the body's first new call arrives at the replay frontier (cleat#4143).
		{"replay frontier", func() []EventRecord { return []EventRecord{{Step: 0, EventType: fakeEvent}} }},
	}
	for _, r := range reasons {
		for _, a := range arrivals {
			t.Run(r.name+"/"+a.name, func(t *testing.T) {
				var ran int32
				s := stepSession(r.opts, a.history())
				if r.mod != nil {
					r.mod(s)
				}
				spec := fakeSpec(effectEffect, recordsOne, &ran, recordOneFake)
				if len(s.history) > 0 {
					if got := s.step(ctx, nil, spec, stepArgs{}); got != 7 {
						t.Fatalf("PRECONDITION FAILED: the step over its own record returned %d, want the decoded 7", got)
					}
				}
				before := len(s.history)
				if got := s.step(ctx, nil, spec, stepArgs{}); got != callSuspendSentinel {
					t.Errorf("a non-Pure step was not refused: returned %#x, want the stop sentinel %#x", got, callSuspendSentinel)
				}
				if ran != 0 || len(s.history) != before {
					t.Errorf("a refused step still ran its work (%d) or recorded (%d new events)", ran, len(s.history)-before)
				}
				// The control: the same step on a session that is NOT stopping runs, so the refusal above is the refusal and not a broken spec.
				var ranControl int32
				ok := stepSession(nil, a.history())
				if len(ok.history) > 0 {
					ok.step(ctx, nil, fakeSpec(effectEffect, recordsOne, &ranControl, recordOneFake), stepArgs{})
				}
				ok.step(ctx, nil, fakeSpec(effectEffect, recordsOne, &ranControl, recordOneFake), stepArgs{})
				if ranControl != 1 {
					t.Errorf("CONTROL: the step on a session that is not stopping ran %d times, want 1", ranControl)
				}
			})
		}
	}
}

func TestAPureStepIsNotRefusedInASegment(t *testing.T) {
	var ran int32
	s := stepSession([]EngineOption{WithDeferPhase()}, nil)
	got := s.step(context.Background(), nil, fakeSpec(effectPure, recordsNone, &ran, func(*stepCtx) {}), stepArgs{})
	if got == callSuspendSentinel || ran != 1 {
		t.Errorf("a Pure step in a segment returned %#x after running %d times, want it to run once and answer", got, ran)
	}
}

// A divergence has one shape and consumes nothing: the record stays for the call it belongs to (cleat#4157).
func TestAStepOverAnotherCallsRecordDivergesAndConsumesNothing(t *testing.T) {
	var ran int32
	s := stepSession(nil, []EventRecord{{Step: 0, EventType: EventTypeCall, Service: "svc", Op: "op"}})
	got := s.step(context.Background(), nil, fakeSpec(effectEffect, recordsOne, &ran, recordOneFake), stepArgs{})
	if got&0xFF != 1 {
		t.Errorf("a step over another call's record returned %#x, want errCode 1 (a divergence)", got)
	}
	if s.stepCount != 0 || !s.isReplay || ran != 0 {
		t.Errorf("a divergence moved the session: step=%d isReplay=%v ran=%d, want 0 / true / 0", s.stepCount, s.isReplay, ran)
	}
}

// Replay never ends with history unread: leaving replay with a step count that is not the history's length is reported.
func TestLeavingReplayWithHistoryUnreadIsReported(t *testing.T) {
	var ran int32
	// stepCount ahead of the history is the shape of a step that consumed a record it did not own.
	s := stepSession(nil, []EventRecord{{Step: 0, EventType: fakeEvent}})
	s.stepCount = 2
	s.step(context.Background(), nil, fakeSpec(effectEffect, recordsOne, &ran, recordOneFake), stepArgs{})
	if len(s.stepViolations) == 0 || !strings.Contains(s.stepViolations[0], "left replay at step 2 with 1 records") {
		t.Errorf("a step that left replay at step 2 over a 1-record history reported %v, want a violation naming both", s.stepViolations)
	}
	// The control: leaving replay exactly at the end reports nothing.
	ok := stepSession(nil, []EventRecord{{Step: 0, EventType: fakeEvent}})
	ok.step(context.Background(), nil, fakeSpec(effectEffect, recordsOne, &ran, recordOneFake), stepArgs{})
	ok.step(context.Background(), nil, fakeSpec(effectEffect, recordsOne, &ran, recordOneFake), stepArgs{})
	if len(ok.stepViolations) != 0 {
		t.Errorf("CONTROL: a replay that ended exactly at the end of its history reported %v", ok.stepViolations)
	}
}

// A spec records what it declared, and only through the handle.
func TestASpecThatRecordsOtherThanItDeclaredIsReported(t *testing.T) {
	cases := []struct {
		name   string
		shape  recordShape
		record func(c *stepCtx)
		want   string // "" means no violation
	}{
		{"one declared, one recorded", recordsOne, recordOneFake, ""},
		{"one declared, none recorded", recordsOne, func(*stepCtx) {}, "declares shape 0 but recorded 0"},
		{"one declared, two recorded", recordsOne, func(c *stepCtx) { recordOneFake(c); recordOneFake(c) }, "declares shape 0 but recorded 2"},
		{"none declared, one recorded", recordsNone, recordOneFake, "declares shape 1 but recorded 1"},
		{"many declared, two recorded", recordsMany, func(c *stepCtx) { recordOneFake(c); recordOneFake(c) }, ""},
		// Recording behind the handle's back is invisible to the declared count and caught by the step count.
		{"recorded behind the handle", recordsNone, func(c *stepCtx) { c.s.recordEvent(EventRecord{Step: c.s.stepCount, EventType: fakeEvent}) }, "step count moved by 1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var ran int32
			s := stepSession(nil, nil)
			s.step(context.Background(), nil, fakeSpec(effectEffect, c.shape, &ran, c.record), stepArgs{})
			joined := strings.Join(s.stepViolations, "; ")
			if c.want == "" && joined != "" {
				t.Errorf("reported %q for a spec that did what it declared", joined)
			}
			if c.want != "" && !strings.Contains(joined, c.want) {
				t.Errorf("reported %q, want it to contain %q", joined, c.want)
			}
		})
	}
}

// The registry of real specs: every one is listed, none claims to be Pure and record, and the list is the set of declarations in the tree.
func TestEverySpecIsRegisteredAndNoPureSpecRecords(t *testing.T) {
	if len(stepSpecs) < 3 {
		t.Fatalf("PRECONDITION FAILED: the registry holds %d specs; the three promise calls are on it", len(stepSpecs))
	}
	for _, sp := range stepSpecs {
		if err := sp.validate(); err != nil {
			t.Error(err)
		}
	}
	// The control: validate refuses what it says it refuses, so the loop above is not vacuous.
	for name, bad := range map[string]stepSpec{
		"no name":              {Want: fakeEvent, Decode: func(*stepCtx, *EventRecord) int64 { return 0 }, Fresh: func(*stepCtx) int64 { return 0 }},
		"no Want":              {Name: "x", Decode: func(*stepCtx, *EventRecord) int64 { return 0 }, Fresh: func(*stepCtx) int64 { return 0 }},
		"no Decode":            {Name: "x", Want: fakeEvent, Fresh: func(*stepCtx) int64 { return 0 }},
		"Pure but records one": {Name: "x", Want: fakeEvent, Effect: effectPure, Records: recordsOne, Decode: func(*stepCtx, *EventRecord) int64 { return 0 }, Fresh: func(*stepCtx) int64 { return 0 }},
	} {
		bad := bad
		if bad.validate() == nil {
			t.Errorf("CONTROL: validate accepted a spec with %s", name)
		}
	}
	// The registry is checked against the SOURCE, in the other direction too, or a new spec nobody registered would be on no list.
	decl := regexp.MustCompile(`(?m)^var \w+ = stepSpec\{`)
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading engine/: %v", err)
	}
	declared := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		b, err := os.ReadFile(e.Name())
		if err != nil {
			t.Fatal(err)
		}
		declared += len(decl.FindAll(b, -1))
	}
	if declared != len(stepSpecs) {
		t.Errorf("%d `var x = stepSpec{` declarations in engine/ but %d specs in stepSpecs: register every spec, or the properties here do not cover it", declared, len(stepSpecs))
	}
}
