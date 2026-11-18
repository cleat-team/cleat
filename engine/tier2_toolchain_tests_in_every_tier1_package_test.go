package engine

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestEveryToolchainBuildingTestInAnyTier1PackageIsExcludedAndRunByAJobThatProvidesIt
// is the widening of TestEveryToolchainBuildingTestIsExcludedFromTier1AndRunByTheE2EJob
// from engine/ to every package tiers.yaml's tier1.packages names. cleat#3690.
//
// WHY. That test walks engine's _test.go files and nothing else, and says so
// in its name only by implication. cmd/cleat is a tier-1 package, and three
// tests there build a Java guest through the Gradle wrapper:
// TestJavaBuildRefusesAStaleManifest, TestJavaBuildRefusesMultipleManifests and
// TestJavaExampleEntryPointResolutionLive (787997c5, 2026-09-24). None was in
// tier1.exclude_tests, so Tier 1 -- which by D5 provisions no JDK or Gradle --
// ran them on the runner image's preinstalled JDK and fetched Gradle from
// services.gradle.org, which redirects to a GitHub release asset. On 2026-11-08
// that answered 504 and develop's Tier 1 Gate went red on a commit that touched
// nothing under cmd/cleat (cleat#3689). Tier 1 had a release-blocking
// dependency on a host D5 had excluded on purpose, and the guard that exists to
// stop exactly that could not see the directory.
//
// It is also very likely the cause of cleat#3039, whose closed report recorded
// a 502 from the same host in the same job and concluded only that the test
// was unreachable from its change.
//
// WHAT COUNTS AS BUILDING A TOOLCHAIN. cmd/cleat's tests do not call
// buildJavaWasm; they run the cleat binary: exec.Command(cleatBinary, "build",
// "--target", "java", ...). So a test is a root here if its body, or a helper
// it reaches, either
//
//   - calls, or merely REFERENCES, one of the in-process builders (builderIdents)
//     -- tests/crash passes them to a table as values, which a scan of call
//     expressions never saw -- or
//   - carries the two adjacent string literals "--target", "<tier-2 language>"
//     in a call's arguments or a composite literal.
//
// REACHING A BUILDER IS NOT NEEDING ITS TOOLCHAIN, and the first run of this
// guard on develop said so: it named 13 cmd/cleat tests, and only 6 of them
// need one. Measured 2026-11-08 by running all 13 with a PATH containing no
// java, javac, node, npm, npx, cargo, rustc, rustup or gradle (what D5 says
// tier 1 provides): six SKIPPED (the three Java tests named in cleat#3689 and
// three AssemblyScript ones) and seven PASSED. The seven refuse before the
// toolchain stage -- a missing build file, a fake gradlew, a determinism finding
// reported before cargo or gradle starts -- and belong in tier 1. Excluding
// them would have removed coverage of the refusals from the gate that decides
// releasability, to cure a dependency they do not have. So the discriminator
// is a short, explicit list (toolchainFreeBuilderCallers), each entry with the
// measurement that put it there, and the guard fails on an entry that has gone
// stale in either direction.
//
// KNOWN BLIND SPOT, stated rather than discovered later: a target that arrives
// in a VARIABLE (`"--target", target`) is invisible to a literal scan.
// cmd/cleatctl's the_sidecar_cleat_build_writes_is_the_one_deploy_reads_test.go
// does that; every value it passes today is go or python (tier 1), which is
// why it is not a finding, but it is a gap and not a proof.
//
// A second one: a helper in ANOTHER package. The call graph is per directory, so
// a test that reaches a builder through an imported package's exported helper is
// not followed. Package-level var tables ARE followed (cleat-review's M2).
//
// WHERE A TEST IS ALLOWED TO RUN INSTEAD. Excluding a test from the gate is
// half; `go test -skip` leaves no --- SKIP line, so a test excluded from Tier 1
// and run nowhere retires silently while every job stays green. The second
// half names the job per package:
//
//   - engine/            -> e2e-cross-language.yml's -run selector
//   - cmd/...            -> ci.yml's test-go "commands" leg, which provisions
//     Rust, Node, Java and Gradle for exactly this and runs ./cmd/... with no
//     -run and no -skip. The guard checks all of that, not just the leg's name.
//   - tests/crash        -> ci.yml's test-go "crash" leg, which provisions the same
//     four and runs ./tests/crash/... with no -run and no -skip. Checked the same
//     way, for EVERY language the excluded test reaches (it reaches all three).
//   - any other tier-1 package -> a failure naming the test, so a toolchain
//     test added there forces a decision about where it runs instead of
//     passing because nobody wrote the mapping.
//
// THERE ARE NO ALLOWANCES. Until cleat#3692 this guard carried one,
// knownOpenToolchainTests, for tests/crash's agent-loop acceptance, which ran a
// subtest per client language and so could not be excluded without dropping its go
// and python clients. The tier-2 clients are now a top-level test of their own, the
// allowance and the machinery for it are deleted, and a toolchain test that is not
// excluded is a failure with nothing to list it under. If a future one genuinely
// cannot be excluded yet, that is a decision to make in the PR that adds the
// exception, not a map this file keeps ready for it.
func TestEveryToolchainBuildingTestInAnyTier1PackageIsExcludedAndRunByAJobThatProvidesIt(t *testing.T) {
	pkgs, err := tier1PackageDirs()
	if err != nil {
		t.Fatalf("reading tiers.yaml tier1.packages: %v", err)
	}
	excl, err := tier1Exclusions()
	if err != nil {
		t.Fatalf("reading tiers.yaml: %v", err)
	}

	type finding struct {
		dir, name string
		langs     []string // every tier-2 language the test reaches, sorted
	}
	var found []finding
	perDirLang := map[string]map[string]int{}
	for _, dir := range pkgs {
		if dir == "../engine" {
			// engine is covered, with its own per-language floors, by
			// TestEveryToolchainBuildingTestIsExcludedFromTier1AndRunByTheE2EJob.
			continue
		}
		calls, roots, err := callGraphAndRootsOfTestDir(dir)
		if err != nil {
			t.Fatalf("parsing %s: %v", dir, err)
		}
		for fn := range calls {
			if !strings.HasPrefix(fn, "Test") {
				continue
			}
			reachedLangs := map[string]bool{}
			tierTwoLanguagesReached(fn, calls, roots, map[string]bool{}, reachedLangs)
			if len(reachedLangs) == 0 {
				continue
			}
			var langs []string
			for lang := range reachedLangs {
				langs = append(langs, lang)
			}
			sort.Strings(langs)
			found = append(found, finding{dir, fn, langs})
			if perDirLang[dir] == nil {
				perDirLang[dir] = map[string]int{}
			}
			for _, lang := range langs {
				perDirLang[dir][lang]++
			}
		}
	}
	sort.Slice(found, func(i, j int) bool {
		if found[i].dir != found[j].dir {
			return found[i].dir < found[j].dir
		}
		return found[i].name < found[j].name
	})

	// The scan is checked before its result is used. Each package below has tests
	// for all three languages today; zero of one means the way tests reach a builder
	// changed -- a renamed helper, `"--target", "<lang>"` no longer written as
	// adjacent literals, or tests/crash's table no longer holding the builders by
	// name -- and both checks below pass vacuously for that language.
	if missing := toolchainTestsTheScanMustFind(perDirLang); len(missing) > 0 {
		t.Fatalf("the scan found no toolchain test where one is expected:\n  %s\n\n"+
			"There are several of each in cmd/cleat and one test in tests/crash that reaches "+
			"each language it lists. Zero means the way tests reach a builder changed, and "+
			"both checks below pass vacuously for it.", strings.Join(missing, "\n  "))
	}

	// An entry that is no longer a finding, or is now excluded, is stale: the list
	// is a statement about the tree and has to be re-read when the tree moves.
	reached := map[string]bool{}
	for _, f := range found {
		reached[f.name] = true
	}
	for name, why := range toolchainFreeBuilderCallers {
		switch {
		case !reached[name]:
			t.Errorf("toolchainFreeBuilderCallers lists %s (%s) but the scan no longer finds it "+
				"reaching a builder -- it was renamed, deleted or stopped building. Remove the entry.",
				name, why)
		case matchesAny(name, excl):
			t.Errorf("%s is listed in toolchainFreeBuilderCallers (%s) AND excluded from tier 1. "+
				"It cannot be both: either it needs a toolchain and the entry is wrong, or it "+
				"does not and the exclusion removes coverage for nothing.", name, why)
		}
	}

	legs := map[string]*testGoLegFacts{}
	for _, f := range found {
		if _, free := toolchainFreeBuilderCallers[f.name]; free {
			continue
		}
		langs := strings.Join(f.langs, ", ")
		if !matchesAny(f.name, excl) {
			t.Errorf("%s (%s) builds the %s toolchain but is not excluded from the tier-1 gate.\n\n"+
				"tiers.yaml puts those languages in tier 2 (D5) and the gate does not set out to "+
				"depend on their toolchains, but the runner image preinstalls them and some "+
				"tier-1 jobs provision them, so this test RUNS in the gate and depends on a download host (services.gradle.org, registry.npmjs.org, "+
				"static.crates.io) being up. cleat#3689 was exactly that, for Gradle. Add an "+
				"anchored pattern to tiers.yaml tier1.exclude_tests -- or, if it needs no "+
				"toolchain (it refuses before one starts), prove that with the toolchain off "+
				"PATH and list it in toolchainFreeBuilderCallers.\n\n"+
				"If the test is a SUBTEST-per-language table (tests/crash was), split the tier-2 "+
				"languages into a top-level test of their own: exclusions are matched against "+
				"`go test -list`, which lists top-level tests only.",
				f.name, strings.TrimPrefix(f.dir, "../"), langs)
		}

		leg, ok := legForTier1Package(f.dir)
		if !ok {
			t.Errorf("%s in %s builds the %s toolchain, and this guard does not know which job "+
				"runs tests from that package once they are excluded from tier 1.\n\n"+
				"Add the package to legForTier1Package with the job that provisions it, "+
				"or an excluded test there will run nowhere.",
				f.name, strings.TrimPrefix(f.dir, "../"), langs)
			continue
		}
		facts := legs[leg.name]
		if facts == nil {
			facts, err = readTestGoLeg(leg)
			if err != nil {
				t.Fatalf("reading ci.yml's %s leg: %v", leg.name, err)
			}
			legs[leg.name] = facts
		}
		// EVERY language the test reaches, not the first: tests/crash's one test
		// reaches all three, and a leg with Rust and Node but no JDK must fail.
		for _, lang := range f.langs {
			for _, problem := range facts.problemsFor(lang) {
				t.Errorf("%s (%s) is excluded from the tier-1 gate on the strength of ci.yml's "+
					"test-go %q leg running it, but %s.\n\nWithout that this test runs "+
					"NOWHERE, and `go test -skip` leaves no --- SKIP line to say so.",
					f.name, lang, leg.name, problem)
			}
		}
	}
}

