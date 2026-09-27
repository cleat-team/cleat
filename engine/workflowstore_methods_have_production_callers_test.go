package engine_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// A WorkflowStore method no production code calls is a method whose tests
// prove nothing about production. cleat#1021.
//
// WHY NEITHER EXISTING GUARD CATCHES THIS. scripts/check-test-only-code.sh
// runs staticcheck U1000 with -tests=false, and U1000 deliberately does not
// report exported identifiers in library packages -- a public API may have
// callers outside the module. scripts/check-dead-exports.sh catches "nothing
// calls it, including tests", which these fail: their tests call them, often
// heavily. So an interface method reachable only from tests and benchmarks
// sits in the blind spot between the two, and none of the names below appears
// in any existing baseline.
//
// WHAT "PRODUCTION" MEANS HERE, stated because it is the whole question.
// A caller in engine/ or cmd/cleat-worker/ is production. A caller in
// cmd/cleat-bench/ is NOT: a benchmark exercises a method the worker never
// reaches, so a test asserting that method's behaviour is a statement about
// the benchmark. That distinction is what cleat#1019 turned on, and
// CompleteWorkflow is its confirmed instance -- 23 tests, and the only
// non-test caller is the benchmark.
//
// TWO EXCLUSIONS, both from the issue's method. Without them the answer is a
// useless "nothing is unreachable":
//
//   - the file DECLARING an implementation is not a user of it, and
//   - engine/sharded_store.go is pure delegation; it calls every method by
//     construction and so vouches for all of them.
func TestEveryWorkflowStoreMethodHasAProductionCaller(t *testing.T) {
	methods := workflowStoreMethods(t)
	if len(methods) < 50 {
		t.Fatalf("found only %d WorkflowStore methods; the interface scan is "+
			"broken and this guard would pass vacuously", len(methods))
	}

	callers := productionCallers(t, methods)

	var missing []string
	for _, m := range methods {
		if len(callers[m]) == 0 {
			missing = append(missing, m)
		}
	}
	sort.Strings(missing)

	// The allow-list. Entries may be REMOVED as methods gain callers; adding
	// one needs a reason, because it means shipping an interface method the
	// worker does not reach.
	allowed := map[string]string{
		"CompleteWorkflow": "cleat#1019: bench-only. finalize_workflow_status is the " +
			"procedure twin the worker actually uses.",
		"UpdateStickyWorker":      "bench-only.",
		"GetWorkflowTag":          "no caller at all outside tests.",
		"StreamEventHistory":      "no caller at all outside tests.",
		"VerifyWorkflowEvents":    "no caller at all outside tests.",
		"ResolveTenantFromAPIKey": "called from auth/middleware.go, which is production but is neither engine/ nor cmd/cleat-worker/.",
	}

	for _, m := range missing {
		if _, ok := allowed[m]; !ok {
			t.Errorf("WorkflowStore.%s has no caller in engine/ or cmd/cleat-worker/.\n\n"+
				"THE RULE, so you can disagree with it rather than only with this "+
				"result: a caller is a call to the method from OUTSIDE its own\n"+
				"function body, in a non-test file under engine/ or\n"+
				"cmd/cleat-worker/, excluding engine/sharded_store.go (pure\n"+
				"delegation, it calls everything).\n\n"+
				"Sibling delegation DOES count -- AppendEventHistory calling\n"+
				"AppendEventHistoryBatch on the same receiver in the same file is a\n"+
				"real path to production, and an earlier version of this rule that\n"+
				"excluded the whole defining file reported that method unreachable\n"+
				"when the worker reaches it on every write.\n\n"+
				"A test of an uncalled method asserts nothing about production. "+
				"Either wire it up, delete it, or add it to the allow-list in this "+
				"test with the reason.", m)
		}
	}

	// The ratchet only tightens. An allow-list entry that has gained a caller
	// is a stale exemption, and a stale exemption is how a guard stops being
	// one -- it would keep passing while silently permitting the next method
	// to go uncalled under the same name.
	missingSet := map[string]bool{}
	for _, m := range missing {
		missingSet[m] = true
	}
	for m, why := range allowed {
		if !missingSet[m] {
			t.Errorf("WorkflowStore.%s is on the allow-list (%q) but now HAS a "+
				"production caller. Remove the entry.", m, why)
		}
	}
}

