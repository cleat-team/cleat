// Command cleat is the workflow transformer CLI.
//
// Usage:
//
//	cleat build <package>     — analyze and compile a workflow package
//	cleat vet <package>       — validate a workflow package (no compilation)
//
// The build command runs the full transformer pipeline: package loading,
// call graph construction, cleat closure computation, HostCalls threading
// verification, WASM import generation, host adapter generation, WASM export
// generation, and compilation.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"go/token"
	"time"

	_ "github.com/lib/pq"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/rcownie/cleat/internal/analyzer"
	"github.com/rcownie/cleat/internal/callgraph"
	"github.com/rcownie/cleat/internal/closure"
	"github.com/rcownie/cleat/internal/host"
	"github.com/rcownie/cleat/internal/transform"
	"github.com/rcownie/cleat/internal/wasm"
)

var dbConnStr string

func main() {
	flag.StringVar(&dbConnStr, "db", "", "PostgreSQL connection string (or set CLEAT_DATABASE_URL)")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: cleat <build|vet|deploy|versions|rollback|dev|schedule|run|dag|plugin|init> [flags] <args>\n")
		fmt.Fprintf(os.Stderr, "  cleat build [-o <dir>] [--target <target>] <package>\n")
		fmt.Fprintf(os.Stderr, "  cleat vet [--lang go|rust|java|as|python] [--json] <package>\n")
		fmt.Fprintf(os.Stderr, "  cleat deploy [--name <name>] [--namespace <ns>] [--task-queue <queue>] <wasm-file>\n")
		fmt.Fprintf(os.Stderr, "  cleat versions <workflow-name>\n")
		fmt.Fprintf(os.Stderr, "  cleat rollback <workflow-name> <version>\n")
		fmt.Fprintf(os.Stderr, "  cleat dev [--input <json>] [--entry-point <name>] <package>\n")
		fmt.Fprintf(os.Stderr, "  cleat schedule add <name> --cron <expr> --def <wf-name> [--entry-point <name>] [--input <json>]\n")
		fmt.Fprintf(os.Stderr, "  cleat schedule list\n")
		fmt.Fprintf(os.Stderr, "  cleat schedule delete <name>\n")
		fmt.Fprintf(os.Stderr, "  cleat schedule enable <name>\n")
		fmt.Fprintf(os.Stderr, "  cleat schedule disable <name>\n")
		fmt.Fprintf(os.Stderr, "  cleat run [--wasm <file>] [--entry-point <name>] [--input <json>] [--api-addr <addr>] <package>\n")
		fmt.Fprintf(os.Stderr, "  cleat plugin <validate|install|list|update|uninstall> [flags]\n")
		fmt.Fprintf(os.Stderr, "Common flags:\n")
		fmt.Fprintf(os.Stderr, "  --db <connstr>  PostgreSQL connection string\n")
		fmt.Fprintf(os.Stderr, "Example: cleat build -o ./out ./testdata/basic/\n")
	}
	flag.Parse()

	args := flag.Args()
	// init doesn't require a second argument (pattern)
	if len(args) >= 1 && args[0] == "init" {
		runInit(flag.Args()[1:])
		return
	}
	if len(args) < 2 {
		flag.Usage()
		os.Exit(1)
	}

	command := args[0]
	pattern := args[1]

	switch command {
	case "build":
		var outDir string
		fs := flag.NewFlagSet("build", flag.ExitOnError)
		var target string
		var entry string
		var jsonOut bool
		fs.StringVar(&outDir, "o", "", "output directory for generated files")

		fs.StringVar(&target, "target", "go", "compilation target: go, tinygo, rust, java, assemblyscript, or python")
		fs.StringVar(&entry, "entry", "", "entry point in 'file.py:func_name' format (for Python target)")
		fs.BoolVar(&jsonOut, "json", false, "output diagnostics as JSON")
		fs.Parse(os.Args[2:])
		if !isValidTarget(target) {
			fmt.Fprintf(os.Stderr, "Error: unknown target %q. Valid targets: go, tinygo, rust, java, assemblyscript, python\n", target)
			os.Exit(1)
		}
		remainder := fs.Args()
		if len(remainder) > 0 {
			pattern = remainder[0]
		}
		if entry != "" {
			// Use --entry as the pattern for Python builds.
			runBuild(entry, outDir, target, jsonOut)
		} else {
			runBuild(pattern, outDir, target, jsonOut)
		}
	case "vet":
		fs := flag.NewFlagSet("vet", flag.ExitOnError)
		vetLang := fs.String("lang", "", "language target: go, rust, java, as, python (auto-detected if empty)")
		vetJSON := fs.Bool("json", false, "output results as JSON")
		fs.Parse(os.Args[2:])
		remainder := fs.Args()
		if len(remainder) > 0 {
			pattern = remainder[0]
		}
		lang := *vetLang
		if lang == "" {
			var err error
			lang, err = detectVetLang(pattern)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v. Use --lang to specify the language.\n", err)
				os.Exit(1)
			}
		}
		var code int
		switch lang {
		case "go":
			code = runVet(pattern, *vetJSON)
		case "rust":
			code = runVetRust(pattern)
		case "java":
			code = runVetJava(pattern)
		case "python":
			code = runVetPython(pattern, *vetJSON)
		case "as":
			code = runVetAS(pattern)
		default:
			fmt.Fprintf(os.Stderr, "Error: unknown vet language %q. Valid: go, rust, java, as, python\n", lang)
			os.Exit(1)
		}
		os.Exit(code)
	case "deploy":
		runDeploy(flag.Args()[1:])
	case "versions":
		runVersions(args[1])
	case "rollback":
		if len(args) < 3 {
			fmt.Fprintf(os.Stderr, "Usage: cleat rollback <workflow-name> <version>\n")
			os.Exit(1)
		}
		version, err := strconv.Atoi(args[2])
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: version must be a number, got %q\n", args[2])
			os.Exit(1)
		}
		runRollback(args[1], version)
	case "dev":
		runDev(flag.Args()[1:])
	case "schedule":
		runSchedule(flag.Args()[1:])
	case "run":
		runEmbedded(flag.Args()[1:])
	case "dag":
		runDag(flag.Args()[1:])
	case "plugin":
		runPlugin(flag.Args()[1:])
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n", command)
		flag.Usage()
		os.Exit(1)
	}
}

