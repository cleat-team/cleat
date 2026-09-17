package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// forbiddenRustPatterns lists Rust APIs that are not allowed in workflow code.
var forbiddenRustPatterns = []struct {
	pattern    string
	code       string
	message    string
	suggestion string
}{
	{`use std::fs`, "R001", "filesystem access is non-deterministic across replays (file contents differ between runs)", "Use h.DurableCall() to interact with external storage"},
	{`std::fs::`, "R001", "filesystem access is non-deterministic across replays (file contents differ between runs)", "Use h.DurableCall() to interact with external storage"},
	{`use std::net`, "R002", "network access is non-deterministic across replays (network conditions differ between runs)", "Use h.DurableCall() to communicate with external services"},
	{`std::net::`, "R002", "network access is non-deterministic across replays (network conditions differ between runs)", "Use h.DurableCall() to communicate with external services"},
	{`use std::process`, "R003", "process spawning is non-deterministic across replays (OS process state differs between runs)", "Use h.DurableCall() for side effects"},
	{`std::process::Command`, "R003", "process spawning is non-deterministic across replays (OS process state differs between runs)", "Use h.DurableCall() for side effects"},
	{`use rand`, "R004", "non-deterministic random number generation is not allowed", "Use h.Random() for deterministic randomness"},
	{`rand::`, "R004", "non-deterministic random number generation is not allowed", "Use h.Random() for deterministic randomness"},
	{`std::time::SystemTime::now`, "R005", "wall-clock time is non-deterministic across replays", "Use h.Now() for deterministic time"},
	{`std::time::Instant::now`, "R005", "wall-clock time is non-deterministic across replays", "Use h.Now() for deterministic time"},
	{`use std::thread`, "R006", "threading is non-deterministic across replays (thread scheduling differs between runs)", "Workflow code is single-threaded by design"},
	{`std::thread::`, "R006", "threading is non-deterministic across replays (thread scheduling differs between runs)", "Workflow code is single-threaded by design"},
	{`use std::sync`, "R007", "synchronization primitives are non-deterministic across replays", "Workflow code is single-threaded by design"},
	{`std::sync::`, "R007", "synchronization primitives are non-deterministic across replays", "Workflow code is single-threaded by design"},
	{`use std::time::Duration`, "", "", ""}, // Allowed — used for h.DurableSleep()
}

// rustCodeOnly returns src with every comment and every string or character
// literal replaced by spaces, byte offsets and line endings untouched.
//
// BLANKING, NOT DELETING, because the caller reports 1-based line and column
// numbers straight out of this result. A filter that shortened anything would
// move every position it reports, and the position is most of what a vet
// finding is worth.
//
// WHY NOT vet_java.go's PREFIX TEST. The sibling checker skips a line whose
// first non-space is "//", "*" or "/*" (vet_java.go:118-123). That is the right
// shape for the common case and misses two others in the same file: a comment
// AFTER code on the same line, and a string literal. Both are lines that name a
// forbidden spelling without using it. Doing it with a scanner costs one
// function and covers all three.
//
// WHAT IT MODELS, since a lexer for a language this size is a claim worth
// bounding:
//
//   - line comments, including the doc forms /// and //!
//   - block comments, WHICH NEST IN RUST -- /* /* */ */ is one comment, and a
//     depth counter is the difference between blanking it and blanking half of
//     it and then treating live code as a comment
//   - "..." with backslash escapes, and b"..."
//   - raw strings r"...", r#"..."#, r##"..."## -- no escapes, and the closing
//     delimiter must match the opening hash count
//   - character literals, 'a' and '\n'
//
// THE ONE AMBIGUITY IS THE APOSTROPHE, and it is resolved in the safe
// direction. `'a` opens a lifetime in `&'a str` and a character literal in
// `'a'`; they differ only in what follows. A quote is treated as a character
// literal when the next byte is a backslash (an escape) or the byte after next
// closes it, and as a lifetime otherwise. A multi-byte character literal such
// as 'e-acute' therefore reads as a lifetime and is NOT blanked -- which leaves
// it visible to the scan, the over-reporting direction. A missed blank can
// produce a spurious finding someone will investigate; a wrong blank hides a
// real one.
func rustCodeOnly(src []byte) []byte {
	out := make([]byte, len(src))
	copy(out, src)
	blank := func(i int) {
		if out[i] != '\n' {
			out[i] = ' '
		}
	}
	n := len(src)
	for i := 0; i < n; {
		c := src[i]
		switch {
		case c == '/' && i+1 < n && src[i+1] == '/':
			for ; i < n && src[i] != '\n'; i++ {
				blank(i)
			}
		case c == '/' && i+1 < n && src[i+1] == '*':
			depth := 0
			for i < n {
				if src[i] == '/' && i+1 < n && src[i+1] == '*' {
					depth++
					blank(i)
					blank(i + 1)
					i += 2
					continue
				}
				if src[i] == '*' && i+1 < n && src[i+1] == '/' {
					depth--
					blank(i)
					blank(i + 1)
					i += 2
					if depth == 0 {
						break
					}
					continue
				}
				blank(i)
				i++
			}
		case c == 'r' && i+1 < n && (src[i+1] == '"' || src[i+1] == '#'):
			j := i + 1
			hashes := 0
			for j < n && src[j] == '#' {
				hashes++
				j++
			}
			if j >= n || src[j] != '"' {
				i++ // an identifier beginning with r, not a raw string
				continue
			}
			for k := i; k <= j; k++ {
				blank(k)
			}
			j++
			for j < n {
				if src[j] == '"' {
					closing := 0
					for j+1+closing < n && closing < hashes && src[j+1+closing] == '#' {
						closing++
					}
					if closing == hashes {
						for k := j; k <= j+hashes; k++ {
							blank(k)
						}
						j += hashes + 1
						break
					}
				}
				blank(j)
				j++
			}
			i = j
		case c == '"':
			blank(i)
			i++
			for i < n {
				if src[i] == '\\' && i+1 < n {
					blank(i)
					blank(i + 1)
					i += 2
					continue
				}
				closes := src[i] == '"'
				blank(i)
				i++
				if closes {
					break
				}
			}
		case c == '\'':
			// A lifetime or a character literal; see the note above.
			isChar := (i+1 < n && src[i+1] == '\\') || (i+2 < n && src[i+2] == '\'')
			if !isChar {
				i++
				continue
			}
			blank(i)
			i++
			for i < n {
				if src[i] == '\\' && i+1 < n {
					blank(i)
					blank(i + 1)
					i += 2
					continue
				}
				closes := src[i] == '\''
				blank(i)
				i++
				if closes {
					break
				}
			}
		default:
			i++
		}
	}
	return out
}

