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
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const fakeEvent EventType = "fake_step_event"

// fakeSpec builds a one-event spec whose Fresh counts how often it ran.
func fakeSpec(effect stepEffect, shape recordShape, ran *int32, record func(c *stepCtx)) *stepSpec {
	return &stepSpec{
		Name: "fake", Result: layoutSimple, Effect: effect, Records: shape, Want: fakeEvent, NoIdentity: "a fake with nothing to compare",
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
		"no name":                         {Result: layoutSimple, NoIdentity: "x", Want: fakeEvent, Decode: func(*stepCtx, *EventRecord) int64 { return 0 }, Fresh: func(*stepCtx) int64 { return 0 }},
		"no Want":                         {Result: layoutSimple, NoIdentity: "x", Name: "x", Decode: func(*stepCtx, *EventRecord) int64 { return 0 }, Fresh: func(*stepCtx) int64 { return 0 }},
		"no Decode":                       {Result: layoutSimple, NoIdentity: "x", Name: "x", Want: fakeEvent, Fresh: func(*stepCtx) int64 { return 0 }},
		"both Identity and NoIdentity":    {Result: layoutSimple, Name: "x", Want: fakeEvent, Identity: func(stepArgs, *EventRecord) string { return "" }, NoIdentity: "x", Decode: func(*stepCtx, *EventRecord) int64 { return 0 }, Fresh: func(*stepCtx) int64 { return 0 }},
		"neither Identity nor NoIdentity": {Result: layoutSimple, Name: "x", Want: fakeEvent, Decode: func(*stepCtx, *EventRecord) int64 { return 0 }, Fresh: func(*stepCtx) int64 { return 0 }},
		"Pure but records one":            {Result: layoutSimple, NoIdentity: "x", Name: "x", Want: fakeEvent, Effect: effectPure, Records: recordsOne, Decode: func(*stepCtx, *EventRecord) int64 { return 0 }, Fresh: func(*stepCtx) int64 { return 0 }},
	} {
		bad := bad
		if bad.validate() == nil {
			t.Errorf("CONTROL: validate accepted a spec with %s", name)
		}
	}
	// The hooks' rules, each with a fixture that reaches ITS rule and no earlier one, and the error asserted by TEXT. A fixture that leaves Records
	// unset gets recordsOne (the iota zero), which the older "Pure and records" rule rejects first, so the new rule would decide nothing and its
	// removal would go unseen (cleat-review3, measured on this test's first version: both mutants stayed green).
	noop := func(*stepCtx, *EventRecord) int64 { return 0 }
	nofresh := func(*stepCtx) int64 { return 0 }
	for name, c := range map[string]struct {
		spec stepSpec
		want string
	}{
		"Refuse on a Pure step": {
			stepSpec{Result: layoutSimple, NoIdentity: "x", Name: "x", Want: fakeEvent, Effect: effectPure, Records: recordsNone, Refuse: func(*execSession) bool { return true }, Decode: noop, Fresh: nofresh},
			"is Pure and has a Refuse",
		},
		"stays in replay but records one": {
			stepSpec{Result: layoutSimple, NoIdentity: "x", Name: "x", Want: fakeEvent, Effect: effectEffect, Records: recordsOne, StaysInReplayAtFrontier: true, Decode: noop, Fresh: nofresh},
			"stays in replay at the frontier",
		},
		"stays in replay but is Pure": {
			stepSpec{Result: layoutSimple, NoIdentity: "x", Name: "x", Want: fakeEvent, Effect: effectPure, Records: recordsNone, StaysInReplayAtFrontier: true, Decode: noop, Fresh: nofresh},
			"stays in replay at the frontier",
		},
	} {
		c := c
		err := c.spec.validate()
		if err == nil {
			t.Errorf("CONTROL: validate accepted a spec with %s", name)
		} else if !strings.Contains(err.Error(), c.want) {
			t.Errorf("CONTROL: a spec with %s was refused for the wrong reason: %q does not name %q", name, err.Error(), c.want)
		}
	}
	// CONTROL for the controls: the same two valid shapes, with the hook the rule is about removed, validate.
	for name, good := range map[string]stepSpec{
		"a Pure step with no Refuse":                     {Result: layoutSimple, NoIdentity: "x", Name: "x", Want: fakeEvent, Effect: effectPure, Records: recordsNone, Decode: noop, Fresh: nofresh},
		"a non-Pure recording-nothing lookup that stays": {Result: layoutSimple, NoIdentity: "x", Name: "x", Want: fakeEvent, Effect: effectEffect, Records: recordsNone, StaysInReplayAtFrontier: true, Decode: noop, Fresh: nofresh},
	} {
		good := good
		if err := good.validate(); err != nil {
			t.Errorf("CONTROL: %s was refused: %v", name, err)
		}
	}
	// The registry is checked against the SOURCE by NAME, in the other direction too, or a new spec nobody registered would be on no list. By name and not
	// by count: a count cannot say WHICH spec is missing, and a spec added beside one deleted nets to zero. The declarations and the registrations are read
	// from the syntax tree, so a comment or a string that quotes `registerStepSpec(&x)` registers nothing.
	problems, declared, registered, err := specRegistrationProblems(engineSources(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(declared) == 0 {
		t.Fatal("PRECONDITION FAILED: the syntax-tree scan found no `var x = stepSpec{` declaration, so it is reading the wrong thing and would pass whatever the tree said")
	}
	for _, p := range problems {
		t.Error(p)
	}
	// Three readings of one population, which must agree. The syntax tree's registrations against the runtime registry (a registration that did not run, or
	// ran twice, or a spec registered from somewhere the tree scan does not look); and the regexp scan the stop-site guards use for declarations against the
	// syntax tree's (so those guards count every dispatcher step the registry holds, which is what a later move must not be able to shrink).
	if len(registered) != len(stepSpecs) {
		t.Errorf("%d registrations in the source but %d specs in the registry", len(registered), len(stepSpecs))
	}
	if viaRegexp := specDeclsIn(engineSources(t)); !specSetsEqual(declared, viaRegexp) {
		t.Errorf("the syntax tree finds %d declarations %v, the regexp scan the stop-site guards use finds %d %v: one reading is wrong, and the guards would miss a dispatcher step",
			len(declared), specNames(declared), len(viaRegexp), specNames(specSetOf(viaRegexp)))
	}
}

// specDeclarationsAndRegistrations reads engine sources (file name -> text) and returns the names of the package-level `var x = stepSpec{...}`
// declarations, and the names registered by a top-level `registerStepSpec(&x)` statement in an init(): a registration in any other function, in a
// condition, or in a comment or string is not one.
func specDeclarationsAndRegistrations(sources map[string]string) (declared, registered map[string]bool, err error) {
	declared, registered = map[string]bool{}, map[string]bool{}
	fset := token.NewFileSet()
	for name, src := range sources {
		f, perr := parser.ParseFile(fset, name, src, 0)
		if perr != nil {
			return nil, nil, perr
		}
		for _, d := range f.Decls {
			switch d := d.(type) {
			case *ast.GenDecl:
				for _, sp := range d.Specs {
					vs, ok := sp.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for i, v := range vs.Values {
						if cl, ok := v.(*ast.CompositeLit); ok && i < len(vs.Names) {
							if id, ok := cl.Type.(*ast.Ident); ok && id.Name == "stepSpec" {
								declared[vs.Names[i].Name] = true
							}
						}
					}
				}
			case *ast.FuncDecl:
				if d.Recv != nil || d.Name.Name != "init" || d.Body == nil {
					continue
				}
				for _, st := range d.Body.List {
					es, ok := st.(*ast.ExprStmt)
					if !ok {
						continue
					}
					call, ok := es.X.(*ast.CallExpr)
					if !ok || len(call.Args) != 1 {
						continue
					}
					if fn, ok := call.Fun.(*ast.Ident); !ok || fn.Name != "registerStepSpec" {
						continue
					}
					if u, ok := call.Args[0].(*ast.UnaryExpr); ok && u.Op == token.AND {
						if id, ok := u.X.(*ast.Ident); ok {
							registered[id.Name] = true
						}
					}
				}
			}
		}
	}
	return declared, registered, nil
}

// specRegistrationProblems is the whole decision, on text, so the real tree and the rows below go through the same code: what is declared and never
// registered (by name, so a spec added beside one deleted does not net to zero), and what is registered and never declared.
func specRegistrationProblems(sources map[string]string) (problems []string, declared, registered map[string]bool, err error) {
	declared, registered, err = specDeclarationsAndRegistrations(sources)
	if err != nil {
		return nil, nil, nil, err
	}
	if missing := specSetDiff(declared, registered); len(missing) > 0 {
		problems = append(problems, fmt.Sprintf("declared but never registered by an init() calling registerStepSpec(&x): %v. Add `func init() { registerStepSpec(&x) }` beside the declaration, or the properties here do not cover it", missing))
	}
	if extra := specSetDiff(registered, declared); len(extra) > 0 {
		problems = append(problems, fmt.Sprintf("registered by name but declared nowhere as `var x = stepSpec{`: %v", extra))
	}
	return problems, declared, registered, nil
}

func specSetDiff(a, b map[string]bool) []string {
	var out []string
	for k := range a {
		if !b[k] {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func specNames(m map[string]bool) []string { return specSetDiff(m, nil) }

func specSetOf(m map[string]string) map[string]bool {
	out := map[string]bool{}
	for k := range m {
		out[k] = true
	}
	return out
}

func specSetsEqual(a map[string]bool, b map[string]string) bool {
	return len(specSetDiff(a, specSetOf(b))) == 0 && len(specSetDiff(specSetOf(b), a)) == 0
}

// The rule for what counts as a registration, on text, with a known-positive: a spec declared and never registered is reported BY NAME; and the shapes
// that look like a registration and are not (a comment, a string, a function that is not init, a condition) do not count. Without these rows a scan that
// matched the text `registerStepSpec(&x)` anywhere would pass the tree it exists to reject.
func TestASpecIsRegisteredOnlyByAnInitThatCallsRegisterStepSpec(t *testing.T) {
	decl := "package p\n\nvar xStep = stepSpec{}\n\n"
	for _, c := range []struct {
		name                  string
		src                   string
		wantMissing, wantExtr []string
	}{
		{"a declaration with its init registration is clean", decl + "func init() { registerStepSpec(&xStep) }\n", nil, nil},
		{"KNOWN-POSITIVE: a spec declared and never registered is reported by name", decl, []string{"xStep"}, nil},
		{"a registration quoted in a comment is not one", decl + "// func init() { registerStepSpec(&xStep) }\n", []string{"xStep"}, nil},
		{"a registration quoted in a string is not one", decl + "var doc = `registerStepSpec(&xStep)`\nfunc init() {}\n", []string{"xStep"}, nil},
		{"a registration in a function that is not init is not one", decl + "func later() { registerStepSpec(&xStep) }\n", []string{"xStep"}, nil},
		{"a registration inside a condition is not one", decl + "func init() { if false { registerStepSpec(&xStep) } }\n", []string{"xStep"}, nil},
		{"a registration of something never declared is reported as extra", decl + "func init() { registerStepSpec(&xStep); registerStepSpec(&yStep) }\n", nil, []string{"yStep"}},
		{"NET ZERO: a spec declared and never registered, beside a registration of one that is not declared: BOTH are named, though the counts agree",
			decl + "var zStep = stepSpec{}\nfunc init() { registerStepSpec(&xStep); registerStepSpec(&yStep) }\n", []string{"zStep"}, []string{"yStep"}},
		{"two specs, one registered: the other is named", decl + "var zStep = stepSpec{}\nfunc init() { registerStepSpec(&xStep) }\n", []string{"zStep"}, nil},
	} {
		problems, declared, registered, err := specRegistrationProblems(map[string]string{"p.go": c.src})
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		missing, extra := specSetDiff(declared, registered), specSetDiff(registered, declared)
		if strings.Join(missing, ",") != strings.Join(c.wantMissing, ",") || strings.Join(extra, ",") != strings.Join(c.wantExtr, ",") {
			t.Errorf("%s: missing %v extra %v, want %v and %v", c.name, missing, extra, c.wantMissing, c.wantExtr)
		}
		if want := len(c.wantMissing) > 0 || len(c.wantExtr) > 0; (len(problems) > 0) != want {
			t.Errorf("%s: %d problem(s) reported %v, want problems=%v", c.name, len(problems), problems, want)
		}
		// every wanted name is in the REPORT, not only in the sets: the decision is what a person reads, and a count-shaped one drops a name
		for _, n := range append(append([]string{}, c.wantMissing...), c.wantExtr...) {
			if !strings.Contains(strings.Join(problems, "\n"), n) {
				t.Errorf("%s: the report %v does not name %q", c.name, problems, n)
			}
		}
	}
}

// registerStepSpecInto rejects what the old start-up loop and the old count could not tell apart: a malformed spec (with validate's own message, so a
// spec rejected for the wrong reason is seen), the same spec twice, and two specs that share a name; and accepts a valid one in order.
func TestRegisterStepSpecRejectsAMalformedOrRepeatedSpecAndAcceptsAValidOne(t *testing.T) {
	good := func(name string) *stepSpec {
		return &stepSpec{Name: name, Result: layoutSimple, NoIdentity: "x", Want: fakeEvent, Effect: effectEffect, Records: recordsOne, Decode: func(*stepCtx, *EventRecord) int64 { return 0 }, Fresh: func(*stepCtx) int64 { return 0 }}
	}
	panics := func(f func()) (msg string) {
		defer func() {
			if r := recover(); r != nil {
				msg = fmt.Sprint(r)
			}
		}()
		f()
		return ""
	}
	var list []*stepSpec
	a, b := good("a"), good("b")
	if m := panics(func() { registerStepSpecInto(&list, a) }); m != "" {
		t.Fatalf("a valid spec was refused: %s", m)
	}
	registerStepSpecInto(&list, b)
	if len(list) != 2 || list[0] != a || list[1] != b {
		t.Fatalf("registered %v, want [a b] in order", list)
	}
	bad := good("c")
	bad.Fresh = nil
	for _, c := range []struct {
		name, want string
		f          func()
	}{
		{"a malformed spec is refused with validate's own message", "is missing its Fresh", func() { registerStepSpecInto(&list, bad) }},
		{"the same spec twice is refused", "is registered twice", func() { registerStepSpecInto(&list, a) }},
		{"two specs with one name are refused", "two step specs are named", func() { registerStepSpecInto(&list, good("a")) }},
	} {
		if m := panics(c.f); !strings.Contains(m, c.want) {
			t.Errorf("%s: panicked with %q, want it to contain %q", c.name, m, c.want)
		}
	}
	if len(list) != 2 {
		t.Errorf("a refused spec was still added: %d entries", len(list))
	}
	// and the real registry has no two of one name, which registration enforces; the names are read here so a reviewer sees what is registered
	seen := map[string]bool{}
	for _, sp := range stepSpecs {
		if seen[sp.Name] {
			t.Errorf("the real registry holds two specs named %q", sp.Name)
		}
		seen[sp.Name] = true
	}
}

// The two exemptions the stop-site guards carry for the dispatcher say something about CALLERS ("leaveReplay's only caller is step",
// "Record is reached only from a spec's Fresh"), and the guards' stale-entry checks look at the exempt function's BODY, not at who calls it.
// A second caller of leaveReplay that skipped step would be legitimised by the exemption, which is the bypass the dispatcher exists to prevent
// (WS-cleat-2, review of #4207). This reads the callers from the syntax tree, so a comment or a string cannot satisfy it.

// dispatcherCallerFacts parses non-test sources (name -> source) and returns where leaveReplay is called from, and how many .Record( calls
// there are in all and how many sit inside the Fresh function of a stepSpec literal.
func dispatcherCallerFacts(t *testing.T, sources map[string]string) (leaveReplayCallers, leaveAtPendingCallers []string, recordCalls, recordInFresh int) {
	t.Helper()
	fset := token.NewFileSet()
	parsed := map[string]*ast.File{}
	for name, src := range sources {
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		parsed[name] = f
	}
	// The source ranges of every `Fresh: func(...) {...}` inside a stepSpec composite literal, per file.
	freshByFile := map[string][][2]token.Pos{}
	for name, f := range parsed {
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
		freshByFile[name] = freshRanges
	}
	inFreshAt := func(file string, pos, end token.Pos) bool {
		for _, r := range freshByFile[file] {
			if pos >= r[0] && end <= r[1] {
				return true
			}
		}
		return false
	}
	// Every REFERENCE to a name (a method or a function), so a helper that holds a Record call can be asked who uses it. A reference is a call or a use as a
	// value (`f := s.helper`, `go s.helper`, an argument): counting calls only let a method value taken outside a Fresh and called later pass as "all in
	// Fresh" (cleat-review3, on #4303). A function's own declaration name is not a reference. A reference to a DIFFERENT function of the same name counts
	// too, which can only make the answer stricter: a helper is vouched for only if every reference by its name is in a Fresh.
	type callSite struct {
		file     string
		pos, end token.Pos
	}
	sitesByName := map[string][]callSite{}
	for name, f := range parsed {
		skip := map[*ast.Ident]bool{}
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.FuncDecl:
				skip[x.Name] = true
			case *ast.SelectorExpr:
				skip[x.Sel] = true // counted once, as the selector, not again as the identifier under it
				sitesByName[x.Sel.Name] = append(sitesByName[x.Sel.Name], callSite{name, x.Pos(), x.End()})
			case *ast.Ident:
				if !skip[x] {
					sitesByName[x.Name] = append(sitesByName[x.Name], callSite{name, x.Pos(), x.End()})
				}
			}
			return true
		})
	}
	for name, f := range parsed {
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
				if sel.Sel.Name == "leaveReplayAtPending" {
					leaveAtPendingCallers = append(leaveAtPendingCallers, fd.Name.Name)
				}
				return true
			})
		}
		// Record calls can sit in a package-level var (the specs are vars), so they are found over the whole file, not only inside functions.
		//
		// A Record call is "reached only from Fresh" in one of two ways. It sits lexically inside a Fresh literal; or it sits in a helper that takes the
		// step's context (a *stepCtx parameter) and EVERY call to that helper is itself inside a Fresh literal, with at least one call (a helper nobody
		// calls is vacuously "all in Fresh" and is exactly the bypass this guard exists to find). The second is what a Fresh too long to write inline
		// needs (the heartbeat call's goroutine and ticker, cleat#4152 G3). One level only: a helper reached through another helper is reported, not
		// followed.
		takesStepCtx := func(fd *ast.FuncDecl) bool {
			if fd.Type.Params == nil {
				return false
			}
			for _, p := range fd.Type.Params.List {
				if star, ok := p.Type.(*ast.StarExpr); ok {
					if id, ok := star.X.(*ast.Ident); ok && id.Name == "stepCtx" {
						return true
					}
				}
			}
			return false
		}
		helperInFresh := map[*ast.FuncDecl]bool{}
		helperVouched := func(fd *ast.FuncDecl) bool {
			if v, ok := helperInFresh[fd]; ok {
				return v
			}
			sites := sitesByName[fd.Name.Name]
			ok := takesStepCtx(fd) && len(sites) > 0
			for _, c := range sites {
				if !inFreshAt(c.file, c.pos, c.end) {
					ok = false
				}
			}
			helperInFresh[fd] = ok
			return ok
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || (sel.Sel.Name != "Record" && sel.Sel.Name != "RecordPersisted" && sel.Sel.Name != "ContinueAsNewAtEventCap") {
				return true
			}
			recordCalls++
			if inFreshAt(name, call.Pos(), call.End()) {
				recordInFresh++
				return true
			}
			for _, d := range f.Decls {
				if fd, ok := d.(*ast.FuncDecl); ok && fd.Body != nil && call.Pos() >= fd.Pos() && call.End() <= fd.End() && helperVouched(fd) {
					recordInFresh++
					break
				}
			}
			return true
		})
	}
	return leaveReplayCallers, leaveAtPendingCallers, recordCalls, recordInFresh
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
	callers, pendingCallers, records, inFresh := dispatcherCallerFacts(t, sources)

	// Known positive: the scan must SEE the calls that exist today, or "no second caller" would be what a scan that matches nothing says.
	if len(callers) != 1 || callers[0] != "step" {
		t.Errorf("leaveReplay is called from %v, want exactly [step]: the exemption for leaveReplay says its only caller is step, and a second caller would skip the refusal that follows it", callers)
	}
	// The second exit from replay (a step that found its own pending record, OverPending) has the same exemption and needs the same proof.
	// step calls it from two places (an own pending record, and a run that ended at a foreign record), so it is the distinct CALLER FUNCTIONS that must be exactly [step].
	distinctPending := map[string]bool{}
	for _, c := range pendingCallers {
		distinctPending[c] = true
	}
	if len(pendingCallers) < 2 || len(distinctPending) != 1 || !distinctPending["step"] {
		t.Errorf("leaveReplayAtPending is called from %v (call sites), want step only, at least twice: its exemption says its only caller is step, and a second caller would skip the refusal that follows it", pendingCallers)
	}
	if records < 3 {
		t.Fatalf("PRECONDITION FAILED: the scan found %d .Record( calls, want the 3 the promise specs make; it is reading nothing", records)
	}
	if inFresh != records {
		t.Errorf("%d of %d .Record( calls are outside a stepSpec's Fresh function: the exemption for Record says it is reached only from there, where step has already refused", records-inFresh, records)
	}

	// Negative control: the same scan over a source with a second caller of each kind must report them.
	badStep := `package engine
func (s *execSession) step() { s.leaveReplay(nil); s.leaveReplayAtPending(nil, 0) }
func (s *execSession) sneaky() { s.leaveReplay(nil) }
func (s *execSession) sneakyPending() { s.leaveReplayAtPending(nil, 0) }
var spec = stepSpec{Fresh: func(c *stepCtx) int64 { c.Record(EventRecord{}); return 0 }}
func (s *execSession) bypass(c *stepCtx) { c.Record(EventRecord{}) }
var specHelper = stepSpec{Fresh: func(c *stepCtx) int64 { return c.s.okHelper(c) }}
func (s *execSession) okHelper(c *stepCtx) int64 { c.Record(EventRecord{}); return 0 }
var specMixed = stepSpec{Fresh: func(c *stepCtx) int64 { return c.s.mixedHelper(c) }}
func (s *execSession) mixedHelper(c *stepCtx) int64 { c.Record(EventRecord{}); return 0 }
func (s *execSession) elsewhere(c *stepCtx) { s.mixedHelper(c) }
var specValue = stepSpec{Fresh: func(c *stepCtx) int64 { return c.s.valueHelper(c) }}
func (s *execSession) valueHelper(c *stepCtx) int64 { c.Record(EventRecord{}); return 0 }
func (s *execSession) grabsItAsAValue() { f := s.valueHelper; _ = f }
`
	cc, pc, rr, rf := dispatcherCallerFacts(t, map[string]string{"bad.go": badStep})
	// Five Records: one inline in a Fresh (in), one in a helper nobody calls (out: vacuously "all in Fresh", the bypass), one in a helper called only from a
	// Fresh (in), one in a helper called from a Fresh AND from elsewhere (out), and one in a helper called from a Fresh whose method value is ALSO taken
	// outside a Fresh (out: a reference that is not a call).
	if len(cc) != 2 || len(pc) != 2 || rr != 5 || rf != 2 {
		t.Errorf("CONTROL: on a source with a second caller of each exit, a Record outside Fresh, a helper called only from Fresh and a helper called from elsewhere too, the scan reported callers=%v pendingCallers=%v records=%d inFresh=%d, want 2, 2, 5 records, 2 in Fresh", cc, pc, rr, rf)
	}
}

