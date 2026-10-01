package main

import (
	"encoding/json"
	"testing"

	"github.com/cleat-team/cleat/internal/analyzer"
)

// TestBuildEntryPointSchemasKeysByWASMExportName pins the keying choice:
// exportedEntryPointNames (used for wasm.Metadata.EntryPoints) and
// buildEntryPointSchemas must key by the SAME name -- the WASM export name,
// snake_case -- so a caller resolving one can look the other up without a
// second translation. testdata/hello.Greet's export name is "greet", not
// "Greet".
func TestBuildEntryPointSchemasKeysByWASMExportName(t *testing.T) {
	result, _, _, _, _, _ := analyze("./../../testdata/hello")
	schemas := buildEntryPointSchemas(result)

	if _, ok := schemas["greet"]; !ok {
		t.Fatalf("schemas = %v, want a \"greet\" key (exportedEntryPointNames' own naming)", schemas)
	}
	if _, ok := schemas["Greet"]; ok {
		t.Errorf("schemas carries the Go source name \"Greet\" as a key -- it must be the export name only")
	}
}

// TestBuildEntryPointSchemasMatchesTheJSONSchemaPackage cross-checks against
// internal/jsonschema's own tests on the same fixture (optionalparam.ApplyCoupon,
// cleat#1065's pointer-optional fixture) rather than re-asserting the schema
// shape here -- this test's job is the WIRING (does buildEntryPointSchemas
// reach the right function with the right arguments), not re-deriving what a
// correct schema looks like.
func TestBuildEntryPointSchemasMatchesTheJSONSchemaPackage(t *testing.T) {
	result, _, _, _, _, _ := analyze("./../../testdata/optionalparam")
	schemas := buildEntryPointSchemas(result)

	got, ok := schemas["apply_coupon"]
	if !ok {
		t.Fatalf("schemas = %v, want an \"apply_coupon\" key", schemas)
	}

	var params map[string]any
	if err := json.Unmarshal(got.Params, &params); err != nil {
		t.Fatalf("Params is not valid JSON: %v (%s)", err, got.Params)
	}
	required, _ := params["required"].([]any)
	if len(required) != 1 || required[0] != "userID" {
		t.Errorf("params.required = %#v, want exactly [\"userID\"] -- promo is a pointer parameter", required)
	}

	var resultSchema map[string]any
	if err := json.Unmarshal(got.Result, &resultSchema); err != nil {
		t.Fatalf("Result is not valid JSON: %v (%s)", err, got.Result)
	}
	if resultSchema["type"] != "string" {
		t.Errorf("result schema = %#v, want {\"type\":\"string\"} -- ApplyCoupon returns (string, error)", resultSchema)
	}
}

// TestBuildEntryPointSchemasZeroParamEntryPointStillGetsAKey covers
// testdata/noargs.Cleanup -- the zero-parameter fixture internal/jsonschema's
// own tests use -- so the "no entry points found" and "an entry point with
// no parameters" cases are not conflated: a zero-field schema is still a
// present map entry, not an absent one.
func TestBuildEntryPointSchemasZeroParamEntryPointStillGetsAKey(t *testing.T) {
	result, _, _, _, _, _ := analyze("./../../testdata/noargs")
	schemas := buildEntryPointSchemas(result)
	if _, ok := schemas["cleanup"]; !ok {
		t.Fatalf("schemas = %v, want a \"cleanup\" key -- a zero-parameter entry point is still an entry point", schemas)
	}
}

// TestBuildEntryPointSchemasNilWhenNoTargetPackage is the one guard clause in
// buildEntryPointSchemas with no fixture that reaches it naturally -- every
// real analyze() result has a TargetPkg, so this exercises it directly rather
// than leaving it unverified.
func TestBuildEntryPointSchemasNilWhenNoTargetPackage(t *testing.T) {
	result := &analyzer.AnalysisResult{}
	if got := buildEntryPointSchemas(result); got != nil {
		t.Errorf("got %v, want nil for a result with no TargetPkg", got)
	}
}
