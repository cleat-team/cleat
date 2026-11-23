package main

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// The memory a worker's held runs are a share of (cleat#4377).
//
// "Available" means the LIMIT, not the limit minus what is in use now. A budget that shrank as running runs grew would refuse by load, and the dynamic backstop
// already exists: --memory-soft-limit reads the cgroup's real memory.current and stops a hold while the worker is under pressure. This reads one number, once, at
// start-up, and the budget is that number times --resident-memory-percent.
//
// The order, which is the order of how much the number means:
//  1. a cgroup v2 limit (memory.max), when it is a number and below the host's memory;
//  2. a cgroup v1 limit (memory.limit_in_bytes), the same, and treating the "unlimited" sentinel (a number near 2^63) as no limit;
//  3. the host's PHYSICAL memory, with a warning, because on a host shared with other tenants a share of physical RAM can be most of what they need;
//  4. nothing readable: an error, and the caller falls back to the old fixed bound. Residency is never turned on silently with an unknown budget.
//
// A limit at or above the host's memory is no limit at all (a v1 container started with no -m reports a huge round number, and a v2 limit above the host cannot
// be reached), so it is read as step 3 and not as step 1.

const (
	memSourceCgroupV2 = "cgroup v2 memory.max"
	memSourceCgroupV1 = "cgroup v1 memory.limit_in_bytes"
	memSourcePhysical = "physical memory (no cgroup limit)"
)

// errNoMemorySource is returned when neither a cgroup limit nor the host's memory can be read.
var errNoMemorySource = errors.New("neither a cgroup memory limit nor the host's physical memory could be read")

// availableMemory is what the held runs' budget is a share of, and where it came from.
type availableMemory struct {
	Bytes  uint64
	Source string
	// Warning is non-empty when the number is one an operator should look at (physical memory on what may be a shared host).
	Warning string
}

// probeAvailableMemory reads the sources above under root ("/" in production; a temporary directory in a test, laid out like /). physical reports the host's memory
// where /proc/meminfo does not exist (macOS).
func probeAvailableMemory(root string, physical func() (uint64, bool)) (availableMemory, error) {
	total, haveTotal := readMeminfoTotal(root + "proc/meminfo")
	if !haveTotal && physical != nil {
		total, haveTotal = physical()
	}

	limit, source, haveLimit := readCgroupLimit(root)
	// A limit that is not below the host's memory is not a limit.
	if haveLimit && haveTotal && limit >= total {
		haveLimit = false
	}
	if haveLimit {
		return availableMemory{Bytes: limit, Source: source}, nil
	}
	if haveTotal {
		return availableMemory{
			Bytes:  total,
			Source: memSourcePhysical,
			Warning: "no cgroup memory limit was found, so the held runs' budget is a share of the host's PHYSICAL memory. On a host shared with other " +
				"workloads that can be most of what they need: set a container or cgroup memory limit, or lower --resident-memory-percent",
		}, nil
	}
	return availableMemory{}, errNoMemorySource
}

func readCgroupLimit(root string) (uint64, string, bool) {
	if raw, err := os.ReadFile(root + "sys/fs/cgroup/memory.max"); err == nil {
		s := strings.TrimSpace(string(raw))
		if n, err := strconv.ParseUint(s, 10, 64); err == nil && n > 0 {
			return n, memSourceCgroupV2, true
		}
		// "max" is v2's "no limit", and anything unreadable is treated the same way: the caller falls through to the host's memory.
	}
	if raw, err := os.ReadFile(root + "sys/fs/cgroup/memory/memory.limit_in_bytes"); err == nil {
		if n, err := strconv.ParseUint(strings.TrimSpace(string(raw)), 10, 64); err == nil && n > 0 && n < 1<<62 {
			return n, memSourceCgroupV1, true
		}
	}
	return 0, "", false
}

// readMeminfoTotal parses MemTotal (in kB) from a /proc/meminfo-shaped file.
func readMeminfoTotal(path string) (uint64, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 2 {
			return 0, false
		}
		kb, err := strconv.ParseUint(f[1], 10, 64)
		if err != nil || kb == 0 {
			return 0, false
		}
		return kb << 10, true
	}
	return 0, false
}

// residencyBudgetBytes is percent of available, rounded down. percent is already validated to be in (0, 100].
func residencyBudgetBytes(available uint64, percent float64) uint64 {
	return uint64(float64(available) * percent / 100)
}

func validateResidentMemoryPercent(p float64) error {
	if p != p || p < 0 || p > 100 {
		return fmt.Errorf("--resident-memory-percent %g must be between 0 and 100 (0 turns residency off)", p)
	}
	return nil
}
