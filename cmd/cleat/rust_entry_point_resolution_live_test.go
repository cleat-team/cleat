// cleat#2097. cmd/cleat/build_entry_points_test.go proves rustEntryPointNames
// reads the right names out of source; cmd/cleat/build_metadata_test.go
// proves nonGoMetadata's result passes Validate(). Neither proves the names
// that land in wasm.Metadata.EntryPoints are the names actually exported by
// the .wasm cargo produces -- a mismatch there (mangled symbols, a build
// target that doesn't preserve `#[no_mangle]`-style naming, an extractor
// regex that drifted from the SDK's real codegen) would build and deploy
// cleanly and only surface as a live "cannot determine entry point" or a
// trap once a worker tried to run it. Same standard
// fullstack_template_run_starts_a_workflow_test.go (cleat#2066) already holds
// the Go path to: "NOT MERELY A 2XX, AND NOT MERELY THAT CURL RAN" -- a
// string search over source cannot tell "the export exists" from "the build
// actually produced it under that name", so this starts a REAL worker
// against a REAL database and runs the REAL cargo-compiled example.
//
// Unlike the fullstack template (one entry point), examples/rust-workflow
// declares several -- exactly the case cleat#2066 did not exercise and
// cleat#2097 exists for. So this checks both halves of determineEntryPoint's
// multi-entry-point branch (cmd/cleat-worker/setup.go:725-731):
//
//  1. Implicit start (no entry_point) must NOT reproduce the PRE-#2097
//     failure -- "no handle_* export in WASM binary" -- because that failure
//     is what "these SDKs are broken for implicit start" (the issue's own
//     title) meant literally: EntryPoints was always empty for every non-Go
//     target, so resolution fell through past the metadata branch entirely.
//     It also must not silently start under the wrong entry point --
//     unreachable here, since determineEntryPoint only auto-resolves a
//     single declared entry point and this example has several, but the
//     assertion is on the SPECIFIC new error rather than "not that old one"
//     so a regression to some THIRD wrong behaviour cannot pass silently.
//  2. An explicit entry_point naming one of the build's real, extracted
//     names must be accepted and actually reach the guest -- proving the
//     string cargo put in the .wasm's export section is byte-for-byte the
//     string rustEntryPointNames put in cleat.metadata.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRustExampleEntryPointResolutionLive(t *testing.T) {
	if testing.Short() || cleatBinary == "" {
		t.Skip("needs the cleat binary, which TestMain does not build in short mode")
	}
	if _, err := exec.LookPath("cargo"); err != nil {
		t.Skip("cargo not available")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}

	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	cargoDir := filepath.Join(repoRoot, "examples", "rust-workflow")
	if _, statErr := os.Stat(filepath.Join(cargoDir, "Cargo.toml")); statErr != nil {
		t.Fatalf("computed rust example dir %s has no Cargo.toml: %v", cargoDir, statErr)
	}

	outDir := t.TempDir()
	buildCmd := exec.Command(cleatBinary, "build", "--target", "rust", "-o", outDir, cargoDir)
	buildCmd.Dir = repoRoot
	if out, buildErr := buildCmd.CombinedOutput(); buildErr != nil {
		if strings.Contains(string(out), "wasm32-unknown-unknown") {
			t.Skipf("wasm32-unknown-unknown target not installed: %s", out)
		}
		t.Fatalf("cleat build --target rust: %v\n%s", buildErr, out)
	}

	// rustEntryPointNames is the same extractor the build just used to
	// populate cleat.metadata -- read directly here as the independent
	// expectation the live error message is checked against below, rather
	// than hardcoding the example's entry point list a second time where it
	// could drift from the source.
	wantEntryPoints := rustEntryPointNames(filepath.Join(cargoDir, "src"))
	if len(wantEntryPoints) < 2 {
		t.Fatalf("expected examples/rust-workflow to declare multiple entry points, got %v", wantEntryPoints)
	}

	wasmPath := filepath.Join(outDir, "rust_workflow.wasm")
	if _, statErr := os.Stat(wasmPath); statErr != nil {
		t.Fatalf("build did not produce %s: %v", wasmPath, statErr)
	}

	dsn, containerName := startSandboxPostgres(t)

	workerBinary := buildWorkerBinaryOnce(t)
	worker := exec.Command(workerBinary,
		"--db="+dsn,
		"--api-addr=:8080",
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

	const workflowName = "rust_workflow"
	deployCmd := exec.Command(cleatBinary, "deploy", "--name", workflowName, wasmPath)
	deployCmd.Dir = cargoDir
	deployCmd.Env = append(os.Environ(), "CLEAT_DATABASE_URL="+dsn)
	if out, deployErr := deployCmd.CombinedOutput(); deployErr != nil {
		t.Fatalf("cleat deploy: %v\n%s", deployErr, out)
	}

	// 1. Implicit start: no entry_point at all.
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
		t.Fatalf("run %s failed with the PRE-#2097 error -- cleat.metadata.entry_points is still empty for the Rust "+
			"build path, EntryPoints regressed: %s", implicitID, status.Error)
	}
	if !strings.Contains(status.Error, "cannot determine entry point") ||
		!strings.Contains(status.Error, fmt.Sprintf("declares %d entry points", len(wantEntryPoints))) {
		t.Fatalf("run %s: want the new multi-entry-point message naming %d entry points, got: %q",
			implicitID, len(wantEntryPoints), status.Error)
	}
	for _, name := range wantEntryPoints {
		if !strings.Contains(status.Error, name) {
			t.Errorf("run %s: error message does not name declared entry point %q: %q", implicitID, name, status.Error)
		}
	}
	t.Logf("implicit start correctly refused ambiguity: %s", status.Error)

	// 2. Explicit start naming one of the build's own extracted names --
	// place_order calls an "inventory" plugin this bare worker has none
	// registered for, so it fails fast on that (an unrelated, expected
	// reason -- see cleat#2066's own test for the same pattern), but it must
	// NOT fail on entry-point resolution, which is the one thing this test
	// exists to check.
	explicitID, explicitErr := startWorkflow(t, workflowName, "place_order")
	if explicitErr != nil {
		t.Fatalf("starting %s with entry_point=place_order: %v", workflowName, explicitErr)
	}
	status = pollWorkflowStatus(t, "http://localhost:8080/api/workflows/"+explicitID)
	if strings.Contains(status.Error, "cannot determine entry point") || strings.Contains(status.Error, "no handle_* export") {
		t.Fatalf("run %s (entry_point=place_order, a name the build itself extracted) failed on entry-point "+
			"resolution: %s -- the name cargo exported does not match what rustEntryPointNames put in cleat.metadata",
			explicitID, status.Error)
	}
	t.Logf("explicit entry_point=place_order reached the guest: status %q, error %q", status.Status, status.Error)
}

// startWorkflow POSTs to /api/workflows/:name/start and returns the new run's id.
func startWorkflow(t *testing.T, name, entryPoint string) (string, error) {
	t.Helper()
	body := map[string]any{"input": map[string]any{"user_id": "u1", "cart": []map[string]any{{"sku": "widget", "quantity": 1}}}}
	if entryPoint != "" {
		body["entry_point"] = entryPoint
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("marshal start body: %w", err)
	}
	resp, err := http.Post("http://localhost:8080/api/workflows/"+name+"/start", "application/json", strings.NewReader(string(payload)))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusAccepted {
		return "", fmt.Errorf("POST start: status %d: %s", resp.StatusCode, respBody)
	}
	var parsed struct {
		ID string `json:"id"`
	}
	if jsonErr := json.Unmarshal(respBody, &parsed); jsonErr != nil {
		return "", fmt.Errorf("start response did not parse as JSON: %w: %s", jsonErr, respBody)
	}
	if parsed.ID == "" {
		return "", fmt.Errorf("start response carried no id: %s", respBody)
	}
	return parsed.ID, nil
}
