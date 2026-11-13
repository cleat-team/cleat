package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/wasm"
)

// cleat#3786, through a worker: a signal sent over HTTP reaches a Python guest
// with its name and payload, and the run completes.
//
// Before the fix the guest received the right LENGTHS and NUL bytes
// (engine/python_await_signals_e2e_test.go reads them directly). Through a
// worker the symptom was worse than wrong data: a workflow that returned the
// payload it received returned `"\u0000\u0000..."`, and PostgreSQL refuses a NUL
// in a text column, so the run could not be FINALISED --
//
//	pq: invalid byte sequence for encoding "UTF8": 0x00 (22P05)
//
// -- and sat there failing the same write on every attempt. A tier-1 language
// whose signals either arrived blank or wedged the run, with every test green,
// because the engine tests used a mock handler and the SDK tests a stub.
//
// EVERYTHING HERE IS REAL: the Python component (built with `cleat build
// --target python`), the wasmtime backend, the engine, the worker's own
// executeWorkflow, the apiServer's signal handler and PostgreSQL. The only thing
// it does not do is start the cleat-worker binary; the in-process worker is the
// same code.

// buildPythonExample compiles one entry of a python-sdk example the way a user's
// build does. The example holds several entries, so the build is told which.
func buildPythonExample(t *testing.T, relPath, entry string) []byte {
	t.Helper()
	if reason := pythonWASMUnavailable(); reason != "" {
		if toolchainRequired("python") {
			t.Fatalf("cannot build a Python workflow, but %s declares python, so this job installs "+
				"componentize-py and treats Python as tier 1: %s", requireToolchainEnv, reason)
		}
		t.Skip("cannot build a Python workflow: " + reason)
	}
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
		"build", "--target", "python", "-o", tmpDir, "--entry", filepath.Join(projectRoot, relPath)+":"+entry)
	cmd.Dir = projectRoot
	cmd.Env = os.Environ()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cleat build --target python failed:\n%s\n%v", out, err)
	}
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		t.Fatalf("reading build output: %v", err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".wasm" {
			b, err := os.ReadFile(filepath.Join(tmpDir, e.Name()))
			if err != nil {
				t.Fatalf("reading %s: %v", e.Name(), err)
			}
			return b
		}
	}
	t.Fatalf("no .wasm in %s", tmpDir)
	return nil
}

