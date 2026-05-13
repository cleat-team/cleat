package e002

import (
	"github.com/cleat-team/cleat/cleat"
)

// Workflow triggers E002 by using channel send.
func Workflow(h cleat.HostCalls) error {
	ch := make(chan int)
	go func() {
		ch <- 42
	}()
	<-ch
	_, err := h.DurableCall("s", "op", "{}")
	return err
}
