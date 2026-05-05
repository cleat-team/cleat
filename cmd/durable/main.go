// Command durable is the workflow transformer CLI.
//
// Usage:
//
//	durable build <package>     — analyze and compile a workflow package
//	durable vet <package>       — validate a workflow package (no compilation)
//
// The build command runs the full transformer pipeline: package loading,
// call graph construction, durable closure computation, HostCalls threading
// verification, WASM import generation, host adapter generation, WASM export
// generation, and compilation.
package main

import (
	"flag"
	"fmt"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/rcownie/durable/internal/analyzer"
	"github.com/rcownie/durable/internal/callgraph"
	"github.com/rcownie/durable/internal/closure"
	"github.com/rcownie/durable/internal/transform"
	"github.com/rcownie/durable/internal/wasm"
)

func main() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: durable <build|vet> [flags] <package>\n")
		fmt.Fprintf(os.Stderr, "  durable build [-o <dir>] <package>\n")
		fmt.Fprintf(os.Stderr, "  durable vet <package>\n")
		fmt.Fprintf(os.Stderr, "Example: durable build -o ./out ./testdata/basic/\n")
	}
	flag.Parse()

	args := flag.Args()
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
		fs.StringVar(&outDir, "o", "", "output directory for generated files")
		fs.Parse(os.Args[2:])
		remainder := fs.Args()
		if len(remainder) > 0 {
			pattern = remainder[0]
		}
		runBuild(pattern, outDir)
	case "vet":
		runVet(pattern)
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n", command)
		flag.Usage()
		os.Exit(1)
	}
}

func runBuild(pattern, outDir string) {
	result, cg, cr, threadingErrs, usage, tr := analyze(pattern)
	_ = cg

	fmt.Printf("  Analyzing package %s...\n", result.TargetPkg.Path)

	leafCount := len(cr.DurableLeaves)
	closureCount := len(cr.DurableClosure)
	fmt.Printf("  Found %d functions, %d entry point(s), %d in durable closure.\n",
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

	outputs := wasm.BuildOutputs(result.TargetPkg.Name, usage, result)
	hostCount := usage.Count()
	fmt.Printf("  Generating WASM imports (%d host functions used)... ", hostCount)
	fmt.Println("OK")
	fmt.Printf("  Generating host adapter... OK\n")
	fmt.Printf("  Generating WASM exports (%d entry point(s))... OK\n", len(result.EntryPoints))

	if len(tr.AddedH) > 0 {
		fmt.Printf("  Auto-threading HostCalls into: %s\n", strings.Join(tr.AddedH, ", "))
	} else {
		fmt.Printf("  Auto-threading: no changes needed\n")
	}

	if outDir == "" {
		var err error
		outDir, err = os.MkdirTemp("", "durable-build-*")
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error creating temp directory: %v\n", err)
			os.Exit(1)
		}
	}

	goVersion := result.GoVersion
	if goVersion == "" {
		goVersion = "1.26"
	}

	wasmFile := wasmOutputName(result)
	buildCfg := &wasm.BuildConfig{
		SrcDir:      result.TargetPkg.Dir,
		OutDir:      outDir,
		PkgName:     result.TargetPkg.Name,
		ModulePath:  result.ModulePath,
		ProjectRoot: result.ModuleDir,
		GoVersion:   goVersion,
		Outputs:     outputs,
		WASMOutput:  wasmFile,
		XfrmSource:  tr.Files,
	}

	if err := wasm.PrepareBuildDir(buildCfg); err != nil {
		fmt.Fprintf(os.Stderr, "Error preparing build directory: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("  Build directory: %s\n", outDir)

	fmt.Printf("  Compiling WASM module (GOOS=wasip1 GOARCH=wasm)...\n")
	wasmPath := filepath.Join(outDir, wasmFile)
	cmd := exec.Command("go", "build",
		"-o", wasmPath,
		".",
	)
	cmd.Dir = outDir
	cmd.Env = append(os.Environ(),
		"GOOS=wasip1",
		"GOARCH=wasm",
	)

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
	fmt.Printf("  Wrote %s (%s)\n", wasmPath, formatSize(fi.Size()))
}

func runVet(pattern string) {
	result, _, cr, threadingErrs, usage, tr := analyze(pattern)
	_ = usage
	_ = tr

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
	os.Exit(exitCode)
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
	return toSnakeCaseMain(analyzer.ShortName(result.EntryPoints[0])) + ".wasm"
}

func toSnakeCaseMain(s string) string {
	var b strings.Builder
	for i, r := range s {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				b.WriteByte('_')
			}
			b.WriteByte(byte(r + 32))
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
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
