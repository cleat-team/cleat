package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// forbiddenJavaPatterns lists Java APIs that are not allowed in workflow code.
// javaCodeOnly returns src with every comment and every string, text block or
// character literal replaced by spaces, byte offsets and line endings
// untouched.
//
// BLANKING, NOT DELETING, because the caller reports 1-based line and column
// numbers straight out of this result. Shortening anything would move every
// position it reports, and the position is most of what a vet finding is worth.
// This is the same contract as rustCodeOnly in vet_rust.go, and the shape is
// deliberately borrowed rather than reinvented.
//
// WHAT IT REPLACES. This checker used to skip a line whose first non-space was
// "//", "*" or "/*". That is the easy case and misses three others, all
// measured (cleat#1820):
//
//	int x = 1; // System.currentTimeMillis()      a comment AFTER code
//	String s = "System.currentTimeMillis()";      a string literal
//	/*
//	  System.currentTimeMillis() described here   a block interior not
//	*/                                            starting with *
//
// None of the three can execute. A string naming an API is not a call to it,
// and neither is a sentence telling a colleague not to use it. Since #1791
// wired this checker into `cleat build --target java`, each was a build
// refusal.
//
// WHAT IT MODELS, since a lexer for a language this size is a claim worth
// bounding:
//
//   - line comments, including the doc form ///
//   - block comments, WHICH DO NOT NEST IN JAVA -- unlike Rust, the first */
//     closes the comment however many /* preceded it, so no depth counter
//   - "..." with backslash escapes
//   - text blocks """...""" (Java 15+), which end at the next unescaped """
//   - character literals, 'a' and '\n'
//
// Java's apostrophe carries none of Rust's ambiguity: there are no lifetimes,
// so ' always opens a character literal. That is the one place this is simpler
// than its sibling rather than merely smaller.
func javaCodeOnly(src []byte) []byte {
	out := make([]byte, len(src))
	copy(out, src)
	blank := func(i int) {
		if out[i] != '\n' {
			out[i] = ' '
		}
	}

	n := len(src)
	for i := 0; i < n; {
		switch {
		// Line comment: // ... end of line.
		case src[i] == '/' && i+1 < n && src[i+1] == '/':
			for ; i < n && src[i] != '\n'; i++ {
				blank(i)
			}

		// Block comment: /* ... */, not nesting.
		case src[i] == '/' && i+1 < n && src[i+1] == '*':
			blank(i)
			blank(i + 1)
			i += 2
			for i < n {
				if src[i] == '*' && i+1 < n && src[i+1] == '/' {
					blank(i)
					blank(i + 1)
					i += 2
					break
				}
				blank(i)
				i++
			}

		// Text block: """ ... """ (Java 15+). Checked before the plain string
		// case, or the opening """ would read as an empty string followed by a
		// stray quote and the block's contents would stay visible.
		case src[i] == '"' && i+2 < n && src[i+1] == '"' && src[i+2] == '"':
			blank(i)
			blank(i + 1)
			blank(i + 2)
			i += 3
			for i < n {
				if src[i] == '\\' && i+1 < n {
					blank(i)
					blank(i + 1)
					i += 2
					continue
				}
				if src[i] == '"' && i+2 < n && src[i+1] == '"' && src[i+2] == '"' {
					blank(i)
					blank(i + 1)
					blank(i + 2)
					i += 3
					break
				}
				blank(i)
				i++
			}

		// String literal.
		case src[i] == '"':
			blank(i)
			i++
			for i < n && src[i] != '\n' {
				if src[i] == '\\' && i+1 < n {
					blank(i)
					blank(i + 1)
					i += 2
					continue
				}
				if src[i] == '"' {
					blank(i)
					i++
					break
				}
				blank(i)
				i++
			}

		// Character literal.
		case src[i] == '\'':
			blank(i)
			i++
			for i < n && src[i] != '\n' {
				if src[i] == '\\' && i+1 < n {
					blank(i)
					blank(i + 1)
					i += 2
					continue
				}
				if src[i] == '\'' {
					blank(i)
					i++
					break
				}
				blank(i)
				i++
			}

		default:
			i++
		}
	}
	return out
}

