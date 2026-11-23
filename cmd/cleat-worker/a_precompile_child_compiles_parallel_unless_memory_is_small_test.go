package main

import (
	"strings"
	"testing"
)

// --component-precompile-parallel=auto is parallel unless the worker's memory limit is under 1 GiB. The log names the reason, so these assert the reason too: a resolver that
// returned the right bool for the wrong reason would read the same in a status-only test.
func TestThePrecompileChildIsParallelUnlessTheMemoryLimitIsUnderOneGiB(t *testing.T) {
	const mib = 1 << 20
	for _, tc := range []struct {
		mode     string
		total    uint64
		parallel bool
		why      string
	}{
		{"auto", 512 * mib, false, "under 1 GiB"},
		{"auto", 1024*mib - 1, false, "under 1 GiB"},
		{"auto", 1024 * mib, true, "at least 1 GiB"},
		{"auto", 2048 * mib, true, "at least 1 GiB"},
		{"", 512 * mib, false, "under 1 GiB"},
		{"AUTO", 4096 * mib, true, "at least 1 GiB"},
		{"auto", 0, true, "could not be read"},
		{"true", 512 * mib, true, "--component-precompile-parallel=true"},
		{"false", 4096 * mib, false, "--component-precompile-parallel=false"},
	} {
		parallel, why, err := resolvePrecompileParallel(tc.mode, tc.total)
		if err != nil || parallel != tc.parallel || !strings.Contains(why, tc.why) {
			t.Errorf("mode %q total %d MiB: got parallel=%v why=%q err=%v, want parallel=%v why containing %q", tc.mode, tc.total/mib, parallel, why, err, tc.parallel, tc.why)
		}
	}
	if _, _, err := resolvePrecompileParallel("sometimes", 2048*mib); err == nil {
		t.Error("an unknown mode was accepted")
	}
}
