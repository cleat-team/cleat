package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// runBuildPython compiles a Python workflow to WASM using componentize-py.
func runBuildPython(pattern, outDir string) {
	pyDir := pattern
	pyFile := ""

	// Determine if pattern is a directory or a .py file.
	if fi, err := os.Stat(pattern); err == nil {
		if fi.IsDir() {
			pyDir = pattern
		} else if strings.HasSuffix(pattern, ".py") {
			pyFile = pattern
			pyDir = filepath.Dir(pattern)
		}
	}
	if pyDir == "" {
		pyDir = "."
	}

	// If no specific .py file was given, look for workflow.py (or any .py file).
	if pyFile == "" {
		candidate := filepath.Join(pyDir, "workflow.py")
		if _, err := os.Stat(candidate); err == nil {
			pyFile = candidate
		} else {
			entries, err := os.ReadDir(pyDir)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error: could not read directory %s: %v\n", pyDir, err)
				os.Exit(1)
			}
			for _, entry := range entries {
				if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".py") {
					pyFile = filepath.Join(pyDir, entry.Name())
					break
				}
			}
			if pyFile == "" {
				fmt.Fprintf(os.Stderr, "Error: no .py file found in %s\n", pyDir)
				fmt.Fprintf(os.Stderr, "Python workflows require a .py file with workflow code.\n")
				os.Exit(1)
			}
		}
	}

	// Check for componentize-py on PATH.
	if _, err := exec.LookPath("componentize-py"); err != nil {
		fmt.Fprintf(os.Stderr, "Error: componentize-py not found.\n")
		fmt.Fprintf(os.Stderr, "Install componentize-py: pip install componentize-py\n")
		fmt.Fprintf(os.Stderr, "Or follow instructions at: https://github.com/bytecodealliance/componentize-py\n")
		os.Exit(1)
	}

	// Determine output name from the .py filename (without extension).
	name := strings.TrimSuffix(filepath.Base(pyFile), ".py")
	name = strings.ReplaceAll(name, "-", "_")
	wasmOutput := name + ".wasm"

	fmt.Printf("  Compiling Python to WASM via componentize-py...\n")
	cmd := exec.Command("componentize-py", pyFile, "-o", wasmOutput)
	cmd.Dir = pyDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: componentize-py failed: %v\n", err)
		fmt.Fprintf(os.Stderr, "Make sure the Python file has a valid componentize-py compatible entry point.\n")
		os.Exit(1)
	}

	// Locate the output .wasm file.
	srcWasm := filepath.Join(pyDir, wasmOutput)
	if _, err := os.Stat(srcWasm); os.IsNotExist(err) {
		// Try to find any .wasm file in the directory.
		entries, _ := os.ReadDir(pyDir)
		for _, entry := range entries {
			if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".wasm") {
				srcWasm = filepath.Join(pyDir, entry.Name())
				break
			}
		}
	}

	input, err := os.ReadFile(srcWasm)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: could not read WASM output: %v\n", err)
		fmt.Fprintf(os.Stderr, "Looked in: %s\n", pyDir)
		os.Exit(1)
	}

	dstWasm := filepath.Join(outDir, name+".wasm")
	if err := os.WriteFile(dstWasm, input, 0644); err != nil {
		fmt.Fprintf(os.Stderr, "Error: writing WASM output: %v\n", err)
		os.Exit(1)
	}

	fi, _ := os.Stat(dstWasm)
	fmt.Printf("  Wrote %s (%s)\n", dstWasm, formatSize(fi.Size()))
}
