package engine

// cleat#4143. A host call that leaves replay (exitReplay) and goes on to do new work is a HAND-OFF, and in a defer segment the body's
// first new call arrives through it: the segment starts in replay whenever the run has history. Every hand-off therefore has to reach
// the segment's refusal (stopBeforeNewWork), or be named here with the reason it need not.
//
// Six entry points had no refusal on this path and a terminated workflow's segment made the body's next call (see
// a_defer_segment_refuses_new_work_at_the_replay_frontier_test.go). The inventories keyed on stop sites could not have found them: they
// list the functions that DO contain a check, and a hand-off with none is the one thing a list of those cannot contain.
//
// WHAT COUNTS AS GUARDED. A function that calls exitReplay() is guarded when its own body calls stopBeforeNewWork() or
// suspendingAwaitResult(), or when it calls a s.fresh*() method whose body does. The call is read as CODE (callsInCode), so a comment
// quoting the name does not satisfy it.
//
// SCOPE. The first scan is an inventory of the exitReplay call sites, so it does not see a hand-off that leaves replay some other way.
//
// A SECOND KIND OF ENTRY ESCAPES IT, AND DID (cleat#4143, the lookup). A LOOKUP answers "is this already recorded?" and never leaves
// replay itself: at the frontier it answers "no", and the CALLER goes on to do the work. SideEffectRecorded is the case: it decides
// whether the user's function runs, answered "not recorded" at the frontier with no stop check, and so contained no exitReplay() for the
// first scan to find. The second scan reads the other shape of the same hole: a function that SPELLS the frontier predicate
// (s.stepCount >= len(s.history)) or ANSWERS with a *NotRecorded status must reach a refusal too, or be named in
// frontierAnswersThatNeedNoRefusal.
//
// WHAT THE SECOND SCAN CANNOT SEE: a lookup that finds the frontier another way (a `<` test that falls through to a bare return) and
// answers "no" with some other name. Naming a new "not recorded" status ending in NotRecorded is what makes it visible.
//
// Run the known positives below before trusting a pass: they show each scan reports an entry with no check.

