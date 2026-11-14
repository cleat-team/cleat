package main

// `cleat build --target ts`: compile a TypeScript workflow to a WASM module.
//
// TypeScript is a TIER-2 language, IN DEVELOPMENT and NOT SUPPORTED (cleat#3825, tiers.yaml): this target exists so the work
// can be exercised and measured, and it makes no support claim. It drives packages/cleat-ts/build.sh (static check ->
// bundle -> compile -> link -> stamp) so a developer's flow is one command. Everything the build does is in that script;
// this file finds the toolchain, checks that it was bootstrapped, passes the arguments through, and keeps the script's exit
// statuses distinct:
//
//	0  built
//	1  a finding: the source was refused (a static-check finding, or a malformed argument)
//	2  UNMEASURED: the toolchain is missing or a build step failed -- a failure of the setup, not a finding about the source
//
// The logic is in tsBuild, which returns the exit code and writes to a writer, so it is tested without the real toolchain and
// without exiting the test binary.

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// tsEntryName is an ABI entry name: snake_case, the same shape as the Go target's derived names. build.sh derives the camelCase
// export (place_order -> placeOrder) and the kebab-case WIT name from it.
var tsEntryName = regexp.MustCompile(`^[a-z][a-z0-9]*(_[a-z0-9]+)*$`)

// tsToolchainFiles are what packages/cleat-ts/bootstrap.sh leaves behind and build.sh needs. A build whose toolchain lacks one
// fails for that reason, named, and not three steps later.
var tsToolchainFiles = []string{".tools/javy", "node_modules/.bin/esbuild"}

// tsToolchainSubdir is where the toolchain lives in a checkout.
var tsToolchainSubdir = filepath.Join("packages", "cleat-ts")

// parseTSEntry splits "path/to/workflow.ts:place_order[,other_entry]" into the file and the ABI entry names. It reports what is
// wrong rather than guessing: the module's exports are found syntactically, so a missing entry list is an error, not something
// to infer.
func parseTSEntry(spec string) (file string, entries []string, err error) {
	i := strings.LastIndex(spec, ":")
	if i < 0 {
		return "", nil, fmt.Errorf("expected file.ts:entry_name[,entry_name] (snake_case; place_order is exported as placeOrder), got %q", spec)
	}
	file, list := spec[:i], spec[i+1:]
	if !strings.HasSuffix(file, ".ts") {
		return "", nil, fmt.Errorf("the workflow file must be a .ts file, got %q", file)
	}
	for _, e := range strings.Split(list, ",") {
		if !tsEntryName.MatchString(e) {
			return "", nil, fmt.Errorf("entry name %q is not snake_case ([a-z][a-z0-9_]*)", e)
		}
		entries = append(entries, e)
	}
	return file, entries, nil
}

// findTSToolchain locates packages/cleat-ts: $CLEAT_TS_DIR, else the nearest ancestor of the working directory or of this
// executable that holds packages/cleat-ts/build.sh.
func findTSToolchain() (string, error) {
	if d := os.Getenv("CLEAT_TS_DIR"); d != "" {
		if _, err := os.Stat(filepath.Join(d, "build.sh")); err != nil {
			return "", fmt.Errorf("CLEAT_TS_DIR=%s has no build.sh", d)
		}
		return d, nil
	}
	var starts []string
	if wd, err := os.Getwd(); err == nil {
		starts = append(starts, wd)
	}
	if exe, err := os.Executable(); err == nil {
		starts = append(starts, filepath.Dir(exe))
	}
	for _, s := range starts {
		for d := s; ; d = filepath.Dir(d) {
			if _, err := os.Stat(filepath.Join(d, tsToolchainSubdir, "build.sh")); err == nil {
				return filepath.Join(d, tsToolchainSubdir), nil
			}
			if filepath.Dir(d) == d {
				break
			}
		}
	}
	return "", fmt.Errorf("cannot find %s/build.sh from the working directory or the cleat executable (set CLEAT_TS_DIR to it)", tsToolchainSubdir)
}

