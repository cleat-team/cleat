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

// ---------------------------------------------------------------------------
// R008: HashMap/HashSet iteration order. cleat#1864.
//
// Everything above matches a MODULE PATH a workflow may not reach at all.
// This rule is a different shape: reaching std::collections::HashMap is not
// forbidden -- constructing, inserting into and looking up in one are all
// deterministic. Only enumerating its contents in an unspecified order is the
// hazard, so the rule has to fire on a USAGE PATTERN (which methods are
// called on a value of that type) rather than on a name appearing at all.
// That needs a type, which the resolver above deliberately does not carry
// (docs/contributor/design/rust-determinism-checker.md, "No type resolution").
//
// WHAT THIS BUYS INSTEAD: syntactic binding tracking, scoped to one function
// body at a time. A parameter's declared type, or a `let` binding's
// annotation or constructor call, is resolved as a type via the SAME alias
// map that resolves a path; a name resolving to HashMap or HashSet is
// tracked for the rest of that function, and an iteration-shaped use of a
// tracked name is reported.
//
// WHAT IT CANNOT SEE, matching the two documented fixtures:
//   - a map reached through a struct field or returned from a call
//     (self.counts.iter(), config.map.values()) -- the receiver is not a bare
//     tracked identifier (known_limit_map_via_struct_field)
//   - a map that is never bound to a name at all, iterated immediately off a
//     `.collect::<HashMap<_, _>>()` chain (known_limit_collect_into_map)
// Per the "over-report rather than under-report" rule this file already
// follows for file-scoped imports, a name shadowed mid-function under a
// DIFFERENT, non-map type keeps its earlier tracked status -- rare, and the
// safe direction for a gate.
//
// THIS IS DEFENCE IN DEPTH, NOT A CORRECTNESS FIX, and the message below must
// not claim otherwise. Measured 2026-09-17
// (docs/contributor/design/rust-determinism-checker.md, "The HashMap rule"):
// cleat intercepts the WASI random_get import a HashMap's RandomState reads to
// seed its hasher and binds it to a value deterministic in (workflow ID,
// step) rather than OS entropy (engine/wasmtime_wasi_determinism.go,
// engine/wasi_policy.go). Two replays of ONE workflow therefore see the same
// order today. What is not guaranteed is the Rust language's own contract,
// and a hardening rule against that is worth having independent of how any
// one runtime happens to seed it.

// forbiddenRustMapTypes lists the resolved TYPES whose iteration this rule
// covers -- a different table from forbiddenRustPaths' module paths, and
// matched by equality, not by prefix: neither type has a meaningful sub-path.
var forbiddenRustMapTypes = map[string]bool{
	"std::collections::HashMap": true,
	"std::collections::HashSet": true,
}

// rustMapOrderMethods are receiver methods whose result exposes a HashMap's
// or HashSet's enumeration order. get, get_mut, insert, remove,
// contains_key, len, entry and clear are deliberately absent: none of them
// depends on iteration order.
var rustMapOrderMethods = map[string]bool{
	"iter": true, "iter_mut": true, "keys": true,
	"values": true, "values_mut": true, "into_iter": true, "drain": true,
}

const rustHashMapSuggestion = "Use BTreeMap/BTreeSet for deterministic ordered iteration, or collect and sort the keys before iterating a HashMap/HashSet"

// resolveRustTypeHead strips a type-position expression down to the path its
// base type resolves to, the same question matchForbiddenRustPath asks of a
// call site's resolved path.
//
//	&HashMap<String, u64>            -> strip '&'      -> HashMap<String, u64> -> strip generics -> HashMap -> alias lookup
//	&mut HashSet<u32>                -> strip '&mut '  -> ditto
//	std::collections::HashMap<K, V>  -> strip generics -> std::collections::HashMap (already a full path, used as written)
//
// A bare identifier is looked up in aliases (the same map expandRustUse
// populates from this file's `use` declarations); a path already containing
// "::" needs no lookup, since writing one out in full requires no import.
func resolveRustTypeHead(text string, aliases map[string]string) string {
	t := strings.TrimSpace(text)
	for {
		switch {
		case strings.HasPrefix(t, "&mut "):
			t = strings.TrimSpace(t[len("&mut "):])
		case strings.HasPrefix(t, "&"):
			t = strings.TrimSpace(t[1:])
		default:
			goto stripped
		}
	}
stripped:
	if idx := strings.IndexByte(t, '<'); idx >= 0 {
		t = t[:idx]
	}
	t = strings.TrimSpace(t)
	if t == "" || strings.Contains(t, "::") {
		return t
	}
	if full, ok := aliases[t]; ok {
		return full
	}
	return t
}

