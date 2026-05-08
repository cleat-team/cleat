package e015

import (
	"fmt"

	"github.com/rcownie/cleat/durable"
)

// Workflow triggers E015 by using fmt.Println.
func Workflow(h cleat.HostCalls) error {
	fmt.Println("hello vet")
	_, err := h.DurableCall("s", "op", "{}")
	return err
}
