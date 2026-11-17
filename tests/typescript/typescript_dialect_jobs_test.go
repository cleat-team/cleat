package typescript

// Every database-backed TypeScript test runs in BOTH dialect jobs, or this fails (cleat#3988, follow-up to part b).
//
// The MySQL and SQL Server jobs in typescript-tier2.yml run only the tests that need a database, selected BY NAME
// (`-run '^(A|B)$'`), because running the whole package three times would triple the cost and prove nothing new about the
// tests that never touch a database. A by-name list has the failure every by-name list has: a database-backed test added
// later is simply absent from it, runs on PostgreSQL alone, and nothing anywhere says so. That happened within hours:
// cleat#3998 added TestTypeScriptDeferSegmentRunsOnARealWorker, which is database-backed through newWorkerEnv, and neither
// job listed it (cleat-review found it, not a test).
//
// So the list is derived and compared, not trusted. A test is database-backed if it reaches needDatabase through any chain
// of calls to functions in this package, which is read from the SOURCE with go/parser rather than guessed from names. The
// workflow's two lists, the `-run` pattern and the loop that asserts each test passed, must each equal that set.
//
// Adding a database-backed test therefore fails this until it is added to both lists in both jobs, and the failure says so.
// Check that it passes on that dialect first: this guard proves the test is SELECTED, not that it passes.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// databaseBackedTests returns the names of the top-level Test functions in dir that reach needDatabase.
func databaseBackedTests(t *testing.T, dir string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*_test.go"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no test files in %s: %v", dir, err)
	}
	fset := token.NewFileSet()
	calls := map[string]map[string]bool{} // function name -> names it calls
	var tests []string
	for _, f := range files {
		file, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", f, err)
		}
		for _, d := range file.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || fn.Body == nil {
				continue
			}
			set := map[string]bool{}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if c, ok := n.(*ast.CallExpr); ok {
					if id, ok := c.Fun.(*ast.Ident); ok {
						set[id.Name] = true
					}
				}
				return true
			})
			calls[fn.Name.Name] = set
			if strings.HasPrefix(fn.Name.Name, "Test") {
				tests = append(tests, fn.Name.Name)
			}
		}
	}
	// A function is database-backed if it calls needDatabase or calls a database-backed function: a fixed point.
	backed := map[string]bool{"needDatabase": true}
	for changed := true; changed; {
		changed = false
		for fn, callees := range calls {
			if backed[fn] {
				continue
			}
			for c := range callees {
				if backed[c] {
					backed[fn], changed = true, true
					break
				}
			}
		}
	}
	var out []string
	for _, name := range tests {
		if backed[name] {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// diffSets names what is in want and not in got, and the reverse.
func diffSets(want, got []string) (missing, extra []string) {
	w, g := map[string]bool{}, map[string]bool{}
	for _, s := range want {
		w[s] = true
	}
	for _, s := range got {
		g[s] = true
	}
	for s := range w {
		if !g[s] {
			missing = append(missing, s)
		}
	}
	for s := range g {
		if !w[s] {
			extra = append(extra, s)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	return
}

type workflowFile struct {
	Jobs map[string]struct {
		Steps []struct {
			Name string `yaml:"name"`
			Run  string `yaml:"run"`
		} `yaml:"steps"`
	} `yaml:"jobs"`
}

var (
	runPatternRE = regexp.MustCompile(`-run '\^\(([^)]*)\)\$'`)
	passLoopRE   = regexp.MustCompile(`for t in ([A-Za-z0-9_ ]+); do`)
)

// jobLists returns the two name lists one dialect job carries: its -run pattern and its pass-assertion loop.
func jobLists(t *testing.T, wf workflowFile, job string) (runList, loopList []string) {
	t.Helper()
	j, ok := wf.Jobs[job]
	if !ok {
		t.Fatalf("typescript-tier2.yml has no job %q: the guard cannot look at a job that is not there", job)
	}
	for _, s := range j.Steps {
		switch {
		case strings.HasPrefix(s.Name, "Tests ("):
			if m := runPatternRE.FindStringSubmatch(s.Run); m != nil {
				runList = strings.Split(m[1], "|")
			}
		case strings.HasPrefix(s.Name, "Read the result"):
			if m := passLoopRE.FindStringSubmatch(s.Run); m != nil {
				loopList = strings.Fields(m[1])
			}
		}
	}
	if len(runList) == 0 || len(loopList) == 0 {
		t.Fatalf("job %s: could not read its -run list (%d names) and its pass loop (%d names) from the workflow. "+
			"The step names or the shape of those two lines changed, which makes this guard blind, so it fails instead of passing.",
			job, len(runList), len(loopList))
	}
	sort.Strings(runList)
	sort.Strings(loopList)
	return
}

func TestTypeScriptEveryDatabaseBackedTestRunsInBothDialectJobs(t *testing.T) {
	root := repoRoot(t)
	derived := databaseBackedTests(t, filepath.Join(root, "tests", "typescript"))

	// KNOWN-POSITIVE for the derivation: the original worker test must be found. An empty or wrong derivation would make
	// every comparison below pass vacuously.
	const known = "TestTypeScriptRunsAllThreeSuspensionsOnARealWorker"
	if len(derived) == 0 || !strings.Contains(strings.Join(derived, " "), known) {
		t.Fatalf("the derivation found %v, which does not include %s: it is not reading the source correctly", derived, known)
	}

	// KNOWN-POSITIVE for the comparison: a list missing one derived test and carrying one that is not must be reported.
	if missing, extra := diffSets(derived, append(append([]string{}, derived[1:]...), "TestNotARealTest")); len(missing) != 1 || len(extra) != 1 {
		t.Fatalf("the comparison cannot see a missing and an extra name: missing=%v extra=%v", missing, extra)
	}

	raw, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "typescript-tier2.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var wf workflowFile
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		t.Fatalf("parsing typescript-tier2.yml: %v", err)
	}

	var problems []string
	for _, job := range []string{"typescript-tier2-mysql", "typescript-tier2-mssql"} {
		runList, loopList := jobLists(t, wf, job)
		for label, got := range map[string][]string{"-run pattern": runList, "pass loop": loopList} {
			missing, extra := diffSets(derived, got)
			if len(missing) > 0 {
				problems = append(problems, fmt.Sprintf("%s's %s is missing %v: that test needs a database and would run on PostgreSQL only", job, label, missing))
			}
			if len(extra) > 0 {
				problems = append(problems, fmt.Sprintf("%s's %s lists %v, which does not reach needDatabase: it would be selected in a job that gives it no database", job, label, extra))
			}
		}
	}
	if len(problems) > 0 {
		t.Fatalf("the dialect jobs and the database-backed tests disagree (derived from source: %v):\n  - %s\n\n"+
			"Fix the workflow in BOTH jobs and in BOTH places (the -run pattern and the loop that asserts each test passed). "+
			"First run the test on that dialect: this guard proves it is selected, not that it passes.",
			derived, strings.Join(problems, "\n  - "))
	}
}
