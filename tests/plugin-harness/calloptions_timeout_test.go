package pluginharness

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cleat-team/cleat/cleat/wasmtest"
)

// callerDelay is how long harness-service takes to answer. It has to be
// comfortably longer than the short case's 50ms timeout and comfortably
// shorter than the controls' 30s, so no assertion here depends on a margin.
const callerDelay = 2 * time.Second

type timeoutProbeResult struct {
	Case      string `json:"case"`
	Err       string `json:"err"`
	IsTimeout bool   `json:"is_timeout"`
	Resp      string `json:"resp"`
}

func buildFixtureWasm(t *testing.T, name string) []byte {
	t.Helper()
	dir := hostCallFixtureDir(t, name)
	tmpDir := t.TempDir()
	cmd := commandAt(dir, cleatBinary(t), "build", "--target", "go", "-o", tmpDir, dir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("cleat build (%s) failed:\n%s\n%v", name, string(out), err)
	}
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		t.Fatalf("reading cleat build output: %v", err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".wasm" {
			b, err := os.ReadFile(filepath.Join(tmpDir, e.Name()))
			if err != nil {
				t.Fatalf("reading WASM: %v", err)
			}
			return b
		}
	}
	t.Fatalf("no .wasm in cleat build output: %s", tmpDir)
	return nil
}

func runTimeoutCase(t *testing.T, wenv *wasmtest.WasmTestEnv, wasmBytes []byte, name string) (timeoutProbeResult, time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	input, err := json.Marshal(map[string]string{"case": name})
	if err != nil {
		t.Fatalf("%s: marshalling input: %v", name, err)
	}
	start := time.Now()
	result, _, suspended, _, _, err := wenv.H().Execute(ctx, wasmBytes, "probe_call_timeout", input)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("%s: engine refused the fixture: %v", name, err)
	}
	// If the caller did not actually block, every case returns instantly and
	// "the timeout did not fire" is indistinguishable from "there was nothing
	// to time out". This is the probe's own negative control.
	if elapsed < callerDelay {
		t.Fatalf("%s: the whole execution took %v, less than the caller's %v delay -- "+
			"the delay did not take effect and this case measures nothing",
			name, elapsed, callerDelay)
	}
	if suspended != nil {
		t.Fatalf("%s: workflow suspended (%s); this fixture is not meant to suspend",
			name, suspended.Reason)
	}
	var got timeoutProbeResult
	if err := json.Unmarshal([]byte(result), &got); err != nil {
		t.Fatalf("%s: undecodable fixture result %q: %v", name, result, err)
	}
	return got, elapsed
}

// TestCallOptionsTimeoutAtTheWasmBoundary measures whether CallOptions.Timeout
// is enforced for a workflow compiled through `cleat build --target go` and
// run on the engine. It is cleat#1006, and it PINS THE DEFECT: a 50ms timeout
// does not interrupt a 2s call.
//
// WHY THIS IS NOT COVERED BY cleat/runtime_test.go. Those tests call
// HostCallsImpl.DurableCallWithOptions directly, in a native binary. The
// enforcement path is a goroutine racing time.After, and both halves of that
// race are runtime facts: a native test has real threads and a caller that
// returns on another goroutine, a wasip1 guest has neither guaranteed. Every
// per-side test is green and nothing spans the boundary -- which is the shape
// that has cost this project twice.
//
// The generated adapter does not bind this method at all: `cleat build` emits
// no DurableCallWithOptions field (the string "CallOptions" appears zero times
// in gen_host_adapter.go, measured 2026-09-08 on the hostcallsgo fixture), so
// h.durableCallWithOptions is nil and the in-SDK branch is what runs. This
// test is about that branch, on this runtime.
func TestCallOptionsTimeoutAtTheWasmBoundary(t *testing.T) {
	env := NewTestPluginEnvInMemory(t)
	defer env.Close()

	wasmBytes := buildFixtureWasm(t, "calloptstimeout")

	wenv := wasmtest.NewWasmTestEnv(t, wasmtest.WithPluginRegistry(env.Registry))
	defer wenv.Close()
	wenv.Caller().SetDelay(callerDelay)

	for _, name := range []string{"plain-call", "short-timeout", "long-timeout", "no-timeout"} {
		t.Run(name, func(t *testing.T) {
			got, elapsed := runTimeoutCase(t, wenv, wasmBytes, name)
			t.Logf("elapsed=%v is_timeout=%v err=%q resp=%q",
				elapsed.Round(10*time.Millisecond), got.IsTimeout, got.Err, got.Resp)

			switch name {
			case "short-timeout":
				// THE DEFECT. A 50ms timeout does not interrupt a call the
				// host takes 2s to answer: the call returns the response,
				// after the full delay, with no error.
				//
				// The mechanism is that enforcement is a goroutine racing
				// time.After while the guest sits inside a synchronous
				// wasmimport. The timer cannot be serviced until the import
				// returns, by which time there is nothing left to interrupt.
				// cleat/runtime_test.go covers the same source natively and
				// passes, because there the caller really does return on
				// another goroutine.
				if got.IsTimeout {
					t.Fatalf("the 50ms timeout FIRED against a %v call (err=%q).\n\n"+
						"That is the correct behaviour and this test is now wrong: "+
						"CallOptions.Timeout is enforced at the WASM boundary. Invert "+
						"this case -- assert IsTimeout and an empty response -- and "+
						"say so on the issue.", callerDelay, got.Err)
				}
				if got.Err != "" {
					t.Fatalf("the call failed for some OTHER reason: %q.\n\n"+
						"This case pins a timeout that does not fire; an unrelated "+
						"failure means it is no longer measuring that.", got.Err)
				}
				if got.Resp != "{}" {
					t.Errorf("expected the caller's response %q, got %q -- the call did "+
						"not time out and did not return the answer either", "{}", got.Resp)
				}
				if elapsed < callerDelay {
					t.Errorf("returned in %v, before the caller's %v delay: something "+
						"short-circuited the call and this case is not measuring a "+
						"timeout that failed to fire", elapsed, callerDelay)
				}

			default:
				// Controls. plain-call proves the fixture and the service
				// work; the two long/absent-timeout cases prove a WithOptions
				// call is not simply broken. Without these, "no timeout
				// fired" would be satisfied by a build where nothing ran.
				if got.Err != "" {
					t.Errorf("control case failed: %q.\n\n"+
						"Every control here must succeed, or the short-timeout case "+
						"above is not evidence about timeouts.", got.Err)
				}
				if got.IsTimeout {
					t.Errorf("control case reported a timeout, which no control should")
				}
				if got.Resp != "{}" {
					t.Errorf("control case: expected response %q, got %q", "{}", got.Resp)
				}
			}
		})
	}
}

