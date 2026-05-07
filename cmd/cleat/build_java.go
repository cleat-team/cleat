package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// runBuildJava compiles a Java workflow to WASM using Gradle and the TeaVM plugin.
func runBuildJava(pattern, outDir string) {
	javaDir := pattern
	if javaDir == "" {
		javaDir = "."
	}

	// Validate build.gradle.kts exists.
	gradlePath := filepath.Join(javaDir, "build.gradle.kts")
	if _, err := os.Stat(gradlePath); os.IsNotExist(err) {
		// Also check for build.gradle (Groovy DSL).
		gradleGroovyPath := filepath.Join(javaDir, "build.gradle")
		if _, err := os.Stat(gradleGroovyPath); os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "Error: no build.gradle.kts or build.gradle found in %s\n", javaDir)
			fmt.Fprintf(os.Stderr, "Java workflows require a Gradle build file with the TeaVM plugin configured.\n")
			os.Exit(1)
		}
		gradlePath = gradleGroovyPath
	}

	// Check for gradle on PATH.
	if _, err := exec.LookPath("gradle"); err != nil {
		fmt.Fprintf(os.Stderr, "Error: gradle not found. Install Gradle: https://gradle.org/install/\n")
		os.Exit(1)
	}

	fmt.Printf("  Compiling Java to WASM via TeaVM...\n")
	cmd := exec.Command("gradle", "build", "-q")
	cmd.Dir = javaDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: gradle build failed: %v\n", err)
		fmt.Fprintf(os.Stderr, "Make sure the TeaVM plugin is configured in the Gradle build file.\n")
		os.Exit(1)
	}

	// Locate the output .wasm file.
	// TeaVM typically outputs to build/wasm/ or build/generated/teavm/.
	wasmDir := filepath.Join(javaDir, "build", "wasm")
	if _, err := os.Stat(wasmDir); os.IsNotExist(err) {
		wasmDir = filepath.Join(javaDir, "build", "generated", "teavm")
	}

	matches, _ := filepath.Glob(filepath.Join(wasmDir, "*.wasm"))
	if len(matches) == 0 {
		// Try broader search — WalkDir, because filepath.Glob doesn't support **.
		_ = filepath.WalkDir(filepath.Join(javaDir, "build"), func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil // skip inaccessible files/dirs
			}
			if !d.IsDir() && strings.HasSuffix(path, ".wasm") {
				matches = append(matches, path)
			}
			return nil
		})
	}
	if len(matches) == 0 {
		fmt.Fprintf(os.Stderr, "Error: no .wasm file found in build output\n")
		fmt.Fprintf(os.Stderr, "Looked in: %s/build/wasm/ and %s/build/generated/teavm/\n", javaDir, javaDir)
		os.Exit(1)
	}

	srcWasm := matches[0]

	input, err := os.ReadFile(srcWasm)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: could not read WASM output: %v\n", err)
		fmt.Fprintf(os.Stderr, "Looked in: %s\n", srcWasm)
		os.Exit(1)
	}

	// Use workflow name from directory name.
	absDir, _ := filepath.Abs(javaDir)
	name := filepath.Base(absDir)
	name = strings.ReplaceAll(name, "-", "_")

	dstWasm := filepath.Join(outDir, name+".wasm")
	if err := os.WriteFile(dstWasm, input, 0644); err != nil {
		fmt.Fprintf(os.Stderr, "Error: writing WASM output: %v\n", err)
		os.Exit(1)
	}

	fi, _ := os.Stat(dstWasm)
	fmt.Printf("  Wrote %s (%s)\n", dstWasm, formatSize(fi.Size()))
}
