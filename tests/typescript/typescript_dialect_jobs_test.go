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
// So the list is derived and compared, not trusted. A test is database-backed if it reaches needDatabase through a chain of
// REFERENCES, read from the SOURCE with go/parser: any identifier mentioned inside a top-level function or inside a
// package-level var's value is an edge, so a helper that is called, passed as a value (`t.Run("x", helper)`) or placed in a
// table of func literals is followed. The workflow's two lists, the `-run` pattern and the loop that asserts each test
// passed, must each equal that set.
//
// WHAT IT CANNOT FOLLOW, AND WHAT IT DOES ABOUT THAT. A method is reached through a selector (`x.env(t)`), and without type
// information a bare name cannot say which method that is: keying methods by name matched 21 unrelated tests on a method
// called `env`, because every test has a local variable of that name. So methods are not followed. Rather than pass over
// them silently, the guard FAILS CLOSED: if a method body, or a package-level var named `_`, mentions a database-backed
// function, the test fails and says it cannot attribute that, so the author lists the test explicitly or restructures.
// Over-matching in the followed direction is loud (a spurious name appears as "missing" from the jobs); under-matching is the
// silent failure, and the tripwire exists for the one place this approach would under-match.
//
// The four shapes that were planted against it (cleat-review on cleat#4006) are fixtures below and must each be caught: a
// direct call, a function passed as a value, a table of func literals, and a method helper.
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

// derivation is what databaseBackedTests found in one directory.
type derivation struct {
	Tests      []string // top-level Test functions that reach needDatabase, sorted
	Unattribut []string // places the guard cannot follow that mention a database-backed function
}

// deriveDatabaseBacked reads every *_test.go in dir and returns the database-backed tests plus the tripwire findings.
func deriveDatabaseBacked(dir string) (derivation, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*_test.go"))
	if err != nil || len(files) == 0 {
		return derivation{}, fmt.Errorf("no test files in %s: %v", dir, err)
	}
	fset := token.NewFileSet()
	refs := map[string]map[string]bool{} // top-level function or var name -> identifiers it mentions
	var tests []string
	type opaque struct {
		where string
		refs  map[string]bool
	}
	var opaques []opaque // methods and blank vars: not followed, but checked

	mentions := func(n ast.Node) map[string]bool {
		set := map[string]bool{}
		ast.Inspect(n, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok {
				set[id.Name] = true
			}
			return true
		})
		return set
	}
	for _, f := range files {
		file, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			return derivation{}, fmt.Errorf("parsing %s: %v", f, err)
		}
		for _, d := range file.Decls {
			switch d := d.(type) {
			case *ast.FuncDecl:
				if d.Body == nil {
					continue
				}
				m := mentions(d.Body)
				if d.Recv != nil {
					opaques = append(opaques, opaque{fmt.Sprintf("method %s in %s", d.Name.Name, filepath.Base(f)), m})
					continue
				}
				refs[d.Name.Name] = m
				if strings.HasPrefix(d.Name.Name, "Test") {
					tests = append(tests, d.Name.Name)
				}
			case *ast.GenDecl:
				if d.Tok != token.VAR {
					continue
				}
				for _, sp := range d.Specs {
					vs := sp.(*ast.ValueSpec)
					m := map[string]bool{}
					for _, v := range vs.Values {
						for k := range mentions(v) {
							m[k] = true
						}
					}
					for _, name := range vs.Names {
						if name.Name == "_" {
							opaques = append(opaques, opaque{fmt.Sprintf("blank var in %s", filepath.Base(f)), m})
						} else {
							refs[name.Name] = m
						}
					}
				}
			}
		}
	}
	// A name is database-backed if it is needDatabase or mentions a database-backed name: a fixed point.
	backed := map[string]bool{"needDatabase": true}
	for changed := true; changed; {
		changed = false
		for name, m := range refs {
			if backed[name] {
				continue
			}
			for r := range m {
				if backed[r] {
					backed[name], changed = true, true
					break
				}
			}
		}
	}
	var out derivation
	for _, name := range tests {
		if backed[name] {
			out.Tests = append(out.Tests, name)
		}
	}
	sort.Strings(out.Tests)
	for _, o := range opaques {
		for r := range o.refs {
			if backed[r] {
				out.Unattribut = append(out.Unattribut, fmt.Sprintf("%s mentions %s", o.where, r))
				break
			}
		}
	}
	sort.Strings(out.Unattribut)
	return out, nil
}

