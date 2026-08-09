package engine

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"
)

// TestPythonCronEndToEnd is what makes "Python can register cron triggers" a
// property CI checks rather than a sentence in tiers.yaml.
//
// python-sdk/wit/cleat.wit gained a durable-cron interface on 2026-08-09, and
// wasm/component_rewrite.go the matching env mapping. Before that,
// componentize-py generated no binding, the built component never imported the
// calls, and HostCalls.schedule_cron raised NotImplementedError whatever the
// host supported. cleat_sdk.local_host DID implement all three, so every test
// that touched cron used the in-process host and nothing noticed.
//
// That is the specific reason this test compiles a REAL component and runs it
// against a REAL engine and a REAL store. Each cheaper option fails to detect
// the original defect:
//
//	the SDK's own unit tests      use local_host, which worked all along
//	a component that only builds  builds fine with the binding unwired
//	a hello-world component       never imports the calls at all
//
// It lives in ./engine/ deliberately: that is a tier-1 package, so
// scripts/tier-gate.sh runs it, and both tier1-gate.yml and
// e2e-cross-language.yml already sweep ./engine/... with a TestPython pattern.
// A test in tests/cross-language would not be executed by the gate at all,
// which would leave the tier-1 claim exactly as unenforced as it is today.
func TestPythonCronEndToEnd(t *testing.T) {
	ctx := context.Background()

	pythonWasm := newPythonWasmTestHelper(t)
	if !pythonWasm.toolsAvailable() {
		if toolchainRequired("python") {
			t.Fatalf("Python WASM prerequisites not met, but %s declares python, so this job "+
				"installs componentize-py/wasm-tools and treats Python as first-class: %s",
				requireToolchainEnv, pythonWasm.missingTools())
		}
		// componentize-py cannot run natively on macOS (EXC_GUARD /
		// GUARD_TYPE_MACH_PORT). See scripts/docker/python-toolchain.Dockerfile
		// for the Linux container that runs this locally.
		t.Skip("Python WASM prerequisites not met: " + pythonWasm.missingTools())
	}

	// A real store, because the cron host calls refuse without one --
	// createCronSchedule returns "no workflow store configured" (engine/schedules.go).
	// An engine built without one would exercise the ABI and prove nothing about
	// persistence, which is most of what these calls are for.
	//
	// PostgreSQL directly rather than registeredBackends: what is under test is
	// the Python guest reaching the host, not dialect behaviour, and looping
	// would add a MySQL and an MSSQL skip to every job that configures neither.
	store, teardown := cronTestStore(t, &PostgresBackend{})
	defer teardown()

	wasmPath := pythonWasm.compileWorkflow(t, "cron_workflow.py", "cron_workflow")
	wasmBytes, err := os.ReadFile(wasmPath)
	if err != nil {
		t.Fatalf("read compiled WASM: %v", err)
	}

	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	// Not a skip if wasmtime is missing: python is in WasmtimeLanguages, so
	// falling back to wazero would test a configuration nothing ships.
	wt, wtErr := NewWasmtimeBackend(ctx)
	if wtErr != nil {
		t.Fatalf("NewWasmtimeBackend: %v (python routes here; there is no fallback to test)", wtErr)
	}
	engine := NewEngine(rt, &mockCaller{},
		WithBackends(WasmtimeLanguages, wt),
		WithWorkflowStore(store),
	)

	const target = "nightly-report"
	input := `{"workflow_name":"` + target + `"}`

	result, _, suspended, _, _, err := engine.Execute(ctx, wasmBytes, "run", json.RawMessage(input))
	if err != nil {
		t.Fatalf("Execute: %v\n"+
			"a failure here is the whole point of the test: it means a cron call did not reach "+
			"the host from a componentized Python guest", err)
	}
	if suspended != nil {
		t.Fatalf("unexpected suspension: %v", suspended.Reason)
	}

	scheduleID, listed := parsePythonCronResult(t, result)

	// schedule_cron: the host minted an ID and handed it back through the
	// component boundary.
	if scheduleID == "" {
		t.Error("schedule_cron returned an empty schedule ID")
	}

	// list_crons: the guest saw the schedule it had just created. This is the
	// only evidence available after the fact, because the workflow deletes it.
	if listed != 1 {
		t.Errorf("list_crons saw %d schedule(s) while one existed, want 1 -- the call reached "+
			"the host but did not observe what schedule_cron had just written", listed)
	}

	// delete_cron: the store is empty again. cronTestStore removes the seeded
	// fixture schedule, so anything left here came from this workflow.
	remaining, err := store.ListSchedules(ctx)
	if err != nil {
		t.Fatalf("ListSchedules: %v", err)
	}
	if len(remaining) != 0 {
		names := make([]string, len(remaining))
		for i, s := range remaining {
			names[i] = s.Name
		}
		t.Errorf("%d schedule(s) left in the store after the workflow deleted its own: %v -- "+
			"delete_cron did not take effect", len(remaining), names)
	}
}

// parsePythonCronResult unwraps the "<schedule-id>|<count>" the fixture
// returns.
//
// The result arrives JSON-encoded, and componentize-py components hand back a
// JSON *string* rather than a bare value -- TestPythonWasmEndToEnd sees the
// same shape ("\"{}\"" for a workflow returning {}). Unwrapping is done here,
// once, rather than asserted on: the encoding is a property of the component
// ABI and not what this test is about.
func parsePythonCronResult(t *testing.T, result string) (scheduleID string, listed int) {
	t.Helper()

	raw := strings.TrimSpace(result)
	// Peel one layer of JSON string encoding if present.
	var unquoted string
	if err := json.Unmarshal([]byte(raw), &unquoted); err == nil {
		raw = unquoted
	}

	parts := strings.Split(raw, "|")
	if len(parts) != 2 {
		t.Fatalf("workflow result %q is not \"<schedule-id>|<count>\"; the fixture and this "+
			"parser have diverged", result)
	}
	n, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil {
		t.Fatalf("workflow result %q has a non-numeric count: %v", result, err)
	}
	return strings.TrimSpace(parts[0]), n
}
