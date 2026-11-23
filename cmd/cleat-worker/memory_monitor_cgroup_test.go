package main

import (
	"os"
	"testing"
)

// cleat#4431: the memory monitor, which backs --memory-soft-limit and --memory-hard-limit, read the cgroup mount's ROOT. That is the process's own cgroup only in a private cgroup
// namespace; under a host namespace it is the whole host, so a 512 MiB container's limits were fractions of the host's memory and the host's usage. These rows read a fake
// filesystem laid out like /, and the discriminating value in every row is the ROOT's memory.current, set to something the process's own cgroup must never be measured against.

func readOnce(t *testing.T, files map[string]string) MemoryInfo {
	t.Helper()
	fn, ok := probeCgroupV2At(fakeHost(t, files))
	if !ok {
		t.Fatalf("no cgroup v2 source was found on %v", files)
	}
	info, err := fn()
	if err != nil {
		t.Fatal(err)
	}
	return info
}

func TestTheMemoryMonitorMeasuresTheProcessesOwnCgroupUnderAHostCgroupNamespace(t *testing.T) {
	info := readOnce(t, map[string]string{
		"proc/self/cgroup":                        "0::/docker/abc\n",
		"proc/meminfo":                            meminfo(64 * gib >> 10),
		"sys/fs/cgroup/memory.max":                "max\n",
		"sys/fs/cgroup/memory.current":            u64(40*gib) + "\n", // the HOST's usage: it must never be what the container is measured against
		"sys/fs/cgroup/docker/abc/memory.max":     "536870912\n",
		"sys/fs/cgroup/docker/abc/memory.current": "268435456\n",
	})
	if info.TotalBytes != 512<<20 || info.UsedBytes != 256<<20 || info.AvailableBytes != 256<<20 {
		t.Errorf("total %d used %d avail %d, want the container's 512 MiB total, 256 MiB used, 256 MiB available (not the host's)", info.TotalBytes, info.UsedBytes, info.AvailableBytes)
	}
	// The consequence that matters: half-full is half-full, so the pressure is what a 512 MiB container's soft limit means, and not "0.6 of a 64 GiB host".
	if p := PressureLevel(info, 0.40, 0.60); p < 0.499 || p > 0.501 {
		t.Errorf("pressure %g for a container at 50%% against soft 40%% / hard 60%%, want 0.5 (the middle of the ramp)", p)
	}
}

func TestTheMemoryMonitorInAPrivateCgroupNamespaceReadsTheRootAsBefore(t *testing.T) {
	info := readOnce(t, map[string]string{
		"proc/self/cgroup":             "0::/\n",
		"proc/meminfo":                 meminfo(64 * gib >> 10),
		"sys/fs/cgroup/memory.max":     "536870912\n",
		"sys/fs/cgroup/memory.current": "268435456\n",
	})
	if info.TotalBytes != 512<<20 || info.UsedBytes != 256<<20 {
		t.Errorf("total %d used %d, want 512 MiB and 256 MiB", info.TotalBytes, info.UsedBytes)
	}
	// And a worker with no /proc/self/cgroup at all (an older kernel, a restricted sandbox) reads the root exactly as it always did.
	bare := readOnce(t, map[string]string{
		"proc/meminfo":                 meminfo(64 * gib >> 10),
		"sys/fs/cgroup/memory.max":     "536870912\n",
		"sys/fs/cgroup/memory.current": "268435456\n",
	})
	if bare.TotalBytes != 512<<20 || bare.UsedBytes != 256<<20 {
		t.Errorf("with no /proc/self/cgroup: total %d used %d, want 512 MiB and 256 MiB", bare.TotalBytes, bare.UsedBytes)
	}
}

func TestAParentsLimitBoundsTheMonitorAndItsUsageIsTheParents(t *testing.T) {
	info := readOnce(t, map[string]string{
		"proc/self/cgroup":                 "0::/a/b\n",
		"proc/meminfo":                     meminfo(64 * gib >> 10),
		"sys/fs/cgroup/memory.max":         "max\n",
		"sys/fs/cgroup/a/memory.max":       "1073741824\n",
		"sys/fs/cgroup/a/memory.current":   "805306368\n", // 768 MiB: the parent's usage, which includes this cgroup's siblings
		"sys/fs/cgroup/a/b/memory.max":     "max\n",
		"sys/fs/cgroup/a/b/memory.current": "104857600\n", // this cgroup alone: 100 MiB, which is NOT what the parent's limit is measured against
	})
	if info.TotalBytes != 1<<30 || info.UsedBytes != 768<<20 {
		t.Errorf("total %d used %d, want the parent's 1 GiB limit against the parent's 768 MiB usage", info.TotalBytes, info.UsedBytes)
	}
}

