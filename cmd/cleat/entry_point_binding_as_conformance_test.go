package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/wasm"
)

// The AssemblyScript rows of tests/conformance/entry_point_binding_cases.json,
// asserted rather than declared. cleat#1065.
//
// WHY THIS EXISTS. The table has carried an "assemblyscript" expectation for
// every case since it was written, and NOTHING READ THEM. Go's reader is
// engine/entry_point_binding_conformance_test.go and Python's is
// python-sdk/tests/test_entry_point_binding_conformance.py; AS had a column and
// no reader. A column nothing reads is worse than a missing one: it looks like
// coverage in the file everyone consults to see what is covered.
//
// It is also the only end-to-end check of the AS optional mechanism. A
// declaration-site default is what "optional" means in AssemblyScript, the
// transform learned to emit it for cleat#1065, and until now the evidence that
// it works was the generated source rather than a bound value.
//
// WHY IT LIVES IN cmd/cleat RATHER THAN BESIDE THE GO READER. It needs the AS
// toolchain, and compileASFixture -- which scaffolds a throwaway project,
// npm-installs the repo's own @cleat/sdk beside it and runs asc with the
// transform -- is here. Moving the reader to engine/ would mean either
// duplicating that or making the engine suite depend on npm.
//
// ONE FIXTURE, ONE ENTRY POINT PER CASE SHAPE, same as the Go fixture: the
// table varies the PAYLOAD, and the payload is not a property of the fixture.
// The composite case is the exception and gets its own compile, because its
// expected outcome is that compilation FAILS -- putting it in the shared
// fixture would take the other four down with it.
func TestTheASRowsOfTheBindingTableAreAsserted(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping the AssemblyScript binding conformance run in short mode")
	}

	cases := loadBindingTable(t)

	// THE THIRD OUTCOME. A table that moved, emptied, or lost its AS rows would
	// otherwise report zero cases and PASS -- a green that compared nothing,
	// which is the failure this table exists to prevent.
	var asCases []bindingCase
	for _, c := range cases {
		if _, ok := c.Expect["assemblyscript"]; ok {
			asCases = append(asCases, c)
		}
	}
	if len(asCases) < 4 {
		t.Fatalf("only %d cases carry an \"assemblyscript\" expectation; the table has lost rows",
			len(asCases))
	}

	ctx := context.Background()

	// The composite row first, because it is a compile-time outcome and needs
	// no engine at all.
	t.Run("absent composite", func(t *testing.T) {
		want := ""
		for _, c := range asCases {
			if len(c.Declared) == 1 && c.Declared[0].Kind == "composite" {
				want = c.Expect["assemblyscript"]
			}
		}
		// Fatal, not Skip. The composite row is always present in this
		// repository, so its absence is a table regression rather than an
		// optional resource -- and a skip here would be indistinguishable
		// from the row passing.
		if want == "" {
			t.Fatal("the table carries no composite row for AssemblyScript; every SDK " +
				"agrees on this row, so losing it means the table was edited wrongly")
		}
		if want != "refused_at_compile_time" {
			t.Fatalf("this reader only knows how to assert refused_at_compile_time, "+
				"the table says %q -- teach it the new outcome rather than deleting the case", want)
		}
		fx := compileASFixture(t, asCompositeSource)
		if fx.err == nil {
			t.Errorf("a composite entry-point parameter COMPILED. The table says AssemblyScript "+
				"refuses it at compile time, and every other SDK refuses it too.\n\nasc said:\n%s",
				fx.out)
		}
	})

	// Everything else runs. One compile for the four runnable shapes.
	fx := compileASFixture(t, asBindingSource)
	if fx.err != nil {
		t.Fatalf("the conformance fixture did not compile: %v\n%s", fx.err, fx.out)
	}
	wasmBytes, err := os.ReadFile(fx.wasmPath)
	if err != nil {
		t.Fatalf("read the compiled fixture: %v", err)
	}

	// asc emits no cleat.metadata; `cleat build` adds it afterwards
	// (build_as.go). Without it Engine.resolveBackend fails closed rather than
	// guessing, which is cleat#503 working as intended -- so the test has to do
	// what the build does.
	wasmBytes, err = wasm.WriteMetadata(wasmBytes, &wasm.Metadata{
		WorkflowName:         "bindingconformance-as",
		WorkflowVersion:      1,
		ABIVersion:           wasm.CurrentABIVersion,
		MinCompatibleVersion: wasm.CurrentABIVersion,
		Language:             "assemblyscript",
	})
	if err != nil {
		t.Fatalf("writing cleat.metadata: %v", err)
	}

	rt, err := engine.NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	backend, err := engine.NewWasmtimeBackend(ctx)
	if err != nil {
		// Not a Skip: the binding contract is a property of the
		// AssemblyScript-on-wasmtime path, so skipping would report green
		// having never exercised it.
		t.Fatalf("wasmtime backend unavailable: %v", err)
	}
	defer backend.Close(ctx)
	eng := engine.NewEngine(rt, &logCaller{}, engine.WithBackend("assemblyscript", backend))

	entryFor := map[string]string{
		"string":      "bind_string",
		"int":         "bind_int",
		"lone_string": "bind_lone_string",
	}

	for _, c := range asCases {
		c := c
		if len(c.Declared) == 1 && c.Declared[0].Kind == "composite" {
			continue // asserted above, at compile time
		}
		t.Run(c.Name, func(t *testing.T) {
			if len(c.Declared) != 1 {
				t.Fatalf("this reader handles one declared parameter; case has %d", len(c.Declared))
			}
			d := c.Declared[0]
			entry := entryFor[d.Kind]
			if d.OptionalDefault != "" {
				// AssemblyScript's optional is a DECLARATION-SITE DEFAULT, not
				// a pointer as in Go -- which is the divergence the table's
				// "absent, declared optional" row records.
				entry = "bind_optional"
			}
			if entry == "" {
				t.Fatalf("no fixture entry point for kind %q", d.Kind)
			}

			payload, merr := json.Marshal(c.Payload)
			if merr != nil {
				t.Fatalf("marshal payload: %v", merr)
			}
			res, _, _, _, _, execErr := eng.Execute(ctx, wasmBytes, entry, payload)

			got := classifyASBinding(res, execErr, d.Name, d.OptionalDefault, string(payload), c.Payload)
			want := c.Expect["assemblyscript"]
			if got != want {
				t.Errorf("%s: AssemblyScript bound %q, the table says %q.\n\n"+
					"why this case is in the table: %s\n\n"+
					"If this changed deliberately, update the table AND every other SDK's row. "+
					"The point of the table is that the SDKs are compared, so editing one "+
					"reader to agree with itself is the one repair that is always wrong.",
					c.Name, got, want, c.Why)
			}
		})
	}
}

