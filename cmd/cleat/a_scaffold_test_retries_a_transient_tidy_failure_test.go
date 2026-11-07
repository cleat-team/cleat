package main

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// cleat#3659, owner decision D61 (option 3). TestEveryGoTemplateScaffoldsIntoAProjectThatBuilds runs `cleat
// init`, which runs `go mod tidy` in the scaffold, and the checksum database once reset an HTTP/2 stream
// under it (run 40907847227). init exited 0 as designed (D45), `cleat build` then failed, and the test
// failed for a reason that had nothing to do with the template.
//
// The test now re-runs `go mod tidy` in the scaffold when init warned AND the cause is a transient one,
// then builds. THE PRODUCT DOES NOT RETRY (D61): a user who meets this is told, by option 2, to re-run the
// command, and this is that re-run done by the test.
//
// A pass must not hide what the user was told, so the retry path ASSERTS the warning, and asserts its
// content: init's closing line must say the cause is transient. A test that retried silently would stay
// green over a regression that dropped that sentence. A permanent failure is not retried, and neither is
// a run whose init did not warn.
//
// The logic is here, behind a function, so that every branch of it can be exercised without a network: a
// tidy that is a function can fail transiently, permanently or not at all on demand. The real scaffold test
// can also meet a transient failure end to end, with a GOPROXY that answers 503 for its first seconds and
// then forwards (cleat-review measured exactly that on cleat#3674: init's tidy got 503s, its closing line
// carried the option 2 sentence, the re-run resolved the scaffold and the build passed), but that needs a
// proxy and the clock, so it is not a test that runs here.

// scaffoldTidyRetries is how many times the scaffold's `go mod tidy` is re-run after a transient warning.
// A transient failure usually clears on the next try, and a run that fails this many times in a row is a
// network that is not recovering, which is a finding and not something to wait out.
const scaffoldTidyRetries = 2

// scaffoldTidyPause is how long to wait before each re-run.
const scaffoldTidyPause = 3 * time.Second

const transientClause = "looks like a transient network error"

// tidyFunc runs `go mod tidy` in dir and returns its combined output.
type tidyFunc func(dir string) (string, error)

func goModTidy(dir string) (string, error) {
	cmd := exec.Command("go", "mod", "tidy")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// resolveAfterATransientInitWarning returns nil when there is nothing to do or the re-run resolved the
// scaffold's dependencies, and an error saying why the build below cannot be trusted to reach them.
func resolveAfterATransientInitWarning(t *testing.T, initOut, proj string, tidy tidyFunc, pause time.Duration) error {
	t.Helper()
	if !strings.Contains(initOut, "WARNING: ") {
		return nil
	}
	if !transientTidyCause(initOut) {
		return fmt.Errorf("init could not resolve its dependencies, and the cause is not one a re-run clears, so it is not retried:\n%s", initOut)
	}
	lines := strings.Split(strings.TrimRight(initOut, "\n"), "\n")
	if last := lines[len(lines)-1]; !strings.Contains(last, transientClause) {
		return fmt.Errorf("the cause is transient but init's closing line does not say so (D61 option 2), so the user was not told:\n%s", last)
	}
	t.Logf("init warned that its dependencies were not resolved, and the cause is transient; re-running `go mod tidy` in %s up to %d times:\n%s",
		proj, scaffoldTidyRetries, initOut)

	for i := 1; i <= scaffoldTidyRetries; i++ {
		time.Sleep(pause)
		out, err := tidy(proj)
		if err == nil {
			t.Logf("`go mod tidy` resolved the scaffold on re-run %d", i)
			return nil
		}
		if !transientTidyCause(out) {
			return fmt.Errorf("re-run %d of `go mod tidy` failed with a cause that is not transient, so it is not retried again:\n%s", i, out)
		}
	}
	return fmt.Errorf("`go mod tidy` was still failing with a transient cause after %d re-runs; the network is not recovering", scaffoldTidyRetries)
}

// realInitOutput is the output of an init whose tidy failed with the one confirmed transient cause
// (realTransientTidyLog, from run 40907847227), with its closing line produced by the real report and not
// typed here. transient is what tidyScaffold would record; passing false builds the output a build WITHOUT
// option 2 would have printed.
func realInitOutput(cause string, transient bool) string {
	var closing bytes.Buffer
	(&tidyFailure{dir: "p_workflow", transient: transient}).report(&closing)
	return "\nWarning: could not resolve dependencies (exit status 1).\nRun 'go mod tidy' in p_workflow before building.\n" +
		cause + "\n\nCreated workflow project in p_workflow/\n" + closing.String()
}

func TestAScaffoldTestRetriesATransientTidyFailureAndNothingElse(t *testing.T) {
	const permanent = "cannot find module providing package x: module lookup disabled by GOPROXY=off"
	transientOut := realInitOutput(realTransientTidyLog, transientTidyCause(realTransientTidyLog))

	// The known positive for everything below: the fixture really is classified transient and really does
	// end with the sentence, so a "no retry" result is not the fixture failing to be transient.
	if !transientTidyCause(transientOut) || !strings.Contains(transientOut, transientClause) {
		t.Fatalf("the fixture is not a transient init output, so this test measures nothing:\n%s", transientOut)
	}

	// failing is a tidy that fails n times with out, then succeeds.
	type fake struct {
		calls int
		fn    tidyFunc
	}
	failing := func(n int, out string) *fake {
		f := &fake{}
		f.fn = func(string) (string, error) {
			f.calls++
			if f.calls <= n {
				return out, fmt.Errorf("exit status 1")
			}
			return "", nil
		}
		return f
	}

	for _, tc := range []struct {
		name      string
		initOut   string
		tidy      *fake
		wantErr   string // a substring; "" means no error
		wantCalls int
	}{
		{"init did not warn: nothing to do, no re-run", "Created workflow project in p_workflow/\n", failing(0, ""), "", 0},
		{"a transient warning, and the first re-run resolves it", transientOut, failing(0, ""), "", 1},
		{"a transient warning, and the second re-run resolves it", transientOut, failing(1, realTransientTidyLog), "", 2},
		{"a transient warning that does not clear: a finding after the last re-run", transientOut, failing(99, realTransientTidyLog), "still failing", scaffoldTidyRetries},
		{"a permanent warning is not retried", realInitOutput(permanent, false), failing(0, ""), "not one a re-run clears", 0},
		{"a re-run that fails permanently is not retried again", transientOut, failing(99, permanent), "not transient", 1},
		// THE WARNING IS ASSERTED, NOT JUST OBSERVED: a transient cause whose closing line does not say so
		// (init without option 2) must fail the test and not be quietly retried to green.
		{"a transient cause that init did not say was transient", realInitOutput(realTransientTidyLog, false), failing(0, ""), "does not say so", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := resolveAfterATransientInitWarning(t, tc.initOut, "p_workflow", tc.tidy.fn, 0)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("unexpected error: %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Errorf("error = %v, want one containing %q", err, tc.wantErr)
			}
			if tc.tidy.calls != tc.wantCalls {
				t.Errorf("`go mod tidy` was re-run %d times, want %d", tc.tidy.calls, tc.wantCalls)
			}
		})
	}
}
