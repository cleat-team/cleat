package e009

import (
	"github.com/rcownie/cleat/durable"
)

// durableHelper is a durable leaf function that uses HostCalls.
func durableHelper(h cleat.HostCalls) {
	h.DurableLog("helper called")
}

// init calls a durable function from init, which is forbidden.
func init() {
	var h cleat.HostCalls
	durableHelper(h)
}

// Workflow is the entry point.
func Workflow(h cleat.HostCalls) string {
	durableHelper(h)
	return "ok"
}
