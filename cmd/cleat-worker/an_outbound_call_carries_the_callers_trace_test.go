package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

// cleat#1596: cleat joined a caller's trace inbound and sent nothing outbound,
// so the chain broke at the first hop and nothing looked wrong.
//
// THIS TEST SPANS BOTH HALVES ON PURPOSE. A unit test of SetTraceparent can be
// perfectly green while nothing calls it -- that is how an export ships to
// three SDKs with no caller. So the assertion here is made on the header a real
// server RECEIVED from a real fetch, not on what the helper returns.
//
// The trace-id is a valid W3C one throughout: 32 lowercase hex digits. Using a
// placeholder like "trace-1" would make the test pass for the wrong reason,
// because SetTraceparent correctly refuses to emit a malformed id and the
// "absent" and "refused" cases look identical at the server.
const testTraceID = "4bf92f3577b34da6a3ce929d0e0e4736"

var traceparentRE = regexp.MustCompile(`^00-([0-9a-f]{32})-([0-9a-f]{16})-0[01]$`)

func TestAnOutboundCallCarriesTheCallersTrace(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("traceparent")
		w.WriteHeader(200)
	}))
	defer srv.Close()

	c := &dbServiceCaller{
		egress:  &engine.EgressGuard{AllowLoopback: true},
		traceID: testTraceID,
	}
	req, _ := json.Marshal(map[string]string{"url": srv.URL, "method": "GET"})
	if _, err := c.handleHTTPFetch(context.Background(), string(req)); err != nil {
		t.Fatalf("fetch: %v", err)
	}

	if got == "" {
		t.Fatal("the callee received NO traceparent, so it starts a new trace and the chain " +
			"breaks here -- cleat appears in a collector as a leaf that swallowed everything " +
			"downstream (cleat#1596)")
	}
	m := traceparentRE.FindStringSubmatch(got)
	if m == nil {
		t.Fatalf("traceparent %q is not well formed; a collector will discard it, which is "+
			"indistinguishable from sending nothing", got)
	}
	if m[1] != testTraceID {
		t.Errorf("propagated trace-id %q, want %q -- a DIFFERENT trace id is the exact defect: "+
			"both ends look healthy and the chain is still broken", m[1], testTraceID)
	}
	if m[2] == "0000000000000000" {
		t.Error("span-id is all zero, which the W3C spec rejects")
	}
}

// A guest that sets its own traceparent has said something deliberate about its
// trace. Overwriting it is the same broken chain by a different route.
func TestAGuestsOwnTraceparentIsNotOverwritten(t *testing.T) {
	const guestTP = "00-11111111111111111111111111111111-2222222222222222-01"
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("traceparent")
		w.WriteHeader(200)
	}))
	defer srv.Close()

	c := &dbServiceCaller{
		egress:  &engine.EgressGuard{AllowLoopback: true},
		traceID: testTraceID,
	}
	req, _ := json.Marshal(map[string]any{
		"url": srv.URL, "method": "GET",
		"headers": map[string]string{"traceparent": guestTP},
	})
	if _, err := c.handleHTTPFetch(context.Background(), string(req)); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if got != guestTP {
		t.Errorf("the guest's traceparent was replaced: got %q, want %q", got, guestTP)
	}
}

// A run with no trace sends no header. THE CONTROL: without this, a version
// that emits a fabricated trace-id for every call passes the test above --
// and a fabricated trace is worse than none, because a collector believes it.
func TestARunWithNoTraceSendsNoTraceparent(t *testing.T) {
	for _, tc := range []struct{ name, traceID string }{
		{"absent", ""},
		{"not hex", "zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"},
		{"wrong length", "4bf92f3577b34da6"},
		{"all zero, which the spec rejects", "00000000000000000000000000000000"},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			var got string
			var seen bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got, seen = r.Header.Get("traceparent"), true
				w.WriteHeader(200)
			}))
			defer srv.Close()

			c := &dbServiceCaller{
				egress:  &engine.EgressGuard{AllowLoopback: true},
				traceID: tc.traceID,
			}
			req, _ := json.Marshal(map[string]string{"url": srv.URL, "method": "GET"})
			if _, err := c.handleHTTPFetch(context.Background(), string(req)); err != nil {
				t.Fatalf("fetch: %v", err)
			}
			// UNMEASURED guard: if the request never arrived, "no header" is
			// vacuously true and this case proves nothing.
			if !seen {
				t.Fatal("the server was never reached, so this case measured nothing")
			}
			if got != "" {
				t.Errorf("sent traceparent %q for trace-id %q -- inventing a trace is worse than "+
					"sending none, because a collector believes it and shows an operator a tree "+
					"that never existed", got, tc.traceID)
			}
		})
	}
}

// THE ONE CASE WHERE THE INJECTION'S PLACEMENT MATTERS, and it is here because
// the comment at the call site originally claimed something broader that is not
// true.
//
// That comment said injecting before the guest's header loop "would let the
// loop silently overwrite ours". Trace it: if the guest sets a traceparent, the
// loop overwrites ours with the GUEST'S deliberate value, which is the outcome
// we want either way. Moving the call before the loop and running these tests
// passes -- measured, not reasoned.
//
// The placement is load-bearing for exactly one input: a guest that supplies
// traceparent as an EMPTY STRING. Injecting after, SetTraceparent sees no
// usable value and supplies the run's trace; injecting before, the loop
// replaces ours with the empty string and the hop is silent again. Narrow, real,
// and now pinned rather than described.
func TestAnEmptyGuestTraceparentDoesNotSilenceTheHop(t *testing.T) {
	var got string
	var seen bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, seen = r.Header.Get("traceparent"), true
		w.WriteHeader(200)
	}))
	defer srv.Close()

	c := &dbServiceCaller{
		egress:  &engine.EgressGuard{AllowLoopback: true},
		traceID: testTraceID,
	}
	req, _ := json.Marshal(map[string]any{
		"url": srv.URL, "method": "GET",
		"headers": map[string]string{"traceparent": ""},
	})
	if _, err := c.handleHTTPFetch(context.Background(), string(req)); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if !seen {
		t.Fatal("the server was never reached, so this case measured nothing")
	}
	m := traceparentRE.FindStringSubmatch(got)
	if m == nil || m[1] != testTraceID {
		t.Errorf("an empty guest traceparent silenced the hop: got %q, want this run's trace %q. "+
			"The injection must run AFTER the guest's header loop, or the loop's empty value wins.",
			got, testTraceID)
	}
}
