// Package main is the fixture for cleat#4285: a Go guest that IGNORES a suspension.
//
// The Go SDK tells a guest to stop by panicking cleat.ErrSuspend out of the call that suspended. This guest recovers that panic and carries on, which is what
// an SDK whose suspension is an ordinary value, or a guest that swallows the exception, does in the other languages. The one thing the host can see of it is
// a DurableCall: a call made after the suspension is dispatched to the service, so a service call that must happen once happens twice (once now, once when the
// workflow resumes and replays past the pending await).
package main

import "github.com/cleat-team/cleat/cleat"

// IgnoreChildSuspend awaits a child that has not finished, swallows the suspension, and makes one service call.
func IgnoreChildSuspend(h cleat.HostCalls, input string) (string, error) {
	awaitAndSwallow(h, "child-1")
	if _, err := h.DurableCall("work", "after_suspend", `{}`); err != nil {
		return "", err
	}
	return `{"status":"ok"}`, nil
}

// awaitAndSwallow is the guest's mistake: recover() discards the panic that was the instruction to stop.
func awaitAndSwallow(h cleat.HostCalls, runID string) {
	defer func() { _ = recover() }()
	_, _ = h.AwaitChild(runID)
}
