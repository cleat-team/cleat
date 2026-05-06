package wasm

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	// (e.g., "github.com/rcownie/durable").
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
			dst := filepath.Join(cfg.OutDir, base)
			content, err := os.ReadFile(src)
			if err != nil {
				return fmt.Errorf("reading %s: %w", base, err)
			}
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

	// Write a main package stub. The stub differs by target:
	//   - tinygo: blocks on channel to keep main() alive (asyncify scheduler
	//     handles exports while main is blocked). The deadlock message on stdout
	//     is harmless — the module stays alive.
	//   - go: select{} blocks forever with zero CPU to keep the runtime alive.
	var mainStub string
	if cfg.Target == "tinygo" {
		mainStub = `package main

func main() {
	<-make(chan struct{})
}
`
	} else {
		mainStub = `package main

func main() {
	select {}
}
`
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
		goVersion = "1.24"
		// Create a minimal dependency tree with go 1.24 so tinygo doesn't
		// reject the project root's go 1.26 requirement.
		depsDir := filepath.Join(cfg.OutDir, ".deps")
		if err := os.MkdirAll(filepath.Join(depsDir, "durable"), 0755); err != nil {
			return fmt.Errorf("creating .deps/durable: %w", err)
		}
		// Copy durable SDK into .deps/
		srcDurable := filepath.Join(cfg.ProjectRoot, "durable")
		goFiles, err := filepath.Glob(filepath.Join(srcDurable, "*.go"))
		if err != nil {
			return fmt.Errorf("globbing durable source: %w", err)
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
			if err := os.WriteFile(filepath.Join(depsDir, "durable", base), content, 0644); err != nil {
				return fmt.Errorf("writing %s: %w", base, err)
			}
		}
		// Also copy durabletest if present.
		srcDurabletest := filepath.Join(srcDurable, "durabletest")
		if st, err := os.Stat(srcDurabletest); err == nil && st.IsDir() {
			if err := os.MkdirAll(filepath.Join(depsDir, "durabletest"), 0755); err != nil {
				return fmt.Errorf("creating .deps/durabletest: %w", err)
			}
			testGoFiles, _ := filepath.Glob(filepath.Join(srcDurabletest, "*.go"))
			for _, gf := range testGoFiles {
				base := filepath.Base(gf)
				content, err := os.ReadFile(gf)
				if err != nil {
					continue
				}
				os.WriteFile(filepath.Join(depsDir, "durabletest", base), content, 0644)
			}
		}
		// Write .deps/go.mod with a compatible version.
		depsMod := fmt.Sprintf(`module %s

go 1.24
`, cfg.ModulePath)
		if err := os.WriteFile(filepath.Join(depsDir, "go.mod"), []byte(depsMod), 0644); err != nil {
			return fmt.Errorf("writing .deps/go.mod: %w", err)
		}
		replaceRoot = depsDir
	}

	modContent := fmt.Sprintf(`module durable-build

go %s

require %s v0.0.0

replace %s => %s
`, goVersion, cfg.ModulePath, cfg.ModulePath, replaceRoot)

	modPath := filepath.Join(cfg.OutDir, "go.mod")
	if err := os.WriteFile(modPath, []byte(modContent), 0644); err != nil {
		return fmt.Errorf("writing go.mod: %w", err)
	}

	return nil
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

