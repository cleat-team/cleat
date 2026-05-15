// Package errors contains deliberately invalid workflow code to
// test the transformer's validation rules.
package errors

import (
	"io"

	"github.com/cleat-team/cleat/cleat"
)

// BadWorkflow is an entry point but calls a helper in the durable closure
// that lacks HostCalls access.
func BadWorkflow(h cleat.HostCalls, input string) (string, error) {
	result := unthreadedHelper(input)
	return result, nil
}

// leafFunc is a durable leaf — it calls a HostCalls method.
func leafFunc(h cleat.HostCalls) {
	h.DurableLog("leaf")
}

// unthreadedHelper calls a durable leaf but does NOT receive h as a
// parameter. This should produce an E010 threading error because it's
// in the closure yet lacks HostCalls access.
func unthreadedHelper(input string) string {
	leafFunc(cleat.HostCalls{}) // would panic at runtime — the threading check catches this
	return "done"
}

// BadWithGoroutine uses a goroutine in a function that calls a durable
// leaf, making it part of the durable closure.
func BadWithGoroutine(h cleat.HostCalls) {
	go func() {
		_, _ = h.DurableCall("svc", "op", "{}")
	}()
}

// pureHelper does not use HostCalls and is not in the durable closure.
func pureHelper(input string) string {
	return "pure: " + input
}

// BadWithInterfaceDispatch calls a method on an interface, which cannot be statically resolved.
func BadWithInterfaceDispatch(h cleat.HostCalls, reader io.Reader) error {
	buf := make([]byte, 1024)
	_, err := reader.Read(buf) // interface dispatch - unresolvable
	h.DurableLog("read some data")
	return err
}

// BadWithFuncValue stores a function in a variable and calls it.
func BadWithFuncValue(h cleat.HostCalls) {
	fn := func() {
		h.DurableLog("inside func value")
	}
	fn() // function value call - unresolvable
}

// BadWithFloatCondition uses a float in an if condition (non-deterministic).
func BadWithFloatCondition(h cleat.HostCalls) {
	ratio := 0.95
	if ratio > 0.5 {
		h.DurableLog("above threshold")
	}
}
