package engine

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// TestGuestReturnedErrorIsAFailureNotAResult is the regression test for
// IMPROVEMENT-PLAN 3.22: a Go workflow that returns an error was reported to
// the worker as a *success* whose result happened to contain the error text,
// so it was stored with status='done'.
//
// The fixture entry point is chosen so that nothing else can produce the
// failure: basic.PlaceOrder returns fmt.Errorf("cart is empty") on an empty
// cart, before it reaches any durable call. No service, no database, no
// crash -- just a workflow that returns an error, which is the case that was
// silently swallowed.
//
// Reverting the precedence in backend_wasmtime.go (completeResult checked
// before completeErr) fails this with a nil error and the message sitting in
// result instead.
func TestGuestReturnedErrorIsAFailureNotAResult(t *testing.T) {
	wasmBytes, err := os.ReadFile(buildFixtureWasm(t, "basic"))
	if err != nil {
		t.Fatalf("read WASM: %v", err)
	}

	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	backend, err := NewWasmtimeBackend(ctx)
	if err != nil {
		// Deliberately not a t.Skip -- scripts/check-skips.sh case (c), and the
		// same reasoning as cancellation_e2e_test.go. The defect this test
		// covers is specific to the Go-on-wasmtime path; skipping it on a
		// CGO-less build would report the suite green having never executed the
		// one branch in question.
		t.Fatalf("wasmtime backend unavailable: %v (if this build disabled CGO, "+
			"that is the defect: it removes the primary backend entirely)", err)
	}
	defer backend.Close(ctx)

	engine := NewEngine(rt, &mockCaller{}, WithBackend("go", backend))

	// An empty cart is the fixture's own guard clause.
	input := []byte(`{"userID":"test-user","cart":[]}`)
	result, _, _, _, _, err := engine.Execute(ctx, wasmBytes, "place_order", input)

	if err == nil {
		t.Fatalf("Execute returned no error for a workflow that returned one; result = %q.\n\n"+
			"The guest reported the failure with cleat_complete(status=1) and then, because "+
			"the generated main() cannot tell that the dispatch failed, reported its return "+
			"value again with status=0. Preferring the status-0 report discards the failure: "+
			"the worker takes the success path and stores status='done'.", result)
	}
	if !strings.Contains(err.Error(), "cart is empty") {
		t.Errorf("Execute error = %q, which does not carry the workflow's own message %q.\n\n"+
			"Surfacing the failure is only half of it -- an operator needs the guest's account "+
			"of it, not just the fact that something went wrong.", err, "cart is empty")
	}
	if result != "" {
		t.Errorf("Execute returned both an error and the result %q; a failed run has no result", result)
	}
}

// TestGuestErrorTextDecodesWhatTheGuestEncoded covers the two shapes that reach
// guestErrorText, because they arrive by different routes and only one of them
// is JSON.
//
// The JSON case is what every Go guest sends: wasm/exports.go passes the
// message through encodeJSONString before calling cleat_complete, so the host
// receives a quoted, escaped string literal. The raw case is the _start panic
// recovery in backend_wasmtime.go, which writes a plain Go string into the same
// variable. Decoding one and passing the other through unchanged is the whole
// job.
func TestGuestErrorTextDecodesWhatTheGuestEncoded(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "what the Go guest encodes",
			raw:  `"durable call payments.Ship: [0] [AMBIGUOUS] call outcome unknown at step 2"`,
			want: `durable call payments.Ship: [0] [AMBIGUOUS] call outcome unknown at step 2`,
		},
		{
			// The case that makes the fallback load-bearing rather than
			// defensive: this string never went through encodeJSONString.
			name: "the host's own _start panic text",
			raw:  `wasm _start panic: runtime error: index out of range [3]`,
			want: `wasm _start panic: runtime error: index out of range [3]`,
		},
		{
			// An unmarshal error routinely contains quotes, so this is the
			// shape that would break a naive strings.Trim(raw, `"`).
			name: "interior quotes survive the round trip",
			raw:  string(mustMarshal(t, `unmarshal input: invalid character '"' after top-level value`)),
			want: `unmarshal input: invalid character '"' after top-level value`,
		},
		{
			// A bare quote decodes as nothing at all; passing it through is
			// better than reporting a decode failure in place of the guest's
			// own account of what went wrong.
			name: "a truncated fragment is passed through",
			raw:  `"unterminated`,
			want: `"unterminated`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := guestErrorText(tc.raw); got != tc.want {
				t.Errorf("guestErrorText(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

func mustMarshal(t *testing.T, s string) []byte {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshalling the fixture string: %v", err)
	}
	return b
}
