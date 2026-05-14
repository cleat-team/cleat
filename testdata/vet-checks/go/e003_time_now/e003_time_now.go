package e003

import (
	"time"

	"github.com/cleat-team/cleat/cleat"
)

// Workflow triggers E003 by using time.Now().
func Workflow(h cleat.HostCalls) error {
	_ = time.Now()
	_, err := h.DurableCall("s", "op", "{}")
	return err
}
