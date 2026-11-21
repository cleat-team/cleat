//go:build chaos

package chaos

// The reclaim oracle at a fire-and-forget send's own boundary (cleat#4241, found by cleat-review3 on d9fa5afa).
//
// A send is dispatched on a goroutine, CONCURRENTLY with the body. With the send held at a boundary the body has already gone on, so by the time
// the kill lands it may have PERFORMED the next synchronous call, whose event is not yet durable; the reclaim then repeats that call. That is the
// ordinary at-least-once window, for the call the kill actually interrupted. verifyReclaim granted the window only to the faulted op (here the send,
// which is exempted separately), so on a loaded machine two cases failed, sigkill@after:send_S and sigkill@after:send_T, with "B succeeded 2 times"
// and the same for C. The sends themselves stayed at most once in both. On a quiet machine the kill lands first and the window never opens, which is
// why the first runs here were green.
//
// These tests drive verifyReclaim directly with the request sequences cleat-review3 measured, because the real window is milliseconds wide and a
// test that needs it to open is a test that passes when the machine is quiet. They need no database and no worker.
//
// What the allowance is and is not: at most ONE call, after the send, in script order. The body is sequential, so only one call can be performed and
// not yet recorded when a kill lands. Everything else stays exact: a call BEFORE the send repeated, two calls repeated, a call three times, or a send
// repeated are all still violations, and a fault on a call (not a send) grants nothing to the calls after it.

import (
	"strings"
	"testing"
)

func sendScenario(t *testing.T) scenario {
	t.Helper()
	for _, sc := range fireAndForgetScenarios {
		if sc.Name == "sends" {
			return sc
		}
	}
	t.Fatal("PRECONDITION FAILED: the sends scenario is not in fireAndForgetScenarios, so this test is not about it")
	return scenario{}
}

// reqs builds a request list from op names in arrival order; Nth counts per op.
func reqs(ops ...string) []request {
	n := map[string]int{}
	var out []request
	for _, op := range ops {
		n[op]++
		out = append(out, request{Op: op, Nth: n[op]})
	}
	return out
}

func reclaimVerdict(t *testing.T, faultOp, mode string, observed ...string) []string {
	t.Helper()
	sc := sendScenario(t)
	g := goldenRecord{Status: "done", Reqs: reqs("A", "send_S", "B", "send_T", "C")}
	f := &fault{Op: faultOp, Nth: 1, Mode: mode, Then: "reclaim"}
	b := request{Op: faultOp, Nth: 1}
	o := outcome{Status: "done", Applied: true, Requests: reqs(observed...)}
	return verifyReclaim(sc, g, f, b, o)
}

func TestAReclaimAtASendBoundaryAllowsTheNextCallToRepeatOnceAndNothingElse(t *testing.T) {
	cases := []struct {
		name      string
		faultOp   string
		mode      string
		observed  []string
		wantClean bool
		wantText  string // for a violation: text that says WHICH rule fired, so a case cannot pass on some other violation
	}{
		// CONTROL: nothing repeated.
		{"a quiet run: nothing repeated", "send_S", "after", []string{"A", "send_S", "B", "send_T", "C"}, true, ""},
		// cleat-review3's two measured cases.
		{"sigkill@after:send_S, body had performed B", "send_S", "after", []string{"A", "send_S", "B", "B", "send_T", "C"}, true, ""},
		{"sigkill@after:send_T, body had performed C", "send_T", "after", []string{"A", "send_S", "B", "send_T", "C", "C"}, true, ""},
		{"a before-boundary at the send, body had performed B", "send_S", "before", []string{"A", "send_S", "B", "B", "send_T", "C"}, true, ""},
		// cleat-review3, Python guest: the send was held BEFORE it was performed and killed, so it was LOST (0 successes, which at-most-once allows),
		// while the body had already run ahead and performed B unrecorded. The allowance keys on the faulted op being a send at EITHER boundary.
		{"sigkill@before:send_S, the send lost and B repeated", "send_S", "before", []string{"A", "B", "B", "send_T", "C"}, true, ""},
		{"sigkill@before:send_S, the send lost and B and C both repeated", "send_S", "before", []string{"A", "B", "B", "send_T", "C", "C"}, false, "at most ONE call"},
		// The allowance is ONE extra success of ONE call: a call three times earns none (want stays 1).
		{"two calls repeated (B and C)", "send_S", "after", []string{"A", "send_S", "B", "B", "send_T", "C", "C"}, false, "at most ONE call"},
		{"one call three times", "send_S", "after", []string{"A", "send_S", "B", "B", "B", "send_T", "C"}, false, `effect "B" succeeded 3 time(s), want 1`},
		// And only for calls AFTER the send.
		{"a call BEFORE the send repeated", "send_S", "after", []string{"A", "A", "send_S", "B", "send_T", "C"}, false, `effect "A" succeeded 2 time(s), want 1`},
		// A send is never repeated.
		{"the send itself repeated", "send_S", "after", []string{"A", "send_S", "send_S", "B", "send_T", "C"}, false, "a replay must not dispatch a recorded send again"},
		// A fault on a CALL grants nothing to the calls after it: its own window is the faulted op's, as before.
		{"a fault on call B: C repeated", "B", "after", []string{"A", "send_S", "B", "B", "send_T", "C", "C"}, false, `effect "C" succeeded 2 time(s), want 1`},
		{"a fault on call B: B repeated once (its own window)", "B", "after", []string{"A", "send_S", "B", "B", "send_T", "C"}, true, ""},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			v := reclaimVerdict(t, c.faultOp, c.mode, c.observed...)
			joined := strings.Join(v, " | ")
			if c.wantClean {
				if len(v) != 0 {
					t.Fatalf("want no violation, got: %s", joined)
				}
				return
			}
			if len(v) == 0 {
				t.Fatalf("want a violation containing %q, got none", c.wantText)
			}
			if !strings.Contains(joined, c.wantText) {
				t.Fatalf("a violation was reported, but not the rule this case is about.\n  want text: %q\n  got:       %s", c.wantText, joined)
			}
		})
	}
}

