package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/internal/closure"
)

// goFixtureExpectations is every directory under testdata/ holding top-level Go
// files, with what analysis must say about it.
//
// A TABLE WITH PER-FIXTURE OUTCOMES, not "they must all build". testdata/errors
// exists to be REJECTED -- its package comment says it "contains deliberately
// invalid workflow code to test the transformer's validation rules" -- so a
// blanket must-build rule could only accommodate it by skipping it, which is
// how a fixture stops being checked.
//
// Kept as an exception it becomes the guard's known-positive instead: the one
// entry that proves this test can report a failure at all. A version of this
// that only ever asserts success passes equally against a checker that has
// stopped checking, which is precisely the defect cleat#1313 is about.
var goFixtureExpectations = map[string]string{
	// "" means analysis must report no threading errors.
	"allhostcalls":   "",
	"autothread":     "",
	"basic":          "",
	"cancelpoll":     "",
	"clew-lifecycle": "",
	"crashcall":      "",
	"crosslang":      "",
	"deferfunc":      "",
	"durablesend":    "",
	"fencereentry":   "",
	"generics":       "",
	"minimal-wf":     "",
	"noargs":         "",
	"nowms":          "",
	"rejectpromise":  "",
	"resolvepromise": "",
	"scheduleinvoke": "",
	"signalworkflow": "",
	"spin":           "",
	"updatedispatch": "",

	// The known-positive. A substring of the message, not just "some error":
	// "it must fail" is satisfied by failing for any reason at all, including
	// one this fixture was never written to exercise.
	"errors": "does not have a HostCalls parameter",
}

// TestEveryGoFixtureMatchesItsExpectedVerification is cleat#1313.
//
// testdata/generics -- the repository's own generics fixture -- did not survive
// `cleat build`, and five test files referencing it all passed, because none ran
// the stage that fails. wasm/generics_build_test.go calls BuildOutputs directly,
// which skips VerifyThreading entirely, and no CI job runs `cleat build` on a Go
// example: `git grep 'cleat build' .github/workflows/` finds only --target rust
// and --target python.
//
// The code's own comment at internal/closure/threading.go said so already --
// "nothing in CI runs cleat build on a Go example" -- which is the part worth
// noting: the gap was documented at the site and documenting it changed nothing.
// A comment cannot go red.
//
// This drives analyze(), the same function cmd/cleat's build path calls, so it
// covers VerifyThreading rather than the generation stage the existing tests
// already reach. The WASM compile beyond it is covered for a fixture by
// TestGeneratedAdapterCompilesForEveryHostCall.
func TestEveryGoFixtureMatchesItsExpectedVerification(t *testing.T) {
	dir := testdataDir(t)

	// The population is DISCOVERED, not listed twice. A fixture added to
	// testdata/ and not to the table fails here rather than being silently
	// uncovered -- which is the failure mode this whole test exists to remove,
	// one level up.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading testdata: %v", err)
	}
	var found []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		matches, _ := filepath.Glob(filepath.Join(dir, e.Name(), "*.go"))
		if len(matches) == 0 {
			continue // e.g. vet-checks, which holds no top-level Go files
		}
		found = append(found, e.Name())
	}
	sort.Strings(found)

	if len(found) < 15 {
		t.Fatalf("found %d Go fixtures under testdata/, expected at least 15 -- "+
			"this discovery is matching almost nothing and the table below would "+
			"be checked against an empty population", len(found))
	}

	for _, name := range found {
		want, listed := goFixtureExpectations[name]
		if !listed {
			t.Errorf("testdata/%s holds Go files and is not in goFixtureExpectations.\n\n"+
				"Add it with \"\" if it must verify cleanly, or with a substring of the "+
				"error it must produce. An unlisted fixture is an unchecked one.", name)
			continue
		}
		t.Run(name, func(t *testing.T) {
			// analyze() takes a package pattern. The ./ prefix is required:
			// without it `testdata/basic` is read as an import path, every
			// invocation fails as a bad pattern, and a sweep reports every
			// fixture broken. Measured while scoping this -- 21 of 21 "failed",
			// and only a known-good control caught it.
			pattern := "./" + filepath.ToSlash(filepath.Join("..", "..", "testdata", name))
			threadingErrs := buildBlockingErrors(t, pattern)

			var msgs []string
			for _, e := range threadingErrs {
				msgs = append(msgs, e.Message)
			}
			joined := strings.Join(msgs, "\n")

			if want == "" {
				if len(threadingErrs) != 0 {
					t.Errorf("testdata/%s fails verification but is expected to pass:\n%s", name, joined)
				}
				return
			}
			if len(threadingErrs) == 0 {
				t.Errorf("testdata/%s is expected to be REJECTED and verified cleanly.\n\n"+
					"Either the fixture was repaired -- update the table -- or the check it "+
					"exercises stopped working, which is the regression this entry exists to "+
					"catch.", name)
				return
			}
			if !strings.Contains(joined, want) {
				t.Errorf("testdata/%s was rejected, but not for the expected reason.\n"+
					"want a message containing: %q\ngot:\n%s\n\n"+
					"Failing for some other reason satisfies \"it must fail\" while proving "+
					"nothing about the rule this fixture covers.", name, want, joined)
			}
		})
	}

	// And the reverse: an entry naming a fixture that no longer exists is a
	// grant covering nothing, and it silently shrinks the checked population.
	have := map[string]bool{}
	for _, n := range found {
		have[n] = true
	}
	for name := range goFixtureExpectations {
		if !have[name] {
			t.Errorf("goFixtureExpectations names %q, which is not a Go fixture under testdata/", name)
		}
	}
}

