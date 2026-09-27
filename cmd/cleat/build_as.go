package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/cleat-team/cleat/wasm"
)

// runBuildAssemblyScript compiles an AssemblyScript project to WASM using asc.
func runBuildAssemblyScript(pattern, outDir, channel string, workflowVersion int) {
	asDir := pattern
	if asDir == "" {
		asDir = "."
	}

	// Validate package.json exists.
	pkgJSONPath := filepath.Join(asDir, "package.json")
	if _, err := os.Stat(pkgJSONPath); os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "Error: no package.json found in %s\n", asDir)
		fmt.Fprintf(os.Stderr, "AssemblyScript workflows require a package.json with assemblyscript as a devDependency.\n")
		fmt.Fprintf(os.Stderr, "Run 'npm init' in %s or copy from the cleat AS example template.\n", asDir)
		os.Exit(1)
	}

	// Validate asconfig.json exists.
	asconfigPath := filepath.Join(asDir, "asconfig.json")
	if _, err := os.Stat(asconfigPath); os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "Error: no asconfig.json found in %s\n", asDir)
		fmt.Fprintf(os.Stderr, "AssemblyScript workflows require an asconfig.json configuration file.\n")
		fmt.Fprintf(os.Stderr, "Create an asconfig.json with 'assemblyscript' settings and a transform pointing to @cleat/transform.\n")
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
			fmt.Fprintf(os.Stderr, "Check your network connection and that package.json has no syntax errors.\n")
			os.Exit(1)
		}
	}

	// Delete any manifest left over from a PRIOR build, before invoking asc.
	// Without this, a build whose transform fails to write a fresh manifest
	// (the transform throws, but @cleat/transform is old, or something else
	// swallows the error) would fall through to the stat/read below and find
	// the STALE one from last time -- silently embedding entry points that do
	// not match this compile. That is exactly the "wrong entry point,
	// silently" failure cleat#2113 exists to kill, reopened through the one
	// path #2113's own check (verifyEntryPointsAreExports) cannot see: a stale
	// manifest can easily still name real exports, just the wrong build's.
	manifestPath := filepath.Join(asDir, "dist", "cleat-entry-points.txt")
	if rmErr := os.Remove(manifestPath); rmErr != nil && !os.IsNotExist(rmErr) {
		fmt.Fprintf(os.Stderr, "Error: assemblyscript build: could not remove stale manifest %s: %v\n", manifestPath, rmErr)
		os.Exit(1)
	}

	// Compile AssemblyScript to WASM.
	fmt.Printf("  Compiling AssemblyScript to WASM...\n")
	args := []string{
		"asc",
		"assembly/index.ts",
		"--runtime", "stub",
		"--transform", "@cleat/transform",
		"--optimize",
		"--initialMemory", "170",
		"-o", "dist/workflow.wasm",
	}

	cmd := exec.Command("npx", args...)
	cmd.Dir = asDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()
	if channel != "" {
		cmd.Env = append(cmd.Env, "CLEAT_CHILD_BINDING_POLICY="+channel)
	}
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: AssemblyScript compilation failed: %v\n", err)
		fmt.Fprintf(os.Stderr, "Make sure assembly/index.ts exists and has no syntax errors.\n")
		fmt.Fprintf(os.Stderr, "Check that (1) assembly/index.ts exists, (2) code has no syntax errors, (3) all @cleatEntry functions have valid signatures.\n")
		os.Exit(1)
	}

	// Locate the output .wasm file.
	wasmPath := filepath.Join(asDir, "dist", "workflow.wasm")
	if _, err := os.Stat(wasmPath); os.IsNotExist(err) {
		// Try to find any .wasm file in dist/.
		matches, _ := filepath.Glob(filepath.Join(asDir, "dist", "*.wasm"))
		if len(matches) == 0 {
			fmt.Fprintf(os.Stderr, "Error: no .wasm file found in %s/dist/\n", asDir)
			fmt.Fprintf(os.Stderr, "Compilation may have failed silently. Run 'npx asc assembly/index.ts --runtime stub' manually to see detailed errors.\n")
			os.Exit(1)
		}
		wasmPath = matches[0]
	}

	input, err := os.ReadFile(wasmPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: could not read WASM output: %v\n", err)
		fmt.Fprintf(os.Stderr, "Looked in: %s\n", wasmPath)
		fmt.Fprintf(os.Stderr, "Check file permissions and disk space.\n")
		os.Exit(1)
	}

	// Use directory name as workflow name. Derived BEFORE the metadata is
	// written, because the metadata needs it -- see nonGoMetadata.
	absDir, _ := filepath.Abs(asDir)
	name := filepath.Base(absDir)

	// entryPoints comes from the sidecar manifest @cleat/transform itself
	// wrote during compilation (cleat#2145) -- the same AST walk that
	// decided which functions to rename and export (packages/cleat-as/transform/index.js),
	// not a separate source-level guess. A missing manifest means an
	// @cleat/transform old enough to predate this mechanism; refused rather
	// than falling back to the source-level regex this replaces, which
	// would reopen the silent-miss risk cleat#2113 exists to close. The
	// staleness case -- a leftover manifest from a build whose write failed
	// -- was ruled out above by deleting manifestPath before invoking asc, so
	// a manifest found here is either absent (predates cleat#2145) or fresh.
	manifest, manifestErr := os.ReadFile(manifestPath)
	if manifestErr != nil {
		fmt.Fprintf(os.Stderr, "Error: assemblyscript build: no entry-point manifest at %s. "+
			"This project's @cleat/transform is old enough to predate cleat#2145 and does not emit one. "+
			"Upgrade @cleat/transform (and @cleat/sdk), then rebuild.\n", manifestPath)
		os.Exit(1)
	}
	var manifestNames []string
	for _, line := range strings.Split(string(manifest), "\n") {
		if line != "" {
			manifestNames = append(manifestNames, line)
		}
	}
	enriched, sectionErr := wasm.WriteEntryPointsSection(input, manifestNames)
	if sectionErr != nil {
		fmt.Fprintf(os.Stderr, "Error: assemblyscript build: embedding entry points: %v\n", sectionErr)
		os.Exit(1)
	}
	input = enriched
	// Re-read what was just written -- this is what gives duplicate-name
	// detection for free (see WriteEntryPointsSection's doc comment) and
	// confirms the round-trip, the same way build_rust.go's ReadEntryPointsSection
	// call does for a linker-assembled section.
	entryPoints, epErr := wasm.ReadEntryPointsSection(input)
	if epErr != nil {
		fmt.Fprintf(os.Stderr, "Error: assemblyscript build: %v\n", epErr)
		os.Exit(1)
	}
	if verifyErr := verifyEntryPointsAreExports("assemblyscript", input, entryPoints); verifyErr != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", verifyErr)
		os.Exit(1)
	}

	// Inject cleat.metadata. Must pass wasm.Metadata.Validate(), which
	// `cleat deploy` runs and exits 1 on; see cleat#1077.
	if enriched, metaErr := wasm.WriteMetadata(input,
		nonGoMetadata("assemblyscript", name, workflowVersion, entryPoints)); metaErr == nil {
		input = enriched
	}
	dstWasm := filepath.Join(outDir, name+".wasm")
	if err := os.WriteFile(dstWasm, input, 0644); err != nil {
		fmt.Fprintf(os.Stderr, "Error: writing WASM output to %s: %v\n", dstWasm, err)
		os.Exit(1)
	}

	fi, _ := os.Stat(dstWasm)
	fmt.Printf("  Wrote %s (%s)\n", dstWasm, formatSize(fi.Size()))
}