// withoutCfgTest blanks every item annotated #[cfg(test)], byte offsets and
// line endings untouched.
//
// WHY A SECOND PASS AND NOT A DIRECTORY RULE. Skipping <crate>/tests handles
// Cargo's integration-test target. It does nothing for the commoner form --
// a unit-test module inside the file the gate MUST scan:
//
//	#[cfg(test)]
//	mod tests {
//	    use std::fs;                       // <- reported, before this
//	    #[test] fn reads_a_fixture() { ... }
//	}
//
// Measured on develop (cleat#1789): a probe crate with an ordinary integration
// test, a bench, and a cfg(test) module produced three errors from three files,
// none of which reach the cdylib. examples/rust-workflow has such a module and
// vets clean only because it happens to use nothing forbidden.
//
// EXPECTS ALREADY-BLANKED INPUT. It counts braces, so it must run after
// rustCodeOnly or a brace inside a comment or a string closes the wrong item.
// The call site composes them in that order for this reason.
//
// WHAT IT MATCHES: #[cfg(test)] and #[cfg(all(test, ...))] -- any attribute
// whose text contains "cfg(" and the word test -- followed by an item with a
// body. It blanks from the attribute to the item's closing brace.
//
// WHAT IT DOES NOT: a bodyless item (`#[cfg(test)] mod tests;`, a file module)
// is left alone, because there is no brace to match and the file it names is
// scanned on its own. That is the over-reporting direction.
func withoutCfgTest(src []byte) []byte {
	out := make([]byte, len(src))
	copy(out, src)
	attr := regexp.MustCompile(`#\[\s*cfg\s*\([^\]]*\btest\b[^\]]*\)\s*\]`)
	for _, loc := range attr.FindAllIndex(src, -1) {
		// Find the item's opening brace, refusing to cross a `;` -- a bodyless
		// item ends there and the next `{` belongs to something else.
		open := -1
		for i := loc[1]; i < len(src); i++ {
			if src[i] == ';' {
				break
			}
			if src[i] == '{' {
				open = i
				break
			}
		}
		if open < 0 {
			continue
		}
		depth, end := 0, -1
		for i := open; i < len(src); i++ {
			switch src[i] {
			case '{':
				depth++
			case '}':
				depth--
				if depth == 0 {
					end = i
				}
			}
			if end >= 0 {
				break
			}
		}
		if end < 0 {
			continue
		}
		for i := loc[0]; i <= end; i++ {
			if out[i] != '\n' {
				out[i] = ' '
			}
		}
	}
	return out
}

