package wasm

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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

// PrepareBuildDir assembles the build directory: copies user source files,
// writes generated files, and creates a go.mod for wasip1 compilation.
func PrepareBuildDir(cfg *BuildConfig) error {
	// Create the build directory.
	if err := os.MkdirAll(cfg.OutDir, 0755); err != nil {
		return fmt.Errorf("creating build directory: %w", err)
	}

	// Copy or write user source files, rewriting package declarations to "main".
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

			dst := filepath.Join(cfg.OutDir, base)
			rewritten := rewritePackageToMain(content)
			if err := os.WriteFile(dst, rewritten, 0644); err != nil {
				return fmt.Errorf("writing transformed %s: %w", base, err)
			}
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

			dst := filepath.Join(cfg.OutDir, base)
			rewritten := rewritePackageToMain(content)
			if err := os.WriteFile(dst, rewritten, 0644); err != nil {
				return fmt.Errorf("writing %s: %w", base, err)
			}
		}
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
	mainStub := `package main

import "unsafe"

func main() {
	var entryNameBuf [256]byte
	var argsBuf [65536]byte
	ret := cleatPollWorkImport(
		unsafe.Pointer(&entryNameBuf[0]), 256,
		unsafe.Pointer(&argsBuf[0]), 65536,
	)
	entryNameLen := uint32(ret >> 32)
	argsLen := uint32(ret)
	if entryNameLen == 0 {
		return
	}
	entryName := string(entryNameBuf[:entryNameLen])
	args := argsBuf[:argsLen]
	result := cleatDispatch(entryName, args)
	resultPtr, resultLen := stringPtr(string(result))
	cleatCompleteImport(0, resultPtr, resultLen)
}
`
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
	if err := propagateReplaces(cfg.ProjectRoot, cfg.OutDir, modPath); err != nil {
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

// BuildPythonWasmWithRuntime compiles a Python workflow to WASM, selecting
// the output format based on targetRuntime:
//   - "wasmtime" — Component Model binary (skip decomposition)
//   - "wazero"   — decomposed core WASM module
//   - ""         — both formats (default)
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

	args := []string{buildScript, "--entry", entry, "--output", output}
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
func propagateReplaces(projectRoot, outDir, modPath string) error {
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
		// The SDK's replace, if one is needed, was already written above.
		// Emitting it twice is not a duplicate that go tolerates -- it is
		// "go.mod: repeated replacement of <path>", and the build fails.
		if r.Old.Path == SDKModulePath || r.Old.Path == RootModulePath {
			continue
		}
		absReplace, err := filepath.Abs(filepath.Join(projectRoot, r.New.Path))
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
func patchAdapterImports(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	content := string(data)
	if !strings.Contains(content, "strings.") {
		return
	}
	if strings.Contains(content, `"strings"`) {
		return
	}
	content = strings.Replace(content, "import (", "import (\n\t\"strings\"", 1)
	os.WriteFile(path, []byte(content), 0644)
}
