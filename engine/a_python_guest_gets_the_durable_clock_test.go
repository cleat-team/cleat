//go:build cgo

package engine

// cleat#1410: a Python guest's clock and entropy do not come from cleat.
//
// componentize-py's CPython satisfies time.time(), random.random() and
// os.urandom() through WASI Preview 2 interfaces -- wasi:clocks/wall-clock and
// wasi:random/random -- which cleat does not register on the component linker.
// So they reach the host's real clock and the host's real entropy, and a replay
// of the same workflow sees different values.
//
// MEASURED BEFORE ANY FIX, two executions of one workflow id with the durable
// clock pinned to 2001-09-09T01:46:40Z:
//
//	durable_now_ms       same    1e12 / 1e12
//	durable_random       same    6.800248775910453e18 / same
//	wall_clock_seconds   DIFFER  1.7895622924e9 / 1.7895622951e9
//	random_float         DIFFER  0.36714823678504416 / 0.35755472097871754
//	urandom_hex          DIFFER  715cc1e57e87429b / fd6da07dc1c2602d
//
// THE FIRST TWO ROWS ARE WHY THE OTHER THREE MEAN ANYTHING. They are cleat's
// own host calls, read by the same guest in the same two runs, and they are
// stable -- so the divergence below is the guest reaching past cleat, not a
// harness that fails to reproduce anything.
//
// WHY `time.time()` IS ASSERTED AGAINST A VALUE THE TEST CHOSE, and not merely
// against itself across two runs. "Two runs agree" is satisfied by any caching
// and by shadowing with the wrong source; equality with a pinned durable clock
// can only hold if cleat's implementation is the one bound. This issue's own
// history is the argument: the import version is @0.2.9, and registering
// @0.2.0 shadows nothing, raises no error, and leaves the guest on the real
// clock -- with "registration succeeded" passing throughout.
//
// MEASURED, by doing exactly that: with wasiDeterminismVersion set to @0.2.0
// the registration returns no error and these two tests fail, reporting the
// real wall clock and divergent entropy. That is the known-positive for this
// file -- a case already proven broken, checked to see the tests report it.
//
// WHY THERE IS NO SEPARATE "the registered names match the guest's imports"
// GUARD. It was the plan, and it is redundant against this: a name that does
// not match produces exactly the failure above, because the effect is what is
// asserted rather than the registration. A name guard would add a clearer
// message and no detection, so the message went here instead.

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"testing"
)

// pinnedClockMs is 2001-09-09T01:46:40Z: far from any real clock reading, so a
// failure reports an obviously-wrong number rather than a plausible one.
const pinnedClockMs = int64(1000000000000)

func runPythonDeterminismProbe(t *testing.T, wfID string) map[string]any {
	t.Helper()
	ctx := context.Background()

	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	t.Cleanup(func() { rt.Close(ctx) })
	wt, err := NewWasmtimeBackend(ctx)
	if err != nil {
		t.Fatalf("NewWasmtimeBackend: %v", err)
	}
	t.Cleanup(func() { wt.Close(ctx) })

	eng := NewEngine(rt, &mockCaller{},
		WithBackends(WasmtimeLanguages, wt),
		WithWorkflowID(wfID),
		WithWorkflowStartTime(pinnedClockMs),
		WithClock(func() int64 { return pinnedClockMs }),
	)
	res, _, _, _, _, err := eng.Execute(ctx, pythonDeterminismWasm(t), "run", []byte(`{}`))
	if err != nil {
		t.Fatalf("execute %s: %v", wfID, err)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(res), &m); err != nil {
		t.Fatalf("result %q is not an object: %v", res, err)
	}
	return m
}

// pythonDeterminismWasm compiles the probe once per test binary. componentize-py
// takes about ten seconds and its output is not reproducible byte-for-byte
// (WORKSTREAM.md), so the bytes are built once and compared by OUTCOME, never
// by digest.
var pythonDeterminismWasmBytes []byte

