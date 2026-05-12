package wasm

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/rcownie/cleat/internal/analyzer"
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
	// (e.g., "github.com/rcownie/cleat").
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

	// Target is the compilation target: "go" (default) or "tinygo".
	Target string

	// XfrmSource, if non-nil, provides transformed source files to write
	// instead of copying original source files. Keyed by filename.
	XfrmSource map[string][]byte
}

// PrepareBuildDir assembles the build directory: copies user source files,
// writes generated files, and creates a go.mod that resolves the project
// module via a replace directive. Returns the path to the build directory.
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
		mainStub = "package main\n\nfunc main() {\n\t// Keep a goroutine always runnable to prevent Go WASI\n\t// deadlock detection from firing proc_exit(2).\n\tdone := make(chan struct{})\n\tgo func() {\n\t\tfor {\n\t\t\tselect {\n\t\t\tcase <-done:\n\t\t\t\treturn\n\t\t\tdefault:\n\t\t\t}\n\t\t}\n\t}()\n\t<-done\n}\n"
	}
	if err := writeFile("gen_main_stub.go", mainStub); err != nil {
		return err
	}

	// Create go.mod with replace directive pointing to the project root.
	// tinygo has a max supported Go version (1.24 for tinygo 0.36-0.37),
	// so cap the go version and provide a stub dependency with a compatible
	// go directive when targeting tinygo.
	goVersion := cfg.GoVersion
	replaceRoot := cfg.ProjectRoot
	if cfg.Target == "tinygo" {
		goVersion = "1.23"
		// Create a minimal dependency tree with go 1.23 so tinygo doesn't
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
			// github.com/rcownie/cleat/cleat is a proper submodule
			// resolvable via the replace directive for the root module.
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
		replaceRoot = depsDir
	}

	// Read the project's go.mod and extract require and replace directives
	// for transitive dependencies (e.g., cleat module) so they are resolvable
	// from the build directory.
	var extraRequires string
	var extraReplaces string
	var inRequireBlock bool
	modFilePath := filepath.Join(cfg.ProjectRoot, "go.mod")
	if modData, err := os.ReadFile(modFilePath); err == nil {
		for _, line := range strings.Split(string(modData), "\n") {
			trimmed := strings.TrimSpace(line)
			switch {
			case strings.HasPrefix(trimmed, "require ("):
				inRequireBlock = true
			case inRequireBlock && trimmed == ")":
				inRequireBlock = false
			case inRequireBlock && trimmed != "":
				parts := strings.Fields(trimmed)
				if len(parts) >= 2 && parts[0] != cfg.ModulePath {
					extraRequires += "\t" + parts[0] + " " + parts[1] + "\n"
				}
			case strings.HasPrefix(trimmed, "require ") && !inRequireBlock:
				// Single-line require: "require mod v0.0.0"
				modExpr := strings.TrimSpace(strings.TrimPrefix(trimmed, "require "))
				if modExpr != "" {
					parts := strings.Fields(modExpr)
					if len(parts) >= 2 && parts[0] != cfg.ModulePath {
						extraRequires += "\t" + parts[0] + " " + parts[1] + "\n"
					}
				}
			case strings.HasPrefix(trimmed, "replace ") && strings.Contains(trimmed, "=>"):
				parts := strings.SplitN(trimmed, "=>", 2)
				modName := strings.TrimSpace(strings.TrimPrefix(parts[0], "replace "))
				if modName != cfg.ModulePath {
					// For TinyGo targets, submodules already provided
					// by the .deps/ tree are skipped to avoid ambiguity
					// with the project root's replace directives.
					if cfg.Target == "tinygo" && strings.HasPrefix(modName, cfg.ModulePath+"/") {
						continue
					}
					// Resolve relative paths in replacement targets
					// to absolute paths so the build directory can
					// be anywhere.
					targetPath := strings.TrimSpace(parts[1])
					if strings.HasPrefix(targetPath, ".") || (!strings.HasPrefix(targetPath, "/") && !strings.HasPrefix(targetPath, `"`) && !strings.HasPrefix(targetPath, "github.com/")) {
						absPath := filepath.Join(cfg.ProjectRoot, targetPath)
						extraReplaces += fmt.Sprintf("\treplace %s => %s\n", modName, absPath)
					} else {
						extraReplaces += "\t" + trimmed + "\n"
					}
				}
			}
		}
	}

	// Wrap extra requires in a require block if present.
	var modContent string
	if cfg.Target == "tinygo" {
		// For TinyGo, use a minimal go.mod. The cleat/cleat
		// submodule is replaced directly (not via the root
		// module), and go mod tidy is run by the caller to
		// generate the go.sum. This avoids issues with v0.0.0
		// version resolution.
		modContent = fmt.Sprintf(`module cleat-build

go %s

require %s v0.0.0

replace %s => %s/cleat
`, goVersion, cfg.ModulePath+"/cleat", cfg.ModulePath+"/cleat", replaceRoot)
	} else {
		requireBlock := fmt.Sprintf("require %s v0.0.0", cfg.ModulePath)
		if extraRequires != "" {
			requireBlock = fmt.Sprintf("require (\n\t%s v0.0.0\n%s)", cfg.ModulePath, extraRequires)
		}

		modContent = fmt.Sprintf(`module cleat-build

go %s

%s`, goVersion, requireBlock)

		if extraReplaces != "" {
			modContent += fmt.Sprintf(`
replace %s => %s
%s`, cfg.ModulePath, replaceRoot, extraReplaces)
		} else {
			modContent += fmt.Sprintf(`
replace %s => %s`, cfg.ModulePath, replaceRoot)
		}
	}

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

// BuildPythonWasm compiles a Python workflow to a WASM component using
// componentize-py via the python-sdk/scripts/build_wasm.py helper script.
//
// entry should be in "file.py:func_name" format. output is the path to the
// resulting .wasm file. If verbose is true, componentize-py output is shown.
func BuildPythonWasm(entry, output string, verbose bool) error {
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

