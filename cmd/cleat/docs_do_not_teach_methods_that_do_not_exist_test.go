package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// documentedCallsNotYetFixed baselines the calls this guard finds on develop today,
// keyed on `file|method`. It may only SHRINK: a stale entry fails the test, so a
// fixed document must drop its line in the same change.
//
// WHY A BASELINE AND NOT A FIX IN THE SAME PR. Shipping this guard red would block
// every pull request until every document it flags was corrected, which is how a
// guard gets muted. Shipping it green with the findings named is the shape
// `scripts/check-dead-exports.sh` uses for its dead exports and
// `engine_option_reachability_test.go` uses for its unwired options — the
// difference between a listed defect and an accepted one is that a list can shrink.
//
// WHERE EACH FIX BELONGS. The rest are renames to a method that already exists,
// except `SetState`, which has no direct equivalent and is a documentation decision
// rather than a substitution.
//
// THE THREE `h.Sleep` ENTRIES WERE REMOVED BY cleat#2524, which fixed all seven
// sites across the three migration guides and `docs/determinism.md`. They are gone
// from this map because they are gone from the documents — and deleting the entry
// IS the check that they are: a stale entry fails this test, so a document quietly
// reverted to `h.Sleep` would have to re-add its line here to build.
var documentedCallsNotYetFixed = map[string]string{
	"ABI.md|DurableRandom":                        "h.Random is the method; the example calls a name that was never one",
	"docs/migration/from-dbos.md|CallWithOptions": "h.DurableCallWithOptions",
	"docs/migration/from-restate.md|CallTyped":    "h.DurableCallTyped",
	"docs/migration/from-restate.md|SetState":     "no direct equivalent; the guide's state API needs a doc decision, not a rename",
	"docs/reference/error-codes.md|SendSignal":    "h.SignalWorkflow is the nearest; this block is the 'after' half of a BAD/GOOD pair",
	"examples/DX_COMPARISON.md|CallWithRetry":     "h.DurableCallWithRetry",
}

