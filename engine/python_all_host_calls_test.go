package engine

import (
	"testing"
)

// TestPythonAllHostCallsWorkflowCompiles builds a Python workflow that calls
// every public method on cleat_sdk.HostCalls.
//
// TestPythonWasmEndToEnd compiles durable_call_workflow.py, which calls one
// host method. Passing it means "Python can make a durable call", not "the
// Python host-call surface builds" -- so a binding that does not exist, or does
// not accept what the SDK passes it, reached users rather than CI. The Go side
// had the same hole and four host calls shipped uncompilable through it
// (IMPROVEMENT-PLAN.md 3.204); this is the Python half, 3.205.
//
// The fixture's breadth is asserted separately and cheaply by
// python-sdk/tests/test_all_host_calls_fixture.py, which fails if a public
// HostCalls method is missing from it. That test needs no toolchain and runs
// everywhere; this one needs componentize-py and is where the coverage is
// actually cashed in.
//
// Prerequisite handling is deliberately identical to TestPythonWasmEndToEnd's,
// including the toolchainRequired escalation: a job that declares "python" in
// CLEAT_REQUIRE_TOOLCHAINS has installed componentize-py on purpose, so a
// missing tool there is a failure rather than a skip. The tier-1 gate declares
// it -- tiers.yaml puts python in tier1.languages, and tier 1 forbids skips.
//
// ON A MAC THIS SKIPS, AND THAT IS NOT A REASON TO LEAVE IT UNVERIFIED.
// componentize-py cannot run on Darwin at all: its embedded wasmtime installs a
// mach exception handler into a guarded port and dies with EXC_GUARD /
// GUARD_TYPE_MACH_PORT, which has no Linux equivalent. Run it in the container
// the repo already ships for this:
//
//	docker --context desktop-linux run --rm -v "$PWD":/src -w /src -e CGO_ENABLED=1 \
//	  cleat-py-toolchain go test ./engine/ -run TestPythonAllHostCallsWorkflowCompiles
//
// --context desktop-linux is not optional on a machine that also runs colima --
// colima bind-mounts these paths as an EMPTY directory without saying so, and
// the run then fails with "go.mod file not found", which reads as a broken
// checkout rather than a wrong context. See
// scripts/docker/python-toolchain.Dockerfile, which documents both.
func TestPythonAllHostCallsWorkflowCompiles(t *testing.T) {
	pythonWasm := newPythonWasmTestHelper(t)
	if !pythonWasm.toolsAvailable() {
		if toolchainRequired("python") {
			t.Fatalf("Python WASM prerequisites not met, but %s declares python: %s",
				requireToolchainEnv, pythonWasm.missingTools())
		}
		t.Skip("Python WASM prerequisites not met: " + pythonWasm.missingTools())
	}

	// compileWorkflow t.Fatals on a build failure, which is the assertion: the
	// whole point is that componentize-py has to accept every host call the
	// fixture makes. There is nothing to check afterwards -- a returned path
	// means the component built.
	wasmPath := pythonWasm.compileWorkflow(t, "all_host_calls_workflow.py", "all_host_calls_workflow")
	if wasmPath == "" {
		t.Fatal("compileWorkflow returned an empty path with no failure reported")
	}
}