// splitRustTopLevelCommas splits s on commas not nested inside (), <> or [].
// expandRustUse already has this shape for `{}` import groups; a parameter
// list and a generic argument list nest in the other three bracket kinds
// instead of that one, so this is a sibling rather than a reuse of it.
func splitRustTopLevelCommas(s string) []string {
	var parts []string
	depth := 0
	start := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '(', '<', '[':
			depth++
		case ')', '>', ']':
			depth--
		case ',':
			if depth == 0 {
				parts = append(parts, s[start:i])
				start = i + 1
			}
		}
	}
	return append(parts, s[start:])
}

// skipRustAngleBrackets advances past a balanced <...> starting at src[i], or
// returns i unchanged if src[i] is not '<'. Needed for a function's own
// generic parameters, fn foo<T: Bar<Baz>>(...), where the naive
// strings.Index(src[i:], ">") stops at Baz's closing angle bracket instead of
// the function's own.
func skipRustAngleBrackets(src string, i int) int {
	if i >= len(src) || src[i] != '<' {
		return i
	}
	depth := 0
	for j := i; j < len(src); j++ {
		switch src[j] {
		case '<':
			depth++
		case '>':
			depth--
			if depth == 0 {
				return j + 1
			}
		}
	}
	return -1
}

// skipRustParens advances past a balanced (...) starting at src[i], which
// must be '(', returning the index one past the matching ')'. Depth-counted
// for the same reason as skipRustAngleBrackets: a parameter's own type can
// nest parens, fn foo(f: Box<dyn Fn(u32) -> u32>).
func skipRustParens(src string, i int) int {
	if i >= len(src) || src[i] != '(' {
		return -1
	}
	depth := 0
	for j := i; j < len(src); j++ {
		switch src[j] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return j + 1
			}
		}
	}
	return -1
}

// rustFuncSpan is one function's parameter-list text and its body's byte
// range in the source that produced it, [bodyStart, bodyEnd).
type rustFuncSpan struct {
	params             string
	bodyStart, bodyEnd int
}

var rustFnNameRe = regexp.MustCompile(`\bfn\s+[A-Za-z_][A-Za-z0-9_]*`)

// findRustFuncSpans locates every function's parameter list and body in
// already-blanked source, by hand rather than with one regex: a parameter's
// type can nest angle brackets and parens arbitrarily deep
// (Box<dyn Fn(u32) -> u32>), which is exactly what a fixed-depth regex gets
// wrong.
//
// A NESTED fn (a closure-adjacent inner function, or one defined inside
// another's body) is found as its own span AND falls inside its enclosing
// function's body text, so the outer function's binding tracking sees the
// inner one's `let`s too. That over-tracks rather than under-tracks, which
// this file already treats as the safe direction for a gate.
func findRustFuncSpans(src string) []rustFuncSpan {
	var spans []rustFuncSpan
	for _, loc := range rustFnNameRe.FindAllStringIndex(src, -1) {
		i := loc[1]
		i = skipRustAngleBrackets(src, i)
		if i < 0 {
			continue
		}
		for i < len(src) && (src[i] == ' ' || src[i] == '\t' || src[i] == '\n' || src[i] == '\r') {
			i++
		}
		if i >= len(src) || src[i] != '(' {
			continue
		}
		parenEnd := skipRustParens(src, i)
		if parenEnd < 0 {
			continue
		}
		params := src[i+1 : parenEnd-1]
		j := parenEnd
		for j < len(src) && src[j] != '{' && src[j] != ';' {
			j++
		}
		if j >= len(src) || src[j] != '{' {
			continue // a trait method with no body, or a signature this does not parse
		}
		depth, bodyEnd := 0, -1
		for k := j; k < len(src); k++ {
			switch src[k] {
			case '{':
				depth++
			case '}':
				depth--
				if depth == 0 {
					bodyEnd = k + 1
				}
			}
			if bodyEnd >= 0 {
				break
			}
		}
		if bodyEnd < 0 {
			continue
		}
		spans = append(spans, rustFuncSpan{params: params, bodyStart: j, bodyEnd: bodyEnd})
	}
	return spans
}

// rustParamBindingType returns a function parameter's name and resolved
// type, for a parameter of the shape "[mut] name: Type". self, &self and
// &mut self have no ':' and return ok=false; so does a destructuring
// pattern parameter, which this does not parse -- a documented miss, not a
// crash.
func rustParamBindingType(param string, aliases map[string]string) (name, resolved string, ok bool) {
	p := strings.TrimSpace(param)
	idx := strings.IndexByte(p, ':')
	if p == "" || idx < 0 {
		return "", "", false
	}
	name = strings.TrimSpace(p[:idx])
	name = strings.TrimSpace(strings.TrimPrefix(name, "mut "))
	if name == "" || strings.ContainsAny(name, "(){}&") {
		return "", "", false
	}
	resolved = resolveRustTypeHead(p[idx+1:], aliases)
	return name, resolved, resolved != ""
}

