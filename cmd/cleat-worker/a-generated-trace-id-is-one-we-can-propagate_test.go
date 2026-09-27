package main

import (
	"net/http"
	"testing"

	"github.com/cleat-team/cleat/plugin"
)

// The worker manufactures a trace-id for any run that arrives without one --
// a scheduled run, a cron fire, an API start with no inbound traceparent:
//
//	traceID := wf.TraceID
//	if traceID == "" { traceID = generateTraceID() }
//
// and plugin.SetTraceparent REFUSES anything that is not 32 lowercase hex
// digits, non-zero. Those are two functions in two packages with nothing
// between them, and the failure mode if they ever disagree is silent: every
// originated run would propagate NO header, which is indistinguishable from a
// run that legitimately has no trace.
//
// So this pins the join. It is the cheapest possible test and it exists because
// the alternative is discovering, from a customer's collector, that traces work
// for API-started runs and not for scheduled ones. cleat#1611.
//
// A change to EITHER side fails here: switch generateTraceID to a UUID (dashes,
// 36 chars) and it goes red; tighten the validator and it goes red.
func TestAGeneratedTraceIDIsOneWeCanPropagate(t *testing.T) {
	// Several, not one: a single sample cannot show that the property holds for
	// every value the generator can produce. A generator emitting uppercase hex
	// half the time would pass a one-shot test half the time, which is worse
	// than failing.
	for i := 0; i < 64; i++ {
		id := generateTraceID()
		req, err := http.NewRequest("GET", "https://example.com", nil)
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		plugin.SetTraceparent(req, id)
		if got := req.Header.Get("traceparent"); got == "" {
			t.Fatalf("generateTraceID() returned %q, which plugin.SetTraceparent refuses.\n\n"+
				"Every run that arrives without an inbound trace -- scheduled runs, cron fires, "+
				"API starts with no traceparent -- gets its id from this generator. If the "+
				"propagator will not accept it, those runs silently send no header at all and "+
				"their traces end at cleat, while API-started runs work fine. cleat#1611.", id)
		}
	}
}
