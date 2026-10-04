package main

import (
	"os"
	"os/exec"
	"path/filepath"
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

// resolveScaffoldAgainstThisCheckout WAS HERE, and went in cleat#3083.
//
// It rewrote each scaffolded project's go.mod with `replace` directives so the
// SDK resolved from this checkout rather than the proxy. That existed for one
// reason, recorded in its own comment: the SDK submodule had never been tagged,
// so a scaffold's `@latest` was a pseudo-version of the DEFAULT BRANCH -- which
// made the version a scaffold resolved to whatever the pushed head said, so a
// pull request could not validate that path at all (cleat#2452).
//
// The submodule was tagged on 2026-09-27 (cleat/v0.3.1, cleat/v0.3.2), and the
// helper's own comment predicted what would follow: "once it is tagged ... the
// network path becomes testable again; this helper can then go". Measured on
// 2026-10-04 before removing it, with every call site disabled:
//
//	the eight scaffold/template/tutorial suites  ->  8 PASS, GO_TEST_RC=0
//	the snippet guard, its scratch module given
//	`go get ...@latest` instead of the helper    ->  passes, and its
//	                                                  known-positive still
//	                                                  fires for its recorded
//	                                                  reason
//
// WHAT THIS COSTS, and it is MORE than "no longer caught here", which is what
// this note said first and why it was corrected during review.
//
// These tests now build against the PUBLISHED release rather than this
// checkout's SDK. And because every source file under templates/ carries
// `//go:build ignore` -- seven of them, nothing enables the tag, and `go list
// ./cmd/cleat/templates/...` matches no packages -- THESE SUITES WERE THE ONLY
// IN-REPO COMPILATION OF THE TEMPLATE SOURCE AGAINST THE TREE. After this there
// is none, so an SDK change that breaks a template is green in its own PR and
// lands at the next tag, on an unrelated commit. That is the cleat#2452 shape
// ("the introducing PR stayed green") reproduced for a new property, and it is
// a worse outcome than losing a check -- losing THIS check is what makes it
// land far from its cause.
//
// IT MAY NOT NEED TO BE A TRADE. A check on the template SOURCE rather than on
// a scaffolded project holds both: copy a template's .go file to a scratch
// module, strip the `//go:build ignore` line, add `replace ... => <checkout>/cleat`,
// and `go vet ./...` compiles it against the tree with no generated files and no
// `cleat build`. Measured to return rc=0 with no output while genuinely naming
// cleat.HostCalls. That is a separate change and is not made here; it is
// recorded because this note would otherwise read as though the property were
// gone, and it is available.
//
// What is bought is the property the helper's own comment said was given up --
// these tests prove a user's scaffold resolves over the network.
// TestTheRootRequireNamesAPublishedVersion still guards the version the
// template names, which is a different property.
