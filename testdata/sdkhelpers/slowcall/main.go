// Package slowcall is a fixture for cleat#4174: one durable call to a service that takes a long time, and nothing else, so what ends the run
// is what the host does while it waits for the service.
package slowcall

import "github.com/cleat-team/cleat/cleat"

//cleat:entry
func Run(h cleat.HostCalls, input string) (string, error) {
	if _, err := h.DurableCall("slow", "wait", `{}`); err != nil {
		return "", err
	}
	return `{"status":"completed"}`, nil
}
