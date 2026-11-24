package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Tests the checker directly, as a built binary, in the same discipline as
// python_build_refuses_nondeterminism_test.go:
//
//   - assertions are on the TEXT of the report, not only the exit status, because "exit 1" is satisfied by a checker that
//     fails for any reason;
//   - a known-positive arm AND a known-negative arm, because either alone is satisfied by a checker that always refuses or
//     never does;
//   - a known-limit arm that PINS what the checker does not see, so the limit cannot move unrecorded;
//   - three outcomes with three statuses: 0 clean, 1 finding, 2 UNMEASURED.
var bin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "tscheck")
	if err != nil {
		panic(err)
	}
	bin = filepath.Join(dir, "tscheck")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		panic("building tscheck: " + string(out))
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func run(t *testing.T, path string) (string, int) {
	t.Helper()
	out, err := exec.Command(bin, path).CombinedOutput()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return string(out), 0
	case errors.As(err, &ee):
		return string(out), ee.ExitCode()
	default:
		t.Fatalf("could not run tscheck: %v", err)
		return "", -1
	}
}

func TestKnownPositivesAreRefusedForTheDeterminismReason(t *testing.T) {
	for _, c := range []struct{ file, code, name string }{
		{"positive_timer.ts", "TS001", "setTimeout"},
		{"positive_weakref.ts", "TS002", "WeakRef"},
		{"positive_global_qualified.ts", "TS003", "performance"},
		{"positive_template_expr.ts", "TS001", "setInterval"}, // inside ${ }: code, not template text
	} {
		t.Run(c.file, func(t *testing.T) {
			out, code := run(t, filepath.Join("testdata", c.file))
			if code != 1 {
				t.Fatalf("exit %d, want 1 (a finding). A non-1 here is a checker that does not refuse.\n%s", code, out)
			}
			// the code AND the name AND a file:line: an exit status alone would be satisfied by any failure
			if !strings.Contains(out, c.code+" "+c.name+":") || !strings.Contains(out, c.file+":") {
				t.Errorf("the report does not name %s %s with a location:\n%s", c.code, c.name, out)
			}
		})
	}
}

func TestKnownNegativesAreNotOverReported(t *testing.T) {
	out, code := run(t, filepath.Join("testdata", "negative_mentions.ts"))
	if code != 0 {
		t.Fatalf("exit %d, want 0: comments, strings, template text, object keys, methods on other objects and the SHIMMED "+
			"Date/Math.random must not be refused.\n%s", code, out)
	}
	if !strings.Contains(out, "scanned ") || strings.Contains(out, "scanned 0 ") {
		t.Errorf("a clean report must say it scanned something, so 'looked at nothing' is visible:\n%s", out)
	}
}

// The known-limit arm. It asserts the file PASSES, i.e. it pins what the check cannot see. When someone teaches the checker
// to see one of these, this fails, and the limits list in PHASE2.md is the thing to update.
func TestKnownLimitsAreStillLimits(t *testing.T) {
	out, code := run(t, filepath.Join("testdata", "limit_computed.ts"))
	if code != 0 {
		t.Fatalf("exit %d: a limit documented as unseen is now seen; update PHASE2.md's list.\n%s", code, out)
	}
}

// The known-over-report arm: the safe direction of the same limit. Pinned so it is a decision, not an accident.
func TestKnownOverReportsAreStillOverReports(t *testing.T) {
	out, code := run(t, filepath.Join("testdata", "limit_overreport.ts"))
	if code != 1 || !strings.Contains(out, "TS001 setTimeout:") || !strings.Contains(out, "TS003 performance:") {
		t.Errorf("a documented over-report changed (exit %d); update PHASE2.md's list if this is intended:\n%s", code, out)
	}
}

func TestACheckThatCouldNotLookIsNotAPass(t *testing.T) {
	for _, f := range []string{"unmeasured_comment.ts", "unmeasured_empty.ts", "unmeasured_template.ts"} {
		t.Run(f, func(t *testing.T) {
			out, code := run(t, filepath.Join("testdata", f))
			if code != 2 {
				t.Fatalf("exit %d, want 2 (UNMEASURED). The unterminated comment in unmeasured_comment.ts hides a setTimeout; "+
					"reporting it clean, or as a finding about the file, would be wrong.\n%s", code, out)
			}
			if !strings.Contains(out, "UNMEASURED") || !strings.Contains(out, "not a finding about the file") {
				t.Errorf("exit 2 must say UNMEASURED and that it is the check that failed:\n%s", out)
			}
		})
	}
	if _, code := run(t, filepath.Join("testdata", "does_not_exist.ts")); code != 2 {
		t.Errorf("an unreadable file exited %d, want 2", code)
	}
}

func TestAnAllowMarkerNeedsAReasonAndIsReported(t *testing.T) {
	out, code := run(t, filepath.Join("testdata", "allowed_with_reason.ts"))
	if code != 0 || !strings.Contains(out, "ALLOWED by marker") || !strings.Contains(out, "TS001") {
		t.Errorf("a marker with a reason should pass AND say what it allowed (exit %d):\n%s", code, out)
	}
	out, code = run(t, filepath.Join("testdata", "allow_without_reason.ts"))
	if code != 1 || !strings.Contains(out, "TS001 setTimeout:") {
		t.Errorf("a marker without a reason must not suppress anything (exit %d):\n%s", code, out)
	}
}
