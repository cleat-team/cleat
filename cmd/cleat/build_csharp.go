package main

// `cleat build --target csharp`: compile a C# workflow project to a Component Model component and stamp it (cleat#4080, C# PR 2).
//
// C# is a TIER-2 language, IN DEVELOPMENT and NOT SUPPORTED (tiers.yaml): this target exists so the work can be exercised and
// measured, and it makes no support claim. The build runs in a digest-pinned Linux .NET SDK container (native arm64 on an arm64 host,
// amd64 elsewhere; the compiler only runs on Linux): packages/cleat-csharp/build.sh does that, from the lock in packages/cleat-csharp/toolchain.env
// and toolchain.packages.tsv. This file finds the toolchain, runs the script, and does the one step that must not be skippable:
// the cleat.metadata stamp. A C# component that carries no stamp is detected as Python and routed as a Python guest (measured in the
// cleat#4077 spike), so a build that cannot stamp fails and writes nothing, instead of emitting an artifact that runs as the wrong
// language.
//
// Exit statuses, the same three the TypeScript build keeps:
//
//	0  built and stamped
//	1  a finding: the project was refused (it does not build, it names no entry points, or an argument was malformed)
//	2  UNMEASURED: the toolchain, docker, the network or a pin failed, so nothing was learned about the project
//
// The logic is in csharpBuild, which returns the exit code and writes to a writer, so it is tested without docker.

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/cleat-team/cleat/wasm"
)

// csharpToolchainSubdir is where the build chain lives in a checkout.
var csharpToolchainSubdir = filepath.Join("packages", "cleat-csharp")

// csharpEntryPointsFile is what build.sh copies out of the project: the names the SDK's generator wrote to obj/cleat-entry-points.txt
// (WS-cleat3's contract, cleat#4080): UTF-8 without a BOM, LF line endings, one name per line, no blank lines, a final newline.
const csharpEntryPointsFile = "entry-points.txt"

// parseCSharpEntryPoints reads the entry-point list strictly. It REFUSES what the contract forbids rather than tolerating it: a BOM
// would turn the first name into "U+FEFF Hello", a stamp that names an entry nothing can call, with no error anywhere; a CRLF would do
// the same to every name. Names are returned in file order and all of them are kept (the file may hold several; Python stamps one).
func parseCSharpEntryPoints(b []byte) ([]string, error) {
	if len(b) == 0 {
		return nil, errors.New("the entry-point list is empty: the project declares no [CleatEntry] method")
	}
	if len(b) >= 3 && b[0] == 0xEF && b[1] == 0xBB && b[2] == 0xBF {
		return nil, errors.New("the entry-point list starts with a UTF-8 byte-order mark; the contract is UTF-8 without a BOM (a BOM would make the first name unreachable)")
	}
	if b[len(b)-1] != '\n' {
		return nil, errors.New("the entry-point list does not end with a newline; it may be truncated")
	}
	var names []string
	seen := map[string]bool{}
	for i, line := range strings.Split(string(b[:len(b)-1]), "\n") {
		if line == "" {
			return nil, fmt.Errorf("the entry-point list has an empty line at line %d (the contract has none)", i+1)
		}
		if strings.ContainsRune(line, '\r') {
			return nil, fmt.Errorf("the entry-point list has a carriage return at line %d; the contract is LF line endings", i+1)
		}
		for _, r := range line {
			if unicode.IsSpace(r) || r == ',' || unicode.IsControl(r) || r == 0xFEFF {
				return nil, fmt.Errorf("entry name %q (line %d) contains whitespace, a comma or a control character", line, i+1)
			}
		}
		if seen[line] {
			return nil, fmt.Errorf("entry name %q appears twice", line)
		}
		seen[line] = true
		names = append(names, line)
	}
	return names, nil
}

// findCSharpToolchain locates packages/cleat-csharp: $CLEAT_CSHARP_DIR, else the nearest ancestor of the working directory or of this
// executable that holds packages/cleat-csharp/build.sh.
func findCSharpToolchain() (string, error) {
	if d := os.Getenv("CLEAT_CSHARP_DIR"); d != "" {
		if _, err := os.Stat(filepath.Join(d, "build.sh")); err != nil {
			return "", fmt.Errorf("CLEAT_CSHARP_DIR=%s has no build.sh", d)
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
			if _, err := os.Stat(filepath.Join(d, csharpToolchainSubdir, "build.sh")); err == nil {
				return filepath.Join(d, csharpToolchainSubdir), nil
			}
			if filepath.Dir(d) == d {
				break
			}
		}
	}
	return "", fmt.Errorf("cannot find %s/build.sh from the working directory or the cleat executable (set CLEAT_CSHARP_DIR to it)", csharpToolchainSubdir)
}

// csharpProject resolves the build argument to a project directory: a directory holding exactly one .csproj, or a path to a .csproj.
func csharpProject(spec string) (dir string, err error) {
	abs, err := filepath.Abs(spec)
	if err != nil {
		return "", err
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return "", err
	}
	if !fi.IsDir() {
		if !strings.HasSuffix(abs, ".csproj") {
			return "", fmt.Errorf("%s is not a directory or a .csproj file", spec)
		}
		return filepath.Dir(abs), nil
	}
	m, _ := filepath.Glob(filepath.Join(abs, "*.csproj"))
	if len(m) != 1 {
		return "", fmt.Errorf("%s must hold exactly one .csproj, found %d", spec, len(m))
	}
	return abs, nil
}

