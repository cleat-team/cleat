// Package e949helper is the fixture for cleat#949: an entry point that DOES
// call the host, calling a helper that does not.
//
// The helper is Pure by the closure's reckoning -- nothing it calls reaches a
// host function -- so before the fix it was never validated, and its six
// violations across three codes were reported as none. The analyzer's own
// summary said "2 functions, 1 in cleat closure": it counted the helper
// without checking it.
//
// Replay re-runs the workflow and constrains only the results of host calls;
// local computation is re-executed. So this helper runs again on every replay,
// with a different goroutine interleaving each time.
package e949helper

import (
	"sync"

	"github.com/cleat-team/cleat/cleat"
)

// Workflow is in the durable closure: it calls the host.
func Workflow(h cleat.HostCalls) error {
	h.SetQueryState("phase", nonDeterministicHelper("k"))
	return nil
}

// nonDeterministicHelper makes no host call and is therefore Pure.
//
// E001 goroutine, E002 channel send/receive/close, E013 sync.Mutex and
// sync.WaitGroup -- none of which were reported before cleat#949.
func nonDeterministicHelper(key string) string {
	var mu sync.Mutex
	var wg sync.WaitGroup
	results := make(chan string, 2)

	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			mu.Lock()
			defer mu.Unlock()
			results <- key
		}()
	}
	wg.Wait()
	close(results)

	out := ""
	for r := range results {
		out += r
	}
	return out
}
