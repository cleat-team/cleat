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

// forbiddenJavaPaths lists RESOLVED Java paths a workflow may not reach,
// replacing the literal-spelling table this checker shipped with. cleat#1812.
//
// The difference is not cosmetic. A spelling table asks "does this text
// appear"; a path table asks "where does this name resolve to", and ordinary
// Java writes the same reach in spellings the first question cannot see:
//
//	import static java.lang.System.currentTimeMillis;
//	currentTimeMillis()                    // no "System." anywhere
//
//	var clock = java.time.Clock.systemUTC();  // fully qualified, no import
//
// Neither is evasion; both are what an IDE's auto-import or a style guide
// produces. See docs/contributor/design/java-determinism-checker.md for why
// this is a hand-written resolver rather than tree-sitter or bytecode --
// briefly, the Go bindings for tree-sitter-java are cgo and cmd/cleat is
// deliberately pure-Go cross-compilable, and bytecode analysis is staged for
// later because it costs the toolchain-free `cleat vet` path that a resolver
// does not.
//
// A prefix matches a resolved path P when P == prefix or P starts with
// prefix + ".". ENTRIES ARE CHECKED IN ORDER, and the first match wins -- so a
// specific class (java.io.File, J013) is listed before the general package it
// lives in (java.io, J004), letting the specific entry keep its more precise
// message while the general one still catches every OTHER class under that
// package, the same net the old import-substring pattern cast. `except`
// exempts named classes from a package-level entry: java.io.ByteArrayInputStream
// and its siblings are pure, in-memory, and were only ever caught because
// "InputStream" and "new java.io." are both substrings of their names -- the
// false positive this table exists to remove. See the design doc for the
// measurement.
//
// THREAD IS DELIBERATELY NOT HERE. `Thread.sleep` and `new Thread(` are the
// only two forms the old table forbade; a class-level entry for
// "java.lang.Thread" would also refuse `Thread.currentThread()` and a plain
// `Thread t` declaration, neither of which was ever forbidden. Both forms are
// matched separately, below the table, by findForbiddenJavaPaths.
var forbiddenJavaPaths = []struct {
	prefix     string
	code       string
	message    string
	suggestion string
	except     []string
}{
	{"java.lang.System.currentTimeMillis", "J001", "wall-clock time is non-deterministic across replays", "Use h.Now() for deterministic time", nil},
	{"java.lang.Math.random", "J002", "non-deterministic random number generation is not allowed", "Use h.Random() for deterministic randomness", nil},
	{"java.util.Random", "J002", "non-deterministic random number generation is not allowed", "Use h.Random() for deterministic randomness", nil},
	{"java.lang.Thread.sleep", "J003", "thread sleeping is non-deterministic across replays", "Use h.DurableSleep() for deterministic timers", nil},
	{"java.io.File", "J013", "filesystem access is non-deterministic across replays (file contents differ between runs)", "Use h.DurableCall() to interact with external storage", nil},
	{"java.io.FileReader", "J013", "filesystem access is non-deterministic across replays (file contents differ between runs)", "Use h.DurableCall() to interact with external storage", nil},
	{"java.io.FileWriter", "J013", "filesystem access is non-deterministic across replays (file contents differ between runs)", "Use h.DurableCall() to interact with external storage", nil},
	{"java.io.FileInputStream", "J013", "filesystem access is non-deterministic across replays (file contents differ between runs)", "Use h.DurableCall() to interact with external storage", nil},
	{"java.io.FileOutputStream", "J013", "filesystem access is non-deterministic across replays (file contents differ between runs)", "Use h.DurableCall() to interact with external storage", nil},
	{"java.io.InputStream", "J016", "I/O stream usage produces non-replayable side effects (stream state differs across replays)", "Use h.DurableCall() to interact with external services", nil},
	{"java.io.OutputStream", "J016", "I/O stream usage produces non-replayable side effects (stream state differs across replays)", "Use h.DurableCall() to interact with external services", nil},
	{"java.io.ObjectInputStream", "J016", "I/O stream usage produces non-replayable side effects (stream state differs across replays)", "Use h.DurableCall() to interact with external services", nil},
	{"java.io.ObjectOutputStream", "J016", "I/O stream usage produces non-replayable side effects (stream state differs across replays)", "Use h.DurableCall() to interact with external services", nil},
	// Everything else under java.io -- BufferedReader, PrintWriter,
	// RandomAccessFile, DataInputStream and the rest -- falls through to this
	// general entry, the same reach the old `import java.io.` pattern had.
	//
	// THE EXCEPTIONS ARE TWO KINDS, found by running this checker against
	// crates/cleat-java's own annotation processor (build-time codegen, never
	// workflow code, and a useful stress corpus for exactly this reason).
	// ByteArrayInputStream and its three siblings are pure in-memory adapters:
	// no filesystem, no stream tied to an OS resource, fully replayable.
	// IOException and its siblings are the second kind and a DIFFERENT
	// argument: a `catch (IOException e)` or `throws IOException` is a type
	// reference, not an I/O operation -- it names what MIGHT be thrown by a
	// call this table already catches on its own. Measured: without this
	// second group, CleatEntryProcessor.java went from the old checker's 2
	// findings (its two `import java.io.` lines) to 11 once every USE of the
	// imported names was resolved -- 9 of the 11 were `IOException` at a
	// catch or throws site. The resolver making every reach visible is the
	// point; making a non-hazard visible nine times over is not.
	{"java.io", "J004", "I/O operations produce non-replayable side effects (file contents differ across replays)", "Use h.DurableCall() to interact with external services",
		[]string{
			"java.io.ByteArrayInputStream", "java.io.ByteArrayOutputStream", "java.io.StringReader", "java.io.StringWriter",
			"java.io.IOException", "java.io.FileNotFoundException", "java.io.UncheckedIOException",
			"java.io.EOFException", "java.io.InterruptedIOException", "java.io.NotSerializableException",
		}},
	{"java.net.Socket", "J014", "network access is non-deterministic across replays (network conditions differ between runs)", "Use h.DurableCall() to communicate with external services", nil},
	{"java.net.ServerSocket", "J014", "network access is non-deterministic across replays (network conditions differ between runs)", "Use h.DurableCall() to communicate with external services", nil},
	// Same reasoning as java.io's exception group, above.
	{"java.net", "J005", "network access is non-deterministic across replays (network conditions differ between runs)", "Use h.DurableCall() to communicate with external services",
		[]string{
			"java.net.UnknownHostException", "java.net.MalformedURLException", "java.net.SocketException",
			"java.net.SocketTimeoutException", "java.net.URISyntaxException", "java.net.ConnectException",
		}},
	{"java.sql.Connection", "J015", "database access produces non-replayable side effects (database state differs across replays)", "Use h.DurableCall() to interact with databases", nil},
	// Same reasoning again: SQLException is what a database call MIGHT throw,
	// not a database call.
	{"java.sql", "J006", "database access produces non-replayable side effects (database state differs across replays)", "Use h.DurableCall() to interact with databases",
		[]string{"java.sql.SQLException"}},
	// Not a package-wide java.time ban: Duration and Period are pure value
	// types with no wall-clock read in their constructors, and the old
	// `import java.time.` pattern banning them was never a deliberate choice,
	// just a side effect of matching the whole package. Only the two
	// "what time is it right now" entry points are named.
	{"java.time.Clock", "J007", "wall-clock time is non-deterministic across replays", "Use h.Now() for deterministic time", nil},
	{"java.time.Instant", "J007", "wall-clock time is non-deterministic across replays", "Use h.Now() for deterministic time", nil},
	{"java.util.Timer", "J012", "timers are non-deterministic across replays", "Use h.DurableSleep() for deterministic timers", nil},
	{"java.lang.Runtime.getRuntime", "J017", "runtime execution is non-deterministic across replays (OS process state differs between runs)", "Use h.DurableCall() for side effects", nil},
	{"java.lang.ProcessBuilder", "J018", "process spawning is non-deterministic across replays (process behavior differs between runs)", "Use h.DurableCall() for side effects", nil},
	{"java.util.concurrent", "J019", "concurrent execution is non-deterministic across replays (thread scheduling differs between runs)", "Workflow code is single-threaded by design", nil},
	// Reflection is undecidable for static analysis -- Class.forName with a
	// computed name cannot be resolved by any resolver, bytecode included --
	// so the rule is to forbid the entry point outright rather than imply
	// coverage that does not exist. See the design doc's "stated limits".
	{"java.lang.Class.forName", "J020", "reflection defeats static determinism analysis and may reach non-deterministic operations", "Avoid Class.forName in workflow code; use h.DurableCall() if dynamic dispatch is required", nil},
}

