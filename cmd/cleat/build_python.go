package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/rcownie/cleat/internal/wasm"
)

// runBuildPython compiles a Python workflow to WASM using componentize-py
// via the python-sdk/scripts/build_wasm.py helper.
//
// The pattern argument can be:
//   - "file.py:func_name"     — explicit entry file and function name
//   - "file.py"               — a single .py file (auto-detects function)
//   - "path/to/dir/"          — a directory (looks for .py files)
func runBuildPython(pattern, outDir string) {
	pyFile := ""
	funcName := ""

	// Check if pattern contains an entry spec with "file.py:func_name" format.
	if strings.Contains(pattern, ":") && !strings.HasSuffix(pattern, ":") {
		parts := strings.SplitN(pattern, ":", 2)
		pyFile = parts[0]
		funcName = parts[1]
	} else {
		// No function name specified — determine the .py file.
		pyDir := pattern

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
	}

	// If no function name was specified, try to auto-detect it from the file.
	if funcName == "" {
		fn, err := detectEntryFunction(pyFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			fmt.Fprintf(os.Stderr, "Specify the entry function with --entry <file.py>:<func_name>\n")
			os.Exit(1)
		}
		funcName = fn
	}

	// Check for componentize-py on PATH.
	if _, err := exec.LookPath("componentize-py"); err != nil {
		fmt.Fprintf(os.Stderr, "Error: componentize-py not found.\n")
		fmt.Fprintf(os.Stderr, "Install componentize-py: pip install componentize-py\n")
		fmt.Fprintf(os.Stderr, "Or follow instructions at: https://github.com/bytecodealliance/componentize-py\n")
		os.Exit(1)
	}

	// Determine output name from the function name.
	name := strings.ReplaceAll(funcName, "-", "_")
	wasmOutput := name + ".wasm"

	entry := pyFile + ":" + funcName

	fmt.Printf("  Compiling Python to WASM via componentize-py...\n")
	fmt.Printf("  Entry: %s\n", entry)

	if err := wasm.BuildPythonWasm(entry, wasmOutput, false); err != nil {
		fmt.Fprintf(os.Stderr, "Error: componentize-py failed: %v\n", err)
		fmt.Fprintf(os.Stderr, "Make sure the Python file has a @cleat_entry decorated function.\n")
		os.Exit(1)
	}

	// Locate and copy the output .wasm file.
	srcWasm := filepath.Join(".", wasmOutput)
	if _, err := os.Stat(srcWasm); os.IsNotExist(err) {
		// The build script may have written it relative to the entry file's directory.
		entryDir := filepath.Dir(pyFile)
		srcWasm = filepath.Join(entryDir, wasmOutput)
	}

	input, err := os.ReadFile(srcWasm)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: could not read WASM output: %v\n", err)
		fmt.Fprintf(os.Stderr, "Looked in: %s\n", srcWasm)
		os.Exit(1)
	}

	dstWasm := filepath.Join(outDir, name+".wasm")
	if err := os.WriteFile(dstWasm, input, 0644); err != nil {
		fmt.Fprintf(os.Stderr, "Error: writing WASM output to %s: %v\n", dstWasm, err)
		os.Exit(1)
	}

	fi, _ := os.Stat(dstWasm)
	fmt.Printf("  Wrote %s (%s)\n", dstWasm, formatSize(fi.Size()))
}

// detectEntryFunction scans a .py file for the first @cleat_entry decorated
// function and returns its name.
func detectEntryFunction(pyFile string) (string, error) {
	data, err := os.ReadFile(pyFile)
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", pyFile, err)
	}

	lines := strings.Split(string(data), "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "@cleat_entry") {
			// Look at the next non-empty line for a function definition.
			for j := i + 1; j < len(lines); j++ {
				next := strings.TrimSpace(lines[j])
				if next == "" {
					continue
				}
				if strings.HasPrefix(next, "def ") {
					// Extract the function name: "def my_func(..."
					defPart := strings.TrimPrefix(next, "def ")
					parenIdx := strings.Index(defPart, "(")
					if parenIdx > 0 {
						return defPart[:parenIdx], nil
					}
					return "", fmt.Errorf("malformed function definition at line %d: %s", j+1, next)
				}
				// Not a function definition line; keep looking.
				break
			}
		}
	}

	return "", fmt.Errorf("no @cleat_entry decorated function found in %s", pyFile)
}
