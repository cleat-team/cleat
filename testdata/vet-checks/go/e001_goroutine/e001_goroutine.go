package e001

import (
	"github.com/rcownie/cleat/durable"
)

// Workflow triggers E001 by using a goroutine.
func Workflow(h cleat.HostCalls) error {
	go func() {}()
	_, err := h.DurableCall("s", "op", "{}")
	return err
}
