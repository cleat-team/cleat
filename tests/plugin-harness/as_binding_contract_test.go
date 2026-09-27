package pluginharness

import (
	"strings"
	"testing"

	"github.com/cleat-team/cleat/cleat/wasmtest"
)

// TestASBindingContract pins what the AssemblyScript multi-parameter binder
// does with a present, an absent, and a wrong-typed argument. cleat#1067.
//
// The contract, decided to match the Go contract cleat#1061 pinned:
//
//	present     -> binds the value
//	absent      -> binds the ZERO value and the workflow body runs
//	wrong-typed -> REFUSED
//
// Before this, all three getters collapsed absent and wrong-typed into the same
// zero: `{"note": 42}` for a string parameter was indistinguishable from `{}`,
// and the workflow ran on "" either way. AssemblyScript was the only one of the
// five SDKs that accepted a wrong-typed argument; Go, Rust and Java all refuse.
//
// Absent deliberately still binds zero. It is how an AS workflow expresses an
// optional parameter -- AS has no Python-style defaults, and the transform
// rejects composite types at compile time, so there is no Option-shaped
// alternative. Changing it would break every guest relying on it.
//
// WHY A NON-STRING PARAMETER IS LOAD-BEARING HERE TOO: the fixture takes
// (note: string, count: i32, flag: bool) and this asserts a wrong type for each
// of the three, because they take three different code paths in the generated
// binder -- getString, getNumber and getBool are separate branches with
// separate guards. A string-only assertion would leave two unguarded.
func TestASBindingContract(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping WASM compilation test in short mode")
	}

	wasmBytes := buildASWorkflowWasm(t)

	wenv := wasmtest.NewWasmTestEnv(t)
	defer wenv.Close()

	run := func(t *testing.T, input string) (string, error) {
		t.Helper()
		result, _, err := wenv.Execute(t, wasmBytes, "bind_multiple_params", input)
		return strings.TrimSpace(strings.TrimRight(result, "\x00")), err
	}

	t.Run("present arguments bind their values", func(t *testing.T) {
		got, err := run(t, `{"note":"hi","count":7,"flag":true}`)
		if err != nil {
			t.Fatalf("a well-formed payload was refused: %v", err)
		}
		for _, want := range []string{`"note":"hi"`, `"count":7`, `"flag":true`} {
			if !strings.Contains(got, want) {
				t.Errorf("result %s does not contain %s", got, want)
			}
		}
	})

	// THE CONTROL that stops "wrong-typed is refused" from being satisfied by a
	// build where EVERYTHING is refused. Without it, a transform that rejected
	// every payload would pass every arm below.
	//
	// IT MOVED FROM AN EMPTY PAYLOAD TO A FULL ONE, and the move is the point.
	// It used to send `{}` and assert the parameters bound zero, which was both
	// the control and an assertion of the old contract. cleat#1065 step 4 made
	// an absent declared parameter an error, so `{}` is now refused -- and a
	// control that is refused controls nothing. A fully specified, correctly
	// typed payload still separates "refuses wrong types" from "refuses
	// everything", which is the property the arms below need.
	t.Run("a fully specified payload binds and the body runs", func(t *testing.T) {
		got, err := run(t, `{"note":"hi","count":7,"flag":true}`)
		if err != nil {
			t.Fatalf("a correct payload was refused: %v\n\n"+
				"Every arm below asserts a REFUSAL, so if this one also refuses they "+
				"are all satisfied by a build that accepts nothing.", err)
		}
		for _, want := range []string{`"note":"hi"`, `"count":7`, `"flag":true`} {
			if !strings.Contains(got, want) {
				t.Errorf("result %s does not contain %s: the parameter did not bind", got, want)
			}
		}
	})

	// ABSENT IS NOW REFUSED. cleat#1065 step 4.
	//
	// This arm used to assert the opposite -- that absence binds zero, because
	// that was how an AS workflow expressed an optional parameter (cleat#1067).
	// It now has a better way to say so: a declaration-site DEFAULT, which is
	// the same spelling Python uses. Zero-binding could not tell "sent zero"
	// from "sent nothing"; a default is the author stating that absence is
	// meaningful, which is the thing that should be explicit.
	t.Run("an absent argument is refused", func(t *testing.T) {
		got, err := run(t, `{}`)
		if err == nil {
			t.Fatalf("an empty payload bound zero and the body ran; result = %s\n\n"+
				"An absent declared parameter is an error since cleat#1065. A workflow "+
				"that accepts absence declares a default on the parameter.", got)
		}
		if !strings.Contains(err.Error(), "absent") {
			t.Errorf("the refusal does not say the parameter was absent: %v\n\n"+
				"Absent and wrong-typed want different fixes, so the message must "+
				"separate them -- the arms below assert the wrong-typed wording.", err)
		}
	})

	// One per binder branch: getString, getNumber, getBool.
	for _, tc := range []struct{ name, input, param string }{
		{"a wrong-typed string is refused", `{"note":42,"count":7,"flag":true}`, "note"},
		{"a wrong-typed number is refused", `{"note":"hi","count":"x","flag":true}`, "count"},
		{"a wrong-typed bool is refused", `{"note":"hi","count":7,"flag":"x"}`, "flag"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := run(t, tc.input)
			if err == nil {
				t.Fatalf("a wrong-typed %s was accepted; result = %s\n\n"+
					"Before cleat#1067 the getter returned a zero value for a wrong "+
					"type, indistinguishably from an absent key, and the workflow ran "+
					"on it.", tc.param, got)
			}
			if !strings.Contains(err.Error(), tc.param) {
				t.Errorf("error does not name the offending parameter %q: %v", tc.param, err)
			}
		})
	}
}
