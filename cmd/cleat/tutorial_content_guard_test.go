package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// cleat#3072. Nothing compiled the Go snippets published in docs/tutorials/,
// and one of them could not compile: your-first-workflow.md's Saga example
// declared its closure-local results with `:=` inside each closure, so the
// sibling compensation closure could not see them -- a reader who copied it got
// `declared and not used: reservation` / `undefined: reservation`. It sat there
// until an audit ran the walkthrough.
//
// WHY THIS IS A GO TEST IN THIS PACKAGE rather than a script wired into a
// workflow: `./cmd/...` is one of the package groups in ci.yml's test-go
// matrix, so a test here is wired the moment it lands. A guard that needs a new
// CI step is a mechanism someone must remember to connect -- and this repo
// already has a defect class for mechanisms that exist and are wired to
// nothing.
//
// WHY `go vet` AND NOT `go build`, since that distinction is load-bearing
// elsewhere in this package (see a_scaffold_builds_test.go's header, which
// refuses to weaken "does the documented command work" into "does the module
// graph resolve"). `vet` runs the compiler's type-checking and catches every
// error a copy-pasting reader would hit, including the two above. What it does
// not do is LINK -- and `func main` is supplied by `cleat build` for exactly
// this project shape, which is why cleat#1888's notes say a basic or agent
// scaffold cannot be `go build`-ed and must not be. Injecting a main here would
// be testing a file the tutorials never show.
//
// NOT GUARDED ON NETWORK AVAILABILITY, for the reason a_scaffold_builds_test.go
// gives: `go mod tidy` is a real dependency of the thing under test, and
// skipping when the network is absent would report clean in the one situation
// where this test has measured nothing.
//
// scripts/build-documented-examples.sh covers the NEIGHBOURING case -- it runs
// the `cleat build` each examples/*/README.md documents, deriving its set from
// `git ls-files` -- so it guards example README *commands*, not the fenced code
// blocks in docs/tutorials/. Its own header states the principle this file
// extends: "A green CI run was evidence that the unit tests pass. It was not
// evidence that the product does what the documentation says."

// tutorialFragment wraps a fenced ```go block that is a FRAGMENT rather than a
// self-contained declaration.
//
// THIS IS THE PART THAT CANNOT BE AUTOMATED AWAY, so it is stated rather than
// inferred. A tutorial presents the Saga step as a block to place inside an
// entry point the document declares elsewhere, so the block's enclosing
// signature is in the prose, not in the block. The stub supplies that context,
// taken from the document's own function -- for your-first-workflow.md, from
// PlaceOrder's parameter list.
//
// Keyed by a MARKER rather than by index, so inserting a block above does not
// silently re-point a stub at the wrong text.
type tutorialFragment struct {
	marker string // a substring that identifies the block
	header string // the function header the document implies for it
	footer string // what closes it, including any use its locals need
}

type tutorialDoc struct {
	path      string
	fragments []tutorialFragment
	// knownBroken replaces one block so the guard can be shown to FAIL. A guard
	// that only ever passes is indistinguishable from a guard that stopped
	// checking, which is the same defect this whole file is about.
	knownBroken *tutorialFragment
	brokenText  string
	brokenWant  string // the text the failure must contain
}

