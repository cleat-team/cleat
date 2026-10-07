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
// WHAT IT COVERS — BOTH POPULATIONS, SINCE cleat#2549. Fenced `go` blocks AND the
// inline code spans outside them. The span half was added because the defect does not
// respect the line between a call and a mention: of cleat#2524's seven sites the fenced
// scan reached three, and of cleat#2532's five it reached one. **Prose is where a reader
// meets the name** — the closing summary line of each migration guide is a code span,
// not a block, and that is the sentence a reader takes away.
//
// THE QUESTION THAT HELD IT BACK IS ANSWERED RATHER THAN ASSUMED AWAY. It was *"a code
// span is as often prose ABOUT a method as a call to one"*, and the answer is that the
// guard's subject is the **reference**, not the call: a backticked `h.Nonexistent` is a
// defect whether it is presented as a call or as a mention, because the reader still
// cannot use it and the page still names something that does not exist. The one class
// of reference legitimately to a method that does not exist is a **planned** API — and
// that is a place, not a name, which is why the heading exclusion below is a mechanism.
//
// TWO EXCLUSIONS, BOTH BY MECHANISM AND BOTH NAMED. Measured 2026-09-27, the
// false-positive class has exactly two members and neither is a defect:
//
//	path `IMPROVEMENT-PLAN*`   `h.X`, `h.Uppercase` — metavariables in planning prose
//	heading `### Planned`      `h.Secret` — an API that SHOULD not exist yet
//
// Excluding a name would be a standing exemption for a defect; excluding a *place* is a
// statement about what those documents are for. No count is written here on purpose:
// cleat#2532 moved several, and a number would have been wrong within the hour.
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
	// exercises — a separate implementation would only prove itself. It covers BOTH
	// shapes a host-call reference takes, and the second is the whole of the
	// widening: a name in a parenthetical list has no paren, so a self-test that
	// passed only a call would go green on a matcher that requires one.
	selfTest := unknownHostCallMethods(
		"h.DurableLog(\"x\")\nh.NotAMethod(1)\n(`h.AlsoNotAMethod`, `h.DurableCall`)\n", known)
	if len(selfTest) != 2 || selfTest[0] != "NotAMethod" || selfTest[1] != "AlsoNotAMethod" {
		t.Fatalf("self-test failed: expected exactly [NotAMethod AlsoNotAMethod], got %v.\n\n"+
			"The first is a call and the second is a parenthetical name; a matcher requiring\n"+
			"an open paren reports only the first, which is why both are asserted here.\n\n"+
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

	// The prose half, cleat#2549. Same key space as the fenced scan — `file|method` — so
	// a baseline entry keeps meaning what it always meant: this document names this
	// method somewhere. The scan itself is the point; a baseline satisfied by a block and
	// a block satisfied by a span are the same defect either way.
	spans := proseCodeSpans(t, root)
	// Anti-vacuity for the extracted population, for the same reason as the blocks above.
	// The floor is loose on purpose — it catches a derivation that has stopped working,
	// not a corpus that has shrunk — and it is two orders below the real population, so
	// it cannot be mistaken for a claim about how many spans there are.
	if len(spans) < 1000 {
		t.Fatalf("extracted only %d inline code span(s) outside fenced blocks.\n\n"+
			"This is a failure of the CHECK, not a finding about the docs: a span scan that\n"+
			"extracts almost nothing reports no bad names in any document.", len(spans))
	}
	for _, s := range spans {
		for _, name := range unknownHostCallMethods(s.text, known) {
			key := s.file + "|" + name
			live[key] = struct{}{}
			if _, baselined := documentedCallsNotYetFixed[key]; baselined {
				continue
			}
			findings = append(findings, finding{s.file, s.line, name, strings.TrimSpace(s.text)})
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
		t.Logf("examined %d fenced go block(s) and %d inline code span(s) against %d known "+
			"host-call method(s); none names a method that does not exist beyond the %d baselined",
			len(blocks), len(spans), len(known), len(documentedCallsNotYetFixed))
		return
	}

	var sb strings.Builder
	for _, f := range findings {
		fmt.Fprintf(&sb, "\n  %s:%d  names h.%s, which is not a method on cleat.HostCalls\n      %s",
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

// codeSpan is one inline code span outside every fenced block.
type codeSpan struct {
	file string
	line int
	text string
}

// proseCodeSpans returns the inline code spans that are NOT inside a fenced block, on
// the paths this guard scans. The test's header says why prose is scanned at all and
// names the two exclusions; what belongs here is how they are implemented.
//
// The fence is BLANKED rather than removed — replaced by the same number of newlines —
// so a span's reported line number is its line in the file. Removing the fences would
// make every finding below the first block point at the wrong line, which is the class
// of error this whole file is about.
func proseCodeSpans(t *testing.T, root string) []codeSpan {
	t.Helper()
	fence := regexp.MustCompile("(?ms)^```.*?^```")
	span := regexp.MustCompile("`([^`]*)`")
	var out []codeSpan
	for _, rel := range trackedFiles(t, root, "*.md") {
		// The path exclusion: planning prose uses `h.X` as a metavariable, so a name
		// there is illustrative text rather than a reference to a method.
		if strings.HasPrefix(rel, "IMPROVEMENT-PLAN") {
			continue
		}
		if strings.Contains(rel, "/design/") || strings.HasSuffix(rel, "-CLOSED.md") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			continue
		}
		src := string(data)
		prose := fence.ReplaceAllStringFunc(src, func(m string) string {
			return strings.Repeat("\n", strings.Count(m, "\n"))
		})
		planned := plannedSectionLines(prose)
		for _, m := range span.FindAllStringSubmatchIndex(prose, -1) {
			line := strings.Count(prose[:m[0]], "\n") + 1
			if planned[line] {
				continue
			}
			out = append(out, codeSpan{file: rel, line: line, text: prose[m[2]:m[3]]})
		}
	}
	return out
}

var plannedHeading = regexp.MustCompile(`^#{2,4}\s+Planned\b`)

// plannedSectionLines marks the lines under a heading naming a planned API, where a
// reference to a method that does not exist yet is correct rather than a defect. The
// section ends at the next heading of any level, so the exclusion cannot leak past it.
func plannedSectionLines(src string) map[int]bool {
	out := map[int]bool{}
	in := false
	for i, l := range strings.Split(src, "\n") {
		if strings.HasPrefix(l, "#") {
			in = plannedHeading.MatchString(l)
			continue
		}
		if in {
			out[i+1] = true
		}
	}
	return out
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

// hostCallRe matches a host-call name on `h`. It does NOT require an open paren,
// and that is deliberate: a name in a parenthetical list has none —
//
//	(`h.DurableCall`, `h.CleatSleep`, `h.CleatLog`, etc.)
//
// — and a pattern that can only return names of the shape it assumes cannot test
// the assumption. Measured 2026-09-28: widening it changes NO finding on the tree
// — the finding SET is identical before and after — so this is recall insurance
// rather than a fix for a live instance.
//
// The count is deliberately not written here. cleat#2536 removes three baseline
// entries from this same file, so a number would be wrong whichever of the two
// landed first, and "nine before, nine after" was already wrong for it when
// written. The property — an identical finding set — is what the change is.
//
// The two names that motivated it are in PROSE, outside this guard's fenced-block
// scope — worth stating because the issue that asked for this widening described
// them as being inside it. Reaching prose is a different change, and it inherits
// the false-positive classes the test's header describes.
var hostCallRe = regexp.MustCompile(`\bh\.([A-Z][A-Za-z0-9_]*)`)

// unknownHostCallMethods returns the method names a block references on `h` that
// are not in `known`, in the order they appear and without duplicates. Both shapes
// count: a call, and a bare name in a parenthetical list.
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

// firstLineOf finds the line a finding sits on, for the diagnostic. It matches the
// name whether or not a paren follows: a helper requiring "(" would print an empty
// line for exactly the findings the matcher above was widened to reach.
func firstLineOf(block, name string) string {
	re := regexp.MustCompile(`\bh\.` + regexp.QuoteMeta(name) + `\b`)
	for _, l := range strings.Split(block, "\n") {
		if re.MatchString(l) {
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