// databaseBackedTests is deriveDatabaseBacked for the real tree; a tripwire finding fails the test.
func databaseBackedTests(t *testing.T, dir string) []string {
	t.Helper()
	d, err := deriveDatabaseBacked(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Unattribut) > 0 {
		t.Fatalf("the guard cannot attribute a database-backed function reached from a method or a blank var, so a test behind "+
			"it could be missing from the dialect jobs without this noticing:\n  - %s\n"+
			"List the test explicitly in both dialect jobs and keep the database call out of methods and blank vars.",
			strings.Join(d.Unattribut, "\n  - "))
	}
	return d.Tests
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

// TestTypeScriptDialectGuardSeesEveryPlantedShape holds the derivation to the shapes cleat-review planted against it on
// cleat#4006. Each is a new test that reaches needDatabase through the same entry the real worker tests use (newWorkerEnv),
// in a throwaway directory with stand-ins for needDatabase and newWorkerEnv, so nothing here depends on the real tests.
//
// A guard that has only ever been run on a tree where it passes has not been shown to be able to fail. The first version of
// this one followed direct calls only and passed on three of these four shapes.
func TestTypeScriptDialectGuardSeesEveryPlantedShape(t *testing.T) {
	const base = `package typescript

import "testing"

func needDatabase(t *testing.T) {}

type workerEnv struct{}

func newWorkerEnv(t *testing.T) *workerEnv { needDatabase(t); return &workerEnv{} }
`
	cases := []struct {
		name       string
		plant      string
		wantTests  []string // must be derived
		wantTrip   bool     // must trip the fail-closed check
		wantAbsent []string // must NOT be derived
	}{
		{"direct call", `func TestPlant(t *testing.T) { newWorkerEnv(t) }`, []string{"TestPlant"}, false, nil},
		{"a function passed as a value", `
func runPlant(t *testing.T) { newWorkerEnv(t) }
func TestPlant(t *testing.T) { t.Run("x", runPlant) }`, []string{"TestPlant"}, false, nil},
		{"a table of func literals", `
var plantCases = []struct{ f func(*testing.T) }{{f: func(t *testing.T) { newWorkerEnv(t) }}}
func TestPlant(t *testing.T) { for _, c := range plantCases { c.f(t) } }`, []string{"TestPlant"}, false, nil},
		{"a method helper", `
type plantSuite struct{}
func (plantSuite) env(t *testing.T) *workerEnv { return newWorkerEnv(t) }
func TestPlant(t *testing.T) { plantSuite{}.env(t) }`, nil, true, nil},
		{"a blank var that reaches the database", `var _ = func(t *testing.T) { newWorkerEnv(t) }`, nil, true, nil},
		// NEGATIVE CONTROLS: the guard must not pay for catching the above by flagging what is not database-backed.
		{"a test that never touches a database", `func TestPlain(t *testing.T) { t.Log("x") }`, nil, false, []string{"TestPlain"}},
		{"a method named env beside tests that have a local env", `
type other struct{}
func (other) env(t *testing.T) int { return 1 }
func TestPlain(t *testing.T) { env := 1; _ = env }`, nil, false, []string{"TestPlain"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "base_test.go"), []byte(base), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "plant_test.go"), []byte("package typescript\n\nimport \"testing\"\n"+c.plant+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			d, err := deriveDatabaseBacked(dir)
			if err != nil {
				t.Fatal(err)
			}
			if missing, _ := diffSets(c.wantTests, d.Tests); len(missing) > 0 {
				t.Errorf("the derivation did not find %v (found %v): a test like this would be missing from the jobs and nothing would say so", missing, d.Tests)
			}
			for _, absent := range c.wantAbsent {
				for _, got := range d.Tests {
					if got == absent {
						t.Errorf("%s was derived as database-backed but never touches a database", absent)
					}
				}
			}
			if c.wantTrip != (len(d.Unattribut) > 0) {
				t.Errorf("fail-closed check: want tripped=%v, got findings %v", c.wantTrip, d.Unattribut)
			}
		})
	}
}
