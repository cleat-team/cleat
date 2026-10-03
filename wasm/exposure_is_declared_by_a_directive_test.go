package wasm

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"testing"

	"github.com/cleat-team/cleat/internal/analyzer"
	"github.com/cleat-team/cleat/internal/callgraph"
	"github.com/cleat-team/cleat/internal/closure"
)

// The source-level exposure declaration, cleat#1986 slice 2c.
//
// The class is a property of a DEFINITION, so cleat#1986 puts it in the
// workflow's source rather than in adjacent config. On the Go path "in source"
// means a //cleat:exposure directive, because the analyzer classifies entry
// points from exportedness and never reads a comment (analyzer.IsEntryPoint) --
// so an attribute written onto the entry point would compile, stamp nothing,
// and be silently ignored, which is the failure cleat#1617 records for the
// sibling //cleat:require directive.
//
// THESE TESTS ARE SPLIT BY WHAT THEY CAN SEE, which is why there are five. The
// first two drive the real pipeline over real packages, so they are the ones
// that fail if the extraction stops being WIRED INTO AnalyzeUsage. The three
// that follow build an analyzer.AnalysisResult in memory and call
// collectExposure directly, so they can cover what no fixture can carry without
// meaning something it does not: an imported package that declares, prose that
// mentions the directive, and two declarations that disagree.

// TestABuiltFixtureDeclaresItsExposureFromSource is the integration half: a real
// package, loaded and analyzed the way `cleat build` does it, carrying the
// directive.
//
// testdata/exposure declares `internal` rather than `auth` deliberately. `auth`
// is what a workflow gets with no declaration at all, so a fixture declaring it
// would pass both with the directive read and with it silently ignored.
func TestABuiltFixtureDeclaresItsExposureFromSource(t *testing.T) {
	usage := analyzeFixture(t, "github.com/cleat-team/cleat/testdata/exposure")

	if usage.Exposure != "internal" {
		t.Errorf("a package whose source declares `//cleat:exposure internal` yielded Exposure=%q, want %q.\n\n"+
			"Either the directive was not read from the fixture's comments, or collectExposure is no "+
			"longer called from AnalyzeUsage -- check the wiring beside collectRequirements, which is "+
			"where a deletion would land.",
			usage.Exposure, "internal")
	}
	if usage.ExposureConflict != "" {
		t.Errorf("the fixture declares one class but ExposureConflict=%q", usage.ExposureConflict)
	}
}

// TestAnUndeclaredExposureIsEmptyNotAuth is the control, and it is the one that
// distinguishes "read nothing" from "read the right thing".
//
// Without it, an extraction that returned `internal` for every package would
// pass the test above. The distinction is load-bearing at the far end of the
// feature: empty means the source expressed no opinion, which the deploy path
// lets stand, where a DECLARED class can only be tightened. Substituting the
// `auth` default here would make every workflow's class look source-declared.
func TestAnUndeclaredExposureIsEmptyNotAuth(t *testing.T) {
	usage := analyzeFixture(t, "github.com/cleat-team/cleat/testdata/hello")

	if usage.Exposure != "" {
		t.Errorf("testdata/hello declares no exposure and yielded Exposure=%q, want empty.\n"+
			"An extraction that cannot report an absence cannot distinguish a declaration from a "+
			"default.", usage.Exposure)
	}
	if usage.ExposureConflict != "" {
		t.Errorf("a package with no directives reported a conflict: %q", usage.ExposureConflict)
	}
}

// TestAnImportedPackagesExposureIsNotInherited pins the one place this directive
// deliberately differs from //cleat:require, because the two differ for a reason
// a reader would otherwise "fix".
//
// //cleat:require is transitive: the need is physical, a library makes the call
// on the caller's behalf, so the compiled artifact needs the import whoever
// named it. Exposure is not: it is a statement about the DEFINITION being
// deployed, and only the definition's own source can make it. Inheriting here
// would let a dependency choose its importers' class, and the direction that
// goes wrong is FAIL-OPEN -- a library declaring `public` would loosen every
// workflow that imports it, silently, in the field this feature exists to
// protect.
//
// The imported package really does declare, so `want empty` below is not the
// vacuous absence of a directive from the whole tree.
func TestAnImportedPackagesExposureIsNotInherited(t *testing.T) {
	result := analysisResult(t,
		map[string]string{
			"target.go": "package target\n\nfunc Handle(h int) {}\n",
		},
		map[string]string{
			"dep.go": "//cleat:exposure public\npackage dep\n",
		},
	)

	info := &UsageInfo{}
	collectExposure(result, info)

	if info.Exposure != "" {
		t.Errorf("a package that declares nothing inherited %q from a package it imports, want empty.\n"+
			"An exposure read from ImportedPkgs lets any dependency choose its importers' class; a "+
			"dependency declaring `public` would then loosen every workflow that imports it. See "+
			"collectExposure -- this is the one place it deliberately differs from collectRequirements.",
			info.Exposure)
	}
	if info.ExposureConflict != "" {
		t.Errorf("nothing was declared in the target package, yet a conflict was reported: %q", info.ExposureConflict)
	}
}