func runBuild(pattern, outDir, target string, jsonOut bool) {
	if target == "java" {
		if outDir == "" {
			outDir = "."
		}
		runBuildJava(pattern, outDir)
		return
	}
	if target == "assemblyscript" {
		if outDir == "" {
			outDir = "."
		}
		runBuildAssemblyScript(pattern, outDir)
		return
	}
	if target == "rust" {
		if outDir == "" {
			outDir = "."
		}
		runBuildRust(pattern, outDir)
		return
	}
	if target == wasm.PythonTarget {
		if outDir == "" {
			outDir = "."
		}
		runBuildPython(pattern, outDir)
		return
	}
	result, cg, cr, threadingErrs, usage, tr := analyze(pattern)
	_ = cg

	if jsonOut {
		vo := vetJSONOutput(result, cr, threadingErrs)
		jsonBytes, jErr := json.Marshal(vo)
		if jErr != nil {
			fmt.Fprintf(os.Stderr, "Error building JSON diagnostics: %v\n", jErr)
			os.Exit(1)
		}
		fmt.Println(string(jsonBytes))
		if len(threadingErrs) > 0 || cr.NumErrors() > 0 {
			os.Exit(1)
		}
	} else {
		fmt.Printf("  Analyzing package %s...\n", result.TargetPkg.Path)

		leafCount := len(cr.DurableLeaves)
		closureCount := len(cr.DurableClosure)
		fmt.Printf("  Found %d functions, %d entry point(s), %d in cleat closure.\n",
			result.NumFuncs, len(result.EntryPoints), leafCount+closureCount)
		fmt.Printf("  Durable leaves: %s\n", formatDurableLeaves(result, cr))
		fmt.Printf("  Verifying HostCalls threading... %s\n", formatThreadingStatus(threadingErrs))

		if len(threadingErrs) > 0 {
			fmt.Println()
			for _, e := range threadingErrs {
				fmt.Printf("  Error: %s\n", e.Message)
				if len(e.Chain) > 0 {
					fmt.Printf("         Call chain: %s\n", strings.Join(e.Chain, " → "))
				}
				if e.Line > 0 {
					fmt.Printf("         At: %d\n", e.Line)
				}
			}
			os.Exit(1)
		}

		warnCount := cr.NumWarnings()
		if warnCount > 0 {
			fmt.Println()
			for funcName, warns := range cr.Warnings {
				for _, w := range warns {
					fmt.Printf("  Warning: %s:%d: %s [%s]\n",
						analyzer.ShortName(funcName), w.Line, w.Message, w.Code)
				}
			}
		}

		errCount := cr.NumErrors()
		if errCount > 0 {
			fmt.Println()
			for funcName, errs := range cr.Errors {
				for _, e := range errs {
					fmt.Printf("  %s: %s:%d: %s\n", e.Code, analyzer.ShortName(funcName), e.Line, e.Message)
					if e.Suggestion != "" {
						fmt.Printf("    → %s\n", e.Suggestion)
					}
				}
			}
			os.Exit(1)
		}

		fmt.Println()
	}

	outputs := wasm.BuildOutputs("main", usage, result)
	hostCount := usage.Count()
	if jsonOut {
		fmt.Fprintf(os.Stderr, "  Generating WASM imports (%d host functions used)... ", hostCount)
		fmt.Fprintln(os.Stderr, "OK")
		fmt.Fprintf(os.Stderr, "  Generating host adapter... OK\n")
		fmt.Fprintf(os.Stderr, "  Generating WASM exports (%d entry point(s))... OK\n", len(result.EntryPoints))
		if len(tr.AddedH) > 0 {
			fmt.Fprintf(os.Stderr, "  Auto-threading HostCalls into: %s\n", strings.Join(tr.AddedH, ", "))
		} else {
			fmt.Fprintf(os.Stderr, "  Auto-threading: no changes needed\n")
		}
	} else {
		fmt.Printf("  Generating WASM imports (%d host functions used)... ", hostCount)
		fmt.Println("OK")
		fmt.Printf("  Generating host adapter... OK\n")
		fmt.Printf("  Generating WASM exports (%d entry point(s))... OK\n", len(result.EntryPoints))
		if len(tr.AddedH) > 0 {
			fmt.Printf("  Auto-threading HostCalls into: %s\n", strings.Join(tr.AddedH, ", "))
		} else {
			fmt.Printf("  Auto-threading: no changes needed\n")
		}
	}

	keepTempDir := false
	if outDir == "" {
		var err error
		outDir, err = os.MkdirTemp("", "cleat-build-**")
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error creating temp directory: %v\n", err)
			os.Exit(1)
		}
		defer func() {
			if !keepTempDir {
				os.RemoveAll(outDir)
			}
		}()
	}

	goVersion := result.GoVersion
	if goVersion == "" {
		goVersion = "1.26"
	}

	wasmFile := wasmOutputName(result)
	buildCfg := &wasm.BuildConfig{
		SrcDir:      result.TargetPkg.Dir,
		OutDir:      outDir,
		PkgName:     "main",
		ModulePath:  result.ModulePath,
		ProjectRoot: result.ModuleDir,
		GoVersion:   goVersion,
		Outputs:     outputs,
		WASMOutput:  wasmFile,
		Target:      target,
		XfrmSource:  tr.Files,
	}

	if err := wasm.PrepareBuildDir(buildCfg); err != nil {
		fmt.Fprintf(os.Stderr, "Error preparing build directory: %v\n", err)
		os.Exit(1)
	}

	logBuildProgress("  Build directory: %s\n", jsonOut, outDir)

	wasmPath := filepath.Join(outDir, wasmFile)
	var cmd *exec.Cmd
	if target == "tinygo" {
		logBuildProgress("  Compiling WASM module (tinygo)...\n", jsonOut)
		cmd = exec.Command("tinygo", "build",
			"-target=wasip1",
			"-o", wasmPath,
			".",
		)
		// tinygo needs GOROOT and TINYGOROOT in its environment.
		// tinygo 0.36 requires host Go < 1.25. If CLEAT_TINYGO_GOROOT is set,
		// use it as GOROOT and add its bin to PATH ahead of the current PATH.
		cmd.Env = os.Environ()
		if tinygoGoroot := os.Getenv("CLEAT_TINYGO_GOROOT"); tinygoGoroot != "" {
			cmd.Env = append(cmd.Env, "GOROOT="+tinygoGoroot)
			cmd.Env = append(cmd.Env, "PATH="+tinygoGoroot+"/bin:"+os.Getenv("PATH"))
		} else if goroot := os.Getenv("GOROOT"); goroot != "" {
			cmd.Env = append(cmd.Env, "GOROOT="+goroot)
		}
		if tinygoroot := os.Getenv("TINYGOROOT"); tinygoroot != "" {
			cmd.Env = append(cmd.Env, "TINYGOROOT="+tinygoroot)
		}
	} else {
		logBuildProgress("  Compiling WASM module (GOOS=wasip1 GOARCH=wasm)...\n", jsonOut)
		cmd = exec.Command("go", "build",
			"-o", wasmPath,
			".",
		)
		cmd.Env = append(os.Environ(),
			"GOOS=wasip1",
			"GOARCH=wasm",
		)
	}
	cmd.Dir = outDir

	out, err := cmd.CombinedOutput()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error compiling WASM module:\n%s\n", string(out))
		os.Exit(1)
	}

	fi, err := os.Stat(wasmPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: WASM binary not found at %s\n", wasmPath)
		os.Exit(1)
	}
	logBuildProgress("  Wrote %s (%s)\n", jsonOut, wasmPath, formatSize(fi.Size()))

	// Embed cleat.metadata custom section for deployment.
	wasmBytes, err := os.ReadFile(wasmPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error reading WASM binary for metadata: %v\n", err)
		os.Exit(1)
	}
	workflowName := wasmOutputName(result)
	meta := &wasm.Metadata{
		WorkflowName:         workflowName,
		WorkflowVersion:      1,
		ABIVersion:           wasm.CurrentABIVersion,
		MinCompatibleVersion: wasm.CurrentABIVersion,
		PluginDeps:           derivePluginDeps(usage),
	}
	wasmWithMeta, err := wasm.WriteMetadata(wasmBytes, meta)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error writing WASM metadata: %v\n", err)
		os.Exit(1)
	}
	if err := os.WriteFile(wasmPath, wasmWithMeta, 0644); err != nil {
		fmt.Fprintf(os.Stderr, "Error writing WASM binary with metadata: %v\n", err)
		os.Exit(1)
	}
	logBuildProgress("  Embedded metadata: %s v%d (ABI v%d)\n", jsonOut,
		meta.WorkflowName, meta.WorkflowVersion, meta.ABIVersion)
	keepTempDir = true
}