import (
	goscan "go/scanner"
	"go/token"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// handOffsThatNeedNoRefusal are functions that call exitReplay() and are not stopped, each with the reason. A list that may only
// SHRINK: an entry added to silence a failure is the next unguarded call site. TestEveryHandOffExemptionStillDescribesSomething
// fails if one stops being true.
var handOffsThatNeedNoRefusal = map[string]string{
	"leaveReplay": "the dispatcher's one exit from replay (cleat#4152). Its only caller is step, which applies stopBeforeNewWork to every non-Pure step on the next lines, so the refusal is not skipped by reaching a call this way; that is the property the dispatcher exists to have.",
	"fastForwardToStop": "the mechanism that makes a stop-at-last-stoppable segment refuse: it leaves replay so that the guest's NEXT call " +
		"takes the non-replay half, where the refusal is.",
	"AwaitChild": "a read of an existing child's result, recorded as one await event. It performs nothing outside the workflow. Whether a " +
		"segment's body should be stopped at a child wait is a separate decision (see the PR for cleat#4143); until it is made " +
		"these three child waits are not stop sites, which stopSiteNotStoppable records.",
	"AwaitAnyChild":          "see AwaitChild.",
	"replayAwaitAllChildren": "see AwaitChild.",
	"ListCrons":              "a read of the schedule table, recorded as one event. It performs nothing outside the workflow.",
	"RegisterUpdateHandler":  "registers a handler in the session and records the registration. It performs nothing outside the workflow.",
}

var (
	handOffDecl = regexp.MustCompile(`(?m)^func \(s \*execSession\) ([A-Za-z_]\w*)\(`)
	freshCallee = regexp.MustCompile(`\bs\.(fresh\w+)\(`)
)

// unrefusedHandOffs returns the functions in src (name -> body) that call exitReplay() and reach no refusal, plus how many hand-offs it
// examined. It is a function of the text so a test can hand it a known positive.
func unrefusedHandOffs(bodies map[string]string) (unrefused []string, examined int) {
	refuses := func(body string) bool {
		return callsInCode(body, "stopBeforeNewWork") || callsInCode(body, "suspendingAwaitResult")
	}
	for name, body := range bodies {
		if name == "exitReplay" || !callsInCode(body, "exitReplay") {
			continue
		}
		examined++
		if refuses(body) {
			continue
		}
		reached := false
		for _, m := range freshCallee.FindAllStringSubmatch(body, -1) {
			if b, ok := bodies[m[1]]; ok && refuses(b) {
				reached = true
			}
		}
		if !reached {
			unrefused = append(unrefused, name)
		}
	}
	sort.Strings(unrefused)
	return unrefused, examined
}

// frontierAnswersThatNeedNoRefusal are functions that read the replay frontier or answer "not recorded" and need no refusal of their own,
// each with the reason. A list that may only SHRINK, like handOffsThatNeedNoRefusal.
var frontierAnswersThatNeedNoRefusal = map[string]string{}

// blankComments returns src with every comment replaced by spaces, offsets and newlines untouched, so a comment that quotes a predicate, a
// status name or a call cannot satisfy or trigger a scan that matches on text.
func blankComments(src string) string {
	fset := token.NewFileSet()
	file := fset.AddFile("", fset.Base(), len(src))
	var sc goscan.Scanner
	sc.Init(file, []byte(src), func(token.Position, string) {}, goscan.ScanComments)
	out := []byte(src)
	for {
		pos, tok, lit := sc.Scan()
		if tok == token.EOF {
			return string(out)
		}
		if tok != token.COMMENT {
			continue
		}
		off := file.Offset(pos)
		for i := off; i < off+len(lit) && i < len(out); i++ {
			if out[i] != '\n' {
				out[i] = ' '
			}
		}
	}
}

var (
	notRecordedStatus = regexp.MustCompile(`\b\w*NotRecorded\b`)
	frontierBranch    = regexp.MustCompile(`s\.stepCount\s*>=\s*len\(s\.history\)\s*\{`)
)

// frontierBlocks returns the text of every `if s.stepCount >= len(s.history) { ... }` block in body's code, braces matched.
func frontierBlocks(body string) []string {
	code := blankComments(body)
	var out []string
	for _, loc := range frontierBranch.FindAllStringIndex(code, -1) {
		depth, end := 1, -1
		for i := loc[1]; i < len(code); i++ {
			switch code[i] {
			case '{':
				depth++
			case '}':
				depth--
			}
			if depth == 0 {
				end = i
				break
			}
		}
		if end < 0 {
			end = len(code)
		}
		out = append(out, code[loc[1]:end])
	}
	return out
}

// answersAtTheFrontier reports whether a function's CODE reads the replay frontier predicate or names a *NotRecorded status.
func answersAtTheFrontier(body string) bool {
	return len(frontierBlocks(body)) > 0 || notRecordedStatus.MatchString(blankComments(body))
}

// unrefusedFrontierAnswers returns the functions that answer at the replay frontier and reach no refusal, plus how many it examined. It is
// a function of the text so a test can hand it a known positive.
//
// THE REFUSAL MUST BE IN THE FRONTIER BRANCH, not just somewhere in the function. SideEffectRecorded already refuses on its non-replay half, so a
// function-wide check is satisfied by the layer that was never the problem: remove the frontier refusal and the scan would stay green. For a
// function that spells the predicate, every such branch must itself call stopBeforeNewWork() (or suspendingAwaitResult()) or hand off to a
// s.fresh*() callee that does. A function that only answers a *NotRecorded status, with no predicate to locate, is checked as a whole.
func unrefusedFrontierAnswers(bodies map[string]string) (unrefused []string, examined int) {
	refuses := func(text string) bool {
		return callsInCode(text, "stopBeforeNewWork") || callsInCode(text, "suspendingAwaitResult")
	}
	reaches := func(text string) bool {
		if refuses(text) {
			return true
		}
		for _, m := range freshCallee.FindAllStringSubmatch(text, -1) {
			if b, ok := bodies[m[1]]; ok && refuses(b) {
				return true
			}
		}
		return false
	}
	for name, body := range bodies {
		if !answersAtTheFrontier(body) {
			continue
		}
		examined++
		ok := true
		if blocks := frontierBlocks(body); len(blocks) > 0 {
			for _, blk := range blocks {
				if !reaches(blk) {
					ok = false
				}
			}
		} else {
			ok = reaches(body)
		}
		if !ok {
			unrefused = append(unrefused, name)
		}
	}
	sort.Strings(unrefused)
	return unrefused, examined
}

// engineMethodBodies reads every non-test engine/*.go file and returns each *execSession method's text, name -> body.
func engineMethodBodies(t *testing.T) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading engine/: %v", err)
	}
	out := map[string]string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		b, err := os.ReadFile(e.Name())
		if err != nil {
			t.Fatalf("reading %s: %v", e.Name(), err)
		}
		src := string(b)
		locs := handOffDecl.FindAllStringSubmatchIndex(src, -1)
		for i, loc := range locs {
			end := len(src)
			if i+1 < len(locs) {
				end = locs[i+1][0]
			}
			out[src[loc[2]:loc[3]]] = src[loc[0]:end]
		}
	}
	return out
}

