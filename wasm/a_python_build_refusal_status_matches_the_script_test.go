package wasm

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"testing"
)

// cleat#4383: build_wasm.py REFUSES a build whose componentize-py is not the measured version, with exit status 3 and not 1, so that `cleat build` can tell a
// refusal (a problem with the environment, already explained on stderr) from a compile that ran and failed (advice about the workflow file). The two sides
// name the status and the override variable separately, in two languages, and nothing but this test says they are the same.

func scriptConstants(t *testing.T) (exitStatus int, allowEnv string) {
	t.Helper()
	src, err := os.ReadFile("../python-sdk/scripts/build_wasm.py")
	if err != nil {
		t.Fatalf("UNMEASURED: cannot read python-sdk/scripts/build_wasm.py: %v (this is a failure of the check, not a finding about the tree)", err)
	}
	// anchored at the start of a line, so a comment that merely QUOTES the name cannot satisfy it
	status := regexp.MustCompile(`(?m)^EXIT_UNMEASURED_COMPONENTIZE_PY = ([0-9]+)\s*$`).FindSubmatch(src)
	env := regexp.MustCompile(`(?m)^ALLOW_UNMEASURED_ENV = "([A-Z_]+)"\s*$`).FindSubmatch(src)
	if status == nil || env == nil {
		t.Fatalf("UNMEASURED: build_wasm.py no longer declares EXIT_UNMEASURED_COMPONENTIZE_PY and ALLOW_UNMEASURED_ENV as plain module-level assignments, so this test cannot read them (status matched: %v, env matched: %v)", status != nil, env != nil)
	}
	n, err := strconv.Atoi(string(status[1]))
	if err != nil {
		t.Fatalf("UNMEASURED: the exit status %q is not a number: %v", status[1], err)
	}
	return n, string(env[1])
}

func TestPythonBuildExitStatusMatchesTheScript(t *testing.T) {
	status, env := scriptConstants(t)
	if status != PythonBuildExitUnmeasuredComponentizePy {
		t.Errorf("build_wasm.py refuses with exit %d and wasm.PythonBuildExitUnmeasuredComponentizePy is %d: `cleat build` would treat a refusal as a failed compile and print advice about the user's source", status, PythonBuildExitUnmeasuredComponentizePy)
	}
	if env != AllowUnmeasuredComponentizePyEnv {
		t.Errorf("build_wasm.py reads %q and `cleat build --allow-unmeasured-componentize-py` sets %q: the flag would do nothing", env, AllowUnmeasuredComponentizePyEnv)
	}
	if status == 0 || status == 1 || status == 2 {
		t.Errorf("exit %d is already taken (0 ok, 1 a build that ran and failed, 2 argparse's usage error), so a refusal could not be told from it", status)
	}
}

func exitError(t *testing.T, code int) error {
	t.Helper()
	err := exec.Command("sh", "-c", fmt.Sprintf("exit %d", code)).Run()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != code {
		t.Fatalf("PRECONDITION FAILED: `sh -c 'exit %d'` did not yield an exit error with that status (%v), so this measures nothing", code, err)
	}
	return err
}

func TestIsPythonComponentizeVersionRefused(t *testing.T) {
	refused := exitError(t, PythonBuildExitUnmeasuredComponentizePy)
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"the refusal status", refused, true},
		{"the refusal status, wrapped", fmt.Errorf("running build_wasm.py: %w", refused), true},
		{"a build that ran and failed (exit 1)", exitError(t, 1), false},
		{"an argparse usage error (exit 2)", exitError(t, 2), false},
		{"another nonzero status", exitError(t, 4), false},
		{"an error that is not an exit status", errors.New("boom"), false},
		{"no error", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsPythonComponentizeVersionRefused(tc.err); got != tc.want {
				t.Errorf("IsPythonComponentizeVersionRefused(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
