package main

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

// TestDeployWorkflow_ReadsSchemaSidecar is the wiring test for cleat#1980's
// deploy-time half: `cleat build` writes <wasm-file>.schema.json next to the
// binary (cmd/cleat/main.go), and `cleatctl deploy workflow` must read it
// from the SAME path it was given on the command line -- not from any path
// internal to a build this process never ran.
func TestDeployWorkflow_ReadsSchemaSidecar(t *testing.T) {
	dir := t.TempDir()
	wasmBytes := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}
	path := writeWASM(t, dir, wasmBytes)
	schemaJSON := `{"greet":{"params":{"type":"string"},"result":{"type":"string"}}}`
	if err := os.WriteFile(path+".schema.json", []byte(schemaJSON), 0644); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}

	var capturedDef *engine.WorkflowDef
	store := &mockStore{
		listWorkflowDefsFn: func(_ context.Context, name string) ([]engine.WorkflowDef, error) {
			return nil, nil
		},
		deployWorkflowDefFn: func(_ context.Context, def *engine.WorkflowDef) error {
			capturedDef = def
			return nil
		},
	}

	deployWorkflow(context.Background(), store, nil, []string{"new-wf", path})

	if capturedDef == nil {
		t.Fatal("expected DeployWorkflowDef to be called")
	}
	got, ok := capturedDef.EntryPointSchemas["greet"]
	if !ok {
		t.Fatalf("EntryPointSchemas = %v, want a \"greet\" key", capturedDef.EntryPointSchemas)
	}
	if string(got.Params) != `{"type":"string"}` {
		t.Errorf("Params = %s, want {\"type\":\"string\"}", got.Params)
	}
}

// TestDeployWorkflow_NoSidecarIsNotAnError covers the common case: a build
// from before cleat#1980, or from a language internal/jsonschema has no
// emitter for, leaves no sidecar at all. Deploy must proceed exactly as it
// did before this feature existed, not refuse or warn.
func TestDeployWorkflow_NoSidecarIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	wasmBytes := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}
	path := writeWASM(t, dir, wasmBytes)

	var capturedDef *engine.WorkflowDef
	store := &mockStore{
		listWorkflowDefsFn: func(_ context.Context, name string) ([]engine.WorkflowDef, error) {
			return nil, nil
		},
		deployWorkflowDefFn: func(_ context.Context, def *engine.WorkflowDef) error {
			capturedDef = def
			return nil
		},
	}

	_, stderr := captureOutputs(t, func() {
		deployWorkflow(context.Background(), store, nil, []string{"new-wf", path})
	})

	if capturedDef == nil {
		t.Fatal("expected DeployWorkflowDef to be called")
	}
	if capturedDef.EntryPointSchemas != nil {
		t.Errorf("EntryPointSchemas = %v, want nil with no sidecar file", capturedDef.EntryPointSchemas)
	}
	if stderr != "" {
		t.Errorf("unexpected stderr for the ordinary no-sidecar case: %s", stderr)
	}
}

// TestDeployWorkflow_MalformedSidecarWarnsAndProceeds is the negative-control
// twin of the two tests above: a sidecar that exists but is not valid JSON
// must not abort the deploy (the WASM bytes are what the caller actually
// needs deployed), but silence here would be the same "operation reports
// success without doing the thing" failure this codebase's own CLAUDE.md
// warns about -- the warning is the only way to see that a schema never
// made it to workflow_defs.
func TestDeployWorkflow_MalformedSidecarWarnsAndProceeds(t *testing.T) {
	dir := t.TempDir()
	wasmBytes := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}
	path := writeWASM(t, dir, wasmBytes)
	if err := os.WriteFile(path+".schema.json", []byte("not json"), 0644); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}

	var capturedDef *engine.WorkflowDef
	store := &mockStore{
		listWorkflowDefsFn: func(_ context.Context, name string) ([]engine.WorkflowDef, error) {
			return nil, nil
		},
		deployWorkflowDefFn: func(_ context.Context, def *engine.WorkflowDef) error {
			capturedDef = def
			return nil
		},
	}

	_, stderr := captureOutputs(t, func() {
		deployWorkflow(context.Background(), store, nil, []string{"new-wf", path})
	})

	if capturedDef == nil {
		t.Fatal("expected DeployWorkflowDef to be called despite the malformed sidecar")
	}
	if capturedDef.EntryPointSchemas != nil {
		t.Errorf("EntryPointSchemas = %v, want nil when the sidecar cannot be parsed", capturedDef.EntryPointSchemas)
	}
	if !strings.Contains(stderr, "schema.json") {
		t.Errorf("expected a warning naming the sidecar in stderr, got: %q", stderr)
	}
}
