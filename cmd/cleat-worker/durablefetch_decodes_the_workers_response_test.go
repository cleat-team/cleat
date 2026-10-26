package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/wasm"
)

// cleat#3408: the Go SDK's DurableFetch decoded {body, status_code}, and the
// worker answers {status, headers, body}. DurableFetch therefore returned
// status 0 for every response on a real worker. The SDK's own tests passed
// because they stubbed the host with the key the SDK read.
//
// This drives a real guest, compiled by `cleat build`, through the real
// executeWorkflow and the real http.fetch handler, and reads what the SDK
// decoded. Nothing in it is stubbed except the destination, which is an
// httptest server (hence AllowLoopback, the sanctioned test-only seam).
func TestDurableFetchReturnsTheStatusTheWorkerSends(t *testing.T) {
	requireShutdownTestDB(t)
	ctx := context.Background()

	// A status that is not 200: a decoder that fell back to a default, or to
	// zero, would not be told apart from success by a 200.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		fmt.Fprint(w, "short and stout")
	}))
	defer srv.Close()

	db := testutil.SuiteTestDB(t, "cleat_worker")
	store := engine.NewPostgresStore(db)

	wasmBytes := buildTestdataFixture(t, "fetchprobe")
	meta, err := wasm.ReadMetadata(wasmBytes)
	if err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}
	const defName = "durablefetch-decode-worker"
	if err := store.DeployWorkflowDef(ctx, &engine.WorkflowDef{
		Name: defName, Version: meta.WorkflowVersion, WASMBytes: wasmBytes,
		ABIVersion: meta.ABIVersion, MinVersion: meta.MinCompatibleVersion,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}
	wfID := fmt.Sprintf("durablefetch-decode-%d", time.Now().UnixNano())
	input, _ := json.Marshal(map[string]string{"__entry_point": "FetchStatus", "url": srv.URL})
	if _, _, err := store.StartNewRun(ctx, wfID, defName, meta.WorkflowVersion,
		input, "", engine.DefaultTenantUUID, 0); err != nil {
		t.Fatalf("StartNewRun: %v", err)
	}

	logs := &syncBuffer{}
	w := newRealDeferPhaseWorker(t, db, store, logs)
	w.egress = &engine.EgressGuard{
		AllowLoopback:  true,
		TenantOptional: func(context.Context) bool { return true },
	}
	runExecuteWorkflow(w, claimOne(t, ctx, store, w.id, wfID))

	wf, err := store.GetWorkflowByID(ctx, wfID)
	if err != nil || wf == nil {
		t.Fatalf("GetWorkflowByID: %v", err)
	}
	if wf.Status != "done" {
		t.Fatalf("UNMEASURED: the run ended %q, not done, so no decoded status was produced "+
			"(error %q; logs:\n%s)", wf.Status, wf.Error, logs.String())
	}
	var got struct {
		Status int    `json:"status"`
		Body   string `json:"body"`
	}
	if err := json.Unmarshal([]byte(wf.Result), &got); err != nil {
		t.Fatalf("UNMEASURED: the guest's result is not JSON: %v\n%s", err, wf.Result)
	}
	if got.Body != "short and stout" {
		t.Fatalf("UNMEASURED: body = %q, so the response did not make the round trip and the status below says nothing", got.Body)
	}
	if got.Status != http.StatusTeapot {
		t.Errorf("DurableFetch returned status %d for a %d response: the SDK decodes a key the worker does not send",
			got.Status, http.StatusTeapot)
	}
}

// buildTestdataFixture compiles testdata/<name> the way buildDeferFixture does,
// which is the way `cleat build` produces a deployed module.
func buildTestdataFixture(t *testing.T, name string) []byte {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if resolved, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = resolved
	}
	root := filepath.Dir(filepath.Dir(cwd)) // cmd/cleat-worker -> repo root
	out := t.TempDir()
	cmd := exec.Command("go", "run", filepath.Join(root, "cmd", "cleat"),
		"build", "--target", "go", "-o", out, filepath.Join(root, "testdata", name))
	cmd.Dir = root
	cmd.Env = os.Environ()
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cleat build %s failed:\n%s\n%v", name, b, err)
	}
	entries, err := os.ReadDir(out)
	if err != nil {
		t.Fatalf("reading build output: %v", err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".wasm" {
			b, err := os.ReadFile(filepath.Join(out, e.Name()))
			if err != nil {
				t.Fatalf("reading fixture: %v", err)
			}
			return b
		}
	}
	t.Fatalf("no .wasm produced in %s", out)
	return nil
}
