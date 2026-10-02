package main

import (
	"context"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

// cleat#1986, the deploy half of slice 2a: `--exposure` is the only way to set
// a definition's exposure class today (the source-level declaration is a later
// slice), and `public` is REFUSED rather than stored.
//
// Driven through deployWorkflow directly with a nil *sql.DB, which is what the
// neighbouring deploy tests do -- the `--db` / CLEAT_DB_URL check happens in the
// command dispatcher BEFORE this function, so a `cleatctl deploy ...` invocation
// with a bad --exposure exits on the missing database and never reaches the
// validation. Measured the hard way: a `go run` of exactly that printed the
// database error, which reads like the validation passing when it means the
// validation never ran.

const bareWASMExposureTest = "\x00asm\x01\x00\x00\x00"

func deploymentFor(t *testing.T, args ...string) *engine.WorkflowDef {
	t.Helper()
	dir := t.TempDir()
	path := writeWASM(t, dir, []byte(bareWASMExposureTest))

	var captured *engine.WorkflowDef
	store := &mockStore{
		deployWorkflowDefFn: func(_ context.Context, def *engine.WorkflowDef) error {
			captured = def
			return nil
		},
	}
	withExitPanic(t, func() {
		deployWorkflow(context.Background(), store, nil, append([]string{"exposure-wf", path}, args...))
	})
	if captured == nil {
		t.Fatalf("DeployWorkflowDef was never called for args %v", args)
	}
	return captured
}

// The default is the class whose meaning is "what the server already does", so a
// deploy that says nothing changes nothing.
func TestDeployWorkflowDefaultsToAuthExposure(t *testing.T) {
	if got := deploymentFor(t).Exposure; got != engine.ExposureAuth {
		t.Errorf("Exposure = %q, want %q for a deploy with no --exposure", got, engine.ExposureAuth)
	}
}

// The flag reaches the definition -- asserted on the struct the store is handed,
// not on the flag variable, so a wiring break between them is visible.
func TestDeployWorkflowCarriesTheRequestedExposure(t *testing.T) {
	if got := deploymentFor(t, "--exposure", "internal").Exposure; got != engine.ExposureInternal {
		t.Errorf("Exposure = %q, want %q -- --exposure did not reach the definition", got, engine.ExposureInternal)
	}
}

// 'public' is refused, and the refusal must NAME what is missing. A definition
// stored as public today becomes world-readable the moment enforcement lands,
// so accepting it would be a time bomb rather than a permissive default.
func TestDeployWorkflowRefusesPublicExposure(t *testing.T) {
	dir := t.TempDir()
	path := writeWASM(t, dir, []byte(bareWASMExposureTest))

	deployed := false
	store := &mockStore{
		deployWorkflowDefFn: func(_ context.Context, _ *engine.WorkflowDef) error {
			deployed = true
			return nil
		},
	}
	stderr := withExitPanic(t, func() {
		deployWorkflow(context.Background(), store, nil, []string{"wf", path, "--exposure", "public"})
	})
	if deployed {
		t.Error("the definition was deployed despite --exposure public being refused")
	}
	if !strings.Contains(stderr, "opt-in") {
		t.Errorf("the refusal does not name the missing per-tenant opt-in, so a reader cannot tell why:\n%s", stderr)
	}
}

// An unknown class is refused too, and the message lists the accepted values --
// otherwise the database's CHECK rejects it later with a message about a
// constraint rather than about the flag the user typed.
func TestDeployWorkflowRefusesAnUnknownExposure(t *testing.T) {
	dir := t.TempDir()
	path := writeWASM(t, dir, []byte(bareWASMExposureTest))

	stderr := withExitPanic(t, func() {
		deployWorkflow(context.Background(), &mockStore{}, nil, []string{"wf", path, "--exposure", "sideways"})
	})
	for _, want := range []string{"auth", "public", "internal"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("the refusal does not list the accepted value %q:\n%s", want, stderr)
		}
	}
}