// testGoLeg names the ci.yml test-go matrix entry that runs a tier-1 package's
// excluded toolchain tests, and the package path that entry must carry.
type testGoLeg struct{ name, path string }

// legForTier1Package says which ci.yml leg runs the tier-2 tests of a tier-1
// package once tiers.yaml excludes them from Tier 1. A package not listed here
// has no such leg, and a toolchain test in it fails the guard by name.
func legForTier1Package(dir string) (testGoLeg, bool) {
	switch {
	case dir == "../cmd" || strings.HasPrefix(dir, "../cmd/"):
		return testGoLeg{"commands", "./cmd/..."}, true
	case dir == "../tests/crash":
		return testGoLeg{"crash", "./tests/crash/..."}, true
	}
	return testGoLeg{}, false
}

// toolchainFreeBuilderCallers are tests that reach a tier-2 builder and do not
// need its toolchain. To add one, run it with the toolchain off PATH and show it
// PASSES (not skips): `go test ./cmd/cleat/ -run '^TestName$' -v` with a PATH
// lacking java, node, npx, cargo and gradle. A test that skips there needs it.
var toolchainFreeBuilderCallers = map[string]string{
	"TestRunBuild_JavaTarget_NoBuildFile":              "exits on a missing build.gradle before any toolchain starts",
	"TestRunBuild_JavaTarget_RelativePathWithGradlew":  "a fake gradlew stands in for the real one; asserts which path was invoked (cleat#1890)",
	"TestRunBuild_RustTarget_NoCargoToml":              "exits on a missing Cargo.toml before cargo starts",
	"TestRunBuild_ASTarget_NoPackageJSON":              "exits on a missing package.json before node starts",
	"TestJavaBuildRefusesNondeterminism":               "asserts on the determinism finding, reported before the gradle stage; later-stage failure is tolerated by design",
	"TestRustBuildRefusesNondeterminism":               "asserts on the determinism finding, reported before the cargo stage",
	"TestRustBuildAcceptsProseNamingAForbiddenPattern": "asserts the checker did not refuse; the later cargo failure is tolerated by design",
}

