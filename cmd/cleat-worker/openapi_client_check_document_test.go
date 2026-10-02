package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"sort"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

// TestEmitOpenAPIDocumentForTheGeneratedClientCheck drives the handler that
// serves /api/openapi.json, and -- when pointed at a real one -- writes the
// document that scripts/run-openapi-client-check.sh generates a TypeScript
// client from (cleat#2972).
//
// # It never skips, and that is a Tier 1 requirement rather than a preference
//
// scripts/tier-gate.sh counts a skip in a tier-1 package as a FAILURE: "either
// the precondition belongs in tier 1 and must be installed, or the test belongs
// in tier 2. Do not widen this gate to accommodate it." This test is in tier 1,
// and the real sidecar cannot be installed there -- producing one needs `cleat
// build`, node and a compiled fixture, which is the dedicated
// .github/workflows/openapi-client.yml's job, not a unit-test package's.
//
// So the precondition is installed INSIDE the test rather than tolerated. With
// no environment set it drives the real handler with an in-repo entry-point
// schema and asserts the document that comes back; that is the same handler and
// the same code path, and only the schema's origin differs. Setting both
// variables upgrades it to the real `cleat build` sidecar and writes the
// document out for the client generator to consume.
//
// An earlier revision t.Skip'd here, which failed `Tier 1 Gate` -- a required
// context -- on every run. A skip declared in scripts/skip-ledger.d/ satisfies
// check-skip-budget.sh and NOT tier-gate.sh: the two guards answer different
// questions, and only the second one is a merge gate.
//
// # The boundary, stated here as well as in the script's header
//
// A reader of either file alone should not have to infer what is covered:
//
//   - EXERCISED: a sidecar shaped exactly like the one `cleat build` writes (a
//     map of engine.EntryPointSchema keyed by WASM export name), read through
//     the real handler, out to the document the client is generated from.
//   - NOT EXERCISED: the sidecar READ in `cleatctl deploy`, the deploy itself,
//     and the HTTP routing registration. Those have their own tests. The
//     build->deploy JOIN is cleat#1980's acceptance item 1 and is deliberately
//     not claimed by this check.
func TestEmitOpenAPIDocumentForTheGeneratedClientCheck(t *testing.T) {
	sidecar := os.Getenv("CLEAT_OPENAPI_SCHEMA_SIDECAR")
	out := os.Getenv("CLEAT_OPENAPI_DOC_OUT")
	if (sidecar == "") != (out == "") {
		t.Fatalf("CLEAT_OPENAPI_SCHEMA_SIDECAR and CLEAT_OPENAPI_DOC_OUT must be set together "+
			"(sidecar=%q out=%q); one without the other is a misconfiguration, not a mode",
			sidecar, out)
	}

	name := os.Getenv("CLEAT_OPENAPI_WORKFLOW")
	if name == "" {
		name = "optionalparam"
	}

	// The built-in fixture, used whenever no sidecar was supplied. One required
	// field, so the document that comes back has something to assert on. It is
	// the shape internal/jsonschema emits for ApplyCoupon(h, userID string,
	// promo *Coupon): a required parameter and a nullable one.
	schemas := map[string]engine.EntryPointSchema{
		"apply_coupon": paramsOf(`{"type":"object","properties":{"userID":{"type":"string"},"promo":{"type":["object","null"]}},"required":["userID"],"additionalProperties":true}`),
	}

	if sidecar != "" {
		raw, err := os.ReadFile(sidecar)
		if err != nil {
			t.Fatalf("reading the schema sidecar %s: %v", sidecar, err)
		}
		if err := json.Unmarshal(raw, &schemas); err != nil {
			t.Fatalf("%s is not a valid entry-point schema sidecar: %v", sidecar, err)
		}
		// A sidecar with no schemas would make every assertion below vacuous:
		// the document would have no paths, and "the misspelled call failed to
		// compile" would be true of a client generated from nothing.
		if len(schemas) == 0 {
			t.Fatalf("%s carries no entry-point schemas -- the generated client would be checked against nothing", sidecar)
		}
	}

	st := &mockStore{listWorkflowDefsFn: func(_ context.Context, _ string) ([]engine.WorkflowDef, error) {
		return []engine.WorkflowDef{{Name: name, Version: 1, EntryPointSchemas: schemas}}, nil
	}}
	resp := serveOpenAPI(t, newTestAPIServer(st), http.MethodGet)
	if resp.Code != http.StatusOK {
		t.Fatalf("GET /api/openapi.json = %d: %s", resp.Code, resp.Body.String())
	}

	// Assert the document is non-trivial BEFORE writing it out. Without this the
	// emit branch could write a 200 with an empty paths object and report
	// success -- the generator would then produce an empty client, and the
	// negative control would "fail to compile" for the wrong reason, which is a
	// green over nothing.
	var doc map[string]any
	if err := json.Unmarshal(resp.Body.Bytes(), &doc); err != nil {
		t.Fatalf("the document did not parse: %v: %s", err, resp.Body.String())
	}
	paths, ok := doc["paths"].(map[string]any)
	if !ok {
		t.Fatalf("paths is %T, not an object: %s", doc["paths"], resp.Body.String())
	}
	// buildOpenAPIDocument publishes one path per workflow name, keyed by the
	// name verbatim, so this is exact rather than a prefix match.
	want := "/api/workflows/" + name + "/start"
	if _, present := paths[want]; !present {
		t.Fatalf("the document is missing %s, so there is nothing for a client to be generated from: %s", want, resp.Body.String())
	}
	if len(paths) != 1 {
		t.Errorf("document has %d path(s) for a single definition, want 1: %v", len(paths), paths)
	}

	if out != "" {
		if err := os.WriteFile(out, resp.Body.Bytes(), 0o644); err != nil {
			t.Fatalf("writing the document to %s: %v", out, err)
		}
		t.Logf("wrote %s (%d bytes) for workflow %q, entry points %v", out, resp.Body.Len(), name, schemaKeys(schemas))
	}
}

// schemaKeys is named for the schema map rather than as a generic sortedKeys:
// the package already has a sortedKeys for map[string]int, and a second one
// under the same name does not compile. Not worth unifying -- they sort
// different types.
func schemaKeys(m map[string]engine.EntryPointSchema) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
