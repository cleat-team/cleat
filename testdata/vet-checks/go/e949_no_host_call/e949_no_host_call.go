// Package e949nohost is the fixture for the half of cleat#949 that #964 left:
// a workflow ENTRY POINT that uses a mutex and makes no host call.
//
// #964 fixed the helper case by walking downward from durable functions to
// their callees. This function is neither -- it is not durable (it calls no
// host function) and it is not the callee of anything durable -- so that
// traversal never reaches it, and it still built clean.
//
// It is the FIRST example in the issue:
//
//	Found 1 functions, 1 entry point(s), 0 in cleat closure.
//	Wrote handle_sync_mutex.wasm          <- no E013, builds, deploys
//
// while the same function with one h.SetQueryState added was refused with two
// E013s. A mutex is exactly as non-deterministic either way: replay re-runs the
// workflow body, and this body is the whole workflow.
package e949nohost

import (
	"sync"

	"github.com/cleat-team/cleat/cleat"
)

// Workflow takes HostCalls and never uses it, which is what makes this the
// minimal case: a workflow entry point by signature, Pure by closure.
func Workflow(h cleat.HostCalls) (string, error) {
	var mu sync.Mutex
	mu.Lock()
	defer mu.Unlock()
	return "done", nil
}