func TestEveryReplayToFreshHandOffReachesARefusalOrIsNamed(t *testing.T) {
	bodies := engineMethodBodies(t)
	unrefused, examined := unrefusedHandOffs(bodies)

	// Vacuity, per thing rather than per total: a floor alone is satisfied by the wrong things being present.
	if examined < 20 {
		t.Fatalf("the scan found %d functions that call exitReplay(); it is reading the wrong thing and would pass whatever the tree said", examined)
	}
	for _, canary := range []string{"replayCall", "replaySetScope", "replaySideEffect"} {
		if !callsInCode(bodies[canary], "exitReplay") {
			t.Fatalf("canary %s was not seen calling exitReplay(); the scan is reading the wrong thing", canary)
		}
	}

	var bad []string
	for _, name := range unrefused {
		if _, ok := handOffsThatNeedNoRefusal[name]; !ok {
			bad = append(bad, name)
		}
	}
	if len(bad) > 0 {
		t.Errorf("function(s) %s call exitReplay() and reach no stopBeforeNewWork(), directly or through a s.fresh*() callee, and are not in "+
			"handOffsThatNeedNoRefusal.\n\nA defer segment over a run with history starts in replay, so its body's first new call arrives "+
			"through this hand-off and nothing refuses it (cleat#4143). Put the check at the top of the fresh* function the hand-off "+
			"reaches, as freshCall does, or name the function here with the reason it needs none.", strings.Join(bad, ", "))
	}
}

// TestEveryHandOffExemptionStillDescribesSomething is the second direction. An exemption for a function that no longer exists, no longer
// calls exitReplay(), or is now refused is a standing grant for whatever next takes that name.
func TestEveryHandOffExemptionStillDescribesSomething(t *testing.T) {
	bodies := engineMethodBodies(t)
	unrefused, _ := unrefusedHandOffs(bodies)
	isUnrefused := map[string]bool{}
	for _, n := range unrefused {
		isUnrefused[n] = true
	}
	for name := range handOffsThatNeedNoRefusal {
		body, ok := bodies[name]
		switch {
		case !ok:
			t.Errorf("%q is exempt but no such *execSession method exists; delete the entry", name)
		case !callsInCode(body, "exitReplay"):
			t.Errorf("%q is exempt but no longer calls exitReplay(); delete the entry", name)
		case !isUnrefused[name]:
			t.Errorf("%q is exempt but now reaches a refusal; the exemption no longer describes anything, so delete the entry", name)
		}
	}
}

// TestTheHandOffScanReportsAHandOffWithNoRefusal is the known positive. A scan that returns nothing for every input also "passes" the
// tree, so show it reporting one hand-off that has no check, and not reporting the same hand-off once the check is added through a
// fresh* callee or in the body itself, and not when the check is only a COMMENT.
func TestTheHandOffScanReportsAHandOffWithNoRefusal(t *testing.T) {
	const noCheck = "func (s *execSession) replayX() int64 {\n\ts.exitReplay()\n\treturn s.freshX()\n}\n"
	cases := []struct {
		name   string
		bodies map[string]string
		want   []string
	}{
		{"no check anywhere", map[string]string{
			"replayX": noCheck,
			"freshX":  "func (s *execSession) freshX() int64 {\n\treturn 1\n}\n",
		}, []string{"replayX"}},
		{"the check is only a comment", map[string]string{
			"replayX": noCheck,
			"freshX":  "func (s *execSession) freshX() int64 {\n\t// if s.stopBeforeNewWork() { return 1 }\n\treturn 1\n}\n",
		}, []string{"replayX"}},
		{"the check is in the fresh callee", map[string]string{
			"replayX": noCheck,
			"freshX":  "func (s *execSession) freshX() int64 {\n\tif s.stopBeforeNewWork() {\n\t\treturn 1\n\t}\n\treturn 2\n}\n",
		}, nil},
		{"the check is in the hand-off itself", map[string]string{
			"replayX": "func (s *execSession) replayX() int64 {\n\ts.exitReplay()\n\tif s.stopBeforeNewWork() {\n\t\treturn 1\n\t}\n\treturn 2\n}\n",
		}, nil},
	}
	for _, c := range cases {
		got, _ := unrefusedHandOffs(c.bodies)
		if strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("%s: scan reported %v, want %v", c.name, got, c.want)
		}
	}
}

