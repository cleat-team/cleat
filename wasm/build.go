package wasm

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/cleat-team/cleat/internal/analyzer"
	"golang.org/x/mod/modfile"
)

// BuildConfig holds the parameters for assembling a build directory.
type BuildConfig struct {
	// SrcDir is the directory containing the user's source files.
	SrcDir string

	// OutDir is the directory where generated files and the WASM binary
	// are written.
	OutDir string

	// PkgName is the Go package name for the build directory.
	PkgName string

	// ModulePath is the module path from the project's go.mod
	// (e.g., "github.com/cleat-team/cleat").
	ModulePath string

	// ProjectRoot is the absolute path to the project root (where go.mod lives).
	ProjectRoot string

	// GoVersion is the Go version from the project's go.mod (e.g., "1.26").
	GoVersion string

	// Outputs holds the generated file contents.
	Outputs *OutputFiles

	// WASMOutput is the filename for the compiled WASM binary
	// (e.g., "place_order.wasm").
	WASMOutput string

	// Target is the compilation target. Only "go" (standard Go/wasip1) is
	// currently supported.
	Target string

	// XfrmSource, if non-nil, provides transformed source files to write
	// instead of copying original source files. Keyed by filename.
	XfrmSource map[string][]byte
}

// stagedManifestName records which user sources the last build copied into an
// output directory. A dotfile, so the Go toolchain ignores it when compiling
// the directory.
const stagedManifestName = ".cleat-staged"

// clearStaleStagedSources removes the user sources a PREVIOUS build staged into
// this directory and this one will not.
//
// THE DEFECT. -o is both the artifact destination and the staging directory:
// the build copies the workflow's own .go files there beside the generated
// ones. Nothing removed them, and every Go example's README documents the same
// `-o /tmp/out`, so following two of them in a row fails. Measured on
// cleat#1823:
//
//	cleat build -o /tmp/out ./examples/datapipeline/    exit 0
//	cleat build -o /tmp/out ./examples/event-driven/    exit 1
//	    subscription_workflow.go:190:6: toJSON redeclared in this block
//	        pipeline.go:186:6: other declaration of toJSON
//
// pipeline.go belongs to datapipeline. The error names two files from two
// different examples, reports a Go symbol clash rather than anything about
// cleat, and points at line numbers in a directory the user thinks of as
// output. The gen_*.go files are overwritten every time, which is why this
// bites only when two projects' OWN sources differ in name -- and why building
// one example twice is fine, so the failure looks intermittent.
//
// A MANIFEST, NOT A GLOB, AND THAT IS THE WHOLE DESIGN. Deleting *.go from
// OutDir would be deleting the user's files: -o is a path they chose, and
// `cleat build -o ~/src/myproject` is a typo away. This removes only names a
// previous cleat build recorded writing, so a directory cleat has never
// written to is never touched.
//
// AND A DIRECTORY WITH FOREIGN SOURCES IS REFUSED RATHER THAN MIXED. Without a
// manifest there is no way to tell a leftover from a file that was always
// there, so the honest answer is to stop and say which files are in the way.
// That replaces a compile error about redeclared symbols with one about the
// output directory, which is where the problem actually is.
func clearStaleStagedSources(outDir string, keep map[string]bool) error {
	previous, hadManifest := readStagedManifest(outDir)
	if hadManifest {
		for _, base := range previous {
			if keep[base] || strings.HasPrefix(base, "gen_") {
				continue
			}
			if err := os.Remove(filepath.Join(outDir, base)); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("removing %s staged by a previous build: %w", base, err)
			}
		}
		return nil
	}

	entries, err := os.ReadDir(outDir)
	if err != nil {
		return fmt.Errorf("reading the build directory: %w", err)
	}
	var foreign []string
	for _, e := range entries {
		base := e.Name()
		if e.IsDir() || !strings.HasSuffix(base, ".go") {
			continue
		}
		if keep[base] || strings.HasPrefix(base, "gen_") {
			continue
		}
		foreign = append(foreign, base)
	}
	if len(foreign) == 0 {
		return nil
	}
	sort.Strings(foreign)
	return fmt.Errorf(
		"the build directory %s already contains Go sources this build did not put there: %s\n"+
			"  cleat stages the workflow's own sources into -o beside the generated files, so\n"+
			"  building into a directory that already has .go files in it compiles them together\n"+
			"  and fails with a redeclaration error naming files from both.\n"+
			"  Use an empty directory, or one only cleat writes to.",
		outDir, strings.Join(foreign, ", "))
}

