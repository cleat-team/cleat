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
	"go/ast"
	"go/parser"
	"go/token"
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
		Name: "fake", Effect: effect, Records: shape, Want: fakeEvent, NoIdentity: "a fake with nothing to compare",
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
		"no name":                         {NoIdentity: "x", Want: fakeEvent, Decode: func(*stepCtx, *EventRecord) int64 { return 0 }, Fresh: func(*stepCtx) int64 { return 0 }},
		"no Want":                         {NoIdentity: "x", Name: "x", Decode: func(*stepCtx, *EventRecord) int64 { return 0 }, Fresh: func(*stepCtx) int64 { return 0 }},
		"no Decode":                       {NoIdentity: "x", Name: "x", Want: fakeEvent, Fresh: func(*stepCtx) int64 { return 0 }},
		"both Identity and NoIdentity":    {Name: "x", Want: fakeEvent, Identity: func(stepArgs, *EventRecord) string { return "" }, NoIdentity: "x", Decode: func(*stepCtx, *EventRecord) int64 { return 0 }, Fresh: func(*stepCtx) int64 { return 0 }},
		"neither Identity nor NoIdentity": {Name: "x", Want: fakeEvent, Decode: func(*stepCtx, *EventRecord) int64 { return 0 }, Fresh: func(*stepCtx) int64 { return 0 }},
		"Refuse on a Pure step":           {NoIdentity: "x", Name: "x", Want: fakeEvent, Effect: effectPure, Refuse: func(*execSession) bool { return true }, Decode: func(*stepCtx, *EventRecord) int64 { return 0 }, Fresh: func(*stepCtx) int64 { return 0 }},
		"stays in replay but records one": {NoIdentity: "x", Name: "x", Want: fakeEvent, Effect: effectEffect, Records: recordsOne, StaysInReplayAtFrontier: true, Decode: func(*stepCtx, *EventRecord) int64 { return 0 }, Fresh: func(*stepCtx) int64 { return 0 }},
		"stays in replay but is Pure":     {NoIdentity: "x", Name: "x", Want: fakeEvent, Effect: effectPure, StaysInReplayAtFrontier: true, Decode: func(*stepCtx, *EventRecord) int64 { return 0 }, Fresh: func(*stepCtx) int64 { return 0 }},
		"Pure but records one":            {NoIdentity: "x", Name: "x", Want: fakeEvent, Effect: effectPure, Records: recordsOne, Decode: func(*stepCtx, *EventRecord) int64 { return 0 }, Fresh: func(*stepCtx) int64 { return 0 }},
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

// The two exemptions the stop-site guards carry for the dispatcher say something about CALLERS ("leaveReplay's only caller is step",
// "Record is reached only from a spec's Fresh"), and the guards' stale-entry checks look at the exempt function's BODY, not at who calls it.
// A second caller of leaveReplay that skipped step would be legitimised by the exemption, which is the bypass the dispatcher exists to prevent
// (WS-cleat-2, review of #4207). This reads the callers from the syntax tree, so a comment or a string cannot satisfy it.

// dispatcherCallerFacts parses non-test sources (name -> source) and returns where leaveReplay is called from, and how many .Record( calls
// there are in all and how many sit inside the Fresh function of a stepSpec literal.
func dispatcherCallerFacts(t *testing.T, sources map[string]string) (leaveReplayCallers []string, recordCalls, recordInFresh int) {
	t.Helper()
	fset := token.NewFileSet()
	for name, src := range sources {
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		// The source ranges of every `Fresh: func(...) {...}` inside a stepSpec composite literal.
		var freshRanges [][2]token.Pos
		ast.Inspect(f, func(n ast.Node) bool {
			cl, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			if id, ok := cl.Type.(*ast.Ident); !ok || id.Name != "stepSpec" {
				return true
			}
			for _, el := range cl.Elts {
				kv, ok := el.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "Fresh" {
					if fl, ok := kv.Value.(*ast.FuncLit); ok {
						freshRanges = append(freshRanges, [2]token.Pos{fl.Pos(), fl.End()})
					}
				}
			}
			return true
		})
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			ast.Inspect(fd, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if sel.Sel.Name == "leaveReplay" {
					leaveReplayCallers = append(leaveReplayCallers, fd.Name.Name)
				}
				return true
			})
		}
		// Record calls can sit in a package-level var (the specs are vars), so they are found over the whole file, not only inside functions.
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Record" {
				return true
			}
			recordCalls++
			for _, r := range freshRanges {
				if call.Pos() >= r[0] && call.End() <= r[1] {
					recordInFresh++
					break
				}
			}
			return true
		})
	}
	return leaveReplayCallers, recordCalls, recordInFresh
}

