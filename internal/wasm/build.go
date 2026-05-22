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

	if cfg.Target == GoTarget {
		// Standard Go wasip1: main() calls cleat_poll_work (a host import)
		// to get the entry point name and input JSON from the host, then
		// dispatches to the entry point. The host calls _start synchronously;
		// main() processes one unit of work and returns.
		// cleatPollWorkImport is declared in gen_wasm_imports.go.
		mainStub := "package main\n\n" +
			"import \"unsafe\"\n\n" +
			"func cleatPollWork() (string, []byte) {\n" +
			"\tvar entryBuf [256]byte\n" +
			"\tvar argsBuf [65536]byte\n" +
			"\tresult := cleatPollWorkImport(unsafe.Pointer(&entryBuf[0]), 256, unsafe.Pointer(&argsBuf[0]), 65536)\n" +
			"\tentryLen := uint32(uint64(result) >> 32)\n" +
			"\targsLen := uint32(uint64(result) & 0xFFFFFFFF)\n" +
			"\tif entryLen > 256 { entryLen = 256 }\n" +
			"\tif argsLen > 65536 { argsLen = 65536 }\n" +
			"\treturn string(entryBuf[:entryLen]), argsBuf[:argsLen]\n" +
			"}\n\n" +
			"func main() {\n" +
			"\tentryName, argsJSON := cleatPollWork()\n" +
			"\tresultJSON := cleatDispatch(entryName, argsJSON)\n" +
			"\tresultStr := string(resultJSON)\n" +
			"\tresultPtr, resultLen := stringPtr(resultStr)\n" +
			"\tcleatCompleteImport(0, resultPtr, resultLen)\n" +
			"}\n"
		if err := writeFile("gen_main_stub.go", mainStub); err != nil {
			return err
		}
	} else {
		// TinyGo: main() blocks on a channel receive so the asyncify
		// scheduler can run exports while main is blocked.
		mainStub := "package main\n\nfunc main() {\n\t<-make(chan struct{})\n}\n"
		if err := writeFile("gen_main_stub.go", mainStub); err != nil {
			return err
		}
	}

	if cfg.Target == GoTarget {
		// Standard Go: use the project's real go.mod version. The replace
		// directive points directly to the project root — no .deps/
		// workaround needed.
		goVersion := cfg.GoVersion
		if goVersion == "" {
			goVersion = "1.25"
		}
		modContent := fmt.Sprintf(`module cleat-build

go %s

require %s v0.0.0

replace %s => %s
`, goVersion, cfg.ModulePath+"/cleat", cfg.ModulePath+"/cleat", filepath.Join(cfg.ProjectRoot, "cleat"))

		modPath := filepath.Join(cfg.OutDir, "go.mod")
		if err := os.WriteFile(modPath, []byte(modContent), 0644); err != nil {
			return fmt.Errorf("writing go.mod: %w", err)
		}
	} else {
		// TinyGo: cap the go version to TinyGo's supported maximum and
		// create a .deps/ shim to avoid the project root's newer go.mod.
		goVersion := "1.23"
		replaceRoot := cfg.ProjectRoot

		depsDir := filepath.Join(cfg.OutDir, ".deps")
		if err := os.MkdirAll(filepath.Join(depsDir, "cleat"), 0755); err != nil {
			return fmt.Errorf("creating .deps/cleat: %w", err)
		}
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

		for _, modFile := range []string{"go.mod", "go.sum"} {
			srcMod := filepath.Join(srcCleat, modFile)
			if data, err := os.ReadFile(srcMod); err == nil {
				os.WriteFile(filepath.Join(depsDir, "cleat", modFile), data, 0644)
			}
		}
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
		depsMod := fmt.Sprintf(`module %s

go 1.23
`, cfg.ModulePath)
		if err := os.WriteFile(filepath.Join(depsDir, "go.mod"), []byte(depsMod), 0644); err != nil {
			return fmt.Errorf("writing .deps/go.mod: %w", err)
		}
		replaceRoot = depsDir

		modContent := fmt.Sprintf(`module cleat-build

go %s

require %s v0.0.0

replace %s => %s/cleat
`, goVersion, cfg.ModulePath+"/cleat", cfg.ModulePath+"/cleat", replaceRoot)

		modPath := filepath.Join(cfg.OutDir, "go.mod")
		if err := os.WriteFile(modPath, []byte(modContent), 0644); err != nil {
			return fmt.Errorf("writing go.mod: %w", err)
		}
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
