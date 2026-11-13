package main

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

type finding struct {
	line, col  int
	code, name string
	reason     string
}

type result struct {
	findings   []finding
	allowed    []string // codes a marker suppressed, with its stated reason
	tokens     int
	unmeasured string
}

// rule: a global name the Javy guest does not define, and why it is refused.
type rule struct{ code, reason string }

var rules = map[string]rule{
	"setTimeout":           {"TS001", "timers are not part of this runtime; use ctx.sleep (a durable sleep)"},
	"setInterval":          {"TS001", "timers are not part of this runtime; use ctx.sleep in a loop"},
	"setImmediate":         {"TS001", "timers are not part of this runtime"},
	"clearTimeout":         {"TS001", "timers are not part of this runtime"},
	"clearInterval":        {"TS001", "timers are not part of this runtime"},
	"WeakRef":              {"TS002", "exposes garbage-collection timing, which differs between a run and its replay"},
	"FinalizationRegistry": {"TS002", "exposes garbage-collection timing, which differs between a run and its replay"},
	"performance":          {"TS003", "an ambient clock outside the recorded host calls; use ctx.now()"},
	"crypto":               {"TS003", "ambient entropy outside the recorded host calls; use ctx.random() or ctx.uuid()"},
	"SharedArrayBuffer":    {"TS004", "shared memory and threads have no deterministic replay"},
	"Atomics":              {"TS004", "shared memory and threads have no deterministic replay"},
	"fetch":                {"TS005", "ambient network I/O is not recorded; call a plugin through the host"},
	"XMLHttpRequest":       {"TS005", "ambient network I/O is not recorded; call a plugin through the host"},
	"WebSocket":            {"TS005", "ambient network I/O is not recorded"},
	"process":              {"TS005", "no process environment in this runtime"},
	"require":              {"TS005", "no module loader at run time; imports are bundled"},
	"Intl":                 {"TS006", "locale and timezone data are host-dependent"},
}

// qualifiers: `globalThis.setTimeout` is a reference to the global; `someObject.setTimeout` is a method call on something else.
var qualifiers = map[string]bool{"globalThis": true, "self": true, "window": true, "global": true}

var allowRe = regexp.MustCompile(`cleat-ts:\s*allow\s+((?:TS\d{3}[ ,]*)+)\s*--\s*(\S.*)`)

func isIdentStart(c byte) bool {
	return c == '_' || c == '$' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}
func isIdentPart(c byte) bool { return isIdentStart(c) || (c >= '0' && c <= '9') }

