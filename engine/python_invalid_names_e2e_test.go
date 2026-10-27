//go:build cgo

package engine

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// invalidNameRefusal is how a refused name reaches a Python guest. It depends on
// the WIT result type of the import, which is the only thing that decides what a
// refusal can look like.
type invalidNameRefusal int

const (
	// refusedAsCallError: the import returns result<_, call-failure>, so the
	// refusal is err(failed) with the invalid-request code and the SDK raises
	// CleatCallPermanentError. Core-module guests get the same classification
	// from badParamDurableCall.
	refusedAsCallError invalidNameRefusal = iota
	// refusedAsBadParam: the import returns u64, so the refusal is errBadParam,
	// the word every core-module host function returns for it. The SDK decodes
	// that word, not the host, and raises.
	refusedAsBadParam
	// refusedByTrap: the import returns a bare string (or nothing). Neither can
	// carry a failure, and "" is a plausible answer, so the guest is trapped.
	refusedByTrap
)

// TestPythonGuestInvalidNamesAreRefusedAsTheCoreModulePathRefusesThem is
// cleat#3436's regression test.
//
// The core-module host functions run validServiceName on every service,
// operation, promise, signal, lock and workflow name and refuse the call before
// any collaborator sees it. The component dispatchers read the same strings with
// readStrArg and passed them on, so a Python guest could make the host call a
// service named "" or "bad name!".
//
// Measured on this test's fixture before the change, for name "bad name!": the
// service caller received Service "bad name!", the promise store a promise of
// that name, and the signal store a delivery to that target, each with the SDK
// returning normally.
//
// Each case asserts the refusal for its result shape AND that no collaborator
// and no history event saw the call. The second half matters on its own: a
// refusal that still ran the call would pass the first.
func TestPythonGuestInvalidNamesAreRefusedAsTheCoreModulePathRefusesThem(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping Python WASM integration test in short mode")
	}

	caller := &mockCaller{}
	promises := &mockPromiseStore{}
	signals := &mockSignalStore{}
	var childStarts []string
	children := &mockChildStore{
		startChildAtomicFn: func(_ context.Context, _, _ /*child, parent*/, name, _ string, _ int, _ string, _ EventRecord, _ int) (string, error) {
			childStarts = append(childStarts, name)
			return "run-1", nil
		},
		startChildFn: func(_ context.Context, _ /*parent*/, name, _ string, _ int, _ string, _ int) (string, error) {
			childStarts = append(childStarts, name)
			return "run-1", nil
		},
	}
	wasmBytes, eng := pythonEntryEngine(t, "invalid_names_workflow.py", "probe", caller,
		WithPromiseStore(promises), WithSignalStore(signals), WithChildWorkflowStore(children))

	cases := []struct {
		kind    string
		names   []string
		refusal invalidNameRefusal
	}{
		{"call", []string{"", "bad name!"}, refusedAsCallError},
		{"call_op", []string{"", "bad name!"}, refusedAsCallError},
		{"plugin_call", []string{"bad name!"}, refusedAsCallError},
		{"child_workflow", []string{"bad name!"}, refusedAsCallError},
		{"await_promise", []string{"bad name!"}, refusedAsCallError},
		{"signal_workflow", []string{"", "bad name!"}, refusedAsBadParam},
		{"send", []string{"bad name!"}, refusedAsBadParam},
		{"resolve_promise", []string{"bad name!"}, refusedAsBadParam},
		{"run_detached", []string{"bad name!"}, refusedAsBadParam},
		{"create_promise", []string{"", "bad name!"}, refusedByTrap},
		{"poll_signal", []string{"bad name!"}, refusedByTrap},
		{"await_child", []string{"bad name!"}, refusedByTrap},
	}

	for _, c := range cases {
		for _, name := range c.names {
			in, _ := json.Marshal(map[string]string{"kind": c.kind, "name": name})
			result, hist, suspended, _, _, err := eng.Execute(context.Background(), wasmBytes, "probe", in)

			if c.refusal == refusedByTrap {
				if err == nil || !strings.Contains(err.Error(), "is not a valid name") {
					t.Errorf("%s(%q): want a trap naming the invalid name, got result=%q err=%v",
						c.kind, name, result, err)
				}
			} else {
				if err != nil {
					t.Errorf("%s(%q): Execute failed (%v); a name refused through a result type must reach the "+
						"guest as that result, not trap it", c.kind, name, err)
					continue
				}
				if suspended != nil {
					t.Errorf("%s(%q): the guest suspended (%s) instead of seeing a refusal", c.kind, name, suspended.Reason)
					continue
				}
				var got struct {
					Raised   string `json:"raised"`
					Message  string `json:"message"`
					Returned string `json:"returned"`
				}
				if jerr := json.Unmarshal([]byte(unwrapPythonResult(t, result)), &got); jerr != nil {
					t.Errorf("%s(%q): result %q is not the fixture's document: %v", c.kind, name, result, jerr)
					continue
				}
				if got.Raised == "" {
					t.Errorf("%s(%q): the SDK returned %q; the host accepted a name the core-module path refuses",
						c.kind, name, got.Returned)
				}
				if c.refusal == refusedAsCallError &&
					(got.Raised != "CleatCallPermanentError" || !strings.Contains(got.Message, "[4]") ||
						!strings.Contains(got.Message, "is not a valid name")) {
					t.Errorf("%s(%q): raised %s %q, want CleatCallPermanentError carrying code 4 (invalid request) and the name's refusal",
						c.kind, name, got.Raised, got.Message)
				}
			}
			if len(hist) != 0 {
				t.Errorf("%s(%q): the refusal recorded %d history event(s) %v; a call the host refused must leave nothing to replay",
					c.kind, name, len(hist), hist)
			}
		}
	}

	// The collaborators, once, after every case: nothing reached any of them.
	if len(caller.calls) != 0 {
		t.Errorf("the service caller was handed %d call(s) %v, want 0", len(caller.calls), caller.calls)
	}
	if promises.lastCreatedPromiseName != "" {
		t.Errorf("the promise store was asked to create %q, want nothing", promises.lastCreatedPromiseName)
	}
	if signals.lastDeliverWFID != "" || signals.lastDeliverName != "" {
		t.Errorf("the signal store was asked to deliver %q to %q, want nothing", signals.lastDeliverName, signals.lastDeliverWFID)
	}
	if len(childStarts) != 0 {
		t.Errorf("the child store started %v, want nothing", childStarts)
	}

	// Control: the same engine and guest still serve a valid call, and an EMPTY
	// optional name (set_scope's two) stays valid, so a refusal that fired on
	// everything would fail here.
	for _, kind := range []string{"call_valid", "set_scope_ok"} {
		in, _ := json.Marshal(map[string]string{"kind": kind, "name": "unused"})
		result, _, _, _, _, err := eng.Execute(context.Background(), wasmBytes, "probe", in)
		if err != nil {
			t.Fatalf("control %s: Execute: %v", kind, err)
		}
		if strings.Contains(result, "raised") {
			t.Errorf("control %s: the guest raised, so the validation refuses valid input: %s", kind, result)
		}
	}
	if len(caller.calls) != 1 || caller.calls[0].Service != "svc" || caller.calls[0].Op != "op" {
		t.Errorf("control: the valid call reached the caller as %v, want exactly svc.op", caller.calls)
	}

	// A refusal that cannot be carried as a result traps the guest, EXCEPT while
	// the guest drains its defers: there a trap would end the drain and lose every
	// cleanup queued after it. The first defer to run (last registered) sends an
	// invalid name; the one registered before it must still make its call.
	deferCaller := &mockCaller{}
	deferWasm, deferEng := pythonEntryEngine(t, "invalid_names_workflow.py", "defer_probe", deferCaller)
	if _, _, _, _, _, err := deferEng.Execute(context.Background(), deferWasm, "defer_probe", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("a defer body that sent an invalid name failed the run (%v); the refusal must answer \"\" there, not trap", err)
	}
	if got := operationsCalled(deferCaller); len(got) != 1 || got[0] != "after" {
		t.Errorf("defers made calls %v, want [after]: the cleanup registered before the refused defer did not run", got)
	}
}