// OverPending (cleat#4152, W3a): a step that finds its OWN pending record does not consume it. It leaves replay with the step count ON that record and runs
// fresh, and the answer is recorded at that step. Six cases of the one mechanism, on a fake spec whose Fresh counts runs.
func TestAStepFindingItsOwnPendingRecordLeavesReplayAtItWithoutConsumingIt(t *testing.T) {
	ctx := context.Background()
	pendingSpec := func(ran *int32) *stepSpec {
		sp := fakeSpec(effectEffect, recordsOne, ran, recordOneFake)
		sp.OverPending = func(_ stepArgs, rec *EventRecord) bool { return rec.Response == "" }
		return sp
	}
	pending := EventRecord{Step: 0, EventType: fakeEvent}
	answered := EventRecord{Step: 0, EventType: fakeEvent, Response: "r"}
	other := EventRecord{Step: 1, EventType: EventTypeSideEffect}

	// 1. a pending record that is the LAST one: not consumed, replay left, Fresh runs and records at the same step.
	var ran int32
	s := stepSession(nil, []EventRecord{pending})
	if got := s.step(ctx, nil, pendingSpec(&ran), stepArgs{}); got != 9 || ran != 1 {
		t.Errorf("a pending record: result %d, Fresh ran %d times, want 9 and 1", got, ran)
	}
	if s.isReplay || s.stepCount != 1 || len(s.history) != 2 || s.history[1].Step != 0 {
		t.Errorf("a pending record: isReplay=%v stepCount=%d history=%d (answer recorded at step %d), want replay left, stepCount 1, 2 records, the answer at step 0",
			s.isReplay, s.stepCount, len(s.history), s.history[len(s.history)-1].Step)
	}
	if len(s.stepViolations) != 0 {
		t.Errorf("a pending record: the dispatcher reported spec violations: %v", s.stepViolations)
	}

	// 2. an ANSWERED record is consumed and decoded as any record: Fresh does not run.
	ran = 0
	s = stepSession(nil, []EventRecord{answered})
	if got := s.step(ctx, nil, pendingSpec(&ran), stepArgs{}); got != 7 || ran != 0 || !s.isReplay || s.stepCount != 1 {
		t.Errorf("an answered record: result %d, ran %d, isReplay=%v stepCount=%d, want 7, 0, still replaying at 1", got, ran, s.isReplay, s.stepCount)
	}

	// 3. a pending record FOLLOWED by another record (cleat#4285). A call the guest's BODY recorded there (a side_effect is the effect of a call) is a divergence that
	// consumes nothing: Fresh does not run, and replay is not left, so the recorded call cannot run a second time. That is the half the first version of this
	// case pinned the other way, as the kept defect.
	ran = 0
	s = stepSession(nil, []EventRecord{pending, other})
	if got := s.step(ctx, nil, pendingSpec(&ran), stepArgs{}); got&0xFF != 1 || ran != 0 || !s.isReplay || s.stepCount != 0 || len(s.history) != 2 {
		t.Errorf("a pending record with a body call after it: result %#x ran %d isReplay=%v stepCount=%d history=%d, want a divergence, 0 runs, still replaying at 0, history unchanged",
			got, ran, s.isReplay, s.stepCount, len(s.history))
	}
	//    ...and the other half, which IS still left unread, on purpose: the same record stamped by a defer drain (its own defers follow its pending await by design).
	drained := other
	drained.InDeferPhase = true
	ran = 0
	s = stepSession(nil, []EventRecord{pending, drained})
	s.step(ctx, nil, pendingSpec(&ran), stepArgs{})
	if ran != 1 || s.isReplay || s.stepCount >= len(s.history) {
		t.Errorf("a pending record with a drain's record after it: ran %d, isReplay=%v stepCount=%d of %d, want Fresh run, replay left and the later record still unread",
			ran, s.isReplay, s.stepCount, len(s.history))
	}

	// 4. a record of another type is a divergence and OverPending is not consulted.
	ran = 0
	s = stepSession(nil, []EventRecord{other})
	sp := pendingSpec(&ran)
	sp.OverPending = func(stepArgs, *EventRecord) bool {
		t.Error("OverPending was asked about a record of another type")
		return true
	}
	if got := s.step(ctx, nil, sp, stepArgs{}); got&0xFF != 1 || ran != 0 || !s.isReplay || s.stepCount != 0 {
		t.Errorf("another type: result %#x ran %d isReplay=%v stepCount=%d, want a divergence, 0 runs, still replaying at 0", got, ran, s.isReplay, s.stepCount)
	}

	// 5. a pending record that names ANOTHER call is a divergence (Identity comes first), not this call's to answer over.
	ran = 0
	s = stepSession(nil, []EventRecord{{Step: 0, EventType: fakeEvent, PromiseID: "other"}})
	sp = pendingSpec(&ran)
	sp.NoIdentity = ""
	sp.Identity = func(_ stepArgs, rec *EventRecord) string {
		if rec.PromiseID != "mine" {
			return "recorded " + rec.PromiseID
		}
		return ""
	}
	if got := s.step(ctx, nil, sp, stepArgs{}); got&0xFF != 1 || ran != 0 || !s.isReplay || s.stepCount != 0 {
		t.Errorf("a pending record naming another call: result %#x ran %d isReplay=%v stepCount=%d, want a divergence, 0 runs, still replaying at 0", got, ran, s.isReplay, s.stepCount)
	}

	// 6. the refusal still applies after a step leaves replay at its pending record: the call is refused, not run, and the record is not consumed.
	ran = 0
	s = stepSession([]EngineOption{WithDeferPhase()}, []EventRecord{pending})
	if got := s.step(ctx, nil, pendingSpec(&ran), stepArgs{}); got != callSuspendSentinel || ran != 0 || s.stepCount != 0 {
		t.Errorf("a refused step over a pending record: result %#x ran %d stepCount=%d, want the stop, 0 runs and the record not consumed", got, ran, s.stepCount)
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

// RecordsNothingOn (cleat#4152, G3, DurableCall): a recordsOne spec may record nothing on a path it LISTED, and on no other. The default for every other spec
// is the rejection that was always there, so each row below that expects a violation is a row that fails if the field loosened anything globally.
func TestARecordsOneSpecMayRecordNothingOnlyOnAPathItListed(t *testing.T) {
	declines := func(why string) func(c *stepCtx) { return func(c *stepCtx) { c.RecordsNothing(why) } }
	cases := []struct {
		name   string
		shape  recordShape
		listed []string
		record func(c *stepCtx)
		want   string // "" means no violation
	}{
		{"a listed path, announced under its name", recordsOne, []string{"cancelled"}, declines("cancelled"), ""},
		{"a listed path, announced under ANOTHER name", recordsOne, []string{"cancelled"}, declines("worker lost"), `records nothing on a path ("worker lost") that its RecordsNothingOn does not list`},
		{"nothing listed, a path announced: the default rejection stands", recordsOne, nil, declines("cancelled"), `records nothing on a path ("cancelled") that its RecordsNothingOn does not list`},
		{"a listed path, nothing announced: silence is not a path", recordsOne, []string{"cancelled"}, func(*stepCtx) {}, "but recorded 0"},
		{"nothing listed, nothing announced: the old rejection", recordsOne, nil, func(*stepCtx) {}, "but recorded 0"},
		{"a listed path and an event recorded anyway is still one event", recordsOne, []string{"cancelled"}, recordOneFake, ""},
		{"two events are still two events", recordsOne, []string{"cancelled"}, func(c *stepCtx) { recordOneFake(c); recordOneFake(c) }, "but recorded 2"},
		// Control: a recordsMany spec could always record nothing and say so, with no list. The field must not have changed that.
		{"recordsMany, announced, no list: unchanged", recordsMany, nil, declines("anything"), ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var ran int32
			s := stepSession(nil, nil)
			sp := fakeSpec(effectEffect, c.shape, &ran, c.record)
			sp.RecordsNothingOn = c.listed
			if c.shape == recordsMany {
				sp.RecordsNothingOn = nil
			}
			s.step(context.Background(), nil, sp, stepArgs{})
			joined := strings.Join(s.stepViolations, "; ")
			if c.want == "" && joined != "" {
				t.Errorf("reported %q for a spec that did what it declared", joined)
			}
			if c.want != "" && !strings.Contains(joined, c.want) {
				t.Errorf("reported %q, want it to contain %q", joined, c.want)
			}
		})
	}

	// The suspend sentinel does not excuse a recordsOne spec: refused is for recordsMany. A Fresh that returns it without recording is reported.
	var ran int32
	s := stepSession(nil, nil)
	sp := fakeSpec(effectEffect, recordsOne, &ran, func(*stepCtx) {})
	sp.Fresh = func(*stepCtx) int64 { return callSuspendSentinel }
	s.step(context.Background(), nil, sp, stepArgs{})
	if joined := strings.Join(s.stepViolations, "; "); !strings.Contains(joined, "but recorded 0") {
		t.Errorf("a recordsOne spec that returned the suspend sentinel and recorded nothing was reported %q, want the shape violation: the sentinel must not excuse it", joined)
	}

	// THE PIN: no spec in the engine lists a path, so none of them is looser than it was before the field existed. When a spec starts to (DurableCall's
	// move), this map is what changes, in the same diff, and the reviewer reads each path against the call it is for. A spec added to the registry that
	// lists a path and is not in this map fails here, which is what makes the opt-in visible and not merely available.
	listing := map[string][]string{}
	for _, sp := range stepSpecs {
		if len(sp.RecordsNothingOn) > 0 {
			listing[sp.Name] = sp.RecordsNothingOn
		}
	}
	if len(stepSpecs) < 20 {
		t.Fatalf("PRECONDITION FAILED: %d registered specs; the scan is not reading the registry", len(stepSpecs))
	}
	if len(listing) != 0 {
		t.Errorf("specs list RecordsNothingOn paths: %v. This test records that none does; a spec that opts in is added to this check in the same change, with the reason for each path", listing)
	}
}

func TestRecordsNothingOnIsValidated(t *testing.T) {
	mk := func(shape recordShape, listed ...string) *stepSpec {
		var ran int32
		sp := fakeSpec(effectEffect, shape, &ran, recordOneFake)
		sp.RecordsNothingOn = listed
		return sp
	}
	for _, c := range []struct {
		name string
		sp   *stepSpec
		want string // "" means valid
	}{
		{"recordsOne with a path", mk(recordsOne, "cancelled", "worker lost"), ""},
		{"recordsOne with no list", mk(recordsOne), ""},
		{"recordsMany with a list", mk(recordsMany, "cancelled"), "does not declare recordsOne"},
		{"recordsNone with a list", mk(recordsNone, "cancelled"), "does not declare recordsOne"},
		{"an empty entry", mk(recordsOne, ""), "empty or repeated"},
		{"a repeated entry", mk(recordsOne, "cancelled", "cancelled"), "empty or repeated"},
	} {
		err := c.sp.validate()
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: a well-formed spec was refused: %v", c.name, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: got %v, want an error containing %q", c.name, err, c.want)
		}
	}
}

