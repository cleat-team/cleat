package prometheus

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

// TestWorkflowDurationCoversADurableWorkflow and
// TestNoMetricRecordsAFreeTextErrorLabel are the two halves of cleat#1309.
//
// Neither can be a behavioural test. A histogram's bucket boundaries are not
// readable back from the OTel API, and "this label would have been unbounded"
// is a statement about values that never arrive in a test. So both read the
// source -- with a parse, not a grep, because the file's own comments now
// discuss `attribute.String("error", ...)` at length and a text search cannot
// tell a call from a paragraph explaining why the call is absent.
func metricsAST(t *testing.T) (*token.FileSet, *ast.File) {
	t.Helper()
	fset := token.NewFileSet()
	src, err := os.ReadFile("metrics.go")
	if err != nil {
		t.Fatalf("reading metrics.go: %v", err)
	}
	f, err := parser.ParseFile(fset, "metrics.go", src, parser.ParseComments)
	if err != nil {
		t.Fatalf("parsing metrics.go: %v", err)
	}
	return fset, f
}

// A durable workflow can run for days. The top finite bucket has to be past
// that, or p95 saturates and reports the bucket edge forever -- a number that
// looks like a measurement and is not.
func TestWorkflowDurationCoversADurableWorkflow(t *testing.T) {
	const oneDay = 86400.0

	_, f := metricsAST(t)

	var found bool
	var top float64
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Float64Histogram" {
			return true
		}
		// Is this the workflow-duration histogram?
		lit, ok := call.Args[0].(*ast.BasicLit)
		if !ok || !strings.Contains(lit.Value, "cleat_workflow_duration_seconds") {
			return true
		}
		found = true
		for _, arg := range call.Args[1:] {
			inner, ok := arg.(*ast.CallExpr)
			if !ok {
				continue
			}
			isel, ok := inner.Fun.(*ast.SelectorExpr)
			if !ok || isel.Sel.Name != "WithExplicitBucketBoundaries" {
				continue
			}
			for _, b := range inner.Args {
				bl, ok := b.(*ast.BasicLit)
				if !ok {
					continue
				}
				v, err := strconv.ParseFloat(bl.Value, 64)
				if err != nil {
					t.Fatalf("bucket boundary %q is not a number", bl.Value)
				}
				if v > top {
					top = v
				}
			}
		}
		return false
	})

	// Vacuity: a scan that did not find the histogram passes for the same
	// reason a correct one does.
	if !found {
		t.Fatal("did not find the cleat_workflow_duration_seconds histogram in metrics.go -- " +
			"this scanned nothing, it did not pass")
	}
	if top == 0 {
		t.Fatal("found the histogram but parsed no bucket boundaries from it")
	}
	if top < oneDay {
		t.Errorf("the highest bucket boundary is %.0fs; a durable workflow can run for days, "+
			"so every run longer than that lands in +Inf and p95/p99 saturate at %.0f and "+
			"report it forever. Want at least %.0fs (cleat#1309).", top, top, oneDay)
	}
}

// An arbitrary error string as a label value is unbounded cardinality. This is
// the specific shape cleat#1309 removed, and the reason it is a guard rather
// than a comment is that the signature invited it once already.
func TestNoMetricRecordsAFreeTextErrorLabel(t *testing.T) {
	fset, f := metricsAST(t)

	var offenders []string
	var stringLabels int
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) != 2 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "String" {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok || pkg.Name != "attribute" {
			return true
		}
		stringLabels++
		key, ok := call.Args[0].(*ast.BasicLit)
		if !ok {
			return true
		}
		name, _ := strconv.Unquote(key.Value)
		if name == "error" || name == "error_message" || name == "message" {
			offenders = append(offenders,
				fset.Position(call.Pos()).String()+": attribute.String("+key.Value+", ...)")
		}
		return true
	})

	// Vacuity: metrics.go is full of attribute.String calls. Finding none means
	// the matcher is broken, not that the file is clean.
	if stringLabels == 0 {
		t.Fatal("parsed no attribute.String label calls at all -- the matcher is broken, " +
			"which is not the same as there being no free-text label")
	}
	t.Logf("examined %d attribute.String label calls", stringLabels)

	if len(offenders) > 0 {
		t.Errorf("a free-text error label is unbounded cardinality -- error strings carry run "+
			"ids, payload fragments and timestamps, and a divergence error embeds up to two 4 KB "+
			"payload snapshots. Use a bounded classification (an error code or a closed enum) "+
			"or no label. cleat#1309.\n  %s", strings.Join(offenders, "\n  "))
	}
}
