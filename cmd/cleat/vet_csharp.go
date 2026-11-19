package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// The C# determinism vet (cleat#4080, PR 6). A pure-Go source check, run by `cleat build --target csharp` before the toolchain is looked
// for and by `cleat vet --lang csharp`, in the shape of vet_java.go and vet_rust.go: blank comments and strings, then resolve names rather
// than match substrings.
//
// EVERY RULE CITES A MEASUREMENT, and nothing is refused on the strength of its name. tests/csharp/determinism_probe_test.go builds
// packages/cleat-csharp/probe (a component whose entries deliberately use each API) and runs each one four ways on the real engine and the
// real wasmtime backend: fresh, fresh again, replayed over the first run's history, and fresh under a DIFFERENT workflow identity. The
// fourth is the control: a value the engine pins to the identity must differ there, or "it replays" would also be true of a constant the
// guest never read from anywhere. What the table says, and so what this file does and does not refuse:
//
//	REFUSED, because the component cannot run it or cannot replay it
//	  Task.Run, Task.Factory.StartNew, Task.Delay, .Wait(), new Thread   threw PlatformNotSupportedException on every run
//	  Thread.Sleep                                   returned after really sleeping: it blocks the worker and is not durable
//	  Stopwatch, Environment.TickCount[64]           the real monotonic clock, not pinned: Stopwatch differs on every run; TickCount64 is a few ms
//	                                                 since instance start and coincides with its replay by chance (7 of 9 runs differed)
//	  RandomNumberGenerator                          threw PlatformNotSupportedException
//	  File, Directory                                no filesystem: File.ReadAllText and WriteAllText threw DirectoryNotFoundException,
//	                                                 and File.Exists answered False without error, which is the quiet kind of wrong
//	  HttpClient                                     the component imports wasi:http, which the engine does not provide, so EVERY entry of
//	                                                 the component fails to instantiate, not just the one that uses it
//
//	NOT REFUSED, because measured fine: DateTime.UtcNow and Now, DateTimeOffset.UtcNow, Random (shared, unseeded and seeded), Guid.NewGuid,
//	string and object hash codes, Task.Yield, Console, the Environment properties, async/await over host calls. The first group is replay-stable:
//	the engine shadows the WASI wall clock and entropy with the durable clock and a source seeded by the workflow identity, and the control
//	run shows the values FOLLOW that identity (cleat#4078 made the shadows reach every WASI minor a C# toolchain emits). The README said the
//	opposite until this PR.
//
//	A WARNING, not an error: a catch that swallows CleatSuspendException. Measured: the engine still saw the suspension and discarded the
//	guest's result, BUT a host call made after the catch REACHED THE SERVICE and was recorded in the history (1 call received, 1 event), where
//	the same entry with a filter that excludes CleatSuspendException, or with a rethrow, made none. So it is real work done past the point the
//	host said to stop. It is a warning because the same catch around code that cannot suspend (parsing, say) is harmless and a source check
//	cannot tell which it is: it flags `catch`, `catch (Exception)` and `catch (Exception e)` whose body has no `throw` and whose filter does
//	not name CleatSuspendException, unless an earlier catch of the same try handles CleatSuspendException.
//
// WHAT THIS CANNOT SEE, stated rather than left to be discovered: there is no type information, so a name is resolved by spelling. A
// reflection call, a delegate pulled out of a variable, and an extension method that wraps one of the above are invisible. A type named like a
// refused one that this file does not declare (reached through an assembly reference, say) is a false positive. `using static` is resolved for
// Task, Thread and Environment, the types whose refusals are by member. The vet reads .cs files only: Compile items in the .csproj are not
// interpreted, so a source file outside the project directory is not read and a file excluded by the project still is.

// csRule is one refusal. typ is the BCL type's simple name and ns the namespace it lives in, which is the only prefix a qualified spelling may
// carry. members is the set of refused members; empty means every use of the type.
type csRule struct {
	code, typ, ns string
	members       []string
	message       string
	suggestion    string
}