func TestEveryFrontierAnswerReachesARefusalOrIsNamed(t *testing.T) {
	bodies := engineMethodBodies(t)
	unrefused, examined := unrefusedFrontierAnswers(bodies)

	// Vacuity per thing: the lookup that escaped the first scan, and a function that spells the predicate and DOES refuse.
	for _, canary := range []string{"SideEffectRecorded", "replayPluginCallStreaming"} {
		if !answersAtTheFrontier(bodies[canary]) {
			t.Fatalf("canary %s was not seen answering at the frontier; the scan is reading the wrong thing and would pass whatever the tree said", canary)
		}
	}
	if examined < 2 {
		t.Fatalf("the scan found %d frontier answers, want at least the two canaries", examined)
	}

	var bad []string
	for _, name := range unrefused {
		if _, ok := frontierAnswersThatNeedNoRefusal[name]; !ok {
			bad = append(bad, name)
		}
	}
	if len(bad) > 0 {
		t.Errorf("function(s) %s read the replay frontier or answer \"not recorded\" and reach no stopBeforeNewWork(), directly or through a "+
			"s.fresh*() callee, and are not in frontierAnswersThatNeedNoRefusal.\n\nA defer segment over a run with history starts in replay, so its "+
			"body's first new work arrives at this answer. A LOOKUP that says \"not recorded\" starts the caller's work, and refusing only the "+
			"record after it is too late (cleat#4143: SideEffectRecorded let the user's function run).", strings.Join(bad, ", "))
	}
}

func TestEveryFrontierAnswerExemptionStillDescribesSomething(t *testing.T) {
	bodies := engineMethodBodies(t)
	unrefused, _ := unrefusedFrontierAnswers(bodies)
	isUnrefused := map[string]bool{}
	for _, n := range unrefused {
		isUnrefused[n] = true
	}
	for name := range frontierAnswersThatNeedNoRefusal {
		body, ok := bodies[name]
		switch {
		case !ok:
			t.Errorf("%q is exempt but no such *execSession method exists; delete the entry", name)
		case !answersAtTheFrontier(body):
			t.Errorf("%q is exempt but no longer answers at the frontier; delete the entry", name)
		case !isUnrefused[name]:
			t.Errorf("%q is exempt but now reaches a refusal; the exemption no longer describes anything, so delete the entry", name)
		}
	}
}

// TestTheFrontierAnswerScanReportsALookupWithNoRefusal is the known positive for the second scan, shaped like the SideEffectRecorded that
// escaped: no exitReplay anywhere, a frontier test, and a "not recorded" answer.
func TestTheFrontierAnswerScanReportsALookupWithNoRefusal(t *testing.T) {
	const lookup = "func (s *execSession) lookupX() int64 {\n\tif s.stepCount >= len(s.history) {\n%s\t\treturn packSimpleResult(xNotRecorded)\n\t}\n\treturn 0\n}\n"
	cases := []struct {
		name   string
		bodies map[string]string
		want   []string
	}{
		{"a lookup with no check", map[string]string{"lookupX": strings.Replace(lookup, "%s", "", 1)}, []string{"lookupX"}},
		{"the check is only a comment", map[string]string{"lookupX": strings.Replace(lookup, "%s", "\t\t// if s.stopBeforeNewWork() { return 1 }\n", 1)}, []string{"lookupX"}},
		{"a refusal on the non-replay half only does not count", map[string]string{"lookupX": "func (s *execSession) lookupX() int64 {\n\tif !s.isReplay {\n\t\tif s.stopBeforeNewWork() {\n\t\t\treturn 1\n\t\t}\n\t\treturn 0\n\t}\n\tif s.stepCount >= len(s.history) {\n\t\treturn packSimpleResult(xNotRecorded)\n\t}\n\treturn 0\n}\n"}, []string{"lookupX"}},
		{"the check is in the lookup", map[string]string{"lookupX": strings.Replace(lookup, "%s", "\t\tif s.stopBeforeNewWork() {\n\t\t\treturn 1\n\t\t}\n", 1)}, nil},
		{"a comment quoting the predicate is not a frontier answer", map[string]string{
			"quotesIt": "func (s *execSession) quotesIt() int64 {\n\t// when s.stepCount >= len(s.history) the lookup answers xNotRecorded\n\treturn 0\n}\n"}, nil},
		{"a frontier answer that hands off to a refusing fresh function", map[string]string{
			"replayY": "func (s *execSession) replayY() int64 {\n\tif s.stepCount >= len(s.history) {\n\t\treturn s.freshY()\n\t}\n\treturn 0\n}\n",
			"freshY":  "func (s *execSession) freshY() int64 {\n\tif s.stopBeforeNewWork() {\n\t\treturn 1\n\t}\n\treturn 2\n}\n"}, nil},
	}
	for _, c := range cases {
		got, _ := unrefusedFrontierAnswers(c.bodies)
		if strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("%s: scan reported %v, want %v", c.name, got, c.want)
		}
	}
}
