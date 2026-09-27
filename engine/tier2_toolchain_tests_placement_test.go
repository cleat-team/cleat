package engine

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// A test that builds a tier-2 language's toolchain must be excluded from the
// tier-1 gate and selected by the job that provisions that toolchain. Both, or
// it is in the wrong place. IMPROVEMENT-PLAN 3.109.
//
// The sharper of the two failures it prevents is a test that runs NOWHERE.
// `TestARustGuestSuspendsCleanly`, `TestARustGuestReachesTheHostRetryLoop` and
// `TestARustLongRetryPolicySuspendsInsteadOfHoldingTheWorker` were excluded
// from the tier-1 gate as tier 2, skipped in test-go/engine for want of cargo,
// and NOT selected by e2e-cross-language.yml -- the only job that installs the
// Rust toolchain -- because its -run is
// "TestRust|TestPython|TestAssemblyScript|TestJava" and "TestARust" does not
// contain "TestRust". Every job green, three tests unrun, including 3.87's
// suspend probe from the day before. `go test -skip` leaves no --- SKIP line
// and an unmatched -run leaves nothing at all, so nothing anywhere said so.
//
// The other failure is a tier-1 gate paying for a tier-2 toolchain. It stopped
// being a red when 3.108 (WS-1) gave scripts/tier-gate.sh an explicit
// -timeout=30m -- that fix is not this one's and is not undone here -- but the
// cost is still real and still in the wrong gate. `tier1-gate.yml` says of
// itself:
//
//	What this job deliberately does NOT provide: Rust, Node, a JDK or Gradle.
//
// That is a statement about the job's own setup steps, and the GitHub runner
// image makes it false: node, npm, java and gradle are all preinstalled. So
// every toolchain-gated test runs there anyway, and `npm ci` is not a small
// thing. Measured 2026-09-03 against the old 10-minute default, one test each
// run, whichever AssemblyScript test happened to go first:
//
//	#644's merge   TestAssemblyScriptWorkflowDefersRun         7.55s   engine 375.850s  ok
//	later merge    TestAssemblyScriptWorkflowDefersRun       137.27s   engine 501.455s  ok
//	#647           TestAssemblyScriptWorkflowDefersRun       263.22s   engine 600.053s  TIMEOUT
//	#651           TestAssemblyScriptWorkflowDefersRun       426.84s   engine 600.053s  TIMEOUT
//	#652           TestAssemblyScriptDeferSegmentRunsOnly... 272.87s   engine 600.050s  TIMEOUT
//
// `fail=0` on every one of those: no test failed, the package ran out of time.
// 3.108 fixed the timing out. It did not make a gate that decides whether tier
// 1 is releasable stop spending four hundred seconds on a language tiers.yaml
// puts in tier 2, and that is what the exclusions below are for.
//
// Note what the row order shows. The cost is `npm ci`, paid once per package
// run by whichever AssemblyScript test runs first -- so it MOVES between tests
// as tests are added, and on #652 it landed on a test added by that very PR.
// Reading the slowest test as "the test that got slow" would have been wrong
// every time.
//
// The exclusion list caught `^TestRustWorkflow` (a prefix, so all five) and
// `^TestAssemblyScriptWorkflowExecute$` and `^TestJavaWorkflowExecute$` (exact,
// so one each). Everything else in those two languages ran. That is the
// difference between excluding a language and excluding a test that was
// noticed.
//
// The other half is the half that makes this a guard rather than a delete.
// Five of these tests matched NEITHER the old exclusions NOR
// e2e-cross-language.yml's `-run` selector, because that selector keys on a
// name prefix and they are named for what they do rather than for their
// language: TestTheHostRunsDefersOfAKilledJavaWorkflow,
// TestTheHostRunsDefersOfAKilledAssemblyScriptWorkflow,
// TestAMissingDeferExportIsNotReportedAsAFailure,
// TestTheBackendLogsToTheConfiguredLogger and
// TestTheWazeroPathRunsDefersOfATrappedWorkflow. The tier-1 gate was the only
// place they ran at all. Excluding them there without widening the selector
// would have retired five tests while every job stayed green -- which is the
// exact shape tiers.yaml's own `exclude_tests` comment warns about, since
// `go test -skip` removes a test with no `--- SKIP` line to notice.
//
// So this asserts both directions, and derives the test set from the source
// rather than from a list someone maintains. A sixth toolchain test added
// tomorrow fails here, in a job that needs no toolchain to run.
func TestEveryToolchainBuildingTestIsExcludedFromTier1AndRunByTheE2EJob(t *testing.T) {
	// Reachability, not a direct call: a test can reach the builder through a
	// helper. engine/java_defer_segment_e2e_test.go's javaDeferSegment is one,
	// and a direct-call check would have declared its two tests toolchain-free
	// and left them in the tier-1 gate -- which is how they got there.
	builders := map[string]string{
		"buildJavaWasm":           "java",
		"buildAssemblyScriptWasm": "assemblyscript",
		"buildRustWasm":           "rust",
	}

	calls, err := callGraphOfEngineTests()
	if err != nil {
		t.Fatalf("parsing the engine test sources: %v", err)
	}

	reaches := map[string]string{} // func name -> language
	for fn := range calls {
		if lang := reachesABuilder(fn, calls, builders, map[string]bool{}); lang != "" {
			reaches[fn] = lang
		}
	}

	var tests []string
	for fn, lang := range reaches {
		if strings.HasPrefix(fn, "Test") {
			tests = append(tests, fn+"\t"+lang)
		}
	}
	sort.Strings(tests)

	// A guard that finds nothing to guard is the vacuous-pass shape this file
	// is about, so the scan is checked before its results are used.
	//
	// PER LANGUAGE, not just a total, and that distinction was measured rather
	// than reasoned. The first version of this asserted `len(tests) >= 8`;
	// renaming buildAssemblyScriptWasm in the map above -- which drops every
	// AssemblyScript test from the scan, nine of them -- left eight Java and
	// Rust tests behind and the guard printed ok. A floor that a whole language
	// can disappear underneath is the same slack this file's own subject is
	// about, caught by falsifying the guard instead of trusting it.
	perLanguage := map[string]int{}
	for _, entry := range tests {
		perLanguage[strings.SplitN(entry, "\t", 2)[1]]++
	}
	for _, lang := range []string{"java", "assemblyscript", "rust"} {
		if perLanguage[lang] == 0 {
			t.Fatalf("the source scan found no %s toolchain tests at all (found %v).\n\n"+
				"There are several of each in engine/. Zero means the builder "+
				"function was renamed and the map at the top of this test was "+
				"not, so every %s test is now invisible to both checks below "+
				"and they pass vacuously for it.", lang, perLanguage, lang)
		}
	}
	if len(tests) < 12 {
		t.Fatalf("only %d toolchain-building tests found (%v), expected at least 12.\n\n"+
			"The source scan is broken, and a broken scan passes both checks "+
			"below vacuously.", len(tests), tests)
	}

	excl, err := tier1Exclusions()
	if err != nil {
		t.Fatalf("reading tiers.yaml: %v", err)
	}
	e2e, err := e2eSelector()
	if err != nil {
		t.Fatalf("reading e2e-cross-language.yml: %v", err)
	}

	for _, entry := range tests {
		name := strings.SplitN(entry, "\t", 2)[0]
		lang := strings.SplitN(entry, "\t", 2)[1]

		if !matchesAny(name, excl) {
			t.Errorf("%s builds the %s toolchain but is not excluded from the "+
				"tier-1 gate.\n\n"+
				"tier1-gate.yml provisions no %s toolchain and tiers.yaml's tier 1 "+
				"declares languages [go, python] -- but the GitHub runner image "+
				"preinstalls node/java/gradle, so this test RUNS there rather "+
				"than skipping, and spends the tier-1 gate's budget on a tier-2 "+
				"language. Add an anchored pattern to tiers.yaml exclude_tests, "+
				"and add the name to e2e-cross-language.yml's -run in the same "+
				"change.", name, lang, lang)
		}
		if !e2e.MatchString(name) {
			t.Errorf("%s builds the %s toolchain but e2e-cross-language.yml's "+
				"-run selector does not match it.\n\n"+
				"That job is the only one that provisions %s. If this test is "+
				"also excluded from the tier-1 gate -- and it must be -- then it "+
				"runs NOWHERE, silently, because `go test -skip` leaves no "+
				"--- SKIP line to notice. Widen the selector or rename the test "+
				"to carry its language prefix.", name, lang, lang)
		}
	}
}

