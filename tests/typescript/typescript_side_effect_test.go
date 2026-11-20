package typescript

// cleat#4113, TypeScript guest: a side effect FREEZES a value. The function must run exactly once across a first run and a replay, and the
// replay must return the recorded value. The workflow counts runs in the module (a fresh instance per execution), so the first run reports
// ran=1 and a replay that did not call the function reports ran=0.

import (
	"context"
	"encoding/json"
	"os"
	"testing"
)

func TestTypeScriptSideEffectFunctionRunsOnceAcrossFirstRunAndReplay(t *testing.T) {
	root := needToolchain(t)
	res := build(t, root, "tests/typescript/testdata/workflows/side_effect.ts", "side_effect_once")
	if res.exit != 0 {
		t.Fatalf("build failed (exit %d):\n%s", res.exit, res.output)
	}
	wasm, err := os.ReadFile(res.module)
	if err != nil {
		t.Fatal(err)
	}
	e, closeRT := newEngineWith(t)
	defer closeRT()

	type outcome struct {
		Value string `json:"value"`
		Ran   int    `json:"ran"`
	}
	read := func(raw string) outcome {
		t.Helper()
		var o outcome
		if err := json.Unmarshal([]byte(raw), &o); err != nil {
			t.Fatalf("result %q: %v", raw, err)
		}
		return o
	}

	input := json.RawMessage(`{}`)
	first, history, susp, _, _, err := e.Execute(context.Background(), wasm, "side_effect_once", input)
	if err != nil || susp != nil {
		t.Fatalf("first run: err=%v suspended=%v", err, susp)
	}
	a := read(first)
	if a.Ran != 1 || a.Value != "computed-in-run-1" {
		t.Fatalf("first run = %+v, want the function run once", a)
	}

	replayed, _, rsusp, _, _, err := e.Replay(context.Background(), wasm, "side_effect_once", input, history)
	if err != nil || rsusp != nil {
		t.Fatalf("replay: err=%v suspended=%v", err, rsusp)
	}
	b := read(replayed)
	if b.Value != a.Value {
		t.Errorf("the replay returned %q, want the recorded %q", b.Value, a.Value)
	}
	if b.Ran != 0 {
		t.Errorf("the function ran %d time(s) in the replay, want 0: across the first run and the replay it must run exactly once", b.Ran)
	}
}