var rustLetRe = regexp.MustCompile(`(?s)\blet\s+(?:mut\s+)?([A-Za-z_][A-Za-z0-9_]*)\s*(?::\s*([^=;]+?))?\s*=\s*([^;]+);`)

// rustConstructorCallRe matches a constructor-style call at the start of a
// let-binding's right-hand side, HashMap::new(), HashSet::with_capacity(8),
// HashMap::from([...]). It deliberately does not match Default::default() --
// resolving that needs the TARGET type, which is bidirectional inference this
// checker does not do -- or .collect::<HashMap<_, _>>(), which
// known_limit_collect_into_map documents as a limit rather than silently
// accepting.
var rustConstructorCallRe = regexp.MustCompile(`^\s*([A-Za-z_][A-Za-z0-9_:]*)::(?:new|with_capacity|from)\s*[(<]`)

// findRustMapBindingsInFunc returns the names, tracked for one function, that
// resolve to a forbidden map type -- from its parameters and from `let`
// bindings inside body.
func findRustMapBindingsInFunc(body, params string, aliases map[string]string) map[string]bool {
	tracked := map[string]bool{}
	for _, p := range splitRustTopLevelCommas(params) {
		name, resolved, ok := rustParamBindingType(p, aliases)
		if ok && forbiddenRustMapTypes[resolved] {
			tracked[name] = true
		}
	}
	for _, m := range rustLetRe.FindAllStringSubmatch(body, -1) {
		name, typeAnnotation, rhs := m[1], m[2], m[3]
		resolved := ""
		if strings.TrimSpace(typeAnnotation) != "" {
			resolved = resolveRustTypeHead(typeAnnotation, aliases)
		}
		if resolved == "" {
			if ctor := rustConstructorCallRe.FindStringSubmatch(rhs); ctor != nil {
				resolved = resolveRustTypeHead(ctor[1], aliases)
			}
		}
		if forbiddenRustMapTypes[resolved] {
			tracked[name] = true
		}
	}
	return tracked
}

var (
	rustForInRe         = regexp.MustCompile(`\bfor\b[^{;]*?\bin\s+(?:&mut\s+|&\s*)?([A-Za-z_][A-Za-z0-9_]*)\s*\{`)
	rustMapMethodCallRe = regexp.MustCompile(`\b([A-Za-z_][A-Za-z0-9_]*)\.(iter_mut|iter|keys|values_mut|values|into_iter|drain)\s*\(`)
)

// findRustMapIterationFindings resolves every function body in already-blanked
// source and reports an iteration-shaped use of a name tracked as a HashMap
// or HashSet within that function.
//
// DEDUPLICATED BY LINE, for the same reason findForbiddenRustPaths dedupes:
// `for (k, v) in &m {}` on one line matches only the for-in shape, but a
// chained `m.iter().rev()` could in principle be revisited by an overlapping
// scan; one report per line is what a reader acts on.
func findRustMapIterationFindings(code []byte) []rustFinding {
	src := string(code)

	aliases := map[string]string{}
	for _, m := range rustUseRe.FindAllStringSubmatch(src, -1) {
		expandRustUse(m[1], aliases)
	}

	var out []rustFinding
	seen := map[int]bool{}
	report := func(pos int, kind string) {
		line := 1 + strings.Count(src[:pos], "\n")
		if seen[line] {
			return
		}
		seen[line] = true
		col := pos - strings.LastIndex(src[:pos], "\n")
		out = append(out, rustFinding{
			line: line, col: col,
			code: "R008",
			message: fmt.Sprintf("%s iteration order is not guaranteed by the language and is not to be relied on for a durable replay",
				kind),
			suggestion: rustHashMapSuggestion,
		})
	}

	for _, span := range findRustFuncSpans(src) {
		body := src[span.bodyStart:span.bodyEnd]
		tracked := findRustMapBindingsInFunc(body, span.params, aliases)
		if len(tracked) == 0 {
			continue
		}

		for _, loc := range rustForInRe.FindAllStringSubmatchIndex(body, -1) {
			name := body[loc[2]:loc[3]]
			if tracked[name] {
				report(span.bodyStart+loc[0], "a for-loop over a map")
			}
		}
		for _, loc := range rustMapMethodCallRe.FindAllStringSubmatchIndex(body, -1) {
			name := body[loc[2]:loc[3]]
			method := body[loc[4]:loc[5]]
			if tracked[name] && rustMapOrderMethods[method] {
				report(span.bodyStart+loc[0], fmt.Sprintf("%s()", method))
			}
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
		findings := findForbiddenRustPaths(code)
		findings = append(findings, findRustMapIterationFindings(code)...)
		for _, f := range findings {
			output.Errors = append(output.Errors, VetResult{
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
