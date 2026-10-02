package main

// cleat#1980 acceptance item 1: the build -> deploy JOIN.
//
// WHAT IS ALREADY COVERED, so this file's job is narrow and stated rather than
// assumed (all four verified by reading the tree, 2026-10-02):
//
//   - the Go writer half: cmd/cleat-worker/a_real_fixture_validates_start_input_test.go
//     runs the real `cleat build`, locates the `.wasm.schema.json` and FAILS if it
//     is absent -- not a skip.
//   - the Python writer half: a_real_python_fixture_validates_start_input_test.go,
//     the same shape for `--target python`.
//   - the reader half: cmd/cleatctl/deploy_entry_point_schemas_test.go -- but it
//     HAND-WRITES the sidecar it then reads, so it supplies BOTH sides of the
//     boundary. It proves the reader reads SOME path; it cannot prove the two
//     sides agree on WHICH path.
//
// WHAT NOTHING CROSSES, and the whole reason this test exists:
//
// The reader derives its path as `wasmPath + ".schema.json"` (cmd/cleatctl/deploy.go).
// The writer-side tests match the sidecar by SUFFIX, after SCANNING the output
// directory:
//
//	case strings.HasSuffix(e.Name(), ".wasm.schema.json"):
//
// Nothing compares those two. A build that wrote `foo.wasm` beside
// `bar.wasm.schema.json` satisfies all three existing tests and then deploys with
// no schemas at all.
//
// WHY THAT MATTERS RATHER THAN BEING A NIT. deploy.go's read is
//
//	if schemaBytes, err := os.ReadFile(wasmPath + ".schema.json"); err == nil {
//
// and main.go's own comment states the contract in words -- absence means "an
// older build, or a language internal/jsonschema has no emitter for yet". So a
// naming disagreement is INDISTINGUISHABLE FROM A PRE-#1980 BUILD: deploy
// succeeds, the definition is stored carrying no schemas, and POST /start then
// validates nothing. The check's green and the absence agree.
//
// AND IT IS NOT HYPOTHETICAL, WHICH IS WHY THIS IS NOT A TIDY-UP. cleat#2976
// shipped `cleat build --target python` reporting Build SUCCESS while writing NO
// sidecar at all, past a writer-side assertion that was already in place and
// passing -- because that assertion's fixture happened to use the parenthesised
// `@cleat_entry("name")` form and the defect was in the bare form. An assertion's
// effective coverage is set by incidental properties of its fixture.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
)

// The fixture and its entry point. `testdata/bindingconformance` is the fixture
// the worker-side writer assertion already uses, and BindInt(h, count int) is the
// entry point whose emitted params it asserts -- so the two tests agree on what
// the build is supposed to produce.
const (
	joinFixture    = "testdata/bindingconformance"
	joinEntryPoint = "bind_int"
	joinWasmName   = "workflow.wasm"
)

