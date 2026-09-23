package main

import (
	"fmt"
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
//
// WHY SOURCE-LEVEL REGEX RATHER THAN READING THE COMPILER'S OWN OUTPUT
// (cleat#2109 review). Go's own EntryPoints (wasm/exports.go, GenerateExports)
// has no such gap: cleat's own analyzer walks the AST to decide what to
// generate, and the SAME computation is what gets recorded, so there is
// nothing to drift. For Rust/Java/AssemblyScript cleat does not generate the
// export code -- cargo, TeaVM and asc do, from the SDK's own macro,
// annotation processor and transform -- so these extractors are a SEPARATE,
// approximate prediction of what that external compilation will produce, not
// a report of what it already produced. That is a materially weaker
// guarantee, and the honest list of what it can miss: a macro-generated
// #[cleat_entry] invocation (one that does not appear literally as
// `#[cleat_entry]` in source), a cfg-gated function compiled out for the
// target platform, a decorator/annotation split across lines in a shape none
// of these regexes anticipated, or the annotation imported under a renamed
// alias.
//
// The principled fix is each SDK's own codegen stating its entries in the
// artifact it produces -- e.g. the Rust proc macro emitting a
// `cleat.entry_points` custom WASM section at expansion, since it is the one
// place that genuinely sees every #[cleat_entry] the way the compiler will.
// That needs the macro to accumulate names across independent expansions
// into one section (Rust's proc-macro model has no such state today) and an
// equivalent per-SDK mechanism for Java/AssemblyScript -- real compiler-level
// work in three different toolchains, not a build_entry_points.go change.
// Out of scope for a 0.3.0 fix; tracked as a follow-up rather than folded in
// here, so the choice is on record rather than left to be rediscovered.
//
// So THIS extractor is a stand-in, and it is not left unchecked: every name
// it puts in cleat.metadata is cross-verified against the .wasm the build
// just produced (verifyEntryPointsAreExports, below) before that binary is
// written to disk. A name this file predicted that the compiler did not
// actually export fails the BUILD, loudly, at the moment the drift is
// introduced -- rather than surfacing later as a live "cannot determine
// entry point" a caller has no way to connect back to a source change.

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

// verifyEntryPointsAreExports fails the build if any name a source-level
// extractor predicted is not actually a function export of the .wasm the
// compiler just produced -- the check that makes the source-scanning
// approach documented at the top of this file safe to ship: a name this
// file's regexes got wrong (a macro-generated entry, a cfg-gated function
// compiled out, a decorator shape none of them anticipated) fails HERE,
// loudly, rather than later as a live "cannot determine entry point" with no
// path back to the source change that caused it.
//
// Deliberately one-directional: it does not require every wasm export to be
// a predicted entry point (a crate may export other things -- allocator
// hooks, helper functions marked pub -- that were never meant to be entry
// points), only that every predicted entry point is a real export.
func verifyEntryPointsAreExports(sdk string, wasmBytes []byte, entryPoints []string) error {
	if len(entryPoints) == 0 {
		return nil
	}
	exports := wasmFuncExportNames(wasmBytes)
	var missing []string
	for _, name := range entryPoints {
		if !exports[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("%s build: cleat.metadata would declare %d entry point(s) not actually exported "+
		"by the compiled .wasm: %s -- the source-level extractor (build_entry_points.go) predicted a name "+
		"the compiler did not produce. This is refused rather than deployed with metadata that lies about "+
		"the binary's own exports; see build_entry_points.go's doc comment for why this check exists",
		sdk, len(missing), strings.Join(missing, ", "))
}

// wasmFuncExportNames returns the names of every function export (export
// kind 0) in a compiled WASM binary's export section. Same binary-format
// parse cmd/cleat-worker/setup.go's firstHandleExport already uses in
// production (magic+version header, then walk sections for id 7), just
// collecting every func export instead of the first "handle_"-prefixed one.
func wasmFuncExportNames(wasmBytes []byte) map[string]bool {
	names := map[string]bool{}
	if len(wasmBytes) < 8 {
		return names
	}
	pos := 8 // skip magic + version
	for pos < len(wasmBytes) {
		sectionID := wasmBytes[pos]
		pos++
		sectionLen, n := decodeULEB128AtOffset(wasmBytes, pos)
		pos = n
		sectionEnd := pos + int(sectionLen)
		if sectionID != 7 { // not the export section
			pos = sectionEnd
			continue
		}
		count, n := decodeULEB128AtOffset(wasmBytes, pos)
		pos = n
		for i := uint32(0); i < count; i++ {
			nameLen, n := decodeULEB128AtOffset(wasmBytes, pos)
			pos = n
			if pos+int(nameLen) > len(wasmBytes) {
				return names // malformed; report what was parsed so far
			}
			name := string(wasmBytes[pos : pos+int(nameLen)])
			pos += int(nameLen)
			if pos >= len(wasmBytes) {
				return names
			}
			kind := wasmBytes[pos]
			pos++
			_, n = decodeULEB128AtOffset(wasmBytes, pos) // index
			pos = n
			if kind == 0 {
				names[name] = true
			}
		}
		return names
	}
	return names
}

// decodeULEB128AtOffset reads an unsigned LEB128 value from buf at offset
// pos and returns the value and the new offset.
func decodeULEB128AtOffset(buf []byte, pos int) (uint32, int) {
	var result uint32
	var shift uint
	for pos < len(buf) {
		b := buf[pos]
		pos++
		result |= uint32(b&0x7F) << shift
		if b&0x80 == 0 {
			break
		}
		shift += 7
	}
	return result, pos
}
