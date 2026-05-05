// Package errors contains deliberately invalid workflow code to
// test the transformer's validation rules.
package errors

import (
	"github.com/rcownie/durable/durable"
)

// BadWorkflow is an entry point but calls a helper in the durable closure
// that lacks HostCalls access.
func BadWorkflow(h durable.HostCalls, input string) (string, error) {
	result := unthreadedHelper(input)
	return result, nil
}

// leafFunc is a durable leaf — it calls a HostCalls method.
func leafFunc(h durable.HostCalls) {
	h.DurableLog("leaf")
}

// unthreadedHelper calls a durable leaf but does NOT receive h as a
// parameter. This should produce an E010 threading error because it's
// in the closure yet lacks HostCalls access.
func unthreadedHelper(input string) string {
	leafFunc(nil) // would panic at runtime — the threading check catches this
	return "done"
}

// BadWithGoroutine uses a goroutine in a function that calls a durable
// leaf, making it part of the durable closure.
func BadWithGoroutine(h durable.HostCalls) {
	go func() {
		_, _ = h.DurableCall("svc", "op", "{}")
	}()
}

// pureHelper does not use HostCalls and is not in the durable closure.
func pureHelper(input string) string {
	return "pure: " + input
}
