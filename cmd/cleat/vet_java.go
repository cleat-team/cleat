package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// forbiddenJavaPatterns lists Java APIs that are not allowed in workflow code.
var forbiddenJavaPatterns = []struct {
	pattern    string
	code       string
	message    string
	suggestion string
}{
	{`System.currentTimeMillis()`, "J001", "wall-clock time is non-deterministic across replays", "Use h.Now() for deterministic time"},
	{`Math.random()`, "J002", "non-deterministic random number generation is not allowed", "Use h.Random() for deterministic randomness"},
	{`Thread.sleep`, "J003", "thread sleeping is non-deterministic across replays", "Use h.DurableSleep() for deterministic timers"},
	{`import java.io.`, "J004", "I/O operations are not allowed in durable functions", "Use h.DurableCall() to interact with external services"},
	{`import java.net.`, "J005", "network access is not allowed in durable functions", "Use h.DurableCall() to communicate with external services"},
	{`import java.sql.`, "J006", "database access is not allowed in durable functions", "Use h.DurableCall() to interact with databases"},
	{`import java.time.`, "J007", "wall-clock time imports may cause non-determinism", "Use h.Now() for deterministic time"},
	{`new java.io.`, "J008", "I/O operations are not allowed in durable functions", "Use h.DurableCall() to interact with external services"},
	{`new java.net.`, "J009", "network access is not allowed in durable functions", "Use h.DurableCall() to communicate with external services"},
	{`new java.sql.`, "J010", "database access is not allowed in durable functions", "Use h.DurableCall() to interact with databases"},
	{`new Thread(`, "J011", "threading is not allowed in durable functions", "Workflow code is single-threaded by design"},
	{`new Timer(`, "J012", "timers are non-deterministic across replays", "Use h.DurableSleep() for deterministic timers"},
	{`new File(`, "J013", "filesystem access is not allowed in durable functions", "Use h.DurableCall() to interact with external storage"},
	{`new Socket(`, "J014", "network access is not allowed in durable functions", "Use h.DurableCall() to communicate with external services"},
	{`new ServerSocket(`, "J014", "network access is not allowed in durable functions", "Use h.DurableCall() to communicate with external services"},
	{`java.io.File`, "J013", "filesystem access is not allowed in durable functions", "Use h.DurableCall() to interact with external storage"},
	{`Socket socket`, "J014", "network access is not allowed in durable functions", "Use h.DurableCall() to communicate with external services"},
	{`ServerSocket`, "J014", "network access is not allowed in durable functions", "Use h.DurableCall() to communicate with external services"},
	{`Connection con`, "J015", "direct database access is not allowed in durable functions", "Use h.DurableCall() to interact with databases"},
	{`InputStream`, "J016", "I/O stream usage is not allowed in durable functions", "Use h.DurableCall() to interact with external services"},
	{`OutputStream`, "J016", "I/O stream usage is not allowed in durable functions", "Use h.DurableCall() to interact with external services"},
	{`FileReader`, "J013", "filesystem access is not allowed in durable functions", "Use h.DurableCall() to interact with external storage"},
	{`FileWriter`, "J013", "filesystem access is not allowed in durable functions", "Use h.DurableCall() to interact with external storage"},
	{`FileInputStream`, "J013", "filesystem access is not allowed in durable functions", "Use h.DurableCall() to interact with external storage"},
	{`FileOutputStream`, "J013", "filesystem access is not allowed in durable functions", "Use h.DurableCall() to interact with external storage"},
	{`ObjectInputStream`, "J016", "I/O stream usage is not allowed in durable functions", "Use h.DurableCall() to interact with external services"},
	{`ObjectOutputStream`, "J016", "I/O stream usage is not allowed in durable functions", "Use h.DurableCall() to interact with external services"},
	{`Random random`, "J002", "non-deterministic random number generation is not allowed", "Use h.Random() for deterministic randomness"},
	{`new Random(`, "J002", "non-deterministic random number generation is not allowed", "Use h.Random() for deterministic randomness"},
	{`Runtime.getRuntime()`, "J017", "runtime execution is not allowed in durable functions", "Use h.DurableCall() for side effects"},
	{`ProcessBuilder`, "J018", "process spawning is not allowed in durable functions", "Use h.DurableCall() for side effects"},
	{`java.util.concurrent`, "J019", "concurrent execution is not allowed in durable functions", "Workflow code is single-threaded by design"},
	{`import java.util.Random`, "J002", "non-deterministic random number generation is not allowed", "Use h.Random() for deterministic randomness"},
}

