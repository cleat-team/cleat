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

// callOptionsProbeResult is what the calloptsonly fixture reports back.
type callOptionsProbeResult struct {
	Err  string `json:"err"`
	Resp string `json:"resp"`
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

	var got callOptionsProbeResult
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
