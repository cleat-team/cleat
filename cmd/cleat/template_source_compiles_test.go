package main

import (
	"bytes"
	"fmt"
	"go/version"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// cleat#3100. Every .go file under cmd/cleat/templates/ carries
// `//go:build ignore` -- 7 of them, and nothing in the tree enables the tag --
// so `go list ./cmd/cleat/templates/...` matches no packages and no ordinary
// build here ever compiles them.
//
// SOMETHING USED TO, AND cleat#3099 REMOVED IT. The scaffold suites
// (a_scaffold_builds_test.go and its siblings) ran each template through
// `cleat init` + `cleat build` with resolveScaffoldAgainstThisCheckout pinning
// the generated project's SDK to this checkout, so a template that stopped
// compiling against the tree failed in the PR that broke it. cleat#3083 removed
// that helper, and cleat#3099 removed it -- correctly, for the reason given
// below. What it had been providing by ACCIDENT was this: the template source
// still compiled against this checkout, and after cleat#3099 nothing here
// compiled it.
//
// WHY REMOVING THE HELPER WAS RIGHT, because this test is not an argument to
// undo it: with the helper's `replace` directives in place the scaffold suites
// never exercised the module proxy they exist to prove, and the SDK submodule's
// tag (2026-09-27) took away the helper's reason to exist. The scaffolds must
// keep resolving from the published release. This test holds the OTHER half.
//
// WHY LOSING IT COSTS MORE THAN A CHECK. Without it an SDK change that breaks a
// template is green in its own PR and fails at the next tag, on an unrelated
// commit -- the cleat#2452 shape, "the introducing PR stayed green". The
// distance between cause and symptom is the whole cost.
//
// WHAT THIS IS NOT. It is a COMPILE check, not a behaviour check: it runs
// nothing and would not catch a template that compiles and is wrong. It does
// not replace the scaffold suites, which prove a user's scaffold resolves over
// the network against the published release.
//
// NO `cleat build` AND NO GENERATED PROJECT. The raw template source vets on its
// own -- `go mod tidy` fills in the module graph exactly as it does for a user's
// scaffold, and nothing else is generated. That is what makes it cheap enough to
// keep. Measured 2026-10-04 before writing this.
//
// THE PROBE COMPILES THE TEMPLATE'S NON-TEST SOURCE, and that scope is a
// decision rather than an oversight. The templates' `_test.go` files import
// cleattest, which reaches the SDK's own TEST dependencies -- a chain through
// cleat/engine, go-mssqldb's tests, azkeys and azcore -- and those modules are
// not in the build graph, so they are not in a CI job's module cache. Measured
// 2026-10-04: with fullstack's two test files present the resolved graph carries
// 10 references to azcore/azidentity/azkeys/azure-sdk-for-go; without them, 0.
// Including them did not widen the check, it made it fail -- red on two required
// checks, reported by cleat-review on cleat#3106. So a break in a template's
// test file is NOT caught here; nor was it caught before cleat#3099, because
// `cleat build` compiles no tests either.
func TestTemplateSourceCompilesAgainstTheTree(t *testing.T) {
	root := repoRoot(t)
	tmplDir := filepath.Join(root, "cmd", "cleat", "templates")
	goDirective := templateProbeGoDirective(t, root)

	entries, err := os.ReadDir(tmplDir)
	if err != nil {
		t.Fatalf("read %s: %v", tmplDir, err)
	}

	compiled := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		src := filepath.Join(tmplDir, name)

		// Collect the template's NON-TEST .go files preserving relative paths, so a
		// template with more than one package keeps them apart: fullstack has
		// main.go and proxy/main.go, and flattening them would redeclare main.
		//
		// _test.go is excluded on purpose -- the file doc gives the measurement.
		// They import cleattest, which reaches test dependencies of a dependency,
		// which are not in the build graph and so not in a CI job's module cache.
		var rels []string
		err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			rel, err := filepath.Rel(src, p)
			if err != nil {
				return err
			}
			rels = append(rels, rel)
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", src, err)
		}
		if len(rels) == 0 {
			continue // e.g. agent-python, which is not a Go template
		}
		compiled += len(rels)

		t.Run(name, func(t *testing.T) {
			scratch := t.TempDir()
			for _, rel := range rels {
				data, err := os.ReadFile(filepath.Join(src, rel))
				if err != nil {
					t.Fatalf("read %s: %v", rel, err)
				}
				// REUSE, not a re-derivation: this is the same strip `cleat init`
				// performs before writing the file into a user's project, so a
				// change to the tag form is covered here for free.
				stripped := stripScaffoldBuildTag(data)
				if bytes.Equal(stripped, data) {
					t.Errorf("%s carries no build constraint for stripScaffoldBuildTag to remove.\n"+
						"If the tag was dropped on purpose, this test no longer needs the strip; "+
						"if it was dropped by accident, the file has joined this repository's own "+
						"build, which is the module cycle stripScaffoldBuildTag's doc describes.", rel)
					continue
				}
				dst := filepath.Join(scratch, rel)
				if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
					t.Fatalf("mkdir %s: %v", filepath.Dir(dst), err)
				}
				if err := os.WriteFile(dst, stripped, 0o644); err != nil {
					t.Fatalf("write %s: %v", rel, err)
				}
			}

			gomod := fmt.Sprintf("module cleat-template-source-probe\n\ngo %s\n\n"+
				"require github.com/cleat-team/cleat/cleat v0.0.0\n\n"+
				"replace github.com/cleat-team/cleat => %s\n"+
				"replace github.com/cleat-team/cleat/cleat => %s\n",
				goDirective, root, filepath.Join(root, "cleat"))
			if err := os.WriteFile(filepath.Join(scratch, "go.mod"), []byte(gomod), 0o644); err != nil {
				t.Fatalf("write go.mod: %v", err)
			}

			// GOWORK=off because this module is not a member of the repo's go.work.
			//
			// GOPROXY=off, AND IT DOES NOT MAKE THE RUN HERMETIC -- it makes the
			// outcome a function of the module cache. That is why the file set above
			// has to stay inside the build graph. It is a correction rather than a
			// caution: an earlier version of this check set the flag, kept the test
			// files, passed locally against a cache months of building had stocked,
			// and failed in CI on two required checks, because the modules it needed
			// were test dependencies of a dependency. A local pass is not evidence
			// about CI when the instrument is the cache.
			env := append(os.Environ(), "GOWORK=off", "GOPROXY=off")

			// A user's generated project runs `go mod tidy` too -- the scaffold's own
			// tidyScaffold does -- so the probe is shaped like the real thing before it
			// is compiled. Measured 2026-10-04 on this file set: with tidy every
			// directive at or above 1.17 passes and the value is neutral; below 1.17
			// the graph is unpruned and this cannot resolve at all; and both commands
			// work only because the file set is inside the build graph.
			// See templateProbeGoDirective for the matrix.
			tidy := exec.Command("go", "mod", "tidy")
			tidy.Dir = scratch
			tidy.Env = env
			if out, err := tidy.CombinedOutput(); err != nil {
				t.Fatalf("the probe module could not resolve against this checkout's SDK.\n"+
					"This is a failure of the CHECK rather than a finding about the template, so read "+
					"the output below before looking for a template defect.\n"+
					"go mod tidy in %s:\n%v\n%s", scratch, err, out)
			}

			vet := exec.Command("go", "vet", "./...")
			vet.Dir = scratch
			vet.Env = env
			out, err := vet.CombinedOutput()
			if err != nil {
				t.Errorf("the template source under templates/%s no longer compiles against this "+
					"checkout's SDK.\n"+
					"This is what cleat#3100 exists to catch: without it, an SDK change that breaks a "+
					"template is green in its own PR and lands at the next tag, on an unrelated commit.\n"+
					"go vet ./... in %s:\n%v\n%s", name, scratch, err, out)
			}
		})
	}

	// A vacuity guard, not a census: if the walk found no template source at all
	// then this test measured nothing, and `ok` would say the opposite.
	if compiled == 0 {
		t.Fatalf("no .go files were found under %s, so nothing was compiled. "+
			"This is a failure of the check, not a finding about the tree -- if the templates moved, "+
			"fix the walk; do not delete this test.", tmplDir)
	}
}