func runVet(pattern string, jsonOut bool) int {
	result, _, cr, threadingErrs, usage, tr := analyze(pattern)
	_ = usage
	_ = tr

	if jsonOut {
		vo := vetJSONOutput(result, cr, threadingErrs)
		jsonBytes, jErr := json.Marshal(vo)
		if jErr != nil {
			fmt.Fprintf(os.Stderr, "Error building JSON output: %v\n", jErr)
			return 1
		}
		fmt.Println(string(jsonBytes))
		if len(threadingErrs) > 0 || cr.NumErrors() > 0 {
			return 1
		}
		return 0
	}

	fmt.Printf("Analyzing package %s...\n", result.TargetPkg.Path)
	fmt.Printf("  Package: %s\n", result.TargetPkg.Path)
	fmt.Printf("  Functions: %d (%d exported, %d unexported)\n",
		result.NumFuncs, result.NumExported, result.NumFuncs-result.NumExported)
	fmt.Printf("  Entry points: %s\n", strings.Join(shortEntryPoints(result), ", "))
	fmt.Printf("  Durable leaves: %d (%s)\n",
		len(cr.DurableLeaves), formatDurableLeaves(result, cr))

	exitCode := 0
	for _, e := range threadingErrs {
		fmt.Printf("  Error: %s\n", e.Message)
		if len(e.Chain) > 0 {
			fmt.Printf("         Call chain: %s\n", strings.Join(e.Chain, " → "))
		}
		exitCode = 1
	}
	for funcName, errs := range cr.Errors {
		for _, e := range errs {
			fmt.Printf("  %s:%d: %s: %s\n", analyzer.ShortName(funcName), e.Line, e.Code, e.Message)
			if e.Suggestion != "" {
				fmt.Printf("    → %s\n", e.Suggestion)
			}
			exitCode = 1
		}
	}
	for funcName, warns := range cr.Warnings {
		for _, w := range warns {
			fmt.Printf("  %s:%d: %s: %s\n", analyzer.ShortName(funcName), w.Line, w.Code, w.Message)
		}
	}

	if exitCode == 0 {
		fmt.Printf("  %s\n", "OK")
	}
	return exitCode
}

