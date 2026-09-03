package engine

import (
	"context"
	"testing"
)

// IMPROVEMENT-PLAN 3.84 guarded six fresh paths so a defer segment cannot
// start new work past the frontier. Fetch was the seventh, and it was not in
// that section's inventory: the table was built by reading the entry points a
// guest uses to reach a *service*, and cleat_fetch reaches one without going
// through the durable-call family.
//
// It matters more than the count suggests. An outbound HTTP request is the
// most externally visible kind of new work there is -- it leaves a side effect
// on someone else's server that no amount of unwinding takes back -- and the
// workflow doing it has already been recorded as terminated.
//
// Go guests were never exposed: cleat/runtime_workflow.go's DurableFetch calls
// DurableCall("http", "fetch", ...), which 3.84 already guards. The guests that
// import cleat_fetch directly are the ones that reach execSession.Fetch, which
// is Java (crates/cleat-java, cleatFetchRaw) and AssemblyScript.

// recordingFetcher reports whether the engine actually went out to the network.
type recordingFetcher struct{ called bool }

func (f *recordingFetcher) Fetch(ctx context.Context, method, url, headersJSON, body string) (string, error) {
	f.called = true
	return `{"body":"","status_code":200}`, nil
}

func deferPhaseFetchSession(t *testing.T, deferPhase bool) (*execSession, *recordingFetcher) {
	t.Helper()
	f := &recordingFetcher{}
	opts := []EngineOption{WithFetcher(f)}
	if deferPhase {
		opts = append(opts, WithDeferPhase())
	}
	return &execSession{
		engine:     NewEngine(nil, nil, opts...),
		nowMs:      1000000,
		deferrals:  make(map[string]string),
		stateStore: make(map[string]string),
		queryState: make(map[string]string),
	}, f
}

func TestADeferSegmentCannotStartAnHTTPFetch(t *testing.T) {
	s, fetcher := deferPhaseFetchSession(t, true)

	got := s.Fetch(context.Background(), nil, "POST", "https://example.invalid/charge", "{}", `{"amount":1}`, 0, 0)

	if got != callSuspendSentinel {
		t.Errorf("Fetch returned %#x, want callSuspendSentinel %#x", got, callSuspendSentinel)
	}
	if fetcher.called {
		t.Error("the fetcher was invoked, so a terminated workflow's defer segment made a real " +
			"outbound HTTP request -- the one kind of new work that cannot be taken back")
	}
	if len(s.history) != 0 {
		t.Errorf("the refused fetch recorded %d events, want 0; a stop is not a step",
			len(s.history))
	}
}

// The other direction, and the one that makes the test above mean something:
// outside a defer segment the identical call must still go out. A guard that
// blocked every fetch would pass the assertions above just as well.
func TestAnOrdinarySegmentStillFetches(t *testing.T) {
	s, fetcher := deferPhaseFetchSession(t, false)

	got := s.Fetch(context.Background(), nil, "POST", "https://example.invalid/charge", "{}", `{"amount":1}`, 0, 0)

	if got == callSuspendSentinel {
		t.Fatalf("Fetch returned the stop sentinel outside a defer segment")
	}
	if !fetcher.called {
		t.Error("the fetcher was not invoked outside a defer segment, so this file's other " +
			"test proves nothing about the defer phase")
	}
	if len(s.history) != 1 {
		t.Errorf("an ordinary fetch recorded %d events, want 1", len(s.history))
	}
}

// The sentinel has to survive the layout Fetch actually returns. packSimpleResult
// is not the layout 3.84's cleat_call work used, and a bit that is free in one
// layout and reachable in another is exactly the defect 3.83 exists to record.
func TestTheStopSentinelIsNotAReachableSimpleResult(t *testing.T) {
	var reachable uint64
	for extra := 0; extra < (1 << 24); extra += 1009 {
		for ec := 0; ec < 256; ec += 7 {
			reachable |= uint64(packSimpleResult(byte(ec), uint32(extra)))
		}
	}
	if reachable&uint64(callSuspendSentinel) != 0 {
		t.Errorf("packSimpleResult can produce bit 31 (reachable=%016x), so a real Fetch result "+
			"is indistinguishable from a stop", reachable)
	}
}
