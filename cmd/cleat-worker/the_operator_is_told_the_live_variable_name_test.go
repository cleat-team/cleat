package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// #1904 unified the database env var on CLEAT_DATABASE_URL and
// TestNoProductionCodeReadsTheGenericDatabaseURL guards the READS. It passed
// while five strings still told operators to set the retired DATABASE_URL
// (cleat#1946): four in cmd/cleat-worker/main.go and one in
// cmd/cleat-bench/main.go, sitting beside the very calls that guard checks.
//
// THIS IS A SECOND GUARD, NOT A WIDENING OF THAT ONE. That guard excludes prose
// on purpose and has a case pinning it -- `// DATABASE_URL used to be read
// here` must NOT match, "a sentence about it is not a read". Teaching it to
// match text would delete a decision someone recorded deliberately.
//
// So the split is by WHAT THE TEXT DOES, which is also what makes this
// checkable without exemptions:
//
//   - a STRING LITERAL naming the variable is an instruction to an operator.
//     If it names a variable that no longer exists, following it produces a
//     worker that cannot connect and an error message that names the same dead
//     variable. That is this guard.
//   - a COMMENT naming it is a statement to a reader, which may be legitimate
//     history. cmd/cleat-worker/config.go says DATABASE_URL twice, explaining
//     why the unification went to the namespaced name; the explanation cannot
//     be written without the old name in it.
//
// Scanning literals rather than lines is what keeps config.go out of scope BY
// CONSTRUCTION instead of by an exemption list. An exemption would have to be
// widened for the next legitimate mention, and widening what a check ignores is
// how it comes to ignore the thing it was built for.
//
// WHAT THIS DOES NOT COVER, stated because a guard whose limits are unwritten
// gets read as covering more than it does: comments. The stale
// `// Fall back to DATABASE_URL env var` at main.go:133 was also part of
// cleat#1946 and is fixed in the same change, but nothing here would catch it
// coming back. Distinguishing a wrong comment from a historical one needs
// intent, and a guard that guesses at intent fails in the direction that
// deletes the history.
func TestNoOperatorFacingStringNamesTheRetiredDatabaseURL(t *testing.T) {
	root := repoRootForEnvScan(t)
	out, err := exec.Command("git", "-C", root, "ls-files", "cmd/**/*.go").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	files := strings.Fields(string(out))
	// A scan that matched nothing reports a clean tree, which reads identically
	// to success. Same guard as the read-scan above it, same reason.
	if len(files) < 20 {
		t.Fatalf("git ls-files matched %d Go files under cmd/; the scan did not see the tree", len(files))
	}

	var offenders []string
	checked := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		checked++
		for _, lit := range stringLiterals(t, filepath.Join(root, f)) {
			if namesTheRetiredVariable(lit.text) {
				offenders = append(offenders, f+":"+lit.pos)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no non-test Go files under cmd/ were parsed; the scan measured nothing")
	}

	if len(offenders) != 0 {
		t.Errorf("these strings tell an operator to set the retired DATABASE_URL: %v\n"+
			"#1904 unified on CLEAT_DATABASE_URL and there is no fallback, so following "+
			"one of these produces a worker that cannot connect -- and the error it then "+
			"prints names the same dead variable.", offenders)
	}
}

type literal struct {
	text string
	pos  string
}

// stringLiterals returns every string literal in a Go file, with its line.
// Parsed rather than grepped: a line scan cannot tell a literal from the
// comment beside it, and this guard's whole correctness rests on that
// distinction.
func stringLiterals(t *testing.T, path string) []literal {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		// Unparseable production Go would fail the build long before this test,
		// so treat it as a scan failure rather than silently skipping the file.
		t.Fatalf("parsing %s: %v", path, err)
	}
	var lits []literal
	ast.Inspect(file, func(n ast.Node) bool {
		bl, ok := n.(*ast.BasicLit)
		if ok && bl.Kind == token.STRING {
			lits = append(lits, literal{
				text: bl.Value,
				pos:  strings.TrimPrefix(fset.Position(bl.Pos()).String(), path+":"),
			})
		}
		return true
	})
	return lits
}

// namesTheRetiredVariable reports whether s contains the BARE DATABASE_URL.
// CLEAT_DATABASE_URL contains it as a substring, so every correct mention would
// match a naive search -- the same trap the read-scan's matcher documents.
func namesTheRetiredVariable(s string) bool {
	const name = "DATABASE_URL"
	for i := 0; ; {
		j := strings.Index(s[i:], name)
		if j < 0 {
			return false
		}
		at := i + j
		// Preceded by an identifier character means it is part of a longer
		// name -- CLEAT_DATABASE_URL, OTHER_DATABASE_URL -- and not this one.
		if at == 0 || !isIdentByte(s[at-1]) {
			return true
		}
		i = at + len(name)
	}
}

func isIdentByte(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

// The matcher must fire on what it forbids and stay silent on what it permits.
// Without this, a matcher that stopped matching would report a clean tree
// forever -- and unlike the read-scan, this one cannot lean on a regex whose
// shape is obvious at a glance.
func TestTheOperatorStringMatcherTellsTheNamesApart(t *testing.T) {
	for _, tc := range []struct {
		src  string
		want bool
		why  string
	}{
		{`"or set DATABASE_URL"`, true, "the instruction this forbids"},
		{`"check the --db flag or DATABASE_URL environment variable"`, true,
			"the error message four call sites printed"},
		{`"DATABASE_URL"`, true, "the bare name alone"},
		{`"or set CLEAT_DATABASE_URL"`, false,
			"the live name CONTAINS the retired one; matching it would report every " +
				"correct string and the scan would be useless"},
		{`"OTHER_DATABASE_URL"`, false, "a different variable ending the same way"},
		{`"MY_DATABASE_URL and DATABASE_URL"`, true,
			"a qualified mention must not mask a bare one later in the same string"},
		{`"no variable here"`, false, "an unrelated string"},
	} {
		if got := namesTheRetiredVariable(tc.src); got != tc.want {
			t.Errorf("namesTheRetiredVariable(%q) = %v, want %v -- %s", tc.src, got, tc.want, tc.why)
		}
	}
}

// The parser must actually separate literals from comments, which is the whole
// basis for cmd/cleat-worker/config.go being out of scope by construction.
// Asserted on a synthetic file rather than on config.go, so that editing
// config.go cannot silently turn this control green for the wrong reason.
func TestTheScanSeesLiteralsAndNotComments(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sample.go")
	src := `package sample

// DATABASE_URL is named here in a comment, as history.
const A = "set CLEAT_DATABASE_URL"
const B = "set DATABASE_URL"
`
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatalf("writing the fixture: %v", err)
	}

	var hits []string
	for _, lit := range stringLiterals(t, path) {
		if namesTheRetiredVariable(lit.text) {
			hits = append(hits, lit.text)
		}
	}
	if len(hits) != 1 || !strings.Contains(hits[0], `set DATABASE_URL`) {
		t.Fatalf("expected exactly the one bad LITERAL, got %v -- if this picked up the "+
			"comment, config.go's historical mentions are in scope and this guard will "+
			"fire on them", hits)
	}
}
