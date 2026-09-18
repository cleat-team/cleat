package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

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

// forbiddenRustPaths lists MODULE PATHS a workflow may not reach, replacing the
// literal-spelling table this checker shipped with. cleat#1811.
//
// The difference is not cosmetic. A spelling table asks "does this text appear";
// a path table asks "where does this name resolve to", and idiomatic Rust
// writes the same reach in spellings the first question cannot see:
//
//	use std::{fs, net};               // grouped -- no literal matched
//	use std::time::SystemTime as ST;  // aliased -- the spelling never appears
//	ST::now()
//
// Neither is evasion; both are what rustfmt produces. See
// docs/contributor/design/rust-determinism-checker.md for why this is a
// hand-written resolver rather than tree-sitter -- briefly, the Go bindings are
// cgo and cmd/cleat is deliberately pure-Go cross-compilable.
//
// A prefix matches a resolved path P when P == prefix or P starts with
// prefix + "::". std::time::Duration is therefore allowed without needing a row
// to say so: it is not under either SystemTime::now or Instant::now. The old
// table carried an explicit empty-code row for it, which is gone.
var forbiddenRustPaths = []struct {
	prefix     string
	code       string
	message    string
	suggestion string
}{
	{"std::fs", "R001", "filesystem access is non-deterministic across replays (file contents differ between runs)", "Use h.DurableCall() to interact with external storage"},
	{"std::net", "R002", "network access is non-deterministic across replays (network conditions differ between runs)", "Use h.DurableCall() to communicate with external services"},
	{"std::process", "R003", "process spawning is non-deterministic across replays (OS process state differs between runs)", "Use h.DurableCall() for side effects"},
	{"rand", "R004", "non-deterministic random number generation is not allowed", "Use h.Random() for deterministic randomness"},
	{"std::time::SystemTime::now", "R005", "wall-clock time is non-deterministic across replays", "Use h.Now() for deterministic time"},
	{"std::time::Instant::now", "R005", "wall-clock time is non-deterministic across replays", "Use h.Now() for deterministic time"},
	{"std::thread", "R006", "threading is non-deterministic across replays (thread scheduling differs between runs)", "Workflow code is single-threaded by design"},
	{"std::sync", "R007", "synchronization primitives are non-deterministic across replays", "Workflow code is single-threaded by design"},
}

type rustFinding struct {
	line, col                 int
	code, message, suggestion string
}

var (
	rustUseRe  = regexp.MustCompile(`(?s)\buse\s+([^;]+);`)
	rustPathRe = regexp.MustCompile(`\b([A-Za-z_][A-Za-z0-9_]*)((?:::[A-Za-z_][A-Za-z0-9_]*)+)`)
)

// expandRustUse turns one `use` declaration's body into local name -> full path,
// flattening nested groups and honouring `as` aliases and `self`.
//
// Recursive on the GROUP, not on the path, because `use a::{b::{c, d}, e}` nests
// arbitrarily and the prefix accumulates down each arm. Splitting on "," at
// depth 0 is what keeps the arms apart; a naive strings.Split eats the inner
// commas and produces `a::b::{c` as a path.
func expandRustUse(body string, out map[string]string) {
	body = strings.TrimSpace(body)
	if body == "" {
		return
	}
	if i := strings.Index(body, "{"); i >= 0 {
		prefix := strings.TrimSuffix(strings.TrimSpace(body[:i]), "::")
		depth, start := 0, i+1
		for j := i + 1; j < len(body); j++ {
			switch body[j] {
			case '{':
				depth++
			case '}':
				if depth == 0 {
					expandRustUse(joinRustPath(prefix, body[start:j]), out)
					return
				}
				depth--
			case ',':
				if depth == 0 {
					expandRustUse(joinRustPath(prefix, body[start:j]), out)
					start = j + 1
				}
			}
		}
		return
	}
	full := strings.TrimSpace(body)
	local := ""
	if k := strings.Index(full, " as "); k >= 0 {
		local = strings.TrimSpace(full[k+4:])
		full = strings.TrimSpace(full[:k])
	}
	// `use std::io::{self, Write}` brings the MODULE in as `io`.
	full = strings.TrimSuffix(full, "::self")
	if full == "" {
		return
	}
	if local == "" {
		parts := strings.Split(full, "::")
		local = parts[len(parts)-1]
	}
	if local == "*" || local == "" {
		// A glob import names nothing locally, so nothing can be resolved
		// through it. Recorded as a known limit rather than guessed at.
		return
	}
	out[local] = full
}