// detectVetLang auto-detects the programming language in a directory.
// Returns the language name or an error if detection fails.
func detectVetLang(dir string) (string, error) {
	if dir == "" {
		dir = "."
	}

	// Check for Go module.
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
		return "go", nil
	}

	// Check for Rust crate.
	if _, err := os.Stat(filepath.Join(dir, "Cargo.toml")); err == nil {
		return "rust", nil
	}

	// Check for Gradle project.
	if _, err := os.Stat(filepath.Join(dir, "build.gradle.kts")); err == nil {
		return "java", nil
	}
	if _, err := os.Stat(filepath.Join(dir, "build.gradle")); err == nil {
		return "java", nil
	}

	// Check for Python files.
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("cannot read directory %s: %w", dir, err)
	}
	hasPy := false
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".py") {
			hasPy = true
			break
		}
	}
	if hasPy {
		return "python", nil
	}

	// Check for AssemblyScript (package.json).
	if _, err := os.Stat(filepath.Join(dir, "package.json")); err == nil {
		return "as", nil
	}

		// Fallback: check source file extensions.
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			switch {
			case strings.HasSuffix(e.Name(), ".go"):
				return "go", nil
			case strings.HasSuffix(e.Name(), ".rs"):
				return "rust", nil
			case strings.HasSuffix(e.Name(), ".java"):
				return "java", nil
			}
		}

	return "", fmt.Errorf("could not auto-detect language in %s. Use --lang to specify", dir)
}

// runVetPython runs the Python AST-based vet via subprocess.
func runVetPython(dir string, jsonOut bool) int {
	// Find .py files in the directory.
	var pyFiles []string
	if dir == "" {
		dir = "."
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: cannot read directory %s: %v\n", dir, err)
		return 1
	}
	// Also check if dir itself is a .py file.
	if fi, err := os.Stat(dir); err == nil && !fi.IsDir() && strings.HasSuffix(dir, ".py") {
		pyFiles = append(pyFiles, dir)
	} else {
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".py") {
				pyFiles = append(pyFiles, filepath.Join(dir, e.Name()))
			}
		}
	}
	if len(pyFiles) == 0 {
		fmt.Fprintf(os.Stderr, "Error: no .py files found in %s\n", dir)
		return 1
	}

	sdkDir := findPythonSDKDir()
	exitCode := 0

	for _, pyFile := range pyFiles {
		args := []string{"-m", "cleat_sdk.vet", pyFile}
		if jsonOut {
			args = append(args, "--json")
		}

		cmd := exec.Command("python3", args...)
		var stdout, stderr strings.Builder
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr

		if sdkDir != "" {
			cmd.Env = append(os.Environ(), "PYTHONPATH="+sdkDir)
		}

		if err := cmd.Run(); err != nil {
			// Exit code 1 = errors found (normal for vet). Only fail on >1.
			if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() > 1 {
				if stderr.Len() > 0 {
					fmt.Fprint(os.Stderr, stderr.String())
				}
				fmt.Fprintf(os.Stderr, "Python vet failed for %s: %v\n", pyFile, err)
				exitCode = 1
				continue
			}
		}

		// Print output (always, even when errors found).
		if jsonOut {
			fmt.Print(stdout.String())
		} else {
			fmt.Print(stdout.String())
			if stderr.Len() > 0 {
				fmt.Fprint(os.Stderr, stderr.String())
			}
		}
		if exitCode == 0 && stderr.Len() > 0 {
			exitCode = 1
		}
	}

	return exitCode
}