func readStagedManifest(outDir string) ([]string, bool) {
	data, err := os.ReadFile(filepath.Join(outDir, stagedManifestName))
	if err != nil {
		return nil, false
	}
	var names []string
	for _, l := range strings.Split(string(data), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			names = append(names, l)
		}
	}
	return names, true
}

func writeStagedManifest(outDir string, names []string) error {
	sort.Strings(names)
	body := strings.Join(names, "\n")
	if body != "" {
		body += "\n"
	}
	if err := os.WriteFile(filepath.Join(outDir, stagedManifestName), []byte(body), 0644); err != nil {
		return fmt.Errorf("writing the staged-source manifest: %w", err)
	}
	return nil
}

// PrepareBuildDir assembles the build directory: copies user source files,
// writes generated files, and creates a go.mod for wasip1 compilation.
func PrepareBuildDir(cfg *BuildConfig) error {
	// Create the build directory.
	if err := os.MkdirAll(cfg.OutDir, 0755); err != nil {
		return fmt.Errorf("creating build directory: %w", err)
	}

	// Collect the user sources this build will stage, BEFORE writing any of
	// them, so the stale ones from a previous build into the same directory
	// can be removed first. cleat#1823.
	type staged struct {
		base    string
		content []byte
	}
	var toStage []staged

	if len(cfg.XfrmSource) > 0 {
		for filename, content := range cfg.XfrmSource {
			base := filepath.Base(filename)
			if strings.HasPrefix(base, "gen_") {
				continue
			}

			// Warn on files with platform-specific suffixes that the
			// compiler will exclude for the WASM target.
			for _, warn := range analyzer.WasmFilenameWarnings(base) {
				fmt.Fprintf(os.Stderr, "warning: %s\n", warn)
			}

			// Evaluate build constraints; skip files constrained out.
			ok, err := analyzer.MatchWasmBuildConstraint(filename, content)
			if err != nil {
				return fmt.Errorf("checking build constraints for %s: %w", base, err)
			}
			if !ok {
				continue
			}
			toStage = append(toStage, staged{base, rewritePackageToMain(content)})
		}
	} else {
		goFiles, err := filepath.Glob(filepath.Join(cfg.SrcDir, "*.go"))
		if err != nil {
			return fmt.Errorf("globbing source files: %w", err)
		}
		for _, src := range goFiles {
			base := filepath.Base(src)
			if strings.HasPrefix(base, "gen_") {
				continue
			}

			// Warn on files with platform-specific suffixes that the
			// compiler will exclude for the WASM target.
			for _, warn := range analyzer.WasmFilenameWarnings(base) {
				fmt.Fprintf(os.Stderr, "warning: %s\n", warn)
			}

			// Evaluate build constraints; skip files constrained out.
			content, err := os.ReadFile(src)
			if err != nil {
				return fmt.Errorf("reading %s: %w", base, err)
			}
			ok, err := analyzer.MatchWasmBuildConstraint(src, content)
			if err != nil {
				return fmt.Errorf("checking build constraints for %s: %w", base, err)
			}
			if !ok {
				continue
			}
			toStage = append(toStage, staged{base, rewritePackageToMain(content)})
		}
	}

	keep := make(map[string]bool, len(toStage))
	for _, f := range toStage {
		keep[f.base] = true
	}
	if err := clearStaleStagedSources(cfg.OutDir, keep); err != nil {
		return err
	}

	for _, f := range toStage {
		if err := os.WriteFile(filepath.Join(cfg.OutDir, f.base), f.content, 0644); err != nil {
			return fmt.Errorf("writing %s: %w", f.base, err)
		}
	}
	names := make([]string, 0, len(toStage))
	for _, f := range toStage {
		names = append(names, f.base)
	}
	if err := writeStagedManifest(cfg.OutDir, names); err != nil {
		return err
	}

	// Write generated files.
	writeFile := func(name, content string) error {
		if content == "" {
			return nil
		}
		path := filepath.Join(cfg.OutDir, name)
		return os.WriteFile(path, []byte(content), 0644)
	}

	if err := writeFile("gen_wasm_imports.go", cfg.Outputs.Imports); err != nil {
		return err
	}
	if err := writeFile("gen_wasm_memory.go", cfg.Outputs.Memory); err != nil {
		return err
	}
	if err := writeFile("gen_host_adapter.go", cfg.Outputs.Adapter); err != nil {
		return err
	}
	patchAdapterImports(filepath.Join(cfg.OutDir, "gen_host_adapter.go"))
	if err := writeFile("gen_wasm_exports.go", cfg.Outputs.Exports); err != nil {
		return err
	}

	// main is required by Go wasip1.  The wasmtime backend calls _start
	// which runs main().  main() polls for work via cleat_poll_work,
	// dispatches to the entry point via cleatDispatch, and signals
	// completion via cleatCompleteImport.  If no work is available
	// (entryLen == 0, e.g. wazero backend), main() returns immediately
	// and the backend calls exports directly instead.
	mainStub := MainStubSource()
	if err := writeFile("gen_main_stub.go", mainStub); err != nil {
		return err
	}

	// Create go.mod with replace directive pointing to the project root.
	goVersion := cfg.GoVersion
	if goVersion == "" {
		goVersion = "1.23"
	}
	// Write a minimal go.mod for wasip1 compilation. go mod tidy is run by
	// the caller to generate go.sum.
	//
	// The require line names SDKModulePath, a constant. It used to be built as
	// cfg.ModulePath+"/cleat" -- the *enclosing* module's path with "/cleat"
	// appended -- with a replace pointing at cfg.ProjectRoot+"/cleat". That is
	// only correct for a workflow that lives directly in this repository's root
	// module, because only there does "<module>/cleat" happen to name the SDK.
	// For a project `cleat init` scaffolds (module example.com/myapp) it
	// generated `require example.com/myapp/cleat v0.0.0` replaced by a
	// myapp/cleat directory that does not exist, and `go mod tidy` in the build
	// directory failed with exactly that path. The same thing happens for a
	// workflow in any nested module of this repo.
	//
	// The version and the local checkout, unlike the path, are not fixed:
	//   - sdkReplaceDir finds a sibling SDK checkout by walking up from the
	//     project root, which is what makes an in-repo workflow build against
	//     the tree it sits in rather than a published release.
	//   - failing that, the version the workflow's own module already requires
	//     is used, so an external project builds against the SDK it compiles
	//     against.
	sdkDir := sdkReplaceDir(cfg.ProjectRoot)
	sdkVersion := "v0.0.0"
	if sdkDir == "" {
		if v := sdkRequiredVersion(cfg.ProjectRoot); v != "" {
			sdkVersion = v
		}
	}

	modContent := fmt.Sprintf(`module cleat-build

go %s

require %s %s
`, goVersion, SDKModulePath, sdkVersion)
	if sdkDir != "" {
		modContent += fmt.Sprintf("\nreplace %s => %s\n", SDKModulePath, sdkDir)
		// The root module too, when the SDK comes from a local checkout.
		//
		// cleat/go.mod requires the root module at v0.0.0 and resolves it with
		// its own `replace ../` -- and a replace inside a *dependency* module is
		// ignored, so nothing here can resolve v0.0.0 unless this go.mod says
		// how. It went unnoticed for a long time only because module graph
		// pruning usually drops that edge; the moment anything in package cleat's
		// own tests imports the engine, `go mod tidy` in this directory needs the
		// root module's go.mod and fails with "unknown revision v0.0.0" -- for
		// every workflow build, from a change in a test file that never runs here.
		//
		// Emitting it unconditionally alongside the SDK replace removes that
		// coupling. TestGeneratedGoModResolvesTheRootModule pins it.
		modContent += fmt.Sprintf("\nrequire %s v0.0.0\n\nreplace %s => %s\n",
			RootModulePath, RootModulePath, filepath.Dir(sdkDir))
	}

	modPath := filepath.Join(cfg.OutDir, "go.mod")
	if err := os.WriteFile(modPath, []byte(modContent), 0644); err != nil {
		return fmt.Errorf("writing go.mod: %w", err)
	}

	// Propagate replace directives from the source module's go.mod into the
	// build directory.  The generated go.mod has a single replace for the
	// cleat submodule, but workflows that import other local modules (e.g.
	// protocol packages, sibling modules) need those path-based replaces
	// carried forward so that go mod tidy resolves local files instead of
	// trying to pull from the network.
	//
	// Whether the SDK and root replaces were written above is passed in rather
	// than assumed: see the skip in propagateReplaces, which is correct only
	// when they were.
	if err := propagateReplaces(cfg.ProjectRoot, cfg.OutDir, modPath, sdkDir != ""); err != nil {
		fmt.Fprintf(os.Stderr, "warning: propagating replace directives: %v\n", err)
	}

	return nil
}

