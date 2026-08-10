// Package main implements OS-level memory monitoring for the cleat-worker
// daemon. It probes for the best available memory source (cgroup v2,
// /proc/meminfo, or Go heap stats) and provides cached reads with a
// configurable check interval.
package main

import (
	"bufio"
	"context"
	"log/slog"
	"math"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// MemoryInfo is a point-in-time snapshot of memory usage.
type MemoryInfo struct {
	AvailableBytes uint64
	UsedBytes      uint64
	TotalBytes     uint64
	Source         string // "cgroupv2", "procmeminfo", "goheap"
	CollectedAt    time.Time
}

// MemoryMonitor reads system/container memory on demand with caching.
type MemoryMonitor struct {
	checkInterval time.Duration
	readFn        func() (MemoryInfo, error)
	lastReading   *MemoryInfo
	lastReadAt    time.Time
	mu            sync.RWMutex

	logger *slog.Logger
}

func (m *MemoryMonitor) log() *slog.Logger {
	if m.logger != nil {
		return m.logger
	}
	return slog.Default()
}

// NewMemoryMonitor probes for the best available memory source and returns a
// MemoryMonitor that uses it consistently.
//
// Probe order:
//  1. cgroup v2 (/sys/fs/cgroup/memory.max + memory.current)
//  2. /proc/meminfo (MemTotal + MemAvailable)
//  3. Go runtime heap stats (runtime.ReadMemStats)
func NewMemoryMonitor(interval time.Duration) *MemoryMonitor {
	mm := &MemoryMonitor{
		checkInterval: interval,
	}

	// Probe cgroup v2.
	if fn, ok := probeCgroupV2(); ok {
		mm.readFn = fn
		return mm
	}

	// Fall back to /proc/meminfo.
	if fn, ok := probeProcMeminfo(); ok {
		mm.readFn = fn
		return mm
	}

	// Final fallback: Go heap stats.
	mm.readFn = readGoHeap
	return mm
}

// probeCgroupV2 checks for /sys/fs/cgroup/memory.max and returns a read
// function backed by cgroup v2 memory accounting. If memory.max is "max"
// (no explicit limit), /proc/meminfo MemTotal is used for the total.
func probeCgroupV2() (func() (MemoryInfo, error), bool) {
	data, err := os.ReadFile("/sys/fs/cgroup/memory.max")
	if err != nil {
		return nil, false
	}

	totalStr := strings.TrimSpace(string(data))
	var totalBytes uint64
	if totalStr == "max" {
		// No explicit cgroup limit; fall back to /proc/meminfo for total.
		memTotal, ok := readMemTotal()
		if !ok {
			return nil, false
		}
		totalBytes = memTotal
	} else {
		totalBytes, err = strconv.ParseUint(totalStr, 10, 64)
		if err != nil {
			return nil, false
		}
	}

	return func() (MemoryInfo, error) {
		currentData, err := os.ReadFile("/sys/fs/cgroup/memory.current")
		if err != nil {
			return MemoryInfo{}, err
		}
		currentStr := strings.TrimSpace(string(currentData))
		currentBytes, err := strconv.ParseUint(currentStr, 10, 64)
		if err != nil {
			return MemoryInfo{}, err
		}

		used := currentBytes
		avail := totalBytes - used
		if totalBytes < used {
			avail = 0
		}

		return MemoryInfo{
			AvailableBytes: avail,
			UsedBytes:      used,
			TotalBytes:     totalBytes,
			Source:         "cgroupv2",
			CollectedAt:    time.Now(),
		}, nil
	}, true
}

// readMemTotal parses MemTotal from /proc/meminfo and returns it in bytes.
func readMemTotal() (uint64, bool) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, false
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "MemTotal:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				kb, err := strconv.ParseUint(fields[1], 10, 64)
				if err != nil {
					return 0, false
				}
				return kb * 1024, true
			}
		}
	}
	return 0, false
}

// probeProcMeminfo checks for /proc/meminfo and returns a read function
// backed by MemTotal and MemAvailable parsing.
func probeProcMeminfo() (func() (MemoryInfo, error), bool) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return nil, false
	}
	f.Close()

	return readProcMeminfo, true
}

