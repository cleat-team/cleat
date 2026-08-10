package main

import (
	"os"
	"testing"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/wasm"
)

// TestRealFixturesRouteToWasmtime is the guard on the trap that made this
// change necessary.
//
// AssemblyScript and TeaVM Java reached the wasmtime backend only because
// DetectLanguage could not identify them and defaulted to "go". Fixing the
// import parser they depend on makes the detection correct -- and, on its own,
// would have routed both to wazero, because the backend map held "go" alone.
//
// So this asserts the end state, from real toolchain output: detect the
// language correctly AND still serve it from wasmtime. Asserting only
// DetectLanguage would pass while the languages silently moved to the fallback
// runtime; asserting only the map would not notice detection regressing.
func TestRealFixturesRouteToWasmtime(t *testing.T) {
	// Tracked fixtures only. examples/rust-workflow builds under **/target/,
	// which .gitignore excludes, so depending on it here passes locally after a
	// cargo build and fails in CI. Rust's routing is asserted directly in
	// TestRustIsNotYetOnWasmtime, which needs no artifact.
	cases := []struct {
		name, path, wantLang string
		wantWasmtime         bool
	}{
		{"assemblyscript", "../../tests/plugin-harness/testdata/asworkflow/dist/workflow.wasm", "assemblyscript", true},
		{"java-teavm", "../../tests/plugin-harness/testdata/javaworkflow/build/wasm/wasm/workflow.wasm", "java", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, err := os.ReadFile(tc.path)
			if err != nil {
				t.Fatalf("fixture %s is missing; it is tracked in git and this "+
					"test cannot mean anything without it: %v", tc.path, err)
			}
			lang := wasm.DetectLanguage(b)
			if lang != tc.wantLang {
				t.Errorf("DetectLanguage = %q, want %q", lang, tc.wantLang)
			}
			if got := engine.RunsOnWasmtime(lang); got != tc.wantWasmtime {
				t.Errorf("engine.RunsOnWasmtime(%q) = %v, want %v", lang, got, tc.wantWasmtime)
			}
		})
	}
}

// TestRustRunsOnWasmtime pins Rust's move onto the primary backend.
//
// It is gated by real coverage rather than by the probe that suggested it:
// tests/cross-language now builds the same wasm32-unknown-unknown cdylib that
// `cleat build --target rust` ships, and all seven of its tests pass on
// wasmtime, including both cross-replay directions. TestPluginCalls_Wasm_Rust
// covers it too.
//
// If this fails because someone moved Rust back, the reason belongs in the
// comment on engine.WasmtimeLanguages -- the previous exclusion note claimed
// wasmtime-go crashed on Rust cdylib modules, which stopped being true without
// anyone noticing, because the suite that would have caught it built a
// different target.
func TestRustRunsOnWasmtime(t *testing.T) {
	if !engine.RunsOnWasmtime("rust") {
		t.Fatal("rust is no longer routed to wasmtime; if that is deliberate, " +
			"record why in engine.WasmtimeLanguages")
	}
}

// TestPythonRunsOnWasmtime replaces TestPythonStaysOnWazero, which pinned the
// opposite and was right about the observation while wrong about the cause.
//
// Python is a Component Model guest. Its component was failing in
// backend_wasmtime.go's hand-rolled decomposition path -- but only ever reached
// that path because the native one, which hands the component to wasmtime's own
// Component Model runtime, was compiled out by the wasmtime_component_cgo build
// tag that no build set. With the headers vendored and the tag gone, the same
// component executes and records its durable call. See engine.WasmtimeLanguages
// and IMPROVEMENT-PLAN 2.72.
//
// If this fails because someone moved Python back, the reason belongs in that
// comment -- and check first whether the native component path is still being
// compiled, because that is what this actually depends on.
func TestPythonRunsOnWasmtime(t *testing.T) {
	if !engine.RunsOnWasmtime("python") {
		t.Fatal("python is no longer routed to wasmtime; if that is deliberate, " +
			"record why in engine.WasmtimeLanguages")
	}
}

// TestGoStillRoutesToWasmtime guards the primary path. A Go workflow reaching
// wazero is the failure mode CLAUDE.md describes as "not evidence about the
// engine", and it would be easy to introduce by mistyping this list.
func TestGoStillRoutesToWasmtime(t *testing.T) {
	if !engine.RunsOnWasmtime("go") {
		t.Fatal("go workflows must run on wasmtime; it is the backend of record")
	}
}

// TestUnknownLanguageFallsBack asserts the default direction. An unrecognised
// guest must not be routed to wasmtime: backendForWasm returns nil for an
// unregistered language, and the engine now has no other backend to try --
// wasmtime is the only one cleat has (see IMPROVEMENT-PLAN.md 3.30), so an
// unrecognised language fails the workflow with a clear error instead of
// falling back to anything.
func TestUnknownLanguageFallsBack(t *testing.T) {
	// "python" was in this list and is now a supported language, so it no
	// longer tests the fallback -- it would have tested the reverse.
	for _, lang := range []string{"ruby", ""} {
		if engine.RunsOnWasmtime(lang) {
			t.Errorf("engine.RunsOnWasmtime(%q) = true; unrecognised guests must not be routed to wasmtime", lang)
		}
	}
}
