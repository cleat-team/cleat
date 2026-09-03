//go:build cgo

package engine

import (
	"fmt"
	"reflect"
	"time"

	wasmtime "github.com/bytecodealliance/wasmtime-go/v44"
)

// The instance-timeout fence measures GUEST EXECUTION, not wall clock.
// IMPROVEMENT-PLAN 3.90.
//
// --wasm-instance-timeout is enforced by epoch interruption, and the epoch
// ticker (startEpochTicker) is free-running: it increments every 50ms whether
// the guest is executing or parked inside a host call. Before this file the
// deadline was set once, at store setup, and never touched again, so anything
// the host did on the guest's behalf -- a service call, a plugin call, a DB
// write, a retry loop's backoff -- was charged to the guest's runaway budget.
//
// Measured before the fix (engine/instance_timeout_hostwait_test.go): a
// workflow whose own cost is under 2s, making one call to a service that took
// 4s, died under a 5s budget. With the 30s default, three 12s service calls
// trip a fence whose error says "execution time limit exceeded" -- which reads
// as a guest that would not stop, and sends whoever debugs it at the wrong half
// of the system.
//
// hostBudget makes the fence mean what its flag says. The guest's remaining
// time is decremented only while the guest is running: entering a host function
// charges what the guest just used, leaving one re-arms the deadline with what
// is left. Time spent inside the host costs the guest nothing.
//
// WHAT THIS DOES NOT WEAKEN, which is the whole point of the pair of tests over
// it: a guest that never calls back into the host never enters a bracket, so
// its deadline is never re-armed and the fence fires exactly as before. Fuel
// (--wasm-instruction-limit) is untouched.
//
// Safe to keep on the backend struct because Execute runs on a PER-EXECUTION
// backend -- executor.go calls PerExecution() at :194 and :615, which is what
// makes b.handler and b.envNeeded safe there too. A budget on the root backend
// would be shared by every concurrent workflow.
type hostBudget struct {
	store *wasmtime.Store

	// remaining is guest-execution time left, not wall time left.
	remaining time.Duration

	// resumedAt is when the guest last started running.
	resumedAt time.Time

	// depth guards against a nested host call double-charging. Nothing in the
	// tree does this today; it costs one int to not depend on that.
	depth int
}

// newHostBudget returns a budget for one invocation, or nil when the fence is
// disabled. A nil *hostBudget is usable: every method tolerates it, so the
// disabled case needs no branch at the call sites.
func newHostBudget(store *wasmtime.Store, timeout time.Duration) *hostBudget {
	if timeout <= 0 {
		return nil
	}
	return &hostBudget{store: store, remaining: timeout}
}

// arm re-applies the deadline and starts the guest's clock. Call it immediately
// before handing control to the guest.
//
// This is also where the budget stops paying for module instantiation.
// configureStore sets the deadline when the STORE is created, which on a cold
// module is before compilation; whatever that took came out of the guest's
// budget. Arming here resets it to the full amount at the moment the guest
// actually starts.
func (h *hostBudget) arm() {
	if h == nil {
		return
	}
	h.setDeadline()
	h.resumedAt = time.Now()
}

// enterHost charges the guest for the time it has just run.
func (h *hostBudget) enterHost() {
	if h == nil {
		return
	}
	h.depth++
	if h.depth > 1 {
		return
	}
	if !h.resumedAt.IsZero() {
		h.remaining -= time.Since(h.resumedAt)
	}
}

// leaveHost re-arms the deadline with what the guest has left, and does not
// charge it for the host call that just finished.
func (h *hostBudget) leaveHost() {
	if h == nil {
		return
	}
	h.depth--
	if h.depth > 0 {
		return
	}
	h.arm()
}

// setDeadline converts the remaining guest time into epoch ticks.
//
// The floor of one tick is deliberate and matches configureStore: an exhausted
// guest is interrupted at the next tick rather than being handed a deadline it
// has already passed, which wasmtime would treat as no deadline at all.
func (h *hostBudget) setDeadline() {
	ticks := uint64(0)
	if h.remaining > 0 {
		ticks = uint64(h.remaining / epochTickInterval)
	}
	if ticks == 0 {
		ticks = 1
	}
	h.store.SetEpochDeadline(ticks)
}

// hostFunc registers a host function with the guest's budget bracketed around
// it, and is the ONLY way host functions are registered on this backend.
//
// Uniform rather than selective, and that is a decision worth stating. Picking
// out "the ones that block" would be a judgment call repeated 70 times, and the
// set is not obvious: a service call blocks, but so does a DB write behind an
// event record, a plugin call, and a WASI stub that touches a file. The
// invariant is simply "the guest is not running while the host is", which is
// true at every one of these boundaries, so the bracket goes at all of them and
// nobody has to decide.
//
// scripts/check-hostfunc-budget.sh enforces that no raw linker.FuncWrap call
// remains in the host function files, because the failure mode of the
// selective version is silent: a host function registered the old way is not
// wrong, it just quietly charges the guest for its wait.
// The signature is `any` because these functions have 70 different shapes and
// wasmtime derives the WASM type from the Go type. reflect.MakeFunc produces a
// wrapper of the IDENTICAL reflect.Type, so what FuncWrap sees is unchanged --
// which is what makes one bracket able to cover every shape.
func (b *wasmtimeBackend) hostFunc(linker *wasmtime.Linker, module, name string, f any) error {
	fv := reflect.ValueOf(f)
	if fv.Kind() != reflect.Func {
		return fmt.Errorf("host: %s.%s: not a function", module, name)
	}
	wrapped := reflect.MakeFunc(fv.Type(), func(args []reflect.Value) []reflect.Value {
		b.budget.enterHost()
		defer b.budget.leaveHost()
		return fv.Call(args)
	}).Interface()
	return linker.FuncWrap(module, name, wrapped)
}
