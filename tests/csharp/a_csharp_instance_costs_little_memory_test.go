//go:build cgo

package csharp

import (
	"fmt"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

// A C# workflow instance costs about 13 MiB, not 49 (cleat#4378). These two tests pin that (the second is in a_csharp_instance_costs_little_memory_linux_test.go, which only Linux builds: it reads /proc), because the cost is set by ONE line in
// packages/cleat-csharp/build/Cleat.Sdk.props that a toolchain bump or a tidy-up could drop with no other test noticing: every conformance
// case passes whatever the GC segment size is.
//
// WHY THE SETTING MATTERS. A NativeAOT-LLVM guest on WASI starts a workstation GC with three 16 MiB segments and the WASI layer writes every
// byte it reserves (posix_memalign then memset(0)), so an empty workflow occupied 48.7 MiB of private memory per instance. GCSegmentSize=4 MiB
// (the floor) makes it 12.7 MiB. Measured 2026-11-22 as a fit over 1 to 64 instances, three repetitions each, R2 = 1.0000; the instrument is on
// feature/resident-memory-percent-design (testdata/heldmem).

// startupMemoryBudget is the per-store linear-memory cap a C# component built by the SDK must START under. The SDK's guest needs about
// 14.5 MiB (232 pages); without the setting it needs about 50.6 MiB (809 pages) and the guest dies at start-up with ".NET runtime failed to
// initialize". 24 MiB leaves room for the toolchain moving by a few MiB and is far below the failure it guards.
const startupMemoryBudget = 24 << 20

// startupMemoryFloor is a cap the guest CANNOT start under: three GC segments of the 4 MiB minimum are 12 MiB before anything else. It is the
// control for the test above: a cap that is not binding would make "it started under 24 MiB" a statement about nothing.
const startupMemoryFloor = 8 << 20

func TestCSharpAnInstanceStartsUnderASmallMemoryCap(t *testing.T) {
	b := conformanceComponent(t)

	under := func(cap int64) conformanceResult {
		return runComponentEntry(t, b, "smoke", entryRun{WorkflowID: fmt.Sprintf("wf-startup-%d", cap), Input: `{"x":1}`,
			Backend: []engine.WasmtimeOption{engine.WithWasmtimeMemoryLimits(cap, 0, 0)}})
	}

	// The control: the cap binds. If the guest starts in 8 MiB the next assertion proves nothing.
	if res := under(startupMemoryFloor); res.Err == nil {
		t.Fatalf("PRECONDITION FAILED: the component started under a %d MiB cap (result %q), so a %d MiB cap does not bind and this test measures nothing; "+
			"if the guest really got that small, lower startupMemoryFloor", startupMemoryFloor>>20, res.Result, startupMemoryBudget>>20)
	}

	res := under(startupMemoryBudget)
	if res.Err != nil {
		t.Fatalf("the component did not start under a %d MiB per-store memory cap: %v\n"+
			"a guest built WITHOUT the GC segment setting needs about 51 MiB to start (cleat#4378): check that packages/cleat-csharp/build/Cleat.Sdk.props still "+
			"passes --runtimeopt:GCSegmentSize and that the build still reaches it", startupMemoryBudget>>20, res.Err)
	}
	if !strings.Contains(res.Result, `"echo"`) {
		t.Errorf("the entry ran but returned %q, not the smoke result, so it did not run the guest it was meant to", res.Result)
	}
}
