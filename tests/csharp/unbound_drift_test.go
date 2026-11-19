package csharp

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The C# SDK wires the WIT world's functions in groups (cleat#4080, PR 5), and "every function is either wired or listed as
// deliberately unbound, checked by a test so it cannot drift" is that PR's Done-when. This is the test.
//
// It parses python-sdk/wit/cleat.wit, scans packages/cleat-csharp/src for the generated import each function becomes, and requires
//
//	unwired == the union of tests/csharp/unbound.d/<group>.txt
//
// One list file per group, not one shared list: a group deletes its own file in the PR that wires its functions, so no two groups edit
// the same file (the scripts/skip-ledger.d convention). The test then fails in both directions: a function wired but still listed, and a
// function neither wired nor listed.
//
// WHAT COUNTS AS WIRED. A call spelled <Interface>Imports.<PascalCaseName>(...) in code. Comments and string literals are stripped
// first, because a doc comment that says "DurableSend is not wired yet" contains the very text a scan looks for: a text search cannot
// tell a thing from a sentence about the thing. The stripper has its own known-positive and negative controls below.

const witRelPath = "python-sdk/wit/cleat.wit"

type witFunc struct{ iface, name string }

var (
	witInterfaceRe = regexp.MustCompile(`^interface ([a-z0-9-]+) \{`)
	witFuncRe      = regexp.MustCompile(`^    ([a-z0-9-]+): func`)
)

func parseWITFuncs(t *testing.T, src string) []witFunc {
	t.Helper()
	var out []witFunc
	cur := ""
	for _, l := range strings.Split(src, "\n") {
		if m := witInterfaceRe.FindStringSubmatch(l); m != nil {
			cur = m[1]
			continue
		}
		if l == "}" {
			cur = ""
			continue
		}
		if m := witFuncRe.FindStringSubmatch(l); m != nil && cur != "" {
			out = append(out, witFunc{cur, m[1]})
		}
	}
	return out
}

// pascal turns a WIT name into the generated C# member name: durable-call-retry -> DurableCallRetry.
func pascal(name string) string {
	var b strings.Builder
	for _, p := range strings.Split(name, "-") {
		if p != "" {
			b.WriteString(strings.ToUpper(p[:1]) + p[1:])
		}
	}
	return b.String()
}

// stripCSharp removes // and /* */ comments and "..." / @"..." / '.' literals, leaving everything else, so a scan sees code only.
func stripCSharp(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		switch {
		case strings.HasPrefix(s[i:], "//"):
			for i < len(s) && s[i] != '\n' {
				i++
			}
		case strings.HasPrefix(s[i:], "/*"):
			j := strings.Index(s[i+2:], "*/")
			if j < 0 {
				return b.String() // unterminated: nothing after it is code
			}
			i += 2 + j + 2
		case strings.HasPrefix(s[i:], `"""`):
			j := strings.Index(s[i+3:], `"""`)
			if j < 0 {
				return b.String()
			}
			i += 3 + j + 3
		case strings.HasPrefix(s[i:], `@"`) || strings.HasPrefix(s[i:], `$@"`) || strings.HasPrefix(s[i:], `@$"`):
			i = strings.IndexByte(s[i:], '"') + i + 1
			for i < len(s) {
				if s[i] == '"' {
					if i+1 < len(s) && s[i+1] == '"' { // "" is an escaped quote in a verbatim string
						i += 2
						continue
					}
					i++
					break
				}
				i++
			}
		case s[i] == '"' || (s[i] == '$' && i+1 < len(s) && s[i+1] == '"'):
			if s[i] == '$' {
				i++
			}
			i++
			for i < len(s) && s[i] != '"' && s[i] != '\n' {
				if s[i] == '\\' {
					i++
				}
				i++
			}
			i++
		case s[i] == '\'':
			i++
			for i < len(s) && s[i] != '\'' && s[i] != '\n' {
				if s[i] == '\\' {
					i++
				}
				i++
			}
			i++
		default:
			b.WriteByte(s[i])
			i++
		}
	}
	return b.String()
}

// wiredFuncs returns the WIT function names that code in the given sources calls through a generated import.
func wiredFuncs(funcs []witFunc, sources map[string]string) map[string]string {
	code := map[string]string{}
	for p, s := range sources {
		code[p] = stripCSharp(s)
	}
	wired := map[string]string{}
	for _, f := range funcs {
		// A trailing \b keeps DurableCall from matching inside DurableCallRetry.
		re := regexp.MustCompile(`\bI[A-Za-z]+Imports\.` + regexp.QuoteMeta(pascal(f.name)) + `\b`)
		for p, c := range code {
			if re.MatchString(c) {
				wired[f.name] = p
				break
			}
		}
	}
	return wired
}