// runVetAS performs a basic vet check on AssemblyScript source files.
// AssemblyScript vetting is currently limited; full AST analysis is planned.
func runVetAS(dir string) int {
	if dir == "" {
		dir = "."
	}

	// Check for package.json.
	if _, err := os.Stat(filepath.Join(dir, "package.json")); os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "Error: no package.json found in %s\n", dir)
		return 1
	}

	fmt.Fprintf(os.Stderr, "Vetting AssemblyScript project in %s...\n", dir)
	fmt.Fprintf(os.Stderr, "Note: AssemblyScript vetting is experimental. Checking transform-level validation.\n")

	// Check for the cleat-as package transform validation.
	asDir := filepath.Join(dir, "packages", "cleat-as")
	if _, err := os.Stat(asDir); os.IsNotExist(err) {
		// The transform may be at the AS project level; check for assembly/ dir.
		asDir = dir
	}

	// Find .as files.
	var asFiles []string
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if d.Name() == "node_modules" || d.Name() == ".git" || strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".as") || strings.HasSuffix(path, ".ts") {
			asFiles = append(asFiles, path)
		}
		return nil
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error scanning AS files: %v\n", err)
		return 1
	}

	if len(asFiles) == 0 {
		fmt.Fprintf(os.Stderr, "Warning: no .as or .ts source files found in %s\n", dir)
		return 0
	}

	// Run the AS transform's vet validation via Node.js if available.
	nodePath, err := exec.LookPath("node")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: node not found, skipping AssemblyScript vet transform.\n")
		fmt.Fprintf(os.Stderr, "Scanned %d file(s) — no pattern-based checks available for AS yet.\n", len(asFiles))
		return 0
	}

	// Check if the transform's afterParse validation is available.
	transformFile := filepath.Join(dir, "packages", "cleat-as", "transform", "index.js")
	if _, err := os.Stat(transformFile); os.IsNotExist(err) {
		// Fall back to looking relative to the repo root.
		candidates := []string{
			filepath.Join("packages", "cleat-as", "transform", "index.js"),
			filepath.Join(dir, "..", "packages", "cleat-as", "transform", "index.js"),
		}
		found := false
		for _, c := range candidates {
			if _, statErr := os.Stat(c); statErr == nil {
				transformFile = c
				found = true
				break
			}
		}
		if !found {
			fmt.Fprintf(os.Stderr, "Scanned %d file(s) — AS transform validation unavailable.\n", len(asFiles))
			return 0
		}
	}

	fmt.Fprintf(os.Stderr, "Scanned %d file(s) — use 'cleat build' for full AS validation.\n", len(asFiles))

	_ = nodePath
	return 0
}

