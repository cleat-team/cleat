package wasm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// FindRepoRoot
// ---------------------------------------------------------------------------

func TestFindRepoRoot(t *testing.T) {
	// The test's own source tree has a go.mod under /localssd/..., and in
	// some CI environments /tmp may also have one, so we create our own
	// isolated temp trees and control exactly where go.mod lives.

	t.Run("found-in-parent", func(t *testing.T) {
		tmpDir := t.TempDir()
		parent := filepath.Join(tmpDir, "a")
		child := filepath.Join(parent, "b")
		if err := os.MkdirAll(child, 0755); err != nil {
			t.Fatal(err)
		}
		modPath := filepath.Join(parent, "go.mod")
		if err := os.WriteFile(modPath, []byte("module test\n"), 0644); err != nil {
			t.Fatal(err)
		}

		root, err := FindRepoRoot(child)
		if err != nil {
			t.Fatalf("FindRepoRoot: %v", err)
		}
		if root != parent {
			t.Errorf("got %q, want %q", root, parent)
		}
	})

	t.Run("found-in-current", func(t *testing.T) {
		tmpDir := t.TempDir()
		modPath := filepath.Join(tmpDir, "go.mod")
		if err := os.WriteFile(modPath, []byte("module test\n"), 0644); err != nil {
			t.Fatal(err)
		}

		root, err := FindRepoRoot(tmpDir)
		if err != nil {
			t.Fatalf("FindRepoRoot: %v", err)
		}
		if root != tmpDir {
			t.Errorf("got %q, want %q", root, tmpDir)
		}
	})

	t.Run("not-found", func(t *testing.T) {
		// /var/tmp is clean (no go.mod in its hierarchy), unlike /tmp
		// which can have one in CI.  This exercises the walk-to-root path.
		cleanDir, err := os.MkdirTemp("/var/tmp", "cleat-test-notfound-*")
		if err != nil {
			t.Fatal(err)
		}
		defer os.RemoveAll(cleanDir)

		_, err = FindRepoRoot(cleanDir)
		if err == nil {
			t.Fatal("expected error for tree without go.mod")
		}
		if !strings.Contains(err.Error(), "go.mod not found") {
			t.Errorf("expected 'go.mod not found' error, got: %v", err)
		}
	})

	t.Run("nonexistent-path", func(t *testing.T) {
		_, err := FindRepoRoot("/nonexistent/path/here")
		if err == nil {
			t.Fatal("expected error for nonexistent path")
		}
	})
}

// ---------------------------------------------------------------------------
// PrepareBuildDir  (default "go" target)
// ---------------------------------------------------------------------------