// csharpMountRoot is the directory mounted into the container: the nearest ancestor of the project that is a git checkout (a project
// refers to the WIT file and the SDK sources by relative path, which sit elsewhere in the repository), else the project directory.
func csharpMountRoot(projectDir string) string {
	for d := projectDir; ; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			return d
		}
		if filepath.Dir(d) == d {
			return projectDir
		}
	}
}

// csharpBuild runs the build and returns the exit code: 0, 1 (the project was refused) or 2 (UNMEASURED). Any status build.sh ends with
// that is not 0, 1 or 2, and a signal, mean it did not run to the end, so they are 2 as well.
func csharpBuild(spec, outDir string, workflowVersion int, stderr io.Writer) int {
	if spec == "" {
		fmt.Fprintf(stderr, "Error: --target csharp wants a project: cleat build --target csharp <dir-or-.csproj>\n")
		return 1
	}
	projectDir, err := csharpProject(spec)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}
	dir, err := findCSharpToolchain()
	if err != nil {
		fmt.Fprintf(stderr, "UNMEASURED: %v.\nThis is a failure of the environment, not a finding about %s.\n", err, spec)
		return 2
	}
	root := csharpMountRoot(projectDir)
	rel, err := filepath.Rel(root, projectDir)
	if err != nil || strings.HasPrefix(rel, "..") {
		fmt.Fprintf(stderr, "Error: the project %s is not under its mount root %s\n", projectDir, root)
		return 1
	}
	absOut, err := filepath.Abs(outDir)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}
	// The build writes into a staging directory, and nothing reaches outDir unless every step succeeded: a failed build must not
	// leave an artifact behind that an earlier build's stamp or a missing stamp would make misleading.
	stage, err := os.MkdirTemp("", "cleat-csharp-build-")
	if err != nil {
		fmt.Fprintf(stderr, "UNMEASURED: cannot create a staging directory: %v\n", err)
		return 2
	}
	defer os.RemoveAll(stage)

	fmt.Fprintf(stderr, "  C# is a tier-2 target: in development and not supported (see tiers.yaml).\n")
	fmt.Fprintf(stderr, "  Building %s in the pinned .NET SDK container (needs docker; the first build downloads the toolchain)...\n", spec)
	cmd := exec.Command("bash", filepath.Join(dir, "build.sh"), root, rel, stage) //nolint:gosec // G204: fixed interpreter, arguments passed as an array, no shell; the variable parts are the toolchain directory found by walking up (or $CLEAT_CSHARP_DIR), the project path the developer just typed, and a temp directory.
	cmd.Stdout, cmd.Stderr = stderr, stderr
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			if code := ee.ExitCode(); code == 1 || code == 2 {
				fmt.Fprintf(stderr, "\nError: the C# build failed (exit %d); no artifact was emitted.\n", code)
				return code
			}
			fmt.Fprintf(stderr, "\nUNMEASURED: the C# build did not run to the end (%v); no artifact was emitted.\n"+
				"  (this is a failure of the setup, not a finding about the project)\n", ee)
			return 2
		}
		fmt.Fprintf(stderr, "UNMEASURED: could not run the C# build: %v\n", err)
		return 2
	}

	wasms, _ := filepath.Glob(filepath.Join(stage, "*.wasm"))
	if len(wasms) != 1 {
		fmt.Fprintf(stderr, "UNMEASURED: the build reported success but produced %d .wasm files, want exactly 1; no artifact was emitted.\n", len(wasms))
		return 2
	}
	component, err := os.ReadFile(wasms[0])
	if err != nil {
		fmt.Fprintf(stderr, "UNMEASURED: %v\n", err)
		return 2
	}
	list, err := os.ReadFile(filepath.Join(stage, csharpEntryPointsFile))
	if err != nil {
		fmt.Fprintf(stderr, "Error: the build wrote no entry-point list: %v\n", err)
		return 1
	}
	entries, err := parseCSharpEntryPoints(list)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}

	// The mandatory stamp. Unlike the Rust and Java builds, a failure here is NOT tolerated: see this file's header.
	name := strings.TrimSuffix(filepath.Base(wasms[0]), ".wasm")
	stamped, err := wasm.WriteMetadata(component, nonGoMetadata("csharp", name, workflowVersion, entries))
	if err != nil {
		fmt.Fprintf(stderr, "UNMEASURED: could not stamp cleat.metadata on the component: %v\n"+
			"  An unstamped C# component runs as a Python guest, so nothing was written.\n", err)
		return 2
	}
	// Read it back the way the engine will, rather than trust the write.
	if got := wasm.DetectLanguage(stamped); got != "csharp" {
		fmt.Fprintf(stderr, "UNMEASURED: the stamped component is detected as %q, not \"csharp\"; nothing was written.\n", got)
		return 2
	}
	meta, err := wasm.ReadMetadata(stamped)
	if err != nil || strings.Join(meta.EntryPoints, "\n") != strings.Join(entries, "\n") {
		fmt.Fprintf(stderr, "UNMEASURED: the stamp does not read back with the entry points that were written (%v); nothing was written.\n", err)
		return 2
	}
	if err := os.MkdirAll(absOut, 0o755); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}
	dst := filepath.Join(absOut, name+".wasm")
	if err := os.WriteFile(dst, stamped, 0o644); err != nil {
		fmt.Fprintf(stderr, "Error: writing %s: %v\n", dst, err)
		return 1
	}
	fmt.Fprintf(stderr, "  Wrote %s (%s), entry points: %s\n", dst, formatSize(int64(len(stamped))), strings.Join(entries, ", "))
	return 0
}

func runBuildCSharp(spec, outDir string, workflowVersion int) {
	os.Exit(csharpBuild(spec, outDir, workflowVersion, os.Stderr))
}