// TestTwoDifferentExposureDeclarationsAreAConflict pins the refusal rather than
// a merge rule.
//
// A single-valued declaration has no correct merge. //cleat:require can union
// because a union's worst case is one unnecessary import; picking a class can
// pick the looser of the two, which is a silent fail-open decided by file order.
// So both values are reported and the build refuses, rather than one winning.
//
// A REPEATED IDENTICAL declaration is the other half, and it is not a conflict:
// two files may both state the class the package has. That case is asserted too,
// because a check that refused any second directive would pass the first
// assertion while rejecting something legitimate.
func TestTwoDifferentExposureDeclarationsAreAConflict(t *testing.T) {
	t.Run("different classes conflict", func(t *testing.T) {
		result := analysisResult(t,
			map[string]string{
				"a.go": "//cleat:exposure internal\npackage target\n",
				"b.go": "//cleat:exposure auth\npackage target\n",
			},
			nil,
		)
		info := &UsageInfo{}
		collectExposure(result, info)

		if info.ExposureConflict != "auth, internal" {
			t.Errorf("ExposureConflict=%q, want %q (both values, sorted, so the message names what "+
				"to remove)", info.ExposureConflict, "auth, internal")
		}
		// Exposure must not carry a winner alongside the conflict: a caller
		// that read only Exposure would stamp the class that happened to be
		// seen last, which is the outcome this refusal exists to prevent.
		if info.Exposure != "" {
			t.Errorf("a conflicting package also reports Exposure=%q; the build path reads both fields, "+
				"and the value must not survive the conflict", info.Exposure)
		}
	})

	t.Run("the same class twice does not", func(t *testing.T) {
		result := analysisResult(t,
			map[string]string{
				"a.go": "//cleat:exposure internal\npackage target\n",
				"b.go": "//cleat:exposure internal\npackage target\n",
			},
			nil,
		)
		info := &UsageInfo{}
		collectExposure(result, info)

		if info.ExposureConflict != "" {
			t.Errorf("the same class declared twice reported a conflict: %q. Two files stating the class "+
				"the package has is not a disagreement.", info.ExposureConflict)
		}
		if info.Exposure != "internal" {
			t.Errorf("Exposure=%q, want %q", info.Exposure, "internal")
		}
	})
}

// TestProseThatMentionsTheDirectiveIsNotADeclaration is the loose reading of the
// same source, and it exists because a text search cannot tell a thing from a
// sentence about the thing.
//
// The first case is essentially the sentence in testdata/exposure's own doc
// comment -- prose a careful author writes in the very file that declares
// nothing. The prefix must START the comment, so a mention inside a sentence, a
// value-less directive, and a block comment are all not declarations.
func TestProseThatMentionsTheDirectiveIsNotADeclaration(t *testing.T) {
	// Built by concatenation rather than as one raw string so that the
	// whitespace-only value below is real content and not trailing whitespace on
	// a physical source line, which a linter would strip or refuse.
	src := "package target\n" +
		"\n" +
		"// The //cleat:exposure directive, if present, is read by the build.\n" +
		"//\n" +
		"// To declare one, write the directive on its own line with a value:\n" +
		"//cleat:exposure\n" +
		"//\n" +
		"// (the line above declares nothing -- a value is required)\n" +
		"//\n" +
		"//cleat:exposure   \n" +
		"//\n" +
		"/* //cleat:exposure public */\n" +
		"func Handle() {}\n"

	result := analysisResult(t, map[string]string{"target.go": src}, nil)

	info := &UsageInfo{}
	collectExposure(result, info)

	if info.Exposure != "" {
		t.Errorf("prose mentioning the directive yielded Exposure=%q, want empty.\n"+
			"Only a comment whose text STARTS with `//cleat:exposure ` followed by a value is a "+
			"declaration; anything else is a sentence about one.", info.Exposure)
	}
	if info.ExposureConflict != "" {
		t.Errorf("no directive was declared, yet a conflict was reported: %q", info.ExposureConflict)
	}
}

// analyzeFixture loads a real testdata package and runs the real usage pass over
// it, so a failure here is about the wiring and not about the extraction rules.
func analyzeFixture(t *testing.T, pkg string) *UsageInfo {
	t.Helper()

	fset := token.NewFileSet()
	result, err := analyzer.LoadPackages(pkg, fset)
	if err != nil {
		t.Fatalf("LoadPackages(%s): %v", pkg, err)
	}
	cg, err := callgraph.Build(result)
	if err != nil {
		t.Fatalf("callgraph.Build(%s): %v", pkg, err)
	}
	if result.TargetPkg == nil {
		t.Fatalf("LoadPackages(%s) retained no target package", pkg)
	}
	return AnalyzeUsage(result, closure.Compute(result, cg))
}

// analysisResult builds the smallest analyzer.AnalysisResult collectExposure
// reads: a target package and, optionally, imported ones.
//
// The sources are PARSED rather than written as ast.File literals, so the test
// exercises the comment shapes the loader really produces -- a hand-built
// ast.File would be a claim about what the parser returns. ParseComments is
// required: a file parsed without it carries no Comments at all, and every case
// here would then report the same empty result and pass by measuring nothing.
func analysisResult(t *testing.T, target map[string]string, imported map[string]string) *analyzer.AnalysisResult {
	t.Helper()
	fset := token.NewFileSet()

	parse := func(name, src string) *ast.File {
		f, err := parser.ParseFile(fset, name, src, parser.ParseComments)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		return f
	}

	// Files are scanned in a sorted order because a map ranges randomly. The
	// value case takes the LAST declaration seen, so an unsorted walk would make
	// it nondeterministic -- the exact property the conflict rule exists to stop
	// depending on.
	sortedNames := func(m map[string]string) []string {
		names := make([]string, 0, len(m))
		for name := range m {
			names = append(names, name)
		}
		sort.Strings(names)
		return names
	}

	res := &analyzer.AnalysisResult{TargetPkg: &analyzer.Package{Path: "target"}}
	for _, name := range sortedNames(target) {
		res.TargetPkg.Files = append(res.TargetPkg.Files, parse(name, target[name]))
	}
	for _, name := range sortedNames(imported) {
		res.ImportedPkgs = append(res.ImportedPkgs, &analyzer.Package{
			Path:  "dep",
			Files: []*ast.File{parse(name, imported[name])},
		})
	}
	return res
}