// FindRepoRoot walks up from the given directory looking for go.mod to
// locate the repository root. Returns the absolute path to the directory
// containing go.mod, or an error if not found.
func FindRepoRoot(from string) (string, error) {
	abs, err := filepath.Abs(from)
	if err != nil {
		return "", fmt.Errorf("resolving path %s: %w", from, err)
	}
	// Two passes, and the order matters. The first looks for the go.mod that
	// declares the ROOT module; only if there is none does it fall back to the
	// nearest go.mod of any kind.
	//
	// Nearest-first was the original behaviour and it broke the moment this
	// repository grew nested modules. Called from a Python workflow under
	// tests/plugin-harness/testdata/, the nearest go.mod became
	// tests/plugin-harness/go.mod, so repoRoot resolved to tests/plugin-harness
	// and the caller looked for python-sdk/scripts/build_wasm.py underneath it.
	// The build failed, and TestPluginCalls_Wasm_Python t.Skipf'd on that
	// failure -- a skip standing in for a break, which only the job's skip
	// budget of 0 caught.
	//
	// The fallback keeps the old behaviour for a tree that is not this
	// repository, where there is no root module to find.
	if root := findModuleRootDeclaring(abs, RootModulePath); root != "" {
		return root, nil
	}
	dir := abs
	for {
		if fi, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil && !fi.IsDir() {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("go.mod not found from %s", from)
		}
		dir = parent
	}
}

