package main

import (
	"os"
	"testing"

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
			if got := runsOnWasmtime(lang); got != tc.wantWasmtime {
				t.Errorf("runsOnWasmtime(%q) = %v, want %v", lang, got, tc.wantWasmtime)
			}
		})
	}
}

// TestRustIsNotYetOnWasmtime pins a deliberate exclusion so that moving Rust is
// an edit to wasmtimeLanguages rather than a side effect of something else.
//
// Rust does load and execute on wasmtime -- verified against both artifact
// shapes, wasm32-wasip1 and the wasm32-unknown-unknown cdylib that
// `cleat build --target rust` actually ships. It stays on wazero because
// tests/cross-language builds Rust for wasm32-wasip1, not the shipped target,
// so the suite that would gate the change does not currently cover the artifact
// users run. See IMPROVEMENT-PLAN 2.72.
func TestRustIsNotYetOnWasmtime(t *testing.T) {
	if runsOnWasmtime("rust") {
		t.Fatal("rust is routed to wasmtime; if that is intended, update this test " +
			"and confirm tests/cross-language builds the shipped wasm32-unknown-unknown target")
	}
}

// TestGoStillRoutesToWasmtime guards the primary path. A Go workflow reaching
// wazero is the failure mode CLAUDE.md describes as "not evidence about the
// engine", and it would be easy to introduce by mistyping this list.
func TestGoStillRoutesToWasmtime(t *testing.T) {
	if !runsOnWasmtime("go") {
		t.Fatal("go workflows must run on wasmtime; it is the backend of record")
	}
}

// TestUnknownLanguageFallsBack asserts the default direction. An unrecognised
// guest must reach wazero rather than nothing: backendForWasm returns nil for
// an unregistered language, and the worker relies on that to build a wazero
// Runtime instead.
func TestUnknownLanguageFallsBack(t *testing.T) {
	for _, lang := range []string{"python", "ruby", ""} {
		if runsOnWasmtime(lang) {
			t.Errorf("runsOnWasmtime(%q) = true; unrecognised guests must fall back to wazero", lang)
		}
	}
}
