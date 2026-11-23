package main

import (
	"os"
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

// memory.oom.group=1 makes an OOM kill take the whole container, so the refusal cannot protect the worker and the operator is told at start-up. The known negatives are as important as the
// positive: 0, a missing file and an unreadable one must not warn, or the warning means nothing.
func TestTheWorkerWarnsAtStartupWhenAnOutOfMemoryKillTakesTheWholeContainer(t *testing.T) {
	if msg := oomGroupWarning("1\n", nil); !strings.Contains(msg, "memory.oom.group is 1") || !strings.Contains(msg, "EVERY process") || !strings.Contains(msg, "the memory limit must hold the compile peak") {
		t.Errorf("memory.oom.group=1 produced %q", msg)
	}
	if msg := oomGroupWarning(" 1 ", nil); msg == "" {
		t.Error("surrounding whitespace hid a 1")
	}
	for name, tc := range map[string]struct {
		content string
		err     error
	}{"zero": {"0\n", nil}, "empty": {"", nil}, "garbage": {"yes", nil}, "unreadable": {"", os.ErrNotExist}, "unreadable with content": {"1", os.ErrPermission}} {
		if msg := oomGroupWarning(tc.content, tc.err); msg != "" {
			t.Errorf("%s warned: %q", name, msg)
		}
	}
}
