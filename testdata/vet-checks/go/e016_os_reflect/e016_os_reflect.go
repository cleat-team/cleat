package e016

import (
	"os"
	"reflect"

	"github.com/rcownie/cleat/cleat"
)

// Workflow triggers E016 by using os.Getenv and reflect.TypeOf.
func Workflow(h cleat.HostCalls) error {
	_ = os.Getenv("HOME")
	_ = reflect.TypeOf(42)
	_, err := h.DurableCall("s", "op", "{}")
	return err
}