// templateProbeGoDirective returns the `go` directive the stand-in project
// should declare: the newest of the two modules its template source compiles
// against.
//
// IT IS READ FROM THE TREE SO THE PROBE CANNOT DRIFT FROM A PIN, and on this
// file set the value does bear on the result in one direction: WITHOUT tidy, a
// directive older than the SDK module's own makes go want to bump it ("go:
// updates to go.mod needed"), while the tree's own does not -- so a pin would
// need bumping by hand every time the tree's Go version moved. WITH tidy, which
// the probe runs, every directive at or above 1.17 passes and the value is
// neutral. Below 1.17 the module graph is unpruned and tidy cannot resolve.
//
// Measured 2026-10-04 on the set this test copies -- fullstack's
// {main.go, proxy/main.go} x {1.16, 1.17, 1.25, 1.27.0} x {tidy, no tidy}.
//
// "go: updates to go.mod needed" HAS TWO CAUSES, and the file set decides which
// one applies -- which is part of why the test copies non-test source only. Over
// a set that INCLUDES the templates' _test.go files, the tree's own 1.27.0
// produces that message too, because those files need requires tidy has not
// added yet. On the shipped set it is the older-directive case above. An earlier
// version of this comment stated the second cause as if it were the only one,
// having measured on a set the test does not copy.
func templateProbeGoDirective(t *testing.T, root string) string {
	t.Helper()
	best := ""
	for _, p := range []string{
		filepath.Join(root, "go.mod"),
		filepath.Join(root, "cleat", "go.mod"),
	} {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		m := goDirectiveRE.FindSubmatch(data)
		if m == nil {
			t.Fatalf("%s carries no `go` directive; this test cannot choose one for the probe module", p)
		}
		if v := string(m[1]); best == "" || version.Compare(v, best) > 0 {
			best = v
		}
	}
	return best
}

// Anchored at the start of a line, so the `go 1.x` inside a comment or a
// replace block cannot be mistaken for the directive.
var goDirectiveRE = regexp.MustCompile(`(?m)^go (\S+)$`)
