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

// cleat#4113, Go guest: a side effect FREEZES a value. The function runs exactly once across the first run and a replay, and the replay
// returns the recorded value. Driven through the real engine and wasmtime backend with a Go guest built by `cleat build`.

type sideEffectOutcome struct {
	Value string `json:"value"`
	Ran   int    `json:"ran"`
}

func buildSideEffectGoWasm(t *testing.T) []byte {
	t.Helper()
	dir := hostCallFixtureDir(t, "sideeffectgo")
	out := t.TempDir()
	cmd := commandAt(dir, cleatBinary(t), "build", "--target", "go", "-o", out, dir)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cleat build (go) failed:\n%s\n%v", b, err)
	}
	entries, err := os.ReadDir(out)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".wasm" {
			b, err := os.ReadFile(filepath.Join(out, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			return b
		}
	}
	t.Fatalf("no .wasm in %s", out)
	return nil
}

func TestSideEffectGoFunctionRunsOnceAcrossFirstRunAndReplay(t *testing.T) {
	env := NewTestPluginEnvInMemory(t)
	defer env.Close()
	wenv := wasmtest.NewWasmTestEnv(t, wasmtest.WithPluginRegistry(env.Registry))
	defer wenv.Close()
	wasmBytes := buildSideEffectGoWasm(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	first, history, suspended, _, _, err := wenv.H().Execute(ctx, wasmBytes, "side_effect_once", json.RawMessage(`{}`))
	if err != nil || suspended != nil {
		t.Fatalf("first run: err=%v suspended=%v", err, suspended)
	}
	var a sideEffectOutcome
	if err := json.Unmarshal([]byte(first), &a); err != nil {
		t.Fatalf("first run result %q: %v", first, err)
	}
	if a.Ran != 1 || a.Value == "" {
		t.Fatalf("first run = %+v, want the function run once and a value", a)
	}

	replayed, _, rsuspended, _, _, err := wenv.H().Replay(ctx, wasmBytes, "side_effect_once", json.RawMessage(`{}`), history)
	if err != nil || rsuspended != nil {
		t.Fatalf("replay: err=%v suspended=%v (a replay must not fail on a value that is not reproducible)", err, rsuspended)
	}
	var b sideEffectOutcome
	if err := json.Unmarshal([]byte(replayed), &b); err != nil {
		t.Fatalf("replay result %q: %v", replayed, err)
	}
	if b.Value != a.Value {
		t.Errorf("replay returned %q, want the recorded %q", b.Value, a.Value)
	}
	if b.Ran != 0 {
		t.Errorf("the function ran %d time(s) in the replay, want 0: across the first run and the replay it must run exactly once", b.Ran)
	}
}
