package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// cleat#1888. Every Go template produced a project that did not build, and
// every scaffold test passed, because the tests asserted that files existed
// and contained expected strings. None of them built the output.
//
// That gap hid three independent defects at once, which is the argument for
// this test existing rather than for any one of the fixes:
//
//   - workflow and fullstack pinned `github.com/cleat-team/cleat v0.0.0`, a
//     tag that has never existed;
//   - after that was corrected to a stamped version, the require still named
//     the PARENT module while the generated code imports
//     github.com/cleat-team/cleat/cleat -- a separate module (cleat/go.mod)
//     which was untagged AT THE TIME (cleat/v0.3.1 and cleat/v0.3.2 were cut
//     on 2026-09-27, after this), so no version string could work and the
//     build failed on a missing go.sum entry;
//   - and underneath both, workflow and fullstack declared `func main() {}`
//     plus a raw //go:wasmexport directive, which collide with the stub
//     `cleat build` generates ("other declaration of main", "symbol process
//     redeclared") and which Go's toolchain rejects for these parameter types.
//
// A reader could not have found any of them from the templates; only running
// the documented command does. So this runs the documented command.
//
// THIS REPLACES TestScaffoldedProjectsActuallyBuild, which cleat#1891 added
// and which ran `go vet` rather than `cleat build`. Its reasoning is worth
// repeating because the misstep is easy to make again: `go build` on a basic
// or agent scaffold fails at the LINK step ("function main is undeclared"),
// since those templates deliberately have no main, so it reached for a
// command that would pass. But the answer to that is `cleat build` -- which
// generates the missing main -- not a weaker command. Redefining the defect
// as "does the module graph resolve" rather than "does the documented command
// work" is what let it stay green over all three defects above.
//
// One observation from it is kept because it is load-bearing and was
// verified rather than assumed: `sdkReplaceDir` (wasm/build.go), which finds
// this repo's own cleat/ by walking up from a project directory, is called
// only by `cleat build`'s pipeline and never by `cleat init`. A scaffold
// created in t.TempDir() therefore resolves the SDK from the real module
// proxy exactly as an external user's would, nowhere near this checkout.
//
// NOT GUARDED ON NETWORK AVAILABILITY. `go mod tidy` in the scaffold needs the
// module proxy, and that is a real dependency of the thing under test rather
// than an optional resource: a scaffold that cannot resolve its own SDK is
// exactly the failure this test exists to catch. Skipping when the network is
// absent would make the test report clean in the one situation where it has
// measured nothing -- the shape scripts/check-skips.sh calls case (b).
func TestEveryGoTemplateScaffoldsIntoAProjectThatBuilds(t *testing.T) {
	if testing.Short() || cleatBinary == "" {
		t.Skip("needs the cleat binary, which TestMain does not build in short mode")
	}

	// agent-python is deliberately absent: its documented first step is
	// `pip install -r requirements.txt` and its checker needs Python >= 3.10,
	// so building it here would test the environment, not the template.
	//
	// Every template now writes a cleat.yaml carrying the project's own
	// actual name (cleat#2692) -- workflow and fullstack via
	// writeScaffoldTemplate, basic and agent via writeYAML. writeYAML wrote
	// `project:` until this same change, a key wasmOutputName never read,
	// so every agent scaffold built to the same workflow.wasm regardless of
	// what the user called it -- the identical collision cleat#2692 exists
	// to prevent, reachable through the one scaffold the fix had not yet
	// reached. So the wanted artifact is always the project's own name,
	// computed below rather than pinned here as a fixed string: a fixed
	// string would silently stop testing the rule the moment any
	// scaffold's own source layout changed.
	for _, template := range []string{"basic", "agent", "workflow", "fullstack"} {
		t.Run(template, func(t *testing.T) {
			root := t.TempDir()
			name := "p_" + template
			wantArtifact := name + ".wasm"

			out, err := runCleatIn(t, root, "init", "--template", template, name)
			if err != nil {
				t.Fatalf("cleat init --template %s failed: %v\n%s", template, err, out)
			}

			proj := filepath.Join(root, name)
			resolveScaffoldAgainstThisCheckout(t, proj)
			out, err = runCleatIn(t, proj, "build", "-o", "./out", ".")
			if err != nil {
				t.Fatalf("a scaffolded %s project does not build.\n"+
					"This is the first command its own README tells a new user to run.\n%v\n%s",
					template, err, out)
			}

			// An exit code of 0 is not the claim -- an artifact is. `cleat
			// build` has previously printed its analysis and emitted nothing.
			art := filepath.Join(proj, "out", wantArtifact)
			if _, statErr := os.Stat(art); statErr != nil {
				listing, _ := os.ReadDir(filepath.Join(proj, "out"))
				var names []string
				for _, e := range listing {
					names = append(names, e.Name())
				}
				t.Errorf("build succeeded but %s was not emitted; out/ contains %v", wantArtifact, names)
			}
		})
	}
}

