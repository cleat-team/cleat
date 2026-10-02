package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
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