func TestTheDispatcherExemptionsAreTrueOfTheirCallers(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading engine/: %v", err)
	}
	sources := map[string]string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		b, err := os.ReadFile(e.Name())
		if err != nil {
			t.Fatal(err)
		}
		sources[e.Name()] = string(b)
	}
	callers, records, inFresh := dispatcherCallerFacts(t, sources)

	// Known positive: the scan must SEE the calls that exist today, or "no second caller" would be what a scan that matches nothing says.
	if len(callers) != 1 || callers[0] != "step" {
		t.Errorf("leaveReplay is called from %v, want exactly [step]: the exemption for leaveReplay says its only caller is step, and a second caller would skip the refusal that follows it", callers)
	}
	if records < 3 {
		t.Fatalf("PRECONDITION FAILED: the scan found %d .Record( calls, want the 3 the promise specs make; it is reading nothing", records)
	}
	if inFresh != records {
		t.Errorf("%d of %d .Record( calls are outside a stepSpec's Fresh function: the exemption for Record says it is reached only from there, where step has already refused", records-inFresh, records)
	}

	// Negative control: the same scan over a source with a second caller of each kind must report them.
	badStep := `package engine
func (s *execSession) step() { s.leaveReplay(nil) }
func (s *execSession) sneaky() { s.leaveReplay(nil) }
var spec = stepSpec{Fresh: func(c *stepCtx) int64 { c.Record(EventRecord{}); return 0 }}
func (s *execSession) bypass(c *stepCtx) { c.Record(EventRecord{}) }
`
	cc, rr, rf := dispatcherCallerFacts(t, map[string]string{"bad.go": badStep})
	if len(cc) != 2 || rr != 2 || rf != 1 {
		t.Errorf("CONTROL: on a source with a second leaveReplay caller and a Record outside Fresh the scan reported callers=%v records=%d inFresh=%d, want 2 callers, 2 records, 1 in Fresh", cc, rr, rf)
	}
}

// A record of the right TYPE that names something else is a divergence, not an answer (cleat#4211), and it is not consumed.
func TestAStepOverARecordOfTheRightTypeThatNamesSomethingElseDiverges(t *testing.T) {
	ctx := context.Background()
	named := func() *stepSpec {
		var ran int32
		sp := fakeSpec(effectEffect, recordsOne, &ran, recordOneFake)
		sp.NoIdentity = ""
		sp.Identity = func(a stepArgs, rec *EventRecord) string {
			if rec.PromiseID != a.s1 {
				return "recorded " + rec.PromiseID + " for " + a.s1
			}
			return ""
		}
		return sp
	}
	rec := []EventRecord{{Step: 0, EventType: fakeEvent, PromiseID: "p-1"}}

	same := stepSession(nil, append([]EventRecord(nil), rec...))
	if got := same.step(ctx, nil, named(), stepArgs{s1: "p-1"}); got != 7 || same.stepCount != 1 {
		t.Errorf("CONTROL: the call whose identity matches returned %d at step %d, want the decoded 7 at step 1", got, same.stepCount)
	}

	other := stepSession(nil, append([]EventRecord(nil), rec...))
	got := other.step(ctx, nil, named(), stepArgs{s1: "p-2"})
	if got&0xFF != 1 {
		t.Errorf("a call whose identity differs from the record returned %#x, want errCode 1 (a divergence)", got)
	}
	if other.stepCount != 0 || !other.isReplay {
		t.Errorf("a divergence on identity consumed the record or left replay: step=%d isReplay=%v, want 0 / true", other.stepCount, other.isReplay)
	}
}