func joinRustPath(prefix, seg string) string {
	seg = strings.TrimSpace(seg)
	if seg == "" {
		return ""
	}
	if prefix == "" {
		return seg
	}
	return prefix + "::" + seg
}

// matchForbiddenRustPath reports the row a resolved path falls under.
func matchForbiddenRustPath(path string) (int, bool) {
	for i, f := range forbiddenRustPaths {
		if path == f.prefix || strings.HasPrefix(path, f.prefix+"::") {
			return i, true
		}
	}
	return 0, false
}

// findForbiddenRustPaths resolves every path expression in already-blanked
// source and reports the ones that reach a forbidden module.
//
// TWO SITES PER REACH, DELIBERATELY. An import is reported where it is written
// and a call where it is called, because they are different things to fix: one
// is a dependency the crate declares, the other a line that runs. Reporting
// only the call would leave `use std::fs;` -- which the old table flagged --
// silent, and reporting only the import would miss a fully-qualified
// `std::fs::read()` written with no import at all.
//
// DEDUPLICATED BY (code, resolved path, line) so that a line naming the same
// reach twice does not print twice, which the old table did whenever two of its
// spellings overlapped -- `use std::fs` and `std::fs::` both matched
// `use std::fs::File;`.
func findForbiddenRustPaths(code []byte) []rustFinding {
	src := string(code)

	aliases := map[string]string{}
	for _, m := range rustUseRe.FindAllStringSubmatch(src, -1) {
		expandRustUse(m[1], aliases)
	}

	var out []rustFinding
	seen := map[string]bool{}
	add := func(line, col, idx int, resolved, why string) {
		f := forbiddenRustPaths[idx]
		key := fmt.Sprintf("%s|%s|%d", f.code, resolved, line)
		if seen[key] {
			return
		}
		seen[key] = true
		msg := f.message
		if why != "" {
			msg = f.message + " " + why
		}
		out = append(out, rustFinding{
			line: line, col: col,
			code: f.code, message: msg, suggestion: f.suggestion,
		})
	}

	lines := strings.Split(src, "\n")

	// Imports, at the line the `use` is written on.
	for _, loc := range rustUseRe.FindAllStringSubmatchIndex(src, -1) {
		one := map[string]string{}
		expandRustUse(src[loc[2]:loc[3]], one)
		line := 1 + strings.Count(src[:loc[0]], "\n")
		col := loc[0] - strings.LastIndex(src[:loc[0]], "\n")
		for local, full := range one {
			if idx, ok := matchForbiddenRustPath(full); ok {
				// ALWAYS NAME THE RESOLVED PATH. A finding that says only
				// "R001 here" is actionable because the line is in front of
				// you; one that says which module it reached is actionable
				// from a CI log. The three forms differ in what else the
				// reader needs: an alias and a grouped arm do not appear in
				// the source as the path that matched.
				why := fmt.Sprintf("(reaches %s)", full)
				if local != lastRustSegment(full) {
					why = fmt.Sprintf("(imported as %q, which resolves to %s)", local, full)
				} else if !strings.Contains(src[loc[0]:loc[1]], full) {
					why = fmt.Sprintf("(a grouped import; this arm resolves to %s)", full)
				}
				add(line, col, idx, full, why)
			}
		}
	}

	// Call sites, resolved through the alias map.
	for lineIdx, line := range lines {
		for _, m := range rustPathRe.FindAllStringSubmatchIndex(line, -1) {
			head := line[m[2]:m[3]]
			rest := line[m[4]:m[5]]
			written := head + rest
			resolved := written
			if base, ok := aliases[head]; ok {
				resolved = base + rest
			}
			idx, ok := matchForbiddenRustPath(resolved)
			if !ok {
				continue
			}
			why := fmt.Sprintf("(reaches %s)", resolved)
			if resolved != written {
				why = fmt.Sprintf("(written %q, which resolves to %s)", written, resolved)
			}
			add(lineIdx+1, m[0]+1, idx, resolved, why)
		}
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].line != out[j].line {
			return out[i].line < out[j].line
		}
		return out[i].col < out[j].col
	})
	return out
}

