package main

// cleat#1980 acceptance item 1, the PYTHON half:
//
//	"A Go and a Python fixture workflow each deploy with a schema."
//
// The Go half is the sibling file's job (cleat#2991): a real `cleat build`,
// deployed through the production store wiring, read back. This is the same
// join for the other language, and it was the half nothing covered --
// measured rather than assumed:
//
//	git grep -ln "pytyped" origin/develop -- '*_test.go'
//	#   cmd/cleat-worker/a_real_python_fixture_validates_start_input_test.go   (one file)
//	git grep -c "deployWorkflow" origin/develop -- \
//	  cmd/cleat-worker/a_real_python_fixture_validates_start_input_test.go
//	#   0
//
// So no test deployed a Python fixture. The sole user of the fixture builds it
// and drives the start handler with a MOCKED store, which covers the emitter
// and the validation path but never `cleatctl deploy`'s read of the sidecar.
//
// # Why this cannot be evidenced with a synthetic fixture
//
// The property this item names is WHICH LANGUAGE the definition came from, and
// a synthetic `typedDef(...)` cannot express it by construction -- there is no
// build behind it to have emitted, or failed to emit, a sidecar. That is the
// same fixture-scope shape recorded on cleat#1980's status comment: a test's
// effective coverage is set by its fixture, not by the acceptance item.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
)

// The Python fixture and its entry point. It is built from the .py FILE rather
// than the directory, because that is how `cleat build --target python` takes a
// Python workflow.
const (
	pythonFixture    = "testdata/pytyped/count_workflow.py"
	pythonEntryPoint = "count_workflow"
)

// requireToolchainEnv mirrors the constant of the same name in
// cmd/cleat-worker/a_real_python_fixture_validates_start_input_test.go and in
// engine/rust_workflow_test.go. A copy rather than an import: those live in
// other packages' test files, which package main cannot reach. The VALUE is the
// contract, and all three must agree.
const requireToolchainEnv = "CLEAT_REQUIRE_TOOLCHAINS"

// toolchainRequired reports whether this job declared that it provides the
// named toolchain. When it did, a missing toolchain is a FAILURE rather than a
// skip: a job that promised python and silently skipped would report green
// while measuring nothing.
func toolchainRequired(name string) bool {
	for _, want := range strings.Split(os.Getenv(requireToolchainEnv), ",") {
		if strings.TrimSpace(want) == name {
			return true
		}
	}
	return false
}

// pythonWASMUnavailable returns a reason if this machine cannot build a Python
// workflow, or "" if it can.
func pythonWASMUnavailable() string {
	if _, err := exec.LookPath("componentize-py"); err != nil {
		return "componentize-py is not on PATH (pip install componentize-py)"
	}
	if _, err := exec.LookPath("python3"); err != nil {
		return "python3 is not on PATH"
	}
	return ""
}

// TestAPythonFixtureWorkflowDeploysWithASchema is cleat#1980 item 1's Python
// half: a real `cleat build --target python`, deployed through the same
// production wiring the Go test uses, with the schema read back out of the
// store.
func TestAPythonFixtureWorkflowDeploysWithASchema(t *testing.T) {
	if reason := pythonWASMUnavailable(); reason != "" {
		if toolchainRequired("python") {
			t.Fatalf("cannot build a Python workflow, but %s declares python, so this job "+
				"installs componentize-py and treats Python as tier 1: %s",
				requireToolchainEnv, reason)
		}
		t.Skip("cannot build a Python workflow: " + reason)
	}
	if testing.Short() {
		t.Skip("compiles a WASM module with the real toolchain; skipped in short mode")
	}
	ctx := context.Background()

	// The real store, built the way cleatctl's own main builds it.
	db := testutil.SuiteTestDB(t, "cleatctl")
	factory, err := dialectPostgres.openStoreFactory(db, "", "public")
	if err != nil {
		t.Fatalf("openStoreFactory: %v", err)
	}
	store, closer, err := factory.OpenStore(ctx, defaultTenantID)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer func() { _ = closer.Close() }()

	// --- 1. the real build -------------------------------------------------
	outDir := t.TempDir()
	cleatBuildInto(t, outDir, pythonFixture, "python")

	// The reader's derivation, not a suffix scan: the build must have written
	// the sidecar where `deploy.go` will look for it.
	wasmPath := soleWasmIn(t, outDir)
	sidecarPath := wasmPath + ".schema.json"
	if _, err := os.Stat(sidecarPath); err != nil {
		t.Fatalf("cleat build --target python wrote no sidecar at the path deploy derives "+
			"(%s): %v\n\nA Python build whose emitter declines or crashes writes NO sidecar "+
			"and still reports Build SUCCESS, so this is the state cleat#2976 shipped.",
			sidecarPath, err)
	}

	raw, err := os.ReadFile(sidecarPath)
	if err != nil {
		t.Fatalf("reading %s: %v", sidecarPath, err)
	}
	var schemas map[string]engine.EntryPointSchema
	if err := json.Unmarshal(raw, &schemas); err != nil {
		t.Fatalf("%s is not a valid entry-point schema sidecar: %v", sidecarPath, err)
	}
	if _, ok := schemas[pythonEntryPoint]; !ok {
		t.Fatalf("the Python build's sidecar carries %v, want %q -- the deploy below would "+
			"be vacuous", schemaNames(schemas), pythonEntryPoint)
	}

	// --- 2. the join: deploy the REAL build output and read it back --------
	name := fmt.Sprintf("cleatctl-pysidecar-%d", time.Now().UnixNano())
	if _, stderr := captureOutputs(t, func() {
		deployWorkflow(ctx, store, db, []string{name, wasmPath})
	}); stderr != "" {
		t.Fatalf("deploy of a real Python build reported an error: %s", stderr)
	}

	defs, err := store.ListWorkflowDefs(ctx, name)
	if err != nil {
		t.Fatalf("ListWorkflowDefs(%q): %v", name, err)
	}
	if len(defs) == 0 {
		t.Fatalf("the deploy reported success and stored nothing for %q", name)
	}
	got, ok := defs[0].EntryPointSchemas[pythonEntryPoint]
	if !ok {
		t.Fatalf("THE JOIN FAILED FOR PYTHON: the definition was stored carrying no %q schema.\n"+
			"Stored entry points: %v\n\ndeploy read %s and found nothing there. This is the "+
			"half of cleat#1980's item 1 that a mocked store in cmd/cleat-worker cannot "+
			"reach: that test supplies the schema to the handler directly and never goes "+
			"through the deploy read at all.", pythonEntryPoint, schemaNames(defs[0].EntryPointSchemas), sidecarPath)
	}
	// The fixture's entry point takes typed parameters; an empty or trivial
	// schema here would mean the emitter produced something deploy could store
	// but nothing could validate against.
	if len(got.Params) == 0 {
		t.Errorf("the stored Python schema carries no params: %s", string(got.Params))
	}
}