// workflowStoreMethods reads the interface with Go's parser.
//
// Not a brace-matched or line-counted scan: a signature spanning lines, or an
// embedded interface, is invisible to those and the count comes out low --
// which makes the guard pass rather than fail.
func workflowStoreMethods(t *testing.T) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "store_interface.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing store_interface.go: %v", err)
	}
	var names []string
	ast.Inspect(f, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok || ts.Name.Name != "WorkflowStore" {
			return true
		}
		it, ok := ts.Type.(*ast.InterfaceType)
		if !ok {
			return true
		}
		for _, m := range it.Methods.List {
			if len(m.Names) == 0 {
				t.Errorf("WorkflowStore embeds an interface (%v); this scan counts "+
					"only directly declared methods and would miss its members", m.Type)
				continue
			}
			for _, nm := range m.Names {
				names = append(names, nm.Name)
			}
		}
		return false
	})
	sort.Strings(names)
	return names
}

// productionCallers maps each method to the files that call it.
//
// The scan is AST-based rather than textual, so a method named in a comment or
// inside a string literal is not a caller. That matters here: this repository
// has had several findings turn on a text search matching prose about a symbol
// rather than a use of it.
func productionCallers(t *testing.T, methods []string) map[string][]string {
	t.Helper()

	want := map[string]bool{}
	for _, m := range methods {
		want[m] = true
	}

	out := map[string][]string{}
	fset := token.NewFileSet()

	for _, path := range repoGoFiles(t) {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		if path == "engine/sharded_store.go" {
			continue // pure delegation: it calls everything
		}
		dir := filepath.Dir(path) + "/"
		if !strings.HasPrefix(dir, "engine/") && !strings.HasPrefix(dir, "cmd/cleat-worker/") {
			continue
		}

		// git ls-files yields repo-root-relative paths; this test runs in
		// engine/. Without the prefix nothing parses, every method looks
		// uncalled, and the guard fails loudly rather than passing -- which
		// is the right direction for the mistake, but it is still a mistake.
		f, err := parser.ParseFile(fset, filepath.Join("..", path), nil, 0)
		if err != nil {
			continue // not parseable in isolation; not this guard's subject
		}

		// Walk each top-level function separately, so "an implementation is
		// not a user of itself" can be applied to the METHOD rather than to
		// the whole file.
		//
		// The file-level version of this exclusion is wrong, and measurably:
		// AppendEventHistory delegates to AppendEventHistoryBatch on the same
		// receiver, in the same file, on all three dialects. Excluding the
		// file drops those and reports AppendEventHistoryBatch unreachable --
		// but the worker calls AppendEventHistory, so it reaches the batch
		// method every time. A sibling calling a sibling is a real caller;
		// only the method's own body is not.
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok {
				continue
			}
			ast.Inspect(fd, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				name := sel.Sel.Name
				if !want[name] {
					return true
				}
				if fd.Recv != nil && fd.Name.Name == name {
					return true // recursion inside its own implementation
				}
				out[name] = append(out[name], path)
				return true
			})
		}
	}
	return out
}

// repoGoFiles lists tracked and untracked-but-not-ignored Go files.
//
// git ls-files rather than filepath.Walk: .claude/worktrees/ holds whole
// copies of this repository, and a walk descends into them -- attributing a
// caller to a file that exists only in a scratch checkout. That is a scope
// mistake rather than a parsing one, and it makes a guard MORE likely to pass
// as the working tree gets messier.
func repoGoFiles(t *testing.T) []string {
	t.Helper()
	cmd := exec.Command("git", "ls-files", "--cached", "--others", "--exclude-standard", "*.go")
	cmd.Dir = ".."
	b, err := cmd.Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	var files []string
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if line != "" {
			files = append(files, line)
		}
	}
	if len(files) == 0 {
		t.Fatal("git ls-files returned nothing; the guard would pass vacuously")
	}
	return files
}