var csharpRules = []csRule{
	{"CV001", "Task", "System.Threading.Tasks", []string{"Run", "Factory"},
		"Task.Run and Task.Factory.StartNew throw PlatformNotSupportedException in a component: there are no threads to run them on (measured)",
		"Workflow code is single-threaded. Call the host (h.Call, h.Sleep) and await it, or compute inline"},
	{"CV002", "Task", "System.Threading.Tasks", []string{"Delay"},
		"Task.Delay throws PlatformNotSupportedException in a component: it needs a timer (measured)",
		"Use h.Sleep(duration), which is durable"},
	{"CV005", "Thread", "System.Threading", []string{"Sleep"},
		"Thread.Sleep really sleeps (measured: it returned after the full wait), so it blocks the worker and is not a durable sleep",
		"Use h.Sleep(duration), which suspends the workflow and does not hold a worker"},
	{"CV006", "Stopwatch", "System.Diagnostics", nil,
		"Stopwatch reads the real monotonic clock, which is NOT replayed: the value of a run differs from its replay (measured)",
		"Use h.Now() for the durable clock; subtract two readings for a duration"},
	{"CV007", "Environment", "System", []string{"TickCount", "TickCount64"},
		"Environment.TickCount reads a monotonic clock the engine does not pin: milliseconds since the instance started, so a replay reads a different value unless the two start-ups happen to take the same time (measured over 9 runs: 7 differed, 2 coincided)",
		"Use h.Now() for the durable clock"},
	{"CV008", "RandomNumberGenerator", "System.Security.Cryptography", nil,
		"RandomNumberGenerator throws PlatformNotSupportedException in a component (measured)",
		"Use h.Random() or new Random(), which are seeded by the workflow identity and replay; real secrets belong in a service call"},
	{"CV009", "File", "System.IO", nil,
		"a component has no filesystem: File.ReadAllText and File.WriteAllText threw DirectoryNotFoundException, and File.Exists answered False without error (measured)",
		"Read data through a service call (h.Call) or pass it in as the workflow input"},
	{"CV009", "Directory", "System.IO", nil,
		"a component has no filesystem (measured on File; Directory shares the mechanism and was not separately measured)",
		"Read data through a service call (h.Call) or pass it in as the workflow input"},
	{"CV010", "HttpClient", "System.Net.Http", nil,
		"a component that references HttpClient imports wasi:http, which the engine does not provide: EVERY entry point of the component then fails to instantiate (measured)",
		"Make the request through a service call (h.Call) so it is journaled and replayed"},
}

type csFinding struct {
	line, col                 int
	code, message, suggestion string
	warning                   bool // reported as a warning: it does not fail the build
}