func pythonDeterminismWasm(t *testing.T) []byte {
	t.Helper()
	if pythonDeterminismWasmBytes != nil {
		return pythonDeterminismWasmBytes
	}
	h := newPythonWasmTestHelper(t)
	if !h.toolsAvailable() {
		if toolchainRequired("python") {
			t.Fatalf("Python WASM prerequisites not met, but %s declares python: %s",
				requireToolchainEnv, h.missingTools())
		}
		t.Skip("Python WASM prerequisites not met: " + h.missingTools())
	}
	path := h.compileWorkflow(t, "determinism_workflow.py", "determinism_probe")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the compiled probe: %v", err)
	}
	pythonDeterminismWasmBytes = b
	return b
}

func TestAPythonGuestGetsTheDurableClock(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping Python WASM integration test in short mode")
	}
	got := runPythonDeterminismProbe(t, "wf-python-durable-clock")

	// PRECONDITION. cleat's own host calls, read by this guest in this run.
	// If the durable clock is not the pinned value, the pin did not take and
	// the assertion below would be comparing two unpinned numbers.
	nowMs, ok := got["durable_now_ms"].(float64)
	if !ok || int64(nowMs) != pinnedClockMs {
		t.Fatalf("PRECONDITION FAILED: the guest's durable clock reads %v, want %d. The pin "+
			"did not take, so nothing below was measured.", got["durable_now_ms"], pinnedClockMs)
	}

	seconds, ok := got["wall_clock_seconds"].(float64)
	if !ok {
		t.Fatalf("wall_clock_seconds is %T (%v), want a number", got["wall_clock_seconds"], got["wall_clock_seconds"])
	}
	want := float64(pinnedClockMs) / 1000
	if math.Abs(seconds-want) > 1 {
		t.Errorf("the guest's time.time() reads %.6f, want %.6f (the pinned durable clock).\n\n"+
			"A Python guest satisfies time.time() through wasi:clocks/wall-clock, which cleat "+
			"does not register on the component linker -- so it reads the host's real clock "+
			"and a replay diverges. cleat#1410.\n\n"+
			"If the difference is about %.0f seconds, that is the real wall clock, and the "+
			"FIRST thing to check is the version in the registered interface name. The "+
			"linker matches it exactly, so wasiDeterminismVersion = %q against a guest "+
			"importing a different one shadows nothing and returns no error. Confirm with\n"+
			"  wasm-tools component wit <guest.wasm> | grep -o 'wasi:clocks/wall-clock@[0-9.]*'",
			seconds, want, seconds-want, wasiDeterminismVersion)
	}
}

func TestAPythonGuestsRandomnessIsReproducible(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping Python WASM integration test in short mode")
	}
	// The SAME workflow id twice. cleat's durable random is seeded from it, so
	// two runs of one id must agree on everything a replay would.
	a := runPythonDeterminismProbe(t, "wf-python-random")
	b := runPythonDeterminismProbe(t, "wf-python-random")

	// PRECONDITION, and it is the same one: the durable host call is what says
	// the harness reproduces anything at all.
	if a["durable_random"] != b["durable_random"] {
		t.Fatalf("PRECONDITION FAILED: cleat's own durable random differs between two runs of "+
			"one workflow id (%v vs %v). The harness is not reproducing, so nothing below "+
			"was measured.", a["durable_random"], b["durable_random"])
	}

	for _, c := range []struct{ key, why string }{
		{"random_float", "CPython seeds the Mersenne Twister from os.urandom at import, so this " +
			"is get-random-bytes one step removed -- and it is why shadowing get-random-u64 " +
			"alone is not a shippable slice"},
		{"urandom_hex", "os.urandom goes straight to wasi:random/random#get-random-bytes"},
	} {
		if a[c.key] != b[c.key] {
			t.Errorf("%s differs between two runs of one workflow id: %v vs %v.\n\n%s\n\n"+
				"cleat#1410.", c.key, a[c.key], b[c.key], c.why)
		}
	}
}
