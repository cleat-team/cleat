//go:build ignore

// Command findblinddoubles reports test doubles that cannot see the argument
// their test is named after.
//
// The defect it detects, from cleat#1167. A handler stopped passing
// Idempotency-Key to the store, so every retry re-drove work that had already
// failed partway. Two tests asserted idempotency and both passed on the broken
// code, because the double they drove looked like this:
//
//	ms.startNewRunFn = func(_ context.Context, runID, defName string, defVersion int,
//		input json.RawMessage, idempotencyKey, tenantID string, priority int) (string, bool, error) {
//		return "wf-existing", true, nil
//	}
//
// It names idempotencyKey and never reads it, and answers "already started" no
// matter what it is handed. A test driving it asserts that the HANDLER RENDERS
// the already-started branch, never that anything can reach it. Replacing the
// handler's header read with "" left both tests green.
//
// Rule, deliberately narrow:
//
//	report a function literal inside a Test function when it declares a named
//	parameter it never references, AND that parameter's name appears in the
//	name of the enclosing Test function.
//
// The second clause is what keeps this useful. Doubles ignore arguments all the
// time and are right to -- a listVersionsFn that ignores defName is fine when
// the test is not about defName. But when the test is CALLED
// TestAPIStartWorkflow_WithIdempotencyKey and its double cannot see
// idempotencyKey, the test's subject and the double's blindness are the same
// identifier, and the test cannot fail for the reason it exists. That is a
// defect regardless of what it asserts.
//
// Matching is case-insensitive with underscores removed on both sides, so
// `idempotencyKey` matches `..._WithIdempotencyKey` and `_with_idempotency_key`.
//
// Known limits, stated because a checker that hides its denominator is the
// thing this file exists to prevent:
//
//   - It cannot see a double whose test is named for the CONCEPT rather than
//     the parameter. TestHandleDeadLetterReprocess_AlreadyExisted had exactly
//     this defect and is not reported here, because "AlreadyExisted" shares no
//     identifier with "idempotencyKey".
//   - A parameter referenced anywhere in the body counts as seen, even if only
//     logged. Reading is not the same as deciding, and this cannot tell them
//     apart.
//   - Only function literals lexically inside a `func Test...` are considered.
//     A double built by a helper outside a test function is not reached.
//
// So a clean run means "none of THIS shape", not "no blind doubles". The
// complementary check is to sabotage the read and run the suite; see
// CONTRIBUTING.md.
//
// Output, one finding per line, tab-separated:
//
//	<file>	<line>	<test-func>	<ignored-param>
//
// Build-tagged `ignore` so `go build ./...` and `go vet ./...` never see it,
// matching scripts/finddeadexports.go.
package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// tokens splits an identifier into lowercased words on CamelCase boundaries,
// underscores and digits, so TestAPIStartWorkflow_WithIdempotencyKey becomes
// {test, api, start, workflow, with, idempotency, key}.
//
// Matching is on whole tokens, never substrings, and that distinction carries
// the checker's whole precision. A substring rule reported `version` against
// TestListVersions_All twelve times across cmd/cleatctl -- the parameter is
// spelled inside the plural "Versions" in the test's name while having nothing
// to do with the test's subject. Whole-token matching removes every one of
// those without losing a single real finding.
func tokens(s string) []string {
	var out []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			out = append(out, strings.ToLower(string(cur)))
			cur = nil
		}
	}
	rs := []rune(s)
	for i, r := range rs {
		switch {
		case r == '_' || r == '-':
			flush()
		case r >= '0' && r <= '9':
			flush()
		case r >= 'A' && r <= 'Z':
			// A capital starts a new word, except inside a run of capitals
			// (an acronym like API) that is not followed by a lowercase.
			if i > 0 && !(rs[i-1] >= 'A' && rs[i-1] <= 'Z') {
				flush()
			} else if i+1 < len(rs) && rs[i+1] >= 'a' && rs[i+1] <= 'z' {
				flush()
			}
			cur = append(cur, r)
		default:
			cur = append(cur, r)
		}
	}
	flush()
	return out
}

// namedFor reports whether every token of the parameter name appears as a whole
// token in the test's name. A multi-word parameter like idempotencyKey must
// have both "idempotency" and "key" present, which is what distinguishes
// TestAPIStartWorkflow_WithIdempotencyKey from a test that merely mentions a key.
func namedFor(testName, param string) bool {
	have := map[string]bool{}
	for _, t := range tokens(testName) {
		have[t] = true
	}
	pt := tokens(param)
	if len(pt) == 0 {
		return false
	}
	for _, t := range pt {
		if !have[t] {
			return false
		}
	}
	return true
}