func TestTheSidecarCleatBuildWritesIsTheOneDeployReads(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles a WASM module with the real toolchain; skipped in short mode")
	}
	ctx := context.Background()

	// The real store, built the way cleatctl's own main builds it, so this tests
	// the production wiring rather than an invention of the test's.
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
	cleatBuildInto(t, outDir, joinFixture)

	wasmPath := filepath.Join(outDir, joinWasmName)
	sidecarPath := wasmPath + ".schema.json"

	if _, err := os.Stat(wasmPath); err != nil {
		t.Fatalf("cleat build produced no %s: %v", wasmPath, err)
	}

	// --- 2. THE ASSERTION THE EXISTING TESTS CANNOT MAKE -------------------
	//
	// At the reader's own derivation, not by scanning for a suffix. If this fails
	// while the writer-side tests pass, the naming is what disagrees -- which is
	// exactly the defect those tests are blind to.
	if _, err := os.Stat(sidecarPath); err != nil {
		t.Fatalf("cleat build wrote no sidecar AT THE PATH DEPLOY DERIVES (%s): %v\n\n"+
			"deploy.go reads exactly wasmPath+\".schema.json\", and it treats a missing sidecar "+
			"as benign, so this disagreement is indistinguishable from a pre-#1980 build: the "+
			"deploy would report success and store no schemas.", sidecarPath, err)
	}

	// And exactly one sidecar claims to be this build's: a suffix scan cannot tell
	// `workflow.wasm.schema.json` from `other.wasm.schema.json`, which is how the
	// slack above stays invisible.
	if got := schemaFilesIn(t, outDir); len(got) != 1 || got[0] != filepath.Base(sidecarPath) {
		t.Fatalf("sidecars in the build output = %v, want exactly [%s]", got, filepath.Base(sidecarPath))
	}

	// The sidecar must carry the entry point the emitter was supposed to produce.
	raw, err := os.ReadFile(sidecarPath)
	if err != nil {
		t.Fatalf("reading %s: %v", sidecarPath, err)
	}
	var schemas map[string]engine.EntryPointSchema
	if err := json.Unmarshal(raw, &schemas); err != nil {
		t.Fatalf("%s is not a valid entry-point schema sidecar: %v", sidecarPath, err)
	}
	if _, ok := schemas[joinEntryPoint]; !ok {
		t.Fatalf("sidecar carries %v, want %q -- the join below would be vacuous", schemaNames(schemas), joinEntryPoint)
	}

	// --- 3. the join: deploy the REAL build output and read it back --------
	// Both names are unique per run, and testutil's suite cleanup wipes
	// workflow_defs between tests (it is in postgresCleanupTables), so the rows
	// this leaves are the harness's to remove rather than this test's.
	name := fmt.Sprintf("cleatctl-sidecar-join-%d", time.Now().UnixNano())

	if _, stderr := captureOutputs(t, func() {
		deployWorkflow(ctx, store, db, []string{name, wasmPath})
	}); stderr != "" {
		t.Fatalf("deploy of a real build reported an error: %s", stderr)
	}

	defs, err := store.ListWorkflowDefs(ctx, name)
	if err != nil {
		t.Fatalf("ListWorkflowDefs(%q): %v", name, err)
	}
	if len(defs) == 0 {
		t.Fatalf("the deploy reported success and stored nothing for %q", name)
	}
	got, ok := defs[0].EntryPointSchemas[joinEntryPoint]
	if !ok {
		t.Fatalf("THE JOIN FAILED: the definition was stored carrying no %q schema.\n"+
			"Stored entry points: %v\n\n"+
			"deploy read %s and found nothing there, which is the same state a naming "+
			"disagreement produces -- and the same state a pre-#1980 build produces, which is "+
			"why nothing else notices it.", joinEntryPoint, schemaNames(defs[0].EntryPointSchemas), sidecarPath)
	}
	if !strings.Contains(string(got.Params), `"count"`) {
		t.Errorf("stored params = %s, want a schema mentioning the fixture's `count` parameter", got.Params)
	}

	// --- 4. the negative control: the ABSENCE branch ------------------------
	//
	// Deliberately the absence branch rather than a malformed sidecar, because
	// absence is the state a naming mismatch produces -- a malformed sidecar takes
	// a branch (deploy warns and continues) that this failure never reaches.
	//
	// ONE BUILD, TWO CASES: the control reuses the same artifact with the sidecar
	// removed, rather than compiling the fixture a second time.
	ctlDir := t.TempDir()
	wasmBytes, err := os.ReadFile(wasmPath)
	if err != nil {
		t.Fatalf("reading %s: %v", wasmPath, err)
	}
	ctlWasm := filepath.Join(ctlDir, joinWasmName)
	if err := os.WriteFile(ctlWasm, wasmBytes, 0o644); err != nil {
		t.Fatalf("writing the control's wasm: %v", err)
	}
	if _, err := os.Stat(ctlWasm + ".schema.json"); err == nil {
		t.Fatalf("the control directory already has a sidecar; it must have none")
	}

	ctlName := fmt.Sprintf("cleatctl-sidecar-absent-%d", time.Now().UnixNano())

	if _, stderr := captureOutputs(t, func() {
		deployWorkflow(ctx, store, db, []string{ctlName, ctlWasm})
	}); stderr != "" {
		t.Fatalf("deploying a build with NO sidecar reported an error; deploy.go documents "+
			"its absence as benign, so a refusal here would be a behaviour change: %s", stderr)
	}
	ctlDefs, err := store.ListWorkflowDefs(ctx, ctlName)
	if err != nil {
		t.Fatalf("ListWorkflowDefs(%q): %v", ctlName, err)
	}
	if len(ctlDefs) == 0 {
		t.Fatalf("the control deploy reported success and stored nothing for %q", ctlName)
	}
	if n := len(ctlDefs[0].EntryPointSchemas); n != 0 {
		t.Errorf("a build with NO sidecar stored %d schema(s) (%v); the absence branch must "+
			"store none -- this is the state a naming mismatch produces, so a pass here is "+
			"what tells step 3's success apart from the control", n, schemaNames(ctlDefs[0].EntryPointSchemas))
	}
}

// cleatBuildInto runs the real `cleat build` over a repo-relative fixture into
// outDir. It shells out exactly as the worker-side writer assertions do, so this
// test exercises the build the way production invokes it.
func cleatBuildInto(t *testing.T, outDir, fixture string) {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if resolved, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = resolved
	}
	root := filepath.Dir(filepath.Dir(cwd)) // cmd/cleatctl -> repo root

	cmd := exec.Command("go", "run", filepath.Join(root, "cmd", "cleat"),
		"build", "--target", "go", "-o", outDir, filepath.Join(root, fixture))
	cmd.Dir = root
	cmd.Env = os.Environ()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cleat build failed:\n%s\n%v", string(out), err)
	}
}

func schemaFilesIn(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	var out []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".schema.json") {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

func schemaNames(m map[string]engine.EntryPointSchema) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
