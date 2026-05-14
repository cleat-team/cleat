package wasm

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/internal/analyzer"
	"github.com/cleat-team/cleat/internal/callgraph"
	"github.com/cleat-team/cleat/internal/closure"
	"github.com/cleat-team/cleat/internal/transform"
)

// TestGenericsPipelineRunsCleanly runs the generics testdata through all 5
// pipeline stages and verifies the output is valid Go.
func TestGenericsPipelineRunsCleanly(t *testing.T) {
	// Stage 1: Analyzer
	fset := token.NewFileSet()
	result, err := analyzer.LoadPackages("github.com/cleat-team/cleat/testdata/generics", fset)
	if err != nil {
		t.Fatalf("Stage 1 (Analyzer): LoadPackages failed: %v", err)
	}
	if len(result.Funcs) == 0 {
		t.Fatal("Stage 1 (Analyzer): no functions found")
	}

	// Verify generic functions are present.
	for _, name := range []string{
		"github.com/cleat-team/cleat/testdata/generics.Process",
		"github.com/cleat-team/cleat/testdata/generics.GenericLeaf",
		"github.com/cleat-team/cleat/testdata/generics.EntryPoint",
		"*Container[T].Process",
	} {
		if result.Funcs[name] == nil {
			t.Errorf("Stage 1 (Analyzer): function %s not found", name)
		}
	}

	// Stage 2: Callgraph
	cg, err := callgraph.Build(result)
	if err != nil {
		t.Fatalf("Stage 2 (Callgraph): Build failed: %v", err)
	}
	if cg.NumEdges() == 0 {
		t.Error("Stage 2 (Callgraph): expected at least one edge")
	}

	// Verify edges from entry point to generic functions.
	epFQ := "github.com/cleat-team/cleat/testdata/generics.EntryPoint"
	processFQ := "github.com/cleat-team/cleat/testdata/generics.Process"
	leafFQ := "github.com/cleat-team/cleat/testdata/generics.GenericLeaf"
	methodFQ := "*Container[T].Process"

	if !cg.Calls[epFQ][processFQ] {
		t.Error("Stage 2 (Callgraph): missing edge EntryPoint -> Process")
	}
	if !cg.Calls[epFQ][leafFQ] {
		t.Error("Stage 2 (Callgraph): missing edge EntryPoint -> GenericLeaf")
	}
	if !cg.Calls[epFQ][methodFQ] {
		t.Error("Stage 2 (Callgraph): missing edge EntryPoint -> *Container[T].Process")
	}

	// Stage 3: Closure computation
	cr := closure.Compute(result, cg)

	if !cr.DurableLeaves[processFQ] {
		t.Error("Stage 3 (Closure): Process should be a durable leaf")
	}
	if !cr.DurableLeaves[leafFQ] {
		t.Error("Stage 3 (Closure): GenericLeaf should be a durable leaf")
	}
	if !cr.DurableLeaves[methodFQ] {
		t.Error("Stage 3 (Closure): *Container[T].Process should be a durable leaf")
	}
	if !cr.DurableClosure[epFQ] {
		t.Error("Stage 3 (Closure): EntryPoint should be in durable closure")
	}

	if cr.NumErrors() > 0 {
		for name, errs := range cr.Errors {
			for _, e := range errs {
				t.Errorf("Stage 3 (Closure): unexpected error in %s: %s", name, e.Message)
			}
		}
	}

	// Stage 4: Analyze usage (must run before Transform since Transform
	// rewrites h.MethodName(...) → host_MethodName(...)).
	usage := AnalyzeUsage(result, cr)
	if usage.Count() == 0 {
		t.Error("Stage 4 (Usage): expected at least one used host function")
	}

	// Stage 6: Transform
	tr, err := transform.Transform(&transform.Config{
		Result:    result,
		CallGraph: cg,
		Closure:   cr,
	})
	if err != nil {
		t.Fatalf("Stage 6 (Transform): Transform failed: %v", err)
	}

	for name, content := range tr.Files {
		// Check that output is valid Go.
		_, parseErr := parser.ParseFile(token.NewFileSet(), name, string(content), parser.AllErrors)
		if parseErr != nil {
			t.Errorf("Stage 6 (Transform): %s is not valid Go: %v", name, parseErr)
		}
	}

	// Stage 6: WASM build preparation

	// Build the output files.
	outputs := BuildOutputs("main", usage, result, "go")
	if outputs == nil {
		t.Fatal("Stage 6 (BuildOutputs): nil OutputFiles")
	}

	// Verify generated code references types used by generics.
	for name, content := range map[string]string{
		"gen_host_adapter.go": outputs.Adapter,
		"gen_wasm_imports.go": outputs.Imports,
		"gen_wasm_exports.go": outputs.Exports,
		"gen_wasm_memory.go":  outputs.Memory,
	} {
		if content == "" {
			t.Errorf("Stage 6: %s is empty", name)
		}
		if !strings.Contains(content, "package main") {
			t.Errorf("Stage 6: %s should have package main", name)
		}
	}

	// Prepare the build directory to verify everything works end-to-end.
	tmpDir := t.TempDir()
	projRoot := filepath.Join(tmpDir, "project")
	if err := os.MkdirAll(projRoot, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projRoot, "go.mod"), []byte("module test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	outDir := filepath.Join(tmpDir, "out")

	// Convert transformed Go files to XfrmSource.
	xfrmSource := make(map[string][]byte)
	for name, content := range tr.Files {
		xfrmSource[filepath.Base(name)] = content
	}

	// Collect source files from the original package.
	srcDir := filepath.Join(tmpDir, "src")
	if err := os.MkdirAll(srcDir, 0755); err != nil {
		t.Fatal(err)
	}
	for _, srcFile := range result.TargetPkg.Files {
		fname := fset.Position(srcFile.Pos()).Filename
		if fname == "" {
			continue
		}
		base := filepath.Base(fname)
		if _, ok := xfrmSource[base]; !ok {
			data, err := os.ReadFile(fname)
			if err != nil {
				t.Fatal(err)
			}
			xfrmSource[base] = data
		}
	}

	cfg := &BuildConfig{
		OutDir:      outDir,
		PkgName:     "main",
		ModulePath:  "github.com/test/module",
		ProjectRoot: projRoot,
		GoVersion:   "1.26",
		XfrmSource:  xfrmSource,
		Outputs:     outputs,
		WASMOutput:  "out.wasm",
	}

	if err := PrepareBuildDir(cfg); err != nil {
		t.Fatalf("Stage 6 (PrepareBuildDir): %v", err)
	}

	// Verify key files were created.
	for _, name := range []string{
		"gen_wasm_imports.go",
		"gen_wasm_memory.go",
		"gen_host_adapter.go",
		"gen_wasm_exports.go",
		"gen_main_stub.go",
		"go.mod",
	} {
		path := filepath.Join(outDir, name)
		if _, err := os.Stat(path); os.IsNotExist(err) {
			t.Errorf("Stage 6: expected file %s was not created", name)
		}
	}

	// Source files should be rewritten to package main.
	if len(xfrmSource) > 0 {
		for base := range xfrmSource {
			if strings.HasPrefix(base, "gen_") {
				continue
			}
			path := filepath.Join(outDir, base)
			data, err := os.ReadFile(path)
			if err != nil {
				t.Errorf("could not read %s: %v", base, err)
				continue
			}
			if !strings.Contains(string(data), "package main") {
				t.Errorf("%s should be rewritten to package main", base)
			}
			break
		}
	}
}
