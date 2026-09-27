package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
)

// TestEveryGoExampleReadmeNamesTheArtifactTheBuildProduces checks the one thing about an example
// README that nothing has ever checked: whether the file it tells you to deploy is a file
// `cleat build` actually produces, and whether the entry point it tells you to run is one the
// compiled module actually exports.
//
// WHY THIS EXISTS. cleat#2048 (closed) fixed every documented `cleat deploy NAME file.wasm`, which
// was a FLAG error: `runDeploy` takes the first positional as the wasm path, so the correct form
// is `--name NAME`. It fixed those and its guard checks that documented invocations clear flag
// parsing. Neither looks at what the positional ARGUMENTS name, and both are wrong:
//
//	example      README deploys        cleat build emits    README runs --entry-point  module exports
//	travel       travel.wasm          book_travel.wasm     BookTravel                 book_travel
//	fooddash     fooddash.wasm        cancel_order.wasm    PlaceOrder                 cancel_order
//	onboarding   onboarding.wasm      register_user.wasm   RegisterUser               register_user
//
// `cleat build` names the artifact after the entry point in snake_case -- `//go:wasmexport
// book_travel` -- not after the example directory the README is named for, and the module exports
// that snake_case form. So a reader who copies the commands gets `Error reading WASM file
// /tmp/out/travel.wasm: no such file or directory` on the deploy line and an export lookup failure
// on the run line. Surfaced by cleat#2049's cost measurement, which drove three examples by hand
// and got three failures; #2048's method was a regex over command SHAPE, and a name is not a shape.
//
// WHY NOT THE END-TO-END JOB FIRST. cleat#2049 proposes running each manifest through a worker and
// asserting on a result. That is the right destination, and this is the cheaper step that has to
// come first: measured, the full job is ~25s of marginal cost over the existing
// `Documented example builds` job, but pointed at the READMEs as they stand it would go red on
// three examples out of three FOR A DOC BUG. A guard whose red is a documentation defect teaches
// people to ignore it. This one needs no database, no worker and no service, and it is what makes
// the end-to-end job's red mean a regression.
//
// WHAT IT DELIBERATELY DOES NOT COVER, because a guard that overstates its scope is worse than a
// small one. Only examples whose README documents BOTH a `cleat deploy` and a Go-target `cleat
// build` are checked. The seven non-Go examples (as-workflow, java-workflow, python-hello,
// python-langchain, rust-workflow, saga-java-port, widget-store-as) build through cargo, gradle,
// npm or componentize-py and produce their artifact names by other rules; covering them from here
// would mean re-implementing four toolchains. They are REPORTED, one line each with the reason,
// rather than silently passing -- and the count of what was checked is asserted against a floor,
// because "every README that could be checked was fine" is also what a run that checked nothing
// reports.
func TestEveryGoExampleReadmeNamesTheArtifactTheBuildProduces(t *testing.T) {
	if cleatBinary == "" {
		t.Fatalf("UNMEASURED: no cleat binary was built, so no example was checked. This is a failure " +
			"of the CHECK, not a finding about the examples -- run without -short (TestMain builds the " +
			"binary only when it is not short). Reporting green here would be this test's own subject.")
	}

	root := filepath.Join("..", "..")
	entries, err := os.ReadDir(filepath.Join(root, "examples"))
	if err != nil {
		t.Fatalf("reading examples/: %v", err)
	}

	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	// Mutex-guarded, because the subtests below run in parallel. Same reason as
	// TestEveryGoExampleBuilds's: `go test -race` runs in CI, and two goroutines appending to one
	// slice is a real race rather than a stylistic point.
	var mu sync.Mutex
	var checked, unchecked []string
	note := func(list *[]string, entry string) {
		mu.Lock()
		defer mu.Unlock()
		*list = append(*list, entry)
	}

	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := filepath.Join(root, "examples", name)

			readmeBytes, err := os.ReadFile(filepath.Join(dir, "README.md"))
			if err != nil {
				t.Logf("not checked: no README.md")
				note(&unchecked, name+" (no README)")
				return
			}
			// Join line continuations FIRST, then look at anything. A documented command is
			// routinely split across lines with a trailing backslash -- `cleat run --wasm X
			// --entry-point Y \` then `--input '{...}'` -- and a parse that reads one line at a
			// time sees a truncated command. Same discipline as the workflow guards, for the same
			// reason: neither the invocation nor the name may be read out of a line that is really
			// half of one.
			readme := joinLineContinuations(string(readmeBytes))

			deployWasm, _ := parseDeployWasm(readme)
			if deployWasm == "" {
				t.Logf("not checked: README documents no `cleat deploy ...<file>.wasm`")
				note(&unchecked, name+" (no documented deploy)")
				return
			}
			if !hasGoFiles(dir) {
				t.Logf("not checked: not a Go example -- %q builds through its own toolchain, so the "+
					"artifact name is not `cleat build`'s to produce", name)
				note(&unchecked, name+" (non-Go toolchain)")
				return
			}
			entryPoint, _ := parseRunEntryPoint(readme)

			outDir := t.TempDir()
			// Per-example output directory, NOT shared: `cleat build` copies source into the build
			// directory and does not clear it, so a shared directory makes every build after the
			// first inherit the previous example's files. See TestEveryGoExampleBuilds.
			cmd := exec.Command(cleatBinary, "build", "--target", "go", "-o", outDir, dir)
			if out, err := cmd.CombinedOutput(); err != nil {
				if strings.Contains(string(out), "no workflow entry points found") {
					t.Logf("not checked: not a workflow (no entry points)")
					note(&unchecked, name+" (not a workflow)")
					return
				}
				t.Fatalf("cleat build failed, so nothing below could be checked:\n%s\n%v\n"+
					"UNMEASURED for this example -- a failure of the CHECK, not a finding about the README.", out, err)
			}

			note(&checked, name)

			// CHECK 1: the file name the README deploys is a file the build produced.
			//
			// Basenames, not paths: the README's `/tmp/out/` and this test's t.TempDir() are
			// different directories on purpose, and the name is the part the README is promising.
			wantBase := filepath.Base(deployWasm)
			produced, err := wasmBasenames(outDir)
			if err != nil {
				t.Fatalf("listing the build output: %v", err)
			}
			if !sliceContains(produced, wantBase) {
				t.Errorf("the README tells the reader to deploy %q, and `cleat build` does not produce it.\n"+
					"  produced instead: %v\n"+
					"  the README's deploy line: cleat deploy … %s\n"+
					"`cleat build` names the artifact after the entry point in snake_case, not after the "+
					"example directory, so a reader who copies the command gets \"Error reading WASM file … "+
					"no such file or directory\". cleat#2049.",
					wantBase, produced, deployWasm)
			}

			// CHECK 2: the entry point the README runs is an export the module actually has.
			//
			// Absent when the README documents a deploy but no run -- event-driven does, because it
			// runs through a worker and an HTTP API rather than `cleat run`. That is a gap in what
			// can be checked here, not a pass: it is logged, and event-driven still gets CHECK 1.
			if entryPoint == "" {
				t.Logf("no `--entry-point` in this README's documented `cleat run`, so only the file name " +
					"was checked -- this example has no in-process run for the entry point to be wrong in")
				return
			}
			// Read the export names from the module the build just produced, using the same parser
			// `cleat build` itself uses to refuse metadata that names an entry the compiler did not
			// export (build_entry_points.go). Reusing it rather than re-parsing WASM here is the
			// point: a second parser is a second thing that can disagree with the binary.
			wasmPath := filepath.Join(outDir, wantBase)
			wasmBytes, err := os.ReadFile(wasmPath)
			if err != nil {
				// CHECK 1 already failed and said why; do not double-report.
				t.Logf("cannot check the entry point: %v", err)
				return
			}
			exports := wasmFuncExportNames(wasmBytes)
			if !exports[entryPoint] {
				var got []string
				for n := range exports {
					got = append(got, n)
				}
				sort.Strings(got)
				t.Errorf("the README tells the reader to run --entry-point %q, and the module does not "+
					"export it.\n"+
					"  the module exports: %v\n"+
					"The Go target exports the entry point in snake_case, so the documented name has to be "+
					"that form. cleat#2049.", entryPoint, got)
			}
		})
	}

	t.Cleanup(func() {
		for _, u := range unchecked {
			t.Logf("not checked: %s", u)
		}
		// A FLOOR, because "every README that could be checked was correct" is also what a run that
		// checked nothing reports. Seven Go examples document a deploy today; five is loose enough
		// not to break on an example being added or removed, and tight enough to catch a collapse --
		// a change that stops this test finding the examples it exists to check.
		if len(checked) < 5 {
			t.Errorf("only %d example(s) were checked (%v), against a floor of 5.\n"+
				"Not checked: %v\n"+
				"This test passes vacuously if it stops finding examples, which is the failure mode it "+
				"exists to guard against.", len(checked), checked, unchecked)
		}
	})
}

