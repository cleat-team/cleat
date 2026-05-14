// Package cleattest_test provides executable examples that appear in
// go doc output. These demonstrate common testing patterns with the
// cleattest harness.
package cleattest_test

import (
	"fmt"
	"time"

	"github.com/cleat-team/cleat/cleat/cleattest"
)

// Example_awaitSignals_basic demonstrates blocking on signals with a
// positive timeout. The workflow goroutine calls h.AwaitSignals, and the
// test delivers a signal via env.Signal(). The SignalResult is checked
// for the signal name, payload, and timeout status.
//
// Note: h.AwaitSignals is a method on cleat.HostCalls, not on TestEnv.
// It corresponds to the workflow-side API, while env.Signal is the
// test-side API for injecting signals.
func Example_awaitSignals_basic() {
	env := cleattest.NewTestEnv()
	h := env.H()

	received := make(chan struct{})

	// Start a workflow goroutine that waits for a signal.
	go func() {
		sr := h.AwaitSignals([]string{"greeting"}, 1*time.Second)
		if sr.TimedOut {
			fmt.Println("Timed out waiting for signal")
		} else {
			fmt.Printf("Got signal %q with payload %s\n", sr.Name, sr.Payload)
		}
		close(received)
	}()

	// Give the goroutine time to reach AwaitSignals.
	time.Sleep(5 * time.Millisecond)

	// Deliver the signal from the test goroutine.
	env.Signal("greeting", `{"msg":"hello"}`)

	// Wait for the workflow goroutine to finish.
	<-received

	// Output: Got signal "greeting" with payload {"msg":"hello"}
}

// Example_pollSignal demonstrates non-blocking signal checking with
// h.PollSignal(). Unlike AwaitSignals, PollSignal returns immediately
// whether or not a matching signal is pending. It is useful for
// checking whether a signal has arrived without blocking the workflow.
func Example_pollSignal() {
	env := cleattest.NewTestEnv()
	h := env.H()

	// Deliver a signal at the current simulated time.
	env.Signal("notice", `{"event":"started"}`)

	// Poll for the signal — returns immediately.
	payload, found, err := h.PollSignal("notice")
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return
	}
	if found {
		fmt.Printf("Found signal: %s\n", payload)
	}

	// Poll again — the signal was consumed on the first poll.
	_, found, err = h.PollSignal("notice")
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return
	}
	fmt.Printf("Still found: %v\n", found)

	// Output: Found signal: {"event":"started"}
	// Still found: false
}

// ExampleTestEnv_AdvanceTime_basic demonstrates how AdvanceTime wakes a
// DurableSleep and later triggers an AwaitSignals timeout.
// The workflow goroutine sleeps for 1 hour, then waits for a signal
// with a 1-hour timeout. The test advances the simulated clock past
// each boundary.
func ExampleTestEnv_AdvanceTime_basic() {
	env := cleattest.NewTestEnv()
	h := env.H()

	done := make(chan struct{})

	go func() {
		// Sleep for 1 simulated hour.
		h.DurableSleep(1 * time.Hour)
		fmt.Println("Woke from sleep")

		// Then wait for a signal with a 1-hour timeout.
		sr := h.AwaitSignals([]string{"next"}, 1*time.Hour)
		if sr.TimedOut {
			fmt.Println("Signal timed out")
		}
		close(done)
	}()

	// Allow the goroutine to reach DurableSleep.
	time.Sleep(5 * time.Millisecond)

	// Advance past the 1-hour sleep.
	env.AdvanceTime(1 * time.Hour)

	// Allow the goroutine to wake, print, and enter AwaitSignals.
	time.Sleep(50 * time.Millisecond)

	// Advance past the 1-hour signal timeout.
	env.AdvanceTime(1 * time.Hour)

	// Wait for the goroutine to finish.
	<-done

	// Output: Woke from sleep
	// Signal timed out
}

// ExampleTestEnv_Signal_delivery demonstrates that env.Signal() can be
// called BEFORE the workflow starts awaiting, and the signal is queued
// for immediate delivery. This pattern avoids the need for time.Sleep
// in many test scenarios.
func ExampleTestEnv_Signal_delivery() {
	env := cleattest.NewTestEnv()
	h := env.H()

	// Deliver the signal BEFORE starting the workflow goroutine.
	// The signal is queued in the test environment's pending list.
	env.Signal("notice", `{"event":"pre-delivered"}`)

	done := make(chan struct{})

	go func() {
		sr := h.AwaitSignals([]string{"notice"}, 1*time.Second)
		if sr.TimedOut {
			fmt.Println("Timed out (unexpected)")
		} else {
			fmt.Printf("Got signal %q with payload %s\n", sr.Name, sr.Payload)
		}
		close(done)
	}()

	// Wait for the goroutine to receive the queued signal and finish.
	<-done

	// Output: Got signal "notice" with payload {"event":"pre-delivered"}
}

// ExampleTestEnv_OnCall demonstrates the three OnCall matcher variants:
//  1. string matches only that exact request.
//  2. nil matches any request string (fallback).
//  3. func(string) bool provides custom matching logic.
//
// Stubs are consumed in registration order: the first match is used
// and removed from the list.
func ExampleTestEnv_OnCall() {
	env := cleattest.NewTestEnv()
	h := env.H()

	// 1. Exact string matcher — matches only this specific request.
	env.OnCall("notify", "send", `{"to":"urgent"}`).ReturnJSON(
		map[string]string{"status": "priority"}, nil,
	)

	// 2. nil matcher — matches any request (acts as a fallback).
	env.OnCall("notify", "send", nil).Return(`{"status":"sent"}`, nil)

	// 3. Custom func matcher — matches requests with payloads longer than 15 chars.
	env.OnCall("notify", "send", func(req string) bool {
		return len(req) > 15
	}).Return(`{"status":"long"}`, nil)

	// String matcher consumes the short request first.
	resp, _ := h.DurableCall("notify", "send", `{"to":"urgent"}`)
	fmt.Println("urgent:", resp)

	// Nil matcher (now first in remaining list) matches any request.
	resp, _ = h.DurableCall("notify", "send", `{"to":"anyone"}`)
	fmt.Println("generic:", resp)

	// Func matcher consumes the long request.
	resp, _ = h.DurableCall("notify", "send", `{"to":"someone","priority":"normal"}`)
	fmt.Println("long:", resp)

	// Output: urgent: {"status":"priority"}
	// generic: {"status":"sent"}
	// long: {"status":"long"}
}
