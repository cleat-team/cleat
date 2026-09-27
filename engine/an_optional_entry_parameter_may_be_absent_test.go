package engine

import (
	"context"
	"os"
	"strings"
	"testing"
)

// A POINTER entry-point parameter may be absent: it binds nil and the workflow
// RUNS. cleat#1065.
//
// WHY GO NEEDED A MECHANISM AT ALL. The entry-point contract decided in
// cleat#1065 is "an absent declared parameter is an error, unless the parameter
// is declared optional". Every other SDK can already say "optional" -- Python
// with a parameter default, Rust with Option<T> or #[serde(default)] -- and Go
// could not. The only thing resembling it was the implicit zero-binding of
// string and int, which is a different statement: it cannot distinguish "the
// caller sent zero" from "the caller sent nothing", and it is unavailable for
// every other type, composites included.
//
// So this is the missing half of that contract, landed BEFORE the contract
// changes. Until the flip, an absent scalar still binds zero (pinned by
// TestEntryPointBindingContract, which this test deliberately does not touch);
// what changes here is only that *T stops being a hard error and starts being
// a declaration.
//
// WHAT THIS TEST DOES NOT COVER, stated because an untested emitter arm is
// exactly what the next person needs to know. The generator has TWO binding
// sites -- the //go:wasmexport wrapper and cleatDispatch -- and both carry the
// pointer arm. This test exercises only the second: the wasmtime backend calls
// _start (engine/backend_wasmtime.go:793), which runs the generated main(),
// which calls cleatDispatch. Falsified both ways: removing the dispatch arm
// turns this red, removing the wasmexport arm leaves it green.
//
// Both arms are kept regardless, because a binding rule living in one of two
// emitters is how cleat#1057 left the int arm reporting success. But the
// wasmexport arm is asserted by nothing here, and saying so is better than a
// green that implies otherwise.
//
// ADDITIVE, and that was checked rather than assumed: a pointer type used to
// fall into the emitter's default arm, where extractJSONRaw returns "" for a
// missing key and json.Unmarshal("") fails. Nothing in testdata/ or examples/
// declares a pointer entry parameter, so nothing relied on that error.
func TestAnOptionalEntryParameterMayBeAbsent(t *testing.T) {
	wasmBytes, err := os.ReadFile(buildFixtureWasm(t, "optionalparam"))
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
		// Not a Skip. The binding contract is a property of the Go-on-wasmtime
		// path; skipping would report green having never exercised it, which is
		// the failure mode TestEntryPointBindingContract was written to close.
		t.Fatalf("wasmtime backend unavailable: %v", err)
	}
	defer backend.Close(ctx)

	// Through the ENGINE, not backend.Execute directly. A bare backend call
	// takes a nil HostHandler, and the generated main() reaches a host call
	// before the workflow body runs -- so every case dies in _start with a nil
	// dereference, identically, whatever the binding did. That looked like a
	// defect in the pointer arm and was a defect in this test; a non-pointer
	// control fixture failing the same way is what separated them.
	eng := NewEngine(rt, &mockCaller{}, WithBackend("go", backend))

	cases := []struct {
		name         string
		payload      string
		wantIn       string
		wantRefused  string // non-empty: expect a refusal naming this
		whyItMatters string
	}{
		{
			// THE CONTROL, and it is load-bearing. A run that bound nothing at
			// all returns the same "coupon":null answer as a run that correctly
			// bound an absent optional. The userID proves binding happened.
			name:    "control: both present",
			payload: `{"userID":"u1","promo":{"code":"SAVE","off_cents":250}}`,
			wantIn:  `"coupon":"SAVE"`,
			whyItMatters: "a present optional must still decode; if this fails the " +
				"pointer arm is not decoding at all and every other row is meaningless",
		},
		{
			name:    "optional absent",
			payload: `{"userID":"u1"}`,
			wantIn:  `"coupon":null`,
			whyItMatters: "this is the mechanism. Before cleat#1065 a pointer parameter " +
				"fell into the emitter's default arm and an absent key produced " +
				"'unexpected end of JSON input', so the workflow never ran",
		},
		{
			// INVERTED BY cleat#1065 step 4, which this row named in advance as
			// "a separate, later change". It asserted that an absent userID
			// bound "" -- the zero-binding the flip removed.
			//
			// It still earns its place: it is the row that proves the pointer
			// arm and the REQUIRED arm are different code paths. The optional
			// `promo` is absent here too and does not refuse; only `userID`
			// does. A change that made the emitter refuse every absent key
			// would pass "optional absent" above and fail here.
			name:        "the REQUIRED param is refused when absent, the optional is not",
			payload:     `{}`,
			wantRefused: "userID",
			whyItMatters: "an absent declared parameter is an error since cleat#1065, " +
				"and declaring it optional is what exempts it -- so this row separates " +
				"the two arms rather than testing one of them twice",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, _, _, _, _, err := eng.Execute(ctx, wasmBytes, "apply_coupon", []byte(tc.payload))
			if tc.wantRefused != "" {
				if err == nil {
					t.Fatalf("the run was accepted and returned %q, want a refusal naming %q"+
						"\n\nwhy this case matters: %s", res, tc.wantRefused, tc.whyItMatters)
				}
				if !strings.Contains(err.Error(), tc.wantRefused) {
					t.Errorf("the refusal does not name %q: %v\n\nwhy this case matters: %s",
						tc.wantRefused, err, tc.whyItMatters)
				}
				return
			}
			if err != nil {
				t.Fatalf("Execute: %v\n\nwhy this case matters: %s", err, tc.whyItMatters)
			}
			if !strings.Contains(res, tc.wantIn) {
				t.Errorf("result %q does not contain %q\n\nwhy this case matters: %s",
					res, tc.wantIn, tc.whyItMatters)
			}
		})
	}
}