// A spec's own Refuse decides when it is refused, on both arrival paths, in place of stopBeforeNewWork (DurableDefer refuses only in a defer
// segment over a killed run, and must not be newly refused in an ordinary segment's body, cleat#4079). Each case has a control that runs.
func TestASpecsOwnRefuseReplacesTheDefaultOnBothArrivals(t *testing.T) {
	ctx := context.Background()
	stoppingOpts := []EngineOption{WithDeferPhase()} // a defer segment's body: stopBeforeNewWork is true
	for _, arrival := range []string{"fresh entry", "replay frontier"} {
		history := func() []EventRecord {
			if arrival == "fresh entry" {
				return nil
			}
			return []EventRecord{{Step: 0, EventType: fakeEvent}}
		}
		run := func(opts []EngineOption, refuse func(*execSession) bool) (got int64, ran int32) {
			s := stepSession(opts, history())
			sp := fakeSpec(effectEffect, recordsOne, &ran, recordOneFake)
			sp.Refuse = refuse
			if len(s.history) > 0 {
				s.step(ctx, nil, fakeSpec(effectEffect, recordsOne, new(int32), recordOneFake), stepArgs{})
			}
			return s.step(ctx, nil, sp, stepArgs{}), ran
		}
		t.Run(arrival, func(t *testing.T) {
			// The default: nil Refuse is stopBeforeNewWork, so a stopping session refuses and a quiet one runs.
			if got, ran := run(stoppingOpts, nil); got != callSuspendSentinel || ran != 0 {
				t.Errorf("nil Refuse in a stopping session returned %#x after %d runs, want the stop sentinel and 0", got, ran)
			}
			if got, ran := run(nil, nil); got != 9 || ran != 1 {
				t.Errorf("CONTROL: nil Refuse in a quiet session returned %#x after %d runs, want 9 and 1", got, ran)
			}
			// A Refuse that says no overrides a stopping session: the step runs where the default would have refused it.
			if got, ran := run(stoppingOpts, func(*execSession) bool { return false }); got != 9 || ran != 1 {
				t.Errorf("a Refuse that declines, in a stopping session, returned %#x after %d runs, want 9 and 1 (not newly refused)", got, ran)
			}
			// A Refuse that says yes refuses a session the default would have let through.
			if got, ran := run(nil, func(*execSession) bool { return true }); got != callSuspendSentinel || ran != 0 {
				t.Errorf("a Refuse that refuses, in a quiet session, returned %#x after %d runs, want the stop sentinel and 0", got, ran)
			}
		})
	}
}

// A lookup answers at the replay frontier WITHOUT leaving replay (SideEffectRecorded asks, then SideEffect decides). It is still refused when the
// session is stopping, and it still reads a record that is there.
func TestALookupAtTheFrontierAnswersWithoutLeavingReplay(t *testing.T) {
	ctx := context.Background()
	lookup := func(ran *int32) *stepSpec {
		sp := fakeSpec(effectEffect, recordsNone, ran, func(*stepCtx) {})
		sp.StaysInReplayAtFrontier = true
		return sp
	}
	var ran int32
	s := stepSession(nil, []EventRecord{{Step: 0, EventType: fakeEvent}})
	if got := s.step(ctx, nil, lookup(&ran), stepArgs{}); got != 7 || s.stepCount != 1 {
		t.Fatalf("PRECONDITION FAILED: the lookup over a recorded event returned %d at step %d, want the decoded 7 at step 1", got, s.stepCount)
	}
	if got := s.step(ctx, nil, lookup(&ran), stepArgs{}); got != 9 || ran != 1 {
		t.Errorf("the lookup at the frontier returned %#x after %d runs, want 9 and 1", got, ran)
	}
	if !s.isReplay {
		t.Error("the lookup at the frontier left replay; the step that follows is the one that leaves")
	}
	if len(s.stepViolations) != 0 {
		t.Errorf("a lookup that stays in replay reported %v", s.stepViolations)
	}
	// The step that follows does the leaving, as SideEffect does after SideEffectRecorded.
	var ran2 int32
	next := fakeSpec(effectEffect, recordsOne, &ran2, recordOneFake)
	if got := s.step(ctx, nil, next, stepArgs{}); got != 9 || s.isReplay {
		t.Errorf("the step after the lookup returned %#x with isReplay=%v, want 9 and false", got, s.isReplay)
	}
	// Refused while stopping, before it answers.
	var ran3 int32
	stopping := stepSession([]EngineOption{WithDeferPhase()}, []EventRecord{{Step: 0, EventType: fakeEvent}})
	stopping.step(ctx, nil, fakeSpec(effectEffect, recordsOne, new(int32), recordOneFake), stepArgs{})
	if got := stopping.step(ctx, nil, lookup(&ran3), stepArgs{}); got != callSuspendSentinel || ran3 != 0 {
		t.Errorf("the lookup in a stopping session returned %#x after %d runs, want the stop sentinel and 0", got, ran3)
	}
	// The control: an ordinary step at the same frontier DOES leave replay, so the lookup's staying is the flag and not the harness.
	var ran4 int32
	plain := stepSession(nil, []EventRecord{{Step: 0, EventType: fakeEvent}})
	plain.step(ctx, nil, fakeSpec(effectEffect, recordsOne, new(int32), recordOneFake), stepArgs{})
	plain.step(ctx, nil, fakeSpec(effectEffect, recordsOne, &ran4, recordOneFake), stepArgs{})
	if plain.isReplay {
		t.Error("CONTROL: an ordinary step at the frontier did not leave replay")
	}
}