// SCOPE, STATED RATHER THAN LEFT AS AN OMISSION. Every file under
// docs/tutorials/ is link-checked (below), but only these two are
// SNIPPET-compiled. The third, signals-and-human-loop.md, is excluded because it
// is currently WRONG in a way that is a documentation defect rather than a
// guard's business: all three of its AwaitSignals calls disagree with the SDK,
// which declares
//
//	AwaitSignals(signalNames []string, timeout time.Duration) SignalResult
//
// -- one return value, no option functions. The document shows one call
// assigning to a single `err` with `cleat.WithTimeout`/`cleat.WithSignalPayload`
// (neither exists), and another taking two values from three string arguments.
// Repairing that is a change to what a tutorial TEACHES and belongs in its own
// change, filed as cleat#3079; adding the doc here is then a one-line entry.
var tutorialDocs = []tutorialDoc{
	{
		path: "docs/tutorials/quick-start.md",
		// Its one Go block is the whole scaffold sample: package clause,
		// imports and the entry point together.
	},
	{
		path: "docs/tutorials/your-first-workflow.md",
		fragments: []tutorialFragment{
			{
				marker: "s := cleat.NewSaga()",
				header: "func _sagaFragment(h cleat.HostCalls, userID string, cart []CartItem, totalCents int) (string, error) {",
				footer: "return \"\", nil\n}",
			},
			{
				marker: "h.DurableCallTyped(",
				header: "func _typedCallFragment(h cleat.HostCalls, userID string, totalCents int) (string, error) {",
				// Ends by USING err, which the fragment declares and would
				// otherwise leave unused.
				footer: "return \"\", err\n}",
			},
		},
		knownBroken: &tutorialFragment{marker: "s := cleat.NewSaga()"},
		brokenText: "// The ORIGINAL documented form, kept as the guard's known-positive.\n" +
			"// `reservation` is declared INSIDE the forward closure, so the sibling\n" +
			"// cannot see it -- exactly the two errors the audit quoted.\n" +
			"s := cleat.NewSaga()\n" +
			"s.AddStep(\"reserve_inventory\",\n" +
			"    func(h cleat.HostCalls) (string, error) {\n" +
			"        reservation, err := reserveInventory(h, userID, cart)\n" +
			"        return \"\", err\n    },\n" +
			"    func(h cleat.HostCalls) error {\n" +
			"        return releaseReservation(h, reservation.ReservationID)\n    },\n)\n" +
			"if err := s.Run(h); err != nil {\n    return \"\", err\n}\n",
		brokenWant: "declared and not used: reservation",
	},
}

var goFenceRe = regexp.MustCompile("(?ms)^```go\\s*$\\n(.*?)^```\\s*$")

// goBlocks returns every fenced ```go block in a markdown file, in order.
func goBlocks(t *testing.T, markdown string) []string {
	t.Helper()
	var out []string
	for _, m := range goFenceRe.FindAllStringSubmatch(markdown, -1) {
		out = append(out, m[1])
	}
	return out
}

// assemble turns a tutorial's own Go blocks into one compilable file, wrapping
// the blocks the document presents as fragments. `override` replaces the marker
// block with different text, which is how the known-positive is built.
func assemble(t *testing.T, doc tutorialDoc, markdown, override string) string {
	t.Helper()
	blocks := goBlocks(t, markdown)
	if len(blocks) == 0 {
		t.Fatalf("%s has no fenced ```go block -- the extractor or the file has drifted", doc.path)
	}
	var parts []string
	for _, b := range blocks {
		text := b
		if override != "" && doc.knownBroken != nil && strings.Contains(b, doc.knownBroken.marker) {
			text = override
		}
		var frag *tutorialFragment
		for i := range doc.fragments {
			if strings.Contains(text, doc.fragments[i].marker) {
				frag = &doc.fragments[i]
				break
			}
		}
		if frag == nil {
			parts = append(parts, text)
			continue
		}
		parts = append(parts, frag.header+"\n"+text+frag.footer)
	}
	return strings.Join(parts, "\n\n")
}

// vetSnippet writes an assembled file into a scratch module that resolves the
// SDK from this checkout, and returns `go vet`'s combined output.
func vetSnippet(t *testing.T, src string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module tutorial_snippet\n\ngo 1.27.0\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "snippet.go"), []byte(src), 0o644); err != nil {
		t.Fatalf("write snippet.go: %v", err)
	}
	resolveScaffoldAgainstThisCheckout(t, dir)

	cmd := exec.Command("go", "vet", "./...")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err == nil {
		return ""
	}
	return string(out)
}