// javaImplicitLangTypes are the java.lang simple names this checker resolves
// WITHOUT an import, because java.lang.* is implicitly imported into every
// Java compilation unit by the language spec itself -- an import statement
// for any of these would be redundant and no real project writes one.
var javaImplicitLangTypes = map[string]string{
	"System":         "java.lang.System",
	"Math":           "java.lang.Math",
	"Thread":         "java.lang.Thread",
	"Class":          "java.lang.Class",
	"Runtime":        "java.lang.Runtime",
	"ProcessBuilder": "java.lang.ProcessBuilder",
}

// javaKnownForbiddenTypes maps the SIMPLE name of every class-shaped entry in
// forbiddenJavaPaths to its fully qualified name. It exists only to resolve a
// wildcard import (`import java.io.*;`) against the classes this checker
// knows about -- there is no JDK class index here, so a wildcard resolves a
// name in this map and nothing else. That is a stated limit, not a bug: a
// class reached only through a wildcard import, and not in this map, is
// simply invisible -- the conservative (under-report, never misreport)
// direction. The wildcard IMPORT LINE ITSELF is still caught, by the general
// package-prefix entries above matching the bare package name.
var javaKnownForbiddenTypes = map[string]string{
	"File":               "java.io.File",
	"FileReader":         "java.io.FileReader",
	"FileWriter":         "java.io.FileWriter",
	"FileInputStream":    "java.io.FileInputStream",
	"FileOutputStream":   "java.io.FileOutputStream",
	"InputStream":        "java.io.InputStream",
	"OutputStream":       "java.io.OutputStream",
	"ObjectInputStream":  "java.io.ObjectInputStream",
	"ObjectOutputStream": "java.io.ObjectOutputStream",
	"Socket":             "java.net.Socket",
	"ServerSocket":       "java.net.ServerSocket",
	"Connection":         "java.sql.Connection",
	"Random":             "java.util.Random",
	"Timer":              "java.util.Timer",
	"Clock":              "java.time.Clock",
	"Instant":            "java.time.Instant",
}

