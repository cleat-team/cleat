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
		if l != "loud" {
			t.Errorf("%s", l)
		}
	}
}
