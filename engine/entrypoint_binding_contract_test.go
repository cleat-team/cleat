package engine

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

// TestEntryPointBindingContract pins what cleatDispatch does when a payload
// does not carry a value for a declared entry-point parameter.
//
// WHY THIS EXISTS: until it did, nothing in cleat's own suite started an entry
// point with a parameter missing from the payload. That was established by
// instrumenting the generator -- making every binding fail loudly on absence,
// validating the probe with a known-positive and a negative control, then
// running ./engine/ ./wasm/... ./cleat/... -- and exactly one test tripped it:
// the probe's own validation case.
//
// The cost of that gap was cleat#1046, which turned an absent int into a hard
// bind error. It passed everything cleat has, and was caught downstream by
// cleat-ports, whose payloads omit parameters by accident rather than by
// design. #1057 restored the behaviour. This test is what should have failed.
//
// A green suite that endorses a breaking change is worse than a red one: from
// inside core the change looked correct, because nothing disagreed.
//
// WHAT IS ASSERTED IS MEASURED, NOT DESIGNED. The three types do not agree,
// and this test states the disagreement rather than smoothing it over:
//
//	int    absent -> binds 0   -> workflow runs
//	string absent -> binds ""  -> workflow runs
//	slice  absent -> BIND ERROR ("unexpected end of JSON input")
//
// The scalar cases go through arms that tolerate an absent key; the composite
// case unmarshals extractJSONRaw's empty string and fails. Whether that
// asymmetry should stand is a product question -- a caller who omits an
// optional struct gets a hard failure where omitting an int gets a zero. It is
// pinned here so that changing it has to be a decision rather than a side
// effect, which is precisely what #1046 was.
func TestEntryPointBindingContract(t *testing.T) {
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
		// Not a t.Skip: the binding contract is a property of the Go-on-wasmtime
		// path, so skipping would report green having never exercised it --
		// the same failure mode this test was written to close.
		t.Fatalf("wasmtime backend unavailable: %v (if this build disabled CGO, "+
			"that is the defect: it removes the primary backend entirely)", err)
	}
	defer backend.Close(ctx)

	eng := NewEngine(rt, &mockCaller{}, WithBackend("go", backend))

	// long_running(iterations int) loops `iterations` times and returns "done",
	// so iterations=0 is observable as a normal completion rather than as an
	// absence of failure. This is the exact case cleat#1046 broke.
	t.Run("an absent int is REFUSED, like the composite", func(t *testing.T) {
		// INVERTED BY cleat#1065 step 4. This arm asserted the opposite --
		// that an absent int binds 0 and the workflow runs -- which was the
		// contract cleat#1046 accidentally broke and this test was written to
		// protect.
		//
		// The composite arm below anticipated this exact edit: "If this is now
		// intended, the two scalar cases above should be reviewed at the same
		// time so all three agree." They now agree, which is the whole point --
		// absence is uniform across types rather than a rule per type.
		res, _, _, _, _, err := eng.Execute(ctx, wasmBytes, "long_running", []byte(`{}`))
		if err == nil {
			t.Fatalf("an omitted int parameter bound and the workflow ran; result = %q.\n\n"+
				"An absent declared parameter is an error since cleat#1065. A workflow "+
				"that accepts absence declares the parameter as *int.", res)
		}
		var gre *GuestReturnedError
		if !errors.As(err, &gre) {
			t.Errorf("error is %T, want *GuestReturnedError: %v\n\n"+
				"A refused parameter must be a clean guest-reported failure, not a "+
				"trap -- the property cleat#1059 established for every bind failure.", err, err)
		}
		if !strings.Contains(err.Error(), "iterations") {
			t.Errorf("the refusal does not name the parameter: %v", err)
		}
	})

	// The control for the case above. Without it, "absent binds zero" is also
	// satisfied by a build where the parameter is ignored entirely.
	t.Run("a present int binds its value", func(t *testing.T) {
		res, _, _, _, _, err := eng.Execute(ctx, wasmBytes, "long_running", []byte(`{"iterations":0}`))
		if err != nil {
			t.Fatalf("a present int parameter failed to bind: %v", err)
		}
		if res != "done" {
			t.Errorf("result = %q, want \"done\"", res)
		}
	})

	// place_order(userID string, cart []CartItem). Omitting userID must still
	// reach the workflow body, which is observable because the body runs far
	// enough to produce a tracking id.
	t.Run("an absent string is REFUSED, like the composite", func(t *testing.T) {
		// INVERTED BY cleat#1065 step 4, with the int arm above. A string has
		// bound "" on absence since the first generator; that is the behaviour
		// that could not tell an empty string from a missing key.
		//
		// place_order(userID string, cart []CartItem) -- so this payload supplies
		// the composite and omits the scalar, isolating the scalar rule.
		res, _, _, _, _, err := eng.Execute(ctx, wasmBytes,
			"place_order", []byte(`{"cart":[{"sku":"a","qty":1}]}`))
		if err == nil {
			t.Fatalf("an omitted string parameter bound and the workflow ran; result = %q.\n\n"+
				"An absent declared parameter is an error since cleat#1065. Declare "+
				"userID as *string if absence is meaningful.", res)
		}
		if !strings.Contains(err.Error(), "userID") {
			t.Errorf("the refusal does not name the parameter: %v", err)
		}
	})

	// The composite case, which since cleat#1065 step 4 matches the two above
	// rather than differing from them. It was asserted here as it behaved, with
	// the difference named, precisely so that unifying them would be a
	// deliberate edit to this test -- which is what it was.
	t.Run("an absent composite is a bind error, like the scalars", func(t *testing.T) {
		res, _, _, _, _, err := eng.Execute(ctx, wasmBytes,
			"place_order", []byte(`{"userID":"u"}`))
		if err == nil {
			t.Fatalf("an omitted slice bound without error; result = %q.\n\n"+
				"If this is now intended, the two scalar cases above should be "+
				"reviewed at the same time so all three agree.", res)
		}
		// It must be a clean guest-reported failure, not a trap (3.23), and it
		// must name the parameter -- the properties cleat#1059 established for
		// every bind failure.
		var gre *GuestReturnedError
		if !errors.As(err, &gre) {
			t.Errorf("error is %T, want *GuestReturnedError: %v", err, err)
		}
		if !strings.Contains(err.Error(), "cart") {
			t.Errorf("error does not name the parameter that failed to bind: %v", err)
		}
	})

	// The workflow's OWN guard, which must remain distinguishable from a bind
	// failure. An empty cart that is PRESENT reaches the body and is rejected
	// there; an absent one never gets that far. If these two ever produce the
	// same error, the caller can no longer tell "you sent nothing" from "what
	// you sent was empty".
	t.Run("a present-but-empty composite reaches the workflow's own guard", func(t *testing.T) {
		_, _, _, _, _, err := eng.Execute(ctx, wasmBytes,
			"place_order", []byte(`{"userID":"u","cart":[]}`))
		if err == nil {
			t.Fatalf("the fixture's empty-cart guard did not fire")
		}
		if strings.Contains(err.Error(), "unmarshal") {
			t.Errorf("a present-but-empty cart was reported as a BIND failure, "+
				"which makes it indistinguishable from an absent one: %v", err)
		}
		if !strings.Contains(err.Error(), "cart is empty") {
			t.Errorf("expected the workflow's own guard, got: %v", err)
		}
	})
}
