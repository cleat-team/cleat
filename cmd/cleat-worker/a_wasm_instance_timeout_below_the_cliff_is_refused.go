package main

import (
	"fmt"
	"time"

	"github.com/cleat-team/cleat/engine"
)

// The floor itself, and the wording of the refusal, live in engine (engine.MinWasmInstanceTimeout and
// engine.CheckWasmInstanceTimeoutFloor), shared with the tenant's wasm_instance_timeout_ms setting so the two cannot drift. See there
// for why it exists and for the measurement behind it.
//
// SCOPE. This guards the worker's flag only. A library user setting the engine option directly, a context deadline, and the epoch
// tick are not covered and still reach the cliff, as ABI.md documents.

// validateWasmInstanceTimeout refuses a --wasm-instance-timeout above 0 and under minWasmInstanceTimeout. 0 keeps its meaning
// ("disabled, NOT recommended") and is allowed. A NEGATIVE value is refused too: flag.Duration parses "-1s" without complaint and
// the engine would treat it as an already-expired budget.
//
// REFUSED, NOT CLAMPED, for the reason validateReclaimTimeout gives: the operator asked for one bound and would silently get another.
// A separate function so it can be tested without running the binary.
func validateWasmInstanceTimeout(d time.Duration) error {
	if d < 0 {
		return fmt.Errorf("--wasm-instance-timeout %v is negative. Use 0 to disable it (not recommended), or at least %v.", d, engine.MinWasmInstanceTimeout)
	}
	return engine.CheckWasmInstanceTimeoutFloor(d, "--wasm-instance-timeout", "set it to 0 to disable it (not recommended)")
}