// runVetRust performs static analysis on a Rust crate.
// Returns 0 on success (no errors), 1 if errors were found.
func runVetRust(crateDir string) int {
	// Validate the directory exists and has Cargo.toml.
	cargoToml := filepath.Join(crateDir, "Cargo.toml")
	if _, err := os.Stat(cargoToml); os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "Error: no Cargo.toml found in %s\n", crateDir)
		fmt.Fprintf(os.Stderr, "Rust crates require a Cargo.toml file.\n")
		os.Exit(1)
	}

	// Crate name for display.
	crateName := extractCrateName(cargoToml)
	fmt.Fprintf(os.Stderr, "Vetting Rust crate %q in %s...\n", crateName, crateDir)

	// Find all .rs files that can reach the artifact.
	var rsFiles []string
	err := filepath.WalkDir(crateDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // skip inaccessible
		}
		if d.IsDir() {
			// Skip the 'target' directory (build artifacts) and hidden dirs.
			if d.Name() == "target" || d.Name() == ".git" || strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			// And Cargo's test, bench and example TARGETS, which are compiled
			// separately and are never part of the cdylib this gate guards.
			// cleat#1789: reading a fixture file in a test is what tests are
			// FOR, so before this the first project with an integration test
			// stopped building, pointed at a file that is not in the artifact.
			//
			// ANCHORED AT THE CRATE ROOT, because Cargo's convention is
			// POSITIONAL: <crate>/tests, <crate>/benches, <crate>/examples. A
			// directory with one of those names anywhere else means nothing to
			// Cargo and is ordinary library code.
			//
			// Two cases a name-only rule gets wrong, both measured rather than
			// imagined -- the first draft of this comment justified the
			// anchoring with examples/rust-workflow, which is NOT one of them
			// (the walk root there is named rust-workflow, so a name-only rule
			// would have been fine):
			//
			//   src/tests/mod.rs   a library module that happens to be called
			//                      tests -- compiled into the cdylib, and its
			//                      violation is a real finding
			//   <crate> named tests  WalkDir's first callback is the root
			//                      itself, so a name-only rule skips the whole
			//                      crate and reports it clean having read none
			//                      of it
			if rel, relErr := filepath.Rel(crateDir, path); relErr == nil {
				switch rel {
				case "tests", "benches", "examples":
					return filepath.SkipDir
				}
			}
			return nil
		}
		if strings.HasSuffix(path, ".rs") {
			rsFiles = append(rsFiles, path)
		}
		return nil
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error scanning Rust source files: %v\n", err)
		os.Exit(1)
	}

	if len(rsFiles) == 0 {
		fmt.Fprintf(os.Stderr, "Error: no .rs source files found in %s\n", crateDir)
		os.Exit(1)
	}

	var output VetOutput
	output.Summary.Functions = len(rsFiles)

	// Scan each .rs file for forbidden patterns.
	for _, rsFile := range rsFiles {
		relPath, _ := filepath.Rel(crateDir, rsFile)
		data, err := os.ReadFile(rsFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: could not read %s: %v\n", rsFile, err)
			continue
		}

		// CODE ONLY. The scan below is `strings.Contains` over a line, and a
		// comment or a string literal is a line like any other, so prose
		// NAMING a forbidden spelling was reported as a use of it (cleat#1782).
		//
		// Not a hypothetical: the known-limit fixture in
		// testdata/vet-checks/rust/known_limit_grouped_use had to be rewritten
		// so that its explanation does not quote the pattern list, because the
		// first draft produced seven errors out of its own comment. Its header
		// still says "read the list there rather than here" for that reason.
		// Since #1784 this decides whether an artifact is emitted, so the same
		// comment now fails a BUILD.
		lines := strings.Split(string(withoutCfgTest(rustCodeOnly(data))), "\n")
		for lineIdx, line := range lines {
			lineNum := lineIdx + 1 // 1-based
			trimmed := strings.TrimSpace(line)

			for _, fb := range forbiddenRustPatterns {
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

					if fb.code != "" && fb.code[0] == 'R' && fb.suggestion != "" {
						// error implied by putting in Errors slice
						output.Errors = append(output.Errors, vr)
					} else {
						// warning implied by putting in Warnings slice
						output.Warnings = append(output.Warnings, vr)
					}
				}
			}
		}
	}

	// Check for #[cleat_entry] functions.
	var hasCleatEntry bool
	for _, rsFile := range rsFiles {
		data, err := os.ReadFile(rsFile)
		if err != nil {
			continue
		}
		if strings.Contains(string(data), "#[cleat_entry]") {
			hasCleatEntry = true
			break
		}
	}
	if !hasCleatEntry {
		output.Warnings = append(output.Warnings, VetResult{
			Code:       "R100",
			Message:    "no #[cleat_entry] attribute found in any source file",
			Suggestion: "Add 'use cleat_sdk::cleat_entry;' and '#[cleat_entry]' above a public function, e.g.:\n    #[cleat_entry]\n    pub fn my_workflow(h: &mut HostCalls, input: &str) -> Result<String, Error>",
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