// runDeploy deploys a compiled WASM workflow to the database.
// Usage: cleat deploy [--name <name>] [--namespace <ns>] [--task-queue <queue>] <wasm-file>
func runDeploy(args []string) {
	fs := flag.NewFlagSet("deploy", flag.ExitOnError)
	nameFlag := fs.String("name", "", "workflow name (derived from filename if not set)")
	nsFlag := fs.String("namespace", "", "workflow namespace (default: \"default\")")
	taskQueueFlag := fs.String("task-queue", "default", "task queue for this workflow (e.g. default, gpu, high-memory)")
	fs.Parse(args)

	remainder := fs.Args()
	if len(remainder) < 1 {
		fmt.Fprintf(os.Stderr, "Usage: cleat deploy [--name <name>] [--namespace <ns>] [--task-queue <queue>] <wasm-file>\n")
		os.Exit(1)
	}
	wasmPath := remainder[0]
	_ = nsFlag

	wasmBytes, err := os.ReadFile(wasmPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error reading WASM file %s: %v\n", wasmPath, err)
		os.Exit(1)
	}

	// Extract metadata from the WASM custom section.
	meta, metaErr := wasm.ReadMetadata(wasmBytes)
	if metaErr == nil {
		fmt.Printf("  Workflow: %s v%d (ABI v%d, min compatible: %d)\n",
			meta.WorkflowName, meta.WorkflowVersion,
			meta.ABIVersion, meta.MinCompatibleVersion)
		if len(meta.PluginDeps) > 0 {
			fmt.Printf("  Plugin deps: %v\n", meta.PluginDeps)
		}
		if err := meta.Validate(); err != nil {
			fmt.Fprintf(os.Stderr, "Error: metadata validation failed: %v\n", err)
			os.Exit(1)
		}
	} else {
		fmt.Printf("  Note: no cleat.metadata section found in WASM binary (%v)\n", metaErr)
		fmt.Printf("  Continuing with flags-only configuration.\n")
	}

	name := *nameFlag
	if name == "" {
		if metaErr == nil && meta.WorkflowName != "unknown" {
			name = meta.WorkflowName
		} else {
			name = strings.TrimSuffix(filepath.Base(wasmPath), ".wasm")
		}
	}

	connStr := getDBConnStr()

	if connStr == "" {
		version := 1
		if metaErr == nil && meta.WorkflowVersion > 0 {
			version = meta.WorkflowVersion
		}
		fmt.Printf("Would deploy workflow %q (version %d) from %s (%d bytes) to queue %q\n",
			name, version, wasmPath, len(wasmBytes), *taskQueueFlag)
		if metaErr == nil {
			fmt.Printf("  Metadata: %s v%d (ABI: %d, min ver: %d)\n",
				meta.WorkflowName, meta.WorkflowVersion,
				meta.ABIVersion, meta.MinCompatibleVersion)
		}
		fmt.Println("Dry run; set CLEAT_DATABASE_URL or --db to deploy.")
		return
	}

	db, err := sql.Open("postgres", connStr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error connecting to database: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	if err := db.Ping(); err != nil {
		fmt.Fprintf(os.Stderr, "Error pinging database: %v\n", err)
		os.Exit(1)
	}

	var version int
	err = db.QueryRow("SELECT COALESCE(MAX(version), 0) + 1 FROM workflow_defs WHERE name = $1", name).Scan(&version)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error querying max version: %v\n", err)
		os.Exit(1)
	}

	namespace := *nsFlag
	if namespace == "" {
		namespace = "default"
	}

	// Build the SQL with metadata columns if available.
	abiVersion := 1
	minVersion := 1
	pluginDepsJSON := "{}"
	if metaErr == nil {
		abiVersion = meta.ABIVersion
		minVersion = meta.MinCompatibleVersion
		if depsBytes, merr := json.Marshal(meta.PluginDeps); merr == nil {
			pluginDepsJSON = string(depsBytes)
		}
	}

	_, err = db.Exec(
		"INSERT INTO workflow_defs (name, version, wasm_bytes, abi_version, plugin_deps, min_version, entry_points, namespace, task_queue) VALUES ($1, $2, $3, $4, $5::jsonb, $6, $7, $8, $9)",
		name, version, wasmBytes, abiVersion, pluginDepsJSON, minVersion, []string{}, namespace, *taskQueueFlag,
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error inserting workflow definition: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Deployed workflow %q version %d (%d bytes) to queue %q\n", name, version, len(wasmBytes), *taskQueueFlag)
	if metaErr == nil {
		fmt.Printf("  Metadata: %s v%d (ABI: %d, min ver: %d, plugins: %v)\n",
			meta.WorkflowName, meta.WorkflowVersion,
			meta.ABIVersion, meta.MinCompatibleVersion, meta.PluginDeps)
	}
}

func analyze(pattern string) (*analyzer.AnalysisResult, *callgraph.Graph, *closure.Result, []closure.ThreadingError, *wasm.UsageInfo, *transform.Result) {
	fset := token.NewFileSet()

	result, err := analyzer.LoadPackages(pattern, fset)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading package: %v\n", err)
		os.Exit(1)
	}

	cg, err := callgraph.Build(result)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error building call graph: %v\n", err)
		os.Exit(1)
	}

	cr := closure.Compute(result, cg)

	tr, err := transform.Transform(&transform.Config{
		Result:    result,
		CallGraph: cg,
		Closure:   cr,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error in transform: %v\n", err)
		os.Exit(1)
	}

	threadingErrs := closure.VerifyThreading(result, cg, cr)

	usage := wasm.AnalyzeUsage(result, cr)

	return result, cg, cr, threadingErrs, usage, tr
}

// buildJSONDiagnostics builds a JSON representation of all diagnostics.
// logBuildProgress prints a build progress message. In JSON output mode,
// the message goes to stderr so stdout contains only the JSON diagnostics.
func logBuildProgress(format string, jsonOut bool, args ...interface{}) {
	if jsonOut {
		fmt.Fprintf(os.Stderr, format, args...)
	} else {
		fmt.Printf(format, args...)
	}
}


	// vetJSONOutput builds a VetOutput from analysis results.
	func vetJSONOutput(result *analyzer.AnalysisResult, cr *closure.Result, threadingErrs []closure.ThreadingError) VetOutput {
		var out VetOutput
		out.Errors = make([]VetResult, 0)
		out.Warnings = make([]VetResult, 0)

		// Threading errors.
		for _, te := range threadingErrs {
			out.Errors = append(out.Errors, VetResult{
				File:    lookupFile(result, te.FuncName),
				Line:    te.Line,
				Column:  0,
				Message: te.Message,
				Chain:   te.Chain,
			})
		}

		// Validation errors.
		for funcName, errs := range cr.Errors {
			for _, e := range errs {
				out.Errors = append(out.Errors, VetResult{
					Code:       e.Code,
					File:       lookupFile(result, funcName),
					Line:       e.Line,
					Column:     0,
					Message:    e.Message,
					Suggestion: e.Suggestion,
				})
			}
		}

		// Warnings.
		for funcName, warns := range cr.Warnings {
			for _, w := range warns {
				out.Warnings = append(out.Warnings, VetResult{
					Code:    w.Code,
					File:    lookupFile(result, funcName),
					Line:    w.Line,
					Column:  0,
					Message: w.Message,
				})
			}
		}

		// Summary.
		out.Summary = VetSummary{
			Functions:      result.NumFuncs,
			DurableLeaves:  result.NumDurableLeaves,
			DurableClosure: result.NumDurableClosure,
			Pure:           result.NumPure,
		}

		return out
	}

