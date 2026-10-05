package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
)

// TestDeployReadsSchemaSidecar is the wiring test for cleat#3150: `cleat
// deploy` (unlike `cleatctl deploy workflow`, which already did this) never
// read the `<wasm>.schema.json` sidecar `cleat build` writes
// (entrypoint_schemas.go's buildEntryPointSchemas), so every workflow
// deployed the way docs/tutorials/quick-start.md's step 6 tells a reader to
// landed with an empty workflow_defs.entry_point_schemas and was invisible to
// GET /api/openapi.json. This asserts the column itself, through a real
// Postgres connection, rather than only that `cleat deploy` exits 0 -- the
// old defect also exited 0.
func TestDeployReadsSchemaSidecar(t *testing.T) {
	if testing.Short() || cleatBinary == "" {
		t.Skip("needs the cleat binary, which TestMain does not build in short mode")
	}
	db := testutil.TestDB(t, testutil.DialectPostgres)

	const wf = "deploy-schema-sidecar-wf"
	wasmPath := writeFakeWasm(t, t.TempDir(), "wf.wasm")
	schemaJSON := `{"greet":{"params":{"type":"string"},"result":{"type":"string"}}}`
	if err := os.WriteFile(wasmPath+".schema.json", []byte(schemaJSON), 0o644); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}

	cmd := exec.Command(cleatBinary, "deploy", "--db", testutil.PostgresTestDSN(), "--name", wf, wasmPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cleat deploy: %v\n%s", err, out)
	}

	var got []byte
	if err := db.QueryRow(
		`SELECT entry_point_schemas FROM workflow_defs WHERE name = $1`, wf,
	).Scan(&got); err != nil {
		t.Fatalf("no workflow_defs row for %q, or entry_point_schemas unreadable: %v", wf, err)
	}
	var decoded map[string]struct {
		Params json.RawMessage `json:"params"`
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatalf("entry_point_schemas is not valid JSON: %v (%s)", err, got)
	}
	entry, ok := decoded["greet"]
	if !ok {
		t.Fatalf("entry_point_schemas = %s, want a %q key", got, "greet")
	}
	// Compared by decoded VALUE, not by byte string: PostgreSQL's jsonb column
	// reformats what it stores (`{"type": "string"}`, with a space), so a raw
	// `got != want` would fail on round-tripping through JSONB, not on anything
	// this fix is responsible for.
	var paramsType struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(entry.Params, &paramsType); err != nil {
		t.Fatalf("greet.params = %s, not valid JSON: %v", entry.Params, err)
	}
	if paramsType.Type != "string" {
		t.Errorf("greet.params.type = %q, want %q (full value: %s)", paramsType.Type, "string", entry.Params)
	}
}

// TestDeployWithNoSidecarLeavesSchemasNull covers the common case: a build
// from before cleat#1980, or from a language internal/jsonschema has no
// emitter for yet, writes no sidecar at all. Deploy must proceed exactly as
// it did before this fix, not refuse or warn.
func TestDeployWithNoSidecarLeavesSchemasNull(t *testing.T) {
	if testing.Short() || cleatBinary == "" {
		t.Skip("needs the cleat binary, which TestMain does not build in short mode")
	}
	db := testutil.TestDB(t, testutil.DialectPostgres)

	const wf = "deploy-no-sidecar-wf"
	wasmPath := writeFakeWasm(t, t.TempDir(), "wf.wasm")

	cmd := exec.Command(cleatBinary, "deploy", "--db", testutil.PostgresTestDSN(), "--name", wf, wasmPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cleat deploy: %v\n%s", err, out)
	}

	var got []byte
	if err := db.QueryRow(
		`SELECT entry_point_schemas FROM workflow_defs WHERE name = $1`, wf,
	).Scan(&got); err != nil {
		t.Fatalf("no workflow_defs row for %q: %v", wf, err)
	}
	if got != nil {
		t.Errorf("entry_point_schemas = %s, want NULL with no sidecar file", got)
	}
}