// builderIdents are the in-process entry points that build a tier-2 guest.
// buildJavaWasm and friends are engine's; runBuild* are cmd/cleat's.
var builderIdents = map[string]string{
	"buildJavaWasm":           "java",
	"buildAssemblyScriptWasm": "assemblyscript",
	"buildRustWasm":           "rust",
	"runBuildJava":            "java",
	"runBuildAssemblyScript":  "assemblyscript",
	"runBuildRust":            "rust",
}

// targetLiterals maps a literal --target value to its tier-2 language. go and
// python are tier 1 and deliberately absent.
var targetLiterals = map[string]string{
	"java":           "java",
	"assemblyscript": "assemblyscript",
	"as":             "assemblyscript",
	"rust":           "rust",
}

// tier1PackageDirs resolves tiers.yaml tier1.packages ("./cmd/...") to the
// directories that hold test files, relative to engine/ (where this test runs),
// pruning nested modules, testdata and hidden or vendored trees.
func tier1PackageDirs() ([]string, error) {
	b, err := os.ReadFile("../tiers.yaml")
	if err != nil {
		return nil, err
	}
	var doc struct {
		Tier1 struct {
			Packages []string `yaml:"packages"`
		} `yaml:"tier1"`
	}
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, err
	}
	if len(doc.Tier1.Packages) == 0 {
		return nil, constErr("tier1.packages is empty -- the guard would walk nothing and pass")
	}
	seen := map[string]bool{}
	var dirs []string
	for _, p := range doc.Tier1.Packages {
		root := filepath.Join("..", strings.TrimSuffix(strings.TrimPrefix(p, "./"), "/..."))
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() {
				return nil
			}
			name := d.Name()
			if path != root && (name == "testdata" || name == "node_modules" ||
				name == "vendor" || strings.HasPrefix(name, ".")) {
				return filepath.SkipDir
			}
			if path != root {
				if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
					return filepath.SkipDir
				}
			}
			if m, _ := filepath.Glob(filepath.Join(path, "*_test.go")); len(m) > 0 && !seen[path] {
				seen[path] = true
				dirs = append(dirs, path)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Strings(dirs)
	return dirs, nil
}