// runVetJava performs static analysis on a Java project by scanning for
// forbidden API patterns via grep-like source analysis.
// Returns 0 on success (no errors), 1 if errors were found.
func runVetJava(projectDir string) int {
	// Validate the directory exists.
	if projectDir == "" {
		projectDir = "."
	}

	buildGradle := filepath.Join(projectDir, "build.gradle.kts")
	if _, err := os.Stat(buildGradle); os.IsNotExist(err) {
		buildGradle = filepath.Join(projectDir, "build.gradle")
		if _, err := os.Stat(buildGradle); os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "Warning: no build.gradle.kts or build.gradle found in %s\n", projectDir)
			// Continue anyway — user may have a different build setup.
		}
	}

	fmt.Fprintf(os.Stderr, "Vetting Java project in %s...\n", projectDir)

	// Find all .java files.
	var javaFiles []string
	err := filepath.WalkDir(projectDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // skip inaccessible
		}
		if d.IsDir() {
			// Skip build directories and hidden dirs.
			if d.Name() == "build" || d.Name() == ".gradle" || d.Name() == ".git" ||
				d.Name() == "target" || d.Name() == "node_modules" ||
				strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".java") {
			javaFiles = append(javaFiles, path)
		}
		return nil
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error scanning Java source files: %v\n", err)
		os.Exit(1)
	}

	if len(javaFiles) == 0 {
		fmt.Fprintf(os.Stderr, "Error: no .java source files found in %s\n", projectDir)
		os.Exit(1)
	}

	var output VetOutput
	output.Summary.Functions = len(javaFiles)

	// Scan each .java file for forbidden patterns.
	for _, javaFile := range javaFiles {
		relPath, _ := filepath.Rel(projectDir, javaFile)
		data, err := os.ReadFile(javaFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: could not read %s: %v\n", javaFile, err)
			continue
		}

		lines := strings.Split(string(data), "\n")
		for lineIdx, line := range lines {
			lineNum := lineIdx + 1 // 1-based
			trimmed := strings.TrimSpace(line)

			// Skip comments.
			if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "*") ||
				strings.HasPrefix(trimmed, "/*") {
				continue
			}

			for _, fb := range forbiddenJavaPatterns {
				if fb.pattern == "" {
					continue
				}
				if strings.Contains(trimmed, fb.pattern) {
					col := strings.Index(trimmed, fb.pattern) + 1 // 1-based

					vr := VetResult{
						Code:       fb.code,
						File:       relPath,
						Line:       lineNum,
						Column:     col,
						Message:    fb.message,
						Suggestion: fb.suggestion,
					}
					output.Errors = append(output.Errors, vr)
				}
			}
		}
	}

	// Check for @CleatEntry annotations.
	var hasCleatEntry bool
	for _, javaFile := range javaFiles {
		data, err := os.ReadFile(javaFile)
		if err != nil {
			continue
		}
		if strings.Contains(string(data), "@CleatEntry") {
			hasCleatEntry = true
			break
		}
	}
	if !hasCleatEntry {
		output.Warnings = append(output.Warnings, VetResult{
			Code:       "J100",
			Message:    "no @CleatEntry annotation found in any source file",
			Suggestion: "Add @CleatEntry to at least one method to define a workflow entry point",
		})
	}

	// Report results.
	// DurableLeaves, DurableClosure, Pure are 0 for pattern-based vets.

	// Check if JSON output is requested.
	jsonOutput := false
	for _, arg := range os.Args {
		if arg == "--json" {
			jsonOutput = true
			break
		}
	}

	if jsonOutput {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(output); err != nil {
			fmt.Fprintf(os.Stderr, "Error encoding JSON output: %v\n", err)
			os.Exit(1)
		}
	} else {
		// Human-readable output.
		for _, e := range output.Errors {
			fmt.Printf("  Error [%s] %s:%d:%d: %s\n", e.Code, e.File, e.Line, e.Column, e.Message)
			if e.Suggestion != "" {
				fmt.Printf("    suggestion: %s\n", e.Suggestion)
			}
		}
		for _, w := range output.Warnings {
			if w.File != "" {
				fmt.Printf("  Warning [%s] %s:%d:%d: %s\n", w.Code, w.File, w.Line, w.Column, w.Message)
			} else {
				fmt.Printf("  Warning [%s] %s\n", w.Code, w.Message)
			}
			if w.Suggestion != "" {
				fmt.Printf("    suggestion: %s\n", w.Suggestion)
			}
		}
		fmt.Printf("\n  Summary: %d files, %d errors, %d warnings\n",
			output.Summary.Functions, len(output.Errors), len(output.Warnings))
	}

	if len(output.Errors) > 0 {
		return 1
	}
	return 0
}
