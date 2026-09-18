package engine

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// engineOptionsNotWired's reasons are the only place several real design
// decisions live, and a guard's exemption list is not where anyone reads an
// API's intent. This asserts the reason also exists where a reader of the
// option would find it: on the option's own doc comment.
//
// # Why this is a check and not a fifth sweep
//
// Four instances, found by three sessions, none of whom went looking:
//
//   - WithAmbiguityResolver -- I argued on cleat#1778 that the ambiguity
//     recovery path had never been built. It had; the decision was in the
//     table. Another session then filed cleat#1871 reaching the identical
//     wrong conclusion from the identical grep, about an hour later.
//   - WithAllowVersionMismatch -- swept up beside it in cleat#1874.
//   - WithPluginCallGuard -- found while verifying the table for cleat#1871
//     (cleat#1873), and sharper: the guard's TRIGGER has no non-test writer,
//     so Check cannot fire at all.
//   - WithPluginCallObserver -- found while checking whether that sweep was
//     complete (cleat#1878).
//
// Two of the four were discovered while fixing another one. That rate is the
// argument for making it fail in CI rather than waiting for the next person to
// happen to read one table entry.
//
// # What it cannot do
//
// It does not check that the prose MATCHES the reason; nothing cheap can. It
// checks that a reader of the declaration is told there is something to know,
// and given a reference to follow. That is the difference between a decision
// that is recorded and a decision that is discoverable.
//
// # The predicate that does not work, measured so nobody builds it
//
// The obvious version extracts the issue number from the exemption entry and
// requires it in the doc comment. Every one of the ten entries cites the same
// issue -- cleat#878, the issue the table was created under -- while the two
// landed fixes cite cleat#1871 and cleat#1778, the issues that fixed them. So
// that version fails on both of the fixes it is modelled on, and passes for
// anyone who pastes #878 near the option. Requiring SOME reference, rather
// than the table's, is what separates the fixed rows from the open ones.

// mundaneExemptions are the categories that describe a test-only or duplicated
// mechanism rather than production behaviour a caller could be surprised by.
// An option that exists because production uses a different spelling of it
// owes its reader nothing beyond what the option already says.
//
// Everything else -- EMBEDDER API, REAL GAP, and anything uncategorised --
// describes what happens in a real deployment, which is what a reader of the
// option is trying to find out.
var mundaneExemptions = []string{"TEST SEAM", "ALTERNATE PATH"}

// exemptionCategory returns the category prefix of a reason string, or "" when
// it carries none. The leading issue citation is stripped first: every entry
// opens with the table's own birth issue, which is not the category.
func exemptionCategory(reason string) string {
	s := strings.TrimSpace(reason)
	if m := regexp.MustCompile(`^cleat#\d+:?\s*`).FindString(s); m != "" {
		s = s[len(m):]
	}
	if m := regexp.MustCompile(`^([A-Z][A-Z ]*[A-Z]):`).FindStringSubmatch(s); m != nil {
		return m[1]
	}
	return ""
}

// needsDeclarationNote reports whether an exemption's reason has to be repeated
// at the option's own declaration.
func needsDeclarationNote(reason string) bool {
	cat := exemptionCategory(reason)
	for _, m := range mundaneExemptions {
		if cat == m {
			return false
		}
	}
	return true
}

var issueRef = regexp.MustCompile(`#\d+`)

// docCitesAnIssue reports whether a doc comment points the reader anywhere.
// A reference is the cheap, checkable half of "this is not the whole story":
// the prose can say what the constraint is, and the number says where the
// argument for it lives.
func docCitesAnIssue(doc string) bool { return issueRef.MatchString(doc) }

func TestAnExemptedOptionSaysWhyAtItsDeclaration(t *testing.T) {
	docs := engineOptionDocs(t)

	// Scanner control. Every failure mode of the scan -- a parse mode without
	// comments, a file that moved, a declaration whose shape changed --
	// produces the same symptom: no docs found, and a check that reports every
	// exempted option at once or none of them. Neither reads as a scan that
	// broke.
	if len(docs) < len(engineOptionsNotWired) {
		t.Fatalf("found doc comments for %d option declarations but %d are exempted; "+
			"the scan is not seeing the declarations it is supposed to judge", len(docs), len(engineOptionsNotWired))
	}

	for name, reason := range engineOptionsNotWired {
		doc, found := docs[name]
		if !found {
			t.Errorf("%s is exempted but was not found as a declaration in engine/. "+
				"If it moved, this check stopped judging it silently.", name)
			continue
		}
		if !needsDeclarationNote(reason) {
			continue
		}
		if !docCitesAnIssue(doc) {
			t.Errorf("%s is exempted from the reachability guard as %q, and its own doc "+
				"comment cites no issue.\n\n"+
				"The reason lives only in engineOptionsNotWired, which nobody reads to learn "+
				"what an API does. Four options reached this state and two of them were found "+
				"while fixing a third. Say at the declaration what a caller would be surprised "+
				"by, and cite the issue that settled it.\n\n"+
				"Doc comment as it stands:\n%s",
				name, reason, indent(doc))
		}
	}
}

