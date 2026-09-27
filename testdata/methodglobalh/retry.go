// Package methodglobalh is a NEGATIVE fixture: `cleat build` must reject it.
//
// It declares the package-level context object that testdata/autothread uses
// legitimately, and then reaches the host from a METHOD. Auto-threading works
// by prepending h as a parameter and rewriting call sites, which cannot be
// done to a receiver, so the global survives the transform unassigned and
// every call through it dereferences a nil *HostCallsImpl at run time.
//
// Before cleat#1614 this package built clean -- "Verifying HostCalls
// threading... OK" -- and produced a .wasm that panicked on first use.
package methodglobalh

import (
	"time"

	"github.com/cleat-team/cleat/cleat"
)

var h cleat.HostCalls

// Retrier has no HostCalls field. That is the whole point: a receiver that
// carries one is the supported shape and is admitted by phase 3.
type Retrier struct {
	Attempts int
}

// Wait is the offending method.
func (r *Retrier) Wait() {
	h.DurableSleep(5 * time.Second)
}

// RetryOrder is the entry point that makes Wait durable-reachable.
func RetryOrder(h cleat.HostCalls, orderID string, attempts int) (string, error) {
	r := &Retrier{Attempts: attempts}
	r.Wait()
	return orderID, nil
}
