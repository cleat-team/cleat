package engine

// cleat#4285: once an execution has suspended, a guest that ignored the suspension must not start fresh work, because every call it makes afterwards is
// recorded AFTER the pending await and runs AGAIN when the workflow resumes. The refusal is applied in one of two places: step() applies it to every non-Pure
// dispatcher step, and a call not yet on the dispatcher calls refuseIfSuspended itself. This test is what makes "every call" true rather than a sentence: each
// method of HostHandler is a dispatcher step, reaches refuseIfSuspended (in its own body or in a function it calls directly: the fresh* helpers), or is named
// below with the reason it needs no refusal. A migration to step() moves a call from the second kind to the first and changes nothing here.

import (
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// callsNeedingNoPostSuspendRefusal names the host calls that need no call to refuseIfSuspended, with the reason: they start no fresh work, or (AwaitAnyChild) they carry their own equivalent guard. A call is here because the reason is true of it, and
// TestThePostSuspendExemptionsStillDescribeSomething fails when one is covered after all, so an entry cannot outlive its reason.
var callsNeedingNoPostSuspendRefusal = map[string]string{
	// Each reason is what was MEASURED of the call (2026-11-20): none of these records an event or assigns a suspension, which is what makes a repeat after
	// an ignored suspension harmless. A call that gains either must leave this list.
	"AwaitAnyChild":        "HAS ITS OWN equivalent guard (suspendErr != nil && !inDeferDrain, answering 1<<62), kept verbatim because the shared helper also exempts the guest's own drain and would loosen it; W3b moves the call onto the dispatcher",
	"DurableLog":           "writes a log line and nothing else, and not even that on replay",
	"GetScope":             "reads the scope; records nothing, sets no suspension",
	"JsonParse":            "a pure function of its arguments",
	"JsonStringify":        "a pure function of its arguments",
	"MinVersion":           "reads the engine's version state",
	"Version":              "reads the engine's version state",
	"Now":                  "reads the recorded clock",
	"PollCancellation":     "a read; writes the answer into the guest's buffer, records nothing, sets no suspension",
	"PollChild":            "a read of the child's state; writes the answer into the guest's buffer, records nothing, sets no suspension",
	"PollSignal":           "a read of the signal store; records nothing, sets no suspension",
	"Random":               "deterministic from the workflow id, the step count and a counter; records nothing",
	"UUID":                 "a hash of the workflow id and the seed; records nothing",
	"RegisterQueryHandler": "appends a name to an in-memory list; records nothing, sets no suspension",
	"RunID":                "reads an id",
	"WorkflowID":           "reads an id",
	"ServeWasiSleep":       "returns a duration the host waits out in-process; it assigns no suspension and records nothing",
	"SetDeferPhase":        "sets a flag, and must stay callable: it is how the guest says it is running its cleanup",
	"SetQueryState":        "writes the in-memory query map; records nothing, sets no suspension",
}

// postSuspendCoverageProblemsIn reports, for the HostHandler methods `methods` with their bodies, each one that is neither a dispatcher step, nor reaches
// refuseIfSuspended directly or through one call, nor is named in exempt; and each exemption that names no method or names a covered one.
func postSuspendCoverageProblemsIn(methods []string, bodies map[string]string, exempt map[string]string) []string {
	call := regexp.MustCompile(`\bs\.([A-Za-z_]\w*)\(`)
	// The CALL form, in code with its comments removed: a sentence ABOUT the helper is not a call to it. A body that explained in a comment why it does not use
	// refuseIfSuspended counted as covered by it, which is the failure this scan exists to prevent.
	refuses := regexp.MustCompile(`\brefuseIfSuspended(With)?\(`)
	code := func(name string) string { return stripGoComments(bodies[name]) }
	covered := func(name string) bool {
		b := code(name)
		if strings.Contains(b, "s.step(") || refuses.MatchString(b) {
			return true
		}
		for _, m := range call.FindAllStringSubmatch(b, -1) {
			if refuses.MatchString(code(m[1])) {
				return true
			}
		}
		return false
	}
	var problems []string
	isMethod := map[string]bool{}
	for _, n := range methods {
		isMethod[n] = true
		if _, ok := exempt[n]; !ok && !covered(n) {
			problems = append(problems, n+": neither a dispatcher step, nor calls refuseIfSuspended (directly or through one call), nor named in callsNeedingNoPostSuspendRefusal")
		}
	}
	for n := range exempt {
		switch {
		case !isMethod[n]:
			problems = append(problems, n+": named in callsNeedingNoPostSuspendRefusal but is not a HostHandler method")
		case covered(n):
			problems = append(problems, n+": named in callsNeedingNoPostSuspendRefusal but is covered, so the entry describes nothing")
		}
	}
	sort.Strings(problems)
	return problems
}

func hostHandlerMethodBodies(t *testing.T) ([]string, map[string]string) {
	t.Helper()
	ht := reflect.TypeOf((*HostHandler)(nil)).Elem()
	var methods []string
	for i := 0; i < ht.NumMethod(); i++ {
		methods = append(methods, ht.Method(i).Name)
	}
	decl := regexp.MustCompile(`(?m)^func \(s \*execSession\) ([A-Za-z_]\w*)\(`)
	bodies := map[string]string{}
	for _, src := range engineSources(t) {
		locs := decl.FindAllStringSubmatchIndex(src, -1)
		for i, loc := range locs {
			end := len(src)
			if i+1 < len(locs) {
				end = locs[i+1][0]
			}
			bodies[src[loc[2]:loc[3]]] = src[loc[0]:end]
		}
	}
	return methods, bodies
}

func TestEveryHostCallRefusesAfterASuspensionOrIsNamed(t *testing.T) {
	methods, bodies := hostHandlerMethodBodies(t)
	if len(methods) < 40 {
		t.Fatalf("read %d HostHandler methods; the scan measured nothing", len(methods))
	}
	if p := postSuspendCoverageProblemsIn(methods, bodies, callsNeedingNoPostSuspendRefusal); len(p) > 0 {
		t.Fatalf("%d host call(s) can start fresh work after the execution has suspended (cleat#4285):\n  %s", len(p), strings.Join(p, "\n  "))
	}
}

// TestThePostSuspendRuleCanFail is the known-positive: the rule must report a call that has no refusal, an exemption naming nothing, and an exemption
// that is covered, on text where each is true.
func TestThePostSuspendRuleCanFail(t *testing.T) {
	methods := []string{"Covered", "ViaStep", "ViaHelper", "Bare", "Named", "OnlyAComment"}
	bodies := map[string]string{
		"Covered":    "func (s *execSession) Covered() { s.refuseIfSuspended(\"x\", 1) }",
		"ViaStep":    "func (s *execSession) ViaStep() { return s.step(ctx) }",
		"ViaHelper":  "func (s *execSession) ViaHelper() { return s.freshThing() }",
		"freshThing": "func (s *execSession) freshThing() { s.refuseIfSuspended(\"x\", 1) }",
		"Bare":       "func (s *execSession) Bare() { s.recordEvent(rec) }",
		"Named":      "func (s *execSession) Named() { }",
		// A comment that NAMES the helper (and a block comment that does) is a sentence about it, not a call to it.
		"OnlyAComment": "func (s *execSession) OnlyAComment() {\n\t// kept apart from refuseIfSuspended(\"x\", 1) on purpose\n\t/* refuseIfSuspendedWith(\"x\", f) */\n\ts.recordEvent(rec)\n}",
	}
	if p := postSuspendCoverageProblemsIn(methods, bodies, map[string]string{"Named": "reads only"}); len(p) != 2 || !strings.HasPrefix(p[0], "Bare:") ||
		!strings.HasPrefix(p[1], "OnlyAComment:") {
		t.Fatalf("want exactly Bare and OnlyAComment reported (a comment naming the helper is not a call), got %v", p)
	}
	if p := postSuspendCoverageProblemsIn(methods, bodies, map[string]string{"Named": "r", "Bare": "r", "OnlyAComment": "r", "Ghost": "r", "Covered": "r"}); len(p) != 2 ||
		!strings.HasPrefix(p[0], "Covered:") || !strings.HasPrefix(p[1], "Ghost:") {
		t.Fatalf("want Covered and Ghost reported as exemptions that describe nothing, got %v", p)
	}
}

func TestThePostSuspendExemptionsStillDescribeSomething(t *testing.T) {
	methods, bodies := hostHandlerMethodBodies(t)
	for _, p := range postSuspendCoverageProblemsIn(methods, bodies, callsNeedingNoPostSuspendRefusal) {
		if strings.Contains(p, "describes nothing") || strings.Contains(p, "is not a HostHandler method") {
			t.Errorf("%s", p)
		}
	}
}