// TestDurableCallWithOptionsAloneIsBound is cleat#1005, FIXED. It was written
// to pin the defect and is now inverted, which is what it asked for.
//
// A workflow whose ONLY durable call is DurableCallWithOptions must be able to
// make it. It could not, and the cause was not where the issue first said.
//
// hostFunctions is a one-to-many relation and the usage scan flattened it into
// a map[string]string, so of the two rows naming DurableCallWithOptions --
// {cleat_call, ...} and {cleat_call_retry, ...} -- only the last survived.
// cleat_call was never marked used, the adapter emitted no DurableCall field,
// and HostCallsImpl's own DurableCallWithOptions delegates to exactly that.
// The call failed at RUN time with "the HostCalls runtime was not
// initialized", which names the entry point and points away from the binding.
//
// WHY IT NEEDS ITS OWN FIXTURE PACKAGE, still. The binding is a property of the
// whole module: any workflow that also calls h.DurableCall for its own reasons
// marks the import used by another route and the defect disappears. The
// sibling fixture calloptstimeout does exactly that, so it could never have
// caught this. One package per binding state is the only way to hold both.
func TestDurableCallWithOptionsAloneIsBound(t *testing.T) {
	env := NewTestPluginEnvInMemory(t)
	defer env.Close()

	wasmBytes := buildFixtureWasm(t, "calloptsonly")

	wenv := wasmtest.NewWasmTestEnv(t, wasmtest.WithPluginRegistry(env.Registry))
	defer wenv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	result, _, suspended, _, _, err := wenv.H().Execute(ctx, wasmBytes, "call_with_options_only", []byte(`{}`))
	if err != nil {
		t.Fatalf("engine refused the fixture: %v", err)
	}
	if suspended != nil {
		t.Fatalf("workflow suspended (%s); this fixture is not meant to suspend", suspended.Reason)
	}

	var got timeoutProbeResult
	if err := json.Unmarshal([]byte(result), &got); err != nil {
		t.Fatalf("undecodable fixture result %q: %v", result, err)
	}

	if got.Err != "" {
		t.Fatalf("DurableCallWithOptions failed in a workflow that makes no other "+
			"durable call: %s\n\n"+
			"If the message mentions \"HostCalls runtime was not initialized\", "+
			"cleat#1005 has regressed: check that fieldImports() in wasm/usage.go "+
			"still returns EVERY hostFunctions row for a field rather than the "+
			"last one. TestEveryHostFunctionRowReachesTheUsageScan covers that "+
			"directly and should have failed first.", got.Err)
	}
	if got.Resp != "{}" {
		t.Errorf("expected the caller's response %q, got %q -- the call was made "+
			"but did not return the answer", "{}", got.Resp)
	}
}