func readCSharpSources(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	base := filepath.Join(root, "packages", "cleat-csharp", "src")
	err := filepath.Walk(base, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && strings.HasSuffix(p, ".cs") {
			b, rerr := os.ReadFile(p)
			if rerr != nil {
				return rerr
			}
			rel, _ := filepath.Rel(root, p)
			out[rel] = string(b)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("reading the C# sources: %v", err)
	}
	if len(out) == 0 {
		t.Fatalf("UNMEASURED: no .cs file under %s, so nothing below was measured", base)
	}
	return out
}

// readUnboundLists returns function name -> the list file that names it. A line is a WIT function name; # starts a comment.
func readUnboundLists(t *testing.T, dir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	listed := map[string]string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".txt") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for n, l := range strings.Split(string(b), "\n") {
			l = strings.TrimSpace(strings.SplitN(l, "#", 2)[0])
			if l == "" {
				continue
			}
			if prev, dup := listed[l]; dup {
				t.Errorf("%s:%d: %q is already listed in %s: a function belongs to exactly one group", e.Name(), n+1, l, prev)
				continue
			}
			listed[l] = e.Name()
		}
	}
	return listed
}

func TestCSharpEveryWITFunctionIsWiredOrListedAsUnbound(t *testing.T) {
	root := repoRoot(t)
	witBytes, err := os.ReadFile(filepath.Join(root, witRelPath))
	if err != nil {
		t.Fatal(err)
	}
	funcs := parseWITFuncs(t, string(witBytes))
	if len(funcs) == 0 {
		t.Fatalf("UNMEASURED: no function parsed from %s", witRelPath)
	}
	seen := map[string]bool{}
	names := map[string]bool{}
	for _, f := range funcs {
		if seen[f.name] {
			t.Fatalf("the function name %q appears in two interfaces: a scan keyed on the name cannot tell them apart", f.name)
		}
		seen[f.name] = true
		names[f.name] = true
	}

	wired := wiredFuncs(funcs, readCSharpSources(t, root))
	listed := readUnboundLists(t, filepath.Join(root, "tests", "csharp", "unbound.d"))

	var neither, both, unknown []string
	for _, f := range funcs {
		_, w := wired[f.name]
		_, l := listed[f.name]
		switch {
		case !w && !l:
			neither = append(neither, f.iface+"."+f.name)
		case w && l:
			both = append(both, f.name+" (listed in "+listed[f.name]+")")
		}
	}
	for n, file := range listed {
		if !names[n] {
			unknown = append(unknown, n+" (in "+file+")")
		}
	}
	sort.Strings(neither)
	sort.Strings(both)
	sort.Strings(unknown)

	if len(neither) > 0 {
		t.Errorf("%d WIT function(s) are neither wired in packages/cleat-csharp/src nor listed in tests/csharp/unbound.d/:\n  %s\n\n"+
			"Wire them, or list each in its group's file. A function must never be silently absent.", len(neither), strings.Join(neither, "\n  "))
	}
	if len(both) > 0 {
		t.Errorf("%d function(s) are wired AND still listed as unbound:\n  %s\n\n"+
			"Delete the line from its group's file in the PR that wires the function; the group deletes the whole file when it is empty.",
			len(both), strings.Join(both, "\n  "))
	}
	if len(unknown) > 0 {
		t.Errorf("%d listed name(s) are not functions in %s (a typo, or a function that was removed):\n  %s", len(unknown), witRelPath, strings.Join(unknown, "\n  "))
	}
	t.Logf("WIT functions: %d; wired: %d; listed as unbound: %d", len(funcs), len(wired), len(listed))
}

// The scan's own controls. A guard that has only ever passed a tree it should pass has not been shown to be able to fail.
func TestCSharpTheWiredScanSeesCodeAndNotSentencesAboutCode(t *testing.T) {
	funcs := []witFunc{{"durable-call", "durable-call"}, {"durable-call", "durable-call-retry"}, {"durable-messaging", "durable-send"}}
	src := map[string]string{
		"a.cs": `
// DurableSend is not wired yet: IDurableMessagingImports.DurableSend(x) would go here.
/* IDurableMessagingImports.DurableSend( in a block comment */
var s = "IDurableMessagingImports.DurableSend(";
var v = @"IDurableMessagingImports.DurableSend(""x"")";
var r = IDurableCallImports.DurableCall("a", "b", c);
`,
	}
	got := wiredFuncs(funcs, src)
	if _, ok := got["durable-call"]; !ok {
		t.Errorf("a real call was not seen: %v", got)
	}
	if _, ok := got["durable-send"]; ok {
		t.Errorf("a comment or a string literal was counted as wiring: %v", got)
	}
	if _, ok := got["durable-call-retry"]; ok {
		t.Errorf("DurableCall matched inside DurableCallRetry: the trailing \\b is missing: %v", got)
	}
}

func TestCSharpPascalMatchesTheGeneratedNames(t *testing.T) {
	for in, want := range map[string]string{
		"durable-call":              "DurableCall",
		"durable-call-retry":        "DurableCallRetry",
		"set-scope":                 "SetScope",
		"uuid":                      "Uuid",
		"continue-as-new-versioned": "ContinueAsNewVersioned",
	} {
		if got := pascal(in); got != want {
			t.Errorf("pascal(%q) = %q, want %q", in, got, want)
		}
	}
}