// callGraphAndRootsOfTestDir parses one directory's _test.go files. calls maps
// each declared function to the identifiers it calls; roots maps each function
// that itself builds a tier-2 toolchain to every language it builds. A SET, not
// one language: tests/crash's tierTwoAgentClients names all three builders, and a
// map that kept the last one written would tell the second half of this guard that
// the job running it needs only AssemblyScript's toolchain.
func callGraphAndRootsOfTestDir(dir string) (calls map[string][]string, roots map[string]map[string]bool, err error) {
	fset := token.NewFileSet()
	files, err := filepath.Glob(filepath.Join(dir, "*_test.go"))
	if err != nil {
		return nil, nil, err
	}
	calls = map[string][]string{}
	roots = map[string]map[string]bool{}
	addRoot := func(fn, lang string) {
		if roots[fn] == nil {
			roots[fn] = map[string]bool{}
		}
		roots[fn][lang] = true
	}
	for _, path := range files {
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return nil, nil, err
		}
		for _, decl := range f.Decls {
			// A package-level `var table = []func(*testing.T) []byte{buildJavaClientWASM}`
			// is a node of its own: tests that range over it reach the builder through
			// it. Only FuncDecls were scanned until cleat-review's M2 on cleat#3691.
			var nodes []struct {
				name string
				root ast.Node
			}
			switch d := decl.(type) {
			case *ast.FuncDecl:
				nodes = append(nodes, struct {
					name string
					root ast.Node
				}{d.Name.Name, d})
			case *ast.GenDecl:
				if d.Tok != token.VAR {
					continue
				}
				for _, spec := range d.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for _, id := range vs.Names {
						nodes = append(nodes, struct {
							name string
							root ast.Node
						}{id.Name, vs})
					}
				}
			}
			for _, node := range nodes {
				name, root := node.name, node.root
				if _, seen := calls[name]; !seen {
					calls[name] = nil
				}
				ast.Inspect(root, func(n ast.Node) bool {
					switch x := n.(type) {
					case *ast.Ident:
						// Every reference, not only a call: tests/crash hands its builders
						// to a table as VALUES (`build: buildJavaClientWASM`), so a scan of
						// call expressions never saw them (cleat-review on cleat#3691).
						// Over-approximate on purpose; only names that are declared
						// functions matter, and a wrongly-named test is a loud finding.
						calls[name] = append(calls[name], x.Name)
						if lang, ok := builderIdents[x.Name]; ok {
							addRoot(name, lang)
						}
					case *ast.CallExpr:
						if lang := targetLanguageIn(x.Args); lang != "" {
							addRoot(name, lang)
						}
					case *ast.CompositeLit:
						if lang := targetLanguageIn(x.Elts); lang != "" {
							addRoot(name, lang)
						}
					}
					return true
				})
			}
		}
	}
	return calls, roots, nil
}

