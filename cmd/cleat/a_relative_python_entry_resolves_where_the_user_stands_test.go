package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// cleat#1836. `cleat build --target python --entry <relative>:<func>` resolves
// the path against the process working directory, which is where the user is
// standing -- not against the SDK root, which is a directory cleat picked and
// the user cannot see.
//
// WHAT WAS BROKEN. Both documented Python example commands failed, from every
// working directory, because wasm/build.go runs build_wasm.py with
// `cmd.Dir = sdkRoot` and handed it the user's path verbatim. The script then
// resolved it relative to python-sdk/.
//
// HOW THIS TEST SEES IT WITHOUT componentize-py. The failure is inside
// build_wasm.py, past the `componentize-py not found` check in runBuildPython,
// so a machine without the toolchain never reaches it and an end-to-end test
// would pass vacuously. A STUB on PATH gets past that check; the script then
// runs for real, and validate_entry -- which reads the file and needs nothing
// installed -- is the first thing it does. So the real resolution is exercised
// and only the compile is faked.
//
// The stub is not a fake of the mechanism under test. It stands in for a step
// that happens strictly AFTER the assertion, which is the difference between a
// stub that preserves a measurement and one that replaces it.
//
// THE DISCRIMINATOR IS THE SCRIPT'S OWN LINE, not the absence of an error.
// "Validated entry: <abs> -> <func>()" is printed only on the path where the
// file was found and read. Asserting merely that "Entry file not found" is
// absent would pass for a command that failed EARLIER -- on a bad flag, a
// missing binary, anything -- which is a check that cannot disagree.
func TestARelativePythonEntryResolvesWhereTheUserStands(t *testing.T) {
	if testing.Short() {
		t.Skip("needs the cleat binary, which TestMain does not build in short mode")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not installed")
	}
	// The determinism gate (cleat#1833) runs BEFORE the build and imports
	// cleat_sdk, which uses `X | None` and so needs 3.10. On an older
	// interpreter the build stops there and never reaches the resolution --
	// which the UNMEASURED precondition below reports rather than passing, and
	// which is how this requirement was found.
	if v, ok := pythonAtLeast(pythonSDKMinVersion); !ok {
		t.Skipf("python3 is %s; the determinism gate needs cleat_sdk, which requires "+
			">= %s, so the build stops before the entry is resolved", v, pythonSDKMinVersion)
	}

	root := repoRoot(t)
	example := filepath.Join(root, "examples", "python-hello")
	if _, err := os.Stat(filepath.Join(example, "hello_workflow.py")); err != nil {
		t.Skipf("examples/python-hello/hello_workflow.py is not present: %v", err)
	}

	// A componentize-py that refuses. Everything this test asserts on happens
	// before the script invokes it.
	stubDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(stubDir, "componentize-py"),
		[]byte("#!/bin/sh\necho 'stub componentize-py: this test does not compile' >&2\nexit 9\n"),
		0o755); err != nil {
		t.Fatalf("writing the componentize-py stub: %v", err)
	}

	build := func(t *testing.T, cwd, entry string) string {
		t.Helper()
		cmd := exec.Command(cleatBinary, "build", "--target", "python", "--entry", entry)
		cmd.Dir = cwd
		cmd.Env = append(os.Environ(),
			"PATH="+stubDir+string(os.PathListSeparator)+os.Getenv("PATH"),
			// The determinism gate ahead of the build needs the SDK importable.
			// Without this the run stops there, the precondition below reports
			// UNMEASURED, and nothing about the entry path is measured.
			"PYTHONPATH="+filepath.Join(root, "python-sdk"))
		out, err := cmd.CombinedOutput()
		var exitErr *exec.ExitError
		if err != nil && !errors.As(err, &exitErr) {
			t.Fatalf("running cleat build: %v\n%s", err, out)
		}
		return string(out)
	}

	// The two forms the READMEs use. Both are relative; they differ in which
	// directory they are relative TO, which is the whole property.
	for _, tc := range []struct {
		name  string
		cwd   string
		entry string
	}{
		{
			name:  "from the directory holding the file, as the README says to",
			cwd:   example,
			entry: "hello_workflow.py:hello",
		},
		{
			name:  "from the repository root, with a path relative to it",
			cwd:   root,
			entry: filepath.Join("examples", "python-hello", "hello_workflow.py") + ":hello",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := build(t, tc.cwd, tc.entry)

			// PRECONDITION. If the stub did not take, the run stopped at
			// runBuildPython's own LookPath and build_wasm.py never executed,
			// so nothing below measured the resolution.
			if !strings.Contains(out, "Compiling Python to WASM") {
				t.Fatalf("UNMEASURED: the build never reached build_wasm.py, so the entry "+
					"resolution was not exercised.\n%s", out)
			}

			// The assertion, on the script's own success line.
			validated, ok := validatedEntryPath(out)
			if !ok {
				// Name the old symptom explicitly when it is present, so a
				// future failure is read as this regression rather than as
				// something new.
				symptom := ""
				if strings.Contains(out, "Entry file not found") {
					symptom = "\n\nAnd the output carries the exact cleat#1836 symptom, " +
						"\"Entry file not found\": the path was resolved against the SDK root."
				}
				t.Fatalf("build_wasm.py printed no \"Validated entry:\" line, so it never "+
					"read the file.%s\n\nA relative --entry is resolved by the script "+
					"against ITS OWN working directory, which wasm/build.go sets to the SDK "+
					"root. Unless cleat makes the path absolute first, a path the user typed "+
					"means a file in python-sdk/.\n\ngot:\n%s", symptom, out)
			}

			// Absolute is the mechanism: a relative path handed to a script
			// running elsewhere is the defect itself, whatever it resolves to.
			if !filepath.IsAbs(validated) {
				t.Errorf("the entry reached build_wasm.py as %q, which is relative. "+
					"The script runs with cmd.Dir set to the SDK root, so a relative path "+
					"means a file in python-sdk/. cleat#1836.", validated)
			}

			// And it is the file the user meant. Compared with os.SameFile
			// rather than by string: this checkout is reachable by two paths
			// (/localssd/... and /Users/Shared/localssd/...), and the binary's
			// os.Getwd and the test's compile-time source path do not agree on
			// which spelling to use. A string comparison here passes or fails
			// on which mount the suite was started from, which is a property of
			// the machine and not of the fix.
			want := filepath.Join(example, "hello_workflow.py")
			wantInfo, err := os.Stat(want)
			if err != nil {
				t.Fatalf("stat the fixture %s: %v", want, err)
			}
			gotInfo, err := os.Stat(validated)
			if err != nil {
				t.Fatalf("the validated entry %q does not stat: %v", validated, err)
			}
			if !os.SameFile(wantInfo, gotInfo) {
				t.Errorf("the entry resolved to %q, which is not %q.\n"+
					"The path was resolved against some directory other than the one the "+
					"user was standing in. cleat#1836.", validated, want)
			}
		})
	}
}

// validatedEntryPath returns the path from build_wasm.py's own
// "Validated entry: <path> -> <func>()" line, which it prints only after
// opening and reading the file.
//
// That line is the discriminator this test is built on. Asserting instead that
// "Entry file not found" is ABSENT would pass for a command that failed
// earlier -- a bad flag, a missing binary, a determinism gate that refused --
// none of which say anything about where the entry was resolved.
func validatedEntryPath(out string) (string, bool) {
	const prefix = "Validated entry: "
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		rest := strings.TrimPrefix(line, prefix)
		// The function half is " -> name()". Cut at the LAST arrow: a path may
		// legitimately contain one, and the suffix may not.
		if i := strings.LastIndex(rest, " -> "); i >= 0 {
			return rest[:i], true
		}
		return rest, true
	}
	return "", false
}
