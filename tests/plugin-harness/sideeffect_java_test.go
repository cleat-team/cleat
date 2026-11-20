package pluginharness

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/cleat-team/cleat/cleat/wasmtest"
)

// javaSideEffectOutcome is what the Java entry reports. Declared here, not shared with the Go test's own type of the same shape, so the two
// SDK PRs for cleat#4113 do not depend on each other.
type javaSideEffectOutcome struct {
	Value string `json:"value"`
	Ran   int    `json:"ran"`
}

// cleat#4113, Java guest: HostCalls.sideEffectOnce asks the host first, so its function runs exactly once across a first run and a replay and
// the replay returns the recorded value. Driven through the real engine and wasmtime backend with a Java guest built by `cleat build`.
func TestSideEffectJavaFunctionRunsOnceAcrossFirstRunAndReplay(t *testing.T) {
	env := NewTestPluginEnvInMemory(t)
	defer env.Close()
	wenv := wasmtest.NewWasmTestEnv(t, wasmtest.WithPluginRegistry(env.Registry))
	defer wenv.Close()
	wasmBytes := buildJavaHostCallWasm(t)

	// Java returns the entry's value as a JSON-ENCODED STRING (see executeOneCall's resultJSONWrapped).
	read := func(raw string) javaSideEffectOutcome {
		t.Helper()
		var inner string
		if err := json.Unmarshal([]byte(raw), &inner); err != nil {
			t.Fatalf("Java returns a JSON-encoded string and %q is not one: %v", raw, err)
		}
		var o javaSideEffectOutcome
		if err := json.Unmarshal([]byte(inner), &o); err != nil {
			t.Fatalf("result %q: %v", inner, err)
		}
		return o
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	first, history, suspended, _, _, err := wenv.H().Execute(ctx, wasmBytes, "side_effect_once", json.RawMessage(`{}`))
	if err != nil || suspended != nil {
		t.Fatalf("first run: err=%v suspended=%v", err, suspended)
	}
	a := read(first)
	if a.Ran != 1 || a.Value != "computed-in-run-1" {
		t.Fatalf("first run = %+v, want the function run once", a)
	}

	replayed, _, rsuspended, _, _, err := wenv.H().Replay(ctx, wasmBytes, "side_effect_once", json.RawMessage(`{}`), history)
	if err != nil || rsuspended != nil {
		t.Fatalf("replay: err=%v suspended=%v", err, rsuspended)
	}
	b := read(replayed)
	if b.Value != a.Value {
		t.Errorf("replay returned %q, want the recorded %q", b.Value, a.Value)
	}
	if b.Ran != 0 {
		t.Errorf("the function ran %d time(s) in the replay, want 0: across the first run and the replay it must run exactly once", b.Ran)
	}
}
