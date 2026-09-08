// Package e949nohost is the fixture for the simplest form of cleat#949: an
// entry point that uses a mutex and makes no host call at all.
//
// Before the fix this built clean -- "0 in cleat closure" -- while the same
// function with one h.SetQueryState added was refused with two E013s. The rule
// was enforced on functions that reach a host call and not otherwise, though
// the mutex is exactly as non-deterministic either way.
package e949nohost

import (
	"sync"

	"github.com/cleat-team/cleat/cleat"
)

// Workflow takes HostCalls and never uses it, which is what makes this the
// minimal case: it is a workflow entry point by signature, and Pure by closure.
func Workflow(h cleat.HostCalls) (string, error) {
	var mu sync.Mutex
	mu.Lock()
	defer mu.Unlock()
	return "done", nil
}
