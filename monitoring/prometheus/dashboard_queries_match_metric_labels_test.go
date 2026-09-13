package prometheus

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// TestDashboardGroupingLabelsExistOnTheMetric is cleat#1317's dashboard half.
//
// A Grafana panel titled "Event History Growth Rate" queried
//
//	sum(cleat_wasm_cache_bytes{...}) by (workflow_name)
//
// SetWasmCacheBytes attaches NO labels, so that `by (workflow_name)` could
// never group by anything -- while SetEventHistorySize takes a workflowName
// and attaches workflow_name, which is exactly the label the query assumes.
// The query identified the metric it was written for, and it was not the one
// it named.
//
// That is the signal this checks, and it is a better one than "does the metric
// exist": cleat_wasm_cache_bytes DOES exist, so an existence check passes the
// broken panel. The mismatch between a query's grouping label and the metric's
// declared label set is what separates them.
//
// Labels set globally (mergeAttrs' defaultAttrs -- namespace and friends) are
// attached to every metric and are not declared per-method, so they are
// allowlisted rather than looked up.
var globalDashboardLabels = map[string]bool{
	"namespace": true, "service": true, "instance": true, "job": true,
	"worker_id": true, "pod": true,

	// `le` is the histogram bucket boundary. Prometheus adds it to every
	// _bucket series; no Go code declares it, and `by (le, ...)` is the
	// REQUIRED idiom for histogram_quantile. Without this entry the guard
	// fails every correct latency panel -- which is what it did on its first
	// working run.
	"le": true,
}

// callerSuppliedLabels returns, per recording-method name, the attribute keys
// that CALLERS pass as extraAttrs.
//
// It reads the whole repository because that is where the callers are; the
// metric is declared in this package and labelled somewhere else entirely.
//
// git ls-files, NOT a filesystem walk. A walk from ../.. descends into
// .claude/worktrees/ when one exists -- a second full copy of the repo -- and
// would attribute a label to a metric from a scratch checkout. git ls-files
// answers "what is in the repo", which is the question.
//
// IT NEEDS NO VACUITY CHECK OF ITS OWN, unusually, because the test is
// self-checking here: knownGroupingMismatches no longer exempts
// cleat_call_retries_total|operation, so if this scan returns nothing the
// dashboard test FAILS rather than passing blind. A silent breakage in this
// function is a red run, not a green one.
func callerSuppliedLabels(t *testing.T) map[string][]string {
	t.Helper()
	cmd := exec.Command("git", "ls-files", "*.go")
	cmd.Dir = "../.."
	outBytes, err := cmd.Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}

	out := map[string][]string{}
	for _, rel := range strings.Fields(string(outBytes)) {
		if strings.HasSuffix(rel, "_test.go") {
			continue
		}
		path := filepath.Join("../..", rel)
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			continue // not every tracked .go file has to parse standalone
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || !strings.HasPrefix(sel.Sel.Name, "Record") && !strings.HasPrefix(sel.Sel.Name, "Set") {
				return true
			}
			// The AST spans line continuations, which a line-oriented grep does
			// not: the RecordCallRetry call site puts each attribute on its own
			// line, and a grep of the call's own line reports zero labels.
			for _, arg := range call.Args {
				inner, ok := arg.(*ast.CallExpr)
				if !ok {
					continue
				}
				isel, ok := inner.Fun.(*ast.SelectorExpr)
				if !ok || len(inner.Args) == 0 {
					continue
				}
				if pkg, ok := isel.X.(*ast.Ident); !ok || pkg.Name != "attribute" {
					continue
				}
				if lit, ok := inner.Args[0].(*ast.BasicLit); ok {
					if s, err := strconv.Unquote(lit.Value); err == nil {
						out[sel.Sel.Name] = append(out[sel.Sel.Name], s)
					}
				}
			}
			return true
		})
	}
	return out
}

