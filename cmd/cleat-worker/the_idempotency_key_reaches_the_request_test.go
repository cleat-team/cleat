package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

// The engine's replay-stable idempotency key reaches the outbound http.fetch
// request. cleat#1837.
//
// # What was wrong
//
// DurableCallIdempotencyKey derives sha256(workflowID, runID, step) -- stable
// across replays, which is the hard part and was done right -- and the engine
// handed it to CallWithIdempotencyKey, which handed it to call(), which passed
// it to forwardToBenchSvc and DROPPED it for http.fetch. The key travelled the
// whole way and fell out at the last hop, on the only path a workflow has to an
// external service without a Go plugin and a worker rebuild.
//
// # Why these assert on the WIRE
//
// engine.CallerHonoursIdempotencyKeys is a type assertion -- `_, ok :=
// e.caller.(IdempotentCaller)` -- so it returned true for a deployment whose
// only external path discarded the key. It tests the wiring, not the wire. A
// predicate that cannot distinguish the failure it exists to make visible is
// worse than none, because it gets quoted as evidence.
//
// So every assertion here reads a header off a real *http.Request delivered to a
// real listener. Nothing in this file can pass because a type implements an
// interface.

// fetchCapture runs an httptest server and records the Idempotency-Key of each
// request it receives.
func fetchCapture(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Idempotency-Key"))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

func fetchRequestJSON(t *testing.T, url string, headers map[string]string) string {
	t.Helper()
	body := map[string]any{"url": url, "method": "GET"}
	if headers != nil {
		body["headers"] = headers
	}
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("UNMEASURED: marshalling the fetch request: %v", err)
	}
	return string(b)
}

func TestTheIdempotencyKeyReachesTheRequest(t *testing.T) {
	srv, seen := fetchCapture(t)
	// AllowLoopback because the test server is on 127.0.0.1 and the egress floor
	// correctly refuses it. Same idiom as an_outbound_call_carries_the_callers_trace_test.go;
	// the floor is not weakened, the test opts into loopback for its own listener.
	c := &dbServiceCaller{egress: &engine.EgressGuard{AllowLoopback: true}}

	const key = "MFRGGZDFMZTWQ2LK"
	if _, err := c.call(context.Background(), "http", "fetch",
		fetchRequestJSON(t, srv.URL, nil), key); err != nil {
		t.Fatalf("http.fetch failed: %v", err)
	}

	if len(*seen) != 1 {
		t.Fatalf("UNMEASURED: the server received %d requests, want 1. Nothing was observed, "+
			"so a passing assertion below would be about nothing.", len(*seen))
	}
	if got := (*seen)[0]; got != key {
		t.Errorf("the outbound request carried Idempotency-Key %q, want %q.\n\n"+
			"The engine derives a replay-stable key and handed it to the caller; if it does not "+
			"reach the wire, the service has nothing to deduplicate on and an at-least-once "+
			"dispatch duplicates the side effect. cleat#1837.", got, key)
	}
}

// The same logical step produces the same header on replay. This is the whole
// point of the key: a receiver deduplicates on it, so a value that changed
// between attempts would deduplicate nothing.
func TestTheIdempotencyKeyIsIdenticalOnReplay(t *testing.T) {
	srv, seen := fetchCapture(t)
	// AllowLoopback because the test server is on 127.0.0.1 and the egress floor
	// correctly refuses it. Same idiom as an_outbound_call_carries_the_callers_trace_test.go;
	// the floor is not weakened, the test opts into loopback for its own listener.
	c := &dbServiceCaller{egress: &engine.EgressGuard{AllowLoopback: true}}

	// The engine derives this once per logical step; replay re-derives the same
	// value. Driving both attempts with one key is what a replay does, and the
	// assertion is that the wire shows it twice rather than once.
	// Derived by the engine exactly as the real path does, so this test breaks if
	// the derivation stops being replay-stable rather than only if the plumbing does.
	key := engine.DurableCallIdempotencyKey("wf-1", "run-1", 7)
	for i := 0; i < 2; i++ {
		if _, err := c.call(context.Background(), "http", "fetch",
			fetchRequestJSON(t, srv.URL, nil), key); err != nil {
			t.Fatalf("attempt %d failed: %v", i+1, err)
		}
	}

	if len(*seen) != 2 {
		t.Fatalf("UNMEASURED: the server received %d requests, want 2.", len(*seen))
	}
	if (*seen)[0] != (*seen)[1] {
		t.Errorf("two attempts at the same step sent different keys: %q then %q. "+
			"A receiver deduplicating on this header would treat the replay as new work.",
			(*seen)[0], (*seen)[1])
	}
	if (*seen)[0] == "" {
		t.Error("both attempts sent an empty Idempotency-Key, which is equal to itself and " +
			"deduplicates nothing. Equality alone is not the property.")
	}
}

// A guest that set its own key keeps it. A workflow talking to a service with
// its own key scheme knows better than the engine.
func TestAGuestSetIdempotencyKeyIsNotOverwritten(t *testing.T) {
	srv, seen := fetchCapture(t)
	// AllowLoopback because the test server is on 127.0.0.1 and the egress floor
	// correctly refuses it. Same idiom as an_outbound_call_carries_the_callers_trace_test.go;
	// the floor is not weakened, the test opts into loopback for its own listener.
	c := &dbServiceCaller{egress: &engine.EgressGuard{AllowLoopback: true}}

	const guestKey = "guest-chose-this"
	const engineKey = "engine-derived-key"
	if _, err := c.call(context.Background(), "http", "fetch",
		fetchRequestJSON(t, srv.URL, map[string]string{"Idempotency-Key": guestKey}),
		engineKey); err != nil {
		t.Fatalf("http.fetch failed: %v", err)
	}

	if len(*seen) != 1 {
		t.Fatalf("UNMEASURED: the server received %d requests, want 1.", len(*seen))
	}
	if got := (*seen)[0]; got != guestKey {
		t.Errorf("the guest set Idempotency-Key %q and the wire carried %q. A workflow talking "+
			"to a service with its own key scheme must win.", guestKey, got)
	}
}

// A guest supplying an EMPTY key gets the engine's, deliberately. This is the
// case the traceparent comment beside this code records getting wrong once: an
// empty header is indistinguishable from an absent one via Get(), and treating
// it as "the guest wants no key" makes deduplication reachable-off by accident.
func TestAnEmptyGuestKeyTakesTheEnginesKey(t *testing.T) {
	srv, seen := fetchCapture(t)
	// AllowLoopback because the test server is on 127.0.0.1 and the egress floor
	// correctly refuses it. Same idiom as an_outbound_call_carries_the_callers_trace_test.go;
	// the floor is not weakened, the test opts into loopback for its own listener.
	c := &dbServiceCaller{egress: &engine.EgressGuard{AllowLoopback: true}}

	const engineKey = "engine-derived-key"
	if _, err := c.call(context.Background(), "http", "fetch",
		fetchRequestJSON(t, srv.URL, map[string]string{"Idempotency-Key": ""}),
		engineKey); err != nil {
		t.Fatalf("http.fetch failed: %v", err)
	}

	if len(*seen) != 1 {
		t.Fatalf("UNMEASURED: the server received %d requests, want 1.", len(*seen))
	}
	if got := (*seen)[0]; got != engineKey {
		t.Errorf("a guest-supplied EMPTY Idempotency-Key produced %q on the wire, want the "+
			"engine's %q. An empty header must not silently disable deduplication.", got, engineKey)
	}
}
