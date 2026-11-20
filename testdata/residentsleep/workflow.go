// Package main is a fixture for residency (cleat#4162): a guest that sleeps and then makes one call.
//
// The call after the sleep is what a test can see. A sleep that was HELD to its end lets the guest reach the call in the same execution; a sleep that
// SUSPENDED stops the execution before it. A run that is held through its own ceiling is the case the fixture exists for.
package main

import (
	"strconv"

	"github.com/cleat-team/cleat/cleat"
)

// SleepThenCall sleeps for 300ms (durably) and then makes one call.
func SleepThenCall(h cleat.HostCalls, input string) (string, error) {
	h.DurableSleepMs(300)
	if _, err := h.DurableCall("work", "after_sleep", `{}`); err != nil {
		return "", err
	}
	return `{"status":"ok"}`, nil
}

// WorkSleepWork makes a call, sleeps for 300ms (durably), then makes another call: the poll-then-act shape. With a host that takes 300ms per call it needs 900ms
// of one execution if the sleep is HELD, and only 300ms per execution if the sleep suspends, which is the difference the known-limit test pins.
func WorkSleepWork(h cleat.HostCalls, input string) (string, error) {
	if _, err := h.DurableCall("work", "before_sleep", `{}`); err != nil {
		return "", err
	}
	h.DurableSleepMs(300)
	if _, err := h.DurableCall("work", "after_sleep", `{}`); err != nil {
		return "", err
	}
	return `{"status":"ok"}`, nil
}

// spin burns guest CPU: n iterations of arithmetic whose result is returned, so that nothing can be optimised away. It is the only thing in this fixture that
// spends GUEST EXECUTION time (a host call does not), which is what the epoch fence charges for.
func spin(n int) uint64 {
	var x uint64
	for i := 0; i < n; i++ {
		x += uint64(i) * 2654435761
	}
	return x
}

// digits reads the first run of digits in the input, so a test can size the work without a JSON dependency: `{"n":50000000}`.
func digits(input string) int {
	start := -1
	for i := 0; i < len(input); i++ {
		if input[i] >= '0' && input[i] <= '9' {
			if start < 0 {
				start = i
			}
			continue
		}
		if start >= 0 {
			n, _ := strconv.Atoi(input[start:i])
			return n
		}
	}
	if start >= 0 {
		n, _ := strconv.Atoi(input[start:])
		return n
	}
	return 0
}

// SpinOnly computes for n iterations and returns: how long the guest's own work takes on this machine, so a test can size the fence from it.
func SpinOnly(h cleat.HostCalls, input string) (string, error) {
	return `{"x":` + strconv.FormatUint(spin(digits(input)), 10) + `}`, nil
}

// SpinSleepSpin computes, sleeps for 300ms (durably), and computes the same again. Under a fence smaller than twice the work, the second spin is the one that
// is killed unless the fence was restarted when the held sleep ended.
func SpinSleepSpin(h cleat.HostCalls, input string) (string, error) {
	n := digits(input)
	a := spin(n)
	h.DurableSleepMs(300)
	b := spin(n)
	return `{"status":"ok","x":` + strconv.FormatUint(a+b, 10) + `}`, nil
}

// SpinTwice computes twice in ONE segment with no wait between: the known positive for the fence row. If this is not killed by a fence that one spin fits,
// the fence is too generous for two spins to be charged to it, and a "completes" from SpinSleepSpin proves nothing about a restart.
func SpinTwice(h cleat.HostCalls, input string) (string, error) {
	n := digits(input)
	a := spin(n)
	b := spin(n)
	return `{"status":"ok","x":` + strconv.FormatUint(a+b, 10) + `}`, nil
}
