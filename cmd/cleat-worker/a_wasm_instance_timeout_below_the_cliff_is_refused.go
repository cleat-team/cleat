package main

import (
	"fmt"
	"time"
)

// minWasmInstanceTimeout is the smallest non-zero --wasm-instance-timeout the worker starts with.
//
// WHY A FLOOR. ABI.md ("There is a cliff below about 200 ms") records the measurement: at an instance timeout of 100ms the soft kill
// reached the guest 0 times in 20 runs, at 150ms 18 of 20, and at 200ms and above 20 of 20 (cleat#3922 review, 2026-11-18). Below
// that range the hard epoch trap, armed in whole 50ms ticks, wins the race to a poll, so the defers of a killed workflow do not
// reliably run. 250ms leaves headroom over the measured 200ms and equals engine.DefaultWasmtimeInterruptMarginTime, but it is
// written out here rather than imported: the floor is an operator-facing policy about the flag, and it should not move because an
// engine tuning constant did.
//
// SCOPE. This guards the worker's flag only. A library user setting the engine option directly, a context deadline, and the epoch
// tick are not covered and still reach the cliff, as ABI.md documents.
const minWasmInstanceTimeout = 250 * time.Millisecond

// validateWasmInstanceTimeout refuses a --wasm-instance-timeout above 0 and under minWasmInstanceTimeout. 0 keeps its meaning
// ("disabled, NOT recommended") and is allowed. A NEGATIVE value is refused too: flag.Duration parses "-1s" without complaint and
// the engine would treat it as an already-expired budget.
//
// REFUSED, NOT CLAMPED, for the reason validateReclaimTimeout gives: the operator asked for one bound and would silently get another.
// A separate function so it can be tested without running the binary.
func validateWasmInstanceTimeout(d time.Duration) error {
	if d == 0 || d >= minWasmInstanceTimeout {
		return nil
	}
	if d < 0 {
		return fmt.Errorf("--wasm-instance-timeout %v is negative. Use 0 to disable it (not recommended), or at least %v.", d, minWasmInstanceTimeout)
	}
	return fmt.Errorf(
		"--wasm-instance-timeout %v is below the %v minimum.\n"+
			"Below about 200ms the hard trap beats the soft kill to a poll, so the defers of a workflow stopped by this timeout do not "+
			"reliably run (measured: 100ms ran them 0 of 20 times, 150ms 18 of 20, 200ms and above 20 of 20; see ABI.md, \"There is a "+
			"cliff below about 200 ms\").\n"+
			"Raise --wasm-instance-timeout to at least %v, or set it to 0 to disable it (not recommended).",
		d, minWasmInstanceTimeout, minWasmInstanceTimeout)
}
