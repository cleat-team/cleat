package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The shared entry-point binding table, run against Go.
// tests/conformance/entry_point_binding_cases.json. cleat#1065.
//
// WHY A SHARED TABLE AND NOT A GO TEST. The five SDKs do not share an
// entry-point shape, and the divergence survived because nothing compared them.
// cleat#1046 turned an absent int into a hard bind error, PASSED CLEAT'S ENTIRE
// SUITE, and was caught downstream by cleat-ports, whose payloads omit
// parameters. #1057 reverted it. A green suite that endorses a breaking change
// is worse than a red one, because from inside core the change looked correct.
//
// Python's reader is python-sdk/tests/test_entry_point_binding_conformance.py.
// Two readers is the minimum for this file to be a COMPARISON rather than a
// test with extra indirection -- cleat#1136 is on record for a conformance
// table whose fourth consumer compared against nothing -- and Go and Python are
// the two that DISAGREE on the scalar row, which is the row the contract has to
// decide.
//
// IT CHARACTERISES TODAY, deliberately. The decision on cleat#1065 is "an
// absent declared parameter is an error unless declared optional", and the flip
// is a versioned breaking change that has not happened. Recording today is what
// makes the flip visible when it lands.
func TestEntryPointBindingMatchesTheSharedTable(t *testing.T) {
	type declared struct {
		Name            string `json:"name"`
		Kind            string `json:"kind"`
		OptionalDefault string `json:"optional_default"`
	}
	type tcase struct {
		Name     string            `json:"name"`
		Why      string            `json:"why"`
		Declared []declared        `json:"declared"`
		Payload  map[string]any    `json:"payload"`
		Expect   map[string]string `json:"expect"`
	}
	var doc struct {
		Cases []tcase `json:"cases"`
	}

	// The table is read through "..", which go test's caching CANNOT see: it
	// keys on files opened INSIDE the package directory, so an edited fixture
	// one level up leaves the package "(cached)" and the edit unmeasured
	// (cleat#1136). CI passes -count=1 to every matrix package, so CI is sound;
	// a local `go test ./engine/` is not, and the tell is the word "(cached)"
	// where a duration should be.
	path := filepath.Join("..", "tests", "conformance", "entry_point_binding_cases.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the conformance table at %s: %v", path, err)
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}

	// THE THIRD OUTCOME. A table that moved, emptied, or lost its "go" rows
	// would otherwise report zero cases and PASS -- a green that compared
	// nothing, which is the failure this table exists to prevent.
	var goCases []tcase
	for _, c := range doc.Cases {
		if _, ok := c.Expect["go"]; ok {
			goCases = append(goCases, c)
		}
	}
	if len(goCases) < 4 {
		t.Fatalf("only %d cases carry a \"go\" expectation in %s; the table has lost rows",
			len(goCases), path)
	}

	ctx := context.Background()
	wasmBytes, err := os.ReadFile(buildFixtureWasm(t, "bindingconformance"))
	if err != nil {
		t.Fatalf("read WASM: %v", err)
	}
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	backend, err := NewWasmtimeBackend(ctx)
	if err != nil {
		// Not a Skip: the binding contract is a property of the Go-on-wasmtime
		// path, so skipping would report green having never exercised it.
		t.Fatalf("wasmtime backend unavailable: %v", err)
	}
	defer backend.Close(ctx)
	eng := NewEngine(rt, &mockCaller{}, WithBackend("go", backend))

	// One entry point per case SHAPE. The payload is the table's business.
	entryFor := map[string]string{
		"string":      "bind_string",
		"int":         "bind_int",
		"composite":   "bind_composite",
		"lone_string": "bind_lone_string",
	}

	for _, c := range goCases {
		c := c
		t.Run(c.Name, func(t *testing.T) {
			if len(c.Declared) != 1 {
				t.Fatalf("this reader handles one declared parameter; case has %d", len(c.Declared))
			}
			d := c.Declared[0]
			entry := entryFor[d.Kind]
			if d.OptionalDefault != "" {
				// Go's optional is a POINTER, not a declaration-site default.
				entry = "bind_optional"
			}
			if entry == "" {
				t.Fatalf("no fixture entry point for kind %q", d.Kind)
			}

			payload, err := json.Marshal(c.Payload)
			if err != nil {
				t.Fatalf("marshal payload: %v", err)
			}
			res, _, _, _, _, execErr := eng.Execute(ctx, wasmBytes, entry, payload)

			got := classifyGoBinding(res, execErr, d.Name, d.OptionalDefault, string(payload), c.Payload)
			want := c.Expect["go"]
			if got != want {
				t.Errorf("%s: Go bound %q, the table says %q.\n\n"+
					"why this case is in the table: %s\n\n"+
					"If this changed deliberately, update the table AND every other SDK's row. "+
					"The point of the table is that the SDKs are compared, so editing one "+
					"reader to agree with itself is the one repair that is always wrong.",
					c.Name, got, want, c.Why)
			}
		})
	}
}

// classifyGoBinding maps Go's answer onto the table's semantic outcome tags.
//
// A tag, never a message. The SDKs word their errors differently and always
// will, which is why the table carries kinds -- the same reasoning
// quorum_cases.json gives for error_kind.
func classifyGoBinding(result string, execErr error, pname, optDefault, wholePayload string, payload map[string]any) string {
	if execErr != nil {
		return "refused"
	}
	var out struct {
		Bound any `json:"bound"`
	}
	if err := json.Unmarshal([]byte(result), &out); err != nil {
		return fmt.Sprintf("unparseable(%s)", strings.TrimSpace(result))
	}
	if _, present := payload[pname]; present {
		return "bound_value"
	}
	// The single-string fast path: the parameter got the whole payload rather
	// than a value extracted by name. Checked BEFORE the zero tests, because
	// "{}" is a non-empty string and would otherwise be reported as
	// bound_other -- which is how it first surfaced, reading as a defect.
	if s, ok := out.Bound.(string); ok && s == wholePayload {
		return "bound_whole_payload"
	}
	if out.Bound == nil {
		// Go's optional marker: *T bound nil because the key was absent.
		if optDefault != "" {
			return "bound_absent_marker"
		}
		return "bound_zero"
	}
	switch v := out.Bound.(type) {
	case string:
		if v == "" {
			return "bound_zero"
		}
		if v == optDefault {
			return "bound_default"
		}
	case float64:
		if v == 0 {
			return "bound_zero"
		}
	}
	return fmt.Sprintf("bound_other(%v)", out.Bound)
}
