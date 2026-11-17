package typescript

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Conformance with Node (cleat#3829). The guest's JavaScript engine is QuickJS; where it differs from Node the answer is not
// an error but a plausible wrong value. Each test here runs ONE source in both places and compares what each prints, so the
// oracle is Node itself and not a belief about Node.
//
// The source is plain JavaScript in a .ts file (testdata/conformance/*.ts): the guest runs the module build.sh makes of it, and
// Node runs the same source bundled by esbuild with TZ=UTC (the guest has no time zone, so its local time is UTC).

// nodeOracle bundles src for Node and returns the string array the entry point passes to __cleat_result.
func nodeOracle(t *testing.T, root, src, entry string) []string {
	t.Helper()
	dir := t.TempDir()
	bundle := filepath.Join(dir, "bundle.js")
	esbuild := filepath.Join(root, "packages", "cleat-ts", "node_modules", ".bin", "esbuild")
	if out, err := exec.Command(esbuild, filepath.Join(root, src), "--bundle", "--format=cjs", "--platform=node",
		"--outfile="+bundle, "--log-level=warning").CombinedOutput(); err != nil {
		t.Fatalf("bundling %s for Node: %v\n%s", src, err, out)
	}
	cmd := exec.Command("node", "-e",
		"globalThis.__cleat_result = (s) => process.stdout.write(s); require(process.argv[1])."+entry+"()", bundle)
	cmd.Env = append(os.Environ(), "TZ=UTC")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("running %s in Node: %v\n%s", src, err, out)
	}
	var lines []string
	if err := json.Unmarshal(out, &lines); err != nil {
		t.Fatalf("Node printed something that is not a JSON array of strings: %v\n%.300s", err, out)
	}
	return lines
}

// guestResult builds src with build.sh, runs the entry in the same in-process engine the numbers test uses, and returns the string
// array it passes to __cleat_result.
func guestResult(t *testing.T, root, src, entry string, env ...string) []string {
	t.Helper()
	res := build(t, root, src, entry, env...)
	if res.exit != 0 {
		t.Fatalf("building %s failed (exit %d):\n%s", src, res.exit, res.output)
	}
	wasm, err := os.ReadFile(res.module)
	if err != nil {
		t.Fatal(err)
	}
	e, closeRT := newBenchEngine(t)
	defer closeRT()
	result, _, susp, _, _, err := e.Execute(context.Background(), wasm, entry, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("running %s in the guest: %v", src, err)
	}
	if susp != nil {
		t.Fatalf("%s suspended (%s); a conformance case must not wait for anything", src, susp.Reason)
	}
	var lines []string
	if err := json.Unmarshal([]byte(result), &lines); err != nil {
		t.Fatalf("the guest returned something that is not a JSON array of strings: %v\n%.300s", err, result)
	}
	return lines
}

// compareLines fails with the number of differing cases and the first few, each shown as the guest's line and Node's.
func compareLines(t *testing.T, what string, guest, node []string) {
	t.Helper()
	if len(guest) != len(node) {
		t.Fatalf("%s: the guest printed %d cases and Node %d, so they ran different code", what, len(guest), len(node))
	}
	if len(node) == 0 {
		t.Fatalf("%s: no cases ran, so nothing was compared", what)
	}
	var diffs []string
	for i := range node {
		if guest[i] != node[i] {
			diffs = append(diffs, fmt.Sprintf("  guest: %s\n  node:  %s", guest[i], node[i]))
		}
	}
	if len(diffs) > 0 {
		shown := diffs
		if len(shown) > 6 {
			shown = shown[:6]
		}
		t.Errorf("%s: %d of %d cases differ from Node (first %d):\n%s", what, len(diffs), len(node), len(shown), strings.Join(shown, "\n"))
	}
}

// TestTypeScriptDateSettersAgreeWithNode is the first silent divergence: QuickJS answers NaN to a Date setter given a
// non-finite argument but leaves the date unchanged, where the specification makes it an Invalid Date. date-fns builds its dates
// with setUTCFullYear, so `isValid(parseISO("nope"))` was true in the guest. Every setter, over non-finite, out-of-range and valid
// arguments, on a valid, an epoch and an already-invalid receiver, and with several arguments.
func TestTypeScriptDateSettersAgreeWithNode(t *testing.T) {
	root := needToolchain(t)
	src := "tests/typescript/testdata/conformance/dates.ts"
	compareLines(t, "Date setters", guestResult(t, root, src, "dates"), nodeOracle(t, root, src, "dates"))
}

