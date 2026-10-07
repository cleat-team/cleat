package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

// cleat#2914. computePythonEntryPointSchema is the Go-side half of the
// Python JSON Schema emitter: it shells out to cleat_sdk.jsonschema_emitter
// and hands the caller back the entry point's logical name alongside the
// schema document, already shaped as map[string]engine.EntryPointSchema so
// it can be written straight to a ".schema.json" sidecar.
//
// This test does not need componentize-py at all -- unlike a full `cleat
// build --target python`, computePythonEntryPointSchema never reaches the
// compiler. It needs python3 >= cleat_sdk's own minimum, the same gate
// a_relative_python_entry_resolves_where_the_user_stands_test.go uses for
// the same reason: cleat_sdk's own imports use `X | None` syntax.
func skipIfNoCleatSDKPython(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not installed")
	}
	if v, ok := pythonAtLeast(pythonSDKMinVersion); !ok {
		t.Skipf("python3 is %s; cleat_sdk requires >= %s", v, pythonSDKMinVersion)
	}
}

func writePythonFixture(t *testing.T, source string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "wf.py")
	if err := os.WriteFile(path, []byte(source), 0o644); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}
	return path
}

func TestComputePythonEntryPointSchemaUsesTheDecoratorsOwnName(t *testing.T) {
	skipIfNoCleatSDKPython(t)
	t.Setenv("PYTHONPATH", filepath.Join(repoRoot(t), "python-sdk"))

	// The decorator's own name argument ("PlaceOrder") differs from the
	// Python identifier ("place_order") on purpose -- this is exactly the
	// case _find_entry's own doc comment explains: matching on
	// module._cleat_entry_wrappers rather than getattr(module, func_name)
	// is what lets this resolve correctly even though they differ.
	path := writePythonFixture(t, `
from dataclasses import dataclass
from cleat_sdk.entry import cleat_entry
from cleat_sdk.host_calls import HostCalls


@dataclass
class Address:
    street: str
    city: str


@cleat_entry("PlaceOrder")
def place_order(h: HostCalls, user_id: str, cart: list[int], address: Address, priority: int = 1) -> str:
    return "{}"
`)

	name, schemaJSON, err := computePythonEntryPointSchema(path, "place_order")
	if err != nil {
		t.Fatalf("computePythonEntryPointSchema: %v", err)
	}
	if name != "PlaceOrder" {
		t.Fatalf("entry point name = %q, want %q (the decorator's own argument, not the Python identifier)", name, "PlaceOrder")
	}

	var decoded map[string]struct {
		Params json.RawMessage `json:"params"`
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(schemaJSON, &decoded); err != nil {
		t.Fatalf("schemaJSON does not unmarshal as map[string]engine.EntryPointSchema: %v\n%s", err, schemaJSON)
	}
	entry, ok := decoded["PlaceOrder"]
	if !ok {
		t.Fatalf("schemaJSON has no %q key: %s", "PlaceOrder", schemaJSON)
	}

	var params struct {
		Type       string                     `json:"type"`
		Required   []string                   `json:"required"`
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(entry.Params, &params); err != nil {
		t.Fatalf("params does not unmarshal: %v\n%s", err, entry.Params)
	}
	if params.Type != "object" {
		t.Errorf("params.type = %q, want %q", params.Type, "object")
	}
	wantProps := []string{"user_id", "cart", "address", "priority"}
	for _, p := range wantProps {
		if _, ok := params.Properties[p]; !ok {
			t.Errorf("params.properties is missing %q: %s", p, entry.Params)
		}
	}
	wantRequired := map[string]bool{"user_id": true, "cart": true, "address": true}
	for _, r := range params.Required {
		if !wantRequired[r] {
			t.Errorf("params.required contains %q, want only %v", r, wantRequired)
		}
		delete(wantRequired, r)
	}
	if len(wantRequired) > 0 {
		t.Errorf("params.required is missing %v (priority correctly absent: it has a default)", wantRequired)
	}
}

// TestComputePythonEntryPointSchemaWorksForTheBareDecoratorForm is cleat#2976.
//
// `@cleat_entry` WITH NO PARENTHESES is what NINE OF THE ELEVEN
// `python-sdk/examples/*.py` still use -- the other two pass an explicit name,
// which is exactly the case the parenthesised form exists for. It also remains
// fully supported: `cleat_sdk/entry.py` accepts both, calling the bare form
// legacy and the parenthesised one preferred -- which is why cleat#3015 moved
// the README's own examples to the parenthesised form, and why this test must
// keep the bare form working. (This paragraph used to say the README documents
// the bare form; it does not any more, and the count beside it is unaffected.)
// It was also the form that produced no schema at all: the
// decorator's dual-form branch passed the decorated FUNCTION into the slot the
// workflow NAME is read from, so the registry was keyed by a function object,
// and `cleat build --target python` reported Build SUCCESS while writing no
// `.schema.json` -- leaving start-input validation silently off.
//
// WHERE IT ACTUALLY FAILED, because the obvious reading is wrong and this
// comment asserted it until cleat-review corrected it. `_find_entry` DID find
// the entry: it matches on `wrapper.__name__`, which `functools.wraps` set
// correctly even on the broken tree, and it RETURNS the registry key as the
// workflow name. The failure is downstream, in the emitter's `main()` --
// `json.dumps({workflow_name: ...})` with a function as the key:
//
//	TypeError: keys must be str, int, float, bool or None, not function
//
// This comment's own evidence is what refutes the old phrasing: "not function"
// is a statement about a key that EXISTS.
//
// WHY THIS TEST AND NOT ONE ABOUT THE REGISTRY KEY. The SDK's own tests already
// covered this form and they PASSED: test_entry.py's `test_cleat_entry_basic`
// decorates with a bare `@cleat_entry` and asserts the wrapper runs
// end-to-end, which it did. The defect was never in what the wrapper does -- it
// was in what the registry is keyed by, which only a reader of the registry can
// see. So this asserts the artifact the build actually consumes, through the
// same helper the build calls, and compares the two decorator forms against
// each other rather than against a hand-written expectation: they address one
// workflow, so a caller must not be able to tell which form it was written
// with.
func TestComputePythonEntryPointSchemaWorksForTheBareDecoratorForm(t *testing.T) {
	skipIfNoCleatSDKPython(t)
	t.Setenv("PYTHONPATH", filepath.Join(repoRoot(t), "python-sdk"))

	const source = `
from cleat_sdk.entry import cleat_entry
from cleat_sdk.host_calls import HostCalls


%s
def place_order(h: HostCalls, user_id: str, priority: int = 1) -> str:
    return "{}"
`

	bare := writePythonFixture(t, fmt.Sprintf(source, "@cleat_entry"))
	paren := writePythonFixture(t, fmt.Sprintf(source, `@cleat_entry("place_order")`))

	bareName, bareSchema, err := computePythonEntryPointSchema(bare, "place_order")
	if err != nil {
		t.Fatalf("computePythonEntryPointSchema on the BARE form: %v\n\n"+
			"This is cleat#2976. `cleat build --target python` treats a schema-computation "+
			"failure as NON-FATAL (see runBuildPython), so this error does not fail the "+
			"build -- it silently drops the .schema.json sidecar and turns start-input "+
			"validation off for the form nine of the eleven bundled examples use.", err)
	}
	if bareName != "place_order" {
		t.Errorf("bare form resolved the entry point as %q, want %q -- the decorator "+
			"defaults to the function's own name, the same key the parenthesised form "+
			"produces when it is given no explicit name", bareName, "place_order")
	}

	_, parenSchema, err := computePythonEntryPointSchema(paren, "place_order")
	if err != nil {
		t.Fatalf("computePythonEntryPointSchema on the parenthesised form (the control): %v", err)
	}

	// Compare decoded, not bytes: key order is the emitter's business, and a
	// difference in whitespace is not a difference a caller can observe.
	var bareDecoded, parenDecoded any
	if err := json.Unmarshal(bareSchema, &bareDecoded); err != nil {
		t.Fatalf("bare schema does not unmarshal: %v\n%s", err, bareSchema)
	}
	if err := json.Unmarshal(parenSchema, &parenDecoded); err != nil {
		t.Fatalf("parenthesised schema does not unmarshal: %v\n%s", err, parenSchema)
	}
	if !reflect.DeepEqual(bareDecoded, parenDecoded) {
		t.Errorf("the two decorator forms describe the same workflow differently:\n"+
			"  bare  %s\n  paren %s\n"+
			"A caller must not be able to tell which form the workflow was written with; "+
			"before cleat#2976 only the parenthesised one produced a document at all.",
			bareSchema, parenSchema)
	}
}

func TestComputePythonEntryPointSchemaIsNonFatalOnAnUnimportableFile(t *testing.T) {
	skipIfNoCleatSDKPython(t)
	t.Setenv("PYTHONPATH", filepath.Join(repoRoot(t), "python-sdk"))

	// Deliberate syntax error. runBuildPython treats a schema-computation
	// failure as non-fatal and simply skips writing the sidecar -- this
	// pins the OTHER half of that contract: computePythonEntryPointSchema
	// itself returns an error rather than panicking or exiting the process.
	path := writePythonFixture(t, "def broken(:\n")

	if _, _, err := computePythonEntryPointSchema(path, "broken"); err == nil {
		t.Fatal("want an error for a file that cannot be imported, got nil")
	}
}

// TestComputePythonEntryPointSchemaCarriesTheExposureClass is cleat#1986's
// Python half: @cleat_entry(expose=...) has nowhere to go but this same
// sidecar (wasm.Metadata is barred from carrying per-entry-point data), so
// this is the other end of the round trip cleatctl deploy's own
// DeclaredExposureFromSchemas reads. Decoded with the REAL
// engine.EntryPointSchema type, not an ad-hoc struct, so a tag mismatch
// between the two sides of the JSON boundary fails here rather than only in
// a deploy test that happens to use the same type.
func TestComputePythonEntryPointSchemaCarriesTheExposureClass(t *testing.T) {
	skipIfNoCleatSDKPython(t)
	t.Setenv("PYTHONPATH", filepath.Join(repoRoot(t), "python-sdk"))

	declaredPath := writePythonFixture(t, `
from cleat_sdk.entry import cleat_entry
from cleat_sdk.host_calls import HostCalls


@cleat_entry("InventorySync", expose="internal")
def inventory_sync(h: HostCalls, item_id: str) -> str:
    return "{}"
`)
	name, schemaJSON, err := computePythonEntryPointSchema(declaredPath, "inventory_sync")
	if err != nil {
		t.Fatalf("computePythonEntryPointSchema: %v", err)
	}
	var decoded map[string]engine.EntryPointSchema
	if err := json.Unmarshal(schemaJSON, &decoded); err != nil {
		t.Fatalf("schemaJSON does not unmarshal as map[string]engine.EntryPointSchema: %v\n%s", err, schemaJSON)
	}
	if got := decoded[name].Exposure; got != engine.ExposureInternal {
		t.Errorf("Exposure = %q, want %q", got, engine.ExposureInternal)
	}

	// The control: an undeclared entry point must leave the field at its
	// zero value, not a literal "auth" -- engine.DeclaredExposureFromSchemas
	// treats "" as "no opinion from this entry" and `omitempty` on the Go
	// side depends on the same thing on the way out, so a round trip through
	// the real type is what proves the two conventions actually agree.
	undeclaredPath := writePythonFixture(t, `
from cleat_sdk.entry import cleat_entry
from cleat_sdk.host_calls import HostCalls


@cleat_entry
def plain_one(h: HostCalls, item_id: str) -> str:
    return "{}"
`)
	name, schemaJSON, err = computePythonEntryPointSchema(undeclaredPath, "plain_one")
	if err != nil {
		t.Fatalf("computePythonEntryPointSchema: %v", err)
	}
	decoded = nil
	if err := json.Unmarshal(schemaJSON, &decoded); err != nil {
		t.Fatalf("schemaJSON does not unmarshal: %v\n%s", err, schemaJSON)
	}
	if got := decoded[name].Exposure; got != "" {
		t.Errorf("Exposure = %q for an undeclared entry point, want \"\" (no declaration)", got)
	}
}
