package engine

import (
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
//   - calls one of the in-process builders (builderIdents), or
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
// WHERE A TEST IS ALLOWED TO RUN INSTEAD. Excluding a test from the gate is
// half; `go test -skip` leaves no --- SKIP line, so a test excluded from Tier 1
// and run nowhere retires silently while every job stays green. The second
// half names the job per package:
//
//   - engine/            -> e2e-cross-language.yml's -run selector
//   - cmd/...            -> ci.yml's test-go "commands" leg, which provisions
//     Rust, Node, Java and Gradle for exactly this and runs ./cmd/... with no
//     -run and no -skip. The guard checks all of that, not just the leg's name.
//   - any other tier-1 package -> a failure naming the test, so a toolchain
//     test added there forces a decision about where it runs instead of
//     passing because nobody wrote the mapping.
func TestEveryToolchainBuildingTestInAnyTier1PackageIsExcludedAndRunByAJobThatProvidesIt(t *testing.T) {
	pkgs, err := tier1PackageDirs()
	if err != nil {
		t.Fatalf("reading tiers.yaml tier1.packages: %v", err)
	}
	excl, err := tier1Exclusions()
	if err != nil {
		t.Fatalf("reading tiers.yaml: %v", err)
	}

	type finding struct{ dir, name, lang string }
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
			if lang := reachesATierTwoToolchain(fn, calls, roots, map[string]bool{}); lang != "" {
				found = append(found, finding{dir, fn, lang})
				if perDirLang[dir] == nil {
					perDirLang[dir] = map[string]int{}
				}
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

	// The scan is checked before its result is used. cmd/cleat has tests for all
	// three languages today; zero of one means the literal scan stopped seeing
	// that language and every check below passes vacuously for it.
	for _, lang := range []string{"java", "assemblyscript", "rust"} {
		if perDirLang["../cmd/cleat"][lang] == 0 {
			t.Fatalf("the scan found no %s toolchain test in cmd/cleat (found %v).\n\n"+
				"There are several of each. Zero means the way tests reach a builder "+
				"changed -- a renamed helper, or `\"--target\", \"%s\"` no longer written "+
				"as adjacent literals -- and both checks below pass vacuously for it.",
				lang, perDirLang["../cmd/cleat"], lang)
		}
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

	var commandsLeg *commandsLegFacts
	for _, f := range found {
		if _, free := toolchainFreeBuilderCallers[f.name]; free {
			continue
		}
		if !matchesAny(f.name, excl) {
			t.Errorf("%s (%s) builds the %s toolchain but is not excluded from the tier-1 gate.\n\n"+
				"tier1-gate.yml provisions no JDK, Gradle, Node or Rust (D5: those languages are "+
				"tier 2), but the GitHub runner image preinstalls them, so this test RUNS in the "+
				"gate and depends on a download host (services.gradle.org, registry.npmjs.org, "+
				"static.crates.io) being up. cleat#3689 was exactly that, for Gradle. Add an "+
				"anchored pattern to tiers.yaml tier1.exclude_tests -- or, if it needs no "+
				"toolchain (it refuses before one starts), prove that with the toolchain off "+
				"PATH and list it in toolchainFreeBuilderCallers.",
				f.name, strings.TrimPrefix(f.dir, "../"), f.lang)
		}

		switch {
		case f.dir == "../cmd/cleat" || strings.HasPrefix(f.dir, "../cmd/"):
			if commandsLeg == nil {
				commandsLeg, err = readCommandsLeg()
				if err != nil {
					t.Fatalf("reading ci.yml's commands leg: %v", err)
				}
			}
			for _, problem := range commandsLeg.problemsFor(f.lang) {
				t.Errorf("%s (%s) is excluded from the tier-1 gate on the strength of ci.yml's "+
					"test-go \"commands\" leg running it, but %s.\n\nWithout that this test runs "+
					"NOWHERE, and `go test -skip` leaves no --- SKIP line to say so.",
					f.name, f.lang, problem)
			}
		default:
			t.Errorf("%s in %s builds the %s toolchain, and this guard does not know which job "+
				"runs tests from that package once they are excluded from tier 1.\n\n"+
				"Add the package to the switch in this test with the job that provisions %s, "+
				"or an excluded test there will run nowhere.",
				f.name, strings.TrimPrefix(f.dir, "../"), f.lang, f.lang)
		}
	}
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
// that itself builds a tier-2 toolchain to its language.
func callGraphAndRootsOfTestDir(dir string) (calls map[string][]string, roots map[string]string, err error) {
	fset := token.NewFileSet()
	files, err := filepath.Glob(filepath.Join(dir, "*_test.go"))
	if err != nil {
		return nil, nil, err
	}
	calls = map[string][]string{}
	roots = map[string]string{}
	for _, path := range files {
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return nil, nil, err
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			name := fn.Name.Name
			if _, seen := calls[name]; !seen {
				calls[name] = nil
			}
			ast.Inspect(fn, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.CallExpr:
					if id, ok := x.Fun.(*ast.Ident); ok {
						calls[name] = append(calls[name], id.Name)
						if lang, ok := builderIdents[id.Name]; ok {
							roots[name] = lang
						}
					}
					if lang := targetLanguageIn(x.Args); lang != "" {
						roots[name] = lang
					}
				case *ast.CompositeLit:
					if lang := targetLanguageIn(x.Elts); lang != "" {
						roots[name] = lang
					}
				}
				return true
			})
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

func reachesATierTwoToolchain(fn string, calls map[string][]string, roots map[string]string, seen map[string]bool) string {
	if seen[fn] {
		return ""
	}
	seen[fn] = true
	if lang, ok := roots[fn]; ok {
		return lang
	}
	for _, callee := range calls[fn] {
		if lang := reachesATierTwoToolchain(callee, calls, roots, seen); lang != "" {
			return lang
		}
	}
	return ""
}

// commandsLegFacts is what ci.yml's test-go "commands" matrix entry provides.
type commandsLegFacts struct {
	legFound        bool     // a test-go matrix entry named commands exists
	pathIsCmd       bool     // ... and its path is ./cmd/...
	setup           []string // `uses:` of steps conditioned on the commands leg
	filteredRunStep string   // the Run tests step's go test line, if it filters
	runStepFound    bool
}

func readCommandsLeg() (*commandsLegFacts, error) {
	b, err := os.ReadFile("../.github/workflows/ci.yml")
	if err != nil {
		return nil, err
	}
	var wf struct {
		Jobs map[string]struct {
			Strategy struct {
				Matrix struct {
					Package []map[string]any `yaml:"package"`
				} `yaml:"matrix"`
			} `yaml:"strategy"`
			Steps []struct {
				Name string `yaml:"name"`
				If   string `yaml:"if"`
				Uses string `yaml:"uses"`
				Run  string `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(b, &wf); err != nil {
		return nil, err
	}
	facts := &commandsLegFacts{}
	for _, job := range wf.Jobs {
		isLeg := false
		for _, p := range job.Strategy.Matrix.Package {
			if p["name"] == "commands" {
				isLeg = true
				facts.legFound = true
				facts.pathIsCmd = p["path"] == "./cmd/..."
			}
		}
		if !isLeg {
			continue
		}
		for _, s := range job.Steps {
			if strings.Contains(s.If, "matrix.package.name == 'commands'") && s.Uses != "" {
				facts.setup = append(facts.setup, s.Uses)
			}
			if s.Name == "Run tests" {
				facts.runStepFound = true
				for _, line := range strings.Split(s.Run, "\n") {
					if strings.HasPrefix(strings.TrimSpace(line), "go test") &&
						regexp.MustCompile(`\s-(run|skip)[\s=]`).MatchString(line) {
						facts.filteredRunStep = strings.TrimSpace(line)
					}
				}
			}
		}
	}
	if !facts.legFound {
		return nil, constErr("found no test-go matrix entry named \"commands\" in ci.yml -- " +
			"the leg was renamed or removed, and the second half of this guard has nothing to check")
	}
	return facts, nil
}

// problemsFor lists what is missing for lang to be built by the commands leg.
func (f *commandsLegFacts) problemsFor(lang string) []string {
	var out []string
	if !f.pathIsCmd {
		out = append(out, "the leg's matrix path is no longer ./cmd/...")
	}
	if !f.runStepFound {
		out = append(out, "the leg has no step named \"Run tests\"")
	}
	if f.filteredRunStep != "" {
		out = append(out, "the leg's go test line carries -run or -skip ("+f.filteredRunStep+")")
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
		out = append(out, "the leg has no step using "+want+" conditioned on the commands leg")
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