// csharpCodeOnly returns src with every comment and every string or character literal replaced by spaces, offsets and line endings
// untouched, so a reported line and column point at the source. It is the shape of javaCodeOnly, for C#.
//
// WHAT IT MODELS:
//
//   - // line comments (including ///), and /* */ block comments, which do not nest
//   - "..." with backslash escapes; @"..." verbatim, where "" is a quote and a backslash is just a backslash; and the three-quote raw form
//     """...""" (C# 11), ended by a run of at least as many quotes
//   - $"..." and $@"..." interpolated strings: the LITERAL text is blanked and the {expression} holes are KEPT, because a hole is code and a
//     refused call inside one is a refused call. {{ and }} are escaped braces. A hole may contain a nested string, which is blanked
//     recursively. An interpolated RAW string ($"""...""") is blanked whole, a stated limit.
//   - 'x' and '\n' character literals
//
// Preprocessor lines are left alone: #if branches are all scanned, which over-reports a disabled branch and never under-reports.
func csharpCodeOnly(src []byte) []byte {
	out := make([]byte, len(src))
	copy(out, src)
	blank := func(i int) {
		if i < len(out) && out[i] != '\n' && out[i] != '\r' {
			out[i] = ' '
		}
	}
	n := len(src)

	// scanString blanks a string literal whose opening quote is at i and returns the index after it. verbatim and interp select the form.
	var scanString func(i int, verbatim, interp bool) int
	scanString = func(i int, verbatim, interp bool) int {
		blank(i) // the opening quote
		i++
		for i < n {
			c := src[i]
			switch {
			case verbatim && c == '"' && i+1 < n && src[i+1] == '"':
				blank(i)
				blank(i + 1)
				i += 2
			case c == '"':
				blank(i)
				return i + 1
			case !verbatim && c == '\\' && i+1 < n:
				blank(i)
				blank(i + 1)
				i += 2
			case interp && c == '{' && i+1 < n && src[i+1] == '{':
				blank(i)
				blank(i + 1)
				i += 2
			case interp && c == '}' && i+1 < n && src[i+1] == '}':
				blank(i)
				blank(i + 1)
				i += 2
			case interp && c == '{':
				// A hole: keep the braces and the expression, blank any string inside it, stop at the matching close brace.
				i++
				depth := 1
				for i < n && depth > 0 {
					switch {
					case src[i] == '"':
						i = scanString(i, false, false)
					case src[i] == '@' && i+1 < n && src[i+1] == '"':
						blank(i)
						i = scanString(i+1, true, false)
					case src[i] == '\'':
						i = scanChar(src, blank, i)
					case src[i] == '{':
						depth++
						i++
					case src[i] == '}':
						depth--
						i++
					default:
						i++
					}
				}
			case c == '\n' && !verbatim:
				// An unterminated regular string ends at the line: keep the scanner from eating the file.
				return i
			default:
				blank(i)
				i++
			}
		}
		return i
	}

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
		// Raw string, with or without a leading $: a run of 3 or more quotes opens it and a run of at least as many closes it.
		case isRawStringStart(src, i):
			j := i
			nd := 0 // the number of $ prefixes: a hole opens at that many braces in a row and closes at that many
			for j < n && (src[j] == '$' || src[j] == '@') {
				if src[j] == '$' {
					nd++
				}
				blank(j)
				j++
			}
			q := 0
			for j+q < n && src[j+q] == '"' {
				q++
			}
			for k := 0; k < q; k++ {
				blank(j + k)
			}
			j += q
			for j < n {
				if src[j] == '"' {
					run := 0
					for j+run < n && src[j+run] == '"' {
						run++
					}
					if run >= q {
						for k := 0; k < run; k++ {
							blank(j + k)
						}
						j += run
						break
					}
					for k := 0; k < run; k++ {
						blank(j + k)
					}
					j += run
					continue
				}
				if nd > 0 && src[j] == '{' {
					run := 0
					for j+run < n && src[j+run] == '{' {
						run++
					}
					if run >= nd {
						// The last nd braces open a hole; any before them are literal text. The hole is CODE and is kept.
						for k := 0; k < run; k++ {
							blank(j + k)
						}
						j += run
						depth := 0
						for j < n {
							switch {
							case src[j] == '"':
								j = scanString(j, false, false)
							case src[j] == '\'':
								j = scanChar(src, blank, j)
							case src[j] == '{':
								depth++
								j++
							case src[j] == '}':
								cl := 0
								for j+cl < n && src[j+cl] == '}' {
									cl++
								}
								if depth == 0 && cl >= nd {
									for k := 0; k < cl; k++ {
										blank(j + k)
									}
									j += cl
									goto holeDone
								}
								depth--
								j++
							default:
								j++
							}
						}
					holeDone:
						continue
					}
				}
				blank(j)
				j++
			}
			i = j
		case (src[i] == '$' && i+1 < n && src[i+1] == '@' && i+2 < n && src[i+2] == '"') || (src[i] == '@' && i+1 < n && src[i+1] == '$' && i+2 < n && src[i+2] == '"'):
			blank(i)
			blank(i + 1)
			i = scanString(i+2, true, true)
		case src[i] == '$' && i+1 < n && src[i+1] == '"':
			blank(i)
			i = scanString(i+1, false, true)
		case src[i] == '@' && i+1 < n && src[i+1] == '"':
			blank(i)
			i = scanString(i+1, true, false)
		case src[i] == '"':
			i = scanString(i, false, false)
		case src[i] == '\'':
			i = scanChar(src, blank, i)
		default:
			i++
		}
	}
	return out
}