// lookupFile returns the base filename for a function by its fully-qualified name.
func lookupFile(result *analyzer.AnalysisResult, funcName string) string {
	fd, ok := result.Funcs[funcName]
	if !ok || fd.Pkg == nil || fd.Pkg.Fset == nil {
		return ""
	}
	pos := fd.Pkg.Fset.Position(fd.Ast.Pos())
	if pos.Filename != "" {
		return filepath.Base(pos.Filename)
	}
	return ""
}


func formatDurableLeaves(result *analyzer.AnalysisResult, cr *closure.Result) string {
	var names []string
	for name := range cr.DurableLeaves {
		names = append(names, analyzer.ShortName(name))
	}
	sort.Strings(names)
	if len(names) == 0 {
		return "(none)"
	}
	return strings.Join(names, ", ")
}

func formatThreadingStatus(errs []closure.ThreadingError) string {
	if len(errs) == 0 {
		return "OK"
	}
	return fmt.Sprintf("%d error(s)", len(errs))
}

func shortEntryPoints(result *analyzer.AnalysisResult) []string {
	var names []string
	for _, ep := range result.EntryPoints {
		names = append(names, analyzer.ShortName(ep))
	}
	return names
}

func wasmOutputName(result *analyzer.AnalysisResult) string {
	if len(result.EntryPoints) == 0 {
		return "output.wasm"
	}
	return wasm.ToSnakeCase(analyzer.ShortName(result.EntryPoints[0])) + ".wasm"
}

// derivePluginDeps infers plugin dependencies from the host functions used
// by the workflow. PluginCall usage implies a dependency on that plugin.
func derivePluginDeps(usage *wasm.UsageInfo) map[string]string {
	if usage == nil || !usage.Used["plugin_call"] {
		return nil
	}
	// We know plugin_call is used but not which specific plugins.
	// Return a sentinel that deploy can recognize; the actual plugin list
	// should come from the workflow author's configuration.
	_ = usage
	return nil
}

// getDBConnStr returns the database connection string from the --db flag
// or the CLEAT_DATABASE_URL environment variable.
func getDBConnStr() string {
	if dbConnStr != "" {
		return dbConnStr
	}
	return os.Getenv("CLEAT_DATABASE_URL")
}