// RecordPersisted (cleat#4152, G3, DurableCall): the write-ahead path persists its record itself and must still be COUNTED, or the shape check reads a
// recorded event as a spec that recorded nothing. The row that matters is the second: a spec that calls recordEventPersisted directly moves the step
// count and not the handle's, and is reported, which is the only thing that stops the next write-ahead call from skipping the accounting unnoticed.
func TestRecordPersistedIsCountedAndRecordingPersistedBehindTheHandleIsReported(t *testing.T) {
	persist := func(c *stepCtx) { c.RecordPersisted(EventRecord{Step: c.s.stepCount, EventType: fakeEvent}, "chain-1") }
	behind := func(c *stepCtx) {
		c.s.recordEventPersisted(EventRecord{Step: c.s.stepCount, EventType: fakeEvent}, "chain-1")
	}
	for _, c := range []struct {
		name   string
		record func(c *stepCtx)
		want   string
	}{
		{"through the handle", persist, ""},
		{"behind the handle", behind, "recorded 0 event(s) through the handle but the step count moved by 1"},
	} {
		t.Run(c.name, func(t *testing.T) {
			var ran int32
			s := stepSession(nil, nil)
			s.step(context.Background(), nil, fakeSpec(effectEffect, recordsOne, &ran, c.record), stepArgs{})
			if len(s.history) != 1 || s.stepCount != 1 || s.lastChecksum != "chain-1" {
				t.Errorf("the persisted record did not reach the session: history=%d stepCount=%d lastChecksum=%q, want 1, 1, chain-1", len(s.history), s.stepCount, s.lastChecksum)
			}
			joined := strings.Join(s.stepViolations, "; ")
			if c.want == "" && joined != "" {
				t.Errorf("reported %q for a record made through the handle", joined)
			}
			if c.want != "" && !strings.Contains(joined, c.want) {
				t.Errorf("reported %q, want it to contain %q", joined, c.want)
			}
		})
	}
}