type bindingDeclared struct {
	Name            string `json:"name"`
	Kind            string `json:"kind"`
	OptionalDefault string `json:"optional_default"`
}

type bindingCase struct {
	Name     string            `json:"name"`
	Why      string            `json:"why"`
	Declared []bindingDeclared `json:"declared"`
	Payload  map[string]any    `json:"payload"`
	Expect   map[string]string `json:"expect"`
}

func loadBindingTable(t *testing.T) []bindingCase {
	t.Helper()
	// Read through "..", which go test's caching CANNOT see: it keys on files
	// opened INSIDE the package directory, so an edited fixture one level up
	// leaves the package "(cached)" and the edit unmeasured (cleat#1136). CI
	// passes -count=1; a local run is not sound, and the tell is the word
	// "(cached)" where a duration should be.
	path := filepath.Join(asRepoRoot(t), "tests", "conformance", "entry_point_binding_cases.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the conformance table at %s: %v", path, err)
	}
	var doc struct {
		Cases []bindingCase `json:"cases"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	return doc.Cases
}

// classifyASBinding maps AssemblyScript's answer onto the table's semantic
// outcome tags.
//
// A tag, never a message -- the SDKs word their errors differently and always
// will, which is why the table carries kinds. This mirrors classifyGoBinding
// with one deliberate difference: AS has no absent-marker, because its optional
// IS a declared default, so an absent optional lands on bound_default.
func classifyASBinding(result string, execErr error, pname, optDefault, wholePayload string, payload map[string]any) string {
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
	// "{}" is a non-empty string and would otherwise read as bound_other.
	if s, ok := out.Bound.(string); ok && s == wholePayload {
		return "bound_whole_payload"
	}
	switch v := out.Bound.(type) {
	case string:
		if optDefault != "" && v == optDefault {
			return "bound_default"
		}
		if v == "" {
			return "bound_zero"
		}
	case float64:
		if v == 0 {
			return "bound_zero"
		}
	case nil:
		return "bound_zero"
	}
	return fmt.Sprintf("bound_other(%v)", out.Bound)
}

// asBindingSource mirrors testdata/bindingconformance/workflow.go, entry point
// for entry point, so the two readers are comparing the same shapes.
//
// Each entry echoes WHAT IT BOUND rather than only succeeding, so the reader
// can tell "bound the zero value and ran" apart from "ran without binding
// anything" -- two outcomes that look identical from a workflow returning a
// constant, and the distinction the whole table rests on.
const asBindingSource = `
import { HostCalls, cleatEntry } from "@cleat/sdk";

// The payload arrives as JSON and comes back inside JSON, so the echo has to
// escape. Without this the lone-string case -- whose bound value IS a JSON
// object -- produces a result the reader cannot parse, which reads as a
// binding failure rather than a fixture one.
function jsonString(s: string): string {
  let out: string = "\"";
  for (let i: i32 = 0; i < s.length; i++) {
    let c: string = s.charAt(i);
    if (c == "\"") { out += "\\\""; }
    else if (c == "\\") { out += "\\\\"; }
    else if (c == "\n") { out += "\\n"; }
    else if (c == "\r") { out += "\\r"; }
    else if (c == "\t") { out += "\\t"; }
    else { out += c; }
  }
  return out + "\"";
}

// bind_string carries a SECOND parameter on purpose. A lone string parameter is
// not bound by name at all -- the transform passes the whole payload straight
// through, the same fast path Go's generator has -- so a one-string entry point
// given {} binds the two-character string "{}" rather than "". That is a
// separate row in the table, and bind_lone_string below is it.
@cleatEntry("BindString")
export function bind_string(h: HostCalls, note: string, other: i32): string {
  return "{\"bound\":" + jsonString(note) + "}";
}

@cleatEntry("BindLoneString")
export function bind_lone_string(h: HostCalls, note: string): string {
  return "{\"bound\":" + jsonString(note) + "}";
}

@cleatEntry("BindInt")
export function bind_int(h: HostCalls, count: i32): string {
  return "{\"bound\":" + count.toString() + "}";
}

// AssemblyScript's optional: a declaration-site default. Absence binds
// FALLBACK, which is NOT what Go does -- Go's optional is a pointer and binds
// nil, so the workflow chooses its own fallback. The table records that
// divergence rather than assuming one shape.
@cleatEntry("BindOptional")
export function bind_optional(h: HostCalls, note: string = "FALLBACK"): string {
  return "{\"bound\":" + jsonString(note) + "}";
}
`

// asCompositeSource is the row every SDK agrees on, and the only one whose
// expected outcome is a COMPILE failure rather than a binding.
const asCompositeSource = `
import { HostCalls, cleatEntry } from "@cleat/sdk";

class Item {
  sku: string = "";
  qty: i32 = 0;
}

@cleatEntry("BindComposite")
export function bind_composite(h: HostCalls, item: Item): string {
  return "{\"bound\":\"" + item.sku + "\"}";
}
`
