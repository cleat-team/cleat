package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestASBuildRefusesNondeterminism asserts that `cleat build --target
// assemblyscript` refuses a workflow with a determinism violation, and records
// in the same run what the checker does not see.
//
// # There is no production change here, and that is the finding
//
// cleat#1770 says four of five guest languages reach a deployable artifact
// with no determinism checking. That is true of Rust (#1784), Java (#1791) and
// Python. It is NOT true of AssemblyScript, and the reason is better than the
// arrangement those two PRs had to build.
//
// runBuildAssemblyScript already passes `--transform @cleat/transform` to asc,
// and the transform's E001-E005 throw from afterParse. So the determinism
// check is not a gate beside the build, it IS the build. Measured on a copy of
// examples/as-workflow, 2026-09-17:
//
//	unmodified (control)       exit 0, .wasm written
//	Date.now() in place_order  exit 1, "E002: Date.now() in durable function
//	                           'place_order'", no .wasm
//
// A separate gate can be removed, reordered or skipped while the build still
// succeeds. A check inside the compiler cannot drift out of sync with it.
//
// What was missing is this test. The property has already been lost once:
// runVetAS's doc comment records that it "used to vet nothing", and
// IMPROVEMENT-PLAN 2.42 records that E001-E005 "used to be console.error() and
// nothing else, so even `cleat build` exited 0 on a violation". Nothing
// asserted otherwise, which is how it stayed true.
func TestASBuildRefusesNondeterminism(t *testing.T) {
	if testing.Short() {
		t.Skip("needs the cleat binary, which TestMain does not build in short mode")
	}

	// Same policy as TestVetAS, for the same reason: on CI the toolchain is
	// expected, so its absence is a broken environment rather than a licence
	// to skip. Skipping there would retire the only test asserting that an AS
	// build CAN fail.
	hasNpx := exec.Command("npx", "--version").Run() == nil
	if !hasNpx && os.Getenv("CI") != "" {
		t.Fatal("npx is expected on CI runners; without it `cleat build --target assemblyscript` is untested, not optional")
	}
	if !hasNpx {
		t.Skip("AS build test requires npx")
	}

	// One project, installed once -- npm install dominates the runtime and the
	// two arms differ only in the contents of assembly/index.ts.
	dir := newASVetProject(t)

	// runBuildAssemblyScript validates that an asconfig.json exists before it
	// compiles. The asc arguments are hardcoded and do not read it, so an
	// empty object is enough to get past the check.
	if err := os.WriteFile(filepath.Join(dir, "asconfig.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("writing asconfig.json: %v", err)
	}

	// -o goes BEFORE the project directory. Go's flag package stops parsing
	// flags at the first non-flag argument, so `build <dir> -o <out>` treats
	// -o as a positional and leaves outDir empty -- which means the current
	// directory. Measured: with -o trailing, 0 files land in the -o directory
	// and 1 lands in cwd; with -o leading, the reverse. This test ran that way
	// first and wrote 001.wasm into cmd/cleat, which is how it was found.
	build := func(t *testing.T) string {
		t.Helper()
		out, _ := exec.Command(cleatBinary, "build", "--target", "assemblyscript", "-o", t.TempDir(), dir).CombinedOutput()
		return string(out)
	}

	// ARM 1 -- KNOWN POSITIVE. Date.now() directly in an entry function.
	t.Run("known positive is refused, and for the determinism reason", func(t *testing.T) {
		writeASEntry(t, dir, `
import { HostCalls, cleatEntry } from "@cleat/sdk";

@cleatEntry()
function myWorkflow(h: HostCalls, input: string): string {
  const t: i64 = Date.now();
  if (t < 0) { return "{}"; }
  return "{\"status\":\"ok\"}";
}
`)
		out := build(t)

		if !strings.Contains(out, "E002") {
			t.Errorf("the build did not report the determinism finding.\n\n"+
				"A non-zero exit is not what this asserts -- an AS build can fail\n"+
				"for a missing toolchain, a failed npm install, or a syntax error.\n"+
				"The E002 code is what separates 'the transform refused this' from\n"+
				"'something else went wrong'.\n\noutput:\n%s", out)
		}
		if strings.Contains(out, "Wrote ") {
			t.Errorf("the build emitted an artifact despite a determinism finding.\n\noutput:\n%s", out)
		}
	})

	// ARM 2 -- KNOWN POSITIVE, added by cleat#1799. A helper the workflow
	// CALLS. This used to build cleanly: _computeDurableClosure walked callers
	// only, so a callee was never in scope. It is now reached by a forward walk
	// from the entry.
	t.Run("a helper the workflow calls is refused", func(t *testing.T) {
		writeASEntry(t, dir, `
import { HostCalls, cleatEntry } from "@cleat/sdk";

function readClock(): i64 {
  return Date.now();
}

@cleatEntry()
function myWorkflow(h: HostCalls, input: string): string {
  const t: i64 = readClock();
  if (t < 0) { return "{}"; }
  return "{\"status\":\"ok\"}";
}
`)
		out := build(t)

		if !strings.Contains(out, "E002") {
			t.Errorf("a non-deterministic helper the workflow calls was not reported.\n\n"+
				"Before cleat#1799 this built cleanly: the closure traversed callers,\n"+
				"so a function the workflow CALLS was never in scope. If this has\n"+
				"regressed, the forward walk in _computeReachable is not reaching\n"+
				"callees of the entry.\n\noutput:\n%s", out)
		}
	})

	// ARM 3 -- the other half of cleat#1799, and the direction the issue did
	// not originally report. A CALLER of durable code is not workflow code: it
	// is a harness or a driver, and it supplies `h` rather than executing under
	// replay. Flagging it refused correct programs.
	t.Run("a caller of durable code is not treated as workflow code", func(t *testing.T) {
		writeASEntry(t, dir, `
import { HostCalls, cleatEntry } from "@cleat/sdk";

function myDurableHelper(h: HostCalls): void {
  h.log("from a durable helper");
}

// Not reachable from any entry. Calls durable code and has its own
// non-determinism, exactly as a test harness does.
export function runTest(h: HostCalls): void {
  const t: i64 = Date.now();
  if (t > 0) { myDurableHelper(h); }
}

@cleatEntry()
function myWorkflow(h: HostCalls, input: string): string {
  myDurableHelper(h);
  return "{\"status\":\"ok\"}";
}
`)
		out := build(t)

		if strings.Contains(out, "E002") {
			t.Errorf("a CALLER of durable code was reported as non-deterministic.\n\n"+
				"runTest is a harness: it supplies `h` and is reachable from no\n"+
				"entry point. Reporting it refuses correct programs -- the Python\n"+
				"twin of this defect produced 22 false findings on a shipped example\n"+
				"and refused the build its own README documents (cleat#1813).\n\n"+
				"output:\n%s", out)
		}
	})

	// ARM 4 -- the pure-helper case, and it exists because the SUITE COULD NOT
	// SEE IT.
	//
	// The obvious repair for cleat#1799 -- use the forward scope for the
	// threading check as well -- passes every test in this package. It also
	// takes an unmodified examples/as-workflow from a clean build to SEVEN E005
	// diagnostics, on extractStringField, extractI64Field, extractRawArray,
	// indexOf, isDigit and parseI64: pure string and arithmetic helpers that
	// touch no host call and have no reason to take `h`.
	//
	// The reason the suite missed it is that its fixtures are small and have no
	// pure helpers, while the example is real code and does. That is cleat#1814
	// in miniature -- nothing in CI builds any example through `cleat build`,
	// so the only thing that would have caught this is a command nobody runs.
	// This arm puts the example's shape into the suite.
	t.Run("a pure helper is not asked for a HostCalls parameter", func(t *testing.T) {
		writeASEntry(t, dir, `
import { HostCalls, cleatEntry } from "@cleat/sdk";

// Pure: no host access, no reason to take h.
function isDigit(c: string): bool {
  return c >= "0" && c <= "9";
}

@cleatEntry()
function myWorkflow(h: HostCalls, input: string): string {
  if (isDigit(input)) { return "{}"; }
  return "{\"status\":\"ok\"}";
}
`)
		out := build(t)

		if strings.Contains(out, "E005") {
			t.Errorf("a pure helper was told it is missing a HostCalls parameter.\n\n"+
				"E005 asks whether a function can obtain the `h` it NEEDS. A helper\n"+
				"that touches no host call needs none, and demanding it is noise on\n"+
				"every string and arithmetic helper a workflow uses.\n\n"+
				"This is what happens if the threading scope becomes the forward\n"+
				"closure instead of its intersection with the callers closure --\n"+
				"seven of these on an unmodified examples/as-workflow.\n\n"+
				"output:\n%s", out)
		}
	})

	// ARM 5 -- KNOWN LIMIT, replacing the helper case that cleat#1799 closed.
	//
	// This one is sharper than the old one, because it is inside the ENTRY
	// FUNCTION ITSELF -- the most unambiguously durable code there is. So it
	// cannot be explained away as a scope question, which is exactly what the
	// old known limit turned out to be.
	//
	// _calleeName requires a dotted member access: the check does
	// `calleeName.indexOf(".")` and returns early when there is none. Bind the
	// function to a local first and the call site has no dot in it.
	t.Run("known limit escapes the checker, and says so out loud", func(t *testing.T) {
		writeASEntry(t, dir, `
import { HostCalls, cleatEntry } from "@cleat/sdk";

@cleatEntry()
function myWorkflow(h: HostCalls, input: string): string {
  const clock = Date.now;
  const t: i64 = clock();
  if (t < 0) { return "{}"; }
  return "{\"status\":\"ok\"}";
}
`)
		out := build(t)

		if strings.Contains(out, "E00") {
			t.Errorf("the AS transform now CATCHES an aliased non-deterministic call.\n\n"+
				"This is an improvement, not a regression -- most likely _calleeName\n"+
				"now resolves a local binding rather than requiring a dotted member\n"+
				"access. Three things to do:\n"+
				"  1. move this case to a known-positive arm above;\n"+
				"  2. write a new known-limit for whatever still escapes -- do not\n"+
				"     leave this arm empty, or the next reader cannot tell a strong\n"+
				"     checker from one that never ran;\n"+
				"  3. update LANGUAGE_SUPPORT.md, which describes AssemblyScript\n"+
				"     enforcement.\n\noutput:\n%s", out)
		}
	})
}