var forbiddenJavaPatterns = []struct {
	pattern    string
	code       string
	message    string
	suggestion string
}{
	{`System.currentTimeMillis()`, "J001", "wall-clock time is non-deterministic across replays", "Use h.Now() for deterministic time"},
	{`Math.random()`, "J002", "non-deterministic random number generation is not allowed", "Use h.Random() for deterministic randomness"},
	{`Thread.sleep`, "J003", "thread sleeping is non-deterministic across replays", "Use h.DurableSleep() for deterministic timers"},
	{`import java.io.`, "J004", "I/O operations produce non-replayable side effects (file contents differ across replays)", "Use h.DurableCall() to interact with external services"},
	{`import java.net.`, "J005", "network access is non-deterministic across replays (network conditions differ between runs)", "Use h.DurableCall() to communicate with external services"},
	{`import java.sql.`, "J006", "database access produces non-replayable side effects (database state differs across replays)", "Use h.DurableCall() to interact with databases"},
	{`import java.time.`, "J007", "wall-clock time imports may cause non-determinism", "Use h.Now() for deterministic time"},
	{`new java.io.`, "J008", "I/O operations produce non-replayable side effects (file contents differ across replays)", "Use h.DurableCall() to interact with external services"},
	{`new java.net.`, "J009", "network access is non-deterministic across replays (network conditions differ between runs)", "Use h.DurableCall() to communicate with external services"},
	{`new java.sql.`, "J010", "database access produces non-replayable side effects (database state differs across replays)", "Use h.DurableCall() to interact with databases"},
	{`new Thread(`, "J011", "threading is non-deterministic across replays (thread scheduling differs between runs)", "Workflow code is single-threaded by design"},
	{`new Timer(`, "J012", "timers are non-deterministic across replays", "Use h.DurableSleep() for deterministic timers"},
	{`new File(`, "J013", "filesystem access is non-deterministic across replays (file contents differ between runs)", "Use h.DurableCall() to interact with external storage"},
	{`new Socket(`, "J014", "network access is non-deterministic across replays (network conditions differ between runs)", "Use h.DurableCall() to communicate with external services"},
	{`new ServerSocket(`, "J014", "network access is non-deterministic across replays (network conditions differ between runs)", "Use h.DurableCall() to communicate with external services"},
	{`java.io.File`, "J013", "filesystem access is non-deterministic across replays (file contents differ between runs)", "Use h.DurableCall() to interact with external storage"},
	{`Socket socket`, "J014", "network access is non-deterministic across replays (network conditions differ between runs)", "Use h.DurableCall() to communicate with external services"},
	{`ServerSocket`, "J014", "network access is non-deterministic across replays (network conditions differ between runs)", "Use h.DurableCall() to communicate with external services"},
	{`Connection con`, "J015", "database access produces non-replayable side effects (database state differs across replays)", "Use h.DurableCall() to interact with databases"},
	{`InputStream`, "J016", "I/O stream usage produces non-replayable side effects (stream state differs across replays)", "Use h.DurableCall() to interact with external services"},
	{`OutputStream`, "J016", "I/O stream usage produces non-replayable side effects (stream state differs across replays)", "Use h.DurableCall() to interact with external services"},
	{`FileReader`, "J013", "filesystem access is non-deterministic across replays (file contents differ between runs)", "Use h.DurableCall() to interact with external storage"},
	{`FileWriter`, "J013", "filesystem access is non-deterministic across replays (file contents differ between runs)", "Use h.DurableCall() to interact with external storage"},
	{`FileInputStream`, "J013", "filesystem access is non-deterministic across replays (file contents differ between runs)", "Use h.DurableCall() to interact with external storage"},
	{`FileOutputStream`, "J013", "filesystem access is non-deterministic across replays (file contents differ between runs)", "Use h.DurableCall() to interact with external storage"},
	{`ObjectInputStream`, "J016", "I/O stream usage produces non-replayable side effects (stream state differs across replays)", "Use h.DurableCall() to interact with external services"},
	{`ObjectOutputStream`, "J016", "I/O stream usage produces non-replayable side effects (stream state differs across replays)", "Use h.DurableCall() to interact with external services"},
	{`Random random`, "J002", "non-deterministic random number generation is not allowed", "Use h.Random() for deterministic randomness"},
	{`new Random(`, "J002", "non-deterministic random number generation is not allowed", "Use h.Random() for deterministic randomness"},
	{`Runtime.getRuntime()`, "J017", "runtime execution is non-deterministic across replays (OS process state differs between runs)", "Use h.DurableCall() for side effects"},
	{`ProcessBuilder`, "J018", "process spawning is non-deterministic across replays (process behavior differs between runs)", "Use h.DurableCall() for side effects"},
	{`java.util.concurrent`, "J019", "concurrent execution is non-deterministic across replays (thread scheduling differs between runs)", "Workflow code is single-threaded by design"},
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
			// Skip test source sets. cleat#1789.
			//
			// These are not workflow code and are not compiled into the
			// artifact, so their determinism is not a property of anything
			// that replays. While `cleat vet` was opt-in that was noise; #1791
			// wired this checker into `cleat build --target java`, so a test
			// that reads a fixture file now REFUSES THE BUILD -- and reading a
			// fixture file is what tests are for.
			//
			// Matched as the PAIR src/test rather than any directory called
			// "test", which would also skip a `test` package inside main
			// sources. Checking the parent's name keeps it to Gradle and Maven
			// source-set layout, and still works for a multi-module project
			// where the path is <module>/src/test.
			//
			// testFixtures is Gradle's other non-production source set and is
			// excluded for the same reason.
			if d.Name() == "test" || d.Name() == "testFixtures" {
				if filepath.Base(filepath.Dir(path)) == "src" {
					return filepath.SkipDir
				}
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

		// Scan code, not prose. javaCodeOnly blanks comments, strings, text
		// blocks and character literals while preserving byte offsets, so the
		// line and column reported below still point at the right place.
		//
		// This replaces a prefix test -- skip a line whose first non-space is
		// "//", "*" or "/*" -- which covered the easy case and missed a comment
		// AFTER code, a string literal, and a block-comment interior whose line
		// does not begin with "*". All three named a forbidden spelling without
		// using it, and since #1791 each refused a build. cleat#1820.
		lines := strings.Split(string(javaCodeOnly(data)), "\n")
		for lineIdx, line := range lines {
			lineNum := lineIdx + 1 // 1-based
			trimmed := strings.TrimSpace(line)

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
			Suggestion: "Add 'import io.cleat.sdk.CleatEntry;' and annotate a public method with '@CleatEntry', e.g.:\n    @CleatEntry\n    public String myWorkflow(HostCalls h, String input) { ... }",
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
