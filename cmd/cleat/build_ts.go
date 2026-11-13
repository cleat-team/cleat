package main

// `cleat build --target ts`: BRANCH-ONLY PROTOTYPE (proto/ts-javy). It is not a supported target, has no tiers.yaml entry, and makes no
// support claim. It drives the prototype's build pipeline in prototypes/ts-javy (tscheck -> esbuild -> Javy -> link -> tsmeta) so that a
// developer's flow is one command; everything it does is in build.sh, and this file only finds the toolchain, checks it exists, and
// passes the arguments through.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// tsEntryName is an ABI entry name: snake_case, the same shape as the Go target's derived names. build.sh derives the camelCase export
// (place_order -> placeOrder) and the kebab-case WIT name from it.
var tsEntryName = regexp.MustCompile(`^[a-z][a-z0-9]*(_[a-z0-9]+)*$`)

// parseTSEntry splits "path/to/workflow.ts:place_order[,other_entry]" into the file and the ABI entry names. It reports what is wrong
// rather than guessing: a missing entry list is an error, because the module's exports are found syntactically and cannot be inferred
// from the file alone without running a parser this prototype does not carry.
func parseTSEntry(spec string) (file string, entries []string, err error) {
	i := strings.LastIndex(spec, ":")
	if i < 0 {
		return "", nil, fmt.Errorf("--entry must be file.ts:entry_name[,entry_name] (snake_case; place_order is exported as placeOrder), got %q", spec)
	}
	file, list := spec[:i], spec[i+1:]
	if !strings.HasSuffix(file, ".ts") {
		return "", nil, fmt.Errorf("--entry file must be a .ts file, got %q", file)
	}
	for _, e := range strings.Split(list, ",") {
		if !tsEntryName.MatchString(e) {
			return "", nil, fmt.Errorf("entry name %q is not snake_case ([a-z][a-z0-9_]*)", e)
		}
		entries = append(entries, e)
	}
	return file, entries, nil
}

// findTSToolchain locates prototypes/ts-javy: $CLEAT_TS_DIR, else the nearest ancestor of the working directory or of this executable
// that holds prototypes/ts-javy/build.sh.
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
			if _, err := os.Stat(filepath.Join(d, "prototypes", "ts-javy", "build.sh")); err == nil {
				return filepath.Join(d, "prototypes", "ts-javy"), nil
			}
			if filepath.Dir(d) == d {
				break
			}
		}
	}
	return "", fmt.Errorf("cannot find prototypes/ts-javy (set CLEAT_TS_DIR to it)")
}

// tsToolchainMissing names what a bootstrapped toolchain needs and this one lacks, so a build fails for that reason and not three
// steps later. Empty means ready.
func tsToolchainMissing(dir string) []string {
	var missing []string
	for _, rel := range []string{".tools/javy", "node_modules/.bin/esbuild", "link/target/release/cleat-js-link"} {
		if _, err := os.Stat(filepath.Join(dir, rel)); err != nil {
			missing = append(missing, rel)
		}
	}
	return missing
}

func runBuildTS(entry, outDir, channel string, workflowVersion int) {
	file, entries, err := parseTSEntry(entry)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	abs, err := filepath.Abs(file)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	if _, err := os.Stat(abs); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	dir, err := findTSToolchain()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(2)
	}
	if m := tsToolchainMissing(dir); len(m) > 0 {
		fmt.Fprintf(os.Stderr, "UNMEASURED: the TypeScript toolchain in %s is not built (missing %s); run %s/bootstrap.sh first.\n"+
			"This is a failure of the environment, not a finding about %s.\n", dir, strings.Join(m, ", "), dir, file)
		os.Exit(2)
	}
	if workflowVersion != 1 {
		fmt.Fprintf(os.Stderr, "  Note: --version is not stamped by the TypeScript target; `cleatctl deploy` assigns the version.\n")
	}
	if channel != "" && channel != "latest" && channel != "stable" {
		fmt.Fprintf(os.Stderr, "  Note: --channel has no meaning for the TypeScript target (no child version resolution at build time).\n")
	}
	absOut, err := filepath.Abs(outDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	// Paths in error messages are relative to where the developer ran the command when the file is under it (src/orders.ts), else to the
	// file's own directory; build.sh falls back to the file's directory when SRC_ROOT is unset.
	srcRoot := filepath.Dir(abs)
	// Compared as REAL paths: on macOS /var is a symlink to /private/var, and a working directory from getcwd and a path the user typed
	// can name the same directory by different spellings (found by phase5_cli.py: the root silently fell back to the file's directory).
	if wd, err := os.Getwd(); err == nil {
		realWd, e1 := filepath.EvalSymlinks(wd)
		realAbs, e2 := filepath.EvalSymlinks(abs)
		if e1 == nil && e2 == nil {
			if rel, err := filepath.Rel(realWd, realAbs); err == nil && !strings.HasPrefix(rel, "..") {
				srcRoot = wd
			}
		}
	}
	cmd := exec.Command("bash", filepath.Join(dir, "build.sh"), abs, strings.Join(entries, ","))
	cmd.Env = append(os.Environ(), "OUT_DIR="+absOut, "SRC_ROOT="+srcRoot)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			// build.sh's own statuses are kept: 1 = a finding (tscheck refused the source), 2 = the check could not look.
			fmt.Fprintf(os.Stderr, "\nError: the TypeScript build failed (exit %d); no artifact was emitted.\n", ee.ExitCode())
			os.Exit(ee.ExitCode())
		}
		fmt.Fprintf(os.Stderr, "Error: could not run the TypeScript build: %v\n", err)
		os.Exit(2)
	}
	name := strings.TrimSuffix(filepath.Base(abs), ".ts")
	fmt.Fprintf(os.Stderr, "  Wrote %s\n", filepath.Join(absOut, name+".wasm"))
}