// checkFile tokenises just enough TypeScript to tell code from comments, strings and template text, and looks at the
// identifiers in code. It does not parse: see main.go for what that leaves unseen. Regex literals are NOT recognised, so a
// quote inside one can desynchronise the scan; an unterminated string, template or comment at end of file is reported as
// UNMEASURED so that desynchronisation cannot silently hide a finding behind a swallowed remainder.
func checkFile(path string) result {
	raw, err := os.ReadFile(path)
	if err != nil {
		return result{unmeasured: "cannot read the file: " + err.Error()}
	}
	src := string(raw)
	var res result
	allowed := map[string]string{}
	line, col := 1, 1
	// state stack: 'c' code, '`' template text, '{' code inside ${ }
	stack := []byte{'c'}
	var prevSig byte     // previous significant (non-space) byte in code
	var prevIdent string // the identifier immediately before a '.', if any
	pendingDot := false  // the last significant token was a '.' that followed prevIdent
	lastIdent := ""
	advance := func(c byte) {
		if c == '\n' {
			line++
			col = 1
		} else {
			col++
		}
	}
	i := 0
	for i < len(src) {
		c := src[i]
		top := stack[len(stack)-1]
		if top == '`' { // template text
			switch {
			case c == '\\' && i+1 < len(src):
				advance(c)
				i++
				advance(src[i])
				i++
			case c == '`':
				stack = stack[:len(stack)-1]
				advance(c)
				i++
				prevSig = '`'
			case c == '$' && i+1 < len(src) && src[i+1] == '{':
				stack = append(stack, '{')
				advance(c)
				i++
				advance(src[i])
				i++
				prevSig = '{'
			default:
				advance(c)
				i++
			}
			continue
		}
		// code
		switch {
		case c == '/' && i+1 < len(src) && src[i+1] == '/':
			j := i
			for j < len(src) && src[j] != '\n' {
				j++
			}
			if m := allowRe.FindStringSubmatch(src[i:j]); m != nil {
				for _, code := range regexp.MustCompile(`TS\d{3}`).FindAllString(m[1], -1) {
					allowed[code] = strings.TrimSpace(m[2])
				}
			}
			for ; i < j; i++ {
				advance(src[i])
			}
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			end := strings.Index(src[i+2:], "*/")
			if end < 0 {
				return result{unmeasured: fmt.Sprintf("unterminated block comment at %d:%d swallows the rest of the file", line, col)}
			}
			for k := 0; k < end+4; k++ {
				advance(src[i+k])
			}
			i += end + 4
		case c == '"' || c == '\'':
			q := c
			sl, sc := line, col
			advance(c)
			i++
			closed := false
			for i < len(src) {
				d := src[i]
				if d == '\\' && i+1 < len(src) {
					advance(d)
					i++
					advance(src[i])
					i++
					continue
				}
				if d == '\n' {
					break
				}
				advance(d)
				i++
				if d == q {
					closed = true
					break
				}
			}
			if !closed {
				return result{unmeasured: fmt.Sprintf("unterminated string starting at %d:%d", sl, sc)}
			}
			prevSig = q
		case c == '`':
			stack = append(stack, '`')
			advance(c)
			i++
		case c == '{' && top == '{':
			stack = append(stack, '{')
			advance(c)
			i++
			prevSig = '{'
		case c == '}' && top == '{':
			stack = stack[:len(stack)-1]
			advance(c)
			i++
			prevSig = '}'
		case isIdentStart(c):
			j, sl, sc := i, line, col
			for j < len(src) && isIdentPart(src[j]) {
				j++
			}
			name := src[i:j]
			for k := i; k < j; k++ {
				advance(src[k])
			}
			i = j
			res.tokens++
			if r, ok := rules[name]; ok {
				member := pendingDot
				flag := true
				if member && !qualifiers[prevIdent] {
					flag = false // obj.setTimeout(...): a method on something else
				}
				if !member {
					// an object key `{ crypto: 1 }` or `, crypto:` is a property name, not a reference
					k := i
					for k < len(src) && (src[k] == ' ' || src[k] == '\t') {
						k++
					}
					if k < len(src) && src[k] == ':' && (prevSig == '{' || prevSig == ',') {
						flag = false
					}
				}
				if flag {
					res.findings = append(res.findings, finding{sl, sc, r.code, name, r.reason})
				}
			}
			lastIdent = name
			prevIdent = name
			pendingDot = false
			prevSig = 'a'
		case c == '.':
			advance(c)
			i++
			pendingDot = true
			prevIdent = lastIdent
			prevSig = '.'
		case c == ' ' || c == '\t' || c == '\r' || c == '\n':
			advance(c)
			i++
		default:
			advance(c)
			i++
			prevSig = c
			pendingDot = false
			lastIdent = ""
		}
	}
	if len(stack) != 1 {
		return result{unmeasured: "unterminated template literal at end of file"}
	}
	if res.tokens == 0 {
		return result{unmeasured: "no identifier tokens found: an empty or unreadable file is not a clean one"}
	}
	// An allow marker suppresses its code only with a stated reason (the regex requires `-- <reason>`); the suppression is reported.
	kept := res.findings[:0]
	for _, f := range res.findings {
		if reason, ok := allowed[f.code]; ok {
			res.allowed = append(res.allowed, fmt.Sprintf("%s (%s)", f.code, reason))
			continue
		}
		kept = append(kept, f)
	}
	res.findings = kept
	res.allowed = dedupe(res.allowed)
	return res
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