func lastRustSegment(p string) string {
	parts := strings.Split(p, "::")
	return parts[len(parts)-1]
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
		code := withoutCfgTest(rustCodeOnly(data))
		for _, f := range findForbiddenRustPaths(code) {
			output.Errors = append(output.Errors, VetResult{
				Code:       f.code,
				File:       relPath,
				Line:       f.line,
				Column:     f.col,
				Message:    f.message,
				Suggestion: f.suggestion,
			})
		}
		// R101 goes to Warnings, not Errors, so it does not change the exit
		// code. See findRustHashIteration for why this one is not a build
		// failure when Go's E021 is.
		for _, f := range findRustHashIteration(code) {
			output.Warnings = append(output.Warnings, VetResult{
				Code:       f.code,
				File:       relPath,
				Line:       f.line,
				Column:     f.col,
				Message:    f.message,
				Suggestion: f.suggestion,
			})
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

// R101 -- hash-container iteration. A WARNING, and the severity is the whole
// design of this rule; see the block above findRustHashIteration.
const rustHashIterCode = "R101"

const rustHashIterMessage = "iteration order of a HashMap/HashSet is unspecified in Rust, so this is " +
	"deterministic only because cleat intercepts the entropy WASI hands the guest"

const rustHashIterSuggestion = "Use BTreeMap/BTreeSet, whose iteration order is sorted and guaranteed by " +
	"the API. If the order genuinely does not matter -- a sum, a count, a collect into another map -- " +
	"this warning is noise and can be ignored; it cannot tell those apart."

// rustHashContainers are the std types whose iteration order is randomised.
// BTreeMap and BTreeSet are deliberately absent: their order is sorted and
// guaranteed, and they are what the suggestion points at.
var rustHashContainers = map[string]bool{"HashMap": true, "HashSet": true}

// rustIterMethods are the methods that expose iteration ORDER. Deliberately not
// every method that touches the map: get, insert, contains_key, len, remove and
// entry are all order-independent and must stay silent, because a rule that
// fires on using a HashMap at all is a rule about the type rather than about
// determinism, and would be argued with rather than fixed.
var rustIterMethods = map[string]bool{
	"iter": true, "iter_mut": true, "into_iter": true,
	"keys": true, "values": true, "values_mut": true,
	"drain": true,
}

var (
	// `let m: HashMap<..>` / `let mut m: HashMap<..>`, and the same for a fn
	// parameter `m: &HashMap<..>`. One expression covers both because the
	// binding-then-type shape is identical.
	rustTypedBindingRe = regexp.MustCompile(`\b(?:let\s+(?:mut\s+)?)?([A-Za-z_][A-Za-z0-9_]*)\s*:\s*&?(?:mut\s+)?([A-Za-z_][A-Za-z0-9_:]*)\s*<`)

	// `let m = HashMap::new()`, `HashMap::with_capacity(n)`, `HashMap::from(..)`.
	rustCtorBindingRe = regexp.MustCompile(`\blet\s+(?:mut\s+)?([A-Za-z_][A-Za-z0-9_]*)\s*(?::[^=]*)?=\s*([A-Za-z_][A-Za-z0-9_:]*)\s*::\s*(?:new|with_capacity|with_hasher|with_capacity_and_hasher|from|from_iter)\b`)

	// `for PAT in &m {` -- the loop form, which names no method at all.
	rustForInRe = regexp.MustCompile(`\bfor\s+[^\n]*?\bin\s+&?(?:mut\s+)?([A-Za-z_][A-Za-z0-9_]*)\s*(?:\{|\.into_iter\b|\.iter\b)`)

	// `m.iter()`, `m.keys()`, ...
	rustMethodCallRe = regexp.MustCompile(`\b([A-Za-z_][A-Za-z0-9_]*)\s*\.\s*([a-z_]+)\s*\(`)
)

// resolvesToHashContainer reports whether a type token names HashMap or HashSet
// once `use` aliases are applied.
//
// Through the SAME alias map the path rules use, so `use std::collections::
// HashMap as HM;` is seen and a local `struct HashMap` shadowing the std one is
// not mistaken for it -- the latter has no alias entry and no std path, so it
// resolves to a bare name that is only treated as std if it IS the std spelling
// and nothing else claimed it. That last case is a known hole, recorded in the
// limitations list rather than papered over.
func resolvesToHashContainer(tok string, aliases map[string]string) bool {
	if full, ok := aliases[tok]; ok {
		tok = full
	}
	if i := strings.LastIndex(tok, "::"); i >= 0 {
		// A fully-qualified std::collections::HashMap<..> written inline.
		if !strings.HasPrefix(tok, "std::collections") && !strings.HasPrefix(tok, "collections") {
			// Some other module's type that merely ends in the same name.
			if !strings.HasPrefix(tok, "HashMap") && !strings.HasPrefix(tok, "HashSet") {
				return false
			}
		}
		tok = tok[i+2:]
	}
	return rustHashContainers[tok]
}

// findRustHashIteration reports every place a HashMap or HashSet is ITERATED.
//
// A WARNING RATHER THAN AN ERROR, and the reason is not timidity. Under cleat
// this code is deterministic today: WASI's random_get is intercepted
// (engine/wasi_policy.go) and served from the same replay-stable source
// cleat_random uses (engine/lifecycle.go Random, sha256 over workflow id, step
// and sequence), so Rust's RandomState seeds identically on every replay and
// iteration order reproduces. The divergence this rule sounds like it is
// preventing does not currently happen.
//
// WHAT IT IS ACTUALLY FOR is the fragility of that arrangement. The property
// holds because of an invariant in a DIFFERENT SUBSYSTEM, which nothing in this
// file knows about and no test here pins. A Component Model migration to
// wasi:random, a backend that forwards random_get instead of intercepting it,
// or a std that stops seeding through WASI, each turn idiomatic code into a
// replay divergence with no diagnostic anywhere. Saying so at build time is
// cheap; discovering it from a diverged history is not.
//
// GO'S E021 IS AN ERROR AND THIS IS A WARNING, which is a deliberate asymmetry
// and is called out because the opposite mistake has already been made here:
// docs/troubleshooting.md and docs/workflow-go-constraints.md agreed for months
// that map iteration was a WARNING under a code the analyzer has never emitted,
// while it actually failed the build with E021, and
// cmd/cleat/documented_flags_and_codes_test.go exists because of it. The code is
// deliberately not named: citing a diagnostic nothing emits is the thing that
// guard refuses, and the first draft of THIS comment's doc-side twin tripped it. That was a document contradicting the code. This is the code choosing,
// for a stated reason: E021 predates the interception and hardening it costs
// nothing, whereas making Rust fail the build would refuse idiomatic code that
// works today and cannot be rewritten without changing the data structure.
//
// WHAT IT CANNOT SEE, stated rather than left for the next reader:
//
//   - a map reached through a STRUCT FIELD (self.counts.iter()) or returned
//     from a call (make_map().iter()) -- the binding is not declared in a shape
//     this matches, so it is silent;
//   - a map in ANOTHER FILE, since the survey is per file, like every other
//     rule here;
//   - a local type named HashMap that shadows the std one, which is reported
//     even though it may iterate in a defined order;
//   - ORDER-INDEPENDENT consumers. m.values().sum(), .count(), .max(), and a
//     collect() into another HashMap are all deterministic regardless of order
//     and are reported anyway. Distinguishing them needs the type of the
//     consumer, which this checker does not have. It is the main source of
//     noise and the main reason this is not an error.
func findRustHashIteration(code []byte) []rustFinding {
	src := string(code)

	aliases := map[string]string{}
	for _, m := range rustUseRe.FindAllStringSubmatch(src, -1) {
		expandRustUse(m[1], aliases)
	}

	// Pass one: which local names hold a hash container.
	hashBindings := map[string]bool{}
	for _, m := range rustTypedBindingRe.FindAllStringSubmatch(src, -1) {
		if resolvesToHashContainer(m[2], aliases) {
			hashBindings[m[1]] = true
		}
	}
	for _, m := range rustCtorBindingRe.FindAllStringSubmatch(src, -1) {
		if resolvesToHashContainer(m[2], aliases) {
			hashBindings[m[1]] = true
		}
	}
	if len(hashBindings) == 0 {
		return nil
	}

	var out []rustFinding
	seen := map[int]bool{}
	add := func(off int) {
		line := 1 + strings.Count(src[:off], "\n")
		if seen[line] {
			return
		}
		seen[line] = true
		out = append(out, rustFinding{
			line:       line,
			col:        off - strings.LastIndex(src[:off], "\n"),
			code:       rustHashIterCode,
			message:    rustHashIterMessage,
			suggestion: rustHashIterSuggestion,
		})
	}

	// Pass two: iteration over one of them.
	for _, loc := range rustForInRe.FindAllStringSubmatchIndex(src, -1) {
		if hashBindings[src[loc[2]:loc[3]]] {
			add(loc[0])
		}
	}
	for _, loc := range rustMethodCallRe.FindAllStringSubmatchIndex(src, -1) {
		if hashBindings[src[loc[2]:loc[3]]] && rustIterMethods[src[loc[4]:loc[5]]] {
			add(loc[0])
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].line < out[j].line })
	return out
}
