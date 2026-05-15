package no_errors

import (
	"github.com/cleat-team/cleat/cleat"
)

// Workflow is a clean workflow with no forbidden patterns.
func Workflow(h cleat.HostCalls) error {
	_, err := h.DurableCall("s", "op", "{}")
	return err
}