type javaFinding struct {
	line, col                 int
	code, message, suggestion string
}

var (
	// The path group allows a trailing "*" -- a wildcard import -- as well as
	// an ordinary dotted name. Dropping it here would silently un-match every
	// wildcard import line rather than matching it with an empty capture;
	// caught by TestFindForbiddenJavaPathsSeesWhatSpellingsMissed, whose
	// wildcard case reported the import line but not the class it resolved.
	javaImportRe    = regexp.MustCompile(`(?m)^\s*import\s+(static\s+)?([A-Za-z_][A-Za-z0-9_.]*(?:\.\*)?)\s*;`)
	javaPathRe      = regexp.MustCompile(`\b([A-Za-z_][A-Za-z0-9_]*)((?:\.[A-Za-z_][A-Za-z0-9_]*)+)`)
	javaBareCallRe  = regexp.MustCompile(`\b([A-Za-z_][A-Za-z0-9_]*)\s*\(`)
	javaNewThreadRe = regexp.MustCompile(`\bnew\s+Thread\s*\(`)
)

// matchForbiddenJavaPath reports the row a resolved path falls under, honouring
// each row's `except` list. Entries are checked in table order and the first
// match (that is not excepted) wins.
func matchForbiddenJavaPath(path string) (int, bool) {
	for i, f := range forbiddenJavaPaths {
		if path != f.prefix && !strings.HasPrefix(path, f.prefix+".") {
			continue
		}
		excepted := false
		for _, e := range f.except {
			if path == e {
				excepted = true
				break
			}
		}
		if !excepted {
			return i, true
		}
	}
	return 0, false
}