// isRawStringStart reports whether src[i:] opens a raw string literal: optional $ and @ prefix characters, then three or more quotes.
func isRawStringStart(src []byte, i int) bool {
	j := i
	for j < len(src) && (src[j] == '$' || src[j] == '@') {
		j++
	}
	return j+2 < len(src) && src[j] == '"' && src[j+1] == '"' && src[j+2] == '"'
}

// scanChar blanks a character literal opening at i (a plain character, an escape such as the newline or quote escapes, or a unicode escape) and returns the index after it. A quote that does not close
// within a few bytes is not a literal (it is rare in C#, where ' is never a lifetime, but a stray one must not eat code).
func scanChar(src []byte, blank func(int), i int) int {
	n := len(src)
	j := i + 1
	if j < n && src[j] == '\\' {
		j += 2
	} else {
		j++
	}
	for k := 0; k < 8 && j < n && src[j] != '\n'; k++ {
		if src[j] == '\'' {
			for m := i; m <= j; m++ {
				blank(m)
			}
			return j + 1
		}
		j++
	}
	return i + 1
}

var (
	csDottedRe   = regexp.MustCompile(`[A-Za-z_@][A-Za-z0-9_]*(?:\s*\.\s*[A-Za-z_@][A-Za-z0-9_]*)*`)
	csUsingAlias = regexp.MustCompile(`(?m)^\s*(?:global\s+)?using\s+([A-Za-z_]\w*)\s*=\s*([A-Za-z_][\w.\s]*?)\s*;`)
	csUsingStat  = regexp.MustCompile(`(?m)^\s*(?:global\s+)?using\s+static\s+([A-Za-z_][\w.\s]*?)\s*;`)
	csNewThread  = regexp.MustCompile(`\bnew\s+((?:[A-Za-z_]\w*\s*\.\s*)*)Thread\s*\(`)
	csWaitCall   = regexp.MustCompile(`\.\s*Wait\s*\(`)
	csGlobalPfx  = regexp.MustCompile(`^global\s*::\s*`)
)

var csNameofRe = regexp.MustCompile(`\bnameof\s*\(`)

// csBlankNameof blanks the argument of every nameof(...): `nameof(Thread.Sleep)` is a string the compiler computes and calls nothing, and it
// must not be refused as if it did. Offsets are kept.
func csBlankNameof(src string) string {
	b := []byte(src)
	for _, m := range csNameofRe.FindAllStringIndex(src, -1) {
		open := m[1] - 1
		end := csMatchClose(src, open, '(', ')')
		if end < 0 {
			continue
		}
		for i := open + 1; i < end; i++ {
			if b[i] != '\n' && b[i] != '\r' {
				b[i] = ' '
			}
		}
	}
	return string(b)
}

// csDeclares returns a regexp matching a declaration of a type called name in this file, which shadows the BCL type of the same name.
func csDeclares(name string) *regexp.Regexp {
	return regexp.MustCompile(`\b(?:class|struct|enum|interface|record(?:\s+(?:class|struct))?|delegate\s+\S+)\s+` + regexp.QuoteMeta(name) + `\b`)
}

func csJoinDots(s string) string {
	parts := strings.Split(s, ".")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return strings.Join(parts, ".")
}

func csRuleFor(typ string) []int {
	var idx []int
	for i, r := range csharpRules {
		if r.typ == typ {
			idx = append(idx, i)
		}
	}
	return idx
}

