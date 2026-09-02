// Package main is a fixture for defer execution.
//
// Each entry point registers defer bodies whose only observable effect is a
// DurableCall, because a DurableCall is what the host can see. A defer that
// did not run leaves its call absent from the recorded history; a defer that
// ran leaves it present, in a position that also pins the ORDER.
package main

import "github.com/cleat-team/cleat/cleat"

// DeferOrder registers two defer bodies and then makes one call of its own.
// A correct run records: body, then the two defers in LIFO order --
// "body", "second", "first".
func DeferOrder(h cleat.HostCalls, input string) (string, error) {
	if _, err := h.DurableDeferFunc(func() {
		h.DurableCall("cleanup", "first", `{}`)
	}); err != nil {
		return "", err
	}
	if _, err := h.DurableDeferFunc(func() {
		h.DurableCall("cleanup", "second", `{}`)
	}); err != nil {
		return "", err
	}

	if _, err := h.DurableCall("work", "body", `{}`); err != nil {
		return "", err
	}
	return `{"status":"ok"}`, nil
}

// DeferOnError is the case a defer exists for: the entry point fails after
// registering cleanup. The cleanup must still run.
func DeferOnError(h cleat.HostCalls, input string) (string, error) {
	if _, err := h.DurableDeferFunc(func() {
		h.DurableCall("cleanup", "on_error", `{}`)
	}); err != nil {
		return "", err
	}
	return "", errFailed
}

// DeferSurvivesSuspension registers cleanup and then sleeps. A suspension is
// not workflow exit, so the first segment must leave the cleanup unrun; the
// segment that finally completes must run it.
func DeferSurvivesSuspension(h cleat.HostCalls, input string) (string, error) {
	if _, err := h.DurableDeferFunc(func() {
		h.DurableCall("cleanup", "after_sleep", `{}`)
	}); err != nil {
		return "", err
	}

	h.DurableSleepMs(300000)

	if _, err := h.DurableCall("work", "body", `{}`); err != nil {
		return "", err
	}
	return `{"status":"ok"}`, nil
}

// DeferRegistration checks the registration contract rather than the body: the
// ID handed back is the host's, and it is what the body is keyed by.
func DeferRegistration(h cleat.HostCalls, input string) (string, error) {
	id, err := h.DurableDeferFunc(func() {})
	if err != nil {
		return "", err
	}
	return `{"defer_id":"` + id + `"}`, nil
}

var errFailed = &workflowError{"the workflow failed"}

type workflowError struct{ msg string }

func (e *workflowError) Error() string { return e.msg }
