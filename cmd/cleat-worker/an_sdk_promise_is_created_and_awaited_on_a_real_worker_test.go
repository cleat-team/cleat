package main

// cleat#3803, cleat#3805, end to end. The documented typed promise -- cleat.NewPromiseTyped and Promise.Await --
// built with `cleat build`, deployed to a real PostgreSQL, and run by a real worker on wasmtime.
//
// The unit-level proof is in cmd/cleat (the module imports cleat_create_promise and cleat_await_promise). This is
// the proof that matters to a user and the one a host-side test cannot give: cleattest and wasmtest construct a
// fully populated HostCalls, so a promise works there whether or not the compiled module imports anything. On the
// worker, a module that does not import cleat_create_promise reaches a nil adapter and the run FAILS on its first
// line, with a message ("CreatePromise can only be called from within a workflow function (the HostCalls runtime
// was not initialized)") that names the entry point and points away from the cause.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/wasm"
)

// buildSDKHelperFixture builds testdata/sdkhelpers/<name> with `cleat build`, as a user would.
func buildSDKHelperFixture(t *testing.T, name string) []byte {
	t.Helper()
	return buildFixture(t, filepath.Join("testdata", "sdkhelpers", name))
}

// buildFixture builds the workflow at relDir (relative to the repository root) with `cleat build`, as a user
// would, and returns the module.
func buildFixture(t *testing.T, relDir string) []byte {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if resolved, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = resolved
	}
	projectRoot := filepath.Dir(filepath.Dir(cwd)) // cmd/cleat-worker -> repo root

	tmpDir := t.TempDir()
	cmd := exec.Command("go", "run", filepath.Join(projectRoot, "cmd", "cleat"),
		"build", "--target", "go", "-o", tmpDir, filepath.Join(projectRoot, relDir))
	cmd.Dir = projectRoot
	cmd.Env = os.Environ()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cleat build failed:\n%s\n%v", string(out), err)
	}
	b, err := os.ReadFile(filepath.Join(tmpDir, "main.wasm"))
	if err != nil {
		t.Fatalf("reading the built module: %v", err)
	}
	return b
}

func TestATypedPromiseIsCreatedAndAwaitedOnARealWorker(t *testing.T) {
	ctx := context.Background()

	db := testutil.SuiteTestDB(t, "cleat_worker")
	store := engine.NewPostgresStore(db)

	wasmBytes := buildSDKHelperFixture(t, "promisetyped")
	meta, err := wasm.ReadMetadata(wasmBytes)
	if err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}

	const defName = "sdk-typed-promise-acceptance"
	if err := store.DeployWorkflowDef(ctx, &engine.WorkflowDef{
		Name: defName, Version: meta.WorkflowVersion, WASMBytes: wasmBytes,
		ABIVersion: meta.ABIVersion, MinVersion: meta.MinCompatibleVersion,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}

	wfID := fmt.Sprintf("sdk-typed-promise-%d", time.Now().UnixNano())
	if _, _, err := store.StartNewRun(ctx, wfID, defName, meta.WorkflowVersion,
		json.RawMessage(`"go"`), "", engine.DefaultTenantUUID, 0); err != nil {
		t.Fatalf("StartNewRun: %v", err)
	}

	logs := &syncBuffer{}
	worker := newRealDeferPhaseWorker(t, db, store, logs)
	worker.id = "sdk-typed-promise-worker"

	// First run: the workflow creates its promise and waits on it.
	first := claimOne(t, ctx, store, worker.id, wfID)
	worker.inflight.Store(wfID, first)
	runExecuteWorkflow(worker, first)

	wf, err := store.GetWorkflowByID(ctx, wfID)
	if err != nil || wf == nil {
		t.Fatalf("GetWorkflowByID: %v, %v", err, wf)
	}
	if wf.Status == "failed" {
		t.Fatalf("the run FAILED on its first execution (error %q) instead of creating its promise and waiting.\n"+
			"A module that does not import cleat_create_promise reaches a nil adapter here.\nlogs:\n%s",
			wf.Error, logs.String())
	}
	promises, err := store.ListPromises(ctx, wfID)
	if err != nil {
		t.Fatalf("ListPromises: %v", err)
	}
	if len(promises) != 1 || promises[0].PromiseName != "approval" || promises[0].Status != "pending" {
		t.Fatalf("want exactly one pending promise named \"approval\", got %+v (status %q)\nlogs:\n%s", promises, wf.Status, logs.String())
	}

	// Resolve it, then let a worker run the workflow again. Await returns the value the promise was resolved with.
	if err := store.ResolvePromise(ctx, promises[0].PromiseID, `{"by":"alice"}`); err != nil {
		t.Fatalf("ResolvePromise: %v", err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for statusOf(t, ctx, store, wfID) != "done" && time.Now().Before(deadline) {
		claimed, err := store.ClaimWorkflows(ctx, worker.id, 50)
		if err != nil {
			t.Fatalf("ClaimWorkflows: %v", err)
		}
		ran := false
		for _, c := range claimed {
			if c.ID != wfID {
				if err := store.ReleaseWorkflow(ctx, c.ID, worker.id, c.Generation, c.NextWakeAt); err != nil {
					t.Logf("releasing %s, which this test does not own: %v", c.ID, err)
				}
				continue
			}
			worker.inflight.Store(wfID, c)
			runExecuteWorkflow(worker, c)
			ran = true
		}
		if !ran {
			time.Sleep(100 * time.Millisecond)
		}
	}

	wf, err = store.GetWorkflowByID(ctx, wfID)
	if err != nil || wf == nil {
		t.Fatalf("GetWorkflowByID: %v, %v", err, wf)
	}
	if wf.Status != "done" {
		t.Fatalf("status = %q (error %q), want \"done\" after the promise was resolved\nlogs:\n%s", wf.Status, wf.Error, logs.String())
	}
	if got, want := wf.Result, `{"approved_by":"alice"}`; got != want {
		t.Fatalf("result = %s, want %s: the value the promise was resolved with did not come back through Promise.Await", got, want)
	}
}
