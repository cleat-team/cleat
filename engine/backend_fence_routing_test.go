package engine

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// The language string that selects an execution path comes from the guest's own
// "cleat.metadata" custom section, verbatim and unvalidated
// (wasm.DetectLanguage, wasm/metadata.go). These tests pin what happens when it
// names something the engine has no backend for.
//
// Before the fix, an engine registered exactly as cmd/cleat-worker registers one
// (WithBackends(WasmtimeLanguages, ...) and a nil Runtime) handled that case
// three different ways:
//
//	RunDefer  compiled and ran the guest on a wazero Runtime it created on
//	          demand. CLAUDE.md records that wazero cannot be fenced for a
//	          compute-bound guest -- measured three ways, all failing -- so a
//	          guest-supplied string selected an unstoppable runtime.
//	Replay    dereferenced the nil e.rt and panicked.
//	Execute   returned a clean error.
//
// tiers.yaml grants no language outside WasmtimeLanguages, so failing closed
// rejects only what was never claimed to work.

// workerShapedEngine builds an Engine the way cmd/cleat-worker does: one backend
// registered for every language in WasmtimeLanguages, and no wazero Runtime.
func workerShapedEngine() *Engine {
	return NewEngine(nil, &mockCaller{},
		WithBackends(WasmtimeLanguages, &mockBackend{name: "wasmtime-stub"}))
}

func TestUnroutableGuestLanguageIsRejectedNotRunUnfenced(t *testing.T) {
	ctx := context.Background()

	// "GO" rather than an obviously bogus name: the lookup is exact, so a
	// legitimate language in the wrong case is equally unroutable. This is a
	// plausible toolchain slip, not only an adversarial one.
	for _, lang := range []string{"cobol", "tinygo", "GO"} {
		t.Run(lang, func(t *testing.T) {
			eng := workerShapedEngine()
			mod := wasmWithLanguage(lang)

			_, err := eng.RunDefer(ctx, mod, "defer-1", json.RawMessage(`{}`))
			if err == nil {
				t.Fatalf("RunDefer accepted guest language %q. Before the fix this "+
					"compiled and ran the guest on a wazero Runtime created on demand, "+
					"which cannot be fenced for a compute-bound guest.", lang)
			}
			if !strings.Contains(err.Error(), "no WASM backend registered") {
				t.Errorf("RunDefer error = %v, want the fail-closed message naming the "+
					"unroutable language", err)
			}
			if !strings.Contains(err.Error(), lang) {
				t.Errorf("RunDefer error %v does not name the offending language %q, "+
					"which is the one thing an operator needs from it", err, lang)
			}

			// Replay: the assertion is as much "does not panic" as "returns an
			// error". The worker constructs its engines with a nil Runtime, so
			// this path used to dereference nil.
			_, _, _, _, _, rErr := eng.Replay(ctx, mod, "run", json.RawMessage(`{}`), nil)
			if rErr == nil {
				t.Errorf("Replay accepted guest language %q", lang)
			}

			_, _, _, _, _, eErr := eng.Execute(ctx, mod, "run", json.RawMessage(`{}`))
			if eErr == nil {
				t.Errorf("Execute accepted guest language %q", lang)
			}
		})
	}
}

// TestRoutableGuestLanguagesStillExecute is the control. Without it, the test
// above passes against a build that rejects everything.
//
// It asserts the backend actually ran, not merely that no error came back: an
// engine that silently did nothing would also return nil.
func TestRoutableGuestLanguagesStillExecute(t *testing.T) {
	ctx := context.Background()
	for _, lang := range WasmtimeLanguages {
		t.Run(lang, func(t *testing.T) {
			backend := &mockBackend{name: "wasmtime-stub"}
			eng := NewEngine(nil, &mockCaller{}, WithBackends(WasmtimeLanguages, backend))

			if _, err := eng.RunDefer(ctx, wasmWithLanguage(lang), "defer-1", json.RawMessage(`{}`)); err != nil {
				t.Fatalf("RunDefer(%s): %v", lang, err)
			}
			if backend.executeCalled == 0 {
				t.Errorf("RunDefer(%s) returned nil without calling the backend, so this "+
					"control would pass against an engine that executed nothing", lang)
			}
		})
	}
}

// TestEngineWithoutBackendsKeepsTheRuntimePath guards the case the fix must not
// break. cmd/cleatctl replay|debug, cmd/cleat run_embedded and cmd/cleat-bench
// all construct an Engine with a real wazero Runtime and no backends at all.
// For them a nil backend is not a routing failure, it is the design, and
// resolveBackend must return (nil, nil) rather than failing closed.
func TestEngineWithoutBackendsKeepsTheRuntimePath(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	eng := NewEngine(rt, &mockCaller{})
	if len(eng.backends) != 0 {
		t.Fatalf("expected no registered backends, got %d", len(eng.backends))
	}

	backend, err := eng.resolveBackend(wasmWithLanguage("cobol"))
	if err != nil {
		t.Errorf("resolveBackend failed closed on an engine that registers no backends: %v\n\n"+
			"That engine shape is cmd/cleatctl replay|debug, cmd/cleat run_embedded and "+
			"cmd/cleat-bench. For them the wazero Runtime is the intended executor, not a "+
			"fallback, so this must return (nil, nil).", err)
	}
	if backend != nil {
		t.Errorf("resolveBackend returned a backend %v from an engine with none", backend)
	}
}
