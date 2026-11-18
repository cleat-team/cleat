// Package csharp holds the Go-side tests of the C# guest (engine and end-to-end), and is the only place they may live.
//
// It is in tiers.yaml's tier2.packages and in no tier-1 pattern, so nothing here runs in the Tier 1 gate, where a skip is a
// failure and a missing toolchain is a release blocker. Every Go test that needs the C# toolchain (the .NET SDK container,
// componentize-dotnet, wasi-sdk) belongs here and is named TestCSharp*; scripts/check-csharp-tier.py refuses a TestCSharp*
// function in any other package, and refuses to pass if this package holds none. The SDK itself and its managed unit tests live
// in packages/cleat-csharp/ and run in the pinned SDK container, not under `go test`. See cleat#4080.
package csharp

import (
	"slices"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

// TestCSharpLabelIsRoutedToWasmtimeButNotADeferSegmentLanguage pins the engine-side fact C# PR 1 adds: a module that declares
// the language "csharp" is served by the wasmtime backend.
//
// It needs no toolchain and could run anywhere; it lives here because the label exists for this guest and the guard that keeps C#
// tests out of tier 1 needs this package to hold a test that really runs. The first two assertions are different questions on
// purpose: membership in the list, and RunsOnWasmtime, which the worker uses to decide whether to build a wazero Runtime at all.
func TestCSharpLabelIsRoutedToWasmtime(t *testing.T) {
	if !slices.Contains(engine.WasmtimeLanguages, "csharp") {
		t.Fatalf("engine.WasmtimeLanguages = %v does not contain %q", engine.WasmtimeLanguages, "csharp")
	}
	if !engine.RunsOnWasmtime("csharp") {
		t.Error(`engine.RunsOnWasmtime("csharp") = false, but the label is in WasmtimeLanguages`)
	}
	// The label is exact. A near-miss falls through to "no backend", which is the fail-closed answer; it must not be accepted as
	// the same language.
	for _, near := range []string{"c#", "CSharp", "Csharp", "dotnet", ".net", "c-sharp"} {
		if engine.RunsOnWasmtime(near) {
			t.Errorf("engine.RunsOnWasmtime(%q) = true; only the exact label %q is routed", near, "csharp")
		}
	}
}