// readProcMeminfo parses /proc/meminfo and returns a MemoryInfo snapshot.
// MemAvailable is the kernel's best estimate including reclaimable cache.
func readProcMeminfo() (MemoryInfo, error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return MemoryInfo{}, err
	}
	defer f.Close()

	var memTotal, memAvail uint64
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		key := fields[0]
		val, parseErr := strconv.ParseUint(fields[1], 10, 64)
		if parseErr != nil {
			continue
		}
		switch {
		case strings.HasPrefix(key, "MemTotal:"):
			memTotal = val * 1024 // kB to bytes
		case strings.HasPrefix(key, "MemAvailable:"):
			memAvail = val * 1024 // kB to bytes
		}
	}

	if memTotal == 0 {
		return MemoryInfo{}, os.ErrNotExist
	}
	if memAvail > memTotal {
		memAvail = memTotal
	}

	return MemoryInfo{
		AvailableBytes: memAvail,
		UsedBytes:      memTotal - memAvail,
		TotalBytes:     memTotal,
		Source:         "procmeminfo",
		CollectedAt:    time.Now(),
	}, nil
}

// readGoHeap uses runtime.ReadMemStats as a final fallback memory source.
// TotalBytes is mem.Sys (OS memory obtained via mmap), UsedBytes is
// mem.HeapInuse, and AvailableBytes is the difference.
func readGoHeap() (MemoryInfo, error) {
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)

	total := mem.Sys
	used := mem.HeapInuse
	avail := total - used
	if total < used {
		avail = 0
	}

	return MemoryInfo{
		AvailableBytes: avail,
		UsedBytes:      used,
		TotalBytes:     total,
		Source:         "goheap",
		CollectedAt:    time.Now(),
	}, nil
}

// Read returns a cached MemoryInfo if the last reading is within
// checkInterval. Otherwise it calls the stored read function.
func (m *MemoryMonitor) Read() MemoryInfo {
	m.mu.RLock()
	if m.lastReading != nil && time.Since(m.lastReadAt) < m.checkInterval {
		info := *m.lastReading
		m.mu.RUnlock()
		return info
	}
	m.mu.RUnlock()

	m.mu.Lock()
	defer m.mu.Unlock()

	// Double-check after acquiring write lock.
	if m.lastReading != nil && time.Since(m.lastReadAt) < m.checkInterval {
		return *m.lastReading
	}

	info, err := m.readFn()
	if err != nil {
		m.log().WarnContext(context.Background(), "memory monitor read failed", "error", err)
		if m.lastReading != nil {
			return *m.lastReading
		}
		return MemoryInfo{}
	}

	m.lastReading = &info
	m.lastReadAt = time.Now()
	return info
}

// SampleUsage bypasses the cache and always reads fresh memory statistics.
// Returns UsedBytes. Useful for before/after workflow memory delta measurement.
func (m *MemoryMonitor) SampleUsage() uint64 {
	info, err := m.readFn()
	if err != nil {
		m.log().WarnContext(context.Background(), "memory monitor sample failed", "error", err)
		return 0
	}
	return info.UsedBytes
}

// PressureLevel computes a normalized 0.0-1.0 memory pressure value.
//
//   - If AvailableBytes > 0 (e.g. /proc/meminfo): usedFraction = 1 - avail/total
//   - Otherwise: usedFraction = used/total
//   - usedFraction <= softFraction: returns 0.0
//   - usedFraction >= hardFraction: returns 1.0
//   - Otherwise: linearly interpolates between softFraction and hardFraction
func PressureLevel(info MemoryInfo, softFraction, hardFraction float64) float64 {
	var usedFraction float64
	if info.TotalBytes == 0 {
		return 1.0
	}

	if info.AvailableBytes > 0 {
		usedFraction = 1.0 - (float64(info.AvailableBytes) / float64(info.TotalBytes))
	} else {
		usedFraction = float64(info.UsedBytes) / float64(info.TotalBytes)
	}

	if math.IsNaN(usedFraction) || math.IsInf(usedFraction, 0) {
		return 1.0
	}

	if usedFraction <= softFraction {
		return 0.0
	}
	if usedFraction >= hardFraction {
		return 1.0
	}
	return (usedFraction - softFraction) / (hardFraction - softFraction)
}
