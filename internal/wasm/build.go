package wasm

import (
	"fmt"
	"io"
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

	// Copy or write user source files.
	if len(cfg.XfrmSource) > 0 {
		// Write transformed source files.
		for filename, content := range cfg.XfrmSource {
			base := filepath.Base(filename)
			if strings.HasPrefix(base, "gen_") {
				continue
			}
			dst := filepath.Join(cfg.OutDir, base)
			if err := os.WriteFile(dst, content, 0644); err != nil {
				return fmt.Errorf("writing transformed %s: %w", base, err)
			}
		}
	} else {
		// Copy original source files.
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
			if err := copyFile(src, dst); err != nil {
				return fmt.Errorf("copying %s: %w", base, err)
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

	// Create go.mod with replace directive pointing to the project root.
	modContent := fmt.Sprintf(`module durable-build

go %s

require %s v0.0.0

replace %s => %s
`, cfg.GoVersion, cfg.ModulePath, cfg.ModulePath, cfg.ProjectRoot)

	modPath := filepath.Join(cfg.OutDir, "go.mod")
	if err := os.WriteFile(modPath, []byte(modContent), 0644); err != nil {
		return fmt.Errorf("writing go.mod: %w", err)
	}

	return nil
}

// copyFile copies a file from src to dst.
func copyFile(src, dst string) error {
	s, err := os.Open(src)
	if err != nil {
		return err
	}
	defer s.Close()

	d, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer d.Close()

	_, err = io.Copy(d, s)
	return err
}