// The scan that requires every Record to sit inside a spec's Fresh must see RecordPersisted as a Record: a second way to record that it did not know
// about would be a way around the refusal the exemption rests on. Known positive, from source with one of each.
func TestTheExemptionsScanSeesRecordPersistedToo(t *testing.T) {
	src := `package engine
var specP = stepSpec{Fresh: func(c *stepCtx) int64 { c.RecordPersisted(EventRecord{}, ""); return 0 }}
func (s *execSession) bypassP(c *stepCtx) { c.RecordPersisted(EventRecord{}, "") }
`
	_, _, records, inFresh := dispatcherCallerFacts(t, map[string]string{"persisted.go": src})
	if records != 2 || inFresh != 1 {
		t.Errorf("the scan found %d RecordPersisted calls of which %d inside a Fresh, want 2 and 1: one inline in a Fresh, one in a function outside any", records, inFresh)
	}
}

// ContinueAsNewAtEventCap (cleat#4152, G3): the continue-as-new an event cap asks for records an event at the step the call was on, and a Fresh that
// starts it through the handle is counted for it, so the shape check agrees with the step count. The second row is the control, and it is the BEHAVIOUR
// BEFORE: the same continue-as-new started directly records the same event behind the handle and is reported.
func TestContinueAsNewAtEventCapIsCountedAndStartingItBehindTheHandleIsReported(t *testing.T) {
	atCap := func(c *stepCtx) { c.ContinueAsNewAtEventCap() }
	behind := func(c *stepCtx) { c.s.triggerEventCapContinueAsNew(c.ctx, c.m) }
	for _, tc := range []struct {
		name   string
		record func(c *stepCtx)
		want   string
	}{
		{"through the handle", atCap, ""},
		{"behind the handle", behind, "recorded 0 event(s) through the handle but the step count moved by 1"},
		// The count is what the continue-as-new DID, not a constant: a continue-as-new that is refused (the stop was set after step() applied its own
		// refusal, which a write failing inside the Fresh does) records nothing, and a spec that counted it as one would hide a Fresh that recorded none.
		{"refused: nothing recorded, and counted as nothing", func(c *stepCtx) {
			c.s.eventWriteFailure = errors.New("event write refused")
			c.ContinueAsNewAtEventCap()
		}, "but recorded 0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var ran int32
			s := stepSession([]EngineOption{WithMaxQuotaEvents(1)}, nil)
			s.eventCount, s.originalInput = 1, `{}`
			sp := fakeSpec(effectEffect, recordsOne, &ran, tc.record)
			s.step(context.Background(), nil, sp, stepArgs{})
			if strings.HasPrefix(tc.name, "refused") {
				if len(s.history) != 0 {
					t.Errorf("PRECONDITION FAILED: the continue-as-new was not refused (history=%v), so this row measures nothing", s.history)
				}
			} else if len(s.history) != 1 || s.history[0].EventType != EventTypeContinueAsNew || !s.autoContinueAsNewTriggered || s.suspendErr == nil {
				t.Errorf("the continue-as-new did not happen: history=%v triggered=%v suspendErr=%v", s.history, s.autoContinueAsNewTriggered, s.suspendErr)
			}
			joined := strings.Join(s.stepViolations, "; ")
			if tc.want == "" && joined != "" {
				t.Errorf("reported %q for a continue-as-new started through the handle", joined)
			}
			if tc.want != "" && !strings.Contains(joined, tc.want) {
				t.Errorf("reported %q, want it to contain %q", joined, tc.want)
			}
		})
	}
}

