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

	// The control that stops "wrong-typed is refused" from being satisfied by a
	// build where everything is refused -- and the case that must NOT change.
	t.Run("absent arguments bind zero and the body runs", func(t *testing.T) {
		got, err := run(t, `{}`)
		if err != nil {
			t.Fatalf("an empty payload was refused: %v\n\n"+
				"Absent must bind the zero value: it is how an AS workflow expresses "+
				"an optional parameter. See cleat#1067.", err)
		}
		for _, want := range []string{`"note":""`, `"count":0`, `"flag":false`} {
			if !strings.Contains(got, want) {
				t.Errorf("result %s does not contain %s: the parameter did not bind zero", got, want)
			}
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