// joinLineContinuations folds `\`-continued lines into one, so a command split across lines is
// parsed as the single command it is.
func joinLineContinuations(s string) string {
	return regexp.MustCompile(`\\\r?\n[ \t]*`).ReplaceAllString(s, " ")
}

var (
	deployRe     = regexp.MustCompile(`cleat deploy\b[^\n]*?([^\s"'` + "`" + `]+\.wasm)`)
	runWasmRe    = regexp.MustCompile(`cleat run\b[^\n]*?--wasm\s+([^\s"'` + "`" + `]+)`)
	entryPointRe = regexp.MustCompile(`--entry-point[=\s]+([A-Za-z_][A-Za-z0-9_]*)`)
)

// parseDeployWasm returns the .wasm path a README's documented `cleat deploy` names.
func parseDeployWasm(readme string) (string, bool) {
	m := deployRe.FindStringSubmatch(readme)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// parseRunEntryPoint returns the entry point a README's documented `cleat run` names.
//
// Scoped to the FIRST `cleat run` that also carries a `--wasm`, so a README's prose about
// `cleat run` elsewhere cannot be read as an invocation. Both halves have to be present for this
// to be a command the reader is told to run.
func parseRunEntryPoint(readme string) (string, bool) {
	for _, line := range strings.Split(readme, "\n") {
		if !strings.Contains(line, "cleat run") {
			continue
		}
		if !runWasmRe.MatchString(line) {
			continue
		}
		if m := entryPointRe.FindStringSubmatch(line); m != nil {
			return m[1], true
		}
	}
	return "", false
}

// wasmBasenames lists the .wasm files in a build output directory.
func wasmBasenames(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".wasm") {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

func sliceContains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}