// targetLanguageIn reports the tier-2 language of an adjacent "--target", "<x>"
// literal pair in exprs.
func targetLanguageIn(exprs []ast.Expr) string {
	lit := func(e ast.Expr) (string, bool) {
		b, ok := e.(*ast.BasicLit)
		if !ok || b.Kind != token.STRING {
			return "", false
		}
		return strings.Trim(b.Value, "\"`"), true
	}
	for i := 0; i+1 < len(exprs); i++ {
		if a, ok := lit(exprs[i]); ok && a == "--target" {
			if v, ok := lit(exprs[i+1]); ok {
				if lang, ok := targetLiterals[v]; ok {
					return lang
				}
			}
		}
	}
	return ""
}

// tierTwoLanguagesReached adds to langs every tier-2 language fn reaches. It
// follows every callee rather than stopping at the first root, so a test that
// reaches two languages is reported for both.
func tierTwoLanguagesReached(fn string, calls map[string][]string, roots map[string]map[string]bool, seen map[string]bool, langs map[string]bool) {
	if seen[fn] {
		return
	}
	seen[fn] = true
	for lang := range roots[fn] {
		langs[lang] = true
	}
	for _, callee := range calls[fn] {
		tierTwoLanguagesReached(callee, calls, roots, seen, langs)
	}
}

