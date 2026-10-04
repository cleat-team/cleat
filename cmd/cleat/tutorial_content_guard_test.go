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
// WHY `go vet` AND NOT `go build` -- AND WHY THIS IS NOT THE WEAKENING ITS
// NEIGHBOUR REFUSES. a_scaffold_builds_test.go's header records that its
// predecessor "ran `go vet` rather than `cleat build`" and calls that misstep
// "easy to make again"; the same fact (a `package main` with no `main`) is
// present here, so the resemblance is real and this choice has to be argued
// rather than assumed.
//
// The distinction that saves it is the SUBJECT, not the command's strength: the
// rejected test's subject was a real generated project, on which `cleat build`
// has something to operate. Here the subject is a synthetic assembly that no
// `cleat build` invocation could be pointed at -- the document's own file is not
// what gets compiled. `vet` is therefore the strongest check available for the
// artifact under test, which is what that rule actually asks for. (It also runs
// the compiler's type-checking, so it reports everything a copying reader would
// hit, `declared and not used` included -- measured, not assumed.)
//
// WHAT IT CANNOT SEE, stated rather than implied: it stops short of the link and
// emit stages, so a collision only those reject would pass here. The mitigation
// for that is a separate gap and not this test's business -- quick-start.md's
// documented path IS run for real by TestTutorialQuickStartReachesADoneWorkflow,
// while NOTHING in this repository runs your-first-workflow.md's `cleat build`:
// that file is referenced by no Go, shell or workflow file other than this
// guard. So the weaker-command objection lands hardest exactly where there is no
// other check at all, and that is worth recording rather than papering over.
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
// signature is in the prose, not in the block. The stub supplies that context.
//
// ★ THE RULE, AND THIS FILE GOT IT WRONG FIRST: a stub's signature must be the
// signature of a function the DOCUMENT declares. Nothing in a stub may be
// invented, because an invented parameter hides a defect of precisely the class
// this guard exists for -- and it hides it in the most convincing way available,
// by making the document compile.
//
// The counter-example is real and was caught in review. Both stubs used to
// declare `totalCents int`, and the document declares `totalCents` NOWHERE: its
// PlaceOrder computes the total as `reservation.TotalCents`, and processPayment's
// parameter is `amountCents`. So the stubs were not reproducing the document's
// context, they were supplying an identifier it did not have -- and a reader
// pasting the block got `undefined: totalCents`. Worse, the same stub footer
// returned the typed block's `err`, hiding a second defect of the same class:
// as published, that block declared an error and never used it.
//
// Each stub is now the signature of the document's own function -- PlaceOrder
// for the Saga block, processPayment (whose body the typed block replaces) for
// the other -- and both snippets were repaired to use what they declare.
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
// docs/tutorials/ is link-checked (below), and every one is now SNIPPET-compiled
// too. The third, signals-and-human-loop.md, was excluded until cleat#3079
// repaired it: all three of its AwaitSignals calls disagreed with the SDK, which
// declares
//
//	AwaitSignals(signalNames []string, timeout time.Duration) SignalResult
//
// -- one return value, no option functions. The document showed one call
// assigning to a single `err` with `cleat.WithTimeout`/`cleat.WithSignalPayload`
// (neither exists), and another taking two values from three string arguments.
// Repairing that was a change to what a tutorial TEACHES; the exclusion naming
// it was deleted with the repair, which is the expiry excludedDocs asserts.
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
				// PlaceOrder's own signature (your-first-workflow.md).
				header: "func _sagaFragment(h cleat.HostCalls, userID string, cart []CartItem) (string, error) {",
				footer: "return \"\", nil\n}",
			},
			{
				marker: "h.DurableCallTyped(",
				// processPayment's own signature -- the block is presented as a
				// replacement for that function's body.
				header: "func _typedCallFragment(h cleat.HostCalls, userID string, amountCents int) (Charge, error) {",
				footer: "return Charge{}, nil\n}",
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
	{
		path: "docs/tutorials/signals-and-human-loop.md",
		// Its first block is the whole example -- package clause, imports,
		// types and submitExpense -- so only the two the prose draws out of it
		// need stubs. Each marker is deliberately NOT text that occurs in that
		// complete block: assemble wraps the FIRST fragment whose marker it
		// finds, so a marker shared with block 0 would wrap block 0 instead,
		// and the failure would look like a broken document rather than a
		// broken table entry.
		fragments: []tutorialFragment{
			{
				marker: "escalating to on-call",
				header: "func _timeoutFragment(h cleat.HostCalls, reportID string) (string, error) {",
				footer: "return \"\", nil\n}",
			},
			{
				marker: "switch res.Name",
				header: "func _multiSignalFragment(h cleat.HostCalls) {",
				footer: "}",
			},
		},
	},
}

// excludedDocs are tutorials that do NOT compile today, each with the error that
// must STILL be present and the issue tracking the repair.
//
// THE ENTRY RETIRES ITSELF, which is why it exists rather than a comment saying
// the same thing. When the document is repaired the expected error disappears
// and this assertion fails -- the prompt to move the document into tutorialDocs.
// An exclusion recorded only as an explanation is a SKIP WITH NO EXPIRY: nothing
// fails if the document is fixed and never re-added, so the guard quietly covers
// less than its name claims. Same shape as the testdata fixture table's recorded
// expected failure, which is likewise the entry that proves its own guard can
// report at all.
//
// `whole` names the block that stands alone as a file, so the expiry needs no
// per-fragment stubs: a tutorial's complete example is self-contained, and the
// blocks the prose draws out of it are fragments of that one.
type excludedDoc struct {
	path      string
	whole     int    // index into the document's fenced ```go blocks
	wantError string // must still appear, or the exclusion has gone stale
	issue     string
}

// EMPTY, and the entry that was here is why it can be: signals-and-human-loop.md
// was the last exclusion, and cleat#3079 repaired it, so the document moved up
// into tutorialDocs. The machinery stays for the next document that needs it
// rather than being removed with its only user -- an empty list is what the
// expiry test reports as "nothing to expire", not as a gap in coverage.
var excludedDocs = []excludedDoc{}

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

// TestExcludedTutorialsStillFailForTheRecordedReason is what stops an exclusion
// from being a silent, permanent hole in the coverage above.
func TestExcludedTutorialsStillFailForTheRecordedReason(t *testing.T) {
	root := repoRoot(t)
	if len(excludedDocs) == 0 {
		t.Log("no tutorials are excluded; nothing to expire")
		return
	}
	for _, doc := range excludedDocs {
		t.Run(doc.path, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(root, doc.path))
			if err != nil {
				t.Fatalf("read %s: %v", doc.path, err)
			}
			blocks := goBlocks(t, string(raw))
			if doc.whole >= len(blocks) {
				t.Fatalf("%s has %d fenced ```go block(s) and the exclusion names block %d -- the file has "+
					"moved on, so this check is looking in the wrong place", doc.path, len(blocks), doc.whole)
			}
			out := vetSnippet(t, blocks[doc.whole])
			if out == "" {
				t.Errorf("%s compiles now. Move it into tutorialDocs and delete it from excludedDocs: its "+
					"exclusion (%s) has lapsed, and leaving the entry here means this guard silently covers "+
					"less than it claims.", doc.path, doc.issue)
				return
			}
			if !strings.Contains(out, doc.wantError) {
				t.Errorf("%s no longer fails for the recorded reason.\nwant an error containing %q\ngot:\n%s\n"+
					"Either the repair is partial or the file moved on for another reason; check %s.",
					doc.path, doc.wantError, out, doc.issue)
			}
		})
	}
}