// TestDocsDoNotTeachMethodsThatDoNotExist pins the property cleat#2524 is about.
//
// THE DEFECT. `h.Sleep(...)` does not exist. It is not a method on `cleat.HostCalls`,
// and there is no alias — the `Timer` interface (cleat/runtime.go) has
// `DurableSleep` and `DurableSleepMs`, and its own doc comment says *"Never call
// time.Now() or time.Sleep() in workflow code"*. Four documents taught `h.Sleep`,
// including all three migration guides, once in an example and once in a summary
// line each, and the correct call appeared ZERO times in those same documents — so a
// reader who wrote what the page told them to write got a compile error naming a
// method the page recommended. cleat#2524 fixed all seven sites; this guard exists
// so the next one of these is caught by CI rather than by a reader.
//
// WHY A PREDICATE AND NOT A COMPILE HARNESS. The census on cleat#2521 measured
// 418 fenced `go` blocks across 201 files, of which **300 (72%) cannot be
// type-checked at all** — they are fragments with no package, no imports and free
// names. A compile harness would need ~330 scratch modules and would still meet
// only 28% of the population. This predicate examines all of it for one class of
// error, cheaply, with no build.
//
// WHAT IT COVERS, STATED SO IT IS NOT MISTAKEN FOR MORE. Fenced `go` blocks only.
// The same defect also appears in **inline code spans** — `In Go, use
// \`h.Sleep(n * time.Second)\`.` — and those are deliberately NOT scanned, because
// a code span is as often prose ABOUT a method as a call to one. Measured when this
// was written: extending to spans added `h.X` and `h.Uppercase` from
// IMPROVEMENT-PLAN.md, which are metavariables in illustrative text, not defects.
// So this guard trades recall for a clean signal on purpose: of #2524's seven
// sites it reached the three that are fenced blocks and missed the four that were
// prose, which is why an issue and not this guard is what closed them. Do not widen
// it without measuring the false-positive class it inherits. Measured over the whole
// tree on 2026-09-27, that class has two members, and neither is a defect: `h.X` and
// `h.Uppercase` are metavariables in IMPROVEMENT-PLAN prose, and `h.Secret` is an API
// under a `### Planned` heading. Both are excludable — by path and by heading
// respectively — rather than by name, so a widened guard is possible; it is a
// separate change with its own false-positive budget, not a tweak to this one. (No
// count is given here on purpose: this PR moved five of the references.)
//
// AND THE COUNT IS NOT IN THIS FILE. #2526 says so explicitly: it is a census of a
// growing population, and a number written into a document is the thing that
// drifts. The test prints its own.
func TestDocsDoNotTeachMethodsThatDoNotExist(t *testing.T) {
	root := repoRootFor(t)

	known := hostCallsMethods(t, root)

	// Anti-vacuity, and it is the whole guard in miniature: a method set derived
	// from nothing agrees with every document. Measured 2026-09-27: 78 methods.
	// The floor is loose on purpose — it catches a derivation that has stopped
	// working, not a corpus that has shrunk.
	if len(known) < 40 {
		t.Fatalf("derived only %d HostCalls method(s); the tree had 78 on 2026-09-27.\n\n"+
			"This is a failure of the CHECK, not a finding about the docs: a method set\n"+
			"derived from almost nothing reports no bad calls in any document.", len(known))
	}

	// The self-test runs the SAME helper the scan uses, so it proves the branch it
	// exercises — a separate implementation would only prove itself. Both
	// directions in one call: the real method must not be reported and the invented
	// one must be.
	selfTest := unknownHostCallMethods("h.DurableLog(\"x\")\nh.NotAMethod(1)\n", known)
	if len(selfTest) != 1 || selfTest[0] != "NotAMethod" {
		t.Fatalf("self-test failed: expected exactly [NotAMethod], got %v.\n\n"+
			"This is a failure of the CHECK, not a finding about the docs.", selfTest)
	}

	blocks := fencedGoBlocks(t, root)
	// Anti-vacuity for the extraction half, same reason as above.
	if len(blocks) < 50 {
		t.Fatalf("extracted %d fenced go block(s) from tracked markdown; there were 394\n"+
			"on 2026-09-27 across docs/ and the root, before the design-doc exclusion.\n\n"+
			"This is a failure of the CHECK, not a finding about the docs.", len(blocks))
	}

	type finding struct {
		file       string
		line       int
		name, text string
	}
	var findings []finding
	live := map[string]struct{}{}
	for _, b := range blocks {
		for _, name := range unknownHostCallMethods(b.body, known) {
			key := b.file + "|" + name
			live[key] = struct{}{}
			if _, baselined := documentedCallsNotYetFixed[key]; baselined {
				continue
			}
			findings = append(findings, finding{b.file, b.line, name, firstLineOf(b.body, name)})
		}
	}

	// A baseline entry that no longer matches is an error, not silence. Without
	// this the list can only be added to, and a fixed document leaves a stale
	// exemption behind that would hide the same defect if it returned. This is the
	// "none stale" property `scripts/check-dead-exports.sh` carries, and the reason
	// the baseline is keyed on file|method rather than file|line: a line number
	// would go stale every time anything above it moved, and a stale entry here
	// looks exactly like a live one.
	var stale []string
	for key := range documentedCallsNotYetFixed {
		if _, ok := live[key]; !ok {
			stale = append(stale, key)
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Errorf("%d baselined entr(y|ies) no longer match anything:\n  %s\n\n"+
			"A baselined call that has been fixed should be DELETED from\n"+
			"documentedCallsNotYetFixed in the same change. Left in place it is a\n"+
			"standing exemption for a defect, and it would hide that defect returning.",
			len(stale), strings.Join(stale, "\n  "))
	}

	if len(findings) == 0 {
		t.Logf("examined %d fenced go block(s) against %d known host-call method(s); "+
			"none calls a method that does not exist beyond the %d baselined",
			len(blocks), len(known), len(documentedCallsNotYetFixed))
		return
	}

	var sb strings.Builder
	for _, f := range findings {
		fmt.Fprintf(&sb, "\n  %s:%d  calls h.%s(  which is not a method on cleat.HostCalls\n      %s",
			f.file, f.line, f.name, f.text)
	}
	t.Errorf("%d documented call(s) name a method that does not exist,%s\n\n"+
		"`h.Sleep` is the instance this guard was written for (cleat#2524): the method is\n"+
		"`h.DurableSleep`, and the `Timer` interface's own doc comment says never to call\n"+
		"`time.Sleep` in workflow code. A reader who writes what the page shows gets a\n"+
		"compile error naming the method the page recommended.\n\n"+
		"Fix the document, not this test. If the method you mean is genuinely absent from\n"+
		"cleat.HostCalls, the document is describing a call that cannot compile.", len(findings), sb.String())
}

type goBlock struct {
	file string
	line int
	body string
}

// fencedGoBlocks returns the fenced ```go blocks of the tracked markdown a reader is
// meant to act on. Design documents and archives are excluded, and the exclusion is
// stated rather than assumed: `docs/contributor/design/` is internal design prose
// rather than a guide, and `*-CLOSED.md` is a historical record where a reference to
// something since removed is correct rather than stale.
func fencedGoBlocks(t *testing.T, root string) []goBlock {
	t.Helper()
	files := trackedFiles(t, root, "*.md")
	re := regexp.MustCompile("(?ms)^```go[ \t]*\n(.*?)^```")
	var out []goBlock
	for _, rel := range files {
		if strings.Contains(rel, "/design/") || strings.HasSuffix(rel, "-CLOSED.md") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			continue
		}
		src := string(data)
		for _, m := range re.FindAllStringSubmatchIndex(src, -1) {
			out = append(out, goBlock{
				file: rel,
				line: strings.Count(src[:m[0]], "\n") + 1,
				body: src[m[2]:m[3]],
			})
		}
	}
	return out
}

