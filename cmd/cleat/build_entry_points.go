package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// This file extracts, from source, the entry-point names each non-Go SDK's
// own codegen will use as the WASM export name for a durable entry point --
// cleat#2097, the Rust/Java/AssemblyScript half of cleat#2096. Each
// extractor mirrors the ACTUAL naming rule the SDK's own codegen uses, not a
// guess at one, so a name in wasm.Metadata.EntryPoints always matches a real
// export determineEntryPoint (cmd/cleat-worker/setup.go) can resolve:
//
//   - Rust (crates/cleat-macro/src/entry.rs): #[cleat_entry] takes no
//     arguments. The export is always the literal function identifier.
//   - Java (crates/cleat-java/.../CleatEntryProcessor.java:121-123): the
//     export is @CleatEntry's `name` attribute WHEN GIVEN, and only falls
//     back to the method's own name when that attribute is absent or empty.
//   - AssemblyScript (packages/cleat-as/transform/index.js:225,982): the
//     export is always the literal function identifier -- @cleatEntry's
//     string argument (a display name) is never read for this.
//
// Best-effort source scanning, at the same rigor as this file's existing
// vet_rust.go/vet_java.go checks (which already text-scan for presence, not
// full parse) -- but every extractor here is proven against the checked-in
// example under examples/, not just a synthetic fixture, in
// build_entry_points_test.go.

// rustEntryPointNames returns the WASM export names crateDir's #[cleat_entry]
// functions will compile to, in file-then-source order.
func rustEntryPointNames(crateDir string) []string {
	var names []string
	rustEntryRe := regexp.MustCompile(`#\[cleat_entry\][\s]*(?:#\[[^\]]*\][\s]*)*(?:pub(?:\([^)]*\))?\s+)?(?:unsafe\s+)?fn\s+(\w+)`)
	walkSourceFiles(crateDir, ".rs", []string{"target", ".git", "tests", "benches", "examples"}, func(data []byte) {
		code := stripCLikeComments(data)
		for _, m := range rustEntryRe.FindAllSubmatch(code, -1) {
			names = append(names, string(m[1]))
		}
	})
	return names
}

// javaEntryPointNames returns the WASM export names projectDir's @CleatEntry
// methods will compile to, in file-then-source order.
func javaEntryPointNames(projectDir string) []string {
	var names []string
	// Captures the annotation's optional name="..." (group 1, may be empty
	// if the attribute list has no `name=`) and the method identifier that
	// follows -- possibly past other modifiers/annotations/a return type
	// (group 2, the LAST identifier before the parameter list's open paren,
	// which is always the method name in a Java declaration).
	annotationRe := regexp.MustCompile(`@CleatEntry(?:\s*\(([^)]*)\))?`)
	nameAttrRe := regexp.MustCompile(`name\s*=\s*"([^"]*)"`)
	methodRe := regexp.MustCompile(`(\w+)\s*\(`)
	walkSourceFiles(projectDir, ".java", []string{"build", ".gradle", ".git", "target", "node_modules"}, func(data []byte) {
		code := stripCLikeCommentsKeepStrings(data)
		locs := annotationRe.FindAllSubmatchIndex(code, -1)
		for _, loc := range locs {
			var explicitName string
			if loc[2] != -1 {
				if nm := nameAttrRe.FindSubmatch(code[loc[2]:loc[3]]); nm != nil {
					explicitName = string(nm[1])
				}
			}
			// Scan forward from the end of the annotation (whatever matched
			// -- with or without parens) for the method's own identifier.
			rest := code[loc[1]:]
			if m := methodRe.FindSubmatch(rest); m != nil {
				methodName := string(m[1])
				if explicitName != "" {
					names = append(names, explicitName)
				} else {
					names = append(names, methodName)
				}
			}
		}
	})
	return names
}

// asEntryPointNames returns the WASM export names asDir's assembly/*.ts
// @cleatEntry functions will compile to, in file-then-source order.
func asEntryPointNames(asDir string) []string {
	var names []string
	entryRe := regexp.MustCompile(`@cleatEntry(?:\([^)]*\))?[\s]*export\s+function\s+(\w+)`)
	walkSourceFiles(filepath.Join(asDir, "assembly"), ".ts", []string{"node_modules", ".git"}, func(data []byte) {
		code := stripCLikeComments(data)
		for _, m := range entryRe.FindAllSubmatch(code, -1) {
			names = append(names, string(m[1]))
		}
	})
	return names
}

// walkSourceFiles finds every file under root ending in ext (skipping any
// directory named in skipDirs) and calls fn with its contents. Errors
// reading an individual file or walking a subtree are skipped silently --
// this is a best-effort metadata enrichment, not the build itself, and
// build_entry_points_test.go proves it finds what it should on the real
// example trees rather than trusting that silence.
func walkSourceFiles(root, ext string, skipDirs []string, fn func(data []byte)) {
	skip := make(map[string]bool, len(skipDirs))
	for _, d := range skipDirs {
		skip[d] = true
	}
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if skip[d.Name()] || strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ext) {
			if data, err := os.ReadFile(path); err == nil {
				fn(data)
			}
		}
		return nil
	})
}

// stripCLikeComments blanks // and /* */ comments (keeping byte length and
// newlines, so regex match positions stay meaningful) without touching
// string contents -- adequate for Rust/TS, neither of which this file needs
// to read a string argument from.
func stripCLikeComments(src []byte) []byte {
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
		case src[i] == '/' && i+1 < n && src[i+1] == '/':
			for ; i < n && src[i] != '\n'; i++ {
				blank(i)
			}
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
		default:
			i++
		}
	}
	return out
}

// stripCLikeCommentsKeepStrings is stripCLikeComments, except it leaves
// string literal contents intact -- Java's extractor needs to read
// @CleatEntry(name = "...")'s actual string argument.
func stripCLikeCommentsKeepStrings(src []byte) []byte {
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
		case src[i] == '/' && i+1 < n && src[i+1] == '/':
			for ; i < n && src[i] != '\n'; i++ {
				blank(i)
			}
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
		case src[i] == '"':
			i++
			for i < n && src[i] != '"' {
				if src[i] == '\\' && i+1 < n {
					i += 2
					continue
				}
				i++
			}
			if i < n {
				i++
			}
		default:
			i++
		}
	}
	return out
}