// flagsFilter matches a -run or -skip, in a go test line or in a matrix entry's flags.
var flagsFilter = regexp.MustCompile(`\s-(run|skip)[\s=]`)

// testGoLegFacts is what one ci.yml leg provides: either an entry in the
// `test-go` matrix (the original shape) or a sharded job group, in which a job
// `test-go-<leg>` splits the leg across several runners and
// `test-go-<leg>-summary` carries the required context name and fails unless
// the union of what the legs selected is the whole leg.
type testGoLegFacts struct {
	leg             testGoLeg
	legFound        bool     // the leg exists in one of the two shapes
	pathIsRight     bool     // ... and selects the leg's package path
	setup           []string // `uses:` of the steps that provision a toolchain
	filteredRunStep string   // a go test line that filters with -run or -skip
	runStepFound    bool
	sharded         bool
	// unionGuarded is the sharded shape's licence to filter at all: a job
	// carrying the required context name that diffs the union of the legs'
	// selections against the package's own listing. Without it a `-run`
	// selector is exactly the failure this guard exists for -- a test that
	// matches nothing and leaves every job green.
	unionGuarded bool
	// contextNamed says the summary job's display name is the required context.
	// Unchecked in the matrix shape, which reports its own name through ci.yml's
	// matrix and predates this field.
	contextNamed bool
}

type ciStep struct {
	Name string `yaml:"name"`
	If   string `yaml:"if"`
	Uses string `yaml:"uses"`
	Run  string `yaml:"run"`
	With struct {
		Path string `yaml:"path"`
	} `yaml:"with"`
}

type ciJob struct {
	Name     string `yaml:"name"`
	Strategy struct {
		Matrix struct {
			Package []map[string]any `yaml:"package"`
		} `yaml:"matrix"`
	} `yaml:"strategy"`
	Steps []ciStep `yaml:"steps"`
}

