package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// runBuildAssemblyScript compiles an AssemblyScript project to WASM using asc.
func runBuildAssemblyScript(pattern, outDir string) {
	asDir := pattern
	if asDir == "" {
		asDir = "."
	}

	// Validate package.json exists.
	pkgJSONPath := filepath.Join(asDir, "package.json")
	if _, err := os.Stat(pkgJSONPath); os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "Error: no package.json found in %s\n", asDir)
		fmt.Fprintf(os.Stderr, "AssemblyScript workflows require a package.json with assemblyscript as a devDependency.\n")
		os.Exit(1)
	}

	// Validate asconfig.json exists.
	asconfigPath := filepath.Join(asDir, "asconfig.json")
	if _, err := os.Stat(asconfigPath); os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "Error: no asconfig.json found in %s\n", asDir)
		fmt.Fprintf(os.Stderr, "AssemblyScript workflows require an asconfig.json configuration file.\n")
		os.Exit(1)
	}

	// Check for npx on PATH.
	if _, err := exec.LookPath("npx"); err != nil {
		fmt.Fprintf(os.Stderr, "Error: npx not found. Install Node.js: https://nodejs.org\n")
		os.Exit(1)
	}

	// Run npm install if node_modules doesn't exist.
	nodeModulesPath := filepath.Join(asDir, "node_modules")
	if _, err := os.Stat(nodeModulesPath); os.IsNotExist(err) {
		fmt.Printf("  Installing npm dependencies...\n")
		cmd := exec.Command("npm", "install")
		cmd.Dir = asDir
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "Error: npm install failed: %v\n", err)
			os.Exit(1)
		}
	}

	// Compile AssemblyScript to WASM.
	fmt.Printf("  Compiling AssemblyScript to WASM...\n")
	args := []string{
		"asc",
		"assembly/index.ts",
		"--runtime", "stub",
		"--optimize",
		"--initialMemory", "170",
		"-o", "dist/workflow.wasm",
	}

	cmd := exec.Command("npx", args...)
	cmd.Dir = asDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: AssemblyScript compilation failed: %v\n", err)
		fmt.Fprintf(os.Stderr, "Make sure assembly/index.ts exists and has no syntax errors.\n")
		os.Exit(1)
	}

	// Locate the output .wasm file.
	wasmPath := filepath.Join(asDir, "dist", "workflow.wasm")
	if _, err := os.Stat(wasmPath); os.IsNotExist(err) {
		// Try to find any .wasm file in dist/.
		matches, _ := filepath.Glob(filepath.Join(asDir, "dist", "*.wasm"))
		if len(matches) == 0 {
			fmt.Fprintf(os.Stderr, "Error: no .wasm file found in %s/dist/\n", asDir)
			os.Exit(1)
		}
		wasmPath = matches[0]
	}

	input, err := os.ReadFile(wasmPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: could not read WASM output: %v\n", err)
		fmt.Fprintf(os.Stderr, "Looked in: %s\n", wasmPath)
		os.Exit(1)
	}

	// Use directory name as workflow name.
	absDir, _ := filepath.Abs(asDir)
	name := filepath.Base(absDir)
	dstWasm := filepath.Join(outDir, name+".wasm")
	if err := os.WriteFile(dstWasm, input, 0644); err != nil {
		fmt.Fprintf(os.Stderr, "Error: writing WASM output: %v\n", err)
		os.Exit(1)
	}

	fi, _ := os.Stat(dstWasm)
	fmt.Printf("  Wrote %s (%s)\n", dstWasm, formatSize(fi.Size()))
}