func TestTutorialGoSnippetsCompile(t *testing.T) {
	root := repoRoot(t)
	for _, doc := range tutorialDocs {
		t.Run(doc.path, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(root, doc.path))
			if err != nil {
				t.Fatalf("read %s: %v", doc.path, err)
			}
			markdown := string(raw)

			if out := vetSnippet(t, assemble(t, doc, markdown, "")); out != "" {
				t.Errorf("a Go snippet in %s does not type-check, so a reader copying it gets a build failure:\n%s", doc.path, out)
			}

			// The known-positive. Without it, a guard that had stopped
			// extracting anything would also report nothing here.
			if doc.knownBroken == nil {
				return
			}
			out := vetSnippet(t, assemble(t, doc, markdown, doc.brokenText))
			if out == "" {
				t.Errorf("the known-positive in %s compiled. This guard cannot report a failure at all, "+
					"so its pass above is not evidence.", doc.path)
				return
			}
			// Printed on every run, not only on failure: the negative control
			// for a guard is the thing that proves it can still report, and one
			// that is only visible when something already went wrong is not
			// evidence that it ran.
			t.Logf("known-positive rejected the broken snippet, as it must:\n%s", strings.TrimSpace(out))

			// Asserted on the TEXT, not only on the status: a second, correct
			// mechanism could otherwise supply the failure and certify a guard
			// that has lost the check it is named after.
			if !strings.Contains(out, doc.brokenWant) {
				t.Errorf("the known-positive in %s failed, but not for the reason this guard exists to catch.\n"+
					"want an error containing %q\ngot:\n%s", doc.path, doc.brokenWant, out)
			}
		})
	}
}

var mdLinkRe = regexp.MustCompile(`\]\(([^)\s]+)\)`)

// TestTutorialRelativeLinksResolve guards the other half of the same gap: the
// same file that carried a snippet which could not compile carried two links
// pointing at paths that do not exist (`docs/guide/` was reorganised away, and
// a sibling link was written one directory too shallow). Nothing checked them.
// deadLinks returns the relative link targets in markdown that do not resolve
// against the file's own directory.
func deadLinks(docDir, markdown string) []string {
	var dead []string
	for _, m := range mdLinkRe.FindAllStringSubmatch(markdown, -1) {
		target := m[1]
		if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") ||
			strings.HasPrefix(target, "mailto:") || strings.HasPrefix(target, "#") {
			continue
		}
		rel := strings.SplitN(target, "#", 2)[0]
		if rel == "" {
			continue
		}
		if _, err := os.Stat(filepath.Join(docDir, rel)); err != nil {
			dead = append(dead, target)
		}
	}
	return dead
}

func TestTutorialRelativeLinksResolve(t *testing.T) {
	root := repoRoot(t)

	// The negative control, FIRST, because it is the thing that makes every
	// clean result below mean something: this checker must report a link to a
	// path that is definitely not there. Without it, a pattern that silently
	// matched nothing would report every tutorial as clean.
	synthetic := "[gone](definitely-not-a-real-path-3072.md)"
	if got := deadLinks(t.TempDir(), synthetic); len(got) != 1 {
		t.Fatalf("the link checker reported %v for a link that certainly does not resolve; it cannot "+
			"report a dead link, so its silence below is not evidence", got)
	}

	// EVERY tutorial, not only the snippet-compiled ones: link rot is not
	// confined to the files that happen to hold Go blocks, and this half needs
	// no per-document stubs.
	docs, err := filepath.Glob(filepath.Join(root, "docs", "tutorials", "*.md"))
	if err != nil || len(docs) == 0 {
		t.Fatalf("no tutorials matched docs/tutorials/*.md (err=%v) -- the check would be vacuous", err)
	}
	for _, abs := range docs {
		rel, _ := filepath.Rel(root, abs)
		t.Run(rel, func(t *testing.T) {
			raw, err := os.ReadFile(abs)
			if err != nil {
				t.Fatalf("read %s: %v", rel, err)
			}
			if dead := deadLinks(filepath.Dir(abs), string(raw)); len(dead) > 0 {
				t.Errorf("%s links to %v, which do not exist", rel, dead)
			}
		})
	}
}
