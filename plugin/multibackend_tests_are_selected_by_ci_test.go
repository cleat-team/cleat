package plugin_test

import (
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

// A test that asks testutil.NewPluginTestBackends for every dialect must be
// SELECTED by the only CI job that supplies MySQL and SQL Server DSNs. Both, or
// it runs on PostgreSQL and reports green for three.
//
// cleat#1174. TestReaperTouchesNothingItShouldNot shipped in #1150 without the
// `_MultiBackend` suffix, and multi-db-ci.yml selects that job's work by name:
//
//	go test -count=1 -timeout=900s -run 'MultiBackend' ./plugins/...
//
// ci.yml's package matrix does run ./plugins/... unfiltered, but sets only
// CLEAT_TEST_DB, so the excluded test got PostgreSQL and nothing else. Its own
// docstring says it is the negative control without which the positive test
// passes against `UPDATE task_queue SET status = 'pending', started_at = NULL`
// -- no WHERE clause at all. That was the state on MySQL and SQL Server, and
// the SQL Server arm is the three-column join #1143 had just rewritten, which
// is the shape where over-reaping is easiest to introduce.
//
// THIS IS THE SECOND INSTANCE OF ONE SHAPE, which is why it is a test rather
// than a note. engine/tier2_toolchain_tests_placement_test.go exists because
// "TestARust" does not contain "TestRust", so three tests ran in no job at all.
// Same mechanism: a -run pattern is a naming contract that nothing enforces,
// and `go test` reports an unmatched -run as success.
//
// The pattern is READ FROM THE WORKFLOW rather than written here. A guard that
// hardcodes 'MultiBackend' passes the day someone changes the filter, which is
// the one day it needed to fail.
func TestEveryMultiBackendTestIsSelectedByTheMultiDialectJob(t *testing.T) {
	root := repoRoot(t)

	selectors := pluginsSelectors(t, root)
	if len(selectors) == 0 {
		t.Fatal("no `go test ... ./plugins/...` invocation in .github/workflows carries a " +
			"-run pattern.\n\n" +
			"Either the multi-dialect plugin job stopped filtering -- in which case delete " +
			"this guard, the contract it enforces is gone -- or it stopped running " +
			"./plugins/... at all, which is worse and is what this would be telling you.")
	}

	tests := multiBackendTests(t, root)
	if len(tests) < 4 {
		t.Fatalf("found only %d test(s) calling testutil.NewPluginTestBackends; there were 6 "+
			"when this guard was written (2026-09-10) across featureflags, jobqueue, kvstore "+
			"and scheduler.\n\n"+
			"A floor rather than a count, because the population grows: this fires when the "+
			"detection breaks, not when someone adds a plugin.", len(tests))
	}

	var unselected []string
	for _, fn := range tests {
		if !matchesAnySelector(fn.name, selectors) {
			unselected = append(unselected, fn.name+"  ("+fn.file+")")
		}
	}
	if len(unselected) > 0 {
		sort.Strings(unselected)
		t.Errorf("%d of %d multi-backend test(s) are not selected by the job that supplies "+
			"the MySQL and SQL Server DSNs:\n\n  %s\n\n"+
			"They ask NewPluginTestBackends for three dialects and CI gives them one. Nothing "+
			"reports it: an unmatched -run is not an error, and the test still passes on "+
			"PostgreSQL in ci.yml's package matrix.\n\n"+
			"Fix by naming, not by widening the filter -- the suffix is what the workflow "+
			"selects on. Patterns in force: %v",
			len(unselected), len(tests), strings.Join(unselected, "\n  "), selectorSources(selectors))
	}
}

type multiBackendTest struct{ name, file string }

// multiBackendTests finds every top-level function that references
// testutil.NewPluginTestBackends, via go/ast rather than a text search.
//
// It fails rather than skipping when the reference sits outside a Test
// function. That case is a shared helper, which this detection cannot follow --
// and a guard that quietly drops what it cannot parse under-reports, which is
// the direction that flatters the number and so the one that survives.
func multiBackendTests(t *testing.T, root string) []multiBackendTest {
	t.Helper()

	// git ls-files, not filepath.Walk: .claude/worktrees/ holds whole extra
	// copies of this repo, and attributing a test to a scratch checkout is a
	// scope mistake that gets MORE likely as the working tree gets messier.
	out, err := exec.Command("git", "-C", root, "ls-files", "plugins/**/*_test.go").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	files := strings.Fields(string(out))
	if len(files) == 0 {
		t.Fatal("git ls-files matched no test files under plugins/")
	}

	var found []multiBackendTest
	fset := token.NewFileSet()
	for _, rel := range files {
		f, err := parser.ParseFile(fset, filepath.Join(root, rel), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", rel, err)
		}
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			uses := false
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if ok && sel.Sel.Name == "NewPluginTestBackends" {
					uses = true
					return false
				}
				return true
			})
			if !uses {
				continue
			}
			if !strings.HasPrefix(fn.Name.Name, "Test") {
				t.Fatalf("%s calls NewPluginTestBackends from %s, which is not a Test "+
					"function.\n\nThis guard attributes a backend set to the test whose body "+
					"names it. A helper breaks that, and the failure would be silent -- the "+
					"tests calling the helper would drop out of the census and the guard "+
					"would pass. Extend it to follow the call before adding one.",
					rel, fn.Name.Name)
			}
			found = append(found, multiBackendTest{name: fn.Name.Name, file: rel})
		}
	}
	return found
}