// findModuleRootDeclaring walks up from dir looking for a go.mod whose module
// path is exactly want, and returns that directory, or "" if there is none.
func findModuleRootDeclaring(dir, want string) string {
	for {
		modPath := filepath.Join(dir, "go.mod")
		if data, err := os.ReadFile(modPath); err == nil {
			if f, err := modfile.Parse(modPath, data, nil); err == nil &&
				f.Module != nil && f.Module.Mod.Path == want {
				return dir
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

func BuildPythonWasm(entry, output string, verbose bool) error {
	return BuildPythonWasmWithRuntime(entry, output, "", verbose)
}

// AbsoluteEntryPath makes the file half of a "path:function" entry spec
// absolute, leaving the function name untouched.
//
// Exported so it can be tested without a Python toolchain: the behaviour is
// pure string and filesystem-path work, and the alternative is a test that
// needs componentize-py to say anything at all.
//
// rsplit on the LAST colon, matching build_wasm.py's `entry.rsplit(":", 1)`,
// so a directory containing a colon resolves the same way on both sides.
//
// An entry with no colon, or one whose colon is at position 0, is returned
// untouched: there is no file half to resolve, and build_wasm.py's parse_entry
// should reject it with its own message rather than have this silently
// reinterpret it. Same for a path that is already absolute, and for the case
// where filepath.Abs fails -- passing the original through leaves the error to
// the layer that can describe it.
func AbsoluteEntryPath(entry string) string {
	i := strings.LastIndex(entry, ":")
	if i <= 0 {
		return entry
	}
	abs, err := filepath.Abs(entry[:i])
	if err != nil {
		return entry
	}
	return abs + entry[i:]
}

// BuildPythonWasmWithRuntime compiles a Python workflow to WASM, selecting
// the output format based on targetRuntime:
//   - "wasmtime" — Component Model binary (skip decomposition)
//   - "wazero"   — decomposed core WASM module
//   - ""         — both formats (default)
//
// FindRepoRoot BELOW MEANS THIS ONLY WORKS INSIDE A CLEAT CHECKOUT (cleat#1971):
// it finds build_wasm.py by walking up from the project directory to a
// cleat repo root and then into python-sdk/scripts/, so a project scaffolded
// outside this repo -- which is every real user's, once cleat-sdk is
// installable from PyPI -- has no repo root to find. Parked on #1779
// (publishing cleat-sdk, targeted for the 0.3.0 release): once the SDK is a
// package rather than a subdirectory, this needs to find build_wasm.py
// relative to the INSTALLED package instead of a repo root.
func BuildPythonWasmWithRuntime(entry, output, targetRuntime string, verbose bool) error {
	repoRoot, err := FindRepoRoot(".")
	if err != nil {
		return fmt.Errorf("finding repo root: %w", err)
	}

	sdkRoot := filepath.Join(repoRoot, "python-sdk")
	buildScript := filepath.Join(sdkRoot, "scripts", "build_wasm.py")

	if _, err := os.Stat(buildScript); err != nil {
		return fmt.Errorf("build script not found at %s: %w", buildScript, err)
	}

	// THE ENTRY PATH IS MADE ABSOLUTE HERE, and the reason is cmd.Dir below.
	//
	// build_wasm.py runs with its working directory set to the SDK root, and
	// validate_entry resolves the path with a bare Path(entry_file) -- so a
	// RELATIVE entry was looked up under python-sdk/ rather than under the
	// directory the user ran the command in. Both documented Python example
	// commands are relative, so both failed:
	//
	//	$ cd examples/python-langchain
	//	$ cleat build --target python --entry research_agent.py:langchain_research_agent
	//	Error: Entry file not found: research_agent.py     <- it is right there
	//
	// cleat#1836. Resolving in Go rather than in build_wasm.py keeps the fix
	// next to the cmd.Dir that causes it: the script is entitled to assume its
	// own working directory, and the caller is the one changing it.
	//
	args := []string{buildScript, "--entry", AbsoluteEntryPath(entry), "--output", output}
	if verbose {
		args = append(args, "--verbose")
	}
	if targetRuntime != "" {
		args = append(args, "--runtime", targetRuntime)
	}

	cmd := exec.Command("python3", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Dir = sdkRoot

	return cmd.Run()
}

// rewritePackageToMain replaces the first "package <name>" declaration with
// "package main", preserving any //go:build constraints or comments above it.
func rewritePackageToMain(content []byte) []byte {
	// Find the first "package <name>" line and rewrite it.
	// We scan each line by looking for newline characters. For files that end
	// without a trailing newline we also check the last fragment directly.
	var result []byte
	done := false
	for i := 0; i < len(content); i++ {
		if !done && content[i] == '\n' {
			// Check if the previous line was "package X"
			lineStart := 0
			if i > 0 {
				for j := i - 1; j >= 0 && content[j] != '\n'; j-- {
					lineStart = j
				}
			}
			line := string(content[lineStart:i])
			if strings.HasPrefix(strings.TrimSpace(line), "package ") {
				// Found the package declaration — rewrite it.
				result = append(result, content[:lineStart]...)
				result = append(result, []byte("package main")...)
				result = append(result, content[i:]...)
				done = true
				break
			}
		}
	}
	if !done {
		// Check the last line of the file (handles files without a trailing
		// newline whose last line is the package declaration).
		lineStart := 0
		for j := len(content) - 1; j >= 0 && content[j] != '\n'; j-- {
			lineStart = j
		}
		line := string(content[lineStart:])
		if strings.HasPrefix(strings.TrimSpace(line), "package ") {
			result = append(result, content[:lineStart]...)
			result = append(result, []byte("package main")...)
			return result
		}
		return content // no package declaration found, return as-is
	}
	return result
}

// SDKModulePath is the guest SDK's module path. It is a constant of the
// project, not something to derive from whichever module a workflow happens to
// live in.
const SDKModulePath = "github.com/cleat-team/cleat/cleat"

// RootModulePath is the engine module's path. The SDK requires it, so a build
// directory replacing the SDK with a local checkout must be able to resolve it
// too -- see where this is used.
const RootModulePath = "github.com/cleat-team/cleat"

// sdkReplaceDir returns the absolute path of a local SDK checkout to replace
// SDKModulePath with, or "" if there is none.
//
// It walks up from start looking for a cleat/ subdirectory whose go.mod
// declares SDKModulePath. That finds this repository's own cleat/ directory
// from anywhere inside the repo -- including from a nested module such as
// tests/plugin-harness or examples/, which is the case that stopped working
// when those became separate modules -- and finds nothing in a user's project,
// which is correct: they should build against a released SDK.
//
// The go.mod is read rather than just stat'ed. A directory named "cleat" in
// someone's project is not unusual, and replacing the SDK with an unrelated
// directory would fail in a way that points nowhere near the cause.
func sdkReplaceDir(start string) string {
	dir := start
	for {
		mod := filepath.Join(dir, "cleat", "go.mod")
		if data, err := os.ReadFile(mod); err == nil {
			if f, err := modfile.Parse(mod, data, nil); err == nil &&
				f.Module != nil && f.Module.Mod.Path == SDKModulePath {
				return filepath.Join(dir, "cleat")
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// sdkRequiredVersion returns the version of SDKModulePath that the module
// rooted at projectRoot requires, or "" if it does not require it.
//
// Used only when there is no local checkout: an external project builds its
// workflow against the same SDK version its own code compiles against, rather
// than against a v0.0.0 that resolves to nothing.
func sdkRequiredVersion(projectRoot string) string {
	modPath := filepath.Join(projectRoot, "go.mod")
	data, err := os.ReadFile(modPath)
	if err != nil {
		return ""
	}
	f, err := modfile.Parse(modPath, data, nil)
	if err != nil {
		return ""
	}
	for _, r := range f.Require {
		if r.Mod.Path == SDKModulePath {
			return r.Mod.Version
		}
	}
	return ""
}

// propagateReplaces reads the source module's go.mod, extracts all replace
// directives with local filesystem paths, adjusts paths to be relative to
// the build directory, and appends them to the build directory's go.mod.
// The generated go.mod only has a single replace for the cleat submodule,
// but workflows often import other local modules that use path-based replaces.
func propagateReplaces(projectRoot, outDir, modPath string, wroteSDKReplaces bool) error {
	srcModPath := filepath.Join(projectRoot, "go.mod")
	data, err := os.ReadFile(srcModPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("reading source go.mod: %w", err)
	}

	modFile, err := modfile.Parse(srcModPath, data, nil)
	if err != nil {
		return fmt.Errorf("parsing source go.mod: %w", err)
	}

	if len(modFile.Replace) == 0 {
		return nil
	}

	var extra []string
	for _, r := range modFile.Replace {
		if !modfile.IsDirectoryPath(r.New.Path) {
			continue
		}
		// The SDK's replace, if one was needed AND found, was already written
		// above. Emitting it twice is not a duplicate that go tolerates -- it
		// is "go.mod: repeated replacement of <path>", and the build fails.
		//
		// The condition used to be unguarded, and "already written above" is
		// true only when sdkReplaceDir found a checkout. When it returned "",
		// nothing was written above and this dropped the only replaces that
		// could resolve the SDK locally -- so a project that deliberately pins
		// the SDK to its own checkout got a workflow compiled against whatever
		// the module proxy served, with no diagnostic. See #771: cleat-ports
		// clones cleat to .cleat-src/ and pins the workflow module there, a
		// layout sdkReplaceDir cannot find, and every workflow it built would
		// have been testing a published release while reporting on a commit.
		// It surfaced only because v0.0.0 does not exist on the proxy and go
		// mod tidy failed for that unrelated reason; a project pinning a real
		// version would have gotten a clean, wrong build.
		if wroteSDKReplaces && (r.Old.Path == SDKModulePath || r.Old.Path == RootModulePath) {
			continue
		}
		// filepath.Join does NOT special-case an already-absolute second
		// argument -- Join("/proj", "/abs/dep") is "/proj/abs/dep" -- so
		// joining unconditionally nested an absolute replace under the project
		// and produced a path that does not exist. `go mod edit
		// -replace=foo=/abs/path` writes exactly that form, and it is the
		// natural one in a monorepo; the failure surfaced as an opaque `go mod
		// tidy` error about a module it could not find. cleat#1322.
		//
		// A RELATIVE path must still resolve against projectRoot, which is what
		// the join was for and what a naive fix would break. Both cases are
		// pinned in absolute_replace_is_not_nested_test.go.
		newPath := r.New.Path
		if !filepath.IsAbs(newPath) {
			newPath = filepath.Join(projectRoot, newPath)
		}
		absReplace, err := filepath.Abs(newPath)
		if err != nil {
			continue
		}
		extra = append(extra, fmt.Sprintf("replace %s => %s", r.Old.Path, absReplace))
	}

	if len(extra) == 0 {
		return nil
	}

	f, err := os.OpenFile(modPath, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("appending to build go.mod: %w", err)
	}
	defer f.Close()

	fmt.Fprintln(f, "")
	for _, s := range extra {
		fmt.Fprintln(f, s)
	}
	return nil
}

// patchAdapterImports adds missing "strings" import to the generated host adapter
// if the adapter body references strings.* functions.
// patchAdapterImports adds "strings" to the generated adapter when its code
// actually uses the package.
//
// It asks the QUESTION with a parser rather than a substring search, and that
// is not fastidiousness. It was
//
//	if !strings.Contains(content, "strings.") { return }
//
// which cannot tell a use from a SENTENCE ABOUT a use. A comment in the
// emitted helpers reading "this was strings.Index(json, ...)" -- prose
// explaining a call that is no longer there -- made this add an import nothing
// referenced, and every guest build failed with
//
//	./gen_host_adapter.go:8:2: "strings" imported and not used
//
// That is the same defect this file's PR was fixing one layer down, where a
// brace scan could not tell a brace from a brace inside a string. A text
// search cannot tell a thing from a sentence about the thing; a parser can,
// because comments are not in the AST.
//
// The generated file parses cleanly even while it is missing this import --
// an undefined package qualifier is a type error, not a syntax error -- so
// go/parser is usable here without the import already being present.
func patchAdapterImports(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	content := string(data)
	if strings.Contains(content, `"strings"`) {
		return
	}
	if !usesPackage(content, "strings") {
		return
	}
	content = strings.Replace(content, "import (", "import (\n\t\"strings\"", 1)
	os.WriteFile(path, []byte(content), 0644)
}

// usesPackage reports whether src contains a real qualified reference to pkg,
// e.g. strings.Index. Comments and string literals do not count.
//
// A parse failure returns false rather than guessing: the caller's only action
// is to ADD an import, and adding one that is not needed breaks the build
// outright, while failing to add one that is needed breaks it in a way the
// compiler names precisely. Neither is good, but the second is diagnosable.
func usesPackage(src, pkg string) bool {
	f, err := parser.ParseFile(token.NewFileSet(), "", src, parser.SkipObjectResolution)
	if err != nil {
		return false
	}
	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		if found {
			return false
		}
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if id, ok := sel.X.(*ast.Ident); ok && id.Name == pkg {
			found = true
			return false
		}
		return true
	})
	return found
}

// MainStubSource is the gen_main_stub.go this package emits for a Go guest.
//
// Exported so that engine/guest_buffer_matches_the_host_test.go can assert the
// input buffer it declares equals engine.DefaultOutBufSize. The dependency only
// runs one way -- engine imports wasm -- so the guest cannot reference the
// host's constant and nothing but that test relates the two numbers.
//
// Returning the SOURCE rather than the size is deliberate: what ships is this
// string, and a stub that stopped using its declared size would still satisfy
// an assertion about a size constant.
func MainStubSource() string {
	return `package main

import "unsafe"

const argsBufSize = 65536

func main() {
	var entryNameBuf [256]byte

	// THE INPUT BUFFER STAYS AT 64 KiB, AND THAT IS A MEASURED DECISION
	// RATHER THAN AN OVERSIGHT -- see the long note on argsBufSize below.
	var argsBuf [argsBufSize]byte

	ret := cleatPollWorkImport(
		unsafe.Pointer(&entryNameBuf[0]), 256,
		unsafe.Pointer(&argsBuf[0]), argsBufSize,
	)
	entryNameLen := uint32(ret >> 32)
	argsLen := uint32(ret)
	if entryNameLen == 0 {
		return
	}
	entryName := string(entryNameBuf[:entryNameLen])
	args := argsBuf[:argsLen]
	result := cleatDispatch(entryName, args)
	if result == nil {
		// nil means cleatDispatch did not recognise entryName. That is a
		// FAILURE and must be reported on the error channel; it used to be
		// returned as a result reading {"error":"unknown entry point: ..."},
		// which arrived here and was completed with status 0 -- success. The
		// host routes by entry-point name and had no other way to tell a name
		// the guest never heard of from one that ran, which is how every defer
		// in every Go WASM workflow did nothing while the host recorded
		// success. IMPROVEMENT-PLAN 3.70.
		errStr := encodeJSONString("unknown entry point: " + entryName)
		errPtr, errLen := stringPtr(errStr)
		cleatCompleteImport(1, errPtr, errLen)
		return
	}
	resultPtr, resultLen := stringPtr(string(result))
	cleatCompleteImport(0, resultPtr, resultLen)
}
`
}