// TestAGenericFunctionIsNotAnEntryPoint pins the specific classification
// cleat#1313 turned on, because the table above would also go green if
// testdata/generics were simply edited to remove its generics.
//
// Two functions, caught two different ways, and only one of them was caught at
// all before the fix:
//
//   - Process[T] returns (T, error), so verifyEntryPointResults rejected it --
//     the right refusal for the wrong reason. The defect is that it is not an
//     entry point, not that T is not a string.
//   - GenericLeaf[T] returns error alone, passes that check, and was therefore
//     classified as an entry point and EXPORTED. Nothing objected.
//
// The second is the one worth a test: it is silent, and it is the shape the
// threading.go comment warns about for non-generic helpers too.
func TestAGenericFunctionIsNotAnEntryPoint(t *testing.T) {
	result, _, _, raw, _, tr := analyze("./../../testdata/generics")
	threadingErrs := dropAutoThreaded(raw, tr)

	if len(threadingErrs) != 0 {
		var msgs []string
		for _, e := range threadingErrs {
			msgs = append(msgs, e.Message)
		}
		t.Fatalf("testdata/generics does not verify:\n%s", strings.Join(msgs, "\n"))
	}

	for _, ep := range result.EntryPoints {
		short := ep[strings.LastIndex(ep, ".")+1:]
		if short == "Process" || short == "GenericLeaf" {
			t.Errorf("%s is a generic function and is classified as an entry point.\n\n"+
				"An entry point is exported with a concrete signature -- wasm/exports.go "+
				"declares `var __r string` and emits `return []byte(__r)` -- so there is "+
				"nothing to instantiate T with. GenericLeaf in particular returns error "+
				"alone, so the result check cannot catch it and it ships.", short)
		}
	}
	if len(result.EntryPoints) != 1 {
		t.Errorf("testdata/generics has %d entry point(s) %v, want 1 (EntryPoint).\n\n"+
			"Its own doc comments say which is which: Process is \"a generic workflow "+
			"helper ... in the durable closure\" and GenericLeaf is \"a generic durable "+
			"leaf\".", len(result.EntryPoints), result.EntryPoints)
	}
}

// buildBlockingErrors returns the threading errors that would actually stop
// `cleat build`, which is NOT the same set analyze() returns.
//
// VerifyThreading reports the PRE-TRANSFORM state deliberately, so it names
// functions auto-threading is about to fix; cmd/cleat drops those with
// dropAutoThreaded before deciding, and failing on them once made `cleat build`
// reject packages the next stage was designed to repair (IMPROVEMENT-PLAN
// 3.229).
//
// The first version of this test omitted that step and reported
// testdata/autothread as broken -- a fixture that exists precisely to have
// pre-transform errors and build cleanly anyway. It was wrong in the direction
// that matters least (over-reporting), but a test modelling a decision has to
// model the whole decision, or it is asserting something the product never
// claimed.
func buildBlockingErrors(t *testing.T, pattern string) []closure.ThreadingError {
	t.Helper()
	_, _, _, raw, _, tr := analyze(pattern)
	return dropAutoThreaded(raw, tr)
}
