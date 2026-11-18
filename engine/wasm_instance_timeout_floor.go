package engine

import (
	"errors"
	"fmt"
	"time"
)

// MinWasmInstanceTimeout is the smallest non-zero guest-execution time bound cleat accepts from an operator or a tenant
// administrator: the worker's --wasm-instance-timeout flag and a tenant's wasm_instance_timeout_ms setting.
//
// WHY A FLOOR. ABI.md ("There is a cliff below about 200 ms") records the measurement: at an instance timeout of 100ms the soft
// kill reached the guest 0 times in 20 runs, at 150ms 18 of 20, and at 200ms and 240ms 20 of 20 (cleat#3922 review, 2026-11-18).
// Below that range the hard epoch trap, armed in whole 50ms ticks, wins the race to a poll, so the defers of a killed workflow do
// not reliably run. 250ms leaves headroom over the measured 200ms and equals DefaultWasmtimeInterruptMarginTime, but it is written
// out rather than aliased: the floor is operator-facing policy, and it should not move because an engine tuning constant did.
//
// ONE CONSTANT, ONE MESSAGE. The worker flag and the tenant setting share this value and the wording below, so the two refusals
// cannot drift apart.
const MinWasmInstanceTimeout = 250 * time.Millisecond

// ErrWasmInstanceTimeoutBelowFloor matches (errors.Is) every refusal CheckWasmInstanceTimeoutFloor returns. The refusal's text is the
// operator-facing message alone, with no sentinel prefix in front of it.
var ErrWasmInstanceTimeoutBelowFloor = errors.New("wasm instance timeout is below the minimum")

type wasmInstanceTimeoutFloorError struct{ msg string }

func (e *wasmInstanceTimeoutFloorError) Error() string { return e.msg }

func (e *wasmInstanceTimeoutFloorError) Is(target error) bool {
	return target == ErrWasmInstanceTimeoutBelowFloor
}

// CheckWasmInstanceTimeoutFloor refuses a duration above 0 and under MinWasmInstanceTimeout.
//
// 0 is allowed here: what 0 means is the caller's (the worker flag reads it as "disabled, not recommended", a tenant setting as
// "unset, use the operator's flag"). zeroHint finishes the sentence "Raise <subject> to at least 250ms, or <zeroHint>." and says
// what 0 does for THIS caller. A negative duration is not judged: each caller already has its own rule for it.
//
// REFUSED, NOT CLAMPED: the caller asked for one bound and would silently get another.
func CheckWasmInstanceTimeoutFloor(d time.Duration, subject, zeroHint string) error {
	if d <= 0 || d >= MinWasmInstanceTimeout {
		return nil
	}
	return &wasmInstanceTimeoutFloorError{fmt.Sprintf(
		"%s %v is below the %v minimum.\n"+
			"Below about 200ms the hard trap beats the soft kill to a poll, so the defers of a workflow stopped by this timeout do not "+
			"reliably run (measured: 100ms ran them 0 of 20 times, 150ms 18 of 20, 200ms and 240ms 20 of 20; see ABI.md, \"There is a "+
			"cliff below about 200 ms\").\n"+
			"Raise %s to at least %v, or %s.",
		subject, d, MinWasmInstanceTimeout, subject, MinWasmInstanceTimeout, zeroHint)}
}