// findForbiddenJavaPaths resolves every path expression in already-blanked
// source and reports the ones that reach a forbidden class or member.
//
// THREE INDEPENDENT PASSES, because Java's import forms are three different
// shapes and each needs its own resolution:
//
//  1. Dotted chains (java.time.Clock.systemUTC(), or System.currentTimeMillis()
//     once "System" resolves) -- an explicit class import, java.lang's
//     implicit set, or a fully-qualified spelling that resolves to itself.
//  2. Bare calls (currentTimeMillis()) -- only reachable through a static
//     import, since Java has no bare function calls otherwise.
//  3. Bare type names used as a declaration, a constructor argument list, or
//     a generic parameter (Connection conn, new Connection(), List<Connection>)
//     -- reachable through an explicit class import, a wildcard import
//     matching a KNOWN forbidden class, or java.lang's implicit set.
//
// DEDUPLICATED BY (code, resolved path, line): a line naming one reach twice
// does not print twice.
func findForbiddenJavaPaths(code []byte) []javaFinding {
	src := string(code)
	lines := strings.Split(src, "\n")

	classAliases := map[string]string{}
	for name, fqn := range javaImplicitLangTypes {
		classAliases[name] = fqn
	}
	staticAliases := map[string]string{}
	wildcardPackages := map[string]bool{}

	for _, m := range javaImportRe.FindAllStringSubmatch(src, -1) {
		isStatic := m[1] != ""
		path := m[2]
		if strings.HasSuffix(path, ".*") {
			pkg := strings.TrimSuffix(path, ".*")
			if isStatic {
				// A static wildcard (`import static java.lang.Math.*;`) cannot
				// be resolved without knowing every static member of the named
				// class, which this checker does not index. Stated limit,
				// same shape as the class wildcard below.
				continue
			}
			wildcardPackages[pkg] = true
			continue
		}
		parts := strings.Split(path, ".")
		simple := parts[len(parts)-1]
		if isStatic {
			staticAliases[simple] = path
		} else {
			classAliases[simple] = path
		}
	}
	// Resolve a wildcard import against the classes this checker knows about.
	for simple, fqn := range javaKnownForbiddenTypes {
		if _, already := classAliases[simple]; already {
			continue
		}
		pkg := fqn[:strings.LastIndex(fqn, ".")]
		if wildcardPackages[pkg] {
			classAliases[simple] = fqn
		}
	}

	// Precompute which aliased simple names are worth scanning for at all
	// (pass 3), and compile each name's boundary regex once rather than once
	// per line -- a file of any real size makes the difference between a
	// vet run and a hang.
	type typeCheck struct {
		re  *regexp.Regexp
		fqn string
	}
	var typeChecks []typeCheck
	for simple, fqn := range classAliases {
		if _, ok := matchForbiddenJavaPath(fqn); ok {
			typeChecks = append(typeChecks, typeCheck{
				re:  regexp.MustCompile(`\b` + regexp.QuoteMeta(simple) + `\b`),
				fqn: fqn,
			})
		}
	}

	var out []javaFinding
	seen := map[string]bool{}
	add := func(line, col, idx int, resolved, why string) {
		f := forbiddenJavaPaths[idx]
		key := fmt.Sprintf("%s|%s|%d", f.code, resolved, line)
		if seen[key] {
			return
		}
		seen[key] = true
		msg := f.message
		if why != "" {
			msg = f.message + " " + why
		}
		out = append(out, javaFinding{line: line, col: col, code: f.code, message: msg, suggestion: f.suggestion})
	}

	for lineIdx, line := range lines {
		lineNum := lineIdx + 1

		// Pass 1: dotted chains.
		for _, m := range javaPathRe.FindAllStringSubmatchIndex(line, -1) {
			head := line[m[2]:m[3]]
			rest := line[m[4]:m[5]]
			written := head + rest
			resolved := written
			if base, ok := classAliases[head]; ok {
				resolved = base + rest
			}
			idx, ok := matchForbiddenJavaPath(resolved)
			if !ok {
				continue
			}
			why := fmt.Sprintf("(reaches %s)", resolved)
			if resolved != written {
				why = fmt.Sprintf("(imported as %q, which resolves to %s)", head, resolved)
			}
			add(lineNum, m[0]+1, idx, resolved, why)
		}

		// Pass 2: bare calls reached through a static import.
		for _, m := range javaBareCallRe.FindAllStringSubmatchIndex(line, -1) {
			name := line[m[2]:m[3]]
			full, ok := staticAliases[name]
			if !ok {
				continue
			}
			idx, ok := matchForbiddenJavaPath(full)
			if !ok {
				continue
			}
			why := fmt.Sprintf("(imported statically as %q, which resolves to %s)", name, full)
			add(lineNum, m[0]+1, idx, full, why)
		}

		// Pass 3: bare type names -- declarations, constructors, generics.
		for _, tc := range typeChecks {
			for _, m := range tc.re.FindAllStringIndex(line, -1) {
				// Skip an occurrence that is the tail of a dotted chain pass 1
				// already resolved (e.g. the "File" in "java.io.File") -- it
				// is preceded by '.', so it is not a bare use of the name.
				if m[0] > 0 && line[m[0]-1] == '.' {
					continue
				}
				idx, ok := matchForbiddenJavaPath(tc.fqn)
				if !ok {
					continue
				}
				why := fmt.Sprintf("(resolves to %s)", tc.fqn)
				add(lineNum, m[0]+1, idx, tc.fqn, why)
			}
		}

		// Thread is deliberately not in forbiddenJavaPaths -- see the table's
		// doc comment. Its one forbidden form is matched directly.
		if loc := javaNewThreadRe.FindStringIndex(line); loc != nil {
			out = append(out, javaFinding{
				line: lineNum, col: loc[0] + 1, code: "J011",
				message:    "threading is non-deterministic across replays (thread scheduling differs between runs)",
				suggestion: "Workflow code is single-threaded by design",
			})
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

	// Scan each .java file for forbidden reaches.
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
		//
		// findForbiddenJavaPaths RESOLVES rather than matches substrings --
		// cleat#1812 -- so a grouped, aliased or fully-qualified reach is seen
		// the same as the spelling the old table happened to list.
		for _, fnd := range findForbiddenJavaPaths(javaCodeOnly(data)) {
			output.Errors = append(output.Errors, VetResult{
				Code:       fnd.code,
				File:       relPath,
				Line:       fnd.line,
				Column:     fnd.col,
				Message:    fnd.message,
				Suggestion: fnd.suggestion,
			})
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