type selector struct {
	re   *regexp.Regexp
	src  string
	expr string
}

func selectorSources(ss []selector) []string {
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		out = append(out, s.src+": -run "+s.expr)
	}
	return out
}

func matchesAnySelector(name string, ss []selector) bool {
	for _, s := range ss {
		if s.re.MatchString(name) {
			return true
		}
	}
	return false
}

var runPattern = regexp.MustCompile(`-run[= ]+'([^']+)'|-run[= ]+"([^"]+)"|-run[= ]+([^\s]+)`)

// pluginsSelectors returns the -run patterns of every workflow command that
// runs ./plugins/....
//
// Continuations are joined and comments dropped BEFORE the command is examined.
// A `-run` on a continuation line otherwise reads as absent, and absent means
// "selects everything" -- which is the permissive direction, so it would make
// this guard pass a tree it should fail (#749 shipped exactly that bug three
// times). A `#` line mentioning go test otherwise reads as an invocation (#748,
// the over-reporting mirror).
func pluginsSelectors(t *testing.T, root string) []selector {
	t.Helper()

	dir := filepath.Join(root, ".github", "workflows")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}

	var out []selector
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yml") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		for _, cmd := range joinedCommands(string(b)) {
			if !strings.Contains(cmd, "./plugins/...") || !strings.Contains(cmd, "go test") {
				continue
			}
			m := runPattern.FindStringSubmatch(cmd)
			if m == nil {
				// Unfiltered: every multi-backend test is selected here, so
				// the contract is satisfied by construction. Represent that
				// rather than dropping the command, or an unfiltered run would
				// look like no run at all.
				out = append(out, selector{re: regexp.MustCompile(``), src: e.Name(), expr: "(unfiltered)"})
				continue
			}
			expr := m[1] + m[2] + m[3]
			re, err := regexp.Compile(expr)
			if err != nil {
				t.Fatalf("%s: -run %q does not compile: %v", e.Name(), expr, err)
			}
			out = append(out, selector{re: re, src: e.Name(), expr: expr})
		}
	}
	return out
}

// joinedCommands splits a workflow into logical shell commands: comments
// removed, backslash-continuations joined.
func joinedCommands(src string) []string {
	var cmds []string
	var cur strings.Builder
	for _, line := range strings.Split(src, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.HasSuffix(trimmed, `\`) {
			cur.WriteString(strings.TrimSuffix(trimmed, `\`))
			cur.WriteString(" ")
			continue
		}
		cur.WriteString(trimmed)
		cmds = append(cmds, cur.String())
		cur.Reset()
	}
	if cur.Len() > 0 {
		cmds = append(cmds, cur.String())
	}
	return cmds
}

func repoRoot(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatalf("git rev-parse --show-toplevel: %v", err)
	}
	return strings.TrimSpace(string(out))
}