func TestAPythonSignalReachesTheGuestThroughTheWorker(t *testing.T) {
	if os.Getenv("CLEAT_TEST_POSTGRES") == "" && os.Getenv("CLEAT_TEST_DB") == "" {
		t.Skip("CLEAT_TEST_POSTGRES not set, skipping the database-backed Python signal test")
	}
	ctx := context.Background()

	db := testutil.SuiteTestDB(t, "cleat_worker")
	store := engine.NewPostgresStore(db)

	logs := &syncBuffer{}
	w := newRealDeferPhaseWorker(t, db, store, logs)
	api := &apiServer{store: store, worker: w, maxBodySize: 1 << 20, db: db}

	// deploy builds one entry of the example and stores it under its own name.
	deploy := func(entry string) (string, int) {
		wasmBytes := buildPythonExample(t, filepath.Join("python-sdk", "examples", "await_signals_outcomes_workflow.py"), entry)
		meta, err := wasm.ReadMetadata(wasmBytes)
		if err != nil {
			t.Fatalf("ReadMetadata: %v", err)
		}
		defName := fmt.Sprintf("py-%s-%d", strings.ReplaceAll(entry, "_", "-"), time.Now().UnixNano())
		if err := store.DeployWorkflowDef(ctx, &engine.WorkflowDef{
			Name: defName, Version: meta.WorkflowVersion, WASMBytes: wasmBytes,
			ABIVersion: meta.ABIVersion, MinVersion: meta.MinCompatibleVersion,
		}); err != nil {
			t.Fatalf("deploy %s: %v", entry, err)
		}
		return defName, meta.WorkflowVersion
	}

	// sendAndFinish starts a run of defName, lets it suspend at the wait, delivers
	// the signal over HTTP, and runs the woken run to its end.
	sendAndFinish := func(t *testing.T, defName string, version int, signal, payload string) *engine.WorkflowInstance {
		t.Helper()
		id := fmt.Sprintf("%s-run-%d", defName, time.Now().UnixNano())
		input := fmt.Sprintf(`{"signal_name":%q}`, signal)
		if _, _, err := store.StartNewRun(ctx, id, defName, version,
			json.RawMessage(input), "", engine.DefaultTenantUUID, 0); err != nil {
			t.Fatalf("StartNewRun: %v", err)
		}

		// Segment 1: nothing has been sent, so the wait is recorded and the run suspends.
		runExecuteWorkflow(w, claimOne(t, ctx, store, w.id, id))
		wf, err := store.GetWorkflowByID(ctx, id)
		if err != nil || wf == nil {
			t.Fatalf("GetWorkflowByID: %v", err)
		}
		if wf.Status == "done" || wf.Status == "failed" {
			t.Fatalf("UNMEASURED: the run reached %q before any signal was sent; the wait was not reached (logs:\n%s)",
				wf.Status, logs.String())
		}

		// The signal goes in over HTTP, through the handler a client uses.
		body, _ := json.Marshal(map[string]string{"signal_name": signal, "payload": payload})
		req := httptest.NewRequest(http.MethodPost, "/api/workflows/"+id+"/signal", strings.NewReader(string(body)))
		rec := httptest.NewRecorder()
		api.handleWorkflows(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("signal: status %d, body %s", rec.Code, rec.Body.String())
		}

		// Segment 2: the delivery wakes the run, which resumes, receives it and finishes.
		deadline := time.Now().Add(20 * time.Second)
		var mine *engine.WorkflowInstance
		for mine == nil && time.Now().Before(deadline) {
			claimed, err := store.ClaimWorkflows(ctx, w.id, 50)
			if err != nil {
				t.Fatalf("ClaimWorkflows: %v", err)
			}
			for _, cw := range claimed {
				if cw.ID == id {
					mine = cw
					continue
				}
				_ = store.ReleaseWorkflow(ctx, cw.ID, w.id, cw.Generation, cw.NextWakeAt)
			}
			if mine == nil {
				time.Sleep(50 * time.Millisecond)
			}
		}
		if mine == nil {
			t.Fatalf("the signalled run was never claimable again (logs:\n%s)", logs.String())
		}
		runExecuteWorkflow(w, mine)

		done, err := store.GetWorkflowByID(ctx, id)
		if err != nil || done == nil {
			t.Fatalf("GetWorkflowByID: %v", err)
		}
		return done
	}

	// Payloads chosen to separate a correct delivery from every wrong one: a JSON
	// document (so "the right length of NULs" is distinguishable from it), and
	// multibyte text (so a byte/character mix-up in either direction shows).
	//
	// The JSON payload has its keys in sorted order on purpose. Observed while
	// writing this, and not investigated because it is not what this test is
	// about: a JSON payload sent as {"approved":true,"amount":42} reached the
	// guest as {"amount":42,"approved":true}, i.e. something between the HTTP
	// handler and the guest canonicalises it. Sorted keys are unchanged by that.
	cases := []struct{ name, signal, payload string }{
		{"json", "go", `{"amount":42,"approved":true}`},
		{"multibyte", "v\u00e9rtigo", "h\u00e9llo \u2713 \u65e5\u672c\u8a9e \U0001F680"},
		{"empty payload", "ping", ""},
	}

	echoDef, echoVer := deploy("signal_echo")
	for _, c := range cases {
		t.Run("echo/"+c.name, func(t *testing.T) {
			done := sendAndFinish(t, echoDef, echoVer, c.signal, c.payload)
			if done.Status != "done" {
				t.Fatalf("status = %q, want done.\nerror: %s %s\nlogs:\n%s", done.Status, done.ErrorCode, done.Error, logs.String())
			}
			var got struct {
				Name     string `json:"name"`
				Payload  string `json:"payload"`
				TimedOut bool   `json:"timed_out"`
			}
			if err := json.Unmarshal([]byte(unwrapStoredResult(t, done.Result)), &got); err != nil {
				t.Fatalf("result %q is not the workflow's JSON document: %v", done.Result, err)
			}
			if got.Name != c.signal || got.Payload != c.payload || got.TimedOut {
				t.Errorf("the guest received name=%q (hex %s) payload=%q (hex %s) timed_out=%v; sent name=%q payload=%q",
					got.Name, hex.EncodeToString([]byte(got.Name)), got.Payload, hex.EncodeToString([]byte(got.Payload)),
					got.TimedOut, c.signal, c.payload)
			}
		})
	}

	// The payload IS the result. Before the fix that result was a run of NUL
	// bytes, which PostgreSQL refuses to store, so the run could not be finalised.
	rawDef, rawVer := deploy("signal_payload_result")
	for _, c := range cases[:2] {
		t.Run("payload is the result/"+c.name, func(t *testing.T) {
			done := sendAndFinish(t, rawDef, rawVer, c.signal, c.payload)
			if done.Status != "done" {
				t.Fatalf("status = %q, want done.\nerror: %s %s\nlogs:\n%s\n(before the fix: the result was NUL bytes and "+
					"the finalising write was refused, 22P05)", done.Status, done.ErrorCode, done.Error, logs.String())
			}
			if got := unwrapStoredResult(t, done.Result); got != c.payload {
				t.Errorf("result = %q (hex %s), want the payload %q", got, hex.EncodeToString([]byte(got)), c.payload)
			}
		})
	}
}

// unwrapStoredResult returns the workflow's own result text from the stored
// result, which the Python entry wraps as {"result": "<text>"} or stores bare.
func unwrapStoredResult(t *testing.T, stored string) string {
	t.Helper()
	var wrapped struct {
		Result *string `json:"result"`
	}
	if json.Unmarshal([]byte(stored), &wrapped) == nil && wrapped.Result != nil {
		return *wrapped.Result
	}
	return stored
}
