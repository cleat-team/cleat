// Package main is a fixture for residency (cleat#4162): a guest that sleeps and then makes one call.
//
// The call after the sleep is what a test can see. A sleep that was HELD to its end lets the guest reach the call in the same execution; a sleep that
// SUSPENDED stops the execution before it. A run that is held through its own ceiling is the case the fixture exists for.
package main

import (
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
