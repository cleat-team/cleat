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

// TestFindRepoRootSkipsNestedModules is the regression test for the defect the
// module split exposed in the Python build path.
//
// FindRepoRoot returned the NEAREST go.mod. Once tests/plugin-harness and
// examples/ became their own modules, a Python workflow under
// tests/plugin-harness/testdata/ resolved "the repo root" to
// tests/plugin-harness, and BuildPythonWasmWithRuntime looked for
// python-sdk/scripts/build_wasm.py underneath it. It is not there, so the build
// failed -- and TestPluginCalls_Wasm_Python reported that failure as a SKIP,
// which is why the only thing that caught it was that job's skip budget of 0.
func TestFindRepoRootSkipsNestedModules(t *testing.T) {
	tmpDir := t.TempDir()

	// A repo root declaring the root module, with a nested module two levels
	// down -- the shape of tests/plugin-harness/testdata/pythonworkflow.
	if err := os.WriteFile(filepath.Join(tmpDir, "go.mod"),
		[]byte("module "+RootModulePath+"\n\ngo 1.25\n"), 0644); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(tmpDir, "tests", "harness")
	deep := filepath.Join(nested, "testdata", "wf")
	if err := os.MkdirAll(deep, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "go.mod"),
		[]byte("module "+RootModulePath+"/tests/harness\n\ngo 1.25\n"), 0644); err != nil {
		t.Fatal(err)
	}

	got, err := FindRepoRoot(deep)
	if err != nil {
		t.Fatalf("FindRepoRoot: %v", err)
	}
	want, err := filepath.EvalSymlinks(tmpDir)
	if err != nil {
		want = tmpDir
	}
	gotResolved, err := filepath.EvalSymlinks(got)
	if err != nil {
		gotResolved = got
	}
	if gotResolved != want {
		t.Errorf("FindRepoRoot(%s) = %s, want %s -- it stopped at the nested module, "+
			"so every repo-relative asset (python-sdk/, migrations/) resolves one level "+
			"too shallow", deep, gotResolved, want)
	}
}

