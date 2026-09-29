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

// TestPrepareBuildDirAllowsANonSDKRootPackageWhenTheWorkflowIsTheRootModule is
// cleat#2672: the test above exercises the root-module-workflow case only
// through an SDK import, and SDKModulePath is a strictly longer prefix match
// than RootModulePath -- so owningModule always resolves it to the SDK
// candidate, and isReplaced(RootModulePath) is never the deciding clause.
// That leaves it possible to mutate away "RootModulePath counts as replaced
// when rootReplaced" without any test noticing, because nothing exercises a
// workflow in the root module importing a genuine ROOT package that is NOT
// the SDK (e.g. something like this repo's own engine/). Measured by
// cleat-review's mutation pass on cleat#2667's rewrite: removing that clause
// from isReplaced left every existing test green.
//
// This pins the missing case directly: a root-module workflow importing a
// non-SDK root package, which can only be satisfied by the SAME unconditional
// `replace RootModulePath => filepath.Dir(sdkDir)` the SDK test's scenario
// also emits -- so this is fail-closed if it regresses (a loud refusal), not
// a live defect, exactly as cleat-review characterized it.
func TestPrepareBuildDirAllowsANonSDKRootPackageWhenTheWorkflowIsTheRootModule(t *testing.T) {
	tmpDir := t.TempDir()

	// Same shape as TestPrepareBuildDirAllowsTheSDKImportWhenTheWorkflowIsThe
	// RootModule: the workflow lives directly at its module's root, and that
	// module IS RootModulePath.
	projRoot := filepath.Join(tmpDir, "project")
	if err := os.MkdirAll(projRoot, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projRoot, "go.mod"),
		[]byte("module "+RootModulePath+"\n\ngo 1.26\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// A local SDK checkout at <projRoot>/cleat, which is what makes sdkDir
	// non-empty and rootReplaced true -- required for RootModulePath's own
	// replace to be emitted at all, independent of what is imported.
	cleatDir := filepath.Join(projRoot, "cleat")
	if err := os.MkdirAll(cleatDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cleatDir, "go.mod"),
		[]byte("module "+SDKModulePath+"\n\ngo 1.26\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// A genuine ROOT-module package that is NOT the SDK -- a stand-in for
	// something like this repo's own engine/. It is a real subdirectory of
	// projRoot, the root module's own tree, and unstaged (staging is a flat
	// glob of SrcDir's own *.go files) -- reachable only via the root
	// module's own replace, never by staging.
	engineDir := filepath.Join(projRoot, "engine")
	if err := os.MkdirAll(engineDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(engineDir, "engine.go"),
		[]byte("package engine\n\nfunc Something() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}

	rootPkgImport := RootModulePath + "/engine"
	if err := os.WriteFile(filepath.Join(projRoot, "workflow.go"),
		[]byte("package mypkg\n\nimport \""+rootPkgImport+"\"\n\nfunc Run() { engine.Something() }\n"),
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
		t.Fatalf("PrepareBuildDir refused a root-module workflow's own non-SDK root package "+
			"import (%q): %v", rootPkgImport, err)
	}
}

// TestPrepareBuildDirRefusesTheInRepoNestedModuleIncident reproduces
// cleat#2657/#2658 at the shape it actually occurs in: a workflow inside a
// NESTED module (its own go.mod, like examples/) importing a subpackage of
// its own directory, built with a real local SDK checkout findable from
// ProjectRoot -- the one combination TestPrepareBuildDirRefusesAnUnstaged
// SubpackageImport does not cover, because that test's synthetic module has
// no SDK checkout anywhere in its ancestry and so never sets rootReplaced.
//
// cleat-review measured that the shipped fix's first version did not catch
// this: rootReplaced is unconditionally true for any workflow built from
// inside this repository (sdkReplaceDir walks all the way up to the real
// cleat/ checkout), and the first version's alwaysLocal exclusion treated
// every import under RootModulePath as resolved by the root replace --
// including one whose actual owning module is a DIFFERENT, nested go.mod
// that merely shares RootModulePath as a string prefix. This test pins that
// combination directly rather than trusting the in-repo-only reproduction
// that first caught it.
func TestPrepareBuildDirRefusesTheInRepoNestedModuleIncident(t *testing.T) {
	tmpDir := t.TempDir()

	repoRoot := filepath.Join(tmpDir, "repo")
	if err := os.MkdirAll(repoRoot, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, "go.mod"),
		[]byte("module "+RootModulePath+"\n\ngo 1.26\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// A local SDK checkout at <repoRoot>/cleat, exactly as in the real repo --
	// this is what makes sdkReplaceDir find something and rootReplaced true.
	cleatDir := filepath.Join(repoRoot, "cleat")
	if err := os.MkdirAll(cleatDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cleatDir, "go.mod"),
		[]byte("module "+SDKModulePath+"\n\ngo 1.26\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// examples/, a NESTED module -- its own go.mod, exactly as in the real
	// repo -- with a workflow that imports its own unstaged subpackage.
	examplesRoot := filepath.Join(repoRoot, "examples")
	wfDir := filepath.Join(examplesRoot, "order-lifecycle")
	subDir := filepath.Join(wfDir, "emailclient")
	if err := os.MkdirAll(subDir, 0755); err != nil {
		t.Fatal(err)
	}
	examplesModule := RootModulePath + "/examples"
	if err := os.WriteFile(filepath.Join(examplesRoot, "go.mod"),
		[]byte("module "+examplesModule+"\n\ngo 1.26\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(subDir, "client.go"),
		[]byte("package emailclient\n\nfunc Send() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	wfImport := examplesModule + "/order-lifecycle/emailclient"
	if err := os.WriteFile(filepath.Join(wfDir, "workflow.go"),
		[]byte("package mypkg\n\nimport \""+wfImport+"\"\n\nfunc Run() { emailclient.Send() }\n"),
		0644); err != nil {
		t.Fatal(err)
	}

	cfg := &BuildConfig{
		SrcDir:      wfDir,
		OutDir:      filepath.Join(tmpDir, "out"),
		PkgName:     "main",
		ModulePath:  examplesModule,
		ProjectRoot: examplesRoot,
		GoVersion:   "1.26",
		Outputs:     &OutputFiles{},
		WASMOutput:  "out.wasm",
	}

	err := PrepareBuildDir(cfg)
	if err == nil {
		t.Fatal("PrepareBuildDir did not refuse the in-repo nested-module subpackage import " +
			"(cleat#2657/#2658, reproduced at the shape it actually occurs in)")
	}
	if !strings.Contains(err.Error(), wfImport) {
		t.Errorf("PrepareBuildDir error = %q, want it to name the unstageable import %q", err, wfImport)
	}
}

// TestPrepareBuildDirRefusesASiblingPackageInTheSameModule is cleat-review's
// first GAP: a package that is unstaged and unreplaced but is NOT nested
// under SrcDir -- a sibling directory elsewhere in the same module -- is
// exactly as unresolvable as a subpackage, and the fix must not be scoped to
// "under SrcDir" only.
func TestPrepareBuildDirRefusesASiblingPackageInTheSameModule(t *testing.T) {
	tmpDir := t.TempDir()

	projRoot := filepath.Join(tmpDir, "project")
	wfDir := filepath.Join(projRoot, "workflows", "order-lifecycle")
	sharedDir := filepath.Join(projRoot, "shared")
	if err := os.MkdirAll(wfDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(sharedDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projRoot, "go.mod"),
		[]byte("module example.com/myapp\n\ngo 1.26\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sharedDir, "shared.go"),
		[]byte("package shared\n\nfunc Do() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wfDir, "workflow.go"),
		[]byte("package mypkg\n\nimport \"example.com/myapp/shared\"\n\nfunc Run() { shared.Do() }\n"),
		0644); err != nil {
		t.Fatal(err)
	}

	cfg := &BuildConfig{
		SrcDir:      wfDir,
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
		t.Fatal("PrepareBuildDir did not refuse a sibling-package import in the workflow's own " +
			"module -- a sibling is exactly as unstageable as a subpackage, and a fix scoped to " +
			"\"under SrcDir\" only would miss it")
	}
}

// TestPrepareBuildDirAllowsANestedModuleWithAReplace is cleat-review's second
// GAP: a subdirectory that is its own go.mod module, with an explicit replace
// in the project's own go.mod, resolves locally via propagateReplaces and
// must NOT be refused even though it is nested under SrcDir on disk.
// The vendored module's path is deliberately a textual subpath of the
// workflow's own module (example.com/myapp/vendored, not some unrelated
// example.com/other/module): that is the only shape that exercises
// sourceModuleDirectoryReplaces at all. An import whose path shares no
// prefix with cfg.ModulePath is allowed by owningModule's longest-match rule
// regardless of what sourceModuleDirectoryReplaces returns -- it never
// matches cfg.ModulePath as the longest prefix in the first place, replace
// or no replace. Confirmed by falsification: with an unrelated module path,
// this test passed even with sourceModuleDirectoryReplaces's result forced
// to nil. Only a path that textually nests under cfg.ModulePath makes the
// owning-module computation ambiguous between cfg.ModulePath and the
// explicitly-replaced nested module, which is what the forwarded-replace set
// exists to resolve.
func TestPrepareBuildDirAllowsANestedModuleWithAReplace(t *testing.T) {
	tmpDir := t.TempDir()

	projRoot := filepath.Join(tmpDir, "project")
	wfDir := filepath.Join(projRoot, "workflows", "order-lifecycle")
	vendoredDir := filepath.Join(wfDir, "vendored")
	if err := os.MkdirAll(vendoredDir, 0755); err != nil {
		t.Fatal(err)
	}
	const vendoredModule = "example.com/myapp/vendored"
	if err := os.WriteFile(filepath.Join(vendoredDir, "go.mod"),
		[]byte("module "+vendoredModule+"\n\ngo 1.26\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(vendoredDir, "helper.go"),
		[]byte("package other\n\nfunc Do() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projRoot, "go.mod"),
		[]byte("module example.com/myapp\n\ngo 1.26\n\nreplace "+vendoredModule+" => ./workflows/order-lifecycle/vendored\n"),
		0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wfDir, "workflow.go"),
		[]byte("package mypkg\n\nimport \""+vendoredModule+"\"\n\nfunc Run() { other.Do() }\n"),
		0644); err != nil {
		t.Fatal(err)
	}

	cfg := &BuildConfig{
		SrcDir:      wfDir,
		OutDir:      filepath.Join(tmpDir, "out"),
		PkgName:     "main",
		ModulePath:  "example.com/myapp",
		ProjectRoot: projRoot,
		GoVersion:   "1.26",
		Outputs:     &OutputFiles{},
		WASMOutput:  "out.wasm",
	}

	if err := PrepareBuildDir(cfg); err != nil {
		t.Fatalf("PrepareBuildDir refused a nested module with an explicit replace, which "+
			"propagateReplaces makes resolvable locally: %v", err)
	}
}
