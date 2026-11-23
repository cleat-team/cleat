package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/wasm"
)

// cleat#4383. `cleat build --target python` refuses a componentize-py that is not the measured version; what the reader is TOLD must match what happened.

func shExit(t *testing.T, code int) error {
	t.Helper()
	err := exec.Command("sh", "-c", fmt.Sprintf("exit %d", code)).Run()
	if err == nil {
		t.Fatalf("PRECONDITION FAILED: `sh -c 'exit %d'` succeeded, so this measures nothing", code)
	}
	return err
}

func TestAPythonBuildFailureSaysWhatFailed(t *testing.T) {
	const compileAdvice = "Make sure the Python file has a @cleat_entry decorated function."
	refused := shExit(t, wasm.PythonBuildExitUnmeasuredComponentizePy)

	t.Run("a refusal does not send the reader to their source", func(t *testing.T) {
		for _, err := range []error{refused, fmt.Errorf("running the build: %w", refused)} {
			msg := pythonBuildFailureMessage(err)
			if !strings.Contains(msg, "refused") || !strings.Contains(msg, "nothing was built") {
				t.Errorf("a refusal must say it was one and that nothing was built: %q", msg)
			}
			if strings.Contains(msg, compileAdvice) || strings.Contains(msg, "@cleat_entry") {
				t.Errorf("advice about the workflow file is wrong for a build that never compiled: %q", msg)
			}
		}
	})

	t.Run("a compile that ran and failed still gets the compile advice, unchanged", func(t *testing.T) {
		for _, err := range []error{shExit(t, 1), errors.New("boom")} {
			msg := pythonBuildFailureMessage(err)
			want := fmt.Sprintf("Error: componentize-py failed: %v\n", err) +
				compileAdvice + "\n" +
				"Check that (1) the Python file has valid syntax, (2) the @cleat_entry function exists and is not async, (3) all imports are available.\n"
			if msg != want {
				t.Errorf("the compile-failure message changed.\n  got:  %q\n  want: %q", msg, want)
			}
			if strings.Contains(msg, "refused") {
				t.Errorf("a failed compile must not read as a refusal: %q", msg)
			}
		}
	})

	t.Run("another status is a failed compile, not a refusal", func(t *testing.T) {
		msg := pythonBuildFailureMessage(shExit(t, wasm.PythonBuildExitUnmeasuredComponentizePy+1))
		if !strings.Contains(msg, compileAdvice) || strings.Contains(msg, "refused") {
			t.Errorf("a status next to the refusal's must not be taken for it: %q", msg)
		}
	})
	// The third branch, "no build script anywhere", needs a *pythonBuildScriptError, which only package wasm can build; its own tests cover IsPythonBuildScriptNotFound.
}

func TestAllowUnmeasuredComponentizePyIsAnExplicitPythonOnlyOverride(t *testing.T) {
	env := wasm.AllowUnmeasuredComponentizePyEnv
	// isolate: t.Setenv registers the restore, and the variable is then removed so "not set" is a real state
	reset := func(t *testing.T) {
		t.Helper()
		t.Setenv(env, "")
		if err := os.Unsetenv(env); err != nil {
			t.Fatal(err)
		}
	}
	read := func() (string, bool) { return os.LookupEnv(env) }

	t.Run("not asked: nothing is set", func(t *testing.T) {
		reset(t)
		if err := applyAllowUnmeasuredComponentizePy(false, "python"); err != nil {
			t.Fatal(err)
		}
		if v, ok := read(); ok {
			t.Errorf("the override was set (%q) without being asked for", v)
		}
	})
	t.Run("not asked, and the caller exported it: left alone", func(t *testing.T) {
		reset(t)
		t.Setenv(env, "1")
		if err := applyAllowUnmeasuredComponentizePy(false, "go"); err != nil {
			t.Fatal(err)
		}
		if v, _ := read(); v != "1" {
			t.Errorf("a variable the caller exported was changed to %q", v)
		}
	})
	t.Run("asked on the python target: the script's variable is 1", func(t *testing.T) {
		reset(t)
		if err := applyAllowUnmeasuredComponentizePy(true, "python"); err != nil {
			t.Fatal(err)
		}
		if v, ok := read(); !ok || v != "1" {
			t.Errorf("the variable build_wasm.py reads is %q (set: %v), want 1", v, ok)
		}
	})
	t.Run("asked on another target: refused, and nothing is set", func(t *testing.T) {
		for _, target := range []string{"go", "rust", "java", "ts", "csharp"} {
			reset(t)
			err := applyAllowUnmeasuredComponentizePy(true, target)
			if err == nil || !strings.Contains(err.Error(), "only to --target python") || !strings.Contains(err.Error(), target) {
				t.Errorf("--target %s: want a refusal naming the target, got %v", target, err)
			}
			if v, ok := read(); ok {
				t.Errorf("--target %s: the variable was set (%q) although the flag was refused", target, v)
			}
		}
	})
}

// The two halves above are functions; this is the wiring in main(), through the real binary, on a target that needs no Python: the flag must be REGISTERED
// (an unregistered flag exits 2 with "flag provided but not defined") and applyAllowUnmeasuredComponentizePy must be CALLED with the target (a flag that parsed
// and was never acted on would go on and try to build). Both are invisible to a test that calls the function directly.
func TestTheOverrideFlagIsRegisteredAndRefusedOffTheListedTarget(t *testing.T) {
	if cleatBinary == "" {
		t.Skip("cleatBinary not built (short mode)")
	}
	cmd := exec.Command(cleatBinary, "build", "--target", "go", "--allow-unmeasured-componentize-py")
	cmd.Dir = t.TempDir()
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("want a nonzero exit, got err=%v, output:\n%s", err, out)
	}
	if ee.ExitCode() != 1 {
		t.Errorf("exit %d, want 1 (2 would be an unregistered flag):\n%s", ee.ExitCode(), out)
	}
	if strings.Contains(string(out), "flag provided but not defined") {
		t.Fatalf("the flag is not registered on `cleat build`:\n%s", out)
	}
	if !strings.Contains(string(out), "--allow-unmeasured-componentize-py applies only to --target python, not --target go") {
		t.Errorf("the flag was accepted for the go target instead of refused; main() does not call applyAllowUnmeasuredComponentizePy with the target:\n%s", out)
	}
}
