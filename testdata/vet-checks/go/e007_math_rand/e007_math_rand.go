package e007

import (
	"math/rand"

	"github.com/rcownie/cleat/cleat"
)

// Workflow triggers E007 by using math/rand.
func Workflow(h cleat.HostCalls) error {
	_ = rand.Intn(10)
	_, err := h.DurableCall("s", "op", "{}")
	return err
}
