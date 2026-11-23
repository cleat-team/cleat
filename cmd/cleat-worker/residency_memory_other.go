//go:build !darwin

package main

// physicalMemory has no source beyond /proc/meminfo off macOS.
func physicalMemory() (uint64, bool) { return 0, false }
