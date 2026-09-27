package engine

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestRustAllHostCallsCompiles builds a Rust workflow that calls every method
// on cleat_sdk::HostCalls.
//
// examples/rust-workflow and the plugin-harness fixture between them called 7
// of 61. Passing them meant "a Rust workflow builds", not "the Rust host-call
// surface builds" -- and the Go SDK had exactly that hole, with four host calls
// shipping uncompilable through it (IMPROVEMENT-PLAN.md 3.204). 3.206 measures
// the gap across all five SDKs; this closes the Rust row.
//
// The fixture's breadth is not asserted here. It is measured by
// scripts/sdk-host-call-coverage.py, which runs in Lint with no toolchain and
// fails when coverage falls -- so a call deleted from the fixture is caught
// even in a job that cannot build Rust at all. This test is where the coverage
// is cashed in.
//
// Prerequisite handling is buildRustWasm's, deliberately: cargo and the
// wasm32-wasip1 target are genuinely optional (only e2e-cross-language.yml
// provisions them), but a job declaring "rust" in CLEAT_REQUIRE_TOOLCHAINS has
// installed them on purpose, so a missing toolchain there is a failure.
func TestRustAllHostCallsCompiles(t *testing.T) {
	cargo, err := exec.LookPath("cargo")
	if err != nil {
		if toolchainRequired("rust") {
			t.Fatalf("cargo not installed, but %s declares rust: %v", requireToolchainEnv, err)
		}
		t.Skip("cargo not installed — only e2e-cross-language.yml provisions Rust")
	}
	if out, err := exec.Command("rustup", "target", "list", "--installed").Output(); err != nil ||
		!strings.Contains(string(out), "wasm32-wasip1") {
		if toolchainRequired("rust") {
			t.Fatalf("wasm32-wasip1 target not installed, but %s declares rust", requireToolchainEnv)
		}
		t.Skip("wasm32-wasip1 Rust target not installed")
	}

	crateDir := filepath.Join(findProjectRoot(t), "examples", "rust-all-host-calls")

	cmd := exec.Command(cargo, "build", "--target", "wasm32-wasip1", "--release")
	cmd.Dir = crateDir
	cmd.Env = append(os.Environ(), "HOME="+os.Getenv("HOME"), "PATH="+os.Getenv("PATH"))

	// The build IS the assertion: every call in the fixture has to typecheck
	// against the SDK and link against the host imports. There is nothing to
	// check afterwards beyond the artefact existing.
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the Rust host-call surface does not compile:\n%s\n%v", string(out), err)
	}

	wasm := filepath.Join(crateDir, "target", "wasm32-wasip1", "release", "rust_all_host_calls.wasm")
	if _, err := os.Stat(wasm); err != nil {
		t.Fatalf("cargo reported success but produced no module at %s: %v", wasm, err)
	}
}
