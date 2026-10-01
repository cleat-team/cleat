package main

import (
	"context"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

// TestDeployWorkflow_NoValidateInputFlag is the wiring test for cleat#1981's
// escape hatch: `cleatctl deploy workflow <name> <wasm-file> --no-validate-input`
// must set WorkflowDef.InputValidationDisabled, and the flag must parse
// whether it appears before or after the positional arguments (parseFlagsAnywhere,
// cleat#1933 -- the same reason every other cleatctl subcommand's flags go
// through it rather than a hand-rolled loop).
func TestDeployWorkflow_NoValidateInputFlag(t *testing.T) {
	dir := t.TempDir()
	wasmBytes := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}
	path := writeWASM(t, dir, wasmBytes)

	for _, args := range [][]string{
		{"new-wf", path, "--no-validate-input"},
		{"--no-validate-input", "new-wf", path},
	} {
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

		deployWorkflow(context.Background(), store, nil, args)

		if capturedDef == nil {
			t.Fatalf("args %v: expected DeployWorkflowDef to be called", args)
		}
		if !capturedDef.InputValidationDisabled {
			t.Errorf("args %v: InputValidationDisabled = false, want true", args)
		}
	}
}

// TestDeployWorkflow_ValidationEnabledByDefault is the negative control:
// without the flag, the owner decision on cleat#1981 ("on by default for
// definitions that carry a schema") must not be defeated by this column's
// own zero value happening to agree with it on every OTHER path -- a
// regression that flipped the flag's default would still pass
// TestDeployWorkflow_NoValidateInputFlag above.
func TestDeployWorkflow_ValidationEnabledByDefault(t *testing.T) {
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

	deployWorkflow(context.Background(), store, nil, []string{"new-wf", path})

	if capturedDef == nil {
		t.Fatal("expected DeployWorkflowDef to be called")
	}
	if capturedDef.InputValidationDisabled {
		t.Error("InputValidationDisabled = true without the flag, want false")
	}
}
