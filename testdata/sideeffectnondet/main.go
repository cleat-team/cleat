// Package sideeffectnondet is the fixture for cleat#4123: which non-deterministic constructs the closure scan refuses, and where. The
// function literal passed to SideEffect is where a wall-clock read, entropy and a map range are meant to go; the same constructs anywhere
// else in a workflow still fail the build.
package sideeffectnondet

import (
	"crypto/rand"
	"time"

	"github.com/cleat-team/cleat/cleat"
)

// InsideLiteral reads the clock, entropy and ranges a map, all inside the function passed to SideEffect. Nothing here may be flagged.
func InsideLiteral(h cleat.HostCalls, input string) (string, error) {
	return h.SideEffect(func() (string, error) {
		b := make([]byte, 4)
		_, _ = rand.Read(b)
		m := map[string]int{"a": 1, "b": 2}
		n := 0
		for k := range m {
			n += len(k)
		}
		return time.Now().String() + string(b), nil
	})
}

// InsideTypedLiteral is the same through the typed form.
func InsideTypedLiteral(h cleat.HostCalls, input string) (int64, error) {
	return cleat.SideEffectTyped(h, func() (int64, error) {
		return time.Now().UnixNano(), nil
	})
}

// OutsideSideEffect makes the same clock read directly in the workflow, and must still be flagged (E003). It also has a SideEffect call whose
// literal is clean, so the flag cannot be explained by this function having none.
func OutsideSideEffect(h cleat.HostCalls, input string) (string, error) {
	started := time.Now()
	v, err := h.SideEffect(func() (string, error) { return "x", nil })
	_ = started
	return v, err
}

// AfterTheLiteral shows the exemption ends with the literal: the clock read after the call is flagged.
func AfterTheLiteral(h cleat.HostCalls, input string) (string, error) {
	v, err := h.SideEffect(func() (string, error) { return time.Now().String(), nil })
	_ = time.Now()
	return v, err
}

// nowString is a NAMED function. It calls no host method, so it is PURE to the closure scan, and the scan validates only functions in the
// durable closure: it is not flagged, with or without this change. Recorded because it is not what one would assume (measured, cleat#4123).
func nowString() (string, error) { return time.Now().String(), nil }

// NamedFunction passes a named function to SideEffect. The exemption concerns a literal written at the call; a named function is outside it
// and is judged by the scan's ordinary rule, which does not look at pure functions.
func NamedFunction(h cleat.HostCalls, input string) (string, error) {
	return h.SideEffect(nowString)
}

// notHostCalls has a SideEffect method that is not the SDK's.
type notHostCalls struct{}

func (notHostCalls) SideEffect(fn func() (string, error)) (string, error) { return fn() }

// NotTheSDKsSideEffect calls a SideEffect method on a type that is not cleat.HostCalls; its literal gets no exemption (E003).
func NotTheSDKsSideEffect(h cleat.HostCalls, input string) (string, error) {
	h.DurableLog("x")
	return notHostCalls{}.SideEffect(func() (string, error) { return time.Now().String(), nil })
}