// TestFindRepoRootFallsBackOutsideThisRepo pins the other half: in a tree that
// is not this repository there is no root module to find, and the nearest
// go.mod is the only answer available. That is what an external user's project
// looks like, and it must keep working.
func TestFindRepoRootFallsBackOutsideThisRepo(t *testing.T) {
	tmpDir := t.TempDir()
	proj := filepath.Join(tmpDir, "myapp")
	sub := filepath.Join(proj, "internal", "wf")
	if err := os.MkdirAll(sub, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proj, "go.mod"),
		[]byte("module example.com/myapp\n\ngo 1.25\n"), 0644); err != nil {
		t.Fatal(err)
	}
	got, err := FindRepoRoot(sub)
	if err != nil {
		t.Fatalf("FindRepoRoot: %v", err)
	}
	want, _ := filepath.EvalSymlinks(proj)
	gotResolved, _ := filepath.EvalSymlinks(got)
	if gotResolved != want {
		t.Errorf("FindRepoRoot(%s) = %s, want %s", sub, gotResolved, want)
	}
}

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
	// The require names the SDK, at its real import path -- NOT the enclosing
	// module's path with "/cleat" appended, which is what this used to assert.
	// cfg.ModulePath here is "github.com/test/module", and the old code emitted
	// `require github.com/test/module/cleat v0.0.0`, a module that does not
	// exist. That was invisible in this repo, where the enclosing module really
	// is github.com/cleat-team/cleat, and broke every project `cleat init`
	// scaffolds. See SDKModulePath.
	if !strings.Contains(string(mod), SDKModulePath) {
		t.Errorf("expected a require of %s in go.mod, got: %s", SDKModulePath, string(mod))
	}
	if strings.Contains(string(mod), "github.com/test/module/cleat") {
		t.Errorf("go.mod derives the SDK path from the enclosing module again; "+
			"that names a module that does not exist for any project but this one: %s", string(mod))
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
			"handler.go":  []byte("package mypkg\n\nfunc Handle() {}\n"),
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

func TestPrepareBuildDirGoTarget(t *testing.T) {
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
		Target:      "go",
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

	// Main stub should use the sleep-loop pattern (not channel block or select{}).
	stub, err := os.ReadFile(filepath.Join(outDir, "gen_main_stub.go"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(stub), "<-make(chan struct{})") {
		t.Errorf("go target stub should not use channel block, got: %s", string(stub))
	}
	if !strings.Contains(string(stub), "cleatPollWork") {
		t.Errorf("expected cleatPollWork in go target stub, got: %s", string(stub))
	}

	// go.mod should use the real Go version (not capped to 1.23).
	mod, err := os.ReadFile(filepath.Join(outDir, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(mod), "go 1.26") {
		t.Errorf("expected go 1.26 in go.mod, got: %s", string(mod))
	}

	// No .deps/ directory should be created for the "go" target.
	if _, err := os.Stat(filepath.Join(outDir, ".deps")); !os.IsNotExist(err) {
		t.Error(".deps directory should not exist for go target")
	}

	// No replace directive: projRoot is a bare temp dir with no cleat/ checkout
	// inside or above it, which is the shape of an ordinary user's project. The
	// old code emitted `replace github.com/test/module/cleat => <projRoot>/cleat`
	// unconditionally, pointing at a directory that does not exist, and `go mod
	// tidy` in the build directory failed on exactly that path.
	if strings.Contains(string(mod), "replace") {
		t.Errorf("go.mod has a replace directive for a project with no local SDK "+
			"checkout; it points at a directory that does not exist: %s", string(mod))
	}
}

// TestPrepareBuildDirUsesLocalSDKCheckout is the other half: when there IS a
// cleat/ checkout at or above the project root -- which is the case for every
// workflow inside this repository, including the ones under tests/ and
// examples/ that now live in their own modules -- the build directory must be
// pointed at it, so a workflow compiles against the SDK in the tree rather than
// a published release of it.
func TestPrepareBuildDirUsesLocalSDKCheckout(t *testing.T) {
	tmpDir := t.TempDir()

	// A fake repo: <root>/cleat/go.mod declaring the SDK, with the project a
	// level below it, mirroring tests/plugin-harness or examples/.
	fakeRepo := filepath.Join(tmpDir, "repo")
	sdkDir := filepath.Join(fakeRepo, "cleat")
	if err := os.MkdirAll(sdkDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sdkDir, "go.mod"),
		[]byte("module "+SDKModulePath+"\n\ngo 1.25\n"), 0644); err != nil {
		t.Fatal(err)
	}

	projRoot := filepath.Join(fakeRepo, "nested", "project")
	srcDir := filepath.Join(projRoot, "wf")
	if err := os.MkdirAll(srcDir, 0755); err != nil {
		t.Fatal(err)
	}
	outDir := filepath.Join(tmpDir, "out")

	cfg := &BuildConfig{
		SrcDir:      srcDir,
		OutDir:      outDir,
		PkgName:     "main",
		ModulePath:  "github.com/cleat-team/cleat/nested/project",
		ProjectRoot: projRoot,
		GoVersion:   "1.26",
		Target:      "go",
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

	mod, err := os.ReadFile(filepath.Join(outDir, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	want := "replace " + SDKModulePath + " => " + sdkDir
	if !strings.Contains(string(mod), want) {
		t.Errorf("expected %q in the generated go.mod, got:\n%s", want, string(mod))
	}
}

// TestGeneratedGoModResolvesTheRootModule pins the second replace.
//
// cleat/go.mod requires the root module at v0.0.0 and resolves it with its own
// `replace => ../`, which is ignored outside that module. So a build directory
// that replaces the SDK with a local checkout must say how to resolve the root
// module too, or `go mod tidy` there fails with
//
//	reading github.com/cleat-team/cleat/go.mod at revision v0.0.0: unknown revision v0.0.0
//
// This was not hypothetical and the trigger was absurdly remote: adding an
// `import ".../engine"` to a test file in package cleat_test was enough to break
// `cleat build` for every workflow in the repository. Module graph pruning had
// been dropping that edge, so nothing depended on it being declared -- until
// `go mod tidy` needed the tests of an imported package and it did.
func TestGeneratedGoModResolvesTheRootModule(t *testing.T) {
	tmpDir := t.TempDir()

	fakeRepo := filepath.Join(tmpDir, "repo")
	sdkDir := filepath.Join(fakeRepo, "cleat")
	if err := os.MkdirAll(sdkDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sdkDir, "go.mod"),
		[]byte("module "+SDKModulePath+"\n\ngo 1.25\n"), 0644); err != nil {
		t.Fatal(err)
	}

	srcDir := filepath.Join(fakeRepo, "wf")
	if err := os.MkdirAll(srcDir, 0755); err != nil {
		t.Fatal(err)
	}
	outDir := filepath.Join(tmpDir, "out")

	cfg := &BuildConfig{
		SrcDir: srcDir, OutDir: outDir, PkgName: "main",
		ModulePath: RootModulePath, ProjectRoot: fakeRepo,
		GoVersion: "1.26", Target: "go",
		Outputs: &OutputFiles{Imports: "// i\n", Memory: "// m\n", Adapter: "// a\n", Exports: "// e\n"},
	}
	if err := PrepareBuildDir(cfg); err != nil {
		t.Fatalf("PrepareBuildDir: %v", err)
	}

	mod, err := os.ReadFile(filepath.Join(outDir, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	want := "replace " + RootModulePath + " => " + fakeRepo
	if !strings.Contains(string(mod), want) {
		t.Errorf("expected %q in the generated go.mod -- without it `go mod tidy` in a "+
			"build directory cannot resolve the root module that the SDK requires. Got:\n%s",
			want, string(mod))
	}
}

// TestSDKReplaceDirIgnoresAnUnrelatedCleatDirectory pins the reason
// sdkReplaceDir reads the go.mod rather than just stat'ing the directory. A
// directory named "cleat" in someone's project is not unusual, and replacing
// the SDK with it would fail somewhere far from the cause.
func TestSDKReplaceDirIgnoresAnUnrelatedCleatDirectory(t *testing.T) {
	tmpDir := t.TempDir()
	decoy := filepath.Join(tmpDir, "proj", "cleat")
	if err := os.MkdirAll(decoy, 0755); err != nil {
		t.Fatal(err)
	}
	// A go.mod, but not the SDK's.
	if err := os.WriteFile(filepath.Join(decoy, "go.mod"),
		[]byte("module example.com/myapp/cleat\n\ngo 1.25\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if got := sdkReplaceDir(filepath.Join(tmpDir, "proj")); got != "" {
		t.Errorf("sdkReplaceDir matched an unrelated cleat/ directory: %s", got)
	}
}

func TestPrepareBuildDirGoTargetWithCleattest(t *testing.T) {
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
		Target:      "go",
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

	// The "go" target should NOT create a .deps/ vendor directory.
	if _, err := os.Stat(filepath.Join(outDir, ".deps")); !os.IsNotExist(err) {
		t.Error(".deps directory should not exist for go target")
	}

	// Generated files should be present.
	for _, name := range []string{"gen_wasm_imports.go", "gen_host_adapter.go", "gen_main_stub.go"} {
		if _, err := os.Stat(filepath.Join(outDir, name)); os.IsNotExist(err) {
			t.Errorf("expected generated file %s was not created", name)
		}
	}
}