// engineOptionDocs maps each EngineOption constructor declared under engine/ to
// its doc comment. Scoped to engine/ deliberately: every exempted option is
// declared there, and the control above turns a move into a failure rather than
// into a silent pass.
func engineOptionDocs(t *testing.T) map[string]string {
	t.Helper()
	root := moduleRoot(t)

	out, err := exec.Command("git", "-C", root, "ls-files", "engine/*.go").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	files := strings.Fields(string(out))
	if len(files) < 20 {
		t.Fatalf("git ls-files returned %d files under engine/; expected dozens", len(files))
	}

	docs := map[string]string{}
	for _, rel := range files {
		if strings.HasSuffix(rel, "_test.go") {
			continue
		}
		// ParseComments, which the sibling reachability guard does not need and
		// so does not ask for. Without it fn.Doc is nil on every declaration and
		// this check would report all ten as undocumented.
		file, perr := parser.ParseFile(token.NewFileSet(), filepath.Join(root, rel), nil,
			parser.ParseComments|parser.SkipObjectResolution)
		if perr != nil {
			continue
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || !strings.HasPrefix(fn.Name.Name, "With") {
				continue
			}
			if fn.Type.Results == nil || len(fn.Type.Results.List) != 1 {
				continue
			}
			if id, ok := fn.Type.Results.List[0].Type.(*ast.Ident); ok && id.Name == "EngineOption" {
				docs[fn.Name.Name] = fn.Doc.Text()
			}
		}
	}
	return docs
}

func indent(s string) string {
	var b strings.Builder
	for _, line := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		b.WriteString("    " + line + "\n")
	}
	return b.String()
}

// TestTheExemptionCategoryIsReadCorrectly is the known-positive for the half
// that decides what is in scope. Every row is a real string from the table or a
// deliberate near-miss of one.
func TestTheExemptionCategoryIsReadCorrectly(t *testing.T) {
	for _, tc := range []struct {
		reason    string
		wantCat   string
		wantNotes bool
	}{
		{"cleat#878 TEST SEAM: production uses WithBackends (plural); 42 test call sites", "TEST SEAM", false},
		{"cleat#878 ALTERNATE PATH: worker passes WithWasmtimeDeferBudget instead", "ALTERNATE PATH", false},
		{"cleat#878 EMBEDDER API: nil is the designed default; degrades to no-op", "EMBEDDER API", true},
		{"cleat#878 REAL GAP: cleat_fetch always fails in a real worker (3.317). ", "REAL GAP", true},
		// The uncategorised shape, which is why the check does not require a
		// category: an entry with no prefix must be IN scope, not skipped.
		{"cleat#878: the worker calls execStore.ContinueAsNew directly", "", true},
		// A category-looking word that is not one must not be mistaken for a
		// prefix, or a reason could opt itself out by opening with a shout.
		{"cleat#878 NOTE this is not a category", "", true},
	} {
		if got := exemptionCategory(tc.reason); got != tc.wantCat {
			t.Errorf("exemptionCategory(%.40q) = %q, want %q", tc.reason, got, tc.wantCat)
		}
		if got := needsDeclarationNote(tc.reason); got != tc.wantNotes {
			t.Errorf("needsDeclarationNote(%.40q) = %v, want %v", tc.reason, got, tc.wantNotes)
		}
	}
}

// TestTheDocCitationCheckCanFail is the other known-positive. The check passes
// on a clean tree by construction the moment the open rows are fixed, so a
// self-test that has only ever observed a pass is indistinguishable from one
// that cannot fail.
func TestTheDocCitationCheckCanFail(t *testing.T) {
	for _, tc := range []struct {
		name string
		doc  string
		want bool
	}{
		{"the shape every unfixed option has", "WithPluginCallObserver sets a post-invocation observer.\n", false},
		{"prose with no reference is not enough", "This is an embedder API and the worker does not use it.\n", false},
		{"a cleat-prefixed reference", "Embedder API; see cleat#1871.\n", true},
		{"a bare reference", "Embedder API; see #1871.\n", true},
		{"empty", "", false},
	} {
		if got := docCitesAnIssue(tc.doc); got != tc.want {
			t.Errorf("%s: docCitesAnIssue = %v, want %v", tc.name, got, tc.want)
		}
	}
}
