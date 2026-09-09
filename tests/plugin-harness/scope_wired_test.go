package pluginharness

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/cleat-team/cleat/cleat/wasmtest"
	"github.com/cleat-team/cleat/engine"
	"github.com/tetratelabs/wazero"
)

func buildScopeFixtureWasm(t *testing.T) []byte {
	t.Helper()
	dir := hostCallFixtureDir(t, "scopefixture")
	tmpDir := t.TempDir()
	cmd := commandAt(dir, cleatBinary(t), "build", "--target", "go", "-o", tmpDir, dir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("cleat build (scopefixture) failed:\n%s\n%v", string(out), err)
	}
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		t.Fatalf("reading cleat build output: %v", err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".wasm" {
			b, rerr := os.ReadFile(filepath.Join(tmpDir, e.Name()))
			if rerr != nil {
				t.Fatalf("reading built wasm: %v", rerr)
			}
			return b
		}
	}
	t.Fatalf("no .wasm in cleat build output: %s", tmpDir)
	return nil
}

// TestACompiledGoWorkflowImportsTheScopeCalls is cleat#984, answered with the
// same instrument that established it.
//
// IMPROVEMENT-PLAN 3.223 did not conclude the gap from tables. It built a Go
// workflow whose body was h.SetScope(obj, key) and read the produced binary:
//
//	imports wired:   cleat_complete, cleat_log, cleat_poll_work
//	adapter fields:  DurableLog
//
// cleat_set_scope was absent entirely, so HostCallsImpl.SetScope was setting
// three local fields against a call that had never been generated. Every table
// in wasm/ is upstream of that binary, and a fix touches those tables -- so a
// test that reads them would pass on a tree where the generator still emitted
// nothing. This reads the artifact.
func TestACompiledGoWorkflowImportsTheScopeCalls(t *testing.T) {
	wasmBytes := buildScopeFixtureWasm(t)

	ctx := context.Background()
	rt := wazero.NewRuntime(ctx)
	defer rt.Close(ctx)

	compiled, err := rt.CompileModule(ctx, wasmBytes)
	if err != nil {
		t.Fatalf("compiling the built fixture: %v", err)
	}
	defer compiled.Close(ctx)

	imports := map[string]bool{}
	for _, fn := range compiled.ImportedFunctions() {
		if mod, name, ok := fn.Import(); ok && mod == "env" {
			imports[name] = true
		}
	}
	if len(imports) == 0 {
		t.Fatal(`the built fixture imports nothing from "env" -- the binary was ` +
			"not read correctly and this test would pass no matter what was in it")
	}

	var names []string
	for n := range imports {
		names = append(names, n)
	}
	sort.Strings(names)

	for _, want := range []string{"cleat_set_scope", "cleat_get_scope"} {
		if !imports[want] {
			t.Errorf("the compiled workflow does not import %q.\n"+
				"It calls h.SetScope/h.GetScope/h.ClearScope, so a missing import "+
				"means those methods set local fields and take no lock -- which is "+
				"exactly cleat#984 / IMPROVEMENT-PLAN 3.223.\nimports from env: %v",
				want, names)
		}
	}
}

// TestACompiledGoWorkflowActuallyReachesTheHostForScope asserts on evidence the
// guest cannot manufacture.
//
// The obvious test -- run the fixture and check SetScope/GetScope/ClearScope
// return sensible values -- is VACUOUS for this purpose, and measured to be so.
// HostCallsImpl still keeps a local mirror of the scope, so with the
// wasm/usage.go rows removed and nothing generated, every one of those
// assertions still passes. That version was written first and it went green on
// an unwired tree, which is the whole failure this fix is about: local fields
// answering for a host call that was never made.
//
// EventTypeScopeAcquired is produced by engine/scope.go freshSetScope, after it
// takes the concurrency key. No guest-side mirror can put one in the history.
func TestACompiledGoWorkflowActuallyReachesTheHostForScope(t *testing.T) {
	env := NewTestPluginEnvInMemory(t)
	defer env.Close()

	wasmBytes := buildScopeFixtureWasm(t)

	wenv := wasmtest.NewWasmTestEnv(t, wasmtest.WithPluginRegistry(env.Registry))
	defer wenv.Close()

	result, history, err := wenv.Execute(t, wasmBytes, "exercise_scope", `{}`)
	if err != nil {
		t.Fatalf("engine refused the fixture: %v", err)
	}
	if len(history) == 0 {
		t.Fatal("the run produced no history at all -- nothing below would be " +
			"measuring the engine")
	}

	var acquired []string
	for _, rec := range history {
		if rec.EventType == engine.EventTypeScopeAcquired {
			acquired = append(acquired, rec.ScopeKey)
		}
	}
	want := []string{"vo:cart:c1", "vo:cart:c2"}
	if !reflect.DeepEqual(acquired, want) {
		t.Errorf("scope-acquired events = %v, want %v.\n"+
			"These are written by engine/scope.go freshSetScope when it takes the "+
			"concurrency key. None means the guest never called cleat_set_scope and "+
			"HostCallsImpl answered from its local mirror -- cleat#984 exactly.\n"+
			"result: %s", acquired, want, result)
	}

	// The SDK-level values still matter; they are just not evidence of the
	// above on their own.
	var got struct {
		FirstPrev  string `json:"firstPrev"`
		SecondPrev string `json:"secondPrev"`
		ObjType    string `json:"objType"`
		InstKey    string `json:"instKey"`
		Cleared    string `json:"cleared"`
		AfterType  string `json:"afterType"`
		AfterKey   string `json:"afterKey"`
	}
	if uerr := json.Unmarshal([]byte(result), &got); uerr != nil {
		t.Fatalf("fixture result did not decode: %v\nraw: %s", uerr, result)
	}
	if got.FirstPrev != "" {
		t.Errorf("first SetScope reported a previous scope %q; nothing was set before it", got.FirstPrev)
	}
	if w := "vo:cart:c1:"; got.SecondPrev != w {
		t.Errorf("replacing the scope reported previous %q, want %q -- the value "+
			"cleat_set_scope writes, and reported a zero length for until #1043",
			got.SecondPrev, w)
	}
	if got.ObjType != "cart" || got.InstKey != "c2" {
		t.Errorf("GetScope reported (%q, %q), want (cart, c2)", got.ObjType, got.InstKey)
	}
	if w := "vo:cart:c2:"; got.Cleared != w {
		t.Errorf("ClearScope reported previous %q, want %q", got.Cleared, w)
	}
	if got.AfterType != "" || got.AfterKey != "" {
		t.Errorf("after ClearScope, GetScope reported (%q, %q), want empty", got.AfterType, got.AfterKey)
	}
}
