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

	// ARM 2 -- KNOWN LIMIT, and this one is a design boundary rather than a
	// spelling trick.
	//
	// _computeDurableClosure traverses CALLERS, not callees: it starts from
	// durable leaves (functions that call h.*, plus @cleatEntry functions) and
	// adds anything that CALLS one. So the closure grows upward, toward the
	// entry, and never downward into what a workflow calls.
	//
	// A helper that does not touch the host is therefore never validated, even
	// when a durable function calls it directly. Measured, and the pair is the
	// evidence for the mechanism rather than just the symptom:
	//
	//	helper calling Date.now() only          exit 0, no diagnostic, .wasm written
	//	helper calling h.log() AND Date.now()   exit 1, E002 -- it became a leaf
	//
	// That is upside down for a determinism check: correctness needs the
	// callee closure -- everything the workflow can reach -- and the callers
	// closure is what the E005 threading check needs. One traversal is serving
	// two checks that want opposite directions. Filed separately.
	//
	// So the limit is reached by extracting a helper, which is the most
	// ordinary refactoring there is, and nothing reports anything.
	t.Run("known limit escapes the checker, and says so out loud", func(t *testing.T) {
		writeASEntry(t, dir, `
import { HostCalls, cleatEntry } from "@cleat/sdk";

// Not in the durable closure: it calls no host method, so nothing marks it
// durable, so E001-E004 never run against it.
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

		if strings.Contains(out, "E00") {
			t.Errorf("the AS transform now CATCHES a non-deterministic helper "+
				"outside the durable closure.\n\n"+
				"This is an improvement, not a regression -- most likely the\n"+
				"closure now traverses callees as well as callers. Three things\n"+
				"to do:\n"+
				"  1. move this case to the known-positive arm above;\n"+
				"  2. write a new known-limit fixture for whatever still escapes\n"+
				"     -- do not leave this arm empty, or the next reader cannot\n"+
				"     tell a strong checker from one that never ran;\n"+
				"  3. update LANGUAGE_SUPPORT.md, which describes AssemblyScript\n"+
				"     enforcement as entry-and-host-callers only.\n\noutput:\n%s", out)
		}
	})
}
