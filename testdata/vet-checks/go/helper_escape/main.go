// Package helperescape reproduces cleat#949: a workflow entry point that is
// durable, calling a helper that is not.
//
// The helper makes no host call, so it is tagged Pure by closure analysis and
// was never handed to validateConstructs -- while the workflow re-executes it
// on every replay, because replay re-runs the workflow body and only constrains
// the results of host calls, not local computation.
package helperescape

import (
	"sync"

	"github.com/cleat-team/cleat/cleat"
)

// nonDeterministicHelper makes no host call and is therefore Pure, but it is
// reached from a durable entry point and runs on every replay. It carries
// goroutines (E001), channel operations (E002), and sync.Mutex and
// sync.WaitGroup (E013).
func nonDeterministicHelper(key string) string {
	var mu sync.Mutex
	results := make(chan string, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			mu.Lock()
			defer mu.Unlock()
			results <- key
		}(i)
	}
	wg.Wait()
	close(results)
	out := ""
	for r := range results {
		out += r
	}
	return out
}

// Workflow is durable: it makes a host call, so it is analysed. Its result is
// the helper's output, which is exactly the value replay must reproduce.
func Workflow(h cleat.HostCalls, key string) (string, error) {
	if _, err := h.DurableCall("svc", "op", "{}"); err != nil {
		return "", err
	}
	return nonDeterministicHelper(key), nil
}
