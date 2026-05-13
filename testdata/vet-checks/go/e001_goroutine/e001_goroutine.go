package e001

import (
	"github.com/cleat-team/cleat/cleat"
)

// Workflow triggers E001 by using a goroutine.
func Workflow(h cleat.HostCalls) error {
	go func() {}()
	_, err := h.DurableCall("s", "op", "{}")
	return err
}
