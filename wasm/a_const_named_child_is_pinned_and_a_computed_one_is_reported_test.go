package wasm

import (
	"go/token"
	"sort"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/internal/analyzer"
	"github.com/cleat-team/cleat/internal/callgraph"
	"github.com/cleat-team/cleat/internal/closure"
)

// cleat#3500. The usage scan took a child's name only from a string LITERAL at
// the call site. clew's ctolap named its children through Go constants, so
// under `--channel frozen` they came out unpinned -- no entry in ChildVersions,
// MAX(version) at runtime -- while the metadata said `frozen` and the build
// exited 0.
//
// The fixture (testdata/childnames) starts children six ways. The scan must
// resolve every compile-time-constant form and REPORT, with a position, the two
// it cannot.
func TestAConstNamedChildIsPinnedAndAComputedOneIsReported(t *testing.T) {
	const pkg = "github.com/cleat-team/cleat/testdata/childnames"

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

	var resolved []string
	for name := range usage.Children {
		resolved = append(resolved, name)
	}
	sort.Strings(resolved)
	want := []string{"child-const", "child-literal", "child-prefix-expr", "child-raw"}
	if strings.Join(resolved, ",") != strings.Join(want, ",") {
		t.Errorf("resolved children = %v, want %v\n"+
			"  child-literal is the control: it resolved before cleat#3500, so a miss there means the "+
			"fixture or the scan is broken, not the const handling.", resolved, want)
	}

	if len(usage.UnresolvedChildren) != 2 {
		t.Fatalf("UnresolvedChildren = %+v, want exactly the variable and the call", usage.UnresolvedChildren)
	}
	exprs := map[string]string{}
	for _, u := range usage.UnresolvedChildren {
		exprs[u.Expr] = u.Pos
		if !strings.Contains(u.Pos, "childnames.go:") {
			t.Errorf("unresolved child %q has position %q, want a childnames.go:line", u.Expr, u.Pos)
		}
	}
	for _, e := range []string{"name", "pick(input)"} {
		if _, ok := exprs[e]; !ok {
			t.Errorf("expected %q among the unresolved children, got %v", e, exprs)
		}
	}
}
