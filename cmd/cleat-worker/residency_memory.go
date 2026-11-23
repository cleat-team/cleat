package main

import (
	"errors"
	"fmt"
	"os"
	"path"
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
//  1. a cgroup v2 limit (memory.max) on the process's own cgroup or any ancestor, the tightest, when it is a number and below the host's memory;
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

// readCgroupLimit finds the tightest memory limit on the process's OWN cgroup and every ancestor of it, up to the mount's root.
//
// The root of the cgroup mount is the process's own cgroup only inside a PRIVATE cgroup namespace, which is what a container runtime usually gives. Under a host cgroup
// namespace (docker run --cgroupns=host, a systemd unit with MemoryMax=, some Kubernetes configurations) the mount shows the whole host, the root's memory.max is "max", and
// the container's limit sits at its own path from /proc/self/cgroup (cleat-review3 measured --memory 512m reading as no limit under --cgroupns=host: a budget of 2403 MiB
// against 102). So the path is read, and every directory from it to the root is consulted: a limit on a parent bounds the child too, so the answer is the minimum, and the
// root read stays as the last step, which is the whole answer in a private namespace.
func readCgroupLimit(root string) (uint64, string, bool) {
	if n, _, ok := cgroupV2Limit(root); ok {
		return n, memSourceCgroupV2, true
	}
	_, v1Path := selfCgroupPaths(root + "proc/self/cgroup")
	var best uint64
	for _, dir := range ancestors(v1Path) {
		if n, ok := parseLimit(root + "sys/fs/cgroup/memory" + dir + "memory.limit_in_bytes"); ok && n < 1<<62 && (best == 0 || n < best) {
			best = n
		}
	}
	return best, memSourceCgroupV1, best != 0
}

// cgroupV2Limit is the tightest numeric memory.max on the process's own cgroup (from /proc/self/cgroup) and every ancestor up to the mount's root, and the directory
// that limit lives in, relative to the cgroup mount and ending in a slash ("/docker/abc/"). A parent's limit bounds its children, and the usage that limit is measured
// against is that directory's memory.current, which is why the directory is returned and not only the number: the memory monitor reads its usage from it (cleat#4431), so
// the budget and the backstop are measured against the SAME cgroup. This is the one resolver both use.
func cgroupV2Limit(root string) (limit uint64, dir string, ok bool) {
	v2Path, _ := selfCgroupPaths(root + "proc/self/cgroup")
	for _, d := range ancestors(v2Path) {
		if n, found := parseLimit(root + "sys/fs/cgroup" + d + "memory.max"); found && (limit == 0 || n < limit) {
			limit, dir = n, d
		}
	}
	return limit, dir, limit != 0
}

// ownCgroupV2Dir is the process's own cgroup directory, in the same form as cgroupV2Limit's, for reading usage when no level of the hierarchy has a limit.
func ownCgroupV2Dir(root string) string {
	v2, _ := selfCgroupPaths(root + "proc/self/cgroup")
	if v2 == "/" {
		return "/"
	}
	return v2 + "/"
}

// parseLimit reads a cgroup limit file: a positive number, or false for "max", a missing file or garbage.
func parseLimit(path string) (uint64, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	n, err := strconv.ParseUint(strings.TrimSpace(string(raw)), 10, 64)
	return n, err == nil && n > 0
}

// selfCgroupPaths returns the process's cgroup path for v2 (the "0::" line) and for the v1 memory controller, each "/" when the file is missing or has no such line, which
// reads only the mount's root: exactly what this probe did before it looked at the path at all.
func selfCgroupPaths(file string) (v2, v1 string) {
	v2, v1 = "/", "/"
	raw, err := os.ReadFile(file)
	if err != nil {
		return v2, v1
	}
	for _, line := range strings.Split(string(raw), "\n") {
		f := strings.SplitN(line, ":", 3)
		if len(f) != 3 {
			continue
		}
		switch {
		case f[0] == "0" && f[1] == "":
			v2 = cleanCgroupPath(f[2])
		case f[1] == "memory" || strings.HasPrefix(f[1], "memory,") || strings.HasSuffix(f[1], ",memory") || strings.Contains(f[1], ",memory,"):
			v1 = cleanCgroupPath(f[2])
		}
	}
	return v2, v1
}

// cleanCgroupPath makes a /proc/self/cgroup path safe to append to a root: rooted, no "..", and without the " (deleted)" suffix the kernel adds to a removed cgroup.
func cleanCgroupPath(p string) string {
	p = strings.TrimSuffix(strings.TrimSpace(p), " (deleted)")
	return path.Clean("/" + p)
}

// ancestors lists p and every parent of it up to and including "/", each with a trailing slash, deepest first: "/a/b" gives "/a/b/", "/a/", "/".
func ancestors(p string) []string {
	var out []string
	for {
		if p == "/" || p == "." || p == "" {
			return append(out, "/")
		}
		out = append(out, p+"/")
		p = path.Dir(p)
	}
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