// tsToolchainMissing names what a bootstrapped toolchain needs and this one lacks. Empty means ready.
func tsToolchainMissing(dir string) []string {
	var missing []string
	for _, rel := range tsToolchainFiles {
		if _, err := os.Stat(filepath.Join(dir, rel)); err != nil {
			missing = append(missing, rel)
		}
	}
	return missing
}

// tsSourceRoot is the directory error paths and the module's embedded file labels are relative to: the working directory when
// the source is under it (src/orders.ts), else the source's own directory. Compared as REAL paths: on macOS /var is a symlink to
// /private/var, and a working directory from getcwd and a path the user typed can name the same directory by different spellings.
func tsSourceRoot(abs string) string {
	root := filepath.Dir(abs)
	wd, err := os.Getwd()
	if err != nil {
		return root
	}
	realWd, e1 := filepath.EvalSymlinks(wd)
	realAbs, e2 := filepath.EvalSymlinks(abs)
	if e1 != nil || e2 != nil {
		return root
	}
	if rel, err := filepath.Rel(realWd, realAbs); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return wd
	}
	return root
}

// tsBuild runs the build and returns the exit code.
func tsBuild(spec, outDir string, channel string, workflowVersion int, stderr io.Writer) int {
	file, entries, err := parseTSEntry(spec)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}
	abs, err := filepath.Abs(file)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}
	if _, err := os.Stat(abs); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}
	dir, err := findTSToolchain()
	if err != nil {
		fmt.Fprintf(stderr, "UNMEASURED: %v.\nThis is a failure of the environment, not a finding about %s.\n", err, file)
		return 2
	}
	if m := tsToolchainMissing(dir); len(m) > 0 {
		fmt.Fprintf(stderr, "UNMEASURED: the TypeScript toolchain in %s is not bootstrapped (missing %s); run %s first.\n"+
			"This is a failure of the environment, not a finding about %s.\n", dir, strings.Join(m, ", "), filepath.Join(dir, "bootstrap.sh"), file)
		return 2
	}
	fmt.Fprintf(stderr, "  TypeScript is a tier-2 target: in development and not supported (see tiers.yaml).\n")
	if workflowVersion != 1 {
		fmt.Fprintf(stderr, "  Note: --version is not stamped by the TypeScript target; `cleat deploy` assigns the version.\n")
	}
	if channel != "" && channel != "latest" && channel != "stable" {
		fmt.Fprintf(stderr, "  Note: --channel has no meaning for the TypeScript target (no child version resolution at build time).\n")
	}
	absOut, err := filepath.Abs(outDir)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}
	cmd := exec.Command("bash", filepath.Join(dir, "build.sh"), abs, strings.Join(entries, ",")) //nolint:gosec // G204: fixed interpreter, arguments passed as an array, no shell; the variable parts are a toolchain directory found by walking up (or $CLEAT_TS_DIR) and the source path the developer just typed, and the entry names matched ^[a-z][a-z0-9]*(_[a-z0-9]+)*$ above.
	cmd.Env = append(os.Environ(), "OUT_DIR="+absOut, "SRC_ROOT="+tsSourceRoot(abs))
	cmd.Stdout, cmd.Stderr = stderr, stderr
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			// build.sh's own statuses are kept: 1 = a finding (the static check refused the source), 2 = UNMEASURED.
			fmt.Fprintf(stderr, "\nError: the TypeScript build failed (exit %d); no artifact was emitted.\n", ee.ExitCode())
			return ee.ExitCode()
		}
		fmt.Fprintf(stderr, "UNMEASURED: could not run the TypeScript build: %v\n", err)
		return 2
	}
	name := strings.TrimSuffix(filepath.Base(abs), ".ts")
	fmt.Fprintf(stderr, "  Wrote %s\n", filepath.Join(absOut, name+".wasm"))
	return 0
}

func runBuildTS(spec, outDir, channel string, workflowVersion int) {
	os.Exit(tsBuild(spec, outDir, channel, workflowVersion, os.Stderr))
}
