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
	if !runsOnWasmtime("rust") {
		t.Fatal("rust is no longer routed to wasmtime; if that is deliberate, " +
			"record why in engine.WasmtimeLanguages")
	}
}

// TestPythonStaysOnWazero pins the one language that genuinely fails on
// wasmtime: its component reaches the decomposition path and dies on
// `incompatible import type for env::abort`. cleat/wasmtest used to register it
// for wasmtime anyway, and nothing noticed, because plugin-harness-ci.yml
// installs no Python toolchain so the test that would exercise it skips.
func TestPythonStaysOnWazero(t *testing.T) {
	if runsOnWasmtime("python") {
		t.Fatal("python is routed to wasmtime, where its component fails to instantiate; " +
			"see IMPROVEMENT-PLAN 2.72")
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