// buildTestConfig is a helper that creates a temporary project root (with
// go.mod), a source directory with one .go file, and returns a *BuildConfig
// pointed at clean temp dirs.  The caller may override fields before calling
// PrepareBuildDir.
func buildTestConfig(t *testing.T) (*BuildConfig, string, string) {
	t.Helper()
	tmpDir := t.TempDir()

	projRoot := filepath.Join(tmpDir, "project")
	if err := os.MkdirAll(projRoot, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projRoot, "go.mod"), []byte("module test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	srcDir := filepath.Join(tmpDir, "src")
	if err := os.MkdirAll(srcDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "workflow.go"),
		[]byte("package mypkg\n\nfunc F() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}

	outDir := filepath.Join(tmpDir, "out")

	cfg := &BuildConfig{
		SrcDir:      srcDir,
		OutDir:      outDir,
		PkgName:     "main",
		ModulePath:  "github.com/test/module",
		ProjectRoot: projRoot,
		GoVersion:   "1.26",
		Outputs: &OutputFiles{
			Imports: "// gen_wasm_imports.go\npackage main\n",
			Memory:  "// gen_wasm_memory.go\npackage main\n",
			Adapter: "// gen_host_adapter.go\npackage main\n",
			Exports: "// gen_wasm_exports.go\npackage main\n",
		},
		WASMOutput: "out.wasm",
	}
	return cfg, outDir, srcDir
}

func TestPrepareBuildDirHappyPath(t *testing.T) {
	cfg, outDir, _ := buildTestConfig(t)

	if err := PrepareBuildDir(cfg); err != nil {
		t.Fatalf("PrepareBuildDir: %v", err)
	}

	expected := []string{
		"workflow.go",
		"gen_wasm_imports.go",
		"gen_wasm_memory.go",
		"gen_host_adapter.go",
		"gen_wasm_exports.go",
		"gen_main_stub.go",
		"go.mod",
	}
	for _, name := range expected {
		path := filepath.Join(outDir, name)
		if _, err := os.Stat(path); os.IsNotExist(err) {
			t.Errorf("expected file %s was not created", name)
		}
	}

	// Source file should be rewritten to "package main".
	content, err := os.ReadFile(filepath.Join(outDir, "workflow.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "package main") {
		t.Errorf("source not rewritten: %s", string(content))
	}

	// Main stub should be valid Go (default target has goroutine-based stub).
	stub, err := os.ReadFile(filepath.Join(outDir, "gen_main_stub.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(stub), "package main") || !strings.Contains(string(stub), "func main()") {
		t.Errorf("expected valid main stub, got: %s", string(stub))
	}

	// go.mod should reference the module path and version.
	mod, err := os.ReadFile(filepath.Join(outDir, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(mod), "go 1.26") {
		t.Errorf("expected go 1.26 in go.mod, got: %s", string(mod))
	}
	if !strings.Contains(string(mod), "github.com/test/module") {
		t.Errorf("expected module path in go.mod, got: %s", string(mod))
	}
}

func TestPrepareBuildDirWithXfrmSource(t *testing.T) {
	tmpDir := t.TempDir()

	projRoot := filepath.Join(tmpDir, "project")
	if err := os.MkdirAll(projRoot, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projRoot, "go.mod"), []byte("module test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	outDir := filepath.Join(tmpDir, "out")

	cfg := &BuildConfig{
		OutDir:      outDir,
		PkgName:     "main",
		ModulePath:  "github.com/test/module",
		ProjectRoot: projRoot,
		GoVersion:   "1.26",
		Outputs: &OutputFiles{
			Imports: "// imports\n",
			Memory:  "// memory\n",
			Adapter: "// adapter\n",
			Exports: "// exports\n",
		},
		XfrmSource: map[string][]byte{
			"handler.go": []byte("package mypkg\n\nfunc Handle() {}\n"),
			"gen_skip.go": []byte("should be skipped\n"),
		},
	}

	if err := PrepareBuildDir(cfg); err != nil {
		t.Fatalf("PrepareBuildDir: %v", err)
	}

	// handler.go should exist and have rewritten package.
	content, err := os.ReadFile(filepath.Join(outDir, "handler.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "package main") {
		t.Errorf("expected rewritten package main, got: %s", string(content))
	}

	// Files beginning with "gen_" should be skipped in the source copy phase.
	if _, err := os.Stat(filepath.Join(outDir, "gen_skip.go")); !os.IsNotExist(err) {
		t.Error("gen_skip.go should not have been written by source copy")
	}

	// Generated files should still be written.
	for _, name := range []string{"gen_wasm_imports.go", "gen_host_adapter.go"} {
		if _, err := os.Stat(filepath.Join(outDir, name)); os.IsNotExist(err) {
			t.Errorf("generated file %s was not created", name)
		}
	}
}

func TestPrepareBuildDirEmptyOutputs(t *testing.T) {
	tmpDir := t.TempDir()

	projRoot := filepath.Join(tmpDir, "project")
	if err := os.MkdirAll(projRoot, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projRoot, "go.mod"), []byte("module test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	srcDir := filepath.Join(tmpDir, "src")
	if err := os.MkdirAll(srcDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "empty.go"), []byte("package x\n"), 0644); err != nil {
		t.Fatal(err)
	}

	outDir := filepath.Join(tmpDir, "out")

	cfg := &BuildConfig{
		SrcDir:      srcDir,
		OutDir:      outDir,
		PkgName:     "main",
		ModulePath:  "github.com/test/module",
		ProjectRoot: projRoot,
		GoVersion:   "1.24",
		Outputs:     &OutputFiles{}, // all empty — should be skipped
	}

	if err := PrepareBuildDir(cfg); err != nil {
		t.Fatalf("PrepareBuildDir: %v", err)
	}

	// Empty outputs should not create gen_* files.
	for _, name := range []string{"gen_wasm_imports.go", "gen_wasm_memory.go",
		"gen_host_adapter.go", "gen_wasm_exports.go"} {
		if _, err := os.Stat(filepath.Join(outDir, name)); !os.IsNotExist(err) {
			t.Errorf("%s should not exist (empty output)", name)
		}
	}

	// Main stub and go.mod should always be written.
	if _, err := os.Stat(filepath.Join(outDir, "gen_main_stub.go")); os.IsNotExist(err) {
		t.Error("gen_main_stub.go should exist")
	}
	if _, err := os.Stat(filepath.Join(outDir, "go.mod")); os.IsNotExist(err) {
		t.Error("go.mod should exist")
	}
}

func TestPrepareBuildDirTinygoTarget(t *testing.T) {
	tmpDir := t.TempDir()

	projRoot := filepath.Join(tmpDir, "project")
	if err := os.MkdirAll(projRoot, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projRoot, "go.mod"), []byte("module test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// The tinygo path copies the cleat SDK source from projectRoot/cleat/
	// into .deps/cleat/.  Create a minimal cleat SDK stub.
	cleatDir := filepath.Join(projRoot, "cleat")
	if err := os.MkdirAll(cleatDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cleatDir, "hostcalls.go"),
		[]byte("package cleat\n\n// stub\n"), 0644); err != nil {
		t.Fatal(err)
	}

	srcDir := filepath.Join(tmpDir, "src")
	if err := os.MkdirAll(srcDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "wf.go"),
		[]byte("package mypkg\n\nfunc Run() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}

	outDir := filepath.Join(tmpDir, "out")

	cfg := &BuildConfig{
		SrcDir:      srcDir,
		OutDir:      outDir,
		PkgName:     "main",
		ModulePath:  "github.com/test/module",
		ProjectRoot: projRoot,
		GoVersion:   "1.26",
		Target:      "tinygo",
		Outputs: &OutputFiles{
			Imports: "// imports\n",
			Memory:  "// memory\n",
			Adapter: "// adapter\n",
			Exports: "// exports\n",
		},
	}

	if err := PrepareBuildDir(cfg); err != nil {
		t.Fatalf("PrepareBuildDir: %v", err)
	}

	// Main stub should use channel block for tinygo.
	stub, err := os.ReadFile(filepath.Join(outDir, "gen_main_stub.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(stub), "<-make(chan struct{})") {
		t.Errorf("expected channel block in tinygo stub, got: %s", string(stub))
	}

	// .deps/go.mod should have go 1.23 (capped).
	depsMod, err := os.ReadFile(filepath.Join(outDir, ".deps", "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(depsMod), "go 1.23") {
		t.Errorf("expected go 1.23 in .deps/go.mod, got: %s", string(depsMod))
	}

	// Cleat SDK should be copied into .deps/cleat/.
	if _, err := os.Stat(filepath.Join(outDir, ".deps", "cleat", "hostcalls.go")); os.IsNotExist(err) {
		t.Error("cleat SDK stub not copied to .deps/cleat/")
	}

	// The main go.mod should reference .deps as the replace root (not projRoot).
	mod, err := os.ReadFile(filepath.Join(outDir, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(mod), filepath.Join(outDir, ".deps")) {
		t.Errorf("expected .deps replace target in go.mod, got: %s", string(mod))
	}
}

func TestPrepareBuildDirSkipsGenFilesInSrc(t *testing.T) {
	tmpDir := t.TempDir()

	projRoot := filepath.Join(tmpDir, "project")
	if err := os.MkdirAll(projRoot, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projRoot, "go.mod"), []byte("module test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	srcDir := filepath.Join(tmpDir, "src")
	if err := os.MkdirAll(srcDir, 0755); err != nil {
		t.Fatal(err)
	}
	// Regular source file.
	if err := os.WriteFile(filepath.Join(srcDir, "workflow.go"),
		[]byte("package mypkg\n\nfunc F() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	// A gen_ file that should be skipped during source copy.
	if err := os.WriteFile(filepath.Join(srcDir, "gen_already.go"),
		[]byte("package gen\n"), 0644); err != nil {
		t.Fatal(err)
	}

	outDir := filepath.Join(tmpDir, "out")

	cfg := &BuildConfig{
		SrcDir:      srcDir,
		OutDir:      outDir,
		PkgName:     "main",
		ModulePath:  "github.com/test/module",
		ProjectRoot: projRoot,
		GoVersion:   "1.26",
		Outputs: &OutputFiles{
			Imports: "// imports\n",
			Memory:  "// memory\n",
			Adapter: "// adapter\n",
			Exports: "// exports\n",
		},
	}

	if err := PrepareBuildDir(cfg); err != nil {
		t.Fatalf("PrepareBuildDir: %v", err)
	}

	// workflow.go should be present and rewritten.
	content, err := os.ReadFile(filepath.Join(outDir, "workflow.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "package main") {
		t.Error("workflow.go not rewritten to package main")
	}

	// gen_already.go should NOT have been copied from source.
	if _, err := os.Stat(filepath.Join(outDir, "gen_already.go")); !os.IsNotExist(err) {
		t.Error("gen_already.go should not have been copied from src (gen_ prefix)")
	}
}

func TestPrepareBuildDir_NoSourceFiles(t *testing.T) {
	// An empty (or non-existent) SrcDir with no Go files should still
	// succeed — the generated files and go.mod are always written.
	tmpDir := t.TempDir()
	projRoot := filepath.Join(tmpDir, "project")
	if err := os.MkdirAll(projRoot, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projRoot, "go.mod"), []byte("module test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	outDir := filepath.Join(tmpDir, "out")

	cfg := &BuildConfig{
		SrcDir:      filepath.Join(tmpDir, "emptysrc"),
		OutDir:      outDir,
		PkgName:     "main",
		ModulePath:  "github.com/test/module",
		ProjectRoot: projRoot,
		GoVersion:   "1.26",
		Outputs: &OutputFiles{
			Imports: "// imports\n",
			Memory:  "// memory\n",
			Adapter: "// adapter\n",
			Exports: "// exports\n",
		},
	}
	if err := PrepareBuildDir(cfg); err != nil {
		t.Fatalf("PrepareBuildDir: %v", err)
	}
	// Generated files should still be written.
	for _, name := range []string{"gen_wasm_imports.go", "gen_main_stub.go", "go.mod"} {
		if _, err := os.Stat(filepath.Join(outDir, name)); os.IsNotExist(err) {
			t.Errorf("expected %s to exist", name)
		}
	}
}

func TestPrepareBuildDirTinygoWithCleattest(t *testing.T) {
	tmpDir := t.TempDir()

	projRoot := filepath.Join(tmpDir, "project")
	if err := os.MkdirAll(projRoot, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projRoot, "go.mod"), []byte("module test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Create cleat SDK with a cleattest subdirectory.
	cleatDir := filepath.Join(projRoot, "cleat")
	if err := os.MkdirAll(cleatDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cleatDir, "hostcalls.go"),
		[]byte("package cleat\n\n// stub\n"), 0644); err != nil {
		t.Fatal(err)
	}

	cleattestDir := filepath.Join(cleatDir, "cleattest")
	if err := os.MkdirAll(cleattestDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cleattestDir, "testutil.go"),
		[]byte("package cleattest\n\n// stub\n"), 0644); err != nil {
		t.Fatal(err)
	}

	srcDir := filepath.Join(tmpDir, "src")
	if err := os.MkdirAll(srcDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "wf.go"),
		[]byte("package mypkg\n\nfunc Run() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}

	outDir := filepath.Join(tmpDir, "out")

	cfg := &BuildConfig{
		SrcDir:      srcDir,
		OutDir:      outDir,
		PkgName:     "main",
		ModulePath:  "github.com/test/module",
		ProjectRoot: projRoot,
		GoVersion:   "1.26",
		Target:      "tinygo",
		Outputs: &OutputFiles{
			Imports: "// imports\n",
			Memory:  "// memory\n",
			Adapter: "// adapter\n",
			Exports: "// exports\n",
		},
	}

	if err := PrepareBuildDir(cfg); err != nil {
		t.Fatalf("PrepareBuildDir: %v", err)
	}

	// Cleattest SDK should be copied into .deps/cleattest/.
	if _, err := os.Stat(filepath.Join(outDir, ".deps", "cleattest", "testutil.go")); os.IsNotExist(err) {
		t.Error("cleattest stub not copied to .deps/cleattest/")
	}
}