func runCleatIn(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(cleatBinary, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// resolveScaffoldAgainstThisCheckout rewrites a scaffolded project's go.mod so
// the SDK resolves from this checkout rather than from the module proxy.
//
// WHY THIS EXISTS, AND WHY IT IS NOT A COSMETIC CHANGE TO THE TESTS.
//
// A freshly scaffolded project's go.mod names no SDK at all -- `module X` and
// `go 1.24`, nothing else -- so `go mod tidy` (tidyScaffold, init.go) resolves
// `github.com/cleat-team/cleat/cleat` from the proxy.
//
// WHEN THIS HELPER WAS WRITTEN, that module had never been tagged, so its
// `@latest` was a PSEUDO-VERSION OF THE DEFAULT BRANCH:
//
//	proxy …/cleat/cleat/@latest -> v0.0.0-...-<develop head>
//
// which meant the version a scaffold resolved to was whatever `cleat/go.mod`
// said AT THE CURRENT PUSHED HEAD -- not at the revision under test. A pull
// request therefore could not validate anything on this path: set the require
// to an unpublished version and the failure landed on the NEXT commit, while
// the PR that made the change stayed green. That is exactly what cleat#2452
// did, and it is what made the develop red un-gateable.
//
// THAT IS NO LONGER THE CURRENT STATE, and the past tense above is deliberate
// rather than stylistic. The submodule was tagged on 2026-09-27, so `@latest`
// is now a release rather than a moving head. Measured 2026-10-04:
//
//	go list -m -versions github.com/cleat-team/cleat/cleat  ->  v0.3.1 v0.3.2
//	`cleat init` writes  require github.com/cleat-team/cleat/cleat v0.3.2
//
// This paragraph said "has never been tagged", present tense, until cleat#3078.
//
// So the tests below build from this checkout instead. THAT IS A REDUCTION IN
// COVERAGE AND IT IS RECORDED AS ONE: they no longer prove a user's scaffold
// resolves over the network. The property is kept, in a form a pull request
// CAN check, by TestTheRootRequireNamesAPublishedVersion -- which reads this
// checkout's own cleat/go.mod rather than the pushed head, and so fails on the
// branch that introduces the bad version rather than the one after it.
//
// THE PRESCRIPTION THIS COMMENT USED TO CARRY -- "once it is tagged ... the
// network path becomes testable again; this helper can then go" -- IS NOW LIVE,
// and is filed as cleat#3083 rather than executed here. Not because it is
// unwelcome but because it is not this change: the helper has SEVEN call sites
// across FIVE test files, and removing it swaps what is under test from THIS
// checkout's SDK to the published release the proxy serves. That is a coverage
// decision in both directions, so it gets a change that argues for it, and the
// stale sentence gets this one. Deciding it here would be fixing the sweep
// where I happened to be looking -- which is what cleat#3066 did.
func resolveScaffoldAgainstThisCheckout(t *testing.T, proj string) {
	t.Helper()
	root := repoRoot(t)
	sdkDir := filepath.Join(root, "cleat")

	mod := filepath.Join(proj, "go.mod")
	existing, err := os.ReadFile(mod)
	if err != nil {
		t.Fatalf("read the scaffolded go.mod: %v", err)
	}
	if strings.Contains(string(existing), "replace github.com/cleat-team/cleat ") {
		return
	}
	added := string(existing)
	if !strings.Contains(added, "require github.com/cleat-team/cleat ") {
		added += "\nrequire github.com/cleat-team/cleat v0.0.0\n"
	}
	added += "\nreplace github.com/cleat-team/cleat => " + root + "\n" +
		"replace github.com/cleat-team/cleat/cleat => " + sdkDir + "\n"
	if err := os.WriteFile(mod, []byte(added), 0o644); err != nil {
		t.Fatalf("write the scaffolded go.mod: %v", err)
	}

	tidy := exec.Command("go", "mod", "tidy")
	tidy.Dir = proj
	if out, err := tidy.CombinedOutput(); err != nil {
		t.Fatalf("go mod tidy in the scaffolded project: %v\n%s", err, out)
	}
}