// A spec that was reporting a violation at the cap (the two no-wait awaits, measured on develop 615c81c4) reports none, and every one of the polls
// below the cap is still clean. The existing test for the cap (a_no_wait_poll_counts_toward_the_event_cap_test.go) never read the violations, which is
// how this stayed in the log at error level.
func TestTheNoWaitAwaitsReportNoSpecViolationAtTheEventCap(t *testing.T) {
	const cap = 3
	for _, name := range []string{"await_promise", "await_signals"} {
		t.Run(name, func(t *testing.T) {
			s, _ := originalRun(t, WithMaxQuotaEvents(cap), WithPromiseStore(&mockPromiseStore{status: ""}), WithSignalStore(&awaitProbeStore{deliveries: map[string]SignalDelivery{}}))
			s.originalInput = `{}`
			poll := func() int64 {
				if name == "await_promise" {
					return s.AwaitPromise(hostCtx(), nil, "prom-cap", 0, 0, 256)
				}
				return s.DurableAwaitSignals(hostCtx(), nil, "go", 0, 0, 64, 64, 256)
			}
			for i := 0; i < cap; i++ {
				poll()
			}
			if len(s.stepViolations) != 0 {
				t.Fatalf("the polls below the cap reported %q", s.stepViolations)
			}
			if got := poll(); got != callSuspendSentinel || !s.autoContinueAsNewTriggered {
				t.Fatalf("PRECONDITION FAILED: the poll at the cap returned %#x (triggered=%v); the cap did not fire, so this proves nothing", got, s.autoContinueAsNewTriggered)
			}
			if len(s.stepViolations) != 0 {
				t.Errorf("the poll at the cap reported %q; the continue-as-new it starts records an event and must be counted", s.stepViolations)
			}
		})
	}
}

// The scan that requires every Record to sit inside a spec's Fresh sees ContinueAsNewAtEventCap too: it records an event on the Fresh's behalf, so a
// second way to start it from outside a Fresh would skip the refusal the exemption rests on. Known positive.
func TestTheExemptionsScanSeesContinueAsNewAtEventCapToo(t *testing.T) {
	src := `package engine
var specC = stepSpec{Fresh: func(c *stepCtx) int64 { c.ContinueAsNewAtEventCap(); return 0 }}
func (s *execSession) bypassC(c *stepCtx) { c.ContinueAsNewAtEventCap() }
`
	_, _, records, inFresh := dispatcherCallerFacts(t, map[string]string{"cap.go": src})
	if records != 2 || inFresh != 1 {
		t.Errorf("the scan found %d ContinueAsNewAtEventCap calls of which %d inside a Fresh, want 2 and 1", records, inFresh)
	}
}