func readTestGoLeg(leg testGoLeg) (*testGoLegFacts, error) {
	b, err := os.ReadFile("../.github/workflows/ci.yml")
	if err != nil {
		return nil, err
	}
	var wf struct {
		Jobs map[string]ciJob `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(b, &wf); err != nil {
		return nil, err
	}

	// SHAPE 1, the original: an entry in the `test-go` matrix whose toolchain
	// steps are conditioned on `matrix.package.name == '<leg>'`.
	facts := &testGoLegFacts{leg: leg}
	for _, job := range wf.Jobs {
		isLeg := false
		for _, p := range job.Strategy.Matrix.Package {
			if p["name"] != leg.name {
				continue
			}
			isLeg = true
			facts.legFound = true
			facts.pathIsRight = p["path"] == leg.path
			// The leg's flags go onto the same go test line as ${{ matrix.package.flags }}.
			if fl, ok := p["flags"].(string); ok && flagsFilter.MatchString(" "+fl+" ") {
				facts.filteredRunStep = "matrix flags: " + fl
			}
		}
		if !isLeg {
			continue
		}
		for _, s := range job.Steps {
			if strings.Contains(s.If, "matrix.package.name == '"+leg.name+"'") && s.Uses != "" {
				facts.setup = append(facts.setup, s.Uses)
			}
			if s.Name == "Run tests" {
				facts.runStepFound = true
				for _, line := range strings.Split(s.Run, "\n") {
					if strings.HasPrefix(strings.TrimSpace(line), "go test") &&
						flagsFilter.MatchString(line) {
						facts.filteredRunStep = strings.TrimSpace(line)
					}
				}
			}
		}
	}
	if facts.legFound {
		facts.contextNamed = true
		return facts, nil
	}

	// SHAPE 2, the sharded group. The properties this guard needs survive the
	// split: every leg of the group provisions the toolchain, each has a "Run
	// tests" step, the group selects the leg's package path, and a job whose
	// display name IS the required context fails unless the union covers the
	// package. That last one is what makes a shard's `-run` selector acceptable
	// where the matrix shape would not be.
	shards, ok := wf.Jobs["test-go-"+leg.name]
	if !ok {
		return nil, constErr("found neither a test-go matrix entry nor a job named \"test-go-" + leg.name +
			"\" in ci.yml -- the leg was renamed or removed, and the second half of this guard has nothing to check")
	}
	summary, ok := wf.Jobs["test-go-"+leg.name+"-summary"]
	if !ok {
		return nil, constErr("found ci.yml's \"test-go-" + leg.name + "\" job but no \"test-go-" +
			leg.name + "-summary\" job beside it -- the required context name would then be reported by nobody, " +
			"and nothing would check that the legs together are the whole leg")
	}
	facts = &testGoLegFacts{leg: leg, legFound: true, sharded: true}

	var listing []string // the package's OWN listing -- the denominator the summary diffs against
	// The path must be selected by an INVOCATION, not by a mention. A scan of the
	// steps' whole text is satisfied by prose: the comments here name
	// `./cmd/...` and the selection step runs `go list ./cmd/...` as its
	// DENOMINATOR, so a doctored tree that deleted the leg's own `PATHS=` arm
	// stayed green when this asked `strings.Contains(text, path)`.
	selected := false
	for _, s := range shards.Steps {
		if strings.Contains(s.Run, "PATHS='"+leg.path+"'") {
			selected = true
		}
		if s.Uses != "" {
			facts.setup = append(facts.setup, s.Uses)
		}
		for _, line := range strings.Split(s.With.Path, "\n") {
			if f := strings.TrimSpace(line); strings.HasPrefix(f, "all-") && strings.HasSuffix(f, ".txt") {
				listing = append(listing, f)
			}
		}
		if s.Name == "Run tests" {
			facts.runStepFound = true
			for _, line := range strings.Split(s.Run, "\n") {
				if !strings.HasPrefix(strings.TrimSpace(line), "go test") {
					continue
				}
				// The crash group selects by test name over the whole package, so
				// its path is on the go test line rather than in a PATHS arm.
				if strings.Contains(line, leg.path) {
					selected = true
				}
				if flagsFilter.MatchString(line) {
					facts.filteredRunStep = strings.TrimSpace(line)
				}
			}
		}
	}
	// The summary's diff is what makes this a claim rather than a string match:
	// a path narrowed here and the listing re-derived there disagree.
	facts.pathIsRight = selected

	facts.contextNamed = strings.HasPrefix(summary.Name, "Test Go ("+leg.name+")")
	for _, s := range summary.Steps {
		if !strings.Contains(s.Run, "diff") {
			continue
		}
		for _, f := range listing {
			if strings.Contains(s.Run, f) {
				facts.unionGuarded = true
			}
		}
	}
	return facts, nil
}

// problemsFor lists what is missing for lang to be built by the leg.
func (f *testGoLegFacts) problemsFor(lang string) []string {
	var out []string
	if !f.pathIsRight {
		out = append(out, "the leg no longer selects "+f.leg.path)
	}
	if !f.runStepFound {
		out = append(out, "the leg has no step named \"Run tests\"")
	}
	if f.filteredRunStep != "" && !f.unionGuarded {
		out = append(out, "the leg's go test line carries -run or -skip ("+f.filteredRunStep+
			") and no summary job diffs the legs' union against the package's listing")
	}
	if f.sharded && !f.contextNamed {
		out = append(out, "the summary job's display name is not the required context \"Test Go ("+
			f.leg.name+") ...\", so the split leg reports a context nothing requires")
	}
	want := map[string]string{
		"java":           "actions/setup-java",
		"assemblyscript": "actions/setup-node",
		"rust":           "dtolnay/rust-toolchain",
	}[lang]
	// The action's name, exactly: a prefix match would accept "actions/setup-javaX".
	action := func(uses string) string { return strings.SplitN(uses, "@", 2)[0] }
	has := false
	for _, u := range f.setup {
		if action(u) == want {
			has = true
		}
	}
	if !has {
		out = append(out, "the leg has no step using "+want+" conditioned on the "+f.leg.name+" leg")
	}
	if lang == "java" {
		gradle := false
		for _, u := range f.setup {
			if action(u) == "gradle/actions/setup-gradle" {
				gradle = true
			}
		}
		if !gradle {
			out = append(out, "the leg has no gradle/actions/setup-gradle step conditioned on it")
		}
	}
	return out
}

// expectedToolchainTests is, per package, every tier-2 language that package must have at least one toolchain-building test for.
//
// A TABLE PER PACKAGE rather than one list applied to all of them, because a language leaves a package when that package's last
// test for it is deleted, and the change that deletes it must be able to say so in one line without touching the other package's
// expectation (the AssemblyScript removal, cleat#4023, removes cmd/cleat's and tests/crash's on different branches). Until then
// each package lists what it has.
var expectedToolchainTests = []struct {
	dir   string
	langs []string
}{
	{"../cmd/cleat", []string{"java", "assemblyscript", "rust"}},
	{"../tests/crash", []string{"java", "rust"}},
}

// toolchainTestsTheScanMustFind names every (package, language) pair in expectedToolchainTests for which perDirLang, the scan's
// own count, has no test. A function so the check can be tested against a count the scan did not produce: a guard that reports
// nothing when a package's last toolchain test is deleted is the failure this exists to catch, and it cannot be shown to catch it
// by deleting a real test.
func toolchainTestsTheScanMustFind(perDirLang map[string]map[string]int) []string {
	var missing []string
	for _, pkg := range expectedToolchainTests {
		for _, lang := range pkg.langs {
			if perDirLang[pkg.dir][lang] == 0 {
				missing = append(missing, fmt.Sprintf("no %s toolchain test in %s (found %v)", lang, strings.TrimPrefix(pkg.dir, "../"), perDirLang[pkg.dir]))
			}
		}
	}
	return missing
}

func TestTheToolchainPresenceCheckNamesAPackageWhoseLastTestForALanguageIsGone(t *testing.T) {
	// A count that satisfies the table exactly, built FROM the table: a hand-written one went stale the first time a language was
	// removed from a package, and the test failed for that rather than for what it is about.
	full := func() map[string]map[string]int {
		m := map[string]map[string]int{}
		for _, pkg := range expectedToolchainTests {
			m[pkg.dir] = map[string]int{}
			for _, lang := range pkg.langs {
				m[pkg.dir][lang] = 1
			}
		}
		return m
	}
	if got := toolchainTestsTheScanMustFind(full()); len(got) != 0 {
		t.Fatalf("a complete count was reported as missing %v: the check would fail on a healthy tree", got)
	}
	// Each package's last test for a language, deleted in turn: the check must name exactly that pair and no other.
	for _, pkg := range expectedToolchainTests {
		for _, lang := range pkg.langs {
			m := full()
			delete(m[pkg.dir], lang)
			got := toolchainTestsTheScanMustFind(m)
			if len(got) != 1 || !strings.Contains(got[0], lang) || !strings.Contains(got[0], strings.TrimPrefix(pkg.dir, "../")) {
				t.Errorf("with %s's last %s test gone the check reported %v, want exactly that one pair", pkg.dir, lang, got)
			}
		}
	}
	// A package the scan found nothing in at all is every one of its languages missing, not a skipped package.
	pkg := expectedToolchainTests[len(expectedToolchainTests)-1]
	m := full()
	delete(m, pkg.dir)
	if got := toolchainTestsTheScanMustFind(m); len(got) != len(pkg.langs) {
		t.Errorf("with %s absent from the count the check reported %d pairs (%v), want %d", pkg.dir, len(got), got, len(pkg.langs))
	}
}
