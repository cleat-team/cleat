package wasm

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/cleat-team/cleat/internal/analyzer"
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

	// Target is the compilation target: "tinygo" for Go code.
	Target string

	// XfrmSource, if non-nil, provides transformed source files to write
	// instead of copying original source files. Keyed by filename.
	XfrmSource map[string][]byte
}

// PrepareBuildDir assembles the build directory: copies user source files,
// writes generated files, and creates a go.mod for TinyGo compilation.
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
	if err := writeFile("gen_wasm_exports.go", cfg.Outputs.Exports); err != nil {
		return err
	}

	var mainStub string
	if cfg.Target == "tinygo" {
		mainStub = "package main\n\nfunc main() {\n\t<-make(chan struct{})\n}\n"
	} else {
		mainStub = `package main

import (
	"runtime"
	"unsafe"
)

func main() {
	// Read work info from a fixed WASM memory offset (1024).
	// The wasmtime backend writes the entry point name and input JSON
	// here before calling _start. The wazero backend doesn't write
	// anything, so entryNameLen stays 0 and we fall through to the
	// blocking keep-alive path for direct export calls.
	const workOffset = 1024
	workPtr := unsafe.Pointer(uintptr(workOffset))
	entryNameLen := *(*int32)(workPtr)
	argsLen := *(*int32)(unsafe.Pointer(uintptr(workOffset + 4)))

	if entryNameLen > 0 && entryNameLen <= 256 && argsLen >= 0 && argsLen <= 65536 {
		entryBytes := unsafe.Slice((*byte)(unsafe.Pointer(uintptr(workOffset+8))), int(entryNameLen))
		argsBytes := unsafe.Slice((*byte)(unsafe.Pointer(uintptr(workOffset+8+entryNameLen))), int(argsLen))
		result := cleatDispatch(string(entryBytes), argsBytes)
		resultPtr, resultLen := stringPtr(string(result))
		cleatCompleteImport(0, resultPtr, resultLen)
		return
	}

	// Keep a goroutine always runnable to prevent Go WASI
	// deadlock detection from firing proc_exit(2).
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				return
			default:
				runtime.Gosched()
			}
		}
	}()
	<-done
}
`
	}
	if err := writeFile("gen_main_stub.go", mainStub); err != nil {
		return err
	}

	// Create go.mod with replace directive pointing to the project root.
	// TinyGo caps the go version to its supported maximum.
	goVersion := "1.23"
	replaceRoot := cfg.ProjectRoot
	// Create a minimal dependency tree with go 1.23 so TinyGo doesn't
	// reject the project root's go 1.26 requirement.
	depsDir := filepath.Join(cfg.OutDir, ".deps")
	if err := os.MkdirAll(filepath.Join(depsDir, "cleat"), 0755); err != nil {
		return fmt.Errorf("creating .deps/cleat: %w", err)
	}
	// Copy cleat SDK into .deps/
	srcCleat := filepath.Join(cfg.ProjectRoot, "cleat")
	goFiles, err := filepath.Glob(filepath.Join(srcCleat, "*.go"))
	if err != nil {
		return fmt.Errorf("globbing cleat source: %w", err)
	}
	for _, gf := range goFiles {
		base := filepath.Base(gf)
		if strings.HasPrefix(base, "gen_") {
			continue
		}
		content, err := os.ReadFile(gf)
		if err != nil {
			return fmt.Errorf("reading %s: %w", base, err)
		}
		if err := os.WriteFile(filepath.Join(depsDir, "cleat", base), content, 0644); err != nil {
			return fmt.Errorf("writing %s: %w", base, err)
		}
	}

	// Copy cleat/go.mod and go.sum into .deps/cleat/ so that
	// github.com/cleat-team/cleat/cleat is a proper submodule
	// resolvable via the replace directive.
	for _, modFile := range []string{"go.mod", "go.sum"} {
		srcMod := filepath.Join(srcCleat, modFile)
		if data, err := os.ReadFile(srcMod); err == nil {
			os.WriteFile(filepath.Join(depsDir, "cleat", modFile), data, 0644)
		}
	}
	// Also copy cleattest if present.
	srcCleattest := filepath.Join(srcCleat, "cleattest")
	if st, err := os.Stat(srcCleattest); err == nil && st.IsDir() {
		if err := os.MkdirAll(filepath.Join(depsDir, "cleattest"), 0755); err != nil {
			return fmt.Errorf("creating .deps/cleattest: %w", err)
		}
		testGoFiles, _ := filepath.Glob(filepath.Join(srcCleattest, "*.go"))
		for _, gf := range testGoFiles {
			base := filepath.Base(gf)
			content, err := os.ReadFile(gf)
			if err != nil {
				continue
			}
			os.WriteFile(filepath.Join(depsDir, "cleattest", base), content, 0644)
		}
	}
	// Write .deps/go.mod with a compatible version.
	depsMod := fmt.Sprintf(`module %s

go 1.23
`, cfg.ModulePath)
	if err := os.WriteFile(filepath.Join(depsDir, "go.mod"), []byte(depsMod), 0644); err != nil {
		return fmt.Errorf("writing .deps/go.mod: %w", err)
	}
	absDeps, err := filepath.Abs(depsDir)
	if err != nil {
		return fmt.Errorf("resolving .deps path: %w", err)
	}
	replaceRoot = absDeps

	// Use a minimal go.mod for TinyGo. The cleat/cleat submodule is
	// replaced directly (not via the root module), and go mod tidy is
	// run by the caller to generate the go.sum.
	modContent := fmt.Sprintf(`module cleat-build

go %s

require %s v0.0.0

replace %s => %s/cleat
`, goVersion, cfg.ModulePath+"/cleat", cfg.ModulePath+"/cleat", replaceRoot)

	modPath := filepath.Join(cfg.OutDir, "go.mod")
	if err := os.WriteFile(modPath, []byte(modContent), 0644); err != nil {
		return fmt.Errorf("writing go.mod: %w", err)
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
