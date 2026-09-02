// Package noargs is the fixture for an entry point that takes no input.
//
// It exists because that case did not compile. Codegen emitted
// `argsJSON := readString(argsPtr, argsLen)` into every generated export
// unconditionally, and an entry point whose only parameter is the HostCalls
// handle has nothing that reads it -- so the guest failed to build with
// "declared and not used: argsJSON", an error pointing at generated code the
// workflow author never wrote and cannot edit.
//
// Nothing caught it because every other fixture, example and doc snippet in
// the repo happens to take an input parameter, and the codegen tests
// parse the generated source rather than compiling it -- and "declared and not
// used" is a type error, which a parser does not raise. The regression test is
// therefore a real `cleat build`, not another assertion about the text.
//
// Both shapes are here on purpose: the fix is "declare argsJSON only when
// something consumes it", and getting that backwards breaks the WITH-argument
// path instead. One fixture that exercises both means a mistake in either
// direction fails the same build.
package noargs

import "github.com/cleat-team/cleat/cleat"

// Cleanup takes no input. This is the shape that would not compile.
func Cleanup(h cleat.HostCalls) (string, error) {
	if _, err := h.DurableCall("janitor", "sweep", `{}`); err != nil {
		return "", err
	}
	return `{"swept":true}`, nil
}

// Greet takes an input, and is the control: it must still receive and use its
// argument, which is what argsJSON is for.
func Greet(h cleat.HostCalls, name string) (string, error) {
	if _, err := h.DurableCall("greeter", "greet", `{"name":"`+name+`"}`); err != nil {
		return "", err
	}
	return `{"greeted":"` + name + `"}`, nil
}