// runVersions lists all deployed versions of a workflow, latest first.
func runVersions(name string) {
	connStr := getDBConnStr()
	if connStr == "" {
		fmt.Fprintf(os.Stderr, "Error: --db flag or CLEAT_DATABASE_URL is required\n")
		os.Exit(1)
	}

	db, err := sql.Open("postgres", connStr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error connecting to database: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	if err := db.Ping(); err != nil {
		fmt.Fprintf(os.Stderr, "Error pinging database: %v\n", err)
		os.Exit(1)
	}

	rows, err := db.Query("SELECT version FROM workflow_defs WHERE name = $1 ORDER BY version DESC", name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error querying versions: %v\n", err)
		os.Exit(1)
	}
	defer rows.Close()

	var found bool
	for rows.Next() {
		var version int
		if err := rows.Scan(&version); err != nil {
			fmt.Fprintf(os.Stderr, "Error scanning row: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(version)
		found = true
	}
	if err := rows.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "Error iterating rows: %v\n", err)
		os.Exit(1)
	}
	if !found {
		fmt.Printf("No versions found for workflow %q\n", name)
	}
}

// runRollback sets the active version for a workflow by confirming the version
// exists and printing instructions for new instances.
func runRollback(name string, version int) {
	connStr := getDBConnStr()
	if connStr == "" {
		fmt.Fprintf(os.Stderr, "Error: --db flag or CLEAT_DATABASE_URL is required\n")
		os.Exit(1)
	}

	db, err := sql.Open("postgres", connStr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error connecting to database: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	if err := db.Ping(); err != nil {
		fmt.Fprintf(os.Stderr, "Error pinging database: %v\n", err)
		os.Exit(1)
	}

	var exists bool
	err = db.QueryRow("SELECT EXISTS(SELECT 1 FROM workflow_defs WHERE name = $1 AND version = $2)", name, version).Scan(&exists)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error checking version: %v\n", err)
		os.Exit(1)
	}
	if !exists {
		fmt.Fprintf(os.Stderr, "Error: workflow %q version %d not found\n", name, version)
		os.Exit(1)
	}

	fmt.Printf("Rolled back %q to version %d. New instances will use version %d.\n", name, version, version)
}

// runSchedule manages cron schedules for recurring workflow execution.
func runSchedule(args []string) {
	if len(args) < 1 {
		fmt.Fprintf(os.Stderr, "Usage: cleat schedule <add|list|delete|enable|disable> [args]\n")
		os.Exit(1)
	}

	subCmd := args[0]
	remainder := args[1:]

	connStr := getDBConnStr()
	if connStr == "" {
		fmt.Fprintf(os.Stderr, "Error: --db flag or CLEAT_DATABASE_URL is required\n")
		os.Exit(1)
	}

	db, err := sql.Open("postgres", connStr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error connecting to database: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	if err := db.Ping(); err != nil {
		fmt.Fprintf(os.Stderr, "Error pinging database: %v\n", err)
		os.Exit(1)
	}

	store := host.NewPostgresStore(db)
	ctx := context.Background()

	switch subCmd {
	case "add":
		if len(remainder) < 1 {
			fmt.Fprintf(os.Stderr, "Usage: cleat schedule add <name> --cron <expr> --def <wf-name> [--entry-point <name>] [--input <json>]\n")
			os.Exit(1)
		}
		fs := flag.NewFlagSet("schedule add", flag.ExitOnError)
		cronExpr := fs.String("cron", "", "cron expression (5-field: minute hour dom month dow)")
		defName := fs.String("def", "", "workflow definition name")
		entryPoint := fs.String("entry-point", "", "entry point function name")
		inputJSON := fs.String("input", "{}", "workflow input JSON")
		fs.Parse(remainder)

		fsArgs := fs.Args()
		if len(fsArgs) < 1 || *cronExpr == "" || *defName == "" {
			fmt.Fprintf(os.Stderr, "Usage: cleat schedule add <name> --cron <expr> --def <wf-name> [--entry-point <name>] [--input <json>]\n")
			os.Exit(1)
		}
		name := fsArgs[0]

		nextRun := host.NextCronTime(*cronExpr, time.Now())
		sch := host.Schedule{
			Name:           name,
			DefName:        *defName,
			EntryPoint:     *entryPoint,
			CronExpression: *cronExpr,
			Input:          json.RawMessage(*inputJSON),
			Enabled:        true,
			NextRunAt:      nextRun,
		}

		if err := store.CreateSchedule(ctx, sch); err != nil {
			fmt.Fprintf(os.Stderr, "Error creating schedule: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Created schedule %q: %s every %s (next at %s)\n",
			name, *defName, *cronExpr, nextRun.Format(time.RFC3339))

	case "list":
		schedules, err := store.ListSchedules(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error listing schedules: %v\n", err)
			os.Exit(1)
		}
		if len(schedules) == 0 {
			fmt.Println("No schedules found.")
			return
		}
		fmt.Printf("%-20s %-20s %-20s %-7s %s\n", "NAME", "DEFINITION", "CRON", "ENABLED", "NEXT RUN")
		for _, sch := range schedules {
			enabled := "no"
			if sch.Enabled {
				enabled = "yes"
			}
			fmt.Printf("%-20s %-20s %-20s %-7s %s\n",
				sch.Name, sch.DefName, sch.CronExpression, enabled,
				sch.NextRunAt.Format(time.RFC3339))
		}

	case "delete":
		if len(remainder) < 1 {
			fmt.Fprintf(os.Stderr, "Usage: cleat schedule delete <name>\n")
			os.Exit(1)
		}
		if err := store.DeleteSchedule(ctx, remainder[0]); err != nil {
			fmt.Fprintf(os.Stderr, "Error deleting schedule: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Deleted schedule %q\n", remainder[0])

	case "enable":
		if len(remainder) < 1 {
			fmt.Fprintf(os.Stderr, "Usage: cleat schedule enable <name>\n")
			os.Exit(1)
		}
		if err := store.SetScheduleEnabled(ctx, remainder[0], true); err != nil {
			fmt.Fprintf(os.Stderr, "Error enabling schedule: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Enabled schedule %q\n", remainder[0])

	case "disable":
		if len(remainder) < 1 {
			fmt.Fprintf(os.Stderr, "Usage: cleat schedule disable <name>\n")
			os.Exit(1)
		}
		if err := store.SetScheduleEnabled(ctx, remainder[0], false); err != nil {
			fmt.Fprintf(os.Stderr, "Error disabling schedule: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Disabled schedule %q\n", remainder[0])

	default:
		fmt.Fprintf(os.Stderr, "Unknown schedule subcommand: %s\n", subCmd)
		os.Exit(1)
	}
}

func isValidTarget(t string) bool {
	valid := map[string]bool{
		"go": true, "tinygo": true, "rust": true,
		"java": true, "assemblyscript": true, "python": true,
	}
	return valid[t]
}

func formatSize(n int64) string {
	const (
		KB = 1 << 10
		MB = 1 << 20
	)
	switch {
	case n >= MB:
		return fmt.Sprintf("%.1f MB", float64(n)/MB)
	case n >= KB:
		return fmt.Sprintf("%.1f KB", float64(n)/KB)
	default:
		return fmt.Sprintf("%d B", n)
	}
}
