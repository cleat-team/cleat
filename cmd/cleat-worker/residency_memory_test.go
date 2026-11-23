package main

import (
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

// The residency budget is a share of the worker's memory (cleat#4377). These pin how the number is read, in what order, and what happens when it cannot be.
//
// Each source is a fake filesystem laid out like /, so the cgroup files are the ones a container has and not whatever machine the suite runs on. EVERY case has a
// control that varies the one thing under test: a budget that came out right because the test host happened to have 2 GiB would pass a function that ignores the
// cgroup, and a function that always returns the host's memory would pass the "no limit" rows.

const gib = uint64(1) << 30

// fakeHost writes files (path under the root -> content) into a temporary directory and returns the root with the trailing slash the probe expects.
func fakeHost(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, content := range files {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root + "/"
}

func u64(n uint64) string { return strconv.FormatUint(n, 10) }

func meminfo(kb uint64) string {
	return "MemTotal:       " + u64(kb) + " kB\nMemFree:         1000 kB\nMemAvailable:    2000 kB\n"
}

// useFakeHostMemory points the probe at a container with a cgroup v2 limit of limit bytes on a 64 GiB host, for every test that builds the policy from the flags, so none of
// them depends on the machine it runs on. It restores the real root afterwards.
func useFakeHostMemory(t *testing.T, limit uint64) {
	t.Helper()
	old, oldPhysical := residencyProbeRoot, residencyPhysical
	residencyProbeRoot = fakeHost(t, map[string]string{
		"sys/fs/cgroup/memory.max": u64(limit) + "\n",
		"proc/meminfo":             meminfo(64 * gib >> 10),
	})
	residencyPhysical = func() (uint64, bool) { return 0, false }
	t.Cleanup(func() { residencyProbeRoot, residencyPhysical = old, oldPhysical })
}

func TestTheAvailableMemoryIsReadFromTheMostSpecificSourceFirst(t *testing.T) {
	const host = 64 * gib
	cases := []struct {
		name       string
		files      map[string]string
		physical   func() (uint64, bool)
		wantBytes  uint64
		wantSource string
		wantWarn   bool
	}{
		{"cgroup v2 limit", map[string]string{"sys/fs/cgroup/memory.max": "2147483648\n", "proc/meminfo": meminfo(host >> 10)}, nil, 2 * gib, memSourceCgroupV2, false},
		{"cgroup v2 limit, no meminfo to compare it with", map[string]string{"sys/fs/cgroup/memory.max": "2147483648\n"}, nil, 2 * gib, memSourceCgroupV2, false},
		{"cgroup v2 max means no limit: the host's memory, with a warning", map[string]string{"sys/fs/cgroup/memory.max": "max\n", "proc/meminfo": meminfo(host >> 10)}, nil, host, memSourcePhysical, true},
		{"cgroup v1 limit", map[string]string{"sys/fs/cgroup/memory/memory.limit_in_bytes": "1073741824\n", "proc/meminfo": meminfo(host >> 10)}, nil, 1 * gib, memSourceCgroupV1, false},
		{"cgroup v1 unlimited sentinel (a number near 2^63): the host's memory", map[string]string{"sys/fs/cgroup/memory/memory.limit_in_bytes": "9223372036854771712\n", "proc/meminfo": meminfo(host >> 10)}, nil, host, memSourcePhysical, true},
		{"a v1 limit at or above the host's memory is no limit", map[string]string{"sys/fs/cgroup/memory/memory.limit_in_bytes": "137438953472\n", "proc/meminfo": meminfo(host >> 10)}, nil, host, memSourcePhysical, true},
		{"a v2 limit above the host's memory is no limit", map[string]string{"sys/fs/cgroup/memory.max": "137438953472\n", "proc/meminfo": meminfo(host >> 10)}, nil, host, memSourcePhysical, true},
		{"v2 wins over v1 when both are there", map[string]string{"sys/fs/cgroup/memory.max": "2147483648\n", "sys/fs/cgroup/memory/memory.limit_in_bytes": "1073741824\n", "proc/meminfo": meminfo(host >> 10)}, nil, 2 * gib, memSourceCgroupV2, false},
		{"no cgroup at all: meminfo", map[string]string{"proc/meminfo": meminfo(host >> 10)}, nil, host, memSourcePhysical, true},
		{"no cgroup and no /proc (macOS): the platform's own figure", map[string]string{}, func() (uint64, bool) { return 16 * gib, true }, 16 * gib, memSourcePhysical, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := probeAvailableMemory(fakeHost(t, c.files), c.physical)
			if err != nil {
				t.Fatalf("the memory could not be read: %v", err)
			}
			if got.Bytes != c.wantBytes || got.Source != c.wantSource {
				t.Errorf("read %d bytes from %q, want %d bytes from %q", got.Bytes, got.Source, c.wantBytes, c.wantSource)
			}
			if (got.Warning != "") != c.wantWarn {
				t.Errorf("warning = %q, want a warning: %v (the host's physical memory is the one source an operator must be told about)", got.Warning, c.wantWarn)
			}
		})
	}
}

func TestMemoryThatCannotBeReadIsAnErrorAndNotAGuess(t *testing.T) {
	if _, err := probeAvailableMemory(fakeHost(t, map[string]string{}), nil); err == nil {
		t.Fatal("no cgroup, no /proc/meminfo and no platform source gave a number: a budget would have been invented")
	}
	// Garbage in a file that exists is the same as no file, and must not be read as a limit of zero or a huge one.
	for _, f := range []map[string]string{
		{"sys/fs/cgroup/memory.max": "not-a-number\n"},
		{"sys/fs/cgroup/memory.max": "0\n"},
		{"proc/meminfo": "MemTotal: lots kB\n"},
		{"proc/meminfo": "MemTotal: 0 kB\n"},
		// The v1 "unlimited" sentinel with NO host figure to compare it with: only the sentinel check itself can refuse it, so this is the row that fails if that check goes
		// (with a readable MemTotal the "not below the host" rule catches it too, which is why the sentinel row in the table above cannot tell the two apart).
		{"sys/fs/cgroup/memory/memory.limit_in_bytes": "9223372036854771712\n"},
	} {
		if got, err := probeAvailableMemory(fakeHost(t, f), nil); err == nil {
			t.Errorf("%v read as %d bytes from %q, want an error", f, got.Bytes, got.Source)
		}
	}
}

func TestTheBudgetIsThePercentageOfTheLimitRoundedDown(t *testing.T) {
	for _, c := range []struct {
		avail   uint64
		percent float64
		want    uint64
	}{
		{2 * gib, 20, 429496729},   // 409.6 MiB
		{512 << 20, 20, 107374182}, // the chart's old limit: 102.4 MiB
		{8 * gib, 10, 858993459},   // 819.2 MiB
		{2 * gib, 100, 2 * gib},
		{1000, 33, 330},
	} {
		if got := residencyBudgetBytes(c.avail, c.percent); got != c.want {
			t.Errorf("%g%% of %d = %d, want %d", c.percent, c.avail, got, c.want)
		}
	}
}

// buildPolicy runs the real flags-to-policy function with the given flag values against a fake host, and returns the policy and what was logged.
func buildPolicy(t *testing.T, root string, flags map[string]string) (view policyView, logs string) {
	t.Helper()
	old, oldPhysical := residencyProbeRoot, residencyPhysical
	residencyProbeRoot, residencyPhysical = root, func() (uint64, bool) { return 0, false } // never the machine the suite runs on
	t.Cleanup(func() { residencyProbeRoot, residencyPhysical = old, oldPhysical })
	var sb strings.Builder
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&sb, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	for n, v := range flags {
		setFlag(t, n, v)
	}
	for _, n := range []string{"resident-max-runs", "resident-memory-percent", "resident-memory-budget-mb"} {
		if _, set := flags[n]; !set {
			stockFlag(t, n)
		}
	}
	cfg, err := residencyConfigFromFlags()
	if err != nil {
		t.Fatalf("residencyConfigFromFlags: %v", err)
	}
	t.Cleanup(func() {
		for n := range flags {
			stockFlag(t, n)
		}
	})
	return policyView{cfg.MaxRuns, cfg.MemoryBudgetBytes}, sb.String()
}

type policyView struct {
	maxRuns int
	budget  uint64
}

func TestTheBudgetFollowsTheLimitTheContainerHas(t *testing.T) {
	two := fakeHost(t, map[string]string{"sys/fs/cgroup/memory.max": "2147483648\n", "proc/meminfo": meminfo(64 * gib >> 10)})
	eight := fakeHost(t, map[string]string{"sys/fs/cgroup/memory.max": "8589934592\n", "proc/meminfo": meminfo(64 * gib >> 10)})
	a, logs := buildPolicy(t, two, nil)
	b, _ := buildPolicy(t, eight, nil)
	if a.budget != 429496729 || b.budget != 1717986918 {
		t.Errorf("budgets %d (2 GiB limit) and %d (8 GiB limit), want 20%% of each", a.budget, b.budget)
	}
	// The CONTROL: the two differ only in the cgroup file, so equal budgets would mean the cgroup is not read.
	if a.budget == b.budget {
		t.Error("a 2 GiB and an 8 GiB limit gave the same budget: the cgroup limit does not reach the policy")
	}
	if !strings.Contains(logs, "residency memory budget") || !strings.Contains(logs, "cgroup v2 memory.max") || !strings.Contains(logs, "percent=20") {
		t.Errorf("the boot did not log the budget and where it came from: %q", logs)
	}
}

func TestAHostWithNoCgroupLimitWarnsAndUsesItsPhysicalMemory(t *testing.T) {
	root := fakeHost(t, map[string]string{"proc/meminfo": meminfo(16 * gib >> 10)})
	p, logs := buildPolicy(t, root, nil)
	if want := residencyBudgetBytes(16*gib, 20); p.budget != want {
		t.Errorf("budget %d on a 16 GiB host with no limit, want %d", p.budget, want)
	}
	if !strings.Contains(logs, "level=WARN") || !strings.Contains(logs, "PHYSICAL") {
		t.Errorf("a share of physical memory was used without a warning: %q", logs)
	}
}

func TestAWorkerWhoseMemoryCannotBeReadStillStartsWithTheOldFixedBound(t *testing.T) {
	root := fakeHost(t, map[string]string{})
	p, logs := buildPolicy(t, root, nil)
	if want := engine.DearestResidentInstanceBytes() * fallbackResidentRuns; p.budget != want || want == 0 {
		t.Errorf("fallback budget %d, want %d (8 runs of the dearest language)", p.budget, want)
	}
	if !strings.Contains(logs, "level=WARN") || !strings.Contains(logs, "could not be read") {
		t.Errorf("an unreadable memory source was not warned about: %q", logs)
	}
}

func TestAnExplicitBudgetOverridesThePercentageAndSkipsTheProbe(t *testing.T) {
	root := fakeHost(t, map[string]string{"sys/fs/cgroup/memory.max": "2147483648\n", "proc/meminfo": meminfo(64 * gib >> 10)})
	p, logs := buildPolicy(t, root, map[string]string{"resident-memory-budget-mb": "100"})
	if p.budget != 100<<20 {
		t.Errorf("--resident-memory-budget-mb 100 gave a budget of %d, want %d", p.budget, 100<<20)
	}
	if !strings.Contains(logs, "--resident-memory-budget-mb") {
		t.Errorf("the log does not say the budget was set explicitly: %q", logs)
	}
}

func TestAPercentageOfZeroTurnsResidencyOff(t *testing.T) {
	root := fakeHost(t, map[string]string{"sys/fs/cgroup/memory.max": "2147483648\n", "proc/meminfo": meminfo(64 * gib >> 10)})
	p, _ := buildPolicy(t, root, map[string]string{"resident-memory-percent": "0"})
	if p.maxRuns != 0 {
		t.Errorf("--resident-memory-percent 0 left MaxRuns at %d, want 0 (off)", p.maxRuns)
	}
	// The control: any other percentage leaves it on.
	on, _ := buildPolicy(t, root, map[string]string{"resident-memory-percent": "1"})
	if on.maxRuns != 1024 || on.budget != 21474836 {
		t.Errorf("--resident-memory-percent 1 gave %+v, want 1024 runs and 1%% of 2 GiB", on)
	}
}
