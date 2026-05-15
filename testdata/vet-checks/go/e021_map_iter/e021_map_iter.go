package e021

import (
	"github.com/cleat-team/cleat/cleat"
)

// Workflow triggers E021 by iterating over a map.
func Workflow(h cleat.HostCalls) error {
	scores := map[string]int{"alice": 95, "bob": 87}
	for name, score := range scores {
		_ = name
		_ = score
	}
	_, err := h.DurableCall("s", "op", "{}")
	return err
}
