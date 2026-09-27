package engine

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// writeResultDiscardAllowlist names call sites permitted to throw away the byte
// count writeResult returns, keyed "file.go:function". A reason must say why the
// count is not needed -- not merely something true about the site.
//
// Empty is the intended state. An entry is a claim that a guest can learn how
// many bytes it got by some other means; if it cannot, the entry is a bug
// wearing an exemption.
var writeResultDiscardAllowlist = map[string]string{}

// TestNoHostCallDiscardsTheBytesWritten is a static guard over a defect family
// this repo has now met twice, in both of its signs.
//
// writeResult copies a string into a guest buffer and returns how many bytes it
// actually wrote, which is not len(source) whenever the guest's buffer is
// smaller. That count is the only thing the guest can use to know where its
// value ends. Two ways to get it wrong:
//
//	reported len(source)  five sites, fixed earlier -- the guest reads past what
//	                      was written. See cancellation_reason_length_test.go.
//	reported nothing      cleat_set_scope, fixed in #1043 -- the guest reads an
//	                      empty value however much was written.
//
// The earlier sweep searched for `_, _ = s.writeResult(...)` followed by a
// length taken from the source string. That shape is the FIRST form and cannot
// match the second, which reports no length at all -- so scope.go read as clean
// to a search built from a real instance of the same family. A pattern derived
// from one bug encodes that bug's incidentals along with its essence.
//
// This guard asks the question that covers both: is the count discarded at all?
// Everything downstream of keeping it is a matter of using it correctly, which a
// static check cannot judge; throwing it away is decidable, and is the common
// root of both forms.
//
// Parsed with go/ast, not grep: cancellation_reason_length_test.go contains the
// string `_, _ = s.writeResult(...)` inside a comment describing the old bug, and
// a textual sweep reports that prose as a call site.
func TestNoHostCallDiscardsTheBytesWritten(t *testing.T) {
	root := repoRootForGuard(t)

	// git ls-files, not filepath.Walk: a working tree can contain scratch
	// checkouts (.claude/worktrees/) that are whole second copies of the repo,
	// and attributing a finding to one is a scope error that gets likelier as
	// the tree gets messier.
	out, err := exec.Command("git", "-C", root, "ls-files", "engine/*.go").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	files := strings.Fields(string(out))
	if len(files) == 0 {
		t.Fatal("git ls-files matched no engine/*.go -- the guard would pass no " +
			"matter what the code did")
	}

	var scanned, calls int
	var bad []string
	fset := token.NewFileSet()

	for _, rel := range files {
		if strings.HasSuffix(rel, "_test.go") {
			continue
		}
		src, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		f, err := parser.ParseFile(fset, rel, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", rel, err)
		}
		scanned++

		var fn string
		ast.Inspect(f, func(n ast.Node) bool {
			if d, ok := n.(*ast.FuncDecl); ok {
				fn = d.Name.Name
			}
			asn, ok := n.(*ast.AssignStmt)
			if !ok || len(asn.Rhs) != 1 {
				return true
			}
			call, ok := asn.Rhs[0].(*ast.CallExpr)
			if !ok || !isWriteResultCall(call) {
				return true
			}
			calls++
			// The count is Lhs[0]. Discarded when it is the blank identifier.
			if len(asn.Lhs) == 0 {
				return true
			}
			id, ok := asn.Lhs[0].(*ast.Ident)
			if !ok || id.Name != "_" {
				return true
			}
			key := filepath.Base(rel) + ":" + fn
			if _, exempt := writeResultDiscardAllowlist[key]; exempt {
				return true
			}
			bad = append(bad, key+"  ("+fset.Position(asn.Pos()).String()+")")
			return true
		})
	}

	// Known-positive: the scan must actually be finding writeResult calls. If
	// the AST walk silently matched nothing, "no discards" is vacuous -- the
	// same failure this guard exists to prevent, one level up.
	if calls == 0 {
		t.Fatalf("found no writeResult call sites across %d files -- the walk is "+
			"broken and this guard proves nothing", scanned)
	}
	t.Logf("scanned %d files, %d writeResult call sites", scanned, calls)

	if len(bad) > 0 {
		sort.Strings(bad)
		t.Errorf("%d writeResult call site(s) discard the byte count:\n  %s\n\n"+
			"That count is how a guest learns where its value ends; it is not "+
			"len(source) when the guest's buffer is smaller. Report it, or add "+
			"an allowlist entry saying how the guest learns the length instead.",
			len(bad), strings.Join(bad, "\n  "))
	}
}

func isWriteResultCall(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "writeResult"
}

func repoRootForGuard(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatalf("git rev-parse: %v", err)
	}
	return strings.TrimSpace(string(out))
}
