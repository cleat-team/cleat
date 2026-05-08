package e005

import (
	"net/http"

	"github.com/rcownie/cleat/cleat"
)

// Workflow triggers E005 by using net/http in a durable function.
func Workflow(h cleat.HostCalls) error {
	_, httpErr := http.Get("https://example.com")
	_, err := h.DurableCall("s", "op", "{}")
	if err != nil {
		return err
	}
	return httpErr
}