var hostCallRe = regexp.MustCompile(`\bh\.([A-Z][A-Za-z0-9_]*)\(`)

// unknownHostCallMethods returns the method names a block calls on `h` that are not
// in `known`, in the order they appear and without duplicates.
func unknownHostCallMethods(block string, known map[string]struct{}) []string {
	var out []string
	seen := map[string]struct{}{}
	for _, m := range hostCallRe.FindAllStringSubmatch(block, -1) {
		name := m[1]
		if _, ok := known[name]; ok {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	return out
}

func firstLineOf(block, name string) string {
	for _, l := range strings.Split(block, "\n") {
		if strings.Contains(l, "h."+name+"(") {
			return strings.TrimSpace(l)
		}
	}
	return ""
}

// hostCallsMethods derives the method set of `cleat.HostCalls` from the tree.
//
// It PARSES rather than greps. The census reached the same 78 with a regex, and a
// regex over `^func (h *HostCallsImpl) Name` would miss a multi-line receiver, a
// differently-named receiver, or a method declared on `HostCalls` itself — the
// shapes differ and a pattern models one of them. `HostCalls` embeds
// `*HostCallsImpl`, so both receivers contribute to what a reader can call.
func hostCallsMethods(t *testing.T, root string) map[string]struct{} {
	t.Helper()

	out, err := exec.Command("git", "-C", root, "ls-files", "*.go").Output()
	if err != nil {
		t.Fatalf("git ls-files '*.go': %v\n\nThis is a failure of the CHECK.", err)
	}

	receivers := map[string]struct{}{"HostCalls": {}, "HostCallsImpl": {}}
	known := map[string]struct{}{}
	fset := token.NewFileSet()
	for _, rel := range strings.Fields(string(out)) {
		if strings.HasSuffix(rel, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(root, rel), nil, 0)
		if err != nil {
			// A file that does not parse is a finding about the tree, not a
			// reason to under-report the method set.
			t.Fatalf("parsing %s: %v", rel, err)
		}
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || len(fn.Recv.List) == 0 || !fn.Name.IsExported() {
				continue
			}
			if _, ok := receivers[recvName(fn.Recv.List[0].Type)]; ok {
				known[fn.Name.Name] = struct{}{}
			}
		}
	}
	return known
}

// recvName unwraps `*T` and `T` to the type name.
func recvName(e ast.Expr) string {
	if star, ok := e.(*ast.StarExpr); ok {
		e = star.X
	}
	if id, ok := e.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}
