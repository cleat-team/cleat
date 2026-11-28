//go:build darwin

package main

import "golang.org/x/sys/unix"

// physicalMemory is hw.memsize: macOS has no /proc/meminfo and no cgroup, so a developer machine's budget is a share of its RAM.
func physicalMemory() (uint64, bool) {
	n, err := unix.SysctlUint64("hw.memsize")
	return n, err == nil && n > 0
}
