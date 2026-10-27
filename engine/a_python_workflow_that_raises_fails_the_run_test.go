//go:build cgo

package engine

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

// pythonExecuteFixture builds python-sdk/examples/<pyFile> into a real
// component, runs it through Engine.Execute on the wasmtime backend, and
// returns everything Execute reported -- including the error, which the
// sibling helper for the defer-segment tests treats as fatal. Here the error IS
// the thing under test.
//
// It holds this file's only skip site, on purpose: scripts/check-skips.sh
// counts skip sites per enclosing function, so three tests sharing one helper
// add one baseline line, not three.
func pythonExecuteFixture(t *testing.T, wfID, pyFile, funcName string) (
	result string, susp *SuspendResult, ops []string, err error,
) {
	t.Helper()

	pythonWasm := newPythonWasmTestHelper(t)
	if !pythonWasm.toolsAvailable() {
		if toolchainRequired("python") {
			t.Fatalf("Python WASM prerequisites not met, but %s declares python: %s",
				requireToolchainEnv, pythonWasm.missingTools())
		}
		t.Skip("Python WASM prerequisites not met: " + pythonWasm.missingTools())
	}

	wasmBytes, readErr := os.ReadFile(pythonWasm.compileWorkflow(t, pyFile, funcName))
	if readErr != nil {
		t.Fatalf("read Python WASM: %v", readErr)
	}

	ctx := context.Background()
	rt, rtErr := NewRuntime(ctx, 0, 0)
	if rtErr != nil {
		t.Fatalf("NewRuntime: %v", rtErr)
	}
	t.Cleanup(func() { rt.Close(ctx) })
	wt, wtErr := NewWasmtimeBackend(ctx)
	if wtErr != nil {
		t.Fatalf("NewWasmtimeBackend: %v", wtErr)
	}
	t.Cleanup(func() { wt.Close(ctx) })

	caller := &mockCaller{}
	eng := NewEngine(rt, caller, WithBackends(WasmtimeLanguages, wt), WithWorkflowID(wfID))

	result, _, susp, _, _, err = eng.Execute(ctx, wasmBytes, "run", []byte(`{}`))
	return result, susp, operationsCalled(caller), err
}

// TestAPythonWorkflowThatRaisesFailsTheRun is cleat#3433. A Python workflow
// body that raises must come out of Execute as an error, like a Go workflow
// that returns one or a Rust workflow that returns Err.
//
// Until the run-outcome gained a `failed` case, the SDK's entry wrapper caught
// the exception and returned {"error": "boom"} as a COMPLETED result: Execute
// returned no error and that string as the run's output, so retry policy, the
// dead-letter queue and any parent awaiting the run as a child never saw a
// failure.
//
// The failure text is asserted to survive: an operator reading the run's error
// needs the exception's message, not just a bit that says it failed.
func TestAPythonWorkflowThatRaisesFailsTheRun(t *testing.T) {
	res, susp, _, err := pythonExecuteFixture(t, "wf-python-raises", "raises_workflow.py", "raises")

	if err == nil {
		t.Fatalf("a Python workflow that raised ValueError(\"boom\") did not fail the run: "+
			"Execute returned no error, suspended=%v, result=%q.\n\n"+
			"This is cleat#3433: the entry wrapper returned the exception as a completed "+
			"{\"error\": ...} result.", susp != nil, res)
	}
	if susp != nil {
		t.Errorf("the failed run reported itself suspended: %v", susp.Reason)
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("the run failed, but its error does not carry the exception's message: %v", err)
	}
	if !strings.Contains(err.Error(), "ValueError") {
		t.Errorf("the run failed, but its error does not name the exception's type: %v", err)
	}
	// A raise is the workflow reporting that it failed, not the module
	// faulting. A host that predates the `failed` case still fails the run but
	// labels it "wasm trap: ... not a run-outcome variant" and drops the
	// message (measured, cleat#3433), which sends the reader hunting a memory
	// fault that never happened. GuestReturnedError is the marker that keeps
	// that label off; see its definition in runtime.go.
	var guestErr *GuestReturnedError
	if !errors.As(err, &guestErr) {
		t.Errorf("the failure is not marked as one the guest reported (GuestReturnedError): %v", err)
	}
	if strings.Contains(err.Error(), "wasm trap") {
		t.Errorf("a raise is labelled a wasm trap, which it is not: %v", err)
	}
}

// TestAPythonWorkflowThatRaisesWithNoMessageStillSaysWhatItRaised covers the
// exceptions whose str() is empty -- `raise KeyError()` is the common one. A
// failure that carried only str(exc) would be a failed run with nothing to read,
// so the message names the type. Found in review of the change that added the
// failed case (cleat-review-69), not by running it.
func TestAPythonWorkflowThatRaisesWithNoMessageStillSaysWhatItRaised(t *testing.T) {
	_, _, _, err := pythonExecuteFixture(t, "wf-python-raises-empty", "raises_empty_workflow.py", "raises_empty")

	if err == nil {
		t.Fatal("a Python workflow that raised KeyError() did not fail the run")
	}
	if !strings.Contains(err.Error(), "KeyError") {
		t.Errorf("the run failed with no indication of what was raised: %v\n\n"+
			"str(KeyError()) is empty, so a failure carrying only the exception's text says nothing.", err)
	}
}

// TestAPythonWorkflowThatReturnsAnErrorDictionaryStaysCompleted is the control
// for the test above, and the reason a host-side check for an {"error": ...}
// shape was rejected: a workflow may legitimately RETURN that shape, and a host
// that sniffed for it could not tell the result from a raise.
func TestAPythonWorkflowThatReturnsAnErrorDictionaryStaysCompleted(t *testing.T) {
	res, susp, _, err := pythonExecuteFixture(t, "wf-python-returns-error-dict",
		"returns_error_dict_workflow.py", "returns_error_dict")

	if err != nil {
		t.Fatalf("a workflow that returned a dictionary with an \"error\" key failed the run: %v\n\n"+
			"That is a result, not a raise; a host that treats the shape as a failure cannot tell them apart.", err)
	}
	if susp != nil {
		t.Fatalf("the run suspended: %v", susp.Reason)
	}
	var got map[string]string
	if jerr := json.Unmarshal([]byte(res), &got); jerr != nil {
		t.Fatalf("the completed result is not the returned dictionary: %q (%v)", res, jerr)
	}
	if want := "a result that happens to say error"; got["error"] != want {
		t.Errorf("result = %q, want the workflow's own return value with error=%q", res, want)
	}
}

// TestAPythonWorkflowThatRaisesRunsItsDeferExactlyOnce pins that a failing run
// still does its cleanup, once. The entry wrapper runs the registered defers on
// its error path, so they have run by the time the host sees the failure; this
// asserts they were neither lost to the new failure path nor repeated after it.
//
// Be clear about what it does NOT show: it also passes on a host that predates
// the `failed` case, because a Python guest's defers live inside the guest and
// not in the session table the executor would drain a second time. It is a
// guard on the behaviour, not the proof that the host change is needed.
func TestAPythonWorkflowThatRaisesRunsItsDeferExactlyOnce(t *testing.T) {
	_, _, ops, err := pythonExecuteFixture(t, "wf-python-raises-defer", "raises_workflow.py", "raises")

	if err == nil {
		t.Fatalf("the raising workflow did not fail, so this test cannot say anything about its defers (ops %v)", ops)
	}
	if len(ops) != 1 || ops[0] != "ran" {
		t.Errorf("the registered defer ran %d time(s) (%v), want exactly once: it runs in the wrapper "+
			"on the error path and the host must not run it again", len(ops), ops)
	}
}
