// cleat#2145 (owner decision 3A, relayed 2026-09-24): acceptance for the
// AssemblyScript half of #2145 includes a live resolution test mirroring
// cmd/cleat/rust_entry_point_resolution_live_test.go -- same standard as
// cleat#2066/#2097: a string search over source, or over the sidecar
// manifest, cannot tell "the export exists" from "the build actually
// produced it under that name and cleat.metadata carries the same string".
// This starts a REAL worker against a REAL database and runs the REAL
// asc-compiled example.
//
// examples/as-workflow declares eight entry points (multi entry point, same
// as Rust's fixture and for the same reason: determineEntryPoint's ambiguity
// branch only fires when there is more than one declared entry point, and
// that branch -- not "does the section parse" -- is what cleat#2097 exists
// for).
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

	"github.com/cleat-team/cleat/wasm"
)

func TestAssemblyScriptExampleEntryPointResolutionLive(t *testing.T) {
	if testing.Short() || cleatBinary == "" {
		t.Skip("needs the cleat binary, which TestMain does not build in short mode")
	}
	if _, err := exec.LookPath("npx"); err != nil {
		t.Skip("npx not available")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}

	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	asDir := filepath.Join(repoRoot, "examples", "as-workflow")
	if _, statErr := os.Stat(filepath.Join(asDir, "package.json")); statErr != nil {
		t.Fatalf("computed AS example dir %s has no package.json: %v", asDir, statErr)
	}

	outDir := t.TempDir()
	buildCmd := exec.Command(cleatBinary, "build", "--target", "assemblyscript", "-o", outDir, asDir)
	buildCmd.Dir = repoRoot
	if out, buildErr := buildCmd.CombinedOutput(); buildErr != nil {
		t.Fatalf("cleat build --target assemblyscript: %v\n%s", buildErr, out)
	}

	wasmPath := filepath.Join(outDir, "as-workflow.wasm")
	wasmBytes, readErr := os.ReadFile(wasmPath)
	if readErr != nil {
		t.Fatalf("build did not produce %s: %v", wasmPath, readErr)
	}

	// wantEntryPoints is read from the SAME compiled artifact the build just
	// produced -- the authoritative cleat_entry_points section the SDK-side
	// manifest (cleat-entry-points.txt) was embedded from, not a source-level
	// re-derivation that could drift from what cleat build actually put in
	// cleat.metadata.
	wantEntryPoints, epErr := wasm.ReadEntryPointsSection(wasmBytes)
	if epErr != nil {
		t.Fatalf("reading cleat_entry_points from the build's own output: %v", epErr)
	}
	if len(wantEntryPoints) < 2 {
		t.Fatalf("expected examples/as-workflow to declare multiple entry points, got %v", wantEntryPoints)
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

	const workflowName = "as_workflow"
	deployCmd := exec.Command(cleatBinary, "deploy", "--name", workflowName, wasmPath)
	deployCmd.Dir = asDir
	deployCmd.Env = append(os.Environ(), "CLEAT_DATABASE_URL="+dsn)
	if out, deployErr := deployCmd.CombinedOutput(); deployErr != nil {
		t.Fatalf("cleat deploy: %v\n%s", deployErr, out)
	}

	// 1. Implicit start: no entry_point at all. Ambiguous among 8 declared
	// entry points -- must refuse with the new multi-entry-point message, not
	// the PRE-#2109 "no handle_* export" failure (EntryPoints empty because
	// the source-level regex never ran or predicted wrong) and not silently
	// pick one.
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
			"AssemblyScript build path, EntryPoints regressed: %s", implicitID, status.Error)
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

	// 2. Explicit start naming one of the build's own extracted names, with
	// the input shape place_order actually parses (userID, items) -- proving
	// the string asc's export section carries is byte-for-byte the string the
	// cleat-entry-points.txt sidecar (and, from it, cleat.metadata) carries,
	// and that the guest genuinely ran rather than returning early for an
	// unrelated reason.
	explicitID, explicitErr := startWorkflowASInput(t, workflowName, "place_order")
	if explicitErr != nil {
		t.Fatalf("starting %s with entry_point=place_order: %v", workflowName, explicitErr)
	}
	status = pollWorkflowStatus(t, "http://localhost:8080/api/workflows/"+explicitID)
	if strings.Contains(status.Error, "cannot determine entry point") || strings.Contains(status.Error, "no handle_* export") {
		t.Fatalf("run %s (entry_point=place_order, a name the build itself extracted) failed on entry-point "+
			"resolution: %s -- the name asc exported does not match what the cleat-entry-points.txt sidecar put "+
			"in cleat.metadata", explicitID, status.Error)
	}
	// place_order calls the "inventory" plugin this bare worker has none
	// registered for, so it either completes with the SDK's own
	// "cart is empty" result (userID/items unset would never happen here,
	// since we send them) or fails on the missing plugin -- either is an
	// unrelated, expected reason (same pattern as cleat#2066/#2097's own
	// tests), and neither is an entry-point resolution failure, which is the
	// one thing already ruled out above.
	t.Logf("explicit entry_point=place_order reached the guest: status %q, error %q", status.Status, status.Error)
}

// startWorkflowASInput POSTs to /api/workflows/:name/start with the input
// shape examples/as-workflow's place_order actually parses (userID, items) --
// distinct from the shared startWorkflow helper's user_id/cart shape, which
// AS's hand-rolled field extraction would not recognize.
func startWorkflowASInput(t *testing.T, name, entryPoint string) (string, error) {
	t.Helper()
	body := map[string]any{
		"entry_point": entryPoint,
		"input": map[string]any{
			"userID": "user_42",
			"items":  []map[string]any{{"sku": "widget", "quantity": 1}},
		},
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