// The allowance is for plain calls only. A retried call after a send can show more than one success inside the in-flight window, which "one extra
// success in total" does not describe, so it gets NO allowance and a repeat there is reported: a loud false-red the day such a scenario exists, and
// not a silent acceptance (cleat-review3, #4280).
func TestAReclaimAtASendBoundaryGrantsARetriedCallNothing(t *testing.T) {
	sc := scenario{Name: "send_then_retried_call", Ops: []op{{Op: "call", Name: "A"}, {Op: "send", Name: "S"}, {Op: "call_retry", Name: "R", Attempts: 2}}}
	// CONTROL: the helper names a plain call after the send, so the empty answer below is about the retried call and not about a send it did not find.
	plain := scenario{Name: "send_then_call", Ops: []op{{Op: "send", Name: "S"}, {Op: "call", Name: "B"}}}
	if got := callsAfterSend(plain, "send_S"); !got["B"] {
		t.Fatalf("PRECONDITION FAILED: callsAfterSend did not name the plain call after the send: %v", got)
	}
	if got := callsAfterSend(sc, "send_S"); len(got) != 0 {
		t.Fatalf("a retried call after a send was granted the in-flight allowance: %v", got)
	}
	g := goldenRecord{Status: "done", Reqs: reqs("A", "send_S", "R")}
	f := &fault{Op: "send_S", Nth: 1, Mode: "after", Then: "reclaim"}
	o := outcome{Status: "done", Applied: true, Requests: reqs("A", "send_S", "R", "R")}
	v := strings.Join(verifyReclaim(sc, g, f, request{Op: "send_S", Nth: 1}, o), " | ")
	if !strings.Contains(v, `effect "R" succeeded 2 time(s), want 1`) {
		t.Errorf("a repeated retried call after a send was not reported: %q", v)
	}
}

// A Drive hook that did not act leaves the tables in a state the scenario never expected; that is a failure of the check, and must not be reported
// as a finding about the engine (cleat-review3: the cron editors returned silently on a wait that timed out).
func TestAReclaimRunWhoseDriveDidNotActIsUnmeasuredNotADefect(t *testing.T) {
	var sc scenario
	for _, s := range scenarios {
		if s.Name == "crons" {
			sc = s
		}
	}
	if sc.Name == "" {
		t.Fatal("PRECONDITION FAILED: no scenario named crons")
	}
	// A run that finished, with the schedule tables untouched: cron C is still there (want 0) because the editor never took it away.
	g := goldenRecord{Status: "done", Reqs: reqs("A")}
	f := &fault{Op: "A", Nth: 1, Mode: "after", Then: "reclaim"}
	b := request{Op: "A", Nth: 1}
	stale := map[string]int{"C": 1, "D": 0, "E": 1}

	// CONTROL: with no note, the same tables ARE reported as a violation, so the case below is not green because the tables happened to be right.
	o := outcome{Status: "done", Applied: true, Requests: reqs("A"), Schedules: stale}
	if v := strings.Join(verifyReclaim(sc, g, f, b, o), " | "); !strings.Contains(v, "cron:C: 1 row(s) after the run, want 0") {
		t.Fatalf("PRECONDITION FAILED: the stale tables were not reported without a Drive note, so this case measures nothing: %q", v)
	}

	o.DriveNote = "cron C's row never appeared, so it was not taken away"
	v := strings.Join(verifyReclaim(sc, g, f, b, o), " | ")
	if !strings.Contains(v, "UNMEASURED") {
		t.Errorf("a run whose Drive did not act was not reported UNMEASURED: %q", v)
	}
	if strings.Contains(v, "row(s) after the run") {
		t.Errorf("a table mismatch was reported as a finding although the Drive that sets the tables did not act: %q", v)
	}
	if !strings.Contains(v, "not a finding about the engine") {
		t.Errorf("the UNMEASURED line does not say it is a failure of the check: %q", v)
	}
}
