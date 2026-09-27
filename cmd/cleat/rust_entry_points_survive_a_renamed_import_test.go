// cleat#2113's own regression test: proves the cleat_entry_points WASM
// section catches an entry point the OLD source-level regex
// (build_entry_points.go's now-deleted rustEntryPointNames) would have
// missed.
//
// That extractor matched the literal text `#[cleat_entry]` immediately
// before `fn`:
//
//	`#\[cleat_entry\][\s]*(?:#\[[^\]]*\][\s]*)*(?:pub(?:\([^)]*\))?\s+)?(?:unsafe\s+)?fn\s+(\w+)`
//
// A function marked through a renamed import -- `use cleat_macro::cleat_entry
// as entry_point;` then `#[entry_point] fn ...` -- is functionally identical
// (the macro still expands, the export still exists, the argument to
// `#[proc_macro_attribute] pub fn cleat_entry` is unaffected by what name it
// is invoked under) but does not contain the substring "cleat_entry" at the
// attribute site, so the regex would never see it. This is exactly the
// "annotation imported under a renamed alias" gap
// build_entry_points.go's file-level comment names as something the old
// approach could miss.
//
// This crate cannot exercise that historical miss directly -- the extractor
// it would have missed against is deleted, per the owner's decision that the
// artifact is authoritative and the regex is not kept as a cross-check. What
// this proves instead is the thing that decision rests on: the artifact-based
// mechanism gets the renamed-import entry right where a source-level regex
// provably could not, so nothing is lost by removing the fallback.
package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/wasm"
)

func TestRustEntryPointsSurviveARenamedMacroImport(t *testing.T) {
	if testing.Short() || cleatBinary == "" {
		t.Skip("needs the cleat binary, which TestMain does not build in short mode")
	}
	if _, err := exec.LookPath("cargo"); err != nil {
		t.Skip("cargo not available")
	}

	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	sdkDir := filepath.Join(repoRoot, "crates", "cleat-sdk")
	macroDir := filepath.Join(repoRoot, "crates", "cleat-macro")
	if _, statErr := os.Stat(filepath.Join(sdkDir, "Cargo.toml")); statErr != nil {
		t.Fatalf("computed cleat-sdk dir %s has no Cargo.toml: %v", sdkDir, statErr)
	}

	cargoDir := t.TempDir()
	cargoToml := `[package]
name = "renamed_entry_fixture"
version = "0.1.0"
edition = "2021"

[lib]
crate-type = ["cdylib"]

[dependencies]
cleat-sdk = { path = "` + filepath.ToSlash(sdkDir) + `" }
cleat-macro = { path = "` + filepath.ToSlash(macroDir) + `" }
serde_json = "1"

[profile.release]
opt-level = "s"
`
	if writeErr := os.WriteFile(filepath.Join(cargoDir, "Cargo.toml"), []byte(cargoToml), 0644); writeErr != nil {
		t.Fatalf("write Cargo.toml: %v", writeErr)
	}
	if mkErr := os.Mkdir(filepath.Join(cargoDir, "src"), 0755); mkErr != nil {
		t.Fatalf("mkdir src: %v", mkErr)
	}
	// ordinary_entry is reachable by the old regex; renamed_entry is marked
	// through an aliased import and is the case the old regex missed.
	libRs := `use cleat_sdk::HostCalls;
use cleat_macro::cleat_entry;
use cleat_macro::cleat_entry as entry_point;

#[cleat_entry]
fn ordinary_entry(h: &HostCalls, input: String) -> Result<String, String> {
    let _ = h;
    Ok(input)
}

#[entry_point]
fn renamed_entry(h: &HostCalls, input: String) -> Result<String, String> {
    let _ = h;
    Ok(input)
}
`
	if writeErr := os.WriteFile(filepath.Join(cargoDir, "src", "lib.rs"), []byte(libRs), 0644); writeErr != nil {
		t.Fatalf("write src/lib.rs: %v", writeErr)
	}

	// Confirmed once, directly, rather than by matching a substring against
	// the build's combined output: "Compiling Rust WASM module
	// (wasm32-unknown-unknown)..." is printed by runBuildRust as an ordinary
	// progress line on every build, successful or not, so a
	// strings.Contains(out, "wasm32-unknown-unknown") check (as used in
	// rust_entry_point_resolution_live_test.go) would misclassify ANY later
	// build failure -- including the very regression this test exists to
	// catch -- as "target not installed" and skip past it silently.
	if listOut, listErr := exec.Command("rustup", "target", "list", "--installed").CombinedOutput(); listErr != nil ||
		!strings.Contains(string(listOut), "wasm32-unknown-unknown") {
		t.Skip("wasm32-unknown-unknown target not installed")
	}

	outDir := t.TempDir()
	buildCmd := exec.Command(cleatBinary, "build", "--target", "rust", "-o", outDir, cargoDir)
	buildCmd.Dir = repoRoot
	if out, buildErr := buildCmd.CombinedOutput(); buildErr != nil {
		t.Fatalf("cleat build --target rust: %v\n%s", buildErr, out)
	}

	wasmPath := filepath.Join(outDir, "renamed_entry_fixture.wasm")
	wasmBytes, readErr := os.ReadFile(wasmPath)
	if readErr != nil {
		t.Fatalf("build did not produce %s: %v", wasmPath, readErr)
	}

	entryPoints, epErr := wasm.ReadEntryPointsSection(wasmBytes)
	if epErr != nil {
		t.Fatalf("reading cleat_entry_points from the build's own output: %v", epErr)
	}

	want := map[string]bool{"ordinary_entry": true, "renamed_entry": true}
	got := make(map[string]bool, len(entryPoints))
	for _, name := range entryPoints {
		got[name] = true
	}
	for name := range want {
		if !got[name] {
			t.Errorf("cleat_entry_points is missing %q -- entry points: %v", name, entryPoints)
		}
	}
	if !got["renamed_entry"] {
		t.Fatalf("renamed_entry (marked via a renamed macro import) is absent -- this is exactly the case "+
			"the old source-level regex would have missed; entry points found: %v", entryPoints)
	}
	if len(entryPoints) != 2 {
		t.Errorf("expected exactly 2 entry points, got %d: %v", len(entryPoints), entryPoints)
	}
}
