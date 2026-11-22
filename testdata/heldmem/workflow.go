// Package main is a MEASUREMENT fixture (cleat#4162, residency memory budget): make `calls` durable calls (the recorded history a real workflow carries), keep `mb` MiB of
// touched live data, then block in one final call named "park". Input: {"calls":N,"mb":M}.
package main

import (
	"runtime"
	"strconv"

	"github.com/cleat-team/cleat/cleat"
)

var keep [][]byte

func nums(input string) []int {
	var out []int
	start := -1
	for i := 0; i <= len(input); i++ {
		digit := i < len(input) && input[i] >= '0' && input[i] <= '9'
		if digit && start < 0 {
			start = i
		}
		if !digit && start >= 0 {
			n, _ := strconv.Atoi(input[start:i])
			out = append(out, n)
			start = -1
		}
	}
	return out
}

// HeldMem is the guest.
func HeldMem(h cleat.HostCalls, input string) (string, error) {
	n := nums(input)
	calls, mb := 0, 0
	if len(n) > 0 {
		calls = n[0]
	}
	if len(n) > 1 {
		mb = n[1]
	}
	for i := 0; i < calls; i++ {
		if _, err := h.DurableCall("work", "step", `{"stage":"step"}`); err != nil {
			return "", err
		}
	}
	for i := 0; i < mb; i++ {
		b := make([]byte, 1<<20)
		for j := 0; j < len(b); j += 4096 {
			b[j] = 1
		}
		keep = append(keep, b)
	}
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	if _, err := h.DurableCall("work", "park", `{"stage":"park","sys":`+strconv.FormatUint(ms.Sys, 10)+`}`); err != nil {
		return "", err
	}
	return `{"kept":` + strconv.Itoa(len(keep)) + `}`, nil
}
