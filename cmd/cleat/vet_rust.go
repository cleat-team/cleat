package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// forbiddenRustPatterns lists Rust APIs that are not allowed in workflow code.
var forbiddenRustPatterns = []struct {
	pattern    string
	code       string
	message    string
	suggestion string
}{
	{`use std::fs`, "R001", "filesystem access is not allowed in durable functions", "Use h.DurableCall() to interact with external storage"},
	{`std::fs::`, "R001", "filesystem access is not allowed in durable functions", "Use h.DurableCall() to interact with external storage"},
	{`use std::net`, "R002", "network access is not allowed in durable functions", "Use h.DurableCall() to communicate with external services"},
	{`std::net::`, "R002", "network access is not allowed in durable functions", "Use h.DurableCall() to communicate with external services"},
	{`use std::process`, "R003", "process spawning is not allowed in durable functions", "Use h.DurableCall() for side effects"},
	{`std::process::Command`, "R003", "process spawning is not allowed in durable functions", "Use h.DurableCall() for side effects"},
	{`use rand`, "R004", "non-deterministic random number generation is not allowed", "Use h.Random() for deterministic randomness"},
	{`rand::`, "R004", "non-deterministic random number generation is not allowed", "Use h.Random() for deterministic randomness"},
	{`std::time::SystemTime::now`, "R005", "wall-clock time is non-deterministic across replays", "Use h.Now() for deterministic time"},
	{`std::time::Instant::now`, "R005", "wall-clock time is non-deterministic across replays", "Use h.Now() for deterministic time"},
	{`use std::thread`, "R006", "threading is not allowed in durable functions", "Workflow code is single-threaded by design"},
	{`std::thread::`, "R006", "threading is not allowed in durable functions", "Workflow code is single-threaded by design"},
	{`use std::sync`, "R007", "synchronization primitives are non-deterministic across replays", "Workflow code is single-threaded by design"},
	{`std::sync::`, "R007", "synchronization primitives are non-deterministic across replays", "Workflow code is single-threaded by design"},
	{`use std::time::Duration`, "", "", ""}, // Allowed — used for h.DurableSleep()
}

// runVetRust performs static analysis on a Rust crate.
// Returns 0 on success (no errors), 1 if errors were found.
func runVetRust(crateDir string) int {
	// Validate the directory exists and has Cargo.toml.
	cargoToml := filepath.Join(crateDir, "Cargo.toml")
	if _, err := os.Stat(cargoToml); os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "Error: no Cargo.toml found in %s\n", crateDir)
		fmt.Fprintf(os.Stderr, "Rust crates require a Cargo.toml file.\n")
		os.Exit(1)
	}

	// Crate name for display.
	crateName := extractCrateName(cargoToml)
	fmt.Fprintf(os.Stderr, "Vetting Rust crate %q in %s...\n", crateName, crateDir)

	// Find all .rs files.
	var rsFiles []string
	err := filepath.WalkDir(crateDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // skip inaccessible
		}
		if d.IsDir() {
			// Skip the 'target' directory (build artifacts) and hidden dirs.
			if d.Name() == "target" || d.Name() == ".git" || strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".rs") {
			rsFiles = append(rsFiles, path)
		}
		return nil
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error scanning Rust source files: %v\n", err)
		os.Exit(1)
	}

	if len(rsFiles) == 0 {
		fmt.Fprintf(os.Stderr, "Error: no .rs source files found in %s\n", crateDir)
		os.Exit(1)
	}

	var output VetOutput
	output.Summary.Functions = len(rsFiles)

	// Scan each .rs file for forbidden patterns.
	for _, rsFile := range rsFiles {
		relPath, _ := filepath.Rel(crateDir, rsFile)
		data, err := os.ReadFile(rsFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: could not read %s: %v\n", rsFile, err)
			continue
		}

		lines := strings.Split(string(data), "\n")
		for lineIdx, line := range lines {
			lineNum := lineIdx + 1 // 1-based
			trimmed := strings.TrimSpace(line)

			for _, fb := range forbiddenRustPatterns {
				if fb.pattern == "" {
					continue
				}
				if strings.Contains(trimmed, fb.pattern) {
					col := strings.Index(trimmed, fb.pattern) + 1 // 1-based

					vr := VetResult{
						Code:       fb.code,
						File:       relPath,
						Line:       lineNum,
						Column:     col,
						Message:    fb.message,
						Suggestion: fb.suggestion,
					}

					if fb.code != "" && fb.code[0] == 'R' && fb.suggestion != "" {
						// error implied by putting in Errors slice
						output.Errors = append(output.Errors, vr)
					} else {
						// warning implied by putting in Warnings slice
						output.Warnings = append(output.Warnings, vr)
					}
				}
			}
		}
	}

	// Check for #[cleat_entry] functions.
	var hasCleatEntry bool
	for _, rsFile := range rsFiles {
		data, err := os.ReadFile(rsFile)
		if err != nil {
			continue
		}
		if strings.Contains(string(data), "#[cleat_entry]") {
			hasCleatEntry = true
			break
		}
	}
	if !hasCleatEntry {
		output.Warnings = append(output.Warnings, VetResult{
			Code:       "R100",
			Message:    "no #[cleat_entry] attribute found in any source file",
			Suggestion: "Add #[cleat_entry] to at least one function to define a workflow entry point",
		})
	}

	// Report results.
	// DurableLeaves, DurableClosure, Pure are 0 for pattern-based vets.

	// Check if JSON output is requested.
	jsonOutput := false
	for _, arg := range os.Args {
		if arg == "--json" {
			jsonOutput = true
			break
		}
	}

	if jsonOutput {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(output); err != nil {
			fmt.Fprintf(os.Stderr, "Error encoding JSON output: %v\n", err)
			os.Exit(1)
		}
	} else {
		// Human-readable output.
		for _, e := range output.Errors {
			fmt.Printf("  Error [%s] %s:%d:%d: %s\n", e.Code, e.File, e.Line, e.Column, e.Message)
			if e.Suggestion != "" {
				fmt.Printf("    suggestion: %s\n", e.Suggestion)
			}
		}
		for _, w := range output.Warnings {
			if w.File != "" {
				fmt.Printf("  Warning [%s] %s:%d:%d: %s\n", w.Code, w.File, w.Line, w.Column, w.Message)
			} else {
				fmt.Printf("  Warning [%s] %s\n", w.Code, w.Message)
			}
			if w.Suggestion != "" {
				fmt.Printf("    suggestion: %s\n", w.Suggestion)
			}
		}
		fmt.Printf("\n  Summary: %d files, %d errors, %d warnings\n",
			output.Summary.Functions, len(output.Errors), len(output.Warnings))
	}

	if len(output.Errors) > 0 {
		return 1
	}
	return 0
}
