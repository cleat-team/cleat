package wasm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// cleat#2658: PrepareBuildDir stages a workflow's source files with a flat,
// non-recursive copy (see checkStagedImportsSatisfiable's doc comment). A
// workflow that imports a subpackage of its own directory therefore has
// nothing local to resolve that import against -- before this fix, `go mod
// tidy` in the build directory would fall through to the module proxy
// instead, which fails loudly for an unpublished parent module (the incident,
// cleat#2657) but can succeed SILENTLY against a stale published copy for a
// published one. These tests are new: there was no existing red to copy, so
// each one is checked to fail without the fix (see the falsification note on
// the "refuses" test).

// TestPrepareBuildDirRefusesAnUnstagedSubpackageImport is the core case: a
// workflow whose own subdirectory is never staged, imported by exact
// subpackage import path.
//
// Falsified by commenting out this test file's call site in PrepareBuildDir
// (the `if err := checkStagedImportsSatisfiable(...)` block): PrepareBuildDir
// then returns nil, and this test fails with "PrepareBuildDir did not refuse
// the unstaged subpackage import" -- not a build failure, the assertion this
// test exists to make.
func TestPrepareBuildDirRefusesAnUnstagedSubpackageImport(t *testing.T) {
	tmpDir := t.TempDir()

	projRoot := filepath.Join(tmpDir, "project")
	srcDir := filepath.Join(projRoot, "workflows", "order-lifecycle")
	subDir := filepath.Join(srcDir, "emailclient")
	if err := os.MkdirAll(subDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projRoot, "go.mod"),
		[]byte("module example.com/myapp\n\ngo 1.26\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(subDir, "client.go"),
		[]byte("package emailclient\n\nfunc Send() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "workflow.go"),
		[]byte("package mypkg\n\nimport \"example.com/myapp/workflows/order-lifecycle/emailclient\"\n\n"+
			"func Run() { emailclient.Send() }\n"), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := &BuildConfig{
		SrcDir:      srcDir,
		OutDir:      filepath.Join(tmpDir, "out"),
		PkgName:     "main",
		ModulePath:  "example.com/myapp",
		ProjectRoot: projRoot,
		GoVersion:   "1.26",
		Outputs:     &OutputFiles{},
		WASMOutput:  "out.wasm",
	}

	err := PrepareBuildDir(cfg)
	if err == nil {
		t.Fatal("PrepareBuildDir did not refuse the unstaged subpackage import")
	}
	const wantImport = "example.com/myapp/workflows/order-lifecycle/emailclient"
	if !strings.Contains(err.Error(), wantImport) {
		t.Errorf("PrepareBuildDir error = %q, want it to name the unstageable import %q", err, wantImport)
	}
}

// TestPrepareBuildDirAllowsAnOrdinaryExternalImport is the negative control:
// an import that is genuinely external (not nested under SrcDir at all) must
// not be refused, whether or not it happens to be resolvable.
func TestPrepareBuildDirAllowsAnOrdinaryExternalImport(t *testing.T) {
	tmpDir := t.TempDir()

	projRoot := filepath.Join(tmpDir, "project")
	srcDir := filepath.Join(projRoot, "workflows", "order-lifecycle")
	if err := os.MkdirAll(srcDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projRoot, "go.mod"),
		[]byte("module example.com/myapp\n\ngo 1.26\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "workflow.go"),
		[]byte("package mypkg\n\nimport \"example.com/other/pkg\"\n\nfunc Run() { pkg.Do() }\n"), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := &BuildConfig{
		SrcDir:      srcDir,
		OutDir:      filepath.Join(tmpDir, "out"),
		PkgName:     "main",
		ModulePath:  "example.com/myapp",
		ProjectRoot: projRoot,
		GoVersion:   "1.26",
		Outputs:     &OutputFiles{},
		WASMOutput:  "out.wasm",
	}

	if err := PrepareBuildDir(cfg); err != nil {
		t.Fatalf("PrepareBuildDir refused an ordinary external import: %v", err)
	}
}

// TestPrepareBuildDirAllowsTheSDKImportWhenTheWorkflowIsTheRootModule is the
// regression test for the false positive this fix's own review caught before
// it shipped: a workflow whose enclosing module IS the root
// "github.com/cleat-team/cleat" module has an import-path prefix that is
// exactly RootModulePath, so every other top-level package of that module --
// including cleat/, the SDK -- textually looks like one of the workflow's own
// unstaged "subpackages". Both are actually resolved by the explicit
// `replace` PrepareBuildDir already emits (SDKModulePath always; RootModulePath
// whenever a local SDK checkout is found), not by staging, and must not be
// refused.
func TestPrepareBuildDirAllowsTheSDKImportWhenTheWorkflowIsTheRootModule(t *testing.T) {
	tmpDir := t.TempDir()

	// The workflow lives directly at its module's root -- SrcDir == ProjectRoot
	// -- and that module IS RootModulePath, the shape that made the old
	// SDK-replace derivation ("cfg.ModulePath + /cleat") happen to work by
	// coincidence. See PrepareBuildDir's own comment on sdkDir.
	projRoot := filepath.Join(tmpDir, "project")
	if err := os.MkdirAll(projRoot, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projRoot, "go.mod"),
		[]byte("module "+RootModulePath+"\n\ngo 1.26\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// A local SDK checkout at <projRoot>/cleat, which is what makes sdkDir
	// non-empty and RootModulePath's own replace get emitted.
	cleatDir := filepath.Join(projRoot, "cleat")
	if err := os.MkdirAll(cleatDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cleatDir, "go.mod"),
		[]byte("module "+SDKModulePath+"\n\ngo 1.26\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cleatDir, "hostcalls.go"),
		[]byte("package cleat\n\n// stub\n"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(projRoot, "workflow.go"),
		[]byte("package mypkg\n\nimport \""+SDKModulePath+"\"\n\nfunc Run() { _ = cleat.Something }\n"),
		0644); err != nil {
		t.Fatal(err)
	}

	cfg := &BuildConfig{
		SrcDir:      projRoot,
		OutDir:      filepath.Join(tmpDir, "out"),
		PkgName:     "main",
		ModulePath:  RootModulePath,
		ProjectRoot: projRoot,
		GoVersion:   "1.26",
		Outputs:     &OutputFiles{},
		WASMOutput:  "out.wasm",
	}

	if err := PrepareBuildDir(cfg); err != nil {
		t.Fatalf("PrepareBuildDir refused a root-module workflow's SDK import: %v", err)
	}
}