type finding struct {
	file  string
	line  int
	test  string
	param string
}

// testFiles lists the repo's _test.go files, tracked or newly written.
//
// git ls-files, not filepath.Walk. .claude/worktrees/ holds whole extra copies
// of this repository, and a walk descends into every one of them: on a machine
// with two agent worktrees this reported 22 findings, ALL of them from scratch
// checkouts and none from the tree being checked, and exited 1. That makes the
// gate fail locally and pass in CI -- the same shape as cleat#1244, and the
// shape that teaches everyone to ignore it.
//
// CLAUDE.md states the rule and the reason: "Prefer git ls-files over
// rglob/find for anything that reasons about 'the repo'", because a scope
// mistake gets MORE likely as the working tree gets messier. .gitignore already
// carries .claude/worktrees/, so --exclude-standard honours it for free.
//
// --others as well as --cached, so a test file that has just been written is
// scanned before it is added. Invisible-until-staged is the permissive
// direction for a gate whose whole job is to catch a double as it is written.
func testFiles(root string) ([]string, error) {
	cmd := exec.Command("git", "-C", root, "ls-files",
		"--cached", "--others", "--exclude-standard", "*_test.go")
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var files []string
	for _, rel := range strings.Fields(string(out)) {
		// Vendored and generated trees are not ours to police.
		if strings.Contains(rel, "/vendor/") || strings.HasPrefix(rel, "vendor/") ||
			strings.Contains(rel, "/node_modules/") || strings.HasPrefix(rel, "node_modules/") ||
			strings.Contains(rel, "/testdata/") || strings.HasPrefix(rel, "testdata/") {
			continue
		}
		files = append(files, filepath.Join(root, rel))
	}
	return files, nil
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: findblinddoubles <dir>...")
		os.Exit(2)
	}
	var findings []finding
	for _, root := range os.Args[1:] {
		files, err := testFiles(root)
		if err != nil {
			fmt.Fprintf(os.Stderr, "enumerate %s: %v\n", root, err)
			os.Exit(1)
		}
		for _, path := range files {
			findings = append(findings, scanFile(path)...)
		}
	}
	sort.Slice(findings, func(i, j int) bool {
		if findings[i].file != findings[j].file {
			return findings[i].file < findings[j].file
		}
		return findings[i].line < findings[j].line
	})
	for _, f := range findings {
		fmt.Printf("%s\t%d\t%s\t%s\n", f.file, f.line, f.test, f.param)
	}
}

func scanFile(path string) []finding {
	fset := token.NewFileSet()
	af, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "parse %s: %v\n", path, err)
		return nil
	}
	var out []finding
	for _, decl := range af.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil || !strings.HasPrefix(fn.Name.Name, "Test") {
			continue
		}
		testName := fn.Name.Name
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			lit, ok := n.(*ast.FuncLit)
			if !ok || lit.Body == nil {
				return true
			}
			for _, p := range ignoredParams(lit) {
				// Only a parameter the test is NAMED for. A short name would
				// match far too much ("id" is a token in a great many test
				// names), so require something long enough to be a subject.
				if len(p) < 5 {
					continue
				}
				if namedFor(testName, p) {
					out = append(out, finding{
						file: path, line: fset.Position(lit.Pos()).Line,
						test: fn.Name.Name, param: p,
					})
				}
			}
			return true
		})
	}
	return out
}

// ignoredParams returns the names of parameters the literal declares and never
// references in its body. Blank (`_`) parameters are already explicitly
// disclaimed by the author and are not reported.
func ignoredParams(lit *ast.FuncLit) []string {
	declared := map[string]bool{}
	if lit.Type.Params != nil {
		for _, field := range lit.Type.Params.List {
			for _, name := range field.Names {
				if name.Name != "_" && name.Name != "" {
					declared[name.Name] = true
				}
			}
		}
	}
	if len(declared) == 0 {
		return nil
	}
	used := map[string]bool{}
	ast.Inspect(lit.Body, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok {
			used[id.Name] = true
		}
		return true
	})
	var out []string
	for name := range declared {
		if !used[name] {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}
