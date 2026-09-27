package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/cleat-team/cleat/wasm"
)

// runBuildPython compiles a Python workflow to WASM using componentize-py
// via the python-sdk/scripts/build_wasm.py helper.
//
// The pattern argument can be:
//   - "file.py:func_name"     — explicit entry file and function name
//   - "file.py"               — a single .py file (auto-detects function)
//   - "path/to/dir/"          — a directory (looks for .py files)
//
// runtime specifies the target WASM runtime: "wasmtime", "wazero", or "" for both.
func runBuildPython(pattern, outDir, runtime, channel string) {
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
					fmt.Fprintf(os.Stderr, "Check that the directory exists and is readable.\n")
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
					fmt.Fprintf(os.Stderr, "Create a workflow.py file or specify the file path explicitly.\n")
					os.Exit(1)
				}
			}
		}
	}

	// Resolve the entry against the process working directory, which is the
	// only place a path the user typed can mean anything.
	//
	// WHY THE SCRIPT CANNOT DO THIS. build_wasm.py runs with cmd.Dir set to
	// the SDK root (wasm/build.go, `cmd.Dir = sdkRoot`), so every relative
	// resolution it makes lands in python-sdk/ rather than where the user is
	// standing -- and there are three of them, not one: validate_entry's
	// `Path(entry_file).exists()`, and the two `Path(entry_file).resolve()`
	// calls that derive componentize-py's package directory and the output
	// path. The script is RIGHT for the direct invocation its own usage line
	// documents; what is wrong is handing it a path whose meaning depends on a
	// directory cleat chose and the user cannot see.
	//
	// Both documented Python examples pass a relative --entry, so both build
	// commands in their READMEs failed from every working directory (cleat#1836).
	//
	// The --output path was made absolute for exactly this reason, and is
	// commented below; the entry never got the same treatment.
	abs, err := filepath.Abs(pyFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: could not resolve %s against the working directory: %v\n", pyFile, err)
		os.Exit(1)
	}
	pyFile = abs

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

	// Determinism gate. runBuild early-returns here before reaching analyze(),
	// so a Python workflow compiled to a deployable artifact with no
	// determinism checking at all (cleat#1770). Rust landed in #1784, Java in
	// #1791; AssemblyScript needed none, because its transform already runs
	// inside the compile.
	//
	// It runs BEFORE the componentize-py lookup for the same reason as the Rust
	// and Java gates run before their toolchain lookups: a workflow with
	// determinism errors should be refused for that, not for a missing
	// compiler it was never going to reach.
	//
	// THE THREE OUTCOMES ARE WHY THIS IS NOT `if code != 0`. Until cleat#1801
	// runVetPython returned 0 for every violating file, so this gate would have
	// been present, green and inert -- exactly the shape #1770 exists to
	// remove. It now returns 2 when it could not look, and that is a different
	// message rather than a different decision: the build is refused either
	// way, because emitting an unchecked artifact is what this gate exists to
	// prevent, but the reader is told which of the two happened.
	//
	// Refusing on UNMEASURED rather than warning is deliberate. detectEntryFunction
	// above falls back to a line scan when the SDK is missing, with the comment
	// "so the build still works without the SDK", and that is right for entry
	// detection: guessing the entry point wrong fails loudly at deploy.
	// Guessing "deterministic" wrong fails silently in production, on replay,
	// possibly much later. Same absence, opposite consequence.
	switch code := runVetPython(pyFile, false); code {
	case vetExitViolations:
		fmt.Fprintf(os.Stderr, "\nError: determinism check failed for %s -- no artifact was emitted.\n", pyFile)
		fmt.Fprintf(os.Stderr, "Fix the errors above, or run 'cleat vet --lang python %s' to see them again.\n", pyFile)
		os.Exit(1)
	case vetExitUnmeasured:
		fmt.Fprintf(os.Stderr, "\nError: the determinism check could not run for %s -- no artifact was emitted.\n", pyFile)
		fmt.Fprintf(os.Stderr, "This is a failure of the CHECK, not a finding about your workflow: the file may be fine.\n")
		fmt.Fprintf(os.Stderr, "See the cause above. Refusing rather than building unchecked, because a workflow that\n")
		fmt.Fprintf(os.Stderr, "is not deterministic replays incorrectly and says nothing at the time.\n")
		os.Exit(1)
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

	// Build into a temporary directory, via an ABSOLUTE path.
	//
	// This was the bare relative name `name + ".wasm"`, and build_wasm.py
	// resolves a relative --output against the *entry file's* directory --
	// not the process CWD, and not -o (python-sdk/scripts/build_wasm.py,
	// "Resolve output path to absolute"). So every Python build dropped two
	// artifacts beside the user's source, the component and the
	// `<name>.wasm.component.wasm` backup it copies alongside, and -o only
	// ever received a copy of the first one. The lookup below used to carry a
	// fallback that searched the entry directory when "." came up empty,
	// which is the shape of a symptom worked around rather than fixed.
	//
	// It is not only untidy. In this repo the entry directory is tracked, so
	// TestPluginCalls_Wasm_Python rewrote two committed fixtures on every
	// run, and componentize-py's output is not reproducible -- five
	// consecutive builds of an unchanged source gave five distinct SHA-256s
	// and sizes from 20398088 to 20482296 bytes -- so the rewrite could never
	// settle. See IMPROVEMENT-PLAN 3.308 for why those particular bytes were
	// worth protecting rather than regenerating.
	buildDir, err := os.MkdirTemp("", "cleat-build-python-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: could not create a temporary build directory: %v\n", err)
		os.Exit(1)
	}
	defer os.RemoveAll(buildDir)
	wasmOutput := filepath.Join(buildDir, name+".wasm")

	entry := pyFile + ":" + funcName

	fmt.Printf("  Compiling Python to WASM via componentize-py...\n")
	fmt.Printf("  Entry: %s\n", entry)

	if channel != "" {
		os.Setenv("CLEAT_CHILD_BINDING_POLICY", channel)
		defer os.Unsetenv("CLEAT_CHILD_BINDING_POLICY")
	}

	if err := wasm.BuildPythonWasmWithRuntime(entry, wasmOutput, runtime, false); err != nil {
		fmt.Fprintf(os.Stderr, "Error: componentize-py failed: %v\n", err)
		fmt.Fprintf(os.Stderr, "Make sure the Python file has a @cleat_entry decorated function.\n")
		fmt.Fprintf(os.Stderr, "Check that (1) the Python file has valid syntax, (2) the @cleat_entry function exists and is not async, (3) all imports are available.\n")
		os.Exit(1)
	}

	// Copy the output .wasm to the requested directory. wasmOutput is
	// absolute, so the build script wrote exactly there and there is nowhere
	// else to look -- the two-place search this replaced existed only because
	// a relative output path could land in either.
	srcWasm := wasmOutput

	input, err := os.ReadFile(srcWasm)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: could not read WASM output: %v\n", err)
		fmt.Fprintf(os.Stderr, "Looked in: %s\n", srcWasm)
		fmt.Fprintf(os.Stderr, "Check file permissions and disk space.\n")
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

