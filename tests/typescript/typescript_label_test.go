// Package typescript holds the TypeScript guest's tests, and is the only place they may live.
//
// It is in tiers.yaml's tier2.packages and in no tier-1 pattern, so nothing here runs in the
// Tier 1 gate, where a skip is a failure and a missing toolchain is a release blocker. Every
// test that needs the TypeScript toolchain (Node, esbuild, Javy, wasm-tools) belongs here and
// is named TestTypeScript*; scripts/check-typescript-tier.py refuses a TestTypeScript* function
// in any other package, and refuses to pass if this package holds none. See cleat#3825.
package typescript

import (
	"slices"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

// TestTypeScriptLabelIsRoutedToWasmtime pins the one engine-side fact this scaffolding adds:
// a module that declares the language "javascript" is served by the wasmtime backend.
//
// It needs no toolchain and could run anywhere; it lives here because the label exists for
// this guest and the guard that keeps TypeScript tests out of tier 1 needs this package to
// hold a test that really runs. The two assertions are different questions on purpose:
// membership in the list, and RunsOnWasmtime, which the worker uses to decide whether to
// build a wazero Runtime at all. If they disagreed, a TypeScript guest would either get a
// redundant runtime or no backend.
func TestTypeScriptLabelIsRoutedToWasmtime(t *testing.T) {
	if !slices.Contains(engine.WasmtimeLanguages, "javascript") {
		t.Fatalf("engine.WasmtimeLanguages = %v does not contain %q", engine.WasmtimeLanguages, "javascript")
	}
	if !engine.RunsOnWasmtime("javascript") {
		t.Error(`engine.RunsOnWasmtime("javascript") = false, but the label is in WasmtimeLanguages`)
	}
	// The label is exact. A near-miss falls through to "no backend", which is the fail-closed
	// answer; it must not be accepted as the same language.
	for _, near := range []string{"typescript", "JavaScript", "js", "node"} {
		if engine.RunsOnWasmtime(near) {
			t.Errorf("engine.RunsOnWasmtime(%q) = true; only the exact label %q is routed", near, "javascript")
		}
	}
}
