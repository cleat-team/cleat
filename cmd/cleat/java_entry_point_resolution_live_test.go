// cleat#2145 (owner decision 3A, relayed 2026-09-24): acceptance for the Java
// half of #2145 includes a live resolution test mirroring
// cmd/cleat/rust_entry_point_resolution_live_test.go -- same standard as
// cleat#2066/#2097: a string search over source, or over the sidecar
// manifest, cannot tell "the export exists" from "the build actually
// produced it under that name and cleat.metadata carries the same string".
// This starts a REAL worker against a REAL database and runs the REAL
// TeaVM-compiled example.
//
// examples/java-workflow declares two entry points (place_order,
// cancel_order) -- enough to exercise determineEntryPoint's ambiguity
// branch, which is what cleat#2097 exists for and what a single-entry-point
// fixture (cleat#2066's fullstack template) cannot reach.
package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/wasm"
)

func TestJavaExampleEntryPointResolutionLive(t *testing.T) {
	if testing.Short() || cleatBinary == "" {
		t.Skip("needs the cleat binary, which TestMain does not build in short mode")
	}
	if _, err := exec.LookPath("java"); err != nil {
		t.Skip("java not available")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}

	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	javaDir := filepath.Join(repoRoot, "examples", "java-workflow")
	if _, statErr := os.Stat(filepath.Join(javaDir, "settings.gradle.kts")); statErr != nil {
		t.Fatalf("computed Java example dir %s has no settings.gradle.kts: %v", javaDir, statErr)
	}

	outDir := t.TempDir()
	buildCmd := exec.Command(cleatBinary, "build", "--target", "java", "-o", outDir, javaDir)
	buildCmd.Dir = repoRoot
	if out, buildErr := buildCmd.CombinedOutput(); buildErr != nil {
		t.Fatalf("cleat build --target java: %v\n%s", buildErr, out)
	}

	wasmPath := filepath.Join(outDir, "java_workflow.wasm")
	wasmBytes, readErr := os.ReadFile(wasmPath)
	if readErr != nil {
		t.Fatalf("build did not produce %s: %v", wasmPath, readErr)
	}

	// wantEntryPoints is read from the SAME compiled artifact the build just
	// produced -- the authoritative cleat_entry_points section
	// CleatEntryProcessor's manifest (cleat-entry-points.txt) was embedded
	// from, not a source-level re-derivation that could drift from what
	// cleat build actually put in cleat.metadata.
	wantEntryPoints, epErr := wasm.ReadEntryPointsSection(wasmBytes)
	if epErr != nil {
		t.Fatalf("reading cleat_entry_points from the build's own output: %v", epErr)
	}
	if len(wantEntryPoints) < 2 {
		t.Fatalf("expected examples/java-workflow to declare multiple entry points, got %v", wantEntryPoints)
	}

	dsn, containerName := startSandboxPostgres(t)

	workerBinary := buildWorkerBinaryOnce(t)
	worker := exec.Command(workerBinary,
		"--db="+dsn,
		"--api-addr=:8080",
		"--migrate-on-start",
		"--require-auth=false",
	)
	worker.Dir = repoRoot
	waitForPortFree(t, 8080)
	worker.Env = os.Environ()
	var workerOut strings.Builder
	worker.Stdout = &workerOut
	worker.Stderr = &workerOut
	if startErr := worker.Start(); startErr != nil {
		t.Fatalf("start cleat-worker: %v", startErr)
	}
	t.Cleanup(func() {
		if worker.Process != nil {
			worker.Process.Kill()
			worker.Wait()
		}
		exec.Command("docker", "rm", "-f", containerName).Run()
		if t.Failed() {
			t.Logf("worker output:\n%s", workerOut.String())
		}
	})
	waitForHealthz(t, "http://localhost:8080/healthz")

	const workflowName = "java_workflow"
	deployCmd := exec.Command(cleatBinary, "deploy", "--name", workflowName, wasmPath)
	deployCmd.Dir = javaDir
	deployCmd.Env = append(os.Environ(), "CLEAT_DATABASE_URL="+dsn)
	if out, deployErr := deployCmd.CombinedOutput(); deployErr != nil {
		t.Fatalf("cleat deploy: %v\n%s", deployErr, out)
	}

	// 1. Implicit start: no entry_point at all. Ambiguous between place_order
	// and cancel_order -- must refuse with the new multi-entry-point message,
	// not the PRE-#2109 "no handle_* export" failure (EntryPoints empty
	// because the source-level regex never ran or predicted wrong) and not
	// silently pick one.
	implicitID, implicitErr := startWorkflow(t, workflowName, "")
	if implicitErr != nil {
		t.Fatalf("starting %s with no entry_point: %v", workflowName, implicitErr)
	}
	status := pollWorkflowStatus(t, "http://localhost:8080/api/workflows/"+implicitID)
	if status.Status != "failed" {
		t.Fatalf("run %s (implicit start, ambiguous entry point): want status \"failed\", got %q (error: %q)",
			implicitID, status.Status, status.Error)
	}
	if strings.Contains(status.Error, "no handle_* export") {
		t.Fatalf("run %s failed with the PRE-#2109 error -- cleat.metadata.entry_points is still empty for the "+
			"Java build path, EntryPoints regressed: %s", implicitID, status.Error)
	}
	if !strings.Contains(status.Error, "cannot determine entry point") ||
		!strings.Contains(status.Error, "declares 2 entry points") {
		t.Fatalf("run %s: want the new multi-entry-point message naming 2 entry points, got: %q",
			implicitID, status.Error)
	}
	for _, name := range wantEntryPoints {
		if !strings.Contains(status.Error, name) {
			t.Errorf("run %s: error message does not name declared entry point %q: %q", implicitID, name, status.Error)
		}
	}
	t.Logf("implicit start correctly refused ambiguity: %s", status.Error)

	// 2. Explicit start naming one of the build's own extracted names --
	// place_order calls the "inventory" plugin this bare worker has none
	// registered for, so it fails fast on that (an unrelated, expected
	// reason -- same pattern as cleat#2066/#2097's own tests), but it must
	// NOT fail on entry-point resolution, which is the one thing this test
	// exists to check.
	explicitID, explicitErr := startWorkflow(t, workflowName, "place_order")
	if explicitErr != nil {
		t.Fatalf("starting %s with entry_point=place_order: %v", workflowName, explicitErr)
	}
	status = pollWorkflowStatus(t, "http://localhost:8080/api/workflows/"+explicitID)
	if strings.Contains(status.Error, "cannot determine entry point") || strings.Contains(status.Error, "no handle_* export") {
		t.Fatalf("run %s (entry_point=place_order, a name the build itself extracted) failed on entry-point "+
			"resolution: %s -- the name TeaVM exported does not match what the cleat-entry-points.txt sidecar put "+
			"in cleat.metadata", explicitID, status.Error)
	}
	t.Logf("explicit entry_point=place_order reached the guest: status %q, error %q", status.Status, status.Error)
}
