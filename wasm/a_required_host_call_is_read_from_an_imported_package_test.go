package wasm

import (
	"go/token"
	"sort"
	"testing"

	"github.com/cleat-team/cleat/internal/analyzer"
	"github.com/cleat-team/cleat/internal/callgraph"
	"github.com/cleat-team/cleat/internal/closure"
)

// A //cleat:require directive in an IMPORTED package reaches the build.
// cleat#1617.
//
// WHY THIS IS THE CASE THE DIRECTIVE EXISTS FOR, AND WAS THE ONE IT MISSED.
// The directive is for a library that makes host calls on the caller's behalf:
// the workflow's own source never names the call, so no amount of scanning the
// workflow finds it. Such a library is, by definition, an IMPORTED package --
// and collectRequirements read result.TargetPkg.Files alone. So the mechanism
// was ignored in precisely the situation it was built for, and the failure is
// silent and late: the module builds, deploys, and dies on its first call with
// "the HostCalls runtime was not initialized".
//
// cleat/dagrun/dagrun.go:28 carries such a directive, correct about what
// dagrun needs, written by someone who expected it to work.
//
// WHY THE TABLE ROUTE CANNOT COVER THIS, so that nobody "simplifies" this test
// away by adding a row to sdkHelperImports. SDKDurableHelper
// (internal/analyzer/types.go) refuses any receiver whose package is not named
// "cleat", and dagrun's package is named dagrun -- so a row for DAG.Execute
// would be written and never consulted. That is also why dagrun is in
// sdkHelperScanSkipDirs, and why it stays there: the table is not how dagrun
// declares its needs.
func TestARequiredHostCallIsReadFromAnImportedPackage(t *testing.T) {
	const pkg = "github.com/cleat-team/cleat/testdata/dagguest"

	fset := token.NewFileSet()
	result, err := analyzer.LoadPackages(pkg, fset)
	if err != nil {
		t.Fatalf("LoadPackages(%s): %v", pkg, err)
	}
	cg, err := callgraph.Build(result)
	if err != nil {
		t.Fatalf("callgraph.Build: %v", err)
	}
	usage := AnalyzeUsage(result, closure.Compute(result, cg))

	var got []string
	for imp := range usage.Used {
		got = append(got, imp)
	}
	sort.Strings(got)

	// The control, and it is load-bearing. The fixture calls h.NowMs() itself,
	// so cleat_now is wired by the ordinary scan of the workflow's own source.
	// If it is missing, the build is broken for some reason that has nothing
	// to do with this issue, and the two assertions below would fail for that
	// reason instead -- reporting a directive bug that is not there.
	if !usage.Used["cleat_now"] {
		t.Fatalf("the fixture's own h.NowMs() did not wire cleat_now, so this run says "+
			"nothing about imported-package directives.\n  imports wired: %v", got)
	}

	for _, imp := range []string{"cleat_child_workflow_with_options", "cleat_await_any_child"} {
		if usage.Used[imp] {
			continue
		}
		t.Errorf("a workflow whose only host-call route is cleat/dagrun does not wire %s.\n"+
			"  imports wired: %v\n\n"+
			"cleat/dagrun/dagrun.go declares this with //cleat:require. If that directive is "+
			"being read only from the workflow's own package again, this is cleat#1617 exactly: "+
			"the module builds, `cleat build` exits 0 with no warning, and the workflow fails on "+
			"its first task with \"durable: ChildWorkflow can only be called from within a "+
			"workflow function (the HostCalls runtime was not initialized)\".", imp, got)
	}
}

// The directive is read from an imported package because the loader RETAINS
// imported packages. This is the mechanism under the test above, asserted on
// its own so that a failure says which half broke.
//
// Without it, a loader change that dropped ImportedPkgs would show up only as
// the dagrun test failing, which reads as a directive bug and sends the reader
// to wasm/usage.go -- the wrong file.
func TestTheLoaderRetainsImportedPackages(t *testing.T) {
	const pkg = "github.com/cleat-team/cleat/testdata/dagguest"

	fset := token.NewFileSet()
	result, err := analyzer.LoadPackages(pkg, fset)
	if err != nil {
		t.Fatalf("LoadPackages(%s): %v", pkg, err)
	}

	if len(result.ImportedPkgs) == 0 {
		t.Fatalf("LoadPackages retained no imported packages, so no directive outside the "+
			"workflow's own package can ever be read (cleat#1617).\n"+
			"  target: %s", result.TargetPkg.Path)
	}

	var found *analyzer.Package
	for _, p := range result.ImportedPkgs {
		if p.Path == "github.com/cleat-team/cleat/cleat/dagrun" {
			found = p
			break
		}
	}
	if found == nil {
		var paths []string
		for _, p := range result.ImportedPkgs {
			paths = append(paths, p.Path)
		}
		sort.Strings(paths)
		t.Fatalf("cleat/dagrun is not among the %d retained imported packages, though the "+
			"fixture imports it.\n  retained: %v", len(result.ImportedPkgs), paths)
	}

	// Files, not just the path: collectRequirements walks Files, and a Package
	// retained with none is indistinguishable from one that is absent, except
	// that it looks present.
	if len(found.Files) == 0 {
		t.Errorf("cleat/dagrun was retained with no syntax files, so its //cleat:require " +
			"directive cannot be read. The loader needs packages.NeedSyntax|NeedDeps.")
	}

	// The target package must NOT also appear in ImportedPkgs -- it is scanned
	// separately, and a duplicate would make a single directive count twice.
	// Harmless today because Used is a set, and worth pinning before that
	// stops being true.
	for _, p := range result.ImportedPkgs {
		if p.Path == result.TargetPkg.Path {
			t.Errorf("the target package %s also appears in ImportedPkgs; it is scanned "+
				"separately and must not be duplicated", p.Path)
		}
	}
}