// detectEntryFunction uses the proper Python AST-based detector from
// cleat_sdk.vet (“--detect-entry“ flag) to find the “@cleat_entry“
// decorated function in a .py file.
//
// It shells out to “python3 -m cleat_sdk.vet --detect-entry <file>“,
// which handles:
//   - AST-based parsing (not fragile string scanning)
//   - Commented-out decorator filtering
//   - Multi-line decorator arguments
//   - “async def“ detection and rejection
//   - Multiple entry function error reporting
func detectEntryFunction(pyFile string) (string, error) {
	// Find the python-sdk directory so we can set PYTHONPATH.
	// The SDK lives at <binary>/../python-sdk/ or CWD/python-sdk/.
	sdkDir := findPythonSDKDir()

	cmd := exec.Command("python3", "-m", "cleat_sdk.vet", "--detect-entry", pyFile)
	var stderrBuf strings.Builder
	cmd.Stderr = &stderrBuf

	if sdkDir != "" {
		cmd.Env = append(os.Environ(), "PYTHONPATH="+sdkDir)
	}

	out, err := cmd.Output()
	if err != nil {
		stderr := strings.TrimSpace(stderrBuf.String())
		// If the Python vet module is not installed, fall back to simple
		// string scanning so the build still works without the SDK.
		if strings.Contains(stderr, "No module named") || strings.Contains(stderr, "ModuleNotFoundError") || errors.Is(err, exec.ErrNotFound) {
			return detectEntryFunctionFallback(pyFile)
		}
		if stderr != "" {
			return "", fmt.Errorf("in %s: %s", pyFile, stderr)
		}
		return "", fmt.Errorf("Python file analysis failed for %s: %v. Check that the file is valid Python.", pyFile, err)
	}

	name := strings.TrimSpace(string(out))
	if name == "" {
		return "", fmt.Errorf("no @cleat_entry decorated function found in %s", pyFile)
	}

	return name, nil
}

