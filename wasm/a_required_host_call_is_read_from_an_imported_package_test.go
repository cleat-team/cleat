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
// WHY THIS READS THE DIRECTIVE BY ITSELF. Until cleat#3803 the build learned what an SDK helper needs from a
// hand list of "Type.Method" names that refused any receiver outside a package named "cleat", so dagrun's needs
// could only be declared by this directive. The build now also reads the bodies of the functions a workflow calls
// (analyzer.DepHostCalls), which wires the same imports for a workflow that calls DAG.Execute. That made the
// assertion below pass with the directive deleted, so it asks collectRequirements alone.
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

	// The directive ALONE. Since cleat#3803 the build also reads the bodies of the functions this workflow
	// calls, so AnalyzeUsage wires these imports with or without the directive -- removing the directive left
	// this test green when it was measured. The directive is still a supported route (a library names what it
	// needs whether or not the body scan can attribute the call), so it is read here by itself.
	fromDirective := &UsageInfo{Used: map[string]bool{}, Children: map[string]bool{}}
	collectRequirements(result, fromDirective)

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
		if fromDirective.Used[imp] {
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