func metricLabels(t *testing.T) map[string][]string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "metrics.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing metrics.go: %v", err)
	}
	src, err := os.ReadFile("metrics.go")
	if err != nil {
		t.Fatalf("reading metrics.go: %v", err)
	}
	text := string(src)

	// field -> instrument name, from the constructor.
	field2name := map[string]string{}
	for _, m := range regexp.MustCompile(
		`m\.(\w+),\s*err\s*=\s*meter\.\w+\(\s*\n?\s*"([a-z_]+)"`).FindAllStringSubmatch(text, -1) {
		field2name[m[1]] = m[2]
	}

	out := map[string][]string{}
	// method name -> the metric(s) it records, so caller-supplied labels can be
	// attributed below. See callerSuppliedLabels.
	method2metric := map[string][]string{}
	ast.Inspect(f, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Recv == nil || fn.Body == nil {
			return true
		}
		var fields, labels []string
		ast.Inspect(fn.Body, func(n2 ast.Node) bool {
			if sel, ok := n2.(*ast.SelectorExpr); ok {
				if x, ok := sel.X.(*ast.SelectorExpr); ok && sel.Sel.Name != "" {
					if id, ok := x.X.(*ast.Ident); ok && id.Name == "m" {
						fields = append(fields, x.Sel.Name)
					}
				}
			}
			call, ok := n2.(*ast.CallExpr)
			if !ok || len(call.Args) == 0 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != "attribute" {
				return true
			}
			if lit, ok := call.Args[0].(*ast.BasicLit); ok {
				if s, err := strconv.Unquote(lit.Value); err == nil {
					labels = append(labels, s)
				}
			}
			return true
		})
		for _, fld := range fields {
			if name, ok := field2name[fld]; ok {
				out[name] = append(out[name], labels...)
				method2metric[fn.Name.Name] = append(method2metric[fn.Name.Name], name)
			}
		}
		return true
	})

	// EVERY recording method takes `extraAttrs ...attribute.KeyValue` and merges
	// it, so a label can be supplied entirely by the CALLER and never appear in
	// metrics.go at all. Reading only the method bodies reports such a metric as
	// carrying fewer labels than it does, and the guard then flags a correct
	// panel -- which is how cleat_call_retries_total|operation reached
	// knownGroupingMismatches when `operation` is in fact supplied at the call
	// site in engine/durablecalls.go.
	for method, labels := range callerSuppliedLabels(t) {
		for _, metric := range method2metric[method] {
			out[metric] = append(out[metric], labels...)
		}
	}
	return out
}

// knownGroupingMismatches are the panels this guard found on the day it was
// written and which need a decision rather than an edit -- exactly like
// unfedMetrics next door. Each groups by a label its metric does not carry and
// no call site adds, so each panel shows one undifferentiated series where it
// promises a breakdown.
//
// THIS LIST HELD FOUR ENTRIES AND THE FOURTH WAS WRONG. It exempted
// cleat_call_retries_total|operation on the stated grounds that "zero callers
// of any of the four pass extraAttrs". Two do: engine/durablecalls.go supplies
// service and operation to RecordCallRetry, and outcome to RecordAmbiguousCall.
// The claim came from a line-oriented grep of each call's own line, and that
// call site puts every attribute on a continuation line. callerSuppliedLabels
// now reads the callers with the AST, so the panel is recognised as correct and
// the entry is gone.
//
// Fixing them is not mechanical. `by (def_name)` is probably right for
// replay_steps; cleat_calls_total carries NO labels at all, so the choice is
// between adding one -- with the cardinality question that implies -- and
// dropping the clause, which changes what the panel means. Tracked in
// cleat#1444.
//
// THIS LIST MAY ONLY SHRINK. A new mismatch fails the test.
var knownGroupingMismatches = map[string]string{
	"cleat_calls_total|workflow_name":             "cleat#1444 -- the metric declares no labels at all",
	"cleat_replay_steps_total|workflow_name":      "cleat#1444 -- the metric declares def_name",
	"cleat_workflows_claimed_total|workflow_name": "cleat#1444",
}

