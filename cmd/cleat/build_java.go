package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/cleat-team/cleat/wasm"
)

// runBuildJava compiles a Java workflow to WASM using Gradle and the TeaVM plugin.
func runBuildJava(pattern, outDir, channel string, workflowVersion int) {
	javaDir := pattern
	if javaDir == "" {
		javaDir = "."
	}
	// RESOLVED TO ABSOLUTE HERE, before it is used to build gradleBin below.
	// exec.Command resolves a RELATIVE executable path against cmd.Dir, not
	// against the caller's cwd -- and cmd.Dir is set to javaDir a few lines
	// down. A relative javaDir with its own gradlew wrapper therefore looked
	// for "<javaDir>/<javaDir>/gradlew", doubled, and failed with "no such
	// file or directory" on a project that plainly had one. cleat#1890.
	//
	// Invisible until now because no Java fixture combined a gradlew wrapper
	// with a caller passing a relative path: tests/plugin-harness's fixtures
	// have wrappers but are invoked through an absolute t.TempDir()-adjacent
	// path (absolute needs no resolution, so cmd.Dir doubling never
	// triggers), and examples/saga-java-port has no wrapper of its own, so it
	// falls back to a bare "gradle" resolved via PATH regardless of cmd.Dir.
	// examples/java-workflow is the first fixture with both properties, and
	// exposed it the first time someone ran `cleat build --target java
	// examples/java-workflow` from the repo root instead of an absolute path.
	if abs, err := filepath.Abs(javaDir); err == nil {
		javaDir = abs
	}

	// Validate build file exists (Groovy or Kotlin DSL).
	gradleFile := filepath.Join(javaDir, "build.gradle")
	gradleKtsFile := filepath.Join(javaDir, "build.gradle.kts")
	if _, err := os.Stat(gradleFile); os.IsNotExist(err) {
		if _, err := os.Stat(gradleKtsFile); os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "Error: no build.gradle or build.gradle.kts found in %s\n", javaDir)
			fmt.Fprintf(os.Stderr, "Java workflows require a Gradle build file with the TeaVM plugin configured.\n")
			os.Exit(1)
		}
	}

	// Determinism gate. See the equivalent in build_rust.go and cleat#1770:
	// runBuild early-returns here before ever reaching analyze(), so a Java
	// workflow used to compile to a deployable artifact with no determinism
	// checking at all.
	//
	// Placed AFTER the build-file validation and BEFORE the gradle lookup.
	// After, because "no build.gradle" is the more fundamental complaint and
	// an empty directory should hear that rather than "no .java files"
	// (TestRunBuild_JavaTarget_NoBuildFile asserts exactly this). Before,
	// because runVetJava is pure Go and needs no toolchain, so a project with
	// determinism errors is refused on a machine that could not have built it.
	//
	// runVetJava resolves imports and fully-qualified names to a canonical
	// path before matching (cleat#1812), not the literal-spelling table this
	// comment used to describe. It still has real limits -- java.nio and
	// reflection with a computed argument -- see the known-limit fixture
	// referenced in java_build_refuses_nondeterminism_test.go.
	if code := runVetJava(javaDir); code != 0 {
		fmt.Fprintf(os.Stderr, "\nError: determinism check failed for %s -- no artifact was emitted.\n", javaDir)
		fmt.Fprintf(os.Stderr, "Fix the errors above, or run 'cleat vet --lang java %s' to see them again.\n", javaDir)
		os.Exit(1)
	}

	// Prefer the Gradle wrapper (gradlew) if present, otherwise fall back to
	// the system gradle.  The wrapper ensures a compatible Gradle version.
	gradleBin := "gradle"
	gradlewPath := filepath.Join(javaDir, "gradlew")
	if _, err := os.Stat(gradlewPath); err == nil {
		gradleBin = gradlewPath
	} else if _, err := exec.LookPath("gradle"); err != nil {
		fmt.Fprintf(os.Stderr, "Error: gradle not found. Install Gradle: https://gradle.org/install/\n")
		os.Exit(1)
	}

	fmt.Printf("  Compiling Java to WASM via TeaVM...\n")
	cmd := exec.Command(gradleBin, "generateWasm", "-q")
	cmd.Dir = javaDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()
	if channel != "" {
		cmd.Env = append(cmd.Env, "CLEAT_CHILD_BINDING_POLICY="+channel)
	}

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

	// Use workflow name from directory name. Derived BEFORE the metadata is
	// written, because the metadata needs it -- see nonGoMetadata. javaDir is
	// already absolute (resolved at the top of this function), so no second
	// filepath.Abs is needed here.
	name := filepath.Base(javaDir)
	name = strings.ReplaceAll(name, "-", "_")

	// Inject cleat.metadata. Must pass wasm.Metadata.Validate(), which
	// `cleat deploy` runs and exits 1 on; see cleat#1077.
	if enriched, metaErr := wasm.WriteMetadata(input,
		nonGoMetadata("java", name, workflowVersion, javaEntryPointNames(javaDir))); metaErr == nil {
		input = enriched
	}

	dstWasm := filepath.Join(outDir, name+".wasm")
	if err := os.WriteFile(dstWasm, input, 0644); err != nil {
		fmt.Fprintf(os.Stderr, "Error: writing WASM output: %v\n", err)
		os.Exit(1)
	}

	fi, _ := os.Stat(dstWasm)
	fmt.Printf("  Wrote %s (%s)\n", dstWasm, formatSize(fi.Size()))
}
