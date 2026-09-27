package engine

import (
	"context"
	"testing"
)

// The replay frontier invariant.
//
// A session resumed from history starts with isReplay set. Each host call the
// workflow makes either matches a recorded event -- in which case it consumes
// it and stays in replay -- or is past the end of that history, in which case
// it is NEW WORK and the session must leave replay and do it.
//
// A call that does neither returns success having done nothing, and the
// workflow completes with a history that says the work happened. That is not a
// theoretical failure mode; it was three of them:
//
//	DurableSend                 returned 0 with no event and no dispatch
//	DurableScheduleInvoke       the same
//	PluginCallStreaming         returned success with a marshalled `null`
//	                            stream the plugin was never asked to produce
//
// All three were reached by any workflow that suspends and then does one of
// those things -- and by EVERY defer, since a defer body runs at the end by
// definition and so is always past the frontier. Measured 2026-09-06: a
// workflow that slept and then sent reached the service 0 times in 3 runs; the
// same send before the sleep arrived every time.
//
// Each of the three had a correct sibling a few lines away -- DurableDefer for
// the first two, replayPluginCall for the third -- which is why this is a test
// about the shape and not three tests about three functions.
//
// The check is `isReplay` rather than "an event was recorded", because the
// correct behaviour for some calls past the frontier is to do fresh work that
// records nothing durable. Leaving replay is the part they all share.

// replayFrontierCase is one host call driven past the end of history.
type replayFrontierCase struct {
	name string
	call func(*execSession) // must invoke exactly one host-call method
}

// callsThatStayInReplay are the host calls that legitimately return during
// replay without leaving it, with the reason each is not the defect above.
//
// A list that may only SHRINK. An entry added to silence a failure is an
// entry that hides the next instance of a defect this repository has now had
// three of at once.
var callsThatStayInReplay = map[string]string{
	"PollCancellation": "not a durable event: it consumes no history and " +
		"records none, so it has no frontier to be past. Returning " +
		"'not cancelled' during replay is deliberate -- see the call site.",
	"DurableLog": "explicitly non-durable: no event recorded, no replay matching.",
}

func TestEveryHostCallLeavesReplayPastTheEndOfHistory(t *testing.T) {
	ctx := context.Background()

	cases := []replayFrontierCase{
		{"DurableCall", func(s *execSession) { s.DurableCall(ctx, nil, "svc", "op", `{}`, 0, 0) }},
		{"DurableSend", func(s *execSession) { s.DurableSend(ctx, nil, "svc", "op", `{}`) }},
		{"DurableScheduleInvoke", func(s *execSession) { s.DurableScheduleInvoke(ctx, nil, "svc", "op", `{}`, 10) }},
		{"PluginCall", func(s *execSession) { s.PluginCall(ctx, nil, "plug", "fn", `{}`, 0, 0) }},
		{"PluginCallStreaming", func(s *execSession) { s.PluginCallStreaming(ctx, nil, "plug", "fn", `{}`, 0, 0) }},
	}

	for _, tc := range cases {
		// Exempt calls are absent from `cases` rather than skipped inside the
		// loop. A skipped subtest reports as neither pass nor fail, so an
		// exemption added by mistake would be invisible in the run -- and the
		// repository's skip ledger exists because that has happened. What
		// keeps the two lists honest is TestTheExemptListStillDescribesSomething
		// below, which drives every exempt call and fails if one stops being
		// exempt.
		if _, exempt := callsThatStayInReplay[tc.name]; exempt {
			t.Fatalf("%q is both in the cases table and in callsThatStayInReplay; "+
				"it must be in exactly one", tc.name)
		}
		t.Run(tc.name, func(t *testing.T) {
			// A session in replay whose history is EMPTY: every call is past
			// the frontier at once, which is the state a resumed workflow
			// reaches as soon as it runs past what it recorded last time.
			s := &execSession{
				engine:   NewEngine(nil, &mockCaller{}),
				isReplay: true,
				history:  nil,
			}
			tc.call(s)

			if s.isReplay {
				t.Errorf("%s returned with the session still in replay and no history to consume.\n"+
					"Past the end of history a host call is new work: it must call s.exitReplay() "+
					"and take its fresh path, the way DurableDefer and replayPluginCall do. "+
					"Returning here reports success for work that never happened.", tc.name)
			}
		})
	}
}

// TestTheExemptListStillDescribesSomething is the second direction. An
// exemption for a call that is no longer exempt -- or no longer exists -- is a
// standing grant for whatever function next takes that name.
func TestTheExemptListStillDescribesSomething(t *testing.T) {
	ctx := context.Background()

	exemptCalls := map[string]func(*execSession){
		"PollCancellation": func(s *execSession) { s.PollCancellation(ctx, nil, 0, 0) },
		"DurableLog":       func(s *execSession) { s.DurableLog(ctx, nil, "message") },
	}

	for name := range callsThatStayInReplay {
		call, known := exemptCalls[name]
		if !known {
			t.Errorf("%q is exempt in callsThatStayInReplay but this test cannot call it, "+
				"so the exemption covers nothing checkable. Add it to exemptCalls or delete "+
				"the entry.", name)
			continue
		}
		s := &execSession{
			engine:   NewEngine(nil, &mockCaller{}),
			isReplay: true,
			history:  nil,
		}
		call(s)
		if !s.isReplay {
			t.Errorf("%q is listed as staying in replay, but it left replay. The exemption "+
				"no longer describes anything -- delete it from callsThatStayInReplay so "+
				"the call is covered by the test above.", name)
		}
	}
}
