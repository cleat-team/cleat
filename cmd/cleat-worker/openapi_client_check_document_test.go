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

// TestEmitOpenAPIDocumentForTheGeneratedClientCheck writes the document that
// scripts/run-openapi-client-check.sh generates a TypeScript client from
// (cleat#2972).
//
// It runs only when both environment variables are set. That is a genuine
// environmental precondition, not a disabled test: a plain `go test ./...` has
// no fixture to read and no output path to write, so skipping is the correct
// outcome rather than a failure.
//
// # The boundary, stated here as well as in the script's header
//
// A reader of either file alone should not have to infer what is covered:
//
//   - EXERCISED: the real sidecar that `cleat build` writes (a map of
//     engine.EntryPointSchema keyed by WASM export name), read through the real
//     handler, out to the document the client is generated from.
//   - NOT EXERCISED: the sidecar READ in `cleatctl deploy`, the deploy itself,
//     and the HTTP routing registration. Those have their own tests. The
//     build->deploy JOIN is cleat#1980's acceptance item 1 and is deliberately
//     not claimed by this check.
func TestEmitOpenAPIDocumentForTheGeneratedClientCheck(t *testing.T) {
	sidecar := os.Getenv("CLEAT_OPENAPI_SCHEMA_SIDECAR")
	out := os.Getenv("CLEAT_OPENAPI_DOC_OUT")
	if sidecar == "" || out == "" {
		t.Skip("set CLEAT_OPENAPI_SCHEMA_SIDECAR and CLEAT_OPENAPI_DOC_OUT to emit the document")
	}

	raw, err := os.ReadFile(sidecar)
	if err != nil {
		t.Fatalf("reading the schema sidecar %s: %v", sidecar, err)
	}
	var schemas map[string]engine.EntryPointSchema
	if err := json.Unmarshal(raw, &schemas); err != nil {
		t.Fatalf("%s is not a valid entry-point schema sidecar: %v", sidecar, err)
	}
	// A sidecar with no schemas would make every assertion downstream vacuous:
	// the document would have no paths, and "the misspelled call failed to
	// compile" would be true of a client generated from nothing.
	if len(schemas) == 0 {
		t.Fatalf("%s carries no entry-point schemas -- the generated client would be checked against nothing", sidecar)
	}

	name := os.Getenv("CLEAT_OPENAPI_WORKFLOW")
	if name == "" {
		name = "optionalparam"
	}

	st := &mockStore{listWorkflowDefsFn: func(_ context.Context, _ string) ([]engine.WorkflowDef, error) {
		return []engine.WorkflowDef{{Name: name, Version: 1, EntryPointSchemas: schemas}}, nil
	}}
	resp := serveOpenAPI(t, newTestAPIServer(st), http.MethodGet)
	if resp.Code != http.StatusOK {
		t.Fatalf("GET /api/openapi.json = %d: %s", resp.Code, resp.Body.String())
	}
	if err := os.WriteFile(out, resp.Body.Bytes(), 0o644); err != nil {
		t.Fatalf("writing the document to %s: %v", out, err)
	}
	t.Logf("wrote %s (%d bytes) for workflow %q, entry points %v", out, resp.Body.Len(), name, schemaKeys(schemas))
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