// needNodeAtLeast fails, not skips, when the oracle is older than the corpus was written against: the toolchain is declared, so a
// missing capability is a failure of the setup. Node 20's URL parser predates two changes to the specification (it leaves "^"
// unencoded in a path, and it drops the root slash of a non-special URL whose path ".." climbs above the root), so against it the
// corpus reports differences that are the oracle's age and not the shim's.
func needNodeAtLeast(t *testing.T, major int) {
	t.Helper()
	out, err := exec.Command("node", "-p", "process.versions.node").Output()
	if err != nil {
		t.Fatalf("node is needed as the oracle: %v", err)
	}
	v := strings.TrimSpace(string(out))
	got, err := strconv.Atoi(strings.SplitN(v, ".", 2)[0])
	if err != nil {
		t.Fatalf("cannot read the Node version %q", v)
	}
	if got < major {
		t.Fatalf("the URL corpus needs Node %d or newer as its oracle (older URL parsers disagree with the current specification); this is Node %s", major, v)
	}
}

// TestTypeScriptURLAgreesWithNode is the second silent divergence: QuickJS has no URL, so `new URL(x)` was a ReferenceError and
// zod's z.url(), which calls it inside a try/catch, reported every URL invalid. The shim is the WHATWG algorithm for what can be
// done exactly. Every getter of every result, over absolute inputs, a relative-resolution grid against twelve bases, the two
// setters it supports, and the way zod decides.
func TestTypeScriptURLAgreesWithNode(t *testing.T) {
	root := needToolchain(t)
	needNodeAtLeast(t, 24)
	src := "tests/typescript/testdata/conformance/urls.ts"
	compareLines(t, "URL", guestResult(t, root, src, "urls"), nodeOracle(t, root, src, "urls"))
}

// TestTypeScriptURLSearchParamsAgreeWithNode covers every constructor form, every method, iteration (including mutation during
// it), invalid UTF-8 in percent-escapes, and the sort.
func TestTypeScriptURLSearchParamsAgreeWithNode(t *testing.T) {
	root := needToolchain(t)
	needNodeAtLeast(t, 24)
	src := "tests/typescript/testdata/conformance/searchparams.ts"
	compareLines(t, "URLSearchParams", guestResult(t, root, src, "search_params"), nodeOracle(t, root, src, "searchParams")) // the guest takes the ABI name, Node calls the function
}

// TestTypeScriptURLRefusesWhatItCannotAnswer is the other half of the contract. The shim does not guess: an input or an operation
// outside what it can do exactly must throw its own error, never return a value, because a plausible wrong value is the failure
// the shim exists to remove. Node answers all of these, so only the guest runs it, and every line must read "loud".
func TestTypeScriptURLRefusesWhatItCannotAnswer(t *testing.T) {
	root := needToolchain(t)
	lines := guestResult(t, root, "tests/typescript/testdata/conformance/urls_loud.ts", "urls_loud")
	if len(lines) == 0 {
		t.Fatal("no cases ran, so nothing was checked")
	}
	for _, l := range lines {
		if !strings.HasPrefix(l, "loud: ") {
			t.Errorf("%s", l)
		}
	}
}

// TestTypeScriptURLFileURLsAgreeWithNode holds the file: rules: the empty host and localhost folding, a Windows drive letter
// kept in the path and normalised from "|" to ":", ".." never removing it, a relative path inheriting a file base's drive
// letter, and backslash separators. cleat-review's probe on the first version of the handler found nineteen silent wrong
// answers, every one of them a shape in this matrix (cleat#3898).
func TestTypeScriptURLFileURLsAgreeWithNode(t *testing.T) {
	root := needToolchain(t)
	needNodeAtLeast(t, 24)
	src := "tests/typescript/testdata/conformance/urls_file.ts"
	compareLines(t, "file: URLs", guestResult(t, root, src, "urls_file"), nodeOracle(t, root, src, "urlsFile"))
}