func TestDashboardGroupingLabelsExistOnTheMetric(t *testing.T) {
	labels := metricLabels(t)

	// Vacuity: if the extractor learned nothing, every dashboard passes.
	if len(labels) == 0 {
		t.Fatal("extracted no metric->label mapping from metrics.go; this checked nothing")
	}
	known := 0
	for _, l := range labels {
		known += len(l)
	}
	if known == 0 {
		t.Fatal("extracted metric names but NO labels at all -- the attribute.String scan is " +
			"broken, and every `by (...)` would read as a mismatch or be skipped")
	}
	t.Logf("metrics with a known label set: %d, labels found: %d", len(labels), known)

	// WIDE ON PURPOSE. The first version of this matched only
	// `sum(cleat_x{...}) by (...)` and covered 1 of the 25 `by (` clauses in
	// these dashboards -- a guard looking at 4% of its subject, which is the
	// under-selecting failure this repository keeps meeting. The real forms
	// are histogram_quantile(0.5, sum(rate(cleat_x_bucket{...}[5m]))) by (...)
	// (12), sum(rate(...)) by (...) (10) and count(...) by (...) (2).
	//
	// So: find any cleat_ metric name, then the NEXT `by (...)` after it. That
	// over-matches -- a query naming two metrics attributes the clause to the
	// first -- and over-matching is the right direction here, because a false
	// report is read and dismissed while a missed one is silence.
	// The expressions are read from the PARSED JSON, not from the file's bytes.
	//
	// Two earlier versions matched the raw text and found nothing. A PromQL
	// selector is written `{namespace=~\"$namespace\"}` inside a JSON string,
	// so the bytes between the metric name and its `by (...)` contain literal
	// `"` characters -- and `[^"]*` stops at the first one. The test SKIPPED
	// rather than passed, which is the only reason it was caught; a guard
	// reporting "0 checked" as success would have looked green while matching
	// a format it does not model. That is CLAUDE.md's row for exactly this.
	byClause := regexp.MustCompile(`(cleat_[a-z_]+)\b.*?\bby\s*\(([^)]*)\)`)

	dashboards, err := filepath.Glob(filepath.Join("..", "**", "*.json"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	more, _ := filepath.Glob(filepath.Join("..", "*.json"))
	dashboards = append(dashboards, more...)
	sort.Strings(dashboards)

	checked := 0
	for _, path := range dashboards {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var doc struct {
			Panels []struct {
				Title   string `json:"title"`
				Targets []struct {
					Expr string `json:"expr"`
				} `json:"targets"`
			} `json:"panels"`
		}
		if json.Unmarshal(raw, &doc) != nil || len(doc.Panels) == 0 {
			continue
		}
		var exprs []string
		for _, panel := range doc.Panels {
			for _, tgt := range panel.Targets {
				if tgt.Expr != "" {
					exprs = append(exprs, tgt.Expr)
				}
			}
		}
		if len(exprs) == 0 {
			t.Errorf("%s has %d panels but no target expressions were read; the "+
				"dashboard shape changed and this guard is reading nothing",
				filepath.Base(path), len(doc.Panels))
			continue
		}
		for _, m := range byClause.FindAllStringSubmatch(strings.Join(exprs, "\n"), -1) {
			metric, group := m[1], m[2]
			// Prometheus appends these to histogram and counter series; the
			// instrument is registered under the base name.
			base := metric
			for _, suffix := range []string{"_bucket", "_sum", "_count"} {
				base = strings.TrimSuffix(base, suffix)
			}
			declared, ok := labels[base]
			if !ok {
				declared, ok = labels[metric]
			}
			if !ok {
				continue // the metric's label set could not be resolved; not this test's claim
			}
			for _, g := range strings.Split(group, ",") {
				g = strings.TrimSpace(g)
				if g == "" || globalDashboardLabels[g] {
					continue
				}
				checked++
				found := false
				for _, d := range declared {
					if d == g {
						found = true
					}
				}
				if !found {
					if why, known := knownGroupingMismatches[metric+"|"+g]; known {
						t.Logf("known mismatch (allowed): %s by %q -- %s", metric, g, why)
						continue
					}
					t.Errorf("%s groups %s by %q, but that metric declares no such label "+
						"(it declares %v). The query was written for a different metric -- "+
						"this is how cleat#1317's 'Event History Growth Rate' panel came to "+
						"query cleat_wasm_cache_bytes.", filepath.Base(path), metric, g, declared)
				}
			}
		}
	}
	// Fatal, NOT Skip, and the distinction is check-skips.sh's case (c): a
	// precondition that is always satisfiable in this repo must fail rather
	// than skip. The dashboards demonstrably contain resolvable grouping
	// queries -- 19 of them -- so "0 resolved" means the extractor broke, not
	// that there is nothing to look at.
	//
	// A Skip here was right while the guard was being WRITTEN: it surfaced
	// three separate defects (a regex covering 1 of 25 clauses, a non-greedy
	// prefix matching "cleat_w", and matching raw JSON whose escaped quotes
	// stop `[^"]*`) by reporting "nothing to check" instead of passing. Shipped,
	// that same branch would hide those defects from CI forever. The third
	// outcome belongs to genuinely optional preconditions; this one is not.
	if checked == 0 {
		t.Fatal("no dashboard `sum(cleat_*{...}) by (label)` query resolved to a known " +
			"metric. The dashboards contain 19; zero means this guard's extractor is " +
			"broken, not that the tree is clean.")
	}
	t.Logf("grouping labels checked: %d", checked)
}
