package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/cleat-team/cleat/wasm"
)

// runBuildRust compiles a Rust workflow crate to WASM using cargo.
// Uses wasm32-unknown-unknown (no WASI) to avoid non-deterministic
// WASI imports (environ_get etc.) that break replay determinism.
func runBuildRust(pattern, outDir string) {
	cargoDir := pattern

	// Validate Cargo.toml exists.
	cargoToml := filepath.Join(cargoDir, "Cargo.toml")
	if _, err := os.Stat(cargoToml); os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "Error: no Cargo.toml found in %s\n", cargoDir)
		fmt.Fprintf(os.Stderr, "Rust workflows require a Cargo.toml with cleat-sdk and cleat-macro dependencies.\n")
		os.Exit(1)
	}

	// Check for cargo.
	if _, err := exec.LookPath("cargo"); err != nil {
		fmt.Fprintf(os.Stderr, "Error: cargo not found. Install Rust: https://rustup.rs\n")
		os.Exit(1)
	}

	fmt.Printf("  Compiling Rust WASM module (wasm32-unknown-unknown)...\n")
	cmd := exec.Command("cargo", "build", "--target", "wasm32-unknown-unknown", "--release")
	cmd.Dir = cargoDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()

	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: cargo build failed: %v\n", err)
		fmt.Fprintf(os.Stderr, "Make sure the wasm32-unknown-unknown target is installed:\n")
		fmt.Fprintf(os.Stderr, "  rustup target add wasm32-unknown-unknown\n")
		os.Exit(1)
	}

	// Locate the output .wasm file.
	crateName := extractCrateName(cargoToml)
	wasmBuildDir := filepath.Join(cargoDir, "target", "wasm32-unknown-unknown", "release")
	srcWasm := filepath.Join(wasmBuildDir, crateName+".wasm")

	if _, err := os.Stat(srcWasm); os.IsNotExist(err) {
		// Try to find any .wasm file in the build directory.
		entries, _ := os.ReadDir(wasmBuildDir)
		for _, entry := range entries {
			if strings.HasSuffix(entry.Name(), ".wasm") {
				srcWasm = filepath.Join(wasmBuildDir, entry.Name())
				break
			}
		}
	}

	input, err := os.ReadFile(srcWasm)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: could not read WASM output: %v\n", err)
		fmt.Fprintf(os.Stderr, "Looked in: %s\n", wasmBuildDir)
		os.Exit(1)
	}

	// Inject cleat.metadata so the engine can detect the source language.
	if enriched, metaErr := wasm.WriteMetadata(input, &wasm.Metadata{Language: "rust"}); metaErr == nil {
		input = enriched
	}

	dstWasm := filepath.Join(outDir, crateName+".wasm")
	if err := os.WriteFile(dstWasm, input, 0644); err != nil {
		fmt.Fprintf(os.Stderr, "Error: writing WASM output: %v\n", err)
		os.Exit(1)
	}

	fi, _ := os.Stat(dstWasm)
	fmt.Printf("  Wrote %s (%s)\n", dstWasm, formatSize(fi.Size()))
}

// extractCrateName parses [package] name = "..." from a Cargo.toml.
func extractCrateName(cargoTomlPath string) string {
	data, err := os.ReadFile(cargoTomlPath)
	if err != nil {
		return "rust_workflow"
	}
	inPackage := false
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "[package]" {
			inPackage = true
			continue
		}
		if inPackage && strings.HasPrefix(line, "[") {
			break
		}
		if inPackage && strings.HasPrefix(line, "name") {
			parts := strings.SplitN(line, "=", 2)
			if len(parts) == 2 {
				name := strings.TrimSpace(parts[1])
				name = strings.Trim(name, "\"'")
				return strings.ReplaceAll(name, "-", "_")
			}
		}
	}
	return "rust_workflow"
}
