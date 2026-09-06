// Package hostcallsinparamstruct is the fixture for
// TestVetGo_HostCallsInAParameterStruct.
//
// A function reaches HostCalls through a field of a struct it is PASSED, not
// through a first parameter of type cleat.HostCalls and not as a method on
// that struct. That is cleat/dagrun's designed shape -- its TaskContext.H is
// handed to every user-written task body -- and the threading check rejected
// it until 2026-09-06, so `cleat vet` refused the pattern a first-party cleat
// SDK package documents itself as requiring. IMPROVEMENT-PLAN 3.229.
//
// This fixture must vet CLEAN. It is the positive case; the negative one is
// covered by the existing threading tests.
package hostcallsinparamstruct

import "github.com/cleat-team/cleat/cleat"

// TaskContext mirrors dagrun.TaskContext: HostCalls arrives as a field.
type TaskContext struct {
	H     cleat.HostCalls
	Input string
}

// task takes the struct by pointer, as dagrun does.
func task(ctx *TaskContext) (string, error) {
	return ctx.H.DurableCall("svc", "op", ctx.Input)
}

// taskByValue takes it by value, so the fixture pins both spellings.
func taskByValue(ctx TaskContext) (string, error) {
	return ctx.H.DurableCall("svc", "op2", ctx.Input)
}

//cleat:entry
func Workflow(h cleat.HostCalls, input string) (string, error) {
	first, err := task(&TaskContext{H: h, Input: input})
	if err != nil {
		return "", err
	}
	return taskByValue(TaskContext{H: h, Input: first})
}