// detectEntryFunctionFallback scans a .py file for a @cleat_entry decorated
// function using simple string scanning. Used when the Python SDK vet module
// is not available.
func detectEntryFunctionFallback(pyFile string) (string, error) {
	data, err := os.ReadFile(pyFile)
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", pyFile, err)
	}

	lines := strings.Split(string(data), "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		// Skip commented-out decorators.
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.HasPrefix(trimmed, "@cleat_entry") {
			for j := i + 1; j < len(lines); j++ {
				next := strings.TrimSpace(lines[j])
				if next == "" || strings.HasPrefix(next, "#") {
					continue
				}
				// Skip continuation lines (decorator args spanning multiple lines).
				if strings.HasPrefix(next, "(") || strings.HasPrefix(next, "\"") || strings.HasPrefix(next, "'") {
					continue
				}
				if strings.HasPrefix(next, "def ") {
					defPart := strings.TrimPrefix(next, "def ")
					parenIdx := strings.Index(defPart, "(")
					if parenIdx > 0 {
						return defPart[:parenIdx], nil
					}
					return "", fmt.Errorf("malformed function definition at line %d: %s", j+1, next)
				}
				if strings.HasPrefix(next, "async def ") {
					defPart := strings.TrimPrefix(next, "async def ")
					parenIdx := strings.Index(defPart, "(")
					name := defPart
					if parenIdx > 0 {
						name = defPart[:parenIdx]
					}
					return "", fmt.Errorf("'%s' in %s is an async function (line %d). Async functions cannot be compiled to WASM. Use cleat's synchronous durable execution model instead.", name, pyFile, j+1)
				}
				// Not a function definition — keep looking past decorator args.
				if !strings.HasPrefix(next, "@") {
					break
				}
			}
		}
	}

	return "", fmt.Errorf("no @cleat_entry decorated function found in %s", pyFile)
}

// findPythonSDKDir locates the python-sdk directory for invoking Python
// analysis tools.  Returns empty string if not found (caller will rely
// on PYTHONPATH or system-installed cleat_sdk package).
func findPythonSDKDir() string {
	// Try relative to the running binary.
	execPath, err := os.Executable()
	if err == nil {
		execDir := filepath.Dir(execPath)
		// Check <bindir>/python-sdk (same-level) and <bindir>/../python-sdk (parent).
		candidates := []string{
			filepath.Join(execDir, "python-sdk"),
			filepath.Join(execDir, "..", "python-sdk"),
		}
		for _, c := range candidates {
			if info, statErr := os.Stat(c); statErr == nil && info.IsDir() {
				return c
			}
		}
	}
	// Fall back to CWD/python-sdk.
	if cwd, err := os.Getwd(); err == nil {
		candidate := filepath.Join(cwd, "python-sdk")
		if info, statErr := os.Stat(candidate); statErr == nil && info.IsDir() {
			return candidate
		}
	}
	return ""
}