// With no limit at any level the cgroup source DECLINES and the monitor falls through to /proc/meminfo, which pairs the host's MemTotal with HOST-WIDE usage (MemAvailable). The first
// version paired the host's total with this cgroup's own memory.current, which reads a busy host as nearly empty (cleat-review3, measured: 2 MiB used against a host at ~5751 MiB).
func TestWithNoLimitAnywhereTheCgroupSourceDeclinesSoTheHostWideReadingIsUsed(t *testing.T) {
	root := fakeHost(t, map[string]string{
		"proc/self/cgroup":                        "0::/docker/abc\n",
		"proc/meminfo":                            meminfo(64 * gib >> 10),
		"sys/fs/cgroup/memory.max":                "max\n",
		"sys/fs/cgroup/memory.current":            u64(40*gib) + "\n",
		"sys/fs/cgroup/docker/abc/memory.max":     "max\n",
		"sys/fs/cgroup/docker/abc/memory.current": "104857600\n",
	})
	if _, ok := probeCgroupV2At(root); ok {
		t.Fatal("the cgroup v2 source answered with no limit at any level: it would pair the host's total with this cgroup's own usage")
	}
	// The control: give the same hierarchy one numeric limit and it answers.
	files := map[string]string{
		"proc/self/cgroup":                        "0::/docker/abc\n",
		"proc/meminfo":                            meminfo(64 * gib >> 10),
		"sys/fs/cgroup/memory.max":                "max\n",
		"sys/fs/cgroup/docker/abc/memory.max":     "536870912\n",
		"sys/fs/cgroup/docker/abc/memory.current": "104857600\n",
	}
	if _, ok := probeCgroupV2At(fakeHost(t, files)); !ok {
		t.Fatal("a hierarchy with a numeric limit was not read: the decline above proves nothing")
	}
}

// cleat-review3's measured case: a leaf with the TIGHTER limit (2 GiB) using 1 GiB, under a parent at 4 GiB using 3.9 GiB. The parent has about 102 MiB of headroom and the leaf
// 1 GiB, so the parent is what runs out first and is what the monitor must report, though its limit is the larger.
func TestTheLevelWithTheLeastHeadroomIsReportedNotTheTightestLimit(t *testing.T) {
	files := map[string]string{
		"proc/self/cgroup":                    "0::/p/leaf\n",
		"proc/meminfo":                        meminfo(64 * gib >> 10),
		"sys/fs/cgroup/p/memory.max":          "4294967296\n",
		"sys/fs/cgroup/p/memory.current":      "4187593114\n", // 3.9 GiB
		"sys/fs/cgroup/p/leaf/memory.max":     "2147483648\n",
		"sys/fs/cgroup/p/leaf/memory.current": "1073741824\n", // 1 GiB
	}
	info := readOnce(t, files)
	if info.TotalBytes != 4*gib || info.UsedBytes != 4187593114 {
		t.Errorf("total %d used %d, want the PARENT's 4 GiB and 3.9 GiB: it has ~102 MiB of headroom and the leaf 1 GiB", info.TotalBytes, info.UsedBytes)
	}
	// The CONTROL: the same two limits, with the leaf nearly full instead. The leaf binds, so a function that always picked the parent would fail THIS row.
	files["sys/fs/cgroup/p/memory.current"] = "2147483648\n"      // the parent at 2 GiB: 2 GiB of headroom
	files["sys/fs/cgroup/p/leaf/memory.current"] = "2040109465\n" // the leaf at 1.9 GiB of its 2: ~102 MiB
	info = readOnce(t, files)
	if info.TotalBytes != 2*gib || info.UsedBytes != 2040109465 {
		t.Errorf("control: total %d used %d, want the LEAF's 2 GiB and 1.9 GiB", info.TotalBytes, info.UsedBytes)
	}
}

// The binding level changes over time, so it is chosen at every READ and not once at probe time: the same probe, the files rewritten between two reads.
func TestTheBindingLevelIsChosenAtEveryReadNotOnceAtStartUp(t *testing.T) {
	root := fakeHost(t, map[string]string{
		"proc/self/cgroup":                    "0::/p/leaf\n",
		"proc/meminfo":                        meminfo(64 * gib >> 10),
		"sys/fs/cgroup/p/memory.max":          "4294967296\n",
		"sys/fs/cgroup/p/memory.current":      "1073741824\n",
		"sys/fs/cgroup/p/leaf/memory.max":     "2147483648\n",
		"sys/fs/cgroup/p/leaf/memory.current": "1932735283\n", // the leaf is the one close to its limit
	})
	fn, ok := probeCgroupV2At(root)
	if !ok {
		t.Fatal("no source")
	}
	first, _ := fn()
	if first.TotalBytes != 2*gib {
		t.Fatalf("first read: total %d, want the leaf's 2 GiB (it is the one at 90%%)", first.TotalBytes)
	}
	// A sibling grows: now the parent is the one about to run out.
	if err := os.WriteFile(root+"sys/fs/cgroup/p/memory.current", []byte("4187593114\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	second, _ := fn()
	if second.TotalBytes != 4*gib || second.UsedBytes != 4187593114 {
		t.Errorf("second read: total %d used %d, want the parent's 4 GiB and 3.9 GiB after a sibling grew", second.TotalBytes, second.UsedBytes)
	}
}

// One resolver, two readers: the residency budget and the monitor must be measured against the same cgroup. They are given the same fake root and must name the same limit.
func TestTheResidencyBudgetAndTheMemoryMonitorAgreeOnTheSameLimit(t *testing.T) {
	root := fakeHost(t, map[string]string{
		"proc/self/cgroup":                        "0::/docker/abc\n",
		"proc/meminfo":                            meminfo(64 * gib >> 10),
		"sys/fs/cgroup/memory.max":                "max\n",
		"sys/fs/cgroup/docker/abc/memory.max":     "536870912\n",
		"sys/fs/cgroup/docker/abc/memory.current": "1\n",
	})
	mem, err := probeAvailableMemory(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	fn, ok := probeCgroupV2At(root)
	if !ok {
		t.Fatal("no monitor source")
	}
	info, _ := fn()
	if mem.Bytes != info.TotalBytes || mem.Bytes != 512<<20 {
		t.Errorf("the budget is a share of %d and the monitor's total is %d, want both 512 MiB", mem.Bytes, info.TotalBytes)
	}
}