// findForbiddenCSharp reports every refused reach in already-blanked source.
//
// RESOLUTION, in the order it is applied:
//
//  1. A type this file declares itself (class File { }) is the file's own and is never refused. So is an alias that points anywhere but the
//     BCL path (using File = MyLib.File;).
//  2. `using X = System.IO.File;` makes X mean File, so an aliased spelling is seen.
//  3. A dotted chain is a refusal when, reading left to right, it is the type alone or the type preceded by EXACTLY its namespace (or
//     global::), and the member that follows is one the rule refuses. A chain whose head is anything else (`this.File.Exists`, `x.Task.Run`)
//     is a member access on a value and is not a reach for the BCL type.
//  4. `using static System.Threading.Tasks.Task;` makes a bare Run( or Delay( a refusal, and likewise for Thread and Environment.
//
// Deduplicated by (code, resolved path, line).
func findForbiddenCSharp(code []byte) []csFinding {
	src := csBlankNameof(string(code))
	lines := strings.Split(src, "\n")

	known := map[string]bool{}
	for _, r := range csharpRules {
		known[r.typ] = true
	}
	nsOf := map[string]string{}
	for _, r := range csharpRules {
		nsOf[r.typ] = r.ns
	}

	shadowed := map[string]bool{}
	for typ := range known {
		if csDeclares(typ).MatchString(src) {
			shadowed[typ] = true
		}
	}
	alias := map[string]string{} // alias name -> BCL type name
	for _, m := range csUsingAlias.FindAllStringSubmatch(src, -1) {
		name, target := m[1], csJoinDots(csGlobalPfx.ReplaceAllString(m[2], ""))
		resolved := ""
		for typ, ns := range nsOf {
			if target == ns+"."+typ {
				resolved = typ
			}
		}
		if resolved != "" {
			alias[name] = resolved
		} else if known[name] {
			shadowed[name] = true // using File = MyLib.File;
		}
	}
	// A namespace alias (`using IO = System.IO;`) makes IO.File mean System.IO.File. Only a namespace that is, or is a prefix of, one a rule
	// names is recorded: that is all a chain can need.
	knownNS := map[string]bool{}
	for _, ns := range nsOf {
		parts := strings.Split(ns, ".")
		for i := 1; i <= len(parts); i++ {
			knownNS[strings.Join(parts[:i], ".")] = true
		}
	}
	nsAlias := map[string][]string{}
	for _, m := range csUsingAlias.FindAllStringSubmatch(src, -1) {
		name, target := m[1], csJoinDots(csGlobalPfx.ReplaceAllString(m[2], ""))
		if knownNS[target] && !known[name] {
			nsAlias[name] = strings.Split(target, ".")
		}
	}
	staticOf := map[string]bool{} // BCL type whose members are imported bare
	for _, m := range csUsingStat.FindAllStringSubmatch(src, -1) {
		target := csJoinDots(csGlobalPfx.ReplaceAllString(m[1], ""))
		for typ, ns := range nsOf {
			if target == ns+"."+typ {
				staticOf[typ] = true
			}
		}
	}

	var out []csFinding
	seen := map[string]bool{}
	add := func(line, col int, r csRule, resolved string) {
		key := fmt.Sprintf("%s|%s|%d", r.code, resolved, line)
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, csFinding{line: line, col: col, code: r.code, message: r.message + " (" + resolved + ")", suggestion: r.suggestion})
	}
	refuses := func(r csRule, member string) bool {
		if len(r.members) == 0 {
			return true
		}
		for _, m := range r.members {
			if m == member {
				return true
			}
		}
		return false
	}

	for li, line := range lines {
		ln := li + 1

		// 3. dotted chains.
		for _, m := range csDottedRe.FindAllStringIndex(line, -1) {
			parts := strings.Split(csJoinDots(line[m[0]:m[1]]), ".")
			if exp, ok := nsAlias[parts[0]]; ok {
				parts = append(append([]string{}, exp...), parts[1:]...)
			}
			// A chain that is itself a member of something before it (`foo.Bar`), where the character before the match is '.', was split
			// by the regex's own start: skip it, it is the tail of a longer chain handled from its head.
			if m[0] > 0 && strings.TrimSpace(line[:m[0]]) != "" && strings.HasSuffix(strings.TrimRight(line[:m[0]], " \t"), ".") {
				continue
			}
			for i, p := range parts {
				typ := p
				if a, ok := alias[p]; ok {
					typ = a
				}
				if !known[typ] || shadowed[typ] {
					continue
				}
				prefix := strings.Join(parts[:i], ".")
				if prefix != "" && prefix != nsOf[typ] && prefix != "global::"+nsOf[typ] {
					break // reached through a value, not named as the BCL type
				}
				member := ""
				if i+1 < len(parts) {
					member = parts[i+1]
				}
				for _, ri := range csRuleFor(typ) {
					r := csharpRules[ri]
					// `new Thread(...)` is CV004 and is matched below; a bare `Thread` with no member is not a refusal.
					if len(r.members) > 0 && member == "" {
						continue
					}
					if refuses(r, member) {
						resolved := nsOf[typ] + "." + typ
						if member != "" {
							resolved += "." + member
						}
						add(ln, m[0]+1, r, resolved)
					}
				}
				break
			}
		}

		// 4. bare members imported by `using static`.
		for typ := range staticOf {
			if shadowed[typ] {
				continue
			}
			for _, ri := range csRuleFor(typ) {
				r := csharpRules[ri]
				for _, mem := range r.members {
					re := regexp.MustCompile(`(^|[^.\w])` + regexp.QuoteMeta(mem) + `\b`)
					if loc := re.FindStringSubmatchIndex(line); loc != nil {
						add(ln, loc[3]+1, r, nsOf[typ]+"."+typ+"."+mem+" (via using static)")
					}
				}
			}
		}

		// `new Thread(...)`.
		if !shadowed["Thread"] {
			for _, m := range csNewThread.FindAllStringSubmatchIndex(line, -1) {
				pfx := strings.TrimSuffix(csJoinDots(strings.TrimSpace(line[m[2]:m[3]])), ".")
				if pfx == "" || pfx == "System.Threading" || pfx == "global::System.Threading" {
					out = append(out, csFinding{line: ln, col: m[0] + 1, code: "CV004",
						message:    "threads cannot be created in a component: Thread.Start throws PlatformNotSupportedException (measured)",
						suggestion: "Workflow code is single-threaded by design"})
				}
			}
		}

		// A blocking wait. Matched by the call shape alone: a method called Wait on any receiver. See the header for the measured case
		// (Task.Wait on an incomplete task) and why .Result and GetAwaiter().GetResult() are NOT here: on a task that has already completed
		// they are ordinary and common, and a source check cannot tell the two apart.
		for _, m := range csWaitCall.FindAllStringIndex(line, -1) {
			key := fmt.Sprintf("CV003|Wait|%d", ln)
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, csFinding{line: ln, col: m[0] + 1, code: "CV003",
				message:    "a blocking Wait throws PlatformNotSupportedException on an incomplete task in a component (measured with Task.Wait)",
				suggestion: "await the task, or call the host (h.Call, h.Sleep) and await that"})
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

// csCatchRe finds the `catch` keyword. The clause that follows is parsed by hand because its parts nest (a filter holds parentheses, a
// body holds braces) and a regular expression cannot match a balanced pair.
var (
	csCatchRe   = regexp.MustCompile(`\bcatch\b`)
	csThrowRe   = regexp.MustCompile(`\bthrow\b`)
	csReturnRe  = regexp.MustCompile(`\breturn\b`)
	csSuspendRe = regexp.MustCompile(`(?i)suspend`)
)

// csMatchClose returns the index of the byte that closes the bracket opened at src[open], or -1. src is already blanked, so a bracket in a
// string or comment is not here to be counted.
func csMatchClose(src string, open int, o, c byte) int {
	depth := 0
	for i := open; i < len(src); i++ {
		switch src[i] {
		case o:
			depth++
		case c:
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

func csSkipSpace(src string, i int) int {
	for i < len(src) && (src[i] == ' ' || src[i] == '\t' || src[i] == '\n' || src[i] == '\r') {
		i++
	}
	return i
}

// findSwallowingCatches reports each catch that can swallow CleatSuspendException (see the header). It returns warnings.
func findSwallowingCatches(code []byte) []csFinding {
	src := string(code)
	var out []csFinding
	prevHandlesSuspend := false // the catch just before this one, in the same try, handles CleatSuspendException and rethrows
	prevEnd := -1
	for _, m := range csCatchRe.FindAllStringIndex(src, -1) {
		i := csSkipSpace(src, m[1])
		clause := ""
		if i < len(src) && src[i] == '(' {
			end := csMatchClose(src, i, '(', ')')
			if end < 0 {
				continue
			}
			clause = strings.TrimSpace(src[i+1 : end])
			i = csSkipSpace(src, end+1)
		}
		filter := ""
		if strings.HasPrefix(src[i:], "when") {
			j := csSkipSpace(src, i+4)
			if j < len(src) && src[j] == '(' {
				end := csMatchClose(src, j, '(', ')')
				if end < 0 {
					continue
				}
				filter = src[j+1 : end]
				i = csSkipSpace(src, end+1)
			}
		}
		if i >= len(src) || src[i] != '{' {
			continue
		}
		bodyEnd := csMatchClose(src, i, '{', '}')
		if bodyEnd < 0 {
			continue
		}
		body := src[i : bodyEnd+1]
		rethrows := csThrowRe.MatchString(body)

		typ := ""
		if f := strings.Fields(clause); len(f) > 0 {
			typ = strings.TrimPrefix(csGlobalPfx.ReplaceAllString(f[0], ""), "System.")
		}
		// prevEnd can lie AFTER this catch: a try nested inside the previous catch's body puts its own catch inside that body.
		adjacent := prevEnd >= 0 && prevEnd < m[0] && strings.TrimSpace(src[prevEnd+1:m[0]]) == ""
		// A handler for the suspension is a catch of CleatSuspendException itself, or a filter that names it or a helper with "suspend" in
		// its name (the SDK's own catches use ExceptionInfo.IsSuspend).
		isSuspend := typ == "CleatSuspendException" || typ == "Cleat.CleatSuspendException" || csSuspendRe.MatchString(filter)
		catchesAll := clause == "" || typ == "Exception"
		safe := rethrows || csSuspendRe.MatchString(filter) || (adjacent && prevHandlesSuspend)
		if catchesAll && !safe {
			line := strings.Count(src[:m[0]], "\n") + 1
			col := m[0] - (strings.LastIndex(src[:m[0]], "\n") + 1) + 1
			out = append(out, csFinding{line: line, col: col, code: "CV101", warning: true,
				message:    "this catch can swallow CleatSuspendException: a host call made after it still reaches the service and is recorded, past the point the host told the workflow to stop (measured)",
				suggestion: "Rethrow it: catch (Exception e) when (e is not CleatSuspendException), or catch (CleatSuspendException) { throw; } before this catch"})
		}
		// The handler ends the method or rethrows: the suspension does not fall through to the code after the try.
		prevHandlesSuspend = isSuspend && (rethrows || csReturnRe.MatchString(body))
		prevEnd = bodyEnd
	}
	return out
}

// csharpSourceFiles lists the .cs files under projectDir, skipping bin, obj and hidden directories (the build's own output, which holds
// generated sources that are not the author's). It returns them sorted so a run is reproducible.
func csharpSourceFiles(projectDir string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(projectDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if path != projectDir && (name == "bin" || name == "obj" || strings.HasPrefix(name, ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".cs") {
			files = append(files, path)
		}
		return nil
	})
	sort.Strings(files)
	return files, err
}

// runVetCSharp vets a C# project directory and returns the exit status, with the three the repository's checks keep:
//
//	0  no refused reach found
//	1  a finding: the project reaches something a component cannot run or cannot replay
//	2  UNMEASURED: the vet could not read the project (no directory, no .cs file, a file it could not open). This is a failure of the check,
//	   not a finding about the project, and it must not read as a pass: a vet that read nothing agrees with every tree.
//
// Findings go to out, as text or as the JSON the other vets emit; progress and every UNMEASURED message go to diag. The build gate passes
// its stderr for both; `cleat vet` passes stdout and stderr.
func runVetCSharp(projectDir string, jsonOutput bool, out, diag io.Writer) int {
	if projectDir == "" {
		projectDir = "."
	}
	if fi, err := os.Stat(projectDir); err != nil || !fi.IsDir() {
		fmt.Fprintf(diag, "UNMEASURED: %s is not a directory the C# vet can read.\n  This is a failure of the check, not a finding about the project.\n", projectDir)
		return 2
	}
	files, err := csharpSourceFiles(projectDir)
	if err != nil {
		fmt.Fprintf(diag, "UNMEASURED: could not list the sources of %s: %v\n  This is a failure of the check, not a finding about the project.\n", projectDir, err)
		return 2
	}
	if len(files) == 0 {
		fmt.Fprintf(diag, "UNMEASURED: no .cs source file under %s, so there was nothing to vet.\n  This is a failure of the check, not a finding about the project.\n", projectDir)
		return 2
	}
	fmt.Fprintf(diag, "Vetting C# project in %s (%d source files)...\n", projectDir, len(files))

	var output VetOutput
	output.Summary.Functions = len(files)
	hasEntry := false
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			fmt.Fprintf(diag, "UNMEASURED: could not read %s: %v\n  This is a failure of the check, not a finding about the project.\n", f, err)
			return 2
		}
		rel, _ := filepath.Rel(projectDir, f)
		code := csharpCodeOnly(data)
		if strings.Contains(string(code), "CleatEntry") {
			hasEntry = true
		}
		for _, fnd := range findForbiddenCSharp(code) {
			output.Errors = append(output.Errors, VetResult{Code: fnd.code, File: rel, Line: fnd.line, Column: fnd.col, Message: fnd.message, Suggestion: fnd.suggestion})
		}
		for _, fnd := range findSwallowingCatches(code) {
			output.Warnings = append(output.Warnings, VetResult{Code: fnd.code, File: rel, Line: fnd.line, Column: fnd.col, Message: fnd.message, Suggestion: fnd.suggestion})
		}
	}
	if !hasEntry {
		output.Warnings = append(output.Warnings, VetResult{Code: "CV100",
			Message:    "no [CleatEntry] attribute found in any source file",
			Suggestion: "Annotate a public static method with [CleatEntry(\"name\")]: public static JsonNode Run(HostCalls h, JsonElement input)"})
	}

	if jsonOutput {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(output); err != nil {
			fmt.Fprintf(diag, "UNMEASURED: could not encode the vet result: %v\n", err)
			return 2
		}
	} else {
		for _, e := range output.Errors {
			fmt.Fprintf(out, "  Error [%s] %s:%d:%d: %s\n", e.Code, e.File, e.Line, e.Column, e.Message)
			if e.Suggestion != "" {
				fmt.Fprintf(out, "    suggestion: %s\n", e.Suggestion)
			}
		}
		for _, w := range output.Warnings {
			if w.File != "" {
				fmt.Fprintf(out, "  Warning [%s] %s:%d:%d: %s\n", w.Code, w.File, w.Line, w.Column, w.Message)
			} else {
				fmt.Fprintf(out, "  Warning [%s] %s\n", w.Code, w.Message)
			}
			if w.Suggestion != "" {
				fmt.Fprintf(out, "    suggestion: %s\n", w.Suggestion)
			}
		}
		fmt.Fprintf(out, "\n  Summary: %d files, %d errors, %d warnings\n", output.Summary.Functions, len(output.Errors), len(output.Warnings))
	}
	if len(output.Errors) > 0 {
		return 1
	}
	return 0
}
