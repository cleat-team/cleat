package e013

import (
	"sync"

	"github.com/cleat-team/cleat/cleat"
)

// Workflow triggers E013 by using sync.Mutex.
func Workflow(h cleat.HostCalls) error {
	var mu sync.Mutex
	mu.Lock()
	mu.Unlock()
	_, err := h.DurableCall("s", "op", "{}")
	return err
}