// TestTypeScriptURLFileSpecShortening pins the one file: rule where Node's own versions disagree, so the corpus cannot be
// its arbiter: shortening a path whose only segment starts with a drive letter but is longer than two code points. The
// specification pops `C:x`; Node 24 (which CI pins) agrees, Node 26 keeps it. The expected strings are literals, not
// Node's answers, and DIVERGENCES.md row 2 records why (cleat#3898).
func TestTypeScriptURLFileSpecShortening(t *testing.T) {
	root := needToolchain(t)
	src := "tests/typescript/testdata/conformance/urls_file.ts"
	got := guestResult(t, root, src, "urls_file_spec")
	want := []string{
		"file:///C:x/.. -> file:///",
		"file:///C:x/../y -> file:///y",
		"file:///h:1/.. -> file:///",
		"file:///a:/.. -> file:///a:/",
		"file:///C:/.. -> file:///C:/",
		"file:///C:/a/.. -> file:///C:/",
	}
	if len(got) != len(want) {
		t.Fatalf("the guest answered %d cases, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("case %d: guest %q, want %q", i, got[i], want[i])
		}
	}
}
// parseRanges reads the sweep's compact range list ("9-a,d,21-24") into pairs.
func parseRanges(t *testing.T, s string) [][2]int {
	t.Helper()
	if s == "" {
		return nil
	}
	var out [][2]int
	for _, r := range strings.Split(s, ",") {
		a, b, ok := strings.Cut(r, "-")
		if !ok {
			b = a
		}
		lo, err := strconv.ParseInt(a, 16, 32)
		if err != nil {
			t.Fatalf("range %q: %v", r, err)
		}
		hi, err := strconv.ParseInt(b, 16, 32)
		if err != nil {
			t.Fatalf("range %q: %v", r, err)
		}
		out = append(out, [2]int{int(lo), int(hi)})
	}
	return out
}

func inRanges(rs [][2]int, c int) bool {
	lo, hi := 0, len(rs)-1
	for lo <= hi {
		m := (lo + hi) / 2
		if c < rs[m][0] {
			hi = m - 1
		} else if c > rs[m][1] {
			lo = m + 1
		} else {
			return true
		}
	}
	return false
}

// sweepSamples is the code-point set urls_sweep.ts measures: the BMP, then every 61st code point above it. Above the BMP
// only a sample is measured, because the whole range takes longer than one guest execution is allowed.
func sweepSamples() []int {
	var cp []int
	for c := 0; c <= 0xffff; c++ {
		cp = append(cp, c)
	}
	for c := 0x10000; c < 0x110000; c += 61 {
		cp = append(cp, c)
	}
	return cp
}

// TestTypeScriptURLHostCodePointSweep holds the IDNA claim of DIVERGENCES.md row 2 at code-point level. It asks, for every
// sampled code point placed in a host label (raw and already Punycode-encoded), whether the guest and Node accept it. A
// code point the guest ACCEPTS where Node refuses is a silent divergence and fails, because a catch-all such as zod's
// z.url() would report an invalid URL as valid. A code point the guest REFUSES where Node accepts is loud: it is reported,
// with a ceiling, but it does not fail.
func TestTypeScriptURLHostCodePointSweep(t *testing.T) {
	root := needToolchain(t)
	needNodeAtLeast(t, 24)
	src := "tests/typescript/testdata/conformance/urls_sweep.ts"
	guest := guestResult(t, root, src, "urls_sweep")
	node := nodeOracle(t, root, src, "urlsSweep")
	if len(guest) != 3 || len(node) != 3 {
		t.Fatalf("expected three strings (raw, xn--, count), got %d and %d", len(guest), len(node))
	}
	samples := sweepSamples()
	if guest[2] != node[2] {
		t.Fatalf("the two runs swept different sets: guest %s code points, Node %s", guest[2], node[2])
	}
	if guest[2] != strconv.Itoa(len(samples)) {
		t.Fatalf("the fixture swept %s code points and this test expects %d", guest[2], len(samples))
	}
	// A ceiling, not an expectation: these are the classes measured on 2026-11-17 (Node 26.4.0), where the guest refuses a
	// Punycode label Node accepts because the engine's case tables are older than ICU's. More than the ceiling is a new
	// class and wants a look; fewer is an improvement and should lower it.
	const loudCeiling = 300
	for i, form := range []string{"raw", "xn--"} {
		gr, nr := parseRanges(t, guest[i]), parseRanges(t, node[i])
		var silent, loud []int
		for _, cp := range samples {
			if cp >= 0xd800 && cp <= 0xdfff {
				continue
			}
			a, b := inRanges(gr, cp), inRanges(nr, cp)
			if a && !b {
				if len(silent) < 20 {
					silent = append(silent, cp)
				}
			} else if b && !a {
				loud = append(loud, cp)
			}
		}
		if len(silent) > 0 {
			t.Errorf("%s: the guest accepts %d code point(s) Node refuses (showing up to 20): %v -- a silent divergence", form, len(silent), silent)
		}
		if len(loud) > loudCeiling {
			t.Errorf("%s: the guest refuses %d code points Node accepts, over the ceiling of %d: %v", form, len(loud), loudCeiling, loud[:20])
		}
		t.Logf("%s: %d silent, %d loud (ceiling %d): %v", form, len(silent), len(loud), loudCeiling, loud)
	}
}