// callGraphOfEngineTests maps each function declared in engine's _test.go files
// to the names it calls.
func callGraphOfEngineTests() (map[string][]string, error) {
	fset := token.NewFileSet()
	files, err := filepath.Glob("*_test.go")
	if err != nil {
		return nil, err
	}
	graph := map[string][]string{}
	for _, path := range files {
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return nil, err
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			name := fn.Name.Name
			if _, seen := graph[name]; !seen {
				graph[name] = nil
			}
			ast.Inspect(fn, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if id, ok := call.Fun.(*ast.Ident); ok {
					graph[name] = append(graph[name], id.Name)
				}
				return true
			})
		}
	}
	return graph, nil
}

func reachesABuilder(fn string, calls map[string][]string, builders map[string]string, seen map[string]bool) string {
	if seen[fn] {
		return ""
	}
	seen[fn] = true
	for _, callee := range calls[fn] {
		if lang, ok := builders[callee]; ok {
			return lang
		}
		if lang := reachesABuilder(callee, calls, builders, seen); lang != "" {
			return lang
		}
	}
	return ""
}

// tier1Exclusions reads the anchored regexes under tiers.yaml's exclude_tests.
func tier1Exclusions() ([]*regexp.Regexp, error) {
	b, err := os.ReadFile("../tiers.yaml")
	if err != nil {
		return nil, err
	}
	var out []*regexp.Regexp
	in := false
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "exclude_tests:") {
			in = true
			continue
		}
		if !in {
			continue
		}
		trimmed := strings.TrimSpace(line)
		// The block ends at the next key at list-item-or-shallower indent that
		// is not a list entry or a comment.
		if trimmed != "" && !strings.HasPrefix(trimmed, "-") && !strings.HasPrefix(trimmed, "#") {
			break
		}
		m := regexp.MustCompile(`^- "(.+)"$`).FindStringSubmatch(trimmed)
		if m == nil {
			continue
		}
		re, err := regexp.Compile(m[1])
		if err != nil {
			return nil, err
		}
		out = append(out, re)
	}
	if len(out) == 0 {
		return nil, errNoExclusions
	}
	return out, nil
}

// e2eSelector reads the -run regex from the cross-language job.
func e2eSelector() (*regexp.Regexp, error) {
	b, err := os.ReadFile("../.github/workflows/e2e-cross-language.yml")
	if err != nil {
		return nil, err
	}
	m := regexp.MustCompile(`-run "([^"]+)"`).FindSubmatch(b)
	if m == nil {
		return nil, errNoSelector
	}
	return regexp.Compile(string(m[1]))
}

func matchesAny(name string, res []*regexp.Regexp) bool {
	for _, re := range res {
		if re.MatchString(name) {
			return true
		}
	}
	return false
}

type constErr string

func (e constErr) Error() string { return string(e) }

const (
	errNoExclusions = constErr("no exclude_tests entries found in tiers.yaml -- " +
		"an empty list would make this guard pass vacuously for every test")
	errNoSelector = constErr("no `-run \"...\"` found in e2e-cross-language.yml -- " +
		"the selector moved, and without it this guard cannot check the second half")
)
