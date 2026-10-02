package main

import (
	"strconv"
	"testing"
)

// TestPythonStampEnvCarriesTheWorkflowVersion is the regression test for the
// half of cleat#2936 that shipped as PR #2941.
//
// `cleat build --target python` stamped cleat.metadata with workflow_version 0,
// because runBuildPython never took --version and stamp_metadata.py's
// env_or_arg("CLEAT_WORKFLOW_VERSION", ...) therefore fell back to its own
// default of 0. That was inert for as long as a Component Model binary's
// cleat.metadata could not be read at all -- cmd/cleat-worker/setup.go's version
// pre-flight saw wfMeta == nil and skipped. The moment PR #2941 made that read
// work, wfMeta stopped being nil, the check engaged, and 0 disagreed with the
// def_version a deploy records (cleat deploy prefers the stamp and defaults
// --version to 1). Every Python run was released as "version_check" on claim and
// looped forever without executing: Python's only working start path, gone.
//
// # Why this asserts the environment rather than a build
//
// The chain has three links, and this is the Go-side one:
//
//	cleat build --version N  ->  CLEAT_WORKFLOW_VERSION=N  ->  stamp_metadata.py
//
// The third link is pinned by python-sdk/tests/test_stamp_metadata.py, which
// sets the variable with monkeypatch and asserts build_metadata honours it; the
// second is this. Asserting the whole chain end to end would mean a real
// componentize-py build, and cmd/cleat's CI leg (`Test Go (commands)`) does not
// install that toolchain -- which is why the cross-language E2E job, the only
// one that does, runs ./engine/... and not this package. A build-level test
// here would skip in CI, and a skip is not a measurement
// (CLAUDE.md, "Is this result real?").
//
// So this is the same shape as build_metadata_test.go's assertion that
// --version reaches Rust, Java and AssemblyScript through nonGoMetadata: a
// value-level check on the seam, in a job that always runs it.
func TestPythonStampEnvCarriesTheWorkflowVersion(t *testing.T) {
	for _, version := range []int{1, 2, 3, 7} {
		env := pythonStampEnv(version, "place_order", "latest")

		got, ok := env["CLEAT_WORKFLOW_VERSION"]
		if !ok {
			t.Fatalf("version %d: CLEAT_WORKFLOW_VERSION is absent from the Python "+
				"build's environment. stamp_metadata.py then defaults the stamp to 0, "+
				"cmd/cleat-worker/setup.go's pre-flight compares that 0 against the "+
				"def_version a deploy records, and every start is released as "+
				"\"version_check\" forever. See cleat#2936.", version)
		}
		if want := strconv.Itoa(version); got != want {
			t.Errorf("version %d: CLEAT_WORKFLOW_VERSION = %q, want %q -- the "+
				"--version flag is threaded through to the Python target, not "+
				"defaulted away", version, got, want)
		}
	}
}

// The two optional variables, and specifically the empty case: build_wasm.py
// reads both with `args.x or os.environ.get(...)`, so an empty string is falsy
// and means "absent". Setting them to "" would be the same as not setting them,
// but the map is what runBuildPython ranges over, so it is worth pinning that
// an empty value produces no entry at all rather than an empty one.
func TestPythonStampEnvSetsOptionalValuesOnlyWhenPresent(t *testing.T) {
	bare := pythonStampEnv(1, "", "")
	if _, ok := bare["CLEAT_ENTRY_POINTS"]; ok {
		t.Errorf("CLEAT_ENTRY_POINTS is set to %q when no entry point was derived; "+
			"a failed schema computation must leave it unset, not empty",
			bare["CLEAT_ENTRY_POINTS"])
	}
	if _, ok := bare["CLEAT_CHILD_BINDING_POLICY"]; ok {
		t.Errorf("CLEAT_CHILD_BINDING_POLICY is set to %q when no channel was given",
			bare["CLEAT_CHILD_BINDING_POLICY"])
	}

	full := pythonStampEnv(1, "place_order", "latest")
	if got := full["CLEAT_ENTRY_POINTS"]; got != "place_order" {
		t.Errorf("CLEAT_ENTRY_POINTS = %q, want %q", got, "place_order")
	}
	if got := full["CLEAT_CHILD_BINDING_POLICY"]; got != "latest" {
		t.Errorf("CLEAT_CHILD_BINDING_POLICY = %q, want %q", got, "latest")
	}
}
